package uiapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

// testAssets 造一份最小产物：两个带 locale 的壳（一个用显式标记、一个只有 </head>）、
// 一个既无标记也无 </head> 的壳（用于证明「结构异常时原样透传」）、一条哈希 chunk 与一个 favicon。
func testAssets() fstest.MapFS {
	return fstest.MapFS{
		"assets/.gitkeep":                          &fstest.MapFile{},
		"assets/BUILD_ID":                          &fstest.MapFile{Data: []byte("build-test01\n")},
		"assets/index.html":                        &fstest.MapFile{Data: []byte("<!doctype html><html><head><title>root</title></head><body>root-shell</body></html>")},
		"assets/zh-CN/index.html":                  &fstest.MapFile{Data: []byte("<!doctype html><html><head><!--CCH_BOOTSTRAP--><title>zh</title></head><body>zh-shell</body></html>")},
		"assets/en/index.html":                     &fstest.MapFile{Data: []byte("<!doctype html><html><head><title>en</title></head><body>en-shell</body></html>")},
		"assets/ja/index.html":                     &fstest.MapFile{Data: []byte("<html><body>no-head-shell</body></html>")},
		"assets/usage-doc/index.html":              &fstest.MapFile{Data: []byte("<!doctype html><html><head></head><body>docs</body></html>")},
		"assets/_next/static/chunks/app-abc123.js": &fstest.MapFile{Data: []byte("console.log(1)")},
		"assets/favicon.ico":                       &fstest.MapFile{Data: []byte{0x00, 0x01, 0x02}},
	}
}

// testLocales 是契约里的注册 locale（src/i18n/config.ts 的 locales）。
var testLocales = []string{"zh-CN", "zh-TW", "en", "ru", "ja"}

func newTestHandler(t *testing.T, options Options) *Handler {
	t.Helper()
	if options.Locales == nil {
		options.Locales = testLocales
	}
	handler, err := newHandler(testAssets(), assetsRoot, options)
	if err != nil {
		t.Fatalf("建处理器失败: %v", err)
	}
	return handler
}

// adminResolver 恒返回管理员会话（并统计调用次数）。
func adminResolver(calls *int) SessionResolver {
	return SessionResolverFunc(func(context.Context, *http.Request) (Session, bool, error) {
		if calls != nil {
			*calls++
		}
		return Session{
			User: UserSnapshot{ID: -1, Name: "Admin Token", Role: "admin"},
			Key:  &KeySnapshot{CanLoginWebUI: true},
		}, true, nil
	})
}

// 择路优先级：静态文件 > 目录 index > locale 壳 > 根壳；locale 只认已注册的一级段。
// 不在产物里的**页面路径**仍回落壳（SPA 语义），资源/API 路径则 notFound（见下一条用例）。
func TestResolvePriority(t *testing.T) {
	handler := newTestHandler(t, Options{})

	cases := []struct {
		path   string
		key    string
		locale string
	}{
		{"/", rootShell, "zh-CN"},
		{"/_next/static/chunks/app-abc123.js", "_next/static/chunks/app-abc123.js", "zh-CN"},
		{"/favicon.ico", "favicon.ico", "zh-CN"},
		{"/usage-doc", "usage-doc/index.html", "zh-CN"},
		{"/dashboard", rootShell, "zh-CN"},
		{"/zh-CN/dashboard", "zh-CN/index.html", "zh-CN"},
		{"/en/dashboard", "en/index.html", "en"},
		{"/de/dashboard", rootShell, "zh-CN"},
		{"/en/", "en/index.html", "en"},
	}
	for _, tc := range cases {
		key, locale, notFound := handler.resolve(tc.path)
		if notFound {
			t.Errorf("%s 是页面路径，应回落壳而不是 404", tc.path)
			continue
		}
		if key != tc.key || locale != tc.locale {
			t.Errorf("%s 应择路到 %s（locale %s），实际 %s（locale %s）", tc.path, tc.key, tc.locale, key, locale)
		}
	}
}

