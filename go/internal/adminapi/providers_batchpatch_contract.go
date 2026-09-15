package adminapi

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// 本文件是 Node 侧**供应商批量补丁契约**的移植。
//
// 唯一真源：src/lib/provider-patch-contract.ts（1164 行）——PATCH_FIELDS（:29-92）、
// CLEARABLE_FIELDS（:94-142）、isValidSetValue（:215-306）、normalizePatchField（:342-406）、
// normalizeProviderBatchPatchDraft（:409-716）、applyPatchField（:718-1019）、
// buildProviderBatchApplyUpdates（:1021-1094）、hasProviderBatchPatchChanges（:1096-1153）、
// prepareProviderBatchApplyUpdates（:1155-1164）；
// 预览行的字段/取值映射见 src/actions/providers.ts:1836-1905（PATCH_FIELD_TO_PROVIDER_KEY /
// PATCH_FIELD_CLEAR_VALUE）与 :1907-2017（按供应商类型分组）、:1995-2078（generatePreviewRows）。
//
// 为什么用规格表而不是给每个字段写一个函数：Node 侧本来就是「字段顺序表 + 逐字段 switch」，
// 且**顺序本身是语义**——归一化按表序返回**第一条**错误，`changedFields` 与 `previewRevision`
// 也按表序拼串。展开成 52 个函数会让这三处顺序约定散落各处，反而更容易与 Node 分叉。

// providerPatchMode 是补丁字段的三态（Node 的 ProviderPatchOperation.mode）。
type providerPatchMode string

const (
	providerPatchModeSet      providerPatchMode = "set"
	providerPatchModeClear    providerPatchMode = "clear"
	providerPatchModeNoChange providerPatchMode = "no_change"
)

// providerPatchErrorCode 是补丁契约的唯一错误码（Node 的 PROVIDER_PATCH_ERROR_CODES）。
const providerPatchErrorCode = "INVALID_PATCH_SHAPE"

// providerPatchFieldValueKind 是字段的「列绑定类型」，决定 set 值如何转成 store 可直接绑定的 Go 值。
//
// 与 store 的 adminProviderValueKind 同义但要在这里知道，因为**转换发生在契约层**：
// 预览行要展示转换后的值，apply 也要用它拼 UPDATE。
type providerPatchFieldValueKind int

const (
	patchValueText providerPatchFieldValueKind = iota
	patchValueNullableText
	patchValueInt
	patchValueNullableInt
	patchValueBool
	patchValueNumeric
	patchValueJSON
	patchValueStringList
)

// providerFieldGroup 是「只对某类供应商有意义」的字段分组（Node 的 CLAUDE_ONLY_FIELDS 等，actions:1907-1932）。
type providerFieldGroup string

const (
	providerGroupAny             providerFieldGroup = ""
	providerGroupClaude          providerFieldGroup = "claude"
	providerGroupCodex           providerFieldGroup = "codex"
	providerGroupGemini          providerFieldGroup = "gemini"
	providerGroupOpenAICompat    providerFieldGroup = "openai-compatible"
	providerPatchMaxGroupTagRune                    = 255
	providerPatchMaxRuleItems                       = 100_000
	providerPatchMaxRuleText                        = 4_096
)

// providerPatchSetValidator 校验 set 值（Node 的 isValidSetValue）。
type providerPatchSetValidator func(value any) bool

// providerPatchFieldSpec 是一条补丁字段的完整语义。
type providerPatchFieldSpec struct {
	// Field 是 Node 侧字段名（snake_case），同时也是写路径 payload 名与 providers 列名。
	Field string
	// ProviderKey 是 Node Provider 行上的 camelCase 键名（预览 before/after 与撤销前像用它，actions:1836）。
	ProviderKey string
	// ValueKind 决定 set 值的 Go 类型转换。
	ValueKind providerPatchFieldValueKind
	// Group 非空时表示该字段只适用于对应类型的供应商（预览行会标 skipped）。
	Group providerFieldGroup
	// Clearable 为 true 表示支持 clear 模式（Node 的 CLEARABLE_FIELDS）。
	Clearable bool
	// ClearValue 是 clear 模式写入的**非 null** 取值（Node 的 PATCH_FIELD_CLEAR_VALUE）。
	// 为 nil 表示该字段 clear 到 NULL。
	ClearValue any
	// Validate 校验 set 值。
	Validate providerPatchSetValidator
}

