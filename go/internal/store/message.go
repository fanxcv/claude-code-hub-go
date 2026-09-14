package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// MessageWriteMode 复刻 MESSAGE_REQUEST_WRITE_MODE：sync 直写、async 经 writer 分道。
type MessageWriteMode string

const (
	MessageWriteSync  MessageWriteMode = "sync"
	MessageWriteAsync MessageWriteMode = "async"
)

// messageRequestColumns 是 CreateMessageRequest 的 INSERT 列表，顺序即参数顺序。
// 列名与 src/drizzle/schema.ts 的 messageRequest 定义一一对应。
var messageRequestColumns = []string{
	"provider_id",
	"user_id",
	"key",
	"model",
	"original_model",
	"duration_ms",
	"cost_usd",
	"cost_multiplier",
	"group_cost_multiplier",
	"session_id",
	"session_identity",
	"session_identity_kind",
	"affinity_scope_tag",
	"affinity_fingerprint",
	"affinity_fingerprint_chain",
	"is_replay",
	"replay_source_request_id",
	"request_sequence",
	"routing_trace",
	"user_agent",
	"client_ip",
	"endpoint",
	"messages_count",
	"special_settings",
	"cache_ttl_applied",
	"cache_creation_input_tokens",
	"cache_creation_5m_input_tokens",
	"cache_creation_1h_input_tokens",
	"cache_read_input_tokens",
}

// messageRequestReturning 是 INSERT ... RETURNING 的表达式列表，复刻 TS 的返回集合。
// numeric 一律显式转 text，避免扫描时依赖驱动的数值解码路径。
var messageRequestReturning = []string{
	"id", "provider_id", "user_id", "key", "model", "original_model", "duration_ms",
	"cost_usd::text", "cost_multiplier::text", "group_cost_multiplier::text",
	"session_id", "session_identity", "session_identity_kind", "affinity_scope_tag",
	"affinity_fingerprint", "affinity_fingerprint_chain", "is_replay",
	"replay_source_request_id", "request_sequence", "routing_trace", "user_agent",
	"client_ip", "endpoint", "messages_count", "special_settings", "cache_ttl_applied",
	"cache_creation_input_tokens", "cache_creation_5m_input_tokens",
	"cache_creation_1h_input_tokens", "cache_read_input_tokens",
	"status_code", "created_at", "updated_at", "deleted_at",
}

// CreateMessageRequestData 复刻 CreateMessageRequestData 中本包用到的字段。
// 指针字段对应 TS 的「未提供即不写」；非指针项对应 NOT NULL 列。
type CreateMessageRequestData struct {
	ProviderID                 int64
	UserID                     int64
	Key                        string
	Model                      *string
	OriginalModel              *string
	DurationMS                 *int
	CostUSD                    *string
	CostMultiplier             *string
	GroupCostMultiplier        *string
	SessionID                  *string
	SessionIdentity            *string
	SessionIdentityKind        *string
	AffinityScopeTag           *string
	AffinityFingerprint        *string
	AffinityFingerprintChain   []byte
	IsReplay                   bool
	ReplaySourceRequestID      *int
	RequestSequence            *int
	RoutingTrace               []byte
	UserAgent                  *string
	ClientIP                   *string
	Endpoint                   *string
	MessagesCount              *int
	SpecialSettings            []byte
	CacheTTLApplied            *string
	CacheCreationInputTokens   *int64
	CacheCreation5mInputTokens *int64
	CacheCreation1hInputTokens *int64
	CacheReadInputTokens       *int64
}

