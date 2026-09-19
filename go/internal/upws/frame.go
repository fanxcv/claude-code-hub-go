package upws

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/coder/websocket"
)

// ErrBinaryFrame 表示上游回传了 binary 帧。Responses WS 的事件面只走文本帧
// （openai/codex 的读循环按文本帧解析，binary 帧不被处理），故这里显式报错而不是
// 猜测其内容——静默丢弃一个 binary 帧等于把上游的异常形态吞掉。
var ErrBinaryFrame = errors.New("upws: 上游回传 binary 帧，Responses WS 只发文本帧")

// SSE 合成用的分隔符。既有 SSE 管线认 `data: <json>\n\n`，与 HTTP 路径逐字同形，
// 故下游（门控、协议转换、结算）无需知道这些字节来自 WS 而非 TCP 上的 HTTP 响应体。
const (
	ssePrefix = "data: "
	sseSuffix = "\n\n"
)

// responseCreateType 是首帧的类型值（上游要求客户端首帧为 response.create）。
const responseCreateType = "response.create"

// responseCreatePrefix 是首帧的类型前缀（`{"type":"response.create",`）。
const responseCreatePrefix = `{"type":"response.create",`

// createFrame 把 Responses create body 包成上游 WS 要求的首帧。
//
// 实现选择：**字节拼接**而不是「解码成 map 再编码」，理由有两条：
//  1. 不额外复制/重排请求正文——正文可能很大（长会话），而走上游 WS 的全部意义就是省延迟；
//  2. 拼接结果必须仍是合法的 Responses create body，即**第一个键只能是 type**。
//
// 因此只有「正文不是对象」与「正文第一个键已是 type」这两种边界走解码-回填-编码兜底，
// 正常路径零解码。
func createFrame(body []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return []byte(`{"type":"` + responseCreateType + `"}`), nil
	}
	if trimmed[0] != '{' {
		return nil, fmt.Errorf("upws: 首个 WS 帧的正文不是 JSON 对象（首字节 %q）", trimmed[0])
	}
	if len(trimmed) <= 2 {
		return []byte(`{"type":"` + responseCreateType + `"}`), nil
	}
	if bytes.HasPrefix(trimmed[1:], []byte(`"type"`)) {
		// 正文自己声明了 type：其余键一个都不能丢，只能解码回填（顺序变化对 JSON 无语义影响）。
		var object map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &object); err != nil {
			return nil, fmt.Errorf("upws: 首个 WS 帧的正文不是合法 JSON 对象: %w", err)
		}
		object["type"] = json.RawMessage(`"` + responseCreateType + `"`)
		encoded, err := json.Marshal(object)
		if err != nil {
			return nil, fmt.Errorf("upws: 首个 WS 帧编码失败: %w", err)
		}
		return encoded, nil
	}
	frame := make([]byte, 0, len(responseCreatePrefix)+len(trimmed))
	frame = append(frame, responseCreatePrefix...)
	frame = append(frame, trimmed[1:]...)
	return frame, nil
}

// isTerminalEvent 判定一帧事件是否收口。终态集合与客户端侧 ws/turn.go 的 terminalEventTypes
// 同集：`response.completed` / `response.failed` / `response.incomplete`。
//
// 解析失败返回 false：非 JSON 的帧照旧透传（管线自行处理），不在这里猜它的语义。
func isTerminalEvent(frame []byte) bool {
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(frame, &envelope); err != nil {
		return false
	}
	switch envelope.Type {
	case "response.completed", "response.failed", "response.incomplete":
		return true
	}
	return false
}

// frameReader 把上游 WS 的文本帧拉平成 SSE 字节流，是 `*dial.Response.Body` 的实现。
//
// 调用方（门控/流式泵）按字节读，本类型按帧读，中间只多一份「单帧的 SSE 形态」缓冲，
// 不复制整条响应。
type frameReader struct {
	conn *websocket.Conn
	// ctx 是请求上下文：它被取消时必须中断在读的帧上（客户端断开 → 中断上游）。
	ctx context.Context
	// stop 撤销「ctx 取消 → 关闭连接」的回调（Close 时调用，避免回调悬挂）。
	stop func() bool

	// headersTimeout 是「发出首帧到首事件」的上限（FETCH_HEADERS_TIMEOUT 语义），prime 用一次。
	headersTimeout time.Duration
	// bodyIdleTimeout 是相邻帧之间的最长间隔（FETCH_BODY_TIMEOUT 语义）。
	bodyIdleTimeout time.Duration

	pending []byte
	// primed 为真表示首事件已读到（headersTimeout 的额度已用掉）。
	primed bool
	// done 为真表示终态事件已产出，后续读取返回 EOF。
	done bool
	// closed 记录连接是否已关闭（Close 幂等）。
	closed bool
}

func (r *frameReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		if r.done {
			return 0, io.EOF
		}
		timeout := r.bodyIdleTimeout
		if !r.primed {
			timeout = r.headersTimeout
		}
		frame, err := r.readFrame(timeout)
		if err != nil {
			return 0, err
		}
		r.primed = true
		r.pending = appendSSEFrame(r.pending[:0], frame)
		if isTerminalEvent(frame) {
			r.done = true
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// prime 读首事件并暂存。它的存在是为了把「握手成功但首事件前就断」变成**建连阶段的失败**：
// 否则这个失败要等客户端已收到字节、门控提交之后才暴露，那时已无法回落到 HTTP。
func (r *frameReader) prime() error {
	if r.primed || r.done {
		return nil
	}
	frame, err := r.readFrame(r.headersTimeout)
	if err != nil {
		return err
	}
	r.primed = true
	r.pending = appendSSEFrame(nil, frame)
	if isTerminalEvent(frame) {
		r.done = true
	}
	return nil
}

func (r *frameReader) readFrame(timeout time.Duration) ([]byte, error) {
	ctx := r.ctx
	cancel := context.CancelFunc(func() {})
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	kind, data, err := r.conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	if kind != websocket.MessageText {
		return nil, ErrBinaryFrame
	}
	return data, nil
}

// Close 立即关闭上游连接（不做关闭握手：正常收口由终态事件决定，异常路径没有对端可握）。
func (r *frameReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	if r.stop != nil {
		r.stop()
	}
	return r.conn.CloseNow()
}

// appendSSEFrame 把一帧事件翻成 SSE 字节追加到 target。
func appendSSEFrame(target, frame []byte) []byte {
	target = append(target, ssePrefix...)
	target = append(target, frame...)
	return append(target, sseSuffix...)
}
