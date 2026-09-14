package ws

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// 本文件钉住四条主路径（成功往返、鉴权失败、上游提前收尾、客户端中断）与帧级协议错误。
// Inner 用假数据面，故不依赖数据库：这里检验的是「帧 ↔ 数据面请求/响应」的翻译与收口语义。

// recordingInner 是假数据面：记录每轮隧道请求，按 handler 作答。
type recordingInner struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   []string
	handler  func(writer http.ResponseWriter, request *http.Request)
}

func (r *recordingInner) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	body, _ := io.ReadAll(request.Body)
	r.mu.Lock()
	r.requests = append(r.requests, request.Clone(context.Background()))
	r.bodies = append(r.bodies, string(body))
	handler := r.handler
	r.mu.Unlock()
	handler(writer, request)
}

// recordedHeaders 返回第 index 轮隧道请求的头。
func (r *recordingInner) recordedHeaders(index int) http.Header {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.requests[index].Header
}

// recordedBody 返回第 index 轮隧道请求的正文。
func (r *recordingInner) recordedBody(index int) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bodies[index]
}

// turnCount 返回已收到的隧道请求数。
func (r *recordingInner) turnCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.requests)
}

// startEdge 起一个只挂了边缘处理器的测试服务器。
func startEdge(t *testing.T, inner http.Handler) *httptest.Server {
	t.Helper()
	edge := New(Options{Inner: inner, Logger: logx.New(nil), Secret: "test-secret"})
	server := httptest.NewServer(edge)
	t.Cleanup(server.Close)
	return server
}

// dialEdge 建立一条客户端连接。
func dialEdge(t *testing.T, server *httptest.Server) (*websocket.Conn, context.Context) {
	t.Helper()
	return dialEdgePath(t, server, RoutePath)
}

// dialEdgePath 按指定路径建立客户端连接。
func dialEdgePath(t *testing.T, server *httptest.Server, path string) (*websocket.Conn, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	url := "ws" + strings.TrimPrefix(server.URL, "http") + path
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("建立 WebSocket 连接失败: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn, ctx
}

// readJSONFrame 读一帧并解析；读不到即失败。
func readJSONFrame(t *testing.T, ctx context.Context, conn *websocket.Conn) map[string]any {
	t.Helper()
	messageType, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("读取帧失败: %v", err)
	}
	if messageType != websocket.MessageText {
		t.Fatalf("期望文本帧，实得 %v", messageType)
	}
	frame := map[string]any{}
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("帧不是 JSON 对象: %v（原文 %s）", err, data)
	}
	return frame
}

// sendCreateFrame 发一帧 response.create。
func sendCreateFrame(t *testing.T, ctx context.Context, conn *websocket.Conn, body string) {
	t.Helper()
	if err := conn.Write(ctx, websocket.MessageText, []byte(body)); err != nil {
		t.Fatalf("写帧失败: %v", err)
	}
}

// sseInner 是一个按 SSE 作答的假数据面。
func sseInner(events ...string) *recordingInner {
	return &recordingInner{handler: func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("content-type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		for _, event := range events {
			if _, err := io.WriteString(writer, event); err != nil {
				return
			}
		}
	}}
}

const terminalEvent = "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"

func TestEdgeStreamsSSEEventsAndKeepsConnectionForNextTurn(t *testing.T) {
	inner := sseInner(
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n",
		terminalEvent,
	)
	server := startEdge(t, inner)
	conn, ctx := dialEdge(t, server)

	sendCreateFrame(t, ctx, conn, `{"type":"response.create","model":"gpt-5-codex","background":true}`)

	delta := readJSONFrame(t, ctx, conn)
	if delta["type"] != "response.output_text.delta" || delta["delta"] != "hello" {
		t.Fatalf("首个事件不符: %#v", delta)
	}
	completed := readJSONFrame(t, ctx, conn)
	if completed["type"] != "response.completed" {
		t.Fatalf("第二个事件应为终态: %#v", completed)
	}

	// 终态之后连接保持打开，可继续下一轮（协议的持久连接语义）。
	sendCreateFrame(t, ctx, conn, `{"type":"response.create","model":"gpt-5-codex"}`)
	if second := readJSONFrame(t, ctx, conn); second["type"] != "response.output_text.delta" {
		t.Fatalf("第二轮首个事件不符: %#v", second)
	}
	if second := readJSONFrame(t, ctx, conn); second["type"] != "response.completed" {
		t.Fatalf("第二轮终态不符: %#v", second)
	}
	if inner.turnCount() != 2 {
		t.Fatalf("期望两轮隧道请求，实得 %d", inner.turnCount())
	}

	// 隧道请求的契约：标记头齐全、方法路径正确、正文被改写。
	headers := inner.recordedHeaders(0)
	for name, want := range map[string]string{
		TransportHeader:   "websocket",
		ForwardFlagHeader: "1",
		SecretHeader:      "test-secret",
		"accept":          "text/event-stream",
		"content-type":    "application/json",
	} {
		if got := headers.Get(name); got != want {
			t.Fatalf("隧道头 %s 期望 %q 实得 %q", name, want, got)
		}
	}
	if headers.Get(SessionHeader) == "" {
		t.Fatal("隧道请求缺少会话 id 头")
	}
	if first, second := headers.Get(SessionHeader), inner.recordedHeaders(1).Get(SessionHeader); first != second {
		t.Fatalf("同一连接的两轮应共享会话 id: %q vs %q", first, second)
	}
	if headers.Get("x-cch-client-transport") != "websocket" {
		t.Fatal("隧道请求应声明来自 WebSocket 客户端")
	}

	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(inner.recordedBody(0)), &body); err != nil {
		t.Fatalf("隧道正文不是 JSON: %v", err)
	}
	if _, exists := body["type"]; exists {
		t.Fatal("隧道正文不应带 type 字段（那是帧标记，不是请求字段）")
	}
	if _, exists := body["background"]; exists {
		t.Fatal("隧道正文应丢弃 background")
	}
	if string(body["stream"]) != "true" {
		t.Fatalf("隧道正文必须强制 stream=true，实得 %s", body["stream"])
	}
	if string(body["model"]) != `"gpt-5-codex"` {
		t.Fatalf("model 应原样透传: %s", body["model"])
	}
}

