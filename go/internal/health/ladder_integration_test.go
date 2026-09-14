package health

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
)

// 本文件是等待阶梯的**真 Redis** 实证（验收项 a/b/d/f）。未设置 CCH_TEST_REDIS_URL 时跳过，
// 故不影响无 Redis 环境下的常规门禁。
//
// 与 ladder_test.go 的分工：那边用内存替身跑状态机语义（快、无外部依赖），这边证明
// 「同一套语义在真 Redis 的读写往返下也成立」——落库形态（HSET 的字符串字段）、
// TTL、以及读回后再解析都对得上。
func ladderRedisClient(t *testing.T) (*redis.Client, context.Context) {
	t.Helper()
	url := os.Getenv("CCH_TEST_REDIS_URL")
	if url == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过真 Redis 集成测试")
	}
	options, err := redis.ParseURL(url)
	if err != nil {
		t.Fatalf("解析 CCH_TEST_REDIS_URL 失败（值已隐去）: %v", err)
	}
	client := redis.NewClient(options)
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Skipf("Redis 不可达，跳过: %v", err)
	}
	return client, ctx
}

// seedLadderConfig 把阶梯配置写进真 Redis 的熔断配置哈希（数据面读的就是这份）。
func seedLadderConfig(t *testing.T, client *redis.Client, ctx context.Context, providerID int64, incrementMS, maxCount int64) {
	t.Helper()
	key := route.ProviderConfigKeyPrefix + strconv.FormatInt(providerID, 10)
	if err := client.HSet(ctx, key, map[string]any{
		"failureThreshold":         "5",
		"openDuration":             "300000",
		"halfOpenSuccessThreshold": "2",
		"releaseIncrement":         strconv.FormatInt(incrementMS, 10),
		"maxOpenCount":             strconv.FormatInt(maxCount, 10),
	}).Err(); err != nil {
		t.Fatalf("写配置哈希失败: %v", err)
	}
	t.Cleanup(func() { client.Del(context.Background(), key) })
}

// redisWindowOf 从真 Redis 读回窗口时长（circuitOpenUntil - 当前注入时刻）。
func redisWindowOf(t *testing.T, client *redis.Client, ctx context.Context, now *time.Time, providerID int64) (int64, int64) {
	t.Helper()
	raw, err := client.HGetAll(ctx, providerKey(providerID)).Result()
	if err != nil {
		t.Fatalf("读状态哈希失败: %v", err)
	}
	until, err := strconv.ParseInt(raw["circuitOpenUntil"], 10, 64)
	if err != nil {
		t.Fatalf("读 circuitOpenUntil 失败: %v（原值 %q）", err, raw["circuitOpenUntil"])
	}
	level, err := strconv.ParseInt(raw["consecutiveOpenCount"], 10, 64)
	if err != nil {
		t.Fatalf("读 consecutiveOpenCount 失败: %v（原值 %q）", err, raw["consecutiveOpenCount"])
	}
	return until - now.UnixMilli(), level
}

