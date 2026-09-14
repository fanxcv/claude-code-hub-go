package adminapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 me 资源的**用量面**（/api/v1/me/usage-logs*），逐条对齐 Node 的
// src/app/api/v1/resources/me/handlers.ts + src/actions/my-usage.ts 的四条读端点：
//   - GET /me/usage-logs           （偏移或游标两种分页，toMeUsageLogsListResponse 的两种 pageInfo）
//   - GET /me/usage-logs/models    （{items} 包装）
//   - GET /me/usage-logs/endpoints （{items} 包装）
//   - GET /me/usage-logs/stats-summary
//
// 主体来源与 me 的其余端点一致：**认证身份**而不是路径参数（无越权面）。密钥维度用的是
// **密钥原文**（keys.key），因为 Node 的 for-key 查询比的就是这一串；Go 侧从
// AdminKeyRecord.Key 取同一串（Principal 只带 id）。
//
// 未实现并回退 Node 的端点：**GET /me/usage-logs/full**。它要的是「完整行 + readonly 脱敏」
// （findReadonlyUsageLogsBatchForKey + scrubUsageLogsBatchForReadonly），而 Go 侧目前只有
// 管理面的完整行渲染，没有 for-key 的只读批量合并（账本行回退 + 脱敏两件都缺）。
// 用假数据作答比不答更糟，故不注册。
//
// 与 Node 的三处白名单差异（均已在报告登记）：
//  1. total 与 distinct 结果不做 TTL 缓存（Node 有 10s / 5min 两层）。
//  2. 管理员合成会话（ADMIN_TOKEN，KeyID = -1）在用量面恒空：Node 拿 ADMIN_TOKEN 字符串去查账本，
//     实务上无行；Go 侧 Principal 不带密钥原文，取不到这串，于是直接作答空集（结果等价）。
//  3. anthropicEffort 取自 DB 里的 specialSettings 原值（与 Go 既有完整行路径同一写法）——
//     派生设置里不可能出现 anthropic_effort，故与 Node 的「先 union 再取」等价。

// meUsageLogsQuery 是 /me/usage-logs 的解析结果（复刻 MeUsageLogsQuerySchema）。
type meUsageLogsQuery struct {
	Cursor                      *store.UsageLogCursor
	Limit                       int
	Page                        *int
	PageSize                    *int
	StartDate                   string
	EndDate                     string
	StartTime                   *int64
	EndTime                     *int64
	SessionID                   string
	Model                       string
	ActualResponseModelMismatch bool
	StatusCode                  *int
	ExcludeStatusCode200        bool
	Endpoint                    string
	MinRetryCount               int
}

// meUsageLogsQueryDefaults 复刻 schema 的默认值（limit 默认 20，pageSize 无默认）。
const (
	meUsageLogsDefaultLimit = 20
	meUsageLogsMaxLimit     = 100
)

