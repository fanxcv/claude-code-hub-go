package forward

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// 本文件钉住 forwardLoop 的两条记账纪律（reviewer 第三轮的 F1' 与 F2'）。
//
// F1'：零 attempt 的退出一律给 **nil Result**。调用方只在 result == nil 分支把「候选不可服务」
// 翻成方言化 400 并结算；非 nil 会被译成通用 502，且因 Attempts 为空跳过本包的结算 defer——
// 请求既错译又在账上消失。故障转移选路失败那条分支曾正是如此。
//
// F2'：`attempted` 按**候选**计，不按重试计。自增原先写在重试循环里，首家重试两次就吃光
// MaxProviderSwitches，排在后面的供应商永远轮不到；TotalProvidersAttempted 也会重复写同一家。

// TestForwardRetryDoesNotConsumeProviderSwitchBudget 是 F2' 的正例：
// 额度刻意设成 2，首家允许重试 2 次（内层共两次尝试），第二家必须仍能被选中作答。
//
// 把重试计进额度时，首家两轮就把额度耗光，扫描在原地停手并报「供应商耗尽」。
func TestForwardRetryDoesNotConsumeProviderSwitchBudget(t *testing.T) {
	failing := fakeServer(t, http.StatusInternalServerError, `{"error":{"message":"boom"}}`)
	succeeding := fakeServer(t, http.StatusOK, `{"id":"msg_1","content":[{"type":"text","text":"pong"}]}`)

	first := newTestCandidate(1, "供应商甲", failing.URL, 2)
	second := newTestCandidate(2, "供应商乙", succeeding.URL, 1)

	selectCalls := 0
	deps := Deps{
		Dial:  newTestDial(t),
		Facts: newTestFacts(),
		// 额度 2：若重试也吃额度，甲的两轮就把额度用尽，乙永远轮不到。
		Limits: Limits{RetryDelay: time.Millisecond, MaxProviderSwitches: 2},
		Select: func(_ context.Context, _ []int64) (*Candidate, error) {
			selectCalls++
			if selectCalls > 1 {
				return nil, nil
			}
			return second, nil
		},
	}

	result, err := Forward(context.Background(), nil, first, deps)
	if err != nil {
		t.Fatalf("重试不该吃供应商切换额度：乙本可作答，实际失败：%v", err)
	}
	if result.Provider.ID != 2 {
		t.Fatalf("应由供应商乙作答，实际 provider#%d", result.Provider.ID)
	}
	// 留痕按**尝试**记：甲重试两次 + 乙一次。
	if len(result.Attempts) != 3 {
		t.Fatalf("尝试留痕 = %d 条，期望 3（甲重试 2 次 + 乙 1 次）：%+v", len(result.Attempts), result.Attempts)
	}
	// 而「尝试过的供应商」按**供应商**记：重试不得重复写同一家（否则链上会出现 [甲, 甲]）。
	want := []int64{1, 2}
	if len(result.TotalProvidersAttempted) != len(want) {
		t.Fatalf("尝试过的供应商 = %v，期望 %v（每供应商一次）", result.TotalProvidersAttempted, want)
	}
	for i, id := range want {
		if result.TotalProvidersAttempted[i] != id {
			t.Fatalf("尝试过的供应商 = %v，期望 %v", result.TotalProvidersAttempted, want)
		}
	}
}

// TestForwardFailoverSelectErrorOnZeroAttemptReturnsNilResult 钉住 F1' 的前半：
// 首个候选被状态型字段跳过、随后的故障转移选路报错时，不能返回「非 nil 且零 Attempts」的结果。
func TestForwardFailoverSelectErrorOnZeroAttemptReturnsNilResult(t *testing.T) {
	var hits int32
	unservable := countingServer(t, &hits)

	initial := statefulCandidate(1, "跨线供应商", convert.ProviderOpenAICompatible, unservable.URL)
	sentinel := errors.New("route: 选路链故障")
	deps := Deps{
		Dial:   newTestDial(t),
		Facts:  PlanFacts{Client: responsesClient(statefulResponsesBody)},
		Limits: Limits{RetryDelay: time.Millisecond},
		Select: func(context.Context, []int64) (*Candidate, error) { return nil, sentinel },
	}

	result, err := Forward(context.Background(), nil, initial, deps)
	if !errors.Is(err, sentinel) {
		t.Fatalf("错误应按原样交回，实际：%v", err)
	}
	if result != nil {
		t.Fatalf("零 attempt 的退出必须给 nil Result（调用方据此翻译错误形状并结算），实际 %+v", result)
	}
	if hits := atomic.LoadInt32(&hits); hits != 0 {
		t.Fatalf("不可服务的候选被拨号 %d 次", hits)
	}
}

// TestForwardFailoverSelectErrorAfterRealAttemptKeepsResult 钉住 F1' 的反面：
// 已经有了真实尝试时，同一条退出路径必须把 Result 交回——否则那次尝试既不留痕也不结算。
// 两条合起来即「判据只有一条：零 attempt 归 nil，否则交回」。
func TestForwardFailoverSelectErrorAfterRealAttemptKeepsResult(t *testing.T) {
	failing := fakeServer(t, http.StatusInternalServerError, `{"error":{"message":"boom"}}`)
	first := newTestCandidate(1, "供应商甲", failing.URL, 1)

	sentinel := errors.New("route: 选路链故障")
	settlements := 0
	deps := Deps{
		Dial:   newTestDial(t),
		Facts:  newTestFacts(),
		Limits: Limits{RetryDelay: time.Millisecond},
		Select: func(context.Context, []int64) (*Candidate, error) { return nil, sentinel },
		Settle: func(context.Context, *pctx.Context, *Result, *Failure) error {
			settlements++
			return nil
		},
	}

	result, err := Forward(context.Background(), nil, first, deps)
	if !errors.Is(err, sentinel) {
		t.Fatalf("错误应按原样交回，实际：%v", err)
	}
	if result == nil {
		t.Fatal("已有真实尝试时必须交回 Result（零 attempt 才归 nil）")
	}
	if len(result.Attempts) != 1 {
		t.Fatalf("尝试留痕 = %d 条，期望 1：%+v", len(result.Attempts), result.Attempts)
	}
	if settlements != 1 {
		t.Fatalf("真实尝试必须恰好结算一次，实际 %d 次", settlements)
	}
}