// MessageRequest 是 message_request 的读取视图，只覆盖本包用到的列。
type MessageRequest struct {
	ID                         int64
	ProviderID                 int64
	UserID                     int64
	Key                        string
	Model                      *string
	OriginalModel              *string
	DurationMS                 *int
	CostUSD                    *string
	CostMultiplier             *string
	GroupCostMultiplier        *string
	SessionID                  *string
	SessionIdentity            *string
	SessionIdentityKind        *string
	AffinityScopeTag           *string
	AffinityFingerprint        *string
	AffinityFingerprintChain   []byte
	IsReplay                   bool
	ReplaySourceRequestID      *int
	RequestSequence            *int
	RoutingTrace               []byte
	UserAgent                  *string
	ClientIP                   *string
	Endpoint                   *string
	MessagesCount              *int
	SpecialSettings            []byte
	CacheTTLApplied            *string
	CacheCreationInputTokens   *int64
	CacheCreation5mInputTokens *int64
	CacheCreation1hInputTokens *int64
	CacheReadInputTokens       *int64
	StatusCode                 *int
	CreatedAt                  *time.Time
	UpdatedAt                  *time.Time
	DeletedAt                  *time.Time
}

// CreateMessageRequest 复刻 src/repository/message.ts 的 createMessageRequest：单条
// INSERT ... RETURNING。CostUSD 为 nil 或空串时按 TS 的 `formattedCost ?? undefined`
// 语义写 NULL。
func (p *Pools) CreateMessageRequest(
	ctx context.Context,
	data CreateMessageRequestData,
) (MessageRequest, error) {
	pool, err := p.Data()
	if err != nil {
		return MessageRequest{}, err
	}

	casts := map[string]string{
		"cost_usd":                   "::numeric",
		"cost_multiplier":            "::numeric",
		"group_cost_multiplier":      "::numeric",
		"affinity_fingerprint_chain": "::jsonb",
		"routing_trace":              "::jsonb",
		"special_settings":           "::jsonb",
	}

	args := make([]any, 0, len(messageRequestColumns))
	placeholders := make([]string, 0, len(messageRequestColumns))
	for index, column := range messageRequestColumns {
		value, err := insertValue(column, data)
		if err != nil {
			return MessageRequest{}, err
		}
		args = append(args, value)
		placeholders = append(placeholders, fmt.Sprintf("$%d%s", index+1, casts[column]))
	}

	query := fmt.Sprintf(
		"INSERT INTO message_request (%s) VALUES (%s) RETURNING %s",
		quoteColumns(messageRequestColumns),
		strings.Join(placeholders, ", "),
		strings.Join(messageRequestReturning, ", "),
	)

	request, err := scanMessageRequest(pool.QueryRow(ctx, query, args...))
	if err != nil {
		return MessageRequest{}, fmt.Errorf("store: 创建 message_request 失败: %w", err)
	}
	return request, nil
}

func insertValue(column string, data CreateMessageRequestData) (any, error) {
	switch column {
	case "provider_id":
		return data.ProviderID, nil
	case "user_id":
		return data.UserID, nil
	case "key":
		return data.Key, nil
	case "model":
		return data.Model, nil
	case "original_model":
		return data.OriginalModel, nil
	case "duration_ms":
		return data.DurationMS, nil
	case "cost_usd":
		return numericArg(data.CostUSD), nil
	case "cost_multiplier":
		return numericArg(data.CostMultiplier), nil
	case "group_cost_multiplier":
		return numericArg(data.GroupCostMultiplier), nil
	case "session_id":
		return data.SessionID, nil
	case "session_identity":
		return data.SessionIdentity, nil
	case "session_identity_kind":
		return data.SessionIdentityKind, nil
	case "affinity_scope_tag":
		return data.AffinityScopeTag, nil
	case "affinity_fingerprint":
		return data.AffinityFingerprint, nil
	case "affinity_fingerprint_chain":
		return jsonbArg(data.AffinityFingerprintChain), nil
	case "is_replay":
		return data.IsReplay, nil
	case "replay_source_request_id":
		return data.ReplaySourceRequestID, nil
	case "request_sequence":
		return data.RequestSequence, nil
	case "routing_trace":
		return jsonbArg(data.RoutingTrace), nil
	case "user_agent":
		return data.UserAgent, nil
	case "client_ip":
		return data.ClientIP, nil
	case "endpoint":
		return data.Endpoint, nil
	case "messages_count":
		return data.MessagesCount, nil
	case "special_settings":
		return jsonbArg(data.SpecialSettings), nil
	case "cache_ttl_applied":
		return data.CacheTTLApplied, nil
	case "cache_creation_input_tokens":
		return data.CacheCreationInputTokens, nil
	case "cache_creation_5m_input_tokens":
		return data.CacheCreation5mInputTokens, nil
	case "cache_creation_1h_input_tokens":
		return data.CacheCreation1hInputTokens, nil
	case "cache_read_input_tokens":
		return data.CacheReadInputTokens, nil
	default:
		return nil, fmt.Errorf("store: message_request 插入列未接线: %s", column)
	}
}

