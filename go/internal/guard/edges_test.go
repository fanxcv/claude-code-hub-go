package guard

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// 补齐边界分支：这些用例覆盖的都是「走不到就等于没实现」的路径。

// 数组与标量在路径穿越中的替换语义。
func TestSetValueByPathContainerReplacement(t *testing.T) {
	body := map[string]any{
		"messages": []any{map[string]any{"content": "a"}},
		"scalar":   "not-an-object",
		"typed":    map[string]any{"nested": "v"},
	}

	// 已有数组的下标写入。
	setValueByPath(body, "messages[0].role", "user")
	if role, _ := getValueByPath(body, "messages[0].role"); role != "user" {
		t.Fatalf("数组下标写入失败: %v", role)
	}

	// 标量位置写对象路径：容器类型不符时整体替换。
	setValueByPath(body, "scalar.inner", "x")
	if value, ok := getValueByPath(body, "scalar.inner"); !ok || value != "x" {
		t.Fatalf("标量应被替换成对象: %v (ok=%v)", value, ok)
	}

	// 对象位置写数组路径：同样整体替换。
	setValueByPath(body, "typed[0]", "arr")
	if value, ok := getValueByPath(body, "typed[0]"); !ok || value != "arr" {
		t.Fatalf("对象应被替换成数组: %v (ok=%v)", value, ok)
	}

	// 越界下标不写入（保守：不制造空洞数组）。
	before := body["messages"]
	setValueByPath(body, "messages[5].x", "y")
	if len(body["messages"].([]any)) != len(before.([]any)) {
		t.Fatal("越界下标不应扩展数组")
	}

	// 首段是下标时静默放弃。
	setValueByPath(body, "[0].x", "y")
}

// 删除语义：数组按下标移除元素，路径不存在时不动。
func TestDeleteByPathBranches(t *testing.T) {
	body := map[string]any{"items": []any{"a", "b", "c"}}

	deleteByPath(body, "items[1]")
	items := body["items"].([]any)
	if len(items) != 2 || items[0] != "a" || items[1] != "c" {
		t.Fatalf("数组元素移除失败: %v", items)
	}

	deleteByPath(body, "missing.path")
	deleteByPath(body, "items[9]")
	if len(body["items"].([]any)) != 2 {
		t.Fatal("越界删除不应改变数组")
	}
}

// 匹配器的三种模式与字段提取。
func TestMatchFilterElement(t *testing.T) {
	cases := []struct {
		name    string
		element any
		matcher *filterMatcher
		want    bool
	}{
		{
			name:    "精确匹配字段",
			element: map[string]any{"role": "user"},
			matcher: &filterMatcher{Field: "role", Value: "user", MatchType: "exact"},
			want:    true,
		},
		{
			name:    "精确不匹配",
			element: map[string]any{"role": "assistant"},
			matcher: &filterMatcher{Field: "role", Value: "user", MatchType: "exact"},
		},
		{
			name:    "包含匹配",
			element: map[string]any{"text": "hello world"},
			matcher: &filterMatcher{Field: "text", Value: "world", MatchType: "contains"},
			want:    true,
		},
		{
			name:    "正则匹配",
			element: map[string]any{"text": "id-42"},
			matcher: &filterMatcher{Field: "text", Value: `id-\d+`, MatchType: "regex"},
			want:    true,
		},
		{
			name:    "非法正则不匹配",
			element: map[string]any{"text": "id-42"},
			matcher: &filterMatcher{Field: "text", Value: `(?<=a)b`, MatchType: "regex"},
		},
		{
			name:    "嵌套字段路径",
			element: map[string]any{"meta": map[string]any{"kind": "x"}},
			matcher: &filterMatcher{Field: "meta.kind", Value: "x", MatchType: "exact"},
			want:    true,
		},
		{
			name:    "字段路径断裂",
			element: map[string]any{"meta": "scalar"},
			matcher: &filterMatcher{Field: "meta.kind", Value: "x", MatchType: "exact"},
		},
		{
			name:    "无字段时比较整个元素",
			element: "scalar",
			matcher: &filterMatcher{Value: "scalar", MatchType: "exact"},
			want:    true,
		},
		{name: "空匹配器", element: "x", matcher: nil},
		{
			name:    "未知匹配类型",
			element: "x",
			matcher: &filterMatcher{Value: "x", MatchType: "unknown"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := matchFilterElement(testCase.element, testCase.matcher); got != testCase.want {
				t.Fatalf("应为 %v，收到 %v", testCase.want, got)
			}
		})
	}
}

