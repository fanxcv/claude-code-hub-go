package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/limit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住 me 与 system 两个资源：上半是**路由契约**（哪些被注册、哪些故意不注册——漏注册是
// 静默回退 Node，只能靠测试发现），下半是真 PG 的集成（夹具自钉自清，见 meFixture 的说明）。
//
// 「越权」在自服务面的等价物是**身份不可达**：/me/* 的签名里没有主体参数，因此证明越权不成立的
// 方式不是断言 403，而是断言「主体 A 的读数里只有 A 的行」——库里同时存在主体 B 的行（本测试自己
// 造），A 的响应里必须看不到它。

// ---- 测试替身 ----

// fakeUserSessionCounter 是 User 维度会话计数的测试替身。
type fakeUserSessionCounter struct{ count int }

func (f fakeUserSessionCounter) UserSessionCount(context.Context, int64) (int, error) {
	return f.count, nil
}

// fakeUserFixed5hWindows 是 User 维度 5h 固定窗口的测试替身。
type fakeUserFixed5hWindows struct{ state limit.Fixed5hState }

func (f fakeUserFixed5hWindows) UserFixed5hWindowState(
	context.Context, int64, time.Time,
) (limit.Fixed5hState, error) {
	return f.state, nil
}

// meRuntimeDeps 是 me 资源四条运行态读数齐备的依赖集合。
func meRuntimeDeps(
	pools *store.Pools,
	principal Principal,
	keySessions int,
	userSessions int,
) Deps {
	return Deps{
		Guard:              principalGuard{principal: principal},
		Problems:           NewProblems(nil),
		Store:              pools,
		SessionCounts:      fakeSessionCounter{count: keySessions},
		UserSessionCounts:  fakeUserSessionCounter{count: userSessions},
		Fixed5hWindows:     fakeFixed5hWindows{state: limit.Fixed5hState{Current: 1.5, Exists: true}},
		UserFixed5hWindows: fakeUserFixed5hWindows{state: limit.Fixed5hState{Current: 2.5, Exists: true}},
	}
}

// ---- 路由契约 ----

// TestRegisterMeRoutesWithoutRuntimeDeps 钉住「缺 Redis 运行态时不注册 /me/quota」。
//
// metadata、today 与四条用量面路由都不依赖 Redis，必须照常注册；quota 的
// userCurrentConcurrentSessions 与 5h 固定窗口读不出来，恒 0 是静默错数，因此宁可不答（回退 Node）。
func TestRegisterMeRoutesWithoutRuntimeDeps(t *testing.T) {
	router := New(Options{Deps: Deps{Guard: &recordingGuard{}}})
	RegisterMeRoutes(router, Deps{Guard: &recordingGuard{}, Store: &store.Pools{}})

	want := []string{
		http.MethodGet + " /me/metadata",
		http.MethodGet + " /me/today",
		http.MethodGet + " /me/usage-logs",
		// /me/usage-logs/full 是纯读（只查账本），不依赖会话/窗口运行态，故这里也注册。
		http.MethodGet + " /me/usage-logs/full",
		http.MethodGet + " /me/usage-logs/models",
		http.MethodGet + " /me/usage-logs/endpoints",
		http.MethodGet + " /me/usage-logs/stats-summary",
	}
	if got := router.RouteCount(); got != len(want) {
		t.Fatalf("缺运行态依赖时应只注册 %d 条，实际 %d：%+v",
			len(want), got, routeKeys(router.RouteList()))
	}
	for _, key := range want {
		if hit := matchPath(t, router, http.MethodGet, strings.SplitN(key, " ", 2)[1]); hit == "" {
			t.Errorf("%s 应命中已注册路由", key)
		}
	}
	if hit := matchPath(t, router, http.MethodGet, "/me/quota"); hit != "" {
		t.Errorf("/me/quota 在缺运行态依赖时不该注册，实际命中 %s", hit)
	}
}

