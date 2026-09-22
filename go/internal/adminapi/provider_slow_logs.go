package adminapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/slowlog"
)

// 本文件是「供应商低速日志」端点：`GET /providers/{id}/slow-logs`。
//
// 与同族的熔断日志端点（provider_circuit_logs.go）配对：那个回答「为什么它被熔断了」，
// 本端点回答「为什么它被低速降权了、什么时候恢复的」。前端把两者放在同一弹窗里 tab 切换。
//
// **数据源是 Redis 事件流，不是表**：熔断日志本身也没有事件表（状态在 Redis、错误行是
// message_request 的实时投影），故低速侧同样只落 Redis——见 internal/slowlog 的包注释里
// 关于「为什么不建表」与「代价（不持久）」的登记。
//
// 命名约定：标识符带 slowLogs 前缀，避免与同包并行 lane 撞符号。

const (
	// slowLogsDefaultLimit 是事件的默认条数，与熔断日志同档。
	slowLogsDefaultLimit = slowlog.DefaultLimit
	// slowLogsMaxLimit 是硬上限，与熔断日志一致。
	slowLogsMaxLimit = slowlog.MaxLimit
	// slowLogsRetentionHours 是事件流的寿命（小时），即界面上「时间范围」要写的数。
	//
	// 它是**常量而非从 Redis 读 TTL**：事件流的 TTL 是写入时定的（slowlog.EventTTL），
	// 而 Redis 的 TTL 只对具体键有意义——键还不存在时读不到任何 TTL，界面就无从显示范围。
	// 故把它当契约常量，与 slowlog.EventTTL 同值（下方有一致性断言）。
	slowLogsRetentionHours = 24
)

// slowLogsEvent 是响应里的一条事件。
//
// 指针字段用 nil 表达「本事件不含此维」，与写入侧 slowlog.Event 同语义：惩罚事件没有 median，
// 基线事件没有 penalty。用 0 冒充会让界面显示「中位数 0」。
type slowLogsEvent struct {
	Kind        string   `json:"kind"`
	At          int64    `json:"at"`
	ModelKey    *string  `json:"modelKey"`
	PenaltyFrom *int     `json:"penaltyFrom"`
	PenaltyTo   *int     `json:"penaltyTo"`
	Median      *float64 `json:"median"`
	Samples     *int64   `json:"samples"`
	Reason      *string  `json:"reason"`
}

// slowLogsWindow 描述事件的时间范围——界面上必须显示它，否则运维会把
// 「24 小时内没有降权」误读成「从来没有降权」。
type slowLogsWindow struct {
	Limit          int    `json:"limit"`
	RetentionHours int    `json:"retentionHours"`
	Since          string `json:"since"`
}

// slowLogsResponse 是本端点的响应体。
type slowLogsResponse struct {
	ProviderID int64           `json:"providerId"`
	Window     slowLogsWindow  `json:"window"`
	Events     []slowLogsEvent `json:"events"`
	// Diverts 是窗口内「因低速被改道」的请求数（用户 2026-09-22 需求）。
	//
	// 与 Events 的关系：Events 回答「什么时候被压了/恢复了」（逐条事件），
	// 本字段回答「压下来之后实际挡掉了多少流量」（聚合读数）。前者是原因，后者是后果。
	//
	// nil 表示未装配该读面（不区分「无改道」与「读不了」）。
	Diverts *slowLogsDiverts `json:"diverts"`
	// UnavailableReason 只在「读不到」时给出；此时 Events 为空数组而非 null
	// （前端不必为 null 与 [] 各写一条分支）。
	UnavailableReason *string `json:"unavailableReason"`
}

// slowLogsDiverts 是改道读数的响应形状。
//
// 两个分项都要，且不合并成一个总数：成因不同（会话冷却 vs 渠道降权），运维的下一步动作也不同。
type slowLogsDiverts struct {
	WindowHours int   `json:"windowHours"`
	Total       int64 `json:"total"`
	Cooldown    int64 `json:"cooldown"`
	Penalty     int64 `json:"penalty"`
}

