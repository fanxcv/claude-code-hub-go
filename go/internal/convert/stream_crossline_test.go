package convert

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// 跨线流式的逐对钉子：工具调用增量、思考增量、多块交错、终止事件与 usage 自洽。
//
// 为什么另建一套：语料 `tests/load/protocol-conformance/corpus/streams.json` 已冻结
// （Node 退役、**不可再生**），且 9 条用例**全是 text-turn**——工具增量、思考增量、多块交错
// 这三面在仓内原本没有任何逐对断言；对角线（同协议对）在语料里也只计数不逐字节比对。
//
// **期望值来源须分清（fixture 头部声明）：来源 = Go 实现自证，非 Node 金标。**
// 期望值由「Go 实现自证 + 三线协议硬约束」推导而来，而**不是** Node 退役时刻的行为快照；
// 其中协议硬约束（事件名、字段名、终止事件、块收尾顺序、sequence_number 单调、usage 算术、
// 工具参数拼接后必须是合法 JSON）与实现无关，可独立复核。断言的强度目标是**能被变异证伪**：
// 把任一编码分支改坏，对应用例必须转红（已用「抹掉 chat 编码器工具名」这一变异实测确认）。
//
// 统一语义场景（一次响应：思考 → 正文 → 工具调用 → 可选尾段正文）在三条上游线上各自表达，
// 再逐对（上游 ≠ 客户端）过真实转换路径（decoder → 枢纽 → encoder）。因此可以用一条不变量
// 覆盖全部六对：**同一场景在任何上游线上都应得到同一份客户端语义骨架**——这正是枢纽 IR
// 存在的理由，也是任何「某条线少写一段、多写一段、静默丢一段」的照妖镜。

const (
	streamSceneThinking = "想一下"
	streamSceneText     = "PONG"
	streamSceneLateText = "TAIL"
	// 工具名刻意取「会被改写」的形态：上游回显的是规范化名，客户端必须拿到原名。
	streamSceneToolWire = "mcp_weather_lookup"
	streamSceneToolName = "mcp.weather.lookup"
	streamSceneArgsHead = `{"city":`
	streamSceneArgsTail = `"sf"}`
	streamSceneArgs     = streamSceneArgsHead + streamSceneArgsTail

	// usage 三段：新鲜输入 120 + 缓存读取 1000 → 上游总量 1120；输出 42。
	streamSceneFresh  = 120
	streamSceneCached = 1000
	streamSceneOutput = 42
	streamSceneTotal  = streamSceneFresh + streamSceneCached
)

// streamScene 是一次响应的语义内容（与协议线无关）。
type streamScene struct {
	name     string
	thinking bool
	text     bool
	tool     bool
	lateText bool
}

func streamScenes() []streamScene {
	return []streamScene{
		{name: "text+tool", text: true, tool: true},
		{name: "thinking+text", thinking: true, text: true},
		{name: "thinking+text+tool", thinking: true, text: true, tool: true},
		{name: "text+tool+late-text", text: true, tool: true, lateText: true},
		{name: "thinking+text+tool+late-text", thinking: true, text: true, tool: true, lateText: true},
	}
}

func crossLinePairs() []struct {
	upstream WireProtocol
	client   WireProtocol
} {
	lines := []WireProtocol{ProtocolAnthropicMessages, ProtocolOpenAIChat, ProtocolOpenAIResponses}
	pairs := make([]struct {
		upstream WireProtocol
		client   WireProtocol
	}, 0, 6)
	for _, upstream := range lines {
		for _, client := range lines {
			if upstream == client {
				continue
			}
			pairs = append(pairs, struct {
				upstream WireProtocol
				client   WireProtocol
			}{upstream: upstream, client: client})
		}
	}
	return pairs
}

// streamSceneToolRestore 是「规范化名 → 客户端原名」逆转表（与真实报文的形状一致）。
func streamSceneToolRestore() map[string]string {
	return map[string]string{streamSceneToolWire: streamSceneToolName}
}

func crossSceneCtx(upstream WireProtocol, client WireProtocol, placeholder bool) ConvertCtx {
	return ConvertCtx{
		ClientFormat:                 clientFormatOfProtocol(client),
		TargetProto:                  upstream,
		Model:                        "m",
		Stream:                       true,
		FromWireToolName:             restoreHook(streamSceneToolRestore()),
		PlaceholderThinkingSignature: placeholder,
	}
}

// runCrossScene 走完整转换路径（上游字节 → 客户端字节），chunkSize<=0 表示整块喂入。
func runCrossScene(t *testing.T, upstream WireProtocol, client WireProtocol, input string, chunkSize int, placeholder bool) string {
	t.Helper()
	pipe, ok := NewStreamPipe(upstream, client, crossSceneCtx(upstream, client, placeholder))
	if !ok {
		t.Fatalf("建管道失败：%s -> %s", upstream, client)
	}
	var out []byte
	if chunkSize <= 0 {
		out = append(out, pipe.Push([]byte(input))...)
	} else {
		for start := 0; start < len(input); start += chunkSize {
			end := start + chunkSize
			if end > len(input) {
				end = len(input)
			}
			out = append(out, pipe.Push([]byte(input[start:end]))...)
		}
	}
	out = append(out, pipe.Flush()...)
	return normalizeSyntheticIDs(string(out))
}

