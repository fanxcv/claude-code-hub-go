package forward

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// 本文件钉住竞速**耗尽**的返回值契约（D2）：全部尝试失败时 Result 必须非 nil 且带尝试留痕，
// 且终态在 forward 内落库一次——与串行路径（forwardLoop 的 defer）同口径。
//
// 生产证据（修复前）：16 行 provider_id=167 / status_code=524 的记录，provider_chain=[]、
// routing_trace=null。根因是 maybeFinishLocked 只送 hedgeResult{err}（Result 为 nil），
// 数据面于是走 settleFailure，而那条结算造的 Result 不带 Attempts。
//
// 反证：把 maybeFinishLocked 的 exhaustedResultLocked() 换回 nil，本用例转红。

// settleCapture 记录终态结算缝的调用。
type settleCapture struct {
	results  []*Result
	failures []*Failure
}

func (c *settleCapture) settle(_ context.Context, _ *pctx.Context, result *Result, failure *Failure) error {
	c.results = append(c.results, result)
	c.failures = append(c.failures, failure)
	return nil
}

// TestHedgeExhaustedReturnsAttemptsAndSettlesOnce 竞速全失败：Result 带留痕，且只结算一次。
func TestHedgeExhaustedReturnsAttemptsAndSettlesOnce(t *testing.T) {
	failing := fakeServer(t, http.StatusInternalServerError, `{"error":{"message":"boom"}}`)

	harness := newHedgeHarness(t)
	// 无更多候选：失败后 Select 返回 nil ⇒ 竞速耗尽。
	harness.setSelect(nil)
	capture := &settleCapture{}
	harness.deps.Settle = capture.settle

	result, err := ForwardStreamHedge(context.Background(), harness.pc,
		sseCandidate(1, "供应商甲", failing.URL, 1), harness.deps, harness.options, harness.cfg)
	if err == nil {
		t.Fatal("全部尝试失败时必须返回错误")
	}
	if result == nil {
		t.Fatal("耗尽时必须返回带尝试留痕的 Result（修复前恒为 nil，终态链整段落空）")
	}
	if result.Stream != nil {
		t.Fatal("耗尽的 Result 不该带已提交的流")
	}
	if len(result.Attempts) != 1 {
		t.Fatalf("应留下 1 条尝试留痕，实际 %d 条：%+v", len(result.Attempts), result.Attempts)
	}
	if result.Attempts[0].ProviderID != 1 || result.Attempts[0].Reason == "" {
		t.Fatalf("尝试留痕应带渠道与失败原因，实际 %+v", result.Attempts[0])
	}
	if len(result.TotalProvidersAttempted) != 1 || result.TotalProvidersAttempted[0] != 1 {
		t.Fatalf("尝试过的供应商应为 [1]，实际 %v", result.TotalProvidersAttempted)
	}
	if len(capture.results) != 1 {
		t.Fatalf("终态只应结算一次，实际 %d 次", len(capture.results))
	}
	if got := capture.results[0]; got == nil || len(got.Attempts) != 1 {
		t.Fatalf("结算缝应收到带留痕的结果，实际 %+v", got)
	}
	var failure *Failure
	if !errors.As(err, &failure) || failure.StatusCode != http.StatusInternalServerError {
		t.Fatalf("错误应可 errors.As 成带 500 的 *Failure，实际 %v", err)
	}
	if capture.failures[0] == nil || capture.failures[0].StatusCode != http.StatusInternalServerError {
		t.Fatalf("结算缝应收到同一份失败归因，实际 %+v", capture.failures[0])
	}
}
