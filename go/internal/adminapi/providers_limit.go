package adminapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/limit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 providers 资源的**限额读数子面**（两条端点）：
//
//	GET  /providers/{id}/limit-usage    router.ts:393 → handlers.ts:288 getProviderLimit
//	POST /providers/limit-usage:batch   router.ts:415 → handlers.ts:299 getProviderLimitBatch
//
// 唯一真源：src/actions/providers.ts:3122 getProviderLimitUsage 与 :3265 getProviderLimitUsageBatch。
//
// 与 keys 同名端点（keys_usage.go）的**三处实质差异**（照抄 keys 版会错）：
//
//  1. 供应商**没有** costResetAt 的「Key/User 取较晚者」概念，因此各周期窗口**一律不做
//     clip**（Node 只把 provider.totalCostResetAt 用在总额度那一项上，且是当起点下限用）。
//  2. 5h 的模式默认值是 **rolling**（provider.limit5hResetMode ?? "rolling"），而 daily 默认
//     **fixed**（provider.dailyResetMode ?? "fixed"）——与 keys 的默认值方向相反。
//  3. 批量版按**可见供应商列表的顺序**输出 items（Node 是 `visibleProviders.filter(...)` 后再
//     Map 迭代），不是请求里的 providerIds 顺序；不可见/不存在的 id 直接缺席，不报 404。
//
// 降级纪律：两条路由要「账本聚合（PG）+ 会话计数（Redis）+ 5h 固定窗口（Redis）」三侧齐备才注册。
// 缺任一侧即整组不注册、原样回退 Node——半个读数（例如固定窗口恒 0）在配额页上是静默错数。

// ProviderFixed5hWindowReader 读 **provider 维度** 5h 固定窗口的累计值与重置时刻
// （Node 的 RateLimitService.getFixed5hWindowState → get5hWindowResetAt / getCurrentCost）。
//
// 与 Deps.Fixed5hWindows（Key 维度）分开声明：两者的键主体不同，合成一个接口会让
// 「谁在读哪族键」变模糊。`*limit.CostWindows` 直接满足本接口（传 limit.EntityProvider）。
type ProviderFixed5hWindowReader interface {
	Fixed5hWindowState(ctx context.Context, entity limit.Entity, id int64, now time.Time) (limit.Fixed5hState, error)
}

// ProvidersLimitOptions 是两条限额读数端点的可注入运行态依赖。
//
// 为什么用注册参数而不是 Deps 字段：本波多路并行，Deps 是共享文件；注册参数与
// RegisterLeaderboardRoutes/RegisterVersionRoutes 同形，接线只需一行。
type ProvidersLimitOptions struct {
	// Fixed5h 为 nil 时整组不注册（回退 Node）：供应商若配了固定窗口，
	// 读不到窗口就只能给出错数。
	Fixed5h ProviderFixed5hWindowReader
}

// providersLimitAPI 是两条端点的处理器。
type providersLimitAPI struct {
	deps     Deps
	pools    *store.Pools
	sessions ObservedSessionRuntime
	fixed5h  ProviderFixed5hWindowReader
	problems ProblemWriter
	logger   *logx.Logger
	now      func() time.Time
}

// RegisterProvidersLimitRoutes 注册两条限额读数端点。
func RegisterProvidersLimitRoutes(router *Router, deps Deps, options ProvidersLimitOptions) {
	if deps.Store == nil || deps.ObservedSessions == nil || options.Fixed5h == nil {
		if deps.Logger != nil {
			deps.Logger.Warn("admin_providers_limit_unwired", map[string]any{
				"module":            "providers",
				"action":            "routes_not_registered",
				"store":             deps.Store != nil,
				"observedSessions":  deps.ObservedSessions != nil,
				"fixed5hWindowRead": options.Fixed5h != nil,
			})
		}
		return
	}
	api := &providersLimitAPI{
		deps:     deps,
		pools:    deps.Store,
		sessions: deps.ObservedSessions,
		fixed5h:  options.Fixed5h,
		problems: adminProblemWriter(deps),
		logger:   adminLoggerOf(deps),
		now:      time.Now,
	}

	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/providers/{id:[0-9]+}/limit-usage",
		Access:      AccessAdmin,
		Module:      "providers",
		OperationID: "getProviderLimit",
		Handler:     http.HandlerFunc(api.handleProviderLimit),
	})
	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/providers/limit-usage:batch",
		Access:      AccessAdmin,
		Module:      "providers",
		OperationID: "getProviderLimitBatch",
		Handler:     http.HandlerFunc(api.handleProviderLimitBatch),
	})
}

