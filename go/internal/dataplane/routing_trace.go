package dataplane

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
)

// 本文件构造 Node 的 `routing_trace` 列（`src/types/routing-trace.ts` 的 `RoutingTraceV1`）。
//
// 背景：审计实测 Go 侧**只投影不产出**——
// `Settlement.RoutingTrace` 非测试代码零赋值、`terminal/patch.go` 默认 `{}`，于是详情页的
// 决策链视图恒空。Node 侧由 `forwarder.ts:1702` 的 discovery/竞速子系统写。
//
// 三条口径，全部为了「不编造」：
//
//  1. **只记 Go 真实跑过的路径**：Go 没有 discovery/竞速探针子系统，故**永不**产出
//     `discovery` 模式，也**不产出** sticky_probe / sticky_timeout / round_started 一类事件
//     （那会声称一个不存在的机制）。Go 的串行路径记 `single_upstream`，竞速路径记
//     `legacy_hedge`——两者都是 Node 模式枚举里的**同名既有模式**，语义一致。
//  2. **只写实测事实**：事件时间戳一律取自 forward 层的真实测量
//     （`AttemptOutcome.StartedAt/FinishedAt`、`HedgeTraceSink`）。没测到的事件**不产出**，
//     不拿别的时刻顶替，也不靠累加推算时间线。
//  3. **未知就省略**：`bypassReason` 不写（Go 不声称「为何绕过 discovery」）、`config` 不写
//     （Go 不读 discoverySlaMs 那组参数）、`ttftMs` 无实测时写 null。
//
// 上限与截断对齐 Node：`ROUTING_TRACE_MAX_EVENTS = 512`，超出置 `truncated: true`。

// routingTraceMaxEvents 对齐 Node 的 ROUTING_TRACE_MAX_EVENTS。
const routingTraceMaxEvents = 512

// 模式取值，与 Node 的 RoutingTraceMode 同名。
const (
	routingTraceModeSingleUpstream = "single_upstream"
	routingTraceModeLegacyHedge    = "legacy_hedge"
)

// 事件类型，只取 Node 枚举里**本构造器会产出**的那几个。
const (
	routingTraceEventRequestStarted   = "request_started"
	routingTraceEventAttemptStarted   = "attempt_started"
	routingTraceEventAttemptFinished  = "attempt_finished"
	routingTraceEventWinnerCommitted  = "winner_committed"
	routingTraceEventRequestFinished  = "request_finished"
	routingTraceEventHedgeSaturated   = "hedge_slot_saturated"
	routingTraceAttemptKindNormal     = "normal"
	routingTraceAttemptKindFallback   = "fallback"
	routingTraceOutcomeSuccess        = "success"
	routingTraceOutcomeFailed         = "failed"
	routingTraceOutcomeClientAbort    = "client_abort"
	routingTraceWinnerOriginNormal    = "normal"
	routingTraceWinnerOriginNone      = "none"
	routingTraceHedgeReasonPrefix     = "hedge_"
	routingTraceClientAbortReasonPart = "client_abort"
)

// routingTraceV1 是 Node `RoutingTraceV1` 的落库形状（字段名与可选性逐项对齐）。
type routingTraceV1 struct {
	Version          int                    `json:"version"`
	Mode             string                 `json:"mode"`
	StartedAt        int64                  `json:"startedAt"`
	UpdatedAt        int64                  `json:"updatedAt"`
	DiscoveryEnabled bool                   `json:"discoveryEnabled"`
	Eligible         bool                   `json:"eligible"`
	Events           []routingTraceEventV1  `json:"events"`
	Summary          *routingTraceSummaryV1 `json:"summary,omitempty"`
	Truncated        bool                   `json:"truncated,omitempty"`
}

type routingTraceProviderV1 struct {
	ID   int64  `json:"id"`
	Name string `json:"name,omitempty"`
}

