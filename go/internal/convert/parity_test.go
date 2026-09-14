package convert

import (
	"strings"
	"testing"
)

// 本文件钉住三件本次新增/修复的行为（矩阵审计 §2.3 / §2.4 / §2.6）：
//  1. 思考强度代数（预算 ↔ 等级双向、阈值口径、不凭空发明）；
//  2. 非 function 工具（MCP / web_search）跨线的**专用损失类别**；
//  3. `parallel_tool_calls` 在 Anthropic 线的取反映射（解码与编码两向）。
//
// 为何单测而不是只靠语料：跨语言语料是「与 Node 逐字节对齐」的门，其中 Node 自己在
// 预算→等级、跨线空内容上会丢弃/产出空数组，那些格位由 corpus_test.go 的有意分歧登记覆盖；
// 本文件的职责是钉住**代数本身的正确性与边界**（阈值、往返、缺省不发明）。

// ——————————————————————————————————————————————————————————————
// 1. 思考强度代数
// ——————————————————————————————————————————————————————————————

func TestThinkingLevelFromBudgetThresholds(t *testing.T) {
	cases := []struct {
		budget float64
		want   string
	}{
		{1024, thinkingLevelLow},
		{thinkingBudgetMediumFloor - 1, thinkingLevelLow},
		{thinkingBudgetMediumFloor, thinkingLevelMedium},
		{thinkingBudgetHighFloor - 1, thinkingLevelMedium},
		{thinkingBudgetHighFloor, thinkingLevelHigh},
		{200_000, thinkingLevelHigh},
		{1, thinkingLevelLow}, // 非法小值按最低档（调用方不应在此路径给出 ≤0）
	}
	for _, testCase := range cases {
		if got := ThinkingLevelFromBudget(testCase.budget); got != testCase.want {
			t.Errorf("ThinkingLevelFromBudget(%v) = %q，期望 %q", testCase.budget, got, testCase.want)
		}
	}
}

func TestThinkingBudgetLevelRoundTrip(t *testing.T) {
	// 反向表与阈值必须互为反函数，否则「等级→预算→等级」会漂移。
	for _, level := range []string{thinkingLevelLow, thinkingLevelMedium, thinkingLevelHigh} {
		budget, ok := ThinkingBudgetFromLevel(level)
		if !ok {
			t.Fatalf("ThinkingBudgetFromLevel(%q) 未命中", level)
		}
		if back := ThinkingLevelFromBudget(budget); back != level {
			t.Errorf("往返漂移：%q → %v → %q", level, budget, back)
		}
	}
	if _, ok := ThinkingBudgetFromLevel("unknown"); ok {
		t.Error("未知等级必须返回 ok=false（不得静默落到某一档）")
	}
}

func TestEffectiveThinkingEffortPrefersClientValue(t *testing.T) {
	// 客户端给了等级 → 原值，即便同时有预算（不换算）。
	budget := float64(32768)
	level, derived, ok := effectiveThinkingEffort(&Reasoning{
		Effort: "medium", HasEffort: true, BudgetTokens: &budget,
	})
	if !ok || derived || level != "medium" {
		t.Fatalf("给了等级时应原值返回：level=%q derived=%v ok=%v", level, derived, ok)
	}

	// 只有预算 → 换算（derived=true）。
	level, derived, ok = effectiveThinkingEffort(&Reasoning{BudgetTokens: &budget})
	if !ok || !derived || level != thinkingLevelHigh {
		t.Fatalf("只有预算时应换算：level=%q derived=%v ok=%v", level, derived, ok)
	}

	// 两者都没有 → 不发明。
	if _, _, ok := effectiveThinkingEffort(&Reasoning{}); ok {
		t.Error("客户端没给任何强度载体时不得发明等级")
	}
	if _, _, ok := effectiveThinkingEffort(nil); ok {
		t.Error("Reasoning 为 nil 时不得发明等级")
	}
}

