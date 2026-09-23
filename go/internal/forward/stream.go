package forward

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
	"github.com/fanxcv/claude-code-hub-go/go/internal/gate"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// 本文件实现流式转发路径，对应 Node 的 forwarder.ts 流式分支与 response-handler.ts 的流式处理。
//
// 与非流式路径的关键差异：
//   - 200 响应在「收到响应头」时无法判定真假：正文可能是错误 JSON，也可能是门控必须
//     拒绝的畸形流。所以成功判定被推迟到流终态（stream-finalization.ts 的延迟结算语义）。
//   - 提交前失败必须做到「客户端零字节」：门控在 precommit 期间缓冲前缀，失败即换供应商，
//     客户端不会看到半截流。
//   - 上游正文的生命周期归调用方：提交后正文由 Stream 按需拉取，本包不读完它。

// StreamSettler 是流式终态的结算接缝。
//
// 三条终态各调用一次，且整条流只调用一次（见 Stream.settleOnce）：
//   - TerminalCompleted / TerminalUpstreamTruncated：上游侧结束；
//   - TerminalUpstreamError / TerminalLocalError：上游或本地失败；
//   - TerminalClientAborted / TerminalIdleTimeout：中断类终态。
//
// 实体（落库、熔断、会话绑定、用量计费）属接线层；本包只保证调用纪律与事实完整。
type StreamSettler interface {
	SettleStream(ctx context.Context, outcome StreamOutcome) error
}

// StreamOutcome 是一条流式请求的终态事实。
type StreamOutcome struct {
	Kind       TerminalKind
	Provider   Provider
	Endpoint   Endpoint
	StatusCode int
	// Plan 是本次尝试的计划（含模型重定向事实）。计费要用重定向后的模型名，
	// 而重定向发生在计划编译期，故终态事实必须带上计划本身。
	// nil 表示未走到编译计划（无上游参与），此时按「无重定向」计费。
	Plan *Plan
	// Observation 是 O(1) 观测快照；截断时 Snapshot 不足以重建终态（Node 语义：跳过工件落盘）。
	Observation Observation
	// Attempts 是本次请求截至终态时刻的尝试留痕，供落 provider_chain。
	//
	// 为什么必须随终态一起给出：流式请求的 provider_chain 只能在流终态那一次写入（终态列
	// 只允许写一次），而竞速的输家留痕是在胜者之后才追加的，故它是这一刻的快照。
	Attempts    []AttemptOutcome
	ClientAbort bool
	Err         error
	At          time.Time
}

// StreamOptions 是流式路径的配置。
type StreamOptions struct {
	// Format 是客户端入站格式，决定观测阶段解析终止标记与用量字段的方言。
	Format convert.ClientFormat
	// Family 覆盖供应商类型到门控家族的映射；为空时按 provider.Type 推导。
	//
	// 映射不出来的供应商按 fail-open 处理（对应 Node 的 gateFamily 为 null）：不做门控，
	// 直接透传，绝不因为「我们认不出它」而拒绝一条正常响应。
	Family gate.Family
	// FamilyResolved 为真表示 Family 是调用方显式给的（含显式要求「不做门控」）。
	FamilyResolved bool
	// ForceGate 为真时即使上游未声明 SSE 也走门控（对应 Node 的 codex-responses 强制流式路径）。
	ForceGate bool
	// GateMode 是流式内容门控模式：enforce 才门控；off/shadow 均不门控（shadow 另挂旁路观察者）。
	//
	// 对齐 Node 的判据（forwarder.ts:2034、5334）：
	//
	//	gateMode === "enforce" || replayOwner || forceCodexResponsesStream
	//
	// 其中两文「强制门控」（replay owner 与 codex 强制流式）不在本包里判：前者由接线层
	// 在解析模式时直接给 enforce，后者由 ForceGate 承载（见 gateFamily）。空值等同 enforce
	// （本字段是后加的，旧调用方不设它就得到改造前的行为）。
	GateMode gate.Mode
	// SkipGate 为真时跳过门控（回放 owner 之外的 shadow/off 之外的自定义策略）。
	SkipGate bool
	// PrebufferEventCap / PrebufferByteCap 是门控前缀上限；0 取 stage 默认（见 Node 的 caps 解析）。
	PrebufferEventCap int
	PrebufferByteCap  int
	// Budget 是进程级共享前缀预算；nil 表示不限制（仅测试与单进程调试）。
	Budget *gate.Budget
	// CaptureCommitMarker 决定是否记录触发提交的帧信息（高并发模式下可关闭）。
	CaptureCommitMarker bool
	// IdleTimeoutFor 返回某供应商的流式静默超时（对应 provider.streamingIdleTimeoutMs，
	// 默认 0 = 不限制）。它同时作用于门控等待期与提交后的正文读取期。
	IdleTimeoutFor func(providerID int64) time.Duration
	// ProbeAfterFirstByteFor 返回某供应商的中途探测阈值T（秒）；<=0 或未装配即不探测。
	//
	// 为什么只传秒数而不传整个 slowrate.ProbeParams：门控侧只需要 T（它判的是时间），
	// 而 token 与系数两个门只属于写样本时的判定（在那里连同基线一起由 slowrate 包评估）。
	// 这样 forward 不引入对 slowrate 的依赖，接缝面最小。
	ProbeAfterFirstByteFor func(providerID int64) int
	// PrecommitRateFor 返回某供应商的分级速率闸阈值 θ（语义字节/秒）；<=0 或未装配即不启用。
	//
	// 启用后提交点后移（首个语义内容帧不再立即提交），详见 gate/progress.go 的三档判据。
	// 与 ProbeAfterFirstByteFor 同形：只传一个整数，forward 不引入对 slowrate/route 的依赖。
	PrecommitRateFor func(providerID int64) int
	// PrecommitShadow 为真时速率闸**只观测不裁决**：门控照旧在首个语义内容帧提交
	// （客户端时延与改造前完全一致），提交后仍采样并在 1s/3s/10s 落标定日志。
	//
	// 它**优先于**渠道开关 slow_rate_precommit_enabled：影子为真时，渠道开关开了也不裁决
	// （precommitRate 首行即返回 0）。出厂值为假，故常态下「是否裁决」只由渠道开关决定。
	PrecommitShadow bool
	// OnPostCommitSlow 在二级闸判定「提交后掉速」时回调一次（渠道级，供后续请求避开）。
	//
	// nil 表示不处置（只落日志）：标记动作的持久化由接线层决定，forward 不假设存哪。
	OnPostCommitSlow func(providerID int64, observedBytesPerSecond int)
	// rateMarks 覆盖提交后采样的标定点序列，**仅供包内单测**缩到毫秒级；
	// nil 时用出厂三档（1s/3s/10s）。生产路径不设。
	rateMarks []rateMark
	// StartedAt 是请求开始时刻，用于 TTFT；零值取 ForwardStream 的当前时刻。
	StartedAt time.Time
	// HeadBytes / TailBytes 是观测窗口容量；0 取默认（默认刻意小，见 observe.go）。
	HeadBytes int
	TailBytes int
	// PendingChunkDeadline 是下游不取走已缓冲数据块的时限；0 取默认 60s，负值关闭。
	PendingChunkDeadline time.Duration
	// Settle 是结算接缝；nil 时只做 pctx 的一次性断言。
	Settle StreamSettler
	// ChunkBytes 是单次上游读缓冲区大小；0 取默认。
	ChunkBytes int
	// Logger 用于泵与门控的诊断日志；nil 时写 stderr。
	Logger *logx.Logger
	// Now 可注入时钟；nil 用 time.Now。
	Now func() time.Time
}

