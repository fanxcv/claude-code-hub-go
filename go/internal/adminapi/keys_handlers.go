package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 keys 资源（A1-2）在 keys.go 之外的处理器：资源的读取（列表）与写路径
// （create / update / delete / enable / renew / batchUpdate）。
//
// 与 Node 的一一对应（src/app/api/v1/resources/keys/handlers.ts → src/actions/keys.ts）：
//
//	GET    /users/{userId}/keys   listUserKeys    → getKeys / getKeysWithStatistics
//	POST   /users/{userId}/keys   createUserKey   → addKey
//	POST   /users:self/keys       createSelfKey   → addKey（目标用户取会话身份）
//	POST   /keys/{keyId}:enable   enableKey       → toggleKeyEnabled
//	POST   /keys/{keyId}:renew    renewKey        → renewKeyExpiresAt
//	PATCH  /keys/{keyId}          updateKey       → editKey
//	DELETE /keys/{keyId}          deleteKey       → removeKey
//	POST   /keys:batchUpdate      batchUpdateKeys → batchUpdateKeys
//
// 写路径的两道门必须分清（Node 把它们放在两层）：
//  1. 处理器层 requireKeyWriteSession（keys/handlers.ts:129-148）：无身份 → 401 auth.missing；
//     非管理员且凭据密钥 canLoginWebUi 不为 true → 403 auth.forbidden。**先于**请求体解析。
//  2. action 层 denyKeyWriteForReadOnlySession（actions/keys.ts:41-53）：同一判据，失败给
//     PERMISSION_DENIED。日常可观测的是第 1 道；第 2 道只在直接调 action 时可见，Go 侧不再复刻。

// keysAPI 是 keys 资源处理器的依赖集合（与 usersAPI 同构，装配期构造一次）。
//
// sessions 与 fixed5h 是**仅读档端点**需要的 Redis 运行态（见 keys_usage.go）；两者可空，
// 缺失时 RegisterKeysRoutes 只不注册那三条（其余十一条照常）。
type keysAPI struct {
	deps     Deps
	pools    *store.Pools
	problems ProblemWriter
	audit    AuditSink
	invalid  Invalidator
	logger   *logx.Logger
	now      func() time.Time
	sessions SessionCounter
	fixed5h  Fixed5hWindowReader
}

// newKeysAPI 组装处理器依赖；Store 未装配时返回 nil（调用方据此不注册路由）。
func newKeysAPI(deps Deps) *keysAPI {
	if deps.Store == nil {
		return nil
	}
	logger := deps.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	problems := deps.Problems
	if problems == nil {
		problems = NewProblems(logger)
	}
	return &keysAPI{
		deps:     deps,
		pools:    deps.Store,
		problems: problems,
		audit:    deps.Audit,
		invalid:  deps.Invalidator,
		logger:   logger,
		now:      time.Now,
		sessions: deps.SessionCounts,
		fixed5h:  deps.Fixed5hWindows,
	}
}

// keysActionError 造一个 keys 资源的 action 错误。
//
// status 为 0 时由 problem.go 的 key 码表推导：三个「未找到」码与两个权限码之外一律 400。
// 显式传 status 的场合是 Node 用中文子串判状态的调用点（"密钥不存在" 等）——那条子串分支
// 按裁决不移植，必须在调用点写死，见 problem.go 的说明。
func keysActionError(code string, status int, err error) *ActionError {
	return NewActionError("key", code, status, err)
}

// requireKeyWriteSession 复刻 requireKeyWriteSession（keys/handlers.ts:129-148）。
//
// 返回 false 表示已作答拒绝，调用方必须立即返回。
func (api *keysAPI) requireKeyWriteSession(writer http.ResponseWriter, request *http.Request) bool {
	principal, ok := PrincipalFrom(request.Context())
	if !ok || principal.UserID == 0 {
		api.problems.WriteProblem(writer, request, http.StatusUnauthorized, "auth.missing",
			"Authentication is required.")
		return false
	}
	if principal.IsAdmin || principal.CanLoginWebUI {
		return true
	}
	api.logger.Warn("admin_key_write_denied", map[string]any{
		"path":        request.URL.Path,
		"userId":      principal.UserID,
		"webSession":  principal.WebSession,
		"canLoginWeb": principal.CanLoginWebUI,
	})
	api.problems.WriteProblem(writer, request, http.StatusForbidden, "auth.forbidden",
		"Read-only sessions cannot manage keys.")
	return false
}

// listUserKeys 复刻 listUserKeys（keys/handlers.ts:30-47）。
//
// include=statistics 走 getKeysWithStatistics，否则走 getKeys；两者都把密钥明文换成人眼可读的
// maskedKey（handler 的 sanitizeKey）。
func (api *keysAPI) listUserKeys(writer http.ResponseWriter, request *http.Request) {
	userID, ok := keysUserIDFromPath(request)
	if !ok {
		api.problems.WriteValidationError(writer, request, []InvalidParam{
			keysInvalid("userId", "invalid_type", "Expected number, received nan"),
		})
		return
	}
	query, problems := keysParseListQuery(request)
	if problems != nil {
		api.problems.WriteValidationError(writer, request, problems)
		return
	}
	// 路由是管理档，守卫已保证 IsAdmin；Node action 里的「非管理员只能看自己」因此不可达。

	keys, err := api.pools.ListAdminUserKeys(request.Context(), []int64{userID})
	if err != nil {
		api.writeKeysActionError(writer, request, "listUserKeys", err)
		return
	}

	items := make([]map[string]any, 0, len(keys))
	if query.Statistics {
		usage, usageErr := api.pools.LoadAdminUserKeyUsage(request.Context(), keys)
		if usageErr != nil {
			api.writeKeysActionError(writer, request, "getKeysWithStatistics", usageErr)
			return
		}
		for _, key := range keys {
			items = append(items, keysStatisticsItem(usage[key.ID].Statistics))
		}
	} else {
		for _, key := range keys {
			items = append(items, keysListItem(key))
		}
	}
	writeShellJSON(writer, http.StatusOK, map[string]any{"items": items})
}

