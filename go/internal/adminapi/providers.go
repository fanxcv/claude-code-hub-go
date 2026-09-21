package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/cfgsync"
	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 providers 资源模块（P0，UI 的 4 个供应商页面依赖它）。
//
// 唯一真源：
//   - 路由与状态码：src/app/api/v1/resources/providers/router.ts（36 条注册，其中 32 条为
//     provider 本体、4 条为 provider-groups）
//   - 响应形状：src/app/api/v1/schemas/providers.ts 的 ProviderSummarySchema 与
//     handlers.ts 的 sanitizeProvider（脱敏逐字对齐）
//   - 业务规则：src/actions/providers.ts、src/repository/provider.ts
//
// 本 lane 只注册**读取面与低风险写入面**（12 条，见 RegisterProviders 的注释）；其余一律
// **不注册**，于是它们原样回退 Node（那里有完整实现）——这是本仓库既定的降级语义：
// 「注册了但语义不对」比「未注册而回退」坏得多。未注册项的清单与原因写在文件末尾。
//
// 命名约定：本文件所有标识符带 provider 前缀，避免与同包并行 lane 撞符号。

// providerHiddenTypes 是隐藏的历史供应商类型（constants.ts:14）。
//
// 非 dashboard-compat 请求要把它们过滤掉；compat 请求（管理界面自身）要看得见。
var providerHiddenTypes = []string{"claude-auth", "gemini-cli"}

// providerPublicTypes 是允许出现在**写请求**（providerType 查询参数与创建/更新体）里的类型。
var providerPublicTypes = []string{"claude", "codex", "gemini", "openai-compatible"}

// providerSummary 逐字对应 ProviderSummarySchema（schemas/providers.ts:27-174）。
//
// 为什么另建一个结构体而不是直接复用 store.AdminProvider：schema 明确**不含** key、
// description、tpm/rpm/rpd/cc（"Hidden legacy provider types and deprecated limit fields are
// omitted"），而 store 行里这些都读回来了（key 用于算 maskedKey）。多一层显式映射，
// 才能保证「不该出现的列一个都不出现」。
type providerSummary struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	URL         string  `json:"url"`
	MaskedKey   string  `json:"maskedKey"`
	IsEnabled   bool    `json:"isEnabled"`
	Weight      int     `json:"weight"`
	Priority    int     `json:"priority"`
	GroupPri    any     `json:"groupPriorities"`
	CostMult    float64 `json:"costMultiplier"`
	GroupTag    *string `json:"groupTag"`
	ProviderTyp string  `json:"providerType"`
	VendorID    *int64  `json:"providerVendorId"`

	PreserveClientIP    bool `json:"preserveClientIp"`
	DisableSessionReuse bool `json:"disableSessionReuse"`
	ModelRedirects      any  `json:"modelRedirects"`

	ActiveTimeStart *string `json:"activeTimeStart"`
	ActiveTimeEnd   *string `json:"activeTimeEnd"`
	AllowedModels   any     `json:"allowedModels"`
	AllowedClients  any     `json:"allowedClients"`
	BlockedClients  any     `json:"blockedClients"`

	MCPPassthroughType any  `json:"mcpPassthroughType"`
	MCPPassthroughURL  any  `json:"mcpPassthroughUrl"`
	ProtocolConversion bool `json:"protocolConversionEnabled"`

	Limit5hUSD      *float64 `json:"limit5hUsd"`
	Limit5hReset    string   `json:"limit5hResetMode"`
	LimitDailyUSD   *float64 `json:"limitDailyUsd"`
	DailyResetMode  string   `json:"dailyResetMode"`
	DailyResetTime  string   `json:"dailyResetTime"`
	LimitWeeklyUSD  *float64 `json:"limitWeeklyUsd"`
	LimitMonthlyUSD *float64 `json:"limitMonthlyUsd"`
	LimitTotalUSD   *float64 `json:"limitTotalUsd"`
	TotalCostReset  *string  `json:"totalCostResetAt"`
	LimitConcurrent *int     `json:"limitConcurrentSessions"`

	MaxRetryAttempts         *int `json:"maxRetryAttempts"`
	CircuitFailureThreshold  *int `json:"circuitBreakerFailureThreshold"`
	CircuitOpenDuration      *int `json:"circuitBreakerOpenDuration"`
	CircuitHalfOpenThreshold *int `json:"circuitBreakerHalfOpenSuccessThreshold"`
	// 等待阶梯：递增时长（ms）与最大次数。可空 = 不启用阶梯（窗口恒为熔断时长）。
	// 必须在这里出现：读投影（store.AdminProvider）带出这两列，汇总结构漏掉就等于
	// 「库里写了、接口不回」——界面拿不到值，用户看到的是空白。
	CircuitReleaseIncrement *int `json:"circuitBreakerReleaseIncrement"`
	CircuitMaxOpenCount     *int `json:"circuitBreakerMaxOpenCount"`

	// 低速降级（逐渠道开关与五参数，默认全关）。
	//
	// 必须在这里出现：读投影（store.AdminProvider）已带出这七列，汇总结构漏掉就等于
	// 「库里写了、接口不回」——表单读不到已有配置，用户看到空白。本字段组在 1.9.11
	// 首次上线时就踩过这一条（store 层登了、这里漏了），故与上面对阶梯字段的注释同例。
	SlowRateMonitorEnabled bool `json:"slowRateMonitorEnabled"`
	// SlowRateWindowSeconds 是判定滑窗（默认 1800s）；SlowRateBaselineWindowSeconds 是基线主窗
	// （默认 3 天）。两列尺度不同（分钟 vs 天），不可混用。
	SlowRateWindowSeconds         *int `json:"slowRateWindowSeconds"`
	SlowRateBaselineWindowSeconds *int `json:"slowRateBaselineWindowSeconds"`
	// SlowRateMinSamples 是基线样本下限；SlowRateTriggerCount 是触发阈值。两者语义不同。
	SlowRateMinSamples    *int `json:"slowRateMinSamples"`
	SlowRateTriggerCount  *int `json:"slowRateTriggerCount"`
	SlowRateRatioPerMille *int `json:"slowRateRatioPerMille"`
	SlowRatePenaltyStep   *int `json:"slowRatePenaltyStep"`
	SlowRatePenaltyMax    *int `json:"slowRatePenaltyMax"`

	ProxyURL              any  `json:"proxyUrl"`
	ProxyFallbackToDirect bool `json:"proxyFallbackToDirect"`
	CustomHeaders         any  `json:"customHeaders"`

	FirstByteTimeoutStreamMs int `json:"firstByteTimeoutStreamingMs"`
	StreamingIdleTimeoutMs   int `json:"streamingIdleTimeoutMs"`
	RequestTimeoutNonStream  int `json:"requestTimeoutNonStreamingMs"`

	WebsiteURL          any     `json:"websiteUrl"`
	FaviconURL          *string `json:"faviconUrl"`
	CacheTTLPreference  *string `json:"cacheTtlPreference"`
	SwapCacheTTLBilling bool    `json:"swapCacheTtlBilling"`
	Context1m           *string `json:"context1mPreference"`

	CodexReasoningEffort    *string `json:"codexReasoningEffortPreference"`
	CodexReasoningSummary   *string `json:"codexReasoningSummaryPreference"`
	CodexTextVerbosity      *string `json:"codexTextVerbosityPreference"`
	CodexParallelToolCalls  *string `json:"codexParallelToolCallsPreference"`
	CodexImageGeneration    any     `json:"codexImageGenerationPreference"`
	CodexServiceTier        any     `json:"codexServiceTierPreference"`
	CodexMaxTokens          *string `json:"codexMaxTokensPreference"`
	AnthropicMaxTokens      *string `json:"anthropicMaxTokensPreference"`
	AnthropicThinkingBudget any     `json:"anthropicThinkingBudgetPreference"`
	AnthropicAdaptive       any     `json:"anthropicAdaptiveThinking"`
	OpenAIMaxTokens         *string `json:"openaiMaxTokensPreference"`
	GeminiGoogleSearch      *string `json:"geminiGoogleSearchPreference"`

	// 以下四项是「已废弃但仍在响应里」的统计镜像（sanitizeProvider:703-706）。
	TodayTotalCostUSD string  `json:"todayTotalCostUsd"`
	TodayCallCount    int64   `json:"todayCallCount"`
	LastCallTime      *string `json:"lastCallTime"`
	LastCallModel     *string `json:"lastCallModel"`

	// Statistics 仅在 include=statistics 时出现（omitempty 语义靠指针）。
	Statistics *providerStatistics `json:"statistics,omitempty"`

	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
}

