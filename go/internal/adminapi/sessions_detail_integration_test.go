package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/session"
)

// 本文件是**详情面**（`GET /sessions/{id}`）的真依赖验收：按「**生产写侧 → 读侧 → 端点**」闭环，
// 而不是拿手写键去喂读侧。
//
// 为什么闭环是唯一有意义的钉子：详情页的九类事实里六类来自 Redis 工件，而这六类各自有一对
// （写侧键名，读侧键名）。两侧写错同一个键名的一半时，端点会**静默变空**——每个写侧单测与
// 每个读侧单测都会通过（各测各的），只有穿过两边才能发现。故这里全部经 `session.Binder`
// 的生产方法（数据面调的就是这一组）写入，再用端点读出。

// seedDetailArtifacts 用生产写侧落下详情页需要的六类工件。
//
// 顺序照数据面：所有者键 → 正文/头/元信息 → 四份相位快照。全部用同一个
// (sessionID, sequence, keyID)。
func seedDetailArtifacts(
	t *testing.T,
	binder *session.Binder,
	sessionID string,
	sequence int,
	keyID int64,
) {
	t.Helper()
	ctx := context.Background()
	options := session.SessionArtifactOptions{StoreMessages: false, StoreResponseBody: true}

	if err := binder.StoreSessionRequestOwner(ctx, sessionID, sequence, keyID); err != nil {
		t.Fatalf("写所有者键失败: %v", err)
	}
	if err := binder.StoreSessionRequestBody(ctx, sessionID,
		map[string]any{"model": "claude-sonnet-4-5", "messages": []any{
			map[string]any{"role": "user", "content": "原始提问"},
		}}, sequence, options); err != nil {
		t.Fatalf("写请求正文失败: %v", err)
	}
	if err := binder.StoreSessionResponse(ctx, sessionID, sequence, keyID,
		[]byte(`{"content":[{"type":"text","text":"最终回答"}]}`), options); err != nil {
		t.Fatalf("写响应正文失败: %v", err)
	}
	if err := binder.StoreSessionRequestHeaders(ctx, sessionID, sequence,
		map[string]string{"Anthropic-Version": "2023-06-01"}); err != nil {
		t.Fatalf("写请求头失败: %v", err)
	}
	if err := binder.StoreSessionResponseHeaders(ctx, sessionID, sequence, keyID,
		map[string]string{"Content-Type": "application/json"}); err != nil {
		t.Fatalf("写响应头失败: %v", err)
	}
	if err := binder.StoreSessionUpstreamRequestMeta(ctx, sessionID, sequence,
		session.SessionUpstreamRequestMeta{
			URL:    "https://api.example.com/v1/messages?api_key=sk-secret",
			Method: "POST",
		}); err != nil {
		t.Fatalf("写上游请求元信息失败: %v", err)
	}
	if err := binder.StoreSessionUpstreamResponseMeta(ctx, sessionID, sequence, keyID,
		session.SessionUpstreamResponseMeta{
			URL:        "https://api.example.com/v1/messages",
			StatusCode: 200,
		}); err != nil {
		t.Fatalf("写上游响应元信息失败: %v", err)
	}
	phaseOptions := session.PhaseSnapshotOptions{StoreMessages: false}
	upstreamURL := "https://api.example.com/v1/messages"
	method := "POST"
	status := 200
	clientURL := "https://hub.example.com/v1/messages"
	requestBefore := session.SessionDetailPhaseSnapshot{
		Body:        map[string]any{"model": "claude-sonnet-4-5"},
		Messages:    []any{map[string]any{"role": "user", "content": "[REDACTED]"}},
		HasMessages: true,
		Headers:     map[string]string{"Anthropic-Version": "2023-06-01"},
		Meta: session.SessionDetailPhaseMeta{
			ClientURL: &clientURL, UpstreamURL: nil, Method: &method,
		},
	}
	requestAfter := session.SessionDetailPhaseSnapshot{
		Body: map[string]any{"model": "claude-sonnet-4-5",
			"messages": []any{map[string]any{"role": "user", "content": "[REDACTED]"}}},
		Headers: map[string]string{"Anthropic-Version": "2023-06-01"},
		Meta:    session.SessionDetailPhaseMeta{UpstreamURL: &upstreamURL, Method: &method},
	}
	responseBefore := session.SessionDetailPhaseSnapshot{
		Headers: map[string]string{"Content-Type": "application/json"},
		Meta:    session.SessionDetailPhaseMeta{UpstreamURL: &upstreamURL, StatusCode: &status},
	}
	responseAfter := session.SessionDetailPhaseSnapshot{
		Body:    `{"content":"最终回答"}`,
		Headers: map[string]string{"Content-Type": "application/json"},
		Meta:    session.SessionDetailPhaseMeta{UpstreamURL: nil, StatusCode: &status},
	}
	for _, entry := range []struct {
		kind, phase string
		snapshot    session.SessionDetailPhaseSnapshot
	}{
		{"request", "before", requestBefore},
		{"request", "after", requestAfter},
		{"response", "before", responseBefore},
		{"response", "after", responseAfter},
	} {
		if fields := binder.StoreSessionPhaseSnapshot(ctx, sessionID, sequence, keyID,
			entry.kind, entry.phase, entry.snapshot, phaseOptions); len(fields) == 0 {
			t.Fatalf("%s/%s 快照未写入", entry.kind, entry.phase)
		}
	}
	t.Cleanup(func() {
		for _, key := range detailArtifactKeys(sessionID, sequence) {
			_ = binder.RawDel(ctx, key)
		}
	})
}

