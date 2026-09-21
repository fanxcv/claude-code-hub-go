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

	// emitted 是已作为**正文增量**发出的字节。
	//
	// 为什么需要：上游存在「声明式」形态（尾帧 `delta` 或 `message` 载累计全文、而非新增片段），
	// 无对账就会把整篇二次放出——客户端看到「前半 == 后半」，且 usage 只计一份。
	// 与 responses 线的 responsesDecodedBlock.emitted 同义（stream_responses.go:27-28）。
	emitted string
	// emittedParts 是「当前块内已发过几个正文帧」，用于消歧「本帧文本恰等于已发内容」：
	// 详见 chatReconcile 的「已知边界」。
	emittedParts int
	// toolArgsEmitted 是「上游 tool_calls index → 已作为参数增量发出的字节」，用于同一类对账。
	// 不做则累计型 arguments 会产出 `{...}{...}` 非法 JSON。
	toolArgsEmitted map[int]string
	// toolArgsParts 语义同 emittedParts，按 tool index 分别计。
	toolArgsParts map[int]int

	usage      *Usage
	stopReason *StopReason
}

type chatOpenBlock struct {
	index int
	kind  chatOpenBlockKind
}

func newChatStreamDecoder(ctx ConvertCtx) StreamDecoder {
	return &chatStreamDecoder{
		ctx:             ctx,
		lastToolIndex:   -1,
		toolBlocks:      map[int]int{},
		toolArgsEmitted: map[int]string{},
		toolArgsParts:   map[int]int{},
	}
}

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
		}
	}

	if usage, ok := payload.Get("usage"); ok && !usage.IsNull() {
		d.usage = mergeUsage(d.usage, usageFromOpenAIChat(usage))
	}
}

func (d *chatStreamDecoder) consumeChoice(choice *Value, out *[]Chunk) {
	delta := choice.ObjectField("delta")
	// fromMessage 记录正文取自 `message` 字段：那是**声明式**（OpenAI 规范里 message 是整条
	// 消息的载体，不是增量片），与 `delta` 的增量语义不同，对账策略也不一样。
	fromMessage := false
	if delta == nil {
		delta = choice.ObjectField("message")
		fromMessage = true
	}
	// finish_reason 非空的帧是**末帧**：它携带的正文按声明式处理（上游常在末帧回放累计全文）。
	_, hasFinish := stringField(choice, "finish_reason")
	declared := fromMessage || hasFinish
	if delta != nil {
		// 工具调用分两趟：先落「已打开块的参数续片」，再处理会关闭当前块的推理/正文，
		// 最后才开新工具块。上游在同一帧里同时带正文与工具参数续片是合法形态（工具调用
		// 途中模型再吐一句正文），若按「正文先、工具后」消费，续片会落在刚被正文关掉的
		// 块上——工具 JSON 参数被截断成非法 JSON，且丢在暗处。详见 consumeToolCalls。
		calls := d.collectToolCalls(delta)
		for _, call := range calls {
			if _, opened := d.toolBlocks[call.index]; opened {
				d.consumeToolCall(call, declared, out)
			}
		}

		if reasoning, ok := decodeChatReasoning(delta); ok && len(reasoning) > 0 {
			d.emitReasoning(reasoning, out)
		}
		d.emitContent(fieldOrNil(delta, "content"), out, declared)

		for _, call := range calls {
			if _, opened := d.toolBlocks[call.index]; !opened {
				d.consumeToolCall(call, declared, out)
			}
		}
	}

	if finish, ok := stringField(choice, "finish_reason"); ok {
		reason := stopReasonFromOpenAIChat(finish, true)
		d.stopReason = &reason
	}
}

func (d *chatStreamDecoder) emitContent(content *Value, out *[]Chunk, declared bool) {
	if content == nil {
		return
	}
	if text, ok := content.String(); ok {
		if len(text) > 0 {
			d.emitText(text, out, declared)
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
					d.emitText(text, out, declared)
					continue
				}
			}
		}
		return
	}
}

func (d *chatStreamDecoder) emitText(text string, out *[]Chunk, declared bool) {
	if len(text) == 0 {
		return
	}
	newBlock := d.openBlock == nil || d.openBlock.kind != chatBlockText
	// 对账只在**同一块内**做：新块没有已发内容可比，emitted 随开块重置。
	if !newBlock {
		tail, isReplay := chatReconcile(d.emitted, text, declared, d.emittedParts)
		if isReplay {
			text = tail
		}
	}
	if len(text) == 0 {
		// 本帧内容已全发过（完整回放）：一个字节都不该再发。
		return
	}
	if newBlock {
		d.closeBlock(out)
		index := d.nextBlockIndex
		d.nextBlockIndex++
		d.openBlock = &chatOpenBlock{index: index, kind: chatBlockText}
		d.emitted = ""
		d.emittedParts = 0
		*out = append(*out, Chunk{
			Kind:       ChunkBlockStart,
			BlockIndex: intPtr(index),
			Block:      &Block{Kind: BlockText, Text: ""},
		})
	}
	d.textStarted = true
	d.emitted += text
	d.emittedParts++
	*out = append(*out, Chunk{
		Kind:       ChunkBlockDelta,
		BlockIndex: intPtr(d.openBlock.index),
		TextDelta:  stringPtr(text),
	})
}

