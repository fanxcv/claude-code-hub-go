package convert

import (
	"math"
	"strconv"
	"strings"
)

// BlockKind 是枢纽块类型。
type BlockKind string

const (
	BlockText     BlockKind = "text"
	BlockImage    BlockKind = "image"
	BlockDocument BlockKind = "document"
	BlockToolCall BlockKind = "tool_call"
	BlockThinking BlockKind = "thinking"
	BlockOpaque   BlockKind = "opaque"
)

// CacheHint 是中立缓存提示；仅 Anthropic 编码器渲染为 cache_control。
type CacheHint struct {
	TTL    string
	HasTTL bool
}

// Block 是枢纽块。各类型共用同一结构，按 Kind 取用相应字段。
type Block struct {
	Kind BlockKind

	// text / thinking
	Text string
	// thinking
	Signature string
	Redacted  bool
	// image / document
	MediaType string
	Data      string
	URL       string
	// tool_call
	ID   string
	Name string
	Args string
	// cacheHint（text / image / document 可携带）
	CacheHint *CacheHint
	// opaque
	Wire  WireProtocol
	Value *Value
}

func textBlock(text string) Block { return Block{Kind: BlockText, Text: text} }

func opaqueBlock(wire WireProtocol, value *Value) Block {
	return Block{Kind: BlockOpaque, Wire: wire, Value: value}
}

// containsToolCall 报告块序列是否含工具调用。
func containsToolCall(blocks []Block) bool {
	for _, block := range blocks {
		if block.Kind == BlockToolCall {
			return true
		}
	}
	return false
}

// ItemKind 是枢纽 item 类型。
type ItemKind string

const (
	ItemMessage    ItemKind = "message"
	ItemToolResult ItemKind = "tool_result"
	ItemReasoning  ItemKind = "reasoning"
)

// Item 是统一消息序列单元。
type Item struct {
	Kind       ItemKind
	Role       string // message 专用：user | assistant
	Blocks     []Block
	ToolCallID string // tool_result 专用
	IsError    bool   // tool_result 专用
}

// Tool 是归一化工具定义；CacheHint 仅在 Anthropic 线渲染为 cache_control。
type Tool struct {
	Name        string
	Description string
	HasDescript bool
	Parameters  *Value
	CacheHint   *CacheHint
}

// ToolChoiceKind 是工具选择类型。
type ToolChoiceKind string

const (
	ChoiceAuto     ToolChoiceKind = "auto"
	ChoiceNone     ToolChoiceKind = "none"
	ChoiceRequired ToolChoiceKind = "required"
	ChoiceTool     ToolChoiceKind = "tool"
)

// ToolChoice 是归一化工具选择。
type ToolChoice struct {
	Kind ToolChoiceKind
	Name string
}

// Sampling 是采样参数；指针字段区分「未给」与「给了零值」。
type Sampling struct {
	MaxTokens   *float64
	Temperature *float64
	TopP        *float64
	TopK        *float64
	Stop        []string
	Seed        *float64
}

// Reasoning 是归一化思考配置。
type Reasoning struct {
	Effort       string
	HasEffort    bool
	BudgetTokens *float64
	Summary      string
	HasSummary   bool
}

// Request 是枢纽请求。
type Request struct {
	Model             string
	System            []Block
	Items             []Item
	Tools             []Tool
	ToolChoice        *ToolChoice
	ParallelToolCalls *bool
	Sampling          Sampling
	Reasoning         *Reasoning
	Stream            bool
	Passthrough       map[WireProtocol]*Value
}

// Response 是枢纽响应。
type Response struct {
	ID          string
	Model       string
	Blocks      []Block
	StopReason  StopReason
	Usage       *Usage
	Passthrough map[WireProtocol]*Value
}

// StopReason 是三线归一后的结束原因。
type StopReason string

const (
	StopEndTurn      StopReason = "end_turn"
	StopMaxTokens    StopReason = "max_tokens"
	StopSequence     StopReason = "stop_sequence"
	StopToolUse      StopReason = "tool_use"
	StopContentFilte StopReason = "content_filter"
	StopUnknown      StopReason = "unknown"
)

