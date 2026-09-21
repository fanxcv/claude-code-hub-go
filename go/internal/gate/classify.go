package gate

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
)

// Family 是门控分类所依据的**上游协议家族**。
//
// 它与 egress.Family（前门路由方言）和 convert.WireProtocol（协议线标识）同源但不同用：
// 本包按供应商原生 wire 格式分类上游帧，且该字符串要出现在门控错误体的 family 字段里，
// 因此保留 TS 侧的字面值（"anthropic" 而非 "anthropic-messages"）。
type Family string

const (
	FamilyAnthropic       Family = "anthropic"
	FamilyOpenAIChat      Family = "openai-chat"
	FamilyOpenAIResponses Family = "openai-responses"
	FamilyGemini          Family = "gemini"
)

// Verdict 是单帧的五态判定。
type Verdict string

const (
	// VerdictContent 携带用户可感知内容，可作为「首个有效内容 chunk」开启透传。
	VerdictContent Verdict = "content"
	// VerdictError 是上游错误信号（fake-200 / 流中 error 帧），提交前出现即 failover。
	VerdictError Verdict = "error"
	// VerdictMalformed 表示 data 不是合法 JSON 载荷，立即终止当前 attempt（fail-closed）。
	VerdictMalformed Verdict = "malformed"
	// VerdictTerminal 是干净终止标记（[DONE] / message_stop 等），本身不开启透传。
	VerdictTerminal Verdict = "terminal"
	// VerdictNeutral 是 bookkeeping / 未知事件，继续缓冲，由首块超时兜底。
	VerdictNeutral Verdict = "neutral"
)

// TerminalKind 区分正常完成与仅表示停止的 incomplete 终态；TerminalNone 表示非终态。
type TerminalKind string

const (
	TerminalComplete   TerminalKind = "complete"
	TerminalIncomplete TerminalKind = "incomplete"
	TerminalNone       TerminalKind = ""
)

type valueMatch struct {
	path   string
	values []string
}

type frameRule struct {
	eventTypes   []string
	anyPaths     []string
	valueMatches []valueMatch
}

type streamSignal struct {
	contentRules   []frameRule
	errorRules     []frameRule
	terminalRules  []frameRule
	terminalEvents []string
	doneSentinel   string
}

