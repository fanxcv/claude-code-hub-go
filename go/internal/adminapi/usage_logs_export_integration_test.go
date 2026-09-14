package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/xuri/excelize/v2"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是导出面的**真实 PG + Redis** 集成测试：三条路由的端到端、作业状态机、
// 属主隔离、超限失败可见、孤儿清扫。夹具自建自清（复用 usage_logs_integration_test.go 的
// seedUsageLogFixture：模型名带唯一后缀，所有查询按该模型过滤，不干扰并发用例）。

// exportITStubGuard 是可按需指定身份与角色的守卫（属主隔离与非 admin 强制过滤都靠它）。
type exportITStubGuard struct {
	userID  int64
	isAdmin bool
}

func (guard exportITStubGuard) Wrap(_ AccessLevel, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		next.ServeHTTP(writer, request.WithContext(WithPrincipal(request.Context(), Principal{
			UserID: guard.userID, Username: "tester", IsAdmin: guard.isAdmin,
		})))
	})
}

// exportITRouter 建一个装配了导出路由的路由表（真池 + 真 Redis 键值面）。
func exportITRouter(t *testing.T, pools *store.Pools, kv UsageLogsExportKV) *Router {
	t.Helper()
	return exportITRouterAs(t, pools, kv, exportITStubGuard{userID: 1, isAdmin: true})
}

func exportITRouterAs(
	t *testing.T,
	pools *store.Pools,
	kv UsageLogsExportKV,
	guard Guard,
) *Router {
	t.Helper()
	deps := Deps{Guard: guard, Problems: NewProblems(nil), Store: pools, UsageLogsExports: kv}
	router := New(Options{Deps: deps})
	RegisterUsageLogsWith(router, deps, UsageLogsOptions{})
	return router
}

// exportITDo 发一次请求并返回原始响应。
func exportITDo(
	t *testing.T,
	router *Router,
	method, target, body string,
	headers map[string]string,
) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	request := httptest.NewRequest(method, target, reader)
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder
}

