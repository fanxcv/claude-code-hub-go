// Package specialsettings 产出随请求记录落库的审计条目（`message_request.special_settings`）。
//
// 为什么单独成包：数据面有**两个写入时刻**，两侧必须用同一套提取规则，否则同一字段会出现
// 两种口径（本仓已多次踩到「两份实现迟早分叉」）：
//   - **建行时**（守卫链的 messageContext）= 客户端侧的请求审计（思考强度等）；
//   - **终态时**（响应期）= 「转换后实际发给上游」的探针条目。
//
// 唯一真源（逐条对齐，勿凭记忆改）：
//   - `src/app/v1/_lib/proxy/message-service.ts:56-105`（建行时的写入条件与顺序）
//   - `src/lib/utils/anthropic-effort.ts` / `codex-reasoning-effort.ts` /
//     `openai-reasoning-effort.ts`（提取与归一）
//   - `src/app/v1/_lib/proxy/endpoint-paths.ts:34-42`（端点归一）
//
// 三条不变量（写错任一条都会让使用记录页的列**静默变空**，而不是报错）：
//  1. **按客户端入站协议**选提取器，不是按供应商协议。`body` 始终是客户端原始 body；
//     协议转换生效时两者不同，按供应商线选提取器会拿错方言的解析器读同一份 body → 恒为空。
//     Node 的实测记录就写在 message-service.ts 的方法头注释里。
//  2. 三个归一器**不完全相同**：anthropic/codex 返回**裁剪后**的值，openai 返回**原值**
//     （只要裁剪后非空）。审计要如实反映客户端发来的字符串。
//  3. openai chat 有**端点守卫**：`originalFormat=openai` 只说明客户端线，embeddings 等端点
//     也走这条线，必须确认 path 确实是 `/v1/chat/completions`。
package specialsettings

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
)

// 客户端侧的三种 kind，与前端提取器逐字对应。
const (
	TypeAnthropicEffort = "anthropic_effort"
	TypeCodexEffort     = "codex_reasoning_effort"
	TypeOpenAIEffort    = "openai_reasoning_effort"
)

// TypeThinkingEffortForwarded 是**转换后思考强度探针**（Go 侧新增，超出 Node parity）。
//
// 为什么需要它：使用记录页的「思考强度」列若只记客户端请求值，就证明不了**协议转换是否正确**——
// 客户端说 high、上游收到 high 才算对。这条条目记录**转换器产物本身**里该字段的值，
// 故它可以直接与假上游抓到的 body 对照。
//
// 与 Node 的关系（报告里已标注）：Node 没有这个 kind；它靠 provider_parameter_override 的
// `changes[{path,before,after}]` 表达「请求值 → 实际值」，但那只覆盖**供应商级参数覆写**，
// 不覆盖**协议转换**造成的改写/丢弃。语义不同，故不硬塞进那个形状。
// 前端识别方式：`type === "thinking_effort_forwarded"` 时读 `forwardedEffort`（effective）；
// `dropped === true` 表示转换把字段丢了（显式记录，不是静默为空）。
const TypeThinkingEffortForwarded = "thinking_effort_forwarded"

// TypeProtocolConversion 是「本次上游尝试**确实施加了**协议转换」的审计类型，与 Node 逐字同形。
//
// 产出条件对齐 Node（`src/app/v1/_lib/proxy/forwarder.ts:3778`）：只有转换计划**真正落到上游正文上**
// 时才写；未转换的请求**不写**反向标记（Node 注释原话：否则等于给全部原生请求白写一条）。
// 在 Go 侧，这个条件就是 `forward.Plan.Conversion != nil`——路径解析失败与正文转换失败
// 都会把 `Conversion` 置回 nil（后者另置 `ConversionFallback`），故它天然等价于 Node 的
// 「conversionPlan 非空且 prepared 成功」。
//
// 形状（`src/types/special-settings.ts:40-48`）：
//
//	{"type":"protocol_conversion","scope":"request","hit":true,
//	 "clientProtocol":"anthropic-messages","targetProtocol":"openai-chat"}
//
// 前端消费点：`protocol-conversion-display.tsx`（使用记录页的「协议转换」列）。
const TypeProtocolConversion = "protocol_conversion"

