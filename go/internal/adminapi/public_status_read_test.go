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

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pubstatus"
)

// 本文件钉住两条公开状态读端点的**HTTP 外壳**：
//
//	GET /api/v1/public/status   Node: src/app/api/v1/resources/public/handlers.ts:22
//	GET /api/public-status      Node: src/app/api/public-status/route.ts
//
// 两者业务内核同源，但外壳差异是**契约的一部分**，且差异恰好都在最容易漏的地方：
//
//  1. v1 在 /api/v1 应用壳内 → 带 X-API-Version 信封头；根级不在 → 不带（与 /api/system-settings 同判）；
//  2. v1 的 400 是 RFC7807 problem+json（errorCode=`public_status.invalid_query` + invalidParams）；
//     根级的 400 是 `{error, details}` —— **连 Content-Type 都不同**；
//  3. **只有路由状态 = rebuilding 才 503**；`no_snapshot` 是 200 + 空 groups（前端据此显示
//     「暂无数据」而不是报错）。把 no_snapshot 也做成 503 会让公开页在首次部署时整页报错；
//  4. 503 必须带 `Cache-Control: no-store`，否则中间缓存会把「正在重建」缓存住。
//
// 读路径与查询契约的分支测试在 `internal/pubstatus`（那里可以用内存假 Redis 覆盖十几条降级路径）。

// fakePubStatusStore 是 `pubstatus.PublicStatusStore` 的内存实现。
type fakePubStatusStore struct {
	values map[string]string
	ttls   map[string]time.Duration
	// notReady 模拟「Redis 不可用」：与「可用但没有快照」必须给出不同答复（503 vs 200）。
	notReady bool
}

func newFakePubStatusStore() *fakePubStatusStore {
	return &fakePubStatusStore{values: map[string]string{}, ttls: map[string]time.Duration{}}
}

func (f *fakePubStatusStore) Ready(_ context.Context) bool { return !f.notReady }

func (f *fakePubStatusStore) Get(_ context.Context, key string) (string, bool) {
	value, ok := f.values[key]
	return value, ok
}

func (f *fakePubStatusStore) PTTL(_ context.Context, key string) (time.Duration, error) {
	if _, ok := f.values[key]; !ok {
		return -2 * time.Nanosecond, nil
	}
	if ttl, ok := f.ttls[key]; ok {
		return ttl, nil
	}
	return -1 * time.Nanosecond, nil
}

func (f *fakePubStatusStore) SetEX(_ context.Context, key, value string, ttl time.Duration) error {
	f.values[key] = value
	f.ttls[key] = ttl
	return nil
}

func (f *fakePubStatusStore) SetPX(_ context.Context, key, value string, ttl time.Duration) error {
	f.values[key] = value
	f.ttls[key] = ttl
	return nil
}

func (f *fakePubStatusStore) Set(_ context.Context, key, value string) error {
	f.values[key] = value
	return nil
}

const (
	psFixtureVersion    = "cfg-1"
	psFixtureGeneration = "gen-1"
	psFixtureNowISO     = "2026-09-13T00:10:00.000Z"
	// freshUntil 必须在测试运行时刻之后：用远未来，避免「跑到半点就红」的时间依赖。
	psFixtureFreshUntil = "2099-01-01T00:00:00.000Z"
)

