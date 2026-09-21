package route

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住 `Simulate` 与前端契约（src/types/dispatch-simulator.ts）以及 Node
// `simulateDispatchDecisionTree` 的九步语义。断言全部是「可观察的形状与文案」，
// 不依赖任何外部读数（数据源以回调注入）。

func simulateTestProvider(id int64, name string, mutate func(*SimulateProvider)) SimulateProvider {
	priority := 0
	provider := SimulateProvider{
		Provider: Provider{
			ID:           id,
			Name:         name,
			ProviderType: convert.ProviderClaude,
			IsEnabled:    true,
			Weight:       1,
			Priority:     &priority,
			GroupTag:     stringPtr(GroupDefault),
		},
	}
	if mutate != nil {
		mutate(&provider)
	}
	return provider
}

func TestSimulateStepsFollowNodeOrder(t *testing.T) {
	providers := []SimulateProvider{
		simulateTestProvider(1, "keep", nil),
		simulateTestProvider(2, "group-mismatch", func(p *SimulateProvider) {
			p.GroupTag = stringPtr("other-group")
		}),
		simulateTestProvider(3, "format-mismatch", func(p *SimulateProvider) {
			p.ProviderType = convert.ProviderGemini
		}),
		simulateTestProvider(4, "disabled", func(p *SimulateProvider) {
			p.IsEnabled = false
		}),
		simulateTestProvider(5, "outside-window", func(p *SimulateProvider) {
			p.ActiveTimeStart = stringPtr("08:00")
			p.ActiveTimeEnd = stringPtr("09:00")
		}),
		simulateTestProvider(6, "model-not-allowed", func(p *SimulateProvider) {
			p.AllowedModels = json.RawMessage(`["gpt-4o"]`)
		}),
	}

	// 固定在 12:00（目标时区），故 08:00-09:00 的供应商必然在窗口外。
	noon := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	result, err := Simulate(context.Background(), SimulateOptions{
		Format:    convert.FormatClaude,
		ModelName: "claude-sonnet-4-5",
		Now:       noon,
		Providers: providers,
	})
	if err != nil {
		t.Fatalf("Simulate 失败: %v", err)
	}

	wantOrder := []string{
		"groupFilter", "formatCompatibility", "enabledCheck", "activeTime",
		"modelAllowlist", "healthAndLimits", "priorityTiers", "modelRedirect",
		"endpointSummary",
	}
	if len(result.Steps) != len(wantOrder) {
		t.Fatalf("步数应为 %d，实际 %d", len(wantOrder), len(result.Steps))
	}
	for index, want := range wantOrder {
		step := result.Steps[index]
		if step.StepName != want {
			t.Errorf("第 %d 步应为 %s，实际 %s", index+1, want, step.StepName)
		}
		if step.StepIndex != index+1 {
			t.Errorf("%s 的 stepIndex 应为 %d，实际 %d", want, index+1, step.StepIndex)
		}
	}

	if result.TotalProviders != len(providers) {
		t.Errorf("totalProviders 应含禁用态（%d），实际 %d", len(providers), result.TotalProviders)
	}

	// 逐轮淘汰：只留 id=1。索引 0..5 分别是分组/格式/启用/时段/模型/健康限额。
	wantFiltered := map[string]int64{
		"groupFilter":         2,
		"formatCompatibility": 3,
		"enabledCheck":        4,
		"activeTime":          5,
		"modelAllowlist":      6,
	}
	for _, step := range result.Steps {
		if wantID, ok := wantFiltered[step.StepName]; ok {
			if len(step.FilteredOut) != 1 || step.FilteredOut[0].ID != wantID {
				t.Errorf("%s 应淘汰 id=%d，实际 %+v", step.StepName, wantID, step.FilteredOut)
			}
		}
	}

	final := result.Steps[8].Surviving
	if len(final) != 1 || final[0].ID != 1 {
		t.Fatalf("末轮应只剩 id=1，实际 %+v", final)
	}
	if result.FinalCandidateCount != 1 {
		t.Errorf("finalCandidateCount 应为 1，实际 %d", result.FinalCandidateCount)
	}
	if result.SelectedPriority == nil || *result.SelectedPriority != 0 {
		t.Errorf("selectedPriority 应为 0，实际 %v", result.SelectedPriority)
	}
}