// JS 的 String(value) 语义：对象走 JSON，数字去尾零。
func TestToStringValue(t *testing.T) {
	cases := []struct {
		value any
		want  string
	}{
		{value: nil, want: "undefined"},
		{value: "a", want: "a"},
		{value: true, want: "true"},
		{value: false, want: "false"},
		{value: float64(2), want: "2"},
		{value: 2.5, want: "2.5"},
		{value: json.Number("3"), want: "3"},
		{value: map[string]any{"a": float64(1)}, want: `{"a":1}`},
	}
	for _, testCase := range cases {
		if got := toStringValue(testCase.value); got != testCase.want {
			t.Errorf("%v 应转为 %q，收到 %q", testCase.value, testCase.want, got)
		}
	}
}

// 头部相的 advanced 操作与未知操作。
func TestApplyHeaderFilterAdvanced(t *testing.T) {
	deps := Deps{}
	ctx := newContext(t, map[string]string{"x-drop": "1", "x-keep": "1"}, nil)

	filter := RequestFilter{
		ID: 1, Scope: "header", RuleMode: "advanced",
		Operations: json.RawMessage(`[
			{"op":"set","scope":"header","path":"x-add","value":"v"},
			{"op":"set","scope":"header","path":"x-keep","value":"ignored","writeMode":"if_missing"},
			{"op":"remove","scope":"header","path":"x-drop"},
			{"op":"merge","scope":"body","path":"a","value":{}}
		]`),
	}
	deps.applyHeaderFilter(filter, ctx)

	if got := ctx.Headers().Get("x-add"); got != "v" {
		t.Fatalf("advanced set 应写入头部，收到 %q", got)
	}
	if got := ctx.Headers().Get("x-keep"); got != "1" {
		t.Fatalf("if_missing 不应覆盖已有头部，收到 %q", got)
	}
	if ctx.Headers().Has("x-drop") {
		t.Fatal("advanced remove 应删除头部")
	}

	// 非法的 operations 只记日志，不改动。
	deps.applyHeaderFilter(RequestFilter{ID: 2, Scope: "header", RuleMode: "advanced", Operations: json.RawMessage(`{`)}, ctx)
	// 未实现的简单动作只记日志。
	deps.applyHeaderFilter(RequestFilter{ID: 3, Scope: "header", Action: "json_path", Target: "x"}, ctx)
}

// body 相的未知动作与非法 operations。
func TestApplyBodyFilterFallbacks(t *testing.T) {
	deps := Deps{}
	body := map[string]any{"messages": []any{}}

	deps.applyBodyFilter(RequestFilter{ID: 1, Scope: "body", Action: "unknown_action", Target: "x"}, body)
	deps.applyBodyFilter(RequestFilter{ID: 2, Scope: "body", RuleMode: "advanced", Operations: json.RawMessage(`{`)}, body)
	// text_replace 的结果不是对象时不动原正文（理论上不会发生，但分支要被走到）。
	deps.applyBodyFilter(RequestFilter{
		ID: 3, Scope: "body", Action: "text_replace", Target: "a",
		Replacement: json.RawMessage(`"b"`), MatchType: "",
	}, body)

	if len(body["messages"].([]any)) != 0 {
		t.Fatal("回退路径不应改变正文结构")
	}
}

