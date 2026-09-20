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
	seed string
	// pending 是悬置待裁的增量：首个增量与 seed 逐字相同时，「上游重发首片」与「合法续写恰好同字」
	// 在本地是同一串字节，只有声明式全文能消歧，故先攒在这里，等声明到达由 reconcile 交付。
	// 只准攒到 responsesPendingMaxBytes，越界即按兜底口径放行（见 releaseSuspended）。
	pending string
	closed  bool
}

// responsesPendingMaxBytes 是「首片歧义」悬置的硬上限。
//
// 悬置的唯一目的是等声明式全文来消歧，而声明通常紧随该片增量到达，故一个小窗口就够覆盖常见
// 形态。若不给上限，等不到声明的长响应会把整段正文扣在本地：客户端在 done 之前一个字节都收不到
// （可触发 idle timeout），pending 又是字符串累加、随响应长度二次复制放大。越界即按既有兜底口径
// （「宁可重复、不静默丢字」，与 closeBlock 同一取舍）放行，把代价换成有界的延迟与驻留。
const responsesPendingMaxBytes = 1024

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

	byKey      map[string]*responsesDecodedBlock
	openBlocks []*responsesDecodedBlock
}

func newResponsesStreamDecoder(ctx ConvertCtx) StreamDecoder {
	return &responsesStreamDecoder{
		ctx:   ctx,
		byKey: map[string]*responsesDecodedBlock{},
	}
}

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
	*out = append(*out, Chunk{Kind: ChunkBlockStart, BlockIndex: intPtr(blockIndex), Block: block})
	return decoded
}

