package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/ipgeo"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住三条 ip-geo 端点的契约。分两层：
//
//   - **单元层**（不需要库）：开关/形状/lang 语义、根级与 v1 面的形状差异、注册条件；
//   - **集成层**（真库）：me 面的可见性判据——那是唯一有真 SQL 的部分，必须打真库。
//
// 共享的 `system_settings` 行**不改**（会给并发跑的其它用例看到；本仓已有先例
// provider_circuit_admin_test.go 的假设置源），开关读数走可注入面。

// stubIPGeoLookup 是查询缝隙的替身：不打外网，记录收到的 (ip, lang)。
type stubIPGeoLookup struct {
	result   ipgeo.Result
	seenIP   string
	seenLang string
	calls    int
}

func (s *stubIPGeoLookup) LookupIP(_ context.Context, ip, lang string) ipgeo.Result {
	s.calls++
	s.seenIP = ip
	s.seenLang = lang
	return s.result
}

func okResult() ipgeo.Result {
	return ipgeo.Result{Status: ipgeo.StatusOK, Data: json.RawMessage(
		`{"ip":"1.1.1.1","location":{"country":{"code":"AU","name":"Australia",` +
			`"flag":{"emoji":"🇦🇺"}}},"timezone":{"id":"Australia/Sydney"},"connection":{"asn":13335}}`)}
}

// newIPGeoAPI 建一个开关可注入的处理器（不开库）。
func newIPGeoAPI(lookup IPGeoLookup, enabled bool, err error) *ipGeoAPI {
	return &ipGeoAPI{
		pools:    &store.Pools{},
		lookup:   lookup,
		problems: NewProblems(nil),
		logger:   logx.New(nil),
		enabled: func(context.Context) (bool, error) {
			return enabled, err
		},
	}
}

func ipGeoRouter(api *ipGeoAPI) *Router {
	router := New(Options{Deps: Deps{Guard: &recordingGuard{}}})
	api.registerRoutes(router)
	return router
}

func getIPGeo(t *testing.T, router *Router, target string) (int, string, http.Header) {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	return recorder.Code, recorder.Body.String(), recorder.Header()
}

func getIPGeoAs(t *testing.T, router *Router, target string, principal Principal) (int, string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, target, nil)
	request = request.WithContext(WithPrincipal(request.Context(), principal))
	router.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.String()
}

func TestIPGeoRootDisabledIsRawJSON(t *testing.T) {
	router := ipGeoRouter(newIPGeoAPI(&stubIPGeoLookup{result: okResult()}, false, nil))

	status, body, _ := getIPGeo(t, router, "/api/ip-geo/1.1.1.1")
	if status != http.StatusNotFound {
		t.Fatalf("关停时应 404，实得 %d（%s）", status, body)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("根级关停应答应是裸 JSON（Node 手写），实得 %s", body)
	}
	if decoded["error"] != "ip geolocation disabled" {
		t.Fatalf("文案应与 Node 同形，实得 %v", decoded["error"])
	}
}

func TestIPGeoPublicDisabledIsProblemEnvelope(t *testing.T) {
	router := ipGeoRouter(newIPGeoAPI(&stubIPGeoLookup{result: okResult()}, false, nil))

	status, body, _ := getIPGeo(t, router, "/api/v1/ip-geo/1.1.1.1")
	if status != http.StatusNotFound {
		t.Fatalf("关停时应 404，实得 %d（%s）", status, body)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("v1 面关停应答应是问题信封，实得 %s", body)
	}
	if decoded["errorCode"] != "ip_geo.disabled" {
		t.Fatalf("v1 面错误码应为 ip_geo.disabled（与根级裸 JSON 不同形），实得 %v", decoded["errorCode"])
	}
}

func TestIPGeoResultPassthroughAndCacheHeader(t *testing.T) {
	lookup := &stubIPGeoLookup{result: okResult()}
	router := ipGeoRouter(newIPGeoAPI(lookup, true, nil))

	status, body, header := getIPGeo(t, router, "/api/v1/ip-geo/1.1.1.1")
	if status != http.StatusOK {
		t.Fatalf("应 200，实得 %d（%s）", status, body)
	}
	if got := header.Get("Cache-Control"); got != "private, max-age=60" {
		t.Fatalf("缓存头应与 Node 同形，实得 %q", got)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("结果应是 JSON: %s", body)
	}
	// Node 直接 `Response.json(result)`：status/data 两个键原样透传。
	if decoded["status"] != "ok" {
		t.Fatalf("结果 status 应透传，实得 %v", decoded["status"])
	}
	if data, ok := decoded["data"].(map[string]any); !ok || data["ip"] != "1.1.1.1" {
		t.Fatalf("结果 data 应逐字段透传，实得 %v", decoded["data"])
	}
	if lookup.seenIP != "1.1.1.1" || lookup.seenLang != "en" {
		t.Fatalf("未传 lang 时应兜 en（Node 的 ?? 语义），实得 ip=%q lang=%q", lookup.seenIP, lookup.seenLang)
	}
}

