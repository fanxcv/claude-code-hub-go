package adminapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/redis/go-redis/v9"
)

// testRedis 读门控变量建客户端；未设置 CCH_TEST_REDIS_URL 时跳过。
// 库号固定落 13（URL 自带库号时以其为准），避免碰生产键空间。
func testRedis(t *testing.T) redis.UniversalClient {
	t.Helper()
	raw := os.Getenv("CCH_TEST_REDIS_URL")
	if raw == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过 Redis 集成测试")
	}
	options, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("解析 Redis URL 失败: %v", err)
	}
	if options.DB == 0 {
		options.DB = 13
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("Redis 不可达: %v", err)
	}
	return client
}

// mustSet 写一个键并在测试结束时兜底删除。
func mustSet(t *testing.T, client redis.UniversalClient, key, value string) {
	t.Helper()
	if err := client.Set(context.Background(), key, value, 5*time.Minute).Err(); err != nil {
		t.Fatalf("写键失败: %v", err)
	}
	t.Cleanup(func() {
		contexts, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = client.Del(contexts, key).Err()
	})
}

// mustExist 断言键存在；mustGone 断言键已删除。
func mustExist(t *testing.T, client redis.UniversalClient, key string) {
	t.Helper()
	if err := client.Get(context.Background(), key).Err(); err != nil {
		t.Fatalf("键应存在 %q: %v", key, err)
	}
}

func mustGone(t *testing.T, client redis.UniversalClient, key string) {
	t.Helper()
	if err := client.Get(context.Background(), key).Err(); err != redis.Nil {
		t.Fatalf("键应已删除 %q: err=%v", key, err)
	}
}

// TestInvalidateKeyAuthDeletesCacheKey 复刻 invalidateCachedKey 的键布局。
func TestInvalidateKeyAuthDeletesCacheKey(t *testing.T) {
	client := testRedis(t)
	invalidator := NewCacheInvalidator(Deps{}, InvalidatorOptions{Redis: client})

	apiKey := "sk-invalidate-" + time.Now().Format("150405.000000000")
	digest := sha256.Sum256([]byte(apiKey))
	target := apiKeyAuthKeyPrefix + hex.EncodeToString(digest[:])
	other := apiKeyAuthKeyPrefix + "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	mustSet(t, client, target, "{}")
	mustSet(t, client, other, "{}")

	invalidator.InvalidateKeyAuth(context.Background(), apiKey)
	mustGone(t, client, target)
	mustExist(t, client, other)
}

// TestInvalidateUserAuthDeletesCacheKey 复刻 invalidateCachedUser。
func TestInvalidateUserAuthDeletesCacheKey(t *testing.T) {
	client := testRedis(t)
	invalidator := NewCacheInvalidator(Deps{}, InvalidatorOptions{Redis: client})

	userID := time.Now().UnixNano() % 1000000
	target := fmt.Sprintf("%s%d", apiKeyAuthUserPrefix, userID)
	other := fmt.Sprintf("%s%d", apiKeyAuthUserPrefix, userID+1)
	mustSet(t, client, target, "{}")
	mustSet(t, client, other, "{}")

	invalidator.InvalidateUserAuth(context.Background(), userID)
	mustGone(t, client, target)
	mustExist(t, client, other)
}

// TestInvalidateKeyCostDeletesCostKeys 复刻 clearSingleKeyCostCache 的两段键。
func TestInvalidateKeyCostDeletesCostKeys(t *testing.T) {
	client := testRedis(t)
	pools := testPools(t)
	invalidator := NewCacheInvalidator(Deps{}, InvalidatorOptions{Redis: client, Pools: pools})

	userID := fixtureUser(t, pools, "user", true)
	keyID, keyValue := fixtureKey(t, pools, fixtureKeyOptions{userID: userID, canLoginWebUI: true, isEnabled: true})

	rolling := fmt.Sprintf("key:%d:cost_daily_rolling", keyID)
	total := "total_cost:key:" + keyValue
	totalSuffixed := total + ":1735689600000"
	unrelated := fmt.Sprintf("key:%d:cost_daily_rolling", keyID+1)
	for _, key := range []string{rolling, total, totalSuffixed, unrelated} {
		mustSet(t, client, key, "1")
	}

	invalidator.InvalidateKeyCost(context.Background(), keyID)
	mustGone(t, client, rolling)
	mustGone(t, client, total)
	mustGone(t, client, totalSuffixed)
	mustExist(t, client, unrelated)
}

