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

// 拨号失败的可判别分类。调用方按 errors.Is 判定，并据此决定计费与熔断归因。
var (
	// ErrUnsupportedUpstreamTransport 表示该供应商需要当前不支持的拨号形态
	// （SOCKS 或自定义 dispatcher 代理）。必须显式失败，不允许静默直连。
	ErrUnsupportedUpstreamTransport = errors.New("dial: unsupported upstream transport")
	// ErrRequestBuild 表示请求本身无法构造（URL 非法、方法非法等）。
	ErrRequestBuild = errors.New("dial: request build failed")
	// ErrConnect 表示建连阶段失败（DNS、TCP、TLS）。
	ErrConnect = errors.New("dial: connect failed")
	// ErrConnectTimeout 表示建连或 TLS 握手超时。
	ErrConnectTimeout = errors.New("dial: connect timeout")
	// ErrHeadersTimeout 表示请求已写出但响应头未在期限内到达。
	ErrHeadersTimeout = errors.New("dial: response headers timeout")
	// ErrBodyIdleTimeout 表示响应体在两次可读数据之间超过空闲上限。
	ErrBodyIdleTimeout = errors.New("dial: response body idle timeout")
	// ErrUpstreamClosed 表示上游在响应完成前关闭了连接。
	ErrUpstreamClosed = errors.New("dial: upstream closed connection early")
	// ErrPoolAdmissionExceeded 表示上游连接准入已满。此时立刻失败，不排队。
	ErrPoolAdmissionExceeded = errors.New("dial: upstream connection admission exceeded")
	// ErrContextCanceled 表示调用方取消（含客户端断开）。
	ErrContextCanceled = errors.New("dial: request canceled")
)

// classifyTransportError 把 net/http 与 net 的错误折叠成可用 errors.Is 判别的分类。
func classifyTransportError(err error, options Options) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("%w: %v", ErrContextCanceled, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		// 上下文超时无法区分建连与等响应头；按「未拿到响应头」归类，与 Node 侧
		// response timeout 的语义一致（那是最常见的一种）。
		return fmt.Errorf("%w: %v", ErrHeadersTimeout, err)
	}

	var netError net.Error
	if errors.As(err, &netError) && netError.Timeout() {
		if isConnectPhaseTimeout(netError.Error()) {
			return fmt.Errorf("%w: %v", ErrConnectTimeout, err)
		}
		return fmt.Errorf("%w: %v", ErrHeadersTimeout, err)
	}

	var urlError *url.Error
	if errors.As(err, &urlError) {
		if isClosedConnection(urlError.Err) {
			return fmt.Errorf("%w: %v", ErrUpstreamClosed, urlError.Err)
		}
	}

	if isClosedConnection(err) {
		return fmt.Errorf("%w: %v", ErrUpstreamClosed, err)
	}
	return fmt.Errorf("%w: %v", ErrConnect, err)
}

// isConnectPhaseTimeout 判定超时文本是否来自 HTTP 传输的建连阶段。
func isConnectPhaseTimeout(text string) bool {
	markers := []string{"dial ", "TLS handshake", "connect: connection", "no route to host"}
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func isClosedConnection(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, io.EOF) {
		return true
	}
	text := strings.ToLower(err.Error())
	markers := []string{
		"connection reset by peer",
		"unexpected eof",
		"broken pipe",
		"use of closed network connection",
		"server closed idle connection",
		"http: server closed",
	}
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// idleTimeoutBody 给响应体加上「两次读之间最长间隔」约束。
//
// net/http 只提供 ResponseHeaderTimeout，没有响应体空闲超时；这里用读前重置的定时器实现：
// 超时即关闭底层 body，使后续读以 ErrBodyIdleTimeout 结束。释放准入名额与停止定时器
// 统一由 sync.Once 收口，避免 Close 与超时路径重复释放。
type idleTimeoutBody struct {
	body    io.ReadCloser
	limit   time.Duration
	timerMu sync.Mutex
	timer   *time.Timer
	timed   bool
	release func()
	once    sync.Once
}

func newIdleTimeoutBody(body io.ReadCloser, limit time.Duration, release func()) *idleTimeoutBody {
	wrapped := &idleTimeoutBody{body: body, limit: limit, release: release}
	if limit > 0 {
		wrapped.timer = time.AfterFunc(limit, func() {
			wrapped.timerMu.Lock()
			wrapped.timed = true
			wrapped.timerMu.Unlock()
			_ = wrapped.body.Close()
		})
	}
	return wrapped
}

func (b *idleTimeoutBody) Read(buffer []byte) (int, error) {
	if b.timer != nil {
		b.timer.Reset(b.limit)
	}
	read, err := b.body.Read(buffer)
	if err == nil {
		return read, nil
	}

	b.timerMu.Lock()
	timed := b.timed
	b.timerMu.Unlock()

	if timed {
		return read, fmt.Errorf("%w after %s", ErrBodyIdleTimeout, b.limit)
	}
	if errors.Is(err, io.EOF) {
		return read, io.EOF
	}
	if isClosedConnection(err) {
		return read, fmt.Errorf("%w: %v", ErrUpstreamClosed, err)
	}
	return read, err
}

func (b *idleTimeoutBody) Close() error {
	err := b.body.Close()
	if b.timer != nil {
		b.timer.Stop()
	}
	b.once.Do(b.release)
	return err
}

var _ io.ReadCloser = (*idleTimeoutBody)(nil)
var _ = http.MethodPost
