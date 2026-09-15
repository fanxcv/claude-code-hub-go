package notify

import (
	"context"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件复刻 `src/lib/notification/tasks/cache-hit-rate-alert.ts`（窗口切分、冷却去重、正文拼装）。
// 判定本身在 cache_alert_decision.go（对应 src/lib/cache-hit-rate-alert/decision.ts）。

// 窗口模式取值（与 src/lib/webhook/types.ts 的 CACHE_HIT_RATE_ALERT_SETTINGS_WINDOW_MODES 同集）。
const (
	CacheWindowModeAuto = "auto"
	CacheWindowMode5m   = "5m"
	CacheWindowMode30m  = "30m"
	CacheWindowMode1h   = "1h"
	CacheWindowMode15h  = "1.5h"

	// maxCacheLookbackDays 复刻源文件的 MAX_LOOKBACK_DAYS（222 行）。
	maxCacheLookbackDays = 90
)

// CacheSettings 是缓存告警的生效参数（源文件 221-241 行）。
//
// 注意两处**不是**解析结果而是原文/原始值：
//   - WindowModeSetting 是设置里的原文（常是 "auto"），正文快照报的就是它（379 行）；
//   - HistoricalLookbackDays 只有上界（min(x,90)），不设下界。
type CacheSettings struct {
	WindowModeSetting      string
	ResolvedWindowMode     string
	DurationMinutes        int
	CheckIntervalMinutes   int
	HistoricalLookbackDays int
	CooldownMinutes        int
	Decision               CacheHitRateAlertDecisionSettings
}

// ResolveCacheSettings 复刻源文件 221-241 行。
func ResolveCacheSettings(settings store.AdminNotificationSettings) CacheSettings {
	intervalMinutes := parseIntOr(settings.CacheHitRateAlertCheckInterval, DefaultCacheCheckIntervalMinutes)
	if intervalMinutes < 1 {
		intervalMinutes = 1
	}
	lookbackDays := parseIntOr(settings.CacheHitRateAlertHistoricalLookbackDays, DefaultCacheLookbackDays)
	if lookbackDays > maxCacheLookbackDays {
		lookbackDays = maxCacheLookbackDays
	}
	cooldownMinutes := parseIntOr(settings.CacheHitRateAlertCooldownMinutes, DefaultCacheCooldownMinutes)

	decision := CacheHitRateAlertDecisionSettings{
		AbsMin:              parseFloatOr(settings.CacheHitRateAlertAbsMin, DefaultCacheAbsMin),
		DropRel:             parseFloatOr(settings.CacheHitRateAlertDropRel, DefaultCacheDropRel),
		DropAbs:             parseFloatOr(settings.CacheHitRateAlertDropAbs, DefaultCacheDropAbs),
		MinEligibleRequests: float64(parseIntOr(settings.CacheHitRateAlertMinEligibleRequests, DefaultCacheMinEligibleRequests)),
		MinEligibleTokens:   float64(parseIntOr(settings.CacheHitRateAlertMinEligibleTokens, DefaultCacheMinEligibleTokens)),
		TopN:                parseIntOr(settings.CacheHitRateAlertTopN, DefaultCacheTopN),
	}

	windowModeSetting := CacheWindowModeAuto
	if settings.CacheHitRateAlertWindowMode != nil {
		windowModeSetting = *settings.CacheHitRateAlertWindowMode
	}
	resolvedMode, durationMinutes := resolveCacheWindowMode(windowModeSetting, intervalMinutes)

	return CacheSettings{
		WindowModeSetting:      windowModeSetting,
		ResolvedWindowMode:     resolvedMode,
		DurationMinutes:        durationMinutes,
		CheckIntervalMinutes:   intervalMinutes,
		HistoricalLookbackDays: lookbackDays,
		CooldownMinutes:        cooldownMinutes,
		Decision:               decision,
	}
}

// resolveCacheWindowMode 复刻源文件 44-67 行：显式模式直用，其余（含 "auto" 与未知值）由检查间隔反推。
func resolveCacheWindowMode(mode string, intervalMinutes int) (string, int) {
	switch mode {
	case CacheWindowMode5m:
		return CacheWindowMode5m, 5
	case CacheWindowMode30m:
		return CacheWindowMode30m, 30
	case CacheWindowMode1h:
		return CacheWindowMode1h, 60
	case CacheWindowMode15h:
		return CacheWindowMode15h, 90
	}
	switch {
	case intervalMinutes <= 5:
		return CacheWindowMode5m, 5
	case intervalMinutes <= 30:
		return CacheWindowMode30m, 30
	case intervalMinutes <= 60:
		return CacheWindowMode1h, 60
	default:
		return CacheWindowMode15h, 90
	}
}

// CacheHitRateAlertResult 复刻源文件的 CacheHitRateAlertTaskResult：正文 +
// **投递成功后**要写下的冷却键 + 冷却分钟数（Node 由 notification-queue 在发送成功后提交）。
type CacheHitRateAlertResult struct {
	Payload         *CacheHitRateAlertData
	CooldownKeys    []string
	CooldownMinutes int
}

// CacheHitRateAlert 生成缓存命中率告警（源文件 209-434 行）。
//
// bindingID 为 0 表示 legacy 单 URL 模式（冷却键不含 binding 段，59-87 行）；非 0 时键里带
// binding 段——targets 模式下每个绑定各自去重，避免「一个目标发成功后把冷却写死」让别的目标永久漏发。
//
// 返回 (nil, nil) 表示本刻不发：无异常、异常全被冷却抑制（357-366 行）。
func (g *Generators) CacheHitRateAlert(
	ctx context.Context,
	settings store.AdminNotificationSettings,
	systemTimezone string,
	now time.Time,
	bindingID int64,
) (*CacheHitRateAlertResult, error) {
	resolved := ResolveCacheSettings(settings)
	location := Location(systemTimezone)
	window := time.Duration(resolved.DurationMinutes) * time.Minute

	currentStart := now.Add(-window)
	prevStart := now.Add(-2 * window)
	prevEnd := currentStart

	zoned := now.In(location)
	todayStart := time.Date(zoned.Year(), zoned.Month(), zoned.Day(), 0, 0, 0, 0, location)
	historicalStart := todayStart.AddDate(0, 0, -resolved.HistoricalLookbackDays)
	historicalEnd := todayStart
	// today 窗口只用于「当日累计」基线：currentStart 在午夜附近可能早于 todayStart，故 clamp 一次。
	todayEnd := todayStart
	if currentStart.After(todayStart) {
		todayEnd = currentStart
	}

	billingSource := g.billingModelSource(ctx)

	// 四个窗口一次取齐：Node 用 Promise.all，任一失败整轮失败（不降级），这里同样直接返回错误。
	currentRows, err := g.Cache.NotifyProviderModelCacheMetrics(ctx, currentStart, now, billingSource)
	if err != nil {
		return nil, err
	}
	prevRows, err := g.Cache.NotifyProviderModelCacheMetrics(ctx, prevStart, prevEnd, billingSource)
	if err != nil {
		return nil, err
	}
	var todayRows, historicalRows []store.NotifyCacheMetric
	if todayStart.Before(todayEnd) {
		todayRows, err = g.Cache.NotifyProviderModelCacheMetrics(ctx, todayStart, todayEnd, billingSource)
		if err != nil {
			return nil, err
		}
	}
	if historicalStart.Before(historicalEnd) {
		historicalRows, err = g.Cache.NotifyProviderModelCacheMetrics(ctx, historicalStart, historicalEnd, billingSource)
		if err != nil {
			return nil, err
		}
	}

	anomalies := decideCacheHitRateAnomalies(cacheDecisionInput{
		Current:    toCacheDecisionMetrics(currentRows),
		Prev:       toCacheDecisionMetrics(prevRows),
		Today:      toCacheDecisionMetrics(todayRows),
		Historical: toCacheDecisionMetrics(historicalRows),
		Settings:   resolved.Decision,
	})
	if len(anomalies) == 0 {
		return nil, nil
	}

	cooldownKeys := make([]string, len(anomalies))
	for index, anomaly := range anomalies {
		cooldownKeys[index] = BuildCacheHitRateAlertCooldownKey(cooldownKeyParams{
			ProviderID: anomaly.ProviderID,
			Model:      anomaly.Model,
			WindowMode: resolved.ResolvedWindowMode,
			BindingID:  bindingID,
		})
	}

	suppressedCount := 0
	remaining := anomalies
	remainingKeys := cooldownKeys
	if g.Cooldown != nil && resolved.CooldownMinutes > 0 {
		present, err := g.Cooldown.Present(ctx, cooldownKeys)
		if err != nil {
			// 读冷却失败：照发，且把全部键都当作「待写」——与源文件 147-162 行一致。
			g.logger().Warn("notify.cache_alert_dedup_read_failed", map[string]any{
				"keysCount":  len(cooldownKeys),
				"windowMode": resolved.ResolvedWindowMode,
				"error":      err.Error(),
			})
		} else {
			remaining = make([]CacheHitRateAlertAnomaly, 0, len(anomalies))
			remainingKeys = make([]string, 0, len(anomalies))
			for index, value := range present {
				if value {
					suppressedCount++
					continue
				}
				remaining = append(remaining, anomalies[index])
				remainingKeys = append(remainingKeys, cooldownKeys[index])
			}
		}
	}

	if len(remaining) == 0 {
		g.logger().Info("notify.cache_alert_all_suppressed", map[string]any{
			"suppressedCount": suppressedCount,
			"windowMode":      resolved.ResolvedWindowMode,
			"cooldownMinutes": resolved.CooldownMinutes,
		})
		return nil, nil
	}

	refs, err := g.Cache.NotifyProviderRefs(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]store.NotifyProviderRef, len(refs))
	for _, ref := range refs {
		byID[ref.ID] = ref
	}

	payloadAnomalies := make([]CacheHitRateAlertAnomaly, 0, len(remaining))
	for _, anomaly := range remaining {
		item := anomaly
		if ref, ok := byID[item.ProviderID]; ok {
			item.ProviderName = ref.Name
			item.ProviderType = ref.ProviderType
		}
		payloadAnomalies = append(payloadAnomalies, item)
	}

	payload := &CacheHitRateAlertData{
		Window: CacheHitRateAlertWindow{
			Mode:            resolved.ResolvedWindowMode,
			StartTime:       isoMillis(currentStart),
			EndTime:         isoMillis(now),
			DurationMinutes: resolved.DurationMinutes,
		},
		Anomalies:       payloadAnomalies,
		SuppressedCount: suppressedCount,
		Settings: CacheHitRateAlertSettingsSnapshot{
			WindowMode:             resolved.WindowModeSetting,
			CheckIntervalMinutes:   resolved.CheckIntervalMinutes,
			HistoricalLookbackDays: resolved.HistoricalLookbackDays,
			MinEligibleRequests:    int(resolved.Decision.MinEligibleRequests),
			MinEligibleTokens:      int(resolved.Decision.MinEligibleTokens),
			AbsMin:                 resolved.Decision.AbsMin,
			DropRel:                resolved.Decision.DropRel,
			DropAbs:                resolved.Decision.DropAbs,
			CooldownMinutes:        resolved.CooldownMinutes,
			TopN:                   resolved.Decision.TopN,
		},
		GeneratedAt: isoMillis(now),
	}

	return &CacheHitRateAlertResult{
		Payload:         payload,
		CooldownKeys:    remainingKeys,
		CooldownMinutes: resolved.CooldownMinutes,
	}, nil
}