// providerPatchFieldSpecs 的顺序与 Node 的 PATCH_FIELDS 逐条一致，**不可重排**：
// 归一化返回第一条错误、changedFields 与 previewRevision 的拼接顺序都依赖它。
var providerPatchFieldSpecs = []providerPatchFieldSpec{
	{Field: "is_enabled", ProviderKey: "isEnabled", ValueKind: patchValueBool, Validate: validateBool},
	{Field: "priority", ProviderKey: "priority", ValueKind: patchValueInt, Validate: validateInt},
	{Field: "weight", ProviderKey: "weight", ValueKind: patchValueInt, Validate: validateInt},
	{Field: "cost_multiplier", ProviderKey: "costMultiplier", ValueKind: patchValueNumeric, Validate: validateNumber},
	{Field: "group_tag", ProviderKey: "groupTag", ValueKind: patchValueNullableText, Clearable: true, Validate: validateGroupTag},
	{Field: "model_redirects", ProviderKey: "modelRedirects", ValueKind: patchValueJSON, Clearable: true, Validate: validateRedirectRules},
	{Field: "allowed_models", ProviderKey: "allowedModels", ValueKind: patchValueJSON, Clearable: true, Validate: validateAllowedModelRules},
	{Field: "allowed_clients", ProviderKey: "allowedClients", ValueKind: patchValueStringList, Clearable: true, ClearValue: []string{}, Validate: validateStringList},
	{Field: "blocked_clients", ProviderKey: "blockedClients", ValueKind: patchValueStringList, Clearable: true, ClearValue: []string{}, Validate: validateStringList},
	{
		Field: "anthropic_thinking_budget_preference", ProviderKey: "anthropicThinkingBudgetPreference",
		ValueKind: patchValueNullableText, Group: providerGroupClaude, Clearable: true, ClearValue: "inherit",
		Validate: validateThinkingBudgetPreference,
	},
	{
		Field: "anthropic_adaptive_thinking", ProviderKey: "anthropicAdaptiveThinking",
		ValueKind: patchValueJSON, Group: providerGroupClaude, Clearable: true,
		Validate: validateAdaptiveThinking,
	},
	// Routing
	{Field: "active_time_start", ProviderKey: "activeTimeStart", ValueKind: patchValueNullableText, Clearable: true, Validate: validateHHMM},
	{Field: "active_time_end", ProviderKey: "activeTimeEnd", ValueKind: patchValueNullableText, Clearable: true, Validate: validateHHMM},
	{Field: "preserve_client_ip", ProviderKey: "preserveClientIp", ValueKind: patchValueBool, Validate: validateBool},
	{Field: "disable_session_reuse", ProviderKey: "disableSessionReuse", ValueKind: patchValueBool, Validate: validateBool},
	{Field: "group_priorities", ProviderKey: "groupPriorities", ValueKind: patchValueJSON, Clearable: true, Validate: validateNumberRecord},
	{
		Field: "cache_ttl_preference", ProviderKey: "cacheTtlPreference",
		ValueKind: patchValueNullableText, Clearable: true, ClearValue: "inherit",
		Validate: validateEnum("inherit", "5m", "1h"),
	},
	{Field: "swap_cache_ttl_billing", ProviderKey: "swapCacheTtlBilling", ValueKind: patchValueBool, Validate: validateBool},
	{
		Field: "context_1m_preference", ProviderKey: "context1mPreference",
		ValueKind: patchValueNullableText, Group: providerGroupClaude, Clearable: true, ClearValue: "inherit",
		Validate: validateEnum("inherit", "force_enable", "disabled"),
	},
	{
		Field: "codex_reasoning_effort_preference", ProviderKey: "codexReasoningEffortPreference",
		ValueKind: patchValueNullableText, Group: providerGroupCodex, Clearable: true, ClearValue: "inherit",
		Validate: validateEnum("inherit", "none", "minimal", "low", "medium", "high", "xhigh", "max"),
	},
	{
		Field: "codex_reasoning_summary_preference", ProviderKey: "codexReasoningSummaryPreference",
		ValueKind: patchValueNullableText, Group: providerGroupCodex, Clearable: true, ClearValue: "inherit",
		Validate: validateEnum("inherit", "auto", "detailed"),
	},
	{
		Field: "codex_text_verbosity_preference", ProviderKey: "codexTextVerbosityPreference",
		ValueKind: patchValueNullableText, Group: providerGroupCodex, Clearable: true, ClearValue: "inherit",
		Validate: validateEnum("inherit", "low", "medium", "high"),
	},
	{
		Field: "codex_parallel_tool_calls_preference", ProviderKey: "codexParallelToolCallsPreference",
		ValueKind: patchValueNullableText, Group: providerGroupCodex, Clearable: true, ClearValue: "inherit",
		Validate: validateEnum("inherit", "true", "false"),
	},
	{
		Field: "codex_image_generation_preference", ProviderKey: "codexImageGenerationPreference",
		ValueKind: patchValueNullableText, Group: providerGroupCodex, Clearable: true, ClearValue: "inherit",
		Validate: validateEnum("inherit", "true", "false"),
	},
	{
		Field: "codex_service_tier_preference", ProviderKey: "codexServiceTierPreference",
		ValueKind: patchValueNullableText, Group: providerGroupCodex, Clearable: true, ClearValue: "inherit",
		Validate: validateEnum("inherit", "auto", "default", "flex", "priority"),
	},
	{
		Field: "codex_max_tokens_preference", ProviderKey: "codexMaxTokensPreference",
		ValueKind: patchValueNullableText, Group: providerGroupCodex, Clearable: true, ClearValue: "inherit",
		Validate: validateMaxTokensPreference,
	},
	{
		Field: "anthropic_max_tokens_preference", ProviderKey: "anthropicMaxTokensPreference",
		ValueKind: patchValueNullableText, Group: providerGroupClaude, Clearable: true, ClearValue: "inherit",
		Validate: validateMaxTokensPreference,
	},
	{
		Field: "openai_max_tokens_preference", ProviderKey: "openaiMaxTokensPreference",
		ValueKind: patchValueNullableText, Group: providerGroupOpenAICompat, Clearable: true, ClearValue: "inherit",
		Validate: validateMaxTokensPreference,
	},
	{
		Field: "gemini_google_search_preference", ProviderKey: "geminiGoogleSearchPreference",
		ValueKind: patchValueNullableText, Group: providerGroupGemini, Clearable: true, ClearValue: "inherit",
		Validate: validateEnum("inherit", "enabled", "disabled"),
	},
	// Rate Limit
	{Field: "limit_5h_usd", ProviderKey: "limit5hUsd", ValueKind: patchValueNumeric, Clearable: true, Validate: validateNumber},
	{
		Field: "limit_5h_reset_mode", ProviderKey: "limit5hResetMode",
		ValueKind: patchValueText, Validate: validateEnum("fixed", "rolling"),
	},
	{Field: "limit_daily_usd", ProviderKey: "limitDailyUsd", ValueKind: patchValueNumeric, Clearable: true, Validate: validateNumber},
	{
		Field: "daily_reset_mode", ProviderKey: "dailyResetMode",
		ValueKind: patchValueText, Validate: validateEnum("fixed", "rolling"),
	},
	{Field: "daily_reset_time", ProviderKey: "dailyResetTime", ValueKind: patchValueText, Validate: validateAnyString},
	{Field: "limit_weekly_usd", ProviderKey: "limitWeeklyUsd", ValueKind: patchValueNumeric, Clearable: true, Validate: validateNumber},
	{Field: "limit_monthly_usd", ProviderKey: "limitMonthlyUsd", ValueKind: patchValueNumeric, Clearable: true, Validate: validateNumber},
	{Field: "limit_total_usd", ProviderKey: "limitTotalUsd", ValueKind: patchValueNumeric, Clearable: true, Validate: validateNumber},
	// 列是可空 integer（drizzle/0000:11 `integer DEFAULT 0`，store 侧 providerNullableIntKind），
	// 故用 nullableInt：裸 int64 会在绑定时报「值类型不符」。
	{Field: "limit_concurrent_sessions", ProviderKey: "limitConcurrentSessions", ValueKind: patchValueNullableInt, Validate: validateInt},
	// Circuit Breaker
	{
		Field: "circuit_breaker_failure_threshold", ProviderKey: "circuitBreakerFailureThreshold",
		ValueKind: patchValueInt, Validate: validateInt,
	},
	{
		Field: "circuit_breaker_open_duration", ProviderKey: "circuitBreakerOpenDuration",
		ValueKind: patchValueInt, Validate: validateInt,
	},
	{
		Field: "circuit_breaker_half_open_success_threshold", ProviderKey: "circuitBreakerHalfOpenSuccessThreshold",
		ValueKind: patchValueInt, Validate: validateInt,
	},
	{Field: "max_retry_attempts", ProviderKey: "maxRetryAttempts", ValueKind: patchValueNullableInt, Clearable: true, Validate: validateInt},
	// Network
	{Field: "proxy_url", ProviderKey: "proxyUrl", ValueKind: patchValueNullableText, Clearable: true, Validate: validateAnyString},
	{Field: "proxy_fallback_to_direct", ProviderKey: "proxyFallbackToDirect", ValueKind: patchValueBool, Validate: validateBool},
	{
		Field: "first_byte_timeout_streaming_ms", ProviderKey: "firstByteTimeoutStreamingMs",
		ValueKind: patchValueInt, Validate: validateInt,
	},
	{
		Field: "streaming_idle_timeout_ms", ProviderKey: "streamingIdleTimeoutMs",
		ValueKind: patchValueInt, Validate: validateInt,
	},
	{
		Field: "request_timeout_non_streaming_ms", ProviderKey: "requestTimeoutNonStreamingMs",
		ValueKind: patchValueInt, Validate: validateInt,
	},
	// MCP
	{
		Field: "mcp_passthrough_type", ProviderKey: "mcpPassthroughType",
		ValueKind: patchValueText, Validate: validateEnum("none", "minimax", "glm", "custom"),
	},
	{Field: "mcp_passthrough_url", ProviderKey: "mcpPassthroughUrl", ValueKind: patchValueNullableText, Clearable: true, Validate: validateAnyString},
}

