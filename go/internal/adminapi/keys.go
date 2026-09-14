package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是管理面 keys 资源（A1-2）的路由与「限额快捷编辑」三条处理器；资源的列表与写路径
// （create/update/delete/enable/renew/batchUpdate）在 keys_handlers.go，请求校验在 keys_schema.go。
//
// **已注册 14 条**：
//	GET    /users/{userId}/keys              管理档：列表（include=statistics 走统计）
//	POST   /users/{userId}/keys              管理档：新建密钥（管理员指定属主）
//	POST   /users:self/keys                  读档：自助新建（目标属主取会话身份）
//	POST   /keys/{keyId}:enable              读档：启用/禁用（含「最后一个启用密钥」保护）
//	POST   /keys/{keyId}:renew               读档：续期（可顺带启用）
//	GET    /keys/{keyId}:reveal              读档：管理员或属主可查看明文密钥（含审计）
//	PATCH  /keys/{keyId}                     读档：编辑（含写会话门与限额校验）
//	DELETE /keys/{keyId}                     读档：删除（含最后一个启用密钥/分组保护）
//	POST   /keys/{keyId}/limits:reset        管理档：只重置限额基准 cost_reset_at
//	PATCH  /keys/{keyId}/limits/{field}      管理档：只改一个限额字段（含「不得超过用户限额」校验）
//	POST   /keys:batchUpdate                 管理档：批量改 8 个字段（事务 + 禁用后复核）
//	GET    /keys/{keyId}                     管理档：读档（正文 = {id, limitUsage}）
//	GET    /keys/{keyId}/limit-usage         读档：五周期用量 + 并发会话（含 Redis 运行态）
//	GET    /keys/{keyId}/quota               管理档：配额弹窗用的 items + currencyCode
//
// 最后三条读档端点比其余多一层依赖：响应的 `concurrentSessions.current` 取自 Node 的
// `SessionTracker.getKeySessionCount`（**Redis 计数**），5h 固定模式的累计值取自 Redis 运行态窗口。
// 两者经 `Deps.SessionCounts` / `Deps.Fixed5hWindows` 注入，**任一缺失就不注册这三条**——否则只能
// 硬造恒 0 的 current，那是静默的错数，比不实现更糟（请求原样回退 Node，与其余未注册路径同语义）。
//
// 路径里的 `{keyId:[0-9]+}` 是**有意**的：Node 用 zod 的 coerce 校验 keyId，非数字 id 得到
// zod 形状的 400；Go 这里让非数字 id 压根不命中路由（回退 Node），于是那条请求由 Node 亲自作答，
// 形状天然一致。`{field}` 的枚举同理会回退 Node，不需要在 Go 侧复刻 zod 的 invalidParams 信封。

// keyLimitFields 是限额快捷编辑允许的字段，逐字取自 PatchKeyLimitFieldSchema
// （src/lib/api/v1/schemas/keys.ts:92-99）。它同时写进路由正则：不在枚举内的 field 回退 Node。
const keyLimitFields = "limit5hUsd|limitDailyUsd|limitWeeklyUsd|limitMonthlyUsd|limitTotalUsd|" +
	"limitConcurrentSessions"

