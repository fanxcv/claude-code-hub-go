package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// 本文件是 providers 表的管理面读写面（P0，UI 的 4 个供应商页面依赖它）。
//
// 唯一真源：
//   - 路由与状态码：src/app/api/v1/resources/providers/router.ts
//   - 响应形状：src/lib/api/v1/schemas/providers.ts 的 ProviderSummarySchema
//   - 读取实现：src/actions/providers.ts 的 getProviders（findAllProvidersFresh 派生）
//   - 写实现：src/repository/provider.ts
//
// 分道：读走 control（管理面其余资源同此），写走 writer。
//
// 列别名取 Node 的 camelCase 直出，故本结构体同时充当响应体形状来源（与
// admin_error_rules.go 同一手法）；handler 只做脱敏、隐藏类型过滤与统计合并。

// AdminProvider 是 providers 表在管理面上的一行。
//
// 数值列（limit_*_usd、cost_multiplier）在 PG 里是 numeric：Node 的 drizzle 配置把 numeric
// 映射成 number，故这里统一 `::float8` 转成 JSON number。若按 text 出，响应里会变成字符串，
// 违反 ProviderSummarySchema 的 z.number()。
type AdminProvider struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	Description *string `json:"description"`
	URL         string  `json:"url"`
	// Key 绝不出现在响应里：list/get 只用它算 maskedKey（见 adminapi 层）。
	Key                      string          `json:"key"`
	ProviderVendorID         *int64          `json:"providerVendorId"`
	IsEnabled                bool            `json:"isEnabled"`
	Weight                   int             `json:"weight"`
	Priority                 int             `json:"priority"`
	GroupPriorities          json.RawMessage `json:"groupPriorities"`
	CostMultiplier           *float64        `json:"costMultiplier"`
	GroupTag                 *string         `json:"groupTag"`
	ProviderType             string          `json:"providerType"`
	PreserveClientIP         bool            `json:"preserveClientIp"`
	DisableSessionReuse      bool            `json:"disableSessionReuse"`
	ModelRedirects           json.RawMessage `json:"modelRedirects"`
	AllowedModels            json.RawMessage `json:"allowedModels"`
	AllowedClients           []string        `json:"allowedClients"`
	BlockedClients           []string        `json:"blockedClients"`
	ActiveTimeStart          *string         `json:"activeTimeStart"`
	ActiveTimeEnd            *string         `json:"activeTimeEnd"`
	MCPPassthroughType       string          `json:"mcpPassthroughType"`
	MCPPassthroughURL        *string         `json:"mcpPassthroughUrl"`
	ProtocolConversionOn     bool            `json:"protocolConversionEnabled"`
	Limit5hUSD               *float64        `json:"limit5hUsd"`
	Limit5hResetMode         string          `json:"limit5hResetMode"`
	LimitDailyUSD            *float64        `json:"limitDailyUsd"`
	DailyResetMode           string          `json:"dailyResetMode"`
	DailyResetTime           string          `json:"dailyResetTime"`
	LimitWeeklyUSD           *float64        `json:"limitWeeklyUsd"`
	LimitMonthlyUSD          *float64        `json:"limitMonthlyUsd"`
	LimitTotalUSD            *float64        `json:"limitTotalUsd"`
	TotalCostResetAt         *string         `json:"totalCostResetAt"`
	LimitConcurrentSessions  *int            `json:"limitConcurrentSessions"`
	MaxRetryAttempts         *int            `json:"maxRetryAttempts"`
	CircuitFailureThreshold  *int            `json:"circuitBreakerFailureThreshold"`
	CircuitOpenDuration      *int            `json:"circuitBreakerOpenDuration"`
	CircuitHalfOpenThreshold *int            `json:"circuitBreakerHalfOpenSuccessThreshold"`
	// 等待阶梯：递增时长（ms）与最大次数。可空 = 不启用（窗口恒为 openDuration）。
	CircuitReleaseIncrement  *int            `json:"circuitBreakerReleaseIncrement"`
	CircuitMaxOpenCount      *int            `json:"circuitBreakerMaxOpenCount"`
	ProxyURL                 *string         `json:"proxyUrl"`
	ProxyFallbackToDirect    bool            `json:"proxyFallbackToDirect"`
	CustomHeaders            json.RawMessage `json:"customHeaders"`
	FirstByteTimeoutStreamMs int             `json:"firstByteTimeoutStreamingMs"`
	StreamingIdleTimeoutMs   int             `json:"streamingIdleTimeoutMs"`
	RequestTimeoutNonStream  int             `json:"requestTimeoutNonStreamingMs"`
	WebsiteURL               *string         `json:"websiteUrl"`
	FaviconURL               *string         `json:"faviconUrl"`
	CacheTTLPreference       *string         `json:"cacheTtlPreference"`
	SwapCacheTTLBilling      bool            `json:"swapCacheTtlBilling"`
	Context1mPreference      *string         `json:"context1mPreference"`

	CodexReasoningEffort    *string         `json:"codexReasoningEffortPreference"`
	CodexReasoningSummary   *string         `json:"codexReasoningSummaryPreference"`
	CodexTextVerbosity      *string         `json:"codexTextVerbosityPreference"`
	CodexParallelToolCalls  *string         `json:"codexParallelToolCallsPreference"`
	CodexImageGeneration    *string         `json:"codexImageGenerationPreference"`
	CodexServiceTier        *string         `json:"codexServiceTierPreference"`
	CodexMaxTokens          *string         `json:"codexMaxTokensPreference"`
	AnthropicMaxTokens      *string         `json:"anthropicMaxTokensPreference"`
	AnthropicThinkingBudget *string         `json:"anthropicThinkingBudgetPreference"`
	AnthropicAdaptive       json.RawMessage `json:"anthropicAdaptiveThinking"`
	OpenAIMaxTokens         *string         `json:"openaiMaxTokensPreference"`
	GeminiGoogleSearch      *string         `json:"geminiGoogleSearchPreference"`

	// 低速降级（逐渠道开关，默认全关）。
	SlowRateMonitorEnabled bool `json:"slowRateMonitorEnabled"`
	// SlowRateWindowMinutes 是判定滑窗（默认 30 **分钟**）；SlowRateBaselineWindowDays 是基线主窗
	// （默认 3 **天**）。两列尺度不同，不可混用。列名与单位已对齐（旧列 *_seconds 见 0134 迁移）。
	//
	// **REST 字段名沿旧**（用户 2026-09-22 裁决）：`slowRateWindowSeconds` 等 JSON 名保持不变
	// 以免断外部调用方，但**值存的是新单位**（分钟/天/0-1 小数）——看名字的人须留意这一点，
	// 故在本注释与 OpenAPI 描述里都写明。
	SlowRateWindowMinutes      *int `json:"slowRateWindowSeconds"`
	SlowRateBaselineWindowDays *int `json:"slowRateBaselineWindowSeconds"`
	// SlowRateMinSamples 是基线样本下限；SlowRateTriggerCount 是触发阈值。两者语义不同。
	SlowRateMinSamples   *int `json:"slowRateMinSamples"`
	SlowRateTriggerCount *int `json:"slowRateTriggerCount"`
	// SlowRateRatio 是低速系数（0-1 小数，默认 0.3）；JSON 名沿旧 `slowRateRatioPerMille`。
	SlowRateRatio       *float64 `json:"slowRateRatioPerMille"`
	SlowRatePenaltyStep *int     `json:"slowRatePenaltyStep"`
	SlowRatePenaltyMax  *int     `json:"slowRatePenaltyMax"`
	// SlowRateRecoveryRequests 是恢复策略阈值（默认 10）。
	SlowRateRecoveryRequests *int `json:"slowRateRecoveryRequests"`
	// SlowRateProbeAfterFirstByteSeconds 是首字后停滞探测阈值 T（秒）。
	//
	// NULL = 未覆盖（不是「关闭」）：监控开关打开时取 slowrate.DefaultProbeAfterFirstByteSeconds
	// （30）；真正的关闭由 SlowRateMonitorEnabled 承载。
	SlowRateProbeAfterFirstByteSeconds *int `json:"slowRateProbeAfterFirstByteSeconds"`
	// SlowRatePrecommitEnabled 是提交前速率闸（默认全关）；NULL = 未覆盖 ⇒ false。
	SlowRatePrecommitEnabled *bool `json:"slowRatePrecommitEnabled"`
	// SlowRatePrecommitMinBytesPerSecond 是速率阈值（语义字节/秒）；NULL = 未覆盖 ⇒ 由基线推导。
	SlowRatePrecommitMinBytesPerSecond *int `json:"slowRatePrecommitMinBytesPerSecond"`

	TPM *int `json:"tpm"`
	RPM *int `json:"rpm"`
	RPD *int `json:"rpd"`
	CC  *int `json:"cc"`

	CreatedAt *string `json:"createdAt"`
	UpdatedAt *string `json:"updatedAt"`
}

