package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// providers_test_routes_test.go：测试族 6 条的**校验面与信封**（形状由 Node 实机对照得来）。
//
// 覆盖：注册点与路径、strict schema 的逐键报错、providerType 的公开/内部集分流、
// 探针失败恒 200、行动作错误走 Problem 400、unified 成功载荷字段。
//
// 未覆盖（在本 lane 之外，见报告）：by-id 的可见性 404（`providerFindVisible` 已有既有测试）
// 与读库取 key 的路径（需真 PG）。

func callProviderTestRoute(
	t *testing.T,
	handler http.HandlerFunc,
	body string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/providers/test:unified", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler(recorder, request)
	return recorder
}

func validationBodyOf(t *testing.T, recorder *httptest.ResponseRecorder) adminValidationBody {
	t.Helper()
	var body adminValidationBody
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("正文解析失败: %v（%s）", err, recorder.Body.String())
	}
	return body
}

func problemBodyMap(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("正文解析失败: %v（%s）", err, recorder.Body.String())
	}
	return body
}

func TestRegisterProvidersTestRoutesRequiresStore(t *testing.T) {
	router := New(Options{Deps: Deps{Guard: &recordingGuard{}}})
	RegisterProvidersTestRoutes(router, Deps{})
	if got := router.RouteCount(); got != 0 {
		t.Fatalf("缺 Store 时不应注册任何测试路由，实际 %d", got)
	}
}

func TestRegisterProvidersTestRoutesPathsAndOperationIDs(t *testing.T) {
	router := New(Options{Deps: Deps{Guard: &recordingGuard{}}})
	RegisterProvidersTestRoutes(router, Deps{Store: &store.Pools{}})

	want := map[string]string{
		"POST /providers/test:unified":                 "testProviderUnified",
		"POST /providers/{id:[0-9]+}/test":             "testProviderById",
		"POST /providers/test:anthropic-messages":      "testProviderAnthropic",
		"POST /providers/test:openai-chat-completions": "testProviderOpenAIChat",
		"POST /providers/test:openai-responses":        "testProviderOpenAIResponses",
		"POST /providers/test:gemini":                  "testProviderGemini",
	}
	routes := router.RouteList()
	if len(routes) != len(want) {
		t.Fatalf("应注册 %d 条，实际 %d：%+v", len(want), len(routes), routeKeys(router.RouteList()))
	}
	for _, route := range routes {
		key := route.Method + " " + route.Path
		expected, ok := want[key]
		if !ok {
			t.Fatalf("出现未预期的测试路由：%s", key)
		}
		if route.OperationID != expected {
			t.Fatalf("%s 的 operationId 应为 %s，实际 %s", key, expected, route.OperationID)
		}
		if route.Access != AccessAdmin {
			t.Fatalf("%s 应是 admin 档，实际 %v", key, route.Access)
		}
	}
}

func TestUnifiedStrictValidationIssues(t *testing.T) {
	handler := handleTestProviderUnified(Deps{})

	cases := []struct {
		name    string
		body    string
		path    string
		code    string
		message string
	}{
		{"未知键", `{"providerUrl":"https://x.test","apiKey":"k","providerType":"claude","nope":1}`, "nope", "unrecognized_keys", "Unrecognized key: nope"},
		{"缺 providerUrl", `{"apiKey":"k","providerType":"claude"}`, "providerUrl", "invalid_type", "Required"},
		{"providerUrl 非 URL", `{"providerUrl":"nope","apiKey":"k","providerType":"claude"}`, "providerUrl", "invalid_format", "Invalid URL"},
		{"apiKey 空串", `{"providerUrl":"https://x.test","apiKey":"","providerType":"claude"}`, "apiKey", "too_small", "String must contain at least 1 character(s)"},
		{"缺 providerType", `{"providerUrl":"https://x.test","apiKey":"k"}`, "providerType", "invalid_type", "Required"},
		{"providerType 非法", `{"providerUrl":"https://x.test","apiKey":"k","providerType":"nope"}`, "providerType", "invalid_enum_value", "Invalid enum value"},
		{"latencyThresholdMs 过小", `{"providerUrl":"https://x.test","apiKey":"k","providerType":"claude","latencyThresholdMs":0}`, "latencyThresholdMs", "too_small", "Number must be greater than or equal to 1"},
		{"timeoutMs 过小", `{"providerUrl":"https://x.test","apiKey":"k","providerType":"claude","timeoutMs":1}`, "timeoutMs", "too_small", "Number must be greater than or equal to 5000"},
		{"timeoutMs 过大", `{"providerUrl":"https://x.test","apiKey":"k","providerType":"claude","timeoutMs":999999}`, "timeoutMs", "too_big", "Number must be less than or equal to 120000"},
		{"customHeaders 非对象", `{"providerUrl":"https://x.test","apiKey":"k","providerType":"claude","customHeaders":"x"}`, "customHeaders", "invalid_type", "Expected object, received string"},
		{"proxyFallbackToDirect 非布尔", `{"providerUrl":"https://x.test","apiKey":"k","providerType":"claude","proxyFallbackToDirect":"yes"}`, "proxyFallbackToDirect", "invalid_type", "Expected boolean, received string"},
	}

	for _, testCase := range cases {
		recorder := callProviderTestRoute(t, handler, testCase.body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: 应 400，实际 %d（%s）", testCase.name, recorder.Code, recorder.Body.String())
		}
		body := validationBodyOf(t, recorder)
		if body.ErrorCode != "request.validation_failed" || body.Title != "Validation failed" {
			t.Fatalf("%s: 信封不符 %+v", testCase.name, body)
		}
		found := false
		for _, issue := range body.InvalidParams {
			path, _ := issue.Path[0].(string)
			if path == testCase.path && issue.Code == testCase.code && issue.Message == testCase.message {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: 缺期望的校验项 (%s,%s,%q)，实际 %+v",
				testCase.name, testCase.path, testCase.code, testCase.message, body.InvalidParams)
		}
	}
}

