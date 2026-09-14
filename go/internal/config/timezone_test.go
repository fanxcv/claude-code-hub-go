package config

import (
	"testing"
	"time"
)

// TestTimezoneChainMatchesNodeResolveSystemTimezone 钉住取值链的三态，逐条对应
// resolveSystemTimezone（src/lib/utils/timezone.ts:34-56）。
//
// 为什么值得单钉：库里的 system_settings.timezone 在部署上常为 NULL，此时整条链的结论
// 完全由 env TZ 的默认值决定；缺了默认值就静默落 UTC，表现为「今日」类统计按 UTC 日界
// 落桶（2026-09 管理面对拍发现：同库同刻 providerRankings[0] 的今日请求数 Go 4 / Node 903）。
func TestTimezoneChainMatchesNodeResolveSystemTimezone(t *testing.T) {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("运行时必须能加载 Asia/Shanghai（内嵌 tzdata 兜底），实际报错: %v", err)
	}
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatalf("加载 Asia/Tokyo 失败: %v", err)
	}

	t.Run("库有值则用库值", func(t *testing.T) {
		db := "Asia/Tokyo"
		cfg, loadErr := LoadLookup(func(name string) (string, bool) {
			if name == "TZ" {
				return "UTC", true
			}
			return "", false
		})
		if loadErr != nil {
			t.Fatalf("装载配置失败: %v", loadErr)
		}
		if got := cfg.ResolveLocation(&db); got.String() != tokyo.String() {
			t.Fatalf("库值应胜过 env TZ，期望 %q，实际 %q", tokyo, got)
		}
	})

	t.Run("库为空且TZ为UTC", func(t *testing.T) {
		cfg, loadErr := LoadLookup(func(name string) (string, bool) {
			if name == "TZ" {
				return "UTC", true
			}
			return "", false
		})
		if loadErr != nil {
			t.Fatalf("装载配置失败: %v", loadErr)
		}
		for _, db := range []*string{nil, ptrTo("")} {
			if got := cfg.ResolveLocation(db); got != time.UTC {
				t.Fatalf("库空 + TZ=UTC 应得 UTC，实际 %q", got)
			}
		}
	})

	t.Run("库为空且TZ未设置则取默认Asia/Shanghai", func(t *testing.T) {
		cfg, loadErr := LoadLookup(func(string) (string, bool) { return "", false })
		if loadErr != nil {
			t.Fatalf("装载配置失败: %v", loadErr)
		}
		if cfg.Env.TZ != EnvTimezoneDefault {
			t.Fatalf("TZ 未设置时 Env.TZ 应为默认 %q，实际 %q", EnvTimezoneDefault, cfg.Env.TZ)
		}
		got := cfg.ResolveLocation(nil)
		if got.String() != shanghai.String() {
			t.Fatalf("TZ 未设置应退到默认 %q，实际 %q（缺默认值时这里会得到 UTC）", shanghai, got)
		}
	})

	t.Run("非法候选逐级下退", func(t *testing.T) {
		cfg, loadErr := LoadLookup(func(name string) (string, bool) {
			if name == "TZ" {
				return "Not/AZone", true
			}
			return "", false
		})
		if loadErr != nil {
			t.Fatalf("装载配置失败: %v", loadErr)
		}
		bad := "Also/NotAZone"
		if got := cfg.ResolveLocation(&bad); got != time.UTC {
			t.Fatalf("库值与环境值都非法时应落 UTC，实际 %q", got)
		}
	})

	t.Run("Local不算IANA名", func(t *testing.T) {
		cfg, loadErr := LoadLookup(func(string) (string, bool) { return "", false })
		if loadErr != nil {
			t.Fatalf("装载配置失败: %v", loadErr)
		}
		local := "Local"
		if got := cfg.ResolveLocation(&local); got.String() != shanghai.String() {
			t.Fatalf("Local 应视为非法并退到默认时区，实际 %q", got)
		}
	})
}

// TestTimezoneDefaultShiftsDayBoundary 钉住跨零点口径：同一批账本行在 Asia/Shanghai 与 UTC
// 下落在不同的「今日」，这正是 4 vs 903 那条差异的机制，也是默认值必须与 Node 一致的根因。
func TestTimezoneDefaultShiftsDayBoundary(t *testing.T) {
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("加载 Asia/Shanghai 失败: %v", err)
	}
	// 2026-09-12T17:00:00Z = 上海 2026-09-13 01:00（已过零点）。
	instant := time.Date(2026, 9, 12, 17, 0, 0, 0, time.UTC)

	utcDay := dayKey(instant, time.UTC)
	localDay := dayKey(instant, shanghai)
	if utcDay == localDay {
		t.Fatalf("同一瞬时的 UTC 日与上海日应不同（本例跨零点），实际都是 %q", utcDay)
	}
	if utcDay != "2026-09-12" || localDay != "2026-09-13" {
		t.Fatalf("日界不符：UTC 应为 2026-09-12、上海应为 2026-09-13，实际 %q / %q", utcDay, localDay)
	}
}

// dayKey 求某时刻在指定时区下的「当日」标签。
func dayKey(at time.Time, location *time.Location) string {
	return at.In(location).Format("2006-01-02")
}

// ptrTo 取字符串指针（三态用例里表示「库列存在但为空串」）。
func ptrTo(value string) *string { return &value }
