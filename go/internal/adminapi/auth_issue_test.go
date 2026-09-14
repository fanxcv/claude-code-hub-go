package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/adminauth"
)

// 本文件钉住根级认证面（/api/auth/login|logout）的语义。唯一真源是
// src/app/api/auth/{login,logout}/route.ts 与 src/lib/auth.ts，故每条断言都指向 Node 的某一行行为：
// cookie 字节、重定向目标、loginType 三分类、失败分类的出现条件、CSRF 同源判定、限流锁定。
//
// 为什么这些断言值得写：登录是纯 Go 部署的**门**。它错了不是「少一个功能」，而是「进不了后台」——
// 而且错法往往很安静（cookie 属性差一个、Set-Cookie 少了 Path 就跨不进子路径）。

// authTestKey 是夹具用的密钥串（带特殊字符，用来钉住 cookie 的百分号编码）。
const authTestKey = "sk-auth-test-Ab 0/9?"

// authTestNow 是夹具时钟，与其它用例的 testClock 同源但独立命名（避免跨文件耦合）。
var authTestNow = time.Date(2026, 9, 12, 8, 0, 0, 0, time.UTC)

// ---- 夹具 ----

// stubAccounts 是一个可编程的账户读取器。
type stubAccounts struct {
	account LoginAccount
	found   bool
	err     error
	calls   int
}

func (s *stubAccounts) LoginAccountByKey(_ context.Context, _ string) (LoginAccount, bool, error) {
	s.calls++
	return s.account, s.found, s.err
}

// createdSession 是一次会话创建请求。
type createdSession struct {
	apiKey         string
	account        LoginAccount
	credentialType string
	ttl            time.Duration
}

// stubSessions 记录会话的创建与吊销。
type stubSessions struct {
	created   []createdSession
	revoked   []string
	createErr error
	revokeErr error
}

func (s *stubSessions) CreateAuthSession(
	_ context.Context,
	apiKey string,
	account LoginAccount,
	credentialType string,
	ttl time.Duration,
) (string, error) {
	if s.createErr != nil {
		return "", s.createErr
	}
	s.created = append(s.created, createdSession{apiKey, account, credentialType, ttl})
	return "sid_11111111-2222-4333-8444-555555555555", nil
}

func (s *stubSessions) RevokeAuthSession(_ context.Context, sessionID string) error {
	if s.revokeErr != nil {
		return s.revokeErr
	}
	s.revoked = append(s.revoked, sessionID)
	return nil
}

// authHarness 是一次登录面装配的结果。
type authHarness struct {
	router   *Router
	accounts *stubAccounts
	sessions *stubSessions
	audit    *recordingAudit
	issuer   *AuthIssuer
	abuse    *LoginAbusePolicy
}

// newAuthHarness 装配一个只依赖桩的登录面（不碰库与 Redis）。
func newAuthHarness(t *testing.T, options AuthIssuerOptions) *authHarness {
	t.Helper()
	accounts := &stubAccounts{}
	sessions := &stubSessions{}
	audit := &recordingAudit{}
	abuse := NewLoginAbusePolicy().WithNow(func() time.Time { return authTestNow })

	options.Deps = Deps{
		Logger:   nil,
		Guard:    newTestGuard(t, GuardOptions{}),
		Problems: NewProblems(nil),
		Audit:    audit,
	}
	options.Accounts = accounts
	options.Sessions = sessions
	options.Abuse = abuse
	if options.Now == nil {
		options.Now = func() time.Time { return authTestNow }
	}
	if options.AdminToken == "" {
		options.AdminToken = testAdminToken
	}
	if options.SessionTokenMode == "" {
		options.SessionTokenMode = "opaque"
	}

	issuer, err := NewAuthIssuer(options)
	if err != nil {
		t.Fatalf("构造登录面失败：%v", err)
	}
	router := New(Options{Deps: options.Deps})
	RegisterAuthRoutes(router, options.Deps, issuer)
	if router.RouteCount() != 2 {
		t.Fatalf("应注册 2 条根级认证路由，实际 %d", router.RouteCount())
	}
	return &authHarness{
		router:   router,
		accounts: accounts,
		sessions: sessions,
		audit:    audit,
		issuer:   issuer,
		abuse:    abuse,
	}
}

