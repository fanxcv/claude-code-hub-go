package forward

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
	"github.com/fanxcv/claude-code-hub-go/go/internal/gate"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// 本文件实现流式竞速（hedge），对齐 Node forwarder.ts 的 sendStreamingWithHedge。
//
// 竞速语义要点：
//   - 首个供应商立即拨号，并带「首字节阈值」计时器（provider.firstByteTimeoutMs）。
//     阈值到期且并发未满时，从候选里启动下一个供应商。
//   - 胜者 = 首条「有效内容」到达的 attempt（门控提交的首个有效内容帧，
//     或未门控路径的首个可读块）。胜者立即提交给客户端，其余 attempt 变为输家。
//   - 输家二选一：开启输家计费时后台 drain 输家正文拿回用量并累加成本；
//     否则直接取消连接，绝不为一条输掉的流占着本地资源。
//   - 「同一请求只结算一次」不变量仍成立：只有胜者走 Stream 终态结算；
//     输家只经 LoserBiller 接缝把成本累加回既有 message_request 行
//     （接线层用 store.AddHedgeLoserCost 的幂等谓词保证不重复）。

// HedgeOptions 是竞速模式的配置。
type HedgeOptions struct {
	// MaxInFlight 是同时活跃的竞速 attempt 上限；0 取 DefaultHedgeMaxInFlight。
	// 初始 attempt 不受此限；达到上限的候选启动被拒并留痕 hedge_slot_saturated。
	MaxInFlight int
	// BillLosers 为真时输家正文被后台 drain 以拿回用量并计费；为假直接取消。
	BillLosers bool
	// LoserBiller 是输家计费接缝；nil 时即使 BillLosers 也不计费。
	LoserBiller HedgeLoserBiller
	// LoserDrainTimeout 是单个输家 drain 的绝对上限；0 取默认（对齐 120s）。
	LoserDrainTimeout time.Duration
	// LoserMaxDrainBytes 是单个输家 drain 的字节上限；0 取默认 64 MiB。
	LoserMaxDrainBytes int64
	// After 是阈值计时器工厂（测试注入用）；nil 用 time.AfterFunc。
	After func(d time.Duration, fn func()) hedgeTimer
	// Now 是时钟注入；nil 用 deps 的时钟。
	Now func() time.Time
	// WinnerDetached 是观测接缝：胜者交接后携带该次尝试的传输上下文（生产不接，
	// 供测试断言「胜者 ctx 随流终态取消、而非随 attempt 返回取消」）。
	WinnerDetached func(providerID int64, attemptCtx context.Context)
	// TraceSink 是竞速过程的观测接缝（可选）。
	//
	// 为何需要：饱和这类**信息性事实**没有 provider_chain 词条——`hedge_slot_saturated`
	// 写进链会被公开状态分类器判成失败（未知 reason 的兜底分支），污染可用率。
	// Node 把它们记进 `routing_trace`；本接缝就是那条通路。nil 时只留 debug 日志。
	TraceSink HedgeTraceSink
}

// HedgeTraceSink 接收竞速过程中**不入 provider_chain** 的事实。
//
// 生产实现是数据面的 routing_trace 构造器（见 dataplane/routing_trace.go）；
// 接口定义放在本包，以免 forward 反向依赖数据面。
type HedgeTraceSink interface {
	// HedgeSlotSaturated 记录一次「并发已满、候选未启动」。
	// active 与 configuredCap 是**当时实测**的并发数与配置上限。
	HedgeSlotSaturated(providerID int64, providerName string, active, configuredCap int, at time.Time)
}

// Hedge 相关默认值。
const (
	// DefaultHedgeMaxInFlight 是竞速并发的出厂上限。
	DefaultHedgeMaxInFlight = 2
	// DefaultHedgeLoserDrainTimeout 对齐 HEDGE_LOSER_DRAIN_TIMEOUT_MS 默认值。
	DefaultHedgeLoserDrainTimeout = 120 * time.Second
	// DefaultHedgeLoserMaxDrainBytes 是输家 drain 字节上限（与 MaxResponseBytes 同档）。
	DefaultHedgeLoserMaxDrainBytes int64 = 64 * 1024 * 1024
)

// hedgeTimer 是可停的阈值计时器（*time.Timer 实现之；测试注入手动触发版本）。
type hedgeTimer interface {
	Stop() bool
}

func hedgeTimerAfter(d time.Duration, fn func()) hedgeTimer {
	return time.AfterFunc(d, fn)
}

// HedgeLoserBiller 是竞速输家计费的接缝。
//
// 实现方（接线层）拿到输家引流后的用量证据，用与胜者相同的计价口径计算成本，
// 调用 store.AddHedgeLoserCost 累加到 message_request 行——该语句的
// `NOT (hedge_losers @> [{providerId, attemptNumber}])` 谓词保证同一输家只计一次。
type HedgeLoserBiller interface {
	BillLoser(ctx context.Context, bill HedgeLoserBill) error
}

// HedgeLoserBill 是一条竞速输家的计费证据。
type HedgeLoserBill struct {
	RequestID  int64
	ProviderID int64
	// ProviderName / Sequence 用于 HedgeLoserEntry 的展示与去重键。
	ProviderName string
	Sequence     int
	// UpstreamStatusCode 是输家的上游状态码。
	UpstreamStatusCode int
	// Usage 是输家流内可解析到的用量（枢纽口径）。
	Usage convert.Usage
	// Model 是输家流内最后一次声明的模型名。
	Model string
	// DrainComplete 为真表示输家正文读到自然结束或见到终态标记。
	DrainComplete bool
	// At 是 drain 完成时刻。
	At time.Time
}

// hedgeVerdict 是协调器对某个 attempt 的终态裁决。
type hedgeVerdict int

const (
	verdictWinner hedgeVerdict = iota
	verdictLoserCancel
	verdictLoserBill
)

// hedgeAttempt 是一次竞速 attempt 的协调状态。
type hedgeAttempt struct {
	seq      int
	provider Provider
	endpoint Endpoint
	plan     *Plan
	verdict  chan hedgeVerdict
	// ctx 是本次 attempt 的传输上下文（胜者交接后供观测断言其生命周期）。
	ctx context.Context
	// cancel 取消本次 attempt 的传输上下文；非计费输家用它打断拨号/门控。
	cancel context.CancelFunc
	// timer 是首字节阈值计时器。
	timer hedgeTimer
	// startedAt 是本 attempt 的**真实启动时刻**（runAttempt 入口），供 routing_trace 用。
	// 不用 race 起点顶替：后启动的备选会被记成「与首发同时开始」，那是编造时间线。
	startedAt time.Time
	// dispatched 为真表示已进入传输调用（阈值从这里起算）。
	dispatched bool
	// ws 是本次 attempt 的上游 WS 事实（nil 表示与上游 WS 无关）。
	//
	// 为什么要存到 attempt 上而不是只留在 runAttempt 的局部变量里：竞速的**胜者/失败/输家**
	// 三类留痕各自在自己的代码路径上**重新构造** AttemptOutcome（不像串行路径那样复用同一个
	// 指针），不传播就会得到「真的走了 WS，但链上一条 WS 事实也没有」——修复前生产正是这样。
	// 写与读都在 race 锁下（见 recordWSFacts / wsFactsOf）：本 attempt 的留痕可能由胜者协程读。
	ws *AttemptWSFacts
	// outcomeRecorded 为真表示本 attempt 的结局留痕已落过一条；必须持有 race.mu 访问。
	// 胜者裁决时会先给在途输家各落一条（Node 的 abortAttempt 即在此时记录），此后
	// 输家自身的收尾路径（取消 / 引流计费 / 在途失败）不得再落第二条。
	outcomeRecorded bool
}

