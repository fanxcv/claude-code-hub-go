package specialsettings

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// TestRequestEntriesFollowsClientProtocol 钉住「按客户端入站协议选提取器」这条不变量。
//
// 若哪天改成按供应商协议选，跨协议请求（客户端 anthropic、供应商 chat）会拿 chat 的解析器
// 去读 anthropic body，结果恒为空——使用记录页的思考强度列会**再次静默变空**。
func TestRequestEntriesFollowsClientProtocol(t *testing.T) {
	cases := []struct {
		name   string
		format convert.ClientFormat
		path   string
		body   string
		want   string
	}{
		{"anthropic 的 output_config.effort", convert.FormatClaude, "/v1/messages",
			`{"output_config":{"effort":"high"}}`, "high"},
		{"codex 的 reasoning.effort", convert.FormatResponse, "/v1/responses",
			`{"reasoning":{"effort":"medium"}}`, "medium"},
		{"openai 顶层 reasoning_effort", convert.FormatOpenAI, "/v1/chat/completions",
			`{"reasoning_effort":"low"}`, "low"},
		{"openai 嵌套 reasoning.effort", convert.FormatOpenAI, "/v1/chat/completions",
			`{"reasoning":{"effort":"high"}}`, "high"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			body := decodeBody(t, testCase.body)
			raw := RequestEntries(body, testCase.format, testCase.path)
			if raw == nil {
				t.Fatal("应产出审计条目，实际为 nil")
			}
			entries := decodeEntries(t, raw)
			if len(entries) != 1 {
				t.Fatalf("应恰好一条，实际 %d：%s", len(entries), string(raw))
			}
			if got := entries[0]["effort"]; got != testCase.want {
				t.Fatalf("effort 应为 %q，实际 %v", testCase.want, got)
			}
		})
	}
}

