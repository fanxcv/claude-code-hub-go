package convert

import "strings"

const anthropicWire = ProtocolAnthropicMessages

func init() { registerCodec(anthropicWire) }

var anthropicKnownRequestKeys = knownKeys{
	"model": true, "messages": true, "system": true, "tools": true, "tool_choice": true,
	"max_tokens": true, "stream": true, "temperature": true, "top_p": true, "top_k": true,
	"stop_sequences": true, "thinking": true, "output_config": true, "metadata": true,
}

var anthropicKnownResponseKeys = knownKeys{
	"id": true, "type": true, "role": true, "model": true, "content": true,
	"stop_reason": true, "stop_sequence": true, "usage": true,
}

const anthropicDefaultMaxTokens = 4096

// ---------------------------------------------------------------------------
// 解码：Anthropic -> 枢纽
// ---------------------------------------------------------------------------

func decodeAnthropicCacheHint(source *Value) *CacheHint {
	raw := fieldOrNil(source, "cache_control")
	if !isRecord(raw) {
		return nil
	}
	ttl, hasTTL := stringField(raw, "ttl")
	return &CacheHint{TTL: ttl, HasTTL: hasTTL && ttl != ""}
}

func decodeAnthropicBlocks(raw *Value, loss *LossCollector, direction string, fromWire func(string) string) []Block {
	if raw == nil || raw.IsNull() {
		return nil
	}
	if raw.Kind() == "string" {
		text, _ := raw.String()
		return []Block{textBlock(text)}
	}
	if !raw.IsArray() {
		loss.Rewritten(LossUnknownField, direction, "content."+raw.Kind())
		return []Block{opaqueBlock(anthropicWire, raw)}
	}
	blocks := []Block{}
	for _, part := range raw.Items() {
		if block, ok := decodeAnthropicBlock(part, loss, direction, fromWire); ok {
			blocks = append(blocks, block)
		}
	}
	return blocks
}

func decodeAnthropicBlock(part *Value, loss *LossCollector, direction string, fromWire func(string) string) (Block, bool) {
	if part == nil || !part.IsObject() {
		loss.Rewritten(LossUnknownField, direction, "content_part")
		return opaqueBlock(anthropicWire, part), true
	}
	partType, _ := stringField(part, "type")
	switch partType {
	case "text":
		text, _ := stringField(part, "text")
		block := textBlock(text)
		block.CacheHint = decodeAnthropicCacheHint(part)
		return block, true
	case "image":
		return decodeAnthropicMedia(part, BlockImage, loss, direction)
	case "document":
		return decodeAnthropicMedia(part, BlockDocument, loss, direction)
	case "tool_use":
		id, _ := stringField(part, "id")
		name, _ := stringField(part, "name")
		if fromWire != nil {
			name = fromWire(name)
		}
		args := "{}"
		if input := fieldOrNil(part, "input"); input != nil {
			args = input.MarshalCompact()
		}
		return Block{Kind: BlockToolCall, ID: id, Name: name, Args: args}, true
	case "thinking":
		text, _ := stringField(part, "thinking")
		signature, _ := stringField(part, "signature")
		return Block{Kind: BlockThinking, Text: text, Signature: signature}, true
	case "redacted_thinking":
		data, _ := stringField(part, "data")
		for _, extra := range meaningfulExtras(part, "type", "data") {
			loss.Dropped(LossUnknownField, direction, "content_part."+extra)
		}
		return Block{Kind: BlockThinking, Text: data, Redacted: true}, true
	default:
		return opaqueBlock(anthropicWire, part), true
	}
}

func decodeAnthropicMedia(part *Value, kind BlockKind, loss *LossCollector, direction string) (Block, bool) {
	source := fieldOrNil(part, "source")
	if !isRecord(source) {
		loss.Rewritten(LossUnknownField, direction, string(kind)+".source")
		return opaqueBlock(anthropicWire, part), true
	}
	block := Block{Kind: kind, CacheHint: decodeAnthropicCacheHint(part)}
	sourceType, _ := stringField(source, "type")
	mediaType, _ := stringField(source, "media_type")
	switch sourceType {
	case "base64":
		data, _ := stringField(source, "data")
		block.MediaType = mediaType
		block.Data = data
		return block, true
	case "url":
		url, _ := stringField(source, "url")
		block.URL = url
		return block, true
	default:
		loss.Rewritten(LossUnknownField, direction, string(kind)+".source.type")
		return opaqueBlock(anthropicWire, part), true
	}
}

