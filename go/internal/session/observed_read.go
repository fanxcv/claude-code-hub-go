package session

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// 本文件是会话**观测集合**的只读面（管理面 sessions 列表的数据源）。
//
// 唯一真源：src/lib/session-tracker.ts 的 getObservedActiveSessions（:678-708）与
// getObservedConcurrentCountBatch（:909-935），以及 src/lib/session-manager.ts 的
// getAllSessionIds（:2205-2243，SCAN `session:*:info`）。
//
// 为什么单独一个文件：tracker.go 那一面是「判定并追踪」（会写入），而列表页只需要**只读**。
// 把读面写在同一个包里是为了复用键名与 TTL 口径（Node 的两个读函数都用 SESSION_TTL_MS 做
// 过期截断），但读函数本身不做任何写入——除 Node 也做的那两处自愈（类型不对的键删除、
// 过期成员摘除），见各自的注释。

// observedSessionInfoPrefix / Suffix 拼出会话详情键（Node 的 `session:${id}:info`）。
const (
	observedSessionInfoPrefix = "session:"
	observedSessionInfoSuffix = ":info"
)

// observedSessionInfoKey 复刻 Node 的 `session:${sessionIdentity}:info`。
func observedSessionInfoKey(sessionIdentity string) string {
	return observedSessionInfoPrefix + sessionIdentity + observedSessionInfoSuffix
}

// observedSessionTTL 取观测窗口（Node 的 SessionTracker.SESSION_TTL_MS）。
//
// Node 的 SESSION_TTL_SECONDS 与绑定 TTL 同源（SESSION_TTL 环境变量，默认 300），故这里直接
// 复用绑定层的默认值：两个包各存一份 300 只会在环境变量改成别的值时悄悄分叉。
func (b *Binder) observedSessionTTL() time.Duration {
	return time.Duration(DefaultBindingTTLSeconds) * time.Second
}

// ObservedActiveSessions 复刻 SessionTracker.getObservedActiveSessions。
//
// 顺序与 Node 逐条对齐：
//  1. 键不存在 → 空；类型不是 zset → 删除该键并返回空（Node 对历史遗留的 Set 数据就是这么自愈的）。
//  2. 按 now - TTL 摘除过期成员（Node 的 zremrangebyscore，"inf" 到 cutoff）。
//  3. 取全部成员，逐个 EXISTS `session:{id}:info`，**只保留仍在的**。
//
// 第 3 步不能省：ZSET 成员是「会话身份」，会话可能已被终止（info 键已删），不校验会把已终止的
// 会话算进活跃列表。代价是 N 次 EXISTS——Node 用 pipeline 一趟发完，这里同样用 pipeline。
func (b *Binder) ObservedActiveSessions(ctx context.Context) ([]string, error) {
	if !b.Ready() {
		return nil, ErrNilClient
	}
	raw := b.client.rc.Raw()
	key := ObservedGlobalActiveSessionsKey()

	exists, err := raw.Exists(ctx, key).Result()
	if err != nil {
		return nil, err
	}
	if exists != 1 {
		return nil, nil
	}
	keyType, err := raw.Type(ctx, key).Result()
	if err != nil {
		return nil, err
	}
	if keyType != "zset" {
		if delErr := raw.Del(ctx, key).Err(); delErr != nil {
			return nil, delErr
		}
		return nil, nil
	}

	cutoff := time.Now().Add(-b.observedSessionTTL()).UnixMilli()
	if err := raw.ZRemRangeByScore(ctx, key, "-inf", strconv.FormatInt(cutoff, 10)).Err(); err != nil {
		return nil, err
	}

	members, err := raw.ZRange(ctx, key, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return nil, nil
	}

	pipe := raw.Pipeline()
	commands := make([]*redis.IntCmd, 0, len(members))
	for _, member := range members {
		commands = append(commands, pipe.Exists(ctx, observedSessionInfoKey(member)))
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, err
	}

	active := make([]string, 0, len(members))
	for index, command := range commands {
		value, cmdErr := command.Result()
		if cmdErr == nil && value == 1 {
			active = append(active, members[index])
		}
	}
	return active, nil
}

