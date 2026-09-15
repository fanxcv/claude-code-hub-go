package notify

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 窗口模式取值（与 src/lib/webhook/types.ts 的 CACHE_HIT_RATE_ALERT_SETTINGS_WINDOW_MODES 同集）。
const (
	CacheWindowModeAuto = "auto"
	CacheWindowMode5m   = "5m"
	CacheWindowMode30m  = "30m"
	CacheWindowMode1h   = "1h"
	CacheWindowMode15h  = "1.5h"
)

// CacheSettings 是缓存告警的**生效**参数：null 与越界值都已折成合法取值。
type CacheSettings struct {
	WindowMode             string
	CheckIntervalMinutes   int
	HistoricalLookbackDays int
	MinEligibleRequests    int
	MinEligibleTokens      int
	AbsMin                 float64
	DropRel                float64
	DropAbs                float64
	CooldownMinutes        int
	TopN                   int
}

// ResolveCacheSettings 把设置列折成生效参数。
//
// 纪律与 jobs 侧的 clampNotifyInterval 一致：空值取缺省、越界夹到区间——设置表是用户可写的，
// 一个 0 分钟的冷却或 10 年的回看都不能变成查询或行为上的坑。
func ResolveCacheSettings(settings store.AdminNotificationSettings) CacheSettings {
	mode := CacheWindowModeAuto
	if settings.CacheHitRateAlertWindowMode != nil {
		candidate := strings.TrimSpace(*settings.CacheHitRateAlertWindowMode)
		if isCacheWindowMode(candidate) {
			mode = candidate
		}
	}

	interval := orInt(settings.CacheHitRateAlertCheckInterval, DefaultCacheCheckIntervalMinutes)
	if interval < 1 {
		interval = 1
	}
	lookback := orInt(settings.CacheHitRateAlertHistoricalLookbackDays, DefaultCacheLookbackDays)
	if lookback < 1 {
		lookback = 1
	}
	if lookback > MaxCacheLookbackDays {
		lookback = MaxCacheLookbackDays
	}
	minRequests := orInt(settings.CacheHitRateAlertMinEligibleRequests, DefaultCacheMinEligibleRequests)
	if minRequests < 0 {
		minRequests = 0
	}
	minTokens := orInt(settings.CacheHitRateAlertMinEligibleTokens, DefaultCacheMinEligibleTokens)
	if minTokens < 0 {
		minTokens = 0
	}
	topN := orInt(settings.CacheHitRateAlertTopN, DefaultCacheTopN)
	if topN < 1 {
		topN = 1
	}
	cooldown := orInt(settings.CacheHitRateAlertCooldownMinutes, DefaultCacheCooldownMinutes)
	if cooldown < 0 {
		cooldown = 0
	}

	return CacheSettings{
		WindowMode:             mode,
		CheckIntervalMinutes:   interval,
		HistoricalLookbackDays: lookback,
		MinEligibleRequests:    minRequests,
		MinEligibleTokens:      minTokens,
		AbsMin:                 clampRate01(rateOr(settings.CacheHitRateAlertAbsMin, DefaultCacheAbsMin)),
		DropRel:                clampRate01(rateOr(settings.CacheHitRateAlertDropRel, DefaultCacheDropRel)),
		DropAbs:                clampRate01(rateOr(settings.CacheHitRateAlertDropAbs, DefaultCacheDropAbs)),
		CooldownMinutes:        cooldown,
		TopN:                   topN,
	}
}

// WindowModeFor 定生效窗口模式：显式取值直用，「auto」由检查间隔反推。
//
// 反推口径（重建，见 cache_alert_decision.go 的文件头登记）：检查间隔 <=5 分钟看 5m 窗，
// <=30 分钟看 30m 窗，<=60 分钟看 1h 窗，更长看 1.5h 窗——窗口不该短于检查间隔，
// 否则两次检查之间会漏掉整段。
func WindowModeFor(setting string, intervalMinutes int) string {
	if isCacheWindowMode(setting) && setting != CacheWindowModeAuto {
		return setting
	}
	switch {
	case intervalMinutes <= 5:
		return CacheWindowMode5m
	case intervalMinutes <= 30:
		return CacheWindowMode30m
	case intervalMinutes <= 60:
		return CacheWindowMode1h
	default:
		return CacheWindowMode15h
	}
}

func isCacheWindowMode(value string) bool {
	switch value {
	case CacheWindowModeAuto, CacheWindowMode5m, CacheWindowMode30m, CacheWindowMode1h, CacheWindowMode15h:
		return true
	default:
		return false
	}
}

// windowMinutes 是各窗口模式的长度（分钟）。1.5h 的键名与 UI 取值都写成「1.5h」。
func windowMinutes(mode string) int {
	switch mode {
	case CacheWindowMode5m:
		return 5
	case CacheWindowMode30m:
		return 30
	case CacheWindowMode1h:
		return 60
	case CacheWindowMode15h:
		return 90
	default:
		return 5
	}
}

