package slowrate

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/slowlog"
	"github.com/redis/go-redis/v9"
)

// 本文件钉住**恢复路径**的两件事（用户 2026-09-22）：
//
//  1. 重置必须**原子**（WATCH/MULTI），且「期间有慢样本落盘」时放弃删除而不是抹掉它；
//  2. 重置必须留一条 `penalty_down` 事件（此前只删键、不记日志，界面上看不到「何时恢复的」）。
//
// 另含一条**边界**用例（N=1）：恢复阈值设成 1 时，「紧接慢样本之后的第一条干净样本」
// 才是那条该触发重置的样本——慢样本自身不得被算作「干净」。
//
// 为什么这些必须钉：三者都**不会报错**。抹掉一条慢样本只是少算一次；不记日志只是界面缺一行；
// N=1 的边界错位会让「刚被压下去就立刻解除」——全部静默。

const (
	recoveryResetProviderOne = 9210
	recoveryResetProviderTwo = 9211
)

// TestRecoveryResetLeavesPenaltyDownLog 钉住恢复时记一条 `penalty_down`（from=N to=0, reason=recovery）。
func TestRecoveryResetLeavesPenaltyDownLog(t *testing.T) {
	const n = 2
	h := newRecoveryHarness(t, recoveryResetProviderOne, n)
	h.seedPenalty()
	ctx := context.Background()

	// 两条干净样本达阈值 ⇒ 触发重置。
	h.recorder.Record(ctx, h.cleanFacts(501))
	h.recorder.Record(ctx, h.cleanFacts(502))

	events := readSlowLogs(t, h, recoveryResetProviderOne)
	var reset *slowlog.Event
	for index := range events {
		if events[index].Kind == slowlog.KindPenaltyDown && events[index].Reason == "recovery" {
			reset = &events[index]
			break
		}
	}
	if reset == nil {
		t.Fatalf("恢复重置必须留一条 reason=recovery 的 %s 事件，实得 %d 条：%+v",
			slowlog.KindPenaltyDown, len(events), events)
	}
	if reset.PenaltyTo == nil || *reset.PenaltyTo != 0 {
		t.Fatalf("解除后降权量应为 0，实得 %v", reset.PenaltyTo)
	}
	if reset.PenaltyFrom == nil || *reset.PenaltyFrom <= 0 {
		t.Fatalf("应记下被解除的旧降权量（>0），实得 %v", reset.PenaltyFrom)
	}
}

// TestRecoveryResetLogsNothingWhenPenaltyWasZero 钉住「本来就是 0」不记事件。
//
// 判据：没有降权可说时，界面不该多出一行「0 → 0 解除」。
func TestRecoveryResetLogsNothingWhenPenaltyWasZero(t *testing.T) {
	const n = 1
	h := newRecoveryHarness(t, recoveryResetProviderTwo, n)
	ctx := context.Background()
	// 不 seedPenalty（state 键不存在 ⇒ 旧值读不到 ⇒ 视作 0），直接两条干净样本。
	h.recorder.Record(ctx, h.cleanFacts(601))
	h.recorder.Record(ctx, h.cleanFacts(602))

	events := readSlowLogs(t, h, recoveryResetProviderTwo)
	for _, event := range events {
		if event.Kind == slowlog.KindPenaltyDown && event.Reason == "recovery" {
			t.Fatalf("无降权可说时不该记解除事件，实得 %+v", event)
		}
	}
}

// TestRecoveryRequiresCleanSampleAfterSlowOne 钉住 N=1 的边界：慢样本自身不得算作「干净」。
//
// 造法：N=1。先写一条**慢**样本（它会写 state 并打断 streak），再写一条干净样本。
// 断言重置发生在第二条之后，而**不是**慢样本之后——后者的实现会让「刚压下去就立刻解除」。
//
// N=2 作为对照同在本用例：N=2 时两条干净样本才够。
func TestRecoveryRequiresCleanSampleAfterSlowOne(t *testing.T) {
	ctx := context.Background()

	// N=1：一条慢 + 一条干净 ⇒ 应已重置（干净那一条触发的）。
	one := newRecoveryHarness(t, 9212, 1)
	one.seedPenalty()
	one.recorder.Record(ctx, one.slowFactsFor(701))
	_, state, _ := one.keys()
	if !one.exists(state) {
		t.Fatal("N=1 时慢样本自身不得触发重置（第一条样本就是慢的）")
	}
	one.recorder.Record(ctx, one.cleanFacts(702))
	if one.exists(state) {
		t.Fatal("N=1 时「紧接慢样本后的第一条干净样本」应触发重置")
	}

	// N=2 对照：一条慢 + 一条干净 不足以重置，第二条干净才够。
	two := newRecoveryHarness(t, 9213, 2)
	two.seedPenalty()
	two.recorder.Record(ctx, two.slowFactsFor(801))
	two.recorder.Record(ctx, two.cleanFacts(802))
	_, state2, _ := two.keys()
	if !two.exists(state2) {
		t.Fatal("N=2 时一条干净样本不足以重置")
	}
	two.recorder.Record(ctx, two.cleanFacts(803))
	if two.exists(state2) {
		t.Fatal("N=2 时第二条干净样本应触发重置")
	}
}

