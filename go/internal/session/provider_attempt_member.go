package session

import "strings"

// providerAttemptMemberSeparator 分隔「会话身份」与「尝试 token」。
//
// 用 US（unit separator, 0x1F）而不是可见字符：会话身份可能是客户端给的
// （见 affinityIgnoreClientSessionId），可见分隔符有被用户内容撞上的风险。
// US 在标识符里实际不可能出现，故按**首个**分隔符切分即无歧义。
const providerAttemptMemberSeparator = "\x1f"

// ProviderAttemptMember 构造 provider 活跃集合的成员：**每次在飞尝试**一个成员。
//
// 为什么成员是尝试而不是会话：并发上限的语义是「在飞请求数（每尝试计）」（用户 2026-09-22
// 裁决）。按会话计时，同一会话的并行请求、竞速（hedge）与重叠尝试只占一个额度，
// 从而绕过渠道并发上限——而那正是 providers.limit_concurrent_sessions 设了不生效的成因之一。
//
// 为什么把会话身份放进成员：这个集合还有**另一个**消费方——管理面改供应商时按
// `provider:{id}:active_sessions` 取「该渠道下活跃的会话」并终止其粘性绑定
// （session.TerminateProviderSessionsBatch）。成员若只是不透明 token，那条路径会静默失效
// （它会把 token 当会话 id 去查绑定）。故会话身份随成员携带，用 SessionIDFromProviderAttemptMember
// 取回；**计数只按成员个数**，与会话身份无关。
func ProviderAttemptMember(sessionID, attemptToken string) string {
	return sessionID + providerAttemptMemberSeparator + attemptToken
}

// SessionIDFromProviderAttemptMember 从活跃集合成员取回会话身份。
//
// 取不到分隔符时原样返回（兼容按会话计的旧成员，也让函数对任意输入都是全函数）。
func SessionIDFromProviderAttemptMember(member string) string {
	before, _, found := strings.Cut(member, providerAttemptMemberSeparator)
	if !found {
		return member
	}
	return before
}
