package adminapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是供应商的**写路径**：创建、部分更新、删除、批量删除，以及两条撤销。
//
// 唯一真源：
//   - 处理器与状态码：src/app/api/v1/resources/providers/handlers.ts 的 createProvider（:109-143）、
//     updateProvider（:145-187）、deleteProvider（:189-204）、batchDeleteProviders（:382-392）、
//     undoDeleteProvider（:394-402）、undoProviderBatchPatch（:428-436）
//   - 动作层：src/actions/providers.ts 的 addProvider（:541-758）、editProvider（:760-1020）、
//     removeProvider（:1024-1109）、batchDeleteProviders（:2951-3013）、undoProviderDelete（:3016-3070）、
//     undoProviderPatch（:2515-2740）
//   - 撤销快照：本包的 provider_undo.go
//
// 三处**不能省**的语义（省掉就是静默错行为，不是简化）：
//  1. 创建/更新成功后必须广播失效（cfgsync.DomainProviders）。数据面各实例都有 providers 快照缓存，
//     不广播则「改了但路由仍用旧配置」，且只在多实例下暴露。
//  2. 更新必须写撤销快照（10 秒窗口）。UI 的撤销按钮只认快照，不写则按钮必失败。
//  3. 删除必须「先校验 operationId、再删 token」。删 token 在前会让一次参数写错的撤销白白烧掉
//     用户仅有的 60 秒窗口（Node 同序，见 actions/providers.ts:3040-3045 的注释）。

// providerWriteUnsupportedFields 是**已知但本轮未移植**的写字段，用于把 400 说清楚。
//
// 为什么列出来而不是让白名单默默拒绝：Node 的 zod 会以 invalidParams 指明字段名，
// 手工调用者据此改调用；「未知字段」与「已知字段但 Go 侧未实现」在运维上是两件事。
var providerWriteUnsupportedFields = map[string]string{
	"codex_instructions_strategy": "策略字段尚未纳入 Go 写入白名单",
}

// RegisterProvidersWrite 注册写路径与撤销路由。
//
// 注册条件（缺一不可）：
//   - Store 已装配；
//   - ProviderUndoKV 已装配——删除/更新都要写撤销快照，只注册写而不注册撤销等于让 UI 的撤销按钮
//     恒失败；反过来，KV 不可用时**整组不注册**（原样回退 Node），而不是只注册能跑的那半边。
func RegisterProvidersWrite(router *Router, deps Deps) {
	if deps.Store == nil {
		if deps.Logger != nil {
			deps.Logger.Warn("admin_providers_write_store_unwired", map[string]any{
				"module": "providers",
				"action": "write_routes_not_registered",
			})
		}
		return
	}
	if deps.ProviderUndoKV == nil {
		if deps.Logger != nil {
			deps.Logger.Warn("admin_providers_write_undo_kv_unwired", map[string]any{
				"module": "providers",
				"action": "write_routes_not_registered",
				"reason": "删除/更新的撤销快照无处置放，注册写路径会让 UI 的撤销按钮恒失败",
			})
		}
		return
	}

	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/providers",
		Access:      AccessAdmin,
		Module:      "providers",
		OperationID: "createProvider",
		Handler:     http.HandlerFunc(handleCreateProvider(deps)),
	})
	router.Add(Route{
		Method:      http.MethodPatch,
		Path:        "/providers/{id}",
		Access:      AccessAdmin,
		Module:      "providers",
		OperationID: "updateProvider",
		Handler:     http.HandlerFunc(handleUpdateProvider(deps)),
	})
	router.Add(Route{
		Method:      http.MethodDelete,
		Path:        "/providers/{id}",
		Access:      AccessAdmin,
		Module:      "providers",
		OperationID: "deleteProvider",
		Handler:     http.HandlerFunc(handleDeleteProvider(deps)),
	})
	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/providers:batchDelete",
		Access:      AccessAdmin,
		Module:      "providers",
		OperationID: "batchDeleteProviders",
		Handler:     http.HandlerFunc(handleBatchDeleteProviders(deps)),
	})
	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/providers:undoDelete",
		Access:      AccessAdmin,
		Module:      "providers",
		OperationID: "undoDeleteProvider",
		Handler:     http.HandlerFunc(handleUndoDeleteProvider(deps)),
	})
	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/providers:undoPatch",
		Access:      AccessAdmin,
		Module:      "providers",
		OperationID: "undoProviderBatchPatch",
		Handler:     http.HandlerFunc(handleUndoProviderPatch(deps)),
	})
}

