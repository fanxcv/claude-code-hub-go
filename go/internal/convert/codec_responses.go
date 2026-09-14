package convert

import "strings"

const responsesWire = ProtocolOpenAIResponses

func init() { registerCodec(responsesWire) }

var responsesKnownRequestKeys = knownKeys{
	"model": true, "instructions": true, "input": true, "tools": true, "tool_choice": true,
	"parallel_tool_calls": true, "max_output_tokens": true, "temperature": true, "top_p": true,
	"stream": true, "reasoning": true,
}

var responsesKnownResponseKeys = knownKeys{
	"id": true, "object": true, "model": true, "status": true, "output": true,
	"usage": true, "incomplete_details": true,
}

var responsesFunctionFields = knownKeys{"type": true, "name": true, "description": true, "parameters": true}

// ---------------------------------------------------------------------------
// 解码：Responses -> 枢纽
// ---------------------------------------------------------------------------

func responsesIsStream(source *Value) bool {
	value, present := source.Get("stream")
	if !present {
		return false
	}
	boolean, ok := value.Bool()
	return ok && boolean
}

// decodeResponsesInstructions 把 instructions 解为系统块。
func decodeResponsesInstructions(raw *Value, loss *LossCollector) []Block {
	if raw == nil || raw.IsNull() {
		return nil
	}
	if raw.Kind() == "string" {
		text, _ := raw.String()
		return []Block{textBlock(text)}
	}
	if !raw.IsArray() {
		loss.Rewritten(LossUnknownField, "request", "instructions."+raw.Kind())
		return []Block{opaqueBlock(responsesWire, raw)}
	}
	blocks := []Block{}
	for _, part := range raw.Items() {
		switch contentPartType(part) {
		case "input_text", "output_text", "text":
			text, _ := stringField(part, "text")
			blocks = append(blocks, textBlock(text))
		default:
			blocks = append(blocks, decodeResponsesPart(part, loss, "request")...)
		}
	}
	return blocks
}

func contentPartType(part *Value) string {
	if !isRecord(part) {
		return ""
	}
	partType, _ := stringField(part, "type")
	return partType
}

// decodeResponsesPart 解 Responses 内容分片；message 分片返回空（由调用方按 role 处理）。
func decodeResponsesPart(part *Value, loss *LossCollector, direction string) []Block {
	switch contentPartType(part) {
	case "input_text", "output_text", "text":
		text, _ := stringField(part, "text")
		return []Block{textBlock(text)}
	case "input_image":
		return decodeResponsesImagePart(part, loss, direction)
	case "refusal":
		loss.Dropped(LossUnknownField, direction, "refusal")
		return nil
	default:
		return []Block{opaqueBlock(responsesWire, part)}
	}
}

func decodeResponsesImagePart(part *Value, loss *LossCollector, direction string) []Block {
	url, _ := stringField(part, "image_url")
	if url == "" {
		loss.Dropped(LossUnknownField, direction, "input_image.image_url")
		return nil
	}
	if strings.HasPrefix(url, "data:") {
		parsed, ok := parseChatDataURL(url)
		if !ok || !strings.HasPrefix(parsed.MediaType, "image/") {
			loss.Rewritten(LossImage, direction, "data_url")
			return []Block{opaqueBlock(responsesWire, part)}
		}
		loss.Rewritten(LossImage, direction, "data_url_to_base64")
		return []Block{{Kind: BlockImage, MediaType: parsed.MediaType, Data: parsed.Data}}
	}
	return []Block{{Kind: BlockImage, URL: url}}
}

func decodeResponsesMessageItem(item *Value, loss *LossCollector) []Item {
	const direction = "request"
	role, _ := stringField(item, "role")
	blocks := []Block{}
	// content 为字符串时是「整段文本」简写（Node: `typeof content === "string" → [textBlock(content)]`）。
	// 用 asUnknownArray 解字符串会得到空切片，整条消息的文本就此丢失（正文只剩空串），
	// 而上游会把它当成「空消息」——系统提示与用户角色消息都受影响。
	if text, isString := stringField(item, "content"); isString {
		blocks = append(blocks, textBlock(text))
	} else {
		for _, part := range asUnknownArray(fieldOrNil(item, "content")) {
			blocks = append(blocks, decodeResponsesPart(part, loss, direction)...)
		}
	}
	switch role {
	case "user", "assistant":
		return []Item{{Kind: ItemMessage, Role: role, Blocks: blocks}}
	case "system", "developer":
		return []Item{{Kind: ItemMessage, Role: "user", Blocks: blocks}}
	default:
		loss.Rewritten(LossUnknownField, direction, "message.role")
		return []Item{{Kind: ItemMessage, Role: "user", Blocks: blocks}}
	}
}

