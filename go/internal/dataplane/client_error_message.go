package dataplane

import (
	"regexp"
	"strings"
)

// 本文件实现 `system_settings.pass_through_upstream_error_message` 的两态口径，以及
// 「客户端可见的上游错误文案」的脱敏派生。
//
// Node 的出处：
//   - 开关判定：error-handler.ts:121-158（resolveFinalClientErrorMessage）；
//   - 兜底文案表：error-handler.ts:59-106（getGenericProxyErrorFallbackMessage）；
//   - 脱敏派生：client-error-message.ts:164-197（deriveClientSafeUpstreamErrorMessage）。
//
// 语义要点：开关**打开**（生产值）不等于「原样透传上游正文」，而是「从上游原文派生一条
// 客户端安全文案」——含供应商名、URL、内网标签、请求 id、密钥形状的文本一律丢弃，派生不出来
// 才回退状态码对应的通用文案。开关关闭时直接用通用文案，连派生都不做。
//
// 与 Node 的刻意差异（登记在报告里）：
//  1. 候选列表只取 failure.Message。Node 的第一个候选是「从原文再抽一次」，而 Go 的
//     failure.Message 本来就是 forward 用 messageFromErrorBody 从同一份原文抽出来的，同源。
//  2. 不实现 stripUpstreamDetailSuffix：它只作用于「把调用方传来的兜底文案原样返回」的两支
//     （空响应 / 非 ProxyError），而这两支在 Go 侧根本不进通用文案表（见 failoverStatusFor）。

const maxClientErrorMessageChars = 240

const clientErrorMessageEllipsis = "..."

// allProvidersUnavailableMessage 对齐 Node errors.ts:20 的同名常量。
const allProvidersUnavailableMessage = "所有供应商暂时不可用，请稍后重试"

// defaultForbiddenProviderLabels 对齐 client-error-message.ts:15-32：出现在文案里的
// 供应商品牌名会让客户端知道「这一跳是谁挂的」，属内部信息。
var defaultForbiddenProviderLabels = []string{
	"anthropic", "openai", "gemini", "google", "vertex", "bedrock", "azure",
	"deepseek", "grok", "claude", "codex", "openrouter", "siliconflow",
	"dashscope", "qwen", "kimi",
}

var (
	internalOnlyRe = regexp.MustCompile(`(?is)^(?:FAKE_200_[A-Z0-9_]+|EMPTY_RESPONSE|HTTP\s+\d{3}|No available providers?|No available provider endpoints|` +
		regexp.QuoteMeta(allProvidersUnavailableMessage) + `)$`)
	providerPrefixRe = regexp.MustCompile(`(?i)\bProvider\s+[\w.-]+(?:\s+returned|\s*:|\s+-)`)
	// 内嵌 JSON 也要拒：`Bad request: {"error":{"message":"…"}}` 会把上游载荷整段带出去。
	rawJSONBlobAnchoredRe = regexp.MustCompile(`^[[{]`)
	rawJSONBlobEmbeddedRe = regexp.MustCompile(`[[{]\s*"[^"\n]{1,80}"\s*:`)
)

// sanitizeErrorTextForDetail 复刻 upstream-error-detection.ts:373-404 的脱敏。
//
// 目的不是「完美脱敏」，而是降低上游文案里夹带凭据的风险；替换语义与 Node 逐条一致。
var (
	reBearer        = regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9._-]+`)
	reAPIKeyPrefix  = regexp.MustCompile(`(?i)\b(?:sk|rk|pk)-[A-Za-z0-9_-]{16,}\b`)
	reGoogleAPIKey  = regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{16,}\b`)
	reJWT           = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`)
	reEmail         = regexp.MustCompile(`\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b`)
	reSensitiveKV   = regexp.MustCompile(`(?i)\b(password|token|secret|api[_-]?key)\b\s*[:=]\s*['"]?[^'"\s]+['"]?`)
	reCredentialish = regexp.MustCompile(`(?i)/[\w.-]+\.(?:env|ya?ml|json|conf|ini)`)
)

// 归一化用的替换表（顺序即语义，与 normalizeClientMessage 的链式 replace 一一对应）。
var (
	reURL          = regexp.MustCompile(`(?i)\bhttps?://[^\s"'<>]+`)
	reDomain       = regexp.MustCompile(`(?i)\b(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+(?:com|net|org|io|ai|app|dev|cloud|cn|co|uk|jp|ru|de|fr|us|example)(?:\b|/[^\s"'<>]*)`)
	reHostLike     = regexp.MustCompile(`(?i)\b(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,}(?::\d{2,5})?(?:/[^\s"'<>]*)?\b`)
	reIPv4         = regexp.MustCompile(`\b(?:\d{1,3}\.){3}\d{1,3}(?::\d{2,5})?\b`)
	reIPv6         = regexp.MustCompile(`(?:\[[0-9a-fA-F:]+\](?::\d{2,5})?|(?:\b[0-9a-fA-F]{1,4}:){2,7}[0-9a-fA-F]{1,4}\b)`)
	reRequestID    = regexp.MustCompile(`(?i)\b(?:request[_ -]?id|x-request-id|req(?:uest)?[_ -]?id|trace[_ -]?id|cf-ray)\b\s*[:=]?\s*[A-Za-z0-9._:-]{3,}\b`)
	reRequestIDTok = regexp.MustCompile(`(?i)\b(?:req|msg|chatcmpl|run|trace)_[A-Za-z0-9._-]{3,}\b`)
	// 刻意收窄：endpoint / node / route 这类普通名词只有在带基础设施式限定
	// （分隔符 + 词元，或显式端口）时才算内部标签，否则会误伤
	// “Rate limit exceeded for this endpoint” 这种正常文案。
	reInternalLabel = regexp.MustCompile(`(?i)\b(?:gateway|proxy|provider|vendor|region|shard|cluster|internal|router|route|endpoint|node|alpha|beta)(?:[-_./][\w.-]{2,}|:\d{2,5}(?:/[^\s"'<>]*)?)\b`)
	reKeyValue      = regexp.MustCompile(`(?i)\b(?:api[_ -]?key|token|secret|password)\b\s*[:=]\s*(?:\[REDACTED(?:_KEY)?\]|\[JWT\]|\*\*\*|[A-Za-z0-9._-]{6,})`)
	reDanglingReqID = regexp.MustCompile(`(?i)\b(?:request[_ -]?id|x-request-id|trace[_ -]?id)\b\s*[:=]?\s*$`)
	reDanglingWord  = regexp.MustCompile(`(?i)\s+\b(?:at|from|for|with)\b\s*(?:[,.;:，。；：]|$)`)
	reListComma     = regexp.MustCompile(`\s*[,;，；]\s*`)
	reTrailingColon = regexp.MustCompile(`\s*[:：]\s*$`)
	reWhitespace    = regexp.MustCompile(`\s+`)
	reLeadingLabel  = regexp.MustCompile(`(?i)^(?:error|message)\s*[:：]\s*`)
)

