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

// 本文件是**统计面**两条端点的端到端用例（真 PG）：
//
//	GET /api/v1/dashboard/statistics           图表数据（管理员 → users 模式）
//	GET /api/v1/admin/users/{id}/insights/key-trend  某用户的密钥趋势行
//
// 逐条钉住的是「数据源口径」与「形状契约」，不是「数值好看」：
//   - 桶数（today = 24 个小时桶；7days = 7 个日桶）；
//   - 「每个桶 × 每个实体」的笛卡尔积（缺行补 0，消费是 15 位小数字符串）；
//   - 计费口径：拦截行（blocked_by）与 replay 行不进统计；
//   - key-trend 的 total_cost 类型分叉（零填充 = 数字 0，真实行 = numeric 字符串）；
//   - 两处 400（非法 timeRange）与 key-trend 对不存在用户的 200 + 空 items。

const statisticsITMarker = "go-admin-stats-it"

// statisticsITGuard 把固定身份塞进上下文（认证语义由 A0-2 的守卫负责）。
type statisticsITGuard struct{ principal Principal }

func (guard *statisticsITGuard) Wrap(_ AccessLevel, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		next.ServeHTTP(writer, request.WithContext(
			WithPrincipal(request.Context(), guard.principal)))
	})
}

// statisticsIT 是本文件的装配。
type statisticsIT struct {
	router     *Router
	pools      *store.Pools
	guard      *statisticsITGuard
	userID     int64
	userName   string
	keyID      int64
	keyName    string
	providerID int64
	// keyString 是写入 usage_ledger.key 的密钥原文（统计按原文关联密钥）。
	keyString string
}

// newStatisticsIT 装配真依赖并种一个用户 + 一个密钥 + 四行账本。
func newStatisticsIT(t *testing.T) *statisticsIT {
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

	integration := &statisticsIT{pools: pools}
	integration.seed(t)

	guard := &statisticsITGuard{principal: Principal{
		UserID:   integration.userID,
		Username: integration.userName,
		IsAdmin:  true,
	}}
	deps := Deps{Guard: guard, Store: pools, Problems: NewProblems(nil)}
	router := New(Options{Deps: deps})
	RegisterDashboardRoutes(router, deps)
	RegisterAdminUserInsights(router, deps)
	integration.router = router
	integration.guard = guard
	return integration
}

// seed 造夹具。
//
// 行设计（便于断言）：
//
//	行1 成功 cost 1.5
//	行2 成功 cost 0.5     → 两行都在当前小时桶里，合计 2.000000000000000 / 2 次
//	行3 成功 cost 9  但 blocked_by 非空（不得进统计）
//	行4 成功 cost 9  但 is_replay = true（不得进统计）
func (integration *statisticsIT) seed(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	pool, err := integration.pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	prefix := fmt.Sprintf("%s-%d", statisticsITMarker, time.Now().UnixNano())
	integration.userName = prefix
	integration.keyName = prefix + "-key"
	integration.keyString = "sk-" + prefix

	var providerID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO providers (name, url, key, provider_type, is_enabled, weight, priority,
			group_tag, protocol_conversion_enabled)
		VALUES ($1, 'http://127.0.0.1:9', 'upstream-not-used', 'codex', true, 1, 0, 'default', true)
		RETURNING id`, prefix).Scan(&providerID); err != nil {
		t.Fatalf("建供应商失败: %v", err)
	}
	integration.providerID = providerID
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (name) VALUES ($1) RETURNING id`, integration.userName).
		Scan(&integration.userID); err != nil {
		t.Fatalf("建用户失败: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO keys (user_id, key, name, is_enabled, provider_group)
		VALUES ($1, $2, $3, true, 'default') RETURNING id`,
		integration.userID, integration.keyString, integration.keyName).
		Scan(&integration.keyID); err != nil {
		t.Fatalf("建密钥失败: %v", err)
	}

	requestID := int(time.Now().UnixNano() % 1_000_000_000)
	insert := func(cost string, blocked bool, replay bool) {
		var blockedBy *string
		if blocked {
			value := "sensitive_word"
			blockedBy = &value
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO usage_ledger (request_id, provider_id, user_id, key, final_provider_id, model,
				cost_usd, input_tokens, output_tokens, status_code, is_success, duration_ms,
				created_at, is_replay, blocked_by)
			VALUES ($1, $2, $3, $4, $2, $5, $6::numeric, 10, 20, 200, true, 100, now(), $7, $8)`,
			requestID, providerID, integration.userID, integration.keyString, prefix+"-model",
			cost, replay, blockedBy); err != nil {
			t.Fatalf("种账本行失败: %v", err)
		}
		requestID++
	}
	insert("1.5", false, false)
	insert("0.5", false, false)
	insert("9", true, false)
	insert("9", false, true)

	t.Cleanup(func() {
		cleanup := context.Background()
		for _, statement := range []string{
			`DELETE FROM usage_ledger WHERE user_id = $1`,
			`DELETE FROM keys WHERE user_id = $1`,
			`DELETE FROM users WHERE id = $1`,
		} {
			if _, err := pool.Exec(cleanup, statement, integration.userID); err != nil {
				t.Errorf("清理夹具失败（%s）: %v", statement, err)
			}
		}
		// 供应商会被 usage_ledger 引用（行已删，仍保守地置失效 + 软删，与对拍台的清理同判）。
		if _, err := pool.Exec(cleanup,
			`UPDATE providers SET is_enabled = false, deleted_at = now() WHERE id = $1`,
			integration.providerID); err != nil {
			t.Errorf("清理供应商夹具失败: %v", err)
		}
	})
}

