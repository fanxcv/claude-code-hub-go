package terminal

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/shopspring/decimal"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 黄金样本行的成本口径：input_tokens=1、output_tokens=1，倍率均为 1，
// cost_usd=0.000024 → 说明单价为 input 0.000004、output 0.00002。
// 该用例把成本公式钉在真实行的数值上。
func TestComputeCostMatchesGoldenRowShape(t *testing.T) {
	price := []byte(`{"input_cost_per_token":0.000004,"output_cost_per_token":0.00002}`)
	cost, err := ComputeCost(CostInput{
		Usage:              Usage{InputTokens: int64Ptr(1), OutputTokens: int64Ptr(1)},
		PriceData:          price,
		ProviderMultiplier: floatPtr(1),
		GroupMultiplier:    floatPtr(1),
	})
	if err != nil {
		t.Fatalf("计算成本失败: %v", err)
	}
	if got := normalizedCost(t, cost.Total); got != "0.000024" {
		t.Fatalf("total = %s, want 0.000024", got)
	}

	var breakdown StoredCostBreakdown
	if err := json.Unmarshal(cost.Breakdown, &breakdown); err != nil {
		t.Fatalf("成本明细不是合法 json: %v", err)
	}
	if breakdown.Input != "0.000004" {
		t.Fatalf("input 桶 = %s, want 0.000004", breakdown.Input)
	}
	if breakdown.Output != "0.00002" {
		t.Fatalf("output 桶 = %s, want 0.00002", breakdown.Output)
	}
	if breakdown.BaseTotal != "0.000024" {
		t.Fatalf("base_total = %s, want 0.000024", breakdown.BaseTotal)
	}
	if breakdown.ProviderMultiplier != 1 || breakdown.GroupMultiplier != 1 {
		t.Fatalf("倍率应为 1/1，得到 %v/%v", breakdown.ProviderMultiplier, breakdown.GroupMultiplier)
	}
}

// total = base_total × provider 倍率 × group 倍率（成本明细的自洽约束）。
func TestComputeCostAppliesBothMultipliers(t *testing.T) {
	price := []byte(`{"input_cost_per_token":0.001}`)
	cost, err := ComputeCost(CostInput{
		Usage:              Usage{InputTokens: int64Ptr(100)},
		PriceData:          price,
		ProviderMultiplier: floatPtr(2),
		GroupMultiplier:    floatPtr(1.5),
	})
	if err != nil {
		t.Fatalf("计算成本失败: %v", err)
	}
	// 100 × 0.001 = 0.1；×2 ×1.5 = 0.3
	if got := normalizedCost(t, cost.Total); got != "0.3" {
		t.Fatalf("total = %s, want 0.3", got)
	}
	var breakdown StoredCostBreakdown
	if err := json.Unmarshal(cost.Breakdown, &breakdown); err != nil {
		t.Fatalf("成本明细不是合法 json: %v", err)
	}
	if breakdown.BaseTotal != "0.1" {
		t.Fatalf("base_total = %s, want 0.1", breakdown.BaseTotal)
	}
	if breakdown.ProviderMultiplier != 2 || breakdown.GroupMultiplier != 1.5 {
		t.Fatalf("倍率应为 2/1.5，得到 %v/%v", breakdown.ProviderMultiplier, breakdown.GroupMultiplier)
	}
}

// 缓存派生单价：5m 默认 1.25×、1h 默认 2×、读取默认 0.1×（TS 同款回落）。
func TestComputeCostDerivesCacheRates(t *testing.T) {
	price := []byte(`{"input_cost_per_token":0.001}`)
	cost, err := ComputeCost(CostInput{
		Usage: Usage{
			CacheCreation5mInputTokens: int64Ptr(100),
			CacheCreation1hInputTokens: int64Ptr(100),
			CacheReadInputTokens:       int64Ptr(100),
		},
		PriceData:          price,
		ProviderMultiplier: floatPtr(1),
		GroupMultiplier:    floatPtr(1),
	})
	if err != nil {
		t.Fatalf("计算成本失败: %v", err)
	}
	// 100×0.00125 + 100×0.002 + 100×0.0001 = 0.125 + 0.2 + 0.01 = 0.335
	if got := normalizedCost(t, cost.Total); got != "0.335" {
		t.Fatalf("total = %s, want 0.335", got)
	}
}

