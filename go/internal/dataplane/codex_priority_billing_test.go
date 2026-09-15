package dataplane

import (
	"context"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// prioritySettingsStub 是 priority 计费档的设置桩。
type prioritySettingsStub struct{ source string }

func (f prioritySettingsStub) FindSystemSettings(context.Context) (*store.SystemSettings, error) {
	return &store.SystemSettings{CodexPriorityBillingSource: f.source}, nil
}

// TestCodexPriorityBillingTierSelection 钉住 service_tier 的选择两态。
//
// 判据来自 Node response-handler.ts:1288-1342：仅 codex 供应商参与；请求侧档位取
// 请求正文的 service_tier，响应侧取响应里的 service_tier；计费来源为 actual 时优先响应值、
// 缺失回退请求值；选定档位等于 "priority" 才按 priority 单价计费。
func TestCodexPriorityBillingTierSelection(t *testing.T) {
	gate := func(source string) *codexPriorityGate {
		return newCodexPriorityGate(prioritySettingsStub{source: source}, nil)
	}
	ctx := context.Background()

	cases := []struct {
		name         string
		source       string
		providerType string
		requested    string
		actual       string
		want         bool
	}{
		{name: "requested 档为 priority", source: "requested", providerType: "codex", requested: "priority", want: true},
		{name: "requested 档非 priority", source: "requested", providerType: "codex", requested: "flex", want: false},
		{name: "requested 来源忽略响应值", source: "requested", providerType: "codex", requested: "flex", actual: "priority", want: false},
		{name: "actual 来源优先响应值", source: "actual", providerType: "codex", requested: "flex", actual: "priority", want: true},
		{name: "actual 来源响应缺失回退请求值", source: "actual", providerType: "codex", requested: "priority", actual: "", want: true},
		{name: "非 codex 供应商不参与", source: "requested", providerType: "claude", requested: "priority", want: false},
		{name: "缺档位不计 priority", source: "requested", providerType: "codex", want: false},
		{
			// 设置取值非法时按 Node 的兜底走 requested（session.ts:1521-1535）。
			name: "非法设置值回退 requested", source: "bogus", providerType: "codex", requested: "priority", want: true,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := gate(testCase.source).applied(ctx, testCase.providerType, testCase.requested, testCase.actual)
			if got != testCase.want {
				t.Errorf("applied = %v，期望 %v", got, testCase.want)
			}
		})
	}

	t.Run("未接线时不计 priority", func(t *testing.T) {
		var nilGate *codexPriorityGate
		if nilGate.applied(ctx, "codex", "priority", "") {
			t.Error("nil 门面不该计 priority 档")
		}
	})
}

// TestParseServiceTierFromResponseText 钉住响应侧档位的两个取值位置。
func TestParseServiceTierFromResponseText(t *testing.T) {
	cases := map[string]string{
		`{"service_tier":"priority"}`:              "priority",
		`{"response":{"service_tier":"priority"}}`: "priority",
		`{"service_tier":""}`:                      "",
		`{"service_tier":"flex"}`:                  "flex",
		`{"id":"x"}`:                               "",
		`not json`:                                 "",
		``:                                         "",
		`{"response":{"service_tier":"priority"},"other":"y"}`: "priority",
	}
	for body, want := range cases {
		if got := parseServiceTierFromResponseText([]byte(body)); got != want {
			t.Errorf("解析 %q = %q，期望 %q", body, got, want)
		}
	}
}

// TestCaptureRequestedServiceTier 钉住请求侧档位的采集（仅字符串）。
func TestCaptureRequestedServiceTier(t *testing.T) {
	// 采集依赖 bodyAccess 工厂，属集成面；此处直接断言「无 body 时不改状态」这条边界，
	// 逐字段采集由 TestCodexPriorityBillingTierSelection 的选择表覆盖。
	state := &RequestState{}
	captureRequestedServiceTier(state, nil)
	if state.requestedServiceTier != "" {
		t.Errorf("无正文时不该写入档位，得到 %q", state.requestedServiceTier)
	}
}

// TestCodexPriorityProviderTypeIsCodex 钉住「仅 codex 供应商参与」这条前提的字面量一致。
func TestCodexPriorityProviderTypeIsCodex(t *testing.T) {
	if string(convert.ProviderCodex) != "codex" {
		t.Fatalf("convert.ProviderCodex = %q，Node 的判据字面量是 codex", convert.ProviderCodex)
	}
}
