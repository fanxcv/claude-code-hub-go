package convert

import (
	"strconv"
	"strings"
)

// openai-chat · 流式编解码
//
// 解码：上游 `chat.completion.chunk` SSE → 枢纽块；编码：枢纽块 → 客户端 SSE 字节。
// Chat 线的顺序约束：`finish_reason` 非空的分块之后才可发 `[DONE]`；推理增量必须排在
// 文本块之前（枢纽顺序契约），否则客户端侧会顺序错乱。

type chatOpenBlockKind string

const (
	chatBlockText     chatOpenBlockKind = "text"
	chatBlockThinking chatOpenBlockKind = "thinking"
	chatBlockTool     chatOpenBlockKind = "tool"
)

type chatStreamDecoder struct {
	ctx    ConvertCtx
	buffer string

	started  bool
	finished bool
	// textStarted 记录「已出现过文本增量」，用于把迟到的推理增量降级忽略。
	textStarted bool

	openBlock      *chatOpenBlock
	nextBlockIndex int
	lastToolIndex  int
	// toolBlocks 是「上游 tool_calls index → 枢纽 blockIndex」。
	toolBlocks map[int]int

	usage      *Usage
	stopReason *StopReason

	ignoredEvents int
}

type chatOpenBlock struct {
	index int
	kind  chatOpenBlockKind
}

func newChatStreamDecoder(ctx ConvertCtx) StreamDecoder {
	return &chatStreamDecoder{ctx: ctx, lastToolIndex: -1, toolBlocks: map[int]int{}}
}

func (d *chatStreamDecoder) IgnoredEvents() int { return d.ignoredEvents }

func (d *chatStreamDecoder) Push(chunk []byte) []Chunk {
	if d.finished {
		return nil
	}
	d.buffer += string(chunk)
	frames, rest := ParseSSEFrames(d.buffer)
	d.buffer = rest
	var out []Chunk
	for _, frame := range frames {
		d.consumeFrame(frame, &out)
	}
	return out
}

func (d *chatStreamDecoder) Flush() []Chunk {
	if d.finished {
		return nil
	}
	d.finished = true
	if len(strings.TrimSpace(d.buffer)) > 0 {
		d.ignoredEvents++
	}
	var out []Chunk
	d.ensureStart(&out)
	d.closeBlock(&out)
	reason := StopUnknown
	if d.stopReason != nil {
		reason = *d.stopReason
	}
	delta := Chunk{Kind: ChunkDelta, StopReason: &reason}
	if !d.usage.IsEmpty() {
		delta.Usage = d.usage
	}
	out = append(out, delta, Chunk{Kind: ChunkEnd})
	return out
}

func (d *chatStreamDecoder) consumeFrame(frame SSEFrame, out *[]Chunk) {
	if IsDoneFrame(frame) {
		return
	}
	payload := parseFrameJSON(frame)
	if payload == nil {
		d.ignoredEvents++
		return
	}
	d.ensureStart(out)

	choices, hasChoices := payload.Get("choices")
	if hasChoices && choices.IsArray() {
		for _, choice := range choices.Items() {
			if choice.IsObject() {
				d.consumeChoice(choice, out)
				continue
			}
			d.ignoredEvents++
		}
	} else if hasChoices && !choices.IsNull() {
		d.ignoredEvents++
	}

	if usage, ok := payload.Get("usage"); ok && !usage.IsNull() {
		d.usage = mergeUsage(d.usage, usageFromOpenAIChat(usage))
	}
	if err, ok := payload.Get("error"); ok && !err.IsNull() {
		d.ignoredEvents++
	}
}

func (d *chatStreamDecoder) consumeChoice(choice *Value, out *[]Chunk) {
	delta := choice.ObjectField("delta")
	if delta == nil {
		delta = choice.ObjectField("message")
	}
	if delta != nil {
		// 工具调用分两趟：先落「已打开块的参数续片」，再处理会关闭当前块的推理/正文，
		// 最后才开新工具块。上游在同一帧里同时带正文与工具参数续片是合法形态（工具调用
		// 途中模型再吐一句正文），若按「正文先、工具后」消费，续片会落在刚被正文关掉的
		// 块上——工具 JSON 参数被截断成非法 JSON，且丢在暗处。详见 consumeToolCalls。
		calls := d.collectToolCalls(delta)
		for _, call := range calls {
			if _, opened := d.toolBlocks[call.index]; opened {
				d.consumeToolCall(call, out)
			}
		}

		if reasoning, ok := decodeChatReasoning(delta); ok && len(reasoning) > 0 {
			d.emitReasoning(reasoning, out)
		}
		d.emitContent(fieldOrNil(delta, "content"), out)

		for _, call := range calls {
			if _, opened := d.toolBlocks[call.index]; !opened {
				d.consumeToolCall(call, out)
			}
		}
	}

	if finish, ok := stringField(choice, "finish_reason"); ok {
		reason := stopReasonFromOpenAIChat(finish, true)
		d.stopReason = &reason
	}
}