// 显式单价优先于派生值。
func TestComputeCostPrefersExplicitCacheRates(t *testing.T) {
	price := []byte(`{"input_cost_per_token":0.001,"cache_creation_input_token_cost":0.002,
		"cache_creation_input_token_cost_above_1hr":0.004,"cache_read_input_token_cost":0.0005}`)
	cost, err := ComputeCost(CostInput{
		Usage: Usage{
			CacheCreation5mInputTokens: int64Ptr(10),
			CacheCreation1hInputTokens: int64Ptr(10),
			CacheReadInputTokens:       int64Ptr(10),
		},
		PriceData:          price,
		ProviderMultiplier: floatPtr(1),
		GroupMultiplier:    floatPtr(1),
	})
	if err != nil {
		t.Fatalf("计算成本失败: %v", err)
	}
	// 10×0.002 + 10×0.004 + 10×0.0005 = 0.02 + 0.04 + 0.005 = 0.065
	if got := normalizedCost(t, cost.Total); got != "0.065" {
		t.Fatalf("total = %s, want 0.065", got)
	}
}

// TTL 归档：cache_creation_input_tokens 减去已分列部分，差额按 cache_ttl 归档。
func TestComputeCostDerivesCacheCreationTokensByTTL(t *testing.T) {
	price := []byte(`{"input_cost_per_token":0.001}`)
	base := CostInput{PriceData: price, ProviderMultiplier: floatPtr(1), GroupMultiplier: floatPtr(1)}

	twoMinutes := base
	twoMinutes.Usage = Usage{
		CacheCreationInputTokens:   int64Ptr(300),
		CacheCreation5mInputTokens: int64Ptr(100),
		CacheTTL:                   "5m",
	}
	cost, err := ComputeCost(twoMinutes)
	if err != nil {
		t.Fatalf("计算成本失败: %v", err)
	}
	// 5m 共 300 × 0.00125 = 0.375
	if got := normalizedCost(t, cost.Total); got != "0.375" {
		t.Fatalf("5m 归档 total = %s, want 0.375", got)
	}

	oneHour := base
	oneHour.Usage = Usage{
		CacheCreationInputTokens:   int64Ptr(300),
		CacheCreation5mInputTokens: int64Ptr(100),
		CacheTTL:                   "1h",
	}
	cost, err = ComputeCost(oneHour)
	if err != nil {
		t.Fatalf("计算成本失败: %v", err)
	}
	// 5m 100×0.00125 = 0.125；1h 200×0.002 = 0.4 → 0.525
	if got := normalizedCost(t, cost.Total); got != "0.525" {
		t.Fatalf("1h 归档 total = %s, want 0.525", got)
	}
}

// 按次计费价格并入 input 桶。
func TestComputeCostIncludesPerRequestPrice(t *testing.T) {
	price := []byte(`{"input_cost_per_request":0.01,"input_cost_per_token":0.001}`)
	cost, err := ComputeCost(CostInput{
		Usage:              Usage{InputTokens: int64Ptr(10)},
		PriceData:          price,
		ProviderMultiplier: floatPtr(1),
		GroupMultiplier:    floatPtr(1),
	})
	if err != nil {
		t.Fatalf("计算成本失败: %v", err)
	}
	if got := normalizedCost(t, cost.Total); got != "0.02" {
		t.Fatalf("total = %s, want 0.02", got)
	}
}

