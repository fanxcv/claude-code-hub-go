package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// 本文件是 system_settings 读写面的**真库**用例（`CCH_TEST_DSN` 未设置时跳过）。
//
// 共享库纪律：本组用例只动 system_settings 的单行，且**改前先记原值、结束时原样写回**——
// 那个库（cch_loadtest）与其它包/其它进程共用，留下一个被改过的站点标题会影响别人的用例
// 与本地压测的期望值。写入的标记值带唯一后缀，便于事后识别是否残留。

func TestAdminSystemSettingsRoundTrip(t *testing.T) {
	settingsIntegrationLock(t)
	pools := openTestPools(t)
	ctx := context.Background()

	row, err := pools.EnsureAdminSystemSettings(ctx)
	if err != nil {
		t.Fatalf("读/补 system_settings 失败：%v", err)
	}
	original := *row

	marker := "go-store-it-" + time.Now().Format("20060102150405.000000000")
	restore := func() {
		patch := AdminSystemSettingsPatch{Updates: map[AdminSystemSettingsColumn]any{
			ColSiteTitle:             original.SiteTitle,
			ColTimezone:              nullableStringValue(original.Timezone),
			ColReplayCacheTTLMinutes: original.ReplayCacheTTLMinutes,
		}}
		if _, err := pools.UpdateAdminSystemSettings(ctx, row.ID, patch); err != nil {
			t.Fatalf("复原 system_settings 失败（库已留下 %q）：%v", marker, err)
		}
	}
	t.Cleanup(restore)

	timezone := "Asia/Tokyo"
	updated, err := pools.UpdateAdminSystemSettings(ctx, row.ID, AdminSystemSettingsPatch{
		Updates: map[AdminSystemSettingsColumn]any{
			ColSiteTitle:             marker,
			ColTimezone:              timezone,
			ColReplayCacheTTLMinutes: 45,
		},
	})
	if err != nil {
		t.Fatalf("部分更新失败：%v", err)
	}
	if updated.SiteTitle != marker {
		t.Fatalf("site_title 未写入：%q", updated.SiteTitle)
	}
	if updated.Timezone == nil || *updated.Timezone != timezone {
		t.Fatalf("timezone 未写入：%v", updated.Timezone)
	}
	if updated.ReplayCacheTTLMinutes != 45 {
		t.Fatalf("replay_cache_ttl_minutes 未写入：%d", updated.ReplayCacheTTLMinutes)
	}
	if updated.UpdatedAt == nil || !updated.UpdatedAt.After(time.Time{}) {
		t.Fatal("updated_at 应被写 now()")
	}

	// 只改一列时其余列必须原样（部分更新的语义就是「没提的列不动」）。
	onlyTitle, err := pools.UpdateAdminSystemSettings(ctx, row.ID, AdminSystemSettingsPatch{
		Updates: map[AdminSystemSettingsColumn]any{ColSiteTitle: marker + "-2"},
	})
	if err != nil {
		t.Fatalf("单列更新失败：%v", err)
	}
	if onlyTitle.Timezone == nil || *onlyTitle.Timezone != timezone {
		t.Fatalf("未提交的列被改动了：timezone=%v", onlyTitle.Timezone)
	}
	if onlyTitle.ReplayCacheTTLMinutes != 45 {
		t.Fatalf("未提交的列被改动了：replay=%d", onlyTitle.ReplayCacheTTLMinutes)
	}

	// 读回与更新返回必须一致（同一条 SQL 的两种出口）。
	reread, err := pools.FindAdminSystemSettings(ctx)
	if err != nil {
		t.Fatalf("读回失败：%v", err)
	}
	if reread.SiteTitle != onlyTitle.SiteTitle {
		t.Fatalf("读回与更新返回不一致：%q vs %q", reread.SiteTitle, onlyTitle.SiteTitle)
	}
}

