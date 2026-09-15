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

// 输入上下文超过 272k 档后，input/output 桶改按 above_272k 单价计；
// 门槛判定看**输入上下文总量**，与 output token 数无关。
func TestComputeCostApplies272kTierRateAboveThreshold(t *testing.T) {
	price := []byte(`{"input_cost_per_token":0.000003,"output_cost_per_token":0.000009,
		"input_cost_per_token_above_272k_tokens":0.000006,
		"output_cost_per_token_above_272k_tokens":0.000018}`)

	// 300000 > 272000：300000×0.000006 + 1000×0.000018 = 1.8 + 0.018
	above := computeCost(t, price, CostInput{
		Usage:              Usage{InputTokens: int64Ptr(300000), OutputTokens: int64Ptr(1000)},
		ProviderMultiplier: floatPtr(1), GroupMultiplier: floatPtr(1),
	})
	if above.Input != "1.8" || above.Output != "0.018" {
		t.Fatalf("超阈应走 272k 档价，input/output = %s/%s, want 1.8/0.018", above.Input, above.Output)
	}

	// 1000 < 272000：回落基础价 1000×0.000003 + 1000×0.000009 = 0.012
	below := computeCost(t, price, CostInput{
		Usage:              Usage{InputTokens: int64Ptr(1000), OutputTokens: int64Ptr(1000)},
		ProviderMultiplier: floatPtr(1), GroupMultiplier: floatPtr(1),
	})
	if below.BaseTotal != "0.012" {
		t.Fatalf("未超阈应走基础价，base_total = %s, want 0.012", below.BaseTotal)
	}
}

// 档位阈值是**严格大于**：等于 200k 仍走基础价（无 272k 字段时阈值是 200k）。
func TestComputeCostTierThresholdIsStrictlyGreater(t *testing.T) {
	price := []byte(`{"input_cost_per_token":0.000003,
		"input_cost_per_token_above_200k_tokens":0.000006}`)
	equal := computeCost(t, price, CostInput{
		Usage:              Usage{InputTokens: int64Ptr(200000)},
		ProviderMultiplier: floatPtr(1), GroupMultiplier: floatPtr(1),
	})
	if equal.Input != "0.6" {
		t.Fatalf("等于阈值应走基础价，input = %s, want 0.6", equal.Input)
	}
	over := computeCost(t, price, CostInput{
		Usage:              Usage{InputTokens: int64Ptr(200001)},
		ProviderMultiplier: floatPtr(1), GroupMultiplier: floatPtr(1),
	})
	if over.Input != "1.200006" {
		t.Fatalf("超阈应走 200k 档价，input = %s, want 1.200006", over.Input)
	}
}

// 显式 long_context_pricing 优先于档位价，且按**它自己的** threshold_tokens 判定；
// 未命中它的请求仍会去比档位阈值。
func TestComputeCostPrefersExplicitLongContextPricingOverTierRates(t *testing.T) {
	price := []byte(`{"input_cost_per_token":0.000003,
		"input_cost_per_token_above_272k_tokens":0.000006,
		"long_context_pricing":{"threshold_tokens":100000,"input_multiplier":4}}`)

	// 300000 > 100000：0.000003×4 = 0.000012 → 300000×0.000012 = 3.6
	matched := computeCost(t, price, CostInput{
		Usage:              Usage{InputTokens: int64Ptr(300000)},
		ProviderMultiplier: floatPtr(1), GroupMultiplier: floatPtr(1),
	})
	if matched.Input != "3.6" {
		t.Fatalf("命中显式分层定价，input = %s, want 3.6", matched.Input)
	}

	// 50000 < 100000 且没到 272k 档：回落基础价 50000×0.000003 = 0.15
	notMatched := computeCost(t, price, CostInput{
		Usage:              Usage{InputTokens: int64Ptr(50000)},
		ProviderMultiplier: floatPtr(1), GroupMultiplier: floatPtr(1),
	})
	if notMatched.Input != "0.15" {
		t.Fatalf("未命中任何分层，input = %s, want 0.15", notMatched.Input)
	}
}

