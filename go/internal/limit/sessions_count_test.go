package limit

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// 本文件钉住 KeySessionCount 的三条判据（真 Redis；未设置 CCH_TEST_REDIS_URL 时跳过）：
// 过期成员先剪掉、非 ZSET 的旧格式键删掉并归零、只有 info 键仍在的成员才算活跃。
//
// 端到端（管理面读档随占位变化）在 adminapi 的集成测试里；这里只覆盖那三条在 HTTP 路径上
// 难以构造的分支。

// TestKeySessionCountCountsOnlyLiveMembers 覆盖「成员在但 info 键不在」这条判据：它正是
// Node countFromZSet 的第三、四步（src/lib/session-tracker.ts:740-758）。
func TestKeySessionCountCountsOnlyLiveMembers(t *testing.T) {
	client := integrationClient(t)
	tracker := NewSessionTracker(client, 60*time.Second, nil)
	raw := client.Raw()
	keyID := uniqueID(t)
	zsetKey := KeyActiveSessionsKey(keyID)
	cleanupKeys(t, client, zsetKey)

	ctx := context.Background()
	now := time.Now()

	// 一个成员同时有 info 键 ⇒ 计入。
	liveID := "limit-count-live-" + strconv.FormatInt(keyID, 10)
	deadID := "limit-count-dead-" + strconv.FormatInt(keyID, 10)
	defer raw.Del(ctx, sessionInfoKeyForTest(liveID), sessionInfoKeyForTest(deadID))

	if err := raw.ZAdd(ctx, zsetKey,
		redis.Z{Score: float64(now.UnixMilli()), Member: liveID},
		redis.Z{Score: float64(now.UnixMilli()), Member: deadID}).Err(); err != nil {
		t.Fatalf("写 ZSET 成员失败: %v", err)
	}
	if err := raw.Set(ctx, sessionInfoKeyForTest(liveID), "1", time.Minute).Err(); err != nil {
		t.Fatalf("写 info 键失败: %v", err)
	}

	count, err := tracker.KeySessionCount(ctx, keyID)
	if err != nil {
		t.Fatalf("计数失败: %v", err)
	}
	if count != 1 {
		t.Fatalf("只有带 info 键的成员应计入，期望 1，实际 %d", count)
	}

	// info 键消失 ⇒ 归零（成员还在，属残留）。
	if err := raw.Del(ctx, sessionInfoKeyForTest(liveID)).Err(); err != nil {
		t.Fatalf("删 info 键失败: %v", err)
	}
	if count, err = tracker.KeySessionCount(ctx, keyID); err != nil || count != 0 {
		t.Fatalf("info 键缺失时应为 0，实际 %d err=%v", count, err)
	}
}

// TestKeySessionCountPrunesExpiredMembers 覆盖 TTL 剪枝：过期成员不算数且会被真删掉
// （Node 的 ZREMRANGEBYSCORE，src/lib/session-tracker.ts:734）。
func TestKeySessionCountPrunesExpiredMembers(t *testing.T) {
	client := integrationClient(t)
	tracker := NewSessionTracker(client, 60*time.Second, nil)
	raw := client.Raw()
	keyID := uniqueID(t)
	zsetKey := KeyActiveSessionsKey(keyID)
	cleanupKeys(t, client, zsetKey)

	ctx := context.Background()
	staleID := "limit-count-stale-" + strconv.FormatInt(keyID, 10)
	defer raw.Del(ctx, sessionInfoKeyForTest(staleID))

	// 时间戳落在 TTL 窗口之外（2 倍 TTL 前），info 键仍在——按时间判过期。
	if err := raw.ZAdd(ctx, zsetKey, redis.Z{Score: float64(time.Now().Add(-2 * time.Minute).UnixMilli()), Member: staleID}).Err(); err != nil {
		t.Fatalf("写过期成员失败: %v", err)
	}
	if err := raw.Set(ctx, sessionInfoKeyForTest(staleID), "1", time.Minute).Err(); err != nil {
		t.Fatalf("写 info 键失败: %v", err)
	}

	count, err := tracker.KeySessionCount(ctx, keyID)
	if err != nil {
		t.Fatalf("计数失败: %v", err)
	}
	if count != 0 {
		t.Fatalf("过期成员不应计入，实际 %d", count)
	}
	if remaining, err := raw.ZCard(ctx, zsetKey).Result(); err != nil || remaining != 0 {
		t.Fatalf("过期成员应被剪掉，剩余 %d err=%v", remaining, err)
	}
}