func (d *chatStreamDecoder) emitContent(content *Value, out *[]Chunk) {
	if content == nil {
		return
	}
	if text, ok := content.String(); ok {
		if len(text) > 0 {
			d.emitText(text, out)
		}
		return
	}
	if content.IsNull() {
		return
	}
	if content.IsArray() {
		for _, part := range content.Items() {
			if part.IsObject() {
				if text, ok := part.StringField("text"); ok {
					d.emitText(text, out)
					continue
				}
			}
			d.ignoredEvents++
		}
		return
	}
	d.ignoredEvents++
}

func (d *chatStreamDecoder) emitText(text string, out *[]Chunk) {
	if len(text) == 0 {
		return
	}
	if d.openBlock == nil || d.openBlock.kind != chatBlockText {
		d.closeBlock(out)
		index := d.nextBlockIndex
		d.nextBlockIndex++
		d.openBlock = &chatOpenBlock{index: index, kind: chatBlockText}
		*out = append(*out, Chunk{
			Kind:       ChunkBlockStart,
			BlockIndex: intPtr(index),
			Block:      &Block{Kind: BlockText, Text: ""},
		})
	}
	d.textStarted = true
	*out = append(*out, Chunk{
		Kind:       ChunkBlockDelta,
		BlockIndex: intPtr(d.openBlock.index),
		TextDelta:  stringPtr(text),
	})
}

func (d *chatStreamDecoder) emitReasoning(text string, out *[]Chunk) {
	if d.textStarted {
		// 文本块已开，推理只能排在其后 → 客户端侧会顺序错乱，放弃该增量并计数
		d.ignoredEvents++
		return
	}
	if d.openBlock == nil || d.openBlock.kind != chatBlockThinking {
		d.closeBlock(out)
		index := d.nextBlockIndex
		d.nextBlockIndex++
		d.openBlock = &chatOpenBlock{index: index, kind: chatBlockThinking}
		*out = append(*out, Chunk{
			Kind:       ChunkBlockStart,
			BlockIndex: intPtr(index),
			Block:      &Block{Kind: BlockThinking},
		})
	}
	*out = append(*out, Chunk{
		Kind:           ChunkBlockDelta,
		BlockIndex:     intPtr(d.openBlock.index),
		ReasoningDelta: stringPtr(text),
	})
}

// chatToolCallEntry 是一帧里的一项工具调用及其已解析的下标。
//
// 下标单独存一份：同一帧要分两趟处理（先递续片、后开新块），下标解析必须只做一次，
// 否则「缺省下标沿用上一个」的语义会在两趟之间漂移。
type chatToolCallEntry struct {
	raw   *Value
	index int
}

// collectToolCalls 解析一帧里的 tool_calls 数组并定出每项的「仅工具调用的相对下标」。
func (d *chatStreamDecoder) collectToolCalls(delta *Value) []chatToolCallEntry {
	raw, hasToolCalls := delta.Get("tool_calls")
	if !hasToolCalls || raw.IsNull() {
		return nil
	}
	if !raw.IsArray() {
		d.ignoredEvents++
		return nil
	}
	entries := make([]chatToolCallEntry, 0, len(raw.Items()))
	for _, item := range raw.Items() {
		if item == nil || !item.IsObject() {
			d.ignoredEvents++
			continue
		}
		entries = append(entries, chatToolCallEntry{raw: item, index: d.toolCallIndexOf(item)})
	}
	return entries
}

// toolCallIndexOf 定出该项的工具下标，并更新「上一个下标」（缺省的续片沿用上一个）。
func (d *chatStreamDecoder) toolCallIndexOf(raw *Value) int {
	index := d.lastToolIndex
	if index < 0 {
		index = 0
	}
	if value, ok := numberField(raw, "index"); ok && value != nil {
		index = int(*value)
	}
	d.lastToolIndex = index
	return index
}

