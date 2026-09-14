package guard

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// 抢答体形状是跨语言契约：客户端按字段名与 code 做分支，改一处就是兼容性变更。
func TestBuildErrorPayloadShape(t *testing.T) {
	response := BuildError(401, "认证失败", "invalid_api_key")

	if response.Status != 401 {
		t.Fatalf("状态码应为 401，收到 %d", response.Status)
	}
	if got := response.Headers.Get("content-type"); got != "application/json; charset=utf-8" {
		t.Fatalf("content-type 应为 application/json; charset=utf-8，收到 %q", got)
	}

	payload := decodeErrorBody(t, response)
	errorObject, ok := payload["error"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 error 对象: %s", string(response.Body))
	}
	if errorObject["message"] != "认证失败" {
		t.Errorf("message 应为认证失败，收到 %v", errorObject["message"])
	}
	if errorObject["type"] != "invalid_api_key" {
		t.Errorf("type 应为 invalid_api_key，收到 %v", errorObject["type"])
	}
	// type 不是 api_error 时 code 直接等于 type。
	if errorObject["code"] != "invalid_api_key" {
		t.Errorf("code 应为 invalid_api_key，收到 %v", errorObject["code"])
	}
	if _, exists := payload["request_id"]; exists {
		t.Errorf("未传 request_id 时不应出现该字段")
	}
}

func TestBuildErrorDefaultsTypeByStatus(t *testing.T) {
	cases := []struct {
		status   int
		wantType string
		wantCode string
	}{
		{status: 400, wantType: "invalid_request_error", wantCode: "invalid_request_error"},
		{status: 401, wantType: "authentication_error", wantCode: "authentication_error"},
		{status: 402, wantType: "payment_required_error", wantCode: "payment_required_error"},
		{status: 403, wantType: "permission_error", wantCode: "permission_error"},
		{status: 404, wantType: "not_found_error", wantCode: "not_found_error"},
		{status: 429, wantType: "rate_limit_error", wantCode: "rate_limit_error"},
		{status: 500, wantType: "internal_server_error", wantCode: "internal_server_error"},
		{status: 502, wantType: "bad_gateway_error", wantCode: "bad_gateway_error"},
		{status: 503, wantType: "service_unavailable_error", wantCode: "service_unavailable_error"},
		{status: 504, wantType: "gateway_timeout_error", wantCode: "gateway_timeout_error"},
		{status: 418, wantType: "api_error", wantCode: "http_418"},
	}

	for _, testCase := range cases {
		response := BuildError(testCase.status, "文案", "")
		if got := errorField(t, response, "type"); got != testCase.wantType {
			t.Errorf("状态码 %d 的 type 应为 %s，收到 %s", testCase.status, testCase.wantType, got)
		}
		if got := errorField(t, response, "code"); got != testCase.wantCode {
			t.Errorf("状态码 %d 的 code 应为 %s，收到 %s", testCase.status, testCase.wantCode, got)
		}
	}
}

func TestBuildErrorWithDetailsAndRequestID(t *testing.T) {
	response := BuildErrorWithDetails(400, "文案", "invalid_request_error", map[string]any{
		"field": "messages",
	}, "req_1")

	payload := decodeErrorBody(t, response)
	errorObject := payload["error"].(map[string]any)
	details, ok := errorObject["details"].(map[string]any)
	if !ok || details["field"] != "messages" {
		t.Fatalf("details 未按预期序列化: %s", string(response.Body))
	}
	if payload["request_id"] != "req_1" {
		t.Errorf("request_id 应为 req_1，收到 %v", payload["request_id"])
	}
}

// details 里塞进不可序列化的值时，必须降级为不带 details 的同一形状，而不是发出坏响应。
func TestBuildErrorUnserializableDetailsFallsBack(t *testing.T) {
	response := BuildErrorWithDetails(400, "文案", "invalid_request_error", map[string]any{
		"channel": make(chan int),
	}, "")

	if response.Status != 400 {
		t.Fatalf("状态码应为 400，收到 %d", response.Status)
	}
	payload := decodeErrorBody(t, response)
	errorObject := payload["error"].(map[string]any)
	if errorObject["message"] != "文案" {
		t.Errorf("降级后仍应保留 message，收到 %v", errorObject["message"])
	}
	if _, exists := errorObject["details"]; exists {
		t.Errorf("降级后不应带 details")
	}
}

