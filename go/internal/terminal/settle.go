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
	// LeaseSettler 是「把本次请求的成本结算到预算租约上」的旁路接收面（见 lease_settle.go 的文件头）。
	// nil 表示未装配：租约结算整段跳过，结算路径行为与接线前一致。
	LeaseSettler LeaseSettler
	// Tracer 是「把本次终态上报到外部观测面」的旁路接收面（见 trace_seam.go 的文件头）。
	// nil 表示未装配（未配置 Langfuse key 时就是这个形态）：上报整段跳过。
	Tracer Tracer
	// Logger 供旁路记 warn（旁路失败不得冒泡成结算错误，故只能记日志）。
	// nil 时静默。
	Logger Logger
	// SlowRate 是低速样本的旁路接收面（见 slow_rate_seam.go 的文件头）。
	// nil 表示未装配：旁路整段跳过，结算路径行为与接线前完全一致。
	SlowRate SlowRateRecorder
	// SlowDiverts 是低速改道计数的旁路接收面（见 slow_rate_seam.go 的 SlowDivert）。
	// nil 表示未装配：该计数不写，其余行为不变。
	SlowDiverts SlowDivertRecorder
	// Queue 是终态写入的异步队列（见 batch.go 的文件头）。
	//
	// nil（默认）表示同步写：终态与成本在 Settle 内写完再返回，与接线前逐字一致。
	// 非 nil 表示 MESSAGE_REQUEST_WRITE_MODE=async：Settle 只同步校验入参并入队，
	// 写入与副作用交给队列 worker；队列满或已停时自动退回同步写（不丢）。
	Queue *WriteQueue
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
	// leaseSettler 是租约结算的旁路接收面；nil 即未装配。
	leaseSettler LeaseSettler
	// tracer 是终态上报的旁路接收面；nil 即未装配。
	tracer Tracer
	// slowRate 是低速样本的旁路接收面；nil 即未装配。
	slowRate SlowRateRecorder
	// slowDiverts 是低速改道计数的旁路接收面；nil 即未装配。
	slowDiverts SlowDivertRecorder
	logger      Logger
	// queue 是终态写入的异步队列；nil（默认）即同步写。
	queue *WriteQueue
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
		writer:       writer,
		maxAttempts:  attempts,
		backoff:      backoff,
		rollup:       options.Rollup,
		newRows:      options.NewRows,
		leaseSettler: options.LeaseSettler,
		tracer:       options.Tracer,
		slowRate:     options.SlowRate,
		slowDiverts:  options.SlowDiverts,
		logger:       options.Logger,
		queue:        options.Queue,
	}
}

