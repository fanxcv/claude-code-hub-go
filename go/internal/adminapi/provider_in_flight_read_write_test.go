package adminapi

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/limit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ratelimit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/session"
)

// 本文件钉住**写面与读面的口径一致**：页面上那个「在飞请求数」必须等于写侧真正占住的名额数，
// 且两边对「同一会话的多个在飞尝试」的看法相同。
//
// 为什么必须跨包钉：写侧是 `limit.CheckAndTrackProviderAttempt`（转发路径经
// dataplane.providerConcurrencyGate 调用），读侧是 `redisSessionRuntime.ProviderInFlightCounts`。
// 它们各自有单测，但「写进去的成员读侧认不认、算几个」只能两边都接上真 Redis 才验得出——
// 这正是本仓反复踩的「已定义 ≠ 已接线」的形制。

// TestProviderInFlightReadMatchesWrite 用真 Redis 钉住「写侧占几个名额 == 读侧报几个」。
func TestProviderInFlightReadMatchesWrite(t *testing.T) {
	rawURL := os.Getenv(dashboardRedisGate)
	if rawURL == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过 Redis 集成测试")
	}
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		t.Fatalf("解析 Redis URL 失败: %v", err)
	}
	raw := redis.NewClient(options)
	t.Cleanup(func() { _ = raw.Close() })
	if err := raw.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("Redis 不可达: %v", err)
	}
	registry, err := ratelimit.Embedded()
	if err != nil {
		t.Fatalf("装载脚本清单失败: %v", err)
	}
	client, err := ratelimit.New(raw, registry)
	if err != nil {
		t.Fatalf("组装脚本调用层失败: %v", err)
	}
	tracker := limit.NewSessionTracker(client, 300*time.Second, nil)
	reader := NewRedisSessionRuntime(raw, 300, nil)

	ctx := context.Background()
	providerID := time.Now().UnixNano() % 900000000
	t.Cleanup(func() {
		_ = raw.Del(context.Background(),
			limit.ProviderActiveSessionsKey(providerID), limit.ProviderSessionRefsKey(providerID)).Err()
	})

	const sessionID = "it-read-write-sess"
	// 同一会话的两个在飞尝试：写侧各占一个额度（每尝试计）。
	for i, token := range []string{"atk1", "atk2"} {
		result, err := tracker.CheckAndTrackProviderAttempt(ctx, providerID,
			session.ProviderAttemptMember(sessionID, token), 10)
		if err != nil {
			t.Fatalf("第 %d 次登记失败: %v", i+1, err)
		}
		if !result.Allowed {
			t.Fatalf("第 %d 次登记应放行: %+v", i+1, result)
		}
	}

	counts, err := reader.ProviderInFlightCounts(ctx, []int64{providerID})
	if err != nil {
		t.Fatalf("读在飞数失败: %v", err)
	}
	if counts[providerID] != 2 {
		t.Fatalf("写侧占了 2 个名额（同会话两个尝试），读侧应报 2，实际 %d", counts[providerID])
	}

	// 释放一个尝试：读侧应随之降到 1。
	if _, _, err := tracker.ReleaseProviderAttempt(ctx, providerID,
		session.ProviderAttemptMember(sessionID, "atk1")); err != nil {
		t.Fatalf("释放失败: %v", err)
	}
	counts, err = reader.ProviderInFlightCounts(ctx, []int64{providerID})
	if err != nil {
		t.Fatalf("释放后读数失败: %v", err)
	}
	if counts[providerID] != 1 {
		t.Fatalf("释放一个尝试后读侧应报 1，实际 %d", counts[providerID])
	}
}

// TestProviderInFlightReadIgnoresMissingSessionInfo 钉住「尝试不是会话」这一条：读侧
// **不得**再拿 session:{id}:info 的存在性去过滤成员（旧实现会，而那会把真实在飞数读小）。
func TestProviderInFlightReadIgnoresMissingSessionInfo(t *testing.T) {
	client, runtime := dashboardRedisRuntime(t, 300)
	ctx := context.Background()
	providerID := time.Now().UnixNano()%900000000 + 3
	key := providerActiveSessionsKey(providerID)
	t.Cleanup(func() { _ = client.Del(context.Background(), key).Err() })

	// 成员写进去，**故意不建 session:{sessionID}:info**。
	if err := client.ZAdd(ctx, key, redis.Z{
		Score:  float64(time.Now().UnixMilli()),
		Member: session.ProviderAttemptMember("it-no-info-sess", "atk1"),
	}).Err(); err != nil {
		t.Fatalf("写在飞成员失败: %v", err)
	}

	counts, err := runtime.ProviderInFlightCounts(ctx, []int64{providerID})
	if err != nil {
		t.Fatalf("读数失败: %v", err)
	}
	if counts[providerID] != 1 {
		t.Fatalf("缺 info 键不得影响在飞计数（应 1），实际 %d", counts[providerID])
	}
}
