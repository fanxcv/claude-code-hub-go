package adminapi

import (
	"errors"
	"net/http"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/slowlog"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
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
	// Quarantine 是**当前隔离运行态**（每 (模型) 组合一行）。
	//
	// 与前两者回答的问题不同：Events/Diverts 是「历史上发生了什么」，本块是「机制现在在哪一档」
	// ——隔离是否落态、放行多少比例、滑窗里还有几条慢样本、探针租约在不在。用户 2026-09-22
	// 要求「不再翻生产 Redis」，本块就是那个替代品。
	//
	// nil 表示未装配该读面（不区分「无组合」与「读不了」）。
	Quarantine *slowLogsQuarantine `json:"quarantine"`
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

// slowLogsQuarantine 是隔离运行态的响应形状。
//
// 与 slowLogsDiverts 同一条纪律：读失败不把端点打成 5xx，而是给空表 + 明确原因——
// 此时 `combinations` 为空数组而非 null（前端不必为 null 与 [] 各写一条分支）。
type slowLogsQuarantine struct {
	Combinations []slowLogsQuarantineCombination `json:"combinations"`
	// UnavailableReason 只在「读不到」时给出（与顶层同名字段同一取值口径）。
	UnavailableReason *string `json:"unavailableReason"`
}

// slowLogsQuarantineCombination 是一个 (渠道, 模型) 组合的隔离运行态。
//
// 字段分两层：原始事实（stateExists / cleanStreak / sampleLiveCount / baselineUsable / 租约）
// 与派生读数（penalty / quarantined / admissionPermille）。只给派生值会让「为什么它没被挡」
// 无从归因：没慢过？基线没了？还是窗里已经没样本了？
type slowLogsQuarantineCombination struct {
	ModelKey string `json:"modelKey"`
	// StateExists 为真表示该组合真的被计过惩罚（状态键只由慢路径创建）。
	StateExists bool `json:"stateExists"`
	// Quarantined 是选路侧当前是否会把它当隔离（活窗派生的惩罚为正 + 基线可用）。
	// 它与「状态里那个 quarantine 字段」不是一回事：状态键一创建就带着该字段，
	// 而活窗空了就不再挡流量——本字段是「现在挡不挡」。
	Quarantined bool `json:"quarantined"`
	// Penalty 是当前生效降权量（活窗派生；参数缺失时回退状态快照）。
	Penalty int `json:"penalty"`
	// CleanStreak 是连续干净样本数，即准入阶梯的唯一输入。
	CleanStreak int `json:"cleanStreak"`
	// AdmissionPermille 是当前有效放行比例（千分比）：1000 = 不挡流量。
	AdmissionPermille int `json:"admissionPermille"`
	// SampleLiveCount 是活窗内的慢样本数（与选路同一下界）。
	SampleLiveCount int `json:"sampleLiveCount"`
	// BaselineUsable 为假表示这条组合**没在监控范围内**（写侧无可用基线即整段跳过），
	// 此时上面所有计数必然为 0——读数的含义是「没在看」而非「看过没慢」。
	BaselineUsable bool `json:"baselineUsable"`
	// ProbeLeaseHeld 表示探针租约键存在（有探针在飞，或刚飞完还没到 TTL）。
	ProbeLeaseHeld bool `json:"probeLeaseHeld"`
	// ProbeLeaseTTLMillis 是租约剩余毫秒；-1 表示无租约键。
	ProbeLeaseTTLMillis int64 `json:"probeLeaseTtlMillis"`
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
		provider, err := providerFindVisible(request, deps, id)
		if err != nil {
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
		// 隔离运行态与事件流**互相独立**：先读它，因为下面事件流读失败会提前作答，
		// 而「读不到事件流」恰恰是运维最需要看隔离态的时候。
		slowLogsAttachQuarantine(&response, request, deps, provider)
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

// slowLogsAttachQuarantine 把隔离运行态读进响应；读失败只补 unavailableReason，不改状态码。
//
// 候选构造复用 providers_health.go 的 slowRateCandidates：它带渠道行的四个降权参数，
// 读侧据此算活窗下界与档位——只传 id 会让管理面回退状态里的旧参数，与数据面读数分叉。
func slowLogsAttachQuarantine(
	response *slowLogsResponse,
	request *http.Request,
	deps Deps,
	provider *store.AdminProvider,
) {
	if deps.SlowRateStates == nil {
		return
	}
	observations, err := deps.SlowRateStates.ObserveStates(
		request.Context(),
		slowRateCandidates([]store.AdminProvider{*provider})[0],
	)
	if err != nil {
		reason := "redis_unavailable"
		response.Quarantine = &slowLogsQuarantine{
			Combinations:      []slowLogsQuarantineCombination{},
			UnavailableReason: &reason,
		}
		adminLoggerOf(deps).Warn("admin_provider_slow_states_query_failed", map[string]any{
			"providerId": provider.ID,
			"error":      err.Error(),
		})
		return
	}
	response.Quarantine = &slowLogsQuarantine{
		Combinations: slowLogsQuarantineCombinations(observations),
	}
}

// slowLogsQuarantineCombinations 把读侧观测投影成响应形状。
func slowLogsQuarantineCombinations(
	observations []route.SlowRateStateObservation,
) []slowLogsQuarantineCombination {
	out := make([]slowLogsQuarantineCombination, 0, len(observations))
	for _, observation := range observations {
		// 无租约键时读侧的 TTL 为负（Redis PTTL 语义），对外统一成 -1：
		// 消费方只需要「有没有」与「还剩多久」，不需要区分 Redis 内部的 -1/-2。
		leaseMillis := int64(-1)
		if observation.ProbeLeaseHeld {
			leaseMillis = int64(observation.ProbeLeaseTTL / time.Millisecond)
		}
		out = append(out, slowLogsQuarantineCombination{
			ModelKey:            observation.ModelKey,
			StateExists:         observation.StateExists,
			Quarantined:         observation.Quarantined,
			Penalty:             observation.Penalty,
			CleanStreak:         observation.CleanStreak,
			AdmissionPermille:   observation.AdmissionPermille,
			SampleLiveCount:     observation.SampleLiveCount,
			BaselineUsable:      observation.BaselineUsable,
			ProbeLeaseHeld:      observation.ProbeLeaseHeld,
			ProbeLeaseTTLMillis: leaseMillis,
		})
	}
	return out
}

// 编译期断言：数据面那份读取器同时是隔离态的读面（装配处直接把同一个实例塞进
// Deps.SlowRateStates，不必再造一个实现）。
var _ SlowRateStateReader = (*route.SlowRateReader)(nil)

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
