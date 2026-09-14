package cfgsync

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTTLMapExpiryAndBump(t *testing.T) {
	now := time.Unix(0, 0)
	table := NewTTLMap[string, int](time.Second, 4)
	table.now = func() time.Time { return now }

	table.Set("a", 1)
	if value, ok := table.Get("a"); !ok || value != 1 {
		t.Fatalf("刚写入的键必须可读，收到 %v/%v", value, ok)
	}

	now = now.Add(time.Second)
	if _, ok := table.Get("a"); ok {
		t.Fatal("到期后必须未命中")
	}
	if table.Size() != 0 {
		t.Fatalf("到期读应删除条目，Size=%d", table.Size())
	}
}

func TestTTLMapEvictsOldestByInsertionOrder(t *testing.T) {
	now := time.Unix(0, 0)
	table := NewTTLMap[string, int](time.Hour, 10)
	table.now = func() time.Time { return now }

	for index := 0; index < 10; index++ {
		table.Set(string(rune('a'+index)), index)
		now = now.Add(time.Millisecond)
	}

	// 第 11 次写入触发淘汰：应淘汰最旧的约 10%（至少 1 条），即 "a"。
	table.Set("k", 99)
	if _, ok := table.Get("a"); ok {
		t.Fatal("容量满时应淘汰最旧条目 a")
	}
	if value, ok := table.Get("k"); !ok || value != 99 {
		t.Fatalf("新写入的键必须可读，收到 %v/%v", value, ok)
	}
}

func TestTTLMapPurgeExpired(t *testing.T) {
	now := time.Unix(0, 0)
	table := NewTTLMap[string, int](time.Second, 8)
	table.now = func() time.Time { return now }
	table.Set("a", 1)
	table.Set("b", 2)
	now = now.Add(2 * time.Second)

	table.PurgeExpired()
	if table.Size() != 0 {
		t.Fatalf("过期项应被清理，Size=%d", table.Size())
	}
}

func TestValueCacheMergesInFlightLoads(t *testing.T) {
	cache := NewValueCache[string](time.Minute)
	var loads int32
	release := make(chan struct{})

	load := func(context.Context) (string, error) {
		atomic.AddInt32(&loads, 1)
		<-release
		return "value", nil
	}

	const concurrency = 16
	var waitGroup sync.WaitGroup
	results := make([]string, concurrency)
	for index := 0; index < concurrency; index++ {
		waitGroup.Add(1)
		go func(slot int) {
			defer waitGroup.Done()
			value, err := cache.Get(context.Background(), load, nil)
			if err != nil {
				t.Errorf("加载失败：%v", err)
				return
			}
			results[slot] = value
		}(index)
	}

	// 等全部等待者进入在途共享，再放行加载。
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&loads) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	waitGroup.Wait()

	if got := atomic.LoadInt32(&loads); got != 1 {
		t.Fatalf("冷缓存下 N 并发必须只加载一次，实际 %d 次", got)
	}
	for index, value := range results {
		if value != "value" {
			t.Fatalf("第 %d 个等待者拿到 %q", index, value)
		}
	}
}

func TestValueCacheFallsBackToPreviousValueThenFallback(t *testing.T) {
	cache := NewValueCache[string](time.Nanosecond)
	succeed := true
	loadErr := errors.New("boom")
	load := func(context.Context) (string, error) {
		if succeed {
			return "fresh", nil
		}
		return "", loadErr
	}

	value, err := cache.Get(context.Background(), load, nil)
	if err != nil || value != "fresh" {
		t.Fatalf("首次加载应成功，收到 %q/%v", value, err)
	}

	// 让 TTL 过期，再让加载失败：必须复用旧值且不报错。
	time.Sleep(2 * time.Nanosecond)
	succeed = false
	value, err = cache.Get(context.Background(), load, nil)
	if err != nil || value != "fresh" {
		t.Fatalf("加载失败时应复用旧值，收到 %q/%v", value, err)
	}

	// 清掉旧值后，失败应降级到 fallback。
	cache.Invalidate()
	value, err = cache.Get(context.Background(), load, func() string { return "default" })
	if err != nil || value != "default" {
		t.Fatalf("无旧值时应降级到 fallback，收到 %q/%v", value, err)
	}
}

func TestValueCacheVersionGuardDropsStaleLoad(t *testing.T) {
	cache := NewValueCache[string](time.Minute)
	started := make(chan struct{})
	release := make(chan struct{})

	load := func(context.Context) (string, error) {
		close(started)
		<-release
		return "stale", nil
	}

	done := make(chan string, 1)
	go func() {
		value, err := cache.Get(context.Background(), load, nil)
		if err != nil {
			done <- "err"
			return
		}
		done <- value
	}()

	<-started
	cache.Invalidate()
	close(release)

	if value := <-done; value != "stale" {
		t.Fatalf("发起者仍应拿到自己那次加载的结果，收到 %q", value)
	}
	if _, ok := cache.Peek(); ok {
		t.Fatal("失效期间完成加载的旧结果不得写回缓存")
	}
	if cache.Version() != 1 {
		t.Fatalf("失效应递增版本号，收到 %d", cache.Version())
	}
}

