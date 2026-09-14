package adminapi

import (
	"context"
	"net/http"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 Node 的 `GET /api/admin/database/status` 的 Go 落点
// （Node 侧 `src/app/api/admin/database/status/route.ts:18-95`）。
//
// 契约（逐条对齐 Node 的 DatabaseStatus，字段名与状态码照抄）：
//
//	非 admin / 未登录 → 401
//	可用          → 200 {isAvailable:true,  containerName:"<host>:<port>", databaseName, databaseSize,
//	                     tableCount, postgresVersion}
//	连接不可用     → 200 {isAvailable:false, ..., databaseSize:"N/A", tableCount:0,
//	                     postgresVersion:"N/A", error:"数据库连接不可用，请检查数据库服务状态"}
//	取元数据失败   → 200 {isAvailable:true, ..., databaseSize:"Unknown", tableCount:0,
//	                     postgresVersion:"Unknown", error:"<原因>"}
//	程序错误      → 500 {error:"获取数据库状态失败", details:"<原因>"}
//
// **一并记下的两处 Node 特性**（有意不复刻，理由在报告里）：
//  1. Node 的 401 正文是 **text/plain 的 "Unauthorized"**，不是 JSON。Go 侧走管理面统一的
//     401 信封（状态码一致、正文形状不同）。
//  2. Node 通过 `docker-executor` 在数据库容器内执行 psql 取 size/tableCount/version；
//     Go 从连接自身查（pg_size_pretty / information_schema / server_version），同一事实、不同来源。

// databaseStatusTimeout 限制单次元数据查询（Node 侧无显式超时，但它是运维读，不该挂住）。
const databaseStatusTimeout = 5 * time.Second

// RegisterDatabaseStatusRoutes 注册数据库状态端点（Store 未装配时不注册，回退 Node）。
func RegisterDatabaseStatusRoutes(router *Router, deps Deps) {
	if deps.Store == nil {
		if deps.Logger != nil {
			deps.Logger.Warn("admin_database_status_store_unwired", map[string]any{
				"module": "database",
				"action": "route_not_registered",
			})
		}
		return
	}
	api := &databaseStatusAPI{pools: deps.Store, logger: adminLoggerOf(deps)}
	router.Add(Route{
		Method:               http.MethodGet,
		Path:                 "/api/admin/database/status",
		Access:               AccessAdmin,
		Module:               "database",
		OperationID:          "getDatabaseStatus",
		NoManagementEnvelope: true,
		Handler:              http.HandlerFunc(api.handle),
	})
}

type databaseStatusAPI struct {
	pools  *store.Pools
	logger adminLogger
}

// adminLogger 是本文件用到的最小日志面（避免直接依赖 *logx.Logger 的具体方法集）。
type adminLogger interface {
	Warn(event string, fields map[string]any)
}

func (api *databaseStatusAPI) handle(writer http.ResponseWriter, request *http.Request) {
	containerName, dsnDatabaseName := api.pools.DSNEndpoint()

	ctx, cancel := context.WithTimeout(request.Context(), databaseStatusTimeout)
	defer cancel()

	info, err := api.pools.ReadDBStatusInfo(ctx)
	if err != nil {
		// Node 区分两种失败：连不上（isAvailable=false）与取不到详情（isAvailable=true + error）。
		// Go 侧只有一次查询，故按「连不上」的同义词处理——查不到元数据就无法证明库可用。
		api.logger.Warn("admin_database_status_unavailable", map[string]any{
			"containerName": containerName,
			"error":         err.Error(),
		})
		adminWriteJSON(writer, http.StatusOK, map[string]any{
			"isAvailable":     false,
			"containerName":   containerName,
			"databaseName":    dsnDatabaseName,
			"databaseSize":    "N/A",
			"tableCount":      0,
			"postgresVersion": "N/A",
			"error":           "数据库连接不可用，请检查数据库服务状态",
		})
		return
	}

	// databaseName 以连接为准（DSN 解析失败时 DSN 侧为空串），与 Node 取库配置同义。
	databaseName := info.DatabaseName
	if databaseName == "" {
		databaseName = dsnDatabaseName
	}
	adminWriteJSON(writer, http.StatusOK, map[string]any{
		"isAvailable":     true,
		"containerName":   containerName,
		"databaseName":    databaseName,
		"databaseSize":    info.SizePretty,
		"tableCount":      info.TableCount,
		"postgresVersion": info.ServerVersion,
	})
}
