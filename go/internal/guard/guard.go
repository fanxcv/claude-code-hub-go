// Package guard 复刻 src/app/v1/_lib/proxy/guard-pipeline.ts 的守卫预设。
//
// 本包只承载「顺序」这一事实：真实守卫逻辑在后续里程碑按包落地，但预设一经写定
// 就不可重排——顺序变化会改变错误优先级与计费时点，属于语义变更。
package guard

// StepKey 是守卫步骤的键，取值与 TS 侧 GuardStepKey 逐字一致。
type StepKey string

const (
	StepAuth                  StepKey = "auth"
	StepClient                StepKey = "client"
	StepModel                 StepKey = "model"
	StepVersion               StepKey = "version"
	StepProbe                 StepKey = "probe"
	StepSession               StepKey = "session"
	StepWarmup                StepKey = "warmup"
	StepRequestFilter         StepKey = "requestFilter"
	StepSensitive             StepKey = "sensitive"
	StepReplayAttach          StepKey = "replayAttach"
	StepRateLimit             StepKey = "rateLimit"
	StepProvider              StepKey = "provider"
	StepProviderRequestFilter StepKey = "providerRequestFilter"
	StepMessageContext        StepKey = "messageContext"
)

// RequestType 对齐 TS 侧 RequestType 枚举。
type RequestType string

const (
	RequestTypeChat        RequestType = "CHAT"
	RequestTypeCountTokens RequestType = "COUNT_TOKENS"
)

// Pipeline 是一个有序的守卫链。
type Pipeline struct {
	// Name 是预设名，用于日志与断言。
	Name string
	// Steps 顺序即执行顺序，不得重排。
	Steps []StepKey
}

// ChatPipeline 是普通对话请求的完整守卫链，对应 CHAT_PIPELINE。
var ChatPipeline = Pipeline{
	Name: "CHAT_PIPELINE",
	Steps: []StepKey{
		StepAuth,
		StepSensitive,
		StepClient,
		StepModel,
		StepVersion,
		StepProbe,
		StepSession,
		StepWarmup,
		StepRequestFilter,
		StepReplayAttach,
		StepRateLimit,
		StepProvider,
		StepProviderRequestFilter,
		StepMessageContext,
	},
}

// RawPassthroughPipeline 对应 RAW_PASSTHROUGH_PIPELINE。
var RawPassthroughPipeline = Pipeline{
	Name: "RAW_PASSTHROUGH_PIPELINE",
	Steps: []StepKey{
		StepAuth,
		StepClient,
		StepModel,
		StepVersion,
		StepProbe,
		StepProvider,
	},
}

// RawSafeSessionPipeline 对应 RAW_SAFE_SESSION_PIPELINE。
var RawSafeSessionPipeline = Pipeline{
	Name: "RAW_SAFE_SESSION_PIPELINE",
	Steps: []StepKey{
		StepAuth,
		StepClient,
		StepModel,
		StepVersion,
		StepProbe,
		StepSession,
		StepProvider,
		StepMessageContext,
	},
}

// CountTokensPipeline 对应 COUNT_TOKENS_PIPELINE，TS 侧直接复用安全会话链。
var CountTokensPipeline = RawSafeSessionPipeline

// allStepKeys 按 TS 侧联合类型的书写顺序列出全部步骤键。
func allStepKeys() []StepKey {
	return []StepKey{
		StepAuth,
		StepClient,
		StepModel,
		StepVersion,
		StepProbe,
		StepSession,
		StepWarmup,
		StepRequestFilter,
		StepSensitive,
		StepReplayAttach,
		StepRateLimit,
		StepProvider,
		StepProviderRequestFilter,
		StepMessageContext,
	}
}

// FromRequestType 复刻 GuardPipelineBuilder.fromRequestType：count_tokens 走安全会话链，
// 其余走完整对话链。
func FromRequestType(requestType RequestType) Pipeline {
	if requestType == RequestTypeCountTokens {
		return CountTokensPipeline
	}
	return ChatPipeline
}