type routingTraceEventV1 struct {
	Type               string                  `json:"type"`
	At                 int64                   `json:"at"`
	ElapsedMs          int64                   `json:"elapsedMs"`
	AttemptID          string                  `json:"attemptId,omitempty"`
	AttemptKind        string                  `json:"attemptKind,omitempty"`
	Provider           *routingTraceProviderV1 `json:"provider,omitempty"`
	Outcome            string                  `json:"outcome,omitempty"`
	CancellationKind   string                  `json:"cancellationKind,omitempty"`
	ActiveAttemptCount int                     `json:"activeAttemptCount,omitempty"`
	ConfiguredCap      int                     `json:"configuredCap,omitempty"`
	StatusCode         int                     `json:"statusCode,omitempty"`
	Reason             string                  `json:"reason,omitempty"`
	DurationMs         int64                   `json:"durationMs,omitempty"`
}

type routingTraceSummaryV1 struct {
	Outcome            string `json:"outcome"`
	StatusCode         int    `json:"statusCode"`
	DurationMs         int64  `json:"durationMs"`
	TTFTMs             *int64 `json:"ttftMs"`
	AttemptsPerRequest int    `json:"attemptsPerRequest"`
	MaxActiveAttempts  int    `json:"maxActiveAttempts"`
	Rounds             int    `json:"rounds"`
	ProviderMs         int64  `json:"providerMs"`
	FallbackPromotions int    `json:"fallbackPromotions"`
	CancelFailures     int    `json:"cancelFailures"`
	WinnerOrigin       string `json:"winnerOrigin"`
	WinnerProviderID   *int64 `json:"winnerProviderId"`
	WinnerRound        *int   `json:"winnerRound"`
}

// routingTraceFacts 是构造一次 trace 所需的全部事实。
type routingTraceFacts struct {
	// StartedAt 是请求进入数据面的时刻（`RequestState.StartedAt`）。
	StartedAt time.Time
	// FinishedAt 是终态时刻；零值时用 StartedAt+DurationMS 回填（两者同源，非推算）。
	FinishedAt time.Time
	// Attempts 是本次请求的真实尝试留痕（含实测起止时刻，见 forward.AttemptOutcome）。
	Attempts []forward.AttemptOutcome
	// Saturations 来自 HedgeTraceSink：竞速并发已满、候选未启动的**实测**时刻与并发数。
	Saturations []routingTraceEventV1
	StatusCode  int
	DurationMS  int64
	// FirstByteMS / TTFTMS 为 nil 表示本次没有实测值（例如非流式），此时 summary.ttftMs 写 null。
	FirstByteMS *int64
	TTFTMS      *int64
}

// buildRoutingTrace 把事实编译成落库字节。返回 nil 表示**没有可记的事实**（调用方应保持列原值）。
func buildRoutingTrace(facts routingTraceFacts) []byte {
	trace, ok := newRoutingTrace(facts)
	if !ok {
		return nil
	}
	encoded, err := json.Marshal(trace)
	if err != nil {
		// 编码失败不该影响入账：trace 是观测面，缺一列不影响账务（调用方会把它当「无 trace」处理）。
		return nil
	}
	return encoded
}

