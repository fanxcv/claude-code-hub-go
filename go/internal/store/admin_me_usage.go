package store

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// 本文件是**自服务用量面**（/api/v1/me/usage-logs*）所需的 for-key 查询层，
// 逐条对齐 Node 的 src/repository/usage-logs.ts：
//   - findUsageLogsForKeySlim（:785，偏移分页 + total）
//   - findUsageLogsForKeyBatch（:1199，keyset 游标）
//   - countKeyScopedMessageRows / countKeyScopedLedgerRows（:1171/:1186）
//   - getDistinctModelsForKey / getDistinctEndpointsForKey（:1596/:1631）
//
// 与既有管理面查询（FindUsageLogsBatch / FindUsageLogsBatchLedger）的**结构差别**有两点，
// 都是 Node 侧就有的：
//  1. 主体是 **key 原文**而不是 key_id：Node 的 me 面用 session.key.key 直接比 message_request.key /
//     usage_ledger.key。Go 侧从 AdminKeyRecord.Key 取同一串，故条件逐字一致。
//  2. **两条源要合并**（message_request 与 usage_ledger），且账本行只在对应 message 行**已软删**时
//     出现——否则同一次请求会被记两行。这就是 Node 的 `not exists (… mr_active …)` 去重条件，
//     也是本文件全部复杂度的来源。
//
// 合并方式与 Node 一致：两条源各自按 (created_at DESC, row id DESC) 取 limit+1 行，在**内存里**
// 归并（不是 SQL UNION）：两支的 top-(n+1) 取并后必然包含并集 top-n，因此归并结果与「先并集再排序」
// 等价，而参数编号不必跨支连续。
//
// 白名单差异（已在报告登记）：Node 对 total 与 distinct 结果各有一层 TTL 缓存
// （usageLogSlimTotalCache 10s / distinct*Cache 5min），Go 侧不缓存——缓存只影响上游压力，
// 不影响取值；引入它要带失效语义，不值得在这个面先行。

// MeUsageSlimFilters 复刻 Node 的 UsageLogSlimFilters（keyString 已隐含为「当前调用方的密钥」）。
type MeUsageSlimFilters struct {
	// KeyString 是密钥原文（非空；调用方保证来自认证身份）。
	KeyString                   string
	SessionID                   string
	StartTime                   *int64
	EndTime                     *int64
	StatusCode                  *int
	ExcludeStatusCode200        bool
	Model                       string
	ActualResponseModelMismatch bool
	Endpoint                    string
	MinRetryCount               int
}

// MeUsageSlimRow 复刻 Node 的 UsageLogSlimRow（usage-logs.ts:745）的取值面。
//
// 账本支没有的列（theoretical_cache_tokens / cache_score_eligible /
// cache_score_excluded_reason / special_settings）恒为 nil——Node 在账本支也显式塞 null。
type MeUsageSlimRow struct {
	ID                         int64
	CreatedAt                  *time.Time
	CreatedAtRaw               *string
	SessionIdentityKind        *string
	Model                      *string
	OriginalModel              *string
	ActualResponseModel        *string
	Endpoint                   *string
	StatusCode                 *int
	InputTokens                *int64
	OutputTokens               *int64
	CostUSD                    *string
	DurationMs                 *int64
	CacheCreationInputTokens   *int64
	CacheReadInputTokens       *int64
	CacheCreation5mInputTokens *int64
	CacheCreation1hInputTokens *int64
	CacheTTLApplied            *string
	TheoreticalCacheTokens     *int64
	CacheScoreEligible         *bool
	CacheScoreExcludedReason   *string
	IsReplay                   bool
	ReplaySourceRequestID      *int64
	SpecialSettings            []byte
}

// MeUsageSlimPage 是偏移分页结果（复刻 findUsageLogsForKeySlim 的 {logs, total}）。
type MeUsageSlimPage struct {
	Logs  []MeUsageSlimRow
	Total int64
}

