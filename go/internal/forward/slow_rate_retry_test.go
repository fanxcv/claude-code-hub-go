package forward

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/gate"
)

// 本文件钉住「判慢 ⇒ 不同家重试、立即换家、不计熔断」这套语义（F6）。
//
// 背景（生产实证）：判慢原先归 CategoryProviderError，而该分类的 RetriesSameProvider 为真，
// 于是判慢后先在同一家重试、重试耗尽才换家。后果是客户端白等一个完整判慢周期（生产实测
// 单条请求拖到 390s/220s/202s，且最终拿到的是 HTTP 200），而「这一家此刻慢」在下一瞬
// 大概率仍然成立，同家重试没有任何信息增益。

// TestSlowRateCategoryRetrySemantics 钉住新分类的三条属性，并钉住「其余供应商错误逐字不变」。
//
// 后者是最关键的一条：判慢与真实 4xx/5xx 都会走到 provider_error 那条老路，改分类时
// 最容易顺手把真实错误也一起改了（那会让正常的 5xx 重试消失，属静默回归）。
func TestSlowRateCategoryRetrySemantics(t *testing.T) {
	t.Run("判慢不得同家重试且必须换家", func(t *testing.T) {
		if CategorySlowRate.RetriesSameProvider() {
			t.Fatal("判慢必须在同一家上不重试（否则客户端要白等一个完整判慢周期）")
		}
		if !CategorySlowRate.SwitchesProvider() {
			t.Fatal("判慢必须换家，否则候选耗尽即直接失败")
		}
	})

	t.Run("判慢不得计入熔断", func(t *testing.T) {
		if CategorySlowRate.CountsTowardCircuit() {
			t.Fatal("判慢不得计入熔断：慢不是错，计了就没有渐进恢复的机会")
		}
	})

	t.Run("真实供应商错误仍同家重试（防误伤）", func(t *testing.T) {
		if !CategoryProviderError.RetriesSameProvider() {
			t.Fatal("真实 4xx/5xx 必须仍同家重试——这一条被改动即为静默回归")
		}
		if !CategoryProviderError.CountsTowardCircuit() {
			t.Fatal("真实 4xx/5xx 必须仍计熔断")
		}
	})

	t.Run("分类标识不与链上词表冲突", func(t *testing.T) {
		// 链上 reason 由 reasonForCategory 给出，此档复用既有词，故不引入新的跨语言词。
		if got := reasonForCategory(CategorySlowRate, statusUpstreamTimeout); got != ReasonVendorTypeAllTimeout {
			t.Fatalf("链上 reason = %q，期望 %q（改变会让消费者判定失配）", got, ReasonVendorTypeAllTimeout)
		}
	})
}

// TestGateFailureClassifiesSlowSourcesDistinctly 钉住来源到分类的映射：
//   - 两条**主动判慢**来源（停滞 stall / 速率 rate）⇒ CategorySlowRate；
//   - 静默超时（FailIdleTimeout）与真实上游错误**不得**被并进新分类。
//
// 这是 F6 的核心钉子：分类只对「我们主动判定」生效，而不是把所有走 524 的都一起改掉。
func TestGateFailureClassifiesSlowSourcesDistinctly(t *testing.T) {
	cases := []struct {
		name     string
		reason   gate.FailReason
		want     Category
		wantSlow bool
	}{
		{"停滞探测判慢", gate.FailSlowProbe, CategorySlowRate, true},
		{"速率闸判慢", gate.FailSlowRate, CategorySlowRate, true},
		{"静默超时不是主动判慢", gate.FailIdleTimeout, CategoryProviderError, false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			deps := Deps{}
			failure := deps.gateFailure(&gate.PrecommitError{Reason: testCase.reason, ProviderID: 167},
				&Plan{URL: "https://example.com"}, &AttemptOutcome{ProviderID: 167, Attempt: 1})
			if failure == nil {
				t.Fatal("应映射为尝试失败")
			}
			if failure.Category != testCase.want {
				t.Fatalf("分类 = %s，期望 %s", failure.Category, testCase.want)
			}
			if failure.Category.RetriesSameProvider() == testCase.wantSlow {
				t.Fatalf("分类 %s 的同家重试属性与预期相反", failure.Category)
			}
			if failure.StatusCode != statusUpstreamTimeout {
				t.Fatalf("状态码 = %d，期望 %d（客户端可重试的 5xx）", failure.StatusCode, statusUpstreamTimeout)
			}
		})
	}
}

