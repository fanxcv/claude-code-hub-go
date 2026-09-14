package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/usagefeed"
	"github.com/redis/go-redis/v9"
)

// 本文件是 `GET /usage-logs/stream`（推送模式）的单元测试。
//
// 断言的判据是**SSE 报文的原始字节**（前缀 `event:` / `data:` / `:`），而不是封装后的结构：
// 前端 `EventSource` 解析的就是这些字节，任何形状改动都必须让这些用例转红。

// stubSubscription 是注入用的订阅替身：由测试显式投递信号，不依赖 Redis。
type stubSubscription struct {
	mu     sync.Mutex
	closed bool
	signal chan usagefeed.Signal
}

func newStubSubscription() *stubSubscription {
	return &stubSubscription{signal: make(chan usagefeed.Signal, 8)}
}

func (s *stubSubscription) Wait(ctx context.Context) (usagefeed.Signal, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return usagefeed.Signal{}, usagefeed.ErrSubscriptionClosed
	}
	s.mu.Unlock()

	select {
	case signal, open := <-s.signal:
		if !open {
			return usagefeed.Signal{}, usagefeed.ErrSubscriptionClosed
		}
		return signal, nil
	case <-ctx.Done():
		return usagefeed.Signal{}, ctx.Err()
	}
}

func (s *stubSubscription) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	close(s.signal)
}

// deliver 投递一条信号；订阅已关闭时忽略。
//
// 只在**持锁**时检查关闭状态：与真实实现一致（关闭后再投递不该 panic，也不该让
// 已结束的连接继续收信号）。
func (s *stubSubscription) deliver(signal usagefeed.Signal) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.signal <- signal
	return true
}

func (s *stubSubscription) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// stubFeed 是 Deps.NewRowsFeed 的替身：每条 Subscribe 返回一个新订阅。
type stubFeed struct {
	mu      sync.Mutex
	subs    []*stubSubscription
	dispose int
}

func (f *stubFeed) Subscribe() (usagefeed.Subscription, func()) {
	sub := newStubSubscription()
	f.mu.Lock()
	f.subs = append(f.subs, sub)
	f.mu.Unlock()
	return sub, func() {
		f.mu.Lock()
		f.dispose++
		f.mu.Unlock()
		sub.Close()
	}
}

func (f *stubFeed) last() *stubSubscription {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.subs) == 0 {
		return nil
	}
	return f.subs[len(f.subs)-1]
}

func (f *stubFeed) disposed() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dispose
}

// registeredStreamRouter 装配一条 /usage-logs/stream 路由（Guard 放行）。
func registeredStreamRouter(feed usagefeed.Feed) *Router {
	router := New(Options{Deps: Deps{Guard: ulStubGuard{}, Problems: NewProblems(nil)}})
	RegisterUsageLogsStream(router, Deps{Guard: ulStubGuard{}, Problems: NewProblems(nil), NewRowsFeed: feed})
	return router
}

// sseRecorder 是能在写入过程中被读取的 ResponseWriter（SSE 是长连接，不能等 handler 返回）。
type sseRecorder struct {
	header  http.Header
	status  int
	mu      sync.Mutex
	body    strings.Builder
	flushed int
	written chan struct{}
	// failAfter 为 >0 时，第 N 次写入之后开始返回错误（模拟客户端断开）。
	failAfter int
	writes    int
}

func newSSERecorder() *sseRecorder {
	return &sseRecorder{header: http.Header{}, written: make(chan struct{}, 64)}
}

func (r *sseRecorder) Header() http.Header { return r.header }

func (r *sseRecorder) WriteHeader(status int) {
	r.mu.Lock()
	if r.status == 0 {
		r.status = status
	}
	r.mu.Unlock()
}

func (r *sseRecorder) Write(payload []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.writes++
	if r.failAfter > 0 && r.writes > r.failAfter {
		return 0, errors.New("模拟客户端已断开")
	}
	r.body.Write(payload)
	select {
	case r.written <- struct{}{}:
	default:
	}
	return len(payload), nil
}