// 产物里没有的**资源/API 路径**必须 404，不得回落 HTML 壳。
//
// 为什么要钉：旧行为下 `/zh-CN/_next/static/chunks/missing.js` 会拿到一份 HTML（200），
// 浏览器把它当 JS 解析报语法错误——把「产物少了个 chunk」误诊成「代码坏了」；
// 图标/字体请求也会拿 HTML 去解码；API 前缀同理（多起诊断因此跑偏）。
func TestResolveAssetAndAPIMissIsNotFound(t *testing.T) {
	handler := newTestHandler(t, Options{})

	cases := []string{
		"/_next/static/chunks/missing.js",
		"/_next/static/css/missing.css",
		"/zh-CN/_next/static/chunks/missing.js",
		"/en/assets/missing.svg",
		"/favicon-missing.ico",
		"/assets/fonts/missing.woff2",
		"/robots.txt",
		// API 前缀：mux 正常已认领，但没配 DSN 时管理面未挂载，终端必须 fail closed。
		"/api/v1/users",
		"/api",
		"/v1/messages",
		"/v1beta/models",
	}
	for _, missing := range cases {
		if _, _, notFound := handler.resolve(missing); !notFound {
			t.Errorf("%s 不在产物里且不是页面路径，应 404（不得回落 HTML 壳）", missing)
		}
	}

	// 点在目录里的页面路径仍按页面处理（扩展名只看末段）。
	if _, _, notFound := handler.resolve("/a.b/route"); notFound {
		t.Error("/a.b/route 是页面路径（扩展名只在末段才判为资源），不应 404")
	}
	// 已存在的资源不受影响。
	if _, _, notFound := handler.resolve("/_next/static/chunks/app-abc123.js"); notFound {
		t.Error("存在的 chunk 不应被判为 404")
	}
}

// 线上响应层：缺失资源 → 404 + 纯文本，且**不得**带壳的引导数据（否则等于泄漏一份 HTML）。
func TestServeMissingAssetAnswers404(t *testing.T) {
	handler := newTestHandler(t, Options{})

	for _, tc := range []struct {
		path        string
		contentType string
	}{
		{"/_next/static/chunks/missing.js", "text/plain"},
		{"/zh-CN/_next/static/chunks/missing.js", "text/plain"},
		{"/robots.txt", "text/plain"},
		{"/api/v1/users", "text/plain"},
	} {
		response := do(handler, http.MethodGet, tc.path, nil)
		if response.Code != http.StatusNotFound {
			t.Errorf("%s 应 404，实际 %d", tc.path, response.Code)
		}
		if got := response.Header().Get("Content-Type"); !strings.HasPrefix(got, tc.contentType) {
			t.Errorf("%s 的 Content-Type 应为 %s，实际 %q", tc.path, tc.contentType, got)
		}
		body := response.Body.String()
		if strings.Contains(body, "__CCH_BOOTSTRAP__") || strings.Contains(strings.ToLower(body), "<html") {
			t.Errorf("%s 的 404 正文不得是壳：%q", tc.path, body)
		}
	}

	// HEAD 也应是 404（不因方法不同而异）。
	if response := do(handler, http.MethodHead, "/_next/static/chunks/missing.js", nil); response.Code != http.StatusNotFound {
		t.Errorf("HEAD 缺失资源应 404，实际 %d", response.Code)
	}

	// 存在的资源仍是 200，壳仍注入引导数据（确认没修坏正常路径）。
	if response := do(handler, http.MethodGet, "/_next/static/chunks/app-abc123.js", nil); response.Code != http.StatusOK {
		t.Errorf("存在的 chunk 应 200，实际 %d", response.Code)
	}
	if response := do(handler, http.MethodGet, "/zh-CN/dashboard", nil); response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), "__CCH_BOOTSTRAP__") {
		t.Errorf("应用路由应仍 200 且带引导数据，实际 %d", response.Code)
	}
}

