package adminapi

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 「仅重置 5H 限额」（`POST /users/{id}/limits:reset5h`）的钉子。
//
// 该端点是 UI 上「仅重置 5H 限额」按钮的 REST 化替代（原先走 server action，静态导出后不可用），
// 与 `POST /users/{id}/limits:reset`（「重置限额」）**语义不同**。三条不变量：
//  1. **只推进 `limit_5h_cost_reset_at`**，不碰 `cost_reset_at`——两者的分界就是两个按钮的差别，
//     合并即语义漂移；
//  2. **单调**：标记取 `greatest(coalesce(col, $1), $1)`，重复调用（含时钟回拨）不回退；
//  3. 错误码沿用 Node `ERROR_CODES` 的语义名，客户端据此查 `errors` 命名空间拿本地化文案。

func users5hResetRouter(
	t *testing.T,
	pools *store.Pools,
	principal Principal,
	invalid Invalidator,
) *Router {
	t.Helper()
	guard := &usersStubGuard{principal: principal}
	deps := Deps{Guard: guard, Store: pools, Problems: NewProblems(nil), Invalidator: invalid}
	router := New(Options{Deps: deps})
	RegisterUsersRoutes(router, deps)
	return router
}

// 非管理员：守卫放行后由处理器自己的 admin 判定拒绝（403 + PERMISSION_DENIED）。
func TestResetUser5hLimitRequiresAdmin(t *testing.T) {
	// 校验之前的路径不需要真实数据库；给一个未连接的池即可（admin 分支不会触库）。
	pools, err := store.Open(context.Background(), store.Options{DSN: "postgres://127.0.0.1:1/none"})
	if err != nil {
		t.Fatalf("建空池失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })

	router := users5hResetRouter(t, pools, Principal{UserID: 9, Username: "u9"}, nil)
	recorder := usersDo(t, router, http.MethodPost, "/api/v1/users/7/limits:reset5h", "")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("应 403，实际 %d body=%s", recorder.Code, recorder.Body.String())
	}
	if payload := usersJSON(t, recorder); payload["errorCode"] != "PERMISSION_DENIED" {
		t.Fatalf("errorCode 应为 PERMISSION_DENIED，实际 %v", payload["errorCode"])
	}
}

// rolling：只动 5h 标记；已配清理通道时响应不带 cleanupRequired。
func TestResetUser5hRollingOnlyTouches5hMarker(t *testing.T) {
	pools := testPools(t)
	userID := fixtureUser(t, pools, "user", true)
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}
	ctx := context.Background()
	past := time.Now().Add(-3 * time.Hour).UTC().Truncate(time.Millisecond)
	future := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Millisecond)
	if _, err := pool.Exec(ctx,
		`UPDATE users SET limit_5h_usd = 1, limit_5h_reset_mode = 'rolling',
		        cost_reset_at = $1, limit_5h_cost_reset_at = $2
		  WHERE id = $3`,
		past, future, userID,
	); err != nil {
		t.Fatalf("铺夹具失败: %v", err)
	}

	invalidator := &recordingInvalidator{}
	router := users5hResetRouter(t, pools, adminPrincipal(), invalidator)
	recorder := usersDo(t, router, http.MethodPost,
		fmt.Sprintf("/api/v1/users/%d/limits:reset5h", userID), "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", recorder.Code, recorder.Body.String())
	}
	payload := usersJSON(t, recorder)
	if payload["resetMode"] != "rolling" {
		t.Fatalf("resetMode 应为 rolling，实际 %v", payload["resetMode"])
	}
	if _, ok := payload["cleanupRequired"]; ok {
		t.Fatalf("清理通道已装配，不应回 cleanupRequired：%v", payload)
	}

	var gotCost, got5h *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT cost_reset_at, limit_5h_cost_reset_at FROM users WHERE id = $1`, userID,
	).Scan(&gotCost, &got5h); err != nil {
		t.Fatalf("读回标记失败: %v", err)
	}
	if got5h == nil || !got5h.UTC().Equal(future) {
		t.Fatalf("5h 标记应保持 future（单调不回退），实际 %v", got5h)
	}
	if gotCost == nil || !gotCost.UTC().Equal(past) {
		t.Fatalf("cost_reset_at 不应被本端点改动，实际 %v", gotCost)
	}
	if len(invalidator.userCost) == 0 || len(invalidator.userAuth) == 0 {
		t.Fatalf("应清用户成本与认证缓存，实际 cost=%v auth=%v",
			invalidator.userCost, invalidator.userAuth)
	}
}

// rolling + 5h 标记为空：应被推进为非空，且 cost_reset_at 仍为空（只动一列）。
func TestResetUser5hAdvancesNullMarker(t *testing.T) {
	pools := testPools(t)
	userID := fixtureUser(t, pools, "user", true)
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`UPDATE users SET limit_5h_usd = 2, limit_5h_reset_mode = 'rolling',
		        cost_reset_at = NULL, limit_5h_cost_reset_at = NULL
		  WHERE id = $1`, userID,
	); err != nil {
		t.Fatalf("铺夹具失败: %v", err)
	}

	router := users5hResetRouter(t, pools, adminPrincipal(), &recordingInvalidator{})
	recorder := usersDo(t, router, http.MethodPost,
		fmt.Sprintf("/api/v1/users/%d/limits:reset5h", userID), "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%s", recorder.Code, recorder.Body.String())
	}

	var gotCost, got5h *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT cost_reset_at, limit_5h_cost_reset_at FROM users WHERE id = $1`, userID,
	).Scan(&gotCost, &got5h); err != nil {
		t.Fatalf("读回标记失败: %v", err)
	}
	if got5h == nil {
		t.Fatal("5h 标记应被推进为非空")
	}
	if gotCost != nil {
		t.Fatalf("cost_reset_at 应保持为空，实际 %v", gotCost)
	}
}

