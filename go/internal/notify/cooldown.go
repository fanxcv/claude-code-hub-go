package notify

import (
	"context"
	"encoding/base64"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// 本文件是缓存命中率告警的冷却去重（Node：tasks/cache-hit-rate-alert.ts 的
// buildCooldownKey / applyCacheHitRateAlertCooldownToPayload / commitCacheHitRateAlertCooldown）。
//
// 两段式语义（不要在别处简化成一次 SETNX）：
//  1. **发送前** 用 MGET 读一遍：键已存在 => 该条被抑制（suppressedCount++），本体不发；
//  2. **发送成功后** 才把剩下那些键 SET EX 写入（源文件 592-597 行）。
//
// 之所以不能提前占坑：投递失败要能重试并照常发出去；提前写死会让一次失败把冷却期吃掉。

// CacheHitRateAlertCooldownKeyPrefix 是冷却键前缀（源文件 80 行）。
const CacheHitRateAlertCooldownKeyPrefix = "cache-hit-rate-alert"

// cooldownKeyParams 是冷却键的输入。
type cooldownKeyParams struct {
	ProviderID int64
	Model      string
	WindowMode string
	// BindingID 为 0 表示 legacy 单 URL 模式：键里不带 binding 段。
	BindingID int64
}

// BuildCacheHitRateAlertCooldownKey 复刻 buildCooldownKey（源文件 73-87 行）。
//
// 形状：`cache-hit-rate-alert:v1[:binding:<id>]:<providerId>:<base64url(model)>:<windowMode>`。
// 模型名走 base64url（不带填充）是为了让任意字符集都能进 Redis 键而不产生歧义。
func BuildCacheHitRateAlertCooldownKey(params cooldownKeyParams) string {
	key := CacheHitRateAlertCooldownKeyPrefix + ":v1:"
	if params.BindingID != 0 {
		key += "binding:" + strconv.FormatInt(params.BindingID, 10) + ":"
	}
	key += strconv.FormatInt(params.ProviderID, 10) + ":"
	key += base64.RawURLEncoding.EncodeToString([]byte(params.Model)) + ":"
	key += params.WindowMode
	return key
}

// Cooldown 是告警去重存储：读一遍（Present）+ 投递成功后写入（Set）。
//
// Present 返回的切片必须与 keys 等长，true 表示该键已存在（本次抑制）。
type Cooldown interface {
	Present(ctx context.Context, keys []string) ([]bool, error)
	Set(ctx context.Context, keys []string, ttl time.Duration) error
}

// redisCooldown 是 Cooldown 的 Redis 实现。
type redisCooldown struct {
	client redis.UniversalClient
}

// NewRedisCooldown 包一个 Redis 客户端；client 为 nil 时返回 nil（调用方据此视为「不去重」，
// 与 Node 的 `getRedisClient()` 回 null 的分支一致）。
func NewRedisCooldown(client redis.UniversalClient) Cooldown {
	if client == nil {
		return nil
	}
	return &redisCooldown{client: client}
}

func (c *redisCooldown) Present(ctx context.Context, keys []string) ([]bool, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	values, err := c.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	present := make([]bool, len(keys))
	for index := range keys {
		if index >= len(values) {
			break
		}
		present[index] = values[index] != nil
	}
	return present, nil
}

func (c *redisCooldown) Set(ctx context.Context, keys []string, ttl time.Duration) error {
	if len(keys) == 0 || ttl <= 0 {
		return nil
	}
	pipe := c.client.Pipeline()
	for _, key := range keys {
		pipe.Set(ctx, key, "1", ttl)
	}
	_, err := pipe.Exec(ctx)
	return err
}
