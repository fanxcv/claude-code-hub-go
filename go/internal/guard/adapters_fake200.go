package guard

import (
	"encoding/json"
	"regexp"
	"strings"
	"unicode/utf8"
)

// 本文件实现 forward.BodyErrorDetector 的实体：上游返回 HTTP 200，但正文其实是错误
//（Node 的「假 200」检测，见 utils/upstream-error-detection.ts）。
//
// 为什么必须有它：假 200 的供应商在鉴权/配额/风控失败时把错误塞进 200 正文，若不检测，
// 这类响应会被记为成功——于是熔断器不开、故障转移不发生，而客户端持续拿到错误正文。
//
// 判定只认**强信号**（与 Node 的非流式路径一致）：
//  1. 明显的 HTML 文档（网关/WAF/Cloudflare 错误页）；
//  2. 顶层 `error` 非空（字符串，或对象且其 message 非空）；
//  3. OpenAI Responses 的失败信号（type/status 为 response.failed / failed）。
//
// 有意不做（宁漏不误）：扫描模型生成的正文（content/choices 里的 "error" 字样）——
// 那是把用户自然语言当供应商故障，会让健康供应商被熔断。
//
// 与 Node 的差异（有意，且只影响推断出的状态码，不影响「是否判为错误」）：
//   - Node 的状态码推断正则用了 lookbehind/lookahead 与 Unicode 属性类，Go 的 regexp（RE2）
//     不支持前者。此处保留同义关键词、去掉前后视断言，因此对 "4290" 这类数字串可能比 Node
//     略宽——放宽只影响记账里记成 429 还是 502，两个方向都会触发故障转移。
//   - SSE 分支按 data: 事件逐个看 JSON，不做 Node 的 `__sseEvent` 注入。

const (
	// fake200HTMLSniffMaxChars 是 HTML 判定只看前 1024 字符的上限（对齐 Node）。
	fake200HTMLSniffMaxChars = 1024
	// fake200StatusInferenceMaxChars 是状态码推断的文本前缀上限（对齐 Node 的 64 KiB）。
	fake200StatusInferenceMaxChars = 64 * 1024
	// fake200DefaultStatus 是推断不出更具体状态码时的默认值（对齐 Node 的 502）。
	fake200DefaultStatus = 502
	// fake200DetailMaxLen 是错误文案的截断长度（对齐 Node 的 truncateForDetail 默认值）。
	fake200DetailMaxLen = 200
)

var (
	fake200HTMLDoctype = regexp.MustCompile(`(?i)^<!doctype\s+html[\s>]`)
	fake200HTMLTag     = regexp.MustCompile(`(?i)^<html[\s>]`)
	// fake200SSEDataPrefix 匹配 SSE 的 data 行前缀。
	fake200SSEDataPrefix = regexp.MustCompile(`^data:\s*`)
)

// fake200StatusMatcher 是一条状态码推断规则（对齐 Node 的 ERROR_STATUS_MATCHERS）。
type fake200StatusMatcher struct {
	statusCode int
	matcherID  string
	re         *regexp.Regexp
}