// priority 档单价只在调用方判定为 priority 时生效；未判定时基础价不变。
func TestComputeCostUsesPriorityRatesOnlyWhenFlagged(t *testing.T) {
	price := []byte(`{"input_cost_per_token":0.000003,"input_cost_per_token_priority":0.000009,
		"cache_read_input_token_cost":0.0000006,"cache_read_input_token_cost_priority":0.0000018}`)
	usage := Usage{InputTokens: int64Ptr(1000), CacheReadInputTokens: int64Ptr(1000)}

	standard := computeCost(t, price, CostInput{
		Usage: usage, ProviderMultiplier: floatPtr(1), GroupMultiplier: floatPtr(1),
	})
	if standard.Input != "0.003" || standard.CacheRead != "0.0006" {
		t.Fatalf("非 priority 应走基础价，input/cache_read = %s/%s", standard.Input, standard.CacheRead)
	}

	priority := computeCost(t, price, CostInput{
		Usage: usage, ProviderMultiplier: floatPtr(1), GroupMultiplier: floatPtr(1),
		PriorityServiceTierApplied: true,
	})
	if priority.Input != "0.009" || priority.CacheRead != "0.0018" {
		t.Fatalf("priority 应走 priority 价，input/cache_read = %s/%s", priority.Input, priority.CacheRead)
	}
}

// 图片 token 与文本 token 并列成段：有图片单价就用图片价，没有才回落文本价；
// 两个方向都不并入文本 token 计数。
func TestComputeCostBillsImageTokensAtImageRate(t *testing.T) {
	usage := Usage{
		InputTokens:       int64Ptr(100),
		OutputTokens:      int64Ptr(100),
		InputImageTokens:  int64Ptr(1000),
		OutputImageTokens: int64Ptr(1000),
	}

	withImageRates := computeCost(t, []byte(`{"input_cost_per_token":0.000003,
		"output_cost_per_token":0.000009,"input_cost_per_image_token":0.00003,
		"output_cost_per_image_token":0.00009}`), CostInput{
		Usage: usage, ProviderMultiplier: floatPtr(1), GroupMultiplier: floatPtr(1),
	})
	// input: 100×0.000003 + 1000×0.00003 = 0.0303；output: 100×0.000009 + 1000×0.00009 = 0.0909
	if withImageRates.Input != "0.0303" || withImageRates.Output != "0.0909" {
		t.Fatalf("图片 token 应按图片价单列，input/output = %s/%s, want 0.0303/0.0909",
			withImageRates.Input, withImageRates.Output)
	}

	withoutImageRates := computeCost(t, []byte(`{"input_cost_per_token":0.000003,
		"output_cost_per_token":0.000009}`), CostInput{
		Usage: usage, ProviderMultiplier: floatPtr(1), GroupMultiplier: floatPtr(1),
	})
	// input: (100+1000)×0.000003 = 0.0033；output: (100+1000)×0.000009 = 0.0099
	if withoutImageRates.Input != "0.0033" || withoutImageRates.Output != "0.0099" {
		t.Fatalf("图片价缺省应回落文本价，input/output = %s/%s, want 0.0033/0.0099",
			withoutImageRates.Input, withoutImageRates.Output)
	}
}

