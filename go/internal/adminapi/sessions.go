package adminapi

import (
	"net/http"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是管理面的 **sessions 资源**（/api/v1/sessions*）的注册与共用底座。
//
// 已就绪的六条端点：
//
//	GET    /sessions/{sessionId}                 read  ← sessions_detail_view.go（本波接管）
//	GET    /sessions/{sessionId}/requests        read  ← 本文件
//	GET    /sessions/{sessionId}/origin-chain    read  ← sessions_detail.go
//	GET    /sessions/{sessionId}/messages        read  ← sessions_detail.go
//	GET    /sessions/{sessionId}/messages/exists read  ← sessions_detail.go
//	GET    /sessions                         read  ← sessions_list.go
//	DELETE /sessions/{sessionId}             read  ← sessions_terminate.go
//	POST   /sessions:batchTerminate          read  ← sessions_terminate.go
//
// 唯一真源：src/app/api/v1/resources/sessions/{router,handlers}.ts（形状与鉴权档位）、
// src/lib/api/v1/schemas/sessions.ts（查询校验）、src/actions/active-sessions.ts。
//
// **本波已接管全部端点**：详情面（`GET /sessions/{id}`）的写侧缺口已补齐——数据面现在写
//
//	response            响应正文（有界头尾窗口，见 internal/session/capture.go）
//	requestHeaders      客户端请求头（脱敏）
//	responseHeaders     上游响应头（脱敏）
//	upstreamReq/ResMeta 上游 URL/方法/状态码（URL 过 sanitizeUrl）
//	相位快照 ×4          request/response × before/after（单件 32 KiB 上限）
//
// 详情视图的归一化与合并器已移植（sessions_detail_view.go 的 normalizeRequestSnapshot /
// buildLegacyCompatibilitySnapshots / mergeLegacy*AfterSnapshot；specialSettings 走既有的
// unionSetting）。
//
// 三处**登记的差异**（都是「少一类来源」而不是「形状不同」，见各处的函数注释）：
//
//	clientReqMeta       数据面不写 → 详情页 requestMeta.clientUrl 为 null（Node 有值）
//	Redis specialSettings 数据面不写 → 只由账本审计行派生
//	request.after 的 body/headers  不留痕（避免给每流再加一份正文驻留）→ 为 null，meta 齐备
//
// 一条**已登记的分工差异**：Node 侧的所有权判定走 aggregateMultipleSessionStats 的 user_id 列
// （一次聚合查询顺带拿到），本实现用 store.ResolveAdminSessionOwner 做一次**窄**查询
// （同一套 messageCanonicalSessionLookup 条件，只取 canonical identity 与 user_id）。两者对
// “这个 identity 属不属于调用者”同判，差别只在 Node 顺手算了账本聚合、这里不算。
//
// 详情面的**权限围栏**：工件本身不带权限信息，靠 `session:{id}:req:{seq}:owner`
// 里存的 keyId 与定位器给出的 keyId 比对（session.StoreSessionRequestOwner 写、
// IsSessionRequestOwnedByKey 读）。少了这条围栏，“知道 sessionId 的任何普通用户”
// 都能读到别人的会话正文。

// sessionAPI 是 sessions 资源处理器的依赖集合。
type sessionAPI struct {
	pools *store.Pools
	// observations 读会话观测集合（列表页的 id 来源）；nil 时 /sessions 不注册。
	observations SessionObservationReader
	// sessions 终止物理会话；nil 时两条终止路由不注册。
	sessions SessionTerminator
	// affinity 推进前缀亲和的代际围栏；nil 时两条终止路由不注册。
	affinity SessionAffinityInvalidator
	// artifacts 读会话工件并判所有权；nil 时依赖工件的详情面端点不注册。
	artifacts SessionArtifactReader
	problems  ProblemWriter
	logger    *logx.Logger
}

// RegisterSessionsRoutes 注册 sessions 资源已就绪的端点（见文件头的五条例外）。
//
// 逐条按**各自的依赖**判定：Store 未装配则全部不注册；列表页额外要观测读数，终止额外要
// 终止器与亲和失效器。缺依赖的那条只是不注册（原样回退 Node），不影响同资源其它端点。
func RegisterSessionsRoutes(router *Router, deps Deps) {
	if deps.Store == nil {
		if deps.Logger != nil {
			deps.Logger.Error("admin_sessions_store_unwired", map[string]any{
				"module": "sessions",
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
	api := &sessionAPI{
		pools:        deps.Store,
		observations: deps.SessionObservations,
		sessions:     deps.SessionTerminations,
		affinity:     deps.SessionAffinity,
		artifacts:    deps.SessionArtifacts,
		problems:     problems,
		logger:       logger,
	}

	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/sessions/{sessionId}/requests",
		Access:      AccessRead,
		Module:      "sessions",
		OperationID: "getSessionRequests",
		Handler:     http.HandlerFunc(api.handleSessionRequests),
	})

	// 来源链：数据源在账本表（provider_chain），不依赖任何 Redis 工件，故只要 Store 就注册。
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/sessions/{sessionId}/origin-chain",
		Access:      AccessRead,
		Module:      "sessions",
		OperationID: "getSessionOriginChain",
		Handler:     http.HandlerFunc(api.handleSessionOriginChain),
	})

	// 列表页：要观测集合（ZSET + 并发计数 + info 扫描）才能给出活跃/非活跃与并发数。
	if api.observations != nil {
		router.Add(Route{
			Method:      http.MethodGet,
			Path:        "/sessions",
			Access:      AccessRead,
			Module:      "sessions",
			OperationID: "listSessions",
			Handler:     http.HandlerFunc(api.handleListSessions),
		})
	}

	// 详情面：读的是一整批 Redis 工件（四份相位快照 + 正文 + 双侧头 + 上游元信息）。
	//
	// 与 /sessions/{sessionId}/requests 的分工：那条只读账本，这条把账本聚合与工件合成一个视图。
	// 注册次序不影响匹配——Router.lookup 按路径权重择路（`/messages` 比裸 `{sessionId}` 更具体，
	// 恒胜），与注册序无关；这里只是为了与 Node 的 router.ts 同序便于对照。
	if api.artifacts != nil {
		router.Add(Route{
			Method:      http.MethodGet,
			Path:        "/sessions/{sessionId}",
			Access:      AccessRead,
			Module:      "sessions",
			OperationID: "getSessionDetail",
			Handler:     http.HandlerFunc(api.handleSessionDetail),
		})
	}

	// 两条消息面：读的是数据面写下的 messages 工件，要工件读数**与**所有权围栏（同一个缝隙）。
	if api.artifacts != nil {
		router.Add(Route{
			Method:      http.MethodGet,
			Path:        "/sessions/{sessionId}/messages",
			Access:      AccessRead,
			Module:      "sessions",
			OperationID: "getSessionMessages",
			Handler:     http.HandlerFunc(api.handleSessionMessages),
		})
		router.Add(Route{
			Method:      http.MethodGet,
			Path:        "/sessions/{sessionId}/messages/exists",
			Access:      AccessRead,
			Module:      "sessions",
			OperationID: "hasSessionMessages",
			Handler:     http.HandlerFunc(api.handleSessionMessagesExist),
		})
	}

	// 两条终止：要终止器**且**亲和失效器。缺任一个都不注册——前缀亲和会话的终止以代际失效
	// 为判据，只做绑定 CAS 会留下「列表已终止、绑定可复活」的会话（见 sessions_terminate.go）。
	if api.sessions != nil && api.affinity != nil {
		router.Add(Route{
			Method:      http.MethodDelete,
			Path:        "/sessions/{sessionId}",
			Access:      AccessRead,
			Module:      "sessions",
			OperationID: "terminateSession",
			Handler:     http.HandlerFunc(api.handleTerminateSession),
		})
		router.Add(Route{
			Method:      http.MethodPost,
			Path:        "/sessions:batchTerminate",
			Access:      AccessRead,
			Module:      "sessions",
			OperationID: "batchTerminateSessions",
			Handler:     http.HandlerFunc(api.handleBatchTerminateSessions),
		})
	}
}

// sessionRequestsQuery 是 /sessions/{sessionId}/requests 的解析结果（SessionRequestsQuerySchema）。
type sessionRequestsQuery struct {
	Page     int
	PageSize int
	Order    string
}

// sessionRequestRowBody 是响应里的一行请求。
//
// 字段顺序与 Node 的对象字面量逐字一致（message.ts:2383-2396），便于 A2 对拍直接比对正文。
type sessionRequestRowBody struct {
	ID              int64  `json:"id"`
	SourceSessionID string `json:"sourceSessionId"`
	Sequence        int64  `json:"sequence"`
	DisplaySequence int64  `json:"displaySequence"`
	Model           any    `json:"model"`
	StatusCode      any    `json:"statusCode"`
	CostUSD         any    `json:"costUsd"`
	CreatedAt       any    `json:"createdAt"`
	InputTokens     any    `json:"inputTokens"`
	OutputTokens    any    `json:"outputTokens"`
	ErrorMessage    any    `json:"errorMessage"`
}

// sessionRequestsBody 是 GET /sessions/{sessionId}/requests 的响应体。
type sessionRequestsBody struct {
	Requests []sessionRequestRowBody `json:"requests"`
	Total    int64                   `json:"total"`
	HasMore  bool                    `json:"hasMore"`
}

// handleSessionRequests 复刻 getSessionRequests（active-sessions.ts:1231-1310）。
//
// 权限档位是 read（Node 的 requireAuth("read")），普通用户能进到处理器里，所以**所有权过滤
// 必须在查询里**：非管理员带上 user_id 条件，别人的会话自然查不到，与 Node 一样作答 404
// （而不是 403——Node 侧这一条的错误文案是「Session 不存在」，actionError 把它映射成 404）。
//
// 401 的判据是 **UserID == 0**（无主体）：Node 侧只判 `if (!authSession)`，而 ADMIN_TOKEN 主体
// 的 id 为 -1（JS 真值），故它进到所有权判定后按 `isAdmin` 免过滤、会话不存在则 404。
// 写成 `<= 0` 会把 ADMIN_TOKEN 误答成 401（对拍 D3）。
func (api *sessionAPI) handleSessionRequests(
	writer http.ResponseWriter,
	request *http.Request,
) {
	ctx := request.Context()
	principal, ok := PrincipalFrom(ctx)
	if !ok || principal.UserID == 0 {
		api.problems.WriteProblem(writer, request, http.StatusUnauthorized, "auth.missing",
			"Authentication is required.")
		return
	}

	sessionID := ParamsFrom(ctx)["sessionId"]
	if sessionID == "" {
		// Node 侧路径段非空由路由保证（Hono 不会把空段匹配成参数），这里只是防御。
		api.problems.WriteValidationError(writer, request, []InvalidParam{{
			Path: []any{"sessionId"}, Code: "too_small",
			Message: "String must contain at least 1 character(s)",
		}})
		return
	}

	query, issues := parseSessionRequestsQuery(request)
	if len(issues) > 0 {
		api.problems.WriteValidationError(writer, request, issues)
		return
	}

	ownerUserID := int64(0)
	if !principal.IsAdmin {
		ownerUserID = principal.UserID
	}

	owner, found, err := api.pools.ResolveAdminSessionOwner(ctx, sessionID, ownerUserID)
	if err != nil {
		api.writeSessionFailure(writer, request, "getSessionRequests", err)
		return
	}
	if !found {
		api.problems.WriteActionError(writer, request,
			NewActionError("session", "session.not_found", http.StatusNotFound, nil))
		return
	}

	offset := (query.Page - 1) * query.PageSize
	page, err := api.pools.FindAdminSessionRequests(
		ctx, owner.CanonicalIdentity, ownerUserID, query.PageSize, offset, query.Order,
	)
	if err != nil {
		api.writeSessionFailure(writer, request, "getSessionRequests", err)
		return
	}

	// Node 侧 flatMap 掉了没有物理 session_id 的行；SQL 已用 `session_id IS NOT NULL` 与
	// `request_sequence IS NOT NULL` 过滤，故这里的 sequence 恒有值（Node 的 `?? 1` 只是
	// 类型收窄，不会命中），不再重复判空。
	rows := make([]sessionRequestRowBody, 0, len(page.Requests))
	for _, row := range page.Requests {
		body := sessionRequestRowBody{
			ID:              row.ID,
			SourceSessionID: row.SourceSessionID,
			Sequence:        row.Sequence,
			DisplaySequence: row.DisplaySequence,
			Model:           sessionOptionalString(row.Model),
			StatusCode:      sessionOptionalInt(row.StatusCode),
			CostUSD:         sessionOptionalString(row.CostUSD),
			CreatedAt:       jsonTime(row.CreatedAt),
			InputTokens:     sessionOptionalInt64(row.InputTokens),
			OutputTokens:    sessionOptionalInt64(row.OutputTokens),
			ErrorMessage:    sessionOptionalString(row.ErrorMessage),
		}
		rows = append(rows, body)
	}

	writeSessionsJSON(writer, http.StatusOK, sessionRequestsBody{
		Requests: rows,
		Total:    page.Total,
		HasMore:  int64(offset+len(rows)) < page.Total,
	})
}

// writeSessionsJSON 作答 JSON 正文（不转义 HTML，与 JS 的 JSON.stringify 同判）。
func writeSessionsJSON(writer http.ResponseWriter, status int, body any) {
	encoded, err := marshalNoEscape(body)
	if err != nil {
		writer.WriteHeader(http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write(encoded)
}

// writeSessionFailure 作答 400 OPERATION_FAILED（复刻 dashboard 资源同一条兜底）。
func (api *sessionAPI) writeSessionFailure(
	writer http.ResponseWriter,
	request *http.Request,
	operation string,
	err error,
) {
	api.logger.Error("admin_sessions_operation_failed", map[string]any{
		"operation": operation,
		"path":      request.URL.Path,
		"error":     err.Error(),
	})
	api.problems.WriteProblem(writer, request, http.StatusBadRequest, "OPERATION_FAILED", "")
}

// parseSessionRequestsQuery 复刻 SessionRequestsQuerySchema（schemas/sessions.ts:33-37）。
func parseSessionRequestsQuery(
	request *http.Request,
) (sessionRequestsQuery, []InvalidParam) {
	values := request.URL.Query()
	query := sessionRequestsQuery{Page: 1, PageSize: 20, Order: "desc"}
	issues := []InvalidParam{}

	// page: z.coerce.number().int().min(1).default(1)
	if value, present, issue := coerceOptionalInt(values, "page", 1, 0); issue != nil {
		issues = append(issues, InvalidParam{
			Path: issue.Path, Code: issue.Code, Message: issue.Message,
		})
	} else if present {
		query.Page = value
	}

	// pageSize: z.coerce.number().int().min(1).max(200).default(20)
	if value, present, issue := coerceOptionalInt(values, "pageSize", 1, 200); issue != nil {
		issues = append(issues, InvalidParam{
			Path: issue.Path, Code: issue.Code, Message: issue.Message,
		})
	} else if present {
		query.PageSize = value
	}

	// order: z.enum(["asc", "desc"]).default("desc")
	if raw, present := queryValue(values, "order"); present {
		if raw != "asc" && raw != "desc" {
			issues = append(issues, InvalidParam{
				Path: []any{"order"}, Code: "invalid_enum_value",
				Message: "Invalid enum value",
			})
		} else {
			query.Order = raw
		}
	}

	if len(issues) > 0 {
		return sessionRequestsQuery{}, issues
	}
	return query, nil
}

// sessionOptionalString 把可空列渲染成 JSON（nil 保持 null）。
func sessionOptionalString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// sessionOptionalInt 把可空 int 列渲染成 JSON。
func sessionOptionalInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

// optionalInt64 把可空 int64 列渲染成 JSON。
func sessionOptionalInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}
