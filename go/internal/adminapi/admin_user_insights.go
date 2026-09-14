package adminapi

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 admin-user-insights 资源模块（**四条**端点）。
//
// 唯一真源：
//   - 路由与形状：src/app/api/v1/resources/admin-user-insights/{router,handlers}.ts
//   - 业务规则：src/actions/admin-user-insights.ts
//   - SQL：src/repository/admin-user-insights.ts（Go 侧在 internal/store/admin_user_insights.go）
//
// 四条端点：
//
//	GET /admin/users/{userId}/insights/overview             用户 + 四个概览指标 + currencyCode
//	GET /admin/users/{userId}/insights/key-trend            按密钥的图表趋势行
//	GET /admin/users/{userId}/insights/model-breakdown      按模型聚合
//	GET /admin/users/{userId}/insights/provider-breakdown    按供应商聚合
//
// key-trend 的数据源不是 admin-user-insights.ts，而是**统计面**：
// `getStatisticsWithCache(timeRange, "keys", userId)`（src/actions/admin-user-insights.ts:89）。
// 它的查询层在 internal/store/admin_statistics.go（AdminChartKeyStatistics）。
// 两侧照抄的语义：
//   - **不查用户是否存在**（Node 的 action 没有 findUserById），故不存在的 userId 也是 200 + 空 items；
//   - `date` 一律是桶的 ISO 串；
//   - `total_cost` 保留数据库原形：零填充行是**数字** 0，真实行是 numeric **字符串**。
//
// 登记进差异白名单的一项：**overview 里的 user 形状**。Node 返回 `findUserById` 经 `toUser` 投影
// 后的对象（src/repository/_shared/transformers.ts:24），而 OpenAPI 只声明了其中一小段
// （AdminUserInsightUserSchema）。Go 侧复用 `store.FindAdminUserByID` 再走 `AdminUserRow.NodeJSON()`
// ——与 users 面同一份列清单 + 同一套投影，因此键集与**值类型**都与 Node 一致。

// adminUserInsightsOverviewResponse 对应 AdminUserInsightsOverviewResponseSchema。
//
// User 用 `map[string]any`（即 AdminUserRow.NodeJSON）而不是裸 struct：Node 返回的是
// findUserById 经 toUser 投影后的对象（src/repository/_shared/transformers.ts:24），
// 里面 description 的空值要变空串、dailyQuota/rpm 的「不大于 0」要回落 null——裸 struct 会把这些
// 原样吐成 null/0，与 Node 的类型级分叉（对拍 D2）。
type adminUserInsightsOverviewResponse struct {
	User         map[string]any                  `json:"user"`
	Overview     store.AdminUserInsightsOverview `json:"overview"`
	CurrencyCode string                          `json:"currencyCode"`
}

// adminUserModelBreakdownResponse 对应 AdminUserModelBreakdownResponseSchema。
type adminUserModelBreakdownResponse struct {
	Breakdown    []store.AdminUserModelBreakdownItem `json:"breakdown"`
	CurrencyCode string                              `json:"currencyCode"`
}

// adminUserProviderBreakdownResponse 对应 AdminUserProviderBreakdownResponseSchema。
type adminUserProviderBreakdownResponse struct {
	Breakdown    []store.AdminUserProviderBreakdownItem `json:"breakdown"`
	CurrencyCode string                                 `json:"currencyCode"`
}

// adminUserInsightsKeyTrendRow 对应 AdminUserInsightsKeyTrendRowSchema
// （schemas/admin-user-insights.ts:65-71）。
//
// TotalCost 用 any：Node 的零填充行是数字 0，真实行是 numeric 字符串，两者都要原形透出。
type adminUserInsightsKeyTrendRow struct {
	KeyID     int64  `json:"key_id"`
	KeyName   string `json:"key_name"`
	Date      string `json:"date"`
	APICalls  int64  `json:"api_calls"`
	TotalCost any    `json:"total_cost"`
}

// adminUserInsightsKeyTrendResponse 对应 key-trend 路由的 `{ items: [...] }`。
type adminUserInsightsKeyTrendResponse struct {
	Items []adminUserInsightsKeyTrendRow `json:"items"`
}