// hedgeRace 是一次 ForwardStreamHedge 的协调器。
type hedgeRace struct {
	ctx     context.Context
	pc      *pctx.Context
	deps    Deps
	options StreamOptions
	cfg     HedgeOptions
	now     func() time.Time

	// drainTimeout / maxDrainBytes 是输家 drain 的绝对上限（从 cfg 解析一次）。
	drainTimeout  time.Duration
	maxDrainBytes int64

	mu              sync.Mutex
	launched        []int64            // 按启动顺序记录供应商 id
	launchedSet     map[int64]struct{} // 已启动供应商（去重与排除）
	failed          []int64            // 已判失败的供应商（Select 排除）
	attempts        map[int]*hedgeAttempt
	active          int
	winnerCommitted bool
	settled         bool
	noMoreProviders bool
	lastFailure     *Failure
	// statefulRejection 是本次竞速遇到的「候选无法承载状态型字段」拒绝（取首次）。
	// 它不落成 lastFailure：跳过候选不是上游故障，只有池子耗尽时它才变成客户端的 400。
	statefulRejection *StatefulConversionError
	launching         bool
	// launchPending 记「启动窗口被占用期间收到过一次启动申请」。窗口持有者退出时必须补跑它，
	// 否则那次申请就凭空消失：active 可能已归零、noMoreProviders 又是假，整局再没人推进
	// （见 launchAlternative）。
	launchPending bool
	outcomes      []AttemptOutcome
	resultCh      chan hedgeResult
}

// hedgeResult 是竞速的终局。
type hedgeResult struct {
	result *StreamResult
	err    error
}

// ForwardStreamHedge 执行一次带竞速的流式转发。
//
// 行为对齐 sendStreamingWithHedge：initial 立即启动并带首字节阈值；
// 阈值到期且并发未满时启动备选供应商；首个有效内容者胜，其余输家按配置取消或计费。
//
// 返回值语义与 ForwardStream 一致：
//   - 胜者产生 Stream 时返回非 nil 的 result.Stream。
//   - 全部耗尽时返回 result（含尝试留痕）与可 errors.As 成 *Failure 的错误。
func ForwardStreamHedge(
	ctx context.Context,
	pc *pctx.Context,
	initial *Candidate,
	deps Deps,
	options StreamOptions,
	cfg HedgeOptions,
) (*StreamResult, error) {
	if deps.Dial == nil {
		return nil, errors.New("forward: 未提供拨号器")
	}
	if initial == nil {
		return nil, ErrNoProviderAvailable
	}
	if options.StartedAt.IsZero() {
		options.StartedAt = deps.now()
	}
	if cfg.After == nil {
		cfg.After = hedgeTimerAfter
	}
	if cfg.Now == nil {
		cfg.Now = deps.now
	}
	maxInFlight := cfg.MaxInFlight
	if maxInFlight <= 0 {
		maxInFlight = DefaultHedgeMaxInFlight
	}
	drainTimeout := cfg.LoserDrainTimeout
	if drainTimeout <= 0 {
		drainTimeout = DefaultHedgeLoserDrainTimeout
	}
	maxDrainBytes := cfg.LoserMaxDrainBytes
	if maxDrainBytes <= 0 {
		maxDrainBytes = DefaultHedgeLoserMaxDrainBytes
	}

	race := &hedgeRace{
		ctx:           ctx,
		pc:            pc,
		deps:          deps,
		options:       options,
		cfg:           cfg,
		now:           cfg.Now,
		drainTimeout:  drainTimeout,
		maxDrainBytes: maxDrainBytes,
		launchedSet:   make(map[int64]struct{}),
		attempts:      make(map[int]*hedgeAttempt),
		resultCh:      make(chan hedgeResult, 1),
	}

	race.launchAttempt(initial, maxInFlight)

	// finish 是唯一的出口收口：竞速耗尽（有尝试留痕、无胜者）时终态必须在这里落库一次，
	// 与串行路径 forwardLoop 的 defer 同口径。少了它，dataplane 只翻归因、不写终态，
	// 尝试留痕无处可去——竞速耗尽的行 provider_chain=[]、routing_trace=null（生产实测）。
	finish := func(out hedgeResult) (*StreamResult, error) {
		if out.result != nil && out.result.Stream == nil && len(out.result.Attempts) > 0 && deps.Settle != nil {
			var failure *Failure
			_ = errors.As(out.err, &failure)
			if err := deps.Settle(ctx, pc, &out.result.Result, failure); err != nil {
				deps.logger().Warn("forward.hedge.settle_failed", map[string]any{
					"status_code": out.result.StatusCode,
					"error":       err.Error(),
				})
			}
		}
		return out.result, out.err
	}

	select {
	case <-ctx.Done():
		// 请求被客户端取消：若胜者同时到达，让胜者胜出。
		select {
		case result := <-race.resultCh:
			return finish(result)
		default:
		}
		return nil, &Failure{
			Category:   CategoryClientAbort,
			Message:    "Request aborted by client",
			StatusCode: 499,
			Err:        ctx.Err(),
		}
	case result := <-race.resultCh:
		return finish(result)
	}
}

// launchAttempt 启动一次竞速 attempt（协程），在 mu 保护下完成准入登记。
func (r *hedgeRace) launchAttempt(c *Candidate, maxInFlight int) {
	r.mu.Lock()
	if r.winnerCommitted || r.settled {
		r.mu.Unlock()
		return
	}
	if len(r.launchedSet) > 0 && r.active >= maxInFlight {
		// 饱和**不写链**：Node 把它记为 routing-trace 事件（`type: "hedge_slot_saturated"`），
		// provider_chain 里没有这个词；而链上任何未知 reason 都会被公开状态分类器判成
		// **失败**（ClassifyRequestOutcomeSignal 的兜底分支只要求 reason 非空），
		// 即「一条信息性记录污染可用率」。Go 暂无数据面 trace 构造器，
		// 故用 debug 日志保留可观测性，同时保持链词表与 Node 逐词一致（见 chainreason.go）。
		providerID := c.Provider.ID
		providerName := c.Provider.Name
		active := len(r.launchedSet)
		at := r.now()
		r.mu.Unlock()
		r.deps.logger().Debug("forward.hedge.slot_saturated", map[string]any{
			"provider_id":   providerID,
			"provider_name": providerName,
			"max_in_flight": maxInFlight,
			"active":        active,
			"effect":        "candidate_not_launched",
		})
		// 与 debug 日志同源的事实交给 trace 构造器：Node 把饱和记为 routing-trace 事件，
		// 链词表里没有它（见上面的说明）。
		if r.cfg.TraceSink != nil {
			r.cfg.TraceSink.HedgeSlotSaturated(providerID, providerName, active, maxInFlight, at)
		}
		return
	}
	seq := len(r.launched) + 1
	if len(r.launchedSet) > 0 {
		// hedge_launched 是「备选已启动」的信息性记录：此刻该 attempt 的 plan **尚未编译**
		// （plan 在 runAttempt 里成形），故没有重定向快照可写；Node 侧此处也只传
		// circuitState，不传 modelRedirect/statusCode。两项都留空。
		r.appendOutcomeLocked(AttemptOutcome{
			ProviderID:   c.Provider.ID,
			ProviderName: c.Provider.Name,
			Reason:       ReasonHedgeLaunched,
			Attempt:      seq,
		})
	}
	r.launched = append(r.launched, c.Provider.ID)
	r.launchedSet[c.Provider.ID] = struct{}{}
	r.active++
	r.mu.Unlock()

	go r.runAttempt(c, seq, maxInFlight)
}

