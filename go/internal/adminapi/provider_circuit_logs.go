package adminapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是「供应商熔断日志」端点：`GET /providers/{id}/circuit-logs`。
//
// 用户需求（超越 Node 的新能力）：给**已熔断的供应商**一个可点的排查入口——点开看到
// 「当前熔断状态」与「该供应商最近的错误」，用来回答「为什么它被熔断了 / 现在恢复了吗」。
//
// 为什么 Node 没有对应端点：Node 的熔断状态 Hash（`circuit_breaker:state:<id>`）只存计数与时刻
// （`src/lib/redis/circuit-breaker-state.ts:16-34` serializeState 的全部字段），**没有错误历史**。
// 故本端点的两块数据都来自别处组装：
//   - 熔断状态：既有 `ProviderCircuitStore`（providers_health.go）——与 `/providers/health` 同源；
//   - 最近错误：`message_request`（两条来源，见 store.AdminProviderCircuitFailures 的注释），
//     即**用户真正要的「错误日志」**。
//
// 设计取舍与理由详见 （含「不新增熔断迁移事件存储」的论证）。
// 命名约定：标识符带 circuitLogs 前缀，避免与同包并行 lane 撞符号。

const (
	// circuitLogsDefaultLimit 是「最近错误」的默认条数。
	circuitLogsDefaultLimit = 20
	// circuitLogsMaxLimit 是硬上限：一次问太多条会让首屏被一个供应商的历史淹没。
	circuitLogsMaxLimit = 100
	// circuitLogsDefaultLookbackHours 是默认回溯窗（24 小时）。
	circuitLogsDefaultLookbackHours = 24
	// circuitLogsMaxLookbackHours 是回溯窗硬上限（7 天）。
	//
	// **有界是刻意的**：链内失败那一路要展开 `provider_chain` 的 jsonb，没有索引可用，
	// 时间窗是把它按在万行量级的唯一手段（实测生产 24h ≈ 9.7k 行、全表 3.5 万行）。
	circuitLogsMaxLookbackHours = 168
)

// circuitLogsState 是响应的「当前熔断状态」块。
//
// `Available=false` 时数值字段一律为 nil：**不用 0 冒充**——0 是合法读数（闭态且从未失败），
// 与「读不到」必须可区分。这也是同族端点 `/providers/health` 的既有纪律
// （providers_health.go:198-199：读不到显示「无数据」，不把整页打成错误）。
//
// 指针字段的 JSON 标签统一 camelCase，与既有 providers 端点同风格。
type circuitLogsState struct {
	Available            bool    `json:"available"`
	CircuitState         *string `json:"circuitState"`
	FailureCount         *int64  `json:"failureCount"`
	LastFailureTime      *int64  `json:"lastFailureTime"`
	CircuitOpenUntil     *int64  `json:"circuitOpenUntil"`
	HalfOpenSuccessCount *int64  `json:"halfOpenSuccessCount"`
	// RecoveryMinutes 是距 `circuitOpenUntil` 还有几分钟（向上取整）；未开启或已到期时为 nil。
	// 口径与 `/providers/health` 的 recoveryMinutes 一致。
	RecoveryMinutes *int64 `json:"recoveryMinutes"`
	// ConsecutiveOpenCount 是等待阶梯的级数 n（0 = 从未爬过阶梯，含阶梯未启用）。
	ConsecutiveOpenCount *int64 `json:"consecutiveOpenCount"`
	// ConsecutiveOpenCountChangedAt 是级数**最近一次变化**的时刻；从未变化即 nil。
	//
	// 为何只能给「当前值 + 最近一次变化时间」：历史级数序列在 Redis 里不存在
	// （哈希只有当前值），回溯不了。界面必须**如实说明**这一点，不能编一条时间线。
	ConsecutiveOpenCountChangedAt *int64 `json:"consecutiveOpenCountChangedAt"`
	// OpenWindowMinutes 是**本次**开闸窗口的时长（分钟，向上取整）；求不出时 nil。
	// 口径与 `/providers/health` 同源（providerCircuitOpenWindowMinutes，一处实现）。
	OpenWindowMinutes *int64 `json:"openWindowMinutes"`
	// UnavailableReason 只在 Available=false 时给出（例如 redis_unavailable）。
	UnavailableReason *string `json:"unavailableReason"`
}

