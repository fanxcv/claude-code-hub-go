package adminapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 `POST /providers:batchPatch:preview` 与 `:apply` 的移植。
//
// 唯一真源：
//   - 预览：src/actions/providers.ts:2225-2301（previewProviderBatchPatch）、:2019-2078（generatePreviewRows）、
//     :1469-1472（快照 store 前缀与 TTL）
//   - 应用：同文件 :2302-2514（applyProviderBatchPatch）、:1619-1631（指纹与 claimKey）、
//     :1633-1674（前像期望/撤销前像）、:1907-2017（按供应商类型分组）、:2139-2213（提交后效应）
//   - 账本：src/repository/provider.ts:1540-1872（本仓 go/internal/store/admin_provider_batchapply.go）
//   - 路由与错误映射：src/app/api/v1/resources/providers/router.ts:563-609、handlers.ts:404-426 与 :841-865
//
// **两条路由必须成对注册**：预览快照写在 `cch:prov:preview:` 前缀下，而 Node 与 Go 对
// token/revision 的校验实现不同；只接管一条会让另一条落到「缓存由对方写、我方读」的未验证路径。

// providerBatchPatchPreviewSnapshot 是预览快照（Node 的 ProviderBatchPatchPreviewSnapshot）。
//
// 字段名与 Node 逐字一致（camelCase）：Node 会读同一个 Redis 前缀的键，形状不同就等于让 Node 读不懂。
type providerBatchPatchPreviewSnapshot struct {
	PreviewToken    string                    `json:"previewToken"`
	PreviewRevision string                    `json:"previewRevision"`
	ProviderIDs     []int64                   `json:"providerIds"`
	Patch           json.RawMessage           `json:"patch"`
	PatchSerialized string                    `json:"patchSerialized"`
	ChangedFields   []string                  `json:"changedFields"`
	Rows            []providerBatchPreviewRow `json:"rows"`
	ProviderTypes   map[string]string         `json:"providerTypes"`
	ProviderEnabled map[string]bool           `json:"providerEnabled"`
}

// providerBatchPreviewRow 是预览行（Node 的 ProviderBatchPreviewRow）。
type providerBatchPreviewRow struct {
	ProviderID   int64  `json:"providerId"`
	ProviderName string `json:"providerName"`
	Field        string `json:"field"`
	Status       string `json:"status"`
	Before       any    `json:"before"`
	After        any    `json:"after"`
	SkipReason   string `json:"skipReason,omitempty"`
}

// providerBatchPatchPreviewResult 是预览响应（Node 的 PreviewProviderBatchPatchResult）。
type providerBatchPatchPreviewResult struct {
	PreviewToken     string                    `json:"previewToken"`
	PreviewRevision  string                    `json:"previewRevision"`
	PreviewExpiresAt string                    `json:"previewExpiresAt"`
	ProviderIDs      []int64                   `json:"providerIds"`
	ChangedFields    []string                  `json:"changedFields"`
	Rows             []providerBatchPreviewRow `json:"rows"`
	Summary          struct {
		ProviderCount int `json:"providerCount"`
		FieldCount    int `json:"fieldCount"`
		SkipCount     int `json:"skipCount"`
	} `json:"summary"`
}

// providerBatchPatchApplyResult 是应用响应（Node 的 ApplyProviderBatchPatchResult）。
type providerBatchPatchApplyResult struct {
	OperationID   string `json:"operationId"`
	AppliedAt     string `json:"appliedAt"`
	UpdatedCount  int64  `json:"updatedCount"`
	UndoToken     string `json:"undoToken"`
	UndoExpiresAt string `json:"undoExpiresAt"`
}

// 错误码（Node 的 PROVIDER_BATCH_PATCH_ERROR_CODES，src/lib/provider-batch-patch-error-codes.ts）。
const (
	providerBatchCodeNothingToApply      = "NOTHING_TO_APPLY"
	providerBatchCodePreviewExpired      = "PREVIEW_EXPIRED"
	providerBatchCodePreviewStale        = "PREVIEW_STALE"
	providerBatchCodeIdempotencyConflict = "IDEMPOTENCY_CONFLICT"
)

// providerBatchMaxSize 是批量操作的供应商数上限（Node 的 BATCH_OPERATION_MAX_SIZE，actions:1340）。
const providerBatchMaxSize = 500

