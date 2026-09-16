package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// 本文件是管理面 usage-logs 资源（A1-1）的读面：筛选条件、游标/偏移两套分页、聚合统计与
// 筛选器选项。逐条对齐 Node 的 src/repository/usage-logs.ts 与
// src/repository/_shared/usage-log-filters.ts，包括：
//
//   - message_request 为主表（INNER JOIN users/keys、LEFT JOIN providers）；列表为空且处于
//     ledger-only 时整体切到 usage_ledger（src/lib/ledger-fallback.ts）。
//   - 软删过滤（deleted_at IS NULL）与 warmup 排除（ExcludeWarmupCondition）的适用范围**不同**：
//     软删是所有查询的前提，warmup 只在统计/联想里排除，列表照常返回 warmup 行。
//   - 统计口径取自 usage_ledger（Node 的 findUsageLogsStats 就是这样），唯一例外是不计费端点。
//   - 读走 control 分道：Node 侧 /api/v1 的 getDb() 在没有 data scope 时取 control
//     （src/drizzle/db.ts:123），与数据面的 data 分道分开。
//
// 数值列的取法与 Node 一致：cost 类 numeric 在 Node 里由 pg 驱动以小数字符串返回，
// 再由 `.toString()` 原样进 JSON；这里用 `::text` 取同一形式，避免浮点往返。

// UsageLogReplayFilter 是 replay 筛选档位（usage-log-filters.ts:6）。
type UsageLogReplayFilter string

const (
	// ReplayFilterAll 表示不按 replay 过滤。
	ReplayFilterAll UsageLogReplayFilter = "all"
	// ReplayFilterReplay 只取 replay 行。
	ReplayFilterReplay UsageLogReplayFilter = "replay"
	// ReplayFilterNonReplay 只取非 replay 行。
	ReplayFilterNonReplay UsageLogReplayFilter = "non-replay"
)

// ExcludeWarmupCondition 复刻 src/repository/_shared/message-request-conditions.ts:10 的
// EXCLUDE_WARMUP_CONDITION：warmup 抢答请求日志可见，但不计入任何聚合统计或限额。
const ExcludeWarmupCondition = `(blocked_by IS NULL OR blocked_by <> 'warmup')`

// RetryCountExpr 复刻 usage-log-filters.ts:115 的 RETRY_COUNT_EXPR：只统计「实际请求」的
// 次数再减一；链里出现 hedge 标记时按 0 处理（并发不算顺序重试）。列名用 providerChainColumn
// 替换后即可用于不同表（Node 侧由 drizzle 注入列引用）。
//
// 同一语义有**三处镜像**，三个集合必须逐项一致，否则「列表按最小重试数过滤」与「界面/导出的
// 重试次数」会互相矛盾（同一条链在列表里查得到、在弹窗里却显示 0 次重试）：
//   - 本表达式（minRetryCount 过滤）；
//   - `adminapi/usage_logs_export_render.go` 的 exportIsActualRequest / exportIsHedgeRace（CSV 导出）；
//   - 前端 `src/lib/utils/provider-chain-formatter.ts` 的 isActualRequest / isHedgeRace。
//
// 两处容易漏的等价点：`unsupported`（上游声明不支持输入形态，仍是一次真实尝试）必须计入；
// 成功条目的判据是**真值**（statusCode 为 0 不算，镜像侧用 `!= nil && != 0`）。
const RetryCountExpr = `(
  SELECT
    CASE
      WHEN COALESCE(
        bool_or(
          (elem->>'reason') IN (
            'hedge_triggered',
            'hedge_launched',
            'hedge_winner',
            'hedge_loser_cancelled',
            'hedge_loser_billed'
          )
        ),
        false
      )
      THEN 0
      ELSE GREATEST(
        COALESCE(
          sum(
            CASE
              WHEN (
                (elem->>'reason') IN (
                  'concurrent_limit_failed',
                  'response_incomplete',
                  'retry_failed',
                  'system_error',
                  'resource_not_found',
                  'client_error_non_retryable',
                  'unsupported',
                  'endpoint_pool_exhausted',
                  'vendor_type_all_timeout',
                  'client_abort',
                  'client_abort_no_first_byte',
                  'http2_fallback'
                )
                OR (
                  (elem->>'reason') IN ('request_success', 'retry_success')
                  AND (elem->>'statusCode') IS NOT NULL
                  AND (elem->>'statusCode') <> '0'
                )
              )
              THEN 1
              ELSE 0
            END
          ),
          0
        ) - 1,
        0
      )
    END
  FROM jsonb_array_elements(COALESCE(%s, '[]'::jsonb)) AS elem
)`

// UsageLogFilters 复刻 usage-logs.ts:31 的 UsageLogFilters（分页参数另行传入）。
type UsageLogFilters struct {
	UserID                      *int64
	KeyID                       *int64
	ProviderID                  *int64
	SessionID                   string
	StartTime                   *int64
	EndTime                     *int64
	StatusCode                  *int
	ExcludeStatusCode200        bool
	Model                       string
	ActualResponseModelMismatch bool
	Endpoint                    string
	MinRetryCount               int
	ReplayFilter                UsageLogReplayFilter
}

// UsageLogCursor 是游标分页的定位点。CreatedAt 为 RFC3339 文本，与 Node 的入参同形。
type UsageLogCursor struct {
	CreatedAt string
	ID        int64
}

// UsageLogIncremental 描述「只取新行」的增量读：`id > SinceID`、按 id 升序。
//
// 为什么按 id 而不是 created_at 做水位：created_at 不单调（夹具与回填都会插入时间更早的行），
// 以它为水位会**永久漏掉**「后插入但时间更早」的行；id 是插入序，`id > SinceID` 是唯一无缝隙的
// 增量水位，且走主键索引。
// 两个分支的「id」都是响应里那个 id：message_request 用 m.id，ledger 回退用 l.request_id
// （ledgerFallbackRowFields 的 id 就取自它），因此同一个水位在两条分支上语义一致。
type UsageLogIncremental struct {
	SinceID int64
}

// UsageLogSummary 复刻 UsageLogSummary（usage-logs.ts:278）。
type UsageLogSummary struct {
	TotalRequests              int64
	TotalCost                  float64
	TotalTokens                int64
	TotalInputTokens           int64
	TotalOutputTokens          int64
	TotalCacheCreationTokens   int64
	TotalCacheReadTokens       int64
	TotalCacheCreation5mTokens int64
	TotalCacheCreation1hTokens int64
}