// TestBudgetOnlyDerivesLevelOnLevelOnlyLines 端到端：claude 客户端只给预算，目标线只有等级载体。
func TestBudgetOnlyDerivesLevelOnLevelOnlyLines(t *testing.T) {
	claudeBody := mustParsePayload(t, `{
		"model":"claude-sonnet-4-5","max_tokens":512,
		"thinking":{"type":"enabled","budget_tokens":32768},
		"messages":[{"role":"user","content":"想一下"}]
	}`)

	t.Run("anthropic→chat 落 reasoning_effort 并记降级", func(t *testing.T) {
		ctx := ConvertCtx{ClientFormat: FormatOpenAI, TargetProto: ProtocolOpenAIChat}
		decoded, _ := DecodeRequest(ProtocolAnthropicMessages, claudeBody, ctx)
		encoded, _ := EncodeRequest(ProtocolOpenAIChat, decoded.Value, ctx)
		effort, present := encoded.Body.Get("reasoning_effort")
		if !present {
			t.Fatalf("chat 正文必须带上换算出的等级（否则思考强度整个丢失）：%s", encoded.Body.MarshalCompact())
		}
		text, _ := effort.String()
		if text != thinkingLevelHigh {
			t.Fatalf("预算 32768 应换算为 %q，实际 %q", thinkingLevelHigh, text)
		}
		assertLossEntry(t, encoded.Loss, LossThinkingDerived, string(LossDowngraded), "reasoning_effort=high")
	})

	t.Run("anthropic→responses 落 reasoning.effort 并记降级", func(t *testing.T) {
		ctx := ConvertCtx{ClientFormat: FormatResponse, TargetProto: ProtocolOpenAIResponses}
		decoded, _ := DecodeRequest(ProtocolAnthropicMessages, claudeBody, ctx)
		encoded, _ := EncodeRequest(ProtocolOpenAIResponses, decoded.Value, ctx)
		reasoning, present := encoded.Body.Get("reasoning")
		if !present {
			t.Fatalf("responses 正文必须带上 reasoning.effort：%s", encoded.Body.MarshalCompact())
		}
		effort, _ := reasoning.Get("effort")
		text, _ := effort.String()
		if text != thinkingLevelHigh {
			t.Fatalf("预算 32768 应换算为 %q，实际 %q", thinkingLevelHigh, text)
		}
		assertLossEntry(t, encoded.Loss, LossThinkingDerived, string(LossDowngraded), "reasoning.effort=high")
	})

	t.Run("anthropic→anthropic 原值搬运、不换算也不记降级", func(t *testing.T) {
		ctx := ConvertCtx{ClientFormat: FormatClaude, TargetProto: ProtocolAnthropicMessages}
		decoded, _ := DecodeRequest(ProtocolAnthropicMessages, claudeBody, ctx)
		encoded, _ := EncodeRequest(ProtocolAnthropicMessages, decoded.Value, ctx)
		thinking, present := encoded.Body.Get("thinking")
		if !present {
			t.Fatalf("同线必须原值保留 thinking 预算：%s", encoded.Body.MarshalCompact())
		}
		budget, _ := thinking.Get("budget_tokens")
		if literal, _ := budget.NumberLiteral(); literal != "32768" {
			t.Fatalf("预算应原值搬运 32768，实际 %q", literal)
		}
		for _, entry := range encoded.Loss.Entries {
			if entry.Capability == LossThinkingDerived {
				t.Fatalf("同线不该出现换算降级：%+v", entry)
			}
		}
	})
}

// ——————————————————————————————————————————————————————————————
// 2. 非 function 工具的专用损失类别
// ——————————————————————————————————————————————————————————————

func TestMCPItemLossIsAttributableOnForeignWire(t *testing.T) {
	body := mustParsePayload(t, `{
		"model":"deepseek-v4.1-flash","instructions":"助手",
		"input":[
			{"type":"message","role":"user","content":"搜一下"},
			{"type":"mcp_call","id":"mcp_1","name":"lookup","arguments":"{}"},
			{"type":"web_search_call","id":"ws_1","status":"completed"}
		]
	}`)
	ctx := ConvertCtx{ClientFormat: FormatClaude, TargetProto: ProtocolAnthropicMessages}
	decoded, _ := DecodeRequest(ProtocolOpenAIResponses, body, ctx)

	// 同线（responses→responses）必须**原样回写**，连字节都保留。
	sameCtx := ConvertCtx{ClientFormat: FormatResponse, TargetProto: ProtocolOpenAIResponses}
	sameWire, _ := EncodeRequest(ProtocolOpenAIResponses, decoded.Value, sameCtx)
	sameText := sameWire.Body.MarshalCompact()
	if !strings.Contains(sameText, `"type":"mcp_call"`) ||
		!strings.Contains(sameText, `"type":"web_search_call"`) {
		t.Fatalf("同线必须原样保留 MCP / web_search 项（否则同线往返也丢上下文）：%s", sameText)
	}
	for _, entry := range sameWire.Loss.Entries {
		if entry.Capability == LossMCPTool || entry.Capability == LossWebSearchTool {
			t.Fatalf("同线是 passthrough，不该记『丢失』：%+v", entry)
		}
	}

	// 跨线（→anthropic）无承载位：必须按**专用类别**记损，而不是一律 unknown_field。
	crossWire, _ := EncodeRequest(ProtocolAnthropicMessages, decoded.Value, ctx)
	assertLossEntry(t, crossWire.Loss, LossMCPTool, string(LossDropped), "mcp_call")
	assertLossEntry(t, crossWire.Loss, LossWebSearchTool, string(LossDropped), "web_search_call")
	for _, entry := range crossWire.Loss.Entries {
		if entry.Capability == LossUnknownField && strings.Contains(entry.Detail, "mcp") {
			t.Fatalf("MCP 项不得只记 unknown_field（不可归因）：%+v", entry)
		}
	}
}

