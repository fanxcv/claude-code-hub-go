package store

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// 本文件是批量补丁的**持久账本与原子应用**（Node 侧 src/repository/provider.ts:1540-1872）。
//
// 唯一真源：
//   - resolveProviderBatchApplyOperation（:1551-1580）：claimKey/previewToken 三态判定
//   - findProviderBatchApplyOperation（:1616-1642）：过期过滤 + 命中即重放
//   - applyProviderBatchOperationIfUnchanged（:1652-1868）：指纹与 TTL 校验 → 清理过期 →
//     事务内「账本占位（on conflict do nothing）→ 行锁 → 前像逐字段比对 → 分组更新 → 账本完成」
//   - PROVIDER_BATCH_APPLY_LEDGER_TTL_MS（:1510）＝ 24 小时
//
// 为什么整段必须在**一个事务**里（含占位）：Node 的语义是「幂等键或预览令牌只能被消费一次」，
// 占位行与更新必须同生共死；若占位先提交、更新失败，预览令牌就被永久烧掉（用户看到「预览已过期」
// 而实际什么都没改）——那是比不接管更坏的行为。

// ProviderBatchApplyLedgerTTL 是账本保留期（Node 的 PROVIDER_BATCH_APPLY_LEDGER_TTL_MS）。
const ProviderBatchApplyLedgerTTL = 24 * time.Hour

// ProviderBatchExpectedPreimage 是某供应商的前像期望（Node 的 ProviderBatchExpectedPreimage）。
//
// Values 的键是 **camelCase 的 Provider 字段名**（不是列名）：账本行要能被 Node 读懂，
// 而 Node 的撤销回写走的就是 Provider 字段空间（actions/providers.ts:1836-1901）。
type ProviderBatchExpectedPreimage struct {
	ProviderID   int64          `json:"providerId"`
	ProviderType string         `json:"providerType"`
	IsEnabled    bool           `json:"isEnabled"`
	Values       map[string]any `json:"values"`
}

// ProviderBatchUpdateGroup 是一组「同类型供应商 + 同一批列更新」（Node 的 ProviderBatchUpdateGroup）。
//
// Updates 的键是 providers **列名**（snake_case），值已按列类型绑定（见 adminProviderBind）。
type ProviderBatchUpdateGroup struct {
	IDs     []int64
	Updates map[string]any
}

// ProviderBatchPostCommitEffects 是提交后效应的参数（Node 的 postCommitEffects）。
type ProviderBatchPostCommitEffects struct {
	ClearLimit5hCostCache              bool `json:"clearLimit5hCostCache"`
	CircuitBreakerChanged              bool `json:"circuitBreakerChanged"`
	NextCircuitBreakerFailureThreshold any  `json:"nextCircuitBreakerFailureThreshold"`
}

// ProviderBatchApplyResult 是账本里的结果（Node 的 ProviderBatchApplyLedgerResult）。
type ProviderBatchApplyResult struct {
	ApplyResult          ProviderBatchApplyOutcome       `json:"applyResult"`
	PreviewProviderIDs   []int64                         `json:"previewProviderIds"`
	EffectiveProviderIDs []int64                         `json:"effectiveProviderIds"`
	Preimages            []ProviderBatchExpectedPreimage `json:"preimages"`
	UndoRestorable       bool                            `json:"undoRestorable"`
	PostCommitEffects    ProviderBatchPostCommitEffects  `json:"postCommitEffects"`
}

// ProviderBatchApplyOutcome 是 apply 的对外结果（Node 的 applyResult）。
type ProviderBatchApplyOutcome struct {
	OperationID   string `json:"operationId"`
	AppliedAt     string `json:"appliedAt"`
	UpdatedCount  int64  `json:"updatedCount"`
	UndoToken     string `json:"undoToken"`
	UndoExpiresAt string `json:"undoExpiresAt"`
}

// ProviderBatchApplyInput 是一次批量应用的入参。
type ProviderBatchApplyInput struct {
	ClaimKey           string
	PreviewToken       string
	PayloadFingerprint string
	OperationID        string
	UndoToken          string
	UndoTracer         string
	UndoTTSec          int
	UndoRestorable     bool
	PreviewProviderIDs []int64
	EffectiveIDs       []int64
	Expected           []ProviderBatchExpectedPreimage
	Groups             []ProviderBatchUpdateGroup
	PostCommitEffects  ProviderBatchPostCommitEffects
	Now                time.Time
}

