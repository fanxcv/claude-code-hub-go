package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件覆盖 users 资源（19 条端点里已实现的 18 条）的路由登记、入参校验与端到端处理。
//
// 分两层：
//   - 不需要数据库的：路由表与校验器（本文件上半部分）；
//   - 需要真实 PG 的：逐端点驱动（门控 CCH_TEST_DSN，夹具自钉自清）。
//
// 夹具复用同包的 testPools/fixtureUser/fixtureKey（auth_test.go），不另起一套。

// ---- 驱动器 ----

// usersStubGuard 是注入固定身份的守卫替身。
//
// 为什么不用真守卫：本文件的被测对象是**处理器**，不是认证（认证由 A0-2 的测试覆盖）。
// 用替身可以把「非管理员读自己」「非管理员改他人被拒」这类分支单独驱动出来。
type usersStubGuard struct {
	principal Principal
	reject    bool
	lastLevel AccessLevel
}

func (g *usersStubGuard) Wrap(level AccessLevel, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		g.lastLevel = level
		if g.reject {
			writer.Header().Set("Content-Type", "application/problem+json")
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = writer.Write([]byte(`{"status":401,"errorCode":"auth.missing"}`))
			return
		}
		next.ServeHTTP(writer, request.WithContext(WithPrincipal(request.Context(), g.principal)))
	})
}

// usersTestRouter 装配一个只挂 users 路由的 Router，并返回守卫替身以便切换身份。
func usersTestRouter(t *testing.T, pools *store.Pools, principal Principal) (*Router, *usersStubGuard) {
	t.Helper()
	guard := &usersStubGuard{principal: principal}
	router := New(Options{Deps: Deps{Guard: guard, Store: pools, Problems: NewProblems(nil)}})
	RegisterUsersRoutes(router, Deps{Guard: guard, Store: pools, Problems: NewProblems(nil)})
	return router, guard
}

// adminPrincipal 是管理员身份（与 AuthGuard 对 ADMIN_TOKEN 的映射一致：id -1、非密钥身份）。
func adminPrincipal() Principal {
	return Principal{UserID: adminPrincipalUserID, Username: adminPrincipalName, IsAdmin: true}
}

// usersDo 发一个请求并返回响应记录器。
func usersDo(t *testing.T, router *Router, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// usersJSON 解析响应正文。
func usersJSON(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是 JSON 对象: %v body=%s", err, recorder.Body.String())
	}
	return payload
}

