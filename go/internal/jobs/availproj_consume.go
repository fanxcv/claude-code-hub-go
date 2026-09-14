package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件复刻可用性投影的 **outbox 消费侧**：src/lib/availability/projection-worker.ts:258-508
// （processBatch / runCycle / startAvailabilityProjectionWorker）。
//
// 与 availproj.go（回填，Node 的 :75-179）的分工：回填把历史终态请求**补投进 outbox**，
// 本文件把 outbox 里的事件**投影成 1 分钟桶与 avail_current**。两者缺一，投影就不完整：
// 只有回填 → 事件越积越多而桶永远空（这正是本轮修的缺口 G1）。
//
// 事务边界与 Node 一致：认领 → 幂等登记 → 累加桶 → 标记已发布 → 重算，**全在一个事务里**
// （见 store.RunProjectionTx）。任一步失败整体回滚，「认领」随之中止——Node 没有独立的
// visibility timeout，靠的就是这个语义。

// 复刻 projection-worker.ts:13-15 的常量。
const (
	// availProjectionBatchSize 对应 BATCH = 300。
	availProjectionBatchSize = 300
	// availProjectionMaxBatches 对应 runCycle 的 `for (let i = 0; i < 20; i++)`。
	availProjectionMaxBatches = 20
	// availProjectionBusyDelay 对应 BUSY_MS = 10。
	availProjectionBusyDelay = 10 * time.Millisecond
	// availCurrentWindowMinutes 对应 availability-service.ts:46 的
	// CURRENT_PROVIDER_STATUS_WINDOW_MINUTES = 15。
	availCurrentWindowMinutes = 15
	// defaultAvailProjectionInterval 是消费侧的调度间隔。
	//
	// Node 的 TICK_MS = 200ms；Go 侧默认取 1s：本仓消费侧**先取一把 advisory 锁**（见 §差异），
	// 200ms 就意味着每秒 5 次加锁往返，而投影新鲜度到秒级已远超需求（读侧窗口是分钟级）。
	// 需要的部署可直接用环境变量调回 200ms。
	defaultAvailProjectionInterval = time.Second
)

// ProjectionTxOps 是消费侧需要的事务内原语（store.ProjectionTx 满足；单测用假实现）。
type ProjectionTxOps interface {
	ClaimOutboxBatch(ctx context.Context, limit int) ([]store.ProjectionOutboxEvent, error)
	InsertAppliedRequest(ctx context.Context, requestID int64, eventID string) (bool, error)
	UpsertAvailBuckets(ctx context.Context, deltas []store.ProjectionBucketDelta) error
	MarkOutboxPublished(ctx context.Context, ids []int64, lastError *string) error
	RecomputeAvailCurrent(ctx context.Context, providerIDs []int64, windowMinutes int) error
}

// ProjectionRunner 开一个事务并把句柄交给 fn（store.Pools 经适配器满足）。
//
// 抽这一层是为了「编排可单测」：假实现可以在内存里模拟 outbox 与桶，不必起真库。
type ProjectionRunner interface {
	RunProjectionTx(ctx context.Context, fn func(ctx context.Context, tx ProjectionTxOps) error) error
}

// storeProjectionRunner 把 store.Pools 适配成 ProjectionRunner。
type storeProjectionRunner struct{ pools *store.Pools }

// RunProjectionTx 实现 ProjectionRunner。
func (r storeProjectionRunner) RunProjectionTx(
	ctx context.Context,
	fn func(ctx context.Context, tx ProjectionTxOps) error,
) error {
	return r.pools.RunProjectionTx(ctx, func(ctx context.Context, tx *store.ProjectionTx) error {
		return fn(ctx, tx)
	})
}

// AvailProjectionConsumerOptions 是消费侧装配参数。
type AvailProjectionConsumerOptions struct {
	// Pools 是共享连接池；必填（除非包内测试注入 runner）。
	Pools *store.Pools
	// Logger 为 nil 时写 stderr。
	Logger *logx.Logger
	// BatchSize 覆盖单批上限（测试用）。
	BatchSize int
	// WindowMinutes 覆盖重算窗口（测试用）。
	WindowMinutes int
	// MaxBatches 覆盖单轮最多批数（测试用）。
	MaxBatches int
	// BusyDelay 覆盖批间等待（测试用）。
	BusyDelay time.Duration
	// Now 注入时钟（测试用；当前实现只在日志里用到）。
	Now func() time.Time

	// runner / acquire / decoder 是包内测试的替换点。
	runner  ProjectionRunner
	acquire lockAcquirer
	decoder func([]byte) map[string]any
}

