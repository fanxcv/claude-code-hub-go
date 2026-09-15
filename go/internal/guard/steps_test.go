package guard

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// 探测识别是「网关抢答」的入口：判定必须严格，否则正常请求会被抢答成空响应。
func TestProbeStep(t *testing.T) {
	cases := []struct {
		name     string
		body     map[string]any
		wantHit  bool
		wantBody string
	}{
		{
			name:     "单条 foo",
			body:     map[string]any{"messages": []any{map[string]any{"content": "Foo"}}},
			wantHit:  true,
			wantBody: `{"input_tokens":0}`,
		},
		{
			name:    "单条 count",
			body:    map[string]any{"messages": []any{map[string]any{"content": " count "}}},
			wantHit: true,
		},
		{
			name:    "两条消息不是探测",
			body:    map[string]any{"messages": []any{map[string]any{"content": "foo"}, map[string]any{"content": "foo"}}},
			wantHit: false,
		},
		{
			name:    "content 非字符串不是探测",
			body:    map[string]any{"messages": []any{map[string]any{"content": []any{"foo"}}}},
			wantHit: false,
		},
		{
			name:    "普通内容不是探测",
			body:    map[string]any{"messages": []any{map[string]any{"content": "你好"}}},
			wantHit: false,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			factory, _ := bodyFactory(t, testCase.body)
			ctx := newContext(t, nil, testCase.body)
			response, err := (Deps{Body: factory}).probeStep()(ctx)
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if !testCase.wantHit {
				if response != nil {
					t.Fatalf("不应抢答，收到 %d (%s)", response.Status, string(response.Body))
				}
				return
			}
			if response == nil || response.Status != 200 {
				t.Fatalf("应抢答 200，收到 %v", response)
			}
			if testCase.wantBody != "" && string(response.Body) != testCase.wantBody {
				t.Fatalf("响应体应为 %s，收到 %s", testCase.wantBody, string(response.Body))
			}
		})
	}
}

// 无正文通道时探测步骤放行（不能因为没有正文就把请求当探测抢答）。
func TestProbeStepWithoutBodyAccess(t *testing.T) {
	ctx := newContext(t, nil, nil)
	response, err := (Deps{}).probeStep()(ctx)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if response != nil {
		t.Fatalf("无正文时不应抢答，收到 %d", response.Status)
	}
}

// 模型限制只在用户配置了白名单时生效。
func TestModelStep(t *testing.T) {
	cases := []struct {
		name       string
		allowed    []string
		body       map[string]any
		wantStatus int
	}{
		{name: "未配置白名单则全放过", allowed: nil, body: map[string]any{"model": "any"}, wantStatus: 0},
		{name: "白名单内放行", allowed: []string{"claude-sonnet-4"}, body: map[string]any{"model": "claude-sonnet-4"}, wantStatus: 0},
		{name: "大小写不敏感", allowed: []string{"Claude-Sonnet-4"}, body: map[string]any{"model": "claude-sonnet-4"}, wantStatus: 0},
		{name: "白名单外的模型拒绝", allowed: []string{"claude-sonnet-4"}, body: map[string]any{"model": "gpt-4"}, wantStatus: 400},
		{name: "配了白名单则模型必填", allowed: []string{"claude-sonnet-4"}, body: map[string]any{}, wantStatus: 400},
		{name: "空白模型视为缺失", allowed: []string{"claude-sonnet-4"}, body: map[string]any{"model": "   "}, wantStatus: 400},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			factory, _ := bodyFactory(t, testCase.body)
			deps := Deps{
				Users: fakeUsers{user: User{ID: 7, IsEnabled: true, AllowedModels: testCase.allowed}},
				Body:  factory,
			}
			ctx := newContext(t, nil, testCase.body)
			withAuth(ctx, 3, 7, "sk-x")

			response, err := deps.modelStep()(ctx)
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if testCase.wantStatus == 0 {
				if response != nil {
					t.Fatalf("应放行，收到 %d (%s)", response.Status, string(response.Body))
				}
				return
			}
			if response == nil || response.Status != testCase.wantStatus {
				t.Fatalf("应返回 %d，收到 %v", testCase.wantStatus, response)
			}
			if got := errorField(t, response, "type"); got != "invalid_request_error" {
				t.Fatalf("type 应为 invalid_request_error，收到 %s", got)
			}
		})
	}
}

