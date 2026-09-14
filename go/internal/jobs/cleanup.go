package jobs

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// 语义出处（Node）：src/lib/provider-endpoints/probe-log-cleanup.ts、
// src/lib/replay-cleanup.ts、src/instrumentation.ts:326（replay 清理调度 10 分钟一跳）。
const (
	// probeLogCleanupEvery 是探活历史清理的固定间隔（Node 硬编码 24 小时）。
	probeLogCleanupEvery = 24 * time.Hour
	// replayCleanupEvery 是 replay 过期行清理的固定间隔（Node 硬编码 10 分钟）。
	replayCleanupEvery = 10 * time.Minute
	// replayCleanupMaxBatches 是单轮最多删几批（Node REPLAY_CLEANUP_MAX_BATCHES）。
	replayCleanupMaxBatches = 5
	// replayCleanupMaxDuration 是单轮的时间预算：清理不能长期占住连接。
	replayCleanupMaxDuration = 30 * time.Second
	// replayCleanupAcquireTimeout 是取连接的上限。
	//
	// 必须有界：池被请求路径占满时，后台任务要么等到一个连接、要么整轮跳过，
	// 绝不能无限期阻塞在取连接上——那会让一批 goroutine 永久挂住，进程再也关不干净。
	// 跳过是安全降级：下一轮（10 分钟后）再来。
	replayCleanupAcquireTimeout = 10 * time.Second
	// ReplayCleanupAdvisoryLock 与 Node 的 REPLAY_CLEANUP_LOCK_NAME 逐字一致：
	// 该锁的键是 hashtext(名字)，名字一致才能与 Node 互斥。
	ReplayCleanupAdvisoryLock = "claude-code-hub:replay-cleanup"
)

// ProbeLogCleanupConfig 是探活历史清理的配置。
type ProbeLogCleanupConfig struct {
	Enabled   bool
	Retention time.Duration
	BatchSize int
	LockTTL   time.Duration
}

// ProbeLogCleanupConfigFromEnv 按 Node 的解析规则读配置。
//
// 默认保留 1 天、单批 10000 行（Node probe-log-cleanup.ts:17）；`CI=true` 时不启动
// （Node 在 CI 里显式跳过，避免测试环境把共享库的历史删掉）。
func ProbeLogCleanupConfigFromEnv(lookup OpsEnv) (ProbeLogCleanupConfig, ProbeEnvWarnings) {
	cfg := ProbeLogCleanupConfig{
		Enabled:   true,
		Retention: 24 * time.Hour,
		BatchSize: 10_000,
		LockTTL:   5 * time.Minute,
	}
	var warnings ProbeEnvWarnings

	if value, ok := opsLookup(lookup, "CI"); ok && value == "true" {
		cfg.Enabled = false
	}
	if value, ok := opsLookup(lookup, "ENDPOINT_PROBE_LOG_RETENTION_DAYS"); ok {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 {
			warnings = append(warnings, "ENDPOINT_PROBE_LOG_RETENTION_DAYS="+value)
		} else {
			cfg.Retention = time.Duration(parsed) * 24 * time.Hour
		}
	}
	if value, ok := opsLookup(lookup, "ENDPOINT_PROBE_LOG_CLEANUP_BATCH_SIZE"); ok {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 {
			warnings = append(warnings, "ENDPOINT_PROBE_LOG_CLEANUP_BATCH_SIZE="+value)
		} else {
			cfg.BatchSize = parsed
		}
	}
	return cfg, warnings
}

// ProbeLogCleanup 删掉过期探活历史。
type ProbeLogCleanup struct {
	deps OpsDeps
	cfg  ProbeLogCleanupConfig
}

// NewProbeLogCleanup 构造清理任务。
func NewProbeLogCleanup(deps OpsDeps, cfg ProbeLogCleanupConfig) *ProbeLogCleanup {
	return &ProbeLogCleanup{deps: deps, cfg: cfg}
}

// Run 执行一轮（Node runCleanupOnce）：循环删批直到某一批没删满。
//
// 分批的意义：一次性 DELETE 会把整张表的历史都放进同一个事务，长事务会把 autovacuum 与
// 复制槽一起拖住；分批每批都能提交并释放锁。
func (c *ProbeLogCleanup) Run(ctx context.Context) (OpsOutcome, error) {
	if c.deps.Pools == nil {
		return OpsOutcome{}, errors.New("jobs: 探活历史清理需要数据库连接池")
	}

	lock, acquired, err := c.deps.AcquireOpsLeaderLock(ctx, ProbeLogCleanupLockKey, c.cfg.LockTTL)
	if err != nil {
		return OpsOutcome{}, err
	}
	if !acquired {
		return OpsOutcome{Fields: map[string]any{"skipped": "not_leader"}}, nil
	}
	defer func() { _ = lock.Release(context.WithoutCancel(ctx)) }()

	workCtx, cancelWork := context.WithCancel(ctx)
	defer cancelWork()
	stopKeepAlive := startOpsKeepAlive(workCtx, c.deps, lock, c.cfg.LockTTL, "probe_log_cleanup", cancelWork)
	defer stopKeepAlive()

	cutoff := c.deps.now().Add(-c.cfg.Retention)
	total := 0
	for {
		if workCtx.Err() != nil {
			break
		}
		deleted, err := c.deps.Pools.DeleteProbeLogsBeforeDateBatch(workCtx, cutoff, c.cfg.BatchSize)
		if err != nil {
			if total > 0 {
				c.deps.logger().Warn("probe_log_cleanup_partial", map[string]any{
					"totalDeleted": total,
					"error":        err.Error(),
				})
			}
			return OpsOutcome{Processed: total}, err
		}
		total += deleted
		if deleted < c.cfg.BatchSize {
			break
		}
	}

	if total > 0 {
		c.deps.logger().Info("probe_log_cleanup_completed", map[string]any{
			"retentionDays": c.cfg.Retention.Hours() / 24,
			"totalDeleted":  total,
		})
	}
	return OpsOutcome{
		Processed: total,
		Fields: map[string]any{
			"retention": c.cfg.Retention.String(),
			"deleted":   total,
		},
	}, nil
}

