package adminapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/limit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件实现管理面 users 资源的路由与处理器。
// 与 Node 的一一对应关系（handler 文件为 src/app/api/v1/resources/users/handlers.ts，动作层为
// src/actions/users.ts，落库层为 src/repository/user.ts）：
//
//	GET    /users                         listUsers            → getUsersBatchCore
//	POST   /users                         createUser           → addUser / createUserOnly
//	GET    /users:self                    listCurrentUser      → getCurrentUserDisplay
//	GET    /users/tags                    getUserTags          → getAllUserTags
//	GET    /users/key-groups              getUserKeyGroups     → getAllUserKeyGroups
//	GET    /users:filter-search           filterSearchUsers    → searchUsersForFilter
//	GET    /users:search                  searchUsers          → searchUsers
//	POST   /users:usageBatch              getUsersUsage        → getUsersUsageBatch
//	POST   /users:batchUpdate             batchUpdateUsers     → batchUpdateUsers
//	GET    /users/{id}                    getUser              → getUserById
//	PATCH  /users/{id}                    updateUser           → editUser
//	DELETE /users/{id}                    deleteUser           → removeUser
//	POST   /users/{id}:enable             enableUser           → toggleUserEnabled
//	POST   /users/{id}:renew              renewUser            → renewUser
//	GET    /users/{id}/limit-usage        getUserLimitUsage    → getUserLimitUsage
//	GET    /users/{id}/limit-usage:all    getUserAllLimitUsage → getUserAllLimitUsage
//	POST   /users/{id}/limits:reset       resetUserLimits      → resetUserLimitsOnly
//
// 另两条（历史注释里曾写为「刻意不实现」）已在 users_reset.go 注册：
//
//	POST /users/{id}/statistics:reset           resetUserStatistics
//	GET  /users/{id}/statistics-resets/{resetId} getUserStatisticsReset
//
// 它们自成一个 registrar（RegisterUsersResetRoutes），因为依赖面不是 PG 而是作业队列（Redis + PG）。
// 原来的「复刻 Bull 等于换掉一套任务系统」这个理由现已由 internal/usersreset 解决：状态键与认领键
// 与 Node 逐字一致（两侧互见），队列由 Redis ZSET + PG advisory lock 选主承担，失败与进度都落在
// 同一个状态键里。降级粒度也因此刚好是这两条：队列没装配就不注册，请求照常回退 Node。

// usersAPIVersion/… 略：版本头与不缓存由 Router 的信封统一补（router.go:applyEnvelopeHeaders）。

// usersAPI 是 users 资源处理器的依赖集合，装配期构造一次。
type usersAPI struct {
	deps     Deps
	pools    *store.Pools
	problems ProblemWriter
	audit    AuditSink
	invalid  Invalidator
	logger   *logx.Logger
	now      func() time.Time
}

// newUsersAPI 组装处理器依赖；Store 未装配时返回 nil（调用方据此不注册路由）。
func newUsersAPI(deps Deps) *usersAPI {
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
	return &usersAPI{
		deps:     deps,
		pools:    deps.Store,
		problems: problems,
		audit:    deps.Audit,
		invalid:  deps.Invalidator,
		logger:   logger,
		now:      time.Now,
	}
}

// RegisterUsersRoutes 注册 users 资源的 18 条路由。
//
// Store 未装配时不注册任何路由并记 Error：此时请求回退 Node（那里有完整的实现），
// 而不是由 Go 作答一个读不出数据的 200。
func RegisterUsersRoutes(router *Router, deps Deps) {
	api := newUsersAPI(deps)
	if api == nil {
		if deps.Logger != nil {
			deps.Logger.Error("admin_users_store_unwired", map[string]any{
				"module": "users",
				"action": "routes_not_registered",
			})
		}
		return
	}
	api.register(router)
}

// register 逐条登记路由。Access 取自 Node OpenAPI 的 x-required-access。
func (api *usersAPI) register(router *Router) {
	type entry struct {
		method string
		path   string
		access AccessLevel
		op     string
		handle http.HandlerFunc
	}
	entries := []entry{
		{http.MethodGet, "/users", AccessAdmin, "listUsers", api.handleListUsers},
		{http.MethodPost, "/users", AccessAdmin, "createUser", api.handleCreateUser},
		{http.MethodGet, "/users:self", AccessRead, "listCurrentUser", api.handleCurrentUser},
		{http.MethodGet, "/users/tags", AccessAdmin, "getUserTags", api.handleUserTags},
		{http.MethodGet, "/users/key-groups", AccessAdmin, "getUserKeyGroups", api.handleUserKeyGroups},
		{http.MethodGet, "/users:filter-search", AccessAdmin, "filterSearchUsers", api.handleFilterSearchUsers},
		{http.MethodGet, "/users:search", AccessAdmin, "searchUsers", api.handleSearchUsers},
		{http.MethodPost, "/users:usageBatch", AccessAdmin, "getUsersUsage", api.handleUsersUsageBatch},
		{http.MethodPost, "/users:batchUpdate", AccessAdmin, "batchUpdateUsers", api.handleBatchUpdateUsers},
		{http.MethodGet, "/users/{id:[0-9]+}", AccessAdmin, "getUser", api.handleGetUser},
		{http.MethodPatch, "/users/{id:[0-9]+}", AccessAdmin, "updateUser", api.handleUpdateUser},
		{http.MethodDelete, "/users/{id:[0-9]+}", AccessAdmin, "deleteUser", api.handleDeleteUser},
		{http.MethodPost, "/users/{id:[0-9]+}:enable", AccessAdmin, "enableUser", api.handleEnableUser},
		{http.MethodPost, "/users/{id:[0-9]+}:renew", AccessAdmin, "renewUser", api.handleRenewUser},
		{http.MethodGet, "/users/{id:[0-9]+}/limit-usage", AccessRead, "getUserLimitUsage", api.handleUserLimitUsage},
		{http.MethodGet, "/users/{id:[0-9]+}/limit-usage:all", AccessRead, "getUserAllLimitUsage", api.handleUserAllLimitUsage},
		{http.MethodPost, "/users/{id:[0-9]+}/limits:reset", AccessAdmin, "resetUserLimits", api.handleResetUserLimits},
		// 「仅重置 5H 限额」：与上一行语义不同（只推进 limit_5h_cost_reset_at），见 users_5h_reset.go。
		// 路径写字面量而不用常量：`scripts/admin-endpoint-gap.mjs` 解析的是条目表里的字面量，
		// 写成常量会让这条路由对「撤 Node 判据」的快照钉子**不可见**（漏计正是它要防的）。
		{http.MethodPost, "/users/{id:[0-9]+}/limits:reset5h", AccessAdmin, "resetUser5hLimitOnly", api.handleResetUser5hLimit},
	}
	for _, item := range entries {
		router.Add(Route{
			Method:      item.method,
			Path:        item.path,
			Access:      item.access,
			Module:      "users",
			OperationID: item.op,
			Handler:     item.handle,
		})
	}
}

