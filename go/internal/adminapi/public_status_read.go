package adminapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pubstatus"
)

// 本文件实现两条**公开状态读端点**：
//
//	GET /api/v1/public/status   Node: src/app/api/v1/resources/public/handlers.ts:22 getPublicStatus
//	GET /api/public-status      Node: src/app/api/public-status/route.ts（69 行）
//
// 两者的业务内核**完全同源**（Node 也是两份入口共用 `readPublicStatusPayload` +
// `buildPublicStatusRouteResponse`），差异只有外壳与错误形状：
//
//	外壳  v1 在 /api/v1 应用壳内 → 带管理面信封头；根级不在壳内 → NoManagementEnvelope
//	400   v1 是 RFC7807 problem+json（errorCode = `public_status.invalid_query` + invalidParams）；
//	      根级是 NextResponse.json(`{error, details}`)，**连错误形状都不同**，故不能共用一条渲染
//	503   只在**路由状态为 rebuilding** 时给（`no_snapshot` 是 200 + 空 groups —— 前端据此
//	      显示「暂无数据」而不是报错）；503 时必须带 `Cache-Control: no-store`，否则中间缓存会把
//	      503 缓存住，重建完成后访客仍然看到「正在重建」
//
// 读路径本体在 `internal/pubstatus`（键布局、manifest 状态判定、payload 净化、查询契约），
// 本文件只做 HTTP 外壳，故两者的分支测试互不牵连。
//
// **与本任务无关但必须写明的退役前置**：`manifest` 与 `snapshot` 两组键今天只有 Node 的
// rebuild-worker 在写（Go 侧无等价物）。这两条读端点因此是「读 Node 产出的投影」；Node 下线后
// 投影不再刷新，公开状态页会在 `freshUntil` 到期后停在 stale 并逐步降级。要真正退役，需另派
// 一路移植写侧（rollup + aggregation + rebuild-worker，约 2050 行），详见报告。

// PublicStatusReadOptions 是两条读端点需要的运行态依赖。
//
// 用注册参数而不是 `Deps` 字段：`Deps` 是多路并行时的共享文件，而这份依赖只有一个消费者
// （与 `RegisterProvidersLimitRoutes` / `RegisterLeaderboardRoutes` 同形）。
type PublicStatusReadOptions struct {
	// Store 是公开状态读路径的 Redis 门面（`pubstatus.NewRedisStatusStore(client, logger)`）。
	// nil 时两条路由都不注册——它们会原样回退 Node；**宁可不答，不可乱答**：没有 Redis 时
	// readPublicStatusPayload 会分支到「redis-unavailable」并恒答 rebuilding，
	// 那会让公开页在 Redis 未配置时永远显示「正在重建」。
	Store pubstatus.PublicStatusStore
}

// RegisterPublicStatusReadRoutes 注册两条公开状态读端点。
func RegisterPublicStatusReadRoutes(router *Router, deps Deps, options PublicStatusReadOptions) {
	if options.Store == nil {
		if deps.Logger != nil {
			deps.Logger.Warn("admin_public_status_read_unwired", map[string]any{
				"module": "public-status",
				"reason": "redis_store_missing",
				"routes": []string{"GET /public/status", "GET /api/public-status"},
			})
		}
		return
	}

	logger := deps.Logger
	if logger == nil {
		logger = logx.New(nil)
	}
	api := &publicStatusReadAPI{store: options.Store, logger: logger}

	router.Add(Route{
		Method: http.MethodGet,
		// Path 相对 /api/v1（Router 加挂载前缀）；Node 侧 x-required-access 为 public
		// （router.ts:54 `requireAuth("public")`：不加认证也不提取凭据）。
		Path:        "/public/status",
		Access:      AccessPublic,
		Module:      "public-status",
		OperationID: "getPublicStatus",
		Handler:     http.HandlerFunc(api.handleV1GetPublicStatus),
	})

	router.Add(Route{
		Method: http.MethodGet,
		// 根级绝对路径：不在 /api/v1 应用壳内，故不发管理面信封头（与 /api/proxy-status 同判）。
		Path:                 "/api/public-status",
		Access:               AccessPublic,
		Module:               "public-status",
		OperationID:          "getPublicStatusRoot",
		NoManagementEnvelope: true,
		Handler:              http.HandlerFunc(api.handleRootGetPublicStatus),
	})
}

type publicStatusReadAPI struct {
	store  pubstatus.PublicStatusStore
	logger *logx.Logger
}

