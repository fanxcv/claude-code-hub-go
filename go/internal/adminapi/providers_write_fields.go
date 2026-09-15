package adminapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 providers_write.go 的**字段解码 / 归一 / 撤销前像**部分。
//
// 三张表的用途：
//   - providerCreateWriteSpecs / providerUpdateWriteSpecs：请求体字段 → 存储层字段袋的绑定类型，
//     同时承担「未知字段拒绝」（Node 的 zod `.strict()`）；
//   - providerPreimageFieldNames：Node 的 payload 名 ↔ Provider 的 camelCase 名
//     （逐条取自 actions/providers.ts:1481-1541 的 SINGLE_EDIT_PREIMAGE_FIELD_TO_PROVIDER_KEY）；
//   - providerModelRedirectMatchTypes：model_redirects / allowed_models 的合法 matchType。

// providerModelRedirectMatchTypes 是 model_redirects 与 allowed_models 的 matchType 取值域。
var providerModelRedirectMatchTypes = []string{"exact", "prefix", "suffix", "contains", "regex"}

// 各偏好列的取值域。
//
// 逐条取自 Node 的权威定义——`provider-patch-contract.ts` 的 isValidSetValue 分支与
// `types/provider.ts` 的联合类型（两者同值）；Go 的 batch patch 契约已用同一批字面量做校验，
// 这里提成命名变量供 REST 创建/更新路径复用，避免同一套枚举出现两份写法。
var (
	providerCacheTTLPreferences     = []string{"inherit", "5m", "1h"}
	providerContext1mPreferences    = []string{"inherit", "force_enable", "disabled"}
	providerCodexReasoningEfforts   = []string{"inherit", "none", "minimal", "low", "medium", "high", "xhigh", "max"}
	providerCodexReasoningSummaries = []string{"inherit", "auto", "detailed"}
	providerCodexTextVerbosities    = []string{"inherit", "low", "medium", "high"}
	// 布尔偏好用 "true"/"false" 字符串（Node 的 Select 值必须是字符串）。
	providerCodexBoolPreferences  = []string{"inherit", "true", "false"}
	providerCodexImageGenerations = providerCodexBoolPreferences
	providerCodexServiceTiers     = []string{"inherit", "auto", "default", "flex", "priority"}
	providerGeminiGoogleSearches  = []string{"inherit", "enabled", "disabled"}
)

// providerPreimageFieldNames 是 payload 名 → Provider（camelCase）名。
//
// 逐条取自 Node 的 SINGLE_EDIT_PREIMAGE_FIELD_TO_PROVIDER_KEY。camelCase 名与
// store.AdminProvider 的 json 标签一致，故前像可以直接从该结构体序列化后按名取值。
var providerPreimageFieldNames = map[string]string{
	"name": "name", "url": "url", "is_enabled": "isEnabled", "weight": "weight",
	"priority": "priority", "cost_multiplier": "costMultiplier", "group_tag": "groupTag",
	"group_priorities": "groupPriorities", "provider_type": "providerType",
	"preserve_client_ip": "preserveClientIp", "disable_session_reuse": "disableSessionReuse",
	"active_time_start": "activeTimeStart", "active_time_end": "activeTimeEnd",
	"model_redirects": "modelRedirects", "allowed_models": "allowedModels",
	"allowed_clients": "allowedClients", "blocked_clients": "blockedClients",
	"limit_5h_usd": "limit5hUsd", "limit_5h_reset_mode": "limit5hResetMode",
	"limit_daily_usd": "limitDailyUsd", "daily_reset_mode": "dailyResetMode",
	"daily_reset_time": "dailyResetTime", "limit_weekly_usd": "limitWeeklyUsd",
	"limit_monthly_usd": "limitMonthlyUsd", "limit_total_usd": "limitTotalUsd",
	"limit_concurrent_sessions":                   "limitConcurrentSessions",
	"cache_ttl_preference":                        "cacheTtlPreference",
	"swap_cache_ttl_billing":                      "swapCacheTtlBilling",
	"context_1m_preference":                       "context1mPreference",
	"codex_reasoning_effort_preference":           "codexReasoningEffortPreference",
	"codex_reasoning_summary_preference":          "codexReasoningSummaryPreference",
	"codex_text_verbosity_preference":             "codexTextVerbosityPreference",
	"codex_parallel_tool_calls_preference":        "codexParallelToolCallsPreference",
	"codex_image_generation_preference":           "codexImageGenerationPreference",
	"codex_service_tier_preference":               "codexServiceTierPreference",
	"codex_max_tokens_preference":                 "codexMaxTokensPreference",
	"anthropic_max_tokens_preference":             "anthropicMaxTokensPreference",
	"anthropic_thinking_budget_preference":        "anthropicThinkingBudgetPreference",
	"anthropic_adaptive_thinking":                 "anthropicAdaptiveThinking",
	"openai_max_tokens_preference":                "openaiMaxTokensPreference",
	"gemini_google_search_preference":             "geminiGoogleSearchPreference",
	"max_retry_attempts":                          "maxRetryAttempts",
	"circuit_breaker_failure_threshold":           "circuitBreakerFailureThreshold",
	"circuit_breaker_open_duration":               "circuitBreakerOpenDuration",
	"circuit_breaker_half_open_success_threshold": "circuitBreakerHalfOpenSuccessThreshold",
	"circuit_breaker_release_increment":           "circuitBreakerReleaseIncrement",
	"circuit_breaker_max_open_count":              "circuitBreakerMaxOpenCount",
	"proxy_url":                                   "proxyUrl",
	"proxy_fallback_to_direct":                    "proxyFallbackToDirect",
	"custom_headers":                              "customHeaders",
	"first_byte_timeout_streaming_ms":             "firstByteTimeoutStreamingMs",
	"streaming_idle_timeout_ms":                   "streamingIdleTimeoutMs",
	"request_timeout_non_streaming_ms":            "requestTimeoutNonStreamingMs",
	"website_url":                                 "websiteUrl",
	"favicon_url":                                 "faviconUrl",
	"mcp_passthrough_type":                        "mcpPassthroughType",
	"mcp_passthrough_url":                         "mcpPassthroughUrl",
	"protocol_conversion_enabled":                 "protocolConversionEnabled",
	"tpm":                                         "tpm",
	"rpm":                                         "rpm",
	"rpd":                                         "rpd",
	"cc":                                          "cc",
}

// providerPayloadFieldForCamel 反向查表（撤销回写时要把前像的 camelCase 名翻译回 payload 名）。
func providerPayloadFieldForCamel(camel string) (string, bool) {
	if camel == "description" {
		return "description", true
	}
	for payload, name := range providerPreimageFieldNames {
		if name == camel {
			return payload, true
		}
	}
	return "", false
}

// providerDecodeSpec 是一个字段的解码方式。
type providerDecodeSpec struct {
	// Decode 读出字段值（存储层字段袋的类型）；第二个返回值表示字段是否被给出。
	Decode func(object *adminObject, payload string) (any, bool)
}