// AvailProjectionConsumer 消费 outbox_events 并维护投影桶与 avail_current。
type AvailProjectionConsumer struct {
	runner        ProjectionRunner
	acquire       lockAcquirer
	decoder       func([]byte) map[string]any
	logger        *logx.Logger
	batchSize     int
	windowMinutes int
	maxBatches    int
	busyDelay     time.Duration
	now           func() time.Time

	mu      sync.Mutex
	running bool
}

// AvailProjectionConsumeResult 描述一次 RunOnce 的结果。
type AvailProjectionConsumeResult struct {
	// Skipped 表示未取得锁（另一实例在消费）。
	Skipped bool
	// Applied 是本次真正新应用的请求数（幂等登记成功数）。
	Applied int
	// Batches 是实际执行的批数。
	Batches int
}

// NewAvailProjectionConsumer 校验装配参数。
func NewAvailProjectionConsumer(options AvailProjectionConsumerOptions) (*AvailProjectionConsumer, error) {
	logger := options.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	runner := options.runner
	if runner == nil {
		if options.Pools == nil {
			return nil, errors.New("jobs: 可用性投影消费缺少连接池")
		}
		runner = storeProjectionRunner{pools: options.Pools}
	}
	acquire := options.acquire
	if acquire == nil {
		if options.Pools == nil {
			return nil, errors.New("jobs: 可用性投影消费缺少连接池（无法取锁）")
		}
		acquire = func(ctx context.Context) (func(context.Context) error, bool, error) {
			lock, acquired, err := AcquireLeader(ctx, options.Pools, availBackfillLockName)
			if err != nil || !acquired {
				return nil, acquired, err
			}
			return lock.Release, true, nil
		}
	}
	decoder := options.decoder
	if decoder == nil {
		decoder = store.DecodeProjectionPayload
	}

	batchSize := options.BatchSize
	if batchSize <= 0 {
		batchSize = availProjectionBatchSize
	}
	windowMinutes := options.WindowMinutes
	if windowMinutes <= 0 {
		windowMinutes = availCurrentWindowMinutes
	}
	maxBatches := options.MaxBatches
	if maxBatches <= 0 {
		maxBatches = availProjectionMaxBatches
	}
	busyDelay := options.BusyDelay
	if busyDelay < 0 {
		busyDelay = 0
	}
	if busyDelay == 0 {
		busyDelay = availProjectionBusyDelay
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}

	return &AvailProjectionConsumer{
		runner:        runner,
		acquire:       acquire,
		decoder:       decoder,
		logger:        logger,
		batchSize:     batchSize,
		windowMinutes: windowMinutes,
		maxBatches:    maxBatches,
		busyDelay:     busyDelay,
		now:           now,
	}, nil
}

// Task 返回可登记进 Scheduler 的任务定义（复刻 runCycle 的单轮语义）。
func (c *AvailProjectionConsumer) Task(interval time.Duration) Task {
	if interval <= 0 {
		interval = defaultAvailProjectionInterval
	}
	return Task{
		Name:     "availability-projection-consume",
		Interval: interval,
		// 20 批 × 300 行，每批多条语句；与回填同档上限。
		Timeout: 10 * time.Minute,
		Run: func(ctx context.Context) error {
			_, err := c.RunOnce(ctx)
			return err
		},
	}
}