// 链的顺序即契约：预设顺序由 guard_test.go 断言，这里断言执行器严格按顺序跑并短路。
func TestChainRunsInOrderAndShortCircuits(t *testing.T) {
	var visited []string
	index := StepIndex{
		StepAuth: func(*pctx.Context) (*Response, error) {
			visited = append(visited, "auth")
			return nil, nil
		},
		StepSensitive: func(*pctx.Context) (*Response, error) {
			visited = append(visited, "sensitive")
			return BuildError(400, "命中敏感词", ""), nil
		},
		StepClient: func(*pctx.Context) (*Response, error) {
			visited = append(visited, "client")
			return nil, nil
		},
	}

	chain, err := Build(Pipeline{Name: "测试链", Steps: []StepKey{StepAuth, StepSensitive, StepClient}}, index)
	if err != nil {
		t.Fatalf("组装链条失败: %v", err)
	}

	response, err := chain.Run(newContext(t, nil, nil))
	if err != nil {
		t.Fatalf("执行链条失败: %v", err)
	}
	if response == nil || response.Status != 400 {
		t.Fatalf("应在 sensitive 处早退并返回 400，收到 %v", response)
	}
	if strings.Join(visited, ",") != "auth,sensitive" {
		t.Fatalf("执行顺序应为 auth,sensitive，收到 %v", visited)
	}
}

func TestChainPropagatesStepError(t *testing.T) {
	index := StepIndex{
		StepAuth: func(*pctx.Context) (*Response, error) {
			return nil, ErrBodyUnavailable
		},
	}
	chain, err := Build(Pipeline{Name: "测试链", Steps: []StepKey{StepAuth}}, index)
	if err != nil {
		t.Fatalf("组装链条失败: %v", err)
	}
	if _, err := chain.Run(newContext(t, nil, nil)); err == nil {
		t.Fatal("步骤返回错误时链条必须上报，而不是静默放行")
	}
}

// 预设里出现没有实现的步骤键时必须报错：静默跳过等于悄悄丢一道闸。
func TestBuildRejectsMissingStep(t *testing.T) {
	if _, err := Build(ChatPipeline, StepIndex{}); err == nil {
		t.Fatal("缺步骤实现时应返回错误")
	}
}

// 全部预设都能被完整装配：任何一个键漏实现都会在这里暴露。
func TestAllPresetsAssemble(t *testing.T) {
	deps := Deps{}
	for _, pipeline := range []Pipeline{
		ChatPipeline, RawPassthroughPipeline, RawSafeSessionPipeline, CountTokensPipeline,
	} {
		chain, err := deps.FromPipeline(pipeline)
		if err != nil {
			t.Fatalf("预设 %s 组装失败: %v", pipeline.Name, err)
		}
		if len(chain.Steps) != len(pipeline.Steps) {
			t.Fatalf("预设 %s 的步数不一致: %d vs %d", pipeline.Name, len(chain.Steps), len(pipeline.Steps))
		}
	}
}

// 端点预设与请求类型的映射与 Node 侧 fromEndpointPolicy / fromRequestType 一致。
func TestPipelineSelection(t *testing.T) {
	deps := Deps{}

	chain, err := deps.FromEndpointPolicy(PresetRawPassthrough, false)
	if err != nil {
		t.Fatalf("组装失败: %v", err)
	}
	if chain.Name != RawPassthroughPipeline.Name {
		t.Errorf("raw_passthrough 且未开回退时应走 RAW_PASSTHROUGH_PIPELINE，收到 %s", chain.Name)
	}

	chain, err = deps.FromEndpointPolicy(PresetRawPassthrough, true)
	if err != nil {
		t.Fatalf("组装失败: %v", err)
	}
	if chain.Name != RawSafeSessionPipeline.Name {
		t.Errorf("raw_passthrough 且开回退时应走 RAW_SAFE_SESSION_PIPELINE，收到 %s", chain.Name)
	}

	chain, err = deps.FromEndpointPolicy(PresetChat, false)
	if err != nil {
		t.Fatalf("组装失败: %v", err)
	}
	if chain.Name != ChatPipeline.Name {
		t.Errorf("默认预设应走 CHAT_PIPELINE，收到 %s", chain.Name)
	}

	chain, err = deps.FromRequestType(RequestTypeCountTokens)
	if err != nil {
		t.Fatalf("组装失败: %v", err)
	}
	if chain.Name != CountTokensPipeline.Name {
		t.Errorf("count_tokens 应走安全会话链，收到 %s", chain.Name)
	}
}

