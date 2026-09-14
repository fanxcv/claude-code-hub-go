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

// 本文件是 request-filters 七条端点的真实 PG 集成测试（lane A1-4）。
//
// 夹具纪律：所有行都由本测试自己创建，name / group_tag 带唯一的 `go-a14-rf-<纳秒>` 前缀，
// 清理按前缀精确删除，且清理用的连接池不会被本测试关闭。
//
// providers 夹具用 is_enabled=false：选项类端点的语义是「禁用行也出现、软删行不出现」，用禁用
// 夹具既能覆盖这条语义，又不会干扰数据面的选路（那条路径按启用行与优先级选供应商）。

// requestFilterPrefix 生成本次测试唯一的前缀。
func requestFilterPrefix(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("go-a14-rf-%d", time.Now().UnixNano())
}

// requestFiltersRouter 建一个走守卫的路由表。
func requestFiltersRouter(t *testing.T, pools *store.Pools) *Router {
	t.Helper()
	deps := Deps{
		Guard:    newTestGuard(t, GuardOptions{}),
		Problems: NewProblems(nil),
		Store:    pools,
	}
	router := New(Options{Deps: deps})
	RegisterRequestFilters(router, deps)
	return router
}

// requestFilterRequest 发一次带管理员令牌的请求。
func requestFilterRequest(
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

// cleanupRequestFilters 按前缀清掉本测试留下的行。
func cleanupRequestFilters(t *testing.T, pools *store.Pools, prefix string) {
	t.Helper()
	pool, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		"DELETE FROM request_filters WHERE name LIKE $1", prefix+"%"); err != nil {
		t.Fatalf("清理请求过滤器失败: %v", err)
	}
}

