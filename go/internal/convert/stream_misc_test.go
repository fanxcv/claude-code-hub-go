package convert

import (
	"strings"
	"testing"
)

// 本文件钉住两件事：流式引用（citations / annotations）的跨线承载，以及相邻思考块之间的分隔串。
//
// 期望值来源 = 三线协议硬约束 + Go 实现自证（非 Node 金标）：responses 的 annotation 事件名与
// 载荷字段、anthropic 的 citations_delta 形状、chat 的 delta.annotations 均为各自规范所定义。

// responsesAnnotationStream 造一条「正文 + 一条 annotation」的 Responses 流。
func responsesAnnotationStream() []string {
	events := append(responsesTextItem(0, "msg0", ""),
		responsesTextDeltaAt(0, "msg0", "正文"))
	events = append(events,
		[2]string{"response.output_text.annotation.added", `"item_id":"msg0","output_index":0,"content_index":0,"annotation":{"type":"url_citation","url":"https://example.com/a","title":"A","start_index":0,"end_index":2}`},
		responsesTextDone(0, "msg0", "正文"),
	)
	events = append(events, responsesCompletedWith(`[]`))
	return []string{responsesEventStream(events)}
}

// responsesAnnotationText 取出 Responses 客户端流里所有 annotation 事件载荷。
func responsesAnnotationText(t *testing.T, out string) []*Value {
	t.Helper()
	frames, _ := ParseSSEFrames(out)
	annotations := []*Value{}
	for _, frame := range frames {
		payload := parseFrameJSON(frame)
		if payload == nil {
			continue
		}
		if eventType, _ := stringField(payload, "type"); eventType != "response.output_text.annotation.added" {
			continue
		}
		if annotation := payload.ObjectField("annotation"); annotation != nil {
			annotations = append(annotations, annotation)
		}
	}
	return annotations
}

// TestResponsesAnnotationReachesChatClient 钉住条目 1 的第一条验收：
// responses 上游的 annotation 不得被 chat 客户端丢掉。
func TestResponsesAnnotationReachesChatClient(t *testing.T) {
	pipe, ok := NewStreamPipe(ProtocolOpenAIResponses, ProtocolOpenAIChat, ConvertCtx{
		ClientFormat: FormatOpenAI,
		TargetProto:  ProtocolOpenAIResponses,
		Stream:       true,
	})
	if !ok {
		t.Fatal("建管道失败：responses -> chat")
	}
	var out []byte
	for _, frame := range responsesAnnotationStream() {
		out = append(out, pipe.Push([]byte(frame))...)
	}
	out = append(out, pipe.Flush()...)

	frames, _ := ParseSSEFrames(string(out))
	annotations := []*Value{}
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
			annotations = append(annotations, delta.ArrayField("annotations")...)
		}
	}
	if len(annotations) != 1 {
		t.Fatalf("chat 客户端应收到 1 条 annotation，实际 %d 条：%q", len(annotations), string(out))
	}
	if url, _ := stringField(annotations[0], "url"); url != "https://example.com/a" {
		t.Fatalf("annotation 载荷被改写：%s", annotations[0].MarshalCompact())
	}
	if got := clientDialectText(t, ProtocolOpenAIChat, string(out)); got != "正文" {
		t.Fatalf("正文未交付：got %q", got)
	}
}

// TestResponsesAnnotationReachesResponsesClient 钉住同线透传：annotation 事件不丢、且块收尾时
// 写进 output_text 分片的 annotations。
func TestResponsesAnnotationReachesResponsesClient(t *testing.T) {
	pipe, ok := NewStreamPipe(ProtocolOpenAIResponses, ProtocolOpenAIResponses, ConvertCtx{
		ClientFormat: FormatResponse,
		TargetProto:  ProtocolOpenAIResponses,
		Stream:       true,
	})
	if !ok {
		t.Fatal("建管道失败：responses -> responses")
	}
	var out []byte
	for _, frame := range responsesAnnotationStream() {
		out = append(out, pipe.Push([]byte(frame))...)
	}
	out = append(out, pipe.Flush()...)
	if annotations := responsesAnnotationText(t, string(out)); len(annotations) != 1 {
		t.Fatalf("responses 客户端应收到 1 条 annotation 事件，实际 %d 条：%q", len(annotations), string(out))
	}
	if !strings.Contains(string(out), `"annotations":[{"type":"url_citation"`) {
		t.Fatalf("块收尾的分片未带上注解：%q", string(out))
	}
}

// anthropicCitationStream 造一条「正文 + 一条 citations_delta」的 Anthropic 流。
func anthropicCitationStream() []string {
	return []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[],\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"正文\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"citations_delta\",\"citation\":{\"type\":\"web_search_result_location\",\"url\":\"https://example.com/b\",\"title\":\"B\",\"cited_text\":\"正文\"}}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
}

