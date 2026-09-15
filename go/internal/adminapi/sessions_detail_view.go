package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/session"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 `GET /sessions/{sessionId}` 的**详情视图**：把九类事实合成一个响应体。
//
// 唯一真源：src/actions/active-sessions.ts:getSessionDetails（:874-1231）与
// src/lib/session-detail-snapshots.ts / src/lib/utils/special-settings.ts 的归一化与合并器。
//
// 九类事实与各自来源（缺任何一类都会让详情页静默变成半个页）：
//
//	requestBody       Redis 工件（数据面写）      ← legacy，快照缺失时的回退
//	messages          Redis 工件（数据面写）      ← 同上
//	response          Redis 工件（数据面写，有界窗口）
//	requestHeaders    Redis 工件（数据面写）
//	responseHeaders   Redis 工件（数据面写）
//	upstreamReq/ResMeta Redis 工件（数据面写）
//	相位快照 ×4        Redis 工件（数据面写）      ← **默认视图**，优先于 legacy
//	sessionStats      账本聚合（store.AggregateAdminSessionStats）
//	specialSettings   账本审计行 + Redis 工件，经 unionSetting 合并
//
// 视图语义只有一条，但它是整个端点的组织原则：**相位快照优先，legacy 键在快照缺失时补位**。
// Node 的 DEFAULT_SESSION_DETAIL_VIEW_MODE 是 "after"，UI 默认展示 after 视图，而 after 视图
// 在只有 legacy 数据时由 buildLegacyCompatibilitySnapshots 拼出来——两个视图各看各的，
// 不是「合并成一个」。

// defaultSessionDetailViewMode 复刻 DEFAULT_SESSION_DETAIL_VIEW_MODE（types/session.ts:89）。
const defaultSessionDetailViewMode = "after"

// sessionDetailRequestMetaBody 是请求侧 meta（三个字段都可为 null）。
type sessionDetailRequestMetaBody struct {
	ClientURL   any `json:"clientUrl"`
	UpstreamURL any `json:"upstreamUrl"`
	Method      any `json:"method"`
}

// sessionDetailResponseMetaBody 是响应侧 meta。
type sessionDetailResponseMetaBody struct {
	UpstreamURL any `json:"upstreamUrl"`
	StatusCode  any `json:"statusCode"`
}

// sessionDetailRequestSnapshotBody 是一份请求相位快照。
type sessionDetailRequestSnapshotBody struct {
	Body     any                          `json:"body"`
	Messages any                          `json:"messages"`
	Headers  any                          `json:"headers"`
	Meta     sessionDetailRequestMetaBody `json:"meta"`
}

// sessionDetailResponseSnapshotBody 是一份响应相位快照。
type sessionDetailResponseSnapshotBody struct {
	Body    any                           `json:"body"`
	Headers any                           `json:"headers"`
	Meta    sessionDetailResponseMetaBody `json:"meta"`
}

// sessionDetailSnapshotsBody 是四个相位的容器（键名与 Node 一致）。
type sessionDetailSnapshotsBody struct {
	DefaultView string `json:"defaultView"`
	Request     struct {
		Before *sessionDetailRequestSnapshotBody `json:"before"`
		After  *sessionDetailRequestSnapshotBody `json:"after"`
	} `json:"request"`
	Response struct {
		Before *sessionDetailResponseSnapshotBody `json:"before"`
		After  *sessionDetailResponseSnapshotBody `json:"after"`
	} `json:"response"`
}

// sessionDetailNavigatorBody 是一条相邻请求（Node 的 SessionRequestNavigationTarget）。
type sessionDetailNavigatorBody struct {
	RequestID       int64  `json:"requestId"`
	SourceSessionID string `json:"sourceSessionId"`
	RequestSequence int64  `json:"requestSequence"`
}

