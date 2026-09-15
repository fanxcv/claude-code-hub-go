package notify

import (
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是缓存命中率告警的**判定**：输入一个「供应商×模型」在若干窗口上的口径行，
// 输出「是否异常」以及异常正文。
//
// **登记：这一段是重建的。** Node 的 tasks/cache-hit-rate-alert.ts 随 Node 层删除，且不在
// 本仓 git 历史里（仓库是压缩导入，只有 8 个提交）。契约（payload 形状与字段名）有真源：
// `src/lib/webhook/types.ts`；判定口径没有真源，只能按键名与 UI 文案
// （messages/zh-CN/settings/notifications.json：absMin「绝对下限」、dropAbs「绝对跌幅阈值」、
// dropRel「相对跌幅阈值」、minEligibleRequests「最小可命中请求数」、
// minEligibleTokens「最小可命中 Tokens」）重建为下面这套最简规则：
//
//  1. 当前窗口的**可命中样本**（eligible）必须够量（requests >= minEligibleRequests 且
//     tokens >= minEligibleTokens），否则该条不判：样本不足时任何跌幅都是噪声。
//  2. 基线按 historical -> today -> prev 取第一个**同样够量**的基线；三个都不够量则不判
//     （baselineSource 在契约里是三选一或 null，null 的情况不产生异常条目）。
//  3. 异常条件是**二者之一**：
//     - 命中率低于绝对下限 absMin；或
//     - 绝对跌幅 >= dropAbs **且** 相对跌幅 >= dropRel。
//     要求两个跌幅阈值同时成立：只看相对会在低命中率上放大噪声，只看绝对会漏掉高命中率上的
//     大幅相对下滑。
//
// 敏感度只由本函数决定：要改口径，改这里，别在生成器里打补丁。
const (
	cacheKindEligible = "eligible"
	cacheKindOverall  = "overall"

	cacheBaselineHistorical = "historical"
	cacheBaselineToday      = "today"
	cacheBaselinePrev       = "prev"

	cacheReasonBelowAbsMin = "below_abs_min"
	cacheReasonDropAbs     = "drop_abs"
	cacheReasonDropRel     = "drop_rel"
)

// cacheDecisionInput 是一次判定的输入：当前窗口 + 三个候选基线 + 生效设置。
type cacheDecisionInput struct {
	Current    store.NotifyCacheMetric
	Historical *store.NotifyCacheMetric
	Today      *store.NotifyCacheMetric
	Prev       *store.NotifyCacheMetric
	Settings   CacheSettings
}

// decideCacheAnomaly 判定一条「供应商×模型」是否是异常，并组好正文。
func decideCacheAnomaly(input cacheDecisionInput) (CacheHitRateAlertAnomaly, bool) {
	current := cacheSample(input.Current, input.Settings)
	if current.Kind != cacheKindEligible {
		return CacheHitRateAlertAnomaly{}, false
	}
	baseline, baselineSource := pickCacheBaseline(input)
	if baseline == nil {
		return CacheHitRateAlertAnomaly{}, false
	}

	deltaAbs := current.HitRateTokens - baseline.HitRateTokens
	dropAbs := -deltaAbs
	deltaRel := 0.0
	if baseline.HitRateTokens > 0 {
		deltaRel = deltaAbs / baseline.HitRateTokens
	}
	dropRel := -deltaRel

	reasons := make([]string, 0, 3)
	if current.HitRateTokens < input.Settings.AbsMin {
		reasons = append(reasons, cacheReasonBelowAbsMin)
	}
	if dropAbs >= input.Settings.DropAbs {
		reasons = append(reasons, cacheReasonDropAbs)
	}
	if dropRel >= input.Settings.DropRel {
		reasons = append(reasons, cacheReasonDropRel)
	}
	belowAbsMin := current.HitRateTokens < input.Settings.AbsMin
	dropTriggered := dropAbs >= input.Settings.DropAbs && dropRel >= input.Settings.DropRel
	if !belowAbsMin && !dropTriggered {
		return CacheHitRateAlertAnomaly{}, false
	}

	return CacheHitRateAlertAnomaly{
		ProviderID:     input.Current.ProviderID,
		ProviderType:   input.Current.ProviderType,
		Model:          input.Current.Model,
		BaselineSource: baselineSource,
		Current:        current,
		Baseline:       baseline,
		DeltaAbs:       &deltaAbs,
		DeltaRel:       &deltaRel,
		DropAbs:        &dropAbs,
		ReasonCodes:    reasons,
	}, true
}

// cacheSample 按样本量门槛选口径：够量取 eligible（只算「上一发请求在同一会话、序号连续、
// 且间隔不超过缓存 TTL」的行），否则退回 overall。
func cacheSample(metric store.NotifyCacheMetric, settings CacheSettings) CacheHitRateAlertSample {
	enough := metric.EligibleRequests >= float64(settings.MinEligibleRequests) &&
		metric.EligibleDenominatorTokens >= float64(settings.MinEligibleTokens)
	if enough {
		return CacheHitRateAlertSample{
			Kind:              cacheKindEligible,
			Requests:          metric.EligibleRequests,
			DenominatorTokens: metric.EligibleDenominatorTokens,
			HitRateTokens:     metric.HitRateTokensEligible,
		}
	}
	return CacheHitRateAlertSample{
		Kind:              cacheKindOverall,
		Requests:          metric.TotalRequests,
		DenominatorTokens: metric.DenominatorTokens,
		HitRateTokens:     metric.HitRateTokens,
	}
}

// pickCacheBaseline 复刻基线优先级：历史基线（长期）比今日、比上一窗更稳，
// 但只有够量的基线才可用。
func pickCacheBaseline(input cacheDecisionInput) (*CacheHitRateAlertSample, string) {
	candidates := []struct {
		source string
		metric *store.NotifyCacheMetric
	}{
		{source: cacheBaselineHistorical, metric: input.Historical},
		{source: cacheBaselineToday, metric: input.Today},
		{source: cacheBaselinePrev, metric: input.Prev},
	}
	for _, candidate := range candidates {
		if candidate.metric == nil {
			continue
		}
		sample := cacheSample(*candidate.metric, input.Settings)
		if sample.Kind != cacheKindEligible {
			continue
		}
		return &sample, candidate.source
	}
	return nil, ""
}
