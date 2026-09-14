package adminapi

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件实现管理面 usage-logs 的 10 条路由中**不需要 Redis 的 7 条读路由**：
//
//	GET /usage-logs                        read   列表（按参数分流：带 page/pageSize 走偏移，否则走游标）
//	GET /usage-logs/stats                  read   聚合统计
//	GET /usage-logs/filter-options         admin  三张筛选器选项（5 分钟进程内缓存）
//	GET /usage-logs/models                 admin  {items}
//	GET /usage-logs/status-codes           admin  {items}
//	GET /usage-logs/endpoints              admin  {items}
//	GET /usage-logs/session-id-suggestions read   {items}
//
// 三条导出路由（POST /usage-logs/exports、GET .../exports/{jobId}、GET .../{jobId}/download）
// 需要 Redis 的导出任务 KV（cch:usage-logs:export:status: 与 :result:）。**Deps 冻结面里没有
// Redis 句柄**（命令连接由 cmd/cchd 持有并只交给守卫与失效广播），因此本模块把它们做成
// 「有 KV 才注册」：没有 KV 时这三条路由**不注册**，请求照常回退 Node（那里的实现是完整的），
// 而不是由 Go 用一个半成品冒充。装配侧只要传 UsageLogsOptions.KV 即可接管（见 usage_logs_exports.go）。
//
// 权限档位取自 Node 路由表：列表/统计/联想/导出是 read，四个筛选器类端点是 admin
// （src/app/api/v1/resources/usage-logs/router.ts:61-210）。

// UsageLogsOptions 是 usage-logs 模块的装配参数。
type UsageLogsOptions struct {
	// Now 可注入时钟（测试用）；nil 用 time.Now。
	Now func() time.Time
}

// usageLogsModule 保存跨请求状态（两个缓存），并入 Router 的处理器闭包。
type usageLogsModule struct {
	deps       Deps
	ledgerOnly ledgerOnlyCache
	now        func() time.Time
	// exports 是导出侧的运行态（键值面 + 有界作业池）；nil 表示未装配（见 RegisterUsageLogsWith）。
	exports *usageLogsExportRuntime

	optionsMu     sync.Mutex
	optionsModels []string
	optionsCodes  []int
	optionsEnds   []string
	optionsExpiry time.Time

	// stats 是顶部统计的短 TTL 进程内缓存；见 handleStats 与 statsCacheTTL。
	statsMu      sync.Mutex
	statsEntries map[string]usageLogStatsEntry
}

// statsCacheTTL 是统计结果的陈旧上限。取值理由：
//
//   - 前端每 3s 轮询列表，而统计按 E2 降到 30s 一次——缓存 TTL 只需盖住「同一窗口内的重复读取」
//     （多个页签/多个管理员同时打开、以及手动刷新连点）；
//   - 20s ≤ 30s 的展示刷新周期，所以**用户看到的数字永远不会比它本该有的更旧一轮以上**，
//     而写入侧的用量事实在行落库后 ≤292ms 就可读，这里不引入额外业务差异。
const statsCacheTTL = 20 * time.Second

// statsCacheMaxEntries 给缓存一个上界：筛选组合是用户可控的，不能无限增长。
// 超界时丢弃**最早过期**的一条（近似 LRU；条目数很小，线性扫描足够）。
const statsCacheMaxEntries = 64

type usageLogStatsEntry struct {
	summary store.UsageLogSummary
	expires time.Time
}

// 复刻 usage-logs.ts:37 的 FILTER_OPTIONS_CACHE_TTL_MS。
const filterOptionsCacheTTL = 5 * time.Minute

// RegisterUsageLogs 注册 usage-logs 的读路由（导出路由需要 KV）。
func RegisterUsageLogs(router *Router, deps Deps) {
	RegisterUsageLogsWith(router, deps, UsageLogsOptions{})
}

