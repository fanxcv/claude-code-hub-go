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
//
// 注：本用例的复核值（streak/samples）按现场真实值传入，以便走到 HGET 那一跳——若复核不通过
// 就会提前放弃，hook 永远不会触发，用例失去分辨力（见 fired 断言）。
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
	// 慢样本已把 streak 键删掉（那是恢复计数的归零），本用例要单独考验「WATCH 窗口内被改」，
	// 故把计数键按预期值重建。
	if err := h.client.Set(ctx, streak, 1, time.Minute).Err(); err != nil {
		t.Fatalf("重建 streak 失败: %v", err)
	}

	// 第二条连接：在 WATCH 回调读到 state 之后改它（模拟并发慢样本写入）。
	concurrent := recoveryRedis(t)
	writer := &concurrentWriteHook{client: concurrent, key: state, value: "concurrent-slow-sample"}
	h.client.AddHook(writer)

	if err := h.recorder.resetAfterRecovery(ctx, h.cleanFacts(902), window, state, streak, 1, h.countSamples()); err != nil {
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

// slowSampleAfterStreakHook 在「含 INCR 的那条 pipeline 提交之后」立即落下一条完整的慢样本效果。
//
// 它模拟的**正是**被修的那段交错：INCR 已提交，而 resetAfterRecovery 的 WATCH 尚未发出——
// 此时慢样本 DEL streak / ZADD window / HSET state，三键随即成为 WATCH 的**基线快照**，
// 单靠 WATCH 挡不住，DEL 会照常提交把慢样本抹掉。
//
// 为何必须挂 pipeline hook 而不是直接手工改键：手工在 `Record` 之前改键就不是这段交错了，
// 用例会退化成「WATCH 监视它建立之后的改动」（Redis 行为，非本代码的）。
type slowSampleAfterStreakHook struct {
	client    redis.UniversalClient
	windowKey string
	stateKey  string
	streakKey string
	member    string
	fired     bool
}

func (h *slowSampleAfterStreakHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *slowSampleAfterStreakHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return next
}

func (h *slowSampleAfterStreakHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		err := next(ctx, cmds)
		if h.fired || err != nil {
			return err
		}
		carriesIncr := false
		for _, cmd := range cmds {
			if cmd.Name() == "incr" {
				carriesIncr = true
				break
			}
		}
		if !carriesIncr {
			return err
		}
		h.fired = true
		// 一条完整的慢样本效果：三键同改（与 recorder.go 慢分支一致）。
		pipe := h.client.Pipeline()
		pipe.Del(ctx, h.streakKey)
		pipe.ZAdd(ctx, h.windowKey, redis.Z{Score: 1790046000000, Member: h.member})
		pipe.HSet(ctx, h.stateKey, StateFieldPenalty, 20)
		_, _ = pipe.Exec(ctx)
		return err
	}
}

// TestRecoveryAbortsWhenSlowSampleLandsBeforeWatch 是本次修复的**主判据**：
//
// 慢样本恰好落在「INCR pipeline 提交」与「WATCH 发出」之间时，恢复路径必须放弃删除——
// window 与 state 都不得被删（那是刚落盘的慢样本及其惩罚，删了就是错误恢复 + 证据销毁）。
//
// 为何必须钉：这条交错下 `WATCH` 完全看不出异常（它把被改过的三键当作基线快照），
// DEL 照常提交、零报错、零日志——是「静默错」的典型形态。
func TestRecoveryAbortsWhenSlowSampleLandsBeforeWatch(t *testing.T) {
	const n = 1
	h := newRecoveryHarness(t, 9215, n)
	h.seedPenalty()
	ctx := context.Background()
	window, state, streak := h.keys()

	concurrent := recoveryRedis(t)
	interleave := &slowSampleAfterStreakHook{
		client:    concurrent,
		windowKey: window,
		stateKey:  state,
		streakKey: streak,
		member:    "concurrent-slow-sample",
	}
	h.client.AddHook(interleave)

	// 干净样本：INCR 到 1 = 阈值，随即触发恢复路径（hook 在此期间插入慢样本）。
	h.recorder.Record(ctx, h.cleanFacts(1001))

	if !interleave.fired {
		t.Fatal("夹具未触发（未捕获含 INCR 的 pipeline）——用例失去分辨力")
	}
	if !h.exists(window) {
		t.Fatal("慢样本落在 INCR 与 WATCH 之间时，滑窗不得被删（那条慢样本会被抹掉且无任何报错）")
	}
	if !h.exists(state) {
		t.Fatal("慢样本落在 INCR 与 WATCH 之间时，state 不得被删（渠道会被错误解除降权）")
	}
	if _, err := concurrent.ZScore(ctx, window, "concurrent-slow-sample").Result(); err != nil {
		t.Fatalf("那条慢样本必须仍在滑窗里，实得 %v", err)
	}
}

// TestRecoveryResetsWithoutConcurrentWrite 是对照（场景 B）：无并发写时一切照旧——
// 三键全清，且记出一条 reason=recovery 的解除事件。
//
// 它同时是「复核不得误伤正常路径」的判据：若复核写错了（比如拿错值自比），本用例会红。
func TestRecoveryResetsWithoutConcurrentWrite(t *testing.T) {
	const n = 2
	h := newRecoveryHarness(t, 9216, n)
	h.seedPenalty()
	ctx := context.Background()
	window, state, streak := h.keys()

	h.recorder.Record(ctx, h.cleanFacts(1101))
	h.recorder.Record(ctx, h.cleanFacts(1102))

	if h.exists(state) {
		t.Fatal("无并发写下，达阈值应删除 state")
	}
	if h.exists(window) {
		t.Fatal("无并发写下，达阈值应删除滑窗")
	}
	if h.exists(streak) {
		t.Fatal("无并发写下，达阈值应删除 streak")
	}
	var found bool
	for _, event := range readSlowLogs(t, h, 9216) {
		if event.Kind == slowlog.KindPenaltyDown && event.Reason == "recovery" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("无并发写下应记出一条 reason=recovery 的解除事件")
	}
}

// TestRecoveryRecheckHoldsAtThresholdBoundary 覆盖边界（场景 C）：N=1 与 N=2 下，
// 复核不得把正常重置误判成并发写（否则恢复永远不发生，惩罚挂到 state TTL）。
//
// 为何单独钉边界：N=1 时复核值就是 1，与「慢样本 DEL 后紧接另一干净样本 INCR 回 1」
// 的巧合值同形，是最容易被复核误伤（或误放）的一档。
func TestRecoveryRecheckHoldsAtThresholdBoundary(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name     string
		provider int64
		n        int
	}{{"N=1", 9217, 1}, {"N=2", 9218, 2}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newRecoveryHarness(t, tc.provider, tc.n)
			h.seedPenalty()
			_, state, streak := h.keys()
			for i := 0; i < tc.n; i++ {
				h.recorder.Record(ctx, h.cleanFacts(int64(1200+i)))
			}
			if h.exists(state) {
				t.Fatalf("%s：达阈值应重置，state 仍在（复核误判为并发写）", tc.name)
			}
			if h.exists(streak) {
				t.Fatalf("%s：达阈值应清 streak，仍在（下一个干净样本会再触发一次重置）", tc.name)
			}
		})
	}
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
