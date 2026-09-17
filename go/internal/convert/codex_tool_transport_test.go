package convert

import (
	"strings"
	"testing"
)

// 本文件钉住四件由生产取证（2026-09-17，真实 Codex 请求体过 decode+encode）逼出来的改动：
//
//	1. `namespace` 工具组必须被展平（此前整组进 preserved，跨线时整组丢）；
//	2. input 里的 `additional_tools` item 也是工具声明（Codex 0.154 的新传输），必须并入 Tools，
//	   且不得产出消息项；
//	3. `tools[].strict` 只在 true 时算损失（Codex 给每个工具都写 false，此前每条都记一条「改写」）；
//	4. `reasoning.summary` 与加密思考块各归专用类别（此前挤在 catch-all，导致 rewrite 档被噪声填满）。

// codexNamespaceToolsBody 是「顶层 tools[] 里带 namespace 组」的请求体（实测形状，已剪裁）。
const codexNamespaceToolsBody = `{
  "model": "gpt-5.1-codex",
  "instructions": "s",
  "input": [{"type":"message","role":"user","content":"hi"}],
  "tools": [
    {"type":"function","name":"exec_command","description":"d","parameters":{"type":"object"},"strict":false},
    {"type":"namespace","name":"agents","description":"g","tools":[
      {"type":"function","name":"spawn_agent","description":"d","parameters":{"type":"object"},"strict":false}
    ]},
    {"type":"web_search"}
  ],
  "tool_choice": "auto"
}`

func decodeResponsesTo(t *testing.T, body string, target WireProtocol) (EncodeResult, LossReport) {
	t.Helper()
	ctx := ConvertCtx{
		ClientFormat:   FormatResponse,
		TargetProto:    target,
		Model:          "m",
		ToWireToolName: NormalizeToolName,
	}
	decoded, ok := DecodeRequest(ProtocolOpenAIResponses, mustParsePayload(t, body), ctx)
	if !ok {
		t.Fatal("responses 线必须能解码")
	}
	encoded, ok := EncodeRequest(target, decoded.Value, ctx)
	if !ok {
		t.Fatal("目标线必须能编码")
	}
	loss := decoded.Loss
	loss.Entries = append(loss.Entries, encoded.Loss.Entries...)
	return encoded, loss
}

// toolNames 取出出站正文里的工具名（chat 取 function.name，responses 取 name）。
func toolNames(t *testing.T, body *Value) []string {
	t.Helper()
	tools, present := body.Get("tools")
	if !present || tools == nil {
		return nil
	}
	out := []string{}
	for _, entry := range tools.Items() {
		if fn := fieldOrNil(entry, "function"); fn != nil {
			name, _ := stringField(fn, "name")
			out = append(out, name)
			continue
		}
		name, _ := stringField(entry, "name")
		out = append(out, name)
	}
	return out
}

func TestNamespaceToolsFlattenedForCrossLine(t *testing.T) {
	for _, target := range []WireProtocol{ProtocolOpenAIChat, ProtocolAnthropicMessages} {
		t.Run(string(target), func(t *testing.T) {
			encoded, _ := decodeResponsesTo(t, codexNamespaceToolsBody, target)
			names := strings.Join(toolNames(t, encoded.Body), ",")
			if !strings.Contains(names, "spawn_agent") {
				t.Fatalf("namespace 内的 function 工具必须被展平送出，实际工具：%q", names)
			}
			if !strings.Contains(names, "exec_command") {
				t.Fatalf("顶层 function 工具丢失，实际工具：%q", names)
			}
			// 展平不加前缀：客户端按裸名派发调用，加前缀会让回传的 function_call 名字对不上。
			if strings.Contains(names, "agents.spawn_agent") {
				t.Fatalf("展平不得加命名空间前缀，实际工具：%q", names)
			}
			// web_search 是非 function 工具，两条线都无承载位，必须留在 loss 台账里（不许静默丢）。
			assertLossEntry(t, mustLoss(t, codexNamespaceToolsBody, target), LossWebSearchTool, "dropped", "web_search")
		})
	}
}

// mustLoss 只要损失报告（上面几条断言用）。
func mustLoss(t *testing.T, body string, target WireProtocol) LossReport {
	t.Helper()
	_, loss := decodeResponsesTo(t, body, target)
	return loss
}

