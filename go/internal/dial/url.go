package dial

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// BuildUpstreamURL 把供应商 base URL 与目标路径拼成上游请求地址。
//
// 依据提交 27b5c422 的实测教训：**必须保留 base URL 自带的路径前缀**。
// 例如 base `https://host/zen/go/` + path `/v1/responses` → `https://host/zen/go/v1/responses`；
// 用 `new URL(base).pathname = path` 那样的整体替换会丢 `/zen/go`，上游返回 404 或网页。
//
// 规则（对齐 src/lib/v1-url.ts 的拼接语义，Node 后端曾用同一份实现）：
//   - base 末尾斜杠去掉，path 前导斜杠补上，二者直接相接；
//   - base 已含与 path 相同的端点段时不重复追加（如 base 已以 /v1/messages 结尾）；
//   - base 停在端点根（/openai/messages）或版本根（/v1、/v3、/v1beta）时只追加缺的那段，
//     见 joinEndpointPath。
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

	// finish 统一收尾：写回路径并带上客户端查询串。
	//
	// 查询串是上游行为开关的一部分（Gemini 的 `?alt=sse` 决定回 SSE 还是 JSON 数组），
	// 故**每条分支**都要带上它——Node 的 buildProxyUrl 在每个 return 前都设 search。
	finish := func(path string) (string, error) {
		parsed.Path = path
		parsed.RawPath = ""
		if query := strings.TrimSpace(rawQuery); query != "" {
			parsed.RawQuery = strings.TrimPrefix(query, "?")
		}
		return parsed.String(), nil
	}

	basePath := strings.TrimSuffix(parsed.Path, "/")
	// Case 1（对齐 Node 的 buildProxyUrl 第一步）：base 已是请求路径的前缀时直接用请求路径。
	//
	// 为何必须有：Gemini 的官方端点是 `https://generativelanguage.googleapis.com/v1beta`，
	// 而客户端的请求路径就是 `/v1beta/models/...`——标准拼接会得到 `/v1beta/v1beta/models/...`。
	// 同一类问题也出现在「供应商 url 填到版本根」（如 `https://host/v1`）的其它协议线上：
	// 那时标准拼接会得到 `/v1/v1/messages`。Node 靠这条规则避开重复。
	if basePath != "" && (target == basePath || strings.HasPrefix(target, basePath+"/")) {
		return finish(target)
	}
	// Case 2（Node 的端点根/版本根识别）：base 已含端点根或版本根时只追加缺的那段。
	if basePath != "" {
		if joined, ok := joinEndpointPath(basePath, target); ok {
			return finish(joined)
		}
	}
	// Case 3：标准拼接。
	return finish(basePath + target)
}

// upstreamEndpoints 对齐 src/lib/v1-url.ts 的 targetEndpoints；顺序即匹配优先级。
var upstreamEndpoints = []string{
	"/responses",            // Codex Response API
	"/messages",             // Claude Messages API
	"/chat/completions",     // OpenAI Compatible
	"/embeddings",           // OpenAI Compatible Embeddings
	"/images",               // OpenAI Compatible Images API
	"/audio/transcriptions", // OpenAI Compatible Audio API
	"/audio/translations",   // OpenAI Compatible Audio API
	"/files",                // OpenAI Compatible Files API
	"/models",               // Gemini & OpenAI models
}

// upstreamEndpointPatterns 是 `^/(v\d+[a-z0-9]*)<endpoint>(/.*)?$`（子匹配：版本段、资源后缀）。
var upstreamEndpointPatterns = func() []*regexp.Regexp {
	patterns := make([]*regexp.Regexp, len(upstreamEndpoints))
	for index, endpoint := range upstreamEndpoints {
		patterns[index] = regexp.MustCompile(`^/(v\d+[a-z0-9]*)` + regexp.QuoteMeta(endpoint) + `(/.*)?$`)
	}
	return patterns
}()

// versionRootPattern 对齐 isVersionRootPath：末段是纯版本 token（v1、v12、v1beta、v1beta1、v1rc1 …）。
var versionRootPattern = regexp.MustCompile(`^v\d+(?:(?:alpha|beta|preview|internal|rc|ga|stable|dev|canary)\d*)?$`)

// joinEndpointPath 复刻 src/lib/v1-url.ts 的 Case 2：先看 target 是不是「版本段 + 端点（+资源后缀）」,
// 再按 base 已含多少决定补什么。命中即返回完整路径，未命中交给调用方做标准拼接。
func joinEndpointPath(basePath string, target string) (string, bool) {
	for index, endpoint := range upstreamEndpoints {
		groups := upstreamEndpointPatterns[index].FindStringSubmatch(target)
		if groups == nil {
			continue
		}
		suffix := groups[2]
		requestRoot := "/" + groups[1] + endpoint
		// base 已含整条端点（含资源后缀）时原样复用。
		if suffix != "" &&
			(strings.HasSuffix(basePath, requestRoot+suffix) || strings.HasSuffix(basePath, endpoint+suffix)) {
			return basePath, true
		}
		// base 已含端点根（可带版本段）时只补资源后缀。
		if strings.HasSuffix(basePath, requestRoot) || strings.HasSuffix(basePath, endpoint) {
			return basePath + suffix, true
		}
		// base 停在版本根（/v1、/v3、/v1beta）时版本段已在 base 里，只补端点与后缀。
		//
		// 生产实证（2026-09-15，ARK Codex / vendor 83）：base `…/api/plan/v3` 被拼成
		// `…/api/plan/v3/v1/chat/completions`，上游 404 ×2 后整家渠道从竞争中被摘掉；
		// 同一 base 下 Node 拼的是 `…/api/plan/v3/chat/completions`（上游 200）。
		if isVersionRootPath(basePath) {
			return basePath + endpoint + suffix, true
		}
	}
	return "", false
}

// isVersionRootPath 判断 basePath 的末段是不是版本根。
//
// 只认版本 token，避免把 `/proxy/v1api`、`/x/v10models` 这类普通路径误判成版本根（Node 注释同义）。
func isVersionRootPath(basePath string) bool {
	segments := strings.FieldsFunc(basePath, func(r rune) bool { return r == '/' })
	if len(segments) == 0 {
		return false
	}
	return versionRootPattern.MatchString(strings.ToLower(segments[len(segments)-1]))
}
