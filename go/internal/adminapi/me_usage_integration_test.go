package adminapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 **me 用量面**（/me/usage-logs*）的真实 PG 集成测试。
//
// 夹具复用 me_test.go 的 seedMeFixture（独立用户 + 独立密钥 + 自有账本行）：me/usage-logs 是按
// **密钥原文**取数的，主体之间只靠这个串区分，所以「看不到别人的行」必须用两个真实主体证明，
// 不能靠共享 loadtest 主体加筛选参数伪造。
//
// 另外两个夹具自带的事实值得先记住，否则断言会写错：
//   - 每个主体有**两条** message_request：一条可计费（进账本口径），一条被拦截（进 list，不进统计）。
//   - 账本行由 message_request 上的 AFTER INSERT 触发器写，测试不显式插账本。

// meUsagePrincipalFor 建一个注入该主体身份的路由表。
func meUsagePrincipalFor(t *testing.T, pools *store.Pools, keyID, userID int64) *Router {
	t.Helper()
	principal := Principal{KeyID: keyID, UserID: userID, Username: "me-usage-it"}
	router := New(Options{Deps: Deps{Guard: principalGuard{principal: principal}}})
	RegisterMeRoutes(router, meRuntimeDeps(pools, principal, 0, 0))
	return router
}

// meUsageInsertRow 插一条 message_request（key 为给定密钥原文），返回行 id。
//
// 与 seedMeFixture 的区别：这里要控制 **created_at** 与 key（跨密钥归属），故不复用它的插入。
func meUsageInsertRow(
	t *testing.T,
	pools *store.Pools,
	userID int64,
	keyValue string,
	model string,
	cost string,
	createdAt time.Time,
) int64 {
	t.Helper()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取 control 分道失败: %v", err)
	}
	var requestID int64
	err = pool.QueryRow(context.Background(), `
		INSERT INTO message_request (
			provider_id, user_id, key, model, original_model, endpoint,
			status_code, input_tokens, output_tokens, cost_usd, duration_ms, ttfb_ms,
			is_replay, created_at
		) VALUES (
			$1, $2, $3, $4, $4, '/v1/messages',
			200, 100, 20, $5::numeric, 1234, 55,
			false, $6
		) RETURNING id`,
		int64(1), userID, keyValue, model, cost, createdAt,
	).Scan(&requestID)
	if err != nil {
		t.Fatalf("插入用量行失败: %v", err)
	}
	return requestID
}

// meUsageItemModels 取出 items 里的 model 列表。
func meUsageItemModels(t *testing.T, body map[string]any) []string {
	t.Helper()
	items, ok := body["items"].([]any)
	if !ok {
		t.Fatalf("items 应为数组: %+v", body)
	}
	models := make([]string, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("item 应为对象: %+v", item)
		}
		value, _ := entry["model"].(string)
		models = append(models, value)
	}
	return models
}

// TestIntegrationMeUsageLogsScopedToOwnKey 钉住**密钥维度的隔离**：
// 库里同时存在另一个主体的行（夹具 B），A 的响应里必须看不到它。
func TestIntegrationMeUsageLogsScopedToOwnKey(t *testing.T) {
	pools := meOpenPools(t)
	fixtureA := seedMeFixture(t, pools, "meusage-a", "0.250000000000000")
	fixtureB := seedMeFixture(t, pools, "meusage-b", "0.750000000000000")
	router := meUsagePrincipalFor(t, pools, fixtureA.keyID, fixtureA.userID)

	status, body := meGet(t, router, "/api/v1/me/usage-logs")
	if status != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d: %+v", status, body)
	}
	models := meUsageItemModels(t, body)
	if len(models) != 2 {
		t.Fatalf("A 应只看到自己的两条行，实际 %d 条: %+v", len(models), models)
	}
	for _, model := range models {
		if strings.Contains(model, "meusage-b") || model == fixtureB.model {
			t.Fatalf("响应里出现了主体 B 的行: %q（整体 %+v）", model, models)
		}
	}

	// models 端点同样只答自己的取值域：B 的模型不得出现。
	status, body = meGet(t, router, "/api/v1/me/usage-logs/models")
	if status != http.StatusOK {
		t.Fatalf("models 状态码应为 200，实际 %d", status)
	}
	items, _ := body["items"].([]any)
	joined, _ := json.Marshal(items)
	if strings.Contains(string(joined), fixtureB.model) {
		t.Fatalf("models 端点泄漏了主体 B 的模型: %s", joined)
	}
	if !strings.Contains(string(joined), fixtureA.model) {
		t.Fatalf("models 端点应含主体 A 的模型，实际 %s", joined)
	}
}