func (o StreamOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o StreamOptions) idleTimeout(providerID int64) time.Duration {
	if o.IdleTimeoutFor == nil {
		return 0
	}
	return o.IdleTimeoutFor(providerID)
}

// probeAfterFirstByte 把探测阈值折成时长；未装配或渠道未配置（<=0）即 0（不探测）。
func (o StreamOptions) probeAfterFirstByte(providerID int64) time.Duration {
	if o.ProbeAfterFirstByteFor == nil {
		return 0
	}
	seconds := o.ProbeAfterFirstByteFor(providerID)
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// precommitRate 取某供应商的分级速率闸阈值（字节/秒）。影子期返回 0：门控不得改变提交时机。
func (o StreamOptions) precommitRate(providerID int64) int {
	if o.PrecommitShadow || o.PrecommitRateFor == nil {
		return 0
	}
	rate := o.PrecommitRateFor(providerID)
	if rate <= 0 {
		return 0
	}
	return rate
}

// rateSamplerEnabled 报告是否需要在提交后采样速率（二级闸与影子标定共用同一采样器）。
func (o StreamOptions) rateSamplerEnabled(providerID int64) bool {
	return o.precommitRate(providerID) > 0 || o.PrecommitShadow
}

// 门控默认上限，对齐 STREAM_GATE_PREBUFFER_* 的默认量级。
const (
	DefaultPrebufferEventCap = 500
	DefaultPrebufferByteCap  = 4 << 20
)

// streamAttempt 是一次已通过门控的流式尝试：前缀 + 仍活着的上游正文。
type streamAttempt struct {
	StatusCode int
	Status     string
	Header     http.Header

	// Prefix 是门控缓冲的前缀字节块（未启用门控或提交前无中性帧时为空）。
	Prefix [][]byte
	// Source 是上游正文；ReaderDone 为真表示门控期间已读到 EOF（Prefix 即全部正文）。
	Source     io.ReadCloser
	ReaderDone bool
	// Lease 是前缀的共享预算租约；前缀离开 pending slot 后必须释放。
	Lease *gate.Lease
	// Gated 为真表示本次响应经过了门控。
	Gated bool
	// UpstreamFirstByteAt 是上游首个非空 chunk 的到达时刻（门控的 OnFirstByte）。
	// 零值表示门控未见到字节；它只用于测算真 TTFB（`first_byte_ms`）。
	UpstreamFirstByteAt time.Time
	// FramesSeen 是门控期间分类过的帧数。
	FramesSeen int
	// Marker 是提交标记（CaptureCommitMarker 为真时非空）。
	Marker *gate.CommitMarker
	// GateWait 是门控等待耗时。
	GateWait time.Duration
	// Family 是本次门控使用的协议家族（未门控时为空）。
	Family gate.Family
}

// StreamResult 是一次流式转发的结果。
type StreamResult struct {
	// Result 是共用审计面：供应商、端点、计划、状态码、尝试留痕。
	Result
	// Stream 非空表示流已提交，调用方必须消费到底或显式取消。
	Stream *Stream
}

// ForwardStream 执行一次流式转发：与非流式共用供应商切换与重试决策，
// 差别在于成功判定推迟到流终态。
//
// 返回值语义：
//   - 已提交（Stream 非空）：调用方接管 Stream；结算由 Stream 的终态路径完成。
//   - 上游返回非流式 2xx（例如 JSON）：与 Forward 相同，正文在 Result.Body 里，已即时结算。
//   - 全部耗尽：Result 非 nil（含尝试留痕），error 可 errors.As 成 *Failure。
func ForwardStream(
	ctx context.Context,
	pc *pctx.Context,
	initial *Candidate,
	deps Deps,
	options StreamOptions,
) (*StreamResult, error) {
	if options.StartedAt.IsZero() {
		options.StartedAt = deps.now()
	}
	execute := func(
		attemptCtx context.Context,
		provider Provider,
		plan *Plan,
		outcome *AttemptOutcome,
	) (*attemptResponse, *Failure) {
		return deps.executeStreamAttempt(attemptCtx, pc, provider, plan, outcome, options)
	}
	var committed *streamAttempt
	onSuccess := func(result *Result, response *attemptResponse) {
		if response.Stream != nil {
			// 流式：结算推迟到流终态，这里不碰 pctx；同时把「本结果不得走非流式结算缝」
			// 写进结果本身，让外层 defer 能看见（它只看得到 Result）。
			committed = response.Stream
			result.DeferredToStream = true
			return
		}
		result.Body = response.Body
		settleNonStream(pc, result, response.StatusCode)
	}

	base, err := forwardLoop(ctx, pc, initial, deps, execute, onSuccess)
	if base == nil {
		// 尝试开始前就失败（无候选、计划构造失败）：没有上游参与，也就没有结果可言。
		// 此处必须显式返回，不能解引用 base——那会让「请求正文非法」这类客户端错误
		// 升级成进程级 panic。
		return nil, err
	}
	result := &StreamResult{Result: *base}
	if err != nil {
		return result, err
	}
	if committed == nil {
		// 非流式 2xx：正文已完整读回并即时结算。
		return result, nil
	}
	result.Stream = newStream(ctx, committed, base.Provider, base.Endpoint, base.Plan, pc, options, attemptsOf(base))
	return result, nil
}

// executeStreamAttempt 发起一次流式尝试。
//
// 顺序与 Node 一致：先拨号拿响应头，再按状态码分流：
//  1. 非 2xx：按非流式语义读完（有界的）错误正文并分类，交给尝试循环决定重试/切换。
//  2. 2xx 且形态是流（SSE / 强制流式）：跑门控。门控失败视为本次尝试失败——
//     此时客户端零字节，换供应商是安全的；门控提交则把前缀与上游正文一起交回。
//  3. 2xx 但不是流（例如 JSON）：按非流式语义处理，交由调用方决定如何面向客户端。
func (d Deps) executeStreamAttempt(
	ctx context.Context,
	pc *pctx.Context,
	provider Provider,
	plan *Plan,
	outcome *AttemptOutcome,
	options StreamOptions,
) (*attemptResponse, *Failure) {
	// 上游 WS：资格全真时先试 WS。成功时返回的响应与 HTTP 同形（Body 是 SSE 字节流），
	// 下游（门控/整流/结算/留痕）完全不知道字节来自 WS；失败则继续走 HTTP，
	// **不记失败、不计熔断**（见 wsAttempt 的回落语义）。
	response := d.wsAttempt(ctx, pc, provider, plan, outcome)
	cancel := context.CancelFunc(func() {})
	if response == nil {
		var failure *Failure
		response, cancel, failure = d.dialAttempt(ctx, plan, outcome, true)
		if failure != nil {
			return nil, failure
		}
	}
	// 流式尝试没有总超时，cancel 只在传输层需要时才有内容；无论如何都要放行。
	defer cancel()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer func() { _ = response.Body.Close() }()
		return d.completeResponse(response, plan, outcome)
	}

	family, gated := options.gateFamily(provider)
	if !gated || !options.shouldGate(response.Header, plan) {
		defer func() { _ = response.Body.Close() }()
		// 不做门控：要么这不是流（按非流式读回），要么我们认不出协议家族（fail-open 透传）。
		if !options.isStreamShaped(response.Header, plan) && !options.ForceGate {
			return d.completeResponse(response, plan, outcome)
		}
		return &attemptResponse{
			StatusCode: response.StatusCode,
			Status:     response.Status,
			Header:     response.Header,
			Stream: &streamAttempt{
				StatusCode: response.StatusCode,
				Status:     response.Status,
				Header:     response.Header,
				Source:     options.shadowSource(response.Body, family, outcome),
				Gated:      false,
				Family:     family,
			},
		}, nil
	}

	return d.gateStreamAttempt(ctx, response, plan, outcome, options, family)
}

