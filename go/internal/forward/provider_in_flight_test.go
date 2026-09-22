package forward

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 本文件钉住「供应商并发名额」这条缝（Deps.ProviderInFlight）在转发层的语义：
// 每次尝试登记一次、名额随 body 关闭或拨号失败归还、被拒时按饱和失败并换家。
//
// 为什么必须逐条构造：这条缝最危险的失败形态是**泄漏**（名额不还，渠道被永久占住），
// 而泄漏只发生在异常路径上——正常路径的用例全绿也证明不了它。

// inFlightSpy 记录登记与释放的次数。
type inFlightSpy struct {
	mu        sync.Mutex
	acquires  int
	releases  int
	limits    []int
	providers []int64
	// denied 里的渠道一律拒绝（模拟满员）。
	denied map[int64]bool
}

func newInFlightSpy() *inFlightSpy { return &inFlightSpy{denied: map[int64]bool{}} }

func (s *inFlightSpy) acquire(_ context.Context, providerID int64, limit int) ProviderInFlightResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acquires++
	s.limits = append(s.limits, limit)
	s.providers = append(s.providers, providerID)
	if s.denied[providerID] {
		return ProviderInFlightResult{Current: limit}
	}
	return ProviderInFlightResult{Allowed: true, Current: 1, Release: s.release}
}

func (s *inFlightSpy) release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releases++
}

func (s *inFlightSpy) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acquires, s.releases
}

// inFlightCandidate 构造一个带并发上限的候选。
func inFlightCandidate(id int64, url string, limit int) *Candidate {
	candidate := newTestCandidate(id, "供应商甲", url, 0)
	candidate.Provider.LimitConcurrentSessions = limit
	return candidate
}

// TestProviderInFlightRegistersAndReleasesOnSuccess：成功路径登记一次、终态归还一次。
func TestProviderInFlightRegistersAndReleasesOnSuccess(t *testing.T) {
	server := fakeServer(t, http.StatusOK, `{"id":"msg_1","content":[{"type":"text","text":"pong"}]}`)
	spy := newInFlightSpy()
	deps := Deps{
		Dial:             newTestDial(t),
		Facts:            newTestFacts(),
		Limits:           Limits{RetryDelay: time.Millisecond},
		ProviderInFlight: spy.acquire,
	}

	if _, err := Forward(context.Background(), nil, inFlightCandidate(1, server.URL, 5), deps); err != nil {
		t.Fatalf("转发应成功: %v", err)
	}
	acquires, releases := spy.counts()
	if acquires != 1 {
		t.Fatalf("登记 %d 次，应为 1（每次尝试一次）", acquires)
	}
	if releases != 1 {
		t.Fatalf("归还 %d 次，应为 1（名额泄漏）", releases)
	}
	if len(spy.limits) != 1 || spy.limits[0] != 5 {
		t.Fatalf("登记收到的上限 = %v，应为 [5]（计划未携带 provider 的上限）", spy.limits)
	}
}

// TestProviderInFlightReleasesOnDialFailure：拨号失败没有 body 可挂，必须就地归还。
func TestProviderInFlightReleasesOnDialFailure(t *testing.T) {
	// 起一个立刻关闭的服务，拿到一个必然拒绝连接的地址。
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()

	spy := newInFlightSpy()
	deps := Deps{
		Dial:             newTestDial(t),
		Facts:            newTestFacts(),
		Limits:           Limits{RetryDelay: time.Millisecond},
		ProviderInFlight: spy.acquire,
	}

	if _, err := Forward(context.Background(), nil, inFlightCandidate(1, url, 5), deps); err == nil {
		t.Fatal("连不上的上游应失败")
	}
	acquires, releases := spy.counts()
	if acquires != 1 || releases != 1 {
		t.Fatalf("登记/归还 = %d/%d，应为 1/1（拨号失败路径泄漏名额）", acquires, releases)
	}
}

// TestProviderInFlightReleasesOnUpstreamError：上游 5xx 走「拿到响应再判失败」，
// 名额随 body 关闭归还（executeAttempt 的 defer）。
func TestProviderInFlightReleasesOnUpstreamError(t *testing.T) {
	server := fakeServer(t, http.StatusInternalServerError, `{"error":{"message":"boom"}}`)
	spy := newInFlightSpy()
	deps := Deps{
		Dial:             newTestDial(t),
		Facts:            newTestFacts(),
		Limits:           Limits{RetryDelay: time.Millisecond},
		ProviderInFlight: spy.acquire,
	}

	if _, err := Forward(context.Background(), nil, inFlightCandidate(1, server.URL, 5), deps); err == nil {
		t.Fatal("上游 500 应失败")
	}
	if acquires, releases := spy.counts(); acquires != 1 || releases != 1 {
		t.Fatalf("登记/归还 = %d/%d，应为 1/1（上游失败路径泄漏名额）", acquires, releases)
	}
}

