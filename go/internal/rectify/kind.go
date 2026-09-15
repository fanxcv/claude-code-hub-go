package rectify

import "github.com/fanxcv/claude-code-hub-go/go/internal/convert"

// Kind 是整流器的适用范围，对应 Node 按 providerType 分的两张注册表。
type Kind uint8

const (
	// KindOther 表示该供应商不属于任何一组：Node 对落到这里的 providerType 直接返回
	// `{matched:false}`（forwarder.ts:1418-1425）。
	KindOther Kind = iota
	// KindAnthropic 对应 providerType ∈ {claude, claude-auth}。
	KindAnthropic
	// KindGemini 对应 providerType ∈ {gemini, gemini-cli}。
	KindGemini
)

// KindOfProviderType 按 Node 的分组口径判定适用范围。
func KindOfProviderType(providerType convert.ProviderType) Kind {
	switch providerType {
	case convert.ProviderClaude, convert.ProviderClaudeAuth:
		return KindAnthropic
	case convert.ProviderGemini, convert.ProviderGeminiCLI:
		return KindGemini
	default:
		return KindOther
	}
}

// Switches 是六个 `enable_*_rectifier` 开关。
//
// Node 一律写作 `settings.x ?? true`：开关缺失时按**开启**处理。故零值结构体不代表 Node 默认值，
// 要用 NodeDefaults() 取值（调用方读设置失败时应回落到它，而不是回落零值）。
type Switches struct {
	ThinkingEffortConflict bool
	ThinkingSignature      bool
	ThinkingBudget         bool
	GeminiFunctionID       bool
	BillingHeader          bool
	ResponseInput          bool
}

// NodeDefaults 返回 Node 的缺省开关（全部开启）。
func NodeDefaults() Switches {
	return Switches{
		ThinkingEffortConflict: true,
		ThinkingSignature:      true,
		ThinkingBudget:         true,
		GeminiFunctionID:       true,
		BillingHeader:          true,
		ResponseInput:          true,
	}
}

// RetryState 是「同一供应商一轮仅重试一次」的幂等状态。
//
// Node 在每个供应商循环迭代开头重置它（forwarder.ts:1766-1772）：同一供应商只整流重试一次，
// 换供应商后可以对新供应商再整流一次。字段与 Node 的 ReactiveRectifierRetryState 逐项对应。
type RetryState struct {
	ThinkingEffortConflict bool
	ThinkingSignature      bool
	ThinkingBudget         bool
	GeminiFunctionID       bool
}

// 被动整流的未应用原因，取值与 Node 一致。
const (
	// ReasonAlreadyRetried：本供应商本轮已经为该整流器重试过一次。
	ReasonAlreadyRetried = "already_retried"
	// ReasonNotApplicable：触发词命中，但正文里没有可整流的东西（整流器自判不适用）。
	ReasonNotApplicable = "not_applicable"
)

// Result 是一次被动整流的判定结果。
//
// 语义与 Node 的 ReactiveRectifierResult 对齐：
//   - Matched=false：没有整流器命中触发词（或命中的整流器被开关关闭）→ 调用方按原失败处理；
//   - Matched=true, Applied=true：正文已被原地整流 → 调用方对**同一供应商**重试一次；
//   - Matched=true, Applied=false：命中但不可整流或已重试过 → 调用方按不可重试的客户端错误终止。
//
// Fields 是整流器特有字段（与 Node buildAuditSetting 的 payload 同名），
// Applied 为假时也要落审计（Node 在判定 applied 之前就写条目），故它此时同样非空。
type Result struct {
	Matched bool
	Applied bool
	Reason  string
	// Type 是审计 type（与 specialsettings 的六个常量同值）。
	Type string
	// Trigger 是命中的触发类型（与 Node 的 Trigger 联合类型同值）。
	Trigger string
	// Fields 是整流器特有字段。
	Fields map[string]any
}
