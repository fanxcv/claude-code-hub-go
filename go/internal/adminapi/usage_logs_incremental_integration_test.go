package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是**增量拉取**（sinceId+asc）与**统计短 TTL 缓存**的真实 PG 集成用例：
// 门控 CCH_TEST_DSN，夹具自建自清、以唯一模型名钉住候选，不与并发集成测试互扰
// （复用 usage_logs_integration_test.go 的 ulOpenPools / ulRouter / ulDoRequest 等夹具）。

// seedIncrementalFixture 插 n 行同模型夹具，created_at **随后插入而变新**（真实形态），
// 返回按 id 升序的 id 列表。
//
// 这种形态让「按 id 升序」与「按 created_at 倒序」的结果**不同**，所以断言能真正分辨排序
// （反证时把升序改回 created_at DESC，用例会红）。
func seedIncrementalFixture(t *testing.T, pools *store.Pools, n int) (string, []int64) {
	t.Helper()
	return seedUsageLogRows(t, pools, n, func(index int) int { return n - index })
}

// seedBackdatedFixture 插 n 行，其中**末行被回填成比首行更早的时间**：
// 它是「后插入但时间更早」的行，正是增量水位必须用 id 而不是 created_at 的理由
// （若按 created_at 做水位，这行会被永久漏掉）。
func seedBackdatedFixture(t *testing.T, pools *store.Pools, n int) (string, []int64) {
	t.Helper()
	return seedUsageLogRows(t, pools, n, func(index int) int {
		if index == n-1 {
			return n * 10 // 末行回填到更早
		}
		return n - index
	})
}

// seedUsageLogRows 是上面两个夹具的共同实现；hoursAgo 给出第 index 行的「几小时前」。
func seedUsageLogRows(
	t *testing.T,
	pools *store.Pools,
	n int,
	hoursAgo func(index int) int,
) (string, []int64) {
	t.Helper()
	ctx := context.Background()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取 control 分道失败: %v", err)
	}
	model := fmt.Sprintf("go-usage-incremental-it-%d", time.Now().UnixNano())
	sessionID := fmt.Sprintf("go-usage-incremental-session-%d", time.Now().UnixNano())
	ids := make([]int64, 0, n)
	for index := 0; index < n; index++ {
		var requestID int64
		// 注意 $6 与 $7 是两个参数：同一个站位符同时当整数列与 interval 乘数会让 PG
		// 报 "inconsistent types deduced for parameter"。
		err := pool.QueryRow(ctx, `
			INSERT INTO message_request (
				provider_id, user_id, key, model, original_model, actual_response_model, endpoint,
				status_code, input_tokens, output_tokens, cache_read_input_tokens,
				cache_creation_input_tokens, cost_usd, duration_ms, ttfb_ms,
				session_id, session_identity, session_identity_kind, request_sequence,
				is_replay, messages_count, special_settings, created_at
			) VALUES (
				$1, $2, $3, $4, $4, $4, '/v1/messages',
				200, 10, 1, 0,
				0, 0.100000000000000::numeric, 100, 5,
				$5, $5, 'session_id', $6,
				false, 1, '[]'::jsonb,
				now() - ($7 * interval '1 hour')
			) RETURNING id`,
			int64(1), fixtureUserID, fixtureKeyValue, model,
			sessionID, index+1, hoursAgo(index),
		).Scan(&requestID)
		if err != nil {
			t.Fatalf("插入增量夹具失败（index=%d）: %v", index, err)
		}
		ids = append(ids, requestID)
	}
	t.Cleanup(func() {
		cleanupPool, err := pools.Control()
		if err != nil {
			t.Logf("清理增量夹具失败（取池）: %v", err)
			return
		}
		if _, err := cleanupPool.Exec(ctx, `DELETE FROM usage_ledger WHERE request_id = ANY($1)`, ids); err != nil {
			t.Logf("清理增量夹具账本行失败: %v", err)
		}
		if _, err := cleanupPool.Exec(ctx, `DELETE FROM message_request WHERE id = ANY($1)`, ids); err != nil {
			t.Logf("清理增量夹具失败: %v", err)
		}
	})
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return model, ids
}