// TestAnthropicCitationReachesResponsesClient 钉住条目 1 的第二条验收：
// anthropic 的 citations_delta 必须在 responses 客户端看到 annotation 事件。
func TestAnthropicCitationReachesResponsesClient(t *testing.T) {
	pipe, ok := NewStreamPipe(ProtocolAnthropicMessages, ProtocolOpenAIResponses, ConvertCtx{
		ClientFormat: FormatResponse,
		TargetProto:  ProtocolAnthropicMessages,
		Stream:       true,
	})
	if !ok {
		t.Fatal("建管道失败：anthropic -> responses")
	}
	var out []byte
	for _, frame := range anthropicCitationStream() {
		out = append(out, pipe.Push([]byte(frame))...)
	}
	out = append(out, pipe.Flush()...)

	annotations := responsesAnnotationText(t, string(out))
	if len(annotations) != 1 {
		t.Fatalf("responses 客户端应收到 1 条 annotation 事件，实际 %d 条：%q", len(annotations), string(out))
	}
	if url, _ := stringField(annotations[0], "url"); url != "https://example.com/b" {
		t.Fatalf("citation 载荷被改写：%s", annotations[0].MarshalCompact())
	}
}

// TestAnthropicCitationStaysOnAnthropicClient 钉住同线：citations_delta 原样发出、仍在块内。
func TestAnthropicCitationStaysOnAnthropicClient(t *testing.T) {
	pipe, ok := NewStreamPipe(ProtocolAnthropicMessages, ProtocolAnthropicMessages, ConvertCtx{
		ClientFormat: FormatClaude,
		TargetProto:  ProtocolAnthropicMessages,
		Stream:       true,
	})
	if !ok {
		t.Fatal("建管道失败：anthropic -> anthropic")
	}
	var out []byte
	for _, frame := range anthropicCitationStream() {
		out = append(out, pipe.Push([]byte(frame))...)
	}
	out = append(out, pipe.Flush()...)

	text := string(out)
	if !strings.Contains(text, `"type":"citations_delta"`) {
		t.Fatalf("anthropic 客户端未收到 citations_delta：%q", text)
	}
	if strings.Index(text, "citations_delta") > strings.Index(text, "content_block_stop") {
		t.Fatalf("citations_delta 必须在块内、stop 之前：%q", text)
	}
}

// reasoningSeparatorStream 造一条「两个思考块 + 一段正文」的 Anthropic 流。
func reasoningSeparatorStream() []string {
	return []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[],\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"第一段\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"第二段\"}}\n\n",
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
}

// TestReasoningBlocksSeparatedByBlankLine 钉住条目 2：两个思考块之间必须插 "\n\n"。
func TestReasoningBlocksSeparatedByBlankLine(t *testing.T) {
	for _, client := range []WireProtocol{ProtocolOpenAIChat, ProtocolAnthropicMessages, ProtocolOpenAIResponses} {
		t.Run(string(client), func(t *testing.T) {
			pipe, ok := NewStreamPipe(ProtocolAnthropicMessages, client, ConvertCtx{
				ClientFormat: clientFormatOfProtocol(client),
				TargetProto:  ProtocolAnthropicMessages,
				Stream:       true,
			})
			if !ok {
				t.Fatalf("建管道失败：anthropic -> %s", client)
			}
			var out []byte
			for _, frame := range reasoningSeparatorStream() {
				out = append(out, pipe.Push([]byte(frame))...)
			}
			out = append(out, pipe.Flush()...)

			reasoning := reasoningTextOf(t, client, string(out))
			if !strings.Contains(reasoning, "第一段\n\n第二段") {
				t.Fatalf("思考块之间缺少 \"\\n\\n\" 分隔：got %q\n原文：%q", reasoning, string(out))
			}
		})
	}
}

// reasoningTextOf 取出客户端流里的思考正文（三线各自的承载位）。
func reasoningTextOf(t *testing.T, client WireProtocol, out string) string {
	t.Helper()
	frames, _ := ParseSSEFrames(out)
	var builder []byte
	for _, frame := range frames {
		payload := parseFrameJSON(frame)
		if payload == nil {
			continue
		}
		switch client {
		case ProtocolOpenAIChat:
			for _, choice := range payload.ArrayField("choices") {
				delta := choice.ObjectField("delta")
				if delta == nil {
					continue
				}
				if reasoning, ok := stringField(delta, "reasoning_content"); ok {
					builder = append(builder, reasoning...)
				}
			}
		case ProtocolAnthropicMessages:
			delta := payload.ObjectField("delta")
			if thinking, ok := stringField(delta, "thinking"); ok {
				builder = append(builder, thinking...)
			}
		case ProtocolOpenAIResponses:
			if reasoning, ok := stringField(payload, "delta"); ok {
				if eventType, _ := stringField(payload, "type"); eventType == "response.reasoning_summary_text.delta" {
					builder = append(builder, reasoning...)
				}
			}
		}
	}
	return string(builder)
}