// 缓存头与协商缓存：哈希产物 immutable、壳 no-cache 且 ETag 参与 304，HEAD 不带正文。
func TestServeHeadersAndConditionalRequest(t *testing.T) {
	handler := newTestHandler(t, Options{})

	chunk := do(handler, http.MethodGet, "/_next/static/chunks/app-abc123.js", nil)
	if chunk.Code != http.StatusOK {
		t.Fatalf("哈希产物应 200，实际 %d", chunk.Code)
	}
	if got := chunk.Header().Get("Cache-Control"); got != "public, max-age=31536000, immutable" {
		t.Errorf("哈希产物应 immutable，实际 %q", got)
	}
	if got := chunk.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/javascript") {
		t.Errorf("哈希产物 MIME 应为 text/javascript，实际 %q", got)
	}

	shell := do(handler, http.MethodGet, "/zh-CN/dashboard", nil)
	if got := shell.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("壳应 no-cache，实际 %q", got)
	}
	etag := shell.Header().Get("ETag")
	if etag == "" {
		t.Fatal("壳应带 ETag")
	}

	notModified := do(handler, http.MethodGet, "/zh-CN/dashboard",
		map[string]string{"If-None-Match": etag})
	if notModified.Code != http.StatusNotModified || notModified.Body.Len() != 0 {
		t.Errorf("命中 ETag 应 304 且无正文，实际 %d %q", notModified.Code, notModified.Body.String())
	}

	anyTag := do(handler, http.MethodGet, "/zh-CN/dashboard",
		map[string]string{"If-None-Match": "*"})
	if anyTag.Code != http.StatusNotModified {
		t.Errorf("If-None-Match: * 应 304，实际 %d", anyTag.Code)
	}

	weakList := do(handler, http.MethodGet, "/zh-CN/dashboard",
		map[string]string{"If-None-Match": `W/"other", ` + etag})
	if weakList.Code != http.StatusNotModified {
		t.Errorf("多值弱校验应 304，实际 %d", weakList.Code)
	}

	head := do(handler, http.MethodHead, "/zh-CN/dashboard", nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Errorf("HEAD 应 200 且无正文，实际 %d %q", head.Code, head.Body.String())
	}
}

// 方法与穿越防护：非 GET/HEAD 405（带 Allow），原始路径含 .. 400。
func TestRejectMethodAndTraversal(t *testing.T) {
	handler := newTestHandler(t, Options{})

	post := do(handler, http.MethodPost, "/zh-CN/dashboard", nil)
	if post.Code != http.StatusMethodNotAllowed || post.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("POST 应 405 且带 Allow，实际 %d %q", post.Code, post.Header().Get("Allow"))
	}

	// httptest 的 URL 解析会归一化 `..`，故直接改 URL.Path 模拟原始 socket 送达的路径。
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.URL.Path = "/../../etc/passwd"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Errorf("含 .. 的原始路径应 400，实际 %d", recorder.Code)
	}
}

