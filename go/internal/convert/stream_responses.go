package convert

import "strconv"

// openai-responses · 流式编解码
//
// 解码：上游 Responses SSE → 枢纽块；编码：枢纽块 → 客户端 SSE 字节。
// 本线的两条硬约束：
//   - 每个事件都带从 0 起单调递增的 `sequence_number`；
//   - `response.completed` / `response.incomplete` 必须是最后一个事件。
//
// 索引映射：本线的 `output_index` 覆盖所有 output item，与枢纽 blockIndex 不是一回事，
// 必须显式双向映射。

type responsesDecodedKind string

const (
	responsesKindText     responsesDecodedKind = "text"
	responsesKindToolCall responsesDecodedKind = "tool_call"
	responsesKindThinking responsesDecodedKind = "thinking"
)

type responsesDecodedBlock struct {
	blockIndex  int
	outputIndex int
	kind        responsesDecodedKind
	// emitted 是已作为 delta 发出的载荷（用于判断 done 帧是否需要补发全文）。
	emitted string
	// seed 是读到的、未发出的 part 初始文本（content_part.added 兜底）。
	seed   string
	closed bool
}

// responsesDoneKinds 把 done 事件映射到块类型（按 output_index 兜底匹配用）。
var responsesDoneKinds = map[string]responsesDecodedKind{
	"response.output_text.done":             responsesKindText,
	"response.refusal.done":                 responsesKindText,
	"response.reasoning_text.done":          responsesKindThinking,
	"response.reasoning_summary_text.done":  responsesKindThinking,
	"response.function_call_arguments.done": responsesKindToolCall,
}

type responsesStreamDecoder struct {
	ctx ConvertCtx

	buffer   string
	started  bool
	finished bool

	sentDelta      bool
	nextBlockIndex int
	usage          *Usage
	stopReason     *StopReason

	byKey             map[string]*responsesDecodedBlock
	openBlocks        []*responsesDecodedBlock
	seenOutputIndexes map[int]bool

	ignoredEvents int
}

func newResponsesStreamDecoder(ctx ConvertCtx) StreamDecoder {
	return &responsesStreamDecoder{
		ctx:               ctx,
		byKey:             map[string]*responsesDecodedBlock{},
		seenOutputIndexes: map[int]bool{},
	}
}

func (d *responsesStreamDecoder) IgnoredEvents() int { return d.ignoredEvents }

func (d *responsesStreamDecoder) ensureStart(out *[]Chunk) {
	if d.started {
		return
	}
	d.started = true
	*out = append(*out, Chunk{Kind: ChunkStart})
}

func (d *responsesStreamDecoder) startBlock(
	out *[]Chunk,
	key string,
	kind responsesDecodedKind,
	block *Block,
	outputIndex int,
) *responsesDecodedBlock {
	if existing, ok := d.byKey[key]; ok {
		return existing
	}
	blockIndex := d.nextBlockIndex
	d.nextBlockIndex++
	decoded := &responsesDecodedBlock{blockIndex: blockIndex, outputIndex: outputIndex, kind: kind}
	d.byKey[key] = decoded
	d.openBlocks = append(d.openBlocks, decoded)
	d.seenOutputIndexes[outputIndex] = true
	*out = append(*out, Chunk{Kind: ChunkBlockStart, BlockIndex: intPtr(blockIndex), Block: block})
	return decoded
}

func (d *responsesStreamDecoder) closeBlock(out *[]Chunk, decoded *responsesDecodedBlock) {
	if decoded.closed {
		return
	}
	decoded.closed = true
	for i, open := range d.openBlocks {
		if open == decoded {
			d.openBlocks = append(d.openBlocks[:i], d.openBlocks[i+1:]...)
			break
		}
	}
	*out = append(*out, Chunk{Kind: ChunkBlockStop, BlockIndex: intPtr(decoded.blockIndex)})
}

func (d *responsesStreamDecoder) closeAll(out *[]Chunk) {
	for len(d.openBlocks) > 0 {
		d.closeBlock(out, d.openBlocks[0])
	}
}

func (d *responsesStreamDecoder) closeByOutputIndex(out *[]Chunk, outputIndex int) bool {
	matched := false
	for _, decoded := range append([]*responsesDecodedBlock{}, d.openBlocks...) {
		if decoded.outputIndex != outputIndex {
			continue
		}
		d.closeBlock(out, decoded)
		matched = true
	}
	return matched
}

