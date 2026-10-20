package jobs

import (
	"context"
	"errors"
	"time"
)

// 本文件是 `outbox_events` 保留期清理的后台驱动。
//
// 为什么需要它：这张表只增不减。唯一读方 `ClaimOutboxBatch`（store/availability_projection_consume.go）
// 带 `WHERE published_at IS NULL`，**已发布行没有任何读路径**；而幂等去重落在
// `proj_applied_requests(request_id)` 主键上，与 outbox 行在不在无关。故按时间删已发布行
// 不削弱任何保证，只是把 88 万行 / 483 MB 的沉淀收掉。
//
// 与 probe_log_cleanup / replay_cleanup 的差别只有一处：**删除谓词必须同时含
// `published_at IS NOT NULL`**（见 store/outbox_cleanup.go）。未发布行是投影消费的唯一输入，
// 删掉即永久缺口——回填标记已 done，没有补偿路径。这条谓词由 store 包的文本钉子与真库负例
// 双重守住，本文件不再重复判断。

const (
	// OpsOutboxEventsRetentionLockKey 是 Go 副本之间的互斥键（本包既有 `locks:*` 惯例）。
	OpsOutboxEventsRetentionLockKey = "locks:outbox-events-retention"
	// outboxEventsRetentionBatchSize 是单批删除上限：一批一个事务，批有界则锁与 WAL 有界。
	outboxEventsRetentionBatchSize = 1_000
	// outboxEventsRetentionMaxBatches 是单轮的批数上限。
	//
	// 首轮要清的是近百万行（生产实测 88 万），一轮跑完要 800 多批；上限 1000 批让首轮能一次
	// 收敛，同时给「表比预期大一个量级」时的失控兜住上界（余量留下一轮）。
	outboxEventsRetentionMaxBatches = 1_000
	// outboxEventsRetentionMaxDuration 是单轮的时间预算，须小于 opsRoundTimeout（10 分钟）：
	// 清理不能长期占住一条连接，也不该把整轮拖进超时日志里。
	outboxEventsRetentionMaxDuration = 5 * time.Minute
	// outboxEventsRetentionLockTTL 是互斥租约：远大于一轮的常见耗时（分钟级）。
	outboxEventsRetentionLockTTL = 30 * time.Minute
	// outboxEventsRetentionDefault 与 outboxEventsRetentionDefaultInterval 是出厂值，
	// 与 config 的 CCH_JOB_OUTBOX_RETENTION_DAYS / CCH_JOB_OUTBOX_CLEANUP_INTERVAL_MS 一致。
	outboxEventsRetentionDefault         = 7 * 24 * time.Hour
	outboxEventsRetentionDefaultInterval = time.Hour
)

// OpsOutboxEventsRetentionConfig 是本任务的配置（由 config.CCH_JOB_OUTBOX_* 翻译而来）。
type OpsOutboxEventsRetentionConfig struct {
	// Retention 是已发布行的保留期；<=0 取出厂值 7 天。
	Retention time.Duration
	// Interval 是触发间隔；<=0 取出厂值 1 小时。
	Interval time.Duration
}

// OpsOutboxEventsRetention 按时间清理已发布的 outbox 事件。
type OpsOutboxEventsRetention struct {
	deps OpsDeps
	cfg  OpsOutboxEventsRetentionConfig
	// deleteBatch 是单批删除执行面（`*store.Pools` 满足）。
	deleteBatch func(ctx context.Context, before time.Time, batchSize int) (int, error)
}

// NewOpsOutboxEventsRetention 构造清理任务；缺省值在这里补齐，装配侧只传显式配置。
func NewOpsOutboxEventsRetention(
	deps OpsDeps,
	cfg OpsOutboxEventsRetentionConfig,
) *OpsOutboxEventsRetention {
	if cfg.Retention <= 0 {
		cfg.Retention = outboxEventsRetentionDefault
	}
	if cfg.Interval <= 0 {
		cfg.Interval = outboxEventsRetentionDefaultInterval
	}
	task := &OpsOutboxEventsRetention{deps: deps, cfg: cfg}
	if deps.Pools != nil {
		task.deleteBatch = deps.Pools.DeletePublishedOutboxBeforeBatch
	}
	return task
}

// 注册形状的编译期证明：Run 与 OpsTask.Run 同型，接进 OpsTasks 不需要适配器。
var _ = OpsTask{Name: "outbox-events-retention", Run: (*OpsOutboxEventsRetention)(nil).Run}

// Interval 返回触发间隔（缺省值已在构造时补齐）。
func (c *OpsOutboxEventsRetention) Interval() time.Duration { return c.cfg.Interval }

// Run 执行一轮：循环删批，直到某一批没删满、达到批数/时长上限、或本轮被取消。
//
// 返回的 OpsOutcome.Processed 是**本轮实际删除的行数**（未获锁时为 0）；Fields 带
// deleted / batches / retentionDays，让运维能看出首轮进度（88 万行不会一轮一条日志地刷屏，
// 但每轮都能看到已删多少）。
func (c *OpsOutboxEventsRetention) Run(ctx context.Context) (OpsOutcome, error) {
	if c.deleteBatch == nil {
		// 与其它任务同判：装配缺陷显式可见，不静默当「本轮无工作」。
		return OpsOutcome{}, errors.New("jobs: outbox 保留期清理需要数据库连接池")
	}

	lock, acquired, err := c.deps.AcquireOpsLeaderLock(
		ctx, OpsOutboxEventsRetentionLockKey, outboxEventsRetentionLockTTL,
	)
	if err != nil {
		return OpsOutcome{}, err
	}
	if !acquired {
		return OpsOutcome{Fields: map[string]any{"skipped": "not_leader"}}, nil
	}
	defer func() { _ = lock.Release(context.WithoutCancel(ctx)) }()

	workCtx, cancelWork := context.WithCancel(ctx)
	defer cancelWork()
	stopKeepAlive := startOpsKeepAlive(
		workCtx, c.deps, lock, outboxEventsRetentionLockTTL, "outbox_events_retention", cancelWork,
	)
	defer stopKeepAlive()

	cutoff := c.deps.now().Add(-c.cfg.Retention)
	startedAt := c.deps.now()
	deleted, batches := 0, 0
	for batches < outboxEventsRetentionMaxBatches &&
		c.deps.now().Sub(startedAt) < outboxEventsRetentionMaxDuration &&
		workCtx.Err() == nil {
		batchDeleted, err := c.deleteBatch(workCtx, cutoff, outboxEventsRetentionBatchSize)
		if err != nil {
			if deleted > 0 {
				c.deps.logger().Warn("outbox_retention_partial", map[string]any{
					"deleted": deleted,
					"batches": batches,
					"error":   err.Error(),
				})
			}
			return OpsOutcome{Processed: deleted, Fields: map[string]any{"batches": batches}}, err
		}
		batches++
		deleted += batchDeleted
		if batchDeleted < outboxEventsRetentionBatchSize {
			break
		}
	}

	fields := map[string]any{
		"retentionDays": c.cfg.Retention.Hours() / 24,
		"deleted":       deleted,
		"batches":       batches,
	}
	if deleted > 0 {
		// 耗时由 runOpsRound 的 ops_job_round_completed 记（每轮都记，不只删到行时）；
		// 本条日志只补「删了多少、跑了几批」这类进度信息。
		c.deps.logger().Info("outbox_retention_completed", fields)
	}
	return OpsOutcome{Processed: deleted, Fields: fields}, nil
}
