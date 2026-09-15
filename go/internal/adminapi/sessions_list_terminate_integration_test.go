package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/session"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 sessions 资源**新增三条**（GET /sessions、DELETE /sessions/{id}、
// POST /sessions:batchTerminate）的测试：路由契约 + 真实依赖集成（真 PG + 真 Redis）。
//
// 为什么非真依赖不可：
//   - 列表页的「并发数」与「活跃/非活跃」分流分别读 `observed_session:{id}:concurrent_count`
//     与 `session:{id}:info`，桩件只能证明调了哪条命令，证不了键名与口径。
//   - 终止是**一次动作五段**（绑定 CAS、响应体 bundle、元数据、四组索引、观测集合），
//     必须用真 Redis 断言「终止后一个都不剩」——只测返回值会漏掉悬空索引。
//
// 夹具纪律与仓库既有测试一致：自钉唯一 model/identity 前缀、按精确键名与 id 清理，
// 共享 ZSET 只摘自己的成员（绝不整键删除）。

// sessionsFullDeps 是本文件用到的依赖集合（真 PG + 真 Redis）。
type sessionsFullDeps struct {
	Pools      *store.Pools
	Redis      redis.UniversalClient
	Binder     *session.Binder
	Adapter    *session.SessionBinderAdapter
	Observed   SessionObservationReader
	Terminator SessionTerminator
	Affinity   SessionAffinityInvalidator
	Artifacts  SessionArtifactReader
}

// openSessionsDeps 建真依赖；未设置门控变量时跳过。
func openSessionsDeps(t *testing.T) sessionsFullDeps {
	t.Helper()
	pools := meOpenPools(t)
	scriptClient, raw := usageTestRedis(t)
	binder := session.NewBinder(scriptClient)
	return sessionsFullDeps{
		Pools:      pools,
		Redis:      raw,
		Binder:     binder,
		Adapter:    session.NewSessionBinderAdapter(session.BinderOptions{Client: binder}),
		Observed:   binder,
		Terminator: binder,
		Affinity:   affinityTestStore(raw),
		Artifacts:  binder,
	}
}

// affinityTestStore 建一个真亲和存储（同库的真 Redis）。窗口与 TTL 只影响 Lookup/Put，
// 终止路径只用 Invalidate（代际围栏），故取 Node 的默认值即可。
func affinityTestStore(raw redis.UniversalClient) SessionAffinityInvalidator {
	return route.NewAffinityStore(route.AffinityOptions{
		Redis: raw, Window: 5, SlidingTTLSeconds: 300,
	})
}

// sessionsRouterWithDeps 以该身份与依赖建路由表（覆盖 sessions 资源的全部已注册端点）。
func sessionsRouterWithDeps(
	t *testing.T,
	pools *store.Pools,
	principal Principal,
	deps sessionsFullDeps,
) *Router {
	t.Helper()
	router := New(Options{Deps: Deps{Guard: principalGuard{principal: principal}}})
	RegisterSessionsRoutes(router, Deps{
		Guard:               principalGuard{principal: principal},
		Problems:            NewProblems(nil),
		Store:               pools,
		SessionObservations: deps.Observed,
		SessionTerminations: deps.Terminator,
		SessionAffinity:     deps.Affinity,
		SessionArtifacts:    deps.Artifacts,
	})
	return router
}

// sessionsRequest 发一次请求并解出 JSON 对象（空体返回 nil 映射）。
func sessionsRequest(
	t *testing.T,
	router *Router,
	method string,
	target string,
	body string,
) (int, map[string]any, string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	var request *http.Request
	if body == "" {
		request = httptest.NewRequest(method, target, nil)
	} else {
		request = httptest.NewRequest(method, target, strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
	}
	router.ServeHTTP(recorder, request)
	raw := recorder.Body.String()
	var parsed map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &parsed); err != nil {
		return recorder.Code, nil, raw
	}
	return recorder.Code, parsed, raw
}

