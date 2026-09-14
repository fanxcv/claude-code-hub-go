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

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 provider-endpoints **两族熔断端点**的测试。
//
// 为什么必须打真 Redis：这六条端点的全部意义就是「读到数据面正在用的那份状态、写回它」，
// 而键名、字段名、空值序列化（空串代表 null）、TTL 这四件事都只能在真 Redis 上验。桩只能
// 证明「我调了我以为的方法」，证明不了数据面的 HGetAll 真的能读出我刚写的那三个字段。
//
// 门控：CCH_TEST_DSN（可见性检查要 PG）与 CCH_TEST_REDIS_URL（状态读写）。夹具纪律同其它集成
// 测试：自钉唯一标记、按精确 id 清、Redis 键按名清。

// circuitFixture 是一次夹具的定位信息。
type circuitFixture struct {
	prefix     string
	vendorID   int64
	visibleID  int64
	hiddenID   int64
	client     *redis.Client
	storeRedis CircuitStateStore
}

// circuitTestRedis 建真 Redis 连接；门控未设置时跳过。
func circuitTestRedis(t *testing.T) *redis.Client {
	t.Helper()
	rawURL := os.Getenv(dashboardRedisGate)
	if rawURL == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过 Redis 集成测试")
	}
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		t.Fatalf("解析 Redis URL 失败: %v", err)
	}
	if options.DB == 0 {
		options.DB = 13
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("Redis 不可达: %v", err)
	}
	return client
}

// seedCircuitFixture 建一个厂 + 两个端点（一个可见类型、一个隐藏类型），并清掉它们的熔断键。
func seedCircuitFixture(t *testing.T, pools *store.Pools) *circuitFixture {
	t.Helper()
	client := circuitTestRedis(t)
	ctx := context.Background()
	prefix := fmt.Sprintf("go-circuit-it-%d", time.Now().UnixNano())
	domain := prefix + ".example.com"

	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	var vendorID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO provider_vendors (website_domain, display_name) VALUES ($1, $2) RETURNING id`,
		domain, prefix,
	).Scan(&vendorID); err != nil {
		t.Fatalf("建厂夹具失败: %v", err)
	}
	insertEndpoint := func(providerType string) int64 {
		var id int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO provider_endpoints (vendor_id, provider_type, url, is_enabled, sort_order)
			 VALUES ($1, $2, $3, true, 0) RETURNING id`,
			vendorID, providerType, "https://"+domain+"/"+providerType,
		).Scan(&id); err != nil {
			t.Fatalf("建端点夹具失败: %v", err)
		}
		return id
	}
	fixture := &circuitFixture{
		prefix:     prefix,
		vendorID:   vendorID,
		visibleID:  insertEndpoint("claude"),
		hiddenID:   insertEndpoint(providerTypeClaudeAuth),
		client:     client,
		storeRedis: NewRedisCircuitStates(client, nil, nil),
	}

	// Redis 夹具键：只用 endpoint_circuit_breaker:state:{id}，按名清（不 SCAN 全库）。
	cleanupRedis := func() {
		for _, endpointID := range []int64{fixture.visibleID, fixture.hiddenID} {
			_ = client.Del(ctx, route.EndpointStateKeyPrefix+fmt.Sprint(endpointID)).Err()
		}
		for _, providerType := range []string{"claude", providerTypeClaudeAuth, "codex"} {
			_ = client.Del(ctx,
				route.VendorTypeStateKeyPrefix+fmt.Sprint(vendorID)+":"+providerType).Err()
		}
	}
	cleanupRedis()

	t.Cleanup(func() {
		cleanupRedis()
		if _, execErr := pool.Exec(ctx, `DELETE FROM provider_endpoints WHERE vendor_id = $1`,
			vendorID); execErr != nil {
			t.Errorf("清端点夹具失败: %v", execErr)
		}
		if _, execErr := pool.Exec(ctx, `DELETE FROM provider_vendors WHERE id = $1`,
			vendorID); execErr != nil {
			t.Errorf("清厂夹具失败: %v", execErr)
		}
	})
	return fixture
}

