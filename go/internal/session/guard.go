package session

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// 守卫链适配：复刻 ProxySessionGuard 里与会话身份、绑定、序号有关的决策点。
//
// 与 Node 的差异（有意）：Codex session id 补全与 claude metadata 注入会改写正文，
// 属于请求改写，由入口波次在调用 Ensure 之前完成（guard 的 sessionStep 注释同样写明）。

// BinderOptions 是 SessionBinder 适配的构造参数。
type BinderOptions struct {
	// Client 是绑定脚本调用层，必填。
	Client *Binder
	// TTL 是会话 TTL；<=0 时取 DefaultBindingTTLSeconds。
	TTL time.Duration
	// Logger nil 时写 stderr。
	Logger *logx.Logger
}

// SessionBinderAdapter 实现 guard.SessionBinder：提取身份、分配/复用会话、取请求序号。
type SessionBinderAdapter struct {
	binder *Binder
	ttl    time.Duration
	log    *logx.Logger
}

// NewSessionBinderAdapter 组装适配器。
func NewSessionBinderAdapter(opts BinderOptions) *SessionBinderAdapter {
	if opts.Client == nil {
		opts.Client = &Binder{}
	}
	if opts.TTL <= 0 {
		opts.TTL = DefaultBindingTTLSeconds * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = logx.New(nil)
	}
	return &SessionBinderAdapter{binder: opts.Client, ttl: opts.TTL, log: opts.Logger}
}

// Ensure 实现 guard.SessionBinder。返回值写回上下文之外由实现自持（接口契约）。
//
// 流程（对应 Node session-guard 的 3/4/4.1 步）：
//  1. 提取客户端携带的 session id（metadata.user_id / session_id 或 Codex 路径）。
//  2. 有则滑动续期并直接复用；无则降级：正文哈希找既有会话（需通过 tenant 所有权
//     校验），找不到就生成新会话并记映射。
//  3. 取会话内请求序号（原子 INCR，Redis 不可用时时间戳降级）。
func (a *SessionBinderAdapter) Ensure(ctx context.Context, req guard.SessionRequest) (guard.SessionResult, error) {
	if a.binder == nil || !a.binder.Ready() {
		// Redis/脚本不可用：Node 在 getOrCreateSessionId 里降级为生成新会话（不查库），
		// 序号同样降级；守卫链继续放行，会话语义退化为「每请求独立」。
		a.log.Warn("session.ensure.redis_unavailable", map[string]any{"note": "降级生成会话"})
		return guard.SessionResult{SessionID: GenerateSessionID(), Sequence: fallbackSequence()}, nil
	}

	sessionID := ExtractClientSessionID(req.Body, req.Headers)
	if sessionID == "" {
		// 正文哈希降级：只在取得到有效哈希且 Redis 可用时尝试复用。
		hash := CalculateMessagesHash(req.Body["messages"])
		if hash != "" {
			existing, ok, err := a.lookupTenantSession(ctx, hash, req.KeyID)
			if err != nil {
				a.log.Warn("session.ensure.hash_lookup_failed", map[string]any{"error": err.Error()})
			} else if ok {
				sessionID = existing
				// 复用已绑定的会话：刷新最后活动时间（对应 Node 的 refreshSessionTTL）。
				if err := a.binder.MarkLastSeen(ctx, sessionID, a.ttl); err != nil {
					a.log.Warn("session.ensure.refresh_ttl_failed", map[string]any{"error": err.Error()})
				}
			}
		}
		// 没哈希或没找到既有会话：生成新会话并记录映射（Node 的 storeSessionMapping）。
		if sessionID == "" {
			sessionID = GenerateSessionID()
			if err := a.storeMapping(ctx, hash, sessionID, req.KeyID); err != nil {
				a.log.Warn("session.ensure.store_mapping_failed", map[string]any{"error": err.Error()})
			}
		}
	} else if err := a.binder.MarkLastSeen(ctx, sessionID, a.ttl); err != nil {
		a.log.Warn("session.ensure.refresh_ttl_failed", map[string]any{"error": err.Error()})
	}

	// 读一次会话绑定，把「粘到哪一家」与 CAS 基准交给选路层。
	//
	// 为何此处必读而不能靠 lookupTenantSession/storeMapping 的副作用：那两处的读发生在
	// **无客户端 session id** 的降级路径上，且返回值被丢弃；客户端自带 id 的主路径
	// 此前完全不碰绑定。两条路径都需要这份事实，故在此统一读一次（幂等，且顺带续期）。
	binding := a.readBindingFacts(ctx, sessionID, req.KeyID)

	sequence, err := a.NextSequence(ctx, sessionID, req.KeyID)
	if err != nil {
		a.log.Warn("session.ensure.sequence_failed", map[string]any{"error": err.Error()})
		sequence = fallbackSequence()
	}
	// AllowRawSession 为 false 时 Node 会注入 metadata.user_id（改写正文），属请求改写；
	// 本适配不做改写，留待入口波次。
	return guard.SessionResult{SessionID: sessionID, Sequence: sequence, Binding: binding}, nil
}