// TestRecoveryResetAbortsWhenSlowSampleLands 钉住「WATCH 窗口内」有并发写入时放弃重置。
//
// 造法：在 `resetAfterRecovery` 的 WATCH 回调里那条 HGET **返回之后**（即 EXEC 之前），
// 用另一条连接改 state 键——模拟真实的并发慢样本写入。断言三键仍在。
//
// 为何必须走 hook：若在调用 resetAfterRecovery **之前**改键，WATCH 是之后才开始监视的，
// 根本不构成冲突（那只能证明「WATCH 监视的是它建立之后的改动」，是 Redis 的行为，不是本代码的）。
// 这条分辨力差异是首版用例没跑通的原因，记下以免退化。
func TestRecoveryResetAbortsWhenSlowSampleLands(t *testing.T) {
	const n = 1
	h := newRecoveryHarness(t, 9214, n)
	h.seedPenalty()
	ctx := context.Background()

	// 一条慢样本先把 state 与滑窗立起来（它们就是本次要保护的对象）。
	h.recorder.Record(ctx, h.slowFactsFor(901))
	window, state, streak := h.keys()
	if !h.exists(state) || !h.exists(window) {
		t.Fatal("慢样本应立起 state 与滑窗")
	}

	// 第二条连接：在 WATCH 回调读到 state 之后改它（模拟并发慢样本写入）。
	concurrent := recoveryRedis(t)
	writer := &concurrentWriteHook{client: concurrent, key: state, value: "concurrent-slow-sample"}
	h.client.AddHook(writer)

	if err := h.recorder.resetAfterRecovery(ctx, h.cleanFacts(902), window, state, streak); err != nil {
		t.Fatalf("并发写入应按「放弃」处理（返回 nil，不报错），实得 %v", err)
	}
	if !writer.fired {
		t.Fatal("夹具未触发（hook 未发并发写）——用例失去分辨力")
	}
	if !h.exists(state) {
		t.Fatal("WATCH 侦测到并发写入后不得删除 state（那可能正是并发写入的慢样本）")
	}
	if !h.exists(window) {
		t.Fatal("WATCH 侦测到并发写入后不得删除滑窗")
	}
}

// concurrentWriteHook 在 WATCH 回调的 HGET 返回后（EXEC 之前）用另一条连接改目标键。
type concurrentWriteHook struct {
	client redis.UniversalClient
	key    string
	value  string
	fired  bool
}

func (h *concurrentWriteHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *concurrentWriteHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if !h.fired && cmd.Name() == "hget" {
			h.fired = true
			_ = h.client.Set(ctx, h.key, h.value, time.Minute).Err()
		}
		return err
	}
}

func (h *concurrentWriteHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// readSlowLogs 读该渠道的低速事件流（本文件的断言只看事件，不看 Redis 命令）。
func readSlowLogs(t *testing.T, h *recoveryHarness, providerID int64) []slowlog.Event {
	t.Helper()
	items, err := h.client.XRevRangeN(context.Background(), slowlog.Key(providerID), "+", "-", 50).Result()
	if err != nil {
		t.Fatalf("读低速事件流失败: %v", err)
	}
	events := make([]slowlog.Event, 0, len(items))
	for _, item := range items {
		raw, ok := item.Values["event"].(string)
		if !ok {
			continue
		}
		var event slowlog.Event
		if err := json.Unmarshal([]byte(raw), &event); err != nil {
			continue
		}
		events = append(events, event)
	}
	return events
}
