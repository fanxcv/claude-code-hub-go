package guard

import (
	"fmt"
	"sort"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
)

// 「无可用供应商」的归因类别。取值进日志字段 `cause`，供运维聚合与告警。
const (
	// NoProviderCauseModelUnknown：没有任何供应商声明支持请求的模型（模型名不认识）。
	NoProviderCauseModelUnknown = "model_matches_no_provider"
	// NoProviderCauseProvidersUnavailable：模型是已知的，但候选被熔断/限流/分组等剔除干净。
	NoProviderCauseProvidersUnavailable = "providers_unavailable"
	// NoProviderCauseNoProviderConfigured：候选集本身就是空的（例如该分组下没有供应商）。
	NoProviderCauseNoProviderConfigured = "no_provider_configured"
)

// NoProviderDiagnostic 是「无可用供应商」的可诊断事实。
//
// 为何需要它：`503 no_available_providers` 的响应体**逐字节一致**（Node 契约，不得改动），
// 但它的成因可以完全不同——
//
//	① 模型名不认识：没有任何供应商的 allowed_models 声明支持它（用户报的 `dsf4` 即此）。
//	   读起来却像「供应商全挂了」，用户与运维都会往错的方向排查。
//	② 供应商真不可用：熔断 / 限流 / 分组不匹配 / 被排除。
//
// 选路**本来就分得清**这两种（每家被剔除时都记了 Reason，见 route.Filtered），但这些事实
// 在「Provider == nil」的返回路径上被丢掉了。本类型把事实带过适配器边界，交给日志。
//
// 它**不参与任何响应**，也不是落库结构：唯一消费者是日志。
type NoProviderDiagnostic struct {
	// RequestedModel 是客户端请求的模型名（原样，不做规范化）。
	RequestedModel string `json:"requestedModel,omitempty"`
	// ClientFormat 是识别出的客户端方言（claude / responses / openai / gemini …）。
	ClientFormat string `json:"clientFormat,omitempty"`
	// TotalProviders 与 ModelSupportedProviders 的统计范围相同（分组过滤之后的家数）。
	TotalProviders int `json:"totalProviders"`
	// ModelSupportedProviders 是其中「声明支持该模型」的家数；0 即模型无人支持。
	ModelSupportedProviders int `json:"modelSupportedProviders"`
	// ModelMatchesNobody 为真表示「模型没有任何供应商声明支持」——即成因 ①。
	ModelMatchesNobody bool `json:"modelMatchesNobody"`
	// Cause 是归因类别（三个常量之一），便于按值聚合。
	Cause string `json:"cause"`
	// EnabledProviders / AfterHealthCheck 是基础过滤与健康过滤后的家数（Node 同口径计数）。
	EnabledProviders int `json:"enabledProviders"`
	AfterHealthCheck int `json:"afterHealthCheck"`
	// FilteredTotal 是被剔除的条目数；ReasonCounts 是各理由的条数（理由取值与 Node 同词）。
	FilteredTotal int            `json:"filteredTotal"`
	ReasonCounts  map[string]int `json:"reasonCounts,omitempty"`
}

// Summary 给出一行中文归因，供人直接读日志（不改响应文本）。
func (d NoProviderDiagnostic) Summary() string {
	// 模型名可能取不到：代理只从**正文**取模型（dataplane 的 bodyModel），而 gemini 一类的
	// 模型名在 URL 路径里（`/v1beta/models/{model}:...`）。此时不得写成「0 家声明支持」
	// ——那是把「没查」说成「查过了且没人支持」，正是本任务要消灭的误诊。
	if d.RequestedModel == "" {
		switch d.Cause {
		case NoProviderCauseNoProviderConfigured:
			return fmt.Sprintf("候选集为空（分组内的供应商数为 %d），且请求未带模型名（正文无 model，可能模型在 URL 路径里）",
				d.TotalProviders)
		default:
			return fmt.Sprintf("请求未带模型名（正文无 model，可能模型在 URL 路径里）→ 未评估模型覆盖；候选在基础过滤后剩 %d 家、健康过滤后剩 %d 家",
				d.EnabledProviders, d.AfterHealthCheck)
		}
	}
	switch d.Cause {
	case NoProviderCauseModelUnknown:
		return fmt.Sprintf("模型 %q 没有任何供应商声明支持（已查 %d 家，允许集均不含它）",
			d.RequestedModel, d.TotalProviders)
	case NoProviderCauseNoProviderConfigured:
		return fmt.Sprintf("候选集为空（分组内的供应商数为 %d）", d.TotalProviders)
	default:
		return fmt.Sprintf("模型 %q 有 %d 家供应商声明支持，但候选已被熔断/限流等剔除（基础过滤后 %d 家、健康过滤后 %d 家）",
			d.RequestedModel, d.ModelSupportedProviders, d.EnabledProviders, d.AfterHealthCheck)
	}
}

// NoProviderError 把 ErrNoProviderAvailable 与诊断事实一起带走。
//
// Unwrap 指向 ErrNoProviderAvailable，故 `errors.Is(err, ErrNoProviderAvailable)` 行为
// 与改造前完全一致（既有调用方与用例无需改动）。
type NoProviderError struct {
	diagnostic NoProviderDiagnostic
}

func (e *NoProviderError) Error() string {
	return ErrNoProviderAvailable.Error() + "：" + e.diagnostic.Summary()
}

// Unwrap 让 errors.Is(err, ErrNoProviderAvailable) 继续成立。
func (e *NoProviderError) Unwrap() error { return ErrNoProviderAvailable }

// Diagnostic 返回归因事实（调用方据此写日志）。
func (e *NoProviderError) Diagnostic() NoProviderDiagnostic { return e.diagnostic }

// NewNoProviderError 从选路留痕构造带归因的无可用供应商错误。
//
// clientFormat 由调用方给出（适配器知道本次请求的方言；留痕里的 TargetType 是归一后的
// 目标族，粒度不同，不能互相替代）。
func NewNoProviderError(context route.DecisionContext, clientFormat string) *NoProviderError {
	counts := make(map[string]int, len(context.FilteredProviders))
	for _, record := range context.FilteredProviders {
		counts[string(record.Reason)]++
	}
	if len(counts) == 0 {
		counts = nil
	}

	diagnostic := NoProviderDiagnostic{
		RequestedModel:          context.RequestedModel,
		ClientFormat:            clientFormat,
		TotalProviders:          context.TotalProviders,
		ModelSupportedProviders: context.ModelSupportedProviders,
		EnabledProviders:        context.EnabledProviders,
		AfterHealthCheck:        context.AfterHealthCheck,
		FilteredTotal:           len(context.FilteredProviders),
		ReasonCounts:            counts,
	}
	switch {
	case context.RequestedModel != "" && context.TotalProviders > 0 &&
		context.ModelSupportedProviders == 0:
		diagnostic.ModelMatchesNobody = true
		diagnostic.Cause = NoProviderCauseModelUnknown
	case context.TotalProviders == 0:
		diagnostic.Cause = NoProviderCauseNoProviderConfigured
	default:
		diagnostic.Cause = NoProviderCauseProvidersUnavailable
	}
	return &NoProviderError{diagnostic: diagnostic}
}

// ReasonCountsLine 把理由直方图渲染成「理由=条数」的稳定字符串（键排序，便于日志比对）。
func (d NoProviderDiagnostic) ReasonCountsLine() string {
	if len(d.ReasonCounts) == 0 {
		return ""
	}
	keys := make([]string, 0, len(d.ReasonCounts))
	for key := range d.ReasonCounts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, d.ReasonCounts[key]))
	}
	return strings.Join(parts, ",")
}
