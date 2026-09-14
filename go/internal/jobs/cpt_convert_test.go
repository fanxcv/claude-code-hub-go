package jobs

import (
	"encoding/json"
	"testing"
)

// 本文件的期望值来自 TS 侧的算法（cpt-convert.ts），逐条手算：
// per_M_tokens 的价格要除以 1e6 变成 per-token；分层/priority 轨道是「基础价 × 轨道 factor」。
// 只要这里的任一断言变了，两端对同一张云端价格表算出的行就会分叉。

func TestConvertCptVariantAppliesDefaultTrackFactor(t *testing.T) {
	variant := CptPricingVariant{
		Provider: "anthropic",
		Official: true,
		Charges: map[string]CptCharge{
			"prompt":     {Price: "3", Unit: chargeUnitPerMTokens},
			"completion": {Price: "15", Unit: chargeUnitPerMTokens},
			// 非 USD 维度必须整列跳过（内部计费是 USD）。
			"cache_read": {Price: "0.3", Unit: chargeUnitPerMTokens, Currency: "CNY"},
			// 负价拒绝。
			"cache_write": {Price: "-1", Unit: chargeUnitPerMTokens},
		},
		Tracks: []CptTrack{{Label: "default", Factor: "2"}},
	}

	node := convertCptVariant(variant)
	if node == nil {
		t.Fatal("应当产出可计费字段")
	}
	if got := node["input_cost_per_token"]; got != 6e-06 {
		t.Fatalf("prompt 应为基础价 3 x factor 2 / 1e6 = 6e-06, 实际 %v", got)
	}
	if got := node["output_cost_per_token"]; got != 3e-05 {
		t.Fatalf("completion 应为 3e-05, 实际 %v", got)
	}
	if _, exists := node["cache_read_input_token_cost"]; exists {
		t.Fatal("非 USD 维度必须跳过")
	}
	if _, exists := node["cache_creation_input_token_cost"]; exists {
		t.Fatal("负价维度必须跳过")
	}
}

func TestConvertCptVariantTierAndPriorityTracks(t *testing.T) {
	threshold := 200000.0
	variant := CptPricingVariant{
		Provider: "anthropic",
		Charges: map[string]CptCharge{
			"prompt":     {Price: "3", Unit: chargeUnitPerMTokens},
			"completion": {Price: "15", Unit: chargeUnitPerMTokens},
		},
		Tracks: []CptTrack{
			{Label: "long-context", Factor: "2", Triggers: []CptTrackTrigger{
				{Kind: "input_tokens_above", Threshold: threshold},
				// 长上下文 beta header 属辅助条件，不影响分层归类。
				{Kind: "header_matches", Header: "anthropic-beta"},
			}},
			{Label: "priority", Factor: "1.5", Triggers: []CptTrackTrigger{
				{Kind: "body_matches", Field: "service_tier", Pattern: ".*priority.*"},
			}},
		},
	}

	node := convertCptVariant(variant)
	if got := node["input_cost_per_token_above_200k_tokens"]; got != 6e-06 {
		t.Fatalf("分层 200K 字段应为 6e-06, 实际 %v", got)
	}
	if got := node["output_cost_per_token_above_200k_tokens"]; got != 3e-05 {
		t.Fatalf("分层 200K 输出字段应为 3e-05, 实际 %v", got)
	}
	// priority 轨道：prompt 与 completion 有映射，cache_read 无映射（基础价也不存在）。
	if got := node["input_cost_per_token_priority"]; got != 4.5e-06 {
		t.Fatalf("priority 字段应为 3 x 1.5 / 1e6 = 4.5e-06, 实际 %v", got)
	}
	if _, exists := node["cache_read_input_token_cost_priority"]; exists {
		t.Fatal("没有 cache_read 基础价时不得写 priority 字段")
	}
}

func TestConvertCptVariantUnsupportedTriggerSuppressesTrackOnly(t *testing.T) {
	threshold := 1234.0
	variant := CptPricingVariant{
		Provider: "openai",
		Charges:  map[string]CptCharge{"prompt": {Price: "1", Unit: chargeUnitPerMTokens}},
		Tracks: []CptTrack{{Label: "weird", Factor: "3", Triggers: []CptTrackTrigger{
			{Kind: "endpoint_matches", Pattern: "/v1/responses"},
		}}},
		// 保留一个已知阈值的轨道以证明「不可识别轨道只影响自己」。
	}
	node := convertCptVariant(variant)
	if got := node["input_cost_per_token"]; got != 1e-06 {
		t.Fatalf("基础价应为 1e-06, 实际 %v", got)
	}
	if _, exists := node["input_cost_per_token_above_200k_tokens"]; exists {
		t.Fatal("不可识别轨道不得产出分层字段")
	}
	_ = threshold
}