// sessionDetailProviderBody 是 sessionStats.providers 的一项。
type sessionDetailProviderBody struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// sessionDetailStatsBody 是 sessionStats：账本聚合项，键名与 Node 的 aggregate 返回逐字一致。
type sessionDetailStatsBody struct {
	SessionID                string                      `json:"sessionId"`
	RequestedSessionIDs      []string                    `json:"requestedSessionIds"`
	SessionIdentityKind      string                      `json:"sessionIdentityKind"`
	SessionFingerprint       any                         `json:"sessionFingerprint"`
	RequestCount             int64                       `json:"requestCount"`
	TotalCostUSD             string                      `json:"totalCostUsd"`
	TotalInputTokens         int64                       `json:"totalInputTokens"`
	TotalOutputTokens        int64                       `json:"totalOutputTokens"`
	TotalCacheCreationTokens int64                       `json:"totalCacheCreationTokens"`
	TotalCacheReadTokens     int64                       `json:"totalCacheReadTokens"`
	TotalDurationMS          int64                       `json:"totalDurationMs"`
	FirstRequestAt           any                         `json:"firstRequestAt"`
	LastRequestAt            any                         `json:"lastRequestAt"`
	Providers                []sessionDetailProviderBody `json:"providers"`
	Models                   []string                    `json:"models"`
	UserName                 string                      `json:"userName"`
	UserID                   int64                       `json:"userId"`
	KeyName                  string                      `json:"keyName"`
	KeyID                    int64                       `json:"keyId"`
	UserAgent                any                         `json:"userAgent"`
	APIType                  any                         `json:"apiType"`
	CacheTTLApplied          any                         `json:"cacheTtlApplied"`
}

// sessionDetailBody 是详情端点的响应体。
//
// 字段顺序照 Node 的返回对象（便于对拍直接比对正文），19 个键一个不少。
type sessionDetailBody struct {
	RequestBody            any                           `json:"requestBody"`
	Messages               any                           `json:"messages"`
	Response               any                           `json:"response"`
	RequestHeaders         any                           `json:"requestHeaders"`
	ResponseHeaders        any                           `json:"responseHeaders"`
	RequestMeta            sessionDetailRequestMetaBody  `json:"requestMeta"`
	ResponseMeta           sessionDetailResponseMetaBody `json:"responseMeta"`
	Snapshots              sessionDetailSnapshotsBody    `json:"snapshots"`
	SpecialSettings        any                           `json:"specialSettings"`
	SessionStats           *sessionDetailStatsBody       `json:"sessionStats"`
	CanonicalSessionID     string                        `json:"canonicalSessionId"`
	CurrentSourceSessionID string                        `json:"currentSourceSessionId"`
	CurrentSequence        any                           `json:"currentSequence"`
	PrevRequest            *sessionDetailNavigatorBody   `json:"prevRequest"`
	NextRequest            *sessionDetailNavigatorBody   `json:"nextRequest"`
	PrevSequence           any                           `json:"prevSequence"`
	NextSequence           any                           `json:"nextSequence"`
}

