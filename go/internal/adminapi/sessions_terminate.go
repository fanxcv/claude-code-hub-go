package adminapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 sessions 资源的两条**终止**端点：
//
//	DELETE /sessions/{sessionId}      read  ← terminateActiveSession
//	POST   /sessions:batchTerminate   read  ← terminateActiveSessionsBatch
//
// 唯一真源：src/app/api/v1/resources/sessions/{router,handlers}.ts、
// src/actions/active-sessions.ts 的 terminateActiveSession（:1326-1404）、
// terminateActiveSessionsBatch（:1438-1628）、active-sessions-utils.ts 的
// summarizeTerminateSessionsBatch，以及 src/actions/active-sessions.ts:67-133 的
// terminateResolvedSessionIdentity。
//
// 终止是**五段动作**（见 internal/session/terminate.go 的文件头）：绑定 CAS、响应体 bundle、
// 会话元数据、四组活跃索引，外加前缀亲和会话的**代际围栏失效**。本文件负责的正是 Node 在
// 动作层做的那部分：所有权判定、物理来源枚举、亲和失效的顺序、以及「什么算成功」。
//
// 为什么必须带亲和失效：pfx: identity 的绑定挂在 (scopeTag, fingerprint) 上，物理来源可能为空
// （会话刚建立、还没有账本行）。不先失效代际，在途请求的旧 generation 写入会把绑定复活——
// 管理页显示「已终止」，实际仍在跑。故 Node 的判据是「invalidate 成功即算终止」，
// 本实现逐字复刻（含 sources 为空时 terminated 仍为 true 的分支）。
//
// 错误码映射（Node 的 actionError 是**子串判定**，本项目按裁决改为显式码表）：
//
//	Session 不存在或已过期             → 404 session.not_found
//	无权终止该 Session                 → 403 session.action_failed
//	终止 Session 失败（Redis 不可用…） → 400 session.action_failed

// SessionTerminator 终止一个物理会话（Node 的 SessionManager.terminateSession）。
//
// nil 表示未装配：两条终止路由不注册（回退 Node）——「终止了但绑定还在」比不接管更坏。
// *session.Binder 的结构方法集直接满足本接口，装配处无需适配器。
type SessionTerminator interface {
	// TerminateSession 终止一个物理会话；expectedProviderIDs 非空时是「按供应商范围」的终止。
	TerminateSession(
		ctx context.Context,
		sessionID string,
		expectedProviderIDs []int64,
		expectedKeyID *int64,
	) (bool, error)
	// TerminateObservedSessionForIdentity 终止展示用的会话 identity（Node 的
	// SessionTracker.terminateObservedSession）。
	TerminateObservedSessionForIdentity(ctx context.Context, identity string) (bool, error)
}

// SessionAffinityInvalidator 推进前缀亲和的 identity 代际（Node 的 affinityStore.invalidate）。
//
// nil 表示未装配：两条终止路由**不注册**（回退 Node）。前缀亲和会话的终止以「代际失效成功」
// 为判据，缺了这一层就会留下「列表已终止、绑定可复活」的会话。
type SessionAffinityInvalidator interface {
	Invalidate(ctx context.Context, scopeTag string, identityFingerprint string, fingerprints []string) bool
}

// sessionBatchTerminateBody 是 POST /sessions:batchTerminate 的请求体。
type sessionBatchTerminateBody struct {
	SessionIDs []string `json:"sessionIds"`
}

// sessionBatchTerminateResult 是批量终止的响应体。
//
// 字段顺序与 Node 的对象字面量（active-sessions.ts:1505-1516）逐字一致，便于对拍比对正文。
type sessionBatchTerminateResult struct {
	SuccessCount           int      `json:"successCount"`
	FailedCount            int      `json:"failedCount"`
	AllowedFailedCount     int      `json:"allowedFailedCount"`
	UnauthorizedCount      int      `json:"unauthorizedCount"`
	MissingCount           int      `json:"missingCount"`
	UnauthorizedSessionIDs []string `json:"unauthorizedSessionIds"`
	MissingSessionIDs      []string `json:"missingSessionIds"`
	RequestedCount         int      `json:"requestedCount"`
	ProcessedCount         int      `json:"processedCount"`
}

