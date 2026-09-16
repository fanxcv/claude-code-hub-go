package convert

import "strings"

const chatWire = ProtocolOpenAIChat

func init() { registerCodec(chatWire) }

// chatCarriedMessageKeys 是枢纽已承载的 message 级字段。
var chatCarriedMessageKeys = knownKeys{
	"role": true, "content": true, "tool_calls": true, "tool_call_id": true,
	"reasoning_content": true, "reasoning": true,
}

// chatKnownRequestKeys 是枢纽已承载（或归入 passthrough）的顶层字段。
var chatKnownRequestKeys = knownKeys{
	"model": true, "messages": true, "stream": true, "tools": true, "tool_choice": true,
	"parallel_tool_calls": true, "max_tokens": true, "max_completion_tokens": true,
	"temperature": true, "top_p": true, "stop": true, "seed": true,
	"reasoning_effort": true, "reasoning": true,
}

var chatKnownResponseKeys = knownKeys{"id": true, "model": true, "choices": true, "usage": true}

// ---------------------------------------------------------------------------
// 解码：Chat -> 枢纽
// ---------------------------------------------------------------------------

// decodeChatReasoning 取 Chat 线内容载体里的思考文本（reasoning_content 优先，回落裸 reasoning）。
func decodeChatReasoning(source *Value) (string, bool) {
	if primary, ok := stringField(source, "reasoning_content"); ok && primary != "" {
		return primary, true
	}
	if fallback, ok := stringField(source, "reasoning"); ok && fallback != "" {
		return fallback, true
	}
	return "", false
}

// decodeChatMessageBlocks 把一条 Chat message 解为枢纽块序列。
func decodeChatMessageBlocks(
	message *Value,
	seed string,
	loss *LossCollector,
	direction string,
	fromWire func(string) string,
) []Block {
	blocks := []Block{}

	if reasoning, ok := decodeChatReasoning(message); ok {
		blocks = append(blocks, Block{Kind: BlockThinking, Text: reasoning})
	}
	blocks = append(blocks, chatContentToBlocks(fieldOrNil(message, "content"), loss, direction)...)

	toolCalls := fieldOrNil(message, "tool_calls")
	switch {
	case toolCalls != nil && toolCalls.IsArray():
		for index, raw := range toolCalls.Items() {
			blocks = append(blocks, decodeChatToolCall(raw, seed+":"+itoa(index), loss, direction, fromWire))
		}
	case toolCalls != nil && !toolCalls.IsNull():
		loss.Dropped(LossUnknownField, direction, "tool_calls")
	}

	for _, member := range message.Members() {
		if chatCarriedMessageKeys.contains(member.Key) {
			continue
		}
		if member.Value.IsNull() {
			continue
		}
		loss.Dropped(LossUnknownField, direction, "message."+member.Key)
	}
	return blocks
}

func decodeChatToolCall(
	raw *Value,
	seed string,
	loss *LossCollector,
	direction string,
	fromWire func(string) string,
) Block {
	if !isRecord(raw) {
		loss.Rewritten(LossUnknownField, direction, "tool_calls[]")
		return opaqueBlock(chatWire, raw)
	}
	fn := fieldOrNil(raw, "function")
	if fn == nil || !fn.IsObject() {
		fn = NewObject()
	}
	wireName, hasName := stringField(fn, "name")
	name := ""
	if hasName {
		name = wireName
		if fromWire != nil {
			name = fromWire(wireName)
		}
	}

	arguments := fieldOrNil(fn, "arguments")
	args := ""
	switch {
	case arguments != nil && arguments.Kind() == "string":
		args, _ = arguments.String()
	case arguments == nil || arguments.IsNull():
		args = ""
	case arguments.IsObject():
		args = arguments.MarshalCompact()
		loss.Rewritten(LossUnknownField, direction, "tool_call.arguments")
	default:
		args = ""
		loss.Downgraded(LossUnknownField, direction, "tool_call.arguments")
	}

	rawID, hasID := stringField(raw, "id")
	if !hasID || rawID == "" {
		loss.Rewritten(LossToolCallIDRewritten, direction, "synthesized:"+seed)
		return Block{Kind: BlockToolCall, ID: MakeToolCallID(seed), Name: name, Args: args}
	}
	return Block{Kind: BlockToolCall, ID: rawID, Name: name, Args: args}
}

func chatContentToBlocks(content *Value, loss *LossCollector, direction string) []Block {
	if content == nil || content.IsNull() {
		return nil
	}
	if content.Kind() == "string" {
		text, _ := content.String()
		return []Block{textBlock(text)}
	}
	if !content.IsArray() {
		loss.Rewritten(LossUnknownField, direction, "content."+content.Kind())
		return []Block{opaqueBlock(chatWire, content)}
	}
	blocks := []Block{}
	for _, part := range content.Items() {
		blocks = append(blocks, chatContentPartToBlocks(part, loss, direction)...)
	}
	return blocks
}