func newRoutingTrace(facts routingTraceFacts) (routingTraceV1, bool) {
	if facts.StartedAt.IsZero() {
		return routingTraceV1{}, false
	}
	if len(facts.Attempts) == 0 && len(facts.Saturations) == 0 {
		// 没有任何真实转发留痕时不写：空壳 trace 只会让界面显示「已记录但什么都没有」。
		return routingTraceV1{}, false
	}
	finishedAt := facts.FinishedAt
	if finishedAt.IsZero() && facts.DurationMS > 0 {
		finishedAt = facts.StartedAt.Add(time.Duration(facts.DurationMS) * time.Millisecond)
	}
	if finishedAt.IsZero() {
		finishedAt = facts.StartedAt
	}
	mode := routingTraceModeSingleUpstream
	if isHedgeRun(facts) {
		mode = routingTraceModeLegacyHedge
	}

	events := make([]routingTraceEventV1, 0, 4+2*len(facts.Attempts))
	elapsed := func(at time.Time) int64 {
		return at.Sub(facts.StartedAt).Milliseconds()
	}
	events = append(events, routingTraceEventV1{
		Type:      routingTraceEventRequestStarted,
		At:        facts.StartedAt.UnixMilli(),
		ElapsedMs: 0,
	})
	events = append(events, facts.Saturations...)

	var (
		winnerIdx      = -1
		providerMs     int64
		fallbacks      int
		cancelFailures int
	)
	for index, attempt := range facts.Attempts {
		providerMs += attempt.DurationMS
		kind := routingTraceAttemptKindNormal
		if attempt.Attempt > 1 {
			kind = routingTraceAttemptKindFallback
			fallbacks++
		}
		if strings.Contains(attempt.Reason, "hedge_loser_cancelled") {
			cancelFailures++
		}
		// 没有实测起点的事件不产出：宁缺，不拿别的时刻顶替。
		if attempt.StartedAt.IsZero() {
			continue
		}
		provider := &routingTraceProviderV1{ID: attempt.ProviderID, Name: attempt.ProviderName}
		events = append(events, routingTraceEventV1{
			Type:        routingTraceEventAttemptStarted,
			At:          attempt.StartedAt.UnixMilli(),
			ElapsedMs:   elapsed(attempt.StartedAt),
			AttemptID:   strconv.Itoa(attempt.Attempt),
			AttemptKind: kind,
			Provider:    provider,
		})
		if attempt.FinishedAt.IsZero() {
			continue
		}
		events = append(events, routingTraceEventV1{
			Type:        routingTraceEventAttemptFinished,
			At:          attempt.FinishedAt.UnixMilli(),
			ElapsedMs:   elapsed(attempt.FinishedAt),
			AttemptID:   strconv.Itoa(attempt.Attempt),
			AttemptKind: kind,
			Provider:    provider,
			Outcome:     attemptOutcome(attempt),
			StatusCode:  attempt.StatusCode,
			Reason:      attempt.Reason,
			DurationMs:  attempt.FinishedAt.Sub(attempt.StartedAt).Milliseconds(),
		})
		if winnerIdx < 0 && isSuccessOutcome(attempt) {
			winnerIdx = index
		}
	}

	winnerOrigin := routingTraceWinnerOriginNone
	var winnerProviderID *int64
	var winnerRound *int
	if winnerIdx >= 0 {
		winner := facts.Attempts[winnerIdx]
		origin := routingTraceWinnerOriginNormal
		// 亲和命中（软提名）在 Go 侧由 route 层记 `affinity_hit`，落链后仍是同一次尝试；
		// trace 只如实标注「胜者来自正常尝试」，不声称 sticky。
		winnerOrigin = origin
		id := winner.ProviderID
		winnerProviderID = &id
		round := 1
		winnerRound = &round
		at := winner.FinishedAt
		if at.IsZero() {
			at = winner.StartedAt
		}
		events = append(events, routingTraceEventV1{
			Type:       routingTraceEventWinnerCommitted,
			At:         at.UnixMilli(),
			ElapsedMs:  elapsed(at),
			AttemptID:  strconv.Itoa(winner.Attempt),
			Provider:   &routingTraceProviderV1{ID: winner.ProviderID, Name: winner.ProviderName},
			Outcome:    routingTraceOutcomeSuccess,
			StatusCode: winner.StatusCode,
			DurationMs: winner.DurationMS,
		})
	}
	events = append(events, routingTraceEventV1{
		Type:       routingTraceEventRequestFinished,
		At:         finishedAt.UnixMilli(),
		ElapsedMs:  elapsed(finishedAt),
		Outcome:    requestOutcome(facts),
		StatusCode: facts.StatusCode,
		DurationMs: facts.DurationMS,
	})

	truncated := false
	if len(events) > routingTraceMaxEvents {
		events = events[:routingTraceMaxEvents]
		truncated = true
	}

	summary := &routingTraceSummaryV1{
		Outcome:            requestOutcome(facts),
		StatusCode:         facts.StatusCode,
		DurationMs:         facts.DurationMS,
		TTFTMs:             facts.TTFTMS,
		AttemptsPerRequest: len(facts.Attempts),
		Rounds:             1,
		ProviderMs:         providerMs,
		FallbackPromotions: fallbacks,
		CancelFailures:     cancelFailures,
		WinnerOrigin:       winnerOrigin,
		WinnerProviderID:   winnerProviderID,
		WinnerRound:        winnerRound,
	}
	if summary.TTFTMs == nil {
		summary.TTFTMs = facts.FirstByteMS
	}
	summary.MaxActiveAttempts = maxActiveAttempts(facts, mode)

	return routingTraceV1{
		Version:          1,
		Mode:             mode,
		StartedAt:        facts.StartedAt.UnixMilli(),
		UpdatedAt:        finishedAt.UnixMilli(),
		DiscoveryEnabled: false,
		Eligible:         false,
		Events:           events,
		Summary:          summary,
		Truncated:        truncated,
	}, true
}