// RegisterProviderBatchPatch 注册批量补丁的预览与应用两条路由。
//
// 依赖：Store（读供应商、写账本）、ProviderUndoKV（预览快照与撤销快照）。
// 任一缺失则**整组不注册**（回退 Node）——只注册一条会让另一半落到跨实现的未验证路径。
func RegisterProviderBatchPatch(router *Router, deps Deps) {
	if deps.Store == nil || deps.ProviderUndoKV == nil {
		logBatchPatchUnwired(deps, "/providers:batchPatch:*")
		return
	}
	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/providers:batchPatch:preview",
		Access:      AccessAdmin,
		Module:      "providers",
		OperationID: "previewBatchPatch",
		Handler:     http.HandlerFunc(handleProviderBatchPatchPreview(deps)),
	})
	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/providers:batchPatch:apply",
		Access:      AccessAdmin,
		Module:      "providers",
		OperationID: "applyBatchPatch",
		Handler:     http.HandlerFunc(handleProviderBatchPatchApply(deps)),
	})
}

func logBatchPatchUnwired(deps Deps, scope string) {
	if deps.Logger == nil {
		return
	}
	deps.Logger.Warn("admin_batchpatch_not_registered", map[string]any{
		"scope":  scope,
		"reason": "Store 或 ProviderUndoKV 未装配：预览与应用必须成对接管，否则另一半会落到跨实现的未验证路径",
	})
}

// handleProviderBatchPatchPreview 复刻 previewProviderBatchPatch（actions:2225-2301）。
func handleProviderBatchPatchPreview(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		fields, ok := adminReadJSONObject(writer, request, deps)
		if !ok {
			return
		}
		providerIDs, issues := providerBatchDecodeProviderIDs(fields, "providerIds", false)
		if len(issues) > 0 {
			adminWriteValidationFailure(writer, request, issues)
			return
		}
		patch, contractErr := providerBatchDecodePatch(fields)
		if contractErr != nil {
			providerBatchWriteContractError(deps, writer, request, contractErr)
			return
		}
		if !hasProviderBatchPatchChanges(patch) {
			providerBatchWriteProblem(deps, writer, request, http.StatusBadRequest, providerBatchCodeNothingToApply)
			return
		}

		changedFields := changedProviderPatchFields(patch)
		rows, err := deps.Store.AdminFindProvidersForBatchPatch(request.Context(), providerIDs)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}
		matched := make(map[int64]store.AdminProviderBatchRow, len(rows))
		for _, row := range rows {
			matched[row.ID] = row
		}

		previewRows := generateProviderBatchPreviewRows(providerIDs, matched, patch, changedFields)
		skipCount := 0
		for _, row := range previewRows {
			if row.Status == "skipped" {
				skipCount++
			}
		}

		nowMS := time.Now().UnixMilli()
		token, err := providerBatchRandomToken("provider_patch_preview_")
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}
		revision := fmt.Sprintf("%d:%s:%s", nowMS, joinInt64(providerIDs, ","), strings.Join(changedFields, ","))
		expiresAt := nowMS + ProviderPreviewTTLSeconds*1000

		snapshot := providerBatchPatchPreviewSnapshot{
			PreviewToken:    token,
			PreviewRevision: revision,
			ProviderIDs:     providerIDs,
			Patch:           json.RawMessage(marshalProviderBatchPatchSerialized(patch)),
			PatchSerialized: string(marshalProviderBatchPatchSerialized(patch)),
			ChangedFields:   changedFields,
			Rows:            previewRows,
			ProviderTypes:   make(map[string]string, len(matched)),
			ProviderEnabled: make(map[string]bool, len(matched)),
		}
		for providerID, row := range matched {
			snapshot.ProviderTypes[fmt.Sprintf("%d", providerID)] = row.ProviderType
			snapshot.ProviderEnabled[fmt.Sprintf("%d", providerID)] = row.IsEnabled
		}
		payload, err := json.Marshal(snapshot)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}
		if err := deps.ProviderUndoKV.SetEx(
			request.Context(),
			providerPreviewPrefix+token,
			payload,
			ProviderPreviewTTLSeconds*time.Second,
		); err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}

		result := providerBatchPatchPreviewResult{
			PreviewToken:     token,
			PreviewRevision:  revision,
			PreviewExpiresAt: formatAdminISO(time.UnixMilli(expiresAt)),
			ProviderIDs:      providerIDs,
			ChangedFields:    changedFields,
			Rows:             previewRows,
		}
		result.Summary.ProviderCount = len(providerIDs)
		result.Summary.FieldCount = len(changedFields)
		result.Summary.SkipCount = skipCount
		adminWriteJSON(writer, http.StatusOK, result)
	}
}

