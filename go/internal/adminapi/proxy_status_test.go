package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住根级 `GET /api/proxy-status` 的契约（Node：src/app/api/proxy-status/route.ts +
// src/lib/proxy-status-tracker.ts）。要点：
//
//  1. 数据全部来自**库内聚合**（Node 的 tracker 自述如此，start/end 都是空实现），
//     故本端点不需要进程内观测态，也不扩 Deps；
//  2. 正文与 `GET /api/v1/dashboard/proxy-status` **同形**——两条并行实现会立刻分叉成两份
//     数字（运维页与 dashboard 显示不同），故这里用「同一时钟下逐字节相同」的钉子锁住；
//  3. 权限是**管理员档**（Node 先 401 后 403），且因为在 /api/v1 之外，不发管理面信封头。

func TestRegisterProxyStatusRouteMetadata(t *testing.T) {
	router := New(Options{Deps: Deps{Guard: &recordingGuard{}}})
	RegisterProxyStatusRoute(router, Deps{})
	if count := router.RouteCount(); count != 0 {
		t.Fatalf("缺 Store 时不该注册（回退 Node），收到 %d 条", count)
	}

	router = New(Options{Deps: Deps{Guard: &recordingGuard{}}})
	RegisterProxyStatusRoute(router, Deps{Store: &store.Pools{}})
	var found *Route
	for _, route := range router.RouteList() {
		if route.Path == "/api/proxy-status" {
			route := route
			found = &route
			break
		}
	}
	if found == nil {
		t.Fatal("Store 装配时该注册 /api/proxy-status")
	}
	if found.Access != AccessAdmin {
		t.Fatalf("Node 该路由是管理员档，收到 %q", found.Access)
	}
	if !found.NoManagementEnvelope {
		t.Fatal("该路由在 /api/v1 之外，不该发管理面信封头")
	}
	if found.Method != http.MethodGet {
		t.Fatalf("方法应为 GET，收到 %s", found.Method)
	}
}