// decodeAnthropicToolResult 把 tool_result 块解为独立 item；is_error 无法承载时声明损失。
func decodeAnthropicToolResult(part *Value, loss *LossCollector, direction string) Item {
	toolUseID, _ := stringField(part, "tool_use_id")
	isError, _ := boolField(part, "is_error")
	return Item{
		Kind:       ItemToolResult,
		ToolCallID: toolUseID,
		Blocks:     decodeAnthropicBlocks(fieldOrNil(part, "content"), loss, direction, nil),
		IsError:    isError,
	}
}

func decodeAnthropicMessages(raw *Value, items *[]Item, loss *LossCollector, fromWire func(string) string) {
	if raw == nil {
		return
	}
	const direction = "request"
	if !raw.IsArray() {
		loss.Dropped(LossUnknownField, direction, "messages")
		return
	}
	for index, message := range raw.Items() {
		if !isRecord(message) {
			loss.Dropped(LossUnknownField, direction, "messages["+itoa(index)+"]")
			continue
		}
		role, _ := stringField(message, "role")
		content, hasContent := message.Get("content")

		pending := []Block{}
		flush := func() {
			if len(pending) == 0 {
				return
			}
			*items = append(*items, Item{Kind: ItemMessage, Role: role, Blocks: pending})
			pending = nil
		}

		for _, part := range asUnknownArray(content) {
			if isRecord(part) {
				if partType, _ := stringField(part, "type"); partType == "tool_result" {
					flush()
					*items = append(*items, decodeAnthropicToolResult(part, loss, direction))
					continue
				}
			}
			if block, ok := decodeAnthropicBlock(part, loss, direction, fromWire); ok {
				pending = append(pending, block)
			}
		}
		if len(pending) == 0 && hasContent && !content.IsArray() {
			pending = decodeAnthropicBlocks(content, loss, direction, fromWire)
		}
		flush()
	}
}

func decodeAnthropicSystem(raw *Value, loss *LossCollector) []Block {
	if raw == nil || raw.IsNull() {
		return nil
	}
	return decodeAnthropicBlocks(raw, loss, "request", nil)
}

func decodeAnthropicTools(raw *Value, loss *LossCollector) []Tool {
	if raw == nil {
		return nil
	}
	if !raw.IsArray() {
		loss.Dropped(LossUnknownField, "request", "tools")
		return nil
	}
	tools := []Tool{}
	for _, entry := range raw.Items() {
		if !isRecord(entry) {
			loss.Dropped(LossUnknownField, "request", "tools[]")
			continue
		}
		name, hasName := stringField(entry, "name")
		if !hasName || name == "" {
			loss.Dropped(LossUnknownField, "request", "tools[].name")
			continue
		}
		if entryType, ok := stringField(entry, "type"); ok && entryType != "" && entryType != "custom" {
			loss.Dropped(LossUnknownField, "request", "tools[].type")
			continue
		}
		tool := Tool{Name: name, Parameters: NewObject(), CacheHint: decodeAnthropicCacheHint(entry)}
		if description, ok := stringField(entry, "description"); ok {
			tool.Description = description
			tool.HasDescript = true
		}
		if schema := fieldOrNil(entry, "input_schema"); schema != nil && schema.IsObject() {
			tool.Parameters = schema
		}
		tools = append(tools, tool)
	}
	return tools
}