// handleSessionDetail 复刻 getSessionDetails（active-sessions.ts:874-1231）。
//
// 步骤次序与 Node 逐条对应（次序本身是语义：先鉴权、再定位、再判所有者、最后才读工件）：
//
//  1. 未登录 → 401；
//  2. 带 requestId 时用 requestId 定位，并用**定位到的行**的 canonical/user 取聚合；否则按
//     sessionId 直接取（管理员不带 owner 条件）；
//  3. 取不到聚合 → 404 session.not_found（文案「Session 不存在」）；
//  4. 非管理员且聚合的 userId 与调用者不符 → 403 session.action_failed（文案「无权访问该 Session」）；
//  5. 用聚合给出的 canonical identity 定位本条请求（requestId 分支已定位过则复用）；
//  6. 相邻请求导航（仅当定位出序号）；
//  7. 所有权围栏 → 决定是否读 Redis 工件（不拥有则全按「没有工件」处理）；
//  8. 合成快照与 legacy 回退，合并 specialSettings，作答。
func (api *sessionAPI) handleSessionDetail(
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
	var stats *store.AdminSessionSummary
	if hasRequestID {
		resolved, err := api.resolveAdminSessionRequestLocator(
			ctx, sessionID, query, requestID, true, ownerUserID)
		if err != nil {
			api.writeSessionActionError(writer, request, false, "getSessionDetails", err)
			return
		}
		locator = resolved
		loaded, err := api.loadCanonicalSessionStats(ctx, resolved.CanonicalSessionID, resolved.UserID)
		if err != nil {
			api.writeSessionActionError(writer, request, true, "getSessionDetails", err)
			return
		}
		stats = loaded
	} else {
		loaded, err := api.loadCanonicalSessionStats(ctx, sessionID, ownerUserID)
		if err != nil {
			api.writeSessionActionError(writer, request, true, "getSessionDetails", err)
			return
		}
		stats = loaded
	}
	if stats == nil {
		api.problems.WriteActionError(writer, request,
			NewActionError("session", "session.not_found", http.StatusNotFound, nil))
		return
	}
	if !principal.IsAdmin && stats.UserID != principal.UserID {
		api.logger.Warn("admin_sessions_detail_denied", map[string]any{
			"sessionId": sessionID,
			"userId":    principal.UserID,
			"ownerId":   stats.UserID,
		})
		api.problems.WriteActionError(writer, request,
			NewActionError("session", "session.action_failed", http.StatusForbidden, nil))
		return
	}

	canonicalSessionID := stats.SessionID
	if locator == nil {
		resolved, err := api.resolveAdminSessionRequestLocator(
			ctx, canonicalSessionID, query, 0, false, stats.UserID)
		if err != nil {
			api.writeSessionActionError(writer, request, false, "getSessionDetails", err)
			return
		}
		locator = resolved
	}
	sourceSessionID := locator.SourceSessionID
	sequence := locator.RequestSequence

	// 相邻请求：Node 在「定位出序号」时才查（`effectiveSequence == null` 时直接给两个 null）。
	adjacent, err := api.pools.FindAdminAdjacentSessionRequests(
		ctx, canonicalSessionID, locator.RequestID, locator.UserID)
	if err != nil {
		api.writeSessionActionError(writer, request, true, "getSessionDetails", err)
		return
	}
	audit, err := api.pools.FindAdminMessageRequestAudit(ctx, locator.RequestID, locator.UserID)
	if err != nil {
		api.writeSessionActionError(writer, request, true, "getSessionDetails", err)
		return
	}

	// 所有权围栏：不拥有则整批 Redis 工件按「没有」处理（Node 的 redisArtifactsOwned 三元）。
	owned := api.artifacts.IsSessionRequestOwnedByKey(
		ctx, sourceSessionID, int(sequence), locator.KeyID)

	body := api.buildSessionDetail(ctx, sessionDetailInputs{
		owned:              owned,
		sourceSessionID:    sourceSessionID,
		sequence:           int(sequence),
		canonicalSessionID: canonicalSessionID,
		stats:              stats,
		adjacent:           adjacent,
		audit:              audit,
		currentSequence:    sequence,
	})
	writeSessionsJSON(writer, http.StatusOK, body)
}

// loadCanonicalSessionStats 复刻 loadCanonicalSessionStats：单 identity 的账本聚合。
//
// 聚合项为「按入参 identity 归并后的一行」；入参存在但归并不出任何行时返回 nil
// （Node 的 `sessionStats ?? null`），这是 404 的判据。
func (api *sessionAPI) loadCanonicalSessionStats(
	ctx context.Context, sessionID string, ownerUserID int64,
) (*store.AdminSessionSummary, error) {
	summaries, err := api.pools.AggregateAdminSessionStats(ctx, []string{sessionID}, ownerUserID)
	if err != nil {
		return nil, err
	}
	if len(summaries) == 0 {
		return nil, nil
	}
	return &summaries[0], nil
}

// sessionDetailInputs 是 buildSessionDetail 的输入（已定位、已鉴权的事实）。
type sessionDetailInputs struct {
	owned              bool
	sourceSessionID    string
	sequence           int
	canonicalSessionID string
	stats              *store.AdminSessionSummary
	adjacent           store.AdminSessionAdjacent
	audit              *store.AdminMessageRequestAudit
	currentSequence    int64
}

