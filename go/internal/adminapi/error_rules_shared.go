package adminapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 lane A1-4（error-rules / request-filters）两个资源模块共用的入参工具、覆写响应
// 校验与错误规则判定。
//
// 为什么不做成公共包：两个模块同属 adminapi 包，而共享文件（deps/problem/auth/audit/
// invalidate/router）在 A0 已冻结；这些工具只服务本 lane，放自己的文件里最省事，也不给别的
// lane 制造耦合。
//
// 命名约定：本 lane 的标识符一律带 a14 前缀（或 errorRule / requestFilter 前缀）。五个 lane
// 并行往同包加文件，`parseBody`、`writeJSON` 这类自然名一定会撞符号。

// a14JSONShape 是 a14JSONField 支持的形状（对应 zod 侧各 schema 的一小撮）。
type a14JSONShape int

const (
	// a14ShapeAny 对应 z.unknown()：任何 JSON 值都放行。
	a14ShapeAny a14JSONShape = iota
	// a14ShapeObject 对应 z.record(z.string(), z.unknown())：必须是 JSON 对象（数组与 null 不是）。
	a14ShapeObject
	// a14ShapeIntArray 对应 z.array(z.number().int().positive())。
	a14ShapeIntArray
	// a14ShapeStringArray 对应 z.array(z.string().min(1))。
	a14ShapeStringArray
	// a14ShapeRecordArray 对应 z.array(z.record(z.string(), z.unknown()))。
	a14ShapeRecordArray
	// a14ShapeEnum 对应 z.enum([...])：取值必须落在 enumValues 里。
	a14ShapeEnum
)

// a14JSONField 读一个「任意 JSON / 可空 / 数组」字段。
//
// 返回 (原始 JSON, 是否出现)。出现且为 JSON null 时返回字面量 `null`——调用方据此决定写 SQL
// NULL（与 drizzle 的 jsonb 列一致，见 store.nullableJSON）。
//
// 校验失败会往 object 上记一条 invalid_type issue 并返回 (nil, false)：调用方一律先查
// object.issues0() 再继续，所以「失败」不需要单独的错误通道。
func a14JSONField(
	object *adminObject,
	key string,
	shape a14JSONShape,
	nullable bool,
	enumValues []string,
) (json.RawMessage, bool) {
	raw, present := object.Raw(key)
	if !present {
		return nil, false
	}
	expected := a14ShapeName(shape)
	if a14IsJSONNull(raw) {
		if nullable || shape == a14ShapeAny {
			return raw, true
		}
		object.fail([]any{key}, "invalid_type", adminTypeMessage(expected, raw))
		return nil, false
	}
	switch shape {
	case a14ShapeAny:
	case a14ShapeObject:
		if adminJSONTypeName(raw) != "object" {
			object.fail([]any{key}, "invalid_type", adminTypeMessage(expected, raw))
			return nil, false
		}
	case a14ShapeEnum:
		value, ok := a14JSONString(raw)
		if !ok {
			object.fail([]any{key}, "invalid_type", adminTypeMessage(expected, raw))
			return nil, false
		}
		matched := false
		for _, candidate := range enumValues {
			if value == candidate {
				matched = true
				break
			}
		}
		if !matched {
			object.fail([]any{key}, "invalid_enum_value",
				fmt.Sprintf("Invalid enum value. Expected %s, received '%s'",
					adminEnumList(enumValues), value))
			return nil, false
		}
	case a14ShapeIntArray:
		items, ok := a14JSONArray(raw)
		if !ok {
			object.fail([]any{key}, "invalid_type", adminTypeMessage(expected, raw))
			return nil, false
		}
		for index, item := range items {
			value, ok := a14JSONInt(item)
			if !ok || value <= 0 {
				object.fail([]any{key, index}, "invalid_type", adminTypeMessage("number", item))
				return nil, false
			}
		}
	case a14ShapeStringArray:
		items, ok := a14JSONArray(raw)
		if !ok {
			object.fail([]any{key}, "invalid_type", adminTypeMessage(expected, raw))
			return nil, false
		}
		for index, item := range items {
			value, ok := a14JSONString(item)
			if !ok {
				object.fail([]any{key, index}, "invalid_type", adminTypeMessage("string", item))
				return nil, false
			}
			if value == "" {
				object.fail([]any{key, index}, "too_small", "String must contain at least 1 character(s)")
				return nil, false
			}
		}
	case a14ShapeRecordArray:
		items, ok := a14JSONArray(raw)
		if !ok {
			object.fail([]any{key}, "invalid_type", adminTypeMessage(expected, raw))
			return nil, false
		}
		for index, item := range items {
			if adminJSONTypeName(item) != "object" {
				object.fail([]any{key, index}, "invalid_type", adminTypeMessage("object", item))
				return nil, false
			}
		}
	}
	return raw, true
}

