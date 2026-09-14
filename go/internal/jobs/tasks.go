package jobs

import (
	"context"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pubstatus"
)

// OpsTasks 组装本轮要点亮的后台任务集。
//
// 这是调度器**唯一**需要知道的入口：调度器只负责按 Every 触发 Run、串行化同一任务的重入，
// 任务语义全在各自文件里。返回空切片表示「本进程不跑任何后台任务」（例如未配 DSN）。
func OpsTasks(deps OpsDeps, options OpsTaskOptions) []OpsTask {
	tasks := make([]OpsTask, 0, 5)

	// 日志自动清理（Node 的 Bull cron「每日 cleanupSchedule」）：算子只支持固定间隔，
	// 到点判定与补跑窗口在 NewLogCleanup 内实现（见 internal/jobs/logcleanup.go 的说明）。
	if options.LogCleanup.Enabled && deps.Pools != nil {
		cleanup := NewLogCleanup(deps, options.LogCleanup)
		tasks = append(tasks, OpsTask{
			Name:  "log-cleanup",
			Every: options.LogCleanup.CheckInterval,
			Run:   cleanup.Run,
		})
	}

	if options.Probe.Enabled && deps.Pools != nil {
		probe := NewEndpointProbe(deps, options.Probe)
		tasks = append(tasks, OpsTask{
			Name:  "endpoint-probe",
			Every: options.Probe.TickInterval(),
			Run:   probe.Run,
		})
	}

	if options.ProbeLogCleanup.Enabled && deps.Pools != nil {
		cleanup := NewProbeLogCleanup(deps, options.ProbeLogCleanup)
		tasks = append(tasks, OpsTask{
			Name:  "endpoint-probe-log-cleanup",
			Every: probeLogCleanupEvery,
			Run:   cleanup.Run,
		})
	}

	if options.ReplayCleanup != nil && deps.Pools != nil {
		tasks = append(tasks, OpsTask{
			Name:  "replay-cleanup",
			Every: replayCleanupEvery,
			Run:   options.ReplayCleanup.Run,
		})
	}

	if options.OutboxReplay && deps.Redis != nil && deps.Pools != nil {
		replay := NewOutboxReplay(deps)
		tasks = append(tasks, OpsTask{
			Name:  "routing-trace-outbox-replay",
			Every: outboxEvery,
			Run:   replay.Run,
		})
	}

	// public-status 投影重建：Node 侧由 instrumentation.ts:749 无条件启动的调度器。
	// 它不需要 PG（桶在 Redis、配置快照也在 Redis），故只要求 Redis。
	if options.PublicStatusRebuild && deps.Redis != nil {
		rebuild := NewPublicStatusRebuild(
			deps,
			pubstatus.NewRedisProjectionRedis(deps.Redis),
			options.PublicStatusRetention,
			options.PublicStatusBootstrap,
		)
		tasks = append(tasks, OpsTask{
			Name:  "public-status-rebuild",
			Every: PublicStatusRebuildEvery,
			Run:   rebuild.Run,
		})
	}

	return tasks
}

// OpsTaskOptions 是任务集的开关与配置。
type OpsTaskOptions struct {
	Probe           ProbeConfig
	ProbeLogCleanup ProbeLogCleanupConfig
	// LogCleanup 是日志自动清理（审计缺口 G4；Node 对应 Bull cron「每日 cleanupSchedule」）。
	LogCleanup    LogCleanupConfig
	ReplayCleanup *ReplayCleanup
	OutboxReplay  bool
	// PublicStatusRebuild 是否启用 public-status 投影重建（Node 无开关，装配方在
	// 有 Redis 时传 true）。
	PublicStatusRebuild bool
	// PublicStatusRetention 是投影保留期策略（高并发模式下压到 24 小时）；nil 即 30 天。
	PublicStatusRetention pubstatus.RetentionTTLResolver
	// PublicStatusBootstrap 是内部配置快照缺失时的自举发布钩子；nil 表示不自举。
	PublicStatusBootstrap func(ctx context.Context) error
}

// OpsTaskOptionsFromEnv 按 Node 的解析规则读全部开关。
func OpsTaskOptionsFromEnv(lookup OpsEnv) (OpsTaskOptions, ProbeEnvWarnings) {
	probe, warnings := ProbeConfigFromEnv(lookup)
	cleanup, cleanupWarnings := ProbeLogCleanupConfigFromEnv(lookup)

	options := OpsTaskOptions{
		Probe:           probe,
		ProbeLogCleanup: cleanup,
		LogCleanup:      LogCleanupConfigFromEnv(lookup),
		// outbox 回放没有独立开关：它只在有 Redis 时才注册（见 OpsTasks），
		// 与 Node 的无条件启动等价——Node 在无 Redis 时 getReadyRedis 返回 null，回放器空转。
		OutboxReplay: true,
	}
	if value, ok := opsLookup(lookup, "CCH_REPLAY_CLEANUP_ENABLED"); ok && (value == "false" || value == "0") {
		options.ReplayCleanup = nil
	}
	return options, append(warnings, cleanupWarnings...)
}