// providerWriteMaxKeyLength 是 key 的长度上界（Node 的 PROVIDER_KEY_MAX_LENGTH）。
const providerWriteMaxKeyLength = 1024

// providerRedactedPrefix 是 Node 脱敏密钥里的标记子串（legacy-action-sanitizers.ts:195-202）。
const providerRedactedMarker = "[REDACTED]"

// providerCreateFieldNames 是创建时接受的字段全集。
//
// 与 Node 的 ProviderCreateSchema（src/lib/api/v1/schemas/providers.ts:362-530）逐条对应；
// 其中 `description` 只在更新里出现（Node 的 create schema 没有它），故不列入。
var providerCreateFieldNames = []string{
	"name", "url", "key", "is_enabled", "weight", "priority", "cost_multiplier",
	"group_tag", "group_priorities", "provider_type", "preserve_client_ip",
	"disable_session_reuse", "model_redirects", "active_time_start", "active_time_end",
	"allowed_models", "allowed_clients", "blocked_clients", "mcp_passthrough_type",
	"mcp_passthrough_url", "protocol_conversion_enabled", "limit_5h_usd", "limit_5h_reset_mode",
	"limit_daily_usd", "daily_reset_mode", "daily_reset_time", "limit_weekly_usd",
	"limit_monthly_usd", "limit_total_usd", "limit_concurrent_sessions", "max_retry_attempts",
	"circuit_breaker_failure_threshold", "circuit_breaker_open_duration",
	"circuit_breaker_half_open_success_threshold", "proxy_url", "proxy_fallback_to_direct",
	"circuit_breaker_release_increment", "circuit_breaker_max_open_count",
	"custom_headers", "first_byte_timeout_streaming_ms", "streaming_idle_timeout_ms",
	"request_timeout_non_streaming_ms", "website_url", "favicon_url", "cache_ttl_preference",
	"swap_cache_ttl_billing", "context_1m_preference", "codex_reasoning_effort_preference",
	"codex_reasoning_summary_preference", "codex_text_verbosity_preference",
	"codex_parallel_tool_calls_preference", "codex_image_generation_preference",
	"codex_service_tier_preference", "codex_max_tokens_preference",
	"anthropic_max_tokens_preference", "anthropic_thinking_budget_preference",
	"anthropic_adaptive_thinking", "openai_max_tokens_preference",
	"gemini_google_search_preference", "tpm", "rpm", "rpd", "cc",
	"slow_rate_monitor_enabled", "slow_rate_window_seconds", "slow_rate_min_samples",
	"slow_rate_baseline_window_seconds",
	"slow_rate_trigger_count", "slow_rate_ratio_per_mille", "slow_rate_penalty_step", "slow_rate_penalty_max",
	"slow_rate_recovery_requests",
	"slow_rate_probe_after_first_byte_seconds",
}

// providerUpdateFieldNames = 创建字段全集 + description（Node 的 ProviderUpdateSchema 是
// create 的部分集合并多出 description 与 key 的改写）。
var providerUpdateFieldNames = append(
	append([]string{}, providerCreateFieldNames...), "description")