// runAttempt 执行一次竞速 attempt。
func (r *hedgeRace) runAttempt(c *Candidate, seq int, maxInFlight int) {
	attemptCtx, cancel := context.WithCancel(r.ctx)
	attempt := &hedgeAttempt{
		seq:       seq,
		provider:  c.Provider,
		verdict:   make(chan hedgeVerdict, 1),
		ctx:       attemptCtx,
		cancel:    cancel,
		startedAt: r.now(),
	}
	// 取消权的归属：默认随本函数返回释放（失败与输家路径）。
	// 胜者路径把它转交给正文生命周期（见 detachWinnerContext）：上游请求是用 attemptCtx
	// 拨的，在这里取消会腰斩尚未读完的正文（下游表现为胜者流截断 + local_error）。
	transferred := false
	defer func() {
		if !transferred {
			cancel()
		}
	}()
	r.registerAttempt(attempt)

	endpoint := Endpoint{}
	if endpoints := c.endpoints(); len(endpoints) > 0 {
		endpoint = endpoints[0]
	}
	attempt.endpoint = endpoint

	plan, err := BuildPlan(PlanInput{
		Client:            r.deps.Facts.Client,
		Target:            Target{Provider: c.Provider, Endpoint: endpoint},
		ConversionEnabled: c.ConversionEnabled,
		Overrides:         r.deps.Facts.Overrides,
		CacheTTL1h:        r.deps.Facts.CacheTTL1h,
		ClientUserAgent:   r.deps.Facts.ClientUserAgent,
		FilteredUserAgent: r.deps.Facts.FilteredUserAgent,
		UserAgentModified: r.deps.Facts.UserAgentModified,
	})
	if err != nil {
		// 候选级不可服务（状态型字段在目标线没有承载位）：不是上游故障，也没拨过号，故不走
		// finishAttemptFailed（那会替它落一条 system_error 留痕，在
		// CountNetworkFailureTowardCircuit 为真时还会记一次供应商健康度失败）。
		var rejected *StatefulConversionError
		if errors.As(err, &rejected) {
			r.skipUnservableStateful(attempt, rejected)
			return
		}
		failure := &Failure{
			Category:   CategorySystemError,
			Message:    err.Error(),
			Internal:   true,
			ProviderID: c.Provider.ID,
			Attempt:    seq,
			Err:        err,
		}
		r.finishAttemptFailed(attempt, failure)
		return
	}
	attempt.plan = plan

	// 首字节阈值：进入传输调用前装配（对齐 Node armAttemptThreshold）。
	if c.Provider.FirstByteTimeoutStreamingMS > 0 {
		attempt.timer = r.cfg.After(time.Duration(c.Provider.FirstByteTimeoutStreamingMS)*time.Millisecond, func() {
			r.triggerThreshold(c.Provider.ID, c.Provider.Name, maxInFlight)
		})
		defer attempt.timer.Stop()
	}

	outcome := AttemptOutcome{
		ProviderID:    c.Provider.ID,
		ProviderName:  c.Provider.Name,
		EndpointID:    endpoint.ID,
		EndpointURL:   plan.URL,
		Attempt:       seq,
		Redirected:    plan.Redirect != nil,
		ModelRedirect: attemptModelRedirect(plan),
	}

	// 上游 WS 与串行路径同一条缝：竞速的每一次 attempt 都先试 WS，失败再回落 HTTP。
	//
	// 为什么必须在这里也试：竞速一旦生效，**所有**流式请求都走本函数（阈值 > 0 且设置允许时），
	// 绕过 executeStreamAttempt——2026-09-19 生产上「上游 WS 72 小时零尝试、链上无键、
	// 日志无痕」就是这么来的：codex 供应商的首字节阈值 60000ms 让每个请求都命中竞速。
	// 只修串行路径等于「看起来支持，实则从不生效」。
	// 走到这里即为「真的尝试」：链上的 WS 事实由 wsAttempt 写进 outcome。
	response := r.deps.wsAttempt(attemptCtx, r.pc, c.Provider, plan, &outcome)
	// WS 事必在**回落 HTTP 之前**发布，理由有两条，都是「晚一步就丢事实」：
	//  1. 回落之后 dialAttempt 失败时直接走 finishAttemptFailed，那条失败留痕会拿不到 WS 事实
	//     （链上不会产生 WS 信息性条目），于是「试过 WS、回落了、HTTP 也挂了」看起来像「压根没试」。
	//  2. 竞速胜者可能在**本 attempt 仍在途**时裁决（markLoserOutcomeLocked 落输家结局），
	//     而裁决发生在别的协程、不回填已落的条目——输家同样会丢 WS 事实。
	r.recordWSFacts(attempt, outcome.WS)
	cancelDial := context.CancelFunc(func() {})
	if response == nil {
		var failure *Failure
		response, cancelDial, failure = r.deps.dialAttempt(attemptCtx, plan, &outcome, true)
		if failure != nil {
			r.finishAttemptFailed(attempt, failure)
			return
		}
	}
	defer cancelDial()
	attempt.dispatched = true

	// 非 2xx：完整读回错误正文，按串行路径同一口径分类。
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer func() { _ = response.Body.Close() }()
		if _, fail := r.deps.completeResponse(response, plan, &outcome); fail != nil {
			r.finishAttemptFailed(attempt, fail)
			return
		}
		// completeResponse 对非 2xx 不返回成功；此处仅防御兜底。
		r.finishAttemptFailed(attempt, &Failure{
			Category:   CategorySystemError,
			StatusCode: response.StatusCode,
			Internal:   true,
			ProviderID: c.Provider.ID,
			Attempt:    seq,
		})
		return
	}

	// 2xx：跑门控或透传读首块，产出「有效内容」。
	content, fail := r.runGateOrFirstChunk(attempt, attemptCtx, response, plan, &outcome)
	if fail != nil {
		r.finishAttemptFailed(attempt, fail)
		return
	}

	// 报告潜在胜者：协调器裁决本 attempt。
	r.reportContent(attempt, content)
	switch <-attempt.verdict {
	case verdictWinner:
		// 胜者的取消权已在 reportContent 转交给正文（见 detachWinnerContext），
		// 故本函数返回时不得再取消：那会把还没读完的正文腰斩。
		transferred = true
		// 成功记账：**竞速路径原先只记失败**，于是开闸后永远无人推进 half-open 计数
		// → 熔断再也回不到 closed（生产现象：一批供应商永远显示「熔断恢复中」）。
		//
		// 位置选在胜者裁决处而非 reportContent：裁决只发生一次，故天然幂等；
		// 若放在 reportContent 里，每个「潜在胜者」都会记一次，多路竞速时会计出多份成功。
		//
		// 输家不记成功也不记失败：它们是被主动取消/引流的，不是上游失败（与串行路径
		// 只在 failure==nil 时记成功、只在计入熔断的分类上记失败同口径）。
		if r.deps.RecordSuccess != nil {
			// 用 race 级 ctx（而非 attempt 级）：胜者的 attempt ctx 刚被交接给正文生命周期，
			// 用它可以避免「交接瞬间取消」导致记账被 ctx 取消掉。
			r.deps.RecordSuccess(r.ctx, attempt.provider.ID, attempt.endpoint.ID)
		}
		r.decrement()
	case verdictLoserCancel:
		// 输家：归还门控租约并切断连接。
		if content.Lease != nil {
			content.Lease.Release()
		}
		if content.Source != nil {
			_ = content.Source.Close()
		}
		r.appendLoserOutcome(attempt, "hedge_loser_cancelled")
		r.decrement()
	case verdictLoserBill:
		r.billLoser(attempt, content, r.drainTimeout, r.maxDrainBytes)
	}
}