// TestIntegrationUsageLogsIncrementalReturnsExactlyNewRowsAscending 是契约 a 的主体：
// 造 3 行 → sinceId=第 1 行 → **恰好**后 2 行且升序。
func TestIntegrationUsageLogsIncrementalReturnsExactlyNewRowsAscending(t *testing.T) {
	pools := ulOpenPools(t)
	model, ids := seedIncrementalFixture(t, pools, 3)
	router := ulRouter(t, pools)

	status, body := ulDoRequest(t, router, fmt.Sprintf(
		"/api/v1/usage-logs?sinceId=%d&asc=true&model=%s", ids[0], model))
	if status != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d: %+v", status, body)
	}
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items 应为数组: %+v", body)
	}
	if len(items) != 2 {
		t.Fatalf("sinceId=第1行应恰好返回后 2 行，实际 %d 行: %+v", len(items), items)
	}
	got := make([]int64, 0, len(items))
	for _, raw := range items {
		row := raw.(map[string]any)
		got = append(got, int64(row["id"].(float64)))
	}
	if got[0] != ids[1] || got[1] != ids[2] {
		t.Fatalf("应恰好返回 id %v 且升序，实际 %v", ids[1:], got)
	}
	if !sort.SliceIsSorted(got, func(i, j int) bool { return got[i] < got[j] }) {
		t.Fatalf("行必须按 id 升序: %v", got)
	}
	// 增量读不返回游标（继续位是调用方已看到的 max id）。
	if cursor := body["pageInfo"].(map[string]any)["nextCursor"]; cursor != nil {
		t.Fatalf("增量读的 nextCursor 应为 null，实际 %v", cursor)
	}
}

// TestIntegrationUsageLogsIncrementalBeyondMaxReturnsEmptyArray 是契约 a 的第二条：
// sinceId 超过最大 id 时返回**空数组**（而不是 null）。
func TestIntegrationUsageLogsIncrementalBeyondMaxReturnsEmptyArray(t *testing.T) {
	pools := ulOpenPools(t)
	model, ids := seedIncrementalFixture(t, pools, 2)
	router := ulRouter(t, pools)

	status, body := ulDoRequest(t, router, fmt.Sprintf(
		"/api/v1/usage-logs?sinceId=%d&asc=true&model=%s", ids[len(ids)-1]+1_000_000, model))
	if status != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d: %+v", status, body)
	}
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items 必须是数组（不能是 null）: %+v", body)
	}
	if len(items) != 0 {
		t.Fatalf("水位在最大 id 之后应返回 0 行，实际 %d", len(items))
	}
	if pageInfo := body["pageInfo"].(map[string]any); pageInfo["hasMore"] != false {
		t.Fatalf("无新行时 hasMore 应为 false: %+v", pageInfo)
	}
}

// TestIntegrationUsageLogsIncrementalWatermarkIsIDNotCreatedAt：增量水位必须是 **id** 而不是
// created_at——末行是「后插入但时间更早」的行，按 created_at 做水位会永久漏掉它。
func TestIntegrationUsageLogsIncrementalWatermarkIsIDNotCreatedAt(t *testing.T) {
	pools := ulOpenPools(t)
	model, ids := seedBackdatedFixture(t, pools, 3)
	router := ulRouter(t, pools)

	status, body := ulDoRequest(t, router, fmt.Sprintf(
		"/api/v1/usage-logs?sinceId=%d&asc=true&model=%s", ids[0], model))
	if status != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d: %+v", status, body)
	}
	items := body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("应返回后 2 行（含被回填到更早时间的那行），实际 %d: %+v", len(items), items)
	}
	got := make([]int64, 0, len(items))
	for _, raw := range items {
		got = append(got, int64(raw.(map[string]any)["id"].(float64)))
	}
	if got[0] != ids[1] || got[1] != ids[2] {
		t.Fatalf("被回填的行（id=%d）不得因时间更早而被漏掉，实际返回 %v", ids[2], got)
	}
}