// streamSignals 移植自 CCHP generated/content_signals.json（TS 侧 STREAM_SIGNALS）。
//
// 求值语义与 CCHP pkg/protocol/sdkcatalog/content_gate.go 对齐：
//   - 规则内多条件 AND；空规则永不命中
//   - anyPaths：任一路径解析出「非空」值即命中（gjson 语义，见 IsNonEmptyValue）
//   - valueMatches：路径值（数组则任一元素）等于任一候选串即命中
var streamSignals = map[Family]streamSignal{
	FamilyAnthropic: {
		contentRules: []frameRule{
			{
				// text_delta / input_json_delta / thinking_delta / signature_delta / citations_delta
				eventTypes: []string{"content_block_delta"},
				anyPaths: []string{
					"delta.text",
					"delta.partial_json",
					"delta.thinking",
					"delta.signature",
					"delta.citation",
				},
			},
			{
				// start 帧即携带完整实体 payload 的内容块。tool_use 系列只提供 id/name，
				// Anthropic SDK 需要后续 input_json_delta 才能得到可执行 input，不能在这里提交。
				eventTypes: []string{"content_block_start"},
				anyPaths: []string{
					"content_block.data",
					"content_block.content",
					"content_block.file_id",
					"content_block.fileId",
					"content_block.url",
					"content_block.result",
				},
				valueMatches: []valueMatch{{
					path: "content_block.type",
					values: []string{
						"redacted_thinking",
						"web_search_tool_result",
						"web_fetch_tool_result",
						"code_execution_tool_result",
						"bash_code_execution_tool_result",
						"text_editor_code_execution_tool_result",
						"tool_search_tool_result",
						"mcp_tool_result",
						"container_upload",
					},
				}},
			},
		},
		errorRules: []frameRule{
			// SSE event: error + data {"type":"error","error":{...}}
			{eventTypes: []string{"error", "response.error"}},
			// 任意帧携带非空顶层 error 对象（非规范上游 fake-200 兜底）
			{anyPaths: []string{"error"}},
		},
		terminalEvents: []string{"message_stop"},
	},
	FamilyOpenAIChat: {
		contentRules: []frameRule{
			{
				// chunk 无事件名；delta 携带 content/reasoning/tool_calls/refusal/audio 即内容。
				// reasoning 的键别名：reasoning_content（DeepSeek 官方）与 reasoning
				// （OpenRouter 归一格式）。漏一个即让整段推理被判为中性帧，
				// 64 帧预算烧穿 → prebuffer_overflow → 502 → 供应商熔断。
				anyPaths: []string{
					"choices.#.delta.content",
					"choices.#.delta.reasoning_content",
					"choices.#.delta.reasoning",
					"choices.#.delta.tool_calls.#.function.arguments",
					"choices.#.delta.function_call.arguments",
					"choices.#.delta.refusal",
					"choices.#.delta.audio.data",
					"choices.#.delta.audio.transcript",
				},
			},
			{
				// 非增量式上游：有的在流里直接发完整 message 对象（或返回非流式体），一条 delta 都没有。
				// 漏掉这一族即让整条流被判为「全中性帧」，预缓冲预算（64 帧 / 4MiB）烧穿 →
				// prebuffer_overflow → 502 → 供应商熔断（与上面 reasoning 别名漏项同一失效链）。
				//
				// 为什么与 delta 族逐项对齐却**不含** type/id/name：本族只收「已可交付的 payload」。
				// tool_calls 只给 id/name 时 arguments 缺失或为空，天然不命中；这与 anthropic
				// content_block_start 的口径一致（id 与 name 不足以交付可执行 input）。
				// 空 message（只有 role）与 content: "" 同理不命中，故 role 宣告帧仍是中性帧。
				anyPaths: []string{
					"choices.#.message.content",
					"choices.#.message.reasoning_content",
					"choices.#.message.reasoning",
					"choices.#.message.tool_calls.#.function.arguments",
					"choices.#.message.function_call.arguments",
					"choices.#.message.refusal",
					"choices.#.message.audio.data",
					"choices.#.message.audio.transcript",
				},
			},
		},
		errorRules: []frameRule{
			// data: {"error":{...}} 可出现在流中任意位置
			{anyPaths: []string{"error"}},
		},
		doneSentinel: "[DONE]",
	},
	FamilyOpenAIResponses: {
		contentRules: []frameRule{
			{
				// 所有 *.delta 内容事件载荷字段统一为 delta
				eventTypes: []string{
					"response.output_text.delta",
					"response.refusal.delta",
					"response.reasoning_text.delta",
					"response.reasoning_summary_text.delta",
					"response.audio.delta",
					"response.audio.transcript.delta",
					"response.function_call_arguments.delta",
					"response.custom_tool_call_input.delta",
					"response.code_interpreter_call_code.delta",
					"response.mcp_call_arguments.delta",
				},
				anyPaths: []string{"delta"},
			},
			{
				// 渐进图片生成 partial base64
				eventTypes: []string{"response.image_generation_call.partial_image"},
				anyPaths:   []string{"partial_image_b64"},
			},
			{
				// done 帧携带完整文本（兜住跳过 delta 的上游）
				eventTypes: []string{
					"response.output_text.done",
					"response.reasoning_text.done",
					"response.reasoning_summary_text.done",
				},
				anyPaths: []string{"text"},
			},
			{
				eventTypes: []string{"response.audio.transcript.done"},
				anyPaths:   []string{"transcript", "text"},
			},
			{
				eventTypes: []string{"response.refusal.done"},
				anyPaths:   []string{"refusal"},
			},
			{
				eventTypes: []string{
					"response.function_call_arguments.done",
					"response.mcp_call_arguments.done",
				},
				anyPaths: []string{"arguments"},
			},
			{
				eventTypes: []string{"response.custom_tool_call_input.done"},
				anyPaths:   []string{"input"},
			},
			{
				eventTypes: []string{"response.code_interpreter_call_code.done"},
				anyPaths:   []string{"code"},
			},
			{
				// output_item.added 的 name/id/status 只是结构元数据；真实 payload 到达前不能提交，
				// 否则紧随其后的 response.failed / 断流将失去透明 fallback 机会。
				// fake-streaming 的完整 item 会在 added/done 中携带 arguments/input/action，仍可提交。
				eventTypes: []string{"response.output_item.added", "response.output_item.done"},
				anyPaths: []string{
					"item.content.#.text",
					"item.summary.#.text",
					"item.arguments",
					"item.input",
					"item.action",
					"item.queries",
					"item.query",
					"item.code",
					"item.command",
					"item.operation",
				},
			},
		},
		errorRules: []frameRule{
			// 顶层 error 事件（code/message/param）
			{eventTypes: []string{"error", "response.error"}},
			// 整个 response 失败（response.error 已填充）；
			// 子工具失败（mcp_call.failed 等）模型可继续，为中性
			{eventTypes: []string{"response.failed"}},
			// 任意帧携带非空 error 对象（response.* 事件的 error:null 不命中）
			{anyPaths: []string{"error", "response.error"}},
		},
		terminalEvents: []string{"response.completed", "response.incomplete", "response.done"},
	},
	FamilyGemini: {
		contentRules: []frameRule{
			{
				// candidates[].content.parts[] 任一实体载荷字段非空
				anyPaths: []string{
					"candidates.#.content.parts.#.text",
					"candidates.#.content.parts.#.inlineData.data",
					"candidates.#.content.parts.#.fileData.fileUri",
					"candidates.#.content.parts.#.functionCall.name",
					"candidates.#.content.parts.#.functionResponse.name",
					"candidates.#.content.parts.#.executableCode.code",
					"candidates.#.content.parts.#.codeExecutionResult.output",
				},
			},
		},
		errorRules: []frameRule{
			// 流中 {"error":{code,message,status}} chunk
			{anyPaths: []string{"error"}},
			// prompt 被安全策略拦截（首 chunk，无 candidates）
			{anyPaths: []string{"promptFeedback.blockReason"}},
			{
				// 异常终止原因；STOP 与 MAX_TOKENS 为正常终止
				valueMatches: []valueMatch{{
					path: "candidates.#.finishReason",
					values: []string{
						"SAFETY",
						"RECITATION",
						"LANGUAGE",
						"BLOCKLIST",
						"PROHIBITED_CONTENT",
						"SPII",
						"MALFORMED_FUNCTION_CALL",
						"IMAGE_SAFETY",
						"UNEXPECTED_TOOL_CALL",
						"IMAGE_PROHIBITED_CONTENT",
						"NO_IMAGE",
						"IMAGE_RECITATION",
						"IMAGE_OTHER",
						"OTHER",
					},
				}},
			},
		},
		terminalRules: []frameRule{
			// chunk 无事件名；finishReason 出现即近终止（无显式终止哨兵）
			{anyPaths: []string{"candidates.#.finishReason"}},
		},
	},
}

