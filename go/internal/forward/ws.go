package forward

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/upws"
)

// 客户端侧 WS 边缘写在入口请求上的隧道标记（internal/ws 的同名常量）。
//
// 它们出站时会被 ReservedInternalHeaderNames 剥掉，但**入口** headers 仍在 pctx 上——
// 上游 WS 的资格判定读的就是这一处事实，而不是出站头（出站头里已经没有了）。
const (
	wsClientTransportHeader = "x-cch-client-transport"
	wsSessionHeader         = "x-cch-responses-ws-session"
	wsClientTransportValue  = "websocket"
)

// WSSkipCause 是「本该走上游 WS 却没走」的路径标识，每条跳过路径一个词。
//
// 为什么需要它：wsAttempt 的三条跳过路径**刻意不留链痕迹**（链词表是冻结的 Node 词表，
// 没有对应词，且「没尝试」不该记成「尝试过但降级」）。代价是生产上「上游 WS 一直没生效」
// 只能靠猜——2026-09-19 的排障正是卡在这里：链上无键、日志无痕，无法区分是资格不成立、
// 端点被缓存还是压根没接线。故这四条路径改为**必然通知**：不改行为、不写链，只让事实可见。
//
// 类型名刻意不含 `Reason`：那些词不是链上的 reason（不写 provider_chain），而
// chainreason_test.go 的词表钉子按 `…Reason = "…"` 的形状扫本包源码，用 Reason 命名会被
// 当成「自造链词」而报错——钉子是对的，该改的是命名。
type WSSkipCause string

const (
	// WSSkipCauseNotWired 表示转发层没接上 WS 拨号器或资格判定（装配缺口）。
	WSSkipCauseNotWired WSSkipCause = "not_wired"
	// WSSkipCauseNotEligible 表示资格判定不成立（客户端不是 WS / 供应商不是 codex / 开关关闭）。
	WSSkipCauseNotEligible WSSkipCause = "not_eligible"
	// WSSkipCauseEndpointCached 表示端点命中「不支持 WS」短期缓存，未发起握手。
	WSSkipCauseEndpointCached WSSkipCause = "endpoint_cached"
	// WSSkipCauseProtocolMismatch 表示本次尝试的协议线不是 Responses 线（正文不能当 WS 首帧发出）。
	WSSkipCauseProtocolMismatch WSSkipCause = "protocol_mismatch"
)

// WSSkip 是一条跳过事实，交给 Deps.WSNotice。
type WSSkip struct {
	Cause        WSSkipCause
	ProviderID   int64
	ProviderType string
	EndpointURL  string
}

// IsWebSocketClientRequest 判定入口请求是否来自客户端 WS 通道。
//
// 供接线层构造 Deps.WSEligible 用：把「怎么认客户端 WS」这件事留在本包一处，
// 免得数据面自己拼头名（头名一改两处就会静默分叉，而分叉的表现是「上游 WS 永远不生效」）。
func IsWebSocketClientRequest(pc *pctx.Context) bool {
	if pc == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(pc.Headers().Get(wsClientTransportHeader)), wsClientTransportValue)
}

