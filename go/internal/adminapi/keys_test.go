package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住 A1-2 的两件事：
//  1. 哪些 keys 路由**被注册**、哪些**故意不注册**——漏注册是静默回退 Node，只能靠测试发现
//     （deps.go 文件头第 2 条）。这张表就是本 lane 的范围契约。
//  2. 已注册的三条在真库上真的改对了行、答对了信封。

// principalGuard 把固定身份注入上下文：本 lane 测的是资源处理器，不是守卫（守卫由 A0-2 覆盖）。
type principalGuard struct{ principal Principal }

func (g principalGuard) Wrap(_ AccessLevel, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		next.ServeHTTP(writer, request.WithContext(WithPrincipal(request.Context(), g.principal)))
	})
}

// recordingAudit 收集审计事件（Emit 是 fire-and-forget，测试里同步收集）。
type recordingAudit struct{ events []AuditEvent }

func (a *recordingAudit) Emit(_ context.Context, event AuditEvent) {
	a.events = append(a.events, event)
}

// recordingInvalidator 收集失效广播。
type recordingInvalidator struct {
	keyAuth  []string
	keyCost  []int64
	userAuth []int64
	userCost []int64
}

func (i *recordingInvalidator) InvalidateKeyAuth(_ context.Context, apiKey string) {
	i.keyAuth = append(i.keyAuth, apiKey)
}

func (i *recordingInvalidator) InvalidateUserAuth(_ context.Context, userID int64) {
	i.userAuth = append(i.userAuth, userID)
}

func (i *recordingInvalidator) InvalidateKeyCost(_ context.Context, keyID int64) {
	i.keyCost = append(i.keyCost, keyID)
}

func (i *recordingInvalidator) InvalidateUserCost(_ context.Context, userID int64) {
	i.userCost = append(i.userCost, userID)
}

func (i *recordingInvalidator) PublishDomain(context.Context, cfgsync.Domain) {}

// TestRegisterKeysRoutesRegistersImplemented 是本 lane 的范围契约。
//
// 本条只看**基础依赖**（无 Redis 运行态）时的注册面：那时必须是 11 条——需 Redis 运行态的三条读档
// 路由缺依赖就不注册（它们会原样回退 Node），这是文件头那条「宁可不答，不可乱答」的落地。
// 两个运行态依赖都装配时的 14 条见 TestRegisterKeysRoutesWithRuntimeDeps。
func TestRegisterKeysRoutesRegistersImplemented(t *testing.T) {
	router := New(Options{Deps: Deps{Guard: &recordingGuard{}}})
	RegisterKeysRoutes(router, Deps{Guard: &recordingGuard{}, Store: &store.Pools{}})

	want := []struct {
		method      string
		path        string
		access      AccessLevel
		operationID string
	}{
		{http.MethodGet, "/users/{userId:[0-9]+}/keys", AccessAdmin, "listUserKeys"},
		{http.MethodPost, "/users/{userId:[0-9]+}/keys", AccessAdmin, "createUserKey"},
		{http.MethodPost, "/users:self/keys", AccessRead, "createSelfKey"},
		{http.MethodPost, "/keys/{keyId:[0-9]+}:enable", AccessRead, "enableKey"},
		{http.MethodPost, "/keys/{keyId:[0-9]+}:renew", AccessRead, "renewKey"},
		{http.MethodPatch, "/keys/{keyId:[0-9]+}", AccessRead, "updateKey"},
		{http.MethodDelete, "/keys/{keyId:[0-9]+}", AccessRead, "deleteKey"},
		{http.MethodPost, "/keys:batchUpdate", AccessAdmin, "batchUpdateKeys"},
		{http.MethodGet, "/keys/{keyId:[0-9]+}:reveal", AccessRead, "revealKey"},
		{http.MethodPost, "/keys/{keyId:[0-9]+}/limits:reset", AccessAdmin, "resetKeyLimits"},
		{
			http.MethodPatch,
			"/keys/{keyId:[0-9]+}/limits/{field:" + keyLimitFields + "}",
			AccessAdmin,
			"patchKeyLimit",
		},
	}
	routes := router.RouteList()
	if len(routes) != len(want) {
		t.Fatalf("注册条数应为 %d，实际 %d：%+v", len(want), len(routes), routeKeys(routes))
	}
	for index, expected := range want {
		route := routes[index]
		if route.Method != expected.method || route.Path != expected.path {
			t.Errorf("第 %d 条应为 %s %s，实际 %s %s", index, expected.method, expected.path, route.Method, route.Path)
		}
		if route.Access != expected.access {
			t.Errorf("%s %s 的档位应为 %s，实际 %s", route.Method, route.Path, expected.access, route.Access)
		}
		if route.OperationID != expected.operationID {
			t.Errorf("%s %s 的 operationId 应为 %s，实际 %s", route.Method, route.Path, expected.operationID, route.OperationID)
		}
		if route.Module != "keys" {
			t.Errorf("%s %s 的 module 应为 keys，实际 %s", route.Method, route.Path, route.Module)
		}
	}

	// 缺 Redis 运行态依赖时，三条读档路由必须一条都不在表里（它们需要真会话计数与 5h 运行态窗口）。
	runtimeDepBlocked := []struct{ method, path string }{
		{http.MethodGet, "/keys/1234"},
		{http.MethodGet, "/keys/1234/limit-usage"},
		{http.MethodGet, "/keys/1234/quota"},
	}
	for _, item := range runtimeDepBlocked {
		if matched := matchPath(t, router, item.method, item.path); matched != "" {
			t.Errorf("%s %s 需要 Redis 运行态依赖，缺依赖时不应注册（应回退 Node），却命中了 %s",
				item.method, item.path, matched)
		}
	}

	// 已注册的每条都必须真的能被匹配到（而不是被权重规则挤掉）。
	matched := []struct{ method, path string }{
		{http.MethodGet, "/users/1234/keys"},
		{http.MethodPost, "/users/1234/keys"},
		{http.MethodPost, "/users:self/keys"},
		{http.MethodPost, "/keys/1234:enable"},
		{http.MethodPost, "/keys/1234:renew"},
		{http.MethodPatch, "/keys/1234"},
		{http.MethodDelete, "/keys/1234"},
		{http.MethodPost, "/keys:batchUpdate"},
		{http.MethodGet, "/keys/1234:reveal"},
		{http.MethodPost, "/keys/1234/limits:reset"},
		{http.MethodPatch, "/keys/1234/limits/limit5hUsd"},
	}
	for _, item := range matched {
		if hit := matchPath(t, router, item.method, item.path); hit == "" {
			t.Errorf("%s %s 应命中已注册路由，实际回退 Node", item.method, item.path)
		}
	}
}

