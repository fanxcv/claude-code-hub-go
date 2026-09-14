package adminapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件把 usage-logs 的数据库行渲染成与 Node **逐字段同形**的响应体。
//
// 两条纪律：
//
//  1. **键序照抄 Node 的 select 与对象字面量**。JS 的对象键序是插入序，Node 的映射里大量使用
//     `{...row, 派生字段}`——已存在的键保持原位，新键追加在末尾。这里用 jsonObject（有序键值
//     序列）显式复刻，使 A2 的对拍可以做字节级比较，而不是只能比解析后的对象。
//  2. **派生字段逐条移植**：totalTokens、请求级缓存指标（cache-effectiveness/request-metrics.ts）、
//     统一 specialSettings（utils/special-settings.ts）与 anthropicEffort
//     （utils/anthropic-effort.ts）。这些不是展示糖——它们直接进响应体，漏一个就是字段级差异。
//
// 已知差异（登记在白名单里，均有书面理由）：
//   - routingTrace：Node 会经 normalizeRoutingTrace 校验，非法值静默置 null；这里原样透出 jsonb。
//   - specialSettings 元素的**键序**：Go 按字典序重建，Node 按 Postgres jsonb 的规范序
//     （键长短优先）。值域一致，仅键序不同。

// jsonField 是有序 JSON 对象的一个键值对。
type jsonField struct {
	Key   string
	Value any
}

// jsonObject 是有序 JSON 对象。零值即空对象。
type jsonObject []jsonField

// set 追加一个键（不覆盖：Node 的映射里同名键都由展开顺序决定，重复键不该出现）。
//
// 值接收者：链式调用方便（`jsonObject{}.set(...).set(...)`），代价是**必须接住返回值**。
func (o jsonObject) set(key string, value any) jsonObject {
	return append(o, jsonField{Key: key, Value: value})
}

// marshalJSON 按 Node 的 JSON.stringify 语义编码：不转义 HTML 字符、键序即插入序。
func (o jsonObject) marshalJSON() ([]byte, error) {
	var buffer bytes.Buffer
	if err := writeJSONValue(&buffer, o); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// writeJSONValue 递归写入 JSON。
func writeJSONValue(buffer *bytes.Buffer, value any) error {
	switch typed := value.(type) {
	case nil:
		buffer.WriteString("null")
		return nil
	case jsonObject:
		buffer.WriteByte('{')
		for index, field := range typed {
			if index > 0 {
				buffer.WriteByte(',')
			}
			if err := writeJSONString(buffer, field.Key); err != nil {
				return err
			}
			buffer.WriteByte(':')
			if err := writeJSONValue(buffer, field.Value); err != nil {
				return err
			}
		}
		buffer.WriteByte('}')
		return nil
	case []jsonObject:
		buffer.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				buffer.WriteByte(',')
			}
			if err := writeJSONValue(buffer, item); err != nil {
				return err
			}
		}
		buffer.WriteByte(']')
		return nil
	case []any:
		buffer.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				buffer.WriteByte(',')
			}
			if err := writeJSONValue(buffer, item); err != nil {
				return err
			}
		}
		buffer.WriteByte(']')
		return nil
	case []string:
		buffer.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				buffer.WriteByte(',')
			}
			if err := writeJSONString(buffer, item); err != nil {
				return err
			}
		}
		buffer.WriteByte(']')
		return nil
	case []int:
		buffer.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				buffer.WriteByte(',')
			}
			buffer.WriteString(strconv.Itoa(item))
		}
		buffer.WriteByte(']')
		return nil
	case json.RawMessage:
		if len(typed) == 0 {
			buffer.WriteString("null")
			return nil
		}
		buffer.Write(typed)
		return nil
	case json.Number:
		// 复刻 JS 的 Number 打印：整数值不带小数点，其余用最短往返表示。
		if integer, err := typed.Int64(); err == nil {
			buffer.WriteString(strconv.FormatInt(integer, 10))
			return nil
		}
		float, err := typed.Float64()
		if err != nil {
			buffer.WriteString("null")
			return nil
		}
		buffer.WriteString(formatJSONNumber(float))
		return nil
	case float64:
		buffer.WriteString(formatJSONNumber(typed))
		return nil
	case string:
		return writeJSONString(buffer, typed)
	default:
		encoded, err := marshalNoEscape(typed)
		if err != nil {
			return err
		}
		buffer.Write(encoded)
		return nil
	}
}

