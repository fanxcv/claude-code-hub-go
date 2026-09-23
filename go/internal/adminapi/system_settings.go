package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 /api/v1/system/settings 的 GET 与 PUT（Node 侧 system 资源里此前未实现的两条）。
//
// 为什么值得一个专门的文件：这两条的响应体是 Node 的 SystemSettings 投影（约 60 个字段），
// 由 toSystemSettings（src/repository/_shared/transformers.ts:246）逐字段归一——缺省值、
// 枚举回退、replay TTL 与竞速上限的钳制、fake 白名单的裁剪。投影错一个字段，设置页就会
// 显示错值或把错值写回；故投影单独成一个函数，且用**Node 真产物**做字段级对拍
// （system_settings_golden_test.go 的 golden 由 bun 跑 Node 的 toSystemSettings 生成）。
//
// PUT 的语义（照 actions/system-config.ts:saveSystemSettings）：
//
//	解析（strict：未知键 400）→ 竞速窗口不变量 → 写库 → 失效广播 → public-status 重发 → 审计
//
// 两条原先的未移植项已补齐（见 Deps.PublicStatusPublisher / Deps.DashboardCaches）：
//
//  1. public-status 投影重发：已接 `internal/pubstatus`（版本化键 + 版本指针 CAS）。
//     仍**未移植**的是聚合侧的 background rebuild（rebuild-hint/scheduler/rebuild-worker），
//     故发布成功也只能回 `PUBLIC_STATUS_BACKGROUND_REFRESH_PENDING`——公开状态页的数据不会
//     自己刷新，运维要看到这个差异。
//  2. 时区变更时的 overview/statistics/leaderboard 三族缓存失效：走 Deps.DashboardCaches。
//     未装配时按 Node 的 catch 分支语义记 warn，不影响请求结果。
const (
	discoverySettingsInvalidErrorCode = "DISCOVERY_SETTINGS_INVALID"
	discoveryWindowInvalidErrorCode   = "DISCOVERY_WINDOW_INVALID"
	replayCacheTTLInvalidErrorCode    = "REPLAY_CACHE_TTL_INVALID"
	legacyHedgeInvalidErrorCode       = "LEGACY_HEDGE_MAX_IN_FLIGHT_INVALID"
	publicStatusPublishFailedCode     = "PUBLIC_STATUS_PROJECTION_PUBLISH_FAILED"
)

// Node 的出厂默认（src/lib/config/system-settings-cache.ts 的 DEFAULT_SETTINGS 与
// transformers.ts 的局部默认）。
const (
	defaultSiteTitle                 = store.DefaultSiteTitle
	defaultReplayCacheTTLMinutes     = 30
	replayCacheTTLMinutesMin         = 5
	replayCacheTTLMinutesMax         = 120
	defaultLegacyHedgeMaxInFlight    = 2
	defaultResponseFixerMaxJSONDepth = 200
	defaultResponseFixerMaxFixSize   = 1024 * 1024
	defaultDiscoveryConcurrency      = 2
	defaultMaxDiscoveryRounds        = 2
	defaultDiscoverySLAMS            = 10_000
	defaultStickySLAMS               = 20_000
	defaultRacingTotalTimeoutMS      = 60_000
	defaultStickyTimeoutCooldownMS   = 300_000
	defaultPublicStatusWindowHours   = 24
	defaultPublicStatusAggregation   = 5
	defaultQuotaLeasePercent         = 0.05
	defaultQuotaDBRefreshSeconds     = 10
	defaultCleanupRetentionDays      = 30
	defaultCleanupSchedule           = "0 2 * * *"
	defaultCleanupBatchSize          = 10000
	maxPublicStatusRangeHours        = 168
)

// discoveryFieldLimits 逐字取自 src/lib/validation/discovery-settings.ts 的 DISCOVERY_FIELD_LIMITS。
var discoveryFieldLimits = map[string][2]int{
	"discoveryConcurrency":    {2, 32},
	"maxDiscoveryRounds":      {1, 32},
	"discoverySlaMs":          {1, 300_000},
	"stickySlaMs":             {1, 600_000},
	"racingTotalTimeoutMs":    {1, 3_600_000},
	"stickyTimeoutCooldownMs": {1, 86_400_000},
}

// publicStatusIntervals 取自 src/lib/public-status/constants.ts 的 PUBLIC_STATUS_INTERVAL_OPTIONS。
var publicStatusIntervals = map[int]bool{5: true, 15: true, 30: true, 60: true}

// currencyCodes 取自 src/lib/utils/currency.ts 的 CURRENCY_CONFIG 键集。
var currencyCodes = map[string]bool{
	"USD": true, "CNY": true, "EUR": true, "JPY": true, "GBP": true,
	"HKD": true, "TWD": true, "KRW": true, "SGD": true,
}

// SystemSettingsBody 是 system_settings 的**对外投影**，字段顺序与键名逐字对齐 Node 的
// toSystemSettings 返回对象（顺序不影响 JSON 语义，但保持同序便于与 golden 逐行比对）。
type SystemSettingsBody struct {
	ID                           int64                         `json:"id"`
	SiteTitle                    string                        `json:"siteTitle"`
	AllowGlobalUsageView         bool                          `json:"allowGlobalUsageView"`
	CurrencyDisplay              string                        `json:"currencyDisplay"`
	BillingModelSource           string                        `json:"billingModelSource"`
	CodexPriorityBillingSource   string                        `json:"codexPriorityBillingSource"`
	BillNonSuccessfulRequests    bool                          `json:"billNonSuccessfulRequests"`
	BillHedgeLosers              bool                          `json:"billHedgeLosers"`
	LegacyHedgeMaxInFlight       int                           `json:"legacyHedgeMaxInFlight"`
	Timezone                     *string                       `json:"timezone"`
	EnableAutoCleanup            bool                          `json:"enableAutoCleanup"`
	CleanupRetentionDays         int                           `json:"cleanupRetentionDays"`
	CleanupSchedule              string                        `json:"cleanupSchedule"`
	CleanupBatchSize             int                           `json:"cleanupBatchSize"`
	EnableClientVersionCheck     bool                          `json:"enableClientVersionCheck"`
	VerboseProviderError         bool                          `json:"verboseProviderError"`
	PassThroughUpstreamErrorMsg  bool                          `json:"passThroughUpstreamErrorMessage"`
	EnableHTTP2                  bool                          `json:"enableHttp2"`
	EnableOpenAIResponsesWS      bool                          `json:"enableOpenaiResponsesWebsocket"`
	EnableHighConcurrencyMode    bool                          `json:"enableHighConcurrencyMode"`
	InterceptAnthropicWarmupReqs bool                          `json:"interceptAnthropicWarmupRequests"`
	EnableThinkingSignatureRect  bool                          `json:"enableThinkingSignatureRectifier"`
	EnableThinkingBudgetRect     bool                          `json:"enableThinkingBudgetRectifier"`
	EnableThinkingEffortConflict bool                          `json:"enableThinkingEffortConflictRectifier"`
	EnableGeminiFunctionIDRect   bool                          `json:"enableGeminiFunctionIdRectifier"`
	EnableBillingHeaderRect      bool                          `json:"enableBillingHeaderRectifier"`
	EnableResponseInputRect      bool                          `json:"enableResponseInputRectifier"`
	AllowNonConvEndpointFallback bool                          `json:"allowNonConversationEndpointProviderFallback"`
	FakeStreamingWhitelist       []FakeStreamingWhitelistEntry `json:"fakeStreamingWhitelist"`
	EnableCodexSessionIDComplete bool                          `json:"enableCodexSessionIdCompletion"`
	EnableClaudeMetadataUserID   bool                          `json:"enableClaudeMetadataUserIdInjection"`
	EnableResponseFixer          bool                          `json:"enableResponseFixer"`
	ResponseFixerConfig          ResponseFixerConfig           `json:"responseFixerConfig"`
	QuotaDBRefreshIntervalSecs   int                           `json:"quotaDbRefreshIntervalSeconds"`
	QuotaLeasePercent5h          float64                       `json:"quotaLeasePercent5h"`
	QuotaLeasePercentDaily       float64                       `json:"quotaLeasePercentDaily"`
	QuotaLeasePercentWeekly      float64                       `json:"quotaLeasePercentWeekly"`
	QuotaLeasePercentMonthly     float64                       `json:"quotaLeasePercentMonthly"`
	QuotaLeaseCapUSD             *float64                      `json:"quotaLeaseCapUsd"`
	PublicStatusWindowHours      int                           `json:"publicStatusWindowHours"`
	PublicStatusAggregationMins  int                           `json:"publicStatusAggregationIntervalMinutes"`
	DiscoveryEnabled             bool                          `json:"discoveryEnabled"`
	DiscoveryConcurrency         int                           `json:"discoveryConcurrency"`
	MaxDiscoveryRounds           int                           `json:"maxDiscoveryRounds"`
	DiscoverySLAMS               int                           `json:"discoverySlaMs"`
	StickySLAMS                  int                           `json:"stickySlaMs"`
	RacingTotalTimeoutMS         int                           `json:"racingTotalTimeoutMs"`
	StickyTimeoutCooldownMS      int                           `json:"stickyTimeoutCooldownMs"`
	IPExtractionConfig           json.RawMessage               `json:"ipExtractionConfig"`
	IPGeoLookupEnabled           bool                          `json:"ipGeoLookupEnabled"`
	StreamGateMode               string                        `json:"streamGateMode"`
	AffinityIgnoreClientSession  bool                          `json:"affinityIgnoreClientSessionId"`
	AffinityEnabled              bool                          `json:"affinityEnabled"`
	// ProviderLiveStatsEnabled 是「供应商实时并发统计」的全局开关（默认 false）。
	//
	// 加它的理由与读侧同：默认关 = 该特性不存在（写侧零 Redis 命令、前端不轮询），
	// 故存量部署不打开开关就与加它之前完全同行为。
	ProviderLiveStatsEnabled  bool   `json:"providerLiveStatsEnabled"`
	ReplayEnabled             *bool  `json:"replayEnabled"`
	ReplayCacheTTLMinutes     int    `json:"replayCacheTtlMinutes"`
	CacheEffectivenessEnabled *bool  `json:"cacheEffectivenessEnabled"`
	CreatedAt                 string `json:"createdAt"`
	UpdatedAt                 string `json:"updatedAt"`
}