// RegisterKeysRoutes 注册 keys 资源的全部 14 条路由（其中三条需 Redis 运行态依赖，缺失时不注册）。
//
// 共享依赖（Guard/Problems/Audit/Invalidator/Store）任一缺失时的行为：
//   - Guard 缺失：Router.Add 自身拒注册（fail-closed，见 router.go），这里不需重复判断；
//   - Store 缺失：本函数直接不注册并记 warn——少了库就没法作答，注册等于制造 500。
func RegisterKeysRoutes(router *Router, deps Deps) {
	api := newKeysAPI(deps)
	if api == nil {
		logKeysUnwired(deps, "store_missing", "/keys*")
		return
	}
	problems := api.problems

	// 读档写路由的处理量与上表同序：列表/创建/启用/续期/编辑/删除/批量。
	writeRoutes := []Route{
		{
			Method:      http.MethodGet,
			Path:        "/users/{userId:[0-9]+}/keys",
			Access:      AccessAdmin,
			Module:      "keys",
			OperationID: "listUserKeys",
			Handler:     http.HandlerFunc(api.listUserKeys),
		},
		{
			Method:      http.MethodPost,
			Path:        "/users/{userId:[0-9]+}/keys",
			Access:      AccessAdmin,
			Module:      "keys",
			OperationID: "createUserKey",
			Handler:     http.HandlerFunc(api.createUserKey),
		},
		{
			Method:      http.MethodPost,
			Path:        "/users:self/keys",
			Access:      AccessRead,
			Module:      "keys",
			OperationID: "createSelfKey",
			Handler:     http.HandlerFunc(api.createSelfKey),
		},
		{
			Method:      http.MethodPost,
			Path:        "/keys/{keyId:[0-9]+}:enable",
			Access:      AccessRead,
			Module:      "keys",
			OperationID: "enableKey",
			Handler:     http.HandlerFunc(api.enableKey),
		},
		{
			Method:      http.MethodPost,
			Path:        "/keys/{keyId:[0-9]+}:renew",
			Access:      AccessRead,
			Module:      "keys",
			OperationID: "renewKey",
			Handler:     http.HandlerFunc(api.renewKey),
		},
		{
			Method:      http.MethodPatch,
			Path:        "/keys/{keyId:[0-9]+}",
			Access:      AccessRead,
			Module:      "keys",
			OperationID: "updateKey",
			Handler:     http.HandlerFunc(api.updateKey),
		},
		{
			Method:      http.MethodDelete,
			Path:        "/keys/{keyId:[0-9]+}",
			Access:      AccessRead,
			Module:      "keys",
			OperationID: "deleteKey",
			Handler:     http.HandlerFunc(api.deleteKey),
		},
		{
			Method:      http.MethodPost,
			Path:        "/keys:batchUpdate",
			Access:      AccessAdmin,
			Module:      "keys",
			OperationID: "batchUpdateKeys",
			Handler:     http.HandlerFunc(api.batchUpdateKeys),
		},
	}
	for _, route := range writeRoutes {
		router.Add(route)
	}

	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/keys/{keyId:[0-9]+}:reveal",
		Access:      AccessRead,
		Module:      "keys",
		OperationID: "revealKey",
		Handler:     http.HandlerFunc(handleKeyReveal(deps, problems)),
	})
	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/keys/{keyId:[0-9]+}/limits:reset",
		Access:      AccessAdmin,
		Module:      "keys",
		OperationID: "resetKeyLimits",
		Handler:     http.HandlerFunc(handleKeyLimitsReset(deps, problems)),
	})
	router.Add(Route{
		Method:      http.MethodPatch,
		Path:        "/keys/{keyId:[0-9]+}/limits/{field:" + keyLimitFields + "}",
		Access:      AccessAdmin,
		Module:      "keys",
		OperationID: "patchKeyLimit",
		Handler:     http.HandlerFunc(handleKeyLimitPatch(deps, problems)),
	})

	registerKeysUsageRoutes(router, api)
}

// registerKeysUsageRoutes 注册三条读档路由；两个 Redis 运行态依赖缺任一就不注册（回退 Node）。
func registerKeysUsageRoutes(router *Router, api *keysAPI) {
	if api.sessions == nil || api.fixed5h == nil {
		logKeysUnwired(api.deps, "runtime_deps_missing", "/keys/{keyId}[/limit-usage|/quota]")
		return
	}
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/keys/{keyId:[0-9]+}",
		Access:      AccessAdmin,
		Module:      "keys",
		OperationID: "getKey",
		Handler:     http.HandlerFunc(api.getKey),
	})
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/keys/{keyId:[0-9]+}/limit-usage",
		Access:      AccessRead,
		Module:      "keys",
		OperationID: "getKeyLimitUsage",
		Handler:     http.HandlerFunc(api.getKeyLimitUsage),
	})
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/keys/{keyId:[0-9]+}/quota",
		Access:      AccessAdmin,
		Module:      "keys",
		OperationID: "getKeyQuotaUsage",
		Handler:     http.HandlerFunc(api.getKeyQuotaUsage),
	})
}

// logKeysUnwired 记录「整个资源未装配」的启动期告警（与 shell.go 的 csrf 未装配同风格）。
func logKeysUnwired(deps Deps, reason, scope string) {
	if deps.Logger != nil {
		deps.Logger.Warn("admin_keys_unwired", map[string]any{
			"reason": reason,
			"scope":  scope,
			"action": "routes_not_registered",
		})
	}
}

