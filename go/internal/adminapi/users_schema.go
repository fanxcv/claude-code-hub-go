package adminapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 本文件是 users 资源的入参校验器：把 Node 的 zod schema（src/lib/api/v1/schemas/users.ts）
// 的约束逐条搬到 Go，并产出同一形状的 invalidParams。
//
// **只对齐约束与错误形状，不对齐逐字文案**（zod 的默认英文消息随版本变化，逐字复制只会得到
// 一份会腐烂的文案表）；message 用与 zod 同义的简述，A2 对拍把文案登记为允许差异。

// usersMutationField 描述一个用户可变更字段的约束。
type usersMutationField struct {
	name     string
	kind     string // string | number | integer | boolean | stringArray | datetime
	optional bool
	nullable bool
	min      *float64
	max      *float64
	maxLen   *int
	pattern  *regexp.Regexp
	enum     []string
	arrayMax int
	itemMax  int
}

// usersResetTimePattern 是 dailyResetTime 的 HH:mm 约束（schemas/users.ts:52）。
var usersResetTimePattern = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

func floatPtr(value float64) *float64 { return &value }
func intPtr(value int) *int           { return &value }

// usersMutationFields 逐条对应 UserMutationFieldsSchema。
var usersMutationFields = []usersMutationField{
	{name: "name", kind: "string", min: floatPtr(1), max: floatPtr(1), maxLen: intPtr(64)},
	{name: "note", kind: "string", optional: true, maxLen: intPtr(200)},
	{name: "providerGroup", kind: "string", optional: true, nullable: true, maxLen: intPtr(200)},
	{name: "tags", kind: "stringArray", optional: true, arrayMax: 20, itemMax: 32},
	{name: "rpm", kind: "integer", optional: true, nullable: true, min: floatPtr(0), max: floatPtr(1_000_000)},
	{name: "dailyQuota", kind: "number", optional: true, nullable: true, min: floatPtr(0), max: floatPtr(10_000)},
	{name: "limit5hUsd", kind: "number", optional: true, nullable: true, min: floatPtr(0), max: floatPtr(10_000)},
	{name: "limit5hResetMode", kind: "string", optional: true, enum: []string{"fixed", "rolling"}},
	{name: "limitWeeklyUsd", kind: "number", optional: true, nullable: true, min: floatPtr(0), max: floatPtr(50_000)},
	{name: "limitMonthlyUsd", kind: "number", optional: true, nullable: true, min: floatPtr(0), max: floatPtr(200_000)},
	{name: "limitTotalUsd", kind: "number", optional: true, nullable: true, min: floatPtr(0), max: floatPtr(10_000_000)},
	{name: "limitConcurrentSessions", kind: "integer", optional: true, nullable: true, min: floatPtr(0), max: floatPtr(1000)},
	{name: "dailyResetMode", kind: "string", optional: true, enum: []string{"fixed", "rolling"}},
	{name: "dailyResetTime", kind: "string", optional: true, pattern: usersResetTimePattern},
	{name: "isEnabled", kind: "boolean", optional: true},
	{name: "expiresAt", kind: "datetime", optional: true, nullable: true},
	{name: "allowedClients", kind: "stringArray", optional: true, arrayMax: 50, itemMax: 64},
	{name: "blockedClients", kind: "stringArray", optional: true, arrayMax: 50, itemMax: 64},
	{name: "allowedModels", kind: "stringArray", optional: true, arrayMax: 50, itemMax: 64},
}

// usersBatchUpdateFieldNames 是 batchUpdateUsers 允许的 8 个字段（UsersBatchUpdateSchema 的 pick）。
var usersBatchUpdateFieldNames = map[string]struct{}{
	"note": {}, "tags": {}, "rpm": {}, "dailyQuota": {}, "limit5hUsd": {},
	"limit5hResetMode": {}, "limitWeeklyUsd": {}, "limitMonthlyUsd": {},
}