func scanMessageRequest(row pgx.Row) (MessageRequest, error) {
	var request MessageRequest
	err := row.Scan(
		&request.ID, &request.ProviderID, &request.UserID, &request.Key,
		&request.Model, &request.OriginalModel, &request.DurationMS,
		&request.CostUSD, &request.CostMultiplier, &request.GroupCostMultiplier,
		&request.SessionID, &request.SessionIdentity, &request.SessionIdentityKind,
		&request.AffinityScopeTag, &request.AffinityFingerprint,
		&request.AffinityFingerprintChain, &request.IsReplay,
		&request.ReplaySourceRequestID, &request.RequestSequence, &request.RoutingTrace,
		&request.UserAgent, &request.ClientIP, &request.Endpoint, &request.MessagesCount,
		&request.SpecialSettings, &request.CacheTTLApplied,
		&request.CacheCreationInputTokens, &request.CacheCreation5mInputTokens,
		&request.CacheCreation1hInputTokens, &request.CacheReadInputTokens,
		&request.StatusCode, &request.CreatedAt, &request.UpdatedAt, &request.DeletedAt,
	)
	if err != nil {
		return MessageRequest{}, err
	}
	return request, nil
}

// DetailsPatch 复刻 MessageRequestDetailsUpdate：指针为 nil 表示本次不写该列，
// 与 TS 的 `details.x !== undefined` 判定一一对应。
type DetailsPatch struct {
	DurationMS                 *int
	StatusCode                 *int
	InputTokens                *int64
	OutputTokens               *int64
	TTFTMS                     *int
	FirstByteMS                *int
	CacheCreationInputTokens   *int64
	CacheReadInputTokens       *int64
	CacheCreation5mInputTokens *int64
	CacheCreation1hInputTokens *int64
	CacheTTLApplied            *string
	// CostMultiplier / GroupCostMultiplier 是本次请求实际使用的成本倍率（numeric 列的文本形）。
	// Node 在建行时写（message-service.ts:125-126），Go 在终态写；理由与残余风险见
	CostMultiplier      *string
	GroupCostMultiplier *string
	ProviderChain       []byte
	RoutingTrace        []byte
	ErrorMessage        *string
	ErrorStack          *string
	ErrorCause          *string
	Model               *string
	ActualResponseModel *string
	ProviderID          *int64
	Context1mApplied    *bool
	SwapCacheTTLApplied *bool
	SpecialSettings     []byte
	// SpecialSettingsAppend 以 **jsonb 追加**（`COALESCE(col,'[]') || $n`）写入，
	// 用于「建行时已写客户端侧审计、终态只补自己那条」的两段写入。
	SpecialSettingsAppend    []byte
	CacheCompatibilityKey    *string
	CacheScoreEligible       *bool
	CacheScoreExcludedReason *string
	TheoreticalCacheTokens   *int64
	CacheTTLBucket           *string
	// BlockedBy/BlockedReason 是拦截类终态（敏感词、预热等，对应
	// sensitive-word-guard.ts:99 与 warmup-guard.ts:98）的写入位。
	// blocked_by 同时被 trg_upsert_usage_ledger 与 message_request_outbox_aiud 监视，
	// 且 fn_is_message_request_finalized 以它作为终态判据之一，因此必须可写。
	BlockedBy     *string
	BlockedReason *string
	// CacheRegressed / PrevCacheReadTokens 是「上游前缀缓存回退」的行级观测：本行的
	// cache_read_input_tokens 小于同会话上一行时为真，并留下上一行读数以便事后取证。
	// 二者由 store 在终态写时自行推导（见 UpdateDetailsIfUnfinalizedWith），调用方不必置位；
	// 显式置位时以调用方为准，推导让位。
	CacheRegressed      *bool
	PrevCacheReadTokens *int64
}

