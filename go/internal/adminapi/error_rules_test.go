package adminapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 error-rules 七条端点的真实 PG 集成测试（lane A1-4）。
//
// 夹具纪律（共享库上踩过的坑）：本测试的所有行都由**本测试自己创建**，pattern 带唯一的
// `go-a14-er-<纳秒>` 前缀，清理时按 pattern 前缀精确删除，且清理用的连接池不会被本测试关闭
// （testPools 注册的 Cleanup 是 LIFO，后注册的本清理先跑）。
//
// 列表与统计断言一律「按自己的前缀过滤」或与直查的数据库结果比对：库里还有别的测试数据，
// 而且 cache:refresh 会同步 35 条默认规则（那是产品数据，本测试不删）。

// errorRulePrefix 生成本次测试唯一的前缀。
func errorRulePrefix(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("go-a14-er-%d", time.Now().UnixNano())
}

// errorRulesRouter 建一个走守卫的路由表。
func errorRulesRouter(t *testing.T, pools *store.Pools) *Router {
	t.Helper()
	deps := Deps{
		Guard:    newTestGuard(t, GuardOptions{}),
		Problems: NewProblems(nil),
		Store:    pools,
	}
	router := New(Options{Deps: deps})
	RegisterErrorRules(router, deps)
	return router
}

// errorRuleRequest 发一次带管理员令牌的请求。
func errorRuleRequest(
	t *testing.T,
	router *Router,
	method, target, body, contentType string,
) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body == "" {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader([]byte(body))
	}
	request := httptest.NewRequest(method, target, reader)
	request.Header.Set("Authorization", "Bearer "+testAdminToken)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// cleanupErrorRules 按前缀清掉本测试留下的行。
func cleanupErrorRules(t *testing.T, pools *store.Pools, prefix string) {
	t.Helper()
	pool, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		"DELETE FROM error_rules WHERE pattern LIKE $1", prefix+"%"); err != nil {
		t.Fatalf("清理错误规则失败: %v", err)
	}
}