// validateUsersPayload 按 schema 校验请求体并回填解析结果。
func validateUsersPayload(
	raw map[string]json.RawMessage,
	kind usersSchemaKind,
	payload *usersPayload,
) []usersInvalidParam {
	problems := []usersInvalidParam{}
	switch kind {
	case usersCreateSchema, usersUpdateSchema:
		allowed := make(map[string]usersMutationField, len(usersMutationFields))
		for _, field := range usersMutationFields {
			allowed[field.name] = field
		}
		problems = append(problems, usersRejectUnknown(raw, allowed)...)
		for _, field := range usersMutationFields {
			item, present := raw[field.name]
			if !present {
				continue
			}
			required := kind == usersCreateSchema && !field.optional
			value, fieldProblems := usersValidateField(field, item, required)
			problems = append(problems, fieldProblems...)
			if len(fieldProblems) > 0 {
				continue
			}
			usersAssignField(payload, field.name, value)
		}
		if kind == usersCreateSchema {
			if _, present := raw["name"]; !present {
				problems = append(problems, usersInvalidParam{
					Path: []any{"name"}, Code: "invalid_type", Message: "Required",
				})
			}
		}
		return problems

	case usersEnableSchema:
		problems = append(problems, usersRejectUnknown(raw, map[string]usersMutationField{
			"enabled": {name: "enabled", kind: "boolean"},
		})...)
		if item, present := raw["enabled"]; present {
			value, fieldProblems := usersValidateField(usersMutationField{name: "enabled", kind: "boolean"}, item, true)
			problems = append(problems, fieldProblems...)
			if len(fieldProblems) == 0 {
				if boolean, ok := value.(bool); ok {
					payload.enabled = &boolean
				}
			}
		}
		return problems

	case usersRenewSchema:
		problems = append(problems, usersRejectUnknown(raw, map[string]usersMutationField{
			"expiresAt":  {name: "expiresAt", kind: "string"},
			"enableUser": {name: "enableUser", kind: "boolean", optional: true},
		})...)
		if item, present := raw["expiresAt"]; present {
			value, fieldProblems := usersValidateField(
				usersMutationField{name: "expiresAt", kind: "string", min: floatPtr(1)}, item, true)
			problems = append(problems, fieldProblems...)
			if len(fieldProblems) == 0 {
				if text, ok := value.(string); ok {
					payload.expiresAt = &text
					payload.expiresAtPresent = true
				}
			}
		} else {
			problems = append(problems, usersInvalidParam{
				Path: []any{"expiresAt"}, Code: "invalid_type", Message: "Required",
			})
		}
		if item, present := raw["enableUser"]; present {
			value, fieldProblems := usersValidateField(
				usersMutationField{name: "enableUser", kind: "boolean", optional: true}, item, false)
			problems = append(problems, fieldProblems...)
			if len(fieldProblems) == 0 {
				if boolean, ok := value.(bool); ok {
					payload.enableUser = &boolean
				}
			}
		}
		return problems

	case usersUsageBatchSchema, usersBatchUpdateSchema:
		unknown := map[string]usersMutationField{"userIds": {name: "userIds", kind: "string"}}
		if kind == usersBatchUpdateSchema {
			unknown["updates"] = usersMutationField{name: "updates", kind: "string"}
		}
		problems = append(problems, usersRejectUnknown(raw, unknown)...)
		if item, present := raw["userIds"]; present {
			ids, idProblems := usersValidateIDArray(item)
			problems = append(problems, idProblems...)
			if len(idProblems) == 0 {
				payload.userIDs = ids
			}
		} else {
			problems = append(problems, usersInvalidParam{
				Path: []any{"userIds"}, Code: "invalid_type", Message: "Required",
			})
		}
		if kind == usersBatchUpdateSchema {
			updates := &usersPayload{}
			problems = append(problems, usersValidateBatchUpdates(raw["updates"], updates)...)
			if len(problems) == 0 {
				payload.updates = updates
			}
		}
		return problems
	}
	return problems
}