// decodeAnthropicToolChoice 解 tool_choice；同时返回从 `disable_parallel_tool_use` 反推的并行开关。
//
// 为何拆出第二个返回值：Anthropic 用 `tool_choice.disable_parallel_tool_use` 的**取反**表达
// 「是否允许并行」，而该字段可以独立于 toolChoice 存在（`{type:"auto",disable_parallel_tool_use:true}`
// 与只有 disable 的两种形态都真实出现过）。Node 的 `decodeToolChoice` 返回
// `{toolChoice, parallelToolCalls}` 两个值，此处复刻同一形状。
func decodeAnthropicToolChoice(raw *Value, loss *LossCollector) (*ToolChoice, *bool) {
	if !isRecord(raw) {
		if raw != nil && !raw.IsNull() {
			loss.Dropped(LossUnknownField, "request", "tool_choice")
		}
		return nil, nil
	}
	parallel := anthropicParallelFromChoice(raw, loss)
	choiceType, _ := stringField(raw, "type")
	switch choiceType {
	case "auto":
		return &ToolChoice{Kind: ChoiceAuto}, parallel
	case "any":
		return &ToolChoice{Kind: ChoiceRequired}, parallel
	case "none":
		return &ToolChoice{Kind: ChoiceNone}, parallel
	case "tool":
		name, _ := stringField(raw, "name")
		if name == "" {
			loss.Dropped(LossUnknownField, "request", "tool_choice.name")
			return nil, parallel
		}
		return &ToolChoice{Kind: ChoiceTool, Name: name}, parallel
	default:
		loss.Dropped(LossUnknownField, "request", "tool_choice.type")
		return nil, parallel
	}
}

// anthropicParallelFromChoice 复刻 Node 的取反：`parallelToolCalls = !disable_parallel_tool_use`。
// 非布尔值记损且视为未给（与 Node `LOSS.UNKNOWN_FIELD` 一致）。
func anthropicParallelFromChoice(choice *Value, loss *LossCollector) *bool {
	disable, present := choice.Get("disable_parallel_tool_use")
	if !present || disable.IsNull() {
		return nil
	}
	value, ok := disable.Bool()
	if !ok {
		// 非布尔：记损且视为未给（与 Node 的 `LOSS.UNKNOWN_FIELD` + 不承载同口径）。
		loss.Dropped(LossUnknownField, "request", "tool_choice.disable_parallel_tool_use 非布尔")
		return nil
	}
	parallel := !value
	return &parallel
}

func decodeAnthropicSampling(body *Value, loss *LossCollector) Sampling {
	sampling := Sampling{}
	if maxTokens, ok := numberField(body, "max_tokens"); ok {
		sampling.MaxTokens = maxTokens
	} else {
		defaulted := float64(anthropicDefaultMaxTokens)
		sampling.MaxTokens = &defaulted
		loss.Downgraded(LossMaxTokensDefaulted, "request", "anthropic.max_tokens")
	}
	if temperature, ok := numberField(body, "temperature"); ok {
		sampling.Temperature = temperature
	}
	if topP, ok := numberField(body, "top_p"); ok {
		sampling.TopP = topP
	}
	if topK, ok := numberField(body, "top_k"); ok {
		sampling.TopK = topK
	}
	stop, present := body.Get("stop_sequences")
	switch {
	case present && stop.Kind() == "string":
		text, _ := stop.String()
		sampling.Stop = []string{text}
	case present && stop.IsArray():
		stops := []string{}
		for _, entry := range stop.Items() {
			if entry.Kind() == "string" {
				text, _ := entry.String()
				stops = append(stops, text)
				continue
			}
			loss.Dropped(LossStopSequences, "request", "stop_sequences[]")
		}
		if len(stops) > 0 {
			sampling.Stop = stops
		}
	case present && !stop.IsNull():
		loss.Dropped(LossStopSequences, "request", "stop_sequences")
	}
	return sampling
}

func decodeAnthropicReasoning(body *Value, loss *LossCollector) *Reasoning {
	reasoning := &Reasoning{}
	found := false

	if thinking := fieldOrNil(body, "thinking"); isRecord(thinking) {
		thinkingType, _ := stringField(thinking, "type")
		switch thinkingType {
		case "enabled":
			if budget, ok := numberField(thinking, "budget_tokens"); ok {
				reasoning.BudgetTokens = budget
				found = true
			}
		case "disabled":
			reasoning.Effort = "none"
			reasoning.HasEffort = true
			found = true
		default:
			loss.Dropped(LossUnknownField, "request", "thinking.type")
		}
	}
	if outputConfig := fieldOrNil(body, "output_config"); isRecord(outputConfig) {
		if effort, ok := stringField(outputConfig, "effort"); ok && effort != "" {
			reasoning.Effort = effort
			reasoning.HasEffort = true
			found = true
		} else if effort, present := outputConfig.Get("effort"); present && !effort.IsNull() {
			loss.Dropped(LossUnknownField, "request", "output_config.effort")
		}
	}
	if !found {
		return nil
	}
	return reasoning
}

