package session

import (
	"context"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// sessionBindingWriteback 实现 pctx.SessionBindingWriteback：把本次请求的会话绑定
// 在终态提交后 CAS 指向获胜供应商，或在供应商侧失败后写冷却。
//
// 为什么实现在本包而不是 terminal：terminal 不得 import session（session → guard →
// terminal 成环，见 terminal/slow_rate_seam.go 的同一说明）。故本包实现中性接口，
// 守卫链把它装进 pctx，终态层只认 pctx 的接口。
//
// sessionID / keyID / generation 在构造时固化：terminal 无从重建它们（与亲和写回
// 固化 scope/指纹同理），而 expectedGeneration 必须是**选路那一刻**读到的值——
// 迟到写入被 generation fence 拒绝是设计要的语义，不是缺陷。
type sessionBindingWriteback struct {
	binder     *Binder
	sessionID  string
	keyID      int64
	generation string
	ttl        time.Duration
	log        *logx.Logger
}

// SessionBindingWriteback 构造本次会话的绑定写回能力；构造不出来时返回 nil。
//
// 构造不出来（binder 未就绪、keyID 非法、generation 为空）时返回 nil，调用方据此整段跳过：
// 与「未装配」同语义，不产生半可用的句柄。
func (a *SessionBinderAdapter) SessionBindingWriteback(
	sessionID string, keyID int64, generation string,
) pctx.SessionBindingWriteback {
	if a.binder == nil || !a.binder.Ready() || sessionID == "" || keyID <= 0 || generation == "" {
		return nil
	}
	return &sessionBindingWriteback{
		binder:     a.binder,
		sessionID:  sessionID,
		keyID:      keyID,
		generation: generation,
		ttl:        a.ttl,
		log:        a.log,
	}
}

// CompareAndSet 在成功终态提交后把绑定指向 winnerProviderID。
//
// 返回 false 的两种情形都不影响结算：本次不该写，或 CAS 被 generation fence 拒绝
// （表示其他请求已更新过绑定，覆盖更新的选择是错的——静默放弃，下次请求会读到新绑定）。
func (w *sessionBindingWriteback) CompareAndSet(ctx context.Context, providerID int64) bool {
	if w == nil || providerID <= 0 {
		return false
	}
	result, err := w.binder.CompareAndSet(
		ctx, w.sessionID, w.keyID, w.generation, providerID, w.ttlSeconds(),
	)
	if err != nil {
		w.warn("session.binding.writeback_failed", err)
		return false
	}
	if !result.OK {
		// 冲突属预期（并发请求）：只留痕，不报错、不改终态。
		w.warn("session.binding.writeback_conflict", nil, "conflictReason", result.ConflictReason)
		return false
	}
	return true
}

// CooldownOnFailure 在供应商侧失败后写会话级冷却键。
//
// 用 Clear 实现：它一次 Lua 同时做「清 provider 绑定 + 写冷却键」，正是设计稿 §4 要求的
// 「写冷却」——分两步做会在两步之间留下「绑定还在但已冷却」的窗口。
// expectedProviderID 取**失败的那一家**：只对「绑定恰好指向它」写冷却（防羊群，
// 与亲和墓碑的判据同源）。
//
// 两个必须同时成立的性质（读侧据此把本条与低速降权分开，见 `route.CooldownKind`）：
//   - **不看低速监控开关**：本动作是故障回避，与「渠道慢不慢」无关。若把它也挂上那个开关，
//     默认（监控关闭）渠道的故障冷却就会写了永不生效；
//   - **写入值是下一代 generation**（正整数，见 `lua/clear-session-binding.lua` 的 `SETEX`），
//     与低速写入侧的固定标记 `slow` 不重叠——这是读侧分辨两者的唯一依据。
func (w *sessionBindingWriteback) CooldownOnFailure(ctx context.Context, providerID int64) bool {
	if w == nil || providerID <= 0 {
		return false
	}
	result, err := w.binder.Clear(
		ctx, w.sessionID, w.keyID, w.generation, providerID,
		providerID, int(sessionCooldownTTL/time.Second), w.ttlSeconds(),
	)
	if err != nil {
		w.warn("session.binding.cooldown_failed", err)
		return false
	}
	if !result.OK {
		w.warn("session.binding.cooldown_conflict", nil, "conflictReason", result.ConflictReason)
		return false
	}
	return true
}

// ClearBinding 在「配置变更导致绑定失效」（停用、不支持模型）时清空绑定且不写冷却。
//
// 与 CooldownOnFailure 分开：故障该冷却，配置变更不该——冷却会把一家只是被停用的渠道
// 记成「慢」，等它再启用时会白背一段冷却。
//
// expectedProviderID 必须取**失效的那一家**。传 0 在 Lua 里表示「期望空绑定」
// （clear-session-binding.lua 直接拿它与当前 provider_id 逐字比对），
// 于是已有绑定时必得 provider_mismatch、清不掉——正好与用途相反。
func (w *sessionBindingWriteback) ClearBinding(ctx context.Context, providerID int64) bool {
	if w == nil || providerID <= 0 {
		return false
	}
	result, err := w.binder.Clear(
		ctx, w.sessionID, w.keyID, w.generation, providerID, 0, 0, w.ttlSeconds(),
	)
	if err != nil {
		w.warn("session.binding.clear_failed", err)
		return false
	}
	if !result.OK {
		// 冲突分两类留痕：fence 按设计拒绍（并发请求已改绑定/生成号、或本来就没绑定）
		// 不是故障，记 skipped；其余（键形制损坏、镜像不一致、键属于别的 key）记 conflict。
		// 不能一律静默：本次缺陷就是「清不掉却不报」被长久漏过的。
		if sessionClearBenignConflicts[result.ConflictReason] {
			w.warn("session.binding.clear_skipped", nil, "conflictReason", result.ConflictReason)
			return false
		}
		w.warn("session.binding.clear_conflict", nil, "conflictReason", result.ConflictReason)
		return false
	}
	return true
}

// sessionClearBenignConflicts 是「清绑定」里属正常 fenced 语义的冲突原因。
//
//   - canonical_missing：本来就没有绑定（幂等重放，或绑定已过期）——无可清。
//   - provider_mismatch：绑定此刻指向别家（并发请求已改），或已无 provider。
//   - generation_mismatch：并发请求已推进生成号，fence 按设计拒绍迟到写入。
//
// 三者都不是数据异常，记成故障只会把正常并发噪声成 warn。
var sessionClearBenignConflicts = map[string]bool{
	"canonical_missing":   true,
	"provider_mismatch":   true,
	"generation_mismatch": true,
}

func (w *sessionBindingWriteback) ttlSeconds() int {
	seconds := int(w.ttl / time.Second)
	if seconds <= 0 {
		return 1
	}
	return seconds
}

func (w *sessionBindingWriteback) warn(event string, err error, kv ...any) {
	if w.log == nil {
		return
	}
	fields := map[string]any{}
	if err != nil {
		fields["error"] = err.Error()
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if key, ok := kv[i].(string); ok {
			fields[key] = kv[i+1]
		}
	}
	w.log.Warn(event, fields)
}

// sessionCooldownTTL 是会话级供应商冷却时长，与设计稿 §4 的 60s 一致。
const sessionCooldownTTL = 60 * time.Second
