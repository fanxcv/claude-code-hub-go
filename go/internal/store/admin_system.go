package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// 本文件是管理面 system_settings 资源（GET/PUT /api/v1/system/settings 与
// GET/POST /api/admin/system-config）的 SQL 层。
//
// 与 read.go 的 SystemSettings 的分工：那份是**数据面**视图，只覆盖选路与整流用得着的列；
// 管理面的设置页要的是全表（见 types/system-config.ts 的 SystemSettings，约 57 列），
// 半张表作答等于让设置页显示错值（system.go 文件头已记过这条）。故此处另立一个全列视图，
// 读取仍沿用本包只读路径的 row_to_json 风格（列名即 JSON 键名，不需要人工抄列清单）。
//
// 列的可空性照 information_schema 实测：只有 created_at/updated_at/enable_auto_cleanup/
// cleanup_*/response_fixer_config/quota_*/timezone/ip_extraction_config/fake_streaming_whitelist/
// replay_enabled/cache_effectiveness_enabled 可空，其余列 NOT NULL，故只有这些用指针。

// AdminSystemSettings 是 system_settings 的单行全列视图。
type AdminSystemSettings struct {
	ID                           int64           `json:"id"`
	SiteTitle                    string          `json:"site_title"`
	AllowGlobalUsageView         bool            `json:"allow_global_usage_view"`
	CurrencyDisplay              string          `json:"currency_display"`
	BillingModelSource           string          `json:"billing_model_source"`
	CodexPriorityBillingSource   string          `json:"codex_priority_billing_source"`
	BillNonSuccessfulRequests    bool            `json:"bill_non_successful_requests"`
	BillHedgeLosers              bool            `json:"bill_hedge_losers"`
	LegacyHedgeMaxInFlight       int             `json:"legacy_hedge_max_in_flight"`
	Timezone                     *string         `json:"timezone"`
	EnableAutoCleanup            *bool           `json:"enable_auto_cleanup"`
	CleanupRetentionDays         *int            `json:"cleanup_retention_days"`
	CleanupSchedule              *string         `json:"cleanup_schedule"`
	CleanupBatchSize             *int            `json:"cleanup_batch_size"`
	EnableClientVersionCheck     bool            `json:"enable_client_version_check"`
	VerboseProviderError         bool            `json:"verbose_provider_error"`
	PassThroughUpstreamErrorMsg  bool            `json:"pass_through_upstream_error_message"`
	EnableHTTP2                  bool            `json:"enable_http2"`
	EnableOpenAIResponsesWS      bool            `json:"enable_openai_responses_websocket"`
	EnableHighConcurrencyMode    bool            `json:"enable_high_concurrency_mode"`
	InterceptAnthropicWarmupReqs bool            `json:"intercept_anthropic_warmup_requests"`
	EnableThinkingSignatureRect  bool            `json:"enable_thinking_signature_rectifier"`
	EnableThinkingBudgetRect     bool            `json:"enable_thinking_budget_rectifier"`
	EnableThinkingEffortConflict bool            `json:"enable_thinking_effort_conflict_rectifier"`
	EnableGeminiFunctionIDRect   bool            `json:"enable_gemini_function_id_rectifier"`
	EnableBillingHeaderRect      bool            `json:"enable_billing_header_rectifier"`
	EnableResponseInputRect      bool            `json:"enable_response_input_rectifier"`
	AllowNonConvEndpointFallback bool            `json:"allow_non_conversation_endpoint_provider_fallback"`
	FakeStreamingWhitelist       json.RawMessage `json:"fake_streaming_whitelist"`
	EnableCodexSessionIDComplete bool            `json:"enable_codex_session_id_completion"`
	EnableClaudeMetadataUserID   bool            `json:"enable_claude_metadata_user_id_injection"`
	EnableResponseFixer          bool            `json:"enable_response_fixer"`
	ResponseFixerConfig          json.RawMessage `json:"response_fixer_config"`
	QuotaDBRefreshIntervalSecs   *int            `json:"quota_db_refresh_interval_seconds"`
	QuotaLeasePercent5h          *json.Number    `json:"quota_lease_percent_5h"`
	QuotaLeasePercentDaily       *json.Number    `json:"quota_lease_percent_daily"`
	QuotaLeasePercentWeekly      *json.Number    `json:"quota_lease_percent_weekly"`
	QuotaLeasePercentMonthly     *json.Number    `json:"quota_lease_percent_monthly"`
	QuotaLeaseCapUSD             *json.Number    `json:"quota_lease_cap_usd"`
	IPExtractionConfig           json.RawMessage `json:"ip_extraction_config"`
	IPGeoLookupEnabled           bool            `json:"ip_geo_lookup_enabled"`
	PublicStatusWindowHours      int             `json:"public_status_window_hours"`
	PublicStatusAggregationMins  int             `json:"public_status_aggregation_interval_minutes"`
	StreamGateMode               string          `json:"stream_gate_mode"`
	AffinityIgnoreClientSession  bool            `json:"affinity_ignore_client_session_id"`
	ReplayEnabled                *bool           `json:"replay_enabled"`
	ReplayCacheTTLMinutes        int             `json:"replay_cache_ttl_minutes"`
	CacheEffectivenessEnabled    *bool           `json:"cache_effectiveness_enabled"`
	DiscoveryEnabled             bool            `json:"discovery_enabled"`
	DiscoveryConcurrency         int             `json:"discovery_concurrency"`
	MaxDiscoveryRounds           int             `json:"max_discovery_rounds"`
	DiscoverySLAMS               int             `json:"discovery_sla_ms"`
	StickySLAMS                  int             `json:"sticky_sla_ms"`
	RacingTotalTimeoutMS         int             `json:"racing_total_timeout_ms"`
	StickyTimeoutCooldownMS      int             `json:"sticky_timeout_cooldown_ms"`
	CreatedAt                    *time.Time      `json:"created_at"`
	UpdatedAt                    *time.Time      `json:"updated_at"`
}

