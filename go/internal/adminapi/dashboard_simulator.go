package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/limit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件实现 `POST /api/v1/dashboard/dispatch-simulator:simulate`。
//
// 唯一真源：
//   - 路由：src/app/api/v1/resources/dashboard/router.ts:227（requireAuth("admin")）
//   - handler：handlers.ts:103 simulateDispatch → actionJson(c, result.data)
//     （成功时返回的是**裸的** DispatchSimulatorResult，不套 {items} 信封）
//   - action：src/actions/dispatch-simulator.ts:163 simulateDispatchDecisionTree（九步）
//   - 契约：src/types/dispatch-simulator.ts（前端消费形状）
//   - 入参 schema：DispatchSimulatorInputSchema（同文件 :40-44，非 strict，故未知键被忽略）
//
// 九步的谓词全部复用 `internal/route` 的**唯一实现**（引擎在 route.Simulate），本文件只做三件事：
// 读全量供应商、把真实运行态读数接进引擎、按契约序列化。

// dispatchSimulatorBody 是请求体：与 DispatchSimulatorInputSchema 逐字段对齐。
type dispatchSimulatorBody struct {
	ClientFormat *convert.ClientFormat `json:"clientFormat"`
	ModelName    *string               `json:"modelName"`
	GroupTags    *[]string             `json:"groupTags"`
	raw          map[string]json.RawMessage
}

// dispatchSimulatorFormats 与 z.enum([...]) 同集合（顺序也照抄，便于报错时给同一份候选集）。
var dispatchSimulatorFormats = []convert.ClientFormat{
	convert.FormatClaude,
	convert.FormatOpenAI,
	convert.FormatResponse,
	convert.FormatGemini,
	convert.FormatGeminiCLI,
}

const (
	dispatchSimulatorModelNameMaxLen = 255
	dispatchSimulatorGroupTagMaxLen  = 255
	dispatchSimulatorGroupTagMax     = 20
)

// handleDispatchSimulator 复刻 simulateDispatch。
//
// 权限（Node 的 `session?.user.role !== "admin"` → PERMISSION_DENIED）由路由的
// `Access: AccessAdmin` 在守卫层统一作答（401/403 信封与 Node 对齐），本函数不再自查。
func (api *dashboardAPI) handleDispatchSimulator(writer http.ResponseWriter, request *http.Request) {
	body, problems := parseDispatchSimulatorBody(request)
	if len(problems) > 0 {
		api.problems.WriteValidationError(writer, request, problems)
		return
	}

	providers, err := api.pools.FindAllProvidersForSimulator(request.Context())
	if err != nil {
		api.logger.Error("admin_dispatch_simulator_providers_failed", map[string]any{
			"error": err.Error(),
		})
		api.writeDashboardFailure(writer, request, "dispatch-simulator", err)
		return
	}

	now, locationErr := api.simulatorLocation(request.Context())
	if locationErr != nil {
		// 时区取值链不可用不该让整个预览失败：按 UTC 继续（与 Node 的解析链末端一致）并留痕。
		api.logger.Warn("admin_dispatch_simulator_timezone_fallback", map[string]any{
			"error": locationErr.Error(),
		})
	}

	options := route.SimulateOptions{
		Format:    *body.ClientFormat,
		ModelName: derefStringOrEmpty(body.ModelName),
		GroupTags: derefStringSlice(body.GroupTags),
		// 活动时段比较要的是目标时区的「当日分钟数」，故在这里就把 now 落到该时区。
		Now:       time.Now().In(now),
		Providers: toSimulateProviders(providers),
	}
	api.wireSimulatorDataSources(request.Context(), &options, providers)

	result, err := route.Simulate(request.Context(), options)
	if err != nil {
		api.writeDashboardFailure(writer, request, "dispatch-simulator", err)
		return
	}

	// 成功响应是裸结果（Node 的 actionJson → jsonResponse(result.data)）。
	adminWriteJSON(writer, http.StatusOK, result)
}