// keysStatisticsItem 复刻 KeyStatistics（src/repository/key.ts:790-803）。
//
// 注意 modelStats 的 totalCost 在 Node 里是 Decimal.toDecimalPlaces(6).toNumber() —— 数值而非
// 字符串；这里保留 numeric 原文（JSON number 的形状一致，尾随零的差异登记进白名单）。
func keysStatisticsItem(statistics store.AdminUserKeyStatistics) map[string]any {
	modelStats := make([]map[string]any, 0, len(statistics.ModelStats))
	for _, item := range statistics.ModelStats {
		modelStats = append(modelStats, map[string]any{
			"model":               item.Model,
			"callCount":           item.CallCount,
			"totalCost":           usersCostNumber(item.TotalCost.String()),
			"inputTokens":         usersCostNumber(item.InputTokens.String()),
			"outputTokens":        usersCostNumber(item.OutputTokens.String()),
			"cacheCreationTokens": usersCostNumber(item.CacheCreationTokens.String()),
			"cacheReadTokens":     usersCostNumber(item.CacheReadTokens.String()),
		})
	}
	var lastUsedAt any
	if statistics.LastUsedAt != nil {
		lastUsedAt = store.JSONDate(*statistics.LastUsedAt)
	}
	var lastProviderName any
	if statistics.LastProviderName != nil {
		lastProviderName = *statistics.LastProviderName
	}
	return map[string]any{
		"keyId":            statistics.KeyID,
		"todayCallCount":   statistics.TodayCallCount,
		"lastUsedAt":       lastUsedAt,
		"lastProviderName": lastProviderName,
		"modelStats":       modelStats,
	}
}

// keysListItem 复刻 toKey（src/repository/_shared/transformers.ts:76-99）叠加 sanitizeKey
// （keys/handlers.ts:345-356）：密钥明文换成 maskedKey，其余列逐字保留。
func keysListItem(key store.AdminUserKey) map[string]any {
	isEnabled := true
	if key.IsEnabled != nil {
		isEnabled = *key.IsEnabled
	}
	canLoginWebUI := true
	if key.CanLoginWebUI != nil {
		canLoginWebUI = *key.CanLoginWebUI
	}
	var expiresAt any
	if key.ExpiresAt != nil {
		expiresAt = store.JSONDate(*key.ExpiresAt)
	}
	var costResetAt any
	if key.CostResetAt != nil {
		costResetAt = store.JSONDate(*key.CostResetAt)
	}
	return map[string]any{
		"id":                      key.ID,
		"userId":                  key.UserID,
		"maskedKey":               usersMaskKey(key.Key),
		"name":                    key.Name,
		"isEnabled":               isEnabled,
		"expiresAt":               expiresAt,
		"canLoginWebUi":           canLoginWebUI,
		"limit5hUsd":              keysMoneyValue(key.Limit5hUSD),
		"limit5hResetMode":        usersStringOr(key.Limit5hResetMode, "rolling"),
		"limitDailyUsd":           keysMoneyValue(key.LimitDailyUSD),
		"dailyResetMode":          usersStringOr(key.DailyResetMode, "fixed"),
		"dailyResetTime":          usersStringOr(key.DailyResetTime, "00:00"),
		"limitWeeklyUsd":          keysMoneyValue(key.LimitWeeklyUSD),
		"limitMonthlyUsd":         keysMoneyValue(key.LimitMonthlyUSD),
		"limitTotalUsd":           keysTotalMoneyValue(key.LimitTotalUSD),
		"costResetAt":             costResetAt,
		"limitConcurrentSessions": nilIfNilInt64OrZero(key.LimitConcurrentSessions),
		"providerGroup":           nilIfNilString(key.ProviderGroup),
		"cacheTtlPreference":      nilIfNilString(key.CacheTTLPreference),
		"createdAt":               store.JSONDate(key.CreatedAt),
		"updatedAt":               store.JSONDate(key.UpdatedAt),
		"deletedAt":               nil,
	}
}

// keysMoneyValue 复刻 toKey 里金额字段的 `dbKey?.limit5hUsd ? parseFloat(...) : null`：
// **空串与 0 都落到 null**（truthiness 判定，不是判空）。
func keysMoneyValue(value json.Number) any {
	parsed := usersNumber(value)
	if parsed == nil || *parsed == 0 {
		return nil
	}
	return *parsed
}

// keysTotalMoneyValue 复刻 limitTotalUsd 的判据：`!== null && !== undefined` → parseFloat，
// 因此 0 会保留成 0（与其余金额字段的真值判定不同，这是 Node 侧的不一致，照抄）。
func keysTotalMoneyValue(value json.Number) any {
	parsed := usersNumber(value)
	if parsed == nil {
		return nil
	}
	return *parsed
}

// createUserKey 复刻 createUserKey（keys/handlers.ts:49-70）。
func (api *keysAPI) createUserKey(writer http.ResponseWriter, request *http.Request) {
	userID, ok := keysUserIDFromPath(request)
	if !ok {
		api.problems.WriteValidationError(writer, request, []InvalidParam{
			keysInvalid("userId", "invalid_type", "Expected number, received nan"),
		})
		return
	}
	api.createKeyForUser(writer, request, userID, false)
}

// createSelfKey 复刻 createSelfKey（keys/handlers.ts:72-109）。
//
// 两道额外门，顺序与 Node 一致（**先于**请求体解析）：会话身份必须存在；且凭据密钥
// canLoginWebUi 必须为 true——读档认证会放行 false 的只读会话，若这里放过，只读密钥就能
// 铸造一把可登录 Web UI 的新密钥。
func (api *keysAPI) createSelfKey(writer http.ResponseWriter, request *http.Request) {
	principal, ok := PrincipalFrom(request.Context())
	if !ok || principal.UserID == 0 {
		api.problems.WriteProblem(writer, request, http.StatusUnauthorized, "auth.missing",
			"Authentication is required.")
		return
	}
	if !principal.CanLoginWebUI {
		api.problems.WriteProblem(writer, request, http.StatusForbidden, "auth.forbidden",
			"Read-only sessions cannot create keys.")
		return
	}
	api.createKeyForUser(writer, request, principal.UserID, true)
}

