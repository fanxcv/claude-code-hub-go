package forward

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/rectify"
	"github.com/fanxcv/claude-code-hub-go/go/internal/upws"
)

// 转发路径的不可恢复错误。
var (
	// ErrNoProviderAvailable 表示选路没有给出任何候选。
	ErrNoProviderAvailable = errors.New("forward: 无可用供应商")
	// ErrProvidersExhausted 表示所有候选都已尝试并失败。
	ErrProvidersExhausted = errors.New("forward: 所有供应商均尝试失败")
)

// Candidate 是一次尝试的候选：供应商 + 端点候选 + 本次尝试的策略开关。
type Candidate struct {
	Provider Provider
	// Endpoints 是端点候选，按尝试顺序排列；为空时退化为 Provider.URL 单端点。
	Endpoints []Endpoint
	// ConversionEnabled 对应 provider.protocol_conversion_enabled。
	ConversionEnabled bool
	// RawPassthrough 为真表示该端点属原始透传策略。
	RawPassthrough bool
	// RawCrossProviderFallback 为真表示原始透传也允许跨供应商回退。
	RawCrossProviderFallback bool
}

// endpoints 返回实际参与尝试的端点列表。
func (c *Candidate) endpoints() []Endpoint {
	if len(c.Endpoints) > 0 {
		return c.Endpoints
	}
	if c.Provider.URL == "" {
		return nil
	}
	return []Endpoint{{URL: c.Provider.URL}}
}

// SelectFunc 选出下一个候选供应商。excludeIDs 是已失败供应商列表。
//
// 返回 (nil, nil) 表示没有候选了；返回错误表示选路本身失败（例如选路数据源不可用）。
type SelectFunc func(ctx context.Context, excludeIDs []int64) (*Candidate, error)

// PlanFacts 是构造计划时与供应商无关的会话级事实。
type PlanFacts struct {
	// Client 是入站事实；Body 必须是最多被读一次的正文快照。
	Client ClientRequest
	// Overrides 为 nil 时不做供应商级参数覆写。
	Overrides OverrideApplier
	// CacheTTL1h 为真时补齐 anthropic-beta 的 1h 缓存标记。
	CacheTTL1h bool
	// ClientUserAgent / FilteredUserAgent / UserAgentModified 决定 codex 供应商的出站 UA。
	ClientUserAgent   string
	FilteredUserAgent string
	UserAgentModified bool
}

// ProviderInFlightResult 是一次在飞登记的结果。
//
// 为什么带上读数：被拒时客户端要拿到 Node 同形的七字段信封（`current` / `limit` 两字段），
// 而名额读数只存在于实现侧（Redis Lua 的返回值）；让实现把读数一并交回，
// 转发层就不需要为了一个文案再问一次 Redis。
type ProviderInFlightResult struct {
	// Allowed 为假表示该渠道已满：本次尝试不得拨号。
	Allowed bool
	// Current 是该渠道登记后的并发会话数（被拒时为拒绝当时的读数）。
	Current int
	// Release 非 nil 时必须在本尝试结束时恰好调用一次。
	Release func()
}

