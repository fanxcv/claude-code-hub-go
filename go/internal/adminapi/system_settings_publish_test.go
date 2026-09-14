package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pubstatus"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 public-status 重发与三族缓存失效的**接线**用例：走真路由表 + 真库（`CCH_TEST_DSN`
// 未设置时跳过），Redis 相关用例另加 `CCH_TEST_REDIS_URL` 门控。
//
// 为什么必须有：这两条接线此前是「如实回答失败码」的降级分支（发布器未移植），
// 接线正确与否只有真跑一次 PUT 才看得出来——桩用例只证明分支按理解跑了。

// fakePublicStatusPublisher 记录调用并回放预设结果。
type fakePublicStatusPublisher struct {
	calls   []string
	result  pubstatus.PublishResult
	err     error
	written bool
}

func (f *fakePublicStatusPublisher) PublishCurrentProjection(
	_ context.Context,
	reason string,
) (pubstatus.PublishResult, error) {
	f.calls = append(f.calls, reason)
	if f.err != nil {
		return pubstatus.PublishResult{}, f.err
	}
	result := f.result
	result.Written = f.written
	return result, nil
}

// systemSettingsPublisherRouter 造真路由表，并挂上发布器与三族缓存清理器。
func systemSettingsPublisherRouter(
	t *testing.T,
	pools *store.Pools,
	publisher PublicStatusPublisher,
	caches DashboardCacheInvalidator,
) *Router {
	t.Helper()
	deps := Deps{
		Logger:                logx.New(nil),
		Guard:                 principalGuard{principal: Principal{UserID: 1, Username: "admin", IsAdmin: true}},
		Store:                 pools,
		PublicStatusPublisher: publisher,
		DashboardCaches:       caches,
	}
	router := New(Options{Deps: deps})
	RegisterSystemSettingsRoutes(router, deps)
	return router
}

// putSettings 发一次 PUT 并解析响应里的警告码。
func putSettings(t *testing.T, router *Router, body string) (*string, string) {
	t.Helper()
	status, responseBody := call(router, http.MethodPut, "/api/v1/system/settings", body)
	if status != http.StatusOK {
		t.Fatalf("PUT 应 200，得到 %d：%s", status, responseBody)
	}
	var response struct {
		SystemSettingsBody
		Warning *string `json:"publicStatusProjectionWarningCode"`
	}
	if err := json.Unmarshal([]byte(responseBody), &response); err != nil {
		t.Fatalf("解析 PUT 响应失败：%v（body=%s）", err, responseBody)
	}
	return response.Warning, responseBody
}

// restoreSiteTitle 记录并复原 site_title（共享库纪律）。
func restoreSiteTitle(t *testing.T, pools *store.Pools) {
	t.Helper()
	ctx := context.Background()
	row, err := pools.EnsureAdminSystemSettings(ctx)
	if err != nil {
		t.Fatalf("读 system_settings 失败：%v", err)
	}
	original := row.SiteTitle
	t.Cleanup(func() {
		if _, err := pools.UpdateAdminSystemSettings(ctx, row.ID, store.AdminSystemSettingsPatch{
			Updates: map[store.AdminSystemSettingsColumn]any{store.ColSiteTitle: original},
		}); err != nil {
			t.Fatalf("复原 site_title 失败：%v", err)
		}
	})
}

func TestSystemSettingsPutRepublishesPublicStatusIntegration(t *testing.T) {
	settingsIntegrationLock(t)
	pools := systemSettingsIntegrationPools(t)
	restoreSiteTitle(t, pools)

	publisher := &fakePublicStatusPublisher{written: true, result: pubstatus.PublishResult{
		ConfigVersion: "cfg-it", Key: "public-status:v2:config:cfg-it", GroupCount: 2,
	}}
	router := systemSettingsPublisherRouter(t, pools, publisher, nil)

	marker := "go-admin-pubstatus-it-" + time.Now().Format("20060102150405.000000000")
	warning, body := putSettings(t, router, `{"siteTitle":"`+marker+`"}`)
	if len(publisher.calls) != 1 {
		t.Fatalf("改了 siteTitle 应重发一次投影，实际调用 %d 次（body=%s）", len(publisher.calls), body)
	}
	if publisher.calls[0] != "save-system-settings" {
		t.Fatalf("重发原因 = %q，want save-system-settings", publisher.calls[0])
	}
	// 发布成功但**聚合侧重发（background rebuild）未移植**，故如实回 PENDING 而不是 nil。
	if warning == nil || *warning != "PUBLIC_STATUS_BACKGROUND_REFRESH_PENDING" {
		t.Fatalf("警告码 = %v，want PUBLIC_STATUS_BACKGROUND_REFRESH_PENDING（body=%s）", warning, body)
	}

	// 与公开状态投影无关的字段不该触发重发。
	warning, body = putSettings(t, router, `{"verboseProviderError":true}`)
	if len(publisher.calls) != 1 {
		t.Fatalf("无关字段不该重发，实际调用 %d 次（body=%s）", len(publisher.calls), body)
	}
	if warning != nil {
		t.Fatalf("无关字段的警告码应为 null，实际 %v", *warning)
	}
}

