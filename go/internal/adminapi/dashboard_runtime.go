package adminapi

import (
	"context"
	"regexp"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// 本文件是 dashboard 资源族的 **Redis 运行态读数**（会话观测集合）。
//
// 唯一真源：src/lib/session-tracker.ts 的 getObservedGlobalSessionCount / getProviderSessionCount
// 与两者共用的 countFromZSet，键名来自 src/lib/redis/active-session-keys.ts。
//
// 为什么不复用 internal/limit 的 SessionTracker：那一层的职责是「判定并追踪」（原子 Lua，
// 会写入），而管理面只需要**只读计数**；把它拉进管理面会让只读面持有写路径的依赖。这里单独
// 实现计数，并把 Node 的清理语义（过期成员摘除 + 供应商引用计数同步）一并搬过来——不清理的话
// 计数会把已过期的成员算进去，而 Node 的这两个读函数**都会顺手清理**。

// observedSessionsKey 与 active-session-keys.ts:17 的 getObservedGlobalActiveSessionsKey 一致。
const observedSessionsKey = "{observed_sessions}:global:active_sessions"

// sessionInfoKeyPrefix 是会话详情键前缀（countFromZSet 用它做「成员是否真存在」的第二道校验）。
const sessionInfoKeyPrefix = "session:"

// sessionInfoKeySuffix 是会话详情键后缀。
const sessionInfoKeySuffix = ":info"

// providerActiveSessionsPattern 与 session-tracker.ts:16 的正则一致：只有供应商维度的
// active_sessions 键才有对应的引用计数 HASH。
var providerActiveSessionsPattern = regexp.MustCompile(`^provider:(\d+):active_sessions$`)

// ObservedSessionRuntime 读会话观测运行态。
//
// nil 表示未装配：依赖它的 dashboard 端点不注册（回退 Node）。恒 0 的并发数在大屏上是**静默
// 错数**，比不接管更坏——与本包其余读档依赖同一条纪律。
type ObservedSessionRuntime interface {
	// ObservedSessionCount 复刻 SessionTracker.getObservedGlobalSessionCount。
	ObservedSessionCount(ctx context.Context) (int, error)
	// ProviderSessionCounts 复刻 getProviderSessionCount 的批量版本。
	ProviderSessionCounts(ctx context.Context, providerIDs []int64) (map[int64]int, error)
	// ObservedSessionIdentities 复刻 getObservedActiveSessions：活跃会话的 identity 清单。
	//
	// 与计数同源、但用途不同（dashboard/realtime 的活动流要按 identity 查最近请求）。
	// 不复用 countZSet 的计数结果：那里把 identity 用掉了，这里要的是清单本身。
	ObservedSessionIdentities(ctx context.Context) ([]string, error)
}

// redisSessionRuntime 是 ObservedSessionRuntime 的 Redis 实现。
type redisSessionRuntime struct {
	client redis.UniversalClient
	// ttl 是会话观测窗口（Node 的 SESSION_TTL_MS）：晚于 now-ttl 的成员才算有效。
	ttl    time.Duration
	logger *logx.Logger
}

// NewRedisSessionRuntime 装配会话观测读数；client 为 nil 时返回 nil（调用方据此不注册）。
//
// ttlSeconds <= 0 时取 300（与 Node 的 SESSION_TTL 默认值一致）。
func NewRedisSessionRuntime(
	client redis.UniversalClient,
	ttlSeconds int64,
	logger *logx.Logger,
) ObservedSessionRuntime {
	if client == nil {
		return nil
	}
	if logger == nil {
		logger = logx.New(nil)
	}
	if ttlSeconds <= 0 {
		ttlSeconds = 300
	}
	return &redisSessionRuntime{
		client: client,
		ttl:    time.Duration(ttlSeconds) * time.Second,
		logger: logger,
	}
}

// ObservedSessionCount 复刻 getObservedGlobalSessionCount。
func (r *redisSessionRuntime) ObservedSessionCount(ctx context.Context) (int, error) {
	return r.countZSet(ctx, observedSessionsKey)
}

// ProviderSessionCounts 逐供应商计数（Node 侧是 Promise.all 的逐个计数，语义相同）。
func (r *redisSessionRuntime) ProviderSessionCounts(
	ctx context.Context,
	providerIDs []int64,
) (map[int64]int, error) {
	counts := make(map[int64]int, len(providerIDs))
	for _, providerID := range providerIDs {
		key := providerActiveSessionsKey(providerID)
		count, err := r.countZSet(ctx, key)
		if err != nil {
			// Node 侧单个供应商读失败是 fail-open（记日志、计 0），不牵连整张列表。
			r.logger.Warn("dashboard_provider_session_count_failed", map[string]any{
				"providerId": providerID,
				"error":      err.Error(),
			})
			counts[providerID] = 0
			continue
		}
		counts[providerID] = count
	}
	return counts, nil
}

// providerActiveSessionsKey 与 session-tracker.ts:445 的字面量一致（无 hash tag）。
func providerActiveSessionsKey(providerID int64) string {
	return "provider:" + strconv.FormatInt(providerID, 10) + ":active_sessions"
}

// countZSet 复刻 countFromZSet：先摘过期成员，再只数「info 键仍存在」的成员。
//
// 顺序与 Node 逐条对齐：
//  1. 键不存在 → 0（Node 的 `exists(key) !== 1`）。
//  2. 类型不是 ZSET → 删除该键并返回 0（Node 对历史遗留的 Set 数据就是这么自愈的）。
//  3. 取过期成员（供应商键才取，用于同步引用计数 HASH）→ 从 ZSET 摘除 → 从引用计数里删。
//  4. 取全部成员 → 逐个 EXISTS session:{id}:info → 存在的才计数。
//
// 为什么第 4 步不能省：ZSET 的成员是「会话身份」，而会话本身可能已被终止而 info 键已删；
// 不校验会把已终止的会话算成并发。
func (r *redisSessionRuntime) countZSet(ctx context.Context, key string) (int, error) {
	exists, err := r.client.Exists(ctx, key).Result()
	if err != nil {
		return 0, err
	}
	if exists != 1 {
		return 0, nil
	}
	keyType, err := r.client.Type(ctx, key).Result()
	if err != nil {
		return 0, err
	}
	if keyType != "zset" {
		if delErr := r.client.Del(ctx, key).Err(); delErr != nil {
			return 0, delErr
		}
		return 0, nil
	}

	cutoff := time.Now().Add(-r.ttl).UnixMilli()
	refsKey := providerRefsKey(key)
	if refsKey != "" {
		expired, rangeErr := r.client.ZRangeByScore(ctx, key, &redis.ZRangeBy{
			Min: "-inf",
			Max: strconv.FormatInt(cutoff, 10),
		}).Result()
		if rangeErr != nil {
			return 0, rangeErr
		}
		if len(expired) > 0 {
			if hdelErr := r.client.HDel(ctx, refsKey, expired...).Err(); hdelErr != nil {
				return 0, hdelErr
			}
		}
	}
	if err := r.client.ZRemRangeByScore(ctx, key, "-inf",
		strconv.FormatInt(cutoff, 10)).Err(); err != nil {
		return 0, err
	}

	members, err := r.client.ZRange(ctx, key, 0, -1).Result()
	if err != nil {
		return 0, err
	}
	if len(members) == 0 {
		return 0, nil
	}

	pipe := r.client.Pipeline()
	commands := make([]*redis.IntCmd, 0, len(members))
	for _, member := range members {
		commands = append(commands, pipe.Exists(ctx,
			sessionInfoKeyPrefix+member+sessionInfoKeySuffix))
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return 0, err
	}
	count := 0
	for _, command := range commands {
		if value, cmdErr := command.Result(); cmdErr == nil && value == 1 {
			count++
		}
	}
	return count, nil
}

// ObservedSessionIdentities 复刻 getObservedActiveSessions（session-tracker.ts:678-709）。
//
// 与 countZSet 的两处差别都照抄 Node：
//   - 这里**不动**供应商引用计数 HASH（那条链只在供应商维度计数的路径上）。
//   - 末尾的 EXISTS 过滤是「成员仍有效」的第二道校验，缺了会把已终止会话当作活跃。
func (r *redisSessionRuntime) ObservedSessionIdentities(ctx context.Context) ([]string, error) {
	exists, err := r.client.Exists(ctx, observedSessionsKey).Result()
	if err != nil {
		return nil, err
	}
	if exists != 1 {
		return nil, nil
	}
	keyType, err := r.client.Type(ctx, observedSessionsKey).Result()
	if err != nil {
		return nil, err
	}
	if keyType != "zset" {
		if delErr := r.client.Del(ctx, observedSessionsKey).Err(); delErr != nil {
			return nil, delErr
		}
		return nil, nil
	}

	cutoff := time.Now().Add(-r.ttl).UnixMilli()
	if err := r.client.ZRemRangeByScore(ctx, observedSessionsKey, "-inf",
		strconv.FormatInt(cutoff, 10)).Err(); err != nil {
		return nil, err
	}
	identities, err := r.client.ZRange(ctx, observedSessionsKey, 0, -1).Result()
	if err != nil {
		return nil, err
	}
	if len(identities) == 0 {
		return nil, nil
	}

	pipe := r.client.Pipeline()
	commands := make([]*redis.IntCmd, 0, len(identities))
	for _, identity := range identities {
		commands = append(commands, pipe.Exists(ctx,
			sessionInfoKeyPrefix+identity+sessionInfoKeySuffix))
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		return nil, err
	}
	active := make([]string, 0, len(identities))
	for index, command := range commands {
		if value, cmdErr := command.Result(); cmdErr == nil && value == 1 {
			active = append(active, identities[index])
		}
	}
	return active, nil
}

// providerRefsKey 复刻 getProviderActiveSessionRefsKey：非供应商键返回空串。
func providerRefsKey(activeSessionsKey string) string {
	match := providerActiveSessionsPattern.FindStringSubmatch(activeSessionsKey)
	if match == nil {
		return ""
	}
	return "provider:" + match[1] + ":active_session_refs"
}