// TestForwardStreamSlowProbeSwitchesProviderWithoutSameProviderRetry 是 F6 的端到端钉子：
// 首家的首个字节到了之后长时间无内容（停滞探测判慢）⇒ **立即换家**，客户端由第二家拿到成功响应。
//
// 判据是**首家的命中次数**：重试上限给到 2，若仍走「先同家重试」则会是 2 次。这条断言比
// 「最终换了家」更硬——旧实现最终也会换家，只是白等了一轮。
func TestForwardStreamSlowProbeSwitchesProviderWithoutSameProviderRetry(t *testing.T) {
	var slowHits int32
	// 慢家：吐一个中性头帧（首字节已到）之后再无内容，直到探测判慢把它关掉。
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&slowHits, 1)
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-4-5\"}}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		// 停住：让停滞探测（T=1s）先于任何内容到达而触发。
		time.Sleep(3 * time.Second)
	}))
	t.Cleanup(slow.Close)

	fast := streamServer(t, "text/event-stream", claudeStreamChunks(), true)

	second := newTestCandidate(2, "供应商乙", fast.URL, 1)
	deps := Deps{
		Dial:   newTestDial(t),
		Facts:  newStreamFacts(),
		Limits: Limits{RetryDelay: time.Millisecond},
		Select: func(ctx context.Context, excludeIDs []int64) (*Candidate, error) {
			// 判慢必须先把慢家排进排除集，否则换家后会再次选中它。
			found := false
			for _, id := range excludeIDs {
				if id == 1 {
					found = true
				}
			}
			if !found {
				t.Fatalf("换家时未把被判慢的家排进排除集：%v", excludeIDs)
			}
			return second, nil
		},
	}

	// 首家的重试上限给 2：若判慢仍走「同家重试」，首家会被调用两次。
	options := StreamOptions{
		Format:                 convert.FormatClaude,
		Settle:                 newCountingSettler(),
		Logger:                 nil,
		StartedAt:              time.Now(),
		ProbeAfterFirstByteFor: func(int64) int { return 1 },
	}
	result, err := ForwardStream(context.Background(), newTestPctx(t),
		newTestCandidate(1, "供应商甲", slow.URL, 2), deps, options)
	if err != nil {
		t.Fatalf("ForwardStream 失败: %v", err)
	}
	if result.Stream == nil {
		t.Fatal("换家后应提交为流")
	}
	body, outcome := consumeStream(t, result.Stream)
	if outcome.Kind != TerminalCompleted {
		t.Fatalf("换家后的流应正常收尾，得 %+v", outcome.Kind)
	}
	if !strings.Contains(string(body), "pong") {
		t.Fatalf("客户端正文应来自第二家，得 %s", body)
	}
	if hits := atomic.LoadInt32(&slowHits); hits != 1 {
		t.Fatalf("慢家被调用 %d 次，期望 1（判慢不得在同一家重试）", hits)
	}

	// 留痕：首条是判慢（新分类 + 停滞来源），次条是成功。
	if len(result.Attempts) < 2 {
		t.Fatalf("应有两家各一条留痕，得 %+v", result.Attempts)
	}
	first := result.Attempts[0]
	if first.Category != CategorySlowRate {
		t.Fatalf("首家留痕分类 = %s，期望 %s", first.Category, CategorySlowRate)
	}
	if !first.ProbeSlow || first.ProbeSlowKind != ProbeSlowKindStall {
		t.Fatalf("首家留痕应带停滞判慢标记，得 %+v", first)
	}
	if result.Attempts[len(result.Attempts)-1].Reason != ReasonRequestSuccess {
		t.Fatalf("末条留痕应为成功，得 %+v", result.Attempts[len(result.Attempts)-1])
	}
}

// TestSlowRateFailureDoesNotRecordCircuitFailure 钉住熔断那一环真的**没被触发**（F6 要求④）。
//
// 为何不只断言 CountsTowardCircuit()：那只是谓词。真正的风险在调用点（attempt.go 与 hedge.go
// 各有一处 `if ... CountsTowardCircuit() ... RecordFailure`），谓词对了但某条路径忘了看它，
// 就会静默计熔断。这里用真调用点证明「判慢走完全程后 RecordFailure 一次都没发」。
func TestSlowRateFailureDoesNotRecordCircuitFailure(t *testing.T) {
	var circuitCalls int32
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-4-5\"}}\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		time.Sleep(3 * time.Second)
	}))
	t.Cleanup(slow.Close)

	deps := Deps{
		Dial:   newTestDial(t),
		Facts:  newStreamFacts(),
		Limits: Limits{RetryDelay: time.Millisecond},
		RecordFailure: func(context.Context, *Failure) {
			atomic.AddInt32(&circuitCalls, 1)
		},
		// 无更多候选：判慢之后直接以失败收场，以便走完「重试耗尽」那一段。
		Select: func(context.Context, []int64) (*Candidate, error) { return nil, nil },
	}

	_, _ = ForwardStream(context.Background(), newTestPctx(t),
		newTestCandidate(1, "供应商甲", slow.URL, 1), deps,
		StreamOptions{Format: convert.FormatClaude, ProbeAfterFirstByteFor: func(int64) int { return 1 }})

	if calls := atomic.LoadInt32(&circuitCalls); calls != 0 {
		t.Fatalf("判慢不得计入熔断，实际调用 RecordFailure %d 次", calls)
	}
}
