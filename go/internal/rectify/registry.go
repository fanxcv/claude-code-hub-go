package rectify

import (
	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/specialsettings"
)

// descriptor 是一个被动整流器：检测 → 整流 → 幂等标记。
//
// 与 Node 的 ReactiveRectifierDescriptor（forwarder.ts:1244-1258）同构，只是把审计构造
// 交给调用方（Go 侧 provider 上下文在 forward 包，不属本包）。
type descriptor struct {
	typeName string
	detect   func(errorMessage string) string
	enabled  func(switches Switches) bool
	retried  func(state *RetryState) bool
	mark     func(state *RetryState)
	rectify  func(body *convert.Value) (map[string]any, bool)
}

// anthropicRectifiers 的顺序**即契约**：effort 必须在 signature 之前。
//
// Node 的注释（forwarder.ts:1378-1380）写明理由：effort 冲突的错误文案更具体
// （reasoning_effort/output_config），若排在后面会被 signature 的通用 invalid request 兜底吞掉。
var anthropicRectifiers = []descriptor{
	{
		typeName: specialsettings.TypeThinkingEffortConflictRectifier,
		detect:   detectThinkingEffortConflict,
		enabled:  func(s Switches) bool { return s.ThinkingEffortConflict },
		retried:  func(s *RetryState) bool { return s.ThinkingEffortConflict },
		mark:     func(s *RetryState) { s.ThinkingEffortConflict = true },
		rectify:  rectifyThinkingEffortConflict,
	},
	{
		typeName: specialsettings.TypeThinkingSignatureRectifier,
		detect:   detectThinkingSignature,
		enabled:  func(s Switches) bool { return s.ThinkingSignature },
		retried:  func(s *RetryState) bool { return s.ThinkingSignature },
		mark:     func(s *RetryState) { s.ThinkingSignature = true },
		rectify:  rectifyThinkingSignature,
	},
	{
		typeName: specialsettings.TypeThinkingBudgetRectifier,
		detect:   detectThinkingBudget,
		enabled:  func(s Switches) bool { return s.ThinkingBudget },
		retried:  func(s *RetryState) bool { return s.ThinkingBudget },
		mark:     func(s *RetryState) { s.ThinkingBudget = true },
		rectify:  rectifyThinkingBudget,
	},
}

// geminiRectifiers 目前只有一项。
var geminiRectifiers = []descriptor{
	{
		typeName: specialsettings.TypeGeminiFunctionIDRectifier,
		detect:   detectGeminiFunctionID,
		enabled:  func(s Switches) bool { return s.GeminiFunctionID },
		retried:  func(s *RetryState) bool { return s.GeminiFunctionID },
		mark:     func(s *RetryState) { s.GeminiFunctionID = true },
		rectify:  rectifyGeminiFunctionIDs,
	},
}

// Apply 复刻 Node 的 tryApplyReactiveRectifier（forwarder.ts:1396-1500）的核心判定。
//
// 三条与 Node 逐条对齐的语义，改错任一条都会让整流行为与上游实际报错对不上：
//  1. **命中即终结**：第一个 detect 命中的整流器就决定结果，不再往后看；
//  2. **命中但开关关闭 → 整链视为未命中**（不是「跳过它继续试下一个」）；
//  3. **已重试过 → Matched=true / Applied=false / already_retried**，调用方据此按不可重试终止。
//
// body 必须是**客户端正文对象**；Applied 为真时它已被原地整流。errorMessage 传上游错误文案。
//
// Node 侧另有两项前置排除（供应商局部模型缺口、400 存储容量故障）在调用方判定，
// 因为那要看 HTTP 状态与正文标记，不属于整流器本身。
func Apply(body *convert.Value, kind Kind, switches Switches, errorMessage string, state *RetryState) Result {
	registry := registryFor(kind)
	if registry == nil || body == nil || !body.IsObject() || state == nil {
		return Result{}
	}

	for _, candidate := range registry {
		trigger := candidate.detect(errorMessage)
		if trigger == "" {
			continue
		}
		if !candidate.enabled(switches) {
			return Result{}
		}
		if candidate.retried(state) {
			return Result{
				Matched: true,
				Applied: false,
				Reason:  ReasonAlreadyRetried,
				Type:    candidate.typeName,
				Trigger: trigger,
			}
		}

		fields, applied := candidate.rectify(body)
		if !applied {
			return Result{
				Matched: true,
				Applied: false,
				Reason:  ReasonNotApplicable,
				Type:    candidate.typeName,
				Trigger: trigger,
				Fields:  fields,
			}
		}
		candidate.mark(state)
		return Result{
			Matched: true,
			Applied: true,
			Type:    candidate.typeName,
			Trigger: trigger,
			Fields:  fields,
		}
	}
	return Result{}
}

// registryFor 按适用范围取注册表；KindOther 返回 nil（Node 同样返回 matched:false）。
func registryFor(kind Kind) []descriptor {
	switch kind {
	case KindAnthropic:
		return anthropicRectifiers
	case KindGemini:
		return geminiRectifiers
	default:
		return nil
	}
}
