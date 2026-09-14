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

// 本文件是 **sessions 资源**（/api/v1/sessions/*）的测试：
// 路由契约（档位与 operationId）+ 真实 PG 集成（所有权过滤、分页、displaySequence 窗口）。
//
// 夹具口径与仓库既有纪律一致：每个用例自建「用户 + 密钥 + 请求行」，以**唯一 model 前缀**
// 定位与清理，绝不写范围删除。

// TestRegisterSessionsRoutes 钉住已就绪端点的档位与 operationId。
func TestRegisterSessionsRoutes(t *testing.T) {
	guard := &recordingGuard{}
	router := New(Options{Deps: Deps{Guard: guard}})
	RegisterSessionsRoutes(router, Deps{
		Guard:            guard,
		Problems:         NewProblems(nil),
		Store:            &store.Pools{},
		SessionArtifacts: stubArtifacts{},
	})

	want := []struct {
		key    string
		access AccessLevel
		op     string
	}{
		{http.MethodGet + " /sessions/{sessionId}/requests", AccessRead, "getSessionRequests"},
		{http.MethodGet + " /sessions/{sessionId}/origin-chain", AccessRead, "getSessionOriginChain"},
		{http.MethodGet + " /sessions/{sessionId}", AccessRead, "getSessionDetail"},
		{http.MethodGet + " /sessions/{sessionId}/messages", AccessRead, "getSessionMessages"},
		{http.MethodGet + " /sessions/{sessionId}/messages/exists", AccessRead, "hasSessionMessages"},
	}
	routes := router.RouteList()
	if len(routes) != len(want) {
		t.Fatalf("应注册 %d 条，实际 %d：%v", len(want), len(routes), auditLogRouteKeys(router))
	}
	for index, route := range routes {
		if route.Method+" "+route.Path != want[index].key {
			t.Errorf("第 %d 条应为 %s，实际 %s %s",
				index, want[index].key, route.Method, route.Path)
		}
		if route.Access != want[index].access {
			t.Errorf("%s 档位应为 %s，实际 %s", want[index].key, want[index].access, route.Access)
		}
		if route.OperationID != want[index].op {
			t.Errorf("%s 的 operationId 应为 %s，实际 %s",
				want[index].key, want[index].op, route.OperationID)
		}
	}
}

// TestRegisterSessionsRoutesWithoutStore 钉住「Store 未装配即整组不注册」的纪律。
func TestRegisterSessionsRoutesWithoutStore(t *testing.T) {
	guard := &recordingGuard{}
	router := New(Options{Deps: Deps{Guard: guard}})
	RegisterSessionsRoutes(router, Deps{Guard: guard, Problems: NewProblems(nil)})
	if routes := router.RouteList(); len(routes) != 0 {
		t.Fatalf("Store 未装配时不应注册任何路由，实际 %d 条", len(routes))
	}
}

// TestParseSessionRequestsQuery 钉住 SessionRequestsQuerySchema 的默认值、边界与错误码。
func TestParseSessionRequestsQuery(t *testing.T) {
	cases := []struct {
		name     string
		target   string
		want     sessionRequestsQuery
		wantCode string
	}{
		{name: "全缺省", target: "/sessions/s1/requests",
			want: sessionRequestsQuery{Page: 1, PageSize: 20, Order: "desc"}},
		{name: "显式取值", target: "/sessions/s1/requests?page=3&pageSize=5&order=asc",
			want: sessionRequestsQuery{Page: 3, PageSize: 5, Order: "asc"}},
		{name: "pageSize 上界", target: "/sessions/s1/requests?pageSize=200",
			want: sessionRequestsQuery{Page: 1, PageSize: 200, Order: "desc"}},
		{name: "pageSize 超上界", target: "/sessions/s1/requests?pageSize=201",
			wantCode: "too_big"},
		{name: "page 下界", target: "/sessions/s1/requests?page=0", wantCode: "too_small"},
		{name: "page 非整数", target: "/sessions/s1/requests?page=1.5", wantCode: "invalid_type"},
		{name: "page 非数值", target: "/sessions/s1/requests?page=abc", wantCode: "invalid_type"},
		{name: "order 非法枚举", target: "/sessions/s1/requests?order=sideways",
			wantCode: "invalid_enum_value"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, testCase.target, nil)
			query, issues := parseSessionRequestsQuery(request)
			if testCase.wantCode == "" {
				if len(issues) != 0 {
					t.Fatalf("不应有校验问题，实际 %v", issues)
				}
				if query != testCase.want {
					t.Fatalf("解析结果应为 %+v，实际 %+v", testCase.want, query)
				}
				return
			}
			if len(issues) != 1 {
				t.Fatalf("应有一条校验问题，实际 %v", issues)
			}
			if issues[0].Code != testCase.wantCode {
				t.Fatalf("错误码应为 %s，实际 %s", testCase.wantCode, issues[0].Code)
			}
		})
	}
}

