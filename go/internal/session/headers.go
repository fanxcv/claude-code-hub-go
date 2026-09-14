package session

import (
	"strings"
)

// 本文件复刻 Node 的两条**脱敏序列化**入口（详情面的 headers 与上游 URL 都经它们落盘）。
//
// 唯一真源：src/app/v1/_lib/proxy/errors.ts 的 sanitizeHeaders（:1327-1377）、
// maskSensitiveValue（:1281-1289）、maskAuthorizationValue（:1299-1316）、
// sanitizeUrl（:1379-1422）与 headersToSanitizedObject（session-manager.ts:397-420）。
//
// 为什么必须逐字复刻而不是「差不多」：这两条入口是**隐私边界**。近似实现（例如用 Go 的
// url.QueryEscape 替 JS 的 encodeURIComponent）会把 `+`、`!`、`'`、`(`、`)` 这些字符编成
// 不同的转义序列，于是「同一个 URL」在两侧落盘不同；更坏的方向是漏掉某个敏感键名，把密钥
// 原样写进 Redis。故这里按 JS 的**字符集规则**实现，而不是按 Go 的标准库习惯。

// sensitiveHeaderNames 是 SENSITIVE_HEADERS（errors.ts:1233-1243）。
var sensitiveHeaderNames = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"x-api-key":           true,
	"api-key":             true,
	"anthropic-api-key":   true,
	"x-goog-api-key":      true,
	"x-auth-token":        true,
	"cookie":              true,
	"set-cookie":          true,
}

// reservedInternalHeaderNames 是 RESERVED_INTERNAL_HEADERS 的小写形式
// （responses-ws/internal-secret.ts:43-48）。这些是进程内隧道的标记头，不应外泄给会话详情。
var reservedInternalHeaderNames = map[string]bool{
	"x-cch-client-transport":     true,
	"x-cch-responses-ws-forward": true,
	"x-cch-responses-ws-session": true,
	"x-cch-internal-secret":      true,
}

// 遮罩常量（errors.ts:1269-1273）：保留前后 4 字符，短于 8 字符完全遮罩。
const (
	maskPrefixLength = 4
	maskSuffixLength = 4
	maskMinLength    = 8
)

// sensitiveURLParams 是 SENSITIVE_URL_PARAMS（errors.ts:1252-1264）的小写形式。
//
// 注意 apiKey 在 Node 侧是**驼峰**写入 Set，与 query 参数比对时用的是 `key.toLowerCase()`，
// 故这里的键必须是小写——驼峰那条在 Node 侧其实永远命中不了（`apiKey` 的 lower 是 `apikey`，
// 已被另一条覆盖），本表按同一套小写判定即可。
var sensitiveURLParams = map[string]bool{
	"key":           true,
	"api_key":       true,
	"api-key":       true,
	"apikey":        true,
	"token":         true,
	"access_token":  true,
	"auth_token":    true,
	"secret":        true,
	"client_secret": true,
	"password":      true,
}

// SanitizeHeaders 复刻 headersToSanitizedObject：脱敏后逐行解析成 name → value 的表。
//
// 为什么先成串再解析：Node 就是这么做的（sanitizeHeaders 出文本，headersToSanitizedObject 再
// 拆行建对象），重复名会以 `\n` 连接。跳掉这一步会让重复头的行为（后者覆盖前者还是合并）
// 与 Node 分叉。空结果返回**空表而不是 nil**：Node 返回 `{}`，JSON 是 `{}`。
func SanitizeHeaders(headers map[string][]string) map[string]string {
	text := SanitizeHeadersText(headers)
	result := map[string]string{}
	if text == "" || text == "(empty)" {
		return result
	}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if line == "" {
			continue
		}
		colon := strings.Index(line, ":")
		if colon == -1 {
			continue
		}
		name := strings.TrimSpace(line[:colon])
		value := strings.TrimSpace(line[colon+1:])
		if name == "" {
			continue
		}
		if existing, ok := result[name]; ok {
			result[name] = existing + "\n" + value
			continue
		}
		result[name] = value
	}
	return result
}