// handleFilterSearchUsers 与 handleSearchUsers 是同一实现（Node 侧 searchUsers 直接委托
// searchUsersForFilter，src/actions/users.ts:614）。
func (api *usersAPI) handleFilterSearchUsers(writer http.ResponseWriter, request *http.Request) {
	api.handleUserSearch(writer, request, "filter-search")
}

func (api *usersAPI) handleSearchUsers(writer http.ResponseWriter, request *http.Request) {
	api.handleUserSearch(writer, request, "search")
}

// handleListUsers 复刻 listUsers：游标分页 + 用户展示对象（含密钥摘要）。
func (api *usersAPI) handleListUsers(writer http.ResponseWriter, request *http.Request) {
	query, problems := parseUsersListQuery(request)
	if len(problems) > 0 {
		api.writeValidationProblem(writer, request, problems)
		return
	}
	principal, _ := PrincipalFrom(request.Context())

	result, err := api.pools.ListAdminUsers(request.Context(), store.AdminUserListFilters{
		Cursor:          query.cursor,
		Limit:           query.limit,
		SearchTerm:      query.searchTerm,
		TagFilters:      query.tagFilters,
		KeyGroupFilters: query.keyGroupFilters,
		StatusFilter:    query.statusFilter,
		SortBy:          query.sortBy,
		SortOrder:       query.sortOrder,
	})
	if err != nil {
		api.writeUsersActionError(writer, request, storeFailure("getUsersBatchCore", err))
		return
	}

	items, err := api.buildUserDisplays(request.Context(), result.Users, principal)
	if err != nil {
		api.writeUsersActionError(writer, request, storeFailure("getUsersBatchCore", err))
		return
	}

	nextCursor := any(nil)
	if result.NextCursor != nil {
		nextCursor = *result.NextCursor
	}
	writeUsersJSON(writer, http.StatusOK, map[string]any{
		"items": items,
		"pageInfo": map[string]any{
			"nextCursor": nextCursor,
			"hasMore":    result.HasMore,
			"limit":      query.limit,
		},
	})
}

// handleCurrentUser 复刻 listCurrentUser：只返回调用者自己的展示对象，形状与列表一致。
//
// 401 的判据是 **UserID == 0**（无主体），不是 `<= 0`：Node 侧判据为 `if (!currentUserId)`
// （handlers.ts:73-83），虚拟管理员 id 为 -1，在 JS 里是真值，所以 ADMIN_TOKEN 主体不在此被拦，
// 而是落到查库未命中 → 404。写成 `<= 0` 会把 ADMIN_TOKEN 误判成「认证不完整」（对拍 D3）。
func (api *usersAPI) handleCurrentUser(writer http.ResponseWriter, request *http.Request) {
	principal, ok := PrincipalFrom(request.Context())
	if !ok || principal.UserID == 0 {
		api.problems.WriteProblem(writer, request, http.StatusUnauthorized, "auth.missing",
			"Authentication is required.")
		return
	}
	row, err := api.pools.FindAdminUserByID(request.Context(), principal.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			api.problems.WriteProblem(writer, request, http.StatusNotFound, "resource.not_found",
				"Current user was not found.")
			return
		}
		api.writeUsersActionError(writer, request, storeFailure("getCurrentUserDisplay", err))
		return
	}
	items, err := api.buildUserDisplays(request.Context(), []store.AdminUserRow{*row}, principal)
	if err != nil {
		api.writeUsersActionError(writer, request, storeFailure("getCurrentUserDisplay", err))
		return
	}
	writeUsersJSON(writer, http.StatusOK, map[string]any{
		"items": items,
		"pageInfo": map[string]any{
			"nextCursor": nil,
			"hasMore":    false,
			"limit":      len(items),
		},
	})
}

// handleUserTags 复刻 getUserTags：{items: string[]}。
func (api *usersAPI) handleUserTags(writer http.ResponseWriter, request *http.Request) {
	tags, err := api.pools.ListAdminUserTags(request.Context())
	if err != nil {
		api.writeUsersActionError(writer, request, storeFailure("getAllUserTags", err))
		return
	}
	writeUsersJSON(writer, http.StatusOK, map[string]any{"items": tags})
}