// fake200StatusMatchers 的顺序即优先级：先命中者胜（与 Node 同序）。
var fake200StatusMatchers = []fake200StatusMatcher{
	{429, "rate_limit", regexp.MustCompile(`(?i)(too\s+many\s+requests|rate\s*limit(ed|ing)?|throttl(e|ed|ing)|retry-after|RESOURCE_EXHAUSTED|RequestLimitExceeded|Throttling|超出频率|请求过于频繁|限流|稍后重试)`)},
	{402, "payment_required", regexp.MustCompile(`(?i)(payment\s+required|insufficient\s+(balance|funds|credits)|(out\s+of|no)\s+credits|insufficient_balance|billing_hard_limit_reached|card\s+(declined|expired)|payment\s+(method|failed)|余额不足|欠费|请充值|支付(失败|方式))`)},
	{401, "unauthorized", regexp.MustCompile(`(?i)(unauthori(sed|zed)|unauthenticated|authentication\s+failed|(invalid|incorrect|missing)\s+api[-_ ]?key|invalid\s+token|expired\s+token|signature\s+(invalid|mismatch)|未授权|鉴权失败|密钥无效|token\s*过期)`)},
	{403, "forbidden", regexp.MustCompile(`(?i)(forbidden|permission\s+denied|access\s+denied|not\s+allowed|account\s+(disabled|suspended|banned)|not\s+whitelisted|PERMISSION_DENIED|AccessDenied|Error\s*1020|地区不支持|禁止访问|无权限|权限不足|账号被封|地区(限制|屏蔽))`)},
	{404, "not_found", regexp.MustCompile(`(?i)((model|deployment|endpoint|resource|route|path|api|service|url)\s+not\s+found|unknown\s+model|does\s+not\s+exist|NOT_FOUND|ResourceNotFoundException|未找到|不存在|模型不存在)`)},
	{413, "payload_too_large", regexp.MustCompile(`(?i)(payload\s+too\s+large|request\s+entity\s+too\s+large|body\s+too\s+large|请求体过大|内容过大|超过最大)`)},
	{415, "unsupported_media_type", regexp.MustCompile(`(?i)(unsupported\s+media\s+type|invalid\s+content-type|不支持的媒体类型)`)},
	{409, "conflict", regexp.MustCompile(`(?i)(conflict|idempotency(|-key)|ABORTED|冲突|幂等)`)},
	{422, "unprocessable_entity", regexp.MustCompile(`(?i)(unprocessable\s+entity|schema\s+validation|实体无法处理)`)},
	{408, "request_timeout", regexp.MustCompile(`(?i)(request\s+timeout|请求\s*超时)`)},
	{451, "legal_restriction", regexp.MustCompile(`(?i)(unavailable\s+for\s+legal\s+reasons|export\s+control|sanctions?|法律原因不可用|合规限制|出口管制)`)},
	{503, "service_unavailable", regexp.MustCompile(`(?i)(service\s+unavailable|overloaded|server\s+is\s+busy|try\s+again\s+later|temporarily\s+unavailable|maintenance|UNAVAILABLE|ServiceUnavailable|Error\s*521|服务不可用|过载|系统繁忙|维护中)`)},
	{504, "gateway_timeout", regexp.MustCompile(`(?i)(gateway\s+timeout|DEADLINE_EXCEEDED|Error\s*522|Error\s*524|网关超时|上游超时)`)},
	{500, "internal_server_error", regexp.MustCompile(`(?i)(internal\s+server\s+error|InternalServerException|内部错误|服务器错误)`)},
	{400, "bad_request", regexp.MustCompile(`(?i)(bad\s+request|INVALID_ARGUMENT|cyber_policy|flagged\s+for\s+possible\s+cybersecurity\s+risk|json\s+parse|invalid\s+json|unexpected\s+token|无效请求|格式错误|JSON\s*解析失败)`)},
}

// fake200StatusLineMatchers 从 HTTP 状态行（部分上游把状态行塞进 200 正文）取码。
var fake200StatusLine = regexp.MustCompile(`(?i)HTTP/\d(\.\d)?\s+(\d{3})`)

// structuredBadRequestTypes / Codes 对齐 Node 的结构化 400 判定。
var (
	structuredBadRequestTypes = map[string]struct{}{
		"bad_request_error": {}, "invalid_request": {}, "invalid_request_error": {},
	}
	structuredBadRequestCodes = map[string]struct{}{
		"context_length_exceeded": {}, "cyber_policy": {}, "invalid_prompt": {},
		"invalid_value": {}, "message_too_big": {}, "string_above_max_length": {},
		"unsupported_value": {},
	}
)

// Fake200Detector 实现 forward.BodyErrorDetector。
type Fake200Detector struct{}

// newFake200Detector 构造检测器。
func newFake200Detector() *Fake200Detector { return &Fake200Detector{} }

// Detect 判定正文是否属于「HTTP 200 但实为错误」。
//
// 返回值语义：ok 为假表示未命中（调用方按成功处理）；命中时 statusCode 是推断出的状态码
// （推断不出时为 502），message 是脱敏截断后的错误文案。
func (d *Fake200Detector) Detect(_ string, isSSE bool, body string) (int, string, bool) {
	text := stripBOMAndTrim(body)
	if text == "" {
		// 空正文属另一条判定（forward 的 EmptyResponse 分支），此处不重复定义。
		return 0, "", false
	}
	if isSSE {
		return d.detectSSE(text)
	}
	return d.detectText(text)
}

