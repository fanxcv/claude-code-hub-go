package adminapi

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 dashboard **实时面**两条端点（proxy-status / realtime）的真实 PG 集成测试。
//
// 夹具与 dashboard_integration_test.go 同一套（fixtureUser/fixtureKey/meOpenPools），并按该文件
// 的三条纪律：唯一 model 名前缀定位、按精确 key 清理、不翻 allowGlobalUsageView 这类共享设置。
//
// 此处额外钉的是**写读闭环**：proxy-status 的真源就是库里的请求行状态（Node 的
// ProxyStatusTracker 是库内聚合，startRequest/endRequest 是空实现），故「写」侧就是请求行的
// 生命周期——未终局行出现在 activeRequests，改成终局后立刻落到 lastRequest。这比注入一个替身
// 更能证明这条链是通的。

// liveRequestRow 是实时面夹具的一行请求。
type liveRequestRow struct {
	model      string
	sessionID  *string
	statusCode *int
	durationMS *int
	cost       string
	// ageMinutes 是 created_at 相对现在的分钟数（越大越旧）。
	ageMinutes int
}

// seedLiveRequests 建「用户 + 密钥 + 请求行」，返回 userID、keyValue 与清理用前缀。
//
// 故意不复用 seedDashboardSubject：那条夹具把 status_code 钉死为 200，而实时面要的正是
// 「未终局」这一半状态。其余口径（唯一前缀、精确清理、账本先行）保持一致。
func seedLiveRequests(
	t *testing.T,
	pools *store.Pools,
	label string,
	rows []liveRequestRow,
) (int64, string, string) {
	t.Helper()
	ctx := context.Background()
	prefix := fmt.Sprintf("go-adminapi-live-it-%s-%d", label, time.Now().UnixNano())
	userID := fixtureUser(t, pools, "user", true)
	_, keyValue := fixtureKey(t, pools, fixtureKeyOptions{
		userID:        userID,
		canLoginWebUI: true,
		isEnabled:     true,
	})

	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取 control 分道失败: %v", err)
	}
	for index, row := range rows {
		model := row.model
		if model == "" {
			model = fmt.Sprintf("%s-%d", prefix, index)
		}
		cost := row.cost
		if cost == "" {
			cost = "0"
		}
		// 终局的「最近一次」判据读 updated_at，故终局行的 updated_at 必须与 created_at 同源。
		_, err := pool.Exec(ctx, `
			INSERT INTO message_request (
				provider_id, user_id, key, model, original_model, endpoint,
				session_id, status_code, duration_ms, cost_usd, is_replay,
				created_at, updated_at
			) VALUES (
				$1, $2, $3, $4, $4, '/v1/messages',
				$5, $6, $7, $8::numeric, false,
				now() - ($9::int * interval '1 minute'),
				now() - ($9::int * interval '1 minute')
			)`,
			int64(1), userID, keyValue, model, row.sessionID, row.statusCode,
			row.durationMS, cost, row.ageMinutes,
		)
		if err != nil {
			t.Fatalf("插入实时面夹具行失败: %v", err)
		}
	}

	t.Cleanup(func() {
		cleanupPool, cleanupErr := pools.Control()
		if cleanupErr != nil {
			return
		}
		// 触发器只写不删：先删账本再删请求行。
		_, _ = cleanupPool.Exec(ctx,
			`DELETE FROM usage_ledger WHERE request_id IN
				(SELECT id FROM message_request WHERE key = $1)`, keyValue)
		_, _ = cleanupPool.Exec(ctx, `DELETE FROM message_request WHERE key = $1`, keyValue)
	})
	return userID, keyValue, prefix
}

// liveRequestIDOf 取某 key 下指定 session_id 的请求行 id（用于断言 lastRequest.requestId）。
func liveRequestIDOf(t *testing.T, pools *store.Pools, keyValue string, sessionID string) int64 {
	t.Helper()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取 control 分道失败: %v", err)
	}
	var id int64
	if err := pool.QueryRow(context.Background(),
		`SELECT id FROM message_request WHERE key = $1 AND session_id = $2
		 ORDER BY id DESC LIMIT 1`, keyValue, sessionID).Scan(&id); err != nil {
		t.Fatalf("取请求行 id 失败: %v", err)
	}
	return id
}

