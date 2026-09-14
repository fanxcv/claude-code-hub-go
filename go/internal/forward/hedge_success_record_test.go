package forward

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestHedgeWinnerRecordsProviderSuccess 钉住「竞速胜者必须记一次供应商成功」。
//
// 为什么值得单测：竞速路径原先**只记失败**（`hedge.go` 只有 RecordFailure 调用），
// 而选路侧把「open 且窗口已过期」视为放行试探、由成功推进 half-open 计数。
// 一旦成功不记账，half-open 计数永远停在 0 → 永远达不到归 closed 的阈值
// → 生产现象是「一批供应商永远显示熔断恢复中，而它们明明在正常处理请求」。
// 串行路径（attempt.go）本来就记，所以这个缺口只在竞速流量上暴露。
func TestHedgeWinnerRecordsProviderSuccess(t *testing.T) {
	blocked := make(chan struct{})
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		<-blocked
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(func() {
		close(blocked)
		first.Close()
	})
	second := streamServer(t, "text/event-stream", claudeStreamChunks(), true)

	harness := newHedgeHarness(t)
	initial := sseCandidate(1, "供应商甲", first.URL, 1)
	harness.setSelect([]*Candidate{sseCandidate(2, "供应商乙", second.URL, 0)})

	var mu sync.Mutex
	type recorded struct {
		providerID int64
		endpointID int64
	}
	var successes []recorded
	harness.deps.RecordSuccess = func(_ context.Context, providerID int64, endpointID int64) {
		mu.Lock()
		defer mu.Unlock()
		successes = append(successes, recorded{providerID: providerID, endpointID: endpointID})
	}

	result, err := runHedgeWithThreshold(t, harness, initial)
	if err != nil {
		t.Fatalf("ForwardStreamHedge 失败: %v", err)
	}
	if result.Provider.ID != 2 {
		t.Fatalf("胜者供应商 = %d，期望 2", result.Provider.ID)
	}
	if result.Stream != nil {
		_, _ = consumeStream(t, result.Stream)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(successes) != 1 {
		t.Fatalf("竞速胜者应记**恰好一次**成功，实际 %d 次：%+v", len(successes), successes)
	}
	if successes[0].providerID != 2 {
		t.Fatalf("记成功的供应商 = %d，期望胜者 2（不得记成首发/输家）", successes[0].providerID)
	}
	if successes[0].endpointID != result.Endpoint.ID {
		t.Fatalf("记成功的端点 = %d，期望胜者端点 %d", successes[0].endpointID, result.Endpoint.ID)
	}
}

// TestHedgeLosersDoNotRecordSuccess 钉住「输家不记成功」。
//
// 输家是被主动取消/引流计费的，不是上游成功；若也记成功，多路竞速会把熔断器
// 的 half-open 计数刷满、提前归 closed，等于用「取消」冒充健康。
func TestHedgeLosersDoNotRecordSuccess(t *testing.T) {
	blocked := make(chan struct{})
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		<-blocked
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(func() {
		close(blocked)
		first.Close()
	})
	second := streamServer(t, "text/event-stream", claudeStreamChunks(), true)

	harness := newHedgeHarness(t)
	initial := sseCandidate(1, "供应商甲", first.URL, 1)
	harness.setSelect([]*Candidate{sseCandidate(2, "供应商乙", second.URL, 0)})

	var mu sync.Mutex
	var successProviders []int64
	harness.deps.RecordSuccess = func(_ context.Context, providerID int64, _ int64) {
		mu.Lock()
		defer mu.Unlock()
		successProviders = append(successProviders, providerID)
	}

	result, err := runHedgeWithThreshold(t, harness, initial)
	if err != nil {
		t.Fatalf("ForwardStreamHedge 失败: %v", err)
	}
	if result.Stream != nil {
		_, _ = consumeStream(t, result.Stream)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, id := range successProviders {
		if id == 1 {
			t.Fatalf("输家（供应商 1）不得记成功，实际记录：%v", successProviders)
		}
	}
	if len(successProviders) != 1 {
		t.Fatalf("只应有胜者那一次成功记账，实际 %v", successProviders)
	}
}

// runHedgeWithThreshold 用注入的阈值计时器把竞速推到裁决，并等待结果。
func runHedgeWithThreshold(t *testing.T, harness *hedgeHarness, initial *Candidate) (*StreamResult, error) {
	t.Helper()
	type outcome struct {
		result *StreamResult
		err    error
	}
	outCh := make(chan outcome, 1)
	go func() {
		result, err := ForwardStreamHedge(context.Background(), harness.pc,
			initial, harness.deps, harness.options, harness.cfg)
		outCh <- outcome{result, err}
	}()

	timer := <-harness.timerCh
	timer.fire()
	<-harness.selectCalls

	out := <-outCh
	if out.err != nil && !strings.Contains(out.err.Error(), "context") {
		return nil, out.err
	}
	return out.result, out.err
}
