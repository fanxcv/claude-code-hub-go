package notify

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是三种数据生成器共用的取数接口、默认值与解析口径。
//
// 取数接口刻意拆成三个窄接口（而不是直接吃 *store.Pools）：生成器的判定逻辑（窗口切分、
// 阈值比较、排序）是本包的重点，真库集成用例未注入 CCH_TEST_DSN 时整组跳过，所以判定必须
// 能被假实现驱动。

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

// 设置缺省值。Node 侧的 resolveNotificationSettings 把 null 一律折成这些数
// （设置列可空，UI 允许留空）。
const (
	// DefaultLeaderboardTopN 是日报条数缺省。
	DefaultLeaderboardTopN = 10
	// DefaultCostAlertThreshold 是「已花 / 限额」的告警比例缺省（80%）。
	DefaultCostAlertThreshold = 0.8

	// DefaultCacheAbsMin 是命中率的绝对下限：低于它的窗口才可能是异常。
	DefaultCacheAbsMin = 0.05
	// DefaultCacheDropRel 是相对跌幅阈值（相对基线）。
	DefaultCacheDropRel = 0.3
	// DefaultCacheDropAbs 是绝对跌幅阈值（命中率绝对差）。
	DefaultCacheDropAbs = 0.1
	// DefaultCacheMinEligibleRequests 是「可命中请求」的最小样本量。
	DefaultCacheMinEligibleRequests = 20
	// DefaultCacheMinEligibleTokens 是最小可命中 token 量。
	DefaultCacheMinEligibleTokens = 0
	// DefaultCacheTopN 是告警条数缺省。
	DefaultCacheTopN = 10
	// DefaultCacheLookbackDays 是历史基线回看天数缺省。
	DefaultCacheLookbackDays = 7
	// DefaultCacheCheckIntervalMinutes 是检查间隔缺省（分钟）。
	DefaultCacheCheckIntervalMinutes = 5
	// DefaultCacheCooldownMinutes 是同一「供应商×模型」的去重冷却缺省（分钟）。
	DefaultCacheCooldownMinutes = 30

	// MaxCacheLookbackDays 是回看天数上限（防止一次查询扫全表）。
	MaxCacheLookbackDays = 90
)

// Generators 是三个数据生成器的合体。
//
// 字段直接暴露（而不是一个构造选项结构体）：装配点只有一处（adminapi 的投递层），
// 测试也直接填假实现。
type Generators struct {
	Leaderboard LeaderboardQuerier
	Cost        CostQuerier
	Cache       CacheQuerier
	Logger      *logx.Logger
	// Cooldown 是缓存告警的去重存储；nil 时不去重（与 Node 的「Redis 不可用」分支一致）。
	Cooldown Cooldown
	// Now 可注入；nil 时用真实时钟。
	Now func() time.Time
}

func (g *Generators) logger() *logx.Logger {
	if g == nil || g.Logger == nil {
		return logx.New(nil)
	}
	return g.Logger
}

func (g *Generators) now() time.Time {
	if g == nil || g.Now == nil {
		return time.Now()
	}
	return g.Now()
}

// Location 把 IANA 时区名解成 *time.Location；解不开时退 UTC 并记一条 warn。
//
// 与 Node 的 resolveSystemTimezone 降级链方向一致：宁可算错一天的边界，也不能因为
// 一个坏时区名让整个后台任务报错。
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
// 为什么不用 time.RFC3339Nano：它省略末尾的 0（`...:05.5Z`），而模板与前端都按 JS 的形状解析。
func isoMillis(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// parseCostText 解析账本/限额列取回的 numeric 文本（Node 侧是 parseFloat）。
//
// 解析失败返回 0 并记 warn：单列坏数据不该让整轮告警消失，但也不能静默——所以留日志。
func (g *Generators) parseCostText(event, text string) float64 {
	value, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil {
		g.logger().Warn(event, map[string]any{"value": text, "error": err.Error()})
		return 0
	}
	return value
}

// orInt 复刻 Node 的 `settings.x ?? fallback`。
func orInt(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

// rateOr 复刻 Node 的 `parseFloat(settings.x ?? fallback)`：空串与非法值都折成 fallback。
func rateOr(value *string, fallback float64) float64 {
	if value == nil {
		return fallback
	}
	parsed, err := strconv.ParseFloat(strings.TrimSpace(*value), 64)
	if err != nil {
		return fallback
	}
	return parsed
}

// intOr 复刻 `settings.x ?? fallback` 的整数字段版本（限额列是 numeric 文本，转 int 用）。
func intOr(value *string, fallback int) int {
	if value == nil {
		return fallback
	}
	parsed, err := strconv.ParseFloat(strings.TrimSpace(*value), 64)
	if err != nil {
		return fallback
	}
	return int(parsed)
}
