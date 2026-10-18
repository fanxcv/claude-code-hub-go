package convert

import (
	"strings"
	"testing"
)

// responsesFailedStream 造一条「已交付正文、随后上游宣告失败」的 Responses 流。
func responsesFailedStream() []string {
	return []string{
		"event: response.created\n" +
			`data: {"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress","output":[]}}` + "\n\n",
		"event: response.output_item.added\n" +
			`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"i1","type":"message","role":"assistant","status":"in_progress","content":[]}}` + "\n\n",
		"event: response.content_part.added\n" +
			`data: {"type":"response.content_part.added","output_index":0,"content_index":0,"item_id":"i1","part":{"type":"output_text","text":""}}` + "\n\n",
		"event: response.output_text.delta\n" +
			`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"i1","delta":"半句正文"}` + "\n\n",
		"event: response.failed\n" +
			`data: {"type":"response.failed","response":{"id":"resp_1","model":"m","status":"failed","error":{"code":"server_error","message":"boom"}}}` + "\n\n",
	}
}

// TestResponsesFailedEmitsNoNormalTerminator 钉住 response.failed 不得被伪装成正常收尾。
//
// 失败流若照常补终止帧，客户端会认为响应完整、把半截正文当最终结果吞下（伪成功）。故三线都必须
// 不发终止事件，但已生成的正文仍要交付出去。
func TestResponsesFailedEmitsNoNormalTerminator(t *testing.T) {
	cases := []struct {
		client    WireProtocol
		forbidden []string
	}{
		// finish_reason 在中间分块里恒为 null，故只禁「非空取值」的形态
		{ProtocolOpenAIChat, []string{`"finish_reason":"`, "[DONE]"}},
		{ProtocolAnthropicMessages, []string{"message_stop", "message_delta"}},
		{ProtocolOpenAIResponses, []string{"response.completed", "response.incomplete"}},
	}
	for _, tc := range cases {
		t.Run(string(tc.client), func(t *testing.T) {
			pipe, ok := NewStreamPipe(ProtocolOpenAIResponses, tc.client, ConvertCtx{
				ClientFormat: clientFormatOfProtocol(tc.client),
				TargetProto:  ProtocolOpenAIResponses,
				Stream:       true,
			})
			if !ok {
				t.Fatalf("建管道失败：responses -> %s", tc.client)
			}
			var out []byte
			for _, frame := range responsesFailedStream() {
				out = append(out, pipe.Push([]byte(frame))...)
			}
			out = append(out, pipe.Flush()...)
			text := string(out)

			for _, forbidden := range tc.forbidden {
				if strings.Contains(text, forbidden) {
					t.Fatalf("失败流不得产出 %q：%q", forbidden, text)
				}
			}
			// 已生成的正文不能因失败而丢
			if got := clientDialectText(t, tc.client, text); got != "半句正文" {
				t.Fatalf("失败前的正文未交付：got %q\n原文：%q", got, text)
			}
		})
	}
}

// TestResponsesCompletedStillTerminatesNormally 反向钉住：正常 completed 流仍要产出终止帧。
func TestResponsesCompletedStillTerminatesNormally(t *testing.T) {
	frames := []string{
		"event: response.created\n" +
			`data: {"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress","output":[]}}` + "\n\n",
		"event: response.output_text.delta\n" +
			`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"i1","delta":"完整正文"}` + "\n\n",
		"event: response.completed\n" +
			`data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[],"usage":{"input_tokens":5,"output_tokens":3}}}` + "\n\n",
	}
	for _, client := range []WireProtocol{ProtocolOpenAIChat, ProtocolAnthropicMessages, ProtocolOpenAIResponses} {
		t.Run(string(client), func(t *testing.T) {
			pipe, ok := NewStreamPipe(ProtocolOpenAIResponses, client, ConvertCtx{
				ClientFormat: clientFormatOfProtocol(client),
				TargetProto:  ProtocolOpenAIResponses,
				Stream:       true,
			})
			if !ok {
				t.Fatalf("建管道失败：responses -> %s", client)
			}
			var out []byte
			for _, frame := range frames {
				out = append(out, pipe.Push([]byte(frame))...)
			}
			out = append(out, pipe.Flush()...)
			assertClientTerminator(t, client, string(out))
			if got := clientDialectText(t, client, string(out)); got != "完整正文" {
				t.Fatalf("正文 = %q", got)
			}
		})
	}
}