// gateStreamAttempt 在向客户端提交前跑首个有效内容门控。
func (d Deps) gateStreamAttempt(
	ctx context.Context,
	response *dial.Response,
	plan *Plan,
	outcome *AttemptOutcome,
	options StreamOptions,
	family gate.Family,
) (*attemptResponse, *Failure) {
	startedAt := options.now()
	// 门控的 OnFirstByte 在**上游**首个非空 chunk 到达时回调（语义与 Node 的
	// gateFirstByteAt/attempt.firstByteAt 同源）；前缀是提交后才交给我们，
	// 那时打戳会把 first_byte_ms 记成提交时刻。
	var upstreamFirstByteAt time.Time
	result, err := gate.Run(ctx, response.Body, options.gateOptions(family, outcome, func() {
		upstreamFirstByteAt = options.now()
	}))
	if err != nil {
		// 门控失败时上游正文所有权仍在我们手里：关闭它，客户端一个字节都还没收到。
		_ = response.Body.Close()
		failure := d.gateFailure(err, plan, outcome)
		// 主动判慢：标出这次尝试的真实死因、种类与「自首字节已等了多久」，
		// 供终态层写中途慢样本（口径见 slowrate.Facts.MidStream）。
		if failure != nil {
			if kind, slow := probeSlowKind(err); slow {
				failure.ProbeSlow = true
				failure.ProbeSlowKind = kind
				failure.ProbeElapsedMS = int(options.now().Sub(upstreamFirstByteAt).Milliseconds())
				failure.ProbeSlowBytesPerSecond = slowRateBytesPerSecond(asPrecommit(err))
			}
		}
		return nil, failure
	}

	// 门控提交：前缀与上游正文都交给调用方，租约随之一并转移。
	return &attemptResponse{
		StatusCode: response.StatusCode,
		Status:     response.Status,
		Header:     response.Header,
		Stream: &streamAttempt{
			StatusCode:          response.StatusCode,
			Status:              response.Status,
			Header:              response.Header,
			Prefix:              result.Prefix,
			Source:              response.Body,
			ReaderDone:          result.ReaderDone,
			Lease:               result.Lease,
			Gated:               true,
			UpstreamFirstByteAt: upstreamFirstByteAt,
			FramesSeen:          result.FramesSeen,
			Marker:              result.Marker,
			GateWait:            options.now().Sub(startedAt),
			Family:              family,
		},
	}, nil
}