// detachWinnerContext 把胜者 attempt 的取消权交给正文生命周期。
//
// 胜者 ctx 的生命周期 = 该次尝试的 stream 生命周期：泵在终态关闭正文时取消它。
// 取消早于最后一次读，就会把上游连接提前提断（胜者流截断）；完全不取消，则 attempt
// 的子 ctx 会活到请求 ctx 结束。正文已读尽（JSON 路径或门控读到 EOF 后无 Source）时
// 没有可截断的正文，当场取消。
func (r *hedgeRace) detachWinnerContext(content *streamAttempt, cancel context.CancelFunc) {
	if content.Source == nil {
		cancel()
		return
	}
	content.Source = &winnerCtxBody{source: content.Source, cancel: cancel}
}

// winnerCtxBody 把胜者 attempt 的 ctx 绑到正文上：正文关闭（流终态）时取消它。
type winnerCtxBody struct {
	source io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (b *winnerCtxBody) Read(buffer []byte) (int, error) { return b.source.Read(buffer) }

func (b *winnerCtxBody) Close() error {
	err := b.source.Close()
	b.once.Do(b.cancel)
	return err
}

// registerAttempt 登记一次在途 attempt。
func (r *hedgeRace) registerAttempt(attempt *hedgeAttempt) {
	r.mu.Lock()
	r.attempts[attempt.seq] = attempt
	r.mu.Unlock()
}

// runGateOrFirstChunk 跑门控或（未门控时）读首个可读块，产出有效内容。
func (r *hedgeRace) runGateOrFirstChunk(
	attempt *hedgeAttempt,
	attemptCtx context.Context,
	response *dial.Response,
	plan *Plan,
	outcome *AttemptOutcome,
) (*streamAttempt, *Failure) {
	family, gated := r.options.gateFamily(attempt.provider)
	if gated && r.options.shouldGate(response.Header, plan) {
		var firstByteAt time.Time
		result, err := r.runGate(attemptCtx, response, plan, outcome, family, &firstByteAt)
		if err != nil {
			_ = response.Body.Close()
			failure := r.deps.gateFailure(err, plan, outcome)
			// 与串行路径同款：主动判慢时标出真实死因、种类与自首字节起的时长。
			if failure != nil {
				if kind, slow := probeSlowKind(err); slow {
					failure.ProbeSlow = true
					failure.ProbeSlowKind = kind
					failure.ProbeElapsedMS = int(r.options.now().Sub(firstByteAt).Milliseconds())
					failure.ProbeSlowBytesPerSecond = slowRateBytesPerSecond(asPrecommit(err))
				}
			}
			return nil, failure
		}
		return result, nil
	}

	if !r.options.isStreamShaped(response.Header, plan) && !r.options.ForceGate {
		// 非流形态（例如 JSON）：完整读回，正文整体作为「成功内容」。
		response2, fail := r.deps.completeResponse(response, plan, outcome)
		_ = response.Body.Close()
		if fail != nil {
			return nil, fail
		}
		return &streamAttempt{
			StatusCode: response2.StatusCode,
			Status:     response2.Status,
			Header:     response2.Header,
			Prefix:     [][]byte{response2.Body},
			ReaderDone: true,
			Gated:      false,
			Family:     family,
		}, nil
	}

	// 未门控的流形态：读第一个可读块判定有效性（空流 = 空响应失败）。
	source := response.Body
	buffer := make([]byte, DefaultPumpChunkBytes)
	n, err := source.Read(buffer)
	if n > 0 {
		chunk := make([]byte, n)
		copy(chunk, buffer[:n])
		return &streamAttempt{
			StatusCode: response.StatusCode,
			Status:     response.Status,
			Header:     response.Header,
			Prefix:     [][]byte{chunk},
			Source:     source,
			Gated:      false,
			Family:     family,
		}, nil
	}
	_ = response.Body.Close()
	if err == nil || errors.Is(err, io.EOF) {
		return nil, &Failure{
			Category:      CategoryProviderError,
			StatusCode:    response.StatusCode,
			Message:       fmt.Sprintf("Empty response from provider %s: Response body is empty", outcome.ProviderName),
			EmptyResponse: true,
			ProviderID:    outcome.ProviderID,
			ProviderName:  outcome.ProviderName,
			EndpointID:    outcome.EndpointID,
			EndpointURL:   plan.URL,
			Attempt:       outcome.Attempt,
		}
	}
	return nil, &Failure{
		Category:     CategorySystemError,
		StatusCode:   response.StatusCode,
		Message:      fmt.Sprintf("读取上游正文失败: %v", err),
		ProviderID:   outcome.ProviderID,
		ProviderName: outcome.ProviderName,
		EndpointID:   outcome.EndpointID,
		EndpointURL:  plan.URL,
		Attempt:      outcome.Attempt,
		Err:          err,
	}
}

// runGate 复用串行路径的门控执行。
//
// firstByteAt 是出参：首字节到达时刻（未到达则为零值）。中途探测判废要靠它算
// 「自首字节起等了多久」——而失败路径拿不到 streamAttempt，故必须单独回传。
func (r *hedgeRace) runGate(
	ctx context.Context,
	response *dial.Response,
	plan *Plan,
	outcome *AttemptOutcome,
	family gate.Family,
	firstByteAt *time.Time,
) (*streamAttempt, error) {
	startedAt := r.options.now()
	// 与 stream.go 同口径：first_byte_ms 取**上游**首个非空 chunk 的到达时刻，
	// 而不是前缀交给我们（提交）的时刻。
	var upstreamFirstByteAt time.Time
	result, err := gate.Run(ctx, response.Body, r.options.gateOptions(family, outcome, func() {
		upstreamFirstByteAt = r.options.now()
	}))
	*firstByteAt = upstreamFirstByteAt
	if err != nil {
		return nil, err
	}
	// 提交时若仍有在飞读，必须改读门控交回的续读句柄，否则那批已从上游取走的字节会丢。
	source := response.Body
	if result.Continuation != nil {
		source = continuationSource{reader: result.Continuation, closer: response.Body}
	}
	return &streamAttempt{
		StatusCode:          response.StatusCode,
		Status:              response.Status,
		Header:              response.Header,
		Prefix:              result.Prefix,
		Source:              source,
		ReaderDone:          result.ReaderDone,
		UpstreamFirstByteAt: upstreamFirstByteAt,
		Lease:               result.Lease,
		Gated:               true,
		FramesSeen:          result.FramesSeen,
		Marker:              result.Marker,
		GateWait:            r.options.now().Sub(startedAt),
		Family:              family,
	}, nil
}

// triggerThreshold 是首字节阈值到期回调：并发未满就启动下一个候选。
func (r *hedgeRace) triggerThreshold(providerID int64, providerName string, maxInFlight int) {
	r.mu.Lock()
	if r.winnerCommitted || r.settled {
		r.mu.Unlock()
		return
	}
	excluded := r.excludedLocked()
	saturated := r.active >= maxInFlight
	// active 必须在**锁内**取：解锁后再读 r.active 会与别的 attempt 协程的
	// decrementLocked（`r.active--`）构成数据竞争——`-race` 实测报 DATA RACE
	// （写 hedge.go 的 `r.active--`、读本函数的日志行），且写侧持锁、读侧未持。
	active := r.active
	r.mu.Unlock()

	if saturated {
		// 同 launchAttempt：饱和是路由留痕事件而非链结局，写链会被分类器判成失败。
		r.deps.logger().Debug("forward.hedge.slot_saturated", map[string]any{
			"provider_id":   providerID,
			"provider_name": providerName,
			"max_in_flight": maxInFlight,
			"active":        active,
			"effect":        "candidate_not_launched",
		})
		return
	}
	r.launchAlternative(excluded)
}

// excludedLocked 返回当前排除列表（已启动 + 已失败）；必须持有 mu。
func (r *hedgeRace) excludedLocked() []int64 {
	excluded := make([]int64, 0, len(r.launchedSet)+len(r.failed))
	excluded = append(excluded, r.launched...)
	excluded = append(excluded, r.failed...)
	return excluded
}

// launchAlternative 从候选里选下一个供应商启动（同一时刻只有一个启动协程）。
// excluded 为 nil 时现场从 launched+failed 组装排除列表。
//
// 循环而非单趟：launchAlternativeOnce 返回真表示「它占着窗口时有人申请过启动，而那次申请
// 被挂起」，此时必须由本循环补跑。为什么不能把那次申请直接丢掉：申请来自「某个 attempt
// 的 BuildPlan 因状态型字段被拒 —— skipUnservableStateful」或「首字节阈值到期」，两者都是
// 推进竞速的唯一动力；丢掉它后 active 可能已经归零、noMoreProviders 却仍是假，于是既没有
// 在途 attempt 也没有启动者，请求只能等 context 取消（竞速悬挂）。
//
// 补跑的次数有界：每次补跑必须先有人重新申请，而申请者（跳过/阈值）的数量受候选集与
// 在途 attempt 数约束；且窗口内的申请只记一个标志位（多次并发申请合并成一次补跑）。
func (r *hedgeRace) launchAlternative(excluded []int64) {
	for {
		if !r.launchAlternativeOnce(excluded) {
			return
		}
		// 补跑时排除列表重新取：这一趟可能已经启动/失败了候选。
		excluded = nil
	}
}

// launchAlternativeOnce 开一次启动窗口；返回真表示窗口内有被挂起的启动申请，调用方须补跑。
func (r *hedgeRace) launchAlternativeOnce(excluded []int64) (relaunch bool) {
	r.mu.Lock()
	if r.winnerCommitted || r.settled || r.noMoreProviders {
		r.mu.Unlock()
		return false
	}
	if r.launching {
		// 另一个启动者正占着窗口：把申请挂到它身上（见 launchAlternative），不静默丢弃。
		r.launchPending = true
		r.mu.Unlock()
		return false
	}
	r.launching = true
	if excluded == nil {
		excluded = r.excludedLocked()
	}
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		r.launching = false
		relaunch = r.launchPending
		r.launchPending = false
		r.mu.Unlock()
	}()

	maxInFlight := r.cfg.MaxInFlight
	if maxInFlight <= 0 {
		maxInFlight = DefaultHedgeMaxInFlight
	}

	for {
		r.mu.Lock()
		if r.winnerCommitted || r.settled {
			r.mu.Unlock()
			return
		}
		r.mu.Unlock()

		next, err := r.deps.Select(r.ctx, excluded)
		if err != nil {
			r.failSelection(err)
			return
		}
		if next == nil {
			r.mu.Lock()
			r.noMoreProviders = true
			r.mu.Unlock()
			r.finishIfExhausted()
			return
		}

		r.mu.Lock()
		if r.winnerCommitted || r.settled {
			r.mu.Unlock()
			return
		}
		if _, already := r.launchedSet[next.Provider.ID]; already {
			// 防御：Select 未遵守排除列表则跳过该候选并继续选。
			excluded = append(excluded, next.Provider.ID)
			r.mu.Unlock()
			continue
		}
		r.mu.Unlock()

		r.launchAttempt(next, maxInFlight)
		return
	}
}