// RunOnce 跑一轮消费（单例；复刻 runCycle 的 20 批上限与批间等待）。
func (c *AvailProjectionConsumer) RunOnce(ctx context.Context) (*AvailProjectionConsumeResult, error) {
	c.mu.Lock()
	if c.running {
		c.mu.Unlock()
		return &AvailProjectionConsumeResult{Skipped: true}, nil
	}
	c.running = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.running = false
		c.mu.Unlock()
	}()

	release, acquired, err := c.acquire(ctx)
	if err != nil {
		return nil, err
	}
	if !acquired {
		c.logger.Info("avail_projection_consume_skipped_lock_held", map[string]any{"lock": availBackfillLockName})
		return &AvailProjectionConsumeResult{Skipped: true}, nil
	}
	if release != nil {
		defer func() {
			if releaseErr := release(context.WithoutCancel(ctx)); releaseErr != nil {
				c.logger.Warn("avail_projection_consume_lock_release_failed", map[string]any{"error": releaseErr.Error()})
			}
		}()
	}

	result := &AvailProjectionConsumeResult{}
	for i := 0; i < c.maxBatches; i++ {
		if err := ctx.Err(); err != nil {
			return result, nil
		}
		applied, err := c.processBatch(ctx)
		if err != nil {
			// 与 Node 同判：单轮失败只记 warn，不回滚已提交的批（每批自成一个事务）。
			c.logger.Warn("avail_projection_consume_cycle_failed", map[string]any{
				"error":   err.Error(),
				"applied": result.Applied,
				"batches": result.Batches,
			})
			return result, err
		}
		result.Applied += applied
		result.Batches++
		if applied == 0 || applied < c.batchSize {
			break
		}
		if c.busyDelay > 0 {
			time.Sleep(c.busyDelay)
		}
	}
	if result.Applied > 0 {
		c.logger.Info("avail_projection_projected_events", map[string]any{
			"count":   result.Applied,
			"batches": result.Batches,
		})
	}
	return result, nil
}

// processBatch 是单批消费（复刻 projection-worker.ts:258-447），全程一个事务。
func (c *AvailProjectionConsumer) processBatch(ctx context.Context) (int, error) {
	applied := 0
	err := c.runner.RunProjectionTx(ctx, func(ctx context.Context, tx ProjectionTxOps) error {
		claimed, err := tx.ClaimOutboxBatch(ctx, c.batchSize)
		if err != nil {
			return err
		}
		if len(claimed) == 0 {
			return nil
		}

		// 同一 (provider, 分钟) 的事件在内存里先合并，再逐桶 upsert——Node 同法（bucketDeltas）。
		type bucketKey struct {
			providerID  int64
			bucketStart time.Time
		}
		deltas := make(map[bucketKey]*store.ProjectionBucketDelta)
		touched := make(map[int64]struct{})
		published := make([]int64, 0, len(claimed))
		invalid := make([]int64, 0)

		for _, row := range claimed {
			payload := c.decoder(row.Payload)
			requestID, okRequest := asProjectionInt64(payload["request_id"])
			providerID, okProvider := asProjectionInt64(payload["provider_id"])
			occurredAt, okOccurred := asProjectionTimestamp(payload["occurred_at"])
			if !okRequest || !okProvider || !okOccurred {
				invalid = append(invalid, row.ID)
				continue
			}

			fresh, err := tx.InsertAppliedRequest(ctx, requestID, row.EventID)
			if err != nil {
				return err
			}
			if fresh {
				outcome := asProjectionOutcome(payload["outcome"])
				durationMS, hasDuration := asProjectionFloat(payload["duration_ms"])

				// 计数与 Node 逐行对齐（projection-worker.ts:318-320）：
				// **只有字面 "excluded" 计 excluded**；未知 outcome（如拼写错）三种计数都不加。
				// 这一条是 golden 对拍揭出来的：我最初把「非 success/failure」都当 excluded，
				// 于是 provider 102 的 excluded_cnt 变成 3 而 Node 是 2。
				successCnt, failureCnt, excludedCnt := 0, 0, 0
				switch outcome {
				case "success":
					successCnt = 1
				case "failure":
					failureCnt = 1
				case "excluded":
					excludedCnt = 1
				}
				latencyCnt := 0
				latencySum := int64(0)
				if (outcome == "success" || outcome == "failure") && hasDuration {
					latencyCnt = 1
					latencySum = int64(math.Trunc(durationMS))
				}

				bucketStart := occurredAt.UTC().Truncate(time.Minute)
				key := bucketKey{providerID: providerID, bucketStart: bucketStart}
				if prev, ok := deltas[key]; ok {
					prev.SuccessCnt += successCnt
					prev.FailureCnt += failureCnt
					prev.ExcludedCnt += excludedCnt
					prev.LatencyCnt += latencyCnt
					prev.LatencySumMS += latencySum
					if occurredAt.After(prev.LastRequestAt) {
						prev.LastRequestAt = occurredAt
					}
				} else {
					deltas[key] = &store.ProjectionBucketDelta{
						ProviderID:    providerID,
						BucketStart:   bucketStart,
						SuccessCnt:    successCnt,
						FailureCnt:    failureCnt,
						ExcludedCnt:   excludedCnt,
						LatencyCnt:    latencyCnt,
						LatencySumMS:  latencySum,
						LastRequestAt: occurredAt,
					}
				}
				touched[providerID] = struct{}{}
				applied++
			}

			published = append(published, row.ID)
		}

		bucketDeltas := make([]store.ProjectionBucketDelta, 0, len(deltas))
		for _, delta := range deltas {
			bucketDeltas = append(bucketDeltas, *delta)
		}
		if err := tx.UpsertAvailBuckets(ctx, bucketDeltas); err != nil {
			return err
		}
		if err := tx.MarkOutboxPublished(ctx, published, nil); err != nil {
			return err
		}
		if len(invalid) > 0 {
			reason := "invalid payload"
			if err := tx.MarkOutboxPublished(ctx, invalid, &reason); err != nil {
				return err
			}
		}

		touchedIDs := make([]int64, 0, len(touched))
		for id := range touched {
			touchedIDs = append(touchedIDs, id)
		}
		return tx.RecomputeAvailCurrent(ctx, touchedIDs, c.windowMinutes)
	})
	if err != nil {
		return 0, err
	}
	return applied, nil
}

