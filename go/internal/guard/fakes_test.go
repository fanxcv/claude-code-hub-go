package guard

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 测试辅助：所有假实现都只实现守卫链真正调用的那部分语义。

// newContext 建一个带 headers 与正文的上下文。
func newContext(t *testing.T, headers map[string]string, body map[string]any) *pctx.Context {
	t.Helper()
	header := http.Header{}
	for key, value := range headers {
		header.Set(key, value)
	}
	init := pctx.Init{
		Method:  "POST",
		Path:    "/v1/messages",
		Headers: header,
	}
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化正文失败: %v", err)
		}
		init.Body = io.NopCloser(strings.NewReader(string(encoded)))
	}
	ctx, err := pctx.New(init)
	if err != nil {
		t.Fatalf("构造上下文失败: %v", err)
	}
	return ctx
}

// fakeBody 是 BodyAccess 的假实现。
type fakeBody struct {
	current map[string]any
	stores  int
	err     error
}

func newFakeBody(body map[string]any) *fakeBody { return &fakeBody{current: body} }

func (b *fakeBody) JSON() (map[string]any, error) {
	if b.err != nil {
		return nil, b.err
	}
	return b.current, nil
}

func (b *fakeBody) Store(body map[string]any) error {
	b.stores++
	b.current = body
	return nil
}

// bodyFactory 返回一个把正文接到 ctx 的工厂，并把 access 回传给调用方。
func bodyFactory(t *testing.T, body map[string]any) (BodyFactory, *fakeBody) {
	t.Helper()
	access := newFakeBody(body)
	return func(*pctx.Context) (BodyAccess, error) { return access, nil }, access
}

// fakeAuth 是 AuthResolver 的假实现。
type fakeAuth struct {
	resolution AuthResolution
	err        error
	calls      int
	lastKey    string
}

func (a *fakeAuth) ResolveAPIKey(_ context.Context, key string) (AuthResolution, error) {
	a.calls++
	a.lastKey = key
	if a.err != nil {
		return AuthResolution{}, a.err
	}
	return a.resolution, nil
}

// fakeSettings 是 SettingsSource 的假实现。
type fakeSettings struct {
	settings settingsStub
	err      error
}

// settingsStub 是 store.SystemSettings 的构造辅助：只填测试用到的开关。
type settingsStub struct {
	enableClientVersionCheck bool
	interceptWarmup          bool
	highConcurrency          bool
	allowRawFallback         bool
}

func (s fakeSettings) FindSystemSettings(_ context.Context) (*store.SystemSettings, error) {
	if s.err != nil {
		return nil, s.err
	}
	settings := &store.SystemSettings{}
	settings.EnableClientVersionCheck = s.settings.enableClientVersionCheck
	settings.InterceptAnthropicWarmupRequests = s.settings.interceptWarmup
	settings.EnableHighConcurrencyMode = s.settings.highConcurrency
	settings.AllowNonConversationEndpointProviderFallback = s.settings.allowRawFallback
	return settings, nil
}

// fakeLimiter 是 RateLimiter 的假实现。
type fakeLimiter struct {
	throttle   ThrottleDecision
	throttleEr error
	block      *RateLimitBlock
	blockErr   error
	successes  int
	failures   int
	lastKey    string
}

func (l *fakeLimiter) Throttle(context.Context, string, string) (ThrottleDecision, error) {
	return l.throttle, l.throttleEr
}

func (l *fakeLimiter) RecordAuthSuccess(_ context.Context, _, key string) {
	l.successes++
	l.lastKey = key
}

func (l *fakeLimiter) RecordAuthFailure(_ context.Context, _, key string) {
	l.failures++
	l.lastKey = key
}

func (l *fakeLimiter) Check(context.Context, *pctx.Context) (*RateLimitBlock, error) {
	return l.block, l.blockErr
}

// fakeUsers 是 UserDirectory 的假实现。
type fakeUsers struct {
	user User
	err  error
}

func (u fakeUsers) User(context.Context, int64) (User, error) { return u.user, u.err }

// fakeSensitive 是 SensitiveWordSource 的假实现。
type fakeSensitive struct {
	words []SensitiveWord
	err   error
}

func (s fakeSensitive) SensitiveWords(context.Context) ([]SensitiveWord, error) {
	return s.words, s.err
}

// fakeBlockedLog 记录被拦截的请求。
type fakeBlockedLog struct {
	records []BlockedRecord
}

func (l *fakeBlockedLog) RecordBlocked(_ context.Context, record BlockedRecord) error {
	l.records = append(l.records, record)
	return nil
}

// fakeFilters 是 FilterSource 的假实现。
type fakeFilters struct {
	filters []RequestFilter
	err     error
}

func (f fakeFilters) RequestFilters(context.Context) ([]RequestFilter, error) {
	return f.filters, f.err
}

// fakeVersionChecker 是 VersionChecker 的假实现。
type fakeVersionChecker struct {
	client        ClientVersion
	parsed        bool
	needsUpgrade  bool
	gaVersion     string
	checkErr      error
	updateErr     error
	updatedUserID int64
}