// TestKeyLimitFieldRouteRejectsUnknownField 钉住 `{field}` 的枚举收缩：不在枚举内的字段
// 不匹配路由 ⇒ 回退 Node ⇒ 由 Node 用它自己的 zod 信封作答（Go 不复刻 invalidParams）。
func TestKeyLimitFieldRouteRejectsUnknownField(t *testing.T) {
	router := New(Options{Deps: Deps{Guard: &recordingGuard{}}})
	RegisterKeysRoutes(router, Deps{Guard: &recordingGuard{}, Store: &store.Pools{}})

	if matched := matchPath(t, router, http.MethodPatch, "/keys/1234/limits/providerGroup"); matched != "" {
		t.Fatalf("未知字段不应命中路由，实际命中 %s", matched)
	}
}

// matchPath 返回命中的路由描述（方法+模式），未命中返回空串。
func matchPath(t *testing.T, router *Router, method, path string) string {
	t.Helper()
	for _, route := range router.RouteList() {
		if route.Method != method {
			continue
		}
		compiled, err := compileRouteForTest(route)
		if err != nil {
			t.Fatalf("编译路由 %s 失败: %v", route.Path, err)
		}
		if _, matched := compiled.match(path); matched {
			return route.Method + " " + route.Path
		}
	}
	return ""
}

// compileRouteForTest 复用生产的路径编译，保证测试与择路用同一份语法。
func compileRouteForTest(route Route) (compiledRoute, error) {
	segments, specificity, err := compilePath(route.Path)
	if err != nil {
		return compiledRoute{}, err
	}
	return compiledRoute{route: route, segments: segments, specificity: specificity}, nil
}

// routeKeys 供失败信息使用。
func routeKeys(routes []Route) []string {
	keys := make([]string, 0, len(routes))
	for _, route := range routes {
		keys = append(keys, route.Method+" "+route.Path)
	}
	return keys
}

// ---- 真库集成（门控 CCH_TEST_DSN）----