// parseMeUsageLogsQuery 解析并校验查询参数；issues 非空即 400（fromZodError 同形）。
func parseMeUsageLogsQuery(request *http.Request) (meUsageLogsQuery, []usageLogsValidationIssue) {
	values := request.URL.Query()
	query := meUsageLogsQuery{Limit: meUsageLogsDefaultLimit}
	issues := []usageLogsValidationIssue{}

	if limit, present, issue := coerceInt(values, "limit", 1, meUsageLogsMaxLimit); issue != nil {
		issues = append(issues, *issue)
	} else if present {
		query.Limit = limit
	}

	if page, present, issue := coerceOptionalInt(values, "page", 1, 0); issue != nil {
		issues = append(issues, *issue)
	} else if present {
		query.Page = &page
	}
	if pageSize, present, issue := coerceOptionalInt(values, "pageSize", 1, meUsageLogsMaxLimit); issue != nil {
		issues = append(issues, *issue)
	} else if present {
		query.PageSize = &pageSize
	}

	if statusCode, present, issue := coerceOptionalInt(values, "statusCode", 0, 0); issue != nil {
		issues = append(issues, *issue)
	} else if present {
		query.StatusCode = &statusCode
	}
	if minRetry, present, issue := coerceOptionalInt(values, "minRetryCount", 0, 0); issue != nil {
		issues = append(issues, *issue)
	} else if present {
		query.MinRetryCount = minRetry
	}

	for _, field := range []struct {
		key    string
		target **int64
	}{
		{"startTime", &query.StartTime},
		{"endTime", &query.EndTime},
	} {
		value, present, issue := coerceOptionalNumber(values, field.key)
		if issue != nil {
			issues = append(issues, *issue)
			continue
		}
		if present {
			millis := int64(value)
			*field.target = &millis
		}
	}

	for _, field := range []struct {
		key    string
		target *bool
	}{
		{"actualResponseModelMismatch", &query.ActualResponseModelMismatch},
		{"excludeStatusCode200", &query.ExcludeStatusCode200},
	} {
		value, present, issue := coerceOptionalBool(values, field.key)
		if issue != nil {
			issues = append(issues, *issue)
			continue
		}
		if present {
			*field.target = *value
		}
	}

	if raw, present := queryValue(values, "startDate"); present {
		query.StartDate = raw
	}
	if raw, present := queryValue(values, "endDate"); present {
		query.EndDate = raw
	}
	if raw, present := queryValue(values, "sessionId"); present {
		query.SessionID = raw
	}
	if raw, present := queryValue(values, "model"); present {
		query.Model = raw
	}
	if raw, present := queryValue(values, "endpoint"); present {
		query.Endpoint = raw
	}

	// 游标：只有 createdAt 与 id **都**给了才算（Node 的 cursorCreatedAt && cursorId）。
	rawCreatedAt, hasCreatedAt := queryValue(values, "cursorCreatedAt")
	rawID, presentID, issue := coerceOptionalInt(values, "cursorId", 1, 0)
	if issue != nil {
		issues = append(issues, *issue)
	} else if hasCreatedAt && presentID {
		query.Cursor = &store.UsageLogCursor{CreatedAt: rawCreatedAt, ID: int64(rawID)}
	}

	if len(issues) > 0 {
		return meUsageLogsQuery{}, issues
	}
	return query, nil
}

// meUsageQueryFilters 把解析结果映射为 store 的 for-key 筛选条件。
func (q meUsageLogsQuery) filters(keyString string) store.MeUsageSlimFilters {
	return store.MeUsageSlimFilters{
		KeyString:                   keyString,
		SessionID:                   q.SessionID,
		StartTime:                   q.StartTime,
		EndTime:                     q.EndTime,
		StatusCode:                  q.StatusCode,
		ExcludeStatusCode200:        q.ExcludeStatusCode200,
		Model:                       q.Model,
		ActualResponseModelMismatch: q.ActualResponseModelMismatch,
		Endpoint:                    q.Endpoint,
		MinRetryCount:               q.MinRetryCount,
	}
}

// offsetPagination 复刻 isOffsetUsageLogsQuery：给了 page 或 pageSize 就走偏移分支。
func (q meUsageLogsQuery) offsetPagination() bool {
	return q.Page != nil || q.PageSize != nil
}

// meDateOnlyPattern 是 Node 的 /^\d{4}-\d{2}-\d{2}$/：**只有**严格 ISO 日期才参与换算，
// 其余（含 "2026-1-1"、带时间的串）按未提供处理而不是报错。
var meDateOnlyPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// meUsageDateRange 复刻 parseDateRangeInServerTimezone（my-usage.ts:116）：
// 起点是**服务端时区**的零点，终点是 endDate 次日零点（开区间上界）。
//
// 时区取错的表现是「跨日边界的用量按别的日子统计」，因此时区一律走 meSystemLocation。
func meUsageDateRange(startDate, endDate string, location *time.Location) (startTime, endTime *int64) {
	if meDateOnlyPattern.MatchString(startDate) {
		if parsed, err := time.ParseInLocation("2006-01-02", startDate, location); err == nil {
			millis := parsed.UnixMilli()
			startTime = &millis
		}
	}
	if meDateOnlyPattern.MatchString(endDate) {
		if parsed, err := time.ParseInLocation("2006-01-02", endDate, location); err == nil {
			exclusive := parsed.AddDate(0, 0, 1)
			millis := exclusive.UnixMilli()
			endTime = &millis
		}
	}
	return startTime, endTime
}