// Usage 是枢纽口径用量：inputTokens 表示「未命中缓存的新鲜输入」，不含缓存读写。
type Usage struct {
	InputTokens      *float64
	OutputTokens     *float64
	CacheReadTokens  *float64
	CacheWriteTokens *float64
	ReasoningTokens  *float64
}

func (u *Usage) IsEmpty() bool {
	if u == nil {
		return true
	}
	return u.InputTokens == nil && u.OutputTokens == nil && u.CacheReadTokens == nil &&
		u.CacheWriteTokens == nil && u.ReasoningTokens == nil
}

// ConvertCtx 是单次转换的上下文，不可变。
type ConvertCtx struct {
	ClientFormat ClientFormat
	TargetProto  WireProtocol
	Model        string
	Stream       bool
	ProviderID   int64

	// ToWireToolName 把客户端原名规范化为上游可接受形态（编码器写名时用）。
	ToWireToolName func(string) string
	// FromWireToolName 把上游回显的规范化名还原为客户端原名（解码器读名时用）。
	FromWireToolName func(string) string

	// PlaceholderThinkingSignature 是响应侧开关（CCH_THINKING_SIGNATURE_PLACEHOLDER）：
	// 允许给「来自非 Anthropic 上游、没有签名」的思考块补一个占位签名，让 Anthropic 客户端
	// 愿意显示它。只影响响应编码，见 thinking_placeholder.go。
	PlaceholderThinkingSignature bool
}

// shouldPlaceholderThinkingSignature 报告本次渲染是否该补占位签名。
//
// 三个条件缺一不可：开关开、客户端说 Anthropic 协议（否则它不认 thinking 块）、上游不是
// Anthropic 线（原生上游的签名是真的，补占位是失真；且原生配对根本不走编码器）。
func (ctx ConvertCtx) shouldPlaceholderThinkingSignature() bool {
	return ctx.PlaceholderThinkingSignature &&
		ctx.ClientFormat == FormatClaude &&
		ctx.TargetProto != ProtocolAnthropicMessages
}

func (ctx ConvertCtx) toWireName(name string) string {
	if ctx.ToWireToolName == nil {
		return name
	}
	return ctx.ToWireToolName(name)
}

func (ctx ConvertCtx) fromWireName(name string) string {
	if ctx.FromWireToolName == nil {
		return name
	}
	return ctx.FromWireToolName(name)
}

// DecodeResult / EncodeResult 与 TS 侧同名结构对应。
type DecodeResult[T any] struct {
	Value T
	Loss  LossReport
}

type EncodeResult struct {
	Body *Value
	Loss LossReport
}

// ---------------------------------------------------------------------------
// 损失声明
// ---------------------------------------------------------------------------

// LossAction 是损失动作。
type LossAction string

const (
	LossDropped    LossAction = "dropped"
	LossDowngraded LossAction = "downgraded"
	LossRewritten  LossAction = "rewritten"
)

// LossEntry 是一条损失声明。
type LossEntry struct {
	Capability string
	Direction  string
	Action     LossAction
	Detail     string
	HasDetail  bool
}

// LossReport 是损失集合。
type LossReport struct {
	Entries []LossEntry
}

// LossCollector 累积损失。
type LossCollector struct {
	entries []LossEntry
}

func (c *LossCollector) add(capability, direction string, action LossAction, detail string) {
	c.entries = append(c.entries, LossEntry{
		Capability: capability,
		Direction:  direction,
		Action:     action,
		Detail:     detail,
		HasDetail:  true,
	})
}

func (c *LossCollector) Dropped(capability, direction, detail string) {
	c.add(capability, direction, LossDropped, detail)
}

func (c *LossCollector) Downgraded(capability, direction, detail string) {
	c.add(capability, direction, LossDowngraded, detail)
}

func (c *LossCollector) Rewritten(capability, direction, detail string) {
	c.add(capability, direction, LossRewritten, detail)
}