// RegisterUsageLogsWith 注册 usage-logs 路由。
//
// 未装配连接池时不注册任何路由并记 Error 日志：与 Router.Add 的 fail-closed 一致——
// 「Go 答不了」不是错误，静默回退 Node 才是正确行为，但必须留下痕迹（否则会表现为
// 「这 10 条端点没切过去却查不出原因」）。
func RegisterUsageLogsWith(router *Router, deps Deps, options UsageLogsOptions) {
	logger := deps.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	if deps.Store == nil {
		logger.Error("admin_usage_logs_unwired", map[string]any{
			"module": "usage-logs",
			"reason": "store_pools_missing",
			"action": "routes_not_registered",
		})
		return
	}

	now := options.Now
	if now == nil {
		now = time.Now
	}
	module := &usageLogsModule{deps: deps, now: now}
	module.ledgerOnly = ledgerOnlyCache{pools: deps.Store, now: now}

	router.Add(Route{
		Method: http.MethodGet, Path: "/usage-logs", Access: AccessRead, Module: "usage-logs",
		OperationID: "getUsageLogs", Handler: http.HandlerFunc(module.handleList),
	})
	router.Add(Route{
		Method: http.MethodGet, Path: "/usage-logs/stats", Access: AccessRead, Module: "usage-logs",
		OperationID: "getUsageLogsStats", Handler: http.HandlerFunc(module.handleStats),
	})
	router.Add(Route{
		Method: http.MethodGet, Path: "/usage-logs/filter-options", Access: AccessAdmin,
		Module: "usage-logs", OperationID: "getUsageLogsFilterOptions",
		Handler: http.HandlerFunc(module.handleFilterOptions),
	})
	router.Add(Route{
		Method: http.MethodGet, Path: "/usage-logs/models", Access: AccessAdmin,
		Module: "usage-logs", OperationID: "getUsageLogModels",
		Handler: http.HandlerFunc(module.handleModelList),
	})
	router.Add(Route{
		Method: http.MethodGet, Path: "/usage-logs/status-codes", Access: AccessAdmin,
		Module: "usage-logs", OperationID: "getUsageLogStatusCodes",
		Handler: http.HandlerFunc(module.handleStatusCodeList),
	})
	router.Add(Route{
		Method: http.MethodGet, Path: "/usage-logs/endpoints", Access: AccessAdmin,
		Module: "usage-logs", OperationID: "getUsageLogEndpoints",
		Handler: http.HandlerFunc(module.handleEndpointList),
	})
	router.Add(Route{
		Method: http.MethodGet, Path: "/usage-logs/session-id-suggestions", Access: AccessRead,
		Module: "usage-logs", OperationID: "suggestUsageLogSessionIds",
		Handler: http.HandlerFunc(module.handleSessionIDSuggestions),
	})

	// 三条导出路由：只在 Deps.UsageLogsExports（Redis 作业键值面）已装配时注册；缺键值面时
	// registerUsageLogsExports 会记 admin_usage_logs_exports_unwired 并保持回退 Node。
	registerUsageLogsExports(router, module, deps.UsageLogsExports)
}

// handleList 复刻 listUsageLogs：按 page/pageSize 是否出现分流到偏移或游标分页分支；
// 带 sinceId+asc 时走增量分支（契约见 usage-logs-pull-optimization.md）。
func (m *usageLogsModule) handleList(writer http.ResponseWriter, request *http.Request) {
	query, issues := parseUsageLogsQuery(request)
	if len(issues) > 0 {
		writeUsageLogsValidationProblem(writer, request, issues)
		return
	}
	if query.Page != nil || query.PageSize != nil {
		m.handleOffsetList(writer, request, query)
		return
	}
	m.handleCursorList(writer, request, query)
}

