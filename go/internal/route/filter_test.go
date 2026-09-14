package route

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

type stubSource struct {
	providers []Provider
	byID      map[int64]Provider
	endpoints map[string][]Endpoint
}

func (s *stubSource) Providers(context.Context) ([]Provider, error) { return s.providers, nil }

func (s *stubSource) Provider(_ context.Context, id int64) (*Provider, error) {
	provider, ok := s.byID[id]
	if !ok {
		return nil, errNoSource
	}
	return &provider, nil
}

func (s *stubSource) Endpoints(
	_ context.Context,
	vendorID int64,
	providerType convert.ProviderType,
) ([]Endpoint, error) {
	return s.endpoints[vendorEndpointKey(vendorID, providerType)], nil
}

func vendorEndpointKey(vendorID int64, providerType convert.ProviderType) string {
	return string(rune('0'+vendorID)) + "|" + string(providerType)
}

func intPtr(value int) *int       { return &value }
func i64Ptr(value int64) *int64   { return &value }
func strPtr(value string) *string { return &value }
func boolPtr(value bool) *bool    { return &value }

func mustRaw(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("构造测试数据失败: %v", err)
	}
	return encoded
}

// baseProvider 是「一切默认通过」的供应商，测试按需改字段。
func baseProvider(id int64, providerType convert.ProviderType) Provider {
	return Provider{
		ID:             id,
		Name:           "p" + string(rune('0'+id)),
		ProviderType:   providerType,
		IsEnabled:      true,
		Weight:         1,
		Priority:       intPtr(0),
		CostMultiplier: "1",
	}
}

func reasonOf(t *testing.T, dc DecisionContext, id int64) Reason {
	t.Helper()
	for _, record := range dc.FilteredProviders {
		if record.ID == id {
			return record.Reason
		}
	}
	t.Fatalf("决策上下文里没有供应商 %d 的过滤记录: %+v", id, dc.FilteredProviders)
	return ""
}

func TestApplyFiltersReasonMatrix(t *testing.T) {
	disabled := baseProvider(1, convert.ProviderClaude)
	disabled.IsEnabled = false

	excluded := baseProvider(2, convert.ProviderClaude)

	modelNotAllowed := baseProvider(3, convert.ProviderClaude)
	modelNotAllowed.AllowedModels = mustRaw(t, []any{"claude-3-*"})

	conversionDisabled := baseProvider(4, convert.ProviderCodex)
	conversionDisabled.ProtocolConversionEnabled = boolPtr(false)

	conversionEnabled := baseProvider(5, convert.ProviderCodex)
	conversionEnabled.ProtocolConversionEnabled = boolPtr(true)

	native := baseProvider(6, convert.ProviderClaude)

	source := &stubSource{providers: []Provider{
		disabled, excluded, modelNotAllowed, conversionDisabled, conversionEnabled, native,
	}}
	selector := NewSelector(Options{Source: source})

	result, err := selector.Select(context.Background(), Request{
		Model:      "claude-3-5-sonnet",
		Format:     convert.FormatClaude,
		ExcludeIDs: []int64{2},
	})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}

	checks := map[int64]Reason{
		1: ReasonDisabled,
		2: ReasonExcluded,
		3: ReasonModelNotAllowed,
		4: ReasonProtocolConversionDisabled,
	}
	for id, want := range checks {
		if got := reasonOf(t, result.Context, id); got != want {
			t.Errorf("供应商 %d 的过滤理由 = %q，期望 %q", id, got, want)
		}
	}
	// 5（跨协议 + 开关开启）与 6（同协议）都应存活。
	if result.Context.EnabledProviders != 2 {
		t.Errorf("enabledProviders = %d，期望 2", result.Context.EnabledProviders)
	}
	if result.Context.BeforeHealthCheck != 2 || result.Context.AfterHealthCheck != 2 {
		t.Errorf("健康过滤计数 = %d/%d，期望 2/2",
			result.Context.BeforeHealthCheck, result.Context.AfterHealthCheck)
	}
	if result.Context.TargetType != TargetClaude {
		t.Errorf("targetType = %q，期望 claude", result.Context.TargetType)
	}
}