// providerStatistics 对应 ProviderSummarySchema 的 statistics（schemas/providers.ts:151-160）。
type providerStatistics struct {
	TodayCost     string  `json:"todayCost"`
	TodayCalls    int64   `json:"todayCalls"`
	LastCallTime  *string `json:"lastCallTime"`
	LastCallModel *string `json:"lastCallModel"`
}

// providerListResponse 对应 `{ items: [...] }`。
type providerListResponse struct {
	Items []providerSummary `json:"items"`
}

// providerKeyRevealResponse 对应 ProviderKeyRevealResponseSchema。
type providerKeyRevealResponse struct {
	Key string `json:"key"`
}

// providerUsageResetResponse 对应 resetProviderUsage 的 `{ ok: true }`。
type providerUsageResetResponse struct {
	OK bool `json:"ok"`
}

// providerBatchUpdateResponse 对应 batchUpdateProviders 的 `{ updatedCount }`。
type providerBatchUpdateResponse struct {
	UpdatedCount int64 `json:"updatedCount"`
}

// providerAutoSortGroup 对应 AutoSortResult["groups"][number]。
type providerAutoSortGroup struct {
	CostMultiplier float64                  `json:"costMultiplier"`
	Priority       int                      `json:"priority"`
	Providers      []providerAutoSortMember `json:"providers"`
}

// providerAutoSortMember 是分组内的一员（id + name）。
type providerAutoSortMember struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// providerAutoSortChange 对应 AutoSortResult["changes"][number]。
type providerAutoSortChange struct {
	ProviderID     int64   `json:"providerId"`
	Name           string  `json:"name"`
	OldPriority    int     `json:"oldPriority"`
	NewPriority    int     `json:"newPriority"`
	CostMultiplier float64 `json:"costMultiplier"`
}

// providerAutoSortSummary 对应 AutoSortResult["summary"]。
type providerAutoSortSummary struct {
	TotalProviders int `json:"totalProviders"`
	ChangedCount   int `json:"changedCount"`
	GroupCount     int `json:"groupCount"`
}

// providerAutoSortResponse 对应 AutoSortResult（含 applied）。
type providerAutoSortResponse struct {
	Groups  []providerAutoSortGroup  `json:"groups"`
	Changes []providerAutoSortChange `json:"changes"`
	Summary providerAutoSortSummary  `json:"summary"`
	Applied bool                     `json:"applied"`
}

