package upws

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// 本文件钉住「上游 WS 帧 → SSE 字节」的翻译面。
//
// 为什么必须逐字节断言：下游（门控、协议转换、结算）认的是 `data: <json>\n\n` 这个形态，
// 形态错了不会在编译期或类型层面暴露——只会在生产上表现为「客户端收不到任何事件」。
// 故断言打在**字节**上。

func TestCreateFrameSplicesTypeWithoutReencoding(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","input":[{"role":"user","content":"你好"}],"stream":true}`)
	frame, err := createFrame(body)
	if err != nil {
		t.Fatalf("createFrame 失败: %v", err)
	}
	want := `{"type":"response.create","model":"gpt-5-codex","input":[{"role":"user","content":"你好"}],"stream":true}`
	if string(frame) != want {
		t.Fatalf("首帧不符合预期：\n得到 %s\n期望 %s", frame, want)
	}
}

func TestCreateFrameHandlesEmptyAndTypeAlreadyPresent(t *testing.T) {
	empty, err := createFrame(nil)
	if err != nil {
		t.Fatalf("空正文应可构造首帧: %v", err)
	}
	if string(empty) != `{"type":"response.create"}` {
		t.Fatalf("空正文首帧错误: %s", empty)
	}

	// 正文自带 type（上游/客户端可能带）：不能拼成 `{"type":"response.create","type":...}`，
	// 必须解码回填，否则第一个键会被后出现的同名字段覆盖。
	withType := []byte(`{"type":"response","model":"gpt-5-codex"}`)
	frame, err := createFrame(withType)
	if err != nil {
		t.Fatalf("带 type 的正文应可构造首帧: %v", err)
	}
	if !strings.Contains(string(frame), `"type":"response.create"`) {
		t.Fatalf("带 type 的正文未被回填: %s", frame)
	}
	if strings.Contains(string(frame), `"type":"response"`) {
		t.Fatalf("带 type 的正文保留了旧值: %s", frame)
	}

	if _, err := createFrame([]byte(`"不是对象"`)); err == nil {
		t.Fatal("非对象正文应报错，不能静默发出非法首帧")
	}
}

func TestIsTerminalEventCoversResponsesTerminals(t *testing.T) {
	terminals := []string{
		`{"type":"response.completed"}`,
		`{"type":"response.failed"}`,
		`{"type":"response.incomplete"}`,
	}
	for _, frame := range terminals {
		if !isTerminalEvent([]byte(frame)) {
			t.Fatalf("终态事件未被识别: %s", frame)
		}
	}
	nonTerminals := []string{
		`{"type":"response.output_text.delta","delta":"嗨"}`,
		`{"type":"codex.rate_limits","x":1}`,
		`不是 JSON`,
	}
	for _, frame := range nonTerminals {
		if isTerminalEvent([]byte(frame)) {
			t.Fatalf("非终态事件被误判为终态: %s", frame)
		}
	}
}

