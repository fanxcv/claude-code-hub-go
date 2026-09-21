package convert

import (
	"fmt"
	"strings"
	"testing"
)

// 本文件钉住「**跨块整段回放**」这一缺陷类。
//
// 两条线的解码器都把「已发内容」按**块**记账，块边界一到就归零，于是随后到达的「整段回放」既无从
// 比对、又被当作新块的首帧原样下发：
//
//   - chat 线：tool_calls / thinking 帧关闭文本块时清 `emitted`，块后同形帧直接落到 newBlock 分支；
//   - responses 线：声明式收尾按 `response.completed` 的 **output[] 数组下标**反推块键
//     （见 recoverTerminalOutput），与流式期的 output_index 不一致时会建出 `emitted` 为空的新块，
//     声明全文被当成新内容补发；此外 tool item 关块之后到达的「累计全文」式增量也没有任何对账。
//
// 生产实测（v1.9.14 上线后 chat 线仍 8.2% 重复、92% 恰为 2x）即为此形态：
//
//	文本增量（多帧拼出全篇 T）→ tool 帧 → 单帧内容 == T
//
// 修复口径：消息级记账 `messageReplay`（见 stream.go）+ 两线入口处的守卫。判据只取最保守的一条
// ——与消息级已发全文**逐字相等**且该全文由**多帧**拼成即判回放；余下一律放行。
const (
	replayFullText = "PONG" // 全篇（由多帧拼成）
	replayLateText = "TAIL" // 与全篇不同的合法续写
)

// ---------------------------------------------------------------------------
// 上游线：chat
// ---------------------------------------------------------------------------

// chatUpstreamCrossBlockReplay 复现 chat 线的跨块回放：块边界之后重发整段。
func chatUpstreamCrossBlockReplay() string {
	return chatUpstreamFrames(`{"content":"PONG"}`)
}

// chatUpstreamCrossBlockLegit 的块边界之后是**不同内容**的合法第二段。
func chatUpstreamCrossBlockLegit() string {
	return chatUpstreamFrames(fmt.Sprintf(`{"content":%q}`, replayLateText))
}

func chatUpstreamFrames(afterTool string) string {
	var builder strings.Builder
	chunk := func(delta string) {
		builder.WriteString(sseFrame("", fmt.Sprintf(
			`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":%s}]}`, delta)))
	}
	chunk(`{"role":"assistant","content":""}`)
	chunk(`{"content":"PO"}`)
	chunk(`{"content":"NG"}`) // 多帧拼出全篇
	chunk(`{"tool_calls":[{"index":0,"id":"call_up1","type":"function","function":{"name":"mcp_weather_lookup","arguments":""}}]}`)
	chunk(`{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"sf\"}"}}]}`)
	chunk(afterTool) // 块边界之后
	builder.WriteString(sseFrame("", `{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`))
	builder.WriteString("data: [DONE]\n\n")
	return builder.String()
}

// ---------------------------------------------------------------------------
// 上游线：responses
// ---------------------------------------------------------------------------

// responsesUpstreamRecoverByArrayIndex 的末态 output[] 次序与流式期 output_index 不一致
// （流式期 message 在下标 0、function_call 在下标 1；末态 output[] 把两者颠倒了）。
// 这正是 assembleRecover 按数组下标反推块键时会撞上的形态。
func responsesUpstreamRecoverByArrayIndex() string {
	return responsesUpstreamReplay(responsesCompletedWith(`[` + replayFCItemJSON() + `,` + replayMsgItemJSON() + `]`))
}

// responsesUpstreamCumulativeDelta 在 tool item 关块之后再补一条「累计全文」式增量——
// 块级记账此时已归零，若不设消息级守卫就会整段二次下发。
func responsesUpstreamCumulativeDelta() string {
	return responsesUpstreamReplay(responsesCompletedWith(`[]`), responsesTextDelta(replayFullText))
}

// responsesUpstreamLegitLateText 的块边界之后是**不同内容**的合法第二段（不同 output_index）。
func responsesUpstreamLegitLateText() string {
	return responsesUpstreamReplay(responsesCompletedWith(`[]`),
		append(responsesTextItem(2, "msg2", replayLateText),
			responsesTextDone(2, "msg2", replayLateText))...)
}

func replayMsgItemJSON() string {
	return `{"id":"msg0","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"PONG"}]}`
}

func replayFCItemJSON() string {
	return `{"id":"fc1","type":"function_call","status":"completed","call_id":"call1","name":"mcp_weather_lookup","arguments":"{\"city\":\"sf\"}"}`
}

func responsesCompletedWith(output string) [2]string {
	return [2]string{"response.completed", `"response":{"id":"resp_up","object":"response","model":"m","status":"completed","output":` + output + `,"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}`}
}

func responsesTextItem(outputIndex int, itemID string, text string) [][2]string {
	return [][2]string{
		{"response.output_item.added", fmt.Sprintf(`"output_index":%d,"item":{"id":%q,"type":"message","role":"assistant","status":"in_progress","content":[]}`, outputIndex, itemID)},
		{"response.content_part.added", fmt.Sprintf(`"item_id":%q,"output_index":%d,"content_index":0,"part":{"type":"output_text","text":""}`, itemID, outputIndex)},
	}
}

