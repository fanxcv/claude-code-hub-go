package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 providers 运行态子面（熔断健康与复位，providers_health.go）的真实 PG + 真实 Redis
// 集成测试。夹具纪律与 providers_test.go 同法：名称带唯一前缀、按前缀清理。
//
// 为什么用**真实** NewRedisCircuitStates 而不是替身：本子面的全部价值在于「键布局与
// 字段序列化同 Node 逐字一致」（circuit-breaker-state.ts:11-82），替身正好会把这件事
// 测掉。真 Redis 下写/读/复位一轮，才算验过。

// providersHealthRouter 造真路由表：真 Store + 真 Redis 熔断面 + 固定管理员身份。
func providersHealthRouter(t *testing.T, pools *store.Pools, states CircuitStateStore) *Router {
	t.Helper()
	deps := Deps{
		Logger:        logx.New(nil),
		Guard:         principalGuard{principal: Principal{UserID: 1, Username: "admin", IsAdmin: true}},
		Store:         pools,
		Problems:      NewProblems(nil),
		CircuitStates: states,
	}
	router := New(Options{Deps: deps})
	RegisterProviders(router, deps)
	return router
}

// providersHealthSnapshot 承接 /providers/health 的响应体（键是供应商 id 的字符串形式）。
type providersHealthSnapshot map[string]struct {
	CircuitState     string `json:"circuitState"`
	FailureCount     int64  `json:"failureCount"`
	LastFailureTime  *int64 `json:"lastFailureTime"`
	CircuitOpenUntil *int64 `json:"circuitOpenUntil"`
	RecoveryMinutes  *int64 `json:"recoveryMinutes"`
	// 等待阶梯的两个加性字段 + 本次窗口时长（Go 侧增强，Node 无这三项）。
	ConsecutiveOpenCount          int64  `json:"consecutiveOpenCount"`
	ConsecutiveOpenCountChangedAt *int64 `json:"consecutiveOpenCountChangedAt"`
	OpenWindowMinutes             *int64 `json:"openWindowMinutes"`
}

func decodeProvidersHealth(t *testing.T, body string) providersHealthSnapshot {
	t.Helper()
	var snapshot providersHealthSnapshot
	if err := json.Unmarshal([]byte(body), &snapshot); err != nil {
		t.Fatalf("解析 health 响应失败: %v（原文 %.300s）", err, body)
	}
	return snapshot
}

// TestProvidersHealthReadsNodeCircuitLayout 钉住「读」这一侧：hash 布局、缺键即闭态、
// recoveryMinutes 的 ceil 口径、以及可见性过滤（隐藏类型只在 compat 请求里出现）。
func TestProvidersHealthReadsNodeCircuitLayout(t *testing.T) {
	pools := testPools(t)
	fixture := seedProviders(t, pools)
	client := circuitTestRedis(t)
	ctx := context.Background()

	openUntil := time.Now().Add(5 * time.Minute).UnixMilli()
	lastFailure := time.Now().Add(-2 * time.Minute).UnixMilli()
	key := providerCircuitKey(fixture.enabledID)
	// 写的是 Node 的形态：null 用空串（serializeState:56-68）。
	if err := client.HSet(ctx, key, map[string]string{
		"failureCount":         "3",
		"lastFailureTime":      strconv.FormatInt(lastFailure, 10),
		"circuitState":         "open",
		"circuitOpenUntil":     strconv.FormatInt(openUntil, 10),
		"halfOpenSuccessCount": "1",
	}).Err(); err != nil {
		t.Fatalf("写熔断夹具失败: %v", err)
	}
	t.Cleanup(func() { _ = client.Del(context.Background(), key).Err() })

	router := providersHealthRouter(t, pools, NewRedisCircuitStates(client, nil, nil))

	status, body := circuitCall(t, router, http.MethodGet, "/providers/health", "", true)
	if status != http.StatusOK {
		t.Fatalf("health 应为 200，收到 %d：%.300s", status, body)
	}
	snapshot := decodeProvidersHealth(t, body)

	opened, ok := snapshot[strconv.FormatInt(fixture.enabledID, 10)]
	if !ok {
		t.Fatalf("health 应含可见供应商 %d，实际键：%v", fixture.enabledID, snapshot)
	}
	if opened.CircuitState != "open" {
		t.Errorf("circuitState 应为 open，收到 %q", opened.CircuitState)
	}
	if opened.FailureCount != 3 {
		t.Errorf("failureCount 应为 3，收到 %d", opened.FailureCount)
	}
	if opened.CircuitOpenUntil == nil || *opened.CircuitOpenUntil != openUntil {
		t.Errorf("circuitOpenUntil 应为 %d，收到 %v", openUntil, opened.CircuitOpenUntil)
	}
	if opened.LastFailureTime == nil || *opened.LastFailureTime != lastFailure {
		t.Errorf("lastFailureTime 应为 %d，收到 %v", lastFailure, opened.LastFailureTime)
	}
	// recoveryMinutes = ceil((openUntil-now)/60000) → 5 分钟内应落在 1..5。
	if opened.RecoveryMinutes == nil || *opened.RecoveryMinutes < 1 || *opened.RecoveryMinutes > 5 {
		t.Errorf("recoveryMinutes 应在 1..5，收到 %v", opened.RecoveryMinutes)
	}

	// 无 Redis 键的供应商 → 出厂闭态（Node 的 DEFAULT_CIRCUIT_STATE），且 recoveryMinutes 为 null。
	hidden, ok := snapshot[strconv.FormatInt(fixture.otherID, 10)]
	if !ok {
		t.Fatalf("compat 请求下隐藏类型供应商 %d 也应出现", fixture.otherID)
	}
	if hidden.CircuitState != "closed" || hidden.FailureCount != 0 ||
		hidden.CircuitOpenUntil != nil || hidden.RecoveryMinutes != nil {
		t.Errorf("无键供应商应报出厂闭态且 recoveryMinutes 为 null，收到 %+v", hidden)
	}

	// 非 compat 请求：隐藏类型供应商被可见性过滤掉（handlers.ts:228-236 的 visibleIds 过滤）。
	status, body = circuitCall(t, router, http.MethodGet, "/providers/health", "", false)
	if status != http.StatusOK {
		t.Fatalf("非 compat health 应为 200，收到 %d：%.300s", status, body)
	}
	filtered := decodeProvidersHealth(t, body)
	if _, exists := filtered[strconv.FormatInt(fixture.otherID, 10)]; exists {
		t.Errorf("非 compat 请求不应含隐藏类型供应商 %d", fixture.otherID)
	}
	if _, exists := filtered[strconv.FormatInt(fixture.enabledID, 10)]; !exists {
		t.Errorf("非 compat 请求仍应含可见供应商 %d", fixture.enabledID)
	}
}

