// Package ws 承载 `/v1/responses` 的客户端 WebSocket 通道。
//
// 与 Node 实现（`server.js` 的 `handleWebSocketConnection`）语义对齐：本包是**薄隧道**——
// 每个客户端文本帧 `{"type":"response.create",...}` 被翻成一个走完整数据面的
// `POST /v1/responses`（守卫链、选路、转发、终态结算都在数据面里，本包不重复实现业务），
// 响应 SSE 的每个事件再翻回一个文本帧。一条连接可连续承载多轮 `response.create`
// （协议的持久连接语义：终态事件后不关闭连接）。
//
// 与 Node 的唯一结构差异：Node 经私有 loopback 监听器做一次真实 HTTP 往返，本包直接调用
// 进程内的数据面处理器（省一趟 socket，且天然共享同一进程状态）。因此在 Node 侧用于
// 防止外部请求伪造隧道标记的「每进程内部密钥」在本包**不是**安全边界——真正的边界是
// 连接建立时剥掉客户端自带的全部 `x-cch-*` 头（见 stripClientHeaders）；密钥仍然照 Node
// 的口径随隧道请求携带，使数据面后续若要按 Node 的 `verifyInternalRequest` 判定也成立。
//
// 未实现（明确回退，不静默丢弃，见 README 的「未实现项」）：上游 WebSocket 建连
// （Node 的 `responses-ws/upstream-adapter.ts`，仅当客户端为 WS、供应商类型为 codex 且全局
// 开关开启时尝试）。本包对这类请求走 HTTP SSE 隧道——与 Node 在「开关关闭 / 端点不支持 /
// 非 codex」时的降级路径完全一致，客户端可见协议不变，只是少了上游 WS 的延迟收益。
package ws

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// RoutePath 是本包承载的唯一路径（与 Node 的 WS_PATH 一致）。
const RoutePath = "/v1/responses"

// 隧道标记头：与 Node 的 server.js / internal-secret.ts 同名同义。
const (
	// TransportHeader 声明「本请求来自 WebSocket 客户端」。
	TransportHeader = "x-cch-client-transport"
	// ForwardFlagHeader 是隧道请求的显式标记（Node 要求它必须为 "1"）。
	ForwardFlagHeader = "x-cch-responses-ws-forward"
	// SessionHeader 携带每连接会话 id，供上游复用持久会话时归组。
	SessionHeader = "x-cch-responses-ws-session"
	// SecretHeader 携带每进程内部密钥。
	SecretHeader = "x-cch-internal-secret"
	// SecretEnv 是内部密钥的运维覆盖项；与 Node 读同一个环境变量。
	SecretEnv = "CCH_RESPONSES_WS_INTERNAL_SECRET"
)

// reservedHeaderPrefix 是内部保留前缀。客户端自带的同名头一律剥掉（防伪造标记）。
const reservedHeaderPrefix = "x-cch-"

// 逐跳与握手专用头：隧道请求不是 WS 请求，这些头照 Node 的表逐个剔除。
var hopByHopHeaders = map[string]bool{
	"host":                     true,
	"connection":               true,
	"upgrade":                  true,
	"sec-websocket-key":        true,
	"sec-websocket-version":    true,
	"sec-websocket-extensions": true,
	"sec-websocket-protocol":   true,
	"content-length":           true,
	"transfer-encoding":        true,
}

// 与 Node 同口径的边界值（server.js 顶部常量）。
const (
	// maxPendingFrames 是一条连接上排队等待轮转的帧数上限。
	maxPendingFrames = 64
	// maxPendingBytes 是排队帧的总字节上限。
	maxPendingBytes = 64 * 1024 * 1024
	// maxFrameBytes 是单个出站帧的上限，也是内部响应/单个 SSE 事件的缓冲上限。
	//
	// Node 侧同名常量为 1 MiB：内部响应最终会成为一个出站帧，因此在同一层设限，
	// 而不是先缓冲出一个 safeSend 永远收不下的正文。
	maxFrameBytes = 1024 * 1024
	// maxInboundFrameBytes 是单个入站帧的上限（Node 的 WS_MAX_PAYLOAD_BYTES）。
	// 32 MiB 是为 codex 的大体积历史留的余量：更紧的上限会让客户端收到 RST 而非关闭帧。
	maxInboundFrameBytes = 32 * 1024 * 1024
	// outboundSendTimeout 限制单个出站帧的写入时长。
	outboundSendTimeout = 30 * time.Second
)