func TestConvertCptVariantScalarAndWebSearchCharges(t *testing.T) {
	variant := CptPricingVariant{
		Provider: "openai",
		Charges: map[string]CptCharge{
			"image_input":  {Price: "0.5", Unit: chargeUnitPerImage},
			"image_output": {Price: "2", Unit: chargeUnitPerImage},
			// 带尺寸后缀的变体必须跳过。
			"image_output_1024x1536": {Price: "9", Unit: chargeUnitPerImage},
			"request":                {Price: "0.01", Unit: chargeUnitPerRequest},
			"web_search":             {Price: "10", Unit: chargeUnitPerKCalls},
			"file_search":            {Price: "2.5", Unit: chargeUnitPerKCalls},
			// 缺 unit 的维度不可识别。
			"audio_input": {Price: "1"},
		},
	}
	node := convertCptVariant(variant)
	// 注意：与 any 比较时无类型常量取默认类型，整数 2 是 int 而不是 float64，
	// 必须写成 2.0，否则会因动态类型不同而永假。
	if node["input_cost_per_image"] != 0.5 || node["output_cost_per_image"] != 2.0 {
		t.Fatalf("per_image 标量应原样落库, 实际 %v / %v",
			node["input_cost_per_image"], node["output_cost_per_image"])
	}
	if node["input_cost_per_request"] != 0.01 {
		t.Fatalf("per_request 应落 input_cost_per_request, 实际 %v", node["input_cost_per_request"])
	}
	search, ok := node["search_context_cost_per_query"].(map[string]any)
	if !ok {
		t.Fatalf("per_k_calls 的 web_search 应落 search_context_cost_per_query, 实际 %v",
			node["search_context_cost_per_query"])
	}
	if search["search_context_size_low"] != 0.01 || search["search_context_size_high"] != 0.01 {
		t.Fatalf("每次查询成本应为 10/1000 = 0.01, 实际 %v", search)
	}
	if node["file_search_cost_per_1k_calls"] != 2.5 {
		t.Fatalf("file_search 每千次成本应为 2.5, 实际 %v", node["file_search_cost_per_1k_calls"])
	}
}

func TestConvertCptVariantReturnsNilWhenNothingBillable(t *testing.T) {
	variant := CptPricingVariant{
		Provider: "openai",
		Charges:  map[string]CptCharge{"prompt": {Price: "1", Unit: "per_M_tokens_per_hour"}},
	}
	if node := convertCptVariant(variant); node != nil {
		t.Fatalf("无可识别计费维度时应返回 nil, 实际 %v", node)
	}
}

func TestConvertCptModelEntryAliasesAndMetadata(t *testing.T) {
	entry := CptModelEntry{
		Slug:            "claude-sonnet-4-5",
		ModelName:       "claude-sonnet-4-5",
		Vendor:          "anthropic",
		DisplayName:     "Claude Sonnet 4.5",
		Aliases:         []string{"claude-sonnet-4-5", "", "sonnet-latest"},
		Family:          "claude-4",
		ModelType:       "image_generation",
		KnowledgeCutoff: "2025-01",
		Deprecated:      true,
		MaxInputTokens:  floatPointer(200000),
		MaxOutputTokens: floatPointer(8192),
		Capabilities:    CptCapabilities{"vision": true, "function_calling": true},
		Pricing: []CptPricingVariant{
			{Provider: "anthropic", Official: true, Charges: map[string]CptCharge{
				"prompt": {Price: "3", Unit: chargeUnitPerMTokens},
			}},
			{Provider: "bedrock", Region: stringPointer("us-east-1"), Charges: map[string]CptCharge{
				"prompt": {Price: "4", Unit: chargeUnitPerMTokens},
			}},
		},
	}

	priceData := convertCptModelEntry(entry, map[string]CptProviderInfo{})
	if priceData == nil {
		t.Fatal("应当产出价格行")
	}
	if priceData["mode"] != "image_generation" {
		t.Fatalf("model_type=image_generation 应映射为 image_generation, 实际 %v", priceData["mode"])
	}
	if priceData["display_name"] != "Claude Sonnet 4.5" || priceData["vendor"] != "anthropic" {
		t.Fatalf("display_name/vendor 不符: %v / %v", priceData["display_name"], priceData["vendor"])
	}
	if priceData["official_pricing_provider"] != "anthropic" {
		t.Fatalf("official 变体键应为 anthropic, 实际 %v", priceData["official_pricing_provider"])
	}
	// 顶层价来自默认（第一个 official）变体；区域变体只出现在 pricing 里。
	if priceData["input_cost_per_token"] != 3e-06 {
		t.Fatalf("顶层价应取 official 变体 3e-06, 实际 %v", priceData["input_cost_per_token"])
	}
	providers, _ := priceData["providers"].([]string)
	if len(providers) != 2 || providers[0] != "anthropic" || providers[1] != "bedrock@us-east-1" {
		t.Fatalf("providers 键顺序应为 [anthropic bedrock@us-east-1], 实际 %v", providers)
	}
	pricing, _ := priceData["pricing"].(map[string]any)
	bedrock, _ := pricing["bedrock@us-east-1"].(map[string]any)
	if bedrock == nil || bedrock["input_cost_per_token"] != 4e-06 {
		t.Fatalf("区域变体应保留自己的价格, 实际 %v", pricing["bedrock@us-east-1"])
	}
	// 别名过滤：去掉自身名与空白项。
	aliases, _ := priceData["aliases"].([]string)
	if len(aliases) != 1 || aliases[0] != "sonnet-latest" {
		t.Fatalf("别名应只剩 sonnet-latest, 实际 %v", priceData["aliases"])
	}
	if priceData["max_tokens"] != 8192.0 || priceData["max_input_tokens"] != 200000.0 {
		t.Fatalf("token 上限字段不符: %v / %v", priceData["max_tokens"], priceData["max_input_tokens"])
	}
	if priceData["model_family"] != "claude-4" || priceData["deprecated"] != true {
		t.Fatal("family / deprecated 未按 Node 口径写入")
	}
	if priceData["supports_vision"] != true || priceData["supports_tool_choice"] != true {
		t.Fatal("能力旗标未展开")
	}
	// official / provider_model_id 是 pricing 内部字段，不得出现在顶层。
	if _, exists := priceData["official"]; exists {
		t.Fatal("顶层不得出现 official")
	}
	if _, exists := priceData["provider_model_id"]; exists {
		t.Fatal("顶层不得出现 provider_model_id")
	}
}

