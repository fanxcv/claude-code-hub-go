package patrol

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 补写的终态口径。依据见 doc.go：Node 在「客户端先于终态断开」时写下的正是这一对值
// （src/app/v1/_lib/proxy/response-handler.ts:2337-2339）。
const (
	RepairStatusCode   = 499
	RepairErrorMessage = "CLIENT_ABORTED"
)

// MarkerPrefix 是补写行的可追溯标记前缀，落在 error_stack 列。
//
// 为什么用 error_stack 而不是 error_message：error_message 是 usage_ledger 的 is_success
// 判据（fn_upsert_usage_ledger）也是成功率的归一化输入，掺进标记会同时污染两处；而
// error_stack 既不在 trg_upsert_usage_ledger 的 37 列里，也不在 message_request_outbox_aiud
// 的 5 列里，写它不会重写账本行、不会重复产生 outbox 事件。按本前缀即可反查全部被补写的行。
const MarkerPrefix = "patrol:unsettled_settlement_lost"

// stopTimeout 有界等待：巡检停止最多等这么久，超过就按「已放弃」记一条 warn。
// 冻结的停止会让退出序列卡死在关依赖之前，那比丢一轮巡检糟得多。
const stopTimeout = 5 * time.Second

// defaultCostGapAfter 是成本缺口的默认宽限：结算里「先终态、后成本」的间隔在重试下
// 至多到秒级，一小时足够宽，不会把在途写入报成缺口。
const defaultCostGapAfter = time.Hour

// UnsettledStore 是巡检需要的两件事：找出候选行、对单行补终态。
// 窄接口使循环逻辑（分页、上界、错误容忍）可以脱离数据库单测。
type UnsettledStore interface {
	ListUnsettledRequests(
		ctx context.Context,
		cutoff time.Time,
		after store.UnsettledCursor,
		limit int,
	) ([]store.UnsettledRequest, error)
	// RepairUnsettled 返回 false 表示谓词未命中（行已有终态或不存在）。
	RepairUnsettled(ctx context.Context, id int64, patch store.DetailsPatch) (bool, error)
}

// DBStore 把 UnsettledStore 接到真实库上。
type DBStore struct {
	pools *store.Pools
}

// NewDBStore 建立库存储。
func NewDBStore(pools *store.Pools) *DBStore { return &DBStore{pools: pools} }

// CostGapStore 是可选的「成本缺口扫描」能力：列出终态已提交、cost_usd 仍为空的旧行。
//
// 不做成 UnsettledStore 的必选方法：那会强制既有实现（测试替身 / 装配点）跟着改，
// 而成本缺口阶段只告警、不写库——Store 未实现它时本轮跳过扫描，补终态职责不受影响。
type CostGapStore interface {
	ListCostGapRequests(
		ctx context.Context,
		cutoff time.Time,
		after store.UnsettledCursor,
		limit int,
	) ([]store.CostGapRequest, error)
}

// ListCostGapRequests 见 store.Pools.ListCostGapRequests（同一条语句，走 control 分道）。
func (s *DBStore) ListCostGapRequests(
	ctx context.Context,
	cutoff time.Time,
	after store.UnsettledCursor,
	limit int,
) ([]store.CostGapRequest, error) {
	return s.pools.ListCostGapRequests(ctx, cutoff, after, limit)
}

// ListUnsettledRequests 见 store.Pools.ListUnsettledRequests（同一条语句，走 control 分道）。
func (s *DBStore) ListUnsettledRequests(
	ctx context.Context,
	cutoff time.Time,
	after store.UnsettledCursor,
	limit int,
) ([]store.UnsettledRequest, error) {
	return s.pools.ListUnsettledRequests(ctx, cutoff, after, limit)
}