// createKeyForUser 是 addKey 的共用后半段（两条创建路由都走它）。
//
// selfService 为 true 表示来自 /users:self/keys：目标用户恒为会话身份，且 Node 在此路径上
// **不**校验非管理员的 providerGroup 子集（请求体里的 userId 被 schema 的 .strict() 挡在门外，
// 目标用户永远是会话身份）。
func (api *keysAPI) createKeyForUser(
	writer http.ResponseWriter,
	request *http.Request,
	userID int64,
	selfService bool,
) {
	principal, _ := PrincipalFrom(request.Context())
	mutation, problems := keysParseMutation(request, true)
	if problems != nil {
		api.problems.WriteValidationError(writer, request, problems)
		return
	}

	user, err := api.pools.FindAdminUserByID(request.Context(), userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Node: { ok:false, error:"用户不存在" }（无 errorCode）⇒ 404 + key.not_found。
			api.problems.WriteActionError(writer, request,
				keysActionError("", http.StatusNotFound, nil))
			return
		}
		api.writeKeysActionError(writer, request, "addKey", err)
		return
	}

	providerGroup, groupProblem := api.resolveCreateProviderGroup(
		request.Context(), principal, mutation, user, selfService)
	if groupProblem != nil {
		api.problems.WriteActionError(writer, request, groupProblem)
		return
	}

	// 同名且正在生效的密钥：Node 用 DUPLICATE_NAME + errorParams{name}。
	existing, err := api.pools.FindAdminActiveKeyByUserAndName(request.Context(), userID, mutation.Name)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		api.writeKeysActionError(writer, request, "addKey", err)
		return
	}
	if existing != nil {
		duplicate := NewActionError("key", "DUPLICATE_NAME", 0, nil)
		duplicate.Params = map[string]any{"name": mutation.Name}
		api.problems.WriteActionError(writer, request, duplicate)
		return
	}

	if limitProblem := keysCheckLimitsAgainstUser(mutation, user, "addKey"); limitProblem != nil {
		api.problems.WriteActionError(writer, request, limitProblem)
		return
	}

	location, err := api.systemLocation(request.Context())
	if err != nil {
		api.writeKeysActionError(writer, request, "addKey", err)
		return
	}
	expiresAt, expiresProblem := keysResolveExpiresAt(mutation.ExpiresAt, location)
	if expiresProblem != nil {
		api.problems.WriteActionError(writer, request, expiresProblem)
		return
	}
	generatedKey, err := usersGenerateKey()
	if err != nil {
		api.writeKeysActionError(writer, request, "addKey", err)
		return
	}
	isEnabled := true
	if mutation.IsEnabled != nil {
		isEnabled = *mutation.IsEnabled
	}
	canLoginWebUI := true
	if mutation.CanLoginWebUI != nil {
		canLoginWebUI = *mutation.CanLoginWebUI
	}

	record, err := api.pools.CreateAdminKey(request.Context(), store.AdminKeyCreateInput{
		UserID:                  userID,
		Key:                     generatedKey,
		Name:                    mutation.Name,
		IsEnabled:               isEnabled,
		ExpiresAt:               expiresAt,
		CanLoginWebUI:           canLoginWebUI,
		Limit5hUSD:              keysMoneyLiteral(mutation.Limit5hUSD),
		Limit5hResetMode:        usersStringOr(mutation.Limit5hResetMode, "rolling"),
		LimitDailyUSD:           keysMoneyLiteral(mutation.LimitDailyUSD),
		DailyResetMode:          usersStringOr(mutation.DailyResetMode, "fixed"),
		DailyResetTime:          usersStringOr(mutation.DailyResetTime, "00:00"),
		LimitWeeklyUSD:          keysMoneyLiteral(mutation.LimitWeeklyUSD),
		LimitMonthlyUSD:         keysMoneyLiteral(mutation.LimitMonthlyUSD),
		LimitTotalUSD:           keysMoneyLiteral(mutation.LimitTotalUSD),
		LimitConcurrentSessions: keysConcurrentLiteral(mutation.LimitConcurrentSessions),
		ProviderGroup:           providerGroup,
		CacheTTLPreference:      mutation.CacheTTLPreference,
	})
	if err != nil {
		// Node 的失败审计只写稳定码 CREATE_FAILED（原始 pg 错误可能含密钥或约束名）。
		api.emitKeyAudit(request, AuditEvent{
			Category:     "key",
			Principal:    principal,
			Action:       "key.create",
			TargetType:   "key",
			TargetName:   mutation.Name,
			Success:      false,
			ErrorMessage: "CREATE_FAILED",
		})
		api.writeKeysActionError(writer, request, "addKey", err)
		return
	}

	// 用户分组 = 其密钥分组的并集；Node 只在管理员创建时同步（actions/keys.ts:373-375）。
	if principal.IsAdmin {
		if _, syncErr := api.pools.SyncAdminUserProviderGroupFromKeys(request.Context(), userID); syncErr != nil {
			api.logger.Warn("admin_keys_provider_group_sync_failed", map[string]any{
				"userId": userID, "error": syncErr.Error(),
			})
		}
	}
	api.emitKeyAudit(request, AuditEvent{
		Category:   "key",
		Principal:  principal,
		Action:     "key.create",
		TargetType: "key",
		TargetID:   strconv.FormatInt(record.ID, 10),
		TargetName: record.Name,
		Details:    keysAuditAfter(record),
		Success:    true,
	})

	// Node 的 createdResponse：201 + Location 指向新建资源；id 缺失时退回列表路径。
	writer.Header().Set("Location", MountPrefix+"/keys/"+strconv.FormatInt(record.ID, 10))
	writeShellJSON(writer, http.StatusCreated, map[string]any{
		"id":           record.ID,
		"generatedKey": generatedKey,
		"name":         mutation.Name,
	})
}

// resolveCreateProviderGroup 复刻 addKey 的分组安全模型（actions/keys.ts:151-186，NOTE(#400)）。
//
// 管理员：用请求里的分组（空 → "default"）。非管理员（仅 /users:self/keys 可达）：
// 请求分组必须是用户现有分组的子集；且要包含 default 时必须已持有一把 default 分组的密钥。
func (api *keysAPI) resolveCreateProviderGroup(
	ctx context.Context,
	principal Principal,
	mutation keysMutation,
	user *store.AdminUserRow,
	selfService bool,
) (*string, *ActionError) {
	requested := ""
	if mutation.ProviderGroup.Provided && !mutation.ProviderGroup.Null {
		requested = mutation.ProviderGroup.Value
	}
	if principal.IsAdmin {
		normalized := usersNormalizeProviderGroup(requested)
		return &normalized, nil
	}
	if !selfService {
		// /users/{id}/keys 是管理档路由，非管理员到此说明守卫被绕过——按无权限作答。
		return nil, keysActionError("PERMISSION_DENIED", 0, nil)
	}
	userGroups := store.SplitProviderGroups(user.ProviderGroup)
	requestedGroups := store.SplitProviderGroups(&requested)
	normalizedRequested := usersNormalizeProviderGroup(requested)
	for _, group := range userGroups {
		if group == "*" {
			return &normalizedRequested, nil
		}
	}
	allowed := make(map[string]struct{}, len(userGroups))
	for _, group := range userGroups {
		allowed[group] = struct{}{}
	}
	if _, wantsDefault := allowed["default"]; !wantsDefault {
		containsDefault := false
		for _, group := range requestedGroups {
			if group == "default" {
				containsDefault = true
			}
		}
		if containsDefault {
			hasDefaultKey, err := api.userHasDefaultGroupKey(ctx, user.ID)
			if err != nil {
				return nil, keysActionError("", http.StatusBadRequest, err)
			}
			if !hasDefaultKey {
				return nil, keysActionError("", http.StatusBadRequest, nil)
			}
		}
	}
	for _, group := range requestedGroups {
		if _, ok := allowed[group]; !ok {
			return nil, keysActionError("", http.StatusBadRequest, nil)
		}
	}
	return &normalizedRequested, nil
}

