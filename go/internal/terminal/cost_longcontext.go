package terminal

import "math"

// 本文件复刻 src/lib/utils/cost-calculation.ts 的 long-context 分层定价解析
// （resolveLongContextPricing / resolveLongContextThreshold / matchLongContextPricing /
// getLongContextTriggerInputTokens）与 priority 档取价规则。
//
// 与 Node 的形状差异（刻意）：Node 由调用方先算 `longContextPricing` 再作为选项传进
// calculateRequestCost，是因为它有三个调用点（流式/非流式/回放）。Go 的计费入口只有
// dataplane 一处，故把「命中判定」留在本包内联——命中语义只有一份实现，调用方只需给用量。
//
// 纯函数：只依赖 price_data 与本次用量，无 IO，可直接单测。

// long-context 阈值两档（Node cost-calculation.ts:7 与 special-attributes/index.ts:15）。
//
// 为什么两档：OpenAI 系（price_data 里带 272k 字段，或 model_family 为 gpt/gpt-pro）
// 的分层起点是 272k，其余（Gemini 等）是 200k。判定依据是**价格字段是否存在**，
// 不是模型名，故不会被模型改名带偏。
const (
	openAILongContextTokenThreshold  int64 = 272000
	defaultLongContextTokenThreshold int64 = 200000
)

// LongContextPricing 是 price_data.long_context_pricing 的读取视图
// （Node types/model-price.ts 的 LongContextPricing）。
type LongContextPricing struct {
	ThresholdTokens                      *float64 `json:"threshold_tokens"`
	Scope                                string   `json:"scope"`
	InputMultiplier                      *float64 `json:"input_multiplier"`
	OutputMultiplier                     *float64 `json:"output_multiplier"`
	CacheCreationInputMultiplier         *float64 `json:"cache_creation_input_multiplier"`
	CacheCreationInputMultiplierAbove1hr *float64 `json:"cache_creation_input_multiplier_above_1hr"`
	CacheReadInputMultiplier             *float64 `json:"cache_read_input_multiplier"`
	InputCostPerToken                    *float64 `json:"input_cost_per_token"`
	OutputCostPerToken                   *float64 `json:"output_cost_per_token"`
	CacheCreationInputTokenCost          *float64 `json:"cache_creation_input_token_cost"`
	CacheCreationInputTokenCostAbove1hr  *float64 `json:"cache_creation_input_token_cost_above_1hr"`
	CacheReadInputTokenCost              *float64 `json:"cache_read_input_token_cost"`
}

// ResolvedLongContextPricing 是解析后的分层单价（Node 的 ResolvedLongContextPricing）。
//
// 各单价为 nil 表示该桶在 long_context_pricing 里没有可用值，计费要回落到
// 「显式分层单价（above_*）」或基础单价，而不是按 0 计。
type ResolvedLongContextPricing struct {
	ThresholdTokens int64
	// Scope 取 "request" 或 "session"（Node 的 LongContextPricingScope）。
	Scope                               string
	InputCostPerToken                   *float64
	OutputCostPerToken                  *float64
	CacheCreationInputTokenCost         *float64
	CacheCreationInputTokenCostAbove1hr *float64
	CacheReadInputTokenCost             *float64
}

// LongContextPricingMatch 是一次命中的分层定价（Node 的 LongContextPricingMatch）。
//
// ObservedInputTokens 是判定用的输入上下文总量，供审计留痕（Node 的
// createLongContextPricingAudit 记的就是它）。
type LongContextPricingMatch struct {
	ThresholdTokens     int64
	Scope               string
	ObservedInputTokens int64
	Pricing             ResolvedLongContextPricing
}

// ResolveLongContextThreshold 复刻 resolveLongContextThreshold。
func ResolveLongContextThreshold(price PriceData) int64 {
	if has272kLongContextFields(price) || price.ModelFamily == "gpt" || price.ModelFamily == "gpt-pro" {
		return openAILongContextTokenThreshold
	}
	return defaultLongContextTokenThreshold
}

// has272kLongContextFields 判定 price_data 是否带 272k 档字段（Node 的 has272kFields）。
func has272kLongContextFields(price PriceData) bool {
	return price.InputCostPerTokenAbove272k != nil ||
		price.InputCostPerTokenAbove272kPriority != nil ||
		price.OutputCostPerTokenAbove272k != nil ||
		price.OutputCostPerTokenAbove272kPriority != nil ||
		price.CacheCreationInputTokenCostAbove272k != nil ||
		price.CacheReadInputTokenCostAbove272k != nil ||
		price.CacheReadInputTokenCostAbove272kPriority != nil ||
		price.CacheCreationInputTokenCostAbove1hrAbove272k != nil
}

