package adminapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/appversion"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// 本文件钉住 /api/version 的三条分支与一项纯函数（版本比较）。
//
// 版本号的规整与兜底已迁到 internal/appversion（单一实现），故这里只验端点确实走它。
//
// 上游一律指向 httptest：这些用例不得真的出网（GitHub 会限流，且测试不该依赖外网）。
// 时钟与缓存窗口可注入，故「5 分钟缓存」的用例不会真等 5 分钟。

func TestCompareVersions(t *testing.T) {
	// 返回 1 表示 latest 更新（Node 的反直觉语义，必须钉住）。
	cases := []struct {
		current, latest string
		want            int
	}{
		{"v1.0.0", "v1.0.1", 1},
		{"v1.0.1", "v1.0.0", -1},
		{"v1.0.0", "v1.0.0", 0},
		{"1.2", "1.2.0", 0},
		{"v1.0.0", "v1.0.0-rc.1", -1},
		{"v1.0.0-rc.1", "v1.0.0", 1},
		{"v1.0.0-rc.1", "v1.0.0-rc.2", 1},
		{"v1.0.0-rc.2", "v1.0.0-rc.1", -1},
		{"v1.0.0-alpha", "v1.0.0-1", -1},
		{"v2.0.0+build.5", "v2.0.0", 0},
		{"nonsense", "v1.0.0", 0},
	}
	for _, testCase := range cases {
		got := compareVersions(testCase.current, testCase.latest)
		if got != testCase.want {
			t.Errorf("compareVersions(%q, %q) = %d，期望 %d",
				testCase.current, testCase.latest, got, testCase.want)
		}
	}
}

// versionUpstream 是假 GitHub：按路径给固定应答，并记录命中次数（用于钉缓存）。
type versionUpstream struct {
	mu     sync.Mutex
	hits   map[string]int
	server *httptest.Server
}