// handleCursorList 复刻 getUsageLogsBatch + toUsageLogsListResponse 的游标分支，
// 并承载增量读（query.SinceID 非 nil）：两者共用同一套行渲染与账本回退链，只是扫描方向不同。
func (m *usageLogsModule) handleCursorList(
	writer http.ResponseWriter,
	request *http.Request,
	query usageLogsQuery,
) {
	filters := query.filters()
	incremental := query.incremental()
	logs, hasMore, nextCursor, err := m.deps.Store.FindUsageLogsBatch(
		request.Context(), filters, query.Cursor, query.Limit, incremental)
	if err != nil {
		m.writeActionError(writer, request, err)
		return
	}

	var items []jsonObject
	sourceIDsAtIdentity := any(map[string][]string{})
	switch {
	case len(logs) > 0:
		items = m.usageLogRowsToJSON(request, logs, query, true)
		sourceIDsAtIdentity = m.messageSourceSessionIdsByIdentity(request, logs, query)
	case !m.ledgerOnly.ledgerOnly(request):
		// message_request 有数据（非 ledger-only）却查不到行：空页，与 Node 一致。
		items = []jsonObject{}
	case query.MinRetry > 0:
		// ledger-only 下带重试筛选无意义：Node 直接返回空页、无游标、hasMore=false。
		items = []jsonObject{}
		hasMore = false
		nextCursor = nil
	default:
		// message_request 为空：整体切到 usage_ledger。
		// limit 只在调用方**显式传值**时参与 hasMore 判定：Node 的 ledger 分支用未夹取的
		// 原始 limit（未传时是 undefined，`n > undefined` 恒假），故未传时传 nil。
		var rawLimit *int
		if query.hasLimit {
			limit := query.Limit
			rawLimit = &limit
		}
		ledgerLogs, ledgerHasMore, ledgerCursor, ledgerErr := m.deps.Store.FindUsageLogsBatchLedger(
			request.Context(), filters, query.Cursor, rawLimit, incremental)
		if ledgerErr != nil {
			m.writeActionError(writer, request, ledgerErr)
			return
		}
		items = ledgerRowsToJSON(ledgerLogs)
		hasMore = ledgerHasMore
		nextCursor = ledgerCursor
		if len(ledgerLogs) > 0 {
			sourceIDsAtIdentity = m.ledgerSourceSessionIdsByIdentity(request, ledgerLogs, query)
		}
	}
	// 增量读不返回游标（它的继续位是调用方已看到的 max id）。
	if incremental != nil {
		nextCursor = nil
	}
	m.writeCursorListResponse(writer, items, sourceIDsAtIdentity, nextCursor, hasMore, query)
}

// writeCursorListResponse 复刻 toUsageLogsListResponse 的游标分支：
// items + sourceSessionIdsByIdentity + pageInfo{nextCursor, hasMore, limit}。
func (m *usageLogsModule) writeCursorListResponse(
	writer http.ResponseWriter,
	items []jsonObject,
	sourceIDsAtIdentity any,
	nextCursor *store.UsageLogCursor,
	hasMore bool,
	query usageLogsQuery,
) {
	body := jsonObject{}.
		set("items", itemsOrEmpty(items)).
		set("sourceSessionIdsByIdentity", sourceIDsAtIdentity).
		set("pageInfo", jsonObject{}.
			set("nextCursor", normalizeUsageLogsCursor(nextCursor)).
			set("hasMore", hasMore).
			set("limit", query.Limit))
	writeOrderedJSONResponse(writer, http.StatusOK, body)
}