// providerTextSpec 复刻 `z.string().trim().min(1).max(n)` 一类的必填文本。
func providerTextFieldSpec(maxRunes int, enum []string) providerDecodeSpec {
	return providerDecodeSpec{Decode: func(object *adminObject, payload string) (any, bool) {
		value, present := object.String(payload, adminStringSpec{MaxRunes: maxRunes, Enum: enum})
		if !present {
			return nil, false
		}
		return value, true
	}}
}

// providerNullableFieldSpec 复刻 `z.string()....nullable().optional()`。
func providerNullableFieldSpec(maxRunes int) providerDecodeSpec {
	return providerDecodeSpec{Decode: func(object *adminObject, payload string) (any, bool) {
		nullable, present := providerNullableString(object, payload, maxRunes, []any{payload})
		if !present {
			return nil, false
		}
		if nullable.Value == nil {
			return (*string)(nil), true
		}
		return nullable.Value, true
	}}
}

// providerNullableEnumFieldSpec 复刻 `z.enum([...]).nullable().optional()`。
//
// 枚举值由调用方从本包既有的 batch 契约校验器取（同一批取值域，不再写第三份字面量）。
// 走 object.String 的 Enum 分支：错误码与文案就是 zod 的 `invalid_enum_value` 风格。
//
// 返回值：null → `(*string)(nil)`，有值 → `*string`——这些列在 store 侧是 providerNullableTextKind，
// 只认 `*string`（裸 string 会在绑定时报「值类型不符」，2026-09-15 生产事故即此）。
func providerNullableEnumFieldSpec(enum []string) providerDecodeSpec {
	return providerDecodeSpec{Decode: func(object *adminObject, payload string) (any, bool) {
		raw, present := object.Raw(payload)
		if !present {
			return nil, false
		}
		if adminJSONTypeName(raw) == "null" {
			return (*string)(nil), true
		}
		value, ok := object.String(payload, adminStringSpec{Enum: enum})
		if !ok {
			return nil, true
		}
		// 可空文本列的绑定形状是 *string（见 store.adminProviderBindValue）；
		// 返回裸 string 会让整条写路径在存储层被「值类型不符」拦下。
		return &value, true
	}}
}

// providerNullablePreferenceSpec 是「可空文本 + 取值判定」的通用形状（枚举以外的偏好：
// 数字串与结构校验）。
//
// 为什么要在**写侧**拦：这些偏好列此前只是「任意字符串」直通，而数据面按枚举 / 数字读。
// 落一个读不出来的值，用户看到的是「我配了却不生效」而没有任何提示（同
// providerNumberRecordJSONSpec 的理由）。与 Node 的关系：Node 的 batch patch 契约
// （`provider-patch-contract.ts`）校验同一批取值域，而其 REST 创建/更新 schema
// （`schemas/providers.ts`）对多数偏好列只写 `z.string()`——只有
// `codex_image_generation_preference` 在 REST 侧也是 `z.enum`。即：本改动对多数列
// **比 Node 的 REST 路径严**，而取值域与 Node 的权威定义（`types/provider.ts` 与 batch 契约）
// 一致，故 UI 能发出的值一个都不会被拒。
//
// 返回值形状同 providerNullableEnumFieldSpec：可空文本列要 `*string`。
func providerNullablePreferenceSpec(validate providerPatchSetValidator, expectation string) providerDecodeSpec {
	return providerDecodeSpec{Decode: func(object *adminObject, payload string) (any, bool) {
		raw, present := object.Raw(payload)
		if !present {
			return nil, false
		}
		if adminJSONTypeName(raw) == "null" {
			return (*string)(nil), true
		}
		value, ok := object.String(payload, adminStringSpec{})
		if !ok {
			return nil, true
		}
		if !validate(value) {
			object.fail([]any{payload}, "invalid_value",
				fmt.Sprintf("Invalid value: expected %s, received '%s'", expectation, value))
			return nil, true
		}
		// 同上：可空文本列要 *string。
		return &value, true
	}}
}

// providerAdaptiveThinkingSpec 校验 anthropic_adaptive_thinking 的 jsonb 形状。
//
// Node 的 REST schema 对它是 `z.unknown()`（不校验），batch 契约才校验结构；Go 此前两者都不校验
// （jsonb 直通），而数据面本轮开始按 `{effort, modelMatchMode, models}` 读它：落一个读不出的
// 形状就是「配了不生效」。这里复用 batch 契约的同一校validator，两个写路径口径一致。
func providerAdaptiveThinkingSpec() providerDecodeSpec {
	return providerDecodeSpec{Decode: func(object *adminObject, payload string) (any, bool) {
		raw, present := object.Raw(payload)
		if !present {
			return nil, false
		}
		if adminJSONTypeName(raw) == "null" {
			return nil, true
		}
		var decoded any
		if err := json.Unmarshal(raw, &decoded); err != nil || !validateAdaptiveThinking(decoded) {
			object.fail([]any{payload}, "invalid_value",
				"Invalid value: adaptive thinking requires effort (low|medium|high|xhigh|max), "+
					"modelMatchMode (specific|all) and a string array models")
			return nil, true
		}
		return json.RawMessage(raw), true
	}}
}

// providerBoolFieldSpec 复刻 `z.boolean().optional()`。
func providerBoolFieldSpec() providerDecodeSpec {
	return providerDecodeSpec{Decode: func(object *adminObject, payload string) (any, bool) {
		value, present := object.Bool(payload)
		if !present {
			return nil, false
		}
		return *value, true
	}}
}

// providerIntFieldSpec 复刻 `z.number().int()`（required 决定是否允许缺失）。
func providerIntFieldSpec(required bool, min, max *int64) providerDecodeSpec {
	return providerDecodeSpec{Decode: func(object *adminObject, payload string) (any, bool) {
		number, present := object.Number(payload, required, []any{payload})
		if !present {
			return nil, false
		}
		if number == nil {
			return nil, true
		}
		value := int64(*number)
		if min != nil && value < *min {
			object.fail([]any{payload}, "too_small",
				fmt.Sprintf("Number must be greater than or equal to %d", *min))
			return nil, true
		}
		if max != nil && value > *max {
			object.fail([]any{payload}, "too_big",
				fmt.Sprintf("Number must be less than or equal to %d", *max))
			return nil, true
		}
		return value, true
	}}
}

// providerNullableIntFieldSpec 复刻 `z.number().int().min(?).max(?).nullable().optional()`。
//
// min/max 为 nil 表示该侧不设界（与 Node 一致：schema 未写 .min()/.max() 就不校验）。
// null 绑定为 NULL —— 仅适用于**可空列**；NOT NULL 列用 providerTimeoutFieldSpec。
func providerNullableIntFieldSpec(min, max *int64) providerDecodeSpec {
	return providerDecodeSpec{Decode: func(object *adminObject, payload string) (any, bool) {
		raw, present := object.Raw(payload)
		if !present {
			return nil, false
		}
		if adminJSONTypeName(raw) == "null" {
			return (*int64)(nil), true
		}
		number, present := object.Number(payload, false, []any{payload})
		if !present || number == nil {
			return (*int64)(nil), true
		}
		value := int64(*number)
		if min != nil && value < *min {
			object.fail([]any{payload}, "too_small",
				fmt.Sprintf("Number must be greater than or equal to %d", *min))
			return nil, true
		}
		if max != nil && value > *max {
			object.fail([]any{payload}, "too_big",
				fmt.Sprintf("Number must be less than or equal to %d", *max))
			return nil, true
		}
		return &value, true
	}}
}

