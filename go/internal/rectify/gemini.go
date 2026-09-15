package rectify

import (
	"regexp"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// triggerUnknownFunctionIDField 是 gemini function id 整流器的唯一触发类型。
const triggerUnknownFunctionIDField = "unknown_function_id_field"

// geminiIDViolationPattern 复刻 Node 的 ID_VIOLATION_PATTERN（gemini-function-id-rectifier.ts:44-45）。
//
// `\\*` 允许 id 两侧出现任意个反斜杠：转发链路常把上游 JSON 原样拼进错误消息，
// 内层引号处于 JSON 转义形态，裸引号正则会漏检。
var geminiIDViolationPattern = regexp.MustCompile(`unknown name \\*"id\\*" at\s+(?:'([^':\n]+)'|([^\s':\n]+))`)

// geminiIndexSuffix 剥离路径段尾部的 `[数字]`，对应 Node 的 `segment.replace(/\[\d+\]$/, "")`。
var geminiIndexSuffix = regexp.MustCompile(`\[\d+\]$`)

// geminiFunctionFieldSegments 是函数字段的路径段全名（小写化后）。
//
// 必须**整段精确**匹配而非子串包含：`tool_config.function_calling_config` 这类真实路径含
// `function_call` 子串，子串匹配会把无关违规误判成函数字段 id 违规。
var geminiFunctionFieldSegments = map[string]bool{
	"function_call":     true,
	"function_response": true,
	"functioncall":      true,
	"functionresponse":  true,
}

// detectGeminiFunctionID 复刻 detectGeminiFunctionIdRectifierTrigger
// （gemini-function-id-rectifier.ts:48-66）：id 违规必须与其自身路径里的函数字段绑定判断。
func detectGeminiFunctionID(errorMessage string) string {
	if errorMessage == "" {
		return ""
	}
	lower := strings.ToLower(errorMessage)

	for _, match := range geminiIDViolationPattern.FindAllStringSubmatch(lower, -1) {
		path := ""
		if match[1] != "" {
			path = match[1]
		} else if len(match) > 2 {
			path = match[2]
		}
		if path == "" {
			continue
		}
		for _, segment := range strings.Split(path, ".") {
			if geminiFunctionFieldSegments[geminiIndexSuffix.ReplaceAllString(segment, "")] {
				return triggerUnknownFunctionIDField
			}
		}
	}
	return ""
}

// rectifyGeminiFunctionIDs 复刻 rectifyGeminiFunctionIds
// （gemini-function-id-rectifier.ts:100-171）：删 contents[].parts[] 里的
// functionCall.id / functionResponse.id，兼容 camelCase 与 snake_case 两种键名，
// 并同时兼容顶层 `contents` 与 gemini-cli 的 `request.contents` 两种请求形状。
func rectifyGeminiFunctionIDs(body *convert.Value) (map[string]any, bool) {
	callIDs, responseIDs := stripFunctionIDs(body.ArrayField("contents"))

	if wrapped := body.ObjectField("request"); wrapped != nil {
		wrappedCalls, wrappedResponses := stripFunctionIDs(wrapped.ArrayField("contents"))
		callIDs += wrappedCalls
		responseIDs += wrappedResponses
	}

	fields := map[string]any{
		"strippedFunctionCallIds":     callIDs,
		"strippedFunctionResponseIds": responseIDs,
	}
	return fields, callIDs > 0 || responseIDs > 0
}

// stripFunctionIDs 删掉一个 contents 数组里所有函数调用/响应的 id 字段，返回两个独立计数。
//
// 两个计数**不可合并**：审计条目里 strippedFunctionCallIds 与 strippedFunctionResponseIds
// 是两个字段（Node 同样分开计数）。
func stripFunctionIDs(contents []*convert.Value) (int, int) {
	callIDs := 0
	responseIDs := 0
	for _, content := range contents {
		if content == nil || !content.IsObject() {
			continue
		}
		for _, part := range content.ArrayField("parts") {
			if part == nil || !part.IsObject() {
				continue
			}
			for _, key := range []string{"functionCall", "function_call"} {
				callIDs += deleteIDField(part.ObjectField(key))
			}
			for _, key := range []string{"functionResponse", "function_response"} {
				responseIDs += deleteIDField(part.ObjectField(key))
			}
		}
	}
	return callIDs, responseIDs
}

// deleteIDField 删一个函数块上的 id 字段；其余字段（如 thoughtSignature）原样保留。
func deleteIDField(block *convert.Value) int {
	if block == nil || !block.Has("id") {
		return 0
	}
	block.Delete("id")
	return 1
}
