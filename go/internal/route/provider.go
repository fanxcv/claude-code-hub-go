package route

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// Provider 是选路视角的供应商，字段集是 store.Provider 的子集加上两个判读方法。
//
// 读取一律走 store 的只读视图（已含 provider_vendor_id、group_priorities、
// protocol_conversion_enabled、disable_session_reuse 四个选路必需列），本包不再自持 SQL。
type Provider struct {
	ID           int64                `json:"id"`
	Name         string               `json:"name"`
	ProviderType convert.ProviderType `json:"provider_type"`
	URL          string               `json:"url"`
	IsEnabled    bool                 `json:"is_enabled"`
	Weight       int                  `json:"weight"`
	Priority     *int                 `json:"priority"`
	// CostMultiplier 是 numeric 列：PostgreSQL 的 row_to_json 对 numeric 既可能给数字也可能给字符串，
	// 用 json.Number 才两者皆容（用 string 会在真实库上直接反序列化失败）。
	CostMultiplier  json.Number    `json:"cost_multiplier"`
	GroupTag        *string        `json:"group_tag"`
	GroupPriorities map[string]int `json:"group_priorities"`
	// groupPrioritiesIssues 记录本条供应商 group_priorities 里被丢弃的坏值（空即无脏值）。
	//
	// 不导出：它只是从 store 桥接到选路器告警点的诊断信息，不是选路输入，也不该出去
	// 被序列化。store 侧不告警（只读层没有 logger），而选路器有（Options.Logger）——
	// 脏值必须**可见**，否则用户只会看到「某一家的覆盖莫名不生效」。
	groupPrioritiesIssues     []store.GroupPrioritiesIssue
	AllowedModels             json.RawMessage `json:"allowed_models"`
	ProviderVendorID          *int64          `json:"provider_vendor_id"`
	ProtocolConversionEnabled *bool           `json:"protocol_conversion_enabled"`
	DisableSessionReuse       bool            `json:"disable_session_reuse"`
	// ActiveTimeStart/End 是供应商活动时段（Node 的 provider.activeTimeStart/End）。
	// 判定函数见 ProviderActiveNow；接线点见 Options.Gates.Schedule 与 Request.ScheduleGate。
	ActiveTimeStart *string `json:"active_time_start"`
	ActiveTimeEnd   *string `json:"active_time_end"`
	// AllowedClients/BlockedClients 是供应商级客户端名单的**原文**：解析与模式匹配属
	// `internal/guard`（它已有一份与 TS 逐条对齐的 client-detector 移植，见 guard/client.go），
	// 本包只负责把它们带到选路处，不重复实现匹配。
	AllowedClients json.RawMessage `json:"allowed_clients"`
	BlockedClients json.RawMessage `json:"blocked_clients"`
	// CostLimits 是供应商级金额限额（Node 的 filterByLimits 判定）。
	CostLimits ProviderCostLimits `json:"-"`
}

// ProviderCostLimits 是供应商级金额限额的选路视角（字段名与 Node 的入参对象同名）：
// `providers.limit_5h_usd` / `limit_5h_reset_mode` / `limit_daily_usd` / `daily_reset_mode` /
// `daily_reset_time` / `limit_weekly_usd` / `limit_monthly_usd` / `limit_total_usd` /
// `total_cost_reset_at`。
//
// 为什么用 *float64 而不是 json.Number：本结构体只给判定器消费（判定器要数值），
// 原始数字文本已在落链与展示路径上由 CostMultiplier 那套保留，这里不必再保一份文本。
type ProviderCostLimits struct {
	Limit5hUSD       *float64
	Limit5hResetMode *string
	LimitDailyUSD    *float64
	DailyResetMode   *string
	DailyResetTime   *string
	LimitWeeklyUSD   *float64
	LimitMonthlyUSD  *float64
	LimitTotalUSD    *float64
	// CostResetAt 对应 `providers.total_cost_reset_at`：既是周期限额的成本重置标记，
	// 也是总额度的重置时刻（Node 在两次调用里传的是同一个字段）。
	CostResetAt *time.Time
}

// EffectivePriority 返回未做分组覆盖时的优先级（对应 Node 的 provider.priority ?? 0）。
func (p Provider) EffectivePriority() int {
	if p.Priority == nil {
		return 0
	}
	return *p.Priority
}

// ConversionEnabled 报告该供应商是否开启了协议转换（Node 用 `=== true` 判定，缺省即关闭）。
func (p Provider) ConversionEnabled() bool {
	return p.ProtocolConversionEnabled != nil && *p.ProtocolConversionEnabled
}

// CostMultiplierFloat 是成本倍率的数值投影，仅用于排序；落链与展示一律保留原始数字。
func (p Provider) CostMultiplierFloat() float64 {
	value, err := strconv.ParseFloat(p.CostMultiplier.String(), 64)
	if err != nil {
		return 0
	}
	return value
}

// Endpoint 是 provider_endpoints 的选路视角（只用到端点可用性所需的列）。
type Endpoint struct {
	ID        int64 `json:"id"`
	VendorID  int64 `json:"vendor_id"`
	IsEnabled bool  `json:"is_enabled"`
}

// Source 提供一次选路所需的全部只读快照。
//
// 三个方法都可被 cfgsync.KeyedCache 包住；实现必须允许并发调用，且不得在调用间保留请求级状态。
type Source interface {
	// Providers 返回全部启用态供应商（对应 Node 的 session.getProvidersSnapshot / findAllProviders）。
	Providers(ctx context.Context) ([]Provider, error)
	// Provider 返回单个供应商（含禁用态），用于亲和提名的硬校验。
	Provider(ctx context.Context, id int64) (*Provider, error)
	// Endpoints 返回某供应商厂与类型的启用态端点，按 sort_order 升序。
	Endpoints(ctx context.Context, vendorID int64, providerType convert.ProviderType) ([]Endpoint, error)
}