// handleKeyReveal 复刻 getUnmaskedKey + revealKey（src/actions/keys.ts:948-1002、
// keys/handlers.ts:213-225）：管理员或属主可读明文；每次调用写一条审计（成功与失败各一条）。
func handleKeyReveal(deps Deps, problems ProblemWriter) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		ctx := request.Context()
		principal, ok := PrincipalFrom(ctx)
		if !ok {
			// 守卫已放行却拿不到身份：说明守卫被绕过，按 Node 的「未登录」档作答（read 档下
			// 这条分支在 REST 路径上不可达，留着是为了不让「无身份」被当成合法调用方）。
			problems.WriteActionError(writer, request, NewActionError("key", "", http.StatusBadRequest, nil))
			return
		}
		keyID, ok := keyIDFromParams(request)
		if !ok {
			problems.WriteProblem(writer, request, http.StatusBadRequest, "request.validation_failed",
				"One or more fields are invalid.")
			return
		}

		record, err := deps.Store.FindAdminKeyByID(ctx, keyID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				emitKeyAudit(deps, request, AuditEvent{
					Category:   "key",
					Principal:  principal,
					Action:     "key.key_reveal",
					TargetType: "key",
					TargetID:   strconv.FormatInt(keyID, 10),
					Success:    false,
					// Node 的失败分支固定写 KEY_REVEAL_FAILED（keys.ts:995）。
					ErrorMessage: "KEY_REVEAL_FAILED",
				})
				problems.WriteActionError(writer, request,
					NewActionError("key", "KEY_NOT_FOUND", http.StatusNotFound, nil))
				return
			}
			logKeyFailure(deps, "key_reveal_failed", keyID, err)
			problems.WriteActionError(writer, request,
				NewActionError("key", "", http.StatusBadRequest, err))
			return
		}

		if !principal.IsAdmin && principal.UserID != record.UserID {
			// Node 在此没有 errorCode，只有「无权限执行此操作」⇒ 403 + key.action_failed。
			problems.WriteActionError(writer, request, NewActionError("key", "", http.StatusForbidden, nil))
			return
		}

		if deps.Logger != nil {
			// 与 Node 的 logger.info("User viewed key", ...) 同字段（keys.ts:975-981）；
			// **不记密钥内容**。
			deps.Logger.Info("admin_key_viewed", map[string]any{
				"viewerId":    principal.UserID,
				"viewerAdmin": principal.IsAdmin,
				"keyId":       keyID,
				"keyName":     record.Name,
				"keyOwnerId":  record.UserID,
			})
		}
		emitKeyAudit(deps, request, AuditEvent{
			Category:   "key",
			Principal:  principal,
			Action:     "key.key_reveal",
			TargetType: "key",
			TargetID:   strconv.FormatInt(record.ID, 10),
			TargetName: record.Name,
			Details: map[string]any{
				"id":     record.ID,
				"name":   record.Name,
				"userId": record.UserID,
			},
			Success: true,
		})

		writeShellJSON(writer, http.StatusOK, keyRevealResponse{Key: record.Key})
	}
}

// keyRevealResponse 逐字对应 KeyRevealResponseSchema（schemas/keys.ts:150-157）。
type keyRevealResponse struct {
	Key string `json:"key"`
}

