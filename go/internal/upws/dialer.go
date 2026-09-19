package upws

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// Downgrade 是「本可走上游 WS、为什么没走成」的事实，取值域逐字对齐 Node 的
// `ProviderChainItem.downgradeReason`（src/types/message.ts:306-321）。空串表示没有降级。
//
// 为什么取值必须逐字对齐而不是自造：它是**跨语言数据契约**——仪表盘按精确值渲染降级原因，
// 自造词会让界面显示空白（与链原因词表同一条纪律）。
type Downgrade string

const (
	// DowngradeEndpointCached：端点此前失败过，短期缓存窗口内不再尝试（未发起握手）。
	DowngradeEndpointCached Downgrade = "endpoint_ws_unsupported_cached"
	// DowngradeUpgradeRejected：握手被拒（非 101，例如 401/403/404）。
	DowngradeUpgradeRejected Downgrade = "ws_upgrade_rejected"
	// DowngradeClosedBeforeFirstEvent：握手成功但首事件前连接就关了。
	DowngradeClosedBeforeFirstEvent Downgrade = "ws_closed_before_first_event"
	// DowngradeErrorPreFirstEvent：首事件前的其他错误（含写首帧失败、超时）。
	DowngradeErrorPreFirstEvent Downgrade = "ws_error_pre_first_event"
)

// 协议常量（来源：openai/codex 源码核实）。
const (
	// betaHeaderName/betaHeaderValue：`OpenAI-Beta: responses_websockets=2026-02-06`，
	// codex 无条件插入该头（常量 RESPONSES_WEBSOCKETS_V2_BETA_HEADER_VALUE）。
	betaHeaderName  = "OpenAI-Beta"
	betaHeaderValue = "responses_websockets=2026-02-06"
	// defaultOriginator 是 identity 头 `originator` 的默认值（codex 的 default_headers 恒插它）。
	defaultOriginator = "codex_cli_rs"
	// defaultUnsupportedTTL 是「端点不支持」短期缓存的出厂 TTL，对齐 Node 的 unsupported-cache。
	defaultUnsupportedTTL = 5 * time.Minute
	// defaultReadLimit 是单帧上限。coder/websocket 的默认读上限只有 32 KiB，远小于
	// 一个含思考增量的事件，不显式抬高会在长回答上静默截断连接。
	defaultReadLimit = 32 << 20
)

// wsHandshakeHeaderStrips 是**不得**带进 WS 握手的 HTTP 专用头。
//
// 它们是上一次 HTTP 形态的遗留：Content-Length/Transfer-Encoding/Accept-Encoding 描述 body 形态，
// Connection/Upgrade/Sec-WebSocket-* 由 WS 库自己写，Content-Type/Accept 对 GET 升级无语义。
// 带上它们反而会让上游把请求当成非法升级拒掉。
var wsHandshakeHeaderStrips = []string{
	"content-length",
	"transfer-encoding",
	"accept-encoding",
	"connection",
	"upgrade",
	"sec-websocket-key",
	"sec-websocket-version",
	"sec-websocket-extensions",
	"sec-websocket-protocol",
	"content-type",
	"accept",
}

// Options 是拨号层配置。超时为零时取 dial.Default* 兜底（与 HTTP 拨号同一套语义）。
type Options struct {
	ConnectTimeout  time.Duration
	HeadersTimeout  time.Duration
	BodyIdleTimeout time.Duration
	// Originator 覆盖 identity 头 `originator`；空串取 defaultOriginator。
	Originator string
	// ClientVersion 是 `User-Agent: codex_cli_rs/<ver>` 与 `version` 头的取值。
	ClientVersion string
	// UnsupportedTTL 是端点不支持缓存的有效期；零值取出厂值。
	UnsupportedTTL time.Duration
	// Now 可注入时钟（测试隔离缓存过期）。
	Now func() time.Time
	// Logger 记录降级原因；为 nil 时写 stderr（与其他包同一约定）。
	Logger *logx.Logger
}

// Dialer 是上游 WS 拨号器。并发安全：它只持有不可变配置与一把缓存锁。
type Dialer struct {
	options Options

	mu sync.Mutex
	// unsupported 是「端点最近失败」的短期缓存：命中即不再尝试 WS（未发起握手）。
	// 条目按需过期（读时判定），条目数上界是去重后的端点 URL 数。
	unsupported map[string]time.Time
}

// New 构造拨号器。options 的零值可用。
func New(options Options) *Dialer {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Originator == "" {
		options.Originator = defaultOriginator
	}
	if options.UnsupportedTTL <= 0 {
		options.UnsupportedTTL = defaultUnsupportedTTL
	}
	return &Dialer{options: options, unsupported: map[string]time.Time{}}
}