// TestRegisterMeRoutesWithRuntimeDeps 钉住齐备时的七条：档位一律 read（Node 的 x-required-access），
// operationId 与 Node 的 meRouter.openapi 逐条一致。
func TestRegisterMeRoutesWithRuntimeDeps(t *testing.T) {
	deps := meRuntimeDeps(&store.Pools{}, Principal{}, 0, 0)
	router := New(Options{Deps: Deps{Guard: &recordingGuard{}}})
	RegisterMeRoutes(router, deps)

	want := map[string]string{
		http.MethodGet + " /me/metadata":                 "getMeMetadata",
		http.MethodGet + " /me/quota":                    "getMeQuota",
		http.MethodGet + " /me/today":                    "getMeToday",
		http.MethodGet + " /me/usage-logs":               "listMeUsageLogs",
		http.MethodGet + " /me/usage-logs/models":        "listMeUsageModels",
		http.MethodGet + " /me/usage-logs/endpoints":     "listMeUsageEndpoints",
		http.MethodGet + " /me/usage-logs/stats-summary": "getMeStatsSummary",
		http.MethodGet + " /me/usage-logs/full":          "listMeUsageLogsFull",
	}
	if got := router.RouteCount(); got != 8 {
		t.Fatalf("运行态依赖齐备时应注册 8 条，实际 %d：%+v", got, routeKeys(router.RouteList()))
	}
	for _, route := range router.RouteList() {
		expected, ok := want[route.Method+" "+route.Path]
		if !ok {
			t.Fatalf("出现未预期的 me 路由：%s %s", route.Method, route.Path)
		}
		if route.OperationID != expected {
			t.Errorf("%s 的 operationId 应为 %s，实际 %s", route.Path, expected, route.OperationID)
		}
		if route.Access != AccessRead {
			t.Errorf("%s 的档位应为 read，实际 %s", route.Path, route.Access)
		}
		delete(want, route.Method+" "+route.Path)
	}
	if len(want) != 0 {
		t.Fatalf("这些 me 路由缺注册：%+v", want)
	}
}

// TestRegisterSystemRoutesRegistersImplemented 钉住 system 只注册两条读端点。
//
// GET/PUT /system/settings 未实现（依赖 Node 的 SystemSettings 投影与部分更新校验，见 system.go
// 文件头），因此**必须不在表内**——若将来实现了却忘了登记，这条会红；若被误注册，也会红。
func TestRegisterSystemRoutesRegistersImplemented(t *testing.T) {
	router := New(Options{Deps: Deps{Guard: &recordingGuard{}}})
	RegisterSystemRoutes(router, Deps{Guard: &recordingGuard{}, Store: &store.Pools{}})

	if got := router.RouteCount(); got != 2 {
		t.Fatalf("system 应只注册 2 条，实际 %d：%+v", got, routeKeys(router.RouteList()))
	}
	want := map[string]string{
		http.MethodGet + " /system/display-settings": "getSystemDisplaySettings",
		http.MethodGet + " /system/timezone":         "getSystemTimezone",
	}
	for _, route := range router.RouteList() {
		expected, ok := want[route.Method+" "+route.Path]
		if !ok {
			t.Fatalf("出现未预期的 system 路由：%s %s", route.Method, route.Path)
		}
		if route.OperationID != expected {
			t.Errorf("%s 的 operationId 应为 %s，实际 %s", route.Path, expected, route.OperationID)
		}
	}
	for key := range want {
		if hit := matchPath(t, router, http.MethodGet, strings.SplitN(key, " ", 2)[1]); hit == "" {
			t.Errorf("%s 应命中已注册路由", key)
		}
	}
	if hit := matchPath(t, router, http.MethodGet, "/system/settings"); hit != "" {
		t.Errorf("/system/settings 本轮不该注册，实际命中 %s", hit)
	}
}

// ---- 真 PG 集成 ----

const meFixtureCost = "0.250000000000000"

