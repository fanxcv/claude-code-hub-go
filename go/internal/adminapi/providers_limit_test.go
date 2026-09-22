package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/limit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ratelimit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 providers 两条限额读数端点的用例：
//   - 纯函数与注册门（不需要外部依赖）；
//   - 集成用例（需要 CCH_TEST_DSN 与 CCH_TEST_REDIS_URL，缺则跳过）。
//
// 夹具自钉：供应商名/密钥原文都带 providersLimitITMarker 前缀，退出时按 id 清理；
// Redis 键走产品自己的键构造函数（limit.Cost5hKey / ProviderActiveSessionsKey），
// 因此用例同时钉住了「读的是产品那套键」，不是另造一套。

const providersLimitITMarker = "go-providers-limit-it"

// providersLimitStub5h 是 5h 固定窗口读取器的替身。
type providersLimitStub5h struct {
	state limit.Fixed5hState
	err   error
	calls int
	seen  []int64
}

func (stub *providersLimitStub5h) Fixed5hWindowState(
	_ context.Context,
	_ limit.Entity,
	id int64,
	_ time.Time,
) (limit.Fixed5hState, error) {
	stub.calls++
	stub.seen = append(stub.seen, id)
	return stub.state, stub.err
}

// providersLimitRouter 装配一个只注册这两条端点的路由（缺省依赖用替身补齐）。
func providersLimitRouter(
	t *testing.T,
	pools *store.Pools,
	sessions ObservedSessionRuntime,
	fixed5h ProviderFixed5hWindowReader,
) *Router {
	t.Helper()
	deps := Deps{
		Logger:   logx.New(nil),
		Guard:    principalGuard{principal: Principal{UserID: 1, Username: "admin", IsAdmin: true}},
		Store:    pools,
		Problems: NewProblems(nil),
	}
	if sessions != nil {
		deps.ObservedSessions = sessions
	}
	router := New(Options{Deps: deps})
	RegisterProvidersLimitRoutes(router, deps, ProvidersLimitOptions{Fixed5h: fixed5h})
	return router
}

// providersLimitCall 发一次请求并返回状态码与正文。
func providersLimitCall(t *testing.T, router *Router, method, path, body string) (int, string) {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.String()
}

// TestProvidersLimitRegistrationGate 钉住「三侧依赖齐备才注册」。
//
// 为什么必须整组不注册：缺账本聚合（PG）或会话计数/固定窗口（Redis）任一侧，
// 端点都会给出**恒 0 或错的**读数，而配额页上看起来是正常数字。
func TestProvidersLimitRegistrationGate(t *testing.T) {
	pools := testPools(t)
	sessions := &providersLimitStubSessions{}
	fixed5h := &providersLimitStub5h{}

	cases := []struct {
		name     string
		pools    *store.Pools
		sessions ObservedSessionRuntime
		fixed5h  ProviderFixed5hWindowReader
		want     int
	}{
		{"齐备", pools, sessions, fixed5h, 2},
		{"缺 store", nil, sessions, fixed5h, 0},
		{"缺会话计数", pools, nil, fixed5h, 0},
		{"缺 5h 固定窗口", pools, sessions, nil, 0},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			router := providersLimitRouter(t, testCase.pools, testCase.sessions, testCase.fixed5h)
			if got := router.RouteCount(); got != testCase.want {
				t.Fatalf("应注册 %d 条，实际 %d", testCase.want, got)
			}
		})
	}
}

// providersLimitStubSessions 是会话观测的替身（只实现本文件用到的部分）。
type providersLimitStubSessions struct {
	counts map[int64]int
	err    error
}

func (stub *providersLimitStubSessions) ObservedSessionCount(context.Context) (int, error) {
	return 0, nil
}

func (stub *providersLimitStubSessions) ProviderInFlightCounts(
	_ context.Context,
	providerIDs []int64,
) (map[int64]int, error) {
	if stub.err != nil {
		return nil, stub.err
	}
	counts := make(map[int64]int, len(providerIDs))
	for _, id := range providerIDs {
		counts[id] = stub.counts[id]
	}
	return counts, nil
}

func (stub *providersLimitStubSessions) ObservedSessionIdentities(context.Context) ([]string, error) {
	return nil, nil
}