// handleOffsetList 复刻 getUsageLogs + toUsageLogsListResponse 的偏移分支。
func (m *usageLogsModule) handleOffsetList(
	writer http.ResponseWriter,
	request *http.Request,
	query usageLogsQuery,
) {
	filters := query.filters()
	// 偏移分支不带游标（Node 的 withoutCursor）。
	page := 1
	if query.Page != nil {
		page = *query.Page
	}
	pageSize := 0
	if query.PageSize != nil {
		pageSize = *query.PageSize
	}

	result, err := m.deps.Store.FindUsageLogsWithDetails(request.Context(), filters, page, pageSize)
	if err != nil {
		m.writeActionError(writer, request, err)
		return
	}

	items := m.usageLogRowsToJSON(request, result.Logs, query, false)
	sourceIDsAtIdentity := m.messageSourceSessionIdsByIdentity(request, result.Logs, query)

	// pageInfo：page = body.page ?? query.page ?? 1（body 里没有 page，故取 query）；
	// pageSize = body.pageSize ?? query.pageSize ?? query.limit（**limit 的默认 20 会露出来**，
	// 这是 Node 的既有行为，如实复刻）；total = body.total；totalPages = ceil(total / pageSize)。
	reportedPageSize := query.Limit
	if query.PageSize != nil {
		reportedPageSize = *query.PageSize
	}
	totalPages := 0
	if reportedPageSize > 0 {
		totalPages = int((result.Total + int64(reportedPageSize) - 1) / int64(reportedPageSize))
	}

	body := jsonObject{}.
		set("items", itemsOrEmpty(items)).
		set("sourceSessionIdsByIdentity", sourceIDsAtIdentity).
		set("pageInfo", jsonObject{}.
			set("page", page).
			set("pageSize", reportedPageSize).
			set("total", result.Total).
			set("totalPages", totalPages))
	writeOrderedJSONResponse(writer, http.StatusOK, body)
}

// itemsOrEmpty 保证 items 是数组而不是 null（Node 的 `body.logs ?? []`）。
func itemsOrEmpty(items []jsonObject) []jsonObject {
	if items == nil {
		return []jsonObject{}
	}
	return items
}

// ledgerRowsToJSON 把账本回退行渲染为响应项。
func ledgerRowsToJSON(rows []store.LedgerUsageLogRow) []jsonObject {
	items := make([]jsonObject, 0, len(rows))
	for _, row := range rows {
		items = append(items, ledgerFallbackRowFields(row))
	}
	return items
}

// usageLogRowsToJSON 渲染 message_request 行，并按 Node 的 includeSourceSessionIds 追加
// 每行的 sourceSessionIds（hydrateUsageLogSourceSessionIds）。
func (m *usageLogsModule) usageLogRowsToJSON(
	request *http.Request,
	rows []store.UsageLogRow,
	query usageLogsQuery,
	cursorPath bool,
) []jsonObject {
	sourceIDs := m.messageSourceSessionIds(request, rows, query)
	items := make([]jsonObject, 0, len(rows))
	for _, row := range rows {
		fields := usageLogRowFields(row, cursorPath)
		if row.SessionID != nil {
			if ids, ok := sourceIDs[*row.SessionID]; ok && len(ids) > 0 {
				fields = fields.set("sourceSessionIds", ids)
			}
		}
		items = append(items, fields)
	}
	return items
}

// messageSourceSessionIds 复刻 hydrateUsageLogSourceSessionIds 的 message 源：
// 以公开 identity 为键，聚出该 identity 下的全部物理 session_id（去重）。
func (m *usageLogsModule) messageSourceSessionIds(
	request *http.Request,
	rows []store.UsageLogRow,
	query usageLogsQuery,
) map[string][]string {
	sessionIDs := uniqueSessionIDs(rows)
	if len(sessionIDs) == 0 {
		return map[string][]string{}
	}
	sources, err := m.deps.Store.FindMessageSourceSessionIDs(request.Context(), sessionIDs,
		store.SourceSessionScope{UserID: query.UserID, KeyID: query.KeyID})
	if err != nil {
		// Node 侧这里会抛错并被 action 捕获成失败响应；Go 的选择是「少一个附加字段、照常返回」，
		// 因为该字段只影响展示，而让整个列表失败更糟。差异登记在白名单。
		return map[string][]string{}
	}
	return sources
}