// RegisterProviders 注册本模块的路由。
//
// 已注册（12 条）：
//
//	GET    /providers                      列表（q / providerType / include=statistics）
//	GET    /providers/{id}                 详情
//	GET    /providers/{id}/key:reveal      明文密钥（仅管理员）
//	GET    /providers/groups               分组列表（带引用计数）
//	POST   /providers/{id}/usage:reset     重置总额度计费窗口
//	POST   /providers:autoSortPriority     按成本倍率重排优先级（支持 preview/apply）
//	POST   /providers:batchUpdate          批量更新（只写给出的字段）
//
// 未注册（原样回退 Node）及原因：
//
//	POST   /providers                     创建：需要厂商解析（provider_vendors 按域名归一）、
//	                                      端点行 provisioning 与全套字段规范化（actions/providers.ts
//	                                      的 normalize* 系列，约 400 行），未对齐前注册会把
//	                                      半成品写进库。
//	PATCH  /providers/{id}                更新：同上 + 前像/撤销快照。
//	DELETE /providers/{id}                删除：Node 会写 Redis 撤销快照
//	                                      （cch:prov:undo-del:<token>，60s TTL）并回 X-CCH-Undo-Token；
//	                                      Go 侧尚无 KV 缝（Deps 里没有 Redis 入口），写不出兼容快照，
//	                                      于是「删除成功但撤销按钮必然失败」——宁可不接管。
//	                                      （store 侧同样不自带软删函数：等 KV 缝就位、能一并写快照时再补，
//	                                      避免留下未接线的死代码。）
//	POST   /providers:batchDelete         同上（响应体里带 undoToken 供 UI 事后撤销）。
//	POST   /providers:undoDelete|undoPatch 撤销：读上述快照。
//	POST   /providers/{id}/circuit:reset  熔断复位：要删 Node 布局的 Redis 熔断键
//	POST   /providers/circuits:batchReset （health.Writer 的键构造未导出），同上。
//	GET    /providers/health              熔断健康：读同一批 Redis 键。
//	GET    /providers/{id}/limit-usage    限额用量：需要 5h/日/周/月/总额度的运行态读取器。
//	POST   /providers/limit-usage:batch   同上（批量）。
//	POST   /providers:batchPatch:preview|apply、POST /providers:batchPatch:apply 的幂等账本
//	                                      （provider_batch_apply_operations）未移植。
//	GET    /providers/cache-effectiveness 缓存有效性统计。
//	POST   /providers/test:*、/providers/{id}/test、/providers/test:unified、
//	       /providers/upstream-models:fetch  需要真实上游拨号与各家协议探测。
//	GET    /providers/test:presets、/providers/model-suggestions、POST /providers/vendors:recluster
func RegisterProviders(router *Router, deps Deps) {
	if deps.Store == nil {
		if deps.Logger != nil {
			deps.Logger.Warn("admin_providers_store_unwired", map[string]any{
				"module": "providers",
				"action": "routes_not_registered",
			})
		}
		return
	}
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/providers",
		Access:      AccessAdmin,
		Module:      "providers",
		OperationID: "listProviders",
		Handler:     http.HandlerFunc(handleListProviders(deps)),
	})
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/providers/groups",
		Access:      AccessAdmin,
		Module:      "providers",
		OperationID: "listProviderGroupOptions",
		// 与 /provider-groups（另一资源的对象列表）同名不同义：此处必须是三分支的选项/计数面，
		// 见 provider_group_options.go 的文件头。
		Handler: http.HandlerFunc(handleListProviderGroupOptions(deps)),
	})
	// 静态段，必须早于任何 /providers/{...} 参数段注册（参数段已带 [0-9]+ 约束，不会遮蔽它）。
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/providers/model-suggestions",
		Access:      AccessAdmin,
		Module:      "providers",
		OperationID: "listProviderModelSuggestions",
		Handler:     http.HandlerFunc(handleProviderModelSuggestions(deps)),
	})
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/providers/test:presets",
		Access:      AccessAdmin,
		Module:      "providers",
		OperationID: "getProviderTestPresets",
		Handler:     http.HandlerFunc(handleProviderTestPresets(deps)),
	})
	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/providers/test:proxy",
		Access:      AccessAdmin,
		Module:      "providers",
		OperationID: "testProviderProxy",
		Handler:     http.HandlerFunc(handleTestProviderProxy(deps)),
	})
	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/providers/upstream-models:fetch",
		Access:      AccessAdmin,
		Module:      "providers",
		OperationID: "fetchProviderUpstreamModels",
		Handler:     http.HandlerFunc(handleFetchProviderUpstreamModels(deps)),
	})
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/providers/cache-effectiveness",
		Access:      AccessAdmin,
		Module:      "providers",
		OperationID: "listProviderCacheEffectiveness",
		Handler:     http.HandlerFunc(handleProviderCacheEffectiveness(deps)),
	})
	// 参数段带 [0-9]+ 约束，与 Node 逐字对齐（src/app/api/v1/resources/providers/router.ts:204,233,254,276
	// 均为 `/providers/:id{[0-9]+}`）。
	//
	// 为何必须带约束（实测缺陷）：无约束的 `{id}` 会**遮蔽**同前缀的静态 Node-only 端点——
	// 回退只在「没有任何 Go 路由命中」时发生，而通用参数段会把这些路径一并接住，于是：
	//
	//	GET /api/v1/providers/health              -> Go 守卫 401，而不是原样反代 Node
	//	GET /api/v1/providers/cache-effectiveness -> 同上
	//	GET /api/v1/providers/model-suggestions   -> 同上
	//	GET /api/v1/providers/test:presets        -> 同上
	//
	// 这四条在双跑架构下会**静默失效**（既不报错也不回退），详见
	// 钉子见 cmd/cchd/admin_route_shadowing_test.go。
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/providers/{id:[0-9]+}",
		Access:      AccessAdmin,
		Module:      "providers",
		OperationID: "getProvider",
		Handler:     http.HandlerFunc(handleGetProvider(deps)),
	})
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/providers/{id:[0-9]+}/key:reveal",
		Access:      AccessAdmin,
		Module:      "providers",
		OperationID: "revealProviderKey",
		Handler:     http.HandlerFunc(handleRevealProviderKey(deps)),
	})
	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/providers/{id:[0-9]+}/usage:reset",
		Access:      AccessAdmin,
		Module:      "providers",
		OperationID: "resetProviderUsage",
		Handler:     http.HandlerFunc(handleResetProviderUsage(deps)),
	})
	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/providers:autoSortPriority",
		Access:      AccessAdmin,
		Module:      "providers",
		OperationID: "autoSortProviders",
		Handler:     http.HandlerFunc(handleAutoSortProviders(deps)),
	})
	router.Add(Route{
		Method:      http.MethodPost,
		Path:        "/providers:batchUpdate",
		Access:      AccessAdmin,
		Module:      "providers",
		OperationID: "batchUpdateProviders",
		Handler:     http.HandlerFunc(handleBatchUpdateProviders(deps)),
	})

	// 运行态子面（熔断健康与复位）见 providers_health.go。
	//
	// 三条都必须带 `[0-9]+` 约束或放在 `{id}` 之后：`/providers/{id}` 无约束时会把这些静态
	// 路径一并接住，
	// 钉子：cmd/cchd/admin_route_shadowing_test.go。
	//
	// 未接线（Deps.CircuitStates 不支持供应商级读写）时**不注册**，原样回退 Node：
	// 注册一个必然失败的路由，比不注册坏得多。
	if circuits, ok := providerCircuitStore(deps); ok && circuits != nil {
		router.Add(Route{
			Method:      http.MethodGet,
			Path:        "/providers/health",
			Access:      AccessAdmin,
			Module:      "providers",
			OperationID: "getProvidersHealth",
			Handler:     http.HandlerFunc(handleGetProvidersHealth(deps)),
		})
		router.Add(Route{
			Method:      http.MethodPost,
			Path:        "/providers/circuits:batchReset",
			Access:      AccessAdmin,
			Module:      "providers",
			OperationID: "batchResetProviderCircuits",
			Handler:     http.HandlerFunc(handleResetProviderCircuitsBatch(deps)),
		})
		router.Add(Route{
			Method:      http.MethodPost,
			Path:        "/providers/{id:[0-9]+}/circuit:reset",
			Access:      AccessAdmin,
			Module:      "providers",
			OperationID: "resetProviderCircuit",
			Handler:     http.HandlerFunc(handleResetProviderCircuit(deps)),
		})
	}
}

// handleListProviders 复刻 listProviders（handlers.ts:60-90）。
//
// 四段：①查询参数校验（q/providerType/include）②可见性过滤（隐藏类型只对 dashboard-compat 可见）
// ③q/providerType 过滤 ④脱敏映射（+ 可选 statistics）。
func handleListProviders(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		rawQuery := query.Get("q")
		providerType := query.Get("providerType")
		include := query.Get("include")

		if providerType != "" && !providerTypeAllowed(request, providerType) {
			adminWriteValidationFailure(writer, request, []invalidParam{{
				Path: []any{"providerType"},
				Code: "invalid_enum_value",
				Message: fmt.Sprintf("Invalid enum value. Expected %s, received '%s'",
					adminEnumList(providerTypesForRequest(request)), providerType),
			}})
			return
		}
		if include != "" && include != "statistics" {
			adminWriteValidationFailure(writer, request, []invalidParam{{
				Path:    []any{"include"},
				Code:    "invalid_enum_value",
				Message: fmt.Sprintf("Invalid enum value. Expected 'statistics', received '%s'", include),
			}})
			return
		}

		providers, err := deps.Store.AdminListProviders(request.Context())
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}

		compat := providerDashboardCompat(request)
		visible := make([]store.AdminProvider, 0, len(providers))
		for _, provider := range providers {
			if !compat && providerIsHidden(provider.ProviderType) {
				continue
			}
			visible = append(visible, provider)
		}
		visible = providerFilter(visible, rawQuery, providerType)

		var statistics map[int64]providerStatistics
		if include == "statistics" {
			statistics, err = providerLoadStatistics(request.Context(), deps)
			if err != nil {
				// Node 侧 getProviderStatisticsAsync 在异常时返回 {}（不失败整条列表）：
				// 统计是装饰性的，宁可少数字也不让列表 500。
				if deps.Logger != nil {
					deps.Logger.Warn("admin_providers_statistics_failed", map[string]any{
						"error": err.Error(),
					})
				}
				statistics = nil
			}
		}

		items := make([]providerSummary, 0, len(visible))
		for _, provider := range visible {
			var stats *providerStatistics
			if statistics != nil {
				if value, ok := statistics[provider.ID]; ok {
					copied := value
					stats = &copied
				}
			}
			items = append(items, providerSummaryPayload(provider, stats))
		}
		adminWriteJSON(writer, http.StatusOK, providerListResponse{Items: items})
	}
}

