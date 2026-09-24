package route

import (
	"context"
	"testing"
	"time"
)

// 本文件钉住探针租约的**单飞语义**（不变量 ③ 的实现面）：
//
//	⑥ 持有期间第二个 acquire 失败；
//	⑦ 释放后可立即重获（不必等满 TTL）；
//	⑧ 续租与释放都只动**本持有者**的租约（compare-and-*）。
//
// 为什么必须有本文件：审计发现原实现只有 SET NX + 固定 30s TTL，无续租、无释放，而生产探针
// 最长可达 requestTimeoutNonStreamingMs（出厂默认 120s）大于 TTL——TTL 到期后第二个实例可再
// 取租约，多探针重叠，文件里「全实例最多 1 个在飞」的宣称不成立。
//
// 三条用例都靠替身（failOpenRedis）的真实键语义，而不是「有没有调 Redis」：替身若把
// 「键存在即 SET NX 失败」放宽，第六条即假绿。

// TestSlowProbeLeaseExcludesSecondHolderUntilRelease 钉住 ⑥ 与 ⑦：持有期间第二个 acquire
// 失败，释放后立即重获。
//
// 反证：把 AcquireSlowProbe 的 SetNX 换成 Set ⇒ 第二段红；把 keepSlowProbeAlive 的取消分支
// 去掉（不释放）⇒ 第三段红（重获要等满 30s TTL，而用例只给 5s）。
func TestSlowProbeLeaseExcludesSecondHolderUntilRelease(t *testing.T) {
	client := newFailOpenRedis()
	reader := NewSlowRateReader(SlowRateOptions{Redis: client})
	ctx, cancel := context.WithCancel(context.Background())

	first := reader.AcquireSlowProbe(ctx, 1, "m1", "holder-a")
	if first == nil {
		t.Fatal("首次取租约失败：空键上的 SET NX 必须成功")
	}
	if second := reader.AcquireSlowProbe(context.Background(), 1, "m1", "holder-b"); second != nil {
		t.Fatalf("持有期间第二个 acquire 成功（%+v）：单飞语义失效", second)
	}
	// 另一组合不受影响：租约是「渠道 x 模型」级的，不是全局单飞。
	if other := reader.AcquireSlowProbe(ctx, 2, "m1", "holder-b"); other == nil {
		t.Fatal("另一渠道取租约失败：租约作用域必须是组合级")
	}

	cancel()
	if !waitLeaseReleased(t, client, 1, "m1") {
		t.Fatalf("ctx 取消 5s 后租约键仍在：终态释放未生效（退化为等满 %v 的 TTL）", slowProbeLeaseTTL)
	}

	regainCtx, cancelRegain := context.WithCancel(context.Background())
	defer cancelRegain()
	third := reader.AcquireSlowProbe(regainCtx, 1, "m1", "holder-c")
	if third == nil {
		t.Fatal("释放后重获失败：终态释放须让下一个探针立即进场，而不是等满 TTL")
	}
}

// TestSlowProbeLeaseRenewsWhileInFlight 钉住续租循环真的在跑，且只续自己的租约。
//
// 反证：把 keepSlowProbeAlive 的 ticker 分支删掉（只留取消即释放）⇒ 续租计数为 0，本用例红。
func TestSlowProbeLeaseRenewsWhileInFlight(t *testing.T) {
	client := newFailOpenRedis()
	reader := NewSlowRateReader(SlowRateOptions{Redis: client})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	grant := &SlowProbeGrant{ProviderID: 1, ModelKey: "m1", Holder: "holder-a", key: SlowProbeLeaseKey(1, "m1")}
	client.values[grant.key] = grant.Holder

	done := make(chan struct{})
	go func() {
		reader.keepSlowProbeAlive(ctx, grant, time.Millisecond)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for client.renewalCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if client.renewalCount() == 0 {
		t.Fatal("在飞期间没有发出任何续租：探针长于 TTL 时租约必然中途失效")
	}
	if got, _ := client.leaseValue(grant.key); got != grant.Holder {
		t.Fatalf("续租改动了租约值 = %q，期望仍是本持有者 %q", got, grant.Holder)
	}
	cancel()
	<-done
	if !waitLeaseReleased(t, client, 1, "m1") {
		t.Fatal("续租 goroutine 退出后租约仍在")
	}
}

// TestSlowProbeLeaseReleaseKeepsForeignLease 钉住 ⑧：compare-and-delete 不得删掉别人的租约。
//
// 场景正是单飞要防的重叠窗口：本持有者的租约已因 TTL 过期被别人重取，本持有者随后才释放——
// 无条件 DEL 会把新持有者的租约删掉，第三个探针立刻挤进来，两个探针同时在飞。该路径真实可达：
// ctx 取消与 TTL 到期是两个独立时钟。
//
// 反证：把 slowProbeReleaseLua 的比对换成无条件 DEL ⇒ 本用例红。
func TestSlowProbeLeaseReleaseKeepsForeignLease(t *testing.T) {
	client := newFailOpenRedis()
	reader := NewSlowRateReader(SlowRateOptions{Redis: client})
	grant := &SlowProbeGrant{ProviderID: 1, ModelKey: "m1", Holder: "holder-a", key: SlowProbeLeaseKey(1, "m1")}
	// 模拟 TTL 过期后他人重取：键值换成别人的持有者标识。
	client.values[grant.key] = "holder-b"

	reader.releaseSlowProbe(context.Background(), grant)
	if got, _ := client.leaseValue(grant.key); got != "holder-b" {
		t.Fatalf("释放后租约值 = %q，期望仍是 holder-b（旧持有者不得删掉新持有者的租约）", got)
	}
}

// waitLeaseReleased 等释放 goroutine 真的删掉键（释放是异步的，不能同步断言）。
func waitLeaseReleased(t *testing.T, client *failOpenRedis, providerID int64, modelKey string) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, held := client.leaseValue(SlowProbeLeaseKey(providerID, modelKey)); !held {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
