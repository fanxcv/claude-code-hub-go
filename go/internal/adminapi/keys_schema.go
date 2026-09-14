package adminapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 keys 资源的请求校验层（A1-2）：把 Node 的 zod schema 逐条落成显式校验。
//
// 对应关系（src/lib/api/v1/schemas/keys.ts）：
//
//	KeyListQuerySchema   → keysParseListQuery
//	KeyCreateSchema      → keysParseCreate（.strict()，未知名段即错）
//	KeyUpdateSchema      → keysParseUpdate（name 必填）
//	KeyEnableSchema      → keysParseEnable
//	KeyRenewSchema       → keysParseRenew
//	KeysBatchUpdateSchema→ keysParseBatchUpdate
//
// 三处刻意的取舍：
//
//  1. 校验失败一律作答 `WriteValidationError`（zod 形状的 400：title "Validation failed" +
//     invalidParams），与 Node 的 fromZodError 同形。invalidParams 的 message 文案不逐字对齐
//     zod（zod 的英文默认消息随版本变化），code 取 zod 的语义码；A2 对拍把文案登记为允许差异。
//  2. 金额用 json.Number 保留十进制原文：numeric(10,2) 的精度不该由 float64 决定（同 keys.go
//     限额快捷编辑的处置）。
//  3. 「未提供」与「显式 null」必须分开：KeyUpdateSchema 里没传 expiresAt 表示保持不动，传了
//     null 表示清空该列（actions/keys.ts:508-520 用 Object.hasOwn 判存在）。故解析结果用
//     store.Nullable 表达三态，而不是 *T。

// keysFieldLimits 是各金额字段的取值上界，逐字取自 KeyMutationFields（schemas/keys.ts:19-58）。
const (
	keysLimit5hMax       = 10_000
	keysLimitDailyMax    = 10_000
	keysLimitWeeklyMax   = 50_000
	keysLimitMonthlyMax  = 200_000
	keysLimitTotalMax    = 10_000_000
	keysConcurrentMax    = 1000
	keysNameMaxLength    = 64
	keysProviderGroupMax = 200
	keysBatchMaxSize     = 500
)

// keysResetModes/keysCacheTTLPreferences 是枚举取值（schemas/keys.ts:3-4）。
var (
	keysResetModes          = []string{"fixed", "rolling"}
	keysCacheTTLPreferences = []string{"inherit", "5m", "1h"}
	keysListIncludeValues   = []string{"statistics"}
)

// keysInvalid 造一个 invalidParams 项（path 为字段名，单段路径）。
func keysInvalid(field, code, message string) InvalidParam {
	return InvalidParam{Path: []any{field}, Code: code, Message: message}
}

// keysInvalidType 造 invalid_type（zod 对缺失或类型不符的通用码）。
func keysInvalidType(field string) InvalidParam {
	return keysInvalid(field, "invalid_type", fmt.Sprintf("Invalid input for %q.", field))
}

// keysReadBody 读取请求体为「字段 → 原始 JSON」的映射。
//
// 空体在 Node 里是 zod 的 invalid_type（必填对象缺失），故空体返回空映射，由各 schema 自己
// 决定哪个字段必填。
func keysReadBody(request *http.Request) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(request.Body)
	var raw map[string]json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return map[string]json.RawMessage{}, nil
		}
		return nil, err
	}
	if raw == nil {
		return map[string]json.RawMessage{}, nil
	}
	return raw, nil
}

// keysRejectUnknown 复刻 zod 的 .strict()：出现未知名段即该项错误。
func keysRejectUnknown(raw map[string]json.RawMessage, allowed ...string) []InvalidParam {
	known := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		known[name] = struct{}{}
	}
	problems := make([]InvalidParam, 0, 1)
	for name := range raw {
		if _, ok := known[name]; ok {
			continue
		}
		problems = append(problems, InvalidParam{
			Path:    []any{name},
			Code:    "unrecognized_keys",
			Message: fmt.Sprintf("Unrecognized key %q.", name),
		})
	}
	// map 迭代无序：排序让同一请求的错误顺序稳定（A2 对拍时逐项比对）。
	keysSortInvalid(problems)
	return problems
}

// keysSortInvalid 按 path 排序 invalidParams，保证响应稳定。
func keysSortInvalid(problems []InvalidParam) {
	for index := 1; index < len(problems); index++ {
		for cursor := index; cursor > 0 && keysInvalidLess(problems[cursor], problems[cursor-1]); cursor-- {
			problems[cursor], problems[cursor-1] = problems[cursor-1], problems[cursor]
		}
	}
}