// asProjectionInt64 复刻 Node 的 `Number(x)` + `Number.isFinite` 判定。
//
// 接受 JSON number 与数字字符串（Node 的 Number("123") === 123）；空值与 NaN 一律不可用。
func asProjectionInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return 0, false
		}
		return int64(typed), true
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		if err == nil {
			return parsed, true
		}
		float, err := typed.Float64()
		if err != nil || math.IsNaN(float) {
			return 0, false
		}
		return int64(float), true
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return 0, false
		}
		parsed, err := strconv.ParseInt(trimmed, 10, 64)
		if err == nil {
			return parsed, true
		}
		float, err := strconv.ParseFloat(trimmed, 64)
		if err != nil || math.IsNaN(float) || math.IsInf(float, 0) {
			return 0, false
		}
		return int64(float), true
	default:
		return 0, false
	}
}

// asProjectionFloat 复刻 Node 的 `Number(x)`：空值不可用，非数字字符串同样不可用。
func asProjectionFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case nil:
		return 0, false
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return 0, false
		}
		return typed, true
	case int64:
		return float64(typed), true
	case int:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil || math.IsNaN(parsed) {
			return 0, false
		}
		return parsed, true
	case string:
		trimmed := strings.TrimSpace(typed)
		if trimmed == "" {
			return 0, false
		}
		parsed, err := strconv.ParseFloat(trimmed, 64)
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

// asProjectionOutcome 复刻 Node 的 `String(payload.outcome || "excluded")`：
// **JS 的 `||` 对一切 falsy 生效**——`null`/`undefined`/`""`/`0`/`false` 都归为 "excluded"，
// 而真值（含非零数字、true、非空串）先字符串化再参与三分支判定。
func asProjectionOutcome(value any) string {
	switch typed := value.(type) {
	case nil:
		return "excluded"
	case bool:
		if !typed {
			return "excluded"
		}
		return "true"
	case float64:
		if typed == 0 {
			return "excluded"
		}
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case int:
		if typed == 0 {
			return "excluded"
		}
		return strconv.Itoa(typed)
	case int64:
		if typed == 0 {
			return "excluded"
		}
		return strconv.FormatInt(typed, 10)
	case json.Number:
		if typed.String() == "0" {
			return "excluded"
		}
		return typed.String()
	case string:
		if typed == "" {
			return "excluded"
		}
		return typed
	default:
		return fmt.Sprint(value)
	}
}

// asProjectionString 复刻 Node 的 `String(x)`（仅用于非 outcome 的展示类字段）。
func asProjectionString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	default:
		return fmt.Sprint(value)
	}
}

// asProjectionTimestamp 解析 occurred_at。
//
// 与 Node 的**有意差异**（登记在报告 §3）：Node 用 `new Date(x)`，值非法时得到 Invalid Date，
// 随后 `toISOString()` **抛异常 → 整事务回滚 → 该行永远重投**（毒丸会卡住批次）。
// 这里把「不可解析的时刻」判为 invalid payload（标记已发布 + last_error），即**不阻塞**。
// 这是更保守的取舍：宁可丢一条坏事件并把原因写进 last_error，也不让整个投影停摆。
func asProjectionTimestamp(value any) (time.Time, bool) {
	text, ok := value.(string)
	if !ok {
		return time.Time{}, false
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, trimmed)
	if err != nil {
		return time.Time{}, false
	}
	return parsed, true
}
