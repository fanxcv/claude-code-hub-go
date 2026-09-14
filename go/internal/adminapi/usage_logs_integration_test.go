package adminapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 usage-logs 的真实 PG 集成测试：夹具自建、自清，且**钉成唯一可命中的候选**
// （模型名带唯一后缀，所有查询都按该模型过滤），因此不会与并发的其它集成测试互相影响。
//
// 两条与库现状有关的事实：
//
//   - 复用库里已有的 user id=1 与密钥 sk-loadtest-50（loadtest 种子数据），避免在共享库上
//     新建用户/密钥这类会影响他人选路的行。
//   - **不显式插 usage_ledger**：库中 message_request 上挂着 AFTER INSERT 触发器
//     `trg_upsert_usage_ledger`，会自动把行同步进账本；显式再插一次会撞
//     `idx_usage_ledger_request_id`（该索引是 request_id 上的唯一索引）。
//     清理时按 request_id 删账本行（触发器不负责删除）。

func ulIntegrationDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("CCH_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 CCH_TEST_DSN，跳过数据库集成测试")
	}
	return dsn
}

func ulOpenPools(t *testing.T) *store.Pools {
	t.Helper()
	pools, err := store.Open(context.Background(), store.Options{
		DSN:      ulIntegrationDSN(t),
		Budget:   config.SplitPoolBudget(6),
		Timeouts: config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("建立连接池失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	return pools
}

// usageLogFixture 是一次集成测试的自有数据。
type usageLogFixture struct {
	model      string
	sessionID  string
	requestIDs []int64
}

const (
	fixtureUserID   = 1
	fixtureKeyValue = "sk-loadtest-50"
)

// seedUsageLogFixture 插入两条 message_request：一条 200/未被拦截，一条 403/被拦截
// （用于验证 guard_intercept 派生与状态码过滤）。
func seedUsageLogFixture(t *testing.T, pools *store.Pools) *usageLogFixture {
	t.Helper()
	ctx := context.Background()
	suffix := time.Now().Format("20060102150405.000000000")
	fixture := &usageLogFixture{
		model:     "go-admin-usage-logs-it-" + suffix,
		sessionID: "go-admin-it-session-" + suffix,
	}

	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取 control 分道失败: %v", err)
	}
	model := fixture.model
	endpoint := "/v1/messages"
	cost := "0.250000000000000"
	sessionIdentityKind := "session_id"
	provider := int64(1)
	blockedBy := "sensitive_word"
	blockedReason := `{"rule":"it"}`

	cases := []struct {
		statusCode   *int
		inputTokens  int64
		outputTokens int64
		cacheRead    int64
		blockedBy    *string
	}{
		{statusCode: ulIntPtr(200), inputTokens: 100, outputTokens: 20, cacheRead: 40},
		{statusCode: ulIntPtr(403), blockedBy: &blockedBy},
	}
	for index, testCase := range cases {
		var requestID int64
		err := pool.QueryRow(ctx, `
			INSERT INTO message_request (
				provider_id, user_id, key, model, original_model, actual_response_model, endpoint,
				status_code, input_tokens, output_tokens, cache_read_input_tokens,
				cache_creation_input_tokens, cost_usd, duration_ms, ttfb_ms,
				session_id, session_identity, session_identity_kind, request_sequence,
				is_replay, blocked_by, blocked_reason, messages_count, special_settings,
				created_at
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7,
				$8, $9, $10, $11,
				0, $12::numeric, 1234, 55,
				$13, $14, $15, $16,
				false, $17, $18, 3, $19::jsonb,
				now() - ($20 * interval '1 second')
			) RETURNING id`,
			provider, fixtureUserID, fixtureKeyValue, model, model, model, endpoint,
			testCase.statusCode, testCase.inputTokens, testCase.outputTokens, testCase.cacheRead,
			cost, fixture.sessionID, fixture.sessionID, sessionIdentityKind, index+1,
			testCase.blockedBy, &blockedReason,
			`[{"type":"anthropic_effort","hit":true,"effort":"high"}]`,
			index,
		).Scan(&requestID)
		if err != nil {
			t.Fatalf("插入 message_request 夹具失败（index=%d）: %v", index, err)
		}
		fixture.requestIDs = append(fixture.requestIDs, requestID)
	}

	t.Cleanup(func() {
		// 用池删除自己插入的行；cleanup 为后进先出，故这两步先于池关闭执行。
		cleanupPool, err := pools.Control()
		if err != nil {
			t.Logf("清理夹具失败（取池）: %v", err)
			return
		}
		if _, err := cleanupPool.Exec(ctx, `DELETE FROM usage_ledger WHERE request_id = ANY($1)`,
			fixture.requestIDs); err != nil {
			t.Logf("清理 usage_ledger 夹具失败: %v", err)
		}
		if _, err := cleanupPool.Exec(ctx, `DELETE FROM message_request WHERE id = ANY($1)`,
			fixture.requestIDs); err != nil {
			t.Logf("清理 message_request 夹具失败: %v", err)
		}
	})
	return fixture
}

func ulIntPtr(value int) *int { return &value }

func ulIntToString(value int64) string { return strconv.FormatInt(value, 10) }

func ulBase64URLDecode(value string) ([]byte, error) {
	return base64.URLEncoding.DecodeString(value)
}

// ulRouter 建一个装配好的路由表（真实池 + 桩守卫）。
func ulRouter(t *testing.T, pools *store.Pools) *Router {
	t.Helper()
	deps := Deps{Guard: ulStubGuard{}, Problems: NewProblems(nil), Store: pools}
	router := New(Options{Deps: deps})
	RegisterUsageLogsWith(router, deps, UsageLogsOptions{})
	return router
}

// ulDoRequest 发一次 GET 并解出 JSON。
func ulDoRequest(t *testing.T, router *Router, target string) (int, map[string]any) {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON（状态 %d）: %s", recorder.Code, recorder.Body.String())
	}
	return recorder.Code, body
}

func TestIntegrationUsageLogsOffsetListMatchesNodeShape(t *testing.T) {
	pools := ulOpenPools(t)
	fixture := seedUsageLogFixture(t, pools)
	router := ulRouter(t, pools)

	status, body := ulDoRequest(t, router,
		"/api/v1/usage-logs?page=1&pageSize=10&model="+fixture.model)
	if status != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d: %+v", status, body)
	}
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items 应为数组: %+v", body)
	}
	if len(items) != 2 {
		t.Fatalf("应命中 2 行夹具，实际 %d: %+v", len(items), items)
	}
	// sourceSessionIdsByIdentity 在 includeSourceSessionIds 为真时总是存在（可能是空对象）。
	if _, ok := body["sourceSessionIdsByIdentity"]; !ok {
		t.Fatalf("响应应含 sourceSessionIdsByIdentity: %+v", body)
	}
	pageInfo, ok := body["pageInfo"].(map[string]any)
	if !ok {
		t.Fatalf("pageInfo 缺失: %+v", body)
	}
	if pageInfo["page"] != float64(1) || pageInfo["pageSize"] != float64(10) {
		t.Fatalf("pageInfo 不符: %+v", pageInfo)
	}
	if pageInfo["total"] != float64(2) || pageInfo["totalPages"] != float64(1) {
		t.Fatalf("pageInfo 计数不符: %+v", pageInfo)
	}

	// 第一行是 200 的那条（按 created_at DESC，index=0 的时间更新）。
	first := items[0].(map[string]any)
	if first["statusCode"] != float64(200) {
		t.Fatalf("首行应为未拦截的那条: %+v", first)
	}
	if first["totalTokens"] != float64(160) {
		t.Fatalf("totalTokens 应为 160（100+20+40），实际 %v", first["totalTokens"])
	}
	if first["anthropicEffort"] != "high" {
		t.Fatalf("anthropicEffort 应透出 high，实际 %v", first["anthropicEffort"])
	}
	// 夹具没有写 theoreticalCacheTokens/cacheScoreEligible/cacheScoreExcludedReason，
	// 三个 F3b 字段全空即 not_recorded（request-metrics.ts:78 的第一个分支）。
	if first["requestCacheMetricAvailability"] != "not_recorded" {
		t.Fatalf("F3b 字段全空时应为 not_recorded，实际 %v",
			first["requestCacheMetricAvailability"])
	}
	// numeric 在 Node 侧以小数字符串进 JSON。
	if first["costUsd"] != "0.250000000000000" {
		t.Fatalf("costUsd 应是原样的小数字符串，实际 %v", first["costUsd"])
	}
	if first["sessionId"] != fixture.sessionID {
		t.Fatalf("sessionId 不符: %v", first["sessionId"])
	}
	createdAt, _ := first["createdAt"].(string)
	if !strings.HasSuffix(createdAt, "Z") || !strings.Contains(createdAt, "T") {
		t.Fatalf("createdAt 应是 ISO 字符串: %v", createdAt)
	}
	if _, hasRaw := first["createdAtRaw"]; hasRaw {
		t.Fatalf("偏移路径不应含 createdAtRaw: %+v", first)
	}
	// 被拦截的那条应派生出 guard_intercept。
	second := items[1].(map[string]any)
	settings, ok := second["specialSettings"].([]any)
	if !ok || len(settings) == 0 {
		t.Fatalf("被拦截行应有 specialSettings: %+v", second)
	}
	// 顺序与 Node 一致：库里已有的在前，派生的在后（guard_intercept 是派生项）。
	if settings[0].(map[string]any)["type"] != "anthropic_effort" {
		t.Fatalf("库内设置应排在前: %+v", settings)
	}
	guardSetting := settings[len(settings)-1].(map[string]any)
	if guardSetting["type"] != "guard_intercept" || guardSetting["guard"] != "sensitive_word" {
		t.Fatalf("guard_intercept 派生不符: %+v", settings)
	}
	if guardSetting["statusCode"] != float64(403) || guardSetting["action"] != "block_request" {
		t.Fatalf("guard_intercept 字段不符: %+v", guardSetting)
	}

	// 按状态码过滤只应命中被拦截的那条（statusCode 优先于 excludeStatusCode200）。
	_, filtered := ulDoRequest(t, router,
		"/api/v1/usage-logs?page=1&model="+fixture.model+"&statusCode=403")
	filteredItems := filtered["items"].([]any)
	if len(filteredItems) != 1 {
		t.Fatalf("statusCode=403 应只命中 1 行，实际 %d", len(filteredItems))
	}
	if blocked := filteredItems[0].(map[string]any); blocked["blockedBy"] != "sensitive_word" {
		t.Fatalf("被拦截行应带 blockedBy: %+v", blocked)
	}

	// excludeStatusCode200 应排除 200 那条。
	_, excluded := ulDoRequest(t, router,
		"/api/v1/usage-logs?page=1&model="+fixture.model+"&excludeStatusCode200=true")
	excludedItems := excluded["items"].([]any)
	if len(excludedItems) != 1 {
		t.Fatalf("excludeStatusCode200 应命中 1 行，实际 %d", len(excludedItems))
	}
}

