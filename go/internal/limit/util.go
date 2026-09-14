package limit

import (
	"strconv"
	"time"
)

// nowMillis 是当前毫秒时间戳。
func nowMillis() int64 { return time.Now().UnixMilli() }

// nowMillisDefault 保留旧名以贴合「缺省时间戳」的读法。
func nowMillisDefault() int64 { return nowMillis() }

func parseFloatOrZeroErr(raw string) (float64, error) {
	return strconv.ParseFloat(raw, 64)
}

// isoMillis 复刻 JS 的 Date#toISOString（毫秒精度、UTC、固定 24 字符）。
func isoMillis(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// isoMillisOrEmpty 是 isoMillis 的可空形态：nil 表示「没有固定重置时刻」（Node 的 null）。
func isoMillisOrEmpty(value *time.Time) string {
	if value == nil {
		return ""
	}
	return isoMillis(*value)
}

// retryAfterSeconds 复刻 error-handler.ts 的 calculateRetryAfter：非负、向上取整、秒。
func retryAfterSeconds(resetAt *time.Time, now time.Time) *int {
	if resetAt == nil {
		return nil
	}
	delta := resetAt.Sub(now)
	seconds := int(delta / time.Second)
	if delta%time.Second != 0 {
		seconds++
	}
	if seconds < 0 {
		seconds = 0
	}
	return &seconds
}
