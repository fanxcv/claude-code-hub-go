package terminal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/shopspring/decimal"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// ErrPriceNotFound 表示价格表里没有该模型的价格。与 TS 一致：取不到价格就跳过计费，
// 不写成本也不报错（response-handler.ts 的 `if (!resolvedPricing?.priceData ...) return null`）。
var ErrPriceNotFound = errors.New("terminal: 价格表无该模型的价格")

// ErrPriceDataInvalid 表示价格数据不是可用的 jsonb 对象。
var ErrPriceDataInvalid = errors.New("terminal: 价格数据不可用")

// Usage 是本次请求的上游用量计量。指针为 nil 表示上游未提供该字段，
// 与 TS 的 `usage.x != null` 判定等价（缺项按 0 计，不报错）。
type Usage struct {
	InputTokens                *int64
	OutputTokens               *int64
	CacheCreationInputTokens   *int64
	CacheCreation5mInputTokens *int64
	CacheCreation1hInputTokens *int64
	CacheReadInputTokens       *int64
	// CacheTTL 是缓存创建 token 的 TTL 归属："5m"、"1h" 或 "mixed"；
	// 与 cache_creation_input_tokens 的差额部分按此归档（TS 同款派生规则）。
	CacheTTL string
}

// PriceData 是 model_prices.price_data 的读取视图，只取本波用到的字段。
type PriceData struct {
	InputCostPerToken                   *float64 `json:"input_cost_per_token"`
	OutputCostPerToken                  *float64 `json:"output_cost_per_token"`
	InputCostPerRequest                 *float64 `json:"input_cost_per_request"`
	CacheCreationInputTokenCost         *float64 `json:"cache_creation_input_token_cost"`
	CacheCreationInputTokenCostAbove1hr *float64 `json:"cache_creation_input_token_cost_above_1hr"`
	CacheReadInputTokenCost             *float64 `json:"cache_read_input_token_cost"`
}

// Cost 是可写库的成本结果：cost_usd 的定点字符串与 cost_breakdown 的 jsonb 载荷。
type Cost struct {
	Total     string
	Breakdown []byte
}

// CostInput 是一次成本计算的输入。
//
// 倍率用指针是刻意的：Go 结构体的零值是 0，而 0 是一个合法的「免费」倍率。
// 若用裸 float64，调用方漏填倍率就会静静地按 0 计费（少收钱），这是账务上最难发现的错。
// nil 表示「未提供」按 1 计，显式传 0 才表示免费。
type CostInput struct {
	Usage     Usage
	PriceData []byte
	// ProviderMultiplier 为 nil 时按 1 计。
	ProviderMultiplier *float64
	// GroupMultiplier 为 nil 时按 1 计。
	GroupMultiplier *float64
}

// StoredCostBreakdown 复刻 src/types/cost-breakdown.ts 的 StoredCostBreakdown。
// 成本一律为十进制字符串（与 TS 的 String(decimal) 同形），倍率为数字。
type StoredCostBreakdown struct {
	Input              string  `json:"input"`
	Output             string  `json:"output"`
	CacheCreation      string  `json:"cache_creation"`
	CacheCreation5m    string  `json:"cache_creation_5m"`
	CacheCreation1h    string  `json:"cache_creation_1h"`
	CacheRead          string  `json:"cache_read"`
	BaseTotal          string  `json:"base_total"`
	ProviderMultiplier float64 `json:"provider_multiplier"`
	GroupMultiplier    float64 `json:"group_multiplier"`
	Total              string  `json:"total"`
}

// ComputeCost 复刻 calculateRequestCost 与 calculateRequestCostBreakdown 的**基础口径**：
// 各费用桶按 token × 单价累加，total = 桶和 × provider 倍率 × group 倍率，全链 Decimal 定点。
//
// 本波有意未搬运的分支（留待后续波次，参数缺口在此登记）：
//   - long-context 分层价格（above_200k / above_272k 及其 priority 变体）；
//   - priority service tier 单价；
//   - 图片 token 计费（input/output_cost_per_image_token）。
//
// 这些分支未落地前，命中它们的请求会按基础单价计费，即**少算**而不会多算。
func ComputeCost(input CostInput) (Cost, error) {
	price, err := ParsePriceData(input.PriceData)
	if err != nil {
		return Cost{}, err
	}

	inputBucket := decimal.Zero
	outputBucket := decimal.Zero
	creation5mBucket := decimal.Zero
	creation1hBucket := decimal.Zero
	readBucket := decimal.Zero

	// 按次计费价格并入 input 桶（TS 同款）。
	if price.InputCostPerRequest != nil && isUsableRate(*price.InputCostPerRequest) {
		inputBucket = inputBucket.Add(decimal.NewFromFloat(*price.InputCostPerRequest))
	}

	cacheCreation5mRate := price.CacheCreationInputTokenCost
	if cacheCreation5mRate == nil && price.InputCostPerToken != nil {
		derived := *price.InputCostPerToken * 1.25
		cacheCreation5mRate = &derived
	}
	cacheCreation1hRate := price.CacheCreationInputTokenCostAbove1hr
	if cacheCreation1hRate == nil && price.InputCostPerToken != nil {
		derived := *price.InputCostPerToken * 2
		cacheCreation1hRate = &derived
	}
	if cacheCreation1hRate == nil {
		cacheCreation1hRate = cacheCreation5mRate
	}
	cacheReadRate := price.CacheReadInputTokenCost
	if cacheReadRate == nil {
		switch {
		case price.InputCostPerToken != nil:
			derived := *price.InputCostPerToken * 0.1
			cacheReadRate = &derived
		case price.OutputCostPerToken != nil:
			derived := *price.OutputCostPerToken * 0.1
			cacheReadRate = &derived
		}
	}

	cache5mTokens, cache1hTokens := deriveCacheCreationTokens(input.Usage)

	inputBucket = inputBucket.Add(multiplyCost(input.Usage.InputTokens, price.InputCostPerToken))
	outputBucket = outputBucket.Add(multiplyCost(input.Usage.OutputTokens, price.OutputCostPerToken))
	creation5mBucket = creation5mBucket.Add(multiplyCost(cache5mTokens, cacheCreation5mRate))
	creation1hBucket = creation1hBucket.Add(multiplyCost(cache1hTokens, cacheCreation1hRate))
	readBucket = readBucket.Add(multiplyCost(input.Usage.CacheReadInputTokens, cacheReadRate))

	creationBucket := creation5mBucket.Add(creation1hBucket)
	baseTotal := inputBucket.Add(outputBucket).Add(creationBucket).Add(readBucket)

	providerMultiplier := multiplierOrOne(input.ProviderMultiplier)
	groupMultiplier := multiplierOrOne(input.GroupMultiplier)
	total := baseTotal.
		Mul(decimal.NewFromFloat(providerMultiplier)).
		Mul(decimal.NewFromFloat(groupMultiplier)).
		Round(store.CostScale)

	breakdown := StoredCostBreakdown{
		Input:              fixedBucket(inputBucket),
		Output:             fixedBucket(outputBucket),
		CacheCreation:      fixedBucket(creationBucket),
		CacheCreation5m:    fixedBucket(creation5mBucket),
		CacheCreation1h:    fixedBucket(creation1hBucket),
		CacheRead:          fixedBucket(readBucket),
		BaseTotal:          fixedBucket(baseTotal),
		ProviderMultiplier: providerMultiplier,
		GroupMultiplier:    groupMultiplier,
		Total:              total.String(),
	}
	payload, err := json.Marshal(breakdown)
	if err != nil {
		return Cost{}, fmt.Errorf("terminal: 成本明细序列化失败: %w", err)
	}
	return Cost{Total: total.String(), Breakdown: payload}, nil
}