// setClause 是「列名 + 值 + 类型转换」三元组，占位符编号在拼接阶段统一分配。
// 整型一律以 int64 入参：pgx 不支持平台相关的 int，传 int 会在编码阶段报错。
//
// expr 非空时直接作为赋值右侧模板（`%s` 处替换为「占位符 + cast」），用于 `col = $n` 之外
// 的形态（如 jsonb 追加）。空串即传统形态，既有列的 SQL 形状不变。
type setClause struct {
	column string
	cast   string
	value  any
	expr   string
}

func (patch DetailsPatch) clauses() []setClause {
	clauses := make([]setClause, 0, 30)
	add := func(column string, cast string, value any) {
		clauses = append(clauses, setClause{column: column, cast: cast, value: value})
	}
	if patch.DurationMS != nil {
		add("duration_ms", "", int64(*patch.DurationMS))
	}
	if patch.StatusCode != nil {
		add("status_code", "", int64(*patch.StatusCode))
	}
	if patch.InputTokens != nil {
		add("input_tokens", "", *patch.InputTokens)
	}
	if patch.OutputTokens != nil {
		add("output_tokens", "", *patch.OutputTokens)
	}
	if patch.TTFTMS != nil {
		// 列名是历史遗留：ttfb_ms 存的是首 Token 时间（TTFT），真 TTFB 在 first_byte_ms。
		// 见 src/drizzle/schema.ts 的 messageRequest.ttftMs 注释。
		add("ttfb_ms", "", int64(*patch.TTFTMS))
	}
	if patch.FirstByteMS != nil {
		add("first_byte_ms", "", int64(*patch.FirstByteMS))
	}
	if patch.CacheCreationInputTokens != nil {
		add("cache_creation_input_tokens", "", *patch.CacheCreationInputTokens)
	}
	if patch.CacheReadInputTokens != nil {
		add("cache_read_input_tokens", "", *patch.CacheReadInputTokens)
	}
	// 观测两列紧跟在读数之后，保持「读数 → 由其派生的事实」的阅读顺序。
	if patch.PrevCacheReadTokens != nil {
		add("prev_cache_read_tokens", "", *patch.PrevCacheReadTokens)
	}
	if patch.CacheRegressed != nil {
		add("cache_regressed", "", *patch.CacheRegressed)
	}
	if patch.CacheCreation5mInputTokens != nil {
		add("cache_creation_5m_input_tokens", "", *patch.CacheCreation5mInputTokens)
	}
	if patch.CacheCreation1hInputTokens != nil {
		add("cache_creation_1h_input_tokens", "", *patch.CacheCreation1hInputTokens)
	}
	if patch.CacheTTLApplied != nil {
		add("cache_ttl_applied", "", *patch.CacheTTLApplied)
	}
	// 供应商/分组成本倍率：两列都在账本触发器的监视列表里（terminal.ledgerMonitoredColumns），
	// 随终态写落库即会驱动账本行更新。Node 在建行时就写这两列
	// （src/app/v1/_lib/proxy/message-service.ts:125-126）；Go 的 pctx.ProviderSelection
	// 不携带倍率，故改在终态写。差异、理由与残余风险见
	if patch.CostMultiplier != nil {
		add("cost_multiplier", "::numeric", *patch.CostMultiplier)
	}
	if patch.GroupCostMultiplier != nil {
		add("group_cost_multiplier", "::numeric", *patch.GroupCostMultiplier)
	}
	if patch.ProviderChain != nil {
		add("provider_chain", "::jsonb", patch.ProviderChain)
	}
	if patch.RoutingTrace != nil {
		add("routing_trace", "::jsonb", patch.RoutingTrace)
	}
	if patch.ErrorMessage != nil {
		add("error_message", "", *patch.ErrorMessage)
	}
	if patch.ErrorStack != nil {
		add("error_stack", "", *patch.ErrorStack)
	}
	if patch.ErrorCause != nil {
		add("error_cause", "", *patch.ErrorCause)
	}
	if patch.Model != nil {
		add("model", "", *patch.Model)
	}
	if patch.ActualResponseModel != nil {
		add("actual_response_model", "", *patch.ActualResponseModel)
	}
	if patch.ProviderID != nil {
		add("provider_id", "", *patch.ProviderID)
	}
	if patch.Context1mApplied != nil {
		add("context_1m_applied", "", *patch.Context1mApplied)
	}
	if patch.SwapCacheTTLApplied != nil {
		add("swap_cache_ttl_applied", "", *patch.SwapCacheTTLApplied)
	}
	if patch.SpecialSettings != nil {
		add("special_settings", "::jsonb", patch.SpecialSettings)
	}
	// SpecialSettingsAppend 走 **jsonb 追加**而不是覆盖：建行时（守卫链）写的客户端侧审计必须
	// 保留，终态只补自己那一条。覆盖会让「先写后补」的两侧互相抹除——使用记录页的思考强度列
	// 变空正是这一类问题的表现形式。COALESCE 兼顾列仍为 NULL 的行。
	if patch.SpecialSettingsAppend != nil {
		clauses = append(clauses, setClause{
			column: "special_settings",
			cast:   "::jsonb",
			value:  patch.SpecialSettingsAppend,
			expr:   `COALESCE("special_settings", '[]'::jsonb) || %s`,
		})
	}
	if patch.CacheCompatibilityKey != nil {
		add("cache_compatibility_key", "", *patch.CacheCompatibilityKey)
	}
	if patch.CacheScoreEligible != nil {
		add("cache_score_eligible", "", *patch.CacheScoreEligible)
	}
	if patch.CacheScoreExcludedReason != nil {
		add("cache_score_excluded_reason", "", *patch.CacheScoreExcludedReason)
	}
	if patch.TheoreticalCacheTokens != nil {
		add("theoretical_cache_tokens", "", *patch.TheoreticalCacheTokens)
	}
	if patch.CacheTTLBucket != nil {
		add("cache_ttl_bucket", "", *patch.CacheTTLBucket)
	}
	if patch.BlockedBy != nil {
		add("blocked_by", "", *patch.BlockedBy)
	}
	if patch.BlockedReason != nil {
		add("blocked_reason", "", *patch.BlockedReason)
	}
	return clauses
}

