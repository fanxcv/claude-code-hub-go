package forward

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/upws"
)

// 本文件钉住「上游 WS 接缝」在 forward 侧的语义：资格怎么判、走成/走不成各留下什么事实、
// 以及**回落绝不被记成供应商失败**（这是本特性唯一的语义红线）。

func wsTestContext(t *testing.T, clientTransport string) *pctx.Context {
	t.Helper()
	headers := http.Header{}
	if clientTransport != "" {
		headers.Set(wsClientTransportHeader, clientTransport)
	}
	headers.Set(wsSessionHeader, "sess-test")
	pc, err := pctx.New(pctx.Init{Method: http.MethodPost, Path: "/v1/responses", Headers: headers})
	if err != nil {
		t.Fatalf("构造 pctx 失败: %v", err)
	}
	return pc
}

func wsTestPlan(t *testing.T, endpointURL string) *Plan {
	t.Helper()
	return &Plan{
		Method:   http.MethodPost,
		URL:      endpointURL,
		Headers:  http.Header{"Authorization": []string{"Bearer sk-test"}},
		Body:     []byte(`{"model":"gpt-5-codex","stream":true}`),
		Protocol: convert.ProtocolOpenAIResponses,
	}
}

func wsTestDeps(endpointEligible func(context.Context, *pctx.Context, Provider) bool) Deps {
	return Deps{
		Logger:     nil,
		WS:         upws.New(upws.Options{ConnectTimeout: 2 * time.Second, HeadersTimeout: 2 * time.Second, BodyIdleTimeout: 2 * time.Second}),
		WSEligible: endpointEligible,
	}
}

func TestIsWebSocketClientRequestReadsTunnelMarker(t *testing.T) {
	if !IsWebSocketClientRequest(wsTestContext(t, "websocket")) {
		t.Fatal("隧道标记为 websocket 时应判为客户端 WS 请求")
	}
	if IsWebSocketClientRequest(wsTestContext(t, "http")) {
		t.Fatal("隧道标记为 http 时不得判为客户端 WS 请求")
	}
	if IsWebSocketClientRequest(wsTestContext(t, "")) {
		t.Fatal("没有隧道标记时不得判为客户端 WS 请求")
	}
	if IsWebSocketClientRequest(nil) {
		t.Fatal("nil 上下文不得判为客户端 WS 请求")
	}
}