// UsageLogRow 是列表行。字段与 Node 的 select 逐字对应；JSONB 列保留原始字节，由上层原样
// 透出（Node 也是把 jsonb 直接放进响应）。
type UsageLogRow struct {
	ID                         int64
	CreatedAt                  *time.Time
	SessionID                  *string
	SourceSessionID            *string
	SessionIdentityKind        *string
	RequestSequence            *int64
	UserName                   *string
	KeyName                    *string
	ProviderName               *string
	Model                      *string
	OriginalModel              *string
	ActualResponseModel        *string
	Endpoint                   *string
	StatusCode                 *int
	InputTokens                *int64
	OutputTokens               *int64
	CacheCreationInputTokens   *int64
	CacheReadInputTokens       *int64
	CacheCreation5mInputTokens *int64
	CacheCreation1hInputTokens *int64
	CacheTTLApplied            *string
	TheoreticalCacheTokens     *int64
	CacheScoreEligible         *bool
	CacheScoreExcludedReason   *string
	CostUSD                    *string
	CostMultiplier             *string
	GroupCostMultiplier        *string
	CostBreakdown              []byte
	HedgeLosers                []byte
	DurationMs                 *int64
	TTFBMs                     *int64
	FirstByteMs                *int64
	ErrorMessage               *string
	ProviderChain              []byte
	RoutingTrace               []byte
	BlockedBy                  *string
	BlockedReason              *string
	IsReplay                   bool
	ReplaySourceRequestID      *int64
	UserAgent                  *string
	ClientIP                   *string
	MessagesCount              *int64
	Context1mApplied           *bool
	SwapCacheTTLApplied        *bool
	SpecialSettings            []byte

	// CreatedAtRaw 只在游标路径出现：Node 在 findUsageLogsBatch 的 select 里多取了一列
	// `to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')`，且映射时用
	// `...row` 原样透出，因而**出现在响应体里**。偏移路径不取该列，故也不出现。这不是笔误：
	// Node 是事实源，Go 照抄。
	CreatedAtRaw *string
}

// LedgerUsageLogRow 是 ledger-only 回退路径的行（Node 的 fallbackLogs 用到的字段）。
type LedgerUsageLogRow struct {
	ID                         int64
	CreatedAt                  *time.Time
	SessionID                  *string
	SourceSessionID            *string
	SessionIdentityKind        *string
	UserID                     *int64
	UserName                   *string
	Key                        *string
	KeyName                    *string
	ProviderName               *string
	Model                      *string
	OriginalModel              *string
	ActualResponseModel        *string
	Endpoint                   *string
	StatusCode                 *int
	InputTokens                *int64
	OutputTokens               *int64
	CacheCreationInputTokens   *int64
	CacheReadInputTokens       *int64
	CacheCreation5mInputTokens *int64
	CacheCreation1hInputTokens *int64
	CacheTTLApplied            *string
	CostUSD                    *string
	CostMultiplier             *string
	GroupCostMultiplier        *string
	DurationMs                 *int64
	TTFBMs                     *int64
	FirstByteMs                *int64
	ClientIP                   *string
	Context1mApplied           *bool
	SwapCacheTTLApplied        *bool
	IsReplay                   bool
	ReplaySourceRequestID      *int64
}

// messageLogColumns 是列表行的取列清单。时间列在 Go 侧格式化，numeric 列取 ::text，
// 与 Node 通过 pg 驱动拿到的形式一致。
const messageLogColumns = `
	m.id, m.created_at, COALESCE(m.session_identity, m.session_id), m.session_id,
	m.session_identity_kind,
	m.request_sequence, u.name, k.name, p.name,
	m.model, m.original_model, m.actual_response_model, m.endpoint, m.status_code,
	m.input_tokens, m.output_tokens, m.cache_creation_input_tokens, m.cache_read_input_tokens,
	m.cache_creation_5m_input_tokens, m.cache_creation_1h_input_tokens, m.cache_ttl_applied,
	m.theoretical_cache_tokens, m.cache_score_eligible, m.cache_score_excluded_reason,
	m.cost_usd::text, m.cost_multiplier::text, m.group_cost_multiplier::text,
	m.cost_breakdown, m.hedge_losers, m.duration_ms, m.ttfb_ms, m.first_byte_ms,
	m.error_message, m.provider_chain, m.routing_trace, m.blocked_by, m.blocked_reason,
	m.is_replay, m.replay_source_request_id, m.user_agent, m.client_ip, m.messages_count,
	m.context_1m_applied, m.swap_cache_ttl_applied, m.special_settings`

const messageLogJoins = `
	FROM message_request m
	INNER JOIN users u ON u.id = m.user_id
	INNER JOIN keys k ON k.key = m.key
	LEFT JOIN providers p ON p.id = m.provider_id`

const messageLogOrder = ` ORDER BY m.created_at DESC, m.id DESC`

// messageLogOrderIncremental 是增量读的排序：按 id 升序（见 UsageLogIncremental 的说明）。
const messageLogOrderIncremental = ` ORDER BY m.id ASC`

// usageLogConditionBuilder 累积 SQL 条件与参数。参数一律走 $n 占位，因此条件拼装顺序即
// 参数顺序（Node 用 drizzle 的同名机制）。
type usageLogConditionBuilder struct {
	conditions []string
	args       []any
}

// add 加入一段无参数的条件。
func (b *usageLogConditionBuilder) add(condition string) {
	b.conditions = append(b.conditions, condition)
}

// addValue 加入一段带一个参数的条件；condition 里的 %s 是占位符位置。
func (b *usageLogConditionBuilder) addValue(condition string, arg any) {
	b.args = append(b.args, arg)
	b.conditions = append(b.conditions, fmt.Sprintf(condition, "$"+strconv.Itoa(len(b.args))))
}

// addSessionCondition 加入「规范 identity（COALESCE(session_identity, session_id)）」条件；
// 非保留前缀（pfx:/sid:）时**同时**匹配物理 session_id 列，与 Node 的双分支一致。
func (b *usageLogConditionBuilder) addSessionCondition(
	expression string,
	physicalColumn string,
	value string,
	reserved bool,
) {
	b.args = append(b.args, value)
	canonical := expression + " = $" + strconv.Itoa(len(b.args))
	if reserved {
		b.conditions = append(b.conditions, canonical)
		return
	}
	b.args = append(b.args, value)
	b.conditions = append(b.conditions,
		"("+canonical+" OR "+physicalColumn+" = $"+strconv.Itoa(len(b.args))+")")
}

// addCursorCondition 加入 keyset 分页条件 (created_at, id) < (cursor, cursorId)。
func (b *usageLogConditionBuilder) addCursorCondition(
	createdColumn string,
	idColumn string,
	createdAt time.Time,
	id int64,
) {
	b.args = append(b.args, createdAt.UTC())
	first := "$" + strconv.Itoa(len(b.args))
	b.args = append(b.args, id)
	second := "$" + strconv.Itoa(len(b.args))
	b.conditions = append(b.conditions,
		"("+createdColumn+", "+idColumn+") < ("+first+", "+second+")")
}

// where 返回 WHERE 子句（无条件时为 TRUE）。
func (b *usageLogConditionBuilder) where() string {
	if len(b.conditions) == 0 {
		return "TRUE"
	}
	return strings.Join(b.conditions, " AND ")
}

// IsReservedSessionIdentity 复刻 usage-log-filters.ts:23：前缀 identity 不做物理 session 匹配。
func IsReservedSessionIdentity(value string) bool {
	return strings.HasPrefix(value, "pfx:") || strings.HasPrefix(value, "sid:")
}