func (d *responsesStreamDecoder) findBlock(payload *Value, kind responsesDecodedKind) *responsesDecodedBlock {
	contentIndex := intOrDefault(payload, "content_index", 0)
	outputIndex := intOrDefault(payload, "output_index", 0)
	itemID, _ := stringField(payload, "item_id")
	for _, key := range []string{
		"part:" + strconv.Itoa(outputIndex) + ":" + strconv.Itoa(contentIndex),
		"item:" + itemID,
	} {
		if decoded, ok := d.byKey[key]; ok && decoded.kind == kind {
			return decoded
		}
	}
	var candidates []*responsesDecodedBlock
	for _, decoded := range d.openBlocks {
		if decoded.kind == kind {
			candidates = append(candidates, decoded)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	for _, decoded := range candidates {
		if decoded.outputIndex == outputIndex {
			return decoded
		}
	}
	return candidates[len(candidates)-1]
}

func (d *responsesStreamDecoder) emitDeltaChunk(out *[]Chunk, decoded *responsesDecodedBlock, delta string) {
	decoded.emitted += delta
	chunk := Chunk{Kind: ChunkBlockDelta, BlockIndex: intPtr(decoded.blockIndex)}
	switch decoded.kind {
	case responsesKindText:
		chunk.TextDelta = stringPtr(delta)
	case responsesKindToolCall:
		chunk.ArgsDelta = stringPtr(delta)
	default:
		chunk.ReasoningDelta = stringPtr(delta)
	}
	*out = append(*out, chunk)
}

func (d *responsesStreamDecoder) captureUsage(raw *Value) {
	if raw == nil || !raw.IsObject() {
		return
	}
	d.usage = mergeUsage(d.usage, usageFromResponses(fieldOrNil(raw, "usage")))
}

func (d *responsesStreamDecoder) emitDelta(out *[]Chunk) {
	if d.sentDelta {
		return
	}
	d.sentDelta = true
	chunk := Chunk{Kind: ChunkDelta}
	if !d.usage.IsEmpty() {
		chunk.Usage = d.usage
	}
	if d.stopReason != nil {
		chunk.StopReason = d.stopReason
	}
	*out = append(*out, chunk)
}

func (d *responsesStreamDecoder) terminal(out *[]Chunk, response *Value) {
	if response != nil {
		d.captureUsage(response)
		status, hasStatus := stringField(response, "status")
		incomplete := response.ObjectField("incomplete_details")
		incompleteReason, _ := stringField(incomplete, "reason")
		reason := stopReasonFromResponses(status, hasStatus, incompleteReason)
		d.stopReason = &reason
	}
	d.closeAll(out)
	d.emitDelta(out)
	*out = append(*out, Chunk{Kind: ChunkEnd})
	d.finished = true
}

func (d *responsesStreamDecoder) handleOutputItemAdded(out *[]Chunk, payload *Value) {
	item := payload.ObjectField("item")
	if item == nil {
		return
	}
	d.ensureStart(out)
	outputIndex := intOrDefault(payload, "output_index", 0)
	itemID, _ := stringField(item, "id")
	if itemID == "" {
		if fallback, ok := stringField(payload, "item_id"); ok && fallback != "" {
			itemID = fallback
		} else {
			itemID = strconv.Itoa(outputIndex)
		}
	}
	d.seenOutputIndexes[outputIndex] = true
	d.captureUsage(payload)

	itemType, _ := stringField(item, "type")
	switch itemType {
	case "function_call":
		callID, _ := stringField(item, "call_id")
		if callID == "" {
			callID = itemID
		}
		name, _ := stringField(item, "name")
		block := &Block{
			Kind: BlockToolCall,
			ID:   callID,
			// 带 id/name 的块起始帧：名字须还原成客户端原名，否则客户端路由不到自己的工具
			Name: d.ctx.fromWireName(name),
		}
		decoded := d.startBlock(out, "item:"+itemID, responsesKindToolCall, block, outputIndex)
		// 上游可能在 added 帧就带全量参数（如本地伪造流），一并转成分片
		if args, ok := stringField(item, "arguments"); ok && len(args) > 0 {
			d.emitDeltaChunk(out, decoded, args)
		}
	case "message":
		// 文本由 content_part.added / output_text.delta 承载；此处不取 item.content，否则重复
	case "reasoning":
		text := responsesReasoningTextOf(item)
		if len(text) == 0 {
			return
		}
		decoded := d.startBlock(out, "item:"+itemID, responsesKindThinking,
			&Block{Kind: BlockThinking}, outputIndex)
		d.emitDeltaChunk(out, decoded, text)
	default:
		// web_search_call 等：流式下无枢纽表示
		d.ignoredEvents++
	}
}

func (d *responsesStreamDecoder) handleContentPartAdded(out *[]Chunk, payload *Value) {
	part := payload.ObjectField("part")
	partType := "output_text"
	if part != nil {
		if value, ok := stringField(part, "type"); ok && value != "" {
			partType = value
		}
	}
	if partType != "output_text" && partType != "refusal" && partType != "text" {
		return
	}
	d.ensureStart(out)
	outputIndex := intOrDefault(payload, "output_index", 0)
	contentIndex := intOrDefault(payload, "content_index", 0)
	decoded := d.startBlock(out,
		"part:"+strconv.Itoa(outputIndex)+":"+strconv.Itoa(contentIndex),
		responsesKindText, &Block{Kind: BlockText}, outputIndex)
	if decoded.seed == "" {
		if seed, ok := stringField(part, "text"); ok && len(seed) > 0 {
			decoded.seed = seed
		}
	}
}

func (d *responsesStreamDecoder) handleDelta(out *[]Chunk, eventType string, payload *Value) {
	delta, ok := stringField(payload, "delta")
	if !ok || len(delta) == 0 {
		return
	}
	d.ensureStart(out)
	kind := responsesKindThinking
	switch eventType {
	case "response.function_call_arguments.delta":
		kind = responsesKindToolCall
	case "response.output_text.delta", "response.refusal.delta":
		kind = responsesKindText
	}
	outputIndex := intOrDefault(payload, "output_index", 0)
	itemID, _ := stringField(payload, "item_id")
	if itemID == "" {
		itemID = strconv.Itoa(outputIndex)
	}
	decoded := d.findBlock(payload, kind)
	if decoded == nil {
		key := "item:" + itemID
		if kind == responsesKindText {
			key = "part:" + strconv.Itoa(outputIndex) + ":" + strconv.Itoa(intOrDefault(payload, "content_index", 0))
		}
		var block *Block
		switch kind {
		case responsesKindToolCall:
			block = &Block{Kind: BlockToolCall, ID: itemID}
		case responsesKindThinking:
			block = &Block{Kind: BlockThinking}
		default:
			block = &Block{Kind: BlockText}
		}
		decoded = d.startBlock(out, key, kind, block, outputIndex)
	}
	d.emitDeltaChunk(out, decoded, delta)
}

func (d *responsesStreamDecoder) handleDone(out *[]Chunk, eventType string, payload *Value) {
	kind := responsesDoneKinds[eventType]
	d.ensureStart(out)
	decoded := d.findBlock(payload, kind)
	full, hasFull := stringField(payload, "text")
	_ = hasFull
	if full == "" {
		if refusal, ok := stringField(payload, "refusal"); ok {
			full = refusal
		}
	}
	if full == "" {
		if args, ok := stringField(payload, "arguments"); ok {
			full = args
		}
	}
	if decoded == nil {
		// 上游跳过 added/delta 直接给 done：按载荷补一个完整块，避免丢内容
		if len(full) == 0 {
			return
		}
		outputIndex := intOrDefault(payload, "output_index", 0)
		itemID, _ := stringField(payload, "item_id")
		if itemID == "" {
			itemID = strconv.Itoa(outputIndex)
		}
		var block *Block
		switch kind {
		case responsesKindToolCall:
			name, _ := stringField(payload, "name")
			block = &Block{Kind: BlockToolCall, ID: itemID, Name: d.ctx.fromWireName(name)}
		case responsesKindThinking:
			block = &Block{Kind: BlockThinking}
		default:
			block = &Block{Kind: BlockText}
		}
		key := "item:" + itemID
		if kind == responsesKindText {
			key = "part:" + strconv.Itoa(outputIndex) + ":" + strconv.Itoa(intOrDefault(payload, "content_index", 0))
		}
		created := d.startBlock(out, key, kind, block, outputIndex)
		d.emitDeltaChunk(out, created, full)
		d.closeBlock(out, created)
		return
	}
	if len(decoded.emitted) == 0 {
		fallback := full
		if fallback == "" {
			fallback = decoded.seed
		}
		if len(fallback) > 0 {
			d.emitDeltaChunk(out, decoded, fallback)
		}
	}
	d.closeBlock(out, decoded)
}

func (d *responsesStreamDecoder) handleOutputItemDone(out *[]Chunk, payload *Value) {
	outputIndex := intOrDefault(payload, "output_index", 0)
	if outputIndex >= 0 && d.seenOutputIndexes[outputIndex] {
		d.closeByOutputIndex(out, outputIndex)
		return
	}
	// 只收到 done 帧（无 added）的上游：按 item 载荷补出内容后关闭
	item := payload.ObjectField("item")
	if item == nil {
		return
	}
	itemID, _ := stringField(item, "id")
	if itemID == "" {
		if fallback, ok := stringField(payload, "item_id"); ok && fallback != "" {
			itemID = fallback
		} else {
			itemID = strconv.Itoa(outputIndex)
		}
	}
	if _, ok := d.byKey["item:"+itemID]; ok {
		d.closeByOutputIndex(out, outputIndex)
		return
	}
	d.ensureStart(out)
	itemType, _ := stringField(item, "type")
	switch itemType {
	case "function_call":
		callID, _ := stringField(item, "call_id")
		if callID == "" {
			callID = itemID
		}
		name, _ := stringField(item, "name")
		decoded := d.startBlock(out, "item:"+itemID, responsesKindToolCall,
			&Block{Kind: BlockToolCall, ID: callID, Name: d.ctx.fromWireName(name)}, outputIndex)
		if args, ok := stringField(item, "arguments"); ok && len(args) > 0 {
			d.emitDeltaChunk(out, decoded, args)
		}
		d.closeBlock(out, decoded)
	case "reasoning":
		text := responsesReasoningTextOf(item)
		if len(text) == 0 {
			return
		}
		decoded := d.startBlock(out, "item:"+itemID, responsesKindThinking,
			&Block{Kind: BlockThinking}, outputIndex)
		d.emitDeltaChunk(out, decoded, text)
		d.closeBlock(out, decoded)
	case "message":
		contents := item.ArrayField("content")
		for index, part := range contents {
			text, ok := stringField(part, "text")
			if !ok || len(text) == 0 {
				continue
			}
			decoded := d.startBlock(out,
				"part:"+strconv.Itoa(outputIndex)+":"+strconv.Itoa(index),
				responsesKindText, &Block{Kind: BlockText}, outputIndex)
			d.emitDeltaChunk(out, decoded, text)
			d.closeBlock(out, decoded)
		}
	}
}

func (d *responsesStreamDecoder) handleFrame(frame SSEFrame) []Chunk {
	var out []Chunk
	payload := parseFrameJSON(frame)
	if payload == nil {
		// 畸形 / 心跳 / 无法解析的帧：忽略，不抛错
		d.ignoredEvents++
		return out
	}
	eventType, _ := stringField(payload, "type")
	if eventType == "" && frame.HasEvent {
		eventType = frame.Event
	}

	switch eventType {
	case "response.created", "response.queued", "response.in_progress":
		d.ensureStart(&out)
		d.captureUsage(payload.ObjectField("response"))
	case "response.output_item.added":
		d.handleOutputItemAdded(&out, payload)
	case "response.content_part.added":
		d.handleContentPartAdded(&out, payload)
	case "response.output_text.delta", "response.refusal.delta",
		"response.reasoning_text.delta", "response.reasoning_summary_text.delta",
		"response.function_call_arguments.delta":
		d.handleDelta(&out, eventType, payload)
	case "response.output_text.done", "response.refusal.done",
		"response.reasoning_text.done", "response.reasoning_summary_text.done",
		"response.function_call_arguments.done":
		d.handleDone(&out, eventType, payload)
	case "response.output_item.done":
		d.handleOutputItemDone(&out, payload)
	case "response.content_part.done":
		// 内容分片结束。上游通常先发 output_text.done（块已在那里关闭，closeBlock 幂等），
		// 本事件是多余确认；但仍有只发本事件的上游，故按分片位置兜底关闭。
		// 关键：识别它而**不计入 ignoredEvents**——那是「无法映射」的健康信号，
		// 把可识别的冗余事件算进去会让每个正常响应都背上一个假损失。
		if part := d.findBlock(payload, responsesKindText); part != nil {
			d.closeBlock(&out, part)
		}
	case "response.completed", "response.incomplete", "response.done", "response.failed":
		response := payload.ObjectField("response")
		if response == nil {
			response = payload
		}
		d.terminal(&out, response)
	default:
		// 未映射事件（error / 各类工具生命周期）：静默忽略
		d.ignoredEvents++
	}
	return out
}

func (d *responsesStreamDecoder) Push(chunk []byte) []Chunk {
	if d.finished {
		return nil
	}
	d.buffer += string(chunk)
	frames, rest := ParseSSEFrames(d.buffer)
	d.buffer = rest
	var out []Chunk
	for _, frame := range frames {
		out = append(out, d.handleFrame(frame)...)
	}
	return out
}

func (d *responsesStreamDecoder) Flush() []Chunk {
	if d.finished {
		return nil
	}
	var out []Chunk
	d.ensureStart(&out)
	d.closeAll(&out)
	d.emitDelta(&out)
	out = append(out, Chunk{Kind: ChunkEnd})
	d.finished = true
	return out
}

// responsesReasoningTextOf 拼接 reasoning item 的 summary 与 content 文本。
func responsesReasoningTextOf(item *Value) string {
	var builder []byte
	for _, part := range item.ArrayField("summary") {
		if text, ok := stringField(part, "text"); ok {
			builder = append(builder, text...)
		}
	}
	for _, part := range item.ArrayField("content") {
		if text, ok := stringField(part, "text"); ok {
			builder = append(builder, text...)
		}
	}
	return string(builder)
}

// ---------------------------------------------------------------------------
// 编码：枢纽块 → 客户端 Responses SSE
// ---------------------------------------------------------------------------

type responsesEncoderBlockState struct {
	blockIndex  int
	outputIndex int
	itemID      string
	item        *Value
	kind        responsesDecodedKind
	text        string
	args        string
	reasoning   string
	// skipped 表示枢纽块在本线无表示（如仅有签名的 thinking）：其 delta/stop 一律忽略。
	skipped bool
	closed  bool
}

type responsesStreamEncoder struct {
	ctx        ConvertCtx
	responseID string

	sequence        int
	started         bool
	finished        bool
	nextOutputIndex int
	hasToolCall     bool
	usage           *Usage
	stopReason      *StopReason

	states      map[int]*responsesEncoderBlockState
	openOrder   []int
	outputItems []*Value

	ignoredEvents int
}

func newResponsesStreamEncoder(ctx ConvertCtx) StreamEncoder {
	return &responsesStreamEncoder{
		ctx:        ctx,
		responseID: MakeSyntheticResponseID(ProtocolOpenAIResponses),
		states:     map[int]*responsesEncoderBlockState{},
	}
}

func (e *responsesStreamEncoder) IgnoredEvents() int { return e.ignoredEvents }

func (e *responsesStreamEncoder) frame(eventType string, payload *Value) []byte {
	body := NewObject().
		Set("type", NewString(eventType)).
		Set("sequence_number", NewNumberInt(int64(e.sequence)))
	e.sequence++
	for _, member := range payload.Members() {
		body.Set(member.Key, member.Value)
	}
	return []byte(SerializeSSEFrame(SSEFrame{
		Event:    eventType,
		HasEvent: true,
		Data:     body.MarshalCompact(),
	}))
}

func (e *responsesStreamEncoder) ensureCreated(out *[][]byte) {
	if e.started {
		return
	}
	e.started = true
	*out = append(*out, e.frame("response.created", NewObject().Set("response", NewObject().
		Set("id", NewString(e.responseID)).
		Set("object", NewString("response")).
		Set("model", NewString(e.ctx.Model)).
		Set("status", NewString("in_progress")).
		Set("output", NewArray()))))
}

func (e *responsesStreamEncoder) closeBlock(out *[][]byte, blockIndex int) {
	state, ok := e.states[blockIndex]
	if !ok || state.closed {
		return
	}
	state.closed = true
	for i, open := range e.openOrder {
		if open == blockIndex {
			e.openOrder = append(e.openOrder[:i], e.openOrder[i+1:]...)
			break
		}
	}
	if state.skipped {
		return
	}

	itemID := state.itemID
	outputIndex := state.outputIndex
	switch state.kind {
	case responsesKindText:
		part := NewObject().
			Set("type", NewString("output_text")).
			Set("text", NewString(state.text)).
			Set("annotations", NewArray())
		state.item.Set("status", NewString("completed"))
		state.item.Set("content", NewArray(part))
		*out = append(*out, e.frame("response.output_text.done", NewObject().
			Set("item_id", NewString(itemID)).
			Set("output_index", NewNumberInt(int64(outputIndex))).
			Set("content_index", NewNumberInt(0)).
			Set("text", NewString(state.text))))
		*out = append(*out, e.frame("response.content_part.done", NewObject().
			Set("item_id", NewString(itemID)).
			Set("output_index", NewNumberInt(int64(outputIndex))).
			Set("content_index", NewNumberInt(0)).
			Set("part", part)))
	case responsesKindToolCall:
		state.item.Set("status", NewString("completed"))
		state.item.Set("arguments", NewString(state.args))
		*out = append(*out, e.frame("response.function_call_arguments.done", NewObject().
			Set("item_id", NewString(itemID)).
			Set("output_index", NewNumberInt(int64(outputIndex))).
			Set("arguments", NewString(state.args))))
	default:
		part := NewObject().
			Set("type", NewString("summary_text")).
			Set("text", NewString(state.reasoning))
		state.item.Set("summary", NewArray(part))
		*out = append(*out, e.frame("response.reasoning_summary_text.done", NewObject().
			Set("item_id", NewString(itemID)).
			Set("output_index", NewNumberInt(int64(outputIndex))).
			Set("summary_index", NewNumberInt(0)).
			Set("text", NewString(state.reasoning))))
		*out = append(*out, e.frame("response.reasoning_summary_part.done", NewObject().
			Set("item_id", NewString(itemID)).
			Set("output_index", NewNumberInt(int64(outputIndex))).
			Set("summary_index", NewNumberInt(0)).
			Set("part", part)))
	}
	*out = append(*out, e.frame("response.output_item.done", NewObject().
		Set("output_index", NewNumberInt(int64(outputIndex))).
		Set("item", state.item)))
	e.outputItems = append(e.outputItems, state.item)
}

func (e *responsesStreamEncoder) closeAll(out *[][]byte) {
	for len(e.openOrder) > 0 {
		e.closeBlock(out, e.openOrder[0])
	}
}

func (e *responsesStreamEncoder) openBlock(out *[][]byte, chunk Chunk) {
	block := chunk.Block
	e.ensureCreated(out)
	// 顺序契约：块必须关闭后才能开下一个
	e.closeAll(out)
	if block == nil {
		return
	}
	blockIndex := e.nextOutputIndex
	if chunk.BlockIndex != nil {
		blockIndex = *chunk.BlockIndex
	}
	if _, exists := e.states[blockIndex]; exists {
		return
	}
	outputIndex := e.nextOutputIndex
	e.nextOutputIndex++

	switch block.Kind {
	case BlockText:
		itemID := "msg_" + strconv.Itoa(outputIndex)
		state := &responsesEncoderBlockState{
			blockIndex:  blockIndex,
			outputIndex: outputIndex,
			itemID:      itemID,
			item: NewObject().
				Set("id", NewString(itemID)).
				Set("type", NewString("message")).
				Set("role", NewString("assistant")).
				Set("status", NewString("in_progress")).
				Set("content", NewArray()),
			kind: responsesKindText,
		}
		e.states[blockIndex] = state
		e.openOrder = append(e.openOrder, blockIndex)
		*out = append(*out, e.frame("response.output_item.added", NewObject().
			Set("output_index", NewNumberInt(int64(outputIndex))).
			Set("item", state.item)))
		*out = append(*out, e.frame("response.content_part.added", NewObject().
			Set("item_id", NewString(itemID)).
			Set("output_index", NewNumberInt(int64(outputIndex))).
			Set("content_index", NewNumberInt(0)).
			Set("part", NewObject().
				Set("type", NewString("output_text")).
				Set("text", NewString("")).
				Set("annotations", NewArray()))))
		return

	case BlockToolCall:
		itemID := "fc_" + strconv.Itoa(outputIndex)
		state := &responsesEncoderBlockState{
			blockIndex:  blockIndex,
			outputIndex: outputIndex,
			itemID:      itemID,
			item: NewObject().
				Set("id", NewString(itemID)).
				Set("type", NewString("function_call")).
				Set("status", NewString("in_progress")).
				Set("call_id", NewString(block.ID)).
				Set("name", NewString(e.ctx.toWireName(block.Name))).
				Set("arguments", NewString("")),
			kind: responsesKindToolCall,
		}
		e.states[blockIndex] = state
		e.openOrder = append(e.openOrder, blockIndex)
		e.hasToolCall = true
		// 先发带 id/name 的块起始事件，再发参数分片
		*out = append(*out, e.frame("response.output_item.added", NewObject().
			Set("output_index", NewNumberInt(int64(outputIndex))).
			Set("item", state.item)))
		if len(block.Args) > 0 {
			state.args = block.Args
			*out = append(*out, e.frame("response.function_call_arguments.delta", NewObject().
				Set("item_id", NewString(itemID)).
				Set("output_index", NewNumberInt(int64(outputIndex))).
				Set("delta", NewString(block.Args))))
		}
		return

	case BlockThinking:
		if block.Redacted {
			// 不透明推理载荷：只搬运不生成，以 encrypted_content 原样发出
			if block.Signature == "" {
				return // 无真实载荷：整块跳过（不得伪造）
			}
			item := NewObject().
				Set("id", NewString("rs_"+strconv.Itoa(outputIndex))).
				Set("type", NewString("reasoning")).
				Set("encrypted_content", NewString(block.Signature))
			*out = append(*out, e.frame("response.output_item.added", NewObject().
				Set("output_index", NewNumberInt(int64(outputIndex))).
				Set("item", item)))
			*out = append(*out, e.frame("response.output_item.done", NewObject().
				Set("output_index", NewNumberInt(int64(outputIndex))).
				Set("item", item)))
			e.outputItems = append(e.outputItems, item)
			return
		}
		itemID := "rs_" + strconv.Itoa(outputIndex)
		state := &responsesEncoderBlockState{
			blockIndex:  blockIndex,
			outputIndex: outputIndex,
			itemID:      itemID,
			item: NewObject().
				Set("id", NewString(itemID)).
				Set("type", NewString("reasoning")).
				Set("summary", NewArray()),
			kind: responsesKindThinking,
		}
		e.states[blockIndex] = state
		e.openOrder = append(e.openOrder, blockIndex)
		*out = append(*out, e.frame("response.output_item.added", NewObject().
			Set("output_index", NewNumberInt(int64(outputIndex))).
			Set("item", state.item)))
		*out = append(*out, e.frame("response.reasoning_summary_part.added", NewObject().
			Set("item_id", NewString(itemID)).
			Set("output_index", NewNumberInt(int64(outputIndex))).
			Set("summary_index", NewNumberInt(0)).
			Set("part", NewObject().
				Set("type", NewString("summary_text")).
				Set("text", NewString("")))))
		if len(block.Text) > 0 {
			state.reasoning = block.Text
			*out = append(*out, e.frame("response.reasoning_summary_text.delta", NewObject().
				Set("item_id", NewString(itemID)).
				Set("output_index", NewNumberInt(int64(outputIndex))).
				Set("summary_index", NewNumberInt(0)).
				Set("delta", NewString(block.Text))))
		}
		return

	case BlockOpaque:
		if block.Wire == ProtocolOpenAIResponses && block.Value != nil && block.Value.IsObject() {
			// 本线私有 item（web_search_call 等）原样搬运：整块一个 item
			item := block.Value
			if _, ok := stringField(item, "type"); ok {
				*out = append(*out, e.frame("response.output_item.added", NewObject().
					Set("output_index", NewNumberInt(int64(outputIndex))).
					Set("item", item)))
				*out = append(*out, e.frame("response.output_item.done", NewObject().
					Set("output_index", NewNumberInt(int64(outputIndex))).
					Set("item", item)))
				e.outputItems = append(e.outputItems, item)
			}
			return
		}
	}

	// 其余块（外线 opaque 等）在本线流式下无表示：整块忽略
	e.ignoredEvents++
	state := &responsesEncoderBlockState{
		blockIndex:  blockIndex,
		outputIndex: outputIndex,
		itemID:      "skipped_" + strconv.Itoa(outputIndex),
		item:        NewObject(),
		kind:        responsesKindText,
		skipped:     true,
	}
	e.states[blockIndex] = state
	e.openOrder = append(e.openOrder, blockIndex)
}

func (e *responsesStreamEncoder) applyDelta(out *[][]byte, chunk Chunk) {
	if chunk.BlockIndex == nil {
		return
	}
	state, ok := e.states[*chunk.BlockIndex]
	if !ok || state.skipped || state.closed {
		return
	}
	itemID := state.itemID
	outputIndex := state.outputIndex

	if chunk.TextDelta != nil && state.kind == responsesKindText {
		state.text += *chunk.TextDelta
		*out = append(*out, e.frame("response.output_text.delta", NewObject().
			Set("item_id", NewString(itemID)).
			Set("output_index", NewNumberInt(int64(outputIndex))).
			Set("content_index", NewNumberInt(0)).
			Set("delta", NewString(*chunk.TextDelta))))
		return
	}
	if chunk.ArgsDelta != nil && state.kind == responsesKindToolCall {
		state.args += *chunk.ArgsDelta
		*out = append(*out, e.frame("response.function_call_arguments.delta", NewObject().
			Set("item_id", NewString(itemID)).
			Set("output_index", NewNumberInt(int64(outputIndex))).
			Set("delta", NewString(*chunk.ArgsDelta))))
		return
	}
	if chunk.ReasoningDelta != nil && state.kind == responsesKindThinking {
		state.reasoning += *chunk.ReasoningDelta
		*out = append(*out, e.frame("response.reasoning_summary_text.delta", NewObject().
			Set("item_id", NewString(itemID)).
			Set("output_index", NewNumberInt(int64(outputIndex))).
			Set("summary_index", NewNumberInt(0)).
			Set("delta", NewString(*chunk.ReasoningDelta))))
	}
}

func (e *responsesStreamEncoder) finish(out *[][]byte) {
	if e.finished {
		return
	}
	e.ensureCreated(out)
	e.closeAll(out)
	reason := StopUnknown
	if e.stopReason != nil {
		reason = *e.stopReason
	}
	status := stopReasonToResponses(reason, e.hasToolCall)
	response := NewObject().
		Set("id", NewString(e.responseID)).
		Set("object", NewString("response")).
		Set("model", NewString(e.ctx.Model)).
		Set("status", NewString(status.Status)).
		Set("output", NewArray(e.outputItems...))
	if status.IncompleteDetails != nil {
		response.Set("incomplete_details", status.IncompleteDetails)
	}
	if !e.usage.IsEmpty() {
		response.Set("usage", usageToResponses(e.usage))
	}
	eventType := "response.completed"
	if status.Status == "incomplete" {
		eventType = "response.incomplete"
	}
	*out = append(*out, e.frame(eventType, NewObject().Set("response", response)))
	e.finished = true
}

func (e *responsesStreamEncoder) Push(chunk Chunk) [][]byte {
	var out [][]byte
	if e.finished {
		return out
	}
	switch chunk.Kind {
	case ChunkStart:
		e.ensureCreated(&out)
	case ChunkBlockStart:
		e.openBlock(&out, chunk)
	case ChunkBlockDelta:
		e.applyDelta(&out, chunk)
	case ChunkBlockStop:
		if chunk.BlockIndex != nil {
			e.closeBlock(&out, *chunk.BlockIndex)
		}
	case ChunkDelta:
		e.usage = mergeUsage(e.usage, chunk.Usage)
		if chunk.StopReason != nil {
			e.stopReason = chunk.StopReason
		}
	case ChunkEnd:
		e.finish(&out)
	default:
		// 未知 kind：忽略
		e.ignoredEvents++
	}
	return out
}

func (e *responsesStreamEncoder) Flush() [][]byte {
	var out [][]byte
	if e.finished {
		return out
	}
	e.finish(&out)
	return out
}
