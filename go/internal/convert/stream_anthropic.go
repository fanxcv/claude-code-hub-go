package convert

import (
	"math"
	"strconv"
)

// anthropic-messages · 流式编解码
//
// 顺序契约：message_start → content_block_start → content_block_delta* → content_block_stop →
// … → message_delta → message_stop。
// 索引映射：维护「枢纽 blockIndex ↔ Anthropic content_block index」双向映射，且 block_start
// 一定先于该块的任何 delta 发出（上游 index 与枢纽下标不是一回事）。

type anthropicStreamDecoder struct {
	ctx    ConvertCtx
	buffer string

	started bool
	ended   bool

	hubCounter int
	// indexMap 是「上游 content_block index → 枢纽 blockIndex」。
	indexMap map[int]int

	ignoredEvents   int
	malformedFrames int
}

func newAnthropicStreamDecoder(ctx ConvertCtx) StreamDecoder {
	return &anthropicStreamDecoder{ctx: ctx, indexMap: map[int]int{}}
}

func (d *anthropicStreamDecoder) IgnoredEvents() int { return d.ignoredEvents }

func (d *anthropicStreamDecoder) hubIndexFor(wireIndex int) int {
	if existing, ok := d.indexMap[wireIndex]; ok {
		return existing
	}
	mapped := d.hubCounter
	d.hubCounter++
	d.indexMap[wireIndex] = mapped
	return mapped
}

func (d *anthropicStreamDecoder) mappedIndex(wireIndex int) (int, bool) {
	mapped, ok := d.indexMap[wireIndex]
	return mapped, ok
}

// startBlockToHub 把 Anthropic 的 content_block 骨架转为枢纽块；无法映射返回 nil。
func (d *anthropicStreamDecoder) startBlockToHub(raw *Value) *Block {
	if raw == nil || !raw.IsObject() {
		return nil
	}
	blockType, _ := stringField(raw, "type")
	switch blockType {
	case "text":
		text, _ := stringField(raw, "text")
		return &Block{Kind: BlockText, Text: text}
	case "tool_use":
		rawID, _ := stringField(raw, "id")
		id := rawID
		if id == "" {
			id = MakeToolCallID("stream:" + strconv.Itoa(d.hubCounter))
		} else {
			id = NormalizeToolCallID(id)
		}
		name, _ := stringField(raw, "name")
		return &Block{
			Kind: BlockToolCall,
			ID:   id,
			Name: d.ctx.fromWireName(name),
		}
	case "thinking":
		text, _ := stringField(raw, "thinking")
		signature, _ := stringField(raw, "signature")
		return &Block{Kind: BlockThinking, Text: text, Signature: signature}
	case "redacted_thinking":
		data, _ := stringField(raw, "data")
		// 无真实 data：不得伪造
		if data == "" {
			return nil
		}
		return &Block{Kind: BlockThinking, Redacted: true, Signature: data}
	default:
		return nil
	}
}