// seedFreshPublicStatus 写一份「可服务的新鲜快照」夹具（配置快照 + manifest + 快照正文）。
func seedFreshPublicStatus(store *fakePubStatusStore) {
	configSnapshot := map[string]any{
		"configVersion":          psFixtureVersion,
		"generatedAt":            psFixtureNowISO,
		"siteTitle":              "  CC Hub  ",
		"siteDescription":        "  desc  ",
		"timeZone":               "Asia/Shanghai",
		"defaultIntervalMinutes": 5,
		"defaultRangeHours":      24,
		"groups":                 []any{map[string]any{"slug": "group-a", "displayName": "Group A"}},
	}
	rawConfig, _ := json.Marshal(configSnapshot)
	store.values[pubstatus.BuildConfigVersionPointerKey()] = psFixtureVersion
	store.values[pubstatus.BuildConfigSnapshotKey(psFixtureVersion)] = string(rawConfig)

	manifest := map[string]any{
		"configVersion":          psFixtureVersion,
		"lastCompleteGeneration": psFixtureGeneration,
		"generatedAt":            psFixtureNowISO,
		"freshUntil":             psFixtureFreshUntil,
		"rebuildState":           "idle",
		"rollupCoverageComplete": true,
	}
	manifestKey, _ := pubstatus.BuildManifestKey(psFixtureVersion, 5, 24, "")
	rawManifest, _ := json.Marshal(manifest)
	store.values[manifestKey] = string(rawManifest)

	snapshot := map[string]any{
		"sourceGeneration": psFixtureGeneration,
		"generatedAt":      psFixtureNowISO,
		"freshUntil":       psFixtureFreshUntil,
		"groups": []any{
			map[string]any{
				"publicGroupSlug": "group-a",
				"displayName":     "Group A",
				"explanatoryCopy": "copy",
				// 内部字段：响应里出现即泄露（防泄露钉子断言）。
				"sourceGroupId":   float64(42),
				"sourceGroupName": "internal-group-a",
				"secret":          "sk-do-not-leak",
				"models": []any{
					map[string]any{
						"publicModelKey":   "deepseek-v4-flash",
						"label":            "DeepSeek V4 Flash",
						"vendorIconKey":    "deepseek",
						"requestTypeBadge": "openaiCompatible",
						"latestState":      "operational",
						"availabilityPct":  float64(99.5),
						"latestTps":        float64(30.5),
						"timeline":         []any{},
						"providerId":       float64(7),
					},
				},
			},
		},
	}
	rawSnapshot, _ := json.Marshal(snapshot)
	snapshotKey, _ := pubstatus.BuildCurrentSnapshotKey(5, 24, psFixtureGeneration, "")
	store.values[snapshotKey] = string(rawSnapshot)
}

func newPublicStatusRouter(t *testing.T, store pubstatus.PublicStatusStore) (*Router, *strings.Builder) {
	t.Helper()
	logs := &strings.Builder{}
	deps := Deps{Guard: &recordingGuard{}, Logger: logx.New(logs)}
	router := New(Options{Deps: deps})
	RegisterPublicStatusReadRoutes(router, deps, PublicStatusReadOptions{Store: store})
	return router, logs
}

func TestPublicStatusReadRoutesAreRegisteredWithNodeShape(t *testing.T) {
	router, _ := newPublicStatusRouter(t, newFakePubStatusStore())

	byPath := map[string]Route{}
	for _, route := range router.RouteList() {
		byPath[route.Method+" "+route.Path] = route
	}

	v1, ok := byPath["GET /public/status"]
	if !ok {
		t.Fatalf("应注册 /public/status（相对挂载前缀）：%v", routeKeysOf(byPath))
	}
	if v1.Access != AccessPublic {
		t.Fatalf("/public/status 的权限档应为 public（Node 的 requireAuth(\"public\")），收到 %q", v1.Access)
	}
	if v1.NoManagementEnvelope {
		t.Fatal("/public/status 在 /api/v1 应用壳内，应带管理面信封")
	}
	if v1.OperationID != "getPublicStatus" {
		t.Fatalf("operationId 应与 Node 的 handler 名一致，收到 %q", v1.OperationID)
	}

	root, ok := byPath["GET /api/public-status"]
	if !ok {
		t.Fatalf("应注册根级 /api/public-status：%v", routeKeysOf(byPath))
	}
	if root.Access != AccessPublic {
		t.Fatalf("根级 /api/public-status 也应是 public 档，收到 %q", root.Access)
	}
	if !root.NoManagementEnvelope {
		t.Fatal("根级路由不在 /api/v1 应用壳内，不该发管理面信封头")
	}
}

func routeKeysOf(routes map[string]Route) []string {
	keys := make([]string, 0, len(routes))
	for key := range routes {
		keys = append(keys, key)
	}
	return keys
}

