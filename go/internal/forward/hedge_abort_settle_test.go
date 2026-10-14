package forward

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
)

// 本文件钉住竞速路径「客户端中断」的退出契约：中断分支同样只经 finish 收口，
// 把已收集的尝试结局连同 499 归因交回调用方。
//
// 生产证据（修复前）：该分支直接返回裸 &Failure（result 为 nil），数据面于是走
// settleFailure，而那条结算造的 Result 不带 Attempts——这一行的 provider_chain 与
// routing_trace 整段落空，与竞速耗尽路径修复前是同一个症状（见 hedge_exhaust_test.go）。
//
// 反证：把 hedge.go 中断分支的 exhaustedResultLocked() 换回 nil，本用例立刻转红。

// TestHedgeClientAbortKeepsAttemptOutcomes 竞速中客户端中断：Result 必须带已有留痕并结算一次。
func TestHedgeClientAbortKeepsAttemptOutcomes(t *testing.T) {
	failing := fakeServer(t, http.StatusInternalServerError, `{"error":{"message":"boom"}}`)

	harness := newHedgeHarness(t)
	// 第一家在途失败后，拉备选的 Select 停在这里：竞速既不耗尽（noMoreProviders 未置位）
	// 也不胜出，故 ctx 取消时 resultCh 必为空——中断分支一定被走到，且此时第一家已留痕
	// （runAttempt 先落结局、后拉备选，见 finishAttemptFailed 末尾的 launchAlternative）。
	selection := make(chan struct{})
	entered := make(chan struct{})
	var enteredOnce sync.Once
	harness.deps.Select = func(context.Context, []int64) (*Candidate, error) {
		enteredOnce.Do(func() { close(entered) })
		<-selection
		return nil, nil
	}
	capture := &settleCapture{}
	harness.deps.Settle = capture.settle

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-entered
		cancel()
	}()

	result, err := ForwardStreamHedge(ctx, harness.pc,
		sseCandidate(1, "供应商甲", failing.URL, 0), harness.deps, harness.options, harness.cfg)
	close(selection)

	var failure *Failure
	if !errors.As(err, &failure) || failure.Category != CategoryClientAbort {
		t.Fatalf("中断应仍以 client_abort 归因返回，实际 %v", err)
	}
	if failure.StatusCode != 499 {
		t.Fatalf("中断的状态码应保持 499，实际 %d", failure.StatusCode)
	}
	if result == nil {
		t.Fatal("中断时必须返回带尝试留痕的 Result（修复前恒为 nil，终态链整段落空）")
	}
	if result.Stream != nil {
		t.Fatal("中断的 Result 不该带已提交的流")
	}
	if len(result.Attempts) != 1 {
		t.Fatalf("已收集的尝试留痕应保留，实际 %d 条：%+v", len(result.Attempts), result.Attempts)
	}
	if got := result.Attempts[0]; got.ProviderID != 1 || got.Reason == "" || got.StatusCode != http.StatusInternalServerError {
		t.Fatalf("留痕应带渠道、失败原因与真实上游状态码，实际 %+v", got)
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
	if capture.failures[0] == nil || capture.failures[0].Category != CategoryClientAbort {
		t.Fatalf("结算缝应收到同一份中断归因，实际 %+v", capture.failures[0])
	}
}