// handleCreateProvider 复刻 createProvider（handlers.ts:109-143 + actions/providers.ts:541-758）。
func handleCreateProvider(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		fields, ok := adminReadJSONObject(writer, request, deps)
		if !ok {
			return
		}
		if name, redacted := providerFindRedactedWriteField(fields); redacted {
			adminProblemWriter(deps).WriteProblem(writer, request, http.StatusUnprocessableEntity,
				"provider.redacted_placeholder_rejected",
				fmt.Sprintf("Redacted placeholders cannot be used when creating providers (field: %s).", name))
			return
		}
		object := adminNewObject(fields, providerCreateFieldNames...)

		names := providerSortedKeys(fields)
		payload, issues := providerDecodeWriteFields(object, names, providerCreateWriteSpecs())
		if len(issues) > 0 {
			adminWriteValidationFailure(writer, request, issues)
			return
		}
		// 三段必填校验（Node 的 ProviderCreateSchema）：name 1..64、url 合法且 ≤255、key 非空。
		if _, present := payload["name"]; !present {
			payload["name"] = ""
		}
		if err := providerValidateCreate(&payload); err != nil {
			adminWriteValidationFailure(writer, request, err.issues)
			return
		}

		domain, err := providerVendorDomainKey(payload)
		if err != nil {
			adminWriteValidationFailure(writer, request, []invalidParam{{
				Path:    []any{"url"},
				Code:    "invalid_string",
				Message: "供应商 URL 无法解析出厂商域名",
			}})
			return
		}
		id, err := deps.Store.AdminCreateProvider(request.Context(), payload, domain)
		if err != nil {
			// 失败也落审计（Node 的 catch：actions/providers.ts:733-745，errorMessage 常量
			// CREATE_FAILED）。此前 Go 只在成功路径落，故「创建报 400」在 audit_log 里是空白。
			providerWriteFailure(deps, writer, request, "provider.create", 0,
				providerWriteAuditName(payload), "CREATE_FAILED", nil, err)
			return
		}

		adminPublishDomain(deps, request, cfgsync.DomainProviders)
		providerEmitWriteAudit(deps, request, "provider.create", id, providerWriteAuditName(payload),
			providerWriteAuditAfter(payload), true, "")

		created, err := providerFindVisible(request, deps, id)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, err)
			return
		}
		providerSyncCircuitConfig(deps, id, providerThresholdsFromRow(created))
		adminWriteCreated(writer, fmt.Sprintf("/api/v1/providers/%d", id),
			providerSummaryPayload(*created, nil))
	}
}