// 未鉴权时模型守卫跳过（认证本应已拦截）。
func TestModelStepSkipsWithoutAuth(t *testing.T) {
	ctx := newContext(t, nil, map[string]any{"model": "gpt-4"})
	response, err := Deps{Users: fakeUsers{user: User{AllowedModels: []string{"claude"}}}}.modelStep()(ctx)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if response != nil {
		t.Fatalf("未鉴权时应跳过，收到 %d", response.Status)
	}
}

// 用户目录读失败必须上报错误：读不到白名单就放行等于白名单失效。
func TestModelStepUserDirectoryFailureIsNotFailOpen(t *testing.T) {
	deps := Deps{Users: fakeUsers{err: errors.New("数据库不可达")}}
	ctx := newContext(t, nil, map[string]any{"model": "gpt-4"})
	withAuth(ctx, 3, 7, "sk-x")

	if _, err := deps.modelStep()(ctx); err == nil {
		t.Fatal("用户目录故障时必须上报错误")
	}
}

// 客户端限制：白/黑名单、通配、Codex 别名、内置关键字需要信号确认。
func TestClientStep(t *testing.T) {
	claudeHeaders := map[string]string{
		"x-app":          "cli",
		"anthropic-beta": "prompt-caching-2024-07-31",
	}

	cases := []struct {
		name       string
		allowed    []string
		blocked    []string
		headers    map[string]string
		body       map[string]any
		wantStatus int
	}{
		{name: "未配置则不检查", headers: map[string]string{}, body: map[string]any{}, wantStatus: 0},
		{name: "UA 通配命中", allowed: []string{"python-*"}, headers: map[string]string{"user-agent": "python-requests/2.31"}, body: map[string]any{}, wantStatus: 0},
		{name: "UA 通配未命中", allowed: []string{"curl-*"}, headers: map[string]string{"user-agent": "python-requests/2.31"}, body: map[string]any{}, wantStatus: 400},
		{name: "配了白名单则 UA 必填", allowed: []string{"curl"}, headers: map[string]string{}, body: map[string]any{}, wantStatus: 400},
		{name: "只配黑名单时缺 UA 放行", blocked: []string{"curl"}, headers: map[string]string{}, body: map[string]any{}, wantStatus: 0},
		{name: "黑名单命中", blocked: []string{"python-*"}, headers: map[string]string{"user-agent": "python-requests/2.31"}, body: map[string]any{}, wantStatus: 400},
		{name: "黑名单优先于白名单", allowed: []string{"python-*"}, blocked: []string{"python-*"}, headers: map[string]string{"user-agent": "python-requests/2.31"}, body: map[string]any{}, wantStatus: 400},
		{
			name:       "内置关键字需要信号确认",
			allowed:    []string{"claude-code"},
			headers:    map[string]string{"user-agent": "claude-cli/1.0.0"},
			body:       map[string]any{},
			wantStatus: 400,
		},
		{
			name:       "三信号齐备即确认",
			allowed:    []string{"claude-code"},
			headers:    mergeHeaders(claudeHeaders, map[string]string{"user-agent": "claude-cli/1.0.0"}),
			body:       map[string]any{"metadata": map[string]any{"user_id": "u-1"}},
			wantStatus: 0,
		},
		{
			name:       "count_tokens 无需 metadata.user_id",
			allowed:    []string{"claude-code"},
			headers:    mergeHeaders(claudeHeaders, map[string]string{"user-agent": "claude-cli/1.0.0"}),
			body:       map[string]any{},
			wantStatus: 0,
		},
		{
			name:       "子客户端关键字命中",
			allowed:    []string{"claude-code-cli"},
			headers:    mergeHeaders(claudeHeaders, map[string]string{"user-agent": "claude-cli/1.0.0 (external, cli)"}),
			body:       map[string]any{"metadata": map[string]any{"user_id": "u-1"}},
			wantStatus: 0,
		},
		{
			name:       "Codex 家族别名",
			allowed:    []string{"codex-cli"},
			headers:    map[string]string{"user-agent": "codex_exec/0.9"},
			body:       map[string]any{},
			wantStatus: 0,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			path := "/v1/messages"
			if strings.Contains(testCase.name, "count_tokens") {
				path = "/v1/messages/count_tokens"
			}
			factory, _ := bodyFactory(t, testCase.body)
			deps := Deps{
				Users: fakeUsers{user: User{
					ID:             7,
					IsEnabled:      true,
					AllowedClients: testCase.allowed,
					BlockedClients: testCase.blocked,
				}},
				Body: factory,
			}
			ctx := newContextAtPath(t, path, testCase.headers, testCase.body)
			withAuth(ctx, 3, 7, "sk-x")

			response, err := deps.clientStep()(ctx)
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if testCase.wantStatus == 0 {
				if response != nil {
					t.Fatalf("应放行，收到 %d (%s)", response.Status, string(response.Body))
				}
				return
			}
			if response == nil || response.Status != testCase.wantStatus {
				t.Fatalf("应返回 %d，收到 %v", testCase.wantStatus, response)
			}
		})
	}
}

