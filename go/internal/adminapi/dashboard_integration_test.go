package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 **dashboard 资源**（/api/v1/dashboard/*）的真实 PG 集成测试（五条已就绪端点各一例）。
//
// 夹具口径与三条纪律：
//   - 每个用例自建「用户 + 密钥 + 用量行」，并以**唯一 model 名**（含纳秒）做定位与清理；
//     绝不用 `DELETE FROM message_request WHERE id > x` 这种会伤到邻座的写法。
//   - 概览与并发数**只有在「非管理员 + allowGlobalUsageView=false」时才是可精确断言的**
//     （那时查询范围被钉在该用户上）。allowGlobalUsageView 是**共享的全局设置行**，测试不翻它
//     （翻了会影响并行跑的其它包），而是读当前值后分支断言，并把这个前提写在用例注释里。
//   - 会话观测读数来自 Redis 运行态（数据面维护），管理面只是读；故这几条用例注入替身，
//     真 Redis 的键语义由 dashboard_runtime 的单元用例与数据面的会话用例覆盖。

// dashboardMessageRow 是一条夹具请求行的参数。
type dashboardMessageRow struct {
	model        string
	cost         string
	durationMs   int
	userAgent    string
	errorMessage string
	// createdAt 指定创建时刻（nil 时用 createdAtSQL / 默认的「3 分钟前」）。
	// 锚定到**当地今日窗口**的行必须显式给时刻：否则 CST 00:00–00:03 内「3 分钟前」
	// 会落到当地昨天，而断言却期望它算今天——期望值在那个窗口内是反的。
	createdAt *time.Time
	// createdAtSQL 用 SQL 表达式指定创建时刻（如 `CURRENT_TIMESTAMP`），优先于 createdAt。
	// 用于必须与**库钟**同源的场景：宿主与库跨主机时的钟差会让「最近一分钟」的断言飘。
	createdAtSQL string
}

// seedDashboardSubject 建「用户 + 密钥」并插若干请求行（触发器会同步写账本行）。
//
// 返回 userID / keyID / keyValue 与一个唯一前缀（清理与定位用）。
func seedDashboardSubject(t *testing.T, pools *store.Pools, label string, rows []dashboardMessageRow) (int64, int64, string, string) {
	t.Helper()
	ctx := context.Background()
	prefix := fmt.Sprintf("go-adminapi-dashboard-it-%s-%d", label, time.Now().UnixNano())
	userID := fixtureUser(t, pools, "user", true)
	keyID, keyValue := fixtureKey(t, pools, fixtureKeyOptions{
		userID:        userID,
		canLoginWebUI: true,
		isEnabled:     true,
	})

	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取 control 分道失败: %v", err)
	}
	for index, row := range rows {
		model := row.model
		if model == "" {
			model = fmt.Sprintf("%s-%d", prefix, index)
		}
		var errorMessage any
		if row.errorMessage != "" {
			errorMessage = row.errorMessage
		}
		var userAgent any
		if row.userAgent != "" {
			userAgent = row.userAgent
		}
		duration := row.durationMs
		if duration == 0 {
			duration = 1234
		}
		// 成本列是 numeric：空串不是合法数值（用例只关心 UA / error_message 时也要给 0）。
		cost := row.cost
		if cost == "" {
			cost = "0"
		}
		createdAtExpr := row.createdAtSQL
		if createdAtExpr == "" {
			createdAtExpr = "now() - interval '3 minutes'"
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO message_request (
				provider_id, user_id, key, model, original_model, endpoint,
				status_code, input_tokens, output_tokens, cost_usd, duration_ms, ttfb_ms,
				error_message, user_agent, is_replay, created_at
			) VALUES (
				$1, $2, $3, $4, $4, '/v1/messages',
				200, 100, 20, $5::numeric, $6, 55,
				$7, $8, false, COALESCE($9::timestamptz, `+createdAtExpr+`)
			)`,
			int64(1), userID, keyValue, model, cost, duration,
			errorMessage, userAgent, row.createdAt,
		); err != nil {
			t.Fatalf("插入 dashboard 夹具行失败: %v", err)
		}
	}

	t.Cleanup(func() {
		cleanupPool, cleanupErr := pools.Control()
		if cleanupErr != nil {
			return
		}
		// 触发器只写不删：先删账本再删请求行；用户与密钥由 fixtureUser 的清理收尾。
		_, _ = cleanupPool.Exec(ctx,
			`DELETE FROM usage_ledger WHERE request_id IN
				(SELECT id FROM message_request WHERE key = $1)`, keyValue)
		_, _ = cleanupPool.Exec(ctx, `DELETE FROM message_request WHERE key = $1`, keyValue)
	})
	return userID, keyID, keyValue, prefix
}

// dashboardRouter 建一个以该身份注入的路由表，并注入会话观测读数替身。
func dashboardRouter(
	t *testing.T,
	pools *store.Pools,
	principal Principal,
	sessions ObservedSessionRuntime,
) *Router {
	t.Helper()
	router := New(Options{Deps: Deps{Guard: principalGuard{principal: principal}}})
	RegisterDashboardRoutes(router, Deps{
		Guard:            principalGuard{principal: principal},
		Problems:         NewProblems(nil),
		Store:            pools,
		ObservedSessions: sessions,
	})
	return router
}

// dashboardGet 发一次 GET 并解出 JSON 对象。
func dashboardGet(t *testing.T, router *Router, target string) (int, map[string]any, string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	raw := recorder.Body.String()
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		return recorder.Code, nil, raw
	}
	return recorder.Code, body, raw
}

// TestDashboardConcurrentSessions 钉住并发数的两条分支：管理员读得到，普通用户看设置。
func TestDashboardConcurrentSessions(t *testing.T) {
	pools := meOpenPools(t)
	userID := fixtureUser(t, pools, "user", true)

	admin := Principal{UserID: userID, Username: "dashboard-it", IsAdmin: true}
	router := dashboardRouter(t, pools, admin, &fakeObservedSessions{count: 9})
	status, body, raw := dashboardGet(t, router, "/dashboard/concurrent-sessions")
	if status != http.StatusOK {
		t.Fatalf("管理员应拿到 200，实际 %d：%s", status, raw)
	}
	if count := body["count"].(float64); count != 9 {
		t.Fatalf("并发数应来自观测读数（9），实际 %v", count)
	}

	settings, err := pools.FindSystemSettings(context.Background())
	if err != nil {
		t.Fatalf("读系统设置失败: %v", err)
	}
	普通用户 := Principal{UserID: userID, Username: "dashboard-it", IsAdmin: false}
	router = dashboardRouter(t, pools, 普通用户, &fakeObservedSessions{count: 9})
	status, body, raw = dashboardGet(t, router, "/dashboard/concurrent-sessions")
	if settings.AllowGlobalUsageView {
		// 开了全局用量视图时普通用户也能看（Node 的 allowGlobalUsageView 分支）。
		if status != http.StatusOK || body["count"].(float64) != 9 {
			t.Fatalf("allowGlobalUsageView=true 时应放行，实际 %d：%s", status, raw)
		}
		return
	}
	if status != http.StatusForbidden {
		t.Fatalf("allowGlobalUsageView=false 时普通用户应被拒，实际 %d：%s", status, raw)
	}
	if body["errorCode"] != "dashboard.global_usage_forbidden" {
		t.Errorf("errorCode 应为 dashboard.global_usage_forbidden，实际 %v", body["errorCode"])
	}
	if body["detail"] != "Global concurrent session metrics are not available to this user." {
		t.Errorf("detail 应为 Node 的固定文案，实际 %v", body["detail"])
	}
}

// adminLocalTodayStart 取「服务端时区下今日零点」这一时刻（timestamptz）。
//
// 与产品的 bounds 表达式同源（`DATE_TRUNC('day', now AT TIME ZONE tz) AT TIME ZONE tz`，
// 见 store/admin_dashboard.go 的 AdminDashboardOverview），时区走产品的降级链
// pools.AdminSystemTimezoneOrUTC（DB -> TZ -> Asia/Shanghai）。用例据此把夹具行锉在
// 当地日窗口的两侧，而不是锉在墙上时刻上。
func adminLocalTodayStart(t *testing.T, pools *store.Pools) time.Time {
	t.Helper()
	ctx := context.Background()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取 control 分道失败: %v", err)
	}
	var start time.Time
	if err := pool.QueryRow(ctx,
		`SELECT (DATE_TRUNC('day', CURRENT_TIMESTAMP AT TIME ZONE $1) AT TIME ZONE $1)`,
		pools.AdminSystemTimezoneOrUTC(ctx),
	).Scan(&start); err != nil {
		t.Fatalf("按服务端时区取今日零点失败: %v", err)
	}
	return start
}

// TestDashboardOverview 钉住概览的聚合口径（今日请求/成本/均值/错误率/窗口）。
//
// 可精确断言的前提：非管理员 + allowGlobalUsageView=false → 查询范围钉在该用户上。
// 若部署把该设置开着，则范围是全站，此时只断言「不小于自己的那一份」。
func TestDashboardOverview(t *testing.T) {
	pools := meOpenPools(t)
	// 夹具只锤三行，都锚在**产品自己的当地日窗口**上，使断言与墙上时刻无关：
	//   A：今日零点 +30 分钟——必在「今日」内；
	//   B：今日零点 -30 分钟（当地昨日 23:30）——必被「今日」排除；这一行是**当地日界 vs UTC 日界**的
	//      判别器：CST 00:00–08:00 内按 UTC 切窗会把它算进来（成本、条数、错误率、均值四项同时报错）；
	//   C：库钟的此刻——既在今日窗内，也在「最近一分钟」内。
	// 时刻用绑定参数（A/B）与库钟表达式（C）给定：跨主机钟差不会让断言飘。
	todayStart := adminLocalTodayStart(t, pools)
	offsetFromTodayStart := func(delta time.Duration) *time.Time {
		value := todayStart.Add(delta)
		return &value
	}
	userID, _, _, _ := seedDashboardSubject(t, pools, "overview", []dashboardMessageRow{
		{cost: "0.25", durationMs: 1234, createdAt: offsetFromTodayStart(30 * time.Minute)},
		{cost: "5", durationMs: 9999, errorMessage: "窗口用例：当地昨日 23:30，不得计入今日",
			createdAt: offsetFromTodayStart(-30 * time.Minute)},
		{cost: "0", durationMs: 1234, createdAtSQL: "CURRENT_TIMESTAMP"},
	})

	settings, err := pools.FindSystemSettings(context.Background())
	if err != nil {
		t.Fatalf("读系统设置失败: %v", err)
	}
	principal := Principal{UserID: userID, Username: "dashboard-it", IsAdmin: false}
	router := dashboardRouter(t, pools, principal, &fakeObservedSessions{count: 5})

	status, body, raw := dashboardGet(t, router, "/dashboard/overview")
	if status != http.StatusOK {
		t.Fatalf("状态应为 200，实际 %d：%s", status, raw)
	}
	// 非管理员的并发数恒 0（Node 的 `isAdmin ? ... : 0`），哪怕 allowGlobalUsageView 开着。
	if got := body["concurrentSessions"].(float64); got != 0 {
		t.Errorf("非管理员的并发数应为 0，实际 %v", got)
	}
	if got := body["todayCost"].(float64); got != 0.25 {
		t.Errorf("今日成本应只含今日窗口内的行（0.25 + 0），实际 %v", got)
	}
	if got := body["avgResponseTime"].(float64); got != 1234 {
		t.Errorf("平均响应时间应只含今日窗口内的行（1234），实际 %v", got)
	}
	if got := body["todayErrorRate"].(float64); got != 0 {
		t.Errorf("今日两行都没有 error_message（即成功），错误率应为 0，实际 %v", got)
	}
	if settings.AllowGlobalUsageView {
		t.Log("allowGlobalUsageView=true：查询范围是全站，故只断言非空读数")
		return
	}
	if got := body["todayRequests"].(float64); got != 2 {
		t.Errorf("今日请求数应为 2（今日零点+30 分与此刻各一行），实际 %v", got)
	}
	if got := body["recentMinuteRequests"].(float64); got != 1 {
		t.Errorf("最近一分钟请求数应为 1（只有刻发行的那一行；零点+30 分那行不在最近一分钟里），实际 %v", got)
	}
}

// TestDashboardProviderSlots 钉住供应商插槽：只出启用的供应商，插槽数来自观测读数。
func TestDashboardProviderSlots(t *testing.T) {
	pools := meOpenPools(t)
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取 control 分道失败: %v", err)
	}
	ctx := context.Background()

	enabledID := dashboardProviderFixture(t, pool, "enabled", true, 3)
	disabledID := dashboardProviderFixture(t, pool, "disabled", false, 7)

	admin := Principal{UserID: 1, Username: "dashboard-it", IsAdmin: true}
	sessions := &fakeObservedSessions{providerWise: map[int64]int{enabledID: 2, disabledID: 5}}
	router := dashboardRouter(t, pools, admin, sessions)

	status, body, raw := dashboardGet(t, router, "/dashboard/provider-slots")
	if status != http.StatusOK {
		t.Fatalf("状态应为 200，实际 %d：%s", status, raw)
	}
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("响应缺 items：%+v", body)
	}

	found := false
	for _, entry := range items {
		item, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("items 元素应为对象：%+v", entry)
		}
		id := int64(item["providerId"].(float64))
		if id == disabledID {
			t.Errorf("未启用的供应商不该出现：%+v", item)
		}
		if volume, ok := item["totalVolume"].(float64); !ok || volume != 0 {
			t.Errorf("totalVolume 应恒为 0（Node 侧由排行榜数据填充）：%+v", item)
		}
		if id != enabledID {
			continue
		}
		found = true
		if got := item["usedSlots"].(float64); got != 2 {
			t.Errorf("usedSlots 应来自观测读数（2），实际 %v", got)
		}
		if got := item["totalSlots"].(float64); got != 3 {
			t.Errorf("totalSlots 应取 limitConcurrentSessions（3），实际 %v", got)
		}
	}
	if !found {
		t.Fatalf("启用的夹具供应商未出现在 items 里（共 %d 条）", len(items))
	}
	// 观测读数只该被问到「启用的」那些供应商。
	for _, asked := range sessions.providerIDs {
		if asked == disabledID {
			t.Errorf("不该为未启用的供应商 %d 读观测计数", disabledID)
		}
	}
	_ = ctx
}

// dashboardProviderFixture 建一个供应商夹具；enabled 控制 is_enabled，limit 控制并发上限。
func dashboardProviderFixture(
	t *testing.T,
	pool *store.Pool,
	label string,
	enabled bool,
	limit int,
) int64 {
	t.Helper()
	name := fmt.Sprintf("go-adminapi-dashboard-provider-%s-%d", label, time.Now().UnixNano())
	var id int64
	err := pool.QueryRow(context.Background(), `
		INSERT INTO providers (
			name, url, key, is_enabled, weight, priority,
			cost_multiplier, group_tag, provider_type, limit_concurrent_sessions
		) VALUES ($1, $2, $3, $4, 1, 100, 1::numeric, NULL, 'openai-compatible', $5)
		RETURNING id`,
		name, "https://example.invalid", "sk-dashboard-it", enabled, limit,
	).Scan(&id)
	if err != nil {
		t.Fatalf("建供应商夹具失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM providers WHERE id = $1`, id)
	})
	return id
}

// TestDashboardClientVersions 钉住分组、去重与 GA 判定（用唯一的客户端类型避免受邻座影响）。
func TestDashboardClientVersions(t *testing.T) {
	pools := meOpenPools(t)
	clientType := fmt.Sprintf("cch-it-cli-%d", time.Now().UnixNano())

	// 两个用户用同一版本（达标），一个用户更旧（需升级）。
	_, _, _, _ = seedDashboardSubject(t, pools, "cv-a", []dashboardMessageRow{
		{userAgent: clientType + "/2.0.35"},
	})
	_, _, _, _ = seedDashboardSubject(t, pools, "cv-b", []dashboardMessageRow{
		{userAgent: clientType + "/2.0.35"},
	})
	_, _, _, _ = seedDashboardSubject(t, pools, "cv-c", []dashboardMessageRow{
		{userAgent: clientType + "/2.0.30"},
	})

	admin := Principal{UserID: 1, Username: "dashboard-it", IsAdmin: true}
	router := dashboardRouter(t, pools, admin, &fakeObservedSessions{})
	status, body, raw := dashboardGet(t, router, "/dashboard/client-versions")
	if status != http.StatusOK {
		t.Fatalf("状态应为 200，实际 %d：%s", status, raw)
	}
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("响应缺 items：%+v", body)
	}

	var found map[string]any
	for _, entry := range items {
		item, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("items 元素应为对象：%+v", entry)
		}
		if item["clientType"] == clientType {
			found = item
			break
		}
	}
	if found == nil {
		t.Fatalf("夹具客户端类型 %s 未出现在结果里", clientType)
	}
	if ga, _ := found["gaVersion"].(string); ga != "2.0.35" {
		t.Errorf("GA 应为两个人用的 2.0.35，实际 %v", found["gaVersion"])
	}
	if total := found["totalUsers"].(float64); total != 3 {
		t.Errorf("去重后的用户数应为 3，实际 %v", total)
	}
	users, ok := found["users"].([]any)
	if !ok {
		t.Fatalf("users 应为数组：%+v", found)
	}
	upgrades, latest := 0, 0
	for _, entry := range users {
		user, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("users 元素应为对象：%+v", entry)
		}
		if user["username"] == nil || user["username"] == "" {
			t.Errorf("用户名不该为空（Node 侧落回 User {id}）：%+v", user)
		}
		if user["needsUpgrade"] == true {
			upgrades++
		}
		if user["isLatest"] == true {
			latest++
		}
	}
	if upgrades != 1 || latest != 2 {
		t.Errorf("应恰好 1 个待升级、2 个最新，实际 upgrades=%d latest=%d", upgrades, latest)
	}
}

// TestDashboardRateLimitStats 钉住限流事件的解析聚合（按用户隔离，避免受库内既有行影响）。
func TestDashboardRateLimitStats(t *testing.T) {
	pools := meOpenPools(t)
	const metadata = `upstream said rate_limit_metadata: {"limit_type":"rpm","current":%d}`
	userID, _, _, _ := seedDashboardSubject(t, pools, "rl", []dashboardMessageRow{
		{errorMessage: fmt.Sprintf(metadata, 10)},
		{errorMessage: fmt.Sprintf(metadata, 14)},
		// 带标记但**没有可解析的 JSON** 的一行：被 SQL 的 LIKE 扫进来（故进 total_events），
		// 但正则取不到 `{...}`，因此不进任何分桶。这正是 Node 的「扫描行数 ≠ 解析成功数」。
		{errorMessage: "upstream error rate_limit_metadata: broken"},
	})

	admin := Principal{UserID: 1, Username: "dashboard-it", IsAdmin: true}
	router := dashboardRouter(t, pools, admin, &fakeObservedSessions{})
	target := fmt.Sprintf("/dashboard/rate-limit-stats?userId=%d", userID)
	status, body, raw := dashboardGet(t, router, target)
	if status != http.StatusOK {
		t.Fatalf("状态应为 200，实际 %d：%s", status, raw)
	}
	if got := body["total_events"].(float64); got != 3 {
		t.Errorf("total_events 是 LIKE 扫到的行数（3），实际 %v", got)
	}
	byType, _ := body["events_by_type"].(map[string]any)
	if got := byType["rpm"].(float64); got != 2 {
		t.Errorf("rpm 事件应为 2，实际 %v", byType)
	}
	byUser, _ := body["events_by_user"].(map[string]any)
	if got := byUser[fmt.Sprintf("%d", userID)].(float64); got != 2 {
		t.Errorf("该用户应有 2 个事件，实际 %v", byUser)
	}
	timeline, _ := body["events_timeline"].([]any)
	if len(timeline) != 1 {
		t.Fatalf("夹具两行相隔毫秒，应聚成 1 个小时桶，实际 %d", len(timeline))
	}
	if got := body["avg_current_usage"].(float64); got != 12 {
		t.Errorf("平均 current 应为 (10+14)/2=12，实际 %v", got)
	}

	t.Run("limitType 过滤", func(t *testing.T) {
		_, filtered, filteredRaw := dashboardGet(t, router,
			target+"&limitType=usd_5h")
		if filtered == nil {
			t.Fatalf("响应不是 JSON：%s", filteredRaw)
		}
		if got := filtered["total_events"].(float64); got != 3 {
			t.Errorf("total_events 不受 limitType 影响（3），实际 %v", got)
		}
		if got := filtered["avg_current_usage"].(float64); got != 0 {
			t.Errorf("换成别的限流类型后没有命中行，均值应为 0，实际 %v", got)
		}
	})
}
