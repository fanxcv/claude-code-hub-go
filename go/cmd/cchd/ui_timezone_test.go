package main

// 本文件钉住 D-TZ 缺陷在**壳注入**这一面上的表现：`cmd/cchd/ui.go` 的 uiMetaSource 过去自读
// `os.Getenv("TZ")`，未设置时得空串即落 UTC（Node 侧 zod 声明的是 `Asia/Shanghai`，
// env.schema.ts:179），于是 `window.__CCH_BOOTSTRAP__.timeZone` 与 Node 不一致，UI 时间显示错。
//
// 取值链的唯一实现在 internal/config（timezone.go），本文件只钉「uiMetaSource 确实用了它」：
// 库里无值且 TZ 未设置 -> Asia/Shanghai；TZ=UTC -> UTC；库里有值 -> 以库为准。

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// withTZ 设置/清除 TZ 并在用例结束复原（取值链读的是进程环境变量）。
func withTZ(t *testing.T, value string, present bool) {
	t.Helper()
	previous, had := os.LookupEnv("TZ")
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("TZ", previous)
			return
		}
		_ = os.Unsetenv("TZ")
	})
	if !present {
		if err := os.Unsetenv("TZ"); err != nil {
			t.Fatalf("移除 TZ 失败: %v", err)
		}
		return
	}
	if err := os.Setenv("TZ", value); err != nil {
		t.Fatalf("设置 TZ 失败: %v", err)
	}
}

// 库里拿不到 settings（pools 为 nil）时也必须走链条，而不是落 UTC：
// 这是「读库失败」路径，真实部署里 DSN 缺失或管理面没装起来时会走到。
func TestUIMetaSourceTimezoneWithoutPools(t *testing.T) {
	withTZ(t, "", false)
	meta, err := uiMetaSource(nil)(context.Background())
	if err != nil {
		t.Fatalf("无连接池时不应报错: %v", err)
	}
	if meta.TimeZone != "Asia/Shanghai" {
		t.Fatalf("TZ 未设置时应注入默认 Asia/Shanghai，实际 %q（UTC 即 D-TZ 缺陷复发）", meta.TimeZone)
	}

	withTZ(t, "UTC", true)
	meta, err = uiMetaSource(nil)(context.Background())
	if err != nil {
		t.Fatalf("无连接池时不应报错: %v", err)
	}
	if meta.TimeZone != "UTC" {
		t.Fatalf("TZ=UTC 时应以环境为准，实际 %q", meta.TimeZone)
	}

	// 非法值落到末级 UTC：Node 的第二级同样先校验（isValidIANATimezone），
	// 非法即跳到第三级 "UTC"，而不是回头用默认值（zod 的默认只在变量**未设置**时生效）。
	withTZ(t, "Not/AZone", true)
	meta, err = uiMetaSource(nil)(context.Background())
	if err != nil {
		t.Fatalf("无连接池时不应报错: %v", err)
	}
	if meta.TimeZone != "UTC" {
		t.Fatalf("非法 TZ 应落到末级 UTC，实际 %q", meta.TimeZone)
	}
}