// handleTerminateSession 复刻 terminateActiveSession。
func (api *sessionAPI) handleTerminateSession(writer http.ResponseWriter, request *http.Request) {
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

	// 1. 所有权判定。非管理员带 owner 条件：别人的会话查不到，与 Node 一样作答 404。
	ownerUserID := int64(0)
	if !principal.IsAdmin {
		ownerUserID = principal.UserID
	}
	owner, found, err := api.pools.ResolveAdminSessionOwner(ctx, sessionID, ownerUserID)
	if err != nil {
		api.writeSessionFailure(writer, request, "terminateSession", err)
		return
	}
	if !found {
		api.problems.WriteActionError(writer, request,
			NewActionError("session", "session.not_found", http.StatusNotFound, nil))
		return
	}
	if !principal.IsAdmin && owner.UserID != principal.UserID {
		// Node 的显式二次判定（SQL 侧过滤后不可达，保留同判以备过滤策略变化）。
		api.problems.WriteActionError(writer, request,
			NewActionError("session", "session.action_failed", http.StatusForbidden, nil))
		return
	}

	// 2. 解析 identity 并终止（owner 用**会话归属**，不是调用者）。
	outcome, err := api.terminateSessionIdentity(ctx, owner.CanonicalIdentity, owner.UserID)
	if err != nil {
		api.writeSessionFailure(writer, request, "terminateSession", err)
		return
	}
	if !outcome.Terminated {
		// Node 的失败文案不含「不存在」「无权」，故是 400 而不是 404。
		api.problems.WriteActionError(writer, request,
			NewActionError("session", "session.action_failed", http.StatusBadRequest, nil))
		return
	}

	writer.WriteHeader(http.StatusNoContent)
}

// sessionTerminationOutcome 是终止一个 identity 的结果（Node 的同名返回对象）。
type sessionTerminationOutcome struct {
	Terminated       bool
	SourceSessionIDs []string
}

// terminateSessionIdentity 复刻 terminateResolvedSessionIdentity（active-sessions.ts:67-133）。
//
// 两条分支的分工（顺序不能换）：
//   - 非前缀亲和：枚举物理来源 → 逐个终止 → **任一个成功**即算终止 → 成功才摘观测集合。
//   - 前缀亲和：先失效代际（失败即整体失败、不动任何物理会话）→ 再逐个终止（结果**忽略**，
//     Node 只记 debug 日志）→ 摘观测集合 → 恒判终止。
//
// sources 为空时 Node 的行为：非亲和分支 terminated=false；亲和分支**仍然 true**（代际已推进，
// 旧绑定写不回来）。两条都保留——这是「终止一个还没有账本行的会话」的正常路径。
func (api *sessionAPI) terminateSessionIdentity(
	ctx context.Context,
	identity string,
	ownerUserID int64,
) (sessionTerminationOutcome, error) {
	resolution, resolved, err := api.pools.ResolveAdminSessionIdentity(ctx, identity, ownerUserID)
	if err != nil {
		return sessionTerminationOutcome{}, err
	}
	resolvedIdentity := identity
	if resolved {
		resolvedIdentity = resolution.Identity
	}

	isPrefixAffinity := resolved &&
		resolution.IdentityKind != nil && *resolution.IdentityKind == "prefix_affinity" &&
		resolution.ScopeTag != nil && *resolution.ScopeTag != "" &&
		resolution.Fingerprint != nil && *resolution.Fingerprint != ""

	if isPrefixAffinity {
		// 调用方已知的绑定键 = 去重后的 [fingerprint, ...fingerprints]（Node 的 Set 顺序）。
		if !api.affinity.Invalidate(ctx, *resolution.ScopeTag, *resolution.Fingerprint,
			appendUniqueStrings([]string{*resolution.Fingerprint}, resolution.Fingerprints)) {
			return sessionTerminationOutcome{Terminated: false, SourceSessionIDs: []string{}}, nil
		}
		sources, sourceErr := api.pools.ListPhysicalSessionSourcesForIdentity(
			ctx, resolvedIdentity, ownerUserID)
		if sourceErr != nil {
			return sessionTerminationOutcome{}, sourceErr
		}
		sourceIDs := make([]string, 0, len(sources))
		for _, source := range sources {
			// 结果忽略：Node 只记 debug（物理会话可能已被后续请求取代）。
			_, _ = api.sessions.TerminateSession(
				ctx, source.SessionID, providerIDsOrNil(source.ProviderIDs), keyIDPointer(source.KeyID))
			sourceIDs = append(sourceIDs, source.SessionID)
		}
		_, _ = api.sessions.TerminateObservedSessionForIdentity(ctx, identity)
		return sessionTerminationOutcome{Terminated: true, SourceSessionIDs: sourceIDs}, nil
	}

	sources, sourceErr := api.pools.ListPhysicalSessionSourcesForIdentity(
		ctx, resolvedIdentity, ownerUserID)
	if sourceErr != nil {
		return sessionTerminationOutcome{}, sourceErr
	}
	terminated := false
	sourceIDs := make([]string, 0, len(sources))
	for _, source := range sources {
		ok, termErr := api.sessions.TerminateSession(
			ctx, source.SessionID, providerIDsOrNil(source.ProviderIDs), keyIDPointer(source.KeyID))
		if termErr != nil {
			// Node 的 Promise.all 在这里会把异常冒泡成整体失败（action 层 catch 后回 400）。
			return sessionTerminationOutcome{}, termErr
		}
		if ok {
			terminated = true
		}
		sourceIDs = append(sourceIDs, source.SessionID)
	}
	if terminated {
		_, _ = api.sessions.TerminateObservedSessionForIdentity(ctx, identity)
	}
	return sessionTerminationOutcome{Terminated: terminated, SourceSessionIDs: sourceIDs}, nil
}