func TestIntegrationUsageLogsCursorListAndCursorRoundTrip(t *testing.T) {
	pools := ulOpenPools(t)
	fixture := seedUsageLogFixture(t, pools)
	router := ulRouter(t, pools)

	status, body := ulDoRequest(t, router, "/api/v1/usage-logs?limit=1&model="+fixture.model)
	if status != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d: %+v", status, body)
	}
	items := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("limit=1 应返回 1 行，实际 %d", len(items))
	}
	pageInfo := body["pageInfo"].(map[string]any)
	if pageInfo["hasMore"] != true {
		t.Fatalf("应还有下一页: %+v", pageInfo)
	}
	if pageInfo["limit"] != float64(1) {
		t.Fatalf("pageInfo.limit 应为 1，实际 %v", pageInfo["limit"])
	}
	cursor, _ := pageInfo["nextCursor"].(string)
	if cursor == "" {
		t.Fatalf("应给出 base64url 游标: %+v", pageInfo)
	}
	decoded := ulDecodeCursor(t, cursor)
	if decoded["id"] == nil || decoded["createdAt"] == nil {
		t.Fatalf("游标内容不符: %+v", decoded)
	}
	// 游标路径的行含 createdAtRaw（Node 多取了该列并原样透出）。
	first := items[0].(map[string]any)
	if _, ok := first["createdAtRaw"]; !ok {
		t.Fatalf("游标路径应含 createdAtRaw: %+v", first)
	}

	nextID := int64(decoded["id"].(float64))
	createdAt := decoded["createdAt"].(string)
	status, body = ulDoRequest(t, router, "/api/v1/usage-logs?limit=1&model="+fixture.model+
		"&cursorCreatedAt="+createdAt+"&cursorId="+ulIntToString(nextID))
	if status != http.StatusOK {
		t.Fatalf("带游标请求应为 200，实际 %d: %+v", status, body)
	}
	nextItems := body["items"].([]any)
	if len(nextItems) != 1 {
		t.Fatalf("第二页应返回 1 行，实际 %d", len(nextItems))
	}
	nextFirst := nextItems[0].(map[string]any)
	if nextFirst["id"] == first["id"] {
		t.Fatalf("第二页不该重复第一页的行: %v", nextFirst["id"])
	}
	// 夹具的两行 created_at 与自增 id 未必同序（插值时减了秒），故按时间比较。
	if nextFirst["createdAt"].(string) > first["createdAt"].(string) {
		t.Fatalf("第二页应是更早的行: %v > %v", nextFirst["createdAt"], first["createdAt"])
	}
	if body["pageInfo"].(map[string]any)["hasMore"] != false {
		t.Fatalf("第二页之后不应还有下一页: %+v", body["pageInfo"])
	}
}