// Options 是边缘处理器的装配参数。
type Options struct {
	// Inner 是隧道目标：已装配的数据面处理器（`POST /v1/responses`）。必填。
	Inner http.Handler
	// Logger 可选；nil 时丢弃日志。
	Logger *logx.Logger
	// Secret 是随隧道请求携带的内部密钥；为空时取环境变量，仍为空则生成一次。
	Secret string
}

// Edge 是 WebSocket 边缘处理器：只接管 `/v1/responses` 的升级请求，其余一律透传给 Inner。
type Edge struct {
	inner  http.Handler
	log    *logx.Logger
	secret string
}

// New 装配边缘处理器。
func New(options Options) *Edge {
	logger := options.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	return &Edge{inner: options.Inner, log: logger, secret: resolveSecret(options.Secret)}
}

// ServeHTTP 只对 `/v1/responses` 的升级请求做 WebSocket 处理；其余交给 Inner。
//
// 路径判定用**原文精确相等**（NormalizePath 会吃掉尾斜杠，那会让 `/v1/responses/` 这类
// 请求被接管，而 Node 对非精确路径是直接 destroy socket）。
func (e *Edge) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if !IsUpgrade(request) || request.URL.Path != RoutePath {
		e.passThrough(writer, request)
		return
	}
	if e.inner == nil {
		// 与 httpapi 同口径：未装配是 503 而不是 501——501 会把「本进程还没接上数据面」
		// 谎报成「协议不支持」，让客户端放弃重试。
		e.log.Error("ws_edge_unavailable", map[string]any{"reason": "inner handler not configured"})
		writer.Header().Set("retry-after", "5")
		http.Error(writer, "websocket edge not configured", http.StatusServiceUnavailable)
		return
	}
	e.serveUpgrade(writer, request)
}

// passThrough 透传非 WS 流量；Inner 未装配时按 503 拒绝（不静默 200）。
func (e *Edge) passThrough(writer http.ResponseWriter, request *http.Request) {
	if e.inner == nil {
		http.Error(writer, "data plane not configured", http.StatusServiceUnavailable)
		return
	}
	e.inner.ServeHTTP(writer, request)
}

// IsUpgrade 报告请求是否为 WebSocket 升级（与 net/http 的升级判定同口径，大小写不敏感，
// 且 Connection 头里可能出现多个逗号分隔的 token）。
func IsUpgrade(request *http.Request) bool {
	if !strings.EqualFold(strings.TrimSpace(request.Header.Get("Upgrade")), "websocket") {
		return false
	}
	for _, token := range strings.Split(request.Header.Get("Connection"), ",") {
		if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
			return true
		}
	}
	return false
}

func (e *Edge) serveUpgrade(writer http.ResponseWriter, request *http.Request) {
	conn, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
		// Node 的 `ws` 服务器不校验 Origin（没有 verifyClient），这里保持同一行为。
		// 客户端侧防护由数据面的凭据校验承担，与 HTTP 路径同一条守卫链。
		InsecureSkipVerify: true,
	})
	if err != nil {
		e.log.Warn("ws_accept_failed", map[string]any{"error": err.Error()})
		return
	}
	conn.SetReadLimit(maxInboundFrameBytes)

	ctx, cancel := context.WithCancel(context.Background())
	connection := &connection{
		edge:        e,
		conn:        conn,
		ctx:         ctx,
		cancel:      cancel,
		log:         e.log,
		sessionID:   newSessionID(),
		queryModel:  strings.TrimSpace(request.URL.Query().Get("model")),
		remoteAddr:  request.RemoteAddr,
		baseHeaders: stripClientHeaders(request.Header),
	}
	e.log.Info("ws_client_connected", map[string]any{
		"path":      RoutePath,
		"sessionId": connection.sessionID,
	})
	connection.run()
}