// circuitLogsThresholds 是熔断阈值块。
//
// **来源是供应商行**（`providers.circuit_breaker_*` 三列），与写路径同步 Redis 哈希时的取数同源
// （provider_circuit.go:102 providerThresholdsFromRow 用的就是这三列）。给出它是为了回答
// 「离熔断还差几次」= failureThreshold - failureCount。
//
// 已知边界（如实登记）：数据面读的是 `circuit_breaker:config:<id>` 哈希，而本端点读的是行。
// 正常情况下写路径（管理面每次写供应商）会把行值同步进哈希，两者一致；但**若有人绕过管理面
// 直接改哈希**，这里显示的阈值会与生效值不同。本端点不为此去读哈希：Deps 没有该哈希的读缝
// （只有 `ProviderCircuitConfigWriter` 写缝），为展示一个可从行推出的值去新增跨面接口不划算。
type circuitLogsThresholds struct {
	FailureThreshold         int64 `json:"failureThreshold"`
	OpenDuration             int64 `json:"openDuration"`
	HalfOpenSuccessThreshold int64 `json:"halfOpenSuccessThreshold"`
}

// circuitLogsWindow 描述「最近错误」的时间范围——界面上必须显示它，
// 否则运维会把「24 小时内没有错误」误读成「从来没有错误」。
type circuitLogsWindow struct {
	Limit         int    `json:"limit"`
	LookbackHours int    `json:"lookbackHours"`
	Since         string `json:"since"`
}

// circuitLogsError 是响应里的一条错误记录。
type circuitLogsError struct {
	RequestID int64   `json:"requestId"`
	CreatedAt string  `json:"createdAt"`
	Model     *string `json:"model"`
	// StatusCode 为空表示「没有 HTTP 状态码」（本地拒绝、客户端中断、在途未结算）。
	StatusCode   *int    `json:"statusCode"`
	ErrorMessage *string `json:"errorMessage"`
	DurationMS   *int    `json:"durationMs"`
	Endpoint     *string `json:"endpoint"`
	// Source 是这条记录从哪来：direct（行级事实）或 chain（该行链内的失败条目）。
	Source string `json:"source"`
	// ChainReason 只在 source=chain 时可能非空：链上的结局词（如 retry_failed）。
	ChainReason *string `json:"chainReason"`
	// Redacted 表示 errorMessage **被本端点脱敏改写过**。
	//
	// 为什么值得单独给一个布尔：运维看到 `[REDACTED_KEY]` 需要知道「这是改写后的文案，不是上游
	// 原话」——否则会拿着改写的串去搜上游日志而搜不到。
	Redacted bool `json:"redacted"`
}

// circuitLogsResponse 是本端点的响应体。
type circuitLogsResponse struct {
	ProviderID int64 `json:"providerId"`
	// Circuit 恒存在（即使不可用），只是 Available=false。
	Circuit    circuitLogsState      `json:"circuit"`
	Thresholds circuitLogsThresholds `json:"thresholds"`
	Window     circuitLogsWindow     `json:"window"`
	Errors     []circuitLogsError    `json:"errors"`
	// ErrorsUnavailableReason 只在「库读不到」时给出；此时 Errors 为空数组而非 null
	// （前端不必为 null 与 [] 各写一条分支）。
	ErrorsUnavailableReason *string `json:"errorsUnavailableReason"`
}

// RegisterProviderCircuitLogs 注册熔断日志端点。
//
// 与同族三条运行态端点同一 fail-closed 策略：`Deps.CircuitStates` 不支持供应商级读时**不注册**，
// 原样回退 Node——注册一个必然失败的路由，比不注册坏得多（providers.go:364-365 同一理由）。
//
// 为什么独立成一个 registrar（而不是塞进 RegisterProviders 的那段 if）：本 lane 的文件归属只到
// 新文件，改 providers.go 会与并行 lane 抢同一区块；故由协调者在 cmd/cchd 装配处调用本函数。
func RegisterProviderCircuitLogs(router *Router, deps Deps) {
	circuits, ok := providerCircuitStore(deps)
	// Store 或熔断面任一未装配即不注册（回退 Node）：可见性检查要 PG，熔断状态要 Redis，
	// 缺任一侧都只能答半张表——与同族 RegisterProviderCircuitRoutes 同一前提
	// （provider_circuit_admin.go:401）。
	if !ok || circuits == nil || deps.Store == nil {
		if deps.Logger != nil {
			deps.Logger.Warn("admin_provider_circuit_logs_unwired", map[string]any{
				"reason": "装配不足，该路由不注册、回退 Node",
				"store":  deps.Store != nil,
				"redis":  ok && circuits != nil,
			})
		}
		return
	}
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/providers/{id:[0-9]+}/circuit-logs",
		Access:      AccessAdmin,
		Module:      "providers",
		OperationID: "getProviderCircuitLogs",
		Handler:     http.HandlerFunc(handleProviderCircuitLogs(deps)),
	})
}