// providerTimeoutFieldSpec 复刻 `z.number().int().min(0).nullable().optional()`
// **落在 NOT NULL 列上**时的语义。
//
// 为何 null 不落 NULL：三个超时列（first_byte_timeout_streaming_ms / streaming_idle_timeout_ms /
// request_timeout_non_streaming_ms）在库里是 NOT NULL DEFAULT 0，而 Node 创建时是
// `providerData.<field> ?? PROVIDER_TIMEOUT_DEFAULTS.*`（三个默认值都是 0）——即 null 等价于「用默认 0」。
// 若照可空处理，UPDATE 会写 NULL 撞 NOT NULL 约束，把「校验 400」换成「500」。
func providerTimeoutFieldSpec(min, max *int64) providerDecodeSpec {
	return providerDecodeSpec{Decode: func(object *adminObject, payload string) (any, bool) {
		raw, present := object.Raw(payload)
		if !present {
			return nil, false
		}
		if adminJSONTypeName(raw) == "null" {
			return int64(0), true
		}
		number, present := object.Number(payload, false, []any{payload})
		if !present || number == nil {
			return int64(0), true
		}
		value := int64(*number)
		if min != nil && value < *min {
			object.fail([]any{payload}, "too_small",
				fmt.Sprintf("Number must be greater than or equal to %d", *min))
			return nil, true
		}
		if max != nil && value > *max {
			object.fail([]any{payload}, "too_big",
				fmt.Sprintf("Number must be less than or equal to %d", *max))
			return nil, true
		}
		return value, true
	}}
}

// providerNumericFieldSpec 复刻 `z.number().min(0).nullable().optional()`（numeric 列）。
func providerNumericFieldSpec(min *float64) providerDecodeSpec {
	return providerDecodeSpec{Decode: func(object *adminObject, payload string) (any, bool) {
		nullable, present := providerNullableNumber(object, payload, []any{payload})
		if !present {
			return nil, false
		}
		if nullable.Value == nil {
			return (*float64)(nil), true
		}
		if min != nil && *nullable.Value < *min {
			object.fail([]any{payload}, "too_small",
				fmt.Sprintf("Number must be greater than or equal to %v", *min))
			return nil, true
		}
		return nullable.Value, true
	}}
}

// providerJSONFieldSpec 直接把 jsonb 字段的原始 JSON 传下去（归一由调用方决定）。
func providerJSONFieldSpec() providerDecodeSpec {
	return providerDecodeSpec{Decode: func(object *adminObject, payload string) (any, bool) {
		return providerNullableRaw(object, payload, []any{payload})
	}}
}

// providerStringArrayJSONSpec 把字符串数组编成 jsonb（allowed_clients / blocked_clients 列是 jsonb）。
func providerStringArrayJSONSpec() providerDecodeSpec {
	return providerDecodeSpec{Decode: func(object *adminObject, payload string) (any, bool) {
		values, present := object.StringArray(payload)
		if !present {
			return nil, false
		}
		if values == nil {
			values = []string{}
		}
		encoded, err := json.Marshal(values)
		if err != nil {
			object.fail([]any{payload}, "invalid_type", "Expected string array")
			return nil, true
		}
		return json.RawMessage(encoded), true
	}}
}

// providerNumberRecordJSONSpec 校验「分组优先级覆盖」是整数记录（Node 的 zod 口径）。
//
// 口径对齐 Node 的 REST schema（src/lib/api/v1/schemas/providers.ts:36：
// `groupPriorities: z.record(z.string(), z.number()).nullable()`）：顶层必须是对象
// （或 null / 缺省），每个值必须是数字。
//
// 为什么非校验不可：列是 jsonb，写入侧原本只是「原样透传」（providerJSONFieldSpec），
// 于是字符串数字、数组、标量都能落库——**而选路侧是按整数读的**，落进去就读不出来，
// 用户只会看到「我配了却不生效」而没有任何提示。
//
// 与 Node 的两处**有意差异**（都比 Node 严，理由都是「不接受自己读不了的值」）：
//   - 值必须是**整数**：Node 的 `z.number()` 放行 2.5 / 2.0，而选路侧的 `int` 解码只接
//     整数（实测：`2.0` / `2e0` / `0.5` 会解失败），放进来就是一行读不出的覆盖；
//   - 值不得为 null：Node 也拒 null；这里同样拒，因为 JSON null 在既有读取语义里等价于
//     **0**（最高优先级），把「清空覆盖」写成 null 会得到「最优先」的反直觉结果。
func providerNumberRecordJSONSpec() providerDecodeSpec {
	return providerDecodeSpec{Decode: func(object *adminObject, payload string) (any, bool) {
		raw, present := object.Raw(payload)
		if !present {
			return nil, false
		}
		switch adminJSONTypeName(raw) {
		case "null", "undefined":
			return nil, true
		case "object":
		default:
			object.fail([]any{payload}, "invalid_type", "Expected object map of integers")
			return nil, true
		}
		var entries map[string]json.RawMessage
		if err := json.Unmarshal(raw, &entries); err != nil {
			object.fail([]any{payload}, "invalid_type", "Expected object map of integers")
			return nil, true
		}
		for key, value := range entries {
			// 用 int 解：接受集与选路侧的解码器**逐字相同**，因此这里放行的值一定读得出来。
			var number int
			if adminJSONTypeName(value) == "null" || json.Unmarshal(value, &number) != nil {
				object.fail([]any{payload, key}, "invalid_type", "Expected integer")
				return nil, true
			}
		}
		return raw, true
	}}
}