// connection 是一条客户端 WS 连接的运行状态。
type connection struct {
	edge *Edge
	conn *websocket.Conn
	// ctx 在客户端断开或致命错误时取消：它同时是本连接上所有轮次请求的父上下文，
	// 因此取消即等于 Node 侧 destroy 内部请求（数据面按客户端中断语义收尾并结算）。
	ctx    context.Context
	cancel context.CancelFunc
	log    *logx.Logger
	// sessionID 是本连接的会话标识，随隧道请求传给数据面。
	sessionID string
	// queryModel 是握手 URL 上的 `?model=`，仅用于补全帧里缺 model 的请求。
	queryModel string
	// remoteAddr 是客户端地址，写进隧道请求，让数据面的客户端 IP 判定与 HTTP 路径同源。
	remoteAddr string
	// baseHeaders 是剥掉逐跳/内部头之后的客户端握手头，逐轮复用。
	baseHeaders http.Header

	mu           sync.Mutex
	pending      []queuedFrame
	pendingBytes int
	inFlight     bool
	closed       bool
}

// queuedFrame 是一帧待转发的 `response.create`（只保留原始文本）。
type queuedFrame struct {
	data []byte
}

// run 是连接的主循环：读帧并排队，轮次由单个 goroutine 串行消费（与 Node 的 drain 同构）。
func (c *connection) run() {
	defer func() {
		if recovered := recover(); recovered != nil {
			c.log.Error("ws_handler_panic", map[string]any{"error": fmt.Sprint(recovered)})
		}
		c.shutdown()
	}()

	for {
		messageType, data, err := c.conn.Read(c.ctx)
		if err != nil {
			// 客户端断开、正常关闭、超限（超限由库按 1009 关闭）都走这里。
			c.log.Info("ws_client_closed", map[string]any{
				"sessionId": c.sessionID,
				"error":     err.Error(),
			})
			return
		}
		if messageType != websocket.MessageText {
			c.fatal("invalid_frame_type", "Only text WebSocket frames are supported",
				websocket.StatusUnsupportedData, "binary_not_supported")
			return
		}
		if !c.enqueue(data) {
			return
		}
	}
}

// enqueue 把一帧放入待处理队列；超限时发错误帧并按 1008 关闭，返回 false。
func (c *connection) enqueue(data []byte) bool {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return false
	}
	if len(c.pending) >= maxPendingFrames || c.pendingBytes+len(data) > maxPendingBytes {
		pendingFrames := len(c.pending)
		pendingBytes := c.pendingBytes
		c.mu.Unlock()
		c.log.Warn("ws_pending_overflow", map[string]any{
			"pendingFrames":      pendingFrames,
			"pendingBytes":       pendingBytes,
			"attemptedFrameSize": len(data),
		})
		c.fatal("too_many_requests", "Pending frame limit exceeded", websocket.StatusPolicyViolation, "too_many_requests")
		return false
	}
	c.pending = append(c.pending, queuedFrame{data: data})
	c.pendingBytes += len(data)
	start := !c.inFlight
	if start {
		c.inFlight = true
	}
	c.mu.Unlock()

	if start {
		go c.drain()
	}
	return true
}

// drain 串行消费排队帧：一次只有一轮在跑，跑完再看队列（与 Node 的 drain 等价）。
func (c *connection) drain() {
	for {
		c.mu.Lock()
		if c.closed || len(c.pending) == 0 {
			c.inFlight = false
			c.mu.Unlock()
			return
		}
		next := c.pending[0]
		c.pending = c.pending[1:]
		c.pendingBytes -= len(next.data)
		c.mu.Unlock()

		c.processFrame(next.data)

		c.mu.Lock()
		stopped := c.closed
		c.mu.Unlock()
		if stopped {
			c.mu.Lock()
			c.inFlight = false
			c.mu.Unlock()
			return
		}
	}
}