// TestProviderLimitResetInfoText 钉住 cost5h.resetInfo 的三态文案（Node 逐字硬编码中文）。
func TestProviderLimitResetInfoText(t *testing.T) {
	if got := providerLimitResetInfoText(limit.ResetRolling, nil); got != "滚动窗口（5 小时）" {
		t.Fatalf("滚动窗口文案不符：%q", got)
	}
	if got := providerLimitResetInfoText(limit.ResetFixed, nil); got != "固定窗口（等待首次成功记账）" {
		t.Fatalf("固定窗口无重置时刻文案不符：%q", got)
	}
	at := time.Date(2026, 9, 13, 4, 5, 6, 700*int(time.Millisecond), time.UTC)
	want := "固定窗口（重置于 2026-09-13T04:05:06.700Z）"
	if got := providerLimitResetInfoText(limit.ResetFixed, &at); got != want {
		t.Fatalf("固定窗口有重置时刻文案不符：\n实际 %q\n期望 %q", got, want)
	}
}

// TestProviderLimitParseResetAt 钉住 total_cost_reset_at 文本的解析（含 Node 的 ISO 毫秒 Z 形状）。
func TestProviderLimitParseResetAt(t *testing.T) {
	cases := []struct {
		name string
		raw  *string
		want string
	}{
		{"nil", nil, ""},
		{"空串", strPtr(""), ""},
		{"ISO 毫秒 Z", strPtr("2026-09-13T04:05:06.700Z"), "2026-09-13T04:05:06.7Z"},
		{"带偏移", strPtr("2026-09-13T04:05:06+08:00"), "2026-09-12T20:05:06Z"},
		{"垃圾输入", strPtr("not-a-time"), ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := providerLimitParseResetAt(testCase.raw)
			if testCase.want == "" {
				if got != nil {
					t.Fatalf("应解析为 nil，实际 %v", *got)
				}
				return
			}
			want, err := time.Parse(time.RFC3339Nano, testCase.want)
			if err != nil {
				t.Fatalf("用例期望值不合法: %v", err)
			}
			if got == nil {
				t.Fatalf("应解析出时刻，实际 nil")
			}
			if !got.Equal(want) {
				t.Fatalf("解析结果不符：实际 %s，期望 %s", got.UTC(), want.UTC())
			}
		})
	}
}

func strPtr(value string) *string { return &value }

