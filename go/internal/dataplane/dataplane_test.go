package dataplane

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/gate"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是不依赖数据库的数据面测试：守卫缝隙用假实现，上游用 httptest，
// 因此它检验的是「链 → 选路 → 转发 → 写回」这条装配是否成立，而不是数据库语义
// （后者见 store_integration_test.go，按 env 门控）。

// ---- 假缝隙 ----

// fakeAuth 按固定表解析密钥。
type fakeAuth struct {
	reject error
	user   guard.User
	key    guard.Key
}

func (f fakeAuth) ResolveAPIKey(context.Context, string) (guard.AuthResolution, error) {
	if f.reject != nil {
		return guard.AuthResolution{}, f.reject
	}
	return guard.AuthResolution{User: f.user, Key: f.key}, nil
}

// User 实现 UserDirectory。
func (f fakeAuth) User(context.Context, int64) (guard.User, error) { return f.user, nil }

// MarkUserExpired 实现 UserExpiryMarker。
func (f fakeAuth) MarkUserExpired(context.Context, int64) error { return nil }

// fakeSettings 返回一份最小系统设置。
type fakeSettings struct{}

func (fakeSettings) FindSystemSettings(context.Context) (*store.SystemSettings, error) {
	return &store.SystemSettings{}, nil
}

// fakeEmptySource 同时充当敏感词与过滤器来源（空表）。
type fakeEmptySource struct{}

func (fakeEmptySource) SensitiveWords(context.Context) ([]guard.SensitiveWord, error) {
	return nil, nil
}

func (fakeEmptySource) RequestFilters(context.Context) ([]guard.RequestFilter, error) {
	return nil, nil
}

// fakeProvider 选路并写回上下文槽位。
type fakeProvider struct {
	selection pctx.ProviderSelection
}

func (f fakeProvider) Select(_ context.Context, pc *pctx.Context) (pctx.ProviderSelection, error) {
	pc.SetProvider(f.selection)
	return f.selection, nil
}

// fakeMessageWriter 模拟「守卫链开行」（真实实现在 store 里建行）。
type fakeMessageWriter struct {
	mu  sync.Mutex
	IDs []int64
}

func (f *fakeMessageWriter) EnsureContext(_ context.Context, pc *pctx.Context) error {
	f.mu.Lock()
	id := int64(len(f.IDs) + 1)
	f.IDs = append(f.IDs, id)
	f.mu.Unlock()
	return pc.SetMessageRequestID(id)
}

func (f *fakeMessageWriter) Count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.IDs)
}

// fakeCandidates 提供指向假上游的候选。
type fakeCandidates struct {
	url string
}

func (f fakeCandidates) Candidate(
	_ context.Context,
	selection pctx.ProviderSelection,
	_ SelectionFacts,
) (*forward.Candidate, route.Result, error) {
	return &forward.Candidate{
		Provider: forward.Provider{
			ID:   selection.ProviderID,
			Name: selection.Name,
			Type: convert.ProviderClaude,
			URL:  f.url,
			Key:  "fake-upstream-key",
		},
	}, route.Result{}, nil
}

// Failover 不做故障转移：返回无候选。
func (f fakeCandidates) Failover(context.Context, SelectionFacts) (*forward.Candidate, route.Result, error) {
	return nil, route.Result{}, nil
}

// fakeSettler 记录两次结算缝的调用。
type fakeSettler struct {
	mu        sync.Mutex
	nonStream []settledNonStream
	stream    []forward.StreamOutcome
	// delay 拖慢流式结算：仅用于结算屏障的测试（见 settle_barrier_test.go）。
	// 测试在发请求之前写入这个字段，之后只读，因此读取不加锁。
	delay time.Duration
}

type settledNonStream struct {
	ID     int64
	Status int
}

func (f *fakeSettler) NonStream(_ context.Context, pc *pctx.Context, result *forward.Result, failure *forward.Failure) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, _ := pc.MessageRequestID()
	status := 0
	if result != nil {
		status = result.StatusCode
	}
	if status == 0 && failure != nil {
		// 与生产实现同口径：尝试耗尽时状态码来自失败归因（见 storeSettler.NonStream）。
		status = failure.StatusCode
	}
	f.nonStream = append(f.nonStream, settledNonStream{ID: id, Status: status})
	return nil
}

func (f *fakeSettler) Stream(_ context.Context, _ *pctx.Context, outcome forward.StreamOutcome) error {
	if delay := f.delay; delay > 0 {
		time.Sleep(delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stream = append(f.stream, outcome)
	return nil
}

// settleCount 返回已完成的流式结算次数。
func (f *fakeSettler) settleCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.stream)
}

// ---- 装配 ----