// consumeToolCall 处理一项工具调用：首次见到该下标时先发带 id/name 的块起始事件，
// 再发参数分片（顺序契约：参数分片到达前必须先有块起始）。
func (d *chatStreamDecoder) consumeToolCall(call chatToolCallEntry, out *[]Chunk) {
	raw := call.raw
	fn := raw.ObjectField("function")
	if fn == nil {
		fn = NewObject()
	}
	blockIndex, exists := d.toolBlocks[call.index]
	if !exists {
		d.closeBlock(out)
		blockIndex = d.nextBlockIndex
		d.nextBlockIndex++
		d.toolBlocks[call.index] = blockIndex
		d.openBlock = &chatOpenBlock{index: blockIndex, kind: chatBlockTool}
		rawID, _ := stringField(raw, "id")
		wireName, _ := stringField(fn, "name")
		id := rawID
		if id == "" {
			id = MakeToolCallID("stream:" + strconv.Itoa(call.index))
		}
		*out = append(*out, Chunk{
			Kind:       ChunkBlockStart,
			BlockIndex: intPtr(blockIndex),
			Block: &Block{
				Kind: BlockToolCall,
				ID:   id,
				// 解出的名字是客户端原名（上游回显的是规范化名）
				Name: d.ctx.fromWireName(wireName),
			},
		})
	}

	if args, ok := stringField(fn, "arguments"); ok && len(args) > 0 {
		*out = append(*out, Chunk{
			Kind:       ChunkBlockDelta,
			BlockIndex: intPtr(blockIndex),
			ArgsDelta:  stringPtr(args),
		})
	}
}

func (d *chatStreamDecoder) closeBlock(out *[]Chunk) {
	if d.openBlock == nil {
		return
	}
	*out = append(*out, Chunk{Kind: ChunkBlockStop, BlockIndex: intPtr(d.openBlock.index)})
	d.openBlock = nil
}

func (d *chatStreamDecoder) ensureStart(out *[]Chunk) {
	if d.started {
		return
	}
	d.started = true
	*out = append(*out, Chunk{Kind: ChunkStart})
}

// ---------------------------------------------------------------------------
// 编码：枢纽块 → 客户端 SSE
// ---------------------------------------------------------------------------

type chatStreamEncoder struct {
	ctx     ConvertCtx
	chunkID string

	started     bool
	finishSent  bool
	doneSent    bool
	hasToolCall bool

	nextToolIndex int
	// toolIndexes 是「枢纽 blockIndex → tool_calls 相对下标」。
	toolIndexes map[int]int

	usage *Usage

	ignoredEvents int
}

func newChatStreamEncoder(ctx ConvertCtx) StreamEncoder {
	return &chatStreamEncoder{
		ctx:         ctx,
		chunkID:     MakeSyntheticResponseID(ProtocolOpenAIChat),
		toolIndexes: map[int]int{},
	}
}

func (e *chatStreamEncoder) IgnoredEvents() int { return e.ignoredEvents }