// TestProviderLimitBatchValidation 钉住 providerIds 的校验与 Node 的 strict 语义。
func TestProviderLimitBatchValidation(t *testing.T) {
	router := providersLimitRouter(t, &store.Pools{}, &providersLimitStubSessions{}, &providersLimitStub5h{})
	cases := []struct {
		name string
		body string
	}{
		{"未知键", `{"providerIds":[1],"extra":1}`},
		{"空数组", `{"providerIds":[]}`},
		{"缺 providerIds", `{}`},
		{"非正整数", `{"providerIds":[0]}`},
		{"非整数", `{"providerIds":[1.5]}`},
		{"超上限", `{"providerIds":[` + strings.TrimSuffix(strings.Repeat("1,", 501), ",") + `]}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			status, body := providersLimitCall(t, router, http.MethodPost, "/providers/limit-usage:batch", testCase.body)
			if status != http.StatusBadRequest {
				t.Fatalf("应 400，实际 %d：%.200s", status, body)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 集成用例
// ---------------------------------------------------------------------------

// providersLimitIT 是本文件集成用例的装配。
type providersLimitIT struct {
	router   *Router
	pools    *store.Pools
	redis    *redis.Client
	sessions ObservedSessionRuntime
	fixed5h  ProviderFixed5hWindowReader
	now      time.Time
	location *time.Location
}

// newProvidersLimitIT 装配真依赖：真库 + 真 Redis + 产品自己的键（不另造一套）。
func newProvidersLimitIT(t *testing.T) *providersLimitIT {
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

	client, sessions := dashboardRedisRuntime(t, 300)
	registry, err := ratelimit.Embedded()
	if err != nil {
		t.Fatalf("加载 Lua 注册表失败: %v", err)
	}
	scriptClient, err := ratelimit.New(client, registry)
	if err != nil {
		t.Fatalf("构造脚本客户端失败: %v", err)
	}
	fixed5h := limit.NewCostWindows(scriptClient, logx.New(nil))

	tz := "Asia/Shanghai"
	loc := config.ResolveLocationFromEnv(&tz)

	integration := &providersLimitIT{
		pools:    pools,
		redis:    client,
		sessions: sessions,
		fixed5h:  fixed5h,
		now:      time.Now(),
		location: loc,
	}
	integration.router = providersLimitRouter(t, pools, sessions, fixed5h)
	return integration
}

// seedProvider 种一个供应商并返回 id。
func (integration *providersLimitIT) seedProvider(t *testing.T, suffix string, overrides map[string]any) int64 {
	t.Helper()
	ctx := context.Background()
	pool, err := integration.pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	columns := []string{"name", "url", "key", "provider_type", "is_enabled", "weight", "priority", "group_tag"}
	values := []any{
		fmt.Sprintf("%s-%s-%d", providersLimitITMarker, suffix, time.Now().UnixNano()),
		"http://127.0.0.1:9", "upstream-not-used", "codex", true, 1, 0, "default",
	}
	for column, value := range overrides {
		if index := slices.Index(columns, column); index >= 0 {
			// 覆盖基底列（如 priority / provider_type），不得重复追加：列名重复会被 PG 拒。
			values[index] = value
			continue
		}
		columns = append(columns, column)
		values = append(values, value)
	}
	placeholders := make([]string, len(values))
	for index := range values {
		placeholders[index] = fmt.Sprintf("$%d", index+1)
	}
	query := fmt.Sprintf(
		"INSERT INTO providers (%s) VALUES (%s) RETURNING id",
		strings.Join(columns, ", "), strings.Join(placeholders, ", "))
	var providerID int64
	if err := pool.QueryRow(ctx, query, values...).Scan(&providerID); err != nil {
		t.Fatalf("建供应商失败: %v", err)
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		if _, err := pool.Exec(cleanup, `DELETE FROM usage_ledger WHERE final_provider_id = $1`, providerID); err != nil {
			t.Errorf("清理账本行失败: %v", err)
		}
		if _, err := pool.Exec(cleanup,
			`UPDATE providers SET is_enabled = false, deleted_at = now() WHERE id = $1`, providerID); err != nil {
			t.Errorf("清理供应商失败: %v", err)
		}
	})
	return providerID
}

// seedLedgerRow 种一行可计费账本（created_at 由调用方给定，避免日界依赖）。
func (integration *providersLimitIT) seedLedgerRow(
	t *testing.T,
	providerID int64,
	cost string,
	createdAt time.Time,
	blocked bool,
	replay bool,
	endpoint string,
) {
	t.Helper()
	ctx := context.Background()
	pool, err := integration.pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	var blockedBy *string
	if blocked {
		value := "sensitive_word"
		blockedBy = &value
	}
	var endpointValue *string
	if endpoint != "" {
		endpointValue = &endpoint
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO usage_ledger (request_id, provider_id, user_id, key, final_provider_id, model,
			endpoint, cost_usd, input_tokens, output_tokens, status_code, is_success, duration_ms,
			created_at, is_replay, blocked_by)
		VALUES ($1, $2, $3, $4, $2, $5, $6, $7::numeric, 10, 20, 200, true, 100, $8, $9, $10)`,
		int(time.Now().UnixNano()%1_000_000_000), providerID,
		int64(4242), "sk-"+providersLimitITMarker, providersLimitITMarker+"-model",
		endpointValue, cost, createdAt, replay, blockedBy); err != nil {
		t.Fatalf("种账本行失败: %v", err)
	}
}

// decodeProviderLimitUsage 承接单条的响应体。
type providerLimitUsageBody struct {
	Cost5h struct {
		Current   json.Number `json:"current"`
		Limit     *float64    `json:"limit"`
		ResetInfo string      `json:"resetInfo"`
	} `json:"cost5h"`
	CostDaily struct {
		Current json.Number `json:"current"`
		Limit   *float64    `json:"limit"`
		ResetAt *string     `json:"resetAt"`
	} `json:"costDaily"`
	CostWeekly struct {
		Current json.Number `json:"current"`
		ResetAt *string     `json:"resetAt"`
	} `json:"costWeekly"`
	CostMonthly struct {
		Current json.Number `json:"current"`
		ResetAt *string     `json:"resetAt"`
	} `json:"costMonthly"`
	LimitTotalUSD struct {
		Current json.Number `json:"current"`
		Limit   *float64    `json:"limit"`
		ResetAt *string     `json:"resetAt"`
	} `json:"limitTotalUsd"`
	ConcurrentSessions struct {
		Current int64 `json:"current"`
		Limit   int64 `json:"limit"`
	} `json:"concurrentSessions"`
}

func decodeProviderLimitUsage(t *testing.T, body string) providerLimitUsageBody {
	t.Helper()
	var decoded providerLimitUsageBody
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("解析响应失败: %v；正文 %.300s", err, body)
	}
	return decoded
}

