package limit

import (
	"context"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// 本文件把租约结算接进生产路径：终态结算器在**成本落库成功之后**调用
// `Service.SettleLeases`（窄接口见 terminal.LeaseSettler，装配见 dataplane 与 cmd/cchd）。
//
// 为什么挂点在终端结算器而不在判定侧或数据面：扣减必须发生在成本真的进了账本之后。
// 若在成本写失败时也扣减，账本没有这笔而租约少一片，而下次刷新是按 DB 权威用量重新切片——
// 少掉的那片不会回来，窗口内判定会比实际更严（白拒请求）。

// SettleLeases 把一次请求的实际成本结算到它判定时用过的切片上（对应 Node `settleLeaseBudgets`）。
//
// 语义与约束：
//   - 幂等：`markerID` 作结算标记，重复调用（重试、重放、重复终态）命中标记即返回上一次结果、
//     不重复扣减（由 LeaseService.settleLeaseTargets 的 Lua 保证）。故本方法可安全重入。
//   - 一次请求可能结算多次：胜者成本一次，每条竞速输家成本各一次（见 dataplane 的输家计费）。
//     它们用**不同的标记**（`{行 id}` 与 `{行 id}:loser:{providerId}:{attempt}`）、
//     **各自的成本增量**，故合计扣减等于本次请求的总成本。标记不同就不会互相吞掉。
//   - 零成本不结算：判定侧从未扣减，这里也没有要扣的。costText 不可解析时按 0 处理
//     （ParseCostText 的既有语义）。
//   - 失败只留痕：租约是判定的加速面，权威额度在 DB（见 lease_service.go 的文件头）。
//     结算失败不得回滚已经写好的终态与账本——失败原因由 LeaseService 记 warn
//     （`limit.lease.settle_fail_open`），本方法再补一条结果摘要。
//   - 空计划表示本次请求没走租约判定（未装配租约、或限流步未执行、或所有维度都回退了账本）：
//     直接返回，不查空键。
func (s *Service) SettleLeases(ctx context.Context, markerID string, costText string, plan pctx.LeaseSettlementPlan) {
	if s == nil || s.leases == nil || plan.Empty() || markerID == "" {
		return
	}
	cost := ParseCostText(costText)
	if !(cost > 0) {
		return
	}
	targets, dropped := leaseSettlementTargetsFromPlan(plan)
	if len(targets) == 0 {
		// 计划里全部目标都无法映射：这只能来自取值域漂移（见 leaseSettlementEntity 的注释），
		// 不能默默当成「没有切片要扣」。
		s.log.Error("limit.lease.settle_no_targets", map[string]any{
			"markerId": markerID,
			"dropped":  dropped,
		})
		return
	}
	if dropped > 0 {
		s.log.Warn("limit.lease.settle_targets_dropped", map[string]any{
			"markerId": markerID,
			"dropped":  dropped,
		})
	}
	result := s.leases.settleLeaseTargets(ctx, markerID, cost, targets)
	// 结果摘要：扣减了几份、几份不足、几份没有租约。missing 在生产上的含义是
	// 「判定与结算的键不合一」（主体 id 或重置模式不同），故它不是噪声而是线索。
	decremented, insufficient, missing := leaseSettlementCounts(result.Settlements)
	s.log.Debug("limit.lease.settled", map[string]any{
		"markerId":     markerID,
		"status":       result.Status,
		"failOpen":     result.FailOpen,
		"decremented":  decremented,
		"insufficient": insufficient,
		"missing":      missing,
	})
}

// leaseSettlementTargetsFromPlan 把请求上下文里的计划翻成本包的结算目标。
//
// 第二个返回值是「因取值域不识别而丢弃的目标数」：调用方据此留痕，不静默当作空计划。
func leaseSettlementTargetsFromPlan(plan pctx.LeaseSettlementPlan) ([]leaseSettlementTarget, int) {
	targets := make([]leaseSettlementTarget, 0, len(plan.Targets))
	dropped := 0
	for _, target := range plan.Targets {
		mapped, ok := leaseSettlementTargetFromPlan(target)
		if !ok {
			dropped++
			continue
		}
		targets = append(targets, mapped)
	}
	return targets, dropped
}

// leaseSettlementTargetFromPlan 把一枚计划里的目标翻成本包的结算目标；取值域不识别即拒绝。
func leaseSettlementTargetFromPlan(target pctx.LeaseSettlementTarget) (leaseSettlementTarget, bool) {
	if !leaseSettlementTargetValid(target) {
		return leaseSettlementTarget{}, false
	}
	// 取值已由 leaseSettlementTargetValid 逐个确认过，此处直接用它的结果。
	entity, _ := leaseEntityFromPlan(target.Entity)
	window, _ := leaseWindowFromPlan(target.Window)
	return leaseSettlementTarget{
		entity:    entity,
		id:        target.ID,
		window:    window,
		resetMode: ResetMode(target.ResetMode),
	}, true
}

// leaseSettlementTargetValid 是结算目标取值域校验的**唯一实现**：主体、ID、窗口、重置模式
// 四者齐全且可识别。判定侧入计划前（leaseSettlementTargetFor）与结算侧读计划时
// （leaseSettlementTargetFromPlan）都走它——两侧共用一份判据，才不会一边放行一边拒。
func leaseSettlementTargetValid(target pctx.LeaseSettlementTarget) bool {
	if target.ID <= 0 {
		return false
	}
	if _, ok := leaseEntityFromPlan(target.Entity); !ok {
		return false
	}
	if _, ok := leaseWindowFromPlan(target.Window); !ok {
		return false
	}
	return leaseSettlementResetModeInDomain(ResetMode(target.ResetMode))
}

// leaseSettlementResetModeInDomain 报告重置模式是否落在本包取值域内（rolling / fixed）。
//
// 为什么不原样透传：租约键把模式写进键名（5h 与 daily），一个取值域外的模式会拼出一份
// **谁都不会再写的键**——判定与结算自洽，但整个切片落在取值域之外，只表现为结算结果里的
// `missing`。判定侧与结算侧共用本判据，取值域外的模式在入计划前就被拦下。
func leaseSettlementResetModeInDomain(mode ResetMode) bool {
	switch mode {
	case ResetRolling, ResetFixed:
		return true
	}
	return false
}

// leaseEntityFromPlan / leaseWindowFromPlan 是反向映射（pctx 取值域 → 本包取值域）。
func leaseEntityFromPlan(entity pctx.LeaseSettlementEntity) (LeaseEntity, bool) {
	switch entity {
	case pctx.LeaseSettlementEntityKey:
		return EntityKey, true
	case pctx.LeaseSettlementEntityUser:
		return EntityUser, true
	case pctx.LeaseSettlementEntityProvider:
		return EntityProvider, true
	}
	return "", false
}

func leaseWindowFromPlan(window pctx.LeaseSettlementWindow) (LeaseWindow, bool) {
	switch window {
	case pctx.LeaseSettlementWindow5h:
		return LeaseWindow5h, true
	case pctx.LeaseSettlementWindowDaily:
		return LeaseWindowDaily, true
	case pctx.LeaseSettlementWindowWeekly:
		return LeaseWindowWeekly, true
	case pctx.LeaseSettlementWindowMonthly:
		return LeaseWindowMonthly, true
	}
	return "", false
}

// leaseSettlementCounts 按状态统计一次结算的结果。
func leaseSettlementCounts(settlements []LeaseBudgetSettlement) (decremented, insufficient, missing int) {
	for _, settlement := range settlements {
		switch settlement.Status {
		case LeaseSettlementDecremented:
			decremented++
		case LeaseSettlementInsufficient:
			insufficient++
		case LeaseSettlementMissing:
			missing++
		}
	}
	return decremented, insufficient, missing
}
