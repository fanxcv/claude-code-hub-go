package route

import "context"

// SessionBindingSnapshot 是会话绑定的只读快照，供选路层判定会话粘性。
//
// 为什么是镜像类型而不是直接引用 session.BindingSnapshot：本包不得 import session
// （session → guard → route，反向引用立即成环；见 guard/adapters_route.go 的同一说明）。
// 两个结构的字段必须手工对齐——对齐点就是本文件与 session/binding.go 的 BindingSnapshot。
//
// 终态写回的能力句柄**不在这里**：它在 pctx（pctx.SessionBindingWriteback），因为终态层
// 只依赖 pctx 的中性接口，而 route 只负责「拿快照做判定」这一件事。
type SessionBindingSnapshot struct {
	SessionID  string
	KeyID      int64
	Generation string
	// ProviderID 为 0 表示空绑定（新会话或已清空）：此时不短路，仍从最小
	// effectivePriority 档开始选（设计稿裁决 D：新会话不继承任何历史绑定）。
	ProviderID int64
}

// nominateBySessionBinding 按会话绑定提名。
//
// 会话绑定是**第一优先级**：命中即短路整场选路，不再查前缀亲和、不再加权随机。
// 与前缀亲和的差别只有「拿谁当候选」——候选一律走 validateAffinityCandidate 的全套硬校验
// （熔断、停用、粘性 opt-out、分组、模型/格式、端点、客户端名单、活动时段、会话冷却），
// 因此熔断中的绑定不会把会话钉死在一家坏渠道上（设计稿 §8 风险二的缓解）。
// f3bCacheScoreFacts 为会话粘性路径供 F3b 缓存模拟列的事实。
//
// 为何需要它：F3b 的五列靠指纹链纯计算供数（设计稿裁决 C），而指纹链原先只在前缀提名里算。
// 会话粘性短路返回时本不带 AffinityWriteback，五列会全空——缓存效果报表随之失去会话粘性下
// 的全部样本。这里只做**纯本地计算**（Fingerprint 不碰 Redis），不造写回能力
// （store 留 nil ⇒ RecordWinner / TombstoneOnFailure 均 no-op），故不会多写任何亲和键。
//
// 返回 nil 表示本次无法供数（未装配亲和 / 不可指纹化）——调用方照旧不写那五列。
func (s *Selector) f3bCacheScoreFacts(req Request) *AffinityWriteback {
	if s.opts.Affinity == nil || req.KeyID == 0 || req.AffinityBody == nil || req.Format == "" {
		return nil
	}
	chain, ok := Fingerprint(req.AffinityBody, req.Format, s.opts.Affinity.window)
	if !ok {
		return nil
	}
	tip := chain.Tip()
	return &AffinityWriteback{
		ScopeTag: ScopeTag(req.KeyID, req.Format, req.Model),
		TipFP:    tip.FP,
		TipDepth: tip.Depth,
		// 会话粘性没有「命中的指纹」（粘性不看前缀）：留空后 cachescore 侧按既有优先级
		// 回落到 TipFingerprint 组兼容键，与设计稿 ⑤ 的描述一致。
		MatchedFP:      "",
		TipPrefixBytes: tip.PrefixBytes,
	}
}

func (s *Selector) nominateBySessionBinding(
	ctx context.Context,
	req Request,
	excluded map[int64]bool,
) (Provider, bool) {
	if req.SessionBinding == nil || req.SessionBinding.ProviderID == 0 {
		return Provider{}, false
	}
	provider, err := s.opts.Source.Provider(ctx, req.SessionBinding.ProviderID)
	if err != nil || provider == nil {
		return Provider{}, false
	}
	if !s.validateAffinityCandidate(ctx, *provider, req, excluded) {
		return Provider{}, false
	}
	return *provider, true
}

