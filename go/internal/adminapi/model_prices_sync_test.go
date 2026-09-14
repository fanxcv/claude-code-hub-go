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

// 本文件是 model-prices 三兄弟（upload / syncLitellmCheck / syncLitellm）的**真库 + 假上游**
// 集成测试。夹具纪律与 model_prices_test.go 一致：模型名带唯一前缀、按前缀清理。
//
// 云端拉取一律指向 httptest 假上游（`CCH_CLOUD_PRICE_TABLE_URL`），测试**不打外网**。

func modelPriceSyncRouter(t *testing.T, pools *store.Pools, deps *Deps) *Router {
	t.Helper()
	if deps.Guard == nil {
		deps.Guard = newTestGuard(t, GuardOptions{})
	}
	if deps.Problems == nil {
		deps.Problems = NewProblems(nil)
	}
	deps.Store = pools
	router := New(Options{Deps: *deps})
	RegisterModelPriceSyncRoutes(router, *deps)
	return router
}

func modelPriceSyncName(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("go-mpsync-%d", time.Now().UnixNano())
}

// snapshotCloudCatalog 快照 cloud_pricing_catalog（单行表）并在 cleanup 还原，
// 避免把共享测试库里的目录数据冲掉。
func snapshotCloudCatalog(t *testing.T, pools *store.Pools) {
	t.Helper()
	ctx := context.Background()
	control, err := pools.Control()
	if err != nil {
		t.Fatalf("取读连接失败: %v", err)
	}
	var rows []string
	result, err := control.Query(ctx, `SELECT row_to_json(t)::text FROM (SELECT * FROM cloud_pricing_catalog) t`)
	if err == nil {
		for result.Next() {
			var encoded string
			if scanErr := result.Scan(&encoded); scanErr == nil {
				rows = append(rows, encoded)
			}
		}
		result.Close()
	}

	t.Cleanup(func() {
		writer, writeErr := pools.Writer()
		if writeErr != nil {
			t.Logf("cleanup 取写连接失败: %v", writeErr)
			return
		}
		if _, delErr := writer.Exec(ctx, `DELETE FROM cloud_pricing_catalog`); delErr != nil {
			t.Logf("cleanup 清空目录失败: %v", delErr)
			return
		}
		for _, encoded := range rows {
			if _, insertErr := writer.Exec(ctx,
				`INSERT INTO cloud_pricing_catalog
				 SELECT * FROM jsonb_populate_record(NULL::cloud_pricing_catalog, $1::jsonb)`,
				encoded); insertErr != nil {
				t.Logf("cleanup 还原目录失败: %v", insertErr)
			}
		}
	})
}

// cloudPriceTableServer 起一个假上游，返回固定 CPT 表（两个模型，名字带前缀）。
func cloudPriceTableServer(t *testing.T, prefix string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(syncCloudTableFixture(prefix)))
	}))
	t.Cleanup(server.Close)
	return server
}

func syncCloudTableFixture(prefix string) string {
	return fmt.Sprintf(`{
	  "schema": "cchp.pricing-table/v1",
	  "version": "2026.09.13-%s",
	  "currency": "USD",
	  "refreshed_at": "2026-09-13T00:00:00Z",
	  "providers": {"acme": {"name": "Acme"}},
	  "models": [
	    {
	      "slug": "%s-a", "model_name": "%s-a", "vendor": "acme", "display_name": "A",
	      "model_type": "chat",
	      "pricing": [{"provider": "acme", "official": true, "source": "vendor",
	        "charges": {"prompt": {"price": "1", "unit": "per_M_tokens"}}}]
	    },
	    {
	      "slug": "%s-b", "model_name": "%s-b", "vendor": "acme", "display_name": "B",
	      "model_type": "chat",
	      "pricing": [{"provider": "acme", "official": true, "source": "vendor",
	        "charges": {"prompt": {"price": "2", "unit": "per_M_tokens"}}}]
	    }
	  ]
	}`, prefix, prefix, prefix, prefix, prefix)
}

