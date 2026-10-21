package convert

import (
	"strings"
	"testing"
)

// TestAnthropicEmptyTextBlockBecomesEllipsis 钉住 R2：空 text 块顶替为 `...` 并记损。
//
// 为何：`{"type":"text","text":""}` 对 Anthropic 是空内容（整条消息无可见内容时被拒），
// `to_claude_messages_req.go:167` 在同位置用 `...` 顶替。
func TestAnthropicEmptyTextBlockBecomesEllipsis(t *testing.T) {
	body := mustParsePayload(t, `{"model":"gpt-4o","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":""},
		{"role":"user","content":"继续"}]}`)
	ctx := ConvertCtx{ClientFormat: FormatClaude, TargetProto: ProtocolAnthropicMessages}
	decoded, _ := DecodeRequest(ProtocolOpenAIChat, body, ctx)
	encoded, _ := EncodeRequest(ProtocolAnthropicMessages, decoded.Value, ctx)

	text := encoded.Body.MarshalCompact()
	if !strings.Contains(text, `{"type":"text","text":"..."}`) {
		t.Fatalf("空 text 块应顶替为 `...`：%s", text)
	}
	if strings.Contains(text, `"text":""`) {
		t.Fatalf("出站不得留空 text 块：%s", text)
	}
	assertLossEntry(t, encoded.Loss, LossUnknownFieldContent, string(LossRewritten), "content_part.text.empty")
}

// TestAnthropicFirstMessageGetsUserPlaceholder 钉住 R1：首条非 user 时插占位 user。
//
// 为何：Anthropic 要求首条消息为 user。chat 源里历史可以 assistant 开头（预填回复），
// 旧实现原样输出，上游直接 400；`to_claude_messages_req.go:208-213` 插一条 `"..."` 占位。
func TestAnthropicFirstMessageGetsUserPlaceholder(t *testing.T) {
	body := mustParsePayload(t, `{"model":"gpt-4o","messages":[
		{"role":"assistant","content":"好的"},
		{"role":"user","content":"继续"}]}`)
	ctx := ConvertCtx{ClientFormat: FormatClaude, TargetProto: ProtocolAnthropicMessages}
	decoded, _ := DecodeRequest(ProtocolOpenAIChat, body, ctx)
	encoded, _ := EncodeRequest(ProtocolAnthropicMessages, decoded.Value, ctx)

	messages, present := encoded.Body.Get("messages")
	if !present || !messages.IsArray() || len(messages.Items()) != 3 {
		t.Fatalf("应在原有两条前插一条占位 user（共 3 条）：%s", encoded.Body.MarshalCompact())
	}
	first := messages.Items()[0]
	if role, _ := stringField(first, "role"); role != "user" {
		t.Fatalf("首条必须是 user，实际 %q：%s", role, encoded.Body.MarshalCompact())
	}
	if !strings.Contains(first.MarshalCompact(), `"text":"..."`) {
		t.Fatalf("占位内容应为 `...`：%s", first.MarshalCompact())
	}
	assertLossEntry(t, encoded.Loss, LossUnknownFieldMessage, string(LossRewritten), "messages[0].role")

	// 首条已是 user 时不得插（否则每条请求都多一条噪声消息）。
	plain := mustParsePayload(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	plainDecoded, _ := DecodeRequest(ProtocolOpenAIChat, plain, ctx)
	plainEncoded, _ := EncodeRequest(ProtocolAnthropicMessages, plainDecoded.Value, ctx)
	plainMessages, _ := plainEncoded.Body.Get("messages")
	if len(plainMessages.Items()) != 1 {
		t.Fatalf("首条已是 user 时不得插占位：%s", plainEncoded.Body.MarshalCompact())
	}
}
