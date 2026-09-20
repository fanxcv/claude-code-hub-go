package adminapi

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 dashboard 资源族的**实时面**两条端点：
//
//	GET /dashboard/proxy-status  admin ← ProxyStatusTracker（库内聚合）
//	GET /dashboard/realtime      admin ← actions/dashboard-realtime.ts 的 getDashboardRealtimeData
//
// 两条端点的**数据源**先写清楚，避免后人按「进程内观测态」去改：
//
//  1. proxy-status 走**库内聚合**（与 Node 的 ProxyStatusTracker 同源，见
//     internal/store/admin_live.go 的文件头）。数据面不写任何观测键，这里也不读。
//  2. realtime 是**七份数据的拼装**（Node 用 Promise.allSettled 实现部分失败容错）：
//     metrics / activityStream / userRankings / providerRankings / providerSlots /
//     modelDistribution / trendData。其中 metrics、providerSlots、trendData 直接复用同族
//     端点的既有组装逻辑（overview / provider-slots / statistics），排行榜三项复用
//     leaderboard 资源族的条目组装（Node 侧也是直接调 repository 的 findDaily*，不过缓存）。
//
// 两处**流式差异登记**：这两条端点在 Node 与 Go 都是**单响应**（无 SSE/长连接），前端靠
// 定时轮询刷新；故无需帧序列比对，也不引入任何推送面。

// dashboardProxyStatusActiveBody 逐字对应 types/proxy-status.ts 的 activeRequests 元素。
type dashboardProxyStatusActiveBody struct {
	RequestID    int64  `json:"requestId"`
	KeyName      string `json:"keyName"`
	ProviderID   int64  `json:"providerId"`
	ProviderName string `json:"providerName"`
	Model        string `json:"model"`
	StartTime    int64  `json:"startTime"`
	Duration     int64  `json:"duration"`
}

// dashboardProxyStatusLastBody 逐字对应 types/proxy-status.ts 的 lastRequest。
type dashboardProxyStatusLastBody struct {
	RequestID    int64  `json:"requestId"`
	KeyName      string `json:"keyName"`
	ProviderID   int64  `json:"providerId"`
	ProviderName string `json:"providerName"`
	Model        string `json:"model"`
	EndTime      int64  `json:"endTime"`
	Elapsed      int64  `json:"elapsed"`
}

// dashboardProxyStatusUserBody 逐字对应 types/proxy-status.ts 的 ProxyStatusResponse.users 元素。
type dashboardProxyStatusUserBody struct {
	UserID         int64                            `json:"userId"`
	UserName       string                           `json:"userName"`
	ActiveCount    int                              `json:"activeCount"`
	ActiveRequests []dashboardProxyStatusActiveBody `json:"activeRequests"`
	LastRequest    *dashboardProxyStatusLastBody    `json:"lastRequest"`
}