func (d *responsesStreamDecoder) closeBlock(out *[]Chunk, decoded *responsesDecodedBlock) {
	if decoded.closed {
		return
	}
	// 上游把整段正文放在 content_part.added 的 part.text 里、之后不发任何增量时，seed 是唯一
	// 的内容来源：关闭前补发，否则整段正文随块一起消失。只在此块从未发出任何内容时补，故不重复
	// （增量路径已由 flushSeed 兜住，走到这里的只剩“一个增量都没发”）。
	if decoded.emitted == "" && decoded.seed != "" {
		d.emitDeltaChunk(out, decoded, decoded.seed)
		decoded.seed = ""
	}
	// 悬置的增量还没等到任何声明就要关块（上游连 done / item / 终态都没给）：此时无从消歧，
	// 按「宁可重复、不静默丢字」交付。声明可用时 reconcile 早已清空它，走不到这里。
	if decoded.pending != "" {
		d.emitDeltaChunk(out, decoded, decoded.pending)
		decoded.pending = ""
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

// flushSeed 在块内首次交付增量之前，先把 content_part.added 读到的初始文本交付出去。
//
// 为什么不能只靠 closeBlock 的兜底：部分上游先给 part.text、再给续写增量（part.text 是最终正文的
// 前缀）。那时 emitted 已非空，兜底不再触发，初始文本就随块消失；而 done / item 的对账也补不回来
// ——emitted 不含这段前缀，不构成声明全文的前缀，按「宁可不补」的策略算不出差额。
// 单点在这里发出去后，「seed 非空 ⇒ emitted 为空」在本块上始终成立（悬置的增量同样尚未交付），
// 所有对账路径看到的 emitted 都已经是声明全文的前缀。
func (d *responsesStreamDecoder) flushSeed(out *[]Chunk, decoded *responsesDecodedBlock) {
	if decoded.seed == "" {
		return
	}
	seed := decoded.seed
	decoded.seed = ""
	d.emitDeltaChunk(out, decoded, seed)
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
	d.recoverTerminalOutput(out, response)
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
	// 首个增量与 part.text 逐字相同：本地无从判定上游是在重发首片，还是续写恰好同字（两种解释
	// 产生同一串字节），而交付顺序不可回退，故把这段悬置到声明式全文到达再交付——声明是权威，
	// 既不会重复交付（重发型），也不会吞掉合法续写的首字（合法续写型）。见 reconcile。
	//
	// 悬置只到 responsesPendingMaxBytes 为止：等不到声明就无限攒，等于把整段正文扣在本地。
	if decoded.emitted == "" && decoded.seed != "" {
		switch {
		case decoded.pending == "" && delta == decoded.seed && len(delta) <= responsesPendingMaxBytes:
			decoded.pending = delta
			return
		case decoded.pending != "" && len(decoded.pending)+len(delta) <= responsesPendingMaxBytes:
			decoded.pending += delta
			return
		case decoded.pending != "":
			d.releaseSuspended(out, decoded, delta)
			return
		}
	}
	d.flushSeed(out, decoded)
	d.emitDeltaChunk(out, decoded, delta)
}

// releaseSuspended 在悬置到达上限时放弃等声明，按既有兜底口径交付，并把这一块交回普通流式
// 路径：此后 emitted 非空，不会再进入悬置分支。（「首个增量本身超过上限」不经过此处——它根本
// 放不进窗口，走的是普通路径的 flushSeed + emitDeltaChunk。）
//
// 取舍与 closeBlock 的兜底完全一致——两种来源各交付一次（重发型的首片在这里多出一份），也不肯
// 继续攒着等一个可能永远不来的声明。声明若随后到达，reconcile 仍会尝试对账，只是此时 emitted 已
// 长于声明全文、不构成前缀，算不出差额故不再改动（绝不重复地回退已交付内容）。
func (d *responsesStreamDecoder) releaseSuspended(out *[]Chunk, decoded *responsesDecodedBlock, delta string) {
	d.flushSeed(out, decoded)
	d.emitDeltaChunk(out, decoded, decoded.pending)
	decoded.pending = ""
	d.emitDeltaChunk(out, decoded, delta)
}

// reconcile 用声明式全文对账一个块尚未交付的内容。
//
// 悬置期内块内一个字节都没交付过（进入悬置的前置条件就是 emitted 为空），所以声明到达时该块的全部
// 内容就是声明全文本身：重发的首片自然被吸收，合法续写的两段则一并交付。没有声明就什么都不做——
// 悬置内容留给 closeBlock 的兜底，绝不凭猜测交付。
func (d *responsesStreamDecoder) reconcile(out *[]Chunk, decoded *responsesDecodedBlock, declared string) {
	if len(declared) == 0 {
		return
	}
	decoded.pending = ""
	if tail := declaredTextTail(decoded.emitted, declared); len(tail) > 0 {
		d.emitDeltaChunk(out, decoded, tail)
	}
}

func (d *responsesStreamDecoder) handleDone(out *[]Chunk, eventType string, payload *Value) {
	kind := responsesDoneKinds[eventType]
	d.ensureStart(out)
	decoded := d.findBlock(payload, kind)
	full, _ := stringField(payload, "text")
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
	declaredMissing := full == ""
	if declaredMissing {
		full = decoded.seed
	}
	// 这里的 full 如果是拿 seed 顶替出来的，就不是上游的声明，不能凭它裁决悬置的增量：
	// 多片悬置时（如累计型上游）seed 只是首段，据此交付会把后面的内容静默吃掉。交给 closeBlock
	// 的「宁可重复、不静默丢字」兜底。
	if declaredMissing && decoded.pending != "" {
		d.closeBlock(out, decoded)
		return
	}
	// done 载荷是声明式全文：先裁决悬置的增量，再只补已发内容之后的差额，绝不重发已发部分。
	d.reconcile(out, decoded, full)
	d.closeBlock(out, decoded)
}

// handleOutputItemDone 处理 item 收尾：先按 item 载荷对账未交付的内容，再按 output_index 关块。
//
// 为什么两者都要：上游可能一个增量都不发、只在 item 载荷里给全文（此时靠对账补出正文）；也可能
// 增量齐全、item 载荷只是确认（此时对账算不出差额，不会重复）。
func (d *responsesStreamDecoder) handleOutputItemDone(out *[]Chunk, payload *Value) {
	outputIndex := intOrDefault(payload, "output_index", 0)
	item := payload.ObjectField("item")
	if item == nil {
		d.closeByOutputIndex(out, outputIndex)
		return
	}
	d.recoverItemContent(out, payload, item, outputIndex)
	d.closeByOutputIndex(out, outputIndex)
}

// recoverItemContent 按 item 载荷补出该 item 尚未交付的正文 / 推理 / 工具参数。
//
// 共享同一套规则：块不存在就补建（上游可能跳过 added），存在就只补差额（已交付部分绝不重发）。
func (d *responsesStreamDecoder) recoverItemContent(out *[]Chunk, payload *Value, item *Value, outputIndex int) {
	itemID, _ := stringField(item, "id")
	if itemID == "" {
		if fallback, ok := stringField(payload, "item_id"); ok && fallback != "" {
			itemID = fallback
		} else {
			itemID = strconv.Itoa(outputIndex)
		}
	}
	itemType, _ := stringField(item, "type")
	switch itemType {
	case "function_call":
		callID, _ := stringField(item, "call_id")
		if callID == "" {
			callID = itemID
		}
		name, _ := stringField(item, "name")
		d.ensureStart(out)
		decoded := d.startBlock(out, "item:"+itemID, responsesKindToolCall,
			&Block{Kind: BlockToolCall, ID: callID, Name: d.ctx.fromWireName(name)}, outputIndex)
		if args, ok := stringField(item, "arguments"); ok && len(args) > 0 {
			d.reconcile(out, decoded, args)
		}
	case "reasoning":
		text := responsesReasoningTextOf(item)
		if len(text) == 0 {
			return
		}
		d.ensureStart(out)
		decoded := d.startBlock(out, "item:"+itemID, responsesKindThinking,
			&Block{Kind: BlockThinking}, outputIndex)
		d.reconcile(out, decoded, text)
	case "message":
		for index, part := range item.ArrayField("content") {
			text, ok := stringField(part, "text")
			if !ok || len(text) == 0 {
				continue
			}
			d.ensureStart(out)
			decoded := d.startBlock(out,
				"part:"+strconv.Itoa(outputIndex)+":"+strconv.Itoa(index),
				responsesKindText, &Block{Kind: BlockText}, outputIndex)
			d.reconcile(out, decoded, text)
		}
	}
}

// recoverTerminalOutput 从终态载荷的 output[] 里捡回「一个增量都没发」的内容。
//
// 只发 response.created + response.completed 的上游（全文在 output[] 里）在修此路径前会让客户端
// 拿到空正文；这里按 output[] 下标当 output_index 复用 item 级对账逻辑。已由增量交付的内容算不出
// 差额，故不会重复；尚未建块的 item 则连同块一起补出。
func (d *responsesStreamDecoder) recoverTerminalOutput(out *[]Chunk, response *Value) {
	if response == nil {
		return
	}
	for index, item := range response.ArrayField("output") {
		if item == nil || !item.IsObject() {
			continue
		}
		d.handleOutputItemDone(out, NewObject().
			Set("output_index", NewNumberInt(int64(index))).
			Set("item", item))
	}
}

func (d *responsesStreamDecoder) handleFrame(frame SSEFrame) []Chunk {
	var out []Chunk
	payload := parseFrameJSON(frame)
	if payload == nil {
		// 畸形 / 心跳 / 无法解析的帧：忽略，不抛错
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
			// 分片收尾帧同样携带该分片的声明式全文：首片悬置未决时用它消歧，免得紧接着的 closeBlock
			// 兜底把重发的首片一并交付出去。
			if part.pending != "" {
				text, _ := stringField(payload.ObjectField("part"), "text")
				d.reconcile(&out, part, text)
			}
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
}

func newResponsesStreamEncoder(ctx ConvertCtx) StreamEncoder {
	return &responsesStreamEncoder{
		ctx:        ctx,
		responseID: MakeSyntheticResponseID(ProtocolOpenAIResponses),
		states:     map[int]*responsesEncoderBlockState{},
	}
}

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
		// 块起始就带正文的上游（Anthropic 的 content_block_start 允许带 text）：本线没有
		// 「起始帧带正文」的位置，必须转成增量帧，否则整段正文随块一起消失。
		if len(block.Text) > 0 {
			state.text = block.Text
			*out = append(*out, e.frame("response.output_text.delta", NewObject().
				Set("item_id", NewString(itemID)).
				Set("output_index", NewNumberInt(int64(outputIndex))).
				Set("content_index", NewNumberInt(0)).
				Set("delta", NewString(block.Text))))
		}
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
