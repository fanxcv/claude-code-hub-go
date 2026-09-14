package store

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"
)

// 本文件复刻 Node 的 findReadonlyUsageLogsBatchForKey（src/repository/usage-logs.ts:1447）。
//
// 与 me 的其它用量端点（FindMeUsageLogsForKeyBatch / FindMeUsageLogsForKeySlim）的差别只有取列：
// 那边是 slim 行（响应只要个数与几个标量），这边是**完整行**——即管理面 /usage-logs 的批次形状。
// 归并语义也因此与 slim 路径不同：
//
//   - message_request 支与 usage_ledger 支**各自**取 fetchLimit = limit + 1 行（Node 的 Promise.all）；
//   - 两支合并后按 Node 的 compareUsageLogOrder 排序（**只排序、不去重**，见 mergeOrderedUsageLogs）；
//   - 排序键的优先级：createdAtRaw（缺省时退回 createdAt 的 ISO 串）降序，同键时 id 降序；
//     createdAtRaw 为 null 的行排最后（Node 的 `if (aRaw === null) return 1;`）。
//
// 账本支在 MinRetryCount > 0 时整体缺席：重试次数只存在于 message_request.provider_chain，
// 账本行答不了这个问题（Node 的 buildKeyLedgerConditions 此时返回 null）——与 slim 路径同判。

// MeUsageFullLedgerRow 是账本支的一行：账本字段面（LedgerUsageLogRow）之上多一列 createdAtRaw。
//
// 为何单独定义而不给 LedgerUsageLogRow 加字段：管理面 /usage-logs 的账本回退路径不取 to_char
// 列（Node 那边也不取），给它加字段会让两条路径的取列清单纠缠在一起。
type MeUsageFullLedgerRow struct {
	LedgerUsageLogRow
	// CreatedAtRaw 是 Node 在 for-key 路径多取的 to_char 列，仅用于排序，不进响应体。
	CreatedAtRaw *string
}

// MeUsageFullRow 是归并后的一行：两支撑其一出。
type MeUsageFullRow struct {
	// Message 非 nil 表示来自 message_request（完整字段面）。
	Message *UsageLogRow
	// Ledger 非 nil 表示来自 usage_ledger（字段面较窄，渲染走 ledgerFallbackRowFields）。
	Ledger *MeUsageFullLedgerRow
}

// MeUsageFullBatch 是 for-key 完整行的批量结果（Node 的 UsageLogsBatchResult 在 Go 侧的载体）。
type MeUsageFullBatch struct {
	Rows       []MeUsageFullRow
	HasMore    bool
	NextCursor *UsageLogCursor
}

// FindReadonlyUsageLogsBatchForKey 复刻 Node 的 findReadonlyUsageLogsBatchForKey。
func (p *Pools) FindReadonlyUsageLogsBatchForKey(
	ctx context.Context,
	filters MeUsageSlimFilters,
	cursor *UsageLogCursor,
	limit int,
) (MeUsageFullBatch, error) {
	safeLimit := clampUsageLogLimit(limit, 1, 100)
	fetchLimit := safeLimit + 1

	messageRows, err := p.selectMeUsageFullMessageRows(ctx, filters, cursor, fetchLimit)
	if err != nil {
		return MeUsageFullBatch{}, err
	}

	// 账本支：有 minRetryCount 时整体缺席（与 slim 路径同判，见文件头）。
	ledgerRows := []MeUsageFullLedgerRow{}
	if filters.MinRetryCount <= 0 {
		ledgerRows, err = p.selectMeUsageFullLedgerRows(ctx, filters, cursor, fetchLimit)
		if err != nil {
			return MeUsageFullBatch{}, err
		}
	}

	merged := mergeMeUsageFullRows(messageRows, ledgerRows)
	hasMore := len(merged) > safeLimit
	rows := merged
	if hasMore {
		rows = merged[:safeLimit]
	}

	result := MeUsageFullBatch{Rows: rows, HasMore: hasMore}
	if hasMore && len(rows) > 0 {
		last := rows[len(rows)-1]
		if last.createdAtRaw() == nil {
			// Node 的 buildNextCursorOrThrow 在这里抛错：有 hasMore 却没有游标时间，
			// 说明 createdAt 为 NULL 的行参与了排序，继续翻页会死循环。
			return MeUsageFullBatch{}, fmt.Errorf("store: me 完整用量翻页游标缺失（行 %d 无 createdAt）", last.id())
		}
		result.NextCursor = &UsageLogCursor{CreatedAt: *last.createdAtRaw(), ID: last.id()}
	}
	return result, nil
}