// TestDialWSTranslatesFramesToSSE 是核心用例：假上游按真实形状回帧，断言我们读出的字节。
func TestDialWSTranslatesFramesToSSE(t *testing.T) {
	received := make(chan []byte, 1)
	headers := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Clone()
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		_, first, err := conn.Read(r.Context())
		if err != nil {
			return
		}
		received <- first
		for _, frame := range []string{
			`{"type":"response.created","response":{"id":"resp_1"}}`,
			`{"type":"response.output_text.delta","delta":"嗨"}`,
			`{"type":"response.completed","response":{"usage":{"input_tokens":3,"output_tokens":1}}}`,
		} {
			if err := conn.Write(r.Context(), websocket.MessageText, []byte(frame)); err != nil {
				return
			}
		}
		// 终态后**不关闭**：客户端（我们）应当自己按终态收口。
		time.Sleep(200 * time.Millisecond)
	}))
	defer server.Close()

	dialer := New(Options{
		ConnectTimeout:  2 * time.Second,
		HeadersTimeout:  2 * time.Second,
		BodyIdleTimeout: 2 * time.Second,
		ClientVersion:   "9.9.9",
	})
	outcome := dialer.DialWS(context.Background(), Request{
		EndpointURL: server.URL + "/v1/responses",
		Headers:     http.Header{"Authorization": []string{"Bearer sk-test"}, "Content-Length": []string{"42"}},
		Body:        []byte(`{"model":"gpt-5-codex","stream":true}`),
		SessionID:   "sess-1",
	})
	if outcome.Response == nil {
		t.Fatalf("WS 建连应成功，得到降级原因 %q（err=%v）", outcome.Reason, outcome.Err)
	}
	if !outcome.Attempted {
		t.Fatal("成功路径也必须标 Attempted")
	}
	if got := outcome.Response.StatusCode; got != http.StatusOK {
		t.Fatalf("合成响应状态码应为 200，得到 %d", got)
	}
	if contentType := outcome.Response.Header.Get("Content-Type"); !strings.Contains(contentType, "text/event-stream") {
		t.Fatalf("合成响应必须是 SSE 形态，得到 %q", contentType)
	}

	payload, err := io.ReadAll(outcome.Response.Body)
	if err != nil {
		t.Fatalf("读合成 SSE 失败: %v", err)
	}
	want := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"嗨\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n"
	if string(payload) != want {
		t.Fatalf("合成 SSE 字节不符：\n得到 %q\n期望 %q", payload, want)
	}
	// 终态后必须停：再读一次应当是 EOF（上游还开着连接）。
	buffer := make([]byte, 8)
	if n, err := outcome.Response.Body.Read(buffer); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("终态后应返回 EOF，得到 n=%d err=%v", n, err)
	}
	if err := outcome.Response.Body.Close(); err != nil {
		t.Fatalf("关闭合成响应失败: %v", err)
	}

	// 首帧与握手头：上游看到的必须是 response.create 与 codex 身份头，且不带 HTTP 专用头。
	select {
	case first := <-received:
		if !strings.HasPrefix(string(first), `{"type":"response.create",`) {
			t.Fatalf("首帧未带 response.create: %s", first)
		}
		if !strings.Contains(string(first), `"model":"gpt-5-codex"`) {
			t.Fatalf("首帧丢了请求正文: %s", first)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("上游没收到首帧")
	}
	select {
	case header := <-headers:
		if got := header.Get("OpenAI-Beta"); got != "responses_websockets=2026-02-06" {
			t.Fatalf("缺 beta 头或取值不对: %q", got)
		}
		if got := header.Get("originator"); got != "codex_cli_rs" {
			t.Fatalf("originator 不对: %q", got)
		}
		if got := header.Get("User-Agent"); got != "codex_cli_rs/9.9.9" {
			t.Fatalf("User-Agent 不对: %q", got)
		}
		if got := header.Get("version"); got != "9.9.9" {
			t.Fatalf("version 不对: %q", got)
		}
		if got := header.Get("session-id"); got != "sess-1" {
			t.Fatalf("session-id 不对: %q", got)
		}
		if got := header.Get("thread-id"); got != "sess-1" {
			t.Fatalf("thread-id 应等于 session-id，得到 %q", got)
		}
		if got := header.Get("Authorization"); got != "Bearer sk-test" {
			t.Fatalf("鉴权头应原样带上，得到 %q", got)
		}
		if got := header.Get("Content-Length"); got != "" {
			t.Fatalf("HTTP 专用头 content-length 不该带进握手，得到 %q", got)
		}
		if got := header.Get("Content-Type"); got != "" {
			t.Fatalf("HTTP 专用头 content-type 不该带进握手，得到 %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("上游没收到握手请求")
	}
}

// TestDialWSDowngradesWhenUpgradeRejected 钉住回落语义：握手被拒 → 降级事实 + 短期缓存。
func TestDialWSDowngradesWhenUpgradeRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	now := time.Now()
	dialer := New(Options{
		ConnectTimeout:  2 * time.Second,
		HeadersTimeout:  2 * time.Second,
		BodyIdleTimeout: 2 * time.Second,
		UnsupportedTTL:  time.Minute,
		Now:             func() time.Time { return now },
	})

	outcome := dialer.DialWS(context.Background(), Request{EndpointURL: server.URL + "/v1/responses"})
	if outcome.Response != nil {
		t.Fatal("握手被拒时不应返回响应")
	}
	if !outcome.Attempted {
		t.Fatal("握手被拒属于「尝试过」，必须标 Attempted（调用方据此写降级痕迹）")
	}
	if outcome.Reason != DowngradeUpgradeRejected {
		t.Fatalf("降级原因应为 %q，得到 %q", DowngradeUpgradeRejected, outcome.Reason)
	}
	if dialer.EndpointEligible(server.URL + "/v1/responses") {
		t.Fatal("失败后该端点应进入短期不支持缓存")
	}

	// 缓存命中时**不发起握手**：Attempted 必须是 false，否则会被记成「尝试过但降级」的假账。
	cached := dialer.DialWS(context.Background(), Request{EndpointURL: server.URL + "/v1/responses"})
	if cached.Attempted {
		t.Fatal("缓存命中不该再发起握手")
	}
	if cached.Reason != DowngradeEndpointCached {
		t.Fatalf("缓存命中的原因应为 %q，得到 %q", DowngradeEndpointCached, cached.Reason)
	}

	// 过期后恢复尝试。
	now = now.Add(2 * time.Minute)
	if !dialer.EndpointEligible(server.URL + "/v1/responses") {
		t.Fatal("缓存过期后应恢复尝试")
	}
}

// TestDialWSClientCancelClosesUpstream 钉住取消传播：客户端断开必须中断上游连接。
func TestDialWSClientCancelClosesUpstream(t *testing.T) {
	upstreamClosed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		_, _, err = conn.Read(r.Context())
		if err != nil {
			close(upstreamClosed)
			return
		}
		// 首事件**不发**：让客户端停在读上，等它自己取消。
		for {
			if _, _, err := conn.Read(r.Context()); err != nil {
				close(upstreamClosed)
				return
			}
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	dialer := New(Options{ConnectTimeout: 5 * time.Second, HeadersTimeout: 5 * time.Second, BodyIdleTimeout: 5 * time.Second})

	// 首事件前取消：DialWS 必须返回，不能挂到超时。
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	outcome := dialer.DialWS(ctx, Request{
		EndpointURL: server.URL + "/v1/responses",
		Body:        []byte(`{"model":"gpt-5-codex"}`),
	})
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("取消后 DialWS 应立刻返回，实际耗时 %s", elapsed)
	}
	if outcome.Response != nil {
		t.Fatal("取消后不应返回可用响应")
	}
	if ctx.Err() == nil {
		t.Fatal("测试自身的 ctx 应当是已取消的")
	}
	// 上游连接必须被我们关掉（否则客户端走了、上游还挂着一条连接）。
	select {
	case <-upstreamClosed:
	case <-time.After(3 * time.Second):
		t.Fatal("客户端取消后上游连接未被关闭")
	}
}

func TestWebsocketURLSchemeMapping(t *testing.T) {
	cases := map[string]string{
		"http://example.com/v1/responses":                 "ws://example.com/v1/responses",
		"https://api.openai.com/v1/responses":             "wss://api.openai.com/v1/responses",
		"https://chatgpt.com/backend-api/codex/responses": "wss://chatgpt.com/backend-api/codex/responses",
		"wss://example.com/x":                             "wss://example.com/x",
	}
	for input, want := range cases {
		got, err := websocketURL(input)
		if err != nil {
			t.Fatalf("websocketURL(%q) 失败: %v", input, err)
		}
		if got != want {
			t.Fatalf("websocketURL(%q) = %q，期望 %q", input, got, want)
		}
	}
	if _, err := websocketURL("ftp://example.com/x"); err == nil {
		t.Fatal("不支持的 scheme 应报错")
	}
}