func decodeAnthropicRequest(body *Value, ctx ConvertCtx) DecodeResult[*Request] {
	loss := &LossCollector{}
	source := body
	if !isRecord(source) {
		source = NewObject()
	}

	system := decodeAnthropicSystem(fieldOrNil(source, "system"), loss)
	items := []Item{}
	decodeAnthropicMessages(fieldOrNil(source, "messages"), &items, loss, ctx.FromWireToolName)

	model, _ := stringField(source, "model")
	stream, _ := boolField(source, "stream")

	request := &Request{
		Model:       firstString(model, ctx.Model),
		Items:       items,
		Sampling:    decodeAnthropicSampling(source, loss),
		Stream:      stream,
		Passthrough: map[WireProtocol]*Value{},
	}
	if len(system) > 0 {
		request.System = system
	}
	if tools := decodeAnthropicTools(fieldOrNil(source, "tools"), loss); len(tools) > 0 {
		request.Tools = tools
	}
	if choice, parallel := decodeAnthropicToolChoice(fieldOrNil(source, "tool_choice"), loss); choice != nil || parallel != nil {
		request.ToolChoice = choice
		request.ParallelToolCalls = parallel
	}
	if reasoning := decodeAnthropicReasoning(source, loss); reasoning != nil {
		request.Reasoning = reasoning
	}
	if passthrough := collectPassthrough(source, anthropicKnownRequestKeys); len(passthrough.Members()) > 0 {
		request.Passthrough[anthropicWire] = passthrough
	}
	return DecodeResult[*Request]{Value: request, Loss: loss.Report()}
}

// ---------------------------------------------------------------------------
// 编码：枢纽 -> Anthropic
// ---------------------------------------------------------------------------

func encodeAnthropicCacheHint(block *Value, hint *CacheHint) {
	if hint == nil {
		return
	}
	cacheControl := NewObject().Set("type", NewString("ephemeral"))
	if hint.HasTTL {
		cacheControl.Set("ttl", NewString(hint.TTL))
	}
	block.Set("cache_control", cacheControl)
}

type anthropicRenderOptions struct {
	direction string
	seed      string
	idMap     map[string]string
	loss      *LossCollector
	toWire    func(string) string
	// placeholderThinkingSignature 允许给无签名的思考块补占位签名（取值见
	// ConvertCtx.shouldPlaceholderThinkingSignature）。
	placeholderThinkingSignature bool
}

func anthropicResolveEmitID(original string, seed string, options *anthropicRenderOptions) string {
	raw := original
	if raw != "" {
		if mapped, ok := options.idMap[raw]; ok {
			return mapped
		}
	}
	emitted := raw
	if raw == "" {
		emitted = MakeToolCallID(seed)
		options.loss.Rewritten(LossToolCallIDRewritten, options.direction, "synthesized:"+seed)
	} else if !isSafeToolName(raw) {
		emitted = NormalizeToolCallID(raw)
		options.loss.Rewritten(LossToolCallIDRewritten, options.direction, "sanitized")
	}
	if raw != "" {
		options.idMap[raw] = emitted
	}
	return emitted
}