// userHasDefaultGroupKey 判断用户是否已持有含 default 分组的密钥
// （actions/keys.ts:177-181 的 hasDefaultKey）。
func (api *keysAPI) userHasDefaultGroupKey(ctx context.Context, userID int64) (bool, error) {
	keys, err := api.pools.ListAdminUserKeys(ctx, []int64{userID})
	if err != nil {
		return false, err
	}
	for _, key := range keys {
		group := key.ProviderGroup
		if group == nil || strings.TrimSpace(*group) == "" {
			return true, nil
		}
		for _, item := range store.SplitProviderGroups(group) {
			if item == "default" {
				return true, nil
			}
		}
	}
	return false, nil
}

// keysCheckLimitsAgainstUser 复刻 addKey 的五组「Key 限额不得超过用户限额」
// （actions/keys.ts:200-336）。addKey 的失败带 KEY_LIMIT_*_EXCEEDS_USER_LIMIT 码与
// errorParams{keyLimit,userLimit}；editKey 的同类失败只有文案（无码），故用 operation 区分。
func keysCheckLimitsAgainstUser(
	mutation keysMutation,
	user *store.AdminUserRow,
	operation string,
) *ActionError {
	type check struct {
		field    string
		code     string
		keyValue float64
		keySet   bool
		userText json.Number
	}
	checks := []check{
		{"limit5hUsd", "KEY_LIMIT_5H_EXCEEDS_USER_LIMIT",
			keysFloatOrZero(mutation.Limit5hUSD), mutation.Limit5hUSD.Provided && !mutation.Limit5hUSD.Null,
			user.Limit5hUSD},
		{"limitDailyUsd", "KEY_LIMIT_DAILY_EXCEEDS_USER_LIMIT",
			keysFloatOrZero(mutation.LimitDailyUSD), mutation.LimitDailyUSD.Provided && !mutation.LimitDailyUSD.Null,
			user.DailyQuota},
		{"limitWeeklyUsd", "KEY_LIMIT_WEEKLY_EXCEEDS_USER_LIMIT",
			keysFloatOrZero(mutation.LimitWeeklyUSD), mutation.LimitWeeklyUSD.Provided && !mutation.LimitWeeklyUSD.Null,
			user.LimitWeeklyUSD},
		{"limitMonthlyUsd", "KEY_LIMIT_MONTHLY_EXCEEDS_USER_LIMIT",
			keysFloatOrZero(mutation.LimitMonthlyUSD), mutation.LimitMonthlyUSD.Provided && !mutation.LimitMonthlyUSD.Null,
			user.LimitMonthlyUSD},
	}
	for _, item := range checks {
		if !item.keySet || item.keyValue <= 0 {
			continue
		}
		userValue := usersNumber(item.userText)
		if userValue == nil || *userValue <= 0 || item.keyValue <= *userValue {
			continue
		}
		err := keysActionError("", 0, nil)
		if operation == "addKey" {
			err = keysActionError(item.code, 0, nil)
			err.Params = map[string]any{
				"keyLimit":  strconv.FormatFloat(item.keyValue, 'f', -1, 64),
				"userLimit": strconv.FormatFloat(*userValue, 'f', -1, 64),
			}
		}
		return err
	}
	if mutation.LimitConcurrentSessions.Provided && !mutation.LimitConcurrentSessions.Null {
		keyValue := float64(mutation.LimitConcurrentSessions.Value)
		if keyValue > 0 && user.LimitConcurrentSessions != nil && *user.LimitConcurrentSessions > 0 &&
			keyValue > float64(*user.LimitConcurrentSessions) {
			err := keysActionError("", 0, nil)
			if operation == "addKey" {
				err = keysActionError("KEY_LIMIT_CONCURRENT_EXCEEDS_USER_LIMIT", 0, nil)
				err.Params = map[string]any{
					"keyLimit":  strconv.FormatFloat(keyValue, 'f', -1, 64),
					"userLimit": strconv.Itoa(int(*user.LimitConcurrentSessions)),
				}
			}
			return err
		}
	}
	return nil
}

// keysMoneyLiteral 取三态金额字段写库用的十进制文本（未提供或 null → nil）。
func keysMoneyLiteral(value store.Nullable[string]) *string {
	if !value.Provided || value.Null {
		return nil
	}
	text := value.Value
	return &text
}

// keysConcurrentLiteral 取三态整数限额写库用的值（未提供或 null → nil）。
func keysConcurrentLiteral(value store.Nullable[int32]) *int32 {
	if !value.Provided || value.Null {
		return nil
	}
	number := value.Value
	return &number
}

// keysFloatOrZero 取三态金额字段的浮点值（未提供或 null → 0，配合「> 0 才校验」的语义）。
func keysFloatOrZero(value store.Nullable[string]) float64 {
	if !value.Provided || value.Null {
		return 0
	}
	parsed, err := strconv.ParseFloat(value.Value, 64)
	if err != nil {
		return 0
	}
	return parsed
}

// keysResolveExpiresAt 复刻 `parseDateInputAsTimezone(expiresAt, timezone)` 的三态语义：
// 未提供 → nil（永不过期）；显式 null → nil；有值 → 按系统时区解析。
func keysResolveExpiresAt(
	value store.Nullable[string],
	location *time.Location,
) (*time.Time, *ActionError) {
	if !value.Provided || value.Null {
		return nil, nil
	}
	if strings.TrimSpace(value.Value) == "" {
		// KeyFormSchema 把空串 transform 成 undefined（永不过期）。
		return nil, nil
	}
	parsed, err := parseUsersDateInput(value.Value, location)
	if err != nil {
		return nil, keysActionError("", http.StatusBadRequest, err)
	}
	return &parsed, nil
}