// usersValidateBatchUpdates 校验 updates 子对象（pick 出的 8 个字段，且至少给一个）。
func usersValidateBatchUpdates(raw json.RawMessage, payload *usersPayload) []usersInvalidParam {
	if len(raw) == 0 {
		return []usersInvalidParam{{Path: []any{"updates"}, Code: "invalid_type", Message: "Required"}}
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return []usersInvalidParam{{Path: []any{"updates"}, Code: "invalid_type", Message: "Expected object"}}
	}
	allowed := make(map[string]usersMutationField, len(usersBatchUpdateFieldNames))
	for _, field := range usersMutationFields {
		if _, ok := usersBatchUpdateFieldNames[field.name]; ok {
			candidate := field
			candidate.optional = true
			allowed[field.name] = candidate
		}
	}
	problems := usersRejectUnknownPrefix(object, "updates", allowed)
	for name, item := range object {
		field, ok := allowed[name]
		if !ok {
			continue
		}
		value, fieldProblems := usersValidateField(field, item, false)
		problems = append(problems, fieldProblems...)
		if len(fieldProblems) > 0 {
			continue
		}
		usersAssignField(payload, name, value)
	}
	return problems
}

// usersRejectUnknown 复刻 zod strict 模式的 unrecognized_keys 报错（顶层字段）。
func usersRejectUnknown(raw map[string]json.RawMessage, allowed map[string]usersMutationField) []usersInvalidParam {
	problems := []usersInvalidParam{}
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sortStrings(names)
	for _, name := range names {
		if _, ok := allowed[name]; ok {
			continue
		}
		problems = append(problems, usersInvalidParam{
			Path: []any{}, Code: "unrecognized_keys", Message: "Unrecognized key: " + name,
		})
	}
	return problems
}

// usersRejectUnknownPrefix 同上，但 path 带前缀（updates 子对象）。
func usersRejectUnknownPrefix(
	raw map[string]json.RawMessage,
	prefix string,
	allowed map[string]usersMutationField,
) []usersInvalidParam {
	problems := []usersInvalidParam{}
	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sortStrings(names)
	for _, name := range names {
		if _, ok := allowed[name]; ok {
			continue
		}
		problems = append(problems, usersInvalidParam{
			Path: []any{prefix}, Code: "unrecognized_keys", Message: "Unrecognized key: " + name,
		})
	}
	return problems
}