// handleGetProvider 复刻 getProvider（handlers.ts:92-98）。
func handleGetProvider(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		id, ok := providerIDParam(writer, request)
		if !ok {
			return
		}
		provider, err := providerFindVisible(request, deps, id)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, err)
			return
		}
		adminWriteJSON(writer, http.StatusOK, providerSummaryPayload(*provider, nil))
	}
}

// handleRevealProviderKey 复刻 revealProviderKey（handlers.ts:236-250）。
func handleRevealProviderKey(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		id, ok := providerIDParam(writer, request)
		if !ok {
			return
		}
		if _, err := providerFindVisible(request, deps, id); err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, err)
			return
		}
		key, err := deps.Store.AdminRevealProviderKey(request.Context(), id)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}
		adminWriteJSON(writer, http.StatusOK, providerKeyRevealResponse{Key: key})
	}
}

// handleResetProviderUsage 复刻 resetProviderUsage（handlers.ts:344-360）。
func handleResetProviderUsage(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		id, ok := providerIDParam(writer, request)
		if !ok {
			return
		}
		if _, err := providerFindVisible(request, deps, id); err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, err)
			return
		}
		reset, err := deps.Store.AdminResetProviderTotalCost(request.Context(), id)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}
		if !reset {
			adminProblemWriter(deps).WriteActionError(writer, request, providerNotFoundError())
			return
		}
		adminPublishDomain(deps, request, cfgsync.DomainProviders)
		adminWriteJSON(writer, http.StatusOK, providerUsageResetResponse{OK: true})
	}
}

// handleAutoSortProviders 复刻 autoSortProviders（handlers.ts:452-460 + actions/providers.ts:1110-1210）。
//
// 排序口径：按 costMultiplier 的**去重升序**分组，组序号即新优先级（从 0 起）；
// 组内成员按 id 升序展示；changes 只记「旧优先级 ≠ 新优先级」的行。
// confirm=false 时只预览不落库（applied=false）。
func handleAutoSortProviders(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		fields, ok := adminReadJSONObject(writer, request, deps)
		if !ok {
			return
		}
		object := adminNewObject(fields, "confirm")
		object.RejectUnknownKeys()
		confirm, _ := object.Bool("confirm")
		if issues := object.issues0(); len(issues) > 0 {
			adminWriteValidationFailure(writer, request, issues)
			return
		}

		providers, err := deps.Store.AdminListProviders(request.Context())
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}

		confirmValue := confirm != nil && *confirm
		response := providerAutoSort(providers, confirmValue)
		if confirmValue && len(response.Changes) > 0 {
			changes := make([]store.AdminProviderPriorityChange, 0, len(response.Changes))
			for _, change := range response.Changes {
				changes = append(changes, store.AdminProviderPriorityChange{
					ID:       change.ProviderID,
					Priority: change.NewPriority,
				})
			}
			if _, err := deps.Store.AdminApplyProviderPriorities(request.Context(), changes); err != nil {
				adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
				return
			}
			adminPublishDomain(deps, request, cfgsync.DomainProviders)
		}
		adminWriteJSON(writer, http.StatusOK, response)
	}
}

// handleBatchUpdateProviders 复刻 batchUpdateProviders（handlers.ts:486-500）。
//
// 与 Node 一致的两处前置：①providerIds 必须全部可见（否则 404）②补丁至少要有一个字段
// （Node 侧 `请指定要更新的字段` → 400）。
func handleBatchUpdateProviders(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		fields, ok := adminReadJSONObject(writer, request, deps)
		if !ok {
			return
		}
		object := adminNewObject(fields, "providerIds", "updates")
		object.RejectUnknownKeys()

		ids, hasIDs := adminInt64Array(object, "providerIds", 1, 500)
		rawUpdates, hasUpdates := object.Raw("updates")
		if issues := object.issues0(); len(issues) > 0 {
			adminWriteValidationFailure(writer, request, issues)
			return
		}
		if !hasIDs {
			adminWriteValidationFailure(writer, request, []invalidParam{{
				Path: []any{"providerIds"}, Code: "invalid_type", Message: "Required",
			}})
			return
		}
		if !hasUpdates || adminJSONTypeName(rawUpdates) != "object" {
			adminWriteValidationFailure(writer, request, []invalidParam{{
				Path: []any{"updates"}, Code: "invalid_type", Message: adminTypeMessage("object", rawUpdates),
			}})
			return
		}

		patch, issues := providerParseBatchPatch(rawUpdates)
		if len(issues) > 0 {
			adminWriteValidationFailure(writer, request, issues)
			return
		}
		if providerBatchPatchEmpty(patch) {
			// Node：`请指定要更新的字段` → action 失败 → 400 provider.action_failed。
			adminProblemWriter(deps).WriteActionError(writer, request,
				NewActionError("provider", "provider.action_failed", http.StatusBadRequest,
					fmt.Errorf("未指定要更新的字段")))
			return
		}

		visible, err := providerVisibleSet(request, deps)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}
		for _, id := range ids {
			if _, ok := visible[id]; !ok {
				adminProblemWriter(deps).WriteActionError(writer, request, providerNotFoundError())
				return
			}
		}

		updated, err := deps.Store.AdminApplyProviderBatchPatch(request.Context(), patch, ids)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, adminActionFailure("provider", err))
			return
		}

		// 只有这五个字段会改变已建立的会话亲和（Node batchUpdateProviders:2918-2926）。
		// 其余字段（优先级/权重/限额…）不动亲和，不该把在跑的会话踢掉。
		if patch.HasGroupTag || patch.HasModelRedirects || patch.HasAllowedModels ||
			patch.HasAllowedClients || patch.HasBlockedClients {
			adminTerminateStickySessions(request, deps, ids, "batchUpdateProviders")
		}

		adminPublishDomain(deps, request, cfgsync.DomainProviders)
		adminWriteJSON(writer, http.StatusOK, providerBatchUpdateResponse{UpdatedCount: updated})
	}
}

