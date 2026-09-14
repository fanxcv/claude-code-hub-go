package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是三条根级可用性读端点的端到端用例：真路由表 + 真库（`CCH_TEST_DSN` 未设置时跳过）。
//
// 要点在「作答形状」与「两级数据源的新鲜度门槛」：这两件事只有真库真路由才验得出来。
// 共享库纪律同其它集成用例：自钉唯一前缀、按精确 id 清、只动自己建的行。

// availabilityRouter 造真路由表（真 Store + 固定管理员身份）。
func availabilityRouter(t *testing.T, pools *store.Pools) *Router {
	t.Helper()
	deps := Deps{
		Logger: logx.New(nil),
		Guard:  principalGuard{principal: Principal{UserID: 1, Username: "admin", IsAdmin: true}},
		Store:  pools,
	}
	router := New(Options{Deps: deps})
	RegisterAvailabilityRoutes(router, deps)
	return router
}

// availabilitySeed 建一个厂、两个端点与一个启用供应商，并登记清理。
type availabilitySeed struct {
	prefix     string
	domain     string
	vendorID   int64
	providerID int64
	endpointID int64
	pool       *store.Pool
}

func seedAvailability(t *testing.T, pools *store.Pools) *availabilitySeed {
	t.Helper()
	ctx := context.Background()
	prefix := fmt.Sprintf("go-avail-it-%d", time.Now().UnixNano())
	domain := prefix + ".example.com"

	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	var vendorID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO provider_vendors (website_domain, display_name) VALUES ($1, $2) RETURNING id`,
		domain, prefix).Scan(&vendorID); err != nil {
		t.Fatalf("建厂夹具失败: %v", err)
	}
	var endpointID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO provider_endpoints (vendor_id, provider_type, url, is_enabled, sort_order, label)
		 VALUES ($1, 'claude', $2, true, 0, $3) RETURNING id`,
		vendorID, "https://"+domain+"/claude", prefix+"-endpoint").Scan(&endpointID); err != nil {
		t.Fatalf("建端点夹具失败: %v", err)
	}
	var providerID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO providers (name, url, key, provider_vendor_id, provider_type, is_enabled)
		 VALUES ($1, $2, $3, $4, 'claude', true) RETURNING id`,
		prefix+"-provider", "https://"+domain+"/claude", "sk-"+prefix, vendorID).Scan(&providerID); err != nil {
		t.Fatalf("建供应商夹具失败: %v", err)
	}

	seed := &availabilitySeed{
		prefix: prefix, domain: domain, vendorID: vendorID,
		providerID: providerID, endpointID: endpointID, pool: pool,
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM avail_current WHERE provider_id = $1`, providerID)
		_, _ = pool.Exec(ctx, `DELETE FROM avail_bucket_1m WHERE provider_id = $1`, providerID)
		_, _ = pool.Exec(ctx, `DELETE FROM provider_endpoints WHERE vendor_id = $1`, vendorID)
		_, _ = pool.Exec(ctx, `DELETE FROM providers WHERE id = $1`, providerID)
		_, _ = pool.Exec(ctx, `DELETE FROM provider_vendors WHERE id = $1`, vendorID)
	})
	return seed
}