func TestEdgeMapsHTTPErrorBodyToErrorFrameAndKeepsConnection(t *testing.T) {
	inner := &recordingInner{handler: func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("content-type", "application/json")
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(writer, `{"error":{"message":"Invalid API key","type":"invalid_request_error"}}`)
	}}
	server := startEdge(t, inner)
	conn, ctx := dialEdge(t, server)

	sendCreateFrame(t, ctx, conn, `{"type":"response.create","model":"m"}`)

	frame := readJSONFrame(t, ctx, conn)
	if frame["type"] != "error" {
		t.Fatalf("鉴权失败应翻成 error 帧: %#v", frame)
	}
	if status, ok := frame["status"].(float64); !ok || int(status) != http.StatusUnauthorized {
		t.Fatalf("error 帧应带 HTTP 状态: %#v", frame)
	}
	errorBody, ok := frame["error"].(map[string]any)
	if !ok || errorBody["message"] != "Invalid API key" {
		t.Fatalf("error 帧应原样带上游 error 体: %#v", frame)
	}

	// Node 语义：HTTP 错误不是传输级失败，连接保持打开，客户端可换密钥重试。
	sendCreateFrame(t, ctx, conn, `{"type":"response.create","model":"m"}`)
	if again := readJSONFrame(t, ctx, conn); again["type"] != "error" {
		t.Fatalf("第二轮应仍收到 error 帧: %#v", again)
	}
	if inner.turnCount() != 2 {
		t.Fatalf("期望两轮隧道请求，实得 %d", inner.turnCount())
	}
}

func TestEdgeFailsTurnWhenStreamEndsWithoutTerminalEvent(t *testing.T) {
	inner := sseInner("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n")
	server := startEdge(t, inner)
	conn, ctx := dialEdge(t, server)

	sendCreateFrame(t, ctx, conn, `{"type":"response.create","model":"m"}`)

	if frame := readJSONFrame(t, ctx, conn); frame["type"] != "response.output_text.delta" {
		t.Fatalf("首个事件不符: %#v", frame)
	}
	frame := readJSONFrame(t, ctx, conn)
	if frame["type"] != "error" {
		t.Fatalf("缺终态事件应翻成 error 帧: %#v", frame)
	}
	errorBody, ok := frame["error"].(map[string]any)
	if !ok || errorBody["code"] != "stream_ended_without_terminal" {
		t.Fatalf("错误码应为 stream_ended_without_terminal: %#v", frame)
	}

	// 这是传输级协议失败：必须按 1011 关闭，客户端不能继续等下一帧。
	_, _, err := conn.Read(ctx)
	var closeError websocket.CloseError
	if !errors.As(err, &closeError) || closeError.Code != websocket.StatusInternalError {
		t.Fatalf("期望 1011 关闭，实得 %v", err)
	}
	if !strings.Contains(closeError.Reason, "stream_ended_without_terminal") {
		t.Fatalf("关闭原因应说明缺终态: %q", closeError.Reason)
	}
}