func decodeResponsesFunctionCall(item *Value, loss *LossCollector, fromWire func(string) string) []Item {
	name, _ := stringField(item, "name")
	if fromWire != nil {
		name = fromWire(name)
	}
	callID, _ := stringField(item, "call_id")
	if callID == "" {
		callID, _ = stringField(item, "id")
	}
	arguments, present := stringField(item, "arguments")
	if !present {
		arguments = "{}"
		loss.Rewritten(LossUnknownField, "request", "function_call.arguments")
	}
	if callID == "" {
		loss.Rewritten(LossToolCallIDRewritten, "request", "function_call.call_id")
	}
	return []Item{{
		Kind:   ItemMessage,
		Role:   "assistant",
		Blocks: []Block{{Kind: BlockToolCall, ID: callID, Name: name, Args: arguments}},
	}}
}

func decodeResponsesFunctionCallOutput(item *Value, loss *LossCollector) []Item {
	const direction = "request"
	callID, _ := stringField(item, "call_id")
	if callID == "" {
		loss.Rewritten(LossToolCallIDRewritten, direction, "function_call_output.call_id")
	}
	output := fieldOrNil(item, "output")
	blocks := []Block{}
	switch {
	case output == nil || output.IsNull():
	case output.Kind() == "string":
		text, _ := output.String()
		blocks = append(blocks, textBlock(text))
	case output.IsArray():
		for _, part := range output.Items() {
			blocks = append(blocks, decodeResponsesPart(part, loss, direction)...)
		}
	default:
		loss.Rewritten(LossUnknownField, direction, "function_call_output.output")
		blocks = append(blocks, textBlock(output.MarshalCompact()))
	}
	return []Item{{Kind: ItemToolResult, ToolCallID: callID, Blocks: blocks}}
}

// decodeResponsesReasoningItem 复刻 Node 的 decodeReasoningBlocks（契约 §2.3 规则 2、§6）。
//
// reasoning 项有三处**互为补充**的思考来源，缺一就不会在 chat 线上产出 `reasoning_content`：
//  1. `summary` 为**字符串**（简写）；
//  2. `summary[]` 里的 `summary_text`；
//  3. `content[]` 里的 `reasoning_text`；
//  4. `encrypted_content`（只搬运不生成 → 落 signature + redacted，本线无承载位时由编码器丢弃）。
//
// 旧实现只认 2，于是 1/3 两种形态整项丢失；而 chat 上游（thinking 模式）要求历史里的 assistant
// 轮次把 reasoning 带回来 —— 丢失即导致上游 400 `reasoning_content must be passed back`
// （生产实测：同渠道 Go 12 例 / Node 时代仅 19 例 ‖ 10620 条链目）。
func decodeResponsesReasoningItem(item *Value, loss *LossCollector) []Item {
	const direction = "request"
	blocks := []Block{}

	summary := fieldOrNil(item, "summary")
	if summary != nil && summary.Kind() == "string" {
		text, _ := summary.String()
		blocks = append(blocks, Block{Kind: BlockThinking, Text: text})
	} else {
		for _, part := range asUnknownArray(summary) {
			if contentPartType(part) == "summary_text" {
				text, _ := stringField(part, "text")
				blocks = append(blocks, Block{Kind: BlockThinking, Text: text})
				continue
			}
			// 未知 summary 分片：原样装箱（Node 的 opaqueBlock，I2）。
			blocks = append(blocks, opaqueBlock(responsesWire, part))
		}
	}

	for _, part := range asUnknownArray(fieldOrNil(item, "content")) {
		if contentPartType(part) == "reasoning_text" {
			text, _ := stringField(part, "text")
			blocks = append(blocks, Block{Kind: BlockThinking, Text: text})
			continue
		}
		blocks = append(blocks, opaqueBlock(responsesWire, part))
	}

	if encrypted, ok := stringField(item, "encrypted_content"); ok && encrypted != "" {
		blocks = append(blocks, Block{Kind: BlockThinking, Redacted: true, Signature: encrypted})
	}

	if len(blocks) == 0 {
		loss.Dropped(LossReasoningReplay, direction, "reasoning.summary.empty")
		return nil
	}
	return []Item{{Kind: ItemReasoning, Role: "assistant", Blocks: blocks}}
}