func (e *chatStreamEncoder) Push(chunk Chunk) [][]byte {
	if e.doneSent {
		if chunk.Kind != ChunkEnd {
			e.ignoredEvents++
		}
		return nil
	}
	var out [][]byte
	switch chunk.Kind {
	case ChunkStart:
		out = append(out, e.ensureStart()...)

	case ChunkBlockStart:
		out = append(out, e.ensureStart()...)
		block := chunk.Block
		if block != nil && block.Kind == BlockToolCall && chunk.BlockIndex != nil {
			index := e.allocateToolIndex(*chunk.BlockIndex)
			e.hasToolCall = true
			id := block.ID
			if id == "" {
				id = MakeToolCallID("stream:" + strconv.Itoa(*chunk.BlockIndex))
			}
			out = append(out, e.frame(NewObject().Set("tool_calls", NewArray(NewObject().
				Set("index", NewNumberInt(int64(index))).
				Set("id", NewString(id)).
				Set("type", NewString("function")).
				Set("function", NewObject().
					Set("name", NewString(block.Name)).
					Set("arguments", NewString("")))))))
		}

	case ChunkBlockDelta:
		out = append(out, e.ensureStart()...)
		switch {
		case chunk.TextDelta != nil && len(*chunk.TextDelta) > 0:
			out = append(out, e.frame(NewObject().Set("content", NewString(*chunk.TextDelta))))
		case chunk.ReasoningDelta != nil && len(*chunk.ReasoningDelta) > 0:
			out = append(out, e.frame(NewObject().Set("reasoning_content", NewString(*chunk.ReasoningDelta))))
		case chunk.ArgsDelta != nil && len(*chunk.ArgsDelta) > 0:
			index, ok := 0, false
			if chunk.BlockIndex != nil {
				index, ok = e.toolIndexes[*chunk.BlockIndex]
			}
			if !ok {
				e.ignoredEvents++
				return out
			}
			out = append(out, e.frame(NewObject().Set("tool_calls", NewArray(NewObject().
				Set("index", NewNumberInt(int64(index))).
				Set("function", NewObject().Set("arguments", NewString(*chunk.ArgsDelta)))))))
		default:
			// 空增量：本线无可产出的帧
			e.ignoredEvents++
		}

	case ChunkBlockStop:
		// 本线无块结束事件

	case ChunkDelta:
		out = append(out, e.ensureStart()...)
		if chunk.Usage != nil && !chunk.Usage.IsEmpty() {
			e.usage = mergeUsage(e.usage, chunk.Usage)
		}
		out = append(out, e.finishFrame(chunk.StopReason))

	case ChunkEnd:
		out = append(out, e.ensureStart()...)
		if !e.finishSent {
			out = append(out, e.finishFrame(nil))
		}
		out = append(out, e.doneFrame())
		e.doneSent = true

	default:
		e.ignoredEvents++
	}
	return out
}

func (e *chatStreamEncoder) Flush() [][]byte {
	if !e.started || e.doneSent {
		return nil
	}
	var out [][]byte
	// 顺序契约：finish_reason 非空的分块之后才可发 [DONE]
	if !e.finishSent {
		out = append(out, e.finishFrame(nil))
	}
	out = append(out, e.doneFrame())
	e.doneSent = true
	return out
}

func (e *chatStreamEncoder) ensureStart() [][]byte {
	if e.started {
		return nil
	}
	e.started = true
	return [][]byte{e.frame(NewObject().
		Set("role", NewString("assistant")).
		Set("content", NewString("")))}
}

func (e *chatStreamEncoder) allocateToolIndex(blockIndex int) int {
	if existing, ok := e.toolIndexes[blockIndex]; ok {
		return existing
	}
	index := e.nextToolIndex
	e.nextToolIndex++
	e.toolIndexes[blockIndex] = index
	return index
}

func (e *chatStreamEncoder) frame(delta *Value) []byte {
	return e.serializeFrame(NewObject().
		Set("index", NewNumberInt(0)).
		Set("delta", delta).
		Set("finish_reason", NewNull()))
}

func (e *chatStreamEncoder) finishFrame(stopReason *StopReason) []byte {
	e.finishSent = true
	reason := StopUnknown
	if stopReason != nil {
		reason = *stopReason
	}
	body := NewObject().
		Set("id", NewString(e.chunkID)).
		Set("object", NewString("chat.completion.chunk")).
		Set("model", NewString(e.ctx.Model)).
		Set("choices", NewArray(NewObject().
			Set("index", NewNumberInt(0)).
			Set("delta", NewObject()).
			Set("finish_reason", NewString(stopReasonToOpenAIChat(reason, e.hasToolCall)))))
	if e.usage != nil && !e.usage.IsEmpty() {
		body.Set("usage", usageToOpenAIChat(e.usage))
	}
	return []byte(SerializeSSEFrame(SSEFrame{Data: body.MarshalCompact()}))
}

func (e *chatStreamEncoder) serializeFrame(choice *Value) []byte {
	body := NewObject().
		Set("id", NewString(e.chunkID)).
		Set("object", NewString("chat.completion.chunk")).
		Set("model", NewString(e.ctx.Model)).
		Set("choices", NewArray(choice))
	return []byte(SerializeSSEFrame(SSEFrame{Data: body.MarshalCompact()}))
}

func (e *chatStreamEncoder) doneFrame() []byte {
	return []byte(SerializeSSEFrame(SSEFrame{Data: DoneSentinel}))
}
