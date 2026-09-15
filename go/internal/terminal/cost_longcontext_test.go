package terminal

import "testing"

// 档位阈值按「价格表里有没有 272k 字段 / model_family」判定，
// 而不是按 max_context_tokens 或模型名——这条判据决定了 272k 与 200k 谁生效。
func TestResolveLongContextThresholdPicksTierByPriceShape(t *testing.T) {
	cases := []struct {
		name  string
		price string
		want  int64
	}{
		{"272k 单价字段存在", `{"input_cost_per_token_above_272k_tokens":0.000006}`, 272000},
		{"272k 的 1h 缓存字段存在", `{"cache_creation_input_token_cost_above_1hr_above_272k_tokens":0.00001}`, 272000},
		{"model_family 为 gpt", `{"model_family":"gpt"}`, 272000},
		{"model_family 为 gpt-pro", `{"model_family":"gpt-pro"}`, 272000},
		{"只有 200k 字段", `{"input_cost_per_token_above_200k_tokens":0.000006}`, 200000},
		{"无分层字段", `{"input_cost_per_token":0.000003}`, 200000},
		{"model_family 为 gpt 之外的族", `{"model_family":"gemini"}`, 200000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			price, err := ParsePriceData([]byte(tc.price))
			if err != nil {
				t.Fatalf("价格解析失败: %v", err)
			}
			if got := ResolveLongContextThreshold(price); got != tc.want {
				t.Fatalf("阈值 = %d, want %d", got, tc.want)
			}
		})
	}
}