// insert 的去重（含按字段去重）与位置回退。
func TestInsertDedupeBranches(t *testing.T) {
	body := map[string]any{"items": []any{map[string]any{"id": "a", "v": float64(1)}}}

	// 整元素去重命中。
	applyBodyOperation(filterOperation{
		Op: "insert", Path: "items", Value: map[string]any{"id": "a", "v": float64(1)},
	}, body)
	if len(body["items"].([]any)) != 1 {
		t.Fatalf("整元素去重应拦住重复插入: %v", body["items"])
	}

	// 按字段去重命中。
	applyBodyOperation(filterOperation{
		Op: "insert", Path: "items", Value: map[string]any{"id": "a", "v": float64(9)},
		Dedupe: &filterDedupe{ByFields: []string{"id"}},
	}, body)
	if len(body["items"].([]any)) != 1 {
		t.Fatalf("按字段去重应拦住重复插入: %v", body["items"])
	}

	// 关闭去重后允许插入，并放在 start。
	disabled := false
	applyBodyOperation(filterOperation{
		Op: "insert", Path: "items", Value: map[string]any{"id": "a", "v": float64(9)},
		Position: "start", Dedupe: &filterDedupe{Enabled: &disabled},
	}, body)
	items := body["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["v"] != float64(9) {
		t.Fatalf("关闭去重后应插入到开头: %v", items)
	}

	// 目标不是数组时新建数组。
	applyBodyOperation(filterOperation{Op: "insert", Path: "fresh", Value: "x"}, body)
	if value, ok := getValueByPath(body, "fresh[0]"); !ok || value != "x" {
		t.Fatalf("目标缺失时应新建数组并插入: %v (ok=%v)", value, ok)
	}

	// 锚点未命中且回退到 end。
	applyBodyOperation(filterOperation{
		Op: "insert", Path: "fresh", Value: "y", Position: "before",
		Anchor: &filterMatcher{Field: "id", Value: "none"},
	}, body)
	if len(body["fresh"].([]any)) != 2 {
		t.Fatalf("锚点未命中应回退到末尾插入: %v", body["fresh"])
	}
}

// merge 的目标缺失或类型不符时新建对象。
func TestMergeCreatesTarget(t *testing.T) {
	body := map[string]any{"scalar": "x"}

	applyBodyOperation(filterOperation{Op: "merge", Path: "fresh", Value: map[string]any{"a": float64(1)}}, body)
	if value, ok := getValueByPath(body, "fresh.a"); !ok || value != float64(1) {
		t.Fatalf("merge 应新建对象: %v (ok=%v)", value, ok)
	}

	applyBodyOperation(filterOperation{Op: "merge", Path: "scalar", Value: map[string]any{"b": float64(2)}}, body)
	if value, ok := getValueByPath(body, "scalar.b"); !ok || value != float64(2) {
		t.Fatalf("merge 应把标量替换成对象: %v (ok=%v)", value, ok)
	}

	// 非对象取值与未知操作都不改正文。
	before := len(body)
	applyBodyOperation(filterOperation{Op: "merge", Path: "scalar", Value: "not-object"}, body)
	applyBodyOperation(filterOperation{Op: "unknown", Path: "x", Value: 1}, body)
	if len(body) != before {
		t.Fatal("非法操作不应改变正文")
	}
}

// remove 的元素匹配分支：目标不是数组时不动。
func TestRemoveWithMatcherOnNonArray(t *testing.T) {
	body := map[string]any{"scalar": "x"}
	applyBodyOperation(filterOperation{
		Op: "remove", Path: "scalar", Matcher: &filterMatcher{Value: "x"},
	}, body)
	if body["scalar"] != "x" {
		t.Fatal("非数组目标不应被元素匹配删除")
	}
}

// 取值与替换的辅助分支。
func TestReplacementAndMatchTypeHelpers(t *testing.T) {
	if got := replacementValue(nil); got != nil {
		t.Fatalf("空 replacement 应为 nil，收到 %v", got)
	}
	if got := replacementValue(json.RawMessage(`"s"`)); got != "s" {
		t.Fatalf("字符串 replacement 应原样返回，收到 %v", got)
	}
	// 非法 JSON 原样当字符串用。
	if got := replacementValue(json.RawMessage(`{`)); got != "{" {
		t.Fatalf("非法 JSON 应原样返回，收到 %v", got)
	}
	if got := normalizeMatchType(""); got != MatchContains {
		t.Fatalf("空匹配类型应归一为 contains，收到 %q", got)
	}
	if got := normalizeMatchType("regex"); got != MatchRegex {
		t.Fatalf("已知匹配类型应原样返回，收到 %q", got)
	}
}

// 正文缝隙不可用时过滤器跳过并留痕，而不是报错。
func TestApplyOneFilterWithoutBody(t *testing.T) {
	deps := Deps{}
	ctx := newContext(t, nil, nil)
	deps.applyOneFilter(ctx, RequestFilter{ID: 1, Scope: "body", Action: "json_path", Target: "a"})

	// header 之外的 scope 直接忽略。
	deps.applyOneFilter(ctx, RequestFilter{ID: 2, Scope: "query", Action: "set", Target: "q"})
}