func chatContentPartToBlocks(part *Value, loss *LossCollector, direction string) []Block {
	if part == nil {
		return nil
	}
	if part.Kind() == "string" {
		text, _ := part.String()
		return []Block{textBlock(text)}
	}
	if !part.IsObject() {
		loss.Rewritten(LossUnknownField, direction, "content_part."+part.Kind())
		return []Block{opaqueBlock(chatWire, part)}
	}
	partType, _ := stringField(part, "type")
	switch partType {
	case "text":
		for _, extra := range meaningfulExtras(part, "type", "text") {
			loss.Dropped(LossUnknownField, direction, "content_part."+extra)
		}
		text, _ := stringField(part, "text")
		return []Block{textBlock(text)}
	case "image_url":
		return []Block{chatImagePartToBlock(part, loss, direction)}
	default:
		return []Block{opaqueBlock(chatWire, part)}
	}
}

// chatImageMediaTypes 把 URL 扩展名映射为媒体类型。
var chatImageMediaTypes = map[string]string{
	"png": "image/png", "jpg": "image/jpeg", "jpeg": "image/jpeg", "gif": "image/gif",
	"webp": "image/webp", "bmp": "image/bmp", "svg": "image/svg+xml",
}

func chatImagePartToBlock(part *Value, loss *LossCollector, direction string) Block {
	raw := fieldOrNil(part, "image_url")
	url := ""
	switch {
	case raw != nil && raw.Kind() == "string":
		url, _ = raw.String()
	case raw != nil && raw.IsObject():
		url, _ = stringField(raw, "url")
	}
	if url == "" {
		loss.Rewritten(LossUnknownField, direction, "image_url.missing")
		return opaqueBlock(chatWire, part)
	}
	if raw != nil && raw.IsObject() {
		for _, member := range raw.Members() {
			if member.Key == "url" || member.Value.IsNull() {
				continue
			}
			loss.Dropped(LossUnknownField, direction, "image_url."+member.Key)
		}
	}
	for _, extra := range meaningfulExtras(part, "type", "image_url") {
		loss.Dropped(LossUnknownField, direction, "content_part."+extra)
	}

	if strings.HasPrefix(url, "data:") {
		parsed, ok := parseChatDataURL(url)
		if !ok || !strings.HasPrefix(parsed.MediaType, "image/") {
			loss.Rewritten(LossUnknownField, direction, "image_url.data_url")
			return opaqueBlock(chatWire, part)
		}
		// 不在此记损：同一张图的最终去向只有编码侧知道（可能被目标线整块丢掉），
		// 两处都记就是一张图两条台账（见 Block.FromDataURL）。
		return Block{Kind: BlockImage, MediaType: parsed.MediaType, Data: parsed.Data, FromDataURL: true}
	}
	return Block{Kind: BlockImage, MediaType: chatGuessMediaType(url), URL: url}
}

type dataURLParts struct {
	MediaType string
	Data      string
}

func parseChatDataURL(url string) (dataURLParts, bool) {
	if !strings.HasPrefix(url, "data:") {
		return dataURLParts{}, false
	}
	comma := strings.IndexByte(url, ',')
	if comma < 0 {
		return dataURLParts{}, false
	}
	header := url[5:comma]
	if !strings.HasSuffix(strings.ToLower(header), ";base64") {
		return dataURLParts{}, false
	}
	mediaType := header[:len(header)-len(";base64")]
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	return dataURLParts{MediaType: mediaType, Data: url[comma+1:]}, true
}

func chatGuessMediaType(url string) string {
	base := url
	if index := strings.IndexAny(base, "?#"); index >= 0 {
		base = base[:index]
	}
	dot := strings.LastIndexByte(base, '.')
	if dot < 0 || dot == len(base)-1 {
		return "image/unknown"
	}
	ext := strings.ToLower(base[dot+1:])
	if mediaType, ok := chatImageMediaTypes[ext]; ok {
		return mediaType
	}
	return "image/unknown"
}

func chatToDataURL(mediaType, data string) string {
	return "data:" + mediaType + ";base64," + data
}

func decodeChatTools(raw *Value, loss *LossCollector) []Tool {
	if raw == nil {
		return nil
	}
	if !raw.IsArray() {
		loss.Dropped(LossUnknownField, "request", "tools")
		return nil
	}
	tools := []Tool{}
	for _, entry := range raw.Items() {
		fn := fieldOrNil(entry, "function")
		if !isRecord(entry) || !isRecord(fn) {
			loss.Dropped(LossUnknownField, "request", "tools[]")
			continue
		}
		entryType, _ := stringField(entry, "type")
		name, hasName := stringField(fn, "name")
		if entryType != "function" || !hasName || name == "" {
			loss.Dropped(LossUnknownField, "request", "tools[].type")
			continue
		}
		tool := Tool{Name: name, Parameters: NewObject()}
		if description, ok := stringField(fn, "description"); ok {
			tool.Description = description
			tool.HasDescript = true
		}
		if parameters := fieldOrNil(fn, "parameters"); parameters != nil && parameters.IsObject() {
			tool.Parameters = parameters
		} else {
			if parameters != nil && !parameters.IsNull() {
				loss.Downgraded(LossUnknownField, "request", "tool.parameters")
			}
		}
		tools = append(tools, tool)
		for _, member := range fn.Members() {
			if member.Key == "name" || member.Key == "description" || member.Key == "parameters" {
				continue
			}
			if member.Value.IsNull() {
				continue
			}
			loss.Dropped(LossUnknownField, "request", "tool."+member.Key)
		}
		for _, member := range entry.Members() {
			if member.Key == "type" || member.Key == "function" {
				continue
			}
			if member.Value.IsNull() {
				continue
			}
			loss.Dropped(LossUnknownField, "request", "tool."+member.Key)
		}
	}
	return tools
}