func TestAvailabilityEndpointsIntegration(t *testing.T) {
	pools := endpointWritePools(t)
	seed := seedAvailability(t, pools)
	router := availabilityRouter(t, pools)

	status, body := call(router, http.MethodGet,
		fmt.Sprintf("/api/availability/endpoints?vendorId=%d&providerType=claude", seed.vendorID), "")
	if status != http.StatusOK {
		t.Fatalf("端点池查询应 200，实得 %d：%s", status, body)
	}
	payload := decodeObject(t, body)
	if payload["vendorId"].(float64) != float64(seed.vendorID) || payload["providerType"] != "claude" {
		t.Fatalf("回显的厂/类型不符：%s", body)
	}
	endpoints, _ := payload["endpoints"].([]any)
	if len(endpoints) != 1 {
		t.Fatalf("应只有夹具那一个端点：%s", body)
	}
	first, _ := endpoints[0].(map[string]any)
	// 这一条**不脱敏**：Node 没走 sanitizeProviderEndpointData，URL 原样出。
	if first["url"] != "https://"+seed.domain+"/claude" {
		t.Fatalf("URL 应原样返回（不脱敏）：%v", first["url"])
	}

	// 隐藏类型在这里是**合法**的（Node 的白名单含两个隐藏类型）。
	status, body = call(router, http.MethodGet,
		fmt.Sprintf("/api/availability/endpoints?vendorId=%d&providerType=claude-auth", seed.vendorID), "")
	if status != http.StatusOK {
		t.Fatalf("隐藏类型应被接受（200），实得 %d：%s", status, body)
	}

	// 非法参数：厂 id 非数字或类型不在白名单，一律 400 + {"error":"Invalid query"}。
	for _, query := range []string{
		"vendorId=abc&providerType=claude",
		"vendorId=0&providerType=claude",
		fmt.Sprintf("vendorId=%d&providerType=unknown-type", seed.vendorID),
	} {
		status, body = call(router, http.MethodGet, "/api/availability/endpoints?"+query, "")
		if status != http.StatusBadRequest || !strings.Contains(body, `"error":"Invalid query"`) {
			t.Fatalf("非法参数应 400 Invalid query，实得 %d：%s", status, body)
		}
	}
}

func TestAvailabilityProbeLogsIntegration(t *testing.T) {
	pools := endpointWritePools(t)
	seed := seedAvailability(t, pools)
	router := availabilityRouter(t, pools)

	status, body := call(router, http.MethodGet,
		fmt.Sprintf("/api/availability/endpoints/probe-logs?endpointId=%d", seed.endpointID), "")
	if status != http.StatusOK {
		t.Fatalf("探活历史应 200，实得 %d：%s", status, body)
	}
	payload := decodeObject(t, body)
	endpoint, _ := payload["endpoint"].(map[string]any)
	if endpoint == nil || endpoint["id"].(float64) != float64(seed.endpointID) {
		t.Fatalf("应带端点对象：%s", body)
	}
	if _, ok := payload["logs"].([]any); !ok {
		t.Fatalf("logs 应为数组：%s", body)
	}

	// 不存在的端点：404 + {"error":"Not found"}。
	status, body = call(router, http.MethodGet,
		"/api/availability/endpoints/probe-logs?endpointId=999999999", "")
	if status != http.StatusNotFound || !strings.Contains(body, `"error":"Not found"`) {
		t.Fatalf("不存在的端点应 404 Not found，实得 %d：%s", status, body)
	}

	// limit / offset 越界：400。
	for _, query := range []string{
		fmt.Sprintf("endpointId=%d&limit=0", seed.endpointID),
		fmt.Sprintf("endpointId=%d&limit=1001", seed.endpointID),
		fmt.Sprintf("endpointId=%d&offset=-1", seed.endpointID),
		"endpointId=0",
	} {
		status, _ = call(router, http.MethodGet, "/api/availability/endpoints/probe-logs?"+query, "")
		if status != http.StatusBadRequest {
			t.Fatalf("非法分页应 400（%s），实得 %d", query, status)
		}
	}
}

