package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// 只读路径统一走 row_to_json：列名即 JSON 键名，因此不需要人工抄写列清单，
// 也不会因为表加列而漏读。写入路径仍用显式列列表（见 message.go），由
// schema_drift_test.go 对着 information_schema 断言列存在性。
//
// 结构体字段名与表列一一对应（json tag 即列名），只覆盖数据面实际消费的列；
// 未列出的列会被忽略，不影响读取。

// 只读视图的数值列在 row_to_json 中是 JSON number，因此一律用 json.Number：它既能接
// 受 number 也能接受 string 形式，不会因 numeric 的表示差异导致反序列化失败。
// 消费方需要十进制字符串时用 .String()，需要数值时自行按其精度转换。

// SystemSettings 是 system_settings 的单行视图。
type SystemSettings struct {
	ID                                           int64           `json:"id"`
	SiteTitle                                    string          `json:"site_title"`
	AllowGlobalUsageView                         bool            `json:"allow_global_usage_view"`
	CurrencyDisplay                              string          `json:"currency_display"`
	EnableClientVersionCheck                     bool            `json:"enable_client_version_check"`
	BillingModelSource                           string          `json:"billing_model_source"`
	CodexPriorityBillingSource                   string          `json:"codex_priority_billing_source"`
	VerboseProviderError                         bool            `json:"verbose_provider_error"`
	EnableHTTP2                                  bool            `json:"enable_http2"`
	InterceptAnthropicWarmupRequests             bool            `json:"intercept_anthropic_warmup_requests"`
	EnableResponseFixer                          bool            `json:"enable_response_fixer"`
	ResponseFixerConfig                          json.RawMessage `json:"response_fixer_config"`
	EnableThinkingSignatureRectifier             bool            `json:"enable_thinking_signature_rectifier"`
	EnableThinkingBudgetRectifier                bool            `json:"enable_thinking_budget_rectifier"`
	EnableThinkingEffortConflictRectifier        bool            `json:"enable_thinking_effort_conflict_rectifier"`
	EnableCodexSessionIDCompletion               bool            `json:"enable_codex_session_id_completion"`
	EnableClaudeMetadataUserIDInjection          bool            `json:"enable_claude_metadata_user_id_injection"`
	EnableBillingHeaderRectifier                 bool            `json:"enable_billing_header_rectifier"`
	EnableResponseInputRectifier                 bool            `json:"enable_response_input_rectifier"`
	EnableGeminiFunctionIDRectifier              bool            `json:"enable_gemini_function_id_rectifier"`
	EnableHighConcurrencyMode                    bool            `json:"enable_high_concurrency_mode"`
	IPExtractionConfig                           json.RawMessage `json:"ip_extraction_config"`
	PublicStatusWindowHours                      int             `json:"public_status_window_hours"`
	PassThroughUpstreamErrorMessage              bool            `json:"pass_through_upstream_error_message"`
	AllowNonConversationEndpointProviderFallback bool            `json:"allow_non_conversation_endpoint_provider_fallback"`
	FakeStreamingWhitelist                       json.RawMessage `json:"fake_streaming_whitelist"`
	EnableOpenAIResponsesWebsocket               bool            `json:"enable_openai_responses_websocket"`
	BillNonSuccessfulRequests                    bool            `json:"bill_non_successful_requests"`
	BillHedgeLosers                              bool            `json:"bill_hedge_losers"`
	DiscoveryEnabled                             bool            `json:"discovery_enabled"`
	DiscoveryConcurrency                         int             `json:"discovery_concurrency"`
	MaxDiscoveryRounds                           int             `json:"max_discovery_rounds"`
	DiscoverySLAMS                               int             `json:"discovery_sla_ms"`
	StickySLAMS                                  int             `json:"sticky_sla_ms"`
	RacingTotalTimeoutMS                         int             `json:"racing_total_timeout_ms"`
	StickyTimeoutCooldownMS                      int             `json:"sticky_timeout_cooldown_ms"`
	StreamGateMode                               string          `json:"stream_gate_mode"`
	// AffinityIgnoreClientSessionID 对齐 Node 的同名设置。
	AffinityIgnoreClientSessionID bool `json:"affinity_ignore_client_session_id"`
	// CacheEffectivenessEnabled 可空：Node 的语义是 `settings.cacheEffectivenessEnabled ?? env`
	// （src/lib/system-settings/proxy-runtime.ts:77-78），故 nil（未设置）必须与 false（显式关闭）区分。
	// 它门控 F3b 缓存模拟五列的写入（Node response-handler.ts:5430）。
	CacheEffectivenessEnabled *bool `json:"cache_effectiveness_enabled"`
	ReplayEnabled             *bool `json:"replay_enabled"`
	ReplayCacheTTLMinutes     int   `json:"replay_cache_ttl_minutes"`
	LegacyHedgeMaxInFlight    int   `json:"legacy_hedge_max_in_flight"`
}

