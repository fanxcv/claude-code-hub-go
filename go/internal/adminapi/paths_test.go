package adminapi

import (
	"net/http"
	"testing"
)

// batchARoute 是管理面批次 A 的一条路由：Go 侧模式 + 一个具体请求路径 + 期望的路径参数。
// 路径取自 §2（72 条）；Go 模式仅在那 5 条「参数+字面后缀」
// 上与 Node 写法不同（`/keys/{keyId:[0-9]+}:enable` ↔ Node 的 `:keyId{[0-9]+:enable}`），
// 其余逐字相同。这份表是 A2 「72 条路由存在性契约测试」的种子。
type batchARoute struct {
	method string
	path   string
	sample string
	params map[string]string
}

var batchARoutes = []batchARoute{
	// usage-logs（10）
	{"GET", "/usage-logs", "/usage-logs", nil},
	{"GET", "/usage-logs/stats", "/usage-logs/stats", nil},
	{"GET", "/usage-logs/filter-options", "/usage-logs/filter-options", nil},
	{"GET", "/usage-logs/models", "/usage-logs/models", nil},
	{"GET", "/usage-logs/status-codes", "/usage-logs/status-codes", nil},
	{"GET", "/usage-logs/endpoints", "/usage-logs/endpoints", nil},
	{"GET", "/usage-logs/session-id-suggestions", "/usage-logs/session-id-suggestions", nil},
	{"POST", "/usage-logs/exports", "/usage-logs/exports", nil},
	{"GET", "/usage-logs/exports/{jobId}", "/usage-logs/exports/job-1", map[string]string{"jobId": "job-1"}},
	{"GET", "/usage-logs/exports/{jobId}/download", "/usage-logs/exports/job-1/download", map[string]string{"jobId": "job-1"}},

	// keys（14）
	{"GET", "/users/{userId}/keys", "/users/7/keys", map[string]string{"userId": "7"}},
	{"POST", "/users/{userId}/keys", "/users/7/keys", map[string]string{"userId": "7"}},
	{"POST", "/users:self/keys", "/users:self/keys", nil},
	{"POST", "/keys/{keyId:[0-9]+}:enable", "/keys/12:enable", map[string]string{"keyId": "12"}},
	{"POST", "/keys/{keyId:[0-9]+}:renew", "/keys/12:renew", map[string]string{"keyId": "12"}},
	{"GET", "/keys/{keyId:[0-9]+}:reveal", "/keys/12:reveal", map[string]string{"keyId": "12"}},
	{"GET", "/keys/{keyId}", "/keys/12", map[string]string{"keyId": "12"}},
	{"PATCH", "/keys/{keyId}", "/keys/12", map[string]string{"keyId": "12"}},
	{"DELETE", "/keys/{keyId}", "/keys/12", map[string]string{"keyId": "12"}},
	{"POST", "/keys/{keyId}/limits:reset", "/keys/12/limits:reset", map[string]string{"keyId": "12"}},
	{"GET", "/keys/{keyId}/limit-usage", "/keys/12/limit-usage", map[string]string{"keyId": "12"}},
	{"GET", "/keys/{keyId}/quota", "/keys/12/quota", map[string]string{"keyId": "12"}},
	{"PATCH", "/keys/{keyId}/limits/{field}", "/keys/12/limits/maxRpm", map[string]string{"keyId": "12", "field": "maxRpm"}},
	{"POST", "/keys:batchUpdate", "/keys:batchUpdate", nil},

	// users（19）
	{"GET", "/users", "/users", nil},
	{"POST", "/users", "/users", nil},
	{"GET", "/users:self", "/users:self", nil},
	{"GET", "/users/tags", "/users/tags", nil},
	{"GET", "/users/key-groups", "/users/key-groups", nil},
	{"GET", "/users:filter-search", "/users:filter-search", nil},
	{"GET", "/users:search", "/users:search", nil},
	{"POST", "/users:usageBatch", "/users:usageBatch", nil},
	{"POST", "/users:batchUpdate", "/users:batchUpdate", nil},
	{"GET", "/users/{id}", "/users/7", map[string]string{"id": "7"}},
	{"PATCH", "/users/{id}", "/users/7", map[string]string{"id": "7"}},
	{"DELETE", "/users/{id}", "/users/7", map[string]string{"id": "7"}},
	{"POST", "/users/{id:[0-9]+}:enable", "/users/7:enable", map[string]string{"id": "7"}},
	{"POST", "/users/{id:[0-9]+}:renew", "/users/7:renew", map[string]string{"id": "7"}},
	{"GET", "/users/{id}/limit-usage", "/users/7/limit-usage", map[string]string{"id": "7"}},
	{"GET", "/users/{id}/limit-usage:all", "/users/7/limit-usage:all", map[string]string{"id": "7"}},
	{"POST", "/users/{id}/limits:reset", "/users/7/limits:reset", map[string]string{"id": "7"}},
	{"POST", "/users/{id}/statistics:reset", "/users/7/statistics:reset", map[string]string{"id": "7"}},
	{"GET", "/users/{id}/statistics-resets/{resetId}", "/users/7/statistics-resets/5", map[string]string{"id": "7", "resetId": "5"}},

	// model-prices（9）
	{"GET", "/model-prices", "/model-prices", nil},
	{"GET", "/model-prices/catalog", "/model-prices/catalog", nil},
	{"GET", "/model-prices/exists", "/model-prices/exists", nil},
	{"POST", "/model-prices:upload", "/model-prices:upload", nil},
	{"POST", "/model-prices:syncLitellmCheck", "/model-prices:syncLitellmCheck", nil},
	{"POST", "/model-prices:syncLitellm", "/model-prices:syncLitellm", nil},
	{"PUT", "/model-prices/{modelName}", "/model-prices/gpt-5.6", map[string]string{"modelName": "gpt-5.6"}},
	{"DELETE", "/model-prices/{modelName}", "/model-prices/gpt-5.6", map[string]string{"modelName": "gpt-5.6"}},
	{"POST", "/model-prices/{modelName}/pricing:pinManual", "/model-prices/gpt-5.6/pricing:pinManual", map[string]string{"modelName": "gpt-5.6"}},

	// error-rules（7）
	{"GET", "/error-rules", "/error-rules", nil},
	{"POST", "/error-rules", "/error-rules", nil},
	{"POST", "/error-rules/cache:refresh", "/error-rules/cache:refresh", nil},
	{"GET", "/error-rules/cache/stats", "/error-rules/cache/stats", nil},
	{"POST", "/error-rules:test", "/error-rules:test", nil},
	{"PATCH", "/error-rules/{id}", "/error-rules/3", map[string]string{"id": "3"}},
	{"DELETE", "/error-rules/{id}", "/error-rules/3", map[string]string{"id": "3"}},

	// request-filters（7）
	{"GET", "/request-filters", "/request-filters", nil},
	{"POST", "/request-filters", "/request-filters", nil},
	{"POST", "/request-filters/cache:refresh", "/request-filters/cache:refresh", nil},
	{"GET", "/request-filters/options/providers", "/request-filters/options/providers", nil},
	{"GET", "/request-filters/options/groups", "/request-filters/options/groups", nil},
	{"PATCH", "/request-filters/{id}", "/request-filters/4", map[string]string{"id": "4"}},
	{"DELETE", "/request-filters/{id}", "/request-filters/4", map[string]string{"id": "4"}},

	// sensitive-words（6）
	{"GET", "/sensitive-words", "/sensitive-words", nil},
	{"POST", "/sensitive-words", "/sensitive-words", nil},
	{"POST", "/sensitive-words/cache:refresh", "/sensitive-words/cache:refresh", nil},
	{"GET", "/sensitive-words/cache/stats", "/sensitive-words/cache/stats", nil},
	{"PATCH", "/sensitive-words/{id}", "/sensitive-words/2", map[string]string{"id": "2"}},
	{"DELETE", "/sensitive-words/{id}", "/sensitive-words/2", map[string]string{"id": "2"}},
}