// TestRegisterSessionsRoutesWithDependencies 钉住新增三条的注册条件与档位。
//
// 分三档断言：只装配观测读数 → 只多列表页；再加终止器与亲和失效器 → 两条终止一并出现。
// 「终止器或亲和失效器缺一即两条终止都不注册」是刻意的（见 sessions_terminate.go 文件头）。
func TestRegisterSessionsRoutesWithDependencies(t *testing.T) {
	guardStub := &recordingGuard{}
	cases := []struct {
		name       string
		deps       Deps
		wantRoutes []string
	}{
		{
			name: "只有 Store",
			deps: Deps{Store: &store.Pools{}},
			wantRoutes: []string{
				http.MethodGet + " /sessions/{sessionId}/requests",
				http.MethodGet + " /sessions/{sessionId}/origin-chain",
			},
		},
		{
			name: "加观测读数",
			deps: Deps{Store: &store.Pools{}, SessionObservations: stubObservations{}},
			wantRoutes: []string{
				http.MethodGet + " /sessions/{sessionId}/requests",
				http.MethodGet + " /sessions/{sessionId}/origin-chain",
				http.MethodGet + " /sessions",
			},
		},
		{
			name: "只有终止器（缺亲和）",
			deps: Deps{Store: &store.Pools{}, SessionTerminations: stubTerminator{}},
			wantRoutes: []string{
				http.MethodGet + " /sessions/{sessionId}/requests",
				http.MethodGet + " /sessions/{sessionId}/origin-chain",
			},
		},
		{
			name: "全装配",
			deps: Deps{
				Store:               &store.Pools{},
				SessionObservations: stubObservations{},
				SessionTerminations: stubTerminator{},
				SessionAffinity:     stubAffinity{},
			},
			wantRoutes: []string{
				http.MethodGet + " /sessions/{sessionId}/requests",
				http.MethodGet + " /sessions/{sessionId}/origin-chain",
				http.MethodGet + " /sessions",
				http.MethodDelete + " /sessions/{sessionId}",
				http.MethodPost + " /sessions:batchTerminate",
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			router := New(Options{Deps: Deps{Guard: guardStub}})
			deps := testCase.deps
			deps.Guard = guardStub
			deps.Problems = NewProblems(nil)
			RegisterSessionsRoutes(router, deps)

			routes := router.RouteList()
			if len(routes) != len(testCase.wantRoutes) {
				t.Fatalf("应注册 %d 条，实际 %d：%v",
					len(testCase.wantRoutes), len(routes), auditLogRouteKeys(router))
			}
			for index, route := range routes {
				if route.Method+" "+route.Path != testCase.wantRoutes[index] {
					t.Errorf("第 %d 条应为 %s，实际 %s %s",
						index, testCase.wantRoutes[index], route.Method, route.Path)
				}
				// 三条新增与既有那一条同为 read 档（Node 的 requireAuth("read")）。
				if route.Access != AccessRead {
					t.Errorf("%s 档位应为 read，实际 %s", route.Path, route.Access)
				}
			}
		})
	}
}

// seededListSession 已移除：列表用例直接用 identity 字符串与 bySessionID 映射断言。