// Provider 是 providers 的读取视图（覆盖选路、路由与拨号需要的列）。
type Provider struct {
	ID             int64           `json:"id"`
	Name           string          `json:"name"`
	ProviderType   string          `json:"provider_type"`
	URL            string          `json:"url"`
	Key            string          `json:"key"`
	IsEnabled      bool            `json:"is_enabled"`
	Weight         int             `json:"weight"`
	Priority       int             `json:"priority"`
	CostMultiplier json.Number     `json:"cost_multiplier"`
	GroupTag       *string         `json:"group_tag"`
	ModelRedirects json.RawMessage `json:"model_redirects"`
	AllowedModels  json.RawMessage `json:"allowed_models"`
	// CustomHeaders 是供应商级静态出站请求头（Node 的 `provider.customHeaders`）。
	//
	// 为什么保留原文而不直接反序列化成 map[string]string：列是 jsonb 且历史上无写入侧校验，
	// 脏数据里可能存在非字符串值；直接解码成 map[string]string 会**整行读取失败**。
	// Node 在施加时是**逐条跳过**非字符串值（forwarder.ts 的 `typeof value !== "string"`
	// 分支），故这里保持原文，由 DecodeCustomHeaders 按同一口径挑出可用的键值对。
	CustomHeaders json.RawMessage `json:"custom_headers"`
	// 选路四列：provider_vendor_id 决定 vendor-type 熔断与厂级端点解析，group_priorities
	// 是分组优先级覆盖（键为用户组，缺失即用 priority），protocol_conversion_enabled 决定
	// 跨协议可服务性，disable_session_reuse 是会话粘性 opt-out。选路侧读的是同一份视图，
	// 因此这四列必须留在本结构体里，否则调用方只能自持一份 SQL。
	ProviderVendorID *int64 `json:"provider_vendor_id"`
	// GroupPriorities 是分组优先级覆盖的**原文**（Node 的 `provider.group_priorities`）。
	//
	// 为什么保留原文而不直接反序列化成 map[string]int：列是 jsonb 且写入侧历史上无形状校验，
	// 脏数据里可能存在非整数（字符串 `"0"`、浮点 `0.5`、数组、标量）；直接解码成
	// map[string]int 会**整行读取失败**——而只读路径是 `row_to_json` 的逐行解码，一行失败
	// 会让整批供应商都读不出来（选路拿到 0 家候选）。故这里保持原文，由
	// DecodeGroupPriorities 逐键挑出可用的覆盖。
	GroupPriorities           json.RawMessage `json:"group_priorities"`
	ProtocolConversionEnabled bool            `json:"protocol_conversion_enabled"`
	DisableSessionReuse       bool            `json:"disable_session_reuse"`
	// 选路门槛三组列：不读它们，数据面就无法判定，Node 下线后行为会静默改变
	// （见 route/filter.go 的 Gates 与各判定函数的 Node 出处）：
	//   - active_time_start / active_time_end：供应商活动时段（Node isProviderActiveNow）；
	//   - allowed_clients / blocked_clients：供应商级客户端名单
	//     （Node pickRandomProvider 的 Step 1 → isClientAllowedDetailed）；
	//   - limit_* / total_cost_reset_at：供应商级金额限额
	//     （Node filterByLimits → checkCostLimitsWithLease + checkTotalCostLimit）。
	ActiveTimeStart                        *string         `json:"active_time_start"`
	ActiveTimeEnd                          *string         `json:"active_time_end"`
	AllowedClients                         json.RawMessage `json:"allowed_clients"`
	BlockedClients                         json.RawMessage `json:"blocked_clients"`
	Limit5hUSD                             *json.Number    `json:"limit_5h_usd"`
	Limit5hResetMode                       *string         `json:"limit_5h_reset_mode"`
	LimitDailyUSD                          *json.Number    `json:"limit_daily_usd"`
	DailyResetMode                         *string         `json:"daily_reset_mode"`
	DailyResetTime                         *string         `json:"daily_reset_time"`
	LimitWeeklyUSD                         *json.Number    `json:"limit_weekly_usd"`
	LimitMonthlyUSD                        *json.Number    `json:"limit_monthly_usd"`
	LimitTotalUSD                          *json.Number    `json:"limit_total_usd"`
	TotalCostResetAt                       *time.Time      `json:"total_cost_reset_at"`
	ProxyURL                               *string         `json:"proxy_url"`
	ProxyFallbackToDirect                  *bool           `json:"proxy_fallback_to_direct"`
	PreserveClientIP                       bool            `json:"preserve_client_ip"`
	MaxRetryAttempts                       *int            `json:"max_retry_attempts"`
	FirstByteTimeoutStreamingMS            int             `json:"first_byte_timeout_streaming_ms"`
	StreamingIdleTimeoutMS                 int             `json:"streaming_idle_timeout_ms"`
	RequestTimeoutNonStreamingMS           int             `json:"request_timeout_non_streaming_ms"`
	CircuitBreakerFailureThreshold         *int            `json:"circuit_breaker_failure_threshold"`
	CircuitBreakerOpenDuration             *int            `json:"circuit_breaker_open_duration"`
	CircuitBreakerHalfOpenSuccessThreshold *int            `json:"circuit_breaker_half_open_success_threshold"`
}