func keysInvalidLess(left, right InvalidParam) bool {
	leftPath := fmt.Sprint(left.Path...)
	rightPath := fmt.Sprint(right.Path...)
	return leftPath < rightPath
}

// keysString 解析必填/可选字符串字段（trim 语义由调用方给出）。
func keysString(raw map[string]json.RawMessage, field string, required bool) (*string, []InvalidParam) {
	value, ok := raw[field]
	if !ok {
		if required {
			return nil, []InvalidParam{keysInvalidType(field)}
		}
		return nil, nil
	}
	if keysIsJSONNull(value) {
		return nil, []InvalidParam{keysInvalid(field, "invalid_type", "Expected string, received null")}
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		return nil, []InvalidParam{keysInvalidType(field)}
	}
	return &text, nil
}

// keysNullableString 解析「字符串 | null」字段（null 是合法取值，如 expiresAt / providerGroup）。
func keysNullableString(raw map[string]json.RawMessage, field string) (store.Nullable[string], []InvalidParam) {
	value, ok := raw[field]
	if !ok {
		return store.Nullable[string]{}, nil
	}
	if keysIsJSONNull(value) {
		return store.ExplicitNull[string](), nil
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		return store.Nullable[string]{}, []InvalidParam{keysInvalidType(field)}
	}
	return store.SomeValue(text), nil
}

// keysBool 解析布尔字段。
func keysBool(raw map[string]json.RawMessage, field string, required bool) (*bool, []InvalidParam) {
	value, ok := raw[field]
	if !ok {
		if required {
			return nil, []InvalidParam{keysInvalidType(field)}
		}
		return nil, nil
	}
	var flag bool
	if err := json.Unmarshal(value, &flag); err != nil {
		return nil, []InvalidParam{keysInvalid(field, "invalid_type", "Expected boolean")}
	}
	return &flag, nil
}

// keysMoney 解析「数值 | null」金额字段：保留十进制原文，并校验上限。
func keysMoney(
	raw map[string]json.RawMessage,
	field string,
	maxValue float64,
) (store.Nullable[string], []InvalidParam) {
	value, ok := raw[field]
	if !ok {
		return store.Nullable[string]{}, nil
	}
	if keysIsJSONNull(value) {
		return store.ExplicitNull[string](), nil
	}
	text, number, problems := keysNumber(value, field)
	if problems != nil {
		return store.Nullable[string]{}, problems
	}
	if number < 0 {
		return store.Nullable[string]{}, []InvalidParam{
			keysInvalid(field, "too_small", "Number must be greater than or equal to 0"),
		}
	}
	if number > maxValue {
		return store.Nullable[string]{}, []InvalidParam{
			keysInvalid(field, "too_big", fmt.Sprintf("Number must be less than or equal to %g", maxValue)),
		}
	}
	return store.SomeValue(text), nil
}

// keysNumber 把原始 JSON 数值解成（十进制原文, 浮点值）。
func keysNumber(value json.RawMessage, field string) (string, float64, []InvalidParam) {
	text := strings.TrimSpace(string(value))
	number, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
		return "", 0, []InvalidParam{keysInvalid(field, "invalid_type", "Expected number")}
	}
	return text, number, nil
}

// keysInt 解析整数「数值 | null」字段（限额并发数支持 null 清空之外的场景）。
func keysInt(
	raw map[string]json.RawMessage,
	field string,
	required bool,
) (store.Nullable[int32], []InvalidParam) {
	value, ok := raw[field]
	if !ok {
		if required {
			return store.Nullable[int32]{}, []InvalidParam{keysInvalidType(field)}
		}
		return store.Nullable[int32]{}, nil
	}
	if keysIsJSONNull(value) {
		return store.ExplicitNull[int32](), nil
	}
	text := strings.TrimSpace(string(value))
	if strings.ContainsAny(text, ".eE") {
		return store.Nullable[int32]{}, []InvalidParam{
			keysInvalid(field, "invalid_type", "Expected integer, received float"),
		}
	}
	parsed, err := strconv.ParseInt(text, 10, 32)
	if err != nil {
		return store.Nullable[int32]{}, []InvalidParam{keysInvalidType(field)}
	}
	if parsed < 0 {
		return store.Nullable[int32]{}, []InvalidParam{
			keysInvalid(field, "too_small", "Number must be greater than or equal to 0"),
		}
	}
	if parsed > keysConcurrentMax {
		return store.Nullable[int32]{}, []InvalidParam{
			keysInvalid(field, "too_big", "Number must be less than or equal to 1000"),
		}
	}
	return store.SomeValue(int32(parsed)), nil
}