// FakeStreamingWhitelistEntry 逐字对应 Node 的 FakeStreamingWhitelistEntry。
type FakeStreamingWhitelistEntry struct {
	Model     string   `json:"model"`
	GroupTags []string `json:"groupTags"`
}

// ResponseFixerConfig 逐字对应 Node 的 ResponseFixerConfig。
type ResponseFixerConfig struct {
	FixTruncatedJSON bool `json:"fixTruncatedJson"`
	FixSSEFormat     bool `json:"fixSseFormat"`
	FixEncoding      bool `json:"fixEncoding"`
	MaxJSONDepth     int  `json:"maxJsonDepth"`
	MaxFixSize       int  `json:"maxFixSize"`
}

// SystemSettingsUpdateResponse 是 PUT 的响应：投影 + 可空的投影告警码（Node 恒带该键）。
type SystemSettingsUpdateResponse struct {
	SystemSettingsBody
	PublicStatusProjectionWarningCode *string `json:"publicStatusProjectionWarningCode"`
}

// buildSystemSettingsBody 复刻 toSystemSettings 的逐字段归一。
func buildSystemSettingsBody(row *store.AdminSystemSettings, now time.Time) SystemSettingsBody {
	return SystemSettingsBody{
		ID:                           row.ID,
		SiteTitle:                    row.SiteTitle,
		AllowGlobalUsageView:         row.AllowGlobalUsageView,
		CurrencyDisplay:              row.CurrencyDisplay,
		BillingModelSource:           row.BillingModelSource,
		CodexPriorityBillingSource:   settingsEnumDefault(row.CodexPriorityBillingSource, "requested", "requested", "actual"),
		BillNonSuccessfulRequests:    row.BillNonSuccessfulRequests,
		BillHedgeLosers:              row.BillHedgeLosers,
		LegacyHedgeMaxInFlight:       settingsClampInt(row.LegacyHedgeMaxInFlight, 1, 4, defaultLegacyHedgeMaxInFlight),
		Timezone:                     row.Timezone,
		EnableAutoCleanup:            settingsBool(row.EnableAutoCleanup, false),
		CleanupRetentionDays:         settingsInt(row.CleanupRetentionDays, defaultCleanupRetentionDays),
		CleanupSchedule:              settingsString(row.CleanupSchedule, defaultCleanupSchedule),
		CleanupBatchSize:             settingsInt(row.CleanupBatchSize, defaultCleanupBatchSize),
		EnableClientVersionCheck:     row.EnableClientVersionCheck,
		VerboseProviderError:         row.VerboseProviderError,
		PassThroughUpstreamErrorMsg:  row.PassThroughUpstreamErrorMsg,
		EnableHTTP2:                  row.EnableHTTP2,
		EnableOpenAIResponsesWS:      row.EnableOpenAIResponsesWS,
		EnableHighConcurrencyMode:    row.EnableHighConcurrencyMode,
		InterceptAnthropicWarmupReqs: row.InterceptAnthropicWarmupReqs,
		EnableThinkingSignatureRect:  row.EnableThinkingSignatureRect,
		EnableThinkingBudgetRect:     row.EnableThinkingBudgetRect,
		EnableThinkingEffortConflict: row.EnableThinkingEffortConflict,
		EnableGeminiFunctionIDRect:   row.EnableGeminiFunctionIDRect,
		EnableBillingHeaderRect:      row.EnableBillingHeaderRect,
		EnableResponseInputRect:      row.EnableResponseInputRect,
		AllowNonConvEndpointFallback: row.AllowNonConvEndpointFallback,
		FakeStreamingWhitelist:       normalizeFakeStreamingWhitelist(row.FakeStreamingWhitelist),
		EnableCodexSessionIDComplete: row.EnableCodexSessionIDComplete,
		EnableClaudeMetadataUserID:   row.EnableClaudeMetadataUserID,
		EnableResponseFixer:          row.EnableResponseFixer,
		ResponseFixerConfig:          normalizeResponseFixerConfig(row.ResponseFixerConfig),
		QuotaDBRefreshIntervalSecs:   settingsInt(row.QuotaDBRefreshIntervalSecs, defaultQuotaDBRefreshSeconds),
		QuotaLeasePercent5h:          quotaLeasePercent(row.QuotaLeasePercent5h),
		QuotaLeasePercentDaily:       quotaLeasePercent(row.QuotaLeasePercentDaily),
		QuotaLeasePercentWeekly:      quotaLeasePercent(row.QuotaLeasePercentWeekly),
		QuotaLeasePercentMonthly:     quotaLeasePercent(row.QuotaLeasePercentMonthly),
		QuotaLeaseCapUSD:             quotaLeaseCap(row.QuotaLeaseCapUSD),
		PublicStatusWindowHours:      settingsDefaultInt(row.PublicStatusWindowHours, defaultPublicStatusWindowHours),
		PublicStatusAggregationMins:  settingsDefaultInt(row.PublicStatusAggregationMins, defaultPublicStatusAggregation),
		DiscoveryEnabled:             row.DiscoveryEnabled,
		DiscoveryConcurrency:         settingsDefaultInt(row.DiscoveryConcurrency, defaultDiscoveryConcurrency),
		MaxDiscoveryRounds:           settingsDefaultInt(row.MaxDiscoveryRounds, defaultMaxDiscoveryRounds),
		DiscoverySLAMS:               settingsDefaultInt(row.DiscoverySLAMS, defaultDiscoverySLAMS),
		StickySLAMS:                  settingsDefaultInt(row.StickySLAMS, defaultStickySLAMS),
		RacingTotalTimeoutMS:         settingsDefaultInt(row.RacingTotalTimeoutMS, defaultRacingTotalTimeoutMS),
		StickyTimeoutCooldownMS:      settingsDefaultInt(row.StickyTimeoutCooldownMS, defaultStickyTimeoutCooldownMS),
		IPExtractionConfig:           normalizeJSONOrNull(row.IPExtractionConfig),
		IPGeoLookupEnabled:           row.IPGeoLookupEnabled,
		StreamGateMode:               settingsEnumDefault(row.StreamGateMode, "enforce", "off", "enforce"),
		AffinityIgnoreClientSession:  row.AffinityIgnoreClientSession,
		AffinityEnabled:              row.AffinityEnabled,
		ProviderLiveStatsEnabled:     row.ProviderLiveStatsEnabled,
		ReplayEnabled:                row.ReplayEnabled,
		ReplayCacheTTLMinutes: settingsClampInt(
			row.ReplayCacheTTLMinutes, replayCacheTTLMinutesMin, replayCacheTTLMinutesMax,
			defaultReplayCacheTTLMinutes,
		),
		CacheEffectivenessEnabled: row.CacheEffectivenessEnabled,
		CreatedAt:                 formatJSDate(settingsTime(row.CreatedAt, now)),
		UpdatedAt:                 formatJSDate(settingsTime(row.UpdatedAt, now)),
	}
}

// settingsEnumDefault 保留白名单内的取值，否则用**显式给定**的回退值。
//
// 为什么回退值必须显式：Node 的三元链各不相同——codexPriorityBillingSource 回退 "requested"（链首），
// streamGateMode 回退 "enforce"（链尾），billingModelSource 回退 "original"。取「链首」当通则是错的。
func settingsEnumDefault(value, fallback string, allowed ...string) string {
	for _, candidate := range allowed {
		if value == candidate {
			return value
		}
	}
	return fallback
}