// detectText 处理非流式正文：HTML 文档、JSON 错误对象、OpenAI Responses 失败。
func (d *Fake200Detector) detectText(text string) (int, string, bool) {
	if isLikelyHTMLDocument(text) {
		return inferFake200Status(text), truncateFake200Detail(text[:min(len(text), 4096)]), true
	}
	if !strings.HasPrefix(text, "{") {
		// 数组与其它形状一律不猜（与 Node 一致：数组语义差异大，宁可漏）。
		return 0, "", false
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		return 0, "", false
	}
	return d.detectJSONObject(parsed)
}

// detectSSE 逐个 data: 事件看 JSON：任一带非空 error 或 Responses 失败信号即命中。
func (d *Fake200Detector) detectSSE(text string) (int, string, bool) {
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if !fake200SSEDataPrefix.MatchString(trimmed) {
			continue
		}
		payload := strings.TrimSpace(fake200SSEDataPrefix.ReplaceAllString(trimmed, ""))
		if payload == "" || payload == "[DONE]" || !strings.HasPrefix(payload, "{") {
			continue
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
			continue
		}
		if status, message, ok := d.detectJSONObject(parsed); ok {
			return status, message, true
		}
	}
	return 0, "", false
}

// detectJSONObject 对单个 JSON 对象做判定（非流式与 SSE 共用同一口径）。
func (d *Fake200Detector) detectJSONObject(parsed map[string]any) (int, string, bool) {
	if detail, ok := detectOpenAIResponsesFailed(parsed); ok {
		return inferFake200Status(mustJSON(parsed)), detail, true
	}
	raw := mustJSON(parsed)
	if value, ok := parsed["error"]; ok && hasNonEmptyValue(value) {
		if text, isString := value.(string); isString {
			return inferFake200Status(raw), truncateFake200Detail(text), true
		}
		if object, isObject := value.(map[string]any); isObject {
			if message, isString := object["message"].(string); isString && strings.TrimSpace(message) != "" {
				return inferFake200Status(raw), truncateFake200Detail(message), true
			}
		}
		return inferFake200Status(raw), "", true
	}
	return 0, "", false
}

// detectOpenAIResponsesFailed 复刻 detectOpenAIResponsesFailed 的判定。
func detectOpenAIResponsesFailed(obj map[string]any) (string, bool) {
	eventType := trimmedString(obj["type"])
	response := obj
	if nested, ok := obj["response"].(map[string]any); ok {
		response = nested
	}
	status := trimmedString(response["status"])
	object := trimmedString(response["object"])
	id := trimmedString(response["id"])

	looksLikeResponse := strings.HasPrefix(eventType, "response.") ||
		object == "response" || strings.HasPrefix(id, "resp_")
	if !looksLikeResponse || (eventType != "response.failed" && status != "failed") {
		return "", false
	}
	switch value := response["error"].(type) {
	case string:
		if strings.TrimSpace(value) != "" {
			return truncateFake200Detail(value), true
		}
	case map[string]any:
		if message, ok := value["message"].(string); ok && strings.TrimSpace(message) != "" {
			return truncateFake200Detail(message), true
		}
		if code, ok := value["code"].(string); ok && strings.TrimSpace(code) != "" {
			return truncateFake200Detail(code), true
		}
	}
	if message := trimmedString(obj["message"]); message != "" {
		return truncateFake200Detail(message), true
	}
	return "", true
}

// inferFake200Status 推断更贴近语义的状态码；推断不出时返回 502。
func inferFake200Status(text string) int {
	trimmed := stripBOMAndTrim(text)
	if trimmed == "" {
		return fake200DefaultStatus
	}
	limited := trimmed
	if len(limited) > fake200StatusInferenceMaxChars {
		limited = limited[:fake200StatusInferenceMaxChars]
	}
	if status, ok := inferStructuredStatus(limited); ok {
		return status
	}
	if matches := fake200StatusLine.FindStringSubmatch(limited); matches != nil {
		if code := parseStatusCode(matches[2]); code != 0 {
			return code
		}
	}
	for _, matcher := range fake200StatusMatchers {
		if matcher.re.MatchString(limited) {
			return matcher.statusCode
		}
	}
	return fake200DefaultStatus
}