// TestKeySessionCountDropsLegacySetKey 覆盖旧格式（Set 实现）残留：Node 直接删键并返回 0
// （session-tracker.ts:417-424），本实现同款。
func TestKeySessionCountDropsLegacySetKey(t *testing.T) {
	client := integrationClient(t)
	tracker := NewSessionTracker(client, 60*time.Second, nil)
	raw := client.Raw()
	keyID := uniqueID(t)
	zsetKey := KeyActiveSessionsKey(keyID)
	cleanupKeys(t, client, zsetKey)

	ctx := context.Background()
	if err := raw.SAdd(ctx, zsetKey, "legacy-member").Err(); err != nil {
		t.Fatalf("写旧格式 Set 失败: %v", err)
	}

	count, err := tracker.KeySessionCount(ctx, keyID)
	if err != nil {
		t.Fatalf("计数失败: %v", err)
	}
	if count != 0 {
		t.Fatalf("非 ZSET 键应返回 0，实际 %d", count)
	}
	if exists, err := raw.Exists(ctx, zsetKey).Result(); err != nil || exists != 0 {
		t.Fatalf("非 ZSET 键应被删除，exists=%d err=%v", exists, err)
	}
}

// TestKeySessionCountMissingKeyAndRedisDown 覆盖两个零值分支：键不存在、Redis 未装配。
func TestKeySessionCountMissingKeyAndRedisDown(t *testing.T) {
	client := integrationClient(t)
	tracker := NewSessionTracker(client, 60*time.Second, nil)
	keyID := uniqueID(t)
	cleanupKeys(t, client, KeyActiveSessionsKey(keyID))

	if count, err := tracker.KeySessionCount(context.Background(), keyID); err != nil || count != 0 {
		t.Fatalf("键不存在时应为 0，实际 %d err=%v", count, err)
	}
	// 未装配 Redis：按 Node 的「status !== ready ⇒ 0」直接归零，且不报错。
	offline := NewSessionTracker(nil, time.Minute, nil)
	if count, err := offline.KeySessionCount(context.Background(), keyID); err != nil || count != 0 {
		t.Fatalf("Redis 不可用时应为 0 且无错误，实际 %d err=%v", count, err)
	}
}

// sessionInfoKeyForTest 给出会话 info 键（与 session.InfoKey 同形制：session:{id}:info）。
//
// 这里不复用 internal/session 的构造器是为了不把 session 包拉进本测试的依赖面；形制由本文件
// 与 TestKeySessionCountCountsOnlyLiveMembers 的语义共同钉住（与 Node 的键名逐字一致）。
func sessionInfoKeyForTest(sessionID string) string {
	return "session:" + sessionID + ":info"
}

// TestUserSessionCountReadsUserDimension 钉住 User 维度计数读的是**自己的键**。
//
// 存在的理由：/api/v1/me/quota 的 userCurrentConcurrentSessions 取 User 维度 ZSET（Node 的
// getUserSessionCount），而不是该用户各密钥计数之和——两者在同一会话跨密钥续用时给出不同的数。
// 本条用「同名成员分别写进 key 键与 user 键」把两者区分开：若实现读错了键，计数会变成 2 或 0。
func TestUserSessionCountReadsUserDimension(t *testing.T) {
	client := integrationClient(t)
	tracker := NewSessionTracker(client, 60*time.Second, nil)
	raw := client.Raw()
	userID := uniqueID(t)
	keyZSet := KeyActiveSessionsKey(userID)
	userZSet := UserActiveSessionsKey(userID)
	for _, key := range []string{keyZSet, userZSet} {
		cleanupKeys(t, client, key)
	}

	ctx := context.Background()
	member := "limit-user-count-" + strconv.FormatInt(userID, 10)
	defer raw.Del(ctx, sessionInfoKeyForTest(member))
	if err := raw.Set(ctx, sessionInfoKeyForTest(member), "1", time.Minute).Err(); err != nil {
		t.Fatalf("写 info 键失败: %v", err)
	}
	// 只写 key 维度：User 维度必须读不到它。
	if err := raw.ZAdd(ctx, keyZSet, redis.Z{Score: float64(time.Now().UnixMilli()), Member: member}).Err(); err != nil {
		t.Fatalf("写 key 维度成员失败: %v", err)
	}
	if count, err := tracker.UserSessionCount(ctx, userID); err != nil || count != 0 {
		t.Fatalf("key 维度的成员不应计入 User 维度，期望 0，实际 %d err=%v", count, err)
	}
	// 再写 user 维度 ⇒ 计入 1。
	if err := raw.ZAdd(ctx, userZSet, redis.Z{Score: float64(time.Now().UnixMilli()), Member: member}).Err(); err != nil {
		t.Fatalf("写 user 维度成员失败: %v", err)
	}
	if count, err := tracker.UserSessionCount(ctx, userID); err != nil || count != 1 {
		t.Fatalf("User 维度成员应计入，期望 1，实际 %d err=%v", count, err)
	}
	// 非正 id 与未装配 Redis 都归零（与 KeySessionCount 同款兜底）。
	if count, err := tracker.UserSessionCount(ctx, 0); err != nil || count != 0 {
		t.Fatalf("非正 id 应为 0，实际 %d err=%v", count, err)
	}
	offline := NewSessionTracker(nil, time.Minute, nil)
	if count, err := offline.UserSessionCount(ctx, userID); err != nil || count != 0 {
		t.Fatalf("Redis 不可用时应为 0 且无错误，实际 %d err=%v", count, err)
	}
}