func (d *anthropicStreamDecoder) handleFrame(frame SSEFrame) []Chunk {
	payload := parseFrameJSON(frame)
	if payload == nil {
		d.malformedFrames++
		return nil
	}
	event := ""
	if frame.HasEvent {
		event = frame.Event
	}
	if event == "" {
		event, _ = stringField(payload, "type")
	}

	switch event {
	case "message_start":
		if d.started {
			d.ignoredEvents++
			return nil
		}
		d.started = true
		message := payload.ObjectField("message")
		usage := usageFromAnthropic(fieldOrNil(message, "usage"))
		chunk := Chunk{Kind: ChunkStart}
		if !usage.IsEmpty() {
			chunk.Usage = usage
		}
		return []Chunk{chunk}

	case "content_block_start":
		wireIndex := intOrDefault(payload, "index", 0)
		block := d.startBlockToHub(payload.ObjectField("content_block"))
		if block == nil {
			d.ignoredEvents++
			return nil
		}
		return []Chunk{{Kind: ChunkBlockStart, BlockIndex: intPtr(d.hubIndexFor(wireIndex)), Block: block}}

	case "content_block_delta":
		wireIndex := intOrDefault(payload, "index", 0)
		hub, ok := d.mappedIndex(wireIndex)
		if !ok {
			d.ignoredEvents++
			return nil
		}
		delta := payload.ObjectField("delta")
		deltaType, _ := stringField(delta, "type")
		switch deltaType {
		case "text_delta":
			text, _ := stringField(delta, "text")
			return []Chunk{{Kind: ChunkBlockDelta, BlockIndex: intPtr(hub), TextDelta: stringPtr(text)}}
		case "input_json_delta":
			partial, _ := stringField(delta, "partial_json")
			return []Chunk{{Kind: ChunkBlockDelta, BlockIndex: intPtr(hub), ArgsDelta: stringPtr(partial)}}
		case "thinking_delta":
			thinking, _ := stringField(delta, "thinking")
			return []Chunk{{Kind: ChunkBlockDelta, BlockIndex: intPtr(hub), ReasoningDelta: stringPtr(thinking)}}
		case "signature_delta":
			// 枢纽块无签名增量通道，只能忽略并计数（不伪造）
			d.ignoredEvents++
			return nil
		default:
			d.ignoredEvents++
			return nil
		}

	case "content_block_stop":
		hub, ok := d.mappedIndex(intOrDefault(payload, "index", 0))
		if !ok {
			d.ignoredEvents++
			return nil
		}
		return []Chunk{{Kind: ChunkBlockStop, BlockIndex: intPtr(hub)}}

	case "message_delta":
		delta := payload.ObjectField("delta")
		rawReason, hasReason := stringField(delta, "stop_reason")
		reason := StopReasonFromAnthropic(rawReason, hasReason)
		usage := usageFromAnthropic(fieldOrNil(payload, "usage"))
		chunk := Chunk{Kind: ChunkDelta, StopReason: &reason}
		if !usage.IsEmpty() {
			chunk.Usage = usage
		}
		return []Chunk{chunk}

	case "message_stop":
		if d.ended {
			d.ignoredEvents++
			return nil
		}
		d.ended = true
		return []Chunk{{Kind: ChunkEnd}}

	default:
		// ping / error / 未知事件：忽略但计数
		d.ignoredEvents++
		return nil
	}
}

func (d *anthropicStreamDecoder) Push(chunk []byte) []Chunk {
	d.buffer += string(chunk)
	frames, rest := ParseSSEFrames(d.buffer)
	d.buffer = rest
	var out []Chunk
	for _, frame := range frames {
		out = append(out, d.handleFrame(frame)...)
	}
	return out
}

func (d *anthropicStreamDecoder) Flush() []Chunk {
	// 上游未发 message_stop 时补发 end，保证 start/end 各一次
	if !d.started || d.ended {
		return nil
	}
	d.ended = true
	return []Chunk{{Kind: ChunkEnd}}
}

// ---------------------------------------------------------------------------
// 编码：枢纽块 → 客户端 Anthropic SSE
// ---------------------------------------------------------------------------

type anthropicStreamEncoder struct {
	ctx ConvertCtx

	started   bool
	deltaSent bool
	ended     bool

	stopReason StopReason
	usage      *Usage

	wireCounter int
	// openBlock 是当前未关闭的块（枢纽下标 ↔ 本线下标）。
	openBlock *anthropicOpenBlock

	ignoredChunks    int
	autoClosedBlocks int
}

type anthropicOpenBlock struct {
	hub  int
	wire int
	// placeholderSignature 非空时，关闭本块前补发一条 signature_delta（见
	// PlaceholderThinkingSignature）：官方流式的签名走增量事件，块起始里不带。
	placeholderSignature string
}

func newAnthropicStreamEncoder(ctx ConvertCtx) StreamEncoder {
	return &anthropicStreamEncoder{ctx: ctx, stopReason: StopEndTurn}
}

func (e *anthropicStreamEncoder) IgnoredEvents() int { return e.ignoredChunks }

func (e *anthropicStreamEncoder) emit(event string, payload *Value) []byte {
	return []byte(SerializeSSEFrame(SSEFrame{
		Event:    event,
		HasEvent: true,
		Data:     payload.MarshalCompact(),
	}))
}

