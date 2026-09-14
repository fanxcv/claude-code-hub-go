package dataplane

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
)

// typedCandidates 按指定供应商类型投影候选，并记录每次选路的事实。
//
// 为什么需要记录：`RawPassthrough` 是**路由级**事实，它必须经 SelectionFacts 传到候选上，
// 否则 count_tokens/compact 会照常重试并切换供应商（与 Node 的 raw 策略相反）。
// 没有这条记录，接线断了也看不出来。
type typedCandidates struct {
	url          string
	providerType convert.ProviderType

	mu    sync.Mutex
	facts []SelectionFacts
}

func (c *typedCandidates) Candidate(
	_ context.Context,
	selection pctx.ProviderSelection,
	facts SelectionFacts,
) (*forward.Candidate, route.Result, error) {
	c.mu.Lock()
	c.facts = append(c.facts, facts)
	c.mu.Unlock()
	return &forward.Candidate{
		Provider: forward.Provider{
			ID:   selection.ProviderID,
			Name: selection.Name,
			Type: c.providerType,
			URL:  c.url,
			Key:  "fake-upstream-key",
		},
	}, route.Result{}, nil
}

func (c *typedCandidates) Failover(context.Context, SelectionFacts) (*forward.Candidate, route.Result, error) {
	return nil, route.Result{}, nil
}

func (c *typedCandidates) lastFacts() (SelectionFacts, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.facts) == 0 {
		return SelectionFacts{}, false
	}
	return c.facts[len(c.facts)-1], true
}

// newRestPathHandler 建一个能跑完整管线（守卫链 → 候选 → 转发 → 结算）的数据面处理器。
func newRestPathHandler(t *testing.T, candidates CandidateSource) (*Handler, *fakeSettler) {
	t.Helper()
	dialClient, err := dial.New(dial.Options{})
	if err != nil {
		t.Fatalf("拨号器构造失败: %v", err)
	}
	auth := fakeAuth{
		user: guard.User{ID: 1, Name: "甲", IsEnabled: true},
		key:  guard.Key{ID: 2, Name: "k", UserID: 1},
	}
	settler := &fakeSettler{}
	handler, err := New(Options{
		Logger: logx.New(nil),
		Base: guard.Deps{
			Auth:           auth,
			Users:          auth,
			Settings:       fakeSettings{},
			Sensitive:      fakeEmptySource{},
			Filters:        fakeEmptySource{},
			Provider:       fakeProvider{selection: pctx.ProviderSelection{ProviderID: 7, Name: "假供应商", Type: "claude"}},
			MessageContext: &fakeMessageWriter{},
		},
		Candidates: candidates,
		Settlers:   func(*RequestState) Settler { return settler },
		Forward:    forward.Deps{Dial: dialClient},
	})
	if err != nil {
		t.Fatalf("数据面构造失败: %v", err)
	}
	return handler, settler
}

// upstreamCapture 记录假上游收到的方法、路径、查询串与正文。
type upstreamCapture struct {
	method string
	path   string
	// query 是原始查询串（含前导 "?"；无查询串时为空）。
	//
	// 必须单独记：拼上游 URL 时丢掉查询串不会报错，只会静默改变上游行为
	// （Gemini 的 `?alt=sse` 就是 SSE/JSON 数组的开关），没有它这条就钉不住。
	query  string
	body   string
	header http.Header
}

// newCapturingUpstream 起一个记录请求并回放固定正文的假上游（JSON 响应）。
func newCapturingUpstream(t *testing.T, response string) (*httptest.Server, *upstreamCapture) {
	t.Helper()
	return newCapturingUpstreamWithContentType(t, "application/json", response)
}

// newCapturingUpstreamWithContentType 同上，但可指定响应 Content-Type。
//
// 为何需要：流式用例必须回 `text/event-stream`——流闸门按 Content-Type + 帧内容判定
// 「首条有效内容」，用 JSON 头回 SSE 帧会被它当成空流拒掉（下例即此）。
func newCapturingUpstreamWithContentType(
	t *testing.T,
	contentType string,
	response string,
) (*httptest.Server, *upstreamCapture) {
	t.Helper()
	capture := &upstreamCapture{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		capture.method = request.Method
		capture.path = request.URL.Path
		capture.query = request.URL.RawQuery
		capture.body = string(body)
		capture.header = request.Header.Clone()
		writer.Header().Set("Content-Type", contentType)
		_, _ = writer.Write([]byte(response))
	}))
	t.Cleanup(server.Close)
	return server, capture
}

