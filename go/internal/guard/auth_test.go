package guard

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// 认证是访问控制路径：三条凭据来源、冲突判定、失败分类与文案都要逐条钉死。

func TestAuthStepAcceptsEachCredentialSource(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		wantKey string
	}{
		{name: "authorization Bearer", headers: map[string]string{"authorization": "Bearer sk-bearer"}, wantKey: "sk-bearer"},
		{name: "x-api-key", headers: map[string]string{"x-api-key": " sk-header "}, wantKey: "sk-header"},
		{name: "x-goog-api-key", headers: map[string]string{"x-goog-api-key": "sk-gemini"}, wantKey: "sk-gemini"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			resolver := &fakeAuth{resolution: AuthResolution{
				User: User{ID: 7, IsEnabled: true},
				Key:  Key{ID: 3, Name: "默认密钥"},
			}}
			ctx := newContext(t, testCase.headers, nil)
			step := Deps{Auth: resolver}.authStep()

			response, err := step(ctx)
			if err != nil {
				t.Fatalf("认证不应报错: %v", err)
			}
			if response != nil {
				t.Fatalf("凭据有效时不应有响应，收到 %d", response.Status)
			}
			if resolver.lastKey != testCase.wantKey {
				t.Fatalf("解析器收到的密钥应为 %q，收到 %q", testCase.wantKey, resolver.lastKey)
			}
			auth, ok := ctx.Auth()
			if !ok || auth.UserID != 7 || auth.KeyID != 3 || auth.APIKey != testCase.wantKey {
				t.Fatalf("鉴权结果未写回上下文: %+v", auth)
			}
		})
	}
}

func TestAuthStepMissingCredentials(t *testing.T) {
	ctx := newContext(t, nil, nil)
	response, err := Deps{Auth: &fakeAuth{}}.authStep()(ctx)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if response == nil || response.Status != 401 {
		t.Fatalf("缺凭据应返回 401，收到 %v", response)
	}
	if got := errorField(t, response, "type"); got != "authentication_error" {
		t.Fatalf("type 应为 authentication_error，收到 %s", got)
	}
	if !strings.Contains(errorField(t, response, "message"), "未提供认证凭据") {
		t.Fatalf("文案应说明缺凭据：%s", string(response.Body))
	}
}

// 多来源冲突必须拒绝：这是「只允许一种认证方式」的落点。
func TestAuthStepRejectsConflictingKeys(t *testing.T) {
	ctx := newContext(t, map[string]string{
		"authorization": "Bearer sk-one",
		"x-api-key":     "sk-two",
	}, nil)
	response, err := Deps{Auth: &fakeAuth{}}.authStep()(ctx)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if response == nil || response.Status != 401 {
		t.Fatalf("冲突凭据应返回 401，收到 %v", response)
	}
	if !strings.Contains(errorField(t, response, "message"), "冲突") {
		t.Fatalf("文案应说明冲突：%s", string(response.Body))
	}
}

// 失败分类决定是否计入防爆破节流，这是安全语义而不是样式问题。
func TestAuthStepFailureClassification(t *testing.T) {
	cases := []struct {
		name              string
		resolveErr        error
		wantStatus        int
		wantType          string
		wantCountedAsFail bool
		wantMessagePart   string
	}{
		{
			name:              "密钥不存在计失败",
			resolveErr:        ErrKeyNotFound,
			wantStatus:        401,
			wantType:          "invalid_api_key",
			wantCountedAsFail: true,
			wantMessagePart:   "API 密钥无效",
		},
		{
			name:              "密钥禁用不计失败",
			resolveErr:        ErrKeyDisabled,
			wantStatus:        401,
			wantType:          "key_disabled",
			wantCountedAsFail: false,
			wantMessagePart:   "已被禁用",
		},
		{
			name:              "密钥过期不计失败",
			resolveErr:        ErrKeyExpired,
			wantStatus:        401,
			wantType:          "key_expired",
			wantCountedAsFail: false,
			wantMessagePart:   "已过期",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			limiter := &fakeLimiter{throttle: ThrottleDecision{Allowed: true}}
			deps := Deps{Auth: &fakeAuth{err: testCase.resolveErr}, AuthThrottle: limiter}
			ctx := newContext(t, map[string]string{"authorization": "Bearer sk-x"}, nil)

			response, err := deps.authStep()(ctx)
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if response == nil || response.Status != testCase.wantStatus {
				t.Fatalf("状态码应为 %d，收到 %v", testCase.wantStatus, response)
			}
			if got := errorField(t, response, "type"); got != testCase.wantType {
				t.Fatalf("type 应为 %s，收到 %s", testCase.wantType, got)
			}
			if !strings.Contains(errorField(t, response, "message"), testCase.wantMessagePart) {
				t.Fatalf("文案应含 %q：%s", testCase.wantMessagePart, string(response.Body))
			}
			counted := limiter.failures > 0
			if counted != testCase.wantCountedAsFail {
				t.Fatalf("是否计入失败应为 %v，收到 %v", testCase.wantCountedAsFail, counted)
			}
		})
	}
}