// providerCreateWriteSpecs 是创建请求的字段表（Node 的 ProviderCreateSchema）。
func providerCreateWriteSpecs() map[string]providerDecodeSpec {
	maxWeight := int64(100)
	minPriority := int64(0)
	minRetry := int64(1)
	maxRetry := int64(10)
	minCost := 0.0
	minTimeout := int64(0)
	return map[string]providerDecodeSpec{
		// name / url / key 的三段校验在 providerValidateCreate 里（需要跨字段与格式判定）。
		"name":                        providerDecodeSpec{Decode: providerRawStringDecode},
		"url":                         providerDecodeSpec{Decode: providerRawStringDecode},
		"key":                         providerDecodeSpec{Decode: providerRawStringDecode},
		"is_enabled":                  providerBoolFieldSpec(),
		"weight":                      providerIntFieldSpec(false, nil, &maxWeight),
		"priority":                    providerIntFieldSpec(false, &minPriority, nil),
		"cost_multiplier":             providerNumericFieldSpec(&minCost),
		"group_tag":                   providerNullableFieldSpec(255),
		"group_priorities":            providerNumberRecordJSONSpec(),
		"provider_type":               providerTextFieldSpec(0, providerPublicTypes),
		"preserve_client_ip":          providerBoolFieldSpec(),
		"disable_session_reuse":       providerBoolFieldSpec(),
		"model_redirects":             providerJSONFieldSpec(),
		"active_time_start":           providerNullableFieldSpec(0),
		"active_time_end":             providerNullableFieldSpec(0),
		"allowed_models":              providerJSONFieldSpec(),
		"allowed_clients":             providerStringArrayJSONSpec(),
		"blocked_clients":             providerStringArrayJSONSpec(),
		"mcp_passthrough_type":        providerTextFieldSpec(0, []string{"none", "minimax", "glm", "custom"}),
		"mcp_passthrough_url":         providerNullableFieldSpec(512),
		"protocol_conversion_enabled": providerBoolFieldSpec(),
		"limit_5h_usd":                providerNumericFieldSpec(&minCost),
		"limit_5h_reset_mode":         providerTextFieldSpec(0, providerResetModes),
		"limit_daily_usd":             providerNumericFieldSpec(&minCost),
		"daily_reset_mode":            providerTextFieldSpec(0, providerResetModes),
		"daily_reset_time":            providerTextFieldSpec(0, nil),
		"limit_weekly_usd":            providerNumericFieldSpec(&minCost),
		"limit_monthly_usd":           providerNumericFieldSpec(&minCost),
		"limit_total_usd":             providerNumericFieldSpec(&minCost),
		"limit_concurrent_sessions":   providerNullableIntFieldSpec(nil, nil),
		// max_retry_attempts：Node 是 `z.number().int().min(1).max(10).nullable().optional()`，
		// 且 providers.max_retry_attempts 是可空列（无默认）——UI 空值就送 null（表单类型 `number | null`）。
		// 曾误用非空 spec → 用户保存时报 invalid_type「Expected number, received null」。
		"max_retry_attempts":                          providerNullableIntFieldSpec(&minRetry, &maxRetry),
		"circuit_breaker_failure_threshold":           providerIntFieldSpec(false, nil, nil),
		"circuit_breaker_open_duration":               providerIntFieldSpec(false, nil, nil),
		"circuit_breaker_half_open_success_threshold": providerIntFieldSpec(false, nil, nil),
		// 等待阶梯两列是可空（无默认）且可空值就是「不启用」：空值送 null（表单类型 `number | null`），
		// 用非空 spec 会让「清空输入框」变成 invalid_type。上限只管不让负值进来。
		"circuit_breaker_release_increment":    providerNullableIntFieldSpec(nil, nil),
		"circuit_breaker_max_open_count":       providerNullableIntFieldSpec(nil, nil),
		"proxy_url":                            providerNullableFieldSpec(0),
		"proxy_fallback_to_direct":             providerBoolFieldSpec(),
		"custom_headers":                       providerCustomHeadersSpec(),
		"first_byte_timeout_streaming_ms":      providerTimeoutFieldSpec(&minTimeout, nil),
		"streaming_idle_timeout_ms":            providerTimeoutFieldSpec(&minTimeout, nil),
		"request_timeout_non_streaming_ms":     providerTimeoutFieldSpec(&minTimeout, nil),
		"website_url":                          providerNullableFieldSpec(0),
		"favicon_url":                          providerNullableFieldSpec(0),
		"cache_ttl_preference":                 providerNullableEnumFieldSpec(providerCacheTTLPreferences),
		"swap_cache_ttl_billing":               providerBoolFieldSpec(),
		"context_1m_preference":                providerNullableEnumFieldSpec(providerContext1mPreferences),
		"codex_reasoning_effort_preference":    providerNullableEnumFieldSpec(providerCodexReasoningEfforts),
		"codex_reasoning_summary_preference":   providerNullableEnumFieldSpec(providerCodexReasoningSummaries),
		"codex_text_verbosity_preference":      providerNullableEnumFieldSpec(providerCodexTextVerbosities),
		"codex_parallel_tool_calls_preference": providerNullableEnumFieldSpec(providerCodexBoolPreferences),
		"codex_image_generation_preference":    providerNullableEnumFieldSpec(providerCodexImageGenerations),
		"codex_service_tier_preference":        providerNullableEnumFieldSpec(providerCodexServiceTiers),
		"codex_max_tokens_preference":          providerNullableFieldSpec(0),
		"anthropic_max_tokens_preference": providerNullablePreferenceSpec(
			validateMaxTokensPreference, "inherit or a positive integer string"),
		"anthropic_thinking_budget_preference": providerNullablePreferenceSpec(
			validateThinkingBudgetPreference, "inherit or an integer string in 1024..32000"),
		"anthropic_adaptive_thinking":     providerAdaptiveThinkingSpec(),
		"openai_max_tokens_preference":    providerNullableFieldSpec(0),
		"gemini_google_search_preference": providerNullableEnumFieldSpec(providerGeminiGoogleSearches),
		"tpm":                             providerNullableIntFieldSpec(nil, nil),
		"rpm":                             providerNullableIntFieldSpec(nil, nil),
		"rpd":                             providerNullableIntFieldSpec(nil, nil),
		"cc":                              providerNullableIntFieldSpec(nil, nil),
	}
}

// providerUpdateWriteSpecs 是更新请求的字段表：创建字段全集 + description。
func providerUpdateWriteSpecs() map[string]providerDecodeSpec {
	specs := providerCreateWriteSpecs()
	specs["description"] = providerNullableFieldSpec(0)
	return specs
}

// providerRawStringDecode 读一个不做 trim 判定、也不限长的字符串（name/url/key 的细则在后一阶段校验）。
func providerRawStringDecode(object *adminObject, payload string) (any, bool) {
	raw, present := object.Raw(payload)
	if !present {
		return nil, false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		object.fail([]any{payload}, "invalid_type", adminTypeMessage("string", raw))
		return nil, true
	}
	return value, true
}

// providerDecodeWriteFields 按字段表解码请求体，产出存储层字段袋。
//
// 同时完成三件事：
//  1. 未知字段拒绝（zod `.strict()`）：**已知但本轮未移植**的字段给出指名错误
//     providerWriteUnsupportedFields），其余按未知字段处理；
//  2. 归一（group_tag、model_redirects、allowed_models 与 Node 的 normalize* 系列对齐）；
//  3. 默认值补齐由调用方按「创建 vs 更新」决定，这里不填。
func providerDecodeWriteFields(
	object *adminObject,
	names []string,
	specs map[string]providerDecodeSpec,
) (map[string]any, []invalidParam) {
	payload := make(map[string]any, len(names))
	for _, name := range names {
		if _, unsupported := providerWriteUnsupportedFields[name]; unsupported {
			object.fail([]any{name}, "unrecognized_keys", "Unsupported field: "+name)
			continue
		}
		spec, ok := specs[name]
		if !ok {
			object.fail([]any{name}, "unrecognized_keys", "Unrecognized key: "+name)
			continue
		}
		value, present := spec.Decode(object, name)
		if !present {
			continue
		}
		payload[name] = value
	}
	if issues := object.issues0(); len(issues) > 0 {
		return nil, issues
	}
	return providerNormalizeWriteFields(payload), nil
}