// TestErrorRulesCRUDRoundTrip 覆盖 create → list → patch → delete → 404 的完整回路。
func TestErrorRulesCRUDRoundTrip(t *testing.T) {
	pools := testPools(t)
	router := errorRulesRouter(t, pools)
	prefix := errorRulePrefix(t)
	t.Cleanup(func() { cleanupErrorRules(t, pools, prefix) })

	created := errorRuleRequest(t, router, http.MethodPost, "/api/v1/error-rules",
		fmt.Sprintf(`{"pattern":"%s-alpha","category":"invalid_request","matchType":"contains",
			"description":"来自 A1-4 集成测试","overrideStatusCode":418}`, prefix),
		"application/json")
	if created.Code != http.StatusCreated {
		t.Fatalf("创建应 201，实际 %d body=%s", created.Code, created.Body.String())
	}
	var createdBody map[string]any
	if err := json.Unmarshal(created.Body.Bytes(), &createdBody); err != nil {
		t.Fatalf("创建响应解析失败: %v", err)
	}
	if createdBody["pattern"] != prefix+"-alpha" || createdBody["matchType"] != "contains" {
		t.Fatalf("创建响应字段不符: %v", createdBody)
	}
	if createdBody["isDefault"] != false || createdBody["isEnabled"] != true {
		t.Fatalf("默认值不符（isDefault=false / isEnabled=true）: %v", createdBody)
	}
	if createdBody["overrideStatusCode"] != float64(418) {
		t.Fatalf("覆写状态码未回显: %v", createdBody)
	}
	// matchType 为 contains 时 overrideResponse 为 null（未提供）。
	if value, present := createdBody["overrideResponse"]; !present || value != nil {
		t.Fatalf("未提供覆写体时应回显 null，实际 %v", value)
	}
	id := int64(createdBody["id"].(float64))
	location := created.Header().Get("Location")
	if location != fmt.Sprintf("/api/v1/error-rules/%d", id) {
		t.Fatalf("Location 不符: %q", location)
	}

	// list 含新行（按前缀过滤，库里还有别的数据与 35 条默认规则）。
	listed := errorRuleRequest(t, router, http.MethodGet, "/api/v1/error-rules", "", "")
	if listed.Code != http.StatusOK {
		t.Fatalf("列表应 200，实际 %d", listed.Code)
	}
	var listBody struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &listBody); err != nil {
		t.Fatalf("列表响应解析失败: %v", err)
	}
	if !errorRuleListContains(listBody.Items, prefix+"-alpha") {
		t.Fatalf("列表未包含新建的规则")
	}

	// patch：改 matchType 为 exact 并加覆写体。
	patched := errorRuleRequest(t, router, http.MethodPatch,
		fmt.Sprintf("/api/v1/error-rules/%d", id),
		`{"matchType":"exact","overrideResponse":{"type":"error",`+
			`"error":{"type":"invalid_request_error","message":"覆写"}},"priority":7}`,
		"application/json")
	if patched.Code != http.StatusOK {
		t.Fatalf("更新应 200，实际 %d body=%s", patched.Code, patched.Body.String())
	}
	var patchedBody map[string]any
	if err := json.Unmarshal(patched.Body.Bytes(), &patchedBody); err != nil {
		t.Fatalf("更新响应解析失败: %v", err)
	}
	if patchedBody["matchType"] != "exact" || patchedBody["priority"] != float64(7) {
		t.Fatalf("更新未生效: %v", patchedBody)
	}
	override, _ := patchedBody["overrideResponse"].(map[string]any)
	if override["type"] != "error" {
		t.Fatalf("覆写体未落库: %v", patchedBody["overrideResponse"])
	}

	deleted := errorRuleRequest(t, router, http.MethodDelete,
		fmt.Sprintf("/api/v1/error-rules/%d", id), "", "")
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("删除应 204，实际 %d", deleted.Code)
	}
	if deleted.Body.Len() != 0 {
		t.Fatalf("204 不该有正文: %s", deleted.Body.String())
	}

	// 再删一次：action 的「不存在」分支 → 404 error_rule.not_found。
	again := errorRuleRequest(t, router, http.MethodDelete,
		fmt.Sprintf("/api/v1/error-rules/%d", id), "", "")
	if again.Code != http.StatusNotFound {
		t.Fatalf("重复删除应 404，实际 %d body=%s", again.Code, again.Body.String())
	}
	var problem map[string]any
	if err := json.Unmarshal(again.Body.Bytes(), &problem); err != nil {
		t.Fatalf("404 正文解析失败: %v", err)
	}
	if problem["errorCode"] != "error_rule.not_found" || problem["status"] != float64(404) {
		t.Fatalf("404 错误信封不符: %v", problem)
	}
	if contentType := again.Header().Get("Content-Type"); contentType != problemContentType {
		t.Fatalf("错误应答应为 problem+json，实际 %q", contentType)
	}
}

