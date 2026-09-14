package store

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// 本文件钉住 2026-09 管理面对拍发现的一处时区缺陷（D-TZ）：
//
// 生产库里 system_settings.timezone 常为 NULL，此时整条取值链的结论完全由 env TZ 的默认值
// 决定。Go 侧过去在六个调用点裸读 os.Getenv("TZ")，未设置时得空串即落 UTC（而非 zod 声明的
// 默认 Asia/Shanghai），于是同库同刻的「今日」统计按 UTC 日界落桶——对拍实测
// providerRankings[0] 的今日请求数 Go 4 / Node 903。
//
// 两条钉子的分工：第一条钉「解析结论」，第二条钉「结论真的改变日界」。

// TestIntegrationAdminSystemTimezoneOrDefaultsToShanghai 钉住库列 NULL 时的解析结论。
//
// 前提：夹具不写 system_settings.timezone（真实部署多为 NULL）。若库里恰好有值，用例跳过
// 而不是把库值改成 NULL——改全局配置不是用例该做的事。
func TestIntegrationAdminSystemTimezoneOrDefaultsToShanghai(t *testing.T) {
	pools := statisticsOpenPools(t)
	ctx := context.Background()

	raw, err := pools.AdminSystemTimezone(ctx)
	if err != nil {
		t.Fatalf("读系统时区失败: %v", err)
	}
	if raw != nil && *raw != "" {
		t.Skipf("库里的 system_settings.timezone 已有值 %q，本用例只覆盖 NULL 场景", *raw)
	}

	// TZ 未设置时应当取 zod 的默认值，而不是 UTC。
	previous, hadTZ := os.LookupEnv("TZ")
	if err := os.Unsetenv("TZ"); err != nil {
		t.Fatalf("移除 TZ 失败: %v", err)
	}
	t.Cleanup(func() {
		if hadTZ {
			_ = os.Setenv("TZ", previous)
		}
	})

	if got := pools.AdminSystemTimezoneOrUTC(ctx); got != "Asia/Shanghai" {
		t.Fatalf("库列 NULL 且 TZ 未设置时应退到默认 Asia/Shanghai，实际 %q（退化为 UTC 即 D-TZ 缺陷复发）", got)
	}

	// TZ 显式设置时以环境为准（与 zod 的「已设置则用原值」一致）。
	if err := os.Setenv("TZ", "UTC"); err != nil {
		t.Fatalf("设置 TZ 失败: %v", err)
	}
	if got := pools.AdminSystemTimezoneOrUTC(ctx); got != "UTC" {
		t.Fatalf("TZ=UTC 时应以环境为准，实际 %q", got)
	}
}