func TestPublicStatusReadRoutesNotRegisteredWithoutStore(t *testing.T) {
	logs := &strings.Builder{}
	deps := Deps{Guard: &recordingGuard{}, Logger: logx.New(logs)}
	router := New(Options{Deps: deps})
	RegisterPublicStatusReadRoutes(router, deps, PublicStatusReadOptions{})

	if router.RouteCount() != 0 {
		t.Fatalf("无 Redis 门面时不该注册任何路由（应回退 Node），收到 %d 条", router.RouteCount())
	}
	if !strings.Contains(logs.String(), "admin_public_status_read_unwired") {
		t.Fatalf("缺依赖必须留一条可检索的告警：%s", logs.String())
	}
}

func TestV1PublicStatusServesFreshSnapshotWithEnvelope(t *testing.T) {
	store := newFakePubStatusStore()
	seedFreshPublicStatus(store)
	router, _ := newPublicStatusRouter(t, store)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/public/status", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("应 200，收到 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("X-API-Version"); got == "" {
		t.Fatal("/api/v1 内的路由应带管理面信封头 X-API-Version")
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("v1 侧 Content-Type 应逐字取 Node 的 jsonResponse：application/json，收到 %q", got)
	}
	// 不断言 Cache-Control：管理面信封本来就给 /api/v1 下所有响应补
	// `no-store, no-cache, must-revalidate`（Node 同判）。「503 才加 no-store」这条语义由
	// 直接调用处理器的用例（TestPublicStatusRedisUnavailableAnswers503WithNoStore /
	// TestPublicStatusNoSnapshotIsNotAnError）断言——那里绕开了信封。

	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("正文不是合法 JSON: %v", err)
	}
	if body["status"] != "ready" {
		t.Fatalf("新鲜快照的 status 应为 ready，收到 %v", body["status"])
	}
	rebuildState, _ := body["rebuildState"].(map[string]any)
	if rebuildState["state"] != "fresh" || rebuildState["hasSnapshot"] != true || rebuildState["reason"] != nil {
		t.Fatalf("rebuildState 不符：%v", rebuildState)
	}
	if body["generatedAt"] != psFixtureNowISO {
		t.Fatalf("generatedAt 应透传：%v", body["generatedAt"])
	}
	// meta 的三处细节：title/description 已 trim，timeZone 原样（不 trim）。
	meta, _ := body["meta"].(map[string]any)
	if meta["siteTitle"] != "CC Hub" || meta["siteDescription"] != "desc" || meta["timeZone"] != "Asia/Shanghai" {
		t.Fatalf("meta 不符：%v", meta)
	}
	defaults, _ := body["defaults"].(map[string]any)
	if defaults["intervalMinutes"] != float64(5) || defaults["rangeHours"] != float64(24) {
		t.Fatalf("defaults 不符：%v", defaults)
	}
	groups, _ := body["groups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("应有一个分组：%v", body["groups"])
	}
	forbidden := []string{"sourceGroupId", "sourceGroupName", "providerId", "sk-do-not-leak"}
	for _, key := range forbidden {
		if strings.Contains(recorder.Body.String(), key) {
			t.Fatalf("响应正文泄露了内部字段 %q：%s", key, recorder.Body.String())
		}
	}
}