// GetLongContextTriggerInputTokens 复刻 getLongContextTriggerInputTokens：
// 输入上下文总量 = 新鲜输入 + 缓存创建 + 缓存读取 + 输入图片 token。
//
// cache5m/cache1h 只有在 usage 没给 cache_creation_input_tokens 聚合值时参与求和
// （Node 的 `??` 语义），调用方按是否已做过 TTL 归档决定要不要传。
func GetLongContextTriggerInputTokens(usage Usage, cache5m, cache1h *int64) int64 {
	var cacheCreation int64
	if usage.CacheCreationInputTokens != nil {
		cacheCreation = *usage.CacheCreationInputTokens
	} else {
		if cache5m != nil {
			cacheCreation += *cache5m
		}
		if cache1h != nil {
			cacheCreation += *cache1h
		}
	}
	total := cacheCreation
	if usage.InputTokens != nil {
		total += *usage.InputTokens
	}
	if usage.CacheReadInputTokens != nil {
		total += *usage.CacheReadInputTokens
	}
	if usage.InputImageTokens != nil {
		total += *usage.InputImageTokens
	}
	return total
}

// MatchLongContextPricing 复刻 matchLongContextPricing：显式分层定价存在、且本次
// 输入上下文总量**严格超过**其 threshold_tokens 时命中，否则为 nil。
func MatchLongContextPricing(usage Usage, price PriceData) *LongContextPricingMatch {
	pricing := ResolveLongContextPricing(price)
	if pricing == nil {
		return nil
	}
	observed := GetLongContextTriggerInputTokens(usage, nil, nil)
	if observed <= pricing.ThresholdTokens {
		return nil
	}
	return &LongContextPricingMatch{
		ThresholdTokens:     pricing.ThresholdTokens,
		Scope:               pricing.Scope,
		ObservedInputTokens: observed,
		Pricing:             *pricing,
	}
}

// ResolveLongContextPricing 复刻 resolveLongContextPricing：把 price_data.long_context_pricing
// 归一成可用的分层单价。
//
// 单价来源三级：显式单价 → 基础单价 × 倍率 → nil（该桶回落到非分层路径）。
// 倍率优先取显式倍率，其次由显式单价 / 基础单价反推（deriveMultiplier）。
// 五个桶全无可用单价时返回 nil——此时整块 long_context_pricing 视为不存在。
func ResolveLongContextPricing(price PriceData) *ResolvedLongContextPricing {
	pricing := price.LongContextPricing
	if pricing == nil {
		return nil
	}
	threshold := normalizeThresholdTokens(pricing.ThresholdTokens)
	if threshold == nil {
		return nil
	}

	baseInput := price.InputCostPerToken
	baseOutput := price.OutputCostPerToken
	baseCacheCreation5m := coalesceRate(price.CacheCreationInputTokenCost, scaleRate(baseInput, 1.25))
	baseCacheCreation1h := coalesceRate(
		price.CacheCreationInputTokenCostAbove1hr,
		coalesceRate(scaleRate(baseInput, 2), baseCacheCreation5m),
	)
	baseCacheRead := coalesceRate(
		price.CacheReadInputTokenCost,
		coalesceRate(scaleRate(baseInput, 0.1), scaleRate(baseOutput, 0.1)),
	)

	inputMultiplier := usableRatePtr(pricing.InputMultiplier)
	if inputMultiplier == nil {
		inputMultiplier = deriveMultiplier(pricing.InputCostPerToken, baseInput)
	}
	outputMultiplier := usableRatePtr(pricing.OutputMultiplier)
	if outputMultiplier == nil {
		outputMultiplier = deriveMultiplier(pricing.OutputCostPerToken, baseOutput)
	}
	cacheCreationMultiplier := usableRatePtr(pricing.CacheCreationInputMultiplier)
	if cacheCreationMultiplier == nil {
		cacheCreationMultiplier = inputMultiplier
	}
	cacheCreation1hMultiplier := usableRatePtr(pricing.CacheCreationInputMultiplierAbove1hr)
	if cacheCreation1hMultiplier == nil {
		cacheCreation1hMultiplier = cacheCreationMultiplier
	}
	cacheReadMultiplier := usableRatePtr(pricing.CacheReadInputMultiplier)
	if cacheReadMultiplier == nil {
		cacheReadMultiplier = inputMultiplier
	}

	inputCost := resolvePremiumUnitCost(
		pricing.InputCostPerToken, baseInput, pricing.InputMultiplier, inputMultiplier)
	outputCost := resolvePremiumUnitCost(
		pricing.OutputCostPerToken, baseOutput, pricing.OutputMultiplier, outputMultiplier)
	cacheCreationCost := resolvePremiumUnitCost(
		pricing.CacheCreationInputTokenCost, baseCacheCreation5m,
		pricing.CacheCreationInputMultiplier, cacheCreationMultiplier)
	cacheCreation1hCost := resolvePremiumUnitCost(
		pricing.CacheCreationInputTokenCostAbove1hr, baseCacheCreation1h,
		pricing.CacheCreationInputMultiplierAbove1hr, cacheCreation1hMultiplier)
	cacheReadCost := resolvePremiumUnitCost(
		pricing.CacheReadInputTokenCost, baseCacheRead,
		pricing.CacheReadInputMultiplier, cacheReadMultiplier)

	if inputCost == nil && outputCost == nil && cacheCreationCost == nil &&
		cacheCreation1hCost == nil && cacheReadCost == nil {
		return nil
	}
	return &ResolvedLongContextPricing{
		ThresholdTokens:                     *threshold,
		Scope:                               getLongContextScope(pricing.Scope),
		InputCostPerToken:                   inputCost,
		OutputCostPerToken:                  outputCost,
		CacheCreationInputTokenCost:         cacheCreationCost,
		CacheCreationInputTokenCostAbove1hr: cacheCreation1hCost,
		CacheReadInputTokenCost:             cacheReadCost,
	}
}

