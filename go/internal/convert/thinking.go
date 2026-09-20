package convert

import "fmt"

// 思考强度代数：三条线的强度载体不同，跨线时需要双向换算。
//
// 载体矩阵（Hub 契约 §2.1 / 能力矩阵审计 §2.6）：
//
//	线                    token 预算载体                             等级载体
//	anthropic-messages    thinking.{type:enabled,budget_tokens}     output_config.effort
//	openai-chat           —— 无                                    reasoning_effort（顶层）
//	openai-responses      —— 无                                    reasoning.effort
//
// 为何需要本模块：客户端只给预算、而目标是只有等级的那两条线时，旧实现把预算**直接丢弃**
// （`codec_chat.go` 的 `reasoning.budget_tokens` 损失；Node 亦然，`openai-chat/request.ts:404-405`），
// 用户表现为「claude 客户端带思考预算打 openai 上游，思考强度整个消失」。
//
// 纪律（三条，缺一不可）：
//  1. **不凭空发明**：只有客户端真给了另一种形态时才换算；两种载体都没有 → 不写任何强度字段。
//  2. **优先原值**：目标线能承载原形态时原值搬运（等级→等级、预算→预算），不换算、不改写等级名。
//  3. **换算必须记损**：`LossThinkingDerived`（action = downgraded），detail 写明方向与数值，
//     便于从 LossReport 直接聚合出「有多少请求发生了强度降级」。
//
// 本模块是**超越 Node 的增强**（Node 在这两条线上直接丢弃预算）：跨语言对拍语料因此不为
// 「预算→等级」这一格生成 Node 期望值——那格由本包单测与矩阵台覆盖（见报告 §5）。
const (
	// thinkingLevelLow/Medium/High 是等级值域，与 OpenAI `reasoning_effort` 同域。
	thinkingLevelLow    = "low"
	thinkingLevelMedium = "medium"
	thinkingLevelHigh   = "high"

	// 预算→等级的阈值（token，**下界包含**）：
	//
	//	budget <  8192  → low
	//	budget < 32768  → medium
	//	budget >= 32768 → high
	//
	// 取值依据：Anthropic 要求 `budget_tokens` ≥ 1024，官方常见档位为 1024 / 8192 / 32768；
	// 阈值与下面 `thinkingBudgetByLevel` 的反向表**互为反函数**（1024→low、8192→medium、32768→high），
	// 故 `level → budget → level` 往返稳定（单测钉住）。
	thinkingBudgetMediumFloor = 8192
	thinkingBudgetHighFloor   = 32768
)

// ThinkingLevelFromBudget 把 token 预算反查为等级（阈值下界包含）。
//
// 非正数视为无效输入，返回 low（调用方不应在预算 ≤ 0 时来找等级——那是「未给预算」的形态）。
func ThinkingLevelFromBudget(budget float64) string {
	switch {
	case budget >= thinkingBudgetHighFloor:
		return thinkingLevelHigh
	case budget >= thinkingBudgetMediumFloor:
		return thinkingLevelMedium
	default:
		return thinkingLevelLow
	}
}

// effectiveThinkingEffort 返回「本次转换该写给目标线的等级」，以及该等级是否由预算换算得出。
//
//   - 客户端给了等级 → 原值返回（derived=false）；
//   - 未给等级但有预算 → 按阈值反查（derived=true，调用方须记 LossThinkingDerived）；
//   - 都没有（或等级为空串且无预算）→ ok=false，调用方不写任何强度字段。
func effectiveThinkingEffort(reasoning *Reasoning) (level string, derived bool, ok bool) {
	if reasoning == nil {
		return "", false, false
	}
	if reasoning.HasEffort && reasoning.Effort != "" {
		return reasoning.Effort, false, true
	}
	if reasoning.BudgetTokens != nil {
		return ThinkingLevelFromBudget(*reasoning.BudgetTokens), true, true
	}
	return "", false, false
}

// thinkingDerivationDetail 生成换算损失的 detail（形如 `budget_tokens=8192→reasoning_effort=medium`）。
//
// 为何带上目标字段名：同一换算在三线上落到不同字段（`reasoning_effort` / `reasoning.effort`），
// 排查时能一眼看出「降级写到了哪」。
func thinkingDerivationDetail(budget float64, targetField string, level string) string {
	return fmt.Sprintf("budget_tokens=%s→%s=%s", trimNumberLiteral(budget), targetField, level)
}

// trimNumberLiteral 把预算渲染成无多余小数的形式（1024 而非 1024.000000），仅用于日志/损失 detail。
func trimNumberLiteral(value float64) string {
	if value == float64(int64(value)) {
		return fmt.Sprintf("%d", int64(value))
	}
	return fmt.Sprintf("%g", value)
}