// TypeProtocolConversionFailed 是「本次**本来要**施加协议转换、但转换失败」的审计类型。
//
// 为何新增（**超出 Node parity，如实登记**）：Node 的实现里转换失败是**静默**的——只把
// conversionPlan 置空并回退原生直通，不向任何审计面写痕；Go 侧原本逐字复刻了这一点（连 err
// 都丢弃）。后果是使用记录页上「没转换」与「想转换但失败了」**长得一样**，而这两件事的排障方向
// 完全不同（前者查配置，后者查正文形状/工具名/端点路径）。本条目把后者显式记出。
//
// 与 TypeProtocolConversion 的互斥（硬约束）：同一次尝试只会有一条——
// 成功写 `protocol_conversion`（条件 `plan.Conversion != nil`），失败写本条
// （条件 `plan.ConversionFailure != nil`），原生同协议两者都不写。
// 因此**成功路径不会多一次写入**：两条都走同一个 AppendEntries 数组。
//
// 形状：
//
//	{"type":"protocol_conversion_failed","scope":"request","hit":true,
//	 "clientProtocol":"anthropic-messages","targetProtocol":"openai-chat",
//	 "phase":"body_conversion","reason":"…已脱敏、已定长…","fallback":true}
//
// 前端消费点：`protocol-conversion-display.tsx`（「协议转换」列显示失败态而非空白）。
const TypeProtocolConversionFailed = "protocol_conversion_failed"

// TypeProtocolConversionLoss 是「本次协议转换**丢/降/改**了哪些能力」的审计类型
// （**Go 侧新增，超出 Node parity**）。
//
// 为何需要（本条的由来）：`convert.LossReport` 一直是**只写不读的死数据**——转换器（decode/encode）
// 逐项记下「某个能力被丢弃/降级/改写」，`forward.BuildPlan` 把它挂到 `plan.ConversionLoss`，
// 然后**没有任何消费者**：不写日志、不写审计、不落库。后果是使用记录页与库里都看不到
// 「这次跨线转换把 cache_control / 思考签名 / MCP 工具声明丢了」——只能读代码或跑单测才知道。
// 本条把损失集变成随行落库的审计事实，使「哪些跨线请求在丢东西」可在生产上直接查询。
//
// 与 TypeProtocolConversion 的关系（**可共存，不是互斥**）：那条只说「转换发生了」，本条说
// 「转换丢了什么」。转换成功且零损失时**只有** protocol_conversion（不写零损失的噪声条目）；
// 转换它本身失败时走 TypeProtocolConversionFailed，此时根本没有损失集（转换没做，无从丢）。
//
// 形状：
//
//	{"type":"protocol_conversion_loss","scope":"request","hit":true,
//	 "clientProtocol":"openai-responses","targetProtocol":"openai-chat",
//	 "total":3,"groups":[{"capability":"cache_control","action":"dropped","count":2}, …]}
const TypeProtocolConversionLoss = "protocol_conversion_loss"

// maxConversionFailureReasonLength 是失败原因落库的字节上限。
//
// 为何定长：原因文本来自底层错误链，长度不可控；审计列是 JSONB，但界面与接口上都按
// 「可读一行」对待，无上限的长文本会把详情弹窗刷满而无助于定位。截断时显式加标记。
const maxConversionFailureReasonLength = 400

// chatCompletionsPath 复刻 endpoint-paths.ts 的 V1_ENDPOINT_PATHS.CHAT_COMPLETIONS。
const chatCompletionsPath = "/v1/chat/completions"

// EffortRequest 是**客户端请求侧**的思考强度（requested）。
type EffortRequest struct {
	// Effort 是客户端声明的值；Present 为假时为空串（客户端没给该字段）。
	Effort string
	// Field 是该值在客户端 body 里的路径（如 "output_config.effort"），供审计可读性用。
	Field string
	// Kind 是识别它的那一种客户端 kind（anthropic_effort 等）；空串表示没识别到。
	Kind     string
	Protocol convert.WireProtocol
	Present  bool
}

// EffortForwarded 是**转换器产物侧**的思考强度（effective）。
type EffortForwarded struct {
	Effort string
	// Field 是该值在上游 body 里的路径（如 "reasoning_effort"）。
	Field    string
	Protocol convert.WireProtocol
	Present  bool
}

