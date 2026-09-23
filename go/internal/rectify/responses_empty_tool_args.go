package rectify

import (
	"bytes"
	"encoding/json"
)

// 本文件承载 native（同协议）路上 Responses 正文的一处定点归一：
// `input[]` 里 function_call 项的**空参数**（`"arguments":""`）改写为规范无参形态 `"arguments":"{}"`。
//
// 为什么是主动型：发送前直接归一，不带触发词、不带重试（同 StripBillingHeader / NormalizeResponseInput）。
//
// 为什么需要：native 路把客户端正文字节原样透传（调用方见 forward 的正文定稿处，`plan.Conversion == nil`
// 那一边），编解码器里本有的同名归一（convert/codec_responses.go 的 responsesFunctionCallItem）根本不跑。
// 而 codex 型上游用 `buger/jsonparser` 解析每个 function_call 的 arguments，空串一律回
// `failed to parse function call arguments`（生产：ollama.com/v1/responses 的 400）。
// 空参数改 `{}` 本就是 Responses 线的规范形态——跨协议路一直这么发，这里只是让两条路一致。
//
// 为什么按字节切片而不是「解码成 map 再编码」：正文除被改写那一处外必须逐字节不变——
// 键序、数字字面量、空白与转义写法都不许动，而 map 往返会重排键并把大整数变成 float64
// （同 dataplane 的 errorMessageSpan 的取舍）。

// NormalizeResponsesEmptyToolArgs 把正文里所有「顶层 input[] 的 function_call 项、arguments 为空串」
// 的值改写成 `{}`。无此类项时返回**原切片**（不重新序列化，零扰动）。
func NormalizeResponsesEmptyToolArgs(body []byte) []byte {
	spans := responsesEmptyArgsSpans(body)
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

// emptyArgsSpan 是正文里一处「值为空字符串」的两字节区间 [start, end)。
type emptyArgsSpan struct{ start, end int }

// responsesEmptyArgsSpans 用 JSON 词法游标找出所有待改写的值区间。
//
// 用游标而不是正则或 convert.Value 树：只有它能在**不改动其余字节**的前提下给出位置。
// 正文不是合法 JSON 时返回空（调用方原样透传，不新增失败模式）。
func responsesEmptyArgsSpans(body []byte) []emptyArgsSpan {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()

	// 对象帧额外记本层是不是 input[] 的一个元素：type 与 arguments 的先后顺序在客户端之间
	// 并不统一，故两者都先记下，等本层对象闭合时再一起判定（不依赖键序）。
	type frame struct {
		isObject  bool
		inputElem bool // 本层对象是顶层 input[] 的一个元素
		inputArr  bool // 本层数组是顶层对象的 input 成员
		expectKey bool
		key       string
		itemType  string
		hasType   bool
		emptyArgs *emptyArgsSpan
	}
	var stack []frame
	var spans []emptyArgsSpan

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
				isElem := len(stack) == 2 && !stack[1].isObject && stack[1].inputArr
				stack = append(stack, frame{isObject: true, inputElem: isElem, expectKey: true})
			case '[':
				isInput := len(stack) == 1 && stack[0].isObject && stack[0].key == "input"
				stack = append(stack, frame{inputArr: isInput})
			default: // '}' 或 ']'
				if len(stack) == 0 {
					continue
				}
				closed := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				if closed.isObject && closed.inputElem && closed.hasType &&
					closed.itemType == "function_call" && closed.emptyArgs != nil {
					spans = append(spans, *closed.emptyArgs)
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
			if !top.inputElem {
				continue
			}
			switch top.key {
			case "type":
				top.itemType = value
				top.hasType = true
			case "arguments":
				// 空串的值字面量只可能是 `""`：定位起始引号，再看它与 token 末尾是否恰好两字节。
				quote := bytes.IndexByte(body[before:after], '"')
				if quote >= 0 && int(after-before)-quote == 2 {
					top.emptyArgs = &emptyArgsSpan{start: int(before) + quote, end: int(after)}
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