// ReplayCleanup 删掉过期的 replay 载荷（Node runReplayCleanupTick）。
//
// 与其它任务的差别：互斥走 **PG advisory lock**（名字与 Node 一致），不是 Redis 锁
// ——replay 落库本身就在 PG 上，用库内锁可以少一个依赖面。
type ReplayCleanup struct {
	deps OpsDeps
	// BatchSize 是单批删除上限（等于 replay store 的批大小，用于判断「是否删满」）。
	BatchSize int
	// Cleanup 执行单批删除；由 replay.Store.CleanupExpired 适配而来。
	Cleanup func(ctx context.Context, cutoff time.Time) (int, error)
}

// NewReplayCleanup 构造 replay 清理任务。
//
// batchSize 必须等于 cleanup 的内部批大小：驱动用「本批没删满即停」判断是否还有积压，
// 两者不一致会把每轮都当成删满，白转满 5 批。
func NewReplayCleanup(
	deps OpsDeps,
	batchSize int,
	cleanup func(ctx context.Context, cutoff time.Time) (int, error),
) *ReplayCleanup {
	return &ReplayCleanup{deps: deps, BatchSize: batchSize, Cleanup: cleanup}
}

// Run 执行一轮。
func (c *ReplayCleanup) Run(ctx context.Context) (OpsOutcome, error) {
	if c.deps.Pools == nil {
		return OpsOutcome{}, errors.New("jobs: replay 清理需要数据库连接池")
	}
	if c.Cleanup == nil {
		return OpsOutcome{}, errors.New("jobs: replay 清理未注入清理函数")
	}

	pool, err := c.deps.Pools.Control()
	if err != nil {
		return OpsOutcome{}, err
	}
	// advisory lock 是**会话级**的：取锁、干活、放锁必须同一条连接，故借一条原生连接独占整轮。
	//
	// 这条借连接不计入分道准入计数（Raw 的既有约定）：后台任务与请求路径抢同一个准入预算时，
	// 稳态下会是请求把清理饿死。代价由「整轮 ≤ 30 秒上限 + 有界取连接」兜住——
	// 拿不到连接就整轮跳过，既不阻塞也不抢占。
	acquireCtx, cancelAcquire := context.WithTimeout(ctx, replayCleanupAcquireTimeout)
	defer cancelAcquire()
	connection, err := pool.Raw().Acquire(acquireCtx)
	if err != nil {
		return OpsOutcome{Fields: map[string]any{"skipped": "pool_busy"}}, nil
	}
	defer connection.Release()

	var locked bool
	if err := connection.QueryRow(ctx,
		`SELECT pg_try_advisory_lock(hashtext($1))`, ReplayCleanupAdvisoryLock,
	).Scan(&locked); err != nil {
		return OpsOutcome{}, fmt.Errorf("jobs: replay 清理取锁失败: %w", err)
	}
	if !locked {
		return OpsOutcome{Fields: map[string]any{"skipped": "locked"}}, nil
	}
	defer func() {
		_, _ = connection.Exec(context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock(hashtext($1))`, ReplayCleanupAdvisoryLock)
	}()

	startedAt := c.deps.now()
	batches := 0
	deleted := 0
	for batches < replayCleanupMaxBatches {
		if c.deps.now().Sub(startedAt) >= replayCleanupMaxDuration || ctx.Err() != nil {
			break
		}
		cutoff := c.deps.now()
		batchDeleted, err := c.Cleanup(ctx, cutoff)
		if err != nil {
			return OpsOutcome{Processed: deleted, Fields: map[string]any{"batches": batches}}, err
		}
		batches++
		deleted += batchDeleted
		if batchDeleted < c.BatchSize {
			break
		}
	}

	return OpsOutcome{
		Processed: deleted,
		Fields: map[string]any{
			"batches": batches,
			"deleted": deleted,
		},
	}, nil
}

// startOpsKeepAlive 是通用锁续约（探活任务的续约多带一层「失去领导权即取消工作」）。
func startOpsKeepAlive(
	ctx context.Context,
	deps OpsDeps,
	lock *OpsLeaderLock,
	ttl time.Duration,
	tag string,
	onLost context.CancelFunc,
) func() {
	interval := opsMaxDuration(time.Second, ttl/2)
	done := make(chan struct{})
	var once sync.Once
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				renewCtx, cancel := context.WithTimeout(ctx, interval)
				renewed, err := lock.Renew(renewCtx, ttl)
				cancel()
				if err == nil && renewed {
					continue
				}
				deps.logger().Warn("ops_job_lock_lost", map[string]any{
					"task":    tag,
					"lockKey": lock.Key(),
					"error":   opsErrorText(err),
				})
				onLost()
				return
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}