// providerPatchFieldByName 按字段名取规格；ok=false 表示该字段不在契约内。
func providerPatchFieldByName(field string) (providerPatchFieldSpec, bool) {
	for _, spec := range providerPatchFieldSpecs {
		if spec.Field == field {
			return spec, true
		}
	}
	return providerPatchFieldSpec{}, false
}

// providerPatchOperation 是一个字段的三态操作；SetValue 仅在 Mode==set 时有意义。
type providerPatchOperation struct {
	Mode     providerPatchMode
	SetValue any
}

// providerBatchPatch 是归一化后的补丁：字段 → 操作。只含被显式给出的字段（缺键即 no_change）。
type providerBatchPatch map[string]providerPatchOperation

// providerPatchContractError 是契约层错误（Node 的 ProviderPatchError）。
type providerPatchContractError struct {
	Field   string
	Message string
}

func (e *providerPatchContractError) Error() string { return e.Message }

func patchContractError(field, message string) *providerPatchContractError {
	return &providerPatchContractError{Field: field, Message: message}
}

// ---- 取值校验（Node 的 isValidSetValue，:215-306）----

var (
	patchHHMMPattern     = regexp.MustCompile(`^([01][0-9]|2[0-3]):[0-5][0-9]$`)
	patchDigitsPattern   = regexp.MustCompile(`^\d+$`)
	patchMatchTypeValues = map[string]bool{
		"exact": true, "prefix": true, "suffix": true, "contains": true, "regex": true,
	}
	patchAdaptiveEffortValues = map[string]bool{
		"low": true, "medium": true, "high": true, "xhigh": true, "max": true,
	}
	patchAdaptiveModeValues = map[string]bool{"specific": true, "all": true}
)

