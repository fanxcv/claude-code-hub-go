package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"
)

// 本文件是会话**观测写侧**：复刻 SessionTracker 与 SessionManager 里「往 Redis 写会话状态」
// 的那一半（只读面见 observed_read.go）。
//
// 为什么写侧不能缺：管理面的 sessions 列表与详情都从这批键读——列表的「并发数」读
// observed_session:{id}:concurrent_count，活跃与否取决于 session:{id}:info 是否存在
// （见 observed_read.go 第 3 步的逐个 EXISTS）。数据面只沉淀账本不写这批键时，纯 Go 世界里
// 会话列表恒空、并发数恒 0，而 UI 不会报错，只是静默变空页。
//
// 唯一真源：src/lib/session-tracker.ts:88-190（trackSession / trackObservedSession /
// refreshObservedSession / 两个并发计数）与 src/lib/session-manager.ts:1805-1870
// （storeSessionInfo / updateSessionProvider）。
//
// 写失败策略与 Node 逐条一致：**只 warn 不阻断**。Node 这些函数全部 try/catch 吞异常，
// 绝不让 Redis 抖动把一条本来能成的代理请求打成 500——本包据此只返回 error 给调用方记日志，
// 由调用方决定（数据面记 warn 后继续）。

// sessionStatusInProgress 是会话初始状态（Node 的 storeSessionInfo 写的 status）。
const sessionStatusInProgress = "in_progress"

// observedConcurrentCountTTL 是并发计数的 TTL（Node 的 600 秒，比会话 TTL 长一倍）。
//
// 为什么比会话 TTL 长：计数是自增自减的，进程被杀会留下永久残留。让计数比会话先死会让
// 「上一个请求还在途、会话已被清」的窗口里计数归零，故 Node 刻意让它活得更久。
const observedConcurrentCountTTL = 600 * time.Second

// PublicSessionIdentity 复刻 buildPublicSessionIdentity（src/lib/request-identity.ts:49）。
//
// 客户端可控的保留前缀（pfx: / sid:）要做 key-bound 定长编码：否则一个自称 `pfx:...` 的
// 客户端能把自己的会话键写进亲和命名空间，或撑爆 varchar(64) 的 identity 列。普通会话 id
// 原样返回，保持既有展示与查询语义。
func PublicSessionIdentity(sessionID string, keyID int64) string {
	if sessionID == "" {
		return ""
	}
	if !hasReservedIdentityPrefix(sessionID) {
		return sessionID
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("session-identity:v1\x00%d\x00%s", keyID, sessionID)))
	return "sid:" + hex.EncodeToString(sum[:])[:32]
}

// hasReservedIdentityPrefix 判定保留前缀（与 Node 的 startsWith 两分支同判）。
func hasReservedIdentityPrefix(sessionID string) bool {
	return len(sessionID) >= 4 && (sessionID[:4] == "pfx:" || sessionID[:4] == "sid:")
}

// SessionInfo 是会话详情 Hash 的写入面（Node 的 SessionStoreInfo）。
type SessionInfo struct {
	UserName string
	UserID   int64
	KeyID    int64
	KeyName  string
	// Model 为空时写空串（Node 的 `info.model || ""`），不是省略字段。
	Model string
	// APIType 取值 "chat" | "codex"（Node 按 originalFormat === "openai" 判定）。
	APIType string
}

// StoreSessionInfo 复刻 SessionManager.storeSessionInfo（session-manager.ts:1808）。
//
// 字段与顺序照抄：userName / userId / keyId / keyName / model / apiType / startTime / status，
// 一次 HSET 写入并刷 TTL。**startTime 只在首次写入时给出**——Node 同样无条件覆盖：会话身份
// 在请求一开始就写了这一条，重复覆盖会把「会话起点」推后，故调用方必须只在请求开始时调用。
func (b *Binder) StoreSessionInfo(ctx context.Context, identity string, info SessionInfo) error {
	if !b.Ready() || identity == "" {
		return nil
	}
	values := map[string]any{
		"userName":  info.UserName,
		"userId":    strconv.FormatInt(info.UserID, 10),
		"keyId":     strconv.FormatInt(info.KeyID, 10),
		"keyName":   info.KeyName,
		"model":     info.Model,
		"apiType":   info.APIType,
		"startTime": strconv.FormatInt(time.Now().UnixMilli(), 10),
		"status":    sessionStatusInProgress,
	}
	key := InfoKey(identity)
	raw := b.client.rc.Raw()
	pipe := raw.Pipeline()
	pipe.HSet(ctx, key, values)
	pipe.Expire(ctx, key, b.sessionTTL())
	_, err := pipe.Exec(ctx)
	return err
}

// UpdateSessionProvider 复刻 SessionManager.updateSessionProvider（session-manager.ts:1840）。
//
// providerId 与 providerName 写在同一个 Hash 上并刷 TTL；providerName 为空也照写空串，
// 与 Node 的 `?? ""` 同判（列表页据此显示 "unknown" 是在读侧兜的）。
func (b *Binder) UpdateSessionProvider(
	ctx context.Context, identity string, providerID int64, providerName string,
) error {
	if !b.Ready() || identity == "" || providerID <= 0 {
		return nil
	}
	key := InfoKey(identity)
	raw := b.client.rc.Raw()
	pipe := raw.Pipeline()
	pipe.HSet(ctx, key, map[string]any{
		"providerId":   strconv.FormatInt(providerID, 10),
		"providerName": providerName,
	})
	pipe.Expire(ctx, key, b.sessionTTL())
	_, err := pipe.Exec(ctx)
	return err
}