func newVersionUpstream(t *testing.T, routes map[string]versionUpstreamReply) *versionUpstream {
	t.Helper()
	upstream := &versionUpstream{hits: map[string]int{}}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.mu.Lock()
		upstream.hits[r.URL.Path]++
		upstream.mu.Unlock()
		reply, ok := routes[r.URL.Path]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(reply.status)
		_, _ = w.Write([]byte(reply.body))
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func (u *versionUpstream) count(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits[path]
}

type versionUpstreamReply struct {
	status int
	body   string
}

// versionRouter 造一条只含版本端点的路由表（守卫放行，用例只关心作答）。
func versionRouter(t *testing.T, options VersionOptions) *Router {
	t.Helper()
	deps := Deps{Logger: logx.New(nil), Guard: &recordingGuard{}}
	router := New(Options{Deps: deps})
	RegisterVersionRoutes(router, deps, options)
	return router
}

// versionRequest 发一次 GET /api/version 并回正文。
func versionRequest(t *testing.T, router *Router) (int, string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/version", nil))
	return recorder.Code, recorder.Body.String()
}

func TestVersionRouteReleaseFlow(t *testing.T) {
	upstream := newVersionUpstream(t, map[string]versionUpstreamReply{
		"/repos/fanxcv/claude-code-hub-go/releases/latest": {
			status: http.StatusOK,
			body:   `{"tag_name":"v9.9.9","name":"x","html_url":"https://example.test/r","published_at":"2026-01-01T00:00:00Z"}`,
		},
	})
	router := versionRouter(t, VersionOptions{
		CurrentOverride: "v1.0.0",
		GitHubAPIBase:   upstream.server.URL,
		RawBaseURL:      upstream.server.URL,
	})
	status, body := versionRequest(t, router)
	if status != http.StatusOK {
		t.Fatalf("应 200，得到 %d（body=%s）", status, body)
	}
	for _, fragment := range []string{
		`"current":"v1.0.0"`, `"latest":"v9.9.9"`, `"hasUpdate":true`,
		`"releaseUrl":"https://example.test/r"`, `"publishedAt":"2026-01-01T00:00:00Z"`,
	} {
		if !strings.Contains(body, fragment) {
			t.Errorf("响应缺 %s：%s", fragment, body)
		}
	}
}

func TestVersionRouteReleaseMissingFallsBackToVersionFile(t *testing.T) {
	upstream := newVersionUpstream(t, map[string]versionUpstreamReply{
		"/repos/fanxcv/claude-code-hub-go/releases/latest": {status: http.StatusNotFound},
		"/fanxcv/claude-code-hub-go/main/VERSION":          {status: http.StatusOK, body: "2.0.0\n"},
	})
	router := versionRouter(t, VersionOptions{
		CurrentOverride: "v1.0.0",
		GitHubAPIBase:   upstream.server.URL,
		RawBaseURL:      upstream.server.URL,
	})
	status, body := versionRequest(t, router)
	if status != http.StatusOK {
		t.Fatalf("应 200，得到 %d（body=%s）", status, body)
	}
	if !strings.Contains(body, `"latest":"v2.0.0"`) || !strings.Contains(body, `"hasUpdate":true`) {
		t.Fatalf("应退回 VERSION 文件并判有更新：%s", body)
	}
	if !strings.Contains(body, `"releaseUrl":"https://github.com/fanxcv/claude-code-hub-go/releases"`) {
		t.Fatalf("退回路径的 releaseUrl 应为通用 releases 页：%s", body)
	}
}

func TestVersionRouteNoReleaseAtAll(t *testing.T) {
	upstream := newVersionUpstream(t, map[string]versionUpstreamReply{
		"/repos/fanxcv/claude-code-hub-go/releases/latest": {status: http.StatusNotFound},
		"/fanxcv/claude-code-hub-go/main/VERSION":          {status: http.StatusNotFound},
	})
	router := versionRouter(t, VersionOptions{
		CurrentOverride: "v1.0.0",
		GitHubAPIBase:   upstream.server.URL,
		RawBaseURL:      upstream.server.URL,
	})
	status, body := versionRequest(t, router)
	if status != http.StatusOK {
		t.Fatalf("应 200，得到 %d", status)
	}
	if !strings.Contains(body, `"latest":null`) || !strings.Contains(body, `"message":"暂无发布版本"`) {
		t.Fatalf("无任何发布版本时应给 message 分支：%s", body)
	}
}

func TestVersionRouteUpstreamFailureAnswers500WithCurrent(t *testing.T) {
	upstream := newVersionUpstream(t, map[string]versionUpstreamReply{}) // 一切 404 + 500 的替身见下
	upstream.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	router := versionRouter(t, VersionOptions{
		CurrentOverride: "v1.0.0",
		GitHubAPIBase:   upstream.server.URL,
		RawBaseURL:      upstream.server.URL,
	})
	status, body := versionRequest(t, router)
	if status != http.StatusInternalServerError {
		t.Fatalf("上游全挂且 VERSION 也拿不到时应 500，得到 %d（body=%s）", status, body)
	}
	if !strings.Contains(body, `"error":"无法获取最新版本信息"`) {
		t.Fatalf("500 正文应带错误文案：%s", body)
	}
	if !strings.Contains(body, `"current":"`+appversion.Fallback+`"`) {
		t.Fatalf("500 正文的 current 应为兜底常量 %s：%s", appversion.Fallback, body)
	}
}

func TestVersionRouteDevBuildUsesBranchHead(t *testing.T) {
	upstream := newVersionUpstream(t, map[string]versionUpstreamReply{
		"/repos/fanxcv/claude-code-hub-go/commits/dev": {
			status: http.StatusOK,
			body: `{"sha":"abcdef1234567890","html_url":"https://example.test/c",
				"commit":{"committer":{"date":"2026-03-04T05:06:07Z"}}}`,
		},
	})
	router := versionRouter(t, VersionOptions{
		CurrentOverride: "dev-1234567",
		GitHubAPIBase:   upstream.server.URL,
		RawBaseURL:      upstream.server.URL,
	})
	status, body := versionRequest(t, router)
	if status != http.StatusOK {
		t.Fatalf("应 200，得到 %d", status)
	}
	for _, fragment := range []string{
		`"current":"dev-1234567"`, `"latest":"dev-abcdef1"`, `"hasUpdate":true`,
		`/compare/1234567...abcdef1`, `"publishedAt":"2026-03-04T05:06:07Z"`,
	} {
		if !strings.Contains(body, fragment) {
			t.Errorf("dev 分支响应缺 %s：%s", fragment, body)
		}
	}
}

func TestVersionRouteCachesUpstreamByTTL(t *testing.T) {
	upstream := newVersionUpstream(t, map[string]versionUpstreamReply{
		"/repos/fanxcv/claude-code-hub-go/releases/latest": {
			status: http.StatusOK,
			body:   `{"tag_name":"v9.9.9","html_url":"https://example.test/r"}`,
		},
	})
	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	router := versionRouter(t, VersionOptions{
		CurrentOverride: "v1.0.0",
		GitHubAPIBase:   upstream.server.URL,
		RawBaseURL:      upstream.server.URL,
		Now:             clock,
		CacheTTL:        5 * time.Minute,
	})
	path := "/repos/fanxcv/claude-code-hub-go/releases/latest"
	versionRequest(t, router)
	versionRequest(t, router)
	if hits := upstream.count(path); hits != 1 {
		t.Fatalf("缓存窗口内应只回源一次，实际 %d 次", hits)
	}
	now = now.Add(6 * time.Minute)
	versionRequest(t, router)
	if hits := upstream.count(path); hits != 2 {
		t.Fatalf("窗口过期后应回源，实际 %d 次", hits)
	}
}

// 当前版本号由 internal/appversion 统一取值；这里只验本端点确实走它，
// 且**工作区里放着 VERSION 文件也不影响判定**（文件兜底已废弃，见 appversion 包注释）。
func TestResolveCurrentVersionFollowsAppVersion(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte("3.4.5\n"), 0o600); err != nil {
		t.Fatalf("写 VERSION 失败：%v", err)
	}
	t.Chdir(dir)

	if got := resolveCurrentVersion(func(string) string { return "" }); got != appversion.Fallback {
		t.Fatalf("无注入时应退回 %q，得到 %q（VERSION 文件不得再参与判定）", appversion.Fallback, got)
	}
	if got := resolveCurrentVersion(
		func(name string) string {
			if name == "APP_VERSION" {
				return " 7.0.0 "
			}
			return ""
		},
	); got != "v7.0.0" {
		t.Fatalf("注入版本应被采用并规整为 v7.0.0，得到 %q", got)
	}
}

// 管理面两条端点的版本号必须与 `internal/appversion` 完全同源（/api/version 用展示形态、
// /api/version 的版本号必须与 `internal/appversion` 完全同源（展示形态带 v 前缀）。两处曾各自读
// VERSION 文件/自带常量，于是同一镜像里报出两个不同的版本号——本用例把「同一个实现」钉死。
//
// 健康端点那半（去前缀形态）已随 adminapi 侧被遮蔽的 /api/health* 实现一并删除；那条不变量
// 现在钉在真正作答的 httpapi 侧（internal/httpapi/health_api_test.go 的
// TestHealthVersionSharesOneSource）。
func TestAdminPlaneVersionsShareOneSource(t *testing.T) {
	t.Setenv("APP_VERSION", "9.9.9")

	display, wantDisplay := resolveCurrentVersion(os.Getenv), appversion.Resolve(os.Getenv)
	if display != wantDisplay || display != "v9.9.9" {
		t.Errorf("/api/version 的当前版本应为 %q（v9.9.9），实际 %q", wantDisplay, display)
	}
}