func decodeResponsesInput(raw *Value, loss *LossCollector, fromWire func(string) string) []Item {
	if raw == nil || raw.IsNull() {
		return nil
	}
	const direction = "request"
	if raw.Kind() == "string" {
		text, _ := raw.String()
		return []Item{{Kind: ItemMessage, Role: "user", Blocks: []Block{textBlock(text)}}}
	}
	if !raw.IsArray() {
		loss.Dropped(LossUnknownField, direction, "input")
		return nil
	}
	items := []Item{}
	for _, entry := range raw.Items() {
		if entry.Kind() == "string" {
			text, _ := entry.String()
			items = append(items, Item{Kind: ItemMessage, Role: "user", Blocks: []Block{textBlock(text)}})
			continue
		}
		if !isRecord(entry) {
			loss.Dropped(LossUnknownField, direction, "input[]")
			continue
		}
		entryType, typed := stringField(entry, "type")
		if !typed {
			// `type` 缺失 / null / 非字符串时按 message 项解（Node 的 `case undefined:` 与 "message" 同分支）。
			//
			// 为何单独判：真实客户端会送 `{"role":"user","content":"..."}` 这种省略 `type` 的简写。
			// 旧实现直接进 default 分支把整项丢掉 → chat 正文只剩 system 一条 → 上游报
			// 「Empty input messages」/「field messages is required」（生产实测：仅此一类就造成
			// 37 行 /v1/responses 请求整批失败，而 Node 同一渠道同期 0 例）。
			items = append(items, decodeResponsesMessageItem(entry, loss)...)
			continue
		}
		switch entryType {
		case "message":
			items = appendResponsesMessageItem(items, entry, loss)
		case "function_call":
			items = appendResponsesFunctionCall(items, entry, loss, fromWire)
		case "function_call_output":
			items = append(items, decodeResponsesFunctionCallOutput(entry, loss)...)
		case "reasoning":
			items = append(items, decodeResponsesReasoningItem(entry, loss)...)
		default:
			// web_search_call / mcp_call / local_shell_call / item_reference 等：枢纽无对应表示，
			// **原样装箱**（Node 的 default 分支：`items.push({kind:"message",role:"user",blocks:[opaqueBlock(WIRE, entry)]})`）。
			//
			// 为何不是丢弃：同线往返（responses → responses）时 opaque 块会被原样回写，字节保留；
			// 旧实现在**解码期**就丢掉，于是同线也丢 MCP 上下文（比跨线更难发现）。
			// 跨线时由编码器按专用类别记损（`mcp.tool` / `web_search.tool` / `tool.non_function`）。
			//
			// 附带效果：只带引用项的请求不再落到「messages 为空」，上游 `Empty input messages` 也随之减少。
			items = append(items, Item{
				Kind:   ItemMessage,
				Role:   "user",
				Blocks: []Block{opaqueBlock(responsesWire, entry)},
			})
		}
	}
	return items
}

// appendResponsesMessageItem 复刻 Node 的 decodeMessageItem：
// 连续 **assistant** 消息归并进前一条 assistant item（契约 §2.3 规则 3）。
//
// 为何在解码侧而不是编码侧做：编码侧的「相邻同 role 合并」会连用户消息一起并（把 `"a"`+`"b"`
// 拼成一条 `"ab"`），而 Node 只在 assistant 这一侧归并（语料
// `request.openai-responses.string-content.*` 与 `…tool-roundtrip.*` 共同钉住两侧语义）。
func appendResponsesMessageItem(items []Item, entry *Value, loss *LossCollector) []Item {
	for _, item := range decodeResponsesMessageItem(entry, loss) {
		if item.Kind == ItemMessage && item.Role == "assistant" && len(items) > 0 {
			last := items[len(items)-1]
			if last.Kind == ItemMessage && last.Role == "assistant" {
				items[len(items)-1].Blocks = append(items[len(items)-1].Blocks, item.Blocks...)
				continue
			}
		}
		items = append(items, item)
	}
	return items
}

// appendResponsesFunctionCall 复刻 Node 的 appendFunctionCall：
// tool_call 并入前一条 assistant 消息（契约 §2.3 规则 3）；前一条不是 assistant 时另起一条。
func appendResponsesFunctionCall(
	items []Item,
	entry *Value,
	loss *LossCollector,
	fromWire func(string) string,
) []Item {
	for _, item := range decodeResponsesFunctionCall(entry, loss, fromWire) {
		if item.Kind == ItemMessage && item.Role == "assistant" && len(items) > 0 {
			last := items[len(items)-1]
			if last.Kind == ItemMessage && last.Role == "assistant" {
				items[len(items)-1].Blocks = append(items[len(items)-1].Blocks, item.Blocks...)
				continue
			}
		}
		items = append(items, item)
	}
	return items
}