// adminProviderColumns 是管理面的完整读取投影。
//
// `key` 也读（算 maskedKey 用），json 标签是 "key"——**handler 必须把它从响应里剔除**，
// 见 adminapi 层的 providerSummaryPayload。
const adminProviderColumns = `
	id, name, description, url, key,
	provider_vendor_id AS "providerVendorId",
	is_enabled AS "isEnabled", weight, priority,
	group_priorities AS "groupPriorities",
	cost_multiplier::float8 AS "costMultiplier",
	group_tag AS "groupTag", provider_type AS "providerType",
	preserve_client_ip AS "preserveClientIp",
	disable_session_reuse AS "disableSessionReuse",
	model_redirects AS "modelRedirects", allowed_models AS "allowedModels",
	COALESCE(allowed_clients, '{}') AS "allowedClients",
	COALESCE(blocked_clients, '{}') AS "blockedClients",
	active_time_start AS "activeTimeStart", active_time_end AS "activeTimeEnd",
	mcp_passthrough_type AS "mcpPassthroughType", mcp_passthrough_url AS "mcpPassthroughUrl",
	protocol_conversion_enabled AS "protocolConversionEnabled",
	limit_5h_usd::float8 AS "limit5hUsd", limit_5h_reset_mode AS "limit5hResetMode",
	limit_daily_usd::float8 AS "limitDailyUsd", daily_reset_mode AS "dailyResetMode",
	daily_reset_time AS "dailyResetTime",
	limit_weekly_usd::float8 AS "limitWeeklyUsd",
	limit_monthly_usd::float8 AS "limitMonthlyUsd",
	limit_total_usd::float8 AS "limitTotalUsd",
	to_char(total_cost_reset_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS "totalCostResetAt",
	limit_concurrent_sessions AS "limitConcurrentSessions",
	max_retry_attempts AS "maxRetryAttempts",
	circuit_breaker_failure_threshold AS "circuitBreakerFailureThreshold",
	circuit_breaker_open_duration AS "circuitBreakerOpenDuration",
	circuit_breaker_half_open_success_threshold AS "circuitBreakerHalfOpenSuccessThreshold",
	circuit_breaker_release_increment AS "circuitBreakerReleaseIncrement",
	circuit_breaker_max_open_count AS "circuitBreakerMaxOpenCount",
	proxy_url AS "proxyUrl", proxy_fallback_to_direct AS "proxyFallbackToDirect",
	custom_headers AS "customHeaders",
	first_byte_timeout_streaming_ms AS "firstByteTimeoutStreamingMs",
	streaming_idle_timeout_ms AS "streamingIdleTimeoutMs",
	request_timeout_non_streaming_ms AS "requestTimeoutNonStreamingMs",
	website_url AS "websiteUrl", favicon_url AS "faviconUrl",
	cache_ttl_preference AS "cacheTtlPreference",
	swap_cache_ttl_billing AS "swapCacheTtlBilling",
	context_1m_preference AS "context1mPreference",
	codex_reasoning_effort_preference AS "codexReasoningEffortPreference",
	codex_reasoning_summary_preference AS "codexReasoningSummaryPreference",
	codex_text_verbosity_preference AS "codexTextVerbosityPreference",
	codex_parallel_tool_calls_preference AS "codexParallelToolCallsPreference",
	codex_image_generation_preference AS "codexImageGenerationPreference",
	codex_service_tier_preference AS "codexServiceTierPreference",
	codex_max_tokens_preference AS "codexMaxTokensPreference",
	anthropic_max_tokens_preference AS "anthropicMaxTokensPreference",
	anthropic_thinking_budget_preference AS "anthropicThinkingBudgetPreference",
	anthropic_adaptive_thinking AS "anthropicAdaptiveThinking",
	openai_max_tokens_preference AS "openaiMaxTokensPreference",
	gemini_google_search_preference AS "geminiGoogleSearchPreference",
	slow_rate_monitor_enabled AS "slowRateMonitorEnabled",
	slow_rate_window_minutes AS "slowRateWindowSeconds",
	slow_rate_baseline_window_days AS "slowRateBaselineWindowSeconds",
	slow_rate_min_samples AS "slowRateMinSamples",
	slow_rate_trigger_count AS "slowRateTriggerCount",
	slow_rate_ratio AS "slowRateRatioPerMille",
	slow_rate_penalty_step AS "slowRatePenaltyStep",
	slow_rate_penalty_max AS "slowRatePenaltyMax",
	slow_rate_recovery_requests AS "slowRateRecoveryRequests",
	slow_rate_probe_after_first_byte_seconds AS "slowRateProbeAfterFirstByteSeconds",
	slow_rate_precommit_enabled AS "slowRatePrecommitEnabled",
	slow_rate_precommit_min_bytes_per_second AS "slowRatePrecommitMinBytesPerSecond",
	tpm, rpm, rpd, cc,
	to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS "createdAt",
	to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS "updatedAt"`

