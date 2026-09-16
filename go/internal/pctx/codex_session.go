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

// AddGatewayInjectedBodyField 登记一个**网关注入**（客户端原文里没有）的正文顶层字段名。
//
// 为何要单独记：跨线转换会把正文里客户端没有的字段也当成「客户端声明的约束」记进损失台账，
// 而 Codex 会话补全恰好会往正文写 `prompt_cache_key`——于是每个转换请求都凭空多一条损失，
// 客户端却从未提过这个字段（生产实测：每一行 +1）。判据只能是「客户端原文里是否出现」，
// 故由写正文的那一步（守卫链）把事实留在上下文里，供转换与审计读取。
func (c *Context) AddGatewayInjectedBodyField(name string) {
	if c == nil || name == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, existing := range c.gatewayInjectedBodyFields {
		if existing == name {
			return
		}
	}
	c.gatewayInjectedBodyFields = append(c.gatewayInjectedBodyFields, name)
}

// GatewayInjectedBodyFields 取网关注入的正文顶层字段名（副本，调用方可自由持有）。
func (c *Context) GatewayInjectedBodyFields() []string {
	if c == nil || len(c.gatewayInjectedBodyFields) == 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.gatewayInjectedBodyFields))
	copy(out, c.gatewayInjectedBodyFields)
	return out
}