// SessionBindingBypass 说明「既有会话绑定为何未在本次被采用」，是终态能否改绑的唯一判据来源。
//
// 为什么必须区分：设计稿 §4 失效规则对熔断逐字明定「跳过该 provider、继续走后续层级
// （不写冷却、**不清空绑定**）」，表下注「熔断是暂时的，绑定保留；待熔断恢复后会话仍粘回去」。
// 若不加区分，绑定 provider 因熔断/会话冷却被跳过、备用成功之后，成功侧的 CAS 会把绑定
// 改写成备用——「待恢复仍粘回去」即为假，且此后每次熔断都把会话永久搬走一次。
//
// 它是零值安全的：零值（SessionBindingBypassNone）即「本次不抑制改绑」，与接线前的行为逐字一致。
type SessionBindingBypass int

const (
	// SessionBindingBypassNone 表示本次不抑制改绑。三种情形：无既有绑定、绑定被正常采用、
	// 或绑定因**结构性**原因被跳过。最后一种允许改绑是对的——旧绑定已结构性失效
	// （渠道停用、模型/端点不兼容），新 winner 才是该会话该去的地方。
	SessionBindingBypassNone SessionBindingBypass = iota
	// SessionBindingBypassTransient 表示既有绑定**仅因临时原因**被跳过：绑定必须保留，
	// 本次成功终态**不得**改绑。
	SessionBindingBypassTransient
)

// KeepsBinding 报告本次成功终态是否必须保留既有绑定。
//
// 接线层（guard 选路适配器）据它决定是否给 pctx 盖「不得改绑」的事实，终态层再据 pctx
// 跳过成功侧 CAS。判定收在这里一处，两侧不各算一遍。
func (b SessionBindingBypass) KeepsBinding() bool {
	return b == SessionBindingBypassTransient
}

// String 返回稳定标识，供日志与排障（勿当协议值或落库值）。
func (b SessionBindingBypass) String() string {
	if b == SessionBindingBypassTransient {
		return "transient"
	}
	return "none"
}

// sessionBindingBypass 由过滤留痕判定「既有绑定未被采用」是否属临时原因。
//
// 为何读留痕而不在 validateAffinityCandidate 里另算一遍：留痕（DecisionContext.FilteredProviders）
// 就是本次过滤的同一份结论，另算必然与它分叉（过滤链一改，两处就不同步）。
// 该家不在留痕里却有既有绑定 ⇒ 绑定指向的行已查不到（已删除/查询失败），属结构性失效，允许改绑。
func sessionBindingBypass(filtered []Filtered, binding *SessionBindingSnapshot) SessionBindingBypass {
	if binding == nil || binding.ProviderID == 0 {
		return SessionBindingBypassNone
	}
	for _, record := range filtered {
		if record.ID != binding.ProviderID {
			continue
		}
		if transientRejection(record.Reason) {
			return SessionBindingBypassTransient
		}
		return SessionBindingBypassNone
	}
	return SessionBindingBypassNone
}

// transientRejection 报告某条排除理由是否属**临时**（不需改任何配置即可能恢复）。
//
// 分界原则只一条：**不改配置就可能恢复的属临时**，绑定该留着等它回来；要改配置才恢复的
// （停用、模型/端点/格式不兼容、客户端名单）属结构性，允许改绑。逐条依据：
//
//	circuit_open          设计稿 §4 明定「跳过、不清空绑定、待恢复后仍粘回去」
//	slow_rate_cooldown    低速写侧只写冷却、不清绑定（设计稿 §4 同源语义）
//	schedule_inactive     活动时段按钟点恢复
//	rate_limited          金额/额度窗口按时间恢复
//	excluded              本次请求内已试过并失败（故障转移），不是该渠道的结构性结论
func transientRejection(reason Reason) bool {
	switch reason {
	case ReasonCircuitOpen, ReasonSlowRateCooldown, ReasonScheduleInactive, ReasonRateLimited, ReasonExcluded:
		return true
	default:
		return false
	}
}