func validateBool(value any) bool { _, ok := value.(bool); return ok }

// validateNumber 对应 Node 的 `typeof value === "number" && Number.isFinite(value)`。
// json.Unmarshal 出来的数字一律是 float64，故这里只认 float64 与整数型。
func validateNumber(value any) bool {
	switch typed := value.(type) {
	case float64:
		return !isNaNOrInf(typed)
	case int64, int:
		return true
	default:
		return false
	}
}

// validateInt 在数值校验之上要求整数（JSON 里 `3.5` 不是整数；Node 侧 int 列由 PG 拒，Go 侧这里先拦）。
func validateInt(value any) bool {
	switch typed := value.(type) {
	case float64:
		return !isNaNOrInf(typed) && typed == float64(int64(typed))
	case int64, int:
		return true
	default:
		return false
	}
}

func validateAnyString(value any) bool { _, ok := value.(string); return ok }

func validateGroupTag(value any) bool {
	text, ok := value.(string)
	return ok && len([]rune(text)) <= providerPatchMaxGroupTagRune
}

func validateHHMM(value any) bool {
	text, ok := value.(string)
	return ok && patchHHMMPattern.MatchString(text)
}

func validateStringList(value any) bool {
	items, ok := value.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		if _, ok := item.(string); !ok {
			return false
		}
	}
	return true
}

