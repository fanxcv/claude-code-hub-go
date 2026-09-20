package adminapi

import (
	"context"
	"net/http"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 `GET /sessions`（列表页）。
//
// 唯一真源：
//   - 路由与形状：src/app/api/v1/resources/sessions/{router,handlers}.ts（listSessions）。
//   - 查询校验：src/lib/api/v1/schemas/sessions.ts 的 SessionsListQuerySchema。
//   - 业务语义：src/actions/active-sessions.ts 的 getActiveSessions（:250-410）与
//     getAllSessions（:412-645）。
//
// 两个分支（Node 原样）：
//
//	?state=active（默认）→ { "items": [ … ] }
//	?state=all          → { "active": […], "inactive": […], "totalActive": N, "totalInactive": N,
//	                        "hasMoreActive": bool, "hasMoreInactive": bool }
//
// 「活跃」的判定是**两条件或**：并发数 > 0（ZSET 里仍在的会话）或最后一次请求在 5 分钟内。
// 五条防坑（都是 Node 的行为，抄错就会与 Node 分叉）：
//
//  1. **聚合查询不带 owner 条件**（Node 的 aggregateMultipleSessionStats(sessionIds) 没有第二个
//     实参），普通用户的过滤发生在**取回之后**的内存里。写成 SQL 侧过滤，输出的键集一样、但
//     排序与 total/hasMore 的取数基数会变（Node 的 totalActive 是「过滤后的条数」）。这里逐字
//     照 Node：先全量聚合，再按 userId 过滤。
//  2. **游标基数是恒等的 5 分钟**，不是观测窗口（SESSION_TTL）：即使观测窗口配短了，5 分钟内
//     有请求的会话仍算活跃。
//  3. sessionIds 是**并集**：观测集合（ZSET）在前、`session:*:info` 扫描结果在后，按首次出现
//     去重；保持这个顺序，否则分页的先后与 Node 不同。
//  4. `providerId: s.providers[0]?.id || null` 是 `||` 不是 `??`：0 也归 null。
//  5. sessionFingerprint 为 null 时**不省略**，keyId 是数字不是字符串（Node 的两处类型收窄）。

// fiveMinuteActivityWindowMillis 是「最近活跃」的窗口（Node 的 `5 * 60 * 1000`）。
const fiveMinuteActivityWindowMillis = 5 * 60 * 1000

// SessionObservationReader 读会话观测集合（列表页的会话 id 来源）。
//
// nil 表示未装配：/sessions 不注册（回退 Node）——半个列表（有聚合无并发数，或有并发数无
// 观测集合）会让「活跃/非活跃」的分组一起错，比不接管更坏。
type SessionObservationReader interface {
	// ObservedActiveSessions 复刻 SessionTracker.getObservedActiveSessions。
	ObservedActiveSessions(ctx context.Context) ([]string, error)
	// ObservedConcurrentCounts 复刻 getObservedConcurrentCountBatch（缺失身份不在返回表里）。
	ObservedConcurrentCounts(ctx context.Context, sessionIdentities []string) (map[string]int, error)
	// AllSessionIDs 复刻 SessionManager.getAllSessionIds（SCAN `session:*:info`）。
	AllSessionIDs(ctx context.Context) ([]string, error)
}

// sessionsListQuery 是 /sessions 的解析结果（SessionsListQuerySchema）。
type sessionsListQuery struct {
	State        string
	ActivePage   int
	InactivePage int
	PageSize     int
}

// activeSessionBody 是一行会话（ActiveSessionInfo，types/session.ts）。
//
// 字段顺序与 Node 的对象字面量逐字一致，便于对拍直接比对正文。
type activeSessionBody struct {
	SessionID                string `json:"sessionId"`
	SessionIdentityKind      string `json:"sessionIdentityKind"`
	SessionFingerprint       any    `json:"sessionFingerprint"`
	UserName                 string `json:"userName"`
	UserID                   int64  `json:"userId"`
	KeyID                    int64  `json:"keyId"`
	KeyName                  string `json:"keyName"`
	ProviderID               any    `json:"providerId"`
	ProviderName             any    `json:"providerName"`
	Model                    any    `json:"model"`
	APIType                  string `json:"apiType"`
	StartTime                int64  `json:"startTime"`
	InputTokens              int64  `json:"inputTokens"`
	OutputTokens             int64  `json:"outputTokens"`
	CacheCreationInputTokens int64  `json:"cacheCreationInputTokens"`
	CacheReadInputTokens     int64  `json:"cacheReadInputTokens"`
	TotalTokens              int64  `json:"totalTokens"`
	CostUSD                  string `json:"costUsd"`
	Status                   string `json:"status"`
	DurationMS               int64  `json:"durationMs"`
	RequestCount             int64  `json:"requestCount"`
	ConcurrentCount          int    `json:"concurrentCount"`
}

// sessionsListBody 是 `?state=active` 的响应体。
type sessionsListBody struct {
	Items []activeSessionBody `json:"items"`
}

// allSessionsBody 是 `?state=all` 的响应体。
type allSessionsBody struct {
	Active          []activeSessionBody `json:"active"`
	Inactive        []activeSessionBody `json:"inactive"`
	TotalActive     int                 `json:"totalActive"`
	TotalInactive   int                 `json:"totalInactive"`
	HasMoreActive   bool                `json:"hasMoreActive"`
	HasMoreInactive bool                `json:"hasMoreInactive"`
}

// handleListSessions 复刻 listSessions → getActiveSessions / getAllSessions。
//
// 401 的判据与 /sessions/{id}/requests 同：UserID == 0 表示无主体（ADMIN_TOKEN 主体的 id 为 -1，
// 会进到所有权判定后按 isAdmin 免过滤，见 sessions.go 的说明）。
func (api *sessionAPI) handleListSessions(writer http.ResponseWriter, request *http.Request) {
	ctx := request.Context()
	principal, ok := PrincipalFrom(ctx)
	if !ok || principal.UserID == 0 {
		api.problems.WriteProblem(writer, request, http.StatusUnauthorized, "auth.missing",
			"Authentication is required.")
		return
	}
	if api.observations == nil {
		// 只可能在装配后被摘掉依赖（RegisterSessionsRoutes 已按 nil 判定不注册）。
		api.writeSessionFailure(writer, request, "listSessions", errSessionsObservationAbsent{})
		return
	}

	query, issues := parseSessionsListQuery(request)
	if len(issues) > 0 {
		api.problems.WriteValidationError(writer, request, issues)
		return
	}

	// 1. 取会话 id 集合。活跃分支只取观测集合；全量分支取并集（观测在前，见文件头第 3 条）。
	observed, err := api.observations.ObservedActiveSessions(ctx)
	if err != nil {
		api.writeSessionFailure(writer, request, "listSessions", err)
		return
	}
	sessionIDs := observed
	if query.State == "all" {
		stored, storedErr := api.observations.AllSessionIDs(ctx)
		if storedErr != nil {
			api.writeSessionFailure(writer, request, "listSessions", storedErr)
			return
		}
		sessionIDs = appendUniqueStrings(observed, stored)
	}
	if len(sessionIDs) == 0 {
		if query.State == "all" {
			adminWriteJSON(writer, http.StatusOK, allSessionsBody{
				Active:   []activeSessionBody{},
				Inactive: []activeSessionBody{},
			})
			return
		}
		adminWriteJSON(writer, http.StatusOK, sessionsListBody{Items: []activeSessionBody{}})
		return
	}

	// 2. 全量聚合（**不带 owner**，见文件头第 1 条），再取并发数。
	summaries, err := api.pools.AggregateAdminSessionStats(ctx, sessionIDs, 0)
	if err != nil {
		api.writeSessionFailure(writer, request, "listSessions", err)
		return
	}
	canonicalIDs := make([]string, 0, len(summaries))
	for _, summary := range summaries {
		canonicalIDs = append(canonicalIDs, summary.SessionID)
	}
	concurrentCounts, err := api.observations.ObservedConcurrentCounts(ctx, canonicalIDs)
	if err != nil {
		api.writeSessionFailure(writer, request, "listSessions", err)
		return
	}

	// 3. 普通用户只看自己的（内存过滤，与 Node 同）。
	filtered := summaries
	if !principal.IsAdmin {
		filtered = make([]store.AdminSessionSummary, 0, len(summaries))
		for _, summary := range summaries {
			if summary.UserID == principal.UserID {
				filtered = append(filtered, summary)
			}
		}
	}

	rows := make([]activeSessionBody, 0, len(filtered))
	for _, summary := range filtered {
		rows = append(rows, buildActiveSessionBody(summary, concurrentCounts[summary.SessionID]))
	}

	if query.State != "all" {
		adminWriteJSON(writer, http.StatusOK, sessionsListBody{Items: rows})
		return
	}

	// 4. 活跃/非活跃分流 + 分页（顺序：先全量分流，再各自切片）。
	now := nowMillis()
	active := make([]activeSessionBody, 0, len(rows))
	inactive := make([]activeSessionBody, 0, len(rows))
	for index, row := range rows {
		summary := filtered[index]
		if isActiveSession(summary, row.ConcurrentCount, now) {
			active = append(active, row)
			continue
		}
		inactive = append(inactive, row)
	}

	activeOffset := (query.ActivePage - 1) * query.PageSize
	inactiveOffset := (query.InactivePage - 1) * query.PageSize
	paginatedActive := sliceSessions(active, activeOffset, query.PageSize)
	paginatedInactive := sliceSessions(inactive, inactiveOffset, query.PageSize)

	adminWriteJSON(writer, http.StatusOK, allSessionsBody{
		Active:          paginatedActive,
		Inactive:        paginatedInactive,
		TotalActive:     len(active),
		TotalInactive:   len(inactive),
		HasMoreActive:   activeOffset+len(paginatedActive) < len(active),
		HasMoreInactive: inactiveOffset+len(paginatedInactive) < len(inactive),
	})
}

// buildActiveSessionBody 把一条聚合结果转成一行会话（Node 的两个 map 回调逐字一致）。
func buildActiveSessionBody(
	summary store.AdminSessionSummary,
	concurrentCount int,
) activeSessionBody {
	startTime := nowMillis()
	if summary.FirstRequestAt != nil {
		startTime = summary.FirstRequestAt.UnixMilli()
	}
	status := "completed"
	if concurrentCount > 0 {
		status = "in_progress"
	}

	// providerId 用 `||`（0 也归 null），providerName / model 用 join(", ") 且空集归 null。
	var providerID any
	providerNames := make([]string, 0, len(summary.Providers))
	for index, provider := range summary.Providers {
		if index == 0 && provider.ID > 0 {
			providerID = provider.ID
		}
		providerNames = append(providerNames, provider.Name)
	}
	var providerName any
	if len(providerNames) > 0 {
		providerName = joinNonEmpty(providerNames, ", ")
	}
	var model any
	if len(summary.Models) > 0 {
		model = joinNonEmpty(summary.Models, ", ")
	}
	apiType := "chat"
	if summary.APIType != nil && *summary.APIType != "" {
		apiType = *summary.APIType
	}
	var fingerprint any
	if summary.SessionFingerprint != nil {
		fingerprint = *summary.SessionFingerprint
	}

	return activeSessionBody{
		SessionID:                summary.SessionID,
		SessionIdentityKind:      summary.SessionIdentityKind,
		SessionFingerprint:       fingerprint,
		UserName:                 summary.UserName,
		UserID:                   summary.UserID,
		KeyID:                    summary.KeyID,
		KeyName:                  summary.KeyName,
		ProviderID:               providerID,
		ProviderName:             providerName,
		Model:                    model,
		APIType:                  apiType,
		StartTime:                startTime,
		InputTokens:              summary.TotalInputTokens,
		OutputTokens:             summary.TotalOutputTokens,
		CacheCreationInputTokens: summary.TotalCacheCreationTokens,
		CacheReadInputTokens:     summary.TotalCacheReadTokens,
		TotalTokens: summary.TotalInputTokens + summary.TotalOutputTokens +
			summary.TotalCacheCreationTokens + summary.TotalCacheReadTokens,
		CostUSD:         summary.TotalCostUSD,
		Status:          status,
		DurationMS:      summary.TotalDurationMS,
		RequestCount:    summary.RequestCount,
		ConcurrentCount: concurrentCount,
	}
}

// isActiveSession 复刻 Node 的活跃判据：并发数 > 0（`isConcurrent`）或最后请求在 5 分钟内。
//
// lastRequestAt 为 null 时 Node 取 0（`s.lastRequestAt ? … : 0`），于是只能靠并发数进活跃组。
func isActiveSession(summary store.AdminSessionSummary, concurrentCount int, now int64) bool {
	if concurrentCount > 0 {
		return true
	}
	if summary.LastRequestAt == nil {
		return false
	}
	return now-summary.LastRequestAt.UnixMilli() < fiveMinuteActivityWindowMillis
}

// sliceSessions 复刻 Node 的 `array.slice(offset, offset + pageSize)`（越界一律空切片，不补 null）。
func sliceSessions(rows []activeSessionBody, offset, pageSize int) []activeSessionBody {
	if offset >= len(rows) {
		return []activeSessionBody{}
	}
	end := offset + pageSize
	if end > len(rows) {
		end = len(rows)
	}
	return rows[offset:end]
}

// joinNonEmpty 复刻 JS 的 `array.join(", ")`：空数组在调用处已被判掉（Node 用 `|| null`）。
func joinNonEmpty(items []string, separator string) string {
	result := ""
	for index, item := range items {
		if index > 0 {
			result += separator
		}
		result += item
	}
	return result
}

// appendUniqueStrings 复刻 `Array.from(new Set([...observed, ...stored]))`：并集、保持首次出现顺序。
func appendUniqueStrings(observed, stored []string) []string {
	merged := make([]string, 0, len(observed)+len(stored))
	seen := make(map[string]struct{}, len(observed)+len(stored))
	for _, list := range [][]string{observed, stored} {
		for _, item := range list {
			if item == "" {
				continue
			}
			if _, duplicate := seen[item]; duplicate {
				continue
			}
			seen[item] = struct{}{}
			merged = append(merged, item)
		}
	}
	return merged
}

// parseSessionsListQuery 复刻 SessionsListQuerySchema（schemas/sessions.ts:7-12）。
func parseSessionsListQuery(request *http.Request) (sessionsListQuery, []InvalidParam) {
	values := request.URL.Query()
	query := sessionsListQuery{State: "active", ActivePage: 1, InactivePage: 1, PageSize: 20}
	issues := []InvalidParam{}

	// state: z.enum(["active", "all"]).default("active")
	if raw, present := queryValue(values, "state"); present {
		if raw != "active" && raw != "all" {
			issues = append(issues, InvalidParam{
				Path: []any{"state"}, Code: "invalid_enum_value",
				Message: "Invalid enum value",
			})
		} else {
			query.State = raw
		}
	}

	// activePage / inactivePage: z.coerce.number().int().min(1).default(1)
	for _, field := range []struct {
		name  string
		apply func(int)
	}{
		{"activePage", func(value int) { query.ActivePage = value }},
		{"inactivePage", func(value int) { query.InactivePage = value }},
	} {
		value, present, issue := coerceOptionalInt(values, field.name, 1, 0)
		if issue != nil {
			issues = append(issues, InvalidParam{
				Path: issue.Path, Code: issue.Code, Message: issue.Message,
			})
			continue
		}
		if present {
			field.apply(value)
		}
	}

	// pageSize: z.coerce.number().int().min(1).max(200).default(20)
	value, present, issue := coerceOptionalInt(values, "pageSize", 1, 200)
	if issue != nil {
		issues = append(issues, InvalidParam{
			Path: issue.Path, Code: issue.Code, Message: issue.Message,
		})
	} else if present {
		query.PageSize = value
	}

	if len(issues) > 0 {
		return sessionsListQuery{}, issues
	}
	return query, nil
}

// errSessionsObservationAbsent 是「依赖在装配后被摘掉」的程序错误（正常路径不该发生）。
type errSessionsObservationAbsent struct{}

func (errSessionsObservationAbsent) Error() string {
	return "sessions: 会话观测读数未装配"
}

// nowMillis 是本文件唯一的时钟出口（Node 的 Date.now()）。
//
// 之所以留一个函数：活跃判定与 startTime 兜底都要「同一时刻的毫秒」，测试要能在不 sleep 的
// 前提下钉住边界。
var nowMillis = func() int64 {
	return time.Now().UnixMilli()
}
