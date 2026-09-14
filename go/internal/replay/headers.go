package replay

import (
	"net/http"
	"strings"
)

// CaptureResponseHeaders 复刻 replay-headers.ts 的 captureReplayResponseHeaders：
// 头名一律小写，剔除无语义项（逐跳头、长度、Set-Cookie 等，见 replayExcludedHeaders），
// content-type 缺失时用 fallback 补齐。
//
// 为什么必须显式记 content-type：重放命中时要靠它决定 cache-control 与恢复出的头集合；
// 上游若省略该头，两条路径（热层与 PG）都会拿到空值，命中响应就退化成默认 SSE。
func CaptureResponseHeaders(source http.Header, fallbackContentType string) map[string]string {
	captured := make(map[string]string, len(source)+1)
	for name, values := range source {
		if len(values) == 0 {
			continue
		}
		lower := strings.ToLower(name)
		if replayExcludedHeaders[lower] {
			continue
		}
		captured[lower] = values[0]
	}
	if fallbackContentType != "" && captured["content-type"] == "" {
		captured["content-type"] = fallbackContentType
	}
	return captured
}
