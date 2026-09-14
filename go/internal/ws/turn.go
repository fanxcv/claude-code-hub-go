package ws

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/coder/websocket"
)

// terminalEventTypes 是「本轮结束」的事件类型（与 Node 的 TERMINAL_EVENT_TYPES 同集合）。
var terminalEventTypes = map[string]bool{
	"response.completed":  true,
	"response.failed":     true,
	"response.incomplete": true,
	"error":               true,
}

// processFrame 处理一帧客户端文本消息：除 `response.create` 外的帧只回错误事件、不断连
// （与 Node 一致：只有传输级协议错误才关闭连接）。
func (c *connection) processFrame(data []byte) {
	body, model, hasPreviousResponseID, failure := decodeCreateFrame(data, c.queryModel)
	if failure != nil {
		c.emitError(failure.code, failure.message)
		return
	}

	c.log.Info("ws_request_started", map[string]any{
		"sessionId":             c.sessionID,
		"model":                 model,
		"payloadBytes":          len(data),
		"hasPreviousResponseId": hasPreviousResponseID,
	})

	c.runTurn(body)
}

// frameFailure 是可回错误事件的帧级失败。
type frameFailure struct {
	code    string
	message string
}

// decodeCreateFrame 校验并改写一帧：只接受 `{"type":"response.create", ...}`，
// 去掉 `type`，按需要补 `model`、强制 `stream: true`、丢弃 `background`。
//
// 正文用 `map[string]json.RawMessage` 承载而不是 `map[string]any`：前者逐字段保留原始字节，
// 不会因 float64 往返改写大整数（如 max_output_tokens）或数字精度。
func decodeCreateFrame(data []byte, queryModel string) (body []byte, model string, hasPreviousResponseID bool, failure *frameFailure) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		// 合法 JSON 但不是对象时按 Node 的判定分流：数组归 unsupported_event_type，
		// 标量归 invalid_frame；真正解析失败才是 invalid_json。
		var probe any
		if json.Unmarshal(data, &probe) == nil {
			if _, isArray := probe.([]any); isArray {
				return nil, "", false, &frameFailure{
					code:    "unsupported_event_type",
					message: "Only type=response.create is supported; received: (missing)",
				}
			}
			return nil, "", false, &frameFailure{
				code:    "invalid_frame",
				message: "Frame must be a JSON object",
			}
		}
		return nil, "", false, &frameFailure{
			code:    "invalid_json",
			message: fmt.Sprintf("Invalid JSON frame: %s", err.Error()),
		}
	}
	if fields == nil {
		// JSON `null` 解成 nil map：与「不是对象」同判。
		return nil, "", false, &frameFailure{
			code:    "invalid_frame",
			message: "Frame must be a JSON object",
		}
	}

	var eventType string
	if raw, ok := fields["type"]; ok {
		if err := json.Unmarshal(raw, &eventType); err != nil {
			// 类型字段不是字符串：Node 的 `frame.type !== "response.create"` 同样不成立。
			eventType = ""
		}
	}
	if eventType != "response.create" {
		received := "(missing)"
		if eventType != "" {
			received = eventType
		}
		return nil, "", false, &frameFailure{
			code:    "unsupported_event_type",
			message: fmt.Sprintf("Only type=response.create is supported; received: %s", received),
		}
	}

	delete(fields, "type")

	if queryModel != "" && isAbsentOrEmptyString(fields["model"]) {
		encoded, err := json.Marshal(queryModel)
		if err == nil {
			fields["model"] = encoded
		}
	}
	if raw, ok := fields["model"]; ok {
		_ = json.Unmarshal(raw, &model)
	}

	_, hasPreviousResponseID = fields["previous_response_id"]

	// 强制流式：SSE 是隧道能逐事件翻译成帧的前提；上游侧的传输专用字段由数据面剥掉。
	fields["stream"] = json.RawMessage("true")
	delete(fields, "background")

	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil, "", false, &frameFailure{
			code:    "invalid_frame",
			message: fmt.Sprintf("Frame could not be re-encoded: %s", err.Error()),
		}
	}
	return encoded, model, hasPreviousResponseID, nil
}

// isAbsentOrEmptyString 判定 RawMessage 是否缺失、字面 null 或空字符串
// （Node 只在这三种情况下才用 query 的 model 补位）。
func isAbsentOrEmptyString(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return true
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return true
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		return text == ""
	}
	return false
}

// runTurn 把一轮请求送进数据面并翻译响应；轮内致命失败由 turnWriter 收口。
func (c *connection) runTurn(body []byte) {
	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel()

	request, err := c.tunnelRequest(ctx, body)
	if err != nil {
		c.fatal("internal_request_error", err.Error(), websocket.StatusInternalError, "internal_request_error")
		return
	}

	writer := &turnWriter{ctx: ctx, conn: c, sessionID: c.sessionID, header: make(http.Header)}

	defer func() {
		if recovered := recover(); recovered != nil {
			c.log.Error("ws_turn_panic", map[string]any{
				"sessionId": c.sessionID,
				"error":     fmt.Sprint(recovered),
			})
			c.fatal("internal_error", "Failed to process request", websocket.StatusInternalError, "internal_error")
			return
		}
		writer.finish()
	}()

	c.edge.inner.ServeHTTP(writer, request)
}

