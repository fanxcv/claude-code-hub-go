package dataplane

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/fanxcv/claude-code-hub-go/go/internal/terminal"
)

// 本文件把流式竞速（forward.ForwardStreamHedge）接进数据面。
//
// 只管两件事：**何时开竞速**（条件判定）与**谁把输家成本写回既有请求行**（计费接缝）。
// 竞速本身（首字节阈值计时、胜者仲裁、有界引流、留痕原因）全在 forward/hedge.go。
//
// 为什么之前是死码：竞速的前置条件分散在端点策略（guard.Policy）、系统设置
// （discovery_enabled / legacy_hedge_max_in_flight / bill_hedge_losers）与选中供应商
// （first_byte_timeout_streaming_ms）三处，任一处无来源就只能猜。本文件把三处接上。

// HedgeWiring 是竞速的装配缝隙。
//
// 零值表示「本进程未接线竞速」：判定一律返回不开，请求走串行路径。竞速是优化而非正确性
// 依赖，因此由装配层显式打开（Enabled），不靠「字段非空即开」的隐式约定。
type HedgeWiring struct {
	// Enabled 是竞速的总开关。
	Enabled bool
	// Costs 是取价与计费口径；与胜者共用同一份解析器，两边不会分叉。
	Costs *costResolver
	// Pools 是输家成本的落点（store.AddHedgeLoserCost 的幂等累加）。
	// nil 表示不计输家成本（与 bill_hedge_losers 关闭同效）。
	Pools hedgeLoserWriter
	// LoserDrainTimeout 是单个输家后台引流的绝对上限；0 取 forward 出厂默认（120s）。
	// 生产装配取 HEDGE_LOSER_DRAIN_TIMEOUT_MS（config 已做 >= 1000 的钳制）。
	LoserDrainTimeout time.Duration
	// LoserMaxDrainBytes 是单个输家引流的字节上限；0 取 forward 出厂默认（64 MiB）。
	LoserMaxDrainBytes int64
	// LeaseSettler 是租约结算面（terminal.LeaseSettler）：每条输家成本入库后按自己的
	// 增量与标记结算一次（见 hedgeLoserBiller.settleLoserLease 与 terminal/lease_settle.go）。
	// nil 表示未装配：输家成本的租约扣减整段跳过，行为与接线前一致。
	LeaseSettler terminal.LeaseSettler
	// Logger 为空时写 stderr。
	Logger *logx.Logger
}

// hedgeLoserWriter 是输家成本的落点缝。
//
// 用接口而不是直接持有 *store.Pools：本层的判定（何时开竞速、拿哪些倍率与分组、什么算
// 「可计费」）全是纯判定，必须能不开数据库就断言；*store.Pools 恰好满足该接口。
type hedgeLoserWriter interface {
	AddHedgeLoserCost(ctx context.Context, id int64, deltaCost string, entry store.HedgeLoserEntry) error
}

// hedgeMinMaxInFlight / hedgeMaxMaxInFlight 是竞速并发上限的钳制区间，
// 对齐 Node 的 LEGACY_STREAMING_HEDGE_MIN/MAX_MAX_IN_FLIGHT（forwarder.ts:180-181）。
const (
	hedgeMinMaxInFlight = 1
	hedgeMaxMaxInFlight = 4
)

// clampLegacyHedgeMaxInFlight 复刻 clampLegacyHedgeMaxInFlight（forwarder.ts:184-191）：
// 非法值取默认 2，其余向下取整后钳到 [1,4]。
//
// 为什么还要夹一次：库里本就有 `>= 1 AND <= 4` 的 CHECK（schema.ts:1088），但设置快照
// 可能来自缓存或测试注入，越界的并发上限会直接把上游压垮，代价不对等。
// 0 在 Node 侧对应 undefined（取默认 2）：Go 的整型零值即「未提供」，故与之一致。
func clampLegacyHedgeMaxInFlight(value int) int {
	if value <= 0 {
		return forward.DefaultHedgeMaxInFlight
	}
	if value < hedgeMinMaxInFlight {
		return hedgeMinMaxInFlight
	}
	if value > hedgeMaxMaxInFlight {
		return hedgeMaxMaxInFlight
	}
	return value
}