// TestRequestEntriesEdgeCases 钉住三类「不该写」的情形：空白、非字符串、端点不匹配。
func TestRequestEntriesEdgeCases(t *testing.T) {
	cases := []struct {
		name   string
		format convert.ClientFormat
		path   string
		body   string
	}{
		{"空白不算值", convert.FormatClaude, "/v1/messages", `{"output_config":{"effort":"   "}}`},
		{"非字符串不算值", convert.FormatClaude, "/v1/messages", `{"output_config":{"effort":123}}`},
		{"output_config 是数组", convert.FormatClaude, "/v1/messages", `{"output_config":[]}`},
		// 端点守卫：openai 客户端线还跑 embeddings 等端点，只有 chat/completions 才是对话体。
		{"openai 非 chat 端点不写", convert.FormatOpenAI, "/v1/embeddings", `{"reasoning_effort":"high"}`},
		{"openai 尾斜杠仍算 chat 端点", convert.FormatOpenAI, "/v1/chat/completions/", `{"reasoning_effort":"high"}`},
		{"无该字段", convert.FormatClaude, "/v1/messages", `{"model":"m"}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			raw := RequestEntries(decodeBody(t, testCase.body), testCase.format, testCase.path)
			// 「尾斜杠仍算 chat 端点」是唯一期望非 nil 的用例。
			wantNil := testCase.name != "openai 尾斜杠仍算 chat 端点"
			if wantNil && raw != nil {
				t.Fatalf("不该产出条目，实际 %s", string(raw))
			}
			if !wantNil && raw == nil {
				t.Fatal("尾斜杠应被归一为 chat 端点，实际未产出条目")
			}
		})
	}
}

// TestProbeEntriesRecordsDroppedExplicitly 钉住探针的三态：保留 / 转换丢弃 / 原生直通读空。
//
// 「转换丢弃必须显式记账」是用户口径：列里不能只留一个空值让人猜是「没转换」还是「转换丢了」。
func TestProbeEntriesRecordsDroppedExplicitly(t *testing.T) {
	requested := EffortRequest{Effort: "high", Field: "output_config.effort", Protocol: convert.ProtocolAnthropicMessages, Present: true}

	t.Run("转换保留", func(t *testing.T) {
		forwarded := EffortForwarded{Effort: "high", Field: "reasoning_effort", Protocol: convert.ProtocolOpenAIChat, Present: true}
		entry := singleEntry(t, ProbeEntries(requested, forwarded, true))
		if entry["dropped"] != false {
			t.Fatalf("保留时 dropped 应为 false：%v", entry)
		}
		if entry["forwardedEffort"] != "high" || entry["requestedEffort"] != "high" {
			t.Fatalf("两侧值都要留：%v", entry)
		}
		if entry["converted"] != true {
			t.Fatalf("converted 应为 true：%v", entry)
		}
	})

	t.Run("转换丢弃", func(t *testing.T) {
		entry := singleEntry(t, ProbeEntries(requested, EffortForwarded{}, true))
		if entry["dropped"] != true {
			t.Fatalf("转换生效且产物缺该字段时应显式记 dropped=true：%v", entry)
		}
		if entry["forwardedEffort"] != nil {
			t.Fatalf("丢弃时 forwardedEffort 应为 null：%v", entry)
		}
	})

	t.Run("原生直通读不到不算丢弃", func(t *testing.T) {
		entry := singleEntry(t, ProbeEntries(requested, EffortForwarded{}, false))
		if entry["dropped"] != false {
			t.Fatalf("未发生转换时读不到只能说明路径取错，不得断言丢弃：%v", entry)
		}
	})

	t.Run("两侧都没有则不写条目", func(t *testing.T) {
		if raw := ProbeEntries(EffortRequest{}, EffortForwarded{}, true); raw != nil {
			t.Fatalf("无值可记时应返回 nil，实际 %s", string(raw))
		}
	})
}

// TestForwardedTrimmedEffortReadsConverterProduct 钉住「按目标线读产物」的字段约定。
func TestForwardedTrimmedEffortReadsConverterProduct(t *testing.T) {
	cases := []struct {
		protocol convert.WireProtocol
		body     string
		want     string
	}{
		{convert.ProtocolOpenAIChat, `{"reasoning_effort":"high"}`, "high"},
		{convert.ProtocolOpenAIChat, `{"reasoning":{"effort":"low"}}`, "low"},
		{convert.ProtocolAnthropicMessages, `{"output_config":{"effort":" medium "}}`, "medium"},
		{convert.ProtocolOpenAIResponses, `{"reasoning":{"effort":"high"}}`, "high"},
		{convert.ProtocolOpenAIChat, `{"model":"m"}`, ""},
	}
	for _, testCase := range cases {
		got := ForwardedTrimmedEffort(testCase.protocol, []byte(testCase.body))
		if testCase.want == "" {
			if got.Present {
				t.Fatalf("%s 不该读到值：%+v", testCase.body, got)
			}
			continue
		}
		if !got.Present || got.Effort != testCase.want {
			t.Fatalf("%s 应读到 %q，实际 %+v", testCase.body, testCase.want, got)
		}
	}
}

// TestForwardedTrimmedEffortAtUsesClientFieldPath 钉住原生直通路径按**客户端**字段路径读。
func TestForwardedTrimmedEffortAtUsesClientFieldPath(t *testing.T) {
	body := []byte(`{"output_config":{"effort":"high"}}`)
	got := ForwardedTrimmedEffortAt(body, "output_config.effort", convert.ProtocolAnthropicMessages)
	if !got.Present || got.Effort != "high" {
		t.Fatalf("应读到 high，实际 %+v", got)
	}
	if got := ForwardedTrimmedEffortAt(body, "reasoning.effort", convert.ProtocolOpenAIChat); got.Present {
		t.Fatalf("按错误路径不该读到值：%+v", got)
	}
}

func decodeBody(t *testing.T, body string) map[string]any {
	t.Helper()
	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("夹具 body 非法: %v", err)
	}
	return parsed
}

// TestConversionEntryMatchesNodeShape 钉住协议转换审计的**形状**与**产出条件**。
//
// 这条对应使用记录页的「协议转换」列（`protocol-conversion-display.tsx`）：该列只在审计里
// 存在 `protocol_conversion` 记录时才画徐章。两边错一处都会让整列恒空：
//   - 形状错（字段名/scope/hit）→ 前端 `getProtocolConversion` 拓不出值；
//   - 条件错（对未转换的请求也写）→ 给全部原生请求白写一条，与 Node 的反向标记取舍相违
//     （`forwarder.ts:3778` 注释：未转换不写反向标记）。
func TestConversionEntryMatchesNodeShape(t *testing.T) {
	plan := &convert.ConversionPlan{
		ClientProtocol: convert.ProtocolAnthropicMessages,
		TargetProtocol: convert.ProtocolOpenAIChat,
	}
	entry := ConversionEntry(plan)
	if entry == nil {
		t.Fatal("转换计划非 nil 时必须产出条目")
	}
	want := map[string]any{
		"type":           "protocol_conversion",
		"scope":          "request",
		"hit":            true,
		"clientProtocol": "anthropic-messages",
		"targetProtocol": "openai-chat",
	}
	for key, expected := range want {
		if got, ok := entry[key]; !ok || got != expected {
			t.Errorf("字段 %s 应为 %v，实际 %v（全条目：%v）", key, expected, got, entry)
		}
	}
	if len(entry) != len(want) {
		t.Errorf("条目字段数应为 %d（与 Node 同集），实际 %d：%v", len(want), len(entry), entry)
	}

	if got := ConversionEntry(nil); got != nil {
		t.Errorf("未施加转换（plan 为 nil）时不得产出条目，实际 %v", got)
	}
}

// TestAppendEntriesDropsNilAndReturnsNilWhenEmpty 钉住合成语义。
//
// 两个关键点：
//   - nil 条目（「本次没有这个事实」）**不得**在数组里留下 null 占位；
//   - 全 nil 时返回 **nil**（而非空数组）：存储层用 nil 表示「不写该列」，
//     空数组会写进去，与 Node 的 `null` 两态语义不同。
func TestAppendEntriesDropsNilAndReturnsNilWhenEmpty(t *testing.T) {
	if got := AppendEntries(nil, nil); got != nil {
		t.Errorf("全 nil 时应返回 nil，实际 %s", string(got))
	}
	converted := ConversionEntry(&convert.ConversionPlan{
		ClientProtocol: convert.ProtocolOpenAIResponses,
		TargetProtocol: convert.ProtocolOpenAIChat,
	})
	probe := ProbeEntry(
		EffortRequest{Effort: "high", Field: "reasoning.effort", Kind: TypeCodexEffort,
			Protocol: convert.ProtocolOpenAIResponses, Present: true},
		EffortForwarded{Effort: "high", Field: "reasoning_effort",
			Protocol: convert.ProtocolOpenAIChat, Present: true},
		true,
	)
	entries := decodeEntries(t, AppendEntries(probe, nil, converted))
	if len(entries) != 2 {
		t.Fatalf("应恰好两条（探针 + 转换），实际 %d：%v", len(entries), entries)
	}
	if entries[0]["type"] != TypeThinkingEffortForwarded {
		t.Errorf("首条应是思考强度探针，实际 %v", entries[0])
	}
	if entries[1]["type"] != TypeProtocolConversion {
		t.Errorf("次条应是协议转换，实际 %v", entries[1])
	}
}

func decodeEntries(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var entries []map[string]any
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("条目不是 JSON 数组: %v（%s）", err, string(raw))
	}
	return entries
}

func singleEntry(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	if raw == nil {
		t.Fatal("应产出条目，实际为 nil")
	}
	entries := decodeEntries(t, raw)
	if len(entries) != 1 {
		t.Fatalf("应恰好一条，实际 %d", len(entries))
	}
	return entries[0]
}

// TestConversionFailureEntryShape 钉住「协议转换失败」审计的形状与产出条件。
//
// 对应使用记录页「协议转换」列的失败态：该列此前只会显示「已转换」，转换失败时**什么都不显示**
// （因为失败在实现里被静默吞掉），于是「没转换」与「想转换但失败」在界面上长得一样。
// 本条目把后者显式记出，故键名与 `protocol_conversion` 保持同一口径（协议对字段逐字同名），
// 只多出 phase / reason / fallback 三个失败专有字段。
func TestConversionFailureEntryShape(t *testing.T) {
	failure := &convert.ConversionFailure{
		ClientProtocol: convert.ProtocolAnthropicMessages,
		TargetProtocol: convert.ProtocolOpenAIChat,
		Phase:          convert.PhaseBodyConversion,
		Reason:         "forward: 请求正文不是合法 JSON 对象: unexpected end of JSON input",
		Fallback:       true,
	}
	entry := ConversionFailureEntry(failure)
	if entry == nil {
		t.Fatal("失败事实非 nil 时必须产出条目")
	}
	want := map[string]any{
		"type":           TypeProtocolConversionFailed,
		"scope":          "request",
		"hit":            true,
		"clientProtocol": "anthropic-messages",
		"targetProtocol": "openai-chat",
		"phase":          string(convert.PhaseBodyConversion),
		"fallback":       true,
	}
	for key, expected := range want {
		if got, ok := entry[key]; !ok || got != expected {
			t.Errorf("字段 %s 应为 %v，实际 %v（全条目：%v）", key, expected, got, entry)
		}
	}
	if reason, _ := entry["reason"].(string); reason == "" {
		t.Error("reason 不得为空：失败原因正是这条审计的价值所在")
	}
	if got := ConversionFailureEntry(nil); got != nil {
		t.Errorf("未失败（failure 为 nil）时不得产出条目，实际 %v", got)
	}
}

// TestConversionFailureEntryAndConversionEntryAreMutuallyExclusive 钉住「成功与失败互斥」。
//
// 同一次尝试只能有一条转换审计：既成功又失败会让界面同时画「已转换」与「转换失败」，
// 而这两种状态在排障上是互斥的结论。互斥由 forward 的事实保证（Conversion 与
// ConversionFailure 不会同时非 nil），此处把它作为契约钉住。
func TestConversionFailureEntryAndConversionEntryAreMutuallyExclusive(t *testing.T) {
	plan := &convert.ConversionPlan{
		ClientProtocol: convert.ProtocolAnthropicMessages,
		TargetProtocol: convert.ProtocolOpenAIChat,
	}
	// 成功：只有成功条目。
	if got := ConversionFailureEntry(nil); got != nil {
		t.Errorf("成功路径不得产出失败条目：%v", got)
	}
	if got := ConversionEntry(plan); got == nil {
		t.Fatal("成功路径必须产出成功条目")
	}
	// 失败：只有失败条目（plan 在失败时已被置回 nil，见 forward.BuildPlan）。
	if got := ConversionEntry(nil); got != nil {
		t.Errorf("失败路径不得产出成功条目：%v", got)
	}
	if got := ConversionFailureEntry(&convert.ConversionFailure{Phase: convert.PhasePathResolution}); got == nil {
		t.Error("失败路径必须产出失败条目")
	}
}

// TestSanitizeReasonRedactsAndBounds 钉住失败原因的脱敏与定长。
//
// 为什么必须脱敏：审计会在管理面展示。错误链里可能带 URL（含 userinfo/query）或形如
// `Bearer …` / `sk-…` 的凭据片段，写进审计等于把凭据落到库里与界面上。
func TestSanitizeReasonRedactsAndBounds(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		mustHave []string
		mustNot  []string
	}{
		{
			name:     "URL 的 userinfo 与查询串必须去掉",
			input:    "上游 https://user:s3cret@relay.example.com/v1/messages?key=abc123 返回异常",
			mustHave: []string{"relay.example.com", "返回异常"},
			mustNot:  []string{"s3cret", "abc123"},
		},
		{
			name:     "Bearer 令牌只保留方案名",
			input:    "上游拒绝：Authorization: Bearer sk-live-abcdef123456",
			mustHave: []string{"Bearer", "<redacted>"},
			mustNot:  []string{"sk-live-abcdef123456"},
		},
		{
			name:     "裸密钥前缀",
			input:    "凭证 sk-proj-1234567890abcdef 无效",
			mustHave: []string{"sk-", "<redacted>"},
			mustNot:  []string{"1234567890abcdef"},
		},
		{
			name:     "换行折叠为单行（审计按一行展示）",
			input:    "第一行\n第二行\t第三行",
			mustHave: []string{"第一行 第二行 第三行"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := SanitizeReason(testCase.input)
			for _, fragment := range testCase.mustHave {
				if !strings.Contains(got, fragment) {
					t.Errorf("结果应含 %q，实际 %q", fragment, got)
				}
			}
			for _, fragment := range testCase.mustNot {
				if strings.Contains(got, fragment) {
					t.Errorf("结果**不得**含 %q，实际 %q", fragment, got)
				}
			}
		})
	}

	t.Run("超长原因被截断且仍可读", func(t *testing.T) {
		long := strings.Repeat("原", 1000)
		got := SanitizeReason(long)
		if len(got) > maxConversionFailureReasonLength {
			t.Errorf("截断后长度 %d 超过上限 %d", len(got), maxConversionFailureReasonLength)
		}
		if !utf8.ValidString(got) {
			t.Error("截断必须落在 rune 边界：不得切出半个汉字")
		}
		if !strings.HasSuffix(got, truncatedMarker) {
			t.Errorf("截断必须留显式标记，实际结尾 %q", got[len(got)-8:])
		}
	})

	t.Run("空原因保持为空（不写噪声）", func(t *testing.T) {
		if got := SanitizeReason("   \n  "); got != "" {
			t.Errorf("空白原因应归一为空串，实际 %q", got)
		}
	})
}

// TestCodexSessionCompletionEntryMatchesNodeShape 钉住补全审计的字段集与取值。
//
// 形状真源：Node `session-guard.ts:128` 的对象字面量与 `src/types/special-settings.ts` 的
// `CodexSessionIdCompletionSpecialSetting`——字段名与取值域都不得改写（前端按名取值）。
func TestCodexSessionCompletionEntryMatchesNodeShape(t *testing.T) {
	entry := CodexSessionCompletionEntry(
		"generated_uuid_v7",
		"fingerprint_cache",
		"01a0a2a1-c7ff-7747-81cc-4e27411e8938",
	)
	if entry == nil {
		t.Fatal("应产出条目，实际为 nil")
	}
	want := map[string]any{
		"type":      "codex_session_id_completion",
		"scope":     "request",
		"hit":       true,
		"action":    "generated_uuid_v7",
		"source":    "fingerprint_cache",
		"sessionId": "01a0a2a1-c7ff-7747-81cc-4e27411e8938",
	}
	for key, expected := range want {
		if got, ok := entry[key]; !ok || got != expected {
			t.Errorf("字段 %s 应为 %v，实际 %v（全条目：%v）", key, expected, got, entry)
		}
	}
	if len(entry) != len(want) {
		t.Errorf("条目字段数应为 %d（与 Node 同集），实际 %d：%v", len(want), len(entry), entry)
	}
}

// TestCodexSessionCompletionEntrySkipsNoneAndEmpty 钉住「不补就不记」。
//
// Node 只在 `completion.applied && completion.action !== "none"` 时记条目。
func TestCodexSessionCompletionEntrySkipsNoneAndEmpty(t *testing.T) {
	cases := []struct {
		name      string
		action    string
		source    string
		sessionID string
	}{
		{name: "action=none", action: "none", source: "header_session_id", sessionID: "01a0a2a1-c7ff-7747-81cc-4e27411e8938"},
		{name: "action 为空", action: "", source: "header_session_id", sessionID: "01a0a2a1-c7ff-7747-81cc-4e27411e8938"},
		{name: "sessionId 为空", action: "completed_missing_fields", source: "header_session_id", sessionID: ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := CodexSessionCompletionEntry(testCase.action, testCase.source, testCase.sessionID); got != nil {
				t.Errorf("应返回 nil，实际 %v", got)
			}
		})
	}
}

// TestConversionLossEntryAggregatesByCapabilityAndAction 钉住损失审计的聚合口径、字段集与确定性。
//
// 为何要这一组：这条审计是本任务新开的口子（`LossReport` 此前**只写不读**），没有 Node 对拍可依，
// 只有本文件的断言能发现它回归。三种回归各自意味着什么：
//   - 不产出 → 跨线丢字段再次变成生产上不可见（回到原状）；
//   - 逐条展开（把 detail 落进条目）→ 一次工具密集的请求把 jsonb 撑大，且把工具名/字段路径
//     这类请求数据搬进审计列；
//   - 分组顺序不定 → 同一份损失集序列化出不同字节，前端按内容去重（buildUnifiedSpecialSettings）
//     会把它当成两条不同事实。
func TestConversionLossEntryAggregatesByCapabilityAndAction(t *testing.T) {
	plan := &convert.ConversionPlan{
		ClientProtocol: convert.ProtocolOpenAIResponses,
		TargetProtocol: convert.ProtocolOpenAIChat,
	}
	loss := &convert.LossReport{Entries: []convert.LossEntry{
		{Capability: convert.LossCacheControl, Direction: "request", Action: convert.LossDropped, Detail: "text"},
		{Capability: convert.LossCacheControl, Direction: "request", Action: convert.LossDropped, Detail: "image"},
		{Capability: convert.LossUnknownField, Direction: "request", Action: convert.LossDropped, Detail: "refusal"},
		// 同一 capability、不同 action 必须分成两组（动作不同 = 事实不同：丢了 vs 改了）。
		{Capability: convert.LossUnknownField, Direction: "request", Action: convert.LossRewritten, Detail: "message.role"},
		{Capability: convert.LossThinkingDerived, Direction: "request", Action: convert.LossDowngraded, Detail: "budget_tokens=8192→effort=high"},
		// 信息档也要出现一次：三档合计恒在且相加等于 total，是该条目的不变量。
		{Capability: convert.LossPromptCacheKey, Direction: "request", Action: convert.LossDropped, Detail: "prompt_cache_key"},
	}}
	entry := ConversionLossEntry(plan, loss)
	if entry == nil {
		t.Fatal("转换成功且有损失时必须产出条目")
	}
	want := map[string]any{
		"type":           TypeProtocolConversionLoss,
		"scope":          "request",
		"hit":            true,
		"clientProtocol": "openai-responses",
		"targetProtocol": "openai-chat",
		"total":          6,
		// 档位合计：cache_control(2) + thinking.derived(1) 为降级档，两个 unknown_field 为改写档，
		// prompt_cache_key 为信息档。三者相加必须等于 total（下面显式断言）。
		"rewriteTotal": 2,
		"degradeTotal": 3,
		"infoTotal":    1,
	}
	for key, expected := range want {
		if got, ok := entry[key]; !ok || got != expected {
			t.Errorf("字段 %s 应为 %v，实际 %v（全条目：%v）", key, expected, got, entry)
		}
	}
	wantGroups := []map[string]any{
		{"capability": "cache_control", "action": "dropped", "count": 2, "severity": "degrade"},
		{"capability": "prompt_cache_key", "action": "dropped", "count": 1, "severity": "info"},
		{"capability": "thinking.derived", "action": "downgraded", "count": 1, "severity": "degrade"},
		{"capability": "unknown_field", "action": "dropped", "count": 1, "severity": "rewrite"},
		{"capability": "unknown_field", "action": "rewritten", "count": 1, "severity": "rewrite"},
	}
	groups, ok := entry["groups"].([]map[string]any)
	if !ok {
		t.Fatalf("groups 应为聚合数组，实际 %T（%v）", entry["groups"], entry["groups"])
	}
	if len(groups) != len(wantGroups) {
		t.Fatalf("应聚合成 %d 组，实际 %d：%v", len(wantGroups), len(groups), groups)
	}
	groupSum := 0
	for index, expected := range wantGroups {
		got := groups[index]
		if got["capability"] != expected["capability"] || got["action"] != expected["action"] ||
			got["count"] != expected["count"] || got["severity"] != expected["severity"] {
			t.Errorf("第 %d 组应为 %v，实际 %v", index, expected, got)
		}
		count, _ := got["count"].(int)
		groupSum += count
	}
	// 不变量：分组计数之和、三档之和都必须等于 total（任一处口径漂移都会被这一行抳住）。
	if groupSum != want["total"] {
		t.Errorf("分组计数之和应等于 total %v，实际 %d", want["total"], groupSum)
	}
	severitySum, _ := entry["rewriteTotal"].(int)
	for _, key := range []string{"degradeTotal", "infoTotal"} {
		value, _ := entry[key].(int)
		severitySum += value
	}
	if severitySum != want["total"] {
		t.Errorf("三档之和应等于 total %v，实际 %d（%v）", want["total"], severitySum, entry)
	}
	// detail 不落进条目：序列化结果里不得出现任何 detail 原文（同时证明没有整条展开）。
	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("条目应可序列化：%v", err)
	}
	for _, detail := range []string{"budget_tokens=8192→effort=high", "message.role", "refusal", "text", "image"} {
		if strings.Contains(string(raw), detail) {
			t.Errorf("detail %q 不得进审计条目（会撑大 jsonb 并把请求数据搬进审计列）：%s", detail, raw)
		}
	}
}

// TestConversionLossEntrySkipsZeroLossAndNative 钉住「零损失不写噪声条目」。
//
// 为什么必须区分「零损失」与「没转换」：转换成功且零损失时只该有 protocol_conversion 一条；
// 给每个零损失的转换请求白写一条空 groups，会让按 type 过滤的查询淹在噪声里——
// 这与 TypeProtocolConversion 注释里「未转换不写反向标记」是同一条取舍。
func TestConversionLossEntrySkipsZeroLossAndNative(t *testing.T) {
	plan := &convert.ConversionPlan{
		ClientProtocol: convert.ProtocolAnthropicMessages,
		TargetProtocol: convert.ProtocolOpenAIChat,
	}
	if got := ConversionLossEntry(plan, &convert.LossReport{}); got != nil {
		t.Errorf("零损失不得产出条目，实际 %v", got)
	}
	if got := ConversionLossEntry(plan, nil); got != nil {
		t.Errorf("损失集为 nil 时不得产出条目，实际 %v", got)
	}
	if got := ConversionLossEntry(nil, &convert.LossReport{Entries: []convert.LossEntry{{
		Capability: convert.LossCacheControl, Action: convert.LossDropped,
	}}}); got != nil {
		t.Errorf("未施加转换（plan 为 nil）时不得产出条目：协议对无从取，实际 %v", got)
	}
}
