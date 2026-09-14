package dataplane

import (
	"context"
	"net/http"

	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/session"
)

// 本文件是数据面的**响应侧工件接线**：把「响应已交付客户端」这一时机翻成会话写侧的调用，
// 组装四份相位快照并交出有界捕获的正文。
//
// 采集点（与 Node 对齐，四份快照各有唯一稳定时机）：
//
//	request.before   请求进入数据面（守卫链通过、正文尚未被改写）→ ServeHTTP
//	request.after    上游请求已成形（URL/method/最终正文）           → forward
//	response.before  上游响应头已到（记录原始上游结果）             → forward
//	response.after   响应已回给客户端（记录最终交付形态）           → 交付路径
//
// 为什么必须在**四条不同路径**上都记一次终态：数据面的终态出口不止一个（非流式正文、
// 流式泵、守卫抢答、失败归因），任何一条漏记都会让详情页的某一相位永远是空的——而空相位
// 在 UI 上表现为「这块没数据」，与「这次确实没采到」无法区分。

// newResponseCapture 按开关决定本请求是否捕获响应正文。
//
// 三道闸（缺一不捕）：接线存在、调试工件开关打开（高并发模式关闭）、响应正文总开关打开。
func (h *Handler) newResponseCapture(state *RequestState) *session.ResponseCapture {
	if h.options.Telemetry == nil || state == nil || !state.PC.ShouldPersistDebugArtifacts() {
		return nil
	}
	options := h.options.SessionArtifacts
	// Stream 里的工件选项由装配填入（见 assemble.go）；零值时按「未开启」处理，
	// 不让「忘配置」变成「静默落正文」。
	if !options.StoreResponseBody {
		return nil
	}
	return session.NewResponseCapture(session.ResponseCaptureWindowBytes)
}

// captureRequestSnapshot 采 request.before 的正文与 messages（与请求工件同一份数据）。
//
// 与 telemetryFacts 共用同一道高并发闸：关掉调试工件时连快照都不留痕（不是「留一份空快照」）。
// 读一次正文的解析结果（body.json）而不是重新读体：重读会解压第二遍，而正文已在守卫链里被
// 解过。
func (h *Handler) captureRequestSnapshot(state *RequestState, body *bodyAccess) {
	if state == nil || body == nil || !state.PC.ShouldPersistDebugArtifacts() {
		return
	}
	tree, err := body.json()
	if err != nil || tree == nil {
		return
	}
	state.requestSnapshotBody = tree
	if messages, ok := tree["messages"]; ok {
		state.requestSnapshotMessages = messages
		state.requestSnapshotHasMsgs = true
	}
}