// 未配置 5h 限额（NULL 或 <= 0）→ 400 + USER_5H_LIMIT_NOT_CONFIGURED（Node 同判据）。
func TestResetUser5hRejectsUnconfiguredLimit(t *testing.T) {
	pools := testPools(t)
	router := users5hResetRouter(t, pools, adminPrincipal(), &recordingInvalidator{})
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}

	cases := []struct {
		name  string
		value string
	}{
		{name: "NULL", value: "NULL"},
		{name: "0", value: "0"},
		{name: "负数", value: "-1"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			userID := fixtureUser(t, pools, "user", true)
			if _, err := pool.Exec(context.Background(),
				fmt.Sprintf(`UPDATE users SET limit_5h_usd = %s, limit_5h_reset_mode = 'rolling' WHERE id = $1`,
					testCase.value), userID,
			); err != nil {
				t.Fatalf("铺夹具失败: %v", err)
			}
			recorder := usersDo(t, router, http.MethodPost,
				fmt.Sprintf("/api/v1/users/%d/limits:reset5h", userID), "")
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("应 400，实际 %d body=%s", recorder.Code, recorder.Body.String())
			}
			if payload := usersJSON(t, recorder); payload["errorCode"] != "USER_5H_LIMIT_NOT_CONFIGURED" {
				t.Fatalf("errorCode 应为 USER_5H_LIMIT_NOT_CONFIGURED，实际 %v", payload["errorCode"])
			}
		})
	}
}

// fixed 模式且清理通道未装配 → 400 + USER_5H_FIXED_RESET_REQUIRES_REDIS（Node 同判据：
// 固定窗口的额度累计只存在于 Redis 运行态，没有它就没法真正重置）。
func TestResetUser5hFixedRequiresCleanupChannel(t *testing.T) {
	pools := testPools(t)
	userID := fixtureUser(t, pools, "user", true)
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE users SET limit_5h_usd = 3, limit_5h_reset_mode = 'fixed' WHERE id = $1`, userID,
	); err != nil {
		t.Fatalf("铺夹具失败: %v", err)
	}

	router := users5hResetRouter(t, pools, adminPrincipal(), nil)
	recorder := usersDo(t, router, http.MethodPost,
		fmt.Sprintf("/api/v1/users/%d/limits:reset5h", userID), "")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("应 400，实际 %d body=%s", recorder.Code, recorder.Body.String())
	}
	if payload := usersJSON(t, recorder); payload["errorCode"] != "USER_5H_FIXED_RESET_REQUIRES_REDIS" {
		t.Fatalf("errorCode 应为 USER_5H_FIXED_RESET_REQUIRES_REDIS，实际 %v", payload["errorCode"])
	}
}

// 用户不存在 → 404 + USER_NOT_FOUND。
func TestResetUser5hUnknownUser(t *testing.T) {
	pools := testPools(t)
	router := users5hResetRouter(t, pools, adminPrincipal(), &recordingInvalidator{})
	recorder := usersDo(t, router, http.MethodPost,
		"/api/v1/users/2147483000/limits:reset5h", "")
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("应 404，实际 %d body=%s", recorder.Code, recorder.Body.String())
	}
	if payload := usersJSON(t, recorder); payload["errorCode"] != "USER_NOT_FOUND" {
		t.Fatalf("errorCode 应为 USER_NOT_FOUND，实际 %v", payload["errorCode"])
	}
}
