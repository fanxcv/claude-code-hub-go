package limit

import (
	"context"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// rememberLeasePlan 把「已用租约判过的切片」落进请求上下文；空计划不落（不写空键、不产生标记）。
func rememberLeasePlan(req *pctx.Context, plan pctx.LeaseSettlementPlan) {
	if plan.Empty() {
		return
	}
	req.SetLeaseSettlementPlan(plan)
}

// rememberLeaseTarget 把一维**已判定用过**的租约切片追加进计划；目标不在取值域内时留痕并跳过。
//
// 为什么不静默跳过：漏掉一维的后果是这份切片永远不结算，租约余额比实际更宽，
// 刷新窗口内判定会放行本该拒的请求（“少扣”在账面上与“多扣”一样是错）。
// 两类丢弃都走同一条日志：取值域漂移（leaseSettlementTargetFor 返 false）与
// 结构不完整（Add 返 false），两者的排查手段相同——比对库里的实体/窗口/重置模式取值。
func rememberLeaseTarget(log *logx.Logger, plan *pctx.LeaseSettlementPlan, dimension costDimension) {
	target, inDomain := leaseSettlementTargetFor(dimension)
	if inDomain && plan.Add(target) {
		return
	}
	log.Error("limit.lease.plan_target_dropped", map[string]any{
		"entity":    string(dimension.entity),
		"period":    string(dimension.period),
		"resetMode": string(dimension.resetMode),
		"entityId":  dimension.id,
		"inDomain":  inDomain,
		"note":      "该维已走租约判定但目标不在结算取值域内，本维不结算；租约余额可能偏宽",
	})
}

// leaseSettlementTargetFor 把一次**以租约完成判定**的维度翻成结算目标。
//
// 第二个返回值为 false 表示这一维的取值不在结算取值域内（未知主体/窗口/重置模式，或 id 非正）：
// 调用方必须留痕（rememberLeaseTarget），不得当成「没什么可结算」而静默放过。
//
// 只记 actual 事实：主体、窗口、该窗口生效的重置模式（dimension.resetMode 已是补齐后的值）。
func leaseSettlementTargetFor(dimension costDimension) (pctx.LeaseSettlementTarget, bool) {
	target := pctx.LeaseSettlementTarget{
		Entity:    leaseSettlementEntity(dimension.entity),
		ID:        dimension.id,
		Window:    leaseSettlementWindow(dimension.period),
		ResetMode: string(dimension.resetMode),
	}
	return target, leaseSettlementTargetValid(target)
}

// leaseSettlementEntity / leaseSettlementWindow 是两套取值域之间的显式映射。
//
// 两边现在同值（key/user/provider、5h/daily/weekly/monthly），但仍写成 switch 而不是类型转换：
// 转换会在任一取值域改名时静默产生一个拼错的键名，而那种错只会表现成结算结果里的 `missing`。
// 未知取值返回空串；未知取值的目标由 leaseSettlementTargetValid（判定侧与结算侧共用）拦下。
func leaseSettlementEntity(entity Entity) pctx.LeaseSettlementEntity {
	switch entity {
	case EntityKey:
		return pctx.LeaseSettlementEntityKey
	case EntityUser:
		return pctx.LeaseSettlementEntityUser
	case EntityProvider:
		return pctx.LeaseSettlementEntityProvider
	}
	return ""
}

func leaseSettlementWindow(period Period) pctx.LeaseSettlementWindow {
	switch period {
	case Period5h:
		return pctx.LeaseSettlementWindow5h
	case PeriodDaily:
		return pctx.LeaseSettlementWindowDaily
	case PeriodWeekly:
		return pctx.LeaseSettlementWindowWeekly
	case PeriodMonthly:
		return pctx.LeaseSettlementWindowMonthly
	}
	return ""
}

// leaseCostLimit 用**租约**判定一个周期限额（对应 Node checkCostLimitsWithLease 的单窗口分支）。
//
// 返回 (block, decided)：
//   - decided=false 表示租约不可用（Redis 未就绪、设置读不到、回表失败）——此时调用方退回
//     窗口/账本路径。Node 在同情形下对本窗口 fail-open（放行），Go 这里选择更严的回退：
//     既有账本求和是「实时用量 vs 限额」，不会因为租约故障而放掉真实超限。
//     这是有意的差异，已在报告里列出（Node 的 fail-open 依赖「Redis 故障时另有 DB 侧兜底」）。
//   - decided=true 且 block=nil 表示本窗口放行。
func (s *Service) leaseCostLimit(
	ctx context.Context,
	dimension costDimension,
	now time.Time,
) (*guard.RateLimitBlock, bool) {
	resetAt := dimension.costResetAt
	// 与账本路径同一条口径：只有 User + 5h + 非固定窗口才用 later(costResetAt, limit5hCostResetAt)，
	// 其它窗口沿用完整重置边界，免得 5h 专用重置污染更长窗口。
	if dimension.entity == EntityUser && dimension.period == Period5h && dimension.resetMode != ResetFixed {
		resetAt = LaterReset(dimension.costResetAt, dimension.limit5hCostResetAt)
	}

	allowed, current, ok := s.leases.CheckCostLimitWithLease(ctx, GetCostLeaseParams{
		Entity:      dimension.entity,
		EntityID:    dimension.id,
		KeyHash:     dimension.keyHash,
		Window:      LeaseWindow(dimension.period),
		LimitAmount: *dimension.amount,
		ResetTime:   dimension.resetTime,
		ResetMode:   dimension.resetMode,
		CostResetAt: resetAt,
	})
	if !ok {
		s.log.Warn("limit.check.lease_unavailable", map[string]any{
			"entity": string(dimension.entity),
			"period": string(dimension.period),
			"note":   "租约不可用，退回窗口/账本路径",
		})
		return nil, false
	}
	if allowed {
		return nil, true
	}
	return s.costBlock(dimension, current, now), true
}