// handleUpdateProvider 复刻 updateProvider（handlers.ts:145-187 + actions/providers.ts:760-1020）。
//
// 与 Node 的差异（登记为白名单）：Node 在 `X-CCH-Dashboard-Compat` 下用另一套 schema
// （InternalProviderUpdateSchema）；Go 侧两套字段集相同，差异只在 Node 的那套允许内部字段，
// 本轮不实现（未接线的内部写入点）。
func handleUpdateProvider(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		id, ok := providerIDParam(writer, request)
		if !ok {
			return
		}
		// updateFailed 作答一次更新失败：失败审计（Node 的 catch：actions/providers.ts:985-997）
		// + 服务端日志 + 400，三件事绑在 providerWriteFailure 里。
		//
		// 与 Node 同：errorMessage 是常量 UPDATE_FAILED，不放原始文案——审计行在管理面里对多
		// 角色可见，pg 约束名与调用方输入不该进它；成因只进日志。
		updateFailed := func(details map[string]any, cause error) {
			providerWriteFailure(deps, writer, request, "provider.update", id, "",
				"UPDATE_FAILED", details, cause)
		}
		existing, err := providerFindVisible(request, deps, id)
		if err != nil {
			updateFailed(nil, err)
			return
		}
		fields, ok := adminReadJSONObject(writer, request, deps)
		if !ok {
			return
		}
		// key 与 custom_headers 里的脱敏回显是 422（Node 的两处前置检查，handlers.ts:167-186）。
		if raw, present := fields["key"]; present && providerHasRedactedPlaceholder(raw) {
			adminProviderRedactedPlaceholder(writer, request, deps,
				"Redacted placeholders cannot be used for the key field when updating providers.")
			return
		}
		if raw, present := fields["custom_headers"]; present &&
			providerHasRedactedHeaderEcho(raw, existing) {
			adminProviderRedactedPlaceholder(writer, request, deps,
				"Redacted placeholders cannot be used for renamed custom header fields.")
			return
		}

		object := adminNewObject(fields, providerUpdateFieldNames...)
		names := providerSortedKeys(fields)
		payload, issues := providerDecodeWriteFields(object, names, providerUpdateWriteSpecs())
		if len(issues) > 0 {
			adminWriteValidationFailure(writer, request, issues)
			return
		}
		if raw, present := fields["key"]; present {
			if _, ok := payload["key"]; !ok {
				_ = raw
			}
		}
		if len(payload) == 0 {
			// Node 对空 payload 回 200 + 当前行（updateProvider 的 :651-654），并**不**写撤销快照。
			adminWriteJSON(writer, http.StatusOK, providerSummaryPayload(*existing, nil))
			return
		}

		preimage := providerBuildPreimage(*existing, payload)
		patched, err := deps.Store.AdminPatchProvider(request.Context(), id, payload)
		if err != nil {
			updateFailed(nil, err)
			return
		}
		if !patched {
			adminProblemWriter(deps).WriteActionError(writer, request, providerNotFoundError())
			return
		}

		// 写成功后立即处置粘性会话（Node editProvider:928-930 就在 updateProvider 之后）。
		// 判据是前像命中「改了就失效」字段集——同值提交不入前像，故不会误终止。
		if providerStickySessionsNeedInvalidation(preimage) {
			adminTerminateStickySessions(request, deps, []int64{id}, "editProvider")
		}

		undoToken := newProviderUndoToken()
		operationID := newProviderOperationID()
		snapshot := ProviderPatchUndo{
			UndoToken:   undoToken,
			OperationID: operationID,
			ProviderIDs: []int64{id},
			Preimage:    map[string]map[string]any{providerIDKey(id): preimage},
		}
		if err := putProviderPatchUndo(request.Context(), deps.ProviderUndoKV, snapshot); err != nil {
			// 快照写不进去必须让调用方看见：改了但撤销不了是「静默的错行为」，
			// 与 Node「写快照失败即整动作失败」同一取舍。
			//
			// 但此刻 DB 已经改完了。这一点必须能从审计与日志里读出来（errorMessage 仍是 Node 的
			// UPDATE_FAILED 常量，区别落在 details 与错误文案），否则排障只能看到「400」，
			// 会误判「没改成功」而重复提交——2026-09-15 的 PATCH /api/v1/providers/149 现场正是如此。
			updateFailed(map[string]any{
				"changedFields":       providerSortedKeys(preimage),
				"dbUpdateApplied":     true,
				"undoSnapshotWritten": false,
			}, fmt.Errorf("provider %d 更新已提交，仅撤销快照写入失败: %w", id, err))
			return
		}

		adminPublishDomain(deps, request, cfgsync.DomainProviders)

		updated, err := providerFindVisible(request, deps, id)
		if err != nil {
			// 成功审计就在下面几行：移到动作末尾之后，这里的失败才不会与「成功」同时出现在审计里
			// （Node 的 catch 与末尾 emit 也是这个次序）。
			updateFailed(map[string]any{
				"changedFields":   providerSortedKeys(preimage),
				"dbUpdateApplied": true,
			}, err)
			return
		}
		providerSyncCircuitConfig(deps, id, providerThresholdsFromRow(updated))
		providerEmitWriteAudit(deps, request, "provider.update", id, "",
			map[string]any{"changedFields": providerSortedKeys(preimage)}, true, "")
		writer.Header().Set("X-CCH-Undo-Token", undoToken)
		writer.Header().Set("X-CCH-Operation-Id", operationID)
		adminWriteJSON(writer, http.StatusOK, providerSummaryPayload(*updated, nil))
	}
}