// liveMarkTerminal 把一行改成终局（写侧的状态迁移）。
func liveMarkTerminal(t *testing.T, pools *store.Pools, requestID int64, statusCode int) {
	t.Helper()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取 control 分道失败: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE message_request SET status_code = $2, duration_ms = 4321, updated_at = now()
		 WHERE id = $1`, requestID, statusCode); err != nil {
		t.Fatalf("改成终局失败: %v", err)
	}
}

// dashboardProxyStatusUserOf 在作答里按 userId 找一行。
func dashboardProxyStatusUserOf(
	t *testing.T,
	body map[string]any,
	userID int64,
) map[string]any {
	t.Helper()
	users, ok := body["users"].([]any)
	if !ok {
		t.Fatalf("proxy-status 应作答 users 数组，实际 %T", body["users"])
	}
	for _, raw := range users {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if value, ok := entry["userId"].(float64); ok && int64(value) == userID {
			return entry
		}
	}
	t.Fatalf("作答里找不到 userId=%d 的行", userID)
	return nil
}

// TestDashboardProxyStatusWriteReadClosure 钉住写读闭环与两处字段映射。
//
// 三处必须成立的语义：
//  1. 未终局行（status_code IS NULL）出现在 activeRequests，activeCount 与之一致。
//  2. 该行改成终局后，立刻从 activeRequests 消失并落到 lastRequest（同一个 requestId）。
//  3. model 走 `|| "unknown"`、keyName 走 `?? maskKey(key)`——两处兜底不是同一种语义。
func TestDashboardProxyStatusWriteReadClosure(t *testing.T) {
	pools := meOpenPools(t)
	sessionID := fmt.Sprintf("go-adminapi-live-session-%d", time.Now().UnixNano())
	userID, keyValue, prefix := seedLiveRequests(t, pools, "proxy-status", []liveRequestRow{
		{model: "", sessionID: &sessionID, cost: "0.25", ageMinutes: 5},
	})
	modelName := prefix + "-0"
	requestID := liveRequestIDOf(t, pools, keyValue, sessionID)

	admin := Principal{UserID: userID, Username: "live-it", IsAdmin: true}
	router := dashboardRouter(t, pools, admin, &fakeObservedSessions{})

	status, body, raw := dashboardGet(t, router, "/dashboard/proxy-status")
	if status != http.StatusOK {
		t.Fatalf("proxy-status 状态码应为 200，实际 %d：%s", status, raw)
	}
	entry := dashboardProxyStatusUserOf(t, body, userID)
	if count, _ := entry["activeCount"].(float64); int(count) != 1 {
		t.Fatalf("活跃数应为 1，实际 %v：%s", entry["activeCount"], raw)
	}
	active, ok := entry["activeRequests"].([]any)
	if !ok || len(active) != 1 {
		t.Fatalf("活跃列表应有一条，实际 %v", entry["activeRequests"])
	}
	activeRow, _ := active[0].(map[string]any)
	if value, _ := activeRow["requestId"].(float64); int64(value) != requestID {
		t.Errorf("活跃行的 requestId 应为 %d，实际 %v", requestID, activeRow["requestId"])
	}
	// 模型名带前缀且非空，故走「原值」分支；未终局行的 providerName 也照实作答。
	if activeRow["model"] != modelName {
		t.Errorf("活跃行的 model 应原样作答，实际 %v", activeRow["model"])
	}
	if activeRow["keyName"] == nil || activeRow["keyName"] == "" {
		t.Errorf("活跃行的 keyName 不应为空（应有遮罩兜底），实际 %v", activeRow["keyName"])
	}
	if entry["lastRequest"] != nil {
		t.Errorf("此刻还不该有 lastRequest，实际 %v", entry["lastRequest"])
	}

	// 写侧迁移：终局。
	liveMarkTerminal(t, pools, requestID, 200)

	_, body, raw = dashboardGet(t, router, "/dashboard/proxy-status")
	entry = dashboardProxyStatusUserOf(t, body, userID)
	if count, _ := entry["activeCount"].(float64); int(count) != 0 {
		t.Fatalf("终局后活跃数应为 0，实际 %v：%s", entry["activeCount"], raw)
	}
	active, ok = entry["activeRequests"].([]any)
	if !ok || len(active) != 0 {
		t.Errorf("终局后活跃列表应为空数组，实际 %v", entry["activeRequests"])
	}
	last, ok := entry["lastRequest"].(map[string]any)
	if !ok {
		t.Fatalf("终局后应有 lastRequest，实际 %v", entry["lastRequest"])
	}
	if value, _ := last["requestId"].(float64); int64(value) != requestID {
		t.Errorf("lastRequest.requestId 应为 %d，实际 %v", requestID, last["requestId"])
	}
	if last["elapsed"] == nil || last["endTime"] == nil {
		t.Errorf("lastRequest 应有 endTime/elapsed，实际 %v", last)
	}
}

// TestDashboardRealtimeShapeAndActivityStream 钉住作答的七个键与活动流的映射。
//
// 活动流这一半走**真实身份链**：替身给出一个活跃 identity，夹具里那条未终局行正好命中了它，
// 故断言的是「Redis identity → 库内该 identity 的最新请求 → 逐字段映射」这条完整路径。
func TestDashboardRealtimeShapeAndActivityStream(t *testing.T) {
	pools := meOpenPools(t)
	sessionID := fmt.Sprintf("go-adminapi-live-realtime-%d", time.Now().UnixNano())
	userID, _, prefix := seedLiveRequests(t, pools, "realtime", []liveRequestRow{
		{
			model:     "",
			sessionID: &sessionID,
			cost:      "1.5",
			// 未终局：provider 字段应留空、status 应为 0（Node 的 isFinalized 判据）。
			ageMinutes: 2,
		},
	})
	modelName := prefix + "-0"

	admin := Principal{UserID: userID, Username: "live-it", IsAdmin: true}
	router := dashboardRouter(t, pools, admin,
		&fakeObservedSessions{count: 1, identities: []string{sessionID}})

	status, body, raw := dashboardGet(t, router, "/dashboard/realtime")
	if status != http.StatusOK {
		t.Fatalf("realtime 状态码应为 200，实际 %d：%s", status, raw)
	}
	for _, key := range []string{
		"metrics", "activityStream", "userRankings", "providerRankings",
		"providerSlots", "modelDistribution", "trendData",
	} {
		if _, ok := body[key]; !ok {
			t.Fatalf("realtime 作答缺键 %s：%s", key, raw)
		}
	}

	metrics, ok := body["metrics"].(map[string]any)
	if !ok {
		t.Fatalf("metrics 应为对象，实际 %T", body["metrics"])
	}
	for _, key := range []string{
		"concurrentSessions", "todayRequests", "todayCost", "avgResponseTime",
		"todayErrorRate", "yesterdaySamePeriodRequests", "yesterdaySamePeriodCost",
		"yesterdaySamePeriodAvgResponseTime", "recentMinuteRequests",
	} {
		if _, ok := metrics[key]; !ok {
			t.Errorf("metrics 缺键 %s", key)
		}
	}

	stream, ok := body["activityStream"].([]any)
	if !ok {
		t.Fatalf("activityStream 应为数组，实际 %T", body["activityStream"])
	}
	if len(stream) == 0 {
		t.Fatalf("活动流不应为空（夹具里有未终局行且 identity 命中）：%s", raw)
	}
	found := false
	for _, rawItem := range stream {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		if item["id"] != sessionID {
			continue
		}
		found = true
		// 未终局：供应商留空、状态 0。
		if item["provider"] != "" {
			t.Errorf("未终局行的 provider 应为空串，实际 %v", item["provider"])
		}
		if value, _ := item["status"].(float64); value != 0 {
			t.Errorf("未终局行的 status 应为 0，实际 %v", item["status"])
		}
		// 成本按 parseFloat 出数（numeric 文本 → float64）。
		if value, _ := item["cost"].(float64); value != 1.5 {
			t.Errorf("活动的 cost 应为 1.5，实际 %v", item["cost"])
		}
		if item["model"] != modelName {
			t.Errorf("活动的 model 应为计费模型（original_model 优先），实际 %v", item["model"])
		}
		if item["user"] == "" || item["user"] == nil {
			t.Errorf("活动的 user 不应为空，实际 %v", item["user"])
		}
	}
	if !found {
		t.Fatalf("活动流里找不到 sessionId=%s 的条目：%s", sessionID, raw)
	}

	trend, ok := body["trendData"].([]any)
	if !ok {
		t.Fatalf("trendData 应为数组，实际 %T", body["trendData"])
	}
	for _, rawPoint := range trend {
		point, ok := rawPoint.(map[string]any)
		if !ok {
			continue
		}
		if _, ok := point["hour"]; !ok {
			t.Fatalf("trendData 元素应含 hour，实际 %v", point)
		}
		if _, ok := point["value"]; !ok {
			t.Fatalf("trendData 元素应含 value，实际 %v", point)
		}
	}

	// 插槽上限 3 条、且只保留设了并发限额的供应商（夹具供应商未设限额，故为空数组而非 null）。
	if slots, ok := body["providerSlots"].([]any); !ok {
		t.Errorf("providerSlots 应为数组，实际 %T", body["providerSlots"])
	} else if len(slots) > 3 {
		t.Errorf("providerSlots 最多 3 条，实际 %d", len(slots))
	}
}
