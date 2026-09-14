package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// providers_test_proxy_test.go：`POST /providers/test:proxy` 的形状与「校验失败也 200」语义。

func callTestProxy(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/providers/test:proxy", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handleTestProviderProxy(Deps{})(recorder, request)
	return recorder
}

func decodeProxyPayload(t *testing.T, recorder *httptest.ResponseRecorder) proxyTestPayload {
	t.Helper()
	var payload proxyTestPayload
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("正文解析失败: %v（%s）", err, recorder.Body.String())
	}
	return payload
}

func TestTestProxySuccessAnyStatusCode(t *testing.T) {
	// 假上游故意回 500：Node 的代理测试只看「连得上」，任何状态码都算成功。
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodHead {
			t.Errorf("应为 HEAD 请求，实际 %s", request.Method)
		}
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	recorder := callTestProxy(t, `{"providerUrl":"`+upstream.URL+`"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d（%s）", recorder.Code, recorder.Body.String())
	}
	payload := decodeProxyPayload(t, recorder)
	if !payload.Success {
		t.Fatalf("500 也应算连接成功，实际 %+v", payload)
	}
	if payload.Details == nil || payload.Details.StatusCode == nil || *payload.Details.StatusCode != 500 {
		t.Errorf("details.statusCode 应为 500，实际 %+v", payload.Details)
	}
	if payload.Details.UsedProxy == nil || *payload.Details.UsedProxy {
		t.Errorf("未用代理时 usedProxy 应为 false，实际 %+v", payload.Details.UsedProxy)
	}
	if payload.Details.ProxyURL != "" {
		t.Errorf("未用代理时不应出现 proxyUrl 键，实际 %q", payload.Details.ProxyURL)
	}
	if !strings.HasPrefix(payload.Message, "成功连接到 ") {
		t.Errorf("成功文案应为「成功连接到 <host>」，实际 %q", payload.Message)
	}
}

// TestTestProxyValidationReturns200WithSuccessFalse 钉住 Node 的「校验失败也 200」语义。
func TestTestProxyValidationReturns200WithSuccessFalse(t *testing.T) {
	cases := []struct {
		name          string
		body          string
		wantErrorType string
		wantMessage   string
	}{
		{"providerUrl 非 http(s)", `{"providerUrl":"ftp://x.example.com"}`, "InvalidProviderUrl", "供应商地址格式无效"},
		{"代理地址非法", `{"providerUrl":"https://a.example.com","proxyUrl":"ftp://p:1"}`, "InvalidProxyUrl", "代理地址格式无效"},
		{"代理协议本包不支持", `{"providerUrl":"https://a.example.com","proxyUrl":"socks4://p:1080"}`, "InvalidProxyUrl", "代理地址格式无效"},
	}
	for _, testCase := range cases {
		recorder := callTestProxy(t, testCase.body)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: 应 200（Node 把校验失败包成 ok:true），实际 %d（%s）", testCase.name, recorder.Code, recorder.Body.String())
		}
		payload := decodeProxyPayload(t, recorder)
		if payload.Success {
			t.Errorf("%s: success 应为 false", testCase.name)
		}
		if payload.Message != testCase.wantMessage {
			t.Errorf("%s: message 应为 %q，实际 %q", testCase.name, testCase.wantMessage, payload.Message)
		}
		if payload.Details == nil || payload.Details.ErrorType != testCase.wantErrorType {
			t.Errorf("%s: errorType 应为 %s，实际 %+v", testCase.name, testCase.wantErrorType, payload.Details)
		}
	}
}

// TestTestProxySchemaValidation 钉住 strict schema 的校验（这些才是 400）。
func TestTestProxySchemaValidation(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantPath string
		wantCode string
	}{
		{"未知键", `{"providerUrl":"https://a.example.com","apiKey":"k"}`, "apiKey", "unrecognized_keys"},
		{"缺 providerUrl", `{"proxyUrl":null}`, "providerUrl", "invalid_type"},
		{"providerUrl 非 URL", `{"providerUrl":"nope"}`, "providerUrl", "invalid_format"},
		{"proxyUrl 类型错", `{"providerUrl":"https://a.example.com","proxyUrl":1}`, "proxyUrl", "invalid_type"},
		{"proxyFallbackToDirect 类型错", `{"providerUrl":"https://a.example.com","proxyFallbackToDirect":"yes"}`, "proxyFallbackToDirect", "invalid_type"},
	}
	for _, testCase := range cases {
		recorder := callTestProxy(t, testCase.body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: 应 400，实际 %d（%s）", testCase.name, recorder.Code, recorder.Body.String())
		}
		var problem struct {
			InvalidParams []invalidParam `json:"invalidParams"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &problem); err != nil {
			t.Fatalf("%s: 正文解析失败: %v", testCase.name, err)
		}
		found := false
		for _, issue := range problem.InvalidParams {
			if len(issue.Path) == 1 && issue.Path[0] == testCase.wantPath && issue.Code == testCase.wantCode {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: 未找到 %s/%s，实际 %+v", testCase.name, testCase.wantPath, testCase.wantCode, problem.InvalidParams)
		}
	}
}

func TestTestProxyConnectionFailureIsReported(t *testing.T) {
	// 指向一个必然连不上的地址（保留给未监听的端口），验证失败形状与错误分类。
	recorder := callTestProxy(t, `{"providerUrl":"http://127.0.0.1:9"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("连接失败也应 200（success:false），实际 %d", recorder.Code)
	}
	payload := decodeProxyPayload(t, recorder)
	if payload.Success {
		t.Fatalf("连不上时 success 应为 false，实际 %+v", payload)
	}
	if !strings.HasPrefix(payload.Message, "连接失败: ") {
		t.Errorf("失败文案应为「连接失败: …」，实际 %q", payload.Message)
	}
	if payload.Details == nil || payload.Details.ErrorType != "NetworkError" {
		t.Errorf("无代理时 errorType 应为 NetworkError，实际 %+v", payload.Details)
	}
}

func TestTestProxyProxyFailureClassifiedAsProxyError(t *testing.T) {
	// 代理指向未监听端口：应归因 ProxyError（与 Node 的 isProxyError 语义一致）。
	recorder := callTestProxy(t, `{"providerUrl":"https://a.example.com","proxyUrl":"http://127.0.0.1:9"}`)
	payload := decodeProxyPayload(t, recorder)
	if payload.Success {
		t.Fatalf("代理不可用时应 success:false，实际 %+v", payload)
	}
	if payload.Details == nil || payload.Details.ErrorType != "ProxyError" {
		t.Errorf("用代理且失败时 errorType 应为 ProxyError，实际 %+v", payload.Details)
	}
	if payload.Details.UsedProxy == nil || !*payload.Details.UsedProxy {
		t.Errorf("usedProxy 应为 true，实际 %+v", payload.Details)
	}
	if payload.Details.ProxyURL == "" {
		t.Errorf("用代理时 details 应带 proxyUrl，实际 %+v", payload.Details)
	}
}
