package terminal

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// Options 是结算器的可注入参数。退避必须可注入：测试用零退避，
// 绝不允许等到真实长超时。
type Options struct {
	// MaxAttempts 是单条语句的尝试次数上限。<=0 时取 DefaultMaxAttempts。
	MaxAttempts int
	// Backoff 返回第 attempt 次失败后的等待时长（attempt 从 0 起）。nil 时用默认退避。
	Backoff func(attempt int) time.Duration
	// Rollup 是 public-status 投影的旁路事件接收面（见 rollup.go 的文件头）。
	// nil 表示未装配：旁路整段跳过，结算路径行为与之前完全一致。
	Rollup RollupRecorder
	// NewRows 是「请求日志有新行落库」的旁路接收面（见 notify.go 的文件头）。
	// nil 表示未装配：使用记录页的推送模式收不到信号，前端照旧轮询。
	NewRows NewRowsNotifier
	// Logger 供旁路记 warn（旁路失败不得冒泡成结算错误，故只能记日志）。
	// nil 时静默。
	Logger Logger
}

// DefaultMaxAttempts 与 store.UpdateWinnerCost 的重试次数一致。
const DefaultMaxAttempts = 3

// DefaultBackoff 复刻 store.UpdateWinnerCost 的 50ms × 次数退避。
func DefaultBackoff(attempt int) time.Duration {
	return time.Duration(50*(attempt+1)) * time.Millisecond
}

// Settler 是终态结算器。它是无状态的：所有可变状态都在参数与 Writer 里，
// 因此可以并发使用。
type Settler struct {
	writer      Writer
	maxAttempts int
	backoff     func(attempt int) time.Duration
	// rollup 是 public-status 事件的旁路接收面；nil 即未装配。
	rollup RollupRecorder
	// newRows 是「有新行落库」的旁路接收面；nil 即未装配。
	newRows NewRowsNotifier
	logger  Logger
}

// New 构造结算器。
func New(writer Writer, options Options) *Settler {
	attempts := options.MaxAttempts
	if attempts <= 0 {
		attempts = DefaultMaxAttempts
	}
	backoff := options.Backoff
	if backoff == nil {
		backoff = DefaultBackoff
	}
	return &Settler{
		writer:      writer,
		maxAttempts: attempts,
		backoff:     backoff,
		rollup:      options.Rollup,
		newRows:     options.NewRows,
		logger:      options.Logger,
	}
}

// Result 是一次结算的结果。
type Result struct {
	// Committed 为真表示本次调用赢得了该行的终态（终态提交屏障）。
	// 只有它为真，调用方才允许发放后续对外可见副作用。
	Committed bool
	// Attempts 是终态写实际尝试的次数（>=1）。
	Attempts int
	// CostWritten 为真表示成本已入库。
	CostWritten bool
}

// 结算入账的判别错误：
//
// ErrNoRow 表示本次请求没有可结算的日志行（守卫链没有开行，与 Node 侧 messageContext
// 为空时不落库一致）。它不是一个要重试的失败，而是「本请求不记账」这个事实。
var ErrNoRow = errors.New("terminal: 本次请求没有请求日志行")

// ErrCostWriteFailed 表示终态已提交但成本写入失败。此时 Committed 仍为真：
// 行已终态、账本行已生成，调用方可以继续发放副作用，但必须把成本缺口报出去，
// 不得静默吞掉。
var ErrCostWriteFailed = errors.New("terminal: 终态已提交但成本写入失败")

// Settle 执行一次终态结算。顺序与理由：
//
//  1. 先写终态（带 `status_code IS NULL` 谓词），只有赢家才写成本——避免给一个
//     自己没赢下的行写成本。TS 侧文本顺序是「先成本后终态」，但账本行由触发器在每次
//     监视列写入后按**行的当前状态**重建，两种顺序的最终账本行相同；先终态在竞争下更安全。
//  2. 成本走 UpdateWinnerCost：它按 hedge 安全语义写（winner 成本 + 既有 hedge 输家之和），
//     本波 hedge_losers 恒为空，等价于覆盖写。
func (s *Settler) Settle(ctx context.Context, id int64, settlement Settlement) (Result, error) {
	patch, err := settlement.toPatch()
	if err != nil {
		return Result{}, err
	}

	committed, attempts, err := s.retry(ctx, s.maxAttempts, func() (bool, error) {
		return s.writer.UpdateDetailsIfUnfinalized(ctx, id, patch)
	})
	result := Result{Committed: committed, Attempts: attempts}
	if err != nil {
		return result, err
	}
	if !committed {
		return result, ErrNotSettled
	}

	if settlement.Cost == nil {
		// 不计费也要发旁路：public-status 统计的是「请求成不成功」与延迟，与计费无关。
		// 「有新行」同理：行已在终态上落库，使用记录页该看到它，哪怕它不产生金额。
		s.notifyNewRow(id)
		s.recordRollup(ctx, id, settlement)
		return result, nil
	}
	if _, _, err := s.retry(ctx, s.maxAttempts, func() (bool, error) {
		if err := s.writer.UpdateWinnerCost(ctx, id, settlement.Cost.Total, settlement.Cost.Breakdown); err != nil {
			return false, err
		}
		return true, nil
	}); err != nil {
		return result, fmt.Errorf("%w: %w", ErrCostWriteFailed, err)
	}
	result.CostWritten = true
	// 「有新行」放在成本写之后：与旁路副作用的既有顺序一致（终态 → 计费 → 旁路），
	// 也让「带金额的行已就绪」成为前端下一次增量拉取看到的状态。
	s.notifyNewRow(id)
	// 旁路放在**成本写之后**：与 Node 的副作用顺序一致（终态 → 计费 → 旁路），
	// 也让「已计费的请求才发统计」这一事实可读。失败只 warn，不影响本函数返回值。
	s.recordRollup(ctx, id, settlement)
	return result, nil
}