// writeJSONString 写一个 JSON 字符串（不转义 HTML）。
func writeJSONString(buffer *bytes.Buffer, value string) error {
	encoded, err := marshalNoEscape(value)
	if err != nil {
		return err
	}
	buffer.Write(encoded)
	return nil
}

// marshalNoEscape 与 encoding/json 的唯一区别是不转义 <、>、&（Node 的 JSON.stringify 也不转）。
func marshalNoEscape(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	encoded := buffer.Bytes()
	if len(encoded) > 0 && encoded[len(encoded)-1] == '\n' {
		encoded = encoded[:len(encoded)-1]
	}
	return encoded, nil
}

// formatJSONNumber 复刻 JS 数字打印：非有限值在 JSON 里是 null。
func formatJSONNumber(value float64) string {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "null"
	}
	if value == math.Trunc(value) && math.Abs(value) < 1e15 {
		return strconv.FormatInt(int64(value), 10)
	}
	return strconv.FormatFloat(value, 'g', -1, 64)
}

// jsonTime 把时间渲染成 Node 的 Date.toISOString() 形式（毫秒恒三位、UTC、Z 结尾）。
func jsonTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return value.UTC().Format("2006-01-02T15:04:05.000Z")
}

// jsonBytes 把 jsonb 列透出为 JSON 值：空/无效一律 null（与 `?? null` 一致）。
func jsonBytes(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	return json.RawMessage(trimmed)
}

// intPointerValue 解引用；nil 即 null。
func intPointerValue[T int | int64](value *T) any {
	if value == nil {
		return nil
	}
	return *value
}

