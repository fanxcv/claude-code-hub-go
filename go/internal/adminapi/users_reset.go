package adminapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/fanxcv/claude-code-hub-go/go/internal/usersreset"
)

// 本文件实现 users 资源的两条统计重置路由（Node 侧 handlers.ts:228-263）：
//
//	POST /users/{id}/statistics:reset           → resetUserStatistics（202 + Location）
//	GET  /users/{id}/statistics-resets/{resetId} → getUserStatisticsReset（200 / 404 / 503）
//
// **为什么单独一个 registrar**：这两条路由的依赖面与 users 其余 17 条不同——它们要的是
// 「作业队列 + Redis + 重置执行器」这一整套（usersreset），而不是那一批纯 PG 的读写。
// 独立注册让「队列没装配」的降级粒度刚好是这两条（回退 Node），而不是整组 users 路由。
// 也因为同一原因，它们不走 users.go 的 usersAPI，而是自带一个只依赖 Deps.Store 的窄结构。

// UsersResetQueue 是用户统计重置作业队列的窄接口（实现：internal/usersreset.Queue）。
//
// 为什么要接口而不是直接用 *usersreset.Queue：管理面只该看到「排一次作业」与「查一次作业」
// 这两件事；把具体类型写进 Deps 会把「ZSET 队列、选主、租约、重试」整套内部形式变成管理面的
// 依赖面（同 Deps.SessionCounter 的取舍）。
type UsersResetQueue interface {
	// Enqueue 排一次重置并返回已入队的公开记录。
	Enqueue(ctx context.Context, userID int64) (usersreset.PublicRecord, error)
	// Find 按 (用户, 作业) 查公开记录；不存在或用户不匹配时返回 (nil, nil)。
	Find(ctx context.Context, userID int64, resetID string) (*usersreset.PublicRecord, error)
}