// gateOptions 组装门控参数；串行与竞速两条路径共用，避免 11 个字段两处各写一遍。
//
// onFirstByte 由调用方提供：两条路径都要在闭包里捕获各自的变量。
func (o StreamOptions) gateOptions(
	family gate.Family,
	outcome *AttemptOutcome,
	onFirstByte func(),
) gate.Options {
	eventCap := o.PrebufferEventCap
	if eventCap <= 0 {
		eventCap = DefaultPrebufferEventCap
	}
	byteCap := o.PrebufferByteCap
	if byteCap <= 0 {
		byteCap = DefaultPrebufferByteCap
	}
	return gate.Options{
		Family:              family,
		ProviderID:          int(outcome.ProviderID),
		ProviderName:        outcome.ProviderName,
		PrebufferEventCap:   eventCap,
		PrebufferByteCap:    byteCap,
		IdleTimeout:         o.idleTimeout(outcome.ProviderID),
		ProbeAfterFirstByte: o.probeAfterFirstByte(outcome.ProviderID),
		PrecommitRate:       o.precommitRate(outcome.ProviderID),
		CaptureCommitMarker: o.CaptureCommitMarker,
		OnFirstByte:         onFirstByte,
		Budget:              o.Budget,
	}
}

// gateFailure 把门控失败映射成尝试失败。
//
// 状态码口径与 Node 一致：能从错误帧文本推断出 4xx 就用它，否则 502（A Bad Gateway）。
// 静默超时走 524（供应商故障），与提交后的静默超时同归因。
func (d Deps) gateFailure(err error, plan *Plan, outcome *AttemptOutcome) *Failure {
	var precommit *gate.PrecommitError
	if !errors.As(err, &precommit) {
		// 上游读错误（含 ctx 取消）：按传输层归因，保持与非流式一致。
		return d.transportFailure(err, plan, outcome, context.Background())
	}

	body := gate.GateErrorBody(precommit)
	status := gateStatusForPrecommit(precommit)
	failure := &Failure{
		Category:      classifyForStatus(status, false, d.Rules, body),
		StatusCode:    status,
		Message:       precommit.Error(),
		Body:          truncateText(body, int(d.Limits.withDefaults().MaxErrorBodyBytes)),
		RequestScoped: gate.IsRequestScopedGateFailure(err),
		ProviderID:    outcome.ProviderID,
		ProviderName:  outcome.ProviderName,
		EndpointID:    outcome.EndpointID,
		EndpointURL:   plan.URL,
		Attempt:       outcome.Attempt,
		Err:           err,
	}
	if precommit.Reason == gate.FailIdleTimeout {
		failure.Category = CategoryProviderError
		failure.StatusCode = statusUpstreamTimeout
	}
	// 主动判慢（停滞与速率两来源）单独一档：不在同一家重试、立即换家、不计熔断。
	// 其余门控失败（含 FailIdleTimeout）仍是 CategoryProviderError，行为逐字不变。
	if precommit.Reason == gate.FailSlowProbe || precommit.Reason == gate.FailSlowRate {
		failure.Category = CategorySlowRate
		failure.StatusCode = statusUpstreamTimeout
	}
	return failure
}

