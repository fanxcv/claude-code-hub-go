package providertest

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 本文件钉住 engine.go / typed.go 的执行语义（假上游，禁打外网）。
//
// 覆盖点与 Node 的唯一真源对应：
//   - 计划生成三分支（自定义正文 / 指定 preset / 候选降序）与两处报错文案；
//   - 逐计划尝试与 shouldRetryWithNextTemplate 的重试条件；
//   - versionless OpenAI URL 回退（仅 codex/openai-compatible + 400 + 标记串，且只重试一次）；
//   - 错误归类（超时/拒连/重置）；
//   - 统一 payload 的字段集与 `testedAt` 形状；
//   - 定型路径的代表性信封（成功 / 非 2xx / 非法 JSON / 代理非法 / URL 非法 / gemini 两次尝试）。

func fakeDoerFactory(server *httptest.Server) ClientFactory {
	return func(proxyURL string, timeout time.Duration) (Doer, error) {
		if proxyURL != "" {
			// 测试里不真连代理：把「用了代理」这一事实留给调用方断言。
			return &recordingDoer{target: server.URL, usedProxy: true}, nil
		}
		return &recordingDoer{target: server.URL}, nil
	}
}

type recordingDoer struct {
	target    string
	usedProxy bool
	lastURL   string
	lastBody  string
}

func (d *recordingDoer) Do(request *http.Request) (*http.Response, error) {
	d.lastURL = request.URL.String()
	var raw []byte
	if request.Body != nil {
		body, err := io.ReadAll(request.Body)
		_ = request.Body.Close()
		if err != nil {
			return nil, err
		}
		raw = body
		d.lastBody = string(raw)
	}
	rewritten := request.Clone(request.Context())
	rewritten.URL.Scheme = "http"
	rewritten.URL.Host = strings.TrimPrefix(d.target, "http://")
	rewritten.RequestURI = ""
	// 把刚读掉的正文装回去（否则会以 ContentLength>0 + Body 空 发出，被 http 客户端拒绝）。
	rewritten.Body = io.NopCloser(bytes.NewReader(raw))
	rewritten.ContentLength = int64(len(raw))
	return http.DefaultClient.Do(rewritten)
}

func TestExecuteProviderTestUnifiedSuccessPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"model":"claude-haiku-4-5-20251001","content":[{"type":"text","text":"pong"}],"usage":{"input_tokens":1,"output_tokens":2}}`))
	}))
	defer server.Close()

	config := Config{
		ProviderURL:     server.URL,
		APIKey:          "sk-test",
		ProviderType:    TypeClaude,
		SuccessContains: "pong",
	}
	result, err := ExecuteProviderTest(context.Background(), config, fakeDoerFactory(server))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !result.Success || result.Status != StatusGreen || result.SubStatus != SubStatusSuccess {
		t.Fatalf("应绿且 success，实际 status=%s sub=%s", result.Status, result.SubStatus)
	}
	if result.Validation.HTTPPassed != true || result.Validation.ContentPassed != true {
		t.Fatalf("校验明细应双通过，实际 %+v", result.Validation)
	}

	payload := BuildUnifiedTestPayload(result)
	if payload["message"] != "供应商 可用: 所有检查通过" {
		t.Fatalf("文案不符: %v", payload["message"])
	}
	details, ok := payload["validationDetails"].(map[string]any)
	if !ok {
		t.Fatalf("缺 validationDetails: %v", payload)
	}
	if details["contentTarget"] != "pong" {
		t.Fatalf("contentTarget 不符: %v", details["contentTarget"])
	}
	testedAt, _ := payload["testedAt"].(string)
	if len(testedAt) != 24 || !strings.HasSuffix(testedAt, "Z") {
		t.Fatalf("testedAt 应是 ISO 毫秒串，实际 %q", testedAt)
	}
	if _, present := payload["streamInfo"]; present {
		t.Fatalf("非流式不应出现 streamInfo: %v", payload)
	}
}

func TestExecuteProviderTestRetriesNextTemplateOnClientError(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = writer.Write([]byte(`{"error":{"message":"bad"}}`))
			return
		}
		_, _ = writer.Write([]byte(`{"model":"claude-haiku-4-5-20251001","content":[{"type":"text","text":"pong"}]}`))
	}))
	defer server.Close()

	config := Config{
		ProviderURL:     server.URL,
		APIKey:          "sk-test",
		ProviderType:    TypeClaude,
		SuccessContains: "pong",
	}
	result, err := ExecuteProviderTest(context.Background(), config, fakeDoerFactory(server))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !result.Success {
		t.Fatalf("第二个计划应成功，实际 sub=%s message=%s", result.SubStatus, result.ErrorMessage)
	}
	if atomic.LoadInt32(&calls) < 2 {
		t.Fatalf("应发生重试，实际请求数 %d", calls)
	}
}

func TestExecuteProviderTestVersionlessFallbackOnlyForOpenAiStyle(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.Path)
		if request.URL.Path == "/v1/responses" {
			writer.WriteHeader(http.StatusBadRequest)
			_, _ = writer.Write([]byte(`{"error":{"message":"Invalid URL (POST /v1/responses)"}}`))
			return
		}
		_, _ = writer.Write([]byte(`{"model":"gpt-5.5","output":[{"type":"message","content":[{"type":"output_text","text":"pong"}]}]}`))
	}))
	defer server.Close()

	config := Config{
		ProviderURL:     server.URL,
		APIKey:          "sk-test",
		ProviderType:    TypeCodex,
		SuccessContains: "pong",
	}
	result, err := ExecuteProviderTest(context.Background(), config, fakeDoerFactory(server))
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if !result.Success {
		t.Fatalf("versionless 回退后应成功，实际 sub=%s", result.SubStatus)
	}
	if len(paths) < 2 || paths[1] != "/responses" {
		t.Fatalf("应回退到 /responses，实际路径序列 %v", paths)
	}

	// 反证：同样的 400 + 标记串，claude 类型**不得**触发 versionless 回退。
	paths = nil
	claudeConfig := config
	claudeConfig.ProviderType = TypeClaude
	_, _ = ExecuteProviderTest(context.Background(), claudeConfig, fakeDoerFactory(server))
	for _, path := range paths {
		if path == "/responses" {
			t.Fatalf("claude 类型不应回退到 versionless 路径，实际路径序列 %v", paths)
		}
	}
}

func TestExecuteProviderTestClassifiesConnectionFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {}))
	server.Close() // 立即关闭：制造拒连

	config := Config{
		ProviderURL:  server.URL,
		APIKey:       "sk-test",
		ProviderType: TypeClaude,
	}
	result, err := ExecuteProviderTest(context.Background(), config, nil)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if result.Success || result.Status != StatusRed {
		t.Fatalf("应红，实际 %s", result.Status)
	}
	if result.SubStatus != SubStatusNetworkError {
		t.Fatalf("应是网络类子状态，实际 %s", result.SubStatus)
	}
	if result.HTTPStatusCode != nil {
		t.Fatalf("没有响应时不应有 httpStatusCode: %v", *result.HTTPStatusCode)
	}
}

func TestBuildAttemptPlansErrorMessages(t *testing.T) {
	if _, err := BuildAttemptPlans(Config{
		ProviderURL: "https://example.com", APIKey: "k", ProviderType: TypeClaude, CustomPayload: "{not json",
	}); err == nil || err.Error() != "Invalid custom payload JSON" {
		t.Fatalf("自定义正文非法应报固定文案，实际 %v", err)
	}
	if _, err := BuildAttemptPlans(Config{
		ProviderURL: "https://example.com", APIKey: "k", ProviderType: TypeClaude, Preset: "nope",
	}); err == nil || err.Error() != "Preset not found: nope" {
		t.Fatalf("preset 不存在应报固定文案，实际 %v", err)
	}
}

func TestExecuteProviderAPITypedEnvelopes(t *testing.T) {
	// 成功：anthropic 形状（content[] 文本 + model/usage）。
	success := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(`{"model":"claude-sonnet-4-6","content":[{"type":"text","text":"Hello there"}],"usage":{"input_tokens":3,"output_tokens":5}}`))
	}))
	defer success.Close()

	result := ExecuteProviderAPITest(context.Background(), TypedArgs{
		ProviderURL: success.URL, APIKey: "sk-test",
	}, TypedConfigAnthropicMessages(), fakeDoerFactory(success))
	if ok, _ := result["success"].(bool); !ok {
		t.Fatalf("应成功: %v", result)
	}
	if result["message"] != "Anthropic Messages API 测试成功" {
		t.Fatalf("文案不符: %v", result["message"])
	}
	details, _ := result["details"].(map[string]any)
	if details["model"] != "claude-sonnet-4-6" || details["content"] != "Hello there" {
		t.Fatalf("details 不符: %v", details)
	}
	if _, present := details["responseTime"]; !present {
		t.Fatalf("details 应含 responseTime: %v", details)
	}

	// 非 2xx：message = `API 返回错误: HTTP 500`，details.error 取错误 JSON 的可读消息。
	bad := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
		_, _ = writer.Write([]byte(`{"error":{"message":"upstream exploded"}}`))
	}))
	defer bad.Close()
	result = ExecuteProviderAPITest(context.Background(), TypedArgs{
		ProviderURL: bad.URL, APIKey: "sk-test",
	}, TypedConfigAnthropicMessages(), fakeDoerFactory(bad))
	if result["message"] != "API 返回错误: HTTP 500" {
		t.Fatalf("文案不符: %v", result["message"])
	}
	details, _ = result["details"].(map[string]any)
	if details["error"] != "upstream exploded" {
		t.Fatalf("错误详情应取可读消息: %v", details["error"])
	}

	// 非法 JSON：`响应格式无效: 无法解析 JSON`。
	notJSON := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte("not-json"))
	}))
	defer notJSON.Close()
	result = ExecuteProviderAPITest(context.Background(), TypedArgs{
		ProviderURL: notJSON.URL, APIKey: "sk-test",
	}, TypedConfigAnthropicMessages(), fakeDoerFactory(notJSON))
	if result["message"] != "响应格式无效: 无法解析 JSON" {
		t.Fatalf("文案不符: %v", result["message"])
	}
}

func TestExecuteProviderAPITypedValidationFailuresAre200(t *testing.T) {
	// 代理地址非法 → 200 + 固定文案（不请求上游）。
	result := ExecuteProviderAPITest(context.Background(), TypedArgs{
		ProviderURL: "https://example.com", APIKey: "sk-test", ProxyURL: "ftp://nope",
	}, TypedConfigAnthropicMessages(), nil)
	if result["success"] != false || result["message"] != "代理地址格式无效" {
		t.Fatalf("代理非法应 200+固定文案: %v", result)
	}
	details, _ := result["details"].(map[string]any)
	if details["error"] != "支持格式: http://, https://, socks5://, socks4://" {
		t.Fatalf("代理非法提示不符: %v", details["error"])
	}

	// 供应商 URL 非法 → 200 + 校验消息。
	result = ExecuteProviderAPITest(context.Background(), TypedArgs{
		ProviderURL: "ftp://example.com", APIKey: "sk-test",
	}, TypedConfigAnthropicMessages(), nil)
	if result["success"] != false {
		t.Fatalf("URL 非法应 200+success:false: %v", result)
	}
	if message, _ := result["message"].(string); message == "" {
		t.Fatalf("应有校验消息: %v", result)
	}
}

func TestExecuteProviderAPITypedGeminiURLParamFallback(t *testing.T) {
	var keys []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		keys = append(keys, request.URL.Query().Get("key"))
		if request.URL.Query().Get("key") == "" {
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = writer.Write([]byte(`{"error":{"message":"API key not valid"}}`))
			return
		}
		_, _ = writer.Write([]byte(`{"modelVersion":"gemini-2.5-pro","candidates":[{"content":{"parts":[{"text":"pong"}]}}]}`))
	}))
	defer server.Close()

	args := TypedArgs{ProviderURL: server.URL, APIKey: "sk-gemini"}
	first := ExecuteProviderAPITest(context.Background(), args, TypedConfigGemini(false), fakeDoerFactory(server))
	if ok, _ := first["success"].(bool); ok {
		t.Fatalf("首次应失败: %v", first)
	}
	if !GeminiAuthErrorFromResult(first) {
		t.Fatalf("应判为认证失败: %v", first["message"])
	}

	second := ExecuteProviderAPITest(context.Background(), args, TypedConfigGeminiURLParam(), fakeDoerFactory(server))
	if ok, _ := second["success"].(bool); !ok {
		t.Fatalf("URL 参数二次尝试应成功: %v", second)
	}
	if second["message"] != "Gemini API 测试成功 (URL 认证)" {
		t.Fatalf("文案不符: %v", second["message"])
	}
	if len(keys) != 2 || keys[0] != "" || keys[1] != "sk-gemini" {
		t.Fatalf("key 出现方式不符: %v", keys)
	}
}

func TestExtractFirstTextSnippetFourShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"anthropic", `{"content":[{"type":"text","text":"A"}]}`, "A"},
		{"openai-chat", `{"choices":[{"message":{"content":"B"}}]}`, "B"},
		{"openai-responses", `{"output":[{"type":"message","content":[{"type":"output_text","text":"C"}]}]}`, "C"},
		{"gemini", `{"candidates":[{"content":{"parts":[{"text":"D"}]}}]}`, "D"},
		{"none", `{"foo":"bar"}`, ""},
	}
	for _, testCase := range cases {
		if got := ExtractFirstTextSnippet(testCase.body); got != testCase.want {
			t.Fatalf("%s: 期望 %q 实际 %q", testCase.name, testCase.want, got)
		}
	}

	long := strings.Repeat("x", 600)
	body, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": long}}}})
	if got := ExtractFirstTextSnippet(string(body)); len(got) != 500 {
		t.Fatalf("应截断到 500，实际 %d", len(got))
	}
}

func TestTypedRawExtractReadsTopLevelFields(t *testing.T) {
	extract := TypedRawExtract(false)
	got := extract(`{"model":"m-1","usage":{"inputTokens":2,"outputTokens":3},"choices":[{"message":{"content":"hi"}}]}`, ParsedResponse{})
	if got.Model != "m-1" || got.Content != "hi" {
		t.Fatalf("顶层字段读取不符: %+v", got)
	}
	if got.Usage == nil || got.Usage.InputTokens != 2 || got.Usage.OutputTokens != 3 {
		t.Fatalf("usage 解码不符: %+v", got.Usage)
	}

	gemini := TypedRawExtract(true)
	got = gemini(`{"modelVersion":"g-1","usageMetadata":{"inputTokens":1,"outputTokens":1},"candidates":[{"content":{"parts":[{"text":"pong"}]}}]}`, ParsedResponse{})
	if got.Model != "g-1" || got.Content != "pong" {
		t.Fatalf("gemini 形状不符: %+v", got)
	}
}