// AdminListProviders 读回全部未软删供应商。
//
// 排序与 Node 的 findAllProvidersFresh 一致（priority ASC, id ASC：选路口径）；
// 管理面列表页自己再按需要排，故此顺序是稳定的基线而非展示契约。
//
// 隐藏类型（claude-auth / gemini-cli）**不在这里过滤**：dashboard-compat 请求要看它们，
// 故过滤放在 handler（复刻 loadVisibleProviders 的 isDashboardCompatRequest 分支）。
func (p *Pools) AdminListProviders(ctx context.Context) ([]AdminProvider, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `SELECT row_to_json(t)::text FROM (SELECT ` + adminProviderColumns +
		` FROM providers WHERE deleted_at IS NULL ORDER BY priority ASC, id ASC) t`
	return adminProviderRows(ctx, pool, query)
}

// AdminGetProviderByID 读回单个未软删供应商；不存在时 ErrNotFound。
func (p *Pools) AdminGetProviderByID(ctx context.Context, id int64) (*AdminProvider, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `SELECT row_to_json(t)::text FROM (SELECT ` + adminProviderColumns +
		` FROM providers WHERE id = $1 AND deleted_at IS NULL) t`
	var payload string
	if err := pool.QueryRow(ctx, query, id).Scan(&payload); err != nil {
		if isNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: 读取供应商失败: %w", err)
	}
	provider, err := decodeAdminProvider(payload)
	if err != nil {
		return nil, err
	}
	return &provider, nil
}