// probeSlowKind 判断门控失败是否来自「主动判慢」，并给出可区分的种类。
//
// 两种来源：stall（首字后 T 秒零内容）与 rate（有内容但速率不达标）。两者都置
// Failure.ProbeSlow 供终态层做「判慢即标慢」的闭环；种类用于区分归因与标定。
func probeSlowKind(err error) (string, bool) {
	var precommit *gate.PrecommitError
	if !errors.As(err, &precommit) {
		return "", false
	}
	switch precommit.Reason {
	case gate.FailSlowProbe:
		return ProbeSlowKindStall, true
	case gate.FailSlowRate:
		return ProbeSlowKindRate, true
	default:
		return "", false
	}
}

// slowRateBytesPerSecond 把速率闸判慢时记下的实测字节数与时长折算成速率。
// 时长非正、字节非正、或不是速率闸判慢（nil）时返回 0（不可折算）。
func slowRateBytesPerSecond(precommit *gate.PrecommitError) int {
	if precommit == nil || precommit.SlowElapsedMS <= 0 || precommit.SlowPayloadBytes <= 0 {
		return 0
	}
	return precommit.SlowPayloadBytes * 1000 / precommit.SlowElapsedMS
}

// asPrecommit 取出 *gate.PrecommitError（不是该类型时返回 nil）。
func asPrecommit(err error) *gate.PrecommitError {
	var precommit *gate.PrecommitError
	if !errors.As(err, &precommit) {
		return nil
	}
	return precommit
}

// gateStatusForPrecommit 复刻 Node 的状态码推断：错误帧文本里能识别出的 4xx 才用，否则 502。
func gateStatusForPrecommit(precommit *gate.PrecommitError) int {
	if precommit.Reason == gate.FailIdleTimeout || precommit.Reason == gate.FailSlowRate {
		return statusUpstreamTimeout
	}
	if precommit.Reason == gate.FailGateError && precommit.FrameData != "" {
		if status, ok := inferStatusFromFrameData(precommit.FrameData); ok && status >= 400 && status < 500 {
			return status
		}
	}
	return http.StatusBadGateway
}

// inferStatusFromFrameData 从上游错误帧文本里认出常见状态码写法。
//
// 这是错误规则层 inferUpstreamErrorStatusCodeFromText 的极简对应物：只认显式的状态码字段，
// 不做启发式猜测（猜错会把「上游 500」记成「客户端 4xx」，那是归因漂移）。
func inferStatusFromFrameData(frameData string) (int, bool) {
	for _, key := range []string{"\"status_code\":", "\"statusCode\":", "\"code\":"} {
		index := strings.Index(frameData, key)
		if index < 0 {
			continue
		}
		rest := strings.TrimSpace(frameData[index+len(key):])
		status := 0
		digits := 0
		for _, char := range rest {
			if char < '0' || char > '9' {
				break
			}
			status = status*10 + int(char-'0')
			digits++
		}
		if digits > 0 && status >= 400 && status < 600 {
			return status, true
		}
	}
	return 0, false
}

// gateFamily 决定本次尝试是否以及按哪个家族跑门控。
//
// 模式语义（Node forwarder.ts:2034/5334）：只有 enforce 或强制门控的请求才跑门控；
// off/shadow 不门控，但**仍返回家族**——shadow 需要它做旁路分类（见 shadowSource）。
func (o StreamOptions) gateFamily(provider Provider) (gate.Family, bool) {
	if o.SkipGate {
		return "", false
	}
	family, ok := o.resolveGateFamily(provider)
	if !ok {
		return "", false
	}
	// ForceGate（codex 强制流式）是 Node 强制门控的两支之一，模式不得把它关掉。
	if !o.ForceGate && (o.GateMode == gate.ModeOff || o.GateMode == gate.ModeShadow) {
		return family, false
	}
	return family, true
}

// resolveGateFamily 只解家族归属，不管模式。
func (o StreamOptions) resolveGateFamily(provider Provider) (gate.Family, bool) {
	if o.FamilyResolved {
		if o.Family == "" {
			return "", false
		}
		return o.Family, true
	}
	return gate.MapProviderTypeToFamily(string(provider.Type))
}