// 缺项按 0 计，不报错（与 TS 的 usage 可选字段语义一致）。
func TestComputeCostTreatsMissingUsageAsZero(t *testing.T) {
	price := []byte(`{"input_cost_per_token":0.001}`)
	cost, err := ComputeCost(CostInput{PriceData: price, ProviderMultiplier: floatPtr(1), GroupMultiplier: floatPtr(1)})
	if err != nil {
		t.Fatalf("缺用量不应报错: %v", err)
	}
	if got := normalizedCost(t, cost.Total); got != "0" {
		t.Fatalf("total = %s, want 0", got)
	}
}

// 无单价时各桶为 0，不因缺字段 panic。
func TestComputeCostWithoutAnyRate(t *testing.T) {
	cost, err := ComputeCost(CostInput{
		Usage:              Usage{InputTokens: int64Ptr(10000), OutputTokens: int64Ptr(10000)},
		PriceData:          []byte(`{}`),
		ProviderMultiplier: floatPtr(1),
		GroupMultiplier:    floatPtr(1),
	})
	if err != nil {
		t.Fatalf("空价格对象不应报错: %v", err)
	}
	if got := normalizedCost(t, cost.Total); got != "0" {
		t.Fatalf("total = %s, want 0", got)
	}
}

// 非法价格数据必须报可判别错误，不得静默按 0 计费。
func TestComputeCostRejectsInvalidPriceData(t *testing.T) {
	cases := map[string][]byte{
		"空":      nil,
		"空白":     []byte("  "),
		"非法json": []byte("{"),
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ComputeCost(CostInput{PriceData: payload})
			if !errors.Is(err, ErrPriceDataInvalid) {
				t.Fatalf("应返回 ErrPriceDataInvalid，得到 %v", err)
			}
		})
	}
}

// 倍率边界：NaN/Inf/负数一律回落 1，与 TS 的 sanitizeMultiplier 一致。
func TestComputeCostSanitizesMultipliers(t *testing.T) {
	price := []byte(`{"input_cost_per_token":0.001}`)
	cases := []struct {
		name      string
		provider  float64
		group     float64
		wantTotal string
	}{
		{name: "NaN 回落 1", provider: math.NaN(), group: math.NaN(), wantTotal: "0.1"},
		{name: "+Inf 回落 1", provider: math.Inf(1), group: 1, wantTotal: "0.1"},
		{name: "-Inf 回落 1", provider: math.Inf(-1), group: 1, wantTotal: "0.1"},
		{name: "负数回落 1", provider: -3, group: 1, wantTotal: "0.1"},
		{name: "零是合法倍率", provider: 0, group: 1, wantTotal: "0"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			cost, err := ComputeCost(CostInput{
				Usage:              Usage{InputTokens: int64Ptr(100)},
				PriceData:          price,
				ProviderMultiplier: floatPtr(testCase.provider),
				GroupMultiplier:    floatPtr(testCase.group),
			})
			if err != nil {
				t.Fatalf("计算成本失败: %v", err)
			}
			if got := normalizedCost(t, cost.Total); got != testCase.wantTotal {
				t.Fatalf("total = %s, want %s", got, testCase.wantTotal)
			}
		})
	}
}

// 非有限单价按不可用处理（TS 的 Number.isFinite 判定）。
func TestComputeCostRejectsNonFiniteRates(t *testing.T) {
	price := []byte(`{"input_cost_per_token":"NaN"}`)
	if _, err := ComputeCost(CostInput{PriceData: price}); !errors.Is(err, ErrPriceDataInvalid) {
		t.Fatalf("字符串单价应为非法价格数据，得到 %v", err)
	}
}