// selectMeUsageFullMessageRows 取 message_request 支的完整行。
func (p *Pools) selectMeUsageFullMessageRows(
	ctx context.Context,
	filters MeUsageSlimFilters,
	cursor *UsageLogCursor,
	fetchLimit int,
) ([]UsageLogRow, error) {
	where, args, err := meSlimConditions(filters.KeyString, filters, cursor, false)
	if err != nil {
		return nil, err
	}
	query := `SELECT ` + messageLogColumns + `,
		to_char(m.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')` +
		messageLogJoins + ` WHERE ` + where + messageLogOrder +
		` LIMIT ` + strconv.Itoa(fetchLimit)

	rows, err := p.usageLogQuery(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectUsageLogRows(rows, fetchLimit)
}

// selectMeUsageFullLedgerRows 取账本支的完整行（含排序用的 createdAtRaw 列）。
func (p *Pools) selectMeUsageFullLedgerRows(
	ctx context.Context,
	filters MeUsageSlimFilters,
	cursor *UsageLogCursor,
	fetchLimit int,
) ([]MeUsageFullLedgerRow, error) {
	where, args, err := meSlimConditions(filters.KeyString, filters, cursor, true)
	if err != nil {
		return nil, err
	}
	query := `SELECT
		l.request_id, l.created_at, COALESCE(l.session_identity, l.session_id), l.session_id,
		l.session_identity_kind,
		l.user_id, u.name, l.key, k.name, p.name,
		l.model, l.original_model, l.actual_response_model, l.endpoint, l.status_code,
		l.input_tokens, l.output_tokens, l.cache_creation_input_tokens, l.cache_read_input_tokens,
		l.cache_creation_5m_input_tokens, l.cache_creation_1h_input_tokens, l.cache_ttl_applied,
		l.cost_usd::text, l.cost_multiplier::text, l.group_cost_multiplier::text,
		l.duration_ms, l.ttfb_ms, l.first_byte_ms, l.client_ip, l.context_1m_applied,
		l.swap_cache_ttl_applied, l.is_replay, l.replay_source_request_id,
		to_char(l.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')
		FROM usage_ledger l
		LEFT JOIN users u ON u.id = l.user_id
		LEFT JOIN keys k ON k.key = l.key
		LEFT JOIN providers p ON p.id = l.final_provider_id
		WHERE ` + where + ` ORDER BY l.created_at DESC, l.request_id DESC
		LIMIT ` + strconv.Itoa(fetchLimit)

	pool, err := p.usageLogControl()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: me 完整用量账本支查询失败: %w", err)
	}
	defer rows.Close()

	results := make([]MeUsageFullLedgerRow, 0, fetchLimit)
	for rows.Next() {
		var row MeUsageFullLedgerRow
		if err := rows.Scan(
			&row.ID, &row.CreatedAt, &row.SessionID, &row.SourceSessionID,
			&row.SessionIdentityKind, &row.UserID, &row.UserName, &row.Key, &row.KeyName,
			&row.ProviderName, &row.Model, &row.OriginalModel, &row.ActualResponseModel,
			&row.Endpoint, &row.StatusCode, &row.InputTokens, &row.OutputTokens,
			&row.CacheCreationInputTokens, &row.CacheReadInputTokens,
			&row.CacheCreation5mInputTokens, &row.CacheCreation1hInputTokens, &row.CacheTTLApplied,
			&row.CostUSD, &row.CostMultiplier, &row.GroupCostMultiplier, &row.DurationMs,
			&row.TTFBMs, &row.FirstByteMs, &row.ClientIP, &row.Context1mApplied,
			&row.SwapCacheTTLApplied, &row.IsReplay, &row.ReplaySourceRequestID,
			&row.CreatedAtRaw,
		); err != nil {
			return nil, fmt.Errorf("store: 读取 me 完整用量账本行失败: %w", err)
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历 me 完整用量账本行失败: %w", err)
	}
	return results, nil
}

// mergeMeUsageFullRows 复刻 Node 的 mergeOrderedUsageLogs：只排序，不去重。
func mergeMeUsageFullRows(messageRows []UsageLogRow, ledgerRows []MeUsageFullLedgerRow) []MeUsageFullRow {
	merged := make([]MeUsageFullRow, 0, len(messageRows)+len(ledgerRows))
	for index := range messageRows {
		row := messageRows[index]
		merged = append(merged, MeUsageFullRow{Message: &row})
	}
	for index := range ledgerRows {
		row := ledgerRows[index]
		merged = append(merged, MeUsageFullRow{Ledger: &row})
	}
	sort.SliceStable(merged, func(i, j int) bool {
		return compareMeUsageFullRows(merged[i], merged[j]) < 0
	})
	return merged
}

// compareMeUsageFullRows 复刻 Node 的 compareUsageLogOrder（usage-logs.ts:949）。
func compareMeUsageFullRows(a, b MeUsageFullRow) int {
	aRaw := meUsageFullRawKey(a)
	bRaw := meUsageFullRawKey(b)

	if aRaw != bRaw {
		if aRaw == nil {
			return 1
		}
		if bRaw == nil {
			return -1
		}
		if *aRaw == *bRaw {
			// 不可能到这里（上面已判不等），保留以免将来改动漏掉分支。
			return compareInt64Desc(a.id(), b.id())
		}
		// Node 用 bRaw.localeCompare(aRaw)：时间戳为定长 ASCII，字典序即时间序，降序排列。
		if *bRaw < *aRaw {
			return -1
		}
		return 1
	}
	return compareInt64Desc(a.id(), b.id())
}

// meUsageFullRawKey 取排序键：createdAtRaw 优先，缺省时退回 createdAt 的 ISO 串。
func meUsageFullRawKey(row MeUsageFullRow) *string {
	if raw := row.createdAtRaw(); raw != nil && *raw != "" {
		return raw
	}
	createdAt := row.createdAt()
	if createdAt == nil {
		return nil
	}
	value := formatUsageLogCursorTime(createdAt)
	if value == "" {
		return nil
	}
	return &value
}

func compareInt64Desc(a, b int64) int {
	switch {
	case a > b:
		return -1
	case a < b:
		return 1
	default:
		return 0
	}
}

func (r MeUsageFullRow) id() int64 {
	if r.Message != nil {
		return r.Message.ID
	}
	if r.Ledger != nil {
		return r.Ledger.ID
	}
	return 0
}

func (r MeUsageFullRow) createdAtRaw() *string {
	if r.Message != nil {
		return r.Message.CreatedAtRaw
	}
	if r.Ledger != nil {
		return r.Ledger.CreatedAtRaw
	}
	return nil
}

func (r MeUsageFullRow) createdAt() *time.Time {
	if r.Message != nil {
		return r.Message.CreatedAt
	}
	if r.Ledger != nil {
		return r.Ledger.CreatedAt
	}
	return nil
}