// ledgerSourceSessionIdsByIdentity 复刻 loadUsageLogSourceSessionIdsByIdentity 的 ledger 源。
func (m *usageLogsModule) ledgerSourceSessionIdsByIdentity(
	request *http.Request,
	rows []store.LedgerUsageLogRow,
	query usageLogsQuery,
) map[string][]string {
	sessionIDs := make([]string, 0, len(rows))
	seen := map[string]struct{}{}
	for _, row := range rows {
		if row.SessionID == nil || *row.SessionID == "" {
			continue
		}
		if _, ok := seen[*row.SessionID]; ok {
			continue
		}
		seen[*row.SessionID] = struct{}{}
		sessionIDs = append(sessionIDs, *row.SessionID)
	}
	if len(sessionIDs) == 0 {
		return map[string][]string{}
	}
	sources, err := m.deps.Store.FindLedgerSourceSessionIDs(request.Context(), sessionIDs,
		store.SourceSessionScope{UserID: query.UserID, KeyID: query.KeyID})
	if err != nil {
		return map[string][]string{}
	}
	return sources
}

// messageSourceSessionIdsByIdentity 复刻 loadUsageLogSourceSessionIdsByIdentity 的 message 源。
func (m *usageLogsModule) messageSourceSessionIdsByIdentity(
	request *http.Request,
	rows []store.UsageLogRow,
	query usageLogsQuery,
) map[string][]string {
	return m.messageSourceSessionIds(request, rows, query)
}

// uniqueSessionIDs 收集行里出现过的公开 identity（保持出现序）。
func uniqueSessionIDs(rows []store.UsageLogRow) []string {
	results := make([]string, 0, len(rows))
	seen := map[string]struct{}{}
	for _, row := range rows {
		if row.SessionID == nil || *row.SessionID == "" {
			continue
		}
		if _, ok := seen[*row.SessionID]; ok {
			continue
		}
		seen[*row.SessionID] = struct{}{}
		results = append(results, *row.SessionID)
	}
	return results
}

// normalizeUsageLogsCursor 复刻 normalizeUsageLogsCursor：
// 游标对象编码为 base64url(JSON.stringify({createdAt, id}))，无游标时 null。
func normalizeUsageLogsCursor(cursor *store.UsageLogCursor) any {
	if cursor == nil {
		return nil
	}
	payload := jsonObject{}.
		set("createdAt", cursor.CreatedAt).
		set("id", cursor.ID)
	encoded, err := payload.marshalJSON()
	if err != nil {
		return nil
	}
	return base64.RawURLEncoding.EncodeToString(encoded)
}

// handleStats 复刻 getUsageLogsStats：响应体就是 summary 对象本身。
//
// 实测该查询在生产是 735–925ms（本地 cch_loadtest 21.7ms，见报告），而前端会周期性重复读同一个
// 筛选组合，故加一层**进程内短 TTL 缓存**（statsCacheTTL）。有意不做的两件事：
//
//   - **不做跨实例一致**：这是只读聚合，每个实例各自缓存，最坏差异就是 TTL；走 Redis 反而要多一次
//     网络往返，把缓存的意义抵消掉；
//   - **不做 single-flight**：并发未击中同一条目时会各查一次。管理面轮询间隔 ≥3s，重复查询的
//     窗口极窄；为它引入新依赖不划算（有上限：并发管理员数 × 1 次/窗口）。
func (m *usageLogsModule) handleStats(writer http.ResponseWriter, request *http.Request) {
	query, issues := parseUsageLogsQuery(request)
	if len(issues) > 0 {
		writeUsageLogsValidationProblem(writer, request, issues)
		return
	}
	filters := query.filters()
	ledgerOnly := m.ledgerOnly.ledgerOnly(request)
	if summary, ok := m.loadCachedStats(filters, ledgerOnly); ok {
		writeOrderedJSONResponse(writer, http.StatusOK, usageLogSummaryFields(summary))
		return
	}
	summary, err := m.deps.Store.FindUsageLogsStats(request.Context(), filters, ledgerOnly)
	if err != nil {
		m.writeActionError(writer, request, err)
		return
	}
	// 只缓存成功结果：失败不该被放大成「一段时间内持续失败」。
	m.storeCachedStats(filters, ledgerOnly, summary)
	writeOrderedJSONResponse(writer, http.StatusOK, usageLogSummaryFields(summary))
}

