package adminapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// recordingGuard 记录 Wrap 收到的权限档位并把处理器原样返回：本包只负责装配守卫，
// 认证语义由 A0-2 的实现负责，故测试只断言「守卫被按档位调用」。
type recordingGuard struct {
	levels []AccessLevel
}

func (g *recordingGuard) Wrap(level AccessLevel, next http.Handler) http.Handler {
	g.levels = append(g.levels, level)
	return next
}

// recorderHandler 记录被调用次数并按固定状态码作答。
type recorderHandler struct {
	status int
	body   string
	calls  int
	// seenPath 保存最后一次被调用的路径，用于断言兜底拿到的是原始请求。
	seenPath string
}

func (h *recorderHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	h.calls++
	h.seenPath = request.URL.Path
	writer.WriteHeader(h.status)
	_, _ = writer.Write([]byte(h.body))
}

func newTestRouter(t *testing.T) *Router {
	t.Helper()
	return New(Options{Deps: Deps{Guard: &recordingGuard{}}})
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusNoContent)
	})
}

// TestAddRefusesRegistrationWithoutGuard 钉住 fail-closed：认证守卫未装配时不得注册路由，
// 请求应照常回退 Node，并由一条 Error 日志说明原因。
func TestAddRefusesRegistrationWithoutGuard(t *testing.T) {
	logBuffer := &bytes.Buffer{}
	router := New(Options{Deps: Deps{Logger: logx.New(logBuffer)}})
	router.Add(Route{Method: "GET", Path: "/keys/{keyId}", Access: AccessRead, Module: "keys", Handler: okHandler()})

	if router.RouteCount() != 0 {
		t.Fatalf("守卫未装配时应拒绝注册，实际注册了 %d 条", router.RouteCount())
	}
	if !strings.Contains(logBuffer.String(), "admin_auth_unwired") {
		t.Fatalf("应记录 admin_auth_unwired 日志，实际日志：%s", logBuffer.String())
	}
}

// TestUnmatchedAdminPathFallsBack 钉住归属回退：未注册的管理路径交给兜底处理器（生产的 Node 反代）。
func TestUnmatchedAdminPathFallsBack(t *testing.T) {
	router := newTestRouter(t)
	fallback := &recorderHandler{status: http.StatusTeapot, body: `{"from":"node"}`}
	router.SetNotFound(fallback)
	router.Add(Route{Method: "GET", Path: "/keys/{keyId}", Access: AccessRead, Module: "keys", Handler: okHandler()})

	for _, path := range []string{"/api/v1/providers", "/api/v1/keys/12/quota", "/api/v1", "/api/v1/me/keys"} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusTeapot {
			t.Errorf("%s 应回退兜底（418），实际 %d", path, recorder.Code)
		}
	}
	if fallback.calls != 4 {
		t.Fatalf("兜底应被调用 4 次，实际 %d 次", fallback.calls)
	}
	if fallback.seenPath != "/api/v1/me/keys" {
		t.Errorf("兜底应拿到未改写的原始路径，实际 %q", fallback.seenPath)
	}
}

// TestUnmatchedWithoutFallbackAnswers503 钉住「兜底未装配」不得伪装成 404：必须显式 503。
func TestUnmatchedWithoutFallbackAnswers503(t *testing.T) {
	logBuffer := &bytes.Buffer{}
	router := New(Options{Deps: Deps{Logger: logx.New(logBuffer), Guard: &recordingGuard{}}})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/providers", nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("兜底未装配应 503，实际 %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "admin.fallback_unwired") {
		t.Errorf("响应体应含 admin.fallback_unwired，实际 %s", recorder.Body.String())
	}
	if !strings.Contains(logBuffer.String(), "admin_fallback_unwired") {
		t.Errorf("应记录 admin_fallback_unwired 日志，实际 %s", logBuffer.String())
	}
}

