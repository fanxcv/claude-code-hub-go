package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 users 资源的入参解析、响应写出与展示对象组装。
//
// 校验对齐 Node 的 zod schema（src/lib/api/v1/schemas/users.ts）。**只对齐约束与错误形状，
// 不对齐逐字文案**：zod 的默认英文消息随版本变化，逐字复制只会得到一份会腐烂的文案表。
// invalidParams 的 code 取 zod 的语义码（invalid_type / too_big / invalid_enum_value /
// invalid_string 等），A2 对拍时把「消息文案」登记为允许差异。

// usersInvalidParam 是 Problem 的 invalidParams 一项（error-envelope.ts:24-28）。
type usersInvalidParam struct {
	Path    []any  `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeUsersJSON 作答 JSON 正文。
//
// Content-Type 取 Node jsonResponse 的 "application/json"（不带 charset）；
// 版本头与不缓存由 Router 信封统一补（router.go:applyEnvelopeHeaders）。
func writeUsersJSON(writer http.ResponseWriter, status int, body any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(body); err != nil {
		// 正文都是可序列化结构；失败说明响应已经写出，只能记日志（无 logger 可用时静默）。
		return
	}
}

// writeUsersNoContent 作答 204（Node noContentResponse）。
func writeUsersNoContent(writer http.ResponseWriter) {
	writer.WriteHeader(http.StatusNoContent)
}

// writeValidationProblem 复刻 fromZodError 的 400 信封。
//
// 为什么不走 Deps.Problems：共享的 problemBody **没有 invalidParams 字段**（A0 冻结面的
// 表达力上限），而校验失败必须带 invalidParams，否则前端拿不到哪个字段错了。这里在自己
// 的文件里构造同一形状的正文，字段顺序与 error-envelope.ts:40-62 一致。
//
// type/title/detail/errorCode/instance 与 createProblemJson 逐字相同，A2 对拍可直接比对。
func (api *usersAPI) writeValidationProblem(
	writer http.ResponseWriter,
	request *http.Request,
	params []usersInvalidParam,
) {
	body := map[string]any{
		"type":          "urn:claude-code-hub:problem:" + usersEncodeURIComponent("request.validation_failed"),
		"title":         "Validation failed",
		"status":        http.StatusBadRequest,
		"detail":        "One or more fields are invalid.",
		"instance":      usersInstance(request),
		"errorCode":     "request.validation_failed",
		"invalidParams": params,
	}
	writer.Header().Set("Content-Type", "application/problem+json")
	writer.WriteHeader(http.StatusBadRequest)
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(body)
}

// usersInstance 取 Problem 的 instance（Node 传入 new URL(c.req.url).pathname）。
func usersInstance(request *http.Request) string {
	if request == nil || request.URL == nil || request.URL.Path == "" {
		return MountPrefix
	}
	return request.URL.Path
}

// usersEncodeURIComponent 是 JS encodeURIComponent 的等价实现（空格用 %20，不是 +）。
func usersEncodeURIComponent(value string) string {
	const unreserved = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_.!~*'()"
	var builder strings.Builder
	for _, char := range []byte(value) {
		if strings.IndexByte(unreserved, char) >= 0 {
			builder.WriteByte(char)
			continue
		}
		fmt.Fprintf(&builder, "%%%02X", char)
	}
	return builder.String()
}

// writeUsersActionError 复刻 users/handlers.ts:365-386 的 getActionErrorStatus 与
// actionError：状态码由**该模块自己的表**决定，detail 取公开文案。
func (api *usersAPI) writeUsersActionError(writer http.ResponseWriter, request *http.Request, err error) {
	var action *ActionError
	if !errors.As(err, &action) {
		api.problems.WriteActionError(writer, request, err)
		return
	}
	if action.Status == 0 {
		action.Status = usersActionStatus(action.Code)
	}
	if action.Code == "" {
		if action.Status == http.StatusNotFound {
			action.Code = "user.not_found"
		} else {
			action.Code = "user.action_failed"
		}
	}
	api.problems.WriteActionError(writer, request, action)
}

// usersActionError 建一条带 users 模块状态码的 action 错误。
func usersActionError(code string, err error) error {
	return &ActionError{Resource: "user", Code: code, Status: usersActionStatus(code), Err: err}
}

// usersActionStatus 复刻 users/handlers.ts:365-386 的显式码表。
//
// **不移植** Node 的中文子串分支（detail.includes("不存在") / includes("权限")）：按文案判
// 状态码会在改文案时静默改语义（problem.go 的同一条裁决）。受影响的调用会显式给码。
func usersActionStatus(code string) int {
	switch code {
	case "UNAUTHORIZED":
		return http.StatusUnauthorized
	case "PERMISSION_DENIED":
		return http.StatusForbidden
	case "NOT_FOUND", "USER_NOT_FOUND":
		return http.StatusNotFound
	case "DATABASE_ERROR", "CONNECTION_FAILED", "TIMEOUT", "NETWORK_ERROR":
		return http.StatusServiceUnavailable
	case "INTERNAL_ERROR", "OPERATION_FAILED", "CREATE_FAILED", "UPDATE_FAILED", "DELETE_FAILED":
		return http.StatusInternalServerError
	default:
		return http.StatusBadRequest
	}
}

// usersCreateSchema 等是入参校验器的名字（见 users_schema.go）。
type usersSchemaKind int

const (
	usersCreateSchema usersSchemaKind = iota
	usersUpdateSchema
	usersEnableSchema
	usersRenewSchema
	usersUsageBatchSchema
	usersBatchUpdateSchema
)

// decodeUsersBody 解析并校验请求体。
//
// 与 Node 的两点差异（都登记为白名单）：
//  1. Node 用 zod 的 strict 模式，未声明字段会**报 400**；这里同样拒绝未知字段（见 schema 定义）。
//  2. 类型错误的文案不同（见文件头）。
func decodeUsersBody(request *http.Request, kind usersSchemaKind) (*usersPayload, []usersInvalidParam) {
	payload := &usersPayload{}
	raw, problems := readUsersJSONObject(request)
	if len(problems) > 0 {
		return payload, problems
	}
	problems = validateUsersPayload(raw, kind, payload)
	if len(problems) > 0 {
		return payload, problems
	}
	return payload, nil
}

// readUsersJSONObject 读请求体并解析成「字段 -> 原始 JSON」的映射（保留是否出现过的信息）。
//
// 空请求体按空对象处理（Node 的 parseHonoJsonBody 对空体的行为取决于端点，这里按空对象，
// 由各 schema 的 required 决定是否报错）。
func readUsersJSONObject(request *http.Request) (map[string]json.RawMessage, []usersInvalidParam) {
	data, err := io.ReadAll(io.LimitReader(request.Body, 1<<20))
	if err != nil {
		return nil, []usersInvalidParam{{
			Path: []any{}, Code: "invalid_body", Message: "Request body could not be read.",
		}}
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, []usersInvalidParam{{
			Path: []any{}, Code: "invalid_json", Message: "Invalid JSON body.",
		}}
	}
	return raw, nil
}

// usersPayload 是各端点共用的解析结果。字段用指针区分「未出现」与「出现为 null」。
type usersPayload struct {
	name                           *string
	note                           *string
	providerGroup                  *string
	providerGroupPresent           bool
	tags                           *[]string
	rpm                            *int64
	rpmPresent                     bool
	dailyQuota                     *float64
	dailyQuotaPresent              bool
	limit5hUSD                     *float64
	limit5hUSDPresent              bool
	limit5hResetMode               *string
	limitWeeklyUSD                 *float64
	limitWeeklyUSDPresent          bool
	limitMonthlyUSD                *float64
	limitMonthlyUSDPresent         bool
	limitTotalUSD                  *float64
	limitTotalUSDPresent           bool
	limitConcurrentSessions        *int64
	limitConcurrentSessionsPresent bool
	dailyResetMode                 *string
	dailyResetTime                 *string
	isEnabled                      *bool
	expiresAt                      *string
	expiresAtPresent               bool
	expiresAtNull                  bool
	allowedClients                 *[]string
	blockedClients                 *[]string
	allowedModels                  *[]string

	enabled    *bool
	enableUser *bool
	userIDs    []int64
	updates    *usersPayload
}

// presentFields 返回出现过的字段名（用于字段级权限判定，与 Node 的 Object.keys(validatedData) 同义）。
func (payload *usersPayload) presentFields() []string {
	if payload == nil {
		return nil
	}
	fields := make([]string, 0, 20)
	appendIf := func(present bool, name string) {
		if present {
			fields = append(fields, name)
		}
	}
	appendIf(payload.name != nil, "name")
	appendIf(payload.note != nil, "note")
	appendIf(payload.providerGroupPresent, "providerGroup")
	appendIf(payload.tags != nil, "tags")
	appendIf(payload.rpmPresent, "rpm")
	appendIf(payload.dailyQuotaPresent, "dailyQuota")
	appendIf(payload.limit5hUSDPresent, "limit5hUsd")
	appendIf(payload.limit5hResetMode != nil, "limit5hResetMode")
	appendIf(payload.limitWeeklyUSDPresent, "limitWeeklyUsd")
	appendIf(payload.limitMonthlyUSDPresent, "limitMonthlyUsd")
	appendIf(payload.limitTotalUSDPresent, "limitTotalUsd")
	appendIf(payload.limitConcurrentSessionsPresent, "limitConcurrentSessions")
	appendIf(payload.dailyResetMode != nil, "dailyResetMode")
	appendIf(payload.dailyResetTime != nil, "dailyResetTime")
	appendIf(payload.isEnabled != nil, "isEnabled")
	appendIf(payload.expiresAtPresent, "expiresAt")
	appendIf(payload.allowedClients != nil, "allowedClients")
	appendIf(payload.blockedClients != nil, "blockedClients")
	appendIf(payload.allowedModels != nil, "allowedModels")
	return fields
}

// usersUnauthorizedFields 复刻 getUnauthorizedFields：非管理员改动管理员专属字段即未授权。
func usersUnauthorizedFields(fields []string, isAdmin bool) []string {
	if isAdmin {
		return nil
	}
	adminOnly := map[string]struct{}{
		"rpm": {}, "dailyQuota": {}, "providerGroup": {},
		"limit5hUsd": {}, "limit5hResetMode": {}, "limitWeeklyUsd": {},
		"limitMonthlyUsd": {}, "limitTotalUsd": {}, "limitConcurrentSessions": {},
		"dailyResetMode": {}, "dailyResetTime": {},
		"isEnabled": {}, "expiresAt": {},
		"allowedClients": {}, "blockedClients": {}, "allowedModels": {},
	}
	unauthorized := make([]string, 0, len(fields))
	for _, field := range fields {
		if _, ok := adminOnly[field]; ok {
			unauthorized = append(unauthorized, field)
		}
	}
	return unauthorized
}

// usersCreateInputFrom 把请求体转成落库入参（复刻 addUser 里的归一：providerGroup 归一为
// normalizeProviderGroup，note 缺省空串，tags/allowed* 缺省空数组）。
func usersCreateInputFrom(raw *usersPayload) store.AdminUserCreateInput {
	input := store.AdminUserCreateInput{}
	if raw.name != nil {
		input.Name = *raw.name
	}
	if raw.note != nil {
		input.Description = *raw.note
	}
	group := ""
	if raw.providerGroup != nil {
		group = *raw.providerGroup
	}
	normalized := usersNormalizeProviderGroup(group)
	input.ProviderGroup = &normalized
	if raw.tags != nil {
		input.Tags = *raw.tags
	}
	input.RPM = raw.rpm
	input.DailyQuota = usersFloatPtrToString(raw.dailyQuota)
	input.Limit5hUSD = usersFloatPtrToString(raw.limit5hUSD)
	if raw.limit5hResetMode != nil {
		input.Limit5hResetMode = *raw.limit5hResetMode
	}
	input.LimitWeeklyUSD = usersFloatPtrToString(raw.limitWeeklyUSD)
	input.LimitMonthlyUSD = usersFloatPtrToString(raw.limitMonthlyUSD)
	input.LimitTotalUSD = usersFloatPtrToString(raw.limitTotalUSD)
	input.LimitConcurrentSessions = raw.limitConcurrentSessions
	if raw.dailyResetMode != nil {
		input.DailyResetMode = *raw.dailyResetMode
	}
	if raw.dailyResetTime != nil {
		input.DailyResetTime = *raw.dailyResetTime
	}
	input.IsEnabled = raw.isEnabled
	if raw.expiresAtPresent && !raw.expiresAtNull && raw.expiresAt != nil {
		if parsed, err := parseUsersDateInput(*raw.expiresAt, timeUTC()); err == nil {
			input.ExpiresAt = &parsed
		}
	}
	if raw.allowedClients != nil {
		input.AllowedClients = *raw.allowedClients
	}
	if raw.blockedClients != nil {
		input.BlockedClients = *raw.blockedClients
	}
	if raw.allowedModels != nil {
		input.AllowedModels = *raw.allowedModels
	}
	return input
}

// usersNormalizeProviderGroup 复刻 normalizeProviderGroup：空值取 "default"，去重排序后逗号连接。
func usersNormalizeProviderGroup(value string) string {
	split := store.SplitProviderGroups(&value)
	if len(split) == 0 {
		return "default"
	}
	seen := make(map[string]struct{}, len(split))
	unique := make([]string, 0, len(split))
	for _, group := range split {
		if _, ok := seen[group]; ok {
			continue
		}
		seen[group] = struct{}{}
		unique = append(unique, group)
	}
	sortStrings(unique)
	return strings.Join(unique, ",")
}

// usersFloatPtrToString 把可空数值转成写入 numeric 列的字符串。
func usersFloatPtrToString(value *float64) *string {
	if value == nil {
		return nil
	}
	text := strconv.FormatFloat(*value, 'f', -1, 64)
	return &text
}

// toStorePatch 把 updateUser 的请求体转成列补丁。
func (payload *usersPayload) toStorePatch() (store.AdminUserPatch, error) {
	patch := store.AdminUserPatch{}
	if payload == nil {
		return patch, nil
	}
	patch.Name = payload.name
	patch.Description = payload.note
	if payload.providerGroupPresent {
		normalized := usersNormalizeProviderGroup(usersStringOr(payload.providerGroup, ""))
		patch.ProviderGroup = &normalized
	}
	patch.Tags = payload.tags
	patch.RPM = payload.rpm
	patch.DailyQuota = usersFloatPtrToString(payload.dailyQuota)
	patch.Limit5hUSD = usersFloatPtrToString(payload.limit5hUSD)
	patch.Limit5hResetMode = payload.limit5hResetMode
	patch.LimitWeeklyUSD = usersFloatPtrToString(payload.limitWeeklyUSD)
	patch.LimitMonthlyUSD = usersFloatPtrToString(payload.limitMonthlyUSD)
	patch.LimitTotalUSD = usersFloatPtrToString(payload.limitTotalUSD)
	patch.LimitConcurrentSessions = payload.limitConcurrentSessions
	patch.DailyResetMode = payload.dailyResetMode
	patch.DailyResetTime = payload.dailyResetTime
	patch.IsEnabled = payload.isEnabled
	if payload.expiresAtPresent {
		if payload.expiresAtNull {
			patch.ClearExpiresAt = true
		} else if payload.expiresAt != nil {
			parsed, err := parseUsersDateInput(*payload.expiresAt, timeUTC())
			if err != nil {
				return patch, err
			}
			patch.ExpiresAt = &parsed
		}
	}
	patch.AllowedClients = payload.allowedClients
	patch.BlockedClients = payload.blockedClients
	patch.AllowedModels = payload.allowedModels
	return patch, nil
}

// toBatchPatch 把 batchUpdateUsers 的 updates 转成批量补丁（只含该 action 允许的 8 个字段）。
func (payload *usersPayload) toBatchPatch() (store.AdminUserBatchPatch, error) {
	patch := store.AdminUserBatchPatch{}
	if payload == nil {
		return patch, nil
	}
	patch.Description = payload.note
	patch.Tags = payload.tags
	patch.RPM = payload.rpm
	patch.DailyQuota = usersFloatPtrToString(payload.dailyQuota)
	patch.Limit5hUSD = usersFloatPtrToString(payload.limit5hUSD)
	patch.Limit5hResetMode = payload.limit5hResetMode
	patch.LimitWeeklyUSD = usersFloatPtrToString(payload.limitWeeklyUSD)
	patch.LimitMonthlyUSD = usersFloatPtrToString(payload.limitMonthlyUSD)
	return patch, nil
}

// usersRedactFullKey 复刻 redactUserKeys：递归剥掉 fullKey 字段。
//
// 本实现的响应里本来就不输出 fullKey（掩码在前），这一步是**防御性**的：审计快照与详情
// 共用同一份对象，任何一处新增 fullKey 都会先经过这里。
func usersRedactFullKey(value any) map[string]any {
	if value == nil {
		return nil
	}
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			if key == "fullKey" {
				continue
			}
			result[key] = usersRedactValue(child)
		}
		return result
	default:
		return nil
	}
}

func usersRedactValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, child := range typed {
			if key == "fullKey" {
				continue
			}
			result[key] = usersRedactValue(child)
		}
		return result
	case []any:
		result := make([]any, 0, len(typed))
		for _, child := range typed {
			result = append(result, usersRedactValue(child))
		}
		return result
	case []map[string]any:
		result := make([]any, 0, len(typed))
		for _, child := range typed {
			result = append(result, usersRedactValue(child))
		}
		return result
	default:
		return value
	}
}

// usersCreateResponseUser 复刻 addUser / createUserOnly 返回的 user 对象（字段与顺序无关）。
func usersCreateResponseUser(row *store.AdminUserRow) map[string]any {
	role := "user"
	if row.Role != nil && *row.Role != "" {
		role = *row.Role
	}
	note := any(nil)
	if row.Description != nil && *row.Description != "" {
		note = *row.Description
	}
	group := any(nil)
	if row.ProviderGroup != nil && *row.ProviderGroup != "" {
		group = *row.ProviderGroup
	}
	var rpm any
	if row.RPM != nil && *row.RPM > 0 {
		rpm = *row.RPM
	}
	dailyQuota := any(nil)
	if quota := usersNumber(row.DailyQuota); quota != nil && *quota > 0 {
		dailyQuota = *quota
	}
	dailyResetMode := usersStringOr(row.DailyResetMode, "fixed")
	dailyResetTime := usersStringOr(row.DailyResetTime, "00:00")
	isEnabled := true
	if row.IsEnabled != nil {
		isEnabled = *row.IsEnabled
	}
	return map[string]any{
		"id":                      row.ID,
		"name":                    row.Name,
		"note":                    note,
		"role":                    role,
		"isEnabled":               isEnabled,
		"expiresAt":               store.JSONDatePtr(row.ExpiresAt),
		"rpm":                     rpm,
		"dailyQuota":              dailyQuota,
		"providerGroup":           group,
		"tags":                    usersNonNilStrings(row.Tags),
		"limit5hUsd":              usersOptionalNumber(row.Limit5hUSD),
		"limit5hResetMode":        usersStringOr(row.Limit5hResetMode, "rolling"),
		"limitWeeklyUsd":          usersOptionalNumber(row.LimitWeeklyUSD),
		"limitMonthlyUsd":         usersOptionalNumber(row.LimitMonthlyUSD),
		"limitTotalUsd":           usersOptionalNumber(row.LimitTotalUSD),
		"limitConcurrentSessions": nilIfNilInt64(row.LimitConcurrentSessions),
		"dailyResetMode":          dailyResetMode,
		"dailyResetTime":          dailyResetTime,
		"allowedModels":           usersNonNilStrings(row.AllowedModels),
	}
}

// buildUserDisplays 复刻 buildUserDisplays：为这批用户一次性预取密钥与统计，再逐个组装。
func (api *usersAPI) buildUserDisplays(
	ctx context.Context,
	rows []store.AdminUserRow,
	principal Principal,
) ([]map[string]any, error) {
	if len(rows) == 0 {
		return []map[string]any{}, nil
	}
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	keys, err := api.pools.ListAdminUserKeys(ctx, ids)
	if err != nil {
		return nil, err
	}
	usage, err := api.pools.LoadAdminUserKeyUsage(ctx, keys)
	if err != nil {
		return nil, err
	}
	keysByUser := make(map[int64][]store.AdminUserKey, len(ids))
	for _, key := range keys {
		keysByUser[key.UserID] = append(keysByUser[key.UserID], key)
	}

	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, api.buildUserDisplay(row, keysByUser[row.ID], usage, principal))
	}
	return items, nil
}

// buildUserDisplay 组装单个 UserDisplay。
//
// canReveal/canCopy 走 Node 的 canExposeFullKey：`key.canLoginWebUi && (isAdmin || self)`。
// canLoginWebUi 是**调用方密钥**的属性，属认证层信息，Principal 未携带它——因此这里按
// 「管理员或本人」判定，并在报告里登记为待 A2 确认的差异项（影响仅在混合权限场景下按钮的可见性）。
func (api *usersAPI) buildUserDisplay(
	row store.AdminUserRow,
	keys []store.AdminUserKey,
	usage map[int64]store.AdminUserKeyUsage,
	principal Principal,
) map[string]any {
	canManageKey := principal.IsAdmin || principal.UserID == row.ID
	display := row.NodeJSON()
	display["note"] = noteOrDefault(row.Description)
	display["keys"] = api.buildUserKeyDisplays(keys, usage, canManageKey)
	return display
}

// buildUserKeyDisplays 组装 UserKeyDisplay 列表。
func (api *usersAPI) buildUserKeyDisplays(
	keys []store.AdminUserKey,
	usage map[int64]store.AdminUserKeyUsage,
	canManageKey bool,
) []map[string]any {
	items := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		entry := usage[key.ID]
		stats := entry.Statistics
		modelStats := make([]map[string]any, 0, len(stats.ModelStats))
		for _, stat := range stats.ModelStats {
			modelStats = append(modelStats, map[string]any{
				"model":               stat.Model,
				"callCount":           stat.CallCount,
				"totalCost":           usersCostNumber(stat.TotalCost.String()),
				"inputTokens":         usersCostNumber(stat.InputTokens.String()),
				"outputTokens":        usersCostNumber(stat.OutputTokens.String()),
				"cacheCreationTokens": usersCostNumber(stat.CacheCreationTokens.String()),
				"cacheReadTokens":     usersCostNumber(stat.CacheReadTokens.String()),
			})
		}
		expiresAt := usersNeverExpiresText
		if key.ExpiresAt != nil {
			expiresAt = key.ExpiresAt.Format("2006-01-02")
		}
		status := "disabled"
		if key.IsEnabled == nil || *key.IsEnabled {
			status = "enabled"
		}
		var lastUsedAt any
		if stats.LastUsedAt != nil {
			lastUsedAt = store.JSONDate(*stats.LastUsedAt)
		}
		var lastProviderName any
		if stats.LastProviderName != nil {
			lastProviderName = *stats.LastProviderName
		}
		var costResetAt any
		if key.CostResetAt != nil {
			costResetAt = store.JSONDate(*key.CostResetAt)
		}
		items = append(items, map[string]any{
			"id":                      key.ID,
			"name":                    key.Name,
			"maskedKey":               usersMaskKey(key.Key),
			"canReveal":               canManageKey,
			"canCopy":                 canManageKey,
			"expiresAt":               expiresAt,
			"status":                  status,
			"todayUsage":              usersCostNumber(entry.Today.TotalCost.String()),
			"todayCallCount":          stats.TodayCallCount,
			"todayTokens":             usersCostNumber(entry.Today.TotalTokens.String()),
			"lastUsedAt":              lastUsedAt,
			"lastProviderName":        lastProviderName,
			"modelStats":              modelStats,
			"createdAt":               store.JSONDate(key.CreatedAt),
			"createdAtFormatted":      usersFormatLocaltime(key.CreatedAt),
			"canLoginWebUi":           key.CanLoginWebUI == nil || *key.CanLoginWebUI,
			"limit5hUsd":              usersOptionalNumber(key.Limit5hUSD),
			"limit5hResetMode":        usersStringOr(key.Limit5hResetMode, "rolling"),
			"limitDailyUsd":           usersOptionalNumber(key.LimitDailyUSD),
			"dailyResetMode":          usersStringOr(key.DailyResetMode, "fixed"),
			"dailyResetTime":          usersStringOr(key.DailyResetTime, "00:00"),
			"limitWeeklyUsd":          usersOptionalNumber(key.LimitWeeklyUSD),
			"limitMonthlyUsd":         usersOptionalNumber(key.LimitMonthlyUSD),
			"limitTotalUsd":           usersOptionalNumber(key.LimitTotalUSD),
			"limitConcurrentSessions": nilIfNilInt64OrZero(key.LimitConcurrentSessions),
			"costResetAt":             costResetAt,
			"providerGroup":           nilIfNilString(key.ProviderGroup),
		})
	}
	return items
}

// usersNeverExpiresText 是 t("neverExpires") 在默认语言（zh-CN）下的取值
// （messages/zh-CN/users.json:4）。管理面 API 请求没有 locale，next-intl 的 getLocale 落到
// defaultLocale，因此 Node 侧实际返回的就是这一串。
//
// 这是**唯一**一处把界面文案固化进 Go 的地方，且只用于密钥的 expiresAt 展示字段。
const usersNeverExpiresText = "永不过期"

// usersFormatLocaltime 复刻 createdAt.toLocaleString(locale, {year,month,day,hour,minute,second})
// 在 zh-CN 下的输出。
//
// zh-CN 的 numeric 日期格式为 "YYYY/MM/DD HH:mm:ss"（24 小时制），时区取进程本地时区。
// 与 Node 的 ICU 输出逐字对齐需在 A2 对拍确认（已登记白名单：locale 相关字段）。
func usersFormatLocaltime(value time.Time) string {
	return value.Local().Format("2006/01/02 15:04:05")
}

// usersMaskKey 复刻 maskKey：长度 <= 8 时全掩，否则保留头 4 与尾 4。
func usersMaskKey(key string) string {
	if len(key) <= 8 {
		return "••••••"
	}
	return key[:4] + "••••••" + key[len(key)-4:]
}

// noteOrDefault 复刻 `note: user.description || undefined`：空串在 JSON 里被省略。
func noteOrDefault(description *string) any {
	if description == nil || *description == "" {
		return nil
	}
	return *description
}

func usersNonNilStrings(value []string) []string {
	if value == nil {
		return []string{}
	}
	return value
}

func nilIfNilInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

// nilIfNilInt64OrZero 复刻 `key.limitConcurrentSessions || 0`（null 与 0 同为 0）。
func nilIfNilInt64OrZero(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

// usersOptionalNumber 复刻 parseOptionalNumber：空为 null，0 保留。
func usersOptionalNumber(value json.Number) any {
	parsed := usersNumber(value)
	if parsed == nil {
		return nil
	}
	return *parsed
}

func sortStrings(values []string) {
	for index := 1; index < len(values); index++ {
		for cursor := index; cursor > 0 && values[cursor] < values[cursor-1]; cursor-- {
			values[cursor], values[cursor-1] = values[cursor-1], values[cursor]
		}
	}
}

// nilIfNilString 把可空字符串转成响应值（nil → JSON null）。
func nilIfNilString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}
