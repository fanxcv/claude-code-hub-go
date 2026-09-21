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
