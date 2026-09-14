package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 model-prices 六条已实现端点的真实 PG 集成测试（lane A1-5）。
//
// 夹具纪律：模型名一律带 `go-a15-mp-<纳秒>` 前缀（唯一），清理按前缀精确删除，且清理用的
// 连接池不会被本测试关闭（testPools 的 Cleanup 是 LIFO，后注册的本清理先跑）。
// 列表类断言必须按自己的模型名过滤——库里有别的测试与生产数据。

func modelPricesRouter(t *testing.T, pools *store.Pools, deps *Deps) *Router {
	t.Helper()
	if deps.Guard == nil {
		deps.Guard = newTestGuard(t, GuardOptions{})
	}
	if deps.Problems == nil {
		deps.Problems = NewProblems(nil)
	}
	deps.Store = pools
	router := New(Options{Deps: *deps})
	RegisterModelPrices(router, *deps)
	return router
}

func modelPriceRequest(
	t *testing.T,
	router *Router,
	method, target, body, contentType string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, target, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+testAdminToken)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

func modelPriceName(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("go-a15-mp-%d", time.Now().UnixNano())
}

func cleanupModelPrices(t *testing.T, pools *store.Pools, prefix string) {
	t.Helper()
	pool, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		"DELETE FROM model_prices WHERE model_name LIKE $1", prefix+"%"); err != nil {
		t.Fatalf("清理模型价格失败: %v", err)
	}
}

// TestModelPricesUpsertListDeleteRoundTrip 覆盖 PUT → list/catalog/exists → DELETE 的回路。
func TestModelPricesUpsertListDeleteRoundTrip(t *testing.T) {
	pools := testPools(t)
	// 跨包互斥：本用例往共享库写价格行再按名读回，并发的整表切换会把中间的行删掉。
	lockModelPricesTable(t, pools)
	router := modelPricesRouter(t, pools, &Deps{})
	prefix := modelPriceName(t)
	t.Cleanup(func() { cleanupModelPrices(t, pools, prefix) })

	body := fmt.Sprintf(`{"modelName":"%s","displayName":"A15 集成测试","mode":"chat",
		"litellmProvider":"openai","supportsPromptCaching":true,
		"inputCostPerToken":0.000003,"outputCostPerToken":0.000015}`, prefix)
	upserted := modelPriceRequest(t, router, http.MethodPut, "/api/v1/model-prices/"+prefix,
		body, "application/json")
	if upserted.Code != http.StatusOK {
		t.Fatalf("PUT 应 200，实际 %d body=%s", upserted.Code, upserted.Body.String())
	}
	item := decodeModelPriceItem(t, upserted.Body.Bytes())
	if item["modelName"] != prefix || item["source"] != "manual" {
		t.Fatalf("PUT 响应字段不符: %v", item)
	}
	priceData, _ := item["priceData"].(map[string]any)
	if priceData["mode"] != "chat" || priceData["display_name"] != "A15 集成测试" ||
		priceData["input_cost_per_token"] != 0.000003 || priceData["supports_prompt_caching"] != true {
		t.Fatalf("priceData 不符: %v", priceData)
	}
	if createdAt, _ := item["createdAt"].(string); len(createdAt) != 24 || !strings.HasSuffix(createdAt, "Z") {
		t.Fatalf("createdAt 应为 toISOString 形状: %v", item["createdAt"])
	}

	listed := modelPriceRequest(t, router, http.MethodGet, "/api/v1/model-prices?search="+prefix, "", "")
	if listed.Code != http.StatusOK {
		t.Fatalf("列表应 200，实际 %d body=%s", listed.Code, listed.Body.String())
	}
	page := a15DecodeJSON(t, listed.Body.Bytes())
	if page["total"].(float64) != 1 || page["page"].(float64) != 1 || page["pageSize"].(float64) != 20 ||
		page["totalPages"].(float64) != 1 {
		t.Fatalf("分页字段不符（search 只应命中本夹具）: %v", page)
	}
	items, _ := page["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items 应恰好 1 条: %v", items)
	}

	// source=cloud 的语义是 `source <> 'manual'`：manual 夹具不得出现。
	cloudOnly := modelPriceRequest(t, router, http.MethodGet,
		"/api/v1/model-prices?search="+prefix+"&source=cloud", "", "")
	if total := a15DecodeJSON(t, cloudOnly.Body.Bytes())["total"].(float64); total != 0 {
		t.Fatalf("source=cloud 不应命中 manual 行: %v", total)
	}
	manualOnly := modelPriceRequest(t, router, http.MethodGet,
		"/api/v1/model-prices?search="+prefix+"&source=manual", "", "")
	if total := a15DecodeJSON(t, manualOnly.Body.Bytes())["total"].(float64); total != 1 {
		t.Fatalf("source=manual 应命中 1 条: %v", total)
	}

	catalog := modelPriceRequest(t, router, http.MethodGet, "/api/v1/model-prices/catalog", "", "")
	if catalog.Code != http.StatusOK {
		t.Fatalf("catalog 应 200，实际 %d", catalog.Code)
	}
	if !modelPriceCatalogContains(t, catalog.Body.Bytes(), prefix) {
		t.Fatalf("catalog 未包含本夹具（mode=chat）：%s", catalog.Body.String())
	}
	allScope := modelPriceRequest(t, router, http.MethodGet, "/api/v1/model-prices/catalog?scope=all", "", "")
	if !modelPriceCatalogContains(t, allScope.Body.Bytes(), prefix) {
		t.Fatalf("catalog?scope=all 未包含本夹具")
	}

	exists := modelPriceRequest(t, router, http.MethodGet, "/api/v1/model-prices/exists", "", "")
	if exists.Code != http.StatusOK {
		t.Fatalf("exists 应 200，实际 %d", exists.Code)
	}
	existsBody := a15DecodeJSON(t, exists.Body.Bytes())
	if _, ok := existsBody["exists"].(bool); !ok {
		t.Fatalf("exists 响应应为 {exists: bool}: %v", existsBody)
	}

	deleted := modelPriceRequest(t, router, http.MethodDelete, "/api/v1/model-prices/"+prefix, "", "")
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("DELETE 应 204，实际 %d body=%s", deleted.Code, deleted.Body.String())
	}
	afterDelete := modelPriceRequest(t, router, http.MethodGet, "/api/v1/model-prices?search="+prefix, "", "")
	if total := a15DecodeJSON(t, afterDelete.Body.Bytes())["total"].(float64); total != 0 {
		t.Fatalf("删除后应查不到: %v", total)
	}
}

