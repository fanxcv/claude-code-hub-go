package limit

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ratelimit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/session"
)

// DefaultSessionTTL 与 Node 侧 SESSION_TTL 的默认值一致（秒）。
const DefaultSessionTTL = 300

// SessionTracker 是并发会话的原子「检查 + 追踪」层。
//
// 为什么必须原子：读一次计数再写一次追踪会被并发击穿——N 个请求同时读到 limit-1 就一起放行。
// Node 侧为此把「清理过期成员 + 判上限 + ZADD + 续 TTL」整体放进 Lua，本包复用同一批脚本。
type SessionTracker struct {
	client *ratelimit.Client
	ttl    time.Duration
	log    *logx.Logger
}

// NewSessionTracker 组装并发会话追踪层；ttl <= 0 时取 DefaultSessionTTL。
func NewSessionTracker(client *ratelimit.Client, ttl time.Duration, logger *logx.Logger) *SessionTracker {
	if logger == nil {
		logger = logx.New(nil)
	}
	if ttl <= 0 {
		ttl = time.Duration(DefaultSessionTTL) * time.Second
	}
	return &SessionTracker{client: client, ttl: ttl, log: logger}
}

// Ready 报告 Redis 是否可用。
func (t *SessionTracker) Ready() bool {
	return t.client != nil && t.client.Raw() != nil
}

// KeyUserSessionResult 是 Key/User 并发判定的结果。
type KeyUserSessionResult struct {
	Allowed     bool
	KeyCount    int
	UserCount   int
	TrackedKey  bool
	TrackedUser bool
	// RejectedBy 取 key 或 user；仅在 Allowed 为 false 时非空。
	RejectedBy string
	// Current 与 Limit 是被拒维度的实际计数与上限（Node 侧 reasonParams）。
	Current int
	Limit   int
}

// CheckAndTrackKeyUserSession 复刻 checkAndTrackKeyUserSession：一次判定同时覆盖 Key 与 User 维度。
//
// 已追踪的会话即使在达到上限后继续请求也放行（ZSCORE 命中即绕过上限判定），
// 否则同一会话的后续轮次会被自己锁死。
func (t *SessionTracker) CheckAndTrackKeyUserSession(ctx context.Context, keyID, userID int64, sessionID string, keyLimit, userLimit int) (KeyUserSessionResult, error) {
	// 两维都无上限：直接放行且不追踪（Node 侧同样不记账，由其它路径负责观测）。
	if keyLimit <= 0 && userLimit <= 0 {
		return KeyUserSessionResult{Allowed: true}, nil
	}
	if !t.Ready() {
		t.log.Warn("limit.sessions.redis_unavailable", map[string]any{"note": "并发会话检查 Fail Open"})
		return KeyUserSessionResult{Allowed: true}, nil
	}

	values, err := t.eval(ctx, "CHECK_AND_TRACK_KEY_USER_SESSION",
		[]string{ActiveSessionsGlobalKey(), KeyActiveSessionsKey(keyID), UserActiveSessionsKey(userID)},
		[]any{sessionID, keyLimit, userLimit, nowMillisDefault(), t.ttl.Milliseconds()},
	)
	if err != nil {
		t.log.Error("limit.sessions.check_failed", map[string]any{"error": err.Error(), "keyId": keyID, "userId": userID})
		return KeyUserSessionResult{Allowed: true}, nil
	}
	if len(values) < 6 {
		t.log.Error("limit.sessions.reply_shape", map[string]any{"len": len(values)})
		return KeyUserSessionResult{Allowed: true}, nil
	}

	result := KeyUserSessionResult{
		Allowed:     values[0] != 0,
		KeyCount:    int(values[2]),
		TrackedKey:  values[3] == 1,
		UserCount:   int(values[4]),
		TrackedUser: values[5] == 1,
	}
	if result.Allowed {
		return result, nil
	}

	// rejectedBy：Lua 用 1 表示 Key、2 表示 User；其它取值按 User 处理（与 Node 的三元表达式一致）。
	if values[1] == 1 {
		result.RejectedBy = string(EntityKey)
		result.Current, result.Limit = result.KeyCount, keyLimit
	} else {
		result.RejectedBy = string(EntityUser)
		result.Current, result.Limit = result.UserCount, userLimit
	}
	return result, nil
}

