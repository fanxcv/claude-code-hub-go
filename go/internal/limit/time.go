package limit

import (
	"regexp"
	"strconv"
	"time"
)

// Period 是限额周期，取值与 Node 侧 TimePeriod 一致。
type Period string

const (
	Period5h      Period = "5h"
	PeriodDaily   Period = "daily"
	PeriodWeekly  Period = "weekly"
	PeriodMonthly Period = "monthly"
)

// ResetMode 是 5h/daily 窗口的重置模式，取值与 Node 侧 DailyResetMode 一致。
type ResetMode string

const (
	// ResetRolling 是滚动窗口（5h / 24h 内逐渐释放）。
	ResetRolling ResetMode = "rolling"
	// ResetFixed 是固定窗口（到点清零）。
	ResetFixed ResetMode = "fixed"
)

// 窗口长度与固定 TTL，与 Node 侧 service.ts 的常量逐字一致。
const (
	window5hMillis  = int64(5 * 60 * 60 * 1000)
	window24hMillis = int64(24 * 60 * 60 * 1000)
	ttl5hRolling    = 21600
	ttlDailyRolling = 90000
	// DefaultResetTime 与 Node 侧 resetTime = "00:00" 的默认值一致。
	DefaultResetTime = "00:00"
)

// SecondUntilDay 等按日窗口的键 TTL 下界：Node 侧一律 Math.max(1, ...)，避免 0 TTL 立刻过期。
const minDailyTTL = 1

var resetTimePattern = regexp.MustCompile(`^([0-9]{1,2}):([0-9]{2})$`)

// ParseResetTime 解析 "HH:mm"；非法输入退化为 00:00，与 Node 侧 parseResetTime 相同。
func ParseResetTime(raw string) (hour int, minute int) {
	matches := resetTimePattern.FindStringSubmatch(trimSpace(raw))
	if matches == nil {
		return 0, 0
	}
	hour, err := strconv.Atoi(matches[1])
	if err != nil || hour < 0 || hour > 23 {
		hour = 0
	}
	minute, err = strconv.Atoi(matches[2])
	if err != nil || minute < 0 || minute > 59 {
		minute = 0
	}
	return hour, minute
}

// NormalizeResetTime 把任意输入归一为 "HH:mm"（补零）。
func NormalizeResetTime(raw string) string {
	hour, minute := ParseResetTime(raw)
	return pad2(hour) + ":" + pad2(minute)
}

func pad2(value int) string {
	if value < 10 {
		return "0" + strconv.Itoa(value)
	}
	return strconv.Itoa(value)
}

func trimSpace(raw string) string {
	start, end := 0, len(raw)
	for start < end && isSpace(raw[start]) {
		start++
	}
	for end > start && isSpace(raw[end-1]) {
		end--
	}
	return raw[start:end]
}

func isSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\n' || b == '\r' }

// DailyWindowStart 复刻 getCustomDailyResetTime：不晚于 now 的最近一次每日重置时刻。
func DailyWindowStart(now time.Time, resetTime string, loc *time.Location) time.Time {
	hour, minute := ParseResetTime(resetTime)
	zoned := now.In(loc)
	resetToday := time.Date(zoned.Year(), zoned.Month(), zoned.Day(), hour, minute, 0, 0, loc)
	if !now.Before(resetToday) {
		return resetToday
	}
	return resetToday.AddDate(0, 0, -1)
}

// NextDailyReset 复刻 getNextDailyResetTime：严格晚于 now 的下一次每日重置时刻。
func NextDailyReset(now time.Time, resetTime string, loc *time.Location) time.Time {
	hour, minute := ParseResetTime(resetTime)
	zoned := now.In(loc)
	resetToday := time.Date(zoned.Year(), zoned.Month(), zoned.Day(), hour, minute, 0, 0, loc)
	if now.Before(resetToday) {
		return resetToday
	}
	setHours := resetToday.AddDate(0, 0, 1)
	return time.Date(setHours.Year(), setHours.Month(), setHours.Day(), hour, minute, 0, 0, loc)
}

// WeekStart 是本周一 00:00（系统时区），对应 startOfWeek(weekStartsOn: 1)。
func WeekStart(now time.Time, loc *time.Location) time.Time {
	zoned := now.In(loc)
	weekday := int(zoned.Weekday())
	if weekday == 0 {
		weekday = 7 // 周日归到第 7 天，使周一成为一周之首
	}
	start := zoned.AddDate(0, 0, -(weekday - 1))
	return time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, loc)
}

// NextWeekStart 是下周一 00:00。
func NextWeekStart(now time.Time, loc *time.Location) time.Time {
	start := WeekStart(now, loc)
	next := start.AddDate(0, 0, 7)
	return time.Date(next.Year(), next.Month(), next.Day(), 0, 0, 0, 0, loc)
}

// MonthStart 是本月 1 号 00:00（系统时区）。
func MonthStart(now time.Time, loc *time.Location) time.Time {
	zoned := now.In(loc)
	return time.Date(zoned.Year(), zoned.Month(), 1, 0, 0, 0, 0, loc)
}

// NextMonthStart 是下月 1 号 00:00。
func NextMonthStart(now time.Time, loc *time.Location) time.Time {
	start := MonthStart(now, loc)
	next := start.AddDate(0, 1, 0)
	return time.Date(next.Year(), next.Month(), 1, 0, 0, 0, 0, loc)
}