// wireSimulatorDataSources 把管理面已装配的运行态读数接进引擎。
//
// 每一项都遵循同一纪律：**读不到就不接**（回调留 nil），引擎会跳过该维度——
// 与 Node 在该读数不可用时放行同一方向；反过来（把读不到当成「排除」）会平白少报候选。
func (api *dashboardAPI) wireSimulatorDataSources(
	ctx context.Context,
	options *route.SimulateOptions,
	rows []store.SimulatorProviderRow,
) {
	if api.deps.ProviderCost != nil {
		options.ProviderCostAllowed = func(ctx context.Context, p route.SimulateProvider) (bool, string) {
			row, ok := simulatorRowFor(rows, p.ID)
			if !ok {
				return true, ""
			}
			allowed, detail := api.deps.ProviderCost.CheckProviderCostLimits(ctx, p.ID, limit.ProviderCostLimits{
				Limit5hUSD:       row.Limit5hUSD,
				Limit5hResetMode: row.Limit5hResetMode,
				LimitDailyUSD:    row.LimitDailyUSD,
				DailyResetMode:   row.DailyResetMode,
				DailyResetTime:   row.DailyResetTime,
				LimitWeeklyUSD:   row.LimitWeeklyUSD,
				LimitMonthlyUSD:  row.LimitMonthlyUSD,
				CostResetAt:      row.TotalCostResetAt,
			})
			return allowed, detail
		}
		options.TotalCostAllowed = func(ctx context.Context, p route.SimulateProvider) (bool, string) {
			row, ok := simulatorRowFor(rows, p.ID)
			if !ok {
				return true, ""
			}
			allowed, detail := api.deps.ProviderCost.CheckProviderCostLimits(ctx, p.ID, limit.ProviderCostLimits{
				LimitTotalUSD: row.LimitTotalUSD,
				CostResetAt:   row.TotalCostResetAt,
			})
			return allowed, detail
		}
	}

	if circuits, ok := providerCircuitStore(api.deps); ok {
		options.ProviderCircuit = func(ctx context.Context, p route.SimulateProvider) (bool, string) {
			snapshot, err := circuits.ProviderCircuit(ctx, p.ID)
			if err != nil {
				api.logger.Warn("admin_dispatch_simulator_provider_circuit_failed", map[string]any{
					"providerId": p.ID, "error": err.Error(),
				})
				return false, ""
			}
			// 与 route.HealthReader.ProviderOpen 同一条判定：仅 open 且在窗口内才算打开
			// （已过期视为 half-open，放行试探）。
			if snapshot.CircuitState != "open" {
				return false, snapshot.CircuitState
			}
			if snapshot.CircuitOpenUntilMS != nil && *snapshot.CircuitOpenUntilMS > 0 &&
				time.Now().UnixMilli() > *snapshot.CircuitOpenUntilMS {
				return false, "half-open"
			}
			return true, "open"
		}
	}

	if api.deps.CircuitStates != nil && api.deps.EndpointCircuitBreaker {
		circuits := api.deps.CircuitStates
		options.VendorTypeCircuit = func(ctx context.Context, p route.SimulateProvider) bool {
			if p.ProviderVendorID == nil || *p.ProviderVendorID <= 0 {
				return false
			}
			snapshot, err := circuits.VendorTypeCircuit(ctx, *p.ProviderVendorID, string(p.ProviderType))
			if err != nil {
				api.logger.Warn("admin_dispatch_simulator_vendor_circuit_failed", map[string]any{
					"providerId": p.ID, "error": err.Error(),
				})
				return false
			}
			// Node 的 isVendorTypeCircuitOpen：manualOpen 直接算开；open 且未过期算开。
			if snapshot.ManualOpen {
				return true
			}
			if snapshot.CircuitState != "open" {
				return false
			}
			if snapshot.CircuitOpenUntilMS != nil && *snapshot.CircuitOpenUntilMS > 0 &&
				time.Now().UnixMilli() > *snapshot.CircuitOpenUntilMS {
				return false
			}
			return true
		}
	}

	if api.pools != nil && api.deps.CircuitStates != nil {
		circuits := api.deps.CircuitStates
		toggleOn := api.deps.EndpointCircuitBreaker
		options.EndpointStats = func(ctx context.Context, p route.SimulateProvider) *route.SimulateEndpointStats {
			if p.ProviderVendorID == nil || *p.ProviderVendorID <= 0 {
				return nil
			}
			endpoints, err := api.pools.FindProviderEndpointsForSimulator(ctx, *p.ProviderVendorID, string(p.ProviderType))
			if err != nil {
				api.logger.Warn("admin_dispatch_simulator_endpoints_failed", map[string]any{
					"providerId": p.ID, "error": err.Error(),
				})
				return nil
			}
			stats := &route.SimulateEndpointStats{Total: len(endpoints)}
			enabledIDs := make([]int64, 0, len(endpoints))
			for _, endpoint := range endpoints {
				if endpoint.IsEnabled && endpoint.DeletedAt == nil {
					enabledIDs = append(enabledIDs, endpoint.ID)
				}
			}
			stats.Enabled = len(enabledIDs)
			// 开关关闭时端点熔断不可能处于 open（Node 的 getEndpointFilterStats 首段同判）。
			if !toggleOn || len(enabledIDs) == 0 {
				stats.Available = stats.Enabled
				return stats
			}
			states, err := circuits.EndpointCircuits(ctx, enabledIDs)
			if err != nil {
				api.logger.Warn("admin_dispatch_simulator_endpoint_circuits_failed", map[string]any{
					"providerId": p.ID, "error": err.Error(),
				})
				stats.Available = stats.Enabled
				return stats
			}
			for _, id := range enabledIDs {
				if states[id].CircuitState == "open" {
					stats.CircuitOpen++
				}
			}
			stats.Available = stats.Enabled - stats.CircuitOpen
			return stats
		}
	}

	// 低速降权表：与真实选路**同一份读取实现**（route.SlowRateReader.Penalties）。
	//
	// 为什么必须接：引擎与真实选路共用 resolveEffectivePriority，不接则预览显示基础档位、
	// 真实选路显示降权后档位，两者静默不一致，而预览页正是排障时被信的那个。
	//
	// 候选里 `SlowRateMonitorEnabled` 为假的渠道由读侧自己滤掉（见 route.SlowRateReader.Penalties
	// 的开关判定），这里不做二次筛；模型名取 options.ModelName（与 route 侧同一个归一口径）。
	if api.deps.SlowRatePenalties != nil {
		candidates := make([]route.Provider, 0, len(options.Providers))
		for _, provider := range options.Providers {
			candidates = append(candidates, provider.Provider)
		}
		options.SlowRatePenalties = api.deps.SlowRatePenalties.Penalties(ctx, candidates, options.ModelName)
	}
}