// handleProviderBatchPatchApply 复刻 applyProviderBatchPatch（actions:2302-2514）。
func handleProviderBatchPatchApply(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		fields, ok := adminReadJSONObject(writer, request, deps)
		if !ok {
			return
		}
		previewToken, tokenIssues := providerBatchDecodeString(fields, "previewToken")
		previewRevision, revisionIssues := providerBatchDecodeString(fields, "previewRevision")
		providerIDs, idIssues := providerBatchDecodeProviderIDs(fields, "providerIds", false)
		excludeIDs, excludeIssues := providerBatchDecodeProviderIDs(fields, "excludeProviderIds", true)
		issues := append(append(append(tokenIssues, revisionIssues...), idIssues...), excludeIssues...)
		if len(issues) > 0 {
			adminWriteValidationFailure(writer, request, issues)
			return
		}
		idempotencyKey, idempotencyIssues := providerBatchDecodeOptionalString(fields, "idempotencyKey")
		if len(idempotencyIssues) > 0 {
			adminWriteValidationFailure(writer, request, idempotencyIssues)
			return
		}
		patch, contractErr := providerBatchDecodePatch(fields)
		if contractErr != nil {
			providerBatchWriteContractError(deps, writer, request, contractErr)
			return
		}
		if !hasProviderBatchPatchChanges(patch) {
			providerBatchWriteProblem(deps, writer, request, http.StatusBadRequest, providerBatchCodeNothingToApply)
			return
		}

		claimKey := providerBatchClaimKey(previewToken, idempotencyKey)
		fingerprint := providerBatchFingerprint(previewToken, previewRevision, providerIDs, patch, excludeIDs)

		existing, err := deps.Store.FindProviderBatchApplyOperation(request.Context(), claimKey, previewToken)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}
		if existing.Status == store.ProviderBatchApplyStatusReplay {
			providerBatchWriteApplyResult(deps, writer, request, existing.Result)
			return
		}
		if existing.Status == store.ProviderBatchApplyStatusIdempotencyConflict ||
			existing.Status == store.ProviderBatchApplyStatusPreviewConsumed {
			providerBatchWriteConflict(deps, writer, request, existing.Status, idempotencyKey != nil)
			return
		}

		snapshot, err := providerBatchLoadPreview(request.Context(), deps.ProviderUndoKV, previewToken)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}
		if snapshot == nil {
			providerBatchWriteProblem(deps, writer, request, http.StatusGone, providerBatchCodePreviewExpired)
			return
		}
		if previewRevision != snapshot.PreviewRevision ||
			!equalInt64List(providerIDs, snapshot.ProviderIDs) ||
			string(marshalProviderBatchPatchSerialized(patch)) != snapshot.PatchSerialized {
			providerBatchWriteProblem(deps, writer, request, http.StatusConflict, providerBatchCodePreviewStale)
			return
		}

		excludeSet := make(map[int64]bool, len(excludeIDs))
		for _, providerID := range excludeIDs {
			excludeSet[providerID] = true
		}
		effectiveIDs := make([]int64, 0, len(providerIDs))
		for _, providerID := range providerIDs {
			if !excludeSet[providerID] {
				effectiveIDs = append(effectiveIDs, providerID)
			}
		}
		if len(effectiveIDs) == 0 {
			providerBatchWriteProblem(deps, writer, request, http.StatusBadRequest, providerBatchCodeNothingToApply)
			return
		}

		changedFields := changedProviderPatchFields(patch)
		preimages, undoPreimage := providerBatchExpectedPreimages(snapshot, providerIDs)
		if preimages == nil {
			providerBatchWriteProblem(deps, writer, request, http.StatusConflict, providerBatchCodePreviewStale)
			return
		}

		updates := buildProviderBatchApplyUpdates(patch)
		groups := providerBatchUpdateGroups(updates, changedFields, preimages, effectiveIDs, excludeSet)

		undoToken, err := providerBatchRandomToken("provider_patch_undo_")
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}
		operationID, err := providerBatchRandomToken("provider_patch_apply_")
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}

		durableUndo := make(map[string]map[string]any, len(effectiveIDs))
		undoRestorable := true
		for _, providerID := range effectiveIDs {
			values := undoPreimage[providerID]
			filtered := make(map[string]any, len(values))
			for key, value := range values {
				if providerBatchSensitiveUndoKeys[key] {
					undoRestorable = false
					continue
				}
				filtered[key] = value
			}
			durableUndo[fmt.Sprintf("%d", providerID)] = filtered
		}

		decision, err := deps.Store.ApplyProviderBatchOperationIfUnchanged(
			request.Context(),
			store.ProviderBatchApplyInput{
				ClaimKey:           claimKey,
				PreviewToken:       previewToken,
				PayloadFingerprint: fingerprint,
				OperationID:        operationID,
				UndoToken:          undoToken,
				UndoTTSec:          ProviderPatchUndoTTLSeconds,
				UndoRestorable:     undoRestorable,
				PreviewProviderIDs: providerIDs,
				EffectiveIDs:       effectiveIDs,
				Expected:           preimages,
				Groups:             groups,
				PostCommitEffects:  providerBatchPostCommitEffects(updates, changedFields, undoPreimage),
			},
		)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}
		if decision.Status == store.ProviderBatchApplyStatusStale {
			providerBatchWriteProblem(deps, writer, request, http.StatusConflict, providerBatchCodePreviewStale)
			return
		}
		if decision.Status == store.ProviderBatchApplyStatusIdempotencyConflict ||
			decision.Status == store.ProviderBatchApplyStatusPreviewConsumed {
			providerBatchWriteConflict(deps, writer, request, decision.Status, idempotencyKey != nil)
			return
		}

		// 撤销快照（Node 的 restoreProviderPatchUndoSnapshot）：与应用同源，形状与单条更新一致。
		volatilePreimage := make(map[string]map[string]any, len(effectiveIDs))
		for _, providerID := range effectiveIDs {
			key := fmt.Sprintf("%d", providerID)
			if values, ok := durableUndo[key]; ok {
				volatilePreimage[key] = values
			}
		}
		if err := putProviderPatchUndo(request.Context(), deps.ProviderUndoKV, ProviderPatchUndo{
			UndoToken:   undoToken,
			OperationID: operationID,
			ProviderIDs: effectiveIDs,
			Preimage:    volatilePreimage,
		}); err != nil && deps.Logger != nil {
			deps.Logger.Warn("admin_batchpatch_undo_snapshot_failed", map[string]any{
				"operationId": operationID,
				"error":       err.Error(),
			})
		}

		providerBatchRunPostCommitEffects(deps, request, decision.Result)
		providerEmitWriteAudit(deps, request, "provider.batch_patch_apply", effectiveIDs[0], "", map[string]any{
			"operationId":   operationID,
			"updatedCount":  decision.Result.ApplyResult.UpdatedCount,
			"changedFields": changedFields,
		}, true, "")
		providerBatchWriteApplyResult(deps, writer, request, decision.Result)
	}
}