// shutdown 收口连接：取消在途轮次（数据面据此中断上游并结算）、丢弃排队帧、关闭 socket。
func (c *connection) shutdown() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	droppedFrames := len(c.pending)
	droppedBytes := c.pendingBytes
	c.pending = nil
	c.pendingBytes = 0
	c.mu.Unlock()

	if droppedFrames > 0 {
		c.log.Warn("ws_pending_dropped_on_close", map[string]any{
			"droppedFrames": droppedFrames,
			"droppedBytes":  droppedBytes,
		})
	}
	c.cancel()
	// 客户端已经不可达或已经收到关闭帧，这里不再等对端回应。
	_ = c.conn.CloseNow()
}

// fatal 发一帧错误事件后按给定状态码关闭连接；同类错误只收效一次。
func (c *connection) fatal(code, message string, status websocket.StatusCode, reason string) {
	c.emitError(code, message)
	c.closeWith(status, reason)
}

// closeWith 发关闭帧并做有界握手。
//
// 库自身的 Close 已带 5 s 写 + 5 s 等的内建上限，且重复调用是无操作；reason 上限 125 字节，
// 故传进来的都是短标识符（与 Node 传 code 字符串同口径）。
func (c *connection) closeWith(status websocket.StatusCode, reason string) {
	c.log.Info("ws_client_close_initiated", map[string]any{"code": int(status), "reason": reason})
	_ = c.conn.Close(status, reason)
}

// emitError 发一帧 `{"type":"error","error":{code,message}}`（Node 的 emitErrorEvent 同形）。
func (c *connection) emitError(code, message string) {
	frame, err := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]string{"code": code, "message": message},
	})
	if err != nil {
		c.log.Error("ws_error_frame_marshal_failed", map[string]any{"error": err.Error()})
		return
	}
	if err := c.writeFrame(c.ctx, frame); err != nil {
		c.log.Warn("ws_send_failed", map[string]any{"reason": "outbound_send_error", "error": err.Error()})
	}
}

// writeFrame 写一个文本帧，写入受 outboundSendTimeout 约束。
func (c *connection) writeFrame(ctx context.Context, payload []byte) error {
	writeCtx, cancel := context.WithTimeout(ctx, outboundSendTimeout)
	defer cancel()
	return c.conn.Write(writeCtx, websocket.MessageText, payload)
}

// newSessionID 生成一个 UUIDv4 形状的连接会话 id（Node 用 randomUUID）；
// 数据面侧的形制校验是 `^[\w.-]+$` 且长度不超过 128。
func newSessionID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// 取不到随机数不阻塞建连：退化为时间戳形状仍是合法 id，只是唯一性弱。
		return fmt.Sprintf("session-%d", time.Now().UnixNano())
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(buf[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[0:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:32])
}

// resolveSecret 解析每进程内部密钥：显式参数 > 环境变量 > 生成一次。
func resolveSecret(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if fromEnv := strings.TrimSpace(os.Getenv(SecretEnv)); fromEnv != "" {
		return fromEnv
	}
	return newSessionID()
}

// stripClientHeaders 复制客户端握手头，剥掉逐跳头与全部 `x-cch-*` 保留头。
//
// 剥前缀是**安全边界**：否则外部客户端可以直接带上 `x-cch-client-transport` 等标记，
// 让数据面把普通 HTTP 请求误判成 WS 隧道请求（Node 侧靠同一剥离 + 密钥双重防护）。
func stripClientHeaders(header http.Header) http.Header {
	stripped := make(http.Header, len(header))
	for name, values := range header {
		lower := strings.ToLower(name)
		if hopByHopHeaders[lower] || strings.HasPrefix(lower, reservedHeaderPrefix) {
			continue
		}
		stripped[name] = append([]string(nil), values...)
	}
	return stripped
}
