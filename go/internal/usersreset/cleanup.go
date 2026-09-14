package usersreset

import (
	"context"
	"fmt"
	"strings"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// 本文件复刻 clearUserCostCache 的用户维度清理与 invalidateCachedUser
// （src/lib/redis/cost-cache-cleanup.ts:103-190、src/lib/security/api-key-auth-cache.ts:378-388）。
//
// 与 Node 的两处差别，都是「同一语义、更少往返」：
//   - Node 用 ioredis 的 pipeline 逐键 DEL；这里按批 `DEL key1 key2 ...`（SCAN 的批大小就是 DEL 的批大小）。
//   - Node 的 scanPattern 只在单次扫描内部重试；这里直接用 go-redis 的 SCAN 迭代器（自带游标与错误）。
//
// 不变的一条：**键名里嵌着 API Key 原文**（`total_cost:key:<明文>`），因此任何日志都不得回显
// 扫描到的键名或模式。这与 adminapi/invalidate.go:203 的取舍是同一条纪律。

// costCacheScanBatch 是 SCAN 与 DEL 的批大小。
//
// 取 256（与 adminapi/invalidate.go:42 一致）：SCAN 的批太小会让大键空间扫描的往返数爆炸，
// 太大则单次 DEL 命令过长且阻塞 Redis 更久。
const costCacheScanBatch = 256

// CostCleanupResult 是一次用户成本缓存清理的结果（cost-cache-cleanup.ts:17-22）。
type CostCleanupResult struct {
	// CostKeysDeleted 是删掉的键数。
	CostKeysDeleted int
	// CleanupFailed 表示至少有一次扫描或删除失败（调用方据此把整次重置判为失败）。
	CleanupFailed bool
}

// CostCleaner 清用户与其密钥的 Redis 运行态。
type CostCleaner struct {
	client redis.UniversalClient
	logger *logx.Logger
}

// NewCostCleaner 构造清理器；client 为 nil 时 Available 为 false。
func NewCostCleaner(client redis.UniversalClient, logger *logx.Logger) *CostCleaner {
	if logger == nil {
		logger = logx.New(nil)
	}
	return &CostCleaner{client: client, logger: logger}
}

// Available 表示命令连接已装配。
func (c *CostCleaner) Available() bool { return c != nil && c.client != nil }

// ClearUserCostCache 复刻 clearUserCostCache（cost-cache-cleanup.ts:103-190）。
//
// preserveFixed5h 为 true 时保留 `*:cost_5h_fixed` 键——重置的语义是「5h 固定窗口从请求时刻重新
// 计时」（那个窗口的键在准备阶段已被删掉并记下切点），若这里再删一次，把「切点已记、键已删」
// 这个成对状态破坏成「切点已记、键还在」，反而让窗口带着旧值继续累计。
func (c *CostCleaner) ClearUserCostCache(
	ctx context.Context,
	userID int64,
	keyIDs []int64,
	keyHashes []string,
	preserveFixed5h bool,
) (CostCleanupResult, error) {
	if !c.Available() {
		return CostCleanupResult{}, newError(ErrCodeCacheCleanupFailed)
	}
	patterns := make([]string, 0, 4+2*len(keyIDs)+2*len(keyHashes))
	for _, keyID := range keyIDs {
		patterns = append(patterns, fmt.Sprintf("key:%d:cost_*", keyID))
	}
	patterns = append(patterns,
		fmt.Sprintf("user:%d:cost_*", userID),
		fmt.Sprintf("total_cost:user:%d", userID),
		fmt.Sprintf("total_cost:user:%d:*", userID),
	)
	for _, keyHash := range keyHashes {
		patterns = append(patterns, "total_cost:key:"+keyHash, "total_cost:key:"+keyHash+":*")
	}
	for _, keyID := range keyIDs {
		patterns = append(patterns, fmt.Sprintf("lease:key:%d:*", keyID))
	}
	patterns = append(patterns, fmt.Sprintf("lease:user:%d:*", userID))

	result := CostCleanupResult{}
	for _, pattern := range patterns {
		deleted, failed := c.scanAndDelete(ctx, pattern, userID, preserveFixed5h)
		result.CostKeysDeleted += deleted
		if failed {
			result.CleanupFailed = true
		}
	}
	return result, nil
}

// scanAndDelete 扫描并删除匹配的键，返回删除数与是否失败。
//
// preserveFixed5h 的过滤放在这里：Node 是「扫出全部再 filter」，这里等价地在逐键判断时跳过
// `:cost_5h_fixed` 结尾的键——注意 `user:<id>:cost_5h_fixed` 与 `key:<id>:cost_5h_fixed` 都命中
// 该后缀，而租约键 `lease:*:cost_5h_fixed` 不存在（租约键形制见 lease.ts:56-67），故过滤只影响
// 那两个累计键，与 Node 的 filter 完全一致。
func (c *CostCleaner) scanAndDelete(
	ctx context.Context,
	pattern string,
	userID int64,
	preserveFixed5h bool,
) (int, bool) {
	iterator := c.client.Scan(ctx, 0, pattern, costCacheScanBatch).Iterator()
	batch := make([]string, 0, costCacheScanBatch)
	deleted := 0
	failed := false
	flush := func() {
		if len(batch) == 0 {
			return
		}
		removed, err := c.client.Del(ctx, batch...).Result()
		if err != nil {
			failed = true
			c.logger.Warn("go_usersreset_cleanup_delete_failed", map[string]any{
				"userId": userID,
				// 模式不回显：total_cost 的模式里嵌着密钥原文。
				"pattern": redactPattern(pattern),
				"error":   err.Error(),
			})
		} else {
			deleted += int(removed)
		}
		batch = batch[:0]
	}
	for iterator.Next(ctx) {
		key := iterator.Val()
		if preserveFixed5h && strings.HasSuffix(key, ":cost_5h_fixed") {
			continue
		}
		batch = append(batch, key)
		if len(batch) >= costCacheScanBatch {
			flush()
		}
	}
	flush()
	if err := iterator.Err(); err != nil {
		failed = true
		c.logger.Warn("go_usersreset_cleanup_scan_failed", map[string]any{
			"userId":  userID,
			"pattern": redactPattern(pattern),
			"error":   err.Error(),
		})
	}
	return deleted, failed
}

// InvalidateCachedUser 复刻 invalidateCachedUser（api-key-auth-cache.ts:378-388）：删掉用户的
// 认证缓存，使重置后的用户记录（成本重置标记已变）在下次请求时重新读库。
//
// 删不掉只记 warn：Node 侧那三行就是 catch-并-忽略（缓存有 TTL，最坏情形是几秒内读到旧记录）。
func (c *CostCleaner) InvalidateCachedUser(ctx context.Context, userID int64) {
	if !c.Available() {
		return
	}
	if err := c.client.Del(ctx, fmt.Sprintf("api_key_auth:v1:user:%d", userID)).Err(); err != nil {
		c.logger.Warn("go_usersreset_user_auth_cache_delete_failed", map[string]any{
			"userId": userID,
			"error":  err.Error(),
		})
	}
}

// redactPattern 把模式里的密钥原文抹掉，只留形状（同 adminapi/invalidate.go:222-229 的取舍）。
func redactPattern(pattern string) string {
	const marker = "total_cost:key:"
	if !strings.HasPrefix(pattern, marker) {
		return pattern
	}
	return marker + "<redacted>"
}