// handleProxyStatus 复刻 actions/proxy-status.ts 的 getProxyStatus。
//
// 权限档位已由路由（AccessAdmin）保证，与 Node 的 requireAuth("admin") 同义；处理器内不再重复判定。
func (api *dashboardAPI) handleProxyStatus(writer http.ResponseWriter, request *http.Request) {
	ctx := request.Context()
	users, err := api.pools.AdminProxyStatusUsers(ctx)
	if err != nil {
		api.writeDashboardFailure(writer, request, "proxy-status", err)
		return
	}
	activeRows, err := api.pools.AdminProxyStatusActiveRequests(ctx)
	if err != nil {
		api.writeDashboardFailure(writer, request, "proxy-status", err)
		return
	}
	lastRows, err := api.pools.AdminProxyStatusLastRequests(ctx)
	if err != nil {
		api.writeDashboardFailure(writer, request, "proxy-status", err)
		return
	}

	now := api.now().UnixMilli()

	activeByUser := make(map[int64][]dashboardProxyStatusActiveBody, len(activeRows))
	for _, row := range activeRows {
		startTime := now
		if row.CreatedAt != nil {
			startTime = row.CreatedAt.UnixMilli()
		}
		activeByUser[row.UserID] = append(activeByUser[row.UserID], dashboardProxyStatusActiveBody{
			RequestID:    row.RequestID,
			KeyName:      proxyStatusKeyName(row.KeyName, row.KeyString),
			ProviderID:   row.ProviderID,
			ProviderName: row.ProviderName,
			Model:        proxyStatusModel(row.Model),
			StartTime:    startTime,
			Duration:     now - startTime,
		})
	}

	lastByUser := make(map[int64]store.AdminProxyStatusLastRow, len(lastRows))
	for _, row := range lastRows {
		// DISTINCT ON 已保证每人一行；这里再挡一次，语义与 Node 的 `if (!lastMap.has(userId))`
		// 一致（保留第一条）。
		if _, seen := lastByUser[row.UserID]; !seen {
			lastByUser[row.UserID] = row
		}
	}

	items := make([]dashboardProxyStatusUserBody, 0, len(users))
	for _, user := range users {
		activeRequests := activeByUser[user.UserID]
		if activeRequests == nil {
			// Node 的 `?? []`：无活跃请求时是空数组，不是 null。
			activeRequests = []dashboardProxyStatusActiveBody{}
		}

		var lastRequest *dashboardProxyStatusLastBody
		if row, ok := lastByUser[user.UserID]; ok {
			endTime := now
			if row.EndTime != nil {
				endTime = row.EndTime.UnixMilli()
			}
			lastRequest = &dashboardProxyStatusLastBody{
				RequestID:    row.RequestID,
				KeyName:      proxyStatusKeyName(row.KeyName, row.KeyString),
				ProviderID:   row.ProviderID,
				ProviderName: row.ProviderName,
				Model:        proxyStatusModel(row.Model),
				EndTime:      endTime,
				Elapsed:      now - endTime,
			}
		}

		items = append(items, dashboardProxyStatusUserBody{
			UserID:         user.UserID,
			UserName:       user.UserName,
			ActiveCount:    len(activeRequests),
			ActiveRequests: activeRequests,
			LastRequest:    lastRequest,
		})
	}

	adminWriteJSON(writer, http.StatusOK, struct {
		Users []dashboardProxyStatusUserBody `json:"users"`
	}{Users: items})
}

// proxyStatusKeyName 复刻 `row.keyName ?? maskKey(row.keyString)`。
func proxyStatusKeyName(keyName *string, keyString string) string {
	if keyName != nil {
		return *keyName
	}
	return usersMaskKey(keyString)
}

// proxyStatusModel 复刻 `row.model || "unknown"`（空串也落到 unknown，是 `||` 不是 `??`）。
func proxyStatusModel(model *string) string {
	if model == nil || *model == "" {
		return "unknown"
	}
	return *model
}

// dashboardRealtimeTrendPoint 逐字对应 trendData 的元素。
type dashboardRealtimeTrendPoint struct {
	Hour  int   `json:"hour"`
	Value int64 `json:"value"`
}

// dashboardRealtimeActivityBody 逐字对应 ActivityStreamEntry（dashboard-realtime.ts:24-44）。
type dashboardRealtimeActivityBody struct {
	ID        string  `json:"id"`
	User      string  `json:"user"`
	Model     string  `json:"model"`
	Provider  string  `json:"provider"`
	Latency   int64   `json:"latency"`
	Status    int64   `json:"status"`
	Cost      float64 `json:"cost"`
	StartTime int64   `json:"startTime"`
}

// dashboardRealtimeUserRankingBody 逐字对应 repository 的 LeaderboardEntry（**不含**币种格式化
// 字段：Node 的 realtime 直调 repository，格式化的 `totalCostFormatted` 是 /api/leaderboard 路由
// 层才加的）。
type dashboardRealtimeUserRankingBody struct {
	UserID        int64   `json:"userId"`
	UserName      string  `json:"userName"`
	TotalRequests float64 `json:"totalRequests"`
	TotalCost     float64 `json:"totalCost"`
	TotalTokens   float64 `json:"totalTokens"`
}