// settingsBool 解引用可空布尔；缺值用默认。
func settingsBool(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

// settingsInt / settingsDefaultInt 解引用可空整数；缺值用默认。两者同名不同参是刻意分开的：
// 前者收 NULL 列（Node 的 `?? default`），后者收 NOT NULL 列（Node 同样 `?? default`，
// 因为 0 在 Node 里是 falsy 之外的真值——故这里只在列值为 0 时按 Node 的行为保留 0）。
func settingsInt(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

func settingsDefaultInt(value int, fallback int) int {
	return value
}

// settingsString 解引用可空字符串；缺值用默认。
func settingsString(value *string, fallback string) string {
	if value == nil || *value == "" {
		return fallback
	}
	return *value
}

// settingsClampInt 复刻 Node 的「整数且落在区间内，否则默认」三段判断。
func settingsClampInt(value, min, max, fallback int) int {
	if value >= min && value <= max {
		return value
	}
	return fallback
}

// settingsTime 复刻 `dbSettings?.createdAt ? new Date(...) : new Date()`。
func settingsTime(value *time.Time, now time.Time) time.Time {
	if value == nil {
		return now
	}
	return *value
}

// formatJSDate 按 JS 的 Date#toJSON 形状输出（UTC、恒三位毫秒、Z 结尾）。
//
// 为什么不用 time.RFC3339Nano：Go 会省略零毫秒（"…05Z"），而 JS 恒输出 "…05.000Z"，
// 前端按同一形状解析/展示日期，形状不同就是可见的分叉。
func formatJSDate(value time.Time) string {
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// normalizeJSONOrNull 把 jsonb 原始值透传；空值（含 SQL NULL）输出 JSON null。
func normalizeJSONOrNull(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage("null")
	}
	return raw
}

// normalizeResponseFixerConfig 复刻 Node 的 `{...defaultResponseFixerConfig, ...row}`。
func normalizeResponseFixerConfig(raw json.RawMessage) ResponseFixerConfig {
	config := ResponseFixerConfig{
		FixTruncatedJSON: true,
		FixSSEFormat:     true,
		FixEncoding:      true,
		MaxJSONDepth:     defaultResponseFixerMaxJSONDepth,
		MaxFixSize:       defaultResponseFixerMaxFixSize,
	}
	if len(raw) == 0 || string(raw) == "null" {
		return config
	}
	var stored map[string]json.RawMessage
	if err := json.Unmarshal(raw, &stored); err != nil {
		return config
	}
	if value, ok := settingsJSONBool(stored["fixTruncatedJson"]); ok {
		config.FixTruncatedJSON = value
	}
	if value, ok := settingsJSONBool(stored["fixSseFormat"]); ok {
		config.FixSSEFormat = value
	}
	if value, ok := settingsJSONBool(stored["fixEncoding"]); ok {
		config.FixEncoding = value
	}
	if value, ok := settingsJSONInt(stored["maxJsonDepth"]); ok {
		config.MaxJSONDepth = value
	}
	if value, ok := settingsJSONInt(stored["maxFixSize"]); ok {
		config.MaxFixSize = value
	}
	return config
}

// normalizeFakeStreamingWhitelist 复刻 normalizeFakeStreamingWhitelist
// （transformers.ts:210-245）：非数组退回默认（空数组，因为 DEFAULT_FAKE_STREAMING_WHITELIST
// 为空）；数组则逐项裁剪——模型名 trim 后非空且首次出现，分组标签 trim 后非空且去重。
func normalizeFakeStreamingWhitelist(raw json.RawMessage) []FakeStreamingWhitelistEntry {
	result := make([]FakeStreamingWhitelistEntry, 0, 4)
	if len(raw) == 0 || string(raw) == "null" {
		return result
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return result
	}
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		// model 与 groupTags 分开解析：Node 只对**类型不符的 groupTags** 取空数组并保留该条目
		// （candidate.groupTags 不是数组即跳过标签循环），整条解构失败会把它错删。
		var candidate struct {
			Model     json.RawMessage `json:"model"`
			GroupTags json.RawMessage `json:"groupTags"`
		}
		if err := json.Unmarshal(entry, &candidate); err != nil {
			continue
		}
		rawModel, modelErr := settingsStringValue(candidate.Model)
		if modelErr != nil {
			continue
		}
		model := strings.TrimSpace(rawModel)
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		tags := make([]string, 0, 4)
		var rawTags []json.RawMessage
		groupSeen := make(map[string]bool, 4)
		if candidate.GroupTags != nil && json.Unmarshal(candidate.GroupTags, &rawTags) == nil {
			for _, rawTag := range rawTags {
				tag, err := settingsStringValue(rawTag)
				if err != nil {
					continue
				}
				trimmed := strings.TrimSpace(tag)
				if trimmed == "" || groupSeen[trimmed] {
					continue
				}
				groupSeen[trimmed] = true
				tags = append(tags, trimmed)
			}
		}
		result = append(result, FakeStreamingWhitelistEntry{Model: model, GroupTags: tags})
	}
	return result
}

// quotaLeasePercent 复刻 Node 的 `row ? parseFloat(row) : 0.05`（注意空串与 null 都算缺值，
// 而 "0" 是**有值**——JS 里非空串恒为真）。
func quotaLeasePercent(value *json.Number) float64 {
	if value == nil || value.String() == "" {
		return defaultQuotaLeasePercent
	}
	parsed, err := strconv.ParseFloat(value.String(), 64)
	if err != nil {
		return defaultQuotaLeasePercent
	}
	return parsed
}

// quotaLeaseCap 复刻 `row ? parseFloat(row) : null`。
func quotaLeaseCap(value *json.Number) *float64 {
	if value == nil || value.String() == "" {
		return nil
	}
	parsed, err := strconv.ParseFloat(value.String(), 64)
	if err != nil {
		return nil
	}
	return &parsed
}

// settingsJSONBool / settingsJSONInt 读 jsonb 里的标量；类型不符即「缺值」。
func settingsJSONBool(raw json.RawMessage) (bool, bool) {
	if len(raw) == 0 {
		return false, false
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, false
	}
	return value, true
}

func settingsJSONInt(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false
	}
	return int(value), true
}

// systemSettingsAPI 是 /system/settings 的处理器依赖。
type systemSettingsAPI struct {
	pools           *store.Pools
	problems        ProblemWriter
	audit           AuditSink
	invalidator     Invalidator
	publisher       PublicStatusPublisher
	dashboardCaches DashboardCacheInvalidator
	logger          *logx.Logger
	now             func() time.Time
}

// RegisterSystemSettingsRoutes 注册 /system/settings 的读与写。
//
// Store 未装配时两条都不注册（回退 Node）——与本包其余模块同一条纪律；此时设置页仍由 Node 作答，
// 而不是由 Go 给出一份读不出库的默认值。
func RegisterSystemSettingsRoutes(router *Router, deps Deps) {
	if deps.Store == nil {
		if deps.Logger != nil {
			deps.Logger.Error("admin_system_settings_store_unwired", map[string]any{
				"module": "system",
				"action": "routes_not_registered",
			})
		}
		return
	}
	api, ok := newSystemSettingsAPI(deps)
	if !ok {
		return
	}
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/system/settings",
		Access:      AccessAdmin,
		Module:      "system",
		OperationID: "getSystemSettings",
		Handler:     http.HandlerFunc(api.handleSettingsGet),
	})
	router.Add(Route{
		Method:      http.MethodPut,
		Path:        "/system/settings",
		Access:      AccessAdmin,
		Module:      "system",
		OperationID: "updateSystemSettings",
		Handler:     http.HandlerFunc(api.handleSettingsPut),
	})
}

// handleSettingsGet 复刻 GET /system/settings（actions/system-config.ts:fetchSystemSettings）：
// admin 档位由守卫保证，读库缺行时补默认行。
func (api *systemSettingsAPI) handleSettingsGet(writer http.ResponseWriter, request *http.Request) {
	row, err := api.pools.EnsureAdminSystemSettings(request.Context())
	if err != nil {
		api.writeSettingsFailure(writer, request, err)
		return
	}
	writeShellJSON(writer, http.StatusOK, buildSystemSettingsBody(row, api.now()))
}

// handleSettingsPut 是 PUT /system/settings。
func (api *systemSettingsAPI) handleSettingsPut(writer http.ResponseWriter, request *http.Request) {
	decoded, problems := readSystemSettingsUpdate(request, settingsUpdateAPISchema)
	if len(problems) > 0 {
		writeSettingsValidationProblem(
			writer, request, settingsValidationErrorCode(problems), toInvalidParams(problems),
		)
		return
	}

	before, err := api.pools.EnsureAdminSystemSettings(request.Context())
	if err != nil {
		api.writeSettingsFailure(writer, request, err)
		return
	}
	beforeBody := buildSystemSettingsBody(before, api.now())

	// 竞速窗口不变量：生效值 = 本次提供的值 ?? 当前值 ?? 出厂默认。
	if !systemSettingsDiscoveryWindowValid(decoded, beforeBody) {
		api.emitSettingsAudit(request, beforeBody, nil, false, "UPDATE_FAILED")
		api.problems.WriteProblem(
			writer, request, http.StatusBadRequest, discoveryWindowInvalidErrorCode, "",
		)
		return
	}

	updated, err := api.pools.UpdateAdminSystemSettings(
		request.Context(), before.ID, decoded.patch(),
	)
	if err != nil {
		api.emitSettingsAudit(request, beforeBody, nil, false, "UPDATE_FAILED")
		api.writeSettingsFailure(writer, request, err)
		return
	}
	afterBody := buildSystemSettingsBody(updated, api.now())

	// 失效广播：Node 的 invalidateSystemSettingsCache() 清本进程缓存并广播配置域。
	ctx := request.Context()
	if api.invalidator != nil {
		api.invalidator.PublishDomain(ctx, cfgsync.DomainSystemSettings)
	}
	if decoded.timezonePresent {
		// Node 还会失效 overview/statistics/leaderboard 三族 dashboard 缓存——那三族键里嵌着时区
		// （`:tz:<zone>`），不清就会继续按旧时区切分“今天”。未装配时记 warn（与 Node 的
		// catch 分支同语义：只记日志，不影响请求结果）。
		if api.dashboardCaches == nil {
			api.logger.Warn("admin_system_settings_timezone_caches_not_invalidated", map[string]any{
				"families": "overview,statistics,leaderboard",
				"reason":   "dashboard_cache_invalidator_unwired",
			})
		} else if err := api.dashboardCaches.InvalidateDashboardCaches(ctx); err != nil {
			api.logger.Warn("admin_system_settings_timezone_caches_invalidation_failed", map[string]any{
				"families": "overview,statistics,leaderboard",
				"error":    err.Error(),
			})
		}
	}

	// public-status 投影重发：Node 在动了 siteTitle/timezone/窗口/聚合间隔时重建整份公开配置
	// 快照（分组、模型、厂商图标、最新价格）并写 Redis。未装配发布器（无 Redis 命令连接）时
	// 如实回失败码——DB 真值已存，但投影没更新，运维要能看到这个差异。
	var warning *string
	if decoded.touchesPublicStatusProjection() {
		if api.publisher == nil {
			code := publicStatusPublishFailedCode
			warning = &code
			api.logger.Warn("admin_system_settings_public_status_projection_not_republished", map[string]any{
				"reason": "publisher_unwired",
			})
		} else {
			result, err := api.publisher.PublishCurrentProjection(ctx, "save-system-settings")
			if err != nil {
				api.logger.Warn("admin_system_settings_public_status_projection_publish_failed", map[string]any{
					"error": err.Error(),
				})
			}
			warning = composePublicStatusRepublish(result, err)
		}
	}

	api.emitSettingsAudit(request, beforeBody, &afterBody, true, "")
	writeShellJSON(writer, http.StatusOK, SystemSettingsUpdateResponse{
		SystemSettingsBody:                afterBody,
		PublicStatusProjectionWarningCode: warning,
	})
}