// tunnelRequest 构造等价的 `POST /v1/responses`：客户端头照抄（已剥逐跳与内部保留头），
// 加上隧道标记，并把客户端地址带过去。
func (c *connection) tunnelRequest(ctx context.Context, body []byte) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, RoutePath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for name, values := range c.baseHeaders {
		request.Header[name] = append([]string(nil), values...)
	}
	request.Header.Set("accept", "text/event-stream")
	request.Header.Set("content-type", "application/json")
	request.Header.Set(TransportHeader, "websocket")
	request.Header.Set(ForwardFlagHeader, "1")
	request.Header.Set(SessionHeader, c.sessionID)
	if secret := c.edge.secret; secret != "" {
		request.Header.Set(SecretHeader, secret)
	}
	request.ContentLength = int64(len(body))
	request.RemoteAddr = c.remoteAddr
	return request, nil
}

// turnWriter 把数据面处理器写出的响应翻成客户端 WS 帧。
//
// 形态判定只看 content-type（与 Node 同口径）：
//   - `text/event-stream`：按 SSE 事件边界切分，事件块里的 `data:` JSON 原样发一帧；
//   - 其它（含错误 JSON 与上游不支持流式的整体响应）：整体缓冲后发一帧终态事件。
//
// 所有方法在同一个轮次 goroutine 上被调用，故无需额外加锁。
type turnWriter struct {
	ctx       context.Context
	conn      *connection
	sessionID string
	header    http.Header
	status    int
	started   bool
	sse       bool
	splitter  sseSplitter
	jsonBuf   []byte
	// terminal 表示已发出终态事件；连接保持打开，等待下一轮。
	terminal     bool
	terminalType string
	// failed 表示本轮已因致命原因收口（错误帧已发），后续写入一律忽略。
	failed bool
}

// Header 实现 http.ResponseWriter。
func (t *turnWriter) Header() http.Header { return t.header }

// WriteHeader 实现 http.ResponseWriter：记录状态并据 content-type 固定本轮的翻译形态。
func (t *turnWriter) WriteHeader(status int) {
	if t.started {
		return
	}
	t.started = true
	t.status = status
	t.sse = strings.Contains(strings.ToLower(t.header.Get("Content-Type")), "text/event-stream")
}

// Write 实现 http.ResponseWriter。
func (t *turnWriter) Write(payload []byte) (int, error) {
	if !t.started {
		t.WriteHeader(http.StatusOK)
	}
	if t.failed {
		// 已收口：吞掉后续字节但不报错，避免数据面把本轮的收尾写成额外的失败路径。
		return len(payload), nil
	}
	if t.sse {
		for _, event := range t.splitter.push(payload) {
			if !t.sendEvent(event) {
				return len(payload), nil
			}
		}
		if len(t.splitter.pending()) > maxFrameBytes {
			t.fail("internal_sse_event_too_large", "Internal SSE event exceeded the WebSocket response limit")
		}
		return len(payload), nil
	}
	if len(t.jsonBuf)+len(payload) > maxFrameBytes {
		t.fail("internal_response_too_large", "Internal JSON response exceeded the WebSocket response limit")
		return len(payload), nil
	}
	t.jsonBuf = append(t.jsonBuf, payload...)
	return len(payload), nil
}

// Flush 实现 http.Flusher：本实现按事件边界即时发帧，无需额外动作；
// 实现它是为了数据面的流式路径不因「响应写者不支持 flush」而改变缓冲策略。
func (t *turnWriter) Flush() {}

// finish 在数据面处理器返回后收口本轮。
func (t *turnWriter) finish() {
	if !t.started {
		t.WriteHeader(http.StatusOK)
	}
	if t.failed {
		return
	}
	if t.sse {
		if pending := t.splitter.takePending(); len(pending) > 0 {
			// 收尾时残留的尾块按 Node 的做法补一个分隔后当作最后一个事件处理。
			if !t.sendEvent(pending) {
				return
			}
		}
		if !t.terminal {
			t.fail("stream_ended_without_terminal", "Upstream stream ended before emitting a terminal response event")
		}
		return
	}

	// 非 SSE：整体作为一帧终态事件。HTTP 错误映射到 error 帧，其余映射到 response.completed，
	// 二者的区别与 Node 完全相同——这让「鉴权失败」这类语义在 WS 上仍然可读。
	if t.status >= 400 {
		t.sendFrame(t.errorFrameFromJSONBody())
		return
	}
	t.sendFrame(t.completedFrameFromJSONBody())
}

