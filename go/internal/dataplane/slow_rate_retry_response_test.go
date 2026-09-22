package dataplane

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/gate"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
)

// 本文件钉住「最终失败确因判慢 ⇒ 返回客户端可识别的可重试错误」。
//
// 为什么必须有：判慢是**本进程主动放弃**（不是上游报错），若照 failoverStatus 的通用口径回
// 524 + 「上游返回 524」，agent 拿到的是一个既非重试语义、也无重试头的响应，只能干等或放弃。
// 三条同时到位才生效：503、retry-after: 0、x-should-retry: true。
//
// 反面同等重要：其余失败（真实上游 5xx、无候选）与判慢**共用同一个 503 状态码**，
// 故「改对了」与「误伤了一片」在状态码上分不开——必须逐字钉住正文与「不得出现重试头」。

// slowRateStallingUpstream 起一个「先给一帧、随后彻底停住」的上游，制造主动判慢。
//
// 首帧必须是合法 SSE：门控据此判定首字节已到，随后在探测阈值内不再有可提交内容即判慢。
// 上游在 stall 关闭前一直不发字节——这正是生产里「低速苦等」的形状。
func slowRateStallingUpstream(t *testing.T) (string, func()) {
	t.Helper()
	stall := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("假上游必须可 flush")
			return
		}
		_, _ = io.WriteString(w, "event: message_start\n"+
			"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m_1\",\"type\":\"message\","+
			"\"role\":\"assistant\",\"content\":[],\"model\":\"claude-sonnet-4-5\"}}\n\n")
		flusher.Flush()
		<-stall
	}))
	return server.URL, func() {
		close(stall)
		server.Close()
	}
}

// slowRateStreamOptions 把探测阈值压到 1 秒并关掉静默超时：判慢必须由探测阈值决定，
// 而不是被读间隔超时抢先归因成 FailIdleTimeout（那是另一档，不走可重试错误）。
func slowRateStreamOptions() forward.StreamOptions {
	return forward.StreamOptions{
		Budget:                 gate.DefaultBudget(),
		ProbeAfterFirstByteFor: func(int64) int { return 1 },
	}
}