// threshold_tokens 不可用（缺失 / 0 / 负数 / NaN）时整块分层定价视为不存在——
// 它**不**回落到档位阈值，否则「阈值写 0」会退化成「对一切请求按分层价计费」。
func TestResolveLongContextPricingIgnoresBlockWithoutUsableThreshold(t *testing.T) {
	cases := []struct {
		name  string
		price string
	}{
		{"未给 threshold_tokens", `{"input_cost_per_token":0.000003,"long_context_pricing":{"input_multiplier":2}}`},
		{"threshold_tokens 为 0", `{"input_cost_per_token":0.000003,"long_context_pricing":{"threshold_tokens":0,"input_multiplier":2}}`},
		{"threshold_tokens 为负", `{"input_cost_per_token":0.000003,"long_context_pricing":{"threshold_tokens":-1,"input_multiplier":2}}`},
		{"无 base 单价又无显式单价", `{"long_context_pricing":{"threshold_tokens":200000,"input_multiplier":2}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			price, err := ParsePriceData([]byte(tc.price))
			if err != nil {
				t.Fatalf("价格解析失败: %v", err)
			}
			if got := ResolveLongContextPricing(price); got != nil {
				t.Fatalf("分层定价应为 nil，得到 %+v", got)
			}
		})
	}
}

// 分层单价三级来源：显式单价 → base 单价 × 倍率（显式倍率优先，其次由显式单价反推）。
// 反推出的 output 倍率同时供缓存桶沿用（Node 同款：缓存倍率缺失时回落 input 倍率）。
func TestResolveLongContextPricingDerivesRatesFromBaseAndMultipliers(t *testing.T) {
	price, err := ParsePriceData([]byte(`{
		"input_cost_per_token":0.000003,
		"output_cost_per_token":0.000002,
		"long_context_pricing":{
			"threshold_tokens":200000,
			"input_multiplier":2,
			"output_cost_per_token":0.00001,
			"scope":"session"
		}}`))
	if err != nil {
		t.Fatalf("价格解析失败: %v", err)
	}
	resolved := ResolveLongContextPricing(price)
	if resolved == nil {
		t.Fatal("分层定价不应为 nil")
	}
	if resolved.ThresholdTokens != 200000 || resolved.Scope != "session" {
		t.Fatalf("阈值/作用域 = %d/%s, want 200000/session", resolved.ThresholdTokens, resolved.Scope)
	}
	assertFloat(t, "input 分层单价", resolved.InputCostPerToken, 0.000006)
	// 显式 output 单价 0.00001 / base output 0.000002 = 倍率 5。
	assertFloat(t, "output 分层单价", resolved.OutputCostPerToken, 0.00001)
	// 缓存桶没给倍率，沿用 input 倍率 2：5m 基价 = 0.000003 x 1.25 = 0.00000375；
	// 1h 基价 = 0.000003 x 2 = 0.000006；读缓存基价 = 0.000003 x 0.1 = 0.0000003。
	assertFloat(t, "cache_creation 分层单价", resolved.CacheCreationInputTokenCost, 0.0000075)
	assertFloat(t, "cache_creation 1h 分层单价", resolved.CacheCreationInputTokenCostAbove1hr, 0.000012)
	assertFloat(t, "cache_read 分层单价", resolved.CacheReadInputTokenCost, 0.0000006)
}

// 显式分层单价优先于 base×倍率；scope 只认 "session"。
func TestResolveLongContextPricingPrefersExplicitCostAndNormalizesScope(t *testing.T) {
	price, err := ParsePriceData([]byte(`{
		"input_cost_per_token":0.000003,
		"long_context_pricing":{
			"threshold_tokens":200000,
			"input_cost_per_token":0.000009,
			"input_multiplier":99,
			"scope":"request"
		}}`))
	if err != nil {
		t.Fatalf("价格解析失败: %v", err)
	}
	resolved := ResolveLongContextPricing(price)
	if resolved == nil {
		t.Fatal("分层定价不应为 nil")
	}
	assertFloat(t, "input 分层单价", resolved.InputCostPerToken, 0.000009)

	// 未知 scope 一律归 "request"（Node 的 getLongContextScope）。
	price, err = ParsePriceData([]byte(`{
		"input_cost_per_token":0.000003,
		"long_context_pricing":{"threshold_tokens":200000,"input_multiplier":2,"scope":"global"}}`))
	if err != nil {
		t.Fatalf("价格解析失败: %v", err)
	}
	if got := ResolveLongContextPricing(price); got == nil || got.Scope != "request" {
		t.Fatalf("未知作用域应归 request，得到 %+v", got)
	}
}

// 判定用的输入上下文总量 = 新鲜输入 + 缓存创建 + 缓存读取 + 输入图片 token；
// 缓存创建优先取聚合值，仅在聚合值缺失时才用 5m/1h 之和。
func TestGetLongContextTriggerInputTokensSumsAllInputBuckets(t *testing.T) {
	usage := Usage{
		InputTokens:                int64Ptr(100),
		CacheReadInputTokens:       int64Ptr(50),
		CacheCreationInputTokens:   int64Ptr(30),
		InputImageTokens:           int64Ptr(20),
		CacheCreation5mInputTokens: int64Ptr(7),
		CacheCreation1hInputTokens: int64Ptr(9),
	}
	if got := GetLongContextTriggerInputTokens(usage, int64Ptr(7), int64Ptr(9)); got != 200 {
		t.Fatalf("有聚合缓存创建值时 = %d, want 200", got)
	}

	usage.CacheCreationInputTokens = nil
	if got := GetLongContextTriggerInputTokens(usage, int64Ptr(7), int64Ptr(9)); got != 186 {
		t.Fatalf("无聚合缓存创建值时 = %d, want 186", got)
	}
}

// 命中判定是**严格大于**：等于阈值不算命中（Node 的 observed <= threshold 返回 nil）。
func TestMatchLongContextPricingRequiresStrictlyAboveThreshold(t *testing.T) {
	priceData := []byte(`{
		"input_cost_per_token":0.000003,
		"long_context_pricing":{"threshold_tokens":100,"input_multiplier":2}}`)
	price, err := ParsePriceData(priceData)
	if err != nil {
		t.Fatalf("价格解析失败: %v", err)
	}

	if match := MatchLongContextPricing(Usage{InputTokens: int64Ptr(100)}, price); match != nil {
		t.Fatalf("等于阈值不应命中，得到 %+v", match)
	}
	match := MatchLongContextPricing(Usage{InputTokens: int64Ptr(101)}, price)
	if match == nil {
		t.Fatal("超过阈值应命中")
	}
	if match.ThresholdTokens != 100 || match.ObservedInputTokens != 101 || match.Scope != "request" {
		t.Fatalf("命中信息 = %+v", match)
	}
	assertFloat(t, "命中单价", match.Pricing.InputCostPerToken, 0.000006)
}

func assertFloat(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s = nil, want %v", name, want)
	}
	if diff := *got - want; diff > 1e-15 || diff < -1e-15 {
		t.Fatalf("%s = %v, want %v", name, *got, want)
	}
}
