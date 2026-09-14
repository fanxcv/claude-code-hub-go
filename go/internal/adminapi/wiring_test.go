package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 本文件验证 A0-2 的四个实现在 Router（A0-1 的冻结面）上装得起来、跑得通。
//
// 为什么单独一个文件：四个实现各自的单元测试证明「各自对」，这里证明「合起来能用」——
// Deps 装配后路由能注册、请求能过守卫、身份能传到处理器、拒绝了会用 ProblemWriter 作答。

// TestWiringRegistersRoutesAndRunsGuard 验证装配后路由可注册且守卫生效。
func TestWiringRegistersRoutesAndRunsGuard(t *testing.T) {
	guard := newTestGuard(t, GuardOptions{})
	problems := NewProblems(nil)

	router := New(Options{Deps: Deps{
		Guard:    guard,
		Problems: problems,
	}})

	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		principal, ok := PrincipalFrom(request.Context())
		if !ok {
			writer.WriteHeader(http.StatusInternalServerError)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"userId":   principal.UserID,
			"isAdmin":  principal.IsAdmin,
			"keyId":    principal.KeyID,
			"username": principal.Username,
		})
	})
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/keys",
		Access:      AccessAdmin,
		Module:      "keys",
		OperationID: "keys_list",
		Handler:     handler,
	})
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/usage-logs",
		Access:      AccessRead,
		Module:      "usage-logs",
		OperationID: "usage_logs_list",
		Handler:     handler,
	})
	if router.RouteCount() != 2 {
		t.Fatalf("装配后应注册 2 条路由，实际 %d", router.RouteCount())
	}

	authorizedRequest := httptest.NewRequest(http.MethodGet, "/api/v1/keys", nil)
	authorizedRequest.Header.Set("Authorization", "Bearer "+testAdminToken)
	authorized := httptest.NewRecorder()
	router.ServeHTTP(authorized, authorizedRequest)
	if authorized.Code != http.StatusOK {
		t.Fatalf("有效管理员令牌应 200，实际 %d body=%s", authorized.Code, authorized.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(authorized.Body.Bytes(), &payload); err != nil {
		t.Fatalf("处理器响应解析失败: %v", err)
	}
	if payload["isAdmin"] != true || payload["userId"] != float64(-1) {
		t.Fatalf("身份未传到处理器: %v", payload)
	}
	if authorized.Header().Get(VersionHeader) != APIVersion {
		t.Fatalf("响应头缺少版本号: %v", authorized.Header())
	}

	unauthorized := httptest.NewRecorder()
	router.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/v1/keys", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("缺凭据应 401，实际 %d", unauthorized.Code)
	}
	if contentType := unauthorized.Header().Get("Content-Type"); contentType != problemContentType {
		t.Fatalf("拒绝应由 ProblemWriter 作答（Content-Type=%q）", contentType)
	}
	var problemBody map[string]any
	if err := json.Unmarshal(unauthorized.Body.Bytes(), &problemBody); err != nil {
		t.Fatalf("Problem 正文解析失败: %v", err)
	}
	if problemBody["errorCode"] != "auth.missing" {
		t.Fatalf("错误码不符: %v", problemBody)
	}
}

// TestWiringWithoutGuardRegistersNothing 验证 fail-closed：无守卫时路由一条都不注册。
func TestWiringWithoutGuardRegistersNothing(t *testing.T) {
	router := New(Options{Deps: Deps{Logger: nil}})
	router.Add(Route{
		Method:  http.MethodGet,
		Path:    "/keys",
		Access:  AccessAdmin,
		Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
	})
	if router.RouteCount() != 0 {
		t.Fatalf("无守卫时不该注册任何路由，实际 %d", router.RouteCount())
	}
}

// TestWiringFullDepsCompiles 验证四个实现与 Deps 的装配形态（与 cmd 侧 wiring 同形）。
//
// 这条测试的作用是「装配形态可编译且构造不报错」：真实接线在 cmd/cchd（A0-1/A2 负责），
// 本 lane 不改那里，故在这里钉住构造签名。
func TestWiringFullDepsCompiles(t *testing.T) {
	pools := testPools(t)
	if err := pools.Close(); err != nil {
		t.Fatalf("关闭连接池失败: %v", err)
	}

	guard, err := NewAuthGuard(Deps{}, GuardOptions{
		Pools:            pools,
		SessionTokenMode: "opaque",
		AdminToken:       testAdminToken,
	})
	if err != nil {
		t.Fatalf("装配守卫失败: %v", err)
	}
	defer guard.Close()

	problems := NewProblems(nil)
	// 审计用已关闭的连接池：写入必然失败，用于验证 fire-and-forget 路径不会 panic。
	audit := &AuditLog{pools: pools}
	invalidator := NewCacheInvalidator(Deps{}, InvalidatorOptions{})

	deps := Deps{
		Guard:       guard,
		Problems:    problems,
		Audit:       audit,
		Invalidator: invalidator,
	}
	router := New(Options{Deps: deps, EnableHSTS: true})
	router.Add(Route{
		Method:  http.MethodGet,
		Path:    "/model-prices",
		Access:  AccessRead,
		Module:  "model-prices",
		Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
	})
	if router.RouteCount() != 1 {
		t.Fatalf("应注册 1 条路由，实际 %d", router.RouteCount())
	}

	// 审计与失效广播在未接线时必须是空操作，不得 panic。
	deps.Audit.Emit(context.Background(), AuditEvent{Action: "key.create"})
	deps.Invalidator.InvalidateKeyAuth(context.Background(), "sk-noop")
}