// TestIntegrationUsageLogsIncrementalHasMoreWhenTruncated：新行多于 limit 时 hasMore 为真，
// 且只返回 limit 行（前端据此继续用返回里的 max id 再拉）。
func TestIntegrationUsageLogsIncrementalHasMoreWhenTruncated(t *testing.T) {
	pools := ulOpenPools(t)
	model, ids := seedIncrementalFixture(t, pools, 3)
	router := ulRouter(t, pools)

	status, body := ulDoRequest(t, router, fmt.Sprintf(
		"/api/v1/usage-logs?sinceId=%d&asc=true&limit=2&model=%s", ids[0]-1, model))
	if status != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d: %+v", status, body)
	}
	items := body["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("limit=2 应返回 2 行，实际 %d", len(items))
	}
	if body["pageInfo"].(map[string]any)["hasMore"] != true {
		t.Fatalf("还有新行时 hasMore 应为 true: %+v", body["pageInfo"])
	}
}

// TestIntegrationUsageLogsWithoutSinceIdKeepsDescendingShape 是契约 a 的第三条：
// **不传 sinceId 时行为与响应形状完全不变**（降序 keyset 语义，仍带游标与 limit）。
func TestIntegrationUsageLogsWithoutSinceIdKeepsDescendingShape(t *testing.T) {
	pools := ulOpenPools(t)
	model, ids := seedIncrementalFixture(t, pools, 3)
	router := ulRouter(t, pools)

	status, body := ulDoRequest(t, router, "/api/v1/usage-logs?limit=10&model="+model)
	if status != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d: %+v", status, body)
	}
	items := body["items"].([]any)
	if len(items) != 3 {
		t.Fatalf("应返回 3 行，实际 %d", len(items))
	}
	// created_at 随 id 递增，所以「按 created_at 倒序」的结果是 **id 降序**。这条断言因此有
	// 区分力：若非增量分支被改成 `ORDER BY id ASC`（或反之），本条会红。
	got := make([]int64, 0, len(items))
	for _, raw := range items {
		got = append(got, int64(raw.(map[string]any)["id"].(float64)))
	}
	for index := range ids {
		want := ids[len(ids)-1-index]
		if got[index] != want {
			t.Fatalf("降序分支应按 created_at 倒序（= 本夹具的 id 降序），实际 %v（期望 %v）", got, ids)
		}
	}
	pageInfo := body["pageInfo"].(map[string]any)
	if pageInfo["limit"] != float64(10) {
		t.Fatalf("pageInfo.limit 应为 10: %+v", pageInfo)
	}
	if pageInfo["nextCursor"] == nil {
		t.Fatalf("非增量读仍应返回游标: %+v", pageInfo)
	}
}

// TestIntegrationUsageLogsIncrementalRejectsHalfSpecified 校验跨字段约束（见 incrementalIssues）：
// 只给一个、或与偏移分页混用，都必须是 400（fail-closed，不自创语义）。
func TestIntegrationUsageLogsIncrementalRejectsHalfSpecified(t *testing.T) {
	pools := ulOpenPools(t)
	router := ulRouter(t, pools)

	cases := []struct {
		name     string
		target   string
		wantPath string
	}{
		{"只给 sinceId", "/api/v1/usage-logs?sinceId=5", "asc"},
		{"只给 asc", "/api/v1/usage-logs?asc=true", "sinceId"},
		{"asc=false 配 sinceId", "/api/v1/usage-logs?sinceId=5&asc=false", "asc"},
		{"增量与偏移分页混用", "/api/v1/usage-logs?sinceId=5&asc=true&page=1", "sinceId"},
		{"sinceId 为负", "/api/v1/usage-logs?sinceId=-1&asc=true", "sinceId"},
		{"sinceId 非数", "/api/v1/usage-logs?sinceId=abc&asc=true", "sinceId"},
		{"asc 非法值", "/api/v1/usage-logs?sinceId=5&asc=1", "asc"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			status, body := ulDoRequest(t, router, testCase.target)
			if status != http.StatusBadRequest {
				t.Fatalf("应为 400，实际 %d: %+v", status, body)
			}
			if body["errorCode"] != "request.validation_failed" {
				t.Fatalf("errorCode 不符: %+v", body["errorCode"])
			}
			issues, _ := body["invalidParams"].([]any)
			if len(issues) == 0 {
				t.Fatalf("应有 invalidParams: %+v", body)
			}
			path, _ := issues[0].(map[string]any)["path"].([]any)
			if len(path) != 1 || path[0] != testCase.wantPath {
				t.Fatalf("invalidParams[].path 应为 [%s]，实际 %+v", testCase.wantPath, path)
			}
		})
	}
}