// loadCachedStats 读缓存；未命中或已过期返回 ok=false。
func (m *usageLogsModule) loadCachedStats(
	filters store.UsageLogFilters,
	ledgerOnly bool,
) (store.UsageLogSummary, bool) {
	key := statsFingerprint(filters, ledgerOnly)
	now := m.now()
	m.statsMu.Lock()
	defer m.statsMu.Unlock()
	entry, ok := m.statsEntries[key]
	if !ok || !now.Before(entry.expires) {
		return store.UsageLogSummary{}, false
	}
	return entry.summary, true
}

// storeCachedStats 写缓存，并在超上界时淘汰最早过期的一条。
func (m *usageLogsModule) storeCachedStats(
	filters store.UsageLogFilters,
	ledgerOnly bool,
	summary store.UsageLogSummary,
) {
	key := statsFingerprint(filters, ledgerOnly)
	expires := m.now().Add(statsCacheTTL)
	m.statsMu.Lock()
	defer m.statsMu.Unlock()
	if m.statsEntries == nil {
		m.statsEntries = make(map[string]usageLogStatsEntry, 8)
	}
	if len(m.statsEntries) >= statsCacheMaxEntries {
		oldestKey := ""
		var oldest time.Time
		for candidate, entry := range m.statsEntries {
			if oldestKey == "" || entry.expires.Before(oldest) {
				oldestKey, oldest = candidate, entry.expires
			}
		}
		delete(m.statsEntries, oldestKey)
	}
	m.statsEntries[key] = usageLogStatsEntry{summary: summary, expires: expires}
}

// resetStatsCacheForTest 清空统计缓存（仅测试用）。
//
// 2026-09-13 复原：它曾在死码清理批次里被判为「零引用」删除，而与此同时另一条 lane 的
// 测试（usage_logs_cache_test.go）正在调用它——两条 lane 并行时 grep 看不到对方的引用，
// 属并行竞态而非真死码。**现有真实调用者，勿再删除**；若要删，请先跑全模块门禁。
func (m *usageLogsModule) resetStatsCacheForTest() {
	m.statsMu.Lock()
	defer m.statsMu.Unlock()
	m.statsEntries = nil
}

// statsFingerprint 把筛选条件序列化成缓存键。
//
// 字符串字段用 %q 包起来，免得「字段值里含分隔符」把两个不同筛选组合压成同一个键。
func statsFingerprint(filters store.UsageLogFilters, ledgerOnly bool) string {
	var b strings.Builder
	for _, value := range []*int64{
		filters.UserID, filters.KeyID, filters.ProviderID, filters.StartTime, filters.EndTime,
	} {
		if value == nil {
			b.WriteByte('-')
		} else {
			b.WriteString(strconv.FormatInt(*value, 10))
		}
		b.WriteByte('|')
	}
	if filters.StatusCode == nil {
		b.WriteByte('-')
	} else {
		b.WriteString(strconv.Itoa(*filters.StatusCode))
	}
	b.WriteString("|")
	b.WriteString(strconv.Quote(filters.SessionID))
	b.WriteString("|")
	b.WriteString(strconv.Quote(filters.Model))
	b.WriteString("|")
	b.WriteString(strconv.Quote(filters.Endpoint))
	b.WriteString("|")
	b.WriteString(strconv.Itoa(filters.MinRetryCount))
	b.WriteString("|")
	b.WriteString(strconv.Quote(string(filters.ReplayFilter)))
	b.WriteString("|")
	b.WriteString(strconv.FormatBool(filters.ExcludeStatusCode200))
	b.WriteString("|")
	b.WriteString(strconv.FormatBool(filters.ActualResponseModelMismatch))
	b.WriteString("|")
	b.WriteString(strconv.FormatBool(ledgerOnly))
	return b.String()
}