func TestConvertCptModelEntryWithoutOfficialVariantDropsInternalFields(t *testing.T) {
	entry := CptModelEntry{
		Slug: "m", ModelName: "m", Vendor: "other",
		Pricing: []CptPricingVariant{{
			Provider:        "openrouter",
			ProviderModelID: "vendor/m",
			Charges:         map[string]CptCharge{"prompt": {Price: "1", Unit: chargeUnitPerMTokens}},
		}},
	}
	priceData := convertCptModelEntry(entry, map[string]CptProviderInfo{})
	if priceData == nil {
		t.Fatal("非 official 变体也应产出行")
	}
	if _, exists := priceData["provider_model_id"]; exists {
		t.Fatal("无 official 变体时顶层不得带 provider_model_id")
	}
	if priceData["official_pricing_provider"] != nil {
		t.Fatalf("无 official 变体时 official_pricing_provider 应为 null, 实际 %v",
			priceData["official_pricing_provider"])
	}
}

func TestConvertCptTableExpandsAliasesWithoutOverwritingCanonical(t *testing.T) {
	table := &CptTable{
		Version: "v1", Currency: "USD", RefreshedAt: "2026-01-01T00:00:00Z",
		Providers: map[string]CptProviderInfo{"anthropic": {Name: "Anthropic", Icon: "anthropic.svg"}},
		Models: []CptModelEntry{
			{
				Slug: "canonical", ModelName: "canonical", Vendor: "anthropic",
				Aliases: []string{"alias-a", "shared-alias"},
				Pricing: []CptPricingVariant{{Provider: "anthropic", Official: true,
					Charges: map[string]CptCharge{"prompt": {Price: "3", Unit: chargeUnitPerMTokens}}}},
			},
			{
				Slug: "second", ModelName: "second", Vendor: "anthropic",
				Aliases: []string{"shared-alias", "alias-b"},
				Pricing: []CptPricingVariant{{Provider: "anthropic", Official: true,
					Charges: map[string]CptCharge{"prompt": {Price: "9", Unit: chargeUnitPerMTokens}}}},
			},
		},
	}

	converted := ConvertCptTable(table)
	for _, name := range []string{"canonical", "second", "alias-a", "alias-b", "shared-alias"} {
		if _, exists := converted.Models[name]; !exists {
			t.Fatalf("模型键 %s 缺失（别名应展开为独立行）", name)
		}
	}
	// 别名先到先得：shared-alias 归第一个模型（价 3e-06），不得被第二个覆盖。
	if got := converted.Models["shared-alias"]["input_cost_per_token"]; got != 3e-06 {
		t.Fatalf("shared-alias 应保持先到者的价格 3e-06, 实际 %v", got)
	}
	if got := converted.Models["alias-b"]["input_cost_per_token"]; got != 9e-06 {
		t.Fatalf("alias-b 应是第二个模型的价格 9e-06, 实际 %v", got)
	}
	// vendors 按 canonical 计数：两个模型都属 anthropic。
	if len(converted.Vendors) != 1 || converted.Vendors[0].ModelCount != 2 {
		t.Fatalf("vendor 汇总应按 canonical 模型计数=2, 实际 %+v", converted.Vendors)
	}
	if converted.Vendors[0].Icon != "anthropic.svg" {
		t.Fatalf("vendor 图标应取 providers 字典, 实际 %q", converted.Vendors[0].Icon)
	}
}

