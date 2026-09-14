package adminapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住旧端点 /api/admin/system-config 的两件易错事：
//
//  1. 非管理员是**纯文本 401**（不是 problem+json）——它在这条路径上是 UI 登录页的判据；
//  2. POST 只转发 Node 显式列出的字段，其余「校验通过但不转发」。照抄这个取舍而不是顺手补上：
//     补上会让两个入口对同一次调用给出两个结果。

// TestSystemConfigPlainTextUnauthorized 钉住纯文本 401。
func TestSystemConfigPlainTextUnauthorized(t *testing.T) {
	api := &systemConfigAPI{logger: logx.New(nil)}
	// 请求上下文里没有身份（Node 的 session 为 null 或角色非 admin 的等价物）。
	request := httptest.NewRequest(http.MethodGet, "/api/admin/system-config", nil)
	recorder := httptest.NewRecorder()
	api.handleSystemConfig(recorder, request)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("非管理员应 401，得到 %d", recorder.Code)
	}
	if body := recorder.Body.String(); body != unauthorizedPlainText {
		t.Fatalf("正文应为纯文本 %q，得到 %q", unauthorizedPlainText, body)
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/plain") {
		t.Fatalf("Content-Type 应为 text/plain，得到 %q", contentType)
	}
}

// TestSystemConfigLegacyPatchDropsFieldsNotForwarded 钉住旧端点的字段取舍。
func TestSystemConfigLegacyPatchDropsFieldsNotForwarded(t *testing.T) {
	title := "Legacy Title"
	keep := true
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{
		"siteTitle": "Legacy Title",
		"allowGlobalUsageView": true,
		"billNonSuccessfulRequests": true,
		"billHedgeLosers": true,
		"allowNonConversationEndpointProviderFallback": true,
		"fakeStreamingWhitelist": [{"model": "m", "groupTags": []}],
		"replayEnabled": true,
		"replayCacheTtlMinutes": 45,
		"cacheEffectivenessEnabled": true,
		"publicStatusWindowHours": 12,
		"publicStatusAggregationIntervalMinutes": 15,
		"ipExtractionConfig": {"headers": [{"name": "x-real-ip"}]},
		"ipGeoLookupEnabled": true
	}`), &raw); err != nil {
		t.Fatalf("解析用例失败：%v", err)
	}
	decoded, problems := decodeSystemSettingsUpdate(raw, settingsUpdateActionSchema)
	if len(problems) > 0 {
		t.Fatalf("这些键在 Node 里是**通过校验**的（只是不转发），不应报错：%+v", problems)
	}
	patch := decoded.legacyPatch()
	updates := patch.Updates

	if got, ok := updates[store.ColSiteTitle]; !ok || got != title {
		t.Fatalf("siteTitle 应转发，得到 %v（present=%v）", got, ok)
	}
	if got, ok := updates[store.ColAllowGlobalUsageView]; !ok || got != keep {
		t.Fatalf("allowGlobalUsageView 应转发，得到 %v（present=%v）", got, ok)
	}
	for _, column := range []store.AdminSystemSettingsColumn{
		store.ColBillNonSuccessfulRequests,
		store.ColBillHedgeLosers,
		store.ColAllowNonConvEndpointFallback,
		store.ColFakeStreamingWhitelist,
		store.ColReplayEnabled,
		store.ColReplayCacheTTLMinutes,
		store.ColCacheEffectivenessEnabled,
		store.ColPublicStatusWindowHours,
		store.ColPublicStatusAggregationMins,
		store.ColIPExtractionConfig,
		store.ColIPGeoLookupEnabled,
	} {
		if _, ok := updates[column]; ok {
			t.Errorf("列 %s 不应被旧端点转发（Node 的字段清单里没有它）", column)
		}
	}
	// v1 路径必须原样保留这些字段（两条入口的差异只应出现在旧端点这一侧）。
	v1Patch := decoded.patch()
	if _, ok := v1Patch.Updates[store.ColReplayCacheTTLMinutes]; !ok {
		t.Error("v1 的 patch 不得丢字段：replayCacheTtlMinutes 缺失")
	}
}

// TestSystemConfigLegacyValidationErrorShapes 钉住旧端点的 400 三段正文。
func TestSystemConfigLegacyValidationErrorShapes(t *testing.T) {
	cases := []struct {
		name     string
		problems []settingsInvalidParam
		fragment string
	}{
		{
			name: "窗口违约",
			problems: []settingsInvalidParam{{
				Path: []any{"racingTotalTimeoutMs"}, Code: "custom", Message: discoveryWindowInvalidErrorCode,
			}},
			fragment: `"error":"discoveryWindowInvalid"`,
		},
		{
			name: "discovery 字段越界",
			problems: []settingsInvalidParam{{
				Path: []any{"discoveryConcurrency"}, Code: "too_small", Message: discoverySettingsInvalidErrorCode,
			}},
			fragment: `"error":"discoverySettingsInvalid"`,
		},
		{
			name: "其余校验失败",
			problems: []settingsInvalidParam{{
				Path: []any{"siteTitle"}, Code: "invalid_type", Message: "Expected string",
			}},
			fragment: `"error":"Expected string"`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			writeLegacySystemConfigValidationError(recorder, testCase.problems)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("应 400，得到 %d", recorder.Code)
			}
			if body := recorder.Body.String(); !strings.Contains(body, testCase.fragment) {
				t.Fatalf("正文应含 %s，得到 %s", testCase.fragment, body)
			}
		})
	}
}

// TestSystemConfigRegistersBothMethods 钉住 GET 与 POST 都在路由表里。
func TestSystemConfigRegistersBothMethods(t *testing.T) {
	deps := Deps{Logger: logx.New(nil), Guard: &recordingGuard{}, Store: new(store.Pools)}
	router := New(Options{Deps: deps})
	RegisterSystemConfigRoutes(router, deps)
	methods := map[string]bool{}
	for _, route := range router.RouteList() {
		if route.Path == "/api/admin/system-config" {
			methods[route.Method] = true
		}
	}
	if !methods[http.MethodGet] || !methods[http.MethodPost] {
		t.Fatalf("GET 与 POST 都应注册，实际 %v", methods)
	}
}