// handleUserKeyGroups 复刻 getUserKeyGroups：{items: string[]}。
func (api *usersAPI) handleUserKeyGroups(writer http.ResponseWriter, request *http.Request) {
	groups, err := api.pools.ListAdminUserProviderGroups(request.Context())
	if err != nil {
		api.writeUsersActionError(writer, request, storeFailure("getAllUserKeyGroups", err))
		return
	}
	writeUsersJSON(writer, http.StatusOK, map[string]any{"items": groups})
}

// handleUserSearch 复刻 filterSearchUsers / searchUsers：两条端点在 Node 侧是同一个实现
// （src/actions/users.ts:614 的 searchUsers 直接委托 searchUsersForFilter）。
func (api *usersAPI) handleUserSearch(writer http.ResponseWriter, request *http.Request, kind string) {
	query := request.URL.Query()
	limit := 20
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			api.writeValidationProblem(writer, request, []usersInvalidParam{{
				Path: []any{"limit"}, Code: "invalid_type",
				Message: "Expected number, received string",
			}})
			return
		}
		if parsed < 1 || parsed > 5000 {
			// 两个方向在上游是 zod 的两条独立检查（`min(1).max(5000)`），语义码也因此不同：
			// zod 4 对 0 报 too_small（`Too small: expected number to be >=1`）、对 5001 报 too_big。
			// 合起来的旧写法把两者都报成 too_big，会让「值太小」在 UI 上被当成「值太大」。
			code, message := "too_small", "Number must be greater than or equal to 1"
			if parsed > 5000 {
				code = "too_big"
				message = fmt.Sprintf("Number must be less than or equal to 5000, received %d", parsed)
			}
			api.writeValidationProblem(writer, request, []usersInvalidParam{{
				Path: []any{"limit"}, Code: code, Message: message,
			}})
			return
		}
		limit = parsed
	}
	searchTerm := strings.TrimSpace(query.Get("q"))

	rows, err := api.pools.SearchAdminUsers(request.Context(), searchTerm, limit)
	if err != nil {
		code := "DATABASE_ERROR"
		if kind == "search" {
			code = "DATABASE_ERROR"
		}
		api.writeUsersActionError(writer, request, usersActionError(code, err))
		return
	}
	items := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		items = append(items, map[string]any{"id": row.ID, "name": row.Name})
	}
	writeUsersJSON(writer, http.StatusOK, map[string]any{"items": items})
}

// handleGetUser 复刻 getUser：单用户详情（toUser 形状），非管理员只能读自己。
func (api *usersAPI) handleGetUser(writer http.ResponseWriter, request *http.Request) {
	id, ok := usersPathID(request)
	if !ok {
		api.writeValidationProblem(writer, request, []usersInvalidParam{{
			Path: []any{"id"}, Code: "invalid_type", Message: "Expected number, received nan",
		}})
		return
	}
	principal, _ := PrincipalFrom(request.Context())
	if !principal.IsAdmin && principal.UserID != id {
		api.writeUsersActionError(writer, request, usersActionError("PERMISSION_DENIED", nil))
		return
	}
	row, err := api.pools.FindAdminUserByID(request.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			api.writeUsersActionError(writer, request, usersActionError("NOT_FOUND", nil))
			return
		}
		api.writeUsersActionError(writer, request, storeFailure("getUserById", err))
		return
	}
	writeUsersJSON(writer, http.StatusOK, usersRedactFullKey(row.NodeJSON()))
}

// handleCreateUser 复刻 createUser：query `withDefaultKey !== "false"` 决定是否附带默认密钥。
func (api *usersAPI) handleCreateUser(writer http.ResponseWriter, request *http.Request) {
	raw, problems := decodeUsersBody(request, usersCreateSchema)
	if len(problems) > 0 {
		api.writeValidationProblem(writer, request, problems)
		return
	}
	principal, _ := PrincipalFrom(request.Context())
	if !principal.IsAdmin {
		api.writeUsersActionError(writer, request, usersActionError("PERMISSION_DENIED", nil))
		return
	}

	input := usersCreateInputFrom(raw)
	row, err := api.pools.CreateAdminUser(request.Context(), input)
	if err != nil {
		api.emitUserAudit(request, principal, "user.create", 0, usersStringOr(raw.name, ""), nil, nil, false, "CREATE_FAILED")
		api.writeUsersActionError(writer, request, storeFailure("addUser", err))
		return
	}

	withDefaultKey := strings.ToLower(strings.TrimSpace(request.URL.Query().Get("withDefaultKey"))) != "false"
	responseBody := map[string]any{"user": usersCreateResponseUser(row)}
	if withDefaultKey {
		plainKey, err := usersGenerateKey()
		if err != nil {
			api.writeUsersActionError(writer, request, storeFailure("addUser", err))
			return
		}
		keyID, err := api.pools.CreateAdminUserDefaultKey(
			request.Context(), row.ID, plainKey, "default", row.ProviderGroup)
		if err != nil {
			api.writeUsersActionError(writer, request, storeFailure("addUser", err))
			return
		}
		// 完整密钥只在这一处返回（Node 同：src/actions/users.ts:1473 的注释）。
		responseBody["defaultKey"] = map[string]any{"id": keyID, "name": "default", "key": plainKey}
	}

	after := usersRedactFullKey(row.NodeJSON())
	api.emitUserAudit(request, principal, "user.create", row.ID, row.Name, nil, after, true, "")
	api.invalidateUser(request, row.ID, nil)

	location := fmt.Sprintf("%s/users/%d", MountPrefix, row.ID)
	writer.Header().Set("Location", location)
	writer.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	writer.Header().Set("Pragma", "no-cache")
	writeUsersJSON(writer, http.StatusCreated, responseBody)
}

