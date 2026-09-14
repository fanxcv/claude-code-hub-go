package providertest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
)

// upstream_models_test.go：全部用本机假上游，禁打外网。

func fakeUpstream(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func newTestClient(t *testing.T) *http.Client {
	t.Helper()
	client, err := NewUpstreamClient(ProxyConfig{}, 5*time.Second)
	if err != nil {
		t.Fatalf("建客户端失败: %v", err)
	}
	return client
}

func TestFetchUpstreamModelsOpenAIStyle(t *testing.T) {
	for _, providerType := range []ProviderType{TypeCodex, TypeOpenAICompatible} {
		server := fakeUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
			if request.URL.Path != "/v1/models" {
				t.Errorf("路径应为 /v1/models，实际 %s", request.URL.Path)
			}
			if got := request.Header.Get("Authorization"); got != "Bearer sk-test" {
				t.Errorf("应发 Bearer 认证，实际 %q", got)
			}
			_, _ = writer.Write([]byte(`{"object":"list","data":[{"id":"b-model"},{"id":"a-model"},{"id":"c-model"}]}`))
		})
		models, err := FetchUpstreamModels(context.Background(), newTestClient(t), providerType, server.URL+"/", "sk-test")
		if err != nil {
			t.Fatalf("%s: 应成功，实际 %v", providerType, err)
		}
		want := []string{"a-model", "b-model", "c-model"}
		if !reflect.DeepEqual(models, want) {
			t.Errorf("%s: 模型列表应为 %v（升序），实际 %v", providerType, want, models)
		}
	}
}

func TestFetchUpstreamModelsAnthropicAuthPerType(t *testing.T) {
	cases := []struct {
		providerType ProviderType
		hostURL      string
		wantBearer   bool
		wantAPIKey   bool
	}{
		// 官方域名非代理：Node 发 Bearer + x-api-key 两件。
		{TypeClaude, "https://api.anthropic.com", true, true},
		// claude-auth 强制 Bearer only。
		{TypeClaudeAuth, "https://api.anthropic.com", true, false},
	}
	for _, testCase := range cases {
		server := fakeUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
			_, _ = writer.Write([]byte(`{"data":[{"id":"claude-opus-4-1"},{"id":"claude-sonnet-4-5"}]}`))
		})
		client := newTestClient(t)
		// 直接用假上游地址：此时 hostname 既非官方也非 relay 标识，故走「两件都发」的兜底分支，
		// 断言 auth 形态用 resolve 结果做白盒核对。
		models, err := FetchUpstreamModels(context.Background(), client, testCase.providerType, server.URL, "sk-ant")
		if err != nil {
			t.Fatalf("%s: 应成功，实际 %v", testCase.providerType, err)
		}
		if len(models) != 2 || models[0] != "claude-opus-4-1" {
			t.Errorf("%s: 模型列表不符：%v", testCase.providerType, models)
		}

		// 白盒核对认证头形态（按 Node 的 resolveAnthropicAuthHeaders 语义）。
		headers := anthropicAuthForTest(testCase.providerType, testCase.hostURL, "sk-ant")
		if _, has := headers["authorization"]; has != testCase.wantBearer {
			t.Errorf("%s@%s: Bearer 头存在性应为 %v，实际 %v（%v）", testCase.providerType, testCase.hostURL, testCase.wantBearer, has, headers)
		}
		if _, has := headers["x-api-key"]; has != testCase.wantAPIKey {
			t.Errorf("%s@%s: x-api-key 存在性应为 %v，实际 %v（%v）", testCase.providerType, testCase.hostURL, testCase.wantAPIKey, has, headers)
		}
	}
}

func TestFetchUpstreamModelsGeminiFilteringAndRetry(t *testing.T) {
	var attempts []string
	server := fakeUpstream(t, func(writer http.ResponseWriter, request *http.Request) {
		attempts = append(attempts, request.URL.RawQuery)
		if request.URL.Query().Get("key") == "" {
			// 首次：header 认证失败，逼 Node 走 URL 参数重试。
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = writer.Write([]byte(`{"error":{"message":"missing key"}}`))
			return
		}
		if got := request.Header.Get("x-goog-api-key"); got != "gm-key" {
			t.Errorf("重试仍应带 x-goog-api-key，实际 %q", got)
		}
		_, _ = writer.Write([]byte(`{"models":[
			{"name":"models/gemini-2.5-pro","supportedGenerationMethods":["generateContent"]},
			{"name":"models/gemini-embedding","supportedGenerationMethods":["embedContent"]},
			{"name":"models/gemini-2.5-flash","supportedGenerationMethods":null},
			{"name":"models/gemini-2.5-flash","supportedGenerationMethods":["generateContent"]}
		]}`))
	})

	models, err := FetchUpstreamModels(context.Background(), newTestClient(t), TypeGemini, server.URL, "gm-key")
	if err != nil {
		t.Fatalf("应成功，实际 %v", err)
	}
	want := []string{"gemini-2.5-flash", "gemini-2.5-flash", "gemini-2.5-pro"}
	if !reflect.DeepEqual(models, want) {
		t.Errorf("过滤/去前缀/排序不符：want %v，got %v", want, models)
	}
	if len(attempts) != 2 {
		t.Fatalf("401 后应重试一次（共 2 次请求），实际 %d 次：%v", len(attempts), attempts)
	}
	if attempts[0] != "pageSize=100" {
		t.Errorf("首次请求查询串应为 pageSize=100，实际 %s", attempts[0])
	}
}

