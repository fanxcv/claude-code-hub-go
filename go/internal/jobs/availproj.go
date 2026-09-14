package jobs

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件复刻可用性投影回填：src/lib/availability/projection-worker.ts:75-179
// （bootstrapBackfill / enqueueBackfillChunk）。
//
// 回填做的是「把历史终态请求补投进 outbox_events」，投影桶由 outbox 消费侧累加；
// 本文件不实现消费侧（增量投影不在本轮范围）。
//
// 语义要点（照搬）：
//   - 幂等标记：projection_meta 里存在 key='backfill_done' 即整段跳过（先查一次，
//     拿到 advisory 锁后再查一次，避免并发实例重复入队）。
//   - 时间是「左闭右开」分块：from now-100d 到 now，每块 6 小时。
//   - 中断即放弃：ctx 被取消时不写完成标记，下次启动会重跑（未入队的块会被
//     proj_applied_requests 的 NOT EXISTS 谓词天然去重）。
//   - 锁名与 Node 同名，故并存期两端只有一个真正执行。

// backfillStore 是回填用到的 store 面（抽接口的理由同 pricesync.go：合成夹具不得驱动真实库的写入）。
type backfillStore interface {
	ProjectionBackfillDone(ctx context.Context) (bool, error)
	EnqueueProjectionBackfillChunk(ctx context.Context, fromISO string, toISO string) (int, error)
	MarkProjectionBackfillDone(ctx context.Context, rangeDays int, inserted int) error
}

const (
	// availBackfillRangeDays 对应 BACKFILL_RANGE_DAYS = 100（与 MAX_AVAILABILITY_QUERY_RANGE_DAYS 对齐）。
	availBackfillRangeDays = 100
	// availBackfillChunkHours 对应 BACKFILL_CHUNK_HOURS = 6。
	availBackfillChunkHours = 6
	// defaultAvailBackfillInterval 是「检查是否还需回填」的间隔。
	//
	// Node 只在进程启动时跑一次 bootstrap；Go 侧也用间隔重查，但重查本身很轻
	// （一条 projection_meta 主键查询），代价是单实例部署下多几次空转，
	// 收益是「首次回填因中断未完成」时无需重启进程即可续跑。
	defaultAvailBackfillInterval = 5 * time.Minute
)

// AvailBackfillOptions 是回填器的装配参数。
type AvailBackfillOptions struct {
	// Pools 是共享连接池；必填。
	Pools *store.Pools
	// Logger 为 nil 时写 stderr。
	Logger *logx.Logger
	// RangeDays 覆盖回填范围天数（测试用）。
	RangeDays int
	// ChunkHours 覆盖分块小时数（测试用）。
	ChunkHours int
	// Now 注入时钟（测试用）。
	Now func() time.Time

	// store 与 acquire 是包内测试的替换点（生产走 Pools + advisory 锁）。
	store   backfillStore
	acquire lockAcquirer
}

// AvailBackfill 是可用性投影回填器。
type AvailBackfill struct {
	pools      *store.Pools
	store      backfillStore
	acquire    lockAcquirer
	logger     *logx.Logger
	rangeDays  int
	chunkHours int
	now        func() time.Time
}

// AvailBackfillResult 描述一次回填的结果。
type AvailBackfillResult struct {
	// Skipped 表示已有 backfill_done 标记或未取得锁。
	Skipped bool
	// SkippedReason 为 "already_done" / "lock_held"。
	SkippedReason string
	// Inserted 是入队到 outbox_events 的事件数。
	Inserted int
	// Chunks 是实际执行的分块数。
	Chunks int
	// Interrupted 表示中途被 ctx 取消（未写完成标记）。
	Interrupted bool
}