func (e *anthropicStreamEncoder) ensureStart() [][]byte {
	if e.started {
		return nil
	}
	e.started = true
	// 枢纽 start 块常不带 usage（上游要等 message_delta 才给），此时以 0 占位，字段名仍由
	// usageToAnthropic 统一渲染；真实数值随后由 message_delta 覆盖。
	startUsage := mergeUsage(&Usage{InputTokens: floatPtr(0), OutputTokens: floatPtr(0)}, e.usage)
	message := NewObject().
		Set("id", NewString(MakeSyntheticResponseID(ProtocolAnthropicMessages))).
		Set("type", NewString("message")).
		Set("role", NewString("assistant")).
		Set("model", NewString(e.ctx.Model)).
		Set("content", NewArray()).
		Set("stop_reason", NewNull()).
		Set("stop_sequence", NewNull()).
		Set("usage", usageToAnthropic(startUsage))
	payload := NewObject().
		Set("type", NewString("message_start")).
		Set("message", message)
	return [][]byte{e.emit("message_start", payload)}
}

// closeOpenBlock 关闭当前块；implicit 表示枢纽未发 block_stop、由编码器代发（计入 stats）。
func (e *anthropicStreamEncoder) closeOpenBlock(implicit bool) [][]byte {
	if e.openBlock == nil {
		return nil
	}
	var out [][]byte
	if signature := e.openBlock.placeholderSignature; signature != "" {
		// 签名必须在块内、stop 之前到达；顺序为 thinking_delta… → signature_delta → stop。
		payload := NewObject().
			Set("type", NewString("content_block_delta")).
			Set("index", NewNumberInt(int64(e.openBlock.wire))).
			Set("delta", NewObject().
				Set("type", NewString("signature_delta")).
				Set("signature", NewString(signature)))
		out = append(out, e.emit("content_block_delta", payload))
	}
	payload := NewObject().
		Set("type", NewString("content_block_stop")).
		Set("index", NewNumberInt(int64(e.openBlock.wire)))
	e.openBlock = nil
	if implicit {
		e.autoClosedBlocks++
	}
	out = append(out, e.emit("content_block_stop", payload))
	return out
}

func (e *anthropicStreamEncoder) ensureDelta() [][]byte {
	if e.deltaSent {
		return nil
	}
	e.deltaSent = true
	payload := NewObject().
		Set("type", NewString("message_delta")).
		Set("delta", NewObject().
			Set("stop_reason", NewString(stopReasonToAnthropic(e.stopReason))).
			Set("stop_sequence", NewNull()))
	if e.usage != nil && !e.usage.IsEmpty() {
		payload.Set("usage", usageToAnthropic(e.usage))
	}
	return [][]byte{e.emit("message_delta", payload)}
}

// skeletonFor 生成本线可用的块起始骨架；无表示返回 nil。
func (e *anthropicStreamEncoder) skeletonFor(block *Block) *Value {
	if block == nil {
		return nil
	}
	switch block.Kind {
	case BlockText:
		return NewObject().Set("type", NewString("text")).Set("text", NewString(block.Text))
	case BlockToolCall:
		id := block.ID
		if id == "" {
			id = MakeToolCallID("stream:" + strconv.Itoa(e.wireCounter))
		} else {
			id = NormalizeToolCallID(id)
		}
		return NewObject().
			Set("type", NewString("tool_use")).
			Set("id", NewString(id)).
			Set("name", NewString(e.ctx.toWireName(block.Name))).
			Set("input", NewObject())
	case BlockThinking:
		if block.Redacted {
			// 无真实 data：不得伪造
			if block.Signature == "" {
				return nil
			}
			return NewObject().
				Set("type", NewString("redacted_thinking")).
				Set("data", NewString(block.Signature))
		}
		skeleton := NewObject().
			Set("type", NewString("thinking")).
			Set("thinking", NewString(block.Text))
		if block.Signature != "" {
			skeleton.Set("signature", NewString(block.Signature))
		}
		return skeleton
	default:
		// opaque 块在流式下不适用
		return nil
	}
}

