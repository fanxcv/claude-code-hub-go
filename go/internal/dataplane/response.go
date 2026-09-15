package dataplane

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
)

// 本文件负责「把结果写成 HTTP 响应」。三条纪律：
//
//  1. 守卫链的抢答响应原样写回：状态码、头部与已序列化正文都是判定结果的一部分，
//     重写其中任何一项都会让客户端看到与 Node 不同的错误形状。
//  2. 逐跳头部不转发（RFC 9110 的 hop-by-hop），Content-Length 由本包按实际正文重算——
//     上游的长度可能与门控后的正文不一致，照抄会挂住客户端。
//  3. 未实现路由交回 Node 而不是 501：501 会让客户端把「Go 还没实现」当成「协议不支持」
//     而放弃重试（见 httpapi 的同款注释）。

// hopByHopHeaders 是不得转发给客户端的逐跳头部。
var hopByHopHeaders = []string{
	"Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Proxy-Connection",
	"TE",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// writeGuardResponse 写回守卫链的抢答响应。
func (h *Handler) writeGuardResponse(writer http.ResponseWriter, response *guard.Response) {
	if response == nil {
		return
	}
	for key, values := range response.Headers {
		for _, value := range values {
			writer.Header().Add(key, value)
		}
	}
	writer.Header().Set("Content-Length", strconv.Itoa(len(response.Body)))
	writer.WriteHeader(response.Status)
	if len(response.Body) > 0 {
		if _, err := writer.Write(response.Body); err != nil {
			h.logger.Debug("dataplane.guard_write_failed", map[string]any{"error": err.Error()})
		}
	}
}

// writeForwardResult 写回非流式转发结果。
//
// 纯非流式请求（客户端未要求 stream）不会成为回放 owner，故本函数的回放分支只在
// 「客户端要 stream 而上游回答非流式正文」时命中；此时正文已完整，可一次性喂入 spool。
func (h *Handler) writeForwardResult(
	writer http.ResponseWriter,
	ctx context.Context,
	result *forward.StreamResult,
	state *RequestState,
) {
	if state != nil && state.replay != nil {
		session := state.replay
		if session.startBuffered(ctx, result.StatusCode, result.Headers) != nil {
			session.observe(result.Body)
			// 非流式结算已在 forward 内完成（ForwardStream 的 settleNonStream），故此处
			// 处于计费之后，可直接做完成屏障。
			session.completeBuffered(ctx)
		}
	}
	body := result.Body
	// 响应修复器（Node 的 response-fixer）：修的是**上游线**字节，故必须早于协议转换——
	// 惰性 `chat.completion.chunk` 帧的判定就依赖「字节还是上游方言」这个前提。
	body = h.fixNonStreamBody(ctx, state, result.Headers, body)
	converted := false
	switch {
	case state == nil:
	case !isSuccessStatus(result.StatusCode):
		// 非 2xx 不转换：错误体保持上游原样
	case hasOpaqueContentEncoding(result.Headers):
	default:
		conversion := newResponseConversion(result.Plan, state.Format, state.Model, false)
		body, converted = conversion.applyNonStream(body)
	}
	copyUpstreamHeaders(writer.Header(), result.Headers)
	if converted {
		// 正文已被重写：长度与媒体类型都由本层给出（上游的长度与 charset 不再成立）。
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	}
	writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
	status := result.StatusCode
	if status == 0 {
		status = http.StatusOK
	}
	// response.after 的两项事实（交付头与状态码）在写头之前采，与流式路径同一纪律。
	h.recordDeliveredHeaders(state, writer.Header())
	h.recordStatus(state, status)
	writer.WriteHeader(status)
	if len(body) > 0 {
		// 非流式正文一次到齐：直接喂有界捕获（与流式路径同一口径，都是「客户端可见字节」）。
		if state != nil && state.responseCapture != nil {
			state.responseCapture.Write(body)
		}
		if _, err := writer.Write(body); err != nil {
			h.logger.Debug("dataplane.body_write_failed", map[string]any{"error": err.Error()})
		}
	}
}

// isSuccessStatus 判定上游状态码是否 2xx（0 视为成功：由调用方补默认码）。
func isSuccessStatus(status int) bool {
	return status == 0 || (status >= 200 && status < 300)
}

// copyUpstreamHeaders 复制上游头部，剔除逐跳项与长度。
func copyUpstreamHeaders(target http.Header, upstream http.Header) {
	for key, values := range upstream {
		if isHopByHop(key) || http.CanonicalHeaderKey(key) == "Content-Length" {
			continue
		}
		for _, value := range values {
			target.Add(key, value)
		}
	}
}

// isHopByHop 判定头部是否属逐跳集合（大小写不敏感）。
func isHopByHop(key string) bool {
	canonical := http.CanonicalHeaderKey(key)
	for _, name := range hopByHopHeaders {
		if canonical == name {
			return true
		}
	}
	return false
}

// failoverStatus 把「尝试耗尽」的归因翻成状态码与文案。
//
// 上游明确给过状态码（4xx/5xx）时原样透传该码：客户端对 429/400 的处理依赖它；
// 没有上游状态的（传输层失败、本地过载）分别归为 502 与 503——Node 侧同样是这两档。
func failoverStatus(failure *forward.Failure) (int, string) {
	if failure == nil {
		return http.StatusBadGateway, "上游请求失败"
	}
	category := failure.Category.String()
	message := failure.Message
	switch {
	case failure.Internal && forward.IsLocalOverloadError(failure.Err):
		if message == "" {
			message = "本进程过载，请稍后重试"
		}
		return http.StatusServiceUnavailable, message
	case failure.StatusCode >= 400 && failure.StatusCode <= 599:
		if message == "" {
			message = "上游返回 " + strconv.Itoa(failure.StatusCode)
		}
		return failure.StatusCode, message
	case failure.Category == forward.CategorySystemError:
		if message == "" {
			message = "上游超时"
		}
		return http.StatusGatewayTimeout, message
	default:
		if message == "" {
			message = "上游请求失败（" + category + "）"
		}
		return http.StatusBadGateway, message
	}
}

// failoverStatusFor 在基础归因之上应用 `pass_through_upstream_error_message`。
//
// 两态口径（Node error-handler.ts:121-158）：
//   - 开关关闭 → 状态码对应的通用文案（genericUpstreamErrorMessage）；
//   - 开关打开 → 从上游原文派生一条客户端安全文案，派生不出来才回退通用文案。
//
// 「打开」不等于「原样透传」：Node 会在派生阶段丢掉供应商名、URL、内网标签、请求 id 与
// 密钥形状的文本，因为这些属于内部信息。改造前 Go 直接回 upstream 抽出的原文，
// 即缺少这一步脱敏——本条是向 Node 对齐。
func (h *Handler) failoverStatusFor(ctx context.Context, failure *forward.Failure) (int, string) {
	status, message := failoverStatus(failure)
	if !eligibleForClientMessageDerivation(failure) {
		return status, message
	}
	generic := genericUpstreamErrorMessage(status)
	if !h.passThroughUpstreamErrorMessage(ctx) {
		return status, generic
	}
	derived := deriveClientSafeUpstreamErrorMessage(deriveClientSafeUpstreamErrorMessageInput{
		CandidateMessage: failure.Message,
		ProviderName:     failure.ProviderName,
	})
	if derived == "" {
		return status, generic
	}
	return status, derived
}

// eligibleForClientMessageDerivation 判定这条失败是否走「上游错误文案」的两态口径。
//
// 只覆盖「上游回传了 4xx/5xx 错误响应」这一类：Go 自己造的失败（本地过载、传输超时归因、
// 空响应、假 200 检测结果）文案由本进程产生，既不含上游内部信息，替换成通用文案反而丢信息。
func eligibleForClientMessageDerivation(failure *forward.Failure) bool {
	if failure == nil || failure.Internal || failure.Synthetic || failure.EmptyResponse {
		return false
	}
	return upstreamErrorStatusCode(failure.StatusCode)
}

// passThroughUpstreamErrorMessage 读 `system_settings.pass_through_upstream_error_message`。
//
// Node 的缺省是 true（error-handler.ts:137 的 `?? true`），故读不到时按 true——
// 与 Node 的「设置读失败仍走派生」同向。
func (h *Handler) passThroughUpstreamErrorMessage(ctx context.Context) bool {
	if h.options.Base.Settings == nil {
		return true
	}
	settings, err := h.options.Base.Settings.FindSystemSettings(ctx)
	if err != nil || settings == nil {
		h.logger.Warn("dataplane.pass_through_error_message_lookup_failed", map[string]any{"error": errorText(err)})
		return true
	}
	return settings.PassThroughUpstreamErrorMessage
}

// errorFailure 从转发的返回值里取最终归因。
func errorFailure(result *forward.Result, err error) *forward.Failure {
	var failure *forward.Failure
	if errors.As(err, &failure) && failure != nil {
		return failure
	}
	status := http.StatusBadGateway
	if result != nil && result.StatusCode >= 400 {
		status = result.StatusCode
	}
	return &forward.Failure{StatusCode: status, Err: err, Message: errorText(err)}
}