// 下列三组用例的期望值逐一取自 Node 侧规格（tests/unit/lib/cost-calculation-long-context.test.ts、
// -priority.test.ts、-image-tokens.test.ts），是这次移植的对拍锚点：它们同时钉住
// 「显式分层定价 > 档位单价 > 基础单价」的取舍顺序、档位阈值判定，以及图片 token 的回落。
func TestComputeCostMatchesNodeLongContextSpec(t *testing.T) {
	cases := []struct {
		name  string
		usage Usage
		price string
		want  string
	}{
		{
			name:  "超过 200k 后 input/output 均按档价",
			usage: Usage{InputTokens: int64Ptr(250000), OutputTokens: int64Ptr(100000)},
			price: `{"model_family":"claude-sonnet","input_cost_per_token":0.000003,
				"input_cost_per_token_above_200k_tokens":0.000006,"output_cost_per_token":0.000015,
				"output_cost_per_token_above_200k_tokens":0.0000225}`,
			want: "3.75",
		},
		{
			name:  "基础缓存创建价缺失时不得用 1h 的 above_272k 价",
			usage: Usage{InputTokens: int64Ptr(250000), CacheCreation1hInputTokens: int64Ptr(1000)},
			price: `{"model_family":"gpt","input_cost_per_token":0.0000025,
				"output_cost_per_token":0.000015,
				"cache_creation_input_token_cost_above_1hr_above_272k_tokens":0.5}`,
			want: "0.63",
		},
		{
			name:  "没有显式档位价时即使超阈也只走基础价",
			usage: Usage{InputTokens: int64Ptr(250001), OutputTokens: int64Ptr(100)},
			price: `{"model_family":"claude-sonnet","input_cost_per_token":0.000003,
				"output_cost_per_token":0.000015}`,
			want: "0.751503",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			breakdown := computeCost(t, []byte(tc.price), CostInput{
				Usage: tc.usage, ProviderMultiplier: floatPtr(1), GroupMultiplier: floatPtr(1),
			})
			assertCostValue(t, breakdown.BaseTotal, tc.want)
		})
	}
}

func TestComputeCostMatchesNodePrioritySpec(t *testing.T) {
	basePrice := `"input_cost_per_token":1,"output_cost_per_token":10,
		"cache_read_input_token_cost":0.1,"input_cost_per_token_priority":2,
		"output_cost_per_token_priority":20,"cache_read_input_token_cost_priority":0.2`
	above272k := `"input_cost_per_token_above_272k_tokens":5,
		"output_cost_per_token_above_272k_tokens":50,
		"cache_read_input_token_cost_above_272k_tokens":0.5`

	cases := []struct {
		name  string
		usage Usage
		price string
		want  string
	}{
		{
			name:  "priority 档启用时三级单价全走 priority 字段",
			usage: Usage{InputTokens: int64Ptr(2), OutputTokens: int64Ptr(3), CacheReadInputTokens: int64Ptr(5)},
			price: `{` + basePrice + `}`,
			want:  "65",
		},
		{
			name:  "priority 字段缺失时回落基础价",
			usage: Usage{InputTokens: int64Ptr(2), OutputTokens: int64Ptr(3), CacheReadInputTokens: int64Ptr(5)},
			price: `{"input_cost_per_token":1,"output_cost_per_token":10,"cache_read_input_token_cost":0.1}`,
			want:  "32.5",
		},
		{
			name:  "priority 档启用且超 272k：档价也取 priority 变体",
			usage: Usage{InputTokens: int64Ptr(272001), OutputTokens: int64Ptr(2), CacheReadInputTokens: int64Ptr(10)},
			price: `{"model_family":"gpt",` + basePrice + `,` + above272k + `,
				"input_cost_per_token_above_272k_tokens_priority":7,
				"output_cost_per_token_above_272k_tokens_priority":70,
				"cache_read_input_token_cost_above_272k_tokens_priority":0.7}`,
			want: "1904154",
		},
		{
			name:  "priority 档价缺失时档价回落普通 272k 变体",
			usage: Usage{InputTokens: int64Ptr(272001), OutputTokens: int64Ptr(2), CacheReadInputTokens: int64Ptr(10)},
			price: `{"model_family":"gpt",` + basePrice + `,` + above272k + `}`,
			want:  "1360110",
		},
		{
			name:  "只需 272k 的 priority 字段存在，阈值即为 272k（不看模型名）",
			usage: Usage{InputTokens: int64Ptr(272001), OutputTokens: int64Ptr(2)},
			price: `{"input_cost_per_token":1,"output_cost_per_token":10,
				"input_cost_per_token_above_272k_tokens_priority":7,
				"output_cost_per_token_above_272k_tokens_priority":70}`,
			want: "1904147",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			breakdown := computeCost(t, []byte(tc.price), CostInput{
				Usage: tc.usage, ProviderMultiplier: floatPtr(1), GroupMultiplier: floatPtr(1),
				PriorityServiceTierApplied: true,
			})
			assertCostValue(t, breakdown.BaseTotal, tc.want)
		})
	}
}