// providerNormalizeWriteFields 对齐 Node 的字段归一（repository/provider.ts 的 createProvider /
// updateProvider 与 lib/utils/provider-group.ts、lib/provider-model-redirects.ts、lib/allowed-model-rules.ts）。
func providerNormalizeWriteFields(payload map[string]any) map[string]any {
	if value, ok := payload["group_tag"]; ok {
		payload["group_tag"] = providerNormalizeGroupTag(value)
	}
	if value, ok := payload["model_redirects"]; ok {
		payload["model_redirects"] = providerNormalizeModelRedirects(value)
	}
	if value, ok := payload["allowed_models"]; ok {
		payload["allowed_models"] = providerNormalizeAllowedModels(value)
	}
	return payload
}

// providerNormalizeGroupTag 复刻 normalizeProviderGroupTag：按中英文逗号与换行切分、trim、去重后
// 用英文逗号重连；空结果转 null。
func providerNormalizeGroupTag(value any) any {
	switch typed := value.(type) {
	case nil:
		return (*string)(nil)
	case *string:
		if typed == nil {
			return (*string)(nil)
		}
		groups := providerSplitGroupValue(*typed)
		if len(groups) == 0 {
			return (*string)(nil)
		}
		joined := strings.Join(groups, ",")
		return &joined
	}
	return value
}

// providerSplitGroupValue 复刻 splitProviderGroupValue：逗号 / 中文逗号 / 换行分隔，trim 后去空去重。
func providerSplitGroupValue(value string) []string {
	fields := strings.FieldsFunc(value, func(char rune) bool {
		return char == ',' || char == '，' || char == '\n' || char == '\r'
	})
	seen := make(map[string]struct{}, len(fields))
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		trimmed := strings.TrimSpace(field)
		if trimmed == "" {
			continue
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	return out
}

// providerNormalizeModelRedirects 复刻 normalizeProviderModelRedirectRules。
//
// 两种输入形式都要支持：对象映射 `{source: target}`（旧 UI）与规则数组。非法输入一律 null
// （Node 同：`isRecord` 判定失败即 null）。
func providerNormalizeModelRedirects(value any) any {
	raw, ok := value.(json.RawMessage)
	if !ok || len(raw) == 0 || adminJSONTypeName(raw) == "null" {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil
	}
	switch typed := decoded.(type) {
	case []any:
		rules := make([]map[string]any, 0, len(typed))
		for _, item := range typed {
			rule, ok := providerNormalizeRedirectRule(item)
			if !ok {
				// 列表里有一项不合法时 Node 整体判为非规则列表（isProviderModelRedirectRuleList 要求 every）。
				rules = nil
				break
			}
			rules = append(rules, rule)
		}
		if rules == nil {
			return nil
		}
		return providerRawJSON(rules)
	case map[string]any:
		keys := providerSortedKeys(typed)
		rules := make([]map[string]any, 0, len(keys))
		for _, key := range keys {
			target, ok := typed[key].(string)
			if !ok {
				continue
			}
			source := strings.TrimSpace(key)
			trimmedTarget := strings.TrimSpace(target)
			if source == "" || trimmedTarget == "" {
				continue
			}
			rules = append(rules, map[string]any{
				"matchType": "exact", "source": source, "target": trimmedTarget,
			})
		}
		return providerRawJSON(rules)
	}
	return nil
}

// providerNormalizeRedirectRule 复刻 isProviderModelRedirectRule + normalizeProviderModelRedirectRule。
func providerNormalizeRedirectRule(value any) (map[string]any, bool) {
	record, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	matchType, _ := record["matchType"].(string)
	source, _ := record["source"].(string)
	target, _ := record["target"].(string)
	if !providerStringInList(matchType, providerModelRedirectMatchTypes) {
		return nil, false
	}
	source = strings.TrimSpace(source)
	target = strings.TrimSpace(target)
	if source == "" || target == "" {
		return nil, false
	}
	return map[string]any{"matchType": matchType, "source": source, "target": target}, true
}

// providerNormalizeAllowedModels 复刻 normalizeAllowedModelRules：非数组即 null，逐项归一后过滤非法项。
func providerNormalizeAllowedModels(value any) any {
	raw, ok := value.(json.RawMessage)
	if !ok || len(raw) == 0 || adminJSONTypeName(raw) == "null" {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil
	}
	items, ok := decoded.([]any)
	if !ok {
		return nil
	}
	rules := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if pattern, ok := item.(string); ok {
			trimmed := strings.TrimSpace(pattern)
			if trimmed == "" {
				continue
			}
			rules = append(rules, map[string]any{"matchType": "exact", "pattern": trimmed})
			continue
		}
		record, ok := item.(map[string]any)
		if !ok {
			continue
		}
		matchType, _ := record["matchType"].(string)
		pattern, _ := record["pattern"].(string)
		trimmed := strings.TrimSpace(pattern)
		if !providerStringInList(matchType, providerModelRedirectMatchTypes) || trimmed == "" {
			continue
		}
		rules = append(rules, map[string]any{"matchType": matchType, "pattern": trimmed})
	}
	return providerRawJSON(rules)
}

func providerRawJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return json.RawMessage(encoded)
}

func providerStringInList(value string, list []string) bool {
	for _, item := range list {
		if item == value {
			return true
		}
	}
	return false
}

// providerCreateValidationError 承载创建时的三段必填校验失败。
type providerCreateValidationError struct {
	issues []invalidParam
}