// RegisterProviderSlowLogs 注册低速日志端点。
//
// fail-closed：`Deps.SlowLogs` 未装配（无 Redis 命令连接）时**不注册**，原样回退 Node——
// 注册一个必然失败的路由，比不注册坏得多（与 provider_circuit_logs.go:139 同一前提）。
// 与熔断日志不同的是：本端点**不需要 Store**（不查库），故可见性门改用
// `providerFindVisible`——它本身要 Store，故仍需 Store 非空。
func RegisterProviderSlowLogs(router *Router, deps Deps) {
	if deps.SlowLogs == nil || deps.Store == nil {
		if deps.Logger != nil {
			deps.Logger.Warn("admin_provider_slow_logs_unwired", map[string]any{
				"reason": "装配不足，该路由不注册、回退 Node",
				"store":  deps.Store != nil,
				"redis":  deps.SlowLogs != nil,
			})
		}
		return
	}
	router.Add(Route{
		Method:      http.MethodGet,
		Path:        "/providers/{id:[0-9]+}/slow-logs",
		Access:      AccessAdmin,
		Module:      "providers",
		OperationID: "getProviderSlowLogs",
		Handler:     http.HandlerFunc(handleProviderSlowLogs(deps)),
	})
}

// handleProviderSlowLogs 读该渠道的近期低速事件。
//
// 读失败**不把端点打成 5xx**：给 200 + 明确原因，比给 500 让人以为整个排障入口坏了更有用
// （与熔断日志的两块独立降级同一纪律）。
func handleProviderSlowLogs(deps Deps) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if deps.SlowLogs == nil || deps.Store == nil {
			adminProblemWriter(deps).WriteActionError(writer, request,
				NewActionError("provider", "provider.unavailable", http.StatusServiceUnavailable,
					errors.New("低速日志存储未接线")))
			return
		}
		id, ok := providerIDParam(writer, request)
		if !ok {
			return
		}
		// 可见性：与同族端点同一道门（隐藏类型的供应商对非 compat 请求不可见 -> 404）。
		if _, err := providerFindVisible(request, deps, id); err != nil {
			adminProblemWriter(deps).WriteActionError(writer, request, err)
			return
		}
		limit, ok := circuitLogsIntParam(writer, request, "limit",
			slowLogsDefaultLimit, 1, slowLogsMaxLimit)
		if !ok {
			return
		}

		response := slowLogsResponse{
			ProviderID: id,
			Window: slowLogsWindow{
				Limit:          limit,
				RetentionHours: slowLogsRetentionHours,
				Since:          slowLogsWindowSince(),
			},
			Events: []slowLogsEvent{},
		}
		events, err := deps.SlowLogs.Recent(request.Context(), id, limit)
		if err != nil {
			reason := "redis_unavailable"
			response.UnavailableReason = &reason
			adminLoggerOf(deps).Warn("admin_provider_slow_logs_query_failed", map[string]any{
				"providerId": id,
				"error":      err.Error(),
			})
			adminWriteJSON(writer, http.StatusOK, response)
			return
		}
		response.Events = slowLogsEventsFrom(events)
		if deps.SlowDiverts != nil {
			snapshot, divertErr := deps.SlowDiverts.ReadDivert(request.Context(), id, time.Now())
			if divertErr != nil {
				// 与事件流同纪律：读失败不把端点打成 5xx。改道读数缺失不影响事件流的可用性，
				// 故只补一条原因不覆盖已有的 unavailableReason（那个描述的是 events）。
				adminLoggerOf(deps).Warn("admin_provider_slow_diverts_query_failed", map[string]any{
					"providerId": id,
					"error":      divertErr.Error(),
				})
			} else {
				response.Diverts = &slowLogsDiverts{
					WindowHours: snapshot.WindowHours,
					Total:       snapshot.Total(),
					Cooldown:    snapshot.Cooldown,
					Penalty:     snapshot.Penalty,
				}
			}
		}
		adminWriteJSON(writer, http.StatusOK, response)
	}
}

// slowLogsEventsFrom 把存储事件投影成响应形状。
func slowLogsEventsFrom(events []slowlog.Event) []slowLogsEvent {
	out := make([]slowLogsEvent, 0, len(events))
	for _, event := range events {
		out = append(out, slowLogsEvent{
			Kind:        string(event.Kind),
			At:          event.At,
			ModelKey:    optionalString(event.ModelKey, event.ModelKey != ""),
			PenaltyFrom: event.PenaltyFrom,
			PenaltyTo:   event.PenaltyTo,
			Median:      event.Median,
			Samples:     event.Samples,
			Reason:      optionalString(event.Reason, event.Reason != ""),
		})
	}
	return out
}

// slowLogsWindowSince 是本端点所覆盖时间窗的起点（ISO 格式），与熔断日志的 window.since 同形。
func slowLogsWindowSince() string {
	return time.Now().Add(-time.Duration(slowLogsRetentionHours) * time.Hour).
		UTC().Format("2006-01-02T15:04:05.000Z")
}
