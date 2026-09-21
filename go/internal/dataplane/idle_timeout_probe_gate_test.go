package dataplane

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住首字后停滞探测阈值的两条边界：**总闸约束**与**失效接线**。
//
// 为什么必须有：
//   - 总闸：该阈值原先是 `resolve` 里「列非 NULL 即生效」，不看 slow_rate_monitor_enabled；
//     于是渠道关掉低速监控后探测仍在跑（同仓 slowrate.SnapshotConfig.SlowRateConfig 是查总闸的，
//     两路语义分叉）。「列非 NULL 且总闸关」这个组合不会被任何单侧测试覆盖，只有跨到这一处才看得见。
//   - 失效：缓存原先只靠 TTL 自愈（providers 域 30s），而管理面改供应商会广播 DomainProviders；
//     不挂失效时「界面显示新值、数据面仍用旧值」可长达 30s。

// probeGateRowReader 是按 providerID 返回固定行的存储替身（计数用于钉住「查了几次库」）。
type probeGateRowReader struct {
	mu    sync.Mutex
	rows  map[int64]*store.Provider
	calls int
}

func newProbeGateRowReader() *probeGateRowReader {
	return &probeGateRowReader{rows: map[int64]*store.Provider{}}
}

func (r *probeGateRowReader) FindProviderByID(_ context.Context, id int64) (*store.Provider, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return r.rows[id], nil
}

func (r *probeGateRowReader) set(id int64, row *store.Provider) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows[id] = row
}

func (r *probeGateRowReader) queries() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func probeThresholdRef(seconds int) *int { return &seconds }

// probeGateRow 造一行：静默超时固定 5000ms，其余两列按参数。
func probeGateRow(monitor bool, threshold *int) *store.Provider {
	return &store.Provider{
		StreamingIdleTimeoutMS:             5000,
		SlowRateMonitorEnabled:             monitor,
		SlowRateProbeAfterFirstByteSeconds: threshold,
	}
}

// TestIdleTimeoutProbeRespectsSlowRateMasterGate 钉住「总闸关 ⇒ 不探测」。
//
// 判据走 probeAfterFirstByte（真正的消费缝），不是只测折行函数——否则「接线被摘掉」照样全绿。
func TestIdleTimeoutProbeRespectsSlowRateMasterGate(t *testing.T) {
	const threshold = 30
	cases := []struct {
		name       string
		monitor    bool
		threshold  *int
		wantProbe  int
		wantIdleMS int
	}{
		{"总闸开+阈值已配", true, probeThresholdRef(threshold), threshold, 5000},
		{"总闸开+阈值为NULL", true, nil, 0, 5000},
		{"总闸关+阈值已配", false, probeThresholdRef(threshold), 0, 5000},
		{"总闸关+阈值为NULL", false, nil, 0, 5000},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			reader := newProbeGateRowReader()
			reader.set(167, probeGateRow(testCase.monitor, testCase.threshold))
			cache := newIdleTimeoutCache(reader, nil, logx.New(nil))

			if got := cache.probeAfterFirstByte(167); got != testCase.wantProbe {
				t.Fatalf("探测阈值应为 %d，实得 %d（monitor=%v threshold=%v）",
					testCase.wantProbe, got, testCase.monitor, testCase.threshold)
			}
			// 静默超时不受总闸约束：它是另一件事，被顺手带上闸会改掉流式超时行为。
			if got := cache.lookup(167); got != time.Duration(testCase.wantIdleMS)*time.Millisecond {
				t.Fatalf("静默超时应恒为 %dms（不受总闸约束），实得 %v", testCase.wantIdleMS, got)
			}
		})
	}
}

// TestIdleTimeoutCacheHoldsThenRefreshesAfterClear 钉住缓存不是「钉死」。
//
// 两条断言合起来才有意义：清缓存**前**必须仍看到旧值（证明缓存真的在挡），清后必须看到新值
// （证明挡的只是 TTL 内的读）。只测后者会被「压根没缓存」的实现蒙混过关。
func TestIdleTimeoutCacheHoldsThenRefreshesAfterClear(t *testing.T) {
	reader := newProbeGateRowReader()
	reader.set(167, probeGateRow(true, probeThresholdRef(30)))
	cache := newIdleTimeoutCache(reader, nil, logx.New(nil))

	if got := cache.probeAfterFirstByte(167); got != 30 {
		t.Fatalf("首次应为 30，实得 %d", got)
	}

	// 渠道刚被改：阈值 12、总闸关掉。
	reader.set(167, probeGateRow(false, probeThresholdRef(12)))
	if got := cache.probeAfterFirstByte(167); got != 30 {
		t.Fatalf("清缓存前不得看到新值（没挡住说明根本没缓存），实得 %d", got)
	}

	cache.ttl.Clear()
	if got := cache.probeAfterFirstByte(167); got != 0 {
		t.Fatalf("清缓存后应重新查库并按新行（总闸关）返回 0，实得 %d", got)
	}
	if got := reader.queries(); got != 2 {
		t.Fatalf("两次装载各查一次库，共应为 2 次，实际 %d 次", got)
	}
}

// TestIdleTimeoutCacheClearedByProvidersDomainBroadcast 是**接线钉子**：把缓存挂到真 Registry +
// 真 Bus 上，发一条 providers 域失效，断言下一次读会重新查库。
//
// 为何必须单有一条：上面两条只钉住「缓存」与「那个失效函数」本身，钉不住「失效函数真的挂到了
// DomainProviders 上」——把 Bind 换成空函数时，前两条全绿。
//
// 门控：未设置 CCH_TEST_REDIS_URL 时跳过（与仓库其余 Redis 集成用例一致）。
func TestIdleTimeoutCacheClearedByProvidersDomainBroadcast(t *testing.T) {
	client := feedRedis(t)
	bus := cfgsync.NewBus(client, nil)
	defer func() { _ = bus.Close() }()
	registry := cfgsync.NewRegistry(bus)
	defer registry.Close()

	reader := newProbeGateRowReader()
	reader.set(167, probeGateRow(true, probeThresholdRef(30)))
	cache := newIdleTimeoutCache(reader, registry, logx.New(nil))

	if got := cache.probeAfterFirstByte(167); got != 30 {
		t.Fatalf("首次应为 30，实得 %d", got)
	}

	// 首次订阅会先派发一次 resync，等它落地再测「真实失效 → 清缓存」。
	deadline := time.Now().Add(5 * time.Second)
	for registry.Invalidations(cfgsync.DomainProviders) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if registry.Invalidations(cfgsync.DomainProviders) == 0 {
		t.Fatal("首次订阅的 resync 未触发 providers 域失效回调")
	}

	// 渠道刚被改：阈值 12、总闸关。
	reader.set(167, probeGateRow(false, probeThresholdRef(12)))
	baseline := registry.Invalidations(cfgsync.DomainProviders)
	bus.Publish(context.Background(), cfgsync.ChannelProvidersUpdated, "")
	for registry.Invalidations(cfgsync.DomainProviders) == baseline && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if registry.Invalidations(cfgsync.DomainProviders) == baseline {
		t.Fatal("未收到 providers 域失效消息")
	}

	if got := cache.probeAfterFirstByte(167); got != 0 {
		t.Fatalf("失效广播后必须重新查库并按新行返回 0，实得 %d", got)
	}
}
