package dataplane

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
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

// bodyDiagnostics 是 dataplane.request_body_unavailable 一条日志的断言视图。
type bodyDiagnostics map[string]any

// lastBodyUnavailable 从捕获的日志里取出最后一条「正文不可用」诊断。
func lastBodyUnavailable(t *testing.T, logs *bytes.Buffer) bodyDiagnostics {
	t.Helper()
	var found bodyDiagnostics
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("日志行不是 JSON: %q（%v）", line, err)
		}
		if record["event"] == "dataplane.request_body_unavailable" {
			found = record
		}
	}
	if found == nil {
		t.Fatalf("未找到 dataplane.request_body_unavailable 日志，实有：%s", logs.String())
	}
	return found
}

// number 取日志里的数值字段（logx 用 encoding/json，数字解出来是 float64）。
func (d bodyDiagnostics) number(t *testing.T, field string) float64 {
	t.Helper()
	value, ok := d[field].(float64)
	if !ok {
		t.Fatalf("日志缺少数值字段 %s：%v", field, d)
	}
	return value
}

// TestDecodeFailedBodyLogsDiagnostics 覆盖 (A)/(B) 里的 (B) 且是最常见的子类：
// 客户端声明了 content-encoding: gzip 却发了明文。日志必须能一眼看出「字节到了我们手上、
// 但我们解不出来」（readBytes>0 而 decodedBytes=0），而不是只能看到一个笼统的「正文不可用」。
func TestDecodeFailedBodyLogsDiagnostics(t *testing.T) {
	upstream, capture := newCapturingUpstream(t, `{"id":"msg_1","type":"message"}`)
	candidates := &typedCandidates{url: upstream.URL, providerType: convert.ProviderClaude}
	var logs bytes.Buffer
	handler, _ := newRestPathHandlerInto(t, candidates, &logs)

	plaintext := `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(plaintext)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Content-Encoding", "gzip")
	// 真客户端会带这个头（线上服务端会把它留在 r.Header 里）；直接调 ServeHTTP 时要自己补。
	request.Header.Set("Content-Length", strconv.Itoa(len(plaintext)))
	request.Header.Set("x-api-key", "fake-client-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码应为 503，实为 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	if capture.body != "" {
		t.Fatalf("正文不可用时不得向上游转发，上游却收到了 %d 字节", len(capture.body))
	}
	diagnostics := lastBodyUnavailable(t, &logs)
	if got := diagnostics["failureKind"]; got != "decode_failed" {
		t.Fatalf("failureKind = %v，期望 decode_failed", got)
	}
	if got := diagnostics["encoding"]; got != "gzip" {
		t.Fatalf("encoding = %v，期望 gzip", got)
	}
	if got := diagnostics.number(t, "declaredCompressed"); got <= 0 {
		t.Fatalf("declaredCompressed 应大于 0，实为 %v", got)
	}
	if got := diagnostics.number(t, "readBytes"); got <= 0 {
		t.Fatalf("客户端确实发了字节，readBytes 应大于 0，实为 %v", got)
	}
	if got := diagnostics.number(t, "decodedBytes"); got != 0 {
		t.Fatalf("解压失败时 decodedBytes 应为 0，实为 %v", got)
	}
}

// TestEmptyInboundPostIsRejected 覆盖 (A)：入站既无 content-length>0 也无 transfer-encoding 的
// POST 是坏请求（正文即请求本体），必须在本地 400 拒掉，而不是把空体送上去换一句上游 400。
func TestEmptyInboundPostIsRejected(t *testing.T) {
	upstream, capture := newCapturingUpstream(t, `{"id":"msg_1","type":"message"}`)
	candidates := &typedCandidates{url: upstream.URL, providerType: convert.ProviderClaude}
	var logs bytes.Buffer
	handler, _ := newRestPathHandlerInto(t, candidates, &logs)

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	request.Header.Set("x-api-key", "fake-client-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("状态码应为 400，实为 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "invalid_request_error") {
		t.Fatalf("错误类型应为 invalid_request_error，实为 %s", recorder.Body.String())
	}
	if capture.body != "" {
		t.Fatalf("空 POST 不得向上游转发，上游却收到了 %q", capture.body)
	}
	if got := lastBodyUnavailable(t, &logs)["failureKind"]; got != "empty_inbound" {
		t.Fatalf("failureKind = %v，期望 empty_inbound", got)
	}
}

// TestDeclaredButEmptyBodyLogsDiagnostics 钉住「声明了长度却读到零字节」的诊断：
// readBytes=0 说明字节根本没到，与 decode_failed 是两种完全不同的现场。
func TestDeclaredButEmptyBodyLogsDiagnostics(t *testing.T) {
	upstream, capture := newCapturingUpstream(t, `{"id":"msg_1","type":"message"}`)
	candidates := &typedCandidates{url: upstream.URL, providerType: convert.ProviderClaude}
	var logs bytes.Buffer
	handler, _ := newRestPathHandlerInto(t, candidates, &logs)

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
	diagnostics := lastBodyUnavailable(t, &logs)
	if got := diagnostics["failureKind"]; got != "declared_but_empty" {
		t.Fatalf("failureKind = %v，期望 declared_but_empty", got)
	}
	if got := diagnostics.number(t, "readBytes"); got != 0 {
		t.Fatalf("readBytes 应为 0，实为 %v", got)
	}
	if got := diagnostics.number(t, "declaredCompressed"); got != 4096 {
		t.Fatalf("declaredCompressed 应为 4096，实为 %v", got)
	}
}

// TestNonObjectJSONBodyLogsDiagnostics 覆盖「读到了字节，但不是 JSON 对象」：
// 与解压失败共用 503，但日志必须分得开（这不是传输问题，是客户端发了别的形状）。
func TestNonObjectJSONBodyLogsDiagnostics(t *testing.T) {
	upstream, capture := newCapturingUpstream(t, `{"id":"msg_1","type":"message"}`)
	candidates := &typedCandidates{url: upstream.URL, providerType: convert.ProviderClaude}
	var logs bytes.Buffer
	handler, _ := newRestPathHandlerInto(t, candidates, &logs)

	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(`[1,2,3]`)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", "fake-client-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("状态码应为 503，实为 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	if capture.body != "" {
		t.Fatalf("正文不是 JSON 对象时不得转发，上游却收到了 %q", capture.body)
	}
	diagnostics := lastBodyUnavailable(t, &logs)
	if got := diagnostics["failureKind"]; got != "not_json_object" {
		t.Fatalf("failureKind = %v，期望 not_json_object", got)
	}
	if got := diagnostics.number(t, "decodedBytes"); got != 7 {
		t.Fatalf("decodedBytes 应为 7，实为 %v", got)
	}
}

// TestEmptyBodyCancelStylePostStillForwards 钉住「空体拒绝只针对正文即请求本体的端点」：
// 族透传里的 cancel 类 POST 本来就合法地不带正文，不得被空体闸误拒（否则会打断批任务取消）。
func TestEmptyBodyCancelStylePostStillForwards(t *testing.T) {
	upstream, capture := newCapturingUpstream(t, `{"id":"batch_1","status":"cancelling"}`)
	candidates := &typedCandidates{url: upstream.URL, providerType: convert.ProviderOpenAICompatible}
	var logs bytes.Buffer
	handler, _ := newRestPathHandlerInto(t, candidates, &logs)

	request := httptest.NewRequest(http.MethodPost, "/v1/batches/batch_1/cancel", nil)
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
