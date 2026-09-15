package notify

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/limit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 成本预警的窗口标签。密钥有三档（5 小时滚动 / 本周 / 本月），供应商只有后两档
// （Node 的 provider 限额表没有 5h 列）。
const (
	costPeriod5h    = "5小时"
	costPeriodWeek  = "本周"
	costPeriodMonth = "本月"
)

// costWindowSpec 是一个待检查的窗口：标签 + 限额 + 起点（右端统一是「现在」）。
type costWindowSpec struct {
	period    string
	limitText *string
	start     time.Time
}

// CostAlerts 生成成本预警（Node：tasks/cost-alert.ts:14 的 checkUserQuotas + checkProviderQuotas）。
//
// 口径：
//   - 限额为 null 或 <= 0 的档位不检查（Node 的 `if (!limit || limit <= 0) continue`）；
//   - 触发条件是 `已花 >= 限额 × threshold`（UI 文案：「当消费达到配额的 {percent}% 时触发告警」）；
//   - 5h 档是**滚动**窗口（now-5h..now），周/月档是系统时区里的自然周/月（复用 limit 包的
//     WeekStart / MonthStart，与限流面同一套边界，避免两处口径漂移）。
//
// 返回空切片表示本刻无告警（调用方跳过投递）。
func (g *Generators) CostAlerts(
	ctx context.Context,
	threshold float64,
	timezone string,
	now time.Time,
) ([]CostAlertData, error) {
	if threshold <= 0 {
		threshold = DefaultCostAlertThreshold
	}
	location := Location(timezone)
	fiveHourStart := now.Add(-5 * time.Hour)
	weekStart := limit.WeekStart(now, location)
	monthStart := limit.MonthStart(now, location)

	keys, err := g.Cost.NotifyKeysWithCostLimits(ctx)
	if err != nil {
		return nil, err
	}
	alerts := make([]CostAlertData, 0, len(keys))
	for _, key := range keys {
		windows := []costWindowSpec{
			{period: costPeriod5h, limitText: key.Limit5h, start: fiveHourStart},
			{period: costPeriodWeek, limitText: key.LimitWeek, start: weekStart},
			{period: costPeriodMonth, limitText: key.LimitMonth, start: monthStart},
		}
		for _, window := range windows {
			quotaLimit := parseQuotaLimit(window.limitText)
			if quotaLimit <= 0 {
				continue
			}
			cost, err := g.sumEntityCost(ctx, store.LedgerEntityKey, key.Key, window.start, now)
			if err != nil {
				// 单个实体读失败只跳过它：Node 的 checkUserQuotas 也把异常吞在单个实体上，
				// 否则一行坏数据会让这一轮所有预警消失。
				g.logger().Warn("notify.cost_alert_sum_failed", map[string]any{
					"targetType": "user",
					"targetId":   key.ID,
					"period":     window.period,
					"error":      err.Error(),
				})
				continue
			}
			if cost < quotaLimit*threshold {
				continue
			}
			alerts = append(alerts, CostAlertData{
				TargetType:  "user",
				TargetName:  key.UserName,
				TargetID:    key.ID,
				CurrentCost: cost,
				QuotaLimit:  quotaLimit,
				Threshold:   threshold,
				Period:      window.period,
			})
		}
	}

	providers, err := g.Cost.NotifyProvidersWithCostLimits(ctx)
	if err != nil {
		return nil, err
	}
	for _, provider := range providers {
		windows := []costWindowSpec{
			{period: costPeriodWeek, limitText: provider.LimitWeek, start: weekStart},
			{period: costPeriodMonth, limitText: provider.LimitMonth, start: monthStart},
		}
		for _, window := range windows {
			quotaLimit := parseQuotaLimit(window.limitText)
			if quotaLimit <= 0 {
				continue
			}
			cost, err := g.sumEntityCost(ctx, store.LedgerEntityProvider, provider.ID, window.start, now)
			if err != nil {
				g.logger().Warn("notify.cost_alert_sum_failed", map[string]any{
					"targetType": "provider",
					"targetId":   provider.ID,
					"period":     window.period,
					"error":      err.Error(),
				})
				continue
			}
			if cost < quotaLimit*threshold {
				continue
			}
			alerts = append(alerts, CostAlertData{
				TargetType:  "provider",
				TargetName:  provider.Name,
				TargetID:    provider.ID,
				CurrentCost: cost,
				QuotaLimit:  quotaLimit,
				Threshold:   threshold,
				Period:      window.period,
			})
		}
	}

	return alerts, nil
}

// sumEntityCost 读某个实体在窗口 [start, end) 内的已花合计（账本 numeric 文本 → float）。
func (g *Generators) sumEntityCost(
	ctx context.Context,
	entityType store.LedgerEntityType,
	entityID any,
	start time.Time,
	end time.Time,
) (float64, error) {
	text, err := g.Cost.SumLedgerCostInTimeRange(ctx, entityType, entityID, start, end)
	if err != nil {
		return 0, err
	}
	return g.parseCostText("notify.cost_alert_cost_unparsable", text), nil
}

// parseQuotaLimit 把限额列（numeric 文本）解成正数；null / 非法 / <= 0 一律表示「不检查该档」。
func parseQuotaLimit(text *string) float64 {
	if text == nil {
		return 0
	}
	value, err := strconv.ParseFloat(strings.TrimSpace(*text), 64)
	if err != nil {
		return 0
	}
	return value
}