// TestSessionsListOnRealDependencies 钉住列表页的形状、并发数、所有权过滤与活跃分流。
func TestSessionsListOnRealDependencies(t *testing.T) {
	deps := openSessionsDeps(t)
	ctx := context.Background()
	userA := fixtureUser(t, deps.Pools, "user", true)
	userB := fixtureUser(t, deps.Pools, "user", true)
	_, keyValue := fixtureKey(t, deps.Pools, fixtureKeyOptions{
		userID: userA, canLoginWebUI: true, isEnabled: true,
	})
	_, keyValueB := fixtureKey(t, deps.Pools, fixtureKeyOptions{
		userID: userB, canLoginWebUI: true, isEnabled: true,
	})
	nonce := strconv.FormatInt(time.Now().UnixNano(), 10)
	providerID := fixtureProviderID(t, deps.Pools)

	// 四个会话：A 三个（其中 -3 是「十分钟前最后一次请求」的非活跃），B 一个。
	recent := "sess-list-" + nonce + "-1"
	withConcurrent := "sess-list-" + nonce + "-2"
	stale := "sess-list-" + nonce + "-3"
	foreign := "sess-list-" + nonce + "-4"

	seedListSessionRow(t, deps.Pools, userA, keyValue, recent, providerID, 0)
	seedListSessionRow(t, deps.Pools, userA, keyValue, withConcurrent, providerID, 0)
	// -3 与 -4：最后一次请求在 10 分钟前 → 只能靠并发数进活跃组。
	seedListSessionRow(t, deps.Pools, userA, keyValue, stale, providerID, 10*time.Minute)
	seedListSessionRow(t, deps.Pools, userB, keyValueB, foreign, providerID, 10*time.Minute)

	// 观测集合与 info 键：列表页只认「ZSET 成员 + info 键仍在」的会话。
	now := time.Now().UnixMilli()
	pipe := deps.Redis.Pipeline()
	for _, identity := range []string{recent, withConcurrent, stale, foreign} {
		pipe.ZAdd(ctx, session.ObservedGlobalActiveSessionsKey(),
			redis.Z{Score: float64(now), Member: identity})
		pipe.HSet(ctx, "session:"+identity+":info", "userId", "1")
	}
	pipe.Set(ctx, session.ObservedConcurrentCountKey(withConcurrent), "3", 0)
	if _, err := pipe.Exec(ctx); err != nil {
		t.Fatalf("写观测夹具失败: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = deps.Redis.ZRem(cleanupCtx, session.ObservedGlobalActiveSessionsKey(),
			recent, withConcurrent, stale, foreign).Err()
		for _, identity := range []string{recent, withConcurrent, stale, foreign} {
			_ = deps.Redis.Del(cleanupCtx,
				"session:"+identity+":info",
				session.ObservedConcurrentCountKey(identity)).Err()
		}
	})

	admin := Principal{UserID: userA, IsAdmin: true}
	router := sessionsRouterWithDeps(t, deps.Pools, admin, deps)

	// 1. ?state=active（默认）：{items: [...]}，含并发数与状态。
	status, body, raw := sessionsRequest(t, router, http.MethodGet, "/sessions", "")
	if status != http.StatusOK {
		t.Fatalf("列表应为 200，实际 %d：%s", status, raw)
	}
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("响应应含 items 数组，实际：%s", raw)
	}
	bySessionID := map[string]map[string]any{}
	for _, item := range items {
		row, isObject := item.(map[string]any)
		if !isObject {
			t.Fatalf("items 的成员应为对象，实际 %T", item)
		}
		bySessionID[row["sessionId"].(string)] = row
	}
	for _, identity := range []string{recent, withConcurrent, stale, foreign} {
		if _, present := bySessionID[identity]; !present {
			t.Fatalf("管理员应看到会话 %s，实际列表：%v", identity, raw)
		}
	}

	concurrent := bySessionID[withConcurrent]
	if concurrent["concurrentCount"].(float64) != 3 || concurrent["status"] != "in_progress" {
		t.Errorf("%s 应为 in_progress/3 并发，实际 %v/%v",
			withConcurrent, concurrent["status"], concurrent["concurrentCount"])
	}
	if bySessionID[recent]["status"] != "completed" {
		t.Errorf("无并发数的会话状态应为 completed，实际 %v", bySessionID[recent]["status"])
	}
	// 键集与类型：这四行是本条命令的形状契约。
	for _, field := range []string{
		"sessionId", "sessionIdentityKind", "sessionFingerprint", "userName", "userId", "keyId",
		"keyName", "providerId", "providerName", "model", "apiType", "startTime", "inputTokens",
		"outputTokens", "cacheCreationInputTokens", "cacheReadInputTokens", "totalTokens",
		"costUsd", "status", "durationMs", "requestCount", "concurrentCount",
	} {
		if _, present := concurrent[field]; !present {
			t.Errorf("行内应含字段 %s，实际：%v", field, concurrent)
		}
	}
	if _, isNumber := concurrent["startTime"].(float64); !isNumber {
		t.Errorf("startTime 应为数字（毫秒），实际 %T", concurrent["startTime"])
	}
	if _, isString := concurrent["costUsd"].(string); !isString {
		t.Errorf("costUsd 应为字符串（numeric 文本），实际 %T", concurrent["costUsd"])
	}
	if concurrent["sessionFingerprint"] != nil {
		t.Errorf("session_id 会话的 sessionFingerprint 应为 null，实际 %v",
			concurrent["sessionFingerprint"])
	}
	// 夹具行带真 provider_id：列表页的 providerId/providerName 因此有值
	// （Node 对空名回 "Provider #N"）。
	if concurrent["providerId"] == nil || concurrent["providerName"] == nil {
		t.Errorf("有 provider_id 的行应给出 providerId/providerName，实际 %v/%v",
			concurrent["providerId"], concurrent["providerName"])
	}
	if concurrent["apiType"] != "chat" {
		t.Errorf("apiType 兜底应为 chat，实际 %v", concurrent["apiType"])
	}
	if concurrent["totalTokens"].(float64) !=
		concurrent["inputTokens"].(float64)+concurrent["outputTokens"].(float64)+
			concurrent["cacheCreationInputTokens"].(float64)+
			concurrent["cacheReadInputTokens"].(float64) {
		t.Errorf("totalTokens 应为四分项之和，实际 %v", concurrent)
	}

	// 2. 普通用户：看不到别人的会话（内存过滤，与 Node 同）。
	memberRouter := sessionsRouterWithDeps(t, deps.Pools, Principal{UserID: userA}, deps)
	status, body, raw = sessionsRequest(t, memberRouter, http.MethodGet, "/sessions", "")
	if status != http.StatusOK {
		t.Fatalf("普通用户列表应为 200，实际 %d：%s", status, raw)
	}
	for _, item := range body["items"].([]any) {
		row := item.(map[string]any)
		if row["sessionId"] == foreign {
			t.Fatalf("普通用户不应看到他人会话：%s", raw)
		}
		if row["userId"].(float64) != float64(userA) {
			t.Fatalf("普通用户列表混入他人行：%v", row)
		}
	}

	// 3. ?state=all：活跃/非活跃分流 + 分页自洽性。
	status, body, raw = sessionsRequest(t, router, http.MethodGet,
		"/sessions?state=all&activePage=1&inactivePage=1&pageSize=1", "")
	if status != http.StatusOK {
		t.Fatalf("全量列表应为 200，实际 %d：%s", status, raw)
	}
	for _, field := range []string{
		"active", "inactive", "totalActive", "totalInactive", "hasMoreActive", "hasMoreInactive",
	} {
		if _, present := body[field]; !present {
			t.Fatalf("全量响应应含字段 %s，实际：%s", field, raw)
		}
	}
	active := body["active"].([]any)
	inactive := body["inactive"].([]any)
	totalActive := body["totalActive"].(float64)
	totalInactive := body["totalInactive"].(float64)

	if len(active) != 1 || len(inactive) != 1 {
		t.Fatalf("pageSize=1 时两侧各应 1 行，实际 active=%d inactive=%d", len(active), len(inactive))
	}
	// 夹具里 userA 共三个：recent（5 分钟内）与 withConcurrent（有并发数）属活跃，
	// stale（十分钟前）属非活跃；foreign 也是十分钟前 → 非活跃。
	if totalActive < 2 {
		t.Errorf("活跃总数应含「近期请求」与「有并发数」两行，实际 %v", totalActive)
	}
	if totalInactive < 2 {
		t.Errorf("十分钟前有请求的两行应落非活跃，实际 totalInactive=%v", totalInactive)
	}
	if body["hasMoreActive"] != (totalActive > 1) {
		t.Errorf("hasMoreActive 与总数不一致：%v / %v", body["hasMoreActive"], totalActive)
	}
	if body["hasMoreInactive"] != (totalInactive > 1) {
		t.Errorf("hasMoreInactive 与总数不一致：%v / %v", body["hasMoreInactive"], totalInactive)
	}

	// 4. 分页越界：越界页是空数组，不是 null（Node 的 slice 语义）。
	status, body, raw = sessionsRequest(t, router, http.MethodGet,
		"/sessions?state=all&activePage=99&inactivePage=99&pageSize=5", "")
	if status != http.StatusOK {
		t.Fatalf("越界分页应为 200，实际 %d：%s", status, raw)
	}
	if len(body["active"].([]any)) != 0 || len(body["inactive"].([]any)) != 0 {
		t.Fatalf("越界页应为空数组，实际：%s", raw)
	}
	if body["hasMoreActive"] != false || body["hasMoreInactive"] != false {
		t.Errorf("越界页的 hasMore 应为 false，实际：%s", raw)
	}

	// 5. 查询校验：state 非法枚举与 pageSize 上界（zod 的口径）。
	status, _, raw = sessionsRequest(t, router, http.MethodGet, "/sessions?state=idle", "")
	if status != http.StatusBadRequest {
		t.Fatalf("非法 state 应 400，实际 %d：%s", status, raw)
	}
	status, _, raw = sessionsRequest(t, router, http.MethodGet, "/sessions?pageSize=201", "")
	if status != http.StatusBadRequest {
		t.Fatalf("pageSize 超上界应 400，实际 %d：%s", status, raw)
	}
}

