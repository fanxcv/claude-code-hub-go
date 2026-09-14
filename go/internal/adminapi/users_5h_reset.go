package adminapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 「仅重置 5H 限额」的 REST 入口。
//
// 为什么单开一条：UI 上该按钮原先是 server action
// （`_components/user/actions/reset-user-5h-limit.ts`），静态导出后没有服务端动作可用；
// 而它与「重置限额」（`/users/{id}/limits:reset`）**语义不同**——前者只推进
// `limit_5h_cost_reset_at`（rolling 下单调不回退），后者同时推进 `cost_reset_at` 与
// `limit_5h_cost_reset_at`。故不合并（合并等于把后者悄悄扩大到全量语义）。
// 背景与取舍见 “ §4。
// 错误码直接沿用 Node `ERROR_CODES` 里的语义名：客户端把 code 当 `errors` 命名空间的键用
// （`getApiErrorMessageKey` 对未登记码原样透传），所以这样返回能让前端拿到**与 Node 逐字相同**
// 的本地化文案，而不是碰运气猜一个中文/英文 detail。
//
// 有意**不**给出 `USER_5H_FIXED_RESET_CLEANUP_FAILED`（Node 在 fixed 清理失败时用它）：
// Go 侧的清理走已装配的 `Invalidator`，失败不返错，故该分支不可达；等它在交接项落地后再补。
const (
	users5hErrorNotConfigured = "USER_5H_LIMIT_NOT_CONFIGURED"
	users5hErrorRequiresRedis = "USER_5H_FIXED_RESET_REQUIRES_REDIS"
)

const (
	users5hResetModeRolling = "rolling"
	users5hResetModeFixed   = "fixed"
	users5hAuditAction      = "user.reset_5h_limit"
)

// handleResetUser5hLimit 复刻 Node 的 `resetUser5hLimitOnly`（逐分支对齐）：
//
//	admin 检查失败            → PERMISSION_DENIED
//	用户不存在 / 已软删        → USER_NOT_FOUND
//	未配置 5h 限额（空或 <= 0）→ USER_5H_LIMIT_NOT_CONFIGURED
//	resetMode                 → 取用户行 `limit5hResetMode`，缺省 rolling
//	fixed 且清缓存能力不可用   → USER_5H_FIXED_RESET_REQUIRES_REDIS
//	rolling                   → 推进 limit_5h_cost_reset_at（单调）；fixed 不写库（与 Node 一致）
//	成功响应                   → {resetMode, cleanupRequired?}，与 Node action 的 data 形状一致
//
// 与 Node 的一致与差异：
//   - Node 在清理失败时对 rolling 返回 `cleanupRequired:true`（前端提示补偿），对 fixed 报
//     `USER_5H_FIXED_RESET_CLEANUP_FAILED`。Go 侧清理走已装配的 `Invalidator`，其语义是
//     fire-and-forget（失败只记日志，不回错），因此**这两条失败分支在当前装配下不可观测**：
//     rolling 恒视为清理成功、fixed 的清理失败码保留但不会触发。要恢复可观测需要在
//     `Invalidator` 上开一个「带错误返回」的清理方法并接线到 `cmd/cchd`（本 lane 不动装配，
//     列为交接项）。
//   - Node 的 5h 清理只 DEL 两个键（`user:{id}:cost_5h_{mode}` 与租约键）。Go 侧复用
//     用户级成本缓存失效（`user:{id}:cost_*` 等），是**超集**；租约键（`lease:user:*:5h:*`）
//     在 Go 数据面根本不存在（`internal/limit/keys.go` 无租约键），只有退役中的 Node 会读，
//     故不单独处理。
func (api *usersAPI) handleResetUser5hLimit(writer http.ResponseWriter, request *http.Request) {
	id, ok := usersPathID(request)
	if !ok {
		api.writeValidationProblem(writer, request, []usersInvalidParam{{
			Path: []any{"id"}, Code: "invalid_type", Message: "Expected number, received nan",
		}})
		return
	}

	principal, _ := PrincipalFrom(request.Context())
	if !principal.IsAdmin {
		api.writeUsersActionError(writer, request, usersActionError("PERMISSION_DENIED", nil))
		return
	}

	row, err := api.pools.FindAdminUserByID(request.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			api.writeUsersActionError(writer, request, usersActionError("USER_NOT_FOUND", nil))
			return
		}
		api.writeUsersActionError(writer, request, storeFailure("resetUser5hLimitOnly", err))
		return
	}

	// Node: `if (!user.limit5hUsd || user.limit5hUsd <= 0)` —— 空、0、负值都算未配置。
	limit5h := usersNumber(row.Limit5hUSD)
	if limit5h == nil || *limit5h <= 0 {
		api.writeUsersActionError(writer, request, usersActionError(users5hErrorNotConfigured, nil))
		return
	}

	resetMode := users5hResetModeRolling
	if row.Limit5hResetMode != nil {
		if trimmed := strings.TrimSpace(*row.Limit5hResetMode); trimmed != "" {
			resetMode = trimmed
		}
	}

	// Node 用 `getRedisClient()?.status === "ready"` 判「能不能清运行态」；Go 侧的等价物是
	// 「缓存失效通道是否已装配」。
	cleanupAvailable := api.invalid != nil
	if resetMode == users5hResetModeFixed && !cleanupAvailable {
		api.writeUsersActionError(writer, request, usersActionError(users5hErrorRequiresRedis, nil))
		return
	}

	if resetMode == users5hResetModeRolling {
		updated, updateErr := api.pools.ResetAdminUser5hCostMarker(request.Context(), id, api.now())
		if updateErr != nil {
			api.writeUsersActionError(writer, request, storeFailure("resetUser5hLimitOnly", updateErr))
			return
		}
		if !updated {
			// 行在两次读之间被软删或删除：与 Node 的 `!updated → USER_NOT_FOUND` 同判。
			api.writeUsersActionError(writer, request, usersActionError("USER_NOT_FOUND", nil))
			return
		}
	}

	if cleanupAvailable {
		api.invalidateUserCost(request, id)
		api.invalidateUser(request, id, nil)
	}

	auditAfter := make(map[string]any, 2)
	body := map[string]any{"resetMode": resetMode}
	if !cleanupAvailable {
		// 与 Node 的 rolling 补偿分支同形：清理能力不可用时如实告知前端需要补偿。
		auditAfter["cleanupRequired"] = true
		body["cleanupRequired"] = true
	}
	if refreshed, refreshErr := api.pools.FindAdminUserByID(request.Context(), id); refreshErr == nil {
		auditAfter["user"] = refreshed
	}
	api.emitUserAudit(
		request, principal, users5hAuditAction, id, usersNameOf(row), row, auditAfter, true, "",
	)

	// 响应形状对齐 Node action 的 data：`{resetMode}` 或 `{resetMode, cleanupRequired:true}`。
	writeUsersJSON(writer, http.StatusOK, body)
}