// providerBatchRunPostCommitEffects 复刻 runProviderBatchPostCommitEffects（actions:2139-2213）的
// **可移植部分**：缓存失效广播。阈值归零时的强制闭合熔断走既有熔断缝，缺失即不做事（与 Node 的
// 「失败只告警」同判）。
func providerBatchRunPostCommitEffects(
	deps Deps,
	request *http.Request,
	result *store.ProviderBatchApplyResult,
) {
	if result == nil {
		return
	}
	adminPublishDomain(deps, request, cfgsync.DomainProviders)
	if result.PostCommitEffects.CircuitBreakerChanged {
		adminPublishDomain(deps, request, cfgsync.DomainProviders)
	}
	// 登记差异（Note）：Node 的 clearLimit5hCostCache 清的是**供应商级**成本缓存
	// （clearSingleProviderCostCache），而 Go 的 Deps.Invalidator 只覆盖 key/user 两级缓存
	// （见 deps.go 的 Invalidator 声明）。新增供应商级失效面属 Deps 冻结面变更，不在本任务的
	// 授权范围，故此处只做配置域广播（providers 域会使缓存重建）。影响面：把 limit_5h_reset_mode
	// 改成固定窗口后，旧窗口中已计的成本可能在最长一个 5h 窗口内继续生效。
}