// readBindingFacts 读一次会话绑定并归一为 guard 的中性事实。
//
// 读失败不阻断请求（与 Node 的绑定读取 fail-open 同向）：绑定读不到时选路走前缀兜底
// 或加权随机，比「因为一次 Redis 抖动就 500」更合理。
func (a *SessionBinderAdapter) readBindingFacts(ctx context.Context, sessionID string, keyID int64) *guard.SessionBindingFacts {
	binding, err := a.binder.ReadOrReconcile(ctx, sessionID, keyID, a.ttlSeconds())
	if err != nil {
		a.log.Warn("session.ensure.binding_read_failed", map[string]any{"error": err.Error()})
		return nil
	}
	if !binding.OK {
		// 冲突（如 generation 不匹配、镜像不一致）：不阻断，但留一条可辨痕迹。
		a.log.Warn("session.ensure.binding_read_conflict", map[string]any{
			"conflictReason": binding.ConflictReason,
		})
		return nil
	}
	return &guard.SessionBindingFacts{
		KeyID:      keyID,
		Generation: binding.Snapshot.Generation,
		ProviderID: binding.Snapshot.ProviderID,
		Writeback:  a.SessionBindingWriteback(sessionID, keyID, binding.Snapshot.Generation),
	}
}

// lookupTenantSession 复刻 getOrCreateSessionId 的「哈希键存在 + tenant 所有权校验」分支。
//
// 两道门：先由 legacy owner 镜像证明租户归属，再做一次 read-or-reconcile 校验规范键与镜像
// 一致性并刷新完整绑定 TTL。第二道不能省：少了它就会出现「哈希命中后 owner 先过期」的
// 竞争，复用方拿到的是一个已经无法绑定的会话。任一道不过即视为不属本租户，调用方新建会话。
func (a *SessionBinderAdapter) lookupTenantSession(ctx context.Context, hash string, keyID int64) (string, bool, error) {
	raw := a.binder.client.rc.Raw()
	key := TenantContentHashSessionKey(keyID, hash)
	existing, err := raw.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		// 键不存在是**正常路径**（首见会话），不是故障：原注释「调用方按未找到处理」只说了
		// 一半——调用方拿到的 err 非空就走 warn，于是每次未命中都报一条
		// `session.ensure.hash_lookup_failed`（生产实测该 warn 的 error 值全是 redis: nil）。
		// 归入「未找到」语义后，真实 Redis 故障（超时、连接拒绝、命令错）仍照旧上抛。
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}

	// proveContentHashSessionOwnership：legacy owner 必须是当前 key。
	ownerKey := BuildBindingKeys(existing, keyID).LegacyOwner
	owner, err := raw.Get(ctx, ownerKey).Result()
	if err != nil {
		return "", false, nil // owner 不存在：不归属当前租户
	}
	if owner != strconv.FormatInt(keyID, 10) {
		return "", false, nil
	}

	binding, err := a.binder.ReadOrReconcile(ctx, existing, keyID, a.ttlSeconds())
	if err != nil {
		return "", false, nil
	}
	if !binding.OK {
		a.log.Warn("session.ensure.hash_mapping_reconcile_conflict", map[string]any{
			"conflictReason": binding.ConflictReason,
		})
		return "", false, nil
	}
	return existing, true, nil
}

// ttlSeconds 把适配器的 TTL 折算成脚本要求的秒数，至少 1 秒（0 会让脚本判 invalid_input）。
func (a *SessionBinderAdapter) ttlSeconds() int {
	seconds := int(a.ttl / time.Second)
	if seconds <= 0 {
		return 1
	}
	return seconds
}