// stringPointerValue 解引用；nil 即 null。
func stringPointerValue(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// boolPointerValue 解引用；nil 即 null。
func boolPointerValue(value *bool) any {
	if value == nil {
		return nil
	}
	return *value
}

// cacheMetricAvailability 复刻 request-metrics.ts:1 的取值域。
type cacheMetricAvailability string

const (
	cacheMetricAvailable       cacheMetricAvailability = "available"
	cacheMetricNoInput         cacheMetricAvailability = "no_input"
	cacheMetricNoAffinityKey   cacheMetricAvailability = "no_affinity_key"
	cacheMetricAttemptFailed   cacheMetricAvailability = "attempt_failed"
	cacheMetricNotObservable   cacheMetricAvailability = "not_observable"
	cacheMetricStreamTruncated cacheMetricAvailability = "stream_truncated"
	cacheMetricNotRecorded     cacheMetricAvailability = "not_recorded"
)

// cacheMetrics 是请求级缓存指标（request-metrics.ts:24）。
type cacheMetrics struct {
	CacheInputTotal                int64
	ActualCacheRate                any
	TheoreticalCacheRate           any
	RequestCacheCoefficientBP      any
	RequestCacheMetricAvailability cacheMetricAvailability
}

// deriveRequestCacheMetrics 复刻 deriveRequestCacheMetrics（request-metrics.ts:46）。
func deriveRequestCacheMetrics(
	inputTokens *int64,
	cacheCreationInputTokens *int64,
	cacheReadInputTokens *int64,
	theoreticalCacheTokens *int64,
	cacheScoreEligible *bool,
	cacheScoreExcludedReason *string,
) cacheMetrics {
	cacheInputTotal := finiteNonNegative(inputTokens) +
		finiteNonNegative(cacheCreationInputTokens) +
		finiteNonNegative(cacheReadInputTokens)

	var actualCacheRate any
	if cacheInputTotal > 0 {
		actualCacheRate = clamp01(float64(finiteNonNegative(cacheReadInputTokens)) /
			float64(cacheInputTotal))
	}

	hasAnyF3bField := theoreticalCacheTokens != nil ||
		cacheScoreEligible != nil ||
		cacheScoreExcludedReason != nil
	var theoreticalTokens *int64
	if theoreticalCacheTokens != nil {
		value := finiteNonNegative(theoreticalCacheTokens)
		theoreticalTokens = &value
	}
	normalizedReason := normalizeExcludedReason(cacheScoreExcludedReason)

	if !hasAnyF3bField {
		return cacheMetrics{
			CacheInputTotal:                cacheInputTotal,
			ActualCacheRate:                actualCacheRate,
			TheoreticalCacheRate:           nil,
			RequestCacheCoefficientBP:      nil,
			RequestCacheMetricAvailability: cacheMetricNotRecorded,
		}
	}

	if normalizedReason != "" {
		if cacheInputTotal <= 0 {
			return cacheMetrics{
				CacheInputTotal:                cacheInputTotal,
				ActualCacheRate:                nil,
				TheoreticalCacheRate:           nil,
				RequestCacheCoefficientBP:      nil,
				RequestCacheMetricAvailability: normalizedReason,
			}
		}
		return cacheMetrics{
			CacheInputTotal:                cacheInputTotal,
			ActualCacheRate:                actualCacheRate,
			TheoreticalCacheRate:           theoreticalRate(theoreticalTokens, cacheInputTotal),
			RequestCacheCoefficientBP:      nil,
			RequestCacheMetricAvailability: normalizedReason,
		}
	}

	if cacheInputTotal <= 0 {
		return cacheMetrics{
			CacheInputTotal:                cacheInputTotal,
			ActualCacheRate:                nil,
			TheoreticalCacheRate:           nil,
			RequestCacheCoefficientBP:      nil,
			RequestCacheMetricAvailability: cacheMetricNoInput,
		}
	}

	availability := cacheMetricNoAffinityKey
	if theoreticalTokens != nil {
		availability = cacheMetricAvailable
	}
	coefficientAvailable := theoreticalTokens != nil && *theoreticalTokens > 0 &&
		(cacheScoreEligible == nil || *cacheScoreEligible)
	var coefficient any
	if coefficientAvailable {
		raw := int64(finiteNonNegative(cacheReadInputTokens)) * 10000 / *theoreticalTokens
		if raw < 0 {
			raw = 0
		}
		if raw > 10000 {
			raw = 10000
		}
		coefficient = raw
	}
	return cacheMetrics{
		CacheInputTotal:                cacheInputTotal,
		ActualCacheRate:                actualCacheRate,
		TheoreticalCacheRate:           theoreticalRate(theoreticalTokens, cacheInputTotal),
		RequestCacheCoefficientBP:      coefficient,
		RequestCacheMetricAvailability: availability,
	}
}

func theoreticalRate(theoreticalTokens *int64, cacheInputTotal int64) any {
	if theoreticalTokens == nil {
		return nil
	}
	return clamp01(float64(*theoreticalTokens) / float64(cacheInputTotal))
}

// finiteNonNegative 复刻 finiteNonNegative：非有限或负数一律 0。
func finiteNonNegative(value *int64) int64 {
	if value == nil || *value < 0 {
		return 0
	}
	return *value
}

// clamp01 复刻 clamp01。
func clamp01(value float64) float64 {
	return math.Min(math.Max(value, 0), 1)
}

// normalizeExcludedReason 复刻 normalizeExcludedReason：只认三种排除理由。
func normalizeExcludedReason(reason *string) cacheMetricAvailability {
	if reason == nil || *reason == "" {
		return ""
	}
	switch cacheMetricAvailability(*reason) {
	case cacheMetricAttemptFailed, cacheMetricNotObservable, cacheMetricStreamTruncated:
		return cacheMetricAvailability(*reason)
	case cacheMetricNoAffinityKey:
		return cacheMetricNoAffinityKey
	default:
		return ""
	}
}

// extractAnthropicEffort 复刻 extractAnthropicEffortFromSpecialSettings
// （utils/anthropic-effort.ts:29）：取第一条 anthropic_effort 的非空 effort。
func extractAnthropicEffort(settings []map[string]any) any {
	for _, setting := range settings {
		if settingType(setting) != "anthropic_effort" {
			continue
		}
		if effort, ok := setting["effort"].(string); ok {
			trimmed := strings.TrimSpace(effort)
			if trimmed != "" {
				return trimmed
			}
		}
	}
	return nil
}

func settingType(setting map[string]any) string {
	value, _ := setting["type"].(string)
	return value
}

// unionSetting 统一展示用的 specialSettings（utils/special-settings.ts:220）。
//
// 合并规则与 Node 一致：先放 DB 里已有的（保持原序），再依次追加由 blockedBy 与
// cacheTtlApplied 派生出的两条；按「类型 + 关键字段」去重，保留首次出现者。
func unionSetting(
	existing []map[string]any,
	blockedBy *string,
	blockedReason *string,
	statusCode *int,
	cacheTTLApplied *string,
) []jsonObject {
	derived := make([]jsonObject, 0, 2)
	if blockedBy != nil && *blockedBy != "" {
		action := "block_request"
		if *blockedBy == "warmup" {
			action = "intercept_response"
		}
		derived = append(derived, jsonObject{}.
			set("type", "guard_intercept").
			set("scope", "guard").
			set("hit", true).
			set("guard", *blockedBy).
			set("action", action).
			set("statusCode", intPointerValue(statusCode)).
			set("reason", stringPointerValue(blockedReason)))
	}
	if cacheTTLApplied != nil && *cacheTTLApplied != "" {
		derived = append(derived, jsonObject{}.
			set("type", "anthropic_cache_ttl_header_override").
			set("scope", "request_header").
			set("hit", true).
			set("ttl", *cacheTTLApplied))
	}
	if len(existing) == 0 && len(derived) == 0 {
		return nil
	}

	seen := map[string]struct{}{}
	results := make([]jsonObject, 0, len(existing)+len(derived))
	for _, setting := range existing {
		key := settingDedupKey(setting)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		results = append(results, settingToJSONObject(setting))
	}
	for _, setting := range derived {
		key := jsonObjectDedupKey(setting)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		results = append(results, setting)
	}
	if len(results) == 0 {
		return nil
	}
	return results
}

// settingToJSONObject 把 jsonb 里的设置对象转成有序对象：键序按字典序（见文件头差异说明）。
func settingToJSONObject(setting map[string]any) jsonObject {
	keys := make([]string, 0, len(setting))
	for key := range setting {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	object := make(jsonObject, 0, len(keys))
	for _, key := range keys {
		object = append(object, jsonField{Key: key, Value: normalizeJSONValue(setting[key])})
	}
	return object
}

// normalizeJSONValue 统一 jsonb 解码后的值：json.Number 保持为数字、嵌套对象转有序对象。
func normalizeJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return settingToJSONObject(typed)
	case []any:
		items := make([]any, 0, len(typed))
		for _, item := range typed {
			items = append(items, normalizeJSONValue(item))
		}
		return items
	default:
		return value
	}
}

