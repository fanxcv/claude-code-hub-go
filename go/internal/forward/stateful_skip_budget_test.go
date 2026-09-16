package forward

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// 本文件钉住「候选跳过」不吃尝试额度、不留假账、且有界收尾。
//
// 起因（reviewer F2）：尝试额度原先按**候选迭代次数**计，而跳过也要迭代一次，于是
// MaxProviderSwitches=20 时同线候选排在第 21 位就永远轮不到它——池子里明明有能承载的
// 供应商，客户端却拿到 400，可用性被白扔。归零口径见 attempt.go 的 attempted/seen 注释。

// pickOutside 按排除列表挑下一个候选；全部被排除时返回 nil（与生产选路同契约）。
func pickOutside(pool []*Candidate, excludeIDs []int64) *Candidate {
	for _, candidate := range pool {
		excluded := false
		for _, id := range excludeIDs {
			if id == candidate.Provider.ID {
				excluded = true
				break
			}
		}
		if !excluded {
			return candidate
		}
	}
	return nil
}

// TestForwardSwitchBudgetIsNotSpentBySkippedCandidates 是 F2 的正例：
// 额度刻意设成 2，池子里却有 5 个必须跨线的候选排在能承载的原生候选之前。
//
// 把跳过算进额度时，扫描在第 2 个候选后就停手并报 400；修好后 5 次跳过都不占额度，
// 原生候选照常被选中作答。顺带钉住「跳过的候选不进 TotalProvidersAttempted」——
// 它从未拨号，把它记成「尝试过的供应商」是假账（reviewer F4）。
func TestForwardSwitchBudgetIsNotSpentBySkippedCandidates(t *testing.T) {
	var unservableHits int32
	unservable := countingServer(t, &unservableHits)
	native := fakeServer(t, 200, `{"id":"resp_1","output":[{"type":"message","content":[]}]}`)

	initial := statefulCandidate(1, "跨线甲", convert.ProviderOpenAICompatible, unservable.URL)
	pool := []*Candidate{
		statefulCandidate(2, "跨线乙", convert.ProviderOpenAICompatible, unservable.URL),
		statefulCandidate(3, "跨线丙", convert.ProviderClaude, unservable.URL),
		statefulCandidate(4, "跨线丁", convert.ProviderOpenAICompatible, unservable.URL),
		statefulCandidate(5, "跨线戊", convert.ProviderClaude, unservable.URL),
		statefulCandidate(6, "原生 Responses 供应商", convert.ProviderCodex, native.URL),
	}

	selectCalls := 0
	deps := Deps{
		Dial:  newTestDial(t),
		Facts: PlanFacts{Client: responsesClient(statefulResponsesBody)},
		// 额度 2：跳过若吃额度，第 3 个候选之后就没有额度了。
		Limits: Limits{RetryDelay: time.Millisecond, MaxProviderSwitches: 2},
		Select: func(_ context.Context, excludeIDs []int64) (*Candidate, error) {
			selectCalls++
			return pickOutside(pool, excludeIDs), nil
		},
	}

	result, err := Forward(context.Background(), nil, initial, deps)
	if err != nil {
		t.Fatalf("跳过不该吃尝试额度：池中第 6 位就是能承载的原生候选，实际失败：%v", err)
	}
	if result.Provider.ID != 6 {
		t.Fatalf("应由原生候选（id=6）作答，实际 provider#%d", result.Provider.ID)
	}
	if selectCalls != 5 {
		t.Fatalf("换候选次数 = %d，期望 5（5 个跨线候选各跳过一次）", selectCalls)
	}
	if hits := atomic.LoadInt32(&unservableHits); hits != 0 {
		t.Fatalf("不可服务的候选被拨号 %d 次", hits)
	}
	if len(result.TotalProvidersAttempted) != 1 || result.TotalProvidersAttempted[0] != 6 {
		t.Fatalf("尝试留痕只该有真正拨号的那个候选，实际 %v", result.TotalProvidersAttempted)
	}
	if !strings.Contains(string(result.Body), "resp_1") {
		t.Fatalf("正文 = %s", result.Body)
	}
}

// TestForwardStopsWhenSelectIgnoresExclusions 钉住跳过的**上界**：跳过早先靠尝试额度兜底，
// 现在额度归尝试专用，终止性由「排除集单调收缩 + seen 守卫」保证。
//
// 选路若破坏契约（反复返回同一个已被排除的候选），循环必须收敛成「候选耗尽」而不是把
// 转发协程钉死——本用例因此用超时断言：不收敛即失败，而不是让整个测试进程挂住。
func TestForwardStopsWhenSelectIgnoresExclusions(t *testing.T) {
	stubborn := statefulCandidate(1, "跨线甲", convert.ProviderOpenAICompatible, "http://127.0.0.1:1")
	initial := statefulCandidate(2, "跨线乙", convert.ProviderOpenAICompatible, "http://127.0.0.1:1")

	deps := Deps{
		Dial:   newTestDial(t),
		Facts:  PlanFacts{Client: responsesClient(statefulResponsesBody)},
		Limits: Limits{RetryDelay: time.Millisecond},
		// 无视排除列表：永远返回同一个候选。
		Select: func(context.Context, []int64) (*Candidate, error) { return stubborn, nil },
	}

	type outcome struct {
		result *Result
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := Forward(context.Background(), nil, initial, deps)
		done <- outcome{result, err}
	}()

	select {
	case out := <-done:
		var rejected *StatefulConversionError
		if !errors.As(out.err, &rejected) {
			t.Fatalf("不守排除列表时应收敛成状态型拒绝（候选耗尽），实际：%v", out.err)
		}
		if out.result != nil {
			t.Fatalf("没有一次尝试开始时不该有结果：%+v", out.result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("选路不守排除列表时转发未收敛（跳过的终止性守卫失效）")
	}
}
