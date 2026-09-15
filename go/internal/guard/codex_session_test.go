package guard

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// 本文件的用例钉住 Node `session-guard.ts:109-131` 那一段的三个门与写回位置：
// 系统设置开关、非原始透传（`!allowRawSessionContext`）、Codex 形态正文（顶层 input 数组）。

// fakeCodexCompleter 是 CodexSessionCompleter 的假实现。
type fakeCodexCompleter struct {
	result   CodexSessionCompletionResult
	err      error
	requests []CodexSessionCompletionRequest
}

func (c *fakeCodexCompleter) Complete(
	_ context.Context,
	request CodexSessionCompletionRequest,
) (CodexSessionCompletionResult, error) {
	c.requests = append(c.requests, request)
	return c.result, c.err
}

// codexRequestBody 是 Codex（Responses）形态的最小正文。
func codexRequestBody() map[string]any {
	return map[string]any{"input": []any{map[string]any{"type": "message", "content": "ping"}}}
}

// completeAll 造一个「两侧都缺、需补齐」的补全结果。
func completeAll(sessionID string) CodexSessionCompletionResult {
	return CodexSessionCompletionResult{
		Applied:               true,
		Action:                "completed_missing_fields",
		Source:                "header_session_id",
		SessionID:             sessionID,
		SetBodyPromptCacheKey: true,
		SetHeaderSessionID:    true,
		SetHeaderXSessionID:   true,
	}
}

// 开关开启、正文是 Codex 形态、非原始透传时：正文与两个请求头都被补齐，并留下审计事实。
func TestSessionStepCompletesCodexSessionIdentifiers(t *testing.T) {
	settings := fakeSettings{}
	settings.settings.codexCompletion = true
	completer := &fakeCodexCompleter{result: completeAll("01a0a2a1-c7ff-7747-81cc-4e27411e8938")}
	binder := &fakeBinder{result: SessionResult{SessionID: "sess-1"}}
	body := codexRequestBody()
	factory, access := bodyFactory(t, body)
	deps := Deps{Settings: settings, Sessions: binder, Body: factory, CodexCompletion: completer}

	ctx := newContext(t, map[string]string{"user-agent": "codex-cli/1.0"}, body)
	withAuth(ctx, 3, 7, "sk-x")

	if _, err := deps.sessionStep()(ctx); err != nil {
		t.Fatalf("不应报错: %v", err)
	}

	if len(completer.requests) != 1 {
		t.Fatalf("应调用一次补全，收到 %d", len(completer.requests))
	}
	if completer.requests[0].KeyID != 3 || completer.requests[0].UserAgent != "codex-cli/1.0" {
		t.Fatalf("补全入参不符: %+v", completer.requests[0])
	}
	if access.current["prompt_cache_key"] != "01a0a2a1-c7ff-7747-81cc-4e27411e8938" {
		t.Fatalf("正文未补 prompt_cache_key: %v", access.current)
	}
	if access.stores == 0 {
		t.Fatalf("正文改写后必须写回，否则后续步骤读到旧正文")
	}
	if got := ctx.Headers().Get("session_id"); got != "01a0a2a1-c7ff-7747-81cc-4e27411e8938" {
		t.Fatalf("session_id 头 = %q", got)
	}
	if got := ctx.Headers().Get("x-session-id"); got != "01a0a2a1-c7ff-7747-81cc-4e27411e8938" {
		t.Fatalf("x-session-id 头 = %q", got)
	}
	completion, ok := ctx.CodexSessionCompletion()
	if !ok {
		t.Fatalf("审计事实未落到上下文")
	}
	if completion.Action != "completed_missing_fields" || completion.Source != "header_session_id" ||
		completion.SessionID != "01a0a2a1-c7ff-7747-81cc-4e27411e8938" {
		t.Fatalf("审计事实不符: %+v", completion)
	}
	// 绑定看到的是**补全后**的正文与请求头（顺序契约：补全在提取会话身份之前）。
	if got := binder.requests[0].Body["prompt_cache_key"]; got != "01a0a2a1-c7ff-7747-81cc-4e27411e8938" {
		t.Fatalf("绑定未看到补全后的正文: %v", got)
	}
	// 注意键的大小写：`ctx.SetHeader` 会写成 Go 的规范形（`Session_id`），与真实入口同形。
	boundHeader := false
	for key, values := range binder.requests[0].Headers {
		if strings.EqualFold(key, "session_id") && len(values) == 1 && values[0] == "01a0a2a1-c7ff-7747-81cc-4e27411e8938" {
			boundHeader = true
		}
	}
	if !boundHeader {
		t.Fatalf("绑定未看到补全后的请求头: %v", binder.requests[0].Headers)
	}
}