func TestAdditionalToolsItemMergedAndNotEmittedAsMessage(t *testing.T) {
	const body = `{
	  "model": "gpt-5.6-sol",
	  "input": [
	    {"type":"additional_tools","id":"at_1","role":"developer","tools":[
	      {"type":"namespace","name":"functions","description":"d","tools":[
	        {"type":"custom","name":"exec","description":"d","format":{"type":"grammar"}},
	        {"type":"function","name":"wait","description":"d","parameters":{"type":"object"},"strict":false}
	      ]}
	    ]},
	    {"type":"message","role":"user","content":"hi"}
	  ],
	  "tool_choice": "auto"
	}`
	encoded, loss := decodeResponsesTo(t, body, ProtocolOpenAIChat)
	names := strings.Join(toolNames(t, encoded.Body), ",")
	if !strings.Contains(names, "wait") {
		t.Fatalf("additional_tools 里的 function 工具必须并入出站 tools，实际：%q", names)
	}
	// 整项不得变成一条消息：它是工具声明，不是对话内容。
	messages, _ := encoded.Body.Get("messages")
	serialized := messages.MarshalCompact()
	if strings.Contains(serialized, "additional_tools") {
		t.Fatalf("additional_tools 不得作为消息内容出现：%s", serialized)
	}
	if count := strings.Count(serialized, `"role"`); count != 1 {
		t.Fatalf("只应产出 1 条消息（system 由 instructions 缺省），实际 %d 条：%s", count, serialized)
	}
	// custom 叶在 chat 线无承载位，必须记损（专用类别），不许静默丢。
	assertLossEntry(t, loss, LossNonFunctionTool, "dropped", "custom")
}

func TestToolStrictOnlyRecordedWhenTrue(t *testing.T) {
	// strict:false 是 Codex 的默认写法，等于两线缺省：既不写进上游、也不记损（否则每请求 +N 条噪声）。
	_, loss := decodeResponsesTo(t, codexNamespaceToolsBody, ProtocolOpenAIChat)
	for _, entry := range loss.Entries {
		if entry.Capability == LossUnknownFieldTool || entry.Capability == LossToolStrict {
			t.Fatalf("strict:false 不得记损，实际：%+v", entry)
		}
	}

	// strict:true 是客户端声明的硬约束：chat/responses 原样带上，anthropic 无承载位时记损。
	const strictBody = `{"model":"gpt-5","input":"hi","tools":[
	  {"type":"function","name":"f","parameters":{"type":"object"},"strict":true}]}`
	chat, _ := decodeResponsesTo(t, strictBody, ProtocolOpenAIChat)
	if !strings.Contains(chat.Body.MarshalCompact(), `"strict":true`) {
		t.Fatalf("chat 线必须原样带上 strict:true：%s", chat.Body.MarshalCompact())
	}
	responses, _ := decodeResponsesTo(t, strictBody, ProtocolOpenAIResponses)
	if !strings.Contains(responses.Body.MarshalCompact(), `"strict":true`) {
		t.Fatalf("responses 线必须原样带上 strict:true：%s", responses.Body.MarshalCompact())
	}
	assertLossEntry(t, mustLoss(t, strictBody, ProtocolAnthropicMessages), LossToolStrict, "dropped", "anthropic")
}

func TestReasoningSummaryAndEncryptedThinkingHaveDedicatedClasses(t *testing.T) {
	const body = `{
	  "model": "gpt-5",
	  "input": [
	    {"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"gAAAA-x"},
	    {"type":"message","role":"user","content":"hi"}
	  ],
	  "reasoning": {"effort":"high","summary":"auto"}
	}`
	_, loss := decodeResponsesTo(t, body, ProtocolOpenAIChat)
	// summary 是显示偏好（上游照旧回 reasoning_content），故走专用类别而非 catch-all。
	assertLossEntry(t, loss, LossReasoningSummary, "dropped", "chat_has_no_summary")
	// 密文思考走 thinking.encrypted，不再与「可读思考块被丢」混为一格。
	assertLossEntry(t, loss, LossThinkingEncrypted, "dropped", "redacted")

	// 档位：摘要 info、密文 degrade、可读思考块被丢仍是 rewrite（真内容损失）。
	if got := LossSeverityOf(LossReasoningSummary, LossDropped); got != SeverityInfo {
		t.Fatalf("reasoning.summary 应为 info 档，实际 %s", got)
	}
	if got := LossSeverityOf(LossThinkingEncrypted, LossDropped); got != SeverityDegrade {
		t.Fatalf("thinking.encrypted 应为 degrade 档，实际 %s", got)
	}
	if got := LossSeverityOf(LossThinkingBlock, LossDropped); got != SeverityRewrite {
		t.Fatalf("可读思考块被丢应为 rewrite 档，实际 %s", got)
	}
	if got := LossSeverityOf(LossToolStrict, LossDropped); got != SeverityDegrade {
		t.Fatalf("tool.strict 应为 degrade 档，实际 %s", got)
	}
}