// ProviderEndpoint 是 provider_endpoints 的读取视图（供应商厂级端点）。
type ProviderEndpoint struct {
	ID                  int64   `json:"id"`
	VendorID            int64   `json:"vendor_id"`
	ProviderType        string  `json:"provider_type"`
	URL                 string  `json:"url"`
	Label               *string `json:"label"`
	SortOrder           int     `json:"sort_order"`
	IsEnabled           bool    `json:"is_enabled"`
	LastProbeOK         *bool   `json:"last_probe_ok"`
	LastProbeStatusCode *int    `json:"last_probe_status_code"`
	LastProbeLatencyMS  *int    `json:"last_probe_latency_ms"`
}

// APIKey 是 keys 的读取视图（鉴权与配额所需列）。
type APIKey struct {
	ID                      int64       `json:"id"`
	UserID                  int64       `json:"user_id"`
	Key                     string      `json:"key"`
	Name                    string      `json:"name"`
	IsEnabled               *bool       `json:"is_enabled"`
	ExpiresAt               *string     `json:"expires_at"`
	ProviderGroup           *string     `json:"provider_group"`
	Limit5hUSD              json.Number `json:"limit_5h_usd"`
	LimitDailyUSD           json.Number `json:"limit_daily_usd"`
	LimitWeeklyUSD          json.Number `json:"limit_weekly_usd"`
	LimitMonthlyUSD         json.Number `json:"limit_monthly_usd"`
	LimitTotalUSD           json.Number `json:"limit_total_usd"`
	LimitConcurrentSessions *int        `json:"limit_concurrent_sessions"`
	CacheTTLPreference      *string     `json:"cache_ttl_preference"`
}

// ModelPrice 是 model_prices 的读取视图。
type ModelPrice struct {
	ID        int64           `json:"id"`
	ModelName string          `json:"model_name"`
	PriceData json.RawMessage `json:"price_data"`
	Source    string          `json:"source"`
}

// FindSystemSettings 读回 system_settings 的单行（按 id 最小者，与 TS 侧取首行一致）。
func (p *Pools) FindSystemSettings(ctx context.Context) (*SystemSettings, error) {
	var settings SystemSettings
	if err := p.readSingleRowAs(
		ctx,
		`SELECT row_to_json(t)::text FROM (SELECT * FROM system_settings ORDER BY id ASC LIMIT 1) t`,
		&settings,
		[]any{},
	); err != nil {
		return nil, err
	}
	return &settings, nil
}

// FindEnabledProviders 读回全部启用态供应商。
func (p *Pools) FindEnabledProviders(ctx context.Context) ([]Provider, error) {
	return readRowsAs[Provider](
		ctx, p,
		`SELECT row_to_json(t)::text FROM (
			SELECT * FROM providers WHERE is_enabled = true AND deleted_at IS NULL
			ORDER BY priority ASC, id ASC
		) t`,
	)
}