// inferStructuredStatus 读错误 JSON 里明确携带的状态码（Node 的 inferStructuredErrorStatusCode）。
func inferStructuredStatus(text string) (int, bool) {
	if !strings.HasPrefix(text, "{") {
		return 0, false
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		return 0, false
	}
	response, _ := parsed["response"].(map[string]any)
	var responseError map[string]any
	if response != nil {
		responseError, _ = response["error"].(map[string]any)
	}
	parsedError, _ := parsed["error"].(map[string]any)
	containers := []map[string]any{parsed, parsedError, response, responseError}
	for _, container := range containers {
		if container == nil {
			continue
		}
		for _, field := range []string{"status", "status_code", "statusCode", "code"} {
			if code := toHTTPErrorStatus(container[field]); code != 0 {
				return code, true
			}
		}
	}
	for _, container := range containers {
		if container == nil {
			continue
		}
		if _, ok := structuredBadRequestTypes[marker(container["type"])]; ok {
			return 400, true
		}
		if _, ok := structuredBadRequestCodes[marker(container["code"])]; ok {
			return 400, true
		}
		if marker(container["status"]) == "invalid_argument" {
			return 400, true
		}
	}
	return 0, false
}

// toHTTPErrorStatus 把 400..599 的值规整为状态码；其它一律 0。
func toHTTPErrorStatus(value any) int {
	switch typed := value.(type) {
	case float64:
		return intOrZero(typed)
	case string:
		return parseStatusCode(strings.TrimSpace(typed))
	default:
		return 0
	}
}

func intOrZero(value float64) int {
	code := int(value)
	if float64(code) != value || code < 400 || code > 599 {
		return 0
	}
	return code
}

// parseStatusCode 只接受三位数且在 400..599 之间的码。
func parseStatusCode(raw string) int {
	if len(raw) != 3 {
		return 0
	}
	code := 0
	for _, char := range raw {
		if char < '0' || char > '9' {
			return 0
		}
		code = code*10 + int(char-'0')
	}
	if code < 400 || code > 599 {
		return 0
	}
	return code
}

// marker 归一化结构化错误标记（Node 的 normalizeStructuredErrorMarker）。
func marker(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(text))
}

// isLikelyHTMLDocument 复刻 Node 的保守 HTML 判定。
func isLikelyHTMLDocument(trimmed string) bool {
	if !strings.HasPrefix(trimmed, "<") {
		return false
	}
	head := trimmed
	if len(head) > fake200HTMLSniffMaxChars {
		head = head[:fake200HTMLSniffMaxChars]
	}
	return fake200HTMLDoctype.MatchString(head) || fake200HTMLTag.MatchString(head)
}

// hasNonEmptyValue 判定 error 字段是否「非空」。
//
// 与 Node 的 hasNonEmptyValue 同义：null/空串/空数组/空对象都算空。
func hasNonEmptyValue(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(typed) != ""
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	default:
		return true
	}
}

// stripBOMAndTrim 去 BOM 与首尾空白（Node 同样先剥一个 BOM 再 trimStart）。
func stripBOMAndTrim(text string) string {
	text = strings.TrimPrefix(text, "\uFEFF")
	return strings.TrimSpace(text)
}

// truncateFake200Detail 截断错误文案（超出部分以省略号收尾）。
//
// 有意不做 Node 的脱敏正则（密钥/JWT/邮箱）：本函数的结果只用于日志与错误文案，
// 而凭据脱敏属日志层职责；在此再写一遍会给每条错误正文加一串正则开销。
func truncateFake200Detail(text string) string {
	trimmed := strings.TrimSpace(text)
	if utf8.RuneCountInString(trimmed) <= fake200DetailMaxLen {
		return trimmed
	}
	runes := []rune(trimmed)
	return string(runes[:fake200DetailMaxLen]) + "…"
}

// mustJSON 把对象重新序列化，供状态推断的正则扫描使用（失败返回空串）。
func mustJSON(value map[string]any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(raw)
}

func trimmedString(value any) string {
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(text)
}