func encodeAnthropicBlock(block Block, seed string, options *anthropicRenderOptions) (*Value, bool) {
	switch block.Kind {
	case BlockText:
		out := NewObject().Set("type", NewString("text")).Set("text", NewString(block.Text))
		encodeAnthropicCacheHint(out, block.CacheHint)
		return out, true
	case BlockImage:
		if mediaOut, ok := encodeAnthropicMedia(block, "image"); ok {
			return mediaOut, true
		}
		options.loss.Dropped(LossImage, options.direction, "no_data_or_url")
		return nil, false
	case BlockDocument:
		if mediaOut, ok := encodeAnthropicMedia(block, "document"); ok {
			return mediaOut, true
		}
		options.loss.Dropped(LossDocument, options.direction, "no_data_or_url")
		return nil, false
	case BlockToolCall:
		name := block.Name
		if options.toWire != nil {
			name = options.toWire(name)
		}
		input := NewObject()
		if block.Args != "" {
			if parsed, err := ParseJSON([]byte(block.Args)); err == nil && parsed.IsObject() {
				input = parsed
			} else {
				options.loss.Rewritten(LossUnknownField, options.direction, "tool_call.input")
			}
		}
		return NewObject().
			Set("type", NewString("tool_use")).
			Set("id", NewString(anthropicResolveEmitID(block.ID, seed, options))).
			Set("name", NewString(name)).
			Set("input", input), true
	case BlockThinking:
		if block.Redacted {
			options.loss.Dropped(LossThinkingBlock, options.direction, "redacted")
			return nil, false
		}
		signature := block.Signature
		if signature == "" {
			if !options.placeholderThinkingSignature {
				options.loss.Dropped(LossThinkingSignature, options.direction, "missing_signature")
				return nil, false
			}
			// 上游（chat/responses 线）没给签名：补占位让客户端显示思考。记 Rewritten 而非
			// Dropped——块留下了，但签名是造的，报表里必须能看出来。
			signature = PlaceholderThinkingSignature()
			options.loss.Rewritten(LossThinkingSignature, options.direction, "placeholder_signature_injected")
		}
		return NewObject().
			Set("type", NewString("thinking")).
			Set("thinking", NewString(block.Text)).
			Set("signature", NewString(signature)), true
	case BlockOpaque:
		if block.Wire != anthropicWire {
			// 外线 opaque（如 responses 的 mcp_call / web_search_call 项）：按专用类别归因，
			// 而不是一律 unknown_field（后者让 MCP 丢失在报告里不可见）。
			class, detail := nonFunctionToolLossClass(block.Wire, block.Value)
			options.loss.Dropped(class, options.direction, detail)
			return nil, false
		}
		return block.Value, true
	default:
		options.loss.Dropped(LossUnknownField, options.direction, "block")
		return nil, false
	}
}

func encodeAnthropicMedia(block Block, blockType string) (*Value, bool) {
	source := NewObject()
	switch {
	case block.Data != "":
		mediaType := block.MediaType
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		source.Set("type", NewString("base64")).
			Set("media_type", NewString(mediaType)).
			Set("data", NewString(block.Data))
	case block.URL != "":
		source.Set("type", NewString("url")).Set("url", NewString(block.URL))
	default:
		return nil, false
	}
	out := NewObject().Set("type", NewString(blockType)).Set("source", source)
	encodeAnthropicCacheHint(out, block.CacheHint)
	return out, true
}

func encodeAnthropicToolResult(item Item, options *anthropicRenderOptions) *Value {
	content := []*Value{}
	for _, block := range item.Blocks {
		if block.Kind == BlockText {
			content = append(content, NewObject().
				Set("type", NewString("text")).
				Set("text", NewString(block.Text)))
			continue
		}
		if encoded, ok := encodeAnthropicBlock(block, options.seed, options); ok {
			content = append(content, encoded)
		}
	}
	out := NewObject().
		Set("type", NewString("tool_result")).
		Set("tool_use_id", NewString(anthropicResolveEmitID(item.ToolCallID, options.seed, options))).
		Set("content", NewArray(content...))
	if item.IsError {
		out.Set("is_error", NewBool(true))
	}
	return out
}

// encodeAnthropicMessage 渲染一条 role 消息；无可见内容时返回 nil。
func encodeAnthropicMessage(role string, blocks []Block, options *anthropicRenderOptions) *Value {
	content := []*Value{}
	blockIndex := 0
	for _, block := range blocks {
		seed := options.seed + ":" + itoa(blockIndex)
		blockIndex++
		if encoded, ok := encodeAnthropicBlock(block, seed, options); ok {
			content = append(content, encoded)
		}
	}
	if len(content) == 0 {
		return nil
	}
	return NewObject().
		Set("role", NewString(role)).
		Set("content", NewArray(content...))
}