// keysEnum 解析枚举字段。
func keysEnum(
	raw map[string]json.RawMessage,
	field string,
	allowed []string,
	required bool,
) (*string, []InvalidParam) {
	value, ok := raw[field]
	if !ok {
		if required {
			return nil, []InvalidParam{keysInvalidType(field)}
		}
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		return nil, []InvalidParam{keysInvalid(field, "invalid_type", "Invalid enum value")}
	}
	for _, candidate := range allowed {
		if candidate == text {
			return &text, nil
		}
	}
	return nil, []InvalidParam{keysInvalid(field, "invalid_enum_value",
		fmt.Sprintf("Invalid enum value. Expected %s, received %q", keysEnumLabel(allowed), text))}
}

// keysEnumLabel 把枚举取值渲染成 zod 报错里的 |，连接形式。
func keysEnumLabel(allowed []string) string {
	quoted := make([]string, 0, len(allowed))
	for _, item := range allowed {
		quoted = append(quoted, "'"+item+"'")
	}
	return strings.Join(quoted, " | ")
}

// keysIsJSONNull 判断原始 JSON 是否为 null。
func keysIsJSONNull(value json.RawMessage) bool {
	return strings.TrimSpace(string(value)) == "null"
}

// keysDailyResetTimePattern 复刻 zod 的 /^([01]?\d|2[0-3]):[0-5]\d$/。
func keysValidateDailyResetTime(text string) bool {
	parts := strings.Split(text, ":")
	if len(parts) != 2 {
		return false
	}
	hour, err := strconv.Atoi(parts[0])
	if err != nil || hour < 0 || hour > 23 {
		return false
	}
	if len(parts[0]) > 2 || (len(parts[0]) == 2 && parts[0][0] == '0' && !isDigit(parts[0][1])) {
		return false
	}
	if len(parts[1]) != 2 {
		return false
	}
	minute, err := strconv.Atoi(parts[1])
	if err != nil || minute < 0 || minute > 59 {
		return false
	}
	return true
}

func isDigit(value byte) bool { return value >= '0' && value <= '9' }

// keysListQuery 是 GET /users/{userId}/keys 的查询参数解析结果。
type keysListQuery struct {
	// Statistics 为 true 表示 include=statistics。
	Statistics bool
}

// keysParseListQuery 复刻 KeyListQuerySchema（只有 include 一个可选枚举字段）。
func keysParseListQuery(request *http.Request) (keysListQuery, []InvalidParam) {
	raw := strings.TrimSpace(request.URL.Query().Get("include"))
	if raw == "" {
		return keysListQuery{}, nil
	}
	for _, candidate := range keysListIncludeValues {
		if candidate == raw {
			return keysListQuery{Statistics: true}, nil
		}
	}
	return keysListQuery{}, []InvalidParam{keysInvalid("include", "invalid_enum_value",
		fmt.Sprintf("Invalid enum value. Expected %s, received %q", keysEnumLabel(keysListIncludeValues), raw))}
}

// keysMutation 是一次密钥写入的解析结果（KeyCreateSchema / KeyUpdateSchema 的并集）。
//
// 每个字段都保留「是否出现」，因为 KeyUpdateSchema 的部分更新语义依赖它。
type keysMutation struct {
	Name                    string
	HasName                 bool
	ExpiresAt               store.Nullable[string]
	IsEnabled               *bool
	CanLoginWebUI           *bool
	Limit5hUSD              store.Nullable[string]
	Limit5hResetMode        *string
	LimitDailyUSD           store.Nullable[string]
	DailyResetMode          *string
	DailyResetTime          *string
	LimitWeeklyUSD          store.Nullable[string]
	LimitMonthlyUSD         store.Nullable[string]
	LimitTotalUSD           store.Nullable[string]
	LimitConcurrentSessions store.Nullable[int32]
	ProviderGroup           store.Nullable[string]
	CacheTTLPreference      *string
}

