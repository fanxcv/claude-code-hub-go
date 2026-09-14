package convert

import (
	"encoding/json"
	"testing"
)

// 本文件钉住 chat 线出站 assistant 消息的**内容不变式**：不得产出
// 「既无正文、又无 tool_calls」的消息。
//
// 两类上游把它当成硬失败，且要求互斥：
//   - 严格上游把空串也算缺失（OpenCode 系「Console Go」实测）：
//     400 `Invalid assistant message: content or tool_calls`；
//   - thinking 模式的 chat 上游反向要求历史 assistant 轮次必须带回思考：
//     400 `reasoning_content must be passed back`（见 codec_responses.go 的说明）。
//
// 故「仅思考」的消息必须**两处都写**：content 取思考文本，reasoning_content 照旧保留。

// chatAssistantMessage 描述一条解析出来的 chat 出站消息，只保留断言需要的字段。
type outboundChatMessage struct {
	role             string
	contentIsNull    bool
	contentIsString  bool
	contentText      string
	contentParts     []map[string]any
	hasToolCalls     bool
	reasoningContent string
	hasReasoning     bool
}

func parseOutboundChatMessages(t *testing.T, raw string) []outboundChatMessage {
	t.Helper()
	var body struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("出站正文不是合法 JSON：%v\n%s", err, raw)
	}
	out := make([]outboundChatMessage, 0, len(body.Messages))
	for _, message := range body.Messages {
		entry := outboundChatMessage{}
		entry.role, _ = message["role"].(string)
		content, present := message["content"]
		switch value := content.(type) {
		case nil:
			entry.contentIsNull = present
		case string:
			entry.contentIsString = true
			entry.contentText = value
		case []any:
			for _, part := range value {
				if object, ok := part.(map[string]any); ok {
					entry.contentParts = append(entry.contentParts, object)
				}
			}
		}
		if calls, ok := message["tool_calls"].([]any); ok && len(calls) > 0 {
			entry.hasToolCalls = true
		}
		if reasoning, ok := message["reasoning_content"].(string); ok {
			entry.hasReasoning = true
			entry.reasoningContent = reasoning
		}
		out = append(out, entry)
	}
	return out
}

// assertNoEmptyAssistantMessage 断言每条 assistant 消息都有非空正文或 tool_calls。
func assertNoEmptyAssistantMessage(t *testing.T, label, raw string) {
	t.Helper()
	messages := parseOutboundChatMessages(t, raw)
	if len(messages) == 0 {
		t.Fatalf("%s：出站 messages 为空，用例本身失效", label)
	}
	for index, message := range messages {
		if message.role != "assistant" {
			continue
		}
		hasVisibleContent := message.contentIsString && message.contentText != ""
		if !message.contentIsString && len(message.contentParts) > 0 {
			hasVisibleContent = true
		}
		if hasVisibleContent || message.hasToolCalls {
			continue
		}
		t.Fatalf("%s：第 %d 条 assistant 消息既无正文又无 tool_calls（strict 上游会 400）：%s",
			label, index, raw)
	}
}