// handleProviderLimit 复刻 getProviderLimit：不可见（不存在或隐藏类型）即 404。
func (api *providersLimitAPI) handleProviderLimit(writer http.ResponseWriter, request *http.Request) {
	providerID, ok := providerPathID(writer, request, "id")
	if !ok {
		return
	}
	provider, err := api.visibleProvider(request, providerID)
	if err != nil {
		if errors.Is(err, errProviderLimitNotFound) {
			api.problems.WriteActionError(writer, request, providerNotFoundError())
			return
		}
		api.problems.WriteActionError(writer, request, adminActionFailure("provider", err))
		return
	}

	location, err := api.systemLocation(request.Context())
	if err != nil {
		api.problems.WriteActionError(writer, request, adminActionFailure("provider", err))
		return
	}

	now := api.now()
	sessionCounts, err := api.sessions.ProviderSessionCounts(request.Context(), []int64{providerID})
	if err != nil {
		// Node 单条会话计数是 fail-open（记日志、计 0），不牵连整条端点。
		api.logger.Warn("admin_providers_limit_session_count_failed", map[string]any{
			"providerId": providerID,
			"error":      err.Error(),
		})
	}
	usage, err := api.usage(request.Context(), provider, now, location, sessionCounts[providerID])
	if err != nil {
		api.problems.WriteActionError(writer, request, adminActionFailure("provider", err))
		return
	}
	adminWriteJSON(writer, http.StatusOK, usage)
}

// handleProviderLimitBatch 复刻 getProviderLimitBatch：只回**可见**供应商的交集，
// 且顺序随可见列表（不是请求顺序）；单条失败按 Node 的 fail-open 语义返回已累计的部分。
func (api *providersLimitAPI) handleProviderLimitBatch(writer http.ResponseWriter, request *http.Request) {
	fields, ok := adminReadJSONObject(writer, request, api.deps)
	if !ok {
		return
	}
	object := adminNewObject(fields, "providerIds")
	object.RejectUnknownKeys()
	ids, hasIDs := adminInt64Array(object, "providerIds", 1, 500)
	if issues := object.issues0(); len(issues) > 0 {
		adminWriteValidationFailure(writer, request, issues)
		return
	}
	if !hasIDs {
		adminWriteValidationFailure(writer, request, []invalidParam{{
			Path: []any{"providerIds"}, Code: "invalid_type", Message: "Required",
		}})
		return
	}

	visible, err := providerVisibleProviders(request, api.deps)
	if err != nil {
		api.problems.WriteActionError(writer, request, adminActionFailure("provider", err))
		return
	}
	requested := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		requested[id] = struct{}{}
	}
	// 顺序：可见列表（Node 的 visibleProviders.filter(providerIds.has) → Map 迭代顺序）。
	selected := make([]store.AdminProvider, 0, len(visible))
	selectedIDs := make([]int64, 0, len(visible))
	for _, provider := range visible {
		if _, want := requested[provider.ID]; !want {
			continue
		}
		selected = append(selected, provider)
		selectedIDs = append(selectedIDs, provider.ID)
	}
	if len(selected) == 0 {
		adminWriteJSON(writer, http.StatusOK, map[string]any{"items": []any{}})
		return
	}

	location, err := api.systemLocation(request.Context())
	if err != nil {
		api.problems.WriteActionError(writer, request, adminActionFailure("provider", err))
		return
	}
	now := api.now()
	sessionCounts, err := api.sessions.ProviderSessionCounts(request.Context(), selectedIDs)
	if err != nil {
		api.logger.Warn("admin_providers_limit_session_counts_failed", map[string]any{
			"providers": len(selectedIDs),
			"error":     err.Error(),
		})
	}

	items := make([]map[string]any, 0, len(selected))
	for _, provider := range selected {
		usage, err := api.usage(request.Context(), provider, now, location, sessionCounts[provider.ID])
		if err != nil {
			// Node 的批量版在 try/catch 里逐条累加，出错即中止并返回**已累计**的部分
			// （src/actions/providers.ts:3399-3402 的 catch → return result）。
			api.logger.Error("admin_providers_limit_usage_failed", map[string]any{
				"providerId": provider.ID,
				"error":      err.Error(),
			})
			break
		}
		items = append(items, map[string]any{"id": provider.ID, "usage": usage})
	}
	adminWriteJSON(writer, http.StatusOK, map[string]any{"items": items})
}

