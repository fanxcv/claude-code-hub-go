package route

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ProviderActiveNow 复刻 Node 的 isProviderActiveNow（src/lib/utils/provider-schedule.ts:34-60）。
//
// 语义逐条对齐：
//   - 任一端为空 → 恒活跃；
//   - start == end → 恒不活跃（零长度窗口**不是**「全天」）；
//   - 格式非法 → **fail-open** 恒活跃（库里脏数据不该让供应商整体消失）；
//   - start < end → 同日窗口 `[start, end)`；
//   - 跨日 → `now >= start || now < end`。
//
// now 必须已经是**目标时区**的时刻（调用方用 time.Now().In(location) 构造）；
// 本函数只比「当日分钟数」，不碰时区——与 Node 的 getCurrentMinutesInTimezone + parseHHMM
// 分工一致（那边用 Intl 取当地时分，这里由调用方先换时区）。
//
// 本函数是**唯一实现**：管理面模拟器与数据面选路都调它（此前两处各一份，见 git 历史）。
func ProviderActiveNow(start *string, end *string, now time.Time) bool {
	if start == nil || end == nil {
		return true
	}
	startText := strings.TrimSpace(*start)
	endText := strings.TrimSpace(*end)
	if startText == "" || endText == "" {
		return true
	}
	if startText == endText {
		return false
	}
	startMinutes, okStart := parseHHMM(startText)
	endMinutes, okEnd := parseHHMM(endText)
	if !okStart || !okEnd {
		// Fail-open：库里脏数据不该让供应商整体消失（Node 同）。
		return true
	}
	nowMinutes := now.Hour()*60 + now.Minute()
	if startMinutes < endMinutes {
		return nowMinutes >= startMinutes && nowMinutes < endMinutes
	}
	return nowMinutes >= startMinutes || nowMinutes < endMinutes
}

// hhmmPattern 与 Node 的 HHMM_RE 同义（00:00–23:59）。
var hhmmPattern = regexp.MustCompile(`^([01]\d|2[0-3]):([0-5]\d)$`)

// parseHHMM 解析 `HH:MM` 为当日分钟数；格式非法返回 false（Node 用 Number.NaN 表达）。
func parseHHMM(value string) (int, bool) {
	match := hhmmPattern.FindStringSubmatch(value)
	if match == nil {
		return 0, false
	}
	hour, err := strconv.Atoi(match[1])
	if err != nil {
		return 0, false
	}
	minute, err := strconv.Atoi(match[2])
	if err != nil {
		return 0, false
	}
	return hour*60 + minute, true
}