func decodeResponsesTools(raw *Value, loss *LossCollector) ([]Tool, []*Value) {
	if raw == nil {
		return nil, nil
	}
	if !raw.IsArray() {
		loss.Dropped(LossUnknownField, "request", "tools")
		return nil, nil
	}
	tools := []Tool{}
	// preserved 是本线无枢纽表示的非 function 工具定义（web_search / mcp / local_shell …）。
	// Node 把它们留在 passthrough（`passthrough.tools = preserved`），编码同线时接在
	// canonical 工具之后（`body.tools = [...tools, ...extraTools]`），从而**同线往返字节保留**。
	// 旧实现直接丢弃 → 同线也丢 MCP 工具声明。
	preserved := []*Value{}
	for _, entry := range raw.Items() {
		if !isRecord(entry) {
			loss.Dropped(LossUnknownField, "request", "tools[]")
			continue
		}
		entryType, _ := stringField(entry, "type")
		name, hasName := stringField(entry, "name")
		if entryType != "function" || !hasName || name == "" {
			preserved = append(preserved, entry)
			continue
		}
		tool := Tool{Name: name, Parameters: NewObject()}
		if description, ok := stringField(entry, "description"); ok {
			tool.Description = description
			tool.HasDescript = true
		}
		if parameters := fieldOrNil(entry, "parameters"); parameters != nil && parameters.IsObject() {
			tool.Parameters = parameters
		}
		for _, member := range entry.Members() {
			if responsesFunctionFields.contains(member.Key) || member.Value.IsNull() {
				continue
			}
			loss.Dropped(LossUnknownField, "request", "tool."+member.Key)
		}
		tools = append(tools, tool)
	}
	return tools, preserved
}

func decodeResponsesToolChoice(raw *Value, loss *LossCollector) *ToolChoice {
	if raw == nil || raw.IsNull() {
		return nil
	}
	if raw.Kind() == "string" {
		text, _ := raw.String()
		switch text {
		case "auto", "none", "required":
			return &ToolChoice{Kind: ToolChoiceKind(text)}
		}
	}
	if isRecord(raw) {
		choiceType, _ := stringField(raw, "type")
		switch choiceType {
		case "function", "custom":
			name, _ := stringField(raw, "name")
			if name != "" {
				return &ToolChoice{Kind: ChoiceTool, Name: name}
			}
		}
	}
	loss.Dropped(LossUnknownField, "request", "tool_choice")
	return nil
}

func decodeResponsesReasoning(raw *Value, loss *LossCollector) *Reasoning {
	if !isRecord(raw) {
		if raw != nil && !raw.IsNull() {
			loss.Dropped(LossUnknownField, "request", "reasoning")
		}
		return nil
	}
	reasoning := &Reasoning{}
	found := false
	if effort, ok := stringField(raw, "effort"); ok && effort != "" {
		reasoning.Effort = effort
		reasoning.HasEffort = true
		found = true
	}
	if summary, ok := stringField(raw, "summary"); ok && summary != "" {
		reasoning.Summary = summary
		reasoning.HasSummary = true
		found = true
	}
	if !found {
		return nil
	}
	return reasoning
}

