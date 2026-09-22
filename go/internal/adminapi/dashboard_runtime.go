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
	// ProviderInFlightCounts 批量读各供应商的在飞尝试数（键口径见 session.ProviderAttemptMember）。
	ProviderInFlightCounts(ctx context.Context, providerIDs []int64) (map[int64]int, error)
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

// ProviderInFlightCounts 批量读各供应商的**在飞尝试数**。
//
// 口径：成员是「会话身份 + 尝试 token」（session.ProviderAttemptMember），故**成员个数**就是
// 在飞尝试数——与写侧 limit.CheckAndTrackProviderAttempt 同一口径（用户 2026-09-22 裁决）。
// 不再做「逐个成员 EXISTS session:{id}:info」的第二道校验：尝试不是会话，那个会话键
// 对在飞请求数没有任何判定力，保留它只会把真实在飞数读小。
//
// 往返次数：旧实现逐个供应商串行调 countZSet（每空渠道 1 次、每活跃渠道 `TYPE`+过期扫描+
// 裁剪+取成员+EXISTS pipeline），几十个渠道就把一次 5 秒轮询放大成几十至数百次**串行**往返。
// 现改为三段 pipeline（往返次数固定为 3，与渠道数无关）：
//
//  1. 全渠道：取过期成员名 + 读键类型；
//  2. 有过期成员的渠道：同步引用计数 HASH（HDEL）并摘除过期成员；
//  3. 确为 zset 的渠道：取成员个数（即在飞尝试数）。
//
// 失败仍按既有口径 fail-open（记日志、计 0、不牵连整张列表）。
func (r *redisSessionRuntime) ProviderInFlightCounts(
	ctx context.Context,
	providerIDs []int64,
) (map[int64]int, error) {
	counts := make(map[int64]int, len(providerIDs))
	if len(providerIDs) == 0 {
		return counts, nil
	}
	cutoff := strconv.FormatInt(time.Now().Add(-r.ttl).UnixMilli(), 10)

	// 第 1 段：过期成员名（供第 2 段同步引用计数）+ 键类型（决定第 3 段要不要取数）。
	// 键不存在时两个命令都是空/ none，不是错误。
	probe := r.client.Pipeline()
	expiredCommands := make([]*redis.StringSliceCmd, len(providerIDs))
	typeCommands := make([]*redis.StatusCmd, len(providerIDs))
	for i, providerID := range providerIDs {
		key := providerActiveSessionsKey(providerID)
		expiredCommands[i] = probe.ZRangeByScore(ctx, key, &redis.ZRangeBy{Min: "-inf", Max: cutoff})
		typeCommands[i] = probe.Type(ctx, key)
	}
	if _, err := probe.Exec(ctx); err != nil && err != redis.Nil {
		// 整段失败：键级错误已落在各自 Cmd 上，按渠道逐条判也救不回整批，整批 fail-open。
		r.reportCountFailure(providerIDs, err)
		return counts, nil
	}

	// 第 2 段：只对「确有过期成员」的渠道同步清理引用计数 HASH + 摘除过期成员。
	// 非 zset 的历史遗留形态不在 ZSET 里、ZRangeByScore 会报类型错，故按 Cmd 判。
	cleanup := r.client.Pipeline()
	cleaned := 0
	for i, providerID := range providerIDs {
		command := expiredCommands[i]
		if command.Err() != nil || len(command.Val()) == 0 {
			continue
		}
		cleanup.HDel(ctx, providerRefsKey(providerActiveSessionsKey(providerID)), command.Val()...)
		cleanup.ZRemRangeByScore(ctx, providerActiveSessionsKey(providerID), "-inf", cutoff)
		cleaned++
	}
	if cleaned > 0 {
		if _, err := cleanup.Exec(ctx); err != nil && err != redis.Nil {
			// 清理失败不阻断读数：计数仍然正确（第 3 段的 ZCard 不受影响）。
			r.logger.Warn("dashboard_provider_session_cleanup_failed", map[string]any{"error": err.Error()})
		}
	}

	// 第 3 段：只对「键存在且是 zset」的渠道取成员个数。非 zset 是历史遗留形态
	// （Node 的自愈动作是删键），一并当作 0 并顺手清掉，不让它把后续轮询永远卡在类型错误上。
	active := make([]int64, 0, len(providerIDs))
	for i, providerID := range providerIDs {
		command := typeCommands[i]
		if command.Err() != nil {
			r.logCountFailure(providerID, command.Err())
			counts[providerID] = 0
			continue
		}
		switch command.Val() {
		case "zset":
			active = append(active, providerID)
		case "none":
			counts[providerID] = 0
		default:
			if delErr := r.client.Del(ctx, providerActiveSessionsKey(providerID)).Err(); delErr != nil {
				r.logCountFailure(providerID, delErr)
			}
			counts[providerID] = 0
		}
	}
	if len(active) == 0 {
		return counts, nil
	}

	card := r.client.Pipeline()
	cardCommands := make([]*redis.IntCmd, len(active))
	for i, providerID := range active {
		cardCommands[i] = card.ZCard(ctx, providerActiveSessionsKey(providerID))
	}
	if _, err := card.Exec(ctx); err != nil && err != redis.Nil {
		r.reportCountFailure(active, err)
		for _, providerID := range active {
			counts[providerID] = 0
		}
		return counts, nil
	}
	for i, providerID := range active {
		if command := cardCommands[i]; command.Err() != nil {
			r.logCountFailure(providerID, command.Err())
			counts[providerID] = 0
			continue
		}
		counts[providerID] = int(cardCommands[i].Val())
	}
	return counts, nil
}

// reportCountFailure 记一批渠道的读数失败；整批失败时逐渠道留痕（与旧口径同形，便于排查）。
func (r *redisSessionRuntime) reportCountFailure(providerIDs []int64, err error) {
	for _, providerID := range providerIDs {
		r.logCountFailure(providerID, err)
	}
}

// logCountFailure 记单个渠道的读数失败（fail-open：调用方计 0，不牵连整张列表）。
func (r *redisSessionRuntime) logCountFailure(providerID int64, err error) {
	r.logger.Warn("dashboard_provider_session_count_failed", map[string]any{
		"providerId": providerID,
		"error":      err.Error(),
	})
}

// providerActiveSessionsKey 与 session-tracker.ts:445 的字面量一致（无 hash tag）。
func providerActiveSessionsKey(providerID int64) string {
	return "provider:" + strconv.FormatInt(providerID, 10) + ":active_sessions"
}

// countZSet 复刻 countFromZSet：先摘过期成员，再只数「info 键仍存在」的成员。
//
// 仅供**观测集合**（{observed_sessions}:global）使用：那里的成员真的是会话身份，
// 「info 键存在」是有效的活性校验。供应商维度的在飞尝试数走 ProviderInFlightCounts
// （成员是尝试 token，info 校验对它没有判定力）。
//
// 顺序与 Node 逐条对齐：
//  1. 键不存在 → 0（Node 的 `exists(key) !== 1`）。
//  2. 类型不是 ZSET → 删除该键并返回 0（Node 对历史遗留的 Set 数据就是这么自愈的）。
//  3. 取过期成员（供应商键才取，用于同步引用计数 HASH）→ 从 ZSET 摘除 → 从引用计数里删。
//  4. 取全部成员 → 逐个 EXISTS session:{id}:info → 存在的才计数。
//
// 为什么第 4 步不能省：观测集合的成员是「会话身份」，而会话本身可能已被终止而 info 键已删；
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
