package dataplane

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/gate"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
)

// noProviderAdapter 是返回「无可用供应商（带归因）」的选路缝隙。
//
// 用它而不是真库：生产库里 371 家启用供应商的 allowed_models 全为空（= 全放行），
// 随便一个模型名都会被它们接住，因此「模型无人支持」这种 503 在共享测试库里**不可达**
// （除非破坏性地改全局数据）。本夹具让真实 dataplane 的 503 分支被确定性地走到。
type noProviderAdapter struct {
	clientFormat string
	context      route.DecisionContext
}

func (a noProviderAdapter) Select(_ context.Context, _ *pctx.Context) (pctx.ProviderSelection, error) {
	return pctx.ProviderSelection{}, guard.NewNoProviderError(a.context, a.clientFormat)
}

// authorizingAuth 是「任何密钥都解析成功」的鉴权缝（键与用户都必须启用，否则
// 认证守卫会在抵达选路之前就 401/403，测不到 503 那条路）。
var authorizingAuth = fakeAuth{
	user: guard.User{ID: 1, Name: "甲", IsEnabled: true},
	key:  guard.Key{ID: 2, Name: "k", UserID: 1},
}

// newNoProviderHandler 装配一个日志可捕获的数据面，选路恒为「无可用供应商」。
func newNoProviderHandler(t *testing.T, adapter noProviderAdapter) (*Handler, *bytes.Buffer) {
	t.Helper()
	dialClient, err := dial.New(dial.Options{})
	if err != nil {
		t.Fatalf("拨号器构造失败: %v", err)
	}
	logs := &bytes.Buffer{}
	handler, err := New(Options{
		Logger: logx.New(logs),
		Base: guard.Deps{
			Auth:           authorizingAuth,
			Users:          authorizingAuth,
			Settings:       fakeSettings{},
			Sensitive:      fakeEmptySource{},
			Filters:        fakeEmptySource{},
			Provider:       adapter,
			MessageContext: &fakeMessageWriter{},
		},
		Candidates: fakeCandidates{url: "http://127.0.0.1:1"},
		Settlers: func(*RequestState) Settler {
			return &fakeSettler{}
		},
		Forward: forward.Deps{Dial: dialClient},
		Stream:  forward.StreamOptions{Budget: gate.DefaultBudget()},
	})
	if err != nil {
		t.Fatalf("数据面构造失败: %v", err)
	}
	return handler, logs
}

// TestNoProviderAvailableResponseContractUnchanged 是**契约钉子**：
// 加归因不许动响应。用户按 Node 契约消费这条 503（message/type/code 三个字段），
// 任何新增字段、改写文案或改状态码都是破坏性变更。
//
// 期望正文逐字节写死（来自改造前的实现），故一旦有人「顺手」把诊断塞进响应，这条就会红。
func TestNoProviderAvailableResponseContractUnchanged(t *testing.T) {
	handler, _ := newNoProviderHandler(t, noProviderAdapter{
		clientFormat: "responses",
		context: route.DecisionContext{
			RequestedModel:          "dsf4",
			TotalProviders:          9,
			ModelSupportedProviders: 0,
			FilteredProviders: []route.Filtered{
				{ID: 1, Name: "p1", Reason: route.ReasonModelNotAllowed},
			},
		},
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"dsf4","input":"hi"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", "fake-client-key")
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码应为 503，收到 %d（%s）", recorder.Code, recorder.Body.String())
	}

	// 逐字段对照 Node 契约（不改形状）。用解析后的比较而不是整串比较：
	// 字段顺序不属于契约，键集与取值才是。
	var payload map[string]map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是 JSON 对象：%v（%s）", err, recorder.Body.String())
	}
	errorObject, ok := payload["error"]
	if !ok {
		t.Fatalf("响应里没有 error 对象：%s", recorder.Body.String())
	}
	want := map[string]string{
		"message": "No available providers",
		"type":    "no_available_providers",
		"code":    "no_available_providers",
	}
	for key, value := range want {
		if got, _ := errorObject[key].(string); got != value {
			t.Errorf("error.%s = %v，期望 %q", key, errorObject[key], value)
		}
	}
	// 归因只能在日志里，不得出现在响应里（防「顺手加 detail」）。
	for _, forbidden := range []string{"dsf4", "modelSupportedProviders", "diagnostic", "cause"} {
		if strings.Contains(recorder.Body.String(), forbidden) {
			t.Errorf("响应体不得包含诊断信息 %q：%s", forbidden, recorder.Body.String())
		}
	}
}