// buildSessionDetail 合成详情响应体（Node 第 6 步之后的全部逻辑）。
func (api *sessionAPI) buildSessionDetail(
	ctx context.Context, in sessionDetailInputs,
) sessionDetailBody {
	// 6. 先读相位快照与轻量 metadata；大字段仅在快照缺失时读 legacy 键。
	requestBefore := api.readPhase(ctx, in, "request", "before")
	requestAfter := api.readPhase(ctx, in, "request", "after")
	responseBefore := api.readPhase(ctx, in, "response", "before")
	responseAfter := api.readPhase(ctx, in, "response", "after")

	requestHeaders := api.readHeaders(ctx, in, sessionRequestHeaders)
	responseHeaders := api.readHeaders(ctx, in, sessionResponseHeaders)
	clientMeta := api.readClientRequestMeta(ctx, in)
	upstreamReqMeta := api.readUpstreamRequestMeta(ctx, in)
	upstreamResMeta := api.readUpstreamResponseMeta(ctx, in)
	redisSettings := api.readSpecialSettings(ctx, in)

	snapshots := sessionDetailSnapshotsBody{DefaultView: defaultSessionDetailViewMode}
	snapshots.Request.Before = normalizeRequestSnapshot(requestBefore, "before")
	snapshots.Request.After = normalizeRequestSnapshot(requestAfter, "after")
	snapshots.Response.Before = normalizeResponseSnapshot(responseBefore)
	snapshots.Response.After = normalizeResponseSnapshot(responseAfter)

	// 视图优先级：after 优先、before 兜底；messages 相反（before 是客户端原文，after 是从改写后
	// 正文里提取的）——这个不对称是 Node 的原文，改动即语义漂移。
	snapshotRequestBody := snapshotBodyOf(snapshots.Request.After, snapshots.Request.Before)
	snapshotMessages := any(nil)
	if snapshots.Request.Before != nil && snapshots.Request.Before.Messages != nil {
		snapshotMessages = snapshots.Request.Before.Messages
	} else if snapshots.Request.After != nil {
		snapshotMessages = snapshots.Request.After.Messages
	}
	snapshotResponse := snapshotResponseBodyOf(snapshots.Response.After, snapshots.Response.Before)

	legacyRequestBody := api.readLegacyRequestBody(ctx, in, snapshotRequestBody == nil)
	legacyMessages := api.readLegacyMessages(ctx, in, snapshotMessages == nil)
	legacyResponse := api.readLegacyResponse(ctx, in, snapshotResponse == nil)

	messages := snapshotMessages
	if messages == nil {
		messages = parseJSONStringOrNull(legacyMessages)
	}
	requestBody := snapshotRequestBody
	if requestBody == nil {
		requestBody = parseJSONStringOrNull(legacyRequestBody)
	}
	var response any
	if snapshotResponse != nil {
		response = *snapshotResponse
	} else if legacyResponse != nil {
		response = *legacyResponse
	}

	// requestMeta 的两个来源优先次序与 Node 一致：clientReqMeta 优先，upstreamReqMeta 兜 method。
	requestMeta := sessionDetailRequestMetaBody{}
	if clientMeta != nil {
		requestMeta.ClientURL = clientMeta.URL
		requestMeta.Method = clientMeta.Method
	}
	if upstreamReqMeta != nil {
		requestMeta.UpstreamURL = upstreamReqMeta.URL
		if requestMeta.Method == nil {
			requestMeta.Method = upstreamReqMeta.Method
		}
	}
	responseMeta := sessionDetailResponseMetaBody{}
	if upstreamResMeta != nil {
		responseMeta.UpstreamURL = upstreamResMeta.URL
		responseMeta.StatusCode = upstreamResMeta.StatusCode
	} else if upstreamReqMeta != nil {
		responseMeta.UpstreamURL = upstreamReqMeta.URL
	}

	// legacy 兼容层：把平铺字段拼成快照，只在**快照字段缺失**处补位（不覆盖）。
	legacy := buildLegacyCompatibilitySnapshots(legacySnapshotInputs{
		requestBody:     requestBody,
		messages:        messages,
		response:        response,
		requestHeaders:  requestHeaders,
		responseHeaders: responseHeaders,
		requestMeta:     requestMeta,
		responseMeta:    responseMeta,
	})
	snapshots.Request.After = mergeLegacyRequestAfterSnapshot(snapshots.Request.After, legacy.Request.After)
	snapshots.Response.After = mergeLegacyResponseAfterSnapshot(snapshots.Response.After, legacy.Response.After)

	// Redis 侧设置项（本波恒空）在前，审计行的在后：与 Node 的 `[...redis, ...audit]` 同序。
	existing := append([]map[string]any{}, redisSettings...)
	existing = append(existing, auditSettings(in.audit)...)
	specialSettings := unionSetting(
		existing,
		blockedByOf(in.audit), blockedReasonOf(in.audit), statusCodeOf(in.audit),
		cacheTTLOf(in.audit),
	)

	body := sessionDetailBody{
		RequestBody:            requestBody,
		Messages:               messages,
		Response:               response,
		RequestHeaders:         headerRecordOrNil(requestHeaders),
		ResponseHeaders:        headerRecordOrNil(responseHeaders),
		RequestMeta:            requestMeta,
		ResponseMeta:           responseMeta,
		Snapshots:              snapshots,
		SpecialSettings:        specialSettings,
		SessionStats:           sessionStatsBody(in.stats),
		CanonicalSessionID:     in.canonicalSessionID,
		CurrentSourceSessionID: in.sourceSessionID,
		CurrentSequence:        int64OrNil(in.currentSequence),
		PrevRequest:            navigatorBody(in.adjacent.Prev),
		NextRequest:            navigatorBody(in.adjacent.Next),
	}
	if in.adjacent.Prev != nil {
		body.PrevSequence = in.adjacent.Prev.RequestSequence
	}
	if in.adjacent.Next != nil {
		body.NextSequence = in.adjacent.Next.RequestSequence
	}
	return body
}