// touchesPublicStatusProjection 复刻 saveSystemSettings 的重发条件。
func (d systemSettingsUpdate) touchesPublicStatusProjection() bool {
	return d.siteTitlePresent || d.timezonePresent || d.publicStatusWindowPresent ||
		d.publicStatusAggregationPresent
}

// systemSettingsDiscoveryWindowValid 复刻两条写路径共用的窗口校验。
func systemSettingsDiscoveryWindowValid(d systemSettingsUpdate, current SystemSettingsBody) bool {
	discoverySLA := current.DiscoverySLAMS
	if d.discoverySLAMPresent {
		discoverySLA = *d.discoverySLA
	}
	stickySLA := current.StickySLAMS
	if d.stickySLAMPresent {
		stickySLA = *d.stickySLA
	}
	rounds := current.MaxDiscoveryRounds
	if d.maxDiscoveryRoundsPresent {
		rounds = *d.maxDiscoveryRounds
	}
	total := current.RacingTotalTimeoutMS
	if d.racingTotalPresent {
		total = *d.racingTotalTimeout
	}
	return total >= stickySLA+rounds*discoverySLA
}

// emitSettingsAudit 写审计（Node 的 emitActionAudit({category:"system_settings", action:
// "system_settings.update", targetName:"global"})）。
func (api *systemSettingsAPI) emitSettingsAudit(
	request *http.Request,
	before SystemSettingsBody,
	after *SystemSettingsBody,
	success bool,
	errorMessage string,
) {
	if api.audit == nil {
		return
	}
	principal, _ := PrincipalFrom(request.Context())
	event := AuditEvent{
		Category:   "system_settings",
		Principal:  principal,
		Action:     "system_settings.update",
		TargetType: "system_settings",
		TargetID:   strconv.FormatInt(before.ID, 10),
		TargetName: "global",
		Before:     systemSettingsAuditMap(before),
		Success:    success,
		IP:         auditRequestIP(request),
		UserAgent:  request.UserAgent(),
	}
	if after != nil {
		event.Details = systemSettingsAuditMap(*after)
	}
	if !success {
		event.ErrorMessage = errorMessage
	}
	api.audit.Emit(request.Context(), event)
}

// systemSettingsAuditMap 把投影转成审计用的通用映射（键名与响应一致）。
func systemSettingsAuditMap(body SystemSettingsBody) map[string]any {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil
	}
	var mapped map[string]any
	if err := json.Unmarshal(raw, &mapped); err != nil {
		return nil
	}
	return mapped
}

// writeSettingsFailure 作答设置读写的内部失败。
//
// 状态码取 Node 的 actionError（system/handlers.ts:44-55）：action 层错误在 Node 一律映射成
// 400/403 的 problem；此处只有内部错误（读库失败），按 400 作答并记日志。
func (api *systemSettingsAPI) writeSettingsFailure(
	writer http.ResponseWriter,
	request *http.Request,
	err error,
) {
	api.logger.Warn("admin_system_settings_request_failed", map[string]any{
		"path":  request.URL.Path,
		"error": err.Error(),
	})
	api.problems.WriteProblem(writer, request, http.StatusBadRequest, "", "")
}

// settingsUpdateSchema 是请求体解析的模式。
type settingsUpdateSchema int

const (
	// settingsUpdateAPISchema 是 /api/v1/system/settings 的 schema：strict（未知键 400）、
	// 不做数字强转（zod 的 .number() 不接受字符串）。
	settingsUpdateAPISchema settingsUpdateSchema = iota
	// settingsUpdateActionSchema 是 /api/admin/system-config 的 schema：忽略未知键、
	// 数字列走 z.coerce.number()（接受 "30" 这类字符串）。
	settingsUpdateActionSchema
)

// systemSettingsUpdate 是解析后的部分更新。
//
// 「出现」与「出现为 null」必须分开：zod 的 .optional().nullable() 里，`null` 是**有值**的
// 清空指令（timezone=null → 跟随 TZ；replayEnabled=null → 清除覆写），漏了 Present 标志就会
// 把「清空」当成「没提交」。
type systemSettingsUpdate struct {
	siteTitle                        *string
	siteTitlePresent                 bool
	allowGlobalUsageView             *bool
	currencyDisplay                  *string
	billingModelSource               *string
	codexPriorityBillingSource       *string
	billNonSuccessfulRequests        *bool
	billHedgeLosers                  *bool
	legacyHedgeMaxInFlight           *int
	timezone                         *string
	timezonePresent                  bool
	enableAutoCleanup                *bool
	cleanupRetentionDays             *int
	cleanupSchedule                  *string
	cleanupBatchSize                 *int
	enableClientVersionCheck         *bool
	verboseProviderError             *bool
	passThroughUpstreamErrorMsg      *bool
	enableHTTP2                      *bool
	enableOpenAIResponsesWS          *bool
	enableHighConcurrencyMode        *bool
	interceptAnthropicWarmupReqs     *bool
	enableThinkingSignatureRect      *bool
	enableThinkingBudgetRect         *bool
	enableThinkingEffortConflict     *bool
	enableGeminiFunctionIDRect       *bool
	enableBillingHeaderRect          *bool
	enableResponseInputRect          *bool
	allowNonConvEndpointFallback     *bool
	fakeStreamingWhitelist           json.RawMessage
	fakeStreamingWhitelistPresent    bool
	enableCodexSessionIDComplete     *bool
	enableClaudeMetadataUserID       *bool
	enableResponseFixer              *bool
	responseFixerConfig              json.RawMessage
	responseFixerConfigPresent       bool
	quotaDBRefreshIntervalSecs       *int
	quotaLeasePercent5h              *float64
	quotaLeasePercentDaily           *float64
	quotaLeasePercentWeekly          *float64
	quotaLeasePercentMonthly         *float64
	quotaLeaseCapUSD                 *float64
	quotaLeaseCapUSDPresent          bool
	publicStatusWindowHours          *int
	publicStatusWindowPresent        bool
	publicStatusAggregationMins      *int
	publicStatusAggregationPresent   bool
	ipExtractionConfig               json.RawMessage
	ipExtractionConfigPresent        bool
	ipGeoLookupEnabled               *bool
	streamGateMode                   *string
	affinityIgnoreClientSession      *bool
	affinityEnabled                  *bool
	providerLiveStatsEnabled         *bool
	replayEnabled                    *bool
	replayEnabledPresent             bool
	replayCacheTTLMinutes            *int
	cacheEffectivenessEnabled        *bool
	cacheEffectivenessEnabledPresent bool
	discoveryEnabled                 *bool
	discoveryConcurrency             *int
	maxDiscoveryRounds               *int
	maxDiscoveryRoundsPresent        bool
	discoverySLA                     *int
	discoverySLAMPresent             bool
	stickySLA                        *int
	stickySLAMPresent                bool
	racingTotalTimeout               *int
	racingTotalPresent               bool
	stickyTimeoutCooldown            *int
}