// newKeysTestRouter 装配一个只注册 keys 三条路由的路由表，身份由 principal 注入。
//
// 守卫必须装在 **Router 自己的 Deps** 上：Router.Add 用 r.deps.Guard 包装处理器，注册函数收到的
// Deps.Guard 不参与包装（见 router.go 的 Add）。
func newKeysTestRouter(pools *store.Pools, principal Principal, audit AuditSink, invalidator Invalidator) *Router {
	router := New(Options{Deps: Deps{Guard: principalGuard{principal: principal}}})
	RegisterKeysRoutes(router, Deps{
		Guard:       principalGuard{principal: principal},
		Problems:    NewProblems(nil),
		Audit:       audit,
		Invalidator: invalidator,
		Store:       pools,
	})
	return router
}

// serveKeys 发一次请求，返回状态码与响应体。
func serveKeys(t *testing.T, router *Router, method, target, body string) (int, map[string]any, string) {
	t.Helper()
	var request *http.Request
	if body == "" {
		request = httptest.NewRequest(method, target, nil)
	} else {
		request = httptest.NewRequest(method, target, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("User-Agent", "go-adminapi-it/1.0")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	payload := map[string]any{}
	raw := recorder.Body.String()
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			t.Fatalf("响应不是 JSON：%q（状态 %d）", raw, recorder.Code)
		}
	}
	return recorder.Code, payload, raw
}

// TestAdminKeyRevealIntegration 覆盖 getUnmaskedKey 的三条分支与审计落点。
func TestAdminKeyRevealIntegration(t *testing.T) {
	pools := testPools(t)
	ownerID := fixtureUser(t, pools, "user", true)
	otherID := fixtureUser(t, pools, "user", true)
	keyID, keyValue := fixtureKey(t, pools, fixtureKeyOptions{userID: ownerID, isEnabled: true})

	audit := &recordingAudit{}

	// 属主可读明文。
	router := newKeysTestRouter(pools, Principal{UserID: ownerID, IsAdmin: false}, audit, nil)
	status, body, _ := serveKeys(t, router, http.MethodGet, keyPath(keyID, ":reveal"), "")
	if status != http.StatusOK {
		t.Fatalf("属主读取明文应 200，实际 %d 与 %s", status, strings.Join(bodyKeys(body), ","))
	}
	if body["key"] != keyValue {
		t.Fatalf("明文不符：期望 %q，实际 %v", keyValue, body["key"])
	}
	if len(audit.events) != 1 || audit.events[0].Action != "key.key_reveal" || !audit.events[0].Success {
		t.Fatalf("应写一条成功的 key.key_reveal 审计，实际 %+v", audit.events)
	}
	if audit.events[0].TargetName == "" || audit.events[0].TargetID == "" {
		t.Fatalf("审计目标名与目标 id 不应为空：%+v", audit.events[0])
	}

	// 非属主非管理员 403 key.action_failed（Node 无 errorCode ⇒ 兜底码）。
	router = newKeysTestRouter(pools, Principal{UserID: otherID}, &recordingAudit{}, nil)
	status, body, _ = serveKeys(t, router, http.MethodGet, keyPath(keyID, ":reveal"), "")
	if status != http.StatusForbidden {
		t.Fatalf("非属主应 403，实际 %d", status)
	}
	if body["errorCode"] != "key.action_failed" {
		t.Fatalf("403 的 errorCode 应为 key.action_failed，实际 %v", body["errorCode"])
	}

	// 管理员可读任意密钥。
	router = newKeysTestRouter(pools, Principal{UserID: 999999, IsAdmin: true}, &recordingAudit{}, nil)
	status, _, _ = serveKeys(t, router, http.MethodGet, keyPath(keyID, ":reveal"), "")
	if status != http.StatusOK {
		t.Fatalf("管理员应 200，实际 %d", status)
	}

	// 不存在的密钥 404 KEY_NOT_FOUND，并写一条失败审计。
	failAudit := &recordingAudit{}
	// 非数字 id（Node 侧由 zod coerce 拦下）：Go 侧压根不命中路由，故走兜底（未装配时 503）。
	router = newKeysTestRouter(pools, Principal{UserID: ownerID}, failAudit, nil)
	status, _, _ = serveKeys(t, router, http.MethodGet, "/api/v1/keys/abc:reveal", "")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("非数字 id 不应命中路由，实际 %d", status)
	}
	if len(failAudit.events) != 0 {
		t.Fatalf("未命中路由不应产生审计，实际 %+v", failAudit.events)
	}

	router = newKeysTestRouter(pools, Principal{UserID: ownerID}, failAudit, nil)
	status, body, _ = serveKeys(t, router, http.MethodGet, "/api/v1/keys/2147483000:reveal", "")
	if status != http.StatusNotFound {
		t.Fatalf("不存在的密钥应 404，实际 %d", status)
	}
	if body["errorCode"] != "KEY_NOT_FOUND" {
		t.Fatalf("404 的 errorCode 应为 KEY_NOT_FOUND，实际 %v", body["errorCode"])
	}
	if len(failAudit.events) != 1 || failAudit.events[0].Success {
		t.Fatalf("应写一条失败审计，实际 %+v", failAudit.events)
	}
}