// 开关关闭时不动任何东西。
func TestSessionStepSkipsCodexCompletionWhenDisabled(t *testing.T) {
	completer := &fakeCodexCompleter{result: completeAll("x")}
	body := codexRequestBody()
	factory, _ := bodyFactory(t, body)
	deps := Deps{Settings: fakeSettings{}, Sessions: &fakeBinder{}, Body: factory, CodexCompletion: completer}

	ctx := newContext(t, nil, body)
	withAuth(ctx, 3, 7, "sk-x")

	if _, err := deps.sessionStep()(ctx); err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if len(completer.requests) != 0 {
		t.Fatalf("开关关闭时不该补全")
	}
}

// 正文不是 Codex 形态（无顶层 input 数组）时不动。
func TestSessionStepSkipsCodexCompletionForNonCodexBody(t *testing.T) {
	settings := fakeSettings{}
	settings.settings.codexCompletion = true
	completer := &fakeCodexCompleter{result: completeAll("x")}
	body := map[string]any{"messages": []any{}}
	factory, _ := bodyFactory(t, body)
	deps := Deps{Settings: settings, Sessions: &fakeBinder{}, Body: factory, CodexCompletion: completer}

	ctx := newContext(t, nil, body)
	withAuth(ctx, 3, 7, "sk-x")

	if _, err := deps.sessionStep()(ctx); err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if len(completer.requests) != 0 {
		t.Fatalf("非 Codex 正文不该补全")
	}
}

// 原始透传端点（allowRawSession）时不动：Node 的 `!allowRawSessionContext` 同判。
//
// allowRawSession 是两因子：设置开关 × 本端点属原始透传。
func TestSessionStepSkipsCodexCompletionForRawSession(t *testing.T) {
	settings := fakeSettings{}
	settings.settings.codexCompletion = true
	settings.settings.allowRawFallback = true
	completer := &fakeCodexCompleter{result: completeAll("x")}
	body := codexRequestBody()
	factory, _ := bodyFactory(t, body)
	deps := Deps{
		Settings: settings, Sessions: &fakeBinder{}, Body: factory, CodexCompletion: completer,
		EndpointRawPassthrough: true,
	}

	ctx := newContext(t, nil, body)
	withAuth(ctx, 3, 7, "sk-x")

	if _, err := deps.sessionStep()(ctx); err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if len(completer.requests) != 0 {
		t.Fatalf("原始透传端点时不该补全")
	}
}

// 两因子的四种组合：只有「设置=true 且端点属原始透传」才跳过补全。
//
// 根因回归：Go 曾把 `allowRawSession` 直接赋成设置值，丢掉端点因子——生产该设置为 true，
// 于是 /v1/responses 的闸门被永久关死，prompt_cache_key 从不注入。
func TestSessionStepCodexCompletionGateIsTwoFactor(t *testing.T) {
	cases := []struct {
		name               string
		setting            bool
		rawPassthrough     bool
		wantCompletionRuns bool
	}{
		{"设置为真 + 普通端点（/v1/responses）", true, false, true},
		{"设置为真 + 原始透传端点（count_tokens）", true, true, false},
		{"设置为假 + 普通端点", false, false, true},
		// 设置关闭时回退本身就不生效（Node：rawFallbackEnabled = 设置 && 端点，缺一即假），
		// 故闸门是开的。只有两因子同时为真才跳过补全。
		{"设置为假 + 原始透传端点（回退未启用）", false, true, true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			settings := fakeSettings{}
			settings.settings.codexCompletion = true
			settings.settings.allowRawFallback = testCase.setting
			completer := &fakeCodexCompleter{result: completeAll("01a0a2a1-c7ff-7747-81cc-4e27411e8938")}
			body := codexRequestBody()
			factory, access := bodyFactory(t, body)
			deps := Deps{
				Settings: settings, Sessions: &fakeBinder{}, Body: factory,
				CodexCompletion:        completer,
				EndpointRawPassthrough: testCase.rawPassthrough,
			}

			ctx := newContext(t, nil, body)
			withAuth(ctx, 3, 7, "sk-x")

			if _, err := deps.sessionStep()(ctx); err != nil {
				t.Fatalf("不应报错: %v", err)
			}
			if got := len(completer.requests) == 1; got != testCase.wantCompletionRuns {
				t.Fatalf("补全执行 = %v，期望 %v", got, testCase.wantCompletionRuns)
			}
			if !testCase.wantCompletionRuns {
				return
			}
			// 闸门开的这条路上要有真实效果：正文、两个请求头与审计事实都得在。
			if access.current["prompt_cache_key"] != "01a0a2a1-c7ff-7747-81cc-4e27411e8938" {
				t.Fatalf("正文未补 prompt_cache_key: %v", access.current)
			}
			if got := ctx.Headers().Get("x-session-id"); got != "01a0a2a1-c7ff-7747-81cc-4e27411e8938" {
				t.Fatalf("x-session-id 头 = %q", got)
			}
			completion, ok := ctx.CodexSessionCompletion()
			if !ok {
				t.Fatalf("审计条目未产出：闸门开的路上必须有 codex_session_id_completion")
			}
			if completion.Action != "completed_missing_fields" || completion.Source != "header_session_id" {
				t.Fatalf("审计事实不符: %+v", completion)
			}
		})
	}
}