func responsesTextDelta(text string) [2]string {
	return [2]string{"response.output_text.delta", fmt.Sprintf(`"item_id":"msg0","output_index":0,"content_index":0,"delta":%q`, text)}
}

func responsesTextDone(outputIndex int, itemID string, text string) [2]string {
	return [2]string{"response.output_text.done", fmt.Sprintf(`"item_id":%q,"output_index":%d,"content_index":0,"text":%q`, itemID, outputIndex, text)}
}

// responsesUpstreamReplay 拼出「文本增量 → tool item → （可选尾帧）→ 末态」的 responses 上行流。
func responsesUpstreamReplay(completed [2]string, afterTool ...[2]string) string {
	events := [][2]string{
		{"response.created", `"response":{"id":"resp_up","object":"response","model":"m","status":"in_progress","output":[]}`},
	}
	events = append(events, responsesTextItem(0, "msg0", replayFullText)...)
	events = append(events,
		responsesTextDelta("PO"),
		responsesTextDelta("NG"),
		responsesTextDone(0, "msg0", replayFullText),
		[2]string{"response.output_item.done", `"output_index":0,"item":` + replayMsgItemJSON()},
		[2]string{"response.output_item.added", `"output_index":1,"item":{"id":"fc1","type":"function_call","status":"in_progress","call_id":"call1","name":"mcp_weather_lookup","arguments":""}`},
		[2]string{"response.function_call_arguments.delta", `"item_id":"fc1","output_index":1,"delta":"{\"city\":\"sf\"}"`},
		[2]string{"response.function_call_arguments.done", `"item_id":"fc1","output_index":1,"arguments":"{\"city\":\"sf\"}"`},
		[2]string{"response.output_item.done", `"output_index":1,"item":` + replayFCItemJSON()},
	)
	events = append(events, afterTool...)
	events = append(events, completed)
	return responsesEventStream(events)
}

func responsesEventStream(events [][2]string) string {
	var builder strings.Builder
	sequence := 0
	for _, event := range events {
		payload := fmt.Sprintf(`{"type":%q,"sequence_number":%d`, event[0], sequence)
		if event[1] != "" {
			payload += "," + event[1]
		}
		payload += "}"
		builder.WriteString(sseFrame(event[0], payload))
		sequence++
	}
	return builder.String()
}

// ---------------------------------------------------------------------------
// 断言
// ---------------------------------------------------------------------------

func TestCrossBlockReplayIsDroppedOnChatUpstream(t *testing.T) {
	out := runCrossScene(t, ProtocolOpenAIChat, ProtocolOpenAIChat, chatUpstreamCrossBlockReplay(), 0, false)
	if got := chatTextOf(out); got != replayFullText {
		t.Fatalf("chat 线跨块回放未被消掉：客户端正文 = %q，期望 %q（恰一份）", got, replayFullText)
	}
}

func TestCrossBlockReplayIsDroppedOnResponsesUpstreamRecovery(t *testing.T) {
	out := runCrossScene(t, ProtocolOpenAIResponses, ProtocolOpenAIChat, responsesUpstreamRecoverByArrayIndex(), 0, false)
	if got := chatTextOf(out); got != replayFullText {
		t.Fatalf("responses 线末态对账按数组下标重建块导致回放：客户端正文 = %q，期望 %q", got, replayFullText)
	}
}

func TestCrossBlockReplayIsDroppedOnResponsesCumulativeDelta(t *testing.T) {
	out := runCrossScene(t, ProtocolOpenAIResponses, ProtocolOpenAIChat, responsesUpstreamCumulativeDelta(), 0, false)
	if got := chatTextOf(out); got != replayFullText {
		t.Fatalf("responses 线块后累计式增量未被消掉：客户端正文 = %q，期望 %q", got, replayFullText)
	}
}

func TestCrossBlockLegitSecondSegmentSurvives(t *testing.T) {
	cases := map[string]string{
		"chat 线":      chatUpstreamCrossBlockLegit(),
		"responses 线": responsesUpstreamLegitLateText(),
	}
	upstreams := map[string]WireProtocol{
		"chat 线":      ProtocolOpenAIChat,
		"responses 线": ProtocolOpenAIResponses,
	}
	for name, input := range cases {
		out := runCrossScene(t, upstreams[name], ProtocolOpenAIChat, input, 0, false)
		if got := chatTextOf(out); got != replayFullText+replayLateText {
			t.Fatalf("%s 的合法续写被误吞：客户端正文 = %q，期望 %q", name, got, replayFullText+replayLateText)
		}
	}
}

// chatTextOf 从客户端 chat SSE 里抽出所有 delta.content 的拼接。
func chatTextOf(sse string) string {
	var builder strings.Builder
	for _, line := range strings.Split(sse, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		body := strings.TrimPrefix(line, "data: ")
		if body == "[DONE]" {
			continue
		}
		payload := parseFrameJSON(SSEFrame{Data: body})
		if payload == nil {
			continue
		}
		choices := payload.ArrayField("choices")
		if len(choices) == 0 {
			continue
		}
		delta := choices[0].ObjectField("delta")
		if delta == nil {
			continue
		}
		if text, ok := stringField(delta, "content"); ok {
			builder.WriteString(text)
		}
	}
	return builder.String()
}
