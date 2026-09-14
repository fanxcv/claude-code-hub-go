package guard

import (
	"errors"
	"fmt"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// ErrUnknownStep 表示预设里出现了没有实现的步骤键。
var ErrUnknownStep = errors.New("guard: 未实现的守卫步骤")

// Step 是守卫链的一步。
//
// 与 Node 侧的 GuardStep.execute 一一对应：返回非 nil 响应即早退，返回 error 表示
// 步骤自身失败（调用方翻译为 500，而不是静默放行）。
type Step func(ctx *pctx.Context) (*Response, error)

// StepIndex 是步骤键到实现的映射，对应 Node 侧的 Steps 常量表。
type StepIndex map[StepKey]Step

// namedStep 是链条中的一步：键用于日志与排障，run 是实际行为。
type namedStep struct {
	key StepKey
	run Step
}

// Chain 是一条按序执行的守卫链。
type Chain struct {
	// Name 是预设名（CHAT_PIPELINE 等），用于日志与断言。
	Name string
	// Steps 是按序展开的步骤键，供接线与自检比对。
	Steps []StepKey
	steps []namedStep
}

// Build 按预设顺序组装链条。
//
// 索引里缺键时返回错误而不是跳过：静默跳过会让某条预设悄悄少一道闸，这正是 Node 侧
// Steps[k] 取到 undefined 会崩掉的原因——崩掉比丢闸好。
func Build(pipeline Pipeline, index StepIndex) (*Chain, error) {
	chain := &Chain{
		Name:  pipeline.Name,
		Steps: make([]StepKey, 0, len(pipeline.Steps)),
		steps: make([]namedStep, 0, len(pipeline.Steps)),
	}
	for _, key := range pipeline.Steps {
		step, ok := index[key]
		if !ok || step == nil {
			return nil, fmt.Errorf("%w: 预设 %s 的步骤 %q 没有实现", ErrUnknownStep, pipeline.Name, key)
		}
		chain.Steps = append(chain.Steps, key)
		chain.steps = append(chain.steps, namedStep{key: key, run: step})
	}
	return chain, nil
}

// Run 顺序执行链条：任一步返回响应即短路返回，返回 error 即中断。
func (c *Chain) Run(ctx *pctx.Context) (*Response, error) {
	for _, step := range c.steps {
		response, err := step.run(ctx)
		if err != nil {
			return nil, fmt.Errorf("守卫步骤 %s 失败: %w", step.key, err)
		}
		if response != nil {
			return response, nil
		}
	}
	return nil, nil
}

// Preset 是端点的守卫预设，对应 Node 侧 EndpointPolicy.guardPreset 的取值。
type Preset string

const (
	// PresetChat 是普通对话端点。
	PresetChat Preset = "chat"
	// PresetRawPassthrough 是原始透传端点。
	PresetRawPassthrough Preset = "raw_passthrough"
)

// EndpointAllowsRetryAndSwitch 报告该预设的端点策略是否允许重试与供应商切换。
//
// 依据 Node 的 endpoint-policy.ts:24-27（default：两者皆 true）与 :39-42
// （raw_passthrough：两者皆 false）。这两个布尔是流式竞速的前置条件之一
// （forwarder.ts:4915-4920），判定必须与守卫链取同一个来源，不能各写一份。
func EndpointAllowsRetryAndSwitch(preset Preset) bool {
	return preset != PresetRawPassthrough
}

// FromEndpointPolicy 复刻 GuardPipelineBuilder.fromEndpointPolicy。
//
// 注意 raw_passthrough 的分支：只有「原始跨供应商回退」开启时才升级为安全会话链，
// 该开关由 session 守卫在更早的步骤里按系统设置写回，因此这里必须接受外部传入的取值。
func (d Deps) FromEndpointPolicy(preset Preset, rawCrossProviderFallbackEnabled bool) (*Chain, error) {
	if preset == PresetRawPassthrough {
		if rawCrossProviderFallbackEnabled {
			return d.FromPipeline(RawSafeSessionPipeline)
		}
		return d.FromPipeline(RawPassthroughPipeline)
	}
	return d.FromPipeline(ChatPipeline)
}

// FromRequestType 复刻 GuardPipelineBuilder.fromRequestType。
func (d Deps) FromRequestType(requestType RequestType) (*Chain, error) {
	if requestType == RequestTypeCountTokens {
		return d.FromPipeline(CountTokensPipeline)
	}
	return d.FromPipeline(ChatPipeline)
}

// FromPipeline 按给定预设组装链条。
func (d Deps) FromPipeline(pipeline Pipeline) (*Chain, error) {
	return Build(pipeline, d.Steps())
}