// TestErrorRulesValidationRejects 覆盖 schema 与 action 两层的拒绝。
func TestErrorRulesValidationRejects(t *testing.T) {
	pools := testPools(t)
	router := errorRulesRouter(t, pools)
	prefix := errorRulePrefix(t)
	t.Cleanup(func() { cleanupErrorRules(t, pools, prefix) })

	cases := []struct {
		name   string
		body   string
		status int
		code   string
	}{
		{
			name:   "未知键（strict）",
			body:   fmt.Sprintf(`{"pattern":"%s-x","category":"invalid_request","surprise":1}`, prefix),
			status: http.StatusBadRequest,
			code:   "request.validation_failed",
		},
		{
			name:   "缺 pattern",
			body:   `{"category":"invalid_request"}`,
			status: http.StatusBadRequest,
			code:   "request.validation_failed",
		},
		{
			name:   "category 不在取值域",
			body:   fmt.Sprintf(`{"pattern":"%s-y","category":"nope"}`, prefix),
			status: http.StatusBadRequest,
			code:   "request.validation_failed",
		},
		{
			name:   "覆写状态码越界（schema 的 min/max）",
			body:   fmt.Sprintf(`{"pattern":"%s-z","category":"invalid_request","overrideStatusCode":200}`, prefix),
			status: http.StatusBadRequest,
			code:   "request.validation_failed",
		},
		{
			name:   "regex 无法编译（action 层）",
			body:   fmt.Sprintf(`{"pattern":"%s([","category":"invalid_request","matchType":"regex"}`, prefix),
			status: http.StatusBadRequest,
			code:   "error_rule.action_failed",
		},
		{
			name: "覆写体形状非法（action 层）",
			body: fmt.Sprintf(`{"pattern":"%s-w","category":"invalid_request",
				"overrideResponse":{"hello":"world"}}`, prefix),
			status: http.StatusBadRequest,
			code:   "error_rule.action_failed",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			response := errorRuleRequest(t, router, http.MethodPost, "/api/v1/error-rules",
				testCase.body, "application/json")
			if response.Code != testCase.status {
				t.Fatalf("应 %d，实际 %d body=%s", testCase.status, response.Code, response.Body.String())
			}
			var problem map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
				t.Fatalf("错误正文解析失败: %v", err)
			}
			if problem["errorCode"] != testCase.code {
				t.Fatalf("错误码应为 %s，实际 %v", testCase.code, problem["errorCode"])
			}
			if testCase.code == "request.validation_failed" {
				if _, present := problem["invalidParams"]; !present {
					t.Fatalf("校验失败必须带 invalidParams: %v", problem)
				}
				if problem["title"] != "Validation failed" {
					t.Fatalf("校验失败 title 不符: %v", problem["title"])
				}
			}
		})
	}

	// content-type 不是 JSON：415（Node parseHonoJsonBody 的第一段）。
	unsupported := errorRuleRequest(t, router, http.MethodPost, "/api/v1/error-rules", `{}`, "text/plain")
	if unsupported.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("非 JSON 正文应 415，实际 %d", unsupported.Code)
	}
}

