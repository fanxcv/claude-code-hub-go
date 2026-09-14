package adminapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/jobs"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 provider-endpoints 资源（Node src/app/api/v1/resources/provider-endpoints/*）的 Go 落点，
// 以及两条根级价格读端点（/api/prices、/api/prices/vendors）。
//
// **两族熔断状态端点不在本文件**：它们要 Redis（状态读写面），与本文件的纯 PG 读面装配条件不同，
// 单独落在 provider_circuit_admin.go 与 RegisterProviderCircuitRoutes（六条）。
//
// **本批实现的是读面**（Node 该资源 18 条里的 4 条读 + 2 条价格读，加另一文件的 6 条熔断 = 已 12）。
// 未实现的 8 条如实登记在文件末的「未实现清单」里——它们要么需要外部拨测（`:probe`、batch 探活日志、
// 端点统计），要么需要 actions 层的一整套写校验（vender/endpoint 的增删改）。登记而不是静默回退，
// 是为了让切流矩阵能按条决策。
//
// 三条形状纪律：
//  1. **dashboard 兼容头**：`X-CCH-Dashboard-Compat: 1`（且当前身份是管理员）时返回**内部**口径
//     （含 claude-auth / gemini-cli 两个隐藏类型），否则过滤掉隐藏类型。隐藏类型是「有内部用途但
//     不在公开面暴露」的档位，漏过滤等于把它们泄露到普通管理页面。
//  2. **URL 凭据脱敏**：响应里任何 `url` / `websiteUrl` 属性都要过 providerRedactURLCredentials
//     （Node 的 sanitizeProviderEndpointData 对这两个键名做同样处理）。
//  3. **列表包 items**：读取多条时一律 `{"items":[...]}`（Node 的 ProviderEndpointArrayResponseSchema）。
const (
	// dashboardCompatHeader 照 src/lib/api/v1/_shared/constants.ts:6。
	dashboardCompatHeader = "X-CCH-Dashboard-Compat"
	// providerTypeClaudeAuth / providerTypeGeminiCLI 是 HIDDEN_PROVIDER_TYPES（constants.ts:14）。
	providerTypeClaudeAuth = "claude-auth"
	providerTypeGeminiCLI  = "gemini-cli"
	// providerVendorListLimit 照 actions 的 findProviderVendors(200, 0)。
	providerVendorListLimit = 200
	// providerProbeLogsLimitDefault / Min / Max 照 ProviderProbeLogsQuerySchema。
	providerProbeLogsLimitDefault = 200
	providerProbeLogsLimitMax     = 1000
	// pricePageSizeDefault / Max 照 /api/prices 的解析（与 v1 资源面不同：那里是 20/100）。
	pricePageSizeDefault = 50
	pricePageSizeMax     = 200
)

// providerEndpointAPI 是本资源处理器的依赖。
type providerEndpointAPI struct {
	pools  *store.Pools
	depts  ProblemWriter
	logger *logx.Logger
}

// RegisterProviderEndpointRoutes 注册读面路由；Store 未装配时整组不注册（回退 Node）。
//
// 模块名取 provider_endpoint：与 Node 的 tag「Provider Endpoints」同名，便于日志与路由表对齐。
func RegisterProviderEndpointRoutes(router *Router, deps Deps) {
	if deps.Store == nil {
		if deps.Logger != nil {
			deps.Logger.Error("admin_provider_endpoint_store_unwired", map[string]any{
				"module": "provider_endpoint",
				"action": "routes_not_registered",
			})
		}
		return
	}
	api := &providerEndpointAPI{
		pools:  deps.Store,
		depts:  adminProblemWriter(deps),
		logger: adminLoggerOf(deps),
	}

	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/provider-vendors",
		Access:      AccessAdmin,
		Module:      "provider_endpoint",
		OperationID: "listProviderVendors",
		Handler:     http.HandlerFunc(api.handleListVendors),
	})
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/provider-vendors/{vendorId}",
		Access:      AccessAdmin,
		Module:      "provider_endpoint",
		OperationID: "getProviderVendor",
		Handler:     http.HandlerFunc(api.handleGetVendor),
	})
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/provider-vendors/{vendorId}/endpoints",
		Access:      AccessAdmin,
		Module:      "provider_endpoint",
		OperationID: "listProviderEndpoints",
		Handler:     http.HandlerFunc(api.handleListEndpoints),
	})
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/provider-endpoints/{endpointId}/probe-logs",
		Access:      AccessAdmin,
		Module:      "provider_endpoint",
		OperationID: "getProviderEndpointProbeLogs",
		Handler:     http.HandlerFunc(api.handleListProbeLogs),
	})

	// 写路径与拨测：与读面同一 resource，但装配条件多一样（拨测那条要同步拨测入口），
	// 故在自己的文件里逐条判定（见 provider_endpoints_write.go 的文件头）。
	registerProviderEndpointWriteRoutes(router, deps)

	registerPriceRootRoutes(router, deps)
}