// TestProviderInFlightReleasesOnReadTimeout：非流式总超时在**读体阶段**触发，
// 名额同样必须归还（这条路径不经过拨号失败分支）。
func TestProviderInFlightReleasesOnReadTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_1","content":[`))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)

	candidate := inFlightCandidate(1, server.URL, 5)
	candidate.Provider.RequestTimeoutNonStreamingMS = 50

	spy := newInFlightSpy()
	deps := Deps{
		Dial:             newTestDial(t),
		Facts:            newTestFacts(),
		Limits:           Limits{RetryDelay: time.Millisecond},
		ProviderInFlight: spy.acquire,
	}

	if _, err := Forward(context.Background(), nil, candidate, deps); err == nil {
		t.Fatal("读体超时应失败")
	}
	if acquires, releases := spy.counts(); acquires != 1 || releases != 1 {
		t.Fatalf("登记/归还 = %d/%d，应为 1/1（读体超时路径泄漏名额）", acquires, releases)
	}
}

// TestProviderInFlightReleasesOnClientAbort：客户端在**读体阶段**断开（ctx 取消）。
//
// 为何刻意在拿到响应头之后取消：名额是在拨号成功后才挂到 body 上的，
// 中途断开走的正是「body 已到手但永远读不完」这条路径——它不经过拨号失败分支。
func TestProviderInFlightReleasesOnClientAbort(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_1","content":[`))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithCancel(context.Background())
	spy := newInFlightSpy()
	deps := Deps{
		Dial:             newTestDial(t),
		Facts:            newTestFacts(),
		Limits:           Limits{RetryDelay: time.Millisecond},
		ProviderInFlight: spy.acquire,
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	if _, err := Forward(ctx, nil, inFlightCandidate(1, server.URL, 5), deps); err == nil {
		t.Fatal("客户端中断应失败")
	}
	if acquires, releases := spy.counts(); acquires != 1 || releases != 1 {
		t.Fatalf("登记/归还 = %d/%d，应为 1/1（客户端中断路径泄漏名额）", acquires, releases)
	}
}

// TestProviderInFlightDeniedFailsOverAsSaturated：渠道满员时不拨号、按饱和失败、换下一家。
func TestProviderInFlightDeniedFailsOverAsSaturated(t *testing.T) {
	var dialedFull int64
	full := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&dialedFull, 1)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","content":[{"type":"text","text":"不应被调用"}]}`))
	}))
	t.Cleanup(full.Close)
	next := fakeServer(t, http.StatusOK, `{"id":"msg_2","content":[{"type":"text","text":"pong"}]}`)

	spy := newInFlightSpy()
	spy.denied[1] = true

	second := newTestCandidate(2, "供应商乙", next.URL, 0)
	deps := Deps{
		Dial:             newTestDial(t),
		Facts:            newTestFacts(),
		Limits:           Limits{RetryDelay: time.Millisecond},
		ProviderInFlight: spy.acquire,
		Select: func(context.Context, []int64) (*Candidate, error) {
			return second, nil
		},
	}

	result, err := Forward(context.Background(), nil, inFlightCandidate(1, full.URL, 5), deps)
	if err != nil {
		t.Fatalf("满员后应换家成功，实际: %v", err)
	}
	if result == nil {
		t.Fatal("换家后应有结果")
	}
	// 第一家满员：它的上游**一次都不该被拨**（拨了就等于名额判定没生效）。
	if got := atomic.LoadInt64(&dialedFull); got != 0 {
		t.Fatalf("满员渠道被拨了 %d 次，应为 0（名额判定未拦住拨号）", got)
	}
	if len(result.Attempts) == 0 {
		t.Fatal("链上应有满员那条留痕")
	}
	if result.Attempts[0].Reason != ReasonConcurrentLimitFailed {
		t.Fatalf("满员条目的 reason = %q，应为 %q（词表须与 Node 同词）",
			result.Attempts[0].Reason, ReasonConcurrentLimitFailed)
	}
	if result.Attempts[0].Category != CategoryProviderSaturated {
		t.Fatalf("满员条目的分类 = %v，应为 CategoryProviderSaturated", result.Attempts[0].Category)
	}
	if !CategoryProviderSaturated.SwitchesProvider() || CategoryProviderSaturated.RetriesSameProvider() {
		t.Fatal("饱和分类必须「不重试同家、可换家」（否则满员会把请求钉死在一家）")
	}
}

// TestNilProviderInFlightMakesNoCalls：未接线（或统计关闭）时转发层一次都不调。
func TestNilProviderInFlightMakesNoCalls(t *testing.T) {
	server := fakeServer(t, http.StatusOK, `{"id":"msg_1","content":[{"type":"text","text":"pong"}]}`)
	deps := Deps{
		Dial:   newTestDial(t),
		Facts:  newTestFacts(),
		Limits: Limits{RetryDelay: time.Millisecond},
	}
	if _, err := Forward(context.Background(), nil, inFlightCandidate(1, server.URL, 5), deps); err != nil {
		t.Fatalf("转发应成功: %v", err)
	}
}