func (r *sseRecorder) Flush() {
	r.mu.Lock()
	r.flushed++
	r.mu.Unlock()
}

func (r *sseRecorder) text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body.String()
}

func (r *sseRecorder) flushCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.flushed
}

// waitFor 等到 body 满足条件（或超时）。
func (r *sseRecorder) waitFor(t *testing.T, timeout time.Duration, predicate func(string) bool) string {
	t.Helper()
	deadline := time.After(timeout)
	for {
		if text := r.text(); predicate(text) {
			return text
		}
		select {
		case <-r.written:
		case <-deadline:
			return r.text()
		}
	}
}

// 契约逐字断言：状态码、三个响应头、ready 事件的形状。
func TestUsageLogsStreamHandshakeContract(t *testing.T) {
	feed := &stubFeed{}
	router := registeredStreamRouter(feed)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/usage-logs/stream", nil)
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	request = request.WithContext(ctx)

	recorder := newSSERecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		router.ServeHTTP(recorder, request)
	}()

	body := recorder.waitFor(t, 2*time.Second, func(text string) bool {
		return strings.Contains(text, "event: ready")
	})
	if !strings.Contains(body, "event: ready\ndata: {}\n\n") {
		t.Fatalf("ready 事件形状不符契约，实际 body=%q", body)
	}
	if recorder.status != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d", recorder.status)
	}
	header := recorder.Header()
	if got := header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("Content-Type 应为 text/event-stream，实际 %q", got)
	}
	if got := header.Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control 应为 no-cache，实际 %q", got)
	}
	if got := header.Get("Connection"); got != "keep-alive" {
		t.Fatalf("Connection 应为 keep-alive，实际 %q", got)
	}
	if recorder.flushCount() == 0 {
		t.Fatal("SSE 必须 flush，否则事件会积在缓冲区里")
	}

	// 断开连接：handler 应退出，且订阅被注销。
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("连接取消后 handler 未退出")
	}
	if feed.disposed() == 0 {
		t.Fatal("连接结束后应注销订阅（否则订阅泄漏）")
	}
	if !feed.last().isClosed() {
		t.Fatal("连接结束后订阅应已关闭")
	}
}

// 收到信号即发 new-rows，且 data 里**只有** maxId、minId 与 count 三个键——
// 这是「信号式推送」的核心契约：任何行内容泄漏都必须让本用例转红。
//
// 键集合为什么是这三个：`maxId` 是增量拉取的高水位，`minId` 是同一窗口的**下界**
// （行 id 是开行顺序而非结算顺序，晚结算的低 id 行只能靠它被带回），`count` 是提示。
// 均为整数，无一提及行内容。
func TestUsageLogsStreamEmitsSignalOnly(t *testing.T) {
	feed := &stubFeed{}
	router := registeredStreamRouter(feed)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/usage-logs/stream", nil)
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	request = request.WithContext(ctx)

	recorder := newSSERecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		router.ServeHTTP(recorder, request)
	}()

	recorder.waitFor(t, 2*time.Second, func(text string) bool {
		return strings.Contains(text, "event: ready")
	})

	subscription := feed.last()
	if subscription == nil {
		t.Fatal("handler 未登记订阅")
	}
	if !subscription.deliver(usagefeed.Signal{MaxID: 987654, MinID: 987651, Count: 3}) {
		t.Fatal("投递信号失败（订阅应仍然打开）")
	}

	body := recorder.waitFor(t, 2*time.Second, func(text string) bool {
		return strings.Contains(text, "event: new-rows")
	})
	if !strings.Contains(body, "event: new-rows\n") {
		t.Fatalf("未收到 new-rows 事件，实际 body=%q", body)
	}

	// 解析 data 行并断言键集合精确相等。
	payload := extractSSEData(t, body, "new-rows")
	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("data 不是合法 JSON（%q）: %v", payload, err)
	}
	if len(decoded) != 3 {
		t.Fatalf("信号只允许 maxId/minId/count 三个键，实际 %v", decoded)
	}
	if decoded["maxId"] != float64(987654) || decoded["count"] != float64(3) ||
		decoded["minId"] != float64(987651) {
		t.Fatalf("信号值不符: %v", decoded)
	}

	cancel()
	<-done
}