// registerPriceRootRoutes 注册两条根级价格读端点。
//
// 它们与 /api/v1/model-prices 不是同一条路由：Node 侧这是「价格页与设置页自己 fetch 的私有端点」，
// 默认每页 50（上限 200）、成功体是 `{ok:true,data:{...}}`；v1 资源面是每页 20（上限 100）、无 ok 包裹。
func registerPriceRootRoutes(router *Router, deps Deps) {
	if deps.Store == nil {
		return
	}
	api := &providerEndpointAPI{
		pools:  deps.Store,
		depts:  adminProblemWriter(deps),
		logger: adminLoggerOf(deps),
	}
	router.Add(Route{
		Method:               http.MethodGet,
		Path:                 "/api/prices",
		Access:               AccessAdmin,
		Module:               "model_price",
		OperationID:          "getRootPrices",
		NoManagementEnvelope: true,
		Handler:              http.HandlerFunc(api.handleRootPrices),
	})
	router.Add(Route{
		Method:               http.MethodGet,
		Path:                 "/api/prices/vendors",
		Access:               AccessAdmin,
		Module:               "model_price",
		OperationID:          "getRootPriceVendors",
		NoManagementEnvelope: true,
		Handler:              http.HandlerFunc(api.handleRootPriceVendors),
	})
}

// adminLoggerOf 取日志器，缺省给一个可用实例（logx 的方法在 nil 接收者上会 panic）。
func adminLoggerOf(deps Deps) *logx.Logger {
	if deps.Logger != nil {
		return deps.Logger
	}
	return logx.New(nil)
}

// dashboardCompatRequest 复刻 isDashboardCompatRequest：必须同时带兼容头且当前身份是管理员。
func dashboardCompatRequest(request *http.Request) bool {
	if request.Header.Get(dashboardCompatHeader) != "1" {
		return false
	}
	principal, ok := PrincipalFrom(request.Context())
	return ok && principal.IsAdmin
}

// hiddenProviderType 复刻 isHiddenProviderType。
func hiddenProviderType(providerType string) bool {
	return providerType == providerTypeClaudeAuth || providerType == providerTypeGeminiCLI
}

// handleListVendors 复刻 listProviderVendors（非 dashboard 口径：findProviderVendors(200, 0)）。
//
// Node 的 dashboard 分支走的是「有启用供应商的 vendor+type 对」推导，那需要额外的
// findEnabledProviderVendorTypePairs 查询；本批只实现默认分支（无 dashboard 头），dashboard 分支
// 登记为未实现——见文件末清单。
func (api *providerEndpointAPI) handleListVendors(writer http.ResponseWriter, request *http.Request) {
	if dashboardCompatRequest(request) {
		api.handleDashboardVendors(writer, request)
		return
	}

	vendors, err := api.pools.AdminListProviderVendors(request.Context(), providerVendorListLimit, 0)
	if err != nil {
		api.writeReadFailure(writer, request, err)
		return
	}
	items := make([]providerVendorPayload, 0, len(vendors))
	for _, vendor := range vendors {
		items = append(items, newProviderVendorPayload(vendor))
	}
	writeShellJSONNoEnvelope(writer, http.StatusOK, providerItemsResponse[providerVendorPayload]{Items: items})
}

