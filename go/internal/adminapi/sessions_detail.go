package adminapi

import (
	"context"
	"net/http"

	"github.com/fanxcv/claude-code-hub-go/go/internal/session"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 sessions 资源**详情面**的共用底座与已就绪端点。
//
// 已就绪：
//
//	GET /sessions/{sessionId}/origin-chain  read  ← 本文件
//
// 唯一真源：src/app/api/v1/resources/sessions/handlers.ts（形状与错误映射）、
// src/lib/api/v1/schemas/sessions.ts（查询校验）、src/actions/session-origin-chain.ts（业务）、
// src/lib/session-request-locator.ts（定位器选择器语义）、src/repository/message.ts
// 的 findSessionRequestLocator / findSessionOriginChain。
//
// 详情面的其余四条（detail / messages / messages-exists / response）读的是**数据面写下的
// Redis 工件**，而 Go 数据面至今不写其中的大多数（响应体、请求/响应头、上游元信息、
// 相位快照、工件所有者键）。只接读侧会得到恒 404 的端点，比不接管更坏；故写侧补齐后
// 才逐条接手，见 internal/session/artifacts.go 与本包 sessions.go 的文件头。
//
// origin-chain 与那四条**不同**：它的数据源全在**账本表**（message_request 的 provider_chain），
// 不依赖任何 Redis 工件，故可在写侧补齐之前独立接手。

// sessionRequestLocatorError 是定位器失败的两类业务码（Node 的 BUSINESS_ERRORS 取值）。
//
// 两者都不是「不存在」也不是「无权」，故 Node 的 actionError 把它们的 HTTP 状态判成 400；
// 错误码原样带出（Node 的 errorCode 就是这两个串本身）。
const (
	sessionRequestSourceMismatch     = "SESSION_REQUEST_SOURCE_MISMATCH"
	sessionRequestSelectorIncomplete = "SESSION_REQUEST_SELECTOR_INCOMPLETE"
)

// sessionSequenceQuery 是 SessionSequenceQuerySchema 的解析结果。
//
// 三个字段都带 present 语义，但**别把 `requestSequence=0` 当成「与没传同判」**：
// Node 的 schema 是 `z.coerce.number().int().positive().optional()`（sessions.ts:15），
// 所以 0 在**解析层就被拒**（400 validation），走不到 action 层那句
// `requestId !== undefined || normalizeRequestSequence(requestSequence) !== null`
// （active-sessions.ts:836）。Go 侧用 `coerceOptionalInt(..., min=1)` 复刻同一语义：
// 0/负数 → 400 too_small；只有**合法正数**才置「指定了序号」。
// 另外 `normalizeSessionRequestSequence`（本文件下方）是 TS `normalizeRequestSequence`
// 的镜像，而那个 util 服务于 action 层；REST 这一层因解析已挡下非正数而用不到它。
type sessionSequenceQuery struct {
	RequestSequence    int64
	HasRequestSequence bool
	SourceSessionID    string
}

// parseSessionSequenceQuery 复刻 SessionSequenceQuerySchema。
func parseSessionSequenceQuery(request *http.Request) (sessionSequenceQuery, []InvalidParam) {
	values := request.URL.Query()
	query := sessionSequenceQuery{}
	issues := []InvalidParam{}

	// requestSequence: z.coerce.number().int().positive().optional()
	if value, present, issue := coerceOptionalInt(values, "requestSequence", 1, 0); issue != nil {
		issues = append(issues, sessionSequenceIssue(issue))
	} else if present {
		query.RequestSequence = int64(value)
		query.HasRequestSequence = true
	}

	// sourceSessionId: z.string().min(1).optional()
	if raw, present := queryValue(values, "sourceSessionId"); present {
		if raw == "" {
			issues = append(issues, InvalidParam{
				Path: []any{"sourceSessionId"}, Code: "too_small",
				Message: "String must contain at least 1 character(s)",
			})
		} else {
			query.SourceSessionID = raw
		}
	}

	if len(issues) > 0 {
		return sessionSequenceQuery{}, issues
	}
	return query, nil
}

// sessionSequenceIssue 把 usage-logs 的校验问题结构转成本包形态。
//
// 转而不是复用：两族端点的 InvalidParam 是同一个类型，但 usage-logs 的 issue 结构名带着
// 那一族的语义；这里只做字段搬运，不引入新的判定。
func sessionSequenceIssue(issue *usageLogsValidationIssue) InvalidParam {
	return InvalidParam{Path: issue.Path, Code: issue.Code, Message: issue.Message}
}

// sessionQueryWithRequestID 解析 SessionExistsQuerySchema / SessionDetailQuerySchema
// （= Sequence 查询 + requestId）。
func sessionQueryWithRequestID(
	request *http.Request,
) (sessionSequenceQuery, int64, bool, []InvalidParam) {
	values := request.URL.Query()
	query, issues := parseSessionSequenceQuery(request)
	requestID := int64(0)
	hasRequestID := false

	// requestId: z.coerce.number().int().positive().optional()
	if value, present, issue := coerceOptionalInt(values, "requestId", 1, 0); issue != nil {
		issues = append(issues, sessionSequenceIssue(issue))
	} else if present {
		requestID = int64(value)
		hasRequestID = true
	}
	if len(issues) > 0 {
		return sessionSequenceQuery{}, 0, false, issues
	}
	return query, requestID, hasRequestID, nil
}

// resolveAdminSessionRequestLocator 复刻 resolveSessionRequestLocator（session-request-locator.ts）。
//
// 返回的 error 是**已定档的 action 错误**（调用方直接 WriteActionError 作答），nil 表示成功。
// 这条链的每一步都有可观察后果，逐条对应 Node：
//
//  1. 带 requestId 时直接用「物理 lookup」查一次，查不到即 SOURCE_MISMATCH——**跳过**前缀亲和的
//     完整性校验（Node :27-41 直接 return）。
//  2. 不带 requestId 时先查一次「无选择器」的定位行：它给出 identityKind，用于下面那条判据。
//     查不到即 SOURCE_MISMATCH。
//  3. **前缀亲和的完整性判据**：identityKind 为 prefix_affinity 且未给 requestId 时，
//     「给了序号没给物理来源」或「给了物理来源没给序号」都是残缺的选择器，答
//     SELECTOR_INCOMPLETE——前缀亲和的会话在物理上会跨多个 session_id，缺任一半都无法唯一确定
//     目标行。两者都给或都不给才继续。
//  4. 指定了序号或物理来源时按选择器再查一次；否则复用第 2 步那条。
func (api *sessionAPI) resolveAdminSessionRequestLocator(
	ctx context.Context,
	identity string,
	query sessionSequenceQuery,
	requestID int64,
	hasRequestID bool,
	ownerUserID int64,
) (*store.AdminSessionRequestLocator, error) {
	if hasRequestID {
		locator, err := api.pools.FindAdminSessionRequestLocator(ctx, identity,
			store.AdminSessionRequestLocatorSelector{
				RequestID:       requestID,
				RequestSequence: query.RequestSequence,
				SourceSessionID: query.SourceSessionID,
			}, ownerUserID)
		if err != nil {
			return nil, err
		}
		if locator == nil {
			return nil, newSessionActionError(sessionRequestSourceMismatch)
		}
		return locator, nil
	}

	identityLocator, err := api.pools.FindAdminSessionRequestLocator(
		ctx, identity, store.AdminSessionRequestLocatorSelector{}, ownerUserID)
	if err != nil {
		return nil, err
	}
	if identityLocator == nil {
		return nil, newSessionActionError(sessionRequestSourceMismatch)
	}

	hasSequence := query.HasRequestSequence
	hasSource := query.SourceSessionID != ""
	if identityLocator.IdentityKind == "prefix_affinity" &&
		((hasSequence && !hasSource) || (!hasSequence && hasSource)) {
		return nil, newSessionActionError(sessionRequestSelectorIncomplete)
	}

	if !hasSequence && !hasSource {
		return identityLocator, nil
	}
	locator, err := api.pools.FindAdminSessionRequestLocator(ctx, identity,
		store.AdminSessionRequestLocatorSelector{
			RequestSequence: query.RequestSequence,
			SourceSessionID: query.SourceSessionID,
		}, ownerUserID)
	if err != nil {
		return nil, err
	}
	if locator == nil {
		return nil, newSessionActionError(sessionRequestSourceMismatch)
	}
	return locator, nil
}

// newSessionActionError 造一个会话资源的 action 错误（Node 的 errorCode 原样带出）。
//
// 状态码判据来自 Node 的 actionError：详情文案不含「不存在」「过期」「无权」「权限」四个子串，
// 故一律 400。**不移植子串判定**（与 problem.go 的既定裁决一致），改在这里显式定档：这两个
// 业务码对应 400，是唯一可能的状态。
func newSessionActionError(code string) error {
	return NewActionError("session", code, http.StatusBadRequest, nil)
}

// SessionArtifactReader 是详情面读会话工件与判所有权的缝隙。
//
// 两个能力放在同一个缝隙里是刻意的：读工件与判「这份工件是不是你的」必须同源——把围栏拆到
// 别处（或让调用方自己判）会让「读了工件但忘了判权限」变成一次参数省略，而不是一次编译错误。
// nil 表示未装配：依赖它的端点（messages 与 messages-exists）不注册。
type SessionArtifactReader interface {
	// IsSessionRequestOwnedByKey 判这份工件是不是该 keyId 写的（见 internal/session/artifacts_read.go）。
	IsSessionRequestOwnedByKey(ctx context.Context, sessionID string, sequence int, expectedKeyID int64) bool
	// SessionMessages 读按序号的 messages 工件。
	SessionMessages(ctx context.Context, sessionID string, sequence int) (any, bool, error)
	// SessionRequestBody 读请求正文工件（详情页 legacy 回退）。
	SessionRequestBody(ctx context.Context, sessionID string, sequence int) (any, bool)
	// SessionResponseBody 读响应正文工件（详情页 legacy 回退）。
	SessionResponseBody(ctx context.Context, sessionID string, sequence int) (string, bool)
	// SessionRequestHeaders 读客户端请求头工件。
	SessionRequestHeaders(ctx context.Context, sessionID string, sequence int) map[string]string
	// SessionResponseHeaders 读上游响应头工件。
	SessionResponseHeaders(ctx context.Context, sessionID string, sequence int) map[string]string
	// ReadSessionPhaseSnapshot 读一份相位快照（kind × phase）。
	ReadSessionPhaseSnapshot(ctx context.Context, sessionID string, sequence int, kind, phase string) *session.SessionDetailSnapshotRead
	// ReadSessionClientRequestMeta 读客户端请求元信息（详情页 requestMeta.clientUrl 的来源）。
	ReadSessionClientRequestMeta(ctx context.Context, sessionID string, sequence int) *session.SessionUpstreamRequestMetaRead
	// ReadSessionUpstreamRequestMeta 读上游请求元信息。
	ReadSessionUpstreamRequestMeta(ctx context.Context, sessionID string, sequence int) *session.SessionUpstreamRequestMetaRead
	// ReadSessionUpstreamResponseMeta 读上游响应元信息。
	ReadSessionUpstreamResponseMeta(ctx context.Context, sessionID string, sequence int) *session.SessionUpstreamResponseMetaRead
	// HasAnySessionMessages 判会话是否有任意 messages（含旧格式键）。
	HasAnySessionMessages(ctx context.Context, sessionID string) bool
}

// sessionOwnerOrError 完成「解析规范 identity + 所有者」这一步，并按 Node 的 actionError 定档。
//
// 返回的 error 为 nil 时 owner 有效。三个失败分支的状态码与错误码逐条对齐 Node：
//
//	查不到                 → 404 session.not_found（文案「Session 不存在」）
//	非管理员且归属不符     → 403 session.action_failed（文案「无权访问该 Session」）
//	查询故障               → 由调用方按各自 action 的 catch 分支定档
func (api *sessionAPI) sessionOwnerOrError(
	ctx context.Context,
	sessionID string,
	ownerUserID int64,
	isAdmin bool,
	operation string,
) (store.AdminSessionOwner, error, bool) {
	owner, found, err := api.pools.ResolveAdminSessionOwner(ctx, sessionID, ownerUserID)
	if err != nil {
		return store.AdminSessionOwner{}, err, true
	}
	if !found {
		return store.AdminSessionOwner{}, NewActionError(
			"session", "session.not_found", http.StatusNotFound, nil), false
	}
	if !isAdmin && owner.UserID != ownerUserID {
		// SQL 侧已按 owner 过滤，这里保留 Node 的显式二次判定（过滤策略变化时同判）。
		return store.AdminSessionOwner{}, NewActionError(
			"session", "session.action_failed", http.StatusForbidden, nil), false
	}
	return owner, nil, false
}

// handleSessionOriginChain 复刻 getSessionOriginChain（session-origin-chain.ts）。
//
// 响应体是**裸数组或 null**（不是包装对象）：Node 的 handler 走 actionJson，直接把 action 的
// data 当作正文。
func (api *sessionAPI) handleSessionOriginChain(
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
		api.problems.WriteValidationError(writer, request, []InvalidParam{{
			Path: []any{"sessionId"}, Code: "too_small",
			Message: "String must contain at least 1 character(s)",
		}})
		return
	}

	query, issues := parseSessionSequenceQuery(request)
	if len(issues) > 0 {
		api.problems.WriteValidationError(writer, request, issues)
		return
	}

	ownerUserID := int64(0)
	if !principal.IsAdmin {
		ownerUserID = principal.UserID
	}

	owner, ownerErr, isFailure := api.sessionOwnerOrError(
		ctx, sessionID, ownerUserID, principal.IsAdmin, "getSessionOriginChain")
	if ownerErr != nil {
		api.writeSessionActionError(writer, request, isFailure, "getSessionOriginChain", ownerErr)
		return
	}

	locator, err := api.resolveAdminSessionRequestLocator(
		ctx, owner.CanonicalIdentity, query, 0, false, owner.UserID)
	if err != nil {
		api.writeSessionActionError(writer, request, false, "getSessionOriginChain", err)
		return
	}

	// Node 的 findSessionOriginChain 也带 owner 条件（sessionStats.userId），不是调用者。
	chain, err := api.pools.FindAdminSessionOriginChain(ctx, locator.RequestID, locator.KeyID, owner.UserID)
	if err != nil {
		api.writeSessionActionError(writer, request, true, "getSessionOriginChain", err)
		return
	}
	if chain == nil {
		adminWriteJSON(writer, http.StatusOK, nil)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(chain)
}

// writeSessionActionError 把 action 错误作答；dependencyFailure 为真时走该 action 的**catch 分支**
// 定档（Node 的 catch 返回 `{ok:false, error:"…失败"}`，无 errorCode ⇒ 400 session.action_failed），
// 否则按调用方给定的显式码作答。
func (api *sessionAPI) writeSessionActionError(
	writer http.ResponseWriter,
	request *http.Request,
	dependencyFailure bool,
	operation string,
	err error,
) {
	if dependencyFailure {
		api.logger.Error("admin_sessions_operation_failed", map[string]any{
			"operation": operation,
			"path":      request.URL.Path,
			"error":     err.Error(),
		})
		api.problems.WriteActionError(writer, request,
			NewActionError("session", "session.action_failed", http.StatusBadRequest, nil))
		return
	}
	api.problems.WriteActionError(writer, request, err)
}

// handleSessionMessages 复刻 getSessionMessages（active-sessions.ts:659）。
//
// 响应体是**裸的 messages 值**（数组或对象）：Node 走 actionJson，直接把 action 的 data 当正文。
//
// 两处「未存储或已过期」都是 **404 session.not_found**，不是 403 也不是 400：
// Node 的 actionError 按详情文案里的「过期」定成 404，再按 404 赋上兜底码。工件不存在与
// 工件已过 TTL 在 Node 侧本来就不可区分（同一条 GET 返回 null），这里同样合判。
func (api *sessionAPI) handleSessionMessages(
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
		api.problems.WriteValidationError(writer, request, []InvalidParam{{
			Path: []any{"sessionId"}, Code: "too_small",
			Message: "String must contain at least 1 character(s)",
		}})
		return
	}

	query, issues := parseSessionSequenceQuery(request)
	if len(issues) > 0 {
		api.problems.WriteValidationError(writer, request, issues)
		return
	}

	ownerUserID := int64(0)
	if !principal.IsAdmin {
		ownerUserID = principal.UserID
	}
	owner, ownerErr, isFailure := api.sessionOwnerOrError(
		ctx, sessionID, ownerUserID, principal.IsAdmin, "getSessionMessages")
	if ownerErr != nil {
		api.writeSessionActionError(writer, request, isFailure, "getSessionMessages", ownerErr)
		return
	}

	locator, err := api.resolveAdminSessionRequestLocator(
		ctx, owner.CanonicalIdentity, query, 0, false, owner.UserID)
	if err != nil {
		api.writeSessionActionError(writer, request, false, "getSessionMessages", err)
		return
	}

	// 权限围栏：工件本身不带权限信息，靠所有者键比对（见 internal/session/artifacts_read.go）。
	if !api.artifacts.IsSessionRequestOwnedByKey(
		ctx, locator.SourceSessionID, int(locator.RequestSequence), locator.KeyID,
	) {
		api.writeSessionMessagesMissing(writer, request)
		return
	}
	messages, found, err := api.artifacts.SessionMessages(
		ctx, locator.SourceSessionID, int(locator.RequestSequence))
	if err != nil {
		api.writeSessionActionError(writer, request, true, "getSessionMessages", err)
		return
	}
	if !found {
		api.writeSessionMessagesMissing(writer, request)
		return
	}

	encoded, err := marshalNoEscape(messages)
	if err != nil {
		api.writeSessionActionError(writer, request, true, "getSessionMessages", err)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(encoded)
}

// writeSessionMessagesMissing 作答「Messages 未存储或已过期」的 404（见 handler 注释）。
func (api *sessionAPI) writeSessionMessagesMissing(writer http.ResponseWriter, request *http.Request) {
	api.problems.WriteActionError(writer, request,
		NewActionError("session", "session.not_found", http.StatusNotFound, nil))
}

// handleSessionMessagesExist 复刻 hasSessionMessages（active-sessions.ts:753）。
//
// 与另三条详情端点的关键差别：**几乎一切都是 200**。会话不存在、工件不存在、Redis 故障——
// 全部答 `{"exists": false}`，因为这条端点的用途只是「详情按钮显不显示」。唯一的非 200 是
// 定位器/权限类错误（那两类说明请求本身的参数不对，静默成 false 会把 bug 藏起来）。
func (api *sessionAPI) handleSessionMessagesExist(
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
		api.problems.WriteValidationError(writer, request, []InvalidParam{{
			Path: []any{"sessionId"}, Code: "too_small",
			Message: "String must contain at least 1 character(s)",
		}})
		return
	}

	query, requestID, hasRequestID, issues := sessionQueryWithRequestID(request)
	if len(issues) > 0 {
		api.problems.WriteValidationError(writer, request, issues)
		return
	}

	ownerUserID := int64(0)
	if !principal.IsAdmin {
		ownerUserID = principal.UserID
	}

	var locator *store.AdminSessionRequestLocator
	var owner store.AdminSessionOwner
	found := false
	if hasRequestID {
		// 带 requestId：先定位，再**用定位到的行**判所有权（Node 同序）。
		resolved, err := api.resolveAdminSessionRequestLocator(
			ctx, sessionID, query, requestID, true, ownerUserID)
		if err != nil {
			api.writeSessionActionError(writer, request, false, "hasSessionMessages", err)
			return
		}
		locator = resolved
		resolvedOwner, resolvedFound, err := api.pools.ResolveAdminSessionOwner(
			ctx, resolved.CanonicalSessionID, resolved.UserID)
		if err != nil {
			api.writeSessionExistsFallback(writer, request, "hasSessionMessages", err)
			return
		}
		owner, found = resolvedOwner, resolvedFound
	} else {
		resolvedOwner, resolvedFound, err := api.pools.ResolveAdminSessionOwner(
			ctx, sessionID, ownerUserID)
		if err != nil {
			api.writeSessionExistsFallback(writer, request, "hasSessionMessages", err)
			return
		}
		owner, found = resolvedOwner, resolvedFound
	}

	if !found {
		// Node：会话不存在不是错误，而是「没有详情按钮」。
		adminWriteJSON(writer, http.StatusOK, sessionExistsBody{Exists: false})
		return
	}
	if !principal.IsAdmin && owner.UserID != principal.UserID {
		api.problems.WriteActionError(writer, request,
			NewActionError("session", "session.action_failed", http.StatusForbidden, nil))
		return
	}

	if locator == nil {
		resolved, err := api.resolveAdminSessionRequestLocator(
			ctx, owner.CanonicalIdentity, query, 0, false, owner.UserID)
		if err != nil {
			api.writeSessionActionError(writer, request, false, "hasSessionMessages", err)
			return
		}
		locator = resolved
	}

	if !api.artifacts.IsSessionRequestOwnedByKey(
		ctx, locator.SourceSessionID, int(locator.RequestSequence), locator.KeyID,
	) {
		adminWriteJSON(writer, http.StatusOK, sessionExistsBody{Exists: false})
		return
	}

	// 指定了序号（或 requestId）就只查那一条；否则查整个会话有没有任意 messages。
	if hasRequestID || query.HasRequestSequence {
		_, exists, _ := api.artifacts.SessionMessages(
			ctx, locator.SourceSessionID, int(locator.RequestSequence))
		adminWriteJSON(writer, http.StatusOK, sessionExistsBody{Exists: exists})
		return
	}
	adminWriteJSON(writer, http.StatusOK,
		sessionExistsBody{Exists: api.artifacts.HasAnySessionMessages(ctx, locator.SourceSessionID)})
}

// writeSessionExistsFallback 作答 `{"exists": false}` 并记一条日志（Node 的 catch 分支）。
func (api *sessionAPI) writeSessionExistsFallback(
	writer http.ResponseWriter,
	request *http.Request,
	operation string,
	err error,
) {
	api.logger.Warn("admin_sessions_operation_failed", map[string]any{
		"operation": operation,
		"path":      request.URL.Path,
		"error":     err.Error(),
	})
	adminWriteJSON(writer, http.StatusOK, sessionExistsBody{Exists: false})
}

// sessionExistsBody 是 SessionBooleanResponseSchema（{"exists": bool}）。
type sessionExistsBody struct {
	Exists bool `json:"exists"`
}
