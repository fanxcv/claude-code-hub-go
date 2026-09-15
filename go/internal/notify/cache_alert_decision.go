package notify

import (
	"sort"
	"strconv"
	"strings"
)

// 本文件是 `src/lib/cache-hit-rate-alert/decision.ts` 的逐行移植。
//
// 判定口径（源文件 194-321 行）：
//   - 当前窗口每行先取样本：eligible 够量就用 eligible，否则退 overall（仍要够量），都不够则跳过；
//   - 命中率低于 absMin 时**即使没有基线**也告警（baselineSource=null、三个跌幅字段为 null）；
//   - 有基线时：跌幅 = baseline - current，相对跌幅用跌幅（非负）除以基线；
//     「绝对跌幅 >= dropAbs 且 相对跌幅 >= dropRel」两条同时成立才算跌幅触发；
//   - 严重度 = max(跌幅, absMin - 当前命中率, 0)，按严重度倒序、providerId、model、key 依次定序，
//     最后截到 topN（topN <= 0 直接不判）。
const (
	cacheKindEligible = "eligible"
	cacheKindOverall  = "overall"

	cacheBaselineHistorical = "historical"
	cacheBaselineToday      = "today"
	cacheBaselinePrev       = "prev"

	cacheReasonUseEligible          = "use_eligible"
	cacheReasonUseOverall           = "use_overall"
	cacheReasonEligibleInsufficient = "eligible_insufficient"
	cacheReasonBaselineEligible     = "baseline_eligible"
	cacheReasonBaselineOverall      = "baseline_overall"
	cacheReasonBaselineMissing      = "baseline_missing"
	cacheReasonAbsMin               = "abs_min"
	cacheReasonDropAbsRel           = "drop_abs_rel"
)

// CacheHitRateAlertMetric 是判定输入的一行（decision.ts 的 CacheHitRateAlertMetric）。
//
// 与取数行的差别：取数行还带 cacheSignalRequests / sumInputTokens 等诊断列，判定只用下面这些。
type CacheHitRateAlertMetric struct {
	ProviderID int64
	Model      string

	TotalRequests     float64
	DenominatorTokens float64
	HitRateTokens     float64

	EligibleRequests          float64
	EligibleDenominatorTokens float64
	HitRateTokensEligible     float64
}

// CacheHitRateAlertDecisionSettings 是判定参数（decision.ts 的 CacheHitRateAlertDecisionSettings）。
type CacheHitRateAlertDecisionSettings struct {
	AbsMin              float64
	DropRel             float64
	DropAbs             float64
	MinEligibleRequests float64
	MinEligibleTokens   float64
	TopN                int
}

// cacheDecisionInput 是判定的四个窗口口径。
type cacheDecisionInput struct {
	Current    []CacheHitRateAlertMetric
	Prev       []CacheHitRateAlertMetric
	Today      []CacheHitRateAlertMetric
	Historical []CacheHitRateAlertMetric
	Settings   CacheHitRateAlertDecisionSettings
}

// decidedCacheAnomaly 是排序用的中间结构：异常正文 + 严重度。
type decidedCacheAnomaly struct {
	anomaly  CacheHitRateAlertAnomaly
	severity float64
}

// cacheMetricKey 复刻 toCacheHitRateAlertMetricKey：`${providerId}:${model}`。
func cacheMetricKey(providerID int64, model string) string {
	return strconv.FormatInt(providerID, 10) + ":" + model
}

// toCacheMetricMap 复刻 toMetricMap：模型名为空/全空白的行丢弃（它们无法与基线对齐）。
func toCacheMetricMap(metrics []CacheHitRateAlertMetric) map[string]CacheHitRateAlertMetric {
	if len(metrics) == 0 {
		return nil
	}
	index := make(map[string]CacheHitRateAlertMetric, len(metrics))
	for _, metric := range metrics {
		if metric.Model == "" || strings.TrimSpace(metric.Model) == "" {
			continue
		}
		index[cacheMetricKey(metric.ProviderID, metric.Model)] = metric
	}
	return index
}