// keysMutationFields 是 KeyMutationFields 的字段名单（.strict() 的白名单）。
var keysMutationFields = []string{
	"name", "expiresAt", "isEnabled", "canLoginWebUi",
	"limit5hUsd", "limit5hResetMode", "limitDailyUsd", "dailyResetMode", "dailyResetTime",
	"limitWeeklyUsd", "limitMonthlyUsd", "limitTotalUsd", "limitConcurrentSessions",
	"providerGroup", "cacheTtlPreference",
}

// keysParseMutation 解析一次写入体；nameRequired 对应 KeyUpdateSchema 里 name 仍是必填。
func keysParseMutation(request *http.Request, nameRequired bool) (keysMutation, []InvalidParam) {
	raw, err := keysReadBody(request)
	if err != nil {
		return keysMutation{}, []InvalidParam{keysInvalidType("body")}
	}
	problems := keysRejectUnknown(raw, keysMutationFields...)

	mutation := keysMutation{}
	add := func(items []InvalidParam) { problems = append(problems, items...) }

	name, nameProblems := keysString(raw, "name", nameRequired)
	add(nameProblems)
	if name != nil {
		trimmed := strings.TrimSpace(*name)
		switch {
		case len(trimmed) < 1:
			add([]InvalidParam{keysInvalid("name", "too_small", "String must contain at least 1 character(s)")})
		case len([]rune(trimmed)) > keysNameMaxLength:
			add([]InvalidParam{keysInvalid("name", "too_big", "String must contain at most 64 character(s)")})
		default:
			mutation.Name = trimmed
			mutation.HasName = true
		}
	}

	var fieldProblems []InvalidParam
	if mutation.ExpiresAt, fieldProblems = keysNullableString(raw, "expiresAt"); fieldProblems != nil {
		add(fieldProblems)
	}
	if mutation.IsEnabled, fieldProblems = keysBool(raw, "isEnabled", false); fieldProblems != nil {
		add(fieldProblems)
	}
	if mutation.CanLoginWebUI, fieldProblems = keysBool(raw, "canLoginWebUi", false); fieldProblems != nil {
		add(fieldProblems)
	}
	if mutation.Limit5hUSD, fieldProblems = keysMoney(raw, "limit5hUsd", keysLimit5hMax); fieldProblems != nil {
		add(fieldProblems)
	}
	if mutation.Limit5hResetMode, fieldProblems = keysEnum(raw, "limit5hResetMode", keysResetModes, false); fieldProblems != nil {
		add(fieldProblems)
	}
	if mutation.LimitDailyUSD, fieldProblems = keysMoney(raw, "limitDailyUsd", keysLimitDailyMax); fieldProblems != nil {
		add(fieldProblems)
	}
	if mutation.DailyResetMode, fieldProblems = keysEnum(raw, "dailyResetMode", keysResetModes, false); fieldProblems != nil {
		add(fieldProblems)
	}
	if mutation.DailyResetTime, fieldProblems = keysDailyResetTime(raw); fieldProblems != nil {
		add(fieldProblems)
	}
	if mutation.LimitWeeklyUSD, fieldProblems = keysMoney(raw, "limitWeeklyUsd", keysLimitWeeklyMax); fieldProblems != nil {
		add(fieldProblems)
	}
	if mutation.LimitMonthlyUSD, fieldProblems = keysMoney(raw, "limitMonthlyUsd", keysLimitMonthlyMax); fieldProblems != nil {
		add(fieldProblems)
	}
	if mutation.LimitTotalUSD, fieldProblems = keysMoney(raw, "limitTotalUsd", keysLimitTotalMax); fieldProblems != nil {
		add(fieldProblems)
	}
	if mutation.LimitConcurrentSessions, fieldProblems = keysInt(raw, "limitConcurrentSessions", false); fieldProblems != nil {
		add(fieldProblems)
	}
	if mutation.ProviderGroup, fieldProblems = keysProviderGroup(raw); fieldProblems != nil {
		add(fieldProblems)
	}
	if mutation.CacheTTLPreference, fieldProblems = keysEnum(raw, "cacheTtlPreference", keysCacheTTLPreferences, false); fieldProblems != nil {
		add(fieldProblems)
	}

	if len(problems) > 0 {
		keysSortInvalid(problems)
		return keysMutation{}, problems
	}
	return mutation, nil
}

// keysDailyResetTime 解析 dailyResetTime（HH:mm）。
func keysDailyResetTime(raw map[string]json.RawMessage) (*string, []InvalidParam) {
	value, ok := raw["dailyResetTime"]
	if !ok {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		return nil, []InvalidParam{keysInvalidType("dailyResetTime")}
	}
	if !keysValidateDailyResetTime(text) {
		return nil, []InvalidParam{keysInvalid("dailyResetTime", "invalid_string",
			"Invalid string: must match the pattern HH:mm")}
	}
	return &text, nil
}