// TestAdminKeyLimitsResetIntegration 覆盖 resetKeyLimitsOnly：写 cost_reset_at、清成本缓存、204。
func TestAdminKeyLimitsResetIntegration(t *testing.T) {
	pools := testPools(t)
	ownerID := fixtureUser(t, pools, "admin", true)
	keyID, _ := fixtureKey(t, pools, fixtureKeyOptions{userID: ownerID, isEnabled: true})

	invalidator := &recordingInvalidator{}
	router := newKeysTestRouter(pools, Principal{UserID: ownerID, IsAdmin: true}, &recordingAudit{}, invalidator)

	status, _, raw := serveKeys(t, router, http.MethodPost, keyPath(keyID, "/limits:reset"), "")
	if status != http.StatusNoContent {
		t.Fatalf("重置限额应 204，实际 %d（%s）", status, raw)
	}
	if strings.TrimSpace(raw) != "" {
		t.Fatalf("204 不应带正文，实际 %q", raw)
	}
	if !reflect.DeepEqual(invalidator.keyCost, []int64{keyID}) {
		t.Fatalf("应清一次该 key 的成本缓存，实际 %+v", invalidator.keyCost)
	}

	var costResetAt *string
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}
	if err := pool.QueryRow(context.Background(),
		`SELECT cost_reset_at::text FROM keys WHERE id = $1`, keyID).Scan(&costResetAt); err != nil {
		t.Fatalf("读 cost_reset_at 失败: %v", err)
	}
	if costResetAt == nil {
		t.Fatal("cost_reset_at 应被写入")
	}

	// 不存在的密钥：404，且没有失效广播。
	empty := &recordingInvalidator{}
	router = newKeysTestRouter(pools, Principal{UserID: ownerID, IsAdmin: true}, &recordingAudit{}, empty)
	if status, _, _ = serveKeys(t, router, http.MethodPost, "/api/v1/keys/2147483000/limits:reset", ""); status != http.StatusNotFound {
		t.Fatalf("不存在的密钥应 404，实际 %d", status)
	}
	if len(empty.keyCost) != 0 {
		t.Fatalf("未命中行不应广播失效，实际 %+v", empty.keyCost)
	}
}