// 注入三态：
//  1. 有显式标记 → 在标记处整段替换（标记消失）；
//  2. 无标记有 </head> → 插在 </head> 之前；
//  3. 两者都无 → 原样透传（改结构异常的壳比不注入更危险）；
//
// 另外证明两条不变量：体积超限跳过注入；静态产物永不注入。
func TestBootstrapInjection(t *testing.T) {
	handler := newTestHandler(t, Options{
		Sessions: adminResolver(nil),
		Meta: func(context.Context) (Meta, error) {
			return Meta{SiteTitle: "CC Hub", TimeZone: "Asia/Shanghai", Version: "v9.9.9"}, nil
		},
	})

	// 1) 显式标记。
	withMarker := do(handler, http.MethodGet, "/zh-CN/dashboard", nil)
	if strings.Contains(withMarker.Body.String(), bootstrapMarker) {
		t.Error("显式标记应被整段替换，不应留在响应里")
	}
	payload := extractBootstrap(t, withMarker.Body.String())
	if payload.Session == nil || payload.Session.User.Role != "admin" {
		t.Fatalf("注入内容应带 user.role=admin，实际 %+v", payload.Session)
	}
	if payload.Session.Key == nil || !payload.Session.Key.CanLoginWebUI {
		t.Errorf("注入内容应带 key.canLoginWebUi=true，实际 %+v", payload.Session.Key)
	}
	if payload.Locale != "zh-CN" || payload.SiteTitle != "CC Hub" || payload.TimeZone != "Asia/Shanghai" || payload.Version != "v9.9.9" {
		t.Errorf("注入内容应带 locale/siteTitle/timeZone/version，实际 %+v", payload)
	}
	if withMarker.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Errorf("壳 MIME 应为 text/html，实际 %q", withMarker.Header().Get("Content-Type"))
	}

	// 2) 无标记有 </head>：注入点紧邻 </head> 之前。
	byHead := do(handler, http.MethodGet, "/en/dashboard", nil)
	body := byHead.Body.String()
	idxTag := strings.Index(body, "__CCH_BOOTSTRAP__")
	idxHead := strings.Index(body, headClose)
	if idxTag < 0 || idxHead < 0 || idxTag > idxHead {
		t.Fatalf("无标记时应插在 </head> 之前，实际 tag@%d head@%d：%s", idxTag, idxHead, body)
	}
	if payload := extractBootstrap(t, body); payload.Locale != "en" {
		t.Errorf("en 壳应注入 locale=en，实际 %q", payload.Locale)
	}

	// 3) 两者都无：原样透传。
	noHead := do(handler, http.MethodGet, "/ja/dashboard", nil)
	if strings.Contains(noHead.Body.String(), "__CCH_BOOTSTRAP__") {
		t.Error("既无标记也无 </head> 时不应注入")
	}
	if !strings.Contains(noHead.Body.String(), "no-head-shell") {
		t.Errorf("壳正文应原样返回，实际 %q", noHead.Body.String())
	}

	// 4) 静态产物不注入。
	asset := do(handler, http.MethodGet, "/_next/static/chunks/app-abc123.js", nil)
	if strings.Contains(asset.Body.String(), "__CCH_BOOTSTRAP__") {
		t.Error("静态资源不应注入引导数据")
	}
	rootAsset := do(handler, http.MethodGet, "/favicon.ico", nil)
	if strings.Contains(rootAsset.Body.String(), "__CCH_BOOTSTRAP__") {
		t.Error("根级二进制产物不应注入引导数据")
	}

	// 5) 体积超限跳过注入。
	tiny := newTestHandler(t, Options{ShellMaxBytes: 16, Sessions: adminResolver(nil)})
	oversized := do(tiny, http.MethodGet, "/zh-CN/dashboard", nil)
	if strings.Contains(oversized.Body.String(), "__CCH_BOOTSTRAP__") {
		t.Error("壳超过体积上限时不应注入")
	}
}

// 会话解析失败与未登录都必须答 200（壳照常返回，session 为 null），绝不因此让页面白屏。
func TestSessionFailuresDoNotBreakShell(t *testing.T) {
	anonymous := newTestHandler(t, Options{
		Sessions: SessionResolverFunc(func(context.Context, *http.Request) (Session, bool, error) {
			return Session{}, false, nil
		}),
	})
	anonymousBody := do(anonymous, http.MethodGet, "/zh-CN/dashboard", nil)
	if anonymousBody.Code != http.StatusOK {
		t.Fatalf("未登录应 200，实际 %d", anonymousBody.Code)
	}
	if payload := extractBootstrap(t, anonymousBody.Body.String()); payload.Session != nil {
		t.Errorf("未登录应注入 session=null，实际 %+v", payload.Session)
	}

	broken := newTestHandler(t, Options{
		Sessions: SessionResolverFunc(func(context.Context, *http.Request) (Session, bool, error) {
			return Session{}, false, errors.New("redis 不可达")
		}),
		Meta: func(context.Context) (Meta, error) { return Meta{}, errors.New("库不可达") },
	})
	failed := do(broken, http.MethodGet, "/zh-CN/dashboard", nil)
	if failed.Code != http.StatusOK {
		t.Fatalf("依赖故障时壳仍应 200，实际 %d", failed.Code)
	}
	if payload := extractBootstrap(t, failed.Body.String()); payload.Session != nil {
		t.Errorf("依赖故障应按未登录处理，实际 %+v", payload.Session)
	}
}

// ETag 必须按注入后的正文算：同一壳在不同会话下 ETag 不同，否则 304 会把上一名用户的引导数据当成新鲜回复。
func TestShellETagTracksInjectedBody(t *testing.T) {
	admin := newTestHandler(t, Options{Sessions: adminResolver(nil)})
	anonymous := newTestHandler(t, Options{
		Sessions: SessionResolverFunc(func(context.Context, *http.Request) (Session, bool, error) {
			return Session{}, false, nil
		}),
	})

	adminTag := do(admin, http.MethodGet, "/zh-CN/dashboard", nil).Header().Get("ETag")
	anonymousTag := do(anonymous, http.MethodGet, "/zh-CN/dashboard", nil).Header().Get("ETag")
	if adminTag == "" || anonymousTag == "" {
		t.Fatal("壳应带 ETag")
	}
	if adminTag == anonymousTag {
		t.Fatal("不同会话的壳 ETag 必须不同（ETag 要覆盖注入内容）")
	}
}