func (c *LossCollector) Report() LossReport {
	if len(c.entries) == 0 {
		return LossReport{}
	}
	out := make([]LossEntry, len(c.entries))
	copy(out, c.entries)
	return LossReport{Entries: out}
}

// 能力标识常量，与 TS 侧 LOSS 对齐。
const (
	LossThinkingSignature   = "thinking.signature"
	LossThinkingBlock       = "thinking.block"
	LossCacheControl        = "cache_control"
	LossDocument            = "document"
	LossImage               = "image"
	LossToolResultIsError   = "tool_result.is_error"
	LossTopK                = "top_k"
	LossMaxTokensDefaulted  = "max_tokens.defaulted"
	LossStopSequences       = "stop_sequences"
	LossToolCallIDRewritten = "tool_call.id.rewritten"
	LossUnknownField        = "unknown_field"
	LossReasoningReplay     = "reasoning.replay"

	// LossAssistantContentEmpty 是「assistant 消息既无正文也无 tool_calls」这一畸形出站形状的
	// 专用归因类别。两类反例把它钉死：
	//   - 严格上游把 content 为空串也判为缺失，实测报 `Invalid assistant message: content or
	//     tool_calls`（OpenCode 系「Console Go」）；
	//   - 另一类 chat 上游（thinking 模式）反向要求历史 assistant 轮次必须带回思考，缺失报
	//     `reasoning_content must be passed back`。
	// 两条要求互斥，编码器因此「既写 content 又保留 reasoning_content」或「整条不产出」；
	// 两条路都记本类别，便于从 LossReport 直接定位，而不是表现为上游 400。
	LossAssistantContentEmpty = "assistant.content.empty"

	// 以下三个是**非 function 工具**（MCP / web_search / local_shell 等）的专用归因类别。
	//
	// 为何单独设类：目标线无承载位时会走 opaque 分支，旧实现一律记 `unknown_field`——
	// 从 LossReport 无法区分「MCP 调用丢了」与「任意未知字段丢了」，排查 MCP / skills 类
	// 问题无从下嘴（Node 侧同样只记 `unknown_field`，属两侧共有的可观测缺口）。
	// 类别用于聚合，具体类型（如 `mcp_call`）进 detail 用于定位。
	LossMCPTool         = "mcp.tool"
	LossWebSearchTool   = "web_search.tool"
	LossNonFunctionTool = "tool.non_function"

	// LossThinkingDerived 记「思考强度载体换算」（budget_tokens ↔ 等级）。
	//
	// 为何需要：三条线的强度载体不同——Anthropic 以 `thinking.budget_tokens`（token 数）为主、
	// 兼有 `output_config.effort`；Chat / Responses 只有等级（`reasoning_effort` / `reasoning.effort`）。
	// 客户端只给预算而目标是只有等级的那两条线时，旧实现直接丢弃——用户表现为「思考强度整个消失」。
	// 现按阈值反查降级为等级并记本类损失（detail 形如 `budget_tokens=8192→effort=high`）。
	LossThinkingDerived = "thinking.derived"

	// 以下四个是**请求侧高层字段跨线丢弃**的专用归因类别（详见 stateful.go）。
	//
	// 为何单独设类：这些字段此前落进 passthrough 逃生舱后再没被任何编码器读过（request 侧
	// passthrough 无消费者），于是 Node 与本仓都表现为**静默丢弃**——客户端拿到一个合法的
	// 回答，却不知道缓存路由键与结构化输出约束早已失效。类别用于聚合，键名进 detail 用于定位。
	// store 记的是 `store:false`（等价于「不额外落库」的默认语义，可降级继续）；`store:true`
	// 与其它状态型字段不走记损，而由 forward 侧 fail-closed（见 convert.StatefulConversionConflict）。
	LossPromptCacheKey = "prompt_cache_key"
	LossResponseFormat = "response_format"
	LossTextControls   = "text.controls"
	LossStoreFlag      = "store"
)