// providerLimitNumberEquals 按**数值**比较响应里的金额。
//
// 为什么不做字面量比较：账本聚合出的是 PG numeric 的文本（如 `1.500000000000000`），
// 本包沿用 keys 侧 keysCostBucket 的同一约定直出（usersCostNumber 包裹成 json.Number），
// 而 Node 侧是 JS number（`1.5`）。两者**数值相等、文本不同**——故此处按
// 「数值叶 + 容差」比对，不按文本严格等值。
func providerLimitNumberEquals(t *testing.T, label string, got json.Number, want float64) {
	t.Helper()
	parsed, err := got.Float64()
	if err != nil {
		t.Fatalf("%s 不是合法数字：%q（%v）", label, got.String(), err)
	}
	if parsed != want {
		t.Fatalf("%s 应为 %v，实际 %v", label, want, parsed)
	}
}

// TestIntegrationProviderLimitUsageRollingAndBillingCondition 钉住 rolling 口径与计费条件。
//
// 行设计（时间锚在**产品同一套窗口函数**上，故不受日界/时区影响）：
//
//	in5h       now-1h            cost 1.5  → 计入 5h/daily(若落在今日)/weekly/monthly/total
//	out5h      now-6h            cost 2.5  → 5h 不计，其余窗口可能计入
//	blocked    与 in5h 同时刻     cost 9    → blocked_by 非空，一律不计
//	replay     与 in5h 同时刻     cost 9    → is_replay，一律不计
//	countTokens 与 in5h 同时刻    cost 9    → 非计费端点，一律不计
func TestIntegrationProviderLimitUsageRollingAndBillingCondition(t *testing.T) {
	integration := newProvidersLimitIT(t)
	providerID := integration.seedProvider(t, "rolling", map[string]any{
		"limit_5h_usd":                10.0,
		"limit_daily_usd":             20.0,
		"limit_weekly_usd":            30.0,
		"limit_monthly_usd":           40.0,
		"limit_total_usd":             100.0,
		"limit_concurrent_sessions":   3,
		"limit_5h_reset_mode":         "rolling",
		"daily_reset_mode":            "fixed",
		"daily_reset_time":            "00:00",
		"protocol_conversion_enabled": true,
	})

	now := integration.now
	integration.seedLedgerRow(t, providerID, "1.5", now.Add(-time.Hour), false, false, "")
	integration.seedLedgerRow(t, providerID, "2.5", now.Add(-6*time.Hour), false, false, "")
	integration.seedLedgerRow(t, providerID, "9", now.Add(-time.Hour), true, false, "")
	integration.seedLedgerRow(t, providerID, "9", now.Add(-time.Hour), false, true, "")
	integration.seedLedgerRow(t, providerID, "9", now.Add(-time.Hour), false, false, "/v1/messages/count_tokens")

	status, body := providersLimitCall(t, integration.router, http.MethodGet,
		fmt.Sprintf("/providers/%d/limit-usage", providerID), "")
	if status != http.StatusOK {
		t.Fatalf("应 200，实际 %d：%.300s", status, body)
	}
	usage := decodeProviderLimitUsage(t, body)

	// 5h rolling：只有 now-1h 那一行（now-6h 出窗；三行不计费行被计费条件排除）。
	providerLimitNumberEquals(t, "cost5h", usage.Cost5h.Current, 1.5)
	if usage.Cost5h.ResetInfo != "滚动窗口（5 小时）" {
		t.Fatalf("rolling 的 resetInfo 不符：%q", usage.Cost5h.ResetInfo)
	}
	if usage.Cost5h.Limit == nil || *usage.Cost5h.Limit != 10 {
		t.Fatalf("cost5h.limit 应为 10，实际 %v", usage.Cost5h.Limit)
	}
	// 总额度：无 totalCostResetAt ⇒ 全时段，等于两行可计费之和。
	providerLimitNumberEquals(t, "limitTotalUsd", usage.LimitTotalUSD.Current, 4)
	if usage.LimitTotalUSD.ResetAt != nil {
		t.Fatalf("totalCostResetAt 为空时 resetAt 应缺席，实际 %v", *usage.LimitTotalUSD.ResetAt)
	}
	if usage.ConcurrentSessions.Limit != 3 {
		t.Fatalf("并发上限应为 3，实际 %d", usage.ConcurrentSessions.Limit)
	}

	// daily/weekly/monthly 的期望值由**同一套窗口函数**算出：
	// 这与端点用的是同一套语义，故用例钉的是「接线是否用对了窗口与计费条件」，
	// 而不是重复验证窗口数学本身（窗口数学另有 internal/limit 的用例）。
	expectInWindow := func(start time.Time) float64 {
		total := 0.0
		for _, row := range []struct {
			cost float64
			at   time.Time
		}{{1.5, now.Add(-time.Hour)}, {2.5, now.Add(-6 * time.Hour)}} {
			if !row.at.Before(start) && row.at.Before(now) {
				total += row.cost
			}
		}
		return total
	}
	dailyStart := limit.WindowStart(limit.PeriodDaily, now, "00:00", limit.ResetFixed, integration.location)
	weeklyStart := limit.WindowStart(limit.PeriodWeekly, now, "00:00", limit.ResetFixed, integration.location)
	monthlyStart := limit.WindowStart(limit.PeriodMonthly, now, "00:00", limit.ResetFixed, integration.location)
	providerLimitNumberEquals(t, "costDaily", usage.CostDaily.Current, expectInWindow(dailyStart))
	providerLimitNumberEquals(t, "costWeekly", usage.CostWeekly.Current, expectInWindow(weeklyStart))
	providerLimitNumberEquals(t, "costMonthly", usage.CostMonthly.Current, expectInWindow(monthlyStart))
	// daily fixed ⇒ 必给 resetAt（Node 的 resetWeekly.resetAt! 同判）。
	if usage.CostDaily.ResetAt == nil {
		t.Fatalf("daily 固定窗口应给 resetAt")
	}
	if usage.CostWeekly.ResetAt == nil || usage.CostMonthly.ResetAt == nil {
		t.Fatalf("weekly/monthly 应给 resetAt")
	}
}