func decodeResponsesRequest(body *Value, ctx ConvertCtx) DecodeResult[*Request] {
	loss := &LossCollector{}
	source := body
	if !isRecord(source) {
		source = NewObject()
	}

	request := &Request{
		Model:       firstString(stringOrEmpty(source, "model"), ctx.Model),
		Items:       decodeResponsesInput(fieldOrNil(source, "input"), loss, ctx.FromWireToolName),
		Stream:      responsesIsStream(source),
		Passthrough: map[WireProtocol]*Value{},
	}
	if system := decodeResponsesInstructions(fieldOrNil(source, "instructions"), loss); len(system) > 0 {
		request.System = system
	}
	tools, preservedTools := decodeResponsesTools(fieldOrNil(source, "tools"), loss)
	if len(tools) > 0 {
		request.Tools = tools
	}
	if choice := decodeResponsesToolChoice(fieldOrNil(source, "tool_choice"), loss); choice != nil {
		request.ToolChoice = choice
	}
	if each, ok := numberField(source, "max_output_tokens"); ok {
		request.Sampling.MaxTokens = each
	}
	if temperature, ok := numberField(source, "temperature"); ok {
		request.Sampling.Temperature = temperature
	}
	if topP, ok := numberField(source, "top_p"); ok {
		request.Sampling.TopP = topP
	}
	if parallel, ok := boolField(source, "parallel_tool_calls"); ok {
		request.ParallelToolCalls = &parallel
	} else if value, present := source.Get("parallel_tool_calls"); present && !value.IsNull() {
		loss.Dropped(LossUnknownField, "request", "parallel_tool_calls")
	}
	if reasoning := decodeResponsesReasoning(fieldOrNil(source, "reasoning"), loss); reasoning != nil {
		request.Reasoning = reasoning
	}
	passthrough := collectPassthrough(source, responsesKnownRequestKeys)
	if len(preservedTools) > 0 {
		// 本线无枢纽表示的非 function 工具定义：随本线 passthrough 保存（Node：`passthrough.tools = preserved`），
		// 同线编码时接在 canonical 工具之后回写，字节保留。
		passthrough.Set("tools", NewArray(preservedTools...))
	}
	if len(passthrough.Members()) > 0 {
		request.Passthrough[responsesWire] = passthrough
	}
	return DecodeResult[*Request]{Value: request, Loss: loss.Report()}
}

// ---------------------------------------------------------------------------
// 编码：枢纽 -> Responses
// ---------------------------------------------------------------------------

type responsesRenderOptions struct {
	direction string
	seed      string
	idMap     map[string]string
	loss      *LossCollector
	toWire    func(string) string
}

func responsesResolveEmitID(original string, seed string, options *responsesRenderOptions) string {
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
	} else if !chatIsSafeToolID(raw) {
		emitted = NormalizeToolCallID(raw)
		options.loss.Rewritten(LossToolCallIDRewritten, options.direction, "sanitized")
	}
	if raw != "" {
		options.idMap[raw] = emitted
	}
	return emitted
}

func responsesTextPart(role string, text string) *Value {
	partType := "output_text"
	if role != "assistant" {
		partType = "input_text"
	}
	return NewObject().Set("type", NewString(partType)).Set("text", NewString(text))
}

func responsesImagePart(block Block, options *responsesRenderOptions) *Value {
	url := block.URL
	if block.Data != "" {
		mediaType := block.MediaType
		if mediaType == "" {
			mediaType = "image/png"
		}
		url = chatToDataURL(mediaType, block.Data)
	}
	if url == "" {
		options.loss.Dropped(LossImage, options.direction, "no_data_or_url")
		return nil
	}
	part := NewObject().Set("type", NewString("input_image")).Set("image_url", NewString(url))
	if block.CacheHint != nil {
		options.loss.Dropped(LossCacheControl, options.direction, "image")
	}
	return part
}

func responsesReasoningItem(blocks []Block, options *responsesRenderOptions) *Value {
	summary := []*Value{}
	for _, block := range blocks {
		if block.Kind != BlockThinking || block.Text == "" {
			continue
		}
		summary = append(summary, NewObject().
			Set("type", NewString("summary_text")).
			Set("text", NewString(block.Text)))
	}
	if len(summary) == 0 {
		return nil
	}
	if blocks[0].Signature != "" {
		options.loss.Dropped(LossThinkingSignature, options.direction, "responses_has_no_signature")
	}
	return NewObject().Set("type", NewString("reasoning")).Set("summary", NewArray(summary...))
}

func responsesFunctionCallItem(block Block, seed string, options *responsesRenderOptions) *Value {
	name := block.Name
	if options.toWire != nil {
		name = options.toWire(name)
	}
	arguments := block.Args
	if arguments == "" {
		arguments = "{}"
		options.loss.Downgraded(LossUnknownField, options.direction, "tool_call.args.empty")
	}
	return NewObject().
		Set("type", NewString("function_call")).
		Set("call_id", NewString(responsesResolveEmitID(block.ID, seed, options))).
		Set("name", NewString(name)).
		Set("arguments", NewString(arguments))
}

func responsesFunctionCallOutputItem(item Item, options *responsesRenderOptions) *Value {
	texts := []string{}
	for _, block := range item.Blocks {
		if block.Kind == BlockText {
			texts = append(texts, block.Text)
			continue
		}
		options.loss.Dropped(LossUnknownField, options.direction, "function_call_output."+string(block.Kind))
	}
	if item.IsError {
		options.loss.Dropped(LossToolResultIsError, options.direction, "responses_has_no_is_error")
	}
	return NewObject().
		Set("type", NewString("function_call_output")).
		Set("call_id", NewString(responsesResolveEmitID(item.ToolCallID, options.seed, options))).
		Set("output", NewString(strings.Join(texts, "")))
}

