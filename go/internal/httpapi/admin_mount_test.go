package httpapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/adminapi"
	"github.com/fanxcv/claude-code-hub-go/go/internal/egress"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// stubGuard 是管理面的占位守卫：本测试只验证挂载与归属回退，认证语义由 adminapi 的测试覆盖。
type stubGuard struct{}

func (stubGuard) Wrap(_ adminapi.AccessLevel, next http.Handler) http.Handler { return next }

// nodeRecorder 假装 Node：记录收到的路径并作答。
type nodeRecorder struct {
	server *httptest.Server
	paths  []string
}

func newNodeRecorder(t *testing.T) *nodeRecorder {
	t.Helper()
	recorder := &nodeRecorder{}
	recorder.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		recorder.paths = append(recorder.paths, request.URL.Path)
		writer.Header().Set("X-From", "node")
		writer.WriteHeader(http.StatusTeapot)
		_, _ = writer.Write([]byte(`{"from":"node"}`))
	}))
	t.Cleanup(recorder.server.Close)
	return recorder
}

// newAdminTestServer 装配一个真实前端：真前门 + 真挂载 + 未实现终端。
func newAdminTestServer(t *testing.T) (*Server, *bytes.Buffer) {
	t.Helper()
	logBuffer := &bytes.Buffer{}
	logger := logx.New(logBuffer)
	frontDoor := egress.New(logger)

	admin := adminapi.New(adminapi.Options{Deps: adminapi.Deps{Logger: logger, Guard: stubGuard{}}})
	admin.SetNotFound(frontDoor.Unimplemented())
	admin.Add(adminapi.Route{
		Method: "GET", Path: "/keys/{keyId}", Access: adminapi.AccessRead, Module: "keys",
		Handler: http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.Header().Set(adminapi.VersionHeader, adminapi.APIVersion)
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write([]byte(`{"served":"go"}`))
		}),
	})

	server := New(ServerOptions{
		Logger:     logger,
		Prober:     stubProber{pg: StatusOK, redis: StatusOK, rules: StatusOK},
		Listening:  func() bool { return true },
		FrontDoor:  frontDoor.Middleware,
		DataPlane:  frontDoor.Unimplemented(),
		AdminPlane: admin,
	})
	return server, logBuffer
}

// TestAdminPathUnregisteredReturnsNotFound 钉住未注册管理路径的终端语义：
// 本进程没有实现的端点**如实回 404 `not_found`**，不再有第三方承载者（原语义是原样反代 Node）。
func TestAdminPathUnregisteredReturnsNotFound(t *testing.T) {
	server, _ := newAdminTestServer(t)

	for _, path := range []string{
		"/api/v1/keys",         // 同子树、方法未注册
		"/api/v1/providers",    // 未纳入批次 A
		"/api/v1/me/keys",      // 个人面
		"/api/v1/users/7/keys", // A1 lane 尚未实现
		"/api/v1/openapi.json", // 文档面
	} {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusNotFound {
			t.Errorf("%s 应由未实现终端回 404，实际 %d", path, recorder.Code)
		}
		if body := recorder.Body.String(); !strings.Contains(body, `"not_found"`) {
			t.Errorf("%s 的错误体应为 not_found，实际 %s", path, body)
		}
	}
}

// TestAdminPathServedByGoWhenRegistered 钉住命中时由本进程作答，且响应头是 Go 的管理面信封。
func TestAdminPathServedByGoWhenRegistered(t *testing.T) {
	server, _ := newAdminTestServer(t)

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/keys/12", nil))
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"served":"go"`) {
		t.Fatalf("已注册路由应由 Go 作答，实际 %d %s", recorder.Code, recorder.Body.String())
	}
	if header := recorder.Header().Get(adminapi.VersionHeader); header != adminapi.APIVersion {
		t.Errorf("Go 作答应带版本头，实际 %q", header)
	}
}

// TestProbesBypassFrontDoorWithAdminMounted 钉住探针不受管理面挂载影响：探针是运维面，
// 不该进前门，也不该因为 /api/v1 的挂载而被 ServeMux 改道。
func TestProbesBypassFrontDoorWithAdminMounted(t *testing.T) {
	server, _ := newAdminTestServer(t)

	for _, path := range []string{"/readyz", "/v1/_ping"} {
		recorder := httptest.NewRecorder()
		server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Errorf("%s 应为 200，实际 %d", path, recorder.Code)
		}
	}
}

// TestAdminMountWithoutTrailingSlash 钉住 /api/v1 不带尾斜杠时不被 ServeMux 301 跳转。
func TestAdminMountWithoutTrailingSlash(t *testing.T) {
	server, _ := newAdminTestServer(t)

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1", nil))
	if recorder.Code == http.StatusMovedPermanently {
		t.Fatalf("/api/v1 不应发生 301 跳转")
	}
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("/api/v1 未注册时应由未实现终端回 404，实际 %d", recorder.Code)
	}
}

// TestAdminPlaneAbsentKeepsOldBehaviour 钉住未装配管理面时的行为：/api/v1 落 404（不是 500）。
func TestAdminPlaneAbsentKeepsOldBehaviour(t *testing.T) {
	logBuffer := &bytes.Buffer{}
	server := New(ServerOptions{
		Logger:    logx.New(logBuffer),
		Prober:    stubProber{pg: StatusOK, redis: StatusOK, rules: StatusOK},
		Listening: func() bool { return true },
	})
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/keys", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("未装配管理面时 /api/v1 应为 404，实际 %d", recorder.Code)
	}
}