// TestInvalidateUserCostDeletesUserAndKeyCostKeys 复刻 clearUserCostCache 的四段键。
func TestInvalidateUserCostDeletesUserAndKeyCostKeys(t *testing.T) {
	client := testRedis(t)
	pools := testPools(t)
	invalidator := NewCacheInvalidator(Deps{}, InvalidatorOptions{Redis: client, Pools: pools})

	userID := fixtureUser(t, pools, "user", true)
	keyID, _ := fixtureKey(t, pools, fixtureKeyOptions{userID: userID, canLoginWebUI: true, isEnabled: true})
	otherUserID := fixtureUser(t, pools, "user", true)
	otherKeyID, _ := fixtureKey(t, pools, fixtureKeyOptions{userID: otherUserID, canLoginWebUI: true, isEnabled: true})

	patterns := []string{
		fmt.Sprintf("user:%d:cost_daily_rolling", userID),
		fmt.Sprintf("total_cost:user:%d", userID),
		fmt.Sprintf("total_cost:user:%d:1735689600000", userID),
		fmt.Sprintf("key:%d:cost_5h_rolling", keyID),
	}
	for _, key := range patterns {
		mustSet(t, client, key, "1")
	}
	others := []string{
		fmt.Sprintf("user:%d:cost_daily_rolling", otherUserID),
		fmt.Sprintf("key:%d:cost_5h_rolling", otherKeyID),
	}
	for _, key := range others {
		mustSet(t, client, key, "1")
	}

	invalidator.InvalidateUserCost(context.Background(), userID)
	for _, key := range patterns {
		mustGone(t, client, key)
	}
	for _, key := range others {
		mustExist(t, client, key)
	}
}

// TestInvalidatePublishDomainUsesNodeChannel 验证通道名与 Node 一致（不得自拼通道）。
func TestInvalidatePublishDomainUsesNodeChannel(t *testing.T) {
	client := testRedis(t)
	bus := cfgsync.NewBus(client, nil)
	t.Cleanup(func() { _ = bus.Close() })
	invalidator := NewCacheInvalidator(Deps{}, InvalidatorOptions{Redis: client, Bus: bus})

	cases := []struct {
		domain  cfgsync.Domain
		channel string
	}{
		{cfgsync.DomainErrorRules, "cch:cache:error_rules:updated"},
		{cfgsync.DomainRequestFilters, "cch:cache:request_filters:updated"},
		{cfgsync.DomainSensitiveWords, "cch:cache:sensitive_words:updated"},
		{cfgsync.DomainAPIKeys, "cch:cache:api_keys:updated"},
		{cfgsync.DomainSystemSettings, "cch:cache:system_settings:updated"},
		// 端点域刻意复用 providers 通道（cfgsync.Spec 的既定语义）。
		{cfgsync.DomainProviderEndpoints, "cch:cache:providers:updated"},
	}

	for _, testCase := range cases {
		t.Run(string(testCase.domain), func(t *testing.T) {
			subscriber := client.Subscribe(context.Background(), testCase.channel)
			t.Cleanup(func() { _ = subscriber.Close() })
			if _, err := subscriber.Receive(context.Background()); err != nil {
				t.Fatalf("订阅通道失败: %v", err)
			}

			invalidator.PublishDomain(context.Background(), testCase.domain)

			receiptCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			receipt, err := subscriber.ReceiveMessage(receiptCtx)
			if err != nil {
				t.Fatalf("未收到 %q 的失效消息: %v", testCase.channel, err)
			}
			if receipt.Channel != testCase.channel {
				t.Fatalf("通道不符: %q", receipt.Channel)
			}
			if receipt.Payload == "" {
				t.Fatalf("消息载荷不该为空（与 Node 一致用毫秒时间戳）")
			}
		})
	}
}

// TestInvalidatorWired 验证接线可观测。
func TestInvalidatorWired(t *testing.T) {
	if NewCacheInvalidator(Deps{}, InvalidatorOptions{}).Wired() {
		t.Fatal("无 Redis 无 Bus 时应报未接线")
	}
	client := testRedis(t)
	if !NewCacheInvalidator(Deps{}, InvalidatorOptions{Redis: client}).Wired() {
		t.Fatal("有 Redis 时应报已接线")
	}
	var nilInvalidator *CacheInvalidator
	if nilInvalidator.Wired() {
		t.Fatal("nil 接收者应报未接线")
	}
}

// TestInvalidateNilClientIsNoop 验证未配 Redis 时静默无操作（与 Node 一致）。
func TestInvalidateNilClientIsNoop(t *testing.T) {
	invalidator := NewCacheInvalidator(Deps{}, InvalidatorOptions{Pools: &store.Pools{}})
	invalidator.InvalidateKeyAuth(context.Background(), "sk-anything")
	invalidator.InvalidateUserAuth(context.Background(), 1)
	invalidator.InvalidateKeyCost(context.Background(), 1)
	invalidator.InvalidateUserCost(context.Background(), 1)
	invalidator.PublishDomain(context.Background(), cfgsync.DomainAPIKeys)
}