// TestChatOutboundAssistantNeverBothEmpty 覆盖四条入站线上会产出该畸形形状的输入。
func TestChatOutboundAssistantNeverBothEmpty(t *testing.T) {
	cases := []struct {
		label string
		proto WireProtocol
		from  ClientFormat
		raw   string
	}{
		{
			label: "anthropic：仅 thinking 块的 assistant",
			proto: ProtocolAnthropicMessages, from: FormatClaude,
			raw: `{"model":"m","max_tokens":64,"messages":[
				{"role":"user","content":"hi"},
				{"role":"assistant","content":[{"type":"thinking","thinking":"先想想","signature":"s"}]},
				{"role":"user","content":"再来"}]}`,
		},
		{
			label: "anthropic：仅空文本块的 assistant",
			proto: ProtocolAnthropicMessages, from: FormatClaude,
			raw: `{"model":"m","max_tokens":64,"messages":[
				{"role":"user","content":"hi"},
				{"role":"assistant","content":[{"type":"text","text":""}]},
				{"role":"user","content":"再来"}]}`,
		},
		{
			label: "responses：仅 reasoning 项（pi 的 /v1/responses 回放形状）",
			proto: ProtocolOpenAIResponses, from: FormatResponse,
			raw: `{"model":"m","input":[
				{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
				{"type":"reasoning","content":[{"type":"reasoning_text","text":"先想想"}]},
				{"type":"message","role":"user","content":[{"type":"input_text","text":"再来"}]}]}`,
		},
		{
			label: "responses：summary 字符串形态的 reasoning 项",
			proto: ProtocolOpenAIResponses, from: FormatResponse,
			raw: `{"model":"m","input":[
				{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
				{"type":"reasoning","summary":"先想想"},
				{"type":"message","role":"user","content":[{"type":"input_text","text":"再来"}]}]}`,
		},
		{
			label: "chat：assistant content 为空串",
			proto: ProtocolOpenAIChat, from: FormatOpenAI,
			raw: `{"model":"m","messages":[
				{"role":"user","content":"hi"},
				{"role":"assistant","content":""},
				{"role":"user","content":"再来"}]}`,
		},
		{
			label: "chat：assistant content 为纯空白",
			proto: ProtocolOpenAIChat, from: FormatOpenAI,
			raw: `{"model":"m","messages":[
				{"role":"user","content":"hi"},
				{"role":"assistant","content":"   "},
				{"role":"user","content":"再来"}]}`,
		},
		{
			// 同协议直通也要过同一条不变式：chat 入站的思考回放（只有 reasoning_content、无 content）
			// 同样会走到「无正文且无 tool_calls」的分支。
			label: "chat：仅 reasoning_content 的 assistant（同线直通）",
			proto: ProtocolOpenAIChat, from: FormatOpenAI,
			raw: `{"model":"m","messages":[
				{"role":"user","content":"hi"},
				{"role":"assistant","reasoning_content":"先想想"},
				{"role":"user","content":"再来"}]}`,
		},
		{
			label: "chat：assistant content 缺字段、只带空 tool_calls",
			proto: ProtocolOpenAIChat, from: FormatOpenAI,
			raw: `{"model":"m","messages":[
				{"role":"user","content":"hi"},
				{"role":"assistant","content":"","tool_calls":[]},
				{"role":"user","content":"再来"}]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			ctx := ConvertCtx{TargetProto: ProtocolOpenAIChat, ClientFormat: tc.from}
			decoded, _ := DecodeRequest(tc.proto, mustParsePayload(t, tc.raw), ctx)
			encoded, ok := EncodeRequest(ProtocolOpenAIChat, decoded.Value, ctx)
			if !ok {
				t.Fatal("chat 线无编码器")
			}
			got := encoded.Body.MarshalCompact()
			assertNoEmptyAssistantMessage(t, tc.label, got)
		})
	}
}

// TestChatThinkingOnlyAssistantCarriesReasoningBothPlaces 钉住「两处都写」这一取舍：
// 只丢掉 reasoning_content 会撞 `reasoning_content must be passed back`，
// 只写空 content 会撞 `Invalid assistant message: content or tool_calls`。
func TestChatThinkingOnlyAssistantCarriesReasoningBothPlaces(t *testing.T) {
	const thinking = "先查旧忆再动手"
	cases := []struct {
		label string
		proto WireProtocol
		from  ClientFormat
		raw   string
	}{
		{
			label: "anthropic 仅 thinking",
			proto: ProtocolAnthropicMessages, from: FormatClaude,
			raw: `{"model":"m","max_tokens":64,"messages":[
				{"role":"assistant","content":[{"type":"thinking","thinking":"先查旧忆再动手","signature":"s"}]},
				{"role":"user","content":"再来"}]}`,
		},
		{
			label: "responses 仅 reasoning",
			proto: ProtocolOpenAIResponses, from: FormatResponse,
			raw: `{"model":"m","input":[
				{"type":"reasoning","content":[{"type":"reasoning_text","text":"先查旧忆再动手"}]},
				{"type":"message","role":"user","content":[{"type":"input_text","text":"再来"}]}]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			ctx := ConvertCtx{TargetProto: ProtocolOpenAIChat, ClientFormat: tc.from}
			decoded, _ := DecodeRequest(tc.proto, mustParsePayload(t, tc.raw), ctx)
			encoded, _ := EncodeRequest(ProtocolOpenAIChat, decoded.Value, ctx)

			var assistant *outboundChatMessage
			messages := parseOutboundChatMessages(t, encoded.Body.MarshalCompact())
			for index := range messages {
				if messages[index].role == "assistant" {
					assistant = &messages[index]
					break
				}
			}
			if assistant == nil {
				t.Fatalf("仅思考的 assistant 轮次被整条丢掉了，上游拿不到 reasoning：%s",
					encoded.Body.MarshalCompact())
			}
			if !assistant.hasReasoning || assistant.reasoningContent != thinking {
				t.Fatalf("reasoning_content 必须原样带回（否则 `reasoning_content must be passed back`）：%s",
					encoded.Body.MarshalCompact())
			}
			if !assistant.contentIsString || assistant.contentText != thinking {
				t.Fatalf("content 必须非空（否则 `Invalid assistant message: content or tool_calls`）：%s",
					encoded.Body.MarshalCompact())
			}
			// 记损：这不是静默改写，LossReport 里必须能查到。
			found := false
			for _, entry := range encoded.Loss.Entries {
				if entry.Capability == LossAssistantContentEmpty && entry.Detail == "reasoning.as_content" {
					found = true
				}
			}
			if !found {
				t.Fatalf("把思考搬进 content 必须记损（类别 %s / detail reasoning.as_content），实际：%+v",
					LossAssistantContentEmpty, encoded.Loss.Entries)
			}
		})
	}
}