// TestIntegrationProviderLimitUsageFixed5hFromRedis 钉住 5h 固定窗口走 Redis 运行态窗口。
func TestIntegrationProviderLimitUsageFixed5hFromRedis(t *testing.T) {
	integration := newProvidersLimitIT(t)
	providerID := integration.seedProvider(t, "fixed5h", map[string]any{
		"limit_5h_usd":        5.0,
		"limit_5h_reset_mode": "fixed",
		"daily_reset_mode":    "fixed",
		"daily_reset_time":    "00:00",
	})
	ctx := context.Background()
	key := limit.Cost5hKey(limit.EntityProvider, providerID, limit.ResetFixed)
	ttl := 7800 * time.Second
	if err := integration.redis.Set(ctx, key, "1.25", ttl).Err(); err != nil {
		t.Fatalf("写 5h 固定窗口失败: %v", err)
	}
	t.Cleanup(func() { _ = integration.redis.Del(context.Background(), key).Err() })

	// 账本里放一行更大的金额：固定模式下**不得**读账本（Node 走 Redis 快路径）。
	integration.seedLedgerRow(t, providerID, "99", integration.now.Add(-time.Hour), false, false, "")

	status, body := providersLimitCall(t, integration.router, http.MethodGet,
		fmt.Sprintf("/providers/%d/limit-usage", providerID), "")
	if status != http.StatusOK {
		t.Fatalf("应 200，实际 %d：%.300s", status, body)
	}
	usage := decodeProviderLimitUsage(t, body)
	providerLimitNumberEquals(t, "固定模式的 cost5h", usage.Cost5h.Current, 1.25)
	wantPrefix := "固定窗口（重置于 "
	if !strings.HasPrefix(usage.Cost5h.ResetInfo, wantPrefix) {
		t.Fatalf("固定模式 resetInfo 前缀不符：%q", usage.Cost5h.ResetInfo)
	}
	// 重置时刻应落在 now+ttl 附近（容差 30s，覆盖读与写之间的耗时）。
	raw := strings.TrimSuffix(strings.TrimPrefix(usage.Cost5h.ResetInfo, wantPrefix), "）")
	resetAt, err := time.Parse("2006-01-02T15:04:05.000Z", raw)
	if err != nil {
		t.Fatalf("resetInfo 里的时刻不可解析：%q（%v）", raw, err)
	}
	delta := resetAt.Sub(integration.now.Add(ttl))
	if delta < -30*time.Second || delta > 30*time.Second {
		t.Fatalf("重置时刻应约为 now+ttl，实际差 %s（%s vs %s）", delta, resetAt, integration.now.Add(ttl))
	}
}