// skipUnservableStateful 处理「该候选无法承载状态型字段」：不留**失败**留痕、不计供应商健康度，
// 只把该供应商记入排除集并把竞速推进到下一个候选；池子耗尽时由终局把这条拒绝交给客户端。
//
// 「不留痕」的确切范围（勿读成「零痕迹」）：本候选既没有失败结局（provider_chain 里不会出现
// retry_failed 之类），也不计供应商健康度，更没有拨号；但它**计入**两处审计——启动时的
// hedge_launched 占位（它确实作为备选被启动过）与 TotalProvidersAttempted，另有一条
// effect=candidate_skipped 的 warn 日志。这两处是有意保留的：它们说明「考虑过它、跳过了」，
// 而不是「它失败了」。
//
// 归零口径与 finishAttemptFailed 的失败路径一致，且必须置 outcomeRecorded：否则胜者裁决时
// markLoserOutcomeLocked 会把这个**从未拨号**的候选记成竞速输家。
func (r *hedgeRace) skipUnservableStateful(attempt *hedgeAttempt, rejection *StatefulConversionError) {
	r.mu.Lock()
	if r.winnerCommitted {
		// 胜者已定：本 attempt 已无意义，静默归零（其 ctx 由 runAttempt 的 defer 释放）。
		attempt.outcomeRecorded = true
		r.active--
		r.mu.Unlock()
		return
	}
	if r.statefulRejection == nil {
		r.statefulRejection = rejection
	}
	r.failed = append(r.failed, attempt.provider.ID)
	attempt.outcomeRecorded = true
	r.active--
	r.mu.Unlock()

	r.deps.logger().Warn("forward: 候选无法承载状态型字段，跳过", map[string]any{
		"provider_id":   attempt.provider.ID,
		"provider_type": string(attempt.provider.Type),
		"field":         rejection.Field,
		"effect":        "candidate_skipped",
		"race":          true,
	})

	r.launchAlternative(nil)
}

// failSelection 记录选路失败并把整局判定为耗尽。
func (r *hedgeRace) failSelection(err error) {
	r.mu.Lock()
	r.noMoreProviders = true
	if r.lastFailure == nil {
		r.lastFailure = &Failure{
			Category: CategorySystemError,
			Message:  err.Error(),
			Internal: true,
			Err:      err,
		}
	}
	r.mu.Unlock()
	r.finishIfExhausted()
}