// mergeRenderableItems 合并同一上游还原文的多个 tool_result（Anthropic 需要；Chat / Responses 需要各自独立条目）。
//
// **不**合并相邻的同 role 消息：Node 的 chat / anthropic 编码器都是「一个 item 一条消息」
// （`openai-chat/request.ts` 的 encodeItems 逐项 push；测试语料 `request.openai-responses.string-content.*`
// 钉着「两条相邻 user 不得并成一条」）。合并会改变正文的消息边界与内容拼接结果
// （实测把 `"a"`+`"b"` 变成一条 `"ab"`），并连带改变上游前缀缓存的分段。
func mergeRenderableItems(items []Item, mergeToolResults bool) []Item {
	out := make([]Item, 0, len(items))
	for _, item := range items {
		if n := len(out); n > 0 {
			last := &out[n-1]
			if mergeToolResults && last.Kind == ItemToolResult && item.Kind == ItemToolResult {
				last.Blocks = append(last.Blocks, item.Blocks...)
				if item.IsError {
					last.IsError = true
				}
				continue
			}
		}
		out = append(out, item)
	}
	return out
}

func encodeAnthropicMessages(items []Item, idMap map[string]string, loss *LossCollector, ctx ConvertCtx) []*Value {
	messages := []*Value{}
	for index, item := range mergeRenderableItems(items, true) {
		options := &anthropicRenderOptions{
			direction: "request",
			seed:      itoa(index),
			idMap:     idMap,
			loss:      loss,
			toWire:    ctx.ToWireToolName,

			placeholderThinkingSignature: ctx.shouldPlaceholderThinkingSignature(),
		}
		switch item.Kind {
		case ItemMessage, ItemReasoning:
			if rendered := encodeAnthropicMessage(item.Role, item.Blocks, options); rendered != nil {
				messages = append(messages, rendered)
			}
		case ItemToolResult:
			messages = append(messages, NewObject().
				Set("role", NewString("user")).
				Set("content", NewArray(encodeAnthropicToolResult(item, options))))
		}
	}
	return messages
}

func encodeAnthropicToolChoice(choice *ToolChoice, ctx ConvertCtx) *Value {
	switch choice.Kind {
	case ChoiceAuto:
		return NewObject().Set("type", NewString("auto"))
	case ChoiceNone:
		return NewObject().Set("type", NewString("none"))
	case ChoiceRequired:
		return NewObject().Set("type", NewString("any"))
	default:
		name := choice.Name
		if ctx.ToWireToolName != nil {
			name = ctx.ToWireToolName(name)
		}
		return NewObject().Set("type", NewString("tool")).Set("name", NewString(name))
	}
}

// encodeAnthropicToolChoiceWithParallel 复刻 Node 的 `encodeToolChoice(choice, parallelToolCalls, ctx)`。
//
// Anthropic 用 `tool_choice.disable_parallel_tool_use` 的**取反**表达「是否允许并行」
// （Hub 契约 §2.1：三线皆可无损承载 parallelToolCalls，故提升为枢纽字段）。
// 三条支路与 Node 逐字对应：
//  1. 未给开关 → 只输出 choice（不凭空造字段）；
//  2. 给了但 choice 缺失 → parallel==false 时合成 `{type:"auto",disable_parallel_tool_use:true}`；
//     parallel==true 时**不输出**（默认就是允许并行，为它造对象会与 Node 分叉）；
//  3. 给了且 choice 存在 → 在 choice 上显式写 `disable_parallel_tool_use = !parallel`。
func encodeAnthropicToolChoiceWithParallel(choice *ToolChoice, parallel *bool, ctx ConvertCtx) *Value {
	var encoded *Value
	if choice != nil {
		encoded = encodeAnthropicToolChoice(choice, ctx)
	}
	if parallel == nil {
		return encoded
	}
	if encoded == nil {
		if *parallel {
			return nil
		}
		return NewObject().
			Set("type", NewString("auto")).
			Set("disable_parallel_tool_use", NewBool(true))
	}
	encoded.Set("disable_parallel_tool_use", NewBool(!*parallel))
	return encoded
}