// 账户状态类失败（禁用/过期）与凭据类失败的分类必须能区分：前者不该喂给节流器。
func TestAuthStepUserStateFailures(t *testing.T) {
	expired := time.Now().Add(-time.Hour)

	cases := []struct {
		name            string
		user            User
		wantType        string
		wantMessagePart string
	}{
		{
			name:            "用户禁用",
			user:            User{ID: 7, IsEnabled: false},
			wantType:        "user_disabled",
			wantMessagePart: "已被禁用",
		},
		{
			name:            "用户过期",
			user:            User{ID: 7, IsEnabled: true, ExpiresAt: &expired},
			wantType:        "user_expired",
			wantMessagePart: expired.UTC().Format("2006-01-02"),
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			limiter := &fakeLimiter{throttle: ThrottleDecision{Allowed: true}}
			deps := Deps{
				Auth:         &fakeAuth{resolution: AuthResolution{User: testCase.user, Key: Key{ID: 3}}},
				AuthThrottle: limiter,
			}
			ctx := newContext(t, map[string]string{"x-api-key": "sk-x"}, nil)

			response, err := deps.authStep()(ctx)
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if response == nil || response.Status != 401 {
				t.Fatalf("应返回 401，收到 %v", response)
			}
			if got := errorField(t, response, "type"); got != testCase.wantType {
				t.Fatalf("type 应为 %s，收到 %s", testCase.wantType, got)
			}
			if !strings.Contains(errorField(t, response, "message"), testCase.wantMessagePart) {
				t.Fatalf("文案应含 %q：%s", testCase.wantMessagePart, string(response.Body))
			}
			if limiter.failures != 0 {
				t.Fatal("账户状态类失败不应计入防爆破节流")
			}
		})
	}
}

// 惰性过期标记是尽力而为：标记缝隙报错不影响拒绝。
func TestAuthStepUserExpiredMarksAndIgnoresMarkerError(t *testing.T) {
	expired := time.Now().Add(-time.Hour)
	marker := &fakeExpiryMarker{err: errors.New("数据库不可达")}
	deps := Deps{
		Auth: &fakeAuth{resolution: AuthResolution{
			User: User{ID: 7, IsEnabled: true, ExpiresAt: &expired},
			Key:  Key{ID: 3},
		}},
		ExpiryMarker: marker,
	}
	ctx := newContext(t, map[string]string{"x-api-key": "sk-x"}, nil)

	response, err := deps.authStep()(ctx)
	if err != nil {
		t.Fatalf("标记失败不应让请求报错: %v", err)
	}
	if response == nil || response.Status != 401 {
		t.Fatalf("应返回 401，收到 %v", response)
	}
	if marker.markedUserID != 7 {
		t.Fatalf("应尝试标记用户 7 过期，收到 %d", marker.markedUserID)
	}
}

