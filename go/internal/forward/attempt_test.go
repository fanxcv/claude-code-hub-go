package forward

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// newTestDial 构造一个直连本地 httptest 的拨号器。
func newTestDial(t *testing.T) *dial.Client {
	t.Helper()
	client, err := dial.New(dial.Options{Transport: http.DefaultTransport})
	if err != nil {
		t.Fatalf("构造拨号器失败: %v", err)
	}
	return client
}

// newTestFacts 构造一次最小可用的会话事实（claude 原生路径）。
func newTestFacts() PlanFacts {
	return PlanFacts{Client: newClaudeRequest(claudeRequestBody)}
}

// newTestCandidate 构造一个端点指向给定地址的候选。
func newTestCandidate(id int64, name, url string, maxRetry int) *Candidate {
	return &Candidate{
		Provider: Provider{
			ID:               id,
			Name:             name,
			Type:             convert.ProviderClaude,
			Key:              "sk-test",
			URL:              url,
			MaxRetryAttempts: &maxRetry,
		},
	}
}

// ssmlServer 是返回固定状态与正文的假上游。
func fakeServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

// TestForwardSucceedsAfterFirstProviderFails 断言首个供应商失败后切换到第二个并成功，留痕完整。
func TestForwardSucceedsAfterFirstProviderFails(t *testing.T) {
	failing := fakeServer(t, http.StatusInternalServerError, `{"error":{"message":"boom"}}`)
	succeeding := fakeServer(t, http.StatusOK, `{"id":"msg_1","content":[{"type":"text","text":"pong"}]}`)

	second := newTestCandidate(2, "供应商乙", succeeding.URL, 1)
	deps := Deps{
		Dial:   newTestDial(t),
		Facts:  newTestFacts(),
		Limits: Limits{RetryDelay: time.Millisecond},
		Select: func(ctx context.Context, excludeIDs []int64) (*Candidate, error) {
			if len(excludeIDs) == 0 {
				t.Fatal("切换时未带上已失败供应商")
			}
			if excludeIDs[0] != 1 {
				t.Fatalf("排除列表 = %v", excludeIDs)
			}
			return second, nil
		},
	}

	result, err := Forward(context.Background(), nil, newTestCandidate(1, "供应商甲", failing.URL, 1), deps)
	if err != nil {
		t.Fatalf("Forward 失败: %v", err)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d", result.StatusCode)
	}
	if result.Provider.ID != 2 {
		t.Fatalf("最终供应商 = %d", result.Provider.ID)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("尝试留痕 = %d 条", len(result.Attempts))
	}
	if result.Attempts[0].Category != CategoryProviderError || result.Attempts[0].Reason != "retry_failed" {
		t.Fatalf("首条留痕 = %+v", result.Attempts[0])
	}
	if result.Attempts[0].StatusCode != http.StatusInternalServerError {
		t.Fatalf("首条状态码 = %d", result.Attempts[0].StatusCode)
	}
	if result.Attempts[1].Reason != ReasonRequestSuccess {
		t.Fatalf("成功留痕 = %+v", result.Attempts[1])
	}
	if len(result.TotalProvidersAttempted) != 2 {
		t.Fatalf("已尝试供应商 = %v", result.TotalProvidersAttempted)
	}
	if !strings.Contains(string(result.Body), "pong") {
		t.Fatalf("正文 = %s", result.Body)
	}
}

// TestForwardReportsProviderErrorRetriesThenSwitches 断言 5xx 先重试当前供应商，再切换。
func TestForwardReportsProviderErrorRetriesThenSwitches(t *testing.T) {
	var firstHits int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&firstHits, 1)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream down"}}`))
	}))
	t.Cleanup(first.Close)
	second := fakeServer(t, http.StatusOK, `{"ok":true}`)

	deps := Deps{
		Dial:   newTestDial(t),
		Facts:  newTestFacts(),
		Limits: Limits{RetryDelay: time.Millisecond},
		Select: func(context.Context, []int64) (*Candidate, error) {
			return newTestCandidate(2, "供应商乙", second.URL, 1), nil
		},
	}

	result, err := Forward(context.Background(), nil, newTestCandidate(1, "供应商甲", first.URL, 2), deps)
	if err != nil {
		t.Fatalf("Forward 失败: %v", err)
	}
	if hits := atomic.LoadInt32(&firstHits); hits != 2 {
		t.Fatalf("首个供应商被调用 %d 次，期望 2（重试上限 2）", hits)
	}
	if result.Provider.ID != 2 {
		t.Fatalf("最终供应商 = %d", result.Provider.ID)
	}
	if len(result.Attempts) != 3 {
		t.Fatalf("尝试留痕 = %d 条", len(result.Attempts))
	}
}