// KeySessionCount 是 Key 级活跃会话数的**只读**计数，复刻 getKeySessionCount
// （src/lib/session-tracker.ts:410-440，计数过程在 countFromZSet :723-770）。
//
// 为什么必须只读：管理面读档（getKeyLimitUsage / getKeyQuotaUsage）只展示
// `concurrentSessions.current`，而本包其它入口都会写并发额度——一次纯查询不该占住名额。
//
// 三步与 Node 逐条对应：把过期成员按 TTL 剪掉（不剪会把已终止的会话算成活跃）、取余下成员、
// 再逐个确认 `session:{id}:info` 仍在（ZSET 成员可能是残留：键过期而成员未清）。
// provider 维度的 refs 清理（Node 在 countFromZSet 里顺带 hdel）只对 provider 键有意义，
// 而 key/user/global 三类键没有 refs 哈希，故此处不做。
//
// 出错时返回 error 而不是 0：Node 把异常吞成 0（fail-open），那是**展示**语义，由调用方
// （管理面）决定是否按 0 作答并留痕，本层不替它决定。
func (t *SessionTracker) KeySessionCount(ctx context.Context, keyID int64) (int, error) {
	if keyID <= 0 || !t.Ready() {
		// Node：redis.status !== "ready" 时直接返回 0（不是异常）。
		return 0, nil
	}
	return t.activeSessionCount(ctx, KeyActiveSessionsKey(keyID), "key", keyID)
}

// UserSessionCount 是 User 级活跃会话数的**只读**计数，复刻 getUserSessionCount
// （src/lib/session-tracker.ts:476-501）。
//
// 为什么要单列：自服务页（/api/v1/me/quota）的 userCurrentConcurrentSessions 取的是
// **User 维度**的活跃集合，而不是该用户各密钥计数之和——两者在「同一会话换密钥续用」时
// 会给出不同的数（User 集合去重，密钥计数各算一次）。该 ZSET 由数据面维护
// （internal/session/tracker.go:42 的 zadd），故此处只需读。
//
// 语义与 KeySessionCount 逐条一致（含「非 zset 键即删并返回 0」与过期成员剪枝）。
func (t *SessionTracker) UserSessionCount(ctx context.Context, userID int64) (int, error) {
	if userID <= 0 || !t.Ready() {
		return 0, nil
	}
	return t.activeSessionCount(ctx, UserActiveSessionsKey(userID), "user", userID)
}

// activeSessionCount 是 Key/User 两个维度共用的计数过程。
//
// kindID 只用于日志归属（keyId/userId 二选一），键与语义全由 key 决定。
func (t *SessionTracker) activeSessionCount(
	ctx context.Context,
	key string,
	kind string,
	kindID int64,
) (int, error) {
	raw := t.client.Raw()

	exists, err := raw.Exists(ctx, key).Result()
	if err != nil {
		return 0, err
	}
	if exists == 0 {
		return 0, nil
	}

	// 旧格式（Set 实现）残留：Node 直接删键并返回 0（session-tracker.ts:417-424）。
	redisKind, err := raw.Type(ctx, key).Result()
	if err != nil {
		return 0, err
	}
	if redisKind != "zset" {
		t.log.Warn("limit.sessions.key_type_mismatch", map[string]any{
			kind: kindID, "type": redisKind,
		})
		if delErr := raw.Del(ctx, key).Err(); delErr != nil {
			return 0, delErr
		}
		return 0, nil
	}

	cutoff := time.Now().Add(-t.ttl).UnixMilli()
	if err := raw.ZRemRangeByScore(ctx, key, "-inf", strconv.FormatInt(cutoff, 10)).Err(); err != nil {
		return 0, err
	}
	members, err := raw.ZRange(ctx, key, 0, -1).Result()
	if err != nil {
		return 0, err
	}
	if len(members) == 0 {
		return 0, nil
	}

	// Node 用一条 pipeline 批量 EXISTS：逐个往返会把读档延迟放大到成员数倍。
	pipe := raw.Pipeline()
	commands := make([]*redis.IntCmd, 0, len(members))
	for _, member := range members {
		commands = append(commands, pipe.Exists(ctx, session.InfoKey(member)))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return 0, err
	}
	count := 0
	for _, command := range commands {
		if value, cmdErr := command.Result(); cmdErr == nil && value > 0 {
			count++
		}
	}
	return count, nil
}

