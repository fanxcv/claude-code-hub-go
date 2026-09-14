package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/jobs"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 provider-endpoints 资源的**写路径与拨测**八条
// （Node 侧 src/app/api/v1/resources/provider-endpoints/{router,handlers}.ts）：
//
//	PATCH  /provider-vendors/{vendorId}                 editProviderVendor
//	DELETE /provider-vendors/{vendorId}                 removeProviderVendor（204）
//	POST   /provider-vendors/{vendorId}/endpoints       addProviderEndpoint（201）
//	PATCH  /provider-endpoints/{endpointId}             editProviderEndpoint
//	DELETE /provider-endpoints/{endpointId}             removeProviderEndpoint（204）
//	POST   /provider-endpoints/{endpointId}:probe       probeProviderEndpoint（同步拨测）
//	POST   /provider-endpoints/probe-logs:batch         batchGetProbeLogs
//	POST   /provider-vendors/endpoint-stats:batch       batchGetVendorEndpointStats
//
// 为什么与读面分开注册：读面只要 PG；拨测那一条要**同步拨测能力**（jobs 的拨测 + 熔断写入器），
// 装配条件不同（缺拨测入口时只退那一条，不该把写路径一起退掉）。
//
// 四处与 Node 的对齐要点：
//  1. **错误码与状态码**。Node 这个资源**不走通用码表**：它的 actionError 按子串判定
//     （handlers.ts:439-455），而 A0 已裁决「子串判定不移植」。本文件因此**显式指定状态码**，
//     逐条按 Node 的子串规则的结果落定：NOT_FOUND→404、CONFLICT / ENDPOINT_REFERENCED_BY_ENABLED_
//     PROVIDERS→409、其余（CREATE_FAILED / UPDATE_FAILED / DELETE_FAILED / OPERATION_FAILED）→400。
//     注意这与 problem.go 的通用码表不同（那里把 CREATE_FAILED 等映射成 500）——通用码表服务的是
//     另一批资源，不能拿它代答本资源。
//  2. **唯一性冲突由库判定**（store.ErrProviderEndpointConflict ← pg 23505），Go 侧不先查后写。
//  3. **写后广播** `cfgsync.DomainProviders`（Node 的 publishProviderCacheInvalidation 发的就是
//     `cch:cache:providers:updated`；端点缓存复用同一条通道）。广播失败只记 warn（Node 同）。
//  4. **不发审计**：Node 这个资源的所有写 action 都不写 audit_log（实测 grep 无 emitAudit），
//     故这里也不发——凭「看起来该审计」补一条会污染审计列表。
//
// 两处登记差异：
//   - **未做 sticky 会话终止**。Node 在端点 URL/排序/启用态变更与端点删除后会调
//     `SessionManager.terminateStickySessionsForProviders`（session-manager.ts:3803）。Go 侧尚无
//     该能力（provider→active_sessions 的批量终止面未移植），故这几条写路径不带这一步：
//     已绑定的亲和会话会继续用旧端点直到亲和 TTL 到期。这是本文件与 Node 最大的行为差，
//     已在编排台账登记为待补项。
//   - **providerType 的取值域恒为公开四档**。Node 的**路由级** schema 就限死了
//     PUBLIC_PROVIDER_TYPE_VALUES（router.ts:51-70），handler 里那条「dashboard-compat 用内部
//     schema」的分支实际不可达；Go 侧照路由级的语义实现（隐藏类型一律 400 invalid_enum_value），
//     不复制那条死分支。

// EndpointProbeRunner 同步拨测一个端点（实现见 internal/jobs 的 ProbeOnceRunner）。
//
// 用窄接口而不是 *jobs.ProbeOnceRunner：管理面只该看到「拨一次」这一件事，而具体实现要带
// 连接池、Redis 与熔断写入器三样，把那个类型的构造与装配细节塞进管理面不合适。
type EndpointProbeRunner interface {
	// ProbeEndpoint 拨测一次并落库；source 是探活历史里的来源标签（手动拨测为 "manual"）。
	ProbeEndpoint(
		ctx context.Context,
		endpoint store.ProbeEndpoint,
		source string,
		timeoutMS int,
	) (jobs.ProbeOnceResult, error)
}

// providerEndpointProbeSourceManual 是手动拨测的来源标签（Node source: "manual"）。
const providerEndpointProbeSourceManual = "manual"

// providerEndpointProbeLogsBatchDefaultLimit 照 actions/provider-endpoints.ts:782 的 `?? 12`。
const providerEndpointProbeLogsBatchDefaultLimit = 12

// providerEndpointBatchMaxIDs 照 Batch*Schema 的 `.max(500)`。
const providerEndpointBatchMaxIDs = 500

// providerVendorMaxNameRunes 照 ProviderVendorUpdateSchema 的 `.max(200)`。
const providerVendorMaxNameRunes = 200