// meFixture 是一个**独立主体**：自有用户 + 自有密钥 + 自有账本行。
//
// 为什么不用共享的 loadtest 用户与密钥（usage-logs 那条路用了）：me/* 的读数是**主体全量**，
// 没有筛选参数可用来把别人的行挡在外面。共享主体下「总额」会被历史行污染，断言只能退化成不等式。
// 独立主体让账本只含本测试的行，断言可以是精确值——而且这才真正证明「看不到别人的行」。
//
// cost 用于区分两个主体：主体 A 0.25、主体 B 0.75，互相看得见就必然对不上数。
type meFixture struct {
	userID   int64
	keyID    int64
	keyValue string
	model    string
	cost     string
	request  int64
}

// seedMeFixture 建主体并插一条**可计费**行与一条**被拦截**行（后者不进账本口径）。
//
// 密钥的重置模式改成 rolling：固定日窗的边界是测试运行时刻的函数（刚过零点就落空），
// rolling 让窗口恒为「最近 24 小时」，断言不必看日历。
func seedMeFixture(t *testing.T, pools *store.Pools, label string, cost string) meFixture {
	t.Helper()
	ctx := context.Background()
	suffix := fmt.Sprintf("%s-%d", label, time.Now().UnixNano())
	fixture := meFixture{
		model: "go-admin-me-it-" + suffix,
		cost:  cost,
	}
	fixture.userID = fixtureUser(t, pools, "user", true)
	fixture.keyID, fixture.keyValue = fixtureKey(t, pools, fixtureKeyOptions{
		userID:        fixture.userID,
		canLoginWebUI: true,
		isEnabled:     true,
	})

	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取 control 分道失败: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE keys SET daily_reset_mode = 'rolling' WHERE id = $1`, fixture.keyID); err != nil {
		t.Fatalf("设置密钥日窗模式失败: %v", err)
	}

	blockedBy := "sensitive_word"
	err = pool.QueryRow(ctx, `
		INSERT INTO message_request (
			provider_id, user_id, key, model, original_model, endpoint,
			status_code, input_tokens, output_tokens, cost_usd, duration_ms, ttfb_ms,
			is_replay, created_at
		) VALUES (
			$1, $2, $3, $4, $4, '/v1/messages',
			200, 100, 20, $5::numeric, 1234, 55,
			false, now() - interval '2 minutes'
		) RETURNING id`,
		int64(1), fixture.userID, fixture.keyValue, fixture.model, fixture.cost,
	).Scan(&fixture.request)
	if err != nil {
		t.Fatalf("插入可计费夹具行失败: %v", err)
	}

	// 被拦截行：账本写入 BillingCondition 会排除它（blocked_by IS NULL 是前提），
	// 因此 today/quota 的成本读数必须只反映上面那一条。
	_, err = pool.Exec(ctx, `
		INSERT INTO message_request (
			provider_id, user_id, key, model, endpoint, status_code,
			blocked_by, cost_usd, is_replay, created_at
		) VALUES (
			$1, $2, $3, $4, '/v1/messages', 403,
			$5, $6::numeric, false, now() - interval '2 minutes'
		)`,
		int64(1), fixture.userID, fixture.keyValue, fixture.model+"-blocked", blockedBy, fixture.cost,
	)
	if err != nil {
		t.Fatalf("插入被拦截夹具行失败: %v", err)
	}

	t.Cleanup(func() {
		cleanupPool, cleanupErr := pools.Control()
		if cleanupErr != nil {
			return
		}
		// 先删账本（触发器不负责删除），再删请求行；用户与密钥由 fixtureUser 的清理收尾。
		_, _ = cleanupPool.Exec(ctx,
			`DELETE FROM usage_ledger WHERE request_id IN (SELECT id FROM message_request WHERE key = $1)`,
			fixture.keyValue)
		_, _ = cleanupPool.Exec(ctx, `DELETE FROM message_request WHERE key = $1`, fixture.keyValue)
	})
	return fixture
}

// meIntegrationDSN 复用 auth_test 的同名约定（未设置即跳过）。
func meOpenPools(t *testing.T) *store.Pools {
	t.Helper()
	dsn := os.Getenv("CCH_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 CCH_TEST_DSN，跳过数据库集成测试")
	}
	pools, err := store.Open(context.Background(), store.Options{
		DSN:      dsn,
		Budget:   config.SplitPoolBudget(6),
		Timeouts: config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("建立连接池失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	return pools
}

// meGet 发一次 GET 并解出 JSON 对象。
func meGet(t *testing.T, router *Router, target string) (int, map[string]any) {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON（状态 %d）: %s", recorder.Code, recorder.Body.String())
	}
	return recorder.Code, body
}

// meNumber 把 JSON number 取成 float。
func meNumber(t *testing.T, body map[string]any, key string) float64 {
	t.Helper()
	value, ok := body[key]
	if !ok {
		t.Fatalf("响应缺字段 %s: %+v", key, body)
	}
	number, ok := value.(float64)
	if !ok {
		t.Fatalf("字段 %s 应为数字，实际 %T（%v）", key, value, value)
	}
	return number
}

// TestMeIntegrationSelfScopedReads 是 me 三条读端点对着两个独立主体的集成断言。
func TestMeIntegrationSelfScopedReads(t *testing.T) {
	pools := meOpenPools(t)
	self := seedMeFixture(t, pools, "self", meFixtureCost)
	other := seedMeFixture(t, pools, "other", "0.750000000000000")

	principal := Principal{
		UserID:   self.userID,
		Username: "me-it-self",
		KeyID:    self.keyID,
		KeyName:  "me-it-key",
		IsAdmin:  false,
	}
	deps := meRuntimeDeps(pools, principal, 3, 5)
	router := New(Options{Deps: Deps{Guard: principalGuard{principal: principal}}})
	RegisterMeRoutes(router, deps)

	t.Run("metadata 取自己的行", func(t *testing.T) {
		status, body := meGet(t, router, "/me/metadata")
		if status != http.StatusOK {
			t.Fatalf("状态应为 200，实际 %d：%+v", status, body)
		}
		if body["keyName"] != "go-adminapi-it" {
			t.Errorf("keyName 应为夹具密钥名，实际 %v", body["keyName"])
		}
		record, err := pools.FindAdminKeyByID(context.Background(), self.keyID)
		if err != nil {
			t.Fatalf("读回夹具密钥失败: %v", err)
		}
		// provider_group 由库的默认值决定（不假定为 NULL）：与库里那一行逐字对齐即可。
		var expectedGroup any
		if record.ProviderGroup != nil {
			expectedGroup = *record.ProviderGroup
		}
		if body["keyProviderGroup"] != expectedGroup {
			t.Errorf("keyProviderGroup 应为库值 %v，实际 %v", expectedGroup, body["keyProviderGroup"])
		}
		if body["keyIsEnabled"] != true || body["userIsEnabled"] != true {
			t.Errorf("启用位应都为 true，实际 key=%v user=%v", body["keyIsEnabled"], body["userIsEnabled"])
		}
		if body["dailyResetMode"] != "rolling" {
			t.Errorf("dailyResetMode 应取密钥的 rolling，实际 %v", body["dailyResetMode"])
		}
		settings, err := pools.FindSystemSettings(context.Background())
		if err != nil {
			t.Fatalf("读系统设置失败: %v", err)
		}
		if body["currencyCode"] != settings.CurrencyDisplay {
			t.Errorf("currencyCode 应取 system_settings，实际 %v", body["currencyCode"])
		}
		if body["billingModelSource"] != settings.BillingModelSource {
			t.Errorf("billingModelSource 应取 system_settings，实际 %v", body["billingModelSource"])
		}
	})

	t.Run("today 只含自己的行", func(t *testing.T) {
		status, body := meGet(t, router, "/me/today")
		if status != http.StatusOK {
			t.Fatalf("状态应为 200，实际 %d：%+v", status, body)
		}
		// 夹具只插了一条可计费行（第二条被拦截，不进账本口径）。
		if calls := meNumber(t, body, "calls"); calls != 1 {
			t.Errorf("calls 应为 1，实际 %v", calls)
		}
		if cost := meNumber(t, body, "costUsd"); cost != 0.25 {
			t.Errorf("costUsd 应为 0.25，实际 %v", cost)
		}
		if tokens := meNumber(t, body, "inputTokens"); tokens != 100 {
			t.Errorf("inputTokens 应为 100，实际 %v", tokens)
		}
		breakdown, ok := body["modelBreakdown"].([]any)
		if !ok || len(breakdown) != 1 {
			t.Fatalf("modelBreakdown 应恰有一项，实际 %#v", body["modelBreakdown"])
		}
		entry := breakdown[0].(map[string]any)
		if entry["model"] != self.model {
			t.Errorf("modelBreakdown 应只含自己的模型 %s，实际 %v", self.model, entry["model"])
		}
		if entry["model"] == other.model {
			t.Errorf("不应看到另一个主体的模型 %s", other.model)
		}
	})

	t.Run("quota 只累计自己的成本", func(t *testing.T) {
		status, body := meGet(t, router, "/me/quota")
		if status != http.StatusOK {
			t.Fatalf("状态应为 200，实际 %d：%+v", status, body)
		}
		for _, key := range []string{"keyCurrentDailyUsd", "keyCurrentWeeklyUsd", "keyCurrentMonthlyUsd"} {
			if value := meNumber(t, body, key); value < 0.25 {
				t.Errorf("%s 应含自己的 0.25，实际 %v", key, value)
			}
			// 两个主体各自独立：读到的值若含另一主体的 0.75 就说明越了界。
			if value := meNumber(t, body, key); value >= 1.0 {
				t.Errorf("%s 把另一个主体的成本算进来了：%v", key, value)
			}
		}
		if total := meNumber(t, body, "keyCurrentTotalUsd"); total < 0.25 || total >= 1.0 {
			t.Errorf("keyCurrentTotalUsd 应恰为自己的成本区间，实际 %v", total)
		}
		if userTotal := meNumber(t, body, "userCurrentTotalUsd"); userTotal < 0.25 || userTotal >= 1.0 {
			t.Errorf("userCurrentTotalUsd 应恰为自己的成本区间，实际 %v", userTotal)
		}
		// 会话计数与 5h 读数取注入的运行态（rolling 日窗不读 Redis，故只断言会话数）。
		if current := meNumber(t, body, "keyCurrentConcurrentSessions"); current != 3 {
			t.Errorf("key 维度会话数应取注入值 3，实际 %v", current)
		}
		if current := meNumber(t, body, "userCurrentConcurrentSessions"); current != 5 {
			t.Errorf("user 维度会话数应取注入值 5，实际 %v", current)
		}
		// 未设限额的列必须是 null，而不是 0：Node 的 `?? null` 语义。
		for _, key := range []string{
			"keyLimit5hUsd", "keyLimitDailyUsd", "keyLimitWeeklyUsd", "keyLimitMonthlyUsd",
			"keyLimitTotalUsd", "userLimit5hUsd", "userLimitWeeklyUsd", "userLimitMonthlyUsd",
			"userLimitTotalUsd", "userLimitConcurrentSessions", "userRpmLimit", "userLimitDailyUsd",
		} {
			if value, ok := body[key]; !ok || value != nil {
				t.Errorf("%s 未设限额时应为 null，实际 %v（存在=%v）", key, value, ok)
			}
		}
		// 数组列不得是 null（Node 的 `?? []`）。
		for _, key := range []string{"userAllowedModels", "userAllowedClients"} {
			if _, ok := body[key].([]any); !ok {
				t.Errorf("%s 应为数组，实际 %T", key, body[key])
			}
		}
	})
}

// TestMeIntegrationFixed5hReadsRuntimeWindow 钉住 5h **固定模式**走 Redis 运行态而不是账本聚合。
//
// 这是 Node 的两条分支（service.ts:1264-1266）：固定模式下账本聚合值必须被运行态读数覆盖，
// 否则「5h 限额到点清零」的语义在自服务页上就看不见了。
func TestMeIntegrationFixed5hReadsRuntimeWindow(t *testing.T) {
	pools := meOpenPools(t)
	fixture := seedMeFixture(t, pools, "fixed5h", meFixtureCost)

	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取 control 分道失败: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE keys SET limit_5h_reset_mode = 'fixed' WHERE id = $1`, fixture.keyID); err != nil {
		t.Fatalf("设置密钥 5h 模式失败: %v", err)
	}

	principal := Principal{UserID: fixture.userID, KeyID: fixture.keyID, KeyName: "me-it-key"}
	deps := meRuntimeDeps(pools, principal, 0, 0)
	router := New(Options{Deps: Deps{Guard: principalGuard{principal: principal}}})
	RegisterMeRoutes(router, deps)

	status, body := meGet(t, router, "/me/quota")
	if status != http.StatusOK {
		t.Fatalf("状态应为 200，实际 %d：%+v", status, body)
	}
	if value := meNumber(t, body, "keyCurrent5hUsd"); value != 1.5 {
		t.Errorf("固定模式下 keyCurrent5hUsd 应取运行态 1.5（而非账本 0.25），实际 %v", value)
	}
	// 用户侧默认是 rolling（夹具未改），故仍走账本聚合：0.25。
	if value := meNumber(t, body, "userCurrent5hUsd"); value < 0.25 {
		t.Errorf("滚动模式下 userCurrent5hUsd 应取账本值，实际 %v", value)
	}
}