// meUsageSubject 取「密钥原文 + 用户 id」；合成会话（或密钥行缺失）返回 ok=false。
func (api *meAPI) meUsageSubject(ctx context.Context) (meSubject, string, bool, error) {
	subject, err := api.resolveMeSubject(ctx)
	if err != nil {
		return subject, "", false, err
	}
	if subject.isVirtual() {
		return subject, "", false, nil
	}
	return subject, subject.key.Key, true, nil
}

// meUsageEntry 逐字对应 mapMyUsageLogEntries 产出的 MyUsageLogEntry（键序一致）。
type meUsageEntry struct {
	ID                             int64   `json:"id"`
	CreatedAt                      any     `json:"createdAt"`
	Model                          any     `json:"model"`
	BillingModel                   any     `json:"billingModel"`
	AnthropicEffort                any     `json:"anthropicEffort"`
	ModelRedirect                  any     `json:"modelRedirect"`
	InputTokens                    int64   `json:"inputTokens"`
	OutputTokens                   int64   `json:"outputTokens"`
	Cost                           float64 `json:"cost"`
	StatusCode                     any     `json:"statusCode"`
	Duration                       any     `json:"duration"`
	Endpoint                       any     `json:"endpoint"`
	CacheCreationInputTokens       any     `json:"cacheCreationInputTokens"`
	CacheReadInputTokens           any     `json:"cacheReadInputTokens"`
	CacheCreation5mInputTokens     any     `json:"cacheCreation5mInputTokens"`
	CacheCreation1hInputTokens     any     `json:"cacheCreation1hInputTokens"`
	CacheTTLApplied                any     `json:"cacheTtlApplied"`
	TheoreticalCacheTokens         any     `json:"theoreticalCacheTokens"`
	CacheScoreEligible             any     `json:"cacheScoreEligible"`
	CacheScoreExcludedReason       any     `json:"cacheScoreExcludedReason"`
	CacheInputTotal                int64   `json:"cacheInputTotal"`
	ActualCacheRate                any     `json:"actualCacheRate"`
	TheoreticalCacheRate           any     `json:"theoreticalCacheRate"`
	RequestCacheCoefficientBp      any     `json:"requestCacheCoefficientBp"`
	RequestCacheMetricAvailability string  `json:"requestCacheMetricAvailability"`
}