// extractSSEData 取指定事件之后紧跟的 data 行内容。
func extractSSEData(t *testing.T, body, event string) string {
	t.Helper()
	marker := "event: " + event + "\n"
	index := strings.Index(body, marker)
	if index < 0 {
		t.Fatalf("body 里没有 %s 事件: %q", event, body)
	}
	rest := body[index+len(marker):]
	if !strings.HasPrefix(rest, "data: ") {
		t.Fatalf("%s 事件后应紧跟 data 行，实际 %q", event, rest)
	}
	line := rest[len("data: "):]
	if end := strings.Index(line, "\n"); end >= 0 {
		line = line[:end]
	}
	return line
}

// 写失败（客户端断开）时 handler 必须收尾，且注销订阅——否则每条断开的连接都漏一个订阅。
func TestUsageLogsStreamWriteFailureClosesSubscription(t *testing.T) {
	feed := &stubFeed{}
	router := registeredStreamRouter(feed)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/usage-logs/stream", nil)
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	request = request.WithContext(ctx)

	// ready 成功（第 1 次写），随后的写入全部失败。
	recorder := newSSERecorder()
	recorder.failAfter = 1
	done := make(chan struct{})
	go func() {
		defer close(done)
		router.ServeHTTP(recorder, request)
	}()

	recorder.waitFor(t, 2*time.Second, func(text string) bool {
		return strings.Contains(text, "event: ready")
	})
	subscription := feed.last()
	if subscription == nil {
		t.Fatal("handler 未登记订阅")
	}
	subscription.deliver(usagefeed.Signal{MaxID: 1, Count: 1})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("写失败后 handler 未退出")
	}
	if !subscription.isClosed() {
		t.Fatal("写失败后订阅应被关闭")
	}
}

// 未装配推送面时不注册路由（请求照旧回退 Node）——不能让「连得上但没信号」的流冒充推送。
func TestRegisterUsageLogsStreamWithoutFeedRegistersNothing(t *testing.T) {
	router := New(Options{Deps: Deps{Guard: ulStubGuard{}}})
	RegisterUsageLogsStream(router, Deps{Guard: ulStubGuard{}})
	if router.RouteCount() != 0 {
		t.Fatalf("未装配 NewRowsFeed 时不应注册路由，实际 %d 条", router.RouteCount())
	}
}

// 装配后的路由表：一条、read 档、模块与 operationId 齐。
func TestRegisterUsageLogsStreamRouteTable(t *testing.T) {
	router := registeredStreamRouter(&stubFeed{})
	routes := router.RouteList()
	if len(routes) != 1 {
		t.Fatalf("应注册 1 条路由，实际 %d: %+v", len(routes), routes)
	}
	route := routes[0]
	if route.Method != http.MethodGet || route.Path != "/usage-logs/stream" {
		t.Fatalf("路由不符: %s %s", route.Method, route.Path)
	}
	if route.Access != AccessRead {
		t.Fatalf("权限档位应为 read，实际 %s", route.Access)
	}
	if route.Module != "usage-logs" || route.OperationID == "" {
		t.Fatalf("模块或 operationId 缺失: %+v", route)
	}
}

