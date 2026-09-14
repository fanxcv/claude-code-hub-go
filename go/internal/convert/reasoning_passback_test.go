package convert

import (
	"strings"
	"testing"
)

// 本文件钉住「思考回传」这一条上游硬规则，来源是真上游二分实测
//	带 `tools` 且开启思考模式时，历史里**每个** assistant 轮次都必须带 `reasoning_content`；
//	缺失即 400 `[invalid_request_error] The reasoning_content in the thinking mode must be
//	passed back to the API.`
//
// 为什么必须钉：该 400 属 `forward.CategoryProviderError`，而 `CountsTowardCircuit` 为真
// （与 Node `errors.ts:557`「所有 4xx/5xx → 计入熔断器」同判）⇒ **我方转换形状的缺陷会被记成
// 供应商失败并推进熔断**。生产 2026-09-13 的证据：provider 138（OpenCode X Chat）的熔断末次
// 失败正是这一条 400，其 circuit-logs 里 21/50 条也是它。
//
// 修法见 `foldReasoningIntoFollowingAssistant`（codec_chat.go）：reasoning 项的思考并入紧随其后的
// assistant 消息，使 `reasoning_content` 落在**承载本轮输出**（正文或 tool_calls）的那条消息上。

// chatMessagesOf 把 responses 入站体转成 chat 出站体，并取出 messages 数组。
func chatMessagesOf(t *testing.T, body string) []*Value {
	t.Helper()
	source := mustParsePayload(t, body)
	ctx := ConvertCtx{
		ClientFormat:   FormatResponse,
		TargetProto:    ProtocolOpenAIChat,
		Model:          "deepseek-v4.1-flash",
		Stream:         false,
		ToWireToolName: NormalizeToolName,
	}
	decoded, ok := DecodeRequest(ProtocolOpenAIResponses, source, ctx)
	if !ok {
		t.Fatal("responses 线无解码器")
	}
	encoded, ok := EncodeRequest(ProtocolOpenAIChat, decoded.Value, ctx)
	if !ok {
		t.Fatal("chat 线无编码器")
	}
	parsed := mustParsePayload(t, encoded.Body.MarshalCompact())
	messages, present := parsed.Get("messages")
	if !present || messages == nil || !messages.IsArray() {
		t.Fatalf("出站体没有 messages 数组：%s", encoded.Body.MarshalCompact())
	}
	return messages.Items()
}

// assistantMessagesWithTools 返回所有「带 tool_calls 的 assistant 消息」。
func assistantMessagesWithTools(messages []*Value) []*Value {
	out := []*Value{}
	for _, message := range messages {
		if role, _ := stringField(message, "role"); role != "assistant" {
			continue
		}
		if _, hasTools := message.Get("tool_calls"); hasTools {
			out = append(out, message)
		}
	}
	return out
}

// TestReasoningPassbackOnToolCallTurn 是本缺陷的核心用例（生产形态）：
// 客户端走 `/v1/responses` 回传了 reasoning 项，其后是同一个 assistant 轮次的 function_call。
// 出站的**那条带 tool_calls 的消息必须同时带 reasoning_content**——否则上游按上面那条规则回 400。
func TestReasoningPassbackOnToolCallTurn(t *testing.T) {
	messages := chatMessagesOf(t, `{"model":"deepseek-v4.1-flash","stream":false,
"instructions":"You are Codex.",
"input":[
 {"type":"message","role":"user","content":[{"type":"input_text","text":"调用工具"}]},
 {"type":"reasoning","summary":[{"type":"summary_text","text":"我需要先读文件"}]},
 {"type":"function_call","call_id":"c1","name":"read_file","arguments":"{\"p\":\"a\"}"},
 {"type":"function_call_output","call_id":"c1","output":"内容"},
 {"type":"message","role":"user","content":[{"type":"input_text","text":"继续"}]}
],
"tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}}],
"reasoning":{"effort":"high"}}`)

	withTools := assistantMessagesWithTools(messages)
	if len(withTools) != 1 {
		t.Fatalf("应恰好一条带 tool_calls 的 assistant 消息，实际 %d 条", len(withTools))
	}
	reasoning, present := withTools[0].Get("reasoning_content")
	if !present || reasoning == nil {
		t.Fatalf("带 tool_calls 的 assistant 消息缺 reasoning_content——上游会回 400 "+
			"`reasoning_content must be passed back`；出站体：%v", dumpMessages(messages))
	}
	if text, isString := reasoning.String(); !isString || text != "我需要先读文件" {
		t.Fatalf("reasoning_content = %q（是字符串：%v），期望原样带回客户端的思考", text, isString)
	}
	// 思考**不得**再另发一条独立 assistant 消息：那正是本缺陷的成因（拆开后本轮输出那条就缺了）。
	for _, message := range messages {
		if role, _ := stringField(message, "role"); role != "assistant" {
			continue
		}
		if _, hasTools := message.Get("tool_calls"); hasTools {
			continue
		}
		if _, hasReasoning := message.Get("reasoning_content"); hasReasoning {
			t.Fatalf("思考仍被拆成独立 assistant 消息：%v", dumpMessages(messages))
		}
	}
}