func TestSimulateDetailsMatchNodeStrings(t *testing.T) {
	providers := []SimulateProvider{
		simulateTestProvider(1, "claude", nil),
		simulateTestProvider(2, "gemini-type", func(p *SimulateProvider) {
			p.ProviderType = convert.ProviderGemini
		}),
	}
	result, err := Simulate(context.Background(), SimulateOptions{
		Format:    convert.FormatClaude,
		ModelName: "claude-sonnet-4-5",
		Now:       time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC),
		Providers: providers,
	})
	if err != nil {
		t.Fatalf("Simulate 失败: %v", err)
	}

	formatStep := result.Steps[1]
	if len(formatStep.FilteredOut) != 1 {
		t.Fatalf("格式步应淘汰一条，实际 %+v", formatStep.FilteredOut)
	}
	details := formatStep.FilteredOut[0].Details
	if details == nil || *details != "format claude incompatible with gemini" {
		t.Errorf("格式步的 details 文案不符：%v", details)
	}
	if formatStep.FilteredOut[0].EffectivePriority != 0 {
		t.Errorf("淘汰项也应带 effectivePriority（Node 的 buildProviderSnapshot 恒填）")
	}
}

func TestSimulateResourceRequestSkipsModelSteps(t *testing.T) {
	providers := []SimulateProvider{simulateTestProvider(1, "claude", nil)}
	result, err := Simulate(context.Background(), SimulateOptions{
		Format: convert.FormatClaude,
		// 模型名为空 = 资源类请求
		Now:       time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC),
		Providers: providers,
	})
	if err != nil {
		t.Fatalf("Simulate 失败: %v", err)
	}

	allowStep := result.Steps[4]
	if allowStep.Note == nil || *allowStep.Note != "model_filter_skipped_for_resource_request" {
		t.Errorf("模型名为空时模型步应带 skip note，实际 %v", allowStep.Note)
	}
	if len(allowStep.FilteredOut) != 0 {
		t.Errorf("模型名为空时模型步不应淘汰任何供应商")
	}

	redirectStep := result.Steps[7]
	if redirectStep.Note == nil || *redirectStep.Note != "redirect_preview_skipped_for_resource_request" {
		t.Errorf("模型名为空时重定向步应带 skip note，实际 %v", redirectStep.Note)
	}
	if redirectStep.Surviving[0].RedirectedModel != nil {
		t.Errorf("模型名为空时 redirectedModel 应为 null（Node 同）")
	}
	if redirectStep.Surviving[0].Details == nil ||
		*redirectStep.Surviving[0].Details != "no_model_name_provided" {
		t.Errorf("模型名为空时重定向 details 应为 no_model_name_provided，实际 %v",
			redirectStep.Surviving[0].Details)
	}
}

func TestSimulatePriorityTiersAndWeightPercent(t *testing.T) {
	high := 5
	providers := []SimulateProvider{
		simulateTestProvider(1, "p0-a", func(p *SimulateProvider) { p.Weight = 3 }),
		simulateTestProvider(2, "p0-b", func(p *SimulateProvider) { p.Weight = 1 }),
		simulateTestProvider(3, "p5", func(p *SimulateProvider) { p.Priority = &high }),
	}
	result, err := Simulate(context.Background(), SimulateOptions{
		Format: convert.FormatClaude,
		Now:    time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC),
		// 显式给模型名，走完整的重定向预览
		ModelName: "claude-sonnet-4-5",
		Providers: providers,
	})
	if err != nil {
		t.Fatalf("Simulate 失败: %v", err)
	}

	if len(result.PriorityTiers) != 2 {
		t.Fatalf("应有 2 档优先级，实际 %d", len(result.PriorityTiers))
	}
	first := result.PriorityTiers[0]
	if first.Priority != 0 || !first.IsSelected {
		t.Errorf("第 0 档应被选中，实际 priority=%d selected=%v", first.Priority, first.IsSelected)
	}
	if len(first.Providers) != 2 {
		t.Fatalf("第 0 档应有 2 个供应商，实际 %d", len(first.Providers))
	}
	if got := first.Providers[0].WeightPercent; got != 75 {
		t.Errorf("权重占比应为 75，实际 %v", got)
	}
	if got := first.Providers[1].WeightPercent; got != 25 {
		t.Errorf("权重占比应为 25，实际 %v", got)
	}
	if result.PriorityTiers[1].IsSelected {
		t.Errorf("第 5 档不应被选中")
	}
	if result.FinalCandidateCount != 2 {
		t.Errorf("最终候选应为第 0 档的 2 个，实际 %d", result.FinalCandidateCount)
	}
}