// adminProviderRows 用 control 分道读多行（row_to_json 文本行）。
func adminProviderRows(ctx context.Context, pool *Pool, query string, args ...any) ([]AdminProvider, error) {
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 查询供应商失败: %w", err)
	}
	defer rows.Close()

	providers := make([]AdminProvider, 0, 16)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("store: 读取供应商行失败: %w", err)
		}
		provider, err := decodeAdminProvider(payload)
		if err != nil {
			return nil, err
		}
		providers = append(providers, provider)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历供应商失败: %w", err)
	}
	return providers, nil
}

// decodeAdminProvider 反序列化一行。
func decodeAdminProvider(payload string) (AdminProvider, error) {
	var provider AdminProvider
	if err := json.Unmarshal([]byte(payload), &provider); err != nil {
		return AdminProvider{}, fmt.Errorf("store: 解析供应商行失败: %w", err)
	}
	return provider, nil
}

// AdminRevealProviderKey 取明文密钥（GET /providers/{id}/key:reveal）。
//
// 只取未软删行；不存在时 ErrNotFound。
func (p *Pools) AdminRevealProviderKey(ctx context.Context, id int64) (string, error) {
	pool, err := p.Control()
	if err != nil {
		return "", err
	}
	var key string
	err = pool.QueryRow(ctx,
		`SELECT key FROM providers WHERE id = $1 AND deleted_at IS NULL`, id).Scan(&key)
	if err != nil {
		if isNoRows(err) {
			return "", ErrNotFound
		}
		return "", fmt.Errorf("store: 读取供应商密钥失败: %w", err)
	}
	return key, nil
}

