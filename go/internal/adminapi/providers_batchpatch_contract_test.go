package adminapi

import "github.com/fanxcv/claude-code-hub-go/go/internal/store"

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustNormalize(t *testing.T, draft map[string]any) providerBatchPatch {
	t.Helper()
	patch, err := normalizeProviderBatchPatchDraft(draft)
	if err != nil {
		t.Fatalf("归一化应成功，得到错误: %v", err)
	}
	return patch
}

func TestProviderBatchPatchFieldTableMatchesNodePATCHFIELDS(t *testing.T) {
	// provider-patch-contract.ts:29-92 的 PATCH_FIELDS 共 52 条；表序即语义（首错、changedFields、revision）。
	// 计数来自 provider-patch-contract.ts:29-92 的 PATCH_FIELDS（逐条数出 49 条），
	// 顺序与集合都用「字段集 + 首尾」钉住：改表就必须同时改测试，不会静默漂移。
	if got := len(providerPatchFieldSpecs); got != 49 {
		t.Fatalf("契约字段数应为 49（Node PATCH_FIELDS），实际 %d", got)
	}
	want := []string{"is_enabled", "priority", "weight", "cost_multiplier", "group_tag", "model_redirects", "allowed_models"}
	for index, field := range want {
		if providerPatchFieldSpecs[index].Field != field {
			t.Fatalf("第 %d 个字段应为 %s，实际 %s", index, field, providerPatchFieldSpecs[index].Field)
		}
	}
	if last := providerPatchFieldSpecs[len(providerPatchFieldSpecs)-1].Field; last != "mcp_passthrough_url" {
		t.Fatalf("最后一个字段应为 mcp_passthrough_url，实际 %s", last)
	}
}

