package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是用户洞察三条端点的端到端用例（真 PG）：概览指标、模型维度、供应商维度。
//
// 数据从 usage_ledger 直接种（该表是计费口径的唯一真源），并逐条钉住：
//   - 拦截行（blocked_by 非空）与 replay 行不进统计；
//   - 概览的 requestCount / totalCost / avgResponseTime / errorRate 四值；
//   - 两个维度的分组与按 cost 倒序；
//   - 404（用户不存在）与日期区间的校验。

const insightsITMarker = "go-admin-insights-it"

// insightsIT 是装配。
type insightsIT struct {
	router     *Router
	pools      *store.Pools
	guard      *notificationsITGuard
	userID     int64
	providerID int64
}

// newInsightsIT 装配真依赖并种一个用户 + 一个供应商 + 四行账本。
func newInsightsIT(t *testing.T) *insightsIT {
	t.Helper()
	dsn := os.Getenv("CCH_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 CCH_TEST_DSN，跳过数据库集成测试")
	}
	ctx := context.Background()
	pools, err := store.Open(ctx, store.Options{
		DSN:      dsn,
		Budget:   config.SplitPoolBudget(6),
		Timeouts: config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("建立连接池失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })

	guard := &notificationsITGuard{}
	deps := Deps{Guard: guard, Store: pools, Problems: NewProblems(nil)}
	router := New(Options{Deps: deps})
	RegisterAdminUserInsights(router, deps)

	integration := &insightsIT{router: router, pools: pools, guard: guard}
	integration.seed(t)
	return integration
}

// seed 造夹具：一个厂商、一个供应商、一个用户、四行账本（含一行拦截行与一行 replay 行）。
//
// 行设计（便于断言）：
//
//	行1 模型 A，成功，cost 1.5，duration 100，input 10，output 20
//	行2 模型 B，失败，cost 0.5，duration 300，input 30，output 40
//	行3 模型 A，成功，cost 0.0000005（验证 6 位舍入）
//	行4 模型 A，成功，cost 9，但 blocked_by 非空（不得进统计）
//	行5 模型 A，成功，cost 9，但 is_replay = true（不得进统计）
func (integration *insightsIT) seed(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	pool, err := integration.pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	prefix := fmt.Sprintf("%s-%d", insightsITMarker, time.Now().UnixNano())

	var vendorID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO provider_vendors (website_domain, display_name) VALUES ($1, $2) RETURNING id`,
		prefix+".invalid", prefix).Scan(&vendorID); err != nil {
		t.Fatalf("建厂商失败: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO providers (name, url, key, provider_vendor_id, is_enabled, weight, priority,
			cost_multiplier, group_tag, provider_type)
		VALUES ($1, $2, $3, $4, true, 1, 0, '1.0'::numeric, $1, 'claude')
		RETURNING id`,
		prefix, "https://"+prefix+".invalid", "sk-insights-it", vendorID).Scan(&integration.providerID); err != nil {
		t.Fatalf("建供应商失败: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO users (name) VALUES ($1) RETURNING id`, prefix).
		Scan(&integration.userID); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx := context.Background()
		pool, err := integration.pools.Writer()
		if err != nil {
			return
		}
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM usage_ledger WHERE user_id = $1`, integration.userID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM users WHERE id = $1`, integration.userID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM providers WHERE id = $1`, integration.providerID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM provider_vendors WHERE id = $1`, vendorID)
	})

	// request_id 是 int4：用「秒级时间戳的低 6 位 x 1000」做基址，既避开 int4 上限，
	// 也几乎不与并行用例相撞（再乘上后 5 行的自增）。
	requestID := time.Now().Unix() % 1_000_000 * 1000
	insert := func(model string, cost string, duration int, isSuccess bool, isReplay bool, blocked bool) {
		var blockedBy *string
		if blocked {
			value := "sensitive_word"
			blockedBy = &value
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO usage_ledger (request_id, provider_id, user_id, key, final_provider_id, model,
				cost_usd, input_tokens, output_tokens, status_code, is_success, duration_ms,
				created_at, is_replay, blocked_by)
			VALUES ($1, $2, $3, $4, $2, $5, $6::numeric, 10, 20, 200, $7, $8, now(), $9, $10)`,
			requestID, integration.providerID, integration.userID, prefix+"-key", model,
			cost, isSuccess, duration, isReplay, blockedBy); err != nil {
			t.Fatalf("种账本行失败: %v", err)
		}
		requestID++
	}
	insert("model-a", "1.5", 100, true, false, false)
	insert("model-b", "0.5", 300, false, false, false)
	insert("model-a", "0.0000005", 0, true, false, false)
	insert("model-a", "9", 0, true, false, true)
	insert("model-a", "9", 0, true, true, false)
}

// callInsights 发一个 GET。
func (integration *insightsIT) callInsights(t *testing.T, path string) (int, string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	integration.router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder.Code, recorder.Body.String()
}

// TestUserInsightsOverviewEndToEnd 钉住概览的四个指标与 404。
func TestUserInsightsOverviewEndToEnd(t *testing.T) {
	integration := newInsightsIT(t)

	code, body := integration.callInsights(t,
		fmt.Sprintf("%s/admin/users/%d/insights/overview", MountPrefix, integration.userID))
	if code != http.StatusOK {
		t.Fatalf("概览应得 200，实际 %d（%s）", code, body)
	}
	var response struct {
		User struct {
			ID int64 `json:"id"`
		} `json:"user"`
		Overview struct {
			RequestCount    int64   `json:"requestCount"`
			TotalCost       float64 `json:"totalCost"`
			AvgResponseTime float64 `json:"avgResponseTime"`
			ErrorRate       float64 `json:"errorRate"`
		} `json:"overview"`
		CurrencyCode string `json:"currencyCode"`
	}
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		t.Fatalf("概览响应不是 JSON：%v（%s）", err, body)
	}
	if response.User.ID != integration.userID {
		t.Fatalf("user.id 应为 %d，实际 %d", integration.userID, response.User.ID)
	}
	// 只有前 3 行进统计（拦截行与 replay 行被计费口径排除）。
	if response.Overview.RequestCount != 3 {
		t.Fatalf("requestCount 应为 3，实际 %d", response.Overview.RequestCount)
	}
	if response.Overview.TotalCost != 2.000001 {
		t.Fatalf("totalCost 应为 2.000001（1.5+0.5+0.0000005 舍入到 6 位），实际 %v",
			response.Overview.TotalCost)
	}
	if response.Overview.AvgResponseTime != 133 {
		t.Fatalf("avgResponseTime 应为 round((100+300+0)/3)=133，实际 %v",
			response.Overview.AvgResponseTime)
	}
	if response.Overview.ErrorRate != 33.33 {
		t.Fatalf("errorRate 应为 (1/3)*100 保留两位 = 33.33，实际 %v", response.Overview.ErrorRate)
	}
	if response.CurrencyCode == "" {
		t.Fatal("currencyCode 不应为空")
	}

	missingCode, missingBody := integration.callInsights(t,
		fmt.Sprintf("%s/admin/users/%d/insights/overview", MountPrefix, integration.userID+1_000_000_000))
	if missingCode != http.StatusNotFound {
		t.Fatalf("用户不存在应得 404，实际 %d（%s）", missingCode, missingBody)
	}
	if !strings.Contains(missingBody, "admin_user_insights.not_found") {
		t.Fatalf("404 错误码不对：%s", missingBody)
	}
}

// TestUserInsightsBreakdownEndToEnd 钉住两个维度聚合。
func TestUserInsightsBreakdownEndToEnd(t *testing.T) {
	integration := newInsightsIT(t)

	t.Run("模型维度按 cost 倒序", func(t *testing.T) {
		code, body := integration.callInsights(t, fmt.Sprintf(
			"%s/admin/users/%d/insights/model-breakdown", MountPrefix, integration.userID))
		if code != http.StatusOK {
			t.Fatalf("模型维度应得 200，实际 %d（%s）", code, body)
		}
		var response struct {
			Breakdown []struct {
				Model    *string `json:"model"`
				Requests int64   `json:"requests"`
				Cost     float64 `json:"cost"`
			} `json:"breakdown"`
			CurrencyCode string `json:"currencyCode"`
		}
		if err := json.Unmarshal([]byte(body), &response); err != nil {
			t.Fatalf("模型维度响应不是 JSON：%v（%s）", err, body)
		}
		if len(response.Breakdown) != 2 {
			t.Fatalf("应有 2 个模型分组，实际 %d（%s）", len(response.Breakdown), body)
		}
		if *response.Breakdown[0].Model != "model-a" || response.Breakdown[0].Requests != 2 {
			t.Fatalf("首组应为 model-a / 2 次：%+v", response.Breakdown[0])
		}
		if response.Breakdown[0].Cost <= response.Breakdown[1].Cost {
			t.Fatalf("应按 cost 倒序：%+v", response.Breakdown)
		}
		if response.CurrencyCode == "" {
			t.Fatal("currencyCode 不应为空")
		}
	})

	t.Run("供应商维度内联名字", func(t *testing.T) {
		code, body := integration.callInsights(t, fmt.Sprintf(
			"%s/admin/users/%d/insights/provider-breakdown", MountPrefix, integration.userID))
		if code != http.StatusOK {
			t.Fatalf("供应商维度应得 200，实际 %d（%s）", code, body)
		}
		var response struct {
			Breakdown []struct {
				ProviderID   int64   `json:"providerId"`
				ProviderName *string `json:"providerName"`
				Requests     int64   `json:"requests"`
				Cost         float64 `json:"cost"`
			} `json:"breakdown"`
		}
		if err := json.Unmarshal([]byte(body), &response); err != nil {
			t.Fatalf("供应商维度响应不是 JSON：%v（%s）", err, body)
		}
		if len(response.Breakdown) != 1 {
			t.Fatalf("应有 1 个供应商分组，实际 %d（%s）", len(response.Breakdown), body)
		}
		item := response.Breakdown[0]
		if item.ProviderID != integration.providerID || item.ProviderName == nil ||
			!strings.HasPrefix(*item.ProviderName, insightsITMarker) {
			t.Fatalf("供应商分组不符：%+v", item)
		}
		if item.Requests != 3 {
			t.Fatalf("供应商维度的请求数应为 3，实际 %d", item.Requests)
		}
	})

	t.Run("日期区间与筛选的校验", func(t *testing.T) {
		badFormat := fmt.Sprintf("%s/admin/users/%d/insights/model-breakdown?startDate=2026/01/01",
			MountPrefix, integration.userID)
		if code, body := integration.callInsights(t, badFormat); code != http.StatusBadRequest {
			t.Fatalf("非法日期格式应得 400，实际 %d（%s）", code, body)
		}
		reversed := fmt.Sprintf("%s/admin/users/%d/insights/model-breakdown?startDate=2026-02-01&endDate=2026-01-01",
			MountPrefix, integration.userID)
		if code, body := integration.callInsights(t, reversed); code != http.StatusBadRequest {
			t.Fatalf("起止倒置应得 400，实际 %d（%s）", code, body)
		}
		// 未来区间：命中 0 行，但响应仍是 200 + 空数组（Node 同法）。
		future := fmt.Sprintf("%s/admin/users/%d/insights/model-breakdown?startDate=2999-01-01",
			MountPrefix, integration.userID)
		code, body := integration.callInsights(t, future)
		if code != http.StatusOK {
			t.Fatalf("未来区间应得 200，实际 %d（%s）", code, body)
		}
		if !strings.Contains(body, `"breakdown":[]`) {
			t.Fatalf("未来区间应回空数组：%s", body)
		}
		badKey := fmt.Sprintf("%s/admin/users/%d/insights/model-breakdown?keyId=0",
			MountPrefix, integration.userID)
		if code, body := integration.callInsights(t, badKey); code != http.StatusBadRequest {
			t.Fatalf("非法 keyId 应得 400，实际 %d（%s）", code, body)
		}
	})

	t.Run("档位为 admin", func(t *testing.T) {
		for _, level := range integration.guard.snapshot() {
			if level != AccessAdmin {
				t.Fatalf("用户洞察应是 admin 档位，实际 %s", level)
			}
		}
	})
}

// TestUserInsightsOverviewUserProjectionShape 钉住 overview 里 user 的投影形状（对拍缺陷 D2）。
//
// Node 返回的是 findUserById 经 `toUser`（src/repository/_shared/transformers.ts:24）整形后的对象：
//   - description 的空值变**空串**（不是 null）；
//   - rpm / dailyQuota 走「不大于 0 即 null」的分支（0 也回落 null）；
//   - limit*Usd 走 parseOptionalNumber（0 保留 0）；
//   - tags / allowed* / blockedClients 缺省空数组。
//
// 这条钉子同时验两个方向：NULL 的来源（列未设）与 0 的来源（列设成 0），因为 toUser 对
// dailyQuota/rpm 与 limit*Usd 的处理**相反**——混起来就写错。
func TestUserInsightsOverviewUserProjectionShape(t *testing.T) {
	integration := newInsightsIT(t)
	ctx := context.Background()
	pool, err := integration.pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}

	// Node 的 findUserById 列清单经 toUser 后的键集（26 个）。
	wantKeys := []string{
		"allowedClients", "allowedModels", "blockedClients", "costResetAt", "createdAt",
		"dailyQuota", "dailyResetMode", "dailyResetTime", "deletedAt", "description", "expiresAt",
		"id", "isEnabled", "limit5hCostResetAt", "limit5hResetMode", "limit5hUsd",
		"limitConcurrentSessions", "limitMonthlyUsd", "limitTotalUsd", "limitWeeklyUsd",
		"name", "providerGroup", "role", "rpm", "tags", "updatedAt",
	}

	readUser := func(t *testing.T) map[string]any {
		t.Helper()
		status, body := integration.callInsights(t,
			fmt.Sprintf("/admin/users/%d/insights/overview", integration.userID))
		if status != 200 {
			t.Fatalf("状态应为 200，实际 %d：%s", status, body)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			t.Fatalf("响应不是 JSON：%v", err)
		}
		user, ok := payload["user"].(map[string]any)
		if !ok {
			t.Fatalf("user 应为对象，实际 %T", payload["user"])
		}
		return user
	}

	t.Run("列全 NULL：空值形态", func(t *testing.T) {
		user := readUser(t)
		if keys := sortedJSONKeys(user); !sameKeys(keys, wantKeys) {
			t.Fatalf("user 键集应为 Node 的 26 键，实际 %v", keys)
		}
		// description 列是 NULL，但 Node 投影成空串。
		if user["description"] != "" {
			t.Errorf("description 应为空串（toUser 的 `|| \"\"`），实际 %T（%v）", user["description"], user["description"])
		}
		// 未设的限额一律 null。
		for _, key := range []string{
			"dailyQuota", "rpm", "limit5hUsd", "limitWeeklyUsd", "limitMonthlyUsd",
			"limitTotalUsd", "limitConcurrentSessions", "expiresAt", "costResetAt", "limit5hCostResetAt",
		} {
			if value, ok := user[key]; !ok || value != nil {
				t.Errorf("%s 应为 null，实际 %v（存在=%v）", key, value, ok)
			}
		}
		// 缺省的数组/枚举/布尔必须与 Node 的兜底一致。
		for _, key := range []string{"tags", "allowedClients", "blockedClients", "allowedModels"} {
			list, ok := user[key].([]any)
			if !ok || len(list) != 0 {
				t.Errorf("%s 应为空数组，实际 %T（%v）", key, user[key], user[key])
			}
		}
		if user["role"] != "user" {
			t.Errorf("role 应为 user，实际 %v", user["role"])
		}
		if user["dailyResetMode"] != "fixed" || user["dailyResetTime"] != "00:00" {
			t.Errorf("重置模式应为 fixed/00:00，实际 %v/%v", user["dailyResetMode"], user["dailyResetTime"])
		}
		if user["limit5hResetMode"] != "rolling" {
			t.Errorf("limit5hResetMode 应为 rolling，实际 %v", user["limit5hResetMode"])
		}
		if user["isEnabled"] != true {
			t.Errorf("isEnabled 应为 true，实际 %v", user["isEnabled"])
		}
		if user["name"] != integrationFixtureName(t, integration) {
			t.Errorf("name 应取库值，实际 %v", user["name"])
		}
	})

	t.Run("dailyQuota/rpm 设 0、limit5hUsd 设正值：0 与正值的相反处理", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `
			UPDATE users SET description = 'pin', daily_limit_usd = 0, rpm_limit = 0,
				limit_5h_usd = 12.5, limit_weekly_usd = 0
			WHERE id = $1`, integration.userID); err != nil {
			t.Fatalf("更新用户夹具失败: %v", err)
		}
		user := readUser(t)
		if user["description"] != "pin" {
			t.Errorf("description 应取库值 pin，实际 %v", user["description"])
		}
		// dailyQuota / rpm：0 不算数 → null。
		for _, key := range []string{"dailyQuota", "rpm"} {
			if value, ok := user[key]; !ok || value != nil {
				t.Errorf("%s 列设为 0 后应回落 null（Node 的 `> 0` 分支），实际 %v（存在=%v）", key, value, ok)
			}
		}
		// limit5hUsd：正数保留；limitWeeklyUsd：0 也保留（parseOptionalNumber）。
		if value, ok := user["limit5hUsd"].(float64); !ok || value != 12.5 {
			t.Errorf("limit5hUsd 应为 12.5，实际 %T（%v）", user["limit5hUsd"], user["limit5hUsd"])
		}
		if value, ok := user["limitWeeklyUsd"].(float64); !ok || value != 0 {
			t.Errorf("limitWeeklyUsd 列设为 0 后应保留 0，实际 %T（%v）", user["limitWeeklyUsd"], user["limitWeeklyUsd"])
		}
	})
}

// integrationFixtureName 读回夹具用户的名字。
func integrationFixtureName(t *testing.T, integration *insightsIT) string {
	t.Helper()
	user, err := integration.pools.FindAdminUserByID(context.Background(), integration.userID)
	if err != nil {
		t.Fatalf("读回夹具用户失败: %v", err)
	}
	return user.Name
}
