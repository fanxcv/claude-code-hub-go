package adminapi

import (
	"net/http"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是三条**根级可用性读端点**的 Go 落点
// （Node 侧 src/app/api/availability/{route.ts,current/route.ts,endpoints/route.ts,endpoints/probe-logs/route.ts}）：
//
//	GET /api/availability/current                  当前状态（15 分钟窗口）
//	GET /api/availability/endpoints?vendorId&providerType
//	GET /api/availability/endpoints/probe-logs?endpointId&limit&offset
//
// **三条的形状与 /api/v1 资源面不同**：它们都是「页面自己 fetch 的私有端点」，作答是 NextResponse.json
// 的裸对象（`{error:"..."}` 与 `{vendorId,providerType,endpoints}`），没有 problem 信封，也没有
// X-API-Version 头。故这里一律走 `NoManagementEnvelope` + 裸 JSON。
//
// 一处登记差异：**拒绝非管理员时的正文形状**。Node 这几条自己 getSession() 后回
// `{error:"Unauthorized"}` + 401；Go 的 AccessAdmin 由守卫作答（problem 信封）。准入判定完全一致
// （都是「非管理员即拒」），差异只在正文形状，且只在越权路径上可见。
//
// **`GET /api/availability`（按时间桶聚合）不在本文件**：它要 availability-service 的
// bucket 聚合面（611 行，含最优桶宽推导、多档延迟分位、按 provider 截断 maxBuckets），
// 与这三条的自足查询不是一回事。仍在 Node，见文件末清单。

const (
	// availabilityCurrentWindowMinutes 照 availability-service.ts:46。
	availabilityCurrentWindowMinutes = 15
	// availabilityProbeLogsLimitDefault / Max 照 endpoints/probe-logs/route.ts 的 200 / 1000。
	availabilityProbeLogsLimitDefault = 200
	availabilityProbeLogsLimitMax     = 1000
)

// availabilityProviderTypes 是 PROVIDER_TYPES（endpoints/route.ts:5-12，**含两个隐藏类型**）。
//
// 这个白名单比 v1 资源面的公开四档宽：它是 Dashboard 内部端点，隐藏类型同样要能查。
var availabilityProviderTypes = []string{
	"claude", "claude-auth", "codex", "gemini-cli", "gemini", "openai-compatible",
}

// RegisterAvailabilityRoutes 注册三条根级可用性读端点（Store 未装配时整组不注册）。
func RegisterAvailabilityRoutes(router *Router, deps Deps) {
	if deps.Store == nil {
		if deps.Logger != nil {
			deps.Logger.Warn("admin_availability_store_unwired", map[string]any{
				"module": "availability",
				"action": "routes_not_registered",
			})
		}
		return
	}
	api := &availabilityAPI{
		pools:  deps.Store,
		deps:   deps,
		logger: adminLoggerOf(deps),
	}

	registerBucketedAvailability(router, api)

	router.Add(Route{
		Method:               http.MethodGet,
		Path:                 "/api/availability/current",
		Access:               AccessAdmin,
		Module:               "availability",
		OperationID:          "getCurrentProviderStatus",
		NoManagementEnvelope: true,
		Handler:              http.HandlerFunc(api.handleCurrent),
	})
	router.Add(Route{
		Method:               http.MethodGet,
		Path:                 "/api/availability/endpoints",
		Access:               AccessAdmin,
		Module:               "availability",
		OperationID:          "listAvailabilityEndpoints",
		NoManagementEnvelope: true,
		Handler:              http.HandlerFunc(api.handleEndpoints),
	})
	router.Add(Route{
		Method:               http.MethodGet,
		Path:                 "/api/availability/endpoints/probe-logs",
		Access:               AccessAdmin,
		Module:               "availability",
		OperationID:          "listAvailabilityEndpointProbeLogs",
		NoManagementEnvelope: true,
		Handler:              http.HandlerFunc(api.handleEndpointProbeLogs),
	})
}

type availabilityAPI struct {
	pools  *store.Pools
	deps   Deps
	logger *logx.Logger
}

// handleCurrent 复刻 getCurrentProviderStatus（availability-service.ts:476）。
//
// 两级数据源：先读 avail_current 的进程内状态行，**新鲜度不足 15 分钟的行被丢弃**（空闲供应商
// 的状态不该永久停在绿/红）；缺失的再回落到 avail_bucket_1m 的 15 分钟窗口聚合。
func (api *availabilityAPI) handleCurrent(writer http.ResponseWriter, request *http.Request) {
	ctx := request.Context()
	providers, err := api.pools.AdminListEnabledProviderNames(ctx)
	if err != nil {
		api.logger.Error("admin_availability_current_failed", map[string]any{"error": err.Error()})
		availabilityError(writer, http.StatusInternalServerError, "Internal server error")
		return
	}
	if len(providers) == 0 {
		availabilityWriteJSON(writer, http.StatusOK, []availabilityCurrentStatus{})
		return
	}

	ids := make([]int64, 0, len(providers))
	for _, provider := range providers {
		ids = append(ids, provider.ID)
	}

	now := time.Now()
	window := time.Duration(availabilityCurrentWindowMinutes) * time.Minute
	states, err := api.pools.AdminListAvailabilityCurrent(ctx, ids)
	if err != nil {
		api.logger.Error("admin_availability_current_failed", map[string]any{"error": err.Error()})
		availabilityError(writer, http.StatusInternalServerError, "Internal server error")
		return
	}

	byID := make(map[int64]store.AdminAvailabilityCurrent, len(states))
	for _, state := range states {
		freshAt := state.UpdatedAt
		if state.LastRequestAt.After(freshAt) {
			freshAt = state.LastRequestAt
		}
		if freshAt.IsZero() || now.Sub(freshAt) > window {
			continue
		}
		byID[state.ProviderID] = state
	}

	missing := make([]int64, 0, len(ids))
	for _, provider := range providers {
		if _, ok := byID[provider.ID]; !ok {
			missing = append(missing, provider.ID)
		}
	}
	if len(missing) > 0 {
		buckets, err := api.pools.AdminListAvailabilityBucketSummary(ctx, missing, availabilityCurrentWindowMinutes)
		if err != nil {
			api.logger.Error("admin_availability_current_failed", map[string]any{"error": err.Error()})
			availabilityError(writer, http.StatusInternalServerError, "Internal server error")
			return
		}
		for _, bucket := range buckets {
			total := bucket.GreenCount + bucket.RedCount
			if total <= 0 {
				continue
			}
			byID[bucket.ProviderID] = store.AdminAvailabilityCurrent{
				ProviderID:    bucket.ProviderID,
				State:         availabilityStateFromScore(bucket.GreenCount, bucket.RedCount),
				Availability:  availabilityScore(bucket.GreenCount, bucket.RedCount),
				RequestCount:  total,
				LastRequestAt: bucket.LastRequestAt,
				UpdatedAt:     bucket.LastRequestAt,
			}
		}
	}

	response := make([]availabilityCurrentStatus, 0, len(providers))
	for _, provider := range providers {
		state, found := byID[provider.ID]
		if !found || state.RequestCount <= 0 {
			response = append(response, availabilityCurrentStatus{
				ProviderID:   provider.ID,
				ProviderName: provider.Name,
				Status:       "unknown",
			})
			continue
		}
		response = append(response, availabilityCurrentStatus{
			ProviderID:    provider.ID,
			ProviderName:  provider.Name,
			Status:        availabilityStatusOf(state),
			Availability:  state.Availability,
			RequestCount:  state.RequestCount,
			LastRequestAt: availabilityISOString(state.LastRequestAt),
		})
	}
	availabilityWriteJSON(writer, http.StatusOK, response)
}

// handleEndpoints 复刻 endpoints/route.ts：按 (厂, 类型) 取 Dashboard 端点池。
//
// 这一条的响应**不做 URL 脱敏**（Node 这里没走 sanitizeProviderEndpointData）——与
// /api/v1/provider-vendors/{id}/endpoints 的处理不同，不能顺手复用那份脱敏后的载荷。
func (api *availabilityAPI) handleEndpoints(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	vendorID, ok := availabilityQueryInt(query.Get("vendorId"))
	if !ok || vendorID <= 0 {
		availabilityError(writer, http.StatusBadRequest, "Invalid query")
		return
	}
	providerType := query.Get("providerType")
	if !availabilityProviderType(providerType) {
		availabilityError(writer, http.StatusBadRequest, "Invalid query")
		return
	}

	endpoints, err := api.pools.AdminListDashboardProviderEndpointsByVendorAndType(
		request.Context(), int64(vendorID), providerType)
	if err != nil {
		api.logger.Error("admin_availability_endpoints_failed", map[string]any{"error": err.Error()})
		availabilityError(writer, http.StatusInternalServerError, "Internal server error")
		return
	}
	items := make([]providerEndpointRawPayload, 0, len(endpoints))
	for _, endpoint := range endpoints {
		items = append(items, newProviderEndpointRawPayload(endpoint))
	}
	availabilityWriteJSON(writer, http.StatusOK, availabilityEndpointsResponse{
		VendorID:     int64(vendorID),
		ProviderType: providerType,
		Endpoints:    items,
	})
}

// handleEndpointProbeLogs 复刻 endpoints/probe-logs/route.ts。
func (api *availabilityAPI) handleEndpointProbeLogs(writer http.ResponseWriter, request *http.Request) {
	query := request.URL.Query()
	endpointID, ok := availabilityQueryInt(query.Get("endpointId"))
	if !ok || endpointID <= 0 {
		availabilityError(writer, http.StatusBadRequest, "Invalid query")
		return
	}

	// limit / offset 的解析照 Node：缺省 200/0，出现但非法（NaN、<=0、>1000、<0）即 400。
	limit := availabilityProbeLogsLimitDefault
	if raw := query.Get("limit"); raw != "" {
		parsed, ok := availabilityQueryInt(raw)
		if !ok || parsed <= 0 || parsed > availabilityProbeLogsLimitMax {
			availabilityError(writer, http.StatusBadRequest, "Invalid query")
			return
		}
		limit = parsed
	}
	offset := 0
	if raw := query.Get("offset"); raw != "" {
		parsed, ok := availabilityQueryInt(raw)
		if !ok || parsed < 0 {
			availabilityError(writer, http.StatusBadRequest, "Invalid query")
			return
		}
		offset = parsed
	}

	ctx := request.Context()
	endpoint, err := api.pools.AdminGetProviderEndpointByID(ctx, int64(endpointID))
	if err == store.ErrNotFound {
		availabilityError(writer, http.StatusNotFound, "Not found")
		return
	}
	if err != nil {
		api.logger.Error("admin_availability_probe_logs_failed", map[string]any{"error": err.Error()})
		availabilityError(writer, http.StatusInternalServerError, "Internal server error")
		return
	}
	logs, err := api.pools.AdminListProviderEndpointProbeLogs(ctx, int64(endpointID), limit, offset)
	if err != nil {
		api.logger.Error("admin_availability_probe_logs_failed", map[string]any{"error": err.Error()})
		availabilityError(writer, http.StatusInternalServerError, "Internal server error")
		return
	}
	availabilityWriteJSON(writer, http.StatusOK, availabilityProbeLogsResponse{
		Endpoint: newProviderEndpointRawPayload(*endpoint),
		Logs:     logs,
	})
}

// availabilityScore 照 calculateAvailabilityScore：green/(green+red)，总数为 0 时 0。
func availabilityScore(green, red int64) float64 {
	total := green + red
	if total == 0 {
		return 0
	}
	return float64(green) / float64(total)
}

// availabilityStateFromScore 是回落分支里的状态取值（Node 的 `availability >= 0.5 ? green : red`）。
func availabilityStateFromScore(green, red int64) string {
	if availabilityScore(green, red) >= 0.5 {
		return "green"
	}
	return "red"
}

// availabilityStatusOf 复刻状态归一（availability-service.ts:584-597）。
//
// 四档映射：green/red/unknown 原样；yellow 与任何越界值都按 availability >= 0.5 收敛成 green/red。
func availabilityStatusOf(state store.AdminAvailabilityCurrent) string {
	switch strings.ToLower(state.State) {
	case "green", "red", "unknown":
		return strings.ToLower(state.State)
	default:
		if state.Availability >= 0.5 {
			return "green"
		}
		return "red"
	}
}

// availabilityISOString 复刻 toIsoString：零值时间给 null，其余给 24 字符 UTC 形状。
func availabilityISOString(value time.Time) *string {
	if value.IsZero() {
		return nil
	}
	formatted := value.UTC().Format("2006-01-02T15:04:05.000Z")
	return &formatted
}

// availabilityQueryInt 复刻 Number.parseInt 的取值面：非数字给 ok=false（调用方据此 400）。
func availabilityQueryInt(raw string) (int, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, false
	}
	return priceQueryInt(trimmed)
}