// TestSlowRateExhaustedReturnsRetryableErrorAnthropic 钉住 Anthropic 入口的终局：
// 唯一候选被判慢、无替代可换 ⇒ 503 + retry-after: 0 + x-should-retry: true +
// overloaded_error 形状的正文。
func TestSlowRateExhaustedReturnsRetryableErrorAnthropic(t *testing.T) {
	upstreamURL, stop := slowRateStallingUpstream(t)
	defer stop()
	handler, _, _ := newTestHandlerWithStream(t, upstreamURL, fakeAuth{
		user: guard.User{ID: 1, Name: "甲", IsEnabled: true},
		key:  guard.Key{ID: 2, Name: "k", UserID: 1},
	}, slowRateStreamOptions())

	recorder := httptest.NewRecorder()
	body := strings.NewReader(`{"model":"claude-sonnet-4-5","max_tokens":16,"stream":true,` +
		`"messages":[{"role":"user","content":"hi"}]}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", body)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", "fake-client-key")
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("判慢终局应为 503，收到 %d（%s）", recorder.Code, recorder.Body.String())
	}
	assertRetryHeaders(t, recorder)
	var payload struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decodeErrorBody(t, recorder.Body.Bytes(), &payload)
	if payload.Type != "error" {
		t.Errorf("Anthropic 正文 type 应为 error，实际 %q（%s）", payload.Type, recorder.Body.String())
	}
	if payload.Error.Type != "overloaded_error" {
		t.Errorf("Anthropic 正文 error.type 应为 overloaded_error，实际 %q", payload.Error.Type)
	}
	if payload.Error.Message == "" {
		t.Error("Anthropic 正文 error.message 不得为空")
	}
}

// TestSlowRateExhaustedReturnsRetryableErrorOpenAI 钉住 OpenAI 入口（chat/completions）的终局：
// 同一档失败在 OpenAI 线上必须换形状为 error.code == server_is_overloaded。
func TestSlowRateExhaustedReturnsRetryableErrorOpenAI(t *testing.T) {
	upstreamURL, stop := slowRateStallingUpstream(t)
	defer stop()
	handler, _, _ := newTestHandlerWithStream(t, upstreamURL, fakeAuth{
		user: guard.User{ID: 1, Name: "甲", IsEnabled: true},
		key:  guard.Key{ID: 2, Name: "k", UserID: 1},
	}, slowRateStreamOptions())

	recorder := httptest.NewRecorder()
	body := strings.NewReader(`{"model":"claude-sonnet-4-5","stream":true,` +
		`"messages":[{"role":"user","content":"hi"}]}`)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", body)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer fake-client-key")
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("判慢终局应为 503，收到 %d（%s）", recorder.Code, recorder.Body.String())
	}
	assertRetryHeaders(t, recorder)
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	decodeErrorBody(t, recorder.Body.Bytes(), &payload)
	if payload.Error.Code != "server_is_overloaded" {
		t.Errorf("OpenAI 正文 error.code 应为 server_is_overloaded，实际 %q（%s）",
			payload.Error.Code, recorder.Body.String())
	}
}

// TestSlowRateRetryBodyShapeByClientFormat 钉住每种入站格式的形状与重试头。
//
// Gemini 走通用 503 形状（其形状未经验证）：这条断言本身就是「我们只是没有核实过它」的显式记录，
// 而不是把它当成已核实的契约。
func TestSlowRateRetryBodyShapeByClientFormat(t *testing.T) {
	failure := &forward.Failure{Category: forward.CategorySlowRate, StatusCode: 524}
	cases := []struct {
		name   string
		format convert.ClientFormat
		want   string
	}{
		{
			name:   "anthropic",
			format: convert.FormatClaude,
			want:   `{"type":"error","error":{"type":"overloaded_error","message":"` + slowRateRetryMessage + `"}}`,
		},
		{
			name:   "openai-chat",
			format: convert.FormatOpenAI,
			want:   `{"error":{"code":"server_is_overloaded","message":"` + slowRateRetryMessage + `"}}`,
		},
		{
			name:   "openai-responses",
			format: convert.FormatResponse,
			want:   `{"error":{"code":"server_is_overloaded","message":"` + slowRateRetryMessage + `"}}`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			state := &RequestState{Format: testCase.format}
			response, ok := slowRateRetryResponse(state, failure)
			if !ok {
				t.Fatalf("判慢归因必须走可重试分支")
			}
			if response.Status != http.StatusServiceUnavailable {
				t.Fatalf("状态码应为 503，收到 %d", response.Status)
			}
			if got := string(response.Body); got != testCase.want {
				t.Errorf("正文形状不符\n实际: %s\n期望: %s", got, testCase.want)
			}
			if got := response.Headers.Get("retry-after"); got != "0" {
				t.Errorf("retry-after 应为 0，实际 %q", got)
			}
			if got := response.Headers.Get("x-should-retry"); got != "true" {
				t.Errorf("x-should-retry 应为 true，实际 %q", got)
			}
		})
	}

	// Gemini 两态：形状未验证，但状态码与重试头同样必须到位。
	for _, format := range []convert.ClientFormat{convert.FormatGemini, convert.FormatGeminiCLI} {
		state := &RequestState{Format: format}
		response, ok := slowRateRetryResponse(state, failure)
		if !ok {
			t.Fatalf("%s 判慢归因必须走可重试分支", format)
		}
		if response.Status != http.StatusServiceUnavailable {
			t.Errorf("%s 状态码应为 503，收到 %d", format, response.Status)
		}
		if got := response.Headers.Get("retry-after"); got != "0" {
			t.Errorf("%s retry-after 应为 0，实际 %q", format, got)
		}
		if got := response.Headers.Get("x-should-retry"); got != "true" {
			t.Errorf("%s x-should-retry 应为 true，实际 %q", format, got)
		}
	}
}

// TestSlowRateRetryResponseOnlyForSlowRate 钉住「只在这一档改」：其余分类一律不接管，
// 调用方必须继续走原有分支。
func TestSlowRateRetryResponseOnlyForSlowRate(t *testing.T) {
	state := &RequestState{Format: convert.FormatClaude}
	for _, failure := range []*forward.Failure{
		nil,
		{Category: forward.CategoryProviderError, StatusCode: 500},
		{Category: forward.CategorySystemError, StatusCode: 524},
		{Category: forward.CategoryProviderSaturated},
		{Category: forward.CategoryClientAbort},
		{Category: forward.CategoryLocalOverload, Internal: true},
		{Category: forward.CategoryProviderUnsupportedInput, StatusCode: 400},
		{Category: forward.CategoryNonRetryableClientError, StatusCode: 400},
	} {
		if _, ok := slowRateRetryResponse(state, failure); ok {
			t.Errorf("分类 %v 不得走可重试分支", failure)
		}
	}
}

// TestSlowRateRetryBodyCarriesNoInternalFacts 是脱敏钉子：这条正文原样交给客户端，
// 不得出现渠道名、上游地址或 IP。
func TestSlowRateRetryBodyCarriesNoInternalFacts(t *testing.T) {
	failure := &forward.Failure{
		Category:     forward.CategorySlowRate,
		StatusCode:   524,
		ProviderName: "假供应商",
		EndpointURL:  "http://127.0.0.1:9/private",
		Message:      "上游返回 524",
	}
	for _, format := range []convert.ClientFormat{
		convert.FormatClaude, convert.FormatOpenAI, convert.FormatResponse,
		convert.FormatGemini, convert.FormatGeminiCLI,
	} {
		state := &RequestState{Format: format}
		response, ok := slowRateRetryResponse(state, failure)
		if !ok {
			t.Fatalf("%s 应走可重试分支", format)
		}
		body := string(response.Body)
		for _, secret := range []string{"假供应商", "127.0.0.1", "http://", "内部"} {
			if strings.Contains(body, secret) {
				t.Errorf("%s 正文不得含内部信息 %q：%s", format, secret, body)
			}
		}
	}
}

// TestUpstream5xxFailureUnchanged 是最关键的防误伤钉子：真实上游 5xx 的终端响应
// 必须逐字不变，且不得带上判慢那套重试头（两者共用 503 状态码，只看状态码分不开）。
//
// 524 单独列一条：它是判慢改造**之前**的终端输出，最容易被「顺手统一成 503」误伤。
func TestUpstream5xxFailureUnchanged(t *testing.T) {
	cases := []struct {
		status   int
		wantBody string
	}{
		{
			status:   http.StatusInternalServerError,
			wantBody: `{"error":{"message":"上游服务暂时不可用，请稍后重试","type":"internal_server_error","code":"internal_server_error"}}`,
		},
		{
			status:   http.StatusServiceUnavailable,
			wantBody: `{"error":{"message":"上游服务暂时不可用，请稍后重试","type":"service_unavailable_error","code":"service_unavailable_error"}}`,
		},
		{
			status:   statusUpstreamTimeoutFixture,
			wantBody: `{"error":{"message":"上游服务响应超时，请稍后重试","type":"api_error","code":"http_524"}}`,
		},
	}
	for _, testCase := range cases {
		t.Run(http.StatusText(testCase.status), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(testCase.status)
				_, _ = io.WriteString(w, `{"error":{"message":"upstream boom at http://198.51.100.7"}}`)
			}))
			defer upstream.Close()

			handler, _, _ := newTestHandler(t, upstream.URL, fakeAuth{
				user: guard.User{ID: 1, Name: "甲", IsEnabled: true},
				key:  guard.Key{ID: 2, Name: "k", UserID: 1},
			})
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/v1/messages",
				strings.NewReader(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Api-Key", "fake-client-key")
			handler.ServeHTTP(recorder, request)

			if recorder.Code != testCase.status {
				t.Fatalf("真实上游 %d 应原样透传，收到 %d（%s）",
					testCase.status, recorder.Code, recorder.Body.String())
			}
			if got := recorder.Body.String(); got != testCase.wantBody {
				t.Errorf("真实上游 %d 的正文不得改变\n实际: %s\n期望: %s", testCase.status, got, testCase.wantBody)
			}
			// 上游原文与其中的地址都不得漏给客户端（这条也顺带钉住脱敏）。
			for _, leaked := range []string{"198.51.100.7", "upstream boom"} {
				if strings.Contains(recorder.Body.String(), leaked) {
					t.Errorf("正文不得回显上游原文 %q：%s", leaked, recorder.Body.String())
				}
			}
			assertNoRetryHeaders(t, recorder)
		})
	}
}

// statusUpstreamTimeoutFixture 是上游超时的合成状态码（与 forward 内部常量同值）。
const statusUpstreamTimeoutFixture = 524

// TestNoAvailableProviderFailureUnchanged 钉住「无候选」这一档（同样回 503）：不得被误伤。
func TestNoAvailableProviderFailureUnchanged(t *testing.T) {
	handler, _, _ := newTestHandlerWithStream(t, "http://127.0.0.1:1", fakeAuth{
		user: guard.User{ID: 1, Name: "甲", IsEnabled: true},
		key:  guard.Key{ID: 2, Name: "k", UserID: 1},
	}, forward.StreamOptions{
		Budget: gate.DefaultBudget(),
	})
	// 选路返回「无可用供应商」（ProviderID 0）走的是守卫之外的专门分支。
	handler.options.Base.Provider = fakeProvider{}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", "fake-client-key")
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("无可用供应商应为 503，收到 %d（%s）", recorder.Code, recorder.Body.String())
	}
	const wantBody = `{"error":{"message":"无可用供应商","type":"service_unavailable_error","code":"service_unavailable_error"}}`
	if got := recorder.Body.String(); got != wantBody {
		t.Errorf("无候选的正文不得改变\n实际: %s\n期望: %s", got, wantBody)
	}
	assertNoRetryHeaders(t, recorder)
}

// ---- 断言助手 ----

func assertRetryHeaders(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if got := recorder.Header().Get("retry-after"); got != "0" {
		t.Errorf("retry-after 应为 0，实际 %q", got)
	}
	if got := recorder.Header().Get("x-should-retry"); got != "true" {
		t.Errorf("x-should-retry 应为 true，实际 %q", got)
	}
	if got := recorder.Header().Get("content-type"); !strings.Contains(got, "application/json") {
		t.Errorf("content-type 应为 JSON，实际 %q", got)
	}
}

func assertNoRetryHeaders(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if got := recorder.Header().Get("retry-after"); got != "" {
		t.Errorf("这一档不得带 retry-after，实际 %q", got)
	}
	if got := recorder.Header().Get("x-should-retry"); got != "" {
		t.Errorf("这一档不得带 x-should-retry，实际 %q", got)
	}
}

func decodeErrorBody(t *testing.T, body []byte, target any) {
	t.Helper()
	if err := json.Unmarshal(body, target); err != nil {
		t.Fatalf("错误体应为合法 JSON：%v（%s）", err, body)
	}
}
