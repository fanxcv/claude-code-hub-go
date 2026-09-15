package limit

import (
	"context"
	"sync"
	"time"
)

// 本文件是租约族的**设置面**：Node 在每次租约刷新时读 `getCachedSystemSettings()` 取
// 六个配额租约配置（lease-service.ts:281-292），Go 侧对应本文件。
//
// 为什么单列一个读取接口而不是塞进 Config：设置是**可变的运行时事实**（管理员在
// /settings/config 改后应生效，不必重启），而 Config 是进程启动期的一次性装配。把可变
// 值钉进 Config 会得到「改了不生效」的假开关——本仓已经在 system_settings 上吃过这个亏。

// QuotaLeaseSettings 是租约判定用到的六个设置，字段名下划线取值对应 system_settings 列。
type QuotaLeaseSettings struct {
	// RefreshIntervalSeconds 对应 quota_db_refresh_interval_seconds：租约在 Redis 里的 TTL，
	// 也就是「多久允许拿一次 DB 权威用量」。Node 默认 10。
	RefreshIntervalSeconds int
	// Percent* 对应 quota_lease_percent_*：每次刷新发放的预算切片占限额的比例，Node 默认 0.05。
	Percent5h      float64
	PercentDaily   float64
	PercentWeekly  float64
	PercentMonthly float64
	// CapUSD 对应 quota_lease_cap_usd：切片上限（USD）。nil 表示不设上限。
	CapUSD *float64
}

// QuotaLeaseSettingsReader 读一次租约设置。
type QuotaLeaseSettingsReader interface {
	QuotaLeaseSettings(ctx context.Context) (QuotaLeaseSettings, error)
}

// DefaultQuotaLeaseRefreshSeconds / DefaultQuotaLeasePercent 是 Node 侧的缺省值
// （drizzle/schema.ts:1034-1039）。零值（含读回来的 0）一律按缺省补齐：百分比为 0 会让
// 每份切片恒为 0，等于把全部限额判成「已用尽」，那是最坏的一种「配置缺失」表现。
const (
	DefaultQuotaLeaseRefreshSeconds = 10
	DefaultQuotaLeasePercent        = 0.05
)

// Normalize 把缺失/非法值补成 Node 缺省值。
func (s QuotaLeaseSettings) Normalize() QuotaLeaseSettings {
	out := s
	if out.RefreshIntervalSeconds <= 0 {
		out.RefreshIntervalSeconds = DefaultQuotaLeaseRefreshSeconds
	}
	if !(out.Percent5h > 0) {
		out.Percent5h = DefaultQuotaLeasePercent
	}
	if !(out.PercentDaily > 0) {
		out.PercentDaily = DefaultQuotaLeasePercent
	}
	if !(out.PercentWeekly > 0) {
		out.PercentWeekly = DefaultQuotaLeasePercent
	}
	if !(out.PercentMonthly > 0) {
		out.PercentMonthly = DefaultQuotaLeasePercent
	}
	return out
}

// PercentFor 取某窗口的切片比例，未知窗口按缺省（对应 Node `getLeasePercent` 的 default 分支）。
func (s QuotaLeaseSettings) PercentFor(window LeaseWindow) float64 {
	switch window {
	case LeaseWindow5h:
		return s.Percent5h
	case LeaseWindowDaily:
		return s.PercentDaily
	case LeaseWindowWeekly:
		return s.PercentWeekly
	case LeaseWindowMonthly:
		return s.PercentMonthly
	default:
		return DefaultQuotaLeasePercent
	}
}

// CachedQuotaLeaseSettings 给读取器加一层 TTL 缓存。
//
// 为什么需要：租约刷新发生在每个实体×窗口上（一次请求最多 8 个维度），若每次都回表读
// system_settings，就把「用租约省 DB 压力」的初衷反过来了。Node 侧同样是缓存读
// （getCachedSystemSettings 带失效订阅）；Go 这里没有 settings 失效订阅可用，
// 故用短 TTL 自愈——TTL 取刷新间隔量级即可，改配置最迟一个 TTL 后生效。
type CachedQuotaLeaseSettings struct {
	reader QuotaLeaseSettingsReader
	ttl    time.Duration
	now    func() time.Time

	mu      sync.Mutex
	value   QuotaLeaseSettings
	loaded  bool
	expires time.Time
}

// NewCachedQuotaLeaseSettings 构造带缓存的读取器；ttl <= 0 时取 30s。
func NewCachedQuotaLeaseSettings(reader QuotaLeaseSettingsReader, ttl time.Duration) *CachedQuotaLeaseSettings {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &CachedQuotaLeaseSettings{reader: reader, ttl: ttl, now: time.Now}
}

// QuotaLeaseSettings 实现 QuotaLeaseSettingsReader。
//
// 读失败时**不缓存失败**：把错误交给调用方（租约刷新据此 fail-open），下一轮还会重试。
func (c *CachedQuotaLeaseSettings) QuotaLeaseSettings(ctx context.Context) (QuotaLeaseSettings, error) {
	if c == nil || c.reader == nil {
		return QuotaLeaseSettings{}.Normalize(), nil
	}
	now := c.now()
	c.mu.Lock()
	if c.loaded && now.Before(c.expires) {
		value := c.value
		c.mu.Unlock()
		return value, nil
	}
	c.mu.Unlock()

	value, err := c.reader.QuotaLeaseSettings(ctx)
	if err != nil {
		return QuotaLeaseSettings{}, err
	}
	value = value.Normalize()

	c.mu.Lock()
	c.value = value
	c.loaded = true
	c.expires = now.Add(c.ttl)
	c.mu.Unlock()
	return value, nil
}
