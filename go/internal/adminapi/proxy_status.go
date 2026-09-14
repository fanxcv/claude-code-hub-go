package adminapi

import (
	"context"
	"net/http"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 根级 GET /api/proxy-status。
//
// 唯一真源：src/app/api/proxy-status/route.ts（50 行）+ src/lib/proxy-status-tracker.ts（223 行）。
//
// **一处必须先纠正的既有判断**：
// Node 的 `ProxyStatusTracker` 明确自述「当前实现基于数据库数据聚合，确保多运行时环境下的一致性」——
// `startRequest`/`endRequest` 都是**空实现**（`void params`），数据全部来自三笔 SQL
// （未终局请求 / 每人最近一次完成请求 / 非删除用户列表）。故本端点**不需要任何进程内观测态**，
// 也不需要扩 Deps：Go 侧早就有了同一套移植（`internal/store/admin_live.go` 的三个
// `AdminProxyStatus*` 查询），`GET /api/v1/dashboard/proxy-status` 已在用它们。
//
// 与那条 dashboard 端点的**唯一区别是外壳与权限档**：
//
//	根级（本条）    不在 /api/v1 应用壳里 → 不发管理面信封头；普通用户 403 `{"error":"权限不足"}`
//	dashboard 那条  /api/v1 内 → 带信封；同样 admin 档
//
// 正文形状两者相同（都是 `{users:[…]}`），故这里**复用同一批 store 查询与同一批 body 类型**，
// 并配一条「两侧逐字节同形」的钉子（TestProxyStatusRootMatchesDashboardPayload）——
// 复制粘贴两份组装逻辑会立刻分叉成两份真相，而症状是运维页面与 dashboard 显示不同的数字。
//
// 与 Node 的两处差异（登记）：
//
//  1. Node 有 2 秒进程内缓存（连同 in-flight 合并），Go 每次直查。差异只体现为
//     `duration`/`elapsed` 的毫秒级新鲜度（两个字段本来就随时间变化），不改变集合与计数；
//  2. Node 的用户列表没有 ORDER BY（顺序未定义），Go 按 `id` 升序稳定输出——便于对拍与人工核对。
type proxyStatusAPI struct {
	pools    *store.Pools
	logger   *logx.Logger
	now      func() time.Time
	problems ProblemWriter
}

// RegisterProxyStatusRoute 注册根级 proxy-status。
func RegisterProxyStatusRoute(router *Router, deps Deps) {
	if deps.Store == nil {
		if deps.Logger != nil {
			deps.Logger.Error("admin_proxy_status_unwired", map[string]any{
				"module": "dashboard",
				"reason": "store_missing",
				"path":   "/api/proxy-status",
			})
		}
		return
	}
	logger := deps.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	problems := deps.Problems
	if problems == nil {
		problems = NewProblems(logger)
	}
	api := &proxyStatusAPI{pools: deps.Store, logger: logger, now: time.Now, problems: problems}
	router.Add(Route{
		Method: http.MethodGet,
		// 管理员档：Node 先判登录（401），再判 role（403 `{"error":"权限不足"}`）。
		Access:               AccessAdmin,
		Path:                 "/api/proxy-status",
		Module:               "dashboard",
		OperationID:          "getProxyStatus",
		NoManagementEnvelope: true,
		Handler:              http.HandlerFunc(api.handleGet),
	})
}

func (api *proxyStatusAPI) handleGet(writer http.ResponseWriter, request *http.Request) {
	payload, err := proxyStatusPayload(request.Context(), api.pools, api.now)
	if err != nil {
		api.logger.Error("admin_proxy_status_failed", map[string]any{
			"path":  request.URL.Path,
			"error": err.Error(),
		})
		writeRawJSON(writer, http.StatusInternalServerError, map[string]any{"error": "获取代理状态失败"})
		return
	}
	writeRawJSON(writer, http.StatusOK, payload)
}

// proxyStatusPayload 组装 `{users:[…]}`（ProxyStatusTracker.fetchAllUsersStatus 的移植）。
//
// 顺序与 Node 一致：先取三份数据（Node 用 Promise.all，这里顺序取——三笔都是只读且无依赖，
// 顺序取只是少一层并发，结果与快照一致性无关）。
func proxyStatusPayload(ctx context.Context, pools *store.Pools, now func() time.Time) (any, error) {
	users, err := pools.AdminProxyStatusUsers(ctx)
	if err != nil {
		return nil, err
	}
	activeRows, err := pools.AdminProxyStatusActiveRequests(ctx)
	if err != nil {
		return nil, err
	}
	lastRows, err := pools.AdminProxyStatusLastRequests(ctx)
	if err != nil {
		return nil, err
	}

	nowMS := now().UnixMilli()

	activeByUser := make(map[int64][]dashboardProxyStatusActiveBody, len(activeRows))
	for _, row := range activeRows {
		startTime := nowMS
		if row.CreatedAt != nil {
			startTime = row.CreatedAt.UnixMilli()
		}
		activeByUser[row.UserID] = append(activeByUser[row.UserID], dashboardProxyStatusActiveBody{
			RequestID:    row.RequestID,
			KeyName:      proxyStatusKeyName(row.KeyName, row.KeyString),
			ProviderID:   row.ProviderID,
			ProviderName: row.ProviderName,
			Model:        proxyStatusModel(row.Model),
			StartTime:    startTime,
			Duration:     nowMS - startTime,
		})
	}

	lastByUser := make(map[int64]store.AdminProxyStatusLastRow, len(lastRows))
	for _, row := range lastRows {
		// DISTINCT ON 已保证每人一行；这里再挡一次，与 Node 的 `if (!lastMap.has(userId))` 同判。
		if _, seen := lastByUser[row.UserID]; !seen {
			lastByUser[row.UserID] = row
		}
	}

	items := make([]dashboardProxyStatusUserBody, 0, len(users))
	for _, user := range users {
		// Node 的 `?? []`：无活跃请求时是空数组，不是 null。
		activeRequests := activeByUser[user.UserID]
		if activeRequests == nil {
			activeRequests = []dashboardProxyStatusActiveBody{}
		}

		var lastRequest *dashboardProxyStatusLastBody
		if row, ok := lastByUser[user.UserID]; ok {
			endTime := nowMS
			if row.EndTime != nil {
				endTime = row.EndTime.UnixMilli()
			}
			lastRequest = &dashboardProxyStatusLastBody{
				RequestID:    row.RequestID,
				KeyName:      proxyStatusKeyName(row.KeyName, row.KeyString),
				ProviderID:   row.ProviderID,
				ProviderName: row.ProviderName,
				Model:        proxyStatusModel(row.Model),
				EndTime:      endTime,
				Elapsed:      nowMS - endTime,
			}
		}

		items = append(items, dashboardProxyStatusUserBody{
			UserID:         user.UserID,
			UserName:       user.UserName,
			ActiveCount:    len(activeRequests),
			ActiveRequests: activeRequests,
			LastRequest:    lastRequest,
		})
	}

	return struct {
		Users []dashboardProxyStatusUserBody `json:"users"`
	}{Users: items}, nil
}