// providerEndpointMaxLabelRunes 照 ProviderEndpointCreate/UpdateSchema 的 `.max(200)`。
const providerEndpointMaxLabelRunes = 200

// providerEndpointProbeTimeoutMinMS / MaxMS 照 ProviderEndpointProbeSchema 的 1000..120000。
const (
	providerEndpointProbeTimeoutMinMS = 1000
	providerEndpointProbeTimeoutMaxMS = 120000
)

// providerEndpointActionError 按本资源的状态码归属作答（见文件头第 1 条）。
//
// status 显式传入而不是留 0：留 0 会落到 problem.go 的通用码表，那张表把 CREATE_FAILED 等
// 映射成 500，与本资源 Node 侧的 400 相抵。
func providerEndpointActionError(
	writer http.ResponseWriter,
	request *http.Request,
	deps Deps,
	code string,
	status int,
	params map[string]any,
	err error,
) {
	action := &ActionError{Resource: "provider_endpoint", Code: code, Status: status, Params: params}
	if err != nil {
		action.Err = err
	}
	adminProblemWriter(deps).WriteActionError(writer, request, action)
}

// registerProviderEndpointWriteRoutes 注册写路径与拨测（由 RegisterProviderEndpointRoutes 调用）。
func registerProviderEndpointWriteRoutes(router *Router, deps Deps) {
	if deps.Store == nil {
		return
	}
	api := &providerEndpointWriteAPI{
		pools:    deps.Store,
		deps:     deps,
		depts:    adminProblemWriter(deps),
		logger:   adminLoggerOf(deps),
		probes:   deps.EndpointProbes,
		circuits: deps.CircuitStates,
	}

	router.Add(Route{
		Method:      http.MethodPatch,
		Path:        "/provider-vendors/{vendorId}",
		Access:      AccessAdmin,
		Module:      "provider_endpoint",
		OperationID: "updateProviderVendor",
		Handler:     http.HandlerFunc(api.handleUpdateVendor),
	})
	router.Add(Route{
		Method:      http.MethodDelete,
		Path:        "/provider-vendors/{vendorId}",
		Access:      AccessAdmin,
		Module:      "provider_endpoint",
		OperationID: "deleteProviderVendor",
		Handler:     http.HandlerFunc(api.handleDeleteVendor),
	})
	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/provider-vendors/{vendorId}/endpoints",
		Access:      AccessAdmin,
		Module:      "provider_endpoint",
		OperationID: "createProviderEndpoint",
		Handler:     http.HandlerFunc(api.handleCreateEndpoint),
	})
	router.Add(Route{
		Method:      http.MethodPatch,
		Path:        "/provider-endpoints/{endpointId}",
		Access:      AccessAdmin,
		Module:      "provider_endpoint",
		OperationID: "updateProviderEndpoint",
		Handler:     http.HandlerFunc(api.handleUpdateEndpoint),
	})
	router.Add(Route{
		Method:      http.MethodDelete,
		Path:        "/provider-endpoints/{endpointId}",
		Access:      AccessAdmin,
		Module:      "provider_endpoint",
		OperationID: "deleteProviderEndpoint",
		Handler:     http.HandlerFunc(api.handleDeleteEndpoint),
	})
	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/provider-endpoints/probe-logs:batch",
		Access:      AccessAdmin,
		Module:      "provider_endpoint",
		OperationID: "batchListProviderEndpointProbeLogs",
		Handler:     http.HandlerFunc(api.handleBatchProbeLogs),
	})
	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/provider-vendors/endpoint-stats:batch",
		Access:      AccessAdmin,
		Module:      "provider_endpoint",
		OperationID: "batchGetVendorEndpointStats",
		Handler:     http.HandlerFunc(api.handleBatchVendorStats),
	})
	// 拨测这条要有同步拨测能力才注册：没有它时如实回退 Node，而不是答一个「探了但没落库」的假结果。
	//
	// 参数段带 [0-9]+ 约束，与 Node 逐字对齐（`/provider-endpoints/:endpointId{[0-9]+:probe}`，
	// src/app/api/v1/resources/provider-endpoints/router.ts）。无约束的 `{endpointId}` 会把
	// `/provider-endpoints/probe-logs:batch` 一类静态端点也接住——那正是
	// 记的静默分叉。
	if api.probes != nil {
		router.Add(Route{
			Method:      http.MethodPost,
			Path:        "/provider-endpoints/{endpointId:[0-9]+}:probe",
			Access:      AccessAdmin,
			Module:      "provider_endpoint",
			OperationID: "probeProviderEndpoint",
			Handler:     http.HandlerFunc(api.handleProbeEndpoint),
		})
	} else if deps.Logger != nil {
		deps.Logger.Warn("admin_provider_endpoint_probe_unwired", map[string]any{
			"module": "provider_endpoint",
			"reason": "同步拨测入口未装配",
			"effect": "probe_route_falls_back_to_node",
		})
	}
}