// WindowStart 复刻 getTimeRangeForPeriodWithMode 的起点。
//
// 5h/daily 的 rolling 模式是相对 now 的回溯窗口（5h / 24h），不看时区；fixed 模式与时区有关。
func WindowStart(period Period, now time.Time, resetTime string, mode ResetMode, loc *time.Location) time.Time {
	switch period {
	case Period5h:
		if mode == ResetRolling {
			return now.Add(-time.Duration(window5hMillis) * time.Millisecond)
		}
		return now
	case PeriodDaily:
		if mode == ResetRolling {
			return now.Add(-time.Duration(window24hMillis) * time.Millisecond)
		}
		return DailyWindowStart(now, resetTime, loc)
	case PeriodWeekly:
		return WeekStart(now, loc)
	case PeriodMonthly:
		return MonthStart(now, loc)
	default:
		return now
	}
}

// TTLForPeriod 复刻 getTTLForPeriod：成本窗口键的存活时间（秒）。
func TTLForPeriod(period Period, now time.Time, resetTime string, loc *time.Location) int64 {
	switch period {
	case Period5h:
		return int64(5 * 3600)
	case PeriodDaily:
		next := NextDailyReset(now, resetTime, loc)
		return maxInt64(minDailyTTL, ceilSeconds(next.Sub(now)))
	case PeriodWeekly:
		return ceilSeconds(NextWeekStart(now, loc).Sub(now))
	case PeriodMonthly:
		return ceilSeconds(NextMonthStart(now, loc).Sub(now))
	default:
		return 0
	}
}

// TTLForPeriodWithMode 复刻 getTTLForPeriodWithMode。
func TTLForPeriodWithMode(period Period, now time.Time, resetTime string, mode ResetMode, loc *time.Location) int64 {
	if period == Period5h && mode == ResetFixed {
		return int64(5 * 3600)
	}
	if period == PeriodDaily && mode == ResetRolling {
		return int64(24 * 3600)
	}
	return TTLForPeriod(period, now, resetTime, loc)
}

// ResetInfo 是展示用的重置信息，字段与 Node 侧 ResetInfo 对应。
type ResetInfo struct {
	// Type 取 rolling、natural、custom 之一。
	Type string
	// ResetAt 是固定/自然窗口的重置时刻；滚动窗口为空。
	ResetAt *time.Time
	// Period 是滚动窗口的周期描述。
	Period string
}

// ResetInfoFor 复刻 getResetInfoWithMode（滚动窗口无固定重置时刻）。
func ResetInfoFor(period Period, now time.Time, resetTime string, mode ResetMode, ttlSeconds *int64, loc *time.Location) ResetInfo {
	if period == Period5h && mode == ResetFixed {
		resetAt := ResetAtFromTTL(now, ttlSeconds)
		return ResetInfo{Type: "custom", ResetAt: resetAt}
	}
	if period == PeriodDaily && mode == ResetRolling {
		return ResetInfo{Type: "rolling", Period: "24 小时"}
	}
	switch period {
	case Period5h:
		return ResetInfo{Type: "rolling", Period: "5 小时"}
	case PeriodDaily:
		resetAt := NextDailyReset(now, resetTime, loc)
		return ResetInfo{Type: "custom", ResetAt: &resetAt}
	case PeriodWeekly:
		resetAt := NextWeekStart(now, loc)
		return ResetInfo{Type: "natural", ResetAt: &resetAt}
	case PeriodMonthly:
		resetAt := NextMonthStart(now, loc)
		return ResetInfo{Type: "natural", ResetAt: &resetAt}
	default:
		return ResetInfo{}
	}
}

// ResetAtFromTTL 复刻 getResetAtFromTtlSeconds：TTL 非正或缺失时不给出重置时刻。
func ResetAtFromTTL(now time.Time, ttlSeconds *int64) *time.Time {
	if ttlSeconds == nil || *ttlSeconds <= 0 {
		return nil
	}
	resetAt := now.Add(time.Duration(*ttlSeconds) * time.Second)
	return &resetAt
}

// LaterReset 取两个重置时刻里更晚的一个（cost-reset-utils.ts 的 resolveLaterResetAt）。
func LaterReset(primary *time.Time, secondary *time.Time) *time.Time {
	if primary == nil {
		return secondary
	}
	if secondary == nil {
		return primary
	}
	if secondary.After(*primary) {
		return secondary
	}
	return primary
}

// ClipStartByResetAt 把窗口起点向重置时刻收敛（cost-reset-utils.ts 的 clipStartByResetAt）。
//
// 语义是「重置之后的消费才计入」，因此起点只能往后，不能提前。
func ClipStartByResetAt(start time.Time, resetAt *time.Time) time.Time {
	if resetAt != nil && resetAt.After(start) {
		return *resetAt
	}
	return start
}

// ceilSeconds 是 Math.ceil(毫秒差 / 1000) 的整数写法。
func ceilSeconds(delta time.Duration) int64 {
	millis := delta.Milliseconds()
	if millis <= 0 {
		return 0
	}
	return (millis + 999) / 1000
}

func maxInt64(left int64, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
