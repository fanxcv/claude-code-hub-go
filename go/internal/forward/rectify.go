package forward

import (
	"context"
	"fmt"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/rectify"
	"github.com/fanxcv/claude-code-hub-go/go/internal/specialsettings"
)

// 本文件是六个「请求正文整流器」在转发主干上的接线（Node 侧对应 forwarder.ts:2653-2697 的
// 2.5 段与 3463-3500 的供应商级发送前段）。整流逻辑全在 `internal/rectify`，这里只管三件事：
// 正文快照的可变副本、同一供应商一轮的幂等状态、以及审计条目的攒取。

// rectifierState 是一次转发里整流器的可变状态。
//
// 为什么需要它：Node 的整流器直接原地改会话对象 `session.request.message`，而 Go 的正文是
// 「客户端正文快照 + 每次尝试重新 BuildPlan」的形态——整流后的正文必须写回快照，否则下一次
// 尝试又会拿到未整流的版本。
type rectifierState struct {
	// client 是本次转发实际使用的客户端正文快照；整流器改写它。
	client ClientRequest
	// retry 是「同一供应商仅重试一次」的幂等标记，每个供应商轮次开头重置。
	retry rectify.RetryState
	// sink 是审计条目的落点（接线层按请求收，终态追加落库）；nil 表示不落审计。
	sink func(entry map[string]any)
}

// record 交出一条审计条目。整流照常生效，只是没地方落痕（如实测缺陷遗留面）。
func (s *rectifierState) record(entry map[string]any) {
	if s == nil || entry == nil || s.sink == nil {
		return
	}
	s.sink(entry)
}

// resetForProvider 在进入一个供应商的尝试循环前重置幂等标记。
//
// Node 在每个供应商循环迭代开头重置（forwarder.ts:1766-1772）：同一供应商只整流重试一次，
// 换供应商后可以对新供应商再整流一次。
func (s *rectifierState) resetForProvider() {
	s.retry = rectify.RetryState{}
}

// entries 已由 sink 直接交付接线层，本包不再自行攒取（见 rectifierState.record）。

// applyBillingHeaderRectifier 是主动型 billing header 整流（Node forwarder.ts:3463-3500）。
//
// 触发条件不是上游报错，而是「请求即将发往 ANTHROPIC 供应商」这一事实：只按 providerType 门控，
// 不区分原生与非原生上游（与 Node 逐字一致）。剥离后的正文写回快照，后续所有尝试都用它。
func (s *rectifierState) applyBillingHeaderRectifier(provider Provider, switches rectify.Switches, logger *logx.Logger) {
	if !switches.BillingHeader || rectify.KindOfProviderType(provider.Type) != rectify.KindAnthropic {
		return
	}
	body, ok := parseClientBody(s.client)
	if !ok {
		return
	}
	fields, applied := rectify.StripBillingHeader(body)
	if !applied {
		return
	}
	s.client.Body = []byte(body.MarshalCompact())
	s.record(specialsettings.ProactiveRectifierEntry(
		specialsettings.TypeBillingHeaderRectifier, fields,
	))
	logger.Warn("forward.rectify.billing_header", map[string]any{
		"provider_id":   provider.ID,
		"provider_name": provider.Name,
		"removed_count": fields["removedCount"],
	})
}

