package main

import (
	"context"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/health"
	"github.com/fanxcv/claude-code-hub-go/go/internal/jobs"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pubstatus"
	"github.com/fanxcv/claude-code-hub-go/go/internal/replay"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件把运维类后台任务（探活 / 清理 / routing trace outbox 回收）接进进程启动。
//
// 为什么驱动放在 cmd 而不是 internal/jobs：那个包只回答「任务是什么」，
// 「什么时候跑、跑几轮、怎么停」属于进程装配。统一调度器（价格同步波次）落地后，
// 这里只需改成把 jobs.OpsTasks 的结果注册进去，任务体一行不用动。
//
// 两条纪律（与数据面同源）：
//
//  1. **就绪不等后台任务**：任务首轮在 goroutine 里跑，启动路径不阻塞。慢的 outbox 行
//     或一次卡住的拨测不该把 /readyz 拖成 503（Node 的同款注释见 routing-trace-outbox.ts:496）。
//  2. **单个任务失败不许拖垮进程**：任何一轮的 error 只记日志，下一个 tick 照常。
//     后台任务挂掉是「功能退化」，把网关一起带走是事故。

// opsShutdownTimeout 是停止后台任务的等待上限。
const opsShutdownTimeout = 5 * time.Second

// opsRuntime 是后台任务的驱动：每任务一个 goroutine + 独立 ticker。
type opsRuntime struct {
	cancel  context.CancelFunc
	wait    *sync.WaitGroup
	redis   redis.UniversalClient
	logger  *logx.Logger
	stopped bool
}

// startOpsRuntime 点亮后台任务集；未配 DSN 时返回 nil（骨架模式没有可跑的任务）。
func startOpsRuntime(
	parent context.Context,
	cfg config.Config,
	pools *store.Pools,
	logger *logx.Logger,
	lookup config.LookupEnvFunc,
) (*opsRuntime, error) {
	if pools == nil {
		logger.Info("ops_jobs_skipped", map[string]any{"reason": "no_dsn"})
		return nil, nil
	}

	redisClient, err := openCommandRedis(cfg)
	if err != nil {
		return nil, err
	}

	deps := jobs.OpsDeps{
		Pools:  pools,
		Redis:  redisClient,
		Logger: logger,
		// 熔断记账：探活成功归闭、失败计数。Settings 留空——端点级状态 TTL 不随设置收缩
		// （收缩只作用于厂级 TTL，探活不写厂级状态）。
		Health: health.NewWriter(health.Options{
			Redis:                         redisClient,
			EndpointCircuitBreakerEnabled: cfg.Env.EnableEndpointCircuitBreaker,
			Logger:                        logger,
		}),
	}

	options, warnings := jobs.OpsTaskOptionsFromEnv(lookup)
	for _, warning := range warnings {
		logger.Warn("ops_jobs_invalid_env", map[string]any{"value": warning})
	}

	// public-status 投影重建：Node 侧由 instrumentation.ts:749 **无条件**启动的调度器，
	// 没有独立开关；这里同样无条件点亮（任务本体自身会再判一次 Redis 是否可用）。
	//
	// 为何必需：投影（manifest/snapshot）只有这一环在写。停掉 Node 后若不跑它，
	// 公开状态页会停在最后一代，`freshUntil` 到期后逐步降级。
	options.PublicStatusRebuild = true
	options.PublicStatusRetention = func(defaultSeconds int) int {
		// 保留期策略取自系统设置（Node 的 resolveRedisRetentionTtlSeconds + 高并发模式开关）。
		// 读设置失败不阻断重建：退回默认保留期并记一次 warn（宁可多留数据，不可让投影停更）。
		settings, settingsErr := pools.FindSystemSettings(parent)
		if settingsErr != nil {
			logger.Warn("public_status_retention_settings_unavailable", map[string]any{
				"error": settingsErr.Error(),
				"note":  "回退默认保留期（30 天）",
			})
			return defaultSeconds
		}
		return pubstatus.HighConcurrencyRetentionTTL(defaultSeconds, settings.EnableHighConcurrencyMode)
	}

	// replay 过期行清理：走 PG advisory lock（名字与 Node 一致），故这里另建一个只做清理的
	// replay store——它只需要 Redis 句柄与连接池，不与数据面的回放存储抢状态。
	if options.ReplayCleanup == nil {
		replayStore, storeErr := replay.NewStore(replay.StoreOptions{Pools: pools, Redis: redisClient})
		if storeErr != nil {
			logger.Warn("ops_jobs_replay_cleanup_unavailable", map[string]any{"error": storeErr.Error()})
		} else {
			// BatchSize 必须等于 replay.Store.CleanupExpired 的内部批大小（见构造器注释）。
			options.ReplayCleanup = jobs.NewReplayCleanup(
				deps, replayCleanupBatchSize, replayStore.CleanupExpired,
			)
		}
	}

	tasks := jobs.OpsTasks(deps, options)
	if len(tasks) == 0 {
		logger.Info("ops_jobs_skipped", map[string]any{"reason": "no_tasks"})
		if redisClient != nil {
			_ = redisClient.Close()
		}
		return nil, nil
	}

	ctx, cancel := context.WithCancel(parent)
	runtime := &opsRuntime{
		cancel: cancel,
		wait:   &sync.WaitGroup{},
		redis:  redisClient,
		logger: logger,
	}

	for _, task := range tasks {
		interval := task.Every
		if interval <= 0 {
			interval = time.Minute
		}
		run := task.Run
		name := task.Name
		runtime.wait.Add(1)
		go func() {
			defer runtime.wait.Done()
			// 首轮立即执行：等一个完整间隔才动，会让重启后的恢复白等一分钟。
			runOpsRound(ctx, logger, name, run)
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					runOpsRound(ctx, logger, name, run)
				}
			}
		}()
		logger.Info("ops_job_registered", map[string]any{
			"task":    name,
			"everyMs": interval.Milliseconds(),
		})
	}
	return runtime, nil
}