func TestIPGeoLangSemanticsDifferBetweenSurfaces(t *testing.T) {
	lookup := &stubIPGeoLookup{result: okResult()}
	router := ipGeoRouter(newIPGeoAPI(lookup, true, nil))

	// 公开面：lang 空串 = 校验失败（schema 是 min(1)）。
	status, body, _ := getIPGeo(t, router, "/api/v1/ip-geo/1.1.1.1?lang=")
	if status != http.StatusBadRequest {
		t.Fatalf("公开面空 lang 应 400，实得 %d（%s）", status, body)
	}
	if lookup.calls != 0 {
		t.Fatalf("校验失败时不该查上游，实得 %d 次", lookup.calls)
	}

	// 自服务面：lang 空串放行（schema 是裸 string），拦不到 400（这里走到「不可见」的 404）。
	status, body = getIPGeoAs(t, router, "/api/v1/me/ip-geo/1.1.1.1?lang=", Principal{UserID: 42})
	if status == http.StatusBadRequest {
		t.Fatalf("自服务面空 lang 不该被拦成 400（Node 是裸 string），实得 %d（%s）", status, body)
	}

	// 根级显式 lang 应原样透传（不透传就会被兜成 en，缓存键随之分叉）。
	status, _, _ = getIPGeo(t, router, "/api/ip-geo/1.1.1.1?lang=ru")
	if status != http.StatusOK {
		t.Fatalf("根级应 200，实得 %d", status)
	}
	if lookup.seenLang != "ru" {
		t.Fatalf("显式 lang 应透传，实得 %q", lookup.seenLang)
	}
}

func TestIPGeoMeWithoutKeyIsNotFound(t *testing.T) {
	// keyID=0（ADMIN_TOKEN 合成身份）：无密钥可查 → 不可见 → 404 NOT_FOUND。
	lookup := &stubIPGeoLookup{result: okResult()}
	router := ipGeoRouter(newIPGeoAPI(lookup, true, nil))

	status, body := getIPGeoAs(t, router, "/api/v1/me/ip-geo/1.1.1.1", Principal{UserID: 42})
	if status != http.StatusNotFound {
		t.Fatalf("无密钥身份应 404，实得 %d（%s）", status, body)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("应是问题信封: %s", body)
	}
	if decoded["errorCode"] != "NOT_FOUND" {
		t.Fatalf("自服务面不可见应 NOT_FOUND（Node 的 errorCode 原样带出），实得 %v", decoded["errorCode"])
	}
	if lookup.calls != 0 {
		t.Fatalf("不可见时不该查上游，实得 %d 次", lookup.calls)
	}
}

func TestRegisterIPGeoRoutesConditionsAndMetadata(t *testing.T) {
	router := New(Options{Deps: Deps{Guard: &recordingGuard{}}})
	RegisterIPGeoRoutes(router, Deps{})
	if count := router.RouteCount(); count != 0 {
		t.Fatalf("缺依赖时不该注册任何路由（整组回退 Node），收到 %d 条", count)
	}

	router = New(Options{Deps: Deps{Guard: &recordingGuard{}}})
	RegisterIPGeoRoutes(router, Deps{Store: &store.Pools{}, IPGeo: &stubIPGeoLookup{}})
	paths := map[string]Route{}
	for _, route := range router.RouteList() {
		paths[route.Method+" "+route.Path] = route
	}
	for _, expected := range []string{
		"GET /api/ip-geo/{ip}",
		"GET /ip-geo/{ip}",
		"GET /me/ip-geo/{ip}",
	} {
		if _, ok := paths[expected]; !ok {
			t.Fatalf("缺少路由 %s，实得 %v", expected, paths)
		}
	}
	if paths["GET /api/ip-geo/{ip}"].Access != AccessAdmin {
		t.Fatal("根级面是管理员档（Node 显式判 role）")
	}
	if !paths["GET /api/ip-geo/{ip}"].NoManagementEnvelope {
		t.Fatal("根级面在 /api/v1 之外，不该发管理面信封头")
	}
	if paths["GET /ip-geo/{ip}"].Access != AccessRead ||
		paths["GET /me/ip-geo/{ip}"].Access != AccessRead {
		t.Fatal("两条 v1 面是 read 档（requireAuth(\"read\")）")
	}
}

// ---- 集成层：me 面的可见性判据（唯一有真 SQL 的部分）----

