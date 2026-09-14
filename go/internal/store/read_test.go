package store

import (
	"encoding/json"
	"testing"
)

// 选路四列在 row_to_json 里的形态各不相同（int4 为数字、jsonb 为对象或 null、boolean 为真假），
// 这里用真实形态的载荷钉住反序列化：
//   - provider_vendor_id / protocol_conversion_enabled / disable_session_reuse 照旧落成强类型；
//   - group_priorities 落成**原文**（脏值不得让整行读不出来），可用覆盖由 DecodeGroupPriorities 挑。
func TestProviderDecodesRoutingColumns(t *testing.T) {
	payload := `{
		"id": 7, "name": "p7", "provider_type": "claude",
		"provider_vendor_id": 3,
		"group_priorities": {"vip": 0, "default": 5},
		"protocol_conversion_enabled": true,
		"disable_session_reuse": true
	}`
	var provider Provider
	if err := json.Unmarshal([]byte(payload), &provider); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if provider.ProviderVendorID == nil || *provider.ProviderVendorID != 3 {
		t.Fatalf("provider_vendor_id = %v, want 3", provider.ProviderVendorID)
	}
	if string(provider.GroupPriorities) != `{"vip": 0, "default": 5}` {
		t.Fatalf("group_priorities 原文 = %s", provider.GroupPriorities)
	}
	decoded, issues := DecodeGroupPriorities(provider.GroupPriorities)
	if len(issues) != 0 {
		t.Fatalf("干净的 group_priorities 不该报问题：%v", issues)
	}
	if decoded["vip"] != 0 || decoded["default"] != 5 {
		t.Fatalf("解码结果 = %v, want vip=0 default=5", decoded)
	}
	if !provider.ProtocolConversionEnabled {
		t.Fatal("protocol_conversion_enabled 应为 true")
	}
	if !provider.DisableSessionReuse {
		t.Fatal("disable_session_reuse 应为 true")
	}

	// jsonb 列的默认值形态是 JSON null，SQL NULL 走的是同一个 JSON 键，
	// 两者都应解成「无覆盖且无问题」，而不是反序列化失败。
	var nullProvider Provider
	if err := json.Unmarshal(
		[]byte(`{"group_priorities": null, "provider_vendor_id": null}`),
		&nullProvider,
	); err != nil {
		t.Fatalf("含 null 的载荷不应反序列化失败: %v", err)
	}
	if decoded, issues := DecodeGroupPriorities(nullProvider.GroupPriorities); len(decoded) != 0 || len(issues) != 0 {
		t.Fatalf("null 的 group_priorities 应解成空且无问题，实际 %v / %v", decoded, issues)
	}
	if nullProvider.ProviderVendorID != nil {
		t.Fatalf("null 的 provider_vendor_id 应为 nil，实际 %v", *nullProvider.ProviderVendorID)
	}
}

// TestDecodeGroupPriorities 钉住容错解码的接受集与丢弃集。
//
// 关键不变量：**任何输入都不返回错误**——旧实现（直接解成 map[string]int）在脏值上返回错误，
// 而这是 row_to_json 的逐行解码，一行脏值会让整批供应商读不出来（选路拿到 0 家候选）。
func TestDecodeGroupPriorities(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		want     map[string]int
		wantKeys []string // 期望的问题键（按集合比较）
		topLevel bool
	}{
		{name: "正常对象", raw: `{"fan": 0, "vip": 2}`, want: map[string]int{"fan": 0, "vip": 2}},
		// 值为 null：json.Unmarshal 把 null 解到 int 是无操作且成功，故键存在、值为 0（既有语义）。
		{name: "null 值取 0", raw: `{"fan": null}`, want: map[string]int{"fan": 0}},
		{name: "空对象", raw: `{}`},
		{name: "JSON null", raw: `null`},
		{name: "SQL NULL（空原文）", raw: ``},
		{name: "字符串值被丢弃", raw: `{"fan": "0"}`, wantKeys: []string{"fan"}},
		{name: "浮点值被丢弃", raw: `{"fan": 0.5}`, wantKeys: []string{"fan"}},
		{name: "布尔值被丢弃", raw: `{"fan": true}`, wantKeys: []string{"fan"}},
		{name: "数组值被丢弃", raw: `{"fan": [1]}`, wantKeys: []string{"fan"}},
		{
			name: "好键保留、坏键丢弃",
			raw:  `{"fan": "0", "vip": 3}`, want: map[string]int{"vip": 3}, wantKeys: []string{"fan"},
		},
		{name: "顶层数组", raw: `["fan"]`, topLevel: true},
		{name: "顶层标量", raw: `"abc"`, topLevel: true},
		{name: "顶层浮点", raw: `0.5`, topLevel: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, issues := DecodeGroupPriorities(json.RawMessage(testCase.raw))
			if len(got) != len(testCase.want) {
				t.Fatalf("解出 %v，期望 %v", got, testCase.want)
			}
			for key, want := range testCase.want {
				if got[key] != want {
					t.Fatalf("键 %s = %d，期望 %d（整体 %v）", key, got[key], want, got)
				}
			}
			if len(issues) != len(testCase.wantKeys)+boolToInt(testCase.topLevel) {
				t.Fatalf("问题数 = %d（%v），期望 %d", len(issues), issues, len(testCase.wantKeys))
			}
			seen := map[string]bool{}
			for _, issue := range issues {
				seen[issue.Key] = true
				if issue.JSONType == "" {
					t.Fatalf("问题必须带 jsonb 类型：%v", issue)
				}
				if issue.TopLevel != testCase.topLevel {
					t.Fatalf("topLevel = %v，期望 %v（%v）", issue.TopLevel, testCase.topLevel, issue)
				}
			}
			for _, key := range testCase.wantKeys {
				if !seen[key] {
					t.Fatalf("键 %s 应出现在问题里：%v", key, issues)
				}
			}
		})
	}
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