// TestSystemIntegrationDisplaySettingsAndTimezone 钉住 system 两条读端点对着真库的形状。
func TestSystemIntegrationDisplaySettingsAndTimezone(t *testing.T) {
	pools := meOpenPools(t)
	principal := Principal{UserID: 1, KeyID: 1, IsAdmin: true}
	router := New(Options{Deps: Deps{Guard: principalGuard{principal: principal}}})
	RegisterSystemRoutes(router, Deps{
		Guard:    principalGuard{principal: principal},
		Problems: NewProblems(nil),
		Store:    pools,
	})

	settings, err := pools.FindSystemSettings(context.Background())
	if err != nil {
		t.Fatalf("读系统设置失败: %v", err)
	}

	status, body := meGet(t, router, "/system/display-settings")
	if status != http.StatusOK {
		t.Fatalf("display-settings 状态应为 200，实际 %d：%+v", status, body)
	}
	if body["siteTitle"] != settings.SiteTitle {
		t.Errorf("siteTitle 应为库值 %q，实际 %v", settings.SiteTitle, body["siteTitle"])
	}
	if body["currencyDisplay"] != settings.CurrencyDisplay {
		t.Errorf("currencyDisplay 应为库值 %q，实际 %v", settings.CurrencyDisplay, body["currencyDisplay"])
	}
	if body["billingModelSource"] != settings.BillingModelSource {
		t.Errorf("billingModelSource 应为库值 %q，实际 %v",
			settings.BillingModelSource, body["billingModelSource"])
	}
	if len(body) != 3 {
		t.Errorf("display-settings 只应有三列（Node 同形），实际 %d 列：%+v", len(body), body)
	}

	status, body = meGet(t, router, "/system/timezone")
	if status != http.StatusOK {
		t.Fatalf("timezone 状态应为 200，实际 %d：%+v", status, body)
	}
	// 三级取值：库里的 timezone 优先；为空时退到 TZ；都不可用才是 UTC。
	raw, err := pools.AdminSystemTimezone(context.Background())
	if err != nil {
		t.Fatalf("读库时区失败: %v", err)
	}
	expected, err := meSystemLocation(context.Background(), pools)
	if err != nil {
		t.Fatalf("解析系统时区失败: %v", err)
	}
	if raw != nil && strings.TrimSpace(*raw) != "" {
		if body["timeZone"] != *raw {
			t.Errorf("库里有 timezone 时应原样作答 %q，实际 %v", *raw, body["timeZone"])
		}
	} else if body["timeZone"] != expected.String() {
		t.Errorf("库无 timezone 时应退到 TZ/UTC，期望 %q，实际 %v", expected.String(), body["timeZone"])
	}
}