// normalizeThresholdTokens 复刻 normalizeThresholdTokens：非有限、负数、0 一律无效
// （返回 nil 表示整块分层定价不可用，而不是「阈值取 0」）。
func normalizeThresholdTokens(value *float64) *int64 {
	if value == nil || !isUsableRate(*value) || *value <= 0 {
		return nil
	}
	threshold := int64(math.Floor(*value))
	return &threshold
}

// deriveMultiplier 复刻 deriveMultiplier：由显式单价与基础单价反推倍率。
// 任一侧不可用、或基础单价为 0（除不出有意义的倍率）时返回 nil。
func deriveMultiplier(explicitCost, baseCost *float64) *float64 {
	if usableRatePtr(explicitCost) == nil || usableRatePtr(baseCost) == nil || *baseCost <= 0 {
		return nil
	}
	value := *explicitCost / *baseCost
	return &value
}

// resolvePremiumUnitCost 复刻 resolvePremiumUnitCost：显式单价优先；没有显式单价时用
// 基础单价乘倍率（显式倍率优先于传入的兜底倍率）。两侧都取不到时返回 nil。
func resolvePremiumUnitCost(
	explicitCost, baseCost, explicitMultiplier, fallbackMultiplier *float64,
) *float64 {
	if explicit := usableRatePtr(explicitCost); explicit != nil {
		return explicit
	}
	if usableRatePtr(baseCost) == nil {
		return nil
	}
	multiplier := usableRatePtr(explicitMultiplier)
	if multiplier == nil {
		multiplier = usableRatePtr(fallbackMultiplier)
	}
	if multiplier == nil {
		return nil
	}
	value := *baseCost * *multiplier
	return &value
}

// getLongContextScope 复刻 getLongContextScope：只认 "session"，其余一律 "request"。
func getLongContextScope(value string) string {
	if value == "session" {
		return "session"
	}
	return "request"
}

// resolvePriorityAwareLongContextRate 复刻 resolvePriorityAwareLongContextRate：
// priority 档优先取 priority 变体，且 **272k 优先于 200k**；非 priority 档不看 priority 变体。
func resolvePriorityAwareLongContextRate(
	priorityApplied bool,
	above272k, above272kPriority, above200k, above200kPriority *float64,
) *float64 {
	if priorityApplied {
		return coalesceRate(above272kPriority, coalesceRate(above200kPriority,
			coalesceRate(above272k, above200k)))
	}
	return coalesceRate(above272k, above200k)
}

// selectPriorityRate 复刻 Node 的 `priorityServiceTierApplied && typeof x === "number" ? x : base`。
//
// 判据是「字段存在」而非「字段可用」：不可用的档位价格由 multiplyCost 归零，
// 这样「档位价写坏了」不会静默回落到基础价（少收），也不会误用坏值（多收）。
func selectPriorityRate(priorityApplied bool, priorityRate, baseRate *float64) *float64 {
	if priorityApplied && priorityRate != nil {
		return priorityRate
	}
	return baseRate
}

// usableRatePtr 把「有限且非负」的单价原样返回，否则返回 nil（对应 Node 的
// isFiniteNonNegativeNumber 判定）。
func usableRatePtr(value *float64) *float64 {
	if value == nil || !isUsableRate(*value) {
		return nil
	}
	return value
}

// coalesceRate 复刻 TS 的 `a ?? b`：只按「是否为 null/undefined」取舍，不看可用性。
func coalesceRate(first, second *float64) *float64 {
	if first != nil {
		return first
	}
	return second
}

// scaleRate 是「基础单价 × 系数」的派生；基础单价缺失时返回 nil（Node 的三元同样）。
func scaleRate(base *float64, factor float64) *float64 {
	if base == nil {
		return nil
	}
	value := *base * factor
	return &value
}