// reportContent 报告潜在胜者并做胜者仲裁。
func (r *hedgeRace) reportContent(attempt *hedgeAttempt, content *streamAttempt) {
	r.mu.Lock()
	if r.settled || r.winnerCommitted {
		r.mu.Unlock()
		if r.billable(attempt) {
			attempt.verdict <- verdictLoserBill
		} else {
			attempt.verdict <- verdictLoserCancel
		}
		return
	}
	r.winnerCommitted = true
	headgeWin := len(r.launched) > 1
	// 其余在途 attempt：非计费的立即取消（拨号/门控随 attempt ctx 停止），
	// 计费的保留（其正文还要被 drain 拿回用量）。
	//
	// 结局留痕在**此刻**就落（对齐 Node 的 abortAttempt：胜者裁决时即写
	// hedge_loser_billed / hedge_loser_cancelled）：输家往往在胜者的流结束之后才得出
	// 结论，若等它自己落链，流终态写下的 provider_chain 就会缺掉输家那一段。
	for _, other := range r.attempts {
		if other == attempt {
			continue
		}
		r.markLoserOutcomeLocked(other)
		if !r.billable(other) {
			other.cancel()
		}
	}
	r.mu.Unlock()

	if headgeWin {
		r.appendOutcome(AttemptOutcome{
			ProviderID:    attempt.provider.ID,
			ProviderName:  attempt.provider.Name,
			EndpointID:    attempt.endpoint.ID,
			EndpointURL:   attempt.plan.URL,
			Attempt:       attempt.seq,
			Reason:        ReasonHedgeWinner,
			StatusCode:    content.StatusCode,
			ModelRedirect: attemptModelRedirect(attempt.plan),
			DurationMS:    r.now().Sub(r.startedOffset()).Milliseconds(),
			StartedAt:     attempt.startedAt,
			FinishedAt:    r.now(),
			WS:            r.wsFactsOf(attempt),
		})
	} else {
		r.appendOutcome(AttemptOutcome{
			ProviderID:    attempt.provider.ID,
			ProviderName:  attempt.provider.Name,
			EndpointID:    attempt.endpoint.ID,
			EndpointURL:   attempt.plan.URL,
			Attempt:       attempt.seq,
			Reason:        ReasonRequestSuccess,
			StatusCode:    content.StatusCode,
			ModelRedirect: attemptModelRedirect(attempt.plan),
			DurationMS:    r.now().Sub(r.startedOffset()).Milliseconds(),
			StartedAt:     attempt.startedAt,
			FinishedAt:    r.now(),
			WS:            r.wsFactsOf(attempt),
		})
	}

	// 其余 attempt：不主动打断它们自己的拨号/门控——它们要么以输家收尾
	// （读到自己的 verdict），要么在失败路径看到 winnerCommitted 后被静默收尾。
	attempt.verdict <- verdictWinner

	// 胜者 ctx 的生命周期 = 该次尝试的 stream 生命周期：把取消权交给正文。
	// 必须早于 newStream 捕获来源——Stream 构造后再换 Source，就没人会去关它了（子 ctx 泄漏）。
	r.detachWinnerContext(content, attempt.cancel)
	if r.cfg.WinnerDetached != nil {
		r.cfg.WinnerDetached(attempt.provider.ID, attempt.ctx)
	}

	result := &StreamResult{
		Result: Result{
			Provider:                attempt.provider,
			Endpoint:                attempt.endpoint,
			Plan:                    attempt.plan,
			StatusCode:              content.StatusCode,
			Status:                  content.Status,
			Headers:                 content.Header,
			Attempts:                r.sortedOutcomes(),
			TotalProvidersAttempted: r.launchedSnapshot(),
			DetectorMissing:         r.deps.Detector == nil,
			StartedAt:               r.options.StartedAt,
			EndedAt:                 r.now(),
		},
	}
	result.Stream = newStream(r.ctx, content, attempt.provider, attempt.endpoint, attempt.plan, r.pc, r.options, r.sortedOutcomes)
	// 竞速留痕只在胜者提交时落进 Result；输家条目是随后才追加的，故 Stream 取的是
	// 「终态那一刻的快照」而不是这里的切片（见 Stream.attempts）。
	// 注意 sortedOutcomes 自带互斥，可以从输家协程与终态协程并发调用。
	r.resultCh <- hedgeResult{result: result}
}

// startedOffset 是报告胜者时的时刻基准（胜者留痕的时长口径）。
func (r *hedgeRace) startedOffset() time.Time { return r.options.StartedAt }

// launchedSnapshot 返回已启动供应商 id 的副本。
func (r *hedgeRace) launchedSnapshot() []int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.launchedSnapshotLocked()
}

// launchedSnapshotLocked 见 launchedSnapshot；必须持有 mu。
func (r *hedgeRace) launchedSnapshotLocked() []int64 {
	return append([]int64(nil), r.launched...)
}

// billable 判断输家是否开启计费：开关 + 请求行存在 + 计费接缝非空。
func (r *hedgeRace) billable(attempt *hedgeAttempt) bool {
	if !r.cfg.BillLosers || r.cfg.LoserBiller == nil {
		return false
	}
	if r.pc == nil {
		return false
	}
	_, ok := r.pc.MessageRequestID()
	return ok
}

// billLoser 后台 drain 输家正文拿回用量并计费（幂等守卫在接线层）。
func (r *hedgeRace) billLoser(
	attempt *hedgeAttempt,
	content *streamAttempt,
	drainTimeout time.Duration,
	maxDrainBytes int64,
) {
	var requestID int64
	if r.pc != nil {
		requestID, _ = r.pc.MessageRequestID()
	}
	evidence, drained := r.drainLoser(content, drainTimeout, maxDrainBytes)

	r.appendLoserOutcome(attempt, "hedge_loser_billed")

	bill := HedgeLoserBill{
		RequestID:          requestID,
		ProviderID:         attempt.provider.ID,
		ProviderName:       attempt.provider.Name,
		Sequence:           attempt.seq,
		UpstreamStatusCode: content.StatusCode,
		Usage:              evidence.Usage,
		Model:              evidence.Model,
		DrainComplete:      drained,
		At:                 r.now(),
	}
	// drain 在后台完成：不给胜者响应让路（对齐 Node 的 fire-and-forget）。
	ctx := context.WithoutCancel(r.ctx)
	if err := r.cfg.LoserBiller.BillLoser(ctx, bill); err != nil {
		r.deps.logger().Warn("forward.hedge.loser_billing_failed", map[string]any{
			"provider_id":   attempt.provider.ID,
			"provider_name": attempt.provider.Name,
			"request_id":    requestID,
			"error":         err.Error(),
		})
	}
	r.decrement()
}