// providerParseBatchPatch 解析 ProviderBatchUpdateFieldsSchema（schemas/providers.ts:245-289）。
//
// 严格模式：未声明的键一律 400。每列都区分「没给」与「显式给 null」——这是批量更新的语义核心
// （只写给出的字段，见 store.AdminApplyProviderBatchPatch）。
func providerParseBatchPatch(raw json.RawMessage) (store.AdminProviderBatchPatch, []invalidParam) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return store.AdminProviderBatchPatch{}, []invalidParam{{
			Path: []any{"updates"}, Code: "invalid_type", Message: "Expected object, received string",
		}}
	}
	allowed := []string{
		"is_enabled", "priority", "weight", "cost_multiplier", "group_tag",
		"model_redirects", "allowed_models", "allowed_clients", "blocked_clients",
		"limit_5h_usd", "limit_5h_reset_mode", "limit_daily_usd", "daily_reset_mode",
		"daily_reset_time", "codex_image_generation_preference", "codex_service_tier_preference",
		"anthropic_thinking_budget_preference", "anthropic_adaptive_thinking",
	}
	object := adminNewObject(fields, allowed...)
	object.RejectUnknownKeys()

	var patch store.AdminProviderBatchPatch
	if value, present := object.Bool("is_enabled"); present {
		patch.IsEnabled = value
	}
	if value, present := object.Number("priority", false, []any{"updates", "priority"}); present {
		patch.Priority = providerIntPointer(value)
	}
	if value, present := object.Number("weight", false, []any{"updates", "weight"}); present {
		patch.Weight = value
	}
	if value, present := object.Number("cost_multiplier", false, []any{"updates", "cost_multiplier"}); present {
		patch.CostMultiplier = value
	}
	if value, present := providerNullableString(object, "group_tag", 200, []any{"updates", "group_tag"}); present {
		patch.HasGroupTag = true
		patch.GroupTag = value
	}
	if value, present := providerNullableRaw(object, "model_redirects", []any{"updates", "model_redirects"}); present {
		patch.HasModelRedirects = true
		patch.ModelRedirects = value
	}
	if value, present := providerNullableRaw(object, "allowed_models", []any{"updates", "allowed_models"}); present {
		patch.HasAllowedModels = true
		patch.AllowedModels = value
	}
	if value, present := object.StringArray("allowed_clients"); present {
		patch.HasAllowedClients = true
		patch.AllowedClients = value
	}
	if value, present := object.StringArray("blocked_clients"); present {
		patch.HasBlockedClients = true
		patch.BlockedClients = value
	}
	if value, present := providerNullableNumber(object, "limit_5h_usd", []any{"updates", "limit_5h_usd"}); present {
		patch.HasLimit5hUSD = true
		patch.Limit5hUSD = value
	}
	if value, present := object.String("limit_5h_reset_mode", adminStringSpec{Enum: providerResetModes}); present {
		patch.Limit5hResetMode = &value
	}
	if value, present := providerNullableNumber(object, "limit_daily_usd", []any{"updates", "limit_daily_usd"}); present {
		patch.HasLimitDailyUSD = true
		patch.LimitDailyUSD = value
	}
	if value, present := object.String("daily_reset_mode", adminStringSpec{Enum: providerResetModes}); present {
		patch.DailyResetMode = &value
	}
	if value, present := object.String("daily_reset_time", adminStringSpec{}); present {
		patch.DailyResetTime = &value
	}
	if value, present := providerNullableString(object, "codex_image_generation_preference", 0,
		[]any{"updates", "codex_image_generation_preference"}); present {
		patch.HasCodexImageGeneration = true
		patch.CodexImageGeneration = value
	}
	if value, present := providerNullableString(object, "codex_service_tier_preference", 0,
		[]any{"updates", "codex_service_tier_preference"}); present {
		patch.HasCodexServiceTier = true
		patch.CodexServiceTier = value
	}
	if value, present := providerNullableString(object, "anthropic_thinking_budget_preference", 0,
		[]any{"updates", "anthropic_thinking_budget_preference"}); present {
		patch.HasAnthropicThinkingBudget = true
		patch.AnthropicThinkingBudget = value
	}
	if value, present := providerNullableRaw(object, "anthropic_adaptive_thinking",
		[]any{"updates", "anthropic_adaptive_thinking"}); present {
		patch.HasAnthropicAdaptiveThinking = true
		patch.AnthropicAdaptiveThinking = value
	}
	return patch, object.issues0()
}

// providerBatchPatchEmpty 判定补丁是否为空（复刻 `Object.keys(updates).length === 0`）。
func providerBatchPatchEmpty(patch store.AdminProviderBatchPatch) bool {
	return patch.IsEnabled == nil && patch.Priority == nil && patch.Weight == nil &&
		patch.CostMultiplier == nil && !patch.HasGroupTag && !patch.HasModelRedirects &&
		!patch.HasAllowedModels && !patch.HasAllowedClients && !patch.HasBlockedClients &&
		!patch.HasLimit5hUSD && patch.Limit5hResetMode == nil && !patch.HasLimitDailyUSD &&
		patch.DailyResetMode == nil && patch.DailyResetTime == nil &&
		!patch.HasCodexImageGeneration && !patch.HasCodexServiceTier &&
		!patch.HasAnthropicThinkingBudget && !patch.HasAnthropicAdaptiveThinking
}

// providerNullableString 读一个 `string | null | undefined` 字段（三者语义各不相同）。
func providerNullableString(
	object *adminObject,
	key string,
	maxRunes int,
	path []any,
) (store.NullableString, bool) {
	raw, present := object.Raw(key)
	if !present {
		return store.NullableString{}, false
	}
	if adminJSONTypeName(raw) == "null" {
		return store.NullableString{Set: true, Value: nil}, true
	}
	value, ok := object.String(key, adminStringSpec{MaxRunes: maxRunes})
	if !ok {
		return store.NullableString{}, true
	}
	return store.NullableString{Set: true, Value: &value}, true
}

// providerNullableNumber 读一个 `number | null | undefined` 字段。
func providerNullableNumber(
	object *adminObject,
	key string,
	path []any,
) (store.NullableFloat, bool) {
	raw, present := object.Raw(key)
	if !present {
		return store.NullableFloat{}, false
	}
	if adminJSONTypeName(raw) == "null" {
		return store.NullableFloat{Set: true, Value: nil}, true
	}
	value, ok := object.Number(key, false, path)
	if !ok {
		return store.NullableFloat{}, true
	}
	return store.NullableFloat{Set: true, Value: value}, true
}

// providerNullableRaw 读一个 `unknown | null | undefined` 字段（jsonb 列，原样透传）。
func providerNullableRaw(object *adminObject, key string, path []any) (json.RawMessage, bool) {
	raw, present := object.Raw(key)
	if !present {
		return nil, false
	}
	if adminJSONTypeName(raw) == "null" {
		return nil, true
	}
	return raw, true
}