func TestUnifiedProviderTypeHiddenOnlyWithDashboardCompat(t *testing.T) {
	handler := handleTestProviderUnified(Deps{})
	body := `{"providerUrl":"https://x.test","apiKey":"k","providerType":"claude-auth"}`

	recorder := callProviderTestRoute(t, handler, body)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("无兼容头时隐藏类型应 400，实际 %d", recorder.Code)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/v1/providers/test:unified", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CCH-Dashboard-Compat", "1")
	recorder = httptest.NewRecorder()
	handler(recorder, request)
	// 兼容头下 schema 通过；随后因 URL 不安全/上游不可达走 200+success:false（不是 400）。
	if recorder.Code != http.StatusOK {
		t.Fatalf("兼容头下应过 schema（可能 200+success:false），实际 %d（%s）", recorder.Code, recorder.Body.String())
	}
}

func TestUnifiedActionErrorsAreProblem400(t *testing.T) {
	handler := handleTestProviderUnified(Deps{})

	cases := []struct {
		name string
		body string
	}{
		{"自定义正文非法", `{"providerUrl":"https://x.test","apiKey":"k","providerType":"claude","customPayload":"{not json"}`},
		{"preset 不存在", `{"providerUrl":"https://x.test","apiKey":"k","providerType":"claude","preset":"nope"}`},
		{"customHeaders 受保护名", `{"providerUrl":"https://x.test","apiKey":"k","providerType":"claude","customHeaders":{"authorization":"x"}}`},
	}
	for _, testCase := range cases {
		recorder := callProviderTestRoute(t, handler, testCase.body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: 应 400，实际 %d（%s）", testCase.name, recorder.Code, recorder.Body.String())
		}
		body := problemBodyMap(t, recorder)
		if body["errorCode"] != "provider.action_failed" {
			t.Fatalf("%s: errorCode 应为 provider.action_failed，实际 %v", testCase.name, body["errorCode"])
		}
	}
}

func TestUnifiedSuccessAgainstFakeUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(`{"model":"claude-haiku-4-5-20251001","content":[{"type":"text","text":"pong"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	handler := handleTestProviderUnified(Deps{})
	recorder := callProviderTestRoute(t, handler,
		`{"providerUrl":"`+upstream.URL+`","apiKey":"sk-test","providerType":"claude","successContains":"pong"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d（%s）", recorder.Code, recorder.Body.String())
	}
	body := problemBodyMap(t, recorder)
	if body["success"] != true || body["status"] != "green" || body["subStatus"] != "success" {
		t.Fatalf("载荷不符: %v", body)
	}
	if body["message"] != "供应商 可用: 所有检查通过" {
		t.Fatalf("文案不符: %v", body["message"])
	}
	details, ok := body["validationDetails"].(map[string]any)
	if !ok {
		t.Fatalf("缺 validationDetails: %v", body)
	}
	if details["httpPassed"] != true || details["contentPassed"] != true || details["contentTarget"] != "pong" {
		t.Fatalf("校验明细不符: %v", details)
	}
	// 未用代理时不应出现 usedProxy（Node 的统一载荷本就不含该键）。
	if _, present := body["usedProxy"]; present {
		t.Fatalf("统一载荷不应含 usedProxy: %v", body)
	}
}

