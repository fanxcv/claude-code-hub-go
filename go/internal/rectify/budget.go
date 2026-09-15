package rectify

import (
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// triggerBudgetTokensTooLow 是 thinking 预算整流器的唯一触发类型。
const triggerBudgetTokensTooLow = "budget_tokens_too_low"

// 与 Node thinking-budget-rectifier.ts:14-16 的三个常量同值。
const (
	maxThinkingBudget     = 32000
	maxTokensValue        = 64000
	minMaxTokensForBudget = maxThinkingBudget + 1
)

// detectThinkingBudget 复刻 detectThinkingBudgetRectifierTrigger
// （thinking-budget-rectifier.ts:23-44）：小写化包含匹配，不做错误规则依赖。
func detectThinkingBudget(errorMessage string) string {
	if errorMessage == "" {
		return ""
	}
	lower := strings.ToLower(errorMessage)

	hasBudgetTokensReference := strings.Contains(lower, "budget_tokens") || strings.Contains(lower, "budget tokens")
	hasThinkingReference := strings.Contains(lower, "thinking")
	has1024Constraint := strings.Contains(lower, "greater than or equal to 1024") ||
		strings.Contains(lower, ">= 1024") ||
		(strings.Contains(lower, "1024") && strings.Contains(lower, "input should be"))

	if hasBudgetTokensReference && hasThinkingReference && has1024Constraint {
		return triggerBudgetTokensTooLow
	}
	return ""
}

// rectifyThinkingBudget 复刻 rectifyThinkingBudget（thinking-budget-rectifier.ts:46-112）：
// adaptive 不动；否则置 thinking.type=enabled、budget_tokens=32000，并在 max_tokens 缺失或
// 小于 32001 时把它抬到 64000。原地修改，返回审计字段（before/after 三件套）。
func rectifyThinkingBudget(body *convert.Value) (map[string]any, bool) {
	currentMaxTokens, hasMaxTokens := jsonNumber(body, "max_tokens")

	thinking := body.ObjectField("thinking")
	currentThinkingType := ""
	if thinking != nil {
		if raw, ok := thinking.StringField("type"); ok {
			currentThinkingType = raw
		}
	}

	if currentThinkingType == "adaptive" {
		before := budgetSnapshot(currentMaxTokens, hasMaxTokens, currentThinkingType, thinking)
		return map[string]any{"before": before, "after": before}, false
	}

	currentBudget, hasBudget := jsonNumber(thinking, "budget_tokens")
	before := budgetSnapshotWith(currentMaxTokens, hasMaxTokens, currentThinkingType, currentBudget, hasBudget)

	if thinking == nil {
		thinking = convert.NewObject()
		body.Set("thinking", thinking)
	}
	thinking.Set("type", convert.NewString("enabled"))
	thinking.Set("budget_tokens", convert.NewNumberInt(maxThinkingBudget))

	if !hasMaxTokens || currentMaxTokens < minMaxTokensForBudget {
		body.Set("max_tokens", convert.NewNumberInt(maxTokensValue))
	}

	afterMaxTokens, hasAfterMax := jsonNumber(body, "max_tokens")
	afterType, _ := thinking.StringField("type")
	afterBudget, hasAfterBudget := jsonNumber(thinking, "budget_tokens")
	after := budgetSnapshotWith(afterMaxTokens, hasAfterMax, afterType, afterBudget, hasAfterBudget)

	applied := before["maxTokens"] != after["maxTokens"] ||
		before["thinkingType"] != after["thinkingType"] ||
		before["thinkingBudgetTokens"] != after["thinkingBudgetTokens"]
	return map[string]any{"before": before, "after": after}, applied
}

// budgetSnapshot 取「thinking 缺失」时的快照（Node 的 early-return 分支）。
func budgetSnapshot(maxTokens int64, hasMaxTokens bool, thinkingType string, thinking *convert.Value) map[string]any {
	budget, hasBudget := jsonNumber(thinking, "budget_tokens")
	return budgetSnapshotWith(maxTokens, hasMaxTokens, thinkingType, budget, hasBudget)
}

// budgetSnapshotWith 组一组审计快照；缺失值一律为 null（Node 的 `number | null`）。
func budgetSnapshotWith(maxTokens int64, hasMaxTokens bool, thinkingType string, budget int64, hasBudget bool) map[string]any {
	snapshot := map[string]any{
		"maxTokens":            nil,
		"thinkingType":         nilIfEmpty(thinkingType),
		"thinkingBudgetTokens": nil,
	}
	if hasMaxTokens {
		snapshot["maxTokens"] = maxTokens
	}
	if hasBudget {
		snapshot["thinkingBudgetTokens"] = budget
	}
	return snapshot
}

// jsonNumber 取字段的整数值；缺失或非数字返回 (0,false)——对应 Node 的 `typeof x === "number"`。
func jsonNumber(value *convert.Value, key string) (int64, bool) {
	if value == nil {
		return 0, false
	}
	child, ok := value.Get(key)
	if !ok {
		return 0, false
	}
	return child.Int64()
}