func encodeAnthropicTools(tools []Tool, ctx ConvertCtx) []*Value {
	out := []*Value{}
	for _, tool := range tools {
		name := tool.Name
		if ctx.ToWireToolName != nil {
			name = ctx.ToWireToolName(name)
		}
		schema := tool.Parameters
		if schema == nil {
			schema = NewObject()
		}
		entry := NewObject().
			Set("name", NewString(name)).
			Set("input_schema", schema)
		if tool.HasDescript {
			entry.Set("description", NewString(tool.Description))
		}
		encodeAnthropicCacheHint(entry, tool.CacheHint)
		out = append(out, entry)
	}
	return out
}

func encodeAnthropicRequest(request *Request, ctx ConvertCtx) EncodeResult {
	loss := &LossCollector{}
	const direction = "request"
	idMap := map[string]string{}

	out := cloneOrNewObject(passthroughFor(request, anthropicWire))
	// 外线留存的非 function 工具（如 responses 线的 MCP / web_search 工具声明）本线无法承载时记损。
	reportForeignPreservedTools(request, anthropicWire, loss, direction)
	reportForeignDroppableFields(request, anthropicWire, loss, direction)
	out.Set("model", NewString(firstString(request.Model, ctx.Model)))
	out.Set("messages", NewArray(encodeAnthropicMessages(request.Items, idMap, loss, ctx)...))

	if len(request.System) > 0 {
		system := []*Value{}
		systemOptions := &anthropicRenderOptions{
			direction: direction, seed: "system", idMap: idMap, loss: loss, toWire: ctx.ToWireToolName,
			placeholderThinkingSignature: ctx.shouldPlaceholderThinkingSignature(),
		}
		for index, block := range request.System {
			if encoded, ok := encodeAnthropicBlock(block, "system:"+itoa(index), systemOptions); ok {
				system = append(system, encoded)
			}
		}
		if len(system) > 0 {
			if plainText, ok := joinableSystemText(request.System); ok {
				out.Set("system", NewString(plainText))
			} else {
				out.Set("system", NewArray(system...))
			}
		}
	}

	if len(request.Tools) > 0 {
		out.Set("tools", NewArray(encodeAnthropicTools(request.Tools, ctx)...))
	}
	if choice := encodeAnthropicToolChoiceWithParallel(request.ToolChoice, request.ParallelToolCalls, ctx); choice != nil {
		out.Set("tool_choice", choice)
	}

	maxTokens := float64(anthropicDefaultMaxTokens)
	if request.Sampling.MaxTokens != nil {
		maxTokens = *request.Sampling.MaxTokens
	} else {
		loss.Downgraded(LossMaxTokensDefaulted, direction, "anthropic.max_tokens")
	}
	out.Set("max_tokens", NewNumber(jsNumber(maxTokens)))
	if request.Sampling.Temperature != nil {
		out.Set("temperature", NewNumber(jsNumber(*request.Sampling.Temperature)))
	}
	if request.Sampling.TopP != nil {
		out.Set("top_p", NewNumber(jsNumber(*request.Sampling.TopP)))
	}
	if request.Sampling.TopK != nil {
		out.Set("top_k", NewNumber(jsNumber(*request.Sampling.TopK)))
	}
	if len(request.Sampling.Stop) > 0 {
		stops := make([]*Value, 0, len(request.Sampling.Stop))
		for _, stop := range request.Sampling.Stop {
			stops = append(stops, NewString(stop))
		}
		out.Set("stop_sequences", NewArray(stops...))
	}
	if request.Sampling.Seed != nil {
		loss.Dropped(LossUnknownField, direction, "seed")
	}
	// parallel_tool_calls 不再记损：已由 encodeAnthropicToolChoiceWithParallel 映射到
	// `tool_choice.disable_parallel_tool_use`（与 Node 同口径）。
	if request.Stream || ctx.Stream {
		out.Set("stream", NewBool(true))
	}

	if request.Reasoning != nil {
		if request.Reasoning.BudgetTokens != nil {
			out.Set("thinking", NewObject().
				Set("type", NewString("enabled")).
				Set("budget_tokens", NewNumber(jsNumber(*request.Reasoning.BudgetTokens))))
		} else if request.Reasoning.HasEffort && request.Reasoning.Effort != "" && request.Reasoning.Effort != "none" {
			out.Set("output_config", NewObject().Set("effort", NewString(request.Reasoning.Effort)))
		}
		if request.Reasoning.HasSummary && request.Reasoning.Summary != "" {
			// 与 Node 同类别（Node：`loss.dropped(LOSS.REASONING_REPLAY, …, "reasoning.summary Anthropic 无对应字段")`）；
			// 旧实现记 unknown_field，与 Node 的归因不一致。
			loss.Dropped(LossReasoningReplay, direction, "reasoning.summary")
		}
	}
	return EncodeResult{Body: out, Loss: loss.Report()}
}