// handleKeyLimitsReset 复刻 resetKeyLimitsOnly（src/actions/keys.ts:1149-1212）。
//
// 顺序与 Node 一致：先取密钥（不存在即 404），再写 cost_reset_at，命中行的同时还回传的明文密钥
// 清 Redis 成本缓存；最后 204 无正文（noContentResponse）。
func handleKeyLimitsReset(deps Deps, problems ProblemWriter) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		ctx := request.Context()
		keyID, ok := keyIDFromParams(request)
		if !ok {
			problems.WriteProblem(writer, request, http.StatusBadRequest, "request.validation_failed",
				"One or more fields are invalid.")
			return
		}
		// Node 的这条 action 自带「非管理员即 PERMISSION_DENIED」；路由是管理档，守卫已经拦过一遍。
		// 这里不再重复判角色：重复判定只会在守卫语义变化时给出两套口径。
		record, err := deps.Store.FindAdminKeyByID(ctx, keyID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				problems.WriteActionError(writer, request,
					NewActionError("key", "KEY_NOT_FOUND", http.StatusNotFound, nil))
				return
			}
			logKeyFailure(deps, "key_limits_reset_failed", keyID, err)
			problems.WriteActionError(writer, request,
				NewActionError("key", "", http.StatusBadRequest, err))
			return
		}

		// 时间取进程时钟：Deps 无注入时钟（A0-1 的冻结面如此），与 Node 的 new Date() 同步语义一致。
		keyValue, found, err := deps.Store.ResetAdminKeyCostResetAt(ctx, keyID, time.Now())
		if err != nil {
			logKeyFailure(deps, "key_limits_reset_failed", keyID, err)
			problems.WriteActionError(writer, request,
				NewActionError("key", "", http.StatusBadRequest, err))
			return
		}
		if !found {
			// 与 Node 的 `if (!updated)` 分支同形：写不到行按「密钥不存在」作答。
			problems.WriteActionError(writer, request,
				NewActionError("key", "KEY_NOT_FOUND", http.StatusNotFound, nil))
			return
		}
		if deps.Invalidator != nil {
			deps.Invalidator.InvalidateKeyCost(ctx, keyID)
		}
		if deps.Logger != nil {
			deps.Logger.Info("admin_key_limits_reset", map[string]any{
				"keyId":  keyID,
				"userId": record.UserID,
				// 只写长度与是否非空，明文密钥不入日志（Node 侧此处也未记密钥）。
				"keyPresent": keyValue != "",
			})
		}
		writer.WriteHeader(http.StatusNoContent)
	}
}

// handleKeyLimitPatch 复刻 patchKeyLimit（src/actions/keys.ts:1605-1723）。
//
// 校验顺序与 Node 一致：字段取值合法性 → 密钥存在 → 属主/管理员 → 用户存在 → 「不得超过用户
// 限额」逐字段比对 → 写库 → 清认证缓存 → 审计。
func handleKeyLimitPatch(deps Deps, problems ProblemWriter) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		ctx := request.Context()
		principal, _ := PrincipalFrom(ctx)
		params := ParamsFrom(ctx)
		keyID, ok := keyIDFromParams(request)
		if !ok {
			problems.WriteProblem(writer, request, http.StatusBadRequest, "request.validation_failed",
				"One or more fields are invalid.")
			return
		}
		field := params["field"]

		value, err := parseKeyLimitValue(field, request)
		if err != nil {
			problems.WriteProblem(writer, request, http.StatusBadRequest, "request.validation_failed",
				"One or more fields are invalid.")
			return
		}

		record, err := deps.Store.FindAdminKeyByID(ctx, keyID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				problems.WriteActionError(writer, request,
					NewActionError("key", "KEY_NOT_FOUND", http.StatusNotFound, nil))
				return
			}
			logKeyFailure(deps, "key_limit_patch_failed", keyID, err)
			problems.WriteActionError(writer, request, NewActionError("key", "", http.StatusBadRequest, err))
			return
		}
		if !principal.IsAdmin && principal.UserID != record.UserID {
			problems.WriteActionError(writer, request, NewActionError("key", "", http.StatusForbidden, nil))
			return
		}

		quota, err := deps.Store.FindAdminUserQuota(ctx, record.UserID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// Node 的 `if (!user) return { ok:false, error:"用户不存在" }`：详情含「不存在」
				// ⇒ 404，且没有 errorCode ⇒ 兜底 key.not_found，故这里 Code 留空。
				problems.WriteActionError(writer, request,
					NewActionError("key", "", http.StatusNotFound, nil))
				return
			}
			logKeyFailure(deps, "key_limit_patch_failed", keyID, err)
			problems.WriteActionError(writer, request, NewActionError("key", "", http.StatusBadRequest, err))
			return
		}
		if exceededKeyLimit(field, value, quota) {
			// Node 这三个分支只带 error（无 errorCode）⇒ 400 + key.action_failed；
			// 文案走 publicActionErrorDetail（响应里不含 i18n 原文）。
			problems.WriteActionError(writer, request, NewActionError("key", "", http.StatusBadRequest, nil))
			return
		}

		patched, err := writeKeyLimit(ctx, deps, keyID, field, value)
		if err != nil {
			logKeyFailure(deps, "key_limit_patch_failed", keyID, err)
			problems.WriteActionError(writer, request, NewActionError("key", "", http.StatusBadRequest, err))
			return
		}
		if !patched {
			problems.WriteActionError(writer, request,
				NewActionError("key", "KEY_NOT_FOUND", http.StatusNotFound, nil))
			return
		}

		if deps.Invalidator != nil {
			// Node 调 invalidateCachedKey(key.key)：认证缓存以密钥明文为键。
			deps.Invalidator.InvalidateKeyAuth(ctx, record.Key)
		}
		emitKeyAudit(deps, request, AuditEvent{
			Category:   "key",
			Principal:  principal,
			Action:     "key.update",
			TargetType: "key",
			TargetID:   strconv.FormatInt(keyID, 10),
			TargetName: record.Name,
			Details:    map[string]any{field: value.auditValue()},
			Success:    true,
		})
		writeShellJSON(writer, http.StatusOK, keyActionOKResponse{OK: true})
	}
}

