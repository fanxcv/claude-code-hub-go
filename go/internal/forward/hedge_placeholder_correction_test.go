package forward

import (
	"testing"
	"time"
)

// 本文件钉住「取消占位结论」的校正规则（hedge.go 的 correctCancelledPlaceholderLocked）。
//
// 背景：胜者裁决与失败观测抢同一把 mutex，谁先拿到谁定 attempt 的留痕，于是同一份事实会
// 按调度顺序得出两种结论。CI 上 2 核 runner 命中的就是这个交错：attempt 先被标成
// hedge_loser_cancelled，随后才观测到上游真回的 500，那条失败被静默丢弃。
//
// 为什么用状态构造而不是端到端复现那条交错：两个协程谁先拿锁取决于调度，没有生产钩子可
// 稳定把「裁决已落锁、失败尚未落锁」排成一前一后；硬造只能靠 sleep 或真实超时（门禁禁等
// 真实长超时）。故这里直接构造裁决后的状态，再调**真实的失败落痕入口**
// finishAttemptFailed——它里面的校正调用点正是生产路径上的那一处。
//
// 两个方向都要钉：
//   - 真失败了（上游确实回了 4xx/5xx）→ 改正，链上必须留下诊断信息；
//   - 只是输了竞速（我们主动取消）→ 保持占位，别把「我们的取消」说成上游故障。

// TestHedgeRaceCorrectsLoserPlaceholderWhenLateFailureArrives 钉住生产入口上的校正：
// 胜者已裁决（attempt 已落取消占位），随后真实的上游失败才到达。
//
// 走的是 finishAttemptFailed 本身，故它同时钉住「调用点在不在」——把 hedge.go 里那行
// correctCancelledPlaceholderLocked 去掉，本用例立刻转红。
func TestHedgeRaceCorrectsLoserPlaceholderWhenLateFailureArrives(t *testing.T) {
	newRace := func() (*hedgeRace, *hedgeAttempt) {
		race := &hedgeRace{
			outcomes: []AttemptOutcome{{
				ProviderID:   1,
				ProviderName: "供应商甲",
				Attempt:      1,
				Reason:       ReasonHedgeLoserCancel,
			}},
			winnerCommitted: true, // 胜者已裁决
			active:          1,    // 该 attempt 仍在途
			now:             time.Now,
			cfg:             HedgeOptions{MaxInFlight: 1}, // 不允许再拉备选（本用例只关心落痕）
		}
		attempt := &hedgeAttempt{
			provider:        Provider{ID: 1, Name: "供应商甲"},
			seq:             1,
			outcomeRecorded: true, // 裁决时已落过取消占位
		}
		return race, attempt
	}

	t.Run("上游真回 500 → 占位被改正且只留一条", func(t *testing.T) {
		race, attempt := newRace()
		race.finishAttemptFailed(attempt, &Failure{
			Category:   CategoryProviderError,
			StatusCode: 500,
			Message:    "Provider returned 500: upstream boom",
		})

		if len(race.outcomes) != 1 {
			t.Fatalf("同一 attempt 只应有一条留痕，实际 %d 条：%+v", len(race.outcomes), race.outcomes)
		}
		entry := race.outcomes[0]
		if entry.Reason != ReasonRetryFailed {
			t.Fatalf("reason = %q，期望 %q（真实上游失败不得被取消占位抹掉）", entry.Reason, ReasonRetryFailed)
		}
		if entry.StatusCode != 500 {
			t.Fatalf("statusCode = %d，期望 500", entry.StatusCode)
		}
		if entry.Message == "" {
			t.Fatal("改正后丢了上游错误文案，决策链上仍然看不到失败原因")
		}
		if race.active != 0 {
			t.Fatalf("active = %d，期望 0（在途计数必须归零，否则排空闸门永远等不完）", race.active)
		}
	})

	t.Run("客户端取消 → 保持「竞速输家」占位", func(t *testing.T) {
		race, attempt := newRace()
		race.finishAttemptFailed(attempt, &Failure{
			Category: CategoryClientAbort,
			Message:  "客户端取消",
		})

		if len(race.outcomes) != 1 {
			t.Fatalf("同一 attempt 只应有一条留痕，实际 %d 条", len(race.outcomes))
		}
		if got := race.outcomes[0].Reason; got != ReasonHedgeLoserCancel {
			t.Fatalf("reason = %q，期望 %q（我们主动取消它，不该说成上游故障）",
				got, ReasonHedgeLoserCancel)
		}
	})

	t.Run("失败未被记录过（裁决未落痕）→ 正常追加失败留痕", func(t *testing.T) {
		race, attempt := newRace()
		attempt.outcomeRecorded = false
		race.outcomes = nil

		race.finishAttemptFailed(attempt, &Failure{
			Category:   CategoryProviderError,
			StatusCode: 502,
			Message:    "Provider returned 502",
		})

		if len(race.outcomes) != 1 {
			t.Fatalf("应追加一条失败留痕，实际 %d 条", len(race.outcomes))
		}
		if race.outcomes[0].Reason != ReasonRetryFailed || race.outcomes[0].StatusCode != 502 {
			t.Fatalf("留痕不对：%+v", race.outcomes[0])
		}
	})
}

