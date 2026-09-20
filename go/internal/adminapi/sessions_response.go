package adminapi

import (
	"net/http"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// GET /api/v1/sessions/{sessionId}/response。
//
// 唯一真源：src/app/api/v1/resources/sessions/handlers.ts:160 的 getSessionResponseBody
// 与 src/actions/session-response.ts:17 的 getSessionResponse。逐条语义：
//
//  1. 参数：`SessionIdParamSchema`（非空）+ `SessionSequenceQuerySchema`
//     （requestSequence 正整数可选、sourceSessionId 非空可选）——两者都复用本包既有解析器，
//     免得同一套 schema 出现第二种读法；
//  2. 会话归属：`aggregateMultipleSessionStats` 判存在性与所有权（管理员看全部，普通用户只看自己）。
//     查不到 → 404 session.not_found（「Session 不存在」）；非本人 → 403 session.action_failed
//     （「无权访问该 Session」）。本包用 sessionOwnerOrError 承担这一步，两处状态码逐条对齐；
//  3. 定位物理行：`resolveSessionRequestLocator`（本包 resolveAdminSessionRequestLocator）。
//     失败（SOURCE_MISMATCH / SELECTOR_INCOMPLETE）→ 400 session.action_failed——
//     Node 的 actionError 按文案子串定档，这两个业务码的文案不含那四个子串，故一律 400；
//  4. **所有权围栏**：`isSessionRequestOwnedByKey` 为假 → 404 session.not_found
//     （Node 文案「响应体已过期（5分钟 TTL）或尚未记录」，含「过期」→ 404）；
//  5. 读响应正文 `getSessionResponse`；null（未记录或已过期）→ 同 404；
//  6. 成功 → `{"response": "<正文字符串>"}`（正文是**裸字符串**：可能是 JSON，也可能是 SSE 文本
//     或带截断标记的头尾窗口，故一律不解码）。
//
// 与详情页的分工：`/sessions/{sessionId}` 把正文塞进快照的某个字段，这条**只**返回正文本身。
// 两者共用同一个工件读与同一条所有权围栏（写进 SessionArtifactReader 的同一个缝隙），
// 以免「读了工件但忘了判权限」在某一条路径上被漏掉。
func RegisterSessionResponseRoute(router *Router, deps Deps) {
	if deps.Store == nil || deps.SessionArtifacts == nil {
		// 缺库或缺工件读面就作答不了：注册等于制造 500（与本包别的 registrar 同一条纪律），
		// 不注册则请求原样回退 Node。
		logSessionResponseUnwired(deps)
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
		pools:     deps.Store,
		artifacts: deps.SessionArtifacts,
		problems:  problems,
		logger:    logger,
	}
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/sessions/{sessionId}/response",
		Access:      AccessRead,
		Module:      "sessions",
		OperationID: "getSessionResponseBody",
		Handler:     http.HandlerFunc(api.handleSessionResponse),
	})
}

func logSessionResponseUnwired(deps Deps) {
	if deps.Logger == nil {
		return
	}
	deps.Logger.Error("admin_session_response_unwired", map[string]any{
		"module": "sessions",
		"path":   "/sessions/{sessionId}/response",
		"reason": "store_or_artifacts_missing",
	})
}

// handleSessionResponse 复刻 getSessionResponseBody（handlers.ts:160）+ getSessionResponse。
func (api *sessionAPI) handleSessionResponse(writer http.ResponseWriter, request *http.Request) {
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
		ctx, sessionID, ownerUserID, principal.IsAdmin, "getSessionResponse")
	if ownerErr != nil {
		api.writeSessionActionError(writer, request, isFailure, "getSessionResponse", ownerErr)
		return
	}

	locator, err := api.resolveAdminSessionRequestLocator(
		ctx, owner.CanonicalIdentity, query, 0, false, owner.UserID)
	if err != nil {
		// 定位失败是「选择器/来源不匹配」，Node 侧无 errorCode ⇒ 400 session.action_failed。
		api.writeSessionActionError(writer, request, false, "getSessionResponse", err)
		return
	}

	// 所有权围栏与正文读**同源**（同一个缝隙）：不拥有就整条按「响应体已过期或未记录」作答，
	// 连读都不读——与详情页的 `redisArtifactsOwned` 同判。
	sequence := int(locator.RequestSequence)
	if !api.artifacts.IsSessionRequestOwnedByKey(ctx, locator.SourceSessionID, sequence, locator.KeyID) {
		api.writeSessionResponseMissing(writer, request)
		return
	}
	body, found := api.artifacts.SessionResponseBody(ctx, locator.SourceSessionID, sequence)
	if !found {
		api.writeSessionResponseMissing(writer, request)
		return
	}

	adminWriteJSON(writer, http.StatusOK, map[string]any{"response": body})
}

// writeSessionResponseMissing 作答「响应体不存在」这一态。
//
// Node 的两条分支（不拥有 / 读到 null）返回同一句话「响应体已过期（5分钟 TTL）或尚未记录」，
// 该文案含「过期」⇒ actionError 定档 404、errorCode 退到 session.not_found。这里显式给码，
// 不移植文案子串判定（与本包 problem.go 的既定裁决一致）。
func (api *sessionAPI) writeSessionResponseMissing(writer http.ResponseWriter, request *http.Request) {
	api.problems.WriteActionError(writer, request,
		NewActionError("session", "session.not_found", http.StatusNotFound, nil))
}