// keyActionOKResponse 对应 Node 的 `result.data ?? { ok: true }`（keys/handlers.ts:338-341）。
type keyActionOKResponse struct {
	OK bool `json:"ok"`
}

// keyLimitValue 是一次限额取值：null 与数值必须区分（null 表示清空限额）。
type keyLimitValue struct {
	// null 为 true 表示请求显式传了 null。
	null bool
	// literal 是请求里的 JSON 数字原文：写库时按原文传给 numeric 列，避免 Go 的 float64 改变
	// numeric(10,2) 的舍入结果。
	literal string
	// number 供「不得超过用户限额」的比较使用（数值量级远小于 float64 的精度边界）。
	number float64
	// concurrent 仅在 limitConcurrentSessions 上有意义。
	concurrent int32
}

// auditValue 返回写进审计 after 的值（null 或数值）。
func (v keyLimitValue) auditValue() any {
	if v.null {
		return nil
	}
	if v.literal != "" {
		return json.Number(v.literal)
	}
	return v.concurrent
}

// parseKeyLimitValue 解析并校验 {"value": <number|null>}。
//
// 用 DisallowUnknownFields 复刻 PatchKeyLimitSchema 的 .strict()：多传字段在 Node 是 400。
// 数字用 json.Number 保留原文（见 keyLimitValue.literal 的说明）。
func parseKeyLimitValue(field string, request *http.Request) (keyLimitValue, error) {
	var body struct {
		Value json.RawMessage `json:"value"`
	}
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&body); err != nil {
		return keyLimitValue{}, err
	}
	if len(body.Value) == 0 {
		return keyLimitValue{}, errors.New("adminapi: 缺少 value 字段")
	}
	raw := strings.TrimSpace(string(body.Value))
	if raw == "null" {
		if field == "limitConcurrentSessions" {
			// Node：limitConcurrentSessions 必须是非空整数（keys.ts:1626-1634）。
			return keyLimitValue{}, errors.New("adminapi: 并发会话上限不接受 null")
		}
		return keyLimitValue{null: true}, nil
	}

	number, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
		return keyLimitValue{}, fmt.Errorf("adminapi: value 不是数字: %s", raw)
	}
	if field == "limitConcurrentSessions" {
		if strings.ContainsAny(raw, ".eE") {
			return keyLimitValue{}, errors.New("adminapi: 并发会话上限必须是整数")
		}
		if number < 0 || number > 1000 {
			return keyLimitValue{}, errors.New("adminapi: 并发会话上限超出 0..1000")
		}
		return keyLimitValue{number: number, concurrent: int32(number)}, nil
	}
	if number < 0 {
		return keyLimitValue{}, errors.New("adminapi: 限额不得为负")
	}
	return keyLimitValue{literal: raw, number: number}, nil
}

// exceededKeyLimit 复刻 checkExceed 的六路比对（keys.ts:1643-1694）。
//
// 注意 limitDailyUsd 比的是 users.daily_limit_usd（Node 的 User.dailyQuota，见 user.ts:74）——
// 这一处抄错会让校验恒不触发。
func exceededKeyLimit(field string, value keyLimitValue, quota *store.AdminUserQuota) bool {
	if quota == nil {
		return false
	}
	if field == "limitConcurrentSessions" {
		if value.null {
			return false
		}
		userValue, ok := int32OrNil(quota.LimitConcurrentSessions)
		return exceeds(float64(value.concurrent), userValue, ok)
	}
	userValue, ok := floatOrNil(userQuotaColumn(field, quota))
	return exceeds(value.number, userValue, ok)
}