func validateNumberRecord(value any) bool {
	record, ok := value.(map[string]any)
	if !ok {
		return false
	}
	for _, entry := range record {
		// 只收**整数值**（含 JS 的 `2.0`，它在 JSON 里就是整数 2，落库后也读得回来）。
		// 小数（2.5）拒掉：选路侧的 group_priorities 按 int 读，落进去就是一行读不出的覆盖。
		// 这里与 create/update 路径（providerNumberRecordJSONSpec）口径一致，区别仅在后者拿到的是
		// JSON 原文，因而能连 `2.0` 这种「读不回的写法」一起拒。
		if !validateInt(entry) {
			return false
		}
	}
	return true
}

// validateEnum 复刻 Node 的 `value === "a" || value === "b" || ...`。
func validateEnum(allowed ...string) providerPatchSetValidator {
	set := make(map[string]bool, len(allowed))
	for _, item := range allowed {
		set[item] = true
	}
	return func(value any) bool {
		text, ok := value.(string)
		return ok && set[text]
	}
}

// validateThinkingBudgetPreference 复刻 isThinkingBudgetPreference（:181-196）：inherit 或 1024..32000 的数字串。
func validateThinkingBudgetPreference(value any) bool {
	text, ok := value.(string)
	if !ok {
		return false
	}
	if text == "inherit" {
		return true
	}
	if !patchDigitsPattern.MatchString(text) {
		return false
	}
	parsed, err := parseDigits(text)
	if err != nil {
		return false
	}
	return parsed >= 1024 && parsed <= 32000
}

// validateMaxTokensPreference 复刻 isMaxTokensPreference（:198-213）：inherit 或 >0 的数字串。
func validateMaxTokensPreference(value any) bool {
	text, ok := value.(string)
	if !ok {
		return false
	}
	if text == "inherit" {
		return true
	}
	if !patchDigitsPattern.MatchString(text) {
		return false
	}
	parsed, err := parseDigits(text)
	if err != nil {
		return false
	}
	return parsed > 0
}

// validateAdaptiveThinking 复刻 isAdaptiveThinkingConfig（:152-179）。
func validateAdaptiveThinking(value any) bool {
	record, ok := value.(map[string]any)
	if !ok {
		return false
	}
	effort, ok := record["effort"].(string)
	if !ok || !patchAdaptiveEffortValues[effort] {
		return false
	}
	mode, ok := record["modelMatchMode"].(string)
	if !ok || !patchAdaptiveModeValues[mode] {
		return false
	}
	models, ok := record["models"].([]any)
	if !ok {
		return false
	}
	for _, model := range models {
		if _, ok := model.(string); !ok {
			return false
		}
	}
	if mode == "specific" && len(models) == 0 {
		return false
	}
	return true
}

// validateRedirectRules 复刻 PROVIDER_MODEL_REDIRECT_RULE_LIST_SCHEMA 的可判部分
// （src/lib/provider-model-redirect-schema.ts:14-60）：数组、每条形如 {matchType, source, target}、
// 文本 trim 后非空且 ≤4096、matchType 属枚举、regex 必须可编译。
//
// **登记差异**：Node 另用 safe-regex 做 ReDoS 启发式拒绝；Go 的 regexp 是 RE2（线性时间、无回溯），
// 该风险在 Go 侧不存在，故**不做**等价判定（不是遗漏，是语言保证更强）。
func validateRedirectRules(value any) bool {
	items, ok := value.([]any)
	if !ok || len(items) > providerPatchMaxRuleItems {
		return false
	}
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		record, ok := item.(map[string]any)
		if !ok {
			return false
		}
		matchType, ok := record["matchType"].(string)
		if !ok || !patchMatchTypeValues[matchType] {
			return false
		}
		source, ok := record["source"].(string)
		if !ok || !validRuleText(source) {
			return false
		}
		target, ok := record["target"].(string)
		if !ok || !validRuleText(target) {
			return false
		}
		if matchType == "regex" {
			if _, err := regexp.Compile(strings.TrimSpace(source)); err != nil {
				return false
			}
		}
		key := matchType + ":" + strings.TrimSpace(source)
		if seen[key] {
			return false
		}
		seen[key] = true
	}
	return true
}

// validateAllowedModelRules 复刻 PROVIDER_ALLOWED_MODEL_RULE_INPUT_LIST_SCHEMA（:64-84）的可判部分：
// 数组、每条形如 {matchType, pattern}、pattern trim 后非空且 ≤4096、去重键是 matchType:pattern。
func validateAllowedModelRules(value any) bool {
	items, ok := value.([]any)
	if !ok || len(items) > providerPatchMaxRuleItems {
		return false
	}
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		record, ok := item.(map[string]any)
		if !ok {
			return false
		}
		matchType, ok := record["matchType"].(string)
		if !ok || !patchMatchTypeValues[matchType] {
			return false
		}
		pattern, ok := record["pattern"].(string)
		if !ok || !validRuleText(pattern) {
			return false
		}
		if matchType == "regex" {
			if _, err := regexp.Compile(strings.TrimSpace(pattern)); err != nil {
				return false
			}
		}
		key := matchType + ":" + strings.TrimSpace(pattern)
		if seen[key] {
			return false
		}
		seen[key] = true
	}
	return true
}