// wsAttempt 在具备资格时尝试上游 WS；成功则返回可直接当 HTTP 响应使用的响应，否则返回 nil。
//
// 只在**流式**路径调用：客户端 WS 通道的每一轮都是流（WS 客户端没有「非流式」形态）。
//
// 回落语义（本函数的全部要点）：失败即让调用方继续走 HTTP，且**不返回 Failure、不记账**——
// 上游 WS 走不成本身不是供应商故障（Node 明示：不切换供应商、不计入熔断器）。留痕只写
// AttemptOutcome.WS，由落链侧决定怎么表达。
func (d Deps) wsAttempt(
	ctx context.Context,
	pc *pctx.Context,
	provider Provider,
	plan *Plan,
	outcome *AttemptOutcome,
) *dial.Response {
	if plan == nil || outcome == nil {
		return nil
	}
	if d.WS == nil || d.WSEligible == nil {
		d.noticeWSSkip(pc, WSSkipCauseNotWired, provider, plan.URL)
		return nil
	}
	if !d.WSEligible(ctx, pc, provider) {
		d.noticeWSSkip(pc, WSSkipCauseNotEligible, provider, plan.URL)
		return nil
	}
	// 端点此前失败过（短期缓存）：不尝试、也不留痕——与「不具备资格」同语义：什么都没发生。
	if !d.WS.EndpointEligible(plan.URL) {
		d.noticeWSSkip(pc, WSSkipCauseEndpointCached, provider, plan.URL)
		return nil
	}
	// 防御：WS 通道的帧就是 Responses 事件，正文必须属于 Responses 线。不是该线时宁可不走，
	// 也不把别的协议线的正文当 response.create 帧发出去——那是把请求发错形态，不是降级。
	if plan.Protocol != convert.ProtocolOpenAIResponses {
		d.noticeWSSkip(pc, WSSkipCauseProtocolMismatch, provider, plan.URL)
		return nil
	}
	// 走到这里即为「真的尝试」：链上的 WS 事实由下面的 result 分支写。

	result := d.WS.DialWS(ctx, upws.Request{
		EndpointURL: plan.URL,
		Headers:     plan.Headers,
		Body:        plan.Body,
		SessionID:   wsSessionID(pc),
	})
	facts := &AttemptWSFacts{ClientTransport: wsClientTransportValue}
	switch {
	case result.Response != nil:
		facts.Attempted = true
		facts.Connected = true
		outcome.WS = facts
		return result.Response
	case result.Attempted && ctx.Err() == nil:
		facts.Attempted = true
		facts.DowngradedToHTTP = true
		facts.DowngradeReason = string(result.Reason)
		outcome.WS = facts
		d.logger().Warn("forward: 上游 WebSocket 回落 HTTP", map[string]any{
			"provider_id": outcome.ProviderID,
			"endpoint_id": outcome.EndpointID,
			"reason":      string(result.Reason),
		})
		return nil
	default:
		// 上下文已取消（客户端中断）或压根没发起握手：不写降级——前者不是降级，后者没发生。
		return nil
	}
}

// noticeWSSkip 上报一条「跳过上游 WS」的事实。
//
// 两类静默：未接 WSNotice（调用方不关心）、客户端不是 WS 通道（普通 HTTP 请求跳过是常态，
// 逐条上报只会淹没日志）。后者也是本钩子唯一能过滤的维度——供应商类型与开关状态由
// 实现侧自行记录。
func (d Deps) noticeWSSkip(pc *pctx.Context, cause WSSkipCause, provider Provider, endpointURL string) {
	if d.WSNotice == nil || !IsWebSocketClientRequest(pc) {
		return
	}
	d.WSNotice(WSSkip{
		Cause:        cause,
		ProviderID:   provider.ID,
		ProviderType: string(provider.Type),
		EndpointURL:  endpointURL,
	})
}

// wsSessionID 取本次客户端 WS 会话 id；缺失时新生成一个 UUID v4。
//
// 上游拿它做会话粘性与 previous_response_id 的连续性，故必须是**每连接稳定**的值：
// 能取到隧道标记就取它，取不到才新生成（不共用固定值，否则不同客户端会被粘到一起）。
func wsSessionID(pc *pctx.Context) string {
	if pc != nil {
		if value := strings.TrimSpace(pc.Headers().Get(wsSessionHeader)); value != "" {
			return value
		}
	}
	return newUUIDv4()
}

// newUUIDv4 生成 RFC 4122 版本 4 UUID（16 字节随机 + 版本/变体位）。
func newUUIDv4() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return ""
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	buffer := make([]byte, 36)
	hex.Encode(buffer[0:8], raw[0:4])
	buffer[8] = '-'
	hex.Encode(buffer[9:13], raw[4:6])
	buffer[13] = '-'
	hex.Encode(buffer[14:18], raw[6:8])
	buffer[18] = '-'
	hex.Encode(buffer[19:23], raw[8:10])
	buffer[23] = '-'
	hex.Encode(buffer[24:36], raw[10:16])
	return string(buffer)
}