// AdminSystemSettingsColumn 是设置列名的**白名单**。
//
// 列名要拼进 SQL，所以不能让调用方自由构造：枚举 + 方法内 switch 是唯一入口，未识别列一律报错。
type AdminSystemSettingsColumn string

const (
	ColSiteTitle                    AdminSystemSettingsColumn = "site_title"
	ColAllowGlobalUsageView         AdminSystemSettingsColumn = "allow_global_usage_view"
	ColCurrencyDisplay              AdminSystemSettingsColumn = "currency_display"
	ColBillingModelSource           AdminSystemSettingsColumn = "billing_model_source"
	ColCodexPriorityBillingSource   AdminSystemSettingsColumn = "codex_priority_billing_source"
	ColBillNonSuccessfulRequests    AdminSystemSettingsColumn = "bill_non_successful_requests"
	ColBillHedgeLosers              AdminSystemSettingsColumn = "bill_hedge_losers"
	ColLegacyHedgeMaxInFlight       AdminSystemSettingsColumn = "legacy_hedge_max_in_flight"
	ColTimezone                     AdminSystemSettingsColumn = "timezone"
	ColEnableAutoCleanup            AdminSystemSettingsColumn = "enable_auto_cleanup"
	ColCleanupRetentionDays         AdminSystemSettingsColumn = "cleanup_retention_days"
	ColCleanupSchedule              AdminSystemSettingsColumn = "cleanup_schedule"
	ColCleanupBatchSize             AdminSystemSettingsColumn = "cleanup_batch_size"
	ColEnableClientVersionCheck     AdminSystemSettingsColumn = "enable_client_version_check"
	ColVerboseProviderError         AdminSystemSettingsColumn = "verbose_provider_error"
	ColPassThroughUpstreamErrorMsg  AdminSystemSettingsColumn = "pass_through_upstream_error_message"
	ColEnableHTTP2                  AdminSystemSettingsColumn = "enable_http2"
	ColEnableOpenAIResponsesWS      AdminSystemSettingsColumn = "enable_openai_responses_websocket"
	ColEnableHighConcurrencyMode    AdminSystemSettingsColumn = "enable_high_concurrency_mode"
	ColInterceptAnthropicWarmupReqs AdminSystemSettingsColumn = "intercept_anthropic_warmup_requests"
	ColEnableThinkingSignatureRect  AdminSystemSettingsColumn = "enable_thinking_signature_rectifier"
	ColEnableThinkingBudgetRect     AdminSystemSettingsColumn = "enable_thinking_budget_rectifier"
	ColEnableThinkingEffortConflict AdminSystemSettingsColumn = "enable_thinking_effort_conflict_rectifier"
	ColEnableGeminiFunctionIDRect   AdminSystemSettingsColumn = "enable_gemini_function_id_rectifier"
	ColEnableBillingHeaderRect      AdminSystemSettingsColumn = "enable_billing_header_rectifier"
	ColEnableResponseInputRect      AdminSystemSettingsColumn = "enable_response_input_rectifier"
	ColAllowNonConvEndpointFallback AdminSystemSettingsColumn = "allow_non_conversation_endpoint_provider_fallback"
	ColFakeStreamingWhitelist       AdminSystemSettingsColumn = "fake_streaming_whitelist"
	ColEnableCodexSessionIDComplete AdminSystemSettingsColumn = "enable_codex_session_id_completion"
	ColEnableClaudeMetadataUserID   AdminSystemSettingsColumn = "enable_claude_metadata_user_id_injection"
	ColEnableResponseFixer          AdminSystemSettingsColumn = "enable_response_fixer"
	ColResponseFixerConfig          AdminSystemSettingsColumn = "response_fixer_config"
	ColQuotaDBRefreshIntervalSecs   AdminSystemSettingsColumn = "quota_db_refresh_interval_seconds"
	ColQuotaLeasePercent5h          AdminSystemSettingsColumn = "quota_lease_percent_5h"
	ColQuotaLeasePercentDaily       AdminSystemSettingsColumn = "quota_lease_percent_daily"
	ColQuotaLeasePercentWeekly      AdminSystemSettingsColumn = "quota_lease_percent_weekly"
	ColQuotaLeasePercentMonthly     AdminSystemSettingsColumn = "quota_lease_percent_monthly"
	ColQuotaLeaseCapUSD             AdminSystemSettingsColumn = "quota_lease_cap_usd"
	ColIPExtractionConfig           AdminSystemSettingsColumn = "ip_extraction_config"
	ColIPGeoLookupEnabled           AdminSystemSettingsColumn = "ip_geo_lookup_enabled"
	ColPublicStatusWindowHours      AdminSystemSettingsColumn = "public_status_window_hours"
	ColPublicStatusAggregationMins  AdminSystemSettingsColumn = "public_status_aggregation_interval_minutes"
	ColStreamGateMode               AdminSystemSettingsColumn = "stream_gate_mode"
	ColAffinityIgnoreClientSession  AdminSystemSettingsColumn = "affinity_ignore_client_session_id"
	ColReplayEnabled                AdminSystemSettingsColumn = "replay_enabled"
	ColReplayCacheTTLMinutes        AdminSystemSettingsColumn = "replay_cache_ttl_minutes"
	ColCacheEffectivenessEnabled    AdminSystemSettingsColumn = "cache_effectiveness_enabled"
	ColDiscoveryEnabled             AdminSystemSettingsColumn = "discovery_enabled"
	ColDiscoveryConcurrency         AdminSystemSettingsColumn = "discovery_concurrency"
	ColMaxDiscoveryRounds           AdminSystemSettingsColumn = "max_discovery_rounds"
	ColDiscoverySLAMS               AdminSystemSettingsColumn = "discovery_sla_ms"
	ColStickySLAMS                  AdminSystemSettingsColumn = "sticky_sla_ms"
	ColRacingTotalTimeoutMS         AdminSystemSettingsColumn = "racing_total_timeout_ms"
	ColStickyTimeoutCooldownMS      AdminSystemSettingsColumn = "sticky_timeout_cooldown_ms"
)

