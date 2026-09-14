package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/appversion"
)

// 本文件钉住 Node 侧的 `/api/health*` 契约（`src/app/api/health/**` + `src/lib/health/checker.ts`）：
//   1. 三档判定：DB down → unhealthy(503)；Redis 或 Proxy down → degraded(200)；否则 healthy(200)；
//   2. 排空中一律 unhealthy（503），三个组件都是 shutting_down；
//   3. 未配 Redis 是 `unchecked` 而**不是** down（Node 的口径），故不摘流量；
//   4. `/api/health/live` 只证明进程活着（200 + status=alive）；
//   5. 响应体字段名与嵌套形状与 Node 一致（监控按它们匹配）。

func decodeHealth(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("健康响应不是合法 JSON: %v", err)
	}
	return payload
}

// componentOf 取出 components.<name> 这一层（缺失即失败）。
func componentOf(t *testing.T, payload map[string]any, name string) map[string]any {
	t.Helper()
	components, ok := payload["components"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 components 对象: %v", payload)
	}
	component, ok := components[name].(map[string]any)
	if !ok {
		t.Fatalf("components.%s 缺失: %v", name, components)
	}
	return component
}

func TestHealthOKReportsHealthyWithComponents(t *testing.T) {
	server, _ := newTestServer(t, stubProber{pg: StatusOK, redis: StatusOK, rules: StatusOK}, true)

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/health", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("全绿时应 200，收到 %d", recorder.Code)
	}
	payload := decodeHealth(t, recorder.Body.Bytes())
	if payload["status"] != "healthy" {
		t.Fatalf("status 应为 healthy，收到 %v", payload["status"])
	}
	for _, name := range []string{"database", "redis", "proxy"} {
		if got := componentOf(t, payload, name)["status"]; got != "up" {
			t.Fatalf("components.%s.status 应为 up，收到 %v", name, got)
		}
	}
	// Node 的 HealthCheckResponse 必带这四项（uptime 是整数秒、version 无 v 前缀）。
	for _, key := range []string{"timestamp", "version", "uptime"} {
		if _, ok := payload[key]; !ok {
			t.Fatalf("响应缺少字段 %s: %v", key, payload)
		}
	}
}

func TestHealthReadyMatchesHealth(t *testing.T) {
	server, _ := newTestServer(t, stubProber{pg: StatusOK, redis: StatusOK, rules: StatusOK}, true)

	first := httptest.NewRecorder()
	server.Handler().ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	second := httptest.NewRecorder()
	server.Handler().ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/api/health/ready", nil))

	if first.Code != second.Code {
		t.Fatalf("两条路径状态码应一致：/api/health=%d /api/health/ready=%d", first.Code, second.Code)
	}
	// 正文里除 timestamp/uptime（时间相关）外必须同形。
	firstPayload, secondPayload := decodeHealth(t, first.Body.Bytes()), decodeHealth(t, second.Body.Bytes())
	if firstPayload["status"] != secondPayload["status"] {
		t.Fatalf("两条路径 status 应一致：%v vs %v", firstPayload["status"], secondPayload["status"])
	}
}

func TestHealthUnhealthyOnlyWhenDatabaseDown(t *testing.T) {
	testCases := []struct {
		name       string
		prober     stubProber
		wantStatus string
		wantCode   int
	}{
		{"数据库不可用", stubProber{pg: StatusError, redis: StatusOK, rules: StatusOK}, "unhealthy", http.StatusServiceUnavailable},
		{"Redis 不可用", stubProber{pg: StatusOK, redis: StatusError, rules: StatusOK}, "degraded", http.StatusOK},
		{"未配 DSN", stubProber{pg: StatusNotSet, redis: StatusOK, rules: StatusOK}, "unhealthy", http.StatusServiceUnavailable},
		{"未配 Redis", stubProber{pg: StatusOK, redis: StatusNotSet, rules: StatusOK}, "healthy", http.StatusOK},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			server, _ := newTestServer(t, testCase.prober, true)
			recorder := httptest.NewRecorder()
			server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/health", nil))

			if recorder.Code != testCase.wantCode {
				t.Fatalf("状态码应为 %d，收到 %d", testCase.wantCode, recorder.Code)
			}
			payload := decodeHealth(t, recorder.Body.Bytes())
			if payload["status"] != testCase.wantStatus {
				t.Fatalf("status 应为 %s，收到 %v", testCase.wantStatus, payload["status"])
			}
		})
	}
}

func TestHealthUncheckedRedisIsNotDown(t *testing.T) {
	server, _ := newTestServer(t, stubProber{pg: StatusOK, redis: StatusNotSet, rules: StatusOK}, true)

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/health", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("未配 Redis 不该摘流量，收到 %d", recorder.Code)
	}
	if got := componentOf(t, decodeHealth(t, recorder.Body.Bytes()), "redis")["status"]; got != "unchecked" {
		t.Fatalf("未配 Redis 应报 unchecked（Node 口径），收到 %v", got)
	}
}

func TestHealthLiveOnlyProvesProcessAlive(t *testing.T) {
	// prober 为 nil：live 不查依赖，仍须 200。
	server, _ := newTestServer(t, nil, true)

	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/health/live", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("live 应 200，收到 %d", recorder.Code)
	}
	payload := decodeHealth(t, recorder.Body.Bytes())
	if payload["status"] != "alive" {
		t.Fatalf("live 的 status 应为 alive，收到 %v", payload["status"])
	}
	if _, ok := payload["timestamp"]; !ok {
		t.Fatalf("live 应带 timestamp: %v", payload)
	}
}

// 本端点的 version 字段必须与 `internal/appversion` 完全同源。
//
// 为什么单列一条：它曾经自带一条链（env → cwd 的 VERSION 文件 → 本包自己的
// healthDefaultVersion 常量），于是同一个镜像里运维面报 `0.9.0`、管理面报 `v0.9.5`——
// 排障时据此误判过「有两台不同的实例」。本用例同时钉住「注入优先」与「无注入时也走同一实现」。
func TestHealthVersionMatchesSingleSource(t *testing.T) {
	// 注入缝优先（cmd/cchd 就是拿 appversion 的结果喂进来）。
	injected := &Server{version: func() string { return "v9.9.9" }}
	if got := injected.appVersion(); got != "9.9.9" {
		t.Errorf("注入 v9.9.9 时应报 9.9.9（去前缀），收到 %q", got)
	}

	// 未注入时也必须等于唯一实现：设 APP_VERSION 后应报它，而不是本包自己的常量。
	t.Setenv("APP_VERSION", "8.8.8")
	noSeam := &Server{}
	if got := noSeam.appVersion(); got != "8.8.8" {
		t.Errorf("设 APP_VERSION=8.8.8 时应报 8.8.8，收到 %q（不得退回本包常量）", got)
	}
	if got, want := noSeam.appVersion(), appversion.ResolveBare(os.Getenv); got != want {
		t.Errorf("本端点 (%q) 与 appversion.ResolveBare (%q) 必须同值", got, want)
	}
}