// handleDeleteProvider 复刻 deleteProvider（handlers.ts:189-204 + actions/providers.ts:1024-1109）。
func handleDeleteProvider(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		id, ok := providerIDParam(writer, request)
		if !ok {
			return
		}
		existing, err := providerFindVisible(request, deps, id)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, err)
			return
		}
		deleted, err := deps.Store.AdminSoftDeleteProviders(request.Context(), []int64{id})
		if err != nil {
			providerEmitWriteAudit(deps, request, "provider.delete", id, "", nil, false, "DELETE_FAILED")
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}
		if deleted == 0 {
			adminProblemWriter(deps).WriteActionError(writer, request, providerNotFoundError())
			return
		}

		// 删除即无条件终止（Node removeProvider:1036）。
		adminTerminateStickySessions(request, deps, []int64{id}, "removeProvider")

		undoToken := newProviderUndoToken()
		operationID := newProviderOperationID()
		snapshot := ProviderDeleteUndo{
			UndoToken:   undoToken,
			OperationID: operationID,
			ProviderIDs: []int64{id},
		}
		if err := putProviderDeleteUndo(request.Context(), deps.ProviderUndoKV, snapshot); err != nil {
			// 与 update 同：DB 软删已生效（Node 的 deleteProvider 也在快照写入之前，:1013-1024），
			// 这一事实须能从审计读出，否则排障会误判「没删成功」。
			providerWriteFailure(deps, writer, request, "provider.delete", id, "", "DELETE_FAILED",
				map[string]any{
					"dbDeleteApplied":     true,
					"undoSnapshotWritten": false,
				}, err)
			return
		}

		adminPublishDomain(deps, request, cfgsync.DomainProviders)
		providerEmitWriteAudit(deps, request, "provider.delete", id, existing.Name,
			map[string]any{"id": id, "name": existing.Name, "url": providerRedactURLCredentials(existing.URL)},
			true, "")

		writer.Header().Set("X-CCH-Undo-Token", undoToken)
		writer.Header().Set("X-CCH-Operation-Id", operationID)
		adminWriteNoContent(writer)
	}
}

// handleBatchDeleteProviders 复刻 batchDeleteProviders（handlers.ts:382-392 + actions:2951-3013）。
func handleBatchDeleteProviders(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		fields, ok := adminReadJSONObject(writer, request, deps)
		if !ok {
			return
		}
		ids, issues := providerDecodeProviderIDs(fields)
		if len(issues) > 0 {
			adminWriteValidationFailure(writer, request, issues)
			return
		}
		if _, err := providerEnsureVisibleIDs(request, deps, ids); err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, err)
			return
		}

		snapshotIDs := providerDedupeIDs(ids)
		deleted, err := deps.Store.AdminSoftDeleteProviders(request.Context(), snapshotIDs)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}

		undoToken := newProviderUndoToken()
		operationID := newProviderOperationID()
		snapshot := ProviderDeleteUndo{
			UndoToken:   undoToken,
			OperationID: operationID,
			ProviderIDs: snapshotIDs,
		}
		if err := putProviderDeleteUndo(request.Context(), deps.ProviderUndoKV, snapshot); err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}

		adminPublishDomain(deps, request, cfgsync.DomainProviders)
		providerEmitWriteAudit(deps, request, "provider.batch_delete", snapshotIDs[0], "",
			map[string]any{"providerIds": snapshotIDs, "deletedCount": deleted}, true, "")

		adminWriteJSON(writer, http.StatusOK, providerBatchDeleteResponse{
			DeletedCount: deleted,
			UndoToken:    undoToken,
			OperationID:  operationID,
		})
	}
}

// handleUndoDeleteProvider 复刻 undoDeleteProvider（handlers.ts:394-402 + actions:3016-3070）。
func handleUndoDeleteProvider(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		fields, ok := adminReadJSONObject(writer, request, deps)
		if !ok {
			return
		}
		undoToken, operationID, issues := providerDecodeUndoBody(fields)
		if len(issues) > 0 {
			adminWriteValidationFailure(writer, request, issues)
			return
		}

		snapshot, err := loadProviderDeleteUndo(request.Context(), deps.ProviderUndoKV, undoToken)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}
		if snapshot == nil {
			adminProblemWriter(deps).WriteActionError(writer, request,
				providerUndoExpiredError("撤销窗口已过期"))
			return
		}
		if snapshot.OperationID != operationID {
			adminProblemWriter(deps).WriteActionError(writer, request,
				providerUndoConflictError("撤销参数与操作不匹配"))
			return
		}
		// 校验通过后才消费 token（Node 同序）：operationId 不匹配不得烧掉窗口。
		if err := deps.ProviderUndoKV.Del(
			request.Context(), providerDeleteUndoKey(undoToken)); err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}

		restored, err := deps.Store.AdminRestoreProviders(request.Context(), snapshot.ProviderIDs)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}
		adminPublishDomain(deps, request, cfgsync.DomainProviders)
		providerEmitWriteAudit(deps, request, "provider.undo_delete", snapshot.ProviderIDs[0], "",
			map[string]any{"operationId": operationID, "restoredCount": restored}, true, "")

		adminWriteJSON(writer, http.StatusOK, providerUndoDeleteResponse{
			OperationID:   snapshot.OperationID,
			RestoredAt:    adminNowISO(),
			RestoredCount: restored,
		})
	}
}

