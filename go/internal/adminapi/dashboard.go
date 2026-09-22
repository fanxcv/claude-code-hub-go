package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"

	"github.com/fanxcv/claude-code-hub-go/go/internal/clientver"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是管理面的 **dashboard 资源**（/api/v1/dashboard/*）中**已就绪**的八条读端点：
//
//	GET /dashboard/overview             read  ← 本文件
//	GET /dashboard/concurrent-sessions  read  ← 本文件
//	GET /dashboard/provider-slots       admin ← 本文件
//	GET /dashboard/rate-limit-stats     admin ← 本文件
//	GET /dashboard/client-versions      admin ← 本文件
//	GET /dashboard/statistics           read  ← 本文件（图表数据，三种模式）
//	GET /dashboard/proxy-status         admin ← dashboard_live.go（库内聚合）
//	GET /dashboard/realtime             admin ← dashboard_live.go（七份数据拼装）
//
// **已实现的模拟器**（2026-09-13）：
//
//	POST /dashboard/dispatch-simulator:simulate ← dashboard_simulator.go（引擎在 internal/route
//	的 simulate.go）。原注释担心的「凭选路函数猜输出形状会与前端对不上」已消除：输出契约
//	在 src/types/dispatch-simulator.ts，引擎按那 73 行契约逐字段构造，九步与 Node 的
//	simulateDispatchDecisionTree 同序同文案。
//
// **仍未实现的一条**：
//
//	provider-slots 的 totalVolume —— Node 侧**恒为 0**并注明「由调用方从排行榜数据填充」，
//	Go 侧照抄 0；realtime 里的插槽总量才由排行榜回填。

// dashboardAPI 是 dashboard 资源处理器的依赖集合。
type dashboardAPI struct {
	pools    *store.Pools
	problems ProblemWriter
	logger   *logx.Logger
	// deps 留给需要复用其它资源族组装逻辑的端点（realtime 的排行榜条目就复用 leaderboard 的）。
	deps Deps
	// sessions 为 nil 时依赖它的四条端点（overview / concurrent-sessions / provider-slots /
	// realtime）不注册：恒 0 的并发数在大屏上是静默错数。
	sessions ObservedSessionRuntime
	now      func() time.Time
}

// newLeaderboardAPIForReuse 构造一个只用于**取原始条目**的排行榜 API（cache 为 nil）。
//
// 为什么可以不带缓存：Node 的 dashboard/realtime 直接调 repository 的 `findDaily*`（不过那层
// 乐观缓存），故这里也走 computeLeaderboard 的直读路径。
func newLeaderboardAPIForReuse(deps Deps, logger *logx.Logger) *leaderboardAPI {
	return &leaderboardAPI{pools: deps.Store, deps: deps, logger: logger}
}

// RegisterDashboardRoutes 注册 dashboard 资源**已就绪**的端点（见文件头的四条例外）。
func RegisterDashboardRoutes(router *Router, deps Deps) {
	if deps.Store == nil {
		if deps.Logger != nil {
			deps.Logger.Error("admin_dashboard_store_unwired", map[string]any{
				"module": "dashboard",
				"action": "routes_not_registered",
			})
		}
		return
	}
	logger := deps.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	problems := deps.Problems
	if problems == nil {
		problems = NewProblems(logger)
	}
	api := &dashboardAPI{
		pools:    deps.Store,
		problems: problems,
		logger:   logger,
		deps:     deps,
		sessions: deps.ObservedSessions,
		now:      time.Now,
	}

	type entry struct {
		method string
		path   string
		access AccessLevel
		op     string
		handle http.HandlerFunc
		// needsSessions 表示这条端点要先拿得到会话观测读数才注册。
		needsSessions bool
	}
	entries := []entry{
		{http.MethodGet, "/dashboard/overview", AccessRead, "getDashboardOverview",
			api.handleOverview, true},
		{http.MethodGet, "/dashboard/concurrent-sessions", AccessRead,
			"getDashboardConcurrentSessions", api.handleConcurrentSessions, true},
		{http.MethodGet, "/dashboard/provider-slots", AccessAdmin, "getDashboardProviderSlots",
			api.handleProviderSlots, true},
		{http.MethodGet, "/dashboard/rate-limit-stats", AccessAdmin, "getDashboardRateLimitStats",
			api.handleRateLimitStats, false},
		{http.MethodGet, "/dashboard/client-versions", AccessAdmin, "getDashboardClientVersions",
			api.handleClientVersions, false},
		{http.MethodGet, "/dashboard/statistics", AccessRead, "getDashboardStatistics",
			api.handleStatistics, false},
		{http.MethodGet, "/dashboard/proxy-status", AccessAdmin, "getDashboardProxyStatus",
			api.handleProxyStatus, false},
		{http.MethodGet, "/dashboard/realtime", AccessAdmin, "getDashboardRealtime",
			api.handleRealtime, true},
		// 调度模拟：纯计算 + 只读读数，故不依赖会话观测（needsSessions=false）。
		{http.MethodPost, "/dashboard/dispatch-simulator:simulate", AccessAdmin,
			"simulateDispatchDecisionTree", api.handleDispatchSimulator, false},
	}
	for _, item := range entries {
		if item.needsSessions && api.sessions == nil {
			api.logger.Warn("admin_dashboard_runtime_unwired", map[string]any{
				"path":   item.path,
				"reason": "observed_session_runtime_missing",
				"action": "route_not_registered",
			})
			continue
		}
		router.Add(Route{
			Method:      item.method,
			Path:        item.path,
			Access:      item.access,
			Module:      "dashboard",
			OperationID: item.op,
			Handler:     item.handle,
		})
	}
}

// dashboardOverviewBody 逐字对应 Node 的 OverviewData（overview.ts:16-38），键序一致。
type dashboardOverviewBody struct {
	ConcurrentSessions                 int     `json:"concurrentSessions"`
	TodayRequests                      int64   `json:"todayRequests"`
	TodayCost                          float64 `json:"todayCost"`
	AvgResponseTime                    float64 `json:"avgResponseTime"`
	TodayErrorRate                     float64 `json:"todayErrorRate"`
	YesterdaySamePeriodRequests        int64   `json:"yesterdaySamePeriodRequests"`
	YesterdaySamePeriodCost            float64 `json:"yesterdaySamePeriodCost"`
	YesterdaySamePeriodAvgResponseTime float64 `json:"yesterdaySamePeriodAvgResponseTime"`
	RecentMinuteRequests               int64   `json:"recentMinuteRequests"`
}

// handleOverview 复刻 getOverviewData（overview.ts:46-108）。
//
// 两处照抄的权限语义：
//   - 范围：管理员或 allowGlobalUsageView 为真时看全站（userId = nil），否则只看自己。
//   - **并发数只有管理员看得到**：非管理员恒 0（Node 里那条 `isAdmin ? ... : 0`），
//     哪怕 allowGlobalUsageView 为真也不放宽——大屏的并发数是全站读数。
func (api *dashboardAPI) handleOverview(writer http.ResponseWriter, request *http.Request) {
	ctx := request.Context()
	principal, _ := PrincipalFrom(ctx)
	overview, err := api.overviewBody(ctx, principal)
	if err != nil {
		api.writeDashboardFailure(writer, request, "overview", err)
		return
	}
	adminWriteJSON(writer, http.StatusOK, overview)
}

// overviewBody 算出概览作答（overview 与 realtime 的 metrics 共用同一份）。
func (api *dashboardAPI) overviewBody(
	ctx context.Context,
	principal Principal,
) (dashboardOverviewBody, error) {
	settings, err := api.pools.FindSystemSettings(ctx)
	if err != nil {
		return dashboardOverviewBody{}, err
	}
	location, err := meSystemLocation(ctx, api.pools)
	if err != nil {
		return dashboardOverviewBody{}, err
	}

	canViewGlobal := principal.IsAdmin || settings.AllowGlobalUsageView
	var scopedUserID *int64
	if !canViewGlobal {
		userID := principal.UserID
		scopedUserID = &userID
	}

	overview, err := api.pools.AdminDashboardOverview(ctx, scopedUserID, location.String())
	if err != nil {
		return dashboardOverviewBody{}, err
	}

	concurrentSessions := 0
	if principal.IsAdmin {
		count, countErr := api.sessions.ObservedSessionCount(ctx)
		if countErr != nil {
			// Node 侧单个读数失败是 fail-open（记日志、计 0），不牵连整份概览。
			api.logger.Warn("dashboard_overview_concurrent_failed", map[string]any{
				"error": countErr.Error(),
			})
		} else {
			concurrentSessions = count
		}
	}

	return dashboardOverviewBody{
		ConcurrentSessions:                 concurrentSessions,
		TodayRequests:                      overview.TodayRequests,
		TodayCost:                          roundCost6(overview.TodayCost),
		AvgResponseTime:                    math.Round(overview.TodayAvgDurationMs),
		TodayErrorRate:                     errorRate(overview.TodayErrorCount, overview.TodayRequests),
		YesterdaySamePeriodRequests:        overview.YesterdaySamePeriodRequests,
		YesterdaySamePeriodCost:            roundCost6(overview.YesterdaySamePeriodCost),
		YesterdaySamePeriodAvgResponseTime: math.Round(overview.YesterdaySamePeriodAvgDurationMs),
		RecentMinuteRequests:               overview.RecentMinuteRequests,
	}, nil
}

// handleConcurrentSessions 复刻 getDashboardConcurrentSessions（dashboard/handlers.ts:24-46）。
//
// 非管理员且 allowGlobalUsageView 为假时作答 403 dashboard.global_usage_forbidden
// （注意这条端点的档位是 read，普通用户能进到处理器里，所以这道门是必需的）。
func (api *dashboardAPI) handleConcurrentSessions(
	writer http.ResponseWriter,
	request *http.Request,
) {
	ctx := request.Context()
	principal, _ := PrincipalFrom(ctx)
	if !principal.IsAdmin {
		settings, err := api.pools.FindSystemSettings(ctx)
		if err != nil {
			api.writeDashboardFailure(writer, request, "concurrent-sessions", err)
			return
		}
		if !settings.AllowGlobalUsageView {
			api.problems.WriteProblem(writer, request, http.StatusForbidden,
				"dashboard.global_usage_forbidden",
				"Global concurrent session metrics are not available to this user.")
			return
		}
	}

	count, err := api.sessions.ObservedSessionCount(ctx)
	if err != nil {
		api.writeDashboardFailure(writer, request, "concurrent-sessions", err)
		return
	}
	adminWriteJSON(writer, http.StatusOK, struct {
		Count int `json:"count"`
	}{Count: count})
}

// dashboardProviderSlotBody 逐字对应 ProviderSlotInfo（provider-slots.ts:17-27）。
type dashboardProviderSlotBody struct {
	ProviderID int64  `json:"providerId"`
	Name       string `json:"name"`
	UsedSlots  int    `json:"usedSlots"`
	TotalSlots int64  `json:"totalSlots"`
	// totalVolume 是「今日 token 总量」：provider-slots 恒 0（Node 注明由调用方回填），
	// realtime 里由排行榜的 totalTokens 回填。类型用 float64：Node 的 totalTokens 是 number，
	// 取整会把小数位抹掉（与 Node 的值不一致）。
	TotalVolume float64 `json:"totalVolume"`
}

// handleProviderSlots 复刻 getProviderSlots（provider-slots.ts:46-107）。
//
// 三处照抄：
//   - **只取启用的供应商**（isEnabled = true），软删的排除；顺序是 priority ASC, id ASC。
//   - totalSlots 取 limitConcurrentSessions，NULL 归一为 0。
//   - totalVolume 恒 0：Node 的注释写明「由调用方从排行榜数据填充」。
func (api *dashboardAPI) handleProviderSlots(writer http.ResponseWriter, request *http.Request) {
	ctx := request.Context()
	principal, _ := PrincipalFrom(ctx)
	settings, err := api.pools.FindSystemSettings(ctx)
	if err != nil {
		api.writeDashboardFailure(writer, request, "provider-slots", err)
		return
	}
	if !principal.IsAdmin && !settings.AllowGlobalUsageView {
		api.problems.WriteProblem(writer, request, http.StatusForbidden,
			"dashboard.permission_denied", "无权限查看全局数据")
		return
	}

	items, err := api.providerSlotItems(ctx)
	if err != nil {
		api.writeDashboardFailure(writer, request, "provider-slots", err)
		return
	}
	adminWriteJSON(writer, http.StatusOK, struct {
		Items []dashboardProviderSlotBody `json:"items"`
	}{Items: items})
}

// providerSlotItems 算出供应商并发插槽（provider-slots 与 realtime 共用）。
func (api *dashboardAPI) providerSlotItems(
	ctx context.Context,
) ([]dashboardProviderSlotBody, error) {
	providers, err := api.pools.AdminListProviders(ctx)
	if err != nil {
		return nil, err
	}
	enabled := make([]store.AdminProvider, 0, len(providers))
	ids := make([]int64, 0, len(providers))
	for _, provider := range providers {
		if !provider.IsEnabled {
			continue
		}
		enabled = append(enabled, provider)
		ids = append(ids, provider.ID)
	}

	counts, err := api.sessions.ProviderInFlightCounts(ctx, ids)
	if err != nil {
		return nil, err
	}

	items := make([]dashboardProviderSlotBody, 0, len(enabled))
	for _, provider := range enabled {
		items = append(items, dashboardProviderSlotBody{
			ProviderID:  provider.ID,
			Name:        provider.Name,
			UsedSlots:   counts[provider.ID],
			TotalSlots:  providerLimitConcurrentSessions(provider),
			TotalVolume: 0,
		})
	}
	return items, nil
}

// providerLimitConcurrentSessions 复刻 `provider.limitConcurrentSessions ?? 0`。
func providerLimitConcurrentSessions(provider store.AdminProvider) int64 {
	if provider.LimitConcurrentSessions == nil {
		return 0
	}
	return int64(*provider.LimitConcurrentSessions)
}

// dashboardClientVersionUserBody 逐字对应 ClientVersionStats.users 的元素。
type dashboardClientVersionUserBody struct {
	UserID       int64  `json:"userId"`
	Username     string `json:"username"`
	Version      string `json:"version"`
	LastSeen     string `json:"lastSeen"`
	IsLatest     bool   `json:"isLatest"`
	NeedsUpgrade bool   `json:"needsUpgrade"`
}

// dashboardClientVersionBody 逐字对应 ClientVersionStats（client-version-checker.ts:61-85）。
type dashboardClientVersionBody struct {
	ClientType string                           `json:"clientType"`
	GAVersion  *string                          `json:"gaVersion"`
	TotalUsers int                              `json:"totalUsers"`
	Users      []dashboardClientVersionUserBody `json:"users"`
}

// clientVersionGAThreshold 与 Node 的 CLIENT_VERSION_GA_THRESHOLD 默认值一致。
//
// Go 侧 clientver 里也有一份同值的私有常量（computeGAVersion 用），但那是**数据面**的
// ShouldUpgrade 路径，不导出；管理面这条读面要按 clientType 分组重算，故在这里声明一份。
// 两处同时改才一致（改一处会出现「升级提示说该升、版本页说不该升」）。
const clientVersionGAThreshold = 2

// handleClientVersions 复刻 fetchClientVersionStats（client-versions.ts:15-30）+
// ClientVersionChecker.getAllClientStats（client-version-checker.ts:304-370）。
//
// 处理顺序与 Node 逐条一致：先按活跃用户表的**返回顺序**（MAX(created_at) DESC）分组，组内
// 按 userId 去重保留最高版本，再按「同版本用户数 >= 阈值」算 GA（取达标者里的最高版本）。
// 分组与用户列表的顺序都要与 Node 相同，否则 items 与 users 的数组顺序会分叉。
func (api *dashboardAPI) handleClientVersions(writer http.ResponseWriter, request *http.Request) {
	rows, err := api.pools.AdminActiveUserVersions(request.Context(), 7)
	if err != nil {
		api.writeDashboardFailure(writer, request, "client-versions", err)
		return
	}

	type parsedUser struct {
		userID   int64
		username string
		version  string
		lastSeen string
	}
	order := make([]string, 0, 4)
	groups := make(map[string][]parsedUser)
	for _, row := range rows {
		client, ok := clientver.ParseUserAgent(row.UserAgent)
		if !ok {
			continue
		}
		if _, seen := groups[client.ClientType]; !seen {
			order = append(order, client.ClientType)
		}
		groups[client.ClientType] = append(groups[client.ClientType], parsedUser{
			userID:   row.UserID,
			username: row.Username,
			version:  client.Version,
			lastSeen: row.LastSeen,
		})
	}

	items := make([]dashboardClientVersionBody, 0, len(order))
	for _, clientType := range order {
		unique := make([]parsedUser, 0, len(groups[clientType]))
		index := make(map[int64]int, len(groups[clientType]))
		for _, user := range groups[clientType] {
			position, seen := index[user.userID]
			if !seen {
				index[user.userID] = len(unique)
				unique = append(unique, user)
				continue
			}
			// 同一用户的新版本覆盖旧版本；并列（同版本）时保留先见者（Node 的 isVersionGreater
			// 在相等时返回 false，故不会覆盖）。
			if clientver.IsVersionGreater(user.version, unique[position].version) {
				unique[position] = user
			}
		}

		gaVersion := computeClientVersionGA(
			unique,
			func(user parsedUser) int64 { return user.userID },
			func(user parsedUser) string { return user.version },
		)
		users := make([]dashboardClientVersionUserBody, 0, len(unique))
		for _, user := range unique {
			isLatest := false
			needsUpgrade := false
			if gaVersion != nil {
				isLatest = user.version == *gaVersion
				needsUpgrade = clientver.IsVersionLess(user.version, *gaVersion)
			}
			users = append(users, dashboardClientVersionUserBody{
				UserID:       user.userID,
				Username:     user.username,
				Version:      user.version,
				LastSeen:     user.lastSeen,
				IsLatest:     isLatest,
				NeedsUpgrade: needsUpgrade,
			})
		}
		items = append(items, dashboardClientVersionBody{
			ClientType: clientType,
			GAVersion:  gaVersion,
			TotalUsers: len(users),
			Users:      users,
		})
	}
	adminWriteJSON(writer, http.StatusOK, struct {
		Items []dashboardClientVersionBody `json:"items"`
	}{Items: items})
}

// computeClientVersionGA 复刻 computeGAVersionFromUsers：同版本去重用户数达标者取最高版本。
//
// 无达标版本时返回 nil（JSON 的 null）。遍历顺序不影响结果：候选是「达标的最高版本」，
// 与 Node 的插入序遍历同解（版本比较是全序）。
func computeClientVersionGA[T any](
	users []T,
	userIDOf func(T) int64,
	versionOf func(T) string,
) *string {
	byVersion := make(map[string]map[int64]struct{}, len(users))
	for _, user := range users {
		version := versionOf(user)
		if byVersion[version] == nil {
			byVersion[version] = make(map[int64]struct{}, 2)
		}
		byVersion[version][userIDOf(user)] = struct{}{}
	}
	var gaVersion *string
	for version, ids := range byVersion {
		if len(ids) < clientVersionGAThreshold {
			continue
		}
		if gaVersion == nil || clientver.IsVersionGreater(version, *gaVersion) {
			candidate := version
			gaVersion = &candidate
		}
	}
	return gaVersion
}

// dashboardRateLimitStatsBody 逐字对应 Node getRateLimitEventStats 的返回（statistics.ts:1198-1206）。
//
// 六个字段的**顺序**与 Node 一致（Node 的早退分支也是这个顺序），A2 对拍可逐字节比对。
type dashboardRateLimitStatsBody struct {
	TotalEvents      int                               `json:"total_events"`
	EventsByType     map[string]int                    `json:"events_by_type"`
	EventsByUser     map[string]int                    `json:"events_by_user"`
	EventsByProvider map[string]int                    `json:"events_by_provider"`
	EventsTimeline   []dashboardRateLimitTimelinePoint `json:"events_timeline"`
	AvgCurrentUsage  float64                           `json:"avg_current_usage"`
}

// dashboardRateLimitTimelinePoint 是 events_timeline 的一个点。
type dashboardRateLimitTimelinePoint struct {
	Hour  string `json:"hour"`
	Count int    `json:"count"`
}

// rateLimitMetadataPattern 与 Node 的正则同形：`/rate_limit_metadata:\s*(\{[^}]+\})/`。
var rateLimitMetadataPattern = regexp.MustCompile(`rate_limit_metadata:\s*(\{[^}]+\})`)

// rateLimitTypes 逐字取自 DashboardRateLimitStatsQuerySchema 的 limitType 枚举。
var rateLimitTypes = map[string]bool{
	"rpm":                 true,
	"usd_5h":              true,
	"usd_weekly":          true,
	"usd_monthly":         true,
	"usd_total":           true,
	"concurrent_sessions": true,
	"daily_quota":         true,
}

// handleRateLimitStats 复刻 getRateLimitStats（rate-limit-stats.ts:20-60）+
// getRateLimitEventStats（statistics.ts:1072-1206）。
func (api *dashboardAPI) handleRateLimitStats(writer http.ResponseWriter, request *http.Request) {
	ctx := request.Context()
	filters, issues := parseDashboardRateLimitFilters(request)
	if len(issues) > 0 {
		api.problems.WriteValidationError(writer, request, issues)
		return
	}

	location, err := meSystemLocation(ctx, api.pools)
	if err != nil {
		api.writeDashboardFailure(writer, request, "rate-limit-stats", err)
		return
	}

	query := store.AdminRateLimitEventFilters{
		UserID:     filters.UserID,
		ProviderID: filters.ProviderID,
		StartTime:  filters.StartTime,
		EndTime:    filters.EndTime,
	}
	if filters.KeyID != nil {
		keyString, found, keyErr := api.pools.AdminKeyStringByID(ctx, *filters.KeyID)
		if keyErr != nil {
			api.writeDashboardFailure(writer, request, "rate-limit-stats", keyErr)
			return
		}
		if !found {
			// 密钥不存在时 Node 直接作答空统计（不是 404）。
			adminWriteJSON(writer, http.StatusOK, emptyRateLimitStats())
			return
		}
		query.KeyString = &keyString
	}

	rows, err := api.pools.AdminRateLimitEventRows(ctx, query, location.String())
	if err != nil {
		api.writeDashboardFailure(writer, request, "rate-limit-stats", err)
		return
	}

	body := emptyRateLimitStats()
	body.TotalEvents = len(rows)
	typeTotals := map[string]int{}
	userTotals := map[string]int{}
	providerTotals := map[string]int{}
	hourTotals := map[string]int{}
	totalCurrentUsage := 0.0
	usageCount := 0

	for _, row := range rows {
		match := rateLimitMetadataPattern.FindStringSubmatch(row.ErrorMessage)
		if match == nil {
			continue
		}
		var metadata struct {
			LimitType *string  `json:"limit_type"`
			Current   *float64 `json:"current"`
		}
		if err := json.Unmarshal([]byte(match[1]), &metadata); err != nil {
			continue
		}
		if filters.LimitType != "" && (metadata.LimitType == nil || *metadata.LimitType != filters.LimitType) {
			continue
		}
		if metadata.LimitType != nil {
			typeTotals[*metadata.LimitType]++
		}
		userTotals[strconv.FormatInt(row.UserID, 10)]++
		providerTotals[rateLimitProviderKey(row.ProviderID)]++
		if row.Hour != nil {
			hourTotals[*row.Hour]++
		}
		if metadata.Current != nil {
			totalCurrentUsage += *metadata.Current
			usageCount++
		}
	}

	body.EventsByType = typeTotals
	body.EventsByUser = userTotals
	body.EventsByProvider = providerTotals
	hours := make([]string, 0, len(hourTotals))
	for hour := range hourTotals {
		hours = append(hours, hour)
	}
	// Node 用 localeCompare 排序：两个 ISO 串的比较与字节序一致。
	sort.Strings(hours)
	body.EventsTimeline = make([]dashboardRateLimitTimelinePoint, 0, len(hours))
	for _, hour := range hours {
		body.EventsTimeline = append(body.EventsTimeline,
			dashboardRateLimitTimelinePoint{Hour: hour, Count: hourTotals[hour]})
	}
	if usageCount > 0 {
		body.AvgCurrentUsage = roundHalfUp2(totalCurrentUsage / float64(usageCount))
	}
	adminWriteJSON(writer, http.StatusOK, body)
}

// rateLimitProviderKey 复刻 JS 里 `obj[row.provider_id]` 的键：null 会变成字符串 "null"。
func rateLimitProviderKey(providerID *int64) string {
	if providerID == nil {
		return "null"
	}
	return strconv.FormatInt(*providerID, 10)
}

// emptyRateLimitStats 造一份空统计（映射不能是 nil，否则序列化成 null 而不是 {}）。
func emptyRateLimitStats() dashboardRateLimitStatsBody {
	return dashboardRateLimitStatsBody{
		EventsByType:     map[string]int{},
		EventsByUser:     map[string]int{},
		EventsByProvider: map[string]int{},
		EventsTimeline:   []dashboardRateLimitTimelinePoint{},
	}
}

// dashboardRateLimitFilters 是 /dashboard/rate-limit-stats 的筛选解析结果。
type dashboardRateLimitFilters struct {
	UserID     *int64
	ProviderID *int64
	KeyID      *int64
	LimitType  string
	StartTime  *time.Time
	EndTime    *time.Time
}

// parseDashboardRateLimitFilters 复刻 DashboardRateLimitStatsQuerySchema。
func parseDashboardRateLimitFilters(
	request *http.Request,
) (dashboardRateLimitFilters, []InvalidParam) {
	values := request.URL.Query()
	filters := dashboardRateLimitFilters{}
	issues := []InvalidParam{}

	for _, field := range []struct {
		key    string
		target **int64
	}{
		{"userId", &filters.UserID},
		{"providerId", &filters.ProviderID},
		{"keyId", &filters.KeyID},
	} {
		value, present, issue := coerceOptionalInt(values, field.key, 1, 0)
		if issue != nil {
			issues = append(issues, InvalidParam{
				Path: issue.Path, Code: issue.Code, Message: issue.Message,
			})
			continue
		}
		if present {
			parsed := int64(value)
			*field.target = &parsed
		}
	}

	if raw, present := queryValue(values, "limitType"); present {
		if !rateLimitTypes[raw] {
			issues = append(issues, InvalidParam{
				Path: []any{"limitType"}, Code: "invalid_enum_value",
				Message: "Invalid enum value",
			})
		} else {
			filters.LimitType = raw
		}
	}

	for _, field := range []struct {
		key    string
		target **time.Time
	}{
		{"startTime", &filters.StartTime},
		{"endTime", &filters.EndTime},
	} {
		raw, present := queryValue(values, field.key)
		if !present {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			issues = append(issues, InvalidParam{
				Path: []any{field.key}, Code: "invalid_string", Message: "Invalid datetime",
			})
			continue
		}
		*field.target = &parsed
	}

	if len(issues) > 0 {
		return dashboardRateLimitFilters{}, issues
	}
	return filters, nil
}

// roundCost6 复刻 `toCostDecimal(x).toDecimalPlaces(6).toNumber()`。
func roundCost6(value float64) float64 {
	rounded, _ := decimal.NewFromFloat(value).Round(6).Float64()
	return rounded
}

// errorRate 复刻 `parseFloat(((errors/total)*100).toFixed(2))`；无请求时为 0。
func errorRate(errorCount int64, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return roundHalfUp2(float64(errorCount) / float64(total) * 100)
}

// roundHalfUp2 把数值四舍五入到两位小数（JS toFixed 的常见分支）。
func roundHalfUp2(value float64) float64 {
	rounded, _ := decimal.NewFromFloat(value).Round(2).Float64()
	return rounded
}

// writeDashboardFailure 作答仓库/运行态失败（Node 侧被 action 兜成 400）。
func (api *dashboardAPI) writeDashboardFailure(
	writer http.ResponseWriter,
	request *http.Request,
	endpoint string,
	err error,
) {
	api.logger.Warn("admin_dashboard_request_failed", map[string]any{
		"endpoint": endpoint,
		"path":     request.URL.Path,
		"error":    strings.TrimSpace(err.Error()),
	})
	api.problems.WriteProblem(writer, request, http.StatusBadRequest, "OPERATION_FAILED", "")
}

// ---------------------------------------------------------------------------
// /dashboard/statistics：图表数据（actions/statistics.ts 的 getUserStatistics）
// ---------------------------------------------------------------------------

// dashboardStatisticsTimeRanges 是 timeRange 的合法取值（DashboardTimeRangeSchema 的枚举）。
var dashboardStatisticsTimeRanges = map[string]store.AdminStatisticsRange{
	"today":     store.AdminStatisticsToday,
	"7days":     store.AdminStatistics7Days,
	"30days":    store.AdminStatistics30Days,
	"thisMonth": store.AdminStatisticsThisMonth,
}

// dashboardStatisticsEntityBody 逐字对应 StatisticsUser（types/statistics.ts:57-61）。
type dashboardStatisticsEntityBody struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	DataKey string `json:"dataKey"`
}

// dashboardStatisticsBody 逐字对应 UserStatisticsData（types/statistics.ts:63-69）。
//
// chartData 的每一项是「一条日期 + 每个实体的两个键」（`<dataKey>_cost` 是 15 位小数字符串、
// `<dataKey>_calls` 是整数），键名由实体 id 动态生成，故用 map 而不用 struct。
type dashboardStatisticsBody struct {
	ChartData  []map[string]any                `json:"chartData"`
	Users      []dashboardStatisticsEntityBody `json:"users"`
	TimeRange  string                          `json:"timeRange"`
	Resolution string                          `json:"resolution"`
	Mode       string                          `json:"mode"`
}

// dashboardStatisticsPoint 是图表的一个数据点（两种实体维度归一后的形态）。
type dashboardStatisticsPoint struct {
	EntityID int64
	Bucket   time.Time
	APICalls int64
	CostText string
}

// handleStatistics 复刻 getDashboardStatistics（dashboard/handlers.ts:23-31 →
// actions/statistics.ts:33-171 getUserStatistics）。
//
// 三处照抄：
//   - 查询参数只有 timeRange（默认 today），非法值作答 zod 形状的 400。
//   - 模式三段：管理员看全站用户（users）；普通用户 + allowGlobalUsageView 为真时是「自己的
//     密钥明细 + 其他用户汇总」（mixed）；否则只看自己的密钥（keys）。
//   - dataKey 前缀：users → `user`，keys 与 mixed → `key`（mixed 的 others 也是 key 前缀）。
func (api *dashboardAPI) handleStatistics(writer http.ResponseWriter, request *http.Request) {
	ctx := request.Context()
	principal, _ := PrincipalFrom(ctx)

	timeRange, ok := parseDashboardStatisticsRange(writer, request, api.problems)
	if !ok {
		return
	}

	settings, err := api.pools.FindSystemSettings(ctx)
	if err != nil {
		api.writeDashboardFailure(writer, request, "statistics", err)
		return
	}
	location, err := meSystemLocation(ctx, api.pools)
	if err != nil {
		api.writeDashboardFailure(writer, request, "statistics", err)
		return
	}

	mode := "keys"
	if principal.IsAdmin {
		mode = "users"
	} else if settings.AllowGlobalUsageView {
		mode = "mixed"
	}

	points, entities, prefix, err := api.statisticsData(ctx, principal, timeRange, mode,
		location.String())
	if err != nil {
		api.writeDashboardFailure(writer, request, "statistics", err)
		return
	}

	adminWriteJSON(writer, http.StatusOK, dashboardStatisticsBody{
		ChartData:  dashboardStatisticsChartData(points, prefix),
		Users:      dashboardStatisticsEntities(entities, mode),
		TimeRange:  string(timeRange),
		Resolution: timeRange.Resolution(),
		Mode:       mode,
	})
}

// statisticsData 取某个模式下的数据点与实体清单。
func (api *dashboardAPI) statisticsData(
	ctx context.Context,
	principal Principal,
	timeRange store.AdminStatisticsRange,
	mode string,
	timezone string,
) ([]dashboardStatisticsPoint, []store.AdminStatisticsEntity, string, error) {
	switch mode {
	case "users":
		rows, err := api.pools.AdminChartUserStatistics(ctx, timeRange, timezone)
		if err != nil {
			return nil, nil, "", err
		}
		entities, err := api.pools.AdminActiveUserEntities(ctx)
		if err != nil {
			return nil, nil, "", err
		}
		points := make([]dashboardStatisticsPoint, 0, len(rows))
		for _, row := range rows {
			points = append(points, dashboardStatisticsPoint{
				EntityID: row.UserID, Bucket: row.Bucket,
				APICalls: row.APICalls, CostText: row.CostText,
			})
		}
		return points, entities, "user", nil
	case "mixed":
		ownRows, others, err := api.pools.AdminChartMixedStatistics(ctx, principal.UserID,
			timeRange, timezone)
		if err != nil {
			return nil, nil, "", err
		}
		entities, err := api.pools.AdminActiveKeyEntities(ctx, principal.UserID)
		if err != nil {
			return nil, nil, "", err
		}
		// Node 把 othersAggregate 拼在自己的密钥**之后**，并追加一个虚拟实体（id = -1）。
		entities = append(entities, store.AdminStatisticsEntity{ID: -1, Name: "__others__"})
		points := make([]dashboardStatisticsPoint, 0, len(ownRows)+len(others))
		for _, row := range ownRows {
			points = append(points, dashboardStatisticsPoint{
				EntityID: row.KeyID, Bucket: row.Bucket,
				APICalls: row.APICalls, CostText: row.CostText,
			})
		}
		for _, row := range others {
			points = append(points, dashboardStatisticsPoint{
				EntityID: row.UserID, Bucket: row.Bucket,
				APICalls: row.APICalls, CostText: row.CostText,
			})
		}
		return points, entities, "key", nil
	default:
		rows, err := api.pools.AdminChartKeyStatistics(ctx, principal.UserID, timeRange, timezone)
		if err != nil {
			return nil, nil, "", err
		}
		entities, err := api.pools.AdminActiveKeyEntities(ctx, principal.UserID)
		if err != nil {
			return nil, nil, "", err
		}
		points := make([]dashboardStatisticsPoint, 0, len(rows))
		for _, row := range rows {
			points = append(points, dashboardStatisticsPoint{
				EntityID: row.KeyID, Bucket: row.Bucket,
				APICalls: row.APICalls, CostText: row.CostText,
			})
		}
		return points, entities, "key", nil
	}
}

// dashboardStatisticsChartData 复刻 actions/statistics.ts 的 dataByDate 合并逻辑。
//
// 每个日期一条：`date` 是桶的 ISO 串（毫秒），消费是 15 位小数字符串（formatCostForStorage），
// 计数是整数（`row.api_calls || 0`）。
func dashboardStatisticsChartData(
	points []dashboardStatisticsPoint,
	prefix string,
) []map[string]any {
	chartData := make([]map[string]any, 0)
	indexByDate := make(map[string]int)
	for _, point := range points {
		date := dashboardStatisticsISO(point.Bucket)
		at, seen := indexByDate[date]
		if !seen {
			chartData = append(chartData, map[string]any{"date": date})
			at = len(chartData) - 1
			indexByDate[date] = at
		}
		dataKey := fmt.Sprintf("%s-%d", prefix, point.EntityID)
		chartData[at][dataKey+"_cost"] = dashboardStatisticsCostText(point.CostText)
		chartData[at][dataKey+"_calls"] = point.APICalls
	}
	return chartData
}

// dashboardStatisticsEntities 复刻 `users` 数组的投影（空名回落 `User{id}` / `Key{id}`）。
func dashboardStatisticsEntities(
	entities []store.AdminStatisticsEntity,
	mode string,
) []dashboardStatisticsEntityBody {
	prefix := "key"
	fallback := "Key"
	if mode == "users" {
		prefix = "user"
		fallback = "User"
	}
	result := make([]dashboardStatisticsEntityBody, 0, len(entities))
	for _, entity := range entities {
		name := entity.Name
		if name == "" {
			name = fmt.Sprintf("%s%d", fallback, entity.ID)
		}
		result = append(result, dashboardStatisticsEntityBody{
			ID:      entity.ID,
			Name:    name,
			DataKey: fmt.Sprintf("%s-%d", prefix, entity.ID),
		})
	}
	return result
}

// dashboardStatisticsCostText 复刻 formatCostForStorage：15 位小数的字符串（不可解析时为 0）。
func dashboardStatisticsCostText(value string) string {
	parsed, err := decimal.NewFromString(strings.TrimSpace(value))
	if err != nil {
		parsed = decimal.Zero
	}
	return parsed.StringFixed(15)
}

// dashboardStatisticsISO 复刻 Date.toISOString()（毫秒三位 + Z）。
func dashboardStatisticsISO(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// parseDashboardStatisticsRange 解析并校验 timeRange（复刻 DashboardStatisticsQuerySchema）。
//
// 未提供时取 today（zod 的 .default）；给了就必须命中四个枚举值，空串同样是非法值。
func parseDashboardStatisticsRange(
	writer http.ResponseWriter,
	request *http.Request,
	problems ProblemWriter,
) (store.AdminStatisticsRange, bool) {
	raw, present := queryValue(request.URL.Query(), "timeRange")
	if !present {
		return store.AdminStatisticsToday, true
	}
	if timeRange, ok := dashboardStatisticsTimeRanges[raw]; ok {
		return timeRange, true
	}
	problems.WriteValidationError(writer, request, []InvalidParam{{
		Path: []any{"timeRange"}, Code: "invalid_enum_value", Message: "Invalid enum value",
	}})
	return "", false
}