// settingDedupKey 复刻 buildSettingKey（special-settings.ts:20）。
func settingDedupKey(setting map[string]any) string {
	return dedupKeyForType(settingType(setting), setting)
}

func jsonObjectDedupKey(object jsonObject) string {
	values := map[string]any{}
	for _, field := range object {
		values[field.Key] = field.Value
	}
	return settingDedupKey(values)
}

// dedupKeyForType 按类型列出参与去重的字段序列，并编码为 JSON 数组。
//
// Node 用 `JSON.stringify([...])` 直接比较字符串，因此字段**顺序必须一致**；
// changes/fixersApplied 两类还要按首元素排序（Node 用 localeCompare，ASCII 路径下与字典序同）。
func dedupKeyForType(settingTypeValue string, setting map[string]any) string {
	var fields []any
	switch settingTypeValue {
	case "provider_parameter_override":
		fields = []any{
			settingTypeValue,
			orNull(setting["providerId"]),
			orNull(setting["providerType"]),
			orNull(setting["hit"]),
			orNull(setting["changed"]),
			sortedPairs(setting["changes"], []string{"path", "before", "after", "changed"}),
		}
	case "response_fixer":
		fields = []any{
			settingTypeValue,
			orNull(setting["hit"]),
			sortedPairs(setting["fixersApplied"], []string{"fixer", "applied"}),
		}
	case "guard_intercept":
		fields = []any{
			settingTypeValue,
			orNull(setting["guard"]),
			orNull(setting["action"]),
			orNull(setting["statusCode"]),
		}
	case "anthropic_effort":
		fields = []any{settingTypeValue, orNull(setting["hit"]), orNull(setting["effort"])}
	case "codex_reasoning_effort":
		fields = []any{settingTypeValue, orNull(setting["hit"]), orNull(setting["effort"])}
	case "openai_reasoning_effort":
		fields = []any{
			settingTypeValue, orNull(setting["hit"]), orNull(setting["effort"]),
			orNull(setting["source"]),
		}
	case "protocol_conversion":
		fields = []any{
			settingTypeValue, orNull(setting["clientProtocol"]), orNull(setting["targetProtocol"]),
		}
	case "anthropic_cache_ttl_header_override":
		fields = []any{settingTypeValue, orNull(setting["ttl"])}
	case "anthropic_context_1m_header_override":
		fields = []any{
			settingTypeValue, orNull(setting["header"]), orNull(setting["flag"]),
		}
	case "long_context_pricing":
		fields = []any{
			settingTypeValue, orNull(setting["hit"]), orNull(setting["pricingScope"]),
			orNull(setting["thresholdTokens"]),
		}
	case "thinking_signature_rectifier":
		fields = []any{
			settingTypeValue, orNull(setting["hit"]), orNull(setting["providerId"]),
			orNull(setting["trigger"]), orNull(setting["attemptNumber"]),
			orNull(setting["retryAttemptNumber"]), orNull(setting["removedThinkingBlocks"]),
			orNull(setting["removedRedactedThinkingBlocks"]),
			orNull(setting["removedSignatureFields"]),
		}
	case "thinking_effort_conflict_rectifier":
		fields = []any{
			settingTypeValue, orNull(setting["hit"]), orNull(setting["providerId"]),
			orNull(setting["trigger"]), orNull(setting["attemptNumber"]),
			orNull(setting["retryAttemptNumber"]), orNull(setting["removedOutputConfigEffort"]),
			orNull(setting["removedReasoningEffort"]), orNull(setting["thinkingType"]),
			orNull(setting["effort"]),
		}
	case "codex_session_id_completion":
		fields = []any{
			settingTypeValue, orNull(setting["hit"]), orNull(setting["action"]),
			orNull(setting["source"]), orNull(setting["sessionId"]),
		}
	case "claude_metadata_user_id_injection":
		fields = []any{
			settingTypeValue, orNull(setting["hit"]), orNull(setting["action"]),
			orNull(setting["reason"]), orNull(setting["keyId"]), orNull(setting["sessionId"]),
		}
	case "thinking_budget_rectifier":
		before, _ := setting["before"].(map[string]any)
		after, _ := setting["after"].(map[string]any)
		fields = []any{
			settingTypeValue, orNull(setting["hit"]), orNull(setting["providerId"]),
			orNull(setting["trigger"]), orNull(setting["attemptNumber"]),
			orNull(setting["retryAttemptNumber"]), orNull(before["maxTokens"]),
			orNull(before["thinkingBudgetTokens"]), orNull(after["maxTokens"]),
			orNull(after["thinkingBudgetTokens"]),
		}
	case "billing_header_rectifier":
		fields = []any{settingTypeValue, orNull(setting["hit"]), orNull(setting["removedCount"])}
	case "gemini_function_id_rectifier":
		fields = []any{
			settingTypeValue, orNull(setting["hit"]), orNull(setting["providerId"]),
			orNull(setting["trigger"]), orNull(setting["attemptNumber"]),
			orNull(setting["retryAttemptNumber"]), orNull(setting["strippedFunctionCallIds"]),
			orNull(setting["strippedFunctionResponseIds"]),
		}
	case "gemini_google_search_override":
		fields = []any{
			settingTypeValue, orNull(setting["hit"]), orNull(setting["providerId"]),
			orNull(setting["action"]), orNull(setting["preference"]),
			orNull(setting["hadGoogleSearchInRequest"]),
		}
	case "pricing_resolution":
		fields = []any{
			settingTypeValue, orNull(setting["hit"]), orNull(setting["modelName"]),
			orNull(setting["resolvedModelName"]), orNull(setting["resolvedPricingProviderKey"]),
			orNull(setting["source"]),
		}
	case "codex_service_tier_result":
		fields = []any{
			settingTypeValue, orNull(setting["hit"]), orNull(setting["requestedServiceTier"]),
			orNull(setting["actualServiceTier"]), orNull(setting["billingSourcePreference"]),
			orNull(setting["resolvedFrom"]), orNull(setting["effectivePriority"]),
		}
	case "response_input_rectifier":
		fields = []any{
			settingTypeValue, orNull(setting["hit"]), orNull(setting["action"]),
			orNull(setting["originalType"]),
		}
	case "thinking_signature_model_detection":
		fields = []any{
			settingTypeValue, orNull(setting["source"]), orNull(setting["signatureFound"]),
			orNull(setting["thinkingEnabled"]), orNull(setting["extractedModel"]),
			orNull(setting["requestedModel"]),
		}
	default:
		// 兜底：未知类型（未来扩展）按字典序整对象编码，保证不崩且稳定。
		return string(settingToJSONObject(setting).mustMarshal())
	}
	return string(mustMarshalOrdered(fields))
}