// 节流命中时返回 429 并带 Retry-After。
func TestAuthStepThrottled(t *testing.T) {
	retryAfter := 42
	limiter := &fakeLimiter{throttle: ThrottleDecision{Allowed: false, RetryAfterSeconds: &retryAfter}}
	resolver := &fakeAuth{}
	ctx := newContext(t, map[string]string{"x-api-key": "sk-x"}, nil)

	response, err := Deps{Auth: resolver, AuthThrottle: limiter}.authStep()(ctx)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if response == nil || response.Status != 429 {
		t.Fatalf("节流命中应返回 429，收到 %v", response)
	}
	if got := response.Headers.Get("Retry-After"); got != "42" {
		t.Fatalf("Retry-After 应为 42，收到 %q", got)
	}
	if resolver.calls != 0 {
		t.Fatal("节流命中时不应再打解析器")
	}
}

// 认证成功要重置节流计数。
func TestAuthStepSuccessResetsThrottle(t *testing.T) {
	limiter := &fakeLimiter{throttle: ThrottleDecision{Allowed: true}}
	deps := Deps{
		Auth: &fakeAuth{resolution: AuthResolution{
			User: User{ID: 7, IsEnabled: true},
			Key:  Key{ID: 3},
		}},
		AuthThrottle: limiter,
	}
	ctx := newContext(t, map[string]string{"x-api-key": "sk-x"}, nil)

	if _, err := deps.authStep()(ctx); err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if limiter.successes != 1 {
		t.Fatalf("应记录一次认证成功，收到 %d", limiter.successes)
	}
}

// 解析器自身故障不能 fail-open：那等于放行未鉴权请求。
func TestAuthStepResolverFailureIsNotFailOpen(t *testing.T) {
	deps := Deps{Auth: &fakeAuth{err: errors.New("连接池耗尽")}}
	ctx := newContext(t, map[string]string{"x-api-key": "sk-x"}, nil)

	response, err := deps.authStep()(ctx)
	if err == nil && response == nil {
		t.Fatal("解析器故障时既不能放行也不能静默")
	}
	if response != nil {
		t.Fatalf("解析器故障不应伪装成认证失败响应，收到 %d", response.Status)
	}
}

// 认证缝隙缺失同样不能放行未鉴权请求。
func TestAuthStepMissingResolverIsNotFailOpen(t *testing.T) {
	ctx := newContext(t, map[string]string{"x-api-key": "sk-x"}, nil)
	response, err := Deps{}.authStep()(ctx)
	if err == nil && response == nil {
		t.Fatal("认证缝隙缺失时不能静默放行")
	}
}

func TestExtractBearerKey(t *testing.T) {
	cases := []struct {
		header string
		want   string
	}{
		{header: "Bearer sk-1", want: "sk-1"},
		{header: "bearer sk-2", want: "sk-2"},
		{header: "  BEARER   sk-3  ", want: "sk-3"},
		{header: "Basic sk-4", want: ""},
		{header: "Bearer", want: ""},
		{header: "", want: ""},
	}
	for _, testCase := range cases {
		if got := extractBearerKey(testCase.header); got != testCase.want {
			t.Errorf("%q 应解析为 %q，收到 %q", testCase.header, testCase.want, got)
		}
	}
}

// 非守卫流程复用的提取函数只取首个可用凭据。
func TestExtractAPIKeyFromHeaders(t *testing.T) {
	headers := map[string]string{"x-chain": "x"}
	if got := extractAPIKeyFromHeaders(headers); got != "" {
		t.Fatalf("无凭据时应为空串，收到 %q", got)
	}
	headers = map[string]string{"authorization": "Bearer sk-a", "x-api-key": "sk-b"}
	if got := extractAPIKeyFromHeaders(headers); got != "sk-a" {
		t.Fatalf("应取 Bearer 凭据，收到 %q", got)
	}
}