// UpdateDetailsIfUnfinalized 复刻 updateMessageRequestDetails 在 onlyIfUnfinalized 下的行为：
//
//	UPDATE message_request SET <patch>, updated_at = now()
//	WHERE id = $n AND status_code IS NULL
//	RETURNING id
//
// 返回 false 表示该行已有终态（或不存在），调用方据此判定是否赢得唯一终态。
// 终态写入走 writer 分道，与 TS 的 getMessageWriterDb() 一致。
func (p *Pools) UpdateDetailsIfUnfinalized(
	ctx context.Context,
	id int64,
	patch DetailsPatch,
) (bool, error) {
	pool, err := p.Writer()
	if err != nil {
		return false, err
	}
	return UpdateDetailsIfUnfinalizedWith(ctx, pool, id, patch)
}

// UpdateDetailsIfUnfinalizedWith 在指定分道上执行条件终态更新，便于调用方在事务内复用。
//
// 本次若写 cache_read_input_tokens，会顺带推导「缓存回退」观测两列（见 deriveCacheRegression）。
// 推导失败不影响终态写本身。
func UpdateDetailsIfUnfinalizedWith(
	ctx context.Context,
	pool *Pool,
	id int64,
	patch DetailsPatch,
) (bool, error) {
	patch = deriveCacheRegression(ctx, pool, id, patch)
	query, args := BuildDetailsPatchQuery(id, patch)
	var returnedID int64
	err := pool.QueryRow(ctx, query, args...).Scan(&returnedID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: 终态更新失败: %w", err)
	}
	return true, nil
}