// TestIntegrationProviderLimitSessionCountFromRedis 钉住并发会话数从 Redis 实读。
func TestIntegrationProviderLimitSessionCountFromRedis(t *testing.T) {
	integration := newProvidersLimitIT(t)
	providerID := integration.seedProvider(t, "sessions", map[string]any{
		"limit_5h_reset_mode":       "rolling",
		"daily_reset_mode":          "fixed",
		"daily_reset_time":          "00:00",
		"limit_concurrent_sessions": 7,
	})
	ctx := context.Background()
	activeKey := limit.ProviderActiveSessionsKey(providerID)
	sessionIDs := []string{providersLimitITMarker + "-s1", providersLimitITMarker + "-s2"}
	for _, sessionID := range sessionIDs {
		if err := integration.redis.Set(ctx, "session:"+sessionID+":info", "{}", 5*time.Minute).Err(); err != nil {
			t.Fatalf("写会话详情失败: %v", err)
		}
	}
	if err := integration.redis.ZAdd(ctx, activeKey,
		redis.Z{Score: float64(time.Now().UnixMilli()), Member: sessionIDs[0]},
		redis.Z{Score: float64(time.Now().UnixMilli()), Member: sessionIDs[1]},
	).Err(); err != nil {
		t.Fatalf("写活跃会话集合失败: %v", err)
	}
	t.Cleanup(func() {
		cleanup := context.Background()
		_ = integration.redis.Del(cleanup, activeKey).Err()
		for _, sessionID := range sessionIDs {
			_ = integration.redis.Del(cleanup, "session:"+sessionID+":info").Err()
		}
	})

	status, body := providersLimitCall(t, integration.router, http.MethodGet,
		fmt.Sprintf("/providers/%d/limit-usage", providerID), "")
	if status != http.StatusOK {
		t.Fatalf("应 200，实际 %d：%.300s", status, body)
	}
	usage := decodeProviderLimitUsage(t, body)
	if usage.ConcurrentSessions.Current != 2 {
		t.Fatalf("并发会话数应为 2，实际 %d（正文 %.300s）", usage.ConcurrentSessions.Current, body)
	}
}