// TestProvidersCircuitResetWritesClosedState 钉住「复位」这一侧：单条与批量的响应体，
// 以及复位后的 Redis 形态（闭态覆盖写 + TTL 续期，而不是删键）。
func TestProvidersCircuitResetWritesClosedState(t *testing.T) {
	pools := testPools(t)
	fixture := seedProviders(t, pools)
	client := circuitTestRedis(t)
	ctx := context.Background()
	key := providerCircuitKey(fixture.enabledID)

	seedOpen := func() {
		t.Helper()
		if err := client.HSet(ctx, key, map[string]string{
			"failureCount":         "7",
			"lastFailureTime":      strconv.FormatInt(time.Now().UnixMilli(), 10),
			"circuitState":         "open",
			"circuitOpenUntil":     strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10),
			"halfOpenSuccessCount": "2",
			// 阶梯两键也写上：复位是 HSet 覆盖写，若不同时归零，复位后的供应商会被读成
			// 「第 2 阶」而状态是 closed——一处自相矛盾的展示（本用例钉住它）。
			"consecutiveOpenCount":          "2",
			"consecutiveOpenCountChangedAt": strconv.FormatInt(time.Now().UnixMilli(), 10),
		}).Err(); err != nil {
			t.Fatalf("写熔断夹具失败: %v", err)
		}
	}
	seedOpen()
	t.Cleanup(func() { _ = client.Del(context.Background(), key).Err() })

	router := providersHealthRouter(t, pools, NewRedisCircuitStates(client, nil, nil))

	status, body := circuitCall(t, router, http.MethodPost,
		"/providers/"+strconv.FormatInt(fixture.enabledID, 10)+"/circuit:reset", "", true)
	if status != http.StatusOK {
		t.Fatalf("单条复位应为 200，收到 %d：%.300s", status, body)
	}
	var resetBody map[string]any
	if err := json.Unmarshal([]byte(body), &resetBody); err != nil {
		t.Fatalf("解析复位响应失败: %v（原文 %.200s）", err, body)
	}
	if resetBody["ok"] != true {
		t.Errorf("单条复位应回 {\"ok\":true}，收到 %.200s", body)
	}

	raw, err := client.HGetAll(ctx, key).Result()
	if err != nil {
		t.Fatalf("读回熔断键失败: %v", err)
	}
	if len(raw) == 0 {
		t.Fatalf("复位后键不应消失（Node 是覆盖写闭态，不是 DEL）")
	}
	if raw["circuitState"] != "closed" || raw["failureCount"] != "0" ||
		raw["lastFailureTime"] != "" || raw["circuitOpenUntil"] != "" ||
		raw["halfOpenSuccessCount"] != "0" ||
		raw["consecutiveOpenCount"] != "0" || raw["consecutiveOpenCountChangedAt"] != "" {
		t.Errorf("复位后应为出厂闭态（null 用空串、阶梯归零），收到 %v", raw)
	}
	if ttl := client.TTL(ctx, key).Val(); ttl <= 0 {
		t.Errorf("复位后应续 24h TTL，收到 %s", ttl)
	}

	// 批量复位：响应体是 action 的 data（{resetCount}）。
	seedOpen()
	status, body = circuitCall(t, router, http.MethodPost, "/providers/circuits:batchReset",
		fmt.Sprintf(`{"providerIds":[%d]}`, fixture.enabledID), true)
	if status != http.StatusOK {
		t.Fatalf("批量复位应为 200，收到 %d：%.300s", status, body)
	}
	var batchBody struct {
		ResetCount int `json:"resetCount"`
	}
	if err := json.Unmarshal([]byte(body), &batchBody); err != nil {
		t.Fatalf("解析批量复位响应失败: %v（原文 %.200s）", err, body)
	}
	if batchBody.ResetCount != 1 {
		t.Errorf("resetCount 应为 1，收到 %d", batchBody.ResetCount)
	}
	if state := client.HGet(ctx, key, "circuitState").Val(); state != "closed" {
		t.Errorf("批量复位后 circuitState 应为 closed，收到 %q", state)
	}
}

