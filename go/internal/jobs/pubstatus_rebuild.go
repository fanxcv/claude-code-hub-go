package jobs

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pubstatus"
)

// public-status 投影重建的调度器（Node：`src/lib/public-status/scheduler.ts`）。
//
// 为什么它是「Node 下线后状态页仍能自更新」的最后一环：Node 侧由
// `instrumentation.ts:749` 启动 `startPublicStatusRebuildScheduler()`，每 30 秒跑一轮：
// 取 Redis 领导权 → 收集需要重建的目标（配置默认档 + 重建提示）→ 逐个重建 → 消费提示。
// 停掉 Node 后若没有这一环，公开状态页会停在最后一代，`freshUntil` 到期后逐步降级。
//
// 与其它 ops 任务的区别：Node 这一环的领导锁**在自己的 Redis 键**上
// （`locks:public-status-rebuild-scheduler`，TTL 30 秒），不是数据库建议锁。这里沿用同一
// 键与同一 TTL——双跑期两侧因此天然互斥，不会同时重建同一代。

const (
	// PublicStatusRebuildLockKey 复刻 Node 的 LOCK_KEY（scheduler.ts:15）。
	PublicStatusRebuildLockKey = "locks:public-status-rebuild-scheduler"
	// publicStatusRebuildLockTTL 复刻 LOCK_TTL_MS = 30_000（scheduler.ts:17）。
	publicStatusRebuildLockTTL = 30 * time.Second
	// PublicStatusRebuildEvery 复刻 TICK_INTERVAL_MS = 30_000（scheduler.ts:16）。
	PublicStatusRebuildEvery = 30 * time.Second
	// publicStatusRebuildHintLimit 复刻 scanPattern(..., 100)（scheduler.ts:116）。
	publicStatusRebuildHintLimit = 100
)

// publicStatusRebuildHintPattern 复刻 parseRebuildHintKey 的正则（scheduler.ts:32）。
var publicStatusRebuildHintPattern = regexp.MustCompile(`rebuild-hint:(\d+)m:(\d+)h$`)

// PublicStatusRebuild 是投影重建任务。
type PublicStatusRebuild struct {
	deps  OpsDeps
	redis pubstatus.ProjectionRedis
	// retention 由装配方决定（高并发模式下压到 24 小时）；nil 即 30 天。
	retention pubstatus.RetentionTTLResolver
	// bootstrap 是内部配置快照缺失时的自举钩子（Node 的
	// publishCurrentPublicStatusConfigProjection）；nil 表示不自举。
	bootstrap func(ctx context.Context) error
}

// NewPublicStatusRebuild 建任务；redis 为 nil 时任务每轮都会直接返回
// （与 Node 在 `redis?.status !== "ready"` 时跳过一轮同判）。
func NewPublicStatusRebuild(
	deps OpsDeps,
	redis pubstatus.ProjectionRedis,
	retention pubstatus.RetentionTTLResolver,
	bootstrap func(ctx context.Context) error,
) *PublicStatusRebuild {
	return &PublicStatusRebuild{deps: deps, redis: redis, retention: retention, bootstrap: bootstrap}
}

// Run 执行一轮（复刻 scheduler.ts:132-196 的 runCycle）。
func (t *PublicStatusRebuild) Run(ctx context.Context) (OpsOutcome, error) {
	if t.redis == nil || !t.redis.Ready(ctx) {
		return OpsOutcome{
			Fields: map[string]any{"publicStatusRebuild": "redis_unavailable"},
		}, nil
	}

	lock, acquired, err := t.deps.AcquireOpsLeaderLock(ctx, PublicStatusRebuildLockKey, publicStatusRebuildLockTTL)
	if err != nil {
		return OpsOutcome{}, err
	}
	if !acquired {
		// 另一进程（含双跑期的 Node）正在跑这一轮：静默跳过，不是错误。
		return OpsOutcome{
			Fields: map[string]any{"publicStatusRebuild": "not_leader"},
		}, nil
	}
	defer func() { _ = lock.Release(ctx) }()

	targets := t.collectTargets(ctx)
	updated := 0
	skipped := 0
	disabled := 0
	for _, target := range targets {
		result, rebuildErr := pubstatus.RebuildProjection(ctx, pubstatus.RebuildOptions{
			Redis:           t.redis,
			IntervalMinutes: target.intervalMinutes,
			RangeHours:      target.rangeHours,
			Retention:       t.retention,
			BootstrapConfig: t.bootstrap,
			Logger:          t.deps.Logger,
		})
		if rebuildErr != nil {
			return OpsOutcome{
				Processed: updated,
				Fields: map[string]any{
					"publicStatusRebuild": "error",
					"intervalMinutes":     target.intervalMinutes,
					"rangeHours":          target.rangeHours,
					"error":               rebuildErr.Error(),
				},
			}, rebuildErr
		}
		switch result.Status {
		case pubstatus.RebuildStatusUpdated:
			updated++
			// 重建成功才消费提示（Node 同判）：失败留着提示，下一轮再试。
			if target.hintKey != "" {
				if delErr := t.redis.Del(ctx, target.hintKey); delErr != nil && t.deps.Logger != nil {
					t.deps.Logger.Warn("public_status_rebuild_hint_delete_failed", map[string]any{
						"hintKey": target.hintKey, "error": delErr.Error(),
					})
				}
			}
		case pubstatus.RebuildStatusSkipped:
			skipped++
		default:
			disabled++
		}
	}

	return OpsOutcome{
		Processed: updated,
		Fields: map[string]any{
			"publicStatusRebuild": "cycled",
			"targets":             len(targets),
			"updated":             updated,
			"skipped":             skipped,
			"disabled":            disabled,
		},
	}, nil
}