// TestStatsCacheServesRepeatedReadsWithinTTL 是 E1 的行为断言：同一筛选组合在 TTL 内重复读
// **不再打到库里**（用「删掉夹具后仍返回旧值」证明真的走了缓存），TTL 外则重新查。
func TestStatsCacheServesRepeatedReadsWithinTTL(t *testing.T) {
	pools := ulOpenPools(t)
	model, ids := seedIncrementalFixture(t, pools, 2)
	router := ulRouter(t, pools)
	target := "/api/v1/usage-logs/stats?model=" + model

	status, first := ulDoRequest(t, router, target)
	if status != http.StatusOK {
		t.Fatalf("首次统计应为 200，实际 %d: %+v", status, first)
	}
	if first["totalRequests"] != float64(2) {
		t.Fatalf("夹具应为 2 行，实际 %v", first["totalRequests"])
	}

	// 把夹具删掉（绕过 t.Cleanup 的删除顺序，这里只删账本行即可让统计归零）。
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取池失败: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM usage_ledger WHERE request_id = ANY($1)`, ids); err != nil {
		t.Fatalf("删除账本夹具失败: %v", err)
	}

	status, cached := ulDoRequest(t, router, target)
	if status != http.StatusOK {
		t.Fatalf("命中缓存时应为 200，实际 %d: %+v", status, cached)
	}
	if cached["totalRequests"] != float64(2) {
		t.Fatalf("TTL 内应命中缓存并返回旧值 2，实际 %v（说明缓存没生效）", cached["totalRequests"])
	}
}

// TestStatsCacheExpiresAfterTTL：TTL 之后必须重新查库（陈旧上限就是 TTL，不能更长）。
func TestStatsCacheExpiresAfterTTL(t *testing.T) {
	pools := ulOpenPools(t)
	model, ids := seedIncrementalFixture(t, pools, 2)
	deps := Deps{Guard: ulStubGuard{}, Problems: NewProblems(nil), Store: pools}
	clock := time.Now()
	module := newStatsModule(deps, pools, func() time.Time { return clock })
	target := "/api/v1/usage-logs/stats?model=" + model

	status, first := doStatsRequest(t, module, target)
	if status != http.StatusOK || first["totalRequests"] != float64(2) {
		t.Fatalf("首次统计应为 2 行，实际 %d %+v", status, first)
	}
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取池失败: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM usage_ledger WHERE request_id = ANY($1)`, ids); err != nil {
		t.Fatalf("删除账本夹具失败: %v", err)
	}
	clock = clock.Add(statsCacheTTL + time.Second)
	status, after := doStatsRequest(t, module, target)
	if status != http.StatusOK {
		t.Fatalf("过期后应为 200，实际 %d: %+v", status, after)
	}
	if after["totalRequests"] != float64(0) {
		t.Fatalf("TTL 之后必须重新查库（应为 0），实际 %v", after["totalRequests"])
	}
}

