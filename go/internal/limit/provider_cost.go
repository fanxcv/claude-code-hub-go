package limit

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// ProviderCostLimits 是供应商维度限额判定的输入：字段与 Node 的
// `RateLimitService.checkCostLimits(id, "provider", { …limits })` 的 `limits` 形参逐项对应。
//
// 之所以要有这个导出入口：数据面的 `route.Gates.Limits` 目前没有装配方，而调度模拟
// （`/dashboard/dispatch-simulator:simulate`）必须复刻 Node 在**选路阶段**对供应商做的
// 只读限额判定。判定口径与顺序复用本包内部的 `costLimit` / `totalCost`，
// 不另写一份窗口读法（那是漂移源）。
type ProviderCostLimits struct {
	Limit5hUSD       *float64
	Limit5hResetMode *string
	LimitDailyUSD    *float64
	DailyResetMode   *string
	DailyResetTime   *string
	LimitWeeklyUSD   *float64
	LimitMonthlyUSD  *float64
	// CostResetAt 对应 `providers.total_cost_reset_at`：既是周期限额的成本重置标记，
	// 也是总额度的重置时刻（Node 在两次调用里传的是同一个字段）。
	CostResetAt *time.Time
	// LimitTotalUSD 对应 `providers.limit_total_usd`（永久硬上限）。
	LimitTotalUSD *float64
}

// CheckProviderCostLimits 只读判定供应商的周期限额与总额度，返回是否放行与未放行的理由。
//
// 「只读」是与 Node 的硬对齐点：Node 的 `checkCostLimits` / `checkTotalCostLimit` 在
// 模拟与预览路径上**不得**刷新 lease、不得写 Redis；本方法只走 `costLimit` / `totalCost`
// 的读路径（Redis 快路径 → 未命中回退账本 → 固定窗口缓存按需回写）。
//
// 判定顺序与理由文案照抄 Node：5 小时 → 每日 → 周 → 月，随后总额度。
// 读取失败时**放行并留痕**（Fail Open）：预览不该因为一次读失败就谎报「该供应商被排除」。
func (s *Service) CheckProviderCostLimits(
	ctx context.Context,
	providerID int64,
	in ProviderCostLimits,
) (bool, string) {
	now := s.now()

	dimensions := []costDimension{
		{
			entity: EntityProvider, id: providerID, period: Period5h,
			amount:      in.Limit5hUSD,
			resetMode:   providerResetMode(in.Limit5hResetMode, ResetRolling),
			costResetAt: in.CostResetAt,
		},
		{
			entity: EntityProvider, id: providerID, period: PeriodDaily,
			amount:      in.LimitDailyUSD,
			resetMode:   providerResetMode(in.DailyResetMode, ResetFixed),
			resetTime:   NormalizeResetTime(derefString(in.DailyResetTime)),
			costResetAt: in.CostResetAt,
		},
		{
			entity: EntityProvider, id: providerID, period: PeriodWeekly,
			amount: in.LimitWeeklyUSD, resetMode: ResetFixed, costResetAt: in.CostResetAt,
		},
		{
			entity: EntityProvider, id: providerID, period: PeriodMonthly,
			amount: in.LimitMonthlyUSD, resetMode: ResetFixed, costResetAt: in.CostResetAt,
		},
	}

	for _, dimension := range dimensions {
		if dimension.amount == nil || *dimension.amount <= 0 {
			continue
		}
		current, exceeded, err := s.costLimit(ctx, dimension, now)
		if err != nil {
			s.log.Error("limit.provider_cost.failed", map[string]any{
				"providerId": providerID,
				"period":     string(dimension.period),
				"error":      err.Error(),
			})
			continue
		}
		if exceeded {
			return false, providerCostReason(dimension.period, current, *dimension.amount)
		}
	}

	if in.LimitTotalUSD != nil && *in.LimitTotalUSD > 0 {
		current, err := s.totalCost(ctx, EntityProvider, providerID, "", in.CostResetAt, now)
		if err != nil {
			s.log.Error("limit.provider_total_cost.failed", map[string]any{
				"providerId": providerID,
				"error":      err.Error(),
			})
			return true, ""
		}
		if current >= *in.LimitTotalUSD {
			return false, "供应商 total spending limit reached (" +
				costAmountText(current) + "/" + costAmountText(*in.LimitTotalUSD) + ")"
		}
	}

	return true, ""
}

// providerCostReason 复刻 Node 的周期限额理由文案：
// “ `${typeName} ${limit.name}消费上限已达到（${current.toFixed(4)}/${limit.amount}）` “
// 其中 provider 的 typeName 为「供应商」，name 依次为 5小时 / 每日 / 周 / 月。
func providerCostReason(period Period, current, amount float64) string {
	name := "5小时"
	switch period {
	case PeriodDaily:
		name = "每日"
	case PeriodWeekly:
		name = "周"
	case PeriodMonthly:
		name = "月"
	}
	return "供应商 " + name + "消费上限已达到（" +
		costAmountText4(current) + "/" + costAmountText(amount) + "）"
}

// costAmountText4 对齐 JS 的 `Number.prototype.toFixed(4)`。
func costAmountText4(value float64) string {
	return strconv.FormatFloat(value, 'f', 4, 64)
}

// costAmountText 对齐 JS 模板串里的裸数字（`${limit.amount}`）：不留多余小数位。
func costAmountText(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// providerResetMode 把库里的模式字符串转成 ResetMode，语义与 Node 逐字对齐：
// Node 传的是 `limits.limit_5h_reset_mode ?? "rolling"` 与 `limits.daily_reset_mode ?? "fixed"`，
// 而下游只判 `resetMode === "fixed"`，故**非空但未知的值一律走滚动路径**，不能当成缺省。
func providerResetMode(value *string, fallback ResetMode) ResetMode {
	if value == nil {
		return fallback
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return fallback
	}
	if ResetMode(trimmed) == ResetFixed {
		return ResetFixed
	}
	return ResetRolling
}
