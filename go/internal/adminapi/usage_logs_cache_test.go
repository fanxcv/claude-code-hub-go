package adminapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件的用例补两处**今日新增进程内缓存**的失效路径覆盖（静态分析 B4）：
// 统计缓存（statsCacheTTL / statsCacheMaxEntries）与 ledger-only 口径缓存（ledgerOnlyCacheTTL）。
//
// 两处各自的 reset*ForTest 钩子此前零调用——钩子存在却没人用，等于这段时间里
// 「换筛选指纹会不会串数据」「TTL 过期会不会重取」这两类不变量一条都没被钉住。

func newStatsCacheTestModule(now func() time.Time) *usageLogsModule {
	module := &usageLogsModule{now: now}
	module.resetStatsCacheForTest()
	return module
}

func summaryWithRequests(total int64) store.UsageLogSummary {
	return store.UsageLogSummary{TotalRequests: total, TotalCost: 1.5, TotalTokens: 42}
}

// TestStatsCacheIsolatesFilterFingerprints 钉住「指纹不同 ⇒ 不串数据」。
//
// 缓存键由全部筛选字段 + ledgerOnly 口径拼成（见 statsFingerprint）。若某一维漏进键里，
// 两个不同筛选组合会互相覆盖，表现为「换个筛选条件看到的还是上次的数字」。
func TestStatsCacheIsolatesFilterFingerprints(t *testing.T) {
	fixed := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	module := newStatsCacheTestModule(func() time.Time { return fixed })

	userA, userB := int64(11), int64(22)
	base := store.UsageLogFilters{UserID: &userA}
	module.storeCachedStats(base, false, summaryWithRequests(100))

	if got, ok := module.loadCachedStats(base, false); !ok || got.TotalRequests != 100 {
		t.Fatalf("同一筛选应命中：ok=%v total=%d", ok, got.TotalRequests)
	}

	cases := []struct {
		name       string
		filters    store.UsageLogFilters
		ledgerOnly bool
	}{
		{"换用户", store.UsageLogFilters{UserID: &userB}, false},
		{"换模型", store.UsageLogFilters{UserID: &userA, Model: "claude-sonnet-4-5"}, false},
		{"换端点", store.UsageLogFilters{UserID: &userA, Endpoint: "/v1/messages"}, false},
		{"换模型不匹配开关", store.UsageLogFilters{UserID: &userA, ActualResponseModelMismatch: true}, false},
		{"换最小重试数", store.UsageLogFilters{UserID: &userA, MinRetryCount: 1}, false},
		{"换状态码", store.UsageLogFilters{UserID: &userA, StatusCode: intPtr(429)}, false},
		{"换 ledgerOnly 口径", store.UsageLogFilters{UserID: &userA}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := module.loadCachedStats(tc.filters, tc.ledgerOnly); ok {
				t.Fatal("不同指纹不应命中同一缓存条目（会串数据）")
			}
		})
	}
}

// TestStatsCacheExpiresExactlyAtTTL 钉住过期边界：TTL 内命中、到期即失效。
//
// 统计是「同一窗口内的重复读取」才该复用，陈旧上限就是 statsCacheTTL；
// 边界写错（例如用 <= 比）会让窗口多活一个 TTL。
func TestStatsCacheExpiresExactlyAtTTL(t *testing.T) {
	base := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	current := base
	module := newStatsCacheTestModule(func() time.Time { return current })

	filters := store.UsageLogFilters{Model: "claude-sonnet-4-5"}
	module.storeCachedStats(filters, false, summaryWithRequests(7))

	current = base.Add(statsCacheTTL - time.Second)
	if got, ok := module.loadCachedStats(filters, false); !ok || got.TotalRequests != 7 {
		t.Fatalf("TTL 内应命中：ok=%v total=%d", ok, got.TotalRequests)
	}

	current = base.Add(statsCacheTTL)
	if _, ok := module.loadCachedStats(filters, false); ok {
		t.Fatal("到期即应失效（边界处不得再命中）")
	}
}