func TestV1PublicStatusValidationFailureIsProblemJSON(t *testing.T) {
	router, _ := newPublicStatusRouter(t, newFakePubStatusStore())

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/public/status?interval=abc", nil))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("应 400，收到 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/problem+json" {
		t.Fatalf("v1 的 400 应是 problem+json，收到 %q", got)
	}

	var body struct {
		Type          string `json:"type"`
		Title         string `json:"title"`
		Status        int    `json:"status"`
		Detail        string `json:"detail"`
		Instance      string `json:"instance"`
		ErrorCode     string `json:"errorCode"`
		InvalidParams []struct {
			Path    []any  `json:"path"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"invalidParams"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("正文不是合法 JSON: %v", err)
	}
	if body.ErrorCode != "public_status.invalid_query" {
		t.Fatalf("errorCode 不符：%q", body.ErrorCode)
	}
	if body.Type != "urn:claude-code-hub:problem:public_status.invalid_query" {
		t.Fatalf("type 应由 errorCode 派生：%q", body.Type)
	}
	if body.Title != "Validation failed" || body.Detail != "One or more query parameters are invalid." {
		t.Fatalf("title/detail 不符：%q / %q", body.Title, body.Detail)
	}
	if body.Instance != "/api/v1/public/status" {
		t.Fatalf("instance 应为请求路径：%q", body.Instance)
	}
	if len(body.InvalidParams) != 1 || body.InvalidParams[0].Code != "invalid_number" ||
		len(body.InvalidParams[0].Path) != 1 || body.InvalidParams[0].Path[0] != "interval" {
		t.Fatalf("invalidParams 不符：%+v", body.InvalidParams)
	}
}

func TestRootPublicStatusUsesItsOwnShell(t *testing.T) {
	store := newFakePubStatusStore()
	seedFreshPublicStatus(store)
	router, _ := newPublicStatusRouter(t, store)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/public-status", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("应 200，收到 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("X-API-Version"); got != "" {
		t.Fatalf("根级路由不在管理面应用壳内，不该发 X-API-Version，收到 %q", got)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("根级走 NextResponse.json，Content-Type 应带 charset，收到 %q", got)
	}
}

func TestRootPublicStatusValidationFailureUsesErrorDetailsShape(t *testing.T) {
	router, _ := newPublicStatusRouter(t, newFakePubStatusStore())

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/public-status?status=nope", nil))

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("应 400，收到 %d", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("根级 400 也是 NextResponse.json，收到 %q", got)
	}
	if got := recorder.Header().Get("Content-Type"); strings.Contains(got, "problem") {
		t.Fatal("根级 400 **不是** problem+json（Node 用的是 {error, details}）")
	}

	var body struct {
		Error   string `json:"error"`
		Details []struct {
			Field   string `json:"field"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"details"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("正文不是合法 JSON: %v（body=%s）", err, recorder.Body.String())
	}
	if body.Error != "Invalid public status query parameters" {
		t.Fatalf("error 文案应与 Node 的 error.message 一致：%q", body.Error)
	}
	if len(body.Details) != 1 || body.Details[0].Code != "invalid_enum" || body.Details[0].Field != "status" {
		t.Fatalf("details 不符：%+v", body.Details)
	}
}

// newPublicStatusAPI 直接造处理器（不经 Router）：Cache-Control 断言必须绕开 Router——
// 管理面信封（含根级路由的 `applyAuthEnvelopeHeaders`）本来就会给所有响应补
// `no-store, no-cache, must-revalidate`，在路由器那一层断言分不出「信封的 no-store」与
// 「503 自己那一条」。
func newPublicStatusAPI(store pubstatus.PublicStatusStore) *publicStatusReadAPI {
	return &publicStatusReadAPI{store: store, logger: logx.New(nil)}
}

// TestPublicStatusRedisUnavailableAnswers503WithNoStore 钉住「Redis 不可用」那一支：
// 服务态 rebuilding + 原因 redis-unavailable → 路由状态 rebuilding → **503** + no-store。
// 这一支必须与下面那条（可用但无数据 → 200）分开验，否则「Redis 挂了」会安静地退化成
// 「暂无数据」：监控看不到、客户端也不会重试。
func TestPublicStatusRedisUnavailableAnswers503WithNoStore(t *testing.T) {
	store := newFakePubStatusStore()
	store.notReady = true
	api := newPublicStatusAPI(store)

	cases := []struct {
		name    string
		handler http.HandlerFunc
		path    string
	}{
		{name: "v1", handler: api.handleV1GetPublicStatus, path: "/api/v1/public/status"},
		{name: "root", handler: api.handleRootGetPublicStatus, path: "/api/public-status"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			testCase.handler(recorder, httptest.NewRequest(http.MethodGet, testCase.path, nil))

			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("Redis 不可用应 503，收到 %d（body=%s）", recorder.Code, recorder.Body.String())
			}
			if got := recorder.Header().Get("Cache-Control"); got != "no-store" {
				t.Fatalf("503 必须带 Cache-Control: no-store，收到 %q", got)
			}
			var body map[string]any
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("503 的正文仍应是状态载荷: %v", err)
			}
			if body["status"] != "rebuilding" {
				t.Fatalf("status 应为 rebuilding，收到 %v", body["status"])
			}
		})
	}
}