// TestWSAttemptSuccessRecordsConnectedFacts：走成时返回可直接用的响应，并留下 connected 事实。
func TestWSAttemptSuccessRecordsConnectedFacts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if _, _, err := conn.Read(r.Context()); err != nil {
			return
		}
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.output_text.delta","delta":"嗨"}`))
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{}}`))
	}))
	defer server.Close()

	deps := wsTestDeps(func(context.Context, *pctx.Context, Provider) bool { return true })
	outcome := &AttemptOutcome{ProviderID: 7, EndpointID: 9}
	response := deps.wsAttempt(context.Background(), wsTestContext(t, "websocket"),
		Provider{ID: 7, Type: convert.ProviderCodex}, wsTestPlan(t, server.URL+"/v1/responses"), outcome)
	if response == nil {
		t.Fatal("假上游正常回帧时应走成 WS")
	}
	if outcome.WS == nil {
		t.Fatal("走成也必须留事实（否则界面无法区分「走了 WS」与「压根没走」）")
	}
	if !outcome.WS.Attempted || !outcome.WS.Connected {
		t.Fatalf("事实应为 attempted+connected，得到 %+v", outcome.WS)
	}
	if outcome.WS.DowngradedToHTTP {
		t.Fatal("走成时不得标降级")
	}
	if outcome.WS.ClientTransport != "websocket" {
		t.Fatalf("clientTransport 应为 websocket，得到 %q", outcome.WS.ClientTransport)
	}
	_ = response.Body.Close()
}

// TestWSAttemptDowngradeRecordsReasonWithoutFailure：走不成时返回 nil（调用方继续走 HTTP），
// 并留下降级原因——这是「不计熔断」的落点：本函数**没有任何**记失败的机会。
func TestWSAttemptDowngradeRecordsReasonWithoutFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	failures := 0
	deps := wsTestDeps(func(context.Context, *pctx.Context, Provider) bool { return true })
	deps.RecordFailure = func(context.Context, *Failure) { failures++ }

	outcome := &AttemptOutcome{ProviderID: 7, EndpointID: 9}
	response := deps.wsAttempt(context.Background(), wsTestContext(t, "websocket"),
		Provider{ID: 7, Type: convert.ProviderCodex}, wsTestPlan(t, server.URL+"/v1/responses"), outcome)
	if response != nil {
		t.Fatal("握手被拒时不得返回响应")
	}
	if outcome.WS == nil || !outcome.WS.Attempted || !outcome.WS.DowngradedToHTTP {
		t.Fatalf("应留下 attempted+downgraded 事实，得到 %+v", outcome.WS)
	}
	if outcome.WS.DowngradeReason != string(upws.DowngradeUpgradeRejected) {
		t.Fatalf("降级原因应为 %q，得到 %q", upws.DowngradeUpgradeRejected, outcome.WS.DowngradeReason)
	}
	if outcome.WS.Connected {
		t.Fatal("没连上就不该标 connected")
	}
	if failures != 0 {
		t.Fatalf("回落不得计入熔断失败，实际记了 %d 次", failures)
	}
}

// TestWSAttemptSkipsWhenNotEligible：三条资格任一不真都不碰 WS，且**不留任何痕迹**
// （与接入前逐字一致：没有痕迹就没有噪声）。
func TestWSAttemptSkipsWhenNotEligible(t *testing.T) {
	outcome := &AttemptOutcome{ProviderID: 7}
	deps := wsTestDeps(func(context.Context, *pctx.Context, Provider) bool { return false })
	response := deps.wsAttempt(context.Background(), wsTestContext(t, "websocket"),
		Provider{ID: 7, Type: convert.ProviderCodex}, wsTestPlan(t, "http://127.0.0.1:1/v1/responses"), outcome)
	if response != nil {
		t.Fatal("资格不成立时不得返回响应")
	}
	if outcome.WS != nil {
		t.Fatalf("资格不成立时不得留痕迹，得到 %+v", outcome.WS)
	}

	// 未接线上游 WS（WS 为 nil）是同一语义：不碰、不留痕。
	bare := Deps{}
	response = bare.wsAttempt(context.Background(), wsTestContext(t, "websocket"),
		Provider{ID: 7, Type: convert.ProviderCodex}, wsTestPlan(t, "http://127.0.0.1:1/v1/responses"), outcome)
	if response != nil || outcome.WS != nil {
		t.Fatal("未接线时不得尝试 WS")
	}
}

// TestWSAttemptRequiresResponsesLine：非 Responses 线的正文不得当 response.create 发出去。
func TestWSAttemptRequiresResponsesLine(t *testing.T) {
	deps := wsTestDeps(func(context.Context, *pctx.Context, Provider) bool { return true })
	outcome := &AttemptOutcome{ProviderID: 7}
	plan := wsTestPlan(t, "http://127.0.0.1:1/v1/responses")
	plan.Protocol = convert.ProtocolOpenAIChat
	if response := deps.wsAttempt(context.Background(), wsTestContext(t, "websocket"),
		Provider{ID: 7, Type: convert.ProviderCodex}, plan, outcome); response != nil || outcome.WS != nil {
		t.Fatal("非 Responses 线不得走上游 WS")
	}
}

// TestNewUUIDv4Shape 钉住会话 id 的形状（上游拿它做粘性，形状错了对端可能拒）。
func TestNewUUIDv4Shape(t *testing.T) {
	id := newUUIDv4()
	if len(id) != 36 || strings.Count(id, "-") != 4 {
		t.Fatalf("UUID v4 形状不对: %q", id)
	}
	if id[14] != '4' {
		t.Fatalf("版本位应为 4: %q", id)
	}
}

// TestExecuteStreamAttemptGatesUpstreamWSFrames 是上游 WS 的**端到端接缝**用例：
// 假上游按真实形状回帧 → 走 forward 的流式尝试（含门控）→ 读出的字节必须与上游事件逐字一致。
//
// 为什么要有它：单测分别验过「帧→SSE」与「资格→降级」，但两者之间还隔着门控与流式提交。
// 只有把这条链跑通，才能说「客户端可见协议不变」——门控若把 WS 合成的事件认成非法形态，
// 表现会是「WS 走成了但客户端收不到内容」，那是比回落更糟的失败。
func TestExecuteStreamAttemptGatesUpstreamWSFrames(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if _, _, err := conn.Read(r.Context()); err != nil {
			return
		}
		for _, frame := range []string{
			`{"type":"response.created","response":{"id":"resp_1"}}`,
			`{"type":"response.output_text.delta","delta":"嗨"}`,
			`{"type":"response.completed","response":{"usage":{"input_tokens":3,"output_tokens":1}}}`,
		} {
			if err := conn.Write(r.Context(), websocket.MessageText, []byte(frame)); err != nil {
				return
			}
		}
		// 终态后保持连接开着：收口必须由我们按终态事件完成，不能依赖对端关闭。
		time.Sleep(300 * time.Millisecond)
	}))
	defer server.Close()

	deps := wsTestDeps(func(context.Context, *pctx.Context, Provider) bool { return true })
	plan := wsTestPlan(t, server.URL+"/v1/responses")
	plan.ClientStream = true
	outcome := &AttemptOutcome{ProviderID: 7, EndpointID: 9}

	response, failure := deps.executeStreamAttempt(context.Background(), wsTestContext(t, "websocket"),
		Provider{ID: 7, Type: convert.ProviderCodex}, plan, outcome,
		StreamOptions{Format: convert.FormatOpenAI, Now: time.Now})
	if failure != nil {
		t.Fatalf("WS 尝试不应失败: %+v", failure)
	}
	if response == nil || response.Stream == nil {
		t.Fatal("WS 响应应作为流提交")
	}
	// 合成响应的状态码是 200（WS 交换已完成）；尝试条目的 StatusCode 由 forwardLoop 填，
	// 本用例直接调 executeStreamAttempt，故这里断言响应侧而不是条目侧。
	if response.StatusCode != 200 {
		t.Fatalf("合成响应状态码应为 200，得到 %d", response.StatusCode)
	}
	if outcome.WS == nil || !outcome.WS.Connected {
		t.Fatalf("应留下 connected 事实，得到 %+v", outcome.WS)
	}

	var received []byte
	for _, chunk := range response.Stream.Prefix {
		received = append(received, chunk...)
	}
	buffer := make([]byte, 4096)
	for {
		n, err := response.Stream.Source.Read(buffer)
		received = append(received, buffer[:n]...)
		if err != nil {
			break
		}
	}
	want := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"嗨\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n"
	if string(received) != want {
		t.Fatalf("门控后的字节与上游事件不一致：\n得到 %q\n期望 %q", received, want)
	}
	if err := response.Stream.Source.Close(); err != nil {
		t.Fatalf("关闭上游流失败: %v", err)
	}
}