// isHedgeRun 判定本次是否跑过竞速：以真实的竞速留痕为准（链上 hedge_* 词条或饱和事件）。
func isHedgeRun(facts routingTraceFacts) bool {
	if len(facts.Saturations) > 0 {
		return true
	}
	for _, attempt := range facts.Attempts {
		if strings.HasPrefix(attempt.Reason, routingTraceHedgeReasonPrefix) {
			return true
		}
	}
	return false
}

// maxActiveAttempts 给出并发上限的**下界**：
//   - 串行路径按构造恒为 1；
//   - 竞速路径：每次 `hedge_launched` 都发生在「已有 attempt 活跃且未达上限」时，
//     故峰值至少是 1+已有竞速启动数；饱和事件里的 `activeAttemptCount` 是当时的实测值。
//
// 这是下界而非测量值——Go 的竞速协调器没有记峰值，此处不凭空写一个更大的数。
func maxActiveAttempts(facts routingTraceFacts, mode string) int {
	if mode != routingTraceModeLegacyHedge {
		return 1
	}
	peak := 1
	launched := 0
	for _, attempt := range facts.Attempts {
		if strings.HasPrefix(attempt.Reason, "hedge_launched") {
			launched++
			if 1+launched > peak {
				peak = 1 + launched
			}
		}
	}
	for _, saturation := range facts.Saturations {
		if saturation.ActiveAttemptCount+1 > peak {
			peak = saturation.ActiveAttemptCount + 1
		}
	}
	return peak
}

func attemptOutcome(attempt forward.AttemptOutcome) string {
	if isSuccessOutcome(attempt) {
		return routingTraceOutcomeSuccess
	}
	if strings.Contains(attempt.Reason, routingTraceClientAbortReasonPart) {
		return routingTraceOutcomeClientAbort
	}
	return routingTraceOutcomeFailed
}

// isSuccessOutcome 以链词表为准（成功词表见 forward/chainreason.go：未知词会被公开状态
// 分类器判成失败，故这里也用同一组词，避免出现「trace 说成功、链说失败」）。
func isSuccessOutcome(attempt forward.AttemptOutcome) bool {
	switch attempt.Reason {
	case forward.ReasonRequestSuccess, forward.ReasonHedgeWinner, "retry_success":
		return true
	}
	return attempt.StatusCode >= 200 && attempt.StatusCode < 300 && attempt.Reason == ""
}