func TestApplyFiltersFormatMismatchWhenEndpointUnmappable(t *testing.T) {
	// 跨协议 + 开关开启，但端点是带副作用的 count_tokens：跨线一律无映射，应记 format_type_mismatch。
	crossConvertible := baseProvider(1, convert.ProviderCodex)
	crossConvertible.ProtocolConversionEnabled = boolPtr(true)

	source := &stubSource{providers: []Provider{crossConvertible}}
	selector := NewSelector(Options{Source: source})

	result, err := selector.Select(context.Background(), Request{
		Model:    "claude-3-5-sonnet",
		Format:   convert.FormatClaude,
		Endpoint: EndpointContext{Pathname: "/v1/messages/count_tokens"},
	})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if got := reasonOf(t, result.Context, 1); got != ReasonFormatTypeMismatch {
		t.Errorf("过滤理由 = %q，期望 %q", got, ReasonFormatTypeMismatch)
	}
	if result.Provider != nil {
		t.Errorf("不应选出供应商，实际选中 %d", result.Provider.ID)
	}
}

func TestApplyFiltersMultipartImageNotServable(t *testing.T) {
	crossConvertible := baseProvider(1, convert.ProviderCodex)
	crossConvertible.ProtocolConversionEnabled = boolPtr(true)

	selector := NewSelector(Options{Source: &stubSource{providers: []Provider{crossConvertible}}})
	result, err := selector.Select(context.Background(), Request{
		Model:    "claude-3-5-sonnet",
		Format:   convert.FormatClaude,
		Endpoint: EndpointContext{Pathname: "/v1/messages", MultipartImageRequest: true},
	})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if got := reasonOf(t, result.Context, 1); got != ReasonFormatTypeMismatch {
		t.Errorf("multipart 跨协议应被判不可服务，实际理由 %q", got)
	}
}

func TestGroupIsolationStrict(t *testing.T) {
	other := baseProvider(1, convert.ProviderClaude)
	other.GroupTag = strPtr("beta")

	selector := NewSelector(Options{Source: &stubSource{providers: []Provider{other}}})
	result, err := selector.Select(context.Background(), Request{
		Model:  "claude-3-5-sonnet",
		Format: convert.FormatClaude,
		Group:  "alpha",
	})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider != nil {
		t.Fatalf("分组无交集时不应选出供应商")
	}
	if result.Context.TotalProviders != 0 || result.Context.AfterGroupFilter == nil ||
		*result.Context.AfterGroupFilter != 0 {
		t.Errorf("分组隔离留痕不符: %+v", result.Context)
	}
	if !result.Context.GroupFilterApplied {
		t.Errorf("groupFilterApplied 应为真")
	}
}

func TestGroupMatchAllAndDefault(t *testing.T) {
	untagged := baseProvider(1, convert.ProviderClaude)
	tagged := baseProvider(2, convert.ProviderClaude)
	tagged.GroupTag = strPtr("beta,gamma")

	if !checkProviderGroupMatch(untagged.GroupTag, GroupDefault) {
		t.Errorf("无标签供应商应属于 default 组")
	}
	if !checkProviderGroupMatch(tagged.GroupTag, GroupAll) {
		t.Errorf("* 应可访问所有供应商")
	}
	if !checkProviderGroupMatch(tagged.GroupTag, "gamma，alpha") {
		t.Errorf("中文逗号分隔的多分组应命中 gamma")
	}
	if checkProviderGroupMatch(tagged.GroupTag, "alpha") {
		t.Errorf("无交集不应命中")
	}
}

func TestProviderSupportsModelRules(t *testing.T) {
	provider := baseProvider(1, convert.ProviderClaude)

	if !providerSupportsModel(provider, "any-model") {
		t.Errorf("未设允许集应放行")
	}

	provider.AllowedModels = mustRaw(t, []any{
		"claude-3-5-sonnet",
		map[string]any{"matchType": "prefix", "pattern": "gpt-"},
		"",
		map[string]any{"matchType": "bogus", "pattern": "x"},
	})
	cases := map[string]bool{
		"claude-3-5-sonnet": true,
		"claude-3-5-haiku":  false,
		"gpt-4o":            true,
		"":                  false,
	}
	for model, want := range cases {
		if got := providerSupportsModel(provider, model); got != want {
			t.Errorf("模型 %q 放行 = %v，期望 %v", model, got, want)
		}
	}

	// 全部规则非法 -> 视为无规则 -> 放行（Node 的归一化语义）。
	provider.AllowedModels = mustRaw(t, []any{"", map[string]any{"matchType": "bogus"}})
	if !providerSupportsModel(provider, "anything") {
		t.Errorf("规则全部非法时应收敛为放行")
	}
}