// providerBatchExpectedPreimages 复刻 buildExpectedProviderBatchPreimages（actions:1633-1674）。
//
// 行数必须等于 changedFields 数，否则返回 nil（调用方按 PREVIEW_STALE 处理）。
func providerBatchExpectedPreimages(
	snapshot *providerBatchPatchPreviewSnapshot,
	providerIDs []int64,
) ([]store.ProviderBatchExpectedPreimage, map[int64]map[string]any) {
	rowsByProvider := make(map[int64][]providerBatchPreviewRow, len(providerIDs))
	for _, row := range snapshot.Rows {
		rowsByProvider[row.ProviderID] = append(rowsByProvider[row.ProviderID], row)
	}
	expected := make([]store.ProviderBatchExpectedPreimage, 0, len(providerIDs))
	undo := make(map[int64]map[string]any, len(providerIDs))
	for _, providerID := range providerIDs {
		key := fmt.Sprintf("%d", providerID)
		providerType, hasType := snapshot.ProviderTypes[key]
		isEnabled, hasEnabled := snapshot.ProviderEnabled[key]
		rows, hasRows := rowsByProvider[providerID]
		if !hasType || !hasEnabled || !hasRows || len(rows) != len(snapshot.ChangedFields) {
			return nil, nil
		}
		values := map[string]any{"isEnabled": isEnabled}
		undoValues := make(map[string]any, len(rows))
		for _, row := range rows {
			spec, ok := providerPatchFieldByName(row.Field)
			if !ok {
				return nil, nil
			}
			before := jsonDisplayValue(row.Before)
			values[spec.ProviderKey] = before
			undoValues[spec.ProviderKey] = before
		}
		expected = append(expected, store.ProviderBatchExpectedPreimage{
			ProviderID:   providerID,
			ProviderType: providerType,
			IsEnabled:    isEnabled,
			Values:       values,
		})
		undo[providerID] = undoValues
	}
	return expected, undo
}

// providerBatchUpdateGroups 复刻 applyProviderBatchPatch 的分组段（actions:2404-2429）。
func providerBatchUpdateGroups(
	updates providerBatchApplyUpdates,
	changedFields []string,
	preimages []store.ProviderBatchExpectedPreimage,
	effectiveIDs []int64,
	excludeSet map[int64]bool,
) []store.ProviderBatchUpdateGroup {
	hasTypeSpecific := false
	for _, field := range changedFields {
		if providerFieldGroupOf(field) != providerGroupAny {
			hasTypeSpecific = true
			break
		}
	}
	if !hasTypeSpecific {
		return []store.ProviderBatchUpdateGroup{{IDs: effectiveIDs, Updates: map[string]any(updates)}}
	}

	byType := make(map[string][]int64)
	for _, expected := range preimages {
		if excludeSet[expected.ProviderID] {
			continue
		}
		byType[expected.ProviderType] = append(byType[expected.ProviderType], expected.ProviderID)
	}
	types := make([]string, 0, len(byType))
	for providerType := range byType {
		types = append(types, providerType)
	}
	sort.Strings(types)
	groups := make([]store.ProviderBatchUpdateGroup, 0, len(types))
	for _, providerType := range types {
		filtered := filterProviderBatchUpdatesByType(updates, providerType)
		if len(filtered) == 0 {
			continue
		}
		groups = append(groups, store.ProviderBatchUpdateGroup{IDs: byType[providerType], Updates: filtered})
	}
	return groups
}

// filterProviderBatchUpdatesByType 复刻 filterRepositoryUpdatesByProviderType（actions:1979-1997）：
// 只保留该类型「专属」的列，其余列对所有类型都适用。
func filterProviderBatchUpdatesByType(
	updates providerBatchApplyUpdates,
	providerType string,
) map[string]any {
	filtered := make(map[string]any, len(updates))
	for field, value := range updates {
		group := providerFieldGroupOf(field)
		if group != providerGroupAny && !providerTypeMatchesGroup(providerType, group) {
			continue
		}
		filtered[field] = value
	}
	return filtered
}

// providerTypeMatchesGroup 复刻 isClaudeProviderType / isCodexProviderType / ...（actions:1934-1952）。
func providerTypeMatchesGroup(providerType string, group providerFieldGroup) bool {
	switch group {
	case providerGroupClaude:
		return providerType == "claude" || providerType == "claude-auth"
	case providerGroupCodex:
		return providerType == "codex"
	case providerGroupGemini:
		return providerType == "gemini" || providerType == "gemini-cli"
	case providerGroupOpenAICompat:
		return providerType == "openai-compatible"
	default:
		return true
	}
}