func TestAvailabilityCurrentIntegration(t *testing.T) {
	pools := endpointWritePools(t)
	seed := seedAvailability(t, pools)
	router := availabilityRouter(t, pools)
	ctx := context.Background()

	// 无状态行时：该供应商必须是 unknown 全零（Node 的 `!stats || requestCount<=0` 分支）。
	status, body := call(router, http.MethodGet, "/api/availability/current", "")
	if status != http.StatusOK {
		t.Fatalf("当前状态应 200，实得 %d：%s", status, body)
	}
	entry := availabilityEntryOf(t, body, seed.providerID)
	if entry == nil {
		t.Fatalf("响应应含夹具供应商：%s", body)
	}
	if entry["status"] != "unknown" || entry["availability"].(float64) != 0 {
		t.Fatalf("无状态行时应为 unknown/0：%v", entry)
	}
	if entry["lastRequestAt"] != nil {
		t.Fatalf("unknown 的 lastRequestAt 应为 null：%v", entry["lastRequestAt"])
	}

	// 写一条**新鲜**的 avail_current：green/0.25 应原样透出。
	if _, err := seed.pool.Exec(ctx,
		`INSERT INTO avail_current (provider_id, state, availability, request_count, last_request_at, updated_at)
		 VALUES ($1, 'green', 0.25, 8, now(), now())
		 ON CONFLICT (provider_id) DO UPDATE SET
		   state = EXCLUDED.state, availability = EXCLUDED.availability,
		   request_count = EXCLUDED.request_count, last_request_at = EXCLUDED.last_request_at,
		   updated_at = EXCLUDED.updated_at`,
		seed.providerID); err != nil {
		t.Fatalf("写 avail_current 夹具失败: %v", err)
	}
	_, body = call(router, http.MethodGet, "/api/availability/current", "")
	entry = availabilityEntryOf(t, body, seed.providerID)
	if entry["status"] != "green" || entry["availability"].(float64) != 0.25 ||
		entry["requestCount"].(float64) != 8 {
		t.Fatalf("新鲜状态行应原样透出：%v", entry)
	}
	if entry["lastRequestAt"] == nil {
		t.Fatalf("有 last_request_at 时不应给 null：%v", entry)
	}

	// 把状态行做旧（超过 15 分钟窗口）：必须被丢弃，再回落 1 分钟桶。
	if _, err := seed.pool.Exec(ctx,
		`UPDATE avail_current SET updated_at = now() - INTERVAL '30 minutes',
		        last_request_at = now() - INTERVAL '30 minutes' WHERE provider_id = $1`,
		seed.providerID); err != nil {
		t.Fatalf("做旧 avail_current 失败: %v", err)
	}
	if _, err := seed.pool.Exec(ctx,
		`INSERT INTO avail_bucket_1m (provider_id, bucket_start, success_cnt, failure_cnt, last_request_at)
		 VALUES ($1, now() - INTERVAL '1 minute', 3, 1, now() - INTERVAL '1 minute')
		 ON CONFLICT (provider_id, bucket_start) DO UPDATE SET
		   success_cnt = EXCLUDED.success_cnt, failure_cnt = EXCLUDED.failure_cnt,
		   last_request_at = EXCLUDED.last_request_at`,
		seed.providerID); err != nil {
		t.Fatalf("写桶夹具失败: %v", err)
	}
	_, body = call(router, http.MethodGet, "/api/availability/current", "")
	entry = availabilityEntryOf(t, body, seed.providerID)
	if entry["status"] != "green" {
		t.Fatalf("回落分支 3 绿 1 红应判 green：%v", entry)
	}
	if entry["availability"].(float64) != 0.75 || entry["requestCount"].(float64) != 4 {
		t.Fatalf("回落分支应给 0.75 / 4：%v", entry)
	}

	// 桶也做旧（超窗口）：回落分支同样不给陈旧数据。
	if _, err := seed.pool.Exec(ctx,
		`UPDATE avail_bucket_1m SET bucket_start = now() - INTERVAL '30 minutes',
		        last_request_at = now() - INTERVAL '30 minutes' WHERE provider_id = $1`,
		seed.providerID); err != nil {
		t.Fatalf("做旧桶失败: %v", err)
	}
	_, body = call(router, http.MethodGet, "/api/availability/current", "")
	entry = availabilityEntryOf(t, body, seed.providerID)
	if entry["status"] != "unknown" {
		t.Fatalf("窗口外应回 unknown：%v", entry)
	}
}

// availabilityEntryOf 从响应里取夹具供应商那一项。
func availabilityEntryOf(t *testing.T, body string, providerID int64) map[string]any {
	t.Helper()
	var entries []map[string]any
	if err := json.Unmarshal([]byte(body), &entries); err != nil {
		t.Fatalf("当前状态响应不是数组：%s", body)
	}
	for _, entry := range entries {
		if id, ok := entry["providerId"].(float64); ok && int64(id) == providerID {
			return entry
		}
	}
	return nil
}