// TestBatchARouteTableSize 钉住批次 A 的条数：72。
func TestBatchARouteTableSize(t *testing.T) {
	if len(batchARoutes) != 72 {
		t.Fatalf("批次 A 路由表应为 72 条，实际 %d 条", len(batchARoutes))
	}
	seen := map[string]bool{}
	for _, entry := range batchARoutes {
		key := entry.method + " " + entry.path
		if seen[key] {
			t.Errorf("路由表有重复项：%s", key)
		}
		seen[key] = true
	}
}

// TestRouteTableCoversBatchARoutes 断言 72 条模式全部可编译，且每条样例路径命中**自己**
// （不是被某条更宽的模式吞掉）。
func TestRouteTableCoversBatchARoutes(t *testing.T) {
	router := newBatchARouter(t)
	if router.RouteCount() != len(batchARoutes) {
		t.Fatalf("已注册路由应为 %d 条，实际 %d 条", len(batchARoutes), router.RouteCount())
	}

	for _, entry := range batchARoutes {
		index, params := router.lookup(entry.method, entry.sample)
		if index == nil {
			t.Errorf("%s %s 未命中任何路由（样例 %s）", entry.method, entry.path, entry.sample)
			continue
		}
		got := router.routes[*index].route
		if got.Path != entry.path {
			t.Errorf("%s %s 命中了 %s（样例 %s）", entry.method, entry.path, got.Path, entry.sample)
			continue
		}
		if len(params) != len(entry.params) {
			t.Errorf("%s %s 参数应为 %v，实际 %v", entry.method, entry.path, entry.params, params)
			continue
		}
		for name, want := range entry.params {
			if params[name] != want {
				t.Errorf("%s %s 参数 %s 应为 %q，实际 %q", entry.method, entry.path, name, want, params[name])
			}
		}
	}
}