// handleUpdateUser 复刻 updateUser：部分更新 + 字段级权限 + 审计（含前后快照）。
func (api *usersAPI) handleUpdateUser(writer http.ResponseWriter, request *http.Request) {
	id, ok := usersPathID(request)
	if !ok {
		api.writeValidationProblem(writer, request, []usersInvalidParam{{
			Path: []any{"id"}, Code: "invalid_type", Message: "Expected number, received nan",
		}})
		return
	}
	raw, problems := decodeUsersBody(request, usersUpdateSchema)
	if len(problems) > 0 {
		api.writeValidationProblem(writer, request, problems)
		return
	}
	principal, _ := PrincipalFrom(request.Context())

	// 快照在改之前取（与 Node 一致）：失败审计也要带 before。
	before, beforeErr := api.pools.FindAdminUserByID(request.Context(), id)
	if beforeErr != nil && !errors.Is(beforeErr, store.ErrNotFound) {
		api.writeUsersActionError(writer, request, storeFailure("editUser", beforeErr))
		return
	}

	unauthorized := usersUnauthorizedFields(raw.presentFields(), principal.IsAdmin)
	if len(unauthorized) > 0 {
		api.emitUserAudit(request, principal, "user.update", id, usersNameOf(before), before, nil, false,
			"PERMISSION_DENIED")
		api.writeUsersActionError(writer, request, usersActionError("PERMISSION_DENIED", nil))
		return
	}
	if !principal.IsAdmin && principal.UserID != id {
		api.emitUserAudit(request, principal, "user.update", id, usersNameOf(before), before, nil, false,
			"PERMISSION_DENIED")
		api.writeUsersActionError(writer, request, usersActionError("PERMISSION_DENIED", nil))
		return
	}
	patch, err := raw.toStorePatch()
	if err != nil {
		api.writeValidationProblem(writer, request, []usersInvalidParam{{
			Path: []any{"expiresAt"}, Code: "invalid_string", Message: err.Error(),
		}})
		return
	}

	updated, err := api.pools.UpdateAdminUser(request.Context(), id, patch)
	if err != nil {
		api.emitUserAudit(request, principal, "user.update", id, usersNameOf(before), before, nil, false,
			"UPDATE_FAILED")
		api.writeUsersActionError(writer, request, storeFailure("editUser", err))
		return
	}
	if !updated {
		api.writeUsersActionError(writer, request, usersActionError("NOT_FOUND", nil))
		return
	}

	// 5h 重置模式变更：清用户与各密钥的成本缓存（Node 同，见 src/actions/users.ts:1827）。
	if raw.limit5hResetMode != nil && before != nil &&
		(before.Limit5hResetMode == nil || *before.Limit5hResetMode != *raw.limit5hResetMode) {
		api.invalidateUserCost(request, id)
	}

	after, _ := api.pools.FindAdminUserByID(request.Context(), id)
	var afterSnapshot map[string]any
	if after != nil {
		afterSnapshot = usersRedactFullKey(after.NodeJSON())
	}
	api.emitUserAudit(request, principal, "user.update", id, usersNameOf(after), before,
		afterSnapshot, true, "")
	api.invalidateUser(request, id, nil)
	writeUsersJSON(writer, http.StatusOK, map[string]any{"id": id, "updated": true})
}

// handleDeleteUser 复刻 deleteUser：软删 + 审计；账本行保留。
func (api *usersAPI) handleDeleteUser(writer http.ResponseWriter, request *http.Request) {
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
	before, beforeErr := api.pools.FindAdminUserByID(request.Context(), id)
	if beforeErr != nil && !errors.Is(beforeErr, store.ErrNotFound) {
		api.writeUsersActionError(writer, request, storeFailure("removeUser", beforeErr))
		return
	}
	deleted, err := api.pools.SoftDeleteAdminUser(request.Context(), id)
	if err != nil {
		api.emitUserAudit(request, principal, "user.delete", id, usersNameOf(before), before, nil, false,
			"DELETE_FAILED")
		api.writeUsersActionError(writer, request, storeFailure("removeUser", err))
		return
	}
	if !deleted {
		api.writeUsersActionError(writer, request, usersActionError("NOT_FOUND", nil))
		return
	}
	api.emitUserAudit(request, principal, "user.delete", id, usersNameOf(before), before, nil, true, "")
	api.invalidateUser(request, id, nil)
	writeUsersNoContent(writer)
}

// handleEnableUser 复刻 enableUser：只改 is_enabled，禁止禁用自己。
func (api *usersAPI) handleEnableUser(writer http.ResponseWriter, request *http.Request) {
	id, ok := usersPathID(request)
	if !ok {
		api.writeValidationProblem(writer, request, []usersInvalidParam{{
			Path: []any{"id"}, Code: "invalid_type", Message: "Expected number, received nan",
		}})
		return
	}
	raw, problems := decodeUsersBody(request, usersEnableSchema)
	if len(problems) > 0 {
		api.writeValidationProblem(writer, request, problems)
		return
	}
	principal, _ := PrincipalFrom(request.Context())
	if !principal.IsAdmin {
		api.writeUsersActionError(writer, request, usersActionError("PERMISSION_DENIED", nil))
		return
	}
	if raw.enabled == nil {
		api.writeValidationProblem(writer, request, []usersInvalidParam{{
			Path: []any{"enabled"}, Code: "invalid_type", Message: "Required",
		}})
		return
	}
	if principal.UserID == id && !*raw.enabled {
		api.writeUsersActionError(writer, request, usersActionError("PERMISSION_DENIED", nil))
		return
	}
	updated, err := api.pools.UpdateAdminUser(request.Context(), id, store.AdminUserPatch{IsEnabled: raw.enabled})
	if err != nil {
		api.writeUsersActionError(writer, request, storeFailure("toggleUserEnabled", err))
		return
	}
	if !updated {
		api.writeUsersActionError(writer, request, usersActionError("NOT_FOUND", nil))
		return
	}
	api.invalidateUser(request, id, nil)
	writeUsersJSON(writer, http.StatusOK, map[string]any{"ok": true})
}