// RepairUnsettled 走 control 分道的条件终态更新。
//
// 不用 store.Pools.UpdateDetailsIfUnfinalized（它固定走 writer 分道）：writer 预算只有 1 条
// 连接，一轮几十上百条补写会把在途请求的终态写入排在后面——巡检不能以推迟结算为代价。
// 条件谓词（status_code IS NULL）由 store.UpdateDetailsIfUnfinalizedWith 原样带上。
func (s *DBStore) RepairUnsettled(ctx context.Context, id int64, patch store.DetailsPatch) (bool, error) {
	pool, err := s.pools.Control()
	if err != nil {
		return false, err
	}
	return store.UpdateDetailsIfUnfinalizedWith(ctx, pool, id, patch)
}

// Options 是巡检的可调参数；零值字段取默认值。
type Options struct {
	// Store 必须提供（无存储即无法巡检）。
	Store UnsettledStore
	// Logger 为 nil 时写 stderr（logx.New(nil)）。
	Logger *logx.Logger
	// UnsettledAfter 是「多久没终态就认定丢了」的阈值，必填且必须为正。
	//
	// 它必须大于本部署可能的最长活跃流时长，否则会先给仍在流中的行补终态，把真实结算挡掉
	// （见 doc.go 的残余风险段）。
	UnsettledAfter time.Duration
	// CostGapAfter 是「终态已提交多久、成本仍为空才认定是缺口」的宽限。
	//
	// 留宽限是因为成本写入在终态之后异步进行：零值取默认一小时，避免把在途写入报成缺口。
	CostGapAfter time.Duration
	// Interval 是巡检间隔。
	Interval time.Duration
	// BatchSize 是单次查询的行数上限。
	BatchSize int
	// MaxRowsPerRound 是单轮处理的总行数上限（跨页累计）。
	MaxRowsPerRound int
	// RoundTimeout 是单轮的墙钟上限。
	RoundTimeout time.Duration
	// Now 可注入时钟；nil 时用 time.Now。
	Now func() time.Time
}

// Result 是一轮的计数。
type Result struct {
	// Found 是查出的候选行数（含未修成功的）。
	Found int
	// Repaired 是补写成功的行数（谓词命中）。
	Repaired int
	// Skipped 是谓词未命中的行数（行已被真实结算或并发巡检终结）。
	Skipped int
	// Failed 是补写报错的行数。
	Failed int
	// CostGapFound 是本轮扫到的「终态已提交但成本未落库」行数（只告警，不补写）。
	CostGapFound int
}

// Patrol 是巡检本体。
type Patrol struct {
	store           UnsettledStore
	logger          *logx.Logger
	unsettledAfter  time.Duration
	costGapAfter    time.Duration
	interval        time.Duration
	batchSize       int
	maxRowsPerRound int
	roundTimeout    time.Duration
	now             func() time.Time

	mu      sync.Mutex
	running bool
	cancel  context.CancelFunc
	done    chan struct{}
}

// New 构造巡检；Store 与 UnsettledAfter 必填，其余零值取默认。
func New(options Options) (*Patrol, error) {
	if options.Store == nil {
		return nil, errors.New("patrol: 缺少存储")
	}
	if options.UnsettledAfter <= 0 {
		return nil, errors.New("patrol: 阈值必须为正——0 会让「刚开行」的行也被判为丢失")
	}
	interval := options.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	batchSize := options.BatchSize
	if batchSize <= 0 {
		batchSize = 200
	}
	maxRows := options.MaxRowsPerRound
	if maxRows <= 0 {
		maxRows = 2000
	}
	if maxRows < batchSize {
		maxRows = batchSize
	}
	roundTimeout := options.RoundTimeout
	if roundTimeout <= 0 {
		roundTimeout = 30 * time.Second
	}
	costGapAfter := options.CostGapAfter
	if costGapAfter <= 0 {
		costGapAfter = defaultCostGapAfter
	}
	logger := options.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	return &Patrol{
		store:           options.Store,
		logger:          logger,
		unsettledAfter:  options.UnsettledAfter,
		costGapAfter:    costGapAfter,
		interval:        interval,
		batchSize:       batchSize,
		maxRowsPerRound: maxRows,
		roundTimeout:    roundTimeout,
		now:             now,
	}, nil
}