// circuitRouter 造真路由表（真 Store + 真 Redis 面 + 固定管理员身份）。
func circuitRouter(t *testing.T, pools *store.Pools, states CircuitStateStore) *Router {
	t.Helper()
	deps := Deps{
		Logger:        logx.New(nil),
		Guard:         principalGuard{principal: Principal{UserID: 1, Username: "admin", IsAdmin: true}},
		Store:         pools,
		CircuitStates: states,
	}
	router := New(Options{Deps: deps})
	RegisterProviderCircuitRoutes(router, deps)
	return router
}

// circuitCall 发一次请求；compat 为真时带 dashboard 兼容头。
func circuitCall(
	t *testing.T,
	router *Router,
	method string,
	path string,
	body string,
	compat bool,
) (int, string) {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, path, reader)
	request.Header.Set("Content-Type", "application/json")
	if compat {
		request.Header.Set(dashboardCompatHeader, "1")
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.String()
}

// TestCircuitRoutesNotRegisteredWithoutRedis 钉住「缺 Redis 整组不注册」：
// 没装配时这六条必须落回 Node，而不是以恒 closed 作答。
func TestCircuitRoutesNotRegisteredWithoutRedis(t *testing.T) {
	pools := testPools(t)
	router := circuitRouter(t, pools, nil)
	if count := router.RouteCount(); count != 0 {
		t.Fatalf("未装配熔断状态面时应零注册，实际 %d 条", count)
	}
}