// TestCorrectCancelledPlaceholderRuleTable 钉住校正规则的排除项。
//
// 排除项不是保守起见，而是语义：只有「上游确实回了 4xx/5xx」才说明它自己失败了；
// 我们自己的取消、以及「读正文时被取消」这类只在文案里才看得出归属的系统错误，
// 占位结论（我们主动放弃了它）本就是最准确的描述。
func TestCorrectCancelledPlaceholderRuleTable(t *testing.T) {
	loser := AttemptOutcome{ProviderID: 1, Attempt: 1, Reason: ReasonHedgeLoserCancel}
	billed := AttemptOutcome{ProviderID: 1, Attempt: 1, Reason: ReasonHedgeLoserBilled}

	cases := []struct {
		name       string
		entry      AttemptOutcome
		failure    AttemptOutcome
		wantReason string
		wantStatus int
	}{
		{
			name:  "上游真回 500 → 就地改正",
			entry: loser,
			failure: AttemptOutcome{
				Category: CategoryProviderError, StatusCode: 500,
				Reason: ReasonRetryFailed, Message: "Provider returned 500",
			},
			wantReason: ReasonRetryFailed,
			wantStatus: 500,
		},
		{
			name:  "客户端取消 → 保持占位",
			entry: loser,
			failure: AttemptOutcome{
				Category: CategoryClientAbort, Reason: ReasonClientAbort,
			},
			wantReason: ReasonHedgeLoserCancel,
		},
		{
			name:  "系统错误（读正文被取消）→ 保持占位",
			entry: loser,
			failure: AttemptOutcome{
				Category: CategorySystemError, StatusCode: 500,
				Reason: ReasonSystemError, Message: "读取上游正文失败: context canceled",
			},
			wantReason: ReasonHedgeLoserCancel,
		},
		{
			name:  "无真实状态码的供应商故障 → 保持占位",
			entry: loser,
			failure: AttemptOutcome{
				Category: CategoryProviderError, StatusCode: 0,
				Reason: ReasonRetryFailed,
			},
			wantReason: ReasonHedgeLoserCancel,
		},
		{
			name:  "计费输家（hedge_loser_billed）→ 不碰（牵着成本写回）",
			entry: billed,
			failure: AttemptOutcome{
				Category: CategoryProviderError, StatusCode: 502,
				Reason: ReasonRetryFailed, Message: "Provider returned 502",
			},
			wantReason: ReasonHedgeLoserBilled,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			race := &hedgeRace{outcomes: []AttemptOutcome{testCase.entry}}
			attempt := &hedgeAttempt{provider: Provider{ID: 1}, seq: 1}

			race.correctCancelledPlaceholderLocked(attempt, testCase.failure)

			if len(race.outcomes) != 1 {
				t.Fatalf("不得新增或删除留痕，实际 %d 条", len(race.outcomes))
			}
			got := race.outcomes[0]
			if got.Reason != testCase.wantReason {
				t.Fatalf("reason = %q，期望 %q", got.Reason, testCase.wantReason)
			}
			if got.StatusCode != testCase.wantStatus {
				t.Fatalf("statusCode = %d，期望 %d", got.StatusCode, testCase.wantStatus)
			}
		})
	}

	t.Run("无匹配条目时不 panic 也不改", func(t *testing.T) {
		race := &hedgeRace{outcomes: []AttemptOutcome{
			{ProviderID: 2, Attempt: 1, Reason: ReasonHedgeLoserCancel},
		}}
		race.correctCancelledPlaceholderLocked(&hedgeAttempt{provider: Provider{ID: 1}, seq: 1},
			AttemptOutcome{Category: CategoryProviderError, StatusCode: 500, Reason: ReasonRetryFailed})
		if race.outcomes[0].Reason != ReasonHedgeLoserCancel {
			t.Fatalf("别家供应商的留痕被改动: %+v", race.outcomes[0])
		}
	})

	t.Run("nil attempt 不 panic", func(t *testing.T) {
		race := &hedgeRace{outcomes: []AttemptOutcome{loser}}
		race.correctCancelledPlaceholderLocked(nil,
			AttemptOutcome{Category: CategoryProviderError, StatusCode: 500, Reason: ReasonRetryFailed})
		if race.outcomes[0].Reason != ReasonHedgeLoserCancel {
			t.Fatal("nil attempt 竟然改了留痕")
		}
	})
}