// shadowSource 在 shadow 模式下把上游字节抄一份给旁路观察者。
//
// Node 的同一动作在 response-handler（response-handler.ts:3922、5372）：观察者由响应
// 处理阶段逐帧喂入，日志事件为 `StreamGate[shadow]: first decisive frame observed`。
// 本函数只包装 reader——不改字节、不缓冲、不阻断，家族未知时直接交回原文。
func (o StreamOptions) shadowSource(source io.ReadCloser, family gate.Family, outcome *AttemptOutcome) io.ReadCloser {
	if o.GateMode != gate.ModeShadow || family == "" || source == nil {
		return source
	}
	config := gate.ShadowConfig{
		Family:       family,
		ProviderID:   int(outcome.ProviderID),
		ProviderName: outcome.ProviderName,
	}
	if o.Logger != nil {
		config.OnReport = func(report gate.ShadowReport) {
			o.Logger.Info("StreamGate[shadow]: first decisive frame observed", map[string]any{
				"providerId":        report.ProviderID,
				"providerName":      report.ProviderName,
				"family":            string(report.Family),
				"decisiveVerdict":   string(report.DecisiveVerdict),
				"divergent":         report.Divergent,
				"firstContentLagMs": report.FirstContentLag.Milliseconds(),
				"verdictCounts":     report.VerdictCounts,
				"incomplete":        report.Incomplete,
			})
		}
	}
	return gate.NewShadowReader(source, config)
}

// shouldGate 判定是否真的对本次响应跑门控。
//
// ForceGate 覆盖 Content-Type：Node 只用 MIME 无法区分「无 SSE 头的 JSON 假 200」，
// 所以 codex-responses 兼容路径一律走门控。
func (o StreamOptions) shouldGate(header http.Header, plan *Plan) bool {
	if o.ForceGate {
		return true
	}
	return isEventStream(header) || plan.ClientStream
}

// isStreamShaped 判定响应看起来是不是流（用于决定是否按非流式读回正文）。
func (o StreamOptions) isStreamShaped(header http.Header, plan *Plan) bool {
	return isEventStream(header) || o.ForceGate || plan.ClientStream
}

// isEventStream 判定 Content-Type 是否为 SSE。
func isEventStream(header http.Header) bool {
	contentType := strings.ToLower(header.Get("Content-Type"))
	return strings.Contains(contentType, "text/event-stream") ||
		strings.Contains(contentType, "application/x-ndjson")
}

// Stream 是已提交的流：先吐门控前缀，再按需拉取上游剩余字节。
type Stream struct {
	attempt  *streamAttempt
	provider Provider
	endpoint Endpoint
	plan     *Plan
	pc       *pctx.Context
	options  StreamOptions

	observer *Observer
	pump     *Pump
	// rateSampler 是提交后的速率采样器（影子标定与二级闸）；未启用时为 nil。
	rateSampler *rateSampler

	mu          sync.Mutex
	prefixIndex int
	prefixDone  bool
	closed      bool
	lease       *gate.Lease

	idleTimer *time.Timer

	settleOnce sync.Once
	outcome    StreamOutcome
	settled    bool
	completion chan struct{}
	// attempts 取「截至当前」的尝试留痕（竞速的输家是在胜者之后才追加的，故取快照而非
	// 构造时的切片）；nil 表示本层拿不到留痕，终态就不带 provider_chain。
	attempts func() []AttemptOutcome
}

// newStream 构造流并启动终态结算协程。
//
// attempts 可以为 nil（测试或没有留痕来源的装配）。
func newStream(
	ctx context.Context,
	attempt *streamAttempt,
	provider Provider,
	endpoint Endpoint,
	plan *Plan,
	pc *pctx.Context,
	options StreamOptions,
	attempts func() []AttemptOutcome,
) *Stream {
	stream := &Stream{
		attempt:    attempt,
		provider:   provider,
		endpoint:   endpoint,
		plan:       plan,
		pc:         pc,
		options:    options,
		lease:      attempt.Lease,
		completion: make(chan struct{}),
		attempts:   attempts,
	}
	stream.observer = NewObserver(ObservationOptions{
		StartedAt:           options.StartedAt,
		UpstreamFirstByteAt: attempt.UpstreamFirstByteAt,
		Format:              observeFormatFor(plan, options.Format),
		HeadBytes:           options.HeadBytes,
		TailBytes:           options.TailBytes,
		Now:                 options.Now,
	})

	// 提交后速率采样器（影子标定与二级闸共用）。未启用或家族未知时不构造：
	// 无分类器即无法计量语义 payload，硬上只会得到一堆零值样本。
	if options.rateSamplerEnabled(provider.ID) {
		stream.rateSampler = newRateSampler(RateSamplerConfig{
			Family:       attempt.Family,
			ProviderID:   provider.ID,
			ProviderName: provider.Name,
			Rate:         options.precommitRate(provider.ID),
			Shadow:       options.PrecommitShadow,
			marks:        options.rateMarks,
			OnDegraded: func(sample RateSample) {
				if options.OnPostCommitSlow != nil {
					options.OnPostCommitSlow(sample.ProviderID, sample.BytesPerSecond)
				}
			},
			RequestID: pc.MessageRequestID,
			Model:     func() string { return stream.observer.Snapshot().Model },
			Logger:    options.Logger,
			Now:       options.Now,
		})
	}

	// 门控前缀在提交前就已经到达：把它按到达顺序先喂给观测器，否则用量与终止标记
	// 会随「第一次内容帧是否与其余帧同处一个读块」而丢失（前缀不再经过泵）。
	// 速率采样器同理需要先吃前缀：首个语义内容帧就在前缀里，漏喂会让时钟起点偏晚。
	for _, chunk := range attempt.Prefix {
		stream.observer.Push(chunk)
		if stream.rateSampler != nil {
			stream.rateSampler.Observe(chunk)
		}
	}

	source := attempt.Source
	if attempt.ReaderDone {
		// 门控期间已读到 EOF：前缀就是全部正文，泵的源立即结束，
		// 但仍要把上游关闭的责任交回泵，否则连接不会被释放。
		source = drainedSource{closer: attempt.Source}
	}
	stream.pump = NewPump(PumpOptions{
		Source:               source,
		OnReadStart:          func() { stream.onReadStart() },
		OnChunk:              func(chunk []byte) { stream.observeChunk(chunk) },
		OnClientCancel:       func(reason error) { stream.onClientCancel(reason) },
		ChunkBytes:           options.ChunkBytes,
		PendingChunkDeadline: options.PendingChunkDeadline,
		Logger:               options.Logger,
	})

	go stream.awaitTerminal(ctx)
	return stream
}

