package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/limit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ratelimit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/session"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 keys 三条**读档**端点（GET /keys/{keyId}、/limit-usage、/quota）的测试：
// 上半是路由契约（含运行态依赖缺失时的回退），下半是真 PG + 真 Redis 的集成。
//
// 夹具纪律：密钥带唯一值、账本行按该密钥串过滤、Redis 只动本测试的会话成员与三个 active_sessions
// 键（清理前先删 info 键），t.Cleanup 全清——不整键删共享的全局键。

// ---- 路由契约 ----

// fakeSessionCounter 是只读会话计数的测试替身。
type fakeSessionCounter struct{ count int }

func (f fakeSessionCounter) KeySessionCount(context.Context, int64) (int, error) {
	return f.count, nil
}

// fakeFixed5hWindows 是 5h 固定窗口读取器的测试替身。
type fakeFixed5hWindows struct{ state limit.Fixed5hState }

func (f fakeFixed5hWindows) Fixed5hWindowState(
	context.Context, int64, time.Time,
) (limit.Fixed5hState, error) {
	return f.state, nil
}

// TestRegisterKeysRoutesWithRuntimeDeps 钉住两个运行态依赖齐备时的 14 条：三条读档路由必须
// 真的在表内、档位与 operationId 与 Node 一致（读档中 /limit-usage 是 read 档，另两条是 admin 档）。
func TestRegisterKeysRoutesWithRuntimeDeps(t *testing.T) {
	deps := Deps{
		Guard:          &recordingGuard{},
		Store:          &store.Pools{},
		SessionCounts:  fakeSessionCounter{},
		Fixed5hWindows: fakeFixed5hWindows{},
	}
	router := New(Options{Deps: Deps{Guard: &recordingGuard{}}})
	RegisterKeysRoutes(router, deps)

	want := map[string]struct {
		access      AccessLevel
		operationID string
	}{
		http.MethodGet + " /keys/{keyId:[0-9]+}":             {AccessAdmin, "getKey"},
		http.MethodGet + " /keys/{keyId:[0-9]+}/limit-usage": {AccessRead, "getKeyLimitUsage"},
		http.MethodGet + " /keys/{keyId:[0-9]+}/quota":       {AccessAdmin, "getKeyQuotaUsage"},
	}
	if got := router.RouteCount(); got != 14 {
		t.Fatalf("两个运行态依赖齐备时应注册 14 条，实际 %d：%+v", got, routeKeys(router.RouteList()))
	}
	for _, route := range router.RouteList() {
		expected, ok := want[route.Method+" "+route.Path]
		if !ok {
			continue
		}
		if route.Access != expected.access {
			t.Errorf("%s 的档位应为 %s，实际 %s", route.Path, expected.access, route.Access)
		}
		if route.OperationID != expected.operationID {
			t.Errorf("%s 的 operationId 应为 %s，实际 %s", route.Path, expected.operationID, route.OperationID)
		}
		delete(want, route.Method+" "+route.Path)
	}
	if len(want) != 0 {
		t.Fatalf("这三条读档路由缺注册：%+v", want)
	}

	// 逐条能命中（而不是被权重规则挤掉）。
	for _, target := range []string{"/keys/1234", "/keys/1234/limit-usage", "/keys/1234/quota"} {
		if hit := matchPath(t, router, http.MethodGet, target); hit == "" {
			t.Errorf("GET %s 应命中已注册路由", target)
		}
	}
}

// newKeysUsageTestRouter 装配注册了全部 14 条的 keys 路由表（身份由 principal 注入）。
func newKeysUsageTestRouter(
	pools *store.Pools,
	principal Principal,
	sessions SessionCounter,
	fixed5h Fixed5hWindowReader,
) *Router {
	router := New(Options{Deps: Deps{Guard: principalGuard{principal: principal}}})
	RegisterKeysRoutes(router, Deps{
		Guard:          principalGuard{principal: principal},
		Problems:       NewProblems(nil),
		Store:          pools,
		SessionCounts:  sessions,
		Fixed5hWindows: fixed5h,
	})
	return router
}

// ---- 真依赖（PG + Redis）----