func ulDecodeCursor(t *testing.T, cursor string) map[string]any {
	t.Helper()
	// base64url 无填充：补齐后再解。
	padded := cursor
	for len(padded)%4 != 0 {
		padded += "="
	}
	raw, err := ulBase64URLDecode(padded)
	if err != nil {
		t.Fatalf("游标不是 base64url: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("游标内层不是 JSON: %v", err)
	}
	return decoded
}

func TestIntegrationUsageLogsStatsUsesLedger(t *testing.T) {
	pools := ulOpenPools(t)
	fixture := seedUsageLogFixture(t, pools)
	router := ulRouter(t, pools)

	status, body := ulDoRequest(t, router, "/api/v1/usage-logs/stats?model="+fixture.model)
	if status != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d: %+v", status, body)
	}
	// 统计口径取 usage_ledger 且排除 blocked_by 非空的行：只应剩那条 200 的。
	if body["totalRequests"] != float64(1) {
		t.Fatalf("统计应来自 usage_ledger 且排除被拦截行，实际 %v", body["totalRequests"])
	}
	if body["totalCost"] != float64(0.25) {
		t.Fatalf("totalCost 应为 0.25，实际 %v", body["totalCost"])
	}
	if body["totalTokens"] != float64(160) {
		t.Fatalf("totalTokens 应为 160，实际 %v", body["totalTokens"])
	}
	if body["totalInputTokens"] != float64(100) || body["totalOutputTokens"] != float64(20) {
		t.Fatalf("输入/输出 token 不符: %+v", body)
	}

	// 非计费端点走 message_request 且成本恒 0、命中数为 0（夹具的 endpoint 不是该端点）。
	status, body = ulDoRequest(t, router,
		"/api/v1/usage-logs/stats?endpoint=/v1/messages/count_tokens&model="+fixture.model)
	if status != http.StatusOK {
		t.Fatalf("非计费端点统计应为 200，实际 %d: %+v", status, body)
	}
	if body["totalCost"] != float64(0) || body["totalRequests"] != float64(0) {
		t.Fatalf("非计费端点应零命中且成本为 0: %+v", body)
	}
}