// TestEncryptedThinkingTierKeepsBadgeHonest 是上面档位表的「为什么」：生产上 rewrite 档要能区分
// 「内容被改」与「密文无处安放」，否则列表徽章会被每请求几十条噪声填满（实测 1228/1238 行）。
func TestEncryptedThinkingTierKeepsBadgeHonest(t *testing.T) {
	report := mustLoss(t, `{"model":"gpt-5","input":[
	  {"type":"reasoning","id":"r1","summary":[],"encrypted_content":"x"},
	  {"type":"reasoning","id":"r2","summary":[],"encrypted_content":"y"},
	  {"type":"message","role":"user","content":"hi"}],
	  "reasoning":{"effort":"high","summary":"auto"}}`, ProtocolOpenAIChat)
	rewrite, degrade, info := 0, 0, 0
	for _, entry := range report.Entries {
		switch LossSeverityOf(entry.Capability, entry.Action) {
		case SeverityRewrite:
			rewrite++
		case SeverityDegrade:
			degrade++
		case SeverityInfo:
			info++
		}
	}
	if rewrite != 0 {
		t.Fatalf("这份请求没有内容损失，rewrite 档必须为 0（否则徽章即噪声），实际 %d，明细 %+v", rewrite, report.Entries)
	}
	if degrade != 2 || info != 1 {
		t.Fatalf("两块密文应计 degrade=2、摘要计 info=1，实际 degrade=%d info=%d", degrade, info)
	}
}

// TestCustomToolCallHistorySurvivesAsFunctionShapedPair 钉住 custom 工具历史的降级而非丢弃。
//
// 为何必须成对保留：custom_tool_call 与其 output 是一对——只保结果（role:"tool"）会变成孤儿工具
// 结果，严格上游直接判 400；只保调用则丢掉结果的文本。故两向都断言，并断言两者在出站正文里
// 仍然相邻（assistant(tool_calls) → tool）。
func TestCustomToolCallHistorySurvivesAsFunctionShapedPair(t *testing.T) {
	const body = `{
	  "model": "gpt-5.6-sol",
	  "input": [
	    {"type":"message","role":"user","content":"run it"},
	    {"type":"custom_tool_call","id":"ctc_1","call_id":"call_abc","name":"exec","input":"const r = await tools.exec_command({cmd:\"ls\"})"},
	    {"type":"custom_tool_call_output","id":"ctco_1","call_id":"call_abc","output":"a.txt\nb.txt"},
	    {"type":"message","role":"assistant","content":"done"}
	  ]
	}`
	encoded, loss := decodeResponsesTo(t, body, ProtocolOpenAIChat)
	serialized := encoded.Body.MarshalCompact()
	if strings.Contains(serialized, "custom_tool_call") {
		t.Fatalf("不得把 responses 专有的项类型原样写进 chat 出站：%s", serialized)
	}
	for _, want := range []string{"call_abc", "exec", "a.txt", "tools.exec_command"} {
		if !strings.Contains(serialized, want) {
			t.Fatalf("custom 工具历史丢了 %q：%s", want, serialized)
		}
	}
	// 归因要能看出来：这是「非 function 工具被改形」，不是静默通过。
	assertLossEntry(t, loss, LossNonFunctionTool, "rewritten", "custom_tool_call")

	// 形状与顺序：tool_calls 在 assistant 消息上，紧跟其后的是 role:"tool"。
	messages, _ := encoded.Body.Get("messages")
	roles := []string{}
	for _, message := range messages.Items() {
		role, _ := stringField(message, "role")
		roles = append(roles, role)
	}
	joined := strings.Join(roles, ",")
	if !strings.Contains(joined, "assistant,tool") {
		t.Fatalf("工具调用与其结果必须相邻（assistant(tool_calls) → tool），实际消息角色序列：%s", joined)
	}
}