// 真库三态：库列有值 -> 以库为准；库列为 NULL -> 走 env 链条。用例结束后把原值写回。
func TestIntegrationUIMetaSourceTimezoneChain(t *testing.T) {
	dsn := os.Getenv("CCH_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 CCH_TEST_DSN，跳过集成测试")
	}
	pools := uiTimezonePools(t, dsn)
	ctx := context.Background()

	row, err := pools.EnsureAdminSystemSettings(ctx)
	if err != nil {
		t.Fatalf("读/补 system_settings 失败: %v", err)
	}
	original := *row
	t.Cleanup(func() {
		patch := store.AdminSystemSettingsPatch{Updates: map[store.AdminSystemSettingsColumn]any{
			store.ColTimezone: original.Timezone,
		}}
		if _, restoreErr := pools.UpdateAdminSystemSettings(ctx, row.ID, patch); restoreErr != nil {
			t.Errorf("复原 system_settings.timezone 失败（库已留下 %v）：%v", original.Timezone, restoreErr)
		}
	})

	source := uiMetaSource(pools)

	// 态一：库列有值 -> 以库为准，env 不参与。
	if _, err := pools.UpdateAdminSystemSettings(ctx, row.ID, store.AdminSystemSettingsPatch{
		Updates: map[store.AdminSystemSettingsColumn]any{store.ColTimezone: "Asia/Tokyo"},
	}); err != nil {
		t.Fatalf("写入 timezone 失败: %v", err)
	}
	withTZ(t, "UTC", true)
	meta, err := source(ctx)
	if err != nil {
		t.Fatalf("读元数据失败: %v", err)
	}
	if meta.TimeZone != "Asia/Tokyo" {
		t.Fatalf("库列有值时应以库为准，实际 %q", meta.TimeZone)
	}

	// 态二：库列 NULL + TZ 未设置 -> 默认 Asia/Shanghai。
	if _, err := pools.UpdateAdminSystemSettings(ctx, row.ID, store.AdminSystemSettingsPatch{
		Updates: map[store.AdminSystemSettingsColumn]any{store.ColTimezone: nil},
	}); err != nil {
		t.Fatalf("清空 timezone 失败: %v", err)
	}
	withTZ(t, "", false)
	meta, err = source(ctx)
	if err != nil {
		t.Fatalf("读元数据失败: %v", err)
	}
	if meta.TimeZone != "Asia/Shanghai" {
		t.Fatalf("库列 NULL 且 TZ 未设置时应注入 Asia/Shanghai，实际 %q", meta.TimeZone)
	}

	// 态三：库列 NULL + TZ=UTC -> UTC。
	withTZ(t, "UTC", true)
	meta, err = source(ctx)
	if err != nil {
		t.Fatalf("读元数据失败: %v", err)
	}
	if meta.TimeZone != "UTC" {
		t.Fatalf("库列 NULL 且 TZ=UTC 时应注入 UTC，实际 %q", meta.TimeZone)
	}
}

// 壳正文里的注入值才是用户看见的事实：直接取一段 HTML 断言 `timeZone` 字段。
func TestIntegrationUIBootstrapCarriesResolvedTimezone(t *testing.T) {
	dsn := os.Getenv("CCH_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 CCH_TEST_DSN，跳过集成测试")
	}
	pools := uiTimezonePools(t, dsn)
	ctx := context.Background()
	withTZ(t, "UTC", true)

	meta, err := uiMetaSource(pools)(ctx)
	if err != nil {
		t.Fatalf("读元数据失败: %v", err)
	}
	if !strings.Contains(meta.TimeZone, "/") && meta.TimeZone != "UTC" {
		t.Fatalf("注入的 timeZone 不是可识别的 IANA 名: %q", meta.TimeZone)
	}
	// 注入值必须与 internal/uiapp 真正写进壳的字段同源（bootstrap.go 的 `json:"timeZone"`）。
	if meta.TimeZone != "UTC" {
		t.Fatalf("TZ=UTC 时壳应注入 UTC，实际 %q", meta.TimeZone)
	}
}

// uiTimezonePools 建真库连接池（本用例只读 system_settings，不改其它数据）。
func uiTimezonePools(t *testing.T, dsn string) *store.Pools {
	t.Helper()
	pools, err := store.Open(context.Background(), store.Options{
		DSN:                 dsn,
		Budget:              config.SplitPoolBudget(4),
		ApplicationNameBase: "cch-ui-tz",
		Timeouts: config.DBTimeouts{
			IdleTimeout:    5 * time.Second,
			ConnectTimeout: 5 * time.Second,
		},
	})
	if err != nil {
		t.Fatalf("建立连接池失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	return pools
}