// dashboardRealtimeProviderRankingBody 逐字对应 repository 的 ProviderLeaderboardEntry。
type dashboardRealtimeProviderRankingBody struct {
	ProviderID              int64    `json:"providerId"`
	ProviderName            string   `json:"providerName"`
	TotalRequests           float64  `json:"totalRequests"`
	TotalCost               float64  `json:"totalCost"`
	TotalTokens             float64  `json:"totalTokens"`
	SuccessRate             *float64 `json:"successRate"`
	AvgTtftMS               float64  `json:"avgTtftMs"`
	AvgTokensPerSecond      float64  `json:"avgTokensPerSecond"`
	CacheCoefficientBP      *int64   `json:"cacheCoefficientBp"`
	AvgCostPerRequest       *float64 `json:"avgCostPerRequest"`
	AvgCostPerMillionTokens *float64 `json:"avgCostPerMillionTokens"`
}

// dashboardRealtimeModelDistributionBody 逐字对应 repository 的 ModelLeaderboardEntry。
type dashboardRealtimeModelDistributionBody struct {
	Model                        string   `json:"model"`
	TotalRequests                float64  `json:"totalRequests"`
	TotalCost                    float64  `json:"totalCost"`
	TotalTokens                  float64  `json:"totalTokens"`
	SuccessRate                  *float64 `json:"successRate"`
	RowIdentityBasis             string   `json:"rowIdentityBasis"`
	SuccessRateBasis             string   `json:"successRateBasis"`
	CostTokensBasis              string   `json:"costTokensBasis"`
	BasisDisclosureRequired      bool     `json:"basisDisclosureRequired"`
	SuccessRateUnavailableReason *string  `json:"successRateUnavailableReason,omitempty"`
}

// dashboardRealtimeBody 逐字对应 DashboardRealtimeData，键序一致。
type dashboardRealtimeBody struct {
	Metrics           dashboardOverviewBody                    `json:"metrics"`
	ActivityStream    []dashboardRealtimeActivityBody          `json:"activityStream"`
	UserRankings      []dashboardRealtimeUserRankingBody       `json:"userRankings"`
	ProviderRankings  []dashboardRealtimeProviderRankingBody   `json:"providerRankings"`
	ProviderSlots     []dashboardProviderSlotBody              `json:"providerSlots"`
	ModelDistribution []dashboardRealtimeModelDistributionBody `json:"modelDistribution"`
	TrendData         []dashboardRealtimeTrendPoint            `json:"trendData"`
}

// 两个与 Node 同值的硬编码上限（dashboard-realtime.ts:66-67）。
const (
	dashboardActivityStreamLimit    = 20
	dashboardModelDistributionLimit = 10
)