// clientSnapshotHeaders 采客户端请求头的脱敏副本（request.before 的 headers）。
//
// 与 Node 的 filterClientRequestSnapshotHeaders 同判：先脱敏（SanitizeHeaders），再按
// CLIENT_HEADER_SNAPSHOT_BLOCKLIST 剔除边缘节点注入的头（cf-* / x-forwarded-* / traceparent
// 等）。这些头的值是基础设施信息，与会话内容无关，留着只会放大落盘体积。
//
// 返回 nil 表示不应采集（高并发模式或未接线）。
func (h *Handler) clientSnapshotHeaders(request *http.Request) map[string]string {
	if request == nil {
		return nil
	}
	sanitized := session.SanitizeHeaders(map[string][]string(request.Header))
	filtered := make(map[string]string, len(sanitized))
	for name, value := range sanitized {
		if isBlockedClientSnapshotHeader(name) {
			continue
		}
		filtered[name] = value
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

// blockedClientSnapshotPrefixes 是 CLIENT_HEADER_SNAPSHOT_BLOCKLIST 的前缀规则（小写比对）。
var blockedClientSnapshotPrefixes = []string{
	"cf-", "x-forwarded-", "x-b3-",
}

// blockedClientSnapshotNames 是 CLIENT_HEADER_SNAPSHOT_BLOCKLIST 的精确规则（小写比对）。
var blockedClientSnapshotNames = map[string]bool{
	"x-real-ip":       true,
	"true-client-ip":  true,
	"forwarded":       true,
	"traceparent":     true,
	"tracestate":      true,
	"baggage":         true,
	"x-amzn-trace-id": true,
}

// isBlockedClientSnapshotHeader 复刻 CLIENT_HEADER_SNAPSHOT_BLOCKLIST 的逐条正则。
func isBlockedClientSnapshotHeader(name string) bool {
	lower := lowerASCII(name)
	if blockedClientSnapshotNames[lower] {
		return true
	}
	for _, prefix := range blockedClientSnapshotPrefixes {
		if hasPrefixASCII(lower, prefix) {
			return true
		}
	}
	return false
}

// lowerASCII 是头名的小写化（头名是 ASCII，不引入 unicode 包）。
func lowerASCII(value string) string {
	buffer := make([]byte, len(value))
	for index := 0; index < len(value); index++ {
		c := value[index]
		if c >= 'A' && c <= 'Z' {
			buffer[index] = c + ('a' - 'A')
			continue
		}
		buffer[index] = c
	}
	return string(buffer)
}

// hasPrefixASCII 判定前缀（大小写已归一的输入）。
func hasPrefixASCII(value, prefix string) bool {
	return len(value) >= len(prefix) && value[:len(prefix)] == prefix
}

// recordUpstream 记下本次实际发往的上游与收到的响应头（request.after 与 response.before 的来源）。
//
// 只记**第一次**：重试与故障转移会多次改写这两项，而详情页要看的是「最终服务这次请求的
// 那一次」，与账本的 provider_chain 口径一致（Node 的 detailSnapshotRequestAfter 同样在
// 发往上游前一刻覆盖，故最后一次胜出；这里的差别是**登记**的，见报告）。
func (h *Handler) recordUpstream(state *RequestState, plan *forwardPlanView, headers http.Header) {
	if state == nil {
		return
	}
	if plan != nil && plan.URL != "" && state.upstreamURL == "" {
		state.upstreamURL = plan.URL
		state.upstreamMethod = plan.Method
	}
	if headers != nil && state.upstreamResponseHeaders == nil {
		state.upstreamResponseHeaders = headers
	}
}

// forwardPlanView 是计划里响应工件需要的两项（避免为一行取值把整个 forward.Plan 传进来）。
type forwardPlanView struct {
	URL    string
	Method string
}

// recordStreamUpstream 记下流式路径的上游事实（计划 URL/method、响应头、状态码）。
func (h *Handler) recordStreamUpstream(state *RequestState, result *forward.StreamResult) {
	if result == nil {
		return
	}
	h.recordUpstream(state, planViewOf(&result.Result), result.Headers)
	h.recordUpstreamStatus(state, result.StatusCode)
}

// planViewOf 安全地取计划里的 URL/method。
//
// **Plan 可能为 nil**：尝试在上游请求成形之前就失败（拨号失败、候选耗尽前的计划构造失败）
// 时结果是「有留痕、没有计划」，解 Plan 会直接 panic。这是失败路径，而失败路径恰恰是最需要
// 保持可用的那条。
func planViewOf(result *forward.Result) *forwardPlanView {
	if result == nil || result.Plan == nil {
		return nil
	}
	return &forwardPlanView{URL: result.Plan.URL, Method: result.Plan.Method}
}

// recordStatus 记下回给客户端的状态码（response.after 的 meta 来源）。
func (h *Handler) recordStatus(state *RequestState, status int) {
	if state == nil || status == 0 {
		return
	}
	state.responseStatus = status
}

// recordUpstreamStatus 记下上游响应的状态码（response.before 的 meta 来源）。
//
// 与 recordStatus 分开：response.before 是**上游原始结果**，response.after 是**回给客户端的
// 结果**；门控前缀或失败归因会让两者不同，合成一个字段就再也分不出来。
func (h *Handler) recordUpstreamStatus(state *RequestState, status int) {
	if state == nil || status == 0 || state.upstreamStatus != 0 {
		return
	}
	state.upstreamStatus = status
}

// recordFailureStatus 记下未走到交付路径时的归因状态码（守卫抢答 / 失败归因）。
func (h *Handler) recordFailureStatus(state *RequestState, status int) {
	if state == nil || status == 0 {
		return
	}
	state.failureStatus = status
}

// recordDeliveredHeaders 记下**已交付客户端**的响应头（response.after 的 headers）。
//
// 取的是 writer.Header() 的快照（copyUpstreamHeaders 与逐跳剔除都已发生），而不是上游头——
// 两者正是 before/after 的区别所在。这里只取一次（首次写头时），后续重写不覆盖。
func (h *Handler) recordDeliveredHeaders(state *RequestState, headers http.Header) {
	if state == nil || headers == nil || state.deliveredResponseHeaders != nil {
		return
	}
	state.deliveredResponseHeaders = session.SanitizeHeaders(httpHeadersToMap(headers))
}

// finishResponseArtifacts 在请求收尾时落响应侧工件。
//
// 走独立后台上下文（与 finishTelemetry 同一理由：客户端可能已断开，而留痕与客户端还在不在
// 无关）；超时由会话写侧统一给（session.ArtifactWriteTimeout）。
func (h *Handler) finishResponseArtifacts(state *RequestState, lease TelemetryLease) {
	if h.options.Telemetry == nil || state == nil || lease.Identity == "" {
		return
	}
	status := state.responseStatus
	if status == 0 {
		// 未走到交付路径（守卫抢答/失败归因）：状态码取归因结果，没有就按非成功记。
		status = state.failureStatus
	}
	if status == 0 {
		status = http.StatusInternalServerError
	}
	ctx, cancel := context.WithTimeout(context.Background(), session.ArtifactWriteTimeout)
	defer cancel()

	h.options.Telemetry.Respond(ctx, lease, ResponseArtifacts{
		StatusCode:      status,
		UpstreamURL:     state.upstreamURL,
		UpstreamMethod:  state.upstreamMethod,
		RequestHeaders:  state.clientHeaders,
		ResponseHeaders: session.SanitizeHeaders(httpHeadersToMap(state.upstreamResponseHeaders)),
		ResponseBody:    state.capturedResponseBody(),
		RequestBefore:   state.requestBeforeSnapshot(),
		RequestAfter:    state.requestAfterSnapshot(),
		ResponseBefore:  state.responseBeforeSnapshot(),
		ResponseAfter:   state.responseAfterSnapshot(),
	})
}

// capturedResponseBody 取有界窗口的正文（未捕时 nil）。
func (s *RequestState) capturedResponseBody() []byte {
	if s == nil || s.responseCapture == nil {
		return nil
	}
	return s.responseCapture.Bytes()
}

// requestBeforeSnapshot 组装 request.before：客户端原始头 + 客户端原始正文/messages。
//
// meta 的 clientUrl 在 Go 侧拿不到（Node 的 ProxySession.requestUrl 由代理层构造），故留空——
// 登记差异：详情页的 request.before.meta.clientUrl 为 null（Node 非 null）。
// 这是**刻意**不猜的：编一个 URL 会让对拍看起来更接近，但它不是真的客户端地址。
func (s *RequestState) requestBeforeSnapshot() session.SessionDetailPhaseSnapshot {
	if s == nil {
		return session.SessionDetailPhaseSnapshot{}
	}
	snapshot := session.SessionDetailPhaseSnapshot{
		Body:        s.requestSnapshotBody,
		Messages:    s.requestSnapshotMessages,
		HasMessages: s.requestSnapshotHasMsgs,
		Headers:     s.clientHeaders,
	}
	if s.PC != nil && s.PC.Method() != "" {
		method := s.PC.Method()
		snapshot.Meta.Method = &method
	}
	return snapshot
}

// requestAfterSnapshot 组装 request.after：最终发往上游的 URL/method。
//
// body 与 headers 在 Go 侧不再单独留痕：转换后的上游正文由 forward 内部构造并立即交出，
// 留一份副本等于给每流再加一份正文驻留（与「有界捕获」的定档相悖）。故这一相位的
// body/headers 为 null，meta 齐备——登记差异，见报告。
func (s *RequestState) requestAfterSnapshot() session.SessionDetailPhaseSnapshot {
	if s == nil {
		return session.SessionDetailPhaseSnapshot{}
	}
	snapshot := session.SessionDetailPhaseSnapshot{}
	if s.upstreamURL != "" {
		url := s.upstreamURL
		snapshot.Meta.UpstreamURL = &url
	}
	if s.upstreamMethod != "" {
		method := s.upstreamMethod
		snapshot.Meta.Method = &method
	}
	return snapshot
}

// responseBeforeSnapshot 组装 response.before：上游原始响应头与状态码。
func (s *RequestState) responseBeforeSnapshot() session.SessionDetailPhaseSnapshot {
	if s == nil || s.upstreamResponseHeaders == nil {
		return session.SessionDetailPhaseSnapshot{}
	}
	snapshot := session.SessionDetailPhaseSnapshot{
		Headers: session.SanitizeHeaders(httpHeadersToMap(s.upstreamResponseHeaders)),
	}
	if s.upstreamURL != "" {
		url := s.upstreamURL
		snapshot.Meta.UpstreamURL = &url
	}
	if status := s.upstreamStatus; status != 0 {
		snapshot.Meta.StatusCode = &status
	}
	return snapshot
}

// responseAfterSnapshot 组装 response.after：最终交付形态（状态码 + 客户端可见头）。
//
// 与 before 的差别正是它存在的理由：门控前缀、格式回译与逐跳头剔除都发生在两者之间。
// 这里记的是**已交付客户端的头**（copyUpstreamHeaders 之后 writer 上的那一份）。
func (s *RequestState) responseAfterSnapshot() session.SessionDetailPhaseSnapshot {
	if s == nil {
		return session.SessionDetailPhaseSnapshot{}
	}
	snapshot := session.SessionDetailPhaseSnapshot{
		Headers: s.deliveredResponseHeaders,
	}
	if s.responseStatus != 0 {
		status := s.responseStatus
		snapshot.Meta.StatusCode = &status
	}
	return snapshot
}

// httpHeadersToMap 把 http.Header 摊成 map[string][]string（SanitizeHeaders 的入参形态）。
func httpHeadersToMap(headers http.Header) map[string][]string {
	if headers == nil {
		return nil
	}
	result := make(map[string][]string, len(headers))
	for name, values := range headers {
		result[name] = values
	}
	return result
}