// mustMarshalOrdered 编码任意有序值；失败时退化成一个稳定占位（去重键只求稳定）。
func mustMarshalOrdered(value any) []byte {
	var buffer bytes.Buffer
	if err := writeJSONValue(&buffer, value); err != nil {
		return []byte(`"unencodable"`)
	}
	return buffer.Bytes()
}

func (o jsonObject) mustMarshal() []byte { return mustMarshalOrdered(o) }

// orNull 复刻 JS 的 `x ?? null`。
func orNull(value any) any {
	if value == nil {
		return nil
	}
	return normalizeJSONValue(value)
}

// sortedPairs 复刻 `[...arr].map(...).sort((a,b) => a[0].localeCompare(b[0]))`。
func sortedPairs(value any, fields []string) any {
	items, ok := value.([]any)
	if !ok {
		return []any{}
	}
	pairs := make([][]any, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			continue
		}
		pair := make([]any, 0, len(fields))
		for _, field := range fields {
			pair = append(pair, orNull(object[field]))
		}
		pairs = append(pairs, pair)
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		return comparePairFirst(pairs[i], pairs[j]) < 0
	})
	results := make([]any, 0, len(pairs))
	for _, pair := range pairs {
		results = append(results, pair)
	}
	return results
}

func comparePairFirst(left, right []any) int {
	if len(left) == 0 || len(right) == 0 {
		return 0
	}
	leftText, _ := left[0].(string)
	rightText, _ := right[0].(string)
	return strings.Compare(leftText, rightText)
}