// TestPublicStatusNoSnapshotIsNotAnError 钉住一处**容易做反**的语义：Redis 可用但没有任何
// 可用代（manifest 缺失）时，路由状态是 no_snapshot —— HTTP **200** + 空 groups。
// 做成 503 会让首次部署的公开页整页报错，而不是显示一个诚实的空状态；
// 反过来把 redis-unavailable 也做成 200，则 Redis 故障期间没有任何可重试信号。
func TestPublicStatusNoSnapshotIsNotAnError(t *testing.T) {
	// 空库：Ready=true（可用），但没有 manifest、也没有 legacy 代。
	store := newFakePubStatusStore()
	api := newPublicStatusAPI(store)

	recorder := httptest.NewRecorder()
	api.handleRootGetPublicStatus(recorder, httptest.NewRequest(http.MethodGet, "/api/public-status", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("可用但无数据应 200（no_snapshot），收到 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Cache-Control"); got != "" {
		t.Fatalf("no_snapshot 不该带 Cache-Control（它不是错误态），收到 %q", got)
	}

	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("正文不是合法 JSON: %v", err)
	}
	if body["status"] != "no_snapshot" {
		t.Fatalf("status 应为 no_snapshot，收到 %v", body["status"])
	}
	state, _ := body["rebuildState"].(map[string]any)
	if state["state"] != "rebuilding" || state["hasSnapshot"] != false {
		t.Fatalf("rebuildState 不符：%v", state)
	}
	groups, _ := body["groups"].([]any)
	if len(groups) != 0 {
		t.Fatalf("无数据时 groups 应为空数组：%v", body["groups"])
	}
}

// TestPublicStatusRebuildHintIsScheduledOnDegradedRead 钉住「降级读会投重建提示」这条行为：
// 无 manifest 时提示键应被写下（EX 300），否则 rebuild-worker 永远不会知道需要重建。
func TestPublicStatusRebuildHintIsScheduledOnDegradedRead(t *testing.T) {
	store := newFakePubStatusStore()
	router, _ := newPublicStatusRouter(t, store)

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/public/status", nil))

	hintKey, _ := pubstatus.BuildRebuildHintKey(5, 24, "")
	raw, ok := store.values[hintKey]
	if !ok {
		t.Fatalf("降级读应写下重建提示键 %s", hintKey)
	}
	if ttl := store.ttls[hintKey]; ttl != 5*time.Minute {
		t.Fatalf("提示键 TTL 应为 300s，收到 %v", ttl)
	}
	var hint struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(raw), &hint); err != nil {
		t.Fatalf("提示正文不是 JSON: %v", err)
	}
	if hint.Reason != "manifest-missing" {
		t.Fatalf("提示原因应为 manifest-missing，收到 %q", hint.Reason)
	}
}

func TestPublicStatusReadRoutesAllowPublicAccessLevel(t *testing.T) {
	// 两条路由都必须走 public 档：任何一条被写成 read/admin，公开页就会要求登录
	// （症状是状态页在未登录访客那里 401）。
	router, _ := newPublicStatusRouter(t, newFakePubStatusStore())
	for _, route := range router.RouteList() {
		if route.Module != "public-status" {
			continue
		}
		if route.Access != AccessPublic {
			t.Fatalf("%s %s 的权限档应为 public，收到 %q", route.Method, route.Path, route.Access)
		}
	}
}

// mapRouteStatusForTest 已在实现侧收敛为纯函数测试（见 internal/pubstatus 的
// TestMapRouteStatusCoversEveryState）；这里不再重复一份判据镜像。
var _ = fmt.Sprintf
