package route

import "context"

// affinityTombstoneReasonFailover 是墓碑原因的唯一取值，逐字对齐 Node
// affinity-recorder.ts 的 tombstone(..., "failover", ...)。
const affinityTombstoneReasonFailover = "failover"

// AffinityWriteback 是一次选路产生的亲和终态写回事实，对应 Node 的 session.affinity
// 与 affinity-recorder 的入参合集：写哪个 scope、哪个指纹、哪个 generation 在**选路时**就定下来，
// 终态层（internal/terminal）提交后直接调用，绝不自行重建 scope 或指纹。
//
// 零值（store 为 nil）表示本次请求不参与亲和：全部方法 no-op。
type AffinityWriteback struct {
	// store 是写入目标；nil 表示本次不写（亲和开关关闭、无法指纹化或查找不可用）。
	store *AffinityStore
	// ScopeTag 是亲和键作用域（密钥 × 格式 × 模型）。
	ScopeTag string
	// TipFP 是本次指纹链的最深边界；成功终态只写它（对话推进天然累积链条，无需写全窗口）。
	TipFP string
	// TipDepth 是 tip 的深度；0 表示 tip 落在系统段（本次没有会话消息）。
	// 此时不写绑定：与查找侧的 F_sys 排除保持一致（Node affinity-recorder.ts 的
	// `if (tip.depth === 0) return`）。
	TipDepth int
	// IdentityFP 与 Generation 是查找（或未命中时 ensure）得到的身份与代际；
	// 写回以 Generation 做 CAS，在途旧请求无法复活已终止的绑定。
	IdentityFP string
	Generation string
	// MatchedFP 是本次命中的绑定指纹；未命中或未提名时为空。
	MatchedFP string
	// TipPrefixBytes 是 tip 边界的前缀字节数。它供 F3b 缓存模拟列粗估「理论可命中缓存量」
	// （Node gate.ts：`Math.floor(tip.prefixBytes / 4)`），值在提名/查找时就已固定，
	// 终态层不重建。
	TipPrefixBytes int
	// NominatedProviderID 是被接受的亲和提名者；0 表示无提名。
	// 墓碑只对它写：失败者不是提名者时写墓碑，会让后续请求绕开一个健康供应商
	// （Node affinity-recorder.ts 的 tombstoneAffinityOnFailure 首段判定）。
	NominatedProviderID int64
}

// CacheScoreFacts 把「F3b 缓存模拟列」所需的事实交给调用方（数据面）。
//
// 为什么由选路包供给：scope 与两个指纹是**选路时固化**的事实，终态层无从重建。
// 为什么用方法而不是让调用方直接读字段：终态层只拿到 pctx 的中性接口
// （pctx.AffinityWriteback），不引入选路包类型；调用方用一条本地接口做类型断言即可，
// 既不动 pctx 的分层，也不把 route 的字段形状泄露到终态层。
func (w AffinityWriteback) CacheScoreFacts() (scopeTag, matchedFP, tipFP string, tipPrefixBytes int, hasTip bool) {
	return w.ScopeTag, w.MatchedFP, w.TipFP, w.TipPrefixBytes, w.TipDepth > 0 || w.TipFP != ""
}

// RecordWinner 在**成功终态提交之后**写回 tip 绑定（Node 的 recordAffinityWinner）。
//
// 调用点纪律（Node 侧同一纪律，犯一处就会让粘性指向错误的供应商）：
//   - 非流式：forwarder.ts:2522 的成功分支；
//   - 流式：response-handler.ts:5414 的 postTerminalSideEffects（计费落库之后）；
//   - 回放命中、竞速败者、失败重试**不得**调用。
//
// 返回 false 只有两类原因：本次不该写（无 store、tip 落在系统段、generation 缺失），
// 或 generation CAS 失败——后者正是 fence 要挡的情形，不报错、只放弃。
func (w AffinityWriteback) RecordWinner(ctx context.Context, winnerProviderID int64) bool {
	if w.store == nil || winnerProviderID <= 0 || w.TipDepth == 0 {
		return false
	}
	return w.store.Put(ctx, w.ScopeTag, w.TipFP, winnerProviderID, w.IdentityFP, w.Generation)
}

// TombstoneOnFailure 在供应商侧失败时对命中边界写短 TTL 墓碑（Node 的 tombstoneAffinityOnFailure）。
//
// 只对「提名者恰好就是失败者」写：这是防羊群的关键判据——故障供应商不是本次提名者时，
// 后续请求本来就不会撞向它。
func (w AffinityWriteback) TombstoneOnFailure(ctx context.Context, failedProviderID int64) bool {
	if w.store == nil || failedProviderID <= 0 || w.MatchedFP == "" ||
		w.NominatedProviderID == 0 || w.NominatedProviderID != failedProviderID {
		return false
	}
	return w.store.Tombstone(
		ctx, w.ScopeTag, w.MatchedFP, affinityTombstoneReasonFailover, w.IdentityFP, w.Generation,
	)
}