// TestStatsCacheKeySeparatesFilterCombinations：不同筛选组合不得互相命中。
func TestStatsCacheKeySeparatesFilterCombinations(t *testing.T) {
	a := store.UsageLogFilters{Model: "m1"}
	b := store.UsageLogFilters{Model: "m2"}
	if statsFingerprint(a, false) == statsFingerprint(b, false) {
		t.Fatal("不同 model 的指纹不应相同")
	}
	// 含分隔符的值不能被压成同一个键。
	if statsFingerprint(store.UsageLogFilters{Model: "m|1"}, false) ==
		statsFingerprint(store.UsageLogFilters{Model: "m", Endpoint: "1"}, false) {
		t.Fatal("含分隔符的字段值不得与另一组合撞键")
	}
	// ledgerOnly 是口径的一部分，必须参与键。
	if statsFingerprint(a, false) == statsFingerprint(a, true) {
		t.Fatal("ledgerOnly 不同不应命中同一条目")
	}
}

// TestStatsCacheIsBounded：筛选组合是用户可控的，条目数必须有上界。
func TestStatsCacheIsBounded(t *testing.T) {
	deps := Deps{Guard: ulStubGuard{}, Problems: NewProblems(nil), Store: &store.Pools{}}
	module := newStatsModule(deps, nil, time.Now)
	for index := 0; index < statsCacheMaxEntries*2; index++ {
		module.storeCachedStats(store.UsageLogFilters{
			Model: fmt.Sprintf("model-%d", index),
		}, false, store.UsageLogSummary{TotalRequests: int64(index)})
	}
	module.statsMu.Lock()
	size := len(module.statsEntries)
	module.statsMu.Unlock()
	if size > statsCacheMaxEntries {
		t.Fatalf("缓存条目数不得超过 %d，实际 %d", statsCacheMaxEntries, size)
	}
}

// newStatsModule 造一个只服务于本用例的模块实例（与 RegisterUsageLogsWith 内一致：
// now 是时钟测试缝，ledgerOnly 缓存需要 pools）。它不经路由，直接打 handler，
// 好把「TTL 过期」用可控时钟推过去，不必等真实 20s。
func newStatsModule(deps Deps, pools *store.Pools, now func() time.Time) *usageLogsModule {
	module := &usageLogsModule{deps: deps, now: now}
	module.ledgerOnly = ledgerOnlyCache{pools: pools, now: now}
	return module
}

// doStatsRequest 直接打 handleStats（绕过路由与守卫，专注缓存行为）。
func doStatsRequest(t *testing.T, module *usageLogsModule, target string) (int, map[string]any) {
	t.Helper()
	recorder := httptest.NewRecorder()
	module.handleStats(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON（状态 %d）: %s", recorder.Code, recorder.Body.String())
	}
	return recorder.Code, body
}

// TestStatsCacheReportsTimingDelta 给出改前/改后的耗时对照（原始数据进报告）。
//
// **用无筛选的默认视图**（使用记录页刚打开时的形态）：带 model 筛选时只命中几行，查询本身
// 只要 ~120µs，量不出差异；生产那个 735–925ms 就是无筛选（对 usage_ledger 全表聚合）的情形。
func TestStatsCacheReportsTimingDelta(t *testing.T) {
	pools := ulOpenPools(t)
	_, _ = seedIncrementalFixture(t, pools, 2)
	deps := Deps{Guard: ulStubGuard{}, Problems: NewProblems(nil), Store: pools}
	target := "/api/v1/usage-logs/stats"

	// 改前等价物之一：直接打 store（纯 SQL，无缓存无信封）。
	storeOnly := make([]time.Duration, 0, 10)
	for index := 0; index < 10; index++ {
		started := time.Now()
		if _, err := pools.FindUsageLogsStats(context.Background(), store.UsageLogFilters{}, false); err != nil {
			t.Fatalf("直读统计失败: %v", err)
		}
		storeOnly = append(storeOnly, time.Since(started))
	}
	// 改前等价物之二：handler + **空缓存**（每次新建模块 = 无缓存）。
	uncached := make([]time.Duration, 0, 10)
	for index := 0; index < 10; index++ {
		module := newStatsModule(deps, pools, time.Now)
		uncached = append(uncached, measureStatsRequest(t, module, target))
	}
	// 改后：复用同一模块 ⇒ 第二次起命中缓存。
	shared := newStatsModule(deps, pools, time.Now)
	cached := make([]time.Duration, 0, 10)
	for index := 0; index < 10; index++ {
		cached = append(cached, measureStatsRequest(t, shared, target))
	}
	t.Logf("统计耗时（真实 PG，无筛选）: 纯 store 查询 p50=%v p95=%v；handler 未命中 p50=%v p95=%v；handler 命中缓存 p50=%v p95=%v",
		percentile(storeOnly, 0.5), percentile(storeOnly, 0.95),
		percentile(uncached, 0.5), percentile(uncached, 0.95),
		percentile(cached, 0.5), percentile(cached, 0.95))

	// 命中缓存的 p50 必须显著低于未命中（宽松判据：至少快一倍），否则缓存没起作用。
	if percentile(cached, 0.5)*2 > percentile(uncached, 0.5) {
		t.Fatalf("命中缓存未带来两倍以上提速：未命中 %v，命中 %v",
			percentile(uncached, 0.5), percentile(cached, 0.5))
	}
}