func TestComputeCostMatchesNodeImageTokenSpec(t *testing.T) {
	cases := []struct {
		name  string
		usage Usage
		price string
		want  string
	}{
		{
			name:  "输出图片按图片单价",
			usage: Usage{OutputImageTokens: int64Ptr(2000)},
			price: `{"output_cost_per_token":0.000012,"output_cost_per_image_token":0.00012}`,
			want:  "0.24",
		},
		{
			name:  "无图片单价时输出图片回落文本单价",
			usage: Usage{OutputImageTokens: int64Ptr(2000)},
			price: `{"output_cost_per_token":0.000012}`,
			want:  "0.024",
		},
		{
			name:  "输入图片按图片单价",
			usage: Usage{InputImageTokens: int64Ptr(560)},
			price: `{"input_cost_per_token":0.000002,"input_cost_per_image_token":0.00000196}`,
			want:  "0.0010976",
		},
		{
			name:  "仅图片 token、无文本单价：另一个方向不补零不乘错价",
			usage: Usage{InputImageTokens: int64Ptr(560), OutputImageTokens: int64Ptr(2000)},
			price: `{"input_cost_per_image_token":0.00000196,"output_cost_per_image_token":0.00012}`,
			want:  "0.2410976",
		},
		{
			name:  "图片 token 为 0 不产生费用",
			usage: Usage{OutputImageTokens: int64Ptr(0)},
			price: `{"output_cost_per_image_token":0.00012}`,
			want:  "0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			breakdown := computeCost(t, []byte(tc.price), CostInput{
				Usage: tc.usage, ProviderMultiplier: floatPtr(1), GroupMultiplier: floatPtr(1),
			})
			assertCostValue(t, breakdown.BaseTotal, tc.want)
		})
	}
}

// 倍率作用于整张明细（图片段也在桶内，故自动被乘）。
func TestComputeCostMultipliesImageTokenSegment(t *testing.T) {
	cost, err := ComputeCost(CostInput{
		Usage:              Usage{OutputImageTokens: int64Ptr(2000)},
		PriceData:          []byte(`{"output_cost_per_image_token":0.00012}`),
		ProviderMultiplier: floatPtr(2),
		GroupMultiplier:    floatPtr(1),
	})
	if err != nil {
		t.Fatalf("计算成本失败: %v", err)
	}
	if got := normalizedCost(t, cost.Total); got != "0.48" {
		t.Fatalf("total = %s, want 0.48", got)
	}
}

// computeCost 跑一遍 ComputeCost 并解出明细（本文件内多个用例共用）。
func computeCost(t *testing.T, priceData []byte, input CostInput) StoredCostBreakdown {
	t.Helper()
	input.PriceData = priceData
	cost, err := ComputeCost(input)
	if err != nil {
		t.Fatalf("计算成本失败: %v", err)
	}
	var breakdown StoredCostBreakdown
	if err := json.Unmarshal(cost.Breakdown, &breakdown); err != nil {
		t.Fatalf("成本明细不是合法 json: %v", err)
	}
	return breakdown
}

// assertCostValue 按数值比较成本值：明细是定点字符串（"136011.000000"），
// 末位零不属于语义差异。
func assertCostValue(t *testing.T, got, want string) {
	t.Helper()
	gotValue, err := decimal.NewFromString(got)
	if err != nil {
		t.Fatalf("成本值 %q 不是合法十进制: %v", got, err)
	}
	wantValue, err := decimal.NewFromString(want)
	if err != nil {
		t.Fatalf("期望值 %q 不是合法十进制: %v", want, err)
	}
	if !gotValue.Equal(wantValue) {
		t.Fatalf("成本值 = %s, want %s", gotValue, wantValue)
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