// ProviderBatchApplyDecision 是三态判定结果（Node 的 ApplyProviderBatchOperationIfUnchangedResult）。
type ProviderBatchApplyDecision struct {
	Status        string
	Result        *ProviderBatchApplyResult
	UndoAvailable bool
}

const (
	// ProviderBatchApplyStatusApplied 表示本次真的写入并完成账本。
	ProviderBatchApplyStatusApplied = "applied"
	// ProviderBatchApplyStatusReplay 表示同一 claimKey 的既有成功结果被原样回放。
	ProviderBatchApplyStatusReplay = "replay"
	// ProviderBatchApplyStatusStale 表示前像与当前行不符（Node 的 PREVIEW_STALE）。
	ProviderBatchApplyStatusStale = "stale"
	// ProviderBatchApplyStatusIdempotencyConflict 表示同一 claimKey 但载荷不同。
	ProviderBatchApplyStatusIdempotencyConflict = "idempotency_conflict"
	// ProviderBatchApplyStatusPreviewConsumed 表示预览令牌已被占用或前一条仍在 applying。
	ProviderBatchApplyStatusPreviewConsumed = "preview_consumed"
)

// FindProviderBatchApplyOperation 复刻 findProviderBatchApplyOperation（:1616-1642）。
func (p *Pools) FindProviderBatchApplyOperation(
	ctx context.Context,
	claimKey string,
	previewToken string,
) (*ProviderBatchApplyDecision, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `SELECT row_to_json(t)::text FROM (
		SELECT claim_key, preview_token, payload_fingerprint, operation_id, undo_token,
		       undo_expires_at, undo_consumed_at, status, result
		  FROM provider_batch_apply_operations
		 WHERE expires_at > now() AND (claim_key = $1 OR preview_token = $2)
	) t`
	rows, err := pool.Query(ctx, query, claimKey, previewToken)
	if err != nil {
		return nil, fmt.Errorf("store: 读取批量补丁账本失败: %w", err)
	}
	defer rows.Close()

	records, err := decodeProviderBatchLedgerRows(rows)
	if err != nil {
		return nil, err
	}
	return resolveProviderBatchLedger(records, claimKey, previewToken, "", time.Now()), nil
}

type providerBatchLedgerRow struct {
	ClaimKey           string                    `json:"claim_key"`
	PreviewToken       string                    `json:"preview_token"`
	PayloadFingerprint string                    `json:"payload_fingerprint"`
	OperationID        string                    `json:"operation_id"`
	UndoToken          string                    `json:"undo_token"`
	UndoExpiresAt      *time.Time                `json:"undo_expires_at"`
	UndoConsumedAt     *time.Time                `json:"undo_consumed_at"`
	Status             string                    `json:"status"`
	Result             *ProviderBatchApplyResult `json:"result"`
}

