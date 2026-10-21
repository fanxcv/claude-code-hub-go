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
func responsesAnnotationStream() string {
	events := append(responsesTextItem(0, "msg0", ""), responsesTextDeltaAt(0, "msg0", "正文"))
	events = append(events,
		[2]string{"response.output_text.annotation.added", `"item_id":"msg0","output_index":0,"content_index":0,"annotation":{"type":"url_citation","url":"https://example.com/a","title":"A","start_index":0,"end_index":2}`},
		responsesTextDone(0, "msg0", "正文"),
	)
	events = append(events, responsesCompletedWith(`[]`))
	return responsesEventStream(events)
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

// chatAnnotationsOf 取出 Chat 客户端流里所有 `delta.annotations` 成员。
func chatAnnotationsOf(t *testing.T, out string) []*Value {
	t.Helper()
	frames, _ := ParseSSEFrames(out)
	annotations := []*Value{}
	for _, frame := range frames {
		payload := parseFrameJSON(frame)
		if payload == nil {
			continue
		}
		for _, choice := range payload.ArrayField("choices") {
			if delta := choice.ObjectField("delta"); delta != nil {
				annotations = append(annotations, delta.ArrayField("annotations")...)
			}
		}
	}
	return annotations
}

// TestCitationsStreamResponsesToChat：responses 上游的 annotation 不得被 chat 客户端丢掉。
func TestCitationsStreamResponsesToChat(t *testing.T) {
	out := runCrossScene(t, ProtocolOpenAIResponses, ProtocolOpenAIChat, responsesAnnotationStream(), 0, false)

	annotations := chatAnnotationsOf(t, out)
	if len(annotations) != 1 {
		t.Fatalf("chat 客户端应收到 1 条 annotation，实际 %d 条：%q", len(annotations), out)
	}
	if url, _ := annotations[0].StringField("url"); url != "https://example.com/a" {
		t.Fatalf("annotation 载荷被改写：%s", annotations[0].MarshalCompact())
	}
	if got := clientDialectText(t, ProtocolOpenAIChat, out); got != "正文" {
		t.Fatalf("正文未交付：got %q", got)
	}
}

// TestCitationsStreamResponsesToResponses：同线透传：annotation 事件不丢，且块收尾时写进
// output_text 分片的 annotations。
func TestCitationsStreamResponsesToResponses(t *testing.T) {
	out := runCrossScene(t, ProtocolOpenAIResponses, ProtocolOpenAIResponses, responsesAnnotationStream(), 0, false)

	if annotations := responsesAnnotationText(t, out); len(annotations) != 1 {
		t.Fatalf("responses 客户端应收到 1 条 annotation 事件，实际 %d 条：%q", len(annotations), out)
	}
	if !strings.Contains(out, `"annotations":[{"type":"url_citation"`) {
		t.Fatalf("块收尾的分片未带上注解：%q", out)
	}
}

// anthropicCitationStream 造一条「正文 + 一条 citations_delta」的 Anthropic 流。
func anthropicCitationStream() string {
	return sseFrame("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":5,"output_tokens":1}}}`) +
		sseFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`) +
		sseFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"正文"}}`) +
		sseFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"citations_delta","citation":{"type":"web_search_result_location","url":"https://example.com/b","title":"B","cited_text":"正文"}}}`) +
		sseFrame("content_block_stop", `{"type":"content_block_stop","index":0}`) +
		sseFrame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`) +
		sseFrame("message_stop", `{"type":"message_stop"}`)
}

// TestCitationsStreamAnthropicToResponses：anthropic 的 citations_delta 必须让 responses 客户端
// 看到 annotation 事件。
func TestCitationsStreamAnthropicToResponses(t *testing.T) {
	out := runCrossScene(t, ProtocolAnthropicMessages, ProtocolOpenAIResponses, anthropicCitationStream(), 0, false)

	annotations := responsesAnnotationText(t, out)
	if len(annotations) != 1 {
		t.Fatalf("responses 客户端应收到 1 条 annotation 事件，实际 %d 条：%q", len(annotations), out)
	}
	if url, _ := annotations[0].StringField("url"); url != "https://example.com/b" {
		t.Fatalf("citation 载荷被改写：%s", annotations[0].MarshalCompact())
	}
}

// TestCitationsStreamAnthropicToAnthropic：同线：citations_delta 原样发出、仍在块内。
func TestCitationsStreamAnthropicToAnthropic(t *testing.T) {
	out := runCrossScene(t, ProtocolAnthropicMessages, ProtocolAnthropicMessages, anthropicCitationStream(), 0, false)

	if !strings.Contains(out, `"type":"citations_delta"`) {
		t.Fatalf("anthropic 客户端未收到 citations_delta：%q", out)
	}
	if strings.Index(out, "citations_delta") > strings.Index(out, "content_block_stop") {
		t.Fatalf("citations_delta 必须在块内、stop 之前：%q", out)
	}
}

// TestCitationsStreamAnthropicToChat：三线里 chat 是引用承载最弱的一条（message 级），
// 但同样不得丢——`delta.annotations` 是它唯一的出口。
func TestCitationsStreamAnthropicToChat(t *testing.T) {
	out := runCrossScene(t, ProtocolAnthropicMessages, ProtocolOpenAIChat, anthropicCitationStream(), 0, false)

	annotations := chatAnnotationsOf(t, out)
	if len(annotations) != 1 {
		t.Fatalf("chat 客户端应收到 1 条 annotation，实际 %d 条：%q", len(annotations), out)
	}
	if url, _ := annotations[0].StringField("url"); url != "https://example.com/b" {
		t.Fatalf("citation 载荷被改写：%s", annotations[0].MarshalCompact())
	}
}

// plainTextStream 按源线造一条**无引用**的文本流。
func plainTextStream(upstream WireProtocol) string {
	switch upstream {
	case ProtocolAnthropicMessages:
		return sseFrame("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":5,"output_tokens":1}}}`) +
			sseFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`) +
			sseFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"正文"}}`) +
			sseFrame("content_block_stop", `{"type":"content_block_stop","index":0}`) +
			sseFrame("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`) +
			sseFrame("message_stop", `{"type":"message_stop"}`)
	case ProtocolOpenAIChat:
		return sseFrame("", `{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"正文"}}]}`) +
			sseFrame("", `{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`) +
			"data: [DONE]\n\n"
	default:
		events := append(responsesTextItem(0, "msg0", ""), responsesTextDeltaAt(0, "msg0", "正文"))
		events = append(events, responsesTextDone(0, "msg0", "正文"))
		events = append(events, responsesCompletedWith(`[]`))
		return responsesEventStream(events)
	}
}

