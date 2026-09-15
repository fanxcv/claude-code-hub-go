package limit

import (
	"context"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
)

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