// handleUndoProviderPatch 复刻 undoProviderBatchPatch（handlers.ts:428-436 + actions:2515-2740）。
//
// 白名单差异：Node 在快照缺失时回退**持久账本**（provider_batch_apply_operations，批量补丁 apply 的
// 幂等记录）。Go 侧尚未移植该账本，故只有可挥发的 10 秒快照一条路径；账本分支的响应形状相同，
// 但「token 已过期但账本仍有效」这种情形在 Go 接管后不存在（批量补丁 apply 本身也没接管）。
func handleUndoProviderPatch(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		fields, ok := adminReadJSONObject(writer, request, deps)
		if !ok {
			return
		}
		undoToken, operationID, issues := providerDecodeUndoBody(fields)
		if len(issues) > 0 {
			adminWriteValidationFailure(writer, request, issues)
			return
		}

		snapshot, err := takeProviderPatchUndo(request.Context(), deps.ProviderUndoKV, undoToken)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}
		if snapshot == nil {
			adminProblemWriter(deps).WriteActionError(writer, request,
				providerUndoExpiredError("撤销窗口已过期"))
			return
		}
		if snapshot.OperationID != operationID {
			adminProblemWriter(deps).WriteActionError(writer, request,
				providerUndoConflictError("撤销参数与操作不匹配"))
			return
		}

		var reverted int64
		for _, providerID := range snapshot.ProviderIDs {
			preimage, ok := snapshot.Preimage[providerIDKey(providerID)]
			if !ok || len(preimage) == 0 {
				continue
			}
			payload, issues := providerPreimageToWriteFields(preimage)
			if len(issues) > 0 {
				adminWriteValidationFailure(writer, request, issues)
				return
			}
			patched, err := deps.Store.AdminPatchProvider(request.Context(), providerID, payload)
			if err != nil {
				adminProblemWriter(deps).WriteActionError(writer, request,
					adminActionFailure("provider", err))
				return
			}
			if patched {
				reverted++
			}
		}
		adminPublishDomain(deps, request, cfgsync.DomainProviders)
		providerEmitWriteAudit(deps, request, "provider.undo_patch", snapshot.ProviderIDs[0], "",
			map[string]any{"operationId": operationID, "revertedCount": reverted}, true, "")

		adminWriteJSON(writer, http.StatusOK, providerUndoPatchResponse{
			OperationID:   snapshot.OperationID,
			RevertedAt:    adminNowISO(),
			RevertedCount: reverted,
		})
	}
}

// providerBatchDeleteResponse 是批量删除的响应体（BatchDeleteProvidersResult）。
type providerBatchDeleteResponse struct {
	DeletedCount int64  `json:"deletedCount"`
	UndoToken    string `json:"undoToken"`
	OperationID  string `json:"operationId"`
}

// providerUndoDeleteResponse 是撤销删除的响应体（UndoProviderDeleteResult）。
type providerUndoDeleteResponse struct {
	OperationID   string `json:"operationId"`
	RestoredAt    string `json:"restoredAt"`
	RestoredCount int64  `json:"restoredCount"`
}

// providerUndoPatchResponse 是撤销更新的响应体（UndoProviderPatchResult）。
type providerUndoPatchResponse struct {
	OperationID   string `json:"operationId"`
	RevertedAt    string `json:"revertedAt"`
	RevertedCount int64  `json:"revertedCount"`
}