// 除认证外的缝隙全缺时链条必须跑得完：过渡期必然存在部分接线的中间态，
// 缺缝隙的步骤按各守卫的 fail-open 语义跳过而不是拦截。
func TestChainRunsWithOnlyAuthWired(t *testing.T) {
	auth, bodyAccess := bodyFactory(t, map[string]any{"messages": []any{}})
	_ = bodyAccess
	deps := Deps{
		Auth: &fakeAuth{resolution: AuthResolution{
			User: User{ID: 7, IsEnabled: true},
			Key:  Key{ID: 3, Name: "默认密钥"},
		}},
		Body: auth,
	}

	ctx := newContext(t, map[string]string{"x-api-key": "sk-x"}, map[string]any{"messages": []any{}})
	chain, err := deps.FromPipeline(ChatPipeline)
	if err != nil {
		t.Fatalf("组装失败: %v", err)
	}
	response, err := chain.Run(ctx)
	if err != nil {
		t.Fatalf("未接线的步骤不应报错，收到 %v", err)
	}
	if response != nil {
		t.Fatalf("未接线的步骤不应产生抢答响应，收到状态码 %d", response.Status)
	}
}

// 认证缝隙缺失是硬缺口：链条必须上报错误，而不是把请求放行。
func TestChainFailsClosedWithoutAuthSeam(t *testing.T) {
	ctx := newContext(t, map[string]string{"x-api-key": "sk-x"}, nil)
	chain, err := Deps{}.FromPipeline(ChatPipeline)
	if err != nil {
		t.Fatalf("组装失败: %v", err)
	}
	response, err := chain.Run(ctx)
	if err == nil {
		t.Fatalf("认证缝隙缺失时应上报错误，收到响应 %v", response)
	}
}

// JSONResponse 用于 probe 之外的抢答路径，形状必须稳定。
func TestJSONResponse(t *testing.T) {
	response, err := JSONResponse(200, probePayload{InputTokens: 0})
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if string(response.Body) != `{"input_tokens":0}` {
		t.Fatalf("探测响应体应为 {\"input_tokens\":0}，收到 %s", string(response.Body))
	}
}

// WithHeader 必须返回副本：原响应被后续步骤或调用方共享，就地改写会串味。
func TestWithHeaderDoesNotMutateOriginal(t *testing.T) {
	original := BuildError(429, "太频繁", "rate_limit_error")
	updated := original.WithHeader("Retry-After", "30")

	if original.Headers.Get("Retry-After") != "" {
		t.Fatal("原响应不应被改动")
	}
	if updated.Headers.Get("Retry-After") != "30" {
		t.Fatalf("副本应带 Retry-After，收到 %q", updated.Headers.Get("Retry-After"))
	}
	if updated.Status != original.Status || string(updated.Body) != string(original.Body) {
		t.Fatal("副本的状态码与正文应与原响应一致")
	}
}

// 错误体里的 code 必须可被 JSON 解析为字符串，避免 itoa 出偏差。
func TestErrorCodeForHTTPStatusFallback(t *testing.T) {
	response := BuildError(599, "文案", "")
	var payload map[string]any
	if err := json.Unmarshal(response.Body, &payload); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	code := payload["error"].(map[string]any)["code"]
	if code != "http_599" {
		t.Fatalf("未登记状态码的 code 应为 http_599，收到 %v", code)
	}
}
