package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住写会话门（write_gate.go）在**每条 read 档写路由**上的落地：只读会话（凭据密钥
// can_login_web_ui=false、非管理员）一律 403 auth.forbidden，且写动作不发生。
//
// 为什么单独一组：read 档位放行的正是只读会话，写动作在档位上没有第二种表达——门在处理器里。
// 因此每条写路由都得有自己的断言，否则新加一条写路由时「忘记过门」不会被任何测试发现。
// keys 的五条写路由的门由 TestAdminKeyWriteSessionGateDeniesReadOnlySession 覆盖；
// 两条终止与一条导出在下面。
func TestWriteSessionGateDeniesReadOnlySessionOnSessionTermination(t *testing.T) {
	readOnly := Principal{UserID: 42, Username: "reader", CanLoginWebUI: false, WebSession: true}
	mutations := []struct {
		name   string
		method string
		target string
		body   string
	}{
		{"single", http.MethodDelete, "/api/v1/sessions/sess-1", ""},
		{"batch", http.MethodPost, "/api/v1/sessions:batchTerminate", `{"sessionIds":["sess-1"]}`},
	}
	for _, item := range mutations {
		t.Run(item.name, func(t *testing.T) {
			router := sessionsWriteGateRouter(readOnly)
			status, body, raw := sessionsRequest(t, router, item.method, item.target, item.body)
			if status != http.StatusForbidden {
				t.Fatalf("只读会话应 403，实际 %d：%s", status, raw)
			}
			if body["errorCode"] != "auth.forbidden" {
				t.Fatalf("errorCode 应为 auth.forbidden，实际 %v", body["errorCode"])
			}
			if body["detail"] != "Read-only sessions cannot terminate sessions." {
				t.Fatalf("detail 不符：%v", body["detail"])
			}
		})
	}

	// 无身份 ⇒ 401 auth.missing，绝不当作合法调用方放行（守卫被绕过时的兜底）。
	router := sessionsWriteGateRouter(Principal{})
	status, body, _ := sessionsRequest(t, router, http.MethodDelete, "/api/v1/sessions/sess-1", "")
	if status != http.StatusUnauthorized || body["errorCode"] != "auth.missing" {
		t.Fatalf("无身份应 401 auth.missing，实际 %d %v", status, body["errorCode"])
	}
}

// sessionsWriteGateRouter 建只带终止路由所需依赖的路由表；零值 *store.Pools 不会被触碰——
// 写门在任何 SQL 之前作答（放行路径由真库集成用例覆盖）。
func sessionsWriteGateRouter(principal Principal) *Router {
	deps := Deps{
		Guard:               principalGuard{principal: principal},
		Problems:            NewProblems(nil),
		Store:               &store.Pools{},
		SessionTerminations: stubTerminator{},
		SessionAffinity:     stubAffinity{},
	}
	router := New(Options{Deps: Deps{Guard: deps.Guard}})
	RegisterSessionsRoutes(router, deps)
	return router
}

// TestWriteSessionGateDeniesReadOnlySessionOnUsageLogsExportCreate 钉住 POST /usage-logs/exports：
// 只读会话 403，且**一个作业键都没写**（导出要占队列与 Redis 配额，不是读动作）。
func TestWriteSessionGateDeniesReadOnlySessionOnUsageLogsExportCreate(t *testing.T) {
	kv := newExportFakeKV()
	readOnly := Principal{UserID: 42, Username: "reader", CanLoginWebUI: false, WebSession: true}
	router := usageLogsExportWriteGateRouter(readOnly, kv)

	status, body, raw := usageLogsExportCreate(t, router, `{"format":"csv"}`)
	if status != http.StatusForbidden {
		t.Fatalf("只读会话应 403，实际 %d：%s", status, raw)
	}
	if body["errorCode"] != "auth.forbidden" {
		t.Fatalf("errorCode 应为 auth.forbidden，实际 %v", body["errorCode"])
	}
	if body["detail"] != "Read-only sessions cannot create exports." {
		t.Fatalf("detail 不符：%v", body["detail"])
	}
	for key := range kv.values {
		if strings.Contains(key, "usage-logs:export:") {
			t.Fatalf("被拒的请求不该写下任何作业键，实际写了 %s", key)
		}
	}

	// 无身份 ⇒ 401 auth.missing，同样不写作业键。
	router = usageLogsExportWriteGateRouter(Principal{}, kv)
	status, body, _ = usageLogsExportCreate(t, router, `{"format":"csv"}`)
	if status != http.StatusUnauthorized || body["errorCode"] != "auth.missing" {
		t.Fatalf("无身份应 401 auth.missing，实际 %d %v", status, body["errorCode"])
	}
}

func usageLogsExportWriteGateRouter(principal Principal, kv UsageLogsExportKV) *Router {
	deps := Deps{
		Guard:            principalGuard{principal: principal},
		Problems:         NewProblems(nil),
		Store:            &store.Pools{},
		UsageLogsExports: kv,
	}
	router := New(Options{Deps: Deps{Guard: deps.Guard}})
	RegisterUsageLogs(router, deps)
	return router
}

func usageLogsExportCreate(
	t *testing.T,
	router *Router,
	body string,
) (int, map[string]any, string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/usage-logs/exports", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
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