// TestIntegrationProviderLimitBatchVisibilityAndOrder 钉住批量版的可见性、顺序与缺席语义。
//
// 顺序：Node 是按**可见列表顺序**（providers 列表 = priority, id）过滤出交集，
// 不是请求里的 providerIds 顺序；隐藏类型（claude-auth / gemini-cli）与不可见 id 直接缺席。
func TestIntegrationProviderLimitBatchVisibilityAndOrder(t *testing.T) {
	integration := newProvidersLimitIT(t)
	// 可见性排序靠 priority：先建的 priority 更大 ⇒ 在列表里更靠后。
	later := integration.seedProvider(t, "order-later", map[string]any{
		"priority": 5, "limit_5h_reset_mode": "rolling", "daily_reset_mode": "fixed", "daily_reset_time": "00:00",
	})
	earlier := integration.seedProvider(t, "order-earlier", map[string]any{
		"priority": 0, "limit_5h_reset_mode": "rolling", "daily_reset_mode": "fixed", "daily_reset_time": "00:00",
	})
	hidden := integration.seedProvider(t, "hidden", map[string]any{
		"provider_type": "claude-auth", "priority": 1,
		"limit_5h_reset_mode": "rolling", "daily_reset_mode": "fixed", "daily_reset_time": "00:00",
	})
	integration.seedLedgerRow(t, earlier, "1.5", integration.now.Add(-time.Hour), false, false, "")

	// 批量：请求顺序故意倒置，且带上隐藏 id 与一个不存在的 id。
	payload := fmt.Sprintf(`{"providerIds":[%d,%d,%d,999999]}`, later, hidden, earlier)
	status, body := providersLimitCall(t, integration.router, http.MethodPost,
		"/providers/limit-usage:batch", payload)
	if status != http.StatusOK {
		t.Fatalf("应 200，实际 %d：%.300s", status, body)
	}
	var decoded struct {
		Items []struct {
			ID    int64                  `json:"id"`
			Usage providerLimitUsageBody `json:"usage"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("解析批量响应失败: %v；正文 %.300s", err, body)
	}
	if len(decoded.Items) != 2 {
		t.Fatalf("应只回两条可见供应商，实际 %d：%.300s", len(decoded.Items), body)
	}
	if decoded.Items[0].ID != earlier || decoded.Items[1].ID != later {
		t.Fatalf("顺序应随可见列表（priority 升序），实际 [%d %d]",
			decoded.Items[0].ID, decoded.Items[1].ID)
	}
	if got := decoded.Items[0].Usage.Cost5h.Current.String(); got == "" {
		t.Fatalf("earlier 的 cost5h 缺失")
	}
	providerLimitNumberEquals(t, "earlier 的 cost5h", decoded.Items[0].Usage.Cost5h.Current, 1.5)
	providerLimitNumberEquals(t, "later 的 cost5h", decoded.Items[1].Usage.Cost5h.Current, 0)

	// 单条：隐藏类型 ⇒ 404 provider.not_found（与 Node 的 findVisibleProvider 同判）。
	status, body = providersLimitCall(t, integration.router, http.MethodGet,
		fmt.Sprintf("/providers/%d/limit-usage", hidden), "")
	if status != http.StatusNotFound {
		t.Fatalf("隐藏类型应 404，实际 %d：%.200s", status, body)
	}
	if !strings.Contains(body, "provider.not_found") {
		t.Fatalf("404 正文应带 provider.not_found：%.200s", body)
	}
}

// TestIntegrationProviderLimitProviderScoped 钉住账本聚合按 final_provider_id 隔离。
func TestIntegrationProviderLimitProviderScoped(t *testing.T) {
	integration := newProvidersLimitIT(t)
	target := integration.seedProvider(t, "scope-target", map[string]any{
		"limit_5h_reset_mode": "rolling", "daily_reset_mode": "fixed", "daily_reset_time": "00:00",
	})
	other := integration.seedProvider(t, "scope-other", map[string]any{
		"limit_5h_reset_mode": "rolling", "daily_reset_mode": "fixed", "daily_reset_time": "00:00",
	})
	integration.seedLedgerRow(t, target, "1.5", integration.now.Add(-time.Hour), false, false, "")
	integration.seedLedgerRow(t, other, "7", integration.now.Add(-time.Hour), false, false, "")

	status, body := providersLimitCall(t, integration.router, http.MethodGet,
		fmt.Sprintf("/providers/%d/limit-usage", target), "")
	if status != http.StatusOK {
		t.Fatalf("应 200，实际 %d：%.300s", status, body)
	}
	usage := decodeProviderLimitUsage(t, body)
	providerLimitNumberEquals(t, "目标供应商的 cost5h", usage.Cost5h.Current, 1.5)
	providerLimitNumberEquals(t, "目标供应商的 limitTotalUsd", usage.LimitTotalUSD.Current, 1.5)
}
