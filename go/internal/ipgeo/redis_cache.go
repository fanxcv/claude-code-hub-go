package ipgeo

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// NewRedisCache 把 go-redis 客户端包成 ipgeo.Cache。
//
// 单独给一个构造函数而不是让装配方自己写适配器：键前缀与值形制是**跨语言契约**
// （双跑期 Node 与 Go 读写同一份缓存），适配器写在各装配点就等于把契约抄成多份。
// client 为 nil 时返回 nil —— 调用方据此传 nil cache（LookupIP 会退化为直查，与 Node 在
// `getRedisClient()` 为 nil 时同判）。
func NewRedisCache(client redis.UniversalClient) Cache {
	if client == nil {
		return nil
	}
	return &redisCache{client: client}
}

type redisCache struct {
	client redis.UniversalClient
}

func (c *redisCache) Get(ctx context.Context, key string) (string, bool, error) {
	value, err := c.client.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

func (c *redisCache) Set(ctx context.Context, key, value string, ttl time.Duration) error {
	return c.client.Set(ctx, key, value, ttl).Err()
}