func validRuleText(text string) bool {
	trimmed := strings.TrimSpace(text)
	return trimmed != "" && len([]rune(trimmed)) <= providerPatchMaxRuleText
}

// ---- 归一化（Node 的 normalizePatchField + normalizeProviderBatchPatchDraft）----

var providerPatchInputKeys = map[string]bool{"set": true, "clear": true, "no_change": true}

// normalizeProviderBatchPatchDraft 把任意输入归一成补丁；错误按**契约表序**返回第一条。
func normalizeProviderBatchPatchDraft(draft map[string]any) (providerBatchPatch, *providerPatchContractError) {
	if draft == nil {
		return nil, patchContractError("__root__", "Patch draft must be an object")
	}
	unknown := make([]string, 0)
	for key := range draft {
		if _, ok := providerPatchFieldByName(key); !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sortStrings(unknown)
		return nil, patchContractError("__root__", "Patch draft contains unknown fields: "+strings.Join(unknown, ","))
	}

	normalized := make(providerBatchPatch, len(draft))
	for _, spec := range providerPatchFieldSpecs {
		raw, given := draft[spec.Field]
		operation, err := normalizeProviderPatchField(spec, raw, given)
		if err != nil {
			return nil, err
		}
		normalized[spec.Field] = operation
	}
	return normalized, nil
}

// normalizeProviderPatchField 复刻 normalizePatchField（:342-406）。
//
// 注意缺键与显式 null 的**不同**：Node 的 `input === undefined` 才是 no_change；显式 null
// 会走 isRecord 失败 → INVALID_PATCH_SHAPE。JSON 解码后两者都表现为「键不存在」或「值为 nil」，
// 故这里用 given 区分。
func normalizeProviderPatchField(
	spec providerPatchFieldSpec,
	raw any,
	given bool,
) (providerPatchOperation, *providerPatchContractError) {
	if !given {
		return providerPatchOperation{Mode: providerPatchModeNoChange}, nil
	}
	input, ok := raw.(map[string]any)
	if !ok {
		return providerPatchOperation{}, patchContractError(spec.Field, "Patch input must be an object")
	}

	unknownKeys := make([]string, 0)
	for key := range input {
		if !providerPatchInputKeys[key] {
			unknownKeys = append(unknownKeys, key)
		}
	}
	if len(unknownKeys) > 0 {
		sortStrings(unknownKeys)
		return providerPatchOperation{}, patchContractError(
			spec.Field, "Patch input contains unknown keys: "+strings.Join(unknownKeys, ","))
	}

	setValue, hasSet := input["set"]
	hasClear := input["clear"] == true
	hasNoChange := input["no_change"] == true
	modeCount := 0
	for _, present := range []bool{hasSet, hasClear, hasNoChange} {
		if present {
			modeCount++
		}
	}
	if modeCount != 1 {
		return providerPatchOperation{}, patchContractError(spec.Field, "Patch input must choose exactly one mode")
	}

	if hasSet {
		if setValue == nil {
			return providerPatchOperation{}, patchContractError(spec.Field, "set mode requires a defined value")
		}
		if !spec.Validate(setValue) {
			return providerPatchOperation{}, patchContractError(spec.Field, "set mode value is invalid for this field")
		}
		converted, err := convertPatchSetValue(spec, setValue)
		if err != nil {
			return providerPatchOperation{}, patchContractError(spec.Field, err.Error())
		}
		return providerPatchOperation{Mode: providerPatchModeSet, SetValue: converted}, nil
	}

	if hasNoChange {
		return providerPatchOperation{Mode: providerPatchModeNoChange}, nil
	}

	if !spec.Clearable {
		return providerPatchOperation{}, patchContractError(spec.Field, "clear mode is not supported for this field")
	}
	return providerPatchOperation{Mode: providerPatchModeClear}, nil
}

