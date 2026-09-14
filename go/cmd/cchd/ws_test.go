package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ws"
)

// 本文件钉住「WS 与同路径 HTTP 归属一致」这条语义：白名单只写 POST /v1/responses 时，
// GET 升级请求也必须跟着 POST 的归属走（否则 HTTP 与 WS 会分裂到两个后端）。

// wsRecordingHandler 记录收到的请求，并对 /v1/responses 的 POST 回一段 SSE。
type wsRecordingHandler struct {
	mu       sync.Mutex
	requests []*http.Request
	bodies   []string
}

func (h *wsRecordingHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	body, _ := io.ReadAll(request.Body)
	h.mu.Lock()
	h.requests = append(h.requests, request.Clone(context.Background()))
	h.bodies = append(h.bodies, string(body))
	h.mu.Unlock()

	if request.Method == http.MethodPost && request.URL.Path == ws.RoutePath {
		writer.Header().Set("content-type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(writer,
			"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
		return
	}
	writer.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(writer, `{"handled":true}`)
}

func (h *wsRecordingHandler) last() (*http.Request, string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.requests) == 0 {
		return nil, ""
	}
	return h.requests[len(h.requests)-1], h.bodies[len(h.bodies)-1]
}

func (h *wsRecordingHandler) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.requests)
}

func TestWebSocketEdgeServesUpgradeOnRoutePath(t *testing.T) {
	handler := &wsRecordingHandler{}
	// WS 与同路径 HTTP 同归本进程：升级请求必须由边缘处理器接管（历史上曾因归属白名单只写
	// POST 而被判给 Node，那时本用例钉的是「判给 Go 时能接管」）。
	server := httptest.NewServer(newWebSocketEdge(handler, logx.New(nil)))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+ws.RoutePath, nil)
	if err != nil {
		t.Fatalf("升级请求应被边缘处理器接管: %v", err)
	}
	defer func() { _ = conn.CloseNow() }()

	if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"m"}`)); err != nil {
		t.Fatalf("写帧失败: %v", err)
	}
	_, frame, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("读取帧失败: %v", err)
	}
	parsed := map[string]any{}
	if err := json.Unmarshal(frame, &parsed); err != nil || parsed["type"] != "response.completed" {
		t.Fatalf("应收到终态帧，实得 %s", frame)
	}

	request, body := handler.last()
	if request == nil || request.Method != http.MethodPost {
		t.Fatalf("隧道请求应回到同一处理器: %v", request)
	}
	if request.Header.Get(ws.TransportHeader) != "websocket" {
		t.Fatalf("隧道请求缺少传输标记: %v", request.Header)
	}
	if !strings.Contains(body, `"stream":true`) {
		t.Fatalf("隧道正文应强制流式: %s", body)
	}
}

func TestWebSocketEdgeLeavesOrdinaryTrafficUntouched(t *testing.T) {
	handler := &wsRecordingHandler{}
	server := httptest.NewServer(newWebSocketEdge(handler, logx.New(nil)))
	defer server.Close()

	response, err := http.Post(server.URL+ws.RoutePath, "application/json", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("普通 POST 失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(response.Body)
	if !strings.Contains(string(body), "response.completed") {
		t.Fatalf("普通请求应原样落到数据面: %s", body)
	}
	if request, _ := handler.last(); request == nil || ws.IsUpgrade(request) {
		t.Fatalf("普通请求不应被当成升级: %v", request)
	}
}
