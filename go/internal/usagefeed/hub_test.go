package usagefeed

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// 本文件的用例分两组：
//
//   - **纯内存组**（不设 CCH_TEST_REDIS_URL 也跑）：窗口合并、槽位合并、未装配 Redis 的
//     空转不阻塞、关闭语义。
//   - **真 Redis 组**（门控 CCH_TEST_REDIS_URL）：跨实例广播、节流、以及「摘掉 publish 就收不到」
//     的反证。门控与仓库其余 Redis 测试一致。

// testRedis 连测试用 Redis（DB 13，与仓库其余用例一致）。
func testRedis(t *testing.T) redis.UniversalClient {
	t.Helper()
	raw := os.Getenv("CCH_TEST_REDIS_URL")
	if raw == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过 Redis 集成测试")
	}
	options, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("解析 Redis URL 失败（值已隐去）: %v", err)
	}
	if options.DB == 0 {
		options.DB = 13
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("Redis 不可达: %v", err)
	}
	return client
}

// --- 纯内存组 ---

// 一个窗口内的多次落库必须合并成**一条**广播：一次请求可能连写开行与终态两笔，
// 逐笔广播会白打一倍 Redis 流量。
func TestNotifyMergesWithinWindow(t *testing.T) {
	// 用一个假 client 拦下 Publish 的载荷：这里只关心「发了几条」。
	fake := newRecordingClient()
	hub := NewHub(Options{Redis: fake, MergeWindow: 50 * time.Millisecond})
	t.Cleanup(func() { _ = hub.Close() })

	for _, id := range []int64{101, 102, 103} {
		hub.NotifyNewRow(id)
	}

	got := fake.waitPublished(t, 2*time.Second, 1)
	if len(got) != 1 {
		t.Fatalf("窗口内 3 次落库应合并成 1 条广播，实际 %d 条：%v", len(got), got)
	}
	signal := decodeSignal(t, got[0])
	if signal.MaxID != 103 || signal.Count != 3 {
		t.Fatalf("合并语义应为 maxId=103 count=3，实际 %+v", signal)
	}
	// 窗口下界：三个 id 里最小的是 101。
	if signal.MinID != 101 {
		t.Fatalf("窗口下界应为 minId=101，实际 %+v", signal)
	}
}

// 无 Redis 时发布空转：**不阻塞、不 panic**。这是「旁路失败不得影响结算」的底座。
func TestNotifyWithoutRedisDoesNotBlockOrPanic(t *testing.T) {
	hub := NewHub(Options{MergeWindow: 10 * time.Millisecond})
	t.Cleanup(func() { _ = hub.Close() })

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := int64(1); i <= 100; i++ {
			hub.NotifyNewRow(i)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("无 Redis 时 NotifyNewRow 阻塞了调用方")
	}
}

// 晚结算的低 id 行必须把窗口下界拉下来。
//
// 这是 minId 存在的唯一理由：行 id 是**开行顺序**而非结算顺序。一条流式请求开行（id=200）
// 后流了五分钟，期间已经有 201..260 落库并结算；它现在才结算（id 仍是 200）。
// 只出发 maxId 的信号时，前端拿着 sinceId=260 永远看不到 200 那一行的用量与计费。
func TestNotifyTracksWindowLowerBoundForLateSettlement(t *testing.T) {
	fake := newRecordingClient()
	hub := NewHub(Options{Redis: fake, MergeWindow: 50 * time.Millisecond})
	t.Cleanup(func() { _ = hub.Close() })

	// 同一窗口内：先几条常规新行，再一条**它之前的**行晚结算。
	for _, id := range []int64{258, 259, 260, 200} {
		hub.NotifyNewRow(id)
	}

	got := fake.waitPublished(t, 2*time.Second, 1)
	if len(got) != 1 {
		t.Fatalf("一个窗口应只广播一条，实际 %d 条", len(got))
	}
	signal := decodeSignal(t, got[0])
	if signal.MaxID != 260 || signal.MinID != 200 || signal.Count != 4 {
		t.Fatalf("应为 maxId=260 minId=200 count=4，实际 %+v", signal)
	}
}