// usersValidateField 校验单个字段，返回 Go 值或问题列表。
func usersValidateField(
	field usersMutationField,
	raw json.RawMessage,
	required bool,
) (any, []usersInvalidParam) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "null" {
		if field.nullable {
			return nil, nil
		}
		return nil, []usersInvalidParam{{
			Path: []any{field.name}, Code: "invalid_type",
			Message: fmt.Sprintf("Expected %s, received null", usersKindLabel(field.kind)),
		}}
	}
	if trimmed == "" && required {
		return nil, []usersInvalidParam{{
			Path: []any{field.name}, Code: "invalid_type", Message: "Required",
		}}
	}

	switch field.kind {
	case "string", "datetime":
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, []usersInvalidParam{{
				Path: []any{field.name}, Code: "invalid_type",
				Message: "Expected string, received " + usersJSONLabel(trimmed),
			}}
		}
		if field.kind == "string" {
			text = strings.TrimSpace(text)
		}
		if field.min != nil && float64(len(text)) < *field.min {
			return nil, []usersInvalidParam{{
				Path: []any{field.name}, Code: "too_small",
				Message: "String must contain at least 1 character(s)",
			}}
		}
		if field.maxLen != nil && len(text) > *field.maxLen {
			return nil, []usersInvalidParam{{
				Path: []any{field.name}, Code: "too_big",
				Message: fmt.Sprintf("String must contain at most %d character(s)", *field.maxLen),
			}}
		}
		if field.pattern != nil && !field.pattern.MatchString(text) {
			return nil, []usersInvalidParam{{
				Path: []any{field.name}, Code: "invalid_string", Message: "Invalid string",
			}}
		}
		if len(field.enum) > 0 && !usersEnumContains(field.enum, text) {
			return nil, []usersInvalidParam{{
				Path: []any{field.name}, Code: "invalid_enum_value",
				Message: fmt.Sprintf("Invalid enum value. Expected %s, received '%s'",
					usersEnumLabel(field.enum), text),
			}}
		}
		if field.kind == "datetime" {
			if _, err := time.Parse(time.RFC3339, text); err != nil {
				return nil, []usersInvalidParam{{
					Path: []any{field.name}, Code: "invalid_string", Message: "Invalid datetime",
				}}
			}
		}
		return text, nil

	case "number", "integer":
		var number float64
		if err := json.Unmarshal(raw, &number); err != nil {
			return nil, []usersInvalidParam{{
				Path: []any{field.name}, Code: "invalid_type",
				Message: "Expected number, received " + usersJSONLabel(trimmed),
			}}
		}
		if field.kind == "integer" && number != float64(int64(number)) {
			return nil, []usersInvalidParam{{
				Path: []any{field.name}, Code: "invalid_type", Message: "Expected integer, received float",
			}}
		}
		if field.min != nil && number < *field.min {
			return nil, []usersInvalidParam{{
				Path: []any{field.name}, Code: "too_small",
				Message: fmt.Sprintf("Number must be greater than or equal to %v", *field.min),
			}}
		}
		if field.max != nil && number > *field.max {
			return nil, []usersInvalidParam{{
				Path: []any{field.name}, Code: "too_big",
				Message: fmt.Sprintf("Number must be less than or equal to %v", *field.max),
			}}
		}
		return number, nil

	case "boolean":
		var boolean bool
		if err := json.Unmarshal(raw, &boolean); err != nil {
			return nil, []usersInvalidParam{{
				Path: []any{field.name}, Code: "invalid_type",
				Message: "Expected boolean, received " + usersJSONLabel(trimmed),
			}}
		}
		return boolean, nil

	case "stringArray":
		var items []string
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, []usersInvalidParam{{
				Path: []any{field.name}, Code: "invalid_type",
				Message: "Expected array, received " + usersJSONLabel(trimmed),
			}}
		}
		if field.arrayMax > 0 && len(items) > field.arrayMax {
			return nil, []usersInvalidParam{{
				Path: []any{field.name}, Code: "too_big",
				Message: fmt.Sprintf("Array must contain at most %d element(s)", field.arrayMax),
			}}
		}
		for index, item := range items {
			if field.itemMax > 0 && len(item) > field.itemMax {
				return nil, []usersInvalidParam{{
					Path: []any{field.name, index}, Code: "too_big",
					Message: fmt.Sprintf("String must contain at most %d character(s)", field.itemMax),
				}}
			}
		}
		return items, nil
	}
	return nil, []usersInvalidParam{{
		Path: []any{field.name}, Code: "invalid_type", Message: "Unsupported field",
	}}
}

// usersValidateIDArray 校验 userIds：正整数数组，最多 500 项。
func usersValidateIDArray(raw json.RawMessage) ([]int64, []usersInvalidParam) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, []usersInvalidParam{{
			Path: []any{"userIds"}, Code: "invalid_type", Message: "Expected array",
		}}
	}
	if len(items) > 500 {
		return nil, []usersInvalidParam{{
			Path: []any{"userIds"}, Code: "too_big",
			Message: "Array must contain at most 500 element(s)",
		}}
	}
	ids := make([]int64, 0, len(items))
	for index, item := range items {
		var number float64
		if err := json.Unmarshal(item, &number); err != nil || number != float64(int64(number)) {
			return nil, []usersInvalidParam{{
				Path: []any{"userIds", index}, Code: "invalid_type", Message: "Expected number",
			}}
		}
		if number <= 0 {
			return nil, []usersInvalidParam{{
				Path: []any{"userIds", index}, Code: "too_small",
				Message: "Number must be greater than 0",
			}}
		}
		ids = append(ids, int64(number))
	}
	return ids, nil
}

