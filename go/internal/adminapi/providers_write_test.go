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

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是供应商写路径的集成测试：真 PG + 真 Redis。
//
// 覆盖三条**只有真依赖才能证明**的链路：
//  1. 创建 → 详情 → 更新 → 删除 → 撤销 → 再详情（软删与恢复、端点行的连带软删与恢复）；
//  2. 撤销快照的键形制与 TTL（替换用真客户端读，替身看不出 TTL 被写成永久）；
//  3. 撤销失败分支：token 过期（410）与 operationId 不匹配（409），且**不匹配一次不得烧掉窗口**。
//
// 夹具纪律同 providers_test.go：名称带 `go-pvw-<纳秒>` 前缀，按前缀精确清理。

// providerWriteRouter 建一个注册了读+写全套的 router。
func providerWriteRouter(t *testing.T, pools *store.Pools, deps *Deps) *Router {
	t.Helper()
	if deps.Guard == nil {
		deps.Guard = newTestGuard(t, GuardOptions{})
	}
	if deps.Problems == nil {
		deps.Problems = NewProblems(nil)
	}
	deps.Store = pools
	router := New(Options{Deps: *deps})
	RegisterProviders(router, *deps)
	RegisterProvidersWrite(router, *deps)
	return router
}

// providerWriteRedis 建真 Redis 客户端（未设置 CCH_TEST_REDIS_URL 时跳过）。
func providerWriteRedis(t *testing.T) *redis.Client {
	t.Helper()
	rawURL := os.Getenv(providerUndoRedisGate)
	if rawURL == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过写路径集成测试")
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

func TestProviderWriteCreateUpdateDeleteUndoOnRealDeps(t *testing.T) {
	pools := testPools(t)
	client := providerWriteRedis(t)
	kv := NewRedisProviderUndoKV(client)
	deps := &Deps{ProviderUndoKV: kv}
	router := providerWriteRouter(t, pools, deps)
	prefix := fmt.Sprintf("go-pvw-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		cleanup, err := storeOpenForCleanup(t)
		if err != nil {
			return
		}
		defer func() { _ = cleanup.Close() }()
		writer, err := cleanup.Writer()
		if err != nil {
			return
		}
		_, _ = writer.Exec(context.Background(), `DELETE FROM providers WHERE name LIKE $1`, prefix+"%")
	})

	// ① 创建：201 + Location + 厂商行与端点行都被建出来。
	createBody := fmt.Sprintf(`{
		"name": "%s 主线路",
		"url": "https://%s.example.com/anthropic",
		"key": "sk-created-%s",
		"website_url": "https://%s.example.com",
		"weight": 7,
		"limit_5h_usd": 3.5,
		"group_tag": "  g1 , g2 ，g1 ",
		"allowed_clients": ["cli-a"]
	}`, prefix, prefix, prefix, prefix)
	recorder := providerRequest(t, router, http.MethodPost, "/api/v1/providers", createBody,
		"application/json", false)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("创建应 201，实得 %d：%s", recorder.Code, recorder.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatalf("解析创建响应失败: %v（原文 %s）", err, recorder.Body.String())
	}
	createdID := int64(created["id"].(float64))
	location := recorder.Header().Get("Location")
	if location != fmt.Sprintf("/api/v1/providers/%d", createdID) {
		t.Fatalf("Location = %q，期望 /api/v1/providers/%d", location, createdID)
	}
	if created["maskedKey"] == nil || created["key"] != nil {
		t.Fatalf("创建响应必须给出 maskedKey 且不得回显 key：%v", created)
	}
	// group_tag 归一（去重 + 英文逗号重连）。
	if got := created["groupTag"]; got != "g1,g2" {
		t.Fatalf("groupTag 归一 = %v，期望 g1,g2", got)
	}
	// 厂商行与端点行：Node 的 createProvider 在同一事务里建两者。
	writer, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	var vendorID int64
	if err := writer.QueryRow(context.Background(),
		`SELECT provider_vendor_id FROM providers WHERE id = $1`, createdID).Scan(&vendorID); err != nil {
		t.Fatalf("读 provider_vendor_id 失败: %v", err)
	}
	if vendorID == 0 {
		t.Fatal("创建后 provider_vendor_id 不应为空")
	}
	var endpointCount int
	if err := writer.QueryRow(context.Background(),
		`SELECT count(*) FROM provider_endpoints
		 WHERE vendor_id = $1 AND provider_type = 'claude' AND deleted_at IS NULL`, vendorID,
	).Scan(&endpointCount); err != nil {
		t.Fatalf("统计端点行失败: %v", err)
	}
	if endpointCount != 1 {
		t.Fatalf("创建后应有 1 行端点，实得 %d", endpointCount)
	}

	// ② 更新：200 + 撤销头。
	updateBody := `{"name": "改过的名字", "priority": 9}`
	recorder = providerRequest(t, router, http.MethodPatch,
		fmt.Sprintf("/api/v1/providers/%d", createdID), updateBody, "application/json", false)
	if recorder.Code != http.StatusOK {
		t.Fatalf("更新应 200，实得 %d：%s", recorder.Code, recorder.Body.String())
	}
	undoToken := recorder.Header().Get("X-CCH-Undo-Token")
	operationID := recorder.Header().Get("X-CCH-Operation-Id")
	if undoToken == "" || operationID == "" {
		t.Fatalf("更新必须回撤销头，实得 token=%q op=%q", undoToken, operationID)
	}
	// 快照：键形制 + TTL 用真客户端看。
	patchKey := providerPatchUndoKey(undoToken)
	if ttl := client.TTL(context.Background(), patchKey).Val(); ttl <= 0 || ttl > 10*time.Second {
		t.Fatalf("更新撤销键 TTL = %v，期望 (0,10s]", ttl)
	}
	t.Cleanup(func() { _ = client.Del(context.Background(), patchKey).Err() })
	// 前像只记发生变化的字段（name），priority 由 0 → 9 也变，故两条都在。
	var snapshot map[string]any
	if err := json.Unmarshal([]byte(client.Get(context.Background(), patchKey).Val()), &snapshot); err != nil {
		t.Fatalf("快照不是合法 JSON: %v", err)
	}
	preimage, _ := snapshot["preimage"].(map[string]any)
	entry, _ := preimage[fmt.Sprintf("%d", createdID)].(map[string]any)
	if _, ok := entry["name"]; !ok {
		t.Fatalf("前像应含 name：%v", entry)
	}
	if entry["priority"] != float64(0) {
		t.Fatalf("前像的 priority 应为旧值 0，实得 %v", entry["priority"])
	}

	// ③ 撤销更新：字段倒回旧值。
	undoBody := fmt.Sprintf(`{"undoToken": "%s", "operationId": "%s"}`, undoToken, operationID)
	recorder = providerRequest(t, router, http.MethodPost, "/api/v1/providers:undoPatch",
		undoBody, "application/json", false)
	if recorder.Code != http.StatusOK {
		t.Fatalf("撤销更新应 200，实得 %d：%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"revertedCount":1`) {
		t.Fatalf("撤销更新应回 revertedCount=1：%s", recorder.Body.String())
	}
	var restoredName string
	var restoredPriority int
	if err := writer.QueryRow(context.Background(),
		`SELECT name, priority FROM providers WHERE id = $1`, createdID,
	).Scan(&restoredName, &restoredPriority); err != nil {
		t.Fatalf("读回供应商失败: %v", err)
	}
	if restoredName != prefix+" 主线路" || restoredPriority != 0 {
		t.Fatalf("撤销后应为旧值，实得 name=%q priority=%d", restoredName, restoredPriority)
	}
	// 撤销 token 是「读并删」：再撤一次即过期。
	recorder = providerRequest(t, router, http.MethodPost, "/api/v1/providers:undoPatch",
		undoBody, "application/json", false)
	if recorder.Code != http.StatusGone {
		t.Fatalf("二次撤销应 410，实得 %d：%s", recorder.Code, recorder.Body.String())
	}

	// ④ 删除：204 + 撤销头 + 软删。
	recorder = providerRequest(t, router, http.MethodDelete,
		fmt.Sprintf("/api/v1/providers/%d", createdID), "", "", false)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("删除应 204，实得 %d：%s", recorder.Code, recorder.Body.String())
	}
	deleteToken := recorder.Header().Get("X-CCH-Undo-Token")
	deleteOp := recorder.Header().Get("X-CCH-Operation-Id")
	if deleteToken == "" || deleteOp == "" {
		t.Fatalf("删除必须回撤销头，实得 token=%q op=%q", deleteToken, deleteOp)
	}
	deleteKey := providerDeleteUndoKey(deleteToken)
	t.Cleanup(func() { _ = client.Del(context.Background(), deleteKey).Err() })
	if ttl := client.TTL(context.Background(), deleteKey).Val(); ttl <= 0 || ttl > 60*time.Second {
		t.Fatalf("删除撤销键 TTL = %v，期望 (0,60s]", ttl)
	}
	var deletedAt *time.Time
	if err := writer.QueryRow(context.Background(),
		`SELECT deleted_at FROM providers WHERE id = $1`, createdID).Scan(&deletedAt); err != nil {
		t.Fatalf("读 deleted_at 失败: %v", err)
	}
	if deletedAt == nil {
		t.Fatal("删除后 deleted_at 不应为空")
	}
	// 端点行被连带软删（无启用引用）。
	var endpointDeleted *time.Time
	if err := writer.QueryRow(context.Background(),
		`SELECT deleted_at FROM provider_endpoints WHERE vendor_id = $1 AND provider_type = 'claude'`,
		vendorID).Scan(&endpointDeleted); err != nil {
		t.Fatalf("读端点 deleted_at 失败: %v", err)
	}
	if endpointDeleted == nil {
		t.Fatal("删除供应商后端点行应被连带软删")
	}

	// ⑤ operationId 不匹配：409，且**不得烧掉**撤销窗口。
	recorder = providerRequest(t, router, http.MethodPost, "/api/v1/providers:undoDelete",
		fmt.Sprintf(`{"undoToken": "%s", "operationId": "provider_patch_apply_wrong"}`, deleteToken),
		"application/json", false)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("operationId 不匹配应 409，实得 %d：%s", recorder.Code, recorder.Body.String())
	}
	if exists := client.Exists(context.Background(), deleteKey).Val(); exists != 1 {
		t.Fatal("operationId 不匹配不应消费撤销 token")
	}

	// ⑥ 撤销删除：供应商与端点都被恢复。
	recorder = providerRequest(t, router, http.MethodPost, "/api/v1/providers:undoDelete",
		fmt.Sprintf(`{"undoToken": "%s", "operationId": "%s"}`, deleteToken, deleteOp),
		"application/json", false)
	if recorder.Code != http.StatusOK {
		t.Fatalf("撤销删除应 200，实得 %d：%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"restoredCount":1`) {
		t.Fatalf("撤销删除应回 restoredCount=1：%s", recorder.Body.String())
	}
	if err := writer.QueryRow(context.Background(),
		`SELECT deleted_at FROM providers WHERE id = $1`, createdID).Scan(&deletedAt); err != nil {
		t.Fatalf("读回 deleted_at 失败: %v", err)
	}
	if deletedAt != nil {
		t.Fatal("撤销删除后 deleted_at 应为空")
	}
	if err := writer.QueryRow(context.Background(),
		`SELECT deleted_at, is_enabled FROM provider_endpoints WHERE vendor_id = $1 AND provider_type = 'claude'`,
		vendorID).Scan(&endpointDeleted, new(bool)); err != nil {
		t.Fatalf("读回端点失败: %v", err)
	}
	if endpointDeleted != nil {
		t.Fatal("撤销删除后端点行应被恢复")
	}

	// ⑦ 详情：撤销后能再读到（读路径与写路径在同一份数据上闭环）。
	recorder = providerRequest(t, router, http.MethodGet,
		fmt.Sprintf("/api/v1/providers/%d", createdID), "", "", false)
	if recorder.Code != http.StatusOK {
		t.Fatalf("撤销后详情应 200，实得 %d：%s", recorder.Code, recorder.Body.String())
	}
}