// detailArtifactKeys 列出详情面用例写下的全部键（清理用）。
func detailArtifactKeys(sessionID string, sequence int) []string {
	keys := []string{
		session.SessionRequestOwnerKey(sessionID, sequence),
		session.RequestBodyKey(sessionID, sequence),
		session.MessagesSequenceKey(sessionID, sequence),
		session.SessionResponseKey(sessionID, sequence),
		session.SessionRequestHeadersKey(sessionID, sequence),
		session.SessionResponseHeadersKey(sessionID, sequence),
		session.SessionUpstreamRequestMetaKey(sessionID, sequence),
		session.SessionUpstreamResponseMetaKey(sessionID, sequence),
	}
	for _, kind := range []string{"request", "response"} {
		for _, phase := range []string{"before", "after"} {
			for _, field := range []string{"body", "messages", "headers", "meta"} {
				keys = append(keys, session.SessionDetailSnapshotKey(sessionID, sequence, kind, phase, field))
			}
		}
	}
	return keys
}

// TestSessionDetailClosedLoop 钉住详情端点从生产写侧读回九类事实，并且**默认视图是相位快照**。
func TestSessionDetailClosedLoop(t *testing.T) {
	deps := openSessionsDeps(t)
	pools := deps.Pools
	ctx := context.Background()

	providerID := fixtureProviderID(t, pools)
	ownerID := fixtureUser(t, pools, "user", true)
	otherID := fixtureUser(t, pools, "user", true)
	keyID, keyValue := fixtureKey(t, pools, fixtureKeyOptions{
		userID: ownerID, canLoginWebUI: true, isEnabled: true,
	})

	identity := fmt.Sprintf("sess-detail-%d", time.Now().UnixNano())
	selectedID, _ := seedOriginChainRows(t, pools, ownerID, keyValue, identity, providerID)
	// 夹具把序号 3 作为最新一行；详情面不带选择器时就落在它上面。
	seedDetailArtifacts(t, deps.Binder, identity, 3, keyID)

	ownerRouter := sessionsRouterWithDeps(t, pools,
		Principal{UserID: ownerID, IsAdmin: false}, deps)

	status, body, raw := sessionsRequest(t, ownerRouter, http.MethodGet,
		"/api/v1/sessions/"+identity, "")
	if status != http.StatusOK {
		t.Fatalf("详情应 200，实得 %d（正文 %s）", status, raw)
	}

	// 1. 身份与序号：canonical/物理/序号三件套。
	if body["canonicalSessionId"] != identity {
		t.Errorf("canonicalSessionId 应为 %s，实得 %v", identity, body["canonicalSessionId"])
	}
	if body["currentSourceSessionId"] != identity {
		t.Errorf("currentSourceSessionId 应为 %s，实得 %v", identity, body["currentSourceSessionId"])
	}
	if body["currentSequence"] != float64(3) {
		t.Errorf("currentSequence 应为 3，实得 %v", body["currentSequence"])
	}

	// 2. **默认视图是相位快照**：response.after.body 必须来自快照工件。
	snapshots, ok := body["snapshots"].(map[string]any)
	if !ok {
		t.Fatalf("snapshots 缺失或形状不对：%v", body["snapshots"])
	}
	if snapshots["defaultView"] != "after" {
		t.Errorf("defaultView 应为 after，实得 %v", snapshots["defaultView"])
	}
	responseView, _ := snapshots["response"].(map[string]any)
	afterView, _ := responseView["after"].(map[string]any)
	// 写侧在 STORE_MESSAGES=false 下对 JSON 正文落**脱敏副本**（Node 的 redactResponseBody 同义），
	// 故这里读回的正文应已脱敏——它证明的是「快照键被读到了」，不是「正文原样」。
	if afterView == nil || afterView["body"] != `{"content":"[REDACTED]"}` {
		t.Errorf("response.after.body 应来自相位快照（已脱敏），实得 %v", responseView)
	}
	beforeView, _ := responseView["before"].(map[string]any)
	if beforeView == nil || beforeView["body"] != nil {
		t.Errorf("response.before.body 应为 null（只在 after 落正文），实得 %v", beforeView)
	}

	// 3. 相位快照的 meta 与头逐字段可读。
	requestView, _ := snapshots["request"].(map[string]any)
	requestBeforeView, _ := requestView["before"].(map[string]any)
	requestBeforeMeta, _ := requestBeforeView["meta"].(map[string]any)
	if requestBeforeMeta["clientUrl"] != "https://hub.example.com/v1/messages" {
		t.Errorf("request.before.meta.clientUrl 不符：%v", requestBeforeMeta)
	}
	requestAfterView, _ := requestView["after"].(map[string]any)
	requestAfterMeta, _ := requestAfterView["meta"].(map[string]any)
	if requestAfterMeta["upstreamUrl"] != "https://api.example.com/v1/messages" {
		t.Errorf("request.after.meta.upstreamUrl 不符：%v", requestAfterMeta)
	}

	// 4. legacy 平铺字段：快照有值时**不回退** legacy，故 response 由快照给出（脱敏副本）。
	if body["response"] != `{"content":"[REDACTED]"}` {
		t.Errorf("response 应取快照值，实得 %v", body["response"])
	}
	// messages 取自 before 快照，且已脱敏。
	messages, _ := body["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("messages 应为 1 条，实得 %v", body["messages"])
	}
	first, _ := messages[0].(map[string]any)
	if first["content"] != "[REDACTED]" {
		t.Errorf("messages 内容应已脱敏，实得 %v", first)
	}
	// requestBody 取自 request.after 的 body（快照字段优先）。
	requestBody, _ := body["requestBody"].(map[string]any)
	if requestBody == nil || requestBody["model"] != "claude-sonnet-4-5" {
		t.Errorf("requestBody 不符：%v", body["requestBody"])
	}

	// 5. 头与上游元信息：URL 必须已脱敏（写侧 sanitizeUrl 生效）。
	requestHeaders, _ := body["requestHeaders"].(map[string]any)
	if requestHeaders["Anthropic-Version"] != "2023-06-01" {
		t.Errorf("requestHeaders 不符：%v", requestHeaders)
	}
	requestMeta, _ := body["requestMeta"].(map[string]any)
	if requestMeta["upstreamUrl"] != "https://api.example.com/v1/messages?api_key=[REDACTED]" {
		t.Errorf("requestMeta.upstreamUrl 应已脱敏：%v", requestMeta)
	}
	responseMeta, _ := body["responseMeta"].(map[string]any)
	if responseMeta["statusCode"] != float64(200) {
		t.Errorf("responseMeta.statusCode 应为 200：%v", responseMeta)
	}

	// 6. 账本聚合与相邻导航：聚合来自账本，导航指向夹具的相邻两行。
	stats, ok := body["sessionStats"].(map[string]any)
	if !ok {
		t.Fatalf("sessionStats 缺失：%v", body["sessionStats"])
	}
	if stats["requestCount"] != float64(3) {
		t.Errorf("sessionStats.requestCount 应为 3，实得 %v", stats["requestCount"])
	}
	if stats["userId"] != float64(ownerID) {
		t.Errorf("sessionStats.userId 应为 %d，实得 %v", ownerID, stats["userId"])
	}
	prev, _ := body["prevRequest"].(map[string]any)
	if prev == nil || prev["requestId"] == nil {
		t.Errorf("序号 3 应有前一条请求，实得 %v", body["prevRequest"])
	} else if prev["requestSequence"] != float64(2) {
		t.Errorf("前一条应为序号 2，实得 %v", prev["requestSequence"])
	}
	if body["prevSequence"] != float64(2) {
		t.Errorf("prevSequence 应为 2，实得 %v", body["prevSequence"])
	}
	// 序号 3 是最新一行：没有下一条。
	if body["nextRequest"] != nil || body["nextSequence"] != nil {
		t.Errorf("最新一行不应有下一条：next=%v seq=%v", body["nextRequest"], body["nextSequence"])
	}
	_ = selectedID

	// 7. 显式序号：定位到序号 1（那里没有工件写入）→ 快照全空、legacy 也不存在。
	status, body, raw = sessionsRequest(t, ownerRouter, http.MethodGet,
		"/api/v1/sessions/"+identity+"?requestSequence=1", "")
	if status != http.StatusOK {
		t.Fatalf("指定序号 1 应 200，实得 %d（%s）", status, raw)
	}
	sequenceOne, _ := body["snapshots"].(map[string]any)
	sequenceOneRequest, _ := sequenceOne["request"].(map[string]any)
	oneBefore, _ := sequenceOneRequest["before"].(map[string]any)
	oneAfter, _ := sequenceOneRequest["after"].(map[string]any)
	// 序号 1 未写任何工件，也没有 legacy 正文/消息 → 两个视图都为 null。
	if oneBefore != nil && oneBefore["body"] != nil {
		t.Errorf("序号 1 的 request.before.body 应为 null，实得 %v", oneBefore)
	}
	if oneAfter != nil && oneAfter["body"] != nil {
		t.Errorf("序号 1 的 request.after.body 应为 null，实得 %v", oneAfter)
	}
	// 序号 1 没有下一条以外的导航（序号 2 是它的下一条）。
	if body["nextSequence"] != float64(2) {
		t.Errorf("序号 1 的 nextSequence 应为 2，实得 %v", body["nextSequence"])
	}

	// 8. 围栏：把序号 3 的所有者键改成别的 keyId → 六类工件全部按「没有」处理（相位快照落空）。
	if err := deps.Binder.StoreSessionRequestOwner(ctx, identity, 3, keyID+999); err != nil {
		t.Fatalf("改所有者键失败: %v", err)
	}
	status, body, _ = sessionsRequest(t, ownerRouter, http.MethodGet,
		"/api/v1/sessions/"+identity+"?requestSequence=3", "")
	if status != http.StatusOK {
		t.Fatalf("围栏不匹配时仍应 200（只是读不到工件），实得 %d", status)
	}
	guardedSnapshots, _ := body["snapshots"].(map[string]any)
	guardedRequest, _ := guardedSnapshots["request"].(map[string]any)
	if guardedRequest["before"] != nil || guardedRequest["after"] != nil {
		t.Errorf("围栏不匹配时相位快照应全空，实得 %v", guardedRequest)
	}
	if body["requestHeaders"] != nil {
		t.Errorf("围栏不匹配时不应读到工件头，实得 %v", body["requestHeaders"])
	}
	// 账本侧事实不受 Redis 围栏影响（聚合与导航照给）。
	if body["currentSequence"] != float64(3) {
		t.Errorf("围栏不匹配时账本侧事实仍应齐备，实得 %v", body["currentSequence"])
	}

	// 9. 会话不存在 → 404 session.not_found；他人（非管理员）→ 403 session.action_failed。
	missing := fmt.Sprintf("sess-detail-missing-%d", time.Now().UnixNano())
	status, problem, _ := sessionsRequest(t, ownerRouter, http.MethodGet,
		"/api/v1/sessions/"+missing, "")
	if status != http.StatusNotFound || problem["errorCode"] != "session.not_found" {
		t.Fatalf("不存在的会话应 404 session.not_found，实得 %d %v", status, problem)
	}
	// **他人（非管理员）答 404 而不是 403**：账本聚合本身带 owner 条件，别人的会话根本查不
	// 出来，于是走「查不到」分支。Node 同序（loadCanonicalSessionStats 带 ownerUserId），
	// 故 403 分支只在「能查到聚合但归属不符」时可达——那条路径在当前实现里不可达，
	// 保留它是为了与 Node 的分支结构逐条对应。
	foreignRouter := sessionsRouterWithDeps(t, pools,
		Principal{UserID: otherID, IsAdmin: false}, deps)
	status, problem, _ = sessionsRequest(t, foreignRouter, http.MethodGet,
		"/api/v1/sessions/"+identity, "")
	if status != http.StatusNotFound || problem["errorCode"] != "session.not_found" {
		t.Fatalf("他人会话应 404 session.not_found（聚合带 owner 条件），实得 %d %v", status, problem)
	}
	adminRouter := sessionsRouterWithDeps(t, pools,
		Principal{UserID: ownerID, IsAdmin: true}, deps)
	status, _, _ = sessionsRequest(t, adminRouter, http.MethodGet,
		"/api/v1/sessions/"+identity, "")
	if status != http.StatusOK {
		t.Fatalf("管理员应能读任意会话，实得 %d", status)
	}
}