func TestLadderSequenceAgainstRealRedis(t *testing.T) {
	client, ctx := ladderRedisClient(t)
	const providerID int64 = 990001
	seedLadderConfig(t, client, ctx, providerID, 600000, 3)
	t.Cleanup(func() { client.Del(context.Background(), providerKey(providerID)) })

	now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	writer := NewWriter(Options{Redis: client, Now: func() time.Time { return now }})

	// 第 0 轮：首次开闸 → 5m（base + increment × 0）
	for attempt := 0; attempt < 5; attempt++ {
		if err := writer.RecordProviderFailure(ctx, providerID, nil); err != nil {
			t.Fatalf("记失败出错: %v", err)
		}
	}
	window, level := redisWindowOf(t, client, ctx, &now, providerID)
	if window != 300000 || level != 0 {
		t.Fatalf("首次开闸：窗口 = %d、级数 = %d，期望 300000 / 0", window, level)
	}
	t.Logf("真 Redis 第 0 轮：窗口=%dms 级数=%d", window, level)

	// 连续 5 轮「窗口到期 → 试探失败」→ 15m / 25m / 35m / 35m / 35m
	wantWindows := []int64{900000, 1500000, 2100000, 2100000, 2100000}
	wantLevels := []int64{1, 2, 3, 3, 3}
	for round, wantWindow := range wantWindows {
		now = now.Add(time.Duration(window) * time.Millisecond).Add(time.Second)
		if err := writer.RecordProviderFailure(ctx, providerID, nil); err != nil {
			t.Fatalf("第 %d 轮试探失败出错: %v", round, err)
		}
		window, level = redisWindowOf(t, client, ctx, &now, providerID)
		if window != wantWindow || level != wantLevels[round] {
			t.Fatalf("第 %d 轮：窗口 = %d、级数 = %d，期望 %d / %d",
				round, window, level, wantWindow, wantLevels[round])
		}
		t.Logf("真 Redis 第 %d 轮：窗口=%dms 级数=%d", round, window, level)
	}

	// 恢复 → closed 且级数归零
	now = now.Add(time.Duration(window) * time.Millisecond).Add(time.Second)
	for attempt := 0; attempt < 2; attempt++ {
		if err := writer.RecordProviderSuccess(ctx, providerID); err != nil {
			t.Fatalf("记成功出错: %v", err)
		}
	}
	raw, err := client.HGetAll(ctx, providerKey(providerID)).Result()
	if err != nil {
		t.Fatalf("读状态失败: %v", err)
	}
	if raw["circuitState"] != string(route.StateClosed) || raw["consecutiveOpenCount"] != "0" {
		t.Fatalf("恢复后：state=%q level=%q，期望 closed / 0", raw["circuitState"], raw["consecutiveOpenCount"])
	}
	t.Logf("真 Redis 恢复：state=%s 级数=%s", raw["circuitState"], raw["consecutiveOpenCount"])

	// 恢复后再熔断 → 回到 base
	for attempt := 0; attempt < 5; attempt++ {
		if err := writer.RecordProviderFailure(ctx, providerID, nil); err != nil {
			t.Fatalf("记失败出错: %v", err)
		}
	}
	window, level = redisWindowOf(t, client, ctx, &now, providerID)
	if window != 300000 || level != 0 {
		t.Fatalf("恢复后再熔断：窗口 = %d、级数 = %d，期望回到 base 300000 / 0", window, level)
	}
}

// TestLadderOldStateCompatibleAgainstRealRedis 覆盖验收项 f：真 Redis 上手写一个**不含**
// 阶梯字段的旧哈希，断言按 0 处理、且其余字段不被破坏。
func TestLadderOldStateCompatibleAgainstRealRedis(t *testing.T) {
	client, ctx := ladderRedisClient(t)
	const providerID int64 = 990002
	seedLadderConfig(t, client, ctx, providerID, 600000, 3)

	now := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	legacyUntil := now.Add(-time.Minute).UnixMilli()
	if err := client.HSet(ctx, providerKey(providerID), map[string]any{
		"failureCount":         "7",
		"lastFailureTime":      strconv.FormatInt(now.Add(-time.Hour).UnixMilli(), 10),
		"circuitState":         string(route.StateOpen),
		"circuitOpenUntil":     strconv.FormatInt(legacyUntil, 10),
		"halfOpenSuccessCount": "0",
	}).Err(); err != nil {
		t.Fatalf("写旧状态失败: %v", err)
	}
	t.Cleanup(func() { client.Del(context.Background(), providerKey(providerID)) })

	writer := NewWriter(Options{Redis: client, Now: func() time.Time { return now }})
	if err := writer.RecordProviderFailure(ctx, providerID, nil); err != nil {
		t.Fatalf("记失败出错: %v", err)
	}
	window, level := redisWindowOf(t, client, ctx, &now, providerID)
	if window != 900000 || level != 1 {
		t.Fatalf("旧状态升级：窗口 = %d、级数 = %d，期望 900000 / 1（缺失按 0 再 +1）", window, level)
	}
	raw, err := client.HGetAll(ctx, providerKey(providerID)).Result()
	if err != nil {
		t.Fatalf("读状态失败: %v", err)
	}
	if raw["failureCount"] != "8" {
		t.Fatalf("既有 failureCount = %q，期望 8（未被抹掉）", raw["failureCount"])
	}
	if raw["circuitState"] != string(route.StateOpen) {
		t.Fatalf("状态 = %q，期望仍为 open", raw["circuitState"])
	}
}