// nonFunctionToolLossClass 把「目标线无法承载的非 function 工具/工具项」归到专用损失类别。
// reportForeignPreservedTools 把「留在外线 passthrough 里、目标线无法承载的工具定义」记入损失。
//
// 为何需要：各编码器只读本线 passthrough（Node 亦然），所以 responses 客户端声明的非 function
// 工具（web_search / mcp）在编码到 chat / anthropic 时会被**静默丢掉**——Node 没有任何记录，
// 报告里看不到「MCP 工具声明没了」。本函数把它变成可聚合、可定位的损失（超出 Node 的可观测性）。
func reportForeignPreservedTools(request *Request, target WireProtocol, loss *LossCollector, direction string) {
	if request == nil || loss == nil {
		return
	}
	for wire, fields := range request.Passthrough {
		if wire == target || fields == nil {
			continue
		}
		raw, present := fields.Get("tools")
		if !present || raw == nil || !raw.IsArray() {
			continue
		}
		for _, entry := range raw.Items() {
			class, detail := nonFunctionToolLossClass(wire, entry)
			loss.Dropped(class, direction, detail)
		}
	}
}

func nonFunctionToolLossClass(wire WireProtocol, value *Value) (string, string) {
	typeName := ""
	if isRecord(value) {
		typeName, _ = stringField(value, "type")
	}
	switch typeName {
	case "mcp_call", "mcp_list_tools", "mcp_approval_request", "mcp":
		return LossMCPTool, typeName
	case "web_search_call", "web_search":
		return LossWebSearchTool, typeName
	case "":
		return LossNonFunctionTool, "opaque:" + string(wire)
	default:
		return LossNonFunctionTool, typeName
	}
}

// ---------------------------------------------------------------------------
// 工具名 / 工具调用 id 规范化
// ---------------------------------------------------------------------------

const toolNameMaxLength = 64

func isSafeToolName(name string) bool {
	if len(name) == 0 || len(name) > toolNameMaxLength {
		return false
	}
	for i := 0; i < len(name); i++ {
		if !isSafeToolNameByte(name[i]) {
			return false
		}
	}
	return true
}

func isSafeToolNameByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9', b == '_', b == '-':
		return true
	default:
		return false
	}
}

// shortHash 是 FNV-1a 32bit 的十六进制表示（8 位，零填充），与 TS 侧逐位一致。
func shortHash(input string) string {
	hash := uint32(0x811c9dc5)
	for _, r := range []uint16(utf16CodeUnits(input)) {
		hash ^= uint32(r)
		hash *= 0x01000193
	}
	return padHex8(hash)
}

func padHex8(value uint32) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 8)
	for i := 7; i >= 0; i-- {
		out[i] = digits[value&0xf]
		value >>= 4
	}
	return string(out)
}

// utf16CodeUnits 按 JS string 的 UTF-16 码元序列取值（charCodeAt 语义）。
func utf16CodeUnits(input string) []uint16 {
	units := make([]uint16, 0, len(input))
	for _, r := range input {
		if r > 0xffff {
			high, low := utf16SurrogatePair(r)
			units = append(units, high, low)
			continue
		}
		units = append(units, uint16(r))
	}
	return units
}

func utf16SurrogatePair(r rune) (uint16, uint16) {
	value := uint32(r) - 0x10000
	return uint16(0xd800 + (value >> 10)), uint16(0xdc00 + (value & 0x3ff))
}

// NormalizeToolName 把任意工具名规范化为官方接口可接受的形态；已合法时原样返回。
func NormalizeToolName(name string) string {
	if isSafeToolName(name) {
		return name
	}
	sanitized := sanitizeToolToken(name)
	suffix := "_" + shortHash(name)
	budget := toolNameMaxLength - len(suffix)
	prefixLength := budget
	if prefixLength < 1 {
		prefixLength = 1
	}
	prefix := sanitized
	if len(prefix) > prefixLength {
		prefix = prefix[:prefixLength]
	}
	if len(prefix) > 0 {
		return prefix + suffix
	}
	return "tool" + suffix
}