// providerValidateCreate 复刻 ProviderCreateSchema 的三段必填：name（trim 后 1..64）、
// url（合法 URL、≤255）、key（1..providerWriteMaxKeyLength），并按 Node 的默认值补齐字段袋。
func providerValidateCreate(payload *map[string]any) *providerCreateValidationError {
	fields := *payload
	issues := make([]invalidParam, 0, 2)

	name, _ := fields["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		issues = append(issues, invalidParam{
			Path: []any{"name"}, Code: "too_small",
			Message: "String must contain at least 1 character(s)",
		})
	} else if len([]rune(name)) > 64 {
		issues = append(issues, invalidParam{
			Path: []any{"name"}, Code: "too_big",
			Message: "String must contain at most 64 character(s)",
		})
	}
	fields["name"] = name

	rawURL, _ := fields["url"].(string)
	trimmedURL := strings.TrimSpace(rawURL)
	if !providerIsAbsoluteURL(trimmedURL) {
		issues = append(issues, invalidParam{
			Path: []any{"url"}, Code: "invalid_format", Message: "Invalid URL",
		})
	} else if len(trimmedURL) > 255 {
		issues = append(issues, invalidParam{
			Path: []any{"url"}, Code: "too_big",
			Message: "String must contain at most 255 character(s)",
		})
	}
	fields["url"] = trimmedURL

	key, _ := fields["key"].(string)
	if key == "" {
		issues = append(issues, invalidParam{
			Path: []any{"key"}, Code: "too_small",
			Message: "String must contain at least 1 character(s)",
		})
	} else if len(key) > providerWriteMaxKeyLength {
		issues = append(issues, invalidParam{
			Path: []any{"key"}, Code: "too_big",
			Message: fmt.Sprintf("String must contain at most %d character(s)", providerWriteMaxKeyLength),
		})
	}
	if len(issues) > 0 {
		return &providerCreateValidationError{issues: issues}
	}

	// 默认值逐条取自 actions/providers.ts:638-694 的 payload 构造。
	providerFillDefault(fields, "provider_type", "claude")
	providerFillDefault(fields, "limit_5h_reset_mode", "rolling")
	providerFillDefault(fields, "daily_reset_mode", "fixed")
	providerFillDefault(fields, "daily_reset_time", "00:00")
	// 可空文本列的默认值必须是 *string：这些列的绑定类型是「可空文本」，
	// 填裸字符串会在存储层被类型检查拦下（这是刻意的——写错类型不该静默写 NULL）。
	providerFillDefault(fields, "cache_ttl_preference", providerDefaultText("inherit"))
	providerFillDefault(fields, "context_1m_preference", providerDefaultText("inherit"))
	providerFillDefault(fields, "codex_reasoning_effort_preference", providerDefaultText("inherit"))
	providerFillDefault(fields, "codex_reasoning_summary_preference", providerDefaultText("inherit"))
	providerFillDefault(fields, "codex_text_verbosity_preference", providerDefaultText("inherit"))
	providerFillDefault(fields, "codex_parallel_tool_calls_preference", providerDefaultText("inherit"))
	providerFillDefault(fields, "codex_service_tier_preference", providerDefaultText("inherit"))
	providerFillDefault(fields, "codex_max_tokens_preference", providerDefaultText("inherit"))
	providerFillDefault(fields, "openai_max_tokens_preference", providerDefaultText("inherit"))
	providerFillDefault(fields, "circuit_breaker_failure_threshold", int64(5))
	providerFillDefault(fields, "circuit_breaker_open_duration", int64(1800000))
	providerFillDefault(fields, "circuit_breaker_half_open_success_threshold", int64(2))
	providerFillDefault(fields, "limit_concurrent_sessions", providerNullableInt(0))
	providerFillDefault(fields, "weight", int64(1))
	providerFillDefault(fields, "priority", int64(0))
	providerFillDefault(fields, "is_enabled", true)
	providerFillDefault(fields, "preserve_client_ip", false)
	providerFillDefault(fields, "disable_session_reuse", false)
	providerFillDefault(fields, "proxy_fallback_to_direct", false)
	providerFillDefault(fields, "swap_cache_ttl_billing", false)
	providerFillDefault(fields, "protocol_conversion_enabled", false)
	providerFillDefault(fields, "mcp_passthrough_type", "none")
	providerFillDefault(fields, "favicon_url", providerFaviconForWebsite(fields["website_url"]))
	return nil
}

// providerFillDefault 只在字段缺失时填默认（Node 的 `validated.x ?? 默认`）。
func providerFillDefault(fields map[string]any, name string, value any) {
	if _, present := fields[name]; present {
		return
	}
	fields[name] = value
}

func providerNullableInt(value int64) *int64 { return &value }

// providerDefaultText 给可空文本列造默认值。
func providerDefaultText(value string) *string { return &value }

// providerFaviconForWebsite 复刻 addProvider 的 favicon 生成：
// `https://www.google.com/s2/favicons?domain=<hostname>&sz=32`；website_url 为空或解析失败时 null。
func providerFaviconForWebsite(website any) any {
	text, ok := website.(*string)
	if !ok || text == nil || strings.TrimSpace(*text) == "" {
		return (*string)(nil)
	}
	domain := providerNormalizeWebsiteDomainKey(*text)
	if domain == "" {
		return (*string)(nil)
	}
	// 去掉可能的端口：Node 取的是 hostname（不含端口）。
	if index := strings.LastIndex(domain, ":"); index > 0 && !strings.HasPrefix(domain, "[") {
		domain = domain[:index]
	}
	generated := "https://www.google.com/s2/favicons?domain=" + domain + "&sz=32"
	return &generated
}

// providerIsAbsoluteURL 判定绝对 URL（Node 的 z.string().url() 走 new URL()：
// 必须有 scheme 与 host，故 mailto: 这类无 host 的会被拒）。
func providerIsAbsoluteURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return parsed.Scheme != "" && parsed.Host != ""
}

// providerDecodeProviderIDs 解析 `{providerIds: number[]}`（Node 的 ProviderIdsBodySchema）。
func providerDecodeProviderIDs(fields map[string]json.RawMessage) ([]int64, []invalidParam) {
	object := adminNewObject(fields, "providerIds")
	object.RejectUnknownKeys()
	raw, present := object.Raw("providerIds")
	if !present {
		object.fail([]any{"providerIds"}, "invalid_type", "Expected array, received undefined")
		return nil, object.issues0()
	}
	var decoded []any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		object.fail([]any{"providerIds"}, "invalid_type", adminTypeMessage("array", raw))
		return nil, object.issues0()
	}
	if len(decoded) == 0 {
		object.fail([]any{"providerIds"}, "too_small",
			"Array must contain at least 1 element(s)")
		return nil, object.issues0()
	}
	if len(decoded) > 500 {
		object.fail([]any{"providerIds"}, "too_big",
			"Array must contain at most 500 element(s)")
		return nil, object.issues0()
	}
	ids := make([]int64, 0, len(decoded))
	for index, item := range decoded {
		number, ok := item.(float64)
		if !ok || number != float64(int64(number)) || number <= 0 {
			object.fail([]any{"providerIds", index}, "invalid_type",
				"Expected positive integer, received invalid value")
			return nil, object.issues0()
		}
		ids = append(ids, int64(number))
	}
	return ids, object.issues0()
}

// providerDecodeUndoBody 解析 `{undoToken, operationId}`（Node 的 ProviderUndoBodySchema：均 trim+min1）。
func providerDecodeUndoBody(fields map[string]json.RawMessage) (string, string, []invalidParam) {
	object := adminNewObject(fields, "undoToken", "operationId")
	object.RejectUnknownKeys()
	token, tokenPresent := object.String("undoToken", adminStringSpec{})
	if !tokenPresent || strings.TrimSpace(token) == "" {
		object.fail([]any{"undoToken"}, "too_small",
			"String must contain at least 1 character(s)")
		return "", "", object.issues0()
	}
	operationID, operationPresent := object.String("operationId", adminStringSpec{})
	if !operationPresent || strings.TrimSpace(operationID) == "" {
		object.fail([]any{"operationId"}, "too_small",
			"String must contain at least 1 character(s)")
		return "", "", object.issues0()
	}
	return strings.TrimSpace(token), strings.TrimSpace(operationID), object.issues0()
}