// AdminResetProviderTotalCost 复刻 resetProviderTotalCostResetAt：把总额度计费窗口重置到现在。
//
// 返回 false 表示目标不存在。
func (p *Pools) AdminResetProviderTotalCost(ctx context.Context, id int64) (bool, error) {
	pool, err := p.Writer()
	if err != nil {
		return false, err
	}
	tag, err := pool.Exec(ctx,
		`UPDATE providers SET total_cost_reset_at = now(), updated_at = now()
		 WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return false, fmt.Errorf("store: 重置供应商总额度失败: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// AdminProviderBatchPatch 是批量更新的有效补丁（只含**被显式给出**的字段）。
//
// 与 Node 的 ProviderBatchUpdateFieldsSchema 一一对应：nil 表示该字段未给，故不写。
type AdminProviderBatchPatch struct {
	IsEnabled                    *bool
	Priority                     *int
	Weight                       *float64
	CostMultiplier               *float64
	GroupTag                     NullableString
	ModelRedirects               json.RawMessage
	AllowedModels                json.RawMessage
	AllowedClients               []string
	BlockedClients               []string
	Limit5hUSD                   NullableFloat
	Limit5hResetMode             *string
	LimitDailyUSD                NullableFloat
	DailyResetMode               *string
	DailyResetTime               *string
	CodexImageGeneration         NullableString
	CodexServiceTier             NullableString
	AnthropicThinkingBudget      NullableString
	AnthropicAdaptiveThinking    json.RawMessage
	HasGroupTag                  bool
	HasModelRedirects            bool
	HasAllowedModels             bool
	HasAllowedClients            bool
	HasBlockedClients            bool
	HasLimit5hUSD                bool
	HasLimitDailyUSD             bool
	HasCodexImageGeneration      bool
	HasCodexServiceTier          bool
	HasAnthropicThinkingBudget   bool
	HasAnthropicAdaptiveThinking bool
}

// NullableString 表达「显式给了 null」与「给了值」两种可空列写法。
type NullableString struct {
	Set   bool
	Value *string
}

// NullableFloat 同上，用于金额列。
type NullableFloat struct {
	Set   bool
	Value *float64
}

// Apply 把补丁写成一条 UPDATE（只包含被给出的列）。
//
// 为什么拼 SQL 而不是固定列：Node 的批量更新是「只改给出的字段」，用固定列会把未给出的字段
// 覆盖成 NULL（静默数据损坏）。列名来自本文件的白名单常量，值全部走参数占位。
func (p *Pools) AdminApplyProviderBatchPatch(
	ctx context.Context,
	patch AdminProviderBatchPatch,
	ids []int64,
) (int64, error) {
	assignments := make([]string, 0, 16)
	args := make([]any, 0, 20)

	assign := func(column string, value any) {
		args = append(args, value)
		assignments = append(assignments, fmt.Sprintf("%s = $%d", column, len(args)))
	}
	if patch.IsEnabled != nil {
		assign("is_enabled", *patch.IsEnabled)
	}
	if patch.Priority != nil {
		assign("priority", *patch.Priority)
	}
	if patch.Weight != nil {
		// weight 是 integer 列：Node 的 zod 只要求 number（min 0），落库时 drizzle 会截断小数。
		assign("weight", int(*patch.Weight))
	}
	if patch.CostMultiplier != nil {
		assign("cost_multiplier", *patch.CostMultiplier)
	}
	if patch.HasGroupTag {
		assign("group_tag", patch.GroupTag.Value)
	}
	if patch.HasModelRedirects {
		assign("model_redirects", rawOrNil(patch.ModelRedirects))
	}
	if patch.HasAllowedModels {
		assign("allowed_models", rawOrNil(patch.AllowedModels))
	}
	if patch.HasAllowedClients {
		assign("allowed_clients", patch.AllowedClients)
	}
	if patch.HasBlockedClients {
		assign("blocked_clients", patch.BlockedClients)
	}
	if patch.HasLimit5hUSD {
		assign("limit_5h_usd", patch.Limit5hUSD.Value)
	}
	if patch.Limit5hResetMode != nil {
		assign("limit_5h_reset_mode", *patch.Limit5hResetMode)
	}
	if patch.HasLimitDailyUSD {
		assign("limit_daily_usd", patch.LimitDailyUSD.Value)
	}
	if patch.DailyResetMode != nil {
		assign("daily_reset_mode", *patch.DailyResetMode)
	}
	if patch.DailyResetTime != nil {
		assign("daily_reset_time", *patch.DailyResetTime)
	}
	if patch.HasCodexImageGeneration {
		assign("codex_image_generation_preference", patch.CodexImageGeneration.Value)
	}
	if patch.HasCodexServiceTier {
		assign("codex_service_tier_preference", patch.CodexServiceTier.Value)
	}
	if patch.HasAnthropicThinkingBudget {
		assign("anthropic_thinking_budget_preference", patch.AnthropicThinkingBudget.Value)
	}
	if patch.HasAnthropicAdaptiveThinking {
		assign("anthropic_adaptive_thinking", rawOrNil(patch.AnthropicAdaptiveThinking))
	}
	if len(assignments) == 0 {
		// 空补丁：Node 侧同样是一次「更新 0 个字段」的 no-op，返回影响行数 0。
		return 0, nil
	}
	assignments = append(assignments, "updated_at = now()")

	args = append(args, ids)
	query := fmt.Sprintf(
		`UPDATE providers SET %s WHERE id = ANY($%d) AND deleted_at IS NULL`,
		strings.Join(assignments, ", "), len(args),
	)

	pool, err := p.Writer()
	if err != nil {
		return 0, err
	}
	tag, err := pool.Exec(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("store: 批量更新供应商失败: %w", err)
	}
	return tag.RowsAffected(), nil
}

// AdminProviderStatisticsRow 是 getProviderStatistics 的一行（Node 的 ProviderStatisticsRow）。
type AdminProviderStatisticsRow struct {
	ID            int64   `json:"id"`
	TodayCost     string  `json:"today_cost"`
	TodayCalls    int64   `json:"today_calls"`
	LastCallTime  *string `json:"last_call_time"`
	LastCallModel *string `json:"last_call_model"`
}

// AdminProviderStatistics 复刻 getProviderStatistics 的 SQL（repository/provider.ts:2342-2427）。
//
// 逐字搬运的部分：CTE 三段（bounds / provider_stats / latest_call）、`blocked_by IS NULL` 与
// `is_replay = false` 两个过滤、今日窗口用**系统时区**的日界、最近一次调用只看 7 天窗口、
// 以及 numeric 成本以文本形式返回（Node 侧同样保持 numeric 的字符串表示）。
//
// 差异：Node 侧有 20 秒的进程内缓存与 in-flight 合并（providerStatisticsCache），Go 侧每次现查
// ——现查不会给出陈旧数字，代价是多一次聚合（该查询走 usage_ledger 的既有索引）。
func (p *Pools) AdminProviderStatistics(ctx context.Context, timezone string) ([]AdminProviderStatisticsRow, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	const query = `
		WITH bounds AS (
		  SELECT
		    (DATE_TRUNC('day', CURRENT_TIMESTAMP AT TIME ZONE $1) AT TIME ZONE $1) AS today_start,
		    ((DATE_TRUNC('day', CURRENT_TIMESTAMP AT TIME ZONE $1) + INTERVAL '1 day') AT TIME ZONE $1) AS tomorrow_start,
		    ((DATE_TRUNC('day', CURRENT_TIMESTAMP AT TIME ZONE $1) - INTERVAL '7 days') AT TIME ZONE $1) AS last7_start
		),
		provider_stats AS (
		  SELECT final_provider_id,
		    COALESCE(SUM(cost_usd), 0)::text AS today_cost,
		    COUNT(*)::bigint AS today_calls
		  FROM usage_ledger
		  WHERE blocked_by IS NULL AND is_replay = false
		    AND created_at >= (SELECT today_start FROM bounds)
		    AND created_at < (SELECT tomorrow_start FROM bounds)
		  GROUP BY final_provider_id
		),
		latest_call AS (
		  SELECT DISTINCT ON (final_provider_id)
		    final_provider_id,
		    created_at AS last_call_time,
		    model AS last_call_model
		  FROM usage_ledger
		  WHERE blocked_by IS NULL AND is_replay = false
		    AND created_at >= (SELECT last7_start FROM bounds)
		  ORDER BY final_provider_id, created_at DESC, id DESC
		)
		SELECT
		  p.id,
		  COALESCE(ps.today_cost, '0') AS today_cost,
		  COALESCE(ps.today_calls, 0) AS today_calls,
		  to_char(lc.last_call_time AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS last_call_time,
		  lc.last_call_model
		FROM providers p
		LEFT JOIN provider_stats ps ON p.id = ps.final_provider_id
		LEFT JOIN latest_call lc ON p.id = lc.final_provider_id
		WHERE p.deleted_at IS NULL
		ORDER BY p.id ASC`
	rows, err := pool.Query(ctx, query, timezone)
	if err != nil {
		return nil, fmt.Errorf("store: 查询供应商统计失败: %w", err)
	}
	defer rows.Close()

	statistics := make([]AdminProviderStatisticsRow, 0, 16)
	for rows.Next() {
		var row AdminProviderStatisticsRow
		if err := rows.Scan(&row.ID, &row.TodayCost, &row.TodayCalls, &row.LastCallTime, &row.LastCallModel); err != nil {
			return nil, fmt.Errorf("store: 读取供应商统计行失败: %w", err)
		}
		statistics = append(statistics, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历供应商统计失败: %w", err)
	}
	return statistics, nil
}

// AdminProviderPriorityChange 是一条优先级变更（自动排序用）。
type AdminProviderPriorityChange struct {
	ID       int64
	Priority int
}

// AdminApplyProviderPriorities 复刻 updateProviderPrioritiesBatch：一条 UPDATE 写完所有变更行。
//
// 空变更集不发查询（Node 侧在 changes.length === 0 时也不调 repository）。
func (p *Pools) AdminApplyProviderPriorities(
	ctx context.Context,
	changes []AdminProviderPriorityChange,
) (int64, error) {
	if len(changes) == 0 {
		return 0, nil
	}
	pool, err := p.Writer()
	if err != nil {
		return 0, err
	}
	ids := make([]int64, 0, len(changes))
	priorities := make([]int32, 0, len(changes))
	for _, change := range changes {
		ids = append(ids, change.ID)
		priorities = append(priorities, int32(change.Priority))
	}
	tag, err := pool.Exec(ctx, `
		UPDATE providers p SET priority = data.priority, updated_at = now()
		FROM (SELECT unnest($1::bigint[]) AS id, unnest($2::int[]) AS priority) AS data
		WHERE p.id = data.id AND p.deleted_at IS NULL`, ids, priorities)
	if err != nil {
		return 0, fmt.Errorf("store: 批量写供应商优先级失败: %w", err)
	}
	return tag.RowsAffected(), nil
}

// rawOrNil 把 json.RawMessage 转成可写参数：空字节写 NULL。
func rawOrNil(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return []byte(raw)
}

// AdminProviderGroup 是 provider_groups 的一行 + 引用计数（Node 的 ProviderGroupWithCount）。
type AdminProviderGroup struct {
	ID             int64   `json:"id"`
	Name           string  `json:"name"`
	CostMultiplier float64 `json:"costMultiplier"`
	Description    *string `json:"description"`
	ProviderCount  int64   `json:"providerCount"`
	CreatedAt      *string `json:"createdAt"`
	UpdatedAt      *string `json:"updatedAt"`
}

// adminProviderGroupColumns 与 Node 的 ProviderGroupSchema 对齐。
const adminProviderGroupColumns = `
	id, name, cost_multiplier::float8 AS "costMultiplier", description,
	(SELECT count(*) FROM providers p
	  WHERE p.group_tag = provider_groups.name AND p.deleted_at IS NULL) AS "providerCount",
	to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS "createdAt",
	to_char(updated_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"') AS "updatedAt"`

// AdminListProviderGroups 读回全部分组（含引用计数），按名升序。
//
// Node 的 getProviderGroups 走 bootstrapProviderGroupsFromProviders：先按 providers.group_tag
// 自愈补登记缺失分组，再返回。自愈那步是 Node 侧的既有行为，Go 侧**不做**（不做隐式写入），
// 故此差异会影响「providers 里出现过但 provider_groups 里还没有的行」——见报告的白名单说明。
func (p *Pools) AdminListProviderGroups(ctx context.Context) ([]AdminProviderGroup, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `SELECT row_to_json(t)::text FROM (SELECT ` + adminProviderGroupColumns +
		` FROM provider_groups ORDER BY name ASC) t`
	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("store: 查询供应商分组失败: %w", err)
	}
	defer rows.Close()

	groups := make([]AdminProviderGroup, 0, 8)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("store: 读取供应商分组行失败: %w", err)
		}
		var group AdminProviderGroup
		if err := json.Unmarshal([]byte(payload), &group); err != nil {
			return nil, fmt.Errorf("store: 解析供应商分组行失败: %w", err)
		}
		groups = append(groups, group)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历供应商分组失败: %w", err)
	}
	return groups, nil
}

// AdminAllProviderGroupTags 返回全部未软删供应商的原始 group_tag（**不去重**）。
//
// 与 AdminDistinctProviderGroups 的差别只有「要不要 DISTINCT」：Node 的
// getProviderGroupsWithCount（src/actions/providers.ts:511-538）按 providers 逐行展开分组后累加计数，
// 同一分组出现在多个供应商上要各算一次，DISTINCT 会把这层计数压掉。
func (p *Pools) AdminAllProviderGroupTags(ctx context.Context) ([]*string, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx,
		`SELECT group_tag FROM providers WHERE deleted_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("store: 查询供应商分组失败: %w", err)
	}
	defer rows.Close()

	tags := make([]*string, 0, 64)
	for rows.Next() {
		var tag *string
		if err := rows.Scan(&tag); err != nil {
			return nil, fmt.Errorf("store: 读取供应商分组失败: %w", err)
		}
		tags = append(tags, tag)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 遍历供应商分组失败: %w", err)
	}
	return tags, nil
}

// AdminGetProviderGroupByID 读单个分组；不存在时 ErrNotFound。
func (p *Pools) AdminGetProviderGroupByID(ctx context.Context, id int64) (*AdminProviderGroup, error) {
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	query := `SELECT row_to_json(t)::text FROM (SELECT ` + adminProviderGroupColumns +
		` FROM provider_groups WHERE id = $1) t`
	var payload string
	if err := pool.QueryRow(ctx, query, id).Scan(&payload); err != nil {
		if isNoRows(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: 读取供应商分组失败: %w", err)
	}
	var group AdminProviderGroup
	if err := json.Unmarshal([]byte(payload), &group); err != nil {
		return nil, fmt.Errorf("store: 解析供应商分组失败: %w", err)
	}
	return &group, nil
}

// AdminProviderGroupNameExists 复刻 findProviderGroupByName 的存在性判定（重名拒绝用）。
func (p *Pools) AdminProviderGroupNameExists(ctx context.Context, name string) (bool, error) {
	pool, err := p.Control()
	if err != nil {
		return false, err
	}
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM provider_groups WHERE name = $1)`, name).Scan(&exists); err != nil {
		return false, fmt.Errorf("store: 判定供应商分组重名失败: %w", err)
	}
	return exists, nil
}

