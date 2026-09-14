package dial

import (
	"fmt"
	"net/url"
	"strings"
)

// BuildUpstreamURL 把供应商 base URL 与目标路径拼成上游请求地址。
//
// 依据提交 27b5c422 的实测教训：**必须保留 base URL 自带的路径前缀**。
// 例如 base `https://host/zen/go/` + path `/v1/responses` → `https://host/zen/go/v1/responses`；
// 用 `new URL(base).pathname = path` 那样的整体替换会丢 `/zen/go`，上游返回 404 或网页。
//
// 规则（对齐 src/app/v1/_lib/url.ts 的拼接语义）：
//   - base 末尾斜杠去掉，path 前导斜杠补上，二者直接相接；
//   - base 已含与 path 相同的端点段时不重复追加（如 base 已以 /v1/messages 结尾）。
func BuildUpstreamURL(base string, path string) (string, error) {
	return BuildUpstreamURLWithQuery(base, path, "")
}

// BuildUpstreamURLWithQuery 在拼接路径的同时带上客户端原始查询串。
//
// 为何要单独一个函数：查询串是**上游行为开关**的一部分（Gemini 的 `?alt=sse` 决定它回 SSE
// 还是 JSON 数组），丢掉它就静默改变响应形状。Node 拼上游 URL 用的是整个 requestUrl
// （path + search），此处对齐。空查询串与 BuildUpstreamURL 完全等价。
func BuildUpstreamURLWithQuery(base string, path string, rawQuery string) (string, error) {
	trimmedBase := strings.TrimSpace(base)
	if trimmedBase == "" {
		return "", fmt.Errorf("%w: empty base url", ErrRequestBuild)
	}
	parsed, err := url.Parse(trimmedBase)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrRequestBuild, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("%w: base url must be absolute: %s", ErrRequestBuild, RedactURL(trimmedBase))
	}

	target := strings.TrimSpace(path)
	if target == "" {
		return parsed.String(), nil
	}
	if !strings.HasPrefix(target, "/") {
		target = "/" + target
	}

	basePath := strings.TrimSuffix(parsed.Path, "/")
	// Case 1（对齐 Node 的 buildProxyUrl 第一步）：base 已是请求路径的前缀时直接用请求路径。
	//
	// 为何必须有：Gemini 的官方端点是 `https://generativelanguage.googleapis.com/v1beta`，
	// 而客户端的请求路径就是 `/v1beta/models/...`——标准拼接会得到 `/v1beta/v1beta/models/...`。
	// 同一类问题也出现在「供应商 url 填到版本根」（如 `https://host/v1`）的其它协议线上：
	// 那时标准拼接会得到 `/v1/v1/messages`。Node 靠这条规则避开重复。
	if basePath != "" && (target == basePath || strings.HasPrefix(target, basePath+"/")) {
		parsed.Path = target
		parsed.RawPath = ""
		return parsed.String(), nil
	}
	// Case 2（Node 的 `basePath.endsWith(endpoint)`）：base 已含相同端点段时不再重复追加；
	// 只比较完整路径段，避免 /v1api 被误判成 /v1，也避免 /responses-archive 被折叠成 /responses。
	if basePath != "" && hasTrailingPathSegment(basePath, target) {
		parsed.Path = basePath
	} else {
		parsed.Path = basePath + target
	}
	parsed.RawPath = ""
	if query := strings.TrimSpace(rawQuery); query != "" {
		parsed.RawQuery = strings.TrimPrefix(query, "?")
	}
	return parsed.String(), nil
}

// hasTrailingPathSegment 判断 basePath 是否以 target 的完整路径段结尾。
func hasTrailingPathSegment(basePath string, target string) bool {
	normalizedTarget := strings.TrimSuffix(target, "/")
	if normalizedTarget == "" || normalizedTarget == "/" {
		return true
	}
	if !strings.HasSuffix(basePath, normalizedTarget) {
		return false
	}
	prefix := strings.TrimSuffix(basePath, normalizedTarget)
	if prefix == "" {
		return true
	}
	// 前缀末尾必须是分隔符，否则是 /v1api 这类「相似但不同」的路径。
	return strings.HasSuffix(prefix, "/")
}
