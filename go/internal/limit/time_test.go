package limit

import (
	"testing"
	"time"
)

func TestNormalizeResetTime(t *testing.T) {
	cases := map[string]string{
		"":        "00:00",
		"18:00":   "18:00",
		"9:05":    "09:05",
		"23:59":   "23:59",
		"24:00":   "00:00", // 小时越界即退化
		"12:60":   "12:00",
		"abc":     "00:00",
		" 07:30 ": "07:30",
	}
	for input, want := range cases {
		if got := NormalizeResetTime(input); got != want {
			t.Errorf("NormalizeResetTime(%q) = %q，期望 %q", input, got, want)
		}
	}
}

func TestDailyWindowAndNextReset(t *testing.T) {
	shanghai := mustLocation(t, "Asia/Shanghai")
	// 2026-03-01 10:00（+08:00）= 02:00 UTC。
	now := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)

	// 重置时刻 18:00：当天还没到 18:00，所以窗口起点是昨天 18:00。
	start := DailyWindowStart(now, "18:00", shanghai)
	wantStart := time.Date(2026, 2, 28, 18, 0, 0, 0, shanghai)
	if !start.Equal(wantStart) {
		t.Errorf("窗口起点 = %s，期望 %s", start, wantStart)
	}

	// 下一个重置时刻是今天 18:00。
	next := NextDailyReset(now, "18:00", shanghai)
	wantNext := time.Date(2026, 3, 1, 18, 0, 0, 0, shanghai)
	if !next.Equal(wantNext) {
		t.Errorf("下次重置 = %s，期望 %s", next, wantNext)
	}

	// 重置时刻 00:00 且当前正好在重置点上：窗口起点就是今天 00:00（不早退到昨天）。
	midnightNow := time.Date(2026, 3, 1, 0, 0, 0, 0, shanghai)
	if got := DailyWindowStart(midnightNow, "00:00", shanghai); !got.Equal(midnightNow) {
		t.Errorf("整点边界窗口起点 = %s，期望 %s", got, midnightNow)
	}
	// 严格晚于 now：整点时的下次重置是明天。
	wantTomorrow := time.Date(2026, 3, 2, 0, 0, 0, 0, shanghai)
	if got := NextDailyReset(midnightNow, "00:00", shanghai); !got.Equal(wantTomorrow) {
		t.Errorf("整点边界下次重置 = %s，期望 %s", got, wantTomorrow)
	}
}

func TestWeekAndMonthBoundaries(t *testing.T) {
	shanghai := mustLocation(t, "Asia/Shanghai")
	// 2026-03-01 是周日；上海时间 10:00。
	now := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)

	weekStart := WeekStart(now, shanghai)
	wantWeekStart := time.Date(2026, 2, 23, 0, 0, 0, 0, shanghai) // 上周一
	if !weekStart.Equal(wantWeekStart) {
		t.Errorf("周起点 = %s，期望 %s（周一）", weekStart, wantWeekStart)
	}
	if next := NextWeekStart(now, shanghai); !next.Equal(wantWeekStart.AddDate(0, 0, 7)) {
		t.Errorf("下周起点 = %s", next)
	}

	monthStart := MonthStart(now, shanghai)
	wantMonthStart := time.Date(2026, 3, 1, 0, 0, 0, 0, shanghai)
	if !monthStart.Equal(wantMonthStart) {
		t.Errorf("月起点 = %s，期望 %s", monthStart, wantMonthStart)
	}
	wantNextMonth := time.Date(2026, 4, 1, 0, 0, 0, 0, shanghai)
	if next := NextMonthStart(now, shanghai); !next.Equal(wantNextMonth) {
		t.Errorf("下月起点 = %s，期望 %s", next, wantNextMonth)
	}
}

func TestWindowStartUsesTimezoneForFixedWindowsOnly(t *testing.T) {
	tokyo := mustLocation(t, "Asia/Tokyo")
	now := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)

	if got := WindowStart(Period5h, now, "", ResetRolling, tokyo); !got.Equal(now.Add(-5 * time.Hour)) {
		t.Errorf("5h 滚动窗口起点 = %s，期望 %s", got, now.Add(-5*time.Hour))
	}
	if got := WindowStart(PeriodDaily, now, "", ResetRolling, tokyo); !got.Equal(now.Add(-24 * time.Hour)) {
		t.Errorf("daily 滚动窗口起点 = %s，期望 %s", got, now.Add(-24*time.Hour))
	}
	// fixed 模式看时区：东京 11:00 还没到 18:00，窗口起点是昨天 18:00。
	wantDaily := time.Date(2026, 2, 28, 18, 0, 0, 0, tokyo)
	if got := WindowStart(PeriodDaily, now, "18:00", ResetFixed, tokyo); !got.Equal(wantDaily) {
		t.Errorf("daily 固定窗口起点 = %s，期望 %s", got, wantDaily)
	}
}

