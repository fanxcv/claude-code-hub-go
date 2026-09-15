// Package dial 实现上游拨号层：请求构造、三档超时、响应字节透传与上游连接准入。
//
// 语义对齐 src/app/v1/_lib/proxy/forwarder.ts：
//   - 默认强制 Accept-Encoding: identity 以禁用上游压缩（对应 Node 侧绕开 undici 自动解压）；
//   - 三档超时独立触发（FETCH_CONNECT_TIMEOUT / FETCH_HEADERS_TIMEOUT / FETCH_BODY_TIMEOUT）；
//   - 响应字节不做解压、不改写，直接交给调用方；
//   - 不支持的上游形态（SOCKS 或自定义 dispatcher 代理）必须显式拒绝，绝不静默直连。
package dial

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultTimeout 是 Options 各超时字段为 0 时使用的兜底值，与 Node 侧环境变量默认值一致。
const (
	DefaultConnectTimeout  = 30 * time.Second
	DefaultHeadersTimeout  = 600 * time.Second
	DefaultBodyIdleTimeout = 600 * time.Second
)

// Options 描述一次拨号所需的全部可配置项。零值可用，字段为零时取 Default* 兜底。
type Options struct {
	// ConnectTimeout 限制 TCP/TLS 建连阶段。对应 FETCH_CONNECT_TIMEOUT。
	ConnectTimeout time.Duration
	// HeadersTimeout 限制「请求写完后到响应头到达」这段。对应 FETCH_HEADERS_TIMEOUT。
	HeadersTimeout time.Duration
	// BodyIdleTimeout 限制响应体两次可读数据之间的最长间隔。对应 FETCH_BODY_TIMEOUT。
	BodyIdleTimeout time.Duration
	// MaxUpstreamConnections 是同时在途的上游响应数上限（含请求体仍在写出的连接）。
	// 为零表示不限制；超限时返回 ErrPoolAdmissionExceeded，不排队等待。
	MaxUpstreamConnections int
	// ProxyURL 非空即代表「需要自定义 dispatcher 的拨号形态」。当前实现不支持，返回
	// ErrUnsupportedUpstreamTransport。它的存在是为了让不支持的供应商显式失败。
	ProxyURL string
	// MaxIdleConnsPerHost 交给 http.Transport 的连接复用上限；为零取 Go 默认。
	MaxIdleConnsPerHost int
	// DisableKeepAlives 为真时禁用连接复用（仅测试与诊断使用）。
	DisableKeepAlives bool
	// Transport 允许测试注入自定义 RoundTripper。非空时忽略其余传输层配置。
	Transport http.RoundTripper
	// HTTP2Enabled 每请求询问一次「这条上游是否允许尝试 HTTP/2」，nil 表示不允许
	// （默认只走 HTTP/1.1，与 Go 端口历史行为一致）。
	//
	// 语义对齐 Node 的 system_settings.enable_http2：它是全局开关，但**按请求**读取——Node 在
	// 每次转发前读一遍设置缓存快照（forwarder.ts:3954）。若改成构造期读一次，改开关就必须重启，
	// 与 Node 行为分叉。
	HTTP2Enabled func(ctx context.Context) bool
}

// Request 是一次上游调用的输入。Body 为 nil 表示无请求体。
type Request struct {
	Method  string
	URL     string
	Headers http.Header
	Body    io.Reader
	// ContentLength 为 -1 表示长度未知（分块写出）。
	ContentLength int64
}

// Response 是上游返回的原始响应。调用方负责关闭 Body。
type Response struct {
	StatusCode int
	Status     string
	Header     http.Header
	Proto      string
	Body       io.ReadCloser
}

// IPHeaders 是决定是否向上游注入客户端 IP 头的输入，对应 provider.preserveClientIp 语义。
type IPHeaders struct {
	// PreserveClientIp 为真才注入，对应 provider 配置项，默认 false。
	PreserveClientIp bool
	// Headers 是客户端原始请求头。
	Headers http.Header
}

// Client 是可复用的拨号器。并发安全。
type Client struct {
	options Options
	// http 是 HTTP/1.1 传输；h2 是按需构造的 HTTP/2 传输（见 http2.go）。
	http *http.Client
	h2   *http.Client
	// quarantine 记录近期出现过 HTTP/2 协议错误的路由（对齐 Node 的隔离表）。
	quarantine *http2Quarantine
	// now 可注入时钟（测试用隔离期过期）。
	now    func() time.Time
	h2Once sync.Once
	admit  chan struct{}
	mu     sync.Mutex
	closed bool
}

// New 构造拨号器。Options.ProxyURL 非空时直接失败，避免调用方以为代理已生效。
func New(options Options) (*Client, error) {
	if strings.TrimSpace(options.ProxyURL) != "" {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedUpstreamTransport, RedactURL(options.ProxyURL))
	}
	transport := options.Transport
	if transport == nil {
		transport = &http.Transport{
			Proxy:                 nil, // 显式禁用环境代理：代理只能经 ProxyURL 声明，而它当前不受支持
			DisableCompression:    true,
			DisableKeepAlives:     options.DisableKeepAlives,
			MaxIdleConnsPerHost:   options.MaxIdleConnsPerHost,
			ForceAttemptHTTP2:     false,
			DialContext:           (&net.Dialer{Timeout: resolve(options.ConnectTimeout, DefaultConnectTimeout)}).DialContext,
			TLSHandshakeTimeout:   resolve(options.ConnectTimeout, DefaultConnectTimeout),
			ResponseHeaderTimeout: resolve(options.HeadersTimeout, DefaultHeadersTimeout),
			ExpectContinueTimeout: time.Second,
		}
	}
	client := &Client{
		options:    options,
		http:       &http.Client{Transport: transport},
		quarantine: newHTTP2Quarantine(),
		now:        time.Now,
	}
	if options.MaxUpstreamConnections > 0 {
		client.admit = make(chan struct{}, options.MaxUpstreamConnections)
	}
	return client, nil
}