// TestResponsesCustomToolCallStreamsToolCall 钉住 custom_tool_call 的流式承载：它此前落
// output_item.added 的 default 分支，客户端只收到空 assistant，工具调用整条丢失。
func TestResponsesCustomToolCallStreamsToolCall(t *testing.T) {
	frames := []string{
		"event: response.created\n" +
			`data: {"type":"response.created","response":{"id":"resp_1","model":"m","status":"in_progress","output":[]}}` + "\n\n",
		"event: response.output_item.added\n" +
			`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"ctc_1","type":"custom_tool_call","call_id":"call_abc","name":"exec","status":"in_progress","input":""}}` + "\n\n",
		"event: response.custom_tool_call_input.delta\n" +
			`data: {"type":"response.custom_tool_call_input.delta","output_index":0,"item_id":"ctc_1","delta":"{\"cmd\":"}` + "\n\n",
		"event: response.custom_tool_call_input.delta\n" +
			`data: {"type":"response.custom_tool_call_input.delta","output_index":0,"item_id":"ctc_1","delta":"\"ls\"}"}` + "\n\n",
		"event: response.custom_tool_call_input.done\n" +
			`data: {"type":"response.custom_tool_call_input.done","output_index":0,"item_id":"ctc_1","input":"{\"cmd\":\"ls\"}"}` + "\n\n",
		"event: response.output_item.done\n" +
			`data: {"type":"response.output_item.done","output_index":0,"item":{"id":"ctc_1","type":"custom_tool_call","call_id":"call_abc","name":"exec","status":"completed","input":"{\"cmd\":\"ls\"}"}}` + "\n\n",
		"event: response.completed\n" +
			`data: {"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[{"id":"ctc_1","type":"custom_tool_call","call_id":"call_abc","name":"exec","status":"completed","input":"{\"cmd\":\"ls\"}"}],"usage":{"input_tokens":5,"output_tokens":3}}}` + "\n\n",
	}

	t.Run("chat 收到 tool_calls", func(t *testing.T) {
		out := pipeResponsesFrames(t, ProtocolOpenAIChat, frames)
		call := firstChatToolCall(t, out)
		if call == nil {
			t.Fatalf("chat 客户端未收到 tool_calls（空 assistant）：%q", out)
		}
		if name, _ := call.ObjectField("function").StringField("name"); name != "exec" {
			t.Fatalf("工具名 = %q", name)
		}
		if id, _ := call.StringField("id"); id != "call_abc" {
			t.Fatalf("调用 id = %q", id)
		}
	})

	t.Run("anthropic 收到 tool_use", func(t *testing.T) {
		out := pipeResponsesFrames(t, ProtocolAnthropicMessages, frames)
		if !strings.Contains(out, `"type":"tool_use"`) {
			t.Fatalf("anthropic 客户端未收到 tool_use：%q", out)
		}
		if !strings.Contains(out, `"name":"exec"`) {
			t.Fatalf("tool_use 缺工具名：%q", out)
		}
		if !strings.Contains(out, `input_json_delta`) {
			t.Fatalf("tool_use 缺参数增量：%q", out)
		}
	})
}

func pipeResponsesFrames(t *testing.T, client WireProtocol, frames []string) string {
	t.Helper()
	pipe, ok := NewStreamPipe(ProtocolOpenAIResponses, client, ConvertCtx{
		ClientFormat: clientFormatOfProtocol(client),
		TargetProto:  ProtocolOpenAIResponses,
		Stream:       true,
	})
	if !ok {
		t.Fatalf("建管道失败：responses -> %s", client)
	}
	var out []byte
	for _, frame := range frames {
		out = append(out, pipe.Push([]byte(frame))...)
	}
	out = append(out, pipe.Flush()...)
	return string(out)
}

// firstChatToolCall 返回首个 tool_calls 分块里的调用对象；没有则 nil。
func firstChatToolCall(t *testing.T, out string) *Value {
	t.Helper()
	frames, _ := ParseSSEFrames(out)
	for _, frame := range frames {
		payload := parseFrameJSON(frame)
		if payload == nil {
			continue
		}
		for _, choice := range payload.ArrayField("choices") {
			delta := choice.ObjectField("delta")
			if delta == nil {
				continue
			}
			if calls := delta.ArrayField("tool_calls"); len(calls) > 0 {
				return calls[0]
			}
		}
	}
	return nil
}