// TestServeHTTPStripsMountPrefixAndNormalizes 钉住前缀剥离与归一化：剥前缀、去尾斜杠、
// 合并重复斜杠、忽略查询串。归一化必须复用前门口径，否则会出现「前门判给 Go、Go 找不到路由」
// 的死循环式回退。
func TestServeHTTPStripsMountPrefixAndNormalizes(t *testing.T) {
	router := newTestRouter(t)
	fallback := &recorderHandler{status: http.StatusTeapot}
	router.SetNotFound(fallback)
	handled := 0
	router.Add(Route{
		Method: "GET", Path: "/keys/{keyId}", Access: AccessRead, Module: "keys",
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			handled++
			if got := ParamsFrom(request.Context())["keyId"]; got != "12" {
				t.Errorf("keyId 参数应为 12，实际 %q", got)
			}
			writer.WriteHeader(http.StatusOK)
		}),
	})

	for _, path := range []string{
		"/api/v1/keys/12",
		"/api/v1/keys/12/",
		"/api/v1//keys//12",
		"/api/v1/keys/12?page=2",
	} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Errorf("%s 应命中本进程路由，实际 %d", path, recorder.Code)
		}
	}
	if handled != 4 {
		t.Fatalf("处理器应被调用 4 次，实际 %d 次", handled)
	}
	if fallback.calls != 0 {
		t.Fatalf("不应触发兜底，实际 %d 次", fallback.calls)
	}
}

// TestEnvelopeHeadersOnlyOnOwnResponses 钉住响应头只在 Go 自己作答时下发：
// 兜底响应来自 Node，必须原样带 Node 自己的头，不得被 Go 覆盖。
func TestEnvelopeHeadersOnlyOnOwnResponses(t *testing.T) {
	router := New(Options{Deps: Deps{Guard: &recordingGuard{}}, EnableHSTS: true})
	fallback := &recorderHandler{status: http.StatusOK}
	router.SetNotFound(fallback)
	router.Add(Route{Method: "GET", Path: "/keys/{keyId}", Access: AccessRead, Module: "keys", Handler: okHandler()})

	matched := httptest.NewRecorder()
	router.ServeHTTP(matched, httptest.NewRequest(http.MethodGet, "/api/v1/keys/12", nil))
	want := map[string]string{
		VersionHeader:                         APIVersion,
		"Cache-Control":                       "no-store, no-cache, must-revalidate",
		"Pragma":                              "no-cache",
		"X-Content-Type-Options":              "nosniff",
		"X-Frame-Options":                     "DENY",
		"Referrer-Policy":                     "strict-origin-when-cross-origin",
		"X-DNS-Prefetch-Control":              "off",
		"Content-Security-Policy-Report-Only": defaultCSP,
		"Strict-Transport-Security":           "max-age=31536000; includeSubDomains",
	}
	for name, value := range want {
		if got := matched.Header().Get(name); got != value {
			t.Errorf("命中响应头 %s 应为 %q，实际 %q", name, value, got)
		}
	}

	unmatched := httptest.NewRecorder()
	router.ServeHTTP(unmatched, httptest.NewRequest(http.MethodGet, "/api/v1/providers", nil))
	if got := unmatched.Header().Get(VersionHeader); got != "" {
		t.Errorf("兜底响应不得带 Go 的版本头，实际 %q", got)
	}
}

// TestHSTSFollowsConfig 钉住 HSTS 与 Node 的 enableHsts（ENABLE_SECURE_COOKIES）同源。
func TestHSTSFollowsConfig(t *testing.T) {
	router := newTestRouter(t)
	router.Add(Route{Method: "GET", Path: "/keys/{keyId}", Access: AccessRead, Module: "keys", Handler: okHandler()})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/keys/12", nil))
	if got := recorder.Header().Get("Strict-Transport-Security"); got != "" {
		t.Fatalf("未开启 HSTS 时不应下发该头，实际 %q", got)
	}
}