// keysProviderGroup 解析 providerGroup（字符串 | null，长度上限 200）。
func keysProviderGroup(raw map[string]json.RawMessage) (store.Nullable[string], []InvalidParam) {
	value, ok := raw["providerGroup"]
	if !ok {
		return store.Nullable[string]{}, nil
	}
	if keysIsJSONNull(value) {
		return store.ExplicitNull[string](), nil
	}
	var text string
	if err := json.Unmarshal(value, &text); err != nil {
		return store.Nullable[string]{}, []InvalidParam{keysInvalidType("providerGroup")}
	}
	if len([]rune(text)) > keysProviderGroupMax {
		return store.Nullable[string]{}, []InvalidParam{
			keysInvalid("providerGroup", "too_big", "String must contain at most 200 character(s)"),
		}
	}
	return store.SomeValue(text), nil
}

// keysEnableRequest 是 KeyEnableSchema 的解析结果。
type keysEnableRequest struct {
	Enabled bool
}

// keysParseEnable 解析 {"enabled": bool}。
func keysParseEnable(request *http.Request) (keysEnableRequest, []InvalidParam) {
	raw, err := keysReadBody(request)
	if err != nil {
		return keysEnableRequest{}, []InvalidParam{keysInvalidType("body")}
	}
	problems := keysRejectUnknown(raw, "enabled")
	enabled, fieldProblems := keysBool(raw, "enabled", true)
	if fieldProblems != nil {
		problems = append(problems, fieldProblems...)
	}
	if len(problems) > 0 {
		keysSortInvalid(problems)
		return keysEnableRequest{}, problems
	}
	return keysEnableRequest{Enabled: *enabled}, nil
}

// keysRenewRequest 是 KeyRenewSchema 的解析结果。
type keysRenewRequest struct {
	ExpiresAt string
	EnableKey bool
}

// keysParseRenew 解析 {"expiresAt": string(>=1), "enableKey"?: bool}。
func keysParseRenew(request *http.Request) (keysRenewRequest, []InvalidParam) {
	raw, err := keysReadBody(request)
	if err != nil {
		return keysRenewRequest{}, []InvalidParam{keysInvalidType("body")}
	}
	problems := keysRejectUnknown(raw, "expiresAt", "enableKey")
	expiresAt, fieldProblems := keysString(raw, "expiresAt", true)
	if fieldProblems != nil {
		problems = append(problems, fieldProblems...)
	} else if expiresAt != nil && len(*expiresAt) < 1 {
		problems = append(problems, keysInvalid("expiresAt", "too_small",
			"String must contain at least 1 character(s)"))
	}
	enableKey, fieldProblems := keysBool(raw, "enableKey", false)
	if fieldProblems != nil {
		problems = append(problems, fieldProblems...)
	}
	if len(problems) > 0 {
		keysSortInvalid(problems)
		return keysRenewRequest{}, problems
	}
	request_value := keysRenewRequest{}
	if expiresAt != nil {
		request_value.ExpiresAt = *expiresAt
	}
	if enableKey != nil {
		request_value.EnableKey = *enableKey
	}
	return request_value, nil
}

// keysBatchRequest 是 KeysBatchUpdateSchema 的解析结果。
type keysBatchRequest struct {
	KeyIDs  []int64
	Updates store.AdminKeyBatchPatch
}

// keysBatchUpdateFields 是 updates 对象允许的字段（.strict()）。
var keysBatchUpdateFields = []string{
	"providerGroup", "limit5hUsd", "limit5hResetMode", "limitDailyUsd",
	"limitWeeklyUsd", "limitMonthlyUsd", "canLoginWebUi", "isEnabled",
}