// usageTestRedis 建一个真 Redis 脚本层与裸客户端；未设置 CCH_TEST_REDIS_URL 时跳过。
//
// 库号纪律与 limit 的集成测试一致：URL 未带库号时落在 13，不碰其它键空间。
func usageTestRedis(t *testing.T) (*ratelimit.Client, redis.UniversalClient) {
	t.Helper()
	rawURL := os.Getenv("CCH_TEST_REDIS_URL")
	if rawURL == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过 Redis 集成测试")
	}
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		t.Fatalf("解析 Redis URL 失败: %v", err)
	}
	if options.DB == 0 {
		options.DB = 13
	}
	raw := redis.NewClient(options)
	t.Cleanup(func() { _ = raw.Close() })
	if err := raw.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("Redis 不可达: %v", err)
	}
	registry, err := ratelimit.Embedded()
	if err != nil {
		t.Fatalf("装载脚本清单失败: %v", err)
	}
	client, err := ratelimit.New(raw, registry)
	if err != nil {
		t.Fatalf("组装脚本调用层失败: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, raw
}

// usageSeedSessionInfo 写入 `session:{id}:info`，模拟 Node 的 SessionManager.storeSessionInfo
// （src/lib/session-manager.ts:1810-1828）。
//
// 为什么测试必须自己写：Go 数据面目前**没有**这个写入（全仓库只有 EXPIRE/DEL 该键，见
// internal/session/tracker.go），而计数按 Node 的口径要求该键存在。这同时意味着：Go 承流的在途
// 会话在 `concurrentSessions.current` 里暂时不会被计入（与 Node 在同一 Redis 状态下的读数一致，
// 因为两边都查同一个键），那条写入补齐后本用例第二段即可改成由占位自动带出。
func usageSeedSessionInfo(t *testing.T, raw redis.UniversalClient, sessionID string) {
	t.Helper()
	key := session.InfoKey(sessionID)
	t.Cleanup(func() { _ = raw.Del(context.Background(), key).Err() })
	if err := raw.HSet(context.Background(), key, map[string]any{
		"status": "in_progress",
		"keyId":  "0",
	}).Err(); err != nil {
		t.Fatalf("写会话 info 键失败: %v", err)
	}
}

// usageFixtureRow 插一条可计费账本行（经 message_request 的触发器同步到 usage_ledger）。
//
// 不显式插 usage_ledger：库中 message_request 上有 AFTER INSERT 触发器会同步，重复插会撞
// idx_usage_ledger_request_id（与 usage_logs 集成测试同款理由）。
func usageFixtureRow(t *testing.T, pools *store.Pools, userID int64, keyValue string, cost string) {
	t.Helper()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}
	var requestID int64
	err = pool.QueryRow(context.Background(),
		`INSERT INTO message_request (
			provider_id, user_id, key, model, endpoint, status_code, cost_usd,
			session_id, session_identity, session_identity_kind, is_replay, created_at
		) VALUES (1, $1, $2, $3, '/v1/messages', 200, $4::numeric, $5, NULL, 'prefix_affinity', false, now())
		RETURNING id`,
		userID, keyValue, "go-adminapi-usage-it", cost, "go-adminapi-usage-it-session",
	).Scan(&requestID)
	if err != nil {
		t.Fatalf("插入夹具账本行失败: %v", err)
	}
	t.Cleanup(func() {
		cleanupPool, cleanupErr := pools.Control()
		if cleanupErr != nil {
			return
		}
		_, _ = cleanupPool.Exec(context.Background(), `DELETE FROM usage_ledger WHERE request_id = $1`, requestID)
		_, _ = cleanupPool.Exec(context.Background(), `DELETE FROM message_request WHERE id = $1`, requestID)
	})
}