func TestProviderWriteValidationFailures(t *testing.T) {
	pools := testPools(t)
	client := providerWriteRedis(t)
	deps := &Deps{ProviderUndoKV: NewRedisProviderUndoKV(client)}
	router := providerWriteRouter(t, pools, deps)

	cases := []struct {
		name   string
		body   string
		status int
		detail string
	}{
		{
			name:   "缺 name",
			body:   `{"url": "https://a.example.com", "key": "k"}`,
			status: http.StatusBadRequest,
			detail: "name",
		},
		{
			name:   "url 非绝对 URL",
			body:   `{"name": "n", "url": "not-a-url", "key": "k"}`,
			status: http.StatusBadRequest,
			detail: "url",
		},
		{
			name:   "未知字段",
			body:   `{"name": "n", "url": "https://a.example.com", "key": "k", "nope": 1}`,
			status: http.StatusBadRequest,
			detail: "nope",
		},
		{
			name: "脱敏占位符回写",
			body: `{"name": "n", "url": "https://a.example.com", "key": "sk-[REDACTED]"}`,
			// 422 是 Node 的 provider.redacted_placeholder_rejected。
			status: http.StatusUnprocessableEntity,
			detail: "redacted_placeholder",
		},
		{
			name:   "provider_type 取值不在枚举内",
			body:   `{"name": "n", "url": "https://a.example.com", "key": "k", "provider_type": "nope"}`,
			status: http.StatusBadRequest,
			detail: "provider_type",
		},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			recorder := providerRequest(t, router, http.MethodPost, "/api/v1/providers",
				item.body, "application/json", false)
			if recorder.Code != item.status {
				t.Fatalf("状态码 = %d，期望 %d：%s", recorder.Code, item.status, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), item.detail) {
				t.Fatalf("响应应提到 %q：%s", item.detail, recorder.Body.String())
			}
		})
	}

	// Content-Type 不是 JSON：415（Node 的 parseHonoJsonBody 同形）。
	recorder := providerRequest(t, router, http.MethodPost, "/api/v1/providers", `{}`, "text/plain", false)
	if recorder.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("非 JSON Content-Type 应 415，实得 %d：%s", recorder.Code, recorder.Body.String())
	}
}

