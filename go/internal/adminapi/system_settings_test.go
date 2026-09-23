package adminapi

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住 system_settings 的两件易错事：
//
//  1. **投影逐字段对齐 Node**：golden 不是手抄的，是用 bun 跑 Node 的 toSystemSettings
//     （src/repository/_shared/transformers.ts:246）喂同一份夹具生成的输出，逐字节比对。
//     手抄 golden 只能钉住「我以为 Node 会这么算」，钉不住 Node 的实际行为。
//  2. **部分更新的「出现」与「出现为 null」**：zod 的 .nullable().optional() 里 null 是有值的
//     清空指令，漏掉就会把「清空时区」当成「没提交」。
//
// systemSettingsFixtureProjection 是 Node 的 toSystemSettings 对 systemSettingsFixtureRow 的输出
// （bun 生成，逐字粘贴）。
//
// **与 Node 的有意偏离（2026-09-21）**：`affinityEnabled` **不在** Node 的投影里，是 Go 侧新增的。
// 原因：Node 把亲和的「总闸」与「忽略会话 ID」挤在同一字段 `affinityIgnoreClientSessionId`，
// 于是「让会话粘性生效」与「保持亲和可用」不可兼得——翻它的默认值会连带关掉整套亲和。
// Go 侧拆成两列：`affinity_enabled` 管总闸（见 drizzle/0132），
// `affinity_ignore_client_session_id` 只管模式。故本条 golden 需手工加此键，
// 它**不是** Node 行为的快照，而是有意偏离的声明；新增 Node 字段时应照旧逐字段对齐。
//
// **与 Node 的有意偏离（2026-09-22，第二处）**：`providerLiveStatsEnabled` 同样**不在** Node 的
// 投影里，是 Go 侧新增的（供应商实时并发统计的全局开关，见 drizzle/0135）。它同样是手工加的键，
// 不是 Node 行为的快照。
const systemSettingsFixtureProjection = `{
  "id": 7,
  "siteTitle": "Fixture Hub",
  "allowGlobalUsageView": false,
  "currencyDisplay": "CNY",
  "billingModelSource": "redirected",
  "codexPriorityBillingSource": "requested",
  "billNonSuccessfulRequests": true,
  "billHedgeLosers": false,
  "legacyHedgeMaxInFlight": 2,
  "timezone": "Asia/Shanghai",
  "enableAutoCleanup": true,
  "cleanupRetentionDays": 45,
  "cleanupSchedule": "0 3 * * *",
  "cleanupBatchSize": 5000,
  "enableClientVersionCheck": true,
  "verboseProviderError": true,
  "passThroughUpstreamErrorMessage": false,
  "enableHttp2": true,
  "enableOpenaiResponsesWebsocket": false,
  "enableHighConcurrencyMode": true,
  "interceptAnthropicWarmupRequests": true,
  "enableThinkingSignatureRectifier": false,
  "enableThinkingBudgetRectifier": false,
  "enableThinkingEffortConflictRectifier": false,
  "enableGeminiFunctionIdRectifier": false,
  "enableBillingHeaderRectifier": false,
  "enableResponseInputRectifier": false,
  "allowNonConversationEndpointProviderFallback": false,
  "fakeStreamingWhitelist": [{"model": "gpt-image", "groupTags": ["a", "b"]}, {"model": "x", "groupTags": []}],
  "enableCodexSessionIdCompletion": false,
  "enableClaudeMetadataUserIdInjection": false,
  "enableResponseFixer": false,
  "responseFixerConfig": {"fixTruncatedJson": true, "fixSseFormat": false, "fixEncoding": true, "maxJsonDepth": 5, "maxFixSize": 1048576},
  "quotaDbRefreshIntervalSeconds": 20,
  "quotaLeasePercent5h": 0.25,
  "quotaLeasePercentDaily": 0,
  "quotaLeasePercentWeekly": 0.05,
  "quotaLeasePercentMonthly": 0.5,
  "quotaLeaseCapUsd": 12.75,
  "publicStatusWindowHours": 72,
  "publicStatusAggregationIntervalMinutes": 15,
  "discoveryEnabled": true,
  "discoveryConcurrency": 4,
  "maxDiscoveryRounds": 3,
  "discoverySlaMs": 5000,
  "stickySlaMs": 30000,
  "racingTotalTimeoutMs": 90000,
  "stickyTimeoutCooldownMs": 1000,
  "ipExtractionConfig": {"headers": [{"name": "x-real-ip"}]},
  "ipGeoLookupEnabled": false,
  "streamGateMode": "enforce",
  "affinityIgnoreClientSessionId": false,
  "affinityEnabled": true,
  "providerLiveStatsEnabled": true,
  "replayEnabled": null,
  "replayCacheTtlMinutes": 30,
  "cacheEffectivenessEnabled": true,
  "createdAt": "2026-01-02T03:04:05.000Z",
  "updatedAt": "2026-02-03T04:05:06.000Z"
}`

