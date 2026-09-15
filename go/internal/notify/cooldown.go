package notify

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// cooldownKeyPrefix 是缓存告警去重键的前缀。键里放 providerId 与 model 两维——
// 冷却的单位就是「一个渠道上的一个模型」，与告警条目一一对应。
const cooldownKeyPrefix = "notification:cache-hit-rate-alert"

// CooldownKey 是某条告警的去重键。
//
// model 里可能有 `:` 或空白（上游模型名不保证干净），故用 `|` 分隔并在键尾保留原文：
// 去重只需要「同一条告警同键」，不需要键可解析。
func CooldownKey(providerID int64, model string) string {
	return fmt.Sprintf("%s|%d|%s", cooldownKeyPrefix, providerID, model)
}

// Cooldown 是告警去重存储。
//
// 契约：Claim 为 keys[i] 返回 true 表示本次占坑成功（应当发出），false 表示冷却期内已被占（丢弃）。
// 返回的切片长度必须与 keys 等长。
type Cooldown interface {
	Claim(ctx context.Context, keys []string, ttl time.Duration) ([]bool, error)
}

// redisCooldown 是 Cooldown 的 Redis 实现。
//
// 与 Node 的差别（登记）：Node 是「先 MGET 读一遍，再对未占用的键 SETEX」两步，竞态下两个实例
// 可能同时通过；这里用 SETNX 一步占坑，天然互斥——多实例同时到点时只有一方发得出。
type redisCooldown struct {
	client redis.UniversalClient
}

// NewRedisCooldown 包一个 Redis 客户端；client 为 nil 时返回 nil（调用方据此视为「不去重」）。
func NewRedisCooldown(client redis.UniversalClient) Cooldown {
	if client == nil {
		return nil
	}
	return &redisCooldown{client: client}
}

func (c *redisCooldown) Claim(
	ctx context.Context,
	keys []string,
	ttl time.Duration,
) ([]bool, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	pipe := c.client.Pipeline()
	commands := make([]*redis.BoolCmd, len(keys))
	for index, key := range keys {
		commands[index] = pipe.SetNX(ctx, key, "1", ttl)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	claimed := make([]bool, len(keys))
	for index, command := range commands {
		claimed[index] = command.Val()
	}
	return claimed, nil
}
