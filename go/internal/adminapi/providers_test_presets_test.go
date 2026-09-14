package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// providers_test_presets_test.go 覆盖 GET /providers/test:presets 的形状与校验语义。
// 本端点不读库（纯静态数据），故无需真 PG。

func callTestPresets(t *testing.T, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	handleProviderTestPresets(Deps{})(recorder, request)
	return recorder
}

func TestProviderTestPresetsShapeAndOrder(t *testing.T) {
	cases := []struct {
		providerType string
		wantIDs      []string
	}{
		{"claude", []string{"cc_haiku_basic", "cc_beta_cli", "cc_public_thinking"}},
		{"codex", []string{"cx_codex_basic", "cx_gpt_basic"}},
		{"openai-compatible", []string{"oa_chat_basic", "oa_chat_stream"}},
		{"gemini", []string{"gm_flash_basic", "gm_pro_basic"}},
	}
	for _, testCase := range cases {
		recorder := callTestPresets(t, "/api/v1/providers/test:presets?providerType="+testCase.providerType, nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: 状态码应为 200，实际 %d（正文 %s）", testCase.providerType, recorder.Code, recorder.Body.String())
		}
		if got := recorder.Header().Get("Content-Type"); got != "application/json" {
			t.Errorf("%s: Content-Type 应为 application/json，实际 %q", testCase.providerType, got)
		}
		var items []providerTestPresetPayload
		if err := json.Unmarshal(recorder.Body.Bytes(), &items); err != nil {
			t.Fatalf("%s: 正文不是数组：%v（%s）", testCase.providerType, err, recorder.Body.String())
		}
		if len(items) != len(testCase.wantIDs) {
			t.Fatalf("%s: 预设数应为 %d，实际 %d", testCase.providerType, len(testCase.wantIDs), len(items))
		}
		for i, want := range testCase.wantIDs {
			if items[i].ID != want {
				t.Errorf("%s[%d] 应为 %s，实际 %s", testCase.providerType, i, want, items[i].ID)
			}
		}
		// 四个字段都要在（缺省字段会让前端拿不到默认模型/成功串）。
		first := items[0]
		if first.Description == "" || first.DefaultModel == "" || first.DefaultSuccessContains == "" {
			t.Errorf("%s: 首项字段不完整：%+v", testCase.providerType, first)
		}
	}
}

// TestProviderTestPresetsHiddenTypesRequireCompatHeader 钉住隐藏类型的可访问性：
// 公开路径下拒绝，带兼容头（且管理员）才允许——与 Node 的 Internal 分支同义。
func TestProviderTestPresetsHiddenTypesRequireCompatHeader(t *testing.T) {
	recorder := callTestPresets(t, "/api/v1/providers/test:presets?providerType=claude-auth", nil)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("无兼容头时 claude-auth 应 400，实际 %d", recorder.Code)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/v1/providers/test:presets?providerType=claude-auth", nil)
	request.Header.Set(dashboardCompatHeader, "1")
	request = request.WithContext(WithPrincipal(request.Context(), Principal{UserID: 1, Username: "admin", IsAdmin: true}))
	recorder2 := httptest.NewRecorder()
	handleProviderTestPresets(Deps{})(recorder2, request)
	if recorder2.Code != http.StatusOK {
		t.Fatalf("带兼容头 + 管理员时 claude-auth 应 200，实际 %d（%s）", recorder2.Code, recorder2.Body.String())
	}
	var items []providerTestPresetPayload
	if err := json.Unmarshal(recorder2.Body.Bytes(), &items); err != nil {
		t.Fatalf("正文解析失败: %v", err)
	}
	if len(items) != 3 {
		t.Errorf("claude-auth 应有 3 个预设，实际 %d", len(items))
	}
}

// TestProviderTestPresetsValidation 钉住三种校验失败的 zod 码。
func TestProviderTestPresetsValidation(t *testing.T) {
	cases := []struct {
		name     string
		target   string
		wantCode string
	}{
		{"缺参数", "/api/v1/providers/test:presets", "invalid_type"},
		// 空串是「有值但是空字符串」：zod 的 z.enum 对空串给 invalid_enum_value（不是 invalid_type）。
		{"空串", "/api/v1/providers/test:presets?providerType=", "invalid_enum_value"},
		{"未知值", "/api/v1/providers/test:presets?providerType=nope", "invalid_enum_value"},
		{"含空白", "/api/v1/providers/test:presets?providerType=%20claude%20", "invalid_enum_value"},
	}
	for _, testCase := range cases {
		recorder := callTestPresets(t, testCase.target, nil)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: 应 400，实际 %d", testCase.name, recorder.Code)
		}
		var body struct {
			ErrorCode     string         `json:"errorCode"`
			InvalidParams []invalidParam `json:"invalidParams"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: 正文解析失败: %v", testCase.name, err)
		}
		if body.ErrorCode != "request.validation_failed" {
			t.Errorf("%s: errorCode 应为 request.validation_failed，实际 %s", testCase.name, body.ErrorCode)
		}
		if len(body.InvalidParams) != 1 {
			t.Fatalf("%s: 应有 1 条 invalidParams，实际 %d", testCase.name, len(body.InvalidParams))
		}
		if body.InvalidParams[0].Code != testCase.wantCode {
			t.Errorf("%s: invalidParams[0].code 应为 %s，实际 %s", testCase.name, testCase.wantCode, body.InvalidParams[0].Code)
		}
	}
}