// 拦截文案要带上被识别出的客户端，便于排查。
func TestClientStepDetectedSuffix(t *testing.T) {
	headers := map[string]string{
		"user-agent":     "claude-cli/1.0.0 (external, cli)",
		"x-app":          "cli",
		"anthropic-beta": "beta-1",
	}
	factory, _ := bodyFactory(t, map[string]any{"metadata": map[string]any{"user_id": "u-1"}})
	deps := Deps{
		Users: fakeUsers{user: User{ID: 7, IsEnabled: true, BlockedClients: []string{"claude-code-cli"}}},
		Body:  factory,
	}
	ctx := newContextAtPath(t, "/v1/messages", headers, nil)
	withAuth(ctx, 3, 7, "sk-x")

	response, err := deps.clientStep()(ctx)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if response == nil {
		t.Fatal("应拦截")
	}
	message := errorField(t, response, "message")
	if !strings.Contains(message, "Client blocked") || !strings.Contains(message, "claude-code-cli") {
		t.Fatalf("文案应含被拦截的客户端：%s", message)
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern string
		text    string
		want    bool
	}{
		{pattern: "python-*", text: "Python-Requests/2.31", want: true},
		{pattern: "*requests*", text: "python-requests/2.31", want: true},
		{pattern: "curl*", text: "python-requests", want: false},
		{pattern: "*", text: "anything", want: true},
		{pattern: "a*b*c", text: "aXXbYYc", want: true},
		{pattern: "a*b*c", text: "aXXcYYb", want: false},
	}
	for _, testCase := range cases {
		if got := globMatch(testCase.pattern, testCase.text); got != testCase.want {
			t.Errorf("globMatch(%q, %q) 应为 %v，收到 %v", testCase.pattern, testCase.text, testCase.want, got)
		}
	}
}