// replayCleanupBatchSize 与 replay.Store.CleanupExpired 的内部批大小一致（Node 的
// REPLAY_CLEANUP_BATCH_SIZE 也是 100）。
const replayCleanupBatchSize = 100

// runOpsRound 执行一轮并把结果落日志；错误只降级不冒泡。
func runOpsRound(
	ctx context.Context,
	logger *logx.Logger,
	name string,
	run func(context.Context) (jobs.OpsOutcome, error),
) {
	startedAt := time.Now()
	outcome, err := run(ctx)
	fields := map[string]any{
		"task":     name,
		"duration": time.Since(startedAt).Milliseconds(),
	}
	for key, value := range outcome.Fields {
		fields[key] = value
	}
	if outcome.Processed > 0 {
		fields["processed"] = outcome.Processed
	}
	if err != nil {
		fields["error"] = err.Error()
		logger.Warn("ops_job_round_failed", fields)
		return
	}
	logger.Info("ops_job_round_completed", fields)
}

// Stop 停止全部任务并等它们收尾（有界）。
//
// 取消后仍要等：正在跑的一轮可能刚写完探活结果，直接关连接池会把它带着一起消失。
func (r *opsRuntime) Stop() {
	if r == nil || r.stopped {
		return
	}
	r.stopped = true
	r.cancel()

	done := make(chan struct{})
	go func() {
		r.wait.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(opsShutdownTimeout):
		r.logger.Warn("ops_jobs_stop_timeout", map[string]any{
			"timeoutMs": opsShutdownTimeout.Milliseconds(),
		})
	}
	if r.redis != nil {
		if err := r.redis.Close(); err != nil {
			r.logger.Warn("ops_jobs_redis_close_failed", map[string]any{"error": err.Error()})
		}
	}
}