// dashboardProviderTypeOrder 是 providerTypes 的排序依据，逐项照抄 Node 的
// `ProviderTypeSchema.options`（src/actions/provider-endpoints.ts:58）：
// claude, claude-auth, codex, gemini-cli, gemini, openai-compatible。
//
// **必须显式写死，不能靠 convert 包的常量声明顺序**：Go 侧常量的出现次序与之不同
// （openai-compatible 在 gemini 之前），照声明顺序会得出相反的相对次序。
var dashboardProviderTypeOrder = map[string]int{
	"claude":            0,
	"claude-auth":       1,
	"codex":             2,
	"gemini-cli":        3,
	"gemini":            4,
	"openai-compatible": 5,
}

// dashboardProviderTypeUnknown 是未知类型的排序位（Node 用 999）。
const dashboardProviderTypeUnknown = 999

// handleDashboardVendors 复刻 getDashboardProviderVendors（src/actions/provider-endpoints.ts:265）。
//
// 与默认分支的区别（这正是「添加供应商」表单需要的口径）：
//   - 只列**真有启用供应商**的 vendor（默认分支列全部 vendor）；
//   - 每个 vendor 多一个 `providerTypes` 字段（它启用中的供应商类型，按 Node 枚举顺序）；
//   - 空集时返回 `{items:[]}` 而不是 [null。
//
// 与默认分支**不**区分的：隐藏类型（claude-auth / gemini-cli）在这里不过滤——
// Node 的 getDashboardProviderVendors 也不过滤，而 isHiddenProviderType 只用在端点列表路径上。
func (api *providerEndpointAPI) handleDashboardVendors(writer http.ResponseWriter, request *http.Request) {
	pairs, err := api.pools.AdminFindEnabledProviderVendorTypePairs(request.Context())
	if err != nil {
		api.writeReadFailure(writer, request, err)
		return
	}

	// 聚合（依 SQL 的 vendorId 升序，与 Node 的 Map 插入序一致），并复刻 Node 的
	// map + filter：非正 vendorId 与空 providerType 都不算数。
	typesByVendor := make(map[int64]map[string]struct{}, len(pairs))
	vendorIDs := make([]int64, 0, len(pairs))
	for _, pair := range pairs {
		if pair.VendorID <= 0 || pair.ProviderType == "" {
			continue
		}
		set, ok := typesByVendor[pair.VendorID]
		if !ok {
			set = make(map[string]struct{}, 2)
			typesByVendor[pair.VendorID] = set
			vendorIDs = append(vendorIDs, pair.VendorID)
		}
		set[pair.ProviderType] = struct{}{}
	}

	// vendorIds 为空时 Node 直接返回空数组（不再查库）。
	if len(vendorIDs) == 0 {
		writeShellJSONNoEnvelope(writer, http.StatusOK, providerItemsResponse[providerVendorPayload]{
			Items: []providerVendorPayload{},
		})
		return
	}

	vendors, err := api.pools.AdminListProviderVendorsByIDs(request.Context(), vendorIDs)
	if err != nil {
		api.writeReadFailure(writer, request, err)
		return
	}

	items := make([]providerVendorPayload, 0, len(vendors))
	for _, vendor := range vendors {
		types := sortedDashboardProviderTypes(typesByVendor[vendor.ID])
		// Node 在 map 之后 filter 掉 providerTypes 为空的 vendor。
		if len(types) == 0 {
			continue
		}
		payload := newProviderVendorPayload(vendor)
		payload.ProviderTypes = types
		items = append(items, payload)
	}
	writeShellJSONNoEnvelope(writer, http.StatusOK, providerItemsResponse[providerVendorPayload]{Items: items})
}

