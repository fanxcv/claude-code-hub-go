package adminapi

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// 本文件是根级认证面的**真实 Redis** 集成测试：钉住会话写入的键布局、payload 字段名与 TTL——
// 这三样任何一处错了，浏览器都会被 Go 签发的 cookie 打回 401，而单元测试里的桩看不出来。
//
// 未设置 CCH_TEST_REDIS_URL 时跳过（与 invalidate_test.go 同一门控）。夹具自钉：会话 id 用固定
// 前缀，测试结束时按精确 id 删除。

const authIssueRedisGate = "CCH_TEST_REDIS_URL"

// TestAuthSessionRoundTripOnRealRedis 验证「Go 写、Go 读」闭环与 payload 形状。
func TestAuthSessionRoundTripOnRealRedis(t *testing.T) {
	rawURL := os.Getenv(authIssueRedisGate)
	if rawURL == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过 Redis 集成测试")
	}
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		t.Fatalf("解析 Redis URL 失败: %v", err)
	}
	if options.DB == 0 {
		options.DB = 13
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("Redis 不可达: %v", err)
	}

	// 自钉：写入前先清掉这个会话 id（上一次跑失败时可能留下）。
	const fixtureSession = "sid_00000000-0000-4000-8000-00000000auth"
	t.Cleanup(func() { _ = client.Del(ctx, sessionKeyPrefix+fixtureSession).Err() })
	if err := client.Del(ctx, sessionKeyPrefix+fixtureSession).Err(); err != nil {
		t.Fatalf("清理夹具失败: %v", err)
	}

	writer := newRedisAuthSessions(client)
	account := LoginAccount{UserID: 4242, UserRole: "user", KeyID: 77, CanLoginWebUI: true}
	sessionID, err := writer.CreateAuthSession(ctx, "sk-round-trip-fixture", account, "session", time.Hour)
	if err != nil {
		t.Fatalf("写会话失败: %v", err)
	}
	if !strings.HasPrefix(sessionID, opaqueSessionIDPrefix) {
		t.Fatalf("会话 id 必须带 %s 前缀（detectSessionTokenKind 靠它分流），实际 %q", opaqueSessionIDPrefix, sessionID)
	}
	t.Cleanup(func() { _ = client.Del(ctx, sessionKeyPrefix+sessionID).Err() })

	// 1) 键布局与 TTL。
	ttl, err := client.TTL(ctx, sessionKeyPrefix+sessionID).Result()
	if err != nil {
		t.Fatalf("读 TTL 失败: %v", err)
	}
	if ttl <= 0 || ttl > time.Hour+time.Second {
		t.Fatalf("TTL 应落在 (0, 1h]，实际 %s", ttl)
	}

	// 2) payload 的七个键名与 SessionData 逐字一致（Node 的 parseSessionData 与 Go 的
	//    redisSessionReader.Read 都按这些键名解析，改一个键名会让两边同时认不出会话）。
	raw, err := client.Get(ctx, sessionKeyPrefix+sessionID).Result()
	if err != nil {
		t.Fatalf("读会话失败: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("会话 payload 不是合法 JSON: %v", err)
	}
	for _, key := range []string{"sessionId", "keyFingerprint", "credentialType", "userId", "userRole", "createdAt", "expiresAt"} {
		if _, ok := parsed[key]; !ok {
			t.Fatalf("会话 payload 缺字段 %q：%v", key, parsed)
		}
	}
	if parsed["userId"] != float64(4242) || parsed["credentialType"] != "session" {
		t.Fatalf("会话 payload 取值不符：%v", parsed)
	}
	// 指纹必须是密钥的 sha256（守卫按它逐把比对，auth.ts:448）。
	if parsed["keyFingerprint"] != keyFingerprint("sk-round-trip-fixture") {
		t.Fatalf("指纹应是密钥的 sha256，实际 %v", parsed["keyFingerprint"])
	}

	// 3) 闭环：AuthGuard 的会话读取端能认回这条会话（两处实现共用一个键布局）。
	reader := redisSessionReader{client: client}
	session, err := reader.Read(ctx, sessionID)
	if err != nil {
		t.Fatalf("守卫侧读会话失败: %v", err)
	}
	if session.UserID != 4242 || session.KeyFingerprint != keyFingerprint("sk-round-trip-fixture") {
		t.Fatalf("守卫侧读回的会话与写入不一致：%+v", session)
	}

	// 4) 吊销即删键。
	if err := writer.RevokeAuthSession(ctx, sessionID); err != nil {
		t.Fatalf("吊销失败: %v", err)
	}
	if err := client.Get(ctx, sessionKeyPrefix+sessionID).Err(); err != redis.Nil {
		t.Fatalf("吊销后键应不存在，实际 err=%v", err)
	}
	// 吊销一个不存在的会话不算错（登出必须永远成功，否则浏览器卡在登录态）。
	if err := writer.RevokeAuthSession(ctx, "sid_not-exists"); err != nil {
		t.Fatalf("吊销不存在的会话不应报错: %v", err)
	}
}