// providerEndpointWriteAPI 是写路径与拨测处理器的依赖。
type providerEndpointWriteAPI struct {
	pools    *store.Pools
	deps     Deps
	depts    ProblemWriter
	logger   *logx.Logger
	probes   EndpointProbeRunner
	circuits CircuitStateStore
}

// handleUpdateVendor 复刻 updateProviderVendor（actions/provider-endpoints.ts:1125）。
func (api *providerEndpointWriteAPI) handleUpdateVendor(writer http.ResponseWriter, request *http.Request) {
	vendorID, ok := providerPathID(writer, request, "vendorId")
	if !ok {
		return
	}
	fields, ok := api.readBody(writer, request)
	if !ok {
		return
	}
	object := adminNewObject(fields, "displayName", "websiteUrl")
	object.RejectUnknownKeys()

	displayName, displayPresent := providerNullableString(object, "displayName",
		providerVendorMaxNameRunes, []any{"displayName"})
	websiteURL, websitePresent := providerNullableString(object, "websiteUrl", 0, []any{"websiteUrl"})
	if websitePresent && websiteURL.Value != nil {
		providerEndpointCheckURL(object, "websiteUrl", *websiteURL.Value)
	}
	if issues := object.issues0(); len(issues) > 0 {
		adminWriteValidationFailure(writer, request, issues)
		return
	}

	patch := store.AdminProviderVendorPatch{}
	if displayPresent {
		patch.DisplayName = displayName
	}
	if websitePresent {
		patch.WebsiteURL = websiteURL
		// favicon 由 websiteUrl 推导（Node 的 editProviderVendor:1148-1160）：
		// 给了新域名就换成对应 favicon，清空或解析失败则一并置空。
		patch.FaviconURL = store.NullableString{Set: true, Value: providerFaviconURL(websiteURL.Value)}
	}

	vendor, err := api.pools.AdminUpdateProviderVendor(request.Context(), vendorID, patch)
	if errors.Is(err, store.ErrNotFound) {
		providerEndpointActionError(writer, request, api.deps, "NOT_FOUND", http.StatusNotFound, nil, err)
		return
	}
	if err != nil {
		api.logger.Error("admin_provider_vendor_update_failed", map[string]any{
			"vendorId": vendorID,
			"error":    err.Error(),
		})
		providerEndpointActionError(writer, request, api.deps, "UPDATE_FAILED", http.StatusBadRequest, nil, err)
		return
	}

	api.publishProviderInvalidation(request)
	adminWriteJSON(writer, http.StatusOK, providerVendorUpdateResponse{
		Vendor: newProviderVendorPayload(*vendor),
	})
}

// handleDeleteVendor 复刻 removeProviderVendor（actions/provider-endpoints.ts:1194）。
//
// Node 对「厂不存在或删不掉」回 DELETE_FAILED，而本资源的状态码规则把它判成 400（不是 404）——
// 这条不直观，但照抄：UI 依赖的是 errorCode 而不是状态码。
func (api *providerEndpointWriteAPI) handleDeleteVendor(writer http.ResponseWriter, request *http.Request) {
	vendorID, ok := providerPathID(writer, request, "vendorId")
	if !ok {
		return
	}
	deleted, err := api.pools.AdminDeleteProviderVendor(request.Context(), vendorID)
	if err != nil {
		api.logger.Error("admin_provider_vendor_delete_failed", map[string]any{
			"vendorId": vendorID,
			"error":    err.Error(),
		})
		providerEndpointActionError(writer, request, api.deps, "DELETE_FAILED", http.StatusBadRequest, nil, err)
		return
	}
	if !deleted {
		providerEndpointActionError(writer, request, api.deps, "DELETE_FAILED", http.StatusBadRequest, nil,
			errors.New("provider vendor not found"))
		return
	}
	api.publishProviderInvalidation(request)
	adminWriteNoContent(writer)
}

