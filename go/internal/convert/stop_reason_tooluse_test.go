package convert

import (
	"strings"
	"testing"
)

// Anthropic 出口的 stop_reason 兜底：上游 chat / responses 线交付工具调用时，客户端必须拿到
// stop_reason=tool_use。改前本用例红——responses 上游的 status=completed 经
// stopReasonFromResponses 映射为 end_turn，而出口只做 stopReasonToAnthropic(response.StopReason)
// 直译；chat 上游的 finish_reason 可以缺席/为 stop 同理。客户端拿到 end_turn 会把工具结果当
// 普通收尾，不去执行工具（Anthropic SDK 按 stop_reason 决定是否走 tool 分支）。
//
// 非流式走 corpus 语料的两个 responses 用例（已登记为有意分歧）；此处覆盖 chat 上游，
// 并补上流式一面（流式在 message_delta 里携带 stop_reason）。

func anthropicStopReasonOf(t *testing.T, response string) string {
	t.Helper()
	value := mustParsePayload(t, response)
	reason, present := value.Get("stop_reason")
	if !present || reason == nil {
		t.Fatalf("响应缺 stop_reason：%s", response)
	}
	text, _ := reason.String()
	return text
}

func streamAnthropicStopReasonOf(t *testing.T, stream string) string {
	t.Helper()
	frames := parseOutFrames(t, stream)
	for _, item := range frames {
		if event, _ := stringField(item.data, "type"); event != "message_delta" {
			continue
		}
		delta := item.data.ObjectField("delta")
		if reason, ok := stringField(delta, "stop_reason"); ok && reason != "" {
			return reason
		}
	}
	t.Fatalf("Anthropic 流式输出缺带 stop_reason 的 message_delta：%s", stream)
	return ""
}

// TestAnthropicStopReasonForForeignToolCall 覆盖 chat / responses 上游 × 非流式 / 流式。
func TestAnthropicStopReasonForForeignToolCall(t *testing.T) {
	const chatUpstreamResponse = `{"id":"chatcmpl-1","object":"chat.completion","model":"m",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":null,` +
		`"tool_calls":[{"id":"call_9","type":"function","function":{"name":"f","arguments":"{\"x\":2}"}}]},` +
		`"finish_reason":"stop"}]}`

	const responsesUpstreamResponse = `{"id":"resp_1","object":"response","model":"m","status":"completed",` +
		`"output":[{"type":"function_call","call_id":"call_1","name":"f","arguments":"{\"x\":2}"}]}`

	t.Run("chat-to-anthropic/non-stream", func(t *testing.T) {
		ctx := ConvertCtx{ClientFormat: FormatClaude, TargetProto: ProtocolOpenAIChat}
		decoded, ok := DecodeResponse(ProtocolOpenAIChat, mustParsePayload(t, chatUpstreamResponse), ctx)
		if !ok {
			t.Fatal("chat 线无响应解码器")
		}
		encoded, ok := EncodeResponse(ProtocolAnthropicMessages, decoded.Value, ctx)
		if !ok {
			t.Fatal("anthropic 线无响应编码器")
		}
		if reason := anthropicStopReasonOf(t, encoded.Body.MarshalCompact()); reason != "tool_use" {
			t.Fatalf("chat 上游交付 tool_calls 时 Anthropic 客户端应收到 stop_reason=tool_use，实为 %q", reason)
		}
	})

	t.Run("responses-to-anthropic/non-stream", func(t *testing.T) {
		ctx := ConvertCtx{ClientFormat: FormatClaude, TargetProto: ProtocolOpenAIResponses}
		decoded, ok := DecodeResponse(ProtocolOpenAIResponses, mustParsePayload(t, responsesUpstreamResponse), ctx)
		if !ok {
			t.Fatal("responses 线无响应解码器")
		}
		encoded, ok := EncodeResponse(ProtocolAnthropicMessages, decoded.Value, ctx)
		if !ok {
			t.Fatal("anthropic 线无响应编码器")
		}
		if reason := anthropicStopReasonOf(t, encoded.Body.MarshalCompact()); reason != "tool_use" {
			t.Fatalf("responses 上游交付 function_call 时 Anthropic 客户端应收到 stop_reason=tool_use，实为 %q", reason)
		}
	})

	t.Run("chat-to-anthropic/stream", func(t *testing.T) {
		input := sseFrame("", `{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":null}}]}`) +
			sseFrame("", `{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_9","type":"function","function":{"name":"f","arguments":"{\"x\":2}"}}]}}]}`) +
			sseFrame("", `{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
			"data: [DONE]\n\n"
		out := runCrossScene(t, ProtocolOpenAIChat, ProtocolAnthropicMessages, input, 0, false)
		if reason := streamAnthropicStopReasonOf(t, out); reason != "tool_use" {
			t.Fatalf("chat 上游流式交付 tool_calls 时 Anthropic 客户端应收到 stop_reason=tool_use，实为 %q\n%s", reason, out)
		}
	})

	t.Run("responses-to-anthropic/stream", func(t *testing.T) {
		input := sseFrame("response.output_item.added", `{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"id":"fc_1","type":"function_call","status":"in_progress","call_id":"call_1","name":"f","arguments":""}}`) +
			sseFrame("response.function_call_arguments.done", `{"type":"response.function_call_arguments.done","sequence_number":1,"item_id":"fc_1","output_index":0,"arguments":"{\"x\":2}"}`) +
			sseFrame("response.output_item.done", `{"type":"response.output_item.done","sequence_number":2,"output_index":0,"item":{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_1","name":"f","arguments":"{\"x\":2}"}}`) +
			sseFrame("response.completed", `{"type":"response.completed","sequence_number":3,"response":{"id":"resp_1","object":"response","model":"m","status":"completed","output":[]}}`)
		out := runCrossScene(t, ProtocolOpenAIResponses, ProtocolAnthropicMessages, input, 0, false)
		if reason := streamAnthropicStopReasonOf(t, out); reason != "tool_use" {
			t.Fatalf("responses 上游流式交付 function_call 时 Anthropic 客户端应收到 stop_reason=tool_use，实为 %q\n%s", reason, out)
		}
	})
}

// TestAnthropicStopReasonUnchangedWithoutToolCall 钉住兜底不外溢：无工具块时 end_turn 照旧。
func TestAnthropicStopReasonUnchangedWithoutToolCall(t *testing.T) {
	ctx := ConvertCtx{ClientFormat: FormatClaude, TargetProto: ProtocolAnthropicMessages}
	response := &Response{
		ID: "msg_1", Model: "m",
		Blocks:     []Block{textBlock("你好")},
		StopReason: StopEndTurn,
	}
	encoded, ok := EncodeResponse(ProtocolAnthropicMessages, response, ctx)
	if !ok {
		t.Fatal("anthropic 线无响应编码器")
	}
	body := encoded.Body.MarshalCompact()
	if reason := anthropicStopReasonOf(t, body); reason != "end_turn" {
		t.Fatalf("无工具块时应保持 end_turn，实为 %q", reason)
	}
	if strings.Contains(body, "tool_use") {
		t.Fatalf("无工具块时不应出现 tool_use：%s", body)
	}
}