// MeUsageSlimBatch 是游标分页结果（复刻 UsageLogSlimBatchResult）。
type MeUsageSlimBatch struct {
	Logs       []MeUsageSlimRow
	NextCursor *UsageLogCursor
	HasMore    bool
}

// MeUsageSlimMaxLegacyPages 复刻 Node 的 MAX_LEGACY_USAGE_LOG_PAGES：偏移翻页内部靠游标推进，
// 上限 10 页后不再前进（防越界页把整表走一遍）。
const MeUsageSlimMaxLegacyPages = 10

// meSlimMessageSelect 是 message_request 支的投影：列顺序必须与 meUsageSlimScanColumns 一致。
const meSlimMessageSelect = `SELECT
	m.id, m.created_at,
	to_char(m.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
	m.session_identity_kind, m.model, m.original_model, m.actual_response_model,
	m.endpoint, m.status_code, m.input_tokens, m.output_tokens, m.cost_usd::text, m.duration_ms,
	m.cache_creation_input_tokens, m.cache_read_input_tokens,
	m.cache_creation_5m_input_tokens, m.cache_creation_1h_input_tokens, m.cache_ttl_applied,
	m.theoretical_cache_tokens, m.cache_score_eligible, m.cache_score_excluded_reason,
	m.is_replay, m.replay_source_request_id, m.special_settings
	FROM message_request m`

// meSlimLedgerSelect 是 usage_ledger 支的投影。id 取 request_id（Node 的 `id: usageLedger.requestId`），
// 末尾四列在账本里不存在，按 Node 的写法塞 NULL。
const meSlimLedgerSelect = `SELECT
	l.request_id, l.created_at,
	to_char(l.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'),
	l.session_identity_kind, l.model, l.original_model, l.actual_response_model,
	l.endpoint, l.status_code, l.input_tokens, l.output_tokens, l.cost_usd::text, l.duration_ms,
	l.cache_creation_input_tokens, l.cache_read_input_tokens,
	l.cache_creation_5m_input_tokens, l.cache_creation_1h_input_tokens, l.cache_ttl_applied,
	NULL::bigint, NULL::boolean, NULL::text,
	l.is_replay, l.replay_source_request_id, NULL::jsonb
	FROM usage_ledger l`

// meSlimOrder 复刻 Node 的 orderBy(desc(createdAt), desc(id))。
const meSlimOrder = ` ORDER BY created_at DESC, id DESC`

// meWarmupCondition 是 ExcludeWarmupCondition（admin_usage_logs.go）带表别名的形式。
//
// 两处一旦分叉，表现为「同一个 me 页面里，warmup 抢答行时有时无」——列名与字面值必须一致。
func meWarmupCondition(alias string) string {
	return "(" + alias + ".blocked_by IS NULL OR " + alias + ".blocked_by <> 'warmup')"
}

// meSlimLedgerDedup 复刻 Node 的 not exists (select 1 from message_request mr_active …)：
// 账本行只在对应 message 行已软删（或不存在）时才作为独立行出现。
const meSlimLedgerDedup = `NOT EXISTS (
		SELECT 1 FROM message_request mr_active
		WHERE mr_active.id = l.request_id
			AND mr_active.deleted_at IS NULL
			AND mr_active.key = l.key
	)`

