package dataplane

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
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

// writeGuardResponse 写回守卫链的抢答响应（自身失败与无可用供应商也走这条）。
//
// 4xx/5xx 的 JSON 正文会在写回前挂上会话 id（见 error_session_id.go）；这与 Node 在
// proxy-handler 末尾统一附着同效——凡本进程自答的错误，客户端都能拿到 cch_session_id。
func (h *Handler) writeGuardResponse(writer http.ResponseWriter, state *RequestState, response *guard.Response) {
	if response == nil {
		return
	}
	body := response.Body
	if state != nil {
		body = attachSessionIDToErrorBody(
			state.sessionID, response.Status, response.Headers.Get("content-type"), body,
		)
	}
	for key, values := range response.Headers {
		for _, value := range values {
			writer.Header().Add(key, value)
		}
	}
	writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
	writer.WriteHeader(response.Status)
	if len(body) > 0 {
		if _, err := writer.Write(body); err != nil {
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
	// 错误体挂会话 id（Node 在 handler 末尾统一附着）：只动 4xx/5xx 的 JSON 正文。
	// 取上游的 content-type 而非 writer 上的——非 2xx 不做协议转换，交付的正是上游那份正文。
	if state != nil {
		body = attachSessionIDToErrorBody(
			state.sessionID, result.StatusCode, result.Headers.Get("Content-Type"), body,
		)
	}
	converted := false
	switch {
	case state == nil:
	case !isSuccessStatus(result.StatusCode):
		// 非 2xx 不转换：错误体保持上游原样
	case hasOpaqueContentEncoding(result.Headers):
	default:
		conversion := newResponseConversion(
			result.Plan, state.Format, state.Model, false, h.options.PlaceholderThinkingSignature)
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
	case failure.Category == forward.CategoryProviderSaturated:
		// 全部候选都因并发上限满员：没有上游状态码可透传，按限流语义回 429
		// （信封由 saturationRateLimitBlock 另行构造，这里只保证结算口径一致）。
		if message == "" {
			message = "供应商并发会话数已达上限"
		}
		return http.StatusTooManyRequests, message
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

// saturationRateLimitBlock 把「全部候选都因并发上限满员」的终态归因翻成限流信封。
//
// 为什么单独一支而不是并进 failoverStatus：客户端对并发上限的处理依赖 Node 同形的七字段
// 信封（type / message / code / limit_type / current / limit / reset_time）与 429 状态码，
// 而 failoverStatus 只产出「状态码 + 文案」——混在一起会让既有 502/503 路径也跟着变形状。
//
// limit_type 取 provider_concurrent_sessions：与 Key/User 维度的 concurrent_sessions 区分开，
// 运维一眼就能分出「是渠道满了」还是「这个 key 的并发满了」。
//
// reset_time 留空（信封里是 null）：并发名额没有窗口重置时刻，它随在飞请求结束而归还；
// 编造一个「此刻」当重置时刻会让客户端按固定时刻重试。
func saturationRateLimitBlock(failure *forward.Failure) (guard.RateLimitBlock, bool) {
	if failure == nil || failure.Category != forward.CategoryProviderSaturated {
		return guard.RateLimitBlock{}, false
	}
	message := failure.Message
	if message == "" {
		message = "供应商并发会话数已达上限"
	}
	return guard.RateLimitBlock{
		Status:    http.StatusTooManyRequests,
		Message:   message,
		ErrorType: "rate_limit_error",
		LimitType: "provider_concurrent_sessions",
		Current:   float64(failure.ConcurrencyCurrent),
		Limit:     float64(failure.ConcurrencyLimit),
	}, true
}

// slowRateRetryMessage 是「判慢导致最终失败」时给客户端的通用文案。
//
// 故意只描述事实：这条正文原样交给 API 客户端，不得出现渠道名、上游 URL、IP 或凭据。
const slowRateRetryMessage = "上游长时间无响应或速率过低，已中断本次请求，请重试"

// slowRateErrorEnvelope 是判慢终局的错误信封，按入口协议取两种形状：
//
//	Anthropic（/v1/messages）：{"type":"error","error":{"type":"overloaded_error","message":"..."}}
//	OpenAI（/v1/chat/completions、/v1/responses）：{"error":{"code":"server_is_overloaded","message":"..."}}
//
// 两家客户端会解析正文决定是否重试，故形状必须是各自协议认得的那一种；omitempty 让同一个
// 信封产出两种形状，而不是维护两份结构。
type slowRateErrorEnvelope struct {
	Type  string        `json:"type,omitempty"`
	Error slowRateRetry `json:"error"`
}

type slowRateRetry struct {
	Type    string `json:"type,omitempty"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
}

// slowRateRetryResponse 把「最终失败确因判慢」翻成客户端可识别的可重试错误。
//
// 为什么与 failoverStatus 分开：这几个字只有在这一档才成立——判慢发生在提交前，客户端仍是
// 零字节，状态码与头部都还可改；而「让 agent 自己重试」需要三样同时到位：5xx（取 503，
// 不用 429——Codex 默认不重试 429）、retry-after: 0（超过 60s 的退避会被 pi 放弃）、
// x-should-retry: true。返回 false 表示这条失败不是判慢，调用方必须走原有分支，
// 其余失败的状态码与正文逐字不变。
func slowRateRetryResponse(state *RequestState, failure *forward.Failure) (*guard.Response, bool) {
	if failure == nil || failure.Category != forward.CategorySlowRate {
		return nil, false
	}
	response, err := slowRateRetryBody(clientFormatOf(state), slowRateRetryMessage)
	if err != nil {
		// 结构固定，正常取不到错误；真有的话宁可退回通用 503 形状，也不发一份客户端解析不了的正文。
		response = guard.BuildError(http.StatusServiceUnavailable, slowRateRetryMessage, "")
	}
	return response.WithHeader("retry-after", "0").WithHeader("x-should-retry", "true"), true
}

// slowRateRetryBody 按入口协议给错误体形状。
//
// Gemini 入口（gemini / gemini-cli）没有已核实的形状，故走通用 503 形状：状态码与两个重试头
// 仍然到位，客户端至少能按 5xx 重试。**该形状未经验证**，见报告。
func slowRateRetryBody(format convert.ClientFormat, message string) (*guard.Response, error) {
	switch format {
	case convert.FormatClaude:
		payload := slowRateErrorEnvelope{Type: "error"}
		payload.Error.Type = "overloaded_error"
		payload.Error.Message = message
		return guard.JSONResponse(http.StatusServiceUnavailable, payload)
	case convert.FormatOpenAI, convert.FormatResponse:
		payload := slowRateErrorEnvelope{}
		payload.Error.Code = "server_is_overloaded"
		payload.Error.Message = message
		return guard.JSONResponse(http.StatusServiceUnavailable, payload)
	default:
		return guard.BuildError(http.StatusServiceUnavailable, message, ""), nil
	}
}

// clientFormatOf 取本次请求的入站格式；无请求状态时返回空串（走通用形状）。
func clientFormatOf(state *RequestState) convert.ClientFormat {
	if state == nil {
		return ""
	}
	return state.Format
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