// TestAdminSystemSettingsNullWrite 钉住「写 NULL」这条路径：JSON null 必须落成 SQL NULL，
// 而不是 JSON 字面量（后者会让 IS NULL 判空失效，Node 侧同一条纪律见 nullableJSON）。
func TestAdminSystemSettingsNullWrite(t *testing.T) {
	settingsIntegrationLock(t)
	pools := openTestPools(t)
	ctx := context.Background()

	row, err := pools.EnsureAdminSystemSettings(ctx)
	if err != nil {
		t.Fatalf("读 system_settings 失败：%v", err)
	}
	original := *row
	t.Cleanup(func() {
		patch := AdminSystemSettingsPatch{Updates: map[AdminSystemSettingsColumn]any{
			ColTimezone:                  nullableStringValue(original.Timezone),
			ColCacheEffectivenessEnabled: nullableBoolValue(original.CacheEffectivenessEnabled),
			ColIPExtractionConfig:        settingsRawOrNil(original.IPExtractionConfig),
		}}
		if _, err := pools.UpdateAdminSystemSettings(ctx, row.ID, patch); err != nil {
			t.Fatalf("复原失败：%v", err)
		}
	})

	updated, err := pools.UpdateAdminSystemSettings(ctx, row.ID, AdminSystemSettingsPatch{
		Updates: map[AdminSystemSettingsColumn]any{
			ColTimezone:                  nil,
			ColCacheEffectivenessEnabled: nil,
			ColIPExtractionConfig:        json.RawMessage("null"),
		},
	})
	if err != nil {
		t.Fatalf("写 NULL 失败：%v", err)
	}
	if updated.Timezone != nil {
		t.Fatalf("timezone 应为 SQL NULL，得到 %v", *updated.Timezone)
	}
	if updated.CacheEffectivenessEnabled != nil {
		t.Fatalf("cache_effectiveness_enabled 应为 SQL NULL，得到 %v", *updated.CacheEffectivenessEnabled)
	}
	// 关键的判据是 SQL NULL 而不是 'null'::jsonb：row_to_json 对两者的输出都是 null，
	// 故必须直接问库。写错会让 IS NULL 判空失效（Node 的 nullableJSON 就是为这条存在的）。
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制分道失败：%v", err)
	}
	var isNull bool
	if err := pool.QueryRow(context.Background(),
		`SELECT ip_extraction_config IS NULL FROM system_settings WHERE id = $1`,
		updated.ID,
	).Scan(&isNull); err != nil {
		t.Fatalf("查 IS NULL 失败：%v", err)
	}
	if !isNull {
		t.Fatalf("ip_extraction_config 应落 SQL NULL，实际是 %s", updated.IPExtractionConfig)
	}
}

// TestAdminSystemSettingsRejectsUnknownColumn 钉住列名白名单：拼进 SQL 的列名绝不能让调用方自由构造。
func TestAdminSystemSettingsRejectsUnknownColumn(t *testing.T) {
	settingsIntegrationLock(t)
	pools := openTestPools(t)
	row, err := pools.EnsureAdminSystemSettings(context.Background())
	if err != nil {
		t.Fatalf("读 system_settings 失败：%v", err)
	}
	_, err = pools.UpdateAdminSystemSettings(context.Background(), row.ID, AdminSystemSettingsPatch{
		Updates: map[AdminSystemSettingsColumn]any{"site_title; drop table users": "x"},
	})
	if err == nil {
		t.Fatal("非白名单列名必须报错")
	}
}

// nullableStringValue 把可空字符串转成 patch 值（nil 即 SQL NULL）。
func nullableStringValue(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// nullableBoolValue 把可空布尔转成 patch 值。
func nullableBoolValue(value *bool) any {
	if value == nil {
		return nil
	}
	return *value
}

// settingsRawOrNil 把 jsonb 原值转成 patch 值（空即 NULL）。
func settingsRawOrNil(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return raw
}

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