// 心跳：连接空闲时必须周期性发注释行（`: keep-alive`），否则中间设备会按空闲断开，
// 前端的推送模式就变成「过一会儿就断」。
//
// 用可注入的心跳间隔（`usageLogsStreamHeartbeatInterval`）而不是等真实的 15s：
// 门禁纪律禁止测试等真实长超时。
func TestUsageLogsStreamEmitsHeartbeat(t *testing.T) {
	original := usageLogsStreamHeartbeatInterval
	usageLogsStreamHeartbeatInterval = 40 * time.Millisecond
	t.Cleanup(func() { usageLogsStreamHeartbeatInterval = original })

	feed := &stubFeed{}
	router := registeredStreamRouter(feed)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/usage-logs/stream", nil)
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	request = request.WithContext(ctx)

	recorder := newSSERecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		router.ServeHTTP(recorder, request)
	}()

	// 不发任何信号：只靠心跳。
	body := recorder.waitFor(t, 3*time.Second, func(text string) bool {
		return strings.Contains(text, ": keep-alive\n\n")
	})
	if !strings.Contains(body, ": keep-alive\n\n") {
		t.Fatalf("空闲连接未收到心跳注释行，实际 body=%q", body)
	}
	// 心跳是注释：不得伪装成事件（否则前端会为它触发刷新）。
	if strings.Contains(body, "event: keep-alive") {
		t.Fatal("心跳必须是注释行，不能是 event")
	}

	cancel()
	<-done
}

// 未授权：守卫拒绝时**不得**进入本文件（也就不会建立长连接）。
//
// 契约把「401 并结束流」落在既有 AuthGuard 上；这条用例钉的是路由确实过了守卫，
// 且在守卫拒绝时连 SSE 响应头都不会下发。
func TestUsageLogsStreamRejectionNeverOpensStream(t *testing.T) {
	feed := &stubFeed{}
	guard := denyingStreamGuard{}
	router := New(Options{Deps: Deps{Guard: guard, Problems: NewProblems(nil)}})
	RegisterUsageLogsStream(router, Deps{Guard: guard, Problems: NewProblems(nil), NewRowsFeed: feed})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/usage-logs/stream", nil))

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("未授权应回 401，实际 %d", recorder.Code)
	}
	if contentType := recorder.Header().Get("Content-Type"); strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("被拒的请求不该带 SSE 响应头，实际 %q", contentType)
	}
	if feed.disposed() != 0 {
		t.Fatal("被拒的请求不该登记订阅")
	}
}

// denyingStreamGuard 模拟认证守卫：直接 401，不放行到 next。
type denyingStreamGuard struct{}

func (denyingStreamGuard) Wrap(_ AccessLevel, _ http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
	})
}

// Redis 不可达时：SSE 连接照常建立（收到 ready）、不发信号、不 panic、不刷屏日志。
//
// 这是「降级」契约的服务端一半：前端以「有没有收到过 new-rows」判定推送是否可用，
// 而不是靠连接成功。另一半（主请求仍 200）见 dataplane 的
// TestIntegrationUsageFeedRedisFailureKeepsRequestsWorking。
func TestUsageLogsStreamWithRedisDownStillOpensAndStaysQuiet(t *testing.T) {
	broken := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond})
	t.Cleanup(func() { _ = broken.Close() })
	hub := usagefeed.NewHub(usagefeed.Options{Redis: broken})
	t.Cleanup(func() { _ = hub.Close() })

	router := registeredStreamRouter(hub)

	request := httptest.NewRequest(http.MethodGet, "/api/v1/usage-logs/stream", nil)
	ctx, cancel := context.WithCancel(request.Context())
	defer cancel()
	request = request.WithContext(ctx)

	recorder := newSSERecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		router.ServeHTTP(recorder, request)
	}()

	body := recorder.waitFor(t, 3*time.Second, func(text string) bool {
		return strings.Contains(text, "event: ready")
	})
	if !strings.Contains(body, "event: ready\ndata: {}\n\n") {
		t.Fatalf("Redis 不可达时仍应收到 ready，实际 body=%q", body)
	}
	if strings.Contains(body, "event: new-rows") {
		t.Fatal("Redis 不可达时不该凭空产生 new-rows")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Redis 不可达时连接取消后 handler 未退出")
	}
}