// newTestHandler 造一个不依赖数据库的数据面。
func newTestHandler(t *testing.T, upstreamURL string, auth guard.AuthResolver) (*Handler, *fakeSettler, *fakeMessageWriter) {
	t.Helper()
	dialClient, err := dial.New(dial.Options{})
	if err != nil {
		t.Fatalf("拨号器构造失败: %v", err)
	}
	settler := &fakeSettler{}
	messages := &fakeMessageWriter{}
	handler, err := New(Options{
		Logger: logx.New(nil),
		Base: guard.Deps{
			Auth:           auth,
			Users:          auth.(fakeAuth),
			Settings:       fakeSettings{},
			Sensitive:      fakeEmptySource{},
			Filters:        fakeEmptySource{},
			Provider:       fakeProvider{selection: pctx.ProviderSelection{ProviderID: 7, Name: "假供应商", Type: "claude"}},
			MessageContext: messages,
		},
		Candidates: fakeCandidates{url: upstreamURL},
		Settlers: func(*RequestState) Settler {
			return settler
		},
		Forward: forward.Deps{Dial: dialClient},
		Stream: forward.StreamOptions{
			// 门控预留按「最坏前缀」计，预算太小会让尝试直接失败（ErrReservationExceedsBudget）。
			// 测试用进程级默认预算，与生产装配一致（见 assemble.go 的 gate.DefaultBudget()）。
			Budget: gate.DefaultBudget(),
		},
	})
	if err != nil {
		t.Fatalf("数据面构造失败: %v", err)
	}
	return handler, settler, messages
}

// ---- 测试 ----

// 未实现的路由必须交回回退处理器，而不是 501 或 404。
func TestUnimplementedRouteFallsBack(t *testing.T) {
	called := false
	handler, _, _ := newTestHandler(t, "http://127.0.0.1:1", fakeAuth{})
	handler.options.Fallback = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("node"))
	})

	// 夹具必须是**真正未知**的路径：族透传把 Node 的 catch-all 语义搬进来之后，
	// /v1/models、count_tokens、/v1beta/*、GET /v1/messages 都已被 Go 接管
	// （它们的归属见 routes_family_test.go 的台账钉子），拿它们当「未实现」会假红。
	for _, target := range []struct{ method, path string }{
		{http.MethodGet, "/v1/definitely-not-real-xyz"},
		{http.MethodPost, "/v1/unknown/deep/path"},
		{http.MethodPost, "/v2/messages"},
		{http.MethodGet, "/v1/messages/extra/segment"},
	} {
		called = false
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(target.method, target.path, nil))
		if !called {
			t.Errorf("%s %s 应回退到 Node，实际未回退（状态 %d）", target.method, target.path, recorder.Code)
		}
	}
}

// 归一化路径也要命中路由：尾斜杠与重复斜杠不得让请求静默落到回退。
func TestRouteMatchNormalizesPath(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/messages/", "/v1//messages", "/V1/Messages"} {
		if _, ok := matchRoute(http.MethodPost, path); !ok {
			t.Errorf("路径 %q 应命中 POST /v1/messages", path)
		}
	}
	// GET /v1/messages 现在**在** Go 承载范围内：Node 的 catch-all 会代理它（族目录按路径建表，
	// 不分方法），Go 的族透传与它同义。真正不在范围内的仍是未知路径。
	if _, ok := matchRoute(http.MethodGet, "/v1/messages"); !ok {
		t.Error("GET /v1/messages 应被族透传命中（与 Node 的 catch-all 同义）")
	}
	if _, ok := matchRoute(http.MethodGet, "/v1/definitely-not-real-xyz"); ok {
		t.Error("未知路径不该被 Go 接管：撤 Node 前必须保持可回退")
	}
}