// TestIntegrationStatisticsTodayWindowShiftsWithTimezone 钉住结论的后果：同一个「今日」窗口
// 在两个时区下差一个日界偏移，这正是 4 vs 903 的机制。
//
// 表达式取自**产品自己**（adminStatisticsWindow），不再手抄 SQL——手抄会在产品改表达式时静默漂移。
//
// 为何不能直接断言「恒为 8 小时」：`DATE_TRUNC('day', now AT TIME ZONE tz)` 的结论是**当地日历日**，
// 而上海与 UTC 的日历日在 CST 00:00–08:00 期间并不同日（上海已滚到次日、UTC 仍是前一日），
// 此时两起点的差值变成 **-16h**。差值只能是 +8h 或 -16h，两者相差整 24h——所以时不变的性质是
// 「差值 ≡ 8h (mod 24h)」且「两侧各自是当地零点」；而把时刻钉死的精确断言放在下面第二段。
func TestIntegrationStatisticsTodayWindowShiftsWithTimezone(t *testing.T) {
	pools := statisticsOpenPools(t)
	ctx := context.Background()

	pool, err := pools.Data()
	if err != nil {
		t.Fatalf("取数据分道失败: %v", err)
	}

	window, err := adminStatisticsWindow(AdminStatisticsToday)
	if err != nil {
		t.Fatalf("取今日窗口表达式失败: %v", err)
	}
	startAtNow := "SELECT " + window.startExpr
	// 把「当前时刻」换成绑定参数，即可在**指定时刻**上验证同一表达式（本段是本用例的确定性部分）。
	startAtInstant := "SELECT " + strings.ReplaceAll(window.startExpr, "CURRENT_TIMESTAMP", "$2::timestamptz")

	localMidnight := func(t *testing.T, instant time.Time, zone string) time.Time {
		t.Helper()
		location, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatalf("加载时区 %s 失败: %v", zone, err)
		}
		local := instant.In(location)
		if hour, minute, second := local.Clock(); hour != 0 || minute != 0 || second != 0 {
			t.Fatalf("按 %s 求得的今日起点不是当地零点（实得 %s），时区参数没有进入窗口计算",
				zone, local.Format(time.RFC3339))
		}
		return local
	}

	// 第一段：生产表达式跑在**此刻**，只断言时不变性质。
	var shanghaiStart, utcStart time.Time
	if err := pool.QueryRow(ctx, startAtNow, "Asia/Shanghai").Scan(&shanghaiStart); err != nil {
		t.Fatalf("按 Asia/Shanghai 求今日起点失败: %v", err)
	}
	if err := pool.QueryRow(ctx, startAtNow, "UTC").Scan(&utcStart); err != nil {
		t.Fatalf("按 UTC 求今日起点失败: %v", err)
	}
	if shanghaiStart.Equal(utcStart) {
		t.Fatalf("两个时区的今日起点相同（%s），说明时区参数没有进入窗口计算", shanghaiStart)
	}
	localMidnight(t, shanghaiStart, "Asia/Shanghai")
	localMidnight(t, utcStart, "UTC")
	// 上海（UTC+8）与 UTC 的日界之差：同一天（当地）时是 +8h，跨日界时是 -16h。
	if offset := utcStart.Sub(shanghaiStart); (offset-8*time.Hour)%(24*time.Hour) != 0 {
		t.Fatalf("两时区今日起点之差应为 +8h 或 -16h（相差整 24h），实际 %s（上海 %s / UTC %s）",
			offset, shanghaiStart, utcStart)
	}

	// 第二段：把时刻钉死，两侧日界都验——这是原先“只验当时恰好所在的那一侧”所缺的覆盖。
	for _, c := range []struct {
		name    string
		instant time.Time
		want    time.Duration
	}{
		{
			// CST 12:30：上海与 UTC 同为 09-13，差值恰为 +8h。
			name:    "同日（CST 正午）",
			instant: time.Date(2026, time.September, 13, 4, 30, 0, 0, time.UTC),
			want:    8 * time.Hour,
		},
		{
			// CST 00:30：上海已进 09-13、UTC 仍是 09-12，故差值 -16h——
			// 这正是每天零点必现、而原先仅靠墙上时钟覆盖不到的那一侧。
			name:    "跨日界（CST 00:30）",
			instant: time.Date(2026, time.September, 12, 16, 30, 0, 0, time.UTC),
			want:    -16 * time.Hour,
		},
	} {
		var shanghaiAtInstant, utcAtInstant time.Time
		if err := pool.QueryRow(ctx, startAtInstant, "Asia/Shanghai", c.instant).Scan(&shanghaiAtInstant); err != nil {
			t.Fatalf("[%s] 按 Asia/Shanghai 求今日起点失败: %v", c.name, err)
		}
		if err := pool.QueryRow(ctx, startAtInstant, "UTC", c.instant).Scan(&utcAtInstant); err != nil {
			t.Fatalf("[%s] 按 UTC 求今日起点失败: %v", c.name, err)
		}
		localMidnight(t, shanghaiAtInstant, "Asia/Shanghai")
		localMidnight(t, utcAtInstant, "UTC")
		if got := utcAtInstant.Sub(shanghaiAtInstant); got != c.want {
			t.Fatalf("[%s] %s 起的两时区今日起点应相差 %s，实际 %s（上海 %s / UTC %s）",
				c.name, c.instant.Format(time.RFC3339), c.want, got,
				shanghaiAtInstant.Format(time.RFC3339), utcAtInstant.Format(time.RFC3339))
		}
	}
}

// statisticsOpenPools 复用 store 集成用例的连接池夹具（CCH_TEST_DSN 未设时自动跳过）。
func statisticsOpenPools(t *testing.T) *Pools {
	t.Helper()
	return openTestPools(t)
}