// TestLiveEmbeddingsPassesThrough 是本批的**实测对照**：一条非主路由的族路径
// （`/v1/embeddings`）走完整管线，上游拿到的路径与正文必须逐字节不变。
//
// 判据三条（缺一不可）：
//  1. 上游收到的路径就是客户端路径（不是被改写成别的线）；
//  2. 上游收到的正文与客户端正文逐字节相同（未经过转换或模型改写）；
//  3. 走的是主路由同一套管线：结算缝被调用一次（fakeSettler 计数）。
func TestLiveEmbeddingsPassesThrough(t *testing.T) {
	upstream, capture := newCapturingUpstream(t, `{"object":"list","data":[{"object":"embedding","embedding":[0.1,0.2],"index":0}]}`)
	candidates := &typedCandidates{url: upstream.URL, providerType: convert.ProviderOpenAICompatible}
	handler, settler := newRestPathHandler(t, candidates)

	body := `{"model":"text-embedding-3-small","input":"hello world"}`
	request := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", "fake-client-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，实为 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	if capture.method != http.MethodPost {
		t.Fatalf("上游方法应为 POST，实为 %s", capture.method)
	}
	if capture.path != "/v1/embeddings" {
		t.Fatalf("上游路径应逐字保留为 /v1/embeddings，实为 %s", capture.path)
	}
	if capture.body != body {
		t.Fatalf("上游正文应与客户端正文逐字节相同\nwant %s\ngot  %s", body, capture.body)
	}
	if !strings.Contains(recorder.Body.String(), "embedding") {
		t.Fatalf("客户端应拿到上游正文，实为 %s", recorder.Body.String())
	}
	// 非流式终态落在 nonStream 缝（settleCount 只计流式）。
	if len(settler.nonStream) != 1 {
		t.Fatalf("应经主路由同一套结算缝落一次终态，实为 %d 次", len(settler.nonStream))
	}
	if settler.nonStream[0].Status != http.StatusOK {
		t.Fatalf("终态状态码应为 200，实为 %d", settler.nonStream[0].Status)
	}
}