// handleRealtime 复刻 getDashboardRealtimeData。
//
// 容错口径照抄 Node 的 allSettled：**概览失败整条失败**（Node 在 overview 拿不到时直接返回
// 错误），其余六份数据失败则各自退化为空集合（Node 的 fallback），不牵连整份作答。
func (api *dashboardAPI) handleRealtime(writer http.ResponseWriter, request *http.Request) {
	ctx := request.Context()
	principal, _ := PrincipalFrom(ctx)

	metrics, err := api.overviewBody(ctx, principal)
	if err != nil {
		api.writeDashboardFailure(writer, request, "realtime", err)
		return
	}

	settings, err := api.pools.FindSystemSettings(ctx)
	if err != nil {
		api.writeDashboardFailure(writer, request, "realtime", err)
		return
	}
	timezone := api.pools.AdminSystemTimezoneOrUTC(ctx)
	currency := "USD"
	if settings.CurrencyDisplay != "" {
		currency = settings.CurrencyDisplay
	}

	activityStream := api.realtimeActivityStream(ctx, api.now().UnixMilli())

	// 排行榜三项：Node 直接调 repository 的 findDaily*（不带筛选、不带模型拆分）。
	query := store.AdminLeaderboardQuery{
		Period:             "daily",
		Timezone:           timezone,
		BillingModelSource: settings.BillingModelSource,
	}
	leaderboards := newLeaderboardAPIForReuse(api.deps, api.logger)

	userRankings := make([]leaderboardUserEntry, 0)
	if raw, err := leaderboards.userLeaderboard(ctx, query, currency, false); err != nil {
		api.logger.Warn("dashboard_realtime_user_rankings_failed", map[string]any{
			"error": err.Error(),
		})
	} else if entries, ok := leaderboardEntriesOf[leaderboardUserEntry](raw); ok {
		userRankings = entries
	} else {
		api.logger.Error("dashboard_realtime_user_rankings_type", map[string]any{
			"got": fmt.Sprintf("%T", raw),
		})
	}

	providerRankings := make([]leaderboardProviderEntry, 0)
	if raw, err := leaderboards.providerLeaderboard(ctx, query, currency, false); err != nil {
		api.logger.Warn("dashboard_realtime_provider_rankings_failed", map[string]any{
			"error": err.Error(),
		})
	} else if entries, ok := leaderboardEntriesOf[leaderboardProviderEntry](raw); ok {
		providerRankings = entries
	} else {
		api.logger.Error("dashboard_realtime_provider_rankings_type", map[string]any{
			"got": fmt.Sprintf("%T", raw),
		})
	}

	modelDistribution := make([]leaderboardModelEntry, 0)
	if raw, err := leaderboards.modelLeaderboard(ctx, query, currency); err != nil {
		api.logger.Warn("dashboard_realtime_model_rankings_failed", map[string]any{
			"error": err.Error(),
		})
	} else if entries, ok := leaderboardEntriesOf[leaderboardModelEntry](raw); ok {
		modelDistribution = entries
	} else {
		api.logger.Error("dashboard_realtime_model_rankings_type", map[string]any{
			"got": fmt.Sprintf("%T", raw),
		})
	}

	slots, err := api.providerSlotItems(ctx)
	if err != nil {
		// Node 侧插槽失败退化为空数组（allSettled 的 fallback）。
		api.logger.Warn("dashboard_realtime_provider_slots_failed", map[string]any{
			"error": err.Error(),
		})
		slots = nil
	}

	adminWriteJSON(writer, http.StatusOK, dashboardRealtimeBody{
		Metrics:           metrics,
		ActivityStream:    activityStream,
		UserRankings:      dashboardRealtimeUserRankings(userRankings, 5),
		ProviderRankings:  dashboardRealtimeProviderRankings(providerRankings),
		ProviderSlots:     dashboardRealtimeProviderSlots(slots, providerRankings),
		ModelDistribution: dashboardRealtimeModelDistribution(modelDistribution),
		TrendData:         api.realtimeTrendData(ctx, principal),
	})
}

// leaderboardEntriesOf 把排行榜的 `any` 作答还原成具体条目切片。
//
// 两个入参形态都支援：scope 分支返回的是切片本身（`[]T`），空作答分支返回的是指向空切片的
// 指针（`*[]T`）。第二个返回值为 false 表示**形态对不上**——调用方必须记错误而不是静默退化成
// 空数组（静默退化会让大屏上的排行榜恒空，而日志里一点痕迹都没有）。
func leaderboardEntriesOf[T any](raw any) ([]T, bool) {
	switch value := raw.(type) {
	case []T:
		return value, true
	case *[]T:
		if value == nil {
			return nil, true
		}
		return *value, true
	default:
		return nil, false
	}
}