// convertPatchSetValue 把 JSON 值转成 store 可直接绑定的 Go 值，并施加 Node 的两处 set 期规整：
// group_tag 走 normalizeProviderGroupTag（:388-393）、allowed_models 走 schema 的 parse（:395-403）。
func convertPatchSetValue(spec providerPatchFieldSpec, value any) (any, error) {
	switch spec.ValueKind {
	case patchValueBool:
		return value.(bool), nil
	case patchValueInt:
		return patchIntValue(value)
	case patchValueNullableInt:
		// 可空 int 列在 store 侧的绑定形状是 *int64（同 providerNullableTextKind 的道理）。
		number, err := patchIntValue(value)
		if err != nil {
			return nil, err
		}
		return &number, nil
	case patchValueNumeric:
		number, err := patchFloatValue(value)
		if err != nil {
			return nil, err
		}
		return &number, nil
	case patchValueText, patchValueNullableText:
		text, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("value must be a string")
		}
		if spec.Field == "group_tag" {
			// Node 的 normalizeProviderGroupTag：按中英文逗号与换行切分、去空、保序去重；空 → null。
			normalized := normalizeProviderGroupTag(text)
			if normalized == "" {
				return (*string)(nil), nil
			}
			return &normalized, nil
		}
		if spec.ValueKind == patchValueNullableText {
			// 可空文本列在 store 侧的绑定形状是 *string（见 adminProviderBindValue）。
			return &text, nil
		}
		return text, nil
	case patchValueStringList:
		items, ok := value.([]any)
		if !ok {
			return nil, fmt.Errorf("value must be an array")
		}
		list := make([]string, 0, len(items))
		for _, item := range items {
			list = append(list, item.(string))
		}
		// allowed_clients / blocked_clients 的列是 jsonb（store 侧 providerJSONKind），
		// 故这里就编成 json.RawMessage——裸 []string 会在绑定时报「值类型不符」。
		return providerStringListJSON(list)
	case patchValueJSON:
		payload, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("value is not JSON-serializable: %w", err)
		}
		return json.RawMessage(payload), nil
	default:
		return nil, fmt.Errorf("unsupported field kind")
	}
}

func patchIntValue(value any) (int64, error) {
	switch typed := value.(type) {
	case float64:
		return int64(typed), nil
	case int64:
		return typed, nil
	case int:
		return int64(typed), nil
	default:
		return 0, fmt.Errorf("value must be an integer")
	}
}

func patchFloatValue(value any) (float64, error) {
	switch typed := value.(type) {
	case float64:
		return typed, nil
	case int64:
		return float64(typed), nil
	case int:
		return float64(typed), nil
	default:
		return 0, fmt.Errorf("value must be a number")
	}
}

// providerStringListJSON 把字符串数组编成 jsonb 列的绑定形状。
func providerStringListJSON(list []string) (json.RawMessage, error) {
	encoded, err := json.Marshal(list)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(encoded), nil
}

// normalizeProviderGroupTag 复刻 normalizeProviderGroupTag（src/lib/utils/provider-group.ts:34-42）：
// 按 `[,，\n\r]+` 切分、trim、丢空、保序去重、空则返回空串（Node 返回 null）。
func normalizeProviderGroupTag(value string) string {
	fields := patchGroupSeparator.Split(value, -1)
	seen := make(map[string]bool, len(fields))
	ordered := make([]string, 0, len(fields))
	for _, field := range fields {
		trimmed := strings.TrimSpace(field)
		if trimmed == "" || seen[trimmed] {
			continue
		}
		seen[trimmed] = true
		ordered = append(ordered, trimmed)
	}
	return strings.Join(ordered, ",")
}

// ---- 应用语义（Node 的 applyPatchField + hasProviderBatchPatchChanges）----

// providerBatchApplyUpdates 是「字段 → 可直接绑定的值」的更新集（键即 providers 列名）。
type providerBatchApplyUpdates map[string]any

// hasProviderBatchPatchChanges 复刻 hasProviderBatchPatchChanges（:1096-1153）。
func hasProviderBatchPatchChanges(patch providerBatchPatch) bool {
	for _, operation := range patch {
		if operation.Mode != providerPatchModeNoChange {
			return true
		}
	}
	return false
}

// changedProviderPatchFields 复刻 getChangedPatchFields（actions:1571）——按**契约表序**。
func changedProviderPatchFields(patch providerBatchPatch) []string {
	fields := make([]string, 0, len(patch))
	for _, spec := range providerPatchFieldSpecs {
		if operation, ok := patch[spec.Field]; ok && operation.Mode != providerPatchModeNoChange {
			fields = append(fields, spec.Field)
		}
	}
	return fields
}

