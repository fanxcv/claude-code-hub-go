package adminapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// 本文件钉住 /api/public-site-meta 的三态（有快照 / 无快照 / 读取异常）与版本指针解析。
//
// 快照读取用替身：真实现要连 Redis 并复刻两套键前缀，那是「键布局」的事，单列在下面的
// extractCurrentConfigVersion 用例里覆盖；此处只钉端点作答。

// fakePublicStatusReader 是快照读取替身。
type fakePublicStatusReader struct {
	snapshot *PublicStatusConfigSnapshot
	err      error
}

func (r fakePublicStatusReader) ReadCurrentPublicStatusConfigSnapshot(
	context.Context,
) (*PublicStatusConfigSnapshot, error) {
	return r.snapshot, r.err
}

// pubMetaRouter 造一条只含该端点的路由表。
func pubMetaRouter(t *testing.T, reader PublicStatusSnapshotReader) *Router {
	t.Helper()
	deps := Deps{Logger: logx.New(nil), Guard: &recordingGuard{}}
	if reader != nil {
		deps.PublicStatusSnapshots = reader
	}
	router := New(Options{Deps: deps})
	RegisterPublicStatusMetaRoutes(router, deps)
	return router
}

func pubMetaRequest(t *testing.T, router *Router) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/public-site-meta", nil))
	return recorder
}

func TestPublicSiteMetaWithSnapshot(t *testing.T) {
	timezone := "Asia/Shanghai"
	router := pubMetaRouter(t, fakePublicStatusReader{snapshot: &PublicStatusConfigSnapshot{
		ConfigVersion: "cfg-1",
		SiteTitle:     "  Fixture Hub  ",
		TimeZone:      &timezone,
	}})
	recorder := pubMetaRequest(t, router)
	if recorder.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d", recorder.Code)
	}
	if got := recorder.Header().Get("Cache-Control"); got != publicSiteMetaCacheControl {
		t.Fatalf("有快照时应可缓存 30s，得到 %q", got)
	}
	body := recorder.Body.String()
	for _, fragment := range []string{
		`"available":true`,
		`"siteTitle":"Fixture Hub"`,
		`"siteDescription":"Fixture Hub public status"`,
		`"timeZone":"Asia/Shanghai"`,
		`"source":"projection"`,
	} {
		if !strings.Contains(body, fragment) {
			t.Errorf("响应缺 %s：%s", fragment, body)
		}
	}
	if strings.Contains(body, "reason") {
		t.Errorf("有快照时不应带 reason：%s", body)
	}
}

func TestPublicSiteMetaEmptyFields(t *testing.T) {
	router := pubMetaRouter(t, fakePublicStatusReader{snapshot: &PublicStatusConfigSnapshot{}})
	recorder := pubMetaRequest(t, router)
	if recorder.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d", recorder.Code)
	}
	body := recorder.Body.String()
	for _, fragment := range []string{
		`"siteTitle":null`,
		`"siteDescription":"Request-derived public status"`,
		`"timeZone":null`,
	} {
		if !strings.Contains(body, fragment) {
			t.Errorf("空字段响应缺 %s：%s", fragment, body)
		}
	}
}

func TestPublicSiteMetaWithoutSnapshot(t *testing.T) {
	router := pubMetaRouter(t, fakePublicStatusReader{})
	recorder := pubMetaRequest(t, router)
	if recorder.Code != http.StatusOK {
		t.Fatalf("无快照仍应 200，得到 %d", recorder.Code)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("无快照时应 no-store，得到 %q", got)
	}
	body := recorder.Body.String()
	for _, fragment := range []string{
		`"available":false`, `"siteTitle":null`, `"reason":"projection_missing"`,
	} {
		if !strings.Contains(body, fragment) {
			t.Errorf("无快照响应缺 %s：%s", fragment, body)
		}
	}
}

func TestPublicSiteMetaReaderFailureAnswers503(t *testing.T) {
	router := pubMetaRouter(t, fakePublicStatusReader{err: errors.New("boom")})
	recorder := pubMetaRequest(t, router)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("读取异常应 503，得到 %d", recorder.Code)
	}
	if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("503 应 no-store，得到 %q", got)
	}
	if !strings.Contains(recorder.Body.String(), publicSiteMetaUnavailableError) {
		t.Fatalf("503 正文应带错误文案：%s", recorder.Body.String())
	}
}

// TestPublicStatusMetaRegistrarRefusesWithoutReader 钉住「无 Redis 就不注册」：
// 宁可由 Node 作答，也不让 Go 恒答「无快照」（那会把「没接 Redis」伪装成「投影不存在」）。
func TestPublicStatusMetaRegistrarRefusesWithoutReader(t *testing.T) {
	router := pubMetaRouter(t, nil)
	if router.RouteCount() != 0 {
		t.Fatalf("读取器未装配时不应注册任何路由，得到 %d 条", router.RouteCount())
	}
}

func TestExtractCurrentConfigVersion(t *testing.T) {
	cases := map[string]string{
		"":                           "",
		"cfg-1700000000000":          "cfg-1700000000000",
		`{"configVersion":"cfg-42"}`: "cfg-42",
		`{"key":"public-status:v2:config:cfg-77"}`:          "cfg-77",
		`{"key":"public-status:v2:config-internal:cfg-88"}`: "cfg-88",
		`{"key":"public-status:v2:other:cfg-99"}`:           "",
		`{"nothing":true}`:                                  "",
		"not json at all":                                   "",
	}
	for input, want := range cases {
		if got := extractCurrentConfigVersion(input); got != want {
			t.Errorf("extractCurrentConfigVersion(%q) = %q，期望 %q", input, got, want)
		}
	}
}