func TestNonFunctionToolsPreservedOnSameWireAndAttributedCrossWire(t *testing.T) {
	body := mustParsePayload(t, `{
		"model":"deepseek-v4.1-flash","instructions":"助手",
		"input":[{"type":"message","role":"user","content":"天气"}],
		"tools":[
			{"type":"function","name":"get_weather","parameters":{"type":"object"}},
			{"type":"web_search"}
		]
	}`)
	sameCtx := ConvertCtx{ClientFormat: FormatResponse, TargetProto: ProtocolOpenAIResponses}
	decoded, _ := DecodeRequest(ProtocolOpenAIResponses, body, sameCtx)
	sameWire, _ := EncodeRequest(ProtocolOpenAIResponses, decoded.Value, sameCtx)
	tools, present := sameWire.Body.Get("tools")
	if !present || len(tools.Items()) != 2 {
		t.Fatalf("同线应回写 canonical + 保留下来的两个工具（顺序：canonical 在前）：%s",
			sameWire.Body.MarshalCompact())
	}
	first, _ := tools.Items()[0].Get("name")
	if name, _ := first.String(); name != "get_weather" {
		t.Fatalf("canonical 工具必须在前（Node: [...tools, ...extraTools]），实际首项名 %q", name)
	}
	second, _ := tools.Items()[1].Get("type")
	if typeName, _ := second.String(); typeName != "web_search" {
		t.Fatalf("保留下来的非 function 工具应接在其后，实际 %q", typeName)
	}

	// 跨线：chat 线无 web_search 承载位 → 记专用类别损失（而非静默丢弃）。
	chatCtx := ConvertCtx{ClientFormat: FormatOpenAI, TargetProto: ProtocolOpenAIChat}
	chatWire, _ := EncodeRequest(ProtocolOpenAIChat, decoded.Value, chatCtx)
	assertLossEntry(t, chatWire.Loss, LossWebSearchTool, string(LossDropped), "web_search")
}

// ——————————————————————————————————————————————————————————————
// 3. parallel_tool_calls ↔ disable_parallel_tool_use
// ——————————————————————————————————————————————————————————————

func TestAnthropicParallelToolCallsDecode(t *testing.T) {
	body := mustParsePayload(t, `{
		"model":"claude-sonnet-4-5","max_tokens":512,
		"messages":[{"role":"user","content":"hi"}],
		"tool_choice":{"type":"auto","disable_parallel_tool_use":true}
	}`)
	decoded, _ := DecodeRequest(ProtocolAnthropicMessages, body, ConvertCtx{})
	if decoded.Value.ParallelToolCalls == nil {
		t.Fatal("disable_parallel_tool_use=true 必须解出 parallel=false（取反）")
	}
	if *decoded.Value.ParallelToolCalls {
		t.Fatal("disable=true ⇒ parallel 必须是 false")
	}

	// 非布尔值：记损且视为未给（与 Node 的 UNKNOWN_FIELD 同口径）。
	bad := mustParsePayload(t, `{
		"model":"claude-sonnet-4-5","max_tokens":512,
		"messages":[{"role":"user","content":"hi"}],
		"tool_choice":{"type":"auto","disable_parallel_tool_use":"yes"}
	}`)
	badDecoded, _ := DecodeRequest(ProtocolAnthropicMessages, bad, ConvertCtx{})
	if badDecoded.Value.ParallelToolCalls != nil {
		t.Fatal("非布尔 disable 不得当作布尔承载")
	}
}