func (e *anthropicStreamEncoder) Push(chunk Chunk) [][]byte {
	var out [][]byte
	switch chunk.Kind {
	case ChunkStart:
		if chunk.Usage != nil {
			e.usage = mergeUsage(e.usage, chunk.Usage)
		}
		out = append(out, e.ensureStart()...)
		return out

	case ChunkBlockStart:
		skeleton := e.skeletonFor(chunk.Block)
		if skeleton == nil {
			e.ignoredChunks++
			return out
		}
		out = append(out, e.ensureStart()...)
		out = append(out, e.closeOpenBlock(true)...)
		hub := e.wireCounter
		if chunk.BlockIndex != nil {
			hub = *chunk.BlockIndex
		}
		wire := e.wireCounter
		e.wireCounter++
		// 无签名的思考块（来自 chat/responses 线上游）：补占位签名，但签名走 signature_delta，
		// 不塞进块起始——与官方流式形态一致。有真实签名时不动（保持既有透传形状）。
		placeholderSignature := ""
		if block := chunk.Block; block != nil && block.Kind == BlockThinking &&
			!block.Redacted && block.Signature == "" && e.ctx.shouldPlaceholderThinkingSignature() {
			placeholderSignature = PlaceholderThinkingSignature()
		}
		e.openBlock = &anthropicOpenBlock{hub: hub, wire: wire, placeholderSignature: placeholderSignature}
		payload := NewObject().
			Set("type", NewString("content_block_start")).
			Set("index", NewNumberInt(int64(wire))).
			Set("content_block", skeleton)
		out = append(out, e.emit("content_block_start", payload))
		return out

	case ChunkBlockDelta:
		if chunk.BlockIndex == nil || e.openBlock == nil || e.openBlock.hub != *chunk.BlockIndex {
			// 未先收到该块的 block_start：无 id/name 可用，不得凭空补块起始事件
			e.ignoredChunks++
			return out
		}
		index := e.openBlock.wire
		emitDelta := func(delta *Value) {
			payload := NewObject().
				Set("type", NewString("content_block_delta")).
				Set("index", NewNumberInt(int64(index))).
				Set("delta", delta)
			out = append(out, e.emit("content_block_delta", payload))
		}
		if chunk.TextDelta != nil {
			emitDelta(NewObject().Set("type", NewString("text_delta")).Set("text", NewString(*chunk.TextDelta)))
		}
		if chunk.ArgsDelta != nil {
			emitDelta(NewObject().
				Set("type", NewString("input_json_delta")).
				Set("partial_json", NewString(*chunk.ArgsDelta)))
		}
		if chunk.ReasoningDelta != nil {
			emitDelta(NewObject().
				Set("type", NewString("thinking_delta")).
				Set("thinking", NewString(*chunk.ReasoningDelta)))
		}
		return out

	case ChunkBlockStop:
		if chunk.BlockIndex == nil || e.openBlock == nil || e.openBlock.hub != *chunk.BlockIndex {
			e.ignoredChunks++
			return out
		}
		out = append(out, e.closeOpenBlock(false)...)
		return out

	case ChunkDelta:
		if chunk.StopReason != nil {
			e.stopReason = *chunk.StopReason
		}
		if chunk.Usage != nil {
			e.usage = mergeUsage(e.usage, chunk.Usage)
		}
		out = append(out, e.ensureStart()...)
		out = append(out, e.closeOpenBlock(true)...)
		out = append(out, e.ensureDelta()...)
		return out

	case ChunkEnd:
		out = append(out, e.ensureStart()...)
		out = append(out, e.closeOpenBlock(true)...)
		out = append(out, e.ensureDelta()...)
		out = append(out, e.emit("message_stop", NewObject().Set("type", NewString("message_stop"))))
		e.ended = true
		return out

	default:
		e.ignoredChunks++
		return out
	}
}

func (e *anthropicStreamEncoder) Flush() [][]byte {
	// 从未收到 start：不产出半个流
	if !e.started {
		return nil
	}
	var out [][]byte
	out = append(out, e.closeOpenBlock(true)...)
	out = append(out, e.ensureDelta()...)
	if !e.ended {
		e.ended = true
		out = append(out, e.emit("message_stop", NewObject().Set("type", NewString("message_stop"))))
	}
	return out
}

// ---------------------------------------------------------------------------
// 局部工具
// ---------------------------------------------------------------------------

// intOrDefault 取整数字段（非有限数字或缺省时用 fallback）。
func intOrDefault(object *Value, key string, fallback int) int {
	value, ok := numberField(object, key)
	if !ok || value == nil {
		return fallback
	}
	if math.IsNaN(*value) || math.IsInf(*value, 0) {
		return fallback
	}
	return int(*value)
}

func floatPtr(value float64) *float64 { return &value }