// adminUserInsightsKeyTrendTimeRanges 是 key-trend 的 timeRange 合法取值
// （AdminUserInsightKeyTrendQuerySchema 的枚举，默认 today）。
var adminUserInsightsKeyTrendTimeRanges = map[string]store.AdminStatisticsRange{
	"today":     store.AdminStatisticsToday,
	"7days":     store.AdminStatistics7Days,
	"30days":    store.AdminStatistics30Days,
	"thisMonth": store.AdminStatisticsThisMonth,
}

// adminUserInsightDatePattern 对应 DateOnlySchema 的 `^\d{4}-\d{2}-\d{2}$`。
var adminUserInsightDatePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// RegisterAdminUserInsights 注册本模块的四条路由。
func RegisterAdminUserInsights(router *Router, deps Deps) {
	if deps.Store == nil {
		if deps.Logger != nil {
			deps.Logger.Warn("admin_user_insights_store_unwired", map[string]any{
				"module": "admin-user-insights",
				"action": "routes_not_registered",
			})
		}
		return
	}
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/admin/users/{userId}/insights/overview",
		Access:      AccessAdmin,
		Module:      "admin-user-insights",
		OperationID: "getAdminUserInsightsOverview",
		Handler:     http.HandlerFunc(handleUserInsightsOverview(deps)),
	})
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/admin/users/{userId}/insights/key-trend",
		Access:      AccessAdmin,
		Module:      "admin-user-insights",
		OperationID: "getAdminUserInsightsKeyTrend",
		Handler:     http.HandlerFunc(handleUserInsightsKeyTrend(deps)),
	})
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/admin/users/{userId}/insights/model-breakdown",
		Access:      AccessAdmin,
		Module:      "admin-user-insights",
		OperationID: "getAdminUserInsightsModelBreakdown",
		Handler:     http.HandlerFunc(handleUserInsightsModelBreakdown(deps)),
	})
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/admin/users/{userId}/insights/provider-breakdown",
		Access:      AccessAdmin,
		Module:      "admin-user-insights",
		OperationID: "getAdminUserInsightsProviderBreakdown",
		Handler:     http.HandlerFunc(handleUserInsightsProviderBreakdown(deps)),
	})
}

// handleUserInsightsKeyTrend 复刻 getAdminUserInsightsKeyTrend（handlers.ts:35-49 →
// actions/admin-user-insights.ts:73-97）。
//
// 两处照抄：
//   - **不查用户是否存在**：action 只校 timeRange，然后拿该用户的密钥统计，
//     不存在的 userId 就是空 items 的 200（不是 404）；
//   - total_cost 保留数据库原形（零填充数字 0 / 真实行 numeric 字符串）。
func handleUserInsightsKeyTrend(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		userID, ok := adminUserInsightIDParam(writer, request)
		if !ok {
			return
		}
		timeRange, ok := adminUserInsightsKeyTrendRange(writer, request, deps)
		if !ok {
			return
		}

		timezone := deps.Store.AdminSystemTimezoneOrUTC(request.Context())
		rows, err := deps.Store.AdminChartKeyStatistics(request.Context(), userID, timeRange, timezone)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request,
				adminActionFailure("admin_user_insights", err))
			return
		}

		items := make([]adminUserInsightsKeyTrendRow, 0, len(rows))
		for _, row := range rows {
			item := adminUserInsightsKeyTrendRow{
				KeyID:     row.KeyID,
				KeyName:   row.KeyName,
				Date:      row.Bucket.UTC().Format("2006-01-02T15:04:05.000Z"),
				APICalls:  row.APICalls,
				TotalCost: row.CostText,
			}
			if row.ZeroFilled {
				// Node 的零填充写的是数字 0（zeroFillKeyStats），不是字符串 "0"。
				item.TotalCost = 0
			}
			items = append(items, item)
		}
		adminWriteJSON(writer, http.StatusOK, adminUserInsightsKeyTrendResponse{Items: items})
	}
}

// adminUserInsightsKeyTrendRange 解析并校验 timeRange（复刻 AdminUserInsightKeyTrendQuerySchema）。
func adminUserInsightsKeyTrendRange(
	writer http.ResponseWriter,
	request *http.Request,
	deps Deps,
) (store.AdminStatisticsRange, bool) {
	raw, present := queryValue(request.URL.Query(), "timeRange")
	if !present {
		return store.AdminStatisticsToday, true
	}
	if timeRange, ok := adminUserInsightsKeyTrendTimeRanges[raw]; ok {
		return timeRange, true
	}
	adminWriteValidationFailure(writer, request, []invalidParam{{
		Path: []any{"timeRange"}, Code: "invalid_enum_value", Message: "Invalid enum value",
	}})
	return "", false
}

