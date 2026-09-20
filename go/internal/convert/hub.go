package convert

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
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
	// FromDataURL 报告「客户端的这幅图是用 data URL 表达的、解码时归一成了 base64」。
	//
	// 为何只留事实、不在解码处记损：同一张逻辑图的最终去向只有编码侧知道——目标线可能
	// 整块丢掉它（chat 不收 GIF、responses 不收 assistant 图）。两处都记就是一张图两条台账，
	// 生产实测把 image 计数抬成实际值的两倍。故解码侧只置此位，编码侧按最终结果记一条：
	// 送达则 rewritten，被丢则只记 dropped（见 chatImageBlockToPart / responsesImagePart /
	// encodeAnthropicBlock）。
	FromDataURL bool
	// tool_call
	ID   string
	Name string
	Args string
	// cacheHint（text / image / document / tool_call 可携带）
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
	// CacheHint 是 tool_result 块自身的缓存提示（tool_result 专用）。
	CacheHint *CacheHint
}

// Tool 是归一化工具定义；CacheHint 仅在 Anthropic 线渲染为 cache_control。
type Tool struct {
	Name        string
	Description string
	HasDescript bool
	Parameters  *Value
	CacheHint   *CacheHint
	// Strict 是客户端声明的「上游须按 schema 严格校验参数」（responses / chat 两线的 tools[].strict）。
	//
	// 为何只在 true 时对外写、且只在该值为 true 时记损：OpenAI 两线 `strict` 缺省即 false，
	// 而 Codex 实测给每个 function 工具都显式写 `strict:false`——把它当损失记进台账，等于
	// 给每个请求凭空添 N 条「改写」（实测 10 个工具 = 10 条），真损失反被淹没。`strict:true`
	// 则是有约束力的声明，目标线无承载位时必须记损（见各编码器）。
	Strict    bool
	HasStrict bool
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
	// PromptCacheKey 是客户端的缓存路由键（OpenAI 两线同名）。
	//
	// 为何要进枢纽：它此前只活在 passthrough 里，而 chat 编码器只读本线 passthrough，于是
	// responses→chat 一律丢弃——偏偏守卫链为了让供应商命中前缀缓存，会**主动**给 Codex
	// 会话补这个字段（guard.completeCodexSession），补完在下游被丢掉等于白补。现由 chat
	// 编码器原样写出，anthropic（无此概念）仍记 info 档损失。
	PromptCacheKey    string
	HasPromptCacheKey bool
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

	// ToWireToolName 把客户端原名规范化为上游可接受形态（编码器写名时用）。
	ToWireToolName func(string) string
	// FromWireToolName 把上游回显的规范化名还原为客户端原名（解码器读名时用）。
	FromWireToolName func(string) string

	// PlaceholderThinkingSignature 是响应侧开关（CCH_THINKING_SIGNATURE_PLACEHOLDER）：
	// 允许给「来自非 Anthropic 上游、没有签名」的思考块补一个占位签名，让 Anthropic 客户端
	// 愿意显示它。只影响响应编码，见 thinking_placeholder.go。
	PlaceholderThinkingSignature bool

	// GatewayInjectedBodyFields 是**网关注入**（客户端原文里没有）的正文顶层字段名。
	//
	// 为何需要：Codex 客户端用 `session_id` 头表达会话身份时，守卫链会把 `prompt_cache_key`
	// 补进正文（见 guard.completeCodexSession），而跨线转换时它会被当成「客户端声明的
	// 缓存路由键」记入损失——客户端根本没提过这个字段，于是**每一个转换请求都凭空多一条**
	// （生产实测：每一行 +1）。判据只能是「客户端原文里是否出现」，而字段是否被注入
	// 只有守卫链知道，故由它把事实传到这里。
	GatewayInjectedBodyFields []string
}

