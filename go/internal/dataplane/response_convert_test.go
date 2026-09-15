package dataplane

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/gate"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 响应侧方言回译的验收钉子（不依赖数据库）。
//
// 四条用例来自切换矩阵的原始证据：入站方言与上游
// 方言不同时，客户端必须收到**自己的**方言；同方言时必须逐字节透传（不得引入重框）。

// conversionCandidates 提供「指定类型 + 是否开转换」的候选。
type conversionCandidates struct {
	url          string
	providerType convert.ProviderType
	conversion   bool
}

func (f conversionCandidates) Candidate(
	_ context.Context,
	selection pctx.ProviderSelection,
	_ SelectionFacts,
) (*forward.Candidate, route.Result, error) {
	return &forward.Candidate{
		Provider: forward.Provider{
			ID:   selection.ProviderID,
			Name: selection.Name,
			Type: f.providerType,
			URL:  f.url,
			Key:  "fake-upstream-key",
		},
		ConversionEnabled: f.conversion,
	}, route.Result{}, nil
}

func (f conversionCandidates) Failover(context.Context, SelectionFacts) (*forward.Candidate, route.Result, error) {
	return nil, route.Result{}, nil
}

// newConversionHandler 装配一个「入站任意方言 → 指定类型上游」的数据面。
func newConversionHandler(
	t *testing.T,
	upstreamURL string,
	providerType convert.ProviderType,
	conversion bool,
) http.Handler {
	t.Helper()
	return newConversionHandlerWithPlaceholder(t, upstreamURL, providerType, conversion, false)
}

// newConversionHandlerWithPlaceholder 同上，另可打开占位思考签名
// （CCH_THINKING_SIGNATURE_PLACEHOLDER）；默认装配保持关闭，避免既有用例被隐式改行为。
func newConversionHandlerWithPlaceholder(
	t *testing.T,
	upstreamURL string,
	providerType convert.ProviderType,
	conversion bool,
	placeholderThinkingSignature bool,
) http.Handler {
	t.Helper()
	dialClient, err := dial.New(dial.Options{})
	if err != nil {
		t.Fatalf("拨号器构造失败: %v", err)
	}
	auth := fakeAuth{
		user: guard.User{ID: 1, Name: "甲", IsEnabled: true},
		key:  guard.Key{ID: 2, Name: "k", UserID: 1},
	}
	handler, err := New(Options{
		Logger: logx.New(nil),
		Base: guard.Deps{
			Auth:           auth,
			Users:          auth,
			Settings:       settingsWithUpstreamPassthrough{},
			Sensitive:      fakeEmptySource{},
			Filters:        fakeEmptySource{},
			Provider:       fakeProvider{selection: pctx.ProviderSelection{ProviderID: 7, Name: "假供应商", Type: string(providerType)}},
			MessageContext: &fakeMessageWriter{},
		},
		Candidates: conversionCandidates{url: upstreamURL, providerType: providerType, conversion: conversion},
		Settlers: func(*RequestState) Settler {
			return &fakeSettler{}
		},
		Forward:                      forward.Deps{Dial: dialClient},
		Stream:                       forward.StreamOptions{Budget: gate.DefaultBudget()},
		PlaceholderThinkingSignature: placeholderThinkingSignature,
	})
	if err != nil {
		t.Fatalf("数据面构造失败: %v", err)
	}
	return handler
}

// responsesNonStreamBody 是上游（codex / Responses 方言）的非流式响应。
const responsesNonStreamBody = `{"id":"resp_1","object":"response","status":"completed","model":"gpt-5.6",` +
	`"output":[{"id":"msg_1","type":"message","role":"assistant","status":"completed",` +
	`"content":[{"type":"output_text","text":"你好","annotations":[]}]}],` +
	`"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}`

// TestCrossDialectNonStreamRewritesBody 钉住 `data.messages.nonstream` / `data.chat.nonstream`：
// 入站 Anthropic、上游 Responses，客户端必须收到 Anthropic 形状（而不是上游的 Responses 形状）。
func TestCrossDialectNonStreamRewritesBody(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, responsesNonStreamBody)
	}))
	defer upstream.Close()

	handler := newConversionHandler(t, upstream.URL, convert.ProviderCodex, true)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"gpt-5.6","max_tokens":16,"messages":[{"role":"user","content":"x"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", "fake-client-key")
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，收到 %d（%s）", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, marker := range []string{`"type":"message"`, `"stop_reason":"end_turn"`, `"content":[{"type":"text","text":"你好"}]`} {
		if !strings.Contains(body, marker) {
			t.Fatalf("客户端应收到 Anthropic 形状（缺 %s），收到 %s", marker, body)
		}
	}
	if strings.Contains(body, `"object":"response"`) {
		t.Fatalf("正文泄漏了上游 Responses 方言：%s", body)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("正文已重写，媒体类型应为 JSON，收到 %q", got)
	}
}