// RunOnce 执行一轮：按 (created_at, id) 游标分页取候选行，逐行补终态。
//
// 上界有三层（批量、单轮总行数、单轮墙钟），任何一层命中就收工——巡检是兜底手段，
// 不能因为堆积量大而变成一次全表长事务。查询失败立即返回错误：继续跑只会重复同一次失败。
func (p *Patrol) RunOnce(ctx context.Context) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, p.roundTimeout)
	defer cancel()

	cutoff := p.now().Add(-p.unsettledAfter)
	var result Result
	result.CostGapFound = p.scanCostGaps(ctx, p.now().Add(-p.costGapAfter))
	cursor := store.UnsettledCursor{}
	var firstID, lastID int64

	for result.Found < p.maxRowsPerRound {
		limit := min(p.batchSize, p.maxRowsPerRound-result.Found)
		rows, err := p.store.ListUnsettledRequests(ctx, cutoff, cursor, limit)
		if err != nil {
			p.logger.Warn("patrol_failed", map[string]any{
				"stage":    "list",
				"found":    result.Found,
				"repaired": result.Repaired,
				"error":    err.Error(),
			})
			p.logRound(result, cutoff, firstID, lastID)
			return result, err
		}
		if len(rows) == 0 {
			break
		}
		result.Found += len(rows)
		for _, row := range rows {
			// 游标严格推进：修不好的行不会把后面的行永远挡住。
			cursor = store.UnsettledCursor{CreatedAt: row.CreatedAt, ID: row.ID}
			if firstID == 0 {
				firstID = row.ID
			}
			lastID = row.ID

			applied, repairErr := p.store.RepairUnsettled(ctx, row.ID, p.repairPatch(row))
			switch {
			case repairErr != nil:
				result.Failed++
				p.logger.Warn("patrol_failed", map[string]any{"id": row.ID, "error": repairErr.Error()})
			case applied:
				result.Repaired++
			default:
				// 谓词未命中：另一个写入者已经终结了这一行（真实结算后到，或并发的巡检轮）。
				result.Skipped++
			}
		}
		// 墙钟用尽或进程在退出：本轮就此收手，不带着已取消的上下文继续空转。
		if err := ctx.Err(); err != nil {
			p.logRound(result, cutoff, firstID, lastID)
			return result, err
		}
	}

	p.logRound(result, cutoff, firstID, lastID)
	return result, nil
}

// repairPatch 编译补写载荷：status_code 与 error_message 同语句落库（不变量 P2），
// 追溯标记落 error_stack（不进监视列集）。
func (p *Patrol) repairPatch(row store.UnsettledRequest) store.DetailsPatch {
	statusCode := RepairStatusCode
	message := RepairErrorMessage
	now := p.now()
	age := now.Sub(row.CreatedAt)
	if age < 0 {
		age = 0
	}
	// 只登记事实（何时补的、这行多旧、当时的阈值），不猜原因：巡检看不到断线与进程退出的
	// 区别，写进任何结论都会变成长期误导。
	marker := fmt.Sprintf(
		"%s repaired_at=%s age_ms=%d threshold_ms=%d",
		MarkerPrefix,
		now.UTC().Format(time.RFC3339),
		age.Milliseconds(),
		p.unsettledAfter.Milliseconds(),
	)
	return store.DetailsPatch{
		StatusCode:   &statusCode,
		ErrorMessage: &message,
		ErrorStack:   &marker,
	}
}

// 成本缺口扫描的样本上限：告警只带前几行 id，够反查即可；带全量会把日志变成数据导出。
const costGapSampleLimit = 10