func TestProviderWriteRoutesNeedUndoKV(t *testing.T) {
	pools := testPools(t)
	// 未装配 KV：整组写路由不注册（原样回退 Node），而不是注册出「能删不能撤销」的半边。
	deps := &Deps{Guard: newTestGuard(t, GuardOptions{}), Problems: NewProblems(nil), Store: pools}
	router := New(Options{Deps: *deps})
	RegisterProviders(router, *deps)
	RegisterProvidersWrite(router, *deps)

	// 读路径仍在（列表 200）。
	if recorder := providerRequest(t, router, http.MethodGet, "/api/v1/providers", "", "", false); recorder.Code != http.StatusOK {
		t.Fatalf("读路径应仍注册（200），实得 %d", recorder.Code)
	}
	// 写路径不在：未注册的路径由 Router 的 NotFound 兜底（此处未设 fallback，故 404）。
	recorder := providerRequest(t, router, http.MethodPost, "/api/v1/providers",
		`{"name": "n", "url": "https://a.example.com", "key": "k"}`, "application/json", false)
	if recorder.Code == http.StatusCreated {
		t.Fatal("未装配撤销 KV 时不得注册创建路由")
	}
}

// TestProviderWriteLadderRoundTripOnRealDeps 走真 HTTP + 真库，钉住等待阶梯两列的**全链**
// （查询串 snake_case → 字段规格 → 存入绑定 → 列 → 响应投影 camelCase）。
//
// 为何非走 HTTP 不可：这三段各有自己的映射表，任一段漏了字段都不会报错，只会**静默丢弃**——
// 界面表单填了、提交 200、库里却是空的，正是「用户看得见的缺陷」那一类。
func TestProviderWriteLadderRoundTripOnRealDeps(t *testing.T) {
	pools := testPools(t)
	client := providerWriteRedis(t)
	deps := &Deps{ProviderUndoKV: NewRedisProviderUndoKV(client)}
	router := providerWriteRouter(t, pools, deps)
	prefix := fmt.Sprintf("go-pvw-ladder-%d", time.Now().UnixNano())
	writer, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	t.Cleanup(func() {
		cleanup, err := storeOpenForCleanup(t)
		if err != nil {
			return
		}
		defer func() { _ = cleanup.Close() }()
		pool, err := cleanup.Writer()
		if err != nil {
			return
		}
		_, _ = pool.Exec(context.Background(), `DELETE FROM providers WHERE name LIKE $1`, prefix+"%")
	})

	// ① 创建时带阶梯：递增 30 分钟、封顶 4 级（单位毫秒，与 circuit_breaker_open_duration 同）。
	createBody := fmt.Sprintf(`{
		"name": "%s 阶梯",
		"url": "https://%s.example.com/anthropic",
		"key": "sk-%s",
		"circuit_breaker_release_increment": 1800000,
		"circuit_breaker_max_open_count": 4
	}`, prefix, prefix, prefix)
	recorder := providerRequest(t, router, http.MethodPost, "/api/v1/providers", createBody,
		"application/json", false)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("创建应 201，实得 %d：%s", recorder.Code, recorder.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatalf("解析创建响应失败: %v", err)
	}
	createdID := int64(created["id"].(float64))
	if got := created["circuitBreakerReleaseIncrement"]; got != float64(1800000) {
		t.Fatalf("创建响应里的递增时长 = %v，期望 1800000", got)
	}
	if got := created["circuitBreakerMaxOpenCount"]; got != float64(4) {
		t.Fatalf("创建响应里的递增最大次数 = %v，期望 4", got)
	}

	var increment, maxOpen *int
	if err := writer.QueryRow(context.Background(),
		`SELECT circuit_breaker_release_increment, circuit_breaker_max_open_count
		   FROM providers WHERE id = $1`, createdID,
	).Scan(&increment, &maxOpen); err != nil {
		t.Fatalf("读回阶梯两列失败: %v", err)
	}
	if increment == nil || *increment != 1800000 || maxOpen == nil || *maxOpen != 4 {
		t.Fatalf("创建应把两列写进库，实得 increment=%v maxOpen=%v", increment, maxOpen)
	}

	// ② 更新为 null（= 不启用阶梯）：两列都清空，而不是写 0。
	patchBody := `{"circuit_breaker_release_increment": null, "circuit_breaker_max_open_count": null}`
	recorder = providerRequest(t, router, http.MethodPatch,
		fmt.Sprintf("/api/v1/providers/%d", createdID), patchBody, "application/json", false)
	if recorder.Code != http.StatusOK {
		t.Fatalf("更新应 200，实得 %d：%s", recorder.Code, recorder.Body.String())
	}
	var patched map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &patched); err != nil {
		t.Fatalf("解析更新响应失败: %v", err)
	}
	if got, ok := patched["circuitBreakerReleaseIncrement"]; !ok || got != nil {
		t.Fatalf("更新响应里的递增时长应为 null，实得 %v（存在=%v）", got, ok)
	}
	if got, ok := patched["circuitBreakerMaxOpenCount"]; !ok || got != nil {
		t.Fatalf("更新响应里的递增最大次数应为 null，实得 %v（存在=%v）", got, ok)
	}
	if err := writer.QueryRow(context.Background(),
		`SELECT circuit_breaker_release_increment, circuit_breaker_max_open_count
		   FROM providers WHERE id = $1`, createdID,
	).Scan(&increment, &maxOpen); err != nil {
		t.Fatalf("清空后读回失败: %v", err)
	}
	if increment != nil || maxOpen != nil {
		t.Fatalf("清空后两列应为 NULL，实得 increment=%v maxOpen=%v", increment, maxOpen)
	}
}

// storeOpenForCleanup 建一个独立于被测池的清理池（被测池的 Cleanup 是 LIFO，会先关）。
func storeOpenForCleanup(t *testing.T) (*store.Pools, error) {
	t.Helper()
	return store.Open(context.Background(), store.Options{
		DSN:                 os.Getenv("CCH_TEST_DSN"),
		Budget:              config.SplitPoolBudget(6),
		Timeouts:            config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
		ApplicationNameBase: "cch-adminapi-pvw",
	})
}

var _ = httptest.NewRequest
