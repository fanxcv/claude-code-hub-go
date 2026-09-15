package responsefix

import (
	"bytes"
	"encoding/json"
	"strings"
)

// chatCompletionChunkMarker 是 `"chat.completion.chunk"` 的字节序列，用于解码前的预扫描。
var chatCompletionChunkMarker = []byte(`"chat.completion.chunk"`)

// filterInertChatCompletionChunks 过滤「惰性的 chat.completion.chunk 帧」。
//
// 场景：客户端要的是 OpenAI Responses 线，上游却往流里混进了 chat 线的空 chunk
// （`choices[].delta` 只有 role、content 为空、无 finish_reason、无 usage）。它们对
// responses 客户端毫无语义，却会让解析器把它当成未知事件。Node 在响应修复器里把它删掉。
//
// 判据（与 Node 的 isInertChatCompletionChunkPayload 逐条对齐）：
//
//	object == "chat.completion.chunk" 且 usage 无实质内容 且 choices 非空
//	且每个 choice 都惰性：finish_reason 为空，且 delta 里除 role 外无实质字段。
//
// 「有实质」的定义同样是 Node 的 hasMeaningfulValue：非空字符串、非空数组、非空对象、
// 其余非 null 值（数字/布尔）都算。
//
// 预扫描是性能护栏：绝大多数块不含该标记，字节比较即可早退，省掉整块解码（含 CJK 的块
// 解码后还要再编码回字节）。
func filterInertChatCompletionChunks(data []byte) Result {
	if !bytes.Contains(data, chatCompletionChunkMarker) {
		return Result{Data: data}
	}

	lines := strings.Split(string(data), "\n")
	out := make([]string, 0, len(lines))
	applied := false
	skipNextBlank := false

	for i, line := range lines {
		hasLineBreak := i < len(lines)-1

		if skipNextBlank && isBlankSSESeparatorLine(line) {
			skipNextBlank = false
			continue
		}
		skipNextBlank = false

		if isInertChatCompletionDataLine(line) {
			applied = true
			// 被删掉的帧后面那个空行（事件分隔符）也要一起删，否则会多出一个空事件。
			skipNextBlank = true
			continue
		}

		out = append(out, line)
		if hasLineBreak {
			out = append(out, "\n")
		}
	}

	if !applied {
		return Result{Data: data}
	}
	return Result{
		Data:    []byte(strings.Join(out, "")),
		Applied: true,
		Details: detailFilteredInertCompletion,
	}
}

// isInertChatCompletionDataLine 判定单行 `data:` 载荷是否为惰性 chat chunk。
func isInertChatCompletionDataLine(line string) bool {
	if !strings.HasPrefix(line, "data:") {
		return false
	}
	payload := line[len("data:"):]
	if strings.HasPrefix(payload, " ") {
		payload = payload[1:]
	}
	if !strings.HasPrefix(payload, "{") {
		return false
	}
	var decoded any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		return false
	}
	return isInertChatCompletionChunkPayload(decoded)
}

// isInertChatCompletionChunkPayload 判定载荷是否为惰性 chunk。
func isInertChatCompletionChunkPayload(payload any) bool {
	record, ok := payload.(map[string]any)
	if !ok {
		return false
	}
	if object, _ := record["object"].(string); object != "chat.completion.chunk" {
		return false
	}
	if hasMeaningfulJSONValue(record["usage"]) {
		return false
	}
	choices, ok := record["choices"].([]any)
	if !ok || len(choices) == 0 {
		return false
	}
	for _, choice := range choices {
		if !isInertChatCompletionChoice(choice) {
			return false
		}
	}
	return true
}

// isInertChatCompletionChoice 判定单个 choice 是否惰性。
func isInertChatCompletionChoice(choice any) bool {
	record, ok := choice.(map[string]any)
	if !ok {
		return false
	}
	// finish_reason 存在且非 null ⇒ 这是一帧收尾信息，必须保留。
	if finish, exists := record["finish_reason"]; exists && finish != nil {
		return false
	}
	delta, exists := record["delta"]
	if !exists {
		return true
	}
	deltaRecord, ok := delta.(map[string]any)
	if !ok {
		return true
	}
	for key, value := range deltaRecord {
		if key == "role" {
			continue
		}
		if hasMeaningfulJSONValue(value) {
			return false
		}
	}
	return true
}

// hasMeaningfulJSONValue 对应 Node 的 hasMeaningfulValue。
func hasMeaningfulJSONValue(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case string:
		return len(typed) > 0
	case []any:
		return len(typed) > 0
	case map[string]any:
		return len(typed) > 0
	default:
		return true
	}
}

// isBlankSSESeparatorLine 判定 SSE 事件分隔空行（LF 或裸 CR 留下的 `\r` 行）。
func isBlankSSESeparatorLine(line string) bool {
	return line == "" || line == "\r"
}