// ---------------------------------------------------------------------------
// 上游线：把语义场景编码成该线的 SSE 字节
// ---------------------------------------------------------------------------

func sseFrame(event string, data string) string {
	if event == "" {
		return "data: " + data + "\n\n"
	}
	return "event: " + event + "\ndata: " + data + "\n\n"
}

func sceneStopReason(scene streamScene) string {
	if scene.tool {
		return "tool_use"
	}
	return "end_turn"
}

// anthropicSceneStream 生成 Anthropic 上游线字节。
func anthropicSceneStream(scene streamScene) string {
	var builder strings.Builder
	builder.WriteString(sseFrame("message_start", `{"type":"message_start","message":{"id":"msg_up","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":120,"cache_read_input_tokens":1000,"output_tokens":1}}}`))

	index := 0
	blockStart := func(block string) {
		builder.WriteString(sseFrame("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":%d,"content_block":%s}`, index, block)))
	}
	blockDelta := func(delta string) {
		builder.WriteString(sseFrame("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":%d,"delta":%s}`, index, delta)))
	}
	blockStop := func() {
		builder.WriteString(sseFrame("content_block_stop", fmt.Sprintf(`{"type":"content_block_stop","index":%d}`, index)))
		index++
	}

	if scene.thinking {
		blockStart(`{"type":"thinking","thinking":"","signature":"sig_up"}`)
		blockDelta(`{"type":"thinking_delta","thinking":"想一下"}`)
		blockStop()
	}
	if scene.text {
		blockStart(`{"type":"text","text":""}`)
		blockDelta(`{"type":"text_delta","text":"PONG"}`)
		blockStop()
	}
	if scene.tool {
		blockStart(fmt.Sprintf(`{"type":"tool_use","id":"toolu_up1","name":%q,"input":{}}`, streamSceneToolWire))
		blockDelta(`{"type":"input_json_delta","partial_json":"{\"city\":"}`)
		blockDelta(`{"type":"input_json_delta","partial_json":"\"sf\"}"}`)
		blockStop()
	}
	if scene.lateText {
		blockStart(`{"type":"text","text":""}`)
		blockDelta(`{"type":"text_delta","text":"TAIL"}`)
		blockStop()
	}

	builder.WriteString(sseFrame("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"output_tokens":42}}`, sceneStopReason(scene))))
	builder.WriteString(sseFrame("message_stop", `{"type":"message_stop"}`))
	return builder.String()
}

// chatSceneStream 生成 OpenAI Chat 上游线字节。
func chatSceneStream(scene streamScene) string {
	var builder strings.Builder
	chunk := func(delta string) {
		builder.WriteString(sseFrame("", fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":%s}]}`, delta)))
	}

	chunk(`{"role":"assistant","content":""}`)
	if scene.thinking {
		chunk(`{"reasoning_content":"想一下"}`)
	}
	if scene.text {
		chunk(`{"content":"PONG"}`)
	}
	if scene.tool {
		chunk(fmt.Sprintf(`{"tool_calls":[{"index":0,"id":"call_up1","type":"function","function":{"name":%q,"arguments":""}}]}`, streamSceneToolWire))
		chunk(`{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}`)
		chunk(`{"tool_calls":[{"index":0,"function":{"arguments":"\"sf\"}"}}]}`)
	}
	if scene.lateText {
		chunk(`{"content":"TAIL"}`)
	}

	finishReason := "stop"
	if scene.tool {
		finishReason = "tool_calls"
	}
	builder.WriteString(sseFrame("", fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":%q}],"usage":{"prompt_tokens":1120,"completion_tokens":42,"total_tokens":1162,"prompt_tokens_details":{"cached_tokens":1000}}}`, finishReason)))
	builder.WriteString("data: [DONE]\n\n")
	return builder.String()
}

// responsesSceneStream 生成 OpenAI Responses 上游线字节。
func responsesSceneStream(scene streamScene) string {
	var builder strings.Builder
	sequence := 0
	emit := func(event string, body string) {
		payload := fmt.Sprintf(`{"type":%q,"sequence_number":%d`, event, sequence)
		if body != "" {
			payload += "," + body
		}
		payload += "}"
		builder.WriteString(sseFrame(event, payload))
		sequence++
	}

	emit("response.created", `"response":{"id":"resp_up","object":"response","model":"m","status":"in_progress","output":[]}`)

	outputIndex := 0
	if scene.thinking {
		itemID := "rs_up0"
		emit("response.output_item.added", fmt.Sprintf(`"output_index":%d,"item":{"id":%q,"type":"reasoning","summary":[]}`, outputIndex, itemID))
		emit("response.reasoning_summary_part.added", fmt.Sprintf(`"item_id":%q,"output_index":%d,"summary_index":0,"part":{"type":"summary_text","text":""}`, itemID, outputIndex))
		emit("response.reasoning_summary_text.delta", fmt.Sprintf(`"item_id":%q,"output_index":%d,"summary_index":0,"delta":"想一下"`, itemID, outputIndex))
		emit("response.reasoning_summary_text.done", fmt.Sprintf(`"item_id":%q,"output_index":%d,"summary_index":0,"text":"想一下"`, itemID, outputIndex))
		emit("response.output_item.done", fmt.Sprintf(`"output_index":%d,"item":{"id":%q,"type":"reasoning","summary":[{"type":"summary_text","text":"想一下"}]}`, outputIndex, itemID))
		outputIndex++
	}

	textItem := func(text string) {
		itemID := fmt.Sprintf("msg_up%d", outputIndex)
		emit("response.output_item.added", fmt.Sprintf(`"output_index":%d,"item":{"id":%q,"type":"message","role":"assistant","status":"in_progress","content":[]}`, outputIndex, itemID))
		emit("response.content_part.added", fmt.Sprintf(`"item_id":%q,"output_index":%d,"content_index":0,"part":{"type":"output_text","text":""}`, itemID, outputIndex))
		emit("response.output_text.delta", fmt.Sprintf(`"item_id":%q,"output_index":%d,"content_index":0,"delta":%q`, itemID, outputIndex, text))
		emit("response.output_text.done", fmt.Sprintf(`"item_id":%q,"output_index":%d,"content_index":0,"text":%q`, itemID, outputIndex, text))
		emit("response.output_item.done", fmt.Sprintf(`"output_index":%d,"item":{"id":%q,"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":%q}]}`, outputIndex, itemID, text))
		outputIndex++
	}

	if scene.text {
		textItem(streamSceneText)
	}
	if scene.tool {
		itemID := fmt.Sprintf("fc_up%d", outputIndex)
		emit("response.output_item.added", fmt.Sprintf(`"output_index":%d,"item":{"id":%q,"type":"function_call","status":"in_progress","call_id":"call_up1","name":%q,"arguments":""}`, outputIndex, itemID, streamSceneToolWire))
		emit("response.function_call_arguments.delta", fmt.Sprintf(`"item_id":%q,"output_index":%d,"delta":"{\"city\":"`, itemID, outputIndex))
		emit("response.function_call_arguments.delta", fmt.Sprintf(`"item_id":%q,"output_index":%d,"delta":"\"sf\"}"`, itemID, outputIndex))
		emit("response.function_call_arguments.done", fmt.Sprintf(`"item_id":%q,"output_index":%d,"arguments":"{\"city\":\"sf\"}"`, itemID, outputIndex))
		emit("response.output_item.done", fmt.Sprintf(`"output_index":%d,"item":{"id":%q,"type":"function_call","status":"completed","call_id":"call_up1","name":%q,"arguments":"{\"city\":\"sf\"}"}`, outputIndex, itemID, streamSceneToolWire))
		outputIndex++
	}
	if scene.lateText {
		textItem(streamSceneLateText)
	}

	emit("response.completed", `"response":{"id":"resp_up","object":"response","model":"m","status":"completed","output":[],"usage":{"input_tokens":1120,"output_tokens":42,"total_tokens":1162,"input_tokens_details":{"cached_tokens":1000}}}`)
	return builder.String()
}

func upstreamSceneStream(upstream WireProtocol, scene streamScene) string {
	switch upstream {
	case ProtocolAnthropicMessages:
		return anthropicSceneStream(scene)
	case ProtocolOpenAIChat:
		return chatSceneStream(scene)
	case ProtocolOpenAIResponses:
		return responsesSceneStream(scene)
	default:
		panic("未覆盖的上游线 " + string(upstream))
	}
}

// ---------------------------------------------------------------------------
// 客户端线：把客户端字节还原成语义骨架
// ---------------------------------------------------------------------------

// spineEntry 是客户端线可观察到的语义单元（块的种类 + 累积内容）。
//
// id 与 signature 不参与跨线比较：前者各线命名规则不同，后者是 Anthropic 独有通道。
type spineEntry struct {
	kind      string
	name      string
	text      string
	id        string
	signature string
}

func stripSpineIdentity(spine []spineEntry) []spineEntry {
	out := make([]spineEntry, 0, len(spine))
	for _, entry := range spine {
		entry.id = ""
		entry.signature = ""
		out = append(out, entry)
	}
	return out
}

func expectedSceneSpine(scene streamScene) []spineEntry {
	var out []spineEntry
	if scene.thinking {
		out = append(out, spineEntry{kind: "thinking", text: streamSceneThinking})
	}
	if scene.text {
		out = append(out, spineEntry{kind: "text", text: streamSceneText})
	}
	if scene.tool {
		out = append(out, spineEntry{kind: "tool", name: streamSceneToolName, text: streamSceneArgs})
	}
	if scene.lateText {
		out = append(out, spineEntry{kind: "text", text: streamSceneLateText})
	}
	return out
}

type outFrame struct {
	frame SSEFrame
	data  *Value
}

func parseOutFrames(t *testing.T, out string) []outFrame {
	t.Helper()
	frames, rest := ParseSSEFrames(out)
	if strings.TrimSpace(rest) != "" {
		t.Fatalf("客户端输出尾部有未成帧残留：%q", rest)
	}
	result := make([]outFrame, 0, len(frames))
	for _, frame := range frames {
		result = append(result, outFrame{frame: frame, data: parseFrameJSON(frame)})
	}
	return result
}

func spineOfAnthropicStream(t *testing.T, frames []outFrame) []spineEntry {
	t.Helper()
	var out []spineEntry
	var current *spineEntry
	openIndex := -1
	flush := func() {
		if current != nil {
			out = append(out, *current)
			current = nil
		}
		openIndex = -1
	}
	for _, item := range frames {
		if item.data == nil {
			continue
		}
		event, _ := stringField(item.data, "type")
		switch event {
		case "content_block_start":
			flush()
			block := item.data.ObjectField("content_block")
			entry := spineEntry{}
			blockType, _ := stringField(block, "type")
			switch blockType {
			case "thinking":
				entry.kind = "thinking"
				entry.text, _ = stringField(block, "thinking")
			case "redacted_thinking":
				entry.kind = "thinking"
				entry.signature, _ = stringField(block, "data")
			case "text":
				entry.kind = "text"
				entry.text, _ = stringField(block, "text")
			case "tool_use":
				entry.kind = "tool"
				entry.name, _ = stringField(block, "name")
				entry.id, _ = stringField(block, "id")
				if input := block.ObjectField("input"); input != nil && input.IsObject() && input.Len() != 0 {
					t.Fatalf("content_block_start 的 input 必须为空对象（参数只走 input_json_delta），实为 %s", input.MarshalCompact())
				}
			default:
				t.Fatalf("未覆盖的 Anthropic 块类型 %q", blockType)
			}
			openIndex = intOrDefault(item.data, "index", -1)
			current = &entry
		case "content_block_delta":
			if current == nil {
				t.Fatalf("content_block_delta 出现在任何 content_block_start 之前：%s", item.frame.Data)
			}
			if index := intOrDefault(item.data, "index", -1); index != openIndex {
				t.Fatalf("delta 的 index=%d 与当前打开块 index=%d 不符", index, openIndex)
			}
			delta := item.data.ObjectField("delta")
			deltaType, _ := stringField(delta, "type")
			switch deltaType {
			case "thinking_delta":
				text, _ := stringField(delta, "thinking")
				current.text += text
			case "text_delta":
				text, _ := stringField(delta, "text")
				current.text += text
			case "input_json_delta":
				partial, _ := stringField(delta, "partial_json")
				current.text += partial
			case "signature_delta":
				signature, _ := stringField(delta, "signature")
				current.signature += signature
			default:
				t.Fatalf("未覆盖的 Anthropic delta 类型 %q", deltaType)
			}
		case "content_block_stop":
			flush()
		}
	}
	flush()
	return out
}

func spineOfChatStream(t *testing.T, frames []outFrame) []spineEntry {
	t.Helper()
	var out []spineEntry
	var current *spineEntry
	flush := func() {
		if current != nil {
			out = append(out, *current)
			current = nil
		}
	}
	start := func(kind string) {
		if current == nil || current.kind != kind {
			flush()
			current = &spineEntry{kind: kind}
		}
	}
	for _, item := range frames {
		if item.data == nil {
			continue
		}
		choices, ok := item.data.Get("choices")
		if !ok || !choices.IsArray() {
			continue
		}
		for _, choice := range choices.Items() {
			delta := choice.ObjectField("delta")
			if delta == nil {
				continue
			}
			if reasoning, ok := stringField(delta, "reasoning_content"); ok && reasoning != "" {
				start("thinking")
				current.text += reasoning
			}
			if content, ok := stringField(delta, "content"); ok && content != "" {
				start("text")
				current.text += content
			}
			calls, hasCalls := delta.Get("tool_calls")
			if !hasCalls || !calls.IsArray() {
				continue
			}
			for _, call := range calls.Items() {
				fn := call.ObjectField("function")
				name, _ := stringField(fn, "name")
				args, _ := stringField(fn, "arguments")
				start("tool")
				if name != "" {
					current.name = name
				}
				if id, ok := stringField(call, "id"); ok && id != "" {
					current.id = id
				}
				current.text += args
			}
		}
	}
	flush()
	return out
}

func spineOfResponsesStream(t *testing.T, frames []outFrame) []spineEntry {
	t.Helper()
	var order []int
	byIndex := map[int]*spineEntry{}
	for _, item := range frames {
		if item.data == nil {
			continue
		}
		event, _ := stringField(item.data, "type")
		switch event {
		case "response.output_item.added":
			index := intOrDefault(item.data, "output_index", -1)
			entry := &spineEntry{}
			item0 := item.data.ObjectField("item")
			itemType, _ := stringField(item0, "type")
			switch itemType {
			case "reasoning":
				entry.kind = "thinking"
			case "message":
				entry.kind = "text"
			case "function_call":
				entry.kind = "tool"
				entry.name, _ = stringField(item0, "name")
				entry.id, _ = stringField(item0, "call_id")
				entry.text, _ = stringField(item0, "arguments")
			default:
				t.Fatalf("未覆盖的 Responses item 类型 %q", itemType)
			}
			if _, seen := byIndex[index]; !seen {
				order = append(order, index)
			}
			byIndex[index] = entry
		case "response.output_text.delta", "response.reasoning_summary_text.delta", "response.function_call_arguments.delta":
			index := intOrDefault(item.data, "output_index", -1)
			entry := byIndex[index]
			if entry == nil {
				t.Fatalf("delta 出现在任何 output_item.added 之前：%s", item.frame.Data)
			}
			delta, _ := stringField(item.data, "delta")
			entry.text += delta
		}
	}
	out := make([]spineEntry, 0, len(order))
	for _, index := range order {
		out = append(out, *byIndex[index])
	}
	return out
}

func spineOfClientStream(t *testing.T, client WireProtocol, frames []outFrame) []spineEntry {
	t.Helper()
	switch client {
	case ProtocolAnthropicMessages:
		return spineOfAnthropicStream(t, frames)
	case ProtocolOpenAIChat:
		return spineOfChatStream(t, frames)
	default:
		return spineOfResponsesStream(t, frames)
	}
}

// ---------------------------------------------------------------------------
// 断言：终止事件、块收尾、usage 自洽
// ---------------------------------------------------------------------------

func assertTerminalEvent(t *testing.T, client WireProtocol, frames []outFrame) {
	t.Helper()
	if len(frames) == 0 {
		t.Fatal("客户端输出为空")
	}
	switch client {
	case ProtocolAnthropicMessages:
		first, _ := stringField(frames[0].data, "type")
		if first != "message_start" {
			t.Fatalf("Anthropic 首帧必须是 message_start，实为 %q", first)
		}
		lastEvent, _ := stringField(frames[len(frames)-1].data, "type")
		if lastEvent != "message_stop" {
			t.Fatalf("Anthropic 末帧必须是 message_stop，实为 %q", lastEvent)
		}
		sawDelta := false
		for _, item := range frames {
			if event, _ := stringField(item.data, "type"); event == "message_delta" {
				sawDelta = true
			}
		}
		if !sawDelta {
			t.Fatal("Anthropic 输出缺 message_delta（stop_reason 无处承载）")
		}
	case ProtocolOpenAIChat:
		if !strings.HasSuffix(strings.TrimSpace(frames[len(frames)-1].frame.Data), DoneSentinel) {
			t.Fatalf("Chat 末帧必须是 %s，实为 %q", DoneSentinel, frames[len(frames)-1].frame.Data)
		}
		// 顺序契约：带 finish_reason 的分块必须早于 [DONE]
		sawFinish := false
		for _, item := range frames[:len(frames)-1] {
			for _, choice := range item.data.ArrayField("choices") {
				if reason, ok := stringField(choice, "finish_reason"); ok && reason != "" {
					sawFinish = true
				}
			}
		}
		if !sawFinish {
			t.Fatal("Chat 输出缺带 finish_reason 的分块")
		}
	case ProtocolOpenAIResponses:
		lastEvent, _ := stringField(frames[len(frames)-1].data, "type")
		if lastEvent != "response.completed" && lastEvent != "response.incomplete" {
			t.Fatalf("Responses 末帧必须是 response.completed/incomplete，实为 %q", lastEvent)
		}
		previous := -1
		for _, item := range frames {
			if item.data == nil {
				continue
			}
			sequence, ok := numberField(item.data, "sequence_number")
			if !ok || sequence == nil {
				t.Fatalf("Responses 每个事件都必须带 sequence_number：%s", item.frame.Data)
			}
			if int(*sequence) != previous+1 {
				t.Fatalf("sequence_number 必须从 0 起单调递增：期望 %d，实为 %d", previous+1, int(*sequence))
			}
			previous = int(*sequence)
		}
	}
}

// assertUsageSane 校验客户端可见 usage 的三条不变量，并返回 (总输入, 缓存读, 输出)。
func assertUsageSane(t *testing.T, client WireProtocol, frames []outFrame) (float64, float64, float64) {
	t.Helper()
	input, cached, output := -1.0, 0.0, 0.0
	switch client {
	case ProtocolAnthropicMessages:
		for _, item := range frames {
			if event, _ := stringField(item.data, "type"); event != "message_delta" {
				continue
			}
			usage := item.data.ObjectField("usage")
			if usage == nil {
				continue
			}
			fresh, _ := numberField(usage, "input_tokens")
			cacheRead, _ := numberField(usage, "cache_read_input_tokens")
			out, _ := numberField(usage, "output_tokens")
			if fresh != nil && cacheRead != nil {
				input = *fresh + *cacheRead
			}
			if cacheRead != nil {
				cached = *cacheRead
			}
			if out != nil {
				output = *out
			}
		}
	case ProtocolOpenAIChat:
		for _, item := range frames {
			usage := item.data.ObjectField("usage")
			if usage == nil {
				continue
			}
			prompt, _ := numberField(usage, "prompt_tokens")
			completion, _ := numberField(usage, "completion_tokens")
			total, _ := numberField(usage, "total_tokens")
			cachedTokens, _ := numberOrNil(nestedField(usage, "prompt_tokens_details", "cached_tokens"))
			if prompt == nil || completion == nil || total == nil {
				t.Fatal("Chat usage 缺 prompt/completion/total")
			}
			if *total != *prompt+*completion {
				t.Fatalf("Chat usage 自相矛盾：total=%v != prompt=%v + completion=%v", *total, *prompt, *completion)
			}
			input = *prompt
			output = *completion
			if cachedTokens != nil {
				cached = *cachedTokens
			}
		}
	case ProtocolOpenAIResponses:
		for _, item := range frames {
			event, _ := stringField(item.data, "type")
			if event != "response.completed" && event != "response.incomplete" {
				continue
			}
			response := item.data.ObjectField("response")
			usage := response.ObjectField("usage")
			if usage == nil {
				t.Fatal("Responses 终态事件必须带 usage")
			}
			in, _ := numberField(usage, "input_tokens")
			out, _ := numberField(usage, "output_tokens")
			total, _ := numberField(usage, "total_tokens")
			cachedTokens, _ := numberOrNil(nestedField(usage, "input_tokens_details", "cached_tokens"))
			if in == nil || out == nil || total == nil {
				t.Fatal("Responses usage 缺 input/output/total")
			}
			if *total != *in+*out {
				t.Fatalf("Responses usage 自相矛盾：total=%v != input=%v + output=%v", *total, *in, *out)
			}
			input = *in
			output = *out
			if cachedTokens != nil {
				cached = *cachedTokens
			}
		}
	}
	if input < 0 {
		t.Fatalf("客户端线的输出里没有可解析的 usage（%s）", client)
	}
	if cached > input {
		t.Fatalf("缓存读取 %v 不得超过总输入 %v", cached, input)
	}
	return input, cached, output
}

// ---------------------------------------------------------------------------
// 逐对矩阵
// ---------------------------------------------------------------------------

// TestCrossLineStreamSceneMatrix 是跨线流式的主钉子：五类语义场景 × 六对跨协议对。
//
// 每个用例断言三件事：① 客户端语义骨架与场景一致（丢一段/多一段/顺序错乱都会红）；
// ② 客户端线的终止事件齐全；③ usage 自洽且数值与场景一致（含「上游含缓存总量 1120 减成
// 新鲜 120、编码时再加回」这条换算法）。
func TestCrossLineStreamSceneMatrix(t *testing.T) {
	for _, scene := range streamScenes() {
		for _, pair := range crossLinePairs() {
			name := fmt.Sprintf("%s/%s_to_%s", scene.name, pair.upstream, pair.client)
			t.Run(name, func(t *testing.T) {
				input := upstreamSceneStream(pair.upstream, scene)
				out := runCrossScene(t, pair.upstream, pair.client, input, 0, true)
				frames := parseOutFrames(t, out)

				assertTerminalEvent(t, pair.client, frames)

				spine := spineOfClientStream(t, pair.client, frames)
				want := expectedSceneSpine(scene)
				if !reflect.DeepEqual(stripSpineIdentity(spine), want) {
					t.Fatalf("客户端语义骨架不符\n实得：%+v\n期望：%+v\n原始输出：\n%s", stripSpineIdentity(spine), want, out)
				}
				for _, entry := range spine {
					if entry.kind != "tool" {
						continue
					}
					if entry.id == "" {
						t.Fatal("工具调用缺少 id/call_id")
					}
					if entry.name != streamSceneToolName {
						t.Fatalf("工具名必须还原为客户端原名 %q，实为 %q", streamSceneToolName, entry.name)
					}
					if _, err := ParseJSON([]byte(entry.text)); err != nil {
						t.Fatalf("拼接后的工具参数不是合法 JSON（参数分片被丢或错位）：%q", entry.text)
					}
				}

				inputTokens, cachedTokens, outputTokens := assertUsageSane(t, pair.client, frames)
				if outputTokens != streamSceneOutput {
					t.Fatalf("输出 token 不符：%v（期望 %d）", outputTokens, streamSceneOutput)
				}
				if pair.upstream == ProtocolAnthropicMessages {
					// 已知缺陷（语料已钉住，见 assertAnthropicStartUsageDroppedOnForeignClients）：
					// Anthropic 上游把输入与缓存读放在 message_start，而 chat/responses 编码器
					// 只在终态合并 usage，忽略 ChunkStart.Usage，客户端因此看到 0。此处如实记录。
					if inputTokens != 0 || cachedTokens != 0 {
						t.Fatalf("Anthropic 上游的起始帧 usage 归一行为已变（总输入=%v 缓存读=%v）；"+
							"若这是修复所致，请同步更新 assertAnthropicStartUsageDroppedOnForeignClients 与语料说明", inputTokens, cachedTokens)
					}
					return
				}
				if inputTokens != streamSceneTotal || cachedTokens != streamSceneCached {
					t.Fatalf("usage 归一后不符：总输入=%v（期望 %d）缓存读=%v（期望 %d）",
						inputTokens, streamSceneTotal, cachedTokens, streamSceneCached)
				}
			})
		}
	}
}

// TestCrossLineStreamSplitFeedingIsStable 钉住「输出不依赖分块位置」。
//
// 语料用 split-half 覆盖跨帧边界；这里进一步按**单字节**喂入——SSE 帧分隔符、JSON 字符串、
// 多字节字符都会被切开，任何把「一块 = 一帧」的前提写死的实现都会在这里露馅。
func TestCrossLineStreamSplitFeedingIsStable(t *testing.T) {
	for _, scene := range streamScenes() {
		for _, pair := range crossLinePairs() {
			name := fmt.Sprintf("%s/%s_to_%s", scene.name, pair.upstream, pair.client)
			t.Run(name, func(t *testing.T) {
				input := upstreamSceneStream(pair.upstream, scene)
				whole := runCrossScene(t, pair.upstream, pair.client, input, 0, true)
				split := runCrossScene(t, pair.upstream, pair.client, input, 1, true)
				if whole != split {
					t.Fatalf("整块与逐字节喂入的输出不一致\n整块：%s\n逐字节：%s", whole, split)
				}
			})
		}
	}
}

// TestCrossLineStreamTerminationWhenUpstreamTruncates 钉住上游半途断流时的收尾。
//
// 生产上上游断流很常见（超时、上游主动关闭）。客户端必须拿到自己方言的终止事件，
// 否则客户端会一直挂着等下一帧。这里去掉上游的终止事件（message_stop / [DONE] /
// response.completed），只喂到工具调用为止。
func TestCrossLineStreamTerminationWhenUpstreamTruncates(t *testing.T) {
	for _, pair := range crossLinePairs() {
		name := fmt.Sprintf("%s_to_%s", pair.upstream, pair.client)
		t.Run(name, func(t *testing.T) {
			input := truncateUpstreamTerminal(pair.upstream, upstreamSceneStream(pair.upstream, streamScene{name: "truncated", tool: true}))
			out := runCrossScene(t, pair.upstream, pair.client, input, 0, true)
			frames := parseOutFrames(t, out)
			assertTerminalEvent(t, pair.client, frames)
			for _, entry := range spineOfClientStream(t, pair.client, frames) {
				if entry.kind == "tool" && entry.name != streamSceneToolName {
					t.Fatalf("断流场景下工具名丢失：%q", entry.name)
				}
			}
		})
	}
}

// TestCrossLineStreamLateReasoningAfterText 钉住 chat 上游「文本已开始后又来推理增量」的处理。
//
// Chat 线的推理增量语义要求排在文本之前（枢纽顺序契约）；迟到的推理无法安放，解码器只能
// 丢弃，不得把流写坏、也不得挤掉正文。
func TestCrossLineStreamLateReasoningAfterTextIsCounted(t *testing.T) {
	input := sseFrame("", `{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`) +
		sseFrame("", `{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"content":"PONG"}}]}`) +
		sseFrame("", `{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{"reasoning_content":"迟到推理"}}]}`) +
		sseFrame("", `{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1120,"completion_tokens":42,"total_tokens":1162,"prompt_tokens_details":{"cached_tokens":1000}}}`) +
		"data: [DONE]\n\n"

	for _, client := range []WireProtocol{ProtocolAnthropicMessages, ProtocolOpenAIResponses} {
		t.Run(string(client), func(t *testing.T) {
			out := runCrossScene(t, ProtocolOpenAIChat, client, input, 0, true)
			frames := parseOutFrames(t, out)
			assertTerminalEvent(t, client, frames)

			spine := spineOfClientStream(t, client, frames)
			want := []spineEntry{{kind: "text", text: streamSceneText}}
			if !reflect.DeepEqual(stripSpineIdentity(spine), want) {
				t.Fatalf("迟到的推理不得变成块（也不得挤掉正文）\n实得：%+v\n期望：%+v", stripSpineIdentity(spine), want)
			}
		})
	}
}

// TestCrossLineStreamSameFrameContentAndToolArgs 钉住「一帧里同时带正文与工具参数续片」这一交错面。
//
// 这是 OpenAI Chat 线上真实存在的帧形态（vLLM 类上游在工具调用途中会再吐一段正文）。
// 若实现按「先正文、后工具」的顺序消费，正文会把已打开的工具块收起，随后到达的参数续片就
// 无处可去——工具的 JSON 参数被截断成非法 JSON，客户端整次工具调用报废，且丢在暗处。
// 本用例的断言就是「拼接后的参数必须是合法 JSON」。
func TestCrossLineStreamSameFrameContentAndToolArgs(t *testing.T) {
	input := chatSameFrameContentAndToolArgs()

	for _, client := range []WireProtocol{ProtocolAnthropicMessages, ProtocolOpenAIResponses} {
		t.Run(string(client), func(t *testing.T) {
			out := runCrossScene(t, ProtocolOpenAIChat, client, input, 0, true)
			frames := parseOutFrames(t, out)
			assertTerminalEvent(t, client, frames)

			spine := spineOfClientStream(t, client, frames)
			want := []spineEntry{
				{kind: "text", text: streamSceneText},
				{kind: "tool", name: streamSceneToolName, text: streamSceneArgs},
				{kind: "text", text: streamSceneLateText},
			}
			if !reflect.DeepEqual(stripSpineIdentity(spine), want) {
				t.Fatalf("同帧正文与参数续片交错后的骨架不符\n实得：%+v\n期望：%+v\n原始输出：\n%s", stripSpineIdentity(spine), want, out)
			}
			for _, entry := range spine {
				if entry.kind != "tool" {
					continue
				}
				if _, err := ParseJSON([]byte(entry.text)); err != nil {
					t.Fatalf("工具参数被截断，拼接结果不是合法 JSON：%q", entry.text)
				}
			}
		})
	}
}

// chatSameFrameContentAndToolArgs 构造「工具块已打开、参数分两片，第二片与一段正文同帧」的上游字节。
func chatSameFrameContentAndToolArgs() string {
	chunk := func(delta string) string {
		return sseFrame("", fmt.Sprintf(`{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":%s}]}`, delta))
	}
	return chunk(`{"role":"assistant","content":""}`) +
		chunk(`{"content":"PONG"}`) +
		chunk(fmt.Sprintf(`{"tool_calls":[{"index":0,"id":"call_up1","type":"function","function":{"name":%q,"arguments":""}}]}`, streamSceneToolWire)) +
		chunk(`{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]}`) +
		chunk(`{"content":"TAIL","tool_calls":[{"index":0,"function":{"arguments":"\"sf\"}"}}]}`) +
		sseFrame("", `{"id":"c1","object":"chat.completion.chunk","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1120,"completion_tokens":42,"total_tokens":1162,"prompt_tokens_details":{"cached_tokens":1000}}}`) +
		"data: [DONE]\n\n"
}

// truncateUpstreamTerminal 砍掉上游的终止事件，模拟上游半途断流。
func truncateUpstreamTerminal(upstream WireProtocol, input string) string {
	switch upstream {
	case ProtocolAnthropicMessages:
		cut := strings.Index(input, "event: message_delta")
		return input[:cut]
	case ProtocolOpenAIChat:
		cut := strings.Index(input, `"finish_reason"`)
		if cut < 0 {
			return input
		}
		// 回退到该帧起点（`data: ` 之前）
		return input[:strings.LastIndex(input[:cut], "data: ")]
	default:
		cut := strings.Index(input, "event: response.completed")
		return input[:cut]
	}
}

// ---------------------------------------------------------------------------
// 已存在的缺陷：钉住事实，避免静默漂移
// ---------------------------------------------------------------------------

// TestCrossLineStreamAnthropicStartUsageIsDroppedOnForeignClients 钉住一条**已存在的缺陷**——
// 记录事实，不是认可该行为。
//
// 事实：Anthropic 上游把「输入 token 与缓存读取」放在 message_start（枢纽里是 ChunkStart.Usage），
// 而 openai-chat / openai-responses 两个客户端编码器**只在终态块合并 usage**（ChunkDelta），
// ChunkStart 携带的 usage 被直接丢弃 → 跨线之后客户端看到 prompt_tokens / input_tokens = 0。
// 对照：Anthropic 客户端（编码器在 ChunkStart 分支里合并 usage）能拿到 120 / 1000。
//
// 为什么不顺手修：语料 streams.json 的 `stream.anthropic-messages.text-turn.to.openai-chat` 与
// `...to.openai-responses` 已把 `prompt_tokens: 0` / `input_tokens: 0` 固化为金标，而语料是
// Node 退役前冻结、**不可手工编辑**（见 corpus/README.md）。即 Node 实现同病：修它等于有意
// 偏离语料，属需要决策的语义变更，不在本 worktree 的行为边界内。
//
// 本测试的价值：① 让缺陷可见且带上下文；② 一旦有人修复，这里立即转红，强制同步语料说明与决策。
func TestCrossLineStreamAnthropicStartUsageIsDroppedOnForeignClients(t *testing.T) {
	scene := streamScene{name: "text+tool", text: true, tool: true}
	input := anthropicSceneStream(scene)

	// 对照：Anthropic 编码器在 ChunkStart 分支里合并 usage，故从 chat 上游转过去不减损。
	t.Run("anthropic客户端保留usage", func(t *testing.T) {
		out := runCrossScene(t, ProtocolOpenAIChat, ProtocolAnthropicMessages, chatSceneStream(scene), 0, true)
		frames := parseOutFrames(t, out)
		total, cached, output := assertUsageSane(t, ProtocolAnthropicMessages, frames)
		if total != streamSceneTotal || cached != streamSceneCached || output != streamSceneOutput {
			t.Fatalf("anthropic 客户端 usage 不符：总输入=%v 缓存读=%v 输出=%v", total, cached, output)
		}
	})

	// 缺陷复现：同一条 Anthropic 上游，换成 chat / responses 客户端，输入与缓存读整段归零。
	for _, client := range []WireProtocol{ProtocolOpenAIChat, ProtocolOpenAIResponses} {
		t.Run("缺陷复现_"+string(client), func(t *testing.T) {
			out := runCrossScene(t, ProtocolAnthropicMessages, client, input, 0, true)
			frames := parseOutFrames(t, out)
			total, cached, output := assertUsageSane(t, client, frames)
			if total != 0 || cached != 0 {
				t.Fatalf("缺陷已消失（总输入=%v 缓存读=%v）：请同步更新语料说明与本次决策，勿只改断言", total, cached)
			}
			if output != streamSceneOutput {
				t.Fatalf("输出 token 应被保留：%v", output)
			}
		})
	}
}