func responsesEncodeInput(items []Item, idMap map[string]string, loss *LossCollector, ctx ConvertCtx) []*Value {
	out := []*Value{}
	for index, item := range mergeRenderableItems(items, false) {
		options := &responsesRenderOptions{
			direction: "request",
			seed:      itoa(index),
			idMap:     idMap,
			loss:      loss,
			toWire:    ctx.ToWireToolName,
		}
		switch item.Kind {
		case ItemReasoning:
			if reasoning := responsesReasoningItem(item.Blocks, options); reasoning != nil {
				out = append(out, reasoning)
			}
		case ItemToolResult:
			out = append(out, responsesFunctionCallOutputItem(item, options))
		case ItemMessage:
			parts := []*Value{}
			flush := func() {
				if len(parts) == 0 {
					return
				}
				out = append(out, NewObject().
					Set("type", NewString("message")).
					Set("role", NewString(item.Role)).
					Set("content", NewArray(parts...)))
				parts = nil
			}
			blockIndex := 0
			for _, block := range item.Blocks {
				seed := options.seed + ":" + itoa(blockIndex)
				blockIndex++
				switch block.Kind {
				case BlockText:
					if block.CacheHint != nil {
						loss.Dropped(LossCacheControl, options.direction, "text")
					}
					parts = append(parts, responsesTextPart(item.Role, block.Text))
				case BlockImage:
					if item.Role == "assistant" {
						loss.Dropped(LossImage, options.direction, "assistant_image")
						continue
					}
					if part := responsesImagePart(block, options); part != nil {
						parts = append(parts, part)
					}
				case BlockThinking:
					flush()
					if reasoning := responsesReasoningItem([]Block{block}, options); reasoning != nil {
						out = append(out, reasoning)
					}
				case BlockToolCall:
					flush()
					out = append(out, responsesFunctionCallItem(block, seed, options))
				case BlockDocument:
					loss.Dropped(LossDocument, options.direction, "responses_has_no_document")
				case BlockOpaque:
					if block.Wire != responsesWire {
						loss.Dropped(LossUnknownField, options.direction, "opaque:"+string(block.Wire))
						continue
					}
					parts = append(parts, block.Value)
				}
			}
			flush()
		}
	}
	return out
}

func responsesEncodeToolChoice(choice *ToolChoice, ctx ConvertCtx) *Value {
	if choice.Kind == ChoiceTool {
		name := choice.Name
		if ctx.ToWireToolName != nil {
			name = ctx.ToWireToolName(name)
		}
		return NewObject().Set("type", NewString("function")).Set("name", NewString(name))
	}
	return NewString(string(choice.Kind))
}

func responsesEncodeTools(tools []Tool, ctx ConvertCtx, loss *LossCollector) []*Value {
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
			Set("type", NewString("function")).
			Set("name", NewString(name))
		if tool.HasDescript {
			entry.Set("description", NewString(tool.Description))
		}
		entry.Set("parameters", schema)
		if tool.CacheHint != nil {
			loss.Dropped(LossCacheControl, "request", "tool")
		}
		out = append(out, entry)
	}
	return out
}