// MapProviderTypeToFamily 把供应商类型映射为协议家族。
//
// 门控分类作用于上游原生 wire 格式（在响应修正与 Gemini 转换之前），因此按供应商类型
// 而非入口格式选择家族。未知类型返回 ok=false（调用方据此跳过门控，fail-open）。
func MapProviderTypeToFamily(providerType string) (Family, bool) {
	switch providerType {
	case "claude", "claude-auth":
		return FamilyAnthropic, true
	case "codex":
		return FamilyOpenAIResponses, true
	case "openai-compatible":
		return FamilyOpenAIChat, true
	case "gemini", "gemini-cli":
		return FamilyGemini, true
	default:
		return "", false
	}
}

// requestEchoEvents 是请求回显帧集合：openai-responses 家族的生命周期首帧
// （response.created / response.in_progress / response.queued）会在 data.response 里回显完整
// 请求体（instructions + input），大上下文请求单帧即可达数百 KB。门控 prebuffer 的字节计数
// 应排除这类帧，避免把「请求大」误判成「流异常」。
var requestEchoEvents = map[Family]map[string]struct{}{
	FamilyOpenAIResponses: {
		"response.created":     {},
		"response.in_progress": {},
		"response.queued":      {},
	},
}

// IsRequestEchoFrame 判定该帧是否属于可豁免字节上限的请求回显帧。
func IsRequestEchoFrame(family Family, event string, data string) bool {
	events, ok := requestEchoEvents[family]
	if !ok {
		return false
	}
	effective := strings.TrimSpace(event)
	if effective != "" {
		_, hit := events[effective]
		return hit
	}
	// 无 event 行时嗅探 data 头部的 type 字段（上游实践中 type 总在最前）
	head := data
	if len(head) > dataHeadBytes {
		head = head[:dataHeadBytes]
	}
	for event := range events {
		if strings.Contains(head, `"type":"`+event+`"`) {
			return true
		}
	}
	return false
}