// TestModelPricesPinManualPromotesProviderPricing 覆盖 pinManual：把云端多供应商价固化成本地 manual。
func TestModelPricesPinManualPromotesProviderPricing(t *testing.T) {
	pools := testPools(t)
	// 跨包互斥：本用例直接往共享库写云端行，再调用 pinManual 改它。
	lockModelPricesTable(t, pools)
	router := modelPricesRouter(t, pools, &Deps{})
	prefix := modelPriceName(t)
	t.Cleanup(func() { cleanupModelPrices(t, pools, prefix) })

	// 云端行由 store 直接写：pinManual 要求 source=cloud（或 litellm）且带 pricing.<key> 节点。
	cloudData := json.RawMessage(`{"mode":"chat","vendor":"acme",
		"input_cost_per_token":0.000002,
		"pricing":{"acme-premium":{"input_cost_per_token":0.000009,"output_cost_per_token":0.00003}}}`)
	if _, err := pools.AdminUpsertModelPrice(context.Background(), prefix, cloudData, "cloud"); err != nil {
		t.Fatalf("写入云端夹具失败: %v", err)
	}

	pinned := modelPriceRequest(t, router, http.MethodPost,
		"/api/v1/model-prices/"+prefix+"/pricing:pinManual",
		`{"pricingProviderKey":"acme-premium"}`, "application/json")
	if pinned.Code != http.StatusOK {
		t.Fatalf("pinManual 应 200，实际 %d body=%s", pinned.Code, pinned.Body.String())
	}
	item := decodeModelPriceItem(t, pinned.Body.Bytes())
	if item["source"] != "manual" {
		t.Fatalf("固化后来源应为 manual: %v", item["source"])
	}
	priceData, _ := item["priceData"].(map[string]any)
	if priceData["input_cost_per_token"] != 0.000009 ||
		priceData["litellm_provider"] != "acme-premium" ||
		priceData["selected_pricing_provider"] != "acme-premium" ||
		priceData["selected_pricing_source_model"] != prefix ||
		priceData["selected_pricing_resolution"] != "manual_pin" {
		t.Fatalf("固化后的价格数据不符: %v", priceData)
	}
	if _, hasPricing := priceData["pricing"]; hasPricing {
		t.Fatalf("pricing 节点应被删除（Node 侧是 pricing: undefined，JSON.stringify 会丢掉该键）: %v", priceData)
	}

	// 没有 pricing 节点的模型 → 404 model_price.not_found（Node 的「未找到对应的多供应商价格节点」）。
	plain := prefix + "-nolist"
	plainData := json.RawMessage(`{"mode":"chat","input_cost_per_token":0.000001}`)
	if _, err := pools.AdminUpsertModelPrice(context.Background(), plain, plainData, "cloud"); err != nil {
		t.Fatalf("写入第二个云端夹具失败: %v", err)
	}
	missingNode := modelPriceRequest(t, router, http.MethodPost,
		"/api/v1/model-prices/"+plain+"/pricing:pinManual",
		`{"pricingProviderKey":"acme-premium"}`, "application/json")
	if missingNode.Code != http.StatusNotFound {
		t.Fatalf("无 pricing 节点应 404，实际 %d body=%s", missingNode.Code, missingNode.Body.String())
	}
	if code := a15DecodeProblemCode(t, missingNode.Body.Bytes()); code != "model_price.not_found" {
		t.Fatalf("404 错误码应为 model_price.not_found，实际 %q", code)
	}

	// 完全查不到云端行 → 同样 404 model_price.not_found。
	unknown := modelPriceRequest(t, router, http.MethodPost,
		"/api/v1/model-prices/"+prefix+"-ghost/pricing:pinManual",
		`{"pricingProviderKey":"acme-premium"}`, "application/json")
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("云端行不存在应 404，实际 %d body=%s", unknown.Code, unknown.Body.String())
	}
}

