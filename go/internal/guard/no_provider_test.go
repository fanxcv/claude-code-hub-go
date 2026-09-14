package guard

import (
	"errors"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
)

// TestNoProviderErrorKeepsErrNoProviderAvailable 钉住**向后兼容**：
// 改造前适配器直接返回 ErrNoProviderAvailable，既有调用方（dataplane 的 503 判定）
// 与既有用例都靠 errors.Is 认这条错误；换成带诊断的类型后必须仍然成立。
func TestNoProviderErrorKeepsErrNoProviderAvailable(t *testing.T) {
	err := NewNoProviderError(route.DecisionContext{RequestedModel: "dsf4", TotalProviders: 5}, "responses")
	if !errors.Is(err, ErrNoProviderAvailable) {
		t.Fatalf("errors.Is(err, ErrNoProviderAvailable) 必须仍为真，实际为假：%v", err)
	}
	// 错误文本必须仍以原句开头：日志/告警里既有对它的字符串匹配不会被这条改造打断。
	if !strings.HasPrefix(err.Error(), ErrNoProviderAvailable.Error()) {
		t.Fatalf("错误文本应以原句开头（既有匹配不受影响），实际 %q", err.Error())
	}
	// 并且必须能被 errors.As 取回（dataplane 用它取诊断字段写日志）。
	var noProvider *NoProviderError
	if !errors.As(err, &noProvider) {
		t.Fatal("errors.As 必须能取回 *NoProviderError")
	}
	if noProvider.Diagnostic().RequestedModel != "dsf4" {
		t.Fatalf("诊断里的模型名 = %q", noProvider.Diagnostic().RequestedModel)
	}
}

// TestNoProviderDiagnosticIdentifiesUnknownModel 是本任务的核心断言：
// 「模型名没人支持」必须能一眼认出来（用户报的 dsf4 就是这一形态）。
func TestNoProviderDiagnosticIdentifiesUnknownModel(t *testing.T) {
	context := route.DecisionContext{
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
	}
	diagnostic := NewNoProviderError(context, "responses").Diagnostic()

	if diagnostic.Cause != NoProviderCauseModelUnknown {
		t.Errorf("cause = %q，期望 %q", diagnostic.Cause, NoProviderCauseModelUnknown)
	}
	if !diagnostic.ModelMatchesNobody {
		t.Error("modelSupportedProviders=0 且候选非空时必须判为「模型无人支持」")
	}
	if diagnostic.TotalProviders != 9 || diagnostic.FilteredTotal != 3 {
		t.Errorf("计数不对：total=%d filtered=%d", diagnostic.TotalProviders, diagnostic.FilteredTotal)
	}
	if diagnostic.ReasonCounts[string(route.ReasonModelNotAllowed)] != 2 {
		t.Errorf("理由直方图不对：%v", diagnostic.ReasonCounts)
	}
	// 人可读的一行必须点名模型——运维 grep 到这一行就能定性，不必回头猜。
	if !strings.Contains(diagnostic.Summary(), "dsf4") {
		t.Errorf("summary 应点名模型名：%q", diagnostic.Summary())
	}
	if got := diagnostic.ReasonCountsLine(); got != "model_not_allowed=2,protocol_conversion_disabled=1" {
		t.Errorf("理由行 = %q（键需排序，便于日志比对）", got)
	}
	// 方言是适配器给的（留痕里的 targetType 粒度不同，不能替代）。
	if diagnostic.ClientFormat != "responses" {
		t.Errorf("clientFormat = %q", diagnostic.ClientFormat)
	}
}

// TestNoProviderDiagnosticSeparatesUnavailableProviders 钉住另一种成因：
// 模型有人支持，但供应商真不可用 —— 不得被判成「模型没人支持」，否则归因会反向误导。
func TestNoProviderDiagnosticSeparatesUnavailableProviders(t *testing.T) {
	context := route.DecisionContext{
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
	}
	diagnostic := NewNoProviderError(context, "claude").Diagnostic()

	if diagnostic.Cause != NoProviderCauseProvidersUnavailable {
		t.Errorf("cause = %q，期望 %q", diagnostic.Cause, NoProviderCauseProvidersUnavailable)
	}
	if diagnostic.ModelMatchesNobody {
		t.Error("有 3 家声明支持时不得判为「模型无人支持」")
	}
	if !strings.Contains(diagnostic.Summary(), "3 家") {
		t.Errorf("summary 应说明有几家声明支持：%q", diagnostic.Summary())
	}
}

// TestNoProviderDiagnosticWithoutCandidates 钉住「候选集本身为空」（例如分组下没有供应商）：
// 此时既不是模型问题也不是熔断问题，不得混进前两类。
func TestNoProviderDiagnosticWithoutCandidates(t *testing.T) {
	diagnostic := NewNoProviderError(route.DecisionContext{RequestedModel: "dsf4"}, "responses").Diagnostic()
	if diagnostic.Cause != NoProviderCauseNoProviderConfigured {
		t.Errorf("cause = %q，期望 %q", diagnostic.Cause, NoProviderCauseNoProviderConfigured)
	}
	if diagnostic.ModelMatchesNobody {
		t.Error("候选为空时不得判为「模型无人支持」（无从判断）")
	}
	if diagnostic.ReasonCounts != nil {
		t.Errorf("无过滤记录时直方图应为 nil（JSON 里不出现该键）：%v", diagnostic.ReasonCounts)
	}
}

// TestNoProviderDiagnosticWithPathBorneModel 钉住真进程上发现的形态：
// gemini 方言把模型名放在 URL 路径里（`/v1beta/models/{model}:...`），而代理只从**正文**取模型
// （dataplane 的 bodyModel），于是诊断拿到的是空模型名。
//
// 此时**不得**把它说成「0 家声明支持」——那是把「没查」写成「查过且没人支持」，正是本任务
// 要消灭的那种误诊。真进程实测（本地起 cchd + `/v1beta/models/...:generateContent`）：
// 日志里 model="" 、modelSupportedProviders=0、cause=providers_unavailable、
// reasonCounts="format_type_mismatch=362"，说明候选确实是被格式理由清空的。
func TestNoProviderDiagnosticWithPathBorneModel(t *testing.T) {
	context := route.DecisionContext{
		RequestedModel:          "", // 正文无 model
		TotalProviders:          362,
		ModelSupportedProviders: 0,
		EnabledProviders:        0,
		AfterHealthCheck:        0,
		FilteredProviders: []route.Filtered{
			{ID: 1, Name: "p1", Reason: route.ReasonFormatTypeMismatch},
		},
	}
	diagnostic := NewNoProviderError(context, "gemini").Diagnostic()

	if diagnostic.Cause != NoProviderCauseProvidersUnavailable {
		t.Errorf("cause = %q，期望 %q（模型未知时不得断言模型问题）",
			diagnostic.Cause, NoProviderCauseProvidersUnavailable)
	}
	if diagnostic.ModelMatchesNobody {
		t.Error("模型名未知时不得判为「模型无人支持」")
	}
	summary := diagnostic.Summary()
	if strings.Contains(summary, "声明支持") {
		t.Errorf("空模型名的摘要不得断言「有几家声明支持」：%q", summary)
	}
	if !strings.Contains(summary, "未带模型名") {
		t.Errorf("摘要应说明模型名取不到（可能模型在 URL 路径里）：%q", summary)
	}
}
