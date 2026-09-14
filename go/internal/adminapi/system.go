package adminapi

import (
	"net/http"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是管理面的 **system 资源**：/api/v1/system/*。
//
// Node 侧该资源共 4 条（src/app/api/v1/resources/system/router.ts）：
//
//	GET /system/settings          admin   ← 已实现（见 system_settings.go）
//	PUT /system/settings          admin   ← 已实现（见 system_settings.go）
//	GET /system/display-settings  read    ← 本文件
//	GET /system/timezone          read    ← 本文件
//
// **订正（2026-09-13）**：本注释此前写「两条 settings 端点未实现、原样回退 Node」，
// 事实已变——`system_settings.go` 的 `RegisterSystemSettingsRoutes` 在**同一注册点**
// （`cmd/cchd/admin.go`）注入了 GET 与 PUT `updateSystemSettings`（`system_settings.go:490-503`），
// 生产实测 GET 200。当时列出的两条理由（投影字段不足、部分更新校验与失效广播缺失）
// 已在那一波补齐（投影逐字段对齐 `toSystemSettings`、写后失效广播与 public-status 重发）。
// 保留这段历史是为了说明「当时为何缓做」，而不是现状描述——**以代码与生产实测为准**。
//
// 两条读端点不需要 Redis，也不需要写权限：它们只读 system_settings 的单行。

// systemDisplaySettingsBody 逐字对应 Node 的 display-settings 响应
// （system/handlers.ts:19-27 的 { siteTitle, currencyDisplay, billingModelSource }）。
type systemDisplaySettingsBody struct {
	SiteTitle          string `json:"siteTitle"`
	CurrencyDisplay    string `json:"currencyDisplay"`
	BillingModelSource string `json:"billingModelSource"`
}

// systemTimezoneBody 逐字对应 getServerTimeZone 的返回（system-config.ts:63-75）。
type systemTimezoneBody struct {
	TimeZone string `json:"timeZone"`
}

// systemAPI 是 system 资源处理器的依赖集合。
type systemAPI struct {
	pools    *store.Pools
	problems ProblemWriter
	logger   *logx.Logger
}

// RegisterSystemRoutes 注册 system 资源的**已就绪**端点（见文件头的两条例外）。
//
// Store 未装配时全部不注册（回退 Node）——与本包其余模块同一条纪律。
func RegisterSystemRoutes(router *Router, deps Deps) {
	if deps.Store == nil {
		if deps.Logger != nil {
			deps.Logger.Error("admin_system_store_unwired", map[string]any{
				"module": "system",
				"action": "routes_not_registered",
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
	api := &systemAPI{pools: deps.Store, problems: problems, logger: logger}

	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/system/display-settings",
		Access:      AccessRead,
		Module:      "system",
		OperationID: "getSystemDisplaySettings",
		Handler:     http.HandlerFunc(api.handleDisplaySettings),
	})
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/system/timezone",
		Access:      AccessRead,
		Module:      "system",
		OperationID: "getSystemTimezone",
		Handler:     http.HandlerFunc(api.handleTimezone),
	})
}

// handleDisplaySettings 复刻 getSystemDisplaySettings（system/handlers.ts:19-27）。
//
// 注意：它读的是**完整设置行**再取三列，不做任何权限判定（Node 侧同样是 read 档位下的裸读）；
// 三列都是非敏感展示项（站点标题、货币、计费模型来源），因此与 Node 的暴露面一致。
func (api *systemAPI) handleDisplaySettings(writer http.ResponseWriter, request *http.Request) {
	settings, err := api.pools.FindSystemSettings(request.Context())
	if err != nil {
		api.writeSystemFailure(writer, request, err)
		return
	}
	writeShellJSON(writer, http.StatusOK, systemDisplaySettingsBody{
		SiteTitle:          settings.SiteTitle,
		CurrencyDisplay:    settings.CurrencyDisplay,
		BillingModelSource: settings.BillingModelSource,
	})
}

// handleTimezone 复刻 getServerTimeZone（system-config.ts:63-75 → resolveSystemTimezone）。
//
// 三级取值（system_settings.timezone → 环境变量 TZ → UTC）由 meSystemLocation 实现；
// 无效时区按 Node 的 try/catch 语义跳到下一级，而不是报错。
func (api *systemAPI) handleTimezone(writer http.ResponseWriter, request *http.Request) {
	location, err := meSystemLocation(request.Context(), api.pools)
	if err != nil {
		api.writeSystemFailure(writer, request, err)
		return
	}
	writeShellJSON(writer, http.StatusOK, systemTimezoneBody{TimeZone: location.String()})
}

// writeSystemFailure 作答 system 资源的失败。
//
// 状态码取 Node 的 actionError（system/handlers.ts:44-55）：含「权限」→ 403，其余 400。
// 这两条读端点没有权限分支（read 档位），实际只有内部的 400/404。
func (api *systemAPI) writeSystemFailure(
	writer http.ResponseWriter,
	request *http.Request,
	err error,
) {
	api.logger.Warn("admin_system_request_failed", map[string]any{
		"path":  request.URL.Path,
		"error": err.Error(),
	})
	api.problems.WriteProblem(writer, request, http.StatusBadRequest, "", "")
}