// TestErrorRuleTestEndpoint 覆盖 POST /error-rules:test 的命中与未命中。
func TestErrorRuleTestEndpoint(t *testing.T) {
	pools := testPools(t)
	router := errorRulesRouter(t, pools)
	prefix := errorRulePrefix(t)
	t.Cleanup(func() { cleanupErrorRules(t, pools, prefix) })

	// contains 规则 + 覆写体（含 request_id，运行时会被替换掉）。
	created := errorRuleRequest(t, router, http.MethodPost, "/api/v1/error-rules",
		fmt.Sprintf(`{"pattern":"%s-test","category":"invalid_request","matchType":"contains",
			"overrideStatusCode":429,
			"overrideResponse":{"type":"error","request_id":"upstream-id",
			 "error":{"type":"invalid_request_error","message":"覆写消息"}}}`, prefix),
		"application/json")
	if created.Code != http.StatusCreated {
		t.Fatalf("建夹具失败: %d %s", created.Code, created.Body.String())
	}

	matched := errorRuleRequest(t, router, http.MethodPost, "/api/v1/error-rules:test",
		fmt.Sprintf(`{"message":"boom %s-test boom"}`, prefix), "application/json")
	if matched.Code != http.StatusOK {
		t.Fatalf(":test 应 200，实际 %d body=%s", matched.Code, matched.Body.String())
	}
	var body struct {
		Matched         bool            `json:"matched"`
		Rule            *map[string]any `json:"rule"`
		FinalResponse   map[string]any  `json:"finalResponse"`
		FinalStatusCode *int            `json:"finalStatusCode"`
	}
	if err := json.Unmarshal(matched.Body.Bytes(), &body); err != nil {
		t.Fatalf(":test 响应解析失败: %v", err)
	}
	if !body.Matched || body.Rule == nil {
		t.Fatalf("应命中: %s", matched.Body.String())
	}
	if (*body.Rule)["matchType"] != "contains" || (*body.Rule)["category"] != "invalid_request" {
		t.Fatalf("命中规则摘要不符: %v", *body.Rule)
	}
	if body.FinalStatusCode == nil || *body.FinalStatusCode != 429 {
		t.Fatalf("最终状态码应为 429，实际 %v", body.FinalStatusCode)
	}
	// 覆写体：request_id 被移除（运行时会注入上游的）。
	if _, present := body.FinalResponse["request_id"]; present {
		t.Fatalf("最终响应不该保留 request_id: %v", body.FinalResponse)
	}
	errorObject, _ := body.FinalResponse["error"].(map[string]any)
	if errorObject["message"] != "覆写消息" {
		t.Fatalf("最终响应消息不符: %v", body.FinalResponse)
	}

	unmatched := errorRuleRequest(t, router, http.MethodPost, "/api/v1/error-rules:test",
		`{"message":"与任何规则都无关的消息"}`, "application/json")
	if unmatched.Code != http.StatusOK {
		t.Fatalf(":test 应 200，实际 %d", unmatched.Code)
	}
	var unmatchedBody map[string]any
	if err := json.Unmarshal(unmatched.Body.Bytes(), &unmatchedBody); err != nil {
		t.Fatalf(":test 响应解析失败: %v", err)
	}
	if unmatchedBody["matched"] != false {
		t.Fatalf("不该命中: %v", unmatchedBody)
	}
	if _, present := unmatchedBody["rule"]; present {
		t.Fatalf("未命中时不该有 rule 键: %v", unmatchedBody)
	}
	if value, present := unmatchedBody["finalResponse"]; !present || value != nil {
		t.Fatalf("未命中时 finalResponse 应为 null: %v", unmatchedBody)
	}

	// 空消息被 schema 的 trim().min(1) 挡下。
	empty := errorRuleRequest(t, router, http.MethodPost, "/api/v1/error-rules:test",
		`{"message":"   "}`, "application/json")
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("空消息应 400，实际 %d", empty.Code)
	}
}