func TestSimulateHealthAndLimitsReasons(t *testing.T) {
	vendorID := int64(42)
	providers := []SimulateProvider{
		simulateTestProvider(1, "vendor-circuit", func(p *SimulateProvider) { p.ProviderVendorID = &vendorID }),
		simulateTestProvider(2, "provider-circuit", nil),
		simulateTestProvider(3, "cost-limited", nil),
		simulateTestProvider(4, "total-limited", nil),
		simulateTestProvider(5, "healthy", nil),
	}
	result, err := Simulate(context.Background(), SimulateOptions{
		Format:    convert.FormatClaude,
		Now:       time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC),
		Providers: providers,
		VendorTypeCircuit: func(_ context.Context, p SimulateProvider) bool {
			return p.ID == 1
		},
		ProviderCircuit: func(_ context.Context, p SimulateProvider) (bool, string) {
			if p.ID == 2 {
				return true, "open"
			}
			return false, "closed"
		},
		ProviderCostAllowed: func(_ context.Context, p SimulateProvider) (bool, string) {
			if p.ID == 3 {
				return false, "供应商 5小时消费上限已达到（12.0000/10）"
			}
			return true, ""
		},
		TotalCostAllowed: func(_ context.Context, p SimulateProvider) (bool, string) {
			if p.ID == 4 {
				return false, "供应商 total spending limit reached (100.0000/100)"
			}
			return true, ""
		},
	})
	if err != nil {
		t.Fatalf("Simulate 失败: %v", err)
	}

	step := result.Steps[5]
	if step.StepName != "healthAndLimits" {
		t.Fatalf("第 6 步应是 healthAndLimits，实际 %s", step.StepName)
	}
	if step.OutputCount != 1 || step.Surviving[0].ID != 5 {
		t.Fatalf("只应剩 id=5，实际 %+v", step.Surviving)
	}
	wantReasons := map[int64]string{
		1: "vendor_type_circuit_open",
		2: "provider_circuit_open",
		3: "供应商 5小时消费上限已达到（12.0000/10）",
		4: "供应商 total spending limit limit reached (100.0000/100)",
	}
	// 注：第 4 条的期望文案按引擎透传实现给出的原字符串断言（引擎不加工回调给的 detail）。
	wantReasons[4] = "供应商 total spending limit reached (100.0000/100)"
	for _, snapshot := range step.FilteredOut {
		want, ok := wantReasons[snapshot.ID]
		if !ok {
			t.Errorf("不应淘汰 id=%d", snapshot.ID)
			continue
		}
		if snapshot.Details == nil || *snapshot.Details != want {
			t.Errorf("id=%d 的 details 应为 %q，实际 %v", snapshot.ID, want, snapshot.Details)
		}
	}
}

