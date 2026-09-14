package adminapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 根级 GET /api/system-settings。
//
// 唯一真源：src/app/api/system-settings/route.ts 的 GET（26 行）——它**不在** `/api/v1` 应用壳里，
// 是 Next 的普通 route handler：
//
//   - 只要「有登录会话」即可（无角色门槛），故 Access 为 AccessRead；
//   - 正文是 `getSystemSettings()` 的返回值（repository/system-config.ts:551 → toSystemSettings）；
//   - 未登录 → 401 `{"error":"未授权，请先登录"}`；查询失败 → 500 `{"error":"获取系统设置失败"}`；
//   - 因为它不在 `/api/v1` 下，Node 不发管理面的 `X-API-Version` 信封头（与 /api/version 同判），
//     故本路由带 NoManagementEnvelope。
//
// 正文形状**复用** `/api/v1/system/settings` 已移植的 `buildSystemSettingsBody`
// （system_settings.go:179，逐字段对齐 transformers.ts:246 的 toSystemSettings，含三处归一化：
// replayCacheTtlMinutes 夹取、legacyHedgeMaxInFlight 夹到 1..4、responseFixerConfig 补默认对象）。
// 不复用会立刻分叉成两份真相——那正是「半做会静默丢字段」的成因。
//
// 与 Node 的两处**登记差异**（形状层面的既有裁决，非本路由引入）：
//
//  1. 401/403 走管理面问题信封（Node 这里是裸 `{"error":"…"}`）——Go 的认证在 Router 层统一完成，
//     各路由看不到「未认证」这一事实；
//  2. 500 同样走问题信封（Node 是裸 JSON）。
//
// 两处都只在故障路径上可见，且与 `/api/admin/database/status` 的同类差异一致。成功路径逐字段同形。
func RegisterRootSystemSettingsRoute(router *Router, deps Deps) {
	if deps.Store == nil {
		logRootSystemSettingsUnwired(deps, "store_missing", "/api/system-settings")
		return
	}
	logger := deps.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	api := &rootSystemSettingsAPI{pools: deps.Store, now: time.Now, logger: logger}
	router.Add(Route{
		Method:               http.MethodGet,
		Path:                 "/api/system-settings",
		Access:               AccessRead,
		Module:               "system",
		OperationID:          "getRootSystemSettings",
		NoManagementEnvelope: true,
		Handler:              http.HandlerFunc(api.handleGet),
	})
}

// rootSystemSettingsPools 是根级读面需要的库能力（与 /system/settings 同源：读库缺行时补默认行）。
type rootSystemSettingsPools interface {
	EnsureAdminSystemSettings(ctx context.Context) (*store.AdminSystemSettings, error)
}

// rootSystemSettingsAPI 是根级系统设置读面的处理器。
type rootSystemSettingsAPI struct {
	pools  rootSystemSettingsPools
	now    func() time.Time
	logger *logx.Logger
}

func (api *rootSystemSettingsAPI) handleGet(writer http.ResponseWriter, request *http.Request) {
	row, err := api.pools.EnsureAdminSystemSettings(request.Context())
	if err != nil {
		api.logger.Error("admin_root_system_settings_failed", map[string]any{
			"path":  request.URL.Path,
			"error": err.Error(),
		})
		writeRawJSON(writer, http.StatusInternalServerError, map[string]any{
			"error": "获取系统设置失败",
		})
		return
	}
	writeShellJSONNoEnvelope(writer, http.StatusOK, buildSystemSettingsBody(row, api.now()))
}

// writeRawJSON 写一份裸 JSON 正文（Node 的 `NextResponse.json({error})` 形状）。
//
// 与 writeShellJSON 的分工：那个用于管理面信封内的端点，这个用于根级裸 body（Node 侧不在
// `/api/v1` 应用壳下的路由），头与写盘方式都照 Node 的 NextResponse.json 对齐。
func writeRawJSON(writer http.ResponseWriter, status int, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		http.Error(writer, "", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_, _ = writer.Write(encoded)
}

// logRootSystemSettingsUnwired 记一条缺依赖的启动告警（与同包其余 registrar 同形）。
func logRootSystemSettingsUnwired(deps Deps, reason, path string) {
	if deps.Logger == nil {
		return
	}
	deps.Logger.Error("admin_root_system_settings_unwired", map[string]any{
		"module": "system",
		"reason": reason,
		"path":   path,
	})
}