// handleCreateEndpoint 复刻 addProviderEndpoint（actions/provider-endpoints.ts:396）。
func (api *providerEndpointWriteAPI) handleCreateEndpoint(writer http.ResponseWriter, request *http.Request) {
	vendorID, ok := providerPathID(writer, request, "vendorId")
	if !ok {
		return
	}
	fields, ok := api.readBody(writer, request)
	if !ok {
		return
	}
	object := adminNewObject(fields, "providerType", "url", "label", "sortOrder", "isEnabled")
	object.RejectUnknownKeys()

	providerType, _ := object.String("providerType", adminStringSpec{
		Required: true,
		Enum:     publicProviderTypeValues,
		Trim:     true,
	})
	rawURL, _ := object.String("url", adminStringSpec{Required: true, Trim: true})
	if _, present := object.Raw("url"); present {
		providerEndpointCheckURL(object, "url", rawURL)
	}
	label, _ := providerNullableString(object, "label", providerEndpointMaxLabelRunes, []any{"label"})
	sortOrder, _ := providerNonNegativeInteger(object, "sortOrder")
	isEnabled, _ := object.Bool("isEnabled")
	if issues := object.issues0(); len(issues) > 0 {
		adminWriteValidationFailure(writer, request, issues)
		return
	}

	input := store.AdminProviderEndpointCreateInput{
		VendorID:     vendorID,
		ProviderType: providerType,
		URL:          rawURL,
		Label:        label.Value,
		IsEnabled:    isEnabled == nil || *isEnabled, // Node 的 `?? true`
	}
	if sortOrder != nil {
		input.SortOrder = *sortOrder
	}

	endpoint, err := api.pools.AdminCreateProviderEndpoint(request.Context(), input)
	if api.writeEndpointFailure(writer, request, err, "CONFLICT", "端点 URL 与同供应商类型下的其他端点冲突",
		"NOT_FOUND", "供应商不存在", "CREATE_FAILED") {
		return
	}

	api.publishProviderInvalidation(request)
	// 201 但**不带 Location 头**：Node 这条用的是 sanitizedActionJson → jsonResponse，
	// 不是 createdResponse（那个才会加 Location）。多一个头会进 A2 的对拍差异面。
	adminWriteJSON(writer, http.StatusCreated, providerEndpointWriteResponse{
		Endpoint: newProviderEndpointPayload(*endpoint),
	})
}

// handleUpdateEndpoint 复刻 editProviderEndpoint（actions/provider-endpoints.ts:461）。
func (api *providerEndpointWriteAPI) handleUpdateEndpoint(writer http.ResponseWriter, request *http.Request) {
	endpointID, ok := providerPathID(writer, request, "endpointId")
	if !ok {
		return
	}
	current, ok := api.visibleEndpoint(writer, request, endpointID)
	if !ok {
		return
	}
	fields, ok := api.readBody(writer, request)
	if !ok {
		return
	}
	object := adminNewObject(fields, "url", "label", "sortOrder", "isEnabled")
	object.RejectUnknownKeys()

	rawURL, urlPresent := object.String("url", adminStringSpec{Trim: true})
	if urlPresent {
		providerEndpointCheckURL(object, "url", rawURL)
	}
	label, labelPresent := providerNullableString(object, "label", providerEndpointMaxLabelRunes, []any{"label"})
	sortOrder, _ := providerNonNegativeInteger(object, "sortOrder")
	isEnabled, _ := object.Bool("isEnabled")
	if issues := object.issues0(); len(issues) > 0 {
		adminWriteValidationFailure(writer, request, issues)
		return
	}

	patch := store.AdminProviderEndpointPatch{}
	if urlPresent {
		value := rawURL
		patch.URL = &value
	}
	if labelPresent {
		patch.Label = label
	}
	patch.SortOrder = sortOrder
	patch.IsEnabled = isEnabled

	endpoint, err := api.pools.AdminUpdateProviderEndpoint(request.Context(), endpointID, patch)
	if errors.Is(err, store.ErrNotFound) {
		api.writeEndpointNotFound(writer, request)
		return
	}
	if api.writeEndpointFailure(writer, request, err, "CONFLICT", "端点 URL 与同供应商类型下的其他端点冲突",
		"NOT_FOUND", "供应商不存在", "UPDATE_FAILED") {
		return
	}

	// 换了 URL、或从停用改成启用时重置端点熔断态（Node editProviderEndpoint:520-535）。
	// 不重置会让「刚从 open 态恢复的端点」继续被拒到开闸到期。
	if (urlPresent && rawURL != current.URL) || (isEnabled != nil && *isEnabled && !current.IsEnabled) {
		api.resetEndpointCircuit(request, endpointID)
	}

	// 只要请求体给了这三个字段就终止（Node provider-endpoints.ts:528-536）：判存在不判变化——
	// 端点池的顺序/启停是数据面选路的输入，即使值没变也可能与上一轮读到的池子不一致。
	if patch.URL != nil || patch.SortOrder != nil || patch.IsEnabled != nil {
		adminTerminateStickySessionsForEndpoint(request, api.deps, current.VendorID, current.ProviderType,
			"editProviderEndpoint")
	}

	api.publishProviderInvalidation(request)
	adminWriteJSON(writer, http.StatusOK, providerEndpointWriteResponse{
		Endpoint: newProviderEndpointPayload(*endpoint),
	})
}