// chatReconcile 判定本帧文本是否已在之前发过，并算出应发的差额（返回 isReplay 为真时 text 已被改写）。
//
// declared 为真表示本帧是**声明式**（`message` 字段帧，或 `finish_reason` 非空的末帧）：
//   - message 字段在 OpenAI 规范里承载**整条消息**，不是增量片；
//   - 末帧常被上游用来回放累计全文（生产 chat 线实测重复即属此类）。
//
// emittedParts 是已发内容跨了几个帧，只用于消歧「本帧文本恰等于已发内容」。
//
// 判据（按已发内容与本帧文本的前缀关系分档）：
//
//	本帧是严格扩展（text 比 emitted 长且以其为前缀）→ 只发差额（累计型上游）
//	两相等（text == emitted）且（声明式 或 emitted 跨多帧）→ 一字不发（完整回放）
//	emitted 以 text 为前缀（本帧更短）且声明式           → 一字不发（声明里无新字节）
//	其余                                                  → 按普通增量放行
//
// 「严格扩展」一档**不分声明与否**都生效，因为它是唯一在两类帧上都安全的前缀关系。
//
// 已知边界（ponytail: 有意留的口，换的是不再重复）：非声明式帧携带「恰等于已发内容」的文本时，
// 字节上与「合法重复」不可区分——模型输出「哈哈」就是两个 `哈` 增量（emitted=`哈`、text=`哈`），
// 而混合型上游（实测的 delta_cum_mid 形态：中间一帧突然发累计全文）也表现为 text == emitted。
// 二者的判据取「已发内容是否由多个帧拼成」：单个帧的相等只能是合法重复（放行，不静默丢字）；
// 多帧拼出的整段被原样重发，则按回放丢弃。这是启发式，不是恒真定理——取舍依仓内既有优先级
// 「宁可重复、不静默丢字」与生产实测（chat 线 37% 重复均为整段回放）共同定的。
func chatReconcile(emitted string, text string, declared bool, emittedParts int) (string, bool) {
	if emitted == "" {
		return "", false
	}
	if strings.HasPrefix(text, emitted) {
		if len(text) > len(emitted) {
			return text[len(emitted):], true
		}
		if declared || emittedParts > 1 {
			return "", true
		}
		return "", false
	}
	if declared && strings.HasPrefix(emitted, text) {
		return "", true
	}
	return "", false
}

func (d *chatStreamDecoder) emitReasoning(text string, out *[]Chunk) {
	if d.textStarted {
		// 文本块已开，推理只能排在其后 → 客户端侧会顺序错乱，放弃该增量并计数
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
		return nil
	}
	entries := make([]chatToolCallEntry, 0, len(raw.Items()))
	for _, item := range raw.Items() {
		if item == nil || !item.IsObject() {
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
//
// declared 为真表示本帧是声明式（累计型 arguments 的常见位置），按 chatDeclaredTail 对账；
// 无此对账时，累计型 arguments 会产出 `{...}{...}` 非法 JSON。
func (d *chatStreamDecoder) consumeToolCall(call chatToolCallEntry, declared bool, out *[]Chunk) {
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
		if _, opened := d.toolBlocks[call.index]; !opened {
			d.toolArgsEmitted[call.index] = ""
			d.toolArgsParts[call.index] = 0
		}
	}

	if args, ok := stringField(fn, "arguments"); ok && len(args) > 0 {
		emitted := d.toolArgsEmitted[call.index]
		if emitted != "" {
			if tail, isReplay := chatReconcile(emitted, args, declared, d.toolArgsParts[call.index]); isReplay {
				args = tail
			}
		}
		if len(args) > 0 {
			d.toolArgsEmitted[call.index] = emitted + args
			d.toolArgsParts[call.index]++
			*out = append(*out, Chunk{
				Kind:       ChunkBlockDelta,
				BlockIndex: intPtr(blockIndex),
				ArgsDelta:  stringPtr(args),
			})
		}
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
}

func newChatStreamEncoder(ctx ConvertCtx) StreamEncoder {
	return &chatStreamEncoder{
		ctx:         ctx,
		chunkID:     MakeSyntheticResponseID(ProtocolOpenAIChat),
		toolIndexes: map[int]int{},
	}
}

func (e *chatStreamEncoder) Push(chunk Chunk) [][]byte {
	if e.doneSent {
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
		out = append(out, e.startPayloadFrames(block)...)

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
				return out
			}
			out = append(out, e.frame(NewObject().Set("tool_calls", NewArray(NewObject().
				Set("index", NewNumberInt(int64(index))).
				Set("function", NewObject().Set("arguments", NewString(*chunk.ArgsDelta)))))))
		default:
			// 空增量：本线无可产出的帧
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

// startPayloadFrames 把块起始帧携带的载荷转成本线的增量帧。
//
// 为什么需要：Anthropic 的 content_block_start 允许自带 text / thinking 正文，而 Chat 线没有
// 「起始帧带正文」的位置；不转就在跨线时把整段载荷丢掉（上游一个增量都不发时正文全无）。
// 只搬当前块声明过的正文，不臆造（签名字段不在本线表示，略过）。
func (e *chatStreamEncoder) startPayloadFrames(block *Block) [][]byte {
	if block == nil {
		return nil
	}
	switch block.Kind {
	case BlockText:
		if block.Text == "" {
			return nil
		}
		return [][]byte{e.frame(NewObject().Set("content", NewString(block.Text)))}
	case BlockThinking:
		if block.Text == "" {
			return nil
		}
		return [][]byte{e.frame(NewObject().Set("reasoning_content", NewString(block.Text)))}
	default:
		return nil
	}
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