// drainedSource 表示「上游已在门控期间读尽」：只按约定关闭上游，不再产出正文。
type drainedSource struct {
	closer io.Closer
}

func (d drainedSource) Read([]byte) (int, error) { return 0, io.EOF }

func (d drainedSource) Close() error {
	if d.closer == nil {
		return nil
	}
	return d.closer.Close()
}

// Read 交付客户端可见的字节：先前缀，再上游。
func (s *Stream) Read(b []byte) (int, error) {
	if n, ok := s.readPrefix(b); ok {
		return n, nil
	}
	return s.pump.Read(b)
}

// readPrefix 交付门控前缀；ok 为假表示前缀已耗尽，应转为读上游。
func (s *Stream) readPrefix(b []byte) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.prefixDone {
		return 0, false
	}
	n := 0
	for n < len(b) && s.prefixIndex < len(s.attempt.Prefix) {
		chunk := s.attempt.Prefix[s.prefixIndex]
		copied := copy(b[n:], chunk)
		if copied < len(chunk) {
			// 调用方缓冲小于该前缀块：把已交付部分切掉，下次继续。
			s.attempt.Prefix[s.prefixIndex] = chunk[copied:]
			n += copied
			return n, true
		}
		s.attempt.Prefix[s.prefixIndex] = nil
		s.prefixIndex++
		n += copied
	}
	if s.prefixIndex >= len(s.attempt.Prefix) {
		s.prefixDone = true
		s.releaseLeaseLocked()
	}
	return n, n > 0
}

// releaseLeaseLocked 释放前缀预算租约；调用方必须持有 s.mu。
func (s *Stream) releaseLeaseLocked() {
	if s.lease == nil {
		return
	}
	s.lease.Release()
	s.lease = nil
}

// observeChunk 把 chunk 喂给观测器；引流期间一旦拿到终态事实就结束引流。
//
// 与 Node 的「计量完成即结束引流」同义：终态帧意味着上游不会再给新账单，
// 继续挂在流上只是白占引流配额。
func (s *Stream) observeChunk(chunk []byte) {
	if s.rateSampler != nil {
		s.rateSampler.Observe(chunk)
	}
	s.observer.Push(chunk)
	if s.pump == nil || s.pump.State() != PumpDraining {
		return
	}
	snapshot := s.observer.Snapshot()
	if snapshot.CompletionMarker || snapshot.ErrorText != "" {
		s.pump.FinishDrain(errStreamDrainComplete)
	}
}

// Observation 返回当前观测快照。
func (s *Stream) Observation() Observation { return s.observer.Snapshot() }

// Completion 阻塞到流终态并返回终态事实（含结算是否已完成）。
func (s *Stream) Completion() StreamOutcome {
	<-s.completion
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.outcome
}