// handleDeleteEndpoint 复刻 removeProviderEndpoint（actions/provider-endpoints.ts:564）。
func (api *providerEndpointWriteAPI) handleDeleteEndpoint(writer http.ResponseWriter, request *http.Request) {
	endpointID, ok := providerPathID(writer, request, "endpointId")
	if !ok {
		return
	}
	current, ok := api.visibleEndpoint(writer, request, endpointID)
	if !ok {
		return
	}

	// 仍被启用的供应商引用时拒绝删除（Node：先查引用再删）。
	references, err := api.pools.AdminFindEnabledProviderReferencesForVendorTypeURL(
		request.Context(), current.VendorID, current.ProviderType, current.URL)
	if err != nil {
		api.logger.Error("admin_provider_endpoint_reference_check_failed", map[string]any{
			"endpointId": endpointID,
			"error":      err.Error(),
		})
		providerEndpointActionError(writer, request, api.deps, "DELETE_FAILED", http.StatusBadRequest, nil, err)
		return
	}
	if len(references) > 0 {
		providerEndpointActionError(writer, request, api.deps,
			"ENDPOINT_REFERENCED_BY_ENABLED_PROVIDERS", http.StatusConflict,
			map[string]any{
				"count":     len(references),
				"providers": providerReferenceSummary(references),
			}, nil)
		return
	}

	deleted, err := api.pools.AdminSoftDeleteProviderEndpoint(request.Context(), endpointID)
	if err != nil {
		api.logger.Error("admin_provider_endpoint_delete_failed", map[string]any{
			"endpointId": endpointID,
			"error":      err.Error(),
		})
		providerEndpointActionError(writer, request, api.deps, "DELETE_FAILED", http.StatusBadRequest, nil, err)
		return
	}
	if !deleted {
		api.writeEndpointNotFound(writer, request)
		return
	}

	api.resetEndpointCircuit(request, endpointID)

	// 删端点后无条件终止该厂该类型下已启用供应商的粘性会话
	// （Node provider-endpoints.ts:627-634）：这些供应商的亲和可能正指着刚删掉的地址。
	adminTerminateStickySessionsForEndpoint(request, api.deps, current.VendorID, current.ProviderType,
		"removeProviderEndpoint")

	// 厂里再无活跃供应商与端点时顺手删厂（Node 的 tryDeleteProviderVendorIfEmpty）。
	// 失败只记 warn：这一步是清理，不该让已经成功的删除变成错误。
	if _, err := api.pools.AdminDeleteProviderVendorIfEmpty(request.Context(), current.VendorID); err != nil {
		api.logger.Warn("admin_provider_vendor_cleanup_failed", map[string]any{
			"endpointId": endpointID,
			"vendorId":   current.VendorID,
			"error":      err.Error(),
		})
	}

	api.publishProviderInvalidation(request)
	adminWriteNoContent(writer)
}

// handleProbeEndpoint 复刻 probeProviderEndpoint（actions/provider-endpoints.ts:667）。
//
// 响应里的 endpoint 是**拨测前**读到的快照（Node 的 `endpoint` 变量在拨测前取），故 lastProbe*
// 五列是这次拨测之前的值；本次结果在 result 里。
func (api *providerEndpointWriteAPI) handleProbeEndpoint(writer http.ResponseWriter, request *http.Request) {
	endpointID, ok := providerPathID(writer, request, "endpointId")
	if !ok {
		return
	}
	current, ok := api.visibleEndpoint(writer, request, endpointID)
	if !ok {
		return
	}
	fields, ok := api.readBody(writer, request)
	if !ok {
		return
	}
	object := adminNewObject(fields, "timeoutMs")
	object.RejectUnknownKeys()
	timeoutMS, _ := providerIntegerInRange(object, "timeoutMs",
		providerEndpointProbeTimeoutMinMS, providerEndpointProbeTimeoutMaxMS)
	if issues := object.issues0(); len(issues) > 0 {
		adminWriteValidationFailure(writer, request, issues)
		return
	}

	requested := 0
	if timeoutMS != nil {
		requested = *timeoutMS
	}

	result, err := api.probes.ProbeEndpoint(request.Context(), store.ProbeEndpoint{
		ID:           current.ID,
		URL:          current.URL,
		VendorID:     current.VendorID,
		ProviderType: current.ProviderType,
	}, providerEndpointProbeSourceManual, requested)
	if err != nil {
		api.logger.Error("admin_provider_endpoint_probe_failed", map[string]any{
			"endpointId": endpointID,
			"error":      err.Error(),
		})
		providerEndpointActionError(writer, request, api.deps, "OPERATION_FAILED", http.StatusBadRequest, nil, err)
		return
	}

	adminWriteJSON(writer, http.StatusOK, providerEndpointProbeResponse{
		Endpoint: newProviderEndpointPayload(*current),
		Result: providerEndpointProbeResult{
			OK:           result.OK,
			Method:       result.Method,
			StatusCode:   result.StatusCode,
			LatencyMS:    result.LatencyMS,
			ErrorType:    result.ErrorType,
			ErrorMessage: result.ErrorMessage,
		},
	})
}

