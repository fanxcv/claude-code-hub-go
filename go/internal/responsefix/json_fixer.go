package responsefix

import (
	"bytes"
	"encoding/json"
	"strings"
)

// JSONFixer 移植 Node 的 `JsonFixer`（json-fixer.ts）：把**被截断**的 JSON 补成合法 JSON。
//
// 典型来路：上游流被中途切断、上游按字节数截断正文、上游把 `max_tokens` 用尽但没关括号。
// 修复是逐字节的状态机（不是正则）：只有这样才能区分「字符串里的括号」与「结构括号」。
//
// 两条护栏与 Node 一致：
//
//   - maxDepth：嵌套超过上限即放弃（畸形输入可能让深度爆炸，修复本身也会退化成 O(n) 的栈操作）；
//   - maxSize：正文超过上限直接不修（避免为大正文做一遍复制 + 两次解析）。
//
// 修完必须**再校验一次**：补出来仍非法的（例如中途出现的非法 token）一律回退原字节，
// 绝不把改坏的正文交给客户端。
type JSONFixer struct {
	// MaxDepth 是允许的最大嵌套深度。
	MaxDepth int
	// MaxSize 是允许修复的最大字节数。
	MaxSize int
}

// CanFix 判定正文看起来像 JSON 对象或数组（Node 的 looksLikeJSON：跳过前导空白后的首字节）。
func (JSONFixer) CanFix(data []byte) bool {
	return looksLikeJSON(data)
}

// Fix 返回补全后的字节；无法安全修复时原样返回且 Applied=false。
func (f JSONFixer) Fix(data []byte) Result {
	// 与 Node 同序：先判大小再判形状。这里**不**对 MaxSize 做 `>0` 豁免——显式配 0
	// 在 Node 的展开语义里就是「所有非空正文都超限」，等价写法是全部不修。
	if len(data) > f.MaxSize {
		return Result{Data: data, Details: detailExceededMaxSize}
	}
	if !f.CanFix(data) {
		return Result{Data: data}
	}

	// 快速路径：本来就合法（按 WHATWG 宽松解码后的文本判定，与 Node 的 JSON.parse 同口径）。
	if jsonValidLenient(data) {
		return Result{Data: data}
	}

	repaired := f.repair(data)
	if repaired == nil {
		return Result{Data: data, Details: detailRepairFailed}
	}
	if jsonValidLenient(repaired) {
		return Result{Data: repaired, Applied: true}
	}
	return Result{Data: data, Details: detailValidateRepairedFailed}
}

// repair 复刻 Node 的私有 `repair`：返回补全后的字节，深度超限时返回 nil。
func (f JSONFixer) repair(data []byte) []byte {
	out := make([]byte, 0, len(data)+8)
	stack := make([]byte, 0, 8)

	inString := false
	escapeNext := false
	depth := 0

	for _, b := range data {
		if escapeNext {
			escapeNext = false
			out = append(out, b)
			continue
		}

		if inString && b == '\\' {
			escapeNext = true
			out = append(out, b)
			continue
		}

		if b == '"' {
			inString = !inString
			out = append(out, b)
			continue
		}

		if !inString {
			switch b {
			case '{':
				depth++
				if depth > f.maxDepth() {
					return nil
				}
				stack = append(stack, '}')
				out = append(out, b)
				continue
			case '[':
				depth++
				if depth > f.maxDepth() {
					return nil
				}
				stack = append(stack, ']')
				out = append(out, b)
				continue
			case '}':
				out = removeTrailingComma(out)
				if len(stack) > 0 && stack[len(stack)-1] == b {
					stack = stack[:len(stack)-1]
					if depth > 0 {
						depth--
					}
					out = append(out, b)
				}
				continue
			case ']':
				out = removeTrailingComma(out)
				if len(stack) > 0 && stack[len(stack)-1] == b {
					stack = stack[:len(stack)-1]
					if depth > 0 {
						depth--
					}
					out = append(out, b)
				}
				continue
			}
		}

		out = append(out, b)
	}

	// 末尾不完整的转义序列：去掉最后一个反斜杠。
	if escapeNext && len(out) > 0 {
		out = out[:len(out)-1]
	}

	// 闭合未关闭的字符串。
	if inString {
		out = append(out, '"')
	}

	out = removeTrailingComma(out)

	// 对象末尾冒号无值：补 null。
	if needsNullValue(out, stack) {
		out = append(out, 'n', 'u', 'l', 'l')
	}

	// 闭合所有未关闭结构。
	for len(stack) > 0 {
		out = removeTrailingComma(out)
		out = append(out, stack[len(stack)-1])
		stack = stack[:len(stack)-1]
	}

	return out
}

// maxDepth 取配置值；未配置时沿用 Node 的 200（Node 由调用方传入，Go 侧零值也要安全）。
func (f JSONFixer) maxDepth() int {
	if f.MaxDepth <= 0 {
		return DefaultConfig().MaxJSONDepth
	}
	return f.MaxDepth
}

// looksLikeJSON 跳过前导空白后要求首字节是 `{` 或 `[`。
func looksLikeJSON(data []byte) bool {
	for _, b := range data {
		if isWhitespaceByte(b) {
			continue
		}
		return b == '{' || b == '['
	}
	return false
}

// isWhitespaceByte 判定 JSON 允许的四处 ASCII 空白。
func isWhitespaceByte(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// removeTrailingComma 删掉末尾（跳过空白后的）多余逗号。
func removeTrailingComma(out []byte) []byte {
	idx := len(out) - 1
	for idx >= 0 && isWhitespaceByte(out[idx]) {
		idx--
	}
	if idx >= 0 && out[idx] == ',' {
		return out[:idx]
	}
	return out
}

// needsNullValue 判定「对象里冒号后直接结束」的情形（`{"key":` 需补 null 才能解析）。
func needsNullValue(out []byte, stack []byte) bool {
	if len(stack) == 0 || stack[len(stack)-1] != '}' {
		return false
	}
	idx := len(out) - 1
	for idx >= 0 && isWhitespaceByte(out[idx]) {
		idx--
	}
	return idx >= 0 && out[idx] == ':'
}

// jsonValidLenient 按 Node 的口径判合法性：先做宽松 UTF-8 解码（TextDecoder 的 fatal:false），
// 再交给 JSON 解析器。直接用原始字节判定会把「字符串里含非法 UTF-8」误判为不合法。
func jsonValidLenient(data []byte) bool {
	if json.Valid(data) {
		return true
	}
	if bytes.Equal(data, []byte(strings.ToValidUTF8(string(data), "\uFFFD"))) {
		return false
	}
	return json.Valid([]byte(strings.ToValidUTF8(string(data), "\uFFFD")))
}