// 客户端文本归一与端点判定。
func TestNormalizeClientText(t *testing.T) {
	if got := normalizeClientText("Claude_Code-CLI"); got != "claudecodecli" {
		t.Fatalf("归一失败: %q", got)
	}
	if !isCountTokensPath("/v1/messages/count_tokens") {
		t.Fatal("应识别 count_tokens 路径")
	}
	if !isCountTokensPath("/v1/messages/count_tokens/?x=1") {
		t.Fatal("应忽略查询串与尾斜杠")
	}
	if isCountTokensPath("/v1/messages") {
		t.Fatal("普通 messages 路径不应识别为 count_tokens")
	}
	if got := normalizeEndpointPath("/v1/messages/"); got != "/v1/messages" {
		t.Fatalf("尾斜杠应被去掉，收到 %q", got)
	}
	if got := normalizeEndpointPath("/"); got != "/" {
		t.Fatalf("根路径应保持，收到 %q", got)
	}
}

// 模型名也可以来自 Gemini CLI 的包装字段。
func TestRequestedModelFromWrappedBody(t *testing.T) {
	body := map[string]any{"request": map[string]any{"model": "wrapped-model"}}
	factory, _ := bodyFactory(t, body)
	deps := Deps{
		Users: fakeUsers{user: User{ID: 7, IsEnabled: true, AllowedModels: []string{"wrapped-model"}}},
		Body:  factory,
	}
	ctx := newContext(t, nil, body)
	withAuth(ctx, 3, 7, "sk-x")

	response, err := deps.modelStep()(ctx)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if response != nil {
		t.Fatalf("包装字段里的模型应被识别并放行，收到 %d", response.Status)
	}
}

// 未接线的可选缝隙：选路与日志上下文报告缺失但不报错。
func TestOptionalSeamsMissing(t *testing.T) {
	ctx := newContext(t, nil, nil)
	if _, err := (Deps{}).providerStep()(ctx); err != nil {
		t.Fatalf("选路缝隙缺失不应报错: %v", err)
	}
	// 日志器缺失时走默认 logger。
	if (Deps{}).logger() == nil {
		t.Fatal("应返回默认日志器")
	}
	if (Deps{Logger: nil}).runContext(ctx) == nil {
		t.Fatal("应返回背景 context")
	}
}

// 客户端 UA 为空的边界。
func TestMatchClientPatternEmptyUA(t *testing.T) {
	deps := Deps{}
	ctx := newContext(t, nil, nil)
	if deps.matchClientPattern(ctx, "curl", clientSignals{}) {
		t.Fatal("空 UA 不应匹配非内置模式")
	}
	if deps.matchClientPattern(ctx, "claude-code-cli", clientSignals{confirmed: false}) {
		t.Fatal("未确认的 Claude Code 信号不应匹配内置关键字")
	}
	if !deps.matchClientPattern(ctx, ClaudeCodeKeywordPrefix, clientSignals{confirmed: true}) {
		t.Fatal("确认后前缀关键字应匹配")
	}
	// 归一化后为空串的模式不匹配。
	if deps.matchClientPattern(ctx, "-", clientSignals{}) {
		t.Fatal("归一化为空串的模式不应匹配")
	}
}

// 会话步骤在设置读取失败时按保守默认值继续。
func TestSessionStepSettingsFailure(t *testing.T) {
	binder := &fakeBinder{}
	deps := Deps{Settings: fakeSettings{err: errFilterSource}, Sessions: binder}
	ctx := newContext(t, nil, nil)
	withAuth(ctx, 3, 7, "sk-x")

	if _, err := deps.sessionStep()(ctx); err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if ctx.ShouldPersistDebugArtifacts() {
		t.Fatal("设置读取失败时应保持调试工件关闭（保守默认）")
	}
	if len(binder.requests) != 1 || binder.requests[0].AllowRawSession {
		t.Fatalf("设置读取失败时应按不允许原始回退处理: %+v", binder.requests)
	}
}

// 预热抢答在没有日志缝隙时也要正常返回。
func TestWarmupStepWithoutLogSeam(t *testing.T) {
	body := map[string]any{
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type": "text", "text": "warmup",
				"cache_control": map[string]any{"type": "ephemeral"},
			}},
		}},
	}
	factory, _ := bodyFactory(t, body)
	settings := fakeSettings{}
	settings.settings.interceptWarmup = true
	deps := Deps{Body: factory, Settings: settings}

	ctx := newContext(t, nil, body)
	withAuth(ctx, 3, 7, "sk-x")
	response, err := deps.warmupStep()(ctx)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if response == nil || response.Status != 200 {
		t.Fatalf("应抢答 200，收到 %v", response)
	}

	// 未鉴权时不抢答（拿不到密钥无法记录）。
	ctx = newContext(t, nil, body)
	response, err = deps.warmupStep()(ctx)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if response != nil {
		t.Fatalf("未鉴权时不应抢答，收到 %d", response.Status)
	}
}