// handleBatchProbeLogs 复刻 batchGetProbeLogs（actions/provider-endpoints.ts:758）。
//
// 两条形状细节：**可见性逐 id 检查**（任一端点不存在或为隐藏类型即整批 404），
// 且响应是**裸数组**（Node 的 sanitizedActionJson 直接吐 result.data）。
func (api *providerEndpointWriteAPI) handleBatchProbeLogs(writer http.ResponseWriter, request *http.Request) {
	fields, ok := api.readBody(writer, request)
	if !ok {
		return
	}
	object := adminNewObject(fields, "endpointIds", "limit")
	object.RejectUnknownKeys()

	endpointIDs, _ := providerPositiveIDArray(object, "endpointIds", providerEndpointBatchMaxIDs, true)
	limit, _ := providerIntegerInRange(object, "limit", 1, 200)
	if issues := object.issues0(); len(issues) > 0 {
		adminWriteValidationFailure(writer, request, issues)
		return
	}

	ids := providerDedupeInt64(endpointIDs)
	for _, id := range ids {
		if _, ok := api.visibleEndpoint(writer, request, id); !ok {
			return
		}
	}

	limitPerEndpoint := providerEndpointProbeLogsBatchDefaultLimit
	if limit != nil {
		limitPerEndpoint = *limit
	}

	logsByEndpoint, err := api.pools.AdminProviderEndpointProbeLogsBatch(request.Context(), ids, limitPerEndpoint)
	if err != nil {
		api.logger.Error("admin_provider_endpoint_probe_logs_batch_failed", map[string]any{
			"error": err.Error(),
		})
		providerEndpointActionError(writer, request, api.deps, "OPERATION_FAILED", http.StatusBadRequest, nil, err)
		return
	}

	response := make([]providerEndpointProbeLogsEntry, 0, len(ids))
	for _, id := range ids {
		logs := logsByEndpoint[id]
		if logs == nil {
			logs = []store.AdminProviderEndpointProbeLog{}
		}
		response = append(response, providerEndpointProbeLogsEntry{EndpointID: id, Logs: logs})
	}
	adminWriteJSON(writer, http.StatusOK, response)
}

// handleBatchVendorStats 复刻 batchGetVendorEndpointStats（actions/provider-endpoints.ts:808）。
//
// 去重后**按首次出现的顺序**逐 id 作答，库里没有的厂给全 0（Node 的 `stats?.total ?? 0`）。
func (api *providerEndpointWriteAPI) handleBatchVendorStats(writer http.ResponseWriter, request *http.Request) {
	fields, ok := api.readBody(writer, request)
	if !ok {
		return
	}
	object := adminNewObject(fields, "vendorIds", "providerType")
	object.RejectUnknownKeys()

	vendorIDs, _ := providerPositiveIDArray(object, "vendorIds", providerEndpointBatchMaxIDs, true)
	providerType, _ := object.String("providerType", adminStringSpec{
		Required: true,
		Enum:     publicProviderTypeValues,
		Trim:     true,
	})
	if issues := object.issues0(); len(issues) > 0 {
		adminWriteValidationFailure(writer, request, issues)
		return
	}

	ids := providerDedupeInt64(vendorIDs)
	response := make([]store.AdminVendorTypeEndpointStats, 0, len(ids))
	if len(ids) > 0 {
		rows, err := api.pools.AdminVendorTypeEndpointStatsBatch(request.Context(), ids, providerType)
		if err != nil {
			api.logger.Error("admin_provider_vendor_stats_batch_failed", map[string]any{"error": err.Error()})
			providerEndpointActionError(writer, request, api.deps, "OPERATION_FAILED", http.StatusBadRequest, nil, err)
			return
		}
		byVendor := make(map[int64]store.AdminVendorTypeEndpointStats, len(rows))
		for _, row := range rows {
			byVendor[row.VendorID] = row
		}
		for _, id := range ids {
			stats, found := byVendor[id]
			if !found {
				stats = store.AdminVendorTypeEndpointStats{VendorID: id}
			}
			stats.VendorID = id
			response = append(response, stats)
		}
	}
	adminWriteJSON(writer, http.StatusOK, response)
}

// visibleEndpoint 读一个端点的**可见性视图**（不存在或隐藏类型即 404）。
//
// 隐藏类型的判定只在非 dashboard-compat 请求下生效（Node 的 ensureVisibleEndpoint 同）。
func (api *providerEndpointWriteAPI) visibleEndpoint(
	writer http.ResponseWriter,
	request *http.Request,
	endpointID int64,
) (*store.AdminProviderEndpoint, bool) {
	endpoint, err := api.pools.AdminGetProviderEndpointByID(request.Context(), endpointID)
	if errors.Is(err, store.ErrNotFound) {
		api.writeEndpointNotFound(writer, request)
		return nil, false
	}
	if err != nil {
		api.logger.Error("admin_provider_endpoint_read_failed", map[string]any{
			"endpointId": endpointID,
			"error":      err.Error(),
		})
		api.depts.WriteProblem(writer, request, http.StatusInternalServerError, "", "")
		return nil, false
	}
	if !dashboardCompatRequest(request) && hiddenProviderType(endpoint.ProviderType) {
		api.writeEndpointNotFound(writer, request)
		return nil, false
	}
	return endpoint, true
}