// readPhase 读一份相位快照（不拥有工件时按「没有」处理）。
func (api *sessionAPI) readPhase(
	ctx context.Context, in sessionDetailInputs, kind, phase string,
) *session.SessionDetailSnapshotRead {
	if !in.owned {
		return nil
	}
	return api.artifacts.ReadSessionPhaseSnapshot(ctx, in.sourceSessionID, in.sequence, kind, phase)
}

// artifactKind 区分两类头工件（请求头的读取语义与响应头完全相同，只是键不同）。
type artifactKind int

const (
	sessionRequestHeaders artifactKind = iota
	sessionResponseHeaders
)

// readHeaders 读一类头工件（不拥有时 nil）。
func (api *sessionAPI) readHeaders(
	ctx context.Context, in sessionDetailInputs, kind artifactKind,
) map[string]string {
	if !in.owned {
		return nil
	}
	if kind == sessionRequestHeaders {
		return api.artifacts.SessionRequestHeaders(ctx, in.sourceSessionID, in.sequence)
	}
	return api.artifacts.SessionResponseHeaders(ctx, in.sourceSessionID, in.sequence)
}

// readClientRequestMeta 读客户端请求元信息（数据面已写：见 session.StoreSessionClientRequestMeta）。
func (api *sessionAPI) readClientRequestMeta(
	ctx context.Context, in sessionDetailInputs,
) *session.SessionUpstreamRequestMetaRead {
	return api.artifacts.ReadSessionClientRequestMeta(ctx, in.sourceSessionID, in.sequence)
}

// readUpstreamRequestMeta 读上游请求元信息（不拥有时 nil）。
func (api *sessionAPI) readUpstreamRequestMeta(
	ctx context.Context, in sessionDetailInputs,
) *session.SessionUpstreamRequestMetaRead {
	if !in.owned {
		return nil
	}
	return api.artifacts.ReadSessionUpstreamRequestMeta(ctx, in.sourceSessionID, in.sequence)
}

// readUpstreamResponseMeta 读上游响应元信息（不拥有时 nil）。
func (api *sessionAPI) readUpstreamResponseMeta(
	ctx context.Context, in sessionDetailInputs,
) *session.SessionUpstreamResponseMetaRead {
	if !in.owned {
		return nil
	}
	return api.artifacts.ReadSessionUpstreamResponseMeta(ctx, in.sourceSessionID, in.sequence)
}

// readSpecialSettings 读 Redis 侧的 specialSettings 工件。
//
// **刻意恒为 nil**：Go 数据面不写这条工件（账本行里的 special_settings 已覆盖绝大多数场景，
// 见 usage_logs 的 unionSetting）。详情页的 specialSettings 因此只由审计行派生——
// 登记差异见报告。
func (api *sessionAPI) readSpecialSettings(
	ctx context.Context, in sessionDetailInputs,
) []map[string]any {
	return nil
}

// readLegacyRequestBody / readLegacyMessages / readLegacyResponse 读三条 legacy 键。
//
// 只在**对应快照字段缺失**时读（Node 的三元判定）：快照有值还去读 legacy 会让详情页在一次
// 响应里带上两份正文，而 UI 只展示一份。
func (api *sessionAPI) readLegacyRequestBody(
	ctx context.Context, in sessionDetailInputs, needed bool,
) any {
	if !needed || !in.owned {
		return nil
	}
	value, found := api.artifacts.SessionRequestBody(ctx, in.sourceSessionID, in.sequence)
	if !found {
		return nil
	}
	return value
}

func (api *sessionAPI) readLegacyMessages(
	ctx context.Context, in sessionDetailInputs, needed bool,
) any {
	if !needed || !in.owned {
		return nil
	}
	value, found, err := api.artifacts.SessionMessages(ctx, in.sourceSessionID, in.sequence)
	if err != nil || !found {
		return nil
	}
	return value
}