// TestStatsCacheIsBoundedAndEvictsEarliestExpiry 钉住上界：筛选组合用户可控，缓存不得无限增长，
// 超界丢**最早过期**的一条。
func TestStatsCacheIsBoundedAndEvictsEarliestExpiry(t *testing.T) {
	base := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	current := base
	module := newStatsCacheTestModule(func() time.Time { return current })

	// 逐条写入并推进时钟，使各条过期时刻互不相同（否则淘汰顺序不确定）。
	for i := 0; i < statsCacheMaxEntries; i++ {
		userID := int64(i)
		module.storeCachedStats(store.UsageLogFilters{UserID: &userID}, false, summaryWithRequests(int64(i)))
		current = current.Add(time.Second)
	}
	if got := len(module.statsEntries); got != statsCacheMaxEntries {
		t.Fatalf("应恰好填满上界：得到 %d 条", got)
	}

	firstUserID := int64(0) // 最早写入 ⇒ 最早过期 ⇒ 应被淘汰
	newUserID := int64(9999)
	module.storeCachedStats(store.UsageLogFilters{UserID: &newUserID}, false, summaryWithRequests(9999))

	if got := len(module.statsEntries); got != statsCacheMaxEntries {
		t.Fatalf("超界后应仍为上界条数：得到 %d 条", got)
	}
	if _, ok := module.loadCachedStats(store.UsageLogFilters{UserID: &firstUserID}, false); ok {
		t.Fatal("最早过期的一条应被淘汰")
	}
	if got, ok := module.loadCachedStats(store.UsageLogFilters{UserID: &newUserID}, false); !ok || got.TotalRequests != 9999 {
		t.Fatalf("新写入的一条应命中：ok=%v total=%d", ok, got.TotalRequests)
	}
}

// TestLedgerOnlyCacheKeepsLastValueOnProbeFailure 钉住粘滞语义：
// 探测失败（含未装配连接池）时**沿用上次结果**，而不是把口径翻转成 false。
//
// 这条对账务口径很要紧：一次探测抖动不该让统计与列表在同一个页面上切成两套口径。
func TestLedgerOnlyCacheKeepsLastValueOnProbeFailure(t *testing.T) {
	fixed := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	cache := &ledgerOnlyCache{
		now:     func() time.Time { return fixed },
		value:   ulBoolPtr(true),
		expires: fixed.Add(-time.Second), // 已过期 ⇒ 必须走一次重探测
	}
	defer cache.resetLedgerOnlyCacheForTest()

	if got := cache.ledgerOnly(httptest.NewRequest(http.MethodGet, "/api/v1/usage-logs", nil)); !got {
		t.Fatal("探测失败时应沿用上次的 true，不得翻转成 false")
	}
	if want := fixed.Add(ledgerOnlyCacheTTL); !cache.expires.Equal(want) {
		t.Fatalf("重探测后应刷新过期时刻：得到 %v，期望 %v", cache.expires, want)
	}
}

// TestLedgerOnlyCacheFirstProbeFailureIsFalse 钉住「首次探测失败则为 false」这一半语义
// （与上一条合起来才是完整的 ledger-fallback 行为）。
func TestLedgerOnlyCacheFirstProbeFailureIsFalse(t *testing.T) {
	fixed := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	cache := &ledgerOnlyCache{now: func() time.Time { return fixed }}
	defer cache.resetLedgerOnlyCacheForTest()

	if got := cache.ledgerOnly(httptest.NewRequest(http.MethodGet, "/api/v1/usage-logs", nil)); got {
		t.Fatal("无上次结果时探测失败应为 false")
	}
	// 结果已落缓存：TTL 内再问一次不再重探测。
	before := cache.expires
	if got := cache.ledgerOnly(httptest.NewRequest(http.MethodGet, "/api/v1/usage-logs", nil)); got {
		t.Fatal("缓存期内应返回已记住的 false")
	}
	if !cache.expires.Equal(before) {
		t.Fatal("缓存期内不应重探测（过期时刻不该被推后）")
	}
}

func ulBoolPtr(v bool) *bool { return &v }