// 预热的判定必须严格：任一条件不满足都不能抢答。
func TestWarmupStep(t *testing.T) {
	warmupBody := map[string]any{
		"model": "claude-sonnet-4",
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type":          "text",
				"text":          "warmup",
				"cache_control": map[string]any{"type": "ephemeral"},
			}},
		}},
	}

	cases := []struct {
		name          string
		body          map[string]any
		headers       map[string]string
		intercept     bool
		wantIntercept bool
	}{
		{name: "标准预热请求被抢答", body: warmupBody, intercept: true, wantIntercept: true},
		{name: "开关关闭则放行", body: warmupBody, intercept: false},
		{
			name: "缺 cache_control 不抢答",
			body: map[string]any{"messages": []any{map[string]any{
				"role":    "user",
				"content": []any{map[string]any{"type": "text", "text": "warmup"}},
			}}},
			intercept: true,
		},
		{
			name: "文本不是 warmup 不抢答",
			body: map[string]any{"messages": []any{map[string]any{
				"role": "user",
				"content": []any{map[string]any{
					"type": "text", "text": "hello",
					"cache_control": map[string]any{"type": "ephemeral"},
				}},
			}}},
			intercept: true,
		},
		{
			name: "role 不是 user 不抢答",
			body: map[string]any{"messages": []any{map[string]any{
				"role": "assistant",
				"content": []any{map[string]any{
					"type": "text", "text": "warmup",
					"cache_control": map[string]any{"type": "ephemeral"},
				}},
			}}},
			intercept: true,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			factory, _ := bodyFactory(t, testCase.body)
			settings := fakeSettings{}
			settings.settings.interceptWarmup = testCase.intercept
			recorder := &fakeWarmupLog{}
			deps := Deps{Body: factory, Settings: settings, WarmupLog: recorder}

			ctx := newContext(t, testCase.headers, testCase.body)
			withAuth(ctx, 3, 7, "sk-x")

			response, err := deps.warmupStep()(ctx)
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if !testCase.wantIntercept {
				if response != nil {
					t.Fatalf("不应抢答，收到 %d (%s)", response.Status, string(response.Body))
				}
				return
			}
			if response == nil || response.Status != 200 {
				t.Fatalf("应抢答 200，收到 %v", response)
			}
			body := string(response.Body)
			if !strings.Contains(body, `"stop_reason":"end_turn"`) || !strings.Contains(body, `"text":"I'm ready to help you."`) {
				t.Fatalf("抢答体形状与 Node 不一致: %s", body)
			}
			if !strings.Contains(body, `"model":"claude-sonnet-4"`) {
				t.Fatalf("抢答体应带请求模型: %s", body)
			}
			if len(recorder.records) != 1 {
				t.Fatalf("应记录一条抢答日志，收到 %d", len(recorder.records))
			}
			if recorder.records[0].MessagesCount != 1 {
				t.Fatalf("日志里的消息条数应为 1，收到 %d", recorder.records[0].MessagesCount)
			}
		})
	}
}

// 模型缺失时抢答体的 model 写 unknown（与 Node 的 `model ?? "unknown"` 一致）。
func TestWarmupPayloadDefaultsModel(t *testing.T) {
	payload := buildWarmupPayload("")
	if payload.Model != "unknown" {
		t.Fatalf("模型缺失时应写 unknown，收到 %q", payload.Model)
	}
	if !strings.HasPrefix(payload.ID, "msg_cch_") || len(payload.ID) != len("msg_cch_")+16 {
		t.Fatalf("消息 id 形状应为 msg_cch_ + 16 位十六进制，收到 %q", payload.ID)
	}
}

