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

	"github.com/fanxcv/claude-code-hub-go/go/internal/jobs"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 provider-endpoints **写路径与拨测**八条端点的端到端用例：真路由表 + 真库
// （`CCH_TEST_DSN` 未设置时跳过）。
//
// 为什么不止有桩用例：这一批的要点全在「库约束与事务的交互」上——唯一索引判定冲突、
// FK restrict 与级联、软删过滤、以及聚合查询的分组口径；桩只能证明「我调了我以为的方法」。
//
// 共享库纪律（与其它集成用例同）：夹具自钉唯一前缀、按精确 id 清、不动别人的行。

// endpointWriteFixture 是一次夹具的定位信息。
type endpointWriteFixture struct {
	prefix   string
	domain   string
	vendorID int64
	pool     *store.Pool
}

// endpointWritePools 取真库连接池；门控未设置时跳过。
func endpointWritePools(t *testing.T) *store.Pools {
	t.Helper()
	return systemSettingsIntegrationPools(t)
}

// seedEndpointWriteFixture 建一个厂与两个端点（claude / codex），并登记清理。
func seedEndpointWriteFixture(t *testing.T, pools *store.Pools) *endpointWriteFixture {
	t.Helper()
	ctx := context.Background()
	prefix := fmt.Sprintf("go-epwrite-it-%d", time.Now().UnixNano())
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
	for _, providerType := range []string{"claude", "codex"} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO provider_endpoints (vendor_id, provider_type, url, is_enabled, sort_order)
			 VALUES ($1, $2, $3, true, 0)`,
			vendorID, providerType, "https://"+domain+"/"+providerType,
		); err != nil {
			t.Fatalf("建端点夹具失败: %v", err)
		}
	}

	fixture := &endpointWriteFixture{prefix: prefix, domain: domain, vendorID: vendorID, pool: pool}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `DELETE FROM provider_endpoints WHERE vendor_id = $1`, vendorID); err != nil {
			t.Errorf("清端点夹具失败: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM providers WHERE provider_vendor_id = $1`, vendorID); err != nil {
			t.Errorf("清供应商夹具失败: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM provider_vendors WHERE id = $1`, vendorID); err != nil {
			t.Errorf("清厂夹具失败: %v", err)
		}
	})
	return fixture
}

// endpointWriteRouter 造真路由表（真 Store + 固定管理员身份 + 可选拨测入口）。
func endpointWriteRouter(
	t *testing.T,
	pools *store.Pools,
	probes EndpointProbeRunner,
	circuits CircuitStateStore,
) *Router {
	t.Helper()
	deps := Deps{
		Logger:         logx.New(nil),
		Guard:          principalGuard{principal: Principal{UserID: 1, Username: "admin", IsAdmin: true}},
		Store:          pools,
		EndpointProbes: probes,
		CircuitStates:  circuits,
	}
	router := New(Options{Deps: deps})
	RegisterProviderEndpointRoutes(router, deps)
	return router
}

// endpointIDOf 读某厂某类型的端点 id。
func endpointIDOf(t *testing.T, fixture *endpointWriteFixture, providerType string) int64 {
	t.Helper()
	var id int64
	if err := fixture.pool.QueryRow(context.Background(),
		`SELECT id FROM provider_endpoints
		  WHERE vendor_id = $1 AND provider_type = $2 AND deleted_at IS NULL
		  ORDER BY id ASC LIMIT 1`,
		fixture.vendorID, providerType).Scan(&id); err != nil {
		t.Fatalf("读端点 id 失败: %v", err)
	}
	return id
}

// decodeObject 把响应正文解成 map（断言字段用）。
func decodeObject(t *testing.T, body string) map[string]any {
	t.Helper()
	decoded := map[string]any{}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("响应不是 JSON 对象: %v（正文：%s）", err, body)
	}
	return decoded
}