// handleRenewUser 复刻 renewUser：按系统时区解析日期输入，可同时启用用户。
func (api *usersAPI) handleRenewUser(writer http.ResponseWriter, request *http.Request) {
	id, ok := usersPathID(request)
	if !ok {
		api.writeValidationProblem(writer, request, []usersInvalidParam{{
			Path: []any{"id"}, Code: "invalid_type", Message: "Expected number, received nan",
		}})
		return
	}
	raw, problems := decodeUsersBody(request, usersRenewSchema)
	if len(problems) > 0 {
		api.writeValidationProblem(writer, request, problems)
		return
	}
	principal, _ := PrincipalFrom(request.Context())
	if !principal.IsAdmin {
		api.writeUsersActionError(writer, request, usersActionError("PERMISSION_DENIED", nil))
		return
	}
	if raw.expiresAt == nil || strings.TrimSpace(*raw.expiresAt) == "" {
		api.writeValidationProblem(writer, request, []usersInvalidParam{{
			Path: []any{"expiresAt"}, Code: "too_small", Message: "String must contain at least 1 character(s)",
		}})
		return
	}

	location, err := api.systemLocation(request.Context())
	if err != nil {
		api.writeUsersActionError(writer, request, storeFailure("renewUser", err))
		return
	}
	expiresAt, err := parseUsersDateInput(*raw.expiresAt, location)
	if err != nil {
		api.writeUsersActionError(writer, request, usersActionError("INVALID_FORMAT", err))
		return
	}
	if _, err := api.pools.FindAdminUserByID(request.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			api.writeUsersActionError(writer, request, usersActionError("NOT_FOUND", nil))
			return
		}
		api.writeUsersActionError(writer, request, storeFailure("renewUser", err))
		return
	}
	patch := store.AdminUserPatch{ExpiresAt: &expiresAt}
	if raw.enableUser != nil && *raw.enableUser {
		enabled := true
		patch.IsEnabled = &enabled
	}
	updated, err := api.pools.UpdateAdminUser(request.Context(), id, patch)
	if err != nil {
		api.writeUsersActionError(writer, request, storeFailure("renewUser", err))
		return
	}
	if !updated {
		api.writeUsersActionError(writer, request, usersActionError("NOT_FOUND", nil))
		return
	}
	api.invalidateUser(request, id, nil)
	writeUsersJSON(writer, http.StatusOK, map[string]any{"ok": true})
}

// handleUserLimitUsage 复刻 getUserLimitUsage：RPM 恒为 0（Node 同，滑动窗口读不到当前值），
// 每日消费按用户的 dailyResetTime/dailyResetMode 起算并被 costResetAt 收敛。
func (api *usersAPI) handleUserLimitUsage(writer http.ResponseWriter, request *http.Request) {
	id, ok := usersPathID(request)
	if !ok {
		api.writeValidationProblem(writer, request, []usersInvalidParam{{
			Path: []any{"id"}, Code: "invalid_type", Message: "Expected number, received nan",
		}})
		return
	}
	principal, _ := PrincipalFrom(request.Context())
	row, err := api.pools.FindAdminUserByID(request.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			if !principal.IsAdmin {
				// Node 先查用户再查权限：不存在时对非管理员表现为 404（不是 403）。
				api.writeUsersActionError(writer, request, usersActionError("NOT_FOUND", nil))
				return
			}
			api.writeUsersActionError(writer, request, usersActionError("NOT_FOUND", nil))
			return
		}
		api.writeUsersActionError(writer, request, storeFailure("getUserLimitUsage", err))
		return
	}
	if !principal.IsAdmin && principal.UserID != id {
		api.writeUsersActionError(writer, request, usersActionError("PERMISSION_DENIED", nil))
		return
	}

	location, err := api.systemLocation(request.Context())
	if err != nil {
		api.writeUsersActionError(writer, request, storeFailure("getUserLimitUsage", err))
		return
	}
	now := api.now()
	resetTime := limit.NormalizeResetTime(usersStringOr(row.DailyResetTime, "00:00"))
	mode := usersResetMode(row.DailyResetMode)
	start := limit.WindowStart(limit.PeriodDaily, now, resetTime, mode, location)
	if row.CostResetAt != nil && row.CostResetAt.After(start) {
		start = *row.CostResetAt
	}
	total, err := api.pools.SumLedgerCostInTimeRange(
		request.Context(), store.LedgerEntityUser, id, start, now)
	if err != nil {
		api.writeUsersActionError(writer, request, storeFailure("getUserLimitUsage", err))
		return
	}
	resetInfo := limit.ResetInfoFor(limit.PeriodDaily, now, resetTime, mode, nil, location)

	dailyQuota := any(nil)
	if quota := usersNumber(row.DailyQuota); quota != nil && *quota > 0 {
		dailyQuota = *quota
	}
	var rpmLimit any
	if row.RPM != nil && *row.RPM > 0 {
		rpmLimit = *row.RPM
	}
	var resetAt any
	if resetInfo.ResetAt != nil {
		resetAt = store.JSONDate(*resetInfo.ResetAt)
	}
	writeUsersJSON(writer, http.StatusOK, map[string]any{
		"rpm": map[string]any{
			"current": 0,
			"limit":   rpmLimit,
			"window":  "per_minute",
		},
		"dailyCost": map[string]any{
			"current": usersCostNumber(total),
			"limit":   dailyQuota,
			"resetAt": resetAt,
		},
	})
}