// exportITWaitJob 轮询作业状态直到终态（有界：最多 10 秒 / 200 次）。
//
// 有界轮询而不是睡固定时长：导出 2 行的作业通常在几十毫秒内完成，固定睡会把测试拖成
// 「等真实长超时」；上限则保证卡住时测试自己失败而不是挂到底。
func exportITWaitJob(t *testing.T, router *Router, jobID string) map[string]any {
	t.Helper()
	for attempt := 0; attempt < 200; attempt++ {
		recorder := exportITDo(t, router, http.MethodGet, "/api/v1/usage-logs/exports/"+jobID, "", nil)
		if recorder.Code != http.StatusOK {
			t.Fatalf("状态查询失败: %d %s", recorder.Code, recorder.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
			t.Fatalf("状态正文不是 JSON: %s", recorder.Body.String())
		}
		switch body["status"] {
		case string(exportStatusCompleted), string(exportStatusFailed):
			return body
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("作业 %s 未在 10 秒内进入终态", jobID)
	return nil
}

// TestUsageLogsExportSyncCsvRoute 钉住同步分支：200 + {"csv": "BOM 开头 + 表头 + 数据行"}。
func TestUsageLogsExportSyncCsvRoute(t *testing.T) {
	pools := ulOpenPools(t)
	fixture := seedUsageLogFixture(t, pools)
	kv := NewRedisUsageLogsExportKV(providerWriteRedis(t))
	router := exportITRouter(t, pools, kv)

	recorder := exportITDo(t, router, http.MethodPost, "/api/v1/usage-logs/exports",
		`{"model":`+strconv.Quote(fixture.model)+`}`, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("同步导出应 200，实得 %d：%s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		CSV string `json:"csv"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("正文不是 JSON: %s", recorder.Body.String())
	}
	if !strings.HasPrefix(body.CSV, "\ufeff") {
		t.Fatal("CSV 应以 BOM 开头")
	}
	lines := strings.Split(body.CSV, "\n")
	if len(lines) != 3 {
		t.Fatalf("应为 表头 + 2 行数据，实得 %d 行", len(lines))
	}
	if !strings.Contains(lines[0], "Time (") || !strings.Contains(lines[0], "Retry Count") {
		t.Fatalf("表头不符: %s", lines[0])
	}
	if !strings.Contains(body.CSV, fixture.model) {
		t.Fatalf("导出内容缺夹具模型名: %s", body.CSV)
	}
	// 夹具的成本是 0.250000000000000，归一到 0.25（15 位有效数字 + 去尾零）。
	if !strings.Contains(body.CSV, ",0.25,") {
		t.Fatalf("成本列未按 spreadsheet 归一: %s", body.CSV)
	}
}

// TestUsageLogsExportXlsxRequiresAsyncRoute 钉住同步分支拒绝 XLSX（Node 的 400 与错误码）。
func TestUsageLogsExportXlsxRequiresAsyncRoute(t *testing.T) {
	pools := ulOpenPools(t)
	kv := NewRedisUsageLogsExportKV(providerWriteRedis(t))
	router := exportITRouter(t, pools, kv)

	recorder := exportITDo(t, router, http.MethodPost, "/api/v1/usage-logs/exports",
		`{"format":"xlsx"}`, nil)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("同步 XLSX 应 400，实得 %d：%s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("正文不是 JSON: %s", recorder.Body.String())
	}
	if body["errorCode"] != "usage_logs.xlsx_requires_async" {
		t.Fatalf("错误码不符: %v", body["errorCode"])
	}
	if body["detail"] != "xlsx export requires asynchronous processing (set 'Prefer: respond-async')." {
		t.Fatalf("detail 不符: %v", body["detail"])
	}
}

// TestUsageLogsExportAsyncCsvRoundTrip 钉住异步 CSV 的三段链路 + 真 Redis 的键与 TTL。
func TestUsageLogsExportAsyncCsvRoundTrip(t *testing.T) {
	pools := ulOpenPools(t)
	fixture := seedUsageLogFixture(t, pools)
	client := providerWriteRedis(t)
	kv := NewRedisUsageLogsExportKV(client)
	router := exportITRouter(t, pools, kv)

	recorder := exportITDo(t, router, http.MethodPost, "/api/v1/usage-logs/exports",
		`{"model":`+strconv.Quote(fixture.model)+`}`, map[string]string{"Prefer": "respond-async"})
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("异步导出应 202，实得 %d：%s", recorder.Code, recorder.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatalf("正文不是 JSON: %s", recorder.Body.String())
	}
	jobID, _ := created["jobId"].(string)
	if jobID == "" {
		t.Fatalf("缺 jobId: %v", created)
	}
	if created["status"] != string(exportStatusQueued) {
		t.Fatalf("初始状态应为 queued: %v", created["status"])
	}
	statusURL := "/api/v1/usage-logs/exports/" + jobID
	if created["statusUrl"] != statusURL {
		t.Fatalf("statusUrl 不符: %v", created["statusUrl"])
	}
	if recorder.Header().Get("Location") != statusURL {
		t.Fatalf("Location 头不符: %q", recorder.Header().Get("Location"))
	}

	// 真 Redis：状态键必须已经存在，且 TTL 是 15 分钟档（切换期与 Node 互读的契约）。
	statusKey := usageLogsExportStatusKey(jobID)
	if exists := client.Exists(context.Background(), statusKey).Val(); exists != 1 {
		t.Fatalf("状态键未写入: %s", statusKey)
	}
	ttl := client.TTL(context.Background(), statusKey).Val()
	if ttl <= 0 || ttl > 15*time.Minute || ttl < 14*time.Minute {
		t.Fatalf("状态键 TTL 不符: %v", ttl)
	}

	status := exportITWaitJob(t, router, jobID)
	if status["status"] != string(exportStatusCompleted) {
		t.Fatalf("作业未完成: %v", status)
	}
	if status["progressPercent"] != float64(100) {
		t.Fatalf("完成时进度应为 100: %v", status["progressPercent"])
	}
	if status["format"] != "csv" {
		t.Fatalf("格式不符: %v", status["format"])
	}
	if processed, _ := status["processedRows"].(float64); processed != 2 {
		t.Fatalf("已处理行数应为 2: %v", status["processedRows"])
	}

	// 真 Redis：结果键的键名与 Node 一致（prefix + jobId + ":result"）。
	resultKey := usageLogsExportResultKey(jobID)
	if exists := client.Exists(context.Background(), resultKey).Val(); exists != 1 {
		t.Fatalf("结果键未写入: %s", resultKey)
	}
	if ttl := client.TTL(context.Background(), resultKey).Val(); ttl <= 0 || ttl > 15*time.Minute {
		t.Fatalf("结果键 TTL 不符: %v", ttl)
	}

	download := exportITDo(t, router, http.MethodGet, statusURL+"/download", "", nil)
	if download.Code != http.StatusOK {
		t.Fatalf("下载应 200，实得 %d：%s", download.Code, download.Body.String())
	}
	if download.Header().Get("Content-Type") != "text/csv; charset=utf-8" {
		t.Fatalf("CSV 的 Content-Type 不符: %q", download.Header().Get("Content-Type"))
	}
	if download.Header().Get("Content-Disposition") !=
		`attachment; filename="usage-logs-`+jobID+`.csv"` {
		t.Fatalf("Content-Disposition 不符: %q", download.Header().Get("Content-Disposition"))
	}
	if !strings.HasPrefix(download.Body.String(), "\ufeff") {
		t.Fatal("下载内容应以 BOM 开头")
	}
	if !strings.Contains(download.Body.String(), fixture.model) {
		t.Fatal("下载内容缺夹具模型名")
	}
}

// TestUsageLogsExportAsyncXlsxRoundTrip 钉住异步 XLSX：下载是二进制工作簿（读回校验）。
func TestUsageLogsExportAsyncXlsxRoundTrip(t *testing.T) {
	pools := ulOpenPools(t)
	fixture := seedUsageLogFixture(t, pools)
	kv := NewRedisUsageLogsExportKV(providerWriteRedis(t))
	router := exportITRouter(t, pools, kv)

	recorder := exportITDo(t, router, http.MethodPost, "/api/v1/usage-logs/exports",
		`{"format":"xlsx","model":`+strconv.Quote(fixture.model)+`}`,
		map[string]string{"Prefer": "respond-async"})
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("异步 XLSX 应 202，实得 %d：%s", recorder.Code, recorder.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
		t.Fatalf("正文不是 JSON: %s", recorder.Body.String())
	}
	jobID, _ := created["jobId"].(string)
	status := exportITWaitJob(t, router, jobID)
	if status["status"] != string(exportStatusCompleted) {
		t.Fatalf("XLSX 作业未完成: %v", status)
	}

	download := exportITDo(t, router, http.MethodGet,
		"/api/v1/usage-logs/exports/"+jobID+"/download", "", nil)
	if download.Code != http.StatusOK {
		t.Fatalf("下载应 200，实得 %d：%s", download.Code, download.Body.String())
	}
	if download.Header().Get("Content-Type") !=
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" {
		t.Fatalf("XLSX 的 Content-Type 不符: %q", download.Header().Get("Content-Type"))
	}
	if download.Header().Get("Content-Disposition") !=
		`attachment; filename="usage-logs-`+jobID+`.xlsx"` {
		t.Fatalf("Content-Disposition 不符: %q", download.Header().Get("Content-Disposition"))
	}

	file, err := excelize.OpenReader(strings.NewReader(download.Body.String()))
	if err != nil {
		t.Fatalf("下载内容不是可读的 XLSX: %v", err)
	}
	defer func() { _ = file.Close() }()
	sheets := file.GetSheetList()
	if len(sheets) != 2 || sheets[0] != exportXlsxDetailSheet {
		t.Fatalf("表结构不符: %v", sheets)
	}
	rows, err := file.GetRows(exportXlsxDetailSheet)
	if err != nil {
		t.Fatalf("读明细表失败: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("明细表应为 表头 + 2 行，实得 %d", len(rows))
	}
	if !strings.Contains(rows[0][0], "Time (") {
		t.Fatalf("表头缺时区后缀: %v", rows[0][0])
	}
}

// TestUsageLogsExportJobOwnershipAndStates 钉住属主隔离与三个非完成态的作答。
func TestUsageLogsExportJobOwnershipAndStates(t *testing.T) {
	pools := ulOpenPools(t)
	client := providerWriteRedis(t)
	kv := NewRedisUsageLogsExportKV(client)
	router := exportITRouter(t, pools, kv)
	otherRouter := exportITRouterAs(t, pools, kv, exportITStubGuard{userID: 2, isAdmin: true})

	// 直接写一份「失败」作业（不造 20 万行夹具就能覆盖失败可见性）。
	failedID := "33333333-3333-4333-8333-333333333333"
	failed := usageLogsExportJobRecord{
		JobID: failedID, OwnerUserID: 1, Status: exportStatusFailed, Format: "csv",
	}
	message := "usage_logs.export_row_limit_exceeded: 导出上限 200000 行"
	failed.Error = &message
	if !(&usageLogsModule{exports: &usageLogsExportRuntime{kv: kv}, now: time.Now}).
		writeExportJob(context.Background(), failed) {
		t.Fatal("写失败作业失败")
	}
	t.Cleanup(func() { _ = client.Del(context.Background(), usageLogsExportStatusKey(failedID)).Err() })

	recorder := exportITDo(t, router, http.MethodGet, "/api/v1/usage-logs/exports/"+failedID, "", nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("状态查询应 200，实得 %d：%s", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("正文不是 JSON: %s", recorder.Body.String())
	}
	if body["status"] != string(exportStatusFailed) {
		t.Fatalf("状态不符: %v", body["status"])
	}
	if body["error"] != message {
		t.Fatalf("失败原因未透出（过期与失败必须可分辨）: %v", body["error"])
	}

	// 失败作业的下载是 400（不是 404）：请求本身合法，是作业失败了。
	download := exportITDo(t, router, http.MethodGet,
		"/api/v1/usage-logs/exports/"+failedID+"/download", "", nil)
	if download.Code != http.StatusBadRequest {
		t.Fatalf("失败作业下载应 400，实得 %d：%s", download.Code, download.Body.String())
	}

	// 未完成态（running）的下载也是 400。
	runningID := "44444444-4444-4444-8444-444444444444"
	running := usageLogsExportJobRecord{
		JobID: runningID, OwnerUserID: 1, Status: exportStatusRunning, Format: "csv",
	}
	if !(&usageLogsModule{exports: &usageLogsExportRuntime{kv: kv}, now: time.Now}).
		writeExportJob(context.Background(), running) {
		t.Fatal("写运行中作业失败")
	}
	t.Cleanup(func() { _ = client.Del(context.Background(), usageLogsExportStatusKey(runningID)).Err() })
	download = exportITDo(t, router, http.MethodGet,
		"/api/v1/usage-logs/exports/"+runningID+"/download", "", nil)
	if download.Code != http.StatusBadRequest {
		t.Fatalf("未完成作业下载应 400，实得 %d", download.Code)
	}

	// 属主隔离：别人的作业按「不存在或已过期」作答（404），不泄露「存在但不属于你」。
	otherStatus := exportITDo(t, otherRouter, http.MethodGet,
		"/api/v1/usage-logs/exports/"+failedID, "", nil)
	if otherStatus.Code != http.StatusNotFound {
		t.Fatalf("他人作业的状态应 404，实得 %d：%s", otherStatus.Code, otherStatus.Body.String())
	}
	otherDownload := exportITDo(t, otherRouter, http.MethodGet,
		"/api/v1/usage-logs/exports/"+failedID+"/download", "", nil)
	if otherDownload.Code != http.StatusNotFound {
		t.Fatalf("他人作业的下载应 404，实得 %d", otherDownload.Code)
	}

	// 不存在的作业也是 404（与属主不符同形，这正是隔离的用意）。
	missing := exportITDo(t, router, http.MethodGet,
		"/api/v1/usage-logs/exports/55555555-5555-4555-8555-555555555555", "", nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("不存在的作业应 404，实得 %d", missing.Code)
	}
}

// TestUsageLogsExportForcesOwnUserForNonAdmin 钉住非 admin 的筛选改写。
func TestUsageLogsExportForcesOwnUserForNonAdmin(t *testing.T) {
	pools := ulOpenPools(t)
	fixture := seedUsageLogFixture(t, pools)
	kv := NewRedisUsageLogsExportKV(providerWriteRedis(t))
	// 非 admin，且请求体里写了别人的 userId：应当被改写成自己的（夹具行属于 user 1）。
	router := exportITRouterAs(t, pools, kv, exportITStubGuard{userID: 1, isAdmin: false})

	recorder := exportITDo(t, router, http.MethodPost, "/api/v1/usage-logs/exports",
		`{"userId":999999,"model":`+strconv.Quote(fixture.model)+`}`, nil)
	if recorder.Code != http.StatusOK {
		t.Fatalf("同步导出应 200，实得 %d：%s", recorder.Code, recorder.Body.String())
	}
	var body struct {
		CSV string `json:"csv"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("正文不是 JSON: %s", recorder.Body.String())
	}
	if !strings.Contains(body.CSV, fixture.model) {
		t.Fatal("非 admin 的筛选应被改写为自己的 user id（否则会查不到属于自己的行）")
	}
}

// TestSweepUsageLogsExportOrphansRedis 用真 Redis 钉住孤儿清扫。
func TestSweepUsageLogsExportOrphansRedis(t *testing.T) {
	client := providerWriteRedis(t)
	kv := NewRedisUsageLogsExportKV(client)
	module := &usageLogsModule{exports: &usageLogsExportRuntime{kv: kv}, now: time.Now}
	ctx := context.Background()

	orphanID := "66666666-6666-4666-8666-666666666666"
	keepID := "77777777-7777-4777-8777-777777777777"
	orphanKey := usageLogsExportResultKey(orphanID)
	keepKey := usageLogsExportResultKey(keepID)
	t.Cleanup(func() {
		_ = client.Del(ctx, orphanKey, keepKey, usageLogsExportStatusKey(keepID)).Err()
	})
	if err := client.Set(ctx, orphanKey, "csv", time.Minute).Err(); err != nil {
		t.Fatalf("写孤儿结果键失败: %v", err)
	}
	if err := client.Set(ctx, keepKey, "csv", time.Minute).Err(); err != nil {
		t.Fatalf("写正常结果键失败: %v", err)
	}
	if err := client.Set(ctx, usageLogsExportStatusKey(keepID), "{}", time.Minute).Err(); err != nil {
		t.Fatalf("写状态键失败: %v", err)
	}

	deleted, err := module.sweepUsageLogsExportOrphans(ctx)
	if err != nil {
		t.Fatalf("清扫失败: %v", err)
	}
	if deleted < 1 {
		t.Fatalf("应至少删掉 1 个孤儿，实得 %d", deleted)
	}
	if exists := client.Exists(ctx, orphanKey).Val(); exists != 0 {
		t.Fatal("孤儿结果键未被删除")
	}
	if exists := client.Exists(ctx, keepKey).Val(); exists != 1 {
		t.Fatal("有状态键的结果不该被删")
	}
}

// TestUsageLogsExportRouteRegistration 钉住装配缝的开关语义：
// 没给键值面时只注册 7 条读路由（导出三条回退 Node），给了才凑齐 10 条。
func TestUsageLogsExportRouteRegistration(t *testing.T) {
	registered := func(kv UsageLogsExportKV) int {
		deps := Deps{Guard: ulStubGuard{}, Problems: NewProblems(nil), Store: &store.Pools{},
			UsageLogsExports: kv}
		router := New(Options{Deps: deps})
		RegisterUsageLogsWith(router, deps, UsageLogsOptions{})
		return router.RouteCount()
	}

	if got := registered(nil); got != 7 {
		t.Fatalf("未装配键值面应只注册 7 条读路由，实得 %d", got)
	}
	if got := registered(&exportFakeKV{}); got != 10 {
		t.Fatalf("装配键值面后应注册 10 条（含 3 条导出），实得 %d", got)
	}
}

// TestUsageLogsExportUnwiredKVKeepsNodeFallback 钉住「没有键值面就不注册」的 fail-closed。
func TestUsageLogsExportUnwiredKVKeepsNodeFallback(t *testing.T) {
	pools := ulOpenPools(t)
	kv := NewRedisUsageLogsExportKV(nil)
	if kv != nil {
		t.Fatal("nil 客户端必须得到 nil 键值面（装配侧据此不注册）")
	}
	router := exportITRouter(t, pools, kv)

	recorder := exportITDo(t, router, http.MethodPost, "/api/v1/usage-logs/exports", `{}`, nil)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("未注册的路径应由 Router 兜底（503），实得 %d", recorder.Code)
	}
}

// TestUsageLogsExportRedisNilClientIsNil 钉住装配函数的 nil 语义。
func TestUsageLogsExportRedisNilClientIsNil(t *testing.T) {
	var client redis.UniversalClient
	if got := NewRedisUsageLogsExportKV(client); got != nil {
		t.Fatal("nil 客户端应返回 nil")
	}
}