// sessionsRouter 建一个以该身份注入的路由表。
func sessionsRouter(t *testing.T, pools *store.Pools, principal Principal) *Router {
	t.Helper()
	router := New(Options{Deps: Deps{Guard: principalGuard{principal: principal}}})
	RegisterSessionsRoutes(router, Deps{
		Guard:    principalGuard{principal: principal},
		Problems: NewProblems(nil),
		Store:    pools,
	})
	return router
}

// sessionsGet 发一次 GET 并解出 JSON 对象。
func sessionsGet(t *testing.T, router *Router, target string) (int, map[string]any, string) {
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

// seedSessionRequests 插若干请求行，全部挂在同一个规范 identity 上。
//
// identityKind 取 "session_id" 或 "prefix_affinity"：后者让 displaySequence 落到窗口序号
// （而非 request_sequence），这正是要钉住的分支。physicalSessionID 是物理 session_id 列
// （prefix_affinity 会话下它与 identity 不同，行里的 sourceSessionId 取的是它）。
func seedSessionRequests(
	t *testing.T,
	pools *store.Pools,
	userID int64,
	keyValue string,
	identity string,
	physicalSessionID string,
	identityKind string,
	sequences []int,
) {
	t.Helper()
	ctx := context.Background()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取 control 分道失败: %v", err)
	}
	for _, sequence := range sequences {
		// created_at 在 Go 侧算好：不能用 SQL 里的 now() 减同一占位符——那个占位符还喂着
		// duration_ms（int），两者类型推断相抵会让 PG 报 inconsistent types 42P08。
		// 时间随 sequence 递增（都落在过去）：真实语义里序号越大越新，desc 首行才是最大序号。
		createdAt := time.Now().Add(time.Duration(sequence-10) * time.Second)
		if _, err := pool.Exec(ctx, `
			INSERT INTO message_request (
				provider_id, user_id, key, model, original_model, endpoint,
				status_code, input_tokens, output_tokens, cost_usd, duration_ms, ttfb_ms,
				session_id, session_identity, session_identity_kind, request_sequence,
				is_replay, created_at
			) VALUES (
				$1, $2, $3, $4, $4, '/v1/messages',
				200, 100, 20, '0.5'::numeric, $5, 55,
				$6, $7, $8, $9,
				false, $10
			)`,
			int64(1), userID, keyValue, fmt.Sprintf("sessions-it-%d-%d", time.Now().UnixNano(), sequence),
			sequence*10, physicalSessionID, identity, identityKind, sequence, createdAt,
		); err != nil {
			t.Fatalf("插入 sessions 夹具行失败: %v", err)
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
}

// TestSessionRequestsTimeline 钉住管理员视角：总数、分页与行形状。
func TestSessionRequestsTimeline(t *testing.T) {
	pools := meOpenPools(t)
	userID := fixtureUser(t, pools, "user", true)
	_, keyValue := fixtureKey(t, pools, fixtureKeyOptions{
		userID: userID, canLoginWebUI: true, isEnabled: true,
	})
	identity := fmt.Sprintf("sessions-it-%d", time.Now().UnixNano())
	seedSessionRequests(t, pools, userID, keyValue, identity, identity, "session_id", []int{1, 2, 3})

	router := sessionsRouter(t, pools, Principal{UserID: userID, Username: "sessions-it", IsAdmin: true})
	status, body, raw := sessionsGet(t, router, "/sessions/"+identity+"/requests?pageSize=2")
	if status != http.StatusOK {
		t.Fatalf("应拿到 200，实际 %d：%s", status, raw)
	}
	if total := body["total"].(float64); total != 3 {
		t.Fatalf("total 应为 3，实际 %v：%s", total, raw)
	}
	requests := body["requests"].([]any)
	if len(requests) != 2 {
		t.Fatalf("第一页应有 2 行，实际 %d：%s", len(requests), raw)
	}
	if hasMore := body["hasMore"].(bool); !hasMore {
		t.Errorf("第一页应还有更多：%s", raw)
	}
	// desc 排序：第 1 行是最后写入的（sequence 3，created_at 最大）。
	first := requests[0].(map[string]any)
	if first["sequence"].(float64) != 3 {
		t.Errorf("desc 首行 sequence 应为 3，实际 %v", first["sequence"])
	}
	if first["sourceSessionId"] != identity {
		t.Errorf("sourceSessionId 应为规范 identity，实际 %v", first["sourceSessionId"])
	}
	// session_id 会话的 displaySequence = request_sequence。
	if first["displaySequence"].(float64) != 3 {
		t.Errorf("session_id 会话的 displaySequence 应为 3，实际 %v", first["displaySequence"])
	}
	if first["costUsd"] != "0.500000000000000" {
		t.Errorf("costUsd 应是 numeric 文本，实际 %v", first["costUsd"])
	}
	if _, ok := first["createdAt"].(string); !ok {
		t.Errorf("createdAt 应是 ISO 串，实际 %v", first["createdAt"])
	}

	// 第二页只剩 1 行，hasMore 为 false。
	status, body, raw = sessionsGet(t, router, "/sessions/"+identity+"/requests?pageSize=2&page=2")
	if status != http.StatusOK {
		t.Fatalf("第二页应拿到 200，实际 %d：%s", status, raw)
	}
	if requests := body["requests"].([]any); len(requests) != 1 {
		t.Fatalf("第二页应有 1 行，实际 %d：%s", len(requests), raw)
	}
	if hasMore := body["hasMore"].(bool); hasMore {
		t.Errorf("最后一页不应还有更多：%s", raw)
	}
}

// TestSessionRequestsPrefixAffinityDisplaySequence 钉住窗口函数分支：
// prefix_affinity 会话的 displaySequence 是按时间重编的轮次，不是 request_sequence。
func TestSessionRequestsPrefixAffinityDisplaySequence(t *testing.T) {
	pools := meOpenPools(t)
	userID := fixtureUser(t, pools, "user", true)
	_, keyValue := fixtureKey(t, pools, fixtureKeyOptions{
		userID: userID, canLoginWebUI: true, isEnabled: true,
	})
	physical := fmt.Sprintf("pfx-it-%d", time.Now().UnixNano())
	identity := "pfx:" + physical
	// request_sequence 刻意从 7 起：窗口序号应从 1 起，两者必须不同才算钉住。
	seedSessionRequests(t, pools, userID, keyValue, identity, physical, "prefix_affinity", []int{7, 8})

	router := sessionsRouter(t, pools, Principal{UserID: userID, Username: "sessions-it", IsAdmin: true})
	status, body, raw := sessionsGet(t, router, "/sessions/"+identity+"/requests?order=asc")
	if status != http.StatusOK {
		t.Fatalf("应拿到 200，实际 %d：%s", status, raw)
	}
	requests := body["requests"].([]any)
	if len(requests) != 2 {
		t.Fatalf("应有 2 行，实际 %d：%s", len(requests), raw)
	}
	for index, entry := range requests {
		row := entry.(map[string]any)
		if got := row["displaySequence"].(float64); got != float64(index+1) {
			t.Errorf("第 %d 行 displaySequence 应为 %d（窗口序号），实际 %v",
				index, index+1, got)
		}
	}
	if sequence := requests[0].(map[string]any)["sequence"].(float64); sequence != 7 {
		t.Errorf("sequence 应保留原始 request_sequence（7），实际 %v", sequence)
	}
}

// TestSessionRequestsOwnership 钉住权限：普通用户查别人的会话得到 404（与 Node 同判）。
func TestSessionRequestsOwnership(t *testing.T) {
	pools := meOpenPools(t)
	ownerID := fixtureUser(t, pools, "user", true)
	_, ownerKey := fixtureKey(t, pools, fixtureKeyOptions{
		userID: ownerID, canLoginWebUI: true, isEnabled: true,
	})
	identity := fmt.Sprintf("sessions-owner-it-%d", time.Now().UnixNano())
	seedSessionRequests(t, pools, ownerID, ownerKey, identity, identity, "session_id", []int{1})

	// 另一个用户（非管理员）看不到它。
	otherID := fixtureUser(t, pools, "user", true)
	router := sessionsRouter(t, pools, Principal{UserID: otherID, Username: "sessions-it", IsAdmin: false})
	status, body, raw := sessionsGet(t, router, "/sessions/"+identity+"/requests")
	if status != http.StatusNotFound {
		t.Fatalf("非所有者应拿到 404，实际 %d：%s", status, raw)
	}
	if body["errorCode"] != "session.not_found" {
		t.Errorf("errorCode 应为 session.not_found，实际 %v", body["errorCode"])
	}
	if body["detail"] != "Not found" {
		t.Errorf("detail 应为 publicActionErrorDetail(404)，实际 %v", body["detail"])
	}

	// 所有者自己（非管理员）看得到，且只看到自己的行。
	router = sessionsRouter(t, pools, Principal{UserID: ownerID, Username: "sessions-it", IsAdmin: false})
	status, body, raw = sessionsGet(t, router, "/sessions/"+identity+"/requests")
	if status != http.StatusOK {
		t.Fatalf("所有者应拿到 200，实际 %d：%s", status, raw)
	}
	if total := body["total"].(float64); total != 1 {
		t.Fatalf("所有者应看到 1 行，实际 %v：%s", total, raw)
	}

	// 不存在的 identity 同样是 404。
	status, _, raw = sessionsGet(t, router, "/sessions/does-not-exist-it/requests")
	if status != http.StatusNotFound {
		t.Fatalf("不存在的会话应拿到 404，实际 %d：%s", status, raw)
	}
}

// TestSessionRequestsPhysicalIdentityResolution 钉住身份归并：用**物理** session_id 打开
// 会话时，服务端应把该物理 id 归并到它的规范 identity 上（Node 的 aggregateMultipleSessionStats
// LATERAL 第二支），而不是作答 404。
func TestSessionRequestsPhysicalIdentityResolution(t *testing.T) {
	pools := meOpenPools(t)
	userID := fixtureUser(t, pools, "user", true)
	_, keyValue := fixtureKey(t, pools, fixtureKeyOptions{
		userID: userID, canLoginWebUI: true, isEnabled: true,
	})
	suffix := time.Now().UnixNano()
	physical := fmt.Sprintf("sessions-phys-it-%d", suffix)
	canonical := fmt.Sprintf("sessions-canon-it-%d", suffix)
	// 行的物理 session_id = physical，规范 identity = canonical（第二支的命中条件）。
	seedSessionRequests(t, pools, userID, keyValue, canonical, physical, "session_id", []int{1, 2})

	router := sessionsRouter(t, pools, Principal{UserID: userID, Username: "sessions-it", IsAdmin: true})
	status, body, raw := sessionsGet(t, router, "/sessions/"+physical+"/requests")
	if status != http.StatusOK {
		t.Fatalf("物理 id 应能归并到规范 identity，实际 %d：%s", status, raw)
	}
	if total := body["total"].(float64); total != 2 {
		t.Fatalf("应归并出 2 行，实际 %v：%s", total, raw)
	}
	// 行里的 sourceSessionId 仍是**物理** id（Node 的 row.sessionId）。
	first := body["requests"].([]any)[0].(map[string]any)
	if first["sourceSessionId"] != physical {
		t.Errorf("sourceSessionId 应为物理 id，实际 %v", first["sourceSessionId"])
	}
}

// TestSessionRequestsValidation 钉住查询校验作答 zod 形状的 400。
func TestSessionRequestsValidation(t *testing.T) {
	pools := meOpenPools(t)
	userID := fixtureUser(t, pools, "user", true)
	router := sessionsRouter(t, pools, Principal{UserID: userID, Username: "sessions-it", IsAdmin: true})

	status, body, raw := sessionsGet(t, router, "/sessions/s1/requests?pageSize=1000")
	if status != http.StatusBadRequest {
		t.Fatalf("越界 pageSize 应拿到 400，实际 %d：%s", status, raw)
	}
	if body["errorCode"] != "request.validation_failed" {
		t.Errorf("errorCode 应为 request.validation_failed，实际 %v", body["errorCode"])
	}
	if body["detail"] != "One or more fields are invalid." {
		t.Errorf("detail 应为 Node 的固定文案，实际 %v", body["detail"])
	}
	params, ok := body["invalidParams"].([]any)
	if !ok || len(params) != 1 {
		t.Fatalf("应有 1 条 invalidParams，实际 %v：%s", body["invalidParams"], raw)
	}
	if code := params[0].(map[string]any)["code"]; code != "too_big" {
		t.Errorf("校验码应为 too_big，实际 %v", code)
	}
}

// TestSessionRequestsAdminTokenPrincipalIsNotFound 钉住对拍 D3（sessions 侧）：
// ADMIN_TOKEN 合成主体（id -1、role admin）在会话不存在时作答 **404**（与 Node 的
// getSessionRequests → 「Session 不存在」→ 404 同判），而不是 401 auth.missing。
//
// 401 只留给「无主体」（UserID == 0）：Node 侧只判 `if (!authSession)`，-1 是真值不进这一支。
func TestSessionRequestsAdminTokenPrincipalIsNotFound(t *testing.T) {
	pools := meOpenPools(t)

	tokenRouter := sessionsRouter(t, pools, adminPrincipal())
	status, body, raw := sessionsGet(t, tokenRouter, "/sessions/sess-nonexistent-parity/requests")
	if status != http.StatusNotFound {
		t.Fatalf("管理令牌主体应拿到 404，实际 %d：%s", status, raw)
	}
	if body["errorCode"] != "session.not_found" {
		t.Errorf("错误码应为 session.not_found，实际 %v", body["errorCode"])
	}

	nobodyRouter := sessionsRouter(t, pools, Principal{IsAdmin: true})
	status, body, raw = sessionsGet(t, nobodyRouter, "/sessions/sess-nonexistent-parity/requests")
	if status != http.StatusUnauthorized {
		t.Fatalf("无主体应拿到 401，实际 %d：%s", status, raw)
	}
	if body["errorCode"] != "auth.missing" {
		t.Errorf("错误码应为 auth.missing，实际 %v", body["errorCode"])
	}
}