func sanitizeToolToken(value string) string {
	out := make([]byte, 0, len(value))
	for i := 0; i < len(value); i++ {
		if isSafeToolNameByte(value[i]) {
			out = append(out, value[i])
			continue
		}
		out = append(out, '_')
	}
	return string(out)
}

// NormalizeToolCallID 把任意 id 规范化为 Anthropic 可接受形态；已合法时原样返回。
func NormalizeToolCallID(id string) string {
	if isSafeToolName(id) {
		return id
	}
	sanitized := sanitizeToolToken(id)
	suffix := "_" + shortHash(id)
	budget := toolNameMaxLength - len(suffix)
	prefixLength := budget
	if prefixLength < 1 {
		prefixLength = 1
	}
	prefix := sanitized
	if len(prefix) > prefixLength {
		prefix = prefix[:prefixLength]
	}
	if len(prefix) > 0 {
		return prefix + suffix
	}
	return "tool" + suffix
}

// MakeToolCallID 为缺失 id 的工具调用合成稳定 id。
func MakeToolCallID(seed string) string { return "call_" + shortHash(seed) }

// BuildToolNameRestoreMap 由一组原始工具名构建「规范化名 -> 原名」逆转表。
func BuildToolNameRestoreMap(originalNames []string) map[string]string {
	restore := map[string]string{}
	for _, original := range originalNames {
		safe := NormalizeToolName(original)
		if safe != original {
			restore[safe] = original
		}
	}
	return restore
}

// ---------------------------------------------------------------------------
// 结束原因三向映射
// ---------------------------------------------------------------------------

func StopReasonFromAnthropic(raw string, has bool) StopReason {
	if !has {
		return StopUnknown
	}
	switch raw {
	case "end_turn":
		return StopEndTurn
	case "max_tokens", "model_context_window_exceeded":
		return StopMaxTokens
	case "stop_sequence":
		return StopSequence
	case "tool_use":
		return StopToolUse
	case "refusal":
		return StopContentFilte
	case "pause_turn":
		return StopEndTurn
	default:
		return StopUnknown
	}
}

func stopReasonFromOpenAIChat(raw string, has bool) StopReason {
	if !has {
		return StopUnknown
	}
	switch raw {
	case "stop":
		return StopEndTurn
	case "length":
		return StopMaxTokens
	case "tool_calls", "function_call":
		return StopToolUse
	case "content_filter":
		return StopContentFilte
	default:
		return StopUnknown
	}
}

func stopReasonFromResponses(status string, hasStatus bool, incompleteReason string) StopReason {
	if !hasStatus {
		return StopUnknown
	}
	switch status {
	case "completed":
		return StopEndTurn
	case "incomplete":
		switch incompleteReason {
		case "max_output_tokens":
			return StopMaxTokens
		case "content_filter":
			return StopContentFilte
		default:
			return StopUnknown
		}
	default:
		return StopUnknown
	}
}

func stopReasonToAnthropic(reason StopReason) string {
	switch reason {
	case StopEndTurn:
		return "end_turn"
	case StopMaxTokens:
		return "max_tokens"
	case StopSequence:
		return "stop_sequence"
	case StopToolUse:
		return "tool_use"
	case StopContentFilte:
		return "refusal"
	default:
		return "end_turn"
	}
}

func stopReasonToOpenAIChat(reason StopReason, hasToolCalls bool) string {
	if hasToolCalls || reason == StopToolUse {
		return "tool_calls"
	}
	switch reason {
	case StopEndTurn, StopSequence:
		return "stop"
	case StopMaxTokens:
		return "length"
	case StopContentFilte:
		return "content_filter"
	default:
		return "stop"
	}
}

// responsesStatus 是 Responses 的 status / incomplete_details 组合。
type responsesStatus struct {
	Status            string
	IncompleteDetails *Value
}