// sortedDashboardProviderTypes 把类型集合排成 Node 枚举顺序（未知类型排最后）。
//
// 未知类型并列时再按字典序收敛，保证输出确定（Node 对并列不保证顺序，但确定的输出才能被用例钉住）。
func sortedDashboardProviderTypes(set map[string]struct{}) []string {
	if len(set) == 0 {
		return []string{}
	}
	types := make([]string, 0, len(set))
	for value := range set {
		types = append(types, value)
	}
	sort.Slice(types, func(left, right int) bool {
		leftOrder, leftKnown := dashboardProviderTypeOrder[types[left]]
		if !leftKnown {
			leftOrder = dashboardProviderTypeUnknown
		}
		rightOrder, rightKnown := dashboardProviderTypeOrder[types[right]]
		if !rightKnown {
			rightOrder = dashboardProviderTypeUnknown
		}
		if leftOrder != rightOrder {
			return leftOrder < rightOrder
		}
		return types[left] < types[right]
	})
	return types
}

// handleGetVendor 复刻 getProviderVendor（404 用 problem 形状）。
func (api *providerEndpointAPI) handleGetVendor(writer http.ResponseWriter, request *http.Request) {
	vendorID, ok := providerPathID(writer, request, "vendorId")
	if !ok {
		return
	}
	vendor, err := api.pools.AdminGetProviderVendorByID(request.Context(), vendorID)
	if errors.Is(err, store.ErrNotFound) {
		api.writeNotFound(writer, request, "provider_vendor.not_found", "Provider vendor was not found.")
		return
	}
	if err != nil {
		api.writeReadFailure(writer, request, err)
		return
	}
	writeShellJSONNoEnvelope(writer, http.StatusOK, newProviderVendorPayload(*vendor))
}

// handleListEndpoints 复刻 listProviderEndpoints：给了 providerType 就按类型过滤，否则按厂全取。
func (api *providerEndpointAPI) handleListEndpoints(writer http.ResponseWriter, request *http.Request) {
	vendorID, ok := providerPathID(writer, request, "vendorId")
	if !ok {
		return
	}
	providerType := strings.TrimSpace(request.URL.Query().Get("providerType"))
	if providerType != "" && !validProviderTypeValue(providerType) {
		api.depts.WriteValidationError(writer, request, []InvalidParam{{
			Path:    []any{"providerType"},
			Code:    "invalid_enum_value",
			Message: "Invalid enum value",
		}})
		return
	}

	ctx := request.Context()
	var (
		endpoints []store.AdminProviderEndpoint
		err       error
	)
	switch {
	case providerType != "" && dashboardCompatRequest(request):
		endpoints, err = api.pools.AdminListDashboardProviderEndpointsByVendorAndType(ctx, vendorID, providerType)
	case providerType != "":
		endpoints, err = api.pools.AdminListProviderEndpointsByVendorAndType(ctx, vendorID, providerType)
	default:
		endpoints, err = api.pools.AdminListProviderEndpointsByVendor(ctx, vendorID)
	}
	if err != nil {
		api.writeReadFailure(writer, request, err)
		return
	}

	dashboard := dashboardCompatRequest(request)
	items := make([]providerEndpointPayload, 0, len(endpoints))
	for _, endpoint := range endpoints {
		if !dashboard && hiddenProviderType(endpoint.ProviderType) {
			continue
		}
		items = append(items, newProviderEndpointPayload(endpoint))
	}
	writeShellJSONNoEnvelope(writer, http.StatusOK, providerItemsResponse[providerEndpointPayload]{Items: items})
}

// handleListProbeLogs 复刻 getProviderEndpointProbeLogs：端点不可见（不存在或隐藏类型）即 404。
func (api *providerEndpointAPI) handleListProbeLogs(writer http.ResponseWriter, request *http.Request) {
	endpointID, ok := providerPathID(writer, request, "endpointId")
	if !ok {
		return
	}
	limit, offset, ok := providerProbeLogsPaging(writer, request)
	if !ok {
		return
	}

	ctx := request.Context()
	endpoint, err := api.pools.AdminGetProviderEndpointByID(ctx, endpointID)
	if errors.Is(err, store.ErrNotFound) {
		api.writeNotFound(writer, request, "provider_endpoint.not_found", "Provider endpoint was not found.")
		return
	}
	if err != nil {
		api.writeReadFailure(writer, request, err)
		return
	}
	if !dashboardCompatRequest(request) && hiddenProviderType(endpoint.ProviderType) {
		api.writeNotFound(writer, request, "provider_endpoint.not_found", "Provider endpoint was not found.")
		return
	}

	logs, err := api.pools.AdminListProviderEndpointProbeLogs(ctx, endpointID, limit, offset)
	if err != nil {
		api.writeReadFailure(writer, request, err)
		return
	}
	writeShellJSONNoEnvelope(writer, http.StatusOK, providerProbeLogsResponse{
		EndpointID: endpointID,
		Logs:       logs,
	})
}