// ProviderSessionResult 是供应商并发判定的结果。
type ProviderSessionResult struct {
	Allowed bool
	Count   int
	// Tracked 为 true 表示本次是新增追踪（ZSET 里原来没有这个会话）。
	Tracked bool
	// Referenced 为 true 表示本次拿到了一个释放引用。
	Referenced bool
}

// CheckAndTrackProviderSession 复刻 checkAndTrackProviderSession：转发前先占住供应商并发额度。
//
// 先占再转发是刻意的：只有这样，上游失败后的回退决策才是原子的（否则要靠 TTL 兜底，
// 供应商故障时会瞬间堆满 active_sessions）。
func (t *SessionTracker) CheckAndTrackProviderSession(ctx context.Context, providerID int64, sessionID string, limit int) (ProviderSessionResult, error) {
	if limit <= 0 {
		return ProviderSessionResult{Allowed: true}, nil
	}
	if !t.Ready() {
		t.log.Warn("limit.sessions.redis_unavailable", map[string]any{"note": "供应商并发检查 Fail Open"})
		return ProviderSessionResult{Allowed: true}, nil
	}

	values, err := t.eval(ctx, "CHECK_AND_TRACK_SESSION",
		[]string{ProviderActiveSessionsKey(providerID), ProviderSessionRefsKey(providerID)},
		[]any{sessionID, limit, nowMillisDefault(), t.ttl.Milliseconds()},
	)
	if err != nil {
		t.log.Error("limit.sessions.provider_check_failed", map[string]any{"error": err.Error(), "providerId": providerID})
		return ProviderSessionResult{Allowed: true}, nil
	}
	if len(values) < 4 {
		t.log.Error("limit.sessions.reply_shape", map[string]any{"len": len(values)})
		return ProviderSessionResult{Allowed: true}, nil
	}

	result := ProviderSessionResult{
		Allowed:    values[0] != 0,
		Count:      int(values[1]),
		Tracked:    values[2] == 1,
		Referenced: values[3] == 1,
	}
	if !result.Allowed {
		result.Tracked = false
		result.Referenced = false
	}
	return result, nil
}

// ReleaseProviderSession 复刻 releaseProviderSession：一个引用归零才真正释放并发额度。
func (t *SessionTracker) ReleaseProviderSession(ctx context.Context, providerID int64, sessionID string) (removed int, remainingRefs int, err error) {
	if providerID <= 0 || sessionID == "" || !t.Ready() {
		return 0, 0, nil
	}
	values, evalErr := t.eval(ctx, "RELEASE_PROVIDER_SESSION",
		[]string{ProviderActiveSessionsKey(providerID), ProviderSessionRefsKey(providerID)},
		[]any{sessionID},
	)
	if evalErr != nil {
		t.log.Error("limit.sessions.release_failed", map[string]any{"error": evalErr.Error(), "providerId": providerID})
		return 0, 0, evalErr
	}
	if len(values) < 2 {
		return 0, 0, fmt.Errorf("limit: RELEASE_PROVIDER_SESSION 返回 %d 项", len(values))
	}
	return int(values[0]), int(values[1]), nil
}