// SanitizeHeadersText 复刻 sanitizeHeaders 的 Headers 分支：逐项脱敏后以 `\n` 连接。
//
// 取值用**原始大小写**的头名（Node 的 Headers.forEach 交出的 key 是原始名），命中判定用
// 小写。保留内部头（x-cch-*）整体剔除；敏感头的值按 maskAuthorizationValue /
// maskSensitiveValue 遮罩。
//
// 无任何头时返回字面量 "(empty)"（Node 同），这是调用方判「空」的约定。
func SanitizeHeadersText(headers map[string][]string) string {
	collected := make([]string, 0, len(headers))
	// Go 的 map 无序，而 Node 的 Headers 保持插入序。这里按**头名排序**固定顺序：
	// 顺序对读侧无意义（键集与值才是），而稳定顺序让测试可断言、比对可复现。
	for _, name := range sortedHeaderNames(headers) {
		lower := strings.ToLower(name)
		if reservedInternalHeaderNames[lower] {
			continue
		}
		for _, value := range headers[name] {
			if sensitiveHeaderNames[lower] {
				if lower == "authorization" {
					collected = append(collected, name+": "+maskAuthorizationValue(value))
				} else {
					collected = append(collected, name+": "+maskSensitiveValue(value))
				}
				continue
			}
			collected = append(collected, name+": "+value)
		}
	}
	if len(collected) == 0 {
		return "(empty)"
	}
	return strings.Join(collected, "\n")
}

// maskSensitiveValue 复刻 maskSensitiveValue。
//
// 长度按 **JS 字符数**（UTF-16 码元）而不是字节数——Go 的 rune 计数与 JS 的 .length 在
// 增补平面字符上不同（一个 emoji 在 JS 是 2、在 Go 是 1 个 rune）。故这里按 UTF-16 码元计数，
// 切片也按码元边界（Node 的 slice 是码元切片，切在代理对中间会产生半个字符——为与 Node 同形，
// 这里同样按码元切）。
func maskSensitiveValue(value string) string {
	trimmed := strings.TrimSpace(value)
	units := utf16Units(trimmed)
	if len(units) <= maskMinLength {
		return redactedMarker
	}
	return utf16Slice(units, 0, maskPrefixLength) + "******" +
		utf16Slice(units, len(units)-maskSuffixLength, len(units))
}

// maskAuthorizationValue 复刻 maskAuthorizationValue：Bearer 保留前缀、Basic 全遮。
func maskAuthorizationValue(value string) string {
	trimmed := strings.TrimSpace(value)
	if token, ok := bearerToken(trimmed); ok {
		return "Bearer " + maskSensitiveValue(token)
	}
	if basicToken(trimmed) {
		return "Basic [REDACTED]"
	}
	return maskSensitiveValue(trimmed)
}

// bearerToken 复刻 `/^Bearer\s+(.+)$/i` 并取捕获组的 trim。
//
// 正则的 `.+` 不匹配换行，故 token 内出现换行时整体不匹配（回落成 maskSensitiveValue）。
func bearerToken(value string) (string, bool) {
	if len(value) < len("Bearer ") {
		return "", false
	}
	if !strings.EqualFold(value[:len("Bearer")], "Bearer") {
		return "", false
	}
	rest := value[len("Bearer"):]
	if rest == "" || !isJSSpace(rest[0]) {
		return "", false
	}
	// JS 的 `\s` 含换行；`\s+` 是贪婪的，故跨过全部空白后取剩余部分。
	index := 0
	for index < len(rest) && isJSSpace(rest[index]) {
		index++
	}
	token := rest[index:]
	if token == "" || strings.ContainsAny(token, "\n\r") {
		return "", false
	}
	return strings.TrimSpace(token), true
}

// basicToken 复刻 `/^Basic\s+(.+)$/i`。
func basicToken(value string) bool {
	if len(value) < len("Basic") || !strings.EqualFold(value[:len("Basic")], "Basic") {
		return false
	}
	rest := value[len("Basic"):]
	if rest == "" || !isJSSpace(rest[0]) {
		return false
	}
	index := 0
	for index < len(rest) && isJSSpace(rest[index]) {
		index++
	}
	token := rest[index:]
	return token != "" && !strings.ContainsAny(token, "\n\r")
}

// isJSSpace 判定 JS 正则 `\s` 覆盖的 ASCII 空白（含换行/回车/制表/换页/垂直制表）。
func isJSSpace(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	default:
		return false
	}
}

