package rectify

import (
	"bytes"
	"encoding/json"
)

// 本文件承载 native（同协议）路上请求正文里 tool 调用**空参数**的一处定点归一：
// `"arguments":""` 改写为规范无参形态 `"arguments":"{}"`。两条线共用一份按 JSON 路径匹配的
// 词法扫描器（emptyStringSpansAt），只有目标路径不同：
//
//   - Responses 线：顶层 `input[]` 里 `type == "function_call"` 的项（NormalizeResponsesEmptyToolArgs）；
//   - Chat 线：`messages[].tool_calls[].function` 对象（NormalizeChatEmptyToolArgs）。
//
// 为什么是主动型：发送前直接归一，不带触发词、不带重试（同 StripBillingHeader / NormalizeResponseInput）。
//
// 为什么需要：native 路把客户端正文字节原样透传（调用方见 forward 的正文定稿处，`plan.Conversion == nil`
// 那一边），编解码器里本有的同名归一（convert/codec_responses.go 的 responsesFunctionCallItem、
// convert/codec_chat.go 的 chatRenderToolCall）根本不跑。而 codex 型上游用 `buger/jsonparser` 解析每个
// function_call 的 arguments，空串一律回 `failed to parse function call arguments`（生产：ollama.com/v1/responses
// 的 400）；chat 线同属一个缺口——空 arguments 并非所有上游都接受。
//
// 为什么按字节切片而不是「解码成 map 再编码」：正文除被改写那一处外必须逐字节不变——
// 键序、数字字面量、空白与转义写法都不许动，而 map 往返会重排键并把大整数变成 float64
// （同 dataplane 的 errorMessageSpan 的取舍）。

// NormalizeResponsesEmptyToolArgs 把正文里所有「顶层 input[] 的 function_call 项、arguments 为空串」
// 的值改写成 `{}`。无此类项时返回**原切片**（不重新序列化，零扰动）。
func NormalizeResponsesEmptyToolArgs(body []byte) []byte {
	return normalizeEmptyToolArgsAt(body, []string{"input", "*"}, "arguments", "function_call")
}

// NormalizeChatEmptyToolArgs 把正文里所有「messages[].tool_calls[].function 的 arguments 为空串」
// 的值改写成 `{}`。无此类项时返回**原切片**（不重新序列化，零扰动）。
//
// 只认 tool_calls[].function.arguments：本仓 chat 编解码器不认旧式 message.function_call
// （见 convert/codec_chat.go 的键白名单与解码），故不为它加分支。
func NormalizeChatEmptyToolArgs(body []byte) []byte {
	return normalizeEmptyToolArgsAt(body, []string{"messages", "*", "tool_calls", "*", "function"}, "arguments", "")
}

// normalizeEmptyToolArgsAt 是两条线共用的定点改写：把 emptyStringSpansAt 找出的每个空串区间替换成 `{}`。
func normalizeEmptyToolArgsAt(body []byte, path []string, key, requireType string) []byte {
	spans := emptyStringSpansAt(body, path, key, requireType)
	if len(spans) == 0 {
		return body
	}
	out := make([]byte, 0, len(body)+2*len(spans))
	previous := 0
	for _, span := range spans {
		out = append(out, body[previous:span.start]...)
		// 空串字面量恰好是两字节 `""`，替换成同样两字节的 `{}`：长度不变，其余字节不动。
		out = append(out, '"', '{', '}', '"')
		previous = span.end
	}
	return append(out, body[previous:]...)
}

// emptyStringSpan 是正文里一处「值为空字符串」的两字节区间 [start, end)。
type emptyStringSpan struct{ start, end int }

