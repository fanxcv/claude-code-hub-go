package notify

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是四种数据生成器共用的取数接口、缺省值与解析口径。
//
// 取数接口刻意拆成三个窄接口（而不是直接吃 *store.Pools）：生成器的窗口切分与判定是本包重点，
// 真库集成用例未注入 CCH_TEST_DSN 时整组跳过，所以判定必须能被假实现驱动。

// LeaderboardQuerier 是日报的取数面。
type LeaderboardQuerier interface {
	AdminUserLeaderboard(ctx context.Context, query store.AdminLeaderboardQuery) ([]store.AdminLeaderboardUserRow, error)
}

// CostQuerier 是成本预警的取数面。
type CostQuerier interface {
	NotifyKeysWithCostLimits(ctx context.Context) ([]store.NotifyKeyCostLimit, error)
	NotifyProvidersWithCostLimits(ctx context.Context) ([]store.NotifyProviderCostLimit, error)
	SumLedgerCostInTimeRange(
		ctx context.Context,
		entityType store.LedgerEntityType,
		entityID any,
		start time.Time,
		end time.Time,
	) (string, error)
}

// CacheQuerier 是缓存命中率告警的取数面。
type CacheQuerier interface {
	NotifyProviderModelCacheMetrics(
		ctx context.Context,
		start time.Time,
		end time.Time,
		billingModelSource string,
	) ([]store.NotifyCacheMetric, error)
	NotifyProviderRefs(ctx context.Context) ([]store.NotifyProviderRef, error)
	FindSystemSettings(ctx context.Context) (*store.SystemSettings, error)
}

// 设置缺省值，来自 Node 的调用点：
//   - 日报条数：notification-queue.ts:430 的 `settings.dailyLeaderboardTopN || 5`
//   - 成本阈值：notification-queue.ts:452-454 的 `settings.costAlertThreshold || "0.80"`
//   - 缓存告警：tasks/cache-hit-rate-alert.ts:221-236
const (
	// DefaultLeaderboardTopN 是日报条数缺省。
	DefaultLeaderboardTopN = 5
	// DefaultCostAlertThreshold 是「已花 / 限额」的告警比例缺省（80%）。
	DefaultCostAlertThreshold = 0.8

	DefaultCacheAbsMin               = 0.05
	DefaultCacheDropRel              = 0.3
	DefaultCacheDropAbs              = 0.1
	DefaultCacheMinEligibleRequests  = 20
	DefaultCacheMinEligibleTokens    = 0
	DefaultCacheTopN                 = 10
	DefaultCacheLookbackDays         = 7
	DefaultCacheCheckIntervalMinutes = 5
	DefaultCacheCooldownMinutes      = 30
)

// Generators 是四个数据生成器的合体。
//
// 字段直接暴露（而不是一个构造选项结构体）：装配点只有一处（adminapi 的投递层），
// 测试也直接填假实现。
type Generators struct {
	Leaderboard LeaderboardQuerier
	Cost        CostQuerier
	Cache       CacheQuerier
	Logger      *logx.Logger
	// Cooldown 是缓存告警的冷却去重存储；nil 时不去重（对应 Node 的 getRedisClient() 回 null）。
	Cooldown Cooldown
}

func (g *Generators) logger() *logx.Logger {
	if g == nil || g.Logger == nil {
		return logx.New(nil)
	}
	return g.Logger
}

// Location 把 IANA 时区名解成 *time.Location；解不开时退 UTC。
//
// 与 Node 的 resolveSystemTimezone 降级链方向一致：宁可算错一天的边界，也不能因为一个坏时区名
// 让整个后台任务报错。
func Location(name string) *time.Location {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return time.UTC
	}
	location, err := time.LoadLocation(trimmed)
	if err != nil {
		return time.UTC
	}
	return location
}

// isoMillis 复刻 JS 的 Date#toISOString：UTC、毫秒精度、末尾 Z。
//
// 不用 time.RFC3339Nano：它省略末尾的 0（`...:05.5Z`），而模板与前端都按 JS 的形状解析。
func isoMillis(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// parseCostText 解析账本/限额列取回的 numeric 文本（Node 侧是 parseFloat / Number）。
//
// 解析失败返回 0 并记 warn：单列坏数据不该让整轮告警消失，但也不能静默。
func (g *Generators) parseCostText(event, text string) float64 {
	value, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil {
		g.logger().Warn(event, map[string]any{"value": text, "error": err.Error()})
		return 0
	}
	return value
}

// LeaderboardTopN 复刻 notification-queue.ts:430 的 `settings.dailyLeaderboardTopN || 5`：
// JS 的 `||` 把 null 与 0 一律折成缺省。
func LeaderboardTopN(settings store.AdminNotificationSettings) int {
	if settings.DailyLeaderboardTopN == nil || *settings.DailyLeaderboardTopN == 0 {
		return DefaultLeaderboardTopN
	}
	return *settings.DailyLeaderboardTopN
}

// CostAlertThreshold 复刻 notification-queue.ts:452 的 `settings.costAlertThreshold || "0.80"`。
func CostAlertThreshold(settings store.AdminNotificationSettings) float64 {
	if settings.CostAlertThreshold == nil {
		return DefaultCostAlertThreshold
	}
	parsed := parseFloatOr(settings.CostAlertThreshold, DefaultCostAlertThreshold)
	if parsed <= 0 {
		return DefaultCostAlertThreshold
	}
	return parsed
}

// parseIntOr 复刻 parseIntNumber：null/undefined 取缺省，其余取整数值（源文件 39-42 行）。
func parseIntOr(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

// parseFloatOr 复刻 parseNumber（源文件 33-37 行）：null/undefined 取缺省；
// 空串按 JS 的 Number("") 折成 0；非法值取缺省。
func parseFloatOr(value *string, fallback float64) float64 {
	if value == nil {
		return fallback
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return 0
	}
	parsed, err := strconv.ParseFloat(trimmed, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return fallback
	}
	return parsed
}
