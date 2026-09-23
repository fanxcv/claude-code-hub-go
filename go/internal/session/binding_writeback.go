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
// 只写冷却键、**绑定一个字段都不动**（与 ClearBinding 的「清绑定」正相反：两者共用
// ProviderCooldownKey 的键形制，但语义不同）。为何不清：冷却的语义是「本会话 60 秒内
// 先绕开这家」，不是「忘掉这家」。清掉绑定会让本会话在冷却期内落到备用渠道、并把
// canonical CAS 过去——冷却到点也回不来，60 秒的临时冷却就此变成永久迁移
// （生产实测：冷却过期后 68 个请求 100% 走备用）。绑定留着，读侧按冷却键跳过该家、
// 并据「绑定仅因临时原因被跳过」抑制成功侧 CAS
// （route.SessionBindingBypassTransient → pctx.SessionBindingKeepReason，见 terminal/settle.go），
// 冷却过期即粘回。
//
// 两个必须同时成立的性质（读侧据此把本条与低速降权分开，见 `route.CooldownKind`）：
//   - **不看低速监控开关**：本动作是故障回避，与「渠道慢不慢」无关。若把它也挂上那个开关，
//     默认（监控关闭）渠道的故障冷却就会写了永不生效；
//   - **写入值不是固定标记 `slow`**（取本次请求的代际）：与低速写入侧的标记不重叠——
//     这是读侧分辨两者的唯一依据，而低速侧刻意不清绑定（recorder.go），本侧从 2026-09-24 起同。
//     断流冷却同样不重叠（写 UpstreamStreamCutCooldownMarker，见 cooldownOn）。
//
// 仍然只对「绑定恰好指向失败的那家」写（防羊群，与原 Lua 的 provider fence 同判据）。
// 不再看代际：那一半 fence 原本只是「同时旋转代际」的配套，绑定不动之后，
// 同会话并发成功请求推进会误杀本该写下的冷却（旧实现下这类误杀只记 warn，静默）。
func (w *sessionBindingWriteback) CooldownOnFailure(ctx context.Context, providerID int64) bool {
	return w.cooldownOn(ctx, providerID, w.generation)
}

// CooldownOnUpstreamStreamCut 在「上游在正文中途干净断流」后写会话级冷却键。
//
// 与 CooldownOnFailure 的差别**只在写入值**：本方法写固定标记（UpstreamStreamCutCooldownMarker），
// 故障冷却写本次代际。两者共用同一冷却键与 60 秒 TTL，fence 语义也逐字相同（只对绑定恰好
// 指向该家的情形写）。读侧只能按值分流，故这个标记是「断流冷却可在无替代候选时放行」
// 唯一的依据（见 route.CooldownKind）。
func (w *sessionBindingWriteback) CooldownOnUpstreamStreamCut(ctx context.Context, providerID int64) bool {
	return w.cooldownOn(ctx, providerID, UpstreamStreamCutCooldownMarker)
}

// cooldownOn 是两类冷却的共同实现：同一 fence（绑定恰好指向该家）+ 同一冷却键与 TTL，
// 只有写入值不同。
func (w *sessionBindingWriteback) cooldownOn(ctx context.Context, providerID int64, value string) bool {
	if w == nil || providerID <= 0 {
		return false
	}
	bound, err := w.binder.boundProvider(ctx, w.sessionID, w.keyID)
	if err != nil {
		w.warn("session.binding.cooldown_failed", err)
		return false
	}
	if bound != providerID {
		// 绑定此刻指向别家（并发请求已改绑）或本会话无绑定：不动冷却键。
		// 与清绑定侧的 skipped 同形：这是 fence 的正常语义，不是故障。
		w.warn("session.binding.cooldown_skipped", nil, "boundProviderID", bound)
		return false
	}
	if err := w.binder.writeProviderCooldown(
		ctx, w.sessionID, w.keyID, providerID, value, sessionCooldownTTL,
	); err != nil {
		w.warn("session.binding.cooldown_failed", err)
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

// UpstreamStreamCutCooldownMarker 是「上游中途断流」类冷却写进冷却键的固定值，与故障冷却
// （写本次代际）和低速冷却（写 "slow"，见 internal/slowrate）的值空间不相交。
//
// 为何必须是独立标记：两类冷却共用同一个键与 TTL，读侧（route）只能按值把它们分开；
// 用同一个值会让「断流冷却可在无替代候选时放行」无法实现。route 侧镜像一份常量，
// 由 route 包的 TestUpstreamStreamCutMarkerMirrorsWriteSide 逐字钉住。
const UpstreamStreamCutCooldownMarker = "stream_cut"