// TestCitationsStreamAbsentEmitsNone：无引用时三线都不得凭空多出引用帧，也不得写出非空
// annotations 数组——空数组同样会改动出站字节（上游/客户端会把它当作「本块有注解集」）。
func TestCitationsStreamAbsentEmitsNone(t *testing.T) {
	targets := []WireProtocol{ProtocolAnthropicMessages, ProtocolOpenAIChat, ProtocolOpenAIResponses}
	upstreams := []WireProtocol{ProtocolAnthropicMessages, ProtocolOpenAIChat, ProtocolOpenAIResponses}
	for _, upstream := range upstreams {
		for _, target := range targets {
			t.Run(string(upstream)+"-to-"+string(target), func(t *testing.T) {
				out := runCrossScene(t, upstream, target, plainTextStream(upstream), 0, false)
				for _, banned := range []string{"citations_delta", "annotation.added", `"annotations":[{`} {
					if strings.Contains(out, banned) {
						t.Fatalf("无引用时不应出现 %q：%q", banned, out)
					}
				}
				if got := clientDialectText(t, target, out); got != "正文" {
					t.Fatalf("正文未交付：got %q", got)
				}
			})
		}
	}
}

// reasoningSeparatorStream 造一条「两个思考块 + 一段正文」的 Anthropic 流。
func reasoningSeparatorStream() string {
	return sseFrame("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":5,"output_tokens":1}}}`) +
		sseFrame("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`) +
		sseFrame("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"第一段"}}`) +
		sseFrame("content_block_stop", `{"type":"content_block_stop","index":0}`) +
		sseFrame("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"thinking","thinking":""}}`) +
		sseFrame("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"thinking_delta","thinking":"第二段"}}`) +
		sseFrame("content_block_stop", `{"type":"content_block_stop","index":1}`) +
		sseFrame("message_stop", `{"type":"message_stop"}`)
}

// TestReasoningSeparatorBetweenThinkingBlocks：两个思考块之间必须插 "\n\n"——不插就粘成一句。
func TestReasoningSeparatorBetweenThinkingBlocks(t *testing.T) {
	for _, client := range []WireProtocol{ProtocolOpenAIChat, ProtocolAnthropicMessages, ProtocolOpenAIResponses} {
		t.Run(string(client), func(t *testing.T) {
			out := runCrossScene(t, ProtocolAnthropicMessages, client, reasoningSeparatorStream(), 0, false)

			reasoning := reasoningTextOf(t, client, out)
			if !strings.Contains(reasoning, "第一段\n\n第二段") {
				t.Fatalf("思考块之间缺少 \"\\n\\n\" 分隔：got %q\n原文：%q", reasoning, out)
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
				if delta := choice.ObjectField("delta"); delta != nil {
					if reasoning, ok := stringField(delta, "reasoning_content"); ok {
						builder = append(builder, reasoning...)
					}
				}
			}
		case ProtocolAnthropicMessages:
			if thinking, ok := stringField(payload.ObjectField("delta"), "thinking"); ok {
				builder = append(builder, thinking...)
			}
		case ProtocolOpenAIResponses:
			if eventType, _ := stringField(payload, "type"); eventType == "response.reasoning_summary_text.delta" {
				if reasoning, ok := stringField(payload, "delta"); ok {
					builder = append(builder, reasoning...)
				}
			}
		}
	}
	return string(builder)
}