// ObservedConcurrentCounts 复刻 SessionTracker.getObservedConcurrentCountBatch。
//
// 返回值**只包含**存在计数的身份：调用方按 Node 的 `?? 0` 语义取缺失值为 0。
// 非数字或非正的值按 0 记（Node 的 Number.parseInt 失败即 NaN，`?? 0` 只在缺失时生效；
// 这里把坏值归一成 0——两者的可观察结果都是「不计并发」，见 tracker.go 同款判据）。
func (b *Binder) ObservedConcurrentCounts(
	ctx context.Context,
	sessionIdentities []string,
) (map[string]int, error) {
	counts := make(map[string]int, len(sessionIdentities))
	if len(sessionIdentities) == 0 {
		return counts, nil
	}
	if !b.Ready() {
		return nil, ErrNilClient
	}
	raw := b.client.rc.Raw()

	pipe := raw.Pipeline()
	commands := make([]*redis.StringCmd, 0, len(sessionIdentities))
	for _, identity := range sessionIdentities {
		commands = append(commands, pipe.Get(ctx, ObservedConcurrentCountKey(identity)))
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		// 命令级错误落在各自的 Cmd 上（缺键就是 redis.Nil），逐条判更接近 Node。
		_ = err
	}

	for index, command := range commands {
		value, cmdErr := command.Result()
		if cmdErr != nil {
			continue
		}
		parsed, parseErr := strconv.Atoi(value)
		if parseErr != nil || parsed <= 0 {
			continue
		}
		counts[sessionIdentities[index]] = parsed
	}
	return counts, nil
}

// AllSessionIDs 复刻 SessionManager.getAllSessionIds：SCAN `session:*:info`，按 Node 的字符串
// 处理抽出会话 id（`replace("session:","").replace(":info","")`——注意只替换首次出现）。
//
// SCAN 而不是 KEYS：Node 也是 SCAN（COUNT 100），大库上 KEYS 会阻塞整个 Redis。
// 去重与顺序：Node 按 SCAN 批次累加、不去重（同一 id 不会出现两次：一个键只被扫一次）。
func (b *Binder) AllSessionIDs(ctx context.Context) ([]string, error) {
	if !b.Ready() {
		return nil, ErrNilClient
	}
	raw := b.client.rc.Raw()

	sessionIDs := make([]string, 0, 16)
	cursor := uint64(0)
	for {
		keys, next, err := raw.Scan(ctx, cursor, observedSessionInfoPrefix+"*"+observedSessionInfoSuffix, 100).Result()
		if err != nil {
			return nil, err
		}
		for _, key := range keys {
			sessionIDs = append(sessionIDs, trimSessionInfoKey(key))
		}
		cursor = next
		if cursor == 0 {
			return sessionIDs, nil
		}
	}
}

// trimSessionInfoKey 复刻 Node 的 `key.replace("session:", "").replace(":info", "")`。
//
// strings.Replace(...,1) 只替换首次出现，与 JS 的 String.replace 同判——用 TrimPrefix/TrimSuffix
// 在这里会**等价**，但换成 ReplaceAll 就不等价了（id 里含 ":info" 时会被截断）。
func trimSessionInfoKey(key string) string {
	trimmed := key
	if len(trimmed) >= len(observedSessionInfoPrefix) &&
		trimmed[:len(observedSessionInfoPrefix)] == observedSessionInfoPrefix {
		trimmed = trimmed[len(observedSessionInfoPrefix):]
	}
	if len(trimmed) >= len(observedSessionInfoSuffix) &&
		trimmed[len(trimmed)-len(observedSessionInfoSuffix):] == observedSessionInfoSuffix {
		trimmed = trimmed[:len(trimmed)-len(observedSessionInfoSuffix)]
	}
	return trimmed
}