// systemSettingsFixtureRow 是同一份夹具的库侧形状（列名即 JSON 键名）。
const systemSettingsFixtureRow = `{
  "id": 7,
  "site_title": "Fixture Hub",
  "allow_global_usage_view": false,
  "currency_display": "CNY",
  "billing_model_source": "redirected",
  "codex_priority_billing_source": "bogus",
  "bill_non_successful_requests": true,
  "bill_hedge_losers": false,
  "legacy_hedge_max_in_flight": 9,
  "timezone": "Asia/Shanghai",
  "enable_auto_cleanup": true,
  "cleanup_retention_days": 45,
  "cleanup_schedule": "0 3 * * *",
  "cleanup_batch_size": 5000,
  "enable_client_version_check": true,
  "verbose_provider_error": true,
  "pass_through_upstream_error_message": false,
  "enable_http2": true,
  "enable_openai_responses_websocket": false,
  "enable_high_concurrency_mode": true,
  "intercept_anthropic_warmup_requests": true,
  "enable_thinking_signature_rectifier": false,
  "enable_thinking_budget_rectifier": false,
  "enable_thinking_effort_conflict_rectifier": false,
  "enable_gemini_function_id_rectifier": false,
  "enable_billing_header_rectifier": false,
  "enable_response_input_rectifier": false,
  "allow_non_conversation_endpoint_provider_fallback": false,
  "fake_streaming_whitelist": [{"model": " gpt-image ", "groupTags": [" a ", "a", "", "b"]}, {"model": 3}, {"model": "x", "groupTags": "no"}],
  "enable_codex_session_id_completion": false,
  "enable_claude_metadata_user_id_injection": false,
  "enable_response_fixer": false,
  "response_fixer_config": {"fixSseFormat": false, "maxJsonDepth": 5},
  "quota_db_refresh_interval_seconds": 20,
  "quota_lease_percent_5h": "0.25",
  "quota_lease_percent_daily": "0",
  "quota_lease_percent_weekly": null,
  "quota_lease_percent_monthly": "0.5",
  "quota_lease_cap_usd": "12.75",
  "public_status_window_hours": 72,
  "public_status_aggregation_interval_minutes": 15,
  "discovery_enabled": true,
  "discovery_concurrency": 4,
  "max_discovery_rounds": 3,
  "discovery_sla_ms": 5000,
  "sticky_sla_ms": 30000,
  "racing_total_timeout_ms": 90000,
  "sticky_timeout_cooldown_ms": 1000,
  "ip_extraction_config": {"headers": [{"name": "x-real-ip"}]},
  "ip_geo_lookup_enabled": false,
  "stream_gate_mode": "enforce",
  "affinity_ignore_client_session_id": false,
  "affinity_enabled": true,
  "provider_live_stats_enabled": true,
  "replay_enabled": null,
  "replay_cache_ttl_minutes": 999,
  "cache_effectiveness_enabled": true,
  "created_at": "2026-01-02T03:04:05Z",
  "updated_at": "2026-02-03T04:05:06Z"
}`

// TestSystemSettingsProjectionMatchesNodeGolden 逐字段对拍 Node 的 toSystemSettings 产物。
//
// 用 map 比对而不是字符串比对：JSON 对象键序无语义，字符串比对会把「键写反」误报成不匹配；
// 而**键集**与**值**都要相等（golden 里多一个键、少一个键、类型不同都会红）。
func TestSystemSettingsProjectionMatchesNodeGolden(t *testing.T) {
	var row store.AdminSystemSettings
	if err := json.Unmarshal([]byte(systemSettingsFixtureRow), &row); err != nil {
		t.Fatalf("解析夹具行失败：%v", err)
	}
	now := time.Date(2026, 6, 7, 8, 9, 10, 0, time.UTC)
	actualRaw, err := json.Marshal(buildSystemSettingsBody(&row, now))
	if err != nil {
		t.Fatalf("序列化投影失败：%v", err)
	}
	var actual, expected map[string]any
	if err := json.Unmarshal(actualRaw, &actual); err != nil {
		t.Fatalf("解析投影失败：%v", err)
	}
	if err := json.Unmarshal([]byte(systemSettingsFixtureProjection), &expected); err != nil {
		t.Fatalf("解析 golden 失败：%v", err)
	}

	for key, want := range expected {
		got, ok := actual[key]
		if !ok {
			t.Errorf("投影缺字段 %s（Node 有）", key)
			continue
		}
		if !jsonValueEqual(got, want) {
			t.Errorf("字段 %s 不一致：Go=%v Node=%v", key, got, want)
		}
	}
	for key := range actual {
		if _, ok := expected[key]; !ok {
			t.Errorf("投影多出字段 %s（Node 没有）", key)
		}
	}
}