// meSlimConditions 构造一条支的 WHERE 与参数。
//
// cursor 非空时追加 keyset 条件。返回的 where 不含 "WHERE" 关键字。
func meSlimConditions(
	keyString string,
	filters MeUsageSlimFilters,
	cursor *UsageLogCursor,
	ledger bool,
) (string, []any, error) {
	b := &usageLogConditionBuilder{}
	if ledger {
		b.conditions = append(b.conditions, "l.blocked_by IS NULL", "l.key = "+placeholder(b, keyString), meSlimLedgerDedup)
		buildLedgerConditions(b, filters.asUsageLogFilters())
	} else {
		b.conditions = append(b.conditions, "m.deleted_at IS NULL", "m.key = "+placeholder(b, keyString),
			meWarmupCondition("m"))
		buildMessageConditions(b, filters.asUsageLogFilters())
	}
	if cursor != nil {
		createdAt, err := time.Parse(time.RFC3339Nano, cursor.CreatedAt)
		if err != nil {
			return "", nil, fmt.Errorf("store: me 用量游标时间非法 %q: %w", cursor.CreatedAt, err)
		}
		idColumn := "m.id"
		createdColumn := "m.created_at"
		if ledger {
			idColumn = "l.request_id"
			createdColumn = "l.created_at"
		}
		b.addCursorCondition(createdColumn, idColumn, createdAt, cursor.ID)
	}
	return b.where(), b.args, nil
}

// placeholder 追加一个参数并返回它的 $n 占位符。
func placeholder(b *usageLogConditionBuilder, value any) string {
	b.args = append(b.args, value)
	return "$" + strconv.Itoa(len(b.args))
}

// asUsageLogFilters 把自服务筛选条件映射为通用筛选条件。
//
// 只填两支共用的字段：主体（KeyID/UserID）与 ReplayFilter 在自服务面不适用
// （前者已由 key 原文代替，后者 Node 的 me 面不传，等价于 "all"）。
func (f MeUsageSlimFilters) asUsageLogFilters() UsageLogFilters {
	return UsageLogFilters{
		SessionID:                   f.SessionID,
		StartTime:                   f.StartTime,
		EndTime:                     f.EndTime,
		StatusCode:                  f.StatusCode,
		ExcludeStatusCode200:        f.ExcludeStatusCode200,
		Model:                       f.Model,
		ActualResponseModelMismatch: f.ActualResponseModelMismatch,
		Endpoint:                    f.Endpoint,
		MinRetryCount:               f.MinRetryCount,
		ReplayFilter:                ReplayFilterAll,
	}
}