// SettleBlocked 复刻「拦截即终态」路径（敏感词、预热等）：这类请求在拦截点才建行，
// 且建行时就已经是终态。
//
// 本实现是两步：先按 store 的插入面建开行，再用一次 Settle 写终态。之所以不是
// 一条 INSERT 写下终态列，是因为 store 的插入面今天不表达 status_code/blocked_by/
// 终态计量列；两步的最终行、账本行与 outbox 事件都与单条 INSERT 等价：
// 开行那一刻 status_code 为 NULL，message_request_outbox_aiud 直接返回不产生事件，
// 随后的终态写才是 outbox 事件的产生点，且它带齐了监视列。
//
// 唯一的可见性差异：开行到终态之间账本行会短暂呈现 status_code 为 NULL 的状态
// （is_success 暂为 true）。这不是本实现引入的——trg_upsert_usage_ledger 在 INSERT
// 上就会跑，Node 的正常路径同样是「先开行、后终态」，所以两侧行为一致。
//
// 升级路径：store 的插入面补上终态列后，本函数应改成单条 INSERT。
func (s *Settler) SettleBlocked(
	ctx context.Context,
	create store.CreateMessageRequestData,
	settlement Settlement,
) (Result, error) {
	if settlement.StatusCode <= 0 {
		return Result{}, incomplete("拦截类终态必须带状态码")
	}
	row, err := s.writer.CreateMessageRequest(ctx, create)
	if err != nil {
		return Result{}, fmt.Errorf("terminal: 建开行失败: %w", err)
	}
	return s.Settle(ctx, row.ID, settlement)
}

// SettleContext 按上下文里的行标识结算，是转发路径的入账入口。
//
// 三种情形：
//
//  1. 上下文已有行标识（守卫链的 messageContext 步骤已开行）——更新该行。这是生产路径：
//     同一条请求只有一行，不会重复建行。
//  2. 无标识但提供了开行载荷——按「建行 + 终态」两步结算（等价于 SettleBlocked）。
//  3. 两者都没有——返回 ErrNoRow：本请求不记账。调用方必须显式处理这个结论，
//     不得把它当成成功，也不得自作主张补一行。
//
// 「只结算一次」由 store 的终态谓词（UpdateDetailsIfUnfinalized）兜底，不在本函数里再
// 加一层进程内闸：pctx.Settlement 是响应侧的一次性标记，与数据库侧的终态屏障是两件事。
func (s *Settler) SettleContext(
	ctx context.Context,
	pc *pctx.Context,
	settlement Settlement,
	create *store.CreateMessageRequestData,
) (Result, error) {
	result, err := s.settleContext(ctx, pc, settlement, create)
	s.affinityWriteback(ctx, pc, settlement.Affinity, result.Committed)
	return result, err
}

// settleContext 是 SettleContext 的入账主体（写终态、写成本）。
func (s *Settler) settleContext(
	ctx context.Context,
	pc *pctx.Context,
	settlement Settlement,
	create *store.CreateMessageRequestData,
) (Result, error) {
	if pc != nil {
		if id, ok := pc.MessageRequestID(); ok {
			return s.Settle(ctx, id, settlement)
		}
	}
	if create == nil {
		return Result{}, ErrNoRow
	}
	return s.SettleBlocked(ctx, *create, settlement)
}

// affinityWriteback 是亲和终态写回的**唯一**位置（终态提交后的副作用，而非散落的调用点）。
// 顺序不可交换，依据逐条来自 Node：
//
//  1. 墓碑先写，且**不依赖**本次是否赢得终态：Node 在失败判定处立即 fire-and-forget
//     （response-handler.ts:5425），与计费无关；失败行本来就常常没赢下终态。
//  2. 成功写回只在终态提交之后：Node 把 recordAffinityWinner 放进 postTerminalSideEffects，
//     它在 message_request 终态与计费落库之后才执行（response-handler.ts:5414-5420）；
//     未提交就写回会把粘性指向一个本次并未真正服务成功的供应商。
//
// 写回失败不改终态，也不上报：Node 侧同样是 fire-and-forget（recorder 内部只记日志）。
func (s *Settler) affinityWriteback(
	ctx context.Context,
	pc *pctx.Context,
	directive AffinityDirective,
	committed bool,
) {
	if pc == nil || (directive.WinnerProviderID <= 0 && directive.TombstoneProviderID <= 0) {
		return
	}
	writeback, ok := pc.AffinityWriteback()
	if !ok || writeback == nil {
		return
	}
	if directive.TombstoneProviderID > 0 {
		writeback.TombstoneOnFailure(ctx, directive.TombstoneProviderID)
	}
	if directive.WinnerProviderID > 0 && committed {
		writeback.RecordWinner(ctx, directive.WinnerProviderID)
	}
}

// retry 按注入的退避重试 operation。返回最后一次的提交结果与尝试次数。
// operation 返回 (false, nil) 是「未赢得该行」这类业务结论，不是错误，不重试。
func (s *Settler) retry(
	ctx context.Context,
	maxAttempts int,
	operation func() (bool, error),
) (bool, int, error) {
	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		committed, err := operation()
		if err == nil {
			return committed, attempt + 1, nil
		}
		lastErr = err
		if attempt < maxAttempts-1 {
			select {
			case <-ctx.Done():
				return false, attempt + 1, fmt.Errorf("terminal: 等待重试时上下文结束: %w", ctx.Err())
			case <-time.After(s.backoff(attempt)):
			}
		}
	}
	return false, maxAttempts, lastErr
}