// projectMeUsageEntry 复刻 mapMyUsageLogEntries：三处派生值——
// 计费模型取值随 billingModelSource 切换、重定向标注（"原 → 新"）、缓存指标的派生。
func projectMeUsageEntry(row store.MeUsageSlimRow, billingModelSource string) meUsageEntry {
	modelRedirect := any(nil)
	if row.OriginalModel != nil && row.Model != nil && *row.OriginalModel != *row.Model {
		modelRedirect = *row.OriginalModel + " → " + *row.Model
	}

	billingModel := row.Model
	if billingModelSource == "original" {
		billingModel = row.OriginalModel
	}

	metrics := deriveRequestCacheMetrics(
		row.InputTokens, row.CacheCreationInputTokens, row.CacheReadInputTokens,
		row.TheoreticalCacheTokens, row.CacheScoreEligible, row.CacheScoreExcludedReason,
	)
	settings := decodeSettings(row.SpecialSettings)

	return meUsageEntry{
		ID:                             row.ID,
		CreatedAt:                      meUsageCreatedAt(row.CreatedAt),
		Model:                          stringPointerValue(row.Model),
		BillingModel:                   stringPointerValue(billingModel),
		AnthropicEffort:                extractAnthropicEffort(settings),
		ModelRedirect:                  modelRedirect,
		InputTokens:                    finiteNonNegative(row.InputTokens),
		OutputTokens:                   finiteNonNegative(row.OutputTokens),
		Cost:                           parseCostText(store.MeUsageCostText(row.CostUSD)),
		StatusCode:                     intPointerValue(row.StatusCode),
		Duration:                       intPointerValue(row.DurationMs),
		Endpoint:                       stringPointerValue(row.Endpoint),
		CacheCreationInputTokens:       intPointerValue(row.CacheCreationInputTokens),
		CacheReadInputTokens:           intPointerValue(row.CacheReadInputTokens),
		CacheCreation5mInputTokens:     intPointerValue(row.CacheCreation5mInputTokens),
		CacheCreation1hInputTokens:     intPointerValue(row.CacheCreation1hInputTokens),
		CacheTTLApplied:                stringPointerValue(row.CacheTTLApplied),
		TheoreticalCacheTokens:         intPointerValue(row.TheoreticalCacheTokens),
		CacheScoreEligible:             boolPointerValue(row.CacheScoreEligible),
		CacheScoreExcludedReason:       stringPointerValue(row.CacheScoreExcludedReason),
		CacheInputTotal:                metrics.CacheInputTotal,
		ActualCacheRate:                metrics.ActualCacheRate,
		TheoreticalCacheRate:           metrics.TheoreticalCacheRate,
		RequestCacheCoefficientBp:      metrics.RequestCacheCoefficientBP,
		RequestCacheMetricAvailability: string(metrics.RequestCacheMetricAvailability),
	}
}