// usersCleanupRow 硬删掉测试用户（夹具自清；软删行会留在库里污染后续用例的列表断言）。
func usersCleanupRow(t *testing.T, pools *store.Pools, userID int64) {
	t.Helper()
	t.Cleanup(func() {
		pool, err := pools.Control()
		if err != nil {
			return
		}
		_, _ = pool.Exec(context.Background(), `DELETE FROM keys WHERE user_id = $1`, userID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM users WHERE id = $1`, userID)
	})
}

// ---- 路由表 ----

// TestUsersRoutesRegistered 钉住 18 条路由的方法与路径（含 6 条正则/冒号后缀写法）。
func TestUsersRoutesRegistered(t *testing.T) {
	guard := &usersStubGuard{principal: adminPrincipal()}
	router := New(Options{Deps: Deps{Guard: guard, Store: &store.Pools{}}})
	RegisterUsersRoutes(router, Deps{Guard: guard, Store: &store.Pools{}})

	got := make(map[string]struct{}, 32)
	for _, route := range router.RouteList() {
		got[route.Method+" "+route.Path] = struct{}{}
	}
	expected := []string{
		"GET /users",
		"POST /users",
		"GET /users:self",
		"GET /users/tags",
		"GET /users/key-groups",
		"GET /users:filter-search",
		"GET /users:search",
		"POST /users:usageBatch",
		"POST /users:batchUpdate",
		"GET /users/{id:[0-9]+}",
		"PATCH /users/{id:[0-9]+}",
		"DELETE /users/{id:[0-9]+}",
		"POST /users/{id:[0-9]+}:enable",
		"POST /users/{id:[0-9]+}:renew",
		"GET /users/{id:[0-9]+}/limit-usage",
		"GET /users/{id:[0-9]+}/limit-usage:all",
		"POST /users/{id:[0-9]+}/limits:reset",
		"POST /users/{id:[0-9]+}/limits:reset5h",
	}
	if len(got) != len(expected) {
		t.Fatalf("路由条数应为 %d，实际 %d：%v", len(expected), len(got), got)
	}
	for _, key := range expected {
		if _, ok := got[key]; !ok {
			t.Fatalf("缺少路由 %s", key)
		}
	}

	// 两条 statistics 端点刻意不注册（依赖 Node 的 bull 队列）：未注册即透明回退 Node。
	var payload map[string]any
	recorder := usersDo(t, router, http.MethodPost, "/api/v1/users/7/statistics:reset", "{}")
	if recorder.Code == http.StatusOK || recorder.Code == http.StatusNoContent {
		t.Fatalf("statistics:reset 不应在 Go 侧作答，实际 %d", recorder.Code)
	}
	_ = payload
}

// TestUsersRoutesRequireStore 钉住「Store 未装配即不注册」：宁可回退 Node，也不作答读不出数据。
func TestUsersRoutesRequireStore(t *testing.T) {
	guard := &usersStubGuard{principal: adminPrincipal()}
	router := New(Options{Deps: Deps{Guard: guard}})
	RegisterUsersRoutes(router, Deps{Guard: guard})
	if router.RouteCount() != 0 {
		t.Fatalf("Store 未装配时应注册 0 条路由，实际 %d", router.RouteCount())
	}
}

// ---- 校验器（不需要数据库） ----

// TestUsersValidationRejects 钉住入参约束与 400 信封形状。
func TestUsersValidationRejects(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		path       string
		body       string
		status     int
		errorCode  string
		paramsSeen bool
	}{
		{
			name: "list 的 limit 超上限", method: http.MethodGet, path: "/api/v1/users?limit=101",
			status: 400, errorCode: "request.validation_failed", paramsSeen: true,
		},
		{
			name: "list 的 status 非枚举", method: http.MethodGet, path: "/api/v1/users?status=bogus",
			status: 400, errorCode: "request.validation_failed", paramsSeen: true,
		},
		{
			name: "create 缺 name", method: http.MethodPost, path: "/api/v1/users", body: `{}`,
			status: 400, errorCode: "request.validation_failed", paramsSeen: true,
		},
		{
			name: "create 多出未知字段（strict）", method: http.MethodPost, path: "/api/v1/users",
			body: `{"name":"x","bogus":1}`, status: 400, errorCode: "request.validation_failed", paramsSeen: true,
		},
		{
			name: "create 的 dailyResetTime 非法", method: http.MethodPost, path: "/api/v1/users",
			body: `{"name":"x","dailyResetTime":"25:00"}`, status: 400, errorCode: "request.validation_failed",
			paramsSeen: true,
		},
		{
			name: "use-batch 的 userIds 非数组", method: http.MethodPost, path: "/api/v1/users:usageBatch",
			body: `{"userIds":"7"}`, status: 400, errorCode: "request.validation_failed", paramsSeen: true,
		},
		{
			name: "batchUpdate 空 updates", method: http.MethodPost, path: "/api/v1/users:batchUpdate",
			body: `{"userIds":[7],"updates":{}}`, status: 400, errorCode: "EMPTY_UPDATE",
		},
		{
			name: "enable 缺 enabled", method: http.MethodPost, path: "/api/v1/users/7:enable",
			body: `{}`, status: 400, errorCode: "request.validation_failed", paramsSeen: true,
		},
		{
			name: "renew 缺 expiresAt", method: http.MethodPost, path: "/api/v1/users/7:renew",
			body: `{}`, status: 400, errorCode: "request.validation_failed", paramsSeen: true,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			// 校验路径不需要真实数据库：给一个未连接的 Pools 即可（校验失败不会触库）。
			pools, err := store.Open(context.Background(), store.Options{
				DSN: "postgres://127.0.0.1:1/none",
			})
			if err != nil {
				t.Fatalf("建空池失败: %v", err)
			}
			t.Cleanup(func() { _ = pools.Close() })
			router, _ := usersTestRouter(t, pools, adminPrincipal())

			recorder := usersDo(t, router, testCase.method, testCase.path, testCase.body)
			if recorder.Code != testCase.status {
				t.Fatalf("状态码应为 %d，实际 %d body=%s", testCase.status, recorder.Code, recorder.Body.String())
			}
			payload := usersJSON(t, recorder)
			if payload["errorCode"] != testCase.errorCode {
				t.Fatalf("errorCode 应为 %s，实际 %v", testCase.errorCode, payload["errorCode"])
			}
			if testCase.paramsSeen {
				params, ok := payload["invalidParams"].([]any)
				if !ok || len(params) == 0 {
					t.Fatalf("应有 invalidParams，实际 %v", payload)
				}
				first := params[0].(map[string]any)
				if first["code"] == nil || first["message"] == nil || first["path"] == nil {
					t.Fatalf("invalidParams 缺少字段: %v", first)
				}
				if payload["title"] != "Validation failed" ||
					payload["type"] != "urn:claude-code-hub:problem:request.validation_failed" {
					t.Fatalf("Problem 形状不符: %v", payload)
				}
				if recorder.Header().Get("Content-Type") != "application/problem+json" {
					t.Fatalf("Content-Type 应为 application/problem+json，实际 %s",
						recorder.Header().Get("Content-Type"))
				}
			}
		})
	}
}

// TestUsersMaskKeyAndGroups 钉住掩码与分组归一的 Node 语义。
func TestUsersMaskKeyAndGroups(t *testing.T) {
	if got := usersMaskKey("sk-1234567890abcdef"); got != "sk-1••••••cdef" {
		t.Fatalf("掩码不符: %s", got)
	}
	if got := usersMaskKey("short"); got != "••••••" {
		t.Fatalf("短键应全掩: %s", got)
	}
	if got := usersNormalizeProviderGroup(""); got != "default" {
		t.Fatalf("空分组应为 default，实际 %s", got)
	}
	if got := usersNormalizeProviderGroup("b, a，b"); got != "a,b" {
		t.Fatalf("分组归一不符: %s", got)
	}
}

// TestUsersRedactFullKey 钉住 fullKey 的递归剥离。
func TestUsersRedactFullKey(t *testing.T) {
	input := map[string]any{
		"name": "u",
		"keys": []any{
			map[string]any{"id": 1, "fullKey": "sk-secret", "name": "k"},
		},
	}
	redacted := usersRedactFullKey(input)
	encoded, err := json.Marshal(redacted)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	if strings.Contains(string(encoded), "sk-secret") || strings.Contains(string(encoded), "fullKey") {
		t.Fatalf("fullKey 未被剥离: %s", encoded)
	}
}

// TestUsersActionStatusTable 钉住 users 模块自己的状态码表（users/handlers.ts:365-386）。
func TestUsersActionStatusTable(t *testing.T) {
	cases := map[string]int{
		"UNAUTHORIZED":      http.StatusUnauthorized,
		"PERMISSION_DENIED": http.StatusForbidden,
		"NOT_FOUND":         http.StatusNotFound,
		"DATABASE_ERROR":    http.StatusServiceUnavailable,
		"TIMEOUT":           http.StatusServiceUnavailable,
		"INTERNAL_ERROR":    http.StatusInternalServerError,
		"UPDATE_FAILED":     http.StatusInternalServerError,
		"CREATE_FAILED":     http.StatusInternalServerError,
		"DELETE_FAILED":     http.StatusInternalServerError,
		"OPERATION_FAILED":  http.StatusInternalServerError,
		"EMPTY_UPDATE":      http.StatusBadRequest,
		"REQUIRED_FIELD":    http.StatusBadRequest,
	}
	for code, expected := range cases {
		if got := usersActionStatus(code); got != expected {
			t.Fatalf("%s 应映射 %d，实际 %d", code, expected, got)
		}
	}
}

// ---- 端到端（真实 PG） ----

// usersFixture 建一个带密钥的用户，返回 id 与名字；结束时连同密钥一起删除。
func usersFixture(t *testing.T, pools *store.Pools) (int64, string) {
	t.Helper()
	userID := fixtureUser(t, pools, "user", true)
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}
	var name string
	if err := pool.QueryRow(context.Background(),
		`SELECT name FROM users WHERE id = $1`, userID).Scan(&name); err != nil {
		t.Fatalf("读回夹具用户名失败: %v", err)
	}
	fixtureKey(t, pools, fixtureKeyOptions{userID: userID, canLoginWebUI: true, isEnabled: true})
	return userID, name
}

// TestUsersEndpointsRoundTrip 逐端点驱动已实现的 17 条路由。
func TestUsersEndpointsRoundTrip(t *testing.T) {
	pools := testPools(t)
	router, _ := usersTestRouter(t, pools, adminPrincipal())
	userID, userName := usersFixture(t, pools)

	t.Run("GET /users", func(t *testing.T) {
		recorder := usersDo(t, router, http.MethodGet,
			"/api/v1/users?limit=5&q="+userName, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("应 200，实际 %d body=%s", recorder.Code, recorder.Body.String())
		}
		payload := usersJSON(t, recorder)
		items, ok := payload["items"].([]any)
		if !ok || len(items) != 1 {
			t.Fatalf("应命中 1 条，实际 %v", payload["items"])
		}
		item := items[0].(map[string]any)
		if fmt.Sprint(item["id"]) != fmt.Sprint(userID) {
			t.Fatalf("用户 id 不符: %v", item["id"])
		}
		keys, ok := item["keys"].([]any)
		if !ok || len(keys) != 1 {
			t.Fatalf("应带 1 把密钥，实际 %v", item["keys"])
		}
		key := keys[0].(map[string]any)
		if !strings.Contains(fmt.Sprint(key["maskedKey"]), "••••••") {
			t.Fatalf("密钥应被掩码: %v", key["maskedKey"])
		}
		if _, leaked := key["fullKey"]; leaked {
			t.Fatal("列表不得下发完整密钥")
		}
		pageInfo := payload["pageInfo"].(map[string]any)
		if pageInfo["limit"] != float64(5) {
			t.Fatalf("pageInfo.limit 应为 5，实际 %v", pageInfo["limit"])
		}
	})

	t.Run("GET /users/{id}", func(t *testing.T) {
		recorder := usersDo(t, router, http.MethodGet, fmt.Sprintf("/api/v1/users/%d", userID), "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("应 200，实际 %d body=%s", recorder.Code, recorder.Body.String())
		}
		payload := usersJSON(t, recorder)
		if fmt.Sprint(payload["id"]) != fmt.Sprint(userID) {
			t.Fatalf("id 不符: %v", payload["id"])
		}
		if payload["role"] != "user" {
			t.Fatalf("role 应为 user，实际 %v", payload["role"])
		}
	})

	t.Run("GET /users/{id} 不存在", func(t *testing.T) {
		recorder := usersDo(t, router, http.MethodGet, "/api/v1/users/2147483000", "")
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("应 404，实际 %d", recorder.Code)
		}
	})

	t.Run("GET /users/tags", func(t *testing.T) {
		recorder := usersDo(t, router, http.MethodGet, "/api/v1/users/tags", "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("应 200，实际 %d", recorder.Code)
		}
		if _, ok := usersJSON(t, recorder)["items"]; !ok {
			t.Fatal("应返回 items")
		}
	})

	t.Run("GET /users/key-groups", func(t *testing.T) {
		recorder := usersDo(t, router, http.MethodGet, "/api/v1/users/key-groups", "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("应 200，实际 %d", recorder.Code)
		}
		if _, ok := usersJSON(t, recorder)["items"]; !ok {
			t.Fatal("应返回 items")
		}
	})

	t.Run("GET /users:filter-search 与 /users:search", func(t *testing.T) {
		for _, path := range []string{"/api/v1/users:filter-search", "/api/v1/users:search"} {
			recorder := usersDo(t, router, http.MethodGet, path+"?q="+userName+"&limit=5", "")
			if recorder.Code != http.StatusOK {
				t.Fatalf("%s 应 200，实际 %d body=%s", path, recorder.Code, recorder.Body.String())
			}
			payload := usersJSON(t, recorder)
			items, ok := payload["items"].([]any)
			if !ok || len(items) != 1 {
				t.Fatalf("%s 应命中 1 条，实际 %v", path, payload["items"])
			}
		}

		// limit 的两侧各自对应 zod 的最小与最大检查，语义码必须分开
		// （zod 4：0 → too_small，5001 → too_big）。旧实现把两者都报成 too_big，
		// 会让「值太小」在 UI 上被当作「值太大」。
		for _, item := range []struct {
			limit string
			code  string
		}{
			{"0", "too_small"},
			{"5001", "too_big"},
		} {
			recorder := usersDo(t, router, http.MethodGet, "/api/v1/users:search?q=a&limit="+item.limit, "")
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("limit=%s 应 400，实际 %d body=%s", item.limit, recorder.Code, recorder.Body.String())
			}
			var problem struct {
				InvalidParams []struct {
					Code string `json:"code"`
				} `json:"invalidParams"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &problem); err != nil {
				t.Fatalf("limit=%s 校验正文解析失败: %v", item.limit, err)
			}
			if len(problem.InvalidParams) == 0 || problem.InvalidParams[0].Code != item.code {
				t.Fatalf("limit=%s 的 invalidParams[0].code 应为 %s，实际 %+v", item.limit, item.code, problem.InvalidParams)
			}
		}
	})

	t.Run("GET /users:self", func(t *testing.T) {
		selfRouter, _ := usersTestRouter(t, pools, Principal{UserID: userID, Username: userName,
			IsAdmin: false, KeyID: 1})
		recorder := usersDo(t, selfRouter, http.MethodGet, "/api/v1/users:self", "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("应 200，实际 %d body=%s", recorder.Code, recorder.Body.String())
		}
		payload := usersJSON(t, recorder)
		items := payload["items"].([]any)
		if len(items) != 1 || fmt.Sprint(items[0].(map[string]any)["id"]) != fmt.Sprint(userID) {
			t.Fatalf("self 应只返回本人: %v", items)
		}
	})

	// 对拍 D3（users 侧）：ADMIN_TOKEN 合成主体（id -1、role admin）在库里没有对应用户实体，
	// Node 走 listCurrentUser 的 getCurrentUserDisplay → NOT_FOUND → **404**。
	// 曾经的 `principal.UserID <= 0` 把 -1 误判成「认证不完整」而答 401。
	t.Run("GET /users:self 管理令牌主体答 404", func(t *testing.T) {
		tokenRouter, _ := usersTestRouter(t, pools, adminPrincipal())
		recorder := usersDo(t, tokenRouter, http.MethodGet, "/api/v1/users:self", "")
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("应 404，实际 %d body=%s", recorder.Code, recorder.Body.String())
		}
		payload := usersJSON(t, recorder)
		if payload["errorCode"] != "resource.not_found" {
			t.Fatalf("错误码应为 resource.not_found，实际 %v", payload["errorCode"])
		}
	})

	// 401 只留给「无主体」（UserID == 0）：与 Node 的 `if (!currentUserId)` 同判（0 在 JS 里为假值）。
	t.Run("GET /users:self 无主体答 401", func(t *testing.T) {
		nobodyRouter, _ := usersTestRouter(t, pools, Principal{IsAdmin: true})
		recorder := usersDo(t, nobodyRouter, http.MethodGet, "/api/v1/users:self", "")
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("应 401，实际 %d body=%s", recorder.Code, recorder.Body.String())
		}
		if errCode := usersJSON(t, recorder)["errorCode"]; errCode != "auth.missing" {
			t.Fatalf("错误码应为 auth.missing，实际 %v", errCode)
		}
	})

	t.Run("POST /users:usageBatch", func(t *testing.T) {
		recorder := usersDo(t, router, http.MethodPost, "/api/v1/users:usageBatch",
			fmt.Sprintf(`{"userIds":[%d]}`, userID))
		if recorder.Code != http.StatusOK {
			t.Fatalf("应 200，实际 %d body=%s", recorder.Code, recorder.Body.String())
		}
		payload := usersJSON(t, recorder)
		if _, ok := payload["usageByKeyId"].(map[string]any); !ok {
			t.Fatalf("应返回 usageByKeyId 对象: %v", payload)
		}
	})

	t.Run("PATCH /users/{id}", func(t *testing.T) {
		recorder := usersDo(t, router, http.MethodPatch, fmt.Sprintf("/api/v1/users/%d", userID),
			`{"note":"改了备注","tags":["t1","t2"]}`)
		if recorder.Code != http.StatusOK {
			t.Fatalf("应 200，实际 %d body=%s", recorder.Code, recorder.Body.String())
		}
		payload := usersJSON(t, recorder)
		if payload["updated"] != true || fmt.Sprint(payload["id"]) != fmt.Sprint(userID) {
			t.Fatalf("响应不符: %v", payload)
		}
		readBack := usersDo(t, router, http.MethodGet, fmt.Sprintf("/api/v1/users/%d", userID), "")
		detail := usersJSON(t, readBack)
		if detail["description"] != "改了备注" {
			t.Fatalf("备注未落库: %v", detail["description"])
		}
	})

	t.Run("非管理员改他人被拒", func(t *testing.T) {
		otherRouter, _ := usersTestRouter(t, pools, Principal{UserID: userID + 100000, IsAdmin: false})
		recorder := usersDo(t, otherRouter, http.MethodPatch, fmt.Sprintf("/api/v1/users/%d", userID),
			`{"note":"越权"}`)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("应 403，实际 %d body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("非管理员改管理字段被拒", func(t *testing.T) {
		selfRouter, _ := usersTestRouter(t, pools, Principal{UserID: userID, IsAdmin: false})
		recorder := usersDo(t, selfRouter, http.MethodPatch, fmt.Sprintf("/api/v1/users/%d", userID),
			`{"rpm":10}`)
		if recorder.Code != http.StatusForbidden {
			t.Fatalf("应 403，实际 %d body=%s", recorder.Code, recorder.Body.String())
		}
	})

	t.Run("GET /users/{id}/limit-usage", func(t *testing.T) {
		recorder := usersDo(t, router, http.MethodGet,
			fmt.Sprintf("/api/v1/users/%d/limit-usage", userID), "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("应 200，实际 %d body=%s", recorder.Code, recorder.Body.String())
		}
		payload := usersJSON(t, recorder)
		rpm, ok := payload["rpm"].(map[string]any)
		if !ok || rpm["window"] != "per_minute" {
			t.Fatalf("rpm 形状不符: %v", payload["rpm"])
		}
		daily, ok := payload["dailyCost"].(map[string]any)
		if !ok {
			t.Fatalf("缺少 dailyCost: %v", payload)
		}
		if _, ok := daily["current"]; !ok {
			t.Fatal("dailyCost 应含 current")
		}
		if _, ok := daily["resetAt"]; !ok {
			t.Fatal("dailyCost 应含 resetAt")
		}
	})

	t.Run("GET /users/{id}/limit-usage:all", func(t *testing.T) {
		recorder := usersDo(t, router, http.MethodGet,
			fmt.Sprintf("/api/v1/users/%d/limit-usage:all", userID), "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("应 200，实际 %d body=%s", recorder.Code, recorder.Body.String())
		}
		payload := usersJSON(t, recorder)
		for _, field := range []string{"limit5h", "limitDaily", "limitWeekly", "limitMonthly", "limitTotal"} {
			entry, ok := payload[field].(map[string]any)
			if !ok {
				t.Fatalf("缺少 %s: %v", field, payload)
			}
			if _, ok := entry["usage"]; !ok {
				t.Fatalf("%s 应含 usage", field)
			}
			if _, ok := entry["limit"]; !ok {
				t.Fatalf("%s 应含 limit", field)
			}
		}
	})

	t.Run("POST /users/{id}/limits:reset", func(t *testing.T) {
		recorder := usersDo(t, router, http.MethodPost,
			fmt.Sprintf("/api/v1/users/%d/limits:reset", userID), "")
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("应 204，实际 %d body=%s", recorder.Code, recorder.Body.String())
		}
		pool, err := pools.Control()
		if err != nil {
			t.Fatalf("取控制分道失败: %v", err)
		}
		var resetAt, fiveHourResetAt *time.Time
		if err := pool.QueryRow(context.Background(),
			`SELECT cost_reset_at, limit_5h_cost_reset_at FROM users WHERE id = $1`, userID,
		).Scan(&resetAt, &fiveHourResetAt); err != nil {
			t.Fatalf("读回重置标记失败: %v", err)
		}
		if resetAt == nil || fiveHourResetAt == nil {
			t.Fatal("两个重置标记都应被推进")
		}
	})

	t.Run("POST /users/{id}:renew 与 :enable", func(t *testing.T) {
		renew := usersDo(t, router, http.MethodPost, fmt.Sprintf("/api/v1/users/%d:renew", userID),
			`{"expiresAt":"2031-01-02","enableUser":true}`)
		if renew.Code != http.StatusOK {
			t.Fatalf("renew 应 200，实际 %d body=%s", renew.Code, renew.Body.String())
		}
		disable := usersDo(t, router, http.MethodPost, fmt.Sprintf("/api/v1/users/%d:enable", userID),
			`{"enabled":false}`)
		if disable.Code != http.StatusOK {
			t.Fatalf("enable 应 200，实际 %d body=%s", disable.Code, disable.Body.String())
		}
		readBack := usersDo(t, router, http.MethodGet, fmt.Sprintf("/api/v1/users/%d", userID), "")
		detail := usersJSON(t, readBack)
		if detail["isEnabled"] != false {
			t.Fatalf("用户应已禁用: %v", detail["isEnabled"])
		}
		if detail["expiresAt"] == nil {
			t.Fatal("expiresAt 应已写入")
		}
	})

	t.Run("POST /users:batchUpdate", func(t *testing.T) {
		recorder := usersDo(t, router, http.MethodPost, "/api/v1/users:batchUpdate",
			fmt.Sprintf(`{"userIds":[%d],"updates":{"rpm":42}}`, userID))
		if recorder.Code != http.StatusOK {
			t.Fatalf("应 200，实际 %d body=%s", recorder.Code, recorder.Body.String())
		}
		payload := usersJSON(t, recorder)
		if payload["requestedCount"] != float64(1) || payload["updatedCount"] != float64(1) {
			t.Fatalf("计数不符: %v", payload)
		}
	})

	t.Run("POST /users:batchUpdate 有缺失 id", func(t *testing.T) {
		recorder := usersDo(t, router, http.MethodPost, "/api/v1/users:batchUpdate",
			fmt.Sprintf(`{"userIds":[%d,2147483000],"updates":{"rpm":43}}`, userID))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("应 404，实际 %d body=%s", recorder.Code, recorder.Body.String())
		}
		// 事务已回滚：rpm 不应变成 43。
		readBack := usersDo(t, router, http.MethodGet, fmt.Sprintf("/api/v1/users/%d", userID), "")
		if usersJSON(t, readBack)["rpm"] != float64(42) {
			t.Fatalf("缺失 id 时不应有任何写入: %v", usersJSON(t, readBack)["rpm"])
		}
	})

	t.Run("POST /users 与 DELETE /users/{id}", func(t *testing.T) {
		create := usersDo(t, router, http.MethodPost, "/api/v1/users",
			`{"name":"go-adminapi-it-created","note":"新建","tags":["x"],"dailyQuota":12.5}`)
		if create.Code != http.StatusCreated {
			t.Fatalf("应 201，实际 %d body=%s", create.Code, create.Body.String())
		}
		payload := usersJSON(t, create)
		user, ok := payload["user"].(map[string]any)
		if !ok {
			t.Fatalf("应返回 user: %v", payload)
		}
		createdID := int64(user["id"].(float64))
		usersCleanupRow(t, pools, createdID)
		if create.Header().Get("Location") != fmt.Sprintf("/api/v1/users/%d", createdID) {
			t.Fatalf("Location 不符: %s", create.Header().Get("Location"))
		}
		defaultKey, ok := payload["defaultKey"].(map[string]any)
		if !ok {
			t.Fatalf("应附带默认密钥: %v", payload)
		}
		if !strings.HasPrefix(fmt.Sprint(defaultKey["key"]), "sk-") {
			t.Fatalf("默认密钥前缀不符: %v", defaultKey["key"])
		}
		if user["providerGroup"] != "default" {
			t.Fatalf("providerGroup 应归一为 default，实际 %v", user["providerGroup"])
		}

		// withDefaultKey=false 时不建密钥。
		createOnly := usersDo(t, router, http.MethodPost, "/api/v1/users?withDefaultKey=false",
			`{"name":"go-adminapi-it-nokey"}`)
		if createOnly.Code != http.StatusCreated {
			t.Fatalf("应 201，实际 %d body=%s", createOnly.Code, createOnly.Body.String())
		}
		onlyPayload := usersJSON(t, createOnly)
		onlyID := int64(onlyPayload["user"].(map[string]any)["id"].(float64))
		usersCleanupRow(t, pools, onlyID)
		if _, present := onlyPayload["defaultKey"]; present {
			t.Fatal("withDefaultKey=false 时不应创建默认密钥")
		}

		deleteRecorder := usersDo(t, router, http.MethodDelete, fmt.Sprintf("/api/v1/users/%d", createdID), "")
		if deleteRecorder.Code != http.StatusNoContent {
			t.Fatalf("删除应 204，实际 %d body=%s", deleteRecorder.Code, deleteRecorder.Body.String())
		}
		readAfter := usersDo(t, router, http.MethodGet, fmt.Sprintf("/api/v1/users/%d", createdID), "")
		if readAfter.Code != http.StatusNotFound {
			t.Fatalf("软删后应 404，实际 %d", readAfter.Code)
		}
	})
}