// a14IntSpec 描述一个整数字段的约束（对应 zod 的一条链）。
type a14IntSpec struct {
	Required bool
	Nullable bool
	// Min / Max 为 nil 表示不检。
	Min *int
	Max *int
}

// a14IntField 读一个整数字段（z.number().int()）。
//
// 与字符串字段同一取舍：JSON null 必须显式判类型——json.Unmarshal 把 null 解进 float64 时是
// 静默无操作，只看 error 会把 `"priority": null` 当成 0 放行。
func a14IntField(object *adminObject, key string, spec a14IntSpec) (*int, bool) {
	raw, present := object.Raw(key)
	if !present {
		if spec.Required {
			object.fail([]any{key}, "invalid_type", "Required")
		}
		return nil, false
	}
	if a14IsJSONNull(raw) {
		if spec.Nullable {
			return nil, true
		}
		object.fail([]any{key}, "invalid_type", adminTypeMessage("number", raw))
		return nil, false
	}
	value, ok := a14JSONInt(raw)
	if !ok {
		object.fail([]any{key}, "invalid_type", adminTypeMessage("number", raw))
		return nil, false
	}
	if spec.Min != nil && value < *spec.Min {
		object.fail([]any{key}, "too_small",
			fmt.Sprintf("Number must be greater than or equal to %d", *spec.Min))
		return &value, true
	}
	if spec.Max != nil && value > *spec.Max {
		object.fail([]any{key}, "too_big",
			fmt.Sprintf("Number must be less than or equal to %d", *spec.Max))
		return &value, true
	}
	return &value, true
}

// a14ShapeName 给出形状在 zod 眼里的期望类型名（只用于报错文案）。
func a14ShapeName(shape a14JSONShape) string {
	switch shape {
	case a14ShapeObject:
		return "object"
	case a14ShapeIntArray, a14ShapeStringArray, a14ShapeRecordArray:
		return "array"
	case a14ShapeEnum:
		return "enum"
	default:
		return "unknown"
	}
}