// handleBatchTerminateSessions 复刻 terminateActiveSessionsBatch。
func (api *sessionAPI) handleBatchTerminateSessions(
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

	body, issues := parseSessionBatchBody(request)
	if len(issues) > 0 {
		api.problems.WriteValidationError(writer, request, issues)
		return
	}

	// new Set(sessionIds)：去重且保持首次出现顺序。
	uniqueSessionIDs := appendUniqueStrings(body.SessionIDs, nil)
	if len(uniqueSessionIDs) == 0 {
		adminWriteJSON(writer, http.StatusOK, sessionBatchTerminateResult{
			UnauthorizedSessionIDs: []string{},
			MissingSessionIDs:      []string{},
		})
		return
	}

	// 聚合时**带** owner 条件（与列表页相反）：非管理员看不到别人的会话，于是它们落进 missing。
	ownerUserID := int64(0)
	if !principal.IsAdmin {
		ownerUserID = principal.UserID
	}
	summaries, err := api.pools.AggregateAdminSessionStats(ctx, uniqueSessionIDs, ownerUserID)
	if err != nil {
		api.writeSessionFailure(writer, request, "batchTerminateSessions", err)
		return
	}

	claim := summarizeTerminateSessionsBatch(uniqueSessionIDs, summaries, principal)
	result := sessionBatchTerminateResult{
		UnauthorizedSessionIDs: claim.UnauthorizedSessionIDs,
		MissingSessionIDs:      claim.MissingSessionIDs,
		UnauthorizedCount:      len(claim.UnauthorizedSessionIDs),
		MissingCount:           len(claim.MissingSessionIDs),
		RequestedCount:         len(uniqueSessionIDs),
	}
	if len(claim.AllowedSessionIDs) == 0 {
		result.FailedCount = result.UnauthorizedCount + result.MissingCount
		adminWriteJSON(writer, http.StatusOK, result)
		return
	}

	ownerByCanonicalID := make(map[string]int64, len(claim.AllowedSessionIDs))
	for _, summary := range summaries {
		ownerByCanonicalID[summary.SessionID] = summary.UserID
	}

	successCount := 0
	for _, identity := range claim.AllowedSessionIDs {
		owner, known := ownerByCanonicalID[identity]
		if !known {
			continue
		}
		outcome, termErr := api.terminateSessionIdentity(ctx, identity, owner)
		if termErr != nil {
			// Node 的 allSettled：单条失败只记 warn，不阻断其余条目，也不计入 successCount。
			api.logger.Warn("admin_sessions_batch_terminate_item_failed", map[string]any{
				"identity": identity,
				"error":    termErr.Error(),
			})
			continue
		}
		if outcome.Terminated {
			successCount++
		}
	}

	processedCount := len(claim.AllowedSessionIDs)
	allowedFailedCount := processedCount - successCount
	if allowedFailedCount < 0 {
		allowedFailedCount = 0
	}
	result.SuccessCount = successCount
	result.ProcessedCount = processedCount
	result.AllowedFailedCount = allowedFailedCount
	result.FailedCount = allowedFailedCount + result.UnauthorizedCount + result.MissingCount
	adminWriteJSON(writer, http.StatusOK, result)
}