// handleRootPrices 复刻 GET /api/prices：默认每页 50（上限 200），成功体带 ok 包裹。
func (api *providerEndpointAPI) handleRootPrices(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	page, hasPage := priceQueryInt(query.Get("page"))
	if hasPage && page < 1 {
		writeShellJSONNoEnvelope(writer, http.StatusBadRequest,
			map[string]any{"ok": false, "error": "页码必须大于0"})
		return
	}
	if !hasPage {
		page = 1
	}

	pageSizeRaw := query.Get("pageSize")
	if pageSizeRaw == "" {
		pageSizeRaw = query.Get("size")
	}
	pageSize, hasPageSize := priceQueryInt(pageSizeRaw)
	if hasPageSize && (pageSize < 1 || pageSize > pricePageSizeMax) {
		writeShellJSONNoEnvelope(writer, http.StatusBadRequest,
			map[string]any{"ok": false, "error": "每页大小必须在1-200之间"})
		return
	}
	if !hasPageSize {
		pageSize = pricePageSizeDefault
	}

	source := query.Get("source")
	if source != "" && source != "manual" && source != "cloud" && source != "litellm" {
		writeShellJSONNoEnvelope(writer, http.StatusBadRequest,
			map[string]any{"ok": false, "error": "source 参数无效"})
		return
	}

	rows, total, err := api.pools.AdminListModelPricesPaginated(request.Context(), store.AdminModelPriceQuery{
		Page:            page,
		PageSize:        pageSize,
		Search:          query.Get("search"),
		Source:          source,
		Vendor:          query.Get("vendor"),
		LitellmProvider: query.Get("litellmProvider"),
	})
	if err != nil {
		api.logger.Error("admin_root_prices_query_failed", map[string]any{"error": err.Error()})
		writeShellJSONNoEnvelope(writer, http.StatusInternalServerError,
			map[string]any{"ok": false, "error": "服务器内部错误"})
		return
	}

	items := make([]modelPriceItem, 0, len(rows))
	for _, row := range rows {
		items = append(items, modelPricePayload(row))
	}
	totalPages := int64(0)
	if pageSize > 0 {
		totalPages = (total + int64(pageSize) - 1) / int64(pageSize)
	}
	writeShellJSONNoEnvelope(writer, http.StatusOK, priceRootResponse{
		OK: true,
		Data: modelPricePaginatedResult{
			Data:       items,
			Total:      total,
			Page:       page,
			PageSize:   pageSize,
			TotalPages: totalPages,
		},
	})
}