// post 发一个登录/登出请求；headers 为额外请求头（值写 "k: v" 形式）。
func (h *authHarness) post(t *testing.T, path string, body string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	for _, header := range headers {
		name, value, found := strings.Cut(header, ": ")
		if !found {
			t.Fatalf("请求头写法应为 `Name: value`，收到 %q", header)
		}
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	h.router.ServeHTTP(recorder, request)
	return recorder
}

// decodeBody 解出响应正文。
func decodeBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是合法 JSON（%s）：%v", recorder.Body.String(), err)
	}
	return payload
}

// ---- 登录成功：ADMIN_TOKEN + opaque 模式 ----

// TestAuthLoginAdminTokenOpaqueSetsSignedCookie 钉住管理令牌登录：签名 cookie、响应形状、无版本头。
func TestAuthLoginAdminTokenOpaqueSetsSignedCookie(t *testing.T) {
	harness := newAuthHarness(t, AuthIssuerOptions{})

	recorder := harness.post(t, "/api/auth/login", `{"key":"`+testAdminToken+`"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("管理令牌登录应 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}

	body := decodeBody(t, recorder)
	if body["ok"] != true || body["redirectTo"] != "/dashboard" || body["loginType"] != "admin" {
		t.Fatalf("响应形状与 Node 不符：%v", body)
	}
	user, ok := body["user"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺 user 对象：%v", body)
	}
	// 三项必须与 auth.ts:196-235 的合成身份逐字一致：id/keyId 均为 -1。
	if user["id"] != float64(-1) || user["name"] != adminPrincipalName ||
		user["description"] != "Environment admin session" || user["role"] != "admin" {
		t.Fatalf("合成管理员身份与 Node 不符：%v", user)
	}

	// opaque 模式下 cookie 是签名的 admin 会话令牌，而不是密钥原文——这是「浏览器不再持密钥」的核心。
	cookie := recorder.Header().Get("Set-Cookie")
	if !strings.HasPrefix(cookie, authCookieName+"="+adminauth.SignedTokenPrefix) {
		t.Fatalf("opaque 模式应下发签名令牌，实际 %q", cookie)
	}
	// 管理令牌登录不写不透明会话（Node 走 createSignedAdminAuthToken 分支）。
	if len(harness.sessions.created) != 0 {
		t.Fatalf("管理令牌不应创建不透明会话，实际 %d 次", len(harness.sessions.created))
	}
	// Node 的 /api/auth/* 不在管理面应用上，故**没有** X-API-Version 头。
	if version := recorder.Header().Get(VersionHeader); version != "" {
		t.Fatalf("根级认证面不得带管理面版本头，实际 %q", version)
	}
	if cache := recorder.Header().Get("Cache-Control"); cache != "no-store, no-cache, must-revalidate" {
		t.Fatalf("认证面响应必须不缓存，实际 %q", cache)
	}
}

// TestAuthLoginOpaqueSessionCookieShape 钉住「普通用户登录 → 会话 id 作 cookie」这条路径。
func TestAuthLoginOpaqueSessionCookieShape(t *testing.T) {
	harness := newAuthHarness(t, AuthIssuerOptions{})
	harness.accounts.account = LoginAccount{
		UserID:          7,
		UserName:        "普通用户",
		UserDescription: "读只读页面",
		UserRole:        "user",
		UserEnabled:     true,
		KeyID:           42,
		KeyName:         "夹具密钥",
		CanLoginWebUI:   true,
	}
	harness.accounts.found = true

	recorder := harness.post(t, "/api/auth/login", `{"key":"`+authTestKey+`"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("合法密钥登录应 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if len(harness.sessions.created) != 1 {
		t.Fatalf("opaque 模式应创建 1 个会话，实际 %d", len(harness.sessions.created))
	}
	created := harness.sessions.created[0]
	// fingerprint 的材料必须是**签发时那把密钥**（守卫找回会话时按它逐把比对，auth.ts:448）。
	if created.apiKey != authTestKey {
		t.Fatalf("会话 fingerprint 的材料应是密钥原文，实际 %q", created.apiKey)
	}
	if created.credentialType != "session" || created.account.UserID != 7 {
		t.Fatalf("会话内容与 Node 不符：%+v", created)
	}
	if created.ttl != defaultAuthSessionTTL {
		t.Fatalf("会话 TTL 应为默认 604800s，实际 %s", created.ttl)
	}

	cookie := recorder.Header().Get("Set-Cookie")
	if !strings.HasPrefix(cookie, authCookieName+"=sid_") {
		t.Fatalf("opaque 模式应把会话 id 写进 cookie，实际 %q", cookie)
	}
	// 属性顺序与 Next 的 stringifyCookie 一致：Path → Expires → Max-Age → Secure → HttpOnly → SameSite。
	for _, required := range []string{"Path=/", "Expires=", "Max-Age=604800", "HttpOnly", "SameSite=lax"} {
		if !strings.Contains(cookie, required) {
			t.Fatalf("cookie 缺属性 %q：%q", required, cookie)
		}
	}
	if strings.Contains(cookie, "Secure") {
		t.Fatalf("ENABLE_SECURE_COOKIES 为假时不得带 Secure：%q", cookie)
	}
	body := decodeBody(t, recorder)
	if body["loginType"] != "dashboard_user" || body["redirectTo"] != "/dashboard" {
		t.Fatalf("可登录 Web UI 的用户应落 dashboard，实际 %v", body)
	}
}

// TestAuthLoginLegacyModeUsesRawKeyAndEncodesCookie 钉住 legacy 模式与 cookie 的值编码。
func TestAuthLoginLegacyModeUsesRawKeyAndEncodesCookie(t *testing.T) {
	harness := newAuthHarness(t, AuthIssuerOptions{SessionTokenMode: "legacy"})
	harness.accounts.account = LoginAccount{
		UserID: 3, UserName: "只读", UserRole: "user", UserEnabled: true,
		KeyID: 9, CanLoginWebUI: false,
	}
	harness.accounts.found = true

	recorder := harness.post(t, "/api/auth/login", `{"key":"`+authTestKey+`"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("legacy 模式登录应 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if len(harness.sessions.created) != 0 {
		t.Fatal("legacy 模式不得创建不透明会话")
	}
	// 值经 encodeURIComponent：空格 → %20、斜杠 → %2F、问号 → %3F。QueryEscape 会把空格写成 "+"，
	// 那与 Node 的 cookie 字节不同，故这条断言同时挡回归。
	cookie := recorder.Header().Get("Set-Cookie")
	if !strings.HasPrefix(cookie, authCookieName+"=sk-auth-test-Ab%200%2F9%3F") {
		t.Fatalf("legacy 模式应下发百分号编码的密钥原文，实际 %q", cookie)
	}
	body := decodeBody(t, recorder)
	if body["loginType"] != "readonly_user" || body["redirectTo"] != "/my-usage" {
		t.Fatalf("只读用户应落 /my-usage，实际 %v", body)
	}
}

// TestAuthLoginSessionCreateFailureIs503 钉住「签不出会话」这一失败面：503 + SESSION_CREATE_FAILED。
func TestAuthLoginSessionCreateFailureIs503(t *testing.T) {
	harness := newAuthHarness(t, AuthIssuerOptions{})
	harness.accounts.account = LoginAccount{UserID: 5, UserEnabled: true, KeyID: 6, CanLoginWebUI: true}
	harness.accounts.found = true
	harness.sessions.createErr = context.DeadlineExceeded

	recorder := harness.post(t, "/api/auth/login", `{"key":"`+authTestKey+`"}`)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("会话写失败应 503，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if body := decodeBody(t, recorder); body["errorCode"] != "SESSION_CREATE_FAILED" {
		t.Fatalf("失败形状与 Node 不符：%v", body)
	}
	if recorder.Header().Get("Set-Cookie") != "" {
		t.Fatal("会话写失败时不得下发 cookie（否则浏览器持一个查不到的会话 id）")
	}
}

// ---- 登录失败面 ----

// TestAuthLoginFailureTaxonomy 钉住「失败分类只在带 x-forwarded-proto 时出现」这条 Node 语义。
func TestAuthLoginFailureTaxonomy(t *testing.T) {
	cases := []struct {
		name          string
		body          string
		headers       []string
		wantStatus    int
		wantError     string
		wantErrorCode string
	}{
		{
			name: "缺 key 且经前门", body: `{}`, headers: []string{"x-forwarded-proto: https"},
			wantStatus: 400, wantError: "请输入 API Key", wantErrorCode: "KEY_REQUIRED",
		},
		{
			name: "缺 key 且直连", body: `{}`,
			wantStatus: 400, wantError: "请输入 API Key", wantErrorCode: "",
		},
		{
			name: "密钥无效且经前门", body: `{"key":"nope"}`, headers: []string{"x-forwarded-proto: https"},
			wantStatus: 401, wantError: "API Key 无效或已过期", wantErrorCode: "KEY_INVALID",
		},
		{
			name: "密钥无效且直连", body: `{"key":"nope"}`,
			wantStatus: 401, wantError: "API Key 无效或已过期", wantErrorCode: "",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newAuthHarness(t, AuthIssuerOptions{})
			recorder := harness.post(t, "/api/auth/login", testCase.body, testCase.headers...)
			if recorder.Code != testCase.wantStatus {
				t.Fatalf("状态码应为 %d，实际 %d：%s", testCase.wantStatus, recorder.Code, recorder.Body.String())
			}
			body := decodeBody(t, recorder)
			if body["error"] != testCase.wantError {
				t.Fatalf("文案应取 i18n 默认语言表，实际 %v", body["error"])
			}
			code, present := body["errorCode"]
			if testCase.wantErrorCode == "" {
				if present {
					t.Fatalf("直连请求不得带 errorCode，实际 %v", code)
				}
				return
			}
			if code != testCase.wantErrorCode {
				t.Fatalf("失败分类应为 %s，实际 %v", testCase.wantErrorCode, code)
			}
		})
	}
}

// TestAuthLoginUnparsableBodyIs500 钉住正文解析失败落 500（Node 的外层 catch）。
func TestAuthLoginUnparsableBodyIs500(t *testing.T) {
	harness := newAuthHarness(t, AuthIssuerOptions{})
	recorder := harness.post(t, "/api/auth/login", `{"key":`)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("正文非法应 500，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
}

// TestAuthLoginCrossSiteOriginRejected 钉住 CSRF 同源门的三条分支。
func TestAuthLoginCrossSiteOriginRejected(t *testing.T) {
	cases := []struct {
		name       string
		headers    []string
		wantStatus int
	}{
		{name: "跨站且带 Origin", headers: []string{"Origin: https://evil.example", "Host: hub.example"}, wantStatus: 403},
		{name: "同源 Origin", headers: []string{"Origin: https://hub.example", "Host: hub.example"}, wantStatus: 200},
		{name: "sec-fetch-site 同源", headers: []string{"Sec-Fetch-Site: same-origin", "Origin: https://evil.example"}, wantStatus: 200},
		{name: "跨站但无 Origin", headers: []string{"Sec-Fetch-Site: cross-site"}, wantStatus: 403},
		{name: "命令行无 Origin 无 sec-fetch", headers: nil, wantStatus: 200},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newAuthHarness(t, AuthIssuerOptions{})
			recorder := harness.post(t, "/api/auth/login", `{"key":"`+testAdminToken+`"}`, testCase.headers...)
			if recorder.Code != testCase.wantStatus {
				t.Fatalf("状态码应为 %d，实际 %d：%s", testCase.wantStatus, recorder.Code, recorder.Body.String())
			}
			if testCase.wantStatus == 403 {
				if body := decodeBody(t, recorder); body["errorCode"] != "CSRF_REJECTED" {
					t.Fatalf("CSRF 拒绝形状与 Node 不符：%v", body)
				}
			}
		})
	}
}

// TestAuthLoginRateLimitLocks 钉住限流：第 11 次失败起拒绝，且带 Retry-After。
//
// 阈值与锁定时长取自 DEFAULT_LOGIN_ABUSE_CONFIG（10 次 / 300s 窗口 / 900s 锁定）。
func TestAuthLoginRateLimitLocks(t *testing.T) {
	harness := newAuthHarness(t, AuthIssuerOptions{})
	for attempt := 0; attempt < 10; attempt++ {
		recorder := harness.post(t, "/api/auth/login", `{"key":"wrong"}`)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败应 401，实际 %d", attempt+1, recorder.Code)
		}
	}

	recorder := harness.post(t, "/api/auth/login", `{"key":"wrong"}`)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("第 11 次应被限流（429），实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if retry := recorder.Header().Get("Retry-After"); retry != "900" {
		t.Fatalf("Retry-After 应为锁定时长 900 秒，实际 %q", retry)
	}
	if body := decodeBody(t, recorder); body["errorCode"] != "RATE_LIMITED" {
		t.Fatalf("限流失败分类应为 RATE_LIMITED，实际 %v", body)
	}

	// 限流生效后即便给出正确的管理令牌也要被拒：否则「限流」在爆破场景下形同虚设。
	correct := harness.post(t, "/api/auth/login", `{"key":"`+testAdminToken+`"}`)
	if correct.Code != http.StatusTooManyRequests {
		t.Fatalf("锁定期间正确凭据同样应被拒，实际 %d", correct.Code)
	}
}

// TestAuthLoginSuccessResetsRateLimit 钉住成功登录清空计数（Node 的 recordSuccess → reset）。
func TestAuthLoginSuccessResetsRateLimit(t *testing.T) {
	harness := newAuthHarness(t, AuthIssuerOptions{})
	for attempt := 0; attempt < 5; attempt++ {
		harness.post(t, "/api/auth/login", `{"key":"wrong"}`)
	}
	if recorder := harness.post(t, "/api/auth/login", `{"key":"`+testAdminToken+`"}`); recorder.Code != http.StatusOK {
		t.Fatalf("未超阈值时正确凭据应 200，实际 %d", recorder.Code)
	}
	for attempt := 0; attempt < 9; attempt++ {
		if recorder := harness.post(t, "/api/auth/login", `{"key":"wrong"}`); recorder.Code != http.StatusUnauthorized {
			t.Fatalf("成功登录后计数应清零：第 %d 次失败即 429（%d）说明计数没清", attempt+1, recorder.Code)
		}
	}
}

// TestAuthLoginLocaleFromCookieAndHeader 钉住 locale 解析顺序（cookie 优先，其次 Accept-Language）。
func TestAuthLoginLocaleFromCookieAndHeader(t *testing.T) {
	cases := []struct {
		name    string
		headers []string
		want    string
	}{
		{name: "cookie 优先", headers: []string{"Cookie: NEXT_LOCALE=ja", "Accept-Language: en-US"}, want: "ログインに失敗しました"},
		{name: "Accept-Language 次之", headers: []string{"Accept-Language: ru-RU,ru;q=0.9"}, want: "Ошибка входа"},
		{name: "都缺则默认 zh-CN", headers: nil, want: "登录失败"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newAuthHarness(t, AuthIssuerOptions{})
			// 走限流拒绝路径取文案：它的正文只含文案，不含分类差异。限流只在「该分桶已有记录」时
			// 锁定（Node 的 check 同样只在已有记录上判），故先记满 10 次失败。
			for attempt := 0; attempt < 10; attempt++ {
				harness.abuse.RecordFailure("1.2.3.4", "")
			}
			headers := append([]string{"X-Real-IP: 1.2.3.4"}, testCase.headers...)
			recorder := harness.post(t, "/api/auth/login", `{"key":"x"}`, headers...)
			if recorder.Code != http.StatusTooManyRequests {
				t.Fatalf("记满阈值后应被限流，实际 %d：%s", recorder.Code, recorder.Body.String())
			}
			if body := decodeBody(t, recorder); body["error"] != testCase.want {
				t.Fatalf("文案应为 %q，实际 %v", testCase.want, body["error"])
			}
		})
	}
}

// ---- 登出 ----

// TestAuthLogoutRevokesOpaqueSession 钉住登出：吊销会话 + 清 cookie（Next 的 delete 形状）。
func TestAuthLogoutRevokesOpaqueSession(t *testing.T) {
	harness := newAuthHarness(t, AuthIssuerOptions{})
	recorder := harness.post(t, "/api/auth/logout", `{}`,
		"Cookie: "+authCookieName+"=sid_11111111-2222-4333-8444-555555555555")
	if recorder.Code != http.StatusOK {
		t.Fatalf("登出应 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if len(harness.sessions.revoked) != 1 ||
		harness.sessions.revoked[0] != "sid_11111111-2222-4333-8444-555555555555" {
		t.Fatalf("应吊销会话，实际 %v", harness.sessions.revoked)
	}
	// Next 的 clearAuthCookie → ResponseCookies.delete 只写 name/value/expires：没有 Path、HttpOnly。
	want := authCookieName + "=; Expires=Thu, 01 Jan 1970 00:00:00 GMT"
	if cookie := recorder.Header().Get("Set-Cookie"); cookie != want {
		t.Fatalf("清 cookie 的字节应为 %q，实际 %q", want, cookie)
	}
	if body := decodeBody(t, recorder); body["ok"] != true {
		t.Fatalf("登出响应形状与 Node 不符：%v", body)
	}
	if version := recorder.Header().Get(VersionHeader); version != "" {
		t.Fatalf("根级认证面不得带管理面版本头，实际 %q", version)
	}
}

// TestAuthLogoutLegacyModeSkipsRevoke 钉住 legacy 模式不吊销（它根本没有不透明会话）。
func TestAuthLogoutLegacyModeSkipsRevoke(t *testing.T) {
	harness := newAuthHarness(t, AuthIssuerOptions{SessionTokenMode: "legacy"})
	recorder := harness.post(t, "/api/auth/logout", `{}`, "Cookie: "+authCookieName+"=sid_whatever")
	if recorder.Code != http.StatusOK {
		t.Fatalf("登出应 200，实际 %d", recorder.Code)
	}
	if len(harness.sessions.revoked) != 0 {
		t.Fatalf("legacy 模式不得吊销会话，实际 %v", harness.sessions.revoked)
	}
}

// TestAuthLogoutRejectsCrossSiteOrigin 钉住登出同样过 CSRF 同源门。
func TestAuthLogoutRejectsCrossSiteOrigin(t *testing.T) {
	harness := newAuthHarness(t, AuthIssuerOptions{})
	recorder := harness.post(t, "/api/auth/logout", `{}`,
		"Origin: https://evil.example", "Host: hub.example")
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("跨站登出应 403，实际 %d", recorder.Code)
	}
	if len(harness.sessions.revoked) != 0 {
		t.Fatal("CSRF 被拒时不得吊销任何会话")
	}
}

// ---- 审计 ----

// TestAuthLoginAuditTrail 钉住三条审计路径的分类与动作名（Node 的 createAuditLogAsync 参数）。
func TestAuthLoginAuditTrail(t *testing.T) {
	harness := newAuthHarness(t, AuthIssuerOptions{})

	// 1) 成功。
	harness.post(t, "/api/auth/login", `{"key":"`+testAdminToken+`"}`)
	// 2) 无效密钥。
	harness.post(t, "/api/auth/login", `{"key":"wrong"}`)
	// 3) 限流（阈值改成 0，下一次必拒）。
	harness.abuse.WithConfig(LoginAbuseConfig{Window: 300 * time.Second, Lockout: 900 * time.Second})
	harness.post(t, "/api/auth/login", `{"key":"wrong"}`)

	events := append([]AuditEvent(nil), harness.audit.events...)

	byAction := map[string]AuditEvent{}
	for _, event := range events {
		byAction[event.Action] = event
	}
	success, ok := byAction["login.success"]
	if !ok {
		t.Fatalf("缺 login.success 审计：%v", events)
	}
	if success.Category != "auth" || !success.Success || success.TargetType != "user" {
		t.Fatalf("成功审计字段与 Node 不符：%+v", success)
	}
	if success.Details["loginType"] != "admin" {
		t.Fatalf("成功审计应记 loginType，实际 %v", success.Details)
	}
	failure, ok := byAction["login.failure"]
	if !ok || failure.Success || failure.ErrorMessage != "KEY_INVALID" {
		t.Fatalf("失败审计与 Node 不符：%+v", failure)
	}
	rateLimited, ok := byAction["login.rate_limited"]
	if !ok || rateLimited.Success || rateLimited.ErrorMessage != "RATE_LIMITED" {
		t.Fatalf("限流审计与 Node 不符：%+v", rateLimited)
	}
}

// ---- 单元面：cookie 序列化与限流窗口 ----

// TestSetAuthCookieHeaderMatchesNextBytes 逐字钉住 Set-Cookie 的字节形状。
func TestSetAuthCookieHeaderMatchesNextBytes(t *testing.T) {
	issuer := &AuthIssuer{
		sessionTTL: 604800 * time.Second,
		now:        func() time.Time { return authTestNow },
	}
	insecure := issuer.setAuthCookieHeader("sid_a b")
	wantInsecure := "auth-token=sid_a%20b; Path=/; Expires=Sat, 19 Sep 2026 08:00:00 GMT; " +
		"Max-Age=604800; HttpOnly; SameSite=lax"
	if insecure != wantInsecure {
		t.Fatalf("无 Secure 的 cookie 字节不符：\nwant %q\ngot  %q", wantInsecure, insecure)
	}

	issuer.secure = true
	secure := issuer.setAuthCookieHeader("sid_a b")
	wantSecure := "auth-token=sid_a%20b; Path=/; Expires=Sat, 19 Sep 2026 08:00:00 GMT; " +
		"Max-Age=604800; Secure; HttpOnly; SameSite=lax"
	if secure != wantSecure {
		t.Fatalf("带 Secure 的 cookie 字节不符：\nwant %q\ngot  %q", wantSecure, secure)
	}

	if cleared := clearAuthCookieHeader(); cleared != "auth-token=; Expires=Thu, 01 Jan 1970 00:00:00 GMT" {
		t.Fatalf("清 cookie 字节不符：%q", cleared)
	}
}

// TestLoginAbusePolicyWindowAndRetryAfter 钉住窗口过期与 Retry-After 的向上取整。
func TestLoginAbusePolicyWindowAndRetryAfter(t *testing.T) {
	current := authTestNow
	policy := NewLoginAbusePolicy().WithNow(func() time.Time { return current })

	for attempt := 0; attempt < 10; attempt++ {
		policy.RecordFailure("1.2.3.4", "")
	}
	decision := policy.Check("1.2.3.4", "")
	if decision.Allowed {
		t.Fatal("记满阈值后应被拒")
	}
	if decision.RetryAfterSeconds == nil || *decision.RetryAfterSeconds != 900 {
		t.Fatalf("锁定时长应为 900 秒，实际 %v", decision.RetryAfterSeconds)
	}

	// 窗口（300s）过期后计数清零；但此刻已进入锁定态，故先跨过锁定时长（900s）。
	current = authTestNow.Add(901 * time.Second)
	if decision := policy.Check("1.2.3.4", ""); !decision.Allowed {
		t.Fatalf("锁定期结束后应放行，实际 %+v", decision)
	}

	// 另一个 IP 不受影响（分桶按 IP）。
	if decision := policy.Check("5.6.7.8", ""); !decision.Allowed {
		t.Fatalf("不同 IP 不得互相影响，实际 %+v", decision)
	}
}