// buildProviderBatchApplyUpdates 复刻 buildProviderBatchApplyUpdates（:1021-1094）：
// set 取转换后的值（allowed_models 空数组 → NULL），clear 取 PATCH_FIELD_CLEAR_VALUE（缺省 NULL）。
//
// clear 的常量在契约表里是**展示形状**（`"inherit"` / `[]string{}`，同一份还要供预览行的
// 「改后值」直接显示），故在这里才按列类型转成 store 的绑定形状。
func buildProviderBatchApplyUpdates(patch providerBatchPatch) providerBatchApplyUpdates {
	updates := make(providerBatchApplyUpdates)
	for _, spec := range providerPatchFieldSpecs {
		operation, ok := patch[spec.Field]
		if !ok || operation.Mode == providerPatchModeNoChange {
			continue
		}
		if operation.Mode == providerPatchModeClear {
			if spec.ClearValue != nil {
				updates[spec.Field] = providerBatchClearValueForStore(spec)
				continue
			}
			updates[spec.Field] = nil
			continue
		}
		if spec.Field == "allowed_models" {
			if list, ok := operation.SetValue.(json.RawMessage); ok && isEmptyJSONArray(list) {
				updates[spec.Field] = nil
				continue
			}
		}
		updates[spec.Field] = operation.SetValue
	}
	return updates
}

func isEmptyJSONArray(payload json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(payload))
	return trimmed == "[]" || trimmed == "null"
}

// providerBatchClearValueForStore 把 clear 常量转成 store 的列绑定形状。
//
// 已知常量只有两种：可空文本列的 `"inherit"`（→ `*string`）与字符串数组列的 `[]string{}`
// （→ jsonb 的 json.RawMessage）。其余类型不设非 null 常量，走了也原样返回，由
// store 的绑定报错而不是在这里静默丢值。
func providerBatchClearValueForStore(spec providerPatchFieldSpec) any {
	switch spec.ValueKind {
	case patchValueNullableText:
		if text, ok := spec.ClearValue.(string); ok {
			return &text
		}
	case patchValueStringList:
		if list, ok := spec.ClearValue.([]string); ok {
			encoded, err := providerStringListJSON(list)
			if err == nil {
				return encoded
			}
		}
	}
	return spec.ClearValue
}

// computeProviderPatchPreviewAfterValue 复刻 computePreviewAfterValue（actions:1995-2016）。
func computeProviderPatchPreviewAfterValue(spec providerPatchFieldSpec, operation providerPatchOperation) any {
	if operation.Mode == providerPatchModeSet {
		if spec.Field == "allowed_models" {
			if list, ok := operation.SetValue.(json.RawMessage); ok && isEmptyJSONArray(list) {
				return nil
			}
		}
		return jsonDisplayValue(operation.SetValue)
	}
	if operation.Mode == providerPatchModeClear {
		if spec.ClearValue != nil {
			return spec.ClearValue
		}
		return nil
	}
	return nil
}

// jsonDisplayValue 把内部值还原成预览行应呈现的 JSON 形态（json.RawMessage → 解码后的值）。
func jsonDisplayValue(value any) any {
	if payload, ok := value.(json.RawMessage); ok {
		var decoded any
		if err := json.Unmarshal(payload, &decoded); err == nil {
			return decoded
		}
	}
	return value
}

// providerFieldGroupOf 返回字段的适用供应商类型（空串表示不限）。
func providerFieldGroupOf(field string) providerFieldGroup {
	spec, ok := providerPatchFieldByName(field)
	if !ok {
		return providerGroupAny
	}
	return spec.Group
}

// ---- 小工具（与 Node 语义一一对应，避免散落各处的重复实现）----

// patchGroupSeparator 复刻 provider-group.ts:3 的 `[,，\n\r]+`。
var patchGroupSeparator = regexp.MustCompile(`[,，\n\r]+`)

// isNaNOrInf 判断浮点非有限值（JS 的 Number.isFinite 取反）。
func isNaNOrInf(value float64) bool {
	return value != value || value > 1.7976931348623157e308 || value < -1.7976931348623157e308
}

// parseDigits 解析纯数字串为整数（Node 用 Number.parseInt(text, 10)；这里只需返回值与是否溢出）。
func parseDigits(text string) (int64, error) {
	var parsed int64
	for _, char := range text {
		if char < '0' || char > '9' {
			return 0, fmt.Errorf("not digits")
		}
		next := parsed*10 + int64(char-'0')
		if next < parsed {
			return 0, fmt.Errorf("out of range")
		}
		parsed = next
	}
	return parsed, nil
}