// decodeSettings 把 jsonb specialSettings 解码为对象序列（数字用 json.Number，保持精度）。
func decodeSettings(raw []byte) []map[string]any {
	if len(raw) == 0 {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var items []map[string]any
	if err := decoder.Decode(&items); err != nil {
		return nil
	}
	return items
}

// usageLogRowFields 复刻 Node 的行对象字面量：键序 = select 顺序，末尾追加派生字段。
//
// cursorPath 为真时包含 createdAtRaw（Node 的游标查询多取了这一列并原样透出）。
func usageLogRowFields(row store.UsageLogRow, cursorPath bool) jsonObject {
	metrics := deriveRequestCacheMetrics(
		row.InputTokens, row.CacheCreationInputTokens, row.CacheReadInputTokens,
		row.TheoreticalCacheTokens, row.CacheScoreEligible, row.CacheScoreExcludedReason,
	)
	settings := decodeSettings(row.SpecialSettings)
	unified := unionSetting(
		settings, row.BlockedBy, row.BlockedReason, row.StatusCode, row.CacheTTLApplied,
	)

	object := jsonObject{}.
		set("id", row.ID).
		set("createdAt", jsonTime(row.CreatedAt))
	if cursorPath {
		object = object.set("createdAtRaw", stringPointerValue(row.CreatedAtRaw))
	}
	object = object.
		set("sessionId", stringPointerValue(row.SessionID)).
		set("sourceSessionId", stringPointerValue(row.SourceSessionID)).
		set("sessionIdentityKind", stringPointerValue(row.SessionIdentityKind)).
		set("requestSequence", intPointerValue(row.RequestSequence)).
		set("userName", stringPointerValue(row.UserName)).
		set("keyName", stringPointerValue(row.KeyName)).
		set("providerName", stringPointerValue(row.ProviderName)).
		set("model", stringPointerValue(row.Model)).
		set("originalModel", stringPointerValue(row.OriginalModel)).
		set("actualResponseModel", stringPointerValue(row.ActualResponseModel)).
		set("endpoint", stringPointerValue(row.Endpoint)).
		set("statusCode", intPointerValue(row.StatusCode)).
		set("inputTokens", intPointerValue(row.InputTokens)).
		set("outputTokens", intPointerValue(row.OutputTokens)).
		set("cacheCreationInputTokens", intPointerValue(row.CacheCreationInputTokens)).
		set("cacheReadInputTokens", intPointerValue(row.CacheReadInputTokens)).
		set("cacheCreation5mInputTokens", intPointerValue(row.CacheCreation5mInputTokens)).
		set("cacheCreation1hInputTokens", intPointerValue(row.CacheCreation1hInputTokens)).
		set("cacheTtlApplied", stringPointerValue(row.CacheTTLApplied)).
		set("theoreticalCacheTokens", intPointerValue(row.TheoreticalCacheTokens)).
		set("cacheScoreEligible", boolPointerValue(row.CacheScoreEligible)).
		set("cacheScoreExcludedReason", stringPointerValue(row.CacheScoreExcludedReason)).
		set("costUsd", stringPointerValue(row.CostUSD)).
		set("costMultiplier", stringPointerValue(row.CostMultiplier)).
		set("groupCostMultiplier", stringPointerValue(row.GroupCostMultiplier)).
		set("costBreakdown", jsonBytes(row.CostBreakdown)).
		set("hedgeLosers", jsonBytes(row.HedgeLosers)).
		set("durationMs", intPointerValue(row.DurationMs)).
		set("ttftMs", intPointerValue(row.TTFBMs)).
		set("firstByteMs", intPointerValue(row.FirstByteMs)).
		set("errorMessage", stringPointerValue(row.ErrorMessage)).
		set("providerChain", jsonBytes(row.ProviderChain)).
		set("routingTrace", jsonBytes(row.RoutingTrace)).
		set("blockedBy", stringPointerValue(row.BlockedBy)).
		set("blockedReason", stringPointerValue(row.BlockedReason)).
		set("isReplay", row.IsReplay).
		set("replaySourceRequestId", intPointerValue(row.ReplaySourceRequestID)).
		set("userAgent", stringPointerValue(row.UserAgent)).
		set("clientIp", stringPointerValue(row.ClientIP)).
		set("messagesCount", intPointerValue(row.MessagesCount)).
		set("context1mApplied", boolPointerValue(row.Context1mApplied)).
		set("swapCacheTtlApplied", boolPointerValue(row.SwapCacheTTLApplied)).
		set("specialSettings", settingsValue(unified))

	// 派生字段：Node 的 `{...row, totalTokens, ...cacheMetrics, anthropicEffort}` 会把新键追加在末尾。
	object = object.
		set("totalTokens", totalRowTokens(row)).
		set("cacheInputTotal", metrics.CacheInputTotal).
		set("actualCacheRate", metrics.ActualCacheRate).
		set("theoreticalCacheRate", metrics.TheoreticalCacheRate).
		set("requestCacheCoefficientBp", metrics.RequestCacheCoefficientBP).
		set("requestCacheMetricAvailability", string(metrics.RequestCacheMetricAvailability)).
		set("anthropicEffort", extractAnthropicEffort(settings))
	return object
}

// settingsValue 把统一设置转成 JSON 值：nil 即 null，否则按顺序输出。
func settingsValue(settings []jsonObject) any {
	if settings == nil {
		return nil
	}
	return settings
}

// totalRowTokens 复刻 totalTokens：四项 token 求和（nil 视为 0）。
func totalRowTokens(row store.UsageLogRow) int64 {
	return rawTokenTotal(
		row.InputTokens, row.OutputTokens,
		row.CacheCreationInputTokens, row.CacheReadInputTokens,
	)
}

func rawTokenTotal(values ...*int64) int64 {
	total := int64(0)
	for _, value := range values {
		if value != nil {
			total += *value
		}
	}
	return total
}

// ledgerFallbackRowFields 复刻 findUsageLogsBatch 回退分支的对象字面量（键序即字面量序）。
func ledgerFallbackRowFields(row store.LedgerUsageLogRow) jsonObject {
	metrics := deriveRequestCacheMetrics(
		row.InputTokens, row.CacheCreationInputTokens, row.CacheReadInputTokens, nil, nil, nil,
	)
	// userName/keyName 的兜底与 Node 一致：`row.userName ?? "User #" + row.userId`。
	userName := stringPointerValue(row.UserName)
	if row.UserName == nil {
		userID := int64(0)
		if row.UserID != nil {
			userID = *row.UserID
		}
		userName = fmt.Sprintf("User #%d", userID)
	}
	keyName := stringPointerValue(row.KeyName)
	if row.KeyName == nil {
		keyName = stringPointerValue(row.Key)
	}

	return jsonObject{}.
		set("id", row.ID).
		set("createdAt", jsonTime(row.CreatedAt)).
		set("sessionId", stringPointerValue(row.SessionID)).
		set("sourceSessionId", stringPointerValue(row.SourceSessionID)).
		set("sessionIdentityKind", stringPointerValue(row.SessionIdentityKind)).
		set("requestSequence", nil).
		set("userName", userName).
		set("keyName", keyName).
		set("providerName", stringPointerValue(row.ProviderName)).
		set("model", stringPointerValue(row.Model)).
		set("originalModel", stringPointerValue(row.OriginalModel)).
		set("actualResponseModel", stringPointerValue(row.ActualResponseModel)).
		set("endpoint", stringPointerValue(row.Endpoint)).
		set("statusCode", intPointerValue(row.StatusCode)).
		set("inputTokens", intPointerValue(row.InputTokens)).
		set("outputTokens", intPointerValue(row.OutputTokens)).
		set("cacheCreationInputTokens", intPointerValue(row.CacheCreationInputTokens)).
		set("cacheReadInputTokens", intPointerValue(row.CacheReadInputTokens)).
		set("cacheCreation5mInputTokens", intPointerValue(row.CacheCreation5mInputTokens)).
		set("cacheCreation1hInputTokens", intPointerValue(row.CacheCreation1hInputTokens)).
		set("cacheTtlApplied", stringPointerValue(row.CacheTTLApplied)).
		set("theoreticalCacheTokens", nil).
		set("cacheScoreEligible", nil).
		set("cacheScoreExcludedReason", nil).
		set("cacheInputTotal", metrics.CacheInputTotal).
		set("actualCacheRate", metrics.ActualCacheRate).
		set("theoreticalCacheRate", metrics.TheoreticalCacheRate).
		set("requestCacheCoefficientBp", metrics.RequestCacheCoefficientBP).
		set("requestCacheMetricAvailability", string(metrics.RequestCacheMetricAvailability)).
		set("totalTokens", rawTokenTotal(
			row.InputTokens, row.OutputTokens,
			row.CacheCreationInputTokens, row.CacheReadInputTokens,
		)).
		set("costUsd", stringPointerValue(row.CostUSD)).
		set("costMultiplier", stringPointerValue(row.CostMultiplier)).
		set("groupCostMultiplier", stringPointerValue(row.GroupCostMultiplier)).
		set("costBreakdown", nil).
		set("hedgeLosers", nil).
		set("durationMs", intPointerValue(row.DurationMs)).
		set("ttftMs", intPointerValue(row.TTFBMs)).
		set("firstByteMs", intPointerValue(row.FirstByteMs)).
		set("errorMessage", nil).
		set("providerChain", nil).
		set("routingTrace", nil).
		set("blockedBy", nil).
		set("blockedReason", nil).
		set("isReplay", row.IsReplay).
		set("replaySourceRequestId", intPointerValue(row.ReplaySourceRequestID)).
		set("userAgent", nil).
		set("clientIp", stringPointerValue(row.ClientIP)).
		set("messagesCount", nil).
		set("context1mApplied", boolPointerValue(row.Context1mApplied)).
		set("swapCacheTtlApplied", boolPointerValue(row.SwapCacheTTLApplied)).
		set("specialSettings", nil)
}