// Classify 对单个完整 SSE 帧分类。
//
// event 为空串时取 data 顶层 "type" 字段作为事件判别值（OpenAI Responses / Anthropic 的
// data 内嵌 type；OpenAI Chat 与 Gemini 无事件名走纯路径规则）。data 非 JSON 时：命中
// doneSentinel 归 terminal；空 data 保持中性；其余损坏或非对象/数组 JSON 归 malformed
// （fail-closed）。未知家族归中性（fail-open，对应 TS 的 STREAM_SIGNALS[family] 未命中）。
//
// 分类器自身异常一律吞为 neutral：绝不因门控 bug 杀正常流（TS 侧 try/catch 的等价物）。
func Classify(family Family, event string, data string) (verdict Verdict) {
	defer func() {
		if recover() != nil {
			verdict = VerdictNeutral
		}
	}()
	signal, ok := streamSignals[family]
	if !ok {
		return VerdictNeutral
	}
	return classifyFrameInner(family, signal, event, data)
}

func classifyFrameInner(family Family, signal streamSignal, event string, data string) Verdict {
	trimmed := strings.TrimSpace(data)
	if trimmed != "" && signal.doneSentinel != "" && trimmed == signal.doneSentinel {
		return VerdictTerminal
	}
	if trimmed == "" {
		return VerdictNeutral
	}
	if trimmed[0] != '{' && trimmed[0] != '[' {
		return VerdictMalformed
	}
	var parsed any
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
		return VerdictMalformed
	}
	if !isObjectLike(parsed) {
		return VerdictMalformed
	}
	return classifyStructuredInner(family, signal, event, parsed)
}

func classifyStructuredInner(family Family, signal streamSignal, event string, parsed any) Verdict {
	outer := classifyParsedFrame(family, signal, event, parsed)
	if outer != VerdictNeutral || family != FamilyGemini {
		return outer
	}
	// Gemini SDK 可能把原生 chunk 包在 response 中；先保留 envelope 外层错误优先级，
	// 只有外层中性时才解包，供所有门控与 observer 共用同一分类结果。
	record, ok := parsed.(map[string]any)
	if !ok {
		return outer
	}
	inner, ok := record["response"]
	if !ok {
		return outer
	}
	if _, isArray := inner.([]any); isArray {
		return outer
	}
	if _, isMap := inner.(map[string]any); !isMap {
		return outer
	}
	return classifyParsedFrame(family, signal, event, inner)
}

func classifyParsedFrame(family Family, signal streamSignal, event string, parsed any) Verdict {
	effective := strings.TrimSpace(event)
	if effective == "" {
		if record, ok := parsed.(map[string]any); ok {
			if typeField, ok := record["type"].(string); ok {
				effective = typeField
			}
		}
	}

	for _, rule := range signal.errorRules {
		if frameRuleMatches(rule, effective, parsed) {
			return VerdictError
		}
	}
	if family == FamilyOpenAIResponses && isResponsesCompactionContent(effective, parsed) {
		return VerdictContent
	}
	for _, rule := range signal.contentRules {
		if frameRuleMatches(rule, effective, parsed) {
			return VerdictContent
		}
	}
	for _, rule := range signal.terminalRules {
		if frameRuleMatches(rule, effective, parsed) {
			return VerdictTerminal
		}
	}
	if effective != "" && slices.Contains(signal.terminalEvents, effective) {
		return VerdictTerminal
	}
	return VerdictNeutral
}

// isResponsesCompactionContent 处理 remote/server-side compaction 的 opaque state：
// 它是完整协议 payload，部分上游只在 response.completed 中返回 output，
// 不会先发送 response.output_item.done。
func isResponsesCompactionContent(event string, parsed any) bool {
	record, ok := parsed.(map[string]any)
	if !ok {
		return false
	}
	if event == "response.output_item.done" {
		return isNonEmptyCompactionItem(record["item"])
	}
	if event != "response.completed" {
		return false
	}
	response, ok := record["response"].(map[string]any)
	if !ok {
		return false
	}
	output, ok := response["output"].([]any)
	if !ok {
		return false
	}
	for _, item := range output {
		if isNonEmptyCompactionItem(item) {
			return true
		}
	}
	return false
}