// TestErrorRulesCacheEndpoints 覆盖 cache:stats 与 cache:refresh。
func TestErrorRulesCacheEndpoints(t *testing.T) {
	pools := testPools(t)
	router := errorRulesRouter(t, pools)
	prefix := errorRulePrefix(t)
	t.Cleanup(func() { cleanupErrorRules(t, pools, prefix) })

	// 两条启用规则（contains + exact）+ 一条禁用规则：禁用的不该进统计。
	for _, body := range []string{
		fmt.Sprintf(`{"pattern":"%s-c","category":"invalid_request","matchType":"contains"}`, prefix),
		fmt.Sprintf(`{"pattern":"%s-e","category":"invalid_request","matchType":"exact"}`, prefix),
		fmt.Sprintf(`{"pattern":"%s-disabled","category":"invalid_request","matchType":"contains"}`, prefix),
	} {
		response := errorRuleRequest(t, router, http.MethodPost, "/api/v1/error-rules", body,
			"application/json")
		if response.Code != http.StatusCreated {
			t.Fatalf("建夹具失败: %d %s", response.Code, response.Body.String())
		}
	}
	// 禁用那条：isEnabled 不在创建 schema 里，故创建后再 PATCH 关闭。
	var disabledID int64
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}
	if err := pool.QueryRow(context.Background(),
		"SELECT id FROM error_rules WHERE pattern = $1", prefix+"-disabled").Scan(&disabledID); err != nil {
		t.Fatalf("查禁用夹具失败: %v", err)
	}
	disabled := errorRuleRequest(t, router, http.MethodPatch,
		fmt.Sprintf("/api/v1/error-rules/%d", disabledID), `{"isEnabled":false}`, "application/json")
	if disabled.Code != http.StatusOK {
		t.Fatalf("关闭夹具失败: %d %s", disabled.Code, disabled.Body.String())
	}

	stats := errorRuleRequest(t, router, http.MethodGet, "/api/v1/error-rules/cache/stats", "", "")
	if stats.Code != http.StatusOK {
		t.Fatalf("cache/stats 应 200，实际 %d", stats.Code)
	}
	var statsBody map[string]any
	if err := json.Unmarshal(stats.Body.Bytes(), &statsBody); err != nil {
		t.Fatalf("统计解析失败: %v", err)
	}
	for _, key := range []string{"regexCount", "containsCount", "exactCount", "totalCount",
		"lastReloadTime", "isLoading"} {
		if _, present := statsBody[key]; !present {
			t.Fatalf("统计缺字段 %s: %v", key, statsBody)
		}
	}
	if statsBody["totalCount"] != statsBody["regexCount"].(float64)+
		statsBody["containsCount"].(float64)+statsBody["exactCount"].(float64) {
		t.Fatalf("totalCount 应为三项之和: %v", statsBody)
	}
	// 库里的启用行数（独立口径）：Go 侧只统计能编译进三张表的行，故这里是下界。
	var enabledRows int64
	if err := pool.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM error_rules WHERE is_enabled = true").Scan(&enabledRows); err != nil {
		t.Fatalf("统计启用行失败: %v", err)
	}
	if int64(statsBody["totalCount"].(float64)) > enabledRows {
		t.Fatalf("统计数(%v)不该超过库内启用行数(%d)", statsBody["totalCount"], enabledRows)
	}
	if statsBody["containsCount"].(float64) < 1 || statsBody["exactCount"].(float64) < 1 {
		t.Fatalf("本测试的两条启用规则未进统计: %v", statsBody)
	}
	if statsBody["isLoading"] != false {
		t.Fatalf("Go 侧没有异步装载状态: %v", statsBody)
	}

	refreshed := errorRuleRequest(t, router, http.MethodPost, "/api/v1/error-rules/cache:refresh",
		"", "")
	if refreshed.Code != http.StatusOK {
		t.Fatalf("cache:refresh 应 200，实际 %d body=%s", refreshed.Code, refreshed.Body.String())
	}
	var refreshBody struct {
		Stats      map[string]any `json:"stats"`
		SyncResult map[string]any `json:"syncResult"`
	}
	if err := json.Unmarshal(refreshed.Body.Bytes(), &refreshBody); err != nil {
		t.Fatalf("refresh 解析失败: %v", err)
	}
	for _, key := range []string{"inserted", "updated", "skipped", "deleted"} {
		if _, present := refreshBody.SyncResult[key]; !present {
			t.Fatalf("syncResult 缺字段 %s: %v", key, refreshBody.SyncResult)
		}
	}
	// 幂等：同步把 35 条默认规则置为「已存在且是默认规则」，故第二次必然全是 updated。
	if refreshBody.SyncResult["inserted"] != float64(0) || refreshBody.SyncResult["updated"] == float64(0) {
		t.Fatalf("第二次同步应为 0 插入 / 非 0 更新: %v", refreshBody.SyncResult)
	}
	if refreshBody.Stats["lastReloadTime"].(float64) <= 0 {
		t.Fatalf("refresh 应打上刷新时刻: %v", refreshBody.Stats)
	}
	// 用户自定义行（isDefault=false）不该被同步删掉。
	var survivors int64
	if err := pool.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM error_rules WHERE pattern = $1", prefix+"-c").Scan(&survivors); err != nil {
		t.Fatalf("查存续夹具失败: %v", err)
	}
	if survivors != 1 {
		t.Fatalf("自定义规则被同步误删")
	}
}

