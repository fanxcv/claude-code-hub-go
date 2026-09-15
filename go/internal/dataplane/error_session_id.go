package dataplane

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// 本文件复刻 `src/app/v1/_lib/proxy/error-session-id.ts`：给 4xx/5xx 的 JSON 错误体挂上会话 id，
// 让客户端与排障者能顺着一条报错找到那次会话。
//
// 为什么按字节切片而不是「解码成 map 再编码」：Node 的实现是 parse + JSON.stringify，V8 的对象
// 保留键的插入顺序，于是上游错误体的键序原样保住；Go 的 map 编码会把键排序——那就不再是同一份
// 正文了。这里只换掉 error.message 的那个字符串字面量，其余字节逐字不动。

// sessionIDMarker 是「已挂过」的判据（Node 以同一子串判重，故不许改写它的拼写）。
const sessionIDMarker = "cch_session_id:"

// attachSessionIDToErrorMessage 复刻 attachSessionIdToErrorMessage。
//
// 无会话 id 时原样返回；已经带过标记时不重复追加（同一响应可能被内外两层各挂一次）。
func attachSessionIDToErrorMessage(sessionID, message string) string {
	if sessionID == "" {
		return message
	}
	if strings.Contains(message, sessionIDMarker) {
		return message
	}
	return message + " (cch_session_id: " + sessionID + ")"
}

// attachSessionIDToErrorBody 复刻 attachSessionIdToErrorResponse 的正文部分。
//
// 三个前置条件与 Node 逐条同判：有会话 id、状态码 >= 400、Content-Type 含 application/json。
// 正文不是合法的「error.message 为字符串」形状时一律原样返回（Node 的 try/catch 兜底语义）。
func attachSessionIDToErrorBody(sessionID string, status int, contentType string, body []byte) []byte {
	if sessionID == "" || status < 400 || len(body) == 0 {
		return body
	}
	if !strings.Contains(strings.ToLower(contentType), "application/json") {
		return body
	}
	// 全文必须是合法 JSON：Node 走 JSON.parse，截断/畸形的正文会抛错并原样返回。
	// 这道闸不能省——json.Decoder 在对截断的正文也会先吐出已读到的 token，
	// 只看 token 流会把 cch_session_id 拼进一份客户端根本解析不了的正文里。
	if !json.Valid(body) {
		return body
	}
	start, end, message, ok := errorMessageSpan(body)
	if !ok {
		return body
	}
	updated := attachSessionIDToErrorMessage(sessionID, message)
	if updated == message {
		return body
	}
	// 只对**新**文案做一次转义：原正文里那一段的转义写法保持不动。
	quoted, err := json.Marshal(updated)
	if err != nil {
		return body
	}
	out := make([]byte, 0, len(body)+len(quoted)-len(message))
	out = append(out, body[:start]...)
	out = append(out, quoted...)
	out = append(out, body[end:]...)
	return out
}

// errorMessageSpan 在正文里定位 `error.message` 那个字符串字面量的字节区间（含两侧引号）。
//
// 用 JSON 词法游标而不是正则：只有它知道「这个 message 字符串是不是 error 对象的那个值」——
// 正则会把 details 里同名的字段一起改掉。Node 用的是 JSON.parse，两种写法在合法正文上同解。
func errorMessageSpan(body []byte) (int, int, string, bool) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()

	// 每一层对象/数组一个栈帧；对象帧记住「刚读到的是哪个键」。
	type frame struct {
		isObject  bool
		expectKey bool
		key       string
	}
	var stack []frame

	for {
		before := decoder.InputOffset()
		token, err := decoder.Token()
		if err != nil {
			return 0, 0, "", false
		}
		after := decoder.InputOffset()

		switch value := token.(type) {
		case json.Delim:
			switch value {
			case '{':
				stack = append(stack, frame{isObject: true, expectKey: true})
			case '[':
				stack = append(stack, frame{})
			default: // '}' 或 ']'
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
			}
		case string:
			if len(stack) == 0 || !stack[len(stack)-1].isObject {
				continue
			}
			top := &stack[len(stack)-1]
			if top.expectKey {
				top.key = value
				top.expectKey = false
				continue
			}
			top.expectKey = true
			// 命中判据：外层对象的键是 error、内层对象的键是 message，且此刻读的是值。
			if len(stack) != 2 || !stack[0].isObject || stack[0].key != "error" || stack[1].key != "message" {
				continue
			}
			head := body[before:after]
			quote := bytes.IndexByte(head, '"')
			if quote < 0 {
				return 0, 0, "", false
			}
			return int(before) + quote, int(after), value, true
		default:
			// 数字/布尔/null 也是值：读完它下一个 token 又是键。
			if len(stack) > 0 && stack[len(stack)-1].isObject {
				stack[len(stack)-1].expectKey = true
			}
		}
	}
}

// clientRequestURL 给出客户端请求的绝对地址（会话详情 clientReqMeta 用）。
//
// 语义与 Node 的 `new URL(c.req.url)` 对齐：两者都是「服务进程收到的那个地址」。scheme 按
// 本连接是否 TLS 取，host 取 Host 头（反代改写过的就是改写后的值）——**不**去解释
// X-Forwarded-*，那是反代的原始视角，与本进程实际服务的东西不是一回事。
func clientRequestURL(request *http.Request) string {
	if request == nil {
		return ""
	}
	scheme := "http"
	if request.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + request.Host + request.URL.RequestURI()
}