// TestIntegrationMeUsageLogsCursorRoundTrip 钉住游标分页：hasMore / nextCursor 的形状
// （base64url 的 {createdAt,id}）以及「顺着游标取下一页不重不漏」。
func TestIntegrationMeUsageLogsCursorRoundTrip(t *testing.T) {
	pools := meOpenPools(t)
	fixture := seedMeFixture(t, pools, "meusage-cursor", "0.250000000000000")
	router := meUsagePrincipalFor(t, pools, fixture.keyID, fixture.userID)

	status, body := meGet(t, router, "/api/v1/me/usage-logs?limit=1")
	if status != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d: %+v", status, body)
	}
	firstModels := meUsageItemModels(t, body)
	if len(firstModels) != 1 {
		t.Fatalf("limit=1 应只回一行，实际 %+v", firstModels)
	}
	pageInfo, _ := body["pageInfo"].(map[string]any)
	if pageInfo == nil {
		t.Fatalf("缺 pageInfo: %+v", body)
	}
	if hasMore, _ := pageInfo["hasMore"].(bool); !hasMore {
		t.Fatalf("两条夹具行下 limit=1 应 hasMore=true: %+v", pageInfo)
	}
	if limit, _ := pageInfo["limit"].(float64); limit != 1 {
		t.Fatalf("pageInfo.limit 应回显请求值 1，实际 %v", pageInfo["limit"])
	}

	rawCursor, _ := pageInfo["nextCursor"].(string)
	if rawCursor == "" {
		t.Fatalf("hasMore 时应给出 nextCursor: %+v", pageInfo)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(rawCursor)
	if err != nil {
		t.Fatalf("游标应为 base64url（无填充）: %v", err)
	}
	var cursor struct {
		CreatedAt string `json:"createdAt"`
		ID        int64  `json:"id"`
	}
	if err := json.Unmarshal(decoded, &cursor); err != nil {
		t.Fatalf("游标正文应为 {createdAt,id}: %s", decoded)
	}
	if cursor.ID == 0 || cursor.CreatedAt == "" {
		t.Fatalf("游标正文缺字段: %s", decoded)
	}

	// 顺着游标取下一页：应拿到另一行（两条夹具行互不相同）。
	status, body = meGet(t, router, "/api/v1/me/usage-logs?limit=1&cursorCreatedAt="+
		url.QueryEscape(cursor.CreatedAt)+"&cursorId="+ulIntToString(cursor.ID))
	if status != http.StatusOK {
		t.Fatalf("第二页状态码应为 200，实际 %d: %+v", status, body)
	}
	secondModels := meUsageItemModels(t, body)
	if len(secondModels) != 1 || secondModels[0] == firstModels[0] {
		t.Fatalf("第二页应给出另一行，实际 %+v（第一页 %+v）", secondModels, firstModels)
	}
}

// TestIntegrationMeUsageLogsOffsetPaginationCarriesTotal 钉住偏移分支的 pageInfo：
// page/pageSize/total/totalPages 四项都要在，且 total 计的是**匹配行数**而不是本页行数。
func TestIntegrationMeUsageLogsOffsetPaginationCarriesTotal(t *testing.T) {
	pools := meOpenPools(t)
	fixture := seedMeFixture(t, pools, "meusage-offset", "0.250000000000000")
	router := meUsagePrincipalFor(t, pools, fixture.keyID, fixture.userID)

	status, body := meGet(t, router, "/api/v1/me/usage-logs?page=1&pageSize=1")
	if status != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d: %+v", status, body)
	}
	if models := meUsageItemModels(t, body); len(models) != 1 {
		t.Fatalf("pageSize=1 应只回一行，实际 %+v", models)
	}
	pageInfo, _ := body["pageInfo"].(map[string]any)
	if pageInfo == nil {
		t.Fatalf("缺 pageInfo: %+v", body)
	}
	if total, _ := pageInfo["total"].(float64); total != 2 {
		t.Fatalf("total 应为匹配到 2 行（可计费 + 被拦截），实际 %v", pageInfo["total"])
	}
	if totalPages, _ := pageInfo["totalPages"].(float64); totalPages != 2 {
		t.Fatalf("totalPages 应为 2，实际 %v", pageInfo["totalPages"])
	}
	if page, _ := pageInfo["page"].(float64); page != 1 {
		t.Fatalf("page 应为 1，实际 %v", pageInfo["page"])
	}
	if _, ok := pageInfo["nextCursor"]; ok {
		t.Fatalf("偏移分支不该带游标: %+v", pageInfo)
	}
}