func decodeProviderBatchLedgerRows(rows pgx.Rows) ([]providerBatchLedgerRow, error) {
	records := make([]providerBatchLedgerRow, 0, 2)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("store: 扫描批量补丁账本失败: %w", err)
		}
		var record providerBatchLedgerRow
		if err := json.Unmarshal([]byte(payload), &record); err != nil {
			return nil, fmt.Errorf("store: 解析批量补丁账本失败: %w", err)
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

// resolveProviderBatchLedger 复刻 resolveProviderBatchApplyOperation（:1551-1580）。
func resolveProviderBatchLedger(
	records []providerBatchLedgerRow,
	claimKey string,
	previewToken string,
	fingerprint string,
	now time.Time,
) *ProviderBatchApplyDecision {
	for index := range records {
		record := records[index]
		if record.ClaimKey != claimKey {
			continue
		}
		if record.PreviewToken != previewToken ||
			(fingerprint != "" && record.PayloadFingerprint != fingerprint) {
			return &ProviderBatchApplyDecision{Status: ProviderBatchApplyStatusIdempotencyConflict}
		}
		if record.Status == "applied" && record.Result != nil {
			undoAvailable := record.Result.UndoRestorable &&
				record.UndoConsumedAt == nil &&
				record.UndoExpiresAt != nil &&
				record.UndoExpiresAt.After(now)
			return &ProviderBatchApplyDecision{
				Status:        ProviderBatchApplyStatusReplay,
				Result:        record.Result,
				UndoAvailable: undoAvailable,
			}
		}
		// applying 行绝不由本函数续作：它的状态需要人工修复（Node :1571-1573 同判）。
		return &ProviderBatchApplyDecision{Status: ProviderBatchApplyStatusPreviewConsumed}
	}
	for index := range records {
		if records[index].PreviewToken == previewToken {
			return &ProviderBatchApplyDecision{Status: ProviderBatchApplyStatusPreviewConsumed}
		}
	}
	return &ProviderBatchApplyDecision{Status: "not_found"}
}

// ApplyProviderBatchOperationIfUnchanged 复刻 applyProviderBatchOperationIfUnchanged（:1652-1868）。
func (p *Pools) ApplyProviderBatchOperationIfUnchanged(
	ctx context.Context,
	input ProviderBatchApplyInput,
) (*ProviderBatchApplyDecision, error) {
	if !isHexDigest64(input.PayloadFingerprint) {
		return nil, fmt.Errorf("store: 批量补丁指纹必须是 64 位十六进制摘要")
	}
	if input.UndoTTSec < 1 {
		return nil, fmt.Errorf("store: 批量补丁撤销 TTL 必须是正整数")
	}

	expectedByID := make(map[int64]ProviderBatchExpectedPreimage, len(input.Expected))
	for _, entry := range input.Expected {
		if _, duplicated := expectedByID[entry.ProviderID]; duplicated {
			return &ProviderBatchApplyDecision{Status: ProviderBatchApplyStatusStale}, nil
		}
		expectedByID[entry.ProviderID] = entry
	}
	if len(expectedByID) == 0 {
		return &ProviderBatchApplyDecision{Status: ProviderBatchApplyStatusStale}, nil
	}

	previewIDs := sortedUniqueInt64(mapKeys(expectedByID))
	effectiveIDs := sortedUniqueInt64(input.EffectiveIDs)
	if len(effectiveIDs) == 0 {
		return &ProviderBatchApplyDecision{Status: ProviderBatchApplyStatusStale}, nil
	}
	for _, providerID := range effectiveIDs {
		if _, ok := expectedByID[providerID]; !ok {
			return &ProviderBatchApplyDecision{Status: ProviderBatchApplyStatusStale}, nil
		}
	}

	effectiveSet := make(map[int64]bool, len(effectiveIDs))
	for _, providerID := range effectiveIDs {
		effectiveSet[providerID] = true
	}
	groupSeen := make(map[int64]bool)
	for _, group := range input.Groups {
		for _, providerID := range uniqueInt64(group.IDs) {
			if groupSeen[providerID] {
				return nil, fmt.Errorf("store: 供应商 %d 出现在多个批量更新分组里", providerID)
			}
			if !effectiveSet[providerID] {
				return &ProviderBatchApplyDecision{Status: ProviderBatchApplyStatusStale}, nil
			}
			if _, ok := expectedByID[providerID]; !ok {
				return &ProviderBatchApplyDecision{Status: ProviderBatchApplyStatusStale}, nil
			}
			groupSeen[providerID] = true
		}
	}

	storedPreimages := make([]ProviderBatchExpectedPreimage, 0, len(effectiveIDs))
	for _, providerID := range effectiveIDs {
		expected, ok := expectedByID[providerID]
		if !ok || len(expected.Values) == 0 {
			return &ProviderBatchApplyDecision{Status: ProviderBatchApplyStatusStale}, nil
		}
		storedPreimages = append(storedPreimages, ProviderBatchExpectedPreimage{
			ProviderID:   providerID,
			ProviderType: expected.ProviderType,
			IsEnabled:    expected.IsEnabled,
			Values:       cloneAnyMap(expected.Values),
		})
	}

	now := input.Now
	if now.IsZero() {
		now = time.Now()
	}
	ledgerExpiresAt := now.Add(ProviderBatchApplyLedgerTTL)

	pool, err := p.Writer()
	if err != nil {
		return nil, err
	}

	// 保留期清理放在加锁事务之外（Node :1709-1712 同判：大堆积不能拉长临界区）。
	if _, err := pool.Exec(ctx,
		`DELETE FROM provider_batch_apply_operations WHERE expires_at < $1`, now,
	); err != nil {
		return nil, fmt.Errorf("store: 清理过期批量补丁账本失败: %w", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: 批量补丁事务开启失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		INSERT INTO provider_batch_apply_operations (
			claim_key, preview_token, payload_fingerprint, operation_id, undo_token,
			undo_expires_at, undo_consumed_at, status, result, expires_at, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, NULL, NULL, 'applying', NULL, $6, $7, $7)
		ON CONFLICT (claim_key) DO NOTHING`,
		input.ClaimKey, input.PreviewToken, input.PayloadFingerprint, input.OperationID,
		input.UndoToken, ledgerExpiresAt, now,
	)
	if err != nil {
		return nil, fmt.Errorf("store: 占位批量补丁账本失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		rows, err := tx.Query(ctx, `
			SELECT row_to_json(t)::text FROM (
				SELECT claim_key, preview_token, payload_fingerprint, operation_id, undo_token,
				       undo_expires_at, undo_consumed_at, status, result
				  FROM provider_batch_apply_operations
				 WHERE claim_key = $1 OR preview_token = $2
			) t`, input.ClaimKey, input.PreviewToken)
		if err != nil {
			return nil, fmt.Errorf("store: 读取冲突账本行失败: %w", err)
		}
		records, err := decodeProviderBatchLedgerRows(rows)
		rows.Close()
		if err != nil {
			return nil, err
		}
		decision := resolveProviderBatchLedger(records, input.ClaimKey, input.PreviewToken, input.PayloadFingerprint, now)
		if decision.Status == "not_found" {
			return nil, fmt.Errorf("store: 批量补丁账本冲突行消失")
		}
		return decision, nil
	}

	lockedRows, err := lockProviderRowsForUpdate(ctx, tx, previewIDs)
	if err != nil {
		return nil, err
	}
	if len(lockedRows) != len(previewIDs) {
		return &ProviderBatchApplyDecision{Status: ProviderBatchApplyStatusStale}, nil
	}
	for _, row := range lockedRows {
		expected, ok := expectedByID[row.ID]
		if !ok || row.ProviderType != expected.ProviderType || row.IsEnabled != expected.IsEnabled {
			return &ProviderBatchApplyDecision{Status: ProviderBatchApplyStatusStale}, nil
		}
		for key, expectedValue := range expected.Values {
			currentValue, ok := row.Columns[providerColumnForProviderKey(key)]
			if !ok || !providerBatchPreimageValueEqual(currentValue, expectedValue) {
				return &ProviderBatchApplyDecision{Status: ProviderBatchApplyStatusStale}, nil
			}
		}
	}

	appliedAt := time.Now()
	var updatedCount int64
	for _, group := range input.Groups {
		ids := sortedUniqueInt64(group.IDs)
		if len(ids) == 0 || len(group.Updates) == 0 {
			continue
		}
		columns, values, err := adminProviderBind(group.Updates)
		if err != nil {
			return nil, err
		}
		if len(columns) == 0 {
			continue
		}
		assignments := make([]string, 0, len(columns)+1)
		params := make([]any, 0, len(columns)+1)
		for index, column := range columns {
			assignments = append(assignments, fmt.Sprintf("%s = $%d", column, index+1))
			params = append(params, values[index])
		}
		assignments = append(assignments, fmt.Sprintf("updated_at = $%d", len(columns)+1))
		params = append(params, appliedAt)
		params = append(params, ids)
		query := fmt.Sprintf(
			`UPDATE providers SET %s WHERE id = ANY($%d) AND deleted_at IS NULL RETURNING id`,
			strings.Join(assignments, ", "), len(params))
		updated, err := tx.Query(ctx, query, params...)
		if err != nil {
			return nil, fmt.Errorf("store: 批量更新供应商失败: %w", err)
		}
		var count int64
		for updated.Next() {
			count++
		}
		updated.Close()
		if err := updated.Err(); err != nil {
			return nil, fmt.Errorf("store: 批量更新供应商失败: %w", err)
		}
		if count != int64(len(ids)) {
			return &ProviderBatchApplyDecision{Status: ProviderBatchApplyStatusStale}, nil
		}
		updatedCount += count
	}

	result := &ProviderBatchApplyResult{
		ApplyResult: ProviderBatchApplyOutcome{
			OperationID:   input.OperationID,
			AppliedAt:     formatStoreISO(appliedAt),
			UpdatedCount:  updatedCount,
			UndoToken:     input.UndoToken,
			UndoExpiresAt: formatStoreISO(appliedAt.Add(time.Duration(input.UndoTTSec) * time.Second)),
		},
		PreviewProviderIDs:   previewIDs,
		EffectiveProviderIDs: effectiveIDs,
		Preimages:            storedPreimages,
		UndoRestorable:       input.UndoRestorable,
		PostCommitEffects:    input.PostCommitEffects,
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("store: 序列化批量补丁结果失败: %w", err)
	}
	completed, err := tx.Exec(ctx, `
		UPDATE provider_batch_apply_operations
		   SET status = 'applied', result = $5, undo_expires_at = $6, updated_at = $7
		 WHERE claim_key = $1 AND preview_token = $2 AND payload_fingerprint = $3 AND status = 'applying'`,
		input.ClaimKey, input.PreviewToken, input.PayloadFingerprint,
		input.OperationID, payload,
		appliedAt.Add(time.Duration(input.UndoTTSec)*time.Second), appliedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("store: 完成批量补丁账本失败: %w", err)
	}
	if completed.RowsAffected() != 1 {
		return nil, fmt.Errorf("store: 批量补丁账本完成约束被破坏")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: 批量补丁事务提交失败: %w", err)
	}
	return &ProviderBatchApplyDecision{
		Status:        ProviderBatchApplyStatusApplied,
		Result:        result,
		UndoAvailable: true,
	}, nil
}

// providerLockedRow 是加锁读回的一行：原始列 + 类型化字段。
type providerLockedRow struct {
	ID           int64
	ProviderType string
	IsEnabled    bool
	Columns      map[string]any
}

func lockProviderRowsForUpdate(
	ctx context.Context,
	tx pgx.Tx,
	ids []int64,
) ([]providerLockedRow, error) {
	rows, err := tx.Query(ctx, `
		SELECT row_to_json(t)::text FROM (
			SELECT * FROM providers
			 WHERE id = ANY($1) AND deleted_at IS NULL
			 ORDER BY id
			 FOR UPDATE
		) t`, ids)
	if err != nil {
		return nil, fmt.Errorf("store: 加锁读取供应商失败: %w", err)
	}
	defer rows.Close()
	locked := make([]providerLockedRow, 0, len(ids))
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("store: 扫描供应商行失败: %w", err)
		}
		var columns map[string]any
		if err := json.Unmarshal([]byte(payload), &columns); err != nil {
			return nil, fmt.Errorf("store: 解析供应商行失败: %w", err)
		}
		row := providerLockedRow{Columns: columns}
		if value, ok := columns["id"].(float64); ok {
			row.ID = int64(value)
		}
		row.ProviderType, _ = columns["provider_type"].(string)
		row.IsEnabled, _ = columns["is_enabled"].(bool)
		locked = append(locked, row)
	}
	return locked, rows.Err()
}

// providerColumnForProviderKey 把 Node 的 camelCase Provider 字段名映射回列名。
//
// 只覆盖**补丁契约可能触及**的字段（契约表的 ProviderKey/Field 两列），未知键返回原串——
// 调用方按「列不存在」处理（前像比对失败 → stale），这正是 Node 侧不匹配时的行为。
func providerColumnForProviderKey(key string) string {
	if column, ok := providerBatchProviderKeyToColumn[key]; ok {
		return column
	}
	return key
}

var providerBatchProviderKeyToColumn = map[string]string{
	"isEnabled":                              "is_enabled",
	"priority":                               "priority",
	"weight":                                 "weight",
	"costMultiplier":                         "cost_multiplier",
	"groupTag":                               "group_tag",
	"modelRedirects":                         "model_redirects",
	"allowedModels":                          "allowed_models",
	"allowedClients":                         "allowed_clients",
	"blockedClients":                         "blocked_clients",
	"anthropicThinkingBudgetPreference":      "anthropic_thinking_budget_preference",
	"anthropicAdaptiveThinking":              "anthropic_adaptive_thinking",
	"activeTimeStart":                        "active_time_start",
	"activeTimeEnd":                          "active_time_end",
	"preserveClientIp":                       "preserve_client_ip",
	"disableSessionReuse":                    "disable_session_reuse",
	"groupPriorities":                        "group_priorities",
	"cacheTtlPreference":                     "cache_ttl_preference",
	"swapCacheTtlBilling":                    "swap_cache_ttl_billing",
	"context1mPreference":                    "context_1m_preference",
	"codexReasoningEffortPreference":         "codex_reasoning_effort_preference",
	"codexReasoningSummaryPreference":        "codex_reasoning_summary_preference",
	"codexTextVerbosityPreference":           "codex_text_verbosity_preference",
	"codexParallelToolCallsPreference":       "codex_parallel_tool_calls_preference",
	"codexImageGenerationPreference":         "codex_image_generation_preference",
	"codexServiceTierPreference":             "codex_service_tier_preference",
	"codexMaxTokensPreference":               "codex_max_tokens_preference",
	"anthropicMaxTokensPreference":           "anthropic_max_tokens_preference",
	"openaiMaxTokensPreference":              "openai_max_tokens_preference",
	"geminiGoogleSearchPreference":           "gemini_google_search_preference",
	"limit5hUsd":                             "limit_5h_usd",
	"limit5hResetMode":                       "limit_5h_reset_mode",
	"limitDailyUsd":                          "limit_daily_usd",
	"dailyResetMode":                         "daily_reset_mode",
	"dailyResetTime":                         "daily_reset_time",
	"limitWeeklyUsd":                         "limit_weekly_usd",
	"limitMonthlyUsd":                        "limit_monthly_usd",
	"limitTotalUsd":                          "limit_total_usd",
	"limitConcurrentSessions":                "limit_concurrent_sessions",
	"circuitBreakerFailureThreshold":         "circuit_breaker_failure_threshold",
	"circuitBreakerOpenDuration":             "circuit_breaker_open_duration",
	"circuitBreakerHalfOpenSuccessThreshold": "circuit_breaker_half_open_success_threshold",
	"circuitBreakerReleaseIncrement":         "circuit_breaker_release_increment",
	"circuitBreakerMaxOpenCount":             "circuit_breaker_max_open_count",
	"maxRetryAttempts":                       "max_retry_attempts",
	"proxyUrl":                               "proxy_url",
	"proxyFallbackToDirect":                  "proxy_fallback_to_direct",
	"firstByteTimeoutStreamingMs":            "first_byte_timeout_streaming_ms",
	"streamingIdleTimeoutMs":                 "streaming_idle_timeout_ms",
	"requestTimeoutNonStreamingMs":           "request_timeout_non_streaming_ms",
	"mcpPassthroughType":                     "mcp_passthrough_type",
	"mcpPassthroughUrl":                      "mcp_passthrough_url",
}

// providerBatchPreimageValueEqual 比较「库里的值」与「期望值」。
//
// 为何不直接用 ==：两侧的表示不同——库里是 row_to_json 出来的 JSON 值（数字一律 float64、
// jsonb 是嵌套结构），而期望值是 Go 类型（int64/*float64/string/json.RawMessage/[]string）。
// 统一做法是把期望值也 JSON 化，再按 JSON 语义深比（含数值容差为 0 的等价判断）。
func providerBatchPreimageValueEqual(current any, expected any) bool {
	normalizedCurrent := normalizePreimageJSON(current)
	normalizedExpected := normalizePreimageJSON(preimageExpectedToJSON(expected))
	return deepJSONEqual(normalizedCurrent, normalizedExpected)
}

// preimageExpectedToJSON 把期望值转成 JSON 值。
func preimageExpectedToJSON(expected any) any {
	switch typed := expected.(type) {
	case json.RawMessage:
		var decoded any
		if err := json.Unmarshal(typed, &decoded); err == nil {
			return decoded
		}
		return string(typed)
	case []string:
		items := make([]any, 0, len(typed))
		for _, item := range typed {
			items = append(items, item)
		}
		return items
	default:
		return expected
	}
}

// normalizePreimageJSON 让数字统一成 float64（int64/int/float32 都归一），其余原样。
func normalizePreimageJSON(value any) any {
	switch typed := value.(type) {
	case float64, float32, int64, int32, int, string, bool, nil:
		if number, ok := toFloat(typed); ok {
			return number
		}
		return typed
	case []any:
		items := make([]any, 0, len(typed))
		for _, item := range typed {
			items = append(items, normalizePreimageJSON(item))
		}
		return items
	case []string:
		items := make([]any, 0, len(typed))
		for _, item := range typed {
			items = append(items, item)
		}
		return items
	case map[string]any:
		normalized := make(map[string]any, len(typed))
		for key, item := range typed {
			normalized[key] = normalizePreimageJSON(item)
		}
		return normalized
	default:
		return typed
	}
}

func toFloat(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int:
		return float64(typed), true
	default:
		return 0, false
	}
}

func deepJSONEqual(left any, right any) bool {
	leftNormalized := normalizePreimageJSON(left)
	rightNormalized := normalizePreimageJSON(right)
	switch leftTyped := leftNormalized.(type) {
	case nil:
		return rightNormalized == nil
	case bool:
		rightTyped, ok := rightNormalized.(bool)
		return ok && leftTyped == rightTyped
	case float64:
		rightTyped, ok := rightNormalized.(float64)
		return ok && leftTyped == rightTyped
	case string:
		rightTyped, ok := rightNormalized.(string)
		return ok && leftTyped == rightTyped
	case []any:
		rightTyped, ok := rightNormalized.([]any)
		if !ok || len(rightTyped) != len(leftTyped) {
			return false
		}
		for index := range leftTyped {
			if !deepJSONEqual(leftTyped[index], rightTyped[index]) {
				return false
			}
		}
		return true
	case map[string]any:
		rightTyped, ok := rightNormalized.(map[string]any)
		if !ok || len(rightTyped) != len(leftTyped) {
			return false
		}
		for key, item := range leftTyped {
			counterpart, exists := rightTyped[key]
			if !exists || !deepJSONEqual(item, counterpart) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func isHexDigest64(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		switch {
		case char >= '0' && char <= '9':
		case char >= 'a' && char <= 'f':
		case char >= 'A' && char <= 'F':
		default:
			return false
		}
	}
	return true
}

func formatStoreISO(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

func mapKeys[V any](source map[int64]V) []int64 {
	keys := make([]int64, 0, len(source))
	for key := range source {
		keys = append(keys, key)
	}
	return keys
}

func sortedUniqueInt64(values []int64) []int64 {
	unique := uniqueInt64(values)
	sort.Slice(unique, func(left, right int) bool { return unique[left] < unique[right] })
	return unique
}

func uniqueInt64(values []int64) []int64 {
	seen := make(map[int64]bool, len(values))
	unique := make([]int64, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		unique = append(unique, value)
	}
	return unique
}

func cloneAnyMap(source map[string]any) map[string]any {
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

// AdminProviderBatchRow 是批量补丁用的一行供应商：类型/启用态 + camelCase 值的完整投影。
//
// Values 用 camelCase（Node 的 Provider 字段空间）：预览行的 before、撤销前像、前像比对都在这套
// 命名下，账本行才能被 Node 读懂。
type AdminProviderBatchRow struct {
	ID           int64
	Name         string
	ProviderType string
	IsEnabled    bool
	Values       map[string]any
}

// AdminFindProvidersForBatchPatch 读回批量补丁所需的供应商行（Node 的 findAllProvidersFresh 子集）。
func (p *Pools) AdminFindProvidersForBatchPatch(
	ctx context.Context,
	ids []int64,
) ([]AdminProviderBatchRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `
		SELECT row_to_json(t)::text FROM (
			SELECT * FROM providers
			 WHERE id = ANY($1) AND deleted_at IS NULL
			 ORDER BY id
		) t`, ids)
	if err != nil {
		return nil, fmt.Errorf("store: 读取批量补丁供应商失败: %w", err)
	}
	defer rows.Close()
	result := make([]AdminProviderBatchRow, 0, len(ids))
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("store: 扫描供应商行失败: %w", err)
		}
		var columns map[string]any
		if err := json.Unmarshal([]byte(payload), &columns); err != nil {
			return nil, fmt.Errorf("store: 解析供应商行失败: %w", err)
		}
		row := AdminProviderBatchRow{Values: make(map[string]any, len(columns))}
		if value, ok := columns["id"].(float64); ok {
			row.ID = int64(value)
		}
		row.Name, _ = columns["name"].(string)
		row.ProviderType, _ = columns["provider_type"].(string)
		row.IsEnabled, _ = columns["is_enabled"].(bool)
		for column, value := range columns {
			key, ok := providerBatchColumnToProviderKey[column]
			if !ok {
				continue
			}
			row.Values[key] = value
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

// providerBatchColumnToProviderKey 是 providerColumnForProviderKey 的反查表（列 → camelCase）。
var providerBatchColumnToProviderKey = func() map[string]string {
	inverted := make(map[string]string, len(providerBatchProviderKeyToColumn))
	for key, column := range providerBatchProviderKeyToColumn {
		inverted[column] = key
	}
	return inverted
}()