// errProviderLimitNotFound 表示供应商不存在或不可见（映射成 provider.not_found）。
var errProviderLimitNotFound = errors.New("adminapi: 供应商不存在")

// visibleProvider 取一个可见供应商（与 Node 的 findVisibleProvider 同语义）。
func (api *providersLimitAPI) visibleProvider(
	request *http.Request,
	providerID int64,
) (store.AdminProvider, error) {
	visible, err := providerVisibleProviders(request, api.deps)
	if err != nil {
		return store.AdminProvider{}, err
	}
	for _, provider := range visible {
		if provider.ID == providerID {
			return provider, nil
		}
	}
	return store.AdminProvider{}, errProviderLimitNotFound
}

// systemLocation 取系统时区（与 keys/users 侧同一取值链：system_setting → env → 默认）。
func (api *providersLimitAPI) systemLocation(ctx context.Context) (*time.Location, error) {
	raw, err := api.pools.AdminSystemTimezone(ctx)
	if err != nil {
		return nil, err
	}
	return config.ResolveLocationFromEnv(raw), nil
}

// usage 复刻一个供应商的 ProviderLimitUsageData（两个 action 共用同一形状）。
func (api *providersLimitAPI) usage(
	ctx context.Context,
	provider store.AdminProvider,
	now time.Time,
	location *time.Location,
	sessionCount int,
) (map[string]any, error) {
	resetMode5h := providerLimitResetMode(provider.Limit5hResetMode, limit.ResetRolling)
	dailyMode := providerLimitResetMode(provider.DailyResetMode, limit.ResetFixed)
	dailyResetTime := limit.NormalizeResetTime(provider.DailyResetTime)
	totalResetAt := providerLimitParseResetAt(provider.TotalCostResetAt)

	// 5h：fixed 读 Redis 运行态窗口（值 + 由 TTL 反推的重置时刻），rolling 读账本 [now-5h, now)。
	var cost5h string
	var resetAt5h *time.Time
	if resetMode5h == limit.ResetFixed {
		state, err := api.fixed5h.Fixed5hWindowState(ctx, limit.EntityProvider, provider.ID, now)
		if err != nil {
			return nil, err
		}
		cost5h = limit.FormatCostText(state.Current)
		resetAt5h = state.ResetAt
	} else {
		value, err := api.pools.SumLedgerCostInTimeRange(
			ctx, store.LedgerEntityProvider, provider.ID, now.Add(-5*time.Hour), now)
		if err != nil {
			return nil, err
		}
		cost5h = value
	}

	dailyStart := limit.WindowStart(limit.PeriodDaily, now, dailyResetTime, dailyMode, location)
	weeklyStart := limit.WindowStart(limit.PeriodWeekly, now, dailyResetTime, dailyMode, location)
	monthlyStart := limit.WindowStart(limit.PeriodMonthly, now, dailyResetTime, dailyMode, location)

	daily, err := api.pools.SumLedgerCostInTimeRange(
		ctx, store.LedgerEntityProvider, provider.ID, dailyStart, now)
	if err != nil {
		return nil, err
	}
	weekly, err := api.pools.SumLedgerCostInTimeRange(
		ctx, store.LedgerEntityProvider, provider.ID, weeklyStart, now)
	if err != nil {
		return nil, err
	}
	monthly, err := api.pools.SumLedgerCostInTimeRange(
		ctx, store.LedgerEntityProvider, provider.ID, monthlyStart, now)
	if err != nil {
		return nil, err
	}
	total, err := api.pools.SumLedgerTotalCost(ctx, store.LedgerEntityProvider, provider.ID, totalResetAt)
	if err != nil {
		return nil, err
	}

	dailyResetInfo := limit.ResetInfoFor(
		limit.PeriodDaily, now, dailyResetTime, dailyMode, nil, location)
	weeklyResetInfo := limit.ResetInfoFor(
		limit.PeriodWeekly, now, dailyResetTime, limit.ResetFixed, nil, location)
	monthlyResetInfo := limit.ResetInfoFor(
		limit.PeriodMonthly, now, dailyResetTime, limit.ResetFixed, nil, location)

	// daily 是唯一可能为滚动窗口的周期（滚动时 Node 给 undefined ⇒ 键缺席）。
	var dailyResetAt *time.Time
	if dailyResetInfo.Type != "rolling" {
		dailyResetAt = dailyResetInfo.ResetAt
	}

	return map[string]any{
		"cost5h": map[string]any{
			"current":   usersCostNumber(cost5h),
			"limit":     providerLimitAmount(provider.Limit5hUSD),
			"resetInfo": providerLimitResetInfoText(resetMode5h, resetAt5h),
		},
		"costDaily":     keysCostBucket(daily, providerLimitAmount(provider.LimitDailyUSD), dailyResetAt),
		"costWeekly":    keysCostBucket(weekly, providerLimitAmount(provider.LimitWeeklyUSD), weeklyResetInfo.ResetAt),
		"costMonthly":   keysCostBucket(monthly, providerLimitAmount(provider.LimitMonthlyUSD), monthlyResetInfo.ResetAt),
		"limitTotalUsd": keysCostBucket(total, providerLimitAmount(provider.LimitTotalUSD), totalResetAt),
		"concurrentSessions": map[string]any{
			"current": sessionCount,
			// 复用 dashboard.go:333 的同名语义（provider.limitConcurrentSessions ?? 0）。
			"limit": providerLimitConcurrentSessions(provider),
		},
	}, nil
}

