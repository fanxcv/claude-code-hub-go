package store

import (
	"context"
	"time"
)

// SimulatorProviderRow 是调度模拟器需要的供应商行：选路视图的全部列
// （`store.Provider`，含选路四列）**加上**模拟器独有的三组列。
//
// 为什么不复用 `route.StoreSource.Providers`：那条读的是**启用态**（Node 的
// findEnabledProviders），而模拟器必须看到全量（含禁用），由 enabledCheck 那一步淘汰——
// Node 的 findAllProvidersFresh() 就是这个语义。
//
// ActiveTimeStart/End 与限额列当前**不在选路视图里**：数据面选路既不判活动时段
// （`route.Gates.Schedule` 无装配方）也不判供应商级限额（`route.Gates.Limits` 无装配方），
// 故这组列只被模拟器读。两者的缺口见报告。
type SimulatorProviderRow struct {
	Provider

	ActiveTimeStart *string `json:"active_time_start"`
	ActiveTimeEnd   *string `json:"active_time_end"`

	Limit5hUSD       *float64   `json:"limit_5h_usd"`
	Limit5hResetMode *string    `json:"limit_5h_reset_mode"`
	LimitDailyUSD    *float64   `json:"limit_daily_usd"`
	DailyResetMode   *string    `json:"daily_reset_mode"`
	DailyResetTime   *string    `json:"daily_reset_time"`
	LimitWeeklyUSD   *float64   `json:"limit_weekly_usd"`
	LimitMonthlyUSD  *float64   `json:"limit_monthly_usd"`
	LimitTotalUSD    *float64   `json:"limit_total_usd"`
	TotalCostResetAt *time.Time `json:"total_cost_reset_at"`
}

// FindAllProvidersForSimulator 读回全部未删除供应商（含禁用态），顺序与选路一致。
func (p *Pools) FindAllProvidersForSimulator(ctx context.Context) ([]SimulatorProviderRow, error) {
	return readRowsAs[SimulatorProviderRow](
		ctx, p,
		`SELECT row_to_json(t)::text FROM (
			SELECT * FROM providers
			WHERE deleted_at IS NULL
			ORDER BY priority ASC, id ASC
		) t`,
	)
}

// SimulatorEndpointRow 是端点池统计所需的单行：Node 的 getEndpointFilterStats 要
// 「总数（含禁用/已删）/ 启用数 / 启用且熔断打开数」，故三列都要取。
type SimulatorEndpointRow struct {
	ID        int64      `json:"id"`
	IsEnabled bool       `json:"is_enabled"`
	DeletedAt *time.Time `json:"deleted_at"`
}

// FindProviderEndpointsForSimulator 读回某个「厂 + 类型」下的全部端点行（不过滤启用态）。
func (p *Pools) FindProviderEndpointsForSimulator(
	ctx context.Context,
	vendorID int64,
	providerType string,
) ([]SimulatorEndpointRow, error) {
	return readRowsAs[SimulatorEndpointRow](
		ctx, p,
		`SELECT row_to_json(t)::text FROM (
			SELECT id, is_enabled, deleted_at FROM provider_endpoints
			WHERE vendor_id = $1 AND provider_type = $2
			ORDER BY sort_order ASC, id ASC
		) t`,
		vendorID, providerType,
	)
}