// providerIntPointer 把浮点取出成整数指针（zod 的 .int() 已在 Number 校验里由调用方保证）。
func providerIntPointer(value *float64) *int {
	if value == nil {
		return nil
	}
	converted := int(*value)
	return &converted
}

// providerIDParam 解析并校验路径参数 id（复刻 ProviderIdParamSchema 的 coerce + int + positive）。
func providerIDParam(writer http.ResponseWriter, request *http.Request) (int64, bool) {
	raw := ParamsFrom(request.Context())["id"]
	// Node 的 /providers/:id{[0-9]+} 只匹配数字；带后缀的路由（/usage:reset）在 Go 侧
	// 由路径模式里的字面后缀吃掉，故这里拿到的就是纯数字。
	id, ok := adminCoerceInt(raw)
	if !ok || id <= 0 {
		adminWriteValidationFailure(writer, request, []invalidParam{{
			Path:    []any{"id"},
			Code:    "invalid_type",
			Message: "Expected number, received string",
		}})
		return 0, false
	}
	return int64(id), true
}

// providerFindVisible 复刻 findVisibleProvider：取可见集合后按 id 找，找不到即 404。
func providerFindVisible(
	request *http.Request,
	deps Deps,
	id int64,
) (*store.AdminProvider, error) {
	providers, err := providerVisibleProviders(request, deps)
	if err != nil {
		return nil, adminActionFailure("provider", err)
	}
	for index := range providers {
		if providers[index].ID == id {
			return &providers[index], nil
		}
	}
	return nil, providerNotFoundError()
}

// providerVisibleProviders 复刻 loadVisibleProviders。
func providerVisibleProviders(request *http.Request, deps Deps) ([]store.AdminProvider, error) {
	providers, err := deps.Store.AdminListProviders(request.Context())
	if err != nil {
		return nil, err
	}
	if providerDashboardCompat(request) {
		return providers, nil
	}
	visible := make([]store.AdminProvider, 0, len(providers))
	for _, provider := range providers {
		if providerIsHidden(provider.ProviderType) {
			continue
		}
		visible = append(visible, provider)
	}
	return visible, nil
}

// providerVisibleSet 返回可见供应商的 id 集合（批量操作的前置校验用）。
func providerVisibleSet(request *http.Request, deps Deps) (map[int64]struct{}, error) {
	providers, err := providerVisibleProviders(request, deps)
	if err != nil {
		return nil, err
	}
	set := make(map[int64]struct{}, len(providers))
	for _, provider := range providers {
		set[provider.ID] = struct{}{}
	}
	return set, nil
}

// providerNotFoundError 复刻 providerNotFound（handlers.ts:769-776）。
func providerNotFoundError() *ActionError {
	return NewActionError("provider", "provider.not_found", http.StatusNotFound,
		fmt.Errorf("供应商不存在"))
}

// providerDashboardCompat 复刻 isDashboardCompatRequest：头为 "1" 且调用方是管理员。
//
// 守卫已保证 admin 档位，故这里只需判头——非管理员根本到不了处理器。
func providerDashboardCompat(request *http.Request) bool {
	return request.Header.Get("X-CCH-Dashboard-Compat") == "1"
}

// providerIsHidden 判定供应商类型是否属于隐藏的历史类型。
func providerIsHidden(providerType string) bool {
	for _, hidden := range providerHiddenTypes {
		if providerType == hidden {
			return true
		}
	}
	return false
}

// providerTypesForRequest 给出当前请求允许的 providerType 集合。
func providerTypesForRequest(request *http.Request) []string {
	if providerDashboardCompat(request) {
		return append(append([]string{}, providerPublicTypes...), providerHiddenTypes...)
	}
	return providerPublicTypes
}

// providerTypeAllowed 判定 providerType 查询参数是否合法。
func providerTypeAllowed(request *http.Request, value string) bool {
	for _, allowed := range providerTypesForRequest(request) {
		if value == allowed {
			return true
		}
	}
	return false
}

// providerFilter 复刻 filterProviders（handlers.ts:611-623）。
//
// q 是大小写不敏感的**子串**匹配，字段范围固定为 name/url/groupTag/providerType；
// providerType 是精确相等。
func providerFilter(providers []store.AdminProvider, rawQuery, providerType string) []store.AdminProvider {
	needle := strings.ToLower(rawQuery)
	if needle == "" && providerType == "" {
		return providers
	}
	filtered := make([]store.AdminProvider, 0, len(providers))
	for _, provider := range providers {
		if providerType != "" && provider.ProviderType != providerType {
			continue
		}
		if needle == "" {
			filtered = append(filtered, provider)
			continue
		}
		candidates := []string{provider.Name, provider.URL, provider.ProviderType}
		if provider.GroupTag != nil {
			candidates = append(candidates, *provider.GroupTag)
		}
		for _, candidate := range candidates {
			if strings.Contains(strings.ToLower(candidate), needle) {
				filtered = append(filtered, provider)
				break
			}
		}
	}
	return filtered
}

// providerLoadStatistics 取今日统计（include=statistics）。
func providerLoadStatistics(ctx context.Context, deps Deps) (map[int64]providerStatistics, error) {
	location, err := providerSystemLocation(ctx, deps)
	if err != nil {
		return nil, err
	}
	rows, err := deps.Store.AdminProviderStatistics(ctx, location.String())
	if err != nil {
		return nil, err
	}
	result := make(map[int64]providerStatistics, len(rows))
	for _, row := range rows {
		result[row.ID] = providerStatistics{
			TodayCost:     row.TodayCost,
			TodayCalls:    row.TodayCalls,
			LastCallTime:  row.LastCallTime,
			LastCallModel: row.LastCallModel,
		}
	}
	return result, nil
}

// providerSystemLocation 复刻 resolveSystemTimezone 的三级取值：system_settings.timezone →
// 环境变量 TZ（未设置取 `Asia/Shanghai`）→ UTC（默认值与校验集中在 config）。
func providerSystemLocation(ctx context.Context, deps Deps) (*time.Location, error) {
	raw, err := deps.Store.AdminSystemTimezone(ctx)
	if err != nil {
		return nil, err
	}
	return config.ResolveLocationFromEnv(raw), nil
}