// TestSystemSettingsProjectionDefaults 钉住归一里的分支：只在库值不合法时生效的默认与枚举回退。
func TestSystemSettingsProjectionDefaults(t *testing.T) {
	now := time.Date(2026, 6, 7, 8, 9, 10, 0, time.UTC)
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	timezone := "Asia/Tokyo"
	replay := true
	row := &store.AdminSystemSettings{
		SiteTitle:                  "x",
		CurrencyDisplay:            "USD",
		BillingModelSource:         "original",
		CodexPriorityBillingSource: "actual",
		LegacyHedgeMaxInFlight:     0,
		Timezone:                   &timezone,
		StreamGateMode:             "bogus",
		ReplayEnabled:              &replay,
		ReplayCacheTTLMinutes:      1,
		CreatedAt:                  &created,
		UpdatedAt:                  &created,
	}
	body := buildSystemSettingsBody(row, now)

	if body.CodexPriorityBillingSource != "actual" {
		t.Errorf("合法枚举被改写：%s", body.CodexPriorityBillingSource)
	}
	if body.LegacyHedgeMaxInFlight != defaultLegacyHedgeMaxInFlight {
		t.Errorf("越界 legacyHedgeMaxInFlight 应回退 %d，得到 %d",
			defaultLegacyHedgeMaxInFlight, body.LegacyHedgeMaxInFlight)
	}
	if body.StreamGateMode != "enforce" {
		t.Errorf("非法 streamGateMode 应回退 enforce，得到 %s", body.StreamGateMode)
	}
	if body.ReplayCacheTTLMinutes != defaultReplayCacheTTLMinutes {
		t.Errorf("越界 replayCacheTtlMinutes 应回退 %d，得到 %d",
			defaultReplayCacheTTLMinutes, body.ReplayCacheTTLMinutes)
	}
	if body.QuotaLeasePercent5h != defaultQuotaLeasePercent {
		t.Errorf("缺值 quotaLeasePercent5h 应回退 %v，得到 %v", defaultQuotaLeasePercent, body.QuotaLeasePercent5h)
	}
	if body.QuotaLeaseCapUSD != nil {
		t.Errorf("缺值 quotaLeaseCapUsd 应为 null，得到 %v", *body.QuotaLeaseCapUSD)
	}
	if body.EnableAutoCleanup {
		t.Error("缺值 enableAutoCleanup 应为 false（Node 的 ?? false）")
	}
	if body.CleanupRetentionDays != defaultCleanupRetentionDays {
		t.Errorf("缺值 cleanupRetentionDays 应为 %d", defaultCleanupRetentionDays)
	}
	if body.CreatedAt != "2026-01-01T00:00:00.000Z" {
		t.Errorf("createdAt 形状应为 JS toJSON，得到 %s", body.CreatedAt)
	}
	if string(body.IPExtractionConfig) != "null" {
		t.Errorf("缺值 ipExtractionConfig 应为 JSON null，得到 %s", body.IPExtractionConfig)
	}
	if len(body.FakeStreamingWhitelist) != 0 {
		t.Errorf("缺值 fakeStreamingWhitelist 应为空数组，得到 %v", body.FakeStreamingWhitelist)
	}
}

// jsonValueEqual 比较两段 JSON 值：数字按 float64 比（Go 输出 0.05 与 golden 的 0.05 同值），
// 其余按结构递归比较（数组顺序有意义）。
func jsonValueEqual(left, right any) bool {
	leftNumber, leftIsNumber := left.(float64)
	rightNumber, rightIsNumber := right.(float64)
	if leftIsNumber || rightIsNumber {
		return leftIsNumber && rightIsNumber && math.Abs(leftNumber-rightNumber) < 1e-12
	}
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	return string(leftJSON) == string(rightJSON)
}