// handleRootPriceVendors 复刻 GET /api/prices/vendors（src/app/api/prices/vendors/route.ts:25-57）。
//
// 两条分支，与 Node 同序：
//  1. **优先分支**：cloud_pricing_catalog 存在且 vendors 是非空数组时，原样透出该列 + catalog.version；
//  2. **降级分支**：否则按 model_prices.price_data->>'vendor' 去重统计（version 为 null）。
//
// 目录读取失败与 Node 同判：记 warn 后走降级分支（Node 的 getCloudPricingCatalog 内部 catch 返回
// null，不把错误抛给路由），而不是答 500。
func (api *providerEndpointAPI) handleRootPriceVendors(writer http.ResponseWriter, request *http.Request) {
	catalog, err := api.pools.GetCloudPricingCatalog(request.Context())
	if err != nil {
		api.logger.Warn("admin_root_price_vendors_catalog_failed", map[string]any{"error": err.Error()})
	}
	if catalog != nil && len(catalog.Vendors) > 0 {
		var vendors []json.RawMessage
		if err := json.Unmarshal(catalog.Vendors, &vendors); err == nil && len(vendors) > 0 {
			version := catalog.Version
			writeShellJSONNoEnvelope(writer, http.StatusOK, priceVendorsRootResponse{
				OK:   true,
				Data: priceVendorsData{Vendors: json.RawMessage(catalog.Vendors), Version: &version},
			})
			return
		}
	}

	vendors, err := api.pools.AdminListCloudVendorSummaries(request.Context())
	if err != nil {
		api.logger.Error("admin_root_price_vendors_query_failed", map[string]any{"error": err.Error()})
		writeShellJSONNoEnvelope(writer, http.StatusInternalServerError,
			map[string]any{"ok": false, "error": "服务器内部错误"})
		return
	}
	items := make([]priceVendorSummary, 0, len(vendors))
	for _, vendor := range vendors {
		item := priceVendorSummary{
			Vendor:     vendor.Vendor,
			Name:       jobs.VendorDisplayName(vendor.Vendor),
			ModelCount: vendor.ModelCount,
		}
		if icon, ok := jobs.VendorIconFileForVendor(vendor.Vendor); ok {
			file := icon.File
			mono := icon.Mono
			item.Icon = &file
			item.IconMono = &mono
		}
		items = append(items, item)
	}
	writeShellJSONNoEnvelope(writer, http.StatusOK, priceVendorsRootResponse{
		OK:   true,
		Data: priceVendorsData{Vendors: items, Version: nil},
	})
}

// priceQueryInt 复刻 Number.parseInt 的可用子集：非数字返回 has=false（调用方据此走默认或 400）。
//
// Number.parseInt("12abc") = 12、"abc" = NaN，故这里按「前导数字」解析而不是全串匹配。
func priceQueryInt(raw string) (int, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, false
	}
	negative := false
	index := 0
	if trimmed[0] == '+' || trimmed[0] == '-' {
		negative = trimmed[0] == '-'
		index = 1
	}
	start := index
	value := 0
	for index < len(trimmed) && trimmed[index] >= '0' && trimmed[index] <= '9' {
		value = value*10 + int(trimmed[index]-'0')
		index++
	}
	if index == start {
		return 0, false
	}
	if negative {
		value = -value
	}
	return value, true
}

// providerPathID 读路径参数里的正整数 id（> 0 才算合法，照 zod 的 .positive()）。
func providerPathID(writer http.ResponseWriter, request *http.Request, name string) (int64, bool) {
	raw := ParamsFrom(request.Context())[name]
	value, ok := priceQueryInt(raw)
	if !ok || value <= 0 {
		writeShellJSONNoEnvelope(writer, http.StatusBadRequest, map[string]any{
			"title":  "Validation failed",
			"status": http.StatusBadRequest,
			"detail": name + " 必须是正整数",
		})
		return 0, false
	}
	return int64(value), true
}

// providerProbeLogsPaging 解 limit/offset（默认 200/0，上限 1000，照 ProviderProbeLogsQuerySchema）。
func providerProbeLogsPaging(writer http.ResponseWriter, request *http.Request) (int, int, bool) {
	query := request.URL.Query()
	limit := providerProbeLogsLimitDefault
	if raw := query.Get("limit"); raw != "" {
		parsed, ok := priceQueryInt(raw)
		if !ok || parsed < 1 || parsed > providerProbeLogsLimitMax {
			writeShellJSONNoEnvelope(writer, http.StatusBadRequest, map[string]any{
				"title": "Validation failed", "status": http.StatusBadRequest,
				"detail": "limit 必须是 1..1000 的整数",
			})
			return 0, 0, false
		}
		limit = parsed
	}
	offset := 0
	if raw := query.Get("offset"); raw != "" {
		parsed, ok := priceQueryInt(raw)
		if !ok || parsed < 0 {
			writeShellJSONNoEnvelope(writer, http.StatusBadRequest, map[string]any{
				"title": "Validation failed", "status": http.StatusBadRequest,
				"detail": "offset 必须是非负整数",
			})
			return 0, 0, false
		}
		offset = parsed
	}
	return limit, offset, true
}