func TestIPGeoMeVisibilityOnRealDependencies(t *testing.T) {
	pools := meOpenPools(t)
	providerID := fixtureProviderID(t, pools)
	userID := fixtureUser(t, pools, "user", true)
	keyID, keyValue := fixtureKey(t, pools, fixtureKeyOptions{
		userID: userID, canLoginWebUI: true, isEnabled: true,
	})

	visibleIP := fmt.Sprintf("203.0.113.%d", time.Now().UnixNano()%200+1)
	hiddenIP := "198.51.100.77"
	rowID := insertIPGeoLogRow(t, pools, userID, keyValue, providerID, visibleIP)
	t.Cleanup(func() { deleteLedgerRows(t, pools, keyValue, visibleIP) })

	lookup := &stubIPGeoLookup{result: okResult()}
	api := &ipGeoAPI{
		pools:    pools,
		lookup:   lookup,
		problems: NewProblems(nil),
		logger:   logx.New(nil),
		enabled:  func(context.Context) (bool, error) { return true, nil },
	}
	router := ipGeoRouter(api)

	// 1. 该密钥的日志里出现过 → 200 且结果透传。
	status, body := getIPGeoAs(t, router, "/api/v1/me/ip-geo/"+visibleIP,
		Principal{UserID: userID, KeyID: keyID})
	if status != http.StatusOK {
		t.Fatalf("可见 IP 应 200，实得 %d（%s）", status, body)
	}
	if lookup.calls != 1 || lookup.seenIP != visibleIP {
		t.Fatalf("可见时应查一次上游，实得 calls=%d ip=%q", lookup.calls, lookup.seenIP)
	}

	// 2. 没出现过的 IP → 404（Node 的可见性判据）。
	status, body = getIPGeoAs(t, router, "/api/v1/me/ip-geo/"+hiddenIP,
		Principal{UserID: userID, KeyID: keyID})
	if status != http.StatusNotFound {
		t.Fatalf("不可见 IP 应 404，实得 %d（%s）", status, body)
	}
	if lookup.calls != 1 {
		t.Fatalf("不可见时不该查上游，实得 %d 次", lookup.calls)
	}

	// 3. **软删请求行后仍然可见**：Node 的第二段判据正是为这个场景写的——账本行永久保留
	//    client_ip（schema.ts 的注释：「永久保留，避免被清理任务删除」），故清理掉日志行后
	//    该 IP 仍算「本密钥见过」。钉住这条是为了防止将来有人把第二段查询删掉「简化实现」。
	softDeleteIPGeoLogRow(t, pools, rowID)
	status, body = getIPGeoAs(t, router, "/api/v1/me/ip-geo/"+visibleIP,
		Principal{UserID: userID, KeyID: keyID})
	if status != http.StatusOK {
		t.Fatalf("请求行软删后仍应可见（账本保留 client_ip），实得 %d（%s）", status, body)
	}

	// 4. 账本行也清掉后转为不可见。
	deleteLedgerRows(t, pools, keyValue, visibleIP)
	status, body = getIPGeoAs(t, router, "/api/v1/me/ip-geo/"+visibleIP,
		Principal{UserID: userID, KeyID: keyID})
	if status != http.StatusNotFound {
		t.Fatalf("日志与账本都清掉后应 404，实得 %d（%s）", status, body)
	}
}

// insertIPGeoLogRow 插一行带 client_ip 的请求日志（触发器会同步写账本行）。
func insertIPGeoLogRow(
	t *testing.T,
	pools *store.Pools,
	userID int64,
	keyValue string,
	providerID int64,
	clientIP string,
) int64 {
	t.Helper()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}
	var id int64
	err = pool.QueryRow(context.Background(), `
		INSERT INTO message_request (
			provider_id, user_id, key, model, original_model, endpoint,
			status_code, input_tokens, output_tokens, cost_usd, duration_ms, ttfb_ms,
			client_ip, is_replay, created_at
		) VALUES (
			$1, $2, $3, $4, $4, '/v1/messages',
			200, 10, 5, '0.01'::numeric, 8, 4,
			$5, false, now()
		) RETURNING id`,
		providerID, userID, keyValue, fmt.Sprintf("ipgeo-%d", time.Now().UnixNano()),
		clientIP,
	).Scan(&id)
	if err != nil {
		t.Fatalf("插 ip-geo 日志夹具失败: %v", err)
	}
	t.Cleanup(func() {
		cleanupPool, cleanupErr := pools.Control()
		if cleanupErr != nil {
			return
		}
		_, _ = cleanupPool.Exec(context.Background(), `DELETE FROM message_request WHERE id = $1`, id)
	})
	return id
}

func softDeleteIPGeoLogRow(t *testing.T, pools *store.Pools, id int64) {
	t.Helper()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE message_request SET deleted_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("软删日志夹具失败: %v", err)
	}
}

func deleteLedgerRows(t *testing.T, pools *store.Pools, keyValue, clientIP string) {
	t.Helper()
	pool, err := pools.Control()
	if err != nil {
		return
	}
	_, _ = pool.Exec(context.Background(),
		`DELETE FROM usage_ledger WHERE key = $1 AND client_ip = $2`, keyValue, clientIP)
}
