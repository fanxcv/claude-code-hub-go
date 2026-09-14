package convert

import "strings"

// 各协议线同线恒等映射的路径集合（已归一化）。
var nativePathSets = map[WireProtocol]map[string]bool{
	ProtocolAnthropicMessages: {
		"/v1/messages":              true,
		"/v1/messages/count_tokens": true,
	},
	ProtocolOpenAIChat: {
		"/v1/chat/completions": true,
		"/v1/embeddings":       true,
	},
	ProtocolOpenAIResponses: {
		"/v1/responses":         true,
		"/v1/responses/compact": true,
	},
}

// crossLinePaths 是跨线映射表：入站路径 -> 目标协议线 -> 上游路径。
// 带副作用的端点（count_tokens / compact / embeddings）只允许同线，跨线一律 nil。
var crossLinePaths = map[string]map[WireProtocol]string{
	"/v1/messages": {
		ProtocolOpenAIChat:      "/v1/chat/completions",
		ProtocolOpenAIResponses: "/v1/responses",
	},
	"/v1/chat/completions": {
		ProtocolAnthropicMessages: "/v1/messages",
		ProtocolOpenAIResponses:   "/v1/responses",
	},
	"/v1/responses": {
		ProtocolAnthropicMessages: "/v1/messages",
		ProtocolOpenAIChat:        "/v1/chat/completions",
	},
}

// NormalizeEndpointPath 去掉 query、去掉尾部斜杠（长度大于 1 时）并转小写。
func NormalizeEndpointPath(pathname string) string {
	pathWithoutQuery := pathname
	if index := strings.IndexByte(pathname, '?'); index >= 0 {
		pathWithoutQuery = pathname[:index]
	}
	trimmed := pathWithoutQuery
	if len(trimmed) > 1 && strings.HasSuffix(trimmed, "/") {
		trimmed = trimmed[:len(trimmed)-1]
	}
	return strings.ToLower(trimmed)
}

// ResolveUpstreamPath 按目标协议线与客户端入站路径解析上游路径；无法映射返回 (零值, false)。
func ResolveUpstreamPath(targetProtocol WireProtocol, clientPathname string) (string, bool) {
	normalized := NormalizeEndpointPath(clientPathname)
	if nativePathSets[targetProtocol][normalized] {
		return normalized, true
	}
	if mapped, ok := crossLinePaths[normalized][targetProtocol]; ok {
		return mapped, true
	}
	return "", false
}