// Request 是一次上游 WS 调用的输入。
type Request struct {
	// EndpointURL 是本打算发往该上游的 **HTTP** 端点 URL；WS URL 由它换 scheme 得出。
	//
	// 为什么从 HTTP URL 推导而不是另配一个 WS base：上游就是同一个端点换了 scheme
	// （openai/codex 的 websocket_url_for_path 只做 http→ws / https→wss，路径原样），
	// 让供应商再配一个「WS 地址」会多出一处可能与 HTTP 地址漂移的真相。
	EndpointURL string
	// Headers 是本次尝试的出站头（已含 Authorization 与供应商自定义头）。
	Headers http.Header
	// Body 是 Responses create body（未包 type 标签）。
	Body []byte
	// SessionID 非空时写入 session-id / thread-id / x-client-request-id（同一值）。
	SessionID string
}

// Outcome 是一次上游 WS 尝试的结论。
type Outcome struct {
	// Response 非 nil 表示 WS 已建连且首事件已到，可直接当 HTTP 响应使用（Body 是 SSE 字节流）。
	Response *dial.Response
	// Attempted 为真表示真的发起过握手。为假只有一种情形：端点命中不支持缓存。
	//
	// 调用方**必须**用它区分留痕语义：「没尝试」不该记成「尝试过但降级」。
	Attempted bool
	// Reason 是降级原因（Attempted 为真且 Response 为 nil 时非空）。
	Reason Downgrade
	// Err 是底层错误，仅用于诊断与「上下文是否已取消」的判断，**不参与熔断记账**。
	Err error
}

// EndpointEligible 报告端点当前是否值得尝试 WS（未命中「不支持」短期缓存）。
func (d *Dialer) EndpointEligible(endpointURL string) bool {
	if d == nil {
		return false
	}
	key := endpointKey(endpointURL)
	d.mu.Lock()
	defer d.mu.Unlock()
	until, ok := d.unsupported[key]
	if !ok {
		return true
	}
	if d.options.Now().Before(until) {
		return false
	}
	// 过期条目就地清理：缓存是「短期不复用」，不是一个长期黑名单。
	delete(d.unsupported, key)
	return true
}

// markUnsupported 把端点记入短期缓存。
func (d *Dialer) markUnsupported(endpointURL string) {
	key := endpointKey(endpointURL)
	if key == "" {
		return
	}
	d.mu.Lock()
	d.unsupported[key] = d.options.Now().Add(d.options.UnsupportedTTL)
	d.mu.Unlock()
}

// DialWS 建连并返回以 SSE 形态呈现的响应。
//
// 失败语义：不返回 Failure、不记账——上游 WS 走不成本身**不是供应商故障**
// （Node 明示：不切换供应商、不计入熔断器）。是否降级留痕由调用方按 Outcome 决定。
func (d *Dialer) DialWS(ctx context.Context, req Request) Outcome {
	if d == nil {
		return Outcome{Attempted: false, Reason: ""}
	}
	if !d.EndpointEligible(req.EndpointURL) {
		return Outcome{Attempted: false, Reason: DowngradeEndpointCached}
	}

	target, err := websocketURL(req.EndpointURL)
	if err != nil {
		return Outcome{Attempted: false, Reason: DowngradeErrorPreFirstEvent, Err: err}
	}

	connectCtx, cancelConnect := context.WithTimeout(ctx, d.resolveConnectTimeout())
	conn, resp, err := websocket.Dial(connectCtx, target, &websocket.DialOptions{
		HTTPHeader: d.handshakeHeaders(req),
	})
	cancelConnect()
	if err != nil {
		reason := DowngradeUpgradeRejected
		if ctx.Err() != nil {
			// 客户端已中断：这既不是端点不支持，也不值得进缓存（下次请求该照常尝试）。
			return Outcome{Attempted: true, Reason: DowngradeErrorPreFirstEvent, Err: ctx.Err()}
		}
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		d.markUnsupported(req.EndpointURL)
		d.logDowngrade("upws.ws_handshake_rejected", req.EndpointURL, reason, err)
		return Outcome{Attempted: true, Reason: reason, Err: err}
	}
	conn.SetReadLimit(defaultReadLimit)

	stop := context.AfterFunc(ctx, func() { _ = conn.CloseNow() })
	reader := &frameReader{
		conn:            conn,
		ctx:             ctx,
		stop:            stop,
		headersTimeout:  d.resolveHeadersTimeout(),
		bodyIdleTimeout: d.resolveBodyIdleTimeout(),
	}

	frame, err := createFrame(req.Body)
	if err == nil {
		writeCtx, cancelWrite := context.WithTimeout(ctx, d.resolveHeadersTimeout())
		err = conn.Write(writeCtx, websocket.MessageText, frame)
		cancelWrite()
	}
	if err == nil {
		err = reader.prime()
	}
	if err != nil {
		_ = reader.Close()
		if ctx.Err() != nil {
			return Outcome{Attempted: true, Reason: DowngradeErrorPreFirstEvent, Err: ctx.Err()}
		}
		reason := DowngradeErrorPreFirstEvent
		if isCleanClose(err) {
			reason = DowngradeClosedBeforeFirstEvent
		}
		d.markUnsupported(req.EndpointURL)
		d.logDowngrade("upws.ws_failed_before_first_event", req.EndpointURL, reason, err)
		return Outcome{Attempted: true, Reason: reason, Err: err}
	}

	return Outcome{
		Attempted: true,
		Response: &dial.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK (websocket)",
			Header:     syntheticSSEHeader(),
			Proto:      "WS",
			Body:       reader,
		},
	}
}