// realtimeActivityStream 复刻 findRecentActivityStream 的混合数据源（Redis 活跃 + 库内最新）。
//
// Node 侧任何一处失败都退化为「已拿到的部分」，整体不报错；这里同样（清单读失败按空清单走，
// 于是退化成纯库内最新 20 条）。
func (api *dashboardAPI) realtimeActivityStream(ctx context.Context, now int64) []dashboardRealtimeActivityBody {
	identities, err := api.sessions.ObservedSessionIdentities(ctx)
	if err != nil {
		api.logger.Warn("dashboard_realtime_active_sessions_failed", map[string]any{
			"error": err.Error(),
		})
		identities = nil
	}

	rows := make([]store.AdminActivityRow, 0, dashboardActivityStreamLimit)
	if len(identities) > 0 {
		latest, latestErr := api.pools.AdminActivityLatestRequestsBySessions(ctx, identities,
			dashboardActivityStreamLimit)
		if latestErr != nil {
			api.logger.Warn("dashboard_realtime_activity_latest_failed", map[string]any{
				"error": latestErr.Error(),
			})
		} else {
			rows = append(rows, latest...)
		}
	}

	if len(rows) < dashboardActivityStreamLimit {
		excluded := make([]string, 0, len(rows))
		for _, row := range rows {
			if row.SessionID != nil {
				excluded = append(excluded, *row.SessionID)
			}
		}
		remaining := dashboardActivityStreamLimit - len(rows)
		recent, recentErr := api.pools.AdminActivityRecentRequests(ctx, remaining, excluded)
		if recentErr != nil {
			api.logger.Warn("dashboard_realtime_activity_recent_failed", map[string]any{
				"error": recentErr.Error(),
			})
		} else {
			rows = append(rows, recent...)
		}
	}

	// 第 4 步：按 startTime 降序、按请求 id 去重、截断到上限。
	seen := make(map[int64]struct{}, len(rows))
	unique := make([]store.AdminActivityRow, 0, len(rows))
	for _, row := range rows {
		if _, ok := seen[row.RequestID]; ok {
			continue
		}
		seen[row.RequestID] = struct{}{}
		unique = append(unique, row)
	}
	sort.SliceStable(unique, func(i, j int) bool {
		return dashboardRealtimeStartTime(unique[i], now) > dashboardRealtimeStartTime(unique[j], now)
	})
	if len(unique) > dashboardActivityStreamLimit {
		unique = unique[:dashboardActivityStreamLimit]
	}

	items := make([]dashboardRealtimeActivityBody, 0, len(unique))
	for _, row := range unique {
		startTime := dashboardRealtimeStartTime(row, now)
		// 终局判据照抄 Node：statusCode 或 durationMs 有一个不为空即视为已终局；
		// 未终局的供应商与状态码都会变（fallback/hedge），故作答里留空/留 0。
		finalized := row.StatusCode != nil || row.DurationMS != nil

		latency := now - startTime
		if row.DurationMS != nil {
			latency = *row.DurationMS
		}
		var status int64
		if finalized {
			status = 200
			if row.StatusCode != nil {
				status = *row.StatusCode
			}
		}
		provider := ""
		if finalized {
			provider = "Unknown"
			if row.ProviderName != nil {
				provider = *row.ProviderName
			}
		}

		id := "req-" + strconv.FormatInt(row.RequestID, 10)
		if row.SessionID != nil {
			id = *row.SessionID
		}
		user := "Unknown"
		if row.UserName != nil {
			user = *row.UserName
		}

		items = append(items, dashboardRealtimeActivityBody{
			ID:        id,
			User:      user,
			Model:     dashboardRealtimeModel(row.OriginalModel, row.Model),
			Provider:  provider,
			Latency:   latency,
			Status:    status,
			Cost:      dashboardRealtimeCost(row.CostUSD),
			StartTime: startTime,
		})
	}
	return items
}

// dashboardRealtimeStartTime 复刻 `row.createdAt ? new Date(createdAt).getTime() : Date.now()`。
func dashboardRealtimeStartTime(row store.AdminActivityRow, now int64) int64 {
	if row.CreatedAt == nil {
		return now
	}
	return row.CreatedAt.UnixMilli()
}

// dashboardRealtimeModel 复刻 `item.originalModel ?? item.model ?? "Unknown"`（nullish 语义）。
func dashboardRealtimeModel(originalModel, model *string) string {
	if originalModel != nil {
		return *originalModel
	}
	if model != nil {
		return *model
	}
	return "Unknown"
}