func decodeChatToolChoice(raw *Value, loss *LossCollector, has bool) *ToolChoice {
	if !has || raw == nil || raw.IsNull() {
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
		fn := fieldOrNil(raw, "function")
		if isRecord(fn) {
			if name, ok := stringField(fn, "name"); ok && name != "" {
				return &ToolChoice{Kind: ChoiceTool, Name: name}
			}
		}
	}
	loss.Dropped(LossUnknownField, "request", "tool_choice")
	return nil
}

func decodeChatSampling(body *Value, loss *LossCollector) Sampling {
	sampling := Sampling{}
	maxCompletion, hasCompletion := numberField(body, "max_completion_tokens")
	maxLegacy, hasLegacy := numberField(body, "max_tokens")
	if hasCompletion {
		sampling.MaxTokens = maxCompletion
	} else if hasLegacy {
		sampling.MaxTokens = maxLegacy
	}
	if hasCompletion && hasLegacy {
		loss.Dropped(LossUnknownField, "request", "max_tokens")
	}
	if temperature, ok := numberField(body, "temperature"); ok {
		sampling.Temperature = temperature
	}
	if topP, ok := numberField(body, "top_p"); ok {
		sampling.TopP = topP
	}
	if _, present := body.Get("seed"); present {
		if seed, ok := numberField(body, "seed"); ok {
			sampling.Seed = seed
		}
	}
	stop, hasStop := body.Get("stop")
	switch {
	case hasStop && stop.Kind() == "string":
		text, _ := stop.String()
		sampling.Stop = []string{text}
	case hasStop && stop.IsArray():
		stops := []string{}
		for _, entry := range stop.Items() {
			if entry.Kind() == "string" {
				text, _ := entry.String()
				stops = append(stops, text)
				continue
			}
			loss.Dropped(LossUnknownField, "request", "stop[]")
		}
		if len(stops) > 0 {
			sampling.Stop = stops
		}
	case hasStop && !stop.IsNull():
		loss.Dropped(LossUnknownField, "request", "stop")
	}
	return sampling
}

func decodeChatReasoningConfig(body *Value) *Reasoning {
	if effort, ok := stringField(body, "reasoning_effort"); ok && effort != "" {
		return &Reasoning{Effort: effort, HasEffort: true}
	}
	nested := fieldOrNil(body, "reasoning")
	if isRecord(nested) {
		if effort, ok := stringField(nested, "effort"); ok && effort != "" {
			return &Reasoning{Effort: effort, HasEffort: true}
		}
	}
	return nil
}

func decodeChatRequest(body *Value, ctx ConvertCtx) DecodeResult[*Request] {
	loss := &LossCollector{}
	source := body
	if !isRecord(source) {
		source = NewObject()
	}

	system := []Block{}
	items := []Item{}
	decodeChatMessages(fieldOrNil(source, "messages"), &system, &items, loss)

	request := &Request{
		Model:       firstString(stringOrEmpty(source, "model"), ctx.Model),
		Items:       items,
		Sampling:    decodeChatSampling(source, loss),
		Stream:      chatIsStream(source),
		Passthrough: map[WireProtocol]*Value{},
	}
	if len(system) > 0 {
		request.System = system
	}
	if tools := decodeChatTools(fieldOrNil(source, "tools"), loss); len(tools) > 0 {
		request.Tools = tools
	}
	if choice := decodeChatToolChoice(fieldOrNil(source, "tool_choice"), loss, source.Has("tool_choice")); choice != nil {
		request.ToolChoice = choice
	}
	if parallel, ok := boolField(source, "parallel_tool_calls"); ok {
		request.ParallelToolCalls = &parallel
	} else if value, present := source.Get("parallel_tool_calls"); present && !value.IsNull() {
		loss.Dropped(LossUnknownField, "request", "parallel_tool_calls")
	}
	if reasoning := decodeChatReasoningConfig(source); reasoning != nil {
		request.Reasoning = reasoning
	}
	if passthrough := collectPassthrough(source, chatKnownRequestKeys); len(passthrough.Members()) > 0 {
		request.Passthrough[chatWire] = passthrough
	}
	return DecodeResult[*Request]{Value: request, Loss: loss.Report()}
}

func chatIsStream(source *Value) bool {
	value, present := source.Get("stream")
	if !present {
		return false
	}
	boolean, ok := value.Bool()
	return ok && boolean
}