// ClientCancel 处理客户端断开：立刻取消上游，不给断线风暴留驻留。
func (s *Stream) ClientCancel(reason error) {
	if reason == nil {
		reason = errStreamClientClosed
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	s.pump.ClientCancel(reason)
	s.pump.CancelSource(reason)
}

// Close 结束本次流：调用方不再消费。幂等。
//
// 已到终态时只是释放资源；未到终态时按「客户端中断」处理——调用方放弃消费与客户端
// 断开在资源语义上等价，都必须让上游连接回到池里。
func (s *Stream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.mu.Unlock()

	if s.pump.State() != PumpClosed {
		s.ClientCancel(errStreamClientClosed)
	}
	return nil
}

// onReadStart 武装静默计时器：只有真正向上游发起读之后的等待才算上游静默。
func (s *Stream) onReadStart() {
	timeout := s.options.idleTimeout(s.provider.ID)
	if timeout <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idleTimer != nil {
		s.idleTimer.Stop()
	}
	s.idleTimer = time.AfterFunc(timeout, func() {
		err := fmt.Errorf("%w: %s", errStreamIdleTimeout, timeout)
		// 静默超时同时定终态（不再等上游）：归因是供应商侧，走 524 一档。
		s.pump.CancelSource(err)
	})
}

// attemptsOf 把串行路径的结果留痕包成快照函数。
//
// 串行路径一旦提交流就结束了尝试循环，列表在提交之后不再变化；包成函数只为与竞速路径
// 共用同一个构造签名（竞速的列表在提交之后仍会被输家追加）。
func attemptsOf(result *Result) func() []AttemptOutcome {
	if result == nil {
		return nil
	}
	return func() []AttemptOutcome { return result.Attempts }
}

// onClientCancel 在泵记录客户端中断时调用一次。
func (s *Stream) onClientCancel(_ error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
}

// awaitTerminal 等待泵终态，落定终态事实并做一次结算。
func (s *Stream) awaitTerminal(ctx context.Context) {
	completion := s.pump.Completion()
	<-s.pump.Teardown()

	s.mu.Lock()
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
	s.releaseLeaseLocked()
	s.mu.Unlock()

	observation := s.observer.Snapshot()
	observation.Kind = terminalKindFor(completion, observation)
	observation.Err = completion.Err

	// 速率采样器收尾：撤销未到期的标定定时器（否则拖着一个已死的流到末档），
	// 并落一条流结束终值日志（供标定 bytesPerToken）。放在终态类型算出来之后，
	// 故日志里能带上真正的结束原因；它在泵终态之后、与提交时机无关。
	s.rateSampler.Close(string(observation.Kind))

	s.settleTerminal(ctx, StreamOutcome{
		Kind:        observation.Kind,
		Provider:    s.provider,
		Endpoint:    s.endpoint,
		StatusCode:  s.attempt.StatusCode,
		Plan:        s.plan,
		Observation: observation,
		Attempts:    s.attemptsSnapshot(),
		ClientAbort: completion.ClientAborted,
		Err:         completion.Err,
		At:          s.options.now(),
	})
	close(s.completion)
}

// attemptsSnapshot 取此刻的尝试留痕（无来源时返回 nil）。
func (s *Stream) attemptsSnapshot() []AttemptOutcome {
	if s.attempts == nil {
		return nil
	}
	return s.attempts()
}

// settleTerminal 执行唯一一次终态结算。
//
// 三条终态路径（正常结束 / 上游失败 / 中断）都汇集到 awaitTerminal，因此「只结算一次」
// 由 settleOnce 与 completion 通道共同保证：泵的 settle 也是幂等的，多个触发源只有一个赢家。
func (s *Stream) settleTerminal(ctx context.Context, outcome StreamOutcome) {
	s.settleOnce.Do(func() {
		s.mu.Lock()
		s.outcome = outcome
		s.settled = true
		settle := s.options.Settle
		pc := s.pc
		s.mu.Unlock()

		if pc != nil {
			pc.MarkSettled(pctx.Settlement{
				StatusCode: outcome.StatusCode,
				Success:    outcome.Kind == TerminalCompleted,
				At:         outcome.At,
			})
		}
		if settle != nil {
			// 结算失败不改变已定终态：数据库侧有自己的幂等谓词，重试属接线层职责。
			_ = settle.SettleStream(ctx, outcome)
		}
	})
}

// terminalKindFor 把泵终态与观测事实合并成终态类型。
func terminalKindFor(completion PumpCompletion, observation Observation) TerminalKind {
	if completion.ClientAborted {
		return TerminalClientAborted
	}
	if errors.Is(completion.Err, errStreamIdleTimeout) {
		return TerminalIdleTimeout
	}
	if completion.Err != nil {
		if errors.Is(completion.Err, io.EOF) {
			// 本分支当前**不可达**：泵在读源返回 io.EOF 时调 settle(true, nil, nil)，
			// 故 completion.Err 永远不会是 io.EOF（见 pump.go 的 readSourceOnce 与全部
			// settle 调用点，Err 只可能是读错误或取消原因）。
			//
			// 保留它并在此处补齐标记判断，是为了让「TerminalUpstreamTruncated 蕴含
			// !CompletionMarker」这条不变式**不依赖分支是否可达**：若将来真有路径传入
			// io.EOF，「已见终止标记」仍归 TerminalCompleted，不会把正文已交付的健康流
			// 误判成截断（那会给健康渠道写冷却）。该不变式是 dataplane 侧判「正文是否
			// 交付」的唯一依据（见 streamErrorMessage 与 affinity 的墓碑判定）。
			if observation.CompletionMarker {
				return TerminalCompleted
			}
			return TerminalUpstreamTruncated
		}
		return TerminalLocalError
	}
	if observation.ErrorText != "" {
		return TerminalUpstreamError
	}
	if observation.SawIncomplete {
		return TerminalIncomplete
	}
	if observation.CompletionMarker {
		return TerminalCompleted
	}
	return TerminalUpstreamTruncated
}

var (
	// errStreamClientClosed 是调用方放弃消费时的归因。
	errStreamClientClosed = errors.New("forward: 调用方已停止消费响应流")
	// errStreamIdleTimeout 是上游静默超时的归因（对应 Node 的 streaming_idle_timeout）。
	errStreamIdleTimeout = errors.New("forward: 上游流式响应静默超时")
	// errStreamDrainComplete 表示引流已拿到终态事实，提前结束。
	errStreamDrainComplete = errors.New("forward: 断线计量引流已完成")
)