// validProviderTypeValue 是 INTERNAL_PROVIDER_TYPE_VALUES 的成员判定。
func validProviderTypeValue(value string) bool {
	switch value {
	case "claude", "codex", "gemini", "openai-compatible",
		providerTypeClaudeAuth, providerTypeGeminiCLI:
		return true
	default:
		return false
	}
}

// providerItemsResponse 是「列表包 items」的统一信封（Node 的 ProviderEndpoint/VendorArrayResponseSchema）。
type providerItemsResponse[T any] struct {
	Items []T `json:"items"`
}

// providerVendorPayload 复刻 toProviderVendor + sanitizeProviderEndpointData（websiteUrl 脱敏）。
// 字段顺序照 Node 的对象字面量，便于逐字节对拍。
type providerVendorPayload struct {
	ID            int64   `json:"id"`
	WebsiteDomain string  `json:"websiteDomain"`
	DisplayName   *string `json:"displayName"`
	WebsiteURL    any     `json:"websiteUrl"`
	FaviconURL    *string `json:"faviconUrl"`
	CreatedAt     *string `json:"createdAt"`
	UpdatedAt     *string `json:"updatedAt"`
	// ProviderTypes **只在仪表盘分支出现**（Node 的 DashboardProviderVendor 才带它）。
	// omitempty 是契约要求：默认分支的响应正文里不得多出该键，否则与既有对拍/快照分叉。
	ProviderTypes []string `json:"providerTypes,omitempty"`
}

func newProviderVendorPayload(vendor store.AdminProviderVendor) providerVendorPayload {
	return providerVendorPayload{
		ID:            vendor.ID,
		WebsiteDomain: vendor.WebsiteDomain,
		DisplayName:   vendor.DisplayName,
		WebsiteURL:    providerRedactURLCredentialsNullable(vendor.WebsiteURL),
		FaviconURL:    vendor.FaviconURL,
		CreatedAt:     vendor.CreatedAt,
		UpdatedAt:     vendor.UpdatedAt,
	}
}

// providerEndpointPayload 复刻 toProviderEndpoint + sanitizeProviderEndpointData（url 脱敏）。
type providerEndpointPayload struct {
	ID                    int64   `json:"id"`
	VendorID              int64   `json:"vendorId"`
	ProviderType          string  `json:"providerType"`
	URL                   string  `json:"url"`
	Label                 *string `json:"label"`
	SortOrder             int     `json:"sortOrder"`
	IsEnabled             bool    `json:"isEnabled"`
	LastProbedAt          *string `json:"lastProbedAt"`
	LastProbeOK           *bool   `json:"lastProbeOk"`
	LastProbeStatusCode   *int    `json:"lastProbeStatusCode"`
	LastProbeLatencyMS    *int    `json:"lastProbeLatencyMs"`
	LastProbeErrorType    *string `json:"lastProbeErrorType"`
	LastProbeErrorMessage *string `json:"lastProbeErrorMessage"`
	CreatedAt             *string `json:"createdAt"`
	UpdatedAt             *string `json:"updatedAt"`
	DeletedAt             *string `json:"deletedAt"`
}

func newProviderEndpointPayload(endpoint store.AdminProviderEndpoint) providerEndpointPayload {
	return providerEndpointPayload{
		ID:                    endpoint.ID,
		VendorID:              endpoint.VendorID,
		ProviderType:          endpoint.ProviderType,
		URL:                   providerRedactURLCredentials(endpoint.URL),
		Label:                 endpoint.Label,
		SortOrder:             endpoint.SortOrder,
		IsEnabled:             endpoint.IsEnabled,
		LastProbedAt:          endpoint.LastProbedAt,
		LastProbeOK:           endpoint.LastProbeOK,
		LastProbeStatusCode:   endpoint.LastProbeStatusCode,
		LastProbeLatencyMS:    endpoint.LastProbeLatencyMS,
		LastProbeErrorType:    endpoint.LastProbeErrorType,
		LastProbeErrorMessage: endpoint.LastProbeErrorMessage,
		CreatedAt:             endpoint.CreatedAt,
		UpdatedAt:             endpoint.UpdatedAt,
		DeletedAt:             endpoint.DeletedAt,
	}
}