// 归一化后的「是否仍不安全」判据（Node 的无 lastIndex 隐患版本，语义等价）。
var (
	reTaintURL          = regexp.MustCompile(`(?i)\bhttps?://[^\s"'<>]+`)
	reTaintDomain       = reDomain
	reTaintHostLike     = reHostLike
	reTaintIPv4         = reIPv4
	reTaintIPv6         = reIPv6
	reTaintRequestID    = regexp.MustCompile(`(?i)\b(?:request[_ -]?id|x-request-id|req(?:uest)?[_ -]?id|trace[_ -]?id|cf-ray)\b\s*[:=]?\s*[A-Za-z0-9._:-]{3,}\b`)
	reTaintRequestIDTok = reRequestIDTok
	reTaintInternal     = reInternalLabel
	reTaintAPIKeyShape  = regexp.MustCompile(`(?i)\b(?:sk|rk|pk)-[A-Za-z0-9_-]{8,}\b`)
)

// deriveClientSafeUpstreamErrorMessageInput 是派生所需的最小事实集。
type deriveClientSafeUpstreamErrorMessageInput struct {
	// CandidateMessage 是候选文案（Go 侧即 forward 从上游原文抽出的 failure.Message）。
	CandidateMessage string
	// ProviderName 是本次上游的供应商名：它自身也是禁用标签，出现即弃。
	ProviderName string
}

// deriveClientSafeUpstreamErrorMessage 复刻 client-error-message.ts:164-197。
// 返回空串表示派生失败（调用方回退通用文案）。
func deriveClientSafeUpstreamErrorMessage(input deriveClientSafeUpstreamErrorMessageInput) string {
	labels := make([]string, 0, len(defaultForbiddenProviderLabels)+1)
	if strings.TrimSpace(input.ProviderName) != "" {
		labels = append(labels, input.ProviderName)
	}
	labels = append(labels, defaultForbiddenProviderLabels...)

	candidates := []string{input.CandidateMessage}
	for _, candidate := range candidates {
		trimmed := strings.TrimSpace(candidate)
		if trimmed == "" || internalOnlyRe.MatchString(trimmed) || containsRawJSONBlob(trimmed) {
			continue
		}
		if hasForbiddenProviderLabel(trimmed, labels) {
			continue
		}
		normalized := normalizeClientMessage(trimmed)
		if normalized == "" || internalOnlyRe.MatchString(normalized) || containsRawJSONBlob(normalized) {
			continue
		}
		if hasForbiddenProviderLabel(normalized, labels) || isUnsafeAfterRedaction(normalized) {
			continue
		}
		return normalized
	}
	return ""
}