func TestProviderVendorUpdateAndDeleteIntegration(t *testing.T) {
	pools := endpointWritePools(t)
	fixture := seedEndpointWriteFixture(t, pools)
	router := endpointWriteRouter(t, pools, nil, nil)

	// PATCH 厂：改名 + 换站点，favicon 应随域名重算。
	status, body := call(router, http.MethodPatch,
		fmt.Sprintf("/api/v1/provider-vendors/%d", fixture.vendorID),
		`{"displayName":"改名后的厂","websiteUrl":"https://new-host.example.org/x"}`)
	if status != http.StatusOK {
		t.Fatalf("PATCH 厂应 200，实得 %d：%s", status, body)
	}
	vendor, _ := decodeObject(t, body)["vendor"].(map[string]any)
	if vendor == nil {
		t.Fatalf("响应缺 vendor 字段：%s", body)
	}
	if vendor["displayName"] != "改名后的厂" {
		t.Fatalf("displayName 未写入：%v", vendor["displayName"])
	}
	if vendor["faviconUrl"] != "https://www.google.com/s2/favicons?domain=new-host.example.org&sz=32" {
		t.Fatalf("favicon 未按域名重算：%v", vendor["faviconUrl"])
	}

	// 清空 websiteUrl：favicon 一并清空（Node 的 editProviderVendor 同语义）。
	status, body = call(router, http.MethodPatch,
		fmt.Sprintf("/api/v1/provider-vendors/%d", fixture.vendorID),
		`{"websiteUrl":null}`)
	if status != http.StatusOK {
		t.Fatalf("清空 websiteUrl 应 200，实得 %d：%s", status, body)
	}
	vendor, _ = decodeObject(t, body)["vendor"].(map[string]any)
	if vendor["websiteUrl"] != nil || vendor["faviconUrl"] != nil {
		t.Fatalf("清空后 websiteUrl/faviconUrl 应为 null：%v / %v",
			vendor["websiteUrl"], vendor["faviconUrl"])
	}

	// 未知字段必须被拒（zod `.strict()`）。
	status, body = call(router, http.MethodPatch,
		fmt.Sprintf("/api/v1/provider-vendors/%d", fixture.vendorID), `{"unknownKey":1}`)
	if status != http.StatusBadRequest || !strings.Contains(body, "unrecognized_keys") {
		t.Fatalf("未知字段应 400 unrecognized_keys，实得 %d：%s", status, body)
	}

	// 非法网站地址必须被拒（zod `.url()`）。
	status, _ = call(router, http.MethodPatch,
		fmt.Sprintf("/api/v1/provider-vendors/%d", fixture.vendorID), `{"websiteUrl":"not-a-url"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("非法 URL 应 400，实得 %d", status)
	}

	// DELETE 厂：端点与供应商一并清掉（FK restrict 下的显式顺序）。
	status, body = call(router, http.MethodDelete,
		fmt.Sprintf("/api/v1/provider-vendors/%d", fixture.vendorID), "")
	if status != http.StatusNoContent {
		t.Fatalf("DELETE 厂应 204，实得 %d：%s", status, body)
	}
	var remaining int
	if err := fixture.pool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM provider_endpoints WHERE vendor_id = $1`, fixture.vendorID).Scan(&remaining); err != nil {
		t.Fatalf("统计残留端点失败: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("删厂后应无残留端点，实得 %d 行", remaining)
	}
	// 删除不存在的厂：本资源按 Node 的口径回 400 + DELETE_FAILED（不是 404）。
	status, body = call(router, http.MethodDelete,
		fmt.Sprintf("/api/v1/provider-vendors/%d", fixture.vendorID), "")
	if status != http.StatusBadRequest || !strings.Contains(body, "DELETE_FAILED") {
		t.Fatalf("删不存在的厂应 400 DELETE_FAILED，实得 %d：%s", status, body)
	}
}

func TestProviderEndpointWritePathsIntegration(t *testing.T) {
	pools := endpointWritePools(t)
	fixture := seedEndpointWriteFixture(t, pools)
	router := endpointWriteRouter(t, pools, nil, nil)

	// 建端点：201 + Location。
	status, body := call(router, http.MethodPost,
		fmt.Sprintf("/api/v1/provider-vendors/%d/endpoints", fixture.vendorID),
		fmt.Sprintf(`{"providerType":"claude","url":"https://%s/extra","label":"备用","sortOrder":3,"isEnabled":false}`,
			fixture.domain))
	if status != http.StatusCreated {
		t.Fatalf("建端点应 201，实得 %d：%s", status, body)
	}
	created, _ := decodeObject(t, body)["endpoint"].(map[string]any)
	if created == nil || created["label"] != "备用" {
		t.Fatalf("建端点响应异常：%s", body)
	}
	createdID := int64(created["id"].(float64))

	// 同一 (厂, 类型, URL) 再建一次：库的唯一索引判冲突，本资源映射成 409 CONFLICT。
	status, body = call(router, http.MethodPost,
		fmt.Sprintf("/api/v1/provider-vendors/%d/endpoints", fixture.vendorID),
		fmt.Sprintf(`{"providerType":"claude","url":"https://%s/extra"}`, fixture.domain))
	if status != http.StatusConflict || !strings.Contains(body, "CONFLICT") {
		t.Fatalf("重复 URL 应 409 CONFLICT，实得 %d：%s", status, body)
	}

	// 隐藏类型在**路由级**就该被拒（Node 的路由 schema 限死公开四档）。
	status, body = call(router, http.MethodPost,
		fmt.Sprintf("/api/v1/provider-vendors/%d/endpoints", fixture.vendorID),
		fmt.Sprintf(`{"providerType":"claude-auth","url":"https://%s/hidden"}`, fixture.domain))
	if status != http.StatusBadRequest || !strings.Contains(body, "invalid_enum_value") {
		t.Fatalf("隐藏类型应 400 invalid_enum_value，实得 %d：%s", status, body)
	}

	// 端点不存在（不存在的 id）时，写路径一律 404 provider_endpoint.not_found。
	status, body = call(router, http.MethodPatch, "/api/v1/provider-endpoints/999999999", `{"label":"x"}`)
	if status != http.StatusNotFound || !strings.Contains(body, "provider_endpoint.not_found") {
		t.Fatalf("改不存在的端点应 404 provider_endpoint.not_found，实得 %d：%s", status, body)
	}

	// 更新端点：改名 + 停用。
	status, body = call(router, http.MethodPatch,
		fmt.Sprintf("/api/v1/provider-endpoints/%d", createdID), `{"label":"改过","isEnabled":true}`)
	if status != http.StatusOK {
		t.Fatalf("改端点应 200，实得 %d：%s", status, body)
	}
	updated, _ := decodeObject(t, body)["endpoint"].(map[string]any)
	if updated["label"] != "改过" || updated["isEnabled"] != true {
		t.Fatalf("端点更新未生效：%s", body)
	}

	// 端点统计批量：按厂聚合，去重后按首次出现顺序作答。
	status, body = call(router, http.MethodPost, "/api/v1/provider-vendors/endpoint-stats:batch",
		fmt.Sprintf(`{"vendorIds":[%d,%d],"providerType":"claude"}`, fixture.vendorID, fixture.vendorID))
	if status != http.StatusOK {
		t.Fatalf("端点统计应 200，实得 %d：%s", status, body)
	}
	var stats []map[string]any
	if err := json.Unmarshal([]byte(body), &stats); err != nil {
		t.Fatalf("统计响应不是数组：%s", body)
	}
	if len(stats) != 1 {
		t.Fatalf("去重后应只一项，实得 %d：%s", len(stats), body)
	}
	if stats[0]["total"].(float64) < 2 {
		t.Fatalf("claude 类型端点总数应至少 2（夹具两行 + 新建一行），实得 %v", stats[0]["total"])
	}

	// 批量探活历史：裸数组、每端点一项、缺日志给空数组。
	status, body = call(router, http.MethodPost, "/api/v1/provider-endpoints/probe-logs:batch",
		fmt.Sprintf(`{"endpointIds":[%d],"limit":5}`, createdID))
	if status != http.StatusOK {
		t.Fatalf("批量探活历史应 200，实得 %d：%s", status, body)
	}
	var logs []map[string]any
	if err := json.Unmarshal([]byte(body), &logs); err != nil {
		t.Fatalf("探活历史响应不是数组：%s", body)
	}
	if len(logs) != 1 || logs[0]["endpointId"].(float64) != float64(createdID) {
		t.Fatalf("探活历史形状异常：%s", body)
	}
	if entries, ok := logs[0]["logs"].([]any); !ok || len(entries) != 0 {
		t.Fatalf("无历史时应给空数组：%s", body)
	}

	// 删除端点：204，行仍在但已软删。
	status, body = call(router, http.MethodDelete,
		fmt.Sprintf("/api/v1/provider-endpoints/%d", createdID), "")
	if status != http.StatusNoContent {
		t.Fatalf("删端点应 204，实得 %d：%s", status, body)
	}
	var deletedAt *time.Time
	if err := fixture.pool.QueryRow(context.Background(),
		`SELECT deleted_at FROM provider_endpoints WHERE id = $1`, createdID).Scan(&deletedAt); err != nil {
		t.Fatalf("读软删列失败: %v", err)
	}
	if deletedAt == nil {
		t.Fatal("删端点应写 deleted_at，实得仍为 NULL")
	}
	// 软删后的端点对写路径不可见（404），且不会挡住同 URL 的重建（partial 唯一索引）。
	status, _ = call(router, http.MethodPatch,
		fmt.Sprintf("/api/v1/provider-endpoints/%d", createdID), `{"label":"z"}`)
	if status != http.StatusNotFound {
		t.Fatalf("软删端点应 404，实得 %d", status)
	}
	status, body = call(router, http.MethodPost,
		fmt.Sprintf("/api/v1/provider-vendors/%d/endpoints", fixture.vendorID),
		fmt.Sprintf(`{"providerType":"claude","url":"https://%s/extra"}`, fixture.domain))
	if status != http.StatusCreated {
		t.Fatalf("软删后同 URL 重建应 201，实得 %d：%s", status, body)
	}
}

func TestProviderEndpointDeleteBlockedByEnabledProviderIntegration(t *testing.T) {
	pools := endpointWritePools(t)
	fixture := seedEndpointWriteFixture(t, pools)
	router := endpointWriteRouter(t, pools, nil, nil)
	endpointID := endpointIDOf(t, fixture, "claude")

	// 造一个**启用**的供应商，URL 与该端点一致：删除必须被拒（409 + 引用错误码）。
	var providerID int64
	if err := fixture.pool.QueryRow(context.Background(),
		`INSERT INTO providers (name, url, key, provider_vendor_id, provider_type, is_enabled)
		 VALUES ($1, $2, $3, $4, 'claude', true) RETURNING id`,
		fixture.prefix+"-provider", "https://"+fixture.domain+"/claude",
		"sk-"+fixture.prefix, fixture.vendorID,
	).Scan(&providerID); err != nil {
		t.Fatalf("建供应商夹具失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = fixture.pool.Exec(context.Background(), `DELETE FROM providers WHERE id = $1`, providerID)
	})

	status, body := call(router, http.MethodDelete,
		fmt.Sprintf("/api/v1/provider-endpoints/%d", endpointID), "")
	if status != http.StatusConflict {
		t.Fatalf("被启用供应商引用时应 409，实得 %d：%s", status, body)
	}
	if !strings.Contains(body, "ENDPOINT_REFERENCED_BY_ENABLED_PROVIDERS") {
		t.Fatalf("错误码应为 ENDPOINT_REFERENCED_BY_ENABLED_PROVIDERS：%s", body)
	}
	problem := decodeObject(t, body)
	params, _ := problem["errorParams"].(map[string]any)
	if params == nil || params["count"].(float64) != 1 {
		t.Fatalf("errorParams 应带引用条数 1：%s", body)
	}
	if params["providers"] != fixture.prefix+"-provider" {
		t.Fatalf("errorParams.providers 应是引用方名字：%s", body)
	}

	// 供应商停用后即可删除（引用检查只看启用态）。
	if _, err := fixture.pool.Exec(context.Background(),
		`UPDATE providers SET is_enabled = false WHERE id = $1`, providerID); err != nil {
		t.Fatalf("停用供应商夹具失败: %v", err)
	}
	status, body = call(router, http.MethodDelete,
		fmt.Sprintf("/api/v1/provider-endpoints/%d", endpointID), "")
	if status != http.StatusNoContent {
		t.Fatalf("停用后应可删端点（204），实得 %d：%s", status, body)
	}
}

// fakeProbeRunner 记录调用并返回固定结果（拨测端点的路由级用例）。
type fakeProbeRunner struct {
	calls    int
	lastID   int64
	lastName string
	timeout  int
	result   jobs.ProbeOnceResult
	err      error
}

func (f *fakeProbeRunner) ProbeEndpoint(
	_ context.Context,
	endpoint store.ProbeEndpoint,
	source string,
	timeoutMS int,
) (jobs.ProbeOnceResult, error) {
	f.calls++
	f.lastID = endpoint.ID
	f.lastName = source
	f.timeout = timeoutMS
	return f.result, f.err
}

func TestProviderEndpointProbeRouteIntegration(t *testing.T) {
	pools := endpointWritePools(t)
	fixture := seedEndpointWriteFixture(t, pools)
	endpointID := endpointIDOf(t, fixture, "claude")

	statusCode := 204
	latency := 7
	runner := &fakeProbeRunner{
		result: jobs.ProbeOnceResult{
			OK: true, Method: "TCP", StatusCode: &statusCode, LatencyMS: &latency,
		},
	}
	router := endpointWriteRouter(t, pools, runner, nil)

	status, body := call(router, http.MethodPost,
		fmt.Sprintf("/api/v1/provider-endpoints/%d:probe", endpointID), `{"timeoutMs":2500}`)
	if status != http.StatusOK {
		t.Fatalf("拨测应 200，实得 %d：%s", status, body)
	}
	if runner.calls != 1 || runner.lastID != endpointID || runner.timeout != 2500 {
		t.Fatalf("拨测入参不符：calls=%d id=%d timeout=%d", runner.calls, runner.lastID, runner.timeout)
	}
	if runner.lastName != providerEndpointProbeSourceManual {
		t.Fatalf("来源应为 manual，实得 %q", runner.lastName)
	}
	probe, _ := decodeObject(t, body)["result"].(map[string]any)
	if probe == nil || probe["method"] != "TCP" || probe["ok"] != true {
		t.Fatalf("拨测结果形状异常：%s", body)
	}
	if _, ok := decodeObject(t, body)["endpoint"]; !ok {
		t.Fatalf("响应应带拨测前的端点快照：%s", body)
	}

	// 超时下限：999 应 400（Schema 的 1000..120000）。
	status, _ = call(router, http.MethodPost,
		fmt.Sprintf("/api/v1/provider-endpoints/%d:probe", endpointID), `{"timeoutMs":999}`)
	if status != http.StatusBadRequest {
		t.Fatalf("900ms 应 400，实得 %d", status)
	}

	// 不可见端点（隐藏类型）应 404：这条路由走的是与写路径同一份可见性判定。
	var hiddenID int64
	if err := fixture.pool.QueryRow(context.Background(),
		`INSERT INTO provider_endpoints (vendor_id, provider_type, url, is_enabled, sort_order)
		 VALUES ($1, 'claude-auth', $2, true, 0) RETURNING id`,
		fixture.vendorID, "https://"+fixture.domain+"/hidden-probe",
	).Scan(&hiddenID); err != nil {
		t.Fatalf("建隐藏端点夹具失败: %v", err)
	}
	status, body = call(router, http.MethodPost,
		fmt.Sprintf("/api/v1/provider-endpoints/%d:probe", hiddenID), `{}`)
	if status != http.StatusNotFound || !strings.Contains(body, "provider_endpoint.not_found") {
		t.Fatalf("隐藏端点应 404，实得 %d：%s", status, body)
	}
}

func TestProviderEndpointProbeRouteNotRegisteredWithoutRunner(t *testing.T) {
	pools := endpointWritePools(t)
	router := endpointWriteRouter(t, pools, nil, nil)

	// 未装配拨测入口时该路由不注册：未命中的路由落到 Node 回退（本测试没装回退处理器，
	// 故 router 以 503 作答——生产里那是 Node 的位置）。
	status, _ := call(router, http.MethodPost, "/api/v1/provider-endpoints/1:probe", `{}`)
	if status != http.StatusServiceUnavailable {
		t.Fatalf("未装配拨测时应未注册（落到回退位），实得 %d", status)
	}
	// 同一份 deps 下写路径仍然注册（两者装配条件不同）：它必须给出端点的真实答复，
	// 而不是「未命中」的那份 503。
	status, _ = call(router, http.MethodPatch, "/api/v1/provider-endpoints/1", `{"label":"x"}`)
	if status == http.StatusServiceUnavailable {
		t.Fatal("写路径不应因拨测未装配而失效")
	}
}

// TestProviderEndpointProbeRunnerIntegration 验真拨测入口本身（jobs 侧），
// 指向 httptest 假上游，不打真实网络。
func TestProviderEndpointProbeRunnerIntegration(t *testing.T) {
	pools := endpointWritePools(t)
	dsn := os.Getenv("CCH_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 CCH_TEST_DSN，跳过数据库集成测试")
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		// 只接受 HEAD（Node 的默认 HTTP 方法是 HEAD，网络失败才回落 GET）。
		if request.Method != http.MethodHead && request.Method != http.MethodGet {
			t.Errorf("拨测方法异常: %s", request.Method)
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	fixture := seedEndpointWriteFixture(t, pools)
	ctx := context.Background()
	var endpointID int64
	if err := fixture.pool.QueryRow(ctx,
		`UPDATE provider_endpoints
		    SET url = $2
		  WHERE vendor_id = $1 AND provider_type = 'claude'
		  RETURNING id`,
		fixture.vendorID, upstream.URL).Scan(&endpointID); err != nil {
		t.Fatalf("改端点 URL 为假上游失败: %v", err)
	}

	runner := jobs.NewProbeOnceRunner(jobs.ProbeOnceOptions{
		Pools:  pools,
		Logger: logx.New(nil),
		Lookup: func(name string) (string, bool) {
			// 钉死 HTTP 路径与 2 秒超时，不受运行环境变量影响。
			switch name {
			case "ENDPOINT_PROBE_METHOD":
				return "HEAD", true
			case "ENDPOINT_PROBE_TIMEOUT_MS":
				return "2000", true
			default:
				return "", false
			}
		},
	})
	if runner == nil {
		t.Fatal("有连接池时拨测入口不应为 nil")
	}

	result, err := runner.ProbeEndpoint(ctx, store.ProbeEndpoint{
		ID: endpointID, URL: upstream.URL,
	}, providerEndpointProbeSourceManual, 0)
	if err != nil {
		t.Fatalf("拨测失败: %v", err)
	}
	if !result.OK || result.Method != "HEAD" {
		t.Fatalf("拨测结果应为 HEAD 成功：%+v", result)
	}
	if result.StatusCode == nil || *result.StatusCode != http.StatusNoContent {
		t.Fatalf("状态码应为 204：%+v", result.StatusCode)
	}

	// 拨测必须落库：端点快照列与探活历史各一处。
	var lastOK *bool
	var source string
	if err := fixture.pool.QueryRow(ctx,
		`SELECT last_probe_ok FROM provider_endpoints WHERE id = $1`, endpointID).Scan(&lastOK); err != nil {
		t.Fatalf("读端点快照失败: %v", err)
	}
	if lastOK == nil || !*lastOK {
		t.Fatalf("端点快照应记成功，实得 %v", lastOK)
	}
	if err := fixture.pool.QueryRow(ctx,
		`SELECT source FROM provider_endpoint_probe_logs WHERE endpoint_id = $1
		  ORDER BY id DESC LIMIT 1`, endpointID).Scan(&source); err != nil {
		t.Fatalf("读探活历史失败: %v", err)
	}
	if source != providerEndpointProbeSourceManual {
		t.Fatalf("探活历史来源应为 manual，实得 %q", source)
	}
}