// TestSessionsListRequiresAuth 钉住 401 判据：无主体（UserID==0）才拒，ADMIN_TOKEN 主体不拒。
func TestSessionsListRequiresAuth(t *testing.T) {
	router := New(Options{Deps: Deps{Guard: principalGuard{principal: Principal{}}}})
	RegisterSessionsRoutes(router, Deps{
		Guard:               principalGuard{principal: Principal{}},
		Problems:            NewProblems(nil),
		Store:               &store.Pools{},
		SessionObservations: stubObservations{},
	})
	status, _, raw := sessionsRequest(t, router, http.MethodGet, "/sessions", "")
	if status != http.StatusUnauthorized {
		t.Fatalf("无主体应 401，实际 %d：%s", status, raw)
	}
}

// TestTerminateSessionOnRealDependencies 端到端终止一个真绑定会话。
//
// 现场用**生产路径**造：先由会话适配器的 Ensure 建立真实绑定（走 READ_OR_RECONCILE 的 Lua），
// 再用 CAS 把供应商绑上——账本行的 provider_id 是 NOT NULL，于是终止必然走**带供应商范围**的
// 分支（这也正是 Node 里最常见的形态）。
func TestTerminateSessionOnRealDependencies(t *testing.T) {
	deps := openSessionsDeps(t)
	ctx := context.Background()
	userID := fixtureUser(t, deps.Pools, "user", true)
	keyID, keyValue := fixtureKey(t, deps.Pools, fixtureKeyOptions{
		userID: userID, canLoginWebUI: true, isEnabled: true,
	})
	providerID := fixtureProviderID(t, deps.Pools)

	// 1. 生产路径建立绑定（哈希分支会写规范键与 legacy 镜像）。
	result, err := deps.Adapter.Ensure(ctx, guard.SessionRequest{
		KeyID: keyID,
		Body: map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": "terminate-it"}},
		},
	})
	if err != nil {
		t.Fatalf("建立会话绑定失败: %v", err)
	}
	sessionID := result.SessionID
	if sessionID == "" {
		t.Fatal("Ensure 应返回会话 id")
	}

	bindingKeys := session.BuildBindingKeys(sessionID, keyID)
	if exists := deps.Redis.Exists(ctx, bindingKeys.Canonical).Val(); exists != 1 {
		t.Fatalf("Ensure 应写规范绑定键 %s", bindingKeys.Canonical)
	}

	// 2. 把供应商绑上（与账本行同值）。
	binding, err := deps.Binder.ReadOrReconcile(ctx, sessionID, keyID, 300)
	if err != nil || !binding.OK {
		t.Fatalf("读绑定失败（ok=%v err=%v）", binding.OK, err)
	}
	cas, err := deps.Binder.CompareAndSet(ctx, sessionID, keyID,
		binding.Snapshot.Generation, providerID, 300)
	if err != nil || !cas.OK {
		t.Fatalf("绑定供应商失败（ok=%v err=%v）", cas.OK, err)
	}

	// 3. 账本行：物理 session_id = identity。
	seedTerminateLedgerRow(t, deps.Pools, userID, keyValue, sessionID, providerID)

	// 4. Node 世界的观测现场：info（userId）、观测 ZSET、观测并发计数、供应商维度索引。
	pipe := deps.Redis.Pipeline()
	pipe.HSet(ctx, session.InfoKey(sessionID), "userId", strconv.FormatInt(userID, 10))
	pipe.ZAdd(ctx, session.ObservedGlobalActiveSessionsKey(),
		redis.Z{Score: float64(time.Now().UnixMilli()), Member: sessionID})
	pipe.Set(ctx, session.ObservedConcurrentCountKey(sessionID), "1", 0)
	pipe.ZAdd(ctx, session.ProviderActiveSessionsKey(providerID),
		redis.Z{Score: float64(time.Now().UnixMilli()), Member: sessionID})
	if _, err := pipe.Exec(ctx); err != nil {
		t.Fatalf("写终止现场失败: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = deps.Redis.Del(cleanupCtx, bindingKeys.Canonical,
			bindingKeys.LegacyProvider, bindingKeys.LegacyOwner,
			session.InfoKey(sessionID), session.ObservedConcurrentCountKey(sessionID)).Err()
		_ = deps.Redis.ZRem(cleanupCtx, session.ObservedGlobalActiveSessionsKey(), sessionID).Err()
		_ = deps.Redis.ZRem(cleanupCtx, session.ProviderActiveSessionsKey(providerID), sessionID).Err()
		_ = deps.Redis.ZRem(cleanupCtx, session.ActiveSessionsGlobalKey(), sessionID).Err()
		_ = deps.Redis.ZRem(cleanupCtx, session.KeyActiveSessionsKey(keyID), sessionID).Err()
		_ = deps.Redis.ZRem(cleanupCtx, session.UserActiveSessionsKey(userID), sessionID).Err()
	})

	router := sessionsRouterWithDeps(t, deps.Pools, Principal{UserID: userID, IsAdmin: true}, deps)

	// 5. 终止：204 且无正文。
	status, _, raw := sessionsRequest(t, router, http.MethodDelete, "/sessions/"+sessionID, "")
	if status != http.StatusNoContent {
		t.Fatalf("终止应为 204，实际 %d：%s", status, raw)
	}
	if raw != "" {
		t.Fatalf("204 不应有正文，实际：%s", raw)
	}

	// 6. 逐项断言。
	//
	// 带供应商范围的终止只摘**该供应商自己的索引**（failover 可能已改绑，见
	// internal/session/terminate.go 的注释），四组活跃 ZSET 不在其中；但动作层仍会摘观测集合
	// 与 info（Node 的 SessionTracker.terminateObservedSession）。
	if exists := deps.Redis.Exists(ctx, bindingKeys.LegacyProvider).Val(); exists != 0 {
		t.Errorf("终止后 legacy provider 镜像应不存在")
	}
	if exists := deps.Redis.Exists(ctx, session.InfoKey(sessionID)).Val(); exists != 0 {
		t.Errorf("终止后 %s 应不存在", session.InfoKey(sessionID))
	}
	if score, scoreErr := deps.Redis.ZScore(ctx,
		session.ObservedGlobalActiveSessionsKey(), sessionID).Result(); scoreErr != redis.Nil {
		t.Errorf("终止后观测集合不应再含会话（score=%v err=%v）", score, scoreErr)
	}
	if exists := deps.Redis.Exists(ctx, session.ObservedConcurrentCountKey(sessionID)).Val(); exists != 0 {
		t.Errorf("终止后观测并发计数键应不存在")
	}
	if score, scoreErr := deps.Redis.ZScore(ctx,
		session.ProviderActiveSessionsKey(providerID), sessionID).Result(); scoreErr != redis.Nil {
		t.Errorf("终止后供应商索引不应再含会话（score=%v err=%v）", score, scoreErr)
	}

	// 7. 再终止一次：**账本行还在**（终止动的是 Redis 绑定与索引，不删账本），故所有权判定
	// 仍然命中，失败落在「绑定已不在」那一段 → 400 session.action_failed。
	//
	// 这一条是 Node 的行为，不是本实现的取舍：Node 的 aggregateMultipleSessionStats 同样按
	// 账本表判定所有权，终止后它照样返回该会话，紧接着的 terminateSession 返回 false。
	// 404 只属于「账本里也查不到」的情形。
	status, _, raw = sessionsRequest(t, router, http.MethodDelete, "/sessions/"+sessionID, "")
	if status != http.StatusBadRequest {
		t.Fatalf("重复终止应 400，实际 %d：%s", status, raw)
	}
	if code := problemErrorCode(t, raw); code != "session.action_failed" {
		t.Errorf("400 的错误码应为 session.action_failed，实际 %s", code)
	}

	// 8. 账本里也没有的会话 → 404 session.not_found。
	status, _, raw = sessionsRequest(t, router, http.MethodDelete,
		"/sessions/sess_absent_"+strconv.FormatInt(time.Now().UnixNano(), 10), "")
	if status != http.StatusNotFound {
		t.Fatalf("不存在的会话应 404，实际 %d：%s", status, raw)
	}
	if code := problemErrorCode(t, raw); code != "session.not_found" {
		t.Errorf("404 的错误码应为 session.not_found，实际 %s", code)
	}
}