// TestIntegrationEndpointCircuitReadDefaultsAndStoredState 钉住 GET 端点熔断的形状与取值。
func TestIntegrationEndpointCircuitReadDefaultsAndStoredState(t *testing.T) {
	pools := testPools(t)
	fixture := seedCircuitFixture(t, pools)
	router := circuitRouter(t, pools, fixture.storeRedis)
	ctx := context.Background()

	status, body := circuitCall(t, router, http.MethodGet,
		fmt.Sprintf("/api/v1/provider-endpoints/%d/circuit", fixture.visibleID), "", false)
	if status != http.StatusOK {
		t.Fatalf("GET 端点熔断应 200，实际 %d：%s", status, body)
	}
	var payload struct {
		EndpointID int64 `json:"endpointId"`
		Health     struct {
			FailureCount         int64  `json:"failureCount"`
			LastFailureTime      *int64 `json:"lastFailureTime"`
			CircuitState         string `json:"circuitState"`
			CircuitOpenUntil     *int64 `json:"circuitOpenUntil"`
			HalfOpenSuccessCount int64  `json:"halfOpenSuccessCount"`
		} `json:"health"`
		Config struct {
			FailureThreshold         int64 `json:"failureThreshold"`
			OpenDuration             int64 `json:"openDuration"`
			HalfOpenSuccessThreshold int64 `json:"halfOpenSuccessThreshold"`
		} `json:"config"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("解析响应失败: %v（原文 %.200s）", err, body)
	}
	if payload.EndpointID != fixture.visibleID {
		t.Fatalf("endpointId 应为 %d，实际 %d", fixture.visibleID, payload.EndpointID)
	}
	if payload.Health.CircuitState != "closed" || payload.Health.FailureCount != 0 ||
		payload.Health.HalfOpenSuccessCount != 0 || payload.Health.LastFailureTime != nil ||
		payload.Health.CircuitOpenUntil != nil {
		t.Fatalf("键缺失时应答出厂闭态，实际 %+v", payload.Health)
	}
	if payload.Config.FailureThreshold != 3 || payload.Config.OpenDuration != 300000 ||
		payload.Config.HalfOpenSuccessThreshold != 1 {
		t.Fatalf("config 三个数应照 DEFAULT_ENDPOINT_CIRCUIT_BREAKER_CONFIG，实际 %+v", payload.Config)
	}

	// 写入与数据面同一键名的状态（数据面 HealthReader 就是这么读的），再读回。
	key := route.EndpointStateKeyPrefix + fmt.Sprint(fixture.visibleID)
	openUntil := time.Now().Add(time.Minute).UnixMilli()
	lastFailure := time.Now().UnixMilli()
	if err := fixture.client.HSet(ctx, key, map[string]any{
		"failureCount":         "3",
		"lastFailureTime":      fmt.Sprint(lastFailure),
		"circuitState":         "open",
		"circuitOpenUntil":     fmt.Sprint(openUntil),
		"halfOpenSuccessCount": "0",
	}).Err(); err != nil {
		t.Fatalf("写 Redis 夹具状态失败: %v", err)
	}
	status, body = circuitCall(t, router, http.MethodGet,
		fmt.Sprintf("/api/v1/provider-endpoints/%d/circuit", fixture.visibleID), "", false)
	if status != http.StatusOK {
		t.Fatalf("GET 端点熔断应 200，实际 %d：%s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	if payload.Health.CircuitState != "open" || payload.Health.FailureCount != 3 {
		t.Fatalf("应读回 open/3，实际 %+v", payload.Health)
	}
	if payload.Health.CircuitOpenUntil == nil || *payload.Health.CircuitOpenUntil != openUntil {
		t.Fatalf("circuitOpenUntil 应为 %d，实际 %v", openUntil, payload.Health.CircuitOpenUntil)
	}
	if payload.Health.LastFailureTime == nil || *payload.Health.LastFailureTime != lastFailure {
		t.Fatalf("lastFailureTime 应为 %d，实际 %v", lastFailure, payload.Health.LastFailureTime)
	}
}

// TestIntegrationEndpointCircuitVisibilityAndBatch 钉住批量读的三条纪律：
// 隐藏类型 404、原始列表的顺序与重复项保留、缺状态补出厂闭态。
func TestIntegrationEndpointCircuitVisibilityAndBatch(t *testing.T) {
	pools := testPools(t)
	fixture := seedCircuitFixture(t, pools)
	router := circuitRouter(t, pools, fixture.storeRedis)
	ctx := context.Background()

	// 隐藏类型（claude-auth）在非 dashboard 口径下不可见。
	status, body := circuitCall(t, router, http.MethodGet,
		fmt.Sprintf("/api/v1/provider-endpoints/%d/circuit", fixture.hiddenID), "", false)
	if status != http.StatusNotFound {
		t.Fatalf("隐藏类型端点应 404，实际 %d：%s", status, body)
	}
	if !strings.Contains(body, "provider_endpoint.not_found") {
		t.Fatalf("404 应带 errorCode provider_endpoint.not_found，实际 %s", body)
	}
	// dashboard 口径下可见。
	status, _ = circuitCall(t, router, http.MethodGet,
		fmt.Sprintf("/api/v1/provider-endpoints/%d/circuit", fixture.hiddenID), "", true)
	if status != http.StatusOK {
		t.Fatalf("兼容头下隐藏类型端点应 200，实际 %d", status)
	}

	// 不存在的端点也是 404（而不是「缺状态」）。
	status, _ = circuitCall(t, router, http.MethodGet,
		"/api/v1/provider-endpoints/2147483000/circuit", "", false)
	if status != http.StatusNotFound {
		t.Fatalf("不存在的端点应 404，实际 %d", status)
	}

	key := route.EndpointStateKeyPrefix + fmt.Sprint(fixture.visibleID)
	if err := fixture.client.HSet(ctx, key, map[string]any{
		"failureCount": "2", "circuitState": "half-open", "halfOpenSuccessCount": "1",
	}).Err(); err != nil {
		t.Fatalf("写 Redis 夹具状态失败: %v", err)
	}

	body = fmt.Sprintf(`{"endpointIds":[%d,%d,%d]}`, fixture.visibleID, fixture.hiddenID, fixture.visibleID)
	status, response := circuitCall(t, router, http.MethodPost,
		"/api/v1/provider-endpoints/circuits:batch", body, false)
	if status != http.StatusNotFound {
		t.Fatalf("含隐藏类型的批量应 404，实际 %d：%s", status, response)
	}
	body = fmt.Sprintf(`{"endpointIds":[%d,%d]}`, fixture.visibleID, fixture.visibleID)
	status, response = circuitCall(t, router, http.MethodPost,
		"/api/v1/provider-endpoints/circuits:batch", body, false)
	if status != http.StatusOK {
		t.Fatalf("批量应 200，实际 %d：%s", status, response)
	}
	var items []struct {
		EndpointID       int64  `json:"endpointId"`
		CircuitState     string `json:"circuitState"`
		FailureCount     int64  `json:"failureCount"`
		CircuitOpenUntil *int64 `json:"circuitOpenUntil"`
	}
	if err := json.Unmarshal([]byte(response), &items); err != nil {
		t.Fatalf("解析批量响应失败: %v（原文 %.200s）", err, response)
	}
	if len(items) != 2 {
		t.Fatalf("重复的 id 应按原列表出现两次，实际 %d 项", len(items))
	}
	if items[0].CircuitState != "half-open" || items[0].FailureCount != 2 {
		t.Fatalf("批量项应读回 half-open/2，实际 %+v", items[0])
	}
	if items[0].EndpointID != items[1].EndpointID {
		t.Fatalf("第二项应是同一 id 的重复，实际 %+v", items[1])
	}

	// 空数组：照 Node 直接回 []（不查库、不查可见性）。
	status, response = circuitCall(t, router, http.MethodPost,
		"/api/v1/provider-endpoints/circuits:batch", `{"endpointIds":[]}`, false)
	if status != http.StatusOK || strings.TrimSpace(response) != "[]" {
		t.Fatalf("空数组应 200 且正文为 []，实际 %d：%s", status, response)
	}

	// 缺字段 / 超上限 / 未知字段：三条校验各一。
	status, response = circuitCall(t, router, http.MethodPost,
		"/api/v1/provider-endpoints/circuits:batch", `{}`, false)
	if status != http.StatusBadRequest || !strings.Contains(response, "invalid_type") {
		t.Fatalf("缺 endpointIds 应 400 invalid_type，实际 %d：%s", status, response)
	}
	status, response = circuitCall(t, router, http.MethodPost,
		"/api/v1/provider-endpoints/circuits:batch", `{"endpointIds":[],"extra":1}`, false)
	if status != http.StatusBadRequest || !strings.Contains(response, "unrecognized_keys") {
		t.Fatalf("多传字段应 400 unrecognized_keys，实际 %d：%s", status, response)
	}
	ids := make([]string, circuitBatchMaxEndpointIDs+1)
	for index := range ids {
		ids[index] = "1"
	}
	status, response = circuitCall(t, router, http.MethodPost,
		"/api/v1/provider-endpoints/circuits:batch",
		`{"endpointIds":[`+strings.Join(ids, ",")+`]}`, false)
	if status != http.StatusBadRequest || !strings.Contains(response, "too_big") {
		t.Fatalf("501 个 id 应 400 too_big，实际 %d：%.200s", status, response)
	}
	status, response = circuitCall(t, router, http.MethodPost,
		"/api/v1/provider-endpoints/circuits:batch", `{"endpointIds":[0]}`, false)
	if status != http.StatusBadRequest {
		t.Fatalf("非正整数应 400，实际 %d：%s", status, response)
	}
}

// TestIntegrationEndpointCircuitResetDeletesKey 钉住 reset 的语义：DEL 键 + 204。
func TestIntegrationEndpointCircuitResetDeletesKey(t *testing.T) {
	pools := testPools(t)
	fixture := seedCircuitFixture(t, pools)
	router := circuitRouter(t, pools, fixture.storeRedis)
	ctx := context.Background()

	key := route.EndpointStateKeyPrefix + fmt.Sprint(fixture.visibleID)
	if err := fixture.client.HSet(ctx, key, map[string]any{"circuitState": "open"}).Err(); err != nil {
		t.Fatalf("写 Redis 夹具状态失败: %v", err)
	}
	status, body := circuitCall(t, router, http.MethodPost,
		fmt.Sprintf("/api/v1/provider-endpoints/%d/circuit:reset", fixture.visibleID), "", false)
	if status != http.StatusNoContent {
		t.Fatalf("reset 应 204，实际 %d：%s", status, body)
	}
	if body != "" {
		t.Fatalf("204 不应有正文，实际 %q", body)
	}
	exists, err := fixture.client.Exists(ctx, key).Result()
	if err != nil {
		t.Fatalf("查键失败: %v", err)
	}
	if exists != 0 {
		t.Fatal("reset 后键应已删除")
	}
	// 重置后读回应是出厂闭态。
	status, body = circuitCall(t, router, http.MethodGet,
		fmt.Sprintf("/api/v1/provider-endpoints/%d/circuit", fixture.visibleID), "", false)
	if status != http.StatusOK || !strings.Contains(body, `"circuitState":"closed"`) {
		t.Fatalf("重置后应读回 closed，实际 %d：%s", status, body)
	}
}

// TestIntegrationVendorCircuitManualOpenRoundTrip 钉住厂级熔断的读/写/清全程：
// 手工开闸写入的四个字段、TTL、读回形状，以及手工闭合与重置。
func TestIntegrationVendorCircuitManualOpenRoundTrip(t *testing.T) {
	pools := testPools(t)
	fixture := seedCircuitFixture(t, pools)
	router := circuitRouter(t, pools, fixture.storeRedis)
	ctx := context.Background()
	base := fmt.Sprintf("/api/v1/provider-vendors/%d/circuit", fixture.vendorID)

	// 默认：键缺失即出厂闭态（Node 也不查厂是否存在）。
	status, body := circuitCall(t, router, http.MethodGet, base+"?providerType=claude", "", false)
	if status != http.StatusOK {
		t.Fatalf("厂级读应 200，实际 %d：%s", status, body)
	}
	var payload struct {
		VendorID         int64  `json:"vendorId"`
		ProviderType     string `json:"providerType"`
		CircuitState     string `json:"circuitState"`
		CircuitOpenUntil *int64 `json:"circuitOpenUntil"`
		LastFailureTime  *int64 `json:"lastFailureTime"`
		ManualOpen       bool   `json:"manualOpen"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("解析厂级响应失败: %v（原文 %.200s）", err, body)
	}
	if payload.CircuitState != "closed" || payload.ManualOpen || payload.CircuitOpenUntil != nil ||
		payload.LastFailureTime != nil || payload.ProviderType != "claude" ||
		payload.VendorID != fixture.vendorID {
		t.Fatalf("键缺失时应答出厂闭态，实际 %+v", payload)
	}

	// 手工开闸：circuitState=open、manualOpen=1、开闸窗口空、lastFailureTime 有值。
	before := time.Now().UnixMilli()
	status, body = circuitCall(t, router, http.MethodPost, base+":setManualOpen",
		`{"providerType":"claude","manualOpen":true}`, false)
	if status != http.StatusNoContent {
		t.Fatalf("setManualOpen 应 204，实际 %d：%s", status, body)
	}
	key := route.VendorTypeStateKeyPrefix + fmt.Sprint(fixture.vendorID) + ":claude"
	raw, err := fixture.client.HGetAll(ctx, key).Result()
	if err != nil {
		t.Fatalf("读厂级状态键失败: %v", err)
	}
	if raw["circuitState"] != "open" || raw["manualOpen"] != "1" {
		t.Fatalf("手工开闸应写 open/manualOpen=1，实际 %v", raw)
	}
	if raw["circuitOpenUntil"] != "" {
		t.Fatalf("手工开闸时开闸窗口应为空串，实际 %q", raw["circuitOpenUntil"])
	}
	failureMS := circuitIntOrZero(raw["lastFailureTime"])
	if failureMS < before {
		t.Fatalf("lastFailureTime 应记当前时刻（>= %d），实际 %d", before, failureMS)
	}
	// TTL：默认 30 天（未装配设置源即非高并发）。
	ttl, err := fixture.client.TTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("读 TTL 失败: %v", err)
	}
	if ttl <= 0 || ttl > time.Duration(vendorTypeCircuitStateTTLSeconds)*time.Second {
		t.Fatalf("TTL 应在 (0, 30d]，实际 %s", ttl)
	}

	status, body = circuitCall(t, router, http.MethodGet, base+"?providerType=claude", "", false)
	if status != http.StatusOK {
		t.Fatalf("厂级读应 200，实际 %d：%s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("解析厂级响应失败: %v", err)
	}
	if payload.CircuitState != "open" || !payload.ManualOpen || payload.LastFailureTime == nil {
		t.Fatalf("读回手工开闸态失败，实际 %+v", payload)
	}

	// 手工闭合：四字段全清。
	status, body = circuitCall(t, router, http.MethodPost, base+":setManualOpen",
		`{"providerType":"claude","manualOpen":false}`, false)
	if status != http.StatusNoContent {
		t.Fatalf("setManualOpen(false) 应 204，实际 %d：%s", status, body)
	}
	raw, err = fixture.client.HGetAll(ctx, key).Result()
	if err != nil {
		t.Fatalf("读厂级状态键失败: %v", err)
	}
	if raw["circuitState"] != "closed" || raw["manualOpen"] != "0" ||
		raw["circuitOpenUntil"] != "" || raw["lastFailureTime"] != "" {
		t.Fatalf("手工闭合应四字段全清，实际 %v", raw)
	}

	// 重置：删键 + 204。
	status, _ = circuitCall(t, router, http.MethodPost, base+":setManualOpen",
		`{"providerType":"claude","manualOpen":true}`, false)
	if status != http.StatusNoContent {
		t.Fatalf("重设手工开闸失败，实际 %d", status)
	}
	status, body = circuitCall(t, router, http.MethodPost, base+":reset",
		`{"providerType":"claude"}`, false)
	if status != http.StatusNoContent {
		t.Fatalf("reset 应 204，实际 %d：%s", status, body)
	}
	exists, err := fixture.client.Exists(ctx, key).Result()
	if err != nil {
		t.Fatalf("查键失败: %v", err)
	}
	if exists != 0 {
		t.Fatal("reset 后厂级状态键应已删除")
	}
}

