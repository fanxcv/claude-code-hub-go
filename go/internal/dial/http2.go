package dial

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// 本文件承载 HTTP/2 开关与「协议错误隔离」两件事，语义对齐 Node：
//
//   - system_settings.enable_http2 是全局开关，Node 在每次转发前读一遍缓存快照
//     （forwarder.ts:3954 `const http2EnabledBySetting = await isHttp2Enabled()`）；
//   - 出现 HTTP/2 协议错误（GOAWAY / RST_STREAM / PROTOCOL_ERROR 等）时，Node 把**这条路由**
//     隔离 5 分钟、透明回退 HTTP/1.1 重试一次，且不计入供应商熔断
//     （forwarder.ts:4297-4340 与 lib/proxy-agent/http2-quarantine.ts）。
//
// 为什么 Go 侧要自己维护两套传输：`http.Transport` 把「是否尝试 h2」固化在 ForceAttemptHTTP2
// 上，无法按请求切换；而 Node 的开关是运行时可变的。故这里同时持有 h1/h2 两个 http.Client，
// 按请求（开关 + 隔离表）选一个。隔离表的键、TTL、容量上限与 Node 同形。
const (
	// http2QuarantineTTL 对齐 Node 的 HTTP2_QUARANTINE_TTL_MS（5 分钟）。
	http2QuarantineTTL = 5 * time.Minute
	// http2QuarantineMaxRoutes 对齐 Node 的 HTTP2_QUARANTINE_MAX_ROUTES（1024 条，超出淘汰最旧）。
	http2QuarantineMaxRoutes = 1024
	// http2ErrorCauseMaxDepth 对齐 Node 的 HTTP2_ERROR_CAUSE_MAX_DEPTH：错误链最多看 4 层。
	http2ErrorCauseMaxDepth = 4
)

// http2ErrorPatterns 逐字对齐 Node 的 HTTP2_ERROR_PATTERNS（proxy/errors.ts:1079）。
//
// Go 侧错误形态不同（http2.StreamError 的文本形如 `stream error: stream ID 1; PROTOCOL_ERROR`，
// http2.GoAwayError 形如 `received GOAWAY ...`），但特征串一致，故沿用同一张表做子串判定。
var http2ErrorPatterns = []string{
	"GOAWAY",
	"RST_STREAM",
	"PROTOCOL_ERROR",
	"HTTP/2",
	"ERR_HTTP2_",
	"NGHTTP2_",
	"HTTP_1_1_REQUIRED",
	"REFUSED_STREAM",
}

// http2Quarantine 是「路由 → 解禁时刻」的有界表，并发安全。
type http2Quarantine struct {
	mu     sync.Mutex
	routes map[string]time.Time
}

func newHTTP2Quarantine() *http2Quarantine {
	return &http2Quarantine{routes: map[string]time.Time{}}
}

// http2RouteKey 给出隔离粒度：目标 origin。Node 的键还带 proxy origin（`target|proxy`，无代理时
// 写作 direct）；Go 侧不支持自定义上游代理（见 Options.ProxyURL 的拒绝语义），故固定为 direct，
// 保持键形制一致以便两边日志可对照。
func http2RouteKey(targetURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(targetURL))
	if err != nil || parsed.Host == "" {
		return strings.TrimSpace(targetURL) + "|direct"
	}
	scheme := parsed.Scheme
	if scheme == "" {
		scheme = "https"
	}
	return scheme + "://" + parsed.Host + "|direct"
}