// applyReactiveRectifier 在一次失败之后尝试被动整流（Node tryApplyReactiveRectifier）。
//
// 前置排除与 Node 同两条：供应商局部模型缺口（404 的「本账号池不支持该模型」）与以 400 回传的
// 上游存储容量故障——这两类失败与请求正文无关，整流改变不了结果，命中文案也不该触发重试。
//
// 返回值语义：Matched=false 表示没有整流器命中（调用方按原失败处理）；
// Matched=true 且 Applied=true 表示正文已被整流（调用方对同一供应商重试一次）；
// Matched=true 且 Applied=false 表示命中但不可整流或已重试过（调用方按不可重试终止）。
func (d Deps) applyReactiveRectifier(
	ctx context.Context,
	provider Provider,
	failure *Failure,
	state *rectifierState,
	attempt int,
) rectify.Result {
	if failure == nil {
		return rectify.Result{}
	}
	// 前置排除与 Node 同两条：供应商局部模型缺口（404 的本账号池不支持该模型）与以 400 回传的
	// 上游存储容量故障——这两类失败与请求正文无关，整流改变不了结果。
	if isProviderLocalModelUnavailable(failure) {
		return rectify.Result{}
	}
	if failure.StatusCode == 400 && !failure.Synthetic && hasStorageCapacityMarker(failure.Body) {
		return rectify.Result{}
	}

	kind := rectify.KindOfProviderType(provider.Type)
	if kind == rectify.KindOther {
		return rectify.Result{}
	}
	body, ok := parseClientBody(state.client)
	if !ok {
		return rectify.Result{}
	}

	result := rectify.Apply(body, kind, d.rectifierSwitches(ctx), rectifierErrorMessage(provider, failure), &state.retry)
	if !result.Matched {
		return result
	}

	// 审计在判定 applied 之前就交出去（Node 同样如此）：命中但整流不适用也是一条事实。
	state.record(specialsettings.ReactiveRectifierEntry(
		result.Type, result.Trigger, provider.ID, provider.Name, attempt, attempt+1, result.Applied, result.Fields,
	))
	if result.Applied {
		state.client.Body = []byte(body.MarshalCompact())
	}
	return result
}

// rectifierSwitches 取本次请求的六个整流器开关。
//
// 缝隙为 nil 或读取失败时回落 Node 默认（全开）：Node 一律 `settings.x ?? true`，
// 读不到设置时按关闭处理会让整流器在生产静默失效，方向相反。
func (d Deps) rectifierSwitches(ctx context.Context) rectify.Switches {
	if d.RectifySwitches == nil {
		return rectify.NodeDefaults()
	}
	return d.RectifySwitches(ctx)
}

// providerLocalModelUnavailableMarker 逐字取自 errors.ts:579（Node 用它把「本账号池不支持该模型」
// 这类 404 从可整流/可重试的错误里排除）。
const providerLocalModelUnavailableMarker = "not supported by any configured account in this group"

// isProviderLocalModelUnavailable 复刻 Node 的 isProviderLocalModelUnavailableError：
// 仅 404、非从响应体推断的合成状态，且**文案或正文**含标记词。
func isProviderLocalModelUnavailable(failure *Failure) bool {
	if failure.StatusCode != 404 || failure.Synthetic {
		return false
	}
	return strings.Contains(strings.ToLower(failure.Message), providerLocalModelUnavailableMarker) ||
		strings.Contains(strings.ToLower(failure.Body), providerLocalModelUnavailableMarker)
}

// rectifierErrorMessage 复刻 Node 的 ProxyError.getDetailedErrorMessage()（errors.ts）：
// `Provider <name> returned <status>: <message> | Upstream: <body>`。
//
// 为什么不在 detect 里直接用 Failure.Message：Node 的整流器检测的是**这个拼装串**，
// 上游正文（常含 JSON 包裹的真实文案）就在里面；只喂提取后的 message 会漏掉只在正文里出现的形态。
func rectifierErrorMessage(provider Provider, failure *Failure) string {
	parts := []string{fmt.Sprintf("Provider %s returned %d: %s", provider.Name, failure.StatusCode, failure.Message)}
	if failure.Body != "" {
		parts = append(parts, "Upstream: "+failure.Body)
	}
	return strings.Join(parts, " | ")
}

// parseClientBody 解析客户端正文快照为保序 JSON 对象。
//
// 正文缺失或不是 JSON 对象时返回 ok=false：整流器一律跳过（Node 在无 message 时同样什么都不做）。
func parseClientBody(client ClientRequest) (*convert.Value, bool) {
	if !client.HasBody || len(client.Body) == 0 {
		return nil, false
	}
	body, err := convert.ParseJSON(client.Body)
	if err != nil || !body.IsObject() {
		return nil, false
	}
	return body, true
}