// TestForwardRetriesClientErrorStatusesToo 断言 4xx 归供应商故障，同样重试当前供应商。
//
// 这条与「上游 4xx 不重试」的常见直觉相反，但它是 Node 的实际行为：4xx 属 PROVIDER_ERROR，
// 重试耗尽后才切换；只有错误规则命中的客户端输入错误才不重试。
func TestForwardRetriesClientErrorStatusesToo(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid request"}}`))
	}))
	t.Cleanup(server.Close)

	deps := Deps{
		Dial:   newTestDial(t),
		Facts:  newTestFacts(),
		Limits: Limits{RetryDelay: time.Millisecond},
	}

	result, err := Forward(context.Background(), nil, newTestCandidate(1, "供应商甲", server.URL, 3), deps)
	if err == nil {
		t.Fatal("全部尝试失败时应返回错误")
	}
	if hits := atomic.LoadInt32(&hits); hits != 3 {
		t.Fatalf("被调用 %d 次，期望 3", hits)
	}
	var failure *Failure
	if !errors.As(err, &failure) {
		t.Fatalf("错误应为 *Failure: %v", err)
	}
	if failure.Category != CategoryProviderError {
		t.Fatalf("分类 = %v", failure.Category)
	}
	if result.Attempts[0].Message != "invalid request" {
		t.Fatalf("错误文案未从正文提取: %q", result.Attempts[0].Message)
	}
}