// TestPairParamsInContext 钉住两参数路由的参数注入（/keys/{keyId}/limits/{field}）。
func TestPairParamsInContext(t *testing.T) {
	router := newTestRouter(t)
	router.Add(Route{
		Method: "PATCH", Path: "/keys/{keyId}/limits/{field}", Access: AccessAdmin, Module: "keys",
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			_ = json.NewEncoder(writer).Encode(ParamsFrom(request.Context()))
		}),
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPatch, "/api/v1/keys/12/limits/maxRpm", nil))

	var params map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &params); err != nil {
		t.Fatalf("响应体不是参数 JSON：%v", err)
	}
	if params["keyId"] != "12" || params["field"] != "maxRpm" {
		t.Fatalf("参数应为 keyId=12, field=maxRpm，实际 %v", params)
	}
}

// TestGuardReceivesDeclaredAccessLevel 钉住权限档位确实交给守卫：档位取自路由声明，
// 若在此处丢档，A0-2 的授权门就会形同虚设。
func TestGuardReceivesDeclaredAccessLevel(t *testing.T) {
	guard := &recordingGuard{}
	router := New(Options{Deps: Deps{Guard: guard}})
	router.Add(Route{Method: "GET", Path: "/keys/{keyId}", Access: AccessRead, Module: "keys", Handler: okHandler()})
	router.Add(Route{Method: "DELETE", Path: "/keys/{keyId}", Access: AccessAdmin, Module: "keys", Handler: okHandler()})

	if len(guard.levels) != 2 || guard.levels[0] != AccessRead || guard.levels[1] != AccessAdmin {
		t.Fatalf("守卫应依次收到 read/admin，实际 %v", guard.levels)
	}
}

// TestDuplicateRouteKeepsFirst 钉住重复注册的行为：保留首次、记 Error、只留一条。
func TestDuplicateRouteKeepsFirst(t *testing.T) {
	logBuffer := &bytes.Buffer{}
	router := New(Options{Deps: Deps{Logger: logx.New(logBuffer), Guard: &recordingGuard{}}})
	first := &recorderHandler{status: http.StatusOK, body: "first"}
	second := &recorderHandler{status: http.StatusOK, body: "second"}
	router.Add(Route{Method: "get", Path: "/keys/{keyId}", Access: AccessRead, Module: "keys", Handler: first})
	router.Add(Route{Method: "GET", Path: "/keys/{keyId}", Access: AccessRead, Module: "keys", Handler: second})

	if router.RouteCount() != 1 {
		t.Fatalf("重复注册应只留一条，实际 %d 条", router.RouteCount())
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/keys/12", nil))
	if recorder.Body.String() != "first" {
		t.Fatalf("应保留首次注册的处理器，实际响应 %q", recorder.Body.String())
	}
	if first.calls != 1 || second.calls != 0 {
		t.Fatalf("调用次数应为 first=1 second=0，实际 first=%d second=%d", first.calls, second.calls)
	}
	if !strings.Contains(logBuffer.String(), "admin_route_duplicate") {
		t.Fatalf("应记录 admin_route_duplicate 日志，实际 %s", logBuffer.String())
	}
}

// TestAddPanicsOnProgrammingErrors 钉住启动期即崩：非法模式与缺处理器不在运行期静默兜底。
func TestAddPanicsOnProgrammingErrors(t *testing.T) {
	cases := []struct {
		name  string
		route Route
	}{
		{"非法模式", Route{Method: "GET", Path: "keys/{keyId}", Access: AccessRead, Handler: okHandler()}},
		{"缺处理器", Route{Method: "GET", Path: "/keys/{keyId}", Access: AccessRead}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			router := newTestRouter(t)
			defer func() {
				if recover() == nil {
					t.Fatalf("%s 应 panic", testCase.name)
				}
			}()
			router.Add(testCase.route)
		})
	}
}

// TestRouteListOmitsHandlers 钉住 RouteList 供契约测试与自报使用：只给路由元数据。
func TestRouteListOmitsHandlers(t *testing.T) {
	router := newTestRouter(t)
	router.Add(Route{Method: "GET", Path: "/keys/{keyId}", Access: AccessRead, Module: "keys", OperationID: "listKeys", Handler: okHandler()})
	routes := router.RouteList()
	if len(routes) != 1 || routes[0].OperationID != "listKeys" || routes[0].Handler != nil {
		t.Fatalf("RouteList 应只含元数据，实际 %+v", routes)
	}
}