// ResolveAndComputeCost 复刻取价 → 计费的入口：先按模型名查价，查不到则返回
// ErrPriceNotFound（调用方据此跳过计费），查到后按基础口径计算。
func ResolveAndComputeCost(
	ctx context.Context,
	writer Writer,
	modelName string,
	input CostInput,
) (Cost, error) {
	if modelName == "" {
		return Cost{}, ErrPriceNotFound
	}
	price, err := writer.FindModelPrice(ctx, modelName)
	if err != nil {
		return Cost{}, err
	}
	if price == nil {
		return Cost{}, ErrPriceNotFound
	}
	input.PriceData = price.PriceData
	return ComputeCost(input)
}

// ParsePriceData 解析 model_prices.price_data。空对象视为「无可用价格」。
func ParsePriceData(raw []byte) (PriceData, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return PriceData{}, ErrPriceDataInvalid
	}
	var price PriceData
	if err := json.Unmarshal(raw, &price); err != nil {
		return PriceData{}, fmt.Errorf("%w: %v", ErrPriceDataInvalid, err)
	}
	return price, nil
}

// deriveCacheCreationTokens 复刻 TS 的 TTL 归档：cache_creation_input_tokens 减去
// 已分列的 5m/1h 之后的差额，按 cache_ttl 归到对应桶（非 "1h" 一律归 5m）。
func deriveCacheCreationTokens(usage Usage) (cache5m, cache1h *int64) {
	cache5m = usage.CacheCreation5mInputTokens
	cache1h = usage.CacheCreation1hInputTokens
	if usage.CacheCreationInputTokens == nil {
		return cache5m, cache1h
	}
	remaining := *usage.CacheCreationInputTokens
	if cache5m != nil {
		remaining -= *cache5m
	}
	if cache1h != nil {
		remaining -= *cache1h
	}
	if remaining <= 0 {
		return cache5m, cache1h
	}
	if usage.CacheTTL == "1h" {
		total := remaining
		if cache1h != nil {
			total += *cache1h
		}
		return cache5m, &total
	}
	total := remaining
	if cache5m != nil {
		total += *cache5m
	}
	return &total, cache1h
}

// multiplyCost 复刻 TS 的 multiplyCost：任一侧缺失即计 0。
func multiplyCost(quantity *int64, unitCost *float64) decimal.Decimal {
	if quantity == nil || unitCost == nil || !isUsableRate(*unitCost) {
		return decimal.Zero
	}
	return decimal.NewFromInt(*quantity).
		Mul(decimal.NewFromFloat(*unitCost)).
		Round(store.CostScale)
}

// fixedBucket 把桶值渲染成 JSON 字符串。与 TS 的 String(Decimal) 同形（去掉多余的
// 尾随零），因此 0.000024 不会写成 0.000024000000000。
func fixedBucket(value decimal.Decimal) string {
	return value.Round(store.CostScale).String()
}

// SanitizeMultiplier 复刻 cost-calculation.ts 的 sanitizeMultiplier：非有限或负数
// 一律回落到 1。
func SanitizeMultiplier(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 1
	}
	return value
}

// multiplierOrOne 把「未提供」解释为 1（而非 0），只对显式给出的值做净化。
func multiplierOrOne(value *float64) float64 {
	if value == nil {
		return 1
	}
	return SanitizeMultiplier(*value)
}

// isUsableRate 对应 TS 的 Number.isFinite 判定：非有限或负数单价不可用。
func isUsableRate(rate float64) bool {
	return !math.IsNaN(rate) && !math.IsInf(rate, 0) && rate >= 0
}