// IsJSONColumn 判断该列是否为 jsonb（拼 SQL 时要补 ::jsonb 转型）。
func (c AdminSystemSettingsColumn) IsJSONColumn() bool {
	switch c {
	case ColFakeStreamingWhitelist, ColResponseFixerConfig, ColIPExtractionConfig:
		return true
	}
	return false
}

// known 判断列名是否在白名单内。
func (c AdminSystemSettingsColumn) known() bool {
	switch c {
	case ColSiteTitle, ColAllowGlobalUsageView, ColCurrencyDisplay, ColBillingModelSource,
		ColCodexPriorityBillingSource, ColBillNonSuccessfulRequests, ColBillHedgeLosers,
		ColLegacyHedgeMaxInFlight, ColTimezone, ColEnableAutoCleanup, ColCleanupRetentionDays,
		ColCleanupSchedule, ColCleanupBatchSize, ColEnableClientVersionCheck, ColVerboseProviderError,
		ColPassThroughUpstreamErrorMsg, ColEnableHTTP2, ColEnableOpenAIResponsesWS,
		ColEnableHighConcurrencyMode, ColInterceptAnthropicWarmupReqs,
		ColEnableThinkingSignatureRect, ColEnableThinkingBudgetRect, ColEnableThinkingEffortConflict,
		ColEnableGeminiFunctionIDRect, ColEnableBillingHeaderRect, ColEnableResponseInputRect,
		ColAllowNonConvEndpointFallback, ColFakeStreamingWhitelist, ColEnableCodexSessionIDComplete,
		ColEnableClaudeMetadataUserID, ColEnableResponseFixer, ColResponseFixerConfig,
		ColQuotaDBRefreshIntervalSecs, ColQuotaLeasePercent5h, ColQuotaLeasePercentDaily,
		ColQuotaLeasePercentWeekly, ColQuotaLeasePercentMonthly, ColQuotaLeaseCapUSD,
		ColIPExtractionConfig, ColIPGeoLookupEnabled, ColPublicStatusWindowHours,
		ColPublicStatusAggregationMins, ColStreamGateMode, ColAffinityIgnoreClientSession,
		ColReplayEnabled, ColReplayCacheTTLMinutes, ColCacheEffectivenessEnabled, ColDiscoveryEnabled,
		ColDiscoveryConcurrency, ColMaxDiscoveryRounds, ColDiscoverySLAMS, ColStickySLAMS,
		ColRacingTotalTimeoutMS, ColStickyTimeoutCooldownMS:
		return true
	}
	return false
}