// 客户端 IP 优先取缝隙结果，其次退回上下文。
func TestAuthStepClientIPResolution(t *testing.T) {
	ctx := newContext(t, map[string]string{"x-api-key": "sk-x"}, nil)
	ctx.SetClientIP("10.0.0.1")
	deps := Deps{
		Auth: &fakeAuth{resolution: AuthResolution{User: User{ID: 7, IsEnabled: true}, Key: Key{ID: 3}}},
		IP:   fakeIP{ip: "203.0.113.9"},
	}
	if _, err := deps.authStep()(ctx); err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if got := ctx.ClientIP(); got != "203.0.113.9" {
		t.Fatalf("缝隙结果应覆盖上下文，收到 %q", got)
	}

	ctx = newContext(t, map[string]string{"x-api-key": "sk-x"}, nil)
	ctx.SetClientIP("10.0.0.2")
	deps.IP = nil
	if _, err := deps.authStep()(ctx); err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if got := ctx.ClientIP(); got != "10.0.0.2" {
		t.Fatalf("无缝隙时应退回上下文值，收到 %q", got)
	}
}

// IP 解析失败要退回上下文，而不是把请求判成无 IP。
func TestAuthStepClientIPFallbackOnExtractorError(t *testing.T) {
	ctx := newContext(t, map[string]string{"x-api-key": "sk-x"}, nil)
	ctx.SetClientIP("10.0.0.3")
	deps := Deps{
		Auth: &fakeAuth{resolution: AuthResolution{User: User{ID: 7, IsEnabled: true}, Key: Key{ID: 3}}},
		IP:   fakeIP{err: errors.New("header 格式非法")},
	}
	if _, err := deps.authStep()(ctx); err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if got := ctx.ClientIP(); got != "10.0.0.3" {
		t.Fatalf("解析失败应退回上下文值，收到 %q", got)
	}
}

// Gemini 的 key 查询参数来自入口注入。
func TestAuthStepGeminiQueryKey(t *testing.T) {
	resolver := &fakeAuth{resolution: AuthResolution{User: User{ID: 7, IsEnabled: true}, Key: Key{ID: 3}}}
	ctx := newContext(t, nil, nil)
	deps := Deps{
		Auth:        resolver,
		QueryAPIKey: func(*pctx.Context) string { return "sk-query" },
	}
	response, err := deps.authStep()(ctx)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if response != nil {
		t.Fatalf("查询参数凭据有效时不应有响应，收到 %d", response.Status)
	}
	if resolver.lastKey != "sk-query" {
		t.Fatalf("应使用查询参数凭据，收到 %q", resolver.lastKey)
	}
}

// 多来源冲突时不做节流键猜测。
func TestKeyCandidatesUnanimous(t *testing.T) {
	same := keyCandidates{Authorization: "Bearer sk-a", APIKey: "sk-a"}
	if got := same.unanimous(); got != "sk-a" {
		t.Fatalf("一致时应返回该凭据，收到 %q", got)
	}
	different := keyCandidates{Authorization: "Bearer sk-a", APIKey: "sk-b"}
	if got := different.unanimous(); got != "" {
		t.Fatalf("冲突时应返回空串，收到 %q", got)
	}
	if got := (keyCandidates{}).unanimous(); got != "" {
		t.Fatalf("无凭据时应返回空串，收到 %q", got)
	}
}

// 语种解析：cookie 优先、Accept-Language 按 q 值协商、默认 zh-CN。
func TestResolveLocale(t *testing.T) {
	cases := []struct {
		name           string
		cookie         string
		acceptLanguage string
		want           string
	}{
		{name: "cookie 优先", cookie: "en", acceptLanguage: "ja", want: "en"},
		{name: "cookie 非法时协商", cookie: "de-DE", acceptLanguage: "ja", want: "ja"},
		{name: "q 值降序", acceptLanguage: "ru;q=0.3, en;q=0.9", want: "en"},
		{name: "区域精确匹配", acceptLanguage: "zh-TW", want: "zh-TW"},
		{name: "赘余子标签截断到主标签", acceptLanguage: "zh-Hant-TW", want: "zh-CN"},
		{name: "zh 退到 zh-CN", acceptLanguage: "zh", want: "zh-CN"},
		{name: "全不支持时默认", acceptLanguage: "de, fr;q=0.8", want: DefaultLocale},
		{name: "无任何信息", want: DefaultLocale},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := ResolveLocale(testCase.cookie, testCase.acceptLanguage); got != testCase.want {
				t.Fatalf("应解析为 %s，收到 %s", testCase.want, got)
			}
		})
	}
}