// generateProviderBatchPreviewRows 复刻 generatePreviewRows（actions:2019-2078）。
func generateProviderBatchPreviewRows(
	providerIDs []int64,
	matched map[int64]store.AdminProviderBatchRow,
	patch providerBatchPatch,
	changedFields []string,
) []providerBatchPreviewRow {
	rows := make([]providerBatchPreviewRow, 0, len(providerIDs)*len(changedFields))
	for _, providerID := range providerIDs {
		row, ok := matched[providerID]
		if !ok {
			continue
		}
		for _, field := range changedFields {
			spec, _ := providerPatchFieldByName(field)
			operation := patch[field]
			before := row.Values[spec.ProviderKey]
			after := computeProviderPatchPreviewAfterValue(spec, operation)
			compatible, reason := providerFieldGroupCompatible(spec, row.ProviderType)
			if compatible {
				rows = append(rows, providerBatchPreviewRow{
					ProviderID: row.ID, ProviderName: row.Name, Field: field,
					Status: "changed", Before: before, After: after,
				})
				continue
			}
			rows = append(rows, providerBatchPreviewRow{
				ProviderID: row.ID, ProviderName: row.Name, Field: field,
				Status: "skipped", Before: before, After: after, SkipReason: reason,
			})
		}
	}
	return rows
}

// providerFieldGroupCompatible 复刻 generatePreviewRows 里的 skipped 判定与理由文案（actions:2041-2060）。
func providerFieldGroupCompatible(spec providerPatchFieldSpec, providerType string) (bool, string) {
	if spec.Group == providerGroupAny || providerTypeMatchesGroup(providerType, spec.Group) {
		return true, ""
	}
	return false, fmt.Sprintf("Field %q is only applicable to %s providers", spec.Field, groupDisplayName(spec.Group))
}

func groupDisplayName(group providerFieldGroup) string {
	switch group {
	case providerGroupClaude:
		return "claude/claude-auth"
	case providerGroupGemini:
		return "gemini/gemini-cli"
	default:
		return string(group)
	}
}

// providerBatchPostCommitEffects 复刻 postCommitEffects 参数的构造（actions:2464-2481）。
func providerBatchPostCommitEffects(
	updates providerBatchApplyUpdates,
	changedFields []string,
	_ map[int64]map[string]any,
) store.ProviderBatchPostCommitEffects {
	_, has5hResetMode := updates["limit_5h_reset_mode"]
	circuitBreakerChanged := false
	nextThreshold := any(nil)
	for _, field := range changedFields {
		switch field {
		case "circuit_breaker_failure_threshold",
			"circuit_breaker_open_duration",
			"circuit_breaker_half_open_success_threshold":
			circuitBreakerChanged = true
		}
	}
	if value, ok := updates["circuit_breaker_failure_threshold"]; ok {
		if number, ok := value.(int64); ok {
			nextThreshold = number
		}
	}
	return store.ProviderBatchPostCommitEffects{
		ClearLimit5hCostCache:              has5hResetMode,
		CircuitBreakerChanged:              circuitBreakerChanged,
		NextCircuitBreakerFailureThreshold: nextThreshold,
	}
}

// ---- 入参解码与作答工具 ----