// availabilityProviderType 判定 providerType 是否在白名单内。
func availabilityProviderType(value string) bool {
	for _, candidate := range availabilityProviderTypes {
		if value == candidate {
			return true
		}
	}
	return false
}

// availabilityWriteJSON 写一份裸 JSON（Content-Type 与 Node 的 NextResponse.json 对齐）。
func availabilityWriteJSON(writer http.ResponseWriter, status int, body any) {
	adminWriteJSON(writer, status, body)
}

// availabilityError 复刻这几条的失败正文：`{"error": "..."}`。
func availabilityError(writer http.ResponseWriter, status int, message string) {
	availabilityWriteJSON(writer, status, map[string]string{"error": message})
}

// providerEndpointRawPayload 是**不脱敏**的端点载荷（本文件三条端点专用）。
//
// 与 providerEndpointPayload 的唯一差别是 url 不做凭据脱敏：Node 的这几条根级端点没有走
// sanitizeProviderEndpointData，顺手复用脱敏版会让「管理页显示的 URL」与真实 URL 不一致。
type providerEndpointRawPayload struct {
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

func newProviderEndpointRawPayload(endpoint store.AdminProviderEndpoint) providerEndpointRawPayload {
	return providerEndpointRawPayload{
		ID:                    endpoint.ID,
		VendorID:              endpoint.VendorID,
		ProviderType:          endpoint.ProviderType,
		URL:                   endpoint.URL,
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

// availabilityCurrentStatus 是 /api/availability/current 的一项（Node 的匿名返回类型）。
type availabilityCurrentStatus struct {
	ProviderID    int64   `json:"providerId"`
	ProviderName  string  `json:"providerName"`
	Status        string  `json:"status"`
	Availability  float64 `json:"availability"`
	RequestCount  int64   `json:"requestCount"`
	LastRequestAt *string `json:"lastRequestAt"`
}

// availabilityEndpointsResponse 是 /api/availability/endpoints 的正文。
type availabilityEndpointsResponse struct {
	VendorID     int64                        `json:"vendorId"`
	ProviderType string                       `json:"providerType"`
	Endpoints    []providerEndpointRawPayload `json:"endpoints"`
}

// availabilityProbeLogsResponse 是 /api/availability/endpoints/probe-logs 的正文。
type availabilityProbeLogsResponse struct {
	Endpoint providerEndpointRawPayload            `json:"endpoint"`
	Logs     []store.AdminProviderEndpointProbeLog `json:"logs"`
}

// 四条都已实现（本组无遗留）：
//
//	GET /api/availability/current
//	GET /api/availability/endpoints
//	GET /api/availability/endpoints/probe-logs
//	GET /api/availability（按时间桶聚合，见 availability_buckets.go）