func TestAnthropicParallelToolCallsEncode(t *testing.T) {
	ctx := ConvertCtx{ClientFormat: FormatClaude, TargetProto: ProtocolAnthropicMessages}

	// parallel=false ⇒ 合成 `{type:auto, disable_parallel_tool_use:true}`。
	falseValue := false
	encoded, _ := EncodeRequest(ProtocolAnthropicMessages, &Request{
		Model:             "claude-sonnet-4-5",
		Items:             []Item{{Kind: ItemMessage, Role: "user", Blocks: []Block{textBlock("hi")}}},
		ParallelToolCalls: &falseValue,
	}, ctx)
	choice, present := encoded.Body.Get("tool_choice")
	if !present {
		t.Fatalf("parallel=false 且无 tool_choice 时应合成 tool_choice：%s", encoded.Body.MarshalCompact())
	}
	disable, _ := choice.Get("disable_parallel_tool_use")
	if value, _ := disable.Bool(); !value {
		t.Fatal("parallel=false ⇒ disable_parallel_tool_use 必须为 true")
	}

	// parallel=true 且无 tool_choice ⇒ 不输出（与 Node 同口径）。
	trueValue := true
	encodedTrue, _ := EncodeRequest(ProtocolAnthropicMessages, &Request{
		Model:             "claude-sonnet-4-5",
		Items:             []Item{{Kind: ItemMessage, Role: "user", Blocks: []Block{textBlock("hi")}}},
		ParallelToolCalls: &trueValue,
	}, ctx)
	if _, present := encodedTrue.Body.Get("tool_choice"); present {
		t.Fatalf("parallel=true 且无 tool_choice 时不得凭空造对象：%s", encodedTrue.Body.MarshalCompact())
	}

	// 已有 tool_choice ⇒ 在它上面显式写 take-reverse（true → false）。
	encodedChoice, _ := EncodeRequest(ProtocolAnthropicMessages, &Request{
		Model:             "claude-sonnet-4-5",
		Items:             []Item{{Kind: ItemMessage, Role: "user", Blocks: []Block{textBlock("hi")}}},
		ToolChoice:        &ToolChoice{Kind: ChoiceAuto},
		ParallelToolCalls: &trueValue,
	}, ctx)
	choice2, _ := encodedChoice.Body.Get("tool_choice")
	disable2, _ := choice2.Get("disable_parallel_tool_use")
	if value, present := disable2.Bool(); !present || value {
		t.Fatalf("parallel=true + 有 tool_choice ⇒ disable_parallel_tool_use 必须显式为 false：%s",
			encodedChoice.Body.MarshalCompact())
	}
}

// TestParallelToolCallsRoundTripAcrossLines 三线往返不丢并行开关。
func TestParallelToolCallsRoundTripAcrossLines(t *testing.T) {
	claudeBody := mustParsePayload(t, `{
		"model":"claude-sonnet-4-5","max_tokens":512,
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"name":"get_weather","input_schema":{"type":"object"}}],
		"tool_choice":{"type":"auto","disable_parallel_tool_use":true}
	}`)
	for _, target := range []WireProtocol{ProtocolOpenAIChat, ProtocolOpenAIResponses} {
		ctx := ConvertCtx{TargetProto: target, ClientFormat: clientFormatOfProtocol(target)}
		decoded, _ := DecodeRequest(ProtocolAnthropicMessages, claudeBody, ctx)
		encoded, _ := EncodeRequest(target, decoded.Value, ctx)
		parallel, present := encoded.Body.Get("parallel_tool_calls")
		if !present {
			t.Fatalf("%s 正文必须带 parallel_tool_calls：%s", target, encoded.Body.MarshalCompact())
		}
		if value, _ := parallel.Bool(); value {
			t.Fatalf("%s 的 parallel_tool_calls 应为 false（取反 disable=true）", target)
		}

		// 回程：该线 → anthropic，必须还原成 disable=true。
		backCtx := ConvertCtx{TargetProto: ProtocolAnthropicMessages, ClientFormat: FormatClaude}
		backDecoded, _ := DecodeRequest(target, encoded.Body, backCtx)
		backEncoded, _ := EncodeRequest(ProtocolAnthropicMessages, backDecoded.Value, backCtx)
		choice, present := backEncoded.Body.Get("tool_choice")
		if !present {
			t.Fatalf("回程应还原 tool_choice：%s", backEncoded.Body.MarshalCompact())
		}
		disable, _ := choice.Get("disable_parallel_tool_use")
		if value, present := disable.Bool(); !present || !value {
			t.Fatalf("回程 disable_parallel_tool_use 必须为 true：%s", backEncoded.Body.MarshalCompact())
		}
	}
}

// ——————————————————————————————————————————————————————————————
// 辅助
// ——————————————————————————————————————————————————————————————

// assertLossEntry 断言损失报告里存在一条「能力 + 动作 + detail 子串」都命中的记录。
func assertLossEntry(t *testing.T, report LossReport, capability, action, detailContains string) {
	t.Helper()
	for _, entry := range report.Entries {
		if entry.Capability != capability || string(entry.Action) != action {
			continue
		}
		if detailContains == "" || strings.Contains(entry.Detail, detailContains) {
			return
		}
	}
	t.Fatalf("未找到损失记录 capability=%s action=%s detail⊇%q；实际：%+v",
		capability, action, detailContains, report.Entries)
}