// ---------------------------------------------------------------------------
// 响应编解码
// ---------------------------------------------------------------------------

func decodeAnthropicResponse(body *Value, ctx ConvertCtx) DecodeResult[*Response] {
	loss := &LossCollector{}
	const direction = "response"
	source := body
	if !isRecord(source) {
		source = NewObject()
	}

	model, _ := stringField(source, "model")
	id, _ := stringField(source, "id")
	response := &Response{
		ID:          id,
		Model:       firstString(model, ctx.Model),
		Blocks:      []Block{},
		StopReason:  StopUnknown,
		Passthrough: map[WireProtocol]*Value{},
	}

	content, hasContent := source.Get("content")
	switch {
	case hasContent && !content.IsArray():
		if content.Kind() == "string" {
			text, _ := content.String()
			response.Blocks = []Block{textBlock(text)}
		} else if !content.IsNull() {
			loss.Rewritten(LossUnknownField, direction, "content."+content.Kind())
			response.Blocks = []Block{opaqueBlock(anthropicWire, content)}
		}
	default:
		response.Blocks = decodeAnthropicBlocks(content, loss, direction, ctx.FromWireToolName)
	}

	if stopReason, ok := stringField(source, "stop_reason"); ok {
		response.StopReason = StopReasonFromAnthropic(stopReason, true)
	}
	if usageValue, hasUsage := source.Get("usage"); hasUsage && !usageValue.IsNull() {
		if usage := usageFromAnthropic(usageValue); !usage.IsEmpty() {
			response.Usage = usage
		}
	}
	if passthrough := collectPassthrough(source, anthropicKnownResponseKeys); len(passthrough.Members()) > 0 {
		response.Passthrough[anthropicWire] = passthrough
	}
	return DecodeResult[*Response]{Value: response, Loss: loss.Report()}
}

func encodeAnthropicResponse(response *Response, ctx ConvertCtx) EncodeResult {
	loss := &LossCollector{}
	idMap := map[string]string{}

	out := cloneOrNewObject(responsePassthroughFor(response, anthropicWire))
	out.Set("type", NewString("message"))
	out.Set("id", NewString(response.ID))
	out.Set("role", NewString("assistant"))
	out.Set("model", NewString(firstString(response.Model, ctx.Model)))

	content := []*Value{}
	options := &anthropicRenderOptions{
		direction: "response", seed: "0", idMap: idMap, loss: loss,
		placeholderThinkingSignature: ctx.shouldPlaceholderThinkingSignature(),
	}
	for index, block := range response.Blocks {
		if encoded, ok := encodeAnthropicBlock(block, "0:"+itoa(index), options); ok {
			content = append(content, encoded)
		}
	}
	out.Set("content", NewArray(content...))
	out.Set("stop_reason", NewString(stopReasonToAnthropic(response.StopReason)))
	out.Set("stop_sequence", NewNull())
	if response.Usage != nil && !response.Usage.IsEmpty() {
		out.Set("usage", usageToAnthropic(response.Usage))
	}
	return EncodeResult{Body: out, Loss: loss.Report()}
}

// joinableSystemText 报告 system 能否以单个字符串表达：全部为无缓存提示的文本块。
func joinableSystemText(blocks []Block) (string, bool) {
	for _, block := range blocks {
		if block.Kind != BlockText || block.CacheHint != nil {
			return "", false
		}
	}
	return strings.Join(splitTexts(blocks), ""), true
}

func splitTexts(blocks []Block) []string {
	out := []string{}
	for _, block := range blocks {
		if block.Kind == BlockText {
			out = append(out, block.Text)
		}
	}
	return out
}