// usersAssignField 把校验后的值回填到解析结果。
func usersAssignField(payload *usersPayload, name string, value any) {
	switch name {
	case "name":
		if text, ok := value.(string); ok {
			payload.name = &text
		}
	case "note":
		if text, ok := value.(string); ok {
			payload.note = &text
		}
	case "providerGroup":
		payload.providerGroupPresent = true
		if value != nil {
			if text, ok := value.(string); ok {
				payload.providerGroup = &text
			}
		}
	case "tags":
		if items, ok := value.([]string); ok {
			payload.tags = &items
		}
	case "rpm":
		payload.rpmPresent = true
		if value != nil {
			if number, ok := value.(float64); ok {
				converted := int64(number)
				payload.rpm = &converted
			}
		}
	case "dailyQuota":
		payload.dailyQuotaPresent = true
		payload.dailyQuota = usersFloat(value)
	case "limit5hUsd":
		payload.limit5hUSDPresent = true
		payload.limit5hUSD = usersFloat(value)
	case "limit5hResetMode":
		if text, ok := value.(string); ok {
			payload.limit5hResetMode = &text
		}
	case "limitWeeklyUsd":
		payload.limitWeeklyUSDPresent = true
		payload.limitWeeklyUSD = usersFloat(value)
	case "limitMonthlyUsd":
		payload.limitMonthlyUSDPresent = true
		payload.limitMonthlyUSD = usersFloat(value)
	case "limitTotalUsd":
		payload.limitTotalUSDPresent = true
		payload.limitTotalUSD = usersFloat(value)
	case "limitConcurrentSessions":
		payload.limitConcurrentSessionsPresent = true
		if value != nil {
			if number, ok := value.(float64); ok {
				converted := int64(number)
				payload.limitConcurrentSessions = &converted
			}
		}
	case "dailyResetMode":
		if text, ok := value.(string); ok {
			payload.dailyResetMode = &text
		}
	case "dailyResetTime":
		if text, ok := value.(string); ok {
			payload.dailyResetTime = &text
		}
	case "isEnabled":
		if boolean, ok := value.(bool); ok {
			payload.isEnabled = &boolean
		}
	case "expiresAt":
		payload.expiresAtPresent = true
		if value == nil {
			payload.expiresAtNull = true
		} else if text, ok := value.(string); ok {
			payload.expiresAt = &text
		}
	case "allowedClients":
		if items, ok := value.([]string); ok {
			payload.allowedClients = &items
		}
	case "blockedClients":
		if items, ok := value.([]string); ok {
			payload.blockedClients = &items
		}
	case "allowedModels":
		if items, ok := value.([]string); ok {
			payload.allowedModels = &items
		}
	}
}

func usersFloat(value any) *float64 {
	if value == nil {
		return nil
	}
	if number, ok := value.(float64); ok {
		return &number
	}
	return nil
}

// usersListQuery 是 GET /users 的查询参数（UserListQuerySchema）。
type usersListQuery struct {
	cursor          string
	limit           int
	searchTerm      string
	tagFilters      []string
	keyGroupFilters []string
	statusFilter    string
	sortBy          string
	sortOrder       string
}

// usersListStatusValues 等是各枚举的取值集合。
var (
	usersListStatusValues = []string{"active", "expired", "expiringSoon", "enabled", "disabled"}
	usersListSortByValues = []string{"name", "tags", "expiresAt", "rpm", "limit5hUsd",
		"limitDailyUsd", "limitWeeklyUsd", "limitMonthlyUsd", "createdAt"}
	usersListSortOrderValues = []string{"asc", "desc"}
)