// action=none（两侧齐全）时不写头、不写正文、不记审计。
func TestSessionStepCodexCompletionNoneIsNoop(t *testing.T) {
	settings := fakeSettings{}
	settings.settings.codexCompletion = true
	completer := &fakeCodexCompleter{result: CodexSessionCompletionResult{
		Applied: false, Action: "none", Source: "header_session_id", SessionID: "01a0a2a1-c7ff-7747-81cc-4e27411e8938",
	}}
	body := codexRequestBody()
	factory, access := bodyFactory(t, body)
	deps := Deps{Settings: settings, Sessions: &fakeBinder{}, Body: factory, CodexCompletion: completer}

	ctx := newContext(t, nil, body)
	withAuth(ctx, 3, 7, "sk-x")

	if _, err := deps.sessionStep()(ctx); err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if _, ok := ctx.CodexSessionCompletion(); ok {
		t.Fatalf("action=none 时不该留下审计事实")
	}
	if access.stores != 0 {
		t.Fatalf("action=none 时不该写回正文")
	}
	if ctx.Headers().Get("session_id") != "" {
		t.Fatalf("action=none 时不该写头")
	}
}

// 正文写不回时整体放弃：不写头、不记审计（补一半会让审计与实际上游不一致）。
func TestSessionStepCodexCompletionAbortsWhenBodyStoreFails(t *testing.T) {
	settings := fakeSettings{}
	settings.settings.codexCompletion = true
	completer := &fakeCodexCompleter{result: completeAll("01a0a2a1-c7ff-7747-81cc-4e27411e8938")}
	body := codexRequestBody()
	factory, access := bodyFactory(t, body)
	access.err = errors.New("正文不可写")
	deps := Deps{Settings: settings, Sessions: &fakeBinder{}, Body: factory, CodexCompletion: completer}

	ctx := newContext(t, nil, body)
	withAuth(ctx, 3, 7, "sk-x")

	if _, err := deps.sessionStep()(ctx); err != nil {
		t.Fatalf("补全失败不该让请求失败: %v", err)
	}
	if _, ok := ctx.CodexSessionCompletion(); ok {
		t.Fatalf("写不回时不该留下审计事实")
	}
	if ctx.Headers().Get("session_id") != "" {
		t.Fatalf("写不回时不该只写头")
	}
}

// 补全器报错时 fail-open：请求继续，不写任何东西。
func TestSessionStepCodexCompletionErrorIsFailOpen(t *testing.T) {
	settings := fakeSettings{}
	settings.settings.codexCompletion = true
	completer := &fakeCodexCompleter{err: errors.New("redis down")}
	binder := &fakeBinder{result: SessionResult{SessionID: "sess-1"}}
	body := codexRequestBody()
	factory, _ := bodyFactory(t, body)
	deps := Deps{Settings: settings, Sessions: binder, Body: factory, CodexCompletion: completer}

	ctx := newContext(t, nil, body)
	withAuth(ctx, 3, 7, "sk-x")

	if _, err := deps.sessionStep()(ctx); err != nil {
		t.Fatalf("补全报错不该让请求失败: %v", err)
	}
	if len(binder.requests) != 1 {
		t.Fatalf("会话绑定仍应执行")
	}
	if _, ok := ctx.CodexSessionCompletion(); ok {
		t.Fatalf("补全报错时不该留下审计事实")
	}
}

// 未接线（CodexCompletion 为 nil）时静默跳过。
func TestSessionStepWithoutCodexCompleter(t *testing.T) {
	settings := fakeSettings{}
	settings.settings.codexCompletion = true
	body := codexRequestBody()
	factory, _ := bodyFactory(t, body)
	deps := Deps{Settings: settings, Sessions: &fakeBinder{}, Body: factory}

	ctx := newContext(t, nil, body)
	withAuth(ctx, 3, 7, "sk-x")

	if _, err := deps.sessionStep()(ctx); err != nil {
		t.Fatalf("不应报错: %v", err)
	}
}

// hasCodexInputArray 的判据：顶层 input 必须是数组。
func TestHasCodexInputArray(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
		want bool
	}{
		{name: "input 数组", body: map[string]any{"input": []any{}}, want: true},
		{name: "input 非数组", body: map[string]any{"input": "x"}, want: false},
		{name: "无 input", body: map[string]any{"messages": []any{}}, want: false},
		{name: "空正文", body: nil, want: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := hasCodexInputArray(testCase.body); got != testCase.want {
				t.Fatalf("hasCodexInputArray = %v，期望 %v", got, testCase.want)
			}
		})
	}
}
