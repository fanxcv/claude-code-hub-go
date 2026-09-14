package limit

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Entity 是限额维度主体，取值与 Node 侧 "key" | "provider" | "user" 逐字一致。
type Entity string

const (
	EntityKey      Entity = "key"
	EntityProvider Entity = "provider"
	EntityUser     Entity = "user"
)

// Label 是 Node 侧 reason 文案里对主体的称呼（service.ts 的 typeName 三元表达式）。
func (e Entity) Label() string {
	switch e {
	case EntityKey:
		return "Key"
	case EntityProvider:
		return "供应商"
	case EntityUser:
		return "User"
	default:
		return string(e)
	}
}

// ledgerEntity 把主体映射到账本聚合维度（store 的 LedgerEntityType 取值与之一致）。
func (e Entity) ledgerEntity() string { return string(e) }

// activeSessionsHashTag 与 Node 侧 active-session-keys.ts 的 "{active_sessions}" 一致。
//
// 三类 active_sessions 键共享 hash tag 是刻意的：并发检查的 Lua 脚本一次操作 global/key/user
// 三个键，Redis Cluster 下不同 slot 会直接 CROSSSLOT 失败。provider 维度的脚本只动两个同前缀
// 键（provider:{id}:...），Node 侧也没有给它加 tag，这里保持一致。
const activeSessionsHashTag = "{active_sessions}"

// ActiveSessionsGlobalKey 是全局活跃 Session ZSET（观测用）。
func ActiveSessionsGlobalKey() string {
	return activeSessionsHashTag + ":global:active_sessions"
}

// KeyActiveSessionsKey 是 Key 维度活跃 Session ZSET（Key 并发上限判定用）。
func KeyActiveSessionsKey(keyID int64) string {
	return fmt.Sprintf("%s:key:%d:active_sessions", activeSessionsHashTag, keyID)
}

// UserActiveSessionsKey 是 User 维度活跃 Session ZSET（跨多 Key 的 User 并发上限判定用）。
func UserActiveSessionsKey(userID int64) string {
	return fmt.Sprintf("%s:user:%d:active_sessions", activeSessionsHashTag, userID)
}

// ProviderActiveSessionsKey 是供应商维度活跃 Session ZSET。
func ProviderActiveSessionsKey(providerID int64) string {
	return fmt.Sprintf("provider:%d:active_sessions", providerID)
}

// ProviderSessionRefsKey 是供应商维度的引用计数 HASH。
//
// 同一物理会话可能对同一供应商持有多次引用（重试/hedge），故并发 ZSET 之外还需要引用计数，
// 只有计数归零才真正释放。
func ProviderSessionRefsKey(providerID int64) string {
	return fmt.Sprintf("provider:%d:active_session_refs", providerID)
}

// Cost5hKey 是 5h 成本窗口键：{type}:{id}:cost_5h_{mode}。
func Cost5hKey(e Entity, id int64, mode ResetMode) string {
	return fmt.Sprintf("%s:%d:cost_5h_%s", e, id, mode)
}

// CostDailyRollingKey 是 daily 滚动窗口键（ZSET，无时间后缀，TTL 固定 24h）。
func CostDailyRollingKey(e Entity, id int64) string {
	return fmt.Sprintf("%s:%d:cost_daily_rolling", e, id)
}

// CostDailyFixedKey 是 daily 固定窗口键。
//
// 后缀是重置时刻（HH:mm -> HHmm）：不同主体可能配不同重置时刻，把时刻写进键名才不会互相覆盖。
func CostDailyFixedKey(e Entity, id int64, resetTime string) string {
	return fmt.Sprintf("%s:%d:cost_daily_%s", e, id, DailyResetSuffix(resetTime))
}

// CostPeriodFixedKey 是周/月固定窗口键：{type}:{id}:cost_weekly 或 cost_monthly。
func CostPeriodFixedKey(e Entity, id int64, period Period) string {
	return fmt.Sprintf("%s:%d:cost_%s", e, id, period)
}

// TotalCostCacheKey 是总额度检查的 Redis 缓存键。
//
// 形制与 Node 侧逐字一致：key 维度按 keyHash（明文密钥），user/provider 按 id；重置时间作为
// 后缀参与键名，使「重置后重新累计」天然换键。provider 无重置时间时补 ":none"。
func TotalCostCacheKey(e Entity, id int64, keyHash string, resetAt *time.Time) string {
	suffix := ""
	if resetAt != nil {
		suffix = ":" + strconv.FormatInt(resetAt.UnixMilli(), 10)
	}
	switch e {
	case EntityKey:
		return "total_cost:key:" + keyHash + suffix
	case EntityUser:
		return "total_cost:user:" + strconv.FormatInt(id, 10) + suffix
	default:
		if suffix == "" {
			suffix = ":none"
		}
		return "total_cost:provider:" + strconv.FormatInt(id, 10) + suffix
	}
}

// RPMWindowKey 是用户每分钟请求数窗口键。
func RPMWindowKey(userID int64) string {
	return fmt.Sprintf("user:%d:rpm_window", userID)
}

// DailyResetSuffix 把规范化的重置时刻转成键名后缀（"18:00" -> "1800"）。
func DailyResetSuffix(resetTime string) string {
	return strings.ReplaceAll(NormalizeResetTime(resetTime), ":", "")
}