func TestConvertCptTablePrefersOfficialAndNonOtherEntry(t *testing.T) {
	table := &CptTable{
		Version: "v1", Currency: "USD",
		Providers: map[string]CptProviderInfo{},
		Models: []CptModelEntry{
			{
				Slug: "s", ModelName: "dup", Vendor: "other", DisplayName: "lower",
				Pricing: []CptPricingVariant{{Provider: "openrouter",
					Charges: map[string]CptCharge{"prompt": {Price: "5", Unit: chargeUnitPerMTokens}}}},
			},
			{
				Slug: "s", ModelName: "dup", Vendor: "anthropic", DisplayName: "higher",
				Pricing: []CptPricingVariant{{Provider: "anthropic", Official: true,
					Charges: map[string]CptCharge{"prompt": {Price: "1", Unit: chargeUnitPerMTokens}}}},
			},
		},
	}

	converted := ConvertCptTable(table)
	row := converted.Models["dup"]
	if row == nil {
		t.Fatal("去重后应保留一行")
	}
	if row["display_name"] != "higher" {
		t.Fatalf("官方报价的条目应胜出, 实际 %q", row["display_name"])
	}
}

func TestParseCptTableRejectsInvalidRoot(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{"schema 不符", `{"schema":"other/v1","models":[],"providers":{}}`},
		{"缺 models", `{"schema":"cchp.pricing-table/v1","providers":{}}`},
		{"缺 providers", `{"schema":"cchp.pricing-table/v1","models":[]}`},
		{"models 为空", `{"schema":"cchp.pricing-table/v1","models":[],"providers":{}}`},
		{"非法 JSON", `{`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := ParseCptTable([]byte(testCase.input)); err == nil {
				t.Fatalf("应报错: %s", testCase.name)
			}
		})
	}
}

func TestParseCptTableSkipsEntriesMissingRequiredFields(t *testing.T) {
	raw := `{
		"schema": "cchp.pricing-table/v1",
		"version": "v9",
		"currency": "USD",
		"refreshed_at": "2026-01-01T00:00:00Z",
		"providers": {"anthropic": {"name": "Anthropic"}, "bad": {"name": ""}},
		"models": [
			{"slug": "ok", "model_name": "ok", "vendor": "anthropic", "pricing": []},
			{"slug": "no-name", "vendor": "anthropic", "pricing": []},
			{"slug": "no-vendor", "model_name": "x", "pricing": []},
			{"slug": "no-pricing", "model_name": "y", "vendor": "anthropic"},
			{"model_name": "   ", "slug": "blank", "vendor": "anthropic", "pricing": []}
		]
	}`
	table, err := ParseCptTable([]byte(raw))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(table.Models) != 1 || table.Models[0].ModelName != "ok" {
		t.Fatalf("只应保留字段完整的条目, 实际 %+v", table.Models)
	}
	if table.Version != "v9" || table.Currency != "USD" {
		t.Fatalf("顶层字段未解析: %+v", table)
	}
	if _, exists := table.Providers["bad"]; exists {
		t.Fatal("providers 里 name 为空的项应被过滤")
	}
}

func TestRoundPrecisionMatchesJsToPrecision(t *testing.T) {
	cases := []struct {
		input float64
		want  float64
	}{
		{0.1 * 3, 0.3},
		{1.0 / 3.0, 0.333333333333},
		{1e-07, 1e-07},
	}
	for _, testCase := range cases {
		got := roundPrecision(testCase.input)
		if got != testCase.want {
			t.Fatalf("roundPrecision(%v) 应为 %v, 实际 %v", testCase.input, testCase.want, got)
		}
	}
}

func TestPriceDataEqualIgnoresKeyOrderAndNumberText(t *testing.T) {
	left := map[string]any{"a": 1.0, "b": map[string]any{"x": 2.0}}
	right := map[string]any{"b": map[string]any{"x": 2.0}, "a": 1.0}
	if !priceDataEqual(left, right) {
		t.Fatal("键序不同应判为相等")
	}
	// jsonb 读回来的数值可能带不同文本形式，但数值相等即相等。
	var decoded any
	if err := json.Unmarshal([]byte(`{"a":1e0,"b":{"x":2.0000}}`), &decoded); err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if !priceDataEqual(decoded, left) {
		t.Fatal("数值相等应判为相等")
	}
	if priceDataEqual(left, map[string]any{"a": 1.5, "b": map[string]any{"x": 2.0}}) {
		t.Fatal("数值不同必须判为不等")
	}
}

func floatPointer(value float64) *float64 { return &value }
func stringPointer(value string) *string  { return &value }