// 版本检查是 fail-open 的软开关。
func TestVersionStep(t *testing.T) {
	enabled := fakeSettings{}
	enabled.settings.enableClientVersionCheck = true

	failing := fakeSettings{err: errors.New("数据库不可达")}

	cases := []struct {
		name       string
		settings   SettingsSource
		checker    *fakeVersionChecker
		wantStatus int
		wantGA     string
	}{
		{
			name:       "开关关闭则跳过",
			settings:   fakeSettings{},
			checker:    &fakeVersionChecker{client: ClientVersion{ClientType: "cli", Version: "1.0.0"}, parsed: true},
			wantStatus: 0,
		},
		{
			name:       "版本合规则放行",
			settings:   enabled,
			checker:    &fakeVersionChecker{client: ClientVersion{ClientType: "cli", Version: "2.0.0"}, parsed: true},
			wantStatus: 0,
		},
		{
			name:     "版本过旧则拦截",
			settings: enabled,
			checker: &fakeVersionChecker{
				client:       ClientVersion{ClientType: "cli", Version: "1.0.0"},
				parsed:       true,
				needsUpgrade: true,
				gaVersion:    "2.0.0",
			},
			wantStatus: 400,
			wantGA:     "2.0.0",
		},
		{
			name:       "设置读取失败则放行",
			settings:   failing,
			checker:    &fakeVersionChecker{client: ClientVersion{ClientType: "cli", Version: "1.0.0"}, parsed: true},
			wantStatus: 0,
		},
		{
			name:       "UA 解析失败则放行",
			settings:   enabled,
			checker:    &fakeVersionChecker{parsed: false},
			wantStatus: 0,
		},
		{
			name:       "检查出错则放行",
			settings:   enabled,
			checker:    &fakeVersionChecker{client: ClientVersion{ClientType: "cli", Version: "1.0.0"}, parsed: true, checkErr: errors.New("查库失败")},
			wantStatus: 0,
		},
		{
			name:       "缝隙缺失则跳过",
			settings:   nil,
			checker:    nil,
			wantStatus: 0,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			deps := Deps{Settings: testCase.settings}
			if testCase.checker != nil {
				deps.Versions = testCase.checker
			}
			ctx := newContext(t, map[string]string{"user-agent": "claude-cli/1.0.0"}, nil)
			withAuth(ctx, 3, 7, "sk-x")

			response, err := deps.versionStep()(ctx)
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if testCase.wantStatus == 0 {
				if response != nil {
					t.Fatalf("应放行，收到 %d (%s)", response.Status, string(response.Body))
				}
				return
			}
			if response == nil || response.Status != testCase.wantStatus {
				t.Fatalf("应返回 %d，收到 %v", testCase.wantStatus, response)
			}
			var payload struct {
				Error struct {
					Type            string `json:"type"`
					Message         string `json:"message"`
					CurrentVersion  string `json:"current_version"`
					RequiredVersion string `json:"required_version"`
				} `json:"error"`
			}
			if err := jsonUnmarshal(response.Body, &payload); err != nil {
				t.Fatalf("响应不是 JSON: %v", err)
			}
			if payload.Error.Type != "client_upgrade_required" {
				t.Fatalf("type 应为 client_upgrade_required，收到 %s", payload.Error.Type)
			}
			if payload.Error.RequiredVersion != testCase.wantGA {
				t.Fatalf("required_version 应为 %s，收到 %s", testCase.wantGA, payload.Error.RequiredVersion)
			}
			if !strings.Contains(payload.Error.Message, "outdated") {
				t.Fatalf("文案应说明过旧：%s", payload.Error.Message)
			}
			if got := response.Headers.Get("content-type"); got != "application/json" {
				t.Fatalf("content-type 应为 application/json，收到 %q", got)
			}
		})
	}
}

// 版本过旧时仍应尽力记录用户版本（与 Node 的异步更新一致）。
func TestVersionStepRecordsUserVersion(t *testing.T) {
	enabled := fakeSettings{}
	enabled.settings.enableClientVersionCheck = true
	checker := &fakeVersionChecker{client: ClientVersion{ClientType: "cli", Version: "1.0.0"}, parsed: true}

	deps := Deps{Settings: enabled, Versions: checker}
	ctx := newContext(t, nil, nil)
	withAuth(ctx, 3, 7, "sk-x")
	if _, err := deps.versionStep()(ctx); err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if checker.updatedUserID != 7 {
		t.Fatalf("应记录用户 7 的版本，收到 %d", checker.updatedUserID)
	}
}