// dashboardRealtimeCost 复刻 `parseFloat(item.costUsd ?? "0")`。
func dashboardRealtimeCost(costUSD *string) float64 {
	if costUSD == nil {
		return 0
	}
	return strconvFloatText(*costUSD)
}

// realtimeTrendData 复刻 trendData：把 getUserStatistics("today") 的 chartData 每个桶折算成
// 「UTC 小时 + 该桶的调用数合计」。
//
// 两处照抄：桶的 hour 取**UTC**小时（Node 的 getUTCHours）；统计面失败时退化成 24 个 0
// （Node 在 statisticsData 为 null 时才走这条兜底，空数组不算 null，故空数组就是空数组）。
func (api *dashboardAPI) realtimeTrendData(
	ctx context.Context,
	principal Principal,
) []dashboardRealtimeTrendPoint {
	settings, err := api.pools.FindSystemSettings(ctx)
	if err != nil {
		api.logger.Warn("dashboard_realtime_trend_settings_failed", map[string]any{
			"error": err.Error(),
		})
		return dashboardRealtimeZeroTrend()
	}
	location, err := meSystemLocation(ctx, api.pools)
	if err != nil {
		api.logger.Warn("dashboard_realtime_trend_timezone_failed", map[string]any{
			"error": err.Error(),
		})
		return dashboardRealtimeZeroTrend()
	}

	mode := "keys"
	if principal.IsAdmin {
		mode = "users"
	} else if settings.AllowGlobalUsageView {
		mode = "mixed"
	}

	points, _, _, err := api.statisticsData(ctx, principal, store.AdminStatisticsToday, mode,
		location.String())
	if err != nil {
		api.logger.Warn("dashboard_realtime_trend_failed", map[string]any{
			"error": err.Error(),
		})
		return dashboardRealtimeZeroTrend()
	}

	// chartData 是「一桶一条」：同一桶内的多个实体累加，桶按时间升序（Node 侧由 SQL 的
	// 日期序保证）。用有序切片而不是 map 累加，键序与 Node 一致。
	type bucketCalls struct {
		at    time.Time
		value int64
	}
	indexByBucket := make(map[int64]int, len(points))
	buckets := make([]bucketCalls, 0)
	for _, point := range points {
		key := point.Bucket.UnixMilli()
		at, seen := indexByBucket[key]
		if !seen {
			buckets = append(buckets, bucketCalls{at: point.Bucket})
			at = len(buckets) - 1
			indexByBucket[key] = at
		}
		buckets[at].value += point.APICalls
	}
	sort.SliceStable(buckets, func(i, j int) bool { return buckets[i].at.Before(buckets[j].at) })

	trend := make([]dashboardRealtimeTrendPoint, 0, len(buckets))
	for _, bucket := range buckets {
		trend = append(trend, dashboardRealtimeTrendPoint{
			Hour:  bucket.at.UTC().Hour(),
			Value: bucket.value,
		})
	}
	return trend
}

// dashboardRealtimeZeroTrend 是统计面不可用时的兜底（Node 的 24 个 0）。
func dashboardRealtimeZeroTrend() []dashboardRealtimeTrendPoint {
	trend := make([]dashboardRealtimeTrendPoint, 0, 24)
	for hour := 0; hour < 24; hour++ {
		trend = append(trend, dashboardRealtimeTrendPoint{Hour: hour, Value: 0})
	}
	return trend
}

// dashboardRealtimeUserRankings 复刻 `userRankings.slice(0, 5)` 并投影成 repository 形状。
func dashboardRealtimeUserRankings(
	values []leaderboardUserEntry,
	limit int,
) []dashboardRealtimeUserRankingBody {
	if len(values) > limit {
		values = values[:limit]
	}
	items := make([]dashboardRealtimeUserRankingBody, 0, len(values))
	for _, value := range values {
		items = append(items, dashboardRealtimeUserRankingBody{
			UserID:        value.UserID,
			UserName:      value.UserName,
			TotalRequests: value.TotalRequests,
			TotalCost:     value.TotalCost,
			TotalTokens:   value.TotalTokens,
		})
	}
	return items
}

