package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是熔断日志端点（provider_circuit_logs.go）的真库 + 真 Redis 集成用例。
//
// 为什么必须真库真 Redis：
//   - 熔断状态块的契约是「键布局与字段序列化同 Node 逐字一致」，替身正好会把这件事测掉；
//   - 错误列表块的契约是「两条来源合并 + jsonb 展开」，替身测不出 SQL。
//
// 夹具纪律：供应商夹具用既有 seedProviders（唯一前缀 + 按前缀清理）；错误行按唯一 key 清理。

// circuitLogsRouter 造真路由表：真 Store + 传入的熔断面 + 固定管理员身份。
func circuitLogsRouter(t *testing.T, pools *store.Pools, states CircuitStateStore) *Router {
	t.Helper()
	deps := Deps{
		Logger:        logx.New(nil),
		Guard:         principalGuard{principal: Principal{UserID: 1, Username: "admin", IsAdmin: true}},
		Store:         pools,
		Problems:      NewProblems(nil),
		CircuitStates: states,
	}
	router := New(Options{Deps: deps})
	RegisterProviderCircuitLogs(router, deps)
	return router
}

// circuitLogsPayload 承接响应体。
type circuitLogsPayload struct {
	ProviderID int64 `json:"providerId"`
	Circuit    struct {
		Available            bool    `json:"available"`
		CircuitState         *string `json:"circuitState"`
		FailureCount         *int64  `json:"failureCount"`
		LastFailureTime      *int64  `json:"lastFailureTime"`
		CircuitOpenUntil     *int64  `json:"circuitOpenUntil"`
		HalfOpenSuccessCount *int64  `json:"halfOpenSuccessCount"`
		RecoveryMinutes      *int64  `json:"recoveryMinutes"`
		// 等待阶梯的三项（Go 侧增强）。
		ConsecutiveOpenCount          *int64  `json:"consecutiveOpenCount"`
		ConsecutiveOpenCountChangedAt *int64  `json:"consecutiveOpenCountChangedAt"`
		OpenWindowMinutes             *int64  `json:"openWindowMinutes"`
		UnavailableReason             *string `json:"unavailableReason"`
	} `json:"circuit"`
	Thresholds struct {
		FailureThreshold         int64 `json:"failureThreshold"`
		OpenDuration             int64 `json:"openDuration"`
		HalfOpenSuccessThreshold int64 `json:"halfOpenSuccessThreshold"`
	} `json:"thresholds"`
	Window struct {
		Limit         int    `json:"limit"`
		LookbackHours int    `json:"lookbackHours"`
		Since         string `json:"since"`
	} `json:"window"`
	Errors []struct {
		RequestID    int64   `json:"requestId"`
		CreatedAt    string  `json:"createdAt"`
		Model        *string `json:"model"`
		StatusCode   *int    `json:"statusCode"`
		ErrorMessage *string `json:"errorMessage"`
		DurationMS   *int    `json:"durationMs"`
		Endpoint     *string `json:"endpoint"`
		Source       string  `json:"source"`
		ChainReason  *string `json:"chainReason"`
		Redacted     bool    `json:"redacted"`
	} `json:"errors"`
	ErrorsUnavailableReason *string `json:"errorsUnavailableReason"`
}

// circuitLogsGet 打一发本端点并解出响应体（同时返回原始 body 供「不出现敏感片段」的断言用）。
func circuitLogsGet(t *testing.T, router *Router, providerID int64, query string) (int, string, circuitLogsPayload) {
	t.Helper()
	target := fmt.Sprintf("/api/v1/providers/%d/circuit-logs", providerID)
	if query != "" {
		target += "?" + query
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK {
		return recorder.Code, body, circuitLogsPayload{}
	}
	var payload circuitLogsPayload
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("解析熔断日志响应失败: %v（原文 %.300s）", err, body)
	}
	return recorder.Code, body, payload
}