// TestAdminKeyLimitUsageConcurrentSessionsIntegration 是本次阻塞点的验收：`concurrentSessions.current`
// 在真 Redis 上随数据面占位而变化（0 → 占位 1 → 释放 0），且 limit 取「Key 优先、User 兜底」的有效值。
func TestAdminKeyLimitUsageConcurrentSessionsIntegration(t *testing.T) {
	pools := testPools(t)
	client, raw := usageTestRedis(t)
	sessions := limit.NewSessionTracker(client, 300*time.Second, nil)

	ownerID := fixtureUser(t, pools, "admin", true)
	keyID, _ := fixtureKey(t, pools, fixtureKeyOptions{userID: ownerID, isEnabled: true})
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}
	// 本用例的 Redis 键：只清本密钥与本用户的会话索引。
	sessionZSet := limit.KeyActiveSessionsKey(keyID)
	userZSet := limit.UserActiveSessionsKey(ownerID)
	t.Cleanup(func() {
		ctx := context.Background()
		_ = raw.Del(ctx, sessionZSet, userZSet).Err()
	})

	router := newKeysUsageTestRouter(
		pools, Principal{UserID: ownerID, IsAdmin: true}, sessions, keyFixed5hWindowsForTest(client))

	// 1) 没有任何占位、也没有限额：current 0、limit 0。
	status, body, raw1 := serveKeys(t, router, http.MethodGet, keyPath(keyID, "/limit-usage"), "")
	if status != http.StatusOK {
		t.Fatalf("读档应 200，实际 %d（%s）", status, raw1)
	}
	if got := usageSessions(t, body); got["current"] != float64(0) || got["limit"] != float64(0) {
		t.Fatalf("无占位无限额时应为 current 0 / limit 0，实际 %v", got)
	}

	// 2) 占一个名额（与数据面同一条 Lua），并把 info 键补上（见 usageSeedSessionInfo 的说明）。
	const sessionID = "go-adminapi-usage-it-session-1"
	tracked, err := sessions.CheckAndTrackKeyUserSession(context.Background(), keyID, ownerID, sessionID, 1, 0)
	if err != nil || !tracked.Allowed {
		t.Fatalf("占位应成功，实际 %+v err=%v", tracked, err)
	}
	usageSeedSessionInfo(t, raw, sessionID)

	// 2a) 只有 ZSET 成员、没有 info 键时**不计入**（Node 的 countFromZSet 同款判据）。
	_ = raw.Del(context.Background(), session.InfoKey(sessionID)).Err()
	status, body, _ = serveKeys(t, router, http.MethodGet, keyPath(keyID, "/limit-usage"), "")
	if status != http.StatusOK {
		t.Fatalf("读档应 200，实际 %d", status)
	}
	if got := usageSessions(t, body)["current"]; got != float64(0) {
		t.Fatalf("缺 info 键的成员不应计入，实际 current=%v", got)
	}

	// 2b) 补回 info 键 ⇒ current 1，limit 取 Key 自身上限 1。
	usageSeedSessionInfo(t, raw, sessionID)
	status, body, _ = serveKeys(t, router, http.MethodGet, keyPath(keyID, "/limit-usage"), "")
	if status != http.StatusOK {
		t.Fatalf("读档应 200，实际 %d", status)
	}
	if got := usageSessions(t, body)["current"]; got != float64(1) {
		t.Fatalf("占位后 current 应为 1，实际 %v", got)
	}

	// 3) 释放（数据面终态清理同款调用）⇒ current 回到 0。
	if _, err := sessions.ForceTerminateKeyUserSession(context.Background(), keyID, ownerID, sessionID); err != nil {
		t.Fatalf("释放占位失败: %v", err)
	}
	status, body, _ = serveKeys(t, router, http.MethodGet, keyPath(keyID, "/limit-usage"), "")
	if status != http.StatusOK {
		t.Fatalf("读档应 200，实际 %d", status)
	}
	if got := usageSessions(t, body)["current"]; got != float64(0) {
		t.Fatalf("释放后 current 应为 0，实际 %v", got)
	}

	// 4) limit 的有效值：Key 未设时回退 User 上限；两者都无则 0（Node 的 resolveKeyConcurrentSessionLimit）。
	if _, err := pool.Exec(context.Background(),
		`UPDATE users SET limit_concurrent_sessions = 4 WHERE id = $1`, ownerID); err != nil {
		t.Fatalf("设置用户并发上限失败: %v", err)
	}
	status, body, _ = serveKeys(t, router, http.MethodGet, keyPath(keyID, "/limit-usage"), "")
	if status != http.StatusOK {
		t.Fatalf("读档应 200，实际 %d", status)
	}
	if got := usageSessions(t, body)["limit"]; got != float64(4) {
		t.Fatalf("Key 未设时应回退 User 上限 4，实际 %v", got)
	}
}