// BuildDetailsPatchQuery 生成终态更新语句与参数。独立成函数以便单测直接断言 SQL 形状，
// 无需数据库：只有被提供的列会出现在 SET 里，且条件恒为 status_code IS NULL。
func BuildDetailsPatchQuery(id int64, patch DetailsPatch) (string, []any) {
	clauses := patch.clauses()
	args := make([]any, 0, len(clauses)+1)
	assignments := []string{`"updated_at" = now()`}
	for _, clause := range clauses {
		args = append(args, clause.value)
		rhs := fmt.Sprintf("$%d%s", len(args), clause.cast)
		if clause.expr != "" {
			rhs = strings.ReplaceAll(clause.expr, "%s", rhs)
		}
		assignments = append(assignments, fmt.Sprintf("%s = %s", quoteColumn(clause.column), rhs))
	}
	args = append(args, id)
	query := fmt.Sprintf(
		"UPDATE message_request SET %s WHERE id = $%d AND status_code IS NULL RETURNING id",
		strings.Join(assignments, ", "),
		len(args),
	)
	return query, args
}

// deriveCacheRegression 在终态写之前推导「缓存回退」观测两列。
//
// 口径（写死在此，免得各处再解释一遍）：
//   - 前一行 = 同一 session_id 中 id 小于本行、且未软删除的最近一行；
//   - 本行 cache_read < 前一行 cache_read ⇒ cache_regressed = true；
//   - 前一行不存在、本行没有 session_id、或前一行读数为 NULL ⇒ 两列都留 NULL
//     （「无从比较」与「没有回退」是两件事，故不写 false）；
//   - 相等不算回退（上游前缀缓存至少没有变小）。
//
// 只在本次确实写 cache_read_input_tokens 时推导：该列就是判定基准，没有它就没有可比的事实。
// 终态写每行只赢一次（`status_code IS NULL` 作幂等谓词），且 cache_read_input_tokens 属账本监视列、
// 不会出现在终态后的补写里，故推导每行至多发生一次，不会反复覆写。
//
// 已知上限（有意）：推导失败时静默让 NULL；store 层没有日志面，为这两列而给它加一条日志缝
// 不划算——真要观测失败，把 logger 接进 Options 再做。
func deriveCacheRegression(ctx context.Context, pool *Pool, id int64, patch DetailsPatch) DetailsPatch {
	if patch.CacheReadInputTokens == nil {
		return patch
	}
	if patch.PrevCacheReadTokens != nil || patch.CacheRegressed != nil {
		return patch
	}
	previous, err := findPrevCacheReadTokens(ctx, pool, id)
	if err != nil || previous == nil {
		return patch
	}
	regressed := *patch.CacheReadInputTokens < *previous
	patch.PrevCacheReadTokens = previous
	patch.CacheRegressed = &regressed
	return patch
}

// findPrevCacheReadTokens 取同会话上一行的 cache_read 读数；无前一行时返回 nil。
//
// 索引依据：`idx_message_request_session_id`（部分索引，`WHERE deleted_at IS NULL`）。
// 谓词里的等值 session_id 与 deleted_at IS NULL 正好命中该索引，命中集是同一会话的若干行
// （每会话几十到几百行量级），排序取一条的代价可忽略。
// 刻意不加 (session_id, id) 复合索引：它为这一列观测而生，却要在写最热的表上多付一份维护代价。
func findPrevCacheReadTokens(ctx context.Context, pool *Pool, id int64) (*int64, error) {
	const query = `
		SELECT p."cache_read_input_tokens"
		FROM "message_request" p
		WHERE p."session_id" = (SELECT c."session_id" FROM "message_request" c WHERE c."id" = $1)
		  AND p."id" < $1
		  AND p."deleted_at" IS NULL
		ORDER BY p."id" DESC
		LIMIT 1`
	var previous *int64
	err := pool.QueryRow(ctx, query, id).Scan(&previous)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: 读同会话上一行缓存读数失败: %w", err)
	}
	return previous, nil
}