// TestUploadModelPricesLegacyJson 内部格式上传：写库来源必须是 manual，计数与形状按 Node。
func TestUploadModelPricesLegacyJson(t *testing.T) {
	pools := testPools(t)
	deps := &Deps{}
	router := modelPriceSyncRouter(t, pools, deps)
	prefix := modelPriceSyncName(t)
	cleanupModelPrices(t, pools, prefix)

	body := fmt.Sprintf(`{"content":"{\"sample_spec\":{\"note\":\"x\"},\"%s\":{\"mode\":\"chat\",\"input_cost_per_token\":0.000001},\"%s\":{\"mode\":\"chat\",\"input_cost_per_token\":0.000002}}","overwriteManual":[]}`,
		prefix+"-a", prefix+"-b")
	recorder := modelPriceRequest(t, router, http.MethodPost, "/api/v1/model-prices:upload", body, "application/json")
	if recorder.Code != http.StatusOK {
		t.Fatalf("上传应 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}

	var result struct {
		Added            []string `json:"added"`
		Updated          []string `json:"updated"`
		Unchanged        []string `json:"unchanged"`
		Failed           []string `json:"failed"`
		Total            int      `json:"total"`
		SkippedConflicts []string `json:"skippedConflicts"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatalf("响应不是 PriceUpdateResult: %v (%s)", err, recorder.Body.String())
	}
	if len(result.Added) != 2 || result.Total != 2 {
		t.Fatalf("应新增 2 条：%+v", result)
	}
	if result.SkippedConflicts == nil {
		t.Error("skippedConflicts 必须是数组（Node 恒为 []）")
	}
	if !strings.Contains(recorder.Body.String(), `"skippedConflicts":[]`) {
		t.Errorf("序列化应含空 skippedConflicts：%s", recorder.Body.String())
	}

	// 来源必须是 manual（用户显式上传是权威导入，不被后续云端同步覆盖）。
	row, err := pools.AdminFindLatestModelPriceByName(context.Background(), prefix+"-a")
	if err != nil {
		t.Fatalf("读回上传行失败: %v", err)
	}
	if row.Source != "manual" {
		t.Errorf("上传写入来源应为 manual，实际 %s", row.Source)
	}

	// 重复上传同一内容 → unchanged（幂等）。
	second := modelPriceRequest(t, router, http.MethodPost, "/api/v1/model-prices:upload", body, "application/json")
	if second.Code != http.StatusOK {
		t.Fatalf("二次上传应 200，实际 %d", second.Code)
	}
	var again struct {
		Unchanged []string `json:"unchanged"`
	}
	_ = json.Unmarshal(second.Body.Bytes(), &again)
	if len(again.Unchanged) != 2 {
		t.Errorf("二次上传应全部 unchanged，实际 %+v", again)
	}
}

// TestUploadModelPricesValidation 校验错误必须与 zod 同形（strict + min(1)）。
func TestUploadModelPricesValidation(t *testing.T) {
	pools := testPools(t)
	router := modelPriceSyncRouter(t, pools, &Deps{})

	cases := []struct {
		name     string
		body     string
		wantCode string
	}{
		{"content 缺失", `{}`, "invalid_type"},
		{"content 空串", `{"content":""}`, "too_small"},
		{"content 类型错", `{"content":123}`, "invalid_type"},
		{"未知键", `{"content":"{}","extra":1}`, "unrecognized_keys"},
		{"overwriteManual 类型错", `{"content":"{}","overwriteManual":"x"}`, "invalid_type"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := modelPriceRequest(t, router, http.MethodPost,
				"/api/v1/model-prices:upload", testCase.body, "application/json")
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("应 400，实际 %d：%s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), testCase.wantCode) {
				t.Errorf("期望包含 %q：%s", testCase.wantCode, recorder.Body.String())
			}
		})
	}

	// 非 JSON Content-Type → 415（parseHonoJsonBody 的第一段）。
	recorder := modelPriceRequest(t, router, http.MethodPost, "/api/v1/model-prices:upload", `{}`, "")
	if recorder.Code != http.StatusUnsupportedMediaType {
		t.Errorf("缺 Content-Type 应 415，实际 %d", recorder.Code)
	}
}

// TestUploadModelPricesCptInvalid cpt-schema 校验失败必须返回 400（不是静默当成内部格式）。
func TestUploadModelPricesCptInvalid(t *testing.T) {
	pools := testPools(t)
	router := modelPriceSyncRouter(t, pools, &Deps{})
	// schema 命中但 models 为空 → Node 侧 parseCptTableValue 报「价格表格式无效：models 为空」，
	// handler 归为 400 model_price.action_failed。
	body := `{"content":"{\"schema\":\"cchp.pricing-table/v1\",\"models\":[],\"providers\":{}}"}`
	recorder := modelPriceRequest(t, router, http.MethodPost, "/api/v1/model-prices:upload", body, "application/json")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("应 400，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "model_price.action_failed") {
		t.Errorf("错误码应为 model_price.action_failed：%s", recorder.Body.String())
	}
}

// TestSyncLitellmCheckConflicts 冲突检查：只读，不写库；manual 行与云端同名且云端带 mode 才算冲突。
func TestSyncLitellmCheckConflicts(t *testing.T) {
	pools := testPools(t)
	router := modelPriceSyncRouter(t, pools, &Deps{})
	prefix := modelPriceSyncName(t)
	cleanupModelPrices(t, pools, prefix)

	server := cloudPriceTableServer(t, prefix)
	t.Setenv("CCH_CLOUD_PRICE_TABLE_URL", server.URL)

	// 先无 manual 行 → 无冲突。
	recorder := modelPriceRequest(t, router, http.MethodPost, "/api/v1/model-prices:syncLitellmCheck", "", "application/json")
	if recorder.Code != http.StatusOK {
		t.Fatalf("检查应 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"hasConflicts":false`) {
		t.Errorf("无 manual 行时不该报冲突：%s", recorder.Body.String())
	}

	// 写一条 manual 行（名字与云端表 A 同名）→ 应报 1 条冲突。
	upload := fmt.Sprintf(`{"content":"{\"%s-a\":{\"mode\":\"chat\",\"input_cost_per_token\":0.5}}"}`, prefix)
	if response := modelPriceRequest(t, router, http.MethodPost, "/api/v1/model-prices:upload", upload, "application/json"); response.Code != http.StatusOK {
		t.Fatalf("准备 manual 行失败：%d %s", response.Code, response.Body.String())
	}

	recorder = modelPriceRequest(t, router, http.MethodPost, "/api/v1/model-prices:syncLitellmCheck", "", "application/json")
	if recorder.Code != http.StatusOK {
		t.Fatalf("检查应 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	var check struct {
		HasConflicts bool `json:"hasConflicts"`
		Conflicts    []struct {
			ModelName   string          `json:"modelName"`
			ManualPrice json.RawMessage `json:"manualPrice"`
			CloudPrice  json.RawMessage `json:"cloudPrice"`
		} `json:"conflicts"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &check); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	if !check.HasConflicts || len(check.Conflicts) != 1 {
		t.Fatalf("应报 1 条冲突：%+v", check)
	}
	if check.Conflicts[0].ModelName != prefix+"-a" {
		t.Errorf("冲突模型应为 %s，实际 %s", prefix+"-a", check.Conflicts[0].ModelName)
	}
	if !strings.Contains(string(check.Conflicts[0].ManualPrice), "0.5") {
		t.Errorf("manualPrice 应来自库中的 manual 行：%s", check.Conflicts[0].ManualPrice)
	}
}

// syncTestPools 连**可弃库**（CCH_TEST_SYNC_DSN），专供会做整表切换的用例。
//
// 为何不共用 testPools：syncLitellm 端点的语义里包含「删除本次表内不存在的非 manual 行」，
// 在共享测试库（数千条云端价格行）上跑会把它们清空——本 lane 初版踩过，把 internal/jobs 的
// 集成用例弄红。准备方式：
//
//	psql -U postgres -c 'CREATE DATABASE cch_sync_it TEMPLATE cch_loadtest'
//	CCH_TEST_SYNC_DSN=postgres://postgres:postgres@127.0.0.1:5432/cch_sync_it go test ./internal/adminapi/
func syncTestPools(t *testing.T) *store.Pools {
	t.Helper()
	dsn := os.Getenv("CCH_TEST_SYNC_DSN")
	if dsn == "" {
		t.Skip("未设置 CCH_TEST_SYNC_DSN（可弃库），跳过会做整表切换的同步用例")
	}
	pools, err := store.Open(context.Background(), store.Options{
		DSN:                 dsn,
		Budget:              config.SplitPoolBudget(6),
		Timeouts:            config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
		ApplicationNameBase: "cch-adminapi-sync-it",
	})
	if err != nil {
		t.Fatalf("建立可弃库连接池失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	return pools
}

// TestSyncLitellmWritesAndCleans 立即同步：假上游 → 写库 + 目录落库；manual 行受保护需显式覆盖。
//
// 跑在**可弃库**上（见 syncTestPools）：它会清掉本次表内不存在的非 manual 行。
func TestSyncLitellmWritesAndCleans(t *testing.T) {
	pools := syncTestPools(t)
	router := modelPriceSyncRouter(t, pools, &Deps{})
	prefix := modelPriceSyncName(t)
	cleanupModelPrices(t, pools, prefix)
	snapshotCloudCatalog(t, pools)

	server := cloudPriceTableServer(t, prefix)
	t.Setenv("CCH_CLOUD_PRICE_TABLE_URL", server.URL)

	recorder := modelPriceRequest(t, router, http.MethodPost, "/api/v1/model-prices:syncLitellm", `{}`, "application/json")
	if recorder.Code != http.StatusOK {
		t.Fatalf("同步应 200，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		Added   []string `json:"added"`
		Total   int      `json:"total"`
		Failed  []string `json:"failed"`
		Updated []string `json:"updated"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	if len(result.Added) != 2 || len(result.Failed) != 0 {
		t.Fatalf("首次同步应新增 2 条：%+v", result)
	}
	row, err := pools.AdminFindLatestModelPriceByName(context.Background(), prefix+"-a")
	if err != nil {
		t.Fatalf("读回云端行失败: %v", err)
	}
	if row.Source != "cloud" {
		t.Errorf("同步写入来源应为 cloud，实际 %s", row.Source)
	}
	catalog, err := pools.GetCloudPricingCatalog(context.Background())
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	if catalog == nil || !strings.Contains(catalog.Version, "+cvt1") {
		t.Fatalf("目录应落库且指纹带 +cvt1：%+v", catalog)
	}

	// 二次同步：同一张表 → 全部 unchanged。
	second := modelPriceRequest(t, router, http.MethodPost, "/api/v1/model-prices:syncLitellm", `{}`, "application/json")
	if second.Code != http.StatusOK {
		t.Fatalf("二次同步应 200，实际 %d", second.Code)
	}
	var again struct {
		Unchanged []string `json:"unchanged"`
	}
	_ = json.Unmarshal(second.Body.Bytes(), &again)
	if len(again.Unchanged) != 2 {
		t.Errorf("二次同步应全部 unchanged：%+v", again)
	}

	// 把 A 改成 manual，再同步（不带 overwrite）→ 该条进 skippedConflicts 且库中仍是 manual 价。
	markManual := fmt.Sprintf(`{"content":"{\"%s-a\":{\"mode\":\"chat\",\"input_cost_per_token\":0.75}}"}`, prefix)
	if response := modelPriceRequest(t, router, http.MethodPost, "/api/v1/model-prices:upload", markManual, "application/json"); response.Code != http.StatusOK {
		t.Fatalf("准备 manual 行失败：%d %s", response.Code, response.Body.String())
	}
	protected := modelPriceRequest(t, router, http.MethodPost, "/api/v1/model-prices:syncLitellm", `{}`, "application/json")
	var protectedResult struct {
		SkippedConflicts []string `json:"skippedConflicts"`
	}
	_ = json.Unmarshal(protected.Body.Bytes(), &protectedResult)
	found := false
	for _, name := range protectedResult.SkippedConflicts {
		if name == prefix+"-a" {
			found = true
		}
	}
	if !found {
		t.Errorf("manual 行应出现在 skippedConflicts：%+v", protectedResult)
	}

	// 显式列入 overwriteManual → 该条被替换回 cloud。
	overwrite := modelPriceRequest(t, router, http.MethodPost, "/api/v1/model-prices:syncLitellm",
		fmt.Sprintf(`{"overwriteManual":["%s-a"]}`, prefix), "application/json")
	if overwrite.Code != http.StatusOK {
		t.Fatalf("覆盖同步应 200，实际 %d：%s", overwrite.Code, overwrite.Body.String())
	}
	var overwriteResult struct {
		Updated []string `json:"updated"`
	}
	_ = json.Unmarshal(overwrite.Body.Bytes(), &overwriteResult)
	if len(overwriteResult.Updated) != 1 || overwriteResult.Updated[0] != prefix+"-a" {
		t.Errorf("列入覆盖后应替换该条：%+v", overwriteResult)
	}
	row, _ = pools.AdminFindLatestModelPriceByName(context.Background(), prefix+"-a")
	if row.Source != "cloud" {
		t.Errorf("覆盖后来源应为 cloud，实际 %s", row.Source)
	}
}

// TestSyncLitellmValidation 校验：strict 对象、overwriteManual 必须是字符串数组。
func TestSyncLitellmValidation(t *testing.T) {
	pools := testPools(t)
	router := modelPriceSyncRouter(t, pools, &Deps{})

	cases := []struct {
		name     string
		body     string
		wantCode string
	}{
		{"未知键", `{"overwriteManual":[],"extra":true}`, "unrecognized_keys"},
		{"类型错", `{"overwriteManual":[1,2]}`, "invalid_type"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := modelPriceRequest(t, router, http.MethodPost, "/api/v1/model-prices:syncLitellm", testCase.body, "application/json")
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("应 400，实际 %d：%s", recorder.Code, recorder.Body.String())
			}
			if !strings.Contains(recorder.Body.String(), testCase.wantCode) {
				t.Errorf("期望包含 %q：%s", testCase.wantCode, recorder.Body.String())
			}
		})
	}
}

// TestSyncLitellmUpstreamFailure 上游不可达 → 400 action_failed（Node 侧 action 失败同样归 400）。
func TestSyncLitellmUpstreamFailure(t *testing.T) {
	pools := testPools(t)
	router := modelPriceSyncRouter(t, pools, &Deps{})
	t.Setenv("CCH_CLOUD_PRICE_TABLE_URL", "http://127.0.0.1:1/models.json")

	recorder := modelPriceRequest(t, router, http.MethodPost, "/api/v1/model-prices:syncLitellm", `{}`, "application/json")
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("上游不可达应 400，实际 %d：%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "model_price.action_failed") {
		t.Errorf("错误码应为 model_price.action_failed：%s", recorder.Body.String())
	}
}