// scanCostGaps 扫一轮「终态已提交、成本仍为空」的行：只计数并告警，不改任何行。
//
// 为什么不补写：补成本要复用 terminal 的 ResolveAndComputeCost，而它需要的输入链
// （模型价、long-context 溢价、分组倍率、本波 hedge 结果）只有结算路径手上才有；巡检
// 用近似价重算会静默错账，比留空更糟，也不得直写 usage_ledger（触发器才是它的真源）。
// 因此本阶段只做结构化告警：让人拿样本 id 去查为什么那笔成本没写进去。
func (p *Patrol) scanCostGaps(ctx context.Context, cutoff time.Time) int {
	costStore, ok := p.store.(CostGapStore)
	if !ok {
		return 0
	}
	cursor := store.UnsettledCursor{}
	found := 0
	sample := make([]int64, 0, costGapSampleLimit)
	for found < p.maxRowsPerRound {
		limit := min(p.batchSize, p.maxRowsPerRound-found)
		rows, err := costStore.ListCostGapRequests(ctx, cutoff, cursor, limit)
		if err != nil {
			p.logger.Warn("patrol_cost_gap_scan_failed", map[string]any{
				"found": found,
				"error": err.Error(),
			})
			break
		}
		if len(rows) == 0 {
			break
		}
		found += len(rows)
		for _, row := range rows {
			cursor = store.UnsettledCursor{CreatedAt: row.CreatedAt, ID: row.ID}
			if len(sample) < costGapSampleLimit {
				sample = append(sample, row.ID)
			}
		}
		if ctx.Err() != nil {
			break
		}
	}
	if found > 0 {
		p.logger.Warn("patrol_cost_gap_found", map[string]any{
			"found":      found,
			"sampleIds":  sample,
			"cutoffUnix": cutoff.Unix(),
		})
	}
	return found
}

// logRound 按轮记三个事件：发现、补写、失败（失败也逐行记，见 RunOnce）。
func (p *Patrol) logRound(result Result, cutoff time.Time, firstID, lastID int64) {
	if result.Found == 0 {
		return
	}
	p.logger.Warn("patrol_unsettled_found", map[string]any{
		"found":      result.Found,
		"repaired":   result.Repaired,
		"skipped":    result.Skipped,
		"failed":     result.Failed,
		"cutoffUnix": cutoff.Unix(),
	})
	if result.Repaired > 0 {
		p.logger.Warn("patrol_repaired", map[string]any{
			"repaired": result.Repaired,
			"firstId":  firstID,
			"lastId":   lastID,
			"status":   RepairStatusCode,
		})
	}
}

// Start 起后台巡检：立即跑一轮（阈值本身就是安全边界，不必先等一个间隔），随后按间隔重复。
// 重复调用只生效一次。
func (p *Patrol) Start(ctx context.Context) {
	p.mu.Lock()
	if p.running {
		p.mu.Unlock()
		return
	}
	runCtx, cancel := context.WithCancel(ctx)
	p.running = true
	p.cancel = cancel
	p.done = make(chan struct{})
	done := p.done
	p.mu.Unlock()

	go func() {
		defer close(done)
		p.runRound(runCtx)
		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				p.runRound(runCtx)
			}
		}
	}()
}

// Stop 停巡检并有界等待。不做无限等待：退出序列不能被巡检拖住。
func (p *Patrol) Stop() {
	p.mu.Lock()
	cancel, done, running := p.cancel, p.done, p.running
	p.running = false
	p.mu.Unlock()
	if !running {
		return
	}
	if cancel != nil {
		cancel()
	}
	select {
	case <-done:
	case <-time.After(stopTimeout):
		p.logger.Warn("patrol_stop_timeout", map[string]any{"timeoutMs": stopTimeout.Milliseconds()})
	}
}

// runRound 跑一轮并只记日志：轮次失败不该终止后台循环——下一轮的阈值仍然成立。
func (p *Patrol) runRound(ctx context.Context) {
	if _, err := p.RunOnce(ctx); err != nil {
		p.logger.Warn("patrol_round_failed", map[string]any{"error": err.Error()})
	}
}