// 元数据按窗口缓存：窗口内多次请求只取一次，过窗后重取。
func TestMetaCacheWindow(t *testing.T) {
	now := time.Unix(1700000000, 0)
	calls := 0
	handler := newTestHandler(t, Options{
		MetaTTL: 60 * time.Second,
		Now:     func() time.Time { return now },
		Meta: func(context.Context) (Meta, error) {
			calls++
			return Meta{SiteTitle: "CC Hub"}, nil
		},
	})

	do(handler, http.MethodGet, "/zh-CN/dashboard", nil)
	do(handler, http.MethodGet, "/en/dashboard", nil)
	if calls != 1 {
		t.Fatalf("窗口内应只取一次元数据，实际 %d 次", calls)
	}

	now = now.Add(61 * time.Second)
	do(handler, http.MethodGet, "/zh-CN/dashboard", nil)
	if calls != 2 {
		t.Fatalf("过窗后应重取元数据，实际 %d 次", calls)
	}
}

// 产物缺失（只有占位文件）必须 fail fast，而不是启动后服务一个空站点。
func TestAssetsMissingFailsFast(t *testing.T) {
	onlyPlaceholder := fstest.MapFS{"assets/.gitkeep": &fstest.MapFile{}}
	if _, err := newHandler(onlyPlaceholder, assetsRoot, Options{}); !errors.Is(err, ErrAssetsMissing) {
		t.Fatalf("只有占位文件应报 ErrAssetsMissing，实际 %v", err)
	}

	noRootShell := fstest.MapFS{"assets/zh-CN/index.html": &fstest.MapFile{Data: []byte("<html/>")}}
	if _, err := newHandler(noRootShell, assetsRoot, Options{}); err == nil {
		t.Fatal("缺少根壳应报错")
	}
}

// 装配结论要能被启动日志与 /readyz 如实报告（含构建号与 locale 清单）。
func TestStatus(t *testing.T) {
	status := newTestHandler(t, Options{}).Status()
	if !status.Enabled {
		t.Fatal("Status.Enabled 应为 true")
	}
	if status.BuildID != "build-test01" {
		t.Errorf("应读到 BUILD_ID，实际 %q", status.BuildID)
	}
	// 7 份产物（3 个 locale 壳 + usage-doc 壳 + 根壳 + chunk + favicon），占位与 BUILD_ID 不计。
	if status.Files != 7 {
		t.Errorf("产物文件数应为 7，实际 %d", status.Files)
	}
	// usage-doc 是路由目录而非 locale，不得出现在清单里（这正是「不从产物推断 locale」的断言）。
	if strings.Join(status.Locales, ",") != "en,ja,zh-CN" {
		t.Errorf("locale 清单应为已注册且产物里有壳者（en,ja,zh-CN），实际 %v", status.Locales)
	}
	if !strings.Contains(status.Describe(), "build-test01") {
		t.Errorf("Describe 应含构建号，实际 %q", status.Describe())
	}
}

// do 发一次请求并返回记录器。
func do(handler http.Handler, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, target, nil)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// extractBootstrap 从壳正文里取出注入的引导数据。
func extractBootstrap(t *testing.T, body string) bootstrap {
	t.Helper()
	const prefix = "window.__CCH_BOOTSTRAP__="
	start := strings.Index(body, prefix)
	if start < 0 {
		t.Fatalf("壳里没有注入引导数据：%s", body)
	}
	raw := body[start+len(prefix):]
	end := strings.Index(raw, "</script>")
	if end < 0 {
		t.Fatalf("注入的脚本标签未闭合：%s", body)
	}
	var payload bootstrap
	if err := json.Unmarshal([]byte(raw[:end]), &payload); err != nil {
		t.Fatalf("注入内容不是合法 JSON（%v）：%s", err, raw[:end])
	}
	return payload
}