// StoreSource 是 Source 的 PostgreSQL 实现，只做类型桥接，查询全部委托给 store 的只读视图。
type StoreSource struct {
	pools *store.Pools
}

// NewStoreSource 用已建好的连接池构造数据源。
func NewStoreSource(pools *store.Pools) *StoreSource {
	return &StoreSource{pools: pools}
}

// Providers 复刻 findEnabledProviders 的读取面（与 store.FindEnabledProviders 同序同过滤）。
func (s *StoreSource) Providers(ctx context.Context) ([]Provider, error) {
	rows, err := s.pools.FindEnabledProviders(ctx)
	if err != nil {
		return nil, err
	}
	providers := make([]Provider, 0, len(rows))
	for _, row := range rows {
		providers = append(providers, providerFromStore(row))
	}
	return providers, nil
}

// Provider 复刻 findProviderById 的读取面。
func (s *StoreSource) Provider(ctx context.Context, id int64) (*Provider, error) {
	row, err := s.pools.FindProviderByID(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("route: 供应商 %d 不存在", id)
		}
		return nil, err
	}
	provider := providerFromStore(*row)
	return &provider, nil
}

// Endpoints 复刻 findEnabledProviderEndpointsByVendorAndType 的读取面。
func (s *StoreSource) Endpoints(
	ctx context.Context,
	vendorID int64,
	providerType convert.ProviderType,
) ([]Endpoint, error) {
	rows, err := s.pools.FindEnabledProviderEndpointsByVendorAndType(ctx, vendorID, string(providerType))
	if err != nil {
		return nil, err
	}
	endpoints := make([]Endpoint, 0, len(rows))
	for _, row := range rows {
		endpoints = append(endpoints, Endpoint{ID: row.ID, VendorID: row.VendorID, IsEnabled: row.IsEnabled})
	}
	return endpoints, nil
}

// providerFromStore 把只读视图投影到选路视角。
//
// priority 与 protocol_conversion_enabled 在库里分别是 NOT NULL 的 integer 与 boolean，
// 这里补成指针只为沿用选路侧既有的「缺省即 0 / 缺省即关闭」判读口径（Node 用 `=== true`），
// 因此桥接出的指针永不落在 nil 上。
func providerFromStore(row store.Provider) Provider {
	priority := row.Priority
	conversionEnabled := row.ProtocolConversionEnabled
	// 容错解码：脏值跳过并留给选路器告警，**不**让它把整批候选拖没（详见 store.DecodeGroupPriorities）。
	groupPriorities, groupPriorityIssues := store.DecodeGroupPriorities(row.GroupPriorities)
	return Provider{
		ID:                        row.ID,
		Name:                      row.Name,
		ProviderType:              convert.ProviderType(row.ProviderType),
		URL:                       row.URL,
		IsEnabled:                 row.IsEnabled,
		Weight:                    row.Weight,
		Priority:                  &priority,
		CostMultiplier:            row.CostMultiplier,
		GroupTag:                  row.GroupTag,
		GroupPriorities:           groupPriorities,
		groupPrioritiesIssues:     groupPriorityIssues,
		AllowedModels:             row.AllowedModels,
		ProviderVendorID:          row.ProviderVendorID,
		ProtocolConversionEnabled: &conversionEnabled,
		DisableSessionReuse:       row.DisableSessionReuse,
		ActiveTimeStart:           row.ActiveTimeStart,
		ActiveTimeEnd:             row.ActiveTimeEnd,
		AllowedClients:            row.AllowedClients,
		BlockedClients:            row.BlockedClients,
		CostLimits: ProviderCostLimits{
			Limit5hUSD:       numberPtr(row.Limit5hUSD),
			Limit5hResetMode: row.Limit5hResetMode,
			LimitDailyUSD:    numberPtr(row.LimitDailyUSD),
			DailyResetMode:   row.DailyResetMode,
			DailyResetTime:   row.DailyResetTime,
			LimitWeeklyUSD:   numberPtr(row.LimitWeeklyUSD),
			LimitMonthlyUSD:  numberPtr(row.LimitMonthlyUSD),
			LimitTotalUSD:    numberPtr(row.LimitTotalUSD),
			CostResetAt:      row.TotalCostResetAt,
		},
	}
}

// numberPtr 把 row_to_json 读出的数值列投影为 *float64（NULL 保持 nil，表示**未设限额**）。
//
// 解析失败也返回 nil：`store` 侧的读取视图已保证这些字段只在 JSON number 或 null 上取值，
// 真正会出现非数字形态的只有脏数据；把脏数据当「无限额」与 Node 的 `!limit || limit <= 0`
// 分支同向（都放行该维度），不会因此把请求拦死。
func numberPtr(value *json.Number) *float64 {
	if value == nil {
		return nil
	}
	parsed, err := strconv.ParseFloat(value.String(), 64)
	if err != nil {
		return nil
	}
	return &parsed
}

// ProviderFromStore 是 store 读到选路视角供应商的**唯一投影入口**。
//
// 导出它是为了让管理面（调度模拟器）能复用同一份字段映射，而不是在管理面再拼一遍
// ——14 个字段的投影抄第二份就是漂移源。
func ProviderFromStore(row store.Provider) Provider { return providerFromStore(row) }