// newBatchARouter 建一个注册了全部 72 条批次 A 路由的路由表。
func newBatchARouter(t *testing.T) *Router {
	t.Helper()
	router := newTestRouter(t)
	for _, entry := range batchARoutes {
		router.Add(Route{
			Method:  entry.method,
			Path:    entry.path,
			Access:  AccessRead,
			Module:  "batch-a",
			Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		})
	}
	return router
}

// TestPathMatcherRejectsNonMatchingPaths 钉住真正的不命中：段数不等、方法不符、未注册子树、
// 未剥前缀，都必须判为未命中（进而回退 Node）。
func TestPathMatcherRejectsNonMatchingPaths(t *testing.T) {
	router := newBatchARouter(t)

	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"后缀后面还有段", "GET", "/users/7/limit-usage:all/extra"},
		{"未注册的方法", "PUT", "/keys/12"},
		{"未注册的子树", "GET", "/providers"},
		{"未注册的一级路径", "GET", "/"},
		{"lookup 不做归一化（归一化在 ServeHTTP）", "GET", "//keys//12"},
		{"未剥挂载前缀的绝对路径", "GET", "/api/v1/keys"},
	}
	for _, testCase := range cases {
		index, _ := router.lookup(testCase.method, testCase.path)
		if index != nil {
			t.Errorf("%s：%s %s 不应命中，实际命中 %s", testCase.name, testCase.method, testCase.path, router.routes[*index].route.Path)
		}
	}
}