func TestTTLForPeriod(t *testing.T) {
	utc := time.UTC
	now := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)

	if got := TTLForPeriod(Period5h, now, "", utc); got != 5*3600 {
		t.Errorf("5h TTL = %d，期望 %d", got, 5*3600)
	}
	if got := TTLForPeriodWithMode(Period5h, now, "", ResetFixed, utc); got != 5*3600 {
		t.Errorf("5h 固定 TTL = %d", got)
	}
	if got := TTLForPeriodWithMode(PeriodDaily, now, "", ResetRolling, utc); got != 24*3600 {
		t.Errorf("daily 滚动 TTL = %d", got)
	}
	if got := TTLForPeriod(PeriodDaily, now, "00:00", utc); got != int64(22*3600) {
		t.Errorf("daily 固定 TTL = %d，期望 %d", got, 22*3600)
	}
	// 恰好落在重置点上时 TTL 不能为 0（Node 侧 Math.max(1, ...)）。
	if got := TTLForPeriod(PeriodDaily, time.Date(2026, 3, 1, 0, 0, 0, 0, utc), "00:00", utc); got != 86400 {
		t.Errorf("整点 TTL = %d，期望 86400", got)
	}
	// 周：2026-03-01 是周日，下一个周一是 03-02 00:00，即 22 小时后。
	if got := TTLForPeriod(PeriodWeekly, now, "", utc); got != int64(22*3600) {
		t.Errorf("周 TTL = %d，期望 %d", got, 22*3600)
	}
	// 月：3 月 1 日 02:00 → 4 月 1 日 00:00。
	wantMonthTTL := int64(time.Date(2026, 4, 1, 0, 0, 0, 0, utc).Sub(now) / time.Second)
	if got := TTLForPeriod(PeriodMonthly, now, "", utc); got != wantMonthTTL {
		t.Errorf("月 TTL = %d，期望 %d", got, wantMonthTTL)
	}
}

func TestLaterResetAndClip(t *testing.T) {
	early := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	late := early.Add(3 * time.Hour)

	if got := LaterReset(&early, &late); got == nil || !got.Equal(late) {
		t.Errorf("LaterReset 取更晚者失败: %v", got)
	}
	if got := LaterReset(nil, &early); got == nil || !got.Equal(early) {
		t.Errorf("LaterReset 单边失败: %v", got)
	}
	if got := LaterReset(nil, nil); got != nil {
		t.Errorf("LaterReset 双边为空应返回 nil")
	}

	start := early
	if got := ClipStartByResetAt(start, &late); !got.Equal(late) {
		t.Errorf("重置时刻更晚时起点应后移: %v", got)
	}
	if got := ClipStartByResetAt(start, &early); !got.Equal(start) {
		t.Errorf("重置时刻不晚于起点时不应变动: %v", got)
	}
	if got := ClipStartByResetAt(start, nil); !got.Equal(start) {
		t.Errorf("无重置时刻时不应变动: %v", got)
	}
}

func TestResetInfoFor(t *testing.T) {
	utc := time.UTC
	now := time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC)

	rolling5h := ResetInfoFor(Period5h, now, "", ResetRolling, nil, utc)
	if rolling5h.Type != "rolling" || rolling5h.Period != "5 小时" || rolling5h.ResetAt != nil {
		t.Errorf("5h 滚动重置信息不符: %+v", rolling5h)
	}
	dailyRolling := ResetInfoFor(PeriodDaily, now, "", ResetRolling, nil, utc)
	if dailyRolling.Type != "rolling" || dailyRolling.Period != "24 小时" {
		t.Errorf("daily 滚动重置信息不符: %+v", dailyRolling)
	}
	weekly := ResetInfoFor(PeriodWeekly, now, "", ResetFixed, nil, utc)
	if weekly.Type != "natural" || weekly.ResetAt == nil {
		t.Errorf("周重置信息不符: %+v", weekly)
	}

	ttl := int64(3600)
	fixed5h := ResetInfoFor(Period5h, now, "", ResetFixed, &ttl, utc)
	if fixed5h.Type != "custom" || fixed5h.ResetAt == nil || !fixed5h.ResetAt.Equal(now.Add(time.Hour)) {
		t.Errorf("5h 固定重置信息不符: %+v", fixed5h)
	}
	// TTL 非正时不给出重置时刻（Node 侧 getResetAtFromTtlSeconds 的同款判断）。
	zero := int64(0)
	if got := ResetInfoFor(Period5h, now, "", ResetFixed, &zero, utc); got.ResetAt != nil {
		t.Errorf("TTL 为 0 时不应给出重置时刻")
	}
}

func mustLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("加载时区 %s 失败: %v", name, err)
	}
	return loc
}