// forwardStream 按竞速条件二选一：开启走 ForwardStreamHedge，否则走 ForwardStream。
func (h *Handler) forwardStream(
	ctx context.Context,
	pc *pctx.Context,
	candidate *forward.Candidate,
	deps forward.Deps,
	options forward.StreamOptions,
	spec routeSpec,
	state *RequestState,
) (*forward.StreamResult, error) {
	hedge, enabled := h.hedgeDecision(ctx, spec, deps, candidate, state)
	if !enabled {
		return forward.ForwardStream(ctx, pc, candidate, deps, options)
	}
	h.logger.Debug("dataplane.hedge_enabled", map[string]any{
		"maxInFlight": hedge.MaxInFlight,
		"billLosers":  hedge.BillLosers,
	})
	return forward.ForwardStreamHedge(ctx, pc, candidate, deps, options, hedge)
}

// hedgeDecision 判定本次请求是否走竞速，并给出竞速选项。
//
// 逐条对齐 Node 的 shouldUseStreamingHedge（forwarder.ts:4910-4921）：
//
//  1. 端点策略允许重试与供应商切换（endpoint-policy.ts:24-27 / 39-42）；
//  2. 客户端正文里 stream === true；
//  3. 选中供应商的首字节阈值 > 0（阈值为 0 表示这家压根不参与竞速）；
//  4. 未处于「绑定/租约冲突」的降级态。
//
// 第 4 条在 Node 只由 Discovery 预备阶段置位（forwarder.ts:4971/4995 的
// disableStreamingHedge），而本层未移植 Discovery，故等价地以「discovery 开启即不开竞速」
// 表达：Node 在 discovery 可用时走 Discovery 竞速、只在预备失败时才回落 legacy hedge。
//
// 与 Node 的已知差异（有意，保守侧）：discovery_enabled=true 时 Go 一律走串行——少一层
// 竞速优化，响应语义不变。等 Discovery 移植时一并收口（见 README 的未实现清单）。
//
// 设置读不到时不开竞速并留一条 warn：并发上限与输家计费开关都在设置里，猜一个值等于
// 用错误的并发压上游。Node 在更早的步骤（读设置失败即整条请求失败）不会走到这里，
// 故这不构成语义分叉。
func (h *Handler) hedgeDecision(
	ctx context.Context,
	spec routeSpec,
	deps forward.Deps,
	candidate *forward.Candidate,
	state *RequestState,
) (forward.HedgeOptions, bool) {
	wiring := h.options.Hedge
	if !wiring.Enabled {
		return forward.HedgeOptions{}, false
	}
	if !guard.EndpointAllowsRetryAndSwitch(spec.Policy.Preset) {
		return forward.HedgeOptions{}, false
	}
	if candidate == nil || candidate.Provider.FirstByteTimeoutStreamingMS <= 0 {
		return forward.HedgeOptions{}, false
	}
	if !forward.ClientStreamRequestedFor(
		deps.Facts.Client.Path,
		deps.Facts.Client.Query,
		deps.Facts.Client.Body,
	) {
		return forward.HedgeOptions{}, false
	}
	if h.options.Base.Settings == nil {
		return forward.HedgeOptions{}, false
	}
	settings, err := h.options.Base.Settings.FindSystemSettings(ctx)
	if err != nil || settings == nil {
		h.logger.Warn("dataplane.hedge_settings_failed", map[string]any{"error": errorText(err)})
		return forward.HedgeOptions{}, false
	}
	if settings.DiscoveryEnabled {
		return forward.HedgeOptions{}, false
	}
	return forward.HedgeOptions{
		MaxInFlight:        clampLegacyHedgeMaxInFlight(settings.LegacyHedgeMaxInFlight),
		BillLosers:         settings.BillHedgeLosers,
		LoserBiller:        h.loserBiller(wiring, state),
		LoserDrainTimeout:  wiring.LoserDrainTimeout,
		LoserMaxDrainBytes: wiring.LoserMaxDrainBytes,
		// 竞速饱和这类事实链词表装不下（写链会被公开状态分类器判成失败），
		// 交给本请求的 trace 收集器，落 routing_trace（Node 的同一位置）。
		TraceSink: state.trace,
	}, true
}