// TestNoProviderAvailableLogsDiagnostic 是**本任务的核心断言**：
// 这条 503 的归因必须能从日志一眼看出——模型没人支持 vs 供应商真不可用。
func TestNoProviderAvailableLogsDiagnostic(t *testing.T) {
	handler, logs := newNoProviderHandler(t, noProviderAdapter{
		clientFormat: "responses",
		context: route.DecisionContext{
			RequestedModel:          "dsf4",
			TotalProviders:          9,
			ModelSupportedProviders: 0,
			EnabledProviders:        0,
			AfterHealthCheck:        0,
			FilteredProviders: []route.Filtered{
				{ID: 1, Name: "p1", Reason: route.ReasonModelNotAllowed},
				{ID: 2, Name: "p2", Reason: route.ReasonModelNotAllowed},
				{ID: 3, Name: "p3", Reason: route.ReasonProtocolConversionDisabled},
			},
		},
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"dsf4","input":"hi"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", "fake-client-key")
	handler.ServeHTTP(recorder, request)

	event := findLogEvent(t, logs.String(), "dataplane.no_provider_available")
	if event == nil {
		t.Fatalf("应写出 dataplane.no_provider_available 事件，实际日志：%s", logs.String())
	}
	if got := event["model"]; got != "dsf4" {
		t.Errorf("日志 model = %v，期望 dsf4", got)
	}
	if got := event["cause"]; got != guard.NoProviderCauseModelUnknown {
		t.Errorf("日志 cause = %v，期望 %q", got, guard.NoProviderCauseModelUnknown)
	}
	if got, ok := event["modelSupportedProviders"].(float64); !ok || int(got) != 0 {
		t.Errorf("日志 modelSupportedProviders = %v，期望 0", event["modelSupportedProviders"])
	}
	if got := event["reasonCounts"]; got != "model_not_allowed=2,protocol_conversion_disabled=1" {
		t.Errorf("日志 reasonCounts = %v", got)
	}
	summary, _ := event["summary"].(string)
	if !strings.Contains(summary, "dsf4") {
		t.Errorf("日志 summary 应点名模型，实际 %q", summary)
	}
	if got := event["path"]; got != "/v1/responses" {
		t.Errorf("原有 path 字段不得丢：%v", got)
	}
}

// TestNoProviderAvailableLogsDistinguishUnavailableProviders 是对照：
// 模型有人支持却全不可用时，cause 必须**不是**模型问题——否则归因会反向误导排查。
func TestNoProviderAvailableLogsDistinguishUnavailableProviders(t *testing.T) {
	handler, logs := newNoProviderHandler(t, noProviderAdapter{
		clientFormat: "claude",
		context: route.DecisionContext{
			RequestedModel:          "deepseek-v4-flash",
			TotalProviders:          4,
			ModelSupportedProviders: 3,
			EnabledProviders:        3,
			AfterHealthCheck:        0,
			FilteredProviders: []route.Filtered{
				{ID: 1, Name: "p1", Reason: route.ReasonCircuitOpen},
				{ID: 2, Name: "p2", Reason: route.ReasonCircuitOpen},
				{ID: 3, Name: "p3", Reason: route.ReasonRateLimited},
			},
		},
	})

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"deepseek-v4-flash","max_tokens":8,"messages":[]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", "fake-client-key")
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码应为 503，收到 %d", recorder.Code)
	}
	event := findLogEvent(t, logs.String(), "dataplane.no_provider_available")
	if event == nil {
		t.Fatalf("应写出 dataplane.no_provider_available 事件，实际日志：%s", logs.String())
	}
	if got := event["cause"]; got != guard.NoProviderCauseProvidersUnavailable {
		t.Errorf("日志 cause = %v，期望 %q（模型有 3 家支持，不是模型问题）",
			got, guard.NoProviderCauseProvidersUnavailable)
	}
	if got, ok := event["modelSupportedProviders"].(float64); !ok || int(got) != 3 {
		t.Errorf("日志 modelSupportedProviders = %v，期望 3", event["modelSupportedProviders"])
	}
}

// findLogEvent 在 JSONL 日志里找指定 event 的最新一条，解析为 map。
func findLogEvent(t *testing.T, raw string, event string) map[string]any {
	t.Helper()
	var found map[string]any
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if !strings.Contains(line, `"event":"`+event+`"`) {
			continue
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			continue
		}
		found = parsed
	}
	return found
}