// 窗口下界不跨窗残留：每一条广播的下界只描述**自己那一窗**。
//
// 若下界不重置（把上一窗的最小值带下来），前端会永久用一个小得过分的 sinceId 重拉，
// 每轮把已经看过的几万行拉一遍——从“看不到”变成“看得到但很贵”。
func TestWindowLowerBoundResetsBetweenWindows(t *testing.T) {
	fake := newRecordingClient()
	hub := NewHub(Options{Redis: fake, MergeWindow: 30 * time.Millisecond})
	t.Cleanup(func() { _ = hub.Close() })

	hub.NotifyNewRow(100)
	hub.NotifyNewRow(300)
	first := fake.waitPublished(t, 2*time.Second, 1)
	if s := decodeSignal(t, first[0]); s.MinID != 100 || s.MaxID != 300 {
		t.Fatalf("第一窗应为 minId=100 maxId=300，实际 %+v", s)
	}

	hub.NotifyNewRow(500)
	hub.NotifyNewRow(501)
	second := fake.waitPublished(t, 2*time.Second, 2)
	if s := decodeSignal(t, second[1]); s.MinID != 500 || s.MaxID != 501 {
		t.Fatalf("第二窗应为 minId=500 maxId=501（下界不得跨窗残留），实际 %+v", s)
	}
}

// 非法 id 直接丢弃，不进窗口（否则会污染 maxId 语义）。
func TestNotifyIgnoresNonPositiveID(t *testing.T) {
	fake := newRecordingClient()
	hub := NewHub(Options{Redis: fake, MergeWindow: 20 * time.Millisecond})
	t.Cleanup(func() { _ = hub.Close() })

	hub.NotifyNewRow(0)
	hub.NotifyNewRow(-5)
	time.Sleep(80 * time.Millisecond)

	if got := fake.published(); len(got) != 0 {
		t.Fatalf("非法 id 不该产生广播，实际 %d 条", len(got))
	}
}

// 非法 id 不得污染窗口下界：它既不进计数，也不该把 minId 拉到 0（或负数）。
//
// 若被拉成 0，前端会按 `sinceId = -1` 重拉——一次全表增量的代价。
func TestNotifyIgnoresNonPositiveIDForLowerBound(t *testing.T) {
	fake := newRecordingClient()
	hub := NewHub(Options{Redis: fake, MergeWindow: 30 * time.Millisecond})
	t.Cleanup(func() { _ = hub.Close() })

	hub.NotifyNewRow(77)
	hub.NotifyNewRow(0)
	hub.NotifyNewRow(-9)

	got := fake.waitPublished(t, 2*time.Second, 1)
	signal := decodeSignal(t, got[0])
	if signal.MinID != 77 || signal.MaxID != 77 || signal.Count != 1 {
		t.Fatalf("非法 id 不得影响下界：应为 minId=77 maxId=77 count=1，实际 %+v", signal)
	}
}

// 槽位合并：订阅者来不及取走时，多条信号合并为「最大 id + 计数相加」，
// 而不是排队（无界队列在高频写入下会吃内存）。
func TestSubscriptionCoalescesPendingSignals(t *testing.T) {
	hub := NewHub(Options{})
	sub, dispose := hub.Subscribe()
	defer dispose()

	hub.fanout(Signal{MaxID: 10, MinID: 9, Count: 1})
	hub.fanout(Signal{MaxID: 12, MinID: 12, Count: 1})
	hub.fanout(Signal{MaxID: 11, MinID: 7, Count: 1})

	signal, ok := waitSignal(t, sub, time.Second)
	if !ok {
		t.Fatal("应能取到合并后的信号")
	}
	if signal.MaxID != 12 || signal.Count != 3 {
		t.Fatalf("槽位合并应为 maxId=12 count=3，实际 %+v", signal)
	}
	// 下界同样合并：两段窗口的并集下界是较小者（7），取大就会把早先的低 id 丢掉。
	if signal.MinID != 7 {
		t.Fatalf("槽位合并的下界应为 minId=7，实际 %+v", signal)
	}

	// 取走后不应再有残留信号。
	if _, ok := waitSignal(t, sub, 100*time.Millisecond); ok {
		t.Fatal("合并后的槽位应被取空")
	}
}

