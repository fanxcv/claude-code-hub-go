package config

import (
	"os"
	"strings"
	"time"

	// tzdata 内嵌 IANA 时区库。
	//
	// 为什么必须内嵌：运行镜像基于 alpine / node-slim，alpine 默认不含 tzdata（Node 侧靠 ICU
	// 自带时区数据解析 Asia/Shanghai，Go 只认系统 zoneinfo 或本内嵌表）。缺失时
	// time.LoadLocation("Asia/Shanghai") 报错，而取值链在「候选非法」时一律跳下一级，
	// 于是整条链静默退到 UTC——正是本次要修的缺陷形态，且只在镜像里复现、本机看不出来。
	// 系统 zoneinfo 存在时优先级更高，内嵌表只在缺失时兜底，故两种环境都正确。
	_ "time/tzdata"
)

// EnvTimezoneDefault 是 TZ 未设置时的默认时区，与 TS 侧 env.schema.ts:179 的
// `TZ: z.string().default("Asia/Shanghai")` 逐字一致。
//
// envSpecs 里的 def 只在「变量未设置」时生效；本常量供拿不到 Config 的调用方复用同一默认，
// 避免第二处硬编码。
const EnvTimezoneDefault = "Asia/Shanghai"

// ResolveLocation 是 resolveSystemTimezone（src/lib/utils/timezone.ts:34-56）的复刻：
//
//	system_settings.timezone -> env TZ（默认 Asia/Shanghai）-> UTC
//
// 每级都用 isValidIANA 校验，非法即跳下一级；dbTimezone 为 nil 或非法（含空串）时用
// Env.TZ，Env.TZ 非法才落 UTC。返回值恒为可加载的时区，调用方不必再判空。
func (c Config) ResolveLocation(dbTimezone *string) *time.Location {
	return resolveLocation(dbTimezone, c.Env.TZ)
}

// ResolveLocationFromEnv 是同一链条的无 Config 形态，供只拿得到环境变量的调用方使用。
func ResolveLocationFromEnv(dbTimezone *string) *time.Location {
	tz, present := os.LookupEnv("TZ")
	if !present {
		tz = EnvTimezoneDefault
	}
	return resolveLocation(dbTimezone, tz)
}

// resolveLocation 是取值链的唯一实现：先 DB，再环境 TZ，最后 UTC。
func resolveLocation(dbTimezone *string, envTZ string) *time.Location {
	if dbTimezone != nil {
		if location := loadIANA(*dbTimezone); location != nil {
			return location
		}
	}
	if location := loadIANA(envTZ); location != nil {
		return location
	}
	return time.UTC
}

// loadIANA 校验并加载 IANA 时区名；非法或不可加载时返回 nil。
func loadIANA(name string) *time.Location {
	trimmed := strings.TrimSpace(name)
	if !isValidIANA(trimmed) {
		return nil
	}
	location, err := time.LoadLocation(trimmed)
	if err != nil {
		return nil
	}
	return location
}

// isValidIANA 判断字符串是否是运行时可加载的 IANA 时区名。
//
// 与 Node 的 isValidIANATimezone（timezone-shared.ts:30-38，用 Intl.DateTimeFormat 试探）
// 同义：空串与 "Local" 都不算可配置的 IANA 名（Node 的 Intl 同样不接受它们）。
func isValidIANA(name string) bool {
	if name == "" || name == "Local" {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}