// pickCacheSample 复刻 pickSample：先试 eligible，再退 overall，都不够量返回 nil。
func pickCacheSample(
	metric CacheHitRateAlertMetric,
	settings CacheHitRateAlertDecisionSettings,
) (*CacheHitRateAlertSample, []string) {
	if metric.EligibleRequests >= settings.MinEligibleRequests &&
		metric.EligibleDenominatorTokens >= settings.MinEligibleTokens {
		return &CacheHitRateAlertSample{
			Kind:              cacheKindEligible,
			Requests:          metric.EligibleRequests,
			DenominatorTokens: metric.EligibleDenominatorTokens,
			HitRateTokens:     clampRate01(metric.HitRateTokensEligible),
		}, []string{cacheReasonUseEligible}
	}

	if metric.TotalRequests < settings.MinEligibleRequests ||
		metric.DenominatorTokens < settings.MinEligibleTokens {
		return nil, nil
	}
	return &CacheHitRateAlertSample{
		Kind:              cacheKindOverall,
		Requests:          metric.TotalRequests,
		DenominatorTokens: metric.DenominatorTokens,
		HitRateTokens:     clampRate01(metric.HitRateTokens),
	}, []string{cacheReasonUseOverall, cacheReasonEligibleInsufficient}
}

// cacheBaselineCandidate 是一个候选基线：来源 + 该窗口的口径。
type cacheBaselineCandidate struct {
	source string
	index  map[string]CacheHitRateAlertMetric
}

// pickCacheBaseline 复刻 pickBaseline：**基线的样本口径必须与当前样本同 kind**
// （当前用 eligible 就要 eligible 基线，当前退到 overall 就用 overall 基线）。
func pickCacheBaseline(
	kind string,
	key string,
	candidates []cacheBaselineCandidate,
	settings CacheHitRateAlertDecisionSettings,
) (*CacheHitRateAlertSample, string, []string) {
	for _, candidate := range candidates {
		metric, ok := candidate.index[key]
		if !ok {
			continue
		}
		sample := CacheHitRateAlertSample{Kind: kind}
		if kind == cacheKindEligible {
			if metric.EligibleRequests < settings.MinEligibleRequests ||
				metric.EligibleDenominatorTokens < settings.MinEligibleTokens {
				continue
			}
			sample.Requests = metric.EligibleRequests
			sample.DenominatorTokens = metric.EligibleDenominatorTokens
			sample.HitRateTokens = clampRate01(metric.HitRateTokensEligible)
		} else {
			if metric.TotalRequests < settings.MinEligibleRequests ||
				metric.DenominatorTokens < settings.MinEligibleTokens {
				continue
			}
			sample.Requests = metric.TotalRequests
			sample.DenominatorTokens = metric.DenominatorTokens
			sample.HitRateTokens = clampRate01(metric.HitRateTokens)
		}

		kindCode := cacheReasonBaselineEligible
		if kind != cacheKindEligible {
			kindCode = cacheReasonBaselineOverall
		}
		return &sample, candidate.source, []string{kindCode, "baseline_" + candidate.source}
	}
	return nil, "", nil
}