// UpdateCost 复刻 updateMessageRequestCost：覆盖式写 cost_usd，无重试。
// 无效成本按 TS 语义直接跳过（不写库、不报错）。
func (p *Pools) UpdateCost(ctx context.Context, id int64, costUSD string) error {
	formatted, ok := FormatCostForStorage(costUSD)
	if !ok {
		return nil
	}
	pool, err := p.Writer()
	if err != nil {
		return err
	}
	if _, err := pool.Exec(
		ctx,
		`UPDATE message_request SET cost_usd = $1::numeric, "updated_at" = now() WHERE id = $2`,
		formatted, id,
	); err != nil {
		return fmt.Errorf("store: 更新 cost_usd 失败: %w", err)
	}
	return nil
}

// UpdateWinnerCost 复刻 updateMessageRequestWinnerCost：winner 成本与既有 hedge 输家成本
// 之和相加后覆盖写入；带 3 次重试与 50ms × 次数 的退避。
func (p *Pools) UpdateWinnerCost(
	ctx context.Context,
	id int64,
	winnerCost string,
	costBreakdown []byte,
) error {
	formatted, ok := FormatCostForStorage(winnerCost)
	if !ok {
		return nil
	}
	pool, err := p.Writer()
	if err != nil {
		return err
	}

	const maxAttempts = 3
	lastErr := error(nil)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		query := `UPDATE message_request SET
			cost_usd = $1::numeric + COALESCE((
				SELECT SUM((entry->>'costUsd')::numeric)
				FROM jsonb_array_elements(COALESCE(hedge_losers, '[]'::jsonb)) AS entry
			), 0),
			"updated_at" = now()`
		args := []any{formatted}
		if costBreakdown != nil {
			query += fmt.Sprintf(", cost_breakdown = $%d::jsonb", len(args)+1)
			args = append(args, costBreakdown)
		}
		args = append(args, id)
		query += fmt.Sprintf(" WHERE id = $%d", len(args))

		if _, err := pool.Exec(ctx, query, args...); err != nil {
			lastErr = err
			if attempt < maxAttempts-1 {
				time.Sleep(time.Duration(50*(attempt+1)) * time.Millisecond)
			}
			continue
		}
		return nil
	}
	return fmt.Errorf("store: 更新 winner 成本失败: %w", lastErr)
}

// HedgeLoserEntry 复刻 src/types/cost-breakdown.ts 的 HedgeLoserBilling。
//
// 去重键只取 providerId 与 attemptNumber（jsonb 的 @> 偏匹配语义）；costUsd 会被
// winner 的加法表达式 `SUM((entry->>'costUsd')::numeric)` 读取，因此**必须与本次
// 累加到 cost_usd 的 delta 一致**——AddHedgeLoserCost 用同一个格式化结果同时充当
// 两者，避免调用方给出不一致的两个值。
type HedgeLoserEntry struct {
	ProviderID               int64
	ProviderName             string
	AttemptNumber            int
	InputTokens              *int64
	OutputTokens             *int64
	CacheCreationInputTokens *int64
	CacheReadInputTokens     *int64
}