// AdminCountProvidersUsingGroup 复刻 countProvidersUsingGroup。
func (p *Pools) AdminCountProvidersUsingGroup(ctx context.Context, name string) (int64, error) {
	pool, err := p.Control()
	if err != nil {
		return 0, err
	}
	var count int64
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM providers WHERE group_tag = $1 AND deleted_at IS NULL`,
		name).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: 统计分组引用失败: %w", err)
	}
	return count, nil
}

// AdminCreateProviderGroup 复刻 createProviderGroup。
//
// costMultiplier 为 nil 时写 1.0（Node 侧 `input.costMultiplier` 未给由 zod 交给 drizzle
// 默认值 '1.0'，两者一致）。
func (p *Pools) AdminCreateProviderGroup(
	ctx context.Context,
	name string,
	costMultiplier *float64,
	description *string,
) (AdminProviderGroup, error) {
	pool, err := p.Writer()
	if err != nil {
		return AdminProviderGroup{}, err
	}
	var id int64
	err = pool.QueryRow(ctx,
		`INSERT INTO provider_groups (name, cost_multiplier, description)
		 VALUES ($1, COALESCE($2, 1.0), $3) RETURNING id`,
		name, costMultiplier, description).Scan(&id)
	if err != nil {
		return AdminProviderGroup{}, fmt.Errorf("store: 创建供应商分组失败: %w", err)
	}
	created, err := p.AdminGetProviderGroupByID(ctx, id)
	if err != nil {
		return AdminProviderGroup{}, err
	}
	return *created, nil
}

// AdminUpdateProviderGroup 复刻 updateProviderGroup：只写给出的列。
func (p *Pools) AdminUpdateProviderGroup(
	ctx context.Context,
	id int64,
	costMultiplier *float64,
	description NullableString,
) (*AdminProviderGroup, error) {
	assignments := make([]string, 0, 3)
	args := make([]any, 0, 4)
	if costMultiplier != nil {
		args = append(args, *costMultiplier)
		assignments = append(assignments, fmt.Sprintf("cost_multiplier = $%d", len(args)))
	}
	if description.Set {
		args = append(args, description.Value)
		assignments = append(assignments, fmt.Sprintf("description = $%d", len(args)))
	}
	if len(assignments) == 0 {
		return p.AdminGetProviderGroupByID(ctx, id)
	}
	assignments = append(assignments, "updated_at = now()")
	args = append(args, id)

	pool, err := p.Writer()
	if err != nil {
		return nil, err
	}
	tag, err := pool.Exec(ctx, fmt.Sprintf(
		`UPDATE provider_groups SET %s WHERE id = $%d`,
		strings.Join(assignments, ", "), len(args),
	), args...)
	if err != nil {
		return nil, fmt.Errorf("store: 更新供应商分组失败: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// 可能是「值未变」也可能是「不存在」：以一次读确认（Node 侧 drizzle 同样返回行）。
		return p.AdminGetProviderGroupByID(ctx, id)
	}
	return p.AdminGetProviderGroupByID(ctx, id)
}

// AdminDeleteProviderGroup 复刻 deleteProviderGroup（物理删除）。
func (p *Pools) AdminDeleteProviderGroup(ctx context.Context, id int64) (bool, error) {
	pool, err := p.Writer()
	if err != nil {
		return false, err
	}
	tag, err := pool.Exec(ctx, `DELETE FROM provider_groups WHERE id = $1`, id)
	if err != nil {
		return false, fmt.Errorf("store: 删除供应商分组失败: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}