func TestSimulateModelRedirectRules(t *testing.T) {
	// 数组形式（逐条合法才生效）
	// 注意 Node 的语义：命中规则时返回的是规则里的 target **原值**（整串替换），
	// 不是「把命中的前缀换成 target」——故这里 target 写成一个完整的模型名。
	arrayRules := json.RawMessage(`[{"matchType":"prefix","source":"claude-","target":"claude-actual-model"}]`)
	// 映射形式（等价于 exact 规则）
	mapRules := json.RawMessage(`{"claude-sonnet-4-5":"claude-sonnet-4-5-20250929"}`)

	providers := []SimulateProvider{
		simulateTestProvider(1, "array-rule", func(p *SimulateProvider) { p.ModelRedirects = arrayRules }),
		simulateTestProvider(2, "map-rule", func(p *SimulateProvider) { p.ModelRedirects = mapRules }),
		simulateTestProvider(3, "no-rule", nil),
	}
	result, err := Simulate(context.Background(), SimulateOptions{
		Format:    convert.FormatClaude,
		ModelName: "claude-sonnet-4-5",
		Now:       time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC),
		Providers: providers,
	})
	if err != nil {
		t.Fatalf("Simulate 失败: %v", err)
	}

	redirectStep := result.Steps[7]
	if redirectStep.InputCount != redirectStep.OutputCount {
		t.Errorf("重定向步不过滤（inputCount == outputCount）")
	}
	byID := map[int64]SimulateProviderSnapshot{}
	for _, snapshot := range redirectStep.Surviving {
		byID[snapshot.ID] = snapshot
	}
	if got := byID[1].RedirectedModel; got == nil || *got != "claude-actual-model" {
		t.Errorf("数组规则应命中前缀并改写，实际 %v", got)
	}
	if got := byID[1].Details; got == nil || *got != "redirect_rule_matched" {
		t.Errorf("命中规则时 details 应为 redirect_rule_matched，实际 %v", got)
	}
	if got := byID[2].RedirectedModel; got == nil || *got != "claude-sonnet-4-5-20250929" {
		t.Errorf("映射规则应命中 exact，实际 %v", got)
	}
	if got := byID[3].RedirectedModel; got == nil || *got != "claude-sonnet-4-5" {
		t.Errorf("未命中规则时 redirectedModel 应回原模型名，实际 %v", got)
	}
	if got := byID[3].Details; got == nil || *got != "no_redirect_rule_matched" {
		t.Errorf("未命中规则时 details 应为 no_redirect_rule_matched，实际 %v", got)
	}
}

func TestSimulateJSONShapeMatchesContract(t *testing.T) {
	providers := []SimulateProvider{simulateTestProvider(7, "shape", nil)}
	result, err := Simulate(context.Background(), SimulateOptions{
		Format:    convert.FormatClaude,
		Now:       time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC),
		Providers: providers,
	})
	if err != nil {
		t.Fatalf("Simulate 失败: %v", err)
	}

	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	payload := string(encoded)

	// 契约里的 camelCase 键名（前端按这些键取值）。
	for _, key := range []string{
		`"steps"`, `"priorityTiers"`, `"totalProviders"`, `"finalCandidateCount"`,
		`"selectedPriority"`, `"stepName"`, `"stepIndex"`, `"inputCount"`, `"outputCount"`,
		`"filteredOut"`, `"surviving"`, `"providerType"`, `"effectivePriority"`,
		`"weightPercent"`, `"isSelected"`,
	} {
		if !strings.Contains(payload, key) {
			t.Errorf("响应缺少契约键 %s", key)
		}
	}
	// groupTag 恒出现（Node 恒挂该字段，可 null）。
	if !strings.Contains(payload, `"groupTag"`) {
		t.Errorf("groupTag 应恒出现（可为 null）")
	}
	// 未提供 details 的快照不应出现该键（Node 的 undefined 被 JSON.stringify 丢掉）。
	firstSurviving := result.Steps[0].Surviving[0]
	if firstSurviving.Details != nil {
		t.Errorf("无淘汰原因的快照不该带 details")
	}
}