// systemLocation 取系统时区（复用 users 侧的三级取值实现）。
func (api *keysAPI) systemLocation(ctx context.Context) (*time.Location, error) {
	users := &usersAPI{pools: api.pools, logger: api.logger}
	return users.systemLocation(ctx)
}

// emitKeyAudit 写一条 keys 审计（IP/UA 从请求补齐，同 keys.go 的 emitKeyAudit）。
//
// IP 同样走 auditClientIP（audit_ip.go 的统一提取链）。
func (api *keysAPI) emitKeyAudit(request *http.Request, event AuditEvent) {
	if api.audit == nil {
		return
	}
	if event.IP == "" {
		event.IP = auditClientIP(request.Context(), api.pools, request)
	}
	if event.UserAgent == "" {
		event.UserAgent = request.Header.Get("User-Agent")
	}
	api.audit.Emit(request.Context(), event)
}

// keysAuditAfter 组装审计 after 快照（Node editKey/addKey 的 after 字段集）。
func keysAuditAfter(record *store.AdminKeyRecord) map[string]any {
	return map[string]any{
		"id":                      record.ID,
		"userId":                  record.UserID,
		"name":                    record.Name,
		"isEnabled":               record.IsEnabled != nil && *record.IsEnabled,
		"expiresAt":               record.ExpiresAt,
		"canLoginWebUi":           record.CanLoginWebUI != nil && *record.CanLoginWebUI,
		"providerGroup":           nilIfNilString(record.ProviderGroup),
		"limit5hUsd":              nilIfNilNumber(record.Limit5hUSD),
		"limit5hResetMode":        nilIfNilString(record.Limit5hResetMode),
		"limitDailyUsd":           nilIfNilNumber(record.LimitDailyUSD),
		"dailyResetMode":          nilIfNilString(record.DailyResetMode),
		"dailyResetTime":          nilIfNilString(record.DailyResetTime),
		"limitWeeklyUsd":          nilIfNilNumber(record.LimitWeeklyUSD),
		"limitMonthlyUsd":         nilIfNilNumber(record.LimitMonthlyUSD),
		"limitTotalUsd":           nilIfNilNumber(record.LimitTotalUSD),
		"limitConcurrentSessions": keysInt32Value(record.LimitConcurrentSessions),
		"cacheTtlPreference":      nilIfNilString(record.CacheTTLPreference),
	}
}

// nilIfNilNumber 把可空 numeric 转成响应值（空 → JSON null）。
func nilIfNilNumber(value json.Number) any {
	parsed := usersNumber(value)
	if parsed == nil {
		return nil
	}
	return *parsed
}

// keysInt32Value 把可空 int32 转成响应值。
func keysInt32Value(value *int32) any {
	if value == nil {
		return nil
	}
	return *value
}

// writeKeysActionError 记录并作答一次内部错误（Store/系统错误）。
func (api *keysAPI) writeKeysActionError(
	writer http.ResponseWriter,
	request *http.Request,
	operation string,
	err error,
) {
	api.logger.Error("admin_keys_"+operation+"_failed", map[string]any{
		"path":  request.URL.Path,
		"error": err.Error(),
	})
	api.problems.WriteActionError(writer, request, keysActionError("", 0, err))
}

// keysUserIDFromPath 取路径参数 userId。
//
// 路由正则已保证是纯数字；仍做一次解析，避免「正则被改宽」直接变成越界或 0 号用户。
func keysUserIDFromPath(request *http.Request) (int64, bool) {
	params := ParamsFrom(request.Context())
	raw, ok := params["userId"]
	if !ok {
		return 0, false
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, false
	}
	return parsed, true
}

// enableKey 复刻 enableKey → toggleKeyEnabled（handlers.ts:175-192、actions/keys.ts:1223-1268）。
func (api *keysAPI) enableKey(writer http.ResponseWriter, request *http.Request) {
	if !api.requireKeyWriteSession(writer, request) {
		return
	}
	keyID, ok := keyIDFromParams(request)
	if !ok {
		api.problems.WriteValidationError(writer, request, []InvalidParam{
			keysInvalid("keyId", "invalid_type", "Expected number, received nan"),
		})
		return
	}
	body, problems := keysParseEnable(request)
	if problems != nil {
		api.problems.WriteValidationError(writer, request, problems)
		return
	}
	principal, _ := PrincipalFrom(request.Context())
	record, ok := api.loadKeyForWrite(writer, request)
	if !ok {
		return
	}
	if !principal.IsAdmin && principal.UserID != record.UserID {
		api.problems.WriteActionError(writer, request, keysActionError("PERMISSION_DENIED", 0, nil))
		return
	}
	if !body.Enabled {
		active, err := api.pools.CountAdminActiveKeysByUser(request.Context(), record.UserID)
		if err != nil {
			api.writeKeysActionError(writer, request, "toggleKeyEnabled", err)
			return
		}
		if active <= 1 {
			api.problems.WriteActionError(writer, request,
				keysActionError("CANNOT_DISABLE_LAST_KEY", 0, nil))
			return
		}
	}
	enabled := body.Enabled
	if _, _, err := api.pools.UpdateAdminKey(request.Context(), keyID, store.AdminKeyPatch{
		IsEnabled: &enabled,
	}); err != nil {
		api.writeKeysActionError(writer, request, "toggleKeyEnabled", err)
		return
	}
	// Node 的 updateKey 会按新状态刷新/失效认证缓存；这里统一失效一次（下次鉴权重新读库）。
	api.invalidateKeyAuth(request.Context(), record.Key)
	writeShellJSON(writer, http.StatusOK, map[string]any{"ok": true})
}

// renewKey 复刻 renewKey → renewKeyExpiresAt（handlers.ts:194-211、actions/keys.ts:1532-1580）。
func (api *keysAPI) renewKey(writer http.ResponseWriter, request *http.Request) {
	if !api.requireKeyWriteSession(writer, request) {
		return
	}
	keyID, ok := keyIDFromParams(request)
	if !ok {
		api.problems.WriteValidationError(writer, request, []InvalidParam{
			keysInvalid("keyId", "invalid_type", "Expected number, received nan"),
		})
		return
	}
	body, problems := keysParseRenew(request)
	if problems != nil {
		api.problems.WriteValidationError(writer, request, problems)
		return
	}
	principal, _ := PrincipalFrom(request.Context())
	record, ok := api.loadKeyForWrite(writer, request)
	if !ok {
		return
	}
	if !principal.IsAdmin && principal.UserID != record.UserID {
		api.problems.WriteActionError(writer, request, keysActionError("PERMISSION_DENIED", 0, nil))
		return
	}
	location, err := api.systemLocation(request.Context())
	if err != nil {
		api.writeKeysActionError(writer, request, "renewKeyExpiresAt", err)
		return
	}
	expiresAt, parseErr := parseUsersDateInput(body.ExpiresAt, location)
	if parseErr != nil {
		api.problems.WriteActionError(writer, request,
			keysActionError("", http.StatusBadRequest, parseErr))
		return
	}
	patch := store.AdminKeyPatch{ExpiresAt: store.SomeValue(expiresAt)}
	if body.EnableKey {
		enabled := true
		patch.IsEnabled = &enabled
	}
	if _, _, err := api.pools.UpdateAdminKey(request.Context(), keyID, patch); err != nil {
		api.writeKeysActionError(writer, request, "renewKeyExpiresAt", err)
		return
	}
	writeShellJSON(writer, http.StatusOK, map[string]any{"ok": true})
}