func (api *sessionAPI) readLegacyResponse(
	ctx context.Context, in sessionDetailInputs, needed bool,
) *string {
	if !needed || !in.owned {
		return nil
	}
	value, found := api.artifacts.SessionResponseBody(ctx, in.sourceSessionID, in.sequence)
	if !found {
		return nil
	}
	// 节点语义：legacy response 是**字符串**（裸正文），快照 response 也是字符串。同类型。
	return &value
}

// sessionStatsBody 把聚合项翻成 Node 的键名与日期格式。
func sessionStatsBody(summary *store.AdminSessionSummary) *sessionDetailStatsBody {
	if summary == nil {
		return nil
	}
	providers := make([]sessionDetailProviderBody, 0, len(summary.Providers))
	for _, provider := range summary.Providers {
		providers = append(providers, sessionDetailProviderBody{ID: provider.ID, Name: provider.Name})
	}
	models := summary.Models
	if models == nil {
		models = []string{}
	}
	return &sessionDetailStatsBody{
		SessionID:                summary.SessionID,
		RequestedSessionIDs:      summary.RequestedSessionIDs,
		SessionIdentityKind:      summary.SessionIdentityKind,
		SessionFingerprint:       stringOrNil(summary.SessionFingerprint),
		RequestCount:             summary.RequestCount,
		TotalCostUSD:             summary.TotalCostUSD,
		TotalInputTokens:         summary.TotalInputTokens,
		TotalOutputTokens:        summary.TotalOutputTokens,
		TotalCacheCreationTokens: summary.TotalCacheCreationTokens,
		TotalCacheReadTokens:     summary.TotalCacheReadTokens,
		TotalDurationMS:          summary.TotalDurationMS,
		FirstRequestAt:           isoTimeOrNil(summary.FirstRequestAt),
		LastRequestAt:            isoTimeOrNil(summary.LastRequestAt),
		Providers:                providers,
		Models:                   models,
		UserName:                 summary.UserName,
		UserID:                   summary.UserID,
		KeyName:                  summary.KeyName,
		KeyID:                    summary.KeyID,
		UserAgent:                stringOrNil(summary.UserAgent),
		APIType:                  stringOrNil(summary.APIType),
		CacheTTLApplied:          stringOrNil(summary.CacheTTLApplied),
	}
}

