package tracing

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// capturedRequest 是一条到达假收集器的请求（只留断言需要的部分）。
type capturedRequest struct {
	path            string
	authorization   string
	contentEncoding string
	body            []byte
}

// collector 是假 Langfuse 收集器：默认立即回 status，可切换为「卡住不响应」以验证不阻塞。
type collector struct {
	server *httptest.Server

	mu       sync.Mutex
	requests []capturedRequest
	gate     chan struct{}
	gateOnce sync.Once
	// sent 每收到一条请求即投一个信号：用信号等结果，不用 sleep 轮询。
	sent chan struct{}
}

func newCollector(t *testing.T, status int) *collector {
	t.Helper()
	c := &collector{sent: make(chan struct{}, 64)}
	c.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.requests = append(c.requests, capturedRequest{
			path:            r.URL.Path,
			authorization:   r.Header.Get("Authorization"),
			contentEncoding: r.Header.Get("Content-Encoding"),
			body:            body,
		})
		gate := c.gate
		c.mu.Unlock()
		select {
		case c.sent <- struct{}{}:
		default:
		}
		if gate != nil {
			<-gate
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(func() {
		c.release()
		c.server.Close()
	})
	return c
}

// hold 让收集器卡住（此后到达的请求都不返回，直到 release）。
func (c *collector) hold() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gate == nil {
		c.gate = make(chan struct{})
	}
}

func (c *collector) release() {
	c.gateOnce.Do(func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.gate != nil {
			close(c.gate)
		}
	})
}

func (c *collector) captured() []capturedRequest {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]capturedRequest(nil), c.requests...)
}

func (c *collector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requests)
}

// waitForRequests 等到至少 n 条请求到达（用信号而非轮询）；超时即失败。
func (c *collector) waitForRequests(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for c.count() < n {
		select {
		case <-c.sent:
		case <-deadline:
			t.Fatalf("等待 %d 条请求超时（实际 %d）", n, c.count())
		}
	}
}

// recordingLogger 记录日志条目，用于断言「日志里不得出现凭据」，以及失败时确实留痕。
type recordingLogger struct {
	mu      sync.Mutex
	entries []string
}

func (l *recordingLogger) Warn(event string, fields map[string]any) {
	l.append("WARN", event, fields)
}

func (l *recordingLogger) Debug(event string, fields map[string]any) {
	l.append("DEBUG", event, fields)
}

func (l *recordingLogger) append(level, event string, fields map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	// 字段整体格式化成一行：断言要的是「日志文本里没有出现凭据」，
	// 而不是「某个字段不存在」。
	l.entries = append(l.entries, fmt.Sprintf("%s %s %v", level, event, fields))
}

func (l *recordingLogger) lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.entries...)
}

func (l *recordingLogger) hasEvent(event string) bool {
	for _, line := range l.lines() {
		if strings.Contains(line, event) {
			return true
		}
	}
	return false
}