// TestBatchTerminateSessionsOnRealDependencies 钉住批量的三段计数与所有者过滤。
func TestBatchTerminateSessionsOnRealDependencies(t *testing.T) {
	deps := openSessionsDeps(t)
	ctx := context.Background()
	userA := fixtureUser(t, deps.Pools, "user", true)
	userB := fixtureUser(t, deps.Pools, "user", true)
	keyID, keyValue := fixtureKey(t, deps.Pools, fixtureKeyOptions{
		userID: userA, canLoginWebUI: true, isEnabled: true,
	})
	_, keyValueB := fixtureKey(t, deps.Pools, fixtureKeyOptions{
		userID: userB, canLoginWebUI: true, isEnabled: true,
	})
	providerID := fixtureProviderID(t, deps.Pools)
	nonce := strconv.FormatInt(time.Now().UnixNano(), 10)
	mine := "sess-batch-" + nonce + "-mine"
	theirs := "sess-batch-" + nonce + "-theirs"
	absent := "sess-batch-" + nonce + "-absent"

	seedTerminateLedgerRow(t, deps.Pools, userA, keyValue, mine, providerID)
	seedTerminateLedgerRow(t, deps.Pools, userB, keyValueB, theirs, providerID)

	// 终止要求真绑定：用适配器给「我的会话」建立绑定（物理 session id 即 identity），
	// 再把供应商 CAS 成与账本行同值——带范围的终止只在绑定正好落在该集合里才动手。
	if _, err := deps.Adapter.Ensure(ctx, guard.SessionRequest{
		KeyID: keyID,
		Body: map[string]any{
			"metadata": map[string]any{"session_id": mine},
			"messages": []any{map[string]any{"role": "user", "content": "batch-it"}},
		},
	}); err != nil {
		t.Fatalf("建立批两会话绑定失败: %v", err)
	}
	bindingKeys := session.BuildBindingKeys(mine, keyID)
	if exists := deps.Redis.Exists(ctx, bindingKeys.Canonical).Val(); exists != 1 {
		t.Skip("夹具未能把绑定落到 identity=sess-batch-... 上，跳过（依赖正文里的客户端会话 id 提取）")
	}
	binding, err := deps.Binder.ReadOrReconcile(ctx, mine, keyID, 300)
	if err != nil || !binding.OK {
		t.Fatalf("读批两夹具绑定失败（ok=%v err=%v）", binding.OK, err)
	}
	if _, casErr := deps.Binder.CompareAndSet(ctx, mine, keyID,
		binding.Snapshot.Generation, providerID, 300); casErr != nil {
		t.Fatalf("批两夹具绑供应商失败: %v", casErr)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = deps.Redis.Del(cleanupCtx, bindingKeys.Canonical,
			bindingKeys.LegacyProvider, bindingKeys.LegacyOwner).Err()
	})

	body := fmt.Sprintf(`{"sessionIds":[%q,%q,%q]}`, mine, theirs, absent)

	// 1. 非管理员：他人的会话在 SQL 侧被所有者条件挡掉 → 计入 missing（不是 unauthorized）。
	router := sessionsRouterWithDeps(t, deps.Pools, Principal{UserID: userA}, deps)
	status, parsed, raw := sessionsRequest(t, router, http.MethodPost,
		"/sessions:batchTerminate", body)
	if status != http.StatusOK {
		t.Fatalf("批量终止应为 200，实际 %d：%s", status, raw)
	}
	if parsed["requestedCount"].(float64) != 3 {
		t.Errorf("requestedCount 应为 3，实际 %v", parsed["requestedCount"])
	}
	missing := stringList(t, parsed["missingSessionIds"])
	if len(missing) != 2 || !contains(missing, theirs) || !contains(missing, absent) {
		t.Errorf("missing 应为 [他者, 不存在]，实际 %v（原始：%s）", missing, raw)
	}
	if unauthorized := stringList(t, parsed["unauthorizedSessionIds"]); len(unauthorized) != 0 {
		t.Errorf("非管理员看不到他人会话，unauthorized 应为空，实际 %v", unauthorized)
	}
	if parsed["unauthorizedCount"].(float64) != 0 || parsed["missingCount"].(float64) != 2 {
		t.Errorf("计数应与列表一致，实际 unauthorized=%v missing=%v",
			parsed["unauthorizedCount"], parsed["missingCount"])
	}
	if parsed["processedCount"].(float64) != 1 {
		t.Errorf("processedCount 应为 1（唯一被授权的会话），实际 %v", parsed["processedCount"])
	}
	if parsed["successCount"].(float64) != 1 {
		t.Errorf("已建立绑定的会话应终止成功，实际 successCount=%v（原始：%s）",
			parsed["successCount"], raw)
	}
	if parsed["allowedFailedCount"].(float64) != 0 {
		t.Errorf("allowedFailedCount 应为 0，实际 %v", parsed["allowedFailedCount"])
	}
	// failedCount = allowedFailed + unauthorized + missing（Node 的三段相加）。
	if parsed["failedCount"].(float64) != 2 {
		t.Errorf("failedCount 应为 2，实际 %v", parsed["failedCount"])
	}

	// 2. 重复请求同一个 id：绑定已终止 → 不再计入成功（仍被认领，故也不在 missing 里）。
	status, parsed, raw = sessionsRequest(t, router, http.MethodPost,
		"/sessions:batchTerminate", body)
	if status != http.StatusOK {
		t.Fatalf("重复批量终止应为 200，实际 %d：%s", status, raw)
	}
	if parsed["successCount"].(float64) != 0 {
		t.Errorf("重复终止不应再有成功项，实际 %v（原始：%s）", parsed["successCount"], raw)
	}
	if parsed["allowedFailedCount"].(float64) != 1 {
		t.Errorf("重复终止应记为 allowedFailed，实际 %v", parsed["allowedFailedCount"])
	}

	// 3. 正文校验：空数组与未知键都是 400。
	status, _, raw = sessionsRequest(t, router, http.MethodPost,
		"/sessions:batchTerminate", `{"sessionIds":[]}`)
	if status != http.StatusBadRequest {
		t.Fatalf("空数组应 400，实际 %d：%s", status, raw)
	}
	status, _, raw = sessionsRequest(t, router, http.MethodPost,
		"/sessions:batchTerminate", `{"sessionIds":["x"],"extra":1}`)
	if status != http.StatusBadRequest {
		t.Fatalf("未知键应 400（strict），实际 %d：%s", status, raw)
	}
}

