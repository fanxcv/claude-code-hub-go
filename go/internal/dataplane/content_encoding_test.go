package dataplane

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// gzipPayload 把正文压成 gzip 字节（模拟客户端以 content-encoding 发来的请求体）。
func gzipPayload(t *testing.T, payload string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write([]byte(payload)); err != nil {
		t.Fatalf("压缩失败: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("关闭压缩器失败: %v", err)
	}
	return buffer.Bytes()
}

// TestLiveGzipRequestForwardsPlaintextWithoutContentEncoding 覆盖带上游的完整管线（CHAT_PIPELINE）：
// 客户端声明 gzip 且正文确实是 gzip 时，网关手里是解压后的明文，故出站既不得带
// content-encoding，正文也必须是可解析的明文 JSON。
func TestLiveGzipRequestForwardsPlaintextWithoutContentEncoding(t *testing.T) {
	upstream, capture := newCapturingUpstream(t, `{"id":"msg_1","type":"message"}`)
	candidates := &typedCandidates{url: upstream.URL, providerType: convert.ProviderClaude}
	handler, _ := newRestPathHandler(t, candidates)

	plaintext := `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(gzipPayload(t, plaintext)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Content-Encoding", "gzip")
	request.Header.Set("x-api-key", "fake-client-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，实为 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	if got := capture.header.Get("Content-Encoding"); got != "" {
		t.Fatalf("出站正文已解压成明文，不得再带 content-encoding，实为 %q", got)
	}
	var tree map[string]any
	if err := json.Unmarshal([]byte(capture.body), &tree); err != nil {
		t.Fatalf("出站正文应是明文 JSON，实为 %q（%v）", capture.body, err)
	}
	if tree["model"] != "claude-sonnet-4-5" {
		t.Fatalf("出站正文的模型名不对: %v", tree["model"])
	}
	if got := capture.header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("出站内容类型应为 application/json，实为 %q", got)
	}
}

// TestLiveRawPassthroughGzipForwardsPlaintextWithoutContentEncoding 钉住原始透传端点
// （RAW_PASSTHROUGH_PIPELINE 不经 BodyAccessor 跑守卫）的正文编码事实：
// 转发拿到的仍是**解压后的明文**（正文快照来自 dataplane.planFacts 的 bodyAccess.bytes()），
// 故该路径同样不得带 content-encoding，且正文等于客户端解压后的原文。
func TestLiveRawPassthroughGzipForwardsPlaintextWithoutContentEncoding(t *testing.T) {
	upstream, capture := newCapturingUpstream(t, `{"input_tokens":12}`)
	candidates := &typedCandidates{url: upstream.URL, providerType: convert.ProviderClaude}
	handler, _ := newRestPathHandler(t, candidates)

	plaintext := `{"model":"claude-haiku-4-5","messages":[{"role":"user","content":"hi"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens",
		bytes.NewReader(gzipPayload(t, plaintext)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Content-Encoding", "gzip")
	request.Header.Set("x-api-key", "fake-client-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，实为 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	if got := capture.header.Get("Content-Encoding"); got != "" {
		t.Fatalf("原始透传的正文也是解压后的明文，不得带 content-encoding，实为 %q", got)
	}
	if capture.body != plaintext {
		t.Fatalf("原始透传的正文应等于客户端解压后的原文\nwant %s\ngot  %s", plaintext, capture.body)
	}
}

// TestLivePlainRequestUnaffectedByEncodingStrip 回归钉子：客户端没声明 content-encoding 时，
// 出站头与正文逐字不受影响。
func TestLivePlainRequestUnaffectedByEncodingStrip(t *testing.T) {
	upstream, capture := newCapturingUpstream(t, `{"id":"msg_1","type":"message"}`)
	candidates := &typedCandidates{url: upstream.URL, providerType: convert.ProviderClaude}
	handler, _ := newRestPathHandler(t, candidates)

	plaintext := `{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader([]byte(plaintext)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-api-key", "fake-client-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，实为 %d（body=%s）", recorder.Code, recorder.Body.String())
	}
	if got := capture.header.Get("Content-Encoding"); got != "" {
		t.Fatalf("无编码的请求不得凭空多出 content-encoding，实为 %q", got)
	}
	if capture.body != plaintext {
		t.Fatalf("正文应逐字节透传\nwant %s\ngot  %s", plaintext, capture.body)
	}
	if got := capture.header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("出站内容类型应为 application/json，实为 %q", got)
	}
}