// NewAvailBackfill 校验装配参数。
func NewAvailBackfill(options AvailBackfillOptions) (*AvailBackfill, error) {
	if options.Pools == nil {
		return nil, errors.New("jobs: 可用性投影回填缺少连接池")
	}
	logger := options.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	rangeDays := options.RangeDays
	if rangeDays <= 0 {
		rangeDays = availBackfillRangeDays
	}
	chunkHours := options.ChunkHours
	if chunkHours <= 0 {
		chunkHours = availBackfillChunkHours
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	backfillStoreImpl := options.store
	if backfillStoreImpl == nil {
		backfillStoreImpl = options.Pools
	}
	acquire := options.acquire
	if acquire == nil {
		acquire = func(ctx context.Context) (func(context.Context) error, bool, error) {
			lock, acquired, err := AcquireLeader(ctx, options.Pools, availBackfillLockName)
			if err != nil || !acquired {
				return nil, acquired, err
			}
			return lock.Release, true, nil
		}
	}
	return &AvailBackfill{
		pools:      options.Pools,
		store:      backfillStoreImpl,
		acquire:    acquire,
		logger:     logger,
		rangeDays:  rangeDays,
		chunkHours: chunkHours,
		now:        now,
	}, nil
}

// Task 返回可登记进 Scheduler 的任务定义（启动即跑一次，之后按 interval 复查标记）。
func (b *AvailBackfill) Task(interval time.Duration) Task {
	if interval <= 0 {
		interval = defaultAvailBackfillInterval
	}
	return Task{
		Name:     "availability-projection-backfill",
		Interval: interval,
		// 100 天 × 6 小时 = 400 个分块，每块一次 INSERT...SELECT；给足上限。
		Timeout: 10 * time.Minute,
		Run: func(ctx context.Context) error {
			_, err := b.RunOnce(ctx)
			return err
		},
	}
}

// RunOnce 执行一次回填（已完成则直接返回）。
func (b *AvailBackfill) RunOnce(ctx context.Context) (*AvailBackfillResult, error) {
	done, err := b.store.ProjectionBackfillDone(ctx)
	if err != nil {
		return nil, err
	}
	if done {
		return &AvailBackfillResult{Skipped: true, SkippedReason: "already_done"}, nil
	}

	release, acquired, err := b.acquire(ctx)
	if err != nil {
		return nil, err
	}
	if !acquired {
		b.logger.Info("avail_backfill_skipped_lock_held", map[string]any{"lock": availBackfillLockName})
		return &AvailBackfillResult{Skipped: true, SkippedReason: "lock_held"}, nil
	}
	if release != nil {
		defer func() {
			if releaseErr := release(context.WithoutCancel(ctx)); releaseErr != nil {
				b.logger.Warn("avail_backfill_lock_release_failed", map[string]any{"error": releaseErr.Error()})
			}
		}()
	}

	// 拿到锁后再查一次：并发实例可能刚写完标记。
	if done, err := b.store.ProjectionBackfillDone(ctx); err != nil {
		return nil, err
	} else if done {
		return &AvailBackfillResult{Skipped: true, SkippedReason: "already_done"}, nil
	}

	b.logger.Info("avail_backfill_started", map[string]any{
		"rangeDays":  b.rangeDays,
		"chunkHours": b.chunkHours,
	})

	end := b.now()
	start := end.Add(-time.Duration(b.rangeDays) * 24 * time.Hour)
	chunk := time.Duration(b.chunkHours) * time.Hour

	result := &AvailBackfillResult{}
	startedAt := time.Now()
	for cursor := start; cursor.Before(end); cursor = cursor.Add(chunk) {
		if err := ctx.Err(); err != nil {
			result.Interrupted = true
			b.logger.Warn("avail_backfill_interrupted", map[string]any{
				"inserted": result.Inserted,
				"chunks":   result.Chunks,
			})
			// 中断不写完成标记：下次启动（或下次 tick）会重跑。
			return result, nil
		}
		to := cursor.Add(chunk)
		if to.After(end) {
			to = end
		}
		inserted, err := b.store.EnqueueProjectionBackfillChunk(
			ctx,
			cursor.UTC().Format(time.RFC3339Nano),
			to.UTC().Format(time.RFC3339Nano),
		)
		if err != nil {
			return result, err
		}
		result.Inserted += inserted
		result.Chunks++
	}

	if err := b.store.MarkProjectionBackfillDone(ctx, b.rangeDays, result.Inserted); err != nil {
		return result, err
	}
	b.logger.Info("avail_backfill_enqueue_finished", map[string]any{
		"inserted":  result.Inserted,
		"chunks":    result.Chunks,
		"elapsedMs": time.Since(startedAt).Milliseconds(),
	})
	return result, nil
}

// String 便于测试断言与日志（不参与业务逻辑）。
func (r AvailBackfillResult) String() string {
	if r.Skipped {
		return fmt.Sprintf("avail backfill skipped (%s)", r.SkippedReason)
	}
	return fmt.Sprintf("avail backfill inserted=%d chunks=%d interrupted=%t", r.Inserted, r.Chunks, r.Interrupted)
}