// sendEvent 处理一个 SSE 事件块。
func (t *turnWriter) sendEvent(event []byte) bool {
	data := sseEventData(event)
	if data == nil {
		// 只有注释或字段名的事件（如心跳）不翻译成帧：Node 同样跳过。
		return true
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		if !t.terminal {
			// 有的上游以 [DONE] 收尾而没有 response.completed：补一个终态事件，
			// 否则客户端会以为本轮永远没有结束。
			if !t.sendFrame([]byte(`{"type":"response.completed","response":null}`)) {
				return false
			}
			t.markTerminal("response.completed")
		}
		return true
	}

	if !json.Valid(data) {
		// 不是 JSON 的 data：Node 原样翻成一个文本增量事件。
		frame, err := json.Marshal(map[string]string{"type": "response.output_text.delta", "delta": string(data)})
		if err != nil {
			return true
		}
		return t.sendFrame(frame)
	}

	var probe struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(data, &probe)

	// data 本身已是合法 JSON 文本，直接作为帧发出（客户端同样会 JSON.parse）：
	// 这样上游的字段值逐字节保留，不经过一次解析-再编码的改写。
	if !t.sendFrame(data) {
		return false
	}
	if terminalEventTypes[probe.Type] {
		t.markTerminal(probe.Type)
	}
	return true
}

// markTerminal 记录终态事件。
func (t *turnWriter) markTerminal(eventType string) {
	t.terminal = true
	t.terminalType = eventType
	t.conn.log.Info("ws_terminal_event_sent", map[string]any{
		"sessionId": t.sessionID,
		"type":      eventType,
		"source":    "sse",
	})
}

// sendFrame 写一个文本帧；写入失败即视为客户端不可达，本轮收口。
func (t *turnWriter) sendFrame(payload []byte) bool {
	if t.failed || len(payload) == 0 {
		return !t.failed
	}
	if len(payload) > maxFrameBytes {
		t.fail("internal_sse_event_too_large", "Internal SSE event exceeded the WebSocket response limit")
		return false
	}
	if err := t.conn.writeFrame(t.ctx, payload); err != nil {
		t.conn.log.Warn("ws_send_failed", map[string]any{
			"reason": "outbound_send_error",
			"error":  err.Error(),
		})
		t.failed = true
		// 写失败说明连接已不可用：取消连接上下文，让在途上游与结算按客户端中断语义收尾。
		t.conn.cancel()
		return false
	}
	return true
}

// fail 发一帧错误事件后按 1011 关闭连接（Node 的 sendFatalError 同语义）。
func (t *turnWriter) fail(code, message string) {
	if t.failed {
		return
	}
	t.failed = true
	t.conn.emitError(code, message)
	t.conn.log.Info("ws_turn_failed", map[string]any{"sessionId": t.sessionID, "code": code})
	t.conn.closeWith(websocket.StatusInternalError, code)
}

// errorFrameFromJSONBody 把非 SSE 的错误响应体翻成 error 帧（与 Node 同字段）。
func (t *turnWriter) errorFrameFromJSONBody() []byte {
	text := t.jsonBuf
	frame := map[string]any{"type": "error", "status": t.status}

	var parsed struct {
		Error json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(text, &parsed); err == nil && len(bytes.TrimSpace(parsed.Error)) > 0 {
		frame["error"] = json.RawMessage(parsed.Error)
	} else {
		frame["error"] = map[string]string{
			"code":    fmt.Sprintf("http_%d", t.status),
			"message": truncateText(string(text), 512),
		}
	}
	encoded, err := json.Marshal(frame)
	if err != nil {
		t.conn.log.Error("ws_error_frame_marshal_failed", map[string]any{"error": err.Error()})
		return nil
	}
	t.conn.log.Info("ws_terminal_event_sent", map[string]any{
		"sessionId": t.sessionID,
		"type":      "error",
		"source":    "json",
		"status":    t.status,
	})
	return encoded
}

// completedFrameFromJSONBody 把非 SSE 的成功响应体翻成 response.completed 帧。
func (t *turnWriter) completedFrameFromJSONBody() []byte {
	text := t.jsonBuf
	var response json.RawMessage
	if json.Valid(text) && len(bytes.TrimSpace(text)) > 0 {
		response = json.RawMessage(text)
	} else {
		encoded, err := json.Marshal(map[string]string{"raw": string(text)})
		if err != nil {
			return nil
		}
		response = encoded
	}
	frame, err := json.Marshal(map[string]any{"type": "response.completed", "response": response})
	if err != nil {
		return nil
	}
	t.markTerminal("response.completed")
	return frame
}

// truncateText 按字节截断错误文本；截断点可能落在多字节字符中间，故退到合法边界。
func truncateText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	trimmed := text[:limit]
	for len(trimmed) > 0 && !isValidUTF8Boundary(trimmed) {
		trimmed = trimmed[:len(trimmed)-1]
	}
	return trimmed
}

// isValidUTF8Boundary 报告字符串是否以完整的 UTF-8 字符结尾。
func isValidUTF8Boundary(text string) bool {
	for index := len(text) - 1; index >= 0 && index > len(text)-4; index-- {
		if text[index]&0xc0 != 0x80 {
			return len(text)-index <= 4
		}
	}
	return true
}
