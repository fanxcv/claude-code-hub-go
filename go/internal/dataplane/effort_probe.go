package dataplane

import (
	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/specialsettings"
)

// 本文件把「思考强度」从一个纯展示字段升级为**协议转换正确性探针**。
//
// 为什么需要它（用户口径，2026-09-13）：使用记录页的思考强度列此前恒空，补写之后还必须能用它
// 回答一个问题——**协议转换是否正确**。若只记客户端请求值（requested），列能显示，但它证明不了
// 上游到底收到了什么；若只记上游值，就丢掉了「客户端要的是什么」这一半对照。
//
// 两者都要留、且要可区分：
//   - requested：客户端原始值，**建行时**由守卫链写入（`anthropic_effort` 等，见 internal/specialsettings）；
//   - forwarded：**转换器产物**（`forward.Plan.Body`）里该字段的值，**终态**追加一条
//     `thinking_effort_forwarded`。
//
// 硬约束：forwarded **只能**来自 `plan.Body`（即将发给上游的那份字节），
// **禁止**由记录侧从客户端值二次推导——那样它就成了「我以为发了什么」而不是「实际发了什么」，
// 也就无法作为转换正确性的证据。转换把字段丢掉时记 `dropped=true`（显式），不是静默为空。
//
// 字段路径按「本次是否真的发生了转换」选择：
//   - 转换生效（`plan.Conversion != nil`）：上游正文属目标线，按目标线读；
//   - 原生直通：上游正文仍是客户端方言，按客户端字段路径读。
//
// 两者读的都是**同一份字节**，区别只在用哪套字段约定，故这不是「二次推导」。
//
// plan 为 nil 表示本次没走到计划编译（无上游参与），此时既没有「转发值」这一事实，
// 也没有「施加了转换」这一事实。

// captureRequestedEffort 取一次客户端请求侧的思考强度，供终态探针与终态断言使用。
//
// 调用点必须在**守卫链之后**：读体即解压，鉴权通过前不该做。
func (h *Handler) captureRequestedEffort(state *RequestState, spec routeSpec, body *bodyAccess) {
	if state == nil || body == nil {
		return
	}
	access, err := body.factory(state.PC)
	if err != nil || access == nil {
		return
	}
	parsed, err := access.JSON()
	if err != nil || parsed == nil {
		return
	}
	state.requestedEffort = specialsettings.RequestTrimmedEffort(parsed, spec.Format, state.PC.Path())
}

// specialSettingsAppendEntries 产出终态要**追加**的审计条目（一个 JSON 数组）：
//
//   - `thinking_effort_forwarded`：转换后思考强度探针（Go 新增，超出 Node parity）；
//   - `protocol_conversion`：本次上游尝试确实施加了协议转换（与 Node 逐字同形）；
//   - `protocol_conversion_failed`：本次**本来要**转换但失败（Go 新增，超出 Node parity；
//     与上一条互斥——成功写 `protocol_conversion`，失败写本条，原生同协议两条都不写）。
//
// 三者合成一个数组一次追加：存储层的追加是 `COALESCE(col,'[]') || $n`
// store.DetailsPatch.SpecialSettingsAppend），多次调用会多出每请求写入，而本仓的纪律是
// 「零额外每请求写入」。故失败条目**不会**让成功路径多写一次——它只是同一个数组里的另一个元素。
//
// 为何把协议转换审计放在终态而不是选路时：Node 写这条的时机是「上游尝试构造成立」那一刻
// （`forwarder.ts:3778` 的 `prepared && conversionPlan`），并对每个 attempt 各记一条（胜出者
// 由 `syncWinningAttemptSession` 合并回主会话）。Go 侧的 `result.Plan` / `outcome.Plan` 就是
// **胜出尝试**的计划，含义等价；而计划在编译期就把「路径解析失败」「正文转换失败」两种情形
// 置回了 `Conversion = nil`（后者另置 `ConversionFallback`），所以 `Conversion != nil`
// 恰好就是 Node 的「确实应用了转换」，不包含回退与失败。
func specialSettingsAppendEntries(state *RequestState, plan *forward.Plan) []byte {
	if plan == nil {
		return nil
	}
	converted := plan.Conversion != nil
	var requested specialsettings.EffortRequest
	if state != nil {
		requested = state.requestedEffort
	}
	var forwarded specialsettings.EffortForwarded
	switch {
	case converted:
		forwarded = specialsettings.ForwardedTrimmedEffort(plan.Protocol, plan.Body)
	case state != nil:
		forwarded = specialsettings.ForwardedTrimmedEffortAt(plan.Body, state.requestedEffort.Field, plan.Protocol)
	}
	// 转换条目二选一：成功则记「已转换」，失败则记「转不了，为什么」。
	// 两个分支互斥由 plan 的事实保证（Conversion 与 ConversionFailure 不会同时非 nil）。
	conversionEntry := specialsettings.ConversionEntry(plan.Conversion)
	if failureEntry := specialsettings.ConversionFailureEntry(plan.ConversionFailure); failureEntry != nil {
		conversionEntry = failureEntry
	}
	return specialsettings.AppendEntries(
		specialsettings.ProbeEntry(requested, forwarded, converted),
		conversionEntry,
		// Codex 会话标识补全条目（守卫链产物，见 codex_session_audit.go）。
		codexSessionEntry(state),
	)
}