// RequestTrimmedEffort 按客户端协议提取并归一请求侧思考强度。
//
// 返回的 Effort 一律用**归一器规则**处理：anthropic/codex 用裁剪后的值，openai 用原值
// （见包注释的「三条不变量」第 2 条）。
func RequestTrimmedEffort(
	body map[string]any,
	format convert.ClientFormat,
	endpoint string,
) EffortRequest {
	switch format {
	case convert.FormatClaude:
		if effort, ok := trimmedNonEmptyString(nestedField(body, "output_config", "effort")); ok {
			return EffortRequest{
				Effort:   effort,
				Field:    "output_config.effort",
				Kind:     TypeAnthropicEffort,
				Protocol: convert.ProtocolAnthropicMessages,
				Present:  true,
			}
		}
	case convert.FormatResponse:
		if effort, ok := trimmedNonEmptyString(nestedField(body, "reasoning", "effort")); ok {
			return EffortRequest{
				Effort:   effort,
				Field:    "reasoning.effort",
				Kind:     TypeCodexEffort,
				Protocol: convert.ProtocolOpenAIResponses,
				Present:  true,
			}
		}
	case convert.FormatOpenAI:
		// 端点守卫：只有 chat/completions 的 body 才是对话请求体。
		if normalizeEndpointPath(endpoint) != chatCompletionsPath {
			return EffortRequest{}
		}
		// 顶层 `reasoning_effort` 优先（与 OpenRouter 官方「二者不可冲突」的语义一致）。
		if value, ok := body["reasoning_effort"].(string); ok && strings.TrimSpace(value) != "" {
			return EffortRequest{
				Effort:   value, // openai 归一返回原值，不裁剪
				Field:    "reasoning_effort",
				Kind:     TypeOpenAIEffort,
				Protocol: convert.ProtocolOpenAIChat,
				Present:  true,
			}
		}
		if value, ok := nestedField(body, "reasoning", "effort").(string); ok && strings.TrimSpace(value) != "" {
			return EffortRequest{
				Effort:   value,
				Field:    "reasoning.effort",
				Kind:     TypeOpenAIEffort,
				Protocol: convert.ProtocolOpenAIChat,
				Present:  true,
			}
		}
	}
	return EffortRequest{}
}

// ForwardedTrimmedEffort 从**即将发往上游的正文**里读取该协议线下的思考强度。
//
// 这是探针的唯一可信来源：入参必须就是转换器的产物（`forward.Plan.Body`），
// **不得**由记录侧二次推导——否则它证明不了转换是否正确。
//
// 仅适用于**目标协议线**：原生直通（未发生转换）时正文仍属客户端方言，
// 该走 `ForwardedTrimmedEffortAt` 用客户端字段路径读（见 dataplane 的选择点）。
func ForwardedTrimmedEffort(protocol convert.WireProtocol, outgoing []byte) EffortForwarded {
	if len(outgoing) == 0 {
		return EffortForwarded{}
	}
	value, err := convert.ParseJSON(outgoing)
	if err != nil {
		return EffortForwarded{}
	}
	body := value
	switch protocol {
	case convert.ProtocolAnthropicMessages:
		if effort, ok := trimmedNonEmptyString(nestedStringField(body, "output_config", "effort")); ok {
			return EffortForwarded{Effort: effort, Field: "output_config.effort", Protocol: protocol, Present: true}
		}
	case convert.ProtocolOpenAIResponses:
		if effort, ok := trimmedNonEmptyString(nestedStringField(body, "reasoning", "effort")); ok {
			return EffortForwarded{Effort: effort, Field: "reasoning.effort", Protocol: protocol, Present: true}
		}
	case convert.ProtocolOpenAIChat:
		if effort, ok := trimmedNonEmptyString(topLevelStringField(body, "reasoning_effort")); ok {
			return EffortForwarded{Effort: effort, Field: "reasoning_effort", Protocol: protocol, Present: true}
		}
		if effort, ok := trimmedNonEmptyString(nestedStringField(body, "reasoning", "effort")); ok {
			return EffortForwarded{Effort: effort, Field: "reasoning.effort", Protocol: protocol, Present: true}
		}
	}
	return EffortForwarded{}
}