// isoTimeOrNil 把时间格式化成 **JS Date 的 JSON 形态**（毫秒精度 + Z 后缀）。
//
// 为什么不直接用 Go 的 time.Time 序列化：Go 给 RFC3339Nano（无小数或有纳秒），而 JS 的
// JSON.stringify(Date) 恒为 `.SSS`。对拍比对正文时这个格式差异会掩盖真正的语义差异。
func isoTimeOrNil(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// stringOrNil 把可空字符串转成 any（nil 保持 null）。
func stringOrNil(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// int64OrNil 把 0 视作「未指定」（Node 的 currentSequence 为 null 而非 0）。
func int64OrNil(value int64) any {
	if value <= 0 {
		return nil
	}
	return value
}

// navigatorBody 把导航目标翻成响应体（nil 保持 null）。
func navigatorBody(target *store.AdminSessionAdjacentRequest) *sessionDetailNavigatorBody {
	if target == nil {
		return nil
	}
	return &sessionDetailNavigatorBody{
		RequestID:       target.RequestID,
		SourceSessionID: target.SourceSessionID,
		RequestSequence: target.RequestSequence,
	}
}

// headerRecordOrNil 把空头表转成 nil（JSON null），与 Node 的 `headers || null` 同判。
func headerRecordOrNil(headers map[string]string) any {
	if len(headers) == 0 {
		return nil
	}
	return headers
}

// auditSettings 取审计行里的设置项；nil 审计行返回 nil。
func auditSettings(audit *store.AdminMessageRequestAudit) []map[string]any {
	if audit == nil {
		return nil
	}
	return audit.DecodeSpecialSettings()
}

func blockedByOf(audit *store.AdminMessageRequestAudit) *string {
	if audit == nil {
		return nil
	}
	return audit.BlockedBy
}

func blockedReasonOf(audit *store.AdminMessageRequestAudit) *string {
	if audit == nil {
		return nil
	}
	return audit.BlockedReason
}

func statusCodeOf(audit *store.AdminMessageRequestAudit) *int {
	if audit == nil {
		return nil
	}
	return audit.StatusCode
}

func cacheTTLOf(audit *store.AdminMessageRequestAudit) *string {
	if audit == nil {
		return nil
	}
	return audit.CacheTTLApplied
}

// parseJSONStringOrNull 复刻 parseJsonStringOrNull：字符串试解 JSON，失败给 null。
func parseJSONStringOrNull(value any) any {
	text, ok := value.(string)
	if !ok {
		return value
	}
	decoded, err := decodeJSONValue(text)
	if err != nil {
		return nil
	}
	return decoded
}

// parseJSONStringOrKeepRaw 复刻 parseJsonStringOrKeepRaw：字符串试解 JSON，失败保留原串。
func parseJSONStringOrKeepRaw(value any) any {
	text, ok := value.(string)
	if !ok {
		return value
	}
	decoded, err := decodeJSONValue(text)
	if err != nil {
		return value
	}
	return decoded
}

// decodeJSONValue 解一个 JSON 值（数字保持 json.Number，避免大整数失真）。
func decodeJSONValue(raw string) (any, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

// normalizeRequestSnapshot 复刻 normalizeRequestSnapshot（active-sessions.ts:136）。
//
// 一条不对称（照抄）：before 的 messages 从快照的 messages 字段解析；after 的 messages 从
// **body.messages** 里提取——因为 after 快照的 body 是最终发往上游的正文，messages 就嵌在
// 里面，而 after 的 messages 字段在 Node 侧不写。
func normalizeRequestSnapshot(
	snapshot *session.SessionDetailSnapshotRead, phase string,
) *sessionDetailRequestSnapshotBody {
	if snapshot == nil {
		return nil
	}
	body := parseJSONStringOrKeepRaw(snapshot.Body)
	messages := any(nil)
	if phase == "before" {
		if snapshot.HasMessages {
			messages = parseJSONStringOrKeepRaw(snapshot.Messages)
		}
	} else {
		messages = extractAfterRequestMessages(body)
	}
	if !isSessionMessages(messages) {
		messages = nil
	}
	return &sessionDetailRequestSnapshotBody{
		Body:     body,
		Messages: messages,
		Headers:  headerRecordOrNil(snapshot.Headers),
		Meta: sessionDetailRequestMetaBody{
			ClientURL:   snapshot.Meta.ClientURL,
			UpstreamURL: snapshot.Meta.UpstreamURL,
			Method:      snapshot.Meta.Method,
		},
	}
}

// normalizeResponseSnapshot 复刻 normalizeResponseSnapshot（:159）：响应侧没有 messages。
func normalizeResponseSnapshot(
	snapshot *session.SessionDetailSnapshotRead,
) *sessionDetailResponseSnapshotBody {
	if snapshot == nil {
		return nil
	}
	var statusCode any
	if snapshot.Meta.StatusCode != nil {
		statusCode = *snapshot.Meta.StatusCode
	}
	return &sessionDetailResponseSnapshotBody{
		Body:    snapshot.Body,
		Headers: headerRecordOrNil(snapshot.Headers),
		Meta: sessionDetailResponseMetaBody{
			UpstreamURL: snapshot.Meta.UpstreamURL,
			StatusCode:  statusCode,
		},
	}
}

// snapshotBodyOf 取「after 优先、before 兜底」的请求正文。
func snapshotBodyOf(after, before *sessionDetailRequestSnapshotBody) any {
	if after != nil && after.Body != nil {
		return after.Body
	}
	if before != nil {
		return before.Body
	}
	return nil
}

// snapshotResponseBodyOf 取「after 优先、before 兜底」的响应正文（返回指针以便区分 nil 与空串）。
func snapshotResponseBodyOf(after, before *sessionDetailResponseSnapshotBody) *string {
	for _, snapshot := range []*sessionDetailResponseSnapshotBody{after, before} {
		if snapshot == nil {
			continue
		}
		if text, ok := snapshot.Body.(string); ok {
			return &text
		}
		if snapshot.Body != nil {
			if encoded, err := json.Marshal(snapshot.Body); err == nil {
				value := string(encoded)
				return &value
			}
		}
	}
	return nil
}

// legacySnapshotInputs 是 buildLegacyCompatibilitySnapshots 的入参。
type legacySnapshotInputs struct {
	requestBody     any
	messages        any
	response        any
	requestHeaders  map[string]string
	responseHeaders map[string]string
	requestMeta     sessionDetailRequestMetaBody
	responseMeta    sessionDetailResponseMetaBody
}

// buildLegacyCompatibilitySnapshots 复刻同名的 legacy 兼容层（:175）。
//
// 它只填 **after** 视图（Node 原文如此），且「有没有 legacy 数据」的判据是三件事的或：
// 正文非 null、头非空、messages 非 null（响应侧是两件事：正文与头）。判据里**不含** meta——
// 只有 meta 时 Node 认为「没有 legacy 数据」，于是 after 为 null。
func buildLegacyCompatibilitySnapshots(in legacySnapshotInputs) sessionDetailSnapshotsBody {
	result := sessionDetailSnapshotsBody{DefaultView: defaultSessionDetailViewMode}
	hasRequest := in.requestBody != nil || len(in.requestHeaders) > 0 || in.messages != nil
	hasResponse := in.response != nil || len(in.responseHeaders) > 0

	if hasRequest {
		var messages any
		if isSessionMessages(in.messages) {
			messages = in.messages
		}
		result.Request.After = &sessionDetailRequestSnapshotBody{
			Body:     in.requestBody,
			Messages: messages,
			Headers:  headerRecordOrNil(in.requestHeaders),
			Meta:     in.requestMeta,
		}
	}
	if hasResponse {
		result.Response.After = &sessionDetailResponseSnapshotBody{
			Body:    in.response,
			Headers: headerRecordOrNil(in.responseHeaders),
			Meta:    in.responseMeta,
		}
	}
	return result
}

// mergeLegacyRequestAfterSnapshot 复刻 mergeLegacyRequestAfterSnapshot（:218）。
//
// 三条分支，逐条照抄：
//   - 快照为 nil → 直接用 legacy；
//   - 快照的三个内容字段**全空**（body/messages/headers 都是 null）且有 legacy → 用 legacy，
//     但 meta 取快照自己的（clientUrl/upstreamUrl/method 三个字段各自回退 legacy）；
//   - 其余 → 用快照（内容是快照的，不做字段级合并）。
func mergeLegacyRequestAfterSnapshot(
	snapshot, legacy *sessionDetailRequestSnapshotBody,
) *sessionDetailRequestSnapshotBody {
	if snapshot == nil {
		return legacy
	}
	if snapshot.Body == nil && snapshot.Messages == nil && snapshot.Headers == nil && legacy != nil {
		merged := *legacy
		merged.Meta = sessionDetailRequestMetaBody{
			ClientURL:   snapshot.Meta.ClientURL,
			UpstreamURL: firstNonNil(snapshot.Meta.UpstreamURL, legacy.Meta.UpstreamURL),
			Method:      firstNonNil(snapshot.Meta.Method, legacy.Meta.Method),
		}
		return &merged
	}
	return snapshot
}

// mergeLegacyResponseAfterSnapshot 复刻 mergeLegacyResponseAfterSnapshot（:242）。
func mergeLegacyResponseAfterSnapshot(
	snapshot, legacy *sessionDetailResponseSnapshotBody,
) *sessionDetailResponseSnapshotBody {
	if snapshot == nil {
		return legacy
	}
	if snapshot.Body == nil && snapshot.Headers == nil && legacy != nil {
		merged := *legacy
		merged.Meta = sessionDetailResponseMetaBody{
			UpstreamURL: firstNonNil(snapshot.Meta.UpstreamURL, legacy.Meta.UpstreamURL),
			StatusCode:  firstNonNil(snapshot.Meta.StatusCode, legacy.Meta.StatusCode),
		}
		return &merged
	}
	return snapshot
}

// firstNonNil 取第一个非 nil 的值（Node 的 `??` 语义）。
func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

// isSessionMessages 复刻 isSessionMessages（session-detail-snapshots.ts:13）。
//
// 空数组判**是**（JS 的 `[].every(...)` 恒为 true）——这不是笔误，是必须照抄的一处边界。
func isSessionMessages(value any) bool {
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if !isNonEmptyPlainRecord(item) {
				return false
			}
		}
		return true
	default:
		return isNonEmptyPlainRecord(value)
	}
}

// isNonEmptyPlainRecord 复刻 isNonEmptyPlainRecord：非数组、非 null 的对象且至少一个键。
func isNonEmptyPlainRecord(value any) bool {
	object, ok := value.(map[string]any)
	if !ok {
		return false
	}
	return len(object) > 0
}

// extractAfterRequestMessages 复刻 extractAfterRequestMessages（:21）。
func extractAfterRequestMessages(value any) any {
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	messages, present := object["messages"]
	if !present {
		return nil
	}
	if isSessionMessages(messages) {
		return messages
	}
	return nil
}