func TestSimulateEmptyProviders(t *testing.T) {
	result, err := Simulate(context.Background(), SimulateOptions{
		Format: convert.FormatClaude,
		Now:    time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("空集合不该报错: %v", err)
	}
	if result.TotalProviders != 0 || result.FinalCandidateCount != 0 {
		t.Errorf("空集合的计数应为 0")
	}
	if result.SelectedPriority != nil {
		t.Errorf("无候选时 selectedPriority 应为 null")
	}
	if len(result.PriorityTiers) != 0 {
		t.Errorf("无候选时梯队应为空数组，实际 %d", len(result.PriorityTiers))
	}
	if len(result.Steps) != 9 {
		t.Errorf("步数恒为 9，实际 %d", len(result.Steps))
	}
}

func TestActiveNowMatchesNodeScheduleSemantics(t *testing.T) {
	at := func(hour, minute int) time.Time {
		return time.Date(2026, 9, 13, hour, minute, 0, 0, time.UTC)
	}
	cases := []struct {
		name       string
		start, end *string
		now        time.Time
		want       bool
	}{
		{"未配置时段恒活跃", nil, nil, at(3, 0), true},
		{"空串视同未配置", stringPtr(""), stringPtr(""), at(3, 0), true},
		{"起止相同视为永不活跃", stringPtr("09:00"), stringPtr("09:00"), at(9, 0), false},
		{"同日窗口内", stringPtr("08:00"), stringPtr("18:00"), at(12, 0), true},
		{"同日窗口外", stringPtr("08:00"), stringPtr("18:00"), at(20, 0), false},
		{"跨日窗口内（晚）", stringPtr("22:00"), stringPtr("06:00"), at(23, 30), true},
		{"跨日窗口内（早）", stringPtr("22:00"), stringPtr("06:00"), at(5, 0), true},
		{"跨日窗口外", stringPtr("22:00"), stringPtr("06:00"), at(12, 0), false},
		{"非法值 fail-open", stringPtr("abc"), stringPtr("18:00"), at(12, 0), true},
		{"越界值 fail-open", stringPtr("25:00"), stringPtr("18:00"), at(12, 0), true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := ProviderActiveNow(testCase.start, testCase.end, testCase.now); got != testCase.want {
				t.Errorf("ProviderActiveNow(%v, %v, %v) = %v，期望 %v",
					testCase.start, testCase.end, testCase.now, got, testCase.want)
			}
		})
	}
}

func TestToSimulateProvidersUsesRouteProjection(t *testing.T) {
	// 该测试守的是「管理面不复刻字段映射」：store 行经 route.ProviderFromStore 投影后，
	// 选路必需的四个列必须原样可用（缺一个都会让模拟器与真实选路给出不同候选）。
	priority := 3
	vendorID := int64(9)
	row := store.SimulatorProviderRow{
		Provider: store.Provider{
			ID:                        11,
			Name:                      "projection",
			ProviderType:              string(convert.ProviderCodex),
			IsEnabled:                 true,
			Weight:                    7,
			Priority:                  priority,
			GroupTag:                  stringPtr("g1"),
			ProviderVendorID:          &vendorID,
			GroupPriorities:           json.RawMessage(`{"g1": 1}`),
			ProtocolConversionEnabled: true,
			AllowedModels:             json.RawMessage(`["deepseek-v4-flash"]`),
		},
		ActiveTimeStart: stringPtr("08:00"),
		ActiveTimeEnd:   stringPtr("18:00"),
	}
	projected := ProviderFromStore(row.Provider)

	if projected.ID != 11 || projected.Weight != 7 || projected.EffectivePriority() != 3 {
		t.Fatalf("基础列投影不正确：%+v", projected)
	}
	if projected.ProviderVendorID == nil || *projected.ProviderVendorID != 9 {
		t.Errorf("provider_vendor_id 未投影")
	}
	if !projected.ConversionEnabled() {
		t.Errorf("protocol_conversion_enabled 未投影")
	}
	if got := resolveEffectivePriority(projected, "g1", nil); got != 1 {
		t.Errorf("分组优先级覆盖应生效（期望 1，实际 %d）", got)
	}
	if !providerSupportsModel(projected, "deepseek-v4-flash") {
		t.Errorf("模型允许集应放行 exact 命中")
	}
	if providerSupportsModel(projected, "gpt-4o") {
		t.Errorf("模型允许集应拒绝未命中模型")
	}
}
