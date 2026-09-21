package pctx

import "context"

// SessionBindingWriteback 是会话绑定终态写回的中性接口。
//
// 为什么是接口（与 AffinityWriteback 同理由）：绑定键的形制、Lua 与 generation fence 都属
// 会话包（internal/session）的实现细节，而写回时机在终态层（internal/terminal）。
// 本包只持有「谁能在终态写会话绑定」这个能力句柄，不引入会话包类型
// （terminal 也不得 import session：session → guard → terminal 成环）。
//
// 参数只有上下文与供应商 id：sessionID、keyID、generation 与 TTL 由会话包在选路时
// 固化进实现里，终态层不需要、也无从重建它们。
type SessionBindingWriteback interface {
	// CompareAndSet 在成功终态提交后把绑定指向该供应商；返回 false 表示未写
	// （本次不该写，或被 generation fence 拒绝——后者表示其他请求已更新过绑定，
	// 覆盖更新的选择是错的，静默放弃）。
	CompareAndSet(ctx context.Context, providerID int64) bool
	// CooldownOnFailure 在供应商侧失败后写会话级冷却（键 session-binding:v1:{tag}:provider:{id}:cooldown）；
	// 返回 false 表示未写。
	CooldownOnFailure(ctx context.Context, providerID int64) bool
	// ClearBinding 在「资源/配置类失效」时清空绑定且**不写冷却**。
	//
	// 设计稿 §4：模型不支持、渠道停用属配置决策而非故障，不该染污「低速」语义。
	// 返回 false 表示未写（被 generation fence 拒绝、或 Redis 失败）。
	ClearBinding(ctx context.Context) bool
}

// SetSessionBindingWriteback 装入本次请求的会话绑定写回能力（会话守卫步骤调用）。
//
// 与 SetAffinityWriteback 同纪律：同一请求只装一次，且必须早于任何终态结算；
// 终态层读不到它就等同于「本次不写会话绑定」。传 nil 不做任何事。
func (c *Context) SetSessionBindingWriteback(writeback SessionBindingWriteback) {
	if c == nil || writeback == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sessionBinding = writeback
}

// SessionBindingWriteback 取本次请求的会话绑定写回能力。
//
// 第二个返回值为 false 表示本次请求不写会话绑定（无会话身份、会话包未装配，
// 或守卫链未接线）。调用方据此跳过，而不是推断。
func (c *Context) SessionBindingWriteback() (SessionBindingWriteback, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sessionBinding == nil {
		return nil, false
	}
	return c.sessionBinding, true
}