// TestVendorCircuitValidation 钉住三条校验与 dashboard 口径的枚举差异。
//
// 这条不需要真库（走到校验就作答了），但仍要真 Redis 面才能过注册条件……故仍打真库 + 真 Redis。
func TestVendorCircuitValidation(t *testing.T) {
	pools := testPools(t)
	fixture := seedCircuitFixture(t, pools)
	router := circuitRouter(t, pools, fixture.storeRedis)
	base := fmt.Sprintf("/api/v1/provider-vendors/%d/circuit", fixture.vendorID)

	// 公开口径不认隐藏类型。
	status, body := circuitCall(t, router, http.MethodGet, base+"?providerType=claude-auth", "", false)
	if status != http.StatusBadRequest || !strings.Contains(body, "invalid_enum_value") {
		t.Fatalf("公开口径的隐藏类型应 400 invalid_enum_value，实际 %d：%s", status, body)
	}
	status, _ = circuitCall(t, router, http.MethodGet, base+"?providerType=claude-auth", "", true)
	if status != http.StatusOK {
		t.Fatalf("兼容头下隐藏类型应 200，实际 %d", status)
	}
	// 缺 providerType。
	status, body = circuitCall(t, router, http.MethodGet, base, "", false)
	if status != http.StatusBadRequest || !strings.Contains(body, "invalid_type") {
		t.Fatalf("缺 providerType 应 400 invalid_type，实际 %d：%s", status, body)
	}
	// 手工开闸缺 manualOpen。
	status, body = circuitCall(t, router, http.MethodPost, base+":setManualOpen",
		`{"providerType":"claude"}`, false)
	if status != http.StatusBadRequest || !strings.Contains(body, "manualOpen") {
		t.Fatalf("缺 manualOpen 应 400，实际 %d：%s", status, body)
	}
	// 非正数路径参数。
	status, _ = circuitCall(t, router, http.MethodGet, "/api/v1/provider-vendors/0/circuit?providerType=claude", "", false)
	if status != http.StatusBadRequest {
		t.Fatalf("vendorId=0 应 400，实际 %d", status)
	}
}

