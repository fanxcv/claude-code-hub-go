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
	return replayMsgItemJSONWith(replayFullText)
}

func replayMsgItemJSONWith(text string) string {
	return fmt.Sprintf(`{"id":"msg0","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":%q}]}`, text)
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
	return responsesTextDeltaAt(0, "msg0", text)
}

func responsesTextDeltaAt(outputIndex int, itemID string, delta string) [2]string {
	return [2]string{"response.output_text.delta", fmt.Sprintf(
		`"item_id":%q,"output_index":%d,"content_index":0,"delta":%q`, itemID, outputIndex, delta)}
}

func responsesTextDone(outputIndex int, itemID string, text string) [2]string {
	return [2]string{"response.output_text.done", fmt.Sprintf(`"item_id":%q,"output_index":%d,"content_index":0,"text":%q`, itemID, outputIndex, text)}
}

// ---------------------------------------------------------------------------
// 上游线：responses 的其余几条回放入口
// ---------------------------------------------------------------------------
//
// 487f662 把判据提到了消息级，但守卫只加在 `handleDelta` 与 `reconcile` 两处**入口**。
// responses 线交付正文的**出口**只有一个（`emitDeltaChunk`），入口却不止那两个：
//
//   - `handleDone` 的「上游跳过 added/delta 直接给 done」兜底（findBlock 查不到块时）；
//   - `closeBlock` / `flushSeed` 的 seed 兜底（`content_part.added` 的 part.text 就是全篇）；
//   - `closeBlock` / `releaseSuspended` 的 pending 兜底。
//
// 这些兜底新建出来的块 `emitted` 必然为空，块级记账比不到；于是整段回放被原样透传。
// 下面四条形状逐一钉这几条路，长度取生产残留样本的长度（30 / 73 / 65 / 24）。

// replayTextOfLength 造一段长度恰为 n 的正文（内容无关紧要，取长度只为与生产样本对读）。
func replayTextOfLength(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	var builder strings.Builder
	for i := 0; i < n; i++ {
		builder.WriteByte(alphabet[i%len(alphabet)])
	}
	return builder.String()
}

// responsesTextDeltaFrames 把全篇按 1–2 字符切片发成增量帧，与生产样本的形态一致
// （每帧 1–2 字符，故「已发全文由多帧拼成」这一前置条件成立）。
func responsesTextDeltaFrames(outputIndex int, itemID string, text string) [][2]string {
	frames := make([][2]string, 0, len(text))
	for start, size := 0, 0; start < len(text); size++ {
		width := 1 + size%2
		if start+width > len(text) {
			width = len(text) - start
		}
		frames = append(frames, [2]string{"response.output_text.delta", fmt.Sprintf(
			`"item_id":%q,"output_index":%d,"content_index":0,"delta":%q`, itemID, outputIndex, text[start:start+width])})
		start += width
	}
	return frames
}

// responsesPartAdded / responsesPartDone 造 content_part 的起止帧，part.text 由调用方给
// （`responsesTextItem` 把 part.text 写死为空串，覆盖不了「part.text 里就是全篇」这类上游）。
func responsesPartAdded(outputIndex int, itemID string, partText string) [2]string {
	return [2]string{"response.content_part.added", fmt.Sprintf(
		`"item_id":%q,"output_index":%d,"content_index":0,"part":{"type":"output_text","text":%q}`, itemID, outputIndex, partText)}
}

func responsesPartDone(outputIndex int, itemID string, partText string) [2]string {
	return [2]string{"response.content_part.done", fmt.Sprintf(
		`"item_id":%q,"output_index":%d,"content_index":0,"part":{"type":"output_text","text":%q}`, itemID, outputIndex, partText)}
}

