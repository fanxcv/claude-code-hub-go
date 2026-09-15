package rectify

import (
	"regexp"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// billingHeaderPattern 复刻 Node 的 /^\s*x-anthropic-billing-header\s*:/i。
var billingHeaderPattern = regexp.MustCompile(`(?i)^\s*x-anthropic-billing-header\s*:`)

// StripBillingHeader 复刻 rectifyBillingHeader（billing-header-rectifier.ts:31-80）。
//
// 为什么是主动型：Claude Code 客户端 v2.1.36+ 会把 `x-anthropic-billing-header: …` 作为
// text 块塞进 system，非原生上游（如 Bedrock）直接 400 拒收；Node 不等报错，发送前就剥离。
//
// 三态行为与 Node 逐条一致：system 缺失/null 不动；纯字符串命中则整体删除；
// 数组则只滤掉 type=="text" 且文本命中的块（其余块保留，即使过滤后为空数组也要写回）。
// 返回的字段直接进审计条目（removedCount / extractedValues）。
func StripBillingHeader(body *convert.Value) (map[string]any, bool) {
	if body == nil {
		return nil, false
	}
	system, hasSystem := body.Get("system")
	if !hasSystem || system == nil || system.IsNull() {
		return billingHeaderFields(0, nil), false
	}

	if raw, isString := system.String(); isString {
		if !billingHeaderPattern.MatchString(raw) {
			return billingHeaderFields(0, nil), false
		}
		body.Delete("system")
		return billingHeaderFields(1, []string{strings.TrimSpace(raw)}), true
	}

	if system.IsArray() {
		extracted := make([]string, 0, 1)
		kept := make([]*convert.Value, 0, system.Len())
		for _, block := range system.Items() {
			text, isTextBlock := billingHeaderText(block)
			if isTextBlock && billingHeaderPattern.MatchString(text) {
				extracted = append(extracted, strings.TrimSpace(text))
				continue
			}
			kept = append(kept, block)
		}
		if len(extracted) == 0 {
			return billingHeaderFields(0, nil), false
		}
		body.Set("system", convert.NewArray(kept...))
		return billingHeaderFields(len(extracted), extracted), true
	}

	// 其余类型（数字、布尔、对象）：Node 同样不动。
	return billingHeaderFields(0, nil), false
}

// billingHeaderText 取块的文本，仅当块是 type=="text" 且 text 为字符串时有效。
func billingHeaderText(block *convert.Value) (string, bool) {
	if block == nil || !block.IsObject() {
		return "", false
	}
	if blockType, _ := block.StringField("type"); blockType != "text" {
		return "", false
	}
	return block.StringField("text")
}

// billingHeaderFields 组审计字段；无命中时 extractedValues 保持空数组（与 Node 的 `[]` 同形）。
func billingHeaderFields(removedCount int, extracted []string) map[string]any {
	if extracted == nil {
		extracted = []string{}
	}
	return map[string]any{
		"removedCount":    removedCount,
		"extractedValues": extracted,
	}
}

// responses input 归一的四种动作与四种原始类型，取值与 Node 逐字一致。
const (
	ActionStringToArray           = "string_to_array"
	ActionObjectToArray           = "object_to_array"
	ActionEmptyStringToEmptyArray = "empty_string_to_empty_array"
	ActionPassthrough             = "passthrough"

	OriginalTypeString = "string"
	OriginalTypeObject = "object"
	OriginalTypeArray  = "array"
	OriginalTypeOther  = "other"
)

// NormalizeResponseInput 复刻 rectifyResponseInput（response-input-rectifier.ts:29-61）。
//
// 为什么是主动型：Responses API 的 `input` 允许字符串简写与单对象，而下游（格式判定、转换器）
// 只认数组；Node 在守卫链**之前**归一，使过滤器与转换器看到同一份形状。
//
// 这里用 map 而不是 convert.Value：本函数挂在守卫链的正文缝隙上（`guard.BodyAccess` 的
// JSON()/Store() 本来就是 map），换模型反而要引一层无谓转换。
// 返回的字段直接进审计条目（action / originalType）。
func NormalizeResponseInput(body map[string]any) (map[string]any, bool) {
	if body == nil {
		return responseInputFields(ActionPassthrough, OriginalTypeOther), false
	}

	input, present := body["input"]
	if !present {
		return responseInputFields(ActionPassthrough, OriginalTypeOther), false
	}

	switch typed := input.(type) {
	case []any:
		return responseInputFields(ActionPassthrough, OriginalTypeArray), false
	case string:
		if typed == "" {
			body["input"] = []any{}
			return responseInputFields(ActionEmptyStringToEmptyArray, OriginalTypeString), true
		}
		body["input"] = []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": typed},
				},
			},
		}
		return responseInputFields(ActionStringToArray, OriginalTypeString), true
	case map[string]any:
		// 单对象形态：MessageInput 带 role、ToolOutputsInput 带 type。
		if _, hasRole := typed["role"]; hasRole {
			body["input"] = []any{typed}
			return responseInputFields(ActionObjectToArray, OriginalTypeObject), true
		}
		if _, hasType := typed["type"]; hasType {
			body["input"] = []any{typed}
			return responseInputFields(ActionObjectToArray, OriginalTypeObject), true
		}
		return responseInputFields(ActionPassthrough, OriginalTypeObject), false
	default:
		// null / undefined / 其他：交给下游报错（Node 同样 pass-through）。
		return responseInputFields(ActionPassthrough, OriginalTypeOther), false
	}
}

// responseInputFields 组审计字段。
func responseInputFields(action, originalType string) map[string]any {
	return map[string]any{
		"action":       action,
		"originalType": originalType,
	}
}