// AdminSystemSettingsPatch 是一次部分更新：键为列名白名单，值为该列的新值（nil 即写 SQL NULL）。
//
// 用「列 -> 值」而不是逐字段三态结构体：可写列有 54 个，而「未出现」与「出现为 null」
// 在 map 里天然可区分（键不存在 = 未出现），逐字段写 Present 标志只会把同一件事抄 54 遍。
type AdminSystemSettingsPatch struct {
	Updates map[AdminSystemSettingsColumn]any
}

// Columns 返回本次更新涉及的列名（升序，便于测试与日志稳定）。
func (p AdminSystemSettingsPatch) Columns() []string {
	names := make([]string, 0, len(p.Updates))
	for column := range p.Updates {
		names = append(names, string(column))
	}
	sort.Strings(names)
	return names
}

const adminSystemSettingsSingleRow = `SELECT row_to_json(t)::text FROM (
	SELECT * FROM system_settings ORDER BY id ASC LIMIT 1
) t`

// ErrAdminSystemSettingsMissing 表示 system_settings 一行都没有且插入也未能补上。
var ErrAdminSystemSettingsMissing = errors.New("store: system_settings 没有可用行")

// FindAdminSystemSettings 读回 system_settings 的全列单行（按 id 最小者，与 TS 侧取首行一致）。
//
// 无行时返回 ErrAdminSystemSettingsMissing（不是 nil,nil）：调用方必须显式决定「补默认行还是
// 报错」，静默的 nil 会让设置页显示一份凭空的默认值。
func (p *Pools) FindAdminSystemSettings(ctx context.Context) (*AdminSystemSettings, error) {
	var settings AdminSystemSettings
	if err := p.readSingleRowAs(ctx, adminSystemSettingsSingleRow, &settings, []any{}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAdminSystemSettingsMissing
		}
		return nil, err
	}
	return &settings, nil
}