// ForwardedTrimmedEffortAt 按**给定字段路径**从上游正文里读思考强度（原生直通用）。
//
// 为何不能一律用 ForwardedTrimmedEffort：原生直通时正文是客户端方言，字段路径属于客户端线；
// 按目标线去猜路径会读空，于是把一个「根本没转换、字段原样透传」的请求误报为 dropped。
func ForwardedTrimmedEffortAt(outgoing []byte, field string, protocol convert.WireProtocol) EffortForwarded {
	if len(outgoing) == 0 || field == "" {
		return EffortForwarded{}
	}
	value, err := convert.ParseJSON(outgoing)
	if err != nil {
		return EffortForwarded{}
	}
	outer, inner, nested := strings.Cut(field, ".")
	var raw any
	if nested {
		raw = nestedStringField(value, outer, inner)
	} else {
		raw = topLevelStringField(value, outer)
	}
	if effort, ok := trimmedNonEmptyString(raw); ok {
		return EffortForwarded{Effort: effort, Field: field, Protocol: protocol, Present: true}
	}
	return EffortForwarded{}
}

// topLevelStringField 取 `value[key]` 的字符串字段；不存在或非字符串时返回 nil。
func topLevelStringField(value *convert.Value, key string) any {
	text, ok := value.StringField(key)
	if !ok {
		return nil
	}
	return text
}

// nestedStringField 取 `value[outer][inner]` 的字符串字段；不存在或非字符串时返回 nil
// （返回 any 是为了与 trimmedNonEmptyString 的入参口径一致：一处过滤，不写两份。
func nestedStringField(value *convert.Value, outer string, inner string) any {
	object := value.ObjectField(outer)
	if object == nil {
		return nil
	}
	text, ok := object.StringField(inner)
	if !ok {
		return nil
	}
	return text
}

// RequestEntries 产出建行时写入的客户端侧条目（JSON 数组）。nil 表示「没有特殊设置」，
// 该列写 NULL——与 Node 的 `getSpecialSettings()` 返回 `null` 同义（不要写空数组：两态不同）。
func RequestEntries(
	body map[string]any,
	format convert.ClientFormat,
	endpoint string,
) []byte {
	requested := RequestTrimmedEffort(body, format, endpoint)
	if !requested.Present {
		return nil
	}
	return marshal([]map[string]any{{
		"type":   requested.Kind,
		"scope":  "request",
		"hit":    true,
		"effort": requested.Effort,
		// source 只有 openai 那条有（与 Node 的字段集一致）。
		"source": openAISource(requested),
	}})
}

// ProbeEntry 产出**一条**转换后思考强度探针条目；无值可记时返回 nil（不写噪声条目）。
//
// 记录规则：
//   - 客户端有值、上游也有值 → `forwardedEffort` 记上游那个值；`dropped=false`。
//     两者不同即说明转换改写了它（调用方可据此对照）。
//   - **转换生效**且客户端有值、上游没值 → `dropped=true`（**显式记账**，不是静默为空）。
//     仅在 converted 为真时判 dropped：原生直通时字段原样透传，读不到只可能是路径取错，
//     把它当丢弃会造出假证据。
//   - 客户端没有值、上游有值 → 也记（转换补了值，同样是事实）。
//   - 两侧都没有 → 返回 nil。
func ProbeEntry(
	requested EffortRequest,
	forwarded EffortForwarded,
	converted bool,
) map[string]any {
	if !requested.Present && !forwarded.Present {
		return nil
	}
	entry := map[string]any{
		"type":            TypeThinkingEffortForwarded,
		"scope":           "request",
		"hit":             true,
		"dropped":         converted && requested.Present && !forwarded.Present,
		"converted":       converted,
		"clientProtocol":  string(requested.Protocol),
		"targetProtocol":  string(forwarded.Protocol),
		"requestedEffort": nil,
		"forwardedEffort": nil,
	}
	if requested.Present {
		entry["requestedEffort"] = requested.Effort
		entry["requestedField"] = requested.Field
		entry["clientProtocol"] = string(requested.Protocol)
	}
	if forwarded.Present {
		entry["forwardedEffort"] = forwarded.Effort
		entry["forwardedField"] = forwarded.Field
		entry["targetProtocol"] = string(forwarded.Protocol)
	}
	return entry
}

// ProbeEntries 产出探针条目的 JSON 数组（单条形态的历史入口，保留供既有调用方与测试使用）。
func ProbeEntries(
	requested EffortRequest,
	forwarded EffortForwarded,
	converted bool,
) []byte {
	return AppendEntries(ProbeEntry(requested, forwarded, converted))
}

// ConversionEntry 产出协议转换审计条目；plan 为 nil（本次未施加转换）时返回 nil。
//
// 值**只能**来自计划里的协议对（即真实用于构造上游端点的那个计划），不从别处推导。
func ConversionEntry(plan *convert.ConversionPlan) map[string]any {
	if plan == nil {
		return nil
	}
	return map[string]any{
		"type":           TypeProtocolConversion,
		"scope":          "request",
		"hit":            true,
		"clientProtocol": string(plan.ClientProtocol),
		"targetProtocol": string(plan.TargetProtocol),
	}
}