// TestCircuitStoreRealRedisRoundTrip 直接钉 seam 的四个读写行为（不经 HTTP），
// 覆盖端点在 HTTP 层不便构造的两处：键缺失的默认值与空值字段的空串序列化。
func TestCircuitStoreRealRedisRoundTrip(t *testing.T) {
	client := circuitTestRedis(t)
	storeRedis := NewRedisCircuitStates(client, nil, nil)
	ctx := context.Background()
	key := route.EndpointStateKeyPrefix + "2147483001"
	t.Cleanup(func() { _ = client.Del(ctx, key).Err() })

	_ = client.Del(ctx, key)
	snapshot, err := storeRedis.EndpointCircuit(ctx, 2147483001)
	if err != nil {
		t.Fatalf("读缺键状态失败: %v", err)
	}
	if snapshot.CircuitState != "closed" || snapshot.FailureCount != 0 {
		t.Fatalf("缺键应给出厂闭态，实际 %+v", snapshot)
	}

	if err := client.HSet(ctx, key, map[string]any{
		"failureCount": "7", "lastFailureTime": "", "circuitState": "open",
		"circuitOpenUntil": "1700000000000", "halfOpenSuccessCount": "2",
	}).Err(); err != nil {
		t.Fatalf("写夹具状态失败: %v", err)
	}
	snapshot, err = storeRedis.EndpointCircuit(ctx, 2147483001)
	if err != nil {
		t.Fatalf("读状态失败: %v", err)
	}
	if snapshot.FailureCount != 7 || snapshot.HalfOpenSuccessCount != 2 ||
		snapshot.CircuitState != "open" || snapshot.LastFailureTimeMS != nil {
		t.Fatalf("空串应解析为 nil、数值应解析为原值，实际 %+v", snapshot)
	}
	if snapshot.CircuitOpenUntilMS == nil || *snapshot.CircuitOpenUntilMS != 1700000000000 {
		t.Fatalf("circuitOpenUntil 解析失败: %v", snapshot.CircuitOpenUntilMS)
	}

	batch, err := storeRedis.EndpointCircuits(ctx, []int64{2147483001, 2147483001, 2147483002})
	if err != nil {
		t.Fatalf("批量读失败: %v", err)
	}
	if len(batch) != 2 {
		t.Fatalf("批量读应按去重后返回 2 项，实际 %d", len(batch))
	}
	if batch[2147483001].CircuitState != "open" || batch[2147483002].CircuitState != "closed" {
		t.Fatalf("批量读结果不对: %+v", batch)
	}

	if err := storeRedis.ResetEndpointCircuit(ctx, 2147483001); err != nil {
		t.Fatalf("重置失败: %v", err)
	}
	exists, err := client.Exists(ctx, key).Result()
	if err != nil || exists != 0 {
		t.Fatalf("重置后键应已删除（exists=%d, err=%v）", exists, err)
	}
}