// handleUserAllLimitUsage 复刻 getUserAllLimitUsage：五个周期的用量与限额。
func (api *usersAPI) handleUserAllLimitUsage(writer http.ResponseWriter, request *http.Request) {
	id, ok := usersPathID(request)
	if !ok {
		api.writeValidationProblem(writer, request, []usersInvalidParam{{
			Path: []any{"id"}, Code: "invalid_type", Message: "Expected number, received nan",
		}})
		return
	}
	principal, _ := PrincipalFrom(request.Context())
	row, err := api.pools.FindAdminUserByID(request.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			api.writeUsersActionError(writer, request, usersActionError("NOT_FOUND", nil))
			return
		}
		api.writeUsersActionError(writer, request, storeFailure("getUserAllLimitUsage", err))
		return
	}
	if !principal.IsAdmin && principal.UserID != id {
		api.writeUsersActionError(writer, request, usersActionError("PERMISSION_DENIED", nil))
		return
	}

	location, err := api.systemLocation(request.Context())
	if err != nil {
		api.writeUsersActionError(writer, request, storeFailure("getUserAllLimitUsage", err))
		return
	}
	now := api.now()
	resetTime := limit.NormalizeResetTime(usersStringOr(row.DailyResetTime, "00:00"))
	dailyMode := usersResetMode(row.DailyResetMode)

	// 5h：Node 在 fixed 模式下读 Redis 运行态窗口；Go 侧此处按账本聚合（差异已登记白名单，
	// 见文件头说明与报告）。窗口起点仍与 Node 的 rolling 起点一致，再被重置标记收敛。
	fiveHourStart := limit.ClipStartByResetAt(
		now.Add(-5*time.Hour), limit.LaterReset(row.CostResetAt, row.Limit5hCostResetAt))
	dailyStart := limit.ClipStartByResetAt(
		limit.WindowStart(limit.PeriodDaily, now, resetTime, dailyMode, location), row.CostResetAt)
	weeklyStart := limit.ClipStartByResetAt(
		limit.WindowStart(limit.PeriodWeekly, now, resetTime, dailyMode, location), row.CostResetAt)
	monthlyStart := limit.ClipStartByResetAt(
		limit.WindowStart(limit.PeriodMonthly, now, resetTime, dailyMode, location), row.CostResetAt)

	usage5h, err := api.pools.SumLedgerCostInTimeRange(
		request.Context(), store.LedgerEntityUser, id, fiveHourStart, now)
	if err != nil {
		api.writeUsersActionError(writer, request, storeFailure("getUserAllLimitUsage", err))
		return
	}
	usageDaily, err := api.pools.SumLedgerCostInTimeRange(
		request.Context(), store.LedgerEntityUser, id, dailyStart, now)
	if err != nil {
		api.writeUsersActionError(writer, request, storeFailure("getUserAllLimitUsage", err))
		return
	}
	usageWeekly, err := api.pools.SumLedgerCostInTimeRange(
		request.Context(), store.LedgerEntityUser, id, weeklyStart, now)
	if err != nil {
		api.writeUsersActionError(writer, request, storeFailure("getUserAllLimitUsage", err))
		return
	}
	usageMonthly, err := api.pools.SumLedgerCostInTimeRange(
		request.Context(), store.LedgerEntityUser, id, monthlyStart, now)
	if err != nil {
		api.writeUsersActionError(writer, request, storeFailure("getUserAllLimitUsage", err))
		return
	}
	usageTotal, err := api.pools.SumLedgerTotalCost(
		request.Context(), store.LedgerEntityUser, id, row.CostResetAt)
	if err != nil {
		api.writeUsersActionError(writer, request, storeFailure("getUserAllLimitUsage", err))
		return
	}

	limitOf := func(value json.Number) any {
		parsed := usersNumber(value)
		if parsed == nil {
			return nil
		}
		return *parsed
	}
	dailyLimit := any(nil)
	if quota := usersNumber(row.DailyQuota); quota != nil && *quota > 0 {
		dailyLimit = *quota
	}
	writeUsersJSON(writer, http.StatusOK, map[string]any{
		"limit5h":     map[string]any{"usage": usersCostNumber(usage5h), "limit": limitOf(row.Limit5hUSD)},
		"limitDaily":  map[string]any{"usage": usersCostNumber(usageDaily), "limit": dailyLimit},
		"limitWeekly": map[string]any{"usage": usersCostNumber(usageWeekly), "limit": limitOf(row.LimitWeeklyUSD)},
		"limitMonthly": map[string]any{
			"usage": usersCostNumber(usageMonthly), "limit": limitOf(row.LimitMonthlyUSD),
		},
		"limitTotal": map[string]any{"usage": usersCostNumber(usageTotal), "limit": limitOf(row.LimitTotalUSD)},
	})
}

// handleResetUserLimits 复刻 resetUserLimits：只推进成本重置标记，不动日志与统计。
func (api *usersAPI) handleResetUserLimits(writer http.ResponseWriter, request *http.Request) {
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
	if _, err := api.pools.FindAdminUserByID(request.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			api.writeUsersActionError(writer, request, usersActionError("NOT_FOUND", nil))
			return
		}
		api.writeUsersActionError(writer, request, storeFailure("resetUserLimitsOnly", err))
		return
	}
	resetAt := api.now()
	updated, err := api.pools.ResetAdminUserCostMarkers(request.Context(), id, resetAt)
	if err != nil {
		api.writeUsersActionError(writer, request, storeFailure("resetUserLimitsOnly", err))
		return
	}
	if !updated {
		api.writeUsersActionError(writer, request, usersActionError("NOT_FOUND", nil))
		return
	}
	api.invalidateUserCost(request, id)
	api.invalidateUser(request, id, nil)
	writeUsersNoContent(writer)
}