// ConversionFailureEntry 产出协议转换失败的审计条目；failure 为 nil（本次未失败）时返回 nil。
//
// 与 ConversionEntry 互斥：调用方在同一处按计划事实二选一（见 dataplane.specialSettingsAppendEntries）。
// 原因文本在这里统一**脱敏 + 定长**（单一咽喉点），因为它是唯一一个来自底层错误链的字段。
func ConversionFailureEntry(failure *convert.ConversionFailure) map[string]any {
	if failure == nil {
		return nil
	}
	return map[string]any{
		"type":           TypeProtocolConversionFailed,
		"scope":          "request",
		"hit":            true,
		"clientProtocol": string(failure.ClientProtocol),
		"targetProtocol": string(failure.TargetProtocol),
		"phase":          string(failure.Phase),
		"reason":         SanitizeReason(failure.Reason),
		"fallback":       failure.Fallback,
	}
}

// ConversionLossEntry 产出协议转换损失的审计条目；plan 为 nil（本次未转换）或本次零损失时返回 nil。
//
// 聚合口径：按 (capability, action) 分组计数，**不逐条展开**。理由是损失条目的 detail 来自数据
// （工具名、字段路径、`synthesized:<tool_call id>` 之类），逐条落库会做两件坏事：把一次工具密集
// 请求的 jsonb 撑到不可读，以及把审计面变成第二份请求体（连带把上游/客户端的数据搬进审计列）。
// 故 detail 一律不进条目——定位靠 capability + action + fix 代码，逐条细节由单测与复现取。
//
// 体积上界（可核）：capability 取自 convert 的 21 个 `Loss*` 常量（hub.go 的两段 capability
// const 块），action 只有 dropped/downgraded/rewritten 三种，故 groups **至多 21×3=63 组**、
// 每组约 60 字节，加上协议对与 total 共约 4KB 的硬上界（远小于同层既有条目如 provider_parameter_override）。
//
// 分组顺序按 (capability, action) 字典序固定：同一份损失集必须序列化成同一份字节，
// 否则测试无法断言、前端按内容去重（buildUnifiedSpecialSettings）也会把同一条事实当成两条。
func ConversionLossEntry(plan *convert.ConversionPlan, loss *convert.LossReport) map[string]any {
	if plan == nil || loss == nil || len(loss.Entries) == 0 {
		return nil
	}
	type lossGroup struct {
		capability string
		action     string
	}
	counts := make(map[lossGroup]int, len(loss.Entries))
	for _, entry := range loss.Entries {
		counts[lossGroup{capability: entry.Capability, action: string(entry.Action)}]++
	}
	groups := make([]lossGroup, 0, len(counts))
	for group := range counts {
		groups = append(groups, group)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].capability != groups[j].capability {
			return groups[i].capability < groups[j].capability
		}
		return groups[i].action < groups[j].action
	})
	aggregated := make([]map[string]any, 0, len(groups))
	for _, group := range groups {
		aggregated = append(aggregated, map[string]any{
			"capability": group.capability,
			"action":     group.action,
			"count":      counts[group],
		})
	}
	return map[string]any{
		"type":           TypeProtocolConversionLoss,
		"scope":          "request",
		"hit":            true,
		"clientProtocol": string(plan.ClientProtocol),
		"targetProtocol": string(plan.TargetProtocol),
		// total 是**未聚合**的损失条目数：与 groups 的分组数一起读，才能看出
		// 「同类丢了 1 次」还是「同类丢了 50 次」。
		"total":  len(loss.Entries),
		"groups": aggregated,
	}
}

