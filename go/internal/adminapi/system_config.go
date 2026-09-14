package adminapi

import (
	"net/http"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是**根级旧管理端点** `/api/admin/system-config`（Node 侧
// src/app/api/admin/system-config/route.ts）的 GET 与 POST。
//
// 它是 UI 静态化之前 dashboard 用的老接口，与 /api/v1/system/settings 是**两条不同的路径**：
//
//   - 鉴权档位不同：这条在 Node 里自己判 `session?.user.role !== "admin"`，失败时答**纯文本
//     `Unauthorized`**（不是 problem+json）。故这里以 AccessRead 注册（需要身份才知道是不是
//     管理员），再在处理器里判管理员并写纯文本 401。
//     已知差异：**未认证**时 Node 也是纯文本 401，而 Go 的守卫会先答 problem+json 401——两者
//     都是 401，只有正文形状不同；已登记为允许差异（见本文件末尾的差异清单）。
//   - 校验档位不同：这条走 action 层 schema（宽松：忽略未知键、数字列接受字符串），
//     且 **POST 只转发它自己列出的字段**——`billNonSuccessfulRequests`、`billHedgeLosers`、
//     `fakeStreamingWhitelist`、`replay*`、`cacheEffectivenessEnabled`、`publicStatus*`、
//     `ipExtraction*`、`ipGeoLookupEnabled`、`allowNonConversationEndpointProviderFallback`
//     都不在其中。照抄这个取舍而不是「顺手补上」：补上会让同一次调用在两个入口下结果不同，
//     而 UI 正在从这条迁到 v1——两个入口给两个答案比少一个开关更糟。
//
// 允许差异清单（与 Node 不同，且不打算对齐）：
//
//  1. 未认证请求的 401 正文：Go 为 problem+json（守卫作答），Node 为纯文本 `Unauthorized`。
//  2. POST 的 zod 细节文案：Node 把 zod 的第一条 issue.message 原样回给前端（中文文案表），
//     Go 的校验器不复刻 zod 文案表（同 users_schema.go 文件头的取舍），改回族码或英文说明。

const unauthorizedPlainText = "Unauthorized"

// systemConfigAPI 是旧端点的处理器依赖：与 /system/settings 共用同一套读写与失效路径。
type systemConfigAPI struct {
	settings *systemSettingsAPI
	pools    *store.Pools
	logger   *logx.Logger
}

// RegisterSystemConfigRoutes 注册根级 GET/POST /api/admin/system-config。
//
// Store 未装配时不注册（回退 Node）：这条端点整体是「读写设置行」，没有库就没有正确作答。
func RegisterSystemConfigRoutes(router *Router, deps Deps) {
	if deps.Store == nil {
		if deps.Logger != nil {
			deps.Logger.Error("admin_system_config_store_unwired", map[string]any{
				"module": "system-config",
				"action": "routes_not_registered",
			})
		}
		return
	}
	settings, ok := newSystemSettingsAPI(deps)
	if !ok {
		return
	}
	logger := deps.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	api := &systemConfigAPI{settings: settings, pools: deps.Store, logger: logger}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		router.Add(Route{
			Method:               method,
			Path:                 "/api/admin/system-config",
			Access:               AccessRead,
			Module:               "system-config",
			OperationID:          "adminSystemConfig",
			NoManagementEnvelope: true,
			Handler:              http.HandlerFunc(api.handleSystemConfig),
		})
	}
}

// handleSystemConfig 按方法分派 GET / POST。
func (api *systemConfigAPI) handleSystemConfig(writer http.ResponseWriter, request *http.Request) {
	principal, _ := PrincipalFrom(request.Context())
	if !principal.IsAdmin {
		// Node 的 `session?.user.role !== "admin"` 分支：纯文本、不带 Content-Type 之外的装饰。
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte(unauthorizedPlainText))
		return
	}
	if request.Method == http.MethodGet {
		api.handleGet(writer, request)
		return
	}
	api.handlePost(writer, request)
}

// handleGet 复刻 GET：读库（缺行补默认行）后返回投影。
func (api *systemConfigAPI) handleGet(writer http.ResponseWriter, request *http.Request) {
	row, err := api.pools.EnsureAdminSystemSettings(request.Context())
	if err != nil {
		api.logger.Error("admin_system_config_read_failed", map[string]any{"error": err.Error()})
		writeShellJSONNoEnvelope(writer, http.StatusInternalServerError, map[string]any{
			"error": "获取系统配置失败",
		})
		return
	}
	writeShellJSONNoEnvelope(writer, http.StatusOK, systemSettingsProjection(row, api.settings.now()))
}