// CacheHitRateAlert 生成缓存命中率异常告警正文（Node：tasks/cache-hit-rate-alert.ts:209）。
//
// 流程：定窗口 -> 取四个窗口的口径行（当前/上一窗/今日/历史）-> 逐条判定 -> 冷却去重 ->
// 按绝对跌幅排序取前 topN -> 补供应商名与类型。
//
// 返回 (nil, nil) 表示本刻无异常（含「异常全被冷却抑制」）：不发零条告警的正文。
func (g *Generators) CacheHitRateAlert(
	ctx context.Context,
	settings store.AdminNotificationSettings,
	timezone string,
	now time.Time,
) (*CacheHitRateAlertData, error) {
	resolved := ResolveCacheSettings(settings)
	mode := WindowModeFor(resolved.WindowMode, resolved.CheckIntervalMinutes)
	duration := windowMinutes(mode)
	location := Location(timezone)

	end := now
	windowStart := end.Add(-time.Duration(duration) * time.Minute)
	prevStart := windowStart.Add(-time.Duration(duration) * time.Minute)
	zoned := end.In(location)
	todayStart := time.Date(zoned.Year(), zoned.Month(), zoned.Day(), 0, 0, 0, 0, location)
	historicalStart := end.AddDate(0, 0, -resolved.HistoricalLookbackDays)

	billingSource := g.billingModelSource(ctx)

	// 当前窗口读不到就是这一轮失败（重试有意义）；三个基线是降级项（少一个基线只影响
	// 能判出多少条，不该让整轮告警停摆）。
	current, err := g.Cache.NotifyProviderModelCacheMetrics(ctx, windowStart, end, billingSource)
	if err != nil {
		return nil, err
	}
	prev := g.cacheMetricsOrNil(ctx, prevStart, windowStart, billingSource, cacheBaselinePrev)
	today := g.cacheMetricsOrNil(ctx, todayStart, end, billingSource, cacheBaselineToday)
	historical := g.cacheMetricsOrNil(ctx, historicalStart, end, billingSource, cacheBaselineHistorical)

	prevIndex := indexCacheMetrics(prev)
	todayIndex := indexCacheMetrics(today)
	historicalIndex := indexCacheMetrics(historical)

	anomalies := make([]CacheHitRateAlertAnomaly, 0, len(current))
	for _, metric := range current {
		anomaly, ok := decideCacheAnomaly(cacheDecisionInput{
			Current:    metric,
			Historical: lookupCacheMetric(historicalIndex, metric),
			Today:      lookupCacheMetric(todayIndex, metric),
			Prev:       lookupCacheMetric(prevIndex, metric),
			Settings:   resolved,
		})
		if ok {
			anomalies = append(anomalies, anomaly)
		}
	}

	suppressed := g.applyCooldown(ctx, &anomalies, resolved.CooldownMinutes)
	if len(anomalies) == 0 {
		return nil, nil
	}

	// 绝对跌幅大的先看；同跌幅按 providerId、model 定序，让同一份输入产出同一份正文。
	sort.SliceStable(anomalies, func(left, right int) bool {
		leftDrop := dropOrZero(anomalies[left])
		rightDrop := dropOrZero(anomalies[right])
		if leftDrop != rightDrop {
			return leftDrop > rightDrop
		}
		if anomalies[left].ProviderID != anomalies[right].ProviderID {
			return anomalies[left].ProviderID < anomalies[right].ProviderID
		}
		return anomalies[left].Model < anomalies[right].Model
	})
	if len(anomalies) > resolved.TopN {
		anomalies = anomalies[:resolved.TopN]
	}
	g.enrichProviderNames(ctx, anomalies)

	return &CacheHitRateAlertData{
		Window: CacheHitRateAlertWindow{
			Mode:            mode,
			StartTime:       isoMillis(windowStart),
			EndTime:         isoMillis(end),
			DurationMinutes: duration,
		},
		Anomalies:       anomalies,
		SuppressedCount: suppressed,
		Settings: CacheHitRateAlertSettingsSnapshot{
			WindowMode:             mode,
			CheckIntervalMinutes:   resolved.CheckIntervalMinutes,
			HistoricalLookbackDays: resolved.HistoricalLookbackDays,
			MinEligibleRequests:    resolved.MinEligibleRequests,
			MinEligibleTokens:      resolved.MinEligibleTokens,
			AbsMin:                 resolved.AbsMin,
			DropRel:                resolved.DropRel,
			DropAbs:                resolved.DropAbs,
			CooldownMinutes:        resolved.CooldownMinutes,
			TopN:                   resolved.TopN,
		},
		GeneratedAt: isoMillis(end),
	}, nil
}