// userQuotaColumn 取该字段要比的用户列。
func userQuotaColumn(field string, quota *store.AdminUserQuota) json.Number {
	switch field {
	case "limit5hUsd":
		return quota.Limit5hUSD
	case "limitDailyUsd":
		return quota.DailyLimitUSD
	case "limitWeeklyUsd":
		return quota.LimitWeeklyUSD
	case "limitMonthlyUsd":
		return quota.LimitMonthlyUSD
	case "limitTotalUsd":
		return quota.LimitTotalUSD
	default:
		return ""
	}
}

// exceeds 复刻 `keyVal > 0 && userVal != null && userVal > 0 && keyVal > userVal`。
func exceeds(keyValue, userValue float64, userPresent bool) bool {
	if keyValue <= 0 || !userPresent || userValue <= 0 {
		return false
	}
	return keyValue > userValue
}

// writeKeyLimit 按字段写库。
func writeKeyLimit(
	ctx context.Context,
	deps Deps,
	keyID int64,
	field string,
	value keyLimitValue,
) (bool, error) {
	if field == "limitConcurrentSessions" {
		return deps.Store.SetAdminKeyConcurrentLimit(ctx, keyID, value.concurrent)
	}
	column, err := moneyLimitColumn(field)
	if err != nil {
		return false, err
	}
	if value.null {
		return deps.Store.SetAdminKeyMoneyLimit(ctx, keyID, column, nil)
	}
	literal := value.literal
	return deps.Store.SetAdminKeyMoneyLimit(ctx, keyID, column, &literal)
}

// moneyLimitColumn 把 camelCase 字段映射成 store 的列白名单（keys.ts:1699-1706）。
func moneyLimitColumn(field string) (store.AdminKeyMoneyLimitColumn, error) {
	switch field {
	case "limit5hUsd":
		return store.AdminKeyLimit5hUSD, nil
	case "limitDailyUsd":
		return store.AdminKeyLimitDailyUSD, nil
	case "limitWeeklyUsd":
		return store.AdminKeyLimitWeeklyUSD, nil
	case "limitMonthlyUsd":
		return store.AdminKeyLimitMonthlyUSD, nil
	case "limitTotalUsd":
		return store.AdminKeyLimitTotalUSD, nil
	default:
		return "", fmt.Errorf("adminapi: 未知的限额字段 %q", field)
	}
}

// floatOrNil 把 json.Number 转成可选 float64（空串即 SQL NULL）。
func floatOrNil(value json.Number) (float64, bool) {
	if value == "" {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(value.String(), 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

// int32OrNil 把可空整数转成可选 float64（比较用）。
func int32OrNil(value *int32) (float64, bool) {
	if value == nil {
		return 0, false
	}
	return float64(*value), true
}

// keyIDFromParams 取路径里的 keyId。
//
// 走到这里时路由正则已经保证它是纯数字；仍做一次解析并处理失败，是为了不让「正则被改宽」的
// 改动直接变成越界或 0 号密钥。
func keyIDFromParams(request *http.Request) (int64, bool) {
	params := ParamsFrom(request.Context())
	raw, ok := params["keyId"]
	if !ok {
		return 0, false
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, false
	}
	return parsed, true
}

// emitKeyAudit 写一条 keys 资源审计。
//
// Success 显式传值（AuditEvent 的零值是「失败审计」）；IP 与 User-Agent 在此补齐——审计事件
// 从上下文里拿不到 HTTP 请求，而 Node 是由 action 适配器把 IP/UA 塞进请求上下文的
// （src/lib/audit/emit.ts:28-32 的 resolveRequestContext）。
//
// IP 走 auditClientIP（audit_ip.go 的统一提取链），不再用本文件旧有的「XFF 首跳」近似。
func emitKeyAudit(deps Deps, request *http.Request, event AuditEvent) {
	if deps.Audit == nil {
		return
	}
	if event.IP == "" {
		event.IP = auditClientIP(request.Context(), deps.Store, request)
	}
	if event.UserAgent == "" {
		event.UserAgent = request.Header.Get("User-Agent")
	}
	deps.Audit.Emit(request.Context(), event)
}

// logKeyFailure 记录一次写路径失败（不含密钥值）。
func logKeyFailure(deps Deps, event string, keyID int64, err error) {
	if deps.Logger == nil {
		return
	}
	deps.Logger.Error(event, map[string]any{"keyId": keyID, "error": err.Error()})
}