// providerProbeLogsResponse 对应 action 返回的 {endpointId, logs}。
type providerProbeLogsResponse struct {
	EndpointID int64                                 `json:"endpointId"`
	Logs       []store.AdminProviderEndpointProbeLog `json:"logs"`
}

// priceRootResponse / modelPricePaginatedResult 对应 Node 的 ActionResult<PaginatedResult>。
//
// 键名是 `data` 而不是 v1 资源面的 `items`——两条路由的正文形状不同，这里不能复用
// modelPriceListResponse。
type priceRootResponse struct {
	OK   bool                      `json:"ok"`
	Data modelPricePaginatedResult `json:"data"`
}

type modelPricePaginatedResult struct {
	Data       []modelPriceItem `json:"data"`
	Total      int64            `json:"total"`
	Page       int              `json:"page"`
	PageSize   int              `json:"pageSize"`
	TotalPages int64            `json:"totalPages"`
}

// priceVendorsRootResponse / priceVendorSummary 对应 /api/prices/vendors 的响应。
//
// Vendors 用 any 而不是 `[]priceVendorSummary`：优先分支要把 cloud_pricing_catalog 的 jsonb 列
// **原样透出**（Node 同样是直接给 catalog.vendors），用定长结构会默默丢掉存储里多出的键。
type priceVendorsRootResponse struct {
	OK   bool             `json:"ok"`
	Data priceVendorsData `json:"data"`
}

type priceVendorsData struct {
	Vendors any     `json:"vendors"`
	Version *string `json:"version"`
}

type priceVendorSummary struct {
	Vendor     string  `json:"vendor"`
	Name       string  `json:"name"`
	Icon       *string `json:"icon,omitempty"`
	IconMono   *bool   `json:"iconMono,omitempty"`
	ModelCount int64   `json:"modelCount"`
}

// writeNotFound 按 Node 的 error-envelope 形状作答 404。
func (api *providerEndpointAPI) writeNotFound(
	writer http.ResponseWriter,
	request *http.Request,
	errorCode string,
	detail string,
) {
	api.depts.WriteProblem(writer, request, http.StatusNotFound, errorCode, detail)
}

// writeReadFailure 把依赖故障（库不可达）按 500 作答并记日志。
func (api *providerEndpointAPI) writeReadFailure(
	writer http.ResponseWriter,
	request *http.Request,
	err error,
) {
	api.logger.Error("admin_provider_endpoint_read_failed", map[string]any{
		"path":  request.URL.Path,
		"error": err.Error(),
	})
	api.depts.WriteProblem(writer, request, http.StatusInternalServerError, "", "")
}

// 未实现清单：**已空**。本资源在 Node 侧 18 条，Go 侧已全部落地。
//
// 最后一条（`GET /provider-vendors?dashboard`）于 2026-09-13 迁出：它曾是本资源唯一的 501，
// 而「添加供应商」表单正是它的消费者——生产上用户因此加不了供应商（网关日志
// `admin_provider_endpoint_dashboard_vendors_not_implemented` 恰好 3 次，与表单轮询次数吻合）。
// 现按 Node 的 getDashboardProviderVendors 实现：只列「真有启用供应商」的 vendor，
// 并为每个 vendor 附 `providerTypes`（按 Node 的 ProviderTypeSchema.options 顺序）。
//
// **本清单已空意味着管理面不再有 501**：全仓 `StatusNotImplemented` 归零，
// 因此「未实现的端点原样回退 Node」这条兼容路径在管理面已无使用者（回退机制本身仍在）。
//
// 已迁出本清单的六条熔断端点（2026-09-12）：见 provider_circuit_admin.go。
// 已迁出本清单的五条写路径与三条拨测（2026-09-13）：见 provider_endpoints_write.go。
// 已迁出本清单的价格面一条（2026-09-13）：见 prices_cloud_count.go。
// 已迁出本清单的仪表盘 vendor 一条（2026-09-13）：本文件 handleDashboardVendors。