// providerAutoSort 复刻 autoSortProviderPriority 的分组与变更计算（actions/providers.ts:1110-1200）。
func providerAutoSort(providers []store.AdminProvider, applied bool) providerAutoSortResponse {
	if len(providers) == 0 {
		return providerAutoSortResponse{
			Groups:  []providerAutoSortGroup{},
			Changes: []providerAutoSortChange{},
			Summary: providerAutoSortSummary{},
			Applied: applied,
		}
	}

	buckets := map[float64][]store.AdminProvider{}
	multipliers := make([]float64, 0, len(providers))
	for _, provider := range providers {
		multiplier := 0.0
		if provider.CostMultiplier != nil {
			multiplier = *provider.CostMultiplier
		}
		if _, seen := buckets[multiplier]; !seen {
			multipliers = append(multipliers, multiplier)
		}
		buckets[multiplier] = append(buckets[multiplier], provider)
	}
	sort.Float64s(multipliers)

	groups := make([]providerAutoSortGroup, 0, len(multipliers))
	changes := make([]providerAutoSortChange, 0, len(providers))
	for priority, multiplier := range multipliers {
		members := buckets[multiplier]
		sorted := make([]store.AdminProvider, len(members))
		copy(sorted, members)
		sort.Slice(sorted, func(left, right int) bool { return sorted[left].ID < sorted[right].ID })

		listed := make([]providerAutoSortMember, 0, len(sorted))
		for _, provider := range sorted {
			listed = append(listed, providerAutoSortMember{ID: provider.ID, Name: provider.Name})
		}
		groups = append(groups, providerAutoSortGroup{
			CostMultiplier: multiplier,
			Priority:       priority,
			Providers:      listed,
		})
		// changes 的顺序是「原始 providers 顺序」，与 Node 一致（它在 for provider of groupProviders 里 push）。
		for _, provider := range members {
			if provider.Priority != priority {
				changes = append(changes, providerAutoSortChange{
					ProviderID:     provider.ID,
					Name:           provider.Name,
					OldPriority:    provider.Priority,
					NewPriority:    priority,
					CostMultiplier: multiplier,
				})
			}
		}
	}

	return providerAutoSortResponse{
		Groups:  groups,
		Changes: changes,
		Summary: providerAutoSortSummary{
			TotalProviders: len(providers),
			ChangedCount:   len(changes),
			GroupCount:     len(groups),
		},
		Applied: applied,
	}
}

// providerSummaryPayload 复刻 sanitizeProvider（handlers.ts:625-712）的逐字段映射与脱敏。
func providerSummaryPayload(provider store.AdminProvider, statistics *providerStatistics) providerSummary {
	summary := providerSummary{
		ID:          provider.ID,
		Name:        provider.Name,
		URL:         providerRedactURLCredentials(provider.URL),
		MaskedKey:   usersMaskKey(provider.Key),
		IsEnabled:   provider.IsEnabled,
		Weight:      provider.Weight,
		Priority:    provider.Priority,
		GroupPri:    providerJSONOrNil(provider.GroupPriorities),
		CostMult:    providerCostMultiplier(provider.CostMultiplier),
		GroupTag:    provider.GroupTag,
		ProviderTyp: provider.ProviderType,
		VendorID:    provider.ProviderVendorID,

		PreserveClientIP:    provider.PreserveClientIP,
		DisableSessionReuse: provider.DisableSessionReuse,
		ModelRedirects:      providerJSONOrNil(provider.ModelRedirects),

		ActiveTimeStart: provider.ActiveTimeStart,
		ActiveTimeEnd:   provider.ActiveTimeEnd,
		AllowedModels:   providerJSONOrNil(provider.AllowedModels),
		AllowedClients:  providerStringSlice(provider.AllowedClients),
		BlockedClients:  providerStringSlice(provider.BlockedClients),

		MCPPassthroughType: provider.MCPPassthroughType,
		MCPPassthroughURL:  providerRedactURLCredentialsNullable(provider.MCPPassthroughURL),
		ProtocolConversion: provider.ProtocolConversionOn,

		Limit5hUSD:      provider.Limit5hUSD,
		Limit5hReset:    provider.Limit5hResetMode,
		LimitDailyUSD:   provider.LimitDailyUSD,
		DailyResetMode:  provider.DailyResetMode,
		DailyResetTime:  provider.DailyResetTime,
		LimitWeeklyUSD:  provider.LimitWeeklyUSD,
		LimitMonthlyUSD: provider.LimitMonthlyUSD,
		LimitTotalUSD:   provider.LimitTotalUSD,
		TotalCostReset:  provider.TotalCostResetAt,
		LimitConcurrent: provider.LimitConcurrentSessions,

		MaxRetryAttempts:         provider.MaxRetryAttempts,
		CircuitFailureThreshold:  provider.CircuitFailureThreshold,
		CircuitOpenDuration:      provider.CircuitOpenDuration,
		CircuitHalfOpenThreshold: provider.CircuitHalfOpenThreshold,
		CircuitReleaseIncrement:  provider.CircuitReleaseIncrement,
		CircuitMaxOpenCount:      provider.CircuitMaxOpenCount,

		SlowRateMonitorEnabled: provider.SlowRateMonitorEnabled,
		SlowRateWindowSeconds:  provider.SlowRateWindowSeconds,
		// 基线主窗与判定滑窗是两列（尺度差三个数量级），故两行都得写。
		SlowRateBaselineWindowSeconds: provider.SlowRateBaselineWindowSeconds,
		SlowRateMinSamples:            provider.SlowRateMinSamples,
		SlowRateTriggerCount:          provider.SlowRateTriggerCount,
		SlowRateRatioPerMille:         provider.SlowRateRatioPerMille,
		SlowRatePenaltyStep:           provider.SlowRatePenaltyStep,
		SlowRatePenaltyMax:            provider.SlowRatePenaltyMax,

		ProxyURL:              providerRedactURLCredentialsNullable(provider.ProxyURL),
		ProxyFallbackToDirect: provider.ProxyFallbackToDirect,
		CustomHeaders:         providerRedactHeaderRecord(provider.CustomHeaders),

		FirstByteTimeoutStreamMs: provider.FirstByteTimeoutStreamMs,
		StreamingIdleTimeoutMs:   provider.StreamingIdleTimeoutMs,
		RequestTimeoutNonStream:  provider.RequestTimeoutNonStream,

		WebsiteURL:          providerRedactURLOrOriginal(provider.WebsiteURL),
		FaviconURL:          provider.FaviconURL,
		CacheTTLPreference:  provider.CacheTTLPreference,
		SwapCacheTTLBilling: provider.SwapCacheTTLBilling,
		Context1m:           provider.Context1mPreference,

		CodexReasoningEffort:    provider.CodexReasoningEffort,
		CodexReasoningSummary:   provider.CodexReasoningSummary,
		CodexTextVerbosity:      provider.CodexTextVerbosity,
		CodexParallelToolCalls:  provider.CodexParallelToolCalls,
		CodexImageGeneration:    providerNullableStringValue(provider.CodexImageGeneration),
		CodexServiceTier:        providerNullableStringValue(provider.CodexServiceTier),
		CodexMaxTokens:          provider.CodexMaxTokens,
		AnthropicMaxTokens:      provider.AnthropicMaxTokens,
		AnthropicThinkingBudget: providerNullableStringValue(provider.AnthropicThinkingBudget),
		AnthropicAdaptive:       providerJSONOrNil(provider.AnthropicAdaptive),
		OpenAIMaxTokens:         provider.OpenAIMaxTokens,
		GeminiGoogleSearch:      provider.GeminiGoogleSearch,

		CreatedAt: adminStringOrNow(provider.CreatedAt),
		UpdatedAt: adminStringOrNow(provider.UpdatedAt),
	}

	// 废弃字段的兜底值：无 statistics 时 todayTotalCostUsd 取 "0"、计数取 0、时间与模型取 null
	// （schemas/providers.ts:133-149 的 describe 明文写了这三个默认值）。
	summary.TodayTotalCostUSD = "0"
	if statistics != nil {
		summary.Statistics = statistics
		summary.TodayTotalCostUSD = statistics.TodayCost
		summary.TodayCallCount = statistics.TodayCalls
		summary.LastCallTime = statistics.LastCallTime
		summary.LastCallModel = statistics.LastCallModel
	}
	return summary
}