// TestUnconstrainedParamAbsorbsSuffixLikeSegment 钉住与 Node 同构的一条容易被误以为「漏匹配」
// 的行为：通用参数段 `{keyId}` 会接住形如 `12:enable` 的段。
//
// Node 侧同样如此（Hono 的 `:keyId` 不限制字符，正则路由只对那 5 条生效），因此 `GET /keys/abc:reveal`
// 在两端都会落到通用处理器，再由处理器校验 id 并作答 400/404。若把这类请求判为「不命中」，
// Go 会回退 Node、Node 又会正常处理——虽然结果一样，但归属抖动会让切换演练的日志难以解读。
func TestUnconstrainedParamAbsorbsSuffixLikeSegment(t *testing.T) {
	router := newBatchARouter(t)

	cases := []struct {
		name   string
		path   string
		want   string
		params map[string]string
	}{
		{"参数正则不满足后缀要求，落到通用参数段", "/keys/abc:reveal", "/keys/{keyId}", map[string]string{"keyId": "abc:reveal"}},
		{"后缀不是已注册的那一个", "/keys/12:enable", "/keys/{keyId}", map[string]string{"keyId": "12:enable"}},
		{"未知字面段由参数段接住", "/users/unknown-fixed-segment", "/users/{id}", map[string]string{"id": "unknown-fixed-segment"}},
		{"已注册的正则路由仍由更高权重胜出", "/keys/12:reveal", "/keys/{keyId:[0-9]+}:reveal", map[string]string{"keyId": "12"}},
	}
	for _, testCase := range cases {
		index, params := router.lookup("GET", testCase.path)
		if index == nil {
			t.Errorf("%s：%s 应命中 %s", testCase.name, testCase.path, testCase.want)
			continue
		}
		if got := router.routes[*index].route.Path; got != testCase.want {
			t.Errorf("%s：%s 应命中 %s，实际 %s", testCase.name, testCase.path, testCase.want, got)
			continue
		}
		for name, want := range testCase.params {
			if params[name] != want {
				t.Errorf("%s：参数 %s 应为 %q，实际 %q", testCase.name, name, want, params[name])
			}
		}
	}
}

// TestLiteralBeatsParam 断言静态段优先于参数段：`/users/tags` 必须命中 tags 而不是 `/users/{id}`。
func TestLiteralBeatsParam(t *testing.T) {
	router := newTestRouter(t)
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	// 刻意先注册参数路由，再注册字面量路由：择路必须靠权重而不是注册序。
	router.Add(Route{Method: "GET", Path: "/users/{id}", Access: AccessRead, Module: "users", Handler: handler})
	router.Add(Route{Method: "GET", Path: "/users/tags", Access: AccessRead, Module: "users", Handler: handler})
	router.Add(Route{Method: "GET", Path: "/users/{id:[0-9]+}:enable", Access: AccessRead, Module: "users", Handler: handler})

	for _, sample := range []struct {
		path string
		want string
	}{
		{"/users/tags", "/users/tags"},
		{"/users/7", "/users/{id}"},
		{"/users/7:enable", "/users/{id:[0-9]+}:enable"},
	} {
		index, params := router.lookup("GET", sample.path)
		if index == nil {
			t.Errorf("%s 未命中", sample.path)
			continue
		}
		if got := router.routes[*index].route.Path; got != sample.want {
			t.Errorf("%s 应命中 %s，实际命中 %s", sample.path, sample.want, got)
		}
		if sample.path == "/users/7" && params["id"] != "7" {
			t.Errorf("/users/7 的 id 参数应为 7，实际 %q", params["id"])
		}
	}
}

// TestCompilePathRejectsInvalidPattern 钉住编译期的严格性：非法模式宁可启动即崩，不得静默丢路由。
func TestCompilePathRejectsInvalidPattern(t *testing.T) {
	invalid := []string{
		"",
		"keys",
		"/keys//12",
		"/keys/{keyId",
		"/keys/{}",
		"/keys/{:}",
		"/keys/{keyId:}+",
		"/keys/{keyId}:",
		"/keys/{keyId}x",
	}
	for _, pattern := range invalid {
		if _, _, err := compilePath(pattern); err == nil {
			t.Errorf("%q 应判为非法模式", pattern)
		}
	}
}