// billingModelSource 读 system_settings 的计费模型口径（源文件 128-134 行经 repository 读取）。
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

// toCacheDecisionMetrics 复刻 toDecisionMetric：只把判定用得到的列搬过去。
func toCacheDecisionMetrics(rows []store.NotifyCacheMetric) []CacheHitRateAlertMetric {
	if len(rows) == 0 {
		return nil
	}
	metrics := make([]CacheHitRateAlertMetric, 0, len(rows))
	for _, row := range rows {
		metrics = append(metrics, CacheHitRateAlertMetric{
			ProviderID:                row.ProviderID,
			Model:                     row.Model,
			TotalRequests:             row.TotalRequests,
			DenominatorTokens:         row.DenominatorTokens,
			HitRateTokens:             row.HitRateTokens,
			EligibleRequests:          row.EligibleRequests,
			EligibleDenominatorTokens: row.EligibleDenominatorTokens,
			HitRateTokensEligible:     row.HitRateTokensEligible,
		})
	}
	return metrics
}

// clampRate01 把比例夹到 [0,1]；非有限值归 0（decision.ts 的 clampRate01）。
func clampRate01(value float64) float64 {
	switch {
	case value != value || value > 1e308 || value < -1e308:
		return 0
	case value < 0:
		return 0
	case value > 1:
		return 1
	default:
		return value
	}
}