func encodeResponsesRequest(request *Request, ctx ConvertCtx) EncodeResult {
	loss := &LossCollector{}
	const direction = "request"
	idMap := map[string]string{}

	out := cloneOrNewObject(passthroughFor(request, responsesWire))
	// 本线保留下来的非 function 工具（解码时进 passthrough.tools）：先取出再清键，末尾按
	// Node 的顺序（canonical 在前 + extra 在后）重建 `tools`。
	var extraTools []*Value
	if preserved, present := out.Get("tools"); present && preserved != nil && preserved.IsArray() {
		extraTools = preserved.Items()
	}
	out.Delete("tools")
	// 外线留存的非 function 工具（如 claude 线没有的 MCP 工具声明）本线无法承载时记损。
	reportForeignPreservedTools(request, responsesWire, loss, direction)
	out.Set("model", NewString(firstString(request.Model, ctx.Model)))
	if request.Stream || ctx.Stream {
		out.Set("stream", NewBool(true))
	}
	if request.ParallelToolCalls != nil {
		out.Set("parallel_tool_calls", NewBool(*request.ParallelToolCalls))
	}
	if len(request.System) > 0 {
		texts := splitTexts(request.System)
		for _, block := range request.System {
			if block.Kind != BlockText {
				loss.Dropped(LossUnknownField, direction, "instructions."+string(block.Kind))
			}
			if block.CacheHint != nil {
				loss.Dropped(LossCacheControl, direction, "instructions")
			}
		}
		out.Set("instructions", NewString(strings.Join(texts, "")))
	}
	out.Set("input", NewArray(responsesEncodeInput(request.Items, idMap, loss, ctx)...))

	canonicalTools := responsesEncodeTools(request.Tools, ctx, loss)
	if len(canonicalTools) > 0 || len(extraTools) > 0 {
		// Node：`body.tools = [...tools, ...extraTools]`——canonical 在前，保留下来的接在后。
		out.Set("tools", NewArray(append(canonicalTools, extraTools...)...))
	}
	if request.ToolChoice != nil {
		out.Set("tool_choice", responsesEncodeToolChoice(request.ToolChoice, ctx))
	}
	if request.Sampling.MaxTokens != nil {
		out.Set("max_output_tokens", NewNumber(jsNumber(*request.Sampling.MaxTokens)))
	}
	if request.Sampling.Temperature != nil {
		out.Set("temperature", NewNumber(jsNumber(*request.Sampling.Temperature)))
	}
	if request.Sampling.TopP != nil {
		out.Set("top_p", NewNumber(jsNumber(*request.Sampling.TopP)))
	}
	if request.Sampling.TopK != nil {
		loss.Dropped(LossTopK, direction, "responses_has_no_top_k")
	}
	if len(request.Sampling.Stop) > 0 {
		loss.Dropped(LossStopSequences, direction, "responses_has_no_stop")
	}
	if request.Sampling.Seed != nil {
		loss.Dropped(LossUnknownField, direction, "seed")
	}
	if request.Reasoning != nil {
		reasoning := NewObject()
		// 等级优先原值搬运；客户端只给预算时按阈值反查降级（responses 线只有等级载体）。
		if level, derived, ok := effectiveThinkingEffort(request.Reasoning); ok {
			reasoning.Set("effort", NewString(level))
			if derived {
				loss.Downgraded(LossThinkingDerived, direction,
					thinkingDerivationDetail(*request.Reasoning.BudgetTokens, "reasoning.effort", level))
			}
		}
		if request.Reasoning.HasSummary && request.Reasoning.Summary != "" {
			reasoning.Set("summary", NewString(request.Reasoning.Summary))
		}
		if len(reasoning.Members()) > 0 {
			out.Set("reasoning", reasoning)
		}
	}
	return EncodeResult{Body: out, Loss: loss.Report()}
}

// ---------------------------------------------------------------------------
// 响应编解码
// ---------------------------------------------------------------------------

func decodeResponsesOutput(raw *Value, loss *LossCollector, fromWire func(string) string) []Block {
	if raw == nil || !raw.IsArray() {
		if raw != nil && !raw.IsNull() {
			loss.Dropped(LossUnknownField, "response", "output")
		}
		return nil
	}
	const direction = "response"
	blocks := []Block{}
	for _, item := range raw.Items() {
		if !isRecord(item) {
			loss.Dropped(LossUnknownField, direction, "output[]")
			continue
		}
		switch contentPartType(item) {
		case "reasoning":
			for _, part := range asUnknownArray(fieldOrNil(item, "summary")) {
				if contentPartType(part) != "summary_text" {
					loss.Dropped(LossUnknownField, direction, "reasoning.summary[]")
					continue
				}
				text, _ := stringField(part, "text")
				blocks = append(blocks, Block{Kind: BlockThinking, Text: text})
			}
		case "message":
			for _, part := range asUnknownArray(fieldOrNil(item, "content")) {
				switch contentPartType(part) {
				case "output_text", "input_text", "text":
					text, _ := stringField(part, "text")
					blocks = append(blocks, textBlock(text))
				case "refusal":
					loss.Dropped(LossUnknownField, direction, "refusal")
				case "input_image":
					blocks = append(blocks, decodeResponsesImagePart(part, loss, direction)...)
				default:
					blocks = append(blocks, opaqueBlock(responsesWire, part))
				}
			}
		case "function_call":
			name, _ := stringField(item, "name")
			if fromWire != nil {
				name = fromWire(name)
			}
			callID, _ := stringField(item, "call_id")
			if callID == "" {
				callID, _ = stringField(item, "id")
			}
			arguments, present := stringField(item, "arguments")
			if !present {
				arguments = "{}"
			}
			blocks = append(blocks, Block{Kind: BlockToolCall, ID: callID, Name: name, Args: arguments})
		case "function_call_output":
			loss.Dropped(LossUnknownField, direction, "output.function_call_output")
		default:
			loss.Dropped(LossUnknownField, direction, "output."+contentPartType(item))
		}
	}
	return blocks
}