// 三条文案在五种语种下都必须存在，且默认语种与 Node 逐字一致。
func TestProxyErrorMessages(t *testing.T) {
	for _, code := range []string{MessageInvalidAPIKey, MessageAPIKeyDisabled, MessageAPIKeyExpired} {
		for _, locale := range SupportedLocales {
			if text := Message(locale, code); text == "" {
				t.Fatalf("%s 的 %s 文案缺失", locale, code)
			}
		}
		if text := Message("de-DE", code); text != Message(DefaultLocale, code) {
			t.Fatalf("不支持的语种应退回默认语种，%s 收到 %q", code, text)
		}
	}
	if got := Message("zh-CN", MessageInvalidAPIKey); got != "API 密钥无效。提供的密钥不存在或已被删除。" {
		t.Fatalf("zh-CN 文案与 Node 不一致: %q", got)
	}
	if got := Message("en", MessageAPIKeyExpired); got != "This API key has expired. Please contact your administrator to renew it or rotate to a new key." {
		t.Fatalf("en 文案与 Node 不一致: %q", got)
	}
}

// 认证文案按请求语种变化：Accept-Language 决定返回哪种语言。
func TestAuthStepLocaleFromHeaders(t *testing.T) {
	deps := Deps{Auth: &fakeAuth{err: ErrKeyNotFound}}
	ctx := newContext(t, map[string]string{
		"x-api-key":       "sk-x",
		"accept-language": "en",
	}, nil)

	response, err := deps.authStep()(ctx)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if got := errorField(t, response, "message"); got != proxyErrorMessages[MessageInvalidAPIKey]["en"] {
		t.Fatalf("应按 en 返回文案，收到 %q", got)
	}

	deps.Locale = "ja"
	ctx = newContext(t, map[string]string{"x-api-key": "sk-x", "accept-language": "en"}, nil)
	response, err = deps.authStep()(ctx)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if got := errorField(t, response, "message"); got != proxyErrorMessages[MessageInvalidAPIKey]["ja"] {
		t.Fatalf("显式语种应覆盖头部，收到 %q", got)
	}
}

// cookieValue 解析 Cookie 头。
func TestCookieValue(t *testing.T) {
	header := "NEXT_LOCALE=ja; other=1"
	if got := cookieValue(header, LocaleCookieName); got != "ja" {
		t.Fatalf("应取到 ja，收到 %q", got)
	}
	if got := cookieValue("other=1", LocaleCookieName); got != "" {
		t.Fatalf("缺失时应为空串，收到 %q", got)
	}
}

// fakeIP 是 IPExtractor 的假实现。
type fakeIP struct {
	ip  string
	err error
}

func (f fakeIP) ClientIP(context.Context, map[string][]string) (string, error) {
	return f.ip, f.err
}

// fakeExpiryMarker 记录惰性过期标记。
type fakeExpiryMarker struct {
	markedUserID int64
	err          error
}

func (m *fakeExpiryMarker) MarkUserExpired(_ context.Context, userID int64) error {
	m.markedUserID = userID
	return m.err
}

// 认证步骤在真实 http 头部大小写混写下也要工作。
func TestAuthStepHeaderCaseInsensitive(t *testing.T) {
	resolver := &fakeAuth{resolution: AuthResolution{User: User{ID: 1, IsEnabled: true}, Key: Key{ID: 1}}}
	header := http.Header{}
	header.Set("X-Api-Key", "sk-upper")
	ctx := newContextWithHeader(t, header, nil)

	response, err := Deps{Auth: resolver}.authStep()(ctx)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if response != nil {
		t.Fatalf("不应有响应，收到 %d", response.Status)
	}
	if resolver.lastKey != "sk-upper" {
		t.Fatalf("应取到大写头部的值，收到 %q", resolver.lastKey)
	}
}