// fixtureProvider 建一个供应商夹具（选项类端点用），返回 id 与分组标签。
func fixtureProvider(t *testing.T, pools *store.Pools, name, groupTag string) int64 {
	t.Helper()
	pool, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	var id int64
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO providers (name, url, key, provider_type, is_enabled, weight, priority,
		 cost_multiplier, group_tag)
		 VALUES ($1, 'http://127.0.0.1:1', 'go-a14-fixture', 'openai-compatible', false, 1, 0, '1.0', $2)
		 RETURNING id`, name, groupTag).Scan(&id); err != nil {
		t.Fatalf("建供应商夹具失败: %v", err)
	}
	t.Cleanup(func() {
		cleanupPool, cleanupErr := pools.Writer()
		if cleanupErr != nil {
			return
		}
		_, _ = cleanupPool.Exec(context.Background(), `DELETE FROM providers WHERE id = $1`, id)
	})
	return id
}

// TestRequestFiltersCRUDRoundTrip 覆盖 create → list → patch → delete → 404 的完整回路。
func TestRequestFiltersCRUDRoundTrip(t *testing.T) {
	pools := testPools(t)
	router := requestFiltersRouter(t, pools)
	prefix := requestFilterPrefix(t)
	t.Cleanup(func() { cleanupRequestFilters(t, pools, prefix) })

	created := requestFilterRequest(t, router, http.MethodPost, "/api/v1/request-filters",
		fmt.Sprintf(`{"name":"%s-alpha","description":"来自 A1-4 集成测试","scope":"header",
			"action":"remove","target":"x-secret","priority":5,"replacement":null,
			"matchType":"contains"}`, prefix),
		"application/json")
	if created.Code != http.StatusCreated {
		t.Fatalf("创建应 201，实际 %d body=%s", created.Code, created.Body.String())
	}
	var createdBody map[string]any
	if err := json.Unmarshal(created.Body.Bytes(), &createdBody); err != nil {
		t.Fatalf("创建响应解析失败: %v", err)
	}
	id := int64(createdBody["id"].(float64))
	if created.Header().Get("Location") != fmt.Sprintf("/api/v1/request-filters/%d", id) {
		t.Fatalf("Location 不符: %q", created.Header().Get("Location"))
	}
	// Node 的默认值：bindingType=global、ruleMode=simple、executionPhase=guard、isEnabled=true。
	for key, expected := range map[string]any{
		"bindingType":    "global",
		"ruleMode":       "simple",
		"executionPhase": "guard",
		"isEnabled":      true,
		"priority":       float64(5),
		"providerIds":    nil,
		"groupTags":      nil,
		"operations":     nil,
		"replacement":    nil,
	} {
		if createdBody[key] != expected {
			t.Fatalf("字段 %s 应为 %v，实际 %v", key, expected, createdBody[key])
		}
	}
	if createdBody["name"] != prefix+"-alpha" {
		t.Fatalf("名称未 trim 或未回显: %v", createdBody["name"])
	}

	listed := requestFilterRequest(t, router, http.MethodGet, "/api/v1/request-filters", "", "")
	if listed.Code != http.StatusOK {
		t.Fatalf("列表应 200，实际 %d", listed.Code)
	}
	var listBody struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(listed.Body.Bytes(), &listBody); err != nil {
		t.Fatalf("列表响应解析失败: %v", err)
	}
	if !requestFilterListContains(listBody.Items, prefix+"-alpha") {
		t.Fatalf("列表未包含新建的过滤器")
	}

	// patch：改成 providers 绑定（同时给 providerIds 与 matchType=null）。
	providerID := fixtureProvider(t, pools, prefix+"-provider", prefix)
	patched := requestFilterRequest(t, router, http.MethodPatch,
		fmt.Sprintf("/api/v1/request-filters/%d", id),
		fmt.Sprintf(`{"name":"%s-beta","bindingType":"providers","providerIds":[%d],
			"isEnabled":false,"target":"x-new"}`, prefix, providerID),
		"application/json")
	if patched.Code != http.StatusOK {
		t.Fatalf("更新应 200，实际 %d body=%s", patched.Code, patched.Body.String())
	}
	var patchedBody map[string]any
	if err := json.Unmarshal(patched.Body.Bytes(), &patchedBody); err != nil {
		t.Fatalf("更新响应解析失败: %v", err)
	}
	if patchedBody["name"] != prefix+"-beta" || patchedBody["bindingType"] != "providers" ||
		patchedBody["target"] != "x-new" || patchedBody["isEnabled"] != false {
		t.Fatalf("更新未生效: %v", patchedBody)
	}
	providerIDs, ok := patchedBody["providerIds"].([]any)
	if !ok || len(providerIDs) != 1 || providerIDs[0] != float64(providerID) {
		t.Fatalf("providerIds 未落库: %v", patchedBody["providerIds"])
	}

	deleted := requestFilterRequest(t, router, http.MethodDelete,
		fmt.Sprintf("/api/v1/request-filters/%d", id), "", "")
	if deleted.Code != http.StatusNoContent {
		t.Fatalf("删除应 204，实际 %d", deleted.Code)
	}
	again := requestFilterRequest(t, router, http.MethodDelete,
		fmt.Sprintf("/api/v1/request-filters/%d", id), "", "")
	if again.Code != http.StatusNotFound {
		t.Fatalf("重复删除应 404，实际 %d body=%s", again.Code, again.Body.String())
	}
	var problem map[string]any
	if err := json.Unmarshal(again.Body.Bytes(), &problem); err != nil {
		t.Fatalf("404 正文解析失败: %v", err)
	}
	if problem["errorCode"] != "request_filter.not_found" {
		t.Fatalf("404 错误码不符: %v", problem)
	}
}

// TestRequestFiltersValidationRejects 覆盖 schema 与 action 两层的拒绝。
func TestRequestFiltersValidationRejects(t *testing.T) {
	pools := testPools(t)
	router := requestFiltersRouter(t, pools)
	prefix := requestFilterPrefix(t)
	t.Cleanup(func() { cleanupRequestFilters(t, pools, prefix) })

	base := fmt.Sprintf(`"name":"%s-v","scope":"header","action":"remove","target":"x"`, prefix)
	cases := []struct {
		name string
		body string
		code string
	}{
		{
			name: "未知键（strict）",
			body: fmt.Sprintf(`{%s,"surprise":1}`, base),
			code: "request.validation_failed",
		},
		{
			name: "scope 不在取值域",
			body: fmt.Sprintf(`{"name":"%s-v","scope":"cookie","action":"remove","target":"x"}`, prefix),
			code: "request.validation_failed",
		},
		{
			name: "priority 不是整数",
			body: fmt.Sprintf(`{%s,"priority":1.5}`, base),
			code: "request.validation_failed",
		},
		{
			name: "target 为空（simple 模式）",
			body: fmt.Sprintf(`{"name":"%s-v","scope":"header","action":"remove","target":"  "}`, prefix),
			code: "request_filter.action_failed",
		},
		{
			name: "providers 绑定但 providerIds 为空数组",
			body: fmt.Sprintf(`{%s,"bindingType":"providers","providerIds":[]}`, base),
			code: "request_filter.action_failed",
		},
		{
			name: "groups 绑定但同时给 providerIds",
			body: fmt.Sprintf(`{%s,"bindingType":"groups","groupTags":["a"],"providerIds":[1]}`, base),
			code: "request_filter.action_failed",
		},
		{
			name: "global 绑定但指定 providerIds",
			body: fmt.Sprintf(`{%s,"providerIds":[1]}`, base),
			code: "request_filter.action_failed",
		},
		{
			name: "高级模式 + guard 阶段",
			body: fmt.Sprintf(`{%s,"ruleMode":"advanced","executionPhase":"guard",
				"operations":[{"op":"set","scope":"body","path":"$.x","value":1}]}`, base),
			code: "request_filter.action_failed",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			response := requestFilterRequest(t, router, http.MethodPost, "/api/v1/request-filters",
				testCase.body, "application/json")
			if response.Code != http.StatusBadRequest {
				t.Fatalf("应 400，实际 %d body=%s", response.Code, response.Body.String())
			}
			var problem map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
				t.Fatalf("错误正文解析失败: %v", err)
			}
			if problem["errorCode"] != testCase.code {
				t.Fatalf("错误码应为 %s，实际 %v", testCase.code, problem["errorCode"])
			}
		})
	}
}

// TestRequestFiltersAdvancedOperations 覆盖高级模式的接受与拒绝。
func TestRequestFiltersAdvancedOperations(t *testing.T) {
	pools := testPools(t)
	router := requestFiltersRouter(t, pools)
	prefix := requestFilterPrefix(t)
	t.Cleanup(func() { cleanupRequestFilters(t, pools, prefix) })

	// 合法：advanced + final + 一条 body 上的 set。
	created := requestFilterRequest(t, router, http.MethodPost, "/api/v1/request-filters",
		fmt.Sprintf(`{"name":"%s-adv","scope":"body","action":"json_path","target":"","ruleMode":"advanced",
			"executionPhase":"final","operations":[{"op":"set","scope":"body","path":"$.model",
			"value":"x"}]}`, prefix),
		"application/json")
	if created.Code != http.StatusCreated {
		t.Fatalf("合法高级模式应 201，实际 %d body=%s", created.Code, created.Body.String())
	}
	var createdBody map[string]any
	if err := json.Unmarshal(created.Body.Bytes(), &createdBody); err != nil {
		t.Fatalf("创建响应解析失败: %v", err)
	}
	operations, ok := createdBody["operations"].([]any)
	if !ok || len(operations) != 1 {
		t.Fatalf("operations 未落库: %v", createdBody["operations"])
	}

	rejects := []struct {
		name       string
		operations string
	}{
		{name: "空数组", operations: `[]`},
		{name: "非法 op", operations: `[{"op":"explode","scope":"body","path":"$.x","value":1}]`},
		{name: "非法 scope", operations: `[{"op":"set","scope":"cookie","path":"$.x","value":1}]`},
		{name: "merge 用在 header 上", operations: `[{"op":"merge","scope":"header","path":"$.x","value":{}}]`},
		{name: "缺 path", operations: `[{"op":"set","scope":"body","value":1}]`},
		{name: "路径含原型污染键", operations: `[{"op":"set","scope":"body","path":"$.__proto__.x","value":1}]`},
		{name: "merge 的 value 不是对象", operations: `[{"op":"merge","scope":"body","path":"$.x","value":[1]}]`},
		{name: "insert 缺 anchor", operations: `[{"op":"insert","scope":"body","path":"$.x","position":"after","value":1}]`},
	}
	for _, testCase := range rejects {
		t.Run(testCase.name, func(t *testing.T) {
			response := requestFilterRequest(t, router, http.MethodPost, "/api/v1/request-filters",
				fmt.Sprintf(`{"name":"%s-adv2","scope":"body","action":"json_path","target":"",
					"ruleMode":"advanced","executionPhase":"final","operations":%s}`,
					prefix, testCase.operations),
				"application/json")
			if response.Code != http.StatusBadRequest {
				t.Fatalf("应 400，实际 %d body=%s", response.Code, response.Body.String())
			}
			var problem map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &problem); err != nil {
				t.Fatalf("错误正文解析失败: %v", err)
			}
			if problem["errorCode"] != "request_filter.action_failed" {
				t.Fatalf("错误码不符: %v", problem["errorCode"])
			}
		})
	}
}

// TestRequestFiltersCacheAndOptions 覆盖 cache:refresh 与两个 options 端点。
func TestRequestFiltersCacheAndOptions(t *testing.T) {
	pools := testPools(t)
	router := requestFiltersRouter(t, pools)
	prefix := requestFilterPrefix(t)
	t.Cleanup(func() { cleanupRequestFilters(t, pools, prefix) })

	// 一条启用 + 一条禁用：count 应等于库里的启用行数（不含禁用行）。
	for _, body := range []string{
		fmt.Sprintf(`{"name":"%s-on","scope":"header","action":"remove","target":"x-a"}`, prefix),
		fmt.Sprintf(`{"name":"%s-off","scope":"header","action":"remove","target":"x-b",
			"isEnabled":false}`, prefix),
	} {
		response := requestFilterRequest(t, router, http.MethodPost, "/api/v1/request-filters", body,
			"application/json")
		if response.Code != http.StatusCreated {
			t.Fatalf("建夹具失败: %d %s", response.Code, response.Body.String())
		}
	}

	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}
	var enabledRows int64
	if err := pool.QueryRow(context.Background(),
		"SELECT COUNT(*) FROM request_filters WHERE is_enabled = true").Scan(&enabledRows); err != nil {
		t.Fatalf("统计启用行失败: %v", err)
	}

	refreshed := requestFilterRequest(t, router, http.MethodPost,
		"/api/v1/request-filters/cache:refresh", "", "")
	if refreshed.Code != http.StatusOK {
		t.Fatalf("cache:refresh 应 200，实际 %d body=%s", refreshed.Code, refreshed.Body.String())
	}
	var refreshBody map[string]any
	if err := json.Unmarshal(refreshed.Body.Bytes(), &refreshBody); err != nil {
		t.Fatalf("refresh 解析失败: %v", err)
	}
	if refreshBody["count"] != float64(enabledRows) {
		t.Fatalf("count 应为启用行数 %d，实际 %v", enabledRows, refreshBody["count"])
	}

	// options/providers：软删行不出现、禁用行出现（库里有 300+ 行，故只按自己的夹具断言）。
	providerID := fixtureProvider(t, pools, prefix+"-opt", prefix)
	providers := requestFilterRequest(t, router, http.MethodGet,
		"/api/v1/request-filters/options/providers", "", "")
	if providers.Code != http.StatusOK {
		t.Fatalf("options/providers 应 200，实际 %d", providers.Code)
	}
	var providerBody struct {
		Items []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"items"`
	}
	if err := json.Unmarshal(providers.Body.Bytes(), &providerBody); err != nil {
		t.Fatalf("options/providers 解析失败: %v", err)
	}
	found := false
	for _, item := range providerBody.Items {
		if item.ID == providerID && item.Name == prefix+"-opt" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("禁用供应商夹具应出现在选项里（Node 只过滤软删行）")
	}

	// options/groups：default 恒在，且展开出自定义分组标签。
	groups := requestFilterRequest(t, router, http.MethodGet,
		"/api/v1/request-filters/options/groups", "", "")
	if groups.Code != http.StatusOK {
		t.Fatalf("options/groups 应 200，实际 %d", groups.Code)
	}
	var groupBody struct {
		Items []string `json:"items"`
	}
	if err := json.Unmarshal(groups.Body.Bytes(), &groupBody); err != nil {
		t.Fatalf("options/groups 解析失败: %v", err)
	}
	if len(groupBody.Items) == 0 || groupBody.Items[0] != "default" {
		t.Fatalf("default 应恒为第一项: %v", groupBody.Items)
	}
	if !containsString(groupBody.Items, prefix) {
		t.Fatalf("夹具分组未展开进选项: %v", groupBody.Items)
	}
}