func decodeChatMessages(raw *Value, system *[]Block, items *[]Item, loss *LossCollector) {
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
		switch role {
		case "system":
			if index != 0 {
				loss.Rewritten("system.position", direction, "messages["+itoa(index)+"]")
			}
			*system = append(*system, decodeChatMessageBlocks(message, "system:"+itoa(index), loss, direction, nil)...)
		case "user", "assistant":
			*items = append(*items, Item{
				Kind:   ItemMessage,
				Role:   role,
				Blocks: decodeChatMessageBlocks(message, itoa(index), loss, direction, nil),
			})
		case "tool":
			toolCallID, hasID := stringField(message, "tool_call_id")
			if hasID && toolCallID != "" {
				*items = append(*items, Item{
					Kind:       ItemToolResult,
					ToolCallID: toolCallID,
					Blocks:     decodeChatMessageBlocks(message, itoa(index), loss, direction, nil),
					IsError:    false,
				})
				continue
			}
			loss.Rewritten(LossUnknownField, direction, "messages["+itoa(index)+"].tool_call_id")
			*items = append(*items, boxedChatMessage(message))
		default:
			loss.Rewritten(LossUnknownField, direction, "messages["+itoa(index)+"].role")
			*items = append(*items, boxedChatMessage(message))
		}
	}
}

func boxedChatMessage(message *Value) Item {
	return Item{Kind: ItemMessage, Role: "user", Blocks: []Block{opaqueBlock(chatWire, message)}}
}

// ---------------------------------------------------------------------------
// 编码：枢纽 -> Chat
// ---------------------------------------------------------------------------

type chatRenderOptions struct {
	direction string
	seed      string
	idMap     map[string]string
	loss      *LossCollector
	toWire    func(string) string
}

func chatIsSafeToolID(id string) bool {
	if len(id) > 256 {
		return false
	}
	for i := 0; i < len(id); i++ {
		b := id[i]
		if b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f' || b < 0x20 || b == 0x7f {
			return false
		}
	}
	return true
}