// cacheMetricsOrNil 读一个基线窗口；失败只记 warn 并返回 nil（该基线缺席）。
func (g *Generators) cacheMetricsOrNil(
	ctx context.Context,
	start time.Time,
	end time.Time,
	billingSource string,
	source string,
) []store.NotifyCacheMetric {
	if !end.After(start) {
		return nil
	}
	metrics, err := g.Cache.NotifyProviderModelCacheMetrics(ctx, start, end, billingSource)
	if err != nil {
		g.logger().Warn("notify.cache_alert_baseline_failed", map[string]any{
			"baseline": source,
			"error":    err.Error(),
		})
		return nil
	}
	return metrics
}

// billingModelSource 读 system_settings 的计费模型口径；读不到就按缺省（redirected）走。
func (g *Generators) billingModelSource(ctx context.Context) string {
	settings, err := g.Cache.FindSystemSettings(ctx)
	if err != nil {
		g.logger().Warn("notify.cache_alert_billing_source_failed", map[string]any{"error": err.Error()})
		return ""
	}
	if settings == nil {
		return ""
	}
	return settings.BillingModelSource
}

// cacheMetricKey 是「供应商 × 模型」在四个窗口之间对齐用的键。
func cacheMetricKey(providerID int64, model string) string {
	return strconv.FormatInt(providerID, 10) + "\x00" + model
}

func indexCacheMetrics(metrics []store.NotifyCacheMetric) map[string]store.NotifyCacheMetric {
	if len(metrics) == 0 {
		return nil
	}
	index := make(map[string]store.NotifyCacheMetric, len(metrics))
	for _, metric := range metrics {
		index[cacheMetricKey(metric.ProviderID, metric.Model)] = metric
	}
	return index
}

func lookupCacheMetric(index map[string]store.NotifyCacheMetric, metric store.NotifyCacheMetric) *store.NotifyCacheMetric {
	if index == nil {
		return nil
	}
	found, ok := index[cacheMetricKey(metric.ProviderID, metric.Model)]
	if !ok {
		return nil
	}
	return &found
}

// enrichProviderNames 补 providerName / providerType（模板与排障都靠它认渠道）。
//
// 读供应商列表失败只记 warn：告警正文里少一个名字，不该让整轮告警不发。
func (g *Generators) enrichProviderNames(ctx context.Context, anomalies []CacheHitRateAlertAnomaly) {
	refs, err := g.Cache.NotifyProviderRefs(ctx)
	if err != nil {
		g.logger().Warn("notify.cache_alert_provider_refs_failed", map[string]any{"error": err.Error()})
		return
	}
	byID := make(map[int64]store.NotifyProviderRef, len(refs))
	for _, ref := range refs {
		byID[ref.ID] = ref
	}
	for index := range anomalies {
		ref, ok := byID[anomalies[index].ProviderID]
		if !ok {
			continue
		}
		anomalies[index].ProviderName = ref.Name
		if anomalies[index].ProviderType == "" {
			anomalies[index].ProviderType = ref.ProviderType
		}
	}
}

// applyCooldown 对已判出的异常做去重：冷却期内的丢弃并计数。
//
// 返回被抑制的条数。未装配 Cooldown 或冷却为 0 时不去重（suppressedCount 恒 0）——
// 与 Node 的「Redis 不可用就照发」分支一致。
func (g *Generators) applyCooldown(
	ctx context.Context,
	anomalies *[]CacheHitRateAlertAnomaly,
	cooldownMinutes int,
) int {
	list := *anomalies
	if g.Cooldown == nil || cooldownMinutes <= 0 || len(list) == 0 {
		return 0
	}
	keys := make([]string, len(list))
	for index, anomaly := range list {
		keys[index] = CooldownKey(anomaly.ProviderID, anomaly.Model)
	}
	claimed, err := g.Cooldown.Claim(ctx, keys, time.Duration(cooldownMinutes)*time.Minute)
	if err != nil {
		g.logger().Warn("notify.cache_alert_cooldown_failed", map[string]any{"error": err.Error()})
		return 0
	}
	if len(claimed) != len(list) {
		g.logger().Warn("notify.cache_alert_cooldown_short", map[string]any{
			"keys":    len(keys),
			"claimed": len(claimed),
		})
		return 0
	}
	kept := make([]CacheHitRateAlertAnomaly, 0, len(list))
	suppressed := 0
	for index, anomaly := range list {
		if claimed[index] {
			kept = append(kept, anomaly)
			continue
		}
		suppressed++
	}
	*anomalies = kept
	return suppressed
}

// dropOrZero 取绝对跌幅（指针字段可空，缺失按 0 参与排序）。
func dropOrZero(anomaly CacheHitRateAlertAnomaly) float64 {
	if anomaly.DropAbs == nil {
		return 0
	}
	return *anomaly.DropAbs
}

// clampRate01 把比例夹到 [0,1]；NaN 归 0。
func clampRate01(value float64) float64 {
	switch {
	case value != value:
		return 0
	case value < 0:
		return 0
	case value > 1:
		return 1
	default:
		return value
	}
}