func TestFetchUpstreamModelsErrorShapes(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		wantErr string
	}{
		{
			name: "非 2xx",
			handler: func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(http.StatusInternalServerError)
			},
			wantErr: "API 返回错误: HTTP 500",
		},
		{
			name: "缺 data 数组",
			handler: func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = writer.Write([]byte(`{"object":"list"}`))
			},
			wantErr: "响应格式无效：缺少 data 数组",
		},
		{
			name: "非 JSON",
			handler: func(writer http.ResponseWriter, _ *http.Request) {
				_, _ = writer.Write([]byte(`not json`))
			},
			wantErr: "响应格式无效：缺少 data 数组",
		},
	}
	for _, testCase := range cases {
		server := fakeUpstream(t, testCase.handler)
		_, err := FetchUpstreamModels(context.Background(), newTestClient(t), TypeOpenAICompatible, server.URL, "k")
		if err == nil || err.Error() != testCase.wantErr {
			t.Errorf("%s: 错误应为 %q，实际 %v", testCase.name, testCase.wantErr, err)
		}
	}
}

func TestFetchUpstreamModelsGeminiMissingModelsArray(t *testing.T) {
	server := fakeUpstream(t, func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"candidates":[]}`))
	})
	_, err := FetchUpstreamModels(context.Background(), newTestClient(t), TypeGemini, server.URL, "k")
	if err == nil || err.Error() != "响应格式无效：缺少 models 数组" {
		t.Errorf("Gemini 缺 models 应报对应错误，实际 %v", err)
	}
}

// TestUpstreamClientProxySchemes 钉住代理协议支持面：http/https/socks5 可建，
// socks4 与未知协议显式失败（不静默直连）。
func TestUpstreamClientProxySchemes(t *testing.T) {
	for _, raw := range []string{"http://127.0.0.1:8080", "https://127.0.0.1:8443", "socks5://user:pass@127.0.0.1:1080"} {
		if _, err := NewUpstreamClient(ProxyConfig{URL: raw}, time.Second); err != nil {
			t.Errorf("%s 应可建客户端，实际 %v", raw, err)
		}
	}
	for _, raw := range []string{"socks4://127.0.0.1:1080", "ftp://127.0.0.1:21"} {
		if _, err := NewUpstreamClient(ProxyConfig{URL: raw}, time.Second); err == nil {
			t.Errorf("%s 应显式失败（不支持），实际成功", raw)
		}
	}
	if _, err := NewUpstreamClient(ProxyConfig{}, time.Second); err != nil {
		t.Errorf("无代理应可建客户端，实际 %v", err)
	}
}

func TestProxyAndProviderURLValidation(t *testing.T) {
	if !IsValidProxyURL("socks5://127.0.0.1:1080") || !IsValidProxyURL("http://proxy.local:3128") {
		t.Error("合法代理 URL 应通过")
	}
	for _, raw := range []string{"", "   ", "not a url", "ftp://x:1", "http://"} {
		if IsValidProxyURL(raw) {
			t.Errorf("%q 应判为非法代理 URL", raw)
		}
	}

	if _, _, ok := ValidateProviderURLForConnectivity("https://api.example.com/"); !ok {
		t.Error("https URL 应通过连通性校验")
	}
	if _, message, ok := ValidateProviderURLForConnectivity("ftp://x"); ok || message != "供应商地址格式无效" {
		t.Errorf("ftp 应被拒且消息固定，实际 ok=%v msg=%q", ok, message)
	}
	// 端到端：把 JSON 编解码往返一次，确保模型列表形状与 Node 一致。
	server := fakeUpstream(t, func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte(`{"data":[{"id":"m1"}]}`))
	})
	models, err := FetchUpstreamModels(context.Background(), newTestClient(t), TypeCodex, server.URL, "k")
	if err != nil {
		t.Fatalf("应成功: %v", err)
	}
	encoded, _ := json.Marshal(models)
	if string(encoded) != `["m1"]` {
		t.Errorf("序列化形状应为 [\"m1\"]，实际 %s", encoded)
	}
}

// anthropicAuthForTest 白盒核对 Node 的 resolveAnthropicAuthHeaders 语义。
func anthropicAuthForTest(providerType ProviderType, providerURL, apiKey string) map[string]string {
	return forward.ResolveAnthropicAuthHeaders(apiKey, providerURL, providerType == TypeClaudeAuth)
}