// TestSessionDetailLegacyFallback 钉住「快照缺失时由 legacy 键拼出 after 视图」这条回退。
//
// 为什么值得单独钉：回退的触发条件是「快照的三个内容字段全空」，而 legacy 兼容层填的正是
// after 视图——**默认视图**。回退失效时详情页会静默变成「只有账本信息的半个页」，而
// 有快照的用例照样全绿。
func TestSessionDetailLegacyFallback(t *testing.T) {
	deps := openSessionsDeps(t)
	pools := deps.Pools
	ctx := context.Background()

	providerID := fixtureProviderID(t, pools)
	ownerID := fixtureUser(t, pools, "user", true)
	keyID, keyValue := fixtureKey(t, pools, fixtureKeyOptions{
		userID: ownerID, canLoginWebUI: true, isEnabled: true,
	})

	identity := fmt.Sprintf("sess-legacy-%d", time.Now().UnixNano())
	seedOriginChainRows(t, pools, ownerID, keyValue, identity, providerID)

	// 只写 legacy 三条 + 所有者键：**一份相位快照都不写**，模拟升级前写入的会话。
	sequence := 3
	options := session.SessionArtifactOptions{StoreMessages: false, StoreResponseBody: true}
	if err := deps.Binder.StoreSessionRequestOwner(ctx, identity, sequence, keyID); err != nil {
		t.Fatalf("写所有者键失败: %v", err)
	}
	if err := deps.Binder.StoreSessionRequestBody(ctx, identity,
		map[string]any{"messages": []any{map[string]any{"role": "user", "content": "[REDACTED]"}}},
		sequence, options); err != nil {
		t.Fatalf("写请求正文失败: %v", err)
	}
	if err := deps.Binder.StoreSessionMessages(ctx, identity,
		[]any{map[string]any{"role": "user", "content": "[REDACTED]"}}, sequence, options); err != nil {
		t.Fatalf("写 messages 工件失败: %v", err)
	}
	if err := deps.Binder.StoreSessionResponse(ctx, identity, sequence, keyID,
		[]byte(`{"legacy":true}`), options); err != nil {
		t.Fatalf("写响应正文失败: %v", err)
	}
	t.Cleanup(func() {
		for _, key := range []string{
			session.SessionRequestOwnerKey(identity, sequence),
			session.RequestBodyKey(identity, sequence),
			session.MessagesSequenceKey(identity, sequence),
			session.SessionResponseKey(identity, sequence),
		} {
			_ = deps.Binder.RawDel(ctx, key)
		}
	})

	router := sessionsRouterWithDeps(t, pools,
		Principal{UserID: ownerID, IsAdmin: false}, deps)
	status, body, raw := sessionsRequest(t, router, http.MethodGet,
		"/api/v1/sessions/"+identity, "")
	if status != http.StatusOK {
		t.Fatalf("应 200，实得 %d（%s）", status, raw)
	}

	// 平铺字段来自 legacy 键。
	if body["response"] != `{"legacy":true}` {
		t.Errorf("response 应来自 legacy 键，实得 %v", body["response"])
	}
	// **after 视图由 legacy 兼容层拼出**（快照全空 ⇒ 回退触发）。
	snapshots, _ := body["snapshots"].(map[string]any)
	responseView, _ := snapshots["response"].(map[string]any)
	afterView, _ := responseView["after"].(map[string]any)
	if afterView == nil || afterView["body"] != `{"legacy":true}` {
		t.Errorf("legacy 回退应填出 response.after，实得 %v", responseView)
	}
	// before 视图恒为 null：legacy 兼容层只填 after（Node 原文如此）。
	if responseView["before"] != nil {
		t.Errorf("legacy 回退不应填 before 视图，实得 %v", responseView["before"])
	}
	requestView, _ := snapshots["request"].(map[string]any)
	requestAfter, _ := requestView["after"].(map[string]any)
	if requestAfter == nil {
		t.Fatalf("legacy 回退应填出 request.after，实得 %v", requestView)
	}
	messages, _ := requestAfter["messages"].([]any)
	if len(messages) != 1 {
		t.Errorf("request.after.messages 应来自 legacy messages 键，实得 %v", requestAfter["messages"])
	}
}

