package usersreset

import (
	"fmt"

	"github.com/fanxcv/claude-code-hub-go/go/internal/limit"
)

// 本文件是键布局与两段 Lua 的唯一出处，逐字对齐 Node：
//   - 三个前缀与 TTL：src/lib/user-statistics-reset/reset-status-store.ts:12-15
//   - 认领释放的 Lua：同文件 :26-30（LUA_COMPARE_DELETE）
//   - 5h 固定窗口准备 Lua 与键序：src/lib/redis/cost-cache-cleanup.ts:44-54 与 :170-190
//
// 键名是跨进程契约：与 Node 不一致不会报错，只会让两侧互相看不见对方的作业。故一律照抄字面量。

const (
	// StatusTTLSeconds 与 reset-status-store.ts:12 的 RESET_STATUS_TTL_SECONDS 一致（7 天）。
	StatusTTLSeconds = 7 * 24 * 60 * 60

	statusPrefix = "cch:user-statistics-reset:status:"
	activePrefix = "cch:user-statistics-reset:active:"
	// fixed5hPrefix 是 5h 固定窗口准备的标记键（值 = 切点毫秒）。
	fixed5hPrefix = "cch:user-statistics-reset:fixed5h:"

	// pendingKey 是 **Go 专有**的待办队列：Redis ZSET，member 是载荷 JSON，score 是到期毫秒。
	//
	// Node 侧没有这个键（它用 Bull 的 `bull:user-statistics-reset:*`），故队列不跨端，见包说明第 2 条。
	// 用 ZSET 而不是 LIST：重试要「推迟到某时刻再取」，那个到期时刻就是 score；LIST 做不到。
	pendingKey = "cch:user-statistics-reset:go:pending"
)

// statusKey 返回作业状态键。
func statusKey(resetID string) string { return statusPrefix + resetID }

// activeKey 返回用户维度的认领键（值是 resetId）。
func activeKey(userID int64) string { return fmt.Sprintf("%s%d", activePrefix, userID) }

// fixed5hKey 返回 5h 固定窗口准备标记键。
func fixed5hKey(resetID string) string { return fixed5hPrefix + resetID }

// fixed5hWindowKeys 复刻 prepareUserStatisticsResetFixed5h 的键序列
// （cost-cache-cleanup.ts:170-190）：第 1 个是准备标记（保留并写入切点），其后逐个是**要删掉**的
// 5h 固定窗口累计键与租约键。
//
// 两张表的键名各取本包外唯一出处：累计键用 limit.Cost5hKey（keys.go:72），租约键用
// leaseKey——Node 的 buildLeaseKey（src/lib/rate-limit/lease.ts:56-67）在 Go 侧只有数据面内部
// 用到，没有导出构造器，故这里按其字面格式拼（`lease:{type}:{id}:{window}:{mode}`）。
func fixed5hWindowKeys(resetID string, userID int64, keyIDs []int64) []string {
	keys := make([]string, 0, 3+2*len(keyIDs))
	keys = append(keys,
		fixed5hKey(resetID),
		limit.Cost5hKey(limit.EntityUser, userID, limit.ResetFixed),
		leaseKey("user", userID, "5h", "fixed"),
	)
	for _, keyID := range keyIDs {
		keys = append(keys,
			limit.Cost5hKey(limit.EntityKey, keyID, limit.ResetFixed),
			leaseKey("key", keyID, "5h", "fixed"),
		)
	}
	return keys
}

// leaseKey 复刻 buildLeaseKey（src/lib/rate-limit/lease.ts:56-67）的 5h 分支。
func leaseKey(entityType string, entityID int64, window, resetMode string) string {
	return fmt.Sprintf("lease:%s:%d:%s:%s", entityType, entityID, window, resetMode)
}

// luaCompareDelete 逐字取自 reset-status-store.ts:26-30。
//
// 作用：只删掉「值还是自己」的认领键——释放别人的认领会把后来者的作业变成无主状态。
const luaCompareDelete = `
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0`

// luaPrepareFixed5h 逐字取自 cost-cache-cleanup.ts:45-54。
//
// 语义：标记键存在即回它存的值（幂等，两端复用同一个切点）；不存在则用 **Redis 服务器时间**取切点、
// 删掉全部 5h 固定窗口键、写入标记并回传切点。
//
// 为什么切点必须来自 Redis 而不是进程时钟：切点同时决定「删了哪些键」与「删到哪一刻的行」，
// 两个进程（Node 与 Go）的时钟不同步时，用各自的时间会删出两个不一致的边界。
const luaPrepareFixed5h = `
if redis.call('EXISTS', KEYS[1]) == 1 then
  return redis.call('GET', KEYS[1])
end
local now = redis.call('TIME')
local cutoff_ms = (tonumber(now[1]) * 1000) + math.floor(tonumber(now[2]) / 1000)
for index = 2, #KEYS do
  redis.call('DEL', KEYS[index])
end
redis.call('SETEX', KEYS[1], ARGV[1], tostring(cutoff_ms))
return tostring(cutoff_ms)`