// ForceTerminateProviderSession 复刻 forceTerminateProviderSession：清掉某物理会话在供应商上的
// 全部引用（终态清理路径）。
func (t *SessionTracker) ForceTerminateProviderSession(ctx context.Context, providerID int64, sessionID string) (removedSession int, removedRefs int, err error) {
	if providerID <= 0 || sessionID == "" || !t.Ready() {
		return 0, 0, nil
	}
	values, evalErr := t.eval(ctx, "FORCE_TERMINATE_PROVIDER_SESSION",
		[]string{ProviderActiveSessionsKey(providerID), ProviderSessionRefsKey(providerID)},
		[]any{sessionID},
	)
	if evalErr != nil {
		t.log.Error("limit.sessions.force_terminate_failed", map[string]any{"error": evalErr.Error(), "providerId": providerID})
		return 0, 0, evalErr
	}
	if len(values) < 2 {
		return 0, 0, fmt.Errorf("limit: FORCE_TERMINATE_PROVIDER_SESSION 返回 %d 项", len(values))
	}
	return int(values[0]), int(values[1]), nil
}

// ForceTerminateKeyUserSession 复刻 forceTerminateKeyUserSession：把某物理会话从
// global/key/user 三个并发索引里同时摘掉。
func (t *SessionTracker) ForceTerminateKeyUserSession(ctx context.Context, keyID, userID int64, sessionID string) (removed int, err error) {
	if keyID <= 0 || userID <= 0 || sessionID == "" || !t.Ready() {
		return 0, nil
	}
	values, evalErr := t.eval(ctx, "FORCE_TERMINATE_KEY_USER_SESSION",
		[]string{ActiveSessionsGlobalKey(), KeyActiveSessionsKey(keyID), UserActiveSessionsKey(userID)},
		[]any{sessionID},
	)
	if evalErr != nil {
		t.log.Error("limit.sessions.force_terminate_failed", map[string]any{"error": evalErr.Error(), "keyId": keyID, "userId": userID})
		return 0, evalErr
	}
	var total int
	for _, value := range values {
		total += int(value)
	}
	return total, nil
}

// eval 执行脚本并把返回值归一为整数数组。
func (t *SessionTracker) eval(ctx context.Context, constName string, keys []string, argv []any) ([]int64, error) {
	if t.client == nil {
		return nil, errors.New("limit: 脚本调用层缺失")
	}
	raw, err := t.client.EvalConst(ctx, constName, keys, argv)
	if err != nil {
		return nil, err
	}
	return toInt64Slice(raw)
}

// toInt64Slice 把 Lua 的数组返回值归一为 int64 切片。
//
// go-redis 会把 Lua 数组解成 []any，元素类型随数值来源而变（整数是 int64，浮点/字符串转换是
// 字符串），故两种都要接。
func toInt64Slice(raw any) ([]int64, error) {
	switch typed := raw.(type) {
	case nil:
		return nil, errors.New("limit: 脚本返回 nil")
	case []any:
		values := make([]int64, 0, len(typed))
		for _, item := range typed {
			parsed, err := toInt64(item)
			if err != nil {
				return nil, err
			}
			values = append(values, parsed)
		}
		return values, nil
	case []int64:
		return typed, nil
	default:
		return nil, fmt.Errorf("limit: 脚本返回值类型不支持 %T", raw)
	}
}

func toInt64(raw any) (int64, error) {
	switch typed := raw.(type) {
	case int64:
		return typed, nil
	case int:
		return int64(typed), nil
	case float64:
		return int64(typed), nil
	case string:
		parsed, err := parseFloatOrZeroErr(typed)
		if err != nil {
			return 0, err
		}
		return int64(parsed), nil
	case []byte:
		parsed, err := parseFloatOrZeroErr(string(typed))
		if err != nil {
			return 0, err
		}
		return int64(parsed), nil
	default:
		return 0, fmt.Errorf("limit: 脚本返回值元素类型不支持 %T", raw)
	}
}