// TestForwardNonRetryableClientErrorStopsImmediately 断言规则命中的客户端错误不重试、不切换。
func TestForwardNonRetryableClientErrorStopsImmediately(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"prompt is too long"}}`))
	}))
	t.Cleanup(server.Close)

	switched := false
	deps := Deps{
		Dial:   newTestDial(t),
		Facts:  newTestFacts(),
		Limits: Limits{RetryDelay: time.Millisecond},
		Rules:  stubRules{match: true},
		Select: func(context.Context, []int64) (*Candidate, error) {
			switched = true
			return nil, nil
		},
	}

	result, err := Forward(context.Background(), nil, newTestCandidate(1, "供应商甲", server.URL, 3), deps)
	if err == nil {
		t.Fatal("应返回失败")
	}
	if hits := atomic.LoadInt32(&hits); hits != 1 {
		t.Fatalf("被调用 %d 次，期望 1", hits)
	}
	if switched {
		t.Fatal("客户端输入错误不得切换供应商")
	}
	var failure *Failure
	if !errors.As(err, &failure) || failure.Category != CategoryNonRetryableClientError {
		t.Fatalf("分类错误: %v", err)
	}
	if result.Attempts[0].Reason != ReasonClientErrorNonRetryable {
		t.Fatalf("留痕原因 = %q", result.Attempts[0].Reason)
	}
}

// TestForwardNetworkErrorAdvancesEndpoint 断言网络错误推进端点索引。
func TestForwardNetworkErrorAdvancesEndpoint(t *testing.T) {
	// 第一个端点指向已关闭端口，第二个端点可用。
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	alive := fakeServer(t, http.StatusOK, `{"ok":true}`)

	candidate := &Candidate{
		Provider: Provider{
			ID:               1,
			Name:             "供应商甲",
			Type:             convert.ProviderClaude,
			Key:              "sk",
			MaxRetryAttempts: intPtr(2),
		},
		Endpoints: []Endpoint{
			{ID: 10, URL: deadURL},
			{ID: 11, URL: alive.URL},
		},
	}

	deps := Deps{
		Dial:   newTestDial(t),
		Facts:  newTestFacts(),
		Limits: Limits{RetryDelay: time.Millisecond},
	}

	result, err := Forward(context.Background(), nil, candidate, deps)
	if err != nil {
		t.Fatalf("Forward 失败: %v", err)
	}
	if result.Endpoint.ID != 11 {
		t.Fatalf("最终端点 = %d，期望推进到第二个", result.Endpoint.ID)
	}
	if result.Attempts[0].Category != CategorySystemError {
		t.Fatalf("首条分类 = %v", result.Attempts[0].Category)
	}
	if result.Attempts[0].Reason != "system_error" {
		t.Fatalf("首条原因 = %q", result.Attempts[0].Reason)
	}
	if result.Attempts[1].EndpointID != 11 {
		t.Fatalf("第二条尝试端点 = %d", result.Attempts[1].EndpointID)
	}
}

// TestForwardRawPassthroughSkipsRetryAndSwitch 断言原始透传端点失败即返回。
func TestForwardRawPassthroughSkipsRetryAndSwitch(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	candidate := newTestCandidate(1, "供应商甲", server.URL, 5)
	candidate.RawPassthrough = true

	deps := Deps{
		Dial:   newTestDial(t),
		Facts:  newTestFacts(),
		Limits: Limits{RetryDelay: time.Millisecond},
		Select: func(context.Context, []int64) (*Candidate, error) {
			t.Fatal("原始透传不得切换供应商")
			return nil, nil
		},
	}

	result, err := Forward(context.Background(), nil, candidate, deps)
	if err == nil {
		t.Fatal("应返回失败")
	}
	if hits := atomic.LoadInt32(&hits); hits != 1 {
		t.Fatalf("被调用 %d 次，期望 1", hits)
	}
	if !result.Attempts[0].SkippedRetryAndSwitch {
		t.Fatal("留痕应标记跳过重试与切换")
	}
}

// TestForwardRawPassthroughCrossProviderFallbackRetries 断言原始透传在允许跨供应商回退时仍重试。
func TestForwardRawPassthroughCrossProviderFallbackRetries(t *testing.T) {
	var hits int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)

	candidate := newTestCandidate(1, "供应商甲", server.URL, 2)
	candidate.RawPassthrough = true
	candidate.RawCrossProviderFallback = true

	deps := Deps{
		Dial:   newTestDial(t),
		Facts:  newTestFacts(),
		Limits: Limits{RetryDelay: time.Millisecond},
	}

	if _, err := Forward(context.Background(), nil, candidate, deps); err == nil {
		t.Fatal("应返回失败")
	}
	if hits := atomic.LoadInt32(&hits); hits != 2 {
		t.Fatalf("被调用 %d 次，期望 2", hits)
	}
}

// TestForwardClientAbortStopsImmediately 断言客户端中断不重试、不切换。
func TestForwardClientAbortStopsImmediately(t *testing.T) {
	server := fakeServer(t, http.StatusOK, `{"ok":true}`)
	candidate := newTestCandidate(1, "供应商甲", server.URL, 3)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	deps := Deps{
		Dial:   newTestDial(t),
		Facts:  newTestFacts(),
		Limits: Limits{RetryDelay: time.Millisecond},
		Select: func(context.Context, []int64) (*Candidate, error) {
			t.Fatal("客户端中断不得切换供应商")
			return nil, nil
		},
	}

	_, err := Forward(ctx, nil, candidate, deps)
	if err == nil {
		t.Fatal("应返回失败")
	}
	var failure *Failure
	if !errors.As(err, &failure) || failure.Category != CategoryClientAbort {
		t.Fatalf("分类 = %v", err)
	}
}

// TestForwardRejectsOversizedResponse 断言超大正文被拒绝，且不当作成功返回。
func TestForwardRejectsOversizedResponse(t *testing.T) {
	payload := strings.Repeat("a", 4096)
	server := fakeServer(t, http.StatusOK, `{"data":"`+payload+`"}`)

	deps := Deps{
		Dial:   newTestDial(t),
		Facts:  newTestFacts(),
		Limits: Limits{MaxResponseBytes: 512, RetryDelay: time.Millisecond},
	}

	result, err := Forward(context.Background(), nil, newTestCandidate(1, "供应商甲", server.URL, 1), deps)
	if err == nil {
		t.Fatal("超限正文不得当作成功返回")
	}
	var failure *Failure
	if !errors.As(err, &failure) || failure.Category != CategoryProviderError {
		t.Fatalf("分类 = %v", err)
	}
	if !strings.Contains(result.Attempts[0].Message, "512") {
		t.Fatalf("留痕应写明上限: %q", result.Attempts[0].Message)
	}
	if result.Body != nil {
		t.Fatalf("失败时不得产出正文: %d 字节", len(result.Body))
	}
}

// TestForwardEmptyBodyIsProviderError 断言零长度正文按空响应错误处理并触发切换。
func TestForwardEmptyBodyIsProviderError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-length", "0")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	deps := Deps{
		Dial:   newTestDial(t),
		Facts:  newTestFacts(),
		Limits: Limits{RetryDelay: time.Millisecond},
	}

	result, err := Forward(context.Background(), nil, newTestCandidate(1, "供应商甲", server.URL, 1), deps)
	if err == nil {
		t.Fatal("空响应应判为供应商故障")
	}
	var failure *Failure
	if !errors.As(err, &failure) || failure.Category != CategoryProviderError {
		t.Fatalf("分类 = %v", err)
	}
	if !strings.Contains(strings.ToLower(result.Attempts[0].Message), "empty") {
		t.Fatalf("留痕文案 = %q", result.Attempts[0].Message)
	}
}

// TestForwardTimeoutBecomes524 断言非流式总超时合成 524 并计入供应商故障。
func TestForwardTimeoutBecomes524(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(server.Close)

	candidate := newTestCandidate(1, "供应商甲", server.URL, 1)
	candidate.Provider.RequestTimeoutNonStreamingMS = 20

	deps := Deps{
		Dial:   newTestDial(t),
		Facts:  newTestFacts(),
		Limits: Limits{RetryDelay: time.Millisecond},
	}

	result, err := Forward(context.Background(), nil, candidate, deps)
	if err == nil {
		t.Fatal("超时应失败")
	}
	if result.Attempts[0].StatusCode != statusUpstreamTimeout {
		t.Fatalf("状态码 = %d，期望 524", result.Attempts[0].StatusCode)
	}
	if result.Attempts[0].Reason != "vendor_type_all_timeout" {
		t.Fatalf("原因 = %q", result.Attempts[0].Reason)
	}
	var failure *Failure
	if !errors.As(err, &failure) || !strings.Contains(failure.Body, "timeout_error") {
		t.Fatalf("超时正文缺失: %v", failure)
	}
}

// TestForwardCircuitRecording 断言只有计入熔断的分类才调用 RecordFailure。
func TestForwardCircuitRecording(t *testing.T) {
	t.Run("供应商错误计入熔断", func(t *testing.T) {
		server := fakeServer(t, http.StatusInternalServerError, `{"error":{"message":"boom"}}`)
		var recorded []int64
		deps := Deps{
			Dial:          newTestDial(t),
			Facts:         newTestFacts(),
			Limits:        Limits{RetryDelay: time.Millisecond},
			RecordFailure: func(ctx context.Context, failure *Failure) { recorded = append(recorded, failure.ProviderID) },
		}
		if _, err := Forward(context.Background(), nil, newTestCandidate(7, "供应商甲", server.URL, 2), deps); err == nil {
			t.Fatal("应失败")
		}
		if len(recorded) != 1 || recorded[0] != 7 {
			t.Fatalf("熔断记录 = %v", recorded)
		}
	})

	t.Run("网络错误默认不计入熔断", func(t *testing.T) {
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		deadURL := dead.URL
		dead.Close()
		var recorded int
		deps := Deps{
			Dial:          newTestDial(t),
			Facts:         newTestFacts(),
			Limits:        Limits{RetryDelay: time.Millisecond},
			RecordFailure: func(ctx context.Context, failure *Failure) { recorded++ },
		}
		if _, err := Forward(context.Background(), nil, newTestCandidate(7, "供应商甲", deadURL, 2), deps); err == nil {
			t.Fatal("应失败")
		}
		if recorded != 0 {
			t.Fatalf("网络错误不应计入熔断，实际 %d 次", recorded)
		}
	})
}

// TestForwardSettlesOnceWithContext 断言最终成功只在 pctx 上结算一次，且供应商槽位被写入。
func TestForwardSettlesOnceWithContext(t *testing.T) {
	server := fakeServer(t, http.StatusOK, `{"ok":true}`)

	pc, err := pctx.New(pctx.Init{
		Method:  http.MethodPost,
		Path:    "/v1/messages",
		Headers: newClientHeaders("content-type", "application/json"),
		Body:    http.NoBody,
	})
	if err != nil {
		t.Fatalf("构造 pctx 失败: %v", err)
	}

	deps := Deps{
		Dial:   newTestDial(t),
		Facts:  newTestFacts(),
		Limits: Limits{RetryDelay: time.Millisecond},
	}
	result, err := Forward(context.Background(), pc, newTestCandidate(9, "供应商甲", server.URL, 1), deps)
	if err != nil {
		t.Fatalf("Forward 失败: %v", err)
	}
	if !result.Settled {
		t.Fatal("成功路径应完成一次性结算断言")
	}
	settlement, ok := pc.Settlement()
	if !ok || !settlement.Success || settlement.StatusCode != http.StatusOK {
		t.Fatalf("结算槽位 = %+v", settlement)
	}
	selection, ok := pc.Provider()
	if !ok || selection.ProviderID != 9 {
		t.Fatalf("供应商槽位 = %+v", selection)
	}
}

// TestForwardRequiresDialer 断言缺少拨号器时立刻失败，不产生半成品请求。
func TestForwardRequiresDialer(t *testing.T) {
	if _, err := Forward(context.Background(), nil, newTestCandidate(1, "甲", "http://127.0.0.1:1", 1), Deps{}); err == nil {
		t.Fatal("缺少拨号器应报错")
	}
}

// TestForwardWithoutCandidateSelects 断言初始候选为空时通过 Select 选取。
func TestForwardWithoutCandidateSelects(t *testing.T) {
	server := fakeServer(t, http.StatusOK, `{"ok":true}`)
	deps := Deps{
		Dial:  newTestDial(t),
		Facts: newTestFacts(),
		Select: func(context.Context, []int64) (*Candidate, error) {
			return newTestCandidate(5, "供应商乙", server.URL, 1), nil
		},
	}
	result, err := Forward(context.Background(), nil, nil, deps)
	if err != nil {
		t.Fatalf("Forward 失败: %v", err)
	}
	if result.Provider.ID != 5 {
		t.Fatalf("供应商 = %d", result.Provider.ID)
	}
}

// TestForwardNoProviderAvailable 断言无候选可用时报确定错误。
func TestForwardNoProviderAvailable(t *testing.T) {
	deps := Deps{Dial: newTestDial(t), Facts: newTestFacts()}
	if _, err := Forward(context.Background(), nil, nil, deps); !errors.Is(err, ErrNoProviderAvailable) {
		t.Fatalf("err = %v", err)
	}
}

// TestForwardExhaustedAfterProviderSwitches 断言切换上限生效且耗尽后返回 ErrProvidersExhausted。
func TestForwardExhaustedAfterProviderSwitches(t *testing.T) {
	server := fakeServer(t, http.StatusInternalServerError, `{"error":{"message":"boom"}}`)
	deps := Deps{
		Dial:   newTestDial(t),
		Facts:  newTestFacts(),
		Limits: Limits{RetryDelay: time.Millisecond, MaxProviderSwitches: 3},
		Select: func(ctx context.Context, excludeIDs []int64) (*Candidate, error) {
			return newTestCandidate(int64(len(excludeIDs)+10), "供应商候选", server.URL, 1), nil
		},
	}
	result, err := Forward(context.Background(), nil, newTestCandidate(1, "供应商甲", server.URL, 1), deps)
	if !errors.Is(err, ErrProvidersExhausted) {
		t.Fatalf("err = %v，期望 ErrProvidersExhausted", err)
	}
	if len(result.TotalProvidersAttempted) != 3 {
		t.Fatalf("已尝试供应商数 = %d，期望 3（上限）", len(result.TotalProvidersAttempted))
	}
}

// TestForwardUsesPlannerOverrides 断言会话级覆写钩子被传入计划构造。
func TestForwardUsesPlannerOverrides(t *testing.T) {
	var seenProtocol convert.WireProtocol
	server := fakeServer(t, http.StatusOK, `{"ok":true}`)

	facts := newTestFacts()
	facts.Overrides = overrideFunc(func(provider Provider, protocol convert.WireProtocol, body []byte) ([]byte, error) {
		seenProtocol = protocol
		return body, nil
	})

	deps := Deps{Dial: newTestDial(t), Facts: facts, Limits: Limits{RetryDelay: time.Millisecond}}
	if _, err := Forward(context.Background(), nil, newTestCandidate(1, "供应商甲", server.URL, 1), deps); err != nil {
		t.Fatalf("Forward 失败: %v", err)
	}
	if seenProtocol != convert.ProtocolAnthropicMessages {
		t.Fatalf("覆写协议线 = %q", seenProtocol)
	}
}

// overrideFunc 是函数形态的 OverrideApplier。
type overrideFunc func(provider Provider, protocol convert.WireProtocol, body []byte) ([]byte, error)

func (f overrideFunc) Apply(provider Provider, protocol convert.WireProtocol, body []byte) ([]byte, error) {
	return f(provider, protocol, body)
}

func intPtr(value int) *int { return &value }