// providerRedactURLCredentials 复刻 redactUrlCredentials（redaction.ts:36-47）。
//
// 仅当 URL 带 userinfo 时改写：用户名与密码都换成 "REDACTED"。解析失败或本就没有凭据时原样返回。
func providerRedactURLCredentials(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.User == nil) {
		return raw
	}
	if parsed.User.Username() == "" && !providerURLHasPassword(parsed) {
		return raw
	}
	parsed.User = url.UserPassword("REDACTED", "REDACTED")
	return parsed.String()
}

// providerURLHasPassword 判定 URL 是否带密码段（user:pass@host）。
func providerURLHasPassword(parsed *url.URL) bool {
	_, has := parsed.User.Password()
	return has
}

// providerRedactURLCredentialsNullable 对可空 URL 列做同样处理（nil 保持 nil）。
func providerRedactURLCredentialsNullable(raw *string) any {
	if raw == nil {
		return nil
	}
	return providerRedactURLCredentials(*raw)
}

// providerRedactURLOrOriginal 复刻 `redactUrlCredentials(x) ?? x`：未给值时保持 null。
func providerRedactURLOrOriginal(raw *string) any {
	if raw == nil {
		return nil
	}
	return providerRedactURLCredentials(*raw)
}

// providerRedactHeaderRecord 复刻 redactHeaderRecord（redaction.ts:25-34）：
// 键名命中敏感模式的头一律换成 [REDACTED]，其余原样；nil 保持 null。
//
// 为什么还要单独判 nil map：本模块的行是经 `row_to_json` 文本读回的，SQL NULL 会变成 JSON 字面量
// null（4 字节），而不是空 RawMessage。直接交给 json.Unmarshal 时，null 会解成 nil map 且不报错，
// 随后 make(map, 0) 就把它变成 {}——Node 侧 customHeaders 为 NULL 时给的是 null（对拍 D4）。
func providerRedactHeaderRecord(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var headers map[string]string
	if err := json.Unmarshal(raw, &headers); err != nil {
		// 列里的 jsonb 形状不可预期（手工改库等）：原样透出比伪造一个 {} 诚实。
		return adminRawValue(raw)
	}
	if headers == nil {
		// JSON null（以及其它解成 nil map 的非对象值）：Node 的 `if (!headers) return null` 同判。
		return nil
	}
	redacted := make(map[string]string, len(headers))
	for name, value := range headers {
		if providerSecretHeaderName(name) {
			redacted[name] = auditRedacted
			continue
		}
		redacted[name] = value
	}
	return redacted
}

// providerSecretHeaderName 复刻 isSecretHeaderName（redaction.ts:49-58）。
func providerSecretHeaderName(name string) bool {
	normalized := strings.ToLower(name)
	switch normalized {
	case "authorization", "x-api-key", "cookie", "set-cookie":
		return true
	}
	return strings.Contains(normalized, "token") || strings.Contains(normalized, "secret") ||
		strings.Contains(normalized, "password") || strings.Contains(normalized, "cookie") ||
		strings.Contains(normalized, "authorization") ||
		strings.Contains(normalized, "apikey") || strings.Contains(normalized, "api-key") ||
		strings.Contains(normalized, "api_key")
}

// providerJSONOrNil 把 jsonb 列转成可编码值：NULL 与空数组都按 null 出（Node 侧类型就是
// `xxx | null`，drizzle 给的是 null 或对象）。
func providerJSONOrNil(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return adminRawValue(raw)
}

// adminRawValue 把 json.RawMessage 变成可被 encoding/json 原样输出的值。
//
// 直接放 json.RawMessage 也行，但它为 nil 时会输出 `null` 而不是被省略——本处调用方都已把
// nil 判过，故只在非空时调用。
func adminRawValue(raw json.RawMessage) any { return raw }

// providerNullableStringValue 把可空字符串转 any（nil 出 null）。
func providerNullableStringValue(raw *string) any {
	if raw == nil {
		return nil
	}
	return *raw
}

// providerStringSlice 保证数组列出 `[]` 而不是 null（列 notNull default '{}'）。
func providerStringSlice(values []string) any {
	if values == nil {
		return []string{}
	}
	return values
}

// providerCostMultiplier 复刻 `Number(provider.costMultiplier)` 的可用性兜底：列可空时按 1 出。
//
// Node 的 ProviderDisplay.costMultiplier 是 number，findAllProvidersFresh 走 drizzle 的 numeric
// 映射；列有 default '1.0' 但允许显式 null，故这里给 1.0（与 Node 侧的默认倍率一致）。
func providerCostMultiplier(raw *float64) float64 {
	if raw == nil {
		return 1.0
	}
	return *raw
}

// adminInt64Array 读一个正整数数组字段（providerIds）。
func adminInt64Array(object *adminObject, key string, minLen, maxLen int) ([]int64, bool) {
	raw, present := object.Raw(key)
	if !present {
		return nil, false
	}
	if adminJSONTypeName(raw) != "array" {
		object.fail([]any{key}, "invalid_type", adminTypeMessage("array", raw))
		return nil, true
	}
	var values []float64
	if err := json.Unmarshal(raw, &values); err != nil {
		object.fail([]any{key}, "invalid_type", adminTypeMessage("array", raw))
		return nil, true
	}
	if len(values) < minLen {
		object.fail([]any{key}, "too_small",
			fmt.Sprintf("Array must contain at least %d element(s)", minLen))
		return nil, true
	}
	if maxLen > 0 && len(values) > maxLen {
		object.fail([]any{key}, "too_big",
			fmt.Sprintf("Array must contain at most %d element(s)", maxLen))
		return nil, true
	}
	ids := make([]int64, 0, len(values))
	for index, value := range values {
		if value != float64(int64(value)) || value <= 0 {
			object.fail([]any{key, index}, "invalid_type", "Expected number, received float")
			return nil, true
		}
		ids = append(ids, int64(value))
	}
	return ids, true
}

// providerResetModes 是 limit_5h_reset_mode / daily_reset_mode 的取值集合。
var providerResetModes = []string{"fixed", "rolling"}