// AddHedgeLoserCost 复刻 addMessageRequestHedgeLoserCost：单条原子且幂等的语句——
// cost_usd 加性累加、hedge_losers 追加一项，并用
// `NOT (hedge_losers @> [{providerId, attemptNumber}])` 保证同一输家不会被计费两次，
// 即使语句已生效而客户端报错后被重试。
func (p *Pools) AddHedgeLoserCost(
	ctx context.Context,
	id int64,
	deltaCost string,
	entry HedgeLoserEntry,
) error {
	formatted, ok := FormatCostForStorage(deltaCost)
	if !ok {
		return nil
	}
	pool, err := p.Writer()
	if err != nil {
		return err
	}

	loserJSON, err := MarshalHedgeLoserEntry(entry, formatted)
	if err != nil {
		return err
	}
	guardJSON, err := MarshalHedgeLoserGuard(entry)
	if err != nil {
		return err
	}

	const maxAttempts = 3
	lastErr := error(nil)
	for attempt := 0; attempt < maxAttempts; attempt++ {
		_, err := pool.Exec(ctx, `UPDATE message_request SET
			cost_usd = COALESCE(cost_usd, 0) + $1::numeric,
			hedge_losers = COALESCE(hedge_losers, '[]'::jsonb) || $2::jsonb,
			"updated_at" = now()
			WHERE id = $3
			  AND NOT (COALESCE(hedge_losers, '[]'::jsonb) @> $4::jsonb)`,
			formatted, loserJSON, id, guardJSON,
		)
		if err == nil {
			return nil
		}
		lastErr = err
		if attempt < maxAttempts-1 {
			time.Sleep(time.Duration(50*(attempt+1)) * time.Millisecond)
		}
	}
	return fmt.Errorf("store: 累加 hedge 输家成本失败: %w", lastErr)
}

// MarshalHedgeLoserEntry 生成写入 hedge_losers 的单个元素 JSON，键名与 TS 的
// HedgeLoserBilling 一致（camelCase）。costUsd 由调用方传入的已格式化 delta 决定。
func MarshalHedgeLoserEntry(entry HedgeLoserEntry, costUSD string) ([]byte, error) {
	payload := map[string]any{
		"providerId":    entry.ProviderID,
		"providerName":  entry.ProviderName,
		"attemptNumber": entry.AttemptNumber,
		"costUsd":       costUSD,
	}
	if entry.InputTokens != nil {
		payload["inputTokens"] = *entry.InputTokens
	}
	if entry.OutputTokens != nil {
		payload["outputTokens"] = *entry.OutputTokens
	}
	if entry.CacheCreationInputTokens != nil {
		payload["cacheCreationInputTokens"] = *entry.CacheCreationInputTokens
	}
	if entry.CacheReadInputTokens != nil {
		payload["cacheReadInputTokens"] = *entry.CacheReadInputTokens
	}
	encoded, err := json.Marshal([]any{payload})
	if err != nil {
		return nil, fmt.Errorf("store: 序列化 hedge 输家条目失败: %w", err)
	}
	return encoded, nil
}

// MarshalHedgeLoserGuard 生成幂等去重键的 JSON（jsonb @> 的偏匹配语义只认这两个字段）。
func MarshalHedgeLoserGuard(entry HedgeLoserEntry) ([]byte, error) {
	payload := map[string]any{
		"providerId":    entry.ProviderID,
		"attemptNumber": entry.AttemptNumber,
	}
	encoded, err := json.Marshal([]any{payload})
	if err != nil {
		return nil, fmt.Errorf("store: 序列化 hedge 去重键失败: %w", err)
	}
	return encoded, nil
}

// FindCostUSD 读回单行的 cost_usd 文本形式，供调用方与审计比对（numeric 不以浮点往返）。
func (p *Pools) FindCostUSD(ctx context.Context, id int64) (string, error) {
	pool, err := p.Writer()
	if err != nil {
		return "", err
	}
	var cost *string
	if err := pool.QueryRow(ctx, `SELECT cost_usd::text FROM message_request WHERE id = $1`, id).Scan(&cost); err != nil {
		return "", fmt.Errorf("store: 读取 cost_usd 失败: %w", err)
	}
	if cost == nil {
		return "", nil
	}
	return *cost, nil
}

func quoteColumns(columns []string) string {
	quoted := make([]string, 0, len(columns))
	for _, column := range columns {
		quoted = append(quoted, quoteColumn(column))
	}
	return strings.Join(quoted, ", ")
}

func quoteColumn(column string) string { return `"` + column + `"` }

// numericArg 把定点字符串变成 numeric 参数；空值按 NULL 处理。
func numericArg(value *string) any {
	if value == nil || *value == "" {
		return nil
	}
	return *value
}

// jsonbArg 把原始 JSON 变成 jsonb 参数；空切片按 NULL 处理。
func jsonbArg(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}