// SanitizeURL 复刻 sanitizeUrl：只替换**查询参数**里的敏感值，其余原样保留。
//
// 三处必须逐字对齐的地方：
//  1. 解析失败时按**相对路径**处理（`new URL(url, "http://localhost")`），输出丢弃源站；
//  2. 查询串的重新编码：键与值都过 encodeURIComponent（不是 Go 的 QueryEscape），
//     故 `!`、`'`、`(`、`)`、`*`、`~` 不转义而空格编成 `%20`；
//  3. 空前缀的兜底文案是字面量 `(empty url)`。
func SanitizeURL(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "(empty url)"
	}
	scheme, rest, ok := splitScheme(raw)
	if !ok {
		// 解析失败：按相对路径处理，输出 path+search+hash。
		path, query, fragment := splitURLParts(raw)
		return path + renderSanitizedQuery(query) + fragment
	}
	// origin = scheme://host（host 含端口与用户信息，Node 的 URL.origin 不含用户信息，
	// 这里按 Node 的 origin 语义去掉 userinfo）。
	authority, tail := splitAuthority(rest)
	host := stripUserInfo(authority)
	path, query, fragment := splitURLParts(tail)
	if path == "" {
		path = "/"
	}
	return scheme + "://" + host + path + renderSanitizedQuery(query) + fragment
}

// splitScheme 拆出 `scheme://` 前缀（大小写不敏感，但原样保留）。
func splitScheme(raw string) (string, string, bool) {
	index := strings.Index(raw, "://")
	if index <= 0 {
		return "", "", false
	}
	return raw[:index], raw[index+3:], true
}

// splitAuthority 把 `host[:port]/rest` 拆成 authority 与其余部分。
func splitAuthority(rest string) (string, string) {
	index := strings.IndexAny(rest, "/?#")
	if index == -1 {
		return rest, ""
	}
	return rest[:index], rest[index:]
}

// stripUserInfo 去掉 authority 里的 `user:pass@` 前缀（Node 的 URL.origin 不含凭据）。
func stripUserInfo(authority string) string {
	index := strings.LastIndex(authority, "@")
	if index == -1 {
		return authority
	}
	return authority[index+1:]
}

// splitURLParts 把 `path?query#fragment` 拆成三段（query 不含 `?`，fragment 含 `#`）。
func splitURLParts(value string) (string, string, string) {
	fragment := ""
	if index := strings.Index(value, "#"); index != -1 {
		fragment = value[index:]
		value = value[:index]
	}
	query := ""
	if index := strings.Index(value, "?"); index != -1 {
		query = value[index+1:]
		value = value[:index]
	}
	return value, query, fragment
}

// renderSanitizedQuery 复刻 `new URLSearchParams(...)` 的迭代 + encodeURIComponent 重编码。
//
// 迭代语义照 URLSearchParams：以 `&` 分段，段内首个 `=` 之前是键、之后是值（无 `=` 时值为
// 空串），`+` 解成空格，百分号转义按 UTF-8 解码（非法转义原样保留）。
func renderSanitizedQuery(query string) string {
	if query == "" {
		return ""
	}
	parts := make([]string, 0, 8)
	for _, segment := range strings.Split(query, "&") {
		if segment == "" {
			continue
		}
		key := segment
		value := ""
		if index := strings.Index(segment, "="); index != -1 {
			key = segment[:index]
			value = segment[index+1:]
		}
		decodedKey := decodeQueryComponent(key)
		if sensitiveURLParams[strings.ToLower(decodedKey)] {
			parts = append(parts, encodeURIComponent(decodedKey)+"=[REDACTED]")
			continue
		}
		parts = append(parts, encodeURIComponent(decodedKey)+"="+
			encodeURIComponent(decodeQueryComponent(value)))
	}
	if len(parts) == 0 {
		return ""
	}
	return "?" + strings.Join(parts, "&")
}

// decodeQueryComponent 复刻 URLSearchParams 的解码：`+` 变空格，再按 UTF-8 解百分号转义。
func decodeQueryComponent(value string) string {
	replaced := strings.ReplaceAll(value, "+", " ")
	decoded, err := urlPathUnescape(replaced)
	if err != nil {
		return replaced
	}
	return decoded
}

// encodeURIComponent 复刻 JS 的 encodeURIComponent：保留 `A-Za-z0-9-_.!~*'()`，其余逐**字节**
// 编成大写 `%XX`。
func encodeURIComponent(value string) string {
	const upperHex = "0123456789ABCDEF"
	var builder strings.Builder
	builder.Grow(len(value))
	for index := 0; index < len(value); index++ {
		c := value[index]
		if isUnreservedURIComponent(c) {
			builder.WriteByte(c)
			continue
		}
		builder.WriteByte('%')
		builder.WriteByte(upperHex[c>>4])
		builder.WriteByte(upperHex[c&0x0f])
	}
	return builder.String()
}

// isUnreservedURIComponent 是 encodeURIComponent 的保留字符集。
func isUnreservedURIComponent(c byte) bool {
	switch {
	case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		return true
	}
	switch c {
	case '-', '_', '.', '!', '~', '*', '\'', '(', ')':
		return true
	default:
		return false
	}
}