// loserBiller 造本次请求的输家计费接缝；计费缝不齐时返回 nil（等价于不计输家成本）。
//
// 判据与 forward 的 billable() 一致：开关、请求行、计费接缝三者缺一即不计费。
func (h *Handler) loserBiller(wiring HedgeWiring, state *RequestState) forward.HedgeLoserBiller {
	if state == nil || wiring.Costs == nil || wiring.Pools == nil {
		return nil
	}
	return &hedgeLoserBiller{wiring: wiring, state: state, logger: h.logger}
}

// hedgeLoserBiller 把输家引流拿回的用量按与胜者相同的口径计费，累加到既有请求行。
//
// 幂等性不靠本层：store.AddHedgeLoserCost 的
// `NOT (hedge_losers @> [{providerId, attemptNumber}])` 谓词保证同一输家只计一次，
// 即使本方法被重试（对齐 Node 的 addMessageRequestHedgeLoserCost）。
type hedgeLoserBiller struct {
	wiring HedgeWiring
	state  *RequestState
	logger *logx.Logger
}

// BillLoser 计一次输家成本。
//
// 三条「不计费」的判据与 Node 一致：
//   - 拿不到真实用量（截断且没解析到 usage）：Node 明确拒绝把 input_cost_per_request 的
//     {0,0} 哨兵当成真实用量（response-handler.ts:6937-6941），否则会给半截流多收一次
//     「按次费用」；
//   - 成本不大于 0（价格为 0 的模型不该在 hedge_losers 里留一条无意义的记录）；
//   - 没有请求行 id（不记账的请求，例如回放命中）。
//
// 状态码门槛不在这里另立一套：与胜者同路，由终态层按 bill_non_successful_requests 处理。
func (b *hedgeLoserBiller) BillLoser(ctx context.Context, bill forward.HedgeLoserBill) error {
	if b == nil || b.wiring.Costs == nil || b.wiring.Pools == nil || bill.RequestID == 0 {
		return nil
	}
	usage := usageFromConvert(bill.Usage)
	if usageEmpty(usage) {
		return nil
	}
	// 输家的倍率与分组标签取**它自己**那次选路的留痕：计费基准是实际服务它的那家。
	capture, captured := b.state.selectionFor(bill.ProviderID)
	multiplier := (*float64)(nil)
	groupTag := ""
	if captured {
		multiplier = costMultiplierOf(capture)
		if capture.Provider != nil && capture.Provider.GroupTag != nil {
			groupTag = *capture.Provider.GroupTag
		}
	}
	// 重定向目标取输家流内最后声明的模型名：重定向就是把模型名改写成目标名，故流内声明
	// 与计划里的 Redirect.Target 在实际上游响应里同值。流里没声明时回落请求模型名，
	// 此时两个取价基准一致，与胜者路径的「无重定向」同路。
	redirected := b.state.Model
	if bill.Model != "" {
		redirected = bill.Model
	}
	cost := b.wiring.Costs.resolve(ctx, costInput{
		RequestedModel:     b.state.Model,
		RedirectedModel:    redirected,
		Usage:              usage,
		ProviderMultiplier: multiplier,
		ProviderGroupTag:   groupTag,
		UserGroup:          b.state.UserGroup,
	})
	if cost == nil || !hedgeCostPositive(cost.Total) {
		return nil
	}
	err := b.wiring.Pools.AddHedgeLoserCost(ctx, bill.RequestID, cost.Total, hedgeLoserEntry(bill, usage))
	if err != nil {
		return err
	}
	b.settleLoserLease(ctx, bill, cost.Total)
	if b.logger != nil {
		b.logger.Info("dataplane.hedge_loser_billed", map[string]any{
			"requestId":    bill.RequestID,
			"providerId":   bill.ProviderID,
			"attempt":      bill.Sequence,
			"costUsd":      cost.Total,
			"drainedFully": bill.DrainComplete,
		})
	}
	return nil
}