// responsesUpstreamIndexShifted 复现生产那条链的具体触发条件：推理 item 占 `output_index` 0、
// 正文 item 在 1；而收尾的 `response.output_text.done` **不带 `output_index`**（默认落 0），
// `findBlock` 于是查不到流式期建出的块键 `part:1:0`（该块又已被 item.done 关闭、不在 openBlocks），
// 落到 `handleDone` 的「跳过 added/delta 直接给 done」兜底——块级记账在新块上必然为空。
func responsesUpstreamIndexShifted(text string) string {
	events := [][2]string{
		{"response.created", `"response":{"id":"resp_up","object":"response","model":"m","status":"in_progress","output":[]}`},
		{"response.output_item.added", `"output_index":0,"item":{"id":"rs0","type":"reasoning","summary":[]}`},
		{"response.reasoning_summary_text.delta", `"item_id":"rs0","output_index":0,"summary_index":0,"delta":"think"`},
		{"response.output_item.done", `"output_index":0,"item":{"id":"rs0","type":"reasoning","summary":[{"type":"summary_text","text":"think"}]}`},
	}
	events = append(events, responsesTextItem(1, "msg1", text)...)
	events = append(events, responsesTextDeltaFrames(1, "msg1", text)...)
	events = append(events,
		responsesTextDone(1, "msg1", text),
		[2]string{"response.output_item.done", fmt.Sprintf(`"output_index":1,"item":{"id":"msg1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":%q}]}`, text)},
		[2]string{"response.output_item.added", `"output_index":2,"item":{"id":"fc1","type":"function_call","status":"in_progress","call_id":"call1","name":"mcp_weather_lookup","arguments":""}`},
		[2]string{"response.function_call_arguments.delta", `"item_id":"fc1","output_index":2,"delta":"{\"city\":\"sf\"}"`},
		[2]string{"response.output_text.done", fmt.Sprintf(`"item_id":"msg1","text":%q`, text)},
	)
	events = append(events, responsesCompletedWith(`[]`))
	return responsesEventStream(events)
}

// TestResponsesCrossBlockReplayShapes 钉住 responses 线**其余几条回放入口**：
// 文本增量（多帧拼出全篇）→ tool item → 块边界之后，上游把全篇原样再给一次 → 末态。
//
// 两条断言：
//   - 客户端可见正文恰为一份（含该行声明的合法续写）；
//   - 首个工具帧之后的正文恰为 wantAfterTool——回放帧若被放行，这里就多出全篇。
func TestResponsesCrossBlockReplayShapes(t *testing.T) {
	text30 := replayTextOfLength(30)
	text73 := replayTextOfLength(73)
	text65 := replayTextOfLength(65)
	text24 := replayTextOfLength(24)

	cases := []struct {
		name          string
		upstream      string
		wantText      string
		wantAfterTool string
	}{
		{
			name:     "done 载荷缺 output_index（推理占 0、正文在 1）⇒ handleDone 兜底",
			upstream: responsesUpstreamIndexShifted(text30),
			// 块边界之后一字都不该有：这一份就是全篇本身。
			wantText: text30, wantAfterTool: "",
		},
		{
			name:     "done 载荷指向全新 item 键 ⇒ handleDone 兜底",
			upstream: responsesUpstreamTextScene(text73, responsesCompletedWith(`[]`), responsesTextDone(2, "msgX", text73)),
			wantText: text73, wantAfterTool: "",
		},
		{
			name: "content_part.added/done 的 part.text 就是全篇 ⇒ closeBlock 的 seed 兜底",
			upstream: responsesUpstreamTextScene(text65, responsesCompletedWith(`[]`),
				responsesPartAdded(2, "msg2", text65), responsesPartDone(2, "msg2", text65)),
			wantText: text65, wantAfterTool: "",
		},
		{
			name: "content_part.added 的 part.text 是全篇 + 一份合法续写 ⇒ flushSeed 的 seed 兜底",
			upstream: responsesUpstreamTextScene(text24, responsesCompletedWith(`[]`),
				responsesPartAdded(2, "msg2", text24), responsesTextDeltaAt(2, "msg2", "Z")),
			// 续写要活下来，而它前面那份全篇副本必须被丢掉。
			wantText: text24 + "Z", wantAfterTool: "Z",
		},
		{
			name: "块边界之后的 part.text 是**不同内容**的合法第二段",
			upstream: responsesUpstreamTextScene(replayFullText, responsesCompletedWith(`[]`),
				responsesPartAdded(2, "msg2", replayLateText), responsesPartDone(2, "msg2", replayLateText)),
			wantText: replayFullText + replayLateText, wantAfterTool: replayLateText,
		},
		{
			name:     "迟到的声明指向已关的旧块（差额已在别的块里发过）⇒ reconcile 的消息级守卫",
			upstream: responsesUpstreamLateDeclaredTail(),
			wantText: "ABCD", wantAfterTool: "CD",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := runCrossScene(t, ProtocolOpenAIResponses, ProtocolOpenAIChat, tc.upstream, 0, false)
			if got := chatTextOf(out); got != tc.wantText {
				t.Fatalf("%s：客户端正文 = %q，期望 %q", tc.name, got, tc.wantText)
			}
			if got := chatTextAfterFirstTool(out); got != tc.wantAfterTool {
				t.Fatalf("%s：首个工具帧之后的正文 = %q，期望 %q（跨块整段回放未被消掉）", tc.name, got, tc.wantAfterTool)
			}
		})
	}
}

