package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 本文件验证管理面前门的两条自有路由（shell.go）与守卫、Problem 信封的接合。
//
// 它们是「管理面已被 Go 接管」的最小可观测证据：没有这两条路由，`admin_plane_ready` 的 routes
// 恒为 0，A0-3 的装配就等于没有任何可在真实进程上验证的行为。

// shellHarness 装配一个带守卫与兜底路由表。
type shellHarness struct {
	router *Router
	guard  *AuthGuard
}

func newShellHarness(t *testing.T, issuerForwarded bool) shellHarness {
	t.Helper()
	guard := newTestGuard(t, GuardOptions{SessionTokenMode: "opaque", AdminToken: testAdminToken})
	deps := Deps{Guard: guard, Problems: NewProblems(nil)}
	router := New(Options{Deps: deps})

	// 兜底：记录「请求落回了 Node」的事实，而不是让测试看见一个没有来源的 503。
	fallbackHits := 0
	router.SetNotFound(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		fallbackHits++
		writer.WriteHeader(http.StatusBadGateway)
		_, _ = writer.Write([]byte(`{"errorCode":"node_backend_unreachable"}`))
	}))

	var issuer CSRFIssuer
	if issuerForwarded {
		issuer = guard
	}
	RegisterShellRoutes(router, deps, issuer)

	return shellHarness{router: router, guard: guard}
}

// serve 跑一次请求，返回记录器与解析后的正文。
func (h shellHarness) serve(t *testing.T, method, target string, headers map[string]string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	request := httptest.NewRequest(method, target, nil)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	h.router.ServeHTTP(recorder, request)

	response := recorder.Result()
	defer func() { _ = response.Body.Close() }()
	body := map[string]any{}
	_ = json.NewDecoder(response.Body).Decode(&body)
	return recorder, body
}

// TestShellRouteTableIsRegistered 验证两条路由都注册上了（routes 非 0 的同一口径）。
func TestShellRouteTableIsRegistered(t *testing.T) {
	harness := newShellHarness(t, true)
	if harness.router.RouteCount() != 2 {
		t.Fatalf("应注册 2 条前门路由，实际 %d", harness.router.RouteCount())
	}
	paths := map[string]AccessLevel{}
	for _, route := range harness.router.RouteList() {
		paths[route.Method+" "+route.Path] = route.Access
	}
	if paths["GET /health"] != AccessPublic {
		t.Fatalf("/health 应为 public 档位: %v", paths["GET /health"])
	}
	if paths["GET /auth/csrf"] != AccessRead {
		t.Fatalf("/auth/csrf 应为 read 档位: %v", paths["GET /auth/csrf"])
	}
}

// TestShellHealthIsPublic 验证 /health 不认证即作答，且正文与响应头逐字对齐 Node。
func TestShellHealthIsPublic(t *testing.T) {
	harness := newShellHarness(t, true)
	recorder, body := harness.serve(t, http.MethodGet, "/api/v1/health", nil)

	if recorder.Code != http.StatusOK {
		t.Fatalf("无凭据应 200（public 档位），实际 %d: %v", recorder.Code, body)
	}
	if body["status"] != "ok" || body["apiVersion"] != APIVersion {
		t.Fatalf("正文不符: %v", body)
	}
	// 逐字节对齐 Node 的 JSON.stringify({status,apiVersion})：键顺序也在契约里。
	if encoded := recorder.Body.String(); encoded != `{"status":"ok","apiVersion":"1.0.0"}` {
		t.Fatalf("正文不是 Node 的形状: %s", encoded)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type 应逐字为 application/json，实际 %q", got)
	}
	if got := recorder.Header().Get(VersionHeader); got != APIVersion {
		t.Fatalf("缺版本头: %q", got)
	}
}

// TestShellCSRFWithoutCredentialsAnswersProblem 验证 /auth/csrf 的未认证作答是 Node 的问题信封，
// 而不是「回退 Node 后端失败」——后者说明路由根本没注册在 Go 上。
func TestShellCSRFWithoutCredentialsAnswersProblem(t *testing.T) {
	harness := newShellHarness(t, true)
	recorder, body := harness.serve(t, http.MethodGet, "/api/v1/auth/csrf", nil)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("无凭据应 401，实际 %d: %v", recorder.Code, body)
	}
	if body["errorCode"] != "auth.missing" {
		t.Fatalf("错误码不符: %v", body)
	}
	if body["status"] != float64(http.StatusUnauthorized) || body["title"] == "" || body["detail"] == "" {
		t.Fatalf("信封字段不全: %v", body)
	}
	if body["instance"] != "/api/v1/auth/csrf" {
		t.Fatalf("instance 应取请求路径: %v", body["instance"])
	}
}

// TestShellCSRFIssuesVerifiableToken 验证签发的 token 能被同一份推导校验通过（签发与校验同源）。
func TestShellCSRFIssuesVerifiableToken(t *testing.T) {
	harness := newShellHarness(t, true)
	recorder, body := harness.serve(t, http.MethodGet, "/api/v1/auth/csrf", map[string]string{
		"Authorization": "Bearer " + testAdminToken,
	})

	if recorder.Code != http.StatusOK {
		t.Fatalf("带 ADMIN_TOKEN 应 200，实际 %d: %v", recorder.Code, body)
	}
	token, _ := body["csrfToken"].(string)
	if token == "" {
		t.Fatalf("未签发 csrfToken: %v", body)
	}
	// 独立复刻的公式（auth_test.go 的 csrfFor）必须给出同一个 token：这是「与 Node 同源」的证据。
	want := csrfFor(testAdminToken, testAdminToken, adminPrincipalUserID, testClock)
	if token != want {
		t.Fatalf("签发的 token 与 Node 公式不符:\n got %s\nwant %s", token, want)
	}
	if !harness.guard.verifyCSRF(token, testAdminToken, adminPrincipalUserID) {
		t.Fatal("签发的 token 应能被守卫自己的 CSRF 门验过")
	}
}

// TestShellCSRFUnwiredFallsBackToNode 验证签发能力缺席时不注册该路由（请求照常回退 Node）。
func TestShellCSRFUnwiredFallsBackToNode(t *testing.T) {
	harness := newShellHarness(t, false)
	if harness.router.RouteCount() != 1 {
		t.Fatalf("签发器缺席时应只注册 /health，实际 %d 条", harness.router.RouteCount())
	}
	recorder, _ := harness.serve(t, http.MethodGet, "/api/v1/auth/csrf", nil)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("未注册路由应落回 Node 兜底，实际 %d", recorder.Code)
	}
}
