package notify

import (
	"context"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件的期望值按 **Node 源码** 手推，不是把 Go 的输出回抄：
//   - 判定：src/lib/cache-hit-rate-alert/decision.ts:194-321
//   - 窗口与设置解析：src/lib/notification/tasks/cache-hit-rate-alert.ts:221-256

func metric(providerID int64, model string, fields map[string]float64) CacheHitRateAlertMetric {
	return CacheHitRateAlertMetric{
		ProviderID:                providerID,
		Model:                     model,
		TotalRequests:             fields["total"],
		DenominatorTokens:         fields["totalTokens"],
		HitRateTokens:             fields["rate"],
		EligibleRequests:          fields["eligible"],
		EligibleDenominatorTokens: fields["eligibleTokens"],
		HitRateTokensEligible:     fields["eligibleRate"],
	}
}

func defaultDecisionSettings() CacheHitRateAlertDecisionSettings {
	return CacheHitRateAlertDecisionSettings{
		AbsMin:              0.05,
		DropRel:             0.3,
		DropAbs:             0.1,
		MinEligibleRequests: 20,
		MinEligibleTokens:   0,
		TopN:                10,
	}
}

// TestDecideCacheAnomalyAbsMinWithoutBaseline 钉住「没有可用基线时只看绝对下限」：
// 命中率 0.02 < 0.05 应入选，且 baselineSource 为 null、三个跌幅字段为空（decision.ts:231-255）。
func TestDecideCacheAnomalyAbsMinWithoutBaseline(t *testing.T) {
	anomalies := decideCacheHitRateAnomalies(cacheDecisionInput{
		Current: []CacheHitRateAlertMetric{
			metric(7, "deepseek-chat", map[string]float64{"eligible": 30, "eligibleTokens": 1000, "eligibleRate": 0.02}),
		},
		Settings: defaultDecisionSettings(),
	})
	if len(anomalies) != 1 {
		t.Fatalf("应判出 1 条，实际 %d", len(anomalies))
	}
	anomaly := anomalies[0]
	if anomaly.BaselineSource != nil || anomaly.Baseline != nil {
		t.Fatalf("无基线时 baselineSource/baseline 应为空: %+v", anomaly)
	}
	if anomaly.DeltaAbs != nil || anomaly.DeltaRel != nil || anomaly.DropAbs != nil {
		t.Fatalf("无基线时跌幅字段应为空: %+v", anomaly)
	}
	if !containsString(anomaly.ReasonCodes, cacheReasonBaselineMissing) ||
		!containsString(anomaly.ReasonCodes, cacheReasonAbsMin) {
		t.Fatalf("原因码应含 baseline_missing 与 abs_min: %v", anomaly.ReasonCodes)
	}
}

// TestDecideCacheAnomalyDropNeedsBothThresholds 钉住「绝对跌幅与相对跌幅同时成立才算跌幅触发」
// （decision.ts:273-277）：跌幅 0.15 >= 0.1 但 0.15/0.9 = 0.167 < 0.3，命中率 0.75 >= absMin 0.05，
// 故这条**不该**入选。
func TestDecideCacheAnomalyDropNeedsBothThresholds(t *testing.T) {
	anomalies := decideCacheHitRateAnomalies(cacheDecisionInput{
		Current: []CacheHitRateAlertMetric{
			metric(7, "m", map[string]float64{"eligible": 30, "eligibleTokens": 1000, "eligibleRate": 0.75}),
		},
		Historical: []CacheHitRateAlertMetric{
			metric(7, "m", map[string]float64{"eligible": 30, "eligibleTokens": 1000, "eligibleRate": 0.9}),
		},
		Settings: defaultDecisionSettings(),
	})
	if len(anomalies) != 0 {
		t.Fatalf("相对跌幅不足时不该入选: %+v", anomalies)
	}
}

// TestDecideCacheAnomalyEligibleBaselineMustMatchKind 钉住「基线口径必须与当前样本同 kind」
// （decision.ts:142-192）：当前只能退到 overall（eligible 请求数不足），此时历史窗口虽然 eligible 够量，
// 也不得拿来当基线——否则是拿两种口径比大小。
func TestDecideCacheAnomalyEligibleBaselineMustMatchKind(t *testing.T) {
	anomalies := decideCacheHitRateAnomalies(cacheDecisionInput{
		Current: []CacheHitRateAlertMetric{
			metric(7, "m", map[string]float64{
				"eligible": 3, "eligibleTokens": 10, "eligibleRate": 0.0,
				"total": 40, "totalTokens": 2000, "rate": 0.2,
			}),
		},
		Historical: []CacheHitRateAlertMetric{
			metric(7, "m", map[string]float64{
				"eligible": 40, "eligibleTokens": 2000, "eligibleRate": 0.9,
				"total": 3, "totalTokens": 10, "rate": 0.9,
			}),
		},
		Settings: defaultDecisionSettings(),
	})
	// 当前命中率 0.2 >= absMin 0.05，故没有「绝对下限」这条兜底，整条不入选。
	if len(anomalies) != 0 {
		t.Fatalf("历史窗口的 overall 样本不足，不该当基线: %+v", anomalies)
	}
}

// TestDecideCacheAnomalySortsBySeverityThenIds 钉住排序与 topN（decision.ts:304-320）：
// 严重度取 max(跌幅, absMin-当前, 0)，同严重度按 providerId、model 定序。
func TestDecideCacheAnomalySortsBySeverityThenIds(t *testing.T) {
	settings := defaultDecisionSettings()
	settings.TopN = 2
	anomalies := decideCacheHitRateAnomalies(cacheDecisionInput{
		Current: []CacheHitRateAlertMetric{
			metric(9, "b", map[string]float64{"eligible": 30, "eligibleTokens": 1000, "eligibleRate": 0.02}),
			metric(3, "a", map[string]float64{"eligible": 30, "eligibleTokens": 1000, "eligibleRate": 0.02}),
			metric(5, "c", map[string]float64{"eligible": 30, "eligibleTokens": 1000, "eligibleRate": 0.04}),
		},
		Settings: settings,
	})
	if len(anomalies) != 2 {
		t.Fatalf("topN=2 应截到 2 条，实际 %d", len(anomalies))
	}
	// 严重度：0.05-0.02=0.03 的两条排在 0.05-0.04=0.01 之前；同严重度按 providerId。
	if anomalies[0].ProviderID != 3 || anomalies[1].ProviderID != 9 {
		t.Fatalf("排序应为 providerId 3 在 9 前: %+v", anomalies)
	}
	if !containsString(anomalies[0].ReasonCodes, cacheReasonUseEligible) {
		t.Fatalf("eligible 够量时应带 use_eligible: %v", anomalies[0].ReasonCodes)
	}
}

// TestDecideCacheAnomalyTopNZeroDisables 钉住 `topN <= 0` 直接不判（decision.ts:198）。
func TestDecideCacheAnomalyTopNZeroDisables(t *testing.T) {
	settings := defaultDecisionSettings()
	settings.TopN = 0
	anomalies := decideCacheHitRateAnomalies(cacheDecisionInput{
		Current: []CacheHitRateAlertMetric{
			metric(7, "m", map[string]float64{"eligible": 30, "eligibleTokens": 1000, "eligibleRate": 0.0}),
		},
		Settings: settings,
	})
	if len(anomalies) != 0 {
		t.Fatalf("topN=0 时不该判出异常: %+v", anomalies)
	}
}

// TestResolveCacheSettingsMatchesNodeDefaults 钉住设置解析（tasks/cache-hit-rate-alert.ts:221-241）：
// 空设置下间隔 5、回看 7、冷却 30、absMin 0.05、dropRel 0.3、dropAbs 0.1、minEligibleRequests 20、topN 10；
// 回看天数上界 90（222-226），间隔下界 1（221）。
func TestResolveCacheSettingsMatchesNodeDefaults(t *testing.T) {
	resolved := ResolveCacheSettings(store.AdminNotificationSettings{})
	if resolved.CheckIntervalMinutes != 5 || resolved.HistoricalLookbackDays != 7 ||
		resolved.CooldownMinutes != 30 {
		t.Fatalf("间隔/回看/冷却的缺省不对: %+v", resolved)
	}
	decision := resolved.Decision
	if decision.AbsMin != 0.05 || decision.DropRel != 0.3 || decision.DropAbs != 0.1 ||
		decision.MinEligibleRequests != 20 || decision.MinEligibleTokens != 0 || decision.TopN != 10 {
		t.Fatalf("判定参数的缺省不对: %+v", decision)
	}
	// 间隔缺省 5 → auto 模式反推 5m 窗。
	if resolved.ResolvedWindowMode != CacheWindowMode5m || resolved.DurationMinutes != 5 {
		t.Fatalf("auto 模式应由间隔反推窗口: %+v", resolved)
	}

	lookback := 500
	interval := 0
	settings := store.AdminNotificationSettings{
		CacheHitRateAlertHistoricalLookbackDays: &lookback,
		CacheHitRateAlertCheckInterval:          &interval,
	}
	resolved = ResolveCacheSettings(settings)
	if resolved.HistoricalLookbackDays != 90 {
		t.Fatalf("回看天数应夹到 90，实际 %d", resolved.HistoricalLookbackDays)
	}
	if resolved.CheckIntervalMinutes != 1 {
		t.Fatalf("检查间隔下界应为 1，实际 %d", resolved.CheckIntervalMinutes)
	}
	// 间隔 1 → auto 仍取 5m 窗。
	if resolved.ResolvedWindowMode != CacheWindowMode5m {
		t.Fatalf("间隔 1 分钟应取 5m 窗，实际 %s", resolved.ResolvedWindowMode)
	}
}

// TestResolveCacheWindowModeExplicitWins 钉住显式窗口模式直用（tasks/...:51-59）。
func TestResolveCacheWindowModeExplicitWins(t *testing.T) {
	mode := "1.5h"
	resolved := ResolveCacheSettings(store.AdminNotificationSettings{
		CacheHitRateAlertWindowMode:    &mode,
		CacheHitRateAlertCheckInterval: intPtr(60),
	})
	if resolved.ResolvedWindowMode != CacheWindowMode15h || resolved.DurationMinutes != 90 {
		t.Fatalf("显式 1.5h 应用 90 分钟窗: %+v", resolved)
	}
	// 快照报的是设置原文（tasks/...:379）。
	if resolved.WindowModeSetting != "1.5h" {
		t.Fatalf("快照应报原文，实际 %q", resolved.WindowModeSetting)
	}
}

// TestBuildCacheHitRateAlertCooldownKey 钉住冷却键形状（tasks/...:73-87）：
// `cache-hit-rate-alert:v1[:binding:<id>]:<providerId>:<base64url(model)>:<windowMode>`。
func TestBuildCacheHitRateAlertCooldownKey(t *testing.T) {
	legacy := BuildCacheHitRateAlertCooldownKey(cooldownKeyParams{
		ProviderID: 156, Model: "deepseek-v4-flash", WindowMode: "5m",
	})
	if legacy != "cache-hit-rate-alert:v1:156:ZGVlcHNlZWstdjQtZmxhc2g:5m" {
		t.Fatalf("legacy 键不对: %s", legacy)
	}
	withBinding := BuildCacheHitRateAlertCooldownKey(cooldownKeyParams{
		ProviderID: 156, Model: "deepseek-v4-flash", WindowMode: "5m", BindingID: 42,
	})
	if withBinding != "cache-hit-rate-alert:v1:binding:42:156:ZGVlcHNlZWstdjQtZmxhc2g:5m" {
		t.Fatalf("targets 键不对: %s", withBinding)
	}
}

// fakeCooldownReader 是 Cooldown 的假实现：Present 按 present 列表作答，Set 记账。
type fakeCooldownReader struct {
	present map[string]bool
	set     []string
	ttl     time.Duration
	getErr  error
}

func (c *fakeCooldownReader) Present(_ context.Context, keys []string) ([]bool, error) {
	if c.getErr != nil {
		return nil, c.getErr
	}
	result := make([]bool, len(keys))
	for index, key := range keys {
		result[index] = c.present[key]
	}
	return result, nil
}

func (c *fakeCooldownReader) Set(_ context.Context, keys []string, ttl time.Duration) error {
	c.set = append(c.set, keys...)
	c.ttl = ttl
	return nil
}

func intPtr(value int) *int { return &value }

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