// drainLoser 有界地读光输家正文并产出用量证据。
//
// 内存不变量：正文只喂进 O(1) 观测器后即丢弃；前缀块（门控缓冲）是唯一的正文驻留，
// 且已被 prebuffer 租约约束。
func (r *hedgeRace) drainLoser(
	content *streamAttempt,
	drainTimeout time.Duration,
	maxDrainBytes int64,
) (Observation, bool) {
	observer := NewObserver(ObservationOptions{
		StartedAt:           r.options.StartedAt,
		UpstreamFirstByteAt: content.UpstreamFirstByteAt,
		Format:              r.options.Format,
		HeadBytes:           r.options.HeadBytes,
		TailBytes:           r.options.TailBytes,
		Now:                 r.options.Now,
	})
	for _, chunk := range content.Prefix {
		observer.Push(chunk)
	}

	drained := false
	if content.Source != nil {
		defer func() { _ = content.Source.Close() }()
		deadline := time.After(drainTimeout)
		buffer := make([]byte, DefaultPumpChunkBytes)
		var total int64
		for {
			select {
			case <-deadline:
				goto done
			default:
			}
			if total >= maxDrainBytes {
				break
			}
			n, err := content.Source.Read(buffer)
			if n > 0 {
				observer.Push(buffer[:n])
				total += int64(n)
			}
			if err != nil {
				drained = errors.Is(err, io.EOF)
				break
			}
		}
	done:
	} else {
		// 无 Source（ReaderDone 的 JSON 路径）：前缀即全部，视为自然结束。
		drained = true
	}

	snapshot := observer.Snapshot()
	if snapshot.CompletionMarker || snapshot.ErrorText != "" {
		// 终态帧意味着没有更多账单；即便没读到 EOF 也视为 drain 完整。
		drained = true
	}
	if content.Lease != nil {
		content.Lease.Release()
		content.Lease = nil
	}
	return snapshot, drained
}

// finishAttemptFailed 记录一次失败。若失败分类要求立即终局（客户端中断/本地过载/
// 不可重试客户端错误），裁掉整局；否则若尚未有胜者则启动新候选、并在耗尽时终局。
//
// 若已有胜者（本 attempt 是在途输家），只归零不碰终局。
func (r *hedgeRace) finishAttemptFailed(attempt *hedgeAttempt, failure *Failure) {
	outcome := AttemptOutcome{
		ProviderID:    attempt.provider.ID,
		ProviderName:  attempt.provider.Name,
		EndpointID:    attempt.endpoint.ID,
		Attempt:       attempt.seq,
		StatusCode:    failure.StatusCode,
		Category:      failure.Category,
		Message:       failure.Message,
		Reason:        reasonForCategory(failure.Category, failure.StatusCode),
		ModelRedirect: attemptModelRedirect(attempt.plan),
		StartedAt:     attempt.startedAt,
		FinishedAt:    r.now(),
		// 上游 WS 事实与结局无关：降级后 HTTP 失败也是「本次尝试试过 WS」的事。
		WS: r.wsFactsOf(attempt),
	}
	if attempt.plan != nil {
		outcome.EndpointURL = attempt.plan.URL
	}

	r.mu.Lock()
	if r.winnerCommitted {
		// 输家在途失败（多半是赢家提交后本 attempt 被取消）：只留痕归零，不动终局。
		if failure.Category == CategoryClientAbort {
			outcome.Reason = ReasonHedgeLoserCancel
		}
		if attempt.outcomeRecorded {
			// 胜者裁决时已落过结局：同一 attempt 不再落第二条（Node 的 attempt.settled 同义）。
			// 但那条结论未必是**真结论**：裁决与失败观测抢同一把锁，谁先拿到谁定留痕
			// （见 correctCancelledPlaceholderLocked），故这里做最后一道校正。
			r.correctCancelledPlaceholderLocked(attempt, outcome)
			r.active--
			r.mu.Unlock()
			return
		}
		attempt.outcomeRecorded = true
		r.appendOutcomeLocked(outcome)
		r.active--
		r.mu.Unlock()
		return
	}

	if failure.Category == CategoryClientAbort ||
		failure.Category == CategoryLocalOverload ||
		failure.Category == CategoryNonRetryableClientError {
		r.settled = true
		r.lastFailure = failure
		attempt.outcomeRecorded = true
		r.appendOutcomeLocked(outcome)
		for _, other := range r.attempts {
			other.cancel()
		}
		r.resultCh <- hedgeResult{err: failure}
		r.mu.Unlock()
		return
	}

	r.lastFailure = failure
	if failure.Category == CategoryProviderError || failure.Category == CategorySystemError ||
		failure.Category == CategorySlowRate {
		if !failure.RequestScoped &&
			(failure.Category.CountsTowardCircuit() ||
				(failure.Category == CategorySystemError && r.deps.CountNetworkFailureTowardCircuit)) {
			if r.deps.RecordFailure != nil {
				r.deps.RecordFailure(r.ctx, failure)
			}
		}
		// 判慢必须进 failed：excludedLocked 据此把该家排出重选，否则竞速路径会再次选中它，
		// 「立即换家」就只对串行路径生效（两条路径行为分叉）。计熔断那一支由
		// CountsTowardCircuit()==false 自然跳过（慢不是错）。
		r.failed = append(r.failed, attempt.provider.ID)
	}
	// 本次尝试的结局在此落定，必须标记：否则日后胜者裁决时 markLoserOutcomeLocked 仍会
	// 把它当成**在途**输家，再落一条 hedge_loser_billed / hedge_loser_cancelled。
	// Node 的 abortAttempt 首行即 `if (attempt.settled) return;`，且已结束的 attempt 会被
	// 移出 attempts 集合，故失败者不会被标成竞速输家（真实事故：HC_Chat 上游 500/503
	// 失败，链里却同时出现 retry_failed 与 hedge_loser_billed，让「谁输掉了竞速」失真）。
	attempt.outcomeRecorded = true
	r.appendOutcomeLocked(outcome)
	r.active--
	r.mu.Unlock()

	r.launchAlternative(nil)
}

// decrement 归零一条活跃 attempt；耗尽时终局。
func (r *hedgeRace) decrement() {
	r.mu.Lock()
	r.decrementLocked()
	r.mu.Unlock()
}

func (r *hedgeRace) decrementLocked() {
	r.active--
	r.maybeFinishLocked()
}

// maybeFinishLocked 在无候选且无在途时终局；必须持有 mu。调用方不释放 mu。
func (r *hedgeRace) maybeFinishLocked() {
	if r.settled || r.winnerCommitted || r.active > 0 || !r.noMoreProviders {
		return
	}
	r.settled = true
	var err error
	switch {
	case r.lastFailure != nil:
		err = r.lastFailure
	case r.statefulRejection != nil:
		// 池中所有候选都无法承载状态型字段：给客户端 400 + 字段名（与串行路径同口径），
		// 而不是一个「供应商耗尽」的 5xx。
		err = r.statefulRejection
	}
	// 只有真的失败（err 非空）才附上结果：err 为空的穷尽路径与接线前逐字一致
	// （err 为空的形态只在两个归因都缺失时出现，那不属于「尝试失败」）。
	var result *StreamResult
	if err != nil {
		result = r.exhaustedResultLocked()
	}
	r.resultCh <- hedgeResult{result: result, err: err}
}

// exhaustedResultLocked 造「全部尝试失败」的终局结果；必须持有 mu。
//
// 与串行路径同一契约（见 stream.go 的 ForwardStream 返回值说明与 forwardLoop 的 exitWithError）：
// 只要真的尝试过，Result 就必须非 nil 且带尝试留痕——终态链只在 len(Attempts) > 0 时才写
// （见 storeSettler.NonStream）。缺了它，竞速耗尽的行 provider_chain=[]、routing_trace=null
// （生产实测：16 行 provider_id=167 / status_code=524 的记录）。
//
// 零尝试（候选全在计划阶段被状态型字段拒掉）时交回 nil：那条路径由调用方翻成 400 并自行结算
// （见 stateful_skip_test.go 的两条穷尽断言）。
func (r *hedgeRace) exhaustedResultLocked() *StreamResult {
	if len(r.outcomes) == 0 {
		return nil
	}
	return &StreamResult{
		Result: Result{
			Attempts:                r.sortedOutcomesLocked(),
			TotalProvidersAttempted: r.launchedSnapshotLocked(),
			DetectorMissing:         r.deps.Detector == nil,
			StartedAt:               r.options.StartedAt,
			EndedAt:                 r.now(),
		},
	}
}