// usageLogSummaryFields 复刻 UsageLogSummary 的对象字面量键序。
func usageLogSummaryFields(summary store.UsageLogSummary) jsonObject {
	return jsonObject{}.
		set("totalRequests", summary.TotalRequests).
		set("totalCost", summary.TotalCost).
		set("totalTokens", summary.TotalTokens).
		set("totalInputTokens", summary.TotalInputTokens).
		set("totalOutputTokens", summary.TotalOutputTokens).
		set("totalCacheCreationTokens", summary.TotalCacheCreationTokens).
		set("totalCacheReadTokens", summary.TotalCacheReadTokens).
		set("totalCacheCreation5mTokens", summary.TotalCacheCreation5mTokens).
		set("totalCacheCreation1hTokens", summary.TotalCacheCreation1hTokens)
}

// handleFilterOptions 复刻 getFilterOptions：5 分钟进程内缓存，命中时零查询。
func (m *usageLogsModule) handleFilterOptions(writer http.ResponseWriter, request *http.Request) {
	now := m.now()
	m.optionsMu.Lock()
	if m.optionsModels != nil && now.Before(m.optionsExpiry) {
		models, codes, endpoints := m.optionsModels, m.optionsCodes, m.optionsEnds
		m.optionsMu.Unlock()
		writeOrderedJSONResponse(writer, http.StatusOK, filterOptionsFields(models, codes, endpoints))
		return
	}
	m.optionsMu.Unlock()

	models, err := m.deps.Store.ListUsedModels(request.Context())
	if err != nil {
		m.writeActionError(writer, request, err)
		return
	}
	codes, err := m.deps.Store.ListUsedStatusCodes(request.Context())
	if err != nil {
		m.writeActionError(writer, request, err)
		return
	}
	endpoints, err := m.deps.Store.ListUsedEndpoints(request.Context())
	if err != nil {
		m.writeActionError(writer, request, err)
		return
	}

	m.optionsMu.Lock()
	m.optionsModels, m.optionsCodes, m.optionsEnds = models, codes, endpoints
	m.optionsExpiry = now.Add(filterOptionsCacheTTL)
	m.optionsMu.Unlock()

	writeOrderedJSONResponse(writer, http.StatusOK, filterOptionsFields(models, codes, endpoints))
}

// filterOptionsFields 复刻 FilterOptions 的键序：models、statusCodes、endpoints。
func filterOptionsFields(models []string, codes []int, endpoints []string) jsonObject {
	return jsonObject{}.
		set("models", usageLogsEmptyIfNilStrings(models)).
		set("statusCodes", usageLogsEmptyIfNilInts(codes)).
		set("endpoints", usageLogsEmptyIfNilStrings(endpoints))
}

// handleModelList 复刻 getModelList：{items}。
func (m *usageLogsModule) handleModelList(writer http.ResponseWriter, request *http.Request) {
	models, err := m.deps.Store.ListUsedModels(request.Context())
	if err != nil {
		m.writeActionError(writer, request, err)
		return
	}
	writeOrderedJSONResponse(writer, http.StatusOK,
		jsonObject{}.set("items", usageLogsEmptyIfNilStrings(models)))
}

// handleStatusCodeList 复刻 getStatusCodeList：{items}。
func (m *usageLogsModule) handleStatusCodeList(writer http.ResponseWriter, request *http.Request) {
	codes, err := m.deps.Store.ListUsedStatusCodes(request.Context())
	if err != nil {
		m.writeActionError(writer, request, err)
		return
	}
	writeOrderedJSONResponse(writer, http.StatusOK, jsonObject{}.set("items", usageLogsEmptyIfNilInts(codes)))
}