// handleUsersUsageBatch 复刻 getUsersUsage：{usageByKeyId: {<keyId>: {...}}}，键是字符串。
func (api *usersAPI) handleUsersUsageBatch(writer http.ResponseWriter, request *http.Request) {
	raw, problems := decodeUsersBody(request, usersUsageBatchSchema)
	if len(problems) > 0 {
		api.writeValidationProblem(writer, request, problems)
		return
	}
	principal, _ := PrincipalFrom(request.Context())
	if !principal.IsAdmin {
		api.writeUsersActionError(writer, request, usersActionError("PERMISSION_DENIED", nil))
		return
	}
	ids := usersSanitizeIDs(raw.userIDs)
	if len(ids) > 500 {
		api.writeUsersActionError(writer, request, usersActionError("INVALID_FORMAT", nil))
		return
	}
	usageByKeyID, err := api.keyUsageByKeyID(request.Context(), ids)
	if err != nil {
		api.writeUsersActionError(writer, request, storeFailure("getUsersUsageBatch", err))
		return
	}
	writeUsersJSON(writer, http.StatusOK, map[string]any{"usageByKeyId": usageByKeyID})
}

// handleBatchUpdateUsers 复刻 batchUpdateUsers：事务内先校验全部存在，再整批更新。
func (api *usersAPI) handleBatchUpdateUsers(writer http.ResponseWriter, request *http.Request) {
	raw, problems := decodeUsersBody(request, usersBatchUpdateSchema)
	if len(problems) > 0 {
		api.writeValidationProblem(writer, request, problems)
		return
	}
	principal, _ := PrincipalFrom(request.Context())
	if !principal.IsAdmin {
		api.writeUsersActionError(writer, request, usersActionError("PERMISSION_DENIED", nil))
		return
	}
	ids := usersSanitizeIDsForBatch(raw.userIDs)
	if len(ids) == 0 {
		api.writeUsersActionError(writer, request, usersActionError("REQUIRED_FIELD", nil))
		return
	}
	if len(ids) > 500 {
		api.writeUsersActionError(writer, request, usersActionError("INVALID_FORMAT", nil))
		return
	}
	patch, err := raw.updates.toBatchPatch()
	if err != nil {
		api.writeValidationProblem(writer, request, []usersInvalidParam{{
			Path: []any{"updates"}, Code: "invalid_format", Message: err.Error(),
		}})
		return
	}
	if patch.IsEmpty() {
		api.writeUsersActionError(writer, request, usersActionError("EMPTY_UPDATE", nil))
		return
	}

	updated, missing, err := api.pools.BatchUpdateAdminUsers(request.Context(), ids, patch)
	if err != nil {
		api.writeUsersActionError(writer, request, storeFailure("batchUpdateUsers", err))
		return
	}
	if len(missing) > 0 {
		api.writeUsersActionError(writer, request, usersActionError("NOT_FOUND", nil))
		return
	}
	if patch.Limit5hResetMode != nil {
		for _, id := range updated {
			api.invalidateUser(request, id, nil)
			api.invalidateUserCost(request, id)
		}
	}
	writeUsersJSON(writer, http.StatusOK, map[string]any{
		"requestedCount": len(ids),
		"updatedCount":   len(updated),
		"updatedIds":     updated,
	})
}

// keyUsageByKeyID 组装 usageByKeyId（Node 的 GetUsersUsageBatchResult）。
func (api *usersAPI) keyUsageByKeyID(ctx context.Context, ids []int64) (map[string]any, error) {
	usageByKeyID := map[string]any{}
	if len(ids) == 0 {
		return usageByKeyID, nil
	}
	keys, err := api.pools.ListAdminUserKeys(ctx, ids)
	if err != nil {
		return nil, err
	}
	usage, err := api.pools.LoadAdminUserKeyUsage(ctx, keys)
	if err != nil {
		return nil, err
	}
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
		var lastUsedAt any
		if stats.LastUsedAt != nil {
			lastUsedAt = store.JSONDate(*stats.LastUsedAt)
		}
		var lastProviderName any
		if stats.LastProviderName != nil {
			lastProviderName = *stats.LastProviderName
		}
		usageByKeyID[strconv.FormatInt(key.ID, 10)] = map[string]any{
			"todayUsage":       usersCostNumber(entry.Today.TotalCost.String()),
			"todayCallCount":   stats.TodayCallCount,
			"todayTokens":      usersCostNumber(entry.Today.TotalTokens.String()),
			"lastUsedAt":       lastUsedAt,
			"lastProviderName": lastProviderName,
			"modelStats":       modelStats,
		}
	}
	return usageByKeyID, nil
}

// usersGenerateKey 复刻 addUser 里的默认密钥生成：`sk-` + 16 随机字节的十六进制。
func usersGenerateKey() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", fmt.Errorf("adminapi: 生成默认密钥失败: %w", err)
	}
	return "sk-" + hex.EncodeToString(buffer), nil
}

// systemLocation 复刻 resolveSystemTimezone 的三级取值：system_settings.timezone →
// 环境变量 TZ（未设置取 `Asia/Shanghai`）→ UTC。默认值与校验集中在 config，
// 调用点不再自读环境变量（自读会丢掉 TZ 默认值并退化为 UTC 日界）。
func (api *usersAPI) systemLocation(ctx context.Context) (*time.Location, error) {
	raw, err := api.pools.AdminSystemTimezone(ctx)
	if err != nil {
		return nil, err
	}
	return config.ResolveLocationFromEnv(raw), nil
}