func providerBatchDecodeAny(raw json.RawMessage) (any, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func providerBatchInvalidParam(key, message string) invalidParam {
	return invalidParam{Path: []any{key}, Code: "invalid_type", Message: message}
}

func providerBatchDecodeProviderIDs(
	fields map[string]json.RawMessage,
	key string,
	optional bool,
) ([]int64, []invalidParam) {
	raw, given := fields[key]
	if !given {
		if optional {
			return nil, nil
		}
		return nil, []invalidParam{providerBatchInvalidParam(key, "Required")}
	}
	value, err := providerBatchDecodeAny(raw)
	if err != nil {
		return nil, []invalidParam{providerBatchInvalidParam(key, "Invalid input: expected array")}
	}
	items, ok := value.([]any)
	if !ok {
		return nil, []invalidParam{providerBatchInvalidParam(key, "Invalid input: expected array")}
	}
	if len(items) == 0 || len(items) > providerBatchMaxSize {
		return nil, []invalidParam{providerBatchInvalidParam(key, "Array size out of range")}
	}
	ids := make([]int64, 0, len(items))
	for _, item := range items {
		number, ok := item.(float64)
		if !ok || number <= 0 || number != float64(int64(number)) {
			return nil, []invalidParam{providerBatchInvalidParam(key, "Invalid input: expected positive integer")}
		}
		ids = append(ids, int64(number))
	}
	return dedupeProviderIDs(ids), nil
}

func providerBatchDecodeString(fields map[string]json.RawMessage, key string) (string, []invalidParam) {
	raw, given := fields[key]
	if !given {
		return "", []invalidParam{providerBatchInvalidParam(key, "Required")}
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil || strings.TrimSpace(text) == "" {
		return "", []invalidParam{providerBatchInvalidParam(key, "Invalid input: expected non-empty string")}
	}
	return strings.TrimSpace(text), nil
}

func providerBatchDecodeOptionalString(fields map[string]json.RawMessage, key string) (*string, []invalidParam) {
	raw, given := fields[key]
	if !given || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return nil, []invalidParam{providerBatchInvalidParam(key, "Invalid input: expected string")}
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || len([]rune(trimmed)) > 128 {
		return nil, []invalidParam{providerBatchInvalidParam(key, "String length out of range")}
	}
	return &trimmed, nil
}

// providerBatchDecodePatch 解析 patch 字段（Node 侧 `.optional().default({})`）。
func providerBatchDecodePatch(fields map[string]json.RawMessage) (providerBatchPatch, *providerPatchContractError) {
	raw, given := fields["patch"]
	if !given || string(raw) == "null" {
		return normalizeProviderBatchPatchDraft(map[string]any{})
	}
	value, err := providerBatchDecodeAny(raw)
	if err != nil {
		return nil, patchContractError("__root__", "Patch draft must be an object")
	}
	draft, ok := value.(map[string]any)
	if !ok {
		return nil, patchContractError("__root__", "Patch draft must be an object")
	}
	return normalizeProviderBatchPatchDraft(draft)
}

func providerBatchWriteContractError(
	deps Deps,
	writer http.ResponseWriter,
	request *http.Request,
	err *providerPatchContractError,
) {
	adminProblemWriter(deps).WriteProblem(
		writer, request, http.StatusBadRequest, providerPatchErrorCode, err.Message)
}

func providerBatchWriteProblem(
	deps Deps,
	writer http.ResponseWriter,
	request *http.Request,
	status int,
	code string,
) {
	adminProblemWriter(deps).WriteProblem(writer, request, status, code, code)
}

// providerBatchWriteConflict 复刻 buildProviderBatchConflictError（actions:2080-2097）：
// 有幂等键的冲突报 IDEMPOTENCY_CONFLICT，其余一律 PREVIEW_STALE（都是 409）。
func providerBatchWriteConflict(
	deps Deps,
	writer http.ResponseWriter,
	request *http.Request,
	status string,
	hasIdempotencyKey bool,
) {
	code := providerBatchCodePreviewStale
	if status == store.ProviderBatchApplyStatusIdempotencyConflict && hasIdempotencyKey {
		code = providerBatchCodeIdempotencyConflict
	}
	providerBatchWriteProblem(deps, writer, request, http.StatusConflict, code)
}

func providerBatchWriteApplyResult(
	deps Deps,
	writer http.ResponseWriter,
	request *http.Request,
	result *store.ProviderBatchApplyResult,
) {
	if result == nil {
		providerBatchWriteProblem(deps, writer, request, http.StatusConflict, providerBatchCodePreviewStale)
		return
	}
	adminWriteJSON(writer, http.StatusOK, providerBatchPatchApplyResult{
		OperationID:   result.ApplyResult.OperationID,
		AppliedAt:     result.ApplyResult.AppliedAt,
		UpdatedCount:  result.ApplyResult.UpdatedCount,
		UndoToken:     result.ApplyResult.UndoToken,
		UndoExpiresAt: result.ApplyResult.UndoExpiresAt,
	})
}

// providerBatchLoadPreview 读取预览快照；不存在返回 nil（Node 的 store.get）。
func providerBatchLoadPreview(
	ctx context.Context,
	kv ProviderUndoKV,
	token string,
) (*providerBatchPatchPreviewSnapshot, error) {
	payload, found, err := kv.Get(ctx, providerPreviewPrefix+token)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	var snapshot providerBatchPatchPreviewSnapshot
	if err := json.Unmarshal(payload, &snapshot); err != nil {
		return nil, fmt.Errorf("adminapi: 解析预览快照失败: %w", err)
	}
	return &snapshot, nil
}

// providerBatchClaimKey 复刻 buildProviderBatchApplyClaimKey（actions:1629-1631）。
func providerBatchClaimKey(previewToken string, idempotencyKey *string) string {
	if idempotencyKey != nil {
		return "idempotency:" + *idempotencyKey
	}
	return "preview:" + previewToken
}

// providerBatchFingerprint 复刻 buildProviderBatchApplyFingerprint（actions:1619-1627）：
// sha256(JSON.stringify({previewToken, previewRevision, providerIds, patch, excludeProviderIds}))。
//
// **登记差异**：本函数用与 JS 相同的键序（对象字面量书写序 = PATCH_FIELDS 序）与不转义 HTML 的
// 编码器，力求与 JSON.stringify 逐字节一致；但 JS 的数值格式化（如 `1e+21`）与极少数转义细节
// 不保证相同。影响面仅限「同一 claimKey 由 Node 写入、Go 重放」这一跨实现场景（双跑期两侧不会
// 同时处理同一请求），且账本对 previewToken 的唯一约束仍保证不会重复应用。
func providerBatchFingerprint(
	previewToken string,
	previewRevision string,
	providerIDs []int64,
	patch providerBatchPatch,
	excludeIDs []int64,
) string {
	var buffer bytes.Buffer
	buffer.WriteString(`{"previewToken":`)
	buffer.Write(marshalJSONNoHTMLEscape(previewToken))
	buffer.WriteString(`,"previewRevision":`)
	buffer.Write(marshalJSONNoHTMLEscape(previewRevision))
	buffer.WriteString(`,"providerIds":`)
	buffer.Write(marshalJSONNoHTMLEscape(providerIDs))
	buffer.WriteString(`,"patch":`)
	buffer.Write(marshalProviderBatchPatchSerialized(patch))
	buffer.WriteString(`,"excludeProviderIds":`)
	buffer.Write(marshalJSONNoHTMLEscape(excludeIDs))
	buffer.WriteString(`}`)
	sum := sha256.Sum256(buffer.Bytes())
	return hex.EncodeToString(sum[:])
}

// marshalProviderBatchPatchSerialized 按契约表序输出 `{field: {mode, value?}}`（Node 的 JSON.stringify）。
func marshalProviderBatchPatchSerialized(patch providerBatchPatch) []byte {
	var buffer bytes.Buffer
	buffer.WriteString("{")
	for index, spec := range providerPatchFieldSpecs {
		operation := patch[spec.Field]
		if index > 0 {
			buffer.WriteString(",")
		}
		buffer.Write(marshalJSONNoHTMLEscape(spec.Field))
		buffer.WriteString(":{\"mode\":")
		buffer.Write(marshalJSONNoHTMLEscape(string(operation.Mode)))
		if operation.Mode == providerPatchModeSet {
			buffer.WriteString(`,"value":`)
			buffer.Write(marshalJSONNoHTMLEscape(jsonDisplayValue(operation.SetValue)))
		}
		buffer.WriteString("}")
	}
	buffer.WriteString("}")
	return buffer.Bytes()
}

func marshalJSONNoHTMLEscape(value any) []byte {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return []byte("null")
	}
	return bytes.TrimRight(buffer.Bytes(), "\n")
}

func providerBatchRandomToken(prefix string) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	// RFC 4122 v4 形状，与 Node 的 crypto.randomUUID() 同形（小写、带连字符）。
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	uuid := fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16])
	return prefix + uuid, nil
}

// providerBatchSensitiveUndoKeys 复刻 SENSITIVE_PROVIDER_BATCH_UNDO_KEYS
// （src/lib/provider-batch-patch-error-codes.ts:19-22）：含这些键的撤销快照不可持久恢复。
var providerBatchSensitiveUndoKeys = map[string]bool{
	"proxyUrl":          true,
	"mcpPassthroughUrl": true,
}

func dedupeProviderIDs(ids []int64) []int64 {
	seen := make(map[int64]bool, len(ids))
	unique := make([]int64, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		unique = append(unique, id)
	}
	sort.Slice(unique, func(left, right int) bool { return unique[left] < unique[right] })
	return unique
}

func equalInt64List(left, right []int64) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func joinInt64(values []int64, separator string) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, fmt.Sprintf("%d", value))
	}
	return strings.Join(parts, separator)
}

func formatAdminISO(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}