// publicStatusRebuildTarget 是一轮里的一个重建目标。
type publicStatusRebuildTarget struct {
	intervalMinutes int
	rangeHours      int
	hintKey         string
}

// collectTargets 复刻 collectTargets（scheduler.ts:65-130）。
//
// 两类目标：
//  1. **配置默认档**：内部配置快照存在分组，且该档的 `manifest:current` 缺失/版本不符/已过
//     `freshUntil` 时纳入——这是「页面被访问时发现有档位过期」的兜底；
//  2. **重建提示**：`<prefix>:rebuild-hint:<interval>m:<range>h`（配置变更或读路径发现不新鲜时
//     由业务侧写下），逐个纳入并在成功后删除。
func (t *PublicStatusRebuild) collectTargets(ctx context.Context) []publicStatusRebuildTarget {
	targets := map[string]publicStatusRebuildTarget{}
	add := func(intervalMinutes, rangeHours int, hintKey string) {
		key := strconv.Itoa(intervalMinutes) + ":" + strconv.Itoa(rangeHours)
		if _, exists := targets[key]; exists && hintKey == "" {
			return
		}
		targets[key] = publicStatusRebuildTarget{
			intervalMinutes: intervalMinutes, rangeHours: rangeHours, hintKey: hintKey,
		}
	}

	// 注意：调度器这一处读的是**允许 legacy 回退**的路径（Node 调用时没传
	// allowLegacyFallback:false），与 worker 内部那处不同（那里显式关闭）。
	snapshot := pubstatus.ReadInternalConfigSnapshotLegacyAware(ctx, t.redis, "")
	if snapshot != nil && len(snapshot.Groups) > 0 {
		intervalMinutes := snapshot.DefaultIntervalMinutes
		rangeHours := snapshot.DefaultRangeHours
		manifestKey, err := pubstatus.BuildManifestKey("current", intervalMinutes, rangeHours, "")
		if err == nil {
			manifestRaw, ok := t.redis.Get(ctx, manifestKey)
			if !ok || t.manifestStale(manifestRaw, snapshot.ConfigVersion) {
				add(intervalMinutes, rangeHours, "")
			}
		}
	}

	hintKeys, err := t.redis.Scan(ctx, pubstatus.PublicStatusRedisPrefix+":rebuild-hint:*", publicStatusRebuildHintLimit)
	if err == nil {
		for _, hintKey := range hintKeys {
			matches := publicStatusRebuildHintPattern.FindStringSubmatch(hintKey)
			if len(matches) != 3 {
				continue
			}
			intervalMinutes, intervalErr := strconv.Atoi(matches[1])
			rangeHours, rangeErr := strconv.Atoi(matches[2])
			if intervalErr != nil || rangeErr != nil {
				continue
			}
			add(intervalMinutes, rangeHours, hintKey)
		}
	}

	out := make([]publicStatusRebuildTarget, 0, len(targets))
	for _, target := range targets {
		out = append(out, target)
	}
	// 顺序固定：map 迭代无序会让同一轮的日志难以比对。
	sortTargets(out)
	return out
}

// manifestStale 复刻 collectTargets 里的过期判据（scheduler.ts:92-101）：
// 版本不符、缺 freshUntil、或已过期都算需要重建。
func (t *PublicStatusRebuild) manifestStale(manifestRaw string, configVersion string) bool {
	var manifest struct {
		FreshUntil    *string `json:"freshUntil"`
		ConfigVersion *string `json:"configVersion"`
	}
	if err := json.Unmarshal([]byte(manifestRaw), &manifest); err != nil {
		// 损坏的 manifest 按「缺失/过期」处理（Node 同判）。
		return true
	}
	if manifest.ConfigVersion == nil || *manifest.ConfigVersion != configVersion {
		return true
	}
	if manifest.FreshUntil == nil || *manifest.FreshUntil == "" {
		return true
	}
	freshUntil, err := pubstatus.ParseISOMilli(*manifest.FreshUntil)
	if err != nil {
		return true
	}
	return !t.deps.now().Before(freshUntil)
}

func sortTargets(targets []publicStatusRebuildTarget) {
	for i := 1; i < len(targets); i++ {
		for j := i; j > 0; j-- {
			less := targets[j].intervalMinutes < targets[j-1].intervalMinutes ||
				(targets[j].intervalMinutes == targets[j-1].intervalMinutes &&
					targets[j].rangeHours < targets[j-1].rangeHours)
			if !less {
				break
			}
			targets[j], targets[j-1] = targets[j-1], targets[j]
		}
	}
}