// callStatistics 发一个 GET 并返回状态码与正文。
func (integration *statisticsIT) call(t *testing.T, path string) (int, string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	integration.router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	return recorder.Code, recorder.Body.String()
}

// dashboardStatisticsBody 是 /dashboard/statistics 的判读形状。
type statisticsChartBody struct {
	ChartData []map[string]json.RawMessage `json:"chartData"`
	Users     []struct {
		ID      int64  `json:"id"`
		Name    string `json:"name"`
		DataKey string `json:"dataKey"`
	} `json:"users"`
	TimeRange  string `json:"timeRange"`
	Resolution string `json:"resolution"`
	Mode       string `json:"mode"`
}

// TestDashboardStatisticsUsersModeEndToEnd 钉住管理员模式的图表形状与口径。
func TestDashboardStatisticsUsersModeEndToEnd(t *testing.T) {
	integration := newStatisticsIT(t)

	code, body := integration.call(t, MountPrefix+"/dashboard/statistics?timeRange=today")
	if code != http.StatusOK {
		t.Fatalf("统计应得 200，实际 %d（%s）", code, body)
	}
	var response statisticsChartBody
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		t.Fatalf("统计响应不是 JSON：%v（%s）", err, body)
	}
	if response.Mode != "users" {
		t.Fatalf("管理员应为 users 模式，实际 %q", response.Mode)
	}
	if response.Resolution != "hour" {
		t.Fatalf("today 的分辨率应为 hour，实际 %q", response.Resolution)
	}
	if response.TimeRange != "today" {
		t.Fatalf("timeRange 应回显 today，实际 %q", response.TimeRange)
	}
	if len(response.ChartData) != 24 {
		t.Fatalf("today 应有 24 个小时桶，实际 %d", len(response.ChartData))
	}

	// 每个桶的键集合必须完全一致（零填充就是「桶 × 实体」的笛卡尔积，缺行补 0）：
	// 以第一个桶为基准逐键比对。**不能**拿另一个查询的 users 数组算键数——那是一次独立的
	// 读，共享库里别的 lane 建/删用户会让两个读数差一拍（本用例上一版就死在这里）。
	baseline := make(map[string]bool, len(response.ChartData[0]))
	for key := range response.ChartData[0] {
		baseline[key] = true
	}
	for at, entry := range response.ChartData {
		if len(entry) != len(baseline) {
			t.Fatalf("第 %d 个桶的键数应与其他桶一致（%d），实际 %d", at, len(baseline), len(entry))
		}
		for key := range entry {
			if !baseline[key] {
				t.Fatalf("第 %d 个桶多出键 %q", at, key)
			}
		}
	}

	// 夹具用户必须出现在实体清单里，且 dataKey 前缀是 user。
	entityKey := ""
	for _, entity := range response.Users {
		if entity.ID == integration.userID {
			if entity.Name != integration.userName {
				t.Fatalf("实体名应为 %q，实际 %q", integration.userName, entity.Name)
			}
			entityKey = entity.DataKey
		}
	}
	if entityKey != fmt.Sprintf("user-%d", integration.userID) {
		t.Fatalf("dataKey 应为 user-%d，实际 %q", integration.userID, entityKey)
	}
	_ = integration.keyID

	// 汇总夹具用户的计数与消费：拦截行与 replay 行不得计入（1.5 + 0.5 = 2）。
	const zeroCost = `"0.000000000000000"`
	const wantCost = `"2.000000000000000"`
	callsTotal := 0
	zeroBuckets := 0
	filledBuckets := 0
	for _, entry := range response.ChartData {
		rawCalls, ok := entry[entityKey+"_calls"]
		if !ok {
			t.Fatalf("桶里缺 %s_calls：%s", entityKey, entry)
		}
		var calls int64
		if err := json.Unmarshal(rawCalls, &calls); err != nil {
			t.Fatalf("计数不是整数：%v（%s）", err, rawCalls)
		}
		callsTotal += int(calls)

		rawCost := string(entry[entityKey+"_cost"])
		switch {
		case calls == 0 && rawCost == zeroCost:
			zeroBuckets++
		case calls == 2 && rawCost == wantCost:
			filledBuckets++
		default:
			t.Fatalf("桶 (calls=%d, cost=%s) 只能是 2 次/%s 或 0 次/%s",
				calls, rawCost, wantCost, zeroCost)
		}
	}
	if callsTotal != 2 {
		t.Fatalf("夹具用户的总计数应为 2（拦截行与 replay 行不计），实际 %d", callsTotal)
	}
	// 两行账本都落在当前小时桶里；跨小时边界时最多分成两个桶，故只钉「有数据 + 全空」的总数。
	if filledBuckets == 0 || filledBuckets+zeroBuckets != 24 {
		t.Fatalf("应有 24 个桶（含至少一个非空），实际非空 %d / 空 %d", filledBuckets, zeroBuckets)
	}

	// 7days 是 7 个日桶。
	code, body = integration.call(t, MountPrefix+"/dashboard/statistics?timeRange=7days")
	if code != http.StatusOK {
		t.Fatalf("7days 应得 200，实际 %d（%s）", code, body)
	}
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		t.Fatalf("7days 响应不是 JSON：%v", err)
	}
	if len(response.ChartData) != 7 || response.Resolution != "day" {
		t.Fatalf("7days 应为 7 个日桶，实际 %d 桶 / 分辨率 %q",
			len(response.ChartData), response.Resolution)
	}

	// 未提供 timeRange 时取默认 today。
	code, body = integration.call(t, MountPrefix+"/dashboard/statistics")
	if code != http.StatusOK {
		t.Fatalf("缺省 timeRange 应得 200，实际 %d（%s）", code, body)
	}
	if !strings.Contains(body, `"timeRange":"today"`) {
		t.Fatalf("缺省 timeRange 应回落 today：%s", body)
	}

	// 非法 timeRange 是 zod 形状的 400。
	code, body = integration.call(t, MountPrefix+"/dashboard/statistics?timeRange=nope")
	if code != http.StatusBadRequest {
		t.Fatalf("非法 timeRange 应得 400，实际 %d（%s）", code, body)
	}
	if !strings.Contains(body, "invalid_enum_value") || !strings.Contains(body, "timeRange") {
		t.Fatalf("400 正文应指明 timeRange 是非法枚举值：%s", body)
	}
}