// SanitizeReason 把失败原因处理成可落库的文本：单行化 + 脱敏 + 定长。
//
// 为什么必须脱敏：审计会被管理面读取并在界面上展示，而错误链里可能带上游 URL（含用户信息/查询串）
// 或形如 `Bearer …`、`sk-…` 的凭据片段。本函数只处理这三个可确定的形态：
//   - URL：逐词用仓库既有的 dial.RedactURL（去 userinfo/query/fragment）——不自造第二套 URL 处理；
//   - `Bearer <token>`：保留认证方案名，token 换成占位符；
//   - 常见密钥前缀（sk-/pk-/rk-/ghp_/glpat-）：保留前缀，其余换成占位符。
//
// 未列出的形态一律保留原文（宁可少改也不把可用于定位的信息洗掉），故它是**尽力而为**而非保证；
// 真正不该外漏的字段不应进入错误文本（上游凭据在 headers 里，不进错误链）。
func SanitizeReason(reason string) string {
	collapsed := strings.Join(strings.Fields(reason), " ")
	if collapsed == "" {
		return ""
	}
	tokens := strings.Split(collapsed, " ")
	for index, token := range tokens {
		switch {
		case strings.Contains(token, "://"):
			tokens[index] = dial.RedactURL(token)
		case index > 0 && strings.EqualFold(tokens[index-1], "Bearer"):
			tokens[index] = redactedPlaceholder
		case hasSecretPrefix(token):
			tokens[index] = redactSecretPrefix(token)
		}
	}
	sanitized := strings.Join(tokens, " ")
	if len(sanitized) <= maxConversionFailureReasonLength {
		return sanitized
	}
	// 字节截断不能把 UTF-8 汉字切一半：按 rune 边界回退。
	cut := maxConversionFailureReasonLength - len(truncatedMarker)
	for cut > 0 && !utf8.RuneStart(sanitized[cut]) {
		cut--
	}
	return sanitized[:cut] + truncatedMarker
}

const (
	redactedPlaceholder = "<redacted>"
	truncatedMarker     = "…（已截断）"
)

// secretPrefixes 是常见密钥前缀（与仓库 Node 侧脱敏工具的覆盖面保持一致的最小集）。
var secretPrefixes = []string{"sk-", "pk-", "rk-", "ghp_", "glpat-"}

// hasSecretPrefix 报告 token 是否形如密钥（前缀匹配且后面还有足够长度）。
func hasSecretPrefix(token string) bool {
	for _, prefix := range secretPrefixes {
		if strings.HasPrefix(token, prefix) && len(token) > len(prefix)+6 {
			return true
		}
	}
	return false
}

// redactSecretPrefix 保留前缀与一个短尾巴，便于比对「是哪把钥匙」而不泄露内容。
func redactSecretPrefix(token string) string {
	for _, prefix := range secretPrefixes {
		if strings.HasPrefix(token, prefix) {
			return prefix + redactedPlaceholder
		}
	}
	return redactedPlaceholder
}

// AppendEntries 把若干个条目合成终态要追加的 JSON 数组；全为 nil 时返回 nil
// （nil 表示「本次没有可追加的审计」，与空数组不同：不会在列上留下空写入）。
func AppendEntries(entries ...map[string]any) []byte {
	kept := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		if entry != nil {
			kept = append(kept, entry)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return marshal(kept)
}

// openAISource 复刻 Node 的 `source` 字段（只有 openai 那条带它），其余 kind 写 null。
func openAISource(requested EffortRequest) any {
	if requested.Kind != TypeOpenAIEffort {
		return nil
	}
	return requested.Field
}

func marshal(entries []map[string]any) []byte {
	raw, err := json.Marshal(entries)
	if err != nil {
		// 入参是本进程构造的 map（值只有 string/bool/nil），序列化不该失败；
		// 真失败也不该让请求失败——审计缺失比请求失败轻。
		return nil
	}
	return raw
}

// nestedField 取 `body[a][b]`（Node 侧对两处都做了「对象且非数组」排除）。
func nestedField(body map[string]any, outer string, inner string) any {
	if body == nil {
		return nil
	}
	object, ok := body[outer].(map[string]any)
	if !ok {
		return nil
	}
	return object[inner]
}

// trimmedNonEmptyString 过滤非字符串与空白值（避免把无效参数写进审计），返回裁剪后的值。
func trimmedNonEmptyString(value any) (string, bool) {
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "", false
	}
	return trimmed, true
}

// normalizeEndpointPath 复刻 endpoint-paths.ts 的 normalizeEndpointPath：去查询串、
// 去尾斜杠（根路径除外）、小写化。
func normalizeEndpointPath(path string) string {
	trimmed := path
	if index := strings.IndexByte(trimmed, '?'); index >= 0 {
		trimmed = trimmed[:index]
	}
	if len(trimmed) > 1 && strings.HasSuffix(trimmed, "/") {
		trimmed = trimmed[:len(trimmed)-1]
	}
	return strings.ToLower(trimmed)
}