// 低速降权读侧的实现必须是选路那一份：签名漂移或换实现会在这里编译不过。
var _ SlowRatePenaltyReader = (*route.SlowRateReader)(nil)

// simulatorLocation 取自然窗口（每日固定 / 周 / 月）要用的时区。
//
// 与数据面同一条取值链 `system_settings.timezone -> env TZ -> UTC`（见 store 的
// AdminSystemTimezoneOrUTC）；Node 的模拟器也是每次都 resolveSystemTimezone()，故这里不缓存。
func (api *dashboardAPI) simulatorLocation(ctx context.Context) (*time.Location, error) {
	if api.pools == nil {
		return time.UTC, nil
	}
	name := api.pools.AdminSystemTimezoneOrUTC(ctx)
	if strings.TrimSpace(name) == "" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return time.UTC, err
	}
	return loc, nil
}

// parseDispatchSimulatorBody 复刻 DispatchSimulatorInputSchema 的校验与缺省值。
func parseDispatchSimulatorBody(request *http.Request) (dispatchSimulatorBody, []InvalidParam) {
	body := dispatchSimulatorBody{raw: map[string]json.RawMessage{}}
	decoder := json.NewDecoder(request.Body)
	var raw map[string]json.RawMessage
	if err := decoder.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			// 空体在 Node 上是「必填对象缺失」→ invalid_type(clientFormat)。
			return body, []InvalidParam{{
				Path: []any{"clientFormat"}, Code: "invalid_type",
				Message: "Invalid input: expected claude | openai | response | gemini | gemini-cli, received undefined.",
			}}
		}
		return body, []InvalidParam{{
			Path: []any{}, Code: "invalid_json", Message: "请求体不是合法 JSON。",
		}}
	}
	body.raw = raw

	problems := make([]InvalidParam, 0, 2)

	// clientFormat 必填且必须落在枚举内。
	rawFormat, ok := raw["clientFormat"]
	if !ok {
		problems = append(problems, InvalidParam{
			Path: []any{"clientFormat"}, Code: "invalid_type",
			Message: "Invalid input: expected claude | openai | response | gemini | gemini-cli, received undefined.",
		})
	} else {
		var text string
		if err := json.Unmarshal(rawFormat, &text); err != nil {
			problems = append(problems, InvalidParam{
				Path: []any{"clientFormat"}, Code: "invalid_type", Message: "clientFormat 必须是字符串。",
			})
		} else if format := convert.ClientFormat(text); !isDispatchSimulatorFormat(format) {
			problems = append(problems, InvalidParam{
				Path: []any{"clientFormat"}, Code: "invalid_enum_value",
				Message: "Invalid enum value for \"clientFormat\": " + text + "。",
			})
		} else {
			body.ClientFormat = &format
		}
	}

	// modelName 可空，缺省 ""；有值时 trim 且长度 ≤255。
	if rawModel, ok := raw["modelName"]; ok {
		var text string
		if err := json.Unmarshal(rawModel, &text); err != nil {
			problems = append(problems, InvalidParam{
				Path: []any{"modelName"}, Code: "invalid_type", Message: "modelName 必须是字符串。",
			})
		} else {
			trimmed := strings.TrimSpace(text)
			if len([]rune(trimmed)) > dispatchSimulatorModelNameMaxLen {
				problems = append(problems, InvalidParam{
					Path: []any{"modelName"}, Code: "too_big", Message: "modelName 超过 255 字符。",
				})
			} else {
				body.ModelName = &trimmed
			}
		}
	}

	// groupTags 可空，缺省 []；数组内每项 trim 且 1..255，数组长度 ≤20。
	if rawTags, ok := raw["groupTags"]; ok {
		var tags []string
		if err := json.Unmarshal(rawTags, &tags); err != nil {
			problems = append(problems, InvalidParam{
				Path: []any{"groupTags"}, Code: "invalid_type", Message: "groupTags 必须是字符串数组。",
			})
		} else {
			cleaned := make([]string, 0, len(tags))
			tagProblems := make([]InvalidParam, 0)
			for index, tag := range tags {
				trimmed := strings.TrimSpace(tag)
				length := len([]rune(trimmed))
				switch {
				case length == 0:
					tagProblems = append(tagProblems, InvalidParam{
						Path: []any{"groupTags", index}, Code: "too_small", Message: "分组标签不能为空。",
					})
				case length > dispatchSimulatorGroupTagMaxLen:
					tagProblems = append(tagProblems, InvalidParam{
						Path: []any{"groupTags", index}, Code: "too_big", Message: "分组标签超过 255 字符。",
					})
				default:
					cleaned = append(cleaned, trimmed)
				}
			}
			if len(tags) > dispatchSimulatorGroupTagMax {
				tagProblems = append(tagProblems, InvalidParam{
					Path: []any{"groupTags"}, Code: "too_big", Message: "分组标签最多 20 个。",
				})
			}
			problems = append(problems, tagProblems...)
			if len(tagProblems) == 0 {
				body.GroupTags = &cleaned
			}
		}
	}

	return body, problems
}