// handleEndpointList 复刻 getEndpointList：{items}。
func (m *usageLogsModule) handleEndpointList(writer http.ResponseWriter, request *http.Request) {
	endpoints, err := m.deps.Store.ListUsedEndpoints(request.Context())
	if err != nil {
		m.writeActionError(writer, request, err)
		return
	}
	writeOrderedJSONResponse(writer, http.StatusOK,
		jsonObject{}.set("items", usageLogsEmptyIfNilStrings(endpoints)))
}

// 复刻 src/lib/constants/usage-logs.constants.ts。
const (
	sessionIDSuggestionMinLen = 2
	sessionIDSuggestionMaxLen = 128
	sessionIDSuggestionLimit  = 20
)

// handleSessionIDSuggestions 复刻 suggestSessionIds + getUsageLogSessionIdSuggestions。
func (m *usageLogsModule) handleSessionIDSuggestions(
	writer http.ResponseWriter,
	request *http.Request,
) {
	query, issues := parseUsageLogsQuery(request)
	if len(issues) > 0 {
		writeUsageLogsValidationProblem(writer, request, issues)
		return
	}

	term := query.Term
	if len(term) > sessionIDSuggestionMaxLen {
		term = term[:sessionIDSuggestionMaxLen]
	}
	term = strings.TrimSpace(term)
	if len(term) < sessionIDSuggestionMinLen {
		// 太短的词直接空数组（Node 连库都不查）。
		writeOrderedJSONResponse(writer, http.StatusOK, jsonObject{}.set("items", []string{}))
		return
	}

	principal, ok := PrincipalFrom(request.Context())
	if !ok {
		m.writeActionError(writer, request, errors.New("adminapi: 请求缺少已认证身份"))
		return
	}
	filters := store.UsageLogSessionIDSuggestionFilters{
		Term:       term,
		KeyID:      query.KeyID,
		ProviderID: query.ProviderID,
		Limit:      sessionIDSuggestionLimit,
	}
	// 非管理员强制按本人过滤（Node: `session.user.role === "admin" ? input.userId : session.user.id`）。
	if !principal.IsAdmin {
		userID := principal.UserID
		filters.UserID = &userID
	} else {
		filters.UserID = query.UserID
	}

	sessionIDs, err := m.deps.Store.FindUsageLogSessionIDSuggestions(
		request.Context(), filters, m.ledgerOnly.ledgerOnly(request))
	if err != nil {
		m.writeActionError(writer, request, err)
		return
	}
	writeOrderedJSONResponse(writer, http.StatusOK,
		jsonObject{}.set("items", usageLogsEmptyIfNilStrings(sessionIDs)))
}

// writeActionError 复刻 usage-logs 的 actionError：状态码按 Node 的子串判定
// （handlers.ts:255-268）——含 "not found"/"不存在" → 404，含 "权限" → 403，否则 400；
// errorCode 恒为 usage_logs.action_failed，detail 是公开文案。
func (m *usageLogsModule) writeActionError(
	writer http.ResponseWriter,
	request *http.Request,
	err error,
) {
	status := http.StatusBadRequest
	message := err.Error()
	switch {
	case usageLogsContainsAny(message, "not found", "不存在"):
		status = http.StatusNotFound
	case usageLogsContainsAny(message, "权限"):
		status = http.StatusForbidden
	}
	problems := m.deps.Problems
	if problems == nil {
		problems = NewProblems(m.deps.Logger)
	}
	problems.WriteActionError(writer, request,
		NewActionError("usage_logs", "usage_logs.action_failed", status, err))
}

func usageLogsContainsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if needle != "" && strings.Contains(value, needle) {
			return true
		}
	}
	return false
}

func usageLogsEmptyIfNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

func usageLogsEmptyIfNilInts(values []int) []int {
	if values == nil {
		return []int{}
	}
	return values
}