// Result 是一次结算的结果。
type Result struct {
	// Committed 为真表示本次调用赢得了该行的终态（终态提交屏障）。
	// 只有它为真，调用方才允许发放后续对外可见副作用。
	Committed bool
	// Queued 为真表示本次终态已入队（异步写模式），提交结论要等队列 flush 之后才有：
	// 此时 Committed 恒为 false，**不代表**没赢下终态；提交后动作已一并交给队列 worker，
	// 调用方不得据 Queued 的结果自己再发一次。
	Queued bool
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

// Settle 执行一次终态结算（无提交后动作）。见 settle 的说明。
//
// 不接 pctx：调用方没有请求上下文时结算照样成立，只是**没有终态上报**（上报需要
// 方法/路径/用户这类请求属性）。生产路径走的是 SettleContext 与 SettleBlocked。
func (s *Settler) Settle(ctx context.Context, id int64, settlement Settlement) (Result, error) {
	return s.settle(ctx, nil, id, settlement, nil)
}

// settle 是结算的唯一入口：先同步校验入参（非法输入必须立刻报错，不能拖到 flush 才发现），
// 再按是否装配队列分两路——
//
//   - 异步（Settler 带 queue）：入队即返回 Result{Queued:true}，写入与提交后动作由队列 worker
//     在 flush 时执行（见 batch.go）；队列满或已停时**退回同步写**，绝不丢。
//   - 同步（默认）：与接线前逐字相同的路径。
//
// afterCommit 只在异步模式下由队列 worker 在写入完成后调用（提交结论那时才有），
// 且**由队列传入写上下文**；同步（与降级同步写）路径**不调它**，由调用方在返回后自己发放
// ——与接线前逐字一致。
//
// pc 一路带到 settleNow：终态上报要的是行 id + 请求属性，而拦截类终态的行 id 由建行结果
// 给出（pc 里没有），故两样都得传下去。
func (s *Settler) settle(
	ctx context.Context,
	pc *pctx.Context,
	id int64,
	settlement Settlement,
	afterCommit func(context.Context, Result),
) (Result, error) {
	patch, err := settlement.toPatch()
	if err != nil {
		return Result{}, err
	}
	if s.queue != nil {
		write := func(writeCtx context.Context) (Result, error) {
			return s.settleNow(writeCtx, pc, id, settlement, patch)
		}
		if s.queue.enqueue(ctx, id, write, afterCommit) {
			return Result{Queued: true}, nil
		}
	}
	return s.settleNow(ctx, pc, id, settlement, patch)
}

// settleNow 在写入主体外裹一层终态上报：写入与上报在同一个位置收口，故同步与异步两条
// 路径的上报时机与条数完全一致（异步由队列 worker 调到这里，时机同样是写完之后）。
// 只上报**赢得终态**的行（Result.Committed）；成本字段只在 result.CostWritten 为真时上报。
func (s *Settler) settleNow(
	ctx context.Context,
	pc *pctx.Context,
	id int64,
	settlement Settlement,
	patch store.DetailsPatch,
) (Result, error) {
	result, err := s.settleNowInner(ctx, id, settlement, patch)
	s.traceTerminal(pc, id, settlement, result)
	return result, err
}

// settleNowInner 是同步写入主体。顺序与理由：
//
//  1. 先写终态（带 `status_code IS NULL` 谓词），只有赢家才写成本——避免给一个
//     自己没赢下的行写成本。TS 侧文本顺序是「先成本后终态」，但账本行由触发器在每次
//     监视列写入后按**行的当前状态**重建，两种顺序的最终账本行相同；先终态在竞争下更安全。
//  2. 成本走 UpdateWinnerCost：它按 hedge 安全语义写（winner 成本 + 既有 hedge 输家之和），
//     本波 hedge_losers 恒为空，等价于覆盖写。
func (s *Settler) settleNowInner(
	ctx context.Context,
	id int64,
	settlement Settlement,
	patch store.DetailsPatch,
) (Result, error) {
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
	// 租约结算放在**成本写成功之后、其余旁路之前**：租约决定后续请求能不能进（并发下越早
	// 看到扣减越好），而它成立的前提正是这笔成本已经进了账本。失败只留痕，不改返回值。
	s.settleLeases(ctx, id, settlement)
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
// pc 用于终态上报（方法/路径/用户/起始时刻）与亲和写回；调用方拿不到请求上下文时可传 nil，
// 此时结算本身照常，只是不上报。
//
// 行 id 的选取：拦截类的正常形态是 pc 里**没有** id（拦截点在 messageContext 之前，行只能在
// 这里建），故按建行载荷开行、用建行结果作为行 id；但 pc 已有 id 时必须**复用那一行**——
// 否则「先 SettleBlocked 建行、后 SettleContext 复用 pc 行」的组合会在同一条请求上写出两行，
// 并随之产生两条终态上报与两条账本行。本函数因此与 SettleContext 同一判据：有 id 就用它。
func (s *Settler) SettleBlocked(
	ctx context.Context,
	pc *pctx.Context,
	create store.CreateMessageRequestData,
	settlement Settlement,
) (Result, error) {
	return s.settleBlocked(ctx, pc, create, settlement, nil)
}

// settleBlocked 是 SettleBlocked 的带提交后动作版本：建行与入参校验始终是**同步**的
// （开行标识是后续一切写入的前提，异步化它就等于丢掉这个 id）。
func (s *Settler) settleBlocked(
	ctx context.Context,
	pc *pctx.Context,
	create store.CreateMessageRequestData,
	settlement Settlement,
	afterCommit func(context.Context, Result),
) (Result, error) {
	if settlement.StatusCode <= 0 {
		return Result{}, incomplete("拦截类终态必须带状态码")
	}
	// 已有行标识（守卫链已开行，或调用方先走了一次建行）就复用它，不再建第二行：
	// 同一条请求只应有一行，重复建行会多出一条账本行与一次终态上报。
	if pc != nil {
		if id, ok := pc.MessageRequestID(); ok {
			return s.settle(ctx, pc, id, settlement, afterCommit)
		}
	}
	row, err := s.writer.CreateMessageRequest(ctx, create)
	if err != nil {
		return Result{}, fmt.Errorf("terminal: 建开行失败: %w", err)
	}
	return s.settle(ctx, pc, row.ID, settlement, afterCommit)
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
	// winner 写回只在终态提交之后发。异步写模式下提交结论只有 flush 之后才有，故把它作为
	// 提交后动作交给队列；同步模式则由下面那句 affinityWriteback 在写入返回后发（接线前同形）。
	//
	// 异步路径的 ctx 由队列给出（入队时派生的不可取消上下文），**不得**由闭包捕获请求 ctx：
	// flush 在请求返回之后，此刻请求 ctx 已取消，亲和 CAS 写会静默失败、粘性绑定永不落库，
	// 每个后续请求都重跑初选与竞速。
	//
	// 终态上报不在这里：它裹在 settleNow 里（同步与异步同一处收口），故两条路径都不会漏、
	// 也不会重复。
	winner := func(writeCtx context.Context, result Result) {
		s.affinityWinner(writeCtx, pc, settlement.Affinity, result.Committed)
		// 会话绑定的**成功侧**与亲和 winner 同一时机、同一队列 ctx：只在终态真提交之后
		// 才 CAS。异步模式下它必须跟着队列走——漏掉这一步，生产（async）路径上会话绑定
		// 写回会**一次都不发放**，绑定永远没有胜出渠道、后续请求无从提名。
		s.sessionBindingWriteback(writeCtx, pc, settlement.Affinity, sessionBindingWinner, result.Committed)
		// 低速样本与亲和写回同一时机、同一队列 ctx：终态提交之后才采样，
		// 且异步模式下跟着队列走（见 batch.go 的 afterCommit）。
		s.recordSlowRateCommitted(writeCtx, settlement.SlowRate, result.Committed)
	}
	result, err := s.settleContext(ctx, pc, settlement, create, winner)
	if result.Queued {
		// 已入队：墓碑**不随异步写入推迟**（它与是否赢得终态无关，Node 在失败判定处立即
		// fire-and-forget）；winner 由队列在 flush 之后发。
		//
		// 会话绑定的**失败侧**与墓碑同机，理由同源：它同样不依赖是否赢得终态，而它的
		// 作用恰恰是让「下一个请求绕开这家」——推迟到 flush 会让该效果晚一个批次生效。
		s.affinityTombstone(ctx, pc, settlement.Affinity)
		s.sessionBindingWriteback(ctx, pc, settlement.Affinity, sessionBindingFailure, false)
		// 改道计数与墓碑同一口径：它不依赖是否赢得终态，而「唯一候选被会话冷却剔掉」
		// 恰恰是**没有可提交账务**的那条 503——把它放在 winner 里会永远收不到。
		s.recordSlowDiverts(ctx, settlement.SlowDiverts)
		return result, err
	}
	// 未入队（同步模式或队列满降级）：与接线前逐字一致——墓碑先、winner 后，且都在
	// 写入返回之后。三种「没写成」的退出也走这里，结论同样是 Committed=false。
	s.affinityWriteback(ctx, pc, settlement.Affinity, result.Committed)
	s.recordSlowRateCommitted(ctx, settlement.SlowRate, result.Committed)
	s.recordSlowDiverts(ctx, settlement.SlowDiverts)
	return result, err
}

// settleContext 是 SettleContext 的入账主体（写终态、写成本、发放提交后动作）。
func (s *Settler) settleContext(
	ctx context.Context,
	pc *pctx.Context,
	settlement Settlement,
	create *store.CreateMessageRequestData,
	afterCommit func(context.Context, Result),
) (Result, error) {
	if pc != nil {
		if id, ok := pc.MessageRequestID(); ok {
			return s.settle(ctx, pc, id, settlement, afterCommit)
		}
	}
	if create == nil {
		return Result{}, ErrNoRow
	}
	return s.settleBlocked(ctx, pc, *create, settlement, afterCommit)
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
// 写回失败不改终态，也不上报：Node  侧同样是 fire-and-forget（recorder 内部只记日志）。
//
// 拆成两半是为了异步写模式：墓碑与 winner 的时机不同（见 SettleContext）。
func (s *Settler) affinityWriteback(
	ctx context.Context,
	pc *pctx.Context,
	directive AffinityDirective,
	committed bool,
) {
	s.affinityTombstone(ctx, pc, directive)
	s.affinityWinner(ctx, pc, directive, committed)
	// 会话绑定与亲和共用同一份终态事实：成功者与失败者都直接来自 directive。
	// 为何共用而不是另开一列：两者的「谁是 winner、谁是失败者」完全同源（同一个
	// forward 结果），各算一次只会得到一个可能与事实不符的副本。
	//
	// 同步路径两半各发一次（与亲和侧的两个函数同形）；异步路径不调这两句，
	// 而是由 SettleContext 在各自时机分别调同一个函数（见那里的注释）。
	s.sessionBindingWriteback(ctx, pc, directive, sessionBindingFailure, committed)
	s.sessionBindingWriteback(ctx, pc, directive, sessionBindingWinner, committed)
}

// sessionBindingWriteback 发放会话绑定的终态写回，是**唯一的分派点**：三种结局
// （成功 CAS / ProviderError 写冷却 / ResourceNotFound 只清绑定）只在这里判定。
//
// 语义与亲和写回平行但**不等价**（不能合并成一个实现）：
//   - 成功：CAS 把绑定指向 winner（generation fence 拒绝迟到写入）；
//   - 供应商故障：写会话冷却（key TTL 60s），使后续请求绕开这家；
//   - 资源/配置类失效（上游 404：该家没这个模型）：**只清绑定、不写冷却**（设计稿 §4）
//     ——模型不支持是配置决策而非故障，写冷却会把「缺模型」记成「慢」。
//
// 两半的时机不同，故调用方用 phase 选本次发哪一半：
//   - sessionBindingFailure 不依赖是否赢得终态，异步模式下与墓碑同机、在入队处立即发放
//     （它的作用正是让下一个请求绕开这家，推迟一个批次即削弱效果）；
//   - sessionBindingWinner 必须等终态**真提交**（异步即队列 flush 之后），否则会把粘性
//     指向一个本次并未真正服务成功的供应商。
//
// 同步路径（含队列满降级）两半各发一次；异步路径入队处发失败半、提交后动作发成功半。
// 两条路径调的是同一个函数、同一套判据，故不可能分叉。
//
// 未装配（无会话身份、会话包未接线）时整段跳过，行为与接线前逐字一致。
func (s *Settler) sessionBindingWriteback(
	ctx context.Context,
	pc *pctx.Context,
	directive AffinityDirective,
	phase sessionBindingPhase,
	committed bool,
) {
	if !sessionBindingApplies(directive, phase, committed) {
		return
	}
	// 成功侧的第三道门（前两道在 sessionBindingApplies 里）：本次选路是否要求保留既有绑定。
	//
	// 设计稿 §4 对熔断明定「跳过该 provider、不清空绑定、待恢复后仍粘回去」。绑定 provider
	// 因临时原因（熔断/会话冷却/活动时段/限额/本次已试过）被跳过、备用成功时，若照旧 CAS，
	// 会话就被永久搬到备用——「待恢复仍粘回去」即为假，且此后每次熔断都搬一次。
	//
	// 不在此处记日志：该判定每请求都可能成立，逐条会淹掉日志；「为何没搬」在链上可读
	// （绑定 provider 带 circuit_open / slow_rate_cooldown 出现在 filteredProviders 里），
	// 选路侧的 Debug 日志也带上了 bypass 值（见 guard.adapters.provider_selected）。
	if phase == sessionBindingWinner {
		if _, keep := pc.SessionBindingKeepReason(); keep {
			return
		}
	}
	writeback, writeCtx, cancel, ok := s.sessionBindingTarget(ctx, pc)
	if !ok {
		return
	}
	defer cancel()
	if phase == sessionBindingFailure {
		if directive.TombstoneKind == AffinityTombstoneResourceNotFound {
			writeback.ClearBinding(writeCtx, directive.TombstoneProviderID)
			return
		}
		writeback.CooldownOnFailure(writeCtx, directive.TombstoneProviderID)
		return
	}
	writeback.CompareAndSet(writeCtx, directive.WinnerProviderID)
}

// sessionBindingPhase 选择本次发放的是哪一半——两半的时机不同，必须能分开调（见上）。
type sessionBindingPhase int

const (
	// sessionBindingFailure 是失败侧：写冷却或清绑定，不依赖是否赢得终态。
	sessionBindingFailure sessionBindingPhase = iota
	// sessionBindingWinner 是成功侧：CAS 指向 winner，只在终态真提交之后。
	sessionBindingWinner
)

// sessionBindingApplies 判「本次这一半要不要发放」。判据与 phase 都收在这里，故两条
// 路径不可能对「什么时候该写」产生分歧。
func sessionBindingApplies(directive AffinityDirective, phase sessionBindingPhase, committed bool) bool {
	switch phase {
	case sessionBindingFailure:
		// PrefixOnly 类（客户端主动中断）只写前缀墓碑：供应商没出错，不得给它写冷却。
		return directive.TombstoneProviderID > 0 &&
			directive.TombstoneKind != AffinityTombstonePrefixOnly
	case sessionBindingWinner:
		return directive.WinnerProviderID > 0 && committed
	default:
		return false
	}
}

// sessionBindingTarget 取本次请求的会话绑定写回实现与它的写上下文。
//
// 第四个返回值在「本次不写会话绑定」时为 false（无会话身份、会话包未装配，或守卫链
// 未接线）——调用方据此跳过，而不是推断。
//
// 写上下文按终态层的既有纪律自建：异步写模式下本函数由队列 worker 在请求返回之后调用，
// 此刻请求 ctx 已取消（与 affinity 写回、低速样本同一个坑），拿它去写 Redis 会静默失败。
func (s *Settler) sessionBindingTarget(
	ctx context.Context,
	pc *pctx.Context,
) (pctx.SessionBindingWriteback, context.Context, context.CancelFunc, bool) {
	if pc == nil {
		return nil, nil, nil, false
	}
	writeback, ok := pc.SessionBindingWriteback()
	if !ok || writeback == nil {
		return nil, nil, nil, false
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionBindingTimeout)
	return writeback, writeCtx, cancel, true
}

// sessionBindingTimeout 是会话绑定写回的上界，与低速旁路同量级（Redis 默认命令超时）。
const sessionBindingTimeout = 3 * time.Second

// affinityTombstone 发放失败墓碑：它**不依赖**本次是否赢得终态。
func (s *Settler) affinityTombstone(ctx context.Context, pc *pctx.Context, directive AffinityDirective) {
	writeback, ok := s.affinityWritebackTarget(pc, directive)
	if !ok || directive.TombstoneProviderID <= 0 {
		return
	}
	writeback.TombstoneOnFailure(ctx, directive.TombstoneProviderID)
}

// affinityWinner 发放成功写回：只在终态提交之后发。
func (s *Settler) affinityWinner(
	ctx context.Context,
	pc *pctx.Context,
	directive AffinityDirective,
	committed bool,
) {
	writeback, ok := s.affinityWritebackTarget(pc, directive)
	if !ok || directive.WinnerProviderID <= 0 || !committed {
		return
	}
	writeback.RecordWinner(ctx, directive.WinnerProviderID)
}

// affinityWritebackTarget 取出本次请求的亲和写回能力；未装配或与本次指令无关时返回 false。
func (s *Settler) affinityWritebackTarget(
	pc *pctx.Context,
	directive AffinityDirective,
) (pctx.AffinityWriteback, bool) {
	if pc == nil || (directive.WinnerProviderID <= 0 && directive.TombstoneProviderID <= 0) {
		return nil, false
	}
	writeback, ok := pc.AffinityWriteback()
	if !ok || writeback == nil {
		return nil, false
	}
	return writeback, true
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
