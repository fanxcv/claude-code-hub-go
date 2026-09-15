package rectify

import (
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// triggerThinkingDisabledWithReasoningEffort 是 effort 冲突整流器的唯一触发类型。
const triggerThinkingDisabledWithReasoningEffort = "thinking_disabled_with_reasoning_effort"

// detectThinkingEffortConflict 复刻 detectThinkingEffortConflictRectifierTrigger
// （thinking-effort-conflict-rectifier.ts:26-58）：小写化包含匹配，不做错误规则依赖。
func detectThinkingEffortConflict(errorMessage string) string {
	if errorMessage == "" {
		return ""
	}
	lower := strings.ToLower(errorMessage)

	if !strings.Contains(lower, "cannot be disabled") && !strings.Contains(lower, "can not be disabled") {
		return ""
	}
	// DeepSeek 原文：thinking options type cannot be disabled when reasoning_effort is set
	if strings.Contains(lower, "reasoning_effort") {
		return triggerThinkingDisabledWithReasoningEffort
	}
	// 变体兜底：以 output_config(.effort) 表述同一冲突的上游
	if strings.Contains(lower, "output_config") && strings.Contains(lower, "thinking") {
		return triggerThinkingDisabledWithReasoningEffort
	}
	return ""
}

// rectifyThinkingEffortConflict 复刻 rectifyThinkingEffortConflict
// （thinking-effort-conflict-rectifier.ts:60-133）：**原地**改正文，返回审计字段。
//
// 行为要点：仅在 thinking 关闭（缺失或 "disabled"）时生效；剥 output_config.effort（其余兄弟键保留，
// 剥空则整体删对象）；同时删顶层 reasoning_effort 透传字段；thinking 的关闭状态保持不动。
func rectifyThinkingEffortConflict(body *convert.Value) (map[string]any, bool) {
	thinking := body.ObjectField("thinking")
	thinkingType := ""
	if thinking != nil {
		if raw, ok := thinking.StringField("type"); ok {
			thinkingType = raw
		}
	}

	fields := map[string]any{
		"removedOutputConfigEffort": false,
		"removedReasoningEffort":    false,
		"thinkingType":              nilIfEmpty(thinkingType),
		"effort":                    nil,
	}

	// thinking 显式启用（enabled/adaptive 等）时不属该冲突，保持原样。
	// Node 的判定是 thinkingType === null || thinkingType === "disabled"。
	if thinkingType != "" && thinkingType != "disabled" {
		return fields, false
	}

	applied := false
	effort := ""

	if outputConfig := body.ObjectField("output_config"); outputConfig != nil {
		if effortValue, ok := outputConfig.Get("effort"); ok {
			if raw, isString := effortValue.String(); isString {
				effort = raw
			}
			outputConfig.Delete("effort")
			if len(outputConfig.Members()) > 0 {
				body.Set("output_config", outputConfig)
			} else {
				body.Delete("output_config")
			}
			fields["removedOutputConfigEffort"] = true
			applied = true
		}
	}

	if _, ok := body.Get("reasoning_effort"); ok {
		if effort == "" {
			if raw, isString := body.StringField("reasoning_effort"); isString {
				effort = raw
			}
		}
		body.Delete("reasoning_effort")
		fields["removedReasoningEffort"] = true
		applied = true
	}

	if effort != "" {
		fields["effort"] = effort
	}
	return fields, applied
}

// nilIfEmpty 把空串折成 nil：审计里该字段是 `string | null`。
func nilIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}