// storeMapping 复刻 storeSessionMapping。
//
// 关键顺序：先建绑定，再写映射。Node 侧同样如此，理由是「映射必须先有可证明的租户归属」——
// legacy owner 镜像就是那份归属凭证；跳过建绑定会写下一个永远不会被复用（也不归属任何租户）
// 的哈希映射，降级复用路径于是永远不生效。绑定失败即不写映射。
// 失败不阻断请求（Node 侧同样容错）。
func (a *SessionBinderAdapter) storeMapping(ctx context.Context, hash, sessionID string, keyID int64) error {
	if hash == "" {
		return nil
	}

	binding, err := a.binder.ReadOrReconcile(ctx, sessionID, keyID, a.ttlSeconds())
	if err != nil {
		return err
	}
	if !binding.OK {
		a.log.Warn("session.ensure.binding_conflict", map[string]any{
			"conflictReason": binding.ConflictReason,
		})
		return nil
	}

	raw := a.binder.client.rc.Raw()
	pipe := raw.Pipeline()
	pipe.Set(ctx, TenantContentHashSessionKey(keyID, hash), sessionID, a.ttl)
	pipe.Set(ctx, LastSeenKey(sessionID), strconv.FormatInt(time.Now().UnixMilli(), 10), a.ttl)
	_, err = pipe.Exec(ctx)
	return err
}

// NextSequence 取会话内请求序号（复刻 getNextRequestSequence 的 Lua）。
//
// 序号键 INCR 并发安全；序号键 PERSIST 保证滑动窗口不因 TTL 过期重置计数；generation
// 键缺失时初始化为 "0"（Node 语义）。
func (a *SessionBinderAdapter) NextSequence(ctx context.Context, sessionID string, keyID int64) (int, error) {
	if a.binder == nil || !a.binder.Ready() {
		return fallbackSequence(), nil
	}
	raw := a.binder.client.rc.Raw()
	pipe := raw.Pipeline()
	seqKey := SeqKey(sessionID)
	incr := pipe.Incr(ctx, seqKey)
	pipe.Persist(ctx, seqKey)
	if _, err := pipe.Exec(ctx); err != nil {
		return fallbackSequence(), err
	}
	sequence := int(incr.Val())

	// generation 与 owner 工件键（session-manager 内联 Lua 的后半段）。
	generationKey := ResponseBodyGenerationKey(sessionID)
	pipe2 := raw.Pipeline()
	genGet := pipe2.Get(ctx, generationKey)
	pipe2.Expire(ctx, ownerRequestGenerationKey(sessionID, sequence), a.ttl)
	pipe2.Expire(ctx, seqKey, a.ttl)
	if _, err := pipe2.Exec(ctx); err != nil {
		return sequence, nil
	}
	if genGet.Err() != nil {
		// 键不存在或读取失败：初始化 generation="0"（Node 的 SETEX）。
		pipe3 := raw.Pipeline()
		pipe3.Set(ctx, generationKey, "0", a.ttl)
		pipe3.Set(ctx, RequestOwnerKey(sessionID, sequence), strconv.FormatInt(keyID, 10), a.ttl)
		pipe3.Set(ctx, ownerRequestGenerationKey(sessionID, sequence), "0", a.ttl)
		_, _ = pipe3.Exec(ctx)
	} else {
		pipe3 := raw.Pipeline()
		pipe3.Expire(ctx, generationKey, a.ttl)
		pipe3.Set(ctx, RequestOwnerKey(sessionID, sequence), strconv.FormatInt(keyID, 10), a.ttl)
		_, _ = pipe3.Exec(ctx)
	}
	if sequence <= 0 {
		return fallbackSequence(), nil
	}
	return sequence, nil
}

// ownerRequestGenerationKey 是 Node 侧 buildSessionRequestResponseBodyGenerationKey。
func ownerRequestGenerationKey(sessionID string, sequence int) string {
	return fmt.Sprintf("session:%s:req:%d:response-body-generation:v1", sessionID, sequence)
}

// fallbackSequence 是 Redis 不可用时的伪唯一序号（Node 侧时间戳 + 随机数降级）。
func fallbackSequence() int {
	var random [2]byte
	_, _ = rand.Read(random[:])
	jitter := int(uint16(random[0])<<8|uint16(random[1])) % 1000
	return int((time.Now().UnixMilli()%1000000)+int64(jitter)) + 1
}
