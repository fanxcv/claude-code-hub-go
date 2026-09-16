package forward

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// 本文件钉住竞速启动窗口的交接：窗口占用期间的启动申请不能被静默丢弃。
//
// 起因（reviewer F3）：启动窗口覆盖「选路 + 启动 attempt」两步，而 attempt 的协程可能在
// 窗口内就跑完 BuildPlan 并因状态型字段被跳过、回到 launchAlternative 申请启动下一个候选。
// 那次申请撞上 launching 被丢掉后，active 可能已归零、noMoreProviders 又是假，于是既没有
// 在途 attempt 也没有启动者——整局再没人推进，请求只能等 context 取消。

// newLaunchWindowRace 造一个只关心启动窗口的竞速实例：已有一个在途候选（id=1），
// 并发上限 1，于是被选中的新候选一律在饱和检查处被挡下，不会真的拨号。
func newLaunchWindowRace(selectFn func(context.Context, []int64) (*Candidate, error)) *hedgeRace {
	return &hedgeRace{
		ctx:         context.Background(),
		deps:        Deps{Select: selectFn, Limits: Limits{RetryDelay: time.Millisecond}},
		cfg:         HedgeOptions{MaxInFlight: 1, Now: time.Now, After: hedgeTimerAfter},
		now:         time.Now,
		launchedSet: map[int64]struct{}{1: {}},
		launched:    []int64{1},
		active:      1,
		attempts:    map[int]*hedgeAttempt{},
		resultCh:    make(chan hedgeResult, 1),
	}
}

// TestHedgeLaunchRequestDuringOpenWindowIsHandedOff 是本缺陷的直接判据：
// 有人在窗口占用期间申请启动时，申请必须被挂起、并由窗口持有者退出后补跑。
//
// 判据取两处：① 挂起标志在窗口内即为真（不丢申请）；② 窗口持有者退出后再选一次路
// （补跑真的发生）。把交接去掉，这两条同时转红。
func TestHedgeLaunchRequestDuringOpenWindowIsHandedOff(t *testing.T) {
	var (
		race        *hedgeRace
		selectCalls int
		pendingSeen bool
	)
	race = newLaunchWindowRace(func(_ context.Context, _ []int64) (*Candidate, error) {
		selectCalls++
		if selectCalls == 1 {
			// 本协程正占着窗口：模拟另一个推进者（跳过的 attempt 或首字节阈值到期）来申请启动。
			race.launchAlternative(nil)
			race.mu.Lock()
			pendingSeen = race.launchPending
			race.mu.Unlock()
			return statefulCandidate(2, "候选乙", convert.ProviderOpenAICompatible, "http://127.0.0.1:1"), nil
		}
		return nil, nil
	})

	race.launchAlternative(nil)

	if !pendingSeen {
		t.Fatal("窗口占用期间的启动申请被丢弃了：它必须挂起，交给窗口持有者补跑")
	}
	if selectCalls != 2 {
		t.Fatalf("被挂起的申请必须补跑，Select 实际调用 %d 次，期望 2", selectCalls)
	}
	if len(race.launched) != 1 {
		t.Fatalf("饱和时不该启动新候选，实际启动 %v", race.launched)
	}
	race.mu.Lock()
	defer race.mu.Unlock()
	if race.launching || race.launchPending {
		t.Fatalf("窗口与挂起标志都该已清空：launching=%v pending=%v", race.launching, race.launchPending)
	}
}

// TestHedgeSkipsConsecutiveUnservableCandidatesWithoutStalling 是 F3 的端到端回归：
// **连续两个**不可服务候选 + 一个原生候选。跳过链的每一次推进都走启动窗口，
// 正是「窗口占用 + 申请到达」容易交错的地方。
//
// 断言三件事：原生候选最终作答（不悬挂）、跨线候选零拨号、终局只出现一次（不重复终局）。
func TestHedgeSkipsConsecutiveUnservableCandidatesWithoutStalling(t *testing.T) {
	harness := newHedgeHarness(t)
	harness.deps.Facts = PlanFacts{Client: responsesClient(responsesStreamBody)}
	harness.options.Format = convert.FormatResponse

	var crossLineHits int32
	crossLine := countingServer(t, &crossLineHits)
	native := streamServer(t, "text/event-stream", responsesStreamChunks(), true)

	initial := statefulCandidate(1, "跨线甲", convert.ProviderOpenAICompatible, crossLine.URL)
	initial.Provider.FirstByteTimeoutStreamingMS = 0
	harness.setSelect([]*Candidate{
		statefulCandidate(2, "跨线乙", convert.ProviderOpenAICompatible, crossLine.URL),
		statefulCandidate(3, "原生 Responses 供应商", convert.ProviderCodex, native.URL),
	})

	type outcome struct {
		result *StreamResult
		err    error
	}
	outCh := make(chan outcome, 1)
	go func() {
		result, err := ForwardStreamHedge(
			context.Background(), harness.pc, initial, harness.deps, harness.options, harness.cfg,
		)
		outCh <- outcome{result, err}
	}()

	select {
	case out := <-outCh:
		if out.err != nil {
			t.Fatalf("池中有能承载的候选时不该失败：%v", out.err)
		}
		if out.result == nil || out.result.Stream == nil {
			t.Fatal("应拿到可读的流")
		}
		if out.result.Provider.ID != 3 {
			t.Fatalf("胜者供应商 = %d，期望 3（原生 Responses）", out.result.Provider.ID)
		}
		if hits := atomic.LoadInt32(&crossLineHits); hits != 0 {
			t.Fatalf("不可服务的候选被拨号 %d 次", hits)
		}
		winners := 0
		for _, attempt := range out.result.Attempts {
			if attempt.Reason == ReasonHedgeWinner {
				winners++
			}
			if attempt.ProviderID != 3 && attempt.Reason == ReasonHedgeWinner {
				t.Fatalf("胜者只能是原生候选：%+v", attempt)
			}
		}
		if winners != 1 {
			t.Fatalf("竞速胜者留痕应恰好一条，实际 %d 条：%+v", winners, out.result.Attempts)
		}
		received, _ := consumeStream(t, out.result.Stream)
		if !strings.Contains(string(received), "pong") {
			t.Fatalf("正文 = %q", received)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("连续两个不可服务候选后竞速悬挂：没有人推进启动（见 launchAlternative 的交接）")
	}
}