func TestIntegrationUsageLogsFilterOptionsAndSuggestions(t *testing.T) {
	pools := ulOpenPools(t)
	fixture := seedUsageLogFixture(t, pools)
	router := ulRouter(t, pools)

	status, body := ulDoRequest(t, router, "/api/v1/usage-logs/filter-options")
	if status != http.StatusOK {
		t.Fatalf("filter-options 应为 200，实际 %d: %+v", status, body)
	}
	models, _ := body["models"].([]any)
	if !ulContainsString(models, fixture.model) {
		t.Fatalf("filter-options.models 应含夹具模型: %v", models)
	}
	if _, ok := body["statusCodes"].([]any); !ok {
		t.Fatalf("statusCodes 应为数组: %+v", body)
	}
	if _, ok := body["endpoints"].([]any); !ok {
		t.Fatalf("endpoints 应为数组: %+v", body)
	}

	status, body = ulDoRequest(t, router, "/api/v1/usage-logs/models")
	if status != http.StatusOK {
		t.Fatalf("models 应为 200，实际 %d", status)
	}
	if items, _ := body["items"].([]any); !ulContainsString(items, fixture.model) {
		t.Fatalf("models.items 应含夹具模型: %v", items)
	}

	status, body = ulDoRequest(t, router, "/api/v1/usage-logs/status-codes")
	if status != http.StatusOK {
		t.Fatalf("status-codes 应为 200，实际 %d", status)
	}
	if codes, _ := body["items"].([]any); !ulContainsNumber(codes, 200) ||
		!ulContainsNumber(codes, 403) {
		t.Fatalf("status-codes.items 应含 200 与 403: %v", codes)
	}

	status, body = ulDoRequest(t, router, "/api/v1/usage-logs/endpoints")
	if status != http.StatusOK {
		t.Fatalf("endpoints 应为 200，实际 %d", status)
	}
	if endpoints, _ := body["items"].([]any); !ulContainsString(endpoints, "/v1/messages") {
		t.Fatalf("endpoints.items 应含 /v1/messages: %v", endpoints)
	}

	// 联想：term 取夹具 sessionId 的前缀（长度 >= 2）。
	term := fixture.sessionID[:6]
	status, body = ulDoRequest(t, router, "/api/v1/usage-logs/session-id-suggestions?term="+term)
	if status != http.StatusOK {
		t.Fatalf("联想应为 200，实际 %d: %+v", status, body)
	}
	if suggestions, _ := body["items"].([]any); !ulContainsString(suggestions, fixture.sessionID) {
		t.Fatalf("联想应含夹具 sessionId: %v", suggestions)
	}

	// term 太短直接空数组（Node 的 minLen=2）。
	status, body = ulDoRequest(t, router, "/api/v1/usage-logs/session-id-suggestions?term=a")
	if status != http.StatusOK {
		t.Fatalf("短 term 应为 200，实际 %d", status)
	}
	if items, _ := body["items"].([]any); len(items) != 0 {
		t.Fatalf("短 term 应返回空数组: %v", items)
	}
}