// isNonEmptyCompactionItem：同一 output item 内的 type 与 opaque state 必须同时满足协议类型约束。
func isNonEmptyCompactionItem(item any) bool {
	record, ok := item.(map[string]any)
	if !ok {
		return false
	}
	if record["type"] != "compaction" {
		return false
	}
	encrypted, ok := record["encrypted_content"].(string)
	return ok && encrypted != ""
}

// ClassifyTerminalKind 区分正常完成与仅表示停止的 incomplete 终态。
// parsed 传 nil 表示只有帧文本可用（此时不做结构化判定）。
func ClassifyTerminalKind(family Family, event string, parsed any) TerminalKind {
	if parsed != nil {
		if structured := ClassifyStructuredTerminalKind(family, event, parsed); structured != TerminalNone {
			return structured
		}
	}
	if family != FamilyOpenAIResponses {
		return TerminalComplete
	}
	return responsesTerminalKindFromEvent(effectiveEventType(event, parsed))
}

// ClassifyStructuredTerminalKind 独立检测结构化帧的终态信号；
// 内容与终态可能出现在同一 Gemini/Responses 帧。
func ClassifyStructuredTerminalKind(family Family, event string, parsed any) TerminalKind {
	effective := effectiveEventType(event, parsed)
	if family == FamilyOpenAIResponses {
		return responsesTerminalKindFromEvent(effective)
	}
	signal, ok := streamSignals[family]
	if !ok {
		return TerminalNone
	}
	if effective != "" && slices.Contains(signal.terminalEvents, effective) {
		return TerminalComplete
	}
	for _, rule := range signal.terminalRules {
		if frameRuleMatches(rule, effective, parsed) {
			return TerminalComplete
		}
	}
	return TerminalNone
}

func responsesTerminalKindFromEvent(effective string) TerminalKind {
	switch effective {
	case "response.incomplete":
		return TerminalIncomplete
	case "response.completed", "response.done":
		return TerminalComplete
	default:
		return TerminalNone
	}
}

// effectiveEventType 复刻 TS 的「event 为空或 message 时取结构化 type 字段」口径。
func effectiveEventType(event string, parsed any) string {
	effective := strings.TrimSpace(event)
	if effective != "" && effective != "message" {
		return effective
	}
	record, ok := parsed.(map[string]any)
	if !ok {
		return effective
	}
	typeField, ok := record["type"].(string)
	if !ok {
		return effective
	}
	return typeField
}

// IsCleanResponsesCompletion 判定干净完成帧：response.completed 且 response.status == "completed"。
//
// 这类帧在 Classify 中仍是 terminal（协议观测器依赖该判定确认流正常收尾），但对门控而言它是
// 协议层面的成功响应：即使可见内容为空也应当透传，而不是当成空流触发 failover。空回复是合法
// 结果——例如审阅 / watchdog 类 prompt 的契约就是「无问题时保持沉默」。
//
// 非成功终止（response.incomplete、status=failed 等）与携带非空 error 的帧不在此列。
func IsCleanResponsesCompletion(event string, data string) bool {
	effective := strings.TrimSpace(event)
	if effective != "" && effective != "response.completed" {
		return false
	}
	record, ok := parseJSONRecord(data)
	if !ok {
		return false
	}
	if record["type"] != "response.completed" {
		return false
	}
	response, ok := record["response"].(map[string]any)
	if !ok {
		return false
	}
	return response["status"] == "completed" && !IsNonEmptyValue(response["error"])
}

// IsResponsesIncompleteCompletion 判定「明确以 incomplete 状态结束」的可透传终态：
// 它不是成功完成，但也不是供应商故障。
func IsResponsesIncompleteCompletion(event string, data string) bool {
	effective := strings.TrimSpace(event)
	if effective != "" && effective != "response.incomplete" {
		return false
	}
	record, ok := parseJSONRecord(data)
	if !ok {
		return false
	}
	if record["type"] != "response.incomplete" {
		return false
	}
	response, ok := record["response"].(map[string]any)
	if !ok {
		return false
	}
	return response["status"] == "incomplete"
}