func decodeResponsesResponse(body *Value, ctx ConvertCtx) DecodeResult[*Response] {
	loss := &LossCollector{}
	source := body
	if !isRecord(source) {
		source = NewObject()
	}
	id, _ := stringField(source, "id")
	model, _ := stringField(source, "model")
	response := &Response{
		ID:          id,
		Model:       firstString(model, ctx.Model),
		Blocks:      decodeResponsesOutput(fieldOrNil(source, "output"), loss, ctx.FromWireToolName),
		StopReason:  StopUnknown,
		Passthrough: map[WireProtocol]*Value{},
	}
	status, hasStatus := stringField(source, "status")
	response.StopReason = stopReasonFromResponses(
		status,
		hasStatus,
		stringOrEmpty(fieldOrNil(source, "incomplete_details"), "reason"),
	)
	if usageValue, hasUsage := source.Get("usage"); hasUsage && !usageValue.IsNull() {
		if usage := usageFromResponses(usageValue); !usage.IsEmpty() {
			response.Usage = usage
		}
	}
	if passthrough := collectPassthrough(source, responsesKnownResponseKeys); len(passthrough.Members()) > 0 {
		response.Passthrough[responsesWire] = passthrough
	}
	return DecodeResult[*Response]{Value: response, Loss: loss.Report()}
}

func encodeResponsesOutput(response *Response, loss *LossCollector, idMap map[string]string) []*Value {
	out := []*Value{}
	options := &responsesRenderOptions{
		direction: "response", seed: "0", idMap: idMap, loss: loss,
	}
	texts := []string{}
	flush := func() {
		if len(texts) == 0 {
			return
		}
		out = append(out, NewObject().
			Set("type", NewString("message")).
			Set("role", NewString("assistant")).
			Set("content", NewArray(textParts(texts)...)).
			Set("status", NewString("completed")))
		texts = nil
	}
	blockIndex := 0
	for _, block := range response.Blocks {
		seed := "0:" + itoa(blockIndex)
		blockIndex++
		switch block.Kind {
		case BlockText:
			if block.CacheHint != nil {
				loss.Dropped(LossCacheControl, options.direction, "text")
			}
			texts = append(texts, block.Text)
		case BlockThinking:
			flush()
			if reasoning := responsesReasoningItem([]Block{block}, options); reasoning != nil {
				out = append(out, reasoning)
			}
		case BlockToolCall:
			flush()
			out = append(out, responsesFunctionCallItem(block, seed, options))
		case BlockImage:
			loss.Dropped(LossImage, options.direction, "output_image")
		case BlockDocument:
			loss.Dropped(LossDocument, options.direction, "responses_has_no_document")
		case BlockOpaque:
			if block.Wire != responsesWire {
				// 外线 opaque：专用类别归因（MCP / web_search / 其余非 function 工具）。
				class, detail := nonFunctionToolLossClass(block.Wire, block.Value)
				loss.Dropped(class, options.direction, detail)
				continue
			}
			flush()
			out = append(out, block.Value)
		}
	}
	flush()
	return out
}

func textParts(texts []string) []*Value {
	out := make([]*Value, 0, len(texts))
	for _, text := range texts {
		out = append(out, NewObject().
			Set("type", NewString("output_text")).
			Set("text", NewString(text)))
	}
	return out
}

func encodeResponsesResponse(response *Response, ctx ConvertCtx) EncodeResult {
	loss := &LossCollector{}
	out := cloneOrNewObject(responsePassthroughFor(response, responsesWire))
	out.Set("id", NewString(response.ID))
	out.Set("object", NewString("response"))
	out.Set("model", NewString(firstString(response.Model, ctx.Model)))

	status := stopReasonToResponses(response.StopReason, false)
	out.Set("status", NewString(status.Status))
	if status.IncompleteDetails != nil {
		out.Set("incomplete_details", status.IncompleteDetails)
	}
	out.Set("output", NewArray(encodeResponsesOutput(response, loss, map[string]string{})...))
	if response.Usage != nil && !response.Usage.IsEmpty() {
		out.Set("usage", usageToResponses(response.Usage))
	}
	return EncodeResult{Body: out, Loss: loss.Report()}
}