func TestEdgeCancelsTurnWhenClientDisconnects(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	var once sync.Once
	inner := &recordingInner{handler: func(_ http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
		once.Do(func() { close(canceled) })
	}}
	server := startEdge(t, inner)
	conn, ctx := dialEdge(t, server)

	sendCreateFrame(t, ctx, conn, `{"type":"response.create","model":"m"}`)
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("数据面没有收到隧道请求")
	}

	// 客户端硬断（不发关闭帧）：在途轮次必须被取消，否则上游与供应商侧仍在计费。
	_ = conn.CloseNow()

	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("客户端断开后轮次上下文未被取消")
	}
}

func TestEdgeRejectsMalformedFramesWithoutClosingConnection(t *testing.T) {
	inner := sseInner(terminalEvent)
	server := startEdge(t, inner)
	conn, ctx := dialEdge(t, server)

	cases := []struct {
		name     string
		frame    string
		wantCode string
	}{
		{name: "非 JSON", frame: "not json at all", wantCode: "invalid_json"},
		{name: "非对象", frame: `"just a string"`, wantCode: "invalid_frame"},
		{name: "数组", frame: `[1,2,3]`, wantCode: "unsupported_event_type"},
		{name: "缺 type", frame: `{"model":"m"}`, wantCode: "unsupported_event_type"},
		{name: "其它 type", frame: `{"type":"response.cancel"}`, wantCode: "unsupported_event_type"},
	}
	for _, testCase := range cases {
		sendCreateFrame(t, ctx, conn, testCase.frame)
		frame := readJSONFrame(t, ctx, conn)
		if frame["type"] != "error" {
			t.Fatalf("%s: 应回 error 帧，实得 %#v", testCase.name, frame)
		}
		errorBody, ok := frame["error"].(map[string]any)
		if !ok || errorBody["code"] != testCase.wantCode {
			t.Fatalf("%s: 错误码期望 %s 实得 %#v", testCase.name, testCase.wantCode, frame)
		}
	}

	// 这些帧级错误都不是传输失败：连接必须还活着，且可用帧仍能正常成轮。
	if inner.turnCount() != 0 {
		t.Fatalf("畸形帧不得进入数据面，实得 %d 轮", inner.turnCount())
	}
	sendCreateFrame(t, ctx, conn, `{"type":"response.create"}`)
	if frame := readJSONFrame(t, ctx, conn); frame["type"] != "response.completed" {
		t.Fatalf("畸形帧之后的正常帧应照常成轮: %#v", frame)
	}
}

func TestEdgeClosesOnBinaryFrame(t *testing.T) {
	inner := sseInner(terminalEvent)
	server := startEdge(t, inner)
	conn, ctx := dialEdge(t, server)

	if err := conn.Write(ctx, websocket.MessageBinary, []byte{0x01, 0x02}); err != nil {
		t.Fatalf("写二进制帧失败: %v", err)
	}
	frame := readJSONFrame(t, ctx, conn)
	if frame["type"] != "error" {
		t.Fatalf("二进制帧应回 error 帧: %#v", frame)
	}
	errorBody, _ := frame["error"].(map[string]any)
	if errorBody["code"] != "invalid_frame_type" {
		t.Fatalf("错误码应为 invalid_frame_type: %#v", frame)
	}
	_, _, err := conn.Read(ctx)
	var closeError websocket.CloseError
	if !errors.As(err, &closeError) || closeError.Code != websocket.StatusUnsupportedData {
		t.Fatalf("期望 1003 关闭，实得 %v", err)
	}
}

func TestEdgePassesNonUpgradeTrafficToInner(t *testing.T) {
	inner := &recordingInner{handler: func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(writer, `{"ok":true}`)
	}}
	server := startEdge(t, inner)

	// 不带 upgrade 头的普通 POST 必须原样落到数据面（WS 通道不改变 HTTP 路径）。
	response, err := http.Post(server.URL+RoutePath, "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("普通 POST 失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || string(body) != `{"ok":true}` {
		t.Fatalf("普通请求应透传数据面：status=%d body=%s", response.StatusCode, body)
	}
}

func TestSplitterHandlesChunkBoundariesAndBothLineEndings(t *testing.T) {
	splitter := &sseSplitter{}
	// 事件边界跨 chunk 截断：只有补齐分隔符后才允许成块。
	if events := splitter.push([]byte("data: {\"a\":1}\n")); len(events) != 0 {
		t.Fatalf("半个分隔符不应成块: %#v", events)
	}
	events := splitter.push([]byte("\ndata: {\"b\":2}\r\n\r\n"))
	if len(events) != 2 {
		t.Fatalf("期望两个事件，实得 %d", len(events))
	}
	if string(sseEventData(events[0])) != `{"a":1}` || string(sseEventData(events[1])) != `{"b":2}` {
		t.Fatalf("事件数据提取有误: %q / %q", sseEventData(events[0]), sseEventData(events[1]))
	}
	if pending := splitter.takePending(); len(pending) != 0 {
		t.Fatalf("无残留时 tail 应为空，实得 %q", pending)
	}
}