// 预热判定的其余边界：消息不是对象、内容不是数组、设置缝隙缺失。
func TestWarmupDetectionEdges(t *testing.T) {
	if isWarmupRequest("/v1/messages", map[string]any{"messages": []any{"not-an-object"}}) {
		t.Fatal("消息不是对象时不应判定为热身")
	}
	if isWarmupRequest("/v1/messages", map[string]any{"messages": []any{map[string]any{
		"role": "user", "content": "字符串内容",
	}}}) {
		t.Fatal("content 不是数组时不应判定为热身")
	}
	if isWarmupRequest("/v1/chat/completions", map[string]any{}) {
		t.Fatal("非 messages 端点不应判定为热身")
	}

	// 设置缝隙缺失时直接跳过。
	factory, _ := bodyFactory(t, map[string]any{"messages": []any{}})
	ctx := newContext(t, nil, nil)
	if response, err := (Deps{Body: factory}).warmupStep()(ctx); err != nil || response != nil {
		t.Fatalf("设置缝隙缺失时应跳过，收到 %v / %v", response, err)
	}
}

// 版本检查里客户端展示名参与文案。
func TestVersionCheckerDisplayName(t *testing.T) {
	checker := &fakeVersionChecker{
		client: ClientVersion{ClientType: "vscode", Version: "1.0.0"}, parsed: true,
		needsUpgrade: true, gaVersion: "2.0.0",
	}
	settings := fakeSettings{}
	settings.settings.enableClientVersionCheck = true
	deps := Deps{Settings: settings, Versions: checker}
	ctx := newContext(t, nil, nil)
	withAuth(ctx, 3, 7, "sk-x")

	response, err := deps.versionStep()(ctx)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if response == nil {
		t.Fatal("版本过旧应拦截")
	}
	if !containsSubstring(string(response.Body), "vscode") {
		t.Fatalf("文案应含客户端类型: %s", string(response.Body))
	}
}

// 滚动窗口（没有固定重置时刻）不写 Retry-After 与 X-RateLimit-Reset。
func TestRateLimitStepWithoutRetryAfter(t *testing.T) {
	limiter := &fakeLimiter{block: &RateLimitBlock{
		Status: 402, Message: "太快", ErrorType: "rate_limit_error", LimitType: "daily_quota", Current: 12.5, Limit: 10,
	}}
	ctx := newContext(t, nil, nil)

	response, err := (Deps{RateLimit: limiter}).rateLimitStep()(ctx)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if response == nil || response.Headers.Get("Retry-After") != "" {
		t.Fatalf("无固定重置时刻时不应写 Retry-After: %v", response)
	}
	if response.Headers.Get("X-RateLimit-Reset") != "" {
		t.Fatalf("无固定重置时刻时不应写 X-RateLimit-Reset: %v", response.Headers)
	}
	// 额度类命中的是 402（Node 的 getRateLimitStatusCode：只有 rpm 与并发是 429）。
	if response.Status != 402 {
		t.Fatalf("额度超限应为 402，收到 %d", response.Status)
	}
	if body := string(response.Body); !strings.Contains(body, `"reset_time":null`) {
		t.Fatalf("滚动窗口的 reset_time 应为 null：%s", body)
	}
}

// 选路结果在上下文里的读写不影响并发语义（值拷贝）。
func TestProviderSelectionIsValueCopy(t *testing.T) {
	ctx := newContext(t, nil, nil)
	selection := pctx.ProviderSelection{ProviderID: 1, Name: "甲"}
	ctx.SetProvider(selection)
	selection.Name = "改过了"

	stored, _ := ctx.Provider()
	if stored.Name != "甲" {
		t.Fatalf("上下文应保存值拷贝，收到 %q", stored.Name)
	}
}

// 不可序列化的抢答体必须报错而不是发出坏响应。
func TestJSONResponseRejectsUnserializable(t *testing.T) {
	if _, err := JSONResponse(200, make(chan int)); err == nil {
		t.Fatal("不可序列化的载荷应返回错误")
	}
}