// TestErrorRuleUpdateConvertsDefault 验证「编辑默认规则即转为自定义规则」。
func TestErrorRuleUpdateConvertsDefault(t *testing.T) {
	pools := testPools(t)
	router := errorRulesRouter(t, pools)
	pool, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	pattern := fmt.Sprintf("go-a14-er-default-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM error_rules WHERE pattern = $1", pattern)
	})
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO error_rules (pattern, match_type, category, description, is_enabled, is_default, priority)
		 VALUES ($1, 'contains', 'invalid_request', '夹具', true, true, 3) RETURNING id`,
		pattern).Scan(&id); err != nil {
		t.Fatalf("建默认规则夹具失败: %v", err)
	}

	response := errorRuleRequest(t, router, http.MethodPatch,
		fmt.Sprintf("/api/v1/error-rules/%d", id), `{"description":"改过的自定义规则"}`, "application/json")
	if response.Code != http.StatusOK {
		t.Fatalf("更新应 200，实际 %d body=%s", response.Code, response.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("更新响应解析失败: %v", err)
	}
	if body["isDefault"] != false {
		t.Fatalf("编辑默认规则应转为自定义（isDefault=false）: %v", body)
	}
	if body["description"] != "改过的自定义规则" {
		t.Fatalf("描述未更新: %v", body)
	}
}

// TestErrorRulesRoutesRegistered 钉住路由表（14 条端点里的 7 条）。
func TestErrorRulesRoutesRegistered(t *testing.T) {
	pools := testPools(t)
	router := errorRulesRouter(t, pools)
	expected := map[string]string{
		"GET /error-rules":                     "listErrorRules",
		"POST /error-rules":                    "createErrorRule",
		"POST /error-rules/cache:refresh":      "refreshErrorRulesCache",
		"GET /error-rules/cache/stats":         "getErrorRulesCacheStats",
		"POST /error-rules:test":               "testErrorRule",
		"PATCH /error-rules/{id:[1-9][0-9]*}":  "updateErrorRule",
		"DELETE /error-rules/{id:[1-9][0-9]*}": "deleteErrorRule",
	}
	routes := router.RouteList()
	if len(routes) != len(expected) {
		t.Fatalf("应注册 %d 条路由，实际 %d: %+v", len(expected), len(routes), routes)
	}
	for _, route := range routes {
		key := route.Method + " " + route.Path
		operationID, ok := expected[key]
		if !ok {
			t.Fatalf("多出路由 %s", key)
		}
		if route.OperationID != operationID {
			t.Fatalf("路由 %s 的 operationId 应为 %s，实际 %s", key, operationID, route.OperationID)
		}
		if route.Access != AccessAdmin {
			t.Fatalf("路由 %s 应为 admin 档位", key)
		}
	}
}

// errorRuleListContains 判断列表里是否有指定 pattern 的行。
func errorRuleListContains(items []map[string]any, pattern string) bool {
	for _, item := range items {
		if value, ok := item["pattern"].(string); ok && value == pattern {
			return true
		}
	}
	return false
}

// TestRulesAndFiltersRequireStore 钉住 fail-closed：没有 Store 时两个模块都不注册路由。
//
// 「注册了但作不了答」比「不注册」危险得多：未注册的路由会原样回退 Node（那里有完整实现），
// 而缺库的处理器会产出 500。
func TestRulesAndFiltersRequireStore(t *testing.T) {
	deps := Deps{Guard: newTestGuard(t, GuardOptions{}), Problems: NewProblems(nil)}

	errorRulesRouter := New(Options{Deps: deps})
	RegisterErrorRules(errorRulesRouter, deps)
	if errorRulesRouter.RouteCount() != 0 {
		t.Fatalf("无 Store 时 error-rules 不该注册路由，实际 %d", errorRulesRouter.RouteCount())
	}

	requestFiltersRouter := New(Options{Deps: deps})
	RegisterRequestFilters(requestFiltersRouter, deps)
	if requestFiltersRouter.RouteCount() != 0 {
		t.Fatalf("无 Store 时 request-filters 不该注册路由，实际 %d", requestFiltersRouter.RouteCount())
	}
}