func chatResolveEmitToolCallID(original string, seed string, options *chatRenderOptions) string {
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

func chatRenderThinking(block Block, role string, reasoning *[]string, options *chatRenderOptions) {
	if role != "assistant" {
		options.loss.Dropped(LossThinkingBlock, options.direction, "thinking.in_"+role)
		return
	}
	if block.Redacted {
		options.loss.Dropped(LossThinkingBlock, options.direction, "redacted")
		return
	}
	if block.Signature != "" {
		options.loss.Dropped(LossThinkingSignature, options.direction, "chat_has_no_signature")
	}
	if block.Text != "" {
		*reasoning = append(*reasoning, block.Text)
	}
}

func chatRenderToolCall(block Block, seed string, options *chatRenderOptions) *Value {
	// chat 线的 tool_calls[] 只有 id / type / function 三层，没有块级 cache_control 承载位
	// （只有 anthropic 线渲染它，见 hub.go 的 CacheHint 说明）；与文本/图片等同口径记损。
	if block.CacheHint != nil {
		options.loss.Dropped(LossCacheControl, options.direction, "tool_call")
	}
	id := chatResolveEmitToolCallID(block.ID, seed, options)
	rawArgs := block.Args
	if rawArgs == "" {
		options.loss.Downgraded(LossUnknownField, options.direction, "tool_call.args.empty")
	}
	name := block.Name
	if options.toWire != nil {
		name = options.toWire(name)
	}
	arguments := rawArgs
	if arguments == "" {
		arguments = "{}"
	}
	fn := NewObject().Set("name", NewString(name)).Set("arguments", NewString(arguments))
	return NewObject().Set("id", NewString(id)).Set("type", NewString("function")).Set("function", fn)
}

func chatRenderMessages(role string, blocks []Block, options *chatRenderOptions) []*Value {
	out := []*Value{}
	texts := []string{}
	parts := []*Value{}
	toolCalls := []*Value{}
	reasoning := []string{}
	blockIndex := 0

	flush := func() {
		joined := strings.Join(texts, "")
		hasText := len(texts) > 0
		// 空串与纯空白都算「无可见正文」：严格上游把 content:"" 也判为缺失。
		hasVisibleText := strings.TrimSpace(joined) != ""
		pendingParts, pendingToolCalls, pendingReasoning := parts, toolCalls, reasoning
		// 先复位累积、再决定是否产出：下面的早期 return 会绕过尾部复位，若先把内容取出
		// 再 return，同一批块会在下一次 flush 里被重复产出一条消息。
		texts, parts, toolCalls, reasoning = nil, nil, nil, nil

		if !hasText && len(pendingParts) == 0 && len(pendingToolCalls) == 0 && len(pendingReasoning) == 0 {
			return
		}
		message := NewObject().Set("role", NewString(role))
		switch {
		case len(pendingParts) > 0:
			content := []*Value{}
			if hasVisibleText {
				content = append(content, NewObject().
					Set("type", NewString("text")).
					Set("text", NewString(joined)))
			}
			content = append(content, pendingParts...)
			message.Set("content", NewArray(content...))
		case hasVisibleText:
			message.Set("content", NewString(joined))
		case len(pendingToolCalls) > 0:
			message.Set("content", NewNull())
		case role == "assistant" && len(pendingReasoning) > 0:
			// 仅思考的 assistant：chat 线没有思考的正文承载位，而「无正文且无 tool_calls」
			// 会被严格上游判为畸形（OpenCode 系「Console Go」实测报 `Invalid assistant
			// message: content or tool_calls`；空串同样算缺失，故不能写 content:""）。
			// 把思考文本落到 content；reasoning_content 照旧保留——另一类 chat 上游
			// （thinking 模式）反向要求历史 assistant 轮次必须把思考带回来，缺失报
			// `reasoning_content must be passed back`。两条要求互斥，只有两处都写才同时满足。
			message.Set("content", NewString(strings.Join(pendingReasoning, "")))
			options.loss.Rewritten(LossAssistantContentEmpty, options.direction, "reasoning.as_content")
		case role == "assistant":
			// 无正文、无工具、无思考的 assistant：整条不产出（与 anthropic 侧
			// encodeAnthropicMessage「无可见内容时返回 nil」同口径）——不产出就不可能畸形。
			options.loss.Dropped(LossAssistantContentEmpty, options.direction, "assistant.empty_message")
			return
		default:
			// 非 assistant 角色的纯空白消息沿用既有形状（本轮不改其语义，避免影响无故障流量）。
			message.Set("content", NewString(""))
		}
		if len(pendingReasoning) > 0 {
			message.Set("reasoning_content", NewString(strings.Join(pendingReasoning, "")))
			options.loss.Downgraded(LossThinkingBlock, options.direction, "reasoning_content")
		}
		if len(pendingToolCalls) > 0 {
			message.Set("tool_calls", NewArray(pendingToolCalls...))
		}
		out = append(out, message)
	}

	for _, block := range blocks {
		seed := options.seed + ":" + itoa(blockIndex)
		blockIndex++
		switch block.Kind {
		case BlockText:
			if block.CacheHint != nil {
				options.loss.Dropped(LossCacheControl, options.direction, "text")
			}
			texts = append(texts, block.Text)
		case BlockImage:
			if part := chatImageBlockToPart(block, options.loss, options.direction); part != nil {
				parts = append(parts, part)
			}
		case BlockDocument:
			if block.CacheHint != nil {
				options.loss.Dropped(LossCacheControl, options.direction, "document")
			}
			options.loss.Dropped(LossDocument, options.direction, "chat_has_no_document")
		case BlockThinking:
			chatRenderThinking(block, role, &reasoning, options)
		case BlockToolCall:
			if role != "assistant" {
				options.loss.Dropped(LossUnknownField, options.direction, "tool_call.outside_assistant")
				break
			}
			toolCalls = append(toolCalls, chatRenderToolCall(block, seed, options))
		case BlockOpaque:
			if block.Wire != chatWire {
				// 外线 opaque（如 responses 的 mcp_call / web_search_call 项）：专用类别归因。
				class, detail := nonFunctionToolLossClass(block.Wire, block.Value)
				options.loss.Dropped(class, options.direction, detail)
				break
			}
			if isChatWholeMessage(block.Value) {
				flush()
				out = append(out, block.Value)
				break
			}
			parts = append(parts, block.Value)
		default:
			options.loss.Dropped(LossUnknownField, options.direction, "block")
		}
	}
	flush()
	return out
}

func isChatWholeMessage(value *Value) bool {
	if !isRecord(value) {
		return false
	}
	_, ok := stringField(value, "role")
	return ok
}

func chatImageBlockToPart(block Block, loss *LossCollector, direction string) *Value {
	if block.CacheHint != nil {
		loss.Dropped(LossCacheControl, direction, "image")
	}
	if !strings.HasPrefix(block.MediaType, "image/") {
		loss.Dropped(LossImage, direction, "media_type:"+block.MediaType)
		return nil
	}
	if strings.HasSuffix(block.MediaType, "/gif") {
		loss.Dropped(LossImage, direction, "image/gif")
		return nil
	}
	if block.Data != "" {
		// 损失记在编码侧（解码侧只置 FromDataURL）：解码侧再记一次就是一张图两条台账
		// （生产实测把 image 计数抬成实际值的两倍）。记在此处的另一个好处是
		// 「最终结果为准」自动成立：上面的 GIF / 非图片媒体分支已经 return，被丢掉的那张图
		// 只留一条 dropped，不会被这里补成「既改写又丢弃」。
		if block.FromDataURL {
			loss.Rewritten(LossImage, direction, "data_url_to_base64")
		}
		return NewObject().
			Set("type", NewString("image_url")).
			Set("image_url", NewObject().Set("url", NewString(chatToDataURL(block.MediaType, block.Data))))
	}
	if block.URL != "" {
		return NewObject().
			Set("type", NewString("image_url")).
			Set("image_url", NewObject().Set("url", NewString(block.URL)))
	}
	loss.Dropped(LossImage, direction, "no_data_or_url")
	return nil
}

func chatRenderToolMessage(item Item, options *chatRenderOptions) *Value {
	texts := []string{}
	for _, block := range item.Blocks {
		if block.Kind == BlockText {
			if block.CacheHint != nil {
				options.loss.Dropped(LossCacheControl, options.direction, "tool_result")
			}
			texts = append(texts, block.Text)
			continue
		}
		options.loss.Dropped(LossUnknownField, options.direction, "tool_result."+string(block.Kind))
	}
	// tool_result 块自身的 cache_control：chat 线的 role:tool 消息只有 content，无承载位。
	if item.CacheHint != nil {
		options.loss.Dropped(LossCacheControl, options.direction, "tool_result")
	}
	if item.IsError {
		options.loss.Dropped(LossToolResultIsError, options.direction, "chat_has_no_is_error")
	}
	return NewObject().
		Set("role", NewString("tool")).
		Set("tool_call_id", NewString(chatResolveEmitToolCallID(item.ToolCallID, options.seed, options))).
		Set("content", NewString(strings.Join(texts, "")))
}

// foldReasoningIntoFollowingAssistant 把 reasoning 项的思考**并入紧随其后的 assistant 消息**。
//
// 为什么必须并：chat 线上 `reasoning_content` 是 assistant 消息**自身**的字段。若把 reasoning 项
// 单独渲染成一条 assistant 消息，紧随其后那条真正承载本轮输出的消息（正文或 tool_calls）就**没有**
// `reasoning_content`；而上游规则是「带 `tools` 且开启思考模式时，历史里**每个** assistant 轮次都必须
// 带 `reasoning_content`」——生产实测（OpenCode X Chat，`/v1/responses` 入站）因此报
// `[invalid_request_error] The reasoning_content in the thinking mode must be passed back to the API.`
//
// 为何这条缺陷会**把供应商推进熔断**：该 400 属 `forward.CategoryProviderError`，而
// `CountsTowardCircuit` 为真（与 Node `errors.ts:557`「所有 4xx/5xx → 计入熔断器」同判）。
// 于是**我方转换形状的缺陷**被记成供应商失败、累计到阈值即开闸——生产上 provider 138 的开闸
// 末次失败正是这一条。修形状（而不是改判定）才是根因修复：判定的对齐关系不能动。
//
// 合并方向恒为「向后」：responses 的 reasoning 项**必须排在其所属 assistant 输出之前**
// （Node `request.ts:768` 的契约注释「§2.3 规则 2」），故不会张冠李戴。
//
// 找不到落点的 reasoning（其后没有 assistant 输出）保持原有语义：仍作为独立 assistant 消息渲染，
// 由 chatRenderMessages 套用「content 取思考文本 + 保留 reasoning_content」的两处都写不变式
// （否则会撞另一类 chat 上游的 `Invalid assistant message: content or tool_calls`）。
func foldReasoningIntoFollowingAssistant(items []Item, loss *LossCollector) []Item {
	out := make([]Item, 0, len(items))
	pending := []Block{}
	// flush 把仍未找到落点的思考按原语义产出为独立的 reasoning 项。
	flush := func() {
		if len(pending) == 0 {
			return
		}
		out = append(out, Item{Kind: ItemReasoning, Role: "assistant", Blocks: pending})
		pending = nil
	}
	for _, item := range items {
		if item.Kind == ItemReasoning {
			for _, block := range item.Blocks {
				if block.Kind != BlockThinking {
					loss.Dropped(LossReasoningReplay, "request", "reasoning."+string(block.Kind))
					continue
				}
				pending = append(pending, block)
			}
			continue
		}
		if item.Kind == ItemMessage && item.Role == "assistant" && len(pending) > 0 {
			blocks := make([]Block, 0, len(pending)+len(item.Blocks))
			blocks = append(blocks, pending...)
			blocks = append(blocks, item.Blocks...)
			item.Blocks = blocks
			pending = nil
			out = append(out, item)
			continue
		}
		flush()
		out = append(out, item)
	}
	flush()
	return out
}

func chatEncodeItems(items []Item, idMap map[string]string, loss *LossCollector, ctx ConvertCtx) []*Value {
	messages := []*Value{}
	const direction = "request"
	for index, item := range foldReasoningIntoFollowingAssistant(mergeRenderableItems(items, false), loss) {
		options := &chatRenderOptions{
			direction: direction,
			seed:      itoa(index),
			idMap:     idMap,
			loss:      loss,
			toWire:    ctx.ToWireToolName,
		}
		switch item.Kind {
		case ItemMessage:
			if len(item.Blocks) == 0 {
				// 空 assistant 消息：既无正文也无工具，出站即畸形（见 chatRenderMessages 的说明），
				// 故整条不产出；其余角色沿用「写空串」的既有形状。
				if item.Role == "assistant" {
					loss.Dropped(LossAssistantContentEmpty, direction, "assistant.empty_message")
					continue
				}
				messages = append(messages, NewObject().
					Set("role", NewString(item.Role)).
					Set("content", NewString("")))
				continue
			}
			messages = append(messages, chatRenderMessages(item.Role, item.Blocks, options)...)
		case ItemToolResult:
			messages = append(messages, chatRenderToolMessage(item, options))
		case ItemReasoning:
			thinking := []Block{}
			for _, block := range item.Blocks {
				if block.Kind != BlockThinking {
					loss.Dropped(LossReasoningReplay, direction, "reasoning."+string(block.Kind))
					continue
				}
				thinking = append(thinking, block)
			}
			if len(thinking) == 0 {
				continue
			}
			messages = append(messages, chatRenderMessages("assistant", thinking, options)...)
		}
	}
	return messages
}

// backfillTrailingToolCallReasoning 给「末尾 tool 结果所属的那条 assistant(tool_calls) 消息」
// 补一个空 reasoning_content。
//
// 为什么补：上游 Console Go（OpenCode 系）真上游二分实测（2026-09-14，36 次请求）——仅当
// 「历史以 tool 结果收尾」且「该 tool 结果归属的 assistant(tool_calls) 轮缺 reasoning_content」
// 时回 400 `The reasoning_content in the thinking mode must be passed back to the API.`；
// 空串被上游视作「已传回」（同载荷写 reasoning_content:"" 即 200），末尾另加一条 user 也 200。
// 生产形态见 965278（`/v1/responses` 入站）：客户端 omit 空 thinking 后我方出站即缺该字段。
//
// 为什么只补这一条：触发条件只落在末轮（更早轮次缺该字段不报），无差别补全等于往所有历史
// assistant 轮次写字段；最小改动只覆盖已证实的形态。空串不含信息，故不属伪造思考内容——
// 无该形态的请求（末条非 tool）一字不动，`TestReasoningAbsentIsNotFabricated` 即其反证。
func backfillTrailingToolCallReasoning(messages []*Value, loss *LossCollector, direction string) {
	if len(messages) == 0 {
		return
	}
	if role, _ := stringField(messages[len(messages)-1], "role"); role != "tool" {
		return
	}
	for index := len(messages) - 1; index >= 0; index-- {
		message := messages[index]
		role, _ := stringField(message, "role")
		if role == "tool" {
			continue
		}
		if role != "assistant" {
			return
		}
		if _, hasTools := message.Get("tool_calls"); !hasTools {
			return
		}
		if reasoning, present := message.Get("reasoning_content"); present && reasoning != nil {
			return
		}
		message.Set("reasoning_content", NewString(""))
		loss.Downgraded(LossReasoningReplay, direction, "reasoning_content.empty_backfill")
		return
	}
}

func chatEncodeToolChoice(choice *ToolChoice, ctx ConvertCtx) *Value {
	if choice.Kind == ChoiceTool {
		name := choice.Name
		if ctx.ToWireToolName != nil {
			name = ctx.ToWireToolName(name)
		}
		return NewObject().
			Set("type", NewString("function")).
			Set("function", NewObject().Set("name", NewString(name)))
	}
	return NewValueString(string(choice.Kind))
}

func chatEncodeTools(tools []Tool, ctx ConvertCtx, loss *LossCollector) []*Value {
	out := []*Value{}
	for _, tool := range tools {
		name := tool.Name
		if ctx.ToWireToolName != nil {
			name = ctx.ToWireToolName(name)
		}
		fn := NewObject().Set("name", NewString(name))
		if tool.HasDescript {
			fn.Set("description", NewString(tool.Description))
		}
		if tool.Parameters == nil {
			fn.Set("parameters", NewObject())
		} else {
			fn.Set("parameters", tool.Parameters)
		}
		// chat 线的 tools[] 只有 type/function 两层，没有工具级 cache_control 承载位（只有 anthropic
		// 线渲染它，见 hub.go 的 CacheHint 说明）；与 responses 编码器同口径记损，避免跨线静默丢失。
		if tool.CacheHint != nil {
			loss.Dropped(LossCacheControl, "request", "tool")
		}
		out = append(out, NewObject().Set("type", NewString("function")).Set("function", fn))
	}
	return out
}

func encodeChatRequest(request *Request, ctx ConvertCtx) EncodeResult {
	loss := &LossCollector{}
	const direction = "request"
	idMap := map[string]string{}

	out := cloneOrNewObject(passthroughFor(request, chatWire))
	// 外线留存的非 function 工具（如 responses 线的 MCP / web_search 工具声明）本线无法承载时记损。
	reportForeignPreservedTools(request, chatWire, loss, direction)
	reportForeignDroppableFields(request, chatWire, loss, direction, ctx)
	out.Set("model", NewString(firstString(ctx.Model, request.Model)))
	out.Set("stream", NewBool(request.Stream || ctx.Stream))

	messages := []*Value{}
	messages = append(messages, chatRenderMessages("system", request.System, &chatRenderOptions{
		direction: direction, seed: "system", idMap: idMap, loss: loss, toWire: ctx.ToWireToolName,
	})...)
	messages = append(messages, chatEncodeItems(request.Items, idMap, loss, ctx)...)
	backfillTrailingToolCallReasoning(messages, loss, direction)
	out.Set("messages", NewArray(messages...))

	if len(request.Tools) > 0 {
		out.Set("tools", NewArray(chatEncodeTools(request.Tools, ctx, loss)...))
	}
	if request.ToolChoice != nil {
		out.Set("tool_choice", chatEncodeToolChoice(request.ToolChoice, ctx))
	}
	if request.ParallelToolCalls != nil {
		out.Set("parallel_tool_calls", NewBool(*request.ParallelToolCalls))
	}
	if request.Sampling.MaxTokens != nil {
		out.Set("max_tokens", NewNumber(jsNumber(*request.Sampling.MaxTokens)))
	}
	if request.Sampling.Temperature != nil {
		out.Set("temperature", NewNumber(jsNumber(*request.Sampling.Temperature)))
	}
	if request.Sampling.TopP != nil {
		out.Set("top_p", NewNumber(jsNumber(*request.Sampling.TopP)))
	}
	if len(request.Sampling.Stop) > 0 {
		values := make([]*Value, 0, len(request.Sampling.Stop))
		for _, stop := range request.Sampling.Stop {
			values = append(values, NewString(stop))
		}
		out.Set("stop", NewArray(values...))
	}
	if request.Sampling.Seed != nil {
		out.Set("seed", NewNumber(jsNumber(*request.Sampling.Seed)))
	}
	if request.Sampling.TopK != nil {
		loss.Dropped(LossTopK, direction, "chat_has_no_top_k")
	}
	if request.Reasoning != nil {
		// 等级优先原值搬运；客户端只给预算时按阈值反查降级（否则预算会在 chat 线上整个丢失）。
		if level, derived, ok := effectiveThinkingEffort(request.Reasoning); ok {
			out.Set("reasoning_effort", NewString(level))
			if derived {
				loss.Downgraded(LossThinkingDerived, direction,
					thinkingDerivationDetail(*request.Reasoning.BudgetTokens, "reasoning_effort", level))
			}
		}
		if request.Reasoning.HasSummary && request.Reasoning.Summary != "" {
			// Chat 线无 reasoning summary 载体（与 Node 同类别、同处置）。
			loss.Dropped(LossUnknownField, direction, "reasoning.summary")
		}
	}
	return EncodeResult{Body: out, Loss: loss.Report()}
}

// ---------------------------------------------------------------------------
// 响应编解码
// ---------------------------------------------------------------------------

func decodeChatResponse(body *Value, ctx ConvertCtx) DecodeResult[*Response] {
	loss := &LossCollector{}
	const direction = "response"
	source := body
	if !isRecord(source) {
		source = NewObject()
	}
	id, _ := stringField(source, "id")
	response := &Response{
		ID:          id,
		Model:       firstString(stringOrEmpty(source, "model"), ctx.Model),
		Blocks:      []Block{},
		StopReason:  StopUnknown,
		Passthrough: map[WireProtocol]*Value{},
	}

	choices, present := source.Get("choices")
	switch {
	case present && choices.IsArray():
		items := choices.Items()
		if len(items) == 0 || !isRecord(items[0]) {
			loss.Dropped(LossUnknownField, direction, "choices[0]")
		} else {
			choice := items[0]
			message := fieldOrNil(choice, "message")
			if isRecord(message) {
				response.Blocks = decodeChatMessageBlocks(message, "0", loss, direction, ctx.FromWireToolName)
			} else {
				loss.Dropped(LossUnknownField, direction, "choices[0].message")
			}
			if finishReason, ok := stringField(choice, "finish_reason"); ok {
				response.StopReason = stopReasonFromOpenAIChat(finishReason, true)
			}
			for _, member := range choice.Members() {
				if member.Key == "index" || member.Key == "finish_reason" || member.Key == "message" {
					continue
				}
				if member.Value.IsNull() {
					continue
				}
				loss.Dropped(LossUnknownField, direction, "choices[0]."+member.Key)
			}
		}
		if len(items) > 1 {
			loss.Dropped(LossUnknownField, direction, "choices[1..]")
		}
	case present:
		loss.Dropped(LossUnknownField, direction, "choices")
	default:
		loss.Dropped(LossUnknownField, direction, "choices")
	}

	if usageValue, hasUsage := source.Get("usage"); hasUsage && !usageValue.IsNull() {
		if usage := usageFromOpenAIChat(usageValue); !usage.IsEmpty() {
			response.Usage = usage
		}
	}
	if passthrough := collectPassthrough(source, chatKnownResponseKeys); len(passthrough.Members()) > 0 {
		response.Passthrough[chatWire] = passthrough
	}
	return DecodeResult[*Response]{Value: response, Loss: loss.Report()}
}

func encodeChatResponse(response *Response, ctx ConvertCtx) EncodeResult {
	loss := &LossCollector{}
	const direction = "response"
	idMap := map[string]string{}

	out := cloneOrNewObject(responsePassthroughFor(response, chatWire))
	out.Set("id", NewString(response.ID))
	out.Set("object", NewString("chat.completion"))
	out.Set("model", NewString(firstString(response.Model, ctx.Model)))

	rendered := chatRenderMessages("assistant", response.Blocks, &chatRenderOptions{
		direction: direction, seed: "0", idMap: idMap, loss: loss,
	})
	if len(rendered) > 1 {
		loss.Dropped(LossUnknownField, direction, "message.extra")
	}
	message := NewObject().
		Set("role", NewString("assistant")).
		Set("content", NewNull())
	if len(rendered) > 0 {
		message = rendered[0]
	}
	choice := NewObject().
		Set("index", NewNumber("0")).
		Set("message", message).
		Set("finish_reason", NewString(stopReasonToOpenAIChat(response.StopReason, containsToolCall(response.Blocks))))
	out.Set("choices", NewArray(choice))

	if response.Usage != nil && !response.Usage.IsEmpty() {
		out.Set("usage", usageToOpenAIChat(response.Usage))
	}
	return EncodeResult{Body: out, Loss: loss.Report()}
}

// NewValueString 由 Go 字符串构造 JSON 字符串值。
func NewValueString(value string) *Value { return NewString(value) }

func stringOrEmpty(object *Value, key string) string {
	text, _ := stringField(object, key)
	return text
}

// itoa 是 strconv.Itoa 的别名，避免在多个文件重复导入 strconv。
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	buf := [20]byte{}
	pos := len(buf)
	for value > 0 {
		pos--
		buf[pos] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