// settleLoserLease 把这条输家成本增量结算到判定时用过的切片上。
//
// 为什么输家要单独结算而不是等胜者那一笔：store.UpdateWinnerCost 只把 winner 自己的成本
// 写入（已入库的输家另由子查询求和加入），而输家引流在后台完成（forward 的 billLoser，
// fire-and-forget）——胜者结算与输家写入的先后顺序不确定：输家先入库时它已被胜者那一笔
// 的求和包含，但胜者传的是 winner 自己的成本，故不会重复扣；输家后入库时由它自己的标记补扣。
// 两种顺序都算对。
//
// 幂等靠独立标记：`{行 id}:loser:{providerId}:{attempt}`，与 store.AddHedgeLoserCost 的
// 去重键（hedge_losers @> {providerId, attemptNumber}）同构。重试时要么没写库、要么命中标记。
// 成本文本直接用交给落库的那一份（cost.Total），结算与计费不会在定点位数上分叉。
func (b *hedgeLoserBiller) settleLoserLease(ctx context.Context, bill forward.HedgeLoserBill, costText string) {
	if b == nil || b.wiring.LeaseSettler == nil || b.state == nil || b.state.PC == nil {
		return
	}
	plan, ok := b.state.PC.LeaseSettlementPlan()
	if !ok || plan.Empty() {
		return
	}
	marker := strconv.FormatInt(bill.RequestID, 10) + ":loser:" +
		strconv.FormatInt(bill.ProviderID, 10) + ":" + strconv.Itoa(bill.Sequence)
	b.wiring.LeaseSettler.SettleLeases(ctx, marker, costText, plan)
}

// hedgeLoserEntry 把用量翻成落库条目（字段与 Node 的 HedgeLoserBilling 同形）。
func hedgeLoserEntry(bill forward.HedgeLoserBill, usage terminal.Usage) store.HedgeLoserEntry {
	return store.HedgeLoserEntry{
		ProviderID:               bill.ProviderID,
		ProviderName:             bill.ProviderName,
		AttemptNumber:            bill.Sequence,
		InputTokens:              usage.InputTokens,
		OutputTokens:             usage.OutputTokens,
		CacheCreationInputTokens: usage.CacheCreationInputTokens,
		CacheReadInputTokens:     usage.CacheReadInputTokens,
	}
}

// hedgeCostPositive 报告成本是否大于零（Node：`if (!cost.gt(0)) return null`，
// response-handler.ts:6998）。
func hedgeCostPositive(total string) bool {
	value, err := strconv.ParseFloat(strings.TrimSpace(total), 64)
	return err == nil && value > 0
}

// recordSelection 记下一次尝试命中的选路留痕（并发安全）。
//
// 为什么需要互斥：竞速的候选是在阈值计时器的协程里选的（forward 的 triggerThreshold
// 在 time.AfterFunc 里跑），而读它的除了终态协程，还有输家计费协程。
func (s *RequestState) recordSelection(providerID int64, capture route.Result) {
	if s == nil {
		return
	}
	s.selectionMu.Lock()
	defer s.selectionMu.Unlock()
	if s.selections == nil {
		s.selections = map[int64]route.Result{}
	}
	s.selections[providerID] = capture
}

// selectionFor 取某供应商的选路留痕（并发安全）。
func (s *RequestState) selectionFor(providerID int64) (route.Result, bool) {
	if s == nil {
		return route.Result{}, false
	}
	s.selectionMu.Lock()
	defer s.selectionMu.Unlock()
	capture, ok := s.selections[providerID]
	return capture, ok
}