// a14IsJSONNull 判断原始 JSON 是否为 null 字面量。
func a14IsJSONNull(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

// a14JSONString 取字符串值；非字符串返回 (\"\", false)。
func a14JSONString(raw json.RawMessage) (string, bool) {
	if adminJSONTypeName(raw) != "string" {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

// a14JSONInt 取整数值（z.number().int()）：非数字、非整数、超出 int 范围一律失败。
func a14JSONInt(raw json.RawMessage) (int, bool) {
	if adminJSONTypeName(raw) != "number" {
		return 0, false
	}
	var parsed json.Number
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return 0, false
	}
	asFloat, err := parsed.Float64()
	if err != nil {
		return 0, false
	}
	if asFloat != float64(int64(asFloat)) {
		return 0, false
	}
	return int(asFloat), true
}

// a14JSONArray 取数组元素；非数组返回 (nil, false)。
func a14JSONArray(raw json.RawMessage) ([]json.RawMessage, bool) {
	if adminJSONTypeName(raw) != "array" {
		return nil, false
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, false
	}
	return items, true
}

// a14ObjectFields 把已读入的请求体转成对象字段表（与 adminReadJSONObject 的返回类型一致）。
func a14ObjectFields(fields map[string]json.RawMessage, allowed ...string) *adminObject {
	return adminNewObject(fields, allowed...)
}

// ---- 覆写响应校验（src/lib/error-override-validator.ts 的移植）----

// a14OverrideMaxBytes 与 error-override-validator.ts:17 的 MAX_OVERRIDE_RESPONSE_BYTES 一致。
const a14OverrideMaxBytes = 10 * 1024

// a14ValidateErrorOverrideResponse 复刻 validateErrorOverrideResponse：返回 "" 表示合法，否则
// 是 Node 的拒绝理由。
//
// 理由文案只在日志与测试里用：Node 侧 action 把 validationError 当 `error` 返回，handler 又
// 只按状态码作答（detail 是通用标题），故文案不进响应体。
func a14ValidateErrorOverrideResponse(raw json.RawMessage) string {
	if len(raw) == 0 || adminJSONTypeName(raw) != "object" {
		return "覆写响应必须是对象"
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return "覆写响应必须是对象"
	}

	errorObject, hasError := a14ObjectField(fields, "error")
	typeField, hasType := a14RawString(fields, "type")

	// detectErrorResponseFormat 的三条判定，顺序即优先级。
	//
	// 类型判定一律看**原始字节**（a14FieldTypeIs）：JS 的 typeof 判的是值的类型，不是它能否解成对象。
	switch {
	case hasType && typeField == "error" && hasError:
		return a14ValidateClaudeOverride(fields)
	case hasError:
		codeIsNumber := a14FieldTypeIs(errorObject, "code", "number")
		statusIsString := a14FieldTypeIs(errorObject, "status", "string")
		if codeIsNumber && statusIsString {
			return a14ValidateGeminiOverride(fields, errorObject)
		}
		typeIsString := a14FieldTypeIs(errorObject, "type", "string")
		messageIsString := a14FieldTypeIs(errorObject, "message", "string")
		if typeIsString && messageIsString {
			return a14ValidateOpenAIOverride(fields, errorObject)
		}
	}

	// 格式无法识别：Node 按「最像哪种残缺格式」给出更具体的理由，这里同样如此。
	if !hasError {
		return "覆写响应缺少 error 对象"
	}
	if hasType && typeField == "error" {
		return a14ValidateClaudeOverride(fields)
	}
	if a14FieldTypeIs(errorObject, "code", "number") || a14FieldTypeIs(errorObject, "status", "string") {
		return a14ValidateGeminiOverride(fields, errorObject)
	}
	if a14FieldTypeIs(errorObject, "type", "string") || a14FieldTypeIs(errorObject, "message", "string") {
		return a14ValidateOpenAIOverride(fields, errorObject)
	}
	return "覆写响应格式无法识别。支持 Claude 格式（需要 type: \"error\" 和 error.type）、" +
		"Gemini 格式（需要 error.code 和 error.status）或 OpenAI 格式（需要 error.type 和 error.message）"
}

// a14FieldTypeIs 判断对象字段的 JSON 类型是否为期望值（字段缺失即 false）。
func a14FieldTypeIs(fields map[string]json.RawMessage, key, expected string) bool {
	raw, present := fields[key]
	return present && adminJSONTypeName(raw) == expected
}

// a14ValidateClaudeOverride 复刻 validateClaudeFormat。
func a14ValidateClaudeOverride(fields map[string]json.RawMessage) string {
	typeRaw, hasType := fields["type"]
	if !hasType || adminJSONTypeName(typeRaw) != "string" {
		return "Claude 格式覆写响应缺少 type 字段"
	}
	typeValue, _ := a14JSONString(typeRaw)
	if strings.TrimSpace(typeValue) == "" {
		return "Claude 格式覆写响应缺少 type 字段"
	}
	if typeValue != "error" {
		return "Claude 格式覆写响应 type 字段必须为 \"error\""
	}
	errorObject, ok := a14ObjectField(fields, "error")
	if !ok {
		return "Claude 格式覆写响应缺少 error 对象"
	}
	errorTypeRaw, hasErrorType := errorObject["type"]
	if !hasErrorType || adminJSONTypeName(errorTypeRaw) != "string" {
		return "Claude 格式覆写响应 error.type 字段缺失或为空"
	}
	errorType, _ := a14JSONString(errorTypeRaw)
	if strings.TrimSpace(errorType) == "" {
		return "Claude 格式覆写响应 error.type 字段缺失或为空"
	}
	if messageRaw, ok := errorObject["message"]; !ok || adminJSONTypeName(messageRaw) != "string" {
		return "Claude 格式覆写响应 error.message 字段必须是字符串"
	}
	if requestID, ok := fields["request_id"]; ok && adminJSONTypeName(requestID) != "string" {
		return "Claude 格式覆写响应 request_id 字段必须是字符串"
	}
	return ""
}

// a14ValidateGeminiOverride 复刻 validateGeminiFormat。
func a14ValidateGeminiOverride(fields, errorObject map[string]json.RawMessage) string {
	if _, ok := a14ObjectField(fields, "error"); !ok {
		return "Gemini 格式覆写响应缺少 error 对象"
	}
	if codeRaw, ok := errorObject["code"]; !ok || adminJSONTypeName(codeRaw) != "number" {
		return "Gemini 格式覆写响应 error.code 字段必须是数字"
	}
	if messageRaw, ok := errorObject["message"]; !ok || adminJSONTypeName(messageRaw) != "string" {
		return "Gemini 格式覆写响应 error.message 字段必须是字符串"
	}
	statusRaw, ok := errorObject["status"]
	if !ok || adminJSONTypeName(statusRaw) != "string" {
		return "Gemini 格式覆写响应 error.status 字段缺失或为空"
	}
	status, _ := a14JSONString(statusRaw)
	if strings.TrimSpace(status) == "" {
		return "Gemini 格式覆写响应 error.status 字段缺失或为空"
	}
	if details, ok := errorObject["details"]; ok && adminJSONTypeName(details) != "array" {
		return "Gemini 格式覆写响应 error.details 字段必须是数组"
	}
	return ""
}

// a14ValidateOpenAIOverride 复刻 validateOpenAIFormat。
func a14ValidateOpenAIOverride(fields, errorObject map[string]json.RawMessage) string {
	if _, ok := a14ObjectField(fields, "error"); !ok {
		return "OpenAI 格式覆写响应缺少 error 对象"
	}
	typeRaw, ok := errorObject["type"]
	if !ok || adminJSONTypeName(typeRaw) != "string" {
		return "OpenAI 格式覆写响应 error.type 字段缺失或为空"
	}
	errorType, _ := a14JSONString(typeRaw)
	if strings.TrimSpace(errorType) == "" {
		return "OpenAI 格式覆写响应 error.type 字段缺失或为空"
	}
	if messageRaw, ok := errorObject["message"]; !ok || adminJSONTypeName(messageRaw) != "string" {
		return "OpenAI 格式覆写响应 error.message 字段必须是字符串"
	}
	if param, ok := errorObject["param"]; ok && !a14IsJSONNull(param) && adminJSONTypeName(param) != "string" {
		return "OpenAI 格式覆写响应 error.param 字段必须是字符串或 null"
	}
	if code, ok := errorObject["code"]; ok && !a14IsJSONNull(code) && adminJSONTypeName(code) != "string" {
		return "OpenAI 格式覆写响应 error.code 字段必须是字符串或 null"
	}
	return ""
}

// a14ObjectField 取一个 JSON 对象字段（排除 null 与数组，与 JS 的 typeof === "object" 判定等价）。
func a14ObjectField(fields map[string]json.RawMessage, key string) (map[string]json.RawMessage, bool) {
	raw, present := fields[key]
	if !present || adminJSONTypeName(raw) != "object" {
		return nil, false
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(raw, &nested); err != nil {
		return nil, false
	}
	return nested, true
}

// a14RawString 取字符串字段值；非字符串返回 ("", false)。
func a14RawString(fields map[string]json.RawMessage, key string) (string, bool) {
	raw, present := fields[key]
	if !present {
		return "", false
	}
	return a14JSONString(raw)
}

// a14OverrideSizeExceeded 复刻「响应体大小」这一段：JSON.stringify 后的字节数超过 10KB 即拒绝。
//
// 用不转义 HTML 的紧凑 JSON：Node 的 JSON.stringify 不转 < > &，Go 的默认编码会转，直接比长度
// 会在含这些字符的覆写体上提前触发。
func a14OverrideSizeExceeded(raw json.RawMessage) bool {
	buffer := &bytes.Buffer{}
	encoder := json.NewEncoder(buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(json.RawMessage(raw)); err != nil {
		return true
	}
	return len(bytes.TrimRight(buffer.Bytes(), "\n")) > a14OverrideMaxBytes
}

// a14ErrorRuleOverride 校验一行规则的 override_response：合法则返回原字节，否则返回 nil。
//
// 对应 Node 的 sanitizeOverrideResponse（repository 层，读的时候做）与 detector 装载时的
// isValidErrorOverrideResponse——两处都是「形状非法即当没有覆写」，但规则本身仍然参与匹配。
func a14ErrorRuleOverride(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || a14IsJSONNull(raw) {
		return nil
	}
	if a14ValidateErrorOverrideResponse(raw) != "" {
		return nil
	}
	if a14OverrideSizeExceeded(raw) {
		return nil
	}
	return raw
}

// ---- 错误规则判定（error-rule-detector 的装载与 detect 移植）----

// a14ErrorRuleEntry 是装载后的一条规则。
type a14ErrorRuleEntry struct {
	rule store.AdminErrorRule
	// lowered 是 contains / exact 用的小写样式。
	lowered string
	// compiled 是 regex 用的大小写不敏感编译结果。
	compiled *regexp.Regexp
	// override 是**校验通过**的覆写体；非法或缺失时为 nil（规则仍然参与匹配）。
	override json.RawMessage
	// overrideStatusCode 是规则的覆写状态码（nil 表示透传）。
	overrideStatusCode *int
}

// a14ErrorRuleIndex 是三张判定表，对应 detector 的 containsPatterns / exactPatterns /
// regexPatterns，以及 getStats 的三个计数。
type a14ErrorRuleIndex struct {
	contains []a14ErrorRuleEntry
	exact    []a14ErrorRuleEntry
	regex    []a14ErrorRuleEntry
}

// a14BuildErrorRuleIndex 复刻 detector 的装载：按 matchType 分组，regex 编译失败即丢弃该条。
//
// 差异（登记进对拍白名单）：Node 在装载期还用 safe-regex 挡 ReDoS 样式，Go 的 regexp 是 RE2、
// 无回溯爆炸，故只做「能否编译」的过滤。
func a14BuildErrorRuleIndex(rules []store.AdminErrorRule) a14ErrorRuleIndex {
	index := a14ErrorRuleIndex{}
	for _, rule := range rules {
		pattern := strings.TrimSpace(rule.Pattern)
		if pattern == "" {
			continue
		}
		entry := a14ErrorRuleEntry{
			rule:               rule,
			override:           a14ErrorRuleOverride(rule.OverrideResponse),
			overrideStatusCode: rule.OverrideStatusCode,
		}
		switch rule.MatchType {
		case "contains":
			entry.lowered = strings.ToLower(pattern)
			index.contains = append(index.contains, entry)
		case "exact":
			entry.lowered = strings.ToLower(pattern)
			index.exact = append(index.exact, entry)
		case "regex":
			compiled, err := regexp.Compile("(?i)" + rule.Pattern)
			if err != nil {
				// Node 侧同样只丢这一条（invalid regex 走 logger.error）。
				continue
			}
			entry.compiled = compiled
			index.regex = append(index.regex, entry)
		default:
			// 未知 matchType：Node 走 default 分支记警告并跳过，这里同样丢弃。
		}
	}
	return index
}

// detect 复刻 detector.detect 的判定顺序：contains -> exact -> regex，任一命中即返回。
//
// 顺序是可见行为：:test 返回的命中规则随之不同。
//
// 一处有意差异：命中「上游声明不支持该输入形态」那一族时，还要过一道瞬时措辞的否定判定
// （store.SuppressUnsupportedInputMatch），命中被作废后**继续往下扫**。数据面走的是同一道闸门
// （guard/adapters_rules.go 的 matchedCategories），:test 若不跟，就会向运营报告一条实际不会生效的命中。
func (index a14ErrorRuleIndex) detect(message string) (a14ErrorRuleEntry, bool) {
	if message == "" {
		return a14ErrorRuleEntry{}, false
	}
	lowered := strings.ToLower(message)
	suppressed := func(entry a14ErrorRuleEntry) bool {
		return store.SuppressUnsupportedInputMatch(entry.rule.Category, lowered)
	}
	for _, entry := range index.contains {
		if strings.Contains(lowered, entry.lowered) && !suppressed(entry) {
			return entry, true
		}
	}
	trimmed := strings.TrimSpace(lowered)
	for _, entry := range index.exact {
		if trimmed == entry.lowered && !suppressed(entry) {
			return entry, true
		}
	}
	for _, entry := range index.regex {
		if entry.compiled.MatchString(message) && !suppressed(entry) {
			return entry, true
		}
	}
	return a14ErrorRuleEntry{}, false
}

// stats 复刻 detector.getStats（lastReloadTime 由调用方填）。
func (index a14ErrorRuleIndex) stats(lastReloadTime int64) a14ErrorRuleStats {
	return a14ErrorRuleStats{
		RegexCount:     len(index.regex),
		ContainsCount:  len(index.contains),
		ExactCount:     len(index.exact),
		TotalCount:     len(index.regex) + len(index.contains) + len(index.exact),
		LastReloadTime: lastReloadTime,
		// Go 侧没有异步装载状态：装载是同步完成的，任何时刻都不处于「装载中」。
		IsLoading: false,
	}
}

// a14ErrorRuleStats 逐字对应 detector.getStats() 的返回形状。
type a14ErrorRuleStats struct {
	RegexCount     int   `json:"regexCount"`
	ContainsCount  int   `json:"containsCount"`
	ExactCount     int   `json:"exactCount"`
	TotalCount     int   `json:"totalCount"`
	LastReloadTime int64 `json:"lastReloadTime"`
	IsLoading      bool  `json:"isLoading"`
}