// TestRequestFiltersRoutesRegistered 钉住路由表（14 条端点里的 7 条）。
func TestRequestFiltersRoutesRegistered(t *testing.T) {
	pools := testPools(t)
	router := requestFiltersRouter(t, pools)
	expected := map[string]string{
		"GET /request-filters":                     "listRequestFilters",
		"POST /request-filters":                    "createRequestFilter",
		"POST /request-filters/cache:refresh":      "refreshRequestFiltersCache",
		"GET /request-filters/options/providers":   "listProviderOptions",
		"GET /request-filters/options/groups":      "listGroupOptions",
		"PATCH /request-filters/{id:[1-9][0-9]*}":  "updateRequestFilter",
		"DELETE /request-filters/{id:[1-9][0-9]*}": "deleteRequestFilter",
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
		if route.OperationID != operationID || route.Access != AccessAdmin {
			t.Fatalf("路由 %s 的 operationId/档位不符: %+v", key, route)
		}
	}
}

// TestRequestFilterUpdateValidationUsesEffectiveValues 验证更新时的「生效值」校验。
//
// 单独一条的理由：这是唯一一处「单看本次入参查不出问题」的地方——先把 action 改成
// text_replace、再单独把 matchType 改成 regex（等价于绕开 ReDoS 检查），Node 侧会按生效值拒。
func TestRequestFilterUpdateValidationUsesEffectiveValues(t *testing.T) {
	pools := testPools(t)
	router := requestFiltersRouter(t, pools)
	prefix := requestFilterPrefix(t)
	t.Cleanup(func() { cleanupRequestFilters(t, pools, prefix) })

	created := requestFilterRequest(t, router, http.MethodPost, "/api/v1/request-filters",
		fmt.Sprintf(`{"name":"%s-eff","scope":"body","action":"text_replace","target":"x"}`, prefix),
		"application/json")
	if created.Code != http.StatusCreated {
		t.Fatalf("建夹具失败: %d %s", created.Code, created.Body.String())
	}
	var createdBody map[string]any
	if err := json.Unmarshal(created.Body.Bytes(), &createdBody); err != nil {
		t.Fatalf("创建响应解析失败: %v", err)
	}
	id := int64(createdBody["id"].(float64))

	// 把 matchType 改成 regex 并把 target 换成一个 RE2 编译不了的样式：生效值校验应拒。
	rejected := requestFilterRequest(t, router, http.MethodPatch,
		fmt.Sprintf("/api/v1/request-filters/%d", id),
		`{"matchType":"regex","target":"(unclosed"}`, "application/json")
	if rejected.Code != http.StatusBadRequest {
		t.Fatalf("生效值校验应拒（400），实际 %d body=%s", rejected.Code, rejected.Body.String())
	}

	// 合法改动仍可通过。
	accepted := requestFilterRequest(t, router, http.MethodPatch,
		fmt.Sprintf("/api/v1/request-filters/%d", id),
		`{"matchType":"regex","target":"^x-[0-9]+$"}`, "application/json")
	if accepted.Code != http.StatusOK {
		t.Fatalf("合法改动应 200，实际 %d body=%s", accepted.Code, accepted.Body.String())
	}
}

// requestFilterListContains 判断列表里是否有指定 name 的行。
func requestFilterListContains(items []map[string]any, name string) bool {
	for _, item := range items {
		if value, ok := item["name"].(string); ok && value == name {
			return true
		}
	}
	return false
}

// containsString 判断字符串切片是否包含目标值。
func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