// normalizeUsageLogEndpoint 复刻 usage-log-filters.ts:27。
func normalizeUsageLogEndpoint(endpoint string) string {
	trimmed := strings.ToLower(strings.TrimSpace(endpoint))
	if trimmed == "" || trimmed == "/" {
		return trimmed
	}
	return strings.TrimRight(trimmed, "/")
}

// normalizedEndpointSQL 复刻 buildNormalizedEndpointSql。
func normalizedEndpointSQL(column string) string {
	return fmt.Sprintf(`LOWER(REGEXP_REPLACE(%s, '/+$', ''))`, column)
}

// addHiddenEndpointCondition 复刻 buildDefaultHiddenUsageLogEndpointCondition：
// **未显式给 endpoint 筛选时**，默认隐藏两类不计费端点。
func addHiddenEndpointCondition(b *usageLogConditionBuilder, column, explicitEndpoint string) {
	if strings.TrimSpace(explicitEndpoint) != "" {
		return
	}
	literals := make([]string, 0, len(NonBillingEndpoints))
	for _, endpoint := range NonBillingEndpoints {
		literals = append(literals, "'"+endpoint+"'")
	}
	b.add(fmt.Sprintf("(%s IS NULL OR %s NOT IN (%s))",
		column, normalizedEndpointSQL(column), strings.Join(literals, ", ")))
}

// addEndpointMatchCondition 复刻 buildUsageLogEndpointMatchCondition。
func addEndpointMatchCondition(b *usageLogConditionBuilder, column, explicitEndpoint string) {
	if strings.TrimSpace(explicitEndpoint) == "" {
		return
	}
	b.addValue(normalizedEndpointSQL(column)+` = %s`, normalizeUsageLogEndpoint(explicitEndpoint))
}

// actualResponseModelMismatchCondition 复刻 buildActualResponseModelMismatchCondition。
func actualResponseModelMismatchCondition(modelColumn, actualColumn, fallbackColumn string) string {
	effective := fmt.Sprintf(
		"COALESCE(NULLIF(btrim(%s), ''), NULLIF(btrim(%s), ''))", modelColumn, fallbackColumn,
	)
	actual := fmt.Sprintf("NULLIF(btrim(%s), '')", actualColumn)
	return fmt.Sprintf("(%s IS NOT NULL AND %s IS NOT NULL AND %s <> %s)",
		effective, actual, effective, actual)
}

// buildMessageConditions 复刻 buildUsageLogConditions（message_request 版）。
func buildMessageConditions(b *usageLogConditionBuilder, filters UsageLogFilters) {
	if trimmed := strings.TrimSpace(filters.SessionID); trimmed != "" {
		b.addSessionCondition(
			"COALESCE(m.session_identity, m.session_id)", "m.session_id", trimmed,
			IsReservedSessionIdentity(trimmed),
		)
	}
	if filters.StartTime != nil {
		b.addValue(`m.created_at >= %s`, time.UnixMilli(*filters.StartTime).UTC())
	}
	if filters.EndTime != nil {
		b.addValue(`m.created_at < %s`, time.UnixMilli(*filters.EndTime).UTC())
	}
	if filters.StatusCode != nil {
		b.addValue(`m.status_code = %s`, *filters.StatusCode)
	} else if filters.ExcludeStatusCode200 {
		b.add(`(m.status_code IS NULL OR m.status_code <> 200)`)
	}
	if filters.Model != "" {
		b.addValue(`m.model = %s`, filters.Model)
	}
	if filters.ActualResponseModelMismatch {
		b.add(actualResponseModelMismatchCondition(
			"m.model", "m.actual_response_model", "m.original_model"))
	}
	switch filters.ReplayFilter {
	case ReplayFilterReplay:
		b.add(`m.is_replay = TRUE`)
	case ReplayFilterNonReplay:
		b.add(`m.is_replay = FALSE`)
	}
	addHiddenEndpointCondition(b, "m.endpoint", filters.Endpoint)
	addEndpointMatchCondition(b, "m.endpoint", filters.Endpoint)
	if filters.MinRetryCount > 0 {
		b.addValue(fmt.Sprintf(RetryCountExpr, "m.provider_chain")+` >= %s`, filters.MinRetryCount)
	}
}

// buildLedgerConditions 复刻 buildLedgerUsageLogConditions + 统计/回退分支的其余条件。
func buildLedgerConditions(b *usageLogConditionBuilder, filters UsageLogFilters) {
	switch filters.ReplayFilter {
	case ReplayFilterReplay:
		b.add(`l.is_replay = TRUE`)
	case ReplayFilterNonReplay:
		b.add(`l.is_replay = FALSE`)
	}
	if trimmed := strings.TrimSpace(filters.SessionID); trimmed != "" {
		b.addSessionCondition(
			"COALESCE(l.session_identity, l.session_id)", "l.session_id", trimmed,
			IsReservedSessionIdentity(trimmed),
		)
	}
	if filters.StartTime != nil {
		b.addValue(`l.created_at >= %s`, time.UnixMilli(*filters.StartTime).UTC())
	}
	if filters.EndTime != nil {
		b.addValue(`l.created_at < %s`, time.UnixMilli(*filters.EndTime).UTC())
	}
	if filters.StatusCode != nil {
		b.addValue(`l.status_code = %s`, *filters.StatusCode)
	} else if filters.ExcludeStatusCode200 {
		b.add(`(l.status_code IS NULL OR l.status_code <> 200)`)
	}
	if filters.Model != "" {
		b.addValue(`l.model = %s`, filters.Model)
	}
	if filters.ActualResponseModelMismatch {
		b.add(actualResponseModelMismatchCondition(
			"l.model", "l.actual_response_model", "l.original_model"))
	}
	addHiddenEndpointCondition(b, "l.endpoint", filters.Endpoint)
	addEndpointMatchCondition(b, "l.endpoint", filters.Endpoint)
}