// TestLiveCountTokensIsRawPassthrough 覆盖原始透传端点：路径与正文原样，且带 raw 标记。
func TestLiveCountTokensIsRawPassthrough(t *testing.T) {
	upstream, capture := newCapturingUpstream(t, `{"input_tokens":12}`)
	candidates := &typedCandidates{url: upstream.URL, providerType: convert.ProviderClaude}
	handler, _ := newRestPathHandler(t, candidates)

	body := `{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hi"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", "fake-client-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，实为 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	if capture.path != "/v1/messages/count_tokens" {
		t.Fatalf("上游路径应保留为 /v1/messages/count_tokens，实为 %s", capture.path)
	}
	if capture.body != body {
		t.Fatalf("原始透传端点的正文必须逐字节不变\nwant %s\ngot  %s", body, capture.body)
	}
	facts, ok := candidates.lastFacts()
	if !ok {
		t.Fatal("候选未收到选路事实")
	}
	if !facts.RawPassthrough {
		t.Fatal("count_tokens 的选路事实未带原始透传标记：会照常重试并切换供应商")
	}
}

// TestLivePrefixFamilyKeepsPath 覆盖前缀族（`/v1/assistants`）：路径原样、且不落原始透传。
func TestLivePrefixFamilyKeepsPath(t *testing.T) {
	upstream, capture := newCapturingUpstream(t, `{"id":"asst_1","object":"assistant"}`)
	candidates := &typedCandidates{url: upstream.URL, providerType: convert.ProviderOpenAICompatible}
	handler, _ := newRestPathHandler(t, candidates)

	request := httptest.NewRequest(http.MethodPost, "/v1/assistants",
		strings.NewReader(`{"model":"gpt-4o","instructions":"hi"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", "fake-client-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，实为 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	if capture.path != "/v1/assistants" {
		t.Fatalf("上游路径应为 /v1/assistants，实为 %s", capture.path)
	}
	facts, _ := candidates.lastFacts()
	if facts.RawPassthrough {
		t.Fatal("前缀族不该被判为原始透传")
	}
}

// TestLiveGeminiBetaPostIsNativePassthrough 覆盖 Gemini 线的**原生透传**：
// `/v1beta/models/{model}:generateContent` 打到 gemini 供应商时，路径、查询与正文都逐字保留，
// 鉴权换成供应商凭据（x-goog-api-key），且不得带上客户端的 key。
//
// 历史：本用例曾断言「能路由但拒发」（502 + “供应商类型不支持转发”）。
// 现在 gemini 已实现（与 Node forwarder.ts:3299 同形态的分派），故改钉正向事实；
// 原先的缺口描述与验收信号保留在 的报告里。
func TestLiveGeminiBetaPostIsNativePassthrough(t *testing.T) {
	upstream, capture := newCapturingUpstream(t, `{"candidates":[]}`)
	candidates := &typedCandidates{url: upstream.URL, providerType: convert.ProviderGemini}
	handler, _ := newRestPathHandler(t, candidates)

	body := `{"contents":[{"parts":[{"text":"hi"}]}]}`
	request := httptest.NewRequest(http.MethodPost,
		"/v1beta/models/gemini-2.0-flash:generateContent", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-goog-api-key", "fake-client-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，实为 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	if capture.path != "/v1beta/models/gemini-2.0-flash:generateContent" {
		t.Fatalf("上游路径应逐字保留，实为 %s", capture.path)
	}
	if capture.body != body {
		t.Fatalf("gemini 透传不得改写正文\nwant %s\ngot  %s", body, capture.body)
	}
	if got := capture.header.Get("x-goog-api-key"); got != "fake-upstream-key" {
		t.Fatalf("上游鉴权应为供应商凭据，实为 %q", got)
	}
	if got := capture.header.Get("x-api-key"); got != "" {
		t.Fatalf("客户端自带的 x-api-key 不得透传，实为 %q", got)
	}
	if got := capture.header.Get("accept-encoding"); got != "identity" {
		t.Fatalf("accept-encoding 应为 identity，实为 %q", got)
	}
}

// TestLiveGeminiStreamGenerateContentKeepsQueryAndStreams 钉住流式形态：
// `?alt=sse` 必须原样带给上游（它是上游回 SSE 而非 JSON 数组的开关），
// 且响应 Content-Type 按上游原样回给客户端。
func TestLiveGeminiStreamGenerateContentKeepsQueryAndStreams(t *testing.T) {
	frame := `{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":2,"totalTokenCount":5}}`
	upstream, capture := newCapturingUpstreamWithContentType(t, "text/event-stream", "data: "+frame+"\n\n")
	candidates := &typedCandidates{url: upstream.URL, providerType: convert.ProviderGemini}
	handler, _ := newRestPathHandler(t, candidates)

	request := httptest.NewRequest(http.MethodPost,
		"/v1beta/models/gemini-2.0-flash:streamGenerateContent?alt=sse",
		strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-goog-api-key", "fake-client-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，实为 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	if capture.path != "/v1beta/models/gemini-2.0-flash:streamGenerateContent" {
		t.Fatalf("上游路径应保留，实为 %s", capture.path)
	}
	if capture.query != "alt=sse" {
		t.Fatalf("查询串必须原样透传（alt=sse 是上游的 SSE 开关），实为 %q", capture.query)
	}
	if !strings.Contains(recorder.Body.String(), "data:") {
		t.Fatalf("SSE 正文应原样回给客户端，实为 %s", recorder.Body.String())
	}
}

// TestLiveGeminiFilesIsNativePassthrough 覆盖 `/v1beta/files`：
// Node 侧它落在 `app.all("*")` 的代理分支（v1beta/[...route]/route.ts:23），故与生成端点同形态。
func TestLiveGeminiFilesIsNativePassthrough(t *testing.T) {
	upstream, capture := newCapturingUpstream(t, `{"files":[]}`)
	candidates := &typedCandidates{url: upstream.URL, providerType: convert.ProviderGemini}
	handler, _ := newRestPathHandler(t, candidates)

	body := `{"file":{"displayName":"x"}}`
	request := httptest.NewRequest(http.MethodPost, "/v1beta/files", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-goog-api-key", "fake-client-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，实为 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	if capture.path != "/v1beta/files" {
		t.Fatalf("上游路径应为 /v1beta/files，实为 %s", capture.path)
	}
	if capture.body != body {
		t.Fatalf("正文应逐字节透传\nwant %s\ngot  %s", body, capture.body)
	}
}

// TestLiveGeminiCLIProviderSetsClientHeader 钉住 gemini-cli 的客户端标识：
// Node 在 buildGeminiHeaders 里恒写 `x-goog-api-client: GeminiCLI/1.0`。
func TestLiveGeminiCLIProviderSetsClientHeader(t *testing.T) {
	upstream, capture := newCapturingUpstream(t, `{"candidates":[]}`)
	candidates := &typedCandidates{url: upstream.URL, providerType: convert.ProviderGeminiCLI}
	handler, _ := newRestPathHandler(t, candidates)

	request := httptest.NewRequest(http.MethodPost,
		"/v1beta/models/gemini-2.5-flash:generateContent",
		strings.NewReader(`{"contents":[{"parts":[{"text":"hi"}]}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-goog-api-key", "fake-client-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，实为 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	if got := capture.header.Get("x-goog-api-client"); got != "GeminiCLI/1.0" {
		t.Fatalf("gemini-cli 供应商应带客户端标识，实为 %q", got)
	}
}

// TestLiveUnknownPathStillFallsBack 回归钉子：伪造路径仍交回 fallback（撤 Node 前不得被 Go 接管）。
func TestLiveUnknownPathStillFallsBack(t *testing.T) {
	fallback := &fallbackRecorder{}
	handler, _ := newRestPathHandler(t, &typedCandidates{url: "https://unused.test", providerType: convert.ProviderClaude})
	handler.options.Fallback = fallback

	request := httptest.NewRequest(http.MethodPost, "/v1/definitely-not-real-xyz", bytes.NewReader([]byte(`{}`)))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if !fallback.called {
		t.Fatal("伪造路径未交回 fallback：撤 Node 前这类路径必须保持可回退")
	}
}

// fallbackRecorder 记录回退处理器是否被调用。
type fallbackRecorder struct {
	called bool
}

func (f *fallbackRecorder) ServeHTTP(writer http.ResponseWriter, _ *http.Request) {
	f.called = true
	writer.WriteHeader(http.StatusNotFound)
}