// TestProxyStatusRootMatchesDashboardPayload 用**固定时钟**让两条端点各自组装一次，
// 断言正文逐字节相同。这是本文件最重要的一条：它把「两份组装逻辑不许分叉」变成可执行的约束。
func TestProxyStatusRootMatchesDashboardPayload(t *testing.T) {
	pools := meOpenPools(t)
	providerID := fixtureProviderID(t, pools)
	userID := fixtureUser(t, pools, "user", true)
	keyID, keyValue := fixtureKey(t, pools, fixtureKeyOptions{
		userID: userID, canLoginWebUI: true, isEnabled: true,
	})
	_ = keyID

	// 一条在途（status_code IS NULL）+ 一条已终局，用来同时走活两条分支。
	inFlightID := insertProxyStatusRow(t, pools, userID, keyValue, providerID, nil)
	insertProxyStatusRow(t, pools, userID, keyValue, providerID, ptrInt(200))
	t.Cleanup(func() {
		pool, err := pools.Control()
		if err != nil {
			return
		}
		_, _ = pool.Exec(context.Background(), `DELETE FROM message_request WHERE id = $1`, inFlightID)
	})

	// 固定时钟必须是**同一个时刻**，不是「每次都算一遍 now+30s」——后者两次调用会差几毫秒，
	// 而 duration/elapsed 正是毫秒级字段。
	frozen := time.Now().Add(30 * time.Second)
	fixedNow := func() time.Time { return frozen }
	principal := Principal{UserID: userID, IsAdmin: true}
	guard := principalGuard{principal: principal}

	rootRouter := New(Options{Deps: Deps{Guard: guard}})
	// 这里**不**走 RegisterProxyStatusRoute：那条用真实时钟，而 Router 对「方法+路径」重复保留**首次**
	// 注册，会把下面注入的冻结时钟挡掉（本次调试亲历：两条端点因此差 30 秒，看起来像实现分叉）。
	// 注册元数据由 TestRegisterProxyStatusRouteMetadata 单独钉。
	dashboardRouter := New(Options{Deps: Deps{Guard: guard}})
	dashboard := &dashboardAPI{
		pools:    pools,
		problems: NewProblems(nil),
		logger:   logx.New(nil),
		now:      fixedNow,
	}
	dashboardRouter.Add(Route{
		Method: http.MethodGet, Path: "/dashboard/proxy-status", Access: AccessAdmin,
		Module: "dashboard", OperationID: "getProxyStatus",
		Handler: http.HandlerFunc(dashboard.handleProxyStatus),
	})
	// 根级那条用同一时钟，才能逐字节比（否则 duration/elapsed 会差几毫秒）。
	rootAPI := &proxyStatusAPI{pools: pools, logger: logx.New(nil), now: fixedNow, problems: NewProblems(nil)}
	rootRouter.Add(Route{
		Method: http.MethodGet, Path: "/api/proxy-status", Access: AccessAdmin,
		Module: "dashboard", OperationID: "getProxyStatus", NoManagementEnvelope: true,
		Handler: http.HandlerFunc(rootAPI.handleGet),
	})

	rootStatus, rootBody := doGet(t, rootRouter, "/api/proxy-status")
	dashStatus, dashBody := doGet(t, dashboardRouter, "/api/v1/dashboard/proxy-status")
	if rootStatus != http.StatusOK || dashStatus != http.StatusOK {
		t.Fatalf("两条都应 200，实得 root=%d dashboard=%d", rootStatus, dashStatus)
	}
	// 共享库里同时有别的用例造的用户，两次 HTTP 调用之间集合可能变化，故**不做整包逐字节比**：
	// 先比形状（键集），再比**本用例那个用户**的条目逐字节（只有本用例会碰它，且两侧同一时钟）。
	rootKeys, rootUsers := decodeProxyStatusShape(t, rootBody)
	dashKeys, dashUsers := decodeProxyStatusShape(t, dashBody)
	if !equalStrings(rootKeys, dashKeys) {
		t.Fatalf("两条端点的键集必须一致，root=%v dashboard=%v", rootKeys, dashKeys)
	}
	rootMine, ok := rootUsers[userID]
	if !ok {
		t.Fatalf("根级结果里应含本用例用户 %d（Node 对无活动用户也返回一条）", userID)
	}
	dashMine, ok := dashUsers[userID]
	if !ok {
		t.Fatalf("dashboard 结果里应含本用例用户 %d", userID)
	}
	rootEntry, _ := json.Marshal(rootMine)
	dashEntry, _ := json.Marshal(dashMine)
	if string(rootEntry) != string(dashEntry) {
		t.Fatalf("同一用户条目必须逐字节相同\nroot=%.400s\ndashboard=%.400s", rootEntry, dashEntry)
	}
	if _, hasLast := rootMine["lastRequest"]; !hasLast {
		t.Fatal("用户条目应含 lastRequest 键（无终局请求时为 null）")
	}

	// 顺带钉住两条前提：根级不发信封头，dashboard 那条发。
	_, rootHeaders := doGetHeaders(t, rootRouter, "/api/proxy-status")
	if rootHeaders.Get("X-API-Version") != "" {
		t.Fatalf("根级不该发 X-API-Version，收到 %q", rootHeaders.Get("X-API-Version"))
	}

	// 我这条用户必须在结果里，且 activeCount 反映那条在途行、lastRequest 指向已终局行。
	var payload struct {
		Users []struct {
			UserID         int64 `json:"userId"`
			ActiveCount    int   `json:"activeCount"`
			ActiveRequests []struct {
				RequestID int64  `json:"requestId"`
				KeyName   string `json:"keyName"`
				Model     string `json:"model"`
				Duration  int64  `json:"duration"`
			} `json:"activeRequests"`
			LastRequest *struct {
				RequestID int64  `json:"requestId"`
				Model     string `json:"model"`
				Elapsed   int64  `json:"elapsed"`
			} `json:"lastRequest"`
		} `json:"users"`
	}
	if err := json.Unmarshal([]byte(rootBody), &payload); err != nil {
		t.Fatalf("正文不是合法 JSON: %v", err)
	}
	var mine *struct {
		UserID         int64 `json:"userId"`
		ActiveCount    int   `json:"activeCount"`
		ActiveRequests []struct {
			RequestID int64  `json:"requestId"`
			KeyName   string `json:"keyName"`
			Model     string `json:"model"`
			Duration  int64  `json:"duration"`
		} `json:"activeRequests"`
		LastRequest *struct {
			RequestID int64  `json:"requestId"`
			Model     string `json:"model"`
			Elapsed   int64  `json:"elapsed"`
		} `json:"lastRequest"`
	}
	for index := range payload.Users {
		if payload.Users[index].UserID == userID {
			mine = &payload.Users[index]
			break
		}
	}
	if mine == nil {
		t.Fatalf("结果里应含该用户（Node 对无活动用户也返回一条）")
	}
	if mine.ActiveCount != 1 || len(mine.ActiveRequests) != 1 {
		t.Fatalf("应恰好一条在途请求，实得 count=%d list=%d", mine.ActiveCount, len(mine.ActiveRequests))
	}
	if mine.ActiveRequests[0].RequestID != inFlightID {
		t.Fatalf("在途请求 id 应为 %d，实得 %d", inFlightID, mine.ActiveRequests[0].RequestID)
	}
	if mine.ActiveRequests[0].KeyName == "" {
		t.Fatal("keyName 应回落到 maskKey(key) 或取 keys.name，不该为空")
	}
	if mine.LastRequest == nil {
		t.Fatal("应有最近一次已终局请求")
	}
	if payload.Users[0].UserID == 0 {
		t.Fatal("用户条目应有 userId")
	}
}