// TestAdminKeyLimitUsageCostsIntegration 覆盖五周期金额与 resetAt 的存在性：
// 金额按**密钥串**从账本聚合（传 id 会静默得到 0），滚动 5h 没有重置时刻，daily/weekly/monthly 有。
func TestAdminKeyLimitUsageCostsIntegration(t *testing.T) {
	pools := testPools(t)
	client, _ := usageTestRedis(t)
	sessions := limit.NewSessionTracker(client, 300*time.Second, nil)

	ownerID := fixtureUser(t, pools, "admin", true)
	keyID, keyValue := fixtureKey(t, pools, fixtureKeyOptions{userID: ownerID, isEnabled: true})
	usageFixtureRow(t, pools, ownerID, keyValue, "12.5")

	router := newKeysUsageTestRouter(
		pools, Principal{UserID: ownerID, IsAdmin: true}, sessions, keyFixed5hWindowsForTest(client))

	status, body, raw := serveKeys(t, router, http.MethodGet, keyPath(keyID, "/limit-usage"), "")
	if status != http.StatusOK {
		t.Fatalf("读档应 200，实际 %d（%s）", status, raw)
	}
	// 顶层恰好 6 个桶（Node 的 result.data 形状）。
	for _, name := range []string{"cost5h", "costDaily", "costWeekly", "costMonthly", "costTotal"} {
		bucket := usageBucket(t, body, name)
		if got := usageAmount(t, bucket["current"]); got != 12.5 {
			t.Fatalf("%s.current 应为 12.5（按密钥串聚合），实际 %v", name, bucket["current"])
		}
	}
	if got := usageAmount(t, usageBucket(t, body, "cost5h")["current"]); got != 12.5 {
		t.Fatalf("cost5h.current 应为 12.5，实际 %v", got)
	}

	// resetAt：滚动 5h 无（Node 给 undefined ⇒ 键不存在），daily/weekly/monthly 有。
	if _, present := usageBucket(t, body, "cost5h")["resetAt"]; present {
		t.Error("rolling 5h 不应带 resetAt 键")
	}
	for _, name := range []string{"costDaily", "costWeekly", "costMonthly"} {
		if _, present := usageBucket(t, body, name)["resetAt"]; !present {
			t.Errorf("%s 应带 resetAt", name)
		}
	}
	// costTotal 的 resetAt 取 costResetAt，未设置时同样不写该键。
	if _, present := usageBucket(t, body, "costTotal")["resetAt"]; present {
		t.Error("未设置 cost_reset_at 时 costTotal 不应带 resetAt 键")
	}

	// 设置 cost_reset_at 后 costTotal 应带上它，且金额只算重置后的账（本行的 created_at 在重置前）。
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE keys SET cost_reset_at = now() + interval '1 second' WHERE id = $1`, keyID); err != nil {
		t.Fatalf("设置 cost_reset_at 失败: %v", err)
	}
	status, body, _ = serveKeys(t, router, http.MethodGet, keyPath(keyID, "/limit-usage"), "")
	if status != http.StatusOK {
		t.Fatalf("读档应 200，实际 %d", status)
	}
	total := usageBucket(t, body, "costTotal")
	if _, present := total["resetAt"]; !present {
		t.Error("设置 cost_reset_at 后 costTotal 应带 resetAt")
	}
	if got := usageAmount(t, total["current"]); got != 0 {
		t.Fatalf("重置时刻之后的总额度应为 0（夹具行在重置前），实际 %v", got)
	}
}

// TestAdminKeyQuotaIntegration 覆盖 /quota 的 items 形状、currencyCode 与 limitSessions 的空值形态。
func TestAdminKeyQuotaIntegration(t *testing.T) {
	pools := testPools(t)
	client, _ := usageTestRedis(t)
	sessions := limit.NewSessionTracker(client, 300*time.Second, nil)

	ownerID := fixtureUser(t, pools, "admin", true)
	keyID, _ := fixtureKey(t, pools, fixtureKeyOptions{userID: ownerID, isEnabled: true})
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}
	// 限额：5h 30、daily 7.5、总数 0（0 保留成 0，与其余金额字段的真值判定不同）。
	if _, err := pool.Exec(context.Background(),
		`UPDATE keys SET limit_5h_usd = 30, limit_daily_usd = 7.5, limit_total_usd = 0,
		        daily_reset_time = '06:30', daily_reset_mode = 'fixed', limit_5h_reset_mode = 'rolling'
		 WHERE id = $1`, keyID); err != nil {
		t.Fatalf("设置密钥限额失败: %v", err)
	}

	router := newKeysUsageTestRouter(
		pools, Principal{UserID: ownerID, IsAdmin: true}, sessions, keyFixed5hWindowsForTest(client))
	status, body, raw := serveKeys(t, router, http.MethodGet, keyPath(keyID, "/quota"), "")
	if status != http.StatusOK {
		t.Fatalf("配额读档应 200，实际 %d（%s）", status, raw)
	}
	if body["keyName"] != "go-adminapi-it" {
		t.Fatalf("keyName 应为密钥名，实际 %v", body["keyName"])
	}
	if _, ok := body["currencyCode"].(string); !ok {
		t.Fatalf("currencyCode 应为字符串，实际 %v", body["currencyCode"])
	}

	items, ok := body["items"].([]any)
	if !ok || len(items) != 6 {
		t.Fatalf("items 应为 6 项，实际 %v", body["items"])
	}
	wantTypes := []string{"limit5h", "limitDaily", "limitWeekly", "limitMonthly", "limitTotal", "limitSessions"}
	for index, want := range wantTypes {
		item, ok := items[index].(map[string]any)
		if !ok {
			t.Fatalf("第 %d 项不是对象：%v", index, items[index])
		}
		if item["type"] != want {
			t.Errorf("第 %d 项的 type 应为 %s，实际 %v", index, want, item["type"])
		}
	}
	first := items[0].(map[string]any)
	if first["mode"] != "rolling" {
		t.Errorf("limit5h.mode 应为 rolling，实际 %v", first["mode"])
	}
	if usageAmount(t, first["limit"]) != 30 {
		t.Errorf("limit5h.limit 应为 30，实际 %v", first["limit"])
	}
	daily := items[1].(map[string]any)
	if daily["mode"] != "fixed" || daily["time"] != "06:30" {
		t.Errorf("limitDaily 的 mode/time 应为 fixed/06:30，实际 %v/%v", daily["mode"], daily["time"])
	}
	if usageAmount(t, daily["limit"]) != 7.5 {
		t.Errorf("limitDaily.limit 应为 7.5，实际 %v", daily["limit"])
	}
	// limitTotal 的 0 保留成 0（Node 的 !== null 判据），而不是 null。
	if items[4].(map[string]any)["limit"] != float64(0) {
		t.Errorf("limitTotal.limit 应保留 0，实际 %v", items[4].(map[string]any)["limit"])
	}
	// limitSessions.limit：有效上限为 0 时给 null（与 costTotal 的判据不同）。
	if items[5].(map[string]any)["limit"] != nil {
		t.Errorf("无并发上限时 limitSessions.limit 应为 null，实际 %v", items[5].(map[string]any)["limit"])
	}
}

// TestAdminKeyUsagePermissionAndNotFound 覆盖三条端点的权限与未找到分支（错误码逐字对齐 Node）。
func TestAdminKeyUsagePermissionAndNotFound(t *testing.T) {
	pools := testPools(t)
	client, _ := usageTestRedis(t)
	sessions := limit.NewSessionTracker(client, 300*time.Second, nil)

	ownerID := fixtureUser(t, pools, "admin", true)
	otherID := fixtureUser(t, pools, "user", true)
	keyID, _ := fixtureKey(t, pools, fixtureKeyOptions{userID: ownerID, isEnabled: true})

	// 非属主非管理员：
	//  - /limit-usage（read 档）⇒ 403 且**无** errorCode ⇒ 兜底码 key.action_failed；
	//  - /quota（admin 档，守卫会先 403，这里直接注入身份以覆盖 action 层的判定）⇒ PERMISSION_DENIED。
	router := newKeysUsageTestRouter(
		pools, Principal{UserID: otherID}, sessions, keyFixed5hWindowsForTest(client))
	status, body, _ := serveKeys(t, router, http.MethodGet, keyPath(keyID, "/limit-usage"), "")
	if status != http.StatusForbidden || body["errorCode"] != "key.action_failed" {
		t.Fatalf("非属主 /limit-usage 应 403 key.action_failed，实际 %d %v", status, body["errorCode"])
	}
	status, body, _ = serveKeys(t, router, http.MethodGet, keyPath(keyID, "/quota"), "")
	if status != http.StatusForbidden || body["errorCode"] != "PERMISSION_DENIED" {
		t.Fatalf("非属主 /quota 应 403 PERMISSION_DENIED，实际 %d %v", status, body["errorCode"])
	}

	// 不存在的密钥：两条的正文码不同（getKeyLimitUsage 无码 ⇒ key.not_found；quota 带 NOT_FOUND）。
	router = newKeysUsageTestRouter(
		pools, Principal{UserID: ownerID, IsAdmin: true}, sessions, keyFixed5hWindowsForTest(client))
	status, body, _ = serveKeys(t, router, http.MethodGet, "/api/v1/keys/2147483000/limit-usage", "")
	if status != http.StatusNotFound || body["errorCode"] != "key.not_found" {
		t.Fatalf("不存在的密钥 /limit-usage 应 404 key.not_found，实际 %d %v", status, body["errorCode"])
	}
	status, body, _ = serveKeys(t, router, http.MethodGet, "/api/v1/keys/2147483000/quota", "")
	if status != http.StatusNotFound || body["errorCode"] != "NOT_FOUND" {
		t.Fatalf("不存在的密钥 /quota 应 404 NOT_FOUND，实际 %d %v", status, body["errorCode"])
	}

	// GET /keys/{keyId}（admin 档）：正文是 { id, limitUsage }。
	status, body, raw := serveKeys(t, router, http.MethodGet, keyPath(keyID, ""), "")
	if status != http.StatusOK {
		t.Fatalf("GET /keys/{id} 应 200，实际 %d（%s）", status, raw)
	}
	if usageAmount(t, body["id"]) != float64(keyID) {
		t.Fatalf("正文 id 应为密钥 id，实际 %v", body["id"])
	}
	limitUsage, ok := body["limitUsage"].(map[string]any)
	if !ok {
		t.Fatalf("正文应含 limitUsage 对象，实际 %v", body)
	}
	if _, ok := limitUsage["concurrentSessions"]; !ok {
		t.Fatalf("limitUsage 应含 concurrentSessions，实际 %v", limitUsage)
	}
}

// keyFixed5hWindowsForTest 把成本窗口层适配成管理面接口（与 cmd/cchd 的 keyFixed5hReader 同构）。
func keyFixed5hWindowsForTest(client *ratelimit.Client) Fixed5hWindowReader {
	return fixed5hAdapter{windows: limit.NewCostWindows(client, nil)}
}

type fixed5hAdapter struct{ windows *limit.CostWindows }

func (a fixed5hAdapter) Fixed5hWindowState(
	ctx context.Context, keyID int64, now time.Time,
) (limit.Fixed5hState, error) {
	return a.windows.Fixed5hWindowState(ctx, limit.EntityKey, keyID, now)
}

// usageSessions 取正文里的 concurrentSessions。
func usageSessions(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	sessions, ok := body["concurrentSessions"].(map[string]any)
	if !ok {
		t.Fatalf("正文应含 concurrentSessions 对象，实际 %v", body)
	}
	return sessions
}

// usageBucket 取正文里的金额桶。
func usageBucket(t *testing.T, body map[string]any, name string) map[string]any {
	t.Helper()
	bucket, ok := body[name].(map[string]any)
	if !ok {
		t.Fatalf("正文应含 %s 对象，实际 %v", name, body)
	}
	return bucket
}

// usageAmount 把响应里的数值（json.Number 或 float64）转成 float64。
func usageAmount(t *testing.T, value any) float64 {
	t.Helper()
	switch typed := value.(type) {
	case float64:
		return typed
	case json.Number:
		parsed, err := strconv.ParseFloat(typed.String(), 64)
		if err != nil {
			t.Fatalf("数值 %q 不可解析: %v", typed.String(), err)
		}
		return parsed
	case int64:
		return float64(typed)
	default:
		t.Fatalf("不支持的值类型 %T：%v", value, value)
		return 0
	}
}