// statisticsKeyTrendBody 是 key-trend 的判读形状（total_cost 保留原形以校验类型）。
type statisticsKeyTrendBody struct {
	Items []struct {
		KeyID     int64           `json:"key_id"`
		KeyName   string          `json:"key_name"`
		Date      string          `json:"date"`
		APICalls  int64           `json:"api_calls"`
		TotalCost json.RawMessage `json:"total_cost"`
	} `json:"items"`
}

// TestAdminUserInsightsKeyTrendEndToEnd 钉住密钥趋势的形状、类型分叉与边界。
func TestAdminUserInsightsKeyTrendEndToEnd(t *testing.T) {
	integration := newStatisticsIT(t)

	code, body := integration.call(t, fmt.Sprintf(
		"%s/admin/users/%d/insights/key-trend?timeRange=today", MountPrefix, integration.userID))
	if code != http.StatusOK {
		t.Fatalf("趋势应得 200，实际 %d（%s）", code, body)
	}
	var response statisticsKeyTrendBody
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		t.Fatalf("趋势响应不是 JSON：%v（%s）", err, body)
	}
	if len(response.Items) != 24 {
		t.Fatalf("today 应有 24 行（桶 × 1 个密钥），实际 %d", len(response.Items))
	}

	filled, empty := 0, 0
	for _, item := range response.Items {
		if item.KeyID != integration.keyID || item.KeyName != integration.keyName {
			t.Fatalf("行的密钥应为 (%d, %q)，实际 (%d, %q)",
				integration.keyID, integration.keyName, item.KeyID, item.KeyName)
		}
		// date 是 ISO 串（毫秒三位 + Z）。
		if _, err := time.Parse("2006-01-02T15:04:05.000Z", item.Date); err != nil {
			t.Fatalf("date 不是 ISO 串：%q（%v）", item.Date, err)
		}
		switch {
		case item.APICalls == 2:
			// 真实行：numeric 原形是**字符串**。
			if string(item.TotalCost) != `"2.000000000000000"` {
				t.Fatalf("真实行的 total_cost 应是 numeric 字符串，实际 %s", item.TotalCost)
			}
			filled++
		case item.APICalls == 0:
			// 零填充行：数字 0（不是字符串 "0"）。
			if string(item.TotalCost) != "0" {
				t.Fatalf("零填充行的 total_cost 应是数字 0，实际 %s", item.TotalCost)
			}
			empty++
		default:
			t.Fatalf("计数应为 0 或 2，实际 %d", item.APICalls)
		}
	}
	if filled == 0 || filled+empty != 24 {
		t.Fatalf("应有 24 行（含至少一个有数据的桶），实际有数据 %d / 空 %d", filled, empty)
	}

	// 7days 是 7 个日桶。
	code, body = integration.call(t, fmt.Sprintf(
		"%s/admin/users/%d/insights/key-trend?timeRange=7days", MountPrefix, integration.userID))
	if code != http.StatusOK {
		t.Fatalf("7days 应得 200，实际 %d（%s）", code, body)
	}
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		t.Fatalf("7days 响应不是 JSON：%v", err)
	}
	if len(response.Items) != 7 {
		t.Fatalf("7days 应有 7 行，实际 %d", len(response.Items))
	}

	// 不存在的用户没有密钥，故是空 items 的 200（Node 的 action 不查用户存在性）。
	code, body = integration.call(t, fmt.Sprintf(
		"%s/admin/users/%d/insights/key-trend", MountPrefix, integration.userID+1_000_000_000))
	if code != http.StatusOK {
		t.Fatalf("不存在的用户应得 200，实际 %d（%s）", code, body)
	}
	if !strings.Contains(body, `"items":[]`) {
		t.Fatalf("不存在的用户应得空 items 数组：%s", body)
	}

	// 非法 timeRange 是 zod 形状的 400。
	code, body = integration.call(t, fmt.Sprintf(
		"%s/admin/users/%d/insights/key-trend?timeRange=nope", MountPrefix, integration.userID))
	if code != http.StatusBadRequest {
		t.Fatalf("非法 timeRange 应得 400，实际 %d（%s）", code, body)
	}
	if !strings.Contains(body, "invalid_enum_value") {
		t.Fatalf("400 正文应指明非法枚举值：%s", body)
	}
}