// handleProviderCircuitLogs 组装两块数据。
//
// 两块**各自独立降级**：Redis 读不到只影响 circuit 块，错误列表照给；库读不到只影响 errors 块，
// 熔断状态照给。任一侧的失败都不把端点打成 5xx——排障入口在最需要它的时候（组件出问题时）
// 必须还能打开。
func handleProviderCircuitLogs(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		circuits, ok := providerCircuitStore(deps)
		if !ok || circuits == nil {
			adminProblemWriter(deps).WriteActionError(writer, request,
				NewActionError("provider", "provider.unavailable", http.StatusServiceUnavailable,
					errors.New("熔断状态存储未接线")))
			return
		}

		id, ok := providerIDParam(writer, request)
		if !ok {
			return
		}
		if deps.Store == nil {
			// 注册前提已保证 Store 非空（RegisterProviderCircuitLogs）；此处仍是**显式**失败
			// 而不是静默降级：没有 Store 就没有「可见性」这道门，绝不能放行。
			adminProblemWriter(deps).WriteActionError(writer, request,
				NewActionError("provider", "provider.unavailable", http.StatusServiceUnavailable,
					errors.New("连接池未接线")))
			return
		}
		// 可见性：与同族端点同一道门（隐藏类型的供应商对非 compat 请求不可见 → 404）。
		// 顺带拿到行，用于给出阈值块，避免再查一次库。
		provider, err := providerFindVisible(request, deps, id)
		if err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, err)
			return
		}

		limit, lookbackHours, ok := circuitLogsQuery(writer, request)
		if !ok {
			return
		}

		since := time.Now().Add(-time.Duration(lookbackHours) * time.Hour)
		response := circuitLogsResponse{
			ProviderID: id,
			Circuit:    readCircuitLogsState(request.Context(), circuits, id),
			Thresholds: circuitLogsThresholdsFrom(providerThresholdsFromRow(provider)),
			Window: circuitLogsWindow{
				Limit:         limit,
				LookbackHours: lookbackHours,
				Since:         since.UTC().Format("2006-01-02T15:04:05.000Z"),
			},
			Errors: []circuitLogsError{},
		}

		if deps.Store == nil {
			reason := "store_unavailable"
			response.ErrorsUnavailableReason = &reason
			adminWriteJSON(writer, http.StatusOK, response)
			return
		}
		failures, failuresErr := deps.Store.AdminProviderCircuitFailures(
			request.Context(), id, since, limit,
		)
		if failuresErr != nil {
			// 读不到错误**不是**端点的失败：熔断状态那一块仍然有价值（用户至少能看到「现在开着、
			// 还有几分钟恢复」）。给 200 + 明确原因，比给 500 让人以为整个排障入口坏了更有用。
			reason := "store_query_failed"
			response.ErrorsUnavailableReason = &reason
			adminLoggerOf(deps).Warn("admin_provider_circuit_logs_query_failed", map[string]any{
				"providerId": id,
				"error":      failuresErr.Error(),
			})
			adminWriteJSON(writer, http.StatusOK, response)
			return
		}
		response.Errors = circuitLogsErrorsFrom(failures)
		adminWriteJSON(writer, http.StatusOK, response)
	}
}

// readCircuitLogsState 读熔断状态；读失败时返回 Available=false 而不是报错。
func readCircuitLogsState(
	ctx context.Context,
	circuits ProviderCircuitStore,
	providerID int64,
) circuitLogsState {
	snapshot, err := circuits.ProviderCircuit(ctx, providerID)
	if err != nil {
		reason := "redis_unavailable"
		return circuitLogsState{Available: false, UnavailableReason: &reason}
	}
	// 回**有效态**而不是 Redis 原值：窗口过期的 open 在数据面是按 half-open 放行的
	// （route.ProviderOpen 走的也是同一个求值函数），若这里照原值报「已熔断」，
	// 就会出现「弹窗说熔断、列表说恢复中、请求照过」的三方分叉。
	// 原始读数（circuitOpenUntil / failureCount / halfOpenSuccessCount）仍原样返回，供排障。
	effectiveState := string(route.EffectiveProviderState(
		route.CircuitState(snapshot.CircuitState),
		int64OrZero(snapshot.CircuitOpenUntilMS),
		time.Now().UnixMilli(),
	))
	state := effectiveState
	failureCount := snapshot.FailureCount
	halfOpen := snapshot.HalfOpenSuccessCount
	// 阶梯读数是**原始**事实（爬了几级），不随窗口过期（open → 有效 half-open）而变，
	// 故与 circuitOpenUntil 一样原样返回。
	ladderLevel := snapshot.ConsecutiveOpenCount
	result := circuitLogsState{
		Available:                     true,
		CircuitState:                  &state,
		FailureCount:                  &failureCount,
		LastFailureTime:               snapshot.LastFailureTimeMS,
		CircuitOpenUntil:              snapshot.CircuitOpenUntilMS,
		HalfOpenSuccessCount:          &halfOpen,
		RecoveryMinutes:               circuitLogsRecoveryMinutes(snapshot.CircuitOpenUntilMS),
		ConsecutiveOpenCount:          &ladderLevel,
		ConsecutiveOpenCountChangedAt: snapshot.ConsecutiveOpenCountChangedAtMS,
		OpenWindowMinutes: providerCircuitOpenWindowMinutes(
			snapshot.CircuitOpenUntilMS,
			snapshot.ConsecutiveOpenCountChangedAtMS,
			snapshot.ConsecutiveOpenCount,
		),
	}
	return result
}

