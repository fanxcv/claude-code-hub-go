package dataplane

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/slowrate"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住低速监控配置读面的**开销**与**新鲜度**。
//
// 为什么必须有：该读面原先在每条终态上直查一次 `SELECT * FROM providers WHERE id=$1`，而生产上
// 绝大多数渠道没开监控——「未开启的渠道逐请求零额外开销」这条设计约束因此不成立，代价还是由
// terminal 的异步写 worker 承担的。判据只能落在「查了几次库」上，故需要一个能计数的存储替身。

// countingProviderReader 是按 providerID 计数的存储替身。
type countingProviderReader struct {
	mu    sync.Mutex
	rows  map[int64]*store.Provider
	calls int
}

func newCountingProviderReader() *countingProviderReader {
	return &countingProviderReader{rows: map[int64]*store.Provider{}}
}

// FindProviderByID 命中即返回该行；未登记即返回 (nil, nil)——与真实读面「查不到行」同形。
func (r *countingProviderReader) FindProviderByID(_ context.Context, id int64) (*store.Provider, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return r.rows[id], nil
}

func (r *countingProviderReader) set(id int64, row *store.Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows[id] = row
}

func (r *countingProviderReader) queries() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// TestDisabledSlowRateProviderDoesNotQueryPerCall 钉住「关闭监控的渠道不再每请求查库」。
//
// 判据：连续 N 次读取（模拟 N 条终态）后，存储只被问过**一次**（惰性装载，手法同
// idleTimeoutCache），不是 N 次。已删除/查不到行的 providerID 同理——只缓存正结果等于没修。
func TestDisabledSlowRateProviderDoesNotQueryPerCall(t *testing.T) {
	reader := newCountingProviderReader()
	reader.set(167, &store.Provider{SlowRateMonitorEnabled: false})
	config := slowrate.NewSnapshotConfig(newProviderSlowRateSource(reader, nil, logx.New(nil)))

	for i := 0; i < 5; i++ {
		if _, enabled := config.SlowRateConfig(context.Background(), 167); enabled {
			t.Fatalf("第 %d 次：关闭监控的渠道必须按未开启处理", i+1)
		}
	}
	if got := reader.queries(); got != 1 {
		t.Fatalf("5 条终态应只查一次库（首次装载），实际 %d 次——回到「每请求一次 SQL」即本缺陷复发", got)
	}

	// 行不存在的 providerID（已删除）：负结果也必须缓存，否则每次终态仍回查一次。
	for i := 0; i < 3; i++ {
		if _, enabled := config.SlowRateConfig(context.Background(), 999); enabled {
			t.Fatalf("第 %d 次：查不到行的渠道必须按未开启处理", i+1)
		}
	}
	if got := reader.queries(); got != 2 {
		t.Fatalf("行不存在的 3 次读取应只查一次库，实际共 %d 次", got)
	}
}

// TestSlowRateProviderCacheRefreshesAfterInvalidation 钉住缓存不是「钉死」。
//
// 两条断言合起来才有意义：失效**前**必须仍看到旧值（证明缓存真的在挡），失效**后**必须看到
// 新值（证明挡的只是 TTL 内的读，不是永久钉死）。只测后者会被「压根没缓存」的实现蒙混过关。
func TestSlowRateProviderCacheRefreshesAfterInvalidation(t *testing.T) {
	reader := newCountingProviderReader()
	reader.set(167, &store.Provider{SlowRateMonitorEnabled: false})
	source := newProviderSlowRateSource(reader, nil, logx.New(nil))
	config := slowrate.NewSnapshotConfig(source)

	if _, enabled := config.SlowRateConfig(context.Background(), 167); enabled {
		t.Fatal("首次：关闭监控的渠道必须按未开启处理")
	}

	// 渠道刚被打开：写库 + 广播 DomainProviders 都发生在下面这次读取之前。
	window, trigger := 1800, 3
	reader.set(167, &store.Provider{
		SlowRateMonitorEnabled: true,
		SlowRateWindowSeconds:  &window,
		SlowRateTriggerCount:   &trigger,
	})
	if _, enabled := config.SlowRateConfig(context.Background(), 167); enabled {
		t.Fatal("失效前不得看到新值：缓存没挡住说明根本没缓存")
	}

	// 失效回调就是 newProviderSlowRateSource 里挂到 DomainProviders 上的那个函数。
	source.cache.Clear()

	params, enabled := config.SlowRateConfig(context.Background(), 167)
	if !enabled {
		t.Fatal("失效后必须重新查库并看到新值")
	}
	if params.WindowSeconds != window || params.TriggerCount != trigger {
		t.Fatalf("失效后参数应为新值，收到 %+v", params)
	}
	if got := reader.queries(); got != 2 {
		t.Fatalf("两次装载各查一次，共应为 2 次，实际 %d 次", got)
	}
}

// TestSlowRateProviderCacheClearedByDomainBroadcast 是**接线钉子**：把缓存挂到真 Registry + 真
// Bus 上，发一条 providers 域失效，断言下一次读会重新查库。
//
// 为何必须单有一条：上面两条只钉住「缓存」与「那个失效函数」本身，钉不住「失效函数真的挂到了
// DomainProviders 上」——把 Bind 换成空函数时，前两条全绿。
//
// 门控：未设置 CCH_TEST_REDIS_URL 时跳过（与仓库其余 Redis 集成用例一致）。
func TestSlowRateProviderCacheClearedByDomainBroadcast(t *testing.T) {
	client := feedRedis(t)
	bus := cfgsync.NewBus(client, nil)
	defer func() { _ = bus.Close() }()
	registry := cfgsync.NewRegistry(bus)
	defer registry.Close()

	reader := newCountingProviderReader()
	reader.set(167, &store.Provider{SlowRateMonitorEnabled: false})
	config := slowrate.NewSnapshotConfig(newProviderSlowRateSource(reader, registry, logx.New(nil)))

	if _, enabled := config.SlowRateConfig(context.Background(), 167); enabled {
		t.Fatal("首次：关闭监控的渠道必须按未开启处理")
	}

	// 首次订阅会先派发一次 resync，等它落地再测「真实失效 → 清缓存」。
	deadline := time.Now().Add(5 * time.Second)
	for registry.Invalidations(cfgsync.DomainProviders) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if registry.Invalidations(cfgsync.DomainProviders) == 0 {
		t.Fatal("首次订阅的 resync 未触发 providers 域失效回调")
	}

	window, trigger := 1800, 3
	reader.set(167, &store.Provider{
		SlowRateMonitorEnabled: true,
		SlowRateWindowSeconds:  &window,
		SlowRateTriggerCount:   &trigger,
	})
	baseline := registry.Invalidations(cfgsync.DomainProviders)
	bus.Publish(context.Background(), cfgsync.ChannelProvidersUpdated, "")
	for registry.Invalidations(cfgsync.DomainProviders) == baseline && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if registry.Invalidations(cfgsync.DomainProviders) == baseline {
		t.Fatal("未收到 providers 域失效消息")
	}

	params, enabled := config.SlowRateConfig(context.Background(), 167)
	if !enabled || params.WindowSeconds != window || params.TriggerCount != trigger {
		t.Fatalf("失效广播后必须看到新值，收到 enabled=%v params=%+v", enabled, params)
	}
}