// providerEnsureVisibleIDs 复刻 ensureVisibleProviderIds：任一 id 不在可见集合内即 404。
func providerEnsureVisibleIDs(request *http.Request, deps Deps, ids []int64) ([]int64, error) {
	visible, err := providerVisibleProviders(request, deps)
	if err != nil {
		return nil, adminActionFailure("provider", err)
	}
	known := make(map[int64]struct{}, len(visible))
	for index := range visible {
		known[visible[index].ID] = struct{}{}
	}
	for _, id := range ids {
		if _, ok := known[id]; !ok {
			return nil, providerNotFoundError()
		}
	}
	return ids, nil
}

// providerBuildPreimage 产出撤销前像：**只记真正发生变化的字段**，键是 Node 的 camelCase 名。
//
// 两处对齐 Node（actions/providers.ts:882-900）：
//   - `key` 不进前像（密钥不回写：撤销一个改名不该把密钥倒回旧值）；
//   - 值未变化（含数组/对象的深比较）的字段不记。
func providerBuildPreimage(existing store.AdminProvider, payload map[string]any) map[string]any {
	current := providerCamelCaseMap(existing)
	preimage := map[string]any{}
	for _, name := range providerSortedKeys(payload) {
		if name == "key" {
			continue
		}
		camel, ok := providerPreimageFieldNames[name]
		if !ok {
			continue
		}
		before, hasBefore := current[camel]
		if !hasBefore {
			continue
		}
		if !providerFieldChangedForUndo(before, providerPreimageComparable(payload[name])) {
			continue
		}
		preimage[camel] = before
	}
	return preimage
}

// providerCamelCaseMap 把库里的一行投影成 Node 的 camelCase 键值（与 AdminProvider 的 json 标签一致）。
func providerCamelCaseMap(provider store.AdminProvider) map[string]any {
	encoded, err := adminMarshalJSON(provider)
	if err != nil {
		return map[string]any{}
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		return map[string]any{}
	}
	return decoded
}

// providerPreimageComparable 把存储层字段袋的值转成可与 JSON 值比较的形式。
func providerPreimageComparable(value any) any {
	switch typed := value.(type) {
	case *string:
		if typed == nil {
			return nil
		}
		return *typed
	case *int64:
		if typed == nil {
			return nil
		}
		return *typed
	case *float64:
		if typed == nil {
			return nil
		}
		return *typed
	case json.RawMessage:
		if len(typed) == 0 {
			return nil
		}
		var decoded any
		if err := json.Unmarshal(typed, &decoded); err != nil {
			return nil
		}
		return decoded
	}
	return value
}

// providerFieldChangedForUndo 复刻 hasProviderFieldChangedForUndo：对象/数组按 JSON 串比较，
// 其余按值比较（字符串与数字不互认）。
func providerFieldChangedForUndo(before, after any) bool {
	if before == nil && after == nil {
		return false
	}
	switch before.(type) {
	case map[string]any, []any:
		beforeJSON := providerRawJSON(before)
		afterJSON := providerRawJSON(after)
		if len(beforeJSON) == 0 || len(afterJSON) == 0 {
			return true
		}
		return string(beforeJSON) != string(afterJSON)
	}
	return !providerScalarEqual(before, after)
}

func providerScalarEqual(left, right any) bool {
	switch leftTyped := left.(type) {
	case string:
		text, ok := right.(string)
		return ok && leftTyped == text
	case float64:
		// 库里的行经 JSON 往返后数值一律是 float64，而字段袋里的整数是 int64：
		// 不跨类型比较会把「没变」误判成「变了」，前像于是多记字段（撤销时白写一次）。
		switch rightTyped := right.(type) {
		case float64:
			return leftTyped == rightTyped
		case int64:
			return leftTyped == float64(rightTyped)
		}
		return false
	case int64:
		switch rightTyped := right.(type) {
		case int64:
			return leftTyped == rightTyped
		case float64:
			return float64(leftTyped) == rightTyped
		}
		return false
	case bool:
		flag, ok := right.(bool)
		return ok && leftTyped == flag
	case nil:
		return right == nil
	}
	return false
}

// providerPreimageToWriteFields 把撤销前像翻译回存储层字段袋。
//
// 值从 Redis 读回后只有 JSON 的六种形态（string/number/bool/null/array/object），故按字段表
// 的**绑定类型**逐个转换；类型不符即返回校验失败——宁可不撤销，也不写错类型。
func providerPreimageToWriteFields(preimage map[string]any) (map[string]any, []invalidParam) {
	specs := providerUpdateWriteSpecs()
	payload := make(map[string]any, len(preimage))
	issues := make([]invalidParam, 0, 1)
	for _, camel := range providerSortedKeys(preimage) {
		name, ok := providerPayloadFieldForCamel(camel)
		if !ok {
			// 前像里出现 Go 侧未知的 camelCase 名（例如 Node 新加的字段）：跳过而不是写错列。
			continue
		}
		if _, ok := specs[name]; !ok {
			continue
		}
		value, ok := providerPreimageValue(name, preimage[camel])
		if !ok {
			issues = append(issues, invalidParam{
				Path: []any{name}, Code: "invalid_type",
				Message: "Undo preimage value has an unsupported type",
			})
			continue
		}
		payload[name] = value
	}
	if len(issues) > 0 {
		return nil, issues
	}
	return payload, nil
}

// providerPreimageValue 按字段表的 kind 把 JSON 值转成存储层类型。
func providerPreimageValue(name string, value any) (any, bool) {
	var kind = providerWriteKindOf(name)
	switch kind {
	case "text":
		text, ok := value.(string)
		return text, ok
	case "nullable_text":
		if value == nil {
			return (*string)(nil), true
		}
		text, ok := value.(string)
		if !ok {
			return nil, false
		}
		return &text, true
	case "int":
		if number, ok := value.(float64); ok && number == float64(int64(number)) {
			return int64(number), true
		}
		return nil, false
	case "nullable_int":
		if value == nil {
			return (*int64)(nil), true
		}
		if number, ok := value.(float64); ok && number == float64(int64(number)) {
			converted := int64(number)
			return &converted, true
		}
		return nil, false
	case "bool":
		flag, ok := value.(bool)
		return flag, ok
	case "numeric":
		if value == nil {
			return (*float64)(nil), true
		}
		number, ok := value.(float64)
		if !ok {
			return nil, false
		}
		return &number, true
	case "json":
		encoded := providerRawJSON(value)
		if len(encoded) == 0 {
			return nil, false
		}
		return encoded, true
	}
	return nil, false
}