// TestSystemSettingsUpdateRejectsUnknownKeys 钉住 v1 的 strict 语义：未知键 400。
func TestSystemSettingsUpdateRejectsUnknownKeys(t *testing.T) {
	decoded, problems := decodeSystemSettingsUpdate(
		map[string]json.RawMessage{"nope": json.RawMessage("1")}, settingsUpdateAPISchema,
	)
	if len(problems) != 1 || problems[0].Code != "unrecognized_keys" {
		t.Fatalf("未知键应报 unrecognized_keys，得到 %+v", problems)
	}
	if len(decoded.patch().Updates) != 0 {
		t.Fatalf("未知键不得产生更新，得到 %v", decoded.patch().Columns())
	}
	// 旧端点（action schema）不 strict：同一个键只是被忽略。
	_, problems = decodeSystemSettingsUpdate(
		map[string]json.RawMessage{"nope": json.RawMessage("1")}, settingsUpdateActionSchema,
	)
	if len(problems) != 0 {
		t.Fatalf("action schema 应忽略未知键，得到 %+v", problems)
	}
}

// TestSystemSettingsUpdateErrorCodes 钉住族码推导：设置页按 errorCode 选文案。
func TestSystemSettingsUpdateErrorCodes(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		field   string
		wantRaw bool
	}{
		{"discovery 越界", `{"discoveryConcurrency": 1}`, "discoveryConcurrency", true},
		{"discovery 非整数", `{"maxDiscoveryRounds": 1.5}`, "maxDiscoveryRounds", true},
		{"replay TTL 越界", `{"replayCacheTtlMinutes": 1}`, "replayCacheTtlMinutes", true},
		{"legacy hedge 越界", `{"legacyHedgeMaxInFlight": 9}`, "legacyHedgeMaxInFlight", true},
		{"时区非法", `{"timezone": "Nowhere/NoCity"}`, "timezone", false},
		{"枚举非法", `{"streamGateMode": "loud"}`, "streamGateMode", false},
		{"影子模式已移除", `{"streamGateMode": "shadow"}`, "streamGateMode", false},
		{"类型错", `{"siteTitle": 5}`, "siteTitle", false},
		{"整流深度越界", `{"responseFixerConfig": {"maxJsonDepth": 0}}`, "responseFixerConfig", false},
		{"白名单模型空", `{"fakeStreamingWhitelist": [{"model": "  ", "groupTags": []}]}`, "fakeStreamingWhitelist", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var raw map[string]json.RawMessage
			if err := json.Unmarshal([]byte(testCase.body), &raw); err != nil {
				t.Fatalf("解析用例失败：%v", err)
			}
			decoded, problems := decodeSystemSettingsUpdate(raw, settingsUpdateAPISchema)
			if len(problems) == 0 {
				t.Fatalf("应报校验失败：%s", testCase.body)
			}
			if path, _ := problems[0].Path[0].(string); path != testCase.field {
				t.Fatalf("报错字段应为 %s，得到 %v", testCase.field, problems[0].Path)
			}
			if len(decoded.patch().Updates) != 0 {
				t.Fatalf("校验失败时不得产生更新：%v", decoded.patch().Columns())
			}
			code := settingsValidationErrorCode(problems)
			if testCase.wantRaw {
				want := map[string]string{
					"discoveryConcurrency":   discoverySettingsInvalidErrorCode,
					"maxDiscoveryRounds":     discoverySettingsInvalidErrorCode,
					"replayCacheTtlMinutes":  replayCacheTTLInvalidErrorCode,
					"legacyHedgeMaxInFlight": legacyHedgeInvalidErrorCode,
				}[testCase.field]
				if code != want {
					t.Fatalf("族码应为 %s，得到 %q（problems=%+v）", want, code, problems)
				}
				return
			}
			if code != "" {
				t.Fatalf("非族字段不应给族码，得到 %q", code)
			}
		})
	}
}