func stopReasonToResponses(reason StopReason, toolChoiceRequired bool) responsesStatus {
	if reason == StopMaxTokens {
		if toolChoiceRequired {
			return responsesStatus{Status: "completed"}
		}
		return responsesStatus{
			Status:            "incomplete",
			IncompleteDetails: NewObject().Set("reason", NewString("max_output_tokens")),
		}
	}
	if reason == StopContentFilte {
		return responsesStatus{
			Status:            "incomplete",
			IncompleteDetails: NewObject().Set("reason", NewString("content_filter")),
		}
	}
	return responsesStatus{Status: "completed"}
}

// ---------------------------------------------------------------------------
// usage 三向映射
// ---------------------------------------------------------------------------

func numberOrNil(value *Value) (*float64, bool) {
	if value == nil {
		return nil, false
	}
	parsed, ok := value.Float64()
	if !ok {
		return nil, false
	}
	return &parsed, true
}

// usageFromAnthropic 解析 Anthropic usage。
func usageFromAnthropic(raw *Value) *Usage {
	usage := &Usage{}
	if raw == nil || !raw.IsObject() {
		return usage
	}
	usage.InputTokens, _ = numberOrNil(fieldOrNil(raw, "input_tokens"))
	usage.OutputTokens, _ = numberOrNil(fieldOrNil(raw, "output_tokens"))
	usage.CacheReadTokens, _ = numberOrNil(fieldOrNil(raw, "cache_read_input_tokens"))
	usage.CacheWriteTokens, _ = numberOrNil(fieldOrNil(raw, "cache_creation_input_tokens"))
	return usage
}

// usageFromOpenAIChat 解析 Chat usage，并把 cached 从 prompt 中减去以归一到枢纽口径。
func usageFromOpenAIChat(raw *Value) *Usage {
	usage := &Usage{}
	if raw == nil || !raw.IsObject() {
		return usage
	}
	prompt, hasPrompt := numberOrNil(fieldOrNil(raw, "prompt_tokens"))
	completion, _ := numberOrNil(fieldOrNil(raw, "completion_tokens"))
	cached, hasCached := numberOrNil(fieldOrNil(nestedField(raw, "prompt_tokens_details", "cached_tokens")))
	reasoning, _ := numberOrNil(fieldOrNil(raw, "completion_tokens_details", "reasoning_tokens"))
	if hasPrompt {
		value := *prompt
		if hasCached {
			value -= *cached
		}
		if value < 0 {
			value = 0
		}
		usage.InputTokens = &value
	}
	usage.OutputTokens = completion
	usage.CacheReadTokens = cached
	usage.ReasoningTokens = reasoning
	return usage
}

// usageFromResponses 解析 Responses usage，归一方式与 Chat 一致。
func usageFromResponses(raw *Value) *Usage {
	usage := &Usage{}
	if raw == nil || !raw.IsObject() {
		return usage
	}
	input, hasInput := numberOrNil(fieldOrNil(raw, "input_tokens"))
	output, _ := numberOrNil(fieldOrNil(raw, "output_tokens"))
	cached, hasCached := numberOrNil(nestedField(raw, "input_tokens_details", "cached_tokens"))
	reasoning, _ := numberOrNil(fieldOrNil(raw, "output_tokens_details", "reasoning_tokens"))
	if hasInput {
		value := *input
		if hasCached {
			value -= *cached
		}
		if value < 0 {
			value = 0
		}
		usage.InputTokens = &value
	}
	usage.OutputTokens = output
	usage.CacheReadTokens = cached
	usage.ReasoningTokens = reasoning
	return usage
}