// patch 把解析结果转成 store 的部分更新（只含「出现过」的列）。
func (d systemSettingsUpdate) patch() store.AdminSystemSettingsPatch {
	updates := make(map[store.AdminSystemSettingsColumn]any)
	set := func(column store.AdminSystemSettingsColumn, value any) {
		updates[column] = value
	}
	if d.siteTitlePresent {
		set(store.ColSiteTitle, strings.TrimSpace(*d.siteTitle))
	}
	if d.allowGlobalUsageView != nil {
		set(store.ColAllowGlobalUsageView, *d.allowGlobalUsageView)
	}
	if d.currencyDisplay != nil {
		set(store.ColCurrencyDisplay, *d.currencyDisplay)
	}
	if d.billingModelSource != nil {
		set(store.ColBillingModelSource, *d.billingModelSource)
	}
	if d.codexPriorityBillingSource != nil {
		set(store.ColCodexPriorityBillingSource, *d.codexPriorityBillingSource)
	}
	if d.billNonSuccessfulRequests != nil {
		set(store.ColBillNonSuccessfulRequests, *d.billNonSuccessfulRequests)
	}
	if d.billHedgeLosers != nil {
		set(store.ColBillHedgeLosers, *d.billHedgeLosers)
	}
	if d.legacyHedgeMaxInFlight != nil {
		set(store.ColLegacyHedgeMaxInFlight, *d.legacyHedgeMaxInFlight)
	}
	if d.timezonePresent {
		set(store.ColTimezone, nullableString(d.timezone))
	}
	if d.enableAutoCleanup != nil {
		set(store.ColEnableAutoCleanup, *d.enableAutoCleanup)
	}
	if d.cleanupRetentionDays != nil {
		set(store.ColCleanupRetentionDays, *d.cleanupRetentionDays)
	}
	if d.cleanupSchedule != nil {
		set(store.ColCleanupSchedule, *d.cleanupSchedule)
	}
	if d.cleanupBatchSize != nil {
		set(store.ColCleanupBatchSize, *d.cleanupBatchSize)
	}
	if d.enableClientVersionCheck != nil {
		set(store.ColEnableClientVersionCheck, *d.enableClientVersionCheck)
	}
	if d.verboseProviderError != nil {
		set(store.ColVerboseProviderError, *d.verboseProviderError)
	}
	if d.passThroughUpstreamErrorMsg != nil {
		set(store.ColPassThroughUpstreamErrorMsg, *d.passThroughUpstreamErrorMsg)
	}
	if d.enableHTTP2 != nil {
		set(store.ColEnableHTTP2, *d.enableHTTP2)
	}
	if d.enableOpenAIResponsesWS != nil {
		set(store.ColEnableOpenAIResponsesWS, *d.enableOpenAIResponsesWS)
	}
	if d.enableHighConcurrencyMode != nil {
		set(store.ColEnableHighConcurrencyMode, *d.enableHighConcurrencyMode)
	}
	if d.interceptAnthropicWarmupReqs != nil {
		set(store.ColInterceptAnthropicWarmupReqs, *d.interceptAnthropicWarmupReqs)
	}
	if d.enableThinkingSignatureRect != nil {
		set(store.ColEnableThinkingSignatureRect, *d.enableThinkingSignatureRect)
	}
	if d.enableThinkingBudgetRect != nil {
		set(store.ColEnableThinkingBudgetRect, *d.enableThinkingBudgetRect)
	}
	if d.enableThinkingEffortConflict != nil {
		set(store.ColEnableThinkingEffortConflict, *d.enableThinkingEffortConflict)
	}
	if d.enableGeminiFunctionIDRect != nil {
		set(store.ColEnableGeminiFunctionIDRect, *d.enableGeminiFunctionIDRect)
	}
	if d.enableBillingHeaderRect != nil {
		set(store.ColEnableBillingHeaderRect, *d.enableBillingHeaderRect)
	}
	if d.enableResponseInputRect != nil {
		set(store.ColEnableResponseInputRect, *d.enableResponseInputRect)
	}
	if d.allowNonConvEndpointFallback != nil {
		set(store.ColAllowNonConvEndpointFallback, *d.allowNonConvEndpointFallback)
	}
	if d.fakeStreamingWhitelistPresent {
		set(store.ColFakeStreamingWhitelist, nullableJSONRaw(d.fakeStreamingWhitelist))
	}
	if d.enableCodexSessionIDComplete != nil {
		set(store.ColEnableCodexSessionIDComplete, *d.enableCodexSessionIDComplete)
	}
	if d.enableClaudeMetadataUserID != nil {
		set(store.ColEnableClaudeMetadataUserID, *d.enableClaudeMetadataUserID)
	}
	if d.enableResponseFixer != nil {
		set(store.ColEnableResponseFixer, *d.enableResponseFixer)
	}
	if d.responseFixerConfigPresent {
		set(store.ColResponseFixerConfig, nullableJSONRaw(d.responseFixerConfig))
	}
	if d.quotaDBRefreshIntervalSecs != nil {
		set(store.ColQuotaDBRefreshIntervalSecs, *d.quotaDBRefreshIntervalSecs)
	}
	if d.quotaLeasePercent5h != nil {
		set(store.ColQuotaLeasePercent5h, *d.quotaLeasePercent5h)
	}
	if d.quotaLeasePercentDaily != nil {
		set(store.ColQuotaLeasePercentDaily, *d.quotaLeasePercentDaily)
	}
	if d.quotaLeasePercentWeekly != nil {
		set(store.ColQuotaLeasePercentWeekly, *d.quotaLeasePercentWeekly)
	}
	if d.quotaLeasePercentMonthly != nil {
		set(store.ColQuotaLeasePercentMonthly, *d.quotaLeasePercentMonthly)
	}
	if d.quotaLeaseCapUSDPresent {
		set(store.ColQuotaLeaseCapUSD, nullableFloat(d.quotaLeaseCapUSD))
	}
	if d.publicStatusWindowPresent {
		set(store.ColPublicStatusWindowHours, *d.publicStatusWindowHours)
	}
	if d.publicStatusAggregationPresent {
		set(store.ColPublicStatusAggregationMins, *d.publicStatusAggregationMins)
	}
	if d.ipExtractionConfigPresent {
		set(store.ColIPExtractionConfig, nullableJSONRaw(d.ipExtractionConfig))
	}
	if d.ipGeoLookupEnabled != nil {
		set(store.ColIPGeoLookupEnabled, *d.ipGeoLookupEnabled)
	}
	if d.streamGateMode != nil {
		set(store.ColStreamGateMode, *d.streamGateMode)
	}
	if d.affinityIgnoreClientSession != nil {
		set(store.ColAffinityIgnoreClientSession, *d.affinityIgnoreClientSession)
	}
	if d.affinityEnabled != nil {
		set(store.ColAffinityEnabled, *d.affinityEnabled)
	}
	if d.providerLiveStatsEnabled != nil {
		set(store.ColProviderLiveStatsEnabled, *d.providerLiveStatsEnabled)
	}
	if d.replayEnabledPresent {
		set(store.ColReplayEnabled, nullableBool(d.replayEnabled))
	}
	if d.replayCacheTTLMinutes != nil {
		set(store.ColReplayCacheTTLMinutes, *d.replayCacheTTLMinutes)
	}
	if d.cacheEffectivenessEnabledPresent {
		set(store.ColCacheEffectivenessEnabled, nullableBool(d.cacheEffectivenessEnabled))
	}
	if d.discoveryEnabled != nil {
		set(store.ColDiscoveryEnabled, *d.discoveryEnabled)
	}
	if d.discoveryConcurrency != nil {
		set(store.ColDiscoveryConcurrency, *d.discoveryConcurrency)
	}
	if d.maxDiscoveryRounds != nil {
		set(store.ColMaxDiscoveryRounds, *d.maxDiscoveryRounds)
	}
	if d.discoverySLAMPresent {
		set(store.ColDiscoverySLAMS, *d.discoverySLA)
	}
	if d.stickySLAMPresent {
		set(store.ColStickySLAMS, *d.stickySLA)
	}
	if d.racingTotalPresent {
		set(store.ColRacingTotalTimeoutMS, *d.racingTotalTimeout)
	}
	if d.stickyTimeoutCooldown != nil {
		set(store.ColStickyTimeoutCooldownMS, *d.stickyTimeoutCooldown)
	}
	return store.AdminSystemSettingsPatch{Updates: updates}
}

// nullableJSONRaw 把 JSON null 归一成未类型化的 nil（写 SQL NULL），非空值原样透传。
func nullableJSONRaw(value json.RawMessage) any {
	if isJSONNull(value) {
		return nil
	}
	return value
}

func nullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableBool(value *bool) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableFloat(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}

// readSystemSettingsUpdate 读并校验请求体。
func readSystemSettingsUpdate(
	request *http.Request,
	mode settingsUpdateSchema,
) (systemSettingsUpdate, []settingsInvalidParam) {
	raw, problems := readSettingsJSONObject(request)
	if len(problems) > 0 {
		return systemSettingsUpdate{}, problems
	}
	return decodeSystemSettingsUpdate(raw, mode)
}

// settingsInvalidParam 是校验失败的单项明细（形状与 InvalidParam 一致，便于直接作答）。
type settingsInvalidParam = InvalidParam

// readSettingsJSONObject 读请求体成「字段 -> 原始 JSON」。空体按空对象（Node 的
// parseHonoJsonBody 对空体同样按空对象处理，是否缺字段由各 schema 决定）。
func readSettingsJSONObject(request *http.Request) (map[string]json.RawMessage, []settingsInvalidParam) {
	data, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
	if err != nil {
		return nil, []settingsInvalidParam{{
			Path: []any{}, Code: "invalid_body", Message: "Request body could not be read.",
		}}
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, []settingsInvalidParam{{
			Path: []any{}, Code: "invalid_json", Message: "Invalid JSON body.",
		}}
	}
	return raw, nil
}

// toInvalidParams 把明细转成 ProblemWriter 要的形状。
func toInvalidParams(problems []settingsInvalidParam) []InvalidParam {
	params := make([]InvalidParam, 0, len(problems))
	for _, problem := range problems {
		params = append(params, InvalidParam(problem))
	}
	return params
}

