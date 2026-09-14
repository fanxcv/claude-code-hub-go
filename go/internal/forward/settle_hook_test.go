package forward

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// settleCall 是一次结算缝隙调用的留痕。
type settleCall struct {
	result  *Result
	failure *Failure
}

// settleRecorder 是结算缝隙的假实现：记录调用并可选返回错误。
type settleRecorder struct {
	calls []settleCall
	err   error
}

func (r *settleRecorder) hook(_ context.Context, _ *pctx.Context, result *Result, failure *Failure) error {
	r.calls = append(r.calls, settleCall{result: result, failure: failure})
	return r.err
}

// newSettleContext 造一个带正文的最小请求上下文。
func newSettleContext(t *testing.T) *pctx.Context {
	t.Helper()
	pc, err := pctx.New(pctx.Init{
		Method:  http.MethodPost,
		Path:    "/v1/messages",
		Headers: newClientHeaders("content-type", "application/json"),
		Body:    http.NoBody,
	})
	if err != nil {
		t.Fatalf("构造 pctx 失败: %v", err)
	}
	return pc
}

// TestForwardSettleHookRunsOnceOnSuccess 钉住终态入账的调用纪律：成功路径恰好一次，且带最终结果。
func TestForwardSettleHookRunsOnceOnSuccess(t *testing.T) {
	server := fakeServer(t, http.StatusOK, `{"ok":true}`)
	recorder := &settleRecorder{}
	deps := Deps{
		Dial:   newTestDial(t),
		Facts:  newTestFacts(),
		Limits: Limits{RetryDelay: time.Millisecond},
		Settle: recorder.hook,
	}

	result, err := Forward(context.Background(), newSettleContext(t), newTestCandidate(9, "供应商甲", server.URL, 2), deps)
	if err != nil {
		t.Fatalf("Forward 失败: %v", err)
	}
	if len(recorder.calls) != 1 {
		t.Fatalf("成功路径应恰好结算一次，得到 %d 次", len(recorder.calls))
	}
	if call := recorder.calls[0]; call.result != result || call.failure != nil {
		t.Fatalf("结算载荷不符: result=%p failure=%+v", call.result, call.failure)
	}
	if result.StatusCode != http.StatusOK {
		t.Fatalf("最终状态码 = %d", result.StatusCode)
	}
}

// TestForwardSettleHookSkipsIntermediateFailures 钉住「中间失败不落库」：重试与换供应商
// 都不产生结算调用，只有最终失败那一次才入账——否则重试会把同一条请求记多次账。
func TestForwardSettleHookSkipsIntermediateFailures(t *testing.T) {
	server := fakeServer(t, http.StatusInternalServerError, `{"error":"boom"}`)

	recorder := &settleRecorder{}
	deps := Deps{
		Dial:   newTestDial(t),
		Facts:  newTestFacts(),
		Limits: Limits{RetryDelay: time.Millisecond},
		Settle: recorder.hook,
	}
	candidate := newTestCandidate(3, "供应商乙", server.URL, 2)

	result, err := Forward(context.Background(), newSettleContext(t), candidate, deps)
	if err == nil {
		t.Fatal("上游 500 应耗尽重试并失败")
	}
	if !errors.Is(err, ErrProvidersExhausted) {
		t.Fatalf("期望 ErrProvidersExhausted，得到 %v", err)
	}
	if len(recorder.calls) != 1 {
		t.Fatalf("失败路径应只在最终结果上结算一次，得到 %d 次", len(recorder.calls))
	}
	if last := recorder.calls[0]; last.failure == nil || last.result != result {
		t.Fatalf("失败结算载荷不符: failure=%+v", last.failure)
	}
	if attempts := len(result.Attempts); attempts != 2 {
		t.Fatalf("应留痕两次尝试（重试上限 2），得到 %d 次", attempts)
	}
}

// TestForwardSettleHookSkippedWithoutAttempts 钉住「没接触过上游就不入账」：计划构造失败时
// 没有终态可言，不得凭空结算。
func TestForwardSettleHookSkippedWithoutAttempts(t *testing.T) {
	recorder := &settleRecorder{}
	deps := Deps{
		Dial:   newTestDial(t),
		Facts:  PlanFacts{},
		Settle: recorder.hook,
	}

	// 空事实 ⇒ 客户端格式未知，计划构造失败。
	if _, err := Forward(context.Background(), newSettleContext(t), newTestCandidate(1, "甲", "http://127.0.0.1:1", 1), deps); err == nil {
		t.Fatal("计划构造应失败")
	}
	if len(recorder.calls) != 0 {
		t.Fatalf("没有尝试过上游时不应结算，得到 %d 次", len(recorder.calls))
	}
}

// TestForwardSettleHookErrorIsLoggedNotFatal 钉住降级语义：入账失败只记日志，
// 不改变已经拿到的上游结果。
func TestForwardSettleHookErrorIsLoggedNotFatal(t *testing.T) {
	server := fakeServer(t, http.StatusOK, `{"ok":true}`)
	recorder := &settleRecorder{err: errors.New("账本拒绝写入")}
	deps := Deps{
		Dial:   newTestDial(t),
		Facts:  newTestFacts(),
		Limits: Limits{RetryDelay: time.Millisecond},
		Settle: recorder.hook,
	}

	result, err := Forward(context.Background(), newSettleContext(t), newTestCandidate(5, "甲", server.URL, 1), deps)
	if err != nil {
		t.Fatalf("入账失败不应把成功的转发变成失败: %v", err)
	}
	if len(result.Body) == 0 || result.StatusCode != http.StatusOK {
		t.Fatalf("上游结果被改动: status=%d body=%q", result.StatusCode, string(result.Body))
	}
	if len(recorder.calls) != 1 {
		t.Fatalf("应调用一次，得到 %d 次", len(recorder.calls))
	}
}