// finishIfExhausted 无 mu 版本：拿锁后检查耗尽。
func (r *hedgeRace) finishIfExhausted() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.maybeFinishLocked()
}

// appendOutcome 追加一条留痕。
func (r *hedgeRace) appendOutcome(outcome AttemptOutcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.appendOutcomeLocked(outcome)
}

// appendOutcomeLocked 追加一条留痕；必须持有 mu。
func (r *hedgeRace) appendOutcomeLocked(outcome AttemptOutcome) {
	r.outcomes = append(r.outcomes, outcome)
}

// recordWSFacts 发布本次 attempt 的上游 WS 事实（nil 不写，保持「无事实即无痕迹」）。
//
// 调用点必须在**任何留痕之前**（见 ForwardStreamHedge 里的说明）：留痕可能由胜者协程构造，
// 而事实由本 attempt 自己的协程发布；发布晚了就会丢。
func (r *hedgeRace) recordWSFacts(attempt *hedgeAttempt, facts *AttemptWSFacts) {
	if facts == nil {
		return
	}
	r.mu.Lock()
	attempt.ws = facts
	r.mu.Unlock()
}

// wsFactsOf 读本次 attempt 的上游 WS 事实。
//
// 用锁而非直接读字段：留痕可能由**别的协程**构造（胜者协程给在途输家落结局），
// 而事实是那个 attempt 自己的协程发布的。
func (r *hedgeRace) wsFactsOf(attempt *hedgeAttempt) *AttemptWSFacts {
	r.mu.Lock()
	defer r.mu.Unlock()
	return attempt.ws
}

// markLoserOutcomeLocked 在胜者裁决时给一条在途 attempt 落输家结局；必须持有 mu。
//
// 原因按「此刻能否计费」二选一：与 Node 的 attempt.billAsLoser 同一判定——链上先标记，
// 真正的引流与成本写回随后在后台发生（无价格等情形仍可能不写库）。
func (r *hedgeRace) markLoserOutcomeLocked(attempt *hedgeAttempt) {
	if attempt.outcomeRecorded {
		return
	}
	reason := "hedge_loser_cancelled"
	if r.billable(attempt) {
		reason = ReasonHedgeLoserBilled
	}
	r.appendLoserOutcomeLocked(attempt, reason)
}

// correctCancelledPlaceholderLocked 把「胜者裁决时落的取消占位结论」校正为真实失败；必须持有 mu。
//
// 为什么需要它：裁决（markLoserOutcomeLocked）与失败观测（finishAttemptFailed）抢同一把
// mutex，**谁先拿到锁谁定留痕**，于是同一份事实会因调度顺序得出两种结论。实测（2 核 +
// 后台负载，本用例连跑 25 次）：**18 次**里 attempt 先被标成 hedge_loser_cancelled，随后才
// 观测到上游真回的 500（`provider_error` / status=500）。那条失败被静默丢弃，链上只留「我们主动取消了它」——把一次真实的上游
// 故障记成我们自己放弃，诊断信息全丢。
//
// 生效条件（三条同时满足，任一不满足都保持原样）：
//  1. 晚到的失败是**供应商故障且带着真实上游状态码（4xx/5xx）**。
//     这一条同时排除了两类不该改正的情形：我们自己的取消（`client_abort`，占位结论
//     本就是最准确的描述）、以及「读正文时被取消」这类只在文案里才看得出归属的系统错误
//     （它带着 response.StatusCode，但那不是上游对本请求的答复）。
//  2. 该 attempt 已落的留痕是**非计费**的取消占位（hedge_loser_cancelled）——计费的
//     hedge_loser_billed 牵着成本写回与引流，改它会与账务口径纠缠，故不碰。
//  3. 能按 providerID + attempt 序号定位到那条占位留痕。
//
// 只**就地改写**那一条，不新增、不重排——同一 attempt 永远只有一条留痕（这是此前那起
// 「同一供应商同时出现 retry_failed 与 hedge_loser_billed」事故的教训）。
func (r *hedgeRace) correctCancelledPlaceholderLocked(attempt *hedgeAttempt, outcome AttemptOutcome) {
	if attempt == nil {
		return
	}
	if outcome.Category != CategoryProviderError {
		return
	}
	if outcome.StatusCode < 400 || outcome.StatusCode > 599 {
		return
	}
	for index := range r.outcomes {
		entry := &r.outcomes[index]
		if entry.ProviderID != attempt.provider.ID || entry.Attempt != attempt.seq {
			continue
		}
		if entry.Reason != ReasonHedgeLoserCancel {
			return
		}
		entry.Reason = outcome.Reason
		entry.StatusCode = outcome.StatusCode
		entry.Category = outcome.Category
		entry.Message = outcome.Message
		entry.FinishedAt = outcome.FinishedAt
		return
	}
}

// appendLoserOutcome 追加一条输家结局留痕（未落过才落）。
func (r *hedgeRace) appendLoserOutcome(attempt *hedgeAttempt, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.appendLoserOutcomeLocked(attempt, reason)
}

// appendLoserOutcomeLocked 追加一条输家结局留痕；必须持有 mu。同一 attempt 只落一条。
func (r *hedgeRace) appendLoserOutcomeLocked(attempt *hedgeAttempt, reason string) {
	if attempt.outcomeRecorded {
		return
	}
	attempt.outcomeRecorded = true
	outcome := AttemptOutcome{
		ProviderID:   attempt.provider.ID,
		ProviderName: attempt.provider.Name,
		EndpointID:   attempt.endpoint.ID,
		Attempt:      attempt.seq,
		Reason:       reason,
		// 输家同样带上本次尝试的 WS 事实：一个「已连上上游 WS 却被取消」的输家与
		// 「连 HTTP 都没发出去」的输家是不同的运维事实。
		WS: attempt.ws,
		// 输家不写 statusCode：Node 在 hedge_loser_billed 上传的是
		// `attempt.response?.status`（`forwarder.ts:5229`），而它同样**多数时候是 undefined**
		// ——裁决发生在胜者首字节到达时，输家未必已收到响应头；Go 的输家结局也在同一时刻
		// 落痕（引流发生在之后的 billLoser），此刻同样无状态码可写。
		// hedge_loser_cancelled 在 Node 侧本就不传 statusCode（`forwarder.ts:5246`）。
		ModelRedirect: attemptModelRedirect(attempt.plan),
	}
	if attempt.plan != nil {
		outcome.EndpointURL = attempt.plan.URL
	}
	r.appendOutcomeLocked(outcome)
}

// sortedOutcomes 返回按启动顺序排序的留痕（seq 即启动序号，失败输家无胜者时补一）。
func (r *hedgeRace) sortedOutcomes() []AttemptOutcome {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sortedOutcomesLocked()
}

// sortedOutcomesLocked 见 sortedOutcomes；必须持有 mu。
func (r *hedgeRace) sortedOutcomesLocked() []AttemptOutcome {
	out := append([]AttemptOutcome(nil), r.outcomes...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Attempt < out[j].Attempt })
	return out
}