// seedListSessionRow 造一行列表夹具（按「最后一次请求」偏移）。
func seedListSessionRow(
	t *testing.T,
	pools *store.Pools,
	userID int64,
	keyValue string,
	identity string,
	providerID int64,
	lastRequestAge time.Duration,
) {
	t.Helper()
	seedSessionRowAt(t, pools, userID, keyValue, identity, providerID,
		time.Now().Add(-lastRequestAge))
}

// fixtureProviderID 取一个真实存在的供应商 id（账本行的 provider_id 有 NOT NULL 约束）。
func fixtureProviderID(t *testing.T, pools *store.Pools) int64 {
	t.Helper()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取 control 分道失败: %v", err)
	}
	var providerID int64
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM providers ORDER BY id LIMIT 1`).Scan(&providerID); err != nil {
		t.Skipf("库内没有可用供应商行，跳过（%v）", err)
	}
	return providerID
}

// seedSessionRowAt 插一行请求行。
func seedSessionRowAt(
	t *testing.T,
	pools *store.Pools,
	userID int64,
	keyValue string,
	identity string,
	providerID int64,
	createdAt time.Time,
) {
	t.Helper()
	ctx := context.Background()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取 control 分道失败: %v", err)
	}
	model := fmt.Sprintf("sessions-list-%d-%s", time.Now().UnixNano(), identity)
	if _, err := pool.Exec(ctx, `
		INSERT INTO message_request (
			provider_id, user_id, key, model, original_model, endpoint,
			status_code, input_tokens, output_tokens, cost_usd, duration_ms, ttfb_ms,
			session_id, session_identity, session_identity_kind, request_sequence,
			is_replay, created_at
		) VALUES (
			$6, $1, $2, $3, $3, '/v1/messages',
			200, 100, 20, '0.5'::numeric, 10, 5,
			$4, $4, 'session_id', 1,
			false, $5
		)`, userID, keyValue, model, identity, createdAt, providerID); err != nil {
		t.Fatalf("插入列表夹具行失败: %v", err)
	}
	t.Cleanup(func() {
		cleanupPool, cleanupErr := pools.Control()
		if cleanupErr != nil {
			return
		}
		_, _ = cleanupPool.Exec(ctx,
			`DELETE FROM usage_ledger WHERE request_id IN
				(SELECT id FROM message_request WHERE key = $1 AND session_id = $2)`,
			keyValue, identity)
		_, _ = cleanupPool.Exec(ctx,
			`DELETE FROM message_request WHERE key = $1 AND session_id = $2`, keyValue, identity)
	})
}

// seedTerminateLedgerRow 造一行终止夹具（物理 session_id = identity）。
func seedTerminateLedgerRow(
	t *testing.T,
	pools *store.Pools,
	userID int64,
	keyValue string,
	identity string,
	providerID int64,
) {
	t.Helper()
	seedSessionRowAt(t, pools, userID, keyValue, identity, providerID, time.Now())
}

// stringList 把 JSON 数组读成字符串切片。
func stringList(t *testing.T, raw any) []string {
	t.Helper()
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("应为数组，实际 %T", raw)
	}
	result := make([]string, 0, len(list))
	for _, item := range list {
		value, isString := item.(string)
		if !isString {
			t.Fatalf("数组元素应为字符串，实际 %T", item)
		}
		result = append(result, value)
	}
	return result
}

// contains 判断切片是否含该值。
func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

// problemErrorCode 从 problem+json 正文里取 errorCode。
func problemErrorCode(t *testing.T, raw string) string {
	t.Helper()
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("解 problem 正文失败: %v（原始：%s）", err, raw)
	}
	code, _ := parsed["errorCode"].(string)
	return code
}

// stubObservations / stubTerminator / stubAffinity 是注册条件用例的桩件（不参与集成路径）。
type stubObservations struct{}

func (stubObservations) ObservedActiveSessions(context.Context) ([]string, error) {
	return nil, nil
}

func (stubObservations) ObservedConcurrentCounts(
	context.Context, []string,
) (map[string]int, error) {
	return nil, nil
}

func (stubObservations) AllSessionIDs(context.Context) ([]string, error) { return nil, nil }

type stubTerminator struct{}

func (stubTerminator) TerminateSession(
	context.Context, string, []int64, *int64,
) (bool, error) {
	return false, nil
}

func (stubTerminator) TerminateObservedSessionForIdentity(context.Context, string) (bool, error) {
	return false, nil
}

type stubAffinity struct{}

func (stubAffinity) Invalidate(context.Context, string, string, []string) bool { return false }

// stubArtifacts 是 SessionArtifactReader 的空实现（路由契约测试只关心注册条件）。
type stubArtifacts struct{}

func (stubArtifacts) IsSessionRequestOwnedByKey(context.Context, string, int, int64) bool {
	return false
}

func (stubArtifacts) SessionMessages(context.Context, string, int) (any, bool, error) {
	return nil, false, nil
}

func (stubArtifacts) HasAnySessionMessages(context.Context, string) bool { return false }

func (stubArtifacts) SessionRequestBody(context.Context, string, int) (any, bool) { return nil, false }

func (stubArtifacts) SessionResponseBody(context.Context, string, int) (string, bool) {
	return "", false
}

func (stubArtifacts) SessionRequestHeaders(context.Context, string, int) map[string]string {
	return nil
}

func (stubArtifacts) SessionResponseHeaders(context.Context, string, int) map[string]string {
	return nil
}

func (stubArtifacts) ReadSessionPhaseSnapshot(context.Context, string, int, string, string) *session.SessionDetailSnapshotRead {
	return nil
}

func (stubArtifacts) ReadSessionClientRequestMeta(context.Context, string, int) *session.SessionUpstreamRequestMetaRead {
	return nil
}

func (stubArtifacts) ReadSessionUpstreamRequestMeta(context.Context, string, int) *session.SessionUpstreamRequestMetaRead {
	return nil
}

func (stubArtifacts) ReadSessionUpstreamResponseMeta(context.Context, string, int) *session.SessionUpstreamResponseMetaRead {
	return nil
}