func (v *fakeVersionChecker) ParseUserAgent(string) (ClientVersion, bool) { return v.client, v.parsed }

func (v *fakeVersionChecker) UpdateUserVersion(_ context.Context, userID int64, _ ClientVersion) error {
	v.updatedUserID = userID
	return v.updateErr
}

func (v *fakeVersionChecker) ShouldUpgrade(context.Context, ClientVersion) (bool, string, error) {
	return v.needsUpgrade, v.gaVersion, v.checkErr
}

func (v *fakeVersionChecker) DisplayName(clientType string) string {
	return "Claude Code (" + clientType + ")"
}

// fakeBinder 是 SessionBinder 的假实现。
type fakeBinder struct {
	result   SessionResult
	err      error
	requests []SessionRequest
}

func (b *fakeBinder) Ensure(_ context.Context, request SessionRequest) (SessionResult, error) {
	b.requests = append(b.requests, request)
	return b.result, b.err
}

// fakeWarmupLog 记录 warmup 抢答。
type fakeWarmupLog struct {
	records []WarmupRecord
}

func (l *fakeWarmupLog) RecordWarmup(_ context.Context, record WarmupRecord) error {
	l.records = append(l.records, record)
	return nil
}

// fakeProvider 是 ProviderSelector 的假实现。
type fakeProvider struct {
	selection pctx.ProviderSelection
	err       error
}

func (p fakeProvider) Select(context.Context, *pctx.Context) (pctx.ProviderSelection, error) {
	return p.selection, p.err
}

// fakeMessageContext 是 MessageContextWriter 的假实现。
type fakeMessageContext struct {
	calls int
	err   error
}

func (m *fakeMessageContext) EnsureContext(context.Context, *pctx.Context) error {
	m.calls++
	return m.err
}

// withAuth 在上下文里写入鉴权结果。
func withAuth(ctx *pctx.Context, keyID, userID int64, apiKey string) {
	ctx.SetAuth(pctx.AuthState{KeyID: keyID, UserID: userID, KeyName: "测试密钥", APIKey: apiKey})
}

// decodeErrorBody 解出错误响应体，便于逐字段断言。
func decodeErrorBody(t *testing.T, response *Response) map[string]any {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(response.Body, &payload); err != nil {
		t.Fatalf("响应体不是 JSON: %v (%s)", err, string(response.Body))
	}
	return payload
}

// errorField 取 error.<field> 的字符串值。
func errorField(t *testing.T, response *Response, field string) string {
	t.Helper()
	payload := decodeErrorBody(t, response)
	errorObject, ok := payload["error"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 error 对象: %s", string(response.Body))
	}
	value, _ := errorObject[field].(string)
	return value
}

// newContextWithHeader 用一个已构造的 http.Header 建上下文（用于大小写混写等用例）。
func newContextWithHeader(t *testing.T, header http.Header, body map[string]any) *pctx.Context {
	t.Helper()
	init := pctx.Init{Method: "POST", Path: "/v1/messages", Headers: header}
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化正文失败: %v", err)
		}
		init.Body = io.NopCloser(strings.NewReader(string(encoded)))
	}
	ctx, err := pctx.New(init)
	if err != nil {
		t.Fatalf("构造上下文失败: %v", err)
	}
	return ctx
}

// newContextAtPath 用指定路径建上下文（判断 count_tokens 等端点语义时用）。
func newContextAtPath(t *testing.T, path string, headers map[string]string, body map[string]any) *pctx.Context {
	t.Helper()
	header := http.Header{}
	for key, value := range headers {
		header.Set(key, value)
	}
	init := pctx.Init{Method: "POST", Path: path, Headers: header}
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("序列化正文失败: %v", err)
		}
		init.Body = io.NopCloser(strings.NewReader(string(encoded)))
	}
	ctx, err := pctx.New(init)
	if err != nil {
		t.Fatalf("构造上下文失败: %v", err)
	}
	return ctx
}

// mergeHeaders 合并两组头部（后者覆盖前者）。
func mergeHeaders(base, extra map[string]string) map[string]string {
	merged := make(map[string]string, len(base)+len(extra))
	for key, value := range base {
		merged[key] = value
	}
	for key, value := range extra {
		merged[key] = value
	}
	return merged
}

// jsonUnmarshal 是测试内的 JSON 解析包装，避免每处都写 import。
func jsonUnmarshal(data []byte, target any) error { return json.Unmarshal(data, target) }

// pctxSelection 造一个选路结果。
func pctxSelection(providerID int64) pctx.ProviderSelection {
	return pctx.ProviderSelection{ProviderID: providerID, Name: "供应商", Type: "claude", Endpoint: "https://upstream.example"}
}

// containsSubstring 报告子串是否出现（测试断言用）。
func containsSubstring(haystack, needle string) bool { return strings.Contains(haystack, needle) }