// TestProvidersCircuitResetRejectsInvalidTargets 钉住前置校验：不存在的 id 一律 404
// （Node 的 ensureVisibleProviderIds 是「每个 id 都必须可见」），空数组 400。
func TestProvidersCircuitResetRejectsInvalidTargets(t *testing.T) {
	pools := testPools(t)
	client := circuitTestRedis(t)
	router := providersHealthRouter(t, pools, NewRedisCircuitStates(client, nil, nil))

	const missingID = 999999999
	status, body := circuitCall(t, router, http.MethodPost,
		fmt.Sprintf("/providers/%d/circuit:reset", missingID), "", true)
	if status != http.StatusNotFound {
		t.Errorf("不存在的供应商应 404，收到 %d：%.200s", status, body)
	}

	status, body = circuitCall(t, router, http.MethodPost, "/providers/circuits:batchReset",
		fmt.Sprintf(`{"providerIds":[%d]}`, missingID), true)
	if status != http.StatusNotFound {
		t.Errorf("批量里含不存在 id 应整批 404，收到 %d：%.200s", status, body)
	}

	status, body = circuitCall(t, router, http.MethodPost, "/providers/circuits:batchReset",
		`{"providerIds":[]}`, true)
	if status != http.StatusBadRequest {
		t.Errorf("空 providerIds 应 400，收到 %d：%.200s", status, body)
	}

	// 未知字段必须被拒（schema 是 .strict()）。
	status, body = circuitCall(t, router, http.MethodPost, "/providers/circuits:batchReset",
		`{"providerIds":[1],"extra":true}`, true)
	if status != http.StatusBadRequest {
		t.Errorf("未知字段应 400，收到 %d：%.200s", status, body)
	}
}

// TestProvidersHealthReportsExpiredOpenAsHalfOpen 是本轮分叉修复的接口级钉子。
//
// 生产实报：供应商页面显示「已熔断」，请求却照常通过（Redis 里 open 但 circuitOpenUntil 已过期，
// 选路按 half-open 放行）。改后 /providers/health 回**有效态**：窗口过期即 half-open，
// 且不再给「还有 N 分钟恢复」的倒计时（原始公式此时会算出负数）。
func TestProvidersHealthReportsExpiredOpenAsHalfOpen(t *testing.T) {
	pools := testPools(t)
	fixture := seedProviders(t, pools)
	client := circuitTestRedis(t)
	ctx := context.Background()

	expired := time.Now().Add(-30 * time.Minute).UnixMilli()
	key := providerCircuitKey(fixture.enabledID)
	if err := client.HSet(ctx, key, map[string]string{
		"failureCount":         "11",
		"lastFailureTime":      strconv.FormatInt(time.Now().Add(-time.Hour).UnixMilli(), 10),
		"circuitState":         "open",
		"circuitOpenUntil":     strconv.FormatInt(expired, 10),
		"halfOpenSuccessCount": "0",
	}).Err(); err != nil {
		t.Fatalf("写熔断夹具失败: %v", err)
	}
	t.Cleanup(func() { _ = client.Del(context.Background(), key).Err() })

	router := providersHealthRouter(t, pools, NewRedisCircuitStates(client, nil, nil))
	status, body := circuitCall(t, router, http.MethodGet, "/providers/health", "", true)
	if status != http.StatusOK {
		t.Fatalf("health 应为 200，收到 %d：%.300s", status, body)
	}
	snapshot := decodeProvidersHealth(t, body)
	entry, ok := snapshot[strconv.FormatInt(fixture.enabledID, 10)]
	if !ok {
		t.Fatalf("health 应含可见供应商 %d，实际键：%v", fixture.enabledID, snapshot)
	}
	if entry.CircuitState != "half-open" {
		t.Errorf("窗口过期的 open 应报 half-open（与数据面放行同源），收到 %q", entry.CircuitState)
	}
	if entry.RecoveryMinutes != nil {
		t.Errorf("窗口已过期时不应再给恢复倒计时，收到 %v", *entry.RecoveryMinutes)
	}
	// 原始读数照旧透传给前端（供排障），不得因为算有效态而丢字段。
	if entry.CircuitOpenUntil == nil || *entry.CircuitOpenUntil != expired {
		t.Errorf("circuitOpenUntil 应保留原值 %d，收到 %v", expired, entry.CircuitOpenUntil)
	}
	if entry.FailureCount != 11 {
		t.Errorf("failureCount 应保留 11，收到 %d", entry.FailureCount)
	}
}