// usageFromGemini 解析 Gemini 的 usageMetadata。
//
// Node 口径（src/app/v1/_lib/proxy/response-handler.ts:6045-6068）：
//   - input = max(promptTokenCount - cachedContentTokenCount, 0)：promptTokenCount **包含**已缓存
//     部分，不减去就与 cache_read 重复计费；
//   - output = candidatesTokenCount；cache_read = cachedContentTokenCount。
//
// reasoning（thoughtsTokenCount）与流式观测器同口径（forward/observe.go 的 Gemini 分支）；
// 落库侧当前只映射 input/output/cache_read，多取一项不会改变计费。
func usageFromGemini(raw *Value) *Usage {
	usage := &Usage{}
	if raw == nil || !raw.IsObject() {
		return usage
	}
	prompt, hasPrompt := numberOrNil(fieldOrNil(raw, "promptTokenCount"))
	output, _ := numberOrNil(fieldOrNil(raw, "candidatesTokenCount"))
	cached, hasCached := numberOrNil(fieldOrNil(raw, "cachedContentTokenCount"))
	reasoning, _ := numberOrNil(fieldOrNil(raw, "thoughtsTokenCount"))
	if hasPrompt {
		value := *prompt
		if hasCached {
			value -= *cached
		}
		if value < 0 {
			value = 0
		}
		usage.InputTokens = &value
	}
	usage.OutputTokens = output
	usage.CacheReadTokens = cached
	usage.ReasoningTokens = reasoning
	return usage
}

func usageToAnthropic(usage *Usage) *Value {
	out := NewObject()
	if usage.InputTokens != nil {
		out.Set("input_tokens", NewNumber(jsNumber(*usage.InputTokens)))
	}
	if usage.OutputTokens != nil {
		out.Set("output_tokens", NewNumber(jsNumber(*usage.OutputTokens)))
	}
	if usage.CacheReadTokens != nil {
		out.Set("cache_read_input_tokens", NewNumber(jsNumber(*usage.CacheReadTokens)))
	}
	if usage.CacheWriteTokens != nil {
		out.Set("cache_creation_input_tokens", NewNumber(jsNumber(*usage.CacheWriteTokens)))
	}
	return out
}

func usageToOpenAIChat(usage *Usage) *Value {
	out := NewObject()
	prompt := numberValue(usage.InputTokens) + numberValue(usage.CacheReadTokens)
	completion := numberValue(usage.OutputTokens)
	out.Set("prompt_tokens", NewNumber(jsNumber(prompt)))
	out.Set("completion_tokens", NewNumber(jsNumber(completion)))
	out.Set("total_tokens", NewNumber(jsNumber(prompt+completion)))
	if usage.CacheReadTokens != nil {
		out.Set("prompt_tokens_details", NewObject().
			Set("cached_tokens", NewNumber(jsNumber(*usage.CacheReadTokens))))
	}
	if usage.ReasoningTokens != nil {
		out.Set("completion_tokens_details", NewObject().
			Set("reasoning_tokens", NewNumber(jsNumber(*usage.ReasoningTokens))))
	}
	return out
}

func usageToResponses(usage *Usage) *Value {
	out := NewObject()
	input := numberValue(usage.InputTokens) + numberValue(usage.CacheReadTokens)
	output := numberValue(usage.OutputTokens)
	out.Set("input_tokens", NewNumber(jsNumber(input)))
	out.Set("output_tokens", NewNumber(jsNumber(output)))
	out.Set("total_tokens", NewNumber(jsNumber(input+output)))
	if usage.CacheReadTokens != nil {
		out.Set("input_tokens_details", NewObject().
			Set("cached_tokens", NewNumber(jsNumber(*usage.CacheReadTokens))))
	}
	if usage.ReasoningTokens != nil {
		out.Set("output_tokens_details", NewObject().
			Set("reasoning_tokens", NewNumber(jsNumber(*usage.ReasoningTokens))))
	}
	return out
}

func numberValue(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}

// ---------------------------------------------------------------------------
// 取字段与数字格式
// ---------------------------------------------------------------------------

func fieldOrNil(object *Value, keys ...string) *Value {
	current := object
	for _, key := range keys {
		if current == nil || !current.IsObject() {
			return nil
		}
		next, ok := current.Get(key)
		if !ok {
			return nil
		}
		current = next
	}
	return current
}

func nestedField(object *Value, parent string, child string) *Value {
	return fieldOrNil(object, parent, child)
}

// stringField 取字符串字段；缺失或非字符串返回 (零值, false)。
func stringField(object *Value, key string) (string, bool) {
	value, ok := object.Get(key)
	if !ok {
		return "", false
	}
	return value.String()
}