// handleV1GetPublicStatus 作答 `/api/v1/public/status`（handlers.ts:22-81）。
func (api *publicStatusReadAPI) handleV1GetPublicStatus(
	writer http.ResponseWriter,
	request *http.Request,
) {
	response, validationErr := api.buildResponse(request)
	if validationErr != nil {
		api.writeV1ValidationFailure(writer, request, validationErr)
		return
	}
	if response.Status == pubstatus.RouteStatusRebuilding {
		writer.Header().Set("Cache-Control", "no-store")
		writeShellJSON(writer, http.StatusServiceUnavailable, response)
		return
	}
	writeShellJSON(writer, http.StatusOK, response)
}

// handleRootGetPublicStatus 作答根级 `/api/public-status`（route.ts:9-49）。
func (api *publicStatusReadAPI) handleRootGetPublicStatus(
	writer http.ResponseWriter,
	request *http.Request,
) {
	response, validationErr := api.buildResponse(request)
	if validationErr != nil {
		// 根级路由**不用** problem+json：Node 这里直接 `NextResponse.json({error, details})`。
		// 抄成 problem+json 会让现有的非 v1 调用方（状态页自取）解析失败。
		writeRawJSON(writer, http.StatusBadRequest, struct {
			Error   string                                  `json:"error"`
			Details []pubstatus.PublicStatusValidationIssue `json:"details"`
		}{
			Error:   validationErr.Error(),
			Details: validationErr.Issues,
		})
		return
	}
	if response.Status == pubstatus.RouteStatusRebuilding {
		writer.Header().Set("Cache-Control", "no-store")
		writeRawJSON(writer, http.StatusServiceUnavailable, response)
		return
	}
	writeRawJSON(writer, http.StatusOK, response)
}

// buildResponse 是两条端点共用的内核：读配置快照 → 解析查询 → 读投影 → 装配响应。
func (api *publicStatusReadAPI) buildResponse(
	request *http.Request,
) (pubstatus.PublicStatusRouteResponse, *pubstatus.PublicStatusQueryValidationError) {
	ctx := request.Context()
	configSnapshot := pubstatus.ReadCurrentConfigSnapshot(ctx, api.store)

	defaults := publicStatusReadDefaults(configSnapshot)
	query, validationErr := pubstatus.ParsePublicStatusQuery(request.URL.Query(), defaults)
	if validationErr != nil {
		return pubstatus.PublicStatusRouteResponse{}, validationErr
	}

	// rebuildReason 取**最后一次**触发的原因（Node 的闭包变量同判）：读路径可能在一次请求里
	// 触发多条提示（例如 rollup-coverage-incomplete 之后紧跟 legacy-generation），
	// 而 mapRouteStatus 只看最后一个——那正是「为什么这次没有 snapshot」的最贴近原因。
	var rebuildReason *string
	trigger := func(reason string) {
		rebuildReason = &reason
		result := pubstatus.SchedulePublicStatusRebuild(ctx, api.store, pubstatus.ScheduleRebuildInput{
			IntervalMinutes: query.IntervalMinutes,
			RangeHours:      query.RangeHours,
			Reason:          reason,
		})
		if !result.Accepted {
			api.logger.Warn("admin_public_status_rebuild_hint_rejected", map[string]any{
				"module": "public-status",
				"reason": reason,
			})
		}
	}

	var configVersion *string
	var hasConfiguredGroups *bool
	if configSnapshot != nil {
		version := configSnapshot.ConfigVersion
		configVersion = &version
		hasGroups := len(configSnapshot.Groups) > 0
		hasConfiguredGroups = &hasGroups
	}

	payload := pubstatus.ReadPublicStatusPayload(ctx, api.store, pubstatus.ReadPublicStatusPayloadInput{
		IntervalMinutes:     query.IntervalMinutes,
		RangeHours:          query.RangeHours,
		NowISO:              time.Now().UTC().Format(publicStatusISOMilli),
		ConfigVersion:       configVersion,
		HasConfiguredGroups: hasConfiguredGroups,
		TriggerRebuildHint:  trigger,
	})

	return pubstatus.BuildPublicStatusRouteResponse(pubstatus.PublicStatusRouteResponseInput{
		Payload:       payload,
		Query:         query,
		Defaults:      defaults,
		Meta:          publicStatusRouteMeta(configSnapshot),
		RebuildReason: rebuildReason,
	}), nil
}