// decodeSystemSettingsUpdate 按字段逐项解析与校验。
//
// 校验口径取**两条 schema 的交集**：API 层的 SystemSettingsUpdateSchema（strict、非强转）决定
// 键集与类型，action 层的 UpdateSystemSettingsSchema（强转 + 区间）决定数值边界与错误码。
// 单独照抄任一层都会漏：只照 API 层会漏掉 cleanupBatchSize 之类的区间，只照 action 层会把
// 未知键放行。
func decodeSystemSettingsUpdate(
	raw map[string]json.RawMessage,
	mode settingsUpdateSchema,
) (systemSettingsUpdate, []settingsInvalidParam) {
	decoded := systemSettingsUpdate{}
	problems := make([]settingsInvalidParam, 0, 4)
	unknown := make([]any, 0)
	for key := range raw {
		if !systemSettingsUpdateKeys[key] {
			unknown = append(unknown, key)
		}
	}
	if mode == settingsUpdateAPISchema && len(unknown) > 0 {
		sort.Slice(unknown, func(i, j int) bool { return unknown[i].(string) < unknown[j].(string) })
		problems = append(problems, settingsInvalidParam{
			Path: unknown, Code: "unrecognized_keys", Message: "Unrecognized key(s) in object",
		})
		return decoded, problems
	}

	coerce := mode == settingsUpdateActionSchema
	field := func(name string) (json.RawMessage, bool) {
		value, ok := raw[name]
		return value, ok
	}
	fail := func(name, code, message string) {
		problems = append(problems, settingsInvalidParam{
			Path: []any{name}, Code: code, Message: message,
		})
	}

	// 布尔字段（含三条可空布尔）。
	booleans := map[string]**bool{
		"allowGlobalUsageView":                         &decoded.allowGlobalUsageView,
		"billNonSuccessfulRequests":                    &decoded.billNonSuccessfulRequests,
		"billHedgeLosers":                              &decoded.billHedgeLosers,
		"enableAutoCleanup":                            &decoded.enableAutoCleanup,
		"enableClientVersionCheck":                     &decoded.enableClientVersionCheck,
		"verboseProviderError":                         &decoded.verboseProviderError,
		"passThroughUpstreamErrorMessage":              &decoded.passThroughUpstreamErrorMsg,
		"enableHttp2":                                  &decoded.enableHTTP2,
		"enableOpenaiResponsesWebsocket":               &decoded.enableOpenAIResponsesWS,
		"enableHighConcurrencyMode":                    &decoded.enableHighConcurrencyMode,
		"interceptAnthropicWarmupRequests":             &decoded.interceptAnthropicWarmupReqs,
		"enableThinkingSignatureRectifier":             &decoded.enableThinkingSignatureRect,
		"enableThinkingBudgetRectifier":                &decoded.enableThinkingBudgetRect,
		"enableThinkingEffortConflictRectifier":        &decoded.enableThinkingEffortConflict,
		"enableGeminiFunctionIdRectifier":              &decoded.enableGeminiFunctionIDRect,
		"enableBillingHeaderRectifier":                 &decoded.enableBillingHeaderRect,
		"enableResponseInputRectifier":                 &decoded.enableResponseInputRect,
		"allowNonConversationEndpointProviderFallback": &decoded.allowNonConvEndpointFallback,
		"enableCodexSessionIdCompletion":               &decoded.enableCodexSessionIDComplete,
		"enableClaudeMetadataUserIdInjection":          &decoded.enableClaudeMetadataUserID,
		"enableResponseFixer":                          &decoded.enableResponseFixer,
		"ipGeoLookupEnabled":                           &decoded.ipGeoLookupEnabled,
		"affinityIgnoreClientSessionId":                &decoded.affinityIgnoreClientSession,
		"affinityEnabled":                              &decoded.affinityEnabled,
		"providerLiveStatsEnabled":                     &decoded.providerLiveStatsEnabled,
		"discoveryEnabled":                             &decoded.discoveryEnabled,
	}
	for name, target := range booleans {
		value, ok := field(name)
		if !ok {
			continue
		}
		parsed, err := settingsBoolValue(value)
		if err != nil {
			fail(name, "invalid_type", "Expected boolean, received "+err.Error())
			continue
		}
		bound := parsed
		*target = &bound
	}

	// 可空布尔：null 是「清除覆写」。
	for name, target := range map[string]struct {
		value   **bool
		present *bool
	}{
		"replayEnabled":             {&decoded.replayEnabled, &decoded.replayEnabledPresent},
		"cacheEffectivenessEnabled": {&decoded.cacheEffectivenessEnabled, &decoded.cacheEffectivenessEnabledPresent},
	} {
		value, ok := field(name)
		if !ok {
			continue
		}
		if isJSONNull(value) {
			*target.present = true
			continue
		}
		parsed, err := settingsBoolValue(value)
		if err != nil {
			fail(name, "invalid_type", "Expected boolean, received "+err.Error())
			continue
		}
		bound := parsed
		*target.value = &bound
		*target.present = true
	}

	// 字符串字段。
	for name, target := range map[string]**string{
		"cleanupSchedule":            &decoded.cleanupSchedule,
		"billingModelSource":         &decoded.billingModelSource,
		"codexPriorityBillingSource": &decoded.codexPriorityBillingSource,
		"currencyDisplay":            &decoded.currencyDisplay,
	} {
		value, ok := field(name)
		if !ok {
			continue
		}
		parsed, err := settingsStringValue(value)
		if err != nil {
			fail(name, "invalid_type", "Expected string, received "+err.Error())
			continue
		}
		bound := parsed
		*target = &bound
	}
	if value, ok := field("siteTitle"); ok {
		parsed, err := settingsStringValue(value)
		if err != nil {
			fail("siteTitle", "invalid_type", "Expected string, received "+err.Error())
		} else {
			decoded.siteTitle = &parsed
			decoded.siteTitlePresent = true
		}
	}
	if value, ok := field("timezone"); ok {
		if isJSONNull(value) {
			// null 是**有值**的清空指令（跟随 TZ 环境变量）。
			decoded.timezonePresent = true
		} else {
			parsed, err := settingsStringValue(value)
			if err != nil {
				fail("timezone", "invalid_type", "Expected string, received "+err.Error())
			} else if !isIANATimezone(parsed) {
				fail("timezone", "invalid_string", "无效的时区标识符，请使用 IANA 时区格式（如 Asia/Shanghai）")
			} else {
				decoded.timezone = &parsed
				decoded.timezonePresent = true
			}
		}
	}

	// 枚举字段。
	enumChecks := []struct {
		name    string
		target  *string
		allowed []string
		message string
	}{
		{"currencyDisplay", decoded.currencyDisplay, settingsKeysOf(currencyCodes), "不支持的货币类型"},
		{"billingModelSource", decoded.billingModelSource, []string{"original", "redirected"}, "不支持的计费模型来源"},
		{"codexPriorityBillingSource", decoded.codexPriorityBillingSource, []string{"requested", "actual"}, "不支持的 Codex Priority 计费来源"},
	}
	for _, check := range enumChecks {
		if check.target == nil {
			continue
		}
		if !settingsContainsString(check.allowed, *check.target) {
			fail(check.name, "invalid_enum_value", check.message)
		}
	}
	if value, ok := field("streamGateMode"); ok {
		parsed, err := settingsStringValue(value)
		if err != nil {
			fail("streamGateMode", "invalid_type", "Expected string, received "+err.Error())
		} else if !settingsContainsString([]string{"off", "enforce"}, parsed) {
			fail("streamGateMode", "invalid_enum_value", "不支持的流式门控模式")
		} else {
			decoded.streamGateMode = &parsed
		}
	}

	// 整数与数值字段（名称 -> 目标、区间与错误码）。
	decodeSettingsNumber(raw, "cleanupRetentionDays", coerce, &decoded.cleanupRetentionDays,
		1, 365, "", &problems)
	decodeSettingsNumber(raw, "cleanupBatchSize", coerce, &decoded.cleanupBatchSize,
		1000, 100000, "", &problems)
	decodeSettingsNumber(raw, "legacyHedgeMaxInFlight", true, &decoded.legacyHedgeMaxInFlight,
		1, 4, legacyHedgeInvalidErrorCode, &problems)
	decodeSettingsNumber(raw, "discoveryConcurrency", coerce, &decoded.discoveryConcurrency,
		discoveryFieldLimits["discoveryConcurrency"][0], discoveryFieldLimits["discoveryConcurrency"][1],
		discoverySettingsInvalidErrorCode, &problems)
	decodeSettingsNumber(raw, "maxDiscoveryRounds", coerce, &decoded.maxDiscoveryRounds,
		discoveryFieldLimits["maxDiscoveryRounds"][0], discoveryFieldLimits["maxDiscoveryRounds"][1],
		discoverySettingsInvalidErrorCode, &problems)
	decodeSettingsNumber(raw, "discoverySlaMs", coerce, &decoded.discoverySLA,
		discoveryFieldLimits["discoverySlaMs"][0], discoveryFieldLimits["discoverySlaMs"][1],
		discoverySettingsInvalidErrorCode, &problems)
	decodeSettingsNumber(raw, "stickySlaMs", coerce, &decoded.stickySLA,
		discoveryFieldLimits["stickySlaMs"][0], discoveryFieldLimits["stickySlaMs"][1],
		discoverySettingsInvalidErrorCode, &problems)
	decodeSettingsNumber(raw, "racingTotalTimeoutMs", coerce, &decoded.racingTotalTimeout,
		discoveryFieldLimits["racingTotalTimeoutMs"][0], discoveryFieldLimits["racingTotalTimeoutMs"][1],
		discoverySettingsInvalidErrorCode, &problems)
	decodeSettingsNumber(raw, "stickyTimeoutCooldownMs", coerce, &decoded.stickyTimeoutCooldown,
		discoveryFieldLimits["stickyTimeoutCooldownMs"][0], discoveryFieldLimits["stickyTimeoutCooldownMs"][1],
		discoverySettingsInvalidErrorCode, &problems)
	decodeSettingsNumber(raw, "replayCacheTtlMinutes", coerce, &decoded.replayCacheTTLMinutes,
		replayCacheTTLMinutesMin, replayCacheTTLMinutesMax, replayCacheTTLInvalidErrorCode, &problems)
	decodeSettingsNumber(raw, "publicStatusWindowHours", coerce, &decoded.publicStatusWindowHours,
		1, maxPublicStatusRangeHours, "", &problems)
	decodeSettingsNumber(raw, "quotaDbRefreshIntervalSeconds", false, &decoded.quotaDBRefreshIntervalSecs,
		1, 300, "", &problems)
	decoded.maxDiscoveryRoundsPresent = decoded.maxDiscoveryRounds != nil
	decoded.discoverySLAMPresent = decoded.discoverySLA != nil
	decoded.stickySLAMPresent = decoded.stickySLA != nil
	decoded.racingTotalPresent = decoded.racingTotalTimeout != nil
	decoded.publicStatusWindowPresent = decoded.publicStatusWindowHours != nil

	// 百分比（0..1 的浮点）。
	for name, target := range map[string]**float64{
		"quotaLeasePercent5h":      &decoded.quotaLeasePercent5h,
		"quotaLeasePercentDaily":   &decoded.quotaLeasePercentDaily,
		"quotaLeasePercentWeekly":  &decoded.quotaLeasePercentWeekly,
		"quotaLeasePercentMonthly": &decoded.quotaLeasePercentMonthly,
	} {
		decodeSettingsFloat(raw, name, false, target, 0, 1, &problems)
	}
	if value, ok := raw["quotaLeaseCapUsd"]; ok {
		if isJSONNull(value) {
			decoded.quotaLeaseCapUSDPresent = true
		} else {
			var parsed float64
			if err := json.Unmarshal(value, &parsed); err != nil {
				fail("quotaLeaseCapUsd", "invalid_type", "Expected number, received string")
			} else if parsed < 0 {
				fail("quotaLeaseCapUsd", "too_small", "Lease cap cannot be negative")
			} else {
				decoded.quotaLeaseCapUSD = &parsed
				decoded.quotaLeaseCapUSDPresent = true
			}
		}
	}

	// 公共状态聚合间隔是**枚举**（5/15/30/60），不是区间。
	if value, ok := raw["publicStatusAggregationIntervalMinutes"]; ok {
		parsed, err := settingsIntValue(value, coerce)
		if err != nil {
			fail("publicStatusAggregationIntervalMinutes", "invalid_type", "PUBLIC_STATUS_INTERVAL_INVALID_INT")
		} else if !publicStatusIntervals[parsed] {
			fail("publicStatusAggregationIntervalMinutes", "invalid_enum_value", "PUBLIC_STATUS_INTERVAL_INVALID")
		} else {
			decoded.publicStatusAggregationMins = &parsed
			decoded.publicStatusAggregationPresent = true
		}
	}

	// 三块 JSON 字段：原样透传（Node 把校验后的对象直接写 jsonb）。
	for name, target := range map[string]*struct {
		raw     *json.RawMessage
		present *bool
	}{
		"fakeStreamingWhitelist": {&decoded.fakeStreamingWhitelist, &decoded.fakeStreamingWhitelistPresent},
		"responseFixerConfig":    {&decoded.responseFixerConfig, &decoded.responseFixerConfigPresent},
		"ipExtractionConfig":     {&decoded.ipExtractionConfig, &decoded.ipExtractionConfigPresent},
	} {
		value, ok := raw[name]
		if !ok {
			continue
		}
		if name == "responseFixerConfig" {
			if problems := validateResponseFixerConfig(value); len(problems) > 0 {
				problems := problems
				fail(name, problems[0].Code, problems[0].Message)
				continue
			}
		}
		if name == "fakeStreamingWhitelist" {
			if problems := validateFakeStreamingWhitelist(value); len(problems) > 0 {
				fail(name, "custom", problems[0].Message)
				continue
			}
		}
		*target.raw = value
		*target.present = true
	}

	return decoded, problems
}

