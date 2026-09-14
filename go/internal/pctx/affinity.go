package pctx

import "context"

// AffinityWriteback 是亲和终态写回的中性接口。
//
// 为什么是接口：亲和键的形制、Lua 与 generation fence 都属选路包（internal/route）的实现细节，
// 而写回时机在终态层（internal/terminal）。本包只持有「谁能在终态写亲和」这个能力句柄，
// 不引入选路包类型（pctx 不得依赖 route，见 go-data-plane-takeover-plan 的分层）。
//
// 参数只有上下文与供应商 id：scope tag、指纹与 generation 由选路包在提名时固化进实现里，
// 终态层不需要、也无从重建它们。
type AffinityWriteback interface {
	// RecordWinner 在成功终态提交后写回 tip 绑定；返回 false 表示未写（本次不该写，或被 fence 拒绝）。
	RecordWinner(ctx context.Context, providerID int64) bool
	// TombstoneOnFailure 在供应商侧失败时写短 TTL 墓碑；返回 false 表示未写。
	TombstoneOnFailure(ctx context.Context, failedProviderID int64) bool
}

// AffinityIdentity 是本次请求的亲和身份事实。
//
// 为什么不是 route 的类型：与 AffinityWriteback 同理，pctx 不得依赖选路包（分层约束）。
// ScopeTag 与 Fingerprint 都由选路包在提名前算出，这里只存两个事实。
type AffinityIdentity struct {
	ScopeTag    string
	Fingerprint string
}

// SetAffinityIdentity 装入本次请求的亲和身份事实（守卫链选路时调用）。
//
// 它与 SetAffinityWriteback 的触发条件**不同**：写回要求查找真跑通（Redis 可用），
// 而身份事实只要求「指纹链可算 + 亲和参与」，与 Redis 是否可达无关——Node 的
// `session.affinity` 就是后者，故 Redis 故障时日志里依旧是「前缀亲和身份」。
// 传空 scopeTag 视为未参与，不装入。
func (c *Context) SetAffinityIdentity(scopeTag, fingerprint string) {
	if c == nil || scopeTag == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.affinityIdentity = AffinityIdentity{ScopeTag: scopeTag, Fingerprint: fingerprint}
	c.affinityIdentitySet = true
}

// AffinityIdentity 取本次请求的亲和身份事实。
//
// 第二个返回值为 false 表示本次请求不按前缀亲和归属（开关关闭、不可指纹化、
// 守卫链未接线），调用方据此写 session_identity_kind = "session_id"，而不是推断。
func (c *Context) AffinityIdentity() (AffinityIdentity, bool) {
	if c == nil {
		return AffinityIdentity{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.affinityIdentitySet {
		return AffinityIdentity{}, false
	}
	return c.affinityIdentity, true
}

// SetAffinityWriteback 装入本次请求的亲和写回能力（守卫链选路时调用）。
//
// 同一请求只装一次，且必须早于任何终态结算：终态层读不到它就等同于「本次不写亲和」。
// 传 nil 不做任何事（亲和未参与时守卫链直接不调用）。
func (c *Context) SetAffinityWriteback(writeback AffinityWriteback) {
	if c == nil || writeback == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.affinity = writeback
}

// AffinityWriteback 取本次请求的亲和写回能力。
//
// 第二个返回值为 false 表示本次请求不写亲和（开关关闭、无法指纹化、查找不可用，
// 或守卫链未接线）。调用方据此跳过，而不是推断。
func (c *Context) AffinityWriteback() (AffinityWriteback, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.affinity == nil {
		return nil, false
	}
	return c.affinity, true
}