func isDispatchSimulatorFormat(format convert.ClientFormat) bool {
	for _, candidate := range dispatchSimulatorFormats {
		if candidate == format {
			return true
		}
	}
	return false
}

// toSimulateProviders 把存储行投影成引擎输入（含模拟器独有的活动时段与重定向规则）。
func toSimulateProviders(rows []store.SimulatorProviderRow) []route.SimulateProvider {
	out := make([]route.SimulateProvider, 0, len(rows))
	for _, row := range rows {
		out = append(out, route.SimulateProvider{
			// ProviderFromStore 是 route 包对 store 行的唯一投影入口，避免这里再拼一遍字段。
			Provider:        route.ProviderFromStore(row.Provider),
			ActiveTimeStart: row.ActiveTimeStart,
			ActiveTimeEnd:   row.ActiveTimeEnd,
			ModelRedirects:  row.ModelRedirects,
		})
	}
	return out
}

func simulatorRowFor(rows []store.SimulatorProviderRow, id int64) (store.SimulatorProviderRow, bool) {
	for _, row := range rows {
		if row.ID == id {
			return row, true
		}
	}
	return store.SimulatorProviderRow{}, false
}

func derefStringOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func derefStringSlice(value *[]string) []string {
	if value == nil {
		return nil
	}
	return *value
}