// TestIntegrationMeUsageLogsLedgerFallbackAfterSoftDelete 钉住两源合并的**去重条件**：
// message 行软删后，同一行应由账本支补上（而不是消失，也不是变成两行）。
func TestIntegrationMeUsageLogsLedgerFallbackAfterSoftDelete(t *testing.T) {
	pools := meOpenPools(t)
	fixture := seedMeFixture(t, pools, "meusage-ledger", "0.250000000000000")

	// 先确认可计费行只出现一次（账本行被 not exists 条件抑制）。
	before := store.MeUsageSlimFilters{KeyString: fixture.keyValue, Model: fixture.model}
	count, err := pools.CountMeUsageLogsForKey(context.Background(), before)
	if err != nil {
		t.Fatalf("计数失败: %v", err)
	}
	if count != 1 {
		t.Fatalf("去重后应只有 1 行，实际 %d", count)
	}

	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取 control 分道失败: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE message_request SET deleted_at = now() WHERE id = $1`, fixture.request); err != nil {
		t.Fatalf("软删夹具行失败: %v", err)
	}

	count, err = pools.CountMeUsageLogsForKey(context.Background(), before)
	if err != nil {
		t.Fatalf("软删后计数失败: %v", err)
	}
	if count != 1 {
		t.Fatalf("软删后账本支应补上同一行（仍为 1），实际 %d", count)
	}
	batch, err := pools.FindMeUsageLogsForKeyBatch(context.Background(), before, nil, 10)
	if err != nil {
		t.Fatalf("软删后取行失败: %v", err)
	}
	if len(batch.Logs) != 1 || batch.Logs[0].ID != fixture.request {
		t.Fatalf("账本支应回同一 request_id=%d，实际 %+v", fixture.request, batch.Logs)
	}
	if batch.Logs[0].CostUSD == nil {
		t.Fatalf("账本支应带回成本列: %+v", batch.Logs[0])
	}
}

// TestIntegrationMeStatsSummarySplitsKeyAndUser 钉住统计摘要的**两个维度**：
// 合计只算当前密钥，用户分解跨该用户的全部密钥——同一个用户放两条密钥行才能验出来。
func TestIntegrationMeStatsSummarySplitsKeyAndUser(t *testing.T) {
	pools := meOpenPools(t)
	fixture := seedMeFixture(t, pools, "meusage-stats", "0.250000000000000")

	// 同一用户的第二把密钥 + 一条自己的行：它必须出现在 userModelBreakdown 里，
	// 但不得进入 keyModelBreakdown 与合计。
	otherKeyID, otherKeyValue := fixtureKey(t, pools, fixtureKeyOptions{
		userID:        fixture.userID,
		canLoginWebUI: true,
		isEnabled:     true,
	})
	otherModel := fixture.model + "-other-key"
	meUsageInsertRow(t, pools, fixture.userID, otherKeyValue, otherModel, "0.500000000000000",
		time.Now().Add(-1*time.Minute))
	t.Cleanup(func() {
		cleanup, err := pools.Control()
		if err != nil {
			return
		}
		_, _ = cleanup.Exec(context.Background(),
			`DELETE FROM usage_ledger WHERE request_id IN (SELECT id FROM message_request WHERE key = $1)`,
			otherKeyValue)
		_, _ = cleanup.Exec(context.Background(),
			`DELETE FROM message_request WHERE key = $1`, otherKeyValue)
		_, _ = cleanup.Exec(context.Background(), `DELETE FROM keys WHERE id = $1`, otherKeyID)
	})

	router := meUsagePrincipalFor(t, pools, fixture.keyID, fixture.userID)
	status, body := meGet(t, router, "/api/v1/me/usage-logs/stats-summary")
	if status != http.StatusOK {
		t.Fatalf("状态码应为 200，实际 %d: %+v", status, body)
	}

	keyBreakdown, _ := body["keyModelBreakdown"].([]any)
	userBreakdown, _ := body["userModelBreakdown"].([]any)
	keyJSON, _ := json.Marshal(keyBreakdown)
	userJSON, _ := json.Marshal(userBreakdown)
	if strings.Contains(string(keyJSON), otherModel) {
		t.Fatalf("keyModelBreakdown 不该含另一把密钥的模型: %s", keyJSON)
	}
	if !strings.Contains(string(userJSON), otherModel) {
		t.Fatalf("userModelBreakdown 应含同一用户另一把密钥的模型: %s", userJSON)
	}
	if !strings.Contains(string(keyJSON), fixture.model) {
		t.Fatalf("keyModelBreakdown 应含当前密钥的模型: %s", keyJSON)
	}

	// 合计只反映当前密钥的行：0.25（可计费行）+ 0.5 的行**不得**计入。
	if cost := meNumber(t, body, "totalCost"); cost != 0.25 {
		t.Fatalf("totalCost 应为 0.25（只算当前密钥），实际 %v", cost)
	}
	if requests := meNumber(t, body, "totalRequests"); requests != 1 {
		t.Fatalf("totalRequests 应为 1，实际 %v", requests)
	}
	// token 合计 = 可计费行的 100 + 20（被拦截行不进账本口径）。
	if tokens := meNumber(t, body, "totalTokens"); tokens != 120 {
		t.Fatalf("totalTokens 应为 120，实际 %v", tokens)
	}
	if _, ok := body["currencyCode"]; !ok {
		t.Fatalf("响应应含 currencyCode: %+v", body)
	}
}
