package dataplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/terminal"
)

// 本文件是「故障转移后成功不再记失败」的 HTTP 层判据：断言的不是转发层的中间变量，
// 而是终态写入用的那个字段（settlement.ErrorMessage）。
//
// 生产事故（message_request.id=1340293）：首供应商 400 两次、故障转移到第二家 200，行却是
// status_code=200 + error_message='Provider returned 400: 400 Bad Request'，
// usage_ledger.is_success=false（触发器以 error_message 为空判定成功）。
// 根因在 forward：成功分支没有清空上一次尝试的 lastFailure，结算缝把它原样交了下来。

// failoverCandidates 首个候选失败后经 Failover 换到第二家（与生产 candidateSource 同形）。
type failoverCandidates struct {
	first, second *forward.Candidate
}

func (c failoverCandidates) Candidate(
	_ context.Context,
	_ pctx.ProviderSelection,
	_ SelectionFacts,
) (*forward.Candidate, route.Result, error) {
	return c.first, route.Result{}, nil
}

func (c failoverCandidates) Failover(_ context.Context, facts SelectionFacts) (*forward.Candidate, route.Result, error) {
	for _, id := range facts.ExcludeIDs {
		if id == c.second.Provider.ID {
			return nil, route.Result{}, nil
		}
	}
	return c.second, route.Result{}, nil
}

// baseSettlingRecorder 用生产实现 baseSettlement 造结算：断言的是落库字段本身，
// 而不是「失败归因为 nil」这个中间结论。
type baseSettlingRecorder struct {
	mu    sync.Mutex
	built []terminal.Settlement
}

func (r *baseSettlingRecorder) NonStream(
	_ context.Context,
	pc *pctx.Context,
	result *forward.Result,
	failure *forward.Failure,
) error {
	settlement := (&storeSettler{}).baseSettlement(pc, failure, servingProviderID(result))
	// 与生产 NonStream 同序：真实状态码随后覆盖失败归因的状态码。
	if result != nil && result.StatusCode > 0 {
		settlement.StatusCode = result.StatusCode
	}
	r.mu.Lock()
	r.built = append(r.built, settlement)
	r.mu.Unlock()
	return nil
}

func (r *baseSettlingRecorder) Stream(context.Context, *pctx.Context, forward.StreamOutcome) error {
	return nil
}

func (r *baseSettlingRecorder) settlements() []terminal.Settlement {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]terminal.Settlement(nil), r.built...)
}

// TestSettlementAfterFailoverHasNoErrorMessage 钉住成功终态的 error_message 为空。
//
// 反证：删掉 forward/attempt.go 成功分支的 `lastFailure = nil`，本条转红
// （ErrorMessage 变成首家 500 的文案，生产上即 is_success=false）。
func TestSettlementAfterFailoverHasNoErrorMessage(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"api_error","message":"boom"}}`))
	}))
	t.Cleanup(failing.Close)
	succeeding := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","content":[{"type":"text","text":"pong"}],` +
			`"usage":{"input_tokens":3,"output_tokens":5}}`))
	}))
	t.Cleanup(succeeding.Close)

	retries := 1
	candidate := func(id int64, name, url string) *forward.Candidate {
		return &forward.Candidate{Provider: forward.Provider{
			ID: id, Name: name, Type: convert.ProviderClaude, URL: url,
			Key: "fake-upstream-key", MaxRetryAttempts: &retries,
		}}
	}
	settler := &baseSettlingRecorder{}
	// 复用既有的无数据库装配（候选来源换成本文件的那一个）。
	handler := newStatefulHandlerWithSettler(t, failoverCandidates{
		first:  candidate(7, "首选", failing.URL),
		second: candidate(8, "备选", succeeding.URL),
	}, settler)

	request := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", "fake-client-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("故障转移后应成功，收到 %d：%s", recorder.Code, recorder.Body.String())
	}
	recorded := settler.settlements()
	if len(recorded) != 1 {
		t.Fatalf("应恰好结算一次，得到 %d 次", len(recorded))
	}
	if recorded[0].ErrorMessage != nil {
		t.Fatalf("成功终态带上了 error_message（生产上即 is_success=false）：%q", *recorded[0].ErrorMessage)
	}
	if recorded[0].StatusCode != http.StatusOK {
		t.Fatalf("终态状态码 = %d", recorded[0].StatusCode)
	}
}