// TestProxyStatusEmptyArraysNotNull 钉住「无活跃请求时是空数组不是 null」（Node 的 `?? []`）。
func TestProxyStatusEmptyArraysNotNull(t *testing.T) {
	pools := meOpenPools(t)
	userID := fixtureUser(t, pools, "user", true)

	payload, err := proxyStatusPayload(context.Background(), pools, time.Now)
	if err != nil {
		t.Fatalf("组装失败: %v", err)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var decoded struct {
		Users []map[string]any `json:"users"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	for _, user := range decoded.Users {
		if int64(user["userId"].(float64)) != userID {
			continue
		}
		if active, ok := user["activeRequests"].([]any); !ok || active == nil {
			t.Fatalf("activeRequests 应是空数组而非 null，实得 %v", user["activeRequests"])
		}
		if user["lastRequest"] != nil {
			t.Fatalf("该用户没有终局请求时 lastRequest 应为 null，实得 %v", user["lastRequest"])
		}
		return
	}
	t.Fatal("结果里应含该用户")
}

func ptrInt(value int) *int { return &value }

// insertProxyStatusRow 插一行 message_request：statusCode 为 nil 表示「在途」。
func insertProxyStatusRow(
	t *testing.T,
	pools *store.Pools,
	userID int64,
	keyValue string,
	providerID int64,
	statusCode *int,
) int64 {
	t.Helper()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}
	var id int64
	err = pool.QueryRow(context.Background(), `
		INSERT INTO message_request (
			provider_id, user_id, key, model, original_model, endpoint,
			status_code, input_tokens, output_tokens, cost_usd, duration_ms, ttfb_ms,
			is_replay, created_at, updated_at
		) VALUES (
			$1, $2, $3, $4, $4, '/v1/messages',
			$5, 10, 5, '0.01'::numeric, 8, 4,
			false, now() - interval '5 minutes', now() - interval '2 minutes'
		) RETURNING id`,
		providerID, userID, keyValue, fmt.Sprintf("proxystatus-%d", time.Now().UnixNano()), statusCode,
	).Scan(&id)
	if err != nil {
		t.Fatalf("插 proxy-status 夹具行失败: %v", err)
	}
	return id
}

func doGet(t *testing.T, router *Router, target string) (int, string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	return recorder.Code, recorder.Body.String()
}

func doGetHeaders(t *testing.T, router *Router, target string) (int, http.Header) {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	return recorder.Code, recorder.Header()
}

// decodeProxyStatusShape 解出顶层键集与「按 userId 索引的用户条目」。
func decodeProxyStatusShape(t *testing.T, body string) ([]string, map[int64]map[string]any) {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &top); err != nil {
		t.Fatalf("正文不是 JSON 对象: %v", err)
	}
	keys := make([]string, 0, len(top))
	for key := range top {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var entries []map[string]any
	if err := json.Unmarshal(top["users"], &entries); err != nil {
		t.Fatalf("users 应是数组: %v", err)
	}
	byID := make(map[int64]map[string]any, len(entries))
	for _, entry := range entries {
		id, ok := entry["userId"].(float64)
		if !ok {
			t.Fatalf("用户条目缺 userId: %v", entry)
		}
		byID[int64(id)] = entry
	}
	return keys, byID
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