// TestSystemSettingsUpdateNullIsMeaningful 钉住 null 的清空语义。
func TestSystemSettingsUpdateNullIsMeaningful(t *testing.T) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(`{
		"timezone": null, "replayEnabled": null, "cacheEffectivenessEnabled": null,
		"ipExtractionConfig": null, "quotaLeaseCapUsd": null
	}`), &raw); err != nil {
		t.Fatalf("解析用例失败：%v", err)
	}
	decoded, problems := decodeSystemSettingsUpdate(raw, settingsUpdateAPISchema)
	if len(problems) != 0 {
		t.Fatalf("null 应当合法：%+v", problems)
	}
	patch := decoded.patch().Updates
	for column, value := range map[store.AdminSystemSettingsColumn]any{
		store.ColTimezone:                  nil,
		store.ColReplayEnabled:             nil,
		store.ColCacheEffectivenessEnabled: nil,
		store.ColIPExtractionConfig:        nil,
		store.ColQuotaLeaseCapUSD:          nil,
	} {
		got, ok := patch[column]
		if !ok {
			t.Fatalf("列 %s 应出现在更新里（null 是有值的清空指令）", column)
		}
		if got != value {
			t.Fatalf("列 %s 应写 NULL，得到 %v", column, got)
		}
	}
}

// TestSystemSettingsDiscoveryWindowInvariant 钉住竞速窗口不变量（生效值 = 本次 ?? 当前 ?? 默认）。
func TestSystemSettingsDiscoveryWindowInvariant(t *testing.T) {
	current := buildSystemSettingsBody(&store.AdminSystemSettings{
		DiscoverySLAMS:       defaultDiscoverySLAMS,
		StickySLAMS:          defaultStickySLAMS,
		MaxDiscoveryRounds:   defaultMaxDiscoveryRounds,
		RacingTotalTimeoutMS: defaultRacingTotalTimeoutMS,
	}, time.Now())
	if !systemSettingsDiscoveryWindowValid(systemSettingsUpdate{}, current) {
		t.Fatal("出厂默认应满足窗口不变量")
	}

	tooSmall := 1
	decoded := systemSettingsUpdate{racingTotalTimeout: &tooSmall, racingTotalPresent: true}
	if systemSettingsDiscoveryWindowValid(decoded, current) {
		t.Fatal("racingTotalTimeoutMs=1 应违反窗口不变量")
	}

	// 只改 discoverySlaMs 时也要用**当前**的其余三值重算：
	// 60000 < 20000 + 2*25000 故必须判为违约。
	bigSLA := 25_000
	decoded = systemSettingsUpdate{discoverySLA: &bigSLA, discoverySLAMPresent: true}
	if systemSettingsDiscoveryWindowValid(decoded, current) {
		t.Fatal("discoverySlaMs=25000 与当前 sticky/rounds 组合应违约")
	}
}

// TestSystemSettingsPutHTTPShapes 用桩依赖跑 PUT 的三条作答路径（无库：失败路径在读库之前就返回）。
func TestSystemSettingsPutHTTPShapes(t *testing.T) {
	api := &systemSettingsAPI{
		problems: NewProblems(nil),
		logger:   nil,
		now:      time.Now,
	}
	// logger 为 nil 时写日志会 panic，故这里用真实 logger 替身（NewProblems 已持有它）。
	api.logger = logx.New(nil)

	request := httptest.NewRequest(http.MethodPut, "/api/v1/system/settings",
		strings.NewReader(`{"nope": 1}`))
	recorder := httptest.NewRecorder()
	api.handleSettingsPut(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("未知键应 400，得到 %d", recorder.Code)
	}
	var body struct {
		Title         string `json:"title"`
		ErrorCode     string `json:"errorCode"`
		InvalidParams []struct {
			Code string `json:"code"`
		} `json:"invalidParams"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败：%v（body=%s）", err, recorder.Body.String())
	}
	if body.Title != validationFailedTitle {
		t.Errorf("title 应为 %q，得到 %q", validationFailedTitle, body.Title)
	}
	if body.ErrorCode != "request.validation_failed" {
		t.Errorf("无族码时 errorCode 应为 request.validation_failed，得到 %q", body.ErrorCode)
	}
	if len(body.InvalidParams) != 1 || body.InvalidParams[0].Code != "unrecognized_keys" {
		t.Errorf("invalidParams 应含 unrecognized_keys，得到 %+v", body.InvalidParams)
	}

	// 族码路径：discovery 越界必须把族码写进 errorCode（设置页按它选文案）。
	request = httptest.NewRequest(http.MethodPut, "/api/v1/system/settings",
		strings.NewReader(`{"discoveryConcurrency": 1}`))
	recorder = httptest.NewRecorder()
	api.handleSettingsPut(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("discovery 越界应 400，得到 %d", recorder.Code)
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	if body.ErrorCode != discoverySettingsInvalidErrorCode {
		t.Errorf("errorCode 应为 %s，得到 %q", discoverySettingsInvalidErrorCode, body.ErrorCode)
	}
}