// meUsageCreatedAt 把行时间渲染为 Node 的 Date.prototype.toISOString（毫秒精度，UTC）。
//
// 注意与游标里的 createdAtRaw（to_char，微秒精度）**不同**：JSON 里的 Date 只到毫秒，
// 拿微秒串当 createdAt 会让前端显示多三位小数。
func meUsageCreatedAt(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// meUsageCursorToken 复刻 normalizeUsageLogsCursor：{createdAt, id} 的 base64url(JSON)。
func meUsageCursorToken(cursor *store.UsageLogCursor) any {
	if cursor == nil {
		return nil
	}
	payload, err := json.Marshal(struct {
		CreatedAt string `json:"createdAt"`
		ID        int64  `json:"id"`
	}{CreatedAt: cursor.CreatedAt, ID: cursor.ID})
	if err != nil {
		return nil
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

// handleMeUsageLogs 复刻 listMeUsageLogs + toMeUsageLogsListResponse。
func (api *meAPI) handleMeUsageLogs(writer http.ResponseWriter, request *http.Request) {
	query, issues := parseMeUsageLogsQuery(request)
	if len(issues) > 0 {
		writeUsageLogsValidationProblem(writer, request, issues)
		return
	}

	ctx := request.Context()
	_, keyString, ok, err := api.meUsageSubject(ctx)
	if err != nil {
		api.writeMeFailure(writer, request, err)
		return
	}

	filters := query.filters(keyString)
	settings, err := api.pools.FindSystemSettings(ctx)
	if err != nil {
		api.writeMeFailure(writer, request, err)
		return
	}
	location, err := meSystemLocation(ctx, api.pools)
	if err != nil {
		api.writeMeFailure(writer, request, err)
		return
	}
	// startTime/endTime 同时给了就用它们（Node：两个都 undefined 时才按日期串换算）。
	if query.StartTime == nil && query.EndTime == nil {
		start, end := meUsageDateRange(query.StartDate, query.EndDate, location)
		filters.StartTime = start
		filters.EndTime = end
	}

	// 两条分支各自赋值后才进入响应组装，故此处用零值声明（空字面量属死赋值，SA4006）。
	var items []meUsageEntry
	var pageInfo any

	if query.offsetPagination() {
		page := 1
		if query.Page != nil {
			page = *query.Page
		}
		pageSize := query.Limit
		if query.PageSize != nil {
			pageSize = *query.PageSize
		}
		result := store.MeUsageSlimPage{}
		if ok {
			result, err = api.pools.FindMeUsageLogsForKeySlim(ctx, filters, page, pageSize)
			if err != nil {
				api.writeMeFailure(writer, request, err)
				return
			}
		}
		items = api.projectMeUsageItems(meUsageBillingModelSource(settings), result.Logs)
		totalPages := 0
		if pageSize > 0 {
			totalPages = int((result.Total + int64(pageSize) - 1) / int64(pageSize))
		}
		pageInfo = jsonObject{}.
			set("page", page).
			set("pageSize", pageSize).
			set("total", result.Total).
			set("totalPages", totalPages)
		writeOrderedJSONResponse(writer, http.StatusOK, jsonObject{}.
			set("items", items).
			set("pageInfo", pageInfo))
		return
	}

	batch := store.MeUsageSlimBatch{}
	if ok {
		batch, err = api.pools.FindMeUsageLogsForKeyBatch(ctx, filters, query.Cursor, query.Limit)
		if err != nil {
			api.writeMeFailure(writer, request, err)
			return
		}
	}
	items = api.projectMeUsageItems(meUsageBillingModelSource(settings), batch.Logs)
	writeOrderedJSONResponse(writer, http.StatusOK, jsonObject{}.
		set("items", items).
		set("pageInfo", jsonObject{}.
			set("nextCursor", meUsageCursorToken(batch.NextCursor)).
			set("hasMore", batch.HasMore).
			set("limit", query.Limit)))
}

// projectMeUsageItems 逐行投影为响应项。
func (api *meAPI) projectMeUsageItems(billingModelSource string, rows []store.MeUsageSlimRow) []meUsageEntry {
	items := make([]meUsageEntry, 0, len(rows))
	for _, row := range rows {
		items = append(items, projectMeUsageEntry(row, billingModelSource))
	}
	return items
}

// meUsageBillingModelSource 取计费模型取值的口径来源（系统设置 billingModelSource）。
//
// 与 Node 的取值链逐字一致（`dbSettings?.billingModelSource ?? "original"`）：
// 设置行缺失 → `"original"`（而不是空串）——Node 的 transformer 在行缺失时会先补成
// `"original"`，再传给 `mapMyUsageLogEntries`（`src/actions/my-usage.ts:718`），该函数只在
// 等于 `"original"` 时改用 `originalModel`。若这里返回空串，就会被当成非 original
// （优先 model 列），与 Node 相反。
//
// 设置行在、但值为空串时，**原样返回空串**（Node 的 `??` 不覆盖空串），同样优先 model 列。
func meUsageBillingModelSource(settings *store.SystemSettings) string {
	return resolveBillingModelSource(settings)
}

// handleMeUsageModels 复刻 listMeUsageModels（{items} 包装，键为 model）。
func (api *meAPI) handleMeUsageModels(writer http.ResponseWriter, request *http.Request) {
	api.handleMeUsageDistinct(writer, request, "models")
}

// handleMeUsageEndpoints 复刻 listMeUsageEndpoints。
func (api *meAPI) handleMeUsageEndpoints(writer http.ResponseWriter, request *http.Request) {
	api.handleMeUsageDistinct(writer, request, "endpoints")
}

// handleMeUsageDistinct 是 models/endpoints 的共同实现。
func (api *meAPI) handleMeUsageDistinct(writer http.ResponseWriter, request *http.Request, kind string) {
	ctx := request.Context()
	_, keyString, ok, err := api.meUsageSubject(ctx)
	if err != nil {
		api.writeMeFailure(writer, request, err)
		return
	}

	values := []string{}
	if ok {
		if kind == "models" {
			values, err = api.pools.DistinctMeUsageModelsForKey(ctx, keyString)
		} else {
			values, err = api.pools.DistinctMeUsageEndpointsForKey(ctx, keyString)
		}
		if err != nil {
			api.writeMeFailure(writer, request, err)
			return
		}
	}
	writeOrderedJSONResponse(writer, http.StatusOK, jsonObject{}.set("items", nonNilStrings(values)))
}

// handleMeStatsSummary 复刻 getMeStatsSummary（my-usage.ts:994）。
//
// 键维度合计由**只有当前密钥的那些行**汇总（Node 的 summaryAcc 只遍历 keyOnlyBreakdown），
// 用户维度 breakdown 则跨该用户的全部密钥——这一点抄错就是「自服务页面显示的总额比页面上
// 各行之和还大」，故合计与 breakdown 分别取不同来源。
func (api *meAPI) handleMeStatsSummary(writer http.ResponseWriter, request *http.Request) {
	ctx := request.Context()
	values := request.URL.Query()
	startDate, _ := queryValue(values, "startDate")
	endDate, _ := queryValue(values, "endDate")

	subject, keyString, ok, err := api.meUsageSubject(ctx)
	if err != nil {
		api.writeMeFailure(writer, request, err)
		return
	}
	settings, err := api.pools.FindSystemSettings(ctx)
	if err != nil {
		api.writeMeFailure(writer, request, err)
		return
	}
	location, err := meSystemLocation(ctx, api.pools)
	if err != nil {
		api.writeMeFailure(writer, request, err)
		return
	}
	startTime, endTime := meUsageDateRange(startDate, endDate, location)
	currencyDisplay := ""
	if settings != nil {
		currencyDisplay = settings.CurrencyDisplay
	}

	summary := store.MeUsageStatsSummary{}
	if ok && subject.principal.UserID > 0 {
		summary, err = api.pools.SummarizeMeUsageForKeyAndUser(
			ctx, subject.principal.UserID, keyString, startTime, endTime)
		if err != nil {
			api.writeMeFailure(writer, request, err)
			return
		}
	}

	writeOrderedJSONResponse(writer, http.StatusOK, jsonObject{}.
		set("totalRequests", summary.TotalRequests).
		set("totalCost", summary.TotalCost).
		set("totalTokens", store.MeUsageTotalTokens(summary)).
		set("totalInputTokens", summary.TotalInputTokens).
		set("totalOutputTokens", summary.TotalOutputTokens).
		set("totalCacheCreationTokens", summary.TotalCacheCreationTokens).
		set("totalCacheReadTokens", summary.TotalCacheReadTokens).
		set("totalCacheCreation5mTokens", summary.TotalCacheCreation5mTokens).
		set("totalCacheCreation1hTokens", summary.TotalCacheCreation1hTokens).
		set("keyModelBreakdown", meUsageBreakdownJSON(summary.KeyBreakdown)).
		set("userModelBreakdown", meUsageBreakdownJSON(summary.UserBreakdown)).
		set("currencyCode", currencyDisplay))
}

// meUsageBreakdownJSON 渲染模型分解（键序与 ModelBreakdownItem 一致；nil 渲染成 []）。
func meUsageBreakdownJSON(items []store.MeUsageModelBreakdown) []jsonObject {
	result := make([]jsonObject, 0, len(items))
	for _, item := range items {
		result = append(result, jsonObject{}.
			set("model", stringPointerValue(item.Model)).
			set("requests", item.Requests).
			set("cost", store.MeUsageCostFloat(item.Cost)).
			set("inputTokens", item.InputTokens).
			set("outputTokens", item.OutputTokens).
			set("cacheCreationTokens", item.CacheCreationTokens).
			set("cacheReadTokens", item.CacheReadTokens).
			set("cacheCreation5mTokens", item.CacheCreation5mTokens).
			set("cacheCreation1hTokens", item.CacheCreation1hTokens))
	}
	return result
}