// TestModelPricesRejectsBadRequests 覆盖查询与正文的校验失败（状态码与信封）。
func TestModelPricesRejectsBadRequests(t *testing.T) {
	pools := testPools(t)
	router := modelPricesRouter(t, pools, &Deps{})
	prefix := modelPriceName(t)
	t.Cleanup(func() { cleanupModelPrices(t, pools, prefix) })

	cases := []struct {
		name   string
		target string
		expect int
		// code 非空时断 invalidParams[0].code：方向语义不得写反
		// （zod 4 对超上限报 too_big、对低于下限报 too_small）。
		code string
	}{
		{"page 下界", "/api/v1/model-prices?page=0", http.StatusBadRequest, "too_small"},
		{"pageSize 上界", "/api/v1/model-prices?pageSize=101", http.StatusBadRequest, "too_big"},
		{"page 非数字", "/api/v1/model-prices?page=abc", http.StatusBadRequest, ""},
		{"source 枚举", "/api/v1/model-prices?source=garbage", http.StatusBadRequest, ""},
		{"catalog scope 枚举", "/api/v1/model-prices/catalog?scope=nope", http.StatusBadRequest, ""},
		{"合法查询", "/api/v1/model-prices?page=1&pageSize=1", http.StatusOK, ""},
	}
	for _, item := range cases {
		response := modelPriceRequest(t, router, http.MethodGet, item.target, "", "")
		if response.Code != item.expect {
			t.Fatalf("%s: 期望 %d，实际 %d body=%s", item.name, item.expect, response.Code, response.Body.String())
		}
		if item.code == "" {
			continue
		}
		var problem struct {
			InvalidParams []struct {
				Path  []any  `json:"path"`
				Code  string `json:"code"`
				Value string `json:"message"`
			} `json:"invalidParams"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
			t.Fatalf("%s: 校验正文解析失败: %v", item.name, err)
		}
		if len(problem.InvalidParams) == 0 {
			t.Fatalf("%s: 校验失败必须带 invalidParams，实际 %s", item.name, response.Body.String())
		}
		if problem.InvalidParams[0].Code != item.code {
			t.Fatalf("%s: invalidParams[0].code 应为 %s，实际 %s（body=%s）",
				item.name, item.code, problem.InvalidParams[0].Code, response.Body.String())
		}
	}

	unknownKey := modelPriceRequest(t, router, http.MethodPut, "/api/v1/model-prices/"+prefix,
		`{"modelName":"`+prefix+`","mode":"chat","nope":1}`, "application/json")
	if unknownKey.Code != http.StatusBadRequest {
		t.Fatalf("未知键应 400，实际 %d", unknownKey.Code)
	}

	badExtra := modelPriceRequest(t, router, http.MethodPut, "/api/v1/model-prices/"+prefix,
		`{"modelName":"`+prefix+`","mode":"chat","extraFieldsJson":"[1,2]"}`, "application/json")
	if badExtra.Code != http.StatusBadRequest {
		t.Fatalf("extraFieldsJson 非对象应 400，实际 %d body=%s", badExtra.Code, badExtra.Body.String())
	}

	negativeExtra := modelPriceRequest(t, router, http.MethodPut, "/api/v1/model-prices/"+prefix,
		`{"modelName":"`+prefix+`","mode":"chat","extraFieldsJson":"{\"input_cost_per_token\":\"-1\"}"}`,
		"application/json")
	if negativeExtra.Code != http.StatusBadRequest {
		t.Fatalf("价格类高级字段为负数应 400，实际 %d body=%s", negativeExtra.Code, negativeExtra.Body.String())
	}

	missingMode := modelPriceRequest(t, router, http.MethodPut, "/api/v1/model-prices/"+prefix,
		`{"modelName":"`+prefix+`"}`, "application/json")
	if missingMode.Code != http.StatusBadRequest {
		t.Fatalf("缺 mode 应 400，实际 %d", missingMode.Code)
	}
	problem := a15DecodeJSON(t, missingMode.Body.Bytes())
	if problem["errorCode"] != "request.validation_failed" {
		t.Fatalf("校验失败信封不符: %v", problem)
	}
}

// TestModelPricesUpsertReplacesRowsAndAudits 验证 upsert 的「先删后插」语义与审计 before 快照。
func TestModelPricesUpsertReplacesRowsAndAudits(t *testing.T) {
	pools := testPools(t)
	// 跨包互斥：本用例验收 upsert 的「先删后插」并读回审计 before 快照，中途被别包删行会失真。
	lockModelPricesTable(t, pools)
	deps := Deps{Problems: NewProblems(nil)}
	audit, err := NewAuditLog(deps, AuditLogOptions{Pools: pools, Timeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("建立审计写入器失败: %v", err)
	}
	deps.Audit = audit
	router := modelPricesRouter(t, pools, &deps)
	prefix := modelPriceName(t)
	t.Cleanup(func() { cleanupModelPrices(t, pools, prefix) })

	first := modelPriceRequest(t, router, http.MethodPut, "/api/v1/model-prices/"+prefix,
		`{"modelName":"`+prefix+`","mode":"chat","inputCostPerToken":0.000001}`, "application/json")
	if first.Code != http.StatusOK {
		t.Fatalf("首次写入应 200，实际 %d body=%s", first.Code, first.Body.String())
	}
	firstID := int64(decodeModelPriceItem(t, first.Body.Bytes())["id"].(float64))

	second := modelPriceRequest(t, router, http.MethodPut, "/api/v1/model-prices/"+prefix,
		`{"modelName":"`+prefix+`","mode":"chat","inputCostPerToken":0.000002}`, "application/json")
	if second.Code != http.StatusOK {
		t.Fatalf("二次写入应 200，实际 %d", second.Code)
	}
	secondID := int64(decodeModelPriceItem(t, second.Body.Bytes())["id"].(float64))
	if secondID == firstID {
		t.Fatalf("upsert 是先删后插，id 必须变化（%d 未变）", firstID)
	}

	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	var rows int64
	if err := pool.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM model_prices WHERE model_name = $1", prefix).Scan(&rows); err != nil {
		t.Fatalf("统计行数失败: %v", err)
	}
	if rows != 1 {
		t.Fatalf("upsert 后该模型应只剩 1 行，实际 %d", rows)
	}

	// 审计的 before 快照必须落在第二次写入那笔上（真库、fire-and-forget → 有界轮询 3 秒）。
	deadline := time.Now().Add(3 * time.Second)
	for {
		var before string
		scanErr := pool.QueryRow(context.Background(),
			`SELECT COALESCE(before_value::text, '') FROM audit_log
			 WHERE action_type = 'model_price.upsert' AND target_id = $1
			 ORDER BY id DESC LIMIT 1`, fmt.Sprintf("%d", secondID)).Scan(&before)
		if scanErr == nil {
			if before == "" || !strings.Contains(before, fmt.Sprintf(`"id": %d`, firstID)) {
				t.Fatalf("审计 before 快照应含首次写入的行（id=%d），实际 %s", firstID, before)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("3 秒内未看到 model_price.upsert 的审计行: %v", scanErr)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func decodeModelPriceItem(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	return a15DecodeJSON(t, payload)
}

func modelPriceCatalogContains(t *testing.T, payload []byte, modelName string) bool {
	t.Helper()
	object := a15DecodeJSON(t, payload)
	items, _ := object["items"].([]any)
	for _, item := range items {
		node, _ := item.(map[string]any)
		if node["modelName"] == modelName {
			if _, ok := node["vendor"]; !ok {
				t.Fatalf("catalog 项缺少 vendor 字段: %v", node)
			}
			if _, ok := node["litellmProvider"]; !ok {
				t.Fatalf("catalog 项缺少 litellmProvider 字段: %v", node)
			}
			if updatedAt, _ := node["updatedAt"].(string); len(updatedAt) != 24 {
				t.Fatalf("catalog 的 updatedAt 应为 toISOString 形状: %v", node["updatedAt"])
			}
			return true
		}
	}
	return false
}
