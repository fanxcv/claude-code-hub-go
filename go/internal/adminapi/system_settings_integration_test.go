package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是三条路由的**端到端**用例：真路由表 + 真库（`CCH_TEST_DSN` 未设置时跳过）。
//
// 为什么不止有桩用例：桩只证明「我的分支按我理解的分支跑了」，证明不了「这一列真的读得出来、
// 写得进去、写回的形状和 Node 一样」。这里的作答都是真实 HTTP 响应正文（等价于 curl 原文）。
//
// 共享库纪律：只动 system_settings 的单行，改前记原值、结束时写回。

// systemSettingsIntegrationPools 建连；门控变量未设置时跳过。
func systemSettingsIntegrationPools(t *testing.T) *store.Pools {
	t.Helper()
	dsn := os.Getenv("CCH_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 CCH_TEST_DSN，跳过数据库集成测试")
	}
	pools, err := store.Open(context.Background(), store.Options{
		DSN:      dsn,
		Budget:   config.SplitPoolBudget(6),
		Timeouts: config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("建立连接池失败：%v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	return pools
}

// principalGuard 复用 keys_test.go 里的同一个替身（把固定身份注入上下文）。
// systemSettingsIntegrationRouter 造真路由表（真 Store + 真注入身份）。
func systemSettingsIntegrationRouter(
	t *testing.T,
	pools *store.Pools,
	principal Principal,
	audit AuditSink,
) *Router {
	t.Helper()
	deps := Deps{
		Logger: logx.New(nil),
		Guard:  principalGuard{principal: principal},
		Store:  pools,
		Audit:  audit,
	}
	router := New(Options{Deps: deps})
	RegisterSystemSettingsRoutes(router, deps)
	RegisterSystemConfigRoutes(router, deps)
	return router
}

// call 发一次请求并把状态码与正文交回。
func call(router *Router, method, path, body string) (int, string) {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	return recorder.Code, recorder.Body.String()
}

func TestSystemSettingsHTTPRoundTripIntegration(t *testing.T) {
	settingsIntegrationLock(t)
	pools := systemSettingsIntegrationPools(t)
	ctx := context.Background()
	row, err := pools.EnsureAdminSystemSettings(ctx)
	if err != nil {
		t.Fatalf("读 system_settings 失败：%v", err)
	}
	originalTitle := row.SiteTitle
	t.Cleanup(func() {
		_, err := pools.UpdateAdminSystemSettings(ctx, row.ID, store.AdminSystemSettingsPatch{
			Updates: map[store.AdminSystemSettingsColumn]any{store.ColSiteTitle: originalTitle},
		})
		if err != nil {
			t.Fatalf("复原 site_title 失败（库留下 %q）：%v", row.SiteTitle, err)
		}
	})

	audit := &recordingAudit{}
	router := systemSettingsIntegrationRouter(t, pools,
		Principal{UserID: 1, Username: "admin", IsAdmin: true}, audit)

	status, body := call(router, http.MethodGet, "/api/v1/system/settings", "")
	if status != http.StatusOK {
		t.Fatalf("GET 应 200，得到 %d：%s", status, body)
	}
	// 键集与 Node 的投影一致：抽查几个「只有管理面才读」的列，它们最容易漏。
	var projection map[string]any
	if err := json.Unmarshal([]byte(body), &projection); err != nil {
		t.Fatalf("解析投影失败：%v", err)
	}
	for _, key := range []string{
		"siteTitle", "enableAutoCleanup", "cleanupRetentionDays", "cleanupSchedule",
		"cleanupBatchSize", "quotaLeasePercent5h", "quotaLeasePercentDaily",
		"quotaLeaseCapUsd", "ipGeoLookupEnabled", "publicStatusWindowHours",
		"publicStatusAggregationIntervalMinutes", "cacheEffectivenessEnabled",
		"createdAt", "updatedAt",
	} {
		if _, ok := projection[key]; !ok {
			t.Errorf("投影缺字段 %s：%s", key, body)
		}
	}
	if projection["siteTitle"] != originalTitle {
		t.Errorf("siteTitle 应与库一致：HTTP=%v DB=%v", projection["siteTitle"], originalTitle)
	}

	// PUT 改标题（部分更新：只有这一列动）。
	marker := "go-admin-it-" + time.Now().Format("20060102150405.000000000")
	status, body = call(router, http.MethodPut, "/api/v1/system/settings",
		`{"siteTitle":"`+marker+`"}`)
	if status != http.StatusOK {
		t.Fatalf("PUT 应 200，得到 %d：%s", status, body)
	}
	var updateResponse struct {
		SystemSettingsBody
		Warning *string `json:"publicStatusProjectionWarningCode"`
	}
	if err := json.Unmarshal([]byte(body), &updateResponse); err != nil {
		t.Fatalf("解析 PUT 响应失败：%v", err)
	}
	if updateResponse.SiteTitle != marker {
		t.Fatalf("PUT 响应未反映新标题：%q（body=%s）", updateResponse.SiteTitle, body)
	}
	if updateResponse.Warning == nil || *updateResponse.Warning != publicStatusPublishFailedCode {
		t.Fatalf("改 siteTitle 时必须带投影告警码 %s，得到 %v", publicStatusPublishFailedCode, updateResponse.Warning)
	}
	reread, err := pools.FindAdminSystemSettings(ctx)
	if err != nil {
		t.Fatalf("读回失败：%v", err)
	}
	if reread.SiteTitle != marker {
		t.Fatalf("库里没有新标题：%q", reread.SiteTitle)
	}
	if reread.UpdatedAt == nil || !reread.UpdatedAt.After(time.Now().Add(-time.Minute)) {
		t.Fatalf("updated_at 未被刷新：%v", reread.UpdatedAt)
	}

	// 竞速窗口违约：用库里的当前值构造一个必然违约的请求。
	violating := `{"racingTotalTimeoutMs":` +
		itoa(row.StickySLAMS+row.MaxDiscoveryRounds*row.DiscoverySLAMS-1) + `}`
	status, body = call(router, http.MethodPut, "/api/v1/system/settings", violating)
	if status != http.StatusBadRequest {
		t.Fatalf("窗口违约应 400，得到 %d：%s", status, body)
	}
	if !strings.Contains(body, discoveryWindowInvalidErrorCode) {
		t.Fatalf("窗口违约正文应带 %s：%s", discoveryWindowInvalidErrorCode, body)
	}
	// 违约请求不得落库。
	again, err := pools.FindAdminSystemSettings(ctx)
	if err != nil {
		t.Fatalf("读回失败：%v", err)
	}
	if again.RacingTotalTimeoutMS != row.RacingTotalTimeoutMS {
		t.Fatalf("违约请求改动了库：%d -> %d", row.RacingTotalTimeoutMS, again.RacingTotalTimeoutMS)
	}

	// 审计：成功一次、失败一次；成功那条带 before/after，失败那条带错误码。
	if len(audit.events) != 2 {
		t.Fatalf("应产生两条审计（成功 + 失败），得到 %d：%+v", len(audit.events), audit.events)
	}
	successEvent := audit.events[0]
	if successEvent.Category != "system_settings" || successEvent.Action != "system_settings.update" ||
		successEvent.TargetType != "system_settings" || successEvent.TargetName != "global" {
		t.Fatalf("成功审计的分类/动作/目标与 Node 不一致：%+v", successEvent)
	}
	if !successEvent.Success || successEvent.Before == nil || successEvent.Details == nil {
		t.Fatalf("成功审计应带 before 与 after：%+v", successEvent)
	}
	if successEvent.Before["siteTitle"] != originalTitle {
		t.Fatalf("before 快照应是改前的标题：%v", successEvent.Before["siteTitle"])
	}
	if successEvent.Principal.Username != "admin" {
		t.Fatalf("审计应记调用方身份：%+v", successEvent.Principal)
	}
	failureEvent := audit.events[1]
	if failureEvent.Success || failureEvent.ErrorMessage != "UPDATE_FAILED" {
		t.Fatalf("失败审计应记 UPDATE_FAILED：%+v", failureEvent)
	}
}

func TestSystemConfigHTTPIntegration(t *testing.T) {
	settingsIntegrationLock(t)
	pools := systemSettingsIntegrationPools(t)
	router := systemSettingsIntegrationRouter(t, pools,
		Principal{UserID: 1, Username: "admin", IsAdmin: true}, nil)

	status, body := call(router, http.MethodGet, "/api/admin/system-config", "")
	if status != http.StatusOK {
		t.Fatalf("GET 应 200，得到 %d：%s", status, body)
	}
	var projection map[string]any
	if err := json.Unmarshal([]byte(body), &projection); err != nil {
		t.Fatalf("解析响应失败：%v（body=%s）", err, body)
	}
	if _, ok := projection["siteTitle"]; !ok {
		t.Fatalf("旧端点返回的应是同一份投影：%s", body)
	}

	// 空体 POST：Node 里等于「什么都不改」（只碰 updatedAt）。
	status, body = call(router, http.MethodPost, "/api/admin/system-config", `{}`)
	if status != http.StatusOK {
		t.Fatalf("空体 POST 应 200，得到 %d：%s", status, body)
	}

	// 非管理员：纯文本 401。
	plainRouter := systemSettingsIntegrationRouter(t, pools, Principal{UserID: 2, Username: "user"}, nil)
	status, body = call(plainRouter, http.MethodGet, "/api/admin/system-config", "")
	if status != http.StatusUnauthorized || body != unauthorizedPlainText {
		t.Fatalf("非管理员应为纯文本 401，得到 %d %q", status, body)
	}
}

// itoa 是 strconv.Itoa 的薄封装：本文件的用例只用一次，不值得让调用点再引一个包名。
func itoa(value int) string { return strconv.Itoa(value) }

// settingsIntegrationLock 用跨进程文件锁把「改共享 system_settings 行」的用例串起来。
//
// 为什么必须跨进程：`go test ./...` 会**并行跑包**，而 internal/store 与 internal/adminapi 的
// 集成用例都要改同一行 system_settings（共享库 cch_loadtest）。不加锁就是两个测试进程互相把
// 对方的期望值改掉——实测出现过一次抖动（同一批用例连着跑七轮只有一轮红）。锁文件放临时目录，
// 用完即释放；拿不到锁时跳过而不是硬闯（并发跑整套门禁是常态）。
func settingsIntegrationLock(t *testing.T) {
	t.Helper()
	path := filepath.Join(os.TempDir(), "cch-system-settings-it.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Skipf("无法建锁文件（%s）：%v", path, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		t.Skipf("加锁失败：%v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	})
}
