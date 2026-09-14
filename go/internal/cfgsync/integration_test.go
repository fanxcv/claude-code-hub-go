package cfgsync

import (
	"context"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// 集成测试的隔离纪律：只允许用 DB index >= 13 的库，避免污染开发/压测数据。
// 未设置 CCH_TEST_REDIS_URL 时整组跳过（仓库要求单测可在无依赖环境跑绿）。
const (
	testRedisEnv      = "CCH_TEST_REDIS_URL"
	testRedisMinDB    = 13
	testRedisDeadline = 5 * time.Second
)

func testRedisClient(t *testing.T) *redis.Client {
	t.Helper()

	raw := os.Getenv(testRedisEnv)
	if raw == "" {
		t.Skipf("未设置 %s，跳过 Redis 集成测试", testRedisEnv)
	}

	options, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("解析 %s 失败：%v", testRedisEnv, err)
	}
	if options.DB < testRedisMinDB {
		t.Fatalf("%s 必须使用 DB index >= %d，收到 %d", testRedisEnv, testRedisMinDB, options.DB)
	}

	client := redis.NewClient(options)
	ctx, cancel := context.WithTimeout(context.Background(), testRedisDeadline)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		t.Fatalf("连接 Redis 失败：%v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func waitForMessage(t *testing.T, channel <-chan string, predicate func(string) bool, description string) string {
	t.Helper()

	deadline := time.After(testRedisDeadline)
	for {
		select {
		case message := <-channel:
			if predicate(message) {
				return message
			}
		case <-deadline:
			t.Fatalf("等待 %s 超时", description)
			return ""
		}
	}
}

func TestBusDeliversInitialResyncThenInvalidation(t *testing.T) {
	client := testRedisClient(t)
	bus := NewBus(client, nil)
	defer func() { _ = bus.Close() }()

	messages := make(chan string, 16)
	dispose, err := bus.Subscribe(ChannelSystemSettingsUpdated, func(message string) {
		messages <- message
	})
	if err != nil {
		t.Fatalf("订阅失败：%v", err)
	}

	// 首次订阅成功必须立刻 resync：登记前缓存可能已经加载过。
	waitForMessage(t, messages, func(message string) bool { return message == ResyncMessage },
		"首次订阅的 resync")

	bus.Publish(context.Background(), ChannelSystemSettingsUpdated, "")
	timestamp := waitForMessage(t, messages, func(message string) bool {
		value, err := strconv.ParseInt(message, 10, 64)
		return err == nil && value > 0
	}, "空消息应回填毫秒时间戳")
	if timestamp == ResyncMessage {
		t.Fatal("空消息不应伪装成 resync")
	}

	dispose()
	if bus.DesiredChannels() != 0 {
		t.Fatalf("注销后不应有登记通道，收到 %d", bus.DesiredChannels())
	}
}

func TestBusForceReconnectTriggersResyncAndKeepsDelivering(t *testing.T) {
	client := testRedisClient(t)
	bus := NewBus(client, nil)
	defer func() { _ = bus.Close() }()

	messages := make(chan string, 32)
	if _, err := bus.Subscribe(ChannelProvidersUpdated, func(message string) {
		messages <- message
	}); err != nil {
		t.Fatalf("订阅失败：%v", err)
	}

	waitForMessage(t, messages, func(message string) bool { return message == ResyncMessage },
		"首次订阅的 resync")

	// 模拟断线恢复：主动丢连接并重建，必须再次强制 resync（Pub/Sub 不补发断线窗口）。
	bus.ForceReconnect()
	waitForMessage(t, messages, func(message string) bool { return message == ResyncMessage },
		"重连后的强制 resync")

	bus.Publish(context.Background(), ChannelProvidersUpdated, "after-reconnect")
	waitForMessage(t, messages, func(message string) bool { return message == "after-reconnect" },
		"重连后仍能收到真实失效消息")
}

func TestRegistryInvalidatesAndReloadsWithinOneSecond(t *testing.T) {
	client := testRedisClient(t)
	bus := NewBus(client, nil)
	defer func() { _ = bus.Close() }()

	registry := NewRegistry(bus)
	defer registry.Close()

	cache := NewValueCache[string](time.Hour)
	var loads int32
	load := func(context.Context) (string, error) {
		return "v" + strconv.Itoa(int(atomic.AddInt32(&loads, 1))), nil
	}

	if _, err := cache.Get(context.Background(), load, nil); err != nil {
		t.Fatalf("首次加载失败：%v", err)
	}
	registry.MarkLoaded(DomainSystemSettings)

	dispose, err := registry.Bind(DomainSystemSettings, cache.Invalidate)
	if err != nil {
		t.Fatalf("绑定失败：%v", err)
	}
	defer dispose()

	// resync 会先到一次；等它落地后再测「真实失效 → 重载」的时延。
	deadline := time.Now().Add(testRedisDeadline)
	for registry.Invalidations(DomainSystemSettings) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if registry.Invalidations(DomainSystemSettings) == 0 {
		t.Fatal("首次订阅的 resync 未触发失效回调")
	}

	baseline := registry.Invalidations(DomainSystemSettings)
	startedAt := time.Now()
	bus.Publish(context.Background(), ChannelSystemSettingsUpdated, "")

	for registry.Invalidations(DomainSystemSettings) == baseline && time.Now().Before(startedAt.Add(testRedisDeadline)) {
		time.Sleep(5 * time.Millisecond)
	}
	if registry.Invalidations(DomainSystemSettings) == baseline {
		t.Fatalf("%v 内未收到失效消息", testRedisDeadline)
	}
	if elapsed := time.Since(startedAt); elapsed > time.Second {
		t.Fatalf("失效传播耗时 %.0fms，超过 1 秒上界", elapsed.Seconds()*1000)
	}

	value, err := cache.Get(context.Background(), load, nil)
	if err != nil {
		t.Fatalf("失效后加载失败：%v", err)
	}
	if value != "v2" {
		t.Fatalf("失效后必须重新加载，收到 %q（加载次数 %d）", value, atomic.LoadInt32(&loads))
	}
	if cache.Version() == 0 {
		t.Fatal("失效必须递增版本号")
	}
}

func TestRegistryResyncCoversMissedMessagesWhileDisconnected(t *testing.T) {
	client := testRedisClient(t)
	bus := NewBus(client, nil)
	defer func() { _ = bus.Close() }()

	registry := NewRegistry(bus)
	defer registry.Close()

	cache := NewValueCache[string](time.Hour)
	var loads int32
	load := func(context.Context) (string, error) {
		return strconv.Itoa(int(atomic.AddInt32(&loads, 1))), nil
	}
	if _, err := cache.Get(context.Background(), load, nil); err != nil {
		t.Fatalf("首次加载失败：%v", err)
	}

	if _, err := registry.Bind(DomainSensitiveWords, cache.Invalidate); err != nil {
		t.Fatalf("绑定失败：%v", err)
	}

	// 等首批 resync 落地，把计数归零到稳定点。
	deadline := time.Now().Add(testRedisDeadline)
	for registry.Invalidations(DomainSensitiveWords) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	before := registry.Invalidations(DomainSensitiveWords)
	// 断线窗口：这里发布的消息按定义收不到；恢复后必须靠 resync 补偿。
	bus.ForceReconnect()

	for registry.Invalidations(DomainSensitiveWords) == before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if registry.Invalidations(DomainSensitiveWords) == before {
		t.Fatal("重连后未强制 resync，断线窗口将失去补偿")
	}

	if _, err := cache.Get(context.Background(), load, nil); err != nil {
		t.Fatalf("resync 后加载失败：%v", err)
	}
	if atomic.LoadInt32(&loads) < 2 {
		t.Fatalf("resync 必须触发重载，实际加载 %d 次", loads)
	}
}