// publicStatusReadDefaults 复刻 handlers.ts:30-33 的默认窗口。
//
// 与 Node 的一处**有意差异**（登记）：Node 用 `configSnapshot?.defaultIntervalMinutes ?? 5`，
// 即快照里若写着 `0` 会**照用 0**，随后在 `buildPublicStatusManifestKey` 里因
// `assertPositiveInteger` 抛错（→500）。Go 侧把 0 视为「缺席」并回退 5/24。
// 实践中不可达：发布侧 `NormalizePublicInterval/NormalizePublicRange` 保证写进去的必然是
// 合法值（5/15/30/60 与 1..168）。取更稳的一支，是因为「手改 Redis 写了个 0」不该让公开页 500。
func publicStatusReadDefaults(
	configSnapshot *pubstatus.PublicStatusConfigSnapshot,
) pubstatus.PublicStatusQueryDefaults {
	defaults := pubstatus.PublicStatusQueryDefaults{IntervalMinutes: 5, RangeHours: 24}
	if configSnapshot == nil {
		return defaults
	}
	if configSnapshot.DefaultIntervalMinutes > 0 {
		defaults.IntervalMinutes = configSnapshot.DefaultIntervalMinutes
	}
	if configSnapshot.DefaultRangeHours > 0 {
		defaults.RangeHours = configSnapshot.DefaultRangeHours
	}
	return defaults
}

// publicStatusRouteMeta 复刻 handlers.ts:52-58 的 meta 构造。
//
// 三处细节都要照抄：title/description `?.trim() || null`（空串归一为 null）、
// timeZone 用 `?? null`（**不 trim**，空串保持空串）、且**无快照时仍是对象**而非 null。
// 把无快照写成 meta:null 会让「include=meta」的调用方拿到 null 而不是三个 null 字段。
func publicStatusRouteMeta(
	configSnapshot *pubstatus.PublicStatusConfigSnapshot,
) *pubstatus.PublicStatusRouteMeta {
	meta := &pubstatus.PublicStatusRouteMeta{}
	if configSnapshot == nil {
		return meta
	}
	if trimmed := strings.TrimSpace(configSnapshot.SiteTitle); trimmed != "" {
		meta.SiteTitle = &trimmed
	}
	if trimmed := strings.TrimSpace(configSnapshot.SiteDescription); trimmed != "" {
		meta.SiteDescription = &trimmed
	}
	meta.TimeZone = configSnapshot.TimeZone
	return meta
}

// writeV1ValidationFailure 作答 v1 侧的 400。
//
// 形状照 Node 的 `createProblemResponse`（error-envelope.ts:32-51）：
// type 由 errorCode 派生（`urn:claude-code-hub:problem:<errorCode>`）、
// title 固定 "Validation failed"、detail 是**本条端点专有**的文案（不是 fromZodError 的
// "One or more fields are invalid."），并带上 invalidParams。
//
// 不复用 `Problems.WriteValidationError`：它的 errorCode 与 detail 是写死的
// `request.validation_failed` / "One or more fields are invalid."，与 Node 这条端点不符；
// 复用会把一处**可见的**契约差异藏进共享实现里。
func (api *publicStatusReadAPI) writeV1ValidationFailure(
	writer http.ResponseWriter,
	request *http.Request,
	validationErr *pubstatus.PublicStatusQueryValidationError,
) {
	invalidParams := make([]InvalidParam, 0, len(validationErr.Issues))
	for _, issue := range validationErr.Issues {
		invalidParams = append(invalidParams, InvalidParam{
			Path:    []any{issue.Field},
			Code:    issue.Code,
			Message: issue.Message,
		})
	}

	body := struct {
		Type          string         `json:"type"`
		Title         string         `json:"title"`
		Status        int            `json:"status"`
		Detail        string         `json:"detail"`
		Instance      string         `json:"instance"`
		ErrorCode     string         `json:"errorCode"`
		InvalidParams []InvalidParam `json:"invalidParams"`
	}{
		Type:          problemType(publicStatusInvalidQueryCode),
		Title:         "Validation failed",
		Status:        http.StatusBadRequest,
		Detail:        "One or more query parameters are invalid.",
		Instance:      problemInstance(request),
		ErrorCode:     publicStatusInvalidQueryCode,
		InvalidParams: invalidParams,
	}

	writer.Header().Set("Content-Type", problemContentType)
	writer.WriteHeader(http.StatusBadRequest)
	encoder := json.NewEncoder(writer)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(body)
}

const publicStatusInvalidQueryCode = "public_status.invalid_query"

// publicStatusISOMilli 是 Node `new Date().toISOString()` 的布局（毫秒精度 + Z）。
const publicStatusISOMilli = "2006-01-02T15:04:05.000Z"