// handshakeHeaders 组装握手头：出站头为底，删掉 HTTP 专用头，再补 WS 身份头。
func (d *Dialer) handshakeHeaders(req Request) http.Header {
	header := req.Headers.Clone()
	if header == nil {
		header = http.Header{}
	}
	for _, name := range wsHandshakeHeaderStrips {
		header.Del(name)
	}
	header.Set(betaHeaderName, betaHeaderValue)
	if header.Get("originator") == "" {
		header.Set("originator", d.options.Originator)
	}
	if version := strings.TrimSpace(d.options.ClientVersion); version != "" {
		// `version` 与 User-Agent 同源：chatgpt.com 侧会检查身份头，缺了直接 403。
		if header.Get("User-Agent") == "" {
			header.Set("User-Agent", defaultOriginator+"/"+version)
		}
		if header.Get("version") == "" {
			header.Set("version", version)
		}
	}
	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		sessionID = firstNonEmpty(header.Get("session-id"), header.Get("thread-id"))
	}
	if sessionID != "" {
		header.Set("session-id", sessionID)
		header.Set("thread-id", sessionID)
		header.Set("x-client-request-id", sessionID)
	}
	return header
}

func (d *Dialer) resolveConnectTimeout() time.Duration {
	return resolveTimeout(d.options.ConnectTimeout, dial.DefaultConnectTimeout)
}

func (d *Dialer) resolveHeadersTimeout() time.Duration {
	return resolveTimeout(d.options.HeadersTimeout, dial.DefaultHeadersTimeout)
}

func (d *Dialer) resolveBodyIdleTimeout() time.Duration {
	return resolveTimeout(d.options.BodyIdleTimeout, dial.DefaultBodyIdleTimeout)
}

func resolveTimeout(value, fallback time.Duration) time.Duration {
	if value > 0 {
		return value
	}
	return fallback
}

func (d *Dialer) logDowngrade(event, endpointURL string, reason Downgrade, err error) {
	if d.options.Logger == nil {
		return
	}
	// 端点 URL 先脱敏：供应商地址里可能带凭据（与拨号层同一口径）。
	d.options.Logger.Warn(event, map[string]any{
		"endpoint_url": dial.RedactURL(endpointURL),
		"reason":       string(reason),
		"error":        err.Error(),
	})
}

// websocketURL 把 HTTP 端点 URL 换成 WS URL（scheme 换、路径与查询原样）。
func websocketURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("upws: 端点 URL 无法解析: %w", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	case "ws", "wss":
		// 已是 WS 形态：原样返回（与 openai/codex 的 websocket_url_for_path 一致）。
	default:
		return "", fmt.Errorf("upws: 端点 scheme %q 不支持 WS 升级", parsed.Scheme)
	}
	if parsed.Host == "" {
		return "", errors.New("upws: 端点 URL 缺少主机")
	}
	return parsed.String(), nil
}

// syntheticSSEHeader 是合成响应的 headers：让下游按流式处理（与 HTTP 路径同形）。
func syntheticSSEHeader() http.Header {
	header := http.Header{}
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	return header
}

// endpointKey 归一化端点 URL 作为缓存键（忽略尾部斜杠差异）。
func endpointKey(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// isCleanClose 判定错误是否为「对端正常关闭」（1000/1001 之外都算异常）。
func isCleanClose(err error) bool {
	var closeErr websocket.CloseError
	if !errors.As(err, &closeErr) {
		return false
	}
	return closeErr.Code == websocket.StatusNormalClosure || closeErr.Code == websocket.StatusGoingAway
}