func TestTypedEndpointsEnvelopes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/messages":
			_, _ = writer.Write([]byte(`{"model":"claude-sonnet-4-6","content":[{"type":"text","text":"hi"}]}`))
		case "/v1/chat/completions":
			_, _ = writer.Write([]byte(`{"model":"gpt-5.5","choices":[{"message":{"content":"hi"}}]}`))
		case "/v1/responses":
			_, _ = writer.Write([]byte(`{"model":"gpt-5.5","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}]}`))
		default:
			_, _ = writer.Write([]byte(`{"modelVersion":"gemini-2.5-pro","candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`))
		}
	}))
	defer upstream.Close()

	cases := []struct {
		name    string
		handler http.HandlerFunc
		message string
	}{
		{"anthropic", handleTestProviderAnthropic(Deps{}), "Anthropic Messages API 测试成功"},
		{"openai-chat", handleTestProviderOpenAIChat(Deps{}), "OpenAI Chat Completions API 测试成功"},
		{"openai-responses", handleTestProviderOpenAIResponses(Deps{}), "OpenAI Responses API 测试成功"},
		{"gemini", handleTestProviderGemini(Deps{}), "Gemini API 测试成功"},
	}
	for _, testCase := range cases {
		recorder := callProviderTestRoute(t, testCase.handler,
			`{"providerUrl":"`+upstream.URL+`","apiKey":"sk-test"}`)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: 应 200，实际 %d（%s）", testCase.name, recorder.Code, recorder.Body.String())
		}
		body := problemBodyMap(t, recorder)
		if body["success"] != true || body["message"] != testCase.message {
			t.Fatalf("%s: 载荷不符 %v", testCase.name, body)
		}
		details, _ := body["details"].(map[string]any)
		if _, present := details["responseTime"]; !present {
			t.Fatalf("%s: details 应含 responseTime: %v", testCase.name, details)
		}
		if details["content"] != "hi" {
			t.Fatalf("%s: content 应为 hi，实际 %v", testCase.name, details["content"])
		}
	}
}

func TestTypedEndpointsStrictValidationAndTimeoutRange(t *testing.T) {
	handler := handleTestProviderAnthropic(Deps{})

	recorder := callProviderTestRoute(t, handler, `{"providerUrl":"https://x.test"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("缺 apiKey 应 400，实际 %d", recorder.Code)
	}
	body := validationBodyOf(t, recorder)
	if len(body.InvalidParams) == 0 {
		t.Fatalf("应有校验项: %+v", body)
	}

	// 未知键：定型 schema 不含 providerType（差异点：四条定型端点不收 providerType）。
	recorder = callProviderTestRoute(t, handler,
		`{"providerUrl":"https://x.test","apiKey":"k","providerType":"claude"}`)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("定型 schema 应拒绝 providerType 键，实际 %d（%s）", recorder.Code, recorder.Body.String())
	}
	body = validationBodyOf(t, recorder)
	found := false
	for _, issue := range body.InvalidParams {
		if path, _ := issue.Path[0].(string); path == "providerType" && issue.Code == "unrecognized_keys" {
			found = true
		}
	}
	if !found {
		t.Fatalf("应报 providerType 为未知键，实际 %+v", body.InvalidParams)
	}

	// gemini 的超时范围是「200 + success:false + 秒数文案」（不是 400）。
	recorder = callProviderTestRoute(t, handleTestProviderGemini(Deps{}),
		`{"providerUrl":"https://x.test","apiKey":"k","timeoutMs":1000}`)
	// 注意：timeoutMs=1000 会先被 schema 的 min(5000) 拦下 → 400。范围校验只在 5000..120000 内无效，
	// 故这里用 schema 允许但 Gemini 自己再判的场景不可构造；退化为断言 400（schema 兜底）。
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("timeoutMs=1000 应被 schema 拦为 400，实际 %d（%s）", recorder.Code, recorder.Body.String())
	}
}

func TestByIDBodyStrictDecode(t *testing.T) {
	model, issues := decodeProviderTestByIDBody(map[string]json.RawMessage{
		"model": json.RawMessage(`"gpt-5.5"`),
	})
	if len(issues) != 0 || model != "gpt-5.5" {
		t.Fatalf("合法体应通过: model=%q issues=%+v", model, issues)
	}

	_, issues = decodeProviderTestByIDBody(map[string]json.RawMessage{
		"model": json.RawMessage(`"  "`),
	})
	if len(issues) != 1 || issues[0].Code != "too_small" {
		t.Fatalf("空白 model 应报 too_small: %+v", issues)
	}

	_, issues = decodeProviderTestByIDBody(map[string]json.RawMessage{
		"providerUrl": json.RawMessage(`"https://x.test"`),
	})
	if len(issues) != 1 || issues[0].Code != "unrecognized_keys" {
		t.Fatalf("未知键应报 unrecognized_keys: %+v", issues)
	}

	// 空对象合法（model 可选）。
	model, issues = decodeProviderTestByIDBody(map[string]json.RawMessage{})
	if len(issues) != 0 || model != "" {
		t.Fatalf("空对象应通过且 model 为空: %+v", issues)
	}
}
