package dataplane

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// TestUnparseableBodyIsRejectedLocally 钉住「正文读不出来不得转发」：
// 客户端声明 content-encoding: gzip 但正文不是 gzip 时，网关若按空正文继续转发，
// 上游会收到 Content-Length: 0 的空 POST 并回一句与真实原因无关的 400
// （生产上即 magpie 的 `invalid request: expected a JSON object`）。
// 正确行为是本地 503 并给出真实原因（5xx 才会触发 agent 重试，4xx 不会），且上游一次都不能被调用。
func TestUnparseableBodyIsRejectedLocally(t *testing.T) {
	upstream, capture := newCapturingUpstream(t, `{"id":"msg_1","type":"message"}`)
	candidates := &typedCandidates{url: upstream.URL, providerType: convert.ProviderClaude}
	handler, _ := newRestPathHandler(t, candidates)

	plaintext := `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(plaintext)))
	request.Header.Set("Content-Type", "application/json")
	// 头与正文不符：声明 gzip，正文是明文。
	request.Header.Set("Content-Encoding", "gzip")
	request.Header.Set("x-api-key", "fake-client-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码应为 503，实为 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "invalid request body") {
		t.Fatalf("响应应说明正文不可用，实为 %s", recorder.Body.String())
	}
	if capture.body != "" {
		t.Fatalf("正文不可用时不得向上游转发，上游却收到了 %d 字节", len(capture.body))
	}
}

// TestEmptyBodyStillForwards 是上一条的反面：客户端本来就没带正文时，不得因
// 「读不出正文」误拒——出站正文为空与入站一致。
// TestDeclaredButEmptyBodyIsRejected 钉住「声明了长度却解出零字节」：这种请求若照常转发，
// 上游收到的同样是空 POST（同一条生产故障），所以必须本地拒。
func TestDeclaredButEmptyBodyIsRejected(t *testing.T) {
	upstream, capture := newCapturingUpstream(t, `{"id":"msg_1","type":"message"}`)
	candidates := &typedCandidates{url: upstream.URL, providerType: convert.ProviderClaude}
	handler, _ := newRestPathHandler(t, candidates)

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(nil))
	request.ContentLength = 4096
	request.Header.Set("Content-Length", "4096")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", "fake-client-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码应为 503，实为 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	if capture.body != "" {
		t.Fatalf("声明了正文却读不出时不得转发，上游却收到了 %q", capture.body)
	}
}

func TestEmptyBodyStillForwards(t *testing.T) {
	upstream, capture := newCapturingUpstream(t, `{"input_tokens":3}`)
	candidates := &typedCandidates{url: upstream.URL, providerType: convert.ProviderClaude}
	handler, _ := newRestPathHandler(t, candidates)

	request := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", nil)
	request.Header.Set("x-api-key", "fake-client-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，实为 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	if capture.body != "" {
		t.Fatalf("无正文请求的出站正文应为空，实为 %q", capture.body)
	}
}