// keysParseBatchUpdate 解析 {"keyIds": [..], "updates": {..}}。
func keysParseBatchUpdate(request *http.Request) (keysBatchRequest, []InvalidParam) {
	raw, err := keysReadBody(request)
	if err != nil {
		return keysBatchRequest{}, []InvalidParam{keysInvalidType("body")}
	}
	problems := keysRejectUnknown(raw, "keyIds", "updates")

	keyIDs, idProblems := keysParseIDArray(raw)
	problems = append(problems, idProblems...)

	updates := store.AdminKeyBatchPatch{}
	if value, ok := raw["updates"]; ok && !keysIsJSONNull(value) {
		nested := map[string]json.RawMessage{}
		if err := json.Unmarshal(value, &nested); err != nil {
			problems = append(problems, keysInvalid("updates", "invalid_type", "Expected object"))
		} else {
			problems = append(problems, keysPrefixInvalid(keysRejectUnknown(nested, keysBatchUpdateFields...), "updates")...)
			var fieldProblems []InvalidParam
			if group, groupProblems := keysProviderGroup(nested); groupProblems != nil {
				problems = append(problems, keysPrefixInvalid(groupProblems, "updates")...)
			} else if group.Provided && !group.Null {
				normalized := usersNormalizeProviderGroup(group.Value)
				updates.ProviderGroup = &normalized
			}
			if updates.IsEnabled, fieldProblems = keysBool(nested, "isEnabled", false); fieldProblems != nil {
				problems = append(problems, keysPrefixInvalid(fieldProblems, "updates")...)
			}
			if updates.CanLoginWebUI, fieldProblems = keysBool(nested, "canLoginWebUi", false); fieldProblems != nil {
				problems = append(problems, keysPrefixInvalid(fieldProblems, "updates")...)
			}
			money := []struct {
				field string
				max   float64
				slot  *store.Nullable[string]
			}{
				{"limit5hUsd", keysLimit5hMax, &updates.Limit5hUSD},
				{"limitDailyUsd", keysLimitDailyMax, &updates.LimitDailyUSD},
				{"limitWeeklyUsd", keysLimitWeeklyMax, &updates.LimitWeeklyUSD},
				{"limitMonthlyUsd", keysLimitMonthlyMax, &updates.LimitMonthlyUSD},
			}
			for _, item := range money {
				if parsed, itemProblems := keysMoney(nested, item.field, item.max); itemProblems != nil {
					problems = append(problems, keysPrefixInvalid(itemProblems, "updates")...)
				} else {
					*item.slot = parsed
				}
			}
			if mode, modeProblems := keysEnum(nested, "limit5hResetMode", keysResetModes, false); modeProblems != nil {
				problems = append(problems, keysPrefixInvalid(modeProblems, "updates")...)
			} else {
				updates.Limit5hResetMode = mode
			}
		}
	} else if !ok {
		problems = append(problems, keysInvalidType("updates"))
	}

	if len(problems) > 0 {
		keysSortInvalid(problems)
		return keysBatchRequest{}, problems
	}
	return keysBatchRequest{KeyIDs: keyIDs, Updates: updates}, nil
}

// keysParseIDArray 解析 keyIds（正整数数组，去重后上限 500）。
func keysParseIDArray(raw map[string]json.RawMessage) ([]int64, []InvalidParam) {
	value, ok := raw["keyIds"]
	if !ok {
		return nil, []InvalidParam{keysInvalidType("keyIds")}
	}
	var items []json.RawMessage
	if err := json.Unmarshal(value, &items); err != nil {
		return nil, []InvalidParam{keysInvalid("keyIds", "invalid_type", "Expected array")}
	}
	if len(items) > keysBatchMaxSize {
		return nil, []InvalidParam{keysInvalid("keyIds", "too_big",
			fmt.Sprintf("Array must contain at most %d element(s)", keysBatchMaxSize))}
	}
	ids := make([]int64, 0, len(items))
	seen := make(map[int64]struct{}, len(items))
	for index, item := range items {
		text := strings.TrimSpace(string(item))
		parsed, err := strconv.ParseInt(text, 10, 64)
		if err != nil || parsed <= 0 {
			return nil, []InvalidParam{{
				Path:    []any{"keyIds", index},
				Code:    "invalid_type",
				Message: "Expected positive integer",
			}}
		}
		if _, ok := seen[parsed]; ok {
			continue
		}
		seen[parsed] = struct{}{}
		ids = append(ids, parsed)
	}
	return ids, nil
}

// keysPrefixInvalid 给嵌套字段的 invalidParams 加一层 path 前缀（zod 的 path 形如
// ["updates","limit5hUsd"]）。
func keysPrefixInvalid(problems []InvalidParam, prefix string) []InvalidParam {
	prefixed := make([]InvalidParam, 0, len(problems))
	for _, problem := range problems {
		path := append([]any{prefix}, problem.Path...)
		problem.Path = path
		prefixed = append(prefixed, problem)
	}
	return prefixed
}