func measureStatsRequest(t *testing.T, module *usageLogsModule, target string) time.Duration {
	t.Helper()
	recorder := httptest.NewRecorder()
	started := time.Now()
	module.handleStats(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	elapsed := time.Since(started)
	if recorder.Code != http.StatusOK {
		t.Fatalf("统计请求应为 200，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	return elapsed
}

func percentile(values []time.Duration, p float64) time.Duration {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	index := int(float64(len(sorted)-1) * p)
	return sorted[index]
}

// TestIntegrationUsageLogsStatsShapeUnchanged 钉住「响应形状一字不改」：缓存与未命中两条路径
// 都必须给出同一组键与同一个值。
func TestIntegrationUsageLogsStatsShapeUnchanged(t *testing.T) {
	if os.Getenv("CCH_TEST_DSN") == "" {
		t.Skip("未设置 CCH_TEST_DSN，跳过数据库集成测试")
	}
	pools, err := store.Open(context.Background(), store.Options{
		DSN:      os.Getenv("CCH_TEST_DSN"),
		Budget:   config.SplitPoolBudget(6),
		Timeouts: config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("建立连接池失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	model, _ := seedIncrementalFixture(t, pools, 2)
	// 直接读 store（无缓存路径）作为基准。
	direct, err := pools.FindUsageLogsStats(context.Background(),
		store.UsageLogFilters{Model: model}, false)
	if err != nil {
		t.Fatalf("直读统计失败: %v", err)
	}
	router := New(Options{Deps: Deps{
		Guard: ulStubGuard{}, Problems: NewProblems(nil), Store: pools,
	}})
	// 必须真注册：不然路由表为空，请求会落在 503「兜底处理器未装配」（本测试要的是走缓存的
	// 真实 handler 路径）。
	RegisterUsageLogsWith(router, Deps{
		Guard: ulStubGuard{}, Problems: NewProblems(nil), Store: pools,
	}, UsageLogsOptions{})
	status, body := ulDoRequest(t, router, "/api/v1/usage-logs/stats?model="+model)
	if status != http.StatusOK {
		t.Fatalf("统计应为 200，实际 %d: %+v", status, body)
	}
	wantKeys := []string{
		"totalRequests", "totalCost", "totalTokens", "totalInputTokens", "totalOutputTokens",
		"totalCacheCreationTokens", "totalCacheReadTokens", "totalCacheCreation5mTokens",
		"totalCacheCreation1hTokens",
	}
	if len(body) != len(wantKeys) {
		t.Fatalf("统计响应键数应为 %d，实际 %d（%+v）", len(wantKeys), len(body), body)
	}
	for _, key := range wantKeys {
		if _, ok := body[key]; !ok {
			t.Fatalf("统计响应缺键 %s: %+v", key, body)
		}
	}
	if body["totalRequests"] != float64(direct.TotalRequests) {
		t.Fatalf("缓存路径与直读不一致: %v vs %d", body["totalRequests"], direct.TotalRequests)
	}
	if body["totalTokens"] != float64(direct.TotalTokens) {
		t.Fatalf("totalTokens 不一致: %v vs %d", body["totalTokens"], direct.TotalTokens)
	}
}
