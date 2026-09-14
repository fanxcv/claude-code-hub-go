package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// providers_upstream_models_test.go：本端点校验 + 形状 + 上游分支，全部走本机假上游。

func callUpstreamModels(t *testing.T, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/providers/upstream-models:fetch", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	handleFetchProviderUpstreamModels(Deps{})(recorder, request)
	return recorder
}

func TestUpstreamModelsSuccessShape(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/models" {
			t.Errorf("上游路径应为 /v1/models，实际 %s", request.URL.Path)
		}
		_, _ = writer.Write([]byte(`{"data":[{"id":"z"},{"id":"a"}]}`))
	}))
	defer upstream.Close()

	recorder := callUpstreamModels(t, `{"providerUrl":"`+upstream.URL+`","apiKey":"sk-x","providerType":"openai-compatible"}`, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d（%s）", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type 应为 application/json，实际 %q", got)
	}
	var body struct {
		Models []string `json:"models"`
		Source string   `json:"source"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("正文解析失败: %v（%s）", err, recorder.Body.String())
	}
	if body.Source != "upstream" {
		t.Errorf("source 应为 upstream，实际 %q", body.Source)
	}
	if len(body.Models) != 2 || body.Models[0] != "a" || body.Models[1] != "z" {
		t.Errorf("模型应升序 [a z]，实际 %v", body.Models)
	}
}

func TestUpstreamModelsValidationIssues(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantPath string
		wantCode string
	}{
		{"未知键", `{"providerUrl":"https://a.example.com","apiKey":"k","providerType":"claude","bogus":1}`, "bogus", "unrecognized_keys"},
		{"缺 providerUrl", `{"apiKey":"k","providerType":"claude"}`, "providerUrl", "invalid_type"},
		{"providerUrl 非 URL", `{"providerUrl":"nope","apiKey":"k","providerType":"claude"}`, "providerUrl", "invalid_format"},
		{"apiKey 空串", `{"providerUrl":"https://a.example.com","apiKey":"","providerType":"claude"}`, "apiKey", "too_small"},
		{"providerType 未知值", `{"providerUrl":"https://a.example.com","apiKey":"k","providerType":"nope"}`, "providerType", "invalid_enum_value"},
		{"隐藏类型无兼容头", `{"providerUrl":"https://a.example.com","apiKey":"k","providerType":"claude-auth"}`, "providerType", "invalid_enum_value"},
		{"timeoutMs 过小", `{"providerUrl":"https://a.example.com","apiKey":"k","providerType":"claude","timeoutMs":4999}`, "timeoutMs", "too_small"},
		{"timeoutMs 过大", `{"providerUrl":"https://a.example.com","apiKey":"k","providerType":"claude","timeoutMs":120001}`, "timeoutMs", "too_big"},
		{"timeoutMs 非整数", `{"providerUrl":"https://a.example.com","apiKey":"k","providerType":"claude","timeoutMs":5000.5}`, "timeoutMs", "invalid_type"},
		{"字段类型错", `{"providerUrl":123,"apiKey":"k","providerType":"claude"}`, "providerUrl", "invalid_type"},
	}
	for _, testCase := range cases {
		recorder := callUpstreamModels(t, testCase.body, nil)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: 应 400，实际 %d（%s）", testCase.name, recorder.Code, recorder.Body.String())
		}
		var problem struct {
			ErrorCode     string         `json:"errorCode"`
			InvalidParams []invalidParam `json:"invalidParams"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &problem); err != nil {
			t.Fatalf("%s: 正文解析失败: %v", testCase.name, err)
		}
		if problem.ErrorCode != "request.validation_failed" {
			t.Errorf("%s: errorCode 应为 request.validation_failed，实际 %s", testCase.name, problem.ErrorCode)
		}
		found := false
		for _, issue := range problem.InvalidParams {
			if len(issue.Path) == 1 && issue.Path[0] == testCase.wantPath && issue.Code == testCase.wantCode {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: 未找到 %s/%s 的校验项，实际 %+v", testCase.name, testCase.wantPath, testCase.wantCode, problem.InvalidParams)
		}
	}
}

func TestUpstreamModelsHiddenTypeAllowedWithCompatHeader(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1beta/models" {
			t.Errorf("gemini 分支应打 /v1beta/models，实际 %s", request.URL.Path)
		}
		if got := request.URL.Query().Get("pageSize"); got != "100" {
			t.Errorf("应带 pageSize=100，实际 %q", got)
		}
		_, _ = writer.Write([]byte(`{"models":[{"name":"models/gemini-2.5-pro","supportedGenerationMethods":["generateContent"]}]}`))
	}))
	defer upstream.Close()

	request := httptest.NewRequest(http.MethodPost, "/api/v1/providers/upstream-models:fetch",
		strings.NewReader(`{"providerUrl":"`+upstream.URL+`","apiKey":"k","providerType":"gemini-cli"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(dashboardCompatHeader, "1")
	request = request.WithContext(WithPrincipal(request.Context(), Principal{UserID: 1, IsAdmin: true}))
	recorder := httptest.NewRecorder()
	handleFetchProviderUpstreamModels(Deps{})(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("兼容头 + 管理员下 gemini-cli 应 200，实际 %d（%s）", recorder.Code, recorder.Body.String())
	}
	var body struct {
		Models []string `json:"models"`
		Source string   `json:"source"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("正文解析失败: %v", err)
	}
	if len(body.Models) != 1 || body.Models[0] != "gemini-2.5-pro" {
		t.Errorf("应返回去前缀后的模型名，实际 %v", body.Models)
	}
	if body.Source != "upstream" {
		t.Errorf("source 应为 upstream，实际 %q", body.Source)
	}
}

func TestUpstreamModelsActionErrorEnvelopes(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"providerUrl 非 http(s)", `{"providerUrl":"ftp://a.example.com","apiKey":"k","providerType":"claude"}`},
		{"代理地址非法", `{"providerUrl":"https://a.example.com","apiKey":"k","providerType":"claude","proxyUrl":"ftp://p:1"}`},
	}
	for _, testCase := range cases {
		recorder := callUpstreamModels(t, testCase.body, nil)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: 应 400，实际 %d（%s）", testCase.name, recorder.Code, recorder.Body.String())
		}
		var problem struct {
			ErrorCode string `json:"errorCode"`
			Status    int    `json:"status"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &problem); err != nil {
			t.Fatalf("%s: 正文解析失败: %v", testCase.name, err)
		}
		if problem.ErrorCode != "provider.action_failed" {
			t.Errorf("%s: errorCode 应为 provider.action_failed（Node 的 statusFromActionError 默认分支），实际 %s", testCase.name, problem.ErrorCode)
		}
		if problem.Status != http.StatusBadRequest {
			t.Errorf("%s: status 应为 400，实际 %d", testCase.name, problem.Status)
		}
	}
}

func TestUpstreamModelsUpstreamFailureBecomesActionError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusTeapot)
	}))
	defer upstream.Close()

	recorder := callUpstreamModels(t, `{"providerUrl":"`+upstream.URL+`","apiKey":"k","providerType":"codex"}`, nil)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("上游 418 时应映射为 400（Node 的 API 返回错误 分支），实际 %d（%s）", recorder.Code, recorder.Body.String())
	}
	var problem struct {
		ErrorCode string `json:"errorCode"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &problem); err != nil {
		t.Fatalf("正文解析失败: %v", err)
	}
	if problem.ErrorCode != "provider.action_failed" {
		t.Errorf("errorCode 应为 provider.action_failed，实际 %s", problem.ErrorCode)
	}
}