// providerWriteKindOf 给出字段在存储层的绑定类型名（与 store 的 kind 常量一一对应）。
func providerWriteKindOf(name string) string {
	switch name {
	case "name", "url", "key", "provider_type", "mcp_passthrough_type",
		"limit_5h_reset_mode", "daily_reset_mode", "daily_reset_time":
		return "text"
	case "is_enabled", "preserve_client_ip", "disable_session_reuse",
		"proxy_fallback_to_direct", "swap_cache_ttl_billing", "protocol_conversion_enabled":
		return "bool"
	case "weight", "priority", "circuit_breaker_failure_threshold",
		"circuit_breaker_open_duration", "circuit_breaker_half_open_success_threshold",
		"first_byte_timeout_streaming_ms", "streaming_idle_timeout_ms",
		"request_timeout_non_streaming_ms":
		return "int"
	case "limit_concurrent_sessions", "max_retry_attempts",
		// 等待阶梯两列为可空（null = 不启用），故归 nullable_int 而不是 int。
		"circuit_breaker_release_increment", "circuit_breaker_max_open_count", "tpm", "rpm", "rpd", "cc":
		return "nullable_int"
	case "cost_multiplier", "limit_5h_usd", "limit_daily_usd", "limit_weekly_usd",
		"limit_monthly_usd", "limit_total_usd":
		return "numeric"
	case "model_redirects", "allowed_models", "allowed_clients", "blocked_clients",
		"custom_headers", "group_priorities", "anthropic_adaptive_thinking":
		return "json"
	}
	return "nullable_text"
}

// providerEmitWriteAudit 写一条写路径审计（Node 的 emitActionAudit，category 固定 "provider"）。
//
// targetID <= 0 表示「失败得早、还没有 id」（创建失败时插入未成）：target_id 落 NULL，与 Node 的
// `targetId: undefined` 同（audit.go 的 nullIfEmpty）。targetName 同为空即落 NULL。
func providerEmitWriteAudit(
	deps Deps,
	request *http.Request,
	action string,
	targetID int64,
	targetName string,
	details map[string]any,
	success bool,
	errorMessage string,
) {
	if deps.Audit == nil {
		return
	}
	target := ""
	if targetID > 0 {
		target = providerIDKey(targetID)
	}
	deps.Audit.Emit(request.Context(), AuditEvent{
		Category:     "provider",
		Principal:    providerPrincipalFrom(request),
		Action:       action,
		TargetType:   "provider",
		TargetID:     target,
		TargetName:   targetName,
		Details:      details,
		IP:           auditClientIP(request.Context(), deps.Store, request),
		UserAgent:    request.UserAgent(),
		Success:      success,
		ErrorMessage: errorMessage,
	})
}

// providerWriteFailure 作答一次写路径失败：落失败审计 + 记成因日志 + 按 action 错误表回 400。
//
// 为什么把两件事绑一起：Node 的 action 层是「catch 里既 logger.error 又 emitActionAudit」
// （actions/providers.ts:985-997），而 Go 侧此前两样都没有——写失败只回一个 detail 恒为
// "Bad request" 的 400，audit_log 与进程日志里都查不到成因（2026-09-15 的 PATCH
// /api/v1/providers/149：DB 已提交、撤销快照写失败，全程无从下手）。绑成一个函数是为了让
// 「审计」与「回应」不再能只写一半。
//
// cause 只进服务端日志（WriteActionError 脱敏后记），不进响应体与审计行：前者是公开文案
// （Node 的 publicActionErrorDetail），后者对管理面多角色可见。
func providerWriteFailure(
	deps Deps,
	writer http.ResponseWriter,
	request *http.Request,
	action string,
	targetID int64,
	targetName string,
	errorMessage string,
	details map[string]any,
	cause error,
) {
	providerEmitWriteAudit(deps, request, action, targetID, targetName, details, false, errorMessage)
	adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", cause))
}

// providerWriteAuditName 取审计的 targetName（Node 的 `targetName: data.name` / `provider.name`）。
func providerWriteAuditName(payload map[string]any) string {
	name, _ := payload["name"].(string)
	return name
}

// providerWriteAuditAfter 复刻 Node 创建审计的 after 段（id/name/url 脱敏）。
func providerWriteAuditAfter(payload map[string]any) map[string]any {
	name, _ := payload["name"].(string)
	rawURL, _ := payload["url"].(string)
	return map[string]any{
		"name": name,
		"url":  providerRedactURLCredentials(rawURL),
	}
}

// providerFindRedactedWriteField 复刻创建路径的脱敏占位符检查
// （handlers.ts:113-120 的 hasLegacyRedactedWritePlaceholders + 422 provider.redacted_placeholder_rejected）。
//
// 返回命中的字段名：凭据类字段（key / custom_headers / proxy_url / mcp_passthrough_url / url /
// website_url）里出现 "[REDACTED]" 说明调用方把 GET 回来的脱敏值原样回写了，那会把真实凭据覆盖成占位符。
func providerFindRedactedWriteField(fields map[string]json.RawMessage) (string, bool) {
	for _, name := range providerSortedKeys(fields) {
		switch name {
		case "key", "custom_headers", "proxy_url", "mcp_passthrough_url", "url", "website_url":
			if providerHasRedactedPlaceholder(fields[name]) {
				return name, true
			}
		}
	}
	return "", false
}

// adminProviderRedactedPlaceholder 作答 422 的脱敏占位符问题（两处调用点共用同一形状）。
func adminProviderRedactedPlaceholder(
	writer http.ResponseWriter,
	request *http.Request,
	deps Deps,
	detail string,
) {
	adminProblemWriter(deps).WriteProblem(writer, request, http.StatusUnprocessableEntity,
		"provider.redacted_placeholder_rejected", detail)
}

// providerUndoExpiredError 复刻 UNDO_EXPIRED（410）。
func providerUndoExpiredError(message string) *ActionError {
	return &ActionError{Resource: "provider", Code: "UNDO_EXPIRED", Status: http.StatusGone,
		Err: fmt.Errorf("%s", message)}
}

// providerUndoConflictError 复刻 UNDO_CONFLICT（409）。
func providerUndoConflictError(message string) *ActionError {
	return &ActionError{Resource: "provider", Code: "UNDO_CONFLICT", Status: http.StatusConflict,
		Err: fmt.Errorf("%s", message)}
}

// providerIDKey 是前像与快照里 provider id 的字符串化形式（Node 的 `String(id)` 与对象键）。
func providerIDKey(id int64) string { return fmt.Sprintf("%d", id) }

// providerSortedKeys 给出 map 的键的确定序（审计与 diff 的输出要稳定，否则对拍永远不平）。
func providerSortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}

// providerSortFieldNames 把字段名排序（解码顺序稳定 → 校验错误的顺序稳定）。
func providerSortFieldNames[T any](fields map[string]T) []string {
	return providerSortedKeys(fields)
}

// providerPrincipalFrom 取请求上下文里的身份（审计用；缺失时给零值 Principal，
// 与 Node 侧 emitActionAudit 在未认证上下文里的行为一致——审计行仍写，但操作者为空）。
func providerPrincipalFrom(request *http.Request) Principal {
	principal, ok := PrincipalFrom(request.Context())
	if !ok {
		return Principal{}
	}
	return principal
}