// TestCrossDialectStreamReframesSSE 钉住 `data.messages.stream` / `data.chat.stream`：
// 上游 Responses SSE 必须逐帧重框为客户端方言，且终止事件按目标线产出。
func TestCrossDialectStreamReframesSSE(t *testing.T) {
	upstreamSSE := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress","output":[]}}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_0","type":"message","content":[]}}` + "\n\n" +
		"event: response.content_part.added\n" +
		`data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"msg_0","delta":"你好"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8}}}` + "\n\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, upstreamSSE)
	}))
	defer upstream.Close()

	handler := newConversionHandler(t, upstream.URL, convert.ProviderCodex, true)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"gpt-5.6","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"x"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", "fake-client-key")
	handler.ServeHTTP(recorder, request)

	body := recorder.Body.String()
	if !strings.Contains(body, "event: message_start") {
		t.Fatalf("首帧应为 Anthropic 的 message_start，收到 %s", body)
	}
	if !strings.Contains(body, "event: message_stop") {
		t.Fatalf("终止事件应为 Anthropic 的 message_stop，收到 %s", body)
	}
	if strings.Contains(body, "response.output_text.delta") {
		t.Fatalf("流里泄漏了上游 Responses 方言：%s", body)
	}
	if !strings.Contains(body, `"text_delta","text":"你好"`) {
		t.Fatalf("增量文本应被重框为 text_delta，收到 %s", body)
	}
}

// TestSameDialectResponseStaysByteIdentical 钉住「同方言零改动」：
// 计划未施加转换时绝不建立转换器，正文与头部逐字节保持上游原样（native 直通是既有行为）。
func TestSameDialectResponseStaysByteIdentical(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Marker", "kept")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, responsesNonStreamBody)
	}))
	defer upstream.Close()

	// providerType 取 codex，与 /v1/responses 同线：无转换计划，故不得触碰正文。
	handler := newConversionHandler(t, upstream.URL, convert.ProviderCodex, true)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"gpt-5.6","input":"x"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", "fake-client-key")
	handler.ServeHTTP(recorder, request)

	if got := recorder.Body.String(); got != responsesNonStreamBody {
		t.Fatalf("同方言必须逐字节透传\n got: %s\nwant: %s", got, responsesNonStreamBody)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("同方言不得改写媒体类型，收到 %q", got)
	}
	if got := recorder.Header().Get("X-Upstream-Marker"); got != "kept" {
		t.Fatalf("同方言不得丢掉上游头部，收到 %q", got)
	}
}

// settingsWithUpstreamPassthrough 是与**生产口径**一致的设置桩：
// pass_through_upstream_error_message 打开（生产值，也是 Node 的 DEFAULT_SETTINGS）。
//
// 为何不能沿用零值 fakeSettings：零值等于「显式关闭」，会让所有上游错误消息被换成状态码
// 通用文案（Node error-handler.ts:121-158 的两态），于是「错误体不进方言回译」这条钉子
// 会被错误消息的形态变化掩盖。两态本身由 TestFailoverStatusForPassThroughSwitch 覆盖。
type settingsWithUpstreamPassthrough struct{}

func (settingsWithUpstreamPassthrough) FindSystemSettings(context.Context) (*store.SystemSettings, error) {
	return &store.SystemSettings{PassThroughUpstreamErrorMessage: true}, nil
}

// TestNonSuccessResponseIsNotConverted 钉住「非 2xx 不转换」：错误体不进方言回译。
//
// 注意与转换职责的边界：上游错误体本身会由既有错误路径（与 Node 同口径）重新整形并保留
// 原文消息，那不是本层的行为；本测试只钉「不额外把错误体译成客户端方言」。
func TestNonSuccessResponseIsNotConverted(t *testing.T) {
	const errorBody = `{"object":"response","status":"failed","error":{"message":"上游拒绝"}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, errorBody)
	}))
	defer upstream.Close()

	handler := newConversionHandler(t, upstream.URL, convert.ProviderCodex, true)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"gpt-5.6","max_tokens":16,"messages":[{"role":"user","content":"x"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", "fake-client-key")
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("上游状态码应透传，收到 %d（%s）", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "上游拒绝") {
		t.Fatalf("上游错误消息应保留，收到 %s", body)
	}
	if strings.Contains(body, `"stop_reason"`) || strings.Contains(body, `"content"`) {
		t.Fatalf("错误体被方言回译成了客户端响应形状：%s", body)
	}
}

// TestResponseConversionSkipsOpaqueEncoding 钉住「压缩态不转换」：解不开的字节喂给编解码器
// 只会产出垃圾，故一律原样透传。
func TestResponseConversionSkipsOpaqueEncoding(t *testing.T) {
	conversion := newResponseConversion(&forward.Plan{
		Conversion: &convert.ConversionPlan{
			ClientProtocol: convert.ProtocolAnthropicMessages,
			TargetProtocol: convert.ProtocolOpenAIResponses,
		},
	}, convert.FormatClaude, "gpt-5.6", false, false)
	if conversion == nil {
		t.Fatal("开了转换的计划应能建出转换器")
	}
	header := http.Header{"Content-Encoding": []string{"gzip"}}
	if !hasOpaqueContentEncoding(header) {
		t.Fatal("gzip 应被判为不可转换")
	}
	if hasOpaqueContentEncoding(http.Header{"Content-Encoding": []string{"identity"}}) {
		t.Fatal("identity 不应被判为不可转换")
	}
}

// TestCrossDialectStreamIsIncremental 钉住「重框不得整流缓冲」。
//
// 与既有 `TestStreamDeliversEventsIncrementally` 同形，但上游方言与客户端方言不同：转换层一旦
// 攒齐整流再输出，首帧就会等到上游结束才到达——TTFT 与内存两条不变量同时失效，故必须钉住。
func TestCrossDialectStreamIsIncremental(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		// 决定性内容帧：门控只在看到真实内容时才提交（中性帧会让客户端等不到响应头）。
		_, _ = io.WriteString(w, "event: response.created\n"+
			`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress","output":[]}}`+"\n\n")
		_, _ = io.WriteString(w, "event: response.output_text.delta\n"+
			`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"msg_0","delta":"甲"}`+"\n\n")
		flusher.Flush()
		<-release
		_, _ = io.WriteString(w, "event: response.completed\n"+
			`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":3,"output_tokens":5}}}`+"\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	handler := newConversionHandler(t, upstream.URL, convert.ProviderCodex, true)
	server := httptest.NewServer(handler)
	defer server.Close()

	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/messages",
		strings.NewReader(`{"model":"gpt-5.6","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", "fake-client-key")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("状态码应为 200，收到 %d", response.StatusCode)
	}

	type readResult struct {
		text string
		err  error
	}
	read := make(chan readResult, 1)
	go func() {
		buffer := make([]byte, 256)
		count, readErr := response.Body.Read(buffer)
		read <- readResult{text: string(buffer[:count]), err: readErr}
	}()

	select {
	case result := <-read:
		if result.err != nil && !errors.Is(result.err, io.EOF) {
			t.Fatalf("读首块失败: %v", result.err)
		}
		if !strings.Contains(result.text, "message_start") {
			t.Fatalf("首块应已是客户端方言的帧，收到 %q", result.text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("首帧未在上游释放前到达：重框把整流缓冲了")
	}
	close(release)

	rest, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("读余下帧失败: %v", err)
	}
	if !strings.Contains(string(rest), "message_stop") {
		t.Fatalf("余下帧应含 message_stop，收到 %q", string(rest))
	}
}

// TestCrossDialectNonStreamInjectsPlaceholderSignature 钉住整条接线：Options 上的
// PlaceholderThinkingSignature 必须一路走到响应编码器，让 Anthropic 客户端收到带签名的思考块。
//
// 为什么在数据面这一层再钉一次：convert 层的用例只能证明「给了开关就会补」，证明不了
// 「开关从装配层传到了编码器」——而生产中这个开关曾经就因为只读了一半条件而恒闭。
func TestCrossDialectNonStreamInjectsPlaceholderSignature(t *testing.T) {
	const chatBody = `{"id":"c1","object":"chat.completion","model":"deepseek-v4-flash",` +
		`"choices":[{"index":0,"message":{"role":"assistant","reasoning_content":"先想一下","content":"答案"},` +
		`"finish_reason":"stop"}]}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, chatBody)
	}))
	defer upstream.Close()

	const requestBody = `{"model":"deepseek-v4-flash","max_tokens":16,"messages":[{"role":"user","content":"x"}]}`

	t.Run("开关开：思考块带占位签名", func(t *testing.T) {
		handler := newConversionHandlerWithPlaceholder(
			t, upstream.URL, convert.ProviderOpenAICompatible, true, true)
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(requestBody))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Api-Key", "fake-client-key")
		handler.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusOK {
			t.Fatalf("状态码应为 200，收到 %d（%s）", recorder.Code, recorder.Body.String())
		}
		body := recorder.Body.String()
		if !strings.Contains(body, `"type":"thinking"`) {
			t.Fatalf("客户端应收到 thinking 块：%s", body)
		}
		if !strings.Contains(body, `"signature":"`+convert.PlaceholderThinkingSignature()+`"`) {
			t.Fatalf("thinking 块应带占位签名：%s", body)
		}
	})

	t.Run("开关关：回退为丢弃思考块", func(t *testing.T) {
		handler := newConversionHandlerWithPlaceholder(
			t, upstream.URL, convert.ProviderOpenAICompatible, true, false)
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(requestBody))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Api-Key", "fake-client-key")
		handler.ServeHTTP(recorder, request)

		if recorder.Code != http.StatusOK {
			t.Fatalf("状态码应为 200，收到 %d（%s）", recorder.Code, recorder.Body.String())
		}
		body := recorder.Body.String()
		if strings.Contains(body, `"type":"thinking"`) {
			t.Fatalf("开关关闭时不应产出 thinking 块：%s", body)
		}
		if strings.Contains(body, convert.PlaceholderThinkingSignature()) {
			t.Fatalf("开关关闭时不应出现占位签名：%s", body)
		}
	})
}