// 非流式：状态码、上游头部与正文原样写回，并结算一次。
func TestNonStreamProxiesUpstreamResponse(t *testing.T) {
	const payload = `{"id":"msg_1","type":"message","usage":{"input_tokens":3,"output_tokens":5}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("上游路径应为 /v1/messages，收到 %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Marker", "kept")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, payload)
	}))
	defer upstream.Close()

	handler, settler, messages := newTestHandler(t, upstream.URL, fakeAuth{
		user: guard.User{ID: 1, Name: "甲", IsEnabled: true},
		key:  guard.Key{ID: 2, Name: "k", UserID: 1},
	})
	recorder := httptest.NewRecorder()
	body := strings.NewReader(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", "fake-client-key")
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，收到 %d（%s）", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Body.String(); got != payload {
		t.Errorf("正文应原样透传，收到 %q", got)
	}
	if got := recorder.Header().Get("X-Upstream-Marker"); got != "kept" {
		t.Errorf("上游头部应保留，收到 %q", got)
	}
	if messages.Count() != 1 {
		t.Errorf("守卫链应只开一行，实际 %d", messages.Count())
	}
	if len(settler.nonStream) != 1 {
		t.Fatalf("非流式应结算一次，实际 %d", len(settler.nonStream))
	}
	if settler.nonStream[0].Status != http.StatusOK || settler.nonStream[0].ID != 1 {
		t.Errorf("结算应指向守卫链开的那一行且状态 200，收到 %+v", settler.nonStream[0])
	}
}

// 流式：SSE 必须逐事件到达（在假上游仍在写的时候客户端就能读到前面的帧）。
func TestStreamDeliversEventsIncrementally(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		// 首帧必须是「决定性内容帧」：门控只在看到真实内容或错误时才提交，
		// message_start 属中性帧，单发它会让客户端一直等不到响应头。
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		_, _ = io.WriteString(w,
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"甲\"}}\n\n")
		flusher.Flush()
		// 挡住后续帧，直到测试确认前一帧已经到达客户端。
		<-release
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	handler, _, _ := newTestHandler(t, upstream.URL, fakeAuth{
		user: guard.User{ID: 1, Name: "甲", IsEnabled: true},
		key:  guard.Key{ID: 2, Name: "k", UserID: 1},
	})
	server := httptest.NewServer(handler)
	defer server.Close()

	streamRequest, err := http.NewRequest(
		http.MethodPost,
		server.URL+"/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`),
	)
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	streamRequest.Header.Set("Content-Type", "application/json")
	streamRequest.Header.Set("x-api-key", "fake-client-key")
	response, err := server.Client().Do(streamRequest)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，收到 %d", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("内容类型应为 SSE，收到 %q", got)
	}

	// 在上游仍被挡住的情况下读第一帧：这证明写回是逐帧 flush 的，而不是攒到最后。
	buffer := make([]byte, 128)
	type readResult struct {
		count int
		err   error
	}
	read := make(chan readResult, 1)
	go func() {
		n, readErr := response.Body.Read(buffer)
		read <- readResult{count: n, err: readErr}
	}()

	select {
	case result := <-read:
		if result.err != nil && !errors.Is(result.err, io.EOF) {
			t.Fatalf("读第一帧失败: %v", result.err)
		}
		first := string(buffer[:result.count])
		if !strings.Contains(first, "message_start") && !strings.Contains(first, "content_block_delta") {
			t.Fatalf("首帧应含 SSE 帧，收到 %q", first)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("首帧未在上游释放前到达：流式写回被缓冲了")
	}
	close(release)

	rest, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("读余下帧失败: %v", err)
	}
	if !strings.Contains(string(rest), "message_stop") {
		t.Fatalf("余下帧应含 message_stop，收到 %q", string(rest))
	}
}

// 守卫拦截：抢答响应原样写回，且不碰上游、不开行、不结算。
func TestGuardRejectionShortCircuits(t *testing.T) {
	upstreamHits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	handler, settler, messages := newTestHandler(t, upstream.URL, fakeAuth{reject: guard.ErrKeyNotFound})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"x"}`))
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("密钥不存在应返回 401，收到 %d（%s）", recorder.Code, recorder.Body.String())
	}
	if upstreamHits != 0 {
		t.Errorf("被拦截的请求不得接触上游，实际上游收到 %d 次", upstreamHits)
	}
	if messages.Count() != 0 {
		t.Errorf("被拦截的请求不得开行，实际 %d 行", messages.Count())
	}
	if len(settler.nonStream) != 0 {
		t.Errorf("被拦截的请求不得走转发结算，实际 %d 次", len(settler.nonStream))
	}
}

// 上游 500：状态码透传给客户端，并结算一次（终态不是成功）。
func TestUpstreamErrorIsPassedThrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	}))
	defer upstream.Close()

	handler, settler, _ := newTestHandler(t, upstream.URL, fakeAuth{
		user: guard.User{ID: 1, Name: "甲", IsEnabled: true},
		key:  guard.Key{ID: 2, Name: "k", UserID: 1},
	})
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"x"}`))
	request.Header.Set("x-api-key", "fake-client-key")
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("上游 500 应原样透传，收到 %d（%s）", recorder.Code, recorder.Body.String())
	}
	if len(settler.nonStream) != 1 || settler.nonStream[0].Status != http.StatusInternalServerError {
		t.Fatalf("应按 500 结算一次，收到 %+v", settler.nonStream)
	}
}

// 没有回退目标时，未实现路由如实 404，而不是假装数据面不存在。
func TestUnimplementedRouteWithoutFallback(t *testing.T) {
	handler, _, _ := newTestHandler(t, "http://127.0.0.1:1", fakeAuth{})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("无回退目标时应 404，收到 %d", recorder.Code)
	}
}