// TestCircuitVendorTTLShrinksUnderHighConcurrency 钉住 TTL 的高并发收缩规则。
//
// 这里用**假设置源**而不改共享的 system_settings 行：改那一行会让并发跑的其它用例看到
// 高并发模式，那是跨用例污染。规则本身（min(默认, 24h)）在假源上验得同样彻底。
func TestCircuitVendorTTLShrinksUnderHighConcurrency(t *testing.T) {
	client := circuitTestRedis(t)
	ctx := context.Background()
	states := NewRedisCircuitStates(client, fakeCircuitSettings{highConcurrency: true}, nil)
	storeRedis, ok := states.(*redisCircuitStateStore)
	if !ok {
		t.Fatal("构造出的实现不是 redisCircuitStateStore")
	}
	ttl, err := storeRedis.vendorTypeTTLSeconds(ctx)
	if err != nil {
		t.Fatalf("解析 TTL 失败: %v", err)
	}
	if ttl != vendorTypeCircuitRetentionShrinkSeconds {
		t.Fatalf("高并发模式下应收缩到 24h，实际 %d", ttl)
	}

	key := route.VendorTypeStateKeyPrefix + "2147483002:claude"
	t.Cleanup(func() { _ = client.Del(ctx, key).Err() })
	if err := states.SaveVendorTypeCircuit(ctx, 2147483002, "claude", VendorTypeCircuitSnapshot{
		CircuitState: "open", ManualOpen: true,
	}); err != nil {
		t.Fatalf("写厂级状态失败: %v", err)
	}
	actualTTL, err := client.TTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("读 TTL 失败: %v", err)
	}
	if actualTTL > time.Duration(vendorTypeCircuitRetentionShrinkSeconds)*time.Second {
		t.Fatalf("键 TTL 应 <= 24h，实际 %s", actualTTL)
	}
}

// fakeCircuitSettings 是只用于 TTL 规则的设置源替身。
type fakeCircuitSettings struct{ highConcurrency bool }

func (f fakeCircuitSettings) FindSystemSettings(context.Context) (*store.SystemSettings, error) {
	return &store.SystemSettings{EnableHighConcurrencyMode: f.highConcurrency}, nil
}

// 编译期钉住：真设置源（连接池）满足接口——装配处传的就是它。
var _ CircuitSettingsSource = (*store.Pools)(nil)