// responsesUpstreamLateDeclaredTail 行的是**声明路径的兄弟口子**：声明式全文晚于「同一条 item 的块已关、
// 剩余正文由另一块补上」而来。此时块级基线（`emitted` = 该块已发的 "AB"）只是消息级的**前缀**，
// 按 `declaredTextTail` 会算出差额 "CD" 并再发一次（而 "CD" 已在另一块里发过）。
// 拦住它的只能是 `reconcile` 的消息级守卫（出口守卫比不到"差额恰好等于已发尾部"）。
func responsesUpstreamLateDeclaredTail() string {
	events := [][2]string{
		{"response.created", `"response":{"id":"resp_up","object":"response","model":"m","status":"in_progress","output":[]}`},
		{"response.output_item.added", `"output_index":0,"item":{"id":"msg0","type":"message","role":"assistant","status":"in_progress","content":[]}`},
		responsesTextDeltaAt(0, "msg0", "A"),
		responsesTextDeltaAt(0, "msg0", "B"),
		[2]string{"response.output_item.done", `"output_index":0,"item":` + replayMsgItemJSONWith("AB")},
		[2]string{"response.output_item.added", `"output_index":1,"item":{"id":"fc1","type":"function_call","status":"in_progress","call_id":"call1","name":"mcp_weather_lookup","arguments":""}`},
		[2]string{"response.function_call_arguments.delta", `"item_id":"fc1","output_index":1,"delta":"{\"city\":\"sf\"}"`},
		// 剩下的 "CD" 由另一个块补上：消息级已发全文于是变成 "ABCD"。
		responsesTextDeltaAt(2, "msg2", "CD"),
		// 迟到的声明：全篇 "ABCD" 指向**已关**的那个只发过 "AB" 的块。
		responsesTextDone(0, "msg0", "ABCD"),
	}
	events = append(events, responsesCompletedWith(`[]`))
	return responsesEventStream(events)
}

// responsesUpstreamReplay 拼出「文本增量 → tool item → （可选尾帧）→ 末态」的 responses 上行流，
// 正文取全篇 replayFullText。
func responsesUpstreamReplay(completed [2]string, afterTool ...[2]string) string {
	return responsesUpstreamTextScene(replayFullText, completed, afterTool...)
}

// responsesUpstreamTextScene 同上，但正文长度由调用方给（生产样本的 30 / 73 / 65 / 24 字节）。
func responsesUpstreamTextScene(text string, completed [2]string, afterTool ...[2]string) string {
	events := [][2]string{
		{"response.created", `"response":{"id":"resp_up","object":"response","model":"m","status":"in_progress","output":[]}`},
	}
	events = append(events, responsesTextItem(0, "msg0", text)...)
	events = append(events, responsesTextDeltaFrames(0, "msg0", text)...)
	events = append(events,
		responsesTextDone(0, "msg0", text),
		[2]string{"response.output_item.done", `"output_index":0,"item":` + replayMsgItemJSONWith(text)},
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
	return chatTextFrames(sse, false)
}

// chatTextAfterFirstTool 抽出「首个工具帧之后」的正文增量拼接。
//
// 为什么单看这一段：跨块整段回放的定义就是「全篇已在工具帧之前发完，工具帧之后又原样来了一份」，
// 故这一段的期望值是**空**（合法续写除外）——比「客户端正文总长不等」更直接地指出缺陷落点。
func chatTextAfterFirstTool(sse string) string {
	return chatTextFrames(sse, true)
}

func chatTextFrames(sse string, afterFirstTool bool) string {
	var builder strings.Builder
	seenTool := false
	for _, line := range strings.Split(sse, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		body := strings.TrimPrefix(line, "data: ")
		if body == DoneSentinel {
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
		if len(delta.ArrayField("tool_calls")) > 0 {
			seenTool = true
		}
		text, ok := stringField(delta, "content")
		if !ok {
			continue
		}
		if afterFirstTool && !seenTool {
			continue
		}
		builder.WriteString(text)
	}
	return builder.String()
}