// EnsureAdminSystemSettings 读回单行；一行都没有时先插默认行再读。
//
// 与 Node 的 getSystemSettings 同义（repository/system-config.ts:545-600）：设置页与数据面都
// 假定「总有一行」，缺行时由读取方补默认行，而不是让调用方到处判空。插入用 on conflict do nothing，
// 并发下最多白插一次。
//
// 默认值与 Node 的 insert 逐字段一致（注意 allowGlobalUsageView 在**插入**时是 false，
// 而 toSystemSettings 的**读取**缺省是 true——两者不同源，这里照抄插入侧）。
func (p *Pools) EnsureAdminSystemSettings(ctx context.Context) (*AdminSystemSettings, error) {
	settings, err := p.FindAdminSystemSettings(ctx)
	if err == nil {
		return settings, nil
	}
	if !errors.Is(err, ErrAdminSystemSettingsMissing) {
		return nil, err
	}
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}
	if _, err := pool.Exec(ctx, `INSERT INTO system_settings (
			site_title, allow_global_usage_view, currency_display, billing_model_source,
			codex_priority_billing_source, pass_through_upstream_error_message,
			allow_non_conversation_endpoint_provider_fallback, enable_high_concurrency_mode,
			public_status_window_hours, public_status_aggregation_interval_minutes,
			replay_cache_ttl_minutes
		) VALUES ($1, false, 'USD', 'original', 'requested', true, true, false, 24, 5, 30)
		ON CONFLICT DO NOTHING`,
		DefaultSiteTitle,
	); err != nil {
		return nil, fmt.Errorf("store: 初始化 system_settings 失败: %w", err)
	}
	settings, err = p.FindAdminSystemSettings(ctx)
	if err != nil {
		return nil, ErrAdminSystemSettingsMissing
	}
	return settings, nil
}

// DefaultSiteTitle 逐字取自 UI 的 DEFAULT_SITE_TITLE（src/lib/site-title.ts:1）。
const DefaultSiteTitle = "CC Hub Go"

// UpdateAdminSystemSettings 按白名单部分更新该行，并把更新后的全列行读回。
//
// id 为空表示「更新当前的首行」（调用方通常先用 EnsureAdminSystemSettings 拿到行再原样传回）。
// updated_at 恒写 now()，与 Node 的 updateSystemSettings 一致（它总是把 updatedAt 放进更新对象）。
func (p *Pools) UpdateAdminSystemSettings(
	ctx context.Context,
	id int64,
	patch AdminSystemSettingsPatch,
) (*AdminSystemSettings, error) {
	if len(patch.Updates) == 0 {
		// 空更新在 Node 里等于「只碰 updatedAt」。这里直接读回当前行，不写库——
		// 空 PUT 不改任何语义，写一次 updated_at 只会让「有没有人改过设置」失去意义。
		return p.FindAdminSystemSettings(ctx)
	}
	pool, err := p.Control()
	if err != nil {
		return nil, err
	}

	// 列名先排序再拼：同一份 patch 生成的 SQL 稳定，便于按语句做慢查询归因。
	columns := make([]string, 0, len(patch.Updates))
	for column := range patch.Updates {
		if !column.known() {
			return nil, fmt.Errorf("store: 未知的 system_settings 列: %q", string(column))
		}
		columns = append(columns, string(column))
	}
	sort.Strings(columns)

	assignments := make([]string, 0, len(columns)+1)
	args := make([]any, 0, len(columns)+2)
	for _, column := range columns {
		value := patch.Updates[AdminSystemSettingsColumn(column)]
		args = append(args, adminSystemSettingsArg(value))
		placeholder := "$" + strconv.Itoa(len(args))
		if AdminSystemSettingsColumn(column).IsJSONColumn() {
			placeholder += "::jsonb"
		}
		assignments = append(assignments, column+" = "+placeholder)
	}
	assignments = append(assignments, "updated_at = now()")
	args = append(args, id)

	query := `UPDATE system_settings SET ` + strings.Join(assignments, ", ") + ` WHERE id = $` +
		strconv.Itoa(len(args)) + ` RETURNING row_to_json(system_settings)::text`
	var raw string
	if err := pool.QueryRow(ctx, query, args...).Scan(&raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAdminSystemSettingsMissing
		}
		return nil, fmt.Errorf("store: 更新 system_settings 失败: %w", err)
	}
	var settings AdminSystemSettings
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return nil, fmt.Errorf("store: 解析 system_settings 失败: %w", err)
	}
	return &settings, nil
}

// adminSystemSettingsArg 归一化入参：json.RawMessage 走 nullableJSON（空与 null 都写 SQL NULL），
// 其余值原样交给 pgx（bool/int/float64/string 由它自己编码）。
func adminSystemSettingsArg(value any) any {
	if raw, ok := value.(json.RawMessage); ok {
		return nullableJSON(raw)
	}
	return value
}