// SetSessionStatus 写会话终态状态并刷新 TTL（Node 的 updateSessionUsage 里那一半）。
//
// 与 Node 的差异（**登记**）：Node 的 updateSessionUsage 同时写 `session:{id}:usage` Hash
// （inputTokens / outputTokens / costUsd / statusCode / errorMessage）。那些事实只存在于本次
// 请求的结算器里（token 与金额在终态才成形），本函数只承接 status。usage Hash 留在会话详情
// 面那一波一起补——那条端点（读侧）目前仍由 Node 承担，而 Node 读不到 usage 时的兜底正是
// `info.status`（buildSessionInfo: `usage.status || info.status || "in_progress"`）。
func (b *Binder) SetSessionStatus(ctx context.Context, identity string, status string) error {
	if !b.Ready() || identity == "" || status == "" {
		return nil
	}
	key := InfoKey(identity)
	raw := b.client.rc.Raw()
	pipe := raw.Pipeline()
	pipe.HSet(ctx, key, "status", status)
	pipe.Expire(ctx, key, b.sessionTTL())
	_, err := pipe.Exec(ctx)
	return err
}

// sessionTTL 是会话状态键的 TTL。
//
// 与观测窗口同源（observed_read.go 的 observedSessionTTL，都取 Node 的 SESSION_TTL）：
// 两处各存一个常量只会在环境变量改动时悄悄分叉。
func (b *Binder) sessionTTL() time.Duration {
	return b.observedSessionTTL()
}

// IncrementObservedConcurrentCount 复刻 incrementObservedConcurrentCount（session-tracker.ts:794）。
func (b *Binder) IncrementObservedConcurrentCount(ctx context.Context, identity string) error {
	return b.incrementCount(ctx, ObservedConcurrentCountKey(identity), identity != "")
}

// DecrementObservedConcurrentCount 复刻 decrementObservedConcurrentCount（session-tracker.ts:830）。
func (b *Binder) DecrementObservedConcurrentCount(ctx context.Context, identity string) error {
	return b.decrementCount(ctx, ObservedConcurrentCountKey(identity), identity != "")
}

// IncrementConcurrentCount 复刻 incrementConcurrentCount（session-tracker.ts:775）。
//
// 与观测计数是**两个不同的键**：这个按物理会话 id 计（session:{id}:concurrent_count），
// 观测计数按身份计。Node 两条都自增，读侧目前只读观测那条。
func (b *Binder) IncrementConcurrentCount(ctx context.Context, sessionID string) error {
	return b.incrementCount(ctx, ConcurrentCountKey(sessionID), sessionID != "")
}

// DecrementConcurrentCount 复刻 decrementConcurrentCount（session-tracker.ts:811）。
func (b *Binder) DecrementConcurrentCount(ctx context.Context, sessionID string) error {
	return b.decrementCount(ctx, ConcurrentCountKey(sessionID), sessionID != "")
}

// incrementCount 自增并刷 TTL（Node 的 INCR + EXPIRE 600）。
func (b *Binder) incrementCount(ctx context.Context, key string, valid bool) error {
	if !b.Ready() || !valid {
		return nil
	}
	raw := b.client.rc.Raw()
	pipe := raw.Pipeline()
	pipe.Incr(ctx, key)
	pipe.Expire(ctx, key, observedConcurrentCountTTL)
	_, err := pipe.Exec(ctx)
	return err
}

// decrementCount 自减，降到 0 或负数则删键（Node 的 DECR 后 `newCount <= 0` 即 DEL）。
//
// 为什么删而不是留 0：留 0 会让「列表里并发数 0 的会话」与「已收尾的会话」在键空间上无法区分，
// 而终止路径与清理都以键是否存在为准。
func (b *Binder) decrementCount(ctx context.Context, key string, valid bool) error {
	if !b.Ready() || !valid {
		return nil
	}
	raw := b.client.rc.Raw()
	remaining, err := raw.Decr(ctx, key).Result()
	if err != nil {
		return err
	}
	if remaining <= 0 {
		return raw.Del(ctx, key).Err()
	}
	return nil
}

// TrackSessionAndObserved 是一次请求开始时要写的三组键，合成一次 pipeline。
//
// 为什么合成：三条写都发生在守卫链通过之后的热路径上，分三次往返只会把这段延迟乘三；
// Node 侧也是三条独立调用但各自 pipeline，本函数把它们并成一次往返（可观察结果相同）。
//
// 与 Node 的**登记**差异：Node 在 session-guard 阶段跟踪物理会话前会先判
// `hasConcurrentSessionLimit`（Key/User 并发会话上限开启时必须在限流里做原子「检查+跟踪」），
// 本实现的调用点在**守卫链跑完之后**——限流步骤已经用同一组 ZSET 做过原子检查与跟踪，
// 此处补写不会把「自己」算进尚未判定的候选里，故无需那条判据。
func (b *Binder) TrackSessionAndObserved(
	ctx context.Context, sessionID string, keyID int64, userID int64, identity string,
) error {
	if !b.Ready() {
		return nil
	}
	now := time.Now()
	raw := b.client.rc.Raw()
	pipe := raw.Pipeline()
	if sessionID != "" && keyID > 0 {
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
	}
	if identity != "" {
		observedKey := ObservedGlobalActiveSessionsKey()
		pipe.ZAdd(ctx, observedKey, redisZ(now, identity))
		pipe.Expire(ctx, observedKey, sessionZSetHostTTL)
	}
	_, err := pipe.Exec(ctx)
	return err
}