// TestReasoningPassbackOnAssistantTextTurn 是同一规则的另一形态：reasoning 项后跟同轮的 assistant 正文。
// 出站应为**一条**消息：正文在 content、思考在 reasoning_content。
func TestReasoningPassbackOnAssistantTextTurn(t *testing.T) {
	messages := chatMessagesOf(t, `{"model":"deepseek-v4.1-flash","stream":false,
"instructions":"You are Codex.",
"input":[
 {"type":"message","role":"user","content":[{"type":"input_text","text":"你好"}]},
 {"type":"reasoning","summary":[{"type":"summary_text","text":"先想想"}]},
 {"type":"message","role":"assistant","content":[{"type":"output_text","text":"我是编码助手"}]},
 {"type":"message","role":"user","content":[{"type":"input_text","text":"继续"}]}
],
"tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}}]}`)

	assistants := []*Value{}
	for _, message := range messages {
		if role, _ := stringField(message, "role"); role == "assistant" {
			assistants = append(assistants, message)
		}
	}
	if len(assistants) != 1 {
		t.Fatalf("assistant 消息应为 1 条（思考并入本轮正文），实际 %d 条：%v",
			len(assistants), dumpMessages(messages))
	}
	content, _ := stringField(assistants[0], "content")
	reasoning, hasReasoning := assistants[0].Get("reasoning_content")
	if content != "我是编码助手" {
		t.Fatalf("content = %q，期望本轮正文（思考不得顶替正文）", content)
	}
	if !hasReasoning {
		t.Fatalf("缺 reasoning_content——上游会回 400：%v", dumpMessages(messages))
	}
	if text, _ := reasoning.String(); text != "先想想" {
		t.Fatalf("reasoning_content = %q，期望 %q", text, "先想想")
	}
}

// TestReasoningPassbackMultipleTurns 钉住「不串轮次」：两轮各自的思考落在各自那条消息上。
func TestReasoningPassbackMultipleTurns(t *testing.T) {
	messages := chatMessagesOf(t, `{"model":"deepseek-v4.1-flash","stream":false,
"instructions":"你是助手",
"input":[
 {"type":"message","role":"user","content":[{"type":"input_text","text":"读一下 a.txt"}]},
 {"type":"reasoning","summary":[{"type":"summary_text","text":"想了想要读文件"}]},
 {"type":"message","role":"assistant","content":[{"type":"output_text","text":"好的"}]},
 {"type":"reasoning","summary":[{"type":"summary_text","text":"再确认一次"}]},
 {"type":"message","role":"assistant","content":[{"type":"output_text","text":"读完了"}]}
],
"tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}}]}`)

	got := []string{}
	for _, message := range messages {
		if role, _ := stringField(message, "role"); role != "assistant" {
			continue
		}
		content, _ := stringField(message, "content")
		reasoningText := ""
		if reasoning, present := message.Get("reasoning_content"); present && reasoning != nil {
			reasoningText, _ = reasoning.String()
		}
		got = append(got, content+"|"+reasoningText)
	}
	want := []string{"好的|想了想要读文件", "读完了|再确认一次"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("轮次串位或丢失：got=%v want=%v", got, want)
	}
}

// TestReasoningStandaloneKeepsBothFields 钉住**未合并**的那条路径不回退：
// reasoning 项后面没有任何 assistant 输出时，仍按既有不变式「两处都写」产出——
// 既不能丢 reasoning_content（thinking 上游要求回传），也不能只写空 content
// （严格上游判空串为缺失，报 `Invalid assistant message: content or tool_calls`）。
func TestReasoningStandaloneKeepsBothFields(t *testing.T) {
	messages := chatMessagesOf(t, `{"model":"deepseek-v4.1-flash","stream":false,
"instructions":"你是助手",
"input":[
 {"type":"message","role":"user","content":[{"type":"input_text","text":"你好"}]},
 {"type":"reasoning","summary":[{"type":"summary_text","text":"先想想"}]}
],
"tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}}]}`)

	assistants := []*Value{}
	for _, message := range messages {
		if role, _ := stringField(message, "role"); role == "assistant" {
			assistants = append(assistants, message)
		}
	}
	if len(assistants) != 1 {
		t.Fatalf("应为 1 条 assistant 消息，实际 %d 条：%v", len(assistants), dumpMessages(messages))
	}
	if text, _ := assistants[0].Get("content"); text == nil {
		t.Fatal("content 缺失：严格上游会报 `Invalid assistant message: content or tool_calls`")
	}
	if _, present := assistants[0].Get("reasoning_content"); !present {
		t.Fatal("reasoning_content 缺失：thinking 上游会报 `reasoning_content must be passed back`")
	}
}

// TestReasoningAbsentIsNotFabricated 钉住边界（**有意不修**的那一半）：
// 客户端根本没回传思考时，我方不得凭空造 reasoning_content——那是伪造上游凭据，
// 且空串能否被上游接受未经验证（`content: ""` 被判为缺失的先例在先，见本文件抬头）。
// 该形态的上游 400 只能靠客户端回传思考消除，已在报告里登记为残余风险。
func TestReasoningAbsentIsNotFabricated(t *testing.T) {
	messages := chatMessagesOf(t, `{"model":"deepseek-v4.1-flash","stream":false,
"instructions":"You are Codex.",
"input":[
 {"type":"message","role":"user","content":[{"type":"input_text","text":"调用工具"}]},
 {"type":"function_call","call_id":"c1","name":"read_file","arguments":"{\"p\":\"a\"}"},
 {"type":"function_call_output","call_id":"c1","output":"内容"},
 {"type":"message","role":"user","content":[{"type":"input_text","text":"继续"}]}
],
"tools":[{"type":"function","name":"read_file","parameters":{"type":"object"}}]}`)

	for _, message := range messages {
		if _, present := message.Get("reasoning_content"); present {
			t.Fatalf("客户端未回传思考时不得伪造 reasoning_content：%v", dumpMessages(messages))
		}
	}
}

// dumpMessages 把消息数组渲染成便于阅读的一行（断言失败时用）。
func dumpMessages(messages []*Value) string {
	parts := make([]string, 0, len(messages))
	for _, message := range messages {
		parts = append(parts, message.MarshalCompact())
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