// RoundTrip 执行一次上游调用。
//
// 返回的 Response.Body 会带上响应体空闲超时；IdleTimeout 触发时 Body 上的读会以
// ErrBodyIdleTimeout 结束，且底层连接被关闭。任何失败都返回可判别的错误（见 errors.go）。
func (c *Client) RoundTrip(ctx context.Context, request Request) (*Response, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("dial: client is closed")
	}
	c.mu.Unlock()

	release, err := c.acquire(ctx)
	if err != nil {
		return nil, err
	}

	useHTTP2 := c.http2Eligible(ctx, request.URL)
	httpResponse, attemptErr := c.attempt(ctx, request, c.transportFor(useHTTP2))
	if errors.Is(attemptErr, ErrRequestBuild) {
		release()
		return nil, attemptErr
	}
	if attemptErr != nil && useHTTP2 && isHTTP2ProtocolError(attemptErr) {
		// 对齐 Node forwarder.ts:4297-4340：HTTP/2 协议错误按**传输层**问题处理——隔离这条
		// 路由 5 分钟，并透明回退 HTTP/1.1 重试一次；不改供应商选路，也不进熔断归因。
		c.quarantine.quarantine(request.URL, c.now())
		if rewindBody(request.Body) {
			httpResponse, attemptErr = c.attempt(ctx, request, c.transportFor(false))
		}
	}
	if attemptErr != nil {
		release()
		return nil, classifyTransportError(attemptErr, c.options)
	}

	idle := resolve(c.options.BodyIdleTimeout, DefaultBodyIdleTimeout)
	return &Response{
		StatusCode: httpResponse.StatusCode,
		Status:     httpResponse.Status,
		Header:     httpResponse.Header.Clone(),
		Proto:      httpResponse.Proto,
		Body:       newIdleTimeoutBody(httpResponse.Body, idle, release),
	}, nil
}

// transportFor 给出本次使用的客户端：允许且未被隔离时用 h2，否则 h1。
func (c *Client) transportFor(useHTTP2 bool) *http.Client {
	if !useHTTP2 {
		return c.http
	}
	if client := c.http2Client(); client != nil {
		return client
	}
	return c.http
}

// attempt 用给定传输发一次请求；错误原样返回，由调用方决定是否回退与如何分类。
func (c *Client) attempt(ctx context.Context, request Request, client *http.Client) (*http.Response, error) {
	method := request.Method
	if method == "" {
		method = http.MethodPost
	}

	httpRequest, err := http.NewRequestWithContext(ctx, method, request.URL, request.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRequestBuild, err)
	}
	if httpRequest.Host == "" {
		// 让 Host 头与请求目标一致（不要在 URL 之外另行覆写）。
		httpRequest.Host = ""
	}
	httpRequest.ContentLength = request.ContentLength
	applyHeaders(httpRequest.Header, request.Headers)
	ForceIdentityEncoding(httpRequest.Header)

	return client.Do(httpRequest)
}

// Close 关闭空闲连接。在途响应不受影响。
func (c *Client) Close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	closeIdle(c.http)
	closeIdle(c.h2)
}

// closeIdle 关闭某个客户端底层传输的空闲连接（注入的 RoundTripper 不是 *http.Transport 时无操作）。
func closeIdle(client *http.Client) {
	if client == nil {
		return
	}
	if transport, ok := client.Transport.(*http.Transport); ok {
		transport.CloseIdleConnections()
	}
}

func (c *Client) acquire(ctx context.Context) (func(), error) {
	if c.admit == nil {
		return func() {}, nil
	}
	select {
	case c.admit <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-c.admit }) }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("%w: %v", ErrPoolAdmissionExceeded, ctx.Err())
	default:
		return nil, ErrPoolAdmissionExceeded
	}
}

func applyHeaders(target http.Header, source http.Header) {
	for key, values := range source {
		if len(values) == 0 {
			continue
		}
		target.Del(key)
		for _, value := range values {
			target.Add(key, value)
		}
	}
}

// ForceIdentityEncoding 把 Accept-Encoding 固定为 identity，禁用上游压缩。
//
// 依据 forwarder.ts 的注释（约 :8990）：代理必须透传原始数据，不能让传输层解压，
// 否则会出现「上游是 gzip、下游按明文处理」以及中途截断时的 ZlibError。
func ForceIdentityEncoding(header http.Header) {
	header.Set("Accept-Encoding", "identity")
}

func resolve(value, fallback time.Duration) time.Duration {
	if value <= 0 {
		return fallback
	}
	return value
}

// RedactURL 去掉 URL 里的用户信息与查询串，供错误信息与日志使用。
func RedactURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "(unparsable url)"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}
