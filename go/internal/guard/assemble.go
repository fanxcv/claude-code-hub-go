package guard

import (
	"errors"
	"fmt"
)

// ErrMissingDependency 表示链条需要的缝隙没有实现方。
//
// 为什么构造期就要报错：缝隙为 nil 时多数守卫按 Node 的 fail-open 语义跳过并留一条 warn，
// 于是「忘了接线」在运行期的表现是「闸门悄悄不存在」——鉴权、模型白名单、敏感词都可能被
// 静默跳过。构造期失败把这件事挪到启动时刻。
var ErrMissingDependency = errors.New("guard: 守卫链缺少必需的依赖")

// Policy 是端点策略中与守卫预设有关的部分，对应 Node 侧 EndpointPolicy。
type Policy struct {
	// Preset 取 chat 或 raw_passthrough。
	Preset Preset
	// RequestType 区分对话与 count_tokens（后者走安全会话链）。
	RequestType RequestType
	// RawCrossProviderFallback 是「原始端点允许跨供应商回退」的当前取值。它由 session
	// 守卫在更早的步骤里按系统设置写回，因此必须在构造链条时从外部传入。
	RawCrossProviderFallback bool
}

// ChatPolicy 是普通对话端点的默认策略。
func ChatPolicy() Policy {
	return Policy{Preset: PresetChat, RequestType: RequestTypeChat}
}

// dependency 描述一条缝隙在链条中的最低要求。
type dependency struct {
	// name 是 Deps 字段名，报错文案直接给字段名，接线方不必猜是哪里缺了。
	name string
	// present 报告该依赖是否已接线。
	present func(Deps) bool
	// requiredIn 列出「缺它就不可接受」的预设名。
	requiredIn map[string]bool
}

// requiredDependencies 是「缺失即拒绝构造」的缝隙表。
//
// 判定标准：拿掉它就会静默少掉一道闸、或让某一步必然退化，而不是「功能暂时不可用」。
// 因此：
//   - Auth / Users / Provider / MessageContext 在**所有**链上必需：鉴权、白名单、选路、
//     请求日志是链的存在理由。
//   - Sensitive / Filters 只在对答链上必需：它们承载内容与规则闸门；原始透传链不含这两步。
//   - Settings 只在对答链上必需：session/warmup/version 三步按它决定行为。
//   - Body 在**含正文判定**的链上必需（model、probe、session、warmup、filter、sensitive 都要它）。
//   - 其余（IP、Versions、Sessions、WarmupLog、BlockedLog、AuthThrottle、RateLimit、Replay、
//     ExpiryMarker、ProviderGroupTag）缺失时按各自已写明的语义退化，属「本波未接线」，
//     不阻断构造：硬校验它们会让 Go 无法按路由灰度。
var requiredDependencies = []dependency{
	{name: "Auth", present: func(d Deps) bool { return d.Auth != nil }, requiredIn: allPipelines()},
	{name: "Users", present: func(d Deps) bool { return d.Users != nil }, requiredIn: allPipelines()},
	{name: "Provider", present: func(d Deps) bool { return d.Provider != nil }, requiredIn: allPipelines()},
	{
		name:    "MessageContext",
		present: func(d Deps) bool { return d.MessageContext != nil },
		// 原始透传链不含 messageContext 步骤，故只在这两条链上必需。
		requiredIn: map[string]bool{SafeSessionChain: true, ChatChain: true},
	},
	{name: "Settings", present: func(d Deps) bool { return d.Settings != nil }, requiredIn: chatOnly()},
	{name: "Sensitive", present: func(d Deps) bool { return d.Sensitive != nil }, requiredIn: chatOnly()},
	{name: "Filters", present: func(d Deps) bool { return d.Filters != nil }, requiredIn: chatOnly()},
	{name: "Body", present: func(d Deps) bool { return d.Body != nil }, requiredIn: chatOnly()},
}

// 链名常量：Preset 与 Pipeline.Name 是两套取值，依赖表按链条名（更细）索引。
const (
	// PassthroughChain 对应 RAW_PASSTHROUGH_PIPELINE。
	PassthroughChain = "RAW_PASSTHROUGH_PIPELINE"
	// SafeSessionChain 对应 RAW_SAFE_SESSION_PIPELINE 与 COUNT_TOKENS_PIPELINE。
	SafeSessionChain = "RAW_SAFE_SESSION_PIPELINE"
	// ChatChain 对应 CHAT_PIPELINE。
	ChatChain = "CHAT_PIPELINE"
)

// allPipelines 是「所有链都必需」的集合。
func allPipelines() map[string]bool {
	return map[string]bool{ChatChain: true, PassthroughChain: true, SafeSessionChain: true}
}

// chatOnly 是「只有完整对话链必需」的集合。
func chatOnly() map[string]bool {
	return map[string]bool{ChatChain: true}
}

// Assemble 按端点策略组装一条可运行的守卫链。
//
// 两个职责：把策略翻译成预设（复用 chain.go 的既有映射，不在此重写顺序），并在构造期
// 校验必需缝隙。步骤顺序仍由 Pipeline 常量唯一决定——本函数不得重排。
func Assemble(deps Deps, policy Policy) (*Chain, error) {
	pipeline, err := pipelineFor(policy)
	if err != nil {
		return nil, err
	}
	if err := validateDependencies(deps, pipeline); err != nil {
		return nil, err
	}
	return deps.FromPipeline(pipeline)
}

// pipelineFor 复刻 GuardPipelineBuilder：先按端点类型定预设，再按预设选链。
func pipelineFor(policy Policy) (Pipeline, error) {
	switch policy.Preset {
	case PresetChat, "":
		if policy.RequestType == RequestTypeCountTokens {
			// count_tokens 走安全会话链（TS 侧 COUNT_TOKENS_PIPELINE 直接复用）。
			return CountTokensPipeline, nil
		}
		return ChatPipeline, nil
	case PresetRawPassthrough:
		if policy.RawCrossProviderFallback {
			return RawSafeSessionPipeline, nil
		}
		return RawPassthroughPipeline, nil
	default:
		return Pipeline{}, fmt.Errorf("guard: 未知的端点预设 %q", policy.Preset)
	}
}

// validateDependencies 校验本条链的必需缝隙。
func validateDependencies(deps Deps, pipeline Pipeline) error {
	for _, item := range requiredDependencies {
		if !item.requiredIn[pipeline.Name] {
			continue
		}
		if item.present(deps) {
			continue
		}
		return fmt.Errorf("%w: 链条 %s 需要 %s", ErrMissingDependency, pipeline.Name, item.name)
	}
	return nil
}

// ChainSteps 返回链条的实际步骤顺序，供接线自检与排障打印。
func ChainSteps(chain *Chain) []StepKey {
	if chain == nil {
		return nil
	}
	steps := make([]StepKey, len(chain.Steps))
	copy(steps, chain.Steps)
	return steps
}

// stepSequence 把链条的步骤顺序渲染成 "auth -> sensitive -> ..." 形式。
//
// 存在的理由：顺序是契约（错误优先级与计费时点都由它决定），排障时需要一眼看出实际跑的
// 是不是以为的那条链。
func stepSequence(chain *Chain) string {
	if chain == nil {
		return ""
	}
	rendered := ""
	for index, key := range chain.Steps {
		if index > 0 {
			rendered += " -> "
		}
		rendered += string(key)
	}
	return rendered
}
