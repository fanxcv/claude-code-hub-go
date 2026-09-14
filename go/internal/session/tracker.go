package session

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// 活跃会话跟踪：复刻 src/lib/session-tracker.ts 的展示侧（global/key/user ZSET
// 与 observed 集合），以及 SessionManager 的 last_seen / 序号刷新。
//
// 与 limit 包的并发判定共用同一组 {active_sessions}:* ZSET 键——Node 侧二者也共用，
// 本包只负责「加成员、续 TTL」，并发判定与清理归属 limit 的 Lua。

// sessionZSetHostTTL 是活跃 ZSET 的兜底 TTL（Node 侧 1 小时）。
const sessionZSetHostTTL = 3600 * time.Second

// redisZ 是 ZSET 成员的 (score=时间戳, member=值) 封装。
func redisZ(now time.Time, member string) redis.Z {
	return redis.Z{Score: float64(now.UnixMilli()), Member: member}
}

// TrackSession 把会话加进 global/key(/user) 活跃 ZSET（复刻 trackSession）。
//
// Redis 不可用时静默跳过（Node 侧同样只在 ready 时写）。
func (b *Binder) TrackSession(ctx context.Context, sessionID string, keyID int64, userID int64) error {
	if !b.Ready() || sessionID == "" || keyID <= 0 {
		return nil
	}
	now := time.Now()
	raw := b.client.rc.Raw()
	pipe := raw.Pipeline()
	globalKey := ActiveSessionsGlobalKey()
	pipe.ZAdd(ctx, globalKey, redisZ(now, sessionID))
	pipe.Expire(ctx, globalKey, sessionZSetHostTTL)
	keyKey := KeyActiveSessionsKey(keyID)
	pipe.ZAdd(ctx, keyKey, redisZ(now, sessionID))
	pipe.Expire(ctx, keyKey, sessionZSetHostTTL)
	if userID > 0 {
		userKey := UserActiveSessionsKey(userID)
		pipe.ZAdd(ctx, userKey, redisZ(now, sessionID))
		pipe.Expire(ctx, userKey, sessionZSetHostTTL)
	}
	_, err := pipe.Exec(ctx)
	return err
}

// TrackObservedSession 记录 Dashboard/Sessions 页统计算法用的有效 Session identity。
func (b *Binder) TrackObservedSession(ctx context.Context, sessionIdentity string) error {
	if !b.Ready() || sessionIdentity == "" {
		return nil
	}
	key := ObservedGlobalActiveSessionsKey()
	raw := b.client.rc.Raw()
	pipe := raw.Pipeline()
	pipe.ZAdd(ctx, key, redisZ(time.Now(), sessionIdentity))
	pipe.Expire(ctx, key, sessionZSetHostTTL)
	_, err := pipe.Exec(ctx)
	return err
}

// RefreshObservedSession 响应完成后刷新有效 Session identity 的滑动窗口。
func (b *Binder) RefreshObservedSession(ctx context.Context, sessionIdentity string, ttl time.Duration) error {
	if !b.Ready() || sessionIdentity == "" {
		return nil
	}
	if ttl <= 0 {
		ttl = DefaultBindingTTLSeconds * time.Second
	}
	raw := b.client.rc.Raw()
	key := ObservedGlobalActiveSessionsKey()
	pipe := raw.Pipeline()
	pipe.ZAdd(ctx, key, redisZ(time.Now(), sessionIdentity))
	hostTTL := sessionZSetHostTTL
	if ttl > hostTTL {
		hostTTL = ttl
	}
	pipe.Expire(ctx, key, hostTTL)
	pipe.Expire(ctx, InfoKey(sessionIdentity), ttl)
	_, err := pipe.Exec(ctx)
	return err
}

// TerminateObservedSession 终止展示用的 Session identity（ZREM + 清计数与 info）。
//
// 返回值语义与 Node 一致：三个子命令中任意一个真的删掉了东西即为 true，而不只看 ZREM——
// 「观测集里已无此成员、但并发计数或 info 还在」同样是终止动作生效。
func (b *Binder) TerminateObservedSession(ctx context.Context, sessionIdentity string) (bool, error) {
	if !b.Ready() || sessionIdentity == "" {
		return false, nil
	}
	raw := b.client.rc.Raw()
	pipe := raw.Pipeline()
	pipe.ZRem(ctx, ObservedGlobalActiveSessionsKey(), sessionIdentity)
	pipe.Del(ctx, ObservedConcurrentCountKey(sessionIdentity))
	pipe.Del(ctx, InfoKey(sessionIdentity))
	commands, err := pipe.Exec(ctx)
	if err != nil {
		return false, err
	}
	deleted := false
	for _, command := range commands {
		if command.Err() != nil {
			continue
		}
		if intCommand, ok := command.(*redis.IntCmd); ok && intCommand.Val() > 0 {
			deleted = true
		}
	}
	return deleted, nil
}

// MarkLastSeen 刷新会话最后活动时间（复刻 refreshSessionTTL）。
func (b *Binder) MarkLastSeen(ctx context.Context, sessionID string, ttl time.Duration) error {
	if !b.Ready() || sessionID == "" {
		return nil
	}
	if ttl <= 0 {
		ttl = DefaultBindingTTLSeconds * time.Second
	}
	key := LastSeenKey(sessionID)
	return b.client.rc.Raw().Set(ctx, key, strconv.FormatInt(time.Now().UnixMilli(), 10), ttl).Err()
}