// systemSettingsUpdateKeys 是 PUT 允许的键集（Node 的 SystemSettingsUpdateSchema 的键，
// 即读 schema 去掉 id/createdAt/updatedAt）。
var systemSettingsUpdateKeys = map[string]bool{
	"siteTitle": true, "allowGlobalUsageView": true, "currencyDisplay": true,
	"billingModelSource": true, "codexPriorityBillingSource": true,
	"billNonSuccessfulRequests": true, "billHedgeLosers": true, "legacyHedgeMaxInFlight": true,
	"discoveryEnabled": true, "discoveryConcurrency": true, "maxDiscoveryRounds": true,
	"discoverySlaMs": true, "stickySlaMs": true, "racingTotalTimeoutMs": true,
	"stickyTimeoutCooldownMs": true, "timezone": true, "enableAutoCleanup": true,
	"cleanupRetentionDays": true, "cleanupSchedule": true, "cleanupBatchSize": true,
	"enableClientVersionCheck": true, "verboseProviderError": true,
	"passThroughUpstreamErrorMessage": true, "enableHttp2": true,
	"enableOpenaiResponsesWebsocket": true, "enableHighConcurrencyMode": true,
	"interceptAnthropicWarmupRequests": true, "enableThinkingSignatureRectifier": true,
	"enableThinkingBudgetRectifier": true, "enableThinkingEffortConflictRectifier": true,
	"enableGeminiFunctionIdRectifier": true, "enableBillingHeaderRectifier": true,
	"enableResponseInputRectifier": true, "allowNonConversationEndpointProviderFallback": true,
	"fakeStreamingWhitelist": true, "enableCodexSessionIdCompletion": true,
	"enableClaudeMetadataUserIdInjection": true, "enableResponseFixer": true,
	"responseFixerConfig": true, "quotaDbRefreshIntervalSeconds": true,
	"quotaLeasePercent5h": true, "quotaLeasePercentDaily": true, "quotaLeasePercentWeekly": true,
	"quotaLeasePercentMonthly": true, "quotaLeaseCapUsd": true, "ipExtractionConfig": true,
	"ipGeoLookupEnabled": true, "publicStatusWindowHours": true,
	"publicStatusAggregationIntervalMinutes": true, "streamGateMode": true,
	"affinityIgnoreClientSessionId": true, "affinityEnabled": true, "replayEnabled": true, "replayCacheTtlMinutes": true,
	"providerLiveStatsEnabled":  true,
	"cacheEffectivenessEnabled": true,
}

// decodeSettingsNumber 解析一个整数列并做区间校验。
func decodeSettingsNumber(
	raw map[string]json.RawMessage,
	name string,
	coerce bool,
	target **int,
	min, max int,
	errorCode string,
	problems *[]settingsInvalidParam,
) {
	value, ok := raw[name]
	if !ok {
		return
	}
	message := "Expected number, received string"
	if coerce {
		message = "数字格式不正确"
	}
	if errorCode != "" {
		message = errorCode
	}
	parsed, err := settingsIntValue(value, coerce)
	if err != nil {
		*problems = append(*problems, settingsInvalidParam{
			Path: []any{name}, Code: "invalid_type", Message: message,
		})
		return
	}
	if parsed < min || parsed > max {
		*problems = append(*problems, settingsInvalidParam{
			Path: []any{name}, Code: "too_small", Message: message,
		})
		return
	}
	bound := parsed
	*target = &bound
}

// decodeSettingsFloat 解析一个浮点列并做区间校验；返回是否解析成功。
func decodeSettingsFloat(
	raw map[string]json.RawMessage,
	name string,
	coerce bool,
	target **float64,
	min, max float64,
	problems *[]settingsInvalidParam,
) bool {
	value, ok := raw[name]
	if !ok {
		return false
	}
	parsed, err := settingsFloatValue(value, coerce)
	if err != nil {
		*problems = append(*problems, settingsInvalidParam{
			Path: []any{name}, Code: "invalid_type", Message: "Expected number, received string",
		})
		return false
	}
	if parsed < min || parsed > max {
		message := "Lease percent cannot be negative"
		if parsed > max {
			message = "Lease percent cannot exceed 1"
		}
		*problems = append(*problems, settingsInvalidParam{
			Path: []any{name}, Code: "too_big", Message: message,
		})
		return false
	}
	bound := parsed
	*target = &bound
	return true
}