// usersResetUUIDPattern 判断路径参数 resetId 的形状。
//
// 与 Node 的差别（登记为白名单）：Node 用 zod 的 .uuid()，它还会校验版本位与变体位
// （zod 的正则要求第 3 组以 1-8 开头、第 4 组以 89ab 开头）。这里只校验 8-4-4-4-12 的十六进制
// 形状——形状不对一样是 400，形状对而版本位不合规的串在 Node 会 400、在这里会走到「查不到」而
// 404。作业 id 由本服务生成（必为 v4），故这条差异只在手工构造的请求上可见。
var usersResetUUIDPattern = regexp.MustCompile(
	`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// usersResetAPI 是这两条路由的处理器依赖。
type usersResetAPI struct {
	deps  Deps
	queue UsersResetQueue
}

// RegisterUsersResetRoutes 注册 users 资源的统计重置两条路由。
//
// 队列或连接池未装配时**整组不注册**并记 Error：这两条都没法诚实地降级——排不进去的作业会让
// UI 显示一个永远 queued 的状态，查不到的状态会让轮询一直 404。回退 Node 是唯一正确选择。
func RegisterUsersResetRoutes(router *Router, deps Deps) {
	if deps.UsersReset == nil || deps.Store == nil {
		if deps.Logger != nil {
			deps.Logger.Error("admin_users_reset_unwired", map[string]any{
				"module": "users",
				"action": "routes_not_registered",
			})
		}
		return
	}
	api := &usersResetAPI{deps: deps, queue: deps.UsersReset}
	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/users/{id:[0-9]+}/statistics:reset",
		Access:      AccessAdmin,
		Module:      "users",
		OperationID: "resetUserStatistics",
		Handler:     http.HandlerFunc(api.handleResetUserStatistics),
	})
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/users/{id:[0-9]+}/statistics-resets/{resetId}",
		Access:      AccessAdmin,
		Module:      "users",
		OperationID: "getUserStatisticsReset",
		Handler:     http.HandlerFunc(api.handleGetUserStatisticsReset),
	})
}

// handleResetUserStatistics 复刻 resetUserStatistics（handlers.ts:228-241）。
//
// 与 Node 的三段一致：查用户是否存在（不存在 404）、排作业、202 带 Location。
// Node 那一步「取出该用户的密钥 id 清单并传给入队」在这里由队列自己做（usersreset 的
// ensurePrepared 用同一个过滤条件查一遍），故少一次管理面到 PG 的往返。
func (api *usersResetAPI) handleResetUserStatistics(writer http.ResponseWriter, request *http.Request) {
	userID, ok := usersResetPathUserID(request)
	if !ok {
		api.writeInvalidID(writer, request)
		return
	}
	if _, err := api.deps.Store.FindAdminUserByID(request.Context(), userID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			api.deps.Problems.WriteActionError(writer, request, usersActionError("NOT_FOUND", nil))
			return
		}
		api.deps.Problems.WriteActionError(writer, request, usersActionError("DATABASE_ERROR", err))
		return
	}

	record, err := api.queue.Enqueue(request.Context(), userID)
	if err != nil {
		// 失败码取 CONNECTION_FAILED（503）而不是 OPERATION_FAILED（500）：Node 的 action 在调用
		// 入队前置 enqueueStarted=true，之后任何异常都落到 503 那一支（actions/users.ts:2385）。
		// 入队的失败面也确实都是依赖不可用（Redis 状态键/认领键、PG 键清单），不是请求本身有问题。
		if api.deps.Logger != nil {
			api.deps.Logger.Warn("admin_users_reset_enqueue_failed", map[string]any{
				"userId": userID,
				"error":  err.Error(),
			})
		}
		api.deps.Problems.WriteActionError(writer, request, usersActionError("CONNECTION_FAILED", err))
		return
	}

	// Location 与 Node 逐字一致：硬编码的 /api/v1 前缀（handlers.ts:231 就是字面量）。
	writer.Header().Set("Location", fmt.Sprintf(
		"%s/users/%d/statistics-resets/%s", MountPrefix, userID, record.ResetID))
	writeUsersJSON(writer, http.StatusAccepted, record)
}

// handleGetUserStatisticsReset 复刻 getUserStatisticsReset（handlers.ts:243-263）。
func (api *usersResetAPI) handleGetUserStatisticsReset(writer http.ResponseWriter, request *http.Request) {
	userID, ok := usersResetPathUserID(request)
	if !ok {
		api.writeInvalidID(writer, request)
		return
	}
	resetID := strings.TrimSpace(ParamsFrom(request.Context())["resetId"])
	if !usersResetUUIDPattern.MatchString(resetID) {
		// Node 的 params schema 是 z.string().uuid()，不合法即 400（走 fromZodError）。
		api.deps.Problems.WriteValidationError(writer, request, []InvalidParam{{
			Path:    []any{"resetId"},
			Code:    "invalid_string",
			Message: "Invalid uuid",
		}})
		return
	}

	record, err := api.queue.Find(request.Context(), userID, resetID)
	if err != nil {
		// 依赖不可用（Redis 读不到/写状态键的内容坏了）与 Node 的 catch 同语义：503 dependency.unavailable。
		api.deps.Problems.WriteProblem(writer, request, http.StatusServiceUnavailable,
			"dependency.unavailable", problemTitle(http.StatusServiceUnavailable))
		return
	}
	if record == nil {
		// 与 Node 的 404 逐字一致：错误码是这条路由特有的 user.statistics_reset_not_found
		// （handlers.ts:255），不是 users 模块通用的 user.not_found。
		api.deps.Problems.WriteProblem(writer, request, http.StatusNotFound,
			"user.statistics_reset_not_found", problemTitle(http.StatusNotFound))
		return
	}
	writeUsersJSON(writer, http.StatusOK, record)
}

// usersResetPathUserID 取路径参数 id（路由正则已限定为数字，故这里只做非空防御）。
func usersResetPathUserID(request *http.Request) (int64, bool) {
	if request == nil {
		return 0, false
	}
	return usersPathID(request)
}

// writeInvalidID 作答 id 参数非法的 400。
//
// 实际上不可达（路由正则 {id:[0-9]+} 已经把非数字挡在门外，那些请求会回退 Node，由 Node 的
// zod 作答同一个 400），保留它是为了让处理器的前置条件显式——不写就等于「假定路由永远匹配」。
func (api *usersResetAPI) writeInvalidID(writer http.ResponseWriter, request *http.Request) {
	api.deps.Problems.WriteValidationError(writer, request, []InvalidParam{{
		Path:    []any{"id"},
		Code:    "invalid_type",
		Message: "Expected number, received nan",
	}})
}