// 关闭订阅后 Wait 立刻返回 ErrSubscriptionClosed，不挂死。
func TestSubscriptionCloseUnblocksWait(t *testing.T) {
	hub := NewHub(Options{})
	sub, dispose := hub.Subscribe()
	defer dispose()

	errCh := make(chan error, 1)
	go func() {
		_, err := sub.Wait(context.Background())
		errCh <- err
	}()

	time.Sleep(20 * time.Millisecond)
	sub.Close()

	select {
	case err := <-errCh:
		if err != ErrSubscriptionClosed {
			t.Fatalf("关闭后应返回 ErrSubscriptionClosed，实际 %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("关闭订阅未能唤醒 Wait")
	}
}

// --- 真 Redis 组 ---

// 跨实例广播：A 进程发布，**B 进程**的订阅者收到。这条用例是「多实例」的判据——
// 少了它，负载均衡下的 SSE 会在别的实例上漏信号。
func TestCrossInstanceFanoutViaRedis(t *testing.T) {
	client := testRedis(t)

	publisher := NewHub(Options{Redis: client, MergeWindow: 30 * time.Millisecond})
	ready, onSubscribed := subscribedChan()
	subscriberHub := NewHub(Options{Redis: client, MergeWindow: 30 * time.Millisecond, OnSubscribed: onSubscribed})
	t.Cleanup(func() {
		_ = publisher.Close()
		_ = subscriberHub.Close()
	})

	sub, dispose := subscriberHub.Subscribe()
	defer dispose()

	// 等订阅真正建立，否则首条消息会丢（Redis 不补发）。
	waitSubscribed(t, ready, 3*time.Second)

	publisher.NotifyNewRow(4242)

	signal, ok := waitSignal(t, sub, 3*time.Second)
	if !ok {
		t.Fatal("另一实例的订阅者未收到跨实例广播")
	}
	if signal.MaxID != 4242 {
		t.Fatalf("maxId 应为 4242，实际 %d", signal.MaxID)
	}
}

// 节流：短时间连打 10 次落库，订阅端收到的**信号条数远小于 10**，
// 且合并后的 maxId/count 语义正确。
func TestThrottleCollapsesBurst(t *testing.T) {
	client := testRedis(t)

	ready, onSubscribed := subscribedChan()
	hub := NewHub(Options{Redis: client, MergeWindow: 100 * time.Millisecond, OnSubscribed: onSubscribed})
	t.Cleanup(func() { _ = hub.Close() })

	sub, dispose := hub.Subscribe()
	defer dispose()
	waitSubscribed(t, ready, 3*time.Second)

	for i := int64(1); i <= 10; i++ {
		hub.NotifyNewRow(1000 + i)
	}

	// 收一条即可判定节流生效（正常应几乎全部落在同一窗口）。
	first, ok := waitSignal(t, sub, 3*time.Second)
	if !ok {
		t.Fatal("未收到任何信号")
	}
	total := first.Count
	signals := 1
	for {
		more, ok := waitSignal(t, sub, 500*time.Millisecond)
		if !ok {
			break
		}
		total += more.Count
		signals++
		if more.MaxID > first.MaxID {
			first.MaxID = more.MaxID
		}
	}
	if signals >= 10 {
		t.Fatalf("10 次落库不该产生 %d 条信号（节流未生效）", signals)
	}
	if total != 10 {
		t.Fatalf("合并后的 count 合计应为 10（不得丢计数），实际 %d", total)
	}
	if first.MaxID != 1010 {
		t.Fatalf("合并后 maxId 应为 1010，实际 %d", first.MaxID)
	}
}

// 反证：摘掉发布（不调 NotifyNewRow）就收不到 new-rows。
// 这条用例保证上面那些用例的绿色来自真实链路，而不是订阅端自己造信号。
func TestCounterProofWithoutPublishNoSignal(t *testing.T) {
	client := testRedis(t)

	ready, onSubscribed := subscribedChan()
	hub := NewHub(Options{Redis: client, MergeWindow: 30 * time.Millisecond, OnSubscribed: onSubscribed})
	t.Cleanup(func() { _ = hub.Close() })

	sub, dispose := hub.Subscribe()
	defer dispose()
	waitSubscribed(t, ready, 3*time.Second)

	// 故意不发布。同时往通道里塞一条**非法载荷**：订阅端必须存活且不误报信号。
	if err := client.Publish(context.Background(), ChannelNewRows, "not-json").Err(); err != nil {
		t.Fatalf("发布非法载荷失败: %v", err)
	}

	if _, ok := waitSignal(t, sub, 500*time.Millisecond); ok {
		t.Fatal("未调用 NotifyNewRow 却收到了信号（链路有旁路）")
	}
}

// Redis 断开时：主业务方（调用 NotifyNewRow 的路径）不受影响，订阅循环按退避重试。
func TestNotifySurvivesRedisFailure(t *testing.T) {
	// 指向一个必然拒连的端口：发布失败只记 warn，不得冒泡。
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond})
	t.Cleanup(func() { _ = client.Close() })

	hub := NewHub(Options{Redis: client, MergeWindow: 10 * time.Millisecond})
	t.Cleanup(func() { _ = hub.Close() })

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := int64(1); i <= 5; i++ {
			hub.NotifyNewRow(i)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Redis 不可达时 NotifyNewRow 阻塞了调用方")
	}
}