func TestSystemSettingsPutReportsPublishFailureIntegration(t *testing.T) {
	settingsIntegrationLock(t)
	pools := systemSettingsIntegrationPools(t)
	restoreSiteTitle(t, pools)

	publisher := &fakePublicStatusPublisher{err: errors.New("redis 不可达")}
	router := systemSettingsPublisherRouter(t, pools, publisher, nil)

	marker := "go-admin-pubstatus-fail-" + time.Now().Format("20060102150405.000000000")
	warning, body := putSettings(t, router, `{"siteTitle":"`+marker+`"}`)
	if warning == nil || *warning != "PUBLIC_STATUS_PROJECTION_PUBLISH_FAILED" {
		t.Fatalf("发布失败应回 PROJECTION_PUBLISH_FAILED，实际 %v（body=%s）", warning, body)
	}
}

// TestSystemSettingsPutPublishNotWrittenReportsFailureIntegration 钉住「写库成功但指针没推进
// （written=false）」也按失败作答——否则运维会以为投影已更新。
func TestSystemSettingsPutPublishNotWrittenReportsFailureIntegration(t *testing.T) {
	settingsIntegrationLock(t)
	pools := systemSettingsIntegrationPools(t)
	restoreSiteTitle(t, pools)

	publisher := &fakePublicStatusPublisher{written: false}
	router := systemSettingsPublisherRouter(t, pools, publisher, nil)

	marker := "go-admin-pubstatus-notwritten-" + time.Now().Format("20060102150405.000000000")
	warning, body := putSettings(t, router, `{"siteTitle":"`+marker+`"}`)
	if warning == nil || *warning != "PUBLIC_STATUS_PROJECTION_PUBLISH_FAILED" {
		t.Fatalf("written=false 应回 PROJECT_STATUS 失败码，实际 %v（body=%s）", warning, body)
	}
}

// TestSystemSettingsPutUnwiredPublisherReportsFailureIntegration 无 Redis 命令连接时发布器为
// nil，PUT 必须如实回失败码（而不是静默当作已发布）。
func TestSystemSettingsPutUnwiredPublisherReportsFailureIntegration(t *testing.T) {
	settingsIntegrationLock(t)
	pools := systemSettingsIntegrationPools(t)
	restoreSiteTitle(t, pools)

	router := systemSettingsPublisherRouter(t, pools, nil, nil)

	marker := "go-admin-pubstatus-unwired-" + time.Now().Format("20060102150405.000000000")
	warning, body := putSettings(t, router, `{"siteTitle":"`+marker+`"}`)
	if warning == nil || *warning != publicStatusPublishFailedCode {
		t.Fatalf("未装配发布器应回失败码，实际 %v（body=%s）", warning, body)
	}
}

// TestDashboardCacheInvalidatorClearsThreeFamilies 用真 Redis 钉住三族前缀与「只删这三族」。
func TestDashboardCacheInvalidatorClearsThreeFamilies(t *testing.T) {
	client := testRedis(t)
	ctx := context.Background()
	deps := Deps{Logger: logx.New(nil)}
	invalidator := NewCacheInvalidator(deps, InvalidatorOptions{Redis: client, Logger: logx.New(nil)})

	seed := map[string]string{
		"overview:global":              "1",
		"overview:user:42:tz:UTC":      "1",
		"statistics:7d:1:user:tz:UTC":  "1",
		"leaderboard:7d:USD:UTC":       "1",
		"unrelated:keep-me":            "1",
		"public-status:v2:config:keep": "1",
	}
	for key, value := range seed {
		if err := client.Set(ctx, key, value, 2*time.Minute).Err(); err != nil {
			t.Fatalf("写种子键失败：%v", err)
		}
	}
	t.Cleanup(func() {
		for key := range seed {
			_ = client.Del(ctx, key).Err()
		}
	})

	if err := invalidator.InvalidateDashboardCaches(ctx); err != nil {
		t.Fatalf("清三族缓存失败：%v", err)
	}

	for _, key := range []string{
		"overview:global", "overview:user:42:tz:UTC",
		"statistics:7d:1:user:tz:UTC", "leaderboard:7d:USD:UTC",
	} {
		if exists := client.Exists(ctx, key).Val(); exists != 0 {
			t.Errorf("%s 应被清掉", key)
		}
	}
	for _, key := range []string{"unrelated:keep-me", "public-status:v2:config:keep"} {
		if exists := client.Exists(ctx, key).Val(); exists != 1 {
			t.Errorf("%s 不该被清掉（只删三族）", key)
		}
	}
}