// isGatewayInjectedField 报告某个正文顶层字段名是否由网关注入（而非客户端给出）。
func (ctx ConvertCtx) isGatewayInjectedField(name string) bool {
	for _, injected := range ctx.GatewayInjectedBodyFields {
		if injected == name {
			return true
		}
	}
	return false
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
	// catch-all 细分在这里收口：`unknown_field` 的记损站点有八十余处，逐个改名既改不动也
	// 容易漏，而 detail 的首词就是来源（各站点传的是字段路径），故在唯一的累积入口按前缀
	// 归类（见 unknownFieldCapability）。
	if capability == LossUnknownField {
		capability = unknownFieldCapability(detail)
	}
	c.entries = append(c.entries, LossEntry{
		Capability: capability,
		Direction:  direction,
		Action:     action,
		Detail:     detail,
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

	// LossReasoningSummary 是请求侧 `reasoning.summary` 在无承载位目标线上的丢弃。
	//
	// 为何单独设类而不是继续走 catch-all：Codex 每个请求都带 `reasoning.summary:"auto"`，
	// 它是「要不要把思考摘要回传给我」的显示偏好，与本次作答无关（上游照旧回 reasoning_content，
	// 我方再回译成 responses 的 reasoning 项）。按 catch-all 记成改写档的后果是：生产上几乎每行
	// 都挂一枚「内容改写」徽章（实测 1228/1238 行），真损失被淹没。故单列并降到 info 档。
	LossReasoningSummary = "reasoning.summary"

	// LossThinkingEncrypted 是「加密思考块（OpenAI 的 encrypted_content / 红化思考）被丢」的专用类别。
	//
	// 为何与 thinking.block 分开：thinking.block 被丢的是**可读的思考内容**（真损失，rewrite 档）；
	// 这里丢的是目标线根本解不开的密文 blob（chat / anthropic 无承载位），密文本身不构成内容，
	// 只是「无法续接上游自己的思考」这一保真弱化。两者混在一格会让 rewrite 档被密文条数抬高。
	LossThinkingEncrypted = "thinking.encrypted"

	// LossToolStrict 是「客户端要求严格 schema 校验（tools[].strict=true），目标线无承载位」的类别。
	//
	// 为何不是 unknown_field.tool：`strict:true` 是**有约束力**的声明，丢了意味着上游可能返回不合
	// schema 的参数；单列才能与「未知工具成员」区分。`strict:false`（Codex 默认写法）与本字段缺省
	// 等价，一律不记损——记了就是每请求 N 条噪声。
	LossToolStrict = "tool.strict"
)

// 以下七个是 catch-all `unknown_field` 的细分类别（按 detail 的来源归类）。
//
// 为何要细分：`unknown_field` 是生产报告里第二大类（仅次于 thinking.block 降级，约占 9.7%），
// 但它把「工具声明丢了」「消息成员丢了」「采样参数丢了」混成一格——界面上看不出是什么，
// 也无法按来源过滤，跟「任意未知字段丢了」完全不可区分。前缀统一为 `unknown_field.`，
// 故“是否属于 catch-all 家族”可由前缀判定（服务端与界面两侧同一判据）。
const (
	LossUnknownFieldTool      = "unknown_field.tool"
	LossUnknownFieldRefusal   = "unknown_field.refusal"
	LossUnknownFieldContent   = "unknown_field.content"
	LossUnknownFieldMessage   = "unknown_field.message"
	LossUnknownFieldParam     = "unknown_field.param"
	LossUnknownFieldStructure = "unknown_field.structure"
	// LossUnknownFieldOther 是未能归类的兜底子类：新增记损站点若引入新前缀，落这里而不是消失。
	LossUnknownFieldOther = "unknown_field.other"
)

// unknownFieldRule 是一条「detail 前缀 → 子类别」规则。
//
// 顺序即优先级：更长的前缀必须排在更短的前面（`input_image…` 必须先于 `input…`），
// 否则后者会把前者的来源吞掉。
var unknownFieldRules = []unknownFieldRule{
	{prefix: "tool", capability: LossUnknownFieldTool},
	{prefix: "refusal", capability: LossUnknownFieldRefusal},
	{prefix: "content", capability: LossUnknownFieldContent},
	{prefix: "image_url", capability: LossUnknownFieldContent},
	{prefix: "input_image", capability: LossUnknownFieldContent},
	{prefix: "block", capability: LossUnknownFieldContent},
	{prefix: "choices", capability: LossUnknownFieldContent},
	{prefix: "message", capability: LossUnknownFieldMessage},
	{prefix: "reasoning", capability: LossUnknownFieldParam},
	{prefix: "thinking", capability: LossUnknownFieldParam},
	{prefix: "output_config", capability: LossUnknownFieldParam},
	{prefix: "seed", capability: LossUnknownFieldParam},
	{prefix: "max_tokens", capability: LossUnknownFieldParam},
	{prefix: "stop", capability: LossUnknownFieldParam},
	{prefix: "parallel_tool_calls", capability: LossUnknownFieldParam},
	{prefix: "instructions", capability: LossUnknownFieldStructure},
	{prefix: "output", capability: LossUnknownFieldStructure},
	{prefix: "input", capability: LossUnknownFieldStructure},
	{prefix: "opaque", capability: LossUnknownFieldStructure},
}

type unknownFieldRule struct {
	prefix     string
	capability string
}

// unknownFieldCapability 把 catch-all 的 `unknown_field` 按 detail 首词归到子类别。
//
// 判据只用 detail 的前缀：各站点传进来的 detail 就是字段路径（`tools[].name`、
// `content_part.image_url`、`messages[3]`…），故这一层无需站点配合；新增站点只要前缀已在
// 规则表内就自动归类，否则落 `unknown_field.other`（可见地暴露“忘了归类”）。
func unknownFieldCapability(detail string) string {
	for _, rule := range unknownFieldRules {
		if strings.HasPrefix(detail, rule.prefix) {
			return rule.capability
		}
	}
	return LossUnknownFieldOther
}

// LossSeverity 是损失条目的档位：为「界面上哪些损失值得一眼看到」提供权威口径。
//
// 分档判据是**对本次作答的影响**，而不是“丢了多少条”（计数大不等于影响大：一轮思考降级
// 只弱化保真度，而一个被丢的 top_k 会直接改模型行为）：
//   - rewrite：内容/工具被改写或删除，或客户端**显式给定**的参数被丢弃——上游看到的东西变了；
//   - degrade：能力仍在但保真度弱化（思考强度、思考签名、缓存提示）；
//   - info：字段不承载约束（默认语义、缓存路由键），丢失后本次作答完全不变。
//
// 为什么档位定在服务端而不是界面侧：add 的同一份判据还要给日志、回放与后续消费者用，
// 界面自建一张表就是第二份真源，迟早分叉。
type LossSeverity string

const (
	SeverityRewrite LossSeverity = "rewrite"
	SeverityDegrade LossSeverity = "degrade"
	SeverityInfo    LossSeverity = "info"
)

// lossSeverityByCapability 是「能力 → 档位」的权威表；**未列出的能力一律 rewrite**。
//
// 为何写成表而不是 switch：界面侧要为库里**历史**条目（没有 severity 字段）推导档位，而档位
// 真源在这里；表可以由 `go/cmd/lossseverity` 原样渲染成 TS（见 loss_severity_gen.go），
// switch 不行——手工抄一份表迟早分叉（2026-09-16 实证：一侧按族前缀、一侧按精确名，未列出的
// 新名两侧判档不同，徽章整枚不画，真损失被降噪吞掉）。
//
// 新增非 rewrite 档能力只改这张表，随后按 TestLossSeverityGeneratedTableIsUpToDate 的提示
// 重新生成界面侧的表即可。
var lossSeverityByCapability = map[string]LossSeverity{
	LossStoreFlag:         SeverityInfo,
	LossPromptCacheKey:    SeverityInfo,
	LossReasoningSummary:  SeverityInfo,
	LossThinkingSignature: SeverityDegrade,
	LossThinkingDerived:   SeverityDegrade,
	LossReasoningReplay:   SeverityDegrade,
	LossCacheControl:      SeverityDegrade,
	LossThinkingEncrypted: SeverityDegrade,
	LossToolStrict:        SeverityDegrade,
}

// lossSeverityByCapabilityAction 是「同一能力按动作分档」的例外表。
//
// 为何要连 action 一起看：thinking.block 是同一能力的两种事实——块被**丢**（客户端的思考
// 内容消失）与被**降级**（强度载体换算），前者改变内容、后者只弱化保真度。
// 表中未列出的动作退回 lossSeverityByCapability，仍无命中则 rewrite。
var lossSeverityByCapabilityAction = map[string]map[LossAction]LossSeverity{
	LossThinkingBlock: {LossDowngraded: SeverityDegrade},
}

// LossSeverityOf 报告一条损失（capability + action）的档位。
//
// 未知 capability 一律按 rewrite 处理：宁可让它显眼，也不要让新能力默默躺在折叠区里。
func LossSeverityOf(capability string, action LossAction) LossSeverity {
	if byAction, ok := lossSeverityByCapabilityAction[capability]; ok {
		if severity, ok := byAction[action]; ok {
			return severity
		}
	}
	if severity, ok := lossSeverityByCapability[capability]; ok {
		return severity
	}
	return SeverityRewrite
}

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
//
// 用 utf16.Encode 而非按 rune 迭代：JS 的 charCodeAt 按 UTF-16 码元计数，非 BMP 字符
// 要拆成代理对，否则哈希与 TS 分叉。
func shortHash(input string) string {
	hash := uint32(0x811c9dc5)
	for _, unit := range utf16.Encode([]rune(input)) {
		hash ^= uint32(unit)
		hash *= 0x01000193
	}
	return fmt.Sprintf("%08x", hash)
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
//
// 与 NormalizeToolName 同一形态规则，故直接委托，避免两份实现漂移。
func NormalizeToolCallID(id string) string { return NormalizeToolName(id) }

// setTemperatureAndTopP 写出三线共用的采样参数。
//
// 三条线的这两个键名与口径完全一致（temperature / top_p），差别只在其余字段
// （max_tokens 键名、stop 载体、seed/top_k 的支持面），故只抽这一对。
func setTemperatureAndTopP(out *Value, sampling Sampling) {
	if sampling.Temperature != nil {
		out.Set("temperature", NewNumber(jsNumber(*sampling.Temperature)))
	}
	if sampling.TopP != nil {
		out.Set("top_p", NewNumber(jsNumber(*sampling.TopP)))
	}
}

// resolveEmitToolCallID 把工具调用 id 规范化为目标线可接受形态，并把映射记入 idMap。
//
// 三条线的规则同形，只在「id 是否安全」的判据上不同：Anthropic 用工具名判据
// （isSafeToolName），Chat 与 Responses 用 id 判据（chatIsSafeToolID）。故由调用方传入。
func resolveEmitToolCallID(
	original string,
	seed string,
	idMap map[string]string,
	safe func(string) bool,
	loss *LossCollector,
	direction string,
) string {
	raw := original
	if raw != "" {
		if mapped, ok := idMap[raw]; ok {
			return mapped
		}
	}
	emitted := raw
	if raw == "" {
		emitted = MakeToolCallID(seed)
		loss.Rewritten(LossToolCallIDRewritten, direction, "synthesized:"+seed)
	} else if !safe(raw) {
		emitted = NormalizeToolCallID(raw)
		loss.Rewritten(LossToolCallIDRewritten, direction, "sanitized")
	}
	if raw != "" {
		idMap[raw] = emitted
	}
	return emitted
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

// usageSpec 描述一条 usage 线的取键差异；三线共用同一个解析器（见 usageFrom）。
type usageSpec struct {
	// inputKey / outputKey 是输入、输出 token 的键。
	inputKey  string
	outputKey string
	// cachedPath / reasoningPath 是缓存与推理 token 的嵌套路径（父键在前）。
	cachedPath    []string
	reasoningPath []string
}

// usageFrom 按 spec 解析 usage，并把 cached 从 input 中减去以归一到枢纽口径。
//
// 为什么减：各线的 input/prompt 都**包含**已缓存部分，不减就与 cache_read 重复计费。
func usageFrom(raw *Value, spec usageSpec) *Usage {
	usage := &Usage{}
	if raw == nil || !raw.IsObject() {
		return usage
	}
	input, hasInput := numberOrNil(fieldOrNil(raw, spec.inputKey))
	output, _ := numberOrNil(fieldOrNil(raw, spec.outputKey))
	cached, hasCached := numberOrNil(fieldOrNil(raw, spec.cachedPath...))
	reasoning, _ := numberOrNil(fieldOrNil(raw, spec.reasoningPath...))
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

// usageFromOpenAIChat 解析 Chat usage，并把 cached 从 prompt 中减去以归一到枢纽口径。
func usageFromOpenAIChat(raw *Value) *Usage {
	return usageFrom(raw, usageSpec{
		inputKey:      "prompt_tokens",
		outputKey:     "completion_tokens",
		cachedPath:    []string{"prompt_tokens_details", "cached_tokens"},
		reasoningPath: []string{"completion_tokens_details", "reasoning_tokens"},
	})
}

// usageFromResponses 解析 Responses usage，归一方式与 Chat 一致。
func usageFromResponses(raw *Value) *Usage {
	return usageFrom(raw, usageSpec{
		inputKey:      "input_tokens",
		outputKey:     "output_tokens",
		cachedPath:    []string{"input_tokens_details", "cached_tokens"},
		reasoningPath: []string{"output_tokens_details", "reasoning_tokens"},
	})
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
	return usageFrom(raw, usageSpec{
		inputKey:      "promptTokenCount",
		outputKey:     "candidatesTokenCount",
		cachedPath:    []string{"cachedContentTokenCount"},
		reasoningPath: []string{"thoughtsTokenCount"},
	})
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

// usageToOpenAIChat 与 usageToResponses 只在键名上不同，故共用 usageTo。
func usageToOpenAIChat(usage *Usage) *Value { return usageTo(usage, "prompt", "completion") }

func usageToResponses(usage *Usage) *Value { return usageTo(usage, "input", "output") }

// usageTo 按线写出 usage：输入含 cache_read（该线的 input 是含缓存的总额）。
func usageTo(usage *Usage, inputKey string, outputKey string) *Value {
	out := NewObject()
	input := numberValue(usage.InputTokens) + numberValue(usage.CacheReadTokens)
	output := numberValue(usage.OutputTokens)
	out.Set(inputKey+"_tokens", NewNumber(jsNumber(input)))
	out.Set(outputKey+"_tokens", NewNumber(jsNumber(output)))
	out.Set("total_tokens", NewNumber(jsNumber(input+output)))
	if usage.CacheReadTokens != nil {
		out.Set(inputKey+"_tokens_details", NewObject().
			Set("cached_tokens", NewNumber(jsNumber(*usage.CacheReadTokens))))
	}
	if usage.ReasoningTokens != nil {
		out.Set(outputKey+"_tokens_details", NewObject().
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