// Deps 是转发主干的外部依赖。除 Dial 外都可为空，空的语义见各字段。
type Deps struct {
	// Dial 是上游拨号器，必填。
	Dial *dial.Client
	// Select 在需要切换供应商时选出下一个候选；nil 表示不切换（首次候选失败即终止）。
	Select SelectFunc
	// Sleep 是可注入的等待函数；nil 时用 time.Sleep（受 ctx 取消约束）。
	Sleep func(ctx context.Context, d time.Duration) error
	// Now 是可注入时钟；nil 时用 time.Now。
	Now func() time.Time
	// Logger 为 nil 时写 stderr。
	Logger *logx.Logger
	// Limits 是路径上限；零值取默认。
	Limits Limits
	// Rules 为 nil 时跳过错误规则匹配。
	Rules RuleMatcher
	// Detector 为 nil 时跳过 fake-200 检测（缺口记录在 Result.DetectorMissing）。
	Detector BodyErrorDetector
	// WS 是上游 WebSocket 拨号器（codex 类供应商专用）；nil 表示不尝试上游 WS。
	//
	// 何时会用到它：客户端本身走 WS + 候选是 codex + 系统设置开关开 + 端点未命中不支持缓存，
	// 四条全真（前三条由 WSEligible 判，第四条由 upws.Dialer.EndpointEligible 判）。
	// 任一条不真都维持现状：走 HTTP 隧道，**不记任何降级痕迹**（与当前行为逐字一致）。
	WS *upws.Dialer
	// WSEligible 判定本次尝试是否具备走上游 WS 的资格（客户端传输层 / 供应商类型 / 全局开关）。
	// nil 表示永远不走 WS。
	//
	// 为什么带上 *pctx.Context：这三条判定的事实分别来自入口 headers（隧道标记）、候选供应商
	// 与系统设置快照，而 Deps 是跨请求共享的——判定必须是**每请求**的函数，不能烘进构造期。
	WSEligible func(ctx context.Context, pc *pctx.Context, provider Provider) bool
	// WSNotice 是「本该走上游 WS 却没走」的上报缝（见 WSSkip 与 WSSkipCause）；
	// nil 表示不上报。
	//
	// 为什么必须逐请求调用而由实现侧去重：跳过是每请求发生的事，而可读的信号是「哪条原因、
	// 哪家供应商」的组合——去重键与限频窗口是运维口径，属于实现侧（热路径上只付一次函数调用）。
	// 回调只在客户端为 WS 通道时触发（见 noticeWSSkip）。
	WSNotice func(skip WSSkip)
	// CountNetworkFailureTowardCircuit 对应 ENABLE_CIRCUIT_BREAKER_ON_NETWORK_ERRORS。
	CountNetworkFailureTowardCircuit bool
	// RecordFailure 计一次供应商熔断失败；nil 时跳过。仅在分类计入熔断时调用。
	RecordFailure func(ctx context.Context, failure *Failure)
	// RecordSuccess 计一次供应商成功（响应已被接受）；nil 时跳过。
	//
	// 与 RecordFailure 成对：只记失败不记成功会让熔断器再也归不了闭（Node 侧
	// recordSuccess 承担半开计数与闭态清零，见 src/lib/circuit-breaker.ts:640）。
	// endpointID <= 0 表示本次尝试没有端点（端点级记账由接线层跳过）。
	RecordSuccess func(ctx context.Context, providerID int64, endpointID int64)
	// ProviderInFlight 在**每次上游尝试**（串行、同家重试、切换供应商、竞速的每个 attempt）
	// 向某渠道拨号**之前**登记一次在飞占用。
	//
	// 契约：
	//   - `Allowed` 为假表示该渠道已满：调用方**不得拨号**，按「供应商饱和」失败处理
	//     （同家不重试、换家，见 CategoryProviderSaturated）；
	//   - `Release` 非 nil 时必须在本尝试结束时**恰好调用一次**，实现侧必须自备幂等
	//     （同一释放函数可能因 body 被重复 Close 而重入）；
	//   - `limit` 是该渠道的并发会话上限（providers.limit_concurrent_sessions），
	//     0 表示不限；统计开启而上限为 0 时实现侧仍应登记（只统计不拒绝）；
	//   - 实现侧的任何错误必须 Fail Open（放行且不登记）——Redis 故障不得变成 429。
	//
	// 为什么缝在这里而不是守卫链的限流步骤：那一步（见 guard.go 的预设顺序）排在 provider 步
	// **之前**，此刻还不知道本次会落到哪一家；而并发额度是按供应商计的，只有拨号点才既知道
	// 渠道、又能在同一次原子调用里「判定 + 占名额」。这也是唯一能同时覆盖串行、重试、切换
	// 与竞速（hedge 的每个 attempt 都经本函数拨号）的位置。
	//
	// nil 表示未接线（或全局统计开关关闭）：整段跳过，**零 Redis 命令**。
	ProviderInFlight func(ctx context.Context, providerID int64, limit int) ProviderInFlightResult
	// Settle 对最终结果做一次终态入账；nil 时跳过（数据面未接线时使用）。
	//
	// 调用纪律：整次 Forward **只调用一次**，且只在产生最终结果的路径上（成功、或已放弃的
	// 失败）；中间失败不落库——否则重试会把同一条请求记多次账。
	// 返回值只记日志，不改变本函数的成功/失败结论：上游的响应已经拿到，不能因为入账失败
	// 就把它丢掉（Node 侧同样把结算错误降级为日志）。
	Settle func(ctx context.Context, pc *pctx.Context, result *Result, failure *Failure) error
	// Facts 是构造计划的会话级事实。
	Facts PlanFacts
	// RectifySwitches 提供六个整流器开关（system_settings 的 enable_*_rectifier）。
	//
	// nil 表示未接线：此时按 Node 默认处理（全开）——Node 一律 `settings.x ?? true`，
	// 读不到设置就当成关闭会让整流器在生产静默失效。
	RectifySwitches func(ctx context.Context) rectify.Switches
	// RectifierAudit 接收整流器审计条目（Node 的 addSpecialSetting + persistSpecialSettings）。
	//
	// 为什么用回调而不是把条目挂在计划上：审计必须在**建行后**的终态追加落库（见
	// store.DetailsPatch.SpecialSettingsAppend），而整流可能发生在最终尝试失败之前——挂计划
	// 只能覆盖成功路径，请求整体失败时条目会丢。交给接线层按请求收，成功与失败两条路都不丢。
	// nil 表示不落审计（整流照常生效）。
	RectifierAudit func(entry map[string]any)
	// PlaceholderThinkingSignature 是 CCH_THINKING_SIGNATURE_PLACEHOLDER 的开关值。
	//
	// 开启时在发往 ANTHROPIC 供应商前剥掉客户端回传的自家占位签名；关闭时整条链不参与，
	// 回退到被动整流器（上游 400 后删 thinking 块）。语义见 rectify.StripPlaceholderSignature。
	PlaceholderThinkingSignature bool
}