// circuitLogsRecoveryMinutes 复刻 /providers/health 的 recoveryMinutes 口径：ceil 到分钟。
//
// 到期或未开启（空/0）时为 nil——「还有 0 分钟」与「没在恢复倒计时」是两件事。
func circuitLogsRecoveryMinutes(openUntilMS *int64) *int64 {
	if openUntilMS == nil || *openUntilMS == 0 {
		return nil
	}
	remaining := *openUntilMS - time.Now().UnixMilli()
	if remaining <= 0 {
		return nil
	}
	minutes := (remaining + int64(time.Minute/time.Millisecond) - 1) / int64(time.Minute/time.Millisecond)
	return &minutes
}

// circuitLogsThresholdsFrom 把 route 包的阈値结构投影成响应形状。
func circuitLogsThresholdsFrom(thresholds providerCircuitThresholds) circuitLogsThresholds {
	return circuitLogsThresholds{
		FailureThreshold:         thresholds.FailureThreshold,
		OpenDuration:             thresholds.OpenDurationMS,
		HalfOpenSuccessThreshold: thresholds.HalfOpenSuccessThreshold,
	}
}

// circuitLogsErrorsFrom 把库行投影成响应形状，并在读路径上做值形态脱敏。
//
// 脱敏放在**读路径**（而不是写路径）是有意的：`message_request.error_message` 是按 Node 原样
// 落库的（写入侧不做形态脱敏——见 guard/adapters_fake200.go:360-362 的既有取舍：凭据脱敏属
// 日志层职责）。本端点是**对外展示**它们的那一层，故由它承担。
func circuitLogsErrorsFrom(failures []store.AdminProviderCircuitFailure) []circuitLogsError {
	out := make([]circuitLogsError, 0, len(failures))
	for _, failure := range failures {
		message, changed := redactErrorTextPointer(failure.ErrorMessage)
		out = append(out, circuitLogsError{
			RequestID:    failure.RequestID,
			CreatedAt:    failure.CreatedAt,
			Model:        failure.Model,
			StatusCode:   failure.StatusCode,
			ErrorMessage: message,
			DurationMS:   failure.DurationMS,
			Endpoint:     failure.Endpoint,
			Source:       failure.Source,
			ChainReason:  failure.ChainReason,
			Redacted:     changed,
		})
	}
	return out
}

// circuitLogsQuery 解析并校验 limit 与 lookbackHours。
//
// 校验码用 zod 的语义码（too_small / too_big / invalid_type），与仓库其它查询端点的信封一致，
// 便于前端按同一套规则渲染「最小值/最大值」提示（本仓已修过一次方向写反的事故，见
func circuitLogsQuery(writer http.ResponseWriter, request *http.Request) (int, int, bool) {
	limit, ok := circuitLogsIntParam(writer, request, "limit",
		circuitLogsDefaultLimit, 1, circuitLogsMaxLimit)
	if !ok {
		return 0, 0, false
	}
	lookback, ok := circuitLogsIntParam(writer, request, "lookbackHours",
		circuitLogsDefaultLookbackHours, 1, circuitLogsMaxLookbackHours)
	if !ok {
		return 0, 0, false
	}
	return limit, lookback, true
}

// circuitLogsIntParam 读一个可选整数查询参数并做区间校验。
func circuitLogsIntParam(
	writer http.ResponseWriter,
	request *http.Request,
	key string,
	fallback, min, max int,
) (int, bool) {
	raw := request.URL.Query().Get(key)
	if raw == "" {
		return fallback, true
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		adminWriteValidationFailure(writer, request, []invalidParam{{
			Path:    []any{key},
			Code:    "invalid_type",
			Message: "Expected number, received nan",
		}})
		return 0, false
	}
	if parsed < min {
		adminWriteValidationFailure(writer, request, []invalidParam{{
			Path:    []any{key},
			Code:    "too_small",
			Message: "Number must be greater than or equal to " + strconv.Itoa(min),
		}})
		return 0, false
	}
	if parsed > max {
		adminWriteValidationFailure(writer, request, []invalidParam{{
			Path:    []any{key},
			Code:    "too_big",
			Message: "Number must be less than or equal to " + strconv.Itoa(max),
		}})
		return 0, false
	}
	return parsed, true
}