func TestProviderBatchPatchThreeStateNormalization(t *testing.T) {
	cases := []struct {
		name      string
		draft     map[string]any
		wantErr   string
		wantField string
	}{
		{name: "缺键即 no_change", draft: map[string]any{}, wantErr: ""},
		{
			name:    "三态互斥",
			draft:   map[string]any{"weight": map[string]any{"set": float64(3), "clear": true}},
			wantErr: "exactly one mode", wantField: "weight",
		},
		{
			name:    "未知输入键",
			draft:   map[string]any{"weight": map[string]any{"sets": float64(3)}},
			wantErr: "unknown keys", wantField: "weight",
		},
		{
			name:    "未知字段",
			draft:   map[string]any{"nope": map[string]any{"set": float64(1)}},
			wantErr: "unknown fields", wantField: "__root__",
		},
		{
			name:    "set 值类型不符",
			draft:   map[string]any{"weight": map[string]any{"set": "abc"}},
			wantErr: "invalid for this field", wantField: "weight",
		},
		{
			name:    "clear 不可清字段",
			draft:   map[string]any{"weight": map[string]any{"clear": true}},
			wantErr: "clear mode is not supported", wantField: "weight",
		},
		{
			name:    "hhmm 校验",
			draft:   map[string]any{"active_time_start": map[string]any{"set": "24:00"}},
			wantErr: "invalid for this field", wantField: "active_time_start",
		},
		{
			name:    "思考预算区间",
			draft:   map[string]any{"anthropic_thinking_budget_preference": map[string]any{"set": "1023"}},
			wantErr: "invalid for this field", wantField: "anthropic_thinking_budget_preference",
		},
		{
			name:    "重定向规则缺 target",
			draft:   map[string]any{"model_redirects": map[string]any{"set": []any{map[string]any{"matchType": "exact", "source": "a"}}}},
			wantErr: "invalid for this field", wantField: "model_redirects",
		},
		{
			name:    "自适应思考 specific 需 models",
			draft:   map[string]any{"anthropic_adaptive_thinking": map[string]any{"set": map[string]any{"effort": "high", "modelMatchMode": "specific", "models": []any{}}}},
			wantErr: "invalid for this field", wantField: "anthropic_adaptive_thinking",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := normalizeProviderBatchPatchDraft(testCase.draft)
			if testCase.wantErr == "" {
				if err != nil {
					t.Fatalf("应通过，实际错误 %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("应报错 %q，实际通过", testCase.wantErr)
			}
			if !strings.Contains(err.Message, testCase.wantErr) {
				t.Fatalf("错误文案应含 %q，实际 %q", testCase.wantErr, err.Message)
			}
			if err.Field != testCase.wantField {
				t.Fatalf("错误字段应为 %s，实际 %s", testCase.wantField, err.Field)
			}
		})
	}
}

func TestProviderBatchPatchClearValuesMatchNode(t *testing.T) {
	patch := mustNormalize(t, map[string]any{
		"group_tag":            map[string]any{"clear": true},
		"allowed_clients":      map[string]any{"clear": true},
		"cache_ttl_preference": map[string]any{"clear": true},
		"limit_5h_usd":         map[string]any{"clear": true},
		"max_retry_attempts":   map[string]any{"clear": true},
		"allowed_models":       map[string]any{"set": []any{}},
		"active_time_start":    map[string]any{"clear": true},
	})
	updates := buildProviderBatchApplyUpdates(patch)
	if value, ok := updates["group_tag"]; !ok || value != nil {
		t.Fatalf("group_tag 应清成 NULL，实际 %#v", value)
	}
	if value, ok := updates["active_time_start"]; !ok || value != nil {
		t.Fatalf("active_time_start 应清成 NULL，实际 %#v", value)
	}
	if value, ok := updates["limit_5h_usd"]; !ok || value != nil {
		t.Fatalf("limit_5h_usd 应清成 NULL，实际 %#v", value)
	}
	if value, ok := updates["max_retry_attempts"]; !ok || value != nil {
		t.Fatalf("max_retry_attempts 应清成 NULL，实际 %#v", value)
	}
	if value, ok := updates["cache_ttl_preference"].(*string); !ok || value == nil || *value != "inherit" {
		t.Fatalf("cache_ttl_preference 应清成 inherit，实际 %#v", updates["cache_ttl_preference"])
	}
	list, ok := updates["allowed_clients"].(json.RawMessage)
	if !ok || string(list) != "[]" {
		t.Fatalf("allowed_clients 应清成空数组（jsonb 绑定形状），实际 %#v", updates["allowed_clients"])
	}
	if value, ok := updates["allowed_models"]; !ok || value != nil {
		t.Fatalf("allowed_models 空数组应写成 NULL，实际 %#v", value)
	}
}

func TestProviderBatchPatchChangedFieldsFollowContractOrder(t *testing.T) {
	patch := mustNormalize(t, map[string]any{
		"limit_5h_usd": map[string]any{"set": float64(1)},
		"is_enabled":   map[string]any{"set": true},
		"group_tag":    map[string]any{"set": "a,b"},
	})
	if !hasProviderBatchPatchChanges(patch) {
		t.Fatal("有 set 时 hasChanges 应为 true")
	}
	got := strings.Join(changedProviderPatchFields(patch), ",")
	if got != "is_enabled,group_tag,limit_5h_usd" {
		t.Fatalf("changedFields 应按契约表序，实际 %s", got)
	}
	if hasProviderBatchPatchChanges(mustNormalize(t, map[string]any{"is_enabled": map[string]any{"no_change": true}})) {
		t.Fatal("全 no_change 时 hasChanges 应为 false")
	}
	if spec, ok := providerPatchFieldByName("is_enabled"); !ok || spec.ProviderKey != "isEnabled" {
		t.Fatalf("is_enabled 的 Provider 键应为 isEnabled")
	}
}

func TestProviderBatchPatchGroupTagNormalization(t *testing.T) {
	patch := mustNormalize(t, map[string]any{"group_tag": map[string]any{"set": " b ,a，b\n a "}})
	value, ok := patch["group_tag"].SetValue.(*string)
	if !ok || value == nil || *value != "b,a" {
		t.Fatalf("group_tag 应去空去重保序（Node normalizeProviderGroupTag），实际 %#v", patch["group_tag"].SetValue)
	}
	empty := mustNormalize(t, map[string]any{"group_tag": map[string]any{"set": "  "}})
	if value := empty["group_tag"].SetValue; value != (*string)(nil) {
		t.Fatalf("空 group_tag 应归一为 NULL，实际 %#v", value)
	}
}

func TestProviderBatchPatchFingerprintIsOrderStable(t *testing.T) {
	patch := mustNormalize(t, map[string]any{"is_enabled": map[string]any{"set": true}, "weight": map[string]any{"set": float64(7)}})
	left := providerBatchFingerprint("tok", "rev", []int64{2, 1}, patch, []int64{})
	right := providerBatchFingerprint("tok", "rev", []int64{2, 1}, patch, []int64{})
	if left != right || len(left) != 64 {
		t.Fatalf("同一载荷应产出稳定 64 位指纹，实际 %s / %s", left, right)
	}
	if changed := providerBatchFingerprint("tok", "rev", []int64{2, 1}, patch, []int64{3}); changed == left {
		t.Fatal("excludeProviderIds 变化必须改变指纹（否则幂等键会被错误复用）")
	}
	if !strings.Contains(string(marshalProviderBatchPatchSerialized(patch)), `"is_enabled":{"mode":"set","value":true}`) {
		t.Fatalf("串行化形态应可读且键序确定，实际 %s", marshalProviderBatchPatchSerialized(patch))
	}
}

func TestGenerateProviderBatchPreviewRowsSkipsIncompatibleGroups(t *testing.T) {
	rows := generateProviderBatchPreviewRows(
		[]int64{1, 2},
		map[int64]store.AdminProviderBatchRow{
			1: {ID: 1, Name: "claude-1", ProviderType: "claude", Values: map[string]any{"isEnabled": true}},
			2: {ID: 2, Name: "codex-1", ProviderType: "codex", Values: map[string]any{"isEnabled": false}},
		},
		mustNormalize(t, map[string]any{"codex_max_tokens_preference": map[string]any{"set": "2048"}}),
		[]string{"codex_max_tokens_preference"},
	)
	if len(rows) != 2 {
		t.Fatalf("应产出两行（每供应商每字段一行），实际 %d", len(rows))
	}
	var skipped *providerBatchPreviewRow
	for index := range rows {
		if rows[index].ProviderID == 1 {
			skipped = &rows[index]
		}
	}
	if skipped == nil || skipped.Status != "skipped" || !strings.Contains(skipped.SkipReason, "only applicable to codex") {
		t.Fatalf("claude 供应商上的 codex 专属字段应标 skipped 并给出理由，实际 %#v", skipped)
	}
	payload, err := json.Marshal(rows)
	if err != nil || len(payload) == 0 {
		t.Fatalf("预览行应可序列化，实际错误 %v", err)
	}
}

// TestProviderBatchPatchGroupPrioritiesRequiresIntegers 钉住批量补丁对分组优先级覆盖的口径：
// 与单条创建/更新（providerNumberRecordJSONSpec）一致——只收整数值。
//
// 理由同单条路径：列是 jsonb，而选路侧按 int 读。放行小数等于写下一行读不出的覆盖，
// 用户只会看到「配了不生效」。两处的差别仅在单条路径拿到 JSON 原文，因而能连
// `2.0` 这种「读不回的写法」也一起拒；这里经 float64 解码后 2.0 与 2 已不可分。
func TestProviderBatchPatchGroupPrioritiesRequiresIntegers(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  bool
	}{
		{name: "整数记录", value: map[string]any{"fan": float64(0), "vip": float64(2)}, want: true},
		{name: "JS 的 2.0（JSON 里就是整数 2）", value: map[string]any{"fan": float64(2)}, want: true},
		{name: "空对象", value: map[string]any{}, want: true},
		{name: "负数", value: map[string]any{"fan": float64(-1)}, want: true},
		{name: "小数拒绝", value: map[string]any{"fan": 2.5}, want: false},
		{name: "字符串值拒绝", value: map[string]any{"fan": "0"}, want: false},
		{name: "null 值拒绝", value: map[string]any{"fan": nil}, want: false},
		{name: "bool 值拒绝", value: map[string]any{"fan": true}, want: false},
		{name: "数组值拒绝", value: map[string]any{"fan": []any{float64(1)}}, want: false},
		{name: "顶层数组拒绝", value: []any{float64(1)}, want: false},
		{name: "顶层标量拒绝", value: "0", want: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := validateNumberRecord(testCase.value); got != testCase.want {
				t.Fatalf("validateNumberRecord(%#v) = %v，期望 %v", testCase.value, got, testCase.want)
			}
		})
	}
}