// FindUsageLogsBatch 复刻 findUsageLogsBatch 的 message_request 分支：keyset 分页，
// 多取一行判断 hasMore。
//
// incremental 非 nil 时切到**增量读**（`id > SinceID` 升序），忽略 cursor；两者互斥由调用方
// 保证（管理面把「同时给 sinceId 与游标分页」判为 400）。
func (p *Pools) FindUsageLogsBatch(
	ctx context.Context,
	filters UsageLogFilters,
	cursor *UsageLogCursor,
	limit int,
	incremental *UsageLogIncremental,
) ([]UsageLogRow, bool, *UsageLogCursor, error) {
	safeLimit := clampUsageLogLimit(limit, 1, 100)

	b := &usageLogConditionBuilder{conditions: []string{"m.deleted_at IS NULL"}}
	if filters.UserID != nil {
		b.addValue(`m.user_id = %s`, *filters.UserID)
	}
	if filters.KeyID != nil {
		b.addValue(`k.id = %s`, *filters.KeyID)
	}
	if filters.ProviderID != nil {
		b.addValue(`m.provider_id = %s`, *filters.ProviderID)
	}
	buildMessageConditions(b, filters)
	// 增量与游标互斥（调用方保证）：增量是「从水位往新处扫」，游标是「往旧处扫」。
	order := messageLogOrder
	switch {
	case incremental != nil:
		b.addValue(`m.id > %s`, incremental.SinceID)
		order = messageLogOrderIncremental
	case cursor != nil:
		createdAt, err := time.Parse(time.RFC3339Nano, cursor.CreatedAt)
		if err != nil {
			return nil, false, nil, fmt.Errorf("store: 游标时间非法 %q: %w", cursor.CreatedAt, err)
		}
		b.addCursorCondition("m.created_at", "m.id", createdAt, cursor.ID)
	}

	query := `SELECT ` + messageLogColumns + `,
		to_char(m.created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')` +
		messageLogJoins + ` WHERE ` + b.where() + order +
		` LIMIT ` + strconv.Itoa(safeLimit+1)

	rows, err := p.usageLogQuery(ctx, query, b.args...)
	if err != nil {
		return nil, false, nil, err
	}
	defer rows.Close()

	results, err := collectUsageLogRows(rows, safeLimit+1)
	if err != nil {
		return nil, false, nil, err
	}

	hasMore := len(results) > safeLimit
	logs := results
	if hasMore {
		logs = results[:safeLimit]
	}
	return logs, hasMore, cursorFromRows(logs), nil
}

