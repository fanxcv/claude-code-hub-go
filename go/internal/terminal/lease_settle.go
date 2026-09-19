package terminal

import (
	"context"
	"strconv"

	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// 本文件是租约结算的**事件捕获旁路**：成本落库之后，把这次请求的实际成本结算到
// 判定时用过的预算租约上（Node 对应物 `settleLeaseBudgets`，挂在终态收尾链的副作用段）。
//
// 三条约束与 RollupRecorder / NewRowsNotifier 同构：
//
//  1. **失败不得影响结算**：接口没有返回值，实现必须自带降级与日志
//     （见 limit.Service.SettleLeases）。调用方在旁路前后不改变控制流。
//
//  2. **时机：成本写入成功之后**（不是终态提交之后）。租约是「DB 权威用量的一次切片」，
//     只有这笔成本真的进了账本，扣减才是对的账。若成本写失败仍扣减：账本没有这笔、
//     租约却少一片，而下次刷新按 DB 权威用量重新切片——少掉的那片不会回来，
//     窗口内的判定会比实际更严（白拒请求）。故成本写失败时本函数根本不会被调用。
//
//  3. **幂等由结算标记承载**：同一条请求会被结算多次——胜者成本一次（标记 = 行 id），
//     每条竞速输家成本各一次（标记 = `{行 id}:loser:{providerId}:{attempt}`，与 store
//     AddHedgeLoserCost 的去重键同构）。标记不同就不会互相吞掉，而重放/重试命中同一标记
//     即不再扣减；终态屏障同时挡住「未赢下终态的那一次」。故本旁路不另建进程内注册表。
//
//     **为什么输家要单独结算而不是等胜者那一笔**：store.UpdateWinnerCost 把 winner 成本与
//     **已入库**的输家成本求和写进 cost_usd，而输家引流在后台完成（见 forward 的 billLoser），
//     两种顺序都可能出现：输家先入库时胜者那一笔已经包含了它（那笔只传 winner 自己的成本，
//     故不会重复扣），输家后入库时由它自己的标记补扣。按各自增量分开结算，两种顺序都算对。
//
// 与 rollup 的差别：rollup 自建 `context.WithoutCancel` 超时上下文（它要在请求被断开后仍写）；
// 本旁路沿用的是结算上下文——`dataplane.logSettle` 传进来的那个已经脱离请求生命周期。

// LeaseSettler 是租约结算的接收面。
//
// 用窄接口而不是直接要求 `*limit.Service`：terminal 只负责把「这一行花了多少钱、
// 判定时用的是哪几份切片」交出去，不关心对方是 Redis 租约、内存队列还是 nil（未装配）。
type LeaseSettler interface {
	// SettleLeases 把 markerID 这一次结算的成本扣到 plan 描述的切片上。
	// markerID 是幂等标记，不要求等于行 id：同一条请求的胜者与每条输家各用各的标记
	// （见本文件头第 3 条）。
	// costText 是账本里的成本文本（numeric 的文本形）：解析口径只有一处（limit.ParseCostText），
	// 不在本层另解析一次，免得两条路径对 NULL 与精度的处理分叉。
	// **不得返回错误、不得 panic、不得阻塞调用方**：签名上没有错误可返，实现须自带降级。
	SettleLeases(ctx context.Context, markerID string, costText string, plan pctx.LeaseSettlementPlan)
}

// settleLeases 在成本入库之后触发租约结算。未装配、无成本、无计划时整段跳过。
func (s *Settler) settleLeases(ctx context.Context, id int64, settlement Settlement) {
	if s == nil || s.leaseSettler == nil || id <= 0 {
		return
	}
	if settlement.Cost == nil || settlement.LeaseSettlement.Empty() {
		return
	}
	s.leaseSettler.SettleLeases(ctx, strconv.FormatInt(id, 10), settlement.Cost.Total, settlement.LeaseSettlement)
}