// quarantined 报告该目标当前是否处于 h2 隔离期。
func (q *http2Quarantine) quarantined(targetURL string, now time.Time) bool {
	if q == nil {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pruneLocked(now)
	expiresAt, ok := q.routes[http2RouteKey(targetURL)]
	return ok && expiresAt.After(now)
}

// quarantine 隔离该目标，并刷新其在表内的顺序（与 Node 一致：反复失败的路线保持可见）。
func (q *http2Quarantine) quarantine(targetURL string, now time.Time) {
	if q == nil {
		return
	}
	key := http2RouteKey(targetURL)
	q.mu.Lock()
	defer q.mu.Unlock()
	q.pruneLocked(now)
	delete(q.routes, key)
	q.routes[key] = now.Add(http2QuarantineTTL)
	for len(q.routes) > http2QuarantineMaxRoutes {
		oldest := ""
		for candidate := range q.routes {
			if oldest == "" {
				oldest = candidate
				continue
			}
			// Go 的 map 无序，这里按解禁时刻取最早一条，等价于 Node 的插入序淘汰。
			if q.routes[candidate].Before(q.routes[oldest]) {
				oldest = candidate
			}
		}
		if oldest == "" {
			break
		}
		delete(q.routes, oldest)
	}
}

func (q *http2Quarantine) pruneLocked(now time.Time) {
	for key, expiresAt := range q.routes {
		if !expiresAt.After(now) {
			delete(q.routes, key)
		}
	}
}

// count 仅用于测试断言表的有界性。
func (q *http2Quarantine) count() int {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.routes)
}

// isHTTP2ProtocolError 判定错误链上是否出现 HTTP/2 协议错误特征，对齐 Node 的 isHttp2Error：
// 沿 cause 链最多看 http2ErrorCauseMaxDepth 层，逐层的 name/message/code 拼串后做子串匹配。
// Go 的 errors.Unwrap 与 Node 的 cause 链同构，故层数与匹配口径都一致。
func isHTTP2ProtocolError(err error) bool {
	current := err
	for depth := 0; depth <= http2ErrorCauseMaxDepth && current != nil; depth++ {
		text := strings.ToUpper(current.Error())
		for _, pattern := range http2ErrorPatterns {
			if strings.Contains(text, pattern) {
				return true
			}
		}
		current = errors.Unwrap(current)
	}
	return false
}

// http2Client 按需构造 HTTP/2 客户端（只构造一次）。
//
// 测试注入 Transport 时不做这件事：注入的按传输语义优先，h2 与其无从组合。
func (c *Client) http2Client() *http.Client {
	c.h2Once.Do(func() {
		if c.options.Transport != nil {
			return
		}
		c.h2 = &http.Client{Transport: newHTTP2Transport(c.options)}
	})
	return c.h2
}

// http2Eligible 报告本次是否对该目标尝试 HTTP/2：开关开启、未在隔离期、且未被测试注入接管。
func (c *Client) http2Eligible(ctx context.Context, targetURL string) bool {
	if c.options.Transport != nil || c.options.HTTP2Enabled == nil {
		return false
	}
	if !c.options.HTTP2Enabled(ctx) {
		return false
	}
	return !c.quarantine.quarantined(targetURL, c.now())
}

// newHTTP2Transport 构造开启 ALPN h2 的传输，其余字段与 h1 传输逐项一致（超时、禁用环境代理、
// 禁用自动解压、连接复用上限），避免两套传输在超时与压缩语义上分叉。
func newHTTP2Transport(options Options) *http.Transport {
	return &http.Transport{
		Proxy:                 nil,
		DisableCompression:    true,
		DisableKeepAlives:     options.DisableKeepAlives,
		MaxIdleConnsPerHost:   options.MaxIdleConnsPerHost,
		ForceAttemptHTTP2:     true,
		DialContext:           (&net.Dialer{Timeout: resolve(options.ConnectTimeout, DefaultConnectTimeout)}).DialContext,
		TLSHandshakeTimeout:   resolve(options.ConnectTimeout, DefaultConnectTimeout),
		ResponseHeaderTimeout: resolve(options.HeadersTimeout, DefaultHeadersTimeout),
		ExpectContinueTimeout: time.Second,
	}
}

// rewindBody 把正文回到起点，供 h2 失败后的 h1 重试复用。
//
// 返回 false 表示正文不可重放：此时不重试（宁可把原始错误交回调用方，也不发一个残缺的正文）。
// 运行时的正文来自 forward 的 `bytes.NewReader(plan.Body)`，本身可 Seek。
func rewindBody(body io.Reader) bool {
	if body == nil {
		return true
	}
	seeker, ok := body.(io.Seeker)
	if !ok {
		return false
	}
	_, err := seeker.Seek(0, io.SeekStart)
	return err == nil
}