// handlePost 复刻 POST：宽松校验 → 窗口不变量 → 写库 → 失效广播。
func (api *systemConfigAPI) handlePost(writer http.ResponseWriter, request *http.Request) {
	decoded, problems := api.settings.legacyDecode(request)
	if len(problems) > 0 {
		writeLegacySystemConfigValidationError(writer, problems)
		return
	}

	current, err := api.pools.EnsureAdminSystemSettings(request.Context())
	if err != nil {
		writeShellJSONNoEnvelope(writer, http.StatusInternalServerError, map[string]any{
			"error": err.Error(),
		})
		return
	}
	beforeBody := systemSettingsProjection(current, api.settings.now())
	if !systemSettingsDiscoveryWindowValid(decoded, beforeBody) {
		writeShellJSONNoEnvelope(writer, http.StatusBadRequest, map[string]any{
			"error":     "discoveryWindowInvalid",
			"errorCode": discoveryWindowInvalidErrorCode,
		})
		return
	}

	updated, err := api.pools.UpdateAdminSystemSettings(
		request.Context(), current.ID, decoded.legacyPatch(),
	)
	if err != nil {
		api.logger.Error("admin_system_config_write_failed", map[string]any{"error": err.Error()})
		writeShellJSONNoEnvelope(writer, http.StatusInternalServerError, map[string]any{
			"error": err.Error(),
		})
		return
	}
	api.settings.invalidateSettingsCaches(request.Context(), decoded.timezonePresent)
	writeShellJSONNoEnvelope(writer, http.StatusOK, systemSettingsProjection(updated, api.settings.now()))
}

// legacyPatch 是旧端点的字段取舍：**丢弃** Node 的 POST 清单里没有的已验证字段。
func (d systemSettingsUpdate) legacyPatch() store.AdminSystemSettingsPatch {
	parsed := d
	// 这些键 Node 的 POST 会先校验再**不转发**（其显式字段清单里没有它们）。
	parsed.billNonSuccessfulRequests = nil
	parsed.billHedgeLosers = nil
	parsed.allowNonConvEndpointFallback = nil
	parsed.fakeStreamingWhitelist = nil
	parsed.fakeStreamingWhitelistPresent = false
	parsed.replayEnabled = nil
	parsed.replayEnabledPresent = false
	parsed.replayCacheTTLMinutes = nil
	parsed.cacheEffectivenessEnabled = nil
	parsed.cacheEffectivenessEnabledPresent = false
	parsed.publicStatusWindowHours = nil
	parsed.publicStatusWindowPresent = false
	parsed.publicStatusAggregationMins = nil
	parsed.publicStatusAggregationPresent = false
	parsed.ipExtractionConfig = nil
	parsed.ipExtractionConfigPresent = false
	parsed.ipGeoLookupEnabled = nil
	return parsed.patch()
}

// writeLegacySystemConfigValidationError 复刻旧端点的 400 正文
// （route.ts:143-155 的三段：窗口码 → discovery 码 → 第一条 issue 的文案）。
func writeLegacySystemConfigValidationError(writer http.ResponseWriter, problems []settingsInvalidParam) {
	code := settingsValidationErrorCode(problems)
	switch code {
	case discoveryWindowInvalidErrorCode:
		writeShellJSONNoEnvelope(writer, http.StatusBadRequest, map[string]any{
			"error": "discoveryWindowInvalid", "errorCode": code,
		})
	case discoverySettingsInvalidErrorCode:
		writeShellJSONNoEnvelope(writer, http.StatusBadRequest, map[string]any{
			"error": "discoverySettingsInvalid", "errorCode": code,
		})
	default:
		message := "数据验证失败"
		if len(problems) > 0 && problems[0].Message != "" {
			message = problems[0].Message
		}
		writeShellJSONNoEnvelope(writer, http.StatusBadRequest, map[string]any{"error": message})
	}
}

// newSystemSettingsAPI 造 /system/settings 的处理器依赖；缺 Store 时返回 false。
func newSystemSettingsAPI(deps Deps) (*systemSettingsAPI, bool) {
	if deps.Store == nil {
		return nil, false
	}
	logger := deps.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	problems := deps.Problems
	if problems == nil {
		problems = NewProblems(logger)
	}
	return &systemSettingsAPI{
		pools:           deps.Store,
		problems:        problems,
		audit:           deps.Audit,
		invalidator:     deps.Invalidator,
		publisher:       deps.PublicStatusPublisher,
		dashboardCaches: deps.DashboardCaches,
		logger:          logger,
		now:             time.Now,
	}, true
}