// parseUsersListQuery 解析并校验 GET /users 的查询串。
func parseUsersListQuery(request *http.Request) (usersListQuery, []usersInvalidParam) {
	query := usersListQuery{limit: 50}
	values := request.URL.Query()
	problems := []usersInvalidParam{}

	if raw := strings.TrimSpace(values.Get("cursor")); raw != "" {
		query.cursor = raw
	}
	if raw := strings.TrimSpace(values.Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			problems = append(problems, usersInvalidParam{
				Path: []any{"limit"}, Code: "invalid_type", Message: "Expected number, received string",
			})
		} else if parsed < 1 {
			problems = append(problems, usersInvalidParam{
				Path: []any{"limit"}, Code: "too_small",
				Message: "Number must be greater than or equal to 1",
			})
		} else if parsed > 100 {
			problems = append(problems, usersInvalidParam{
				Path: []any{"limit"}, Code: "too_big",
				Message: "Number must be less than or equal to 100",
			})
		} else {
			query.limit = parsed
		}
	}
	if raw := values.Get("q"); strings.TrimSpace(raw) != "" {
		query.searchTerm = strings.TrimSpace(raw)
	}
	query.tagFilters = usersSplitCSV(values.Get("tags"))
	query.keyGroupFilters = usersSplitCSV(values.Get("keyGroups"))
	if raw := strings.TrimSpace(values.Get("status")); raw != "" {
		if !usersEnumContains(usersListStatusValues, raw) {
			problems = append(problems, usersInvalidParam{
				Path: []any{"status"}, Code: "invalid_enum_value",
				Message: fmt.Sprintf("Invalid enum value. Expected %s, received '%s'",
					usersEnumLabel(usersListStatusValues), raw),
			})
		} else {
			query.statusFilter = raw
		}
	}
	if raw := strings.TrimSpace(values.Get("sortBy")); raw != "" {
		if !usersEnumContains(usersListSortByValues, raw) {
			problems = append(problems, usersInvalidParam{
				Path: []any{"sortBy"}, Code: "invalid_enum_value",
				Message: fmt.Sprintf("Invalid enum value. Expected %s, received '%s'",
					usersEnumLabel(usersListSortByValues), raw),
			})
		} else {
			query.sortBy = raw
		}
	}
	if raw := strings.TrimSpace(values.Get("sortOrder")); raw != "" {
		if !usersEnumContains(usersListSortOrderValues, raw) {
			problems = append(problems, usersInvalidParam{
				Path: []any{"sortOrder"}, Code: "invalid_enum_value",
				Message: fmt.Sprintf("Invalid enum value. Expected %s, received '%s'",
					usersEnumLabel(usersListSortOrderValues), raw),
			})
		} else {
			query.sortOrder = raw
		}
	}
	return query, problems
}

// usersSplitCSV 复刻 splitCsv：逗号分隔、去空白、去空段，全空时返回 nil。
func usersSplitCSV(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	items := make([]string, 0, 4)
	for _, part := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			items = append(items, trimmed)
		}
	}
	if len(items) == 0 {
		return nil
	}
	return items
}

func usersEnumContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func usersEnumLabel(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, "'"+value+"'")
	}
	return strings.Join(quoted, "|")
}

func usersKindLabel(kind string) string {
	switch kind {
	case "number", "integer":
		return "number"
	case "boolean":
		return "boolean"
	case "stringArray":
		return "array"
	case "datetime":
		return "string"
	default:
		return "string"
	}
}

// usersJSONLabel 给出 zod 风格的类型描述（null / array / object / boolean / number / string）。
func usersJSONLabel(raw string) string {
	switch {
	case raw == "null":
		return "null"
	case strings.HasPrefix(raw, "["):
		return "array"
	case strings.HasPrefix(raw, "{"):
		return "object"
	case raw == "true" || raw == "false":
		return "boolean"
	case strings.HasPrefix(raw, "\""):
		return "string"
	default:
		return "number"
	}
}

// timeUTC 是解析日期输入时的默认时区占位（调用方会先解析系统时区并覆盖）。
//
// 保留独立函数是为了让「默认走 UTC」这件事只有一个来源：与 Node 的
// resolveSystemTimezone 第三级兜底一致。
func timeUTC() *time.Location { return time.UTC }