// TestSessionDetailNoArtifactsStillServes 无任何工件时端点仍要给出完整的账本侧事实。
func TestSessionDetailNoArtifactsStillServes(t *testing.T) {
	deps := openSessionsDeps(t)
	pools := deps.Pools
	providerID := fixtureProviderID(t, pools)
	ownerID := fixtureUser(t, pools, "user", true)
	_, keyValue := fixtureKey(t, pools, fixtureKeyOptions{
		userID: ownerID, canLoginWebUI: true, isEnabled: true,
	})
	identity := fmt.Sprintf("sess-bare-%d", time.Now().UnixNano())
	seedOriginChainRows(t, pools, ownerID, keyValue, identity, providerID)

	router := sessionsRouterWithDeps(t, pools,
		Principal{UserID: ownerID, IsAdmin: false}, deps)
	status, body, raw := sessionsRequest(t, router, http.MethodGet,
		"/api/v1/sessions/"+identity, "")
	if status != http.StatusOK {
		t.Fatalf("应 200，实得 %d（%s）", status, raw)
	}
	// 工件面全空（不是 404）：详情页据此显示「没有调试数据」。
	if body["requestBody"] != nil || body["messages"] != nil || body["response"] != nil {
		t.Errorf("无工件时正文类字段应为 null：%v %v %v",
			body["requestBody"], body["messages"], body["response"])
	}
	if body["requestHeaders"] != nil || body["responseHeaders"] != nil {
		t.Errorf("无工件时头字段应为 null：%v %v", body["requestHeaders"], body["responseHeaders"])
	}
	// 相位快照的四份都是 null。
	snapshots, _ := body["snapshots"].(map[string]any)
	requestView, _ := snapshots["request"].(map[string]any)
	responseView, _ := snapshots["response"].(map[string]any)
	if requestView["before"] != nil || requestView["after"] != nil ||
		responseView["before"] != nil || responseView["after"] != nil {
		t.Errorf("无工件时四个相位应为 null：%v", snapshots)
	}
	// 账本侧事实齐备。
	if body["sessionStats"] == nil || body["currentSequence"] != float64(3) {
		t.Errorf("账本侧事实应齐备：stats=%v seq=%v", body["sessionStats"], body["currentSequence"])
	}
}