// handleUserInsightsOverview 复刻 getAdminUserInsightsOverview。
func handleUserInsightsOverview(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		userID, ok := adminUserInsightIDParam(writer, request)
		if !ok {
			return
		}
		query, ok := adminUserInsightDateQuery(writer, request, []string{"startDate", "endDate"})
		if !ok {
			return
		}

		user, err := deps.Store.FindAdminUserByID(request.Context(), userID)
		if err == store.ErrNotFound {
			adminWriteUserInsightsNotFound(writer, request, deps)
			return
		}
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request,
				adminActionFailure("admin_user_insights", err))
			return
		}

		timezone := deps.Store.AdminSystemTimezoneOrUTC(request.Context())
		overview, err := deps.Store.AdminUserOverviewMetrics(request.Context(), userID, timezone,
			query["startDate"], query["endDate"])
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request,
				adminActionFailure("admin_user_insights", err))
			return
		}
		adminWriteJSON(writer, http.StatusOK, adminUserInsightsOverviewResponse{
			User:         user.NodeJSON(),
			Overview:     overview,
			CurrencyCode: adminUserInsightsCurrency(deps, request),
		})
	}
}

// handleUserInsightsModelBreakdown 复刻 getAdminUserInsightsModelBreakdown。
func handleUserInsightsModelBreakdown(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		userID, ok := adminUserInsightIDParam(writer, request)
		if !ok {
			return
		}
		query, ok := adminUserInsightDateQuery(writer, request,
			[]string{"startDate", "endDate", "keyId", "providerId"})
		if !ok {
			return
		}

		filters := store.AdminUserInsightsFilters{
			StartDate: query["startDate"],
			EndDate:   query["endDate"],
		}
		if raw, present := query["keyId"]; present && raw != "" {
			parsed, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || parsed <= 0 {
				adminWriteValidationFailure(writer, request, []invalidParam{{
					Path: []any{"keyId"}, Code: "invalid_type", Message: "Expected number, received string",
				}})
				return
			}
			filters.KeyID = parsed
		}
		if raw, present := query["providerId"]; present && raw != "" {
			parsed, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || parsed <= 0 {
				adminWriteValidationFailure(writer, request, []invalidParam{{
					Path: []any{"providerId"}, Code: "invalid_type", Message: "Expected number, received string",
				}})
				return
			}
			filters.ProviderID = parsed
		}

		timezone := deps.Store.AdminSystemTimezoneOrUTC(request.Context())
		items, err := deps.Store.AdminUserModelBreakdown(request.Context(), userID,
			adminUserInsightsBillingModelSource(deps, request), filters, timezone)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request,
				adminActionFailure("admin_user_insights", err))
			return
		}
		adminWriteJSON(writer, http.StatusOK, adminUserModelBreakdownResponse{
			Breakdown:    items,
			CurrencyCode: adminUserInsightsCurrency(deps, request),
		})
	}
}

// handleUserInsightsProviderBreakdown 复刻 getAdminUserInsightsProviderBreakdown。
func handleUserInsightsProviderBreakdown(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		userID, ok := adminUserInsightIDParam(writer, request)
		if !ok {
			return
		}
		query, ok := adminUserInsightDateQuery(writer, request,
			[]string{"startDate", "endDate", "keyId", "model"})
		if !ok {
			return
		}

		filters := store.AdminUserInsightsFilters{
			StartDate: query["startDate"],
			EndDate:   query["endDate"],
			Model:     strings.TrimSpace(query["model"]),
		}
		if raw, present := query["keyId"]; present && raw != "" {
			parsed, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || parsed <= 0 {
				adminWriteValidationFailure(writer, request, []invalidParam{{
					Path: []any{"keyId"}, Code: "invalid_type", Message: "Expected number, received string",
				}})
				return
			}
			filters.KeyID = parsed
		}
		if raw, present := query["model"]; present && strings.TrimSpace(raw) == "" {
			adminWriteValidationFailure(writer, request, []invalidParam{{
				Path: []any{"model"}, Code: "too_small",
				Message: "String must contain at least 1 character(s)",
			}})
			return
		}

		timezone := deps.Store.AdminSystemTimezoneOrUTC(request.Context())
		items, err := deps.Store.AdminUserProviderBreakdown(request.Context(), userID, filters, timezone)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request,
				adminActionFailure("admin_user_insights", err))
			return
		}
		adminWriteJSON(writer, http.StatusOK, adminUserProviderBreakdownResponse{
			Breakdown:    items,
			CurrencyCode: adminUserInsightsCurrency(deps, request),
		})
	}
}