// writeEndpointNotFound 按 Node 的 notFound 作答（errorCode 是带域的 provider_endpoint.not_found）。
func (api *providerEndpointWriteAPI) writeEndpointNotFound(writer http.ResponseWriter, request *http.Request) {
	api.depts.WriteProblem(writer, request, http.StatusNotFound,
		"provider_endpoint.not_found", "Provider endpoint was not found.")
}

// writeEndpointFailure 归并端点写失败的三种归宿（唯一性冲突 / 外键缺失 / 其余按 Node 的兜底码）。
//
// 返回 true 表示已作答。冲突与外键是库判定的（store 的两个哨兵错误），其余一律 400 —— 与 Node
// 本资源的 actionError 子串规则结果一致（见文件头第 1 条）。
func (api *providerEndpointWriteAPI) writeEndpointFailure(
	writer http.ResponseWriter,
	request *http.Request,
	err error,
	conflictCode, conflictMessage string,
	missingCode, missingMessage string,
	fallbackCode string,
) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrProviderEndpointConflict):
		api.logger.Warn("admin_provider_endpoint_duplicate_url", map[string]any{"path": request.URL.Path})
		providerEndpointActionError(writer, request, api.deps, conflictCode, http.StatusConflict, nil,
			errors.New(conflictMessage))
		return true
	case errors.Is(err, store.ErrProviderVendorMissing), errors.Is(err, store.ErrNotFound):
		providerEndpointActionError(writer, request, api.deps, missingCode, http.StatusNotFound, nil,
			errors.New(missingMessage))
		return true
	default:
		api.logger.Error("admin_provider_endpoint_write_failed", map[string]any{
			"path":  request.URL.Path,
			"error": err.Error(),
		})
		providerEndpointActionError(writer, request, api.deps, fallbackCode, http.StatusBadRequest, nil, err)
		return true
	}
}

// resetEndpointCircuit 清端点熔断态；失败只记 warn（Node 的两处 try/catch 同）。
func (api *providerEndpointWriteAPI) resetEndpointCircuit(request *http.Request, endpointID int64) {
	if api.circuits == nil {
		return
	}
	if err := api.circuits.ResetEndpointCircuit(request.Context(), endpointID); err != nil {
		api.logger.Warn("admin_provider_endpoint_circuit_reset_failed", map[string]any{
			"endpointId": endpointID,
			"error":      err.Error(),
		})
	}
}

// publishProviderInvalidation 广播 provider 域失效；失败只记 warn（Node 的 catch-并-warn 同）。
func (api *providerEndpointWriteAPI) publishProviderInvalidation(request *http.Request) {
	if api.deps.Invalidator == nil {
		return
	}
	api.deps.Invalidator.PublishDomain(request.Context(), cfgsync.DomainProviders)
}

// readBody 读 JSON 对象（顶层必须是对象；Content-Type 必须是 application/json）。
func (api *providerEndpointWriteAPI) readBody(
	writer http.ResponseWriter,
	request *http.Request,
) (map[string]json.RawMessage, bool) {
	return adminReadJSONObject(writer, request, api.deps)
}

// providerFaviconURL 复刻 favicon 推导（actions/provider-endpoints.ts:1148-1160）。
//
// 给了合法 URL 就用它的 hostname 拼 Google 的 favicon 服务；清空或解析失败一律 null。
func providerFaviconURL(websiteURL *string) *string {
	if websiteURL == nil {
		return nil
	}
	parsed, err := url.Parse(*websiteURL)
	if err != nil || parsed.Hostname() == "" {
		return nil
	}
	value := "https://www.google.com/s2/favicons?domain=" + parsed.Hostname() + "&sz=32"
	return &value
}