// selectMeSlimRows 取一条支的前 limit 行（limit 已由调用方 +1）。
func (p *Pools) selectMeSlimRows(
	ctx context.Context,
	selectClause string,
	where string,
	args []any,
	limit int,
	offset int,
) ([]MeUsageSlimRow, error) {
	query := selectClause + ` WHERE ` + where + meSlimOrder + ` LIMIT ` + strconv.Itoa(limit)
	if offset > 0 {
		// ponytail: 偏移分支照 Node 的 .offset() 原样翻页；只有 page>1 的偏移分支会带上它，
		// 而那条分支本就靠游标翻页（见 FindMeUsageLogsForKeySlim），这里的 OFFSET 恒为 0。
		query += ` OFFSET ` + strconv.Itoa(offset)
	}
	rows, err := p.usageLogQuery(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	results := make([]MeUsageSlimRow, 0, limit)
	for rows.Next() {
		row, scanErr := scanMeUsageSlimRow(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: me 用量行迭代失败: %w", err)
	}
	return results, nil
}

// scanMeUsageSlimRow 按 meSlimMessageSelect/meSlimLedgerSelect 的列序扫描。
func scanMeUsageSlimRow(rows pgx.Rows) (MeUsageSlimRow, error) {
	var row MeUsageSlimRow
	if err := rows.Scan(
		&row.ID, &row.CreatedAt, &row.CreatedAtRaw,
		&row.SessionIdentityKind, &row.Model, &row.OriginalModel, &row.ActualResponseModel,
		&row.Endpoint, &row.StatusCode, &row.InputTokens, &row.OutputTokens, &row.CostUSD,
		&row.DurationMs, &row.CacheCreationInputTokens, &row.CacheReadInputTokens,
		&row.CacheCreation5mInputTokens, &row.CacheCreation1hInputTokens, &row.CacheTTLApplied,
		&row.TheoreticalCacheTokens, &row.CacheScoreEligible, &row.CacheScoreExcludedReason,
		&row.IsReplay, &row.ReplaySourceRequestID, &row.SpecialSettings,
	); err != nil {
		return MeUsageSlimRow{}, fmt.Errorf("store: me 用量行扫描失败: %w", err)
	}
	return row, nil
}

// FindMeUsageLogsForKeyBatch 复刻 findUsageLogsForKeyBatch（usage-logs.ts:1199）：
// 两支各取 limit+1 行，归并后取前 limit 行，多出来的那一行只用来判 hasMore。
func (p *Pools) FindMeUsageLogsForKeyBatch(
	ctx context.Context,
	filters MeUsageSlimFilters,
	cursor *UsageLogCursor,
	limit int,
) (MeUsageSlimBatch, error) {
	safeLimit := clampUsageLogLimit(limit, 1, 100)
	fetchLimit := safeLimit + 1

	messageWhere, messageArgs, err := meSlimConditions(filters.KeyString, filters, cursor, false)
	if err != nil {
		return MeUsageSlimBatch{}, err
	}
	messageRows, err := p.selectMeSlimRows(ctx, meSlimMessageSelect, messageWhere, messageArgs, fetchLimit, 0)
	if err != nil {
		return MeUsageSlimBatch{}, err
	}

	// 有 minRetryCount 时账本支整体缺席（Node 的 buildKeyLedgerConditions 返回 null）：
	// 重试次数只在 message_request.provider_chain 里有，账本行答不了这个问题。
	ledgerRows := []MeUsageSlimRow{}
	if filters.MinRetryCount <= 0 {
		ledgerWhere, ledgerArgs, ledgerErr := meSlimConditions(filters.KeyString, filters, cursor, true)
		if ledgerErr != nil {
			return MeUsageSlimBatch{}, ledgerErr
		}
		ledgerRows, err = p.selectMeSlimRows(ctx, meSlimLedgerSelect, ledgerWhere, ledgerArgs, fetchLimit, 0)
		if err != nil {
			return MeUsageSlimBatch{}, err
		}
	}

	merged := mergeMeUsageSlimRows(messageRows, ledgerRows)
	hasMore := len(merged) > safeLimit
	logs := merged
	if hasMore {
		logs = merged[:safeLimit]
	}

	result := MeUsageSlimBatch{Logs: logs, HasMore: hasMore}
	if hasMore && len(logs) > 0 {
		last := logs[len(logs)-1]
		if last.CreatedAtRaw == nil {
			// Node 在这里抛错（buildNextCursorOrThrow）：有 hasMore 却没有游标时间，
			// 说明 createdAt 为 NULL 的行参与了排序，继续翻页会死循环。
			return MeUsageSlimBatch{}, fmt.Errorf("store: me 用量翻页游标缺失（行 %d 无 createdAt）", last.ID)
		}
		result.NextCursor = &UsageLogCursor{CreatedAt: *last.CreatedAtRaw, ID: last.ID}
	}
	return result, nil
}

// FindMeUsageLogsForKeySlim 复刻 findUsageLogsForKeySlim（usage-logs.ts:785）。
//
// page == 1 走一次批量取（Node 同）；page > 1 用游标逐页推进，最多 MeUsageSlimMaxLegacyPages 页
// （Node 的 legacy 翻页上限，超出后返回空 logs 但 total 照答）。
func (p *Pools) FindMeUsageLogsForKeySlim(
	ctx context.Context,
	filters MeUsageSlimFilters,
	page int,
	pageSize int,
) (MeUsageSlimPage, error) {
	safePage := page
	if safePage < 1 {
		safePage = 1
	}
	safePageSize := clampUsageLogLimit(pageSize, 1, 100)

	total, err := p.CountMeUsageLogsForKey(ctx, filters)
	if err != nil {
		return MeUsageSlimPage{}, err
	}
	if safePage == 1 {
		batch, batchErr := p.FindMeUsageLogsForKeyBatch(ctx, filters, nil, safePageSize)
		if batchErr != nil {
			return MeUsageSlimPage{}, batchErr
		}
		return MeUsageSlimPage{Logs: batch.Logs, Total: total}, nil
	}

	var cursor *UsageLogCursor
	currentPage := 1
	logs := []MeUsageSlimRow{}
	for {
		batch, batchErr := p.FindMeUsageLogsForKeyBatch(ctx, filters, cursor, safePageSize)
		if batchErr != nil {
			return MeUsageSlimPage{}, batchErr
		}
		if currentPage == safePage {
			logs = batch.Logs
			break
		}
		if !batch.HasMore || batch.NextCursor == nil {
			break
		}
		cursor = batch.NextCursor
		currentPage++
		if currentPage > MeUsageSlimMaxLegacyPages {
			break
		}
	}
	return MeUsageSlimPage{Logs: logs, Total: total}, nil
}

// CountMeUsageLogsForKey 复刻 countKeyScopedMessageRows + countKeyScopedLedgerRows 的和。
func (p *Pools) CountMeUsageLogsForKey(ctx context.Context, filters MeUsageSlimFilters) (int64, error) {
	messageWhere, messageArgs, err := meSlimConditions(filters.KeyString, filters, nil, false)
	if err != nil {
		return 0, err
	}
	var messageTotal int64
	if err := p.scanMeUsageCount(ctx,
		`SELECT count(*)::int8 FROM message_request m WHERE `+messageWhere, messageArgs, &messageTotal); err != nil {
		return 0, err
	}

	ledgerTotal := int64(0)
	if filters.MinRetryCount <= 0 {
		ledgerWhere, ledgerArgs, ledgerErr := meSlimConditions(filters.KeyString, filters, nil, true)
		if ledgerErr != nil {
			return 0, ledgerErr
		}
		if err := p.scanMeUsageCount(ctx,
			`SELECT count(*)::int8 FROM usage_ledger l WHERE `+ledgerWhere, ledgerArgs, &ledgerTotal); err != nil {
			return 0, err
		}
	}
	return messageTotal + ledgerTotal, nil
}

// scanMeUsageCount 执行一条单值计数查询。
func (p *Pools) scanMeUsageCount(ctx context.Context, query string, args []any, target *int64) error {
	rows, err := p.usageLogQuery(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return err
		}
		return nil
	}
	if err := rows.Scan(target); err != nil {
		return fmt.Errorf("store: me 用量计数扫描失败: %w", err)
	}
	return rows.Err()
}

// mergeMeUsageSlimRows 复刻 mergeOrderedUsageLogs + compareUsageLogOrder：
// createdAtRaw 文本降序（NULL 排最后），同刻按 id 降序。
//
// 用 createdAtRaw 而不是 time.Time 比较，是因为 Node 就是拿 to_char 出来的文本比
// （微秒精度、UTC 定长），两者在微秒级并列时可能给出不同顺序；文本比较与 Node 一致。
func mergeMeUsageSlimRows(left, right []MeUsageSlimRow) []MeUsageSlimRow {
	merged := make([]MeUsageSlimRow, 0, len(left)+len(right))
	merged = append(merged, left...)
	merged = append(merged, right...)
	sort.SliceStable(merged, func(i, j int) bool {
		return meSlimRowLess(merged[i], merged[j])
	})
	return merged
}

// meSlimRowLess 是排序判据本身（compareUsageLogOrder 的 Go 版）。
func meSlimRowLess(a, b MeUsageSlimRow) bool {
	aRaw, aHas := meSlimCreatedAtRaw(a)
	bRaw, bHas := meSlimCreatedAtRaw(b)
	if aRaw != bRaw {
		// 无时间的行排最后（Node: aRaw === null → return 1）。
		if !aHas {
			return false
		}
		if !bHas {
			return true
		}
		return aRaw > bRaw
	}
	return a.ID > b.ID
}

// meSlimCreatedAtRaw 取排序用的时间文本；缺 to_char 结果时退化为 createdAt 的 ISO 文本。
func meSlimCreatedAtRaw(row MeUsageSlimRow) (string, bool) {
	if row.CreatedAtRaw != nil {
		return *row.CreatedAtRaw, true
	}
	if row.CreatedAt != nil {
		return row.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000Z"), true
	}
	return "", false
}

// DistinctMeUsageModelsForKey 复刻 getDistinctModelsForKey（usage-logs.ts:1596）：
// 两支各自 distinct，去空去重后按字典序升序。
func (p *Pools) DistinctMeUsageModelsForKey(ctx context.Context, keyString string) ([]string, error) {
	filters := MeUsageSlimFilters{KeyString: keyString}
	return p.distinctMeUsageValuesForKey(ctx, keyString, filters, "model")
}

// DistinctMeUsageEndpointsForKey 复刻 getDistinctEndpointsForKey（usage-logs.ts:1631）。
func (p *Pools) DistinctMeUsageEndpointsForKey(ctx context.Context, keyString string) ([]string, error) {
	filters := MeUsageSlimFilters{KeyString: keyString}
	return p.distinctMeUsageValuesForKey(ctx, keyString, filters, "endpoint")
}

// distinctMeUsageValuesForKey 是 models/endpoints 两条 distinct 查询的共同实现。
//
// distinct 面不带任何筛选（Node 也只传 keyString），因此这里只拼 key + 软删 + warmup 三条件，
// 不复用 meSlimConditions（那里的筛选项恒为空，拼出来是一样的，但读起来会误导）。
func (p *Pools) distinctMeUsageValuesForKey(
	ctx context.Context,
	keyString string,
	_ MeUsageSlimFilters,
	column string,
) ([]string, error) {
	if column != "model" && column != "endpoint" {
		return nil, fmt.Errorf("store: 不支持的 distinct 列 %q", column)
	}
	values := make([]string, 0)
	for _, source := range []struct {
		query string
		args  []any
	}{
		{
			query: `SELECT DISTINCT m.` + column + ` FROM message_request m
				WHERE m.key = $1 AND m.deleted_at IS NULL AND ` + meWarmupCondition("m") +
				` AND m.` + column + ` IS NOT NULL`,
			args: []any{keyString},
		},
		{
			query: `SELECT DISTINCT l.` + column + ` FROM usage_ledger l
				WHERE l.key = $1 AND l.blocked_by IS NULL AND ` + meSlimLedgerDedup +
				` AND l.` + column + ` IS NOT NULL`,
			args: []any{keyString},
		},
	} {
		rows, err := p.usageLogQuery(ctx, source.query, source.args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var value *string
			if scanErr := rows.Scan(&value); scanErr != nil {
				rows.Close()
				return nil, fmt.Errorf("store: me distinct 扫描失败: %w", scanErr)
			}
			if value != nil && strings.TrimSpace(*value) != "" {
				values = append(values, *value)
			}
		}
		iterationErr := rows.Err()
		rows.Close()
		if iterationErr != nil {
			return nil, fmt.Errorf("store: me distinct 迭代失败: %w", iterationErr)
		}
	}

	unique := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, seen := unique[value]; seen {
			continue
		}
		unique[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

// MeUsageModelBreakdown 是统计摘要里的单模型聚合（复刻 ModelBreakdownItem）。
//
// Cost 用 numeric 文本承载，避免浮点往返；上层按需转展示值。
type MeUsageModelBreakdown struct {
	Model                 *string
	Requests              int64
	Cost                  string
	InputTokens           float64
	OutputTokens          float64
	CacheCreationTokens   float64
	CacheReadTokens       float64
	CacheCreation5mTokens float64
	CacheCreation1hTokens float64
}

// MeUsageStatsSummary 复刻 getMyStatsSummary 的聚合结果（两套 breakdown 加键维度合计）。
//
// KeyBreakdown 只含键维度请求数 > 0 的行（Node 的 keyOnlyBreakdown），UserBreakdown 含全部行；
// 合计与 totalTokens 由**键维度**行汇总（Node 的 summaryAcc 只遍历 keyOnlyBreakdown）。
type MeUsageStatsSummary struct {
	KeyBreakdown               []MeUsageModelBreakdown
	UserBreakdown              []MeUsageModelBreakdown
	TotalRequests              int64
	TotalCost                  float64
	TotalInputTokens           float64
	TotalOutputTokens          float64
	TotalCacheCreationTokens   float64
	TotalCacheReadTokens       float64
	TotalCacheCreation5mTokens float64
	TotalCacheCreation1hTokens float64
}

// SummarizeMeUsageForKeyAndUser 复刻 getMyStatsSummary 的那条聚合 SQL（my-usage.ts:1027-1060）：
// 以 user_id 为范围扫一遍，用 FILTER 同时算出「该用户全部密钥」与「当前密钥」两套模型分解。
//
// 计费口径与 Node 一致：LEDGER_BILLING_CONDITION（排拦截行、replay 行与不计费端点）。
func (p *Pools) SummarizeMeUsageForKeyAndUser(
	ctx context.Context,
	userID int64,
	keyString string,
	startTime *int64,
	endTime *int64,
) (MeUsageStatsSummary, error) {
	b := &usageLogConditionBuilder{}
	b.conditions = append(b.conditions, "l.user_id = "+placeholder(b, userID), BillingCondition)
	keyPlaceholder := placeholder(b, keyString)
	if startTime != nil {
		b.addValue(`l.created_at >= %s`, time.UnixMilli(*startTime).UTC())
	}
	if endTime != nil {
		b.addValue(`l.created_at < %s`, time.UnixMilli(*endTime).UTC())
	}

	query := `SELECT l.model,
			count(*)::int8,
			COALESCE(sum(l.cost_usd), 0)::text,
			COALESCE(sum(l.input_tokens), 0)::double precision,
			COALESCE(sum(l.output_tokens), 0)::double precision,
			COALESCE(sum(l.cache_creation_input_tokens), 0)::double precision,
			COALESCE(sum(l.cache_read_input_tokens), 0)::double precision,
			COALESCE(sum(l.cache_creation_5m_input_tokens), 0)::double precision,
			COALESCE(sum(l.cache_creation_1h_input_tokens), 0)::double precision,
			count(*) FILTER (WHERE l.key = ` + keyPlaceholder + `)::int8,
			COALESCE(sum(l.cost_usd) FILTER (WHERE l.key = ` + keyPlaceholder + `), 0)::text,
			COALESCE(sum(l.input_tokens) FILTER (WHERE l.key = ` + keyPlaceholder + `), 0)::double precision,
			COALESCE(sum(l.output_tokens) FILTER (WHERE l.key = ` + keyPlaceholder + `), 0)::double precision,
			COALESCE(sum(l.cache_creation_input_tokens) FILTER (WHERE l.key = ` + keyPlaceholder + `), 0)::double precision,
			COALESCE(sum(l.cache_read_input_tokens) FILTER (WHERE l.key = ` + keyPlaceholder + `), 0)::double precision,
			COALESCE(sum(l.cache_creation_5m_input_tokens) FILTER (WHERE l.key = ` + keyPlaceholder + `), 0)::double precision,
			COALESCE(sum(l.cache_creation_1h_input_tokens) FILTER (WHERE l.key = ` + keyPlaceholder + `), 0)::double precision
		FROM usage_ledger l
		WHERE ` + b.where() + `
		GROUP BY l.model
		ORDER BY sum(l.cost_usd) DESC`

	rows, err := p.usageLogQuery(ctx, query, b.args...)
	if err != nil {
		return MeUsageStatsSummary{}, err
	}
	defer rows.Close()

	summary := MeUsageStatsSummary{}
	for rows.Next() {
		var (
			model                 *string
			userRequests          int64
			userCost              string
			userInput, userOutput float64
			userCacheCreate       float64
			userCacheRead         float64
			userCacheCreate5m     float64
			userCacheCreate1h     float64
			keyRequests           int64
			keyCost               string
			keyInput, keyOutput   float64
			keyCacheCreate        float64
			keyCacheRead          float64
			keyCacheCreate5m      float64
			keyCacheCreate1h      float64
		)
		if scanErr := rows.Scan(
			&model,
			&userRequests, &userCost, &userInput, &userOutput, &userCacheCreate, &userCacheRead,
			&userCacheCreate5m, &userCacheCreate1h,
			&keyRequests, &keyCost, &keyInput, &keyOutput, &keyCacheCreate, &keyCacheRead,
			&keyCacheCreate5m, &keyCacheCreate1h,
		); scanErr != nil {
			return MeUsageStatsSummary{}, fmt.Errorf("store: me 统计摘要扫描失败: %w", scanErr)
		}

		summary.UserBreakdown = append(summary.UserBreakdown, MeUsageModelBreakdown{
			Model:                 model,
			Requests:              userRequests,
			Cost:                  userCost,
			InputTokens:           userInput,
			OutputTokens:          userOutput,
			CacheCreationTokens:   userCacheCreate,
			CacheReadTokens:       userCacheRead,
			CacheCreation5mTokens: userCacheCreate5m,
			CacheCreation1hTokens: userCacheCreate1h,
		})

		if keyRequests <= 0 {
			continue
		}
		summary.KeyBreakdown = append(summary.KeyBreakdown, MeUsageModelBreakdown{
			Model:                 model,
			Requests:              keyRequests,
			Cost:                  keyCost,
			InputTokens:           keyInput,
			OutputTokens:          keyOutput,
			CacheCreationTokens:   keyCacheCreate,
			CacheReadTokens:       keyCacheRead,
			CacheCreation5mTokens: keyCacheCreate5m,
			CacheCreation1hTokens: keyCacheCreate1h,
		})
		summary.TotalRequests += keyRequests
		summary.TotalCost += MeUsageCostFloat(keyCost)
		summary.TotalInputTokens += keyInput
		summary.TotalOutputTokens += keyOutput
		summary.TotalCacheCreationTokens += keyCacheCreate
		summary.TotalCacheReadTokens += keyCacheRead
		summary.TotalCacheCreation5mTokens += keyCacheCreate5m
		summary.TotalCacheCreation1hTokens += keyCacheCreate1h
	}
	if err := rows.Err(); err != nil {
		return MeUsageStatsSummary{}, fmt.Errorf("store: me 统计摘要迭代失败: %w", err)
	}

	// Node 对 keyOnlyBreakdown 再按 cost 降序排一次（SQL 已按 user 维度 cost 排序，键维度可能不同序）。
	sort.SliceStable(summary.KeyBreakdown, func(i, j int) bool {
		return MeUsageCostFloat(summary.KeyBreakdown[i].Cost) > MeUsageCostFloat(summary.KeyBreakdown[j].Cost)
	})
	return summary, nil
}

// MeUsageCostFloat 把 numeric 文本转成浮点；空或非有限值归 0（Node 的 `Number(x ?? 0)` + isFinite 兜底）。
func MeUsageCostFloat(value string) float64 {
	parsed := parseUsageLogCost(value)
	if parsed != 0 {
		return parsed
	}
	return 0
}

// MeUsageCostText 把可空 numeric 文本归一为字符串（nil → 空串）。
func MeUsageCostText(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// MeUsageTotalTokens 复刻 totalTokens：四项 token 求和（Key 维度）。
func MeUsageTotalTokens(summary MeUsageStatsSummary) float64 {
	return summary.TotalInputTokens + summary.TotalOutputTokens +
		summary.TotalCacheCreationTokens + summary.TotalCacheReadTokens
}