// updateKey 复刻 updateKey → editKey（handlers.ts:150-162、actions/keys.ts:429-760）。
//
// 顺序与 Node 一致：写会话门 → schema 校验 → 取密钥（404）→ 属主/管理员 → 非管理员禁改分组
// → 非管理员关停最后一个启用密钥的保护 → 限额不得超用户限额 → 落库 → 同步用户分组（管理员
// 且改了分组）→ 缓存失效 → 审计 before/after。
func (api *keysAPI) updateKey(writer http.ResponseWriter, request *http.Request) {
	if !api.requireKeyWriteSession(writer, request) {
		return
	}
	keyID, ok := keyIDFromParams(request)
	if !ok {
		api.problems.WriteValidationError(writer, request, []InvalidParam{
			keysInvalid("keyId", "invalid_type", "Expected number, received nan"),
		})
		return
	}
	mutation, problems := keysParseMutation(request, true)
	if problems != nil {
		api.problems.WriteValidationError(writer, request, problems)
		return
	}
	principal, _ := PrincipalFrom(request.Context())
	record, ok := api.loadKeyForWrite(writer, request)
	if !ok {
		return
	}
	if !principal.IsAdmin && principal.UserID != record.UserID {
		api.problems.WriteActionError(writer, request, keysActionError("PERMISSION_DENIED", 0, nil))
		return
	}
	if !principal.IsAdmin && mutation.ProviderGroup.Provided {
		current := usersNormalizeProviderGroup(usersStringOr(record.ProviderGroup, ""))
		requested := usersNormalizeProviderGroup(keysProviderGroupValue(mutation.ProviderGroup))
		if current != requested {
			api.problems.WriteActionError(writer, request, keysActionError("PERMISSION_DENIED", 0, nil))
			return
		}
	}
	if !principal.IsAdmin && mutation.IsEnabled != nil && !*mutation.IsEnabled &&
		record.IsEnabled != nil && *record.IsEnabled {
		active, err := api.pools.CountAdminActiveKeysByUser(request.Context(), record.UserID)
		if err != nil {
			api.writeKeysActionError(writer, request, "editKey", err)
			return
		}
		if active <= 1 {
			api.problems.WriteActionError(writer, request,
				keysActionError("CANNOT_DISABLE_LAST_KEY", 0, nil))
			return
		}
	}

	quota, err := api.pools.FindAdminUserQuota(request.Context(), record.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			api.problems.WriteActionError(writer, request, keysActionError("", http.StatusNotFound, nil))
			return
		}
		api.writeKeysActionError(writer, request, "editKey", err)
		return
	}
	if limitProblem := keysCheckLimitsAgainstQuota(mutation, quota); limitProblem != nil {
		api.problems.WriteActionError(writer, request, limitProblem)
		return
	}

	location, err := api.systemLocation(request.Context())
	if err != nil {
		api.writeKeysActionError(writer, request, "editKey", err)
		return
	}
	patch, patchProblem := keysBuildPatch(mutation, principal, location)
	if patchProblem != nil {
		api.problems.WriteActionError(writer, request, patchProblem)
		return
	}
	if patch == nil {
		// Node 在没有可写字段时只回读一次；响应仍为 { ok: true }。
		writeShellJSON(writer, http.StatusOK, map[string]any{"ok": true})
		return
	}
	updated, found, err := api.pools.UpdateAdminKey(request.Context(), keyID, *patch)
	if err != nil {
		api.writeKeysActionError(writer, request, "editKey", err)
		return
	}
	if !found {
		api.problems.WriteActionError(writer, request, keysActionError("KEY_NOT_FOUND", 0, nil))
		return
	}
	if principal.IsAdmin && mutation.ProviderGroup.Provided {
		if _, syncErr := api.pools.SyncAdminUserProviderGroupFromKeys(
			request.Context(), record.UserID); syncErr != nil {
			api.logger.Warn("admin_keys_provider_group_sync_failed", map[string]any{
				"userId": record.UserID, "error": syncErr.Error(),
			})
		}
	}
	api.invalidateKeyAuth(request.Context(), record.Key)
	if mutation.Limit5hResetMode != nil &&
		usersStringOr(record.Limit5hResetMode, "rolling") != *mutation.Limit5hResetMode {
		// Node：5h 重置模式变化时清该密钥的成本缓存（clearSingleKeyCostCache）。
		api.invalidateKeyCost(request.Context(), keyID)
	}
	api.emitKeyAudit(request, AuditEvent{
		Category:   "key",
		Principal:  principal,
		Action:     "key.update",
		TargetType: "key",
		TargetID:   strconv.FormatInt(keyID, 10),
		TargetName: mutation.Name,
		Before:     keysAuditAfter(record),
		Details:    keysAuditAfter(updated),
		Success:    true,
	})
	writeShellJSON(writer, http.StatusOK, map[string]any{"ok": true})
}