func requestOutcome(facts routingTraceFacts) string {
	for _, attempt := range facts.Attempts {
		if strings.Contains(attempt.Reason, routingTraceClientAbortReasonPart) {
			return routingTraceOutcomeClientAbort
		}
	}
	if facts.StatusCode >= 200 && facts.StatusCode < 400 {
		return routingTraceOutcomeSuccess
	}
	return routingTraceOutcomeFailed
}

// routingTraceRecorder 是**每请求**的 trace 事实收集器，实现 forward.HedgeTraceSink。
//
// 它只收集「链词表装不下」的事实（当前是竞速饱和）；其余事实在结算时从 attempts 直接读取。
type routingTraceRecorder struct {
	startedAt time.Time
	// mu 保护 saturations：饱和事件的调用者是**竞速的阈值计时器协程**（每个 attempt 一个
	// `time.AfterFunc`），多个候选可能同时被拒；而读取发生在终态结算的另一个协程里。
	mu          sync.Mutex
	saturations []routingTraceEventV1
}

func newRoutingTraceRecorder(startedAt time.Time) *routingTraceRecorder {
	return &routingTraceRecorder{startedAt: startedAt}
}

// HedgeSlotSaturated 实现 forward.HedgeTraceSink。
func (r *routingTraceRecorder) HedgeSlotSaturated(
	providerID int64,
	providerName string,
	active, configuredCap int,
	at time.Time,
) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.saturations = append(r.saturations, routingTraceEventV1{
		Type:               routingTraceEventHedgeSaturated,
		At:                 at.UnixMilli(),
		ElapsedMs:          at.Sub(r.startedAt).Milliseconds(),
		Provider:           &routingTraceProviderV1{ID: providerID, Name: providerName},
		ActiveAttemptCount: active,
		ConfiguredCap:      configuredCap,
	})
}

// snapshotSaturations 返回饱和事件的副本（读侧持锁取，避免结算协程与计时器协程竞争）。
func (r *routingTraceRecorder) snapshotSaturations() []routingTraceEventV1 {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.saturations) == 0 {
		return nil
	}
	out := make([]routingTraceEventV1, len(r.saturations))
	copy(out, r.saturations)
	return out
}

// routingTrace 把本请求的事实编译成落库字节（nil = 本次无可记事实，列保持原值）。
//
// 事实来源：forward 层的实测留痕（attempts 的 StartedAt/FinishedAt）与 trace 收集器
// （竞速饱和）。**不接受**调用方传"推算出来"的时刻：见 routingTraceFacts 的注释。
func (s *storeSettler) routingTrace(
	attempts []forward.AttemptOutcome,
	finishedAt time.Time,
	statusCode int,
	ttftMS *int,
	firstByteMS *int,
) []byte {
	if s == nil || s.state == nil {
		return nil
	}
	saturations := s.state.trace.snapshotSaturations() // nil 安全（trace 为 nil 时返回 nil）
	facts := routingTraceFacts{
		StartedAt:   s.state.StartedAt,
		FinishedAt:  finishedAt,
		Attempts:    attempts,
		Saturations: saturations,
		StatusCode:  statusCode,
		DurationMS:  durationMSOrZero(finishedAt, s.state.StartedAt),
		FirstByteMS: intPtrToInt64(firstByteMS),
		TTFTMS:      intPtrToInt64(ttftMS),
	}
	return buildRoutingTrace(facts)
}

func durationMSOrZero(finishedAt, startedAt time.Time) int64 {
	if finishedAt.IsZero() || startedAt.IsZero() {
		return 0
	}
	return finishedAt.Sub(startedAt).Milliseconds()
}

// intPtrToInt64 把终态层的 *int 毫秒读数转成 trace 的 *int64（nil 透传为 nil，
// 保持「无实测值」与「实测为 0」的区别）。
func intPtrToInt64(value *int) *int64 {
	if value == nil {
		return nil
	}
	converted := int64(*value)
	return &converted
}