// seedCircuitLogsError 插一行失败请求（带唯一 key，供按 key 清理）。
func seedCircuitLogsError(
	t *testing.T,
	pools *store.Pools,
	providerID int64,
	key string,
	status int,
	errMsg string,
	chain []map[string]any,
	minutesAgo int,
) int64 {
	t.Helper()
	ctx := context.Background()
	pool, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	var chainJSON *string
	if chain != nil {
		raw, marshalErr := json.Marshal(chain)
		if marshalErr != nil {
			t.Fatalf("序列化链夹具失败: %v", marshalErr)
		}
		text := string(raw)
		chainJSON = &text
	}
	var id int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO message_request (
			provider_id, user_id, key, model, original_model, endpoint,
			status_code, duration_ms, cost_usd, is_replay, error_message,
			provider_chain, created_at, updated_at
		) VALUES (
			$1, 1, $2, $3, $3, '/v1/responses',
			$4, 77, 0::numeric, false, NULLIF($5, ''),
			$6::jsonb, now() - ($7::int * interval '1 minute'), now()
		) RETURNING id`,
		providerID, key, "it-model-"+key, status, errMsg, chainJSON, minutesAgo,
	).Scan(&id); err != nil {
		t.Fatalf("插错误夹具失败: %v", err)
	}
	t.Cleanup(func() {
		cleanup, cleanupErr := pools.Control()
		if cleanupErr != nil {
			return
		}
		_, _ = cleanup.Exec(ctx, `DELETE FROM usage_ledger WHERE request_id = $1`, id)
		_, _ = cleanup.Exec(ctx, `DELETE FROM message_request WHERE id = $1`, id)
	})
	return id
}

// TestCircuitLogsMergesStateAndErrors 是主用例：熔断状态来自 Redis、错误来自库，两块逐字段对齐。
func TestCircuitLogsMergesStateAndErrors(t *testing.T) {
	pools := testPools(t)
	fixture := seedProviders(t, pools)
	client := circuitTestRedis(t)
	ctx := context.Background()

	openUntil := time.Now().Add(30 * time.Minute).UnixMilli()
	lastFailure := time.Now().Add(-3 * time.Minute).UnixMilli()
	// 级数推进时刻：与窗口结束相差 35 分钟（即“本次窗口 35 分钟”），用于验证窗口时长
	// 是由哈希差值得出的，而不是在这里重算阶梯公式。
	ladderChangedAt := time.Now().Add(-5 * time.Minute).UnixMilli()
	key := providerCircuitKey(fixture.enabledID)
	// 写的是 Node 的形态：null 用空串（serializeState:56-68）；两个阶梯键是 Go 侧增强。
	if err := client.HSet(ctx, key, map[string]string{
		"failureCount":                  "4",
		"lastFailureTime":               strconv.FormatInt(lastFailure, 10),
		"circuitState":                  "open",
		"circuitOpenUntil":              strconv.FormatInt(openUntil, 10),
		"halfOpenSuccessCount":          "0",
		"consecutiveOpenCount":          "3",
		"consecutiveOpenCountChangedAt": strconv.FormatInt(ladderChangedAt, 10),
	}).Err(); err != nil {
		t.Fatalf("写熔断夹具失败: %v", err)
	}
	t.Cleanup(func() { _ = client.Del(context.Background(), key).Err() })

	// 两条错误：一条行级（503），一条只在链里（目标供应商在链上 404，行级属别家）。
	marker := fixture.prefix + "-clog"
	directID := seedCircuitLogsError(t, pools, fixture.enabledID, marker+"-direct", 503, "upstream_error", []map[string]any{
		{"id": fixture.enabledID, "name": marker, "statusCode": 503, "reason": "retry_failed"},
	}, 20)
	chainID := seedCircuitLogsError(t, pools, fixture.otherID, marker+"-chain", 404, "", []map[string]any{
		{"id": fixture.enabledID, "name": marker, "statusCode": 404, "errorMessage": "resource_not_found", "reason": "hedge_launched"},
		{"id": fixture.otherID, "name": marker + "-other", "statusCode": 200, "reason": "hedge_winner"},
	}, 10)

	router := circuitLogsRouter(t, pools, NewRedisCircuitStates(client, nil, nil))
	status, _, payload := circuitLogsGet(t, router, fixture.enabledID, "")
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", status)
	}

	// ① 熔断状态块与 Redis 逐字段一致
	if !payload.Circuit.Available {
		t.Fatal("Redis 可用时 Available 应为 true")
	}
	if payload.Circuit.CircuitState == nil || *payload.Circuit.CircuitState != "open" {
		t.Errorf("circuitState 应为 open，收到 %v", payload.Circuit.CircuitState)
	}
	if payload.Circuit.FailureCount == nil || *payload.Circuit.FailureCount != 4 {
		t.Errorf("failureCount 应为 4，收到 %v", payload.Circuit.FailureCount)
	}
	if payload.Circuit.LastFailureTime == nil || *payload.Circuit.LastFailureTime != lastFailure {
		t.Errorf("lastFailureTime 应为 %d，收到 %v", lastFailure, payload.Circuit.LastFailureTime)
	}
	if payload.Circuit.CircuitOpenUntil == nil || *payload.Circuit.CircuitOpenUntil != openUntil {
		t.Errorf("circuitOpenUntil 应为 %d，收到 %v", openUntil, payload.Circuit.CircuitOpenUntil)
	}
	// recoveryMinutes 是 ceil 到分钟：30 分钟窗口应落在 30（允许 29~30 的时钟抖动）
	if payload.Circuit.RecoveryMinutes == nil || *payload.Circuit.RecoveryMinutes < 29 || *payload.Circuit.RecoveryMinutes > 30 {
		t.Errorf("recoveryMinutes 应约为 30，收到 %v", payload.Circuit.RecoveryMinutes)
	}
	// 等待阶梯：级数与最近一次变化时间原样返回，窗口时长由哈希差值得出（35 分钟）。
	if payload.Circuit.ConsecutiveOpenCount == nil || *payload.Circuit.ConsecutiveOpenCount != 3 {
		t.Errorf("consecutiveOpenCount 应为 3，收到 %v", payload.Circuit.ConsecutiveOpenCount)
	}
	if payload.Circuit.ConsecutiveOpenCountChangedAt == nil ||
		*payload.Circuit.ConsecutiveOpenCountChangedAt != ladderChangedAt {
		t.Errorf("consecutiveOpenCountChangedAt 应为 %d，收到 %v",
			ladderChangedAt, payload.Circuit.ConsecutiveOpenCountChangedAt)
	}
	if payload.Circuit.OpenWindowMinutes == nil || *payload.Circuit.OpenWindowMinutes != 35 {
		t.Errorf("openWindowMinutes 应为 35，收到 %v", payload.Circuit.OpenWindowMinutes)
	}

	// ② 阈値块来自供应商行（seedProviders 未设该列 → 出厂默认）
	if payload.Thresholds.FailureThreshold == 0 {
		t.Errorf("阈値应给出非零的出厂默认，收到 %+v", payload.Thresholds)
	}

	// ③ 时间窗要显示出来（否则「24 小时内无错误」会被误读成「从无错误」）
	if payload.Window.Limit != circuitLogsDefaultLimit || payload.Window.LookbackHours != circuitLogsDefaultLookbackHours {
		t.Errorf("默认窗口应为 limit=%d/lookbackHours=%d，收到 %+v",
			circuitLogsDefaultLimit, circuitLogsDefaultLookbackHours, payload.Window)
	}
	if payload.Window.Since == "" {
		t.Error("window.since 不应为空")
	}

	// ④ 两条错误都在，且字段逐个对齐
	byID := make(map[int64]int, len(payload.Errors))
	for index, row := range payload.Errors {
		byID[row.RequestID] = index
	}
	directIndex, ok := byID[directID]
	if !ok {
		t.Fatalf("行级失败 %d 未出现：%+v", directID, payload.Errors)
	}
	if payload.Errors[directIndex].Source != "direct" {
		t.Errorf("行级失败 source 应为 direct，收到 %q", payload.Errors[directIndex].Source)
	}
	if payload.Errors[directIndex].StatusCode == nil || *payload.Errors[directIndex].StatusCode != 503 {
		t.Errorf("行级失败 statusCode 应为 503，收到 %v", payload.Errors[directIndex].StatusCode)
	}
	if payload.Errors[directIndex].ErrorMessage == nil || *payload.Errors[directIndex].ErrorMessage != "upstream_error" {
		t.Errorf("行级失败 errorMessage 应为 upstream_error，收到 %v", payload.Errors[directIndex].ErrorMessage)
	}

	chainIndex, ok := byID[chainID]
	if !ok {
		t.Fatalf("链内失败 %d 未出现（只查行级 provider_id 会漏掉它）：%+v", chainID, payload.Errors)
	}
	if payload.Errors[chainIndex].Source != "chain" {
		t.Errorf("链内失败 source 应为 chain，收到 %q", payload.Errors[chainIndex].Source)
	}
	if payload.Errors[chainIndex].StatusCode == nil || *payload.Errors[chainIndex].StatusCode != 404 {
		t.Errorf("链内失败应暴露链项的 404，收到 %v", payload.Errors[chainIndex].StatusCode)
	}
	if payload.Errors[chainIndex].ErrorMessage == nil || *payload.Errors[chainIndex].ErrorMessage != "resource_not_found" {
		t.Errorf("链内失败应暴露链项的 errorMessage，收到 %v", payload.Errors[chainIndex].ErrorMessage)
	}
	if payload.Errors[chainIndex].ChainReason == nil || *payload.Errors[chainIndex].ChainReason != "hedge_launched" {
		t.Errorf("链内失败应带链上的 reason，收到 %v", payload.Errors[chainIndex].ChainReason)
	}

	// ⑤ 排序：时间倒序（链内那条 10 分钟前，行级那条 20 分钟前）
	if len(payload.Errors) >= 2 && payload.Errors[0].RequestID != chainID {
		t.Errorf("应按时间倒序（最近在前），首条应为 %d，收到 %d", chainID, payload.Errors[0].RequestID)
	}
}

// TestCircuitLogsLimitAndLookbackBounds 钉住两个参数的边界与校验码。
func TestCircuitLogsLimitAndLookbackBounds(t *testing.T) {
	pools := testPools(t)
	fixture := seedProviders(t, pools)
	router := circuitLogsRouter(t, pools, NewRedisCircuitStates(circuitTestRedis(t), nil, nil))

	cases := []struct {
		name   string
		query  string
		status int
		code   string
	}{
		{"limit=0", "limit=0", http.StatusBadRequest, "too_small"},
		{"limit=1", "limit=1", http.StatusOK, ""},
		{"limit=20", "limit=20", http.StatusOK, ""},
		{"limit=100", "limit=100", http.StatusOK, ""},
		{"limit=101", "limit=101", http.StatusBadRequest, "too_big"},
		{"limit=abc", "limit=abc", http.StatusBadRequest, "invalid_type"},
		{"lookbackHours=0", "lookbackHours=0", http.StatusBadRequest, "too_small"},
		{"lookbackHours=1", "lookbackHours=1", http.StatusOK, ""},
		{"lookbackHours=168", "lookbackHours=168", http.StatusOK, ""},
		{"lookbackHours=169", "lookbackHours=169", http.StatusBadRequest, "too_big"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			status, body, _ := circuitLogsGet(t, router, fixture.enabledID, testCase.query)
			if status != testCase.status {
				t.Fatalf("状态码 = %d，期望 %d（原文 %.200s）", status, testCase.status, body)
			}
			if testCase.code == "" {
				return
			}
			// 校验失败的信封必须带 zod 语义码，前端据此渲染「最小/最大值」提示。
			if !circuitLogsContains(body, testCase.code) {
				t.Fatalf("校验失败响应应含码 %q，原文 %.300s", testCase.code, body)
			}
		})
	}
}

// TestCircuitLogsDegradesWhenRedisUnavailable 钉住「Redis 读不到」的降级：200 + available=false + 错误列表照给。
//
// 这是本端点最要紧的一条韧性契约：熔断日志是**排障入口**，Redis 出问题时恰恰最需要它还能打开。
func TestCircuitLogsDegradesWhenRedisUnavailable(t *testing.T) {
	pools := testPools(t)
	fixture := seedProviders(t, pools)
	marker := fixture.prefix + "-clog-degraded"
	seedCircuitLogsError(t, pools, fixture.enabledID, marker, 503, "upstream_error", nil, 5)

	states := failingCircuitStates{err: errors.New("simulated redis outage")}
	router := circuitLogsRouter(t, pools, states)

	status, _, payload := circuitLogsGet(t, router, fixture.enabledID, "")
	if status != http.StatusOK {
		t.Fatalf("Redis 不可用时仍应 200（排障入口不能坏），实际 %d", status)
	}
	if payload.Circuit.Available {
		t.Error("Redis 读失败时 Available 应为 false")
	}
	// **不用 0 冒充**：数值字段必须缺席，而不是 0
	if payload.Circuit.FailureCount != nil || payload.Circuit.CircuitState != nil ||
		payload.Circuit.CircuitOpenUntil != nil || payload.Circuit.RecoveryMinutes != nil {
		t.Errorf("不可用时数值字段应为 null，实际 %+v", payload.Circuit)
	}
	if payload.Circuit.UnavailableReason == nil || *payload.Circuit.UnavailableReason == "" {
		t.Error("不可用时应给出 unavailableReason")
	}
	// 库可用 → 错误列表必须照给
	if len(payload.Errors) == 0 {
		t.Error("Redis 挂了不应影响错误列表（两块独立降级）")
	}
}

// TestRegisterProviderCircuitLogsSkipsWhenStoreMissing 钉住 fail-closed：连接池未装配时不注册该路由。
//
// 为什么 Store 也是注册前提（与同族 RegisterProviderCircuitRoutes 同）：可见性检查要走 PG，
// 没有 Store 就无法回答「这个供应商对这个请求可见吗」——宁可原样回退 Node，也不放行未校可见性的读。
func TestRegisterProviderCircuitLogsSkipsWhenStoreMissing(t *testing.T) {
	client := circuitTestRedis(t)
	deps := Deps{
		Logger:        logx.New(nil),
		Guard:         principalGuard{principal: Principal{UserID: 1, Username: "admin", IsAdmin: true}},
		Problems:      NewProblems(nil),
		CircuitStates: NewRedisCircuitStates(client, nil, nil),
		// Store 刻意留空
	}
	router := New(Options{Deps: deps})
	RegisterProviderCircuitLogs(router, deps)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/providers/1/circuit-logs", nil))
	if recorder.Code == http.StatusOK {
		t.Fatalf("Store 未装配时不应注册该路由（可见性无法校验），实际 %d", recorder.Code)
	}
}

// TestCircuitLogsRedactsCredentialShapes 是脱敏的反证用例。
//
// 造三类真实世界会出现在上游错误里的敏感形态，断言它们**不出现在响应里**，且行级 redacted=true；
// 同时用一个干净文案断言 redacted=false（否则「永远为 true」也能骗过前一半）。
func TestCircuitLogsRedactsCredentialShapes(t *testing.T) {
	pools := testPools(t)
	fixture := seedProviders(t, pools)
	router := circuitLogsRouter(t, pools, NewRedisCircuitStates(circuitTestRedis(t), nil, nil))
	marker := fixture.prefix + "-clog-redact"

	const (
		bearerSecret = "Bearer abcDEF123456ghiJKL"
		apiKeySecret = "sk-proj-ABCDEFGHIJKLMNOPQRSTUVWX"
		urlSecret    = "https://leaky-user:leaky-pass@upstream.invalid/v1"
		emailSecret  = "ops-leak@example.com"
	)
	dirtyID := seedCircuitLogsError(t, pools, fixture.enabledID, marker+"-dirty", 500,
		fmt.Sprintf("dial %s failed with %s and %s contact %s", urlSecret, bearerSecret, apiKeySecret, emailSecret),
		nil, 5)
	cleanID := seedCircuitLogsError(t, pools, fixture.enabledID, marker+"-clean", 502,
		"upstream_error", nil, 4)

	status, body, payload := circuitLogsGet(t, router, fixture.enabledID, "")
	if status != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200", status)
	}

	for _, fragment := range []string{
		"leaky-user", "leaky-pass", bearerSecret, apiKeySecret, emailSecret, "sk-proj-",
	} {
		if circuitLogsContains(body, fragment) {
			t.Errorf("响应里不应出现敏感片段 %q；原文 %.400s", fragment, body)
		}
	}

	byID := make(map[int64]int, len(payload.Errors))
	for index, row := range payload.Errors {
		byID[row.RequestID] = index
	}
	if index, ok := byID[dirtyID]; ok {
		if !payload.Errors[index].Redacted {
			t.Error("含敏感形态的那条应标 redacted=true")
		}
		// 替换文案与 Node 一致（值形态规则的占位符）
		message := ""
		if payload.Errors[index].ErrorMessage != nil {
			message = *payload.Errors[index].ErrorMessage
		}
		if !circuitLogsContains(message, "[REDACTED") && !circuitLogsContains(message, "[EMAIL]") {
			t.Errorf("脱敏后应出现占位符，实际 %q", message)
		}
	} else {
		t.Fatalf("含敏感形态的错误 %d 未出现在结果里", dirtyID)
	}
	if index, ok := byID[cleanID]; ok {
		if payload.Errors[index].Redacted {
			t.Error("干净文案不应标 redacted=true（否则该标记失去信息量）")
		}
	} else {
		t.Fatalf("干净错误 %d 未出现在结果里", cleanID)
	}
}

// TestRegisterProviderCircuitLogsSkipsWhenUnwired 钉住 fail-closed：熔断面未接线时不注册该路由。
func TestRegisterProviderCircuitLogsSkipsWhenUnwired(t *testing.T) {
	deps := Deps{
		Logger:   logx.New(nil),
		Guard:    principalGuard{principal: Principal{UserID: 1, Username: "admin", IsAdmin: true}},
		Problems: NewProblems(nil),
		// CircuitStates 留空
	}
	router := New(Options{Deps: deps})
	RegisterProviderCircuitLogs(router, deps)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/providers/1/circuit-logs", nil))
	if recorder.Code == http.StatusOK {
		t.Fatal("未接线时不应注册该路由（原样回退 Node），实际返回 200")
	}
}

// failingCircuitStates 是「熔断状态读失败」的替身。
//
// 只实现读到失败那一条；其余方法由内嵌接口兜底——本用例不会调用它们，故传 nil 也安全。
type failingCircuitStates struct {
	CircuitStateStore
	err error
}

func (f failingCircuitStates) ProviderCircuit(context.Context, int64) (providerCircuitSnapshot, error) {
	return providerCircuitSnapshot{}, f.err
}

func (f failingCircuitStates) ProviderCircuits(context.Context, []int64) (map[int64]providerCircuitSnapshot, error) {
	return nil, f.err
}

func (f failingCircuitStates) ResetProviderCircuit(context.Context, int64) error { return f.err }

// 保证替身确实满足两族接口（编译期断言，避免将来接口漂移后静默失效）。
var (
	_ CircuitStateStore    = failingCircuitStates{}
	_ ProviderCircuitStore = failingCircuitStates{}
	_                      = redis.UniversalClient(nil)
)

// circuitLogsContains 是子串断言（包内已有的 containsString 是「切片里是否含某元素」，语义不同）。
func circuitLogsContains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