// deleteKey 复刻 deleteKey → removeKey（handlers.ts:164-173、actions/keys.ts:777-895）。
func (api *keysAPI) deleteKey(writer http.ResponseWriter, request *http.Request) {
	if !api.requireKeyWriteSession(writer, request) {
		return
	}
	keyID, ok := keyIDFromParams(request)
	if !ok {
		api.problems.WriteValidationError(writer, request, []InvalidParam{
			keysInvalid("keyId", "invalid_type", "Expected number, received nan"),
		})
		return
	}
	principal, _ := PrincipalFrom(request.Context())
	record, ok := api.loadKeyForWrite(writer, request)
	if !ok {
		return
	}
	if !principal.IsAdmin && principal.UserID != record.UserID {
		api.problems.WriteActionError(writer, request, keysActionError("PERMISSION_DENIED", 0, nil))
		return
	}
	// 只有删除**启用**的密钥才需要「最后一个启用密钥」保护（删禁用的不影响可用密钥数）。
	if record.IsEnabled != nil && *record.IsEnabled {
		active, err := api.pools.CountAdminActiveKeysByUser(request.Context(), record.UserID)
		if err != nil {
			api.writeKeysActionError(writer, request, "removeKey", err)
			return
		}
		if active <= 1 {
			api.problems.WriteActionError(writer, request,
				keysActionError("CANNOT_DELETE_LAST_KEY", 0, nil))
			return
		}
	}
	if !principal.IsAdmin {
		remaining, err := api.remainingProviderGroups(request.Context(), record.UserID, keyID)
		if err != nil {
			api.writeKeysActionError(writer, request, "removeKey", err)
			return
		}
		if remaining == 0 {
			api.problems.WriteActionError(writer, request,
				keysActionError("CANNOT_DELETE_LAST_GROUP_KEY", 0, nil))
			return
		}
	}

	keyValue, found, err := api.pools.DeleteAdminKey(request.Context(), keyID)
	if err != nil {
		api.writeKeysActionError(writer, request, "removeKey", err)
		return
	}
	if !found {
		api.problems.WriteActionError(writer, request, keysActionError("KEY_NOT_FOUND", 0, nil))
		return
	}
	// Node：删除后同步用户分组（分组可能因此变化）。
	if _, syncErr := api.pools.SyncAdminUserProviderGroupFromKeys(
		request.Context(), record.UserID); syncErr != nil {
		api.logger.Warn("admin_keys_provider_group_sync_failed", map[string]any{
			"userId": record.UserID, "error": syncErr.Error(),
		})
	}
	api.invalidateKeyAuth(request.Context(), keyValue)
	api.emitKeyAudit(request, AuditEvent{
		Category:   "key",
		Principal:  principal,
		Action:     "key.delete",
		TargetType: "key",
		TargetID:   strconv.FormatInt(keyID, 10),
		TargetName: record.Name,
		Before:     keysAuditAfter(record),
		Success:    true,
	})
	writer.WriteHeader(http.StatusNoContent)
}

// remainingProviderGroups 复刻 removeKey 的非管理员检查：删掉该密钥后用户还剩几个分组
// （0 表示这会是最后一把分组密钥，禁止删除）。
func (api *keysAPI) remainingProviderGroups(
	ctx context.Context,
	userID int64,
	removingKeyID int64,
) (int, error) {
	keys, err := api.pools.ListAdminUserKeys(ctx, []int64{userID})
	if err != nil {
		return 0, err
	}
	remaining := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if key.ID == removingKeyID {
			continue
		}
		group := key.ProviderGroup
		if group == nil || strings.TrimSpace(*group) == "" {
			fallback := "default"
			group = &fallback
		}
		for _, item := range store.SplitProviderGroups(group) {
			remaining[item] = struct{}{}
		}
	}
	return len(remaining), nil
}

// batchUpdateKeys 复刻 batchUpdateKeys（keys/handlers.ts:281-296、actions/keys.ts:1288-1530）。
func (api *keysAPI) batchUpdateKeys(writer http.ResponseWriter, request *http.Request) {
	body, problems := keysParseBatchUpdate(request)
	if problems != nil {
		api.problems.WriteValidationError(writer, request, problems)
		return
	}
	// 路由是管理档，守卫已保证管理员；Node action 的二次角色判定因此不可达。
	if len(body.KeyIDs) == 0 {
		api.problems.WriteActionError(writer, request, keysActionError("REQUIRED_FIELD", 0, nil))
		return
	}
	if len(body.KeyIDs) > keysBatchMaxSize {
		api.problems.WriteActionError(writer, request, keysActionError("INVALID_FORMAT", 0, nil))
		return
	}
	if body.Updates.Empty() {
		api.problems.WriteActionError(writer, request, keysActionError("EMPTY_UPDATE", 0, nil))
		return
	}
	principal, _ := PrincipalFrom(request.Context())

	updated, affectedUsers, err := api.pools.BatchUpdateAdminKeys(
		request.Context(), body.KeyIDs, body.Updates)
	if err != nil {
		var batchError *store.AdminKeyBatchError
		if errors.As(err, &batchError) {
			switch batchError.Code {
			case "NOT_FOUND":
				api.problems.WriteActionError(writer, request,
					keysActionError("NOT_FOUND", 0, nil))
			case "CANNOT_DISABLE_LAST_KEY":
				api.problems.WriteActionError(writer, request,
					keysActionError("CANNOT_DISABLE_LAST_KEY", 0, nil))
			default:
				api.problems.WriteActionError(writer, request,
					keysActionError("UPDATE_FAILED", 0, nil))
			}
			return
		}
		api.writeKeysActionError(writer, request, "batchUpdateKeys", err)
		return
	}

	if body.Updates.ProviderGroup != nil {
		for _, userID := range affectedUsers {
			if _, syncErr := api.pools.SyncAdminUserProviderGroupFromKeys(
				request.Context(), userID); syncErr != nil {
				api.logger.Warn("admin_keys_provider_group_sync_failed", map[string]any{
					"userId": userID, "error": syncErr.Error(),
				})
			}
		}
	}
	if body.Updates.Limit5hResetMode != nil {
		// Node：改了 5h 重置模式就清这批密钥的认证缓存与成本缓存。
		keys, listErr := api.pools.ListAdminUserKeys(request.Context(), affectedUsers)
		if listErr != nil {
			api.logger.Warn("admin_keys_batch_invalidate_failed", map[string]any{
				"error": listErr.Error(),
			})
		} else {
			for _, key := range keys {
				api.invalidateKeyAuth(request.Context(), key.Key)
				api.invalidateKeyCost(request.Context(), key.ID)
			}
		}
	}
	api.emitKeyAudit(request, AuditEvent{
		Category:   "key",
		Principal:  principal,
		Action:     "key.update",
		TargetType: "key",
		Details: map[string]any{
			"requestedCount": len(body.KeyIDs),
			"updatedCount":   len(updated),
			"updatedIds":     updated,
		},
		Success: true,
	})
	writeShellJSON(writer, http.StatusOK, map[string]any{
		"requestedCount": len(body.KeyIDs),
		"updatedCount":   len(updated),
		"updatedIds":     updated,
	})
}