// providerLimitResetInfoText 复刻 cost5h.resetInfo 的三态文案（actions/providers.ts:3195-3199）。
//
// 文案是**用户可见字符串**且 Node 侧同样是硬编码中文（无 i18n），故此处逐字对齐而非走 i18n。
func providerLimitResetInfoText(mode limit.ResetMode, resetAt *time.Time) string {
	if mode != limit.ResetFixed {
		return "滚动窗口（5 小时）"
	}
	if resetAt == nil {
		return "固定窗口（等待首次成功记账）"
	}
	return "固定窗口（重置于 " + store.JSONDate(*resetAt) + "）"
}

// providerLimitResetMode 把库里存的重置模式归一为枚举；空串按 Node 的 `?? fallback` 处理。
//
// 与 keysResetModeOrDefault 同语义（keys_usage.go:419），此处单列 provider 前缀以免两处
// 被误认为同一维度的默认值——两者的 fallback 方向相反（见文件头 §2）。
func providerLimitResetMode(raw string, fallback limit.ResetMode) limit.ResetMode {
	if raw == "" {
		return fallback
	}
	if raw == string(limit.ResetRolling) {
		return limit.ResetRolling
	}
	return limit.ResetFixed
}

// providerLimitAmount 把可空的 numeric 额度原样交给 JSON（nil → null）。
//
// 类型必须是 number 而非字符串：Node 的 drizzle 把 numeric 映射成 number，
// 故 ProviderLimitUsageData.limit 是 number|null（与 store.AdminProvider 的 ::float8 同判）。
func providerLimitAmount(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

// providerLimitParseResetAt 解析 providers 读档里 total_cost_reset_at 的 ISO 文本
// （admin_providers.go:127 用 to_char 直出 `YYYY-MM-DDTHH:MM:SS.mmmZ`）。
func providerLimitParseResetAt(raw *string) *time.Time {
	if raw == nil || *raw == "" {
		return nil
	}
	if parsed, err := time.Parse("2006-01-02T15:04:05.000Z", *raw); err == nil {
		return &parsed
	}
	if parsed, err := time.Parse(time.RFC3339Nano, *raw); err == nil {
		return &parsed
	}
	return nil
}