// sessionBatchClaim 是 summarizeTerminateSessionsBatch 的结果。
type sessionBatchClaim struct {
	AllowedSessionIDs      []string
	UnauthorizedSessionIDs []string
	MissingSessionIDs      []string
}

// summarizeTerminateSessionsBatch 复刻 active-sessions-utils.ts 的 summarizeTerminateSessionsBatch。
//
// requestedSessionIds 是**归并到该规范 identity 的入参集合**（空时退化成规范 identity 本身）；
// claimed 取并集，于是 missing = 请求里没被任何聚合结果认领的那些 id。
func summarizeTerminateSessionsBatch(
	uniqueRequestedIDs []string,
	summaries []store.AdminSessionSummary,
	principal Principal,
) sessionBatchClaim {
	claimed := make(map[string]struct{}, len(uniqueRequestedIDs))
	allowed := make([]string, 0, len(summaries))
	unauthorized := make([]string, 0, len(summaries))

	for _, summary := range summaries {
		requested := summary.RequestedSessionIDs
		if len(requested) == 0 {
			requested = []string{summary.SessionID}
		}
		for _, requestedID := range requested {
			claimed[requestedID] = struct{}{}
		}
		if principal.IsAdmin || summary.UserID == principal.UserID {
			allowed = append(allowed, summary.SessionID)
			continue
		}
		unauthorized = append(unauthorized, summary.SessionID)
	}

	missing := make([]string, 0, len(uniqueRequestedIDs))
	for _, requestedID := range uniqueRequestedIDs {
		if _, found := claimed[requestedID]; !found {
			missing = append(missing, requestedID)
		}
	}

	return sessionBatchClaim{
		AllowedSessionIDs:      allowed,
		UnauthorizedSessionIDs: unauthorized,
		MissingSessionIDs:      missing,
	}
}

// parseSessionBatchBody 解析并校验 POST /sessions:batchTerminate 的正文。
//
// 校验与 BatchTerminateSessionsSchema 同：**严格对象**（多键即 unrecognized_keys）、
// sessionIds 为 1..200 个非空字符串。复用 keys_* 两个通用助手（严格对象与未知键判定），
// 不另写一份——同一条纪律在 keys/providers 的写路径上已经实现过一次。
func parseSessionBatchBody(request *http.Request) (sessionBatchTerminateBody, []InvalidParam) {
	raw, err := keysReadBody(request)
	if err != nil {
		return sessionBatchTerminateBody{}, []InvalidParam{keysInvalidType("sessionIds")}
	}
	if unknown := keysRejectUnknown(raw, "sessionIds"); len(unknown) > 0 {
		return sessionBatchTerminateBody{}, unknown
	}

	encoded, present := raw["sessionIds"]
	if !present {
		return sessionBatchTerminateBody{}, []InvalidParam{keysInvalidType("sessionIds")}
	}
	var list []string
	if unmarshalErr := json.Unmarshal(encoded, &list); unmarshalErr != nil {
		return sessionBatchTerminateBody{}, []InvalidParam{keysInvalidType("sessionIds")}
	}
	if len(list) == 0 {
		return sessionBatchTerminateBody{}, []InvalidParam{keysInvalid(
			"sessionIds", "too_small", "Array must contain at least 1 element(s)")}
	}
	if len(list) > 200 {
		return sessionBatchTerminateBody{}, []InvalidParam{keysInvalid(
			"sessionIds", "too_big", "Array must contain at most 200 element(s)")}
	}
	for index, item := range list {
		if item == "" {
			return sessionBatchTerminateBody{}, []InvalidParam{{
				Path: []any{"sessionIds", index}, Code: "too_small",
				Message: "String must contain at least 1 character(s)",
			}}
		}
	}
	return sessionBatchTerminateBody{SessionIDs: list}, nil
}

// providerIDsOrNil 复刻 `providerIds.length > 0 ? providerIds : undefined`。
func providerIDsOrNil(providerIDs []int64) []int64 {
	if len(providerIDs) == 0 {
		return nil
	}
	return providerIDs
}

// keyIDPointer 复刻按 key 范围终止时传的 owner 期望值（Node 传的是 number 或 undefined）。
//
// 0 表示该来源没有 key（理论上不该出现，SQL 侧 INNER JOIN keys 已保证），此时不传期望值——
// 传 0 会让 Binder 的预检一律失败。
func keyIDPointer(keyID int64) *int64 {
	if keyID <= 0 {
		return nil
	}
	return &keyID
}