func TestValueCacheEventDrivenNeverExpiresByTime(t *testing.T) {
	cache := NewValueCache[string](0, WithValueCacheNoExpiry[string]())
	loads := 0
	load := func(context.Context) (string, error) {
		loads++
		return "v", nil
	}

	if _, err := cache.Get(context.Background(), load, nil); err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if _, err := cache.Get(context.Background(), load, nil); err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if loads != 1 {
		t.Fatalf("无 TTL 的域不应按时间重载，实际加载 %d 次", loads)
	}

	cache.Invalidate()
	if _, err := cache.Get(context.Background(), load, nil); err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if loads != 2 {
		t.Fatalf("失效后必须重载，实际加载 %d 次", loads)
	}
}

func TestKeyedCacheSharesInFlightPerKey(t *testing.T) {
	cache := NewKeyedCache[int](time.Minute, 16)
	release := make(chan struct{})
	var loads int32
	fetch := func(context.Context) ([]int, error) {
		atomic.AddInt32(&loads, 1)
		<-release
		return []int{1, 2}, nil
	}

	for index := 0; index < 8; index++ {
		go func() { _, _ = cache.Get(context.Background(), "1:codex", fetch) }()
	}
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&loads) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond)
	close(release)

	deadline = time.Now().Add(2 * time.Second)
	for cache.Size() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := atomic.LoadInt32(&loads); got != 1 {
		t.Fatalf("同键并发必须只查询一次，实际 %d 次", got)
	}

	// 不同键各查一次。
	release2 := make(chan struct{})
	close(release2)
	if _, err := cache.Get(context.Background(), "2:claude", func(context.Context) ([]int, error) {
		return []int{3}, nil
	}); err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if cache.Size() != 2 {
		t.Fatalf("两个键应各有缓存条目，Size=%d", cache.Size())
	}
}

func TestKeyedCacheInvalidateDropsInFlight(t *testing.T) {
	cache := NewKeyedCache[int](time.Minute, 16)
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)
		_, _ = cache.Get(context.Background(), "k", func(context.Context) ([]int, error) {
			close(started)
			<-release
			return []int{1}, nil
		})
	}()

	<-started
	cache.Invalidate()
	close(release)
	<-done

	if cache.Size() != 0 {
		t.Fatalf("失效期间完成的结果不得写回，Size=%d", cache.Size())
	}
	if cache.Version() != 1 {
		t.Fatalf("失效应递增版本号，收到 %d", cache.Version())
	}
}

func TestRegistryTracksLoadAndInvalidation(t *testing.T) {
	registry := NewRegistry(nil)
	invalidated := 0
	dispose, err := registry.Bind(DomainSystemSettings, func() { invalidated++ })
	if err != nil {
		t.Fatalf("绑定失败：%v", err)
	}
	defer dispose()

	registry.MarkLoaded(DomainSystemSettings)
	if !registry.Loaded(DomainSystemSettings) {
		t.Fatal("MarkLoaded 后必须报告已装载")
	}

	dispose()
	if registry.Invalidations(DomainSystemSettings) != 0 {
		t.Fatal("未收到失效消息时计数应为 0")
	}
	if invalidated != 0 {
		t.Fatal("未收到失效消息时回调不应被调用")
	}
}

func TestRegistryReadyRequiresEventDrivenDomains(t *testing.T) {
	registry := NewRegistry(nil)
	if registry.Ready() {
		t.Fatal("事件驱动域未装载时不得报告就绪")
	}
	for _, domain := range AllDomains() {
		if Spec(domain).EventDriven {
			registry.MarkLoaded(domain)
		}
	}
	if !registry.Ready() {
		t.Fatal("全部事件驱动域装载后必须报告就绪")
	}
	if registry.LoadedDomains() != 4 {
		t.Fatalf("事件驱动域应为 4 个，收到 %d", registry.LoadedDomains())
	}
}

func TestSpecMatchesNodeChannels(t *testing.T) {
	cases := map[Domain]string{
		DomainSystemSettings:    ChannelSystemSettingsUpdated,
		DomainProviders:         ChannelProvidersUpdated,
		DomainProviderEndpoints: ChannelProvidersUpdated,
		DomainProviderGroups:    ChannelProviderGroupsUpdated,
		DomainAPIKeys:           ChannelAPIKeysUpdated,
		DomainRequestFilters:    ChannelRequestFiltersUpdated,
		DomainSensitiveWords:    ChannelSensitiveWordsUpdated,
		DomainErrorRules:        ChannelErrorRulesUpdated,
	}
	for domain, channel := range cases {
		if got := Spec(domain).Channel; got != channel {
			t.Fatalf("%s 的通道应为 %s，收到 %s", domain, channel, got)
		}
	}
	if SubscriberBackoffForTest(0) != 0 {
		t.Fatal("0 次失败不应退避")
	}
	if SubscriberBackoffForTest(1) != time.Second {
		t.Fatal("首次失败的退避应为 1 秒")
	}
	if SubscriberBackoffForTest(20) != subscriberConnectBackoffMax {
		t.Fatal("退避上限应为 60 秒")
	}
}

// SubscriberBackoffForTest 暴露退避计算供测试断言（生产不应依赖）。
func SubscriberBackoffForTest(failures int) time.Duration {
	return subscriberBackoff(failures)
}