// TestChatBlankOnlyAssistantOmittedWithLoss 钉住另一半取舍：无正文、无工具、无思考的
// assistant 消息整条不产出（与 anthropic 侧 encodeAnthropicMessage 同口径），且记损可见。
func TestChatBlankOnlyAssistantOmittedWithLoss(t *testing.T) {
	ctx := ConvertCtx{TargetProto: ProtocolOpenAIChat, ClientFormat: FormatClaude}
	raw := `{"model":"m","max_tokens":64,"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[{"type":"text","text":""}]},
		{"role":"user","content":"再来"}]}`
	decoded, _ := DecodeRequest(ProtocolAnthropicMessages, mustParsePayload(t, raw), ctx)
	encoded, _ := EncodeRequest(ProtocolOpenAIChat, decoded.Value, ctx)

	messages := parseOutboundChatMessages(t, encoded.Body.MarshalCompact())
	if len(messages) != 2 {
		t.Fatalf("空 assistant 消息应整条不产出（期望 2 条），实际 %s", encoded.Body.MarshalCompact())
	}
	for _, message := range messages {
		if message.role == "assistant" {
			t.Fatalf("空 assistant 消息不得产出：%s", encoded.Body.MarshalCompact())
		}
	}
	found := false
	for _, entry := range encoded.Loss.Entries {
		if entry.Capability == LossAssistantContentEmpty && entry.Detail == "assistant.empty_message" {
			found = true
		}
	}
	if !found {
		t.Fatalf("省略空 assistant 消息必须记损（类别 %s / detail assistant.empty_message），实际：%+v",
			LossAssistantContentEmpty, encoded.Loss.Entries)
	}
}

// TestChatAssistantToolCallsKeepsNullContent 守住既有规则不被本次修改破坏：
// OpenAI 只在**带 tool_calls** 时允许 content 为 null；且「有正文 + 带 tool_calls」时正文优先，
// 不得被压成 null 而静默丢正文。
func TestChatAssistantToolCallsKeepsNullContent(t *testing.T) {
	cases := []struct {
		label       string
		blocks      []Block
		wantNull    bool
		wantContent string
	}{
		{
			label:    "仅 tool_call ⇒ content:null",
			blocks:   []Block{{Kind: BlockToolCall, ID: "c1", Name: "n", Args: "{}"}},
			wantNull: true,
		},
		{
			label: "正文 + tool_call ⇒ 正文优先",
			blocks: []Block{
				{Kind: BlockText, Text: "好的"},
				{Kind: BlockToolCall, ID: "c1", Name: "n", Args: "{}"},
			},
			wantContent: "好的",
		},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			ctx := ConvertCtx{TargetProto: ProtocolOpenAIChat, ClientFormat: FormatClaude}
			encoded, ok := EncodeRequest(ProtocolOpenAIChat, &Request{
				Model: "m",
				Items: []Item{{Kind: ItemMessage, Role: "assistant", Blocks: tc.blocks}},
			}, ctx)
			if !ok {
				t.Fatal("chat 线无编码器")
			}
			body := encoded.Body
			messages := parseOutboundChatMessages(t, body.MarshalCompact())
			if len(messages) != 1 {
				t.Fatalf("期望 1 条 assistant 消息，实际 %s", body.MarshalCompact())
			}
			message := messages[0]
			if !message.hasToolCalls {
				t.Fatalf("tool_calls 必须保留：%s", body.MarshalCompact())
			}
			if tc.wantNull {
				if !message.contentIsNull {
					t.Fatalf("仅 tool_call 时 content 必须是 null：%s", body.MarshalCompact())
				}
				return
			}
			if !message.contentIsString || message.contentText != tc.wantContent {
				t.Fatalf("有正文时必须优先保留正文（不得压成 null）：%s", body.MarshalCompact())
			}
		})
	}
}