func TestIntegrationUsageLogsRejectsInvalidQuery(t *testing.T) {
	pools := ulOpenPools(t)
	router := ulRouter(t, pools)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet,
		"/api/v1/usage-logs?limit=999", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("越界 limit 应为 400，实际 %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != problemContentType {
		t.Fatalf("Content-Type 应为 %s，实际 %s", problemContentType, got)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if body["errorCode"] != "request.validation_failed" {
		t.Fatalf("errorCode 不符: %v", body["errorCode"])
	}
	issues, _ := body["invalidParams"].([]any)
	if len(issues) != 1 {
		t.Fatalf("应有一条 invalidParams: %+v", body)
	}
	if issue := issues[0].(map[string]any); issue["code"] != "too_big" {
		t.Fatalf("issue.code 应为 too_big: %+v", issue)
	}
}

func ulContainsString(values []any, target string) bool {
	for _, value := range values {
		if text, ok := value.(string); ok && text == target {
			return true
		}
	}
	return false
}

func ulContainsNumber(values []any, target float64) bool {
	for _, value := range values {
		if number, ok := value.(float64); ok && number == target {
			return true
		}
	}
	return false
}

// TestIntegrationUsageLogsSessionIdentityFallback 钉住 Node 的 identity 语义：
// 响应里的 sessionId 是 COALESCE(session_identity, session_id)，sourceSessionId 是物理列。
// 该细节极易被写成「直接读 session_identity」，而在两者都非空的夹具上看不出差别。
func TestIntegrationUsageLogsSessionIdentityFallback(t *testing.T) {
	pools := ulOpenPools(t)
	ctx := context.Background()
	model := "go-admin-usage-logs-it-identity-" + time.Now().Format("20060102150405.000000000")
	physicalSessionID := "go-admin-it-physical-" + time.Now().Format("20060102150405.000000000")
	status := 200
	modelValue := model
	endpoint := "/v1/messages"
	cost := "0.000000000000000"

	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取 control 分道失败: %v", err)
	}
	var requestID int64
	// session_identity 留空，只写物理 session_id。
	err = pool.QueryRow(ctx, `
		INSERT INTO message_request (
			provider_id, user_id, key, model, endpoint, status_code, cost_usd,
			session_id, session_identity, session_identity_kind, is_replay, created_at
		) VALUES (1, $1, $2, $3, $4, $5, $6::numeric, $7, NULL, 'prefix_affinity', false, now())
		RETURNING id`,
		fixtureUserID, fixtureKeyValue, modelValue, endpoint, status, cost, physicalSessionID,
	).Scan(&requestID)
	if err != nil {
		t.Fatalf("插入夹具失败: %v", err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `DELETE FROM usage_ledger WHERE request_id = $1`,
			requestID); err != nil {
			t.Logf("清理 usage_ledger 夹具失败: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM message_request WHERE id = $1`,
			requestID); err != nil {
			t.Logf("清理 message_request 夹具失败: %v", err)
		}
	})

	router := ulRouter(t, pools)
	_, body := ulDoRequest(t, router, "/api/v1/usage-logs?page=1&model="+model)
	items := body["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("应命中 1 行，实际 %d", len(items))
	}
	row := items[0].(map[string]any)
	if row["sessionId"] != physicalSessionID {
		t.Fatalf("session_identity 为空时应回落到 session_id，实际 %v", row["sessionId"])
	}
	if row["sourceSessionId"] != physicalSessionID {
		t.Fatalf("sourceSessionId 应是物理列，实际 %v", row["sourceSessionId"])
	}
	if row["sessionIdentityKind"] != "prefix_affinity" {
		t.Fatalf("sessionIdentityKind 应透出，实际 %v", row["sessionIdentityKind"])
	}
}