func collectUsageLogRows(rows pgx.Rows, capacity int) ([]UsageLogRow, error) {
	results := make([]UsageLogRow, 0, capacity)
	for rows.Next() {
		var row UsageLogRow
		if err := rows.Scan(
			&row.ID, &row.CreatedAt, &row.SessionID, &row.SourceSessionID,
			&row.SessionIdentityKind, &row.RequestSequence, &row.UserName, &row.KeyName,
			&row.ProviderName, &row.Model, &row.OriginalModel, &row.ActualResponseModel,
			&row.Endpoint, &row.StatusCode, &row.InputTokens, &row.OutputTokens,
			&row.CacheCreationInputTokens, &row.CacheReadInputTokens,
			&row.CacheCreation5mInputTokens, &row.CacheCreation1hInputTokens, &row.CacheTTLApplied,
			&row.TheoreticalCacheTokens, &row.CacheScoreEligible, &row.CacheScoreExcludedReason,
			&row.CostUSD, &row.CostMultiplier, &row.GroupCostMultiplier, &row.CostBreakdown,
			&row.HedgeLosers, &row.DurationMs, &row.TTFBMs, &row.FirstByteMs, &row.ErrorMessage,
			&row.ProviderChain, &row.RoutingTrace, &row.BlockedBy, &row.BlockedReason,
			&row.IsReplay, &row.ReplaySourceRequestID, &row.UserAgent, &row.ClientIP,
			&row.MessagesCount, &row.Context1mApplied, &row.SwapCacheTTLApplied,
			&row.SpecialSettings, &row.CreatedAtRaw,
		); err != nil {
			return nil, fmt.Errorf("store: 读取使用日志行失败: %w", err)
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历使用日志行失败: %w", err)
	}
	return results, nil
}

// collectUsageLogRowsOffset 读偏移分页的行（无 createdAtRaw 列）。
func collectUsageLogRowsOffset(rows pgx.Rows, capacity int) ([]UsageLogRow, error) {
	results := make([]UsageLogRow, 0, capacity)
	for rows.Next() {
		var row UsageLogRow
		if err := rows.Scan(
			&row.ID, &row.CreatedAt, &row.SessionID, &row.SourceSessionID,
			&row.SessionIdentityKind, &row.RequestSequence, &row.UserName, &row.KeyName,
			&row.ProviderName, &row.Model, &row.OriginalModel, &row.ActualResponseModel,
			&row.Endpoint, &row.StatusCode, &row.InputTokens, &row.OutputTokens,
			&row.CacheCreationInputTokens, &row.CacheReadInputTokens,
			&row.CacheCreation5mInputTokens, &row.CacheCreation1hInputTokens, &row.CacheTTLApplied,
			&row.TheoreticalCacheTokens, &row.CacheScoreEligible, &row.CacheScoreExcludedReason,
			&row.CostUSD, &row.CostMultiplier, &row.GroupCostMultiplier, &row.CostBreakdown,
			&row.HedgeLosers, &row.DurationMs, &row.TTFBMs, &row.FirstByteMs, &row.ErrorMessage,
			&row.ProviderChain, &row.RoutingTrace, &row.BlockedBy, &row.BlockedReason,
			&row.IsReplay, &row.ReplaySourceRequestID, &row.UserAgent, &row.ClientIP,
			&row.MessagesCount, &row.Context1mApplied, &row.SwapCacheTTLApplied,
			&row.SpecialSettings,
		); err != nil {
			return nil, fmt.Errorf("store: 读取使用日志行失败: %w", err)
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历使用日志行失败: %w", err)
	}
	return results, nil
}

// cursorFromRows 复刻 buildNextCursorOrThrow：以最后一行为游标；无行时 cursor 为 nil。
func cursorFromRows(logs []UsageLogRow) *UsageLogCursor {
	if len(logs) == 0 {
		return nil
	}
	last := logs[len(logs)-1]
	return &UsageLogCursor{CreatedAt: formatUsageLogCursorTime(last.CreatedAt), ID: last.ID}
}

// UsageLogsPage 是偏移分页的结果（列表 + 总数 + 统计）。
type UsageLogsPage struct {
	Logs    []UsageLogRow
	Total   int64
	Summary UsageLogSummary
}

// FindUsageLogsWithDetails 复刻 findUsageLogsWithDetails：一条聚合查询 + 一条分页查询。
//
// 两处与 Node 一致：total 计入 warmup 而 summary 一律排除；只有给了 keyId 才 join keys。
func (p *Pools) FindUsageLogsWithDetails(
	ctx context.Context,
	filters UsageLogFilters,
	page int,
	pageSize int,
) (*UsageLogsPage, error) {
	safePage := page
	if safePage < 1 {
		safePage = 1
	}
	safePageSize := clampUsageLogLimit(pageSize, 1, 200)

	summaryBuilder := &usageLogConditionBuilder{conditions: []string{"m.deleted_at IS NULL"}}
	if filters.UserID != nil {
		summaryBuilder.addValue(`m.user_id = %s`, *filters.UserID)
	}
	if filters.ProviderID != nil {
		summaryBuilder.addValue(`m.provider_id = %s`, *filters.ProviderID)
	}
	buildMessageConditions(summaryBuilder, filters)

	summaryQuery := `SELECT
		count(*)::double precision AS total_rows,
		count(*) FILTER (WHERE ` + ExcludeWarmupCondition + `)::double precision AS total_requests,
		COALESCE(sum(m.cost_usd) FILTER (WHERE ` + ExcludeWarmupCondition + `), 0)::text,
		COALESCE(sum(m.input_tokens) FILTER (WHERE ` + ExcludeWarmupCondition + `)::double precision, 0::double precision),
		COALESCE(sum(m.output_tokens) FILTER (WHERE ` + ExcludeWarmupCondition + `)::double precision, 0::double precision),
		COALESCE(sum(m.cache_creation_input_tokens) FILTER (WHERE ` + ExcludeWarmupCondition + `)::double precision, 0::double precision),
		COALESCE(sum(m.cache_read_input_tokens) FILTER (WHERE ` + ExcludeWarmupCondition + `)::double precision, 0::double precision),
		COALESCE(sum(m.cache_creation_5m_input_tokens) FILTER (WHERE ` + ExcludeWarmupCondition + `)::double precision, 0::double precision),
		COALESCE(sum(m.cache_creation_1h_input_tokens) FILTER (WHERE ` + ExcludeWarmupCondition + `)::double precision, 0::double precision)
		FROM message_request m`
	if filters.KeyID != nil {
		summaryQuery += ` INNER JOIN keys k ON k.key = m.key`
	}
	summaryQuery += ` WHERE ` + summaryBuilder.where()

	listBuilder := &usageLogConditionBuilder{conditions: []string{"m.deleted_at IS NULL"}}
	if filters.UserID != nil {
		listBuilder.addValue(`m.user_id = %s`, *filters.UserID)
	}
	if filters.KeyID != nil {
		listBuilder.addValue(`k.id = %s`, *filters.KeyID)
	}
	if filters.ProviderID != nil {
		listBuilder.addValue(`m.provider_id = %s`, *filters.ProviderID)
	}
	buildMessageConditions(listBuilder, filters)

	listQuery := `SELECT ` + messageLogColumns + messageLogJoins +
		` WHERE ` + listBuilder.where() + messageLogOrder +
		` LIMIT ` + strconv.Itoa(safePageSize) +
		` OFFSET ` + strconv.Itoa((safePage-1)*safePageSize)

	// 两条查询串行而非并发：Node 用 Promise.all 并发，但同一连接池内串行不改变结果，
	// 且少占一条连接（管理面读面本来不是吞吐瓶颈）。
	pool, err := p.usageLogControl()
	if err != nil {
		return nil, err
	}
	var totalRows, totalRequests float64
	var totalCost string
	var totalInput, totalOutput, totalCacheCreation, totalCacheRead float64
	var totalCacheCreation5m, totalCacheCreation1h float64
	if err := pool.QueryRow(ctx, summaryQuery, summaryBuilder.args...).Scan(
		&totalRows, &totalRequests, &totalCost, &totalInput, &totalOutput,
		&totalCacheCreation, &totalCacheRead, &totalCacheCreation5m, &totalCacheCreation1h,
	); err != nil {
		return nil, fmt.Errorf("store: 使用日志聚合查询失败: %w", err)
	}

	rows, err := pool.Query(ctx, listQuery, listBuilder.args...)
	if err != nil {
		return nil, fmt.Errorf("store: 使用日志分页查询失败: %w", err)
	}
	defer rows.Close()
	logs, err := collectUsageLogRowsOffset(rows, safePageSize)
	if err != nil {
		return nil, err
	}

	return &UsageLogsPage{
		Logs:  logs,
		Total: int64(totalRows),
		Summary: UsageLogSummary{
			TotalRequests:              int64(totalRequests),
			TotalCost:                  parseUsageLogCost(totalCost),
			TotalTokens:                int64(totalInput + totalOutput + totalCacheCreation + totalCacheRead),
			TotalInputTokens:           int64(totalInput),
			TotalOutputTokens:          int64(totalOutput),
			TotalCacheCreationTokens:   int64(totalCacheCreation),
			TotalCacheReadTokens:       int64(totalCacheRead),
			TotalCacheCreation5mTokens: int64(totalCacheCreation5m),
			TotalCacheCreation1hTokens: int64(totalCacheCreation1h),
		},
	}, nil
}

// FindUsageLogsStats 复刻 findUsageLogsStats 的三条分支：ledger-only 且带重试筛选时空结果；
// 非计费端点走 message_request 且成本恒 0；其余一律走 usage_ledger。
func (p *Pools) FindUsageLogsStats(
	ctx context.Context,
	filters UsageLogFilters,
	ledgerOnly bool,
) (UsageLogSummary, error) {
	minRetryCount := filters.MinRetryCount
	if ledgerOnly && minRetryCount > 0 {
		return UsageLogSummary{}, nil
	}

	if !ledgerOnly && IsNonBillingEndpoint(filters.Endpoint) {
		b := &usageLogConditionBuilder{
			conditions: []string{"m.deleted_at IS NULL", ExcludeWarmupCondition},
		}
		if filters.UserID != nil {
			b.addValue(`m.user_id = %s`, *filters.UserID)
		}
		if filters.KeyID != nil {
			b.addValue(`k.id = %s`, *filters.KeyID)
		}
		if filters.ProviderID != nil {
			b.addValue(`m.provider_id = %s`, *filters.ProviderID)
		}
		buildMessageConditions(b, filters)

		query := `SELECT
			count(*)::double precision,
			COALESCE(sum(m.input_tokens)::double precision, 0::double precision),
			COALESCE(sum(m.output_tokens)::double precision, 0::double precision),
			COALESCE(sum(m.cache_creation_input_tokens)::double precision, 0::double precision),
			COALESCE(sum(m.cache_read_input_tokens)::double precision, 0::double precision),
			COALESCE(sum(m.cache_creation_5m_input_tokens)::double precision, 0::double precision),
			COALESCE(sum(m.cache_creation_1h_input_tokens)::double precision, 0::double precision)
			FROM message_request m`
		if filters.KeyID != nil {
			query += ` INNER JOIN keys k ON k.key = m.key`
		}
		query += ` WHERE ` + b.where()
		return p.scanNonBillingStats(ctx, query, b.args...)
	}

	b := &usageLogConditionBuilder{conditions: []string{"l.blocked_by IS NULL"}}
	buildLedgerConditions(b, filters)
	if filters.UserID != nil {
		b.addValue(`l.user_id = %s`, *filters.UserID)
	}
	if filters.KeyID != nil {
		b.addValue(`k.id = %s`, *filters.KeyID)
	}
	if filters.ProviderID != nil {
		b.addValue(`l.final_provider_id = %s`, *filters.ProviderID)
	}
	if minRetryCount > 0 && !ledgerOnly {
		b.addValue(fmt.Sprintf(RetryCountExpr, "m.provider_chain")+` >= %s`, minRetryCount)
	}

	query := `SELECT
		count(*)::double precision,
		COALESCE(sum(l.cost_usd), 0)::text,
		COALESCE(sum(l.input_tokens)::double precision, 0::double precision),
		COALESCE(sum(l.output_tokens)::double precision, 0::double precision),
		COALESCE(sum(l.cache_creation_input_tokens)::double precision, 0::double precision),
		COALESCE(sum(l.cache_read_input_tokens)::double precision, 0::double precision),
		COALESCE(sum(l.cache_creation_5m_input_tokens)::double precision, 0::double precision),
		COALESCE(sum(l.cache_creation_1h_input_tokens)::double precision, 0::double precision)
		FROM usage_ledger l`
	if filters.KeyID != nil {
		query += ` INNER JOIN keys k ON k.key = l.key`
	}
	if minRetryCount > 0 && !ledgerOnly {
		query += ` INNER JOIN message_request m ON m.id = l.request_id`
	}
	query += ` WHERE ` + b.where()
	return p.scanLedgerStats(ctx, query, b.args...)
}

func (p *Pools) scanNonBillingStats(ctx context.Context, query string, args ...any) (UsageLogSummary, error) {
	pool, err := p.usageLogControl()
	if err != nil {
		return UsageLogSummary{}, err
	}
	var totalRequests, input, output, cacheCreation, cacheRead, cache5m, cache1h float64
	if err := pool.QueryRow(ctx, query, args...).Scan(
		&totalRequests, &input, &output, &cacheCreation, &cacheRead, &cache5m, &cache1h,
	); err != nil {
		return UsageLogSummary{}, fmt.Errorf("store: 使用日志统计查询失败: %w", err)
	}
	return UsageLogSummary{
		TotalRequests:              int64(totalRequests),
		TotalTokens:                int64(input + output + cacheCreation + cacheRead),
		TotalInputTokens:           int64(input),
		TotalOutputTokens:          int64(output),
		TotalCacheCreationTokens:   int64(cacheCreation),
		TotalCacheReadTokens:       int64(cacheRead),
		TotalCacheCreation5mTokens: int64(cache5m),
		TotalCacheCreation1hTokens: int64(cache1h),
	}, nil
}

func (p *Pools) scanLedgerStats(ctx context.Context, query string, args ...any) (UsageLogSummary, error) {
	pool, err := p.usageLogControl()
	if err != nil {
		return UsageLogSummary{}, err
	}
	var totalRequests, input, output, cacheCreation, cacheRead, cache5m, cache1h float64
	var totalCost string
	if err := pool.QueryRow(ctx, query, args...).Scan(
		&totalRequests, &totalCost, &input, &output, &cacheCreation, &cacheRead, &cache5m, &cache1h,
	); err != nil {
		return UsageLogSummary{}, fmt.Errorf("store: 使用日志统计查询失败: %w", err)
	}
	return UsageLogSummary{
		TotalRequests:              int64(totalRequests),
		TotalCost:                  parseUsageLogCost(totalCost),
		TotalTokens:                int64(input + output + cacheCreation + cacheRead),
		TotalInputTokens:           int64(input),
		TotalOutputTokens:          int64(output),
		TotalCacheCreationTokens:   int64(cacheCreation),
		TotalCacheReadTokens:       int64(cacheRead),
		TotalCacheCreation5mTokens: int64(cache5m),
		TotalCacheCreation1hTokens: int64(cache1h),
	}, nil
}

// FindUsageLogsBatchLedger 复刻 findUsageLogsBatch 的 ledger-only 回退分支。
//
// rawLimit 是调用方**原始**传入的 limit（未做 1..100 夹取）。Node 在这个分支里写的是
// `ledgerHasMore = ledgerResults.length > limit`——用的是原始值而不是 safeLimit，
// 未传 limit 时该比较恒为 false（JS 里 `n > undefined` 为假），hasMore 因此恒假。
// 这里如实复刻，避免与 Node 分叉。
//
// incremental 非 nil 时同样支持增量读（`request_id > SinceID`，request_id 就是响应里的 id）。
func (p *Pools) FindUsageLogsBatchLedger(
	ctx context.Context,
	filters UsageLogFilters,
	cursor *UsageLogCursor,
	rawLimit *int,
	incremental *UsageLogIncremental,
) ([]LedgerUsageLogRow, bool, *UsageLogCursor, error) {
	safeLimit := 50
	if rawLimit != nil {
		safeLimit = clampUsageLogLimit(*rawLimit, 1, 100)
	}

	b := &usageLogConditionBuilder{conditions: []string{"l.blocked_by IS NULL"}}
	buildLedgerConditions(b, filters)
	if filters.UserID != nil {
		b.addValue(`l.user_id = %s`, *filters.UserID)
	}
	if filters.KeyID != nil {
		b.addValue(`k.id = %s`, *filters.KeyID)
	}
	if filters.ProviderID != nil {
		b.addValue(`l.final_provider_id = %s`, *filters.ProviderID)
	}
	order := ` ORDER BY l.created_at DESC, l.request_id DESC`
	switch {
	case incremental != nil:
		b.addValue(`l.request_id > %s`, incremental.SinceID)
		order = ` ORDER BY l.request_id ASC`
	case cursor != nil:
		createdAt, err := time.Parse(time.RFC3339Nano, cursor.CreatedAt)
		if err != nil {
			return nil, false, nil, fmt.Errorf("store: 游标时间非法 %q: %w", cursor.CreatedAt, err)
		}
		b.addCursorCondition("l.created_at", "l.request_id", createdAt, cursor.ID)
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
		l.swap_cache_ttl_applied, l.is_replay, l.replay_source_request_id
		FROM usage_ledger l
		LEFT JOIN users u ON u.id = l.user_id
		LEFT JOIN keys k ON k.key = l.key
		LEFT JOIN providers p ON p.id = l.final_provider_id
		WHERE ` + b.where() + order +
		` LIMIT ` + strconv.Itoa(safeLimit+1)

	pool, err := p.usageLogControl()
	if err != nil {
		return nil, false, nil, err
	}
	rows, err := pool.Query(ctx, query, b.args...)
	if err != nil {
		return nil, false, nil, fmt.Errorf("store: 使用日志账本回退查询失败: %w", err)
	}
	defer rows.Close()

	results := make([]LedgerUsageLogRow, 0, safeLimit+1)
	for rows.Next() {
		var row LedgerUsageLogRow
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
		); err != nil {
			return nil, false, nil, fmt.Errorf("store: 读取账本回退行失败: %w", err)
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, false, nil, fmt.Errorf("store: 遍历账本回退行失败: %w", err)
	}

	hasMore := rawLimit != nil && len(results) > *rawLimit
	logs := results
	if len(results) > safeLimit {
		logs = results[:safeLimit]
	}
	var nextCursor *UsageLogCursor
	// 增量读没有「下一页往旧处」的游标：它的继续位是调用方已看到的 max id。
	if len(logs) > 0 && incremental == nil {
		last := logs[len(logs)-1]
		nextCursor = &UsageLogCursor{CreatedAt: formatUsageLogCursorTime(last.CreatedAt), ID: last.ID}
	}
	return logs, hasMore, nextCursor, nil
}

// ListUsedModels 复刻 getUsedModels：distinct 且按模型名升序。
func (p *Pools) ListUsedModels(ctx context.Context) ([]string, error) {
	pool, err := p.usageLogControl()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `SELECT DISTINCT m.model FROM message_request m
		WHERE m.deleted_at IS NULL AND m.model IS NOT NULL AND btrim(m.model) <> ''
		ORDER BY m.model`)
	if err != nil {
		return nil, fmt.Errorf("store: 读取模型列表失败: %w", err)
	}
	defer rows.Close()
	models := make([]string, 0, 16)
	for rows.Next() {
		var model *string
		if err := rows.Scan(&model); err != nil {
			return nil, fmt.Errorf("store: 读取模型失败: %w", err)
		}
		if isNonBlankUsageLogValue(model) {
			models = append(models, *model)
		}
	}
	return models, rows.Err()
}

// ListUsedStatusCodes 复刻 getUsedStatusCodes。
func (p *Pools) ListUsedStatusCodes(ctx context.Context) ([]int, error) {
	pool, err := p.usageLogControl()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `SELECT DISTINCT m.status_code FROM message_request m
		WHERE m.deleted_at IS NULL AND m.status_code IS NOT NULL ORDER BY m.status_code`)
	if err != nil {
		return nil, fmt.Errorf("store: 读取状态码列表失败: %w", err)
	}
	defer rows.Close()
	codes := make([]int, 0, 8)
	for rows.Next() {
		var code *int
		if err := rows.Scan(&code); err != nil {
			return nil, fmt.Errorf("store: 读取状态码失败: %w", err)
		}
		if code != nil {
			codes = append(codes, *code)
		}
	}
	return codes, rows.Err()
}

// ListUsedEndpoints 复刻 getUsedEndpoints。
func (p *Pools) ListUsedEndpoints(ctx context.Context) ([]string, error) {
	pool, err := p.usageLogControl()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `SELECT DISTINCT m.endpoint FROM message_request m
		WHERE m.deleted_at IS NULL AND m.endpoint IS NOT NULL ORDER BY m.endpoint`)
	if err != nil {
		return nil, fmt.Errorf("store: 读取 endpoint 列表失败: %w", err)
	}
	defer rows.Close()
	endpoints := make([]string, 0, 8)
	for rows.Next() {
		var endpoint *string
		if err := rows.Scan(&endpoint); err != nil {
			return nil, fmt.Errorf("store: 读取 endpoint 失败: %w", err)
		}
		if endpoint != nil {
			endpoints = append(endpoints, *endpoint)
		}
	}
	return endpoints, rows.Err()
}

// UsageLogSessionIDSuggestionFilters 复刻 usage-logs.ts:1926。
type UsageLogSessionIDSuggestionFilters struct {
	Term       string
	UserID     *int64
	KeyID      *int64
	ProviderID *int64
	Limit      int
}

// FindUsageLogSessionIDSuggestions 复刻 findUsageLogSessionIdSuggestions：两条候选列
// （规范 identity 与物理 session_id）各自在子查询里按 id 倒序取 max(500, limit*25) 行，
// 再按候选分组取最近时间，最后合并去重、按时间倒序截断。
func (p *Pools) FindUsageLogSessionIDSuggestions(
	ctx context.Context,
	filters UsageLogSessionIDSuggestionFilters,
	ledgerOnly bool,
) ([]string, error) {
	limit := 20
	if filters.Limit > 0 {
		limit = filters.Limit
	}
	if limit > 50 {
		limit = 50
	}
	term := strings.TrimSpace(filters.Term)
	if term == "" {
		return []string{}, nil
	}
	pattern := escapeUsageLogLike(term) + "%"
	subqueryLimit := limit * 25
	if subqueryLimit < 500 {
		subqueryLimit = 500
	}

	table := "message_request"
	rowIDColumn := "id"
	createdColumn := "created_at"
	canonicalColumn := "COALESCE(session_identity, session_id)"
	physicalColumn := "session_id"
	scopeConditions := ExcludeWarmupCondition + " AND " + table + ".deleted_at IS NULL"
	userColumn, providerColumn := "user_id", "provider_id"
	if ledgerOnly {
		table = "usage_ledger"
		scopeConditions = "blocked_by IS NULL"
		providerColumn = "final_provider_id"
	}

	pool, err := p.usageLogControl()
	if err != nil {
		return nil, err
	}

	firstSeen := map[string]time.Time{}
	// 候选列顺序与 Node 一致：先规范 identity，再物理 session_id。
	for _, candidate := range []struct {
		expression string
		excludePfx bool
	}{
		{expression: canonicalColumn},
		{expression: physicalColumn, excludePfx: true},
	} {
		b := &usageLogConditionBuilder{
			conditions: []string{
				scopeConditions,
				candidate.expression + " IS NOT NULL",
				"length(" + candidate.expression + ") > 0",
			},
		}
		if candidate.excludePfx {
			b.add(candidate.expression + " NOT LIKE 'pfx:%' AND " +
				candidate.expression + " NOT LIKE 'sid:%'")
		}
		b.addValue(candidate.expression+` LIKE %s ESCAPE '\'`, pattern)
		if filters.UserID != nil {
			b.addValue(userColumn+" = %s", *filters.UserID)
		}
		if filters.KeyID != nil {
			b.addValue("k.id = %s", *filters.KeyID)
		}
		if filters.ProviderID != nil {
			b.addValue(providerColumn+" = %s", *filters.ProviderID)
		}

		subquery := `SELECT ` + candidate.expression + ` AS session_id, ` + createdColumn +
			` AS created_at, ` + rowIDColumn + ` AS row_id FROM ` + table
		if filters.KeyID != nil {
			subquery += ` INNER JOIN keys k ON k.key = ` + table + `.key`
		}
		subquery += ` WHERE ` + b.where() + ` ORDER BY row_id DESC LIMIT ` +
			strconv.Itoa(subqueryLimit)

		query := `SELECT sub.session_id, max(sub.created_at) AS first_seen
			FROM (` + subquery + `) AS sub
			GROUP BY sub.session_id
			ORDER BY max(sub.created_at) DESC
			LIMIT ` + strconv.Itoa(limit)

		rows, err := pool.Query(ctx, query, b.args...)
		if err != nil {
			return nil, fmt.Errorf("store: sessionId 联想查询失败: %w", err)
		}
		for rows.Next() {
			var sessionID *string
			var seen *time.Time
			if err := rows.Scan(&sessionID, &seen); err != nil {
				rows.Close()
				return nil, fmt.Errorf("store: 读取 sessionId 联想行失败: %w", err)
			}
			if sessionID == nil || seen == nil {
				continue
			}
			if current, ok := firstSeen[*sessionID]; !ok || seen.After(current) {
				firstSeen[*sessionID] = *seen
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: 遍历 sessionId 联想行失败: %w", err)
		}
		rows.Close()
	}

	suggestions := make([]sessionSuggestion, 0, len(firstSeen))
	for sessionID, seen := range firstSeen {
		suggestions = append(suggestions, sessionSuggestion{sessionID: sessionID, firstSeen: seen})
	}
	sortSuggestionsByRecency(suggestions)
	if len(suggestions) > limit {
		suggestions = suggestions[:limit]
	}
	results := make([]string, 0, len(suggestions))
	for _, item := range suggestions {
		results = append(results, item.sessionID)
	}
	return results, nil
}

// sessionSuggestion 是 sessionId 联想的一条候选（session 与其最近出现时间）。
type sessionSuggestion struct {
	sessionID string
	firstSeen time.Time
}

// sortSuggestionsByRecency 按最近时间倒序；时间相同时按 sessionId 升序，保证结果稳定。
func sortSuggestionsByRecency(items []sessionSuggestion) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0; j-- {
			swapped := false
			switch {
			case items[j].firstSeen.After(items[j-1].firstSeen):
				swapped = true
			case items[j].firstSeen.Equal(items[j-1].firstSeen) &&
				items[j].sessionID < items[j-1].sessionID:
				swapped = true
			}
			if !swapped {
				break
			}
			items[j-1], items[j] = items[j], items[j-1]
		}
	}
}

// MessageRequestExists 复刻 isLedgerOnlyMode 的存在性探测（缓存与 TTL 由上层负责）。
func (p *Pools) MessageRequestExists(ctx context.Context) (bool, error) {
	pool, err := p.usageLogControl()
	if err != nil {
		return false, err
	}
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM message_request LIMIT 1) AS has_data`,
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("store: 探测 message_request 是否存在失败: %w", err)
	}
	return exists, nil
}

// SourceSessionScope 是物理 session 源查询的可见范围（Node 的 UsageLogSourceSessionScope）。
type SourceSessionScope struct {
	UserID    *int64
	KeyID     *int64
	KeyString string
}

// FindMessageSourceSessionIDs 复刻 loadMessageSourceSessionIds。
func (p *Pools) FindMessageSourceSessionIDs(
	ctx context.Context,
	sessionIDs []string,
	scope SourceSessionScope,
) (map[string][]string, error) {
	if len(sessionIDs) == 0 {
		return map[string][]string{}, nil
	}
	b := &usageLogConditionBuilder{conditions: []string{"m.deleted_at IS NULL"}}
	b.addValue(`COALESCE(m.session_identity, m.session_id) = ANY(%s)`, sessionIDs)
	if scope.UserID != nil {
		b.addValue(`m.user_id = %s`, *scope.UserID)
	}
	if scope.KeyID != nil {
		b.addValue(`k.id = %s`, *scope.KeyID)
	}
	if scope.KeyString != "" {
		b.addValue(`m.key = %s`, scope.KeyString)
	}

	query := `SELECT COALESCE(m.session_identity, m.session_id) AS session_id,
		ARRAY_AGG(DISTINCT m.session_id) FILTER (WHERE m.session_id IS NOT NULL) AS source_ids
		FROM message_request m`
	if scope.KeyID != nil {
		query += ` INNER JOIN keys k ON k.key = m.key`
	}
	query += ` WHERE ` + b.where() +
		` GROUP BY COALESCE(m.session_identity, m.session_id)`
	return p.readSourceSessionIDs(ctx, query, b.args...)
}

// FindLedgerSourceSessionIDs 复刻 loadLedgerSourceSessionIds。
func (p *Pools) FindLedgerSourceSessionIDs(
	ctx context.Context,
	sessionIDs []string,
	scope SourceSessionScope,
) (map[string][]string, error) {
	if len(sessionIDs) == 0 {
		return map[string][]string{}, nil
	}
	b := &usageLogConditionBuilder{}
	b.addValue(`COALESCE(l.session_identity, l.session_id) = ANY(%s)`, sessionIDs)
	if scope.UserID != nil {
		b.addValue(`l.user_id = %s`, *scope.UserID)
	}
	if scope.KeyID != nil {
		b.addValue(`k.id = %s`, *scope.KeyID)
	}
	if scope.KeyString != "" {
		b.addValue(`l.key = %s`, scope.KeyString)
	}

	query := `SELECT COALESCE(l.session_identity, l.session_id) AS session_id,
		ARRAY_AGG(DISTINCT l.session_id) FILTER (WHERE l.session_id IS NOT NULL) AS source_ids
		FROM usage_ledger l`
	if scope.KeyID != nil {
		query += ` INNER JOIN keys k ON k.key = l.key`
	}
	query += ` WHERE ` + b.where() +
		` GROUP BY COALESCE(l.session_identity, l.session_id)`
	return p.readSourceSessionIDs(ctx, query, b.args...)
}

func (p *Pools) readSourceSessionIDs(
	ctx context.Context,
	query string,
	args ...any,
) (map[string][]string, error) {
	pool, err := p.usageLogControl()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 读取物理 session 源失败: %w", err)
	}
	defer rows.Close()
	results := map[string][]string{}
	for rows.Next() {
		var sessionID *string
		var sourceIDs []string
		if err := rows.Scan(&sessionID, &sourceIDs); err != nil {
			return nil, fmt.Errorf("store: 读取物理 session 源行失败: %w", err)
		}
		if sessionID == nil {
			continue
		}
		results[*sessionID] = sourceIDs
	}
	return results, rows.Err()
}

// usageLogControl 返回管理面读面所用的池。
//
// Node 侧 /api/v1 的 getDb() 在没有 data scope 时取 control 分道（src/drizzle/db.ts:123），
// 管理面读与数据面热路径因此分道。
func (p *Pools) usageLogControl() (*Pool, error) {
	return p.Control()
}

// usageLogQuery 在 control 分道上执行查询。
func (p *Pools) usageLogQuery(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	pool, err := p.usageLogControl()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 使用日志查询失败: %w", err)
	}
	return rows, nil
}

// clampUsageLogLimit 把 limit 夹到 [min, max]。
func clampUsageLogLimit(value, min, max int) int {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

// formatUsageLogCursorTime 把时间渲染成游标里的 RFC3339 文本（Node 用 Date 的 ISO 形式）。
func formatUsageLogCursorTime(value *time.Time) string {
	if value == nil {
		return "1970-01-01T00:00:00.000Z"
	}
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// parseUsageLogCost 复刻 Node 的 parseFloat(totalCost ?? "0")。
func parseUsageLogCost(value string) float64 {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0
	}
	var parsed float64
	if _, err := fmt.Sscanf(trimmed, "%g", &parsed); err != nil {
		return 0
	}
	return parsed
}

// escapeUsageLogLike 复刻 src/repository/_shared/like.ts。
func escapeUsageLogLike(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, "%", `\%`)
	return strings.ReplaceAll(value, "_", `\_`)
}

// IsNonBillingEndpoint 复刻 src/lib/utils/performance-formatter.ts:17。
func IsNonBillingEndpoint(endpoint string) bool {
	if endpoint == "" {
		return false
	}
	normalized := normalizeUsageLogEndpoint(endpoint)
	for _, candidate := range NonBillingEndpoints {
		if normalized == candidate {
			return true
		}
	}
	return false
}

func isNonBlankUsageLogValue(value *string) bool {
	return value != nil && strings.TrimSpace(*value) != ""
}