// 超长小数与极大 token 数不得丢精度或溢出：定点保留 15 位后与原值一致。
func TestComputeCostKeepsFixedPointPrecision(t *testing.T) {
	price := []byte(`{"input_cost_per_token":0.0000000000000001}`)
	cost, err := ComputeCost(CostInput{
		Usage:              Usage{InputTokens: int64Ptr(1)},
		PriceData:          price,
		ProviderMultiplier: floatPtr(1),
		GroupMultiplier:    floatPtr(1),
	})
	if err != nil {
		t.Fatalf("计算成本失败: %v", err)
	}
	// 1e-16 超出 15 位定点精度，四舍五入为 0（与 TS 的 toDecimalPlaces(15) 一致）。
	if got := normalizedCost(t, cost.Total); got != "0" {
		t.Fatalf("total = %s, want 0", got)
	}

	bigPrice := []byte(`{"input_cost_per_token":0.000001}`)
	cost, err = ComputeCost(CostInput{
		Usage:              Usage{InputTokens: int64Ptr(1 << 40)},
		PriceData:          bigPrice,
		ProviderMultiplier: floatPtr(1),
		GroupMultiplier:    floatPtr(1),
	})
	if err != nil {
		t.Fatalf("计算成本失败: %v", err)
	}
	expected := decimal.NewFromInt(1 << 40).Mul(decimal.NewFromFloat(0.000001)).String()
	if got := normalizedCost(t, cost.Total); got != expected {
		t.Fatalf("total = %s, want %s", got, expected)
	}
}

// 成本明细的 json 字段名必须与 TS 的 StoredCostBreakdown 一致，否则前端读不到。
func TestStoredCostBreakdownFieldNames(t *testing.T) {
	cost, err := ComputeCost(CostInput{
		Usage:              Usage{InputTokens: int64Ptr(1)},
		PriceData:          []byte(`{"input_cost_per_token":0.001}`),
		ProviderMultiplier: floatPtr(1),
		GroupMultiplier:    floatPtr(1),
	})
	if err != nil {
		t.Fatalf("计算成本失败: %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(cost.Breakdown, &payload); err != nil {
		t.Fatalf("成本明细不是合法 json: %v", err)
	}
	for _, field := range []string{
		"input", "output", "cache_creation", "cache_creation_5m", "cache_creation_1h",
		"cache_read", "base_total", "provider_multiplier", "group_multiplier", "total",
	} {
		if _, ok := payload[field]; !ok {
			t.Fatalf("成本明细缺少字段 %s", field)
		}
	}
}

// 写库的 cost_usd 必须是 store 接受的字面量（否则 store 会静默跳过整次成本写入）。
func TestComputeCostTotalIsStorableLiteral(t *testing.T) {
	cost, err := ComputeCost(CostInput{
		Usage:              Usage{InputTokens: int64Ptr(7), OutputTokens: int64Ptr(13)},
		PriceData:          []byte(`{"input_cost_per_token":0.000003,"output_cost_per_token":0.000015}`),
		ProviderMultiplier: floatPtr(1.25),
		GroupMultiplier:    floatPtr(0.9),
	})
	if err != nil {
		t.Fatalf("计算成本失败: %v", err)
	}
	if _, ok := store.FormatCostForStorage(cost.Total); !ok {
		t.Fatalf("total %q 不是 store 可接受的成本字面量", cost.Total)
	}
}

func TestSanitizeMultiplierDefaultsToOne(t *testing.T) {
	if got := SanitizeMultiplier(2.5); got != 2.5 {
		t.Fatalf("合法倍率被改写为 %v", got)
	}
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -0.1} {
		if got := SanitizeMultiplier(value); got != 1 {
			t.Fatalf("非法倍率 %v 应回落 1，得到 %v", value, got)
		}
	}
}

// normalizedCost 去掉定点尾零，便于与 TS 的短写形（"0.000024"）比较。
func normalizedCost(t *testing.T, value string) string {
	t.Helper()
	parsed, err := decimal.NewFromString(value)
	if err != nil {
		t.Fatalf("total %q 不是合法十进制: %v", value, err)
	}
	trimmed := strings.TrimRight(strings.TrimRight(parsed.String(), "0"), ".")
	if trimmed == "" || trimmed == "-" {
		return "0"
	}
	return trimmed
}