// providerVendorDomainKey 复刻 computeVendorKey（repository/provider-endpoints.ts:172-188）。
//
// 两条分支：websiteUrl 非空 → 取它的 hostname（带显式端口时带端口）；否则 → provider URL 的
// host:port（缺省端口按协议取 80/443，无 scheme 视为 https）。
func providerVendorDomainKey(payload map[string]any) (string, error) {
	website, _ := payload["website_url"].(*string)
	if website != nil && strings.TrimSpace(*website) != "" {
		if key := providerNormalizeWebsiteDomainKey(*website); key != "" {
			return key, nil
		}
	}
	raw, _ := payload["url"].(string)
	key := providerNormalizeHostWithPort(raw)
	if key == "" {
		return "", fmt.Errorf("adminapi: 供应商 URL 无法解析出厂商域名: %q", raw)
	}
	return key, nil
}

// providerNormalizeWebsiteDomainKey 取 website URL 的 hostname（显式端口时带端口）。
func providerNormalizeWebsiteDomainKey(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Hostname() == "" {
		return ""
	}
	host := strings.TrimPrefix(strings.ToLower(parsed.Hostname()), "www.")
	if parsed.Port() != "" {
		return host + ":" + parsed.Port()
	}
	return host
}

// providerNormalizeHostWithPort 复刻 normalizeHostWithPort（provider-endpoints.ts:122-170）。
func providerNormalizeHostWithPort(rawURL string) string {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return ""
	}
	// 无 scheme 时按 https 解析（Node 的 `https://${trimmed}`）。
	if !providerHasScheme(trimmed) {
		trimmed = "https://" + trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Hostname() == "" {
		return ""
	}
	host := strings.TrimPrefix(strings.ToLower(parsed.Hostname()), "www.")
	port := parsed.Port()
	if port == "" {
		if parsed.Scheme == "http" {
			port = "80"
		} else {
			port = "443"
		}
	}
	return host + ":" + port
}

func providerHasScheme(raw string) bool {
	for index, char := range raw {
		if index == 0 {
			if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z')) {
				return false
			}
			continue
		}
		switch {
		case char >= 'a' && char <= 'z', char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
			continue
		case char == '+', char == '-', char == '.':
			continue
		case char == ':':
			return strings.HasPrefix(raw[index:], "://")
		default:
			return false
		}
	}
	return false
}

// providerDedupeIDs 复刻 dedupeProviderIds：去重 + 升序（Node 的 [...new Set(ids)].sort()）。
func providerDedupeIDs(ids []int64) []int64 {
	seen := make(map[int64]struct{}, len(ids))
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// providerHasRedactedPlaceholder 复刻 hasLegacyRedactedWritePlaceholders 的字符串判定。
func providerHasRedactedPlaceholder(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return false
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return strings.Contains(string(raw), providerRedactedMarker)
	}
	return providerValueHasRedactedPlaceholder(decoded)
}

func providerValueHasRedactedPlaceholder(value any) bool {
	switch typed := value.(type) {
	case string:
		return strings.Contains(typed, providerRedactedMarker) ||
			strings.Contains(typed, "REDACTED:REDACTED@")
	case []any:
		for _, item := range typed {
			if providerValueHasRedactedPlaceholder(item) {
				return true
			}
		}
	case map[string]any:
		for _, item := range typed {
			if providerValueHasRedactedPlaceholder(item) {
				return true
			}
		}
	}
	return false
}

// providerHasRedactedHeaderEcho 复刻 hasUnresolvedRedactedHeaderEcho（handlers.ts:752-777）。
//
// 语义：custom_headers 里出现了「现有头值被脱敏后的形状」但头名对不上现有头名时，判为未解析的脱敏回显。
// Node 的实现细节见同函数；Go 侧按同一判据：存在某键的值等于其现有值的脱敏形状，而键不在现有头名里。
func providerHasRedactedHeaderEcho(raw json.RawMessage, existing *store.AdminProvider) bool {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || len(existing.CustomHeaders) == 0 {
		return false
	}
	var next map[string]any
	if err := json.Unmarshal(raw, &next); err != nil {
		return false
	}
	var current map[string]any
	if err := json.Unmarshal(existing.CustomHeaders, &current); err != nil {
		return false
	}
	for name, value := range next {
		if _, ok := current[name]; ok {
			continue
		}
		if providerValueHasRedactedPlaceholder(value) {
			return true
		}
	}
	return false
}