// TestAdminKeyLimitPatchIntegration 覆盖 patchKeyLimit：写值、清空、并发列、超用户限额、坏输入。
func TestAdminKeyLimitPatchIntegration(t *testing.T) {
	pools := testPools(t)
	ownerID := fixtureUser(t, pools, "admin", true)
	keyID, keyValue := fixtureKey(t, pools, fixtureKeyOptions{userID: ownerID, isEnabled: true})

	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}
	// 用户限额：5 小时 100、并发 10（用于「不得超过用户限额」的判定）。
	if _, err := pool.Exec(context.Background(),
		`UPDATE users SET limit_5h_usd = 100, limit_concurrent_sessions = 10 WHERE id = $1`,
		ownerID); err != nil {
		t.Fatalf("设置用户限额失败: %v", err)
	}

	invalidator := &recordingInvalidator{}
	audit := &recordingAudit{}
	router := newKeysTestRouter(pools, Principal{UserID: ownerID, IsAdmin: true}, audit, invalidator)

	// 1) 写金额。
	status, body, _ := serveKeys(t, router, http.MethodPatch, limitPath(keyID, "limit5hUsd"), `{"value":12.34}`)
	if status != http.StatusOK || body["ok"] != true {
		t.Fatalf("写限额应 200 {\"ok\":true}，实际 %d %v", status, body)
	}
	if got := readKeyColumn(t, pool, keyID, "limit_5h_usd"); got != "12.34" {
		t.Fatalf("limit_5h_usd 应为 12.34，实际 %q", got)
	}
	if !reflect.DeepEqual(invalidator.keyAuth, []string{keyValue}) {
		t.Fatalf("应清一次该 key 的认证缓存，实际 %+v", invalidator.keyAuth)
	}
	if len(audit.events) != 1 || audit.events[0].Action != "key.update" || audit.events[0].Details["limit5hUsd"] == nil {
		t.Fatalf("应写一条带 after 值的 key.update 审计，实际 %+v", audit.events)
	}

	// 2) 清空金额（null ⇒ NULL）。
	if status, _, _ = serveKeys(t, router, http.MethodPatch, limitPath(keyID, "limit5hUsd"), `{"value":null}`); status != http.StatusOK {
		t.Fatalf("清空限额应 200，实际 %d", status)
	}
	if got := readKeyColumn(t, pool, keyID, "limit_5h_usd"); got != "" {
		t.Fatalf("清空后 limit_5h_usd 应为 NULL，实际 %q", got)
	}

	// 3) 并发列（整数）。
	if status, _, _ = serveKeys(t, router, http.MethodPatch, limitPath(keyID, "limitConcurrentSessions"), `{"value":7}`); status != http.StatusOK {
		t.Fatalf("写并发上限应 200，实际 %d", status)
	}
	if got := readKeyColumn(t, pool, keyID, "limit_concurrent_sessions"); got != "7" {
		t.Fatalf("limit_concurrent_sessions 应为 7，实际 %q", got)
	}

	// 4) 超过用户限额 ⇒ 400 + key.action_failed（Node 无 errorCode）。
	status, body, _ = serveKeys(t, router, http.MethodPatch, limitPath(keyID, "limit5hUsd"), `{"value":101}`)
	if status != http.StatusBadRequest {
		t.Fatalf("超用户限额应 400，实际 %d", status)
	}
	if body["errorCode"] != "key.action_failed" {
		t.Fatalf("400 的 errorCode 应为 key.action_failed，实际 %v", body["errorCode"])
	}

	// 5) 并发列传 null 与小数 ⇒ 400 校验信封。
	if status, _, _ = serveKeys(t, router, http.MethodPatch, limitPath(keyID, "limitConcurrentSessions"), `{"value":null}`); status != http.StatusBadRequest {
		t.Fatalf("并发上限传 null 应 400，实际 %d", status)
	}
	status, body, _ = serveKeys(t, router, http.MethodPatch, limitPath(keyID, "limitConcurrentSessions"), `{"value":1.5}`)
	if status != http.StatusBadRequest || body["errorCode"] != "request.validation_failed" {
		t.Fatalf("小数并发上限应 400 request.validation_failed，实际 %d %v", status, body["errorCode"])
	}

	// 6) 多传字段（.strict()）与非数字 ⇒ 400。
	if status, _, _ = serveKeys(t, router, http.MethodPatch, limitPath(keyID, "limit5hUsd"), `{"value":1,"extra":2}`); status != http.StatusBadRequest {
		t.Fatalf("多传字段应 400，实际 %d", status)
	}
	if status, _, _ = serveKeys(t, router, http.MethodPatch, limitPath(keyID, "limit5hUsd"), `{"value":"abc"}`); status != http.StatusBadRequest {
		t.Fatalf("非数字应 400，实际 %d", status)
	}

	// 7) 不存在的密钥 ⇒ 404 KEY_NOT_FOUND。
	status, body, _ = serveKeys(t, router, http.MethodPatch, "/api/v1/keys/2147483000/limits/limit5hUsd", `{"value":1}`)
	if status != http.StatusNotFound || body["errorCode"] != "KEY_NOT_FOUND" {
		t.Fatalf("不存在的密钥应 404 KEY_NOT_FOUND，实际 %d %v", status, body["errorCode"])
	}
}

// keyPath 生成 `/api/v1/keys/<id><suffix>`。
func keyPath(keyID int64, suffix string) string {
	return "/api/v1/keys/" + strconv.FormatInt(keyID, 10) + suffix
}

// limitPath 生成 `/api/v1/keys/<id>/limits/<field>`。
func limitPath(keyID int64, field string) string {
	return keyPath(keyID, "/limits/"+field)
}

// readKeyColumn 读回 keys 表的一列文本（NULL 返回空串）。
func readKeyColumn(t *testing.T, pool *store.Pool, keyID int64, column string) string {
	t.Helper()
	var value *string
	if err := pool.QueryRow(context.Background(),
		"SELECT "+column+"::text FROM keys WHERE id = $1", keyID).Scan(&value); err != nil {
		t.Fatalf("读列 %s 失败: %v", column, err)
	}
	if value == nil {
		return ""
	}
	return *value
}

// bodyKeys 供失败信息使用。
func bodyKeys(body map[string]any) []string {
	keys := make([]string, 0, len(body))
	for key := range body {
		keys = append(keys, key)
	}
	return keys
}
