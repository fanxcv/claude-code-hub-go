package adminapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// panicProbe 复刻「探测自身抛错」：Node 的 handleReadinessRequest 用 try/catch 把它归一为
// 503 + {status:"unhealthy",timestamp,error:"Health check failed"}（checker.ts:194-197，
// 本包 health_routes.go 文件头也记了这条语义）。
//
// 这个探针钉住两件事：
//  1. 三个探测 goroutine 里的 panic **不得终止进程**——未捕获的 goroutine panic 会 kill 整个程序，
//     而就绪端点被编排器高频轮询，任何罕见 panic 都会变成容器 crash loop（Node 在同情形只回 503）；
//  2. 归一后的形状与 Node 对齐（503 + 那个 error 字段），且**不得**把「探测失败」误报成健康。
type panicProbe struct {
	which string
}

func (p panicProbe) Database(context.Context) HealthComponent {
	if p.which == "database" {
		panic("boom-database")
	}
	return HealthComponent{Status: healthComponentUp}
}

func (p panicProbe) Redis(context.Context) HealthComponent {
	if p.which == "redis" {
		panic("boom-redis")
	}
	return HealthComponent{Status: healthComponentUp}
}

func (p panicProbe) Proxy(context.Context, string) HealthComponent {
	if p.which == "proxy" {
		panic("boom-proxy")
	}
	return HealthComponent{Status: healthComponentUp}
}

// okProbe 三个组件全 up：用于钉住「正常路径不受 panic 归一的改动影响」。
type okProbe struct{}

func (okProbe) Database(context.Context) HealthComponent {
	return HealthComponent{Status: healthComponentUp}
}
func (okProbe) Redis(context.Context) HealthComponent {
	return HealthComponent{Status: healthComponentUp}
}
func (okProbe) Proxy(context.Context, string) HealthComponent {
	return HealthComponent{Status: healthComponentUp}
}

func healthTestRouter(t *testing.T, probe HealthProbe) *Router {
	t.Helper()
	// Guard 不可省：Router.Add 在 deps.Guard 为 nil 时**拒绝注册**（有意设计，见 Add 的注释），
	// 少了它这条用例会静默落到「未命中兜底」，根本碰不到探针。
	deps := Deps{Logger: logx.New(io.Discard), Problems: NewProblems(nil), Guard: &recordingGuard{}}
	router := New(Options{Deps: deps})
	RegisterHealthRoutes(router, deps, probe)
	if router.RouteCount() == 0 {
		t.Fatal("健康路由一条都没注册：用例会在未命中兜底上打转，等于没测到")
	}
	return router
}

// TestReadinessProbePanicIsContainedAsNodeFailure 是 P0 的验收用例：
// 任一探测 goroutine panic，都应按 Node 的 catch 分支回 503 + 三键失败形状，且进程存活。
func TestReadinessProbePanicIsContainedAsNodeFailure(t *testing.T) {
	for _, which := range []string{"database", "redis", "proxy"} {
		t.Run(which, func(t *testing.T) {
			router := healthTestRouter(t, panicProbe{which: which})

			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/health/ready", nil))

			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("探测 panic 应归一为 503，收到 %d（体：%s）", recorder.Code, recorder.Body.String())
			}

			var body healthFailureResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("失败体不是预期 JSON: %v（原文 %s）", err, recorder.Body.String())
			}
			if body.Status != healthBodyUnhealthy {
				t.Fatalf("status 应为 %q，收到 %q", healthBodyUnhealthy, body.Status)
			}
			if body.Error != healthFailureError {
				t.Fatalf("error 应为 %q，收到 %q", healthFailureError, body.Error)
			}
			if body.Timestamp == "" {
				t.Fatal("timestamp 不应为空")
			}

			// Node 的失败形状只有 status/timestamp/error 三键，不得混入 components 或 version。
			var keys map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &keys); err != nil {
				t.Fatalf("失败体解析失败: %v", err)
			}
			if len(keys) != 3 {
				t.Fatalf("失败形状应只有 3 个键，收到 %d 个：%v", len(keys), keys)
			}
			for _, key := range []string{"status", "timestamp", "error"} {
				if _, ok := keys[key]; !ok {
					t.Fatalf("失败形状缺键 %q（收到 %v）", key, keys)
				}
			}
		})
	}
}

// TestReadinessHealthyPathUnchanged 钉住正常路径：全 up 时仍是 200 与既有成功形状。
func TestReadinessHealthyPathUnchanged(t *testing.T) {
	router := healthTestRouter(t, okProbe{})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/health/ready", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("全 up 应为 200，收到 %d（体：%s）", recorder.Code, recorder.Body.String())
	}

	var body healthCheckResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("成功体不是预期 JSON: %v（原文 %s）", err, recorder.Body.String())
	}
	if body.Status != healthBodyHealthy {
		t.Fatalf("status 应为 %q，收到 %q", healthBodyHealthy, body.Status)
	}
	if body.Components.Database.Status != healthComponentUp {
		t.Fatalf("components.database.status 应为 %q，收到 %q", healthComponentUp, body.Components.Database.Status)
	}
	if body.Version == "" {
		t.Fatal("成功形状应含 version")
	}
}

// TestReadinessDatabaseDownStillUnhealthy 钉住既有判定：数据库 down 仍是 unhealthy + 503
// （意思是「失败形状的引入」不得把这条既有路径改掉）。
func TestReadinessDatabaseDownStillUnhealthy(t *testing.T) {
	router := healthTestRouter(t, downProbe{})

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/health/ready", nil))

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("数据库 down 应为 503，收到 %d", recorder.Code)
	}

	var body healthCheckResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应体解析失败: %v（原文 %s）", err, recorder.Body.String())
	}
	if body.Status != healthBodyUnhealthy {
		t.Fatalf("status 应为 %q，收到 %q", healthBodyUnhealthy, body.Status)
	}
	// 普通失败（非 panic）**不走** catch 分支，故不带 error 字段——这是与 Node 的一致点。
	if body.Components.Database.Status != healthComponentDown {
		t.Fatalf("components.database.status 应为 %q，收到 %q", healthComponentDown, body.Components.Database.Status)
	}
}

type downProbe struct{}

func (downProbe) Database(context.Context) HealthComponent {
	return HealthComponent{Status: healthComponentDown, Message: "Database connection failed"}
}
func (downProbe) Redis(context.Context) HealthComponent {
	return HealthComponent{Status: healthComponentUp}
}
func (downProbe) Proxy(context.Context, string) HealthComponent {
	return HealthComponent{Status: healthComponentUp}
}