// keysProviderGroupValue 取三态分组字段的文本（null 或未提供 → 空串，供 normalize 兜底 default）。
func keysProviderGroupValue(value store.Nullable[string]) string {
	if !value.Provided || value.Null {
		return ""
	}
	return value.Value
}

// keysBuildPatch 把校验结果落成列级补丁；nil 表示没有可写字段。
func keysBuildPatch(
	mutation keysMutation,
	principal Principal,
	location *time.Location,
) (*store.AdminKeyPatch, *ActionError) {
	patch := store.AdminKeyPatch{}
	if mutation.HasName {
		name := mutation.Name
		patch.Name = &name
	}
	if mutation.IsEnabled != nil {
		patch.IsEnabled = mutation.IsEnabled
	}
	if mutation.ExpiresAt.Provided {
		if mutation.ExpiresAt.Null {
			patch.ExpiresAt = store.ExplicitNull[time.Time]()
		} else {
			parsed, problem := keysResolveExpiresAt(mutation.ExpiresAt, location)
			if problem != nil {
				return nil, problem
			}
			if parsed == nil {
				patch.ExpiresAt = store.ExplicitNull[time.Time]()
			} else {
				patch.ExpiresAt = store.SomeValue(*parsed)
			}
		}
	}
	if mutation.CanLoginWebUI != nil {
		patch.CanLoginWebUI = mutation.CanLoginWebUI
	}
	if mutation.Limit5hUSD.Provided {
		patch.Limit5hUSD = mutation.Limit5hUSD
	}
	if mutation.Limit5hResetMode != nil {
		patch.Limit5hResetMode = mutation.Limit5hResetMode
	}
	if mutation.LimitDailyUSD.Provided {
		patch.LimitDailyUSD = mutation.LimitDailyUSD
	}
	if mutation.DailyResetMode != nil {
		patch.DailyResetMode = mutation.DailyResetMode
	}
	if mutation.DailyResetTime != nil {
		patch.DailyResetTime = mutation.DailyResetTime
	}
	if mutation.LimitWeeklyUSD.Provided {
		patch.LimitWeeklyUSD = mutation.LimitWeeklyUSD
	}
	if mutation.LimitMonthlyUSD.Provided {
		patch.LimitMonthlyUSD = mutation.LimitMonthlyUSD
	}
	if mutation.LimitTotalUSD.Provided {
		patch.LimitTotalUSD = mutation.LimitTotalUSD
	}
	if mutation.LimitConcurrentSessions.Provided {
		patch.LimitConcurrentSessions = mutation.LimitConcurrentSessions
	}
	// 分组只有管理员能改（非管理员改到不同值已在调用方拦下；值未变时按 Node 允许继续）。
	if principal.IsAdmin && mutation.ProviderGroup.Provided {
		if mutation.ProviderGroup.Null {
			patch.ProviderGroup = store.ExplicitNull[string]()
		} else {
			normalized := usersNormalizeProviderGroup(mutation.ProviderGroup.Value)
			patch.ProviderGroup = store.SomeValue(normalized)
		}
	}
	if mutation.CacheTTLPreference != nil {
		patch.CacheTTLPreference = store.SomeValue(*mutation.CacheTTLPreference)
	}
	if patch == (store.AdminKeyPatch{}) {
		return nil, nil
	}
	return &patch, nil
}

// keysCheckLimitsAgainstQuota 复刻 editKey 的限额校验（actions/keys.ts:526-620）：
// 与 addKey 同一组比对，但失败**不带** errorCode，故状态码走 key 码表的默认 400。
func keysCheckLimitsAgainstQuota(
	mutation keysMutation,
	quota *store.AdminUserQuota,
) *ActionError {
	compare := func(value store.Nullable[string], userValue json.Number) bool {
		if !value.Provided || value.Null {
			return false
		}
		keyValue := keysFloatOrZero(value)
		if keyValue <= 0 {
			return false
		}
		parsed := usersNumber(userValue)
		return parsed != nil && *parsed > 0 && keyValue > *parsed
	}
	if compare(mutation.Limit5hUSD, quota.Limit5hUSD) ||
		compare(mutation.LimitDailyUSD, quota.DailyLimitUSD) ||
		compare(mutation.LimitWeeklyUSD, quota.LimitWeeklyUSD) ||
		compare(mutation.LimitMonthlyUSD, quota.LimitMonthlyUSD) ||
		compare(mutation.LimitTotalUSD, quota.LimitTotalUSD) {
		return keysActionError("", 0, nil)
	}
	if mutation.LimitConcurrentSessions.Provided && !mutation.LimitConcurrentSessions.Null {
		keyValue := mutation.LimitConcurrentSessions.Value
		if keyValue > 0 && quota.LimitConcurrentSessions != nil &&
			*quota.LimitConcurrentSessions > 0 && keyValue > *quota.LimitConcurrentSessions {
			return keysActionError("", 0, nil)
		}
	}
	return nil
}

// loadKeyForWrite 取路径里的密钥并处理「不存在」；返回 false 表示已作答。
//
// Node 在这类分支上的文案是「密钥不存在」且**无** errorCode ⇒ 404 + key.not_found
// （按裁决不复刻中文子串判定，故在此显式给 404）。
func (api *keysAPI) loadKeyForWrite(
	writer http.ResponseWriter,
	request *http.Request,
) (*store.AdminKeyRecord, bool) {
	keyID, ok := keyIDFromParams(request)
	if !ok {
		api.problems.WriteValidationError(writer, request, []InvalidParam{
			keysInvalid("keyId", "invalid_type", "Expected number, received nan"),
		})
		return nil, false
	}
	record, err := api.pools.FindAdminKeyByID(request.Context(), keyID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			api.problems.WriteActionError(writer, request,
				keysActionError("KEY_NOT_FOUND", 0, nil))
			return nil, false
		}
		api.writeKeysActionError(writer, request, "findKeyById", err)
		return nil, false
	}
	return record, true
}

// invalidateKeyAuth 使某把密钥的认证缓存失效（Node 的 invalidateCachedKey）。
func (api *keysAPI) invalidateKeyAuth(ctx context.Context, keyValue string) {
	if api.invalid == nil || keyValue == "" {
		return
	}
	api.invalid.InvalidateKeyAuth(ctx, keyValue)
}

// invalidateKeyCost 使某个密钥的成本缓存失效（Node 的 clearSingleKeyCostCache）。
func (api *keysAPI) invalidateKeyCost(ctx context.Context, keyID int64) {
	if api.invalid == nil {
		return
	}
	api.invalid.InvalidateKeyCost(ctx, keyID)
}