// FindProviderByID 读回单个供应商（含禁用态，便于报错时说明原因）。
func (p *Pools) FindProviderByID(ctx context.Context, id int64) (*Provider, error) {
	var provider Provider
	if err := p.readSingleRowAs(
		ctx,
		`SELECT row_to_json(t)::text FROM (SELECT * FROM providers WHERE id = $1) t`,
		&provider,
		[]any{id},
	); err != nil {
		return nil, err
	}
	return &provider, nil
}

// FindEnabledProviderEndpointsByVendorAndType 复刻同名 repository 函数：按供应商厂与类型
// 取启用态端点，按 sort_order 升序。这是数据面唯一需要走端点表缓存的查询。
func (p *Pools) FindEnabledProviderEndpointsByVendorAndType(
	ctx context.Context,
	vendorID int64,
	providerType string,
) ([]ProviderEndpoint, error) {
	return readRowsAs[ProviderEndpoint](
		ctx, p,
		`SELECT row_to_json(t)::text FROM (
			SELECT * FROM provider_endpoints
			WHERE vendor_id = $1 AND provider_type = $2 AND is_enabled = true AND deleted_at IS NULL
			ORDER BY sort_order ASC, id ASC
		) t`,
		vendorID, providerType,
	)
}

// FindKeyByValue 复刻鉴权路径的密钥查询：按明文密钥精确匹配。
func (p *Pools) FindKeyByValue(ctx context.Context, key string) (*APIKey, error) {
	var record APIKey
	if err := p.readSingleRowAs(
		ctx,
		`SELECT row_to_json(t)::text FROM (SELECT * FROM keys WHERE key = $1 LIMIT 1) t`,
		&record,
		[]any{key},
	); err != nil {
		return nil, err
	}
	return &record, nil
}

// FindModelPrice 复刻价格查表：按模型名取最新一行。
func (p *Pools) FindModelPrice(ctx context.Context, modelName string) (*ModelPrice, error) {
	var price ModelPrice
	if err := p.readSingleRowAs(
		ctx,
		`SELECT row_to_json(t)::text FROM (
			SELECT * FROM model_prices WHERE model_name = $1 ORDER BY updated_at DESC NULLS LAST, id DESC LIMIT 1
		) t`,
		&price,
		[]any{modelName},
	); err != nil {
		return nil, err
	}
	return &price, nil
}

// ErrNotFound 在读只读视图时表示目标行不存在（与「查询失败」区分）。
var ErrNotFound = fmt.Errorf("store: 记录不存在")

// readSingleRowAs 读回单行。走 data 分道：/v1 请求处理器在 TS 侧由 withDataDbScope
// 包住，getDb() 因而总是取 data 分道；控制面调用方如需他道应自备查询。
func (p *Pools) readSingleRowAs(
	ctx context.Context,
	query string,
	destination any,
	args []any,
) error {
	pool, err := p.Data()
	if err != nil {
		return err
	}
	var payload string
	err = pool.QueryRow(ctx, query, args...).Scan(&payload)
	if err != nil {
		if isNoRows(err) {
			return ErrNotFound
		}
		return fmt.Errorf("store: 只读查询失败: %w", err)
	}
	if err := json.Unmarshal([]byte(payload), destination); err != nil {
		return fmt.Errorf("store: 反序列化只读行失败: %w", err)
	}
	return nil
}

// readRowsAs 依次把 row_to_json 的文本行反序列化为目标类型（分道说明同 readSingleRowAs）。
func readRowsAs[T any](ctx context.Context, p *Pools, query string, args ...any) ([]T, error) {
	pool, err := p.Data()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: 只读查询失败: %w", err)
	}
	defer rows.Close()

	results := make([]T, 0, 8)
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, fmt.Errorf("store: 读取只读行失败: %w", err)
		}
		var item T
		if err := json.Unmarshal([]byte(payload), &item); err != nil {
			return nil, fmt.Errorf("store: 反序列化只读行失败: %w", err)
		}
		results = append(results, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: 只读遍历失败: %w", err)
	}
	return results, nil
}

func isNoRows(err error) bool { return errors.Is(err, pgx.ErrNoRows) }

