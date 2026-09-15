package pctx

// CodexSessionCompletion 是本次请求的 Codex 会话标识补全事实。
//
// 为什么放在上下文里：补全发生在守卫链（session 步骤，认证之后才允许读体），而审计条目要在
// **终态**随其它探针条目一次追加（本仓纪律：零额外每请求写入）。两者之间没有别的通道——
// 每一步的返回值都只服务本步，故用上下文承载这一个事实，与 SetAffinityIdentity 同一手法。
type CodexSessionCompletion struct {
	// Action 取 none / completed_missing_fields / generated_uuid_v7 / reused_fingerprint_cache。
	Action string
	// Source 取 header_session_id / header_x_session_id / body_prompt_cache_key /
	// fingerprint_cache / generated_uuid_v7。
	Source string
	// SessionID 是补全后（或复用）的会话标识。
	SessionID string
}

// SetCodexSessionCompletion 装入补全事实（仅当确实补写了字段时调用）。
func (c *Context) SetCodexSessionCompletion(completion CodexSessionCompletion) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.codexSessionCompletion = completion
	c.codexSessionCompletionSet = true
}

// CodexSessionCompletion 取补全事实；第二个返回值为 false 表示本次没有补全。
func (c *Context) CodexSessionCompletion() (CodexSessionCompletion, bool) {
	if c == nil {
		return CodexSessionCompletion{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.codexSessionCompletionSet {
		return CodexSessionCompletion{}, false
	}
	return c.codexSessionCompletion, true
}