// parseUsersDateInput 复刻 parseDateInputAsTimezone：把 "YYYY-MM-DD" 或带时间的字符串
// 按系统时区解析为绝对时刻。
func parseUsersDateInput(raw string, location *time.Location) (time.Time, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return time.Time{}, errors.New("日期为空")
	}
	if len(trimmed) == len("2006-01-02") {
		parsed, err := time.ParseInLocation("2006-01-02", trimmed, location)
		if err == nil {
			return parsed, nil
		}
	}
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04",
	} {
		if parsed, err := time.ParseInLocation(layout, trimmed, location); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("无法解析日期 %q", raw)
}

// invalidateUser 使该用户的认证缓存失效（Node 的 invalidateCachedUser）。
func (api *usersAPI) invalidateUser(request *http.Request, userID int64, keys []int64) {
	if api.invalid == nil {
		return
	}
	ctx := request.Context()
	api.invalid.InvalidateUserAuth(ctx, userID)
	for _, keyID := range keys {
		api.invalid.InvalidateKeyCost(ctx, keyID)
	}
}

// invalidateUserCost 清用户与其全部密钥的成本缓存（Node 的 clearUserCostCache）。
func (api *usersAPI) invalidateUserCost(request *http.Request, userID int64) {
	if api.invalid == nil {
		return
	}
	ctx := request.Context()
	api.invalid.InvalidateUserCost(ctx, userID)
	keys, err := api.pools.ListAdminUserKeys(ctx, []int64{userID})
	if err != nil {
		api.logger.Warn("admin_users_cost_cache_keys_failed", map[string]any{
			"user_id": userID,
			"error":   err.Error(),
		})
		return
	}
	for _, key := range keys {
		api.invalid.InvalidateKeyCost(ctx, key.ID)
	}
}

// emitUserAudit 写一条用户类审计。Node 侧同为 fire-and-forget（src/lib/audit/emit.ts:38-60）。
//
// IP 走 auditClientIP（audit_ip.go 的统一提取链）：旧实现只取 RemoteAddr、连头都不看，
// 与 Node 的 getClientIp 相抵。
func (api *usersAPI) emitUserAudit(
	request *http.Request,
	principal Principal,
	action string,
	targetID int64,
	targetName string,
	before *store.AdminUserRow,
	after map[string]any,
	success bool,
	errorMessage string,
) {
	if api.audit == nil {
		return
	}
	event := AuditEvent{
		Category:   "user",
		Principal:  principal,
		Action:     action,
		TargetType: "user",
		TargetName: targetName,
		Details:    after,
		IP:         auditClientIP(request.Context(), api.pools, request),
		UserAgent:  request.UserAgent(),
		Success:    success,
	}
	if targetID > 0 {
		event.TargetID = strconv.FormatInt(targetID, 10)
	}
	if before != nil {
		event.Before = usersRedactFullKey(before.NodeJSON())
		if event.TargetName == "" {
			event.TargetName = before.Name
		}
	}
	if !success {
		event.ErrorMessage = errorMessage
	}
	api.audit.Emit(request.Context(), event)
}

// usersPathID 取路径参数 id；缺失或非数字时返回 false。
func usersPathID(request *http.Request) (int64, bool) {
	params := ParamsFrom(request.Context())
	raw := strings.TrimSpace(params["id"])
	if raw == "" {
		return 0, false
	}
	parsed, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || parsed <= 0 {
		return 0, false
	}
	return parsed, true
}

// usersSanitizeIDs 复刻 getUsersUsageBatch 的 id 归一：去重、只留正整数。
func usersSanitizeIDs(raw []int64) []int64 {
	seen := make(map[int64]struct{}, len(raw))
	ids := make([]int64, 0, len(raw))
	for _, id := range raw {
		if id <= 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

// usersSanitizeIDsForBatch 复刻 batchUpdateUsers 的 id 归一：只去重（**不**过滤非正数，
// Node 侧只做 Number.isInteger 判定）。
func usersSanitizeIDsForBatch(raw []int64) []int64 {
	seen := make(map[int64]struct{}, len(raw))
	ids := make([]int64, 0, len(raw))
	for _, id := range raw {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

// storeFailure 把落库失败包成 users 模块的 action 错误。
func storeFailure(action string, err error) error {
	return &ActionError{Resource: "user", Code: "DATABASE_ERROR", Status: 503, Err: err}
}

// usersNameOf 取可空行的名字（nil 行返回空串）。
func usersNameOf(row *store.AdminUserRow) string {
	if row == nil {
		return ""
	}
	return row.Name
}

// usersNumber 把 JSON number 解成 float64（空与非数都返回 nil）。
func usersNumber(value json.Number) *float64 {
	text := strings.TrimSpace(value.String())
	if text == "" || text == "null" {
		return nil
	}
	parsed, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return nil
	}
	return &parsed
}

// usersCostNumber 把 numeric 文本转成响应里的 JSON number。
func usersCostNumber(value string) json.Number {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return json.Number("0")
	}
	return json.Number(trimmed)
}

// usersStringOr 取可空字符串的兜底值。
func usersStringOr(value *string, fallback string) string {
	if value == nil || *value == "" {
		return fallback
	}
	return *value
}

// usersResetMode 把可空重置模式映射成 limit.ResetMode（空取 fixed，与 Node 的 ?? "fixed" 一致）。
func usersResetMode(value *string) limit.ResetMode {
	if value == nil || *value == "" {
		return limit.ResetFixed
	}
	if *value == string(limit.ResetRolling) {
		return limit.ResetRolling
	}
	return limit.ResetFixed
}