// genericUpstreamErrorMessage 复刻 error-handler.ts:59-106 的状态码兜底文案表。
//
// 表里的文案是**面向客户端**的固定中文，与 status 一一对应；≥500 与其余状态各有一档。
func genericUpstreamErrorMessage(statusCode int) string {
	switch statusCode {
	case 400:
		return "上游请求参数无效，请检查后重试"
	case 401:
		return "上游鉴权失败，请稍后重试"
	case 402:
		return "上游服务当前无法处理该请求"
	case 403:
		return "上游拒绝了本次请求"
	case 404:
		return "上游资源不存在"
	case 408, 504, 524:
		return "上游服务响应超时，请稍后重试"
	case 409:
		return "上游请求发生冲突，请稍后重试"
	case 413:
		return "请求内容过大，上游无法处理"
	case 415:
		return "上游不支持当前请求格式"
	case 422:
		return "上游无法处理当前请求"
	case 429:
		return "上游服务当前限流，请稍后重试"
	}
	if statusCode >= 500 {
		return "上游服务暂时不可用，请稍后重试"
	}
	return "请求上游服务失败，请稍后重试"
}

// sanitizeErrorTextForDetail 脱敏（Bearer / 常见密钥前缀 / JWT / 邮箱 / 键值 / 凭据路径）。
func sanitizeErrorTextForDetail(text string) string {
	sanitized := reBearer.ReplaceAllString(text, "Bearer [REDACTED]")
	sanitized = reAPIKeyPrefix.ReplaceAllString(sanitized, "[REDACTED_KEY]")
	sanitized = reGoogleAPIKey.ReplaceAllString(sanitized, "[REDACTED_KEY]")
	sanitized = reJWT.ReplaceAllString(sanitized, "[JWT]")
	sanitized = reEmail.ReplaceAllString(sanitized, "[EMAIL]")
	sanitized = reSensitiveKV.ReplaceAllString(sanitized, "${1}:***")
	sanitized = reCredentialish.ReplaceAllString(sanitized, "[PATH]")
	return sanitized
}

// normalizeClientMessage 复刻 client-error-message.ts:117-157 的归一化链。
func normalizeClientMessage(text string) string {
	sanitized := sanitizeErrorTextForDetail(text)
	for _, re := range []*regexp.Regexp{
		reURL, reDomain, reHostLike, reIPv4, reIPv6,
		reRequestID, reRequestIDTok, reInternalLabel, reKeyValue,
		reDanglingReqID, reDanglingWord,
	} {
		sanitized = re.ReplaceAllString(sanitized, " ")
	}
	sanitized = reListComma.ReplaceAllString(sanitized, ", ")
	sanitized = reTrailingColon.ReplaceAllString(sanitized, "")
	sanitized = reWhitespace.ReplaceAllString(sanitized, " ")
	sanitized = strings.TrimSpace(sanitized)
	sanitized = strings.TrimSpace(reLeadingLabel.ReplaceAllString(sanitized, ""))
	// 截断按**字符**计（Node 的 slice 是 UTF-16 码元，中文文案在两种口径下同长）。
	if len([]rune(sanitized)) > maxClientErrorMessageChars {
		budget := maxClientErrorMessageChars - len(clientErrorMessageEllipsis)
		sanitized = strings.TrimSpace(string([]rune(sanitized)[:budget])) + clientErrorMessageEllipsis
	}
	return sanitized
}

// containsRawJSONBlob 判定文案是否夹带原始 JSON 载荷。
func containsRawJSONBlob(text string) bool {
	return rawJSONBlobAnchoredRe.MatchString(text) || rawJSONBlobEmbeddedRe.MatchString(text)
}

// hasForbiddenProviderLabel 判定文案是否含供应商品牌名或 `Provider xxx` 前缀。
func hasForbiddenProviderLabel(message string, labels []string) bool {
	if providerPrefixRe.MatchString(message) {
		return true
	}
	lowered := strings.ToLower(message)
	for _, label := range labels {
		trimmed := strings.TrimSpace(label)
		if trimmed == "" || len([]rune(trimmed)) <= 1 {
			continue
		}
		if isASCIIWordToken(trimmed) {
			if regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(trimmed) + `\b`).MatchString(message) {
				return true
			}
			continue
		}
		if strings.Contains(lowered, strings.ToLower(trimmed)) {
			return true
		}
	}
	return false
}

// isASCIIWordToken 判定标签是否是「词式」的（可加词边界），对齐 Node 的 /^[\w.-]+$/ + ASCII 判据。
func isASCIIWordToken(value string) bool {
	for _, char := range value {
		if char > 0x7f {
			return false
		}
		switch {
		case char >= '0' && char <= '9':
		case char >= 'a' && char <= 'z':
		case char >= 'A' && char <= 'Z':
		case char == '_', char == '.', char == '-':
		default:
			return false
		}
	}
	return value != ""
}

// isUnsafeAfterRedaction 判定归一化后的文案是否仍带敏感形状。
func isUnsafeAfterRedaction(text string) bool {
	if strings.TrimSpace(text) == "" {
		return true
	}
	for _, re := range []*regexp.Regexp{
		reTaintURL, reTaintDomain, reTaintHostLike, reTaintIPv4, reTaintIPv6,
		reTaintRequestID, reTaintRequestIDTok, reTaintInternal, reTaintAPIKeyShape,
	} {
		if re.MatchString(text) {
			return true
		}
	}
	return false
}

// upstreamErrorStatusCode 判定「上游返回过错误响应」的状态码区间。
func upstreamErrorStatusCode(status int) bool {
	return status >= 400 && status <= 599
}