func TestMatchesPatternGlobFallback(t *testing.T) {
	if !matchesPattern("claude-opus-4-1", "regex", "*-opus-*") {
		t.Errorf("glob 回退应整串匹配 *-opus-*")
	}
	if matchesPattern("bar.foo.baz", "regex", "*.foo") {
		t.Errorf("glob 回退两端补锚点后不应匹配中间子串")
	}
	if !matchesPattern("claude-3-5-sonnet", "regex", "^claude-.*sonnet$") {
		t.Errorf("合法正则应保持子串/锚点语义")
	}
}

func TestEndpointGateExcludesVendorWithoutEndpoints(t *testing.T) {
	vendorProvider := baseProvider(1, convert.ProviderClaude)
	vendorProvider.ProviderVendorID = i64Ptr(7)

	plain := baseProvider(2, convert.ProviderClaude)

	source := &stubSource{providers: []Provider{vendorProvider, plain}, endpoints: map[string][]Endpoint{}}
	selector := NewSelector(Options{Source: source, EndpointGate: true})

	result, err := selector.Select(context.Background(), Request{Model: "m", Format: convert.FormatClaude})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if got := reasonOf(t, result.Context, 1); got != ReasonEndpointUnavailable {
		t.Errorf("过滤理由 = %q，期望 %q", got, ReasonEndpointUnavailable)
	}
	if result.Provider == nil || result.Provider.ID != 2 {
		t.Fatalf("应选中无厂信息的供应商 2")
	}
}

func TestEndpointGateEnabledKeepsVendorProvider(t *testing.T) {
	vendorProvider := baseProvider(1, convert.ProviderClaude)
	vendorProvider.ProviderVendorID = i64Ptr(7)

	source := &stubSource{
		providers: []Provider{vendorProvider},
		endpoints: map[string][]Endpoint{
			vendorEndpointKey(7, convert.ProviderClaude): {{ID: 100, VendorID: 7, IsEnabled: true}},
		},
	}
	selector := NewSelector(Options{Source: source, EndpointGate: true})
	result, err := selector.Select(context.Background(), Request{Model: "m", Format: convert.FormatClaude})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider == nil || result.Provider.ID != 1 {
		t.Fatalf("端点可用时不应排除厂级供应商")
	}
}