// providerReferenceSummary 复刻 formatProviderReferenceSummary：最多列 3 个去重名，其余计成 +N。
func providerReferenceSummary(references []store.AdminProviderReference) string {
	names := make([]string, 0, len(references))
	seen := make(map[string]struct{}, len(references))
	for _, reference := range references {
		name := strings.TrimSpace(reference.Name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	if len(names) == 0 {
		return ""
	}
	const maxDisplayCount = 3
	if len(names) <= maxDisplayCount {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:maxDisplayCount], ", ") +
		fmt.Sprintf(" +%d", len(names)-maxDisplayCount)
}

// providerDedupeInt64 保序去重（Node 的 Array.from(new Set(...))）。
func providerDedupeInt64(values []int64) []int64 {
	seen := make(map[int64]struct{}, len(values))
	result := make([]int64, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

// publicProviderTypeValues 是 PUBLIC_PROVIDER_TYPE_VALUES（constants.ts:9-14）。
var publicProviderTypeValues = []string{"claude", "codex", "gemini", "openai-compatible"}

// providerEndpointPayloadResponse / providerEndpointWriteResponse 是写路径的成功体。
type providerEndpointWriteResponse struct {
	Endpoint providerEndpointPayload `json:"endpoint"`
}

// providerVendorUpdateResponse 是 PATCH 厂的成功体（Node 的 `{vendor}`）。
type providerVendorUpdateResponse struct {
	Vendor providerVendorPayload `json:"vendor"`
}

// providerEndpointProbeResponse 是拨测的成功体（Node 的 `{endpoint, result}`）。
type providerEndpointProbeResponse struct {
	Endpoint providerEndpointPayload     `json:"endpoint"`
	Result   providerEndpointProbeResult `json:"result"`
}

// providerEndpointProbeResult 是拨测结果（Node 的 EndpointProbeResult）。
type providerEndpointProbeResult struct {
	OK           bool    `json:"ok"`
	Method       string  `json:"method"`
	StatusCode   *int    `json:"statusCode"`
	LatencyMS    *int    `json:"latencyMs"`
	ErrorType    *string `json:"errorType"`
	ErrorMessage *string `json:"errorMessage"`
}

// providerEndpointProbeLogsEntry 是批量探活历史的一项（Node 的 `{endpointId, logs}`）。
type providerEndpointProbeLogsEntry struct {
	EndpointID int64                                 `json:"endpointId"`
	Logs       []store.AdminProviderEndpointProbeLog `json:"logs"`
}

// providerEndpointCheckURL 复刻 zod 的 `.url()`：必须是绝对 URL（有 scheme 与 host）。
//
// 只报 invalid_string（zod 的 url 校验码），文案取 zod 的默认 "Invalid url"。
func providerEndpointCheckURL(o *adminObject, key string, value string) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		o.fail([]any{key}, "invalid_string", "Invalid url")
	}
}

// providerNonNegativeInteger 读一个非负整数字段（zod 的 `.number().int().min(0).optional()`）。
func providerNonNegativeInteger(o *adminObject, key string) (*int, bool) {
	return providerIntegerInRange(o, key, 0, 0)
}

// providerIntegerInRange 读一个整数字段；max 为 0 表示不设上界。
//
// 与 adminObject.Number 的差别：那条是给价格用的浮点读法，这里要的是 `.int()` 的语义
// （1.5 不合法，报 invalid_type 而不是 too_big）。
func providerIntegerInRange(o *adminObject, key string, min, max int) (*int, bool) {
	raw, present := o.fields[key]
	if !present {
		return nil, false
	}
	if adminJSONTypeName(raw) != "number" {
		o.fail([]any{key}, "invalid_type", adminTypeMessage("number", raw))
		return nil, false
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		o.fail([]any{key}, "invalid_type", adminTypeMessage("number", raw))
		return nil, false
	}
	if value != float64(int64(value)) {
		o.fail([]any{key}, "invalid_type", "Expected integer, received float")
		return nil, false
	}
	parsed := int(value)
	if parsed < min {
		o.fail([]any{key}, "too_small", fmt.Sprintf("Number must be greater than or equal to %d", min))
		return &parsed, true
	}
	if max > 0 && parsed > max {
		o.fail([]any{key}, "too_big", fmt.Sprintf("Number must be less than or equal to %d", max))
		return &parsed, true
	}
	return &parsed, true
}

// providerPositiveIDArray 读一个正整数数组字段（zod 的 `.array(z.number().int().positive()).max(n)`）。
//
// required 为 false 时字段缺席返回空数组（Node 的 optional 语义）。
func providerPositiveIDArray(o *adminObject, key string, maxItems int, required bool) ([]int64, bool) {
	raw, present := o.fields[key]
	if !present {
		if required {
			o.fail([]any{key}, "invalid_type", "Required")
		}
		return nil, false
	}
	if adminJSONTypeName(raw) != "array" {
		o.fail([]any{key}, "invalid_type", adminTypeMessage("array", raw))
		return nil, false
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		o.fail([]any{key}, "invalid_type", adminTypeMessage("array", raw))
		return nil, false
	}
	if len(items) > maxItems {
		o.fail([]any{key}, "too_big", fmt.Sprintf("Array must contain at most %d element(s)", maxItems))
		return nil, true
	}
	result := make([]int64, 0, len(items))
	for index, item := range items {
		if adminJSONTypeName(item) != "number" {
			o.fail([]any{key, index}, "invalid_type", adminTypeMessage("number", item))
			continue
		}
		var value float64
		if err := json.Unmarshal(item, &value); err != nil {
			o.fail([]any{key, index}, "invalid_type", adminTypeMessage("number", item))
			continue
		}
		if value != float64(int64(value)) {
			o.fail([]any{key, index}, "invalid_type", "Expected integer, received float")
			continue
		}
		if int64(value) <= 0 {
			o.fail([]any{key, index}, "too_small", "Number must be greater than 0")
			continue
		}
		result = append(result, int64(value))
	}
	return result, true
}