// decideCacheHitRateAnomalies 复刻 decideCacheHitRateAnomalies。
func decideCacheHitRateAnomalies(input cacheDecisionInput) []CacheHitRateAlertAnomaly {
	settings := input.Settings
	if settings.TopN <= 0 {
		return nil
	}

	currentMap := toCacheMetricMap(input.Current)
	if len(currentMap) == 0 {
		return nil
	}
	candidates := []cacheBaselineCandidate{
		{source: cacheBaselineHistorical, index: toCacheMetricMap(input.Historical)},
		{source: cacheBaselineToday, index: toCacheMetricMap(input.Today)},
		{source: cacheBaselinePrev, index: toCacheMetricMap(input.Prev)},
	}

	decided := make([]decidedCacheAnomaly, 0, len(currentMap))
	for key, currentMetric := range currentMap {
		current, currentReasons := pickCacheSample(currentMetric, settings)
		if current == nil {
			continue
		}
		currentValue := current.HitRateTokens
		absMinTriggered := currentValue < settings.AbsMin

		baseline, baselineSource, baselineReasons := pickCacheBaseline(
			current.Kind, key, candidates, settings,
		)
		if baseline == nil {
			// 没有可用基线：只有「低于绝对下限」才算异常，且三个跌幅字段留 null。
			if !absMinTriggered {
				continue
			}
			reasons := append(append([]string{}, currentReasons...), cacheReasonBaselineMissing, cacheReasonAbsMin)
			decided = append(decided, decidedCacheAnomaly{
				severity: maxFloat(settings.AbsMin-currentValue, 0),
				anomaly: CacheHitRateAlertAnomaly{
					ProviderID:     currentMetric.ProviderID,
					Model:          currentMetric.Model,
					BaselineSource: nil,
					Current:        *current,
					Baseline:       nil,
					DeltaAbs:       nil,
					DeltaRel:       nil,
					DropAbs:        nil,
					ReasonCodes:    reasons,
				},
			})
			continue
		}

		baselineValue := baseline.HitRateTokens
		deltaAbs := currentValue - baselineValue
		dropAbs := maxFloat(baselineValue-currentValue, 0)
		var deltaRel *float64
		if baselineValue > 0 {
			value := deltaAbs / baselineValue
			deltaRel = &value
		}

		reasons := append(append([]string{}, currentReasons...), baselineReasons...)
		triggered := make([]string, 0, 2)
		if absMinTriggered {
			triggered = append(triggered, cacheReasonAbsMin)
		}
		if baselineValue > 0 &&
			dropAbs >= settings.DropAbs &&
			dropAbs/baselineValue >= settings.DropRel {
			triggered = append(triggered, cacheReasonDropAbsRel)
		}
		if len(triggered) == 0 {
			continue
		}
		reasons = append(reasons, triggered...)

		decided = append(decided, decidedCacheAnomaly{
			severity: maxFloat(maxFloat(dropAbs, settings.AbsMin-currentValue), 0),
			anomaly: CacheHitRateAlertAnomaly{
				ProviderID:     currentMetric.ProviderID,
				Model:          currentMetric.Model,
				BaselineSource: &baselineSource,
				Current:        *current,
				Baseline:       baseline,
				DeltaAbs:       &deltaAbs,
				DeltaRel:       deltaRel,
				DropAbs:        &dropAbs,
				ReasonCodes:    reasons,
			},
		})
	}

	sort.SliceStable(decided, func(left, right int) bool {
		if decided[left].severity != decided[right].severity {
			return decided[left].severity > decided[right].severity
		}
		if decided[left].anomaly.ProviderID != decided[right].anomaly.ProviderID {
			return decided[left].anomaly.ProviderID < decided[right].anomaly.ProviderID
		}
		if decided[left].anomaly.Model != decided[right].anomaly.Model {
			return decided[left].anomaly.Model < decided[right].anomaly.Model
		}
		return cacheMetricKey(decided[left].anomaly.ProviderID, decided[left].anomaly.Model) <
			cacheMetricKey(decided[right].anomaly.ProviderID, decided[right].anomaly.Model)
	})

	limit := settings.TopN
	if limit > len(decided) {
		limit = len(decided)
	}
	anomalies := make([]CacheHitRateAlertAnomaly, 0, limit)
	for _, item := range decided[:limit] {
		anomalies = append(anomalies, item.anomaly)
	}
	return anomalies
}

// maxFloat 是 math.Max 的两参版本（避免为两处取大引入 math 依赖的可读性损失）。
func maxFloat(left, right float64) float64 {
	if left > right {
		return left
	}
	return right
}