// 会话步骤按系统设置决定调试工件位，并把绑定交给缝隙。
func TestSessionStep(t *testing.T) {
	cases := []struct {
		name              string
		highConcurrency   bool
		wantDebugArtifact bool
	}{
		{name: "高并发模式关闭调试工件", highConcurrency: true, wantDebugArtifact: false},
		{name: "普通模式保留调试工件", highConcurrency: false, wantDebugArtifact: true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			settings := fakeSettings{}
			settings.settings.highConcurrency = testCase.highConcurrency
			settings.settings.allowRawFallback = true
			binder := &fakeBinder{result: SessionResult{SessionID: "sess-1", Sequence: 3}}
			body := map[string]any{"messages": []any{}}
			factory, _ := bodyFactory(t, body)
			// 两因子：设置开关 × 端点属原始透传（Node session.ts:574-582）。
			deps := Deps{
				Settings: settings, Sessions: binder, Body: factory,
				EndpointRawPassthrough: true,
			}

			ctx := newContext(t, map[string]string{"user-agent": "claude-cli/1.0.0"}, body)
			withAuth(ctx, 3, 7, "sk-x")

			response, err := deps.sessionStep()(ctx)
			if err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if response != nil {
				t.Fatalf("会话步骤不应抢答，收到 %d", response.Status)
			}
			if got := ctx.ShouldPersistDebugArtifacts(); got != testCase.wantDebugArtifact {
				t.Fatalf("调试工件位应为 %v，收到 %v", testCase.wantDebugArtifact, got)
			}
			if len(binder.requests) != 1 {
				t.Fatalf("应调用一次会话绑定，收到 %d", len(binder.requests))
			}
			if binder.requests[0].KeyID != 3 || !binder.requests[0].AllowRawSession {
				t.Fatalf("绑定请求参数不符: %+v", binder.requests[0])
			}
		})
	}
}

// 无密钥时不分配会话。
func TestSessionStepSkipsWithoutKey(t *testing.T) {
	binder := &fakeBinder{}
	ctx := newContext(t, nil, nil)
	response, err := (Deps{Sessions: binder}).sessionStep()(ctx)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if response != nil || len(binder.requests) != 0 {
		t.Fatalf("无密钥时不应分配会话：%v", binder.requests)
	}
}

