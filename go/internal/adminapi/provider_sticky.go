package adminapi

import (
	"net/http"
)

// 供应商写路径的粘性会话终止。
//
// 复刻 SessionManager.terminateStickySessionsForProviders（session-manager.ts:3803-3816）在管理面
// 写路径上的**五个**调用点：
//
//	| Node 调用点（TS 行号）                              | 触发条件                                    | 影响范围 |
//	| editProvider（actions/providers.ts:928-930）        | 前像命中了「改了就失效」字段集（见下）        | [providerId] |
//	| removeProvider（actions/providers.ts:1036）         | 无条件                                      | [providerId] |
//	| batchUpdateProviders（actions/providers.ts:2920-2926）| updates 里给了 group_tag / model_redirects / allowed_models / allowed_clients / blocked_clients | providerIds |
//	| editProviderEndpoint（provider-endpoints.ts:528-536）| 请求体给了 url / sortOrder / isEnabled（**看是否给出，不看是否变化**） | 该厂该类型下**已启用**的供应商 |
//	| removeProviderEndpoint（provider-endpoints.ts:627-634）| 无条件                                      | 同上 |
//
// 为什么不做成「缺依赖就不注册路由」：它不是一条路由，而是写路径的副作用。Node 对它是 fail-open——
// 内部 try/catch 只记 warn（session-manager.ts:3811-3815），调用方的写操作照常成功。Go 侧同判：
// 未装配或调用失败都只记 warn，绝不把已经落库的写操作变成错误。
//
// Node 侧的**非**触发点（照抄，别顺手补）：batchDeleteProviders（actions/providers.ts:2951-3013）
// 删完供应商后**不**终止粘性会话——同是删除，单个走 removeProvider 会终止、批量走
// batchDeleteProviders 不会。这是 Node 的既存不对称，Go 侧保留以维持行为一致。
//
// 与 Node 的一处**登记差异**（只在数据库故障窗口内可观测）：Node 在 provider-endpoints 两处
// 把「查该厂该类型下已启用供应商」的读**裸放在 action 的 try 里**（provider-endpoints.ts:533、630），
// 读失败会冒泡成 action 失败（`更新端点失败`/`删除端点失败` → 400）——即便端点已经写成功。
// Go 侧把该读失败降级为 warn（写操作已落库，再回 400 会得到一个「报错但其实改成了」的状态）。
// 触发条件要求「端点已写成功、随后这次读失败」，即数据库在这两步之间故障；两侧此前都已成功
// 读过同一张表，故实际窗口极窄。

// providerStickySessionKeys 复刻 STICKY_SESSION_INVALIDATING_PROVIDER_KEYS
// （actions/providers.ts:218-230）：单家编辑时，前像里出现这些 camelCase 字段就说明改动会影响
// 已建立的会话亲和（端点、类型、分组、启停、模型/客户端白黑名单、活跃时段），必须终止该供应商
// 名下正在跑的粘性会话，否则老亲和会继续落到旧配置上直到 TTL 到期。
var providerStickySessionKeys = map[string]struct{}{
	"url":             {},
	"websiteUrl":      {},
	"providerType":    {},
	"groupTag":        {},
	"isEnabled":       {},
	"allowedModels":   {},
	"allowedClients":  {},
	"blockedClients":  {},
	"modelRedirects":  {},
	"activeTimeStart": {},
	"activeTimeEnd":   {},
}

// providerStickySessionsNeedInvalidation 报告单家编辑的前像是否命中「改了就失效」字段集。
//
// 判据是**前像的键**而不是 payload 的键：前像只收「确实变了」的字段
// （providerBuildPreimage → providerFieldChangedForUndo），所以「提交了同值」不该触发终止——
// 与 Node 把 changedProviderFields 交给同一个判定一致。
func providerStickySessionsNeedInvalidation(preimage map[string]any) bool {
	for key := range preimage {
		if _, sticky := providerStickySessionKeys[key]; sticky {
			return true
		}
	}
	return false
}

// adminTerminateStickySessions 终止给定供应商名下的粘性会话；任何失败只记 warn。
//
// context 是 Node 的日志前缀（"editProvider" / "removeProvider" / "batchUpdateProviders" /
// "editProviderEndpoint" / "removeProviderEndpoint"），保留它是为了让两侧日志能逐条对齐排查。
func adminTerminateStickySessions(request *http.Request, deps Deps, providerIDs []int64, context string) {
	if len(providerIDs) == 0 {
		return
	}
	if deps.StickySessions == nil {
		// 未装配（没有 Redis 命令连接）：数据面此刻也建不起亲和，无会话可终止；记 warn 留痕。
		adminLoggerOf(deps).Warn(context+":terminate_provider_sessions_skipped", map[string]any{
			"providerIds": providerIDs,
			"reason":      "sticky_sessions_unwired",
		})
		return
	}
	if _, err := deps.StickySessions.TerminateProviderSessionsBatch(request.Context(), providerIDs); err != nil {
		adminLoggerOf(deps).Warn(context+":terminate_provider_sessions_failed", map[string]any{
			"providerIds": providerIDs,
			"error":       err.Error(),
		})
	}
}

// adminTerminateStickySessionsForEndpoint 复刻 provider-endpoints 两处调用：先把该厂该类型下的
// **已启用**供应商查出来，再终止它们的粘性会话（禁用/已删的供应商没有在跑的会话）。
func adminTerminateStickySessionsForEndpoint(
	request *http.Request,
	deps Deps,
	vendorID int64,
	providerType string,
	context string,
) {
	providerIDs, err := deps.Store.AdminFindEnabledProviderIDsByVendorAndType(
		request.Context(), vendorID, providerType)
	if err != nil {
		adminLoggerOf(deps).Warn(context+":find_enabled_providers_failed", map[string]any{
			"vendorId":     vendorID,
			"providerType": providerType,
			"error":        err.Error(),
		})
		return
	}
	adminTerminateStickySessions(request, deps, providerIDs, context)
}