// dashboardRealtimeModelDistribution 复刻 `modelRankings.slice(0, 10)` 并投影成 repository 形状。
func dashboardRealtimeModelDistribution(
	values []leaderboardModelEntry,
) []dashboardRealtimeModelDistributionBody {
	if len(values) > dashboardModelDistributionLimit {
		values = values[:dashboardModelDistributionLimit]
	}
	items := make([]dashboardRealtimeModelDistributionBody, 0, len(values))
	for _, value := range values {
		items = append(items, dashboardRealtimeModelDistributionBody{
			Model:                        value.Model,
			TotalRequests:                value.TotalRequests,
			TotalCost:                    value.TotalCost,
			TotalTokens:                  value.TotalTokens,
			SuccessRate:                  value.SuccessRate,
			RowIdentityBasis:             value.RowIdentityBasis,
			SuccessRateBasis:             value.SuccessRateBasis,
			CostTokensBasis:              value.CostTokensBasis,
			BasisDisclosureRequired:      value.BasisDisclosureRequired,
			SuccessRateUnavailableReason: value.SuccessRateUnavailableReason,
		})
	}
	return items
}

// dashboardRealtimeProviderRankings 复刻 realtime 对供应商排行的二次排序：按金额降序取前 5，
// 并投影成 repository 形状（丢掉格式化字段）。
func dashboardRealtimeProviderRankings(
	values []leaderboardProviderEntry,
) []dashboardRealtimeProviderRankingBody {
	sorted := make([]leaderboardProviderEntry, len(values))
	copy(sorted, values)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].TotalCost > sorted[j].TotalCost
	})
	if len(sorted) > 5 {
		sorted = sorted[:5]
	}
	items := make([]dashboardRealtimeProviderRankingBody, 0, len(sorted))
	for _, value := range sorted {
		items = append(items, dashboardRealtimeProviderRankingBody{
			ProviderID:              value.ProviderID,
			ProviderName:            value.ProviderName,
			TotalRequests:           value.TotalRequests,
			TotalCost:               value.TotalCost,
			TotalTokens:             value.TotalTokens,
			SuccessRate:             value.SuccessRate,
			AvgTtftMS:               value.AvgTtftMS,
			AvgTokensPerSecond:      value.AvgTokensPerSecond,
			CacheCoefficientBP:      value.CacheCoefficientBP,
			AvgCostPerRequest:       value.AvgCostPerRequest,
			AvgCostPerMillionTokens: value.AvgCostPerMillionTokens,
		})
	}
	return items
}

// dashboardRealtimeProviderSlots 复刻 realtime 对插槽的处理：过滤未设限额的、回填今日 token
// 总量、按占用率降序、取前 3。
func dashboardRealtimeProviderSlots(
	slots []dashboardProviderSlotBody,
	rankings []leaderboardProviderEntry,
) []dashboardProviderSlotBody {
	volumeByProvider := make(map[int64]float64, len(rankings))
	for _, ranking := range rankings {
		volumeByProvider[ranking.ProviderID] = ranking.TotalTokens
	}

	filtered := make([]dashboardProviderSlotBody, 0, len(slots))
	for _, slot := range slots {
		if slot.TotalSlots <= 0 {
			continue
		}
		slot.TotalVolume = volumeByProvider[slot.ProviderID]
		filtered = append(filtered, slot)
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		return dashboardRealtimeSlotUsage(filtered[i]) > dashboardRealtimeSlotUsage(filtered[j])
	})
	if len(filtered) > 3 {
		filtered = filtered[:3]
	}
	return filtered
}

// dashboardRealtimeSlotUsage 复刻 `usedSlots / totalSlots`（totalSlots 已过滤为正数）。
func dashboardRealtimeSlotUsage(slot dashboardProviderSlotBody) float64 {
	if slot.TotalSlots <= 0 {
		return 0
	}
	return float64(slot.UsedSlots) / float64(slot.TotalSlots)
}
