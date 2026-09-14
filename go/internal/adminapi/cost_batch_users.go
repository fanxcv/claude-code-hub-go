package adminapi

import (
	"net/http"
	"sort"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件实现**批量累计成本读数**两条端点：
//
//	POST /users:costBatch   用户维度
//	POST /keys:costBatch    密钥维度
//
// **为什么是「新能力」而不是「补齐漏字段」**：Node 版的「累计成本」列是 **SSR 服务端聚合**
// （`src/app/[locale]/dashboard/quotas/users/page.tsx` 直接调 `repository/statistics.ts` 的两个
// 批量函数），**Node 的 REST 面从来没有这个端点**。静态导出后前端没有服务端可用，于是那两列
// 被硬编码成 `totalUsage: 0` —— 显示的是**一个确定的错数**（同时让总额度进度条恒 0%、
// 按成本排序等价于不排序），比「无读数」更坏。故这里按 Node 的取数语义补出 REST 面。
//
// 取数口径（表、计费条件、时间上限、重置语义）逐条见 `internal/store/admin_cost_batch.go`
// 的文件注释——**那是唯一真源**，本文件只负责「校验入参、调用取数、装配信封」。
//
// 与 Node 的**有意差异**（登记在此，避免后人误判为分叉）：
//  1. 这是 Go-only 端点：Node 侧没有任何对应路由（端点差分台账会把它记为 Go-only，属预期）。
//  2. 前端不再自己算 `resetAtMap`：Node 的 SSR 页面把 `user.costResetAt` 与
//     `resolveKeyCostResetAt(key.costResetAt, user.costResetAt)` 传给取数函数；本实现在
//     **服务端**直接读同样两列（`users.cost_reset_at` / `keys.cost_reset_at`），
//     语义等价而客户端契约更小（只传 id，不传时间戳，也就不存在「客户端与库不一致」的可能）。
//  3. 单次上限 **200** 个 id：超限返 400 + `too_big`，**不做静默截断**（静默截断会让界面
//     把「没查到的实体」显示成 0——正是本次要消灭的那种错数）。

// costBatchMaxIDs 是单次请求可查的最大实体数。
//
// 取值理由：前端分页拉用户（`USERS_PAGE_SIZE=200`）后再批量取成本，200 与一页同量级；
// 前端按此上限自行分片（见 `src/lib/api-client/v1/actions/cost-batch.ts`）。
const costBatchMaxIDs = 200

// costBatchAPI 是两条批量成本端点的处理器。
type costBatchAPI struct {
	deps     Deps
	pools    *store.Pools
	problems ProblemWriter
	logger   *logx.Logger
}

// RegisterCostBatchRoutes 注册批量累计成本两条端点。
//
// 缺连接池即整组不注册（回退 Node）：没有库就没有成本可算，注册等于制造 500。
// 双跑期 Node 侧无此端点，故「回退 Node」只会得到 404 —— 这是**预期**：该能力只有 Go 有。
func RegisterCostBatchRoutes(router *Router, deps Deps) {
	if deps.Store == nil {
		if deps.Logger != nil {
			deps.Logger.Warn("admin_cost_batch_unwired", map[string]any{
				"module": "cost-batch",
				"action": "routes_not_registered",
				"reason": "store_missing",
				"routes": []string{"POST /users:costBatch", "POST /keys:costBatch"},
			})
		}
		return
	}
	api := &costBatchAPI{
		deps:     deps,
		pools:    deps.Store,
		problems: adminProblemWriter(deps),
		logger:   adminLoggerOf(deps),
	}

	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/users:costBatch",
		Access:      AccessAdmin,
		Module:      "users",
		OperationID: "getUserCostBatch",
		Handler:     http.HandlerFunc(api.handleUserCostBatch),
	})
	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/keys:costBatch",
		Access:      AccessAdmin,
		Module:      "keys",
		OperationID: "getKeyCostBatch",
		Handler:     http.HandlerFunc(api.handleKeyCostBatch),
	})
}

// handleUserCostBatch 读 `{ids:[...]}` 并返回用户维度的累计成本。
func (api *costBatchAPI) handleUserCostBatch(writer http.ResponseWriter, request *http.Request) {
	ids, ok := api.readIDs(writer, request)
	if !ok {
		return
	}
	totals, err := api.pools.UserCostBatchTotalCost(request.Context(), ids)
	if err != nil {
		api.problems.WriteActionError(writer, request, adminActionFailure("user", err))
		return
	}
	adminWriteJSON(writer, http.StatusOK, costBatchEnvelope(ids, totals))
}

// handleKeyCostBatch 读 `{ids:[...]}` 并返回密钥维度的累计成本。
func (api *costBatchAPI) handleKeyCostBatch(writer http.ResponseWriter, request *http.Request) {
	ids, ok := api.readIDs(writer, request)
	if !ok {
		return
	}
	totals, err := api.pools.KeyCostBatchTotalCost(request.Context(), ids)
	if err != nil {
		api.problems.WriteActionError(writer, request, adminActionFailure("key", err))
		return
	}
	adminWriteJSON(writer, http.StatusOK, costBatchEnvelope(ids, totals))
}

// readIDs 解析并校验请求体，返回**去重且升序**的 id 列表。
//
// 去重理由：调用方（配额页）可能把同一 id 重复塞进分片；重复在 SQL 的 `ANY($1)` 里无害，
// 但会让响应的 items 出现重复 id，前端按 id 建 Map 时后者覆盖前者——不报错却难查。
func (api *costBatchAPI) readIDs(writer http.ResponseWriter, request *http.Request) ([]int64, bool) {
	fields, ok := adminReadJSONObject(writer, request, api.deps)
	if !ok {
		return nil, false
	}
	object := adminNewObject(fields, "ids")
	object.RejectUnknownKeys()
	ids, hasIDs := adminInt64Array(object, "ids", 1, costBatchMaxIDs)
	if issues := object.issues0(); len(issues) > 0 {
		adminWriteValidationFailure(writer, request, issues)
		return nil, false
	}
	if !hasIDs {
		adminWriteValidationFailure(writer, request, []invalidParam{{
			Path: []any{"ids"}, Code: "invalid_type", Message: "Required",
		}})
		return nil, false
	}

	seen := make(map[int64]struct{}, len(ids))
	unique := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	sort.Slice(unique, func(left, right int) bool { return unique[left] < unique[right] })
	return unique, true
}

// costBatchEnvelope 把取数结果装配成 `{items:[{id,totalCost}]}`，按 id 升序。
//
// totalCost 是 **numeric 文本**（不经 float64）：与 `store` 层的既有纪律一致
// （见 `SumLedgerTotalCost` 的注释），前端在展示处再转数字。
func costBatchEnvelope(ids []int64, totals map[int64]string) map[string]any {
	items := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		total, ok := totals[id]
		if !ok {
			total = "0"
		}
		items = append(items, map[string]any{"id": id, "totalCost": total})
	}
	return map[string]any{"items": items}
}