func (d Deps) logger() *logx.Logger {
	if d.Logger != nil {
		return d.Logger
	}
	return sharedLogger()
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d Deps) sleep(ctx context.Context, duration time.Duration) error {
	if d.Sleep != nil {
		return d.Sleep(ctx, duration)
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// AttemptOutcome 是一次尝试的留痕，供落 provider_chain。
//
// 字段刻意保持中立：把 AttemptOutcome 映射为落库结构（route.ChainItem）是接线层的职责，
// 本包不引入选路包的类型依赖。
type AttemptOutcome struct {
	ProviderID   int64
	ProviderName string
	EndpointID   int64
	EndpointURL  string
	Attempt      int
	StatusCode   int
	Category     Category
	// Reason 与 Node 的 provider_chain[].reason 同值。
	Reason  string
	Message string
	// DurationMS 是本次尝试的耗时。
	DurationMS int64
	// ProbeSlow 为真表示本次尝试因中途低速探测被主动判废（见 Failure.ProbeSlow）。
	ProbeSlow bool
	// ProbeElapsedMS 是自首字节起、直到判废的时长（毫秒）。
	ProbeElapsedMS int
	// StartedAt / FinishedAt 是本次尝试的**真实测量**起止时刻，供 routing_trace 用。
	//
	// 与 DurationMS 同源（同一对 now() 读数），但保留绝对时刻：`routing_trace` 的事件
	// 必须带真实时间戳，否则只能靠累加推算——那是编造时间线。零值表示本次尝试未记录
	// 时刻（例如 hedge 在途输家在胜者裁决时被批量落链），此时 trace 侧**不产出该事件**，
	// 而不是拿别的时刻顶替。
	StartedAt  time.Time
	FinishedAt time.Time
	// Redirected 为真表示本次尝试改写了模型名。
	Redirected bool
	// ModelRedirect 是本次尝试实际生效的模型重定向快照，对应 Node 链项上的
	// `modelRedirect`（由 `model-redirector.ts` 的 redirectInfo 产出，最终由
	// `addProviderToChain` 落到 `provider_chain[].modelRedirect`）。
	//
	// nil 表示本次尝试没有施加重定向——**链上就不写这个键**，与 Node 的
	// `modelRedirect: metadata?.modelRedirect ?? getCurrentModelRedirect(id)` 一致
	// （没规则时两者都是 undefined，JSON 序列化后该键不存在）。
	ModelRedirect *AttemptModelRedirect
	// SkippedRetryAndSwitch 为真表示该端点策略禁止重试与切换。
	SkippedRetryAndSwitch bool
	// WS 是本次尝试上的上游 WebSocket 事实；nil 表示本次尝试与上游 WS 无关
	// （客户端不是 WS / 供应商不是 codex / 开关关闭 / 端点命中不支持缓存）。
	WS *AttemptWSFacts
}

// AttemptWSFacts 是一次尝试上的上游 WebSocket 事实，逐字对应 Node 写在同一链项上的那组键
// （src/types/message.ts:294-321：clientTransport / upstreamWsAttempted / upstreamWsConnected /
// downgradedToHttp / downgradeReason）。
//
// 为什么这些事实必须落链而不是只写日志：它们是**跨语言数据契约**——仪表盘的链路弹窗按这些键
// 渲染「这次到底走没走上游 WS、为什么没走成」，只进日志就查不出「我的 key 为什么没吃到 WS」。
type AttemptWSFacts struct {
	// ClientTransport 恒为 websocket：只有该传输层才可能走上游 WS。
	ClientTransport string
	// Attempted 为真表示真的发起过握手。
	Attempted bool
	// Connected 为真表示握手成功且收到了首个事件。
	Connected bool
	// DowngradedToHTTP 为真表示本次回落到了 HTTP。
	DowngradedToHTTP bool
	// DowngradeReason 取 Node 的 downgradeReason 取值域（upws.Downgrade）。
	DowngradeReason string
}

// AttemptModelRedirect 是一次尝试上的模型重定向快照（链项字段的领域侧形式）。
//
// 为什么不直接引 plan 里的类型：落链只需要这几个字段，而 `Plan` 持有正文；
// 让留痕引用 Plan 会把请求正文的生存期拖到结算之后（破坏流式驻留有界这条不变量）。
type AttemptModelRedirect struct {
	// OriginalModel 是用户请求的模型名（Node 的 originalModel，也是计费依据）。
	OriginalModel string
	// RedirectedModel 是实际发往上游的模型名（Node 的 redirectedModel）。
	RedirectedModel string
	// BillingModel 是计费模型名。Node 恒等于 originalModel
	// （`model-redirector.ts`：`billingModel: originalModel`），Go 照写以保持逐字一致。
	BillingModel string
	// MatchType / Source / Target 是命中规则的详情（Node 的 matchedRule）。
	MatchType string
	Source    string
	Target    string
}

// attemptModelRedirect 把 plan 上的重定向转成留痕用的快照；无重定向时返回 nil。
func attemptModelRedirect(plan *Plan) *AttemptModelRedirect {
	if plan == nil || plan.Redirect == nil {
		return nil
	}
	redirect := plan.Redirect
	return &AttemptModelRedirect{
		OriginalModel:   redirect.Original,
		RedirectedModel: redirect.Target,
		// 与 Node 同一口径：计费名是**用户请求的模型**，不是转发目标。
		BillingModel: redirect.Original,
		MatchType:    redirect.Rule,
		Source:       redirect.Source,
		Target:       redirect.Target,
	}
}

// Result 是一次成功转发的产出。
type Result struct {
	Provider Provider
	Endpoint Endpoint
	Plan     *Plan

	StatusCode int
	Status     string
	Headers    http.Header
	// Body 是非流式正文。超过 Limits.MaxResponseBytes 的响应不会被接收（见下），
	// 故这里的正文恒为完整正文：残缺 JSON 绝不能流向下游。
	Body []byte

	Attempts                []AttemptOutcome
	TotalProvidersAttempted []int64
	// DetectorMissing 为真表示本次走了 200 响应但未做 fake-200 检测（Detector 为空）。
	DetectorMissing bool
	// Settled 为真表示已在 pctx 上完成一次性结算断言。
	Settled bool
	// DeferredToStream 为真表示本次转发的终态推迟到流终态处理（见 ForwardStream）。
	//
	// 为什么必须显式标出：流式尝试一旦提交，就**不能**再走非流式的结算缝——那条缝拿不到
	// 用量与 TTFT，会把行先钉成「无用量」的终态，而真正带用量的流终态结算随后会被
	// store 的终态谓词挡住（一行只允许一次终态写）。见 forwardLoop 的 defer。
	DeferredToStream bool

	StartedAt time.Time
	EndedAt   time.Time
}

// Duration 返回本次转发耗时。
func (r *Result) Duration() time.Duration {
	if r == nil {
		return 0
	}
	return r.EndedAt.Sub(r.StartedAt)
}

// Forward 执行一次非流式转发：外层按供应商切换，内层按供应商重试上限重试。
//
// 返回值语义：
//   - 成功：Result 非 nil，error 为 nil。
//   - 全部耗尽：Result 非 nil（含完整尝试留痕），error 是可 errors.As 成 *Failure 的最终失败。
//   - 尝试开始前即失败（无候选、计划构造失败）：Result 为 nil，error 说明原因。
//
// 结算纪律：无论尝试了多少次，本函数只在最终结果上调用一次 pctx.MarkSettled——
// 中间失败不做任何终态落库，否则重试会导致同一条请求被结算多次（重复计费）。
func Forward(ctx context.Context, pc *pctx.Context, initial *Candidate, deps Deps) (*Result, error) {
	return forwardLoop(ctx, pc, initial, deps, deps.executeAttempt,
		func(result *Result, response *attemptResponse) {
			result.Body = response.Body
			settleNonStream(pc, result, response.StatusCode)
		})
}

// settleNonStream 在最终结果上做一次即时结算。
//
// 非流式正文已经在手上，成功与否当场可知，所以结算就地完成；流式路径不走这里，
// 它的结算推迟到流终态（见 Stream.settleTerminal）。
func settleNonStream(pc *pctx.Context, result *Result, statusCode int) {
	if pc == nil {
		return
	}
	result.Settled = pc.MarkSettled(pctx.Settlement{
		StatusCode: statusCode,
		Success:    true,
		At:         result.EndedAt,
	})
}

// attemptExecutor 是「执行一次上游尝试」的策略。
//
// 非流式实现读完整正文（executeAttempt）；流式实现只做到「门控提交」为止，
// 把还活着的上游正文交回调用方（executeStreamAttempt，见 stream.go）。
// 两者共用同一份重试/切换决策，避免两条路径的失败语义漂移。
type attemptExecutor func(
	ctx context.Context,
	provider Provider,
	plan *Plan,
	outcome *AttemptOutcome,
) (*attemptResponse, *Failure)

// onAttemptSuccess 把某条路径特有的成功产物落到 Result 上（含该路径的结算纪律）。
type onAttemptSuccess func(result *Result, response *attemptResponse)

// forwardLoop 是流式与非流式共用的尝试循环。
//
// 公共字段（供应商、端点、计划、状态码、状态行、headers、结束时刻）由本函数填；
// 正文与结算由 onSuccess 按路径填：非流式即时结算，流式推迟到流的终态。
func forwardLoop(
	ctx context.Context,
	pc *pctx.Context,
	initial *Candidate,
	deps Deps,
	execute attemptExecutor,
	onSuccess onAttemptSuccess,
) (*Result, error) {
	if deps.Dial == nil {
		return nil, errors.New("forward: 未提供拨号器")
	}
	limits := deps.Limits.withDefaults()
	startedAt := deps.now()

	current := initial
	if current == nil {
		if deps.Select == nil {
			return nil, ErrNoProviderAvailable
		}
		selected, err := deps.Select(ctx, nil)
		if err != nil {
			return nil, err
		}
		if selected == nil {
			return nil, ErrNoProviderAvailable
		}
		current = selected
	}

	result := &Result{StartedAt: startedAt, DetectorMissing: deps.Detector == nil}
	failedProviders := make([]int64, 0, 4)
	var lastFailure *Failure
	// statefulRejection 记住首次「候选无法承载状态型字段」的拒绝。候选级跳过不留失败留痕，
	// 故池子耗尽时由它（而不是 lastFailure）决定客户端看到什么。
	var statefulRejection *StatefulConversionError
	// 整流器的可变副本：整流器改写的是**客户端正文快照**，而客户端正文是「每次尝试重新
	// BuildPlan」的输入，故改写必须回到这份副本上，下一次尝试才拿得到整流后的正文。
	rectifier := rectifierState{client: deps.Facts.Client, sink: deps.RectifierAudit}

	// 终态入账只在本函数退出时发生一次：三个提前返回点（客户端中断、禁重试切换、重试等待被
	// 取消）与两个穷尽点共用同一条路径，不会因为新增返回点而漏账或多账。
	// 没有任何尝试时（计划构造失败、选路失败且无候选）不入账：与上游从未接触，也就没有终态。
	defer func() {
		if deps.Settle == nil || len(result.Attempts) == 0 {
			return
		}
		if result.DeferredToStream {
			// 流已提交：终态（含用量、TTFT、断线归因）属流终态结算缝。
			return
		}
		if err := deps.Settle(ctx, pc, result, lastFailure); err != nil {
			deps.logger().Warn("forward.settle.failed", map[string]any{
				"status_code": result.StatusCode,
				"error":       err.Error(),
			})
		}
	}()

	// 零尝试的退出一律交回 nil Result。判据只有一条，所有错误退出都经它收口，不逐分支各写一份
	// （reviewer F1'）：调用方只在 result == nil 时把「候选不可服务」翻成方言化 400 **并结算**，
	// 而本函数的结算 defer 以 len(Attempts) > 0 为前提。于是「非 nil + 零 attempt」两头落空——
	// 既被译成通用 502（客户端据 502 会重试，而正解是改道），又没有任何终态，请求在账上凭空
	// 消失（故障转移选路失败那条分支曾正是如此）。
	exitWithError := func(err error) (*Result, error) {
		result.EndedAt = deps.now()
		if len(result.Attempts) == 0 {
			return nil, err
		}
		return result, err
	}

	// 候选扫描有三本独立账：
	//   - attempted 记**真正进入尝试的候选数**（计划构造成功），且每个候选只记一次：内层重试
	//     不是「换了一家供应商」（见 counted）。按重试次数计会让首家重试两次就吃光
	//     MaxProviderSwitches，排在后面的供应商永远轮不到。
	//   - TotalProvidersAttempted 记尝试过的供应商（同样每供应商一次，重试不重复写）。
	//   - 跳过只记计划阶段因状态型字段被拒的候选，它不占上面两本账中的任何一本——把跳过计进
	//     尝试额度，同线候选排在第 21 位时就永远轮不到它，可用性被白扔。
	// 终止性不靠额度兜底：每轮外层迭代要么消耗一次额度、要么把候选记进 seen 与排除集，两者
	// 都单调，故有界；seen 再兜一层「选路不守排除列表」的异常实现，避免退化成死循环。
	seen := make(map[int64]struct{}, 4)
	for attempted := 0; attempted < limits.MaxProviderSwitches; {
		if _, repeat := seen[current.Provider.ID]; repeat {
			deps.logger().Warn("forward: 选路返回了已排除的候选，按候选耗尽处理", map[string]any{
				"provider_id": current.Provider.ID,
			})
			break
		}
		seen[current.Provider.ID] = struct{}{}
		// counted 守卫「每候选一次」：候选的首个计划构造成功即计额度，之后的内层重试不再计
		// （reviewer F2'：原先把自增写在重试循环里，首家重试两次就把额度用光）。
		counted := false

		skipRetryAndSwitch := current.RawPassthrough && !current.RawCrossProviderFallback
		maxAttempts := ResolveMaxAttempts(current.Provider, limits)
		if skipRetryAndSwitch {
			maxAttempts = 1
		}

		// 整流器：幂等标记按供应商轮次重置（Node forwarder.ts:1766-1772），
		// 并在发送前执行主动型 billing header 剥离（只对 ANTHROPIC 供应商，
		// Node forwarder.ts:3463-3500）。幂等只约束「被动型同供应商重试一次」，
		// 原始透传的「不重试、不切换」不受影响：它的整流器仅在失败分支里才被考虑，
		// 而失败分支对原始透传短路（见下）。
		rectifier.resetForProvider()
		rectifier.applyBillingHeaderRectifier(current.Provider, deps.rectifierSwitches(ctx), deps.logger())
		rectifier.applyPlaceholderSignatureStrip(current.Provider, deps.PlaceholderThinkingSignature, deps.logger())

		endpoints := current.endpoints()
		endpointIndex := 0

		for attempt := 1; attempt <= maxAttempts; attempt++ {
			if pc != nil {
				pc.SetProvider(pctx.ProviderSelection{
					ProviderID: current.Provider.ID,
					Name:       current.Provider.Name,
					Type:       string(current.Provider.Type),
				})
			}

			endpoint := Endpoint{}
			if len(endpoints) > 0 {
				index := endpointIndex
				if index > len(endpoints)-1 {
					index = len(endpoints) - 1
				}
				endpoint = endpoints[index]
			}

			attemptStartedAt := deps.now()
			plan, err := BuildPlan(PlanInput{
				Client:            rectifier.client,
				Target:            Target{Provider: current.Provider, Endpoint: endpoint},
				ConversionEnabled: current.ConversionEnabled,
				Overrides:         deps.Facts.Overrides,
				CacheTTL1h:        deps.Facts.CacheTTL1h,
				ClientUserAgent:   deps.Facts.ClientUserAgent,
				FilteredUserAgent: deps.Facts.FilteredUserAgent,
				UserAgentModified: deps.Facts.UserAgentModified,
			})
			if err != nil {
				// 候选级不可服务：该候选会转正文，而客户端正文里的状态型字段在目标线没有承载位。
				// 这不是上游故障（根本没接触上游）、也不靠重试同一供应商改善，故把它记入排除集、
				// 换下一个候选——池中有能承载的原生同线供应商时请求照常成功；池子耗尽才由下面的
				// statefulRejection 分支给出 400 与字段名。
				var rejected *StatefulConversionError
				if errors.As(err, &rejected) {
					if statefulRejection == nil {
						statefulRejection = rejected
					}
					failedProviders = append(failedProviders, current.Provider.ID)
					deps.logger().Warn("forward: 候选无法承载状态型字段，跳过", map[string]any{
						"provider_id":   current.Provider.ID,
						"provider_type": string(current.Provider.Type),
						"field":         rejected.Field,
						"effect":        "candidate_skipped",
					})
					// 跳出重试循环换候选：同一供应商的正文相同，重试拿到的还是同一条拒绝。
					break
				}
				// 计划构造失败属本地配置/请求问题：重试同一供应商或换供应商都不会变好。但若此前
				// 已有真实尝试，那些尝试仍须交回调用方——零 attempt 才归 nil。
				return exitWithError(err)
			}

			// 计划构造成功＝这个候选真的进入尝试：此刻才计尝试额度与留痕，而且**每个候选只计
			// 一次**——内层重试不是「换了一家供应商」。被跳过的候选两者都不占（它从未拨号，把
			// 它记成「尝试过的供应商」是假账）。
			if !counted {
				counted = true
				attempted++
				result.TotalProvidersAttempted = append(result.TotalProvidersAttempted, current.Provider.ID)
			}

			outcome := AttemptOutcome{
				ProviderID:            current.Provider.ID,
				ProviderName:          current.Provider.Name,
				EndpointID:            endpoint.ID,
				EndpointURL:           plan.URL,
				Attempt:               attempt,
				Redirected:            plan.Redirect != nil,
				ModelRedirect:         attemptModelRedirect(plan),
				SkippedRetryAndSwitch: skipRetryAndSwitch,
			}

			response, failure := execute(ctx, current.Provider, plan, &outcome)
			if failure == nil {
				// 成功记账：与失败记账同层，保证「开闸之后一定有机会归闭」。
				if deps.RecordSuccess != nil {
					deps.RecordSuccess(ctx, current.Provider.ID, endpoint.ID)
				}
				outcome.Reason = ReasonRequestSuccess
				finishedAt := deps.now()
				outcome.StatusCode = response.StatusCode
				outcome.DurationMS = finishedAt.Sub(attemptStartedAt).Milliseconds()
				outcome.StartedAt = attemptStartedAt
				outcome.FinishedAt = finishedAt
				result.Attempts = append(result.Attempts, outcome)
				result.Provider = current.Provider
				result.Endpoint = endpoint
				result.Plan = plan
				result.StatusCode = response.StatusCode
				result.Status = response.Status
				result.Headers = response.Header
				result.EndedAt = deps.now()
				// 路径特有的产物与结算纪律（非流式即时结算，流式推迟到流的终态）。
				onSuccess(result, response)
				return result, nil
			}

			lastFailure = failure
			finishedAt := deps.now()
			outcome.StatusCode = failure.StatusCode
			outcome.Category = failure.Category
			outcome.Message = failure.Message
			outcome.Reason = reasonForCategory(failure.Category, failure.StatusCode)
			outcome.DurationMS = finishedAt.Sub(attemptStartedAt).Milliseconds()
			outcome.StartedAt = attemptStartedAt
			outcome.FinishedAt = finishedAt
			outcome.ProbeSlow = failure.ProbeSlow
			outcome.ProbeElapsedMS = failure.ProbeElapsedMS
			result.Attempts = append(result.Attempts, outcome)
			deps.logger().Warn("forward: 尝试失败", map[string]any{
				"provider_id":   failure.ProviderID,
				"endpoint_id":   failure.EndpointID,
				"endpoint_url":  failure.EndpointURL,
				"attempt":       failure.Attempt,
				"status_code":   failure.StatusCode,
				"category":      failure.Category.String(),
				"provider_type": string(current.Provider.Type),
			})

			// 2.5 被动整流：上游报错的文案命中整流器时，整流客户端正文并对**同一供应商**再试一次
			// （Node forwarder.ts:2653-2697 的 2.5 段）。
			//
			// 位置有讲究：必须在下面的分类早退**之前**——上游 400 在 Go 侧的归类是不可重试的
			// 客户端错误，而它正是整流器要抢救的那一类（Node 同样在分类判定之前调用）。
			// 整流会改写 rectifier.client，下一次 BuildPlan 用的就是整流后的正文。
			//
			// 原始透传短路：它的契约是「不重试、不切换」，整流器的同供应商重试与之相冲，
			// 故对 skipRetryAndSwitch 不做被动整流（Node 侧无此分支，属有意的 Go 侧限制）。
			if !skipRetryAndSwitch {
				rectified := deps.applyReactiveRectifier(ctx, current.Provider, failure, &rectifier, attempt)
				if rectified.Matched && !rectified.Applied {
					// already_retried / not_applicable：Node 归为不可重试的客户端错误并直接终止
					// （不重试、不切换、不计入熔断器）。
					failure.Category = CategoryNonRetryableClientError
					return exitWithError(failure)
				}
				if rectified.Applied {
					// 整流成立：确保即使重试上限为 1 也能完成这次额外重试（Node 的
					// `maxAttemptsPerProvider = max(..., attemptCount + 1)`），且不等待重试间隔
					// （Node 同样是立即 continue）。
					if attempt >= maxAttempts {
						maxAttempts = attempt + 1
					}
					continue
				}
			}

			// 客户端中断、客户端输入错误与本地过载：不重试、不切换，立即终止。
			if !failure.Category.RetriesSameProvider() && !failure.Category.SwitchesProvider() {
				return exitWithError(failure)
			}

			if skipRetryAndSwitch {
				return exitWithError(failure)
			}

			// 网络错误与上游超时（524）推进端点索引：这两种失败往往与具体端点相关。
			if failure.Category == CategorySystemError || failure.StatusCode == statusUpstreamTimeout {
				endpointIndex++
			}

			// 同家重试只对「重试可能改变结果」的分类成立：CategoryProviderUnsupportedInput
			// 声明的是「同一份输入再送也一样被拒」，重试只是把同一份反复送上去，故跳过重试、
			// 直接走下面的换家分支（Category.RetriesSameProvider 是这一判断的唯一真源）。
			if failure.Category.RetriesSameProvider() && attempt < maxAttempts {
				if err := deps.sleep(ctx, limits.RetryDelay); err != nil {
					return exitWithError(&Failure{
						Category:     CategoryClientAbort,
						Message:      "等待重试期间请求被取消",
						ProviderID:   current.Provider.ID,
						ProviderName: current.Provider.Name,
						EndpointID:   endpoint.ID,
						EndpointURL:  plan.URL,
						Attempt:      attempt,
						Err:          err,
					})
				}
				continue
			}

			// 重试耗尽：按分类决定是否计入熔断器，然后切换供应商。
			// 请求作用域失败（由请求内容决定，见 Failure.RequestScoped）不计健康度。
			if !failure.RequestScoped &&
				(failure.Category.CountsTowardCircuit() || (failure.Category == CategorySystemError && deps.CountNetworkFailureTowardCircuit)) {
				if deps.RecordFailure != nil {
					deps.RecordFailure(ctx, failure)
				}
			}
			// 内层循环只可能因重试耗尽而走到这里：其余分类都在循环内直接返回。
			failedProviders = append(failedProviders, current.Provider.ID)
			break
		}

		if deps.Select == nil {
			break
		}
		next, err := deps.Select(ctx, failedProviders)
		if err != nil {
			// 变更前的缺陷：这里返回非 nil、零 Attempts 的 result，于是「翻 400」与「结算」
			// 两条路都走不到（见上面 exitWithError 的注释）。
			return exitWithError(err)
		}
		if next == nil {
			break
		}
		current = next
	}

	if lastFailure == nil {
		// 没有任何一次尝试真正开始（候选都在计划阶段就被拒），故「尝试开始前即失败」这一档
		// 必须让调用方自己结算——判据统一在 exitWithError，此处只说为什么这一档恒为空尝试：
		// 尝试只会在成功或失败时入账，而成功当场返回，故 lastFailure == nil ⟺ Attempts 为空。
		//
		// 全池候选都因状态型字段不可服务与「供应商都失败」必须分开：前者要客户端改道，
		// 后者只需重试。
		if statefulRejection != nil {
			return exitWithError(statefulRejection)
		}
		return exitWithError(fmt.Errorf("%w: provider#%d", ErrProvidersExhausted, initial.Provider.ID))
	}
	return exitWithError(fmt.Errorf("%w: %w", ErrProvidersExhausted, lastFailure))
}

// statusUpstreamTimeout 是上游超时的合成状态码（524 = A Timeout Occurred）。
const statusUpstreamTimeout = 524

// reasonForCategory 把失败分类映射为 provider_chain 的 reason，取值与 Node 一致。
func reasonForCategory(category Category, statusCode int) string {
	switch category {
	case CategoryProviderError:
		if statusCode == statusUpstreamTimeout {
			return ReasonVendorTypeAllTimeout
		}
		return ReasonRetryFailed
	case CategorySystemError:
		return ReasonSystemError
	case CategoryClientAbort:
		return ReasonClientAbort
	case CategoryNonRetryableClientError:
		return ReasonClientErrorNonRetryable
	case CategoryResourceNotFound:
		return ReasonResourceNotFound
	case CategoryProviderUnsupportedInput:
		return ReasonUnsupported
	case CategoryProviderSaturated:
		return ReasonConcurrentLimitFailed
	case CategoryLocalOverload:
		return ReasonLocalOverload
	default:
		return ReasonRetryFailed
	}
}

// attemptResponse 是 executeAttempt 的成功产出。
type attemptResponse struct {
	StatusCode int
	Status     string
	Header     http.Header
	Body       []byte
	// Stream 非空表示这是流式路径的成功产物：上游响应还活着，正文所有权已交给调用方。
	// 此时 Body 必为空——正文不再完整驻留。
	Stream *streamAttempt
}

// dialAttempt 发起一次上游调用并返回仍活着的响应。
//
// streaming 为真时不施加非流式总超时：流式请求的边界由首字节/静默超时与客户端生命周期
// 决定，一个总时限会把长回答腰斩（Node 侧 provider.requestTimeout 同样只作用于非流式）。
// 返回的 cancel 非 nil 时必须由调用方调用，它是非流式总超时的取消函数。
//
// 本函数是**唯一的上游拨号口**（串行流式、串行非流式、竞速的每个 attempt 都经此），
// 故供应商并发名额的登记与释放都缝在这里：先占名额再拨号，名额随 body 关闭归还。
func (d Deps) dialAttempt(
	ctx context.Context,
	plan *Plan,
	outcome *AttemptOutcome,
	streaming bool,
) (*dial.Response, context.CancelFunc, *Failure) {
	attemptCtx := ctx
	cancel := context.CancelFunc(func() {})
	if !streaming && plan.RequestTimeout > 0 {
		attemptCtx, cancel = context.WithTimeout(ctx, plan.RequestTimeout)
	}

	// 先占名额再拨号（与 Node 同序）：只有先占，上游失败后的回退决策才是原子的；
	// 反过来靠 TTL 兜底，会在渠道故障时瞬间堆满 active_sessions。
	var release func()
	if d.ProviderInFlight != nil {
		acquired := d.ProviderInFlight(attemptCtx, outcome.ProviderID, plan.LimitConcurrentSessions)
		if !acquired.Allowed {
			cancel()
			return nil, func() {}, d.saturatedFailure(plan, outcome, acquired.Current)
		}
		release = acquired.Release
	}

	response, err := d.Dial.RoundTrip(attemptCtx, plan.Request())
	if err != nil {
		// 拨号失败没有 body 可挂：名额必须**就地**还回去，否则这条渠道的名额会被永久占住
		// （只能等 TTL 过期）。这是本任务里最容易漏的一条泄漏路径。
		if release != nil {
			release()
		}
		cancel()
		return nil, func() {}, d.transportFailure(err, plan, outcome, attemptCtx)
	}
	if release != nil {
		// 释放挂在 body 上：正文的所有权在调用方手里（流式路径会持有到客户端读完），
		// 而关闭是**所有**结局（读完、读错、客户端中断、静默超时、竞速输家被取消）
		// 唯一都会经过的动作。
		response.Body = &providerInFlightBody{ReadCloser: response.Body, release: release}
	}
	return response, cancel, nil
}

// providerInFlightBody 在正文关闭时归还供应商并发名额。
//
// 为何用 sync.Once：多条路径都会 Close（正常读完、错误分支的 defer、竞速裁决时的强制关闭），
// 而释放必须恰好一次——重复释放会把别的在飞尝试的名额提前抹掉（引用计数被多减）。
type providerInFlightBody struct {
	io.ReadCloser
	release func()
	once    sync.Once
}

func (b *providerInFlightBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}

// saturatedFailure 是「该渠道并发额度已满、本次尝试未发出」的归因。
//
// Internal 为真：没有上游参与，状态码留 0（本进程按名额读数自答）。
// ConcurrencyCurrent / ConcurrencyLimit 供终态把「全部候选都满员」翻成 429 信封。
func (d Deps) saturatedFailure(plan *Plan, outcome *AttemptOutcome, current int) *Failure {
	target := outcome.ProviderName
	if target == "" {
		target = fmt.Sprintf("provider#%d", outcome.ProviderID)
	}
	return &Failure{
		Category:           CategoryProviderSaturated,
		Internal:           true,
		ProviderID:         outcome.ProviderID,
		ProviderName:       outcome.ProviderName,
		EndpointID:         outcome.EndpointID,
		EndpointURL:        plan.URL,
		Attempt:            outcome.Attempt,
		Message:            fmt.Sprintf("%s 的并发会话数已达上限 %d", target, plan.LimitConcurrentSessions),
		ConcurrencyCurrent: current,
		ConcurrencyLimit:   plan.LimitConcurrentSessions,
	}
}

// executeAttempt 发起一次上游调用并读回正文；返回 (成功响应, 失败归因)，两者恰有一个非 nil。
//
// 正文只驻留一份：读到的是上游未经解压的原始字节（拨号层强制 identity 编码），
// 本函数不做任何二次复制，也不为日志额外保留副本。
func (d Deps) executeAttempt(
	ctx context.Context,
	_ Provider,
	plan *Plan,
	outcome *AttemptOutcome,
) (*attemptResponse, *Failure) {
	response, cancel, failure := d.dialAttempt(ctx, plan, outcome, false)
	if failure != nil {
		return nil, failure
	}
	defer cancel()
	defer func() {
		_ = response.Body.Close()
	}()
	return d.completeResponse(response, plan, outcome)
}

// completeResponse 把一个已拿到响应头的上游响应按非流式语义处理完：
// 读完整正文（有硬上限）、空响应判定、fake-200 检测、非 2xx 分类。
//
// 流式路径在判定「本次响应不是流」时复用它，两条路径共享同一份状态码/错误规则口径。
func (d Deps) completeResponse(
	response *dial.Response,
	plan *Plan,
	outcome *AttemptOutcome,
) (*attemptResponse, *Failure) {
	limits := d.Limits.withDefaults()

	body, oversized, readErr := readAllBounded(response.Body, limits.MaxResponseBytes)
	if readErr != nil {
		if errors.Is(readErr, context.DeadlineExceeded) {
			return nil, &Failure{
				Category:     CategoryProviderError,
				StatusCode:   statusUpstreamTimeout,
				Message:      timeoutFailureMessage(plan.RequestTimeout, false),
				Body:         timeoutFailureBody(plan.RequestTimeout, false),
				ProviderID:   outcome.ProviderID,
				ProviderName: outcome.ProviderName,
				EndpointID:   outcome.EndpointID,
				EndpointURL:  plan.URL,
				Attempt:      outcome.Attempt,
				Err:          readErr,
			}
		}
		return nil, &Failure{
			Category:     CategorySystemError,
			StatusCode:   response.StatusCode,
			Message:      fmt.Sprintf("读取上游正文失败: %v", readErr),
			ProviderID:   outcome.ProviderID,
			ProviderName: outcome.ProviderName,
			EndpointID:   outcome.EndpointID,
			EndpointURL:  plan.URL,
			Attempt:      outcome.Attempt,
			Err:          readErr,
		}
	}

	// 正文超过上限：整次尝试判为失败。
	//
	// Node 侧无上限，这里刻意收紧——超限时截断后当成功返回会把残缺 JSON 交给下游，
	// 那是静默的数据损坏；宁可换成「换一个供应商再试」。
	if oversized {
		return nil, &Failure{
			Category:     CategoryProviderError,
			StatusCode:   response.StatusCode,
			Message:      fmt.Sprintf("上游响应正文超过上限 %d 字节", limits.MaxResponseBytes),
			ProviderID:   outcome.ProviderID,
			ProviderName: outcome.ProviderName,
			EndpointID:   outcome.EndpointID,
			EndpointURL:  plan.URL,
			Attempt:      outcome.Attempt,
		}
	}

	// 上游声明零长度正文：Node 视为空响应错误（供应商故障），不进入成功分支。
	if response.StatusCode >= 200 && response.StatusCode < 300 && isEmptyBody(response.Header, body) {
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

	if response.StatusCode >= 200 && response.StatusCode < 300 {
		if d.Detector != nil {
			if detected, message, ok := d.Detector.Detect(string(plan.Protocol), false, string(body)); ok {
				truncatedBody, _ := truncateBody(body, limits.MaxErrorBodyBytes)
				return nil, &Failure{
					Category:     classifyForStatus(detected, true, d.Rules, truncatedBody),
					StatusCode:   detected,
					Message:      message,
					Body:         truncatedBody,
					Synthetic:    true,
					ProviderID:   outcome.ProviderID,
					ProviderName: outcome.ProviderName,
					EndpointID:   outcome.EndpointID,
					EndpointURL:  plan.URL,
					Attempt:      outcome.Attempt,
				}
			}
		}
		return &attemptResponse{
			StatusCode: response.StatusCode,
			Status:     response.Status,
			Header:     response.Header,
			Body:       body,
		}, nil
	}

	errorBody, _ := truncateBody(body, limits.MaxErrorBodyBytes)
	message := messageFromErrorBody(errorBody)
	if message == "" {
		message = fmt.Sprintf("Provider returned %d: %s", response.StatusCode, response.Status)
	}
	return nil, &Failure{
		Category:     classifyForStatus(response.StatusCode, false, d.Rules, errorBody),
		StatusCode:   response.StatusCode,
		Message:      message,
		Body:         errorBody,
		ProviderID:   outcome.ProviderID,
		ProviderName: outcome.ProviderName,
		EndpointID:   outcome.EndpointID,
		EndpointURL:  plan.URL,
		Attempt:      outcome.Attempt,
	}
}

// transportFailure 把拨号层错误归因。
//
// 两级超时在这里分开：本包自己的非流式总超时（plan.RequestTimeout）对应 Node 的
// ProxyError(524)，分类为供应商故障；拨号层的 headers/body 空闲超时是传输层限制，
// 对应 Node 的 undici 超时（fetch failed），分类为系统错误。两者不可混为一谈，
// 否则熔断归因与端点推进都会漂移。
func (d Deps) transportFailure(err error, plan *Plan, outcome *AttemptOutcome, attemptCtx context.Context) *Failure {
	timedOut := errors.Is(attemptCtx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded)

	failure := &Failure{
		Category:     Classify(ClassifyInput{Err: err, Rules: d.Rules}),
		Message:      err.Error(),
		ProviderID:   outcome.ProviderID,
		ProviderName: outcome.ProviderName,
		EndpointID:   outcome.EndpointID,
		EndpointURL:  plan.URL,
		Attempt:      outcome.Attempt,
		Err:          err,
	}
	if timedOut {
		failure.Category = CategoryProviderError
		failure.StatusCode = statusUpstreamTimeout
		failure.Message = timeoutFailureMessage(plan.RequestTimeout, false)
		failure.Body = timeoutFailureBody(plan.RequestTimeout, false)
	}
	if errors.Is(err, dial.ErrUnsupportedUpstreamTransport) || errors.Is(err, dial.ErrRequestBuild) {
		failure.Internal = true
	}
	return failure
}

// classifyForStatus 按状态码与错误规则分类，供非流式路径复用。
func classifyForStatus(statusCode int, synthetic bool, rules RuleMatcher, body string) Category {
	return Classify(ClassifyInput{
		StatusCode: statusCode,
		Synthetic:  synthetic,
		Body:       body,
		Rules:      rules,
	})
}

// isEmptyBody 复刻 Node 的空响应判定：Content-Length 为 0，或读完为空且非分块。
func isEmptyBody(header http.Header, body []byte) bool {
	if len(body) > 0 {
		return false
	}
	contentLength := header.Get("content-length")
	if contentLength != "" && contentLength != "0" {
		// 声明了长度却读不到内容：属截断，交给上层按读取失败处理。
		return false
	}
	return true
}