// emptyStringSpansAt 用 JSON 词法游标找出所有「路径恰为 path 的对象里、key 的值为空串」的值区间。
//
// path 是相对顶层对象的键路径，数组元素用 "*" 表示（responses：{"input","*"}；
// chat：{"messages","*","tool_calls","*","function"}）。判定用**整条路径完全相等**（长度也必须相等），
// 故不会误命中更深的同名路径（例如 response_format 里的同名 schema 字段）。
// requireType 非空时还要求该对象自身的 "type" 等于它（responses 传 "function_call"）；
// 键序无关：type 可能在 arguments 之后，故两者都先记下、在对象闭合时一并判定。
//
// 用游标而不是正则或 convert.Value 树：只有它能在**不改动其余字节**的前提下给出位置。
// 正文不是合法 JSON 时返回空（调用方原样透传，不新增失败模式）。
func emptyStringSpansAt(body []byte, path []string, key, requireType string) []emptyStringSpan {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()

	// 对象帧额外记下本层的 JSON 路径与 type/arguments 候选：type 与 arguments 的先后顺序在客户端
	// 之间并不统一，故两者都先记下，等本层对象闭合时再一起判定（不依赖键序）。
	type frame struct {
		isObject  bool
		path      []string
		expectKey bool
		key       string
		itemType  string
		hasType   bool
		emptyArgs *emptyStringSpan
	}
	var stack []frame
	var spans []emptyStringSpan

	// childPath 给出新开容器所处的路径：父对象取其当前键，父数组取 "*"；栈空即顶层（无路径）。
	childPath := func(parents []frame) []string {
		if len(parents) == 0 {
			return nil
		}
		parent := parents[len(parents)-1]
		step := "*"
		if parent.isObject {
			step = parent.key
		}
		return append(append(make([]string, 0, len(parent.path)+1), parent.path...), step)
	}

	for {
		before := decoder.InputOffset()
		token, err := decoder.Token()
		if err != nil {
			// 读到结尾（栈已空）则前面的判定都建立在**完整**正文上，可用；正文在中途断了的话
			// 一律不改——不新增失败模式（同 dataplane 的 errorMessageSpan 的 json.Valid 闸）。
			if len(stack) == 0 {
				return spans
			}
			return nil
		}
		after := decoder.InputOffset()

		switch value := token.(type) {
		case json.Delim:
			switch value {
			case '{':
				stack = append(stack, frame{isObject: true, path: childPath(stack), expectKey: true})
			case '[':
				stack = append(stack, frame{path: childPath(stack)})
			default: // '}' 或 ']'
				if len(stack) == 0 {
					continue
				}
				closed := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				if closed.isObject && pathEquals(closed.path, path) && closed.emptyArgs != nil &&
					(requireType == "" || (closed.hasType && closed.itemType == requireType)) {
					spans = append(spans, *closed.emptyArgs)
				}
				// 容器值读完后，父对象的下一个 token 又是键：不同步这一步，键会被当成值读，
				// 其后所有路径判定都会错位（旧实现按固定层深判定，掩盖了这个缺口）。
				if len(stack) > 0 && stack[len(stack)-1].isObject {
					stack[len(stack)-1].expectKey = true
				}
			}
		case string:
			if len(stack) == 0 {
				continue
			}
			top := &stack[len(stack)-1]
			if !top.isObject {
				continue
			}
			if top.expectKey {
				top.key = value
				top.expectKey = false
				continue
			}
			top.expectKey = true
			if !pathEquals(top.path, path) {
				continue
			}
			switch top.key {
			case "type":
				top.itemType = value
				top.hasType = true
			case key:
				// 空串的值字面量只可能是 `""`：定位起始引号，再看它与 token 末尾是否恰好两字节。
				quote := bytes.IndexByte(body[before:after], '"')
				if quote >= 0 && int(after-before)-quote == 2 {
					top.emptyArgs = &emptyStringSpan{start: int(before) + quote, end: int(after)}
				}
			}
		default:
			// 数字/布尔/null 也是值：读完它，本层的下一个 token 又是键。
			if len(stack) > 0 && stack[len(stack)-1].isObject {
				stack[len(stack)-1].expectKey = true
			}
		}
	}
}

// pathEquals 报告两条键路径是否完全相等（长度也必须相等）。
func pathEquals(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