// numberField 取数字字段；缺失或非数字返回 (nil, false)。
func numberField(object *Value, key string) (*float64, bool) {
	value, ok := object.Get(key)
	if !ok {
		return nil, false
	}
	return numberOrNil(value)
}

// boolField 取布尔字段。
func boolField(object *Value, key string) (bool, bool) {
	value, ok := object.Get(key)
	if !ok {
		return false, false
	}
	return value.Bool()
}

// firstString 取第一个非空字符串（model 回落、id 回落用）。
func firstString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// jsNumber 以 JS Number → String 的规则格式化浮点值，保证与 JSON.stringify 一致。
func jsNumber(value float64) string {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "null"
	}
	if math.Abs(value) < 1e15 && value == math.Trunc(value) {
		return strconv.FormatFloat(value, 'f', -1, 64)
	}
	abs := math.Abs(value)
	if abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		return trimExponent(strconv.FormatFloat(value, 'e', -1, 64))
	}
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// trimExponent 去掉指数部分的冗余前导零（Go 输出 1e-07，JS 输出 1e-7）。
func trimExponent(text string) string {
	index := strings.IndexAny(text, "eE")
	if index < 0 {
		return text
	}
	mantissa := text[:index]
	exponent := text[index+1:]
	sign := ""
	if len(exponent) > 0 && (exponent[0] == '+' || exponent[0] == '-') {
		sign = string(exponent[0])
		exponent = exponent[1:]
	}
	trimmed := strings.TrimLeft(exponent, "0")
	if trimmed == "" {
		trimmed = "0"
	}
	return mantissa + "e" + sign + trimmed
}

// cloneOrNewObject 复制 passthrough 对象；为空时返回新对象。
func cloneOrNewObject(value *Value) *Value {
	if value == nil || !value.IsObject() {
		return NewObject()
	}
	return value.Clone()
}

// passthroughFor 取指定协议线的逃生舱对象。
func passthroughFor(request *Request, protocol WireProtocol) *Value {
	if request == nil || request.Passthrough == nil {
		return nil
	}
	return request.Passthrough[protocol]
}

func responsePassthroughFor(response *Response, protocol WireProtocol) *Value {
	if response == nil || response.Passthrough == nil {
		return nil
	}
	return response.Passthrough[protocol]
}

// knownKeysContains / collectPassthrough 收集未知字段（逃生舱）。
type knownKeys map[string]bool

func (k knownKeys) contains(key string) bool { return k[key] }

func collectPassthrough(source *Value, known knownKeys) *Value {
	out := NewObject()
	for _, member := range source.Members() {
		if known.contains(member.Key) {
			continue
		}
		if member.Value.IsNull() && member.Value.Kind() == "null" {
			// TS 侧 undefined 的字段在 JSON 里不存在；null 会被保留（与 Object.keys 一致）。
		}
		out.Set(member.Key, member.Value)
	}
	return out
}

// isRecord 判断是否为 JSON 对象。
func isRecord(value *Value) bool { return value != nil && value.IsObject() }

// asUnknownArray 非数组返回空切片。
func asUnknownArray(value *Value) []*Value {
	if value == nil || !value.IsArray() {
		return nil
	}
	return value.Items()
}

// isMeaningful 判定「有实际内容」：undefined / null / 空串 / 空数组 / 空对象 视为无内容。
func isMeaningful(value *Value) bool {
	if value == nil || value.IsNull() {
		return false
	}
	switch value.Kind() {
	case "string":
		text, _ := value.String()
		return text != ""
	case "array":
		return value.Len() > 0
	case "object":
		return len(value.Members()) > 0
	default:
		return true
	}
}

// meaningfulExtras 取除已知键外有实际内容的键名。
func meaningfulExtras(record *Value, known ...string) []string {
	knownSet := knownKeys{}
	for _, key := range known {
		knownSet[key] = true
	}
	out := []string{}
	for _, member := range record.Members() {
		if knownSet.contains(member.Key) {
			continue
		}
		if isMeaningful(member.Value) {
			out = append(out, member.Key)
		}
	}
	return out
}
