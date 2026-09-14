package adminapi

import (
	"context"
	"fmt"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pubstatus"
)

// 本文件实现「随时区变化而失效」的三族缓存清理。
//
// 唯一真源：Node 的 invalidateAllOverviewCaches / invalidateAllStatisticsCaches /
// invalidateAllLeaderboardCaches（src/lib/redis/{overview,statistics,leaderboard}-cache.ts）。
// 三个函数做的是同一件事——按前缀 SCAN 后 DEL。缓存键里嵌着时区（`:tz:<zone>`），故改时区必须
// 全清，否则仪表盘会继续按旧时区切分当天的数据。
//
// 用 SCAN 而不是 KEYS：这三族在生产里是上万键量级，KEYS 会阻塞整个 Redis 实例。
type DashboardCacheInvalidator interface {
	// InvalidateDashboardCaches 清 overview/statistics/leaderboard 三族缓存。
	// 返回错误仅供调用方记 warn——Node 侧同样只记日志，不影响请求结果。
	InvalidateDashboardCaches(ctx context.Context) error
}

// dashboardCachePatterns 是三个前缀，照 Node 的 scanPattern 参数。
var dashboardCachePatterns = []struct {
	pattern string
	label   string
}{
	{"overview:*", "overview"},
	{"statistics:*", "statistics"},
	{"leaderboard:*", "leaderboard"},
}

// InvalidateDashboardCaches 见 DashboardCacheInvalidator。
func (i *CacheInvalidator) InvalidateDashboardCaches(ctx context.Context) error {
	if i == nil || i.client == nil {
		return nil
	}

	for _, family := range dashboardCachePatterns {
		iterator := i.client.Scan(ctx, 0, family.pattern, costCacheScanBatch).Iterator()
		deleted := 0
		for iterator.Next(ctx) {
			if err := i.client.Del(ctx, iterator.Val()).Err(); err != nil {
				return fmt.Errorf("管理面: 清 %s 缓存失败: %w", family.label, err)
			}
			deleted++
		}
		if err := iterator.Err(); err != nil {
			return fmt.Errorf("管理面: 扫描 %s 缓存失败: %w", family.label, err)
		}
		i.info("admin_dashboard_cache_invalidated", map[string]any{
			"family":       family.label,
			"deletedCount": deleted,
		})
	}
	return nil
}

// 编译期断言：默认失效器同时是仪表盘三族缓存的清理器（装配处直接把 Invalidator 塞进
// Deps.DashboardCaches，不必再造一个实现）。
var _ DashboardCacheInvalidator = (*CacheInvalidator)(nil)

// composePublicStatusRepublish 把「settings PUT 之后的 public-status 重发」的两种结果
// 归成 Node 的两个警告码（actions/system-config.ts:243-277）。
//
//   - 未装配发布器（无 Redis）或发布器报错 → PUBLIC_STATUS_PROJECTION_PUBLISH_FAILED
//   - 发布成功但 background rebuild 未调度 → PUBLIC_STATUS_BACKGROUND_REFRESH_PENDING
//
// 第二条当前**恒为待办**：聚合侧（rebuild-hint/scheduler/rebuild-worker）未移植，故发布成功
// 也如实回 PENDING，让运维知道公开状态页的数据不会自己刷新。
func composePublicStatusRepublish(result pubstatus.PublishResult, err error) *string {
	failed := "PUBLIC_STATUS_PROJECTION_PUBLISH_FAILED"
	pending := "PUBLIC_STATUS_BACKGROUND_REFRESH_PENDING"
	if err != nil || !result.Written {
		return &failed
	}
	return &pending
}