// TestMeQuotaVirtualAdminProjection 钉住 ADMIN_TOKEN 合成主体的 me/quota 投影（对拍缺陷 D2）。
//
// Node 的合成主体（src/lib/auth.ts:196-212）把 rpm 与 dailyQuota 写死为 **0**，而 getMyQuota 直接取
// `user.rpm ?? null` / `user.dailyQuota ?? null`（my-usage.ts:508/516）——于是这两项是数字 0，
// **不是 null**；合成主体里没有其余限额字段，它们才走 null。
//
// 这条钉子只认类型：`null` 与 `0` 在前端是两条分支（`? :` vs `??`），形状级回归必须靠类型级断言抓。
func TestMeQuotaVirtualAdminProjection(t *testing.T) {
	principal := Principal{
		UserID:   -1,
		Username: virtualAdminUserName,
		KeyID:    -1,
		KeyName:  virtualAdminKeyName,
		IsAdmin:  true,
	}
	deps := meRuntimeDeps(&store.Pools{}, principal, 0, 0)
	router := New(Options{Deps: Deps{Guard: principalGuard{principal: principal}}})
	RegisterMeRoutes(router, deps)

	status, body := meGet(t, router, "/me/quota")
	if status != http.StatusOK {
		t.Fatalf("状态应为 200，实际 %d：%+v", status, body)
	}

	// 合成主体写死的两个 0：必须是数字。
	if rpm := meNumber(t, body, "userRpmLimit"); rpm != 0 {
		t.Errorf("userRpmLimit 应为数字 0，实际 %v", rpm)
	}
	if daily := meNumber(t, body, "userLimitDailyUsd"); daily != 0 {
		t.Errorf("userLimitDailyUsd 应为数字 0，实际 %v", daily)
	}

	// 合成主体里不存在的限额：必须是 null（Node 的 `?? null`）。
	for _, key := range []string{
		"userLimit5hUsd", "userLimitWeeklyUsd", "userLimitMonthlyUsd", "userLimitTotalUsd",
		"userLimitConcurrentSessions",
		"keyLimit5hUsd", "keyLimitDailyUsd", "keyLimitWeeklyUsd", "keyLimitMonthlyUsd", "keyLimitTotalUsd",
		"userExpiresAt", "keyProviderGroup", "expiresAt",
	} {
		if value, ok := body[key]; !ok || value != nil {
			t.Errorf("%s 应为 null，实际 %v（存在=%v）", key, value, ok)
		}
	}

	// 合成主体的名字与成本读数。
	if body["userName"] != virtualAdminUserName {
		t.Errorf("userName 应为 %q，实际 %v", virtualAdminUserName, body["userName"])
	}
	if body["keyName"] != virtualAdminKeyName {
		t.Errorf("keyName 应为 %q，实际 %v", virtualAdminKeyName, body["keyName"])
	}
	for _, key := range []string{
		"keyCurrent5hUsd", "keyCurrentDailyUsd", "keyCurrentWeeklyUsd", "keyCurrentMonthlyUsd",
		"keyCurrentTotalUsd", "userCurrent5hUsd", "userCurrentDailyUsd", "userCurrentWeeklyUsd",
		"userCurrentMonthlyUsd", "userCurrentTotalUsd",
	} {
		if value := meNumber(t, body, key); value != 0 {
			t.Errorf("%s 应为 0（虚拟会话不读库），实际 %v", key, value)
		}
	}
}