func parseJSONRecord(data string) (map[string]any, bool) {
	var parsed any
	if err := json.Unmarshal([]byte(data), &parsed); err != nil {
		return nil, false
	}
	record, ok := parsed.(map[string]any)
	return record, ok
}

func isObjectLike(value any) bool {
	switch value.(type) {
	case map[string]any, []any:
		return true
	default:
		return false
	}
}

// frameRuleMatches 是单条帧规则的 AND 语义；空规则永不命中
// （防目录笔误把所有帧判成内容/错误）。
func frameRuleMatches(rule frameRule, eventType string, parsed any) bool {
	if len(rule.eventTypes) > 0 && !slices.Contains(rule.eventTypes, eventType) {
		return false
	}
	if len(rule.anyPaths) > 0 {
		hit := false
		for _, path := range rule.anyPaths {
			if IsNonEmptyValue(ResolvePath(parsed, path)) {
				hit = true
				break
			}
		}
		if !hit {
			return false
		}
	}
	for _, match := range rule.valueMatches {
		if !valueMatchHits(match, parsed) {
			return false
		}
	}
	return len(rule.eventTypes) > 0 || len(rule.anyPaths) > 0 || len(rule.valueMatches) > 0
}

func valueMatchHits(match valueMatch, parsed any) bool {
	if match.path == "" || len(match.values) == 0 {
		return false
	}
	resolved := ResolvePath(parsed, match.path)
	if resolved == nil {
		return false
	}
	candidates, isArray := resolved.([]any)
	if !isArray {
		candidates = []any{resolved}
	}
	for _, candidate := range candidates {
		switch value := candidate.(type) {
		case string:
			if slices.Contains(match.values, value) {
				return true
			}
		case float64:
			if slices.Contains(match.values, strconv.FormatFloat(value, 'g', -1, 64)) {
				return true
			}
		case bool:
			if slices.Contains(match.values, strconv.FormatBool(value)) {
				return true
			}
		}
	}
	return false
}

// ResolvePath 是 gjson 风格路径求值：`a.b.c` 逐层取键；`#` 段在数组上映射收集。
//
// 含 `#` 的路径返回收集数组（可能为空数组）；路径中断（键不存在 / 非对象）返回 nil。
// 与 TS 的差异：TS 区分 undefined（缺失）与 null，本实现统一用 nil 表示——对规则判定无可观测差异
// （null 与缺失在 IsNonEmptyValue 下同为「空」，在 valueMatches 下同为不命中）。
func ResolvePath(node any, path string) any {
	return resolveSegments(node, strings.Split(path, "."), 0)
}

func resolveSegments(node any, segments []string, index int) any {
	if index == len(segments) {
		return node
	}
	segment := segments[index]
	if segment == "#" {
		list, ok := node.([]any)
		if !ok {
			return nil
		}
		collected := make([]any, 0, len(list))
		for _, item := range list {
			resolved := resolveSegments(item, segments, index+1)
			if resolved == nil {
				continue
			}
			if nested, isArray := resolved.([]any); isArray && hasHashSegment(segments[index+1:]) {
				// 嵌套 # 收集结果展平（gjson a.#.b.#.c 语义）
				collected = append(collected, nested...)
				continue
			}
			collected = append(collected, resolved)
		}
		return collected
	}
	record, ok := node.(map[string]any)
	if !ok {
		return nil
	}
	child, ok := record[segment]
	if !ok {
		return nil
	}
	return resolveSegments(child, segments, index+1)
}

func hasHashSegment(segments []string) bool {
	return slices.Contains(segments, "#")
}

// IsNonEmptyValue 是 gjson 语义的「非空」判定：
//   - 字符串：非 ""
//   - 数字：算内容（含 0）
//   - true 算内容；false / null / 缺失不算
//   - 数组：任一元素非空（覆盖 # 收集结果）
//   - 对象：至少一个键（空对象不算内容）
func IsNonEmptyValue(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case string:
		return typed != ""
	case bool:
		return typed
	case float64:
		return true
	case map[string]any:
		return len(typed) > 0
	case []any:
		for _, item := range typed {
			if IsNonEmptyValue(item) {
				return true
			}
		}
		return false
	default:
		return false
	}
}