// settingsIntValue / settingsFloatValue / settingsBoolValue / settingsStringValue 是标量解码。
//
// coerce 为真时接受数字字符串（zod 的 z.coerce.number() 语义）；整数列拒绝小数（zod 的 .int()）。
func settingsIntValue(value json.RawMessage, coerce bool) (int, error) {
	if isJSONNull(value) {
		return 0, errors.New("null")
	}
	var number float64
	if err := json.Unmarshal(value, &number); err != nil {
		var text string
		if coerce && json.Unmarshal(value, &text) == nil && strings.TrimSpace(text) != "" {
			parsed, parseErr := strconv.ParseFloat(strings.TrimSpace(text), 64)
			if parseErr != nil {
				return 0, parseErr
			}
			number = parsed
		} else {
			return 0, err
		}
	}
	if math.Trunc(number) != number {
		return 0, fmt.Errorf("not an integer: %v", number)
	}
	return int(number), nil
}

func settingsFloatValue(value json.RawMessage, coerce bool) (float64, error) {
	if isJSONNull(value) {
		return 0, errors.New("null")
	}
	var number float64
	if err := json.Unmarshal(value, &number); err != nil {
		var text string
		if coerce && json.Unmarshal(value, &text) == nil && strings.TrimSpace(text) != "" {
			return strconv.ParseFloat(strings.TrimSpace(text), 64)
		}
		return 0, err
	}
	return number, nil
}

func settingsBoolValue(value json.RawMessage) (bool, error) {
	if isJSONNull(value) {
		return false, errors.New("null")
	}
	var parsed bool
	if err := json.Unmarshal(value, &parsed); err != nil {
		return false, err
	}
	return parsed, nil
}

func settingsStringValue(value json.RawMessage) (string, error) {
	if isJSONNull(value) {
		return "", errors.New("null")
	}
	var parsed string
	if err := json.Unmarshal(value, &parsed); err != nil {
		return "", err
	}
	return parsed, nil
}

// validateResponseFixerConfig 校验整流配置的区间（Node 的 ResponseFixerConfigSchema.partial()）。
func validateResponseFixerConfig(value json.RawMessage) []settingsInvalidParam {
	if isJSONNull(value) {
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(value, &object); err != nil {
		return []settingsInvalidParam{{Code: "invalid_type", Message: "Expected object"}}
	}
	if raw, ok := object["maxJsonDepth"]; ok && !isJSONNull(raw) {
		parsed, err := settingsIntValue(raw, true)
		if err != nil || parsed < 1 || parsed > 2000 {
			return []settingsInvalidParam{{Code: "too_small", Message: "maxJsonDepth 必须是 1..2000 的整数"}}
		}
	}
	if raw, ok := object["maxFixSize"]; ok && !isJSONNull(raw) {
		parsed, err := settingsIntValue(raw, true)
		if err != nil || parsed < 1024 || parsed > 10*1024*1024 {
			return []settingsInvalidParam{{Code: "too_small", Message: "maxFixSize 必须是 1024..10485760 的整数"}}
		}
	}
	return nil
}

// validateFakeStreamingWhitelist 校验白名单条目（Node 的 fakeStreamingWhitelist superRefine：
// 模型名非空且不重复）。
func validateFakeStreamingWhitelist(value json.RawMessage) []settingsInvalidParam {
	if isJSONNull(value) {
		return nil
	}
	var entries []map[string]json.RawMessage
	if err := json.Unmarshal(value, &entries); err != nil {
		return []settingsInvalidParam{{Code: "invalid_type", Message: "Expected array"}}
	}
	seen := make(map[string]bool, len(entries))
	for index, entry := range entries {
		rawModel, ok := entry["model"]
		if !ok {
			return []settingsInvalidParam{{
				Code: "invalid_type", Message: fmt.Sprintf("%d.model 不能为空", index),
			}}
		}
		var model string
		if err := json.Unmarshal(rawModel, &model); err != nil {
			return []settingsInvalidParam{{
				Code: "invalid_type", Message: fmt.Sprintf("%d.model 必须是字符串", index),
			}}
		}
		trimmed := strings.TrimSpace(model)
		if trimmed == "" {
			return []settingsInvalidParam{{
				Code: "custom", Message: fmt.Sprintf("%d.model 不能为空", index),
			}}
		}
		if len(trimmed) > 200 {
			return []settingsInvalidParam{{
				Code: "too_big", Message: fmt.Sprintf("%d.model 不能超过 200 个字符", index),
			}}
		}
		if seen[trimmed] {
			return []settingsInvalidParam{{
				Code: "custom", Message: "fakeStreamingWhitelist 模型重复: " + trimmed,
			}}
		}
		seen[trimmed] = true
	}
	return nil
}

// isJSONNull 判断原始值是否为 JSON null。
func isJSONNull(value json.RawMessage) bool {
	return len(value) == 0 || string(value) == "null"
}

// isIANATimezone 判断时区名是否被 tzdata 认识（Node 用 Intl.DateTimeFormat 试探）。
func isIANATimezone(name string) bool {
	if name == "" {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}

// keysOf 取集合的键（用于枚举错误消息里的允许值提示）。
func settingsKeysOf(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// containsString 判断切片是否含该值。
func settingsContainsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// auditRequestIP 取审计用客户端 IP（与 auth.go 的口径一致：X-Forwarded-For 首项，否则 RemoteAddr）。
func auditRequestIP(request *http.Request) string {
	if forwarded := request.Header.Get("X-Forwarded-For"); forwarded != "" {
		if first, _, found := strings.Cut(forwarded, ","); found {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(forwarded)
	}
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		return request.RemoteAddr
	}
	return host
}

// systemSettingsProjection 供其他模块（如 /api/admin/system-config）复用投影。
func systemSettingsProjection(row *store.AdminSystemSettings, now time.Time) SystemSettingsBody {
	return buildSystemSettingsBody(row, now)
}

// 供 /api/admin/system-config 复用的两条适配（避免在该文件里重复 decode 逻辑）。
func (api *systemSettingsAPI) legacyDecode(
	request *http.Request,
) (systemSettingsUpdate, []settingsInvalidParam) {
	return readSystemSettingsUpdate(request, settingsUpdateActionSchema)
}

// invalidateSettingsCaches 是 PUT 与旧端点共用的失效广播。
//
// 旧端点（POST /api/admin/system-config）照 Node 只清缓存、**不重发** public-status 投影
// （src/app/api/admin/system-config/route.ts:130-140 只有三族失效），故重发只在 PUT 里做。
func (api *systemSettingsAPI) invalidateSettingsCaches(ctx context.Context, timezoneTouched bool) {
	if api.invalidator != nil {
		api.invalidator.PublishDomain(ctx, cfgsync.DomainSystemSettings)
	}
	if !timezoneTouched {
		return
	}
	if api.dashboardCaches == nil {
		api.logger.Warn("admin_system_settings_timezone_caches_not_invalidated", map[string]any{
			"families": "overview,statistics,leaderboard",
			"reason":   "dashboard_cache_invalidator_unwired",
		})
		return
	}
	if err := api.dashboardCaches.InvalidateDashboardCaches(ctx); err != nil {
		api.logger.Warn("admin_system_settings_timezone_caches_invalidation_failed", map[string]any{
			"families": "overview,statistics,leaderboard",
			"error":    err.Error(),
		})
	}
}

// writeSettingsValidationProblem 作答「带错误码的 400 校验失败」。
//
// 为什么不直接用 Problems.WriteValidationError：那条路的 errorCode 恒为
// "request.validation_failed"，而 Node 的 v1 校验失败会把**族码**写进 errorCode
// （discovery 窗口/字段、replay TTL、legacy hedge），且设置页正是按 errorCode 选文案
// （settings/config/_components/system-settings-form.tsx:458-463）。丢掉族码 = 页面显示通用
// 错误；故这里按 Node 的 fromZodError（error-envelope.ts:40-62）逐字段自组正文。
func writeSettingsValidationProblem(
	writer http.ResponseWriter,
	request *http.Request,
	errorCode string,
	params []InvalidParam,
) {
	if errorCode == "" {
		errorCode = "request.validation_failed"
	}
	if params == nil {
		params = []InvalidParam{}
	}
	body := struct {
		Type          string         `json:"type"`
		Title         string         `json:"title"`
		Status        int            `json:"status"`
		Detail        string         `json:"detail"`
		Instance      string         `json:"instance"`
		ErrorCode     string         `json:"errorCode"`
		InvalidParams []InvalidParam `json:"invalidParams"`
	}{
		Type:          problemType(errorCode),
		Title:         validationFailedTitle,
		Status:        http.StatusBadRequest,
		Detail:        validationFailedDetail,
		Instance:      problemInstance(request),
		ErrorCode:     errorCode,
		InvalidParams: params,
	}
	header := writer.Header()
	header.Set("Content-Type", "application/problem+json")
	writer.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(writer).Encode(body)
}

// settingsValidationErrorCode 复刻 Node 的族码推导链
// （router.ts 的 defaultHook：discovery → replay → legacy hedge）。
func settingsValidationErrorCode(problems []settingsInvalidParam) string {
	discoveryPath := false
	for _, problem := range problems {
		for _, candidate := range []string{problem.Code, problem.Message} {
			switch candidate {
			case discoverySettingsInvalidErrorCode, discoveryWindowInvalidErrorCode,
				replayCacheTTLInvalidErrorCode, legacyHedgeInvalidErrorCode:
				return candidate
			}
		}
		if len(problem.Path) > 0 {
			if name, ok := problem.Path[0].(string); ok && discoveryFieldLimits[name] != [2]int{} {
				discoveryPath = true
			}
		}
	}
	if discoveryPath {
		return discoverySettingsInvalidErrorCode
	}
	return ""
}