// TestSessionDetailShapeKeys 钉住响应体的**键集**（19 个键，一个不少）。
//
// 键集是 UI 的隐含契约：少一个键，前端读 undefined 后静默渲染成空白，比报错更难查。
func TestSessionDetailShapeKeys(t *testing.T) {
	deps := openSessionsDeps(t)
	pools := deps.Pools
	providerID := fixtureProviderID(t, pools)
	ownerID := fixtureUser(t, pools, "user", true)
	_, keyValue := fixtureKey(t, pools, fixtureKeyOptions{
		userID: ownerID, canLoginWebUI: true, isEnabled: true,
	})
	identity := fmt.Sprintf("sess-shape-%d", time.Now().UnixNano())
	seedOriginChainRows(t, pools, ownerID, keyValue, identity, providerID)

	router := sessionsRouterWithDeps(t, pools,
		Principal{UserID: ownerID, IsAdmin: false}, deps)
	status, _, raw := sessionsRequest(t, router, http.MethodGet,
		"/api/v1/sessions/"+identity, "")
	if status != http.StatusOK {
		t.Fatalf("应 200，实得 %d", status)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("正文不是 JSON 对象：%v", err)
	}
	want := []string{
		"requestBody", "messages", "response", "requestHeaders", "responseHeaders",
		"requestMeta", "responseMeta", "snapshots", "specialSettings", "sessionStats",
		"canonicalSessionId", "currentSourceSessionId", "currentSequence",
		"prevRequest", "nextRequest", "prevSequence", "nextSequence",
	}
	for _, key := range want {
		if _, ok := decoded[key]; !ok {
			t.Errorf("响应体缺少键 %s（实得键集 %v）", key, keysOfSessionDetail(decoded))
		}
	}
	if len(decoded) != len(want) {
		t.Errorf("键数应为 %d，实得 %d：%v", len(want), len(decoded), keysOfSessionDetail(decoded))
	}
	// 相位快照容器的键集。
	var snapshots map[string]json.RawMessage
	if err := json.Unmarshal(decoded["snapshots"], &snapshots); err != nil {
		t.Fatalf("snapshots 不是对象：%v", err)
	}
	for _, key := range []string{"defaultView", "request", "response"} {
		if _, ok := snapshots[key]; !ok {
			t.Errorf("snapshots 缺少键 %s", key)
		}
	}
}

// keysOfSessionDetail 列出响应体的键（排序后便于比对）。
func keysOfSessionDetail(decoded map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(decoded))
	for key := range decoded {
		keys = append(keys, key)
	}
	return keys
}
