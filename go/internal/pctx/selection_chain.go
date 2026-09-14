package pctx

// 本文件承载「选择期」provider_chain 条目（链首）的交接。
//
// 为什么需要它：Node 的 decision chain 首项是**选择期**条目
// （`initial_selection` / `affinity_hit` / `session_reuse`），其后才是**尝试期**条目
// （`request_success` / `retry_failed` / `hedge_*`）。使用记录页的弹窗读的正是链首：
// `provider-chain-popover.tsx:522`（会话复用）、`:526`（亲和命中）、`:533`（决策上下文）。
// 少了首项，界面就看不到「本次是否命中了前缀亲和（渠道复用）」与选择的决策上下文，
// 只剩失败与竞速留痕——2026-09-13 生产 Go 时代的链首全部是尝试期条目，正是这个成因。
//
// 为什么存 JSON 而不是选路包的类型：pctx 不得依赖 internal/route（分层约束，同 affinity.go）。
// 条目由选路包用 `ChainItem()` 构造（字段集由黄金样本钉子钉住：route/chain_test.go），
// 数据面只把它排在尝试期条目之前、不解读字段——选路侧增删字段不需要动 pctx。

// SelectionChainEntry 是选择期 provider_chain 条目。
type SelectionChainEntry struct {
	// JSON 是 route.ChainItem() 的序列化结果。
	JSON []byte
}

// SetSelectionChainEntry 装入选择期链条目（守卫链选路成功后调用一次）。
//
// 传空视为未参与，不做任何事：宁可不写链首，也不写一个空条目——
// 界面对空条目的判定与缺失不同。
func (c *Context) SetSelectionChainEntry(entry []byte) {
	if c == nil || len(entry) == 0 {
		return
	}
	clone := make([]byte, len(entry))
	copy(clone, entry)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.selectionChainEntry = clone
}

// SelectionChainEntry 取选择期链条目。
//
// 第二个返回值为 false 表示本次请求没有选择期留痕（守卫链未接线或选路失败），
// 调用方据此跳过链首，而不是编造一条。
func (c *Context) SelectionChainEntry() ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.selectionChainEntry) == 0 {
		return nil, false
	}
	return c.selectionChainEntry, true
}