// adminUserInsightIDParam 解析并校验 {userId}（复刻 AdminUserInsightIdParamSchema 的 coerce + 正数）。
func adminUserInsightIDParam(writer http.ResponseWriter, request *http.Request) (int64, bool) {
	parsed, ok := adminCoerceInt(ParamsFrom(request.Context())["userId"])
	if !ok || parsed <= 0 {
		adminWriteValidationFailure(writer, request, []invalidParam{{
			Path:    []any{"userId"},
			Code:    "invalid_type",
			Message: "Expected number, received string",
		}})
		return 0, false
	}
	return int64(parsed), true
}

// adminUserInsightDateQuery 读并校验日期区间（复刻 validateDateRange 的三条规则）。
//
// 返回 map 只含调用方要求读取的键；未提供的键不出现在结果里。
func adminUserInsightDateQuery(
	writer http.ResponseWriter,
	request *http.Request,
	keys []string,
) (map[string]string, bool) {
	values := make(map[string]string, len(keys))
	query := request.URL.Query()
	for _, key := range keys {
		if query.Has(key) {
			values[key] = query.Get(key)
		}
	}
	start := values["startDate"]
	end := values["endDate"]
	if start != "" && !adminUserInsightDatePattern.MatchString(start) {
		adminWriteValidationFailure(writer, request, []invalidParam{{
			Path: []any{"startDate"}, Code: "invalid_string", Message: "Invalid",
		}})
		return nil, false
	}
	if end != "" && !adminUserInsightDatePattern.MatchString(end) {
		adminWriteValidationFailure(writer, request, []invalidParam{{
			Path: []any{"endDate"}, Code: "invalid_string", Message: "Invalid",
		}})
		return nil, false
	}
	// Node 用 `new Date(startDate) > new Date(endDate)`：字典序比较对 YYYY-MM-DD 与日期序等价。
	if start != "" && end != "" && start > end {
		adminWriteValidationFailure(writer, request, []invalidParam{{
			Path: []any{"endDate"}, Code: "custom",
			Message: "startDate must not be after endDate",
		}})
		return nil, false
	}
	return values, true
}

// adminUserInsightsNotFound 复刻 handlers 的 404 分支（action 回 "User not found"）。
func adminWriteUserInsightsNotFound(writer http.ResponseWriter, request *http.Request, deps Deps) {
	adminProblemWriter(deps).WriteProblem(writer, request, http.StatusNotFound,
		"admin_user_insights.not_found", problemTitle(http.StatusNotFound))
}

// adminUserInsightsCurrency 读 currencyCode（Node 取 system settings 的 currencyDisplay）。
func adminUserInsightsCurrency(deps Deps, request *http.Request) string {
	settings, err := deps.Store.FindSystemSettings(request.Context())
	if err != nil || settings == nil {
		return "USD"
	}
	if settings.CurrencyDisplay == "" {
		return "USD"
	}
	return settings.CurrencyDisplay
}

// adminUserInsightsBillingModelSource 读 billingModelSource（口径与 Node 的取值链一致）。
//
// 读库失败或设置行缺失 → `"original"`（Node 的 `dbSettings?.billingModelSource ?? "original"`，
// 见 `src/repository/_shared/transformers.ts:275`）；设置行在则原样透传。
// 本处曾返回域外值 `"model"`（Node 只有 `original`/`redirected`），使该页在设置行缺失时
// 按「重定向后模型」聚合，与 Node 相反。详见 resolveBillingModelSource。
func adminUserInsightsBillingModelSource(deps Deps, request *http.Request) string {
	settings, err := deps.Store.FindSystemSettings(request.Context())
	if err != nil {
		return billingModelSourceDefault
	}
	return resolveBillingModelSource(settings)
}