// 限流步骤把判定翻译成 **Node 同形的七字段信封与 X-RateLimit-* 头**并早退。
//
// 形状逐条钉住：`code` 固定 rate_limit_exceeded（不是 errorType 的回显）、limit_type/current/limit/
// reset_time 平铺在 error 下，且 Retry-After 与 X-RateLimit-Reset 只在有固定重置时刻时出现。
func TestRateLimitStep(t *testing.T) {
	retryAfter := 15
	limiter := &fakeLimiter{block: &RateLimitBlock{
		Status:            429,
		Message:           "已达今日额度",
		ErrorType:         "rate_limit_error",
		RetryAfterSeconds: &retryAfter,
		LimitType:         "rpm",
		Current:           1,
		Limit:             1,
		ResetTime:         "2026-09-12T16:28:05.527Z",
	}}
	ctx := newContext(t, nil, nil)

	response, err := (Deps{RateLimit: limiter}).rateLimitStep()(ctx)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if response == nil || response.Status != 429 {
		t.Fatalf("应返回 429，收到 %v", response)
	}
	if got := response.Headers.Get("Retry-After"); got != "15" {
		t.Fatalf("Retry-After 应为 15，收到 %q", got)
	}
	if got := response.Headers.Get("X-RateLimit-Limit"); got != "1" {
		t.Fatalf("X-RateLimit-Limit 应为 1，收到 %q", got)
	}
	if got := response.Headers.Get("X-RateLimit-Remaining"); got != "0" {
		t.Fatalf("X-RateLimit-Remaining 应为 0，收到 %q", got)
	}
	if got := response.Headers.Get("X-RateLimit-Reset"); got == "" {
		t.Fatal("有固定重置时刻时 X-RateLimit-Reset 必填")
	}
	body := string(response.Body)
	for _, want := range []string{
		`"code":"rate_limit_exceeded"`,
		`"limit_type":"rpm"`,
		`"current":1`,
		`"limit":1`,
		`"reset_time":"2026-09-12T16:28:05.527Z"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("响应体应含 %s：%s", want, body)
		}
	}
	if !strings.Contains(errorField(t, response, "message"), "额度") {
		t.Fatalf("文案应来自限流判定：%s", string(response.Body))
	}
}

// 放行判定不产生响应；缝隙缺失是跳过而不是拦截。
func TestRateLimitStepAllowAndMissing(t *testing.T) {
	ctx := newContext(t, nil, nil)
	response, err := Deps{RateLimit: &fakeLimiter{}}.rateLimitStep()(ctx)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if response != nil {
		t.Fatalf("放行时不应有响应，收到 %d", response.Status)
	}

	response, err = (Deps{}).rateLimitStep()(ctx)
	if err != nil {
		t.Fatalf("缝隙缺失不应报错: %v", err)
	}
	if response != nil {
		t.Fatalf("缝隙缺失时应跳过，收到 %d", response.Status)
	}
}

// 选路步骤把结果写回上下文槽位。
func TestProviderStepWritesSelection(t *testing.T) {
	ctx := newContext(t, nil, nil)
	deps := Deps{Provider: fakeProvider{selection: pctx.ProviderSelection{
		ProviderID: 11, Name: "供应商甲", Type: "claude", Endpoint: "https://upstream.example",
	}}}

	response, err := deps.providerStep()(ctx)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if response != nil {
		t.Fatalf("选路不应抢答，收到 %d", response.Status)
	}
	selection, ok := ctx.Provider()
	if !ok || selection.ProviderID != 11 || selection.Endpoint != "https://upstream.example" {
		t.Fatalf("选路结果未写回: %+v", selection)
	}
}

// 选路失败要上报（选不出供应商就无法转发）。
func TestProviderStepPropagatesError(t *testing.T) {
	deps := Deps{Provider: fakeProvider{err: errors.New("无可用供应商")}}
	if _, err := deps.providerStep()(newContext(t, nil, nil)); err == nil {
		t.Fatal("选路失败时必须上报错误")
	}
}

// messageContext 步骤只创建请求日志上下文。
func TestMessageContextStep(t *testing.T) {
	writer := &fakeMessageContext{}
	ctx := newContext(t, nil, nil)
	if _, err := (Deps{MessageContext: writer}).messageContextStep()(ctx); err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if writer.calls != 1 {
		t.Fatalf("应调用一次上下文创建，收到 %d", writer.calls)
	}

	writer = &fakeMessageContext{err: errors.New("写库失败")}
	if _, err := (Deps{MessageContext: writer}).messageContextStep()(ctx); err == nil {
		t.Fatal("写库失败时必须上报错误")
	}

	// 缝隙缺失时跳过而不是报错。
	if _, err := (Deps{}).messageContextStep()(ctx); err != nil {
		t.Fatalf("缝隙缺失不应报错: %v", err)
	}
}

// 回放命中即抢答；未接线时跳过。
func TestReplayStep(t *testing.T) {
	ctx := newContext(t, nil, nil)

	response, err := (Deps{}).replayStep()(ctx)
	if err != nil {
		t.Fatalf("缝隙缺失不应报错: %v", err)
	}
	if response != nil {
		t.Fatalf("未接线时应跳过，收到 %d", response.Status)
	}

	hit := fakeReplay{response: BuildError(200, "", "api_error")}
	response, err = (Deps{Replay: hit}).replayStep()(ctx)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if response == nil {
		t.Fatal("命中时应抢答")
	}

	miss := fakeReplay{}
	response, err = (Deps{Replay: miss}).replayStep()(ctx)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if response != nil {
		t.Fatalf("未命中时不应抢答，收到 %d", response.Status)
	}
}

// fakeReplay 是 ReplayAttacher 的假实现。
type fakeReplay struct {
	response *Response
	err      error
}

func (r fakeReplay) Attach(context.Context, *pctx.Context) (*Response, error) {
	return r.response, r.err
}