// DecodeCustomHeaders 把 providers.custom_headers 的原文挑成可施加的键值对。
//
// 口径对齐 Node 的 applyProviderCustomHeaders（forwarder.ts:262-273）：
//   - null / 空对象 / 非法 JSON / 非对象（数组、标量）→ nil（Node 的 `if (!customHeaders) return`）；
//   - **逐条**只保留字符串值，非字符串值跳过（Node 的 `typeof value !== "string" → continue`）——
//     不是整份作废，否则一条脏值会让其余头一起失效。
//
// 鉴权头与保留名（host / 传输层黑名单 / 内部标记头）的剥离**不在这里**：那是施加点
// forward.applyProviderCustomHeaders 的职责，与 Node 的分工一致（validator 管形状，
// apply 管保留名）。
func DecodeCustomHeaders(raw json.RawMessage) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		// 数组/标量/null 都会落到这里。脏数据视为「本供应商没有自定义头」：
		// 不阻断请求，也不猜内容。
		return nil
	}
	if len(entries) == 0 {
		return nil
	}
	out := make(map[string]string, len(entries))
	for name, value := range entries {
		// 先解成 any 再断言 string：Go 对 JSON `null` 解到 string 是**无操作且成功**，
		// 直接解 string 会把 `{"x":null}` 当成空串留下，而 Node 的 `typeof value !== "string"`
		// 会跳过它。用 any + 类型断言才与 Node 逐条等价。
		var decoded any
		if err := json.Unmarshal(value, &decoded); err != nil {
			continue
		}
		text, ok := decoded.(string)
		if !ok {
			continue
		}
		out[name] = text
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// GroupPrioritiesIssue 是 group_priorities 里一处无法采用的值（供调用方告警）。
//
// 只带定位信息（哪个键、jsonb 类型），**不带原始值**：原文是不受控输入，写进日志既刷屏，
// 也难说没有敏感内容。
type GroupPrioritiesIssue struct {
	// Key 是出问题的键；TopLevel 为真时为空（整列都不是对象，无键可言）。
	Key string
	// JSONType 是该处的 jsonb 类型名（string / number / array / boolean / object / null）。
	JSONType string
	// TopLevel 为真表示整列不是 JSON 对象，因而一条覆盖都取不出来。
	TopLevel bool
}

// DecodeGroupPriorities 把 providers.group_priorities 的原文挑成可用的分组覆盖。
//
// 口径（对齐 DecodeCustomHeaders 的取舍理由：一行的脏值不该让整批供应商读不出来）：
//   - 缺失 / SQL NULL / JSON null / 空对象 → nil（没有覆盖）；
//   - 非对象（数组、标量）→ nil，并报一条 TopLevel 问题；
//   - 对象：**逐键**取整数，坏键跳过并各报一条问题。
//
// 「值为 JSON null 的键」取 0（落在下面的整数分支：`json.Unmarshal` 把 null 解到 int 是
// 无操作且成功），而不是丢弃：这是既有语义（键存在、值与 0 同效），改它会把这行覆盖从
// 「生效」变成「不生效」，属选路行为变化，不在容错范围内。
func DecodeGroupPriorities(raw json.RawMessage) (map[string]int, []GroupPrioritiesIssue) {
	if len(raw) == 0 {
		return nil, nil
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, []GroupPrioritiesIssue{{JSONType: jsonValueTypeName(raw), TopLevel: true}}
	}
	if len(entries) == 0 {
		return nil, nil
	}
	out := make(map[string]int, len(entries))
	var issues []GroupPrioritiesIssue
	for key, value := range entries {
		// 先用 int 解：接受集与旧行为**逐字相同**（旧码就是用 map[string]int 解整份），
		// 因此整数、负数与 Go 接受的整数形 JSON 照旧收下，不因容错而改变结果。
		var number int
		if err := json.Unmarshal(value, &number); err == nil {
			out[key] = number
			continue
		}
		issues = append(issues, GroupPrioritiesIssue{Key: key, JSONType: jsonValueTypeName(value)})
	}
	if len(out) == 0 {
		out = nil
	}
	return out, issues
}

// jsonValueTypeName 取一段 JSON 原文的类型名（与 adminapi 侧的 adminJSONTypeName 同口径）。
// 只按首个非空白字符判定，不做完整校验：这里只需要给日志一个可读的类型。
func jsonValueTypeName(raw []byte) string {
	for _, b := range raw {
		switch {
		case b == ' ' || b == '\t' || b == '\n' || b == '\r':
			continue
		case b == '{':
			return "object"
		case b == '[':
			return "array"
		case b == '"':
			return "string"
		case b == 't' || b == 'f':
			return "boolean"
		case b == 'n':
			return "null"
		default:
			return "number"
		}
	}
	return "unknown"
}
