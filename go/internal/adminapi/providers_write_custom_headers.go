package adminapi

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
)

// 本文件是 providers 写路径里 `provider.customHeaders`（列 `custom_headers`）的校验，逐条复刻
// Node 的 `normalizeCustomHeadersRecord`（src/lib/custom-headers.ts:33-91）与包装它的
// `PROVIDER_CUSTOM_HEADERS_SCHEMA`（src/lib/validation/schemas.ts:467-481）。
//
// **为什么必须在写入侧拒**：列是 jsonb，而数据面只在**施加时**剥离保留名与鉴权头
// （forward.applyProviderCustomHeaders）。若写入侧不校验，脏值（数组、非字符串、CRLF、伪装成
// 普通头的分帧头）会永久留在库里，界面还会把它显示成一份「可用配置」。
//
// 与 Node 的分工刻意保持一致：**validator 管形状**（本文件）、**apply 管保留名**（数据面）。
// 因此这里**不**拒绝 `host` / `content-length` 之类的保留名——Node 也不拒（它在施加时静默剥离），
// 我们若在写入侧拒就会与 Node 分叉（详见报告 §安全边界）。

// providerHeaderNamePattern 逐字对应 Node 的 HTTP_TOKEN_NAME_REGEX（src/lib/custom-headers.ts:21）。
var providerHeaderNamePattern = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

// Node 的自定义错误码（`CustomHeadersValidationErrorCode`）。`invalid_json` 不在其中：
// 那一码属 JSON 解析层，而本层拿到的已是解析后的字段原文。
const (
	providerCustomHeadersNotObject     = "not_object"
	providerCustomHeadersEmptyName     = "empty_name"
	providerCustomHeadersCRLF          = "crlf"
	providerCustomHeadersInvalidName   = "invalid_name"
	providerCustomHeadersProtectedName = "protected_name"
	providerCustomHeadersDuplicateName = "duplicate_name"
	providerCustomHeadersInvalidValue  = "invalid_value"
)

// providerCustomHeadersSpec 绑定 `custom_headers` 字段。
//
// 归一语义与 Node 逐条一致：
//   - 字段缺失 → 不下发（保留 `.optional()` 语义，既有行不被清空）；
//   - 显式 `null` → `null`（清空）；
//   - `{}` → `null`（Node 的 `names.length === 0 → value: null`）；
//   - 合法对象 → 原样下发（键序保持输入顺序，便于与 Node 逐字节对拍）。
func providerCustomHeadersSpec() providerDecodeSpec {
	return providerDecodeSpec{Decode: func(object *adminObject, payload string) (any, bool) {
		raw, present := object.Raw(payload)
		if !present {
			return nil, false
		}
		if adminJSONTypeName(raw) == "null" {
			return nil, true
		}
		normalized, code, offender := providerNormalizeCustomHeaders(raw)
		if code != "" {
			object.fail([]any{payload}, code, providerCustomHeadersErrorMessage(code, offender))
			return nil, false
		}
		return normalized, true
	}}
}

// providerCustomHeadersErrorMessage 沿用 Node 的前缀：其 zod 包装把码编进 message
// （`custom_headers_<code>`，validation/schemas.ts:478），前端按该前缀映射五语种文案。
// 这里额外在括号里附上命中的键名——否则服务端拒绝时无从判断是哪一条。
func providerCustomHeadersErrorMessage(code, offender string) string {
	if offender == "" {
		return "custom_headers_" + code
	}
	return "custom_headers_" + code + " (" + offender + ")"
}

// providerNormalizeCustomHeaders 复刻 normalizeCustomHeadersRecord：按**输入顺序**逐条校验，
// 返回归一化后的 JSON、失败码与命中的键名；code 为空表示通过（此时 normalized 为 nil 表示
// 「空对象 → 归一化为清空」）。
//
// 校验顺序与 Node 相同（先命中先返回）：空键 → 键含 CRLF → 键非 HTTP token → 键是鉴权头 →
// 键（小写）重复 → 值非字符串 → 值含 CRLF。
func providerNormalizeCustomHeaders(raw json.RawMessage) (json.RawMessage, string, string) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	open, err := decoder.Token()
	if err != nil || open != json.Delim('{') {
		return nil, providerCustomHeadersNotObject, ""
	}

	names := make([]string, 0, 8)
	values := make([]string, 0, 8)
	seen := make(map[string]bool, 8)

	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, providerCustomHeadersNotObject, ""
		}
		name, ok := token.(string)
		if !ok {
			return nil, providerCustomHeadersNotObject, ""
		}
		var rawValue json.RawMessage
		if err := decoder.Decode(&rawValue); err != nil {
			return nil, providerCustomHeadersNotObject, name
		}

		if name == "" || strings.TrimSpace(name) == "" {
			return nil, providerCustomHeadersEmptyName, name
		}
		if strings.ContainsAny(name, "\r\n") {
			return nil, providerCustomHeadersCRLF, name
		}
		if !providerHeaderNamePattern.MatchString(name) {
			return nil, providerCustomHeadersInvalidName, name
		}
		lower := strings.ToLower(name)
		// 鉴权头清单复用数据面的那一份（forward.ProtectedAuthHeaderNames，逐条对齐
		// src/lib/custom-headers.ts:23-27），避免第二份真相。
		if forward.ProtectedAuthHeaderNames[lower] {
			return nil, providerCustomHeadersProtectedName, name
		}
		if seen[lower] {
			return nil, providerCustomHeadersDuplicateName, name
		}
		seen[lower] = true

		// Node 的判据是 `typeof value !== "string"`。注意不能只靠 json.Unmarshal 进 string：
		// Go 对 JSON `null` 解到 string 是**无操作且成功**，会把 `{"x":null}` 当成空串放过，
		// 而 Node 会判 invalid_value。故先按 JSON 类型名卡一道。
		if adminJSONTypeName(rawValue) != "string" {
			return nil, providerCustomHeadersInvalidValue, name
		}
		var value string
		if err := json.Unmarshal(rawValue, &value); err != nil {
			return nil, providerCustomHeadersInvalidValue, name
		}
		if strings.ContainsAny(value, "\r\n") {
			return nil, providerCustomHeadersCRLF, name
		}
		names = append(names, name)
		values = append(values, value)
	}

	if len(names) == 0 {
		return nil, "", ""
	}

	var buffer bytes.Buffer
	buffer.WriteByte('{')
	for index, name := range names {
		if index > 0 {
			buffer.WriteByte(',')
		}
		buffer.Write(providerEncodeJSONString(name))
		buffer.WriteByte(':')
		buffer.Write(providerEncodeJSONString(values[index]))
	}
	buffer.WriteByte('}')
	return json.RawMessage(buffer.Bytes()), "", ""
}

// providerEncodeJSONString 编码一个 JSON 字符串且**不做 HTML 转义**：Node 的 JSON.stringify
// 不会把 `<`/`>`/`&` 写成 \u003c 一类，两侧要逐字节可比就必须关掉 Go 的默认转义。
func providerEncodeJSONString(value string) []byte {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	// Encode 总会追加换行；只在出错时退化为显式空串（键/值已过校验，实际不会走到）。
	if err := encoder.Encode(value); err != nil {
		return []byte(`""`)
	}
	return bytes.TrimRight(buffer.Bytes(), "\n")
}