// TestModelSupportedProvidersCountsIndependentlyOfFilterOrder 钉住「模型覆盖计数」的口径。
//
// 为何要有这个计数：`503 no_available_providers` 的响应体不可改（Node 契约），而它的两种
// 成因（模型名没人支持 / 供应商真不可用）必须能被分开。逐家短路的过滤理由数不出来这个数
// ——格式不兼容的家根本走不到模型检查。故它必须**独立于过滤顺序**地用同一个
// providerSupportsModel 数一遍。
func TestModelSupportedProvidersCountsIndependentlyOfFilterOrder(t *testing.T) {
	const requested = "dsf4"

	// 1：跨协议且未开转换。codex→claude 本可转换，故理由码是专门的那个
	//    （而不是 format_type_mismatch）。它没设允许集 → 按 Node 语义「空规则集 = 全放行」，
	//    因此**算作支持**（这一点很关键：计数为 0 才真正等于「没人声明支持」）。
	formatFiltered := baseProvider(1, convert.ProviderCodex)
	formatFiltered.ProtocolConversionEnabled = boolPtr(false)

	// 2：停用 → 会被**启用态**理由先拦下；它的允许集里有 dsf4，故应被算作「支持」。
	disabled := baseProvider(2, convert.ProviderClaude)
	disabled.IsEnabled = false
	disabled.AllowedModels = mustRaw(t, []any{"dsf4"})

	// 3：正常启用且允许集含 dsf4 → 支持。
	supporter := baseProvider(3, convert.ProviderClaude)
	supporter.AllowedModels = mustRaw(t, []any{"dsf4"})

	// 4：启用但允许集只有别的模型（字符串规则按 exact 归一）→ 不支持。
	other := baseProvider(4, convert.ProviderClaude)
	other.AllowedModels = mustRaw(t, []any{"claude-3-*"})

	source := &stubSource{providers: []Provider{formatFiltered, disabled, supporter, other}}
	selector := NewSelector(Options{Source: source})

	result, err := selector.Select(context.Background(), Request{
		Model:  requested,
		Format: convert.FormatClaude,
	})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}

	if result.Context.TotalProviders != 4 {
		t.Errorf("totalProviders = %d，期望 4", result.Context.TotalProviders)
	}
	// 1、2、3 声明支持（1 无允许集 = 全放行；2/3 的 exact 规则命中 dsf4）；4 不声明。
	// 与「谁先被什么理由拦下」无关：这正是逐家短路口径数不出来的那个数。
	if got := result.Context.ModelSupportedProviders; got != 3 {
		t.Errorf("modelSupportedProviders = %d，期望 3（无允许集的 1 + 停用的 2 + 启用的 3）", got)
	}
	// 过滤理由仍按短路口径记录（本次首命中理由）。
	if got := reasonOf(t, result.Context, 1); got != ReasonProtocolConversionDisabled {
		t.Errorf("供应商 1 的理由 = %q，期望 %q", got, ReasonProtocolConversionDisabled)
	}
	if got := reasonOf(t, result.Context, 2); got != ReasonDisabled {
		t.Errorf("供应商 2 的理由 = %q，期望 %q", got, ReasonDisabled)
	}
	if got := reasonOf(t, result.Context, 4); got != ReasonModelNotAllowed {
		t.Errorf("供应商 4 的理由 = %q，期望 %q", got, ReasonModelNotAllowed)
	}
}

// TestModelSupportedProvidersZeroWhenNobodyDeclaresIt 钉住用户实报的那种形态：
// 模型名不认识 → 覆盖为 0（这是日志能一眼认出「模型没人支持」的唯一依据）。
func TestModelSupportedProvidersZeroWhenNobodyDeclaresIt(t *testing.T) {
	first := baseProvider(1, convert.ProviderClaude)
	first.AllowedModels = mustRaw(t, []any{"deepseek-v4-flash"})
	second := baseProvider(2, convert.ProviderClaude)
	second.AllowedModels = mustRaw(t, []any{"claude-3-*"})

	source := &stubSource{providers: []Provider{first, second}}
	selector := NewSelector(Options{Source: source})

	result, err := selector.Select(context.Background(), Request{
		Model:  "dsf4",
		Format: convert.FormatClaude,
	})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	if result.Provider != nil {
		t.Fatalf("dsf4 不该被任何供应商接受，实际选中了 %q", result.Provider.Name)
	}
	if got := result.Context.ModelSupportedProviders; got != 0 {
		t.Errorf("modelSupportedProviders = %d，期望 0", got)
	}
}

// TestModelCoverageStaysOutOfDecisionContextJSON 是**契约钉子**：
// decisionContext 会落进 provider_chain 并参与黄金样本的键集精确断言，
// 诊断计数只能走 json:"-"，任何把它序列化出去的改动都会破坏对拍。
func TestModelCoverageStaysOutOfDecisionContextJSON(t *testing.T) {
	encoded, err := json.Marshal(DecisionContext{
		RequestedModel:          "dsf4",
		ModelSupportedProviders: 3,
		FilteredProviders:       []Filtered{},
	})
	if err != nil {
		t.Fatalf("序列化决策上下文失败: %v", err)
	}
	if strings.Contains(string(encoded), "modelSupportedProviders") {
		t.Fatalf("诊断计数不得进落链 JSON（会破坏黄金样本键集）：%s", encoded)
	}
	if strings.Contains(string(encoded), "ModelSupportedProviders") {
		t.Fatalf("诊断计数不得进落链 JSON（无 tag 的导出字段会被序列化）：%s", encoded)
	}
}
