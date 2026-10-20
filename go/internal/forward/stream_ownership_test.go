package forward

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/gate"
)

// closeTracker 记录 Close 是否被调用过，用来观察「返回时流是否已被关闭」。
type closeTracker struct {
	inner  io.ReadCloser
	closed atomic.Bool
}

func (b *closeTracker) Read(p []byte) (int, error) { return b.inner.Read(p) }
func (b *closeTracker) Close() error               { b.closed.Store(true); return b.inner.Close() }

// TestNonGatedStreamOwnsNoCloseDefer 钉住流所有权：
// 非门控流式分支返回时**不得**已关闭上游正文，否则调用方读到 0 字节。
//
// 实证来源：端到端测量在 gate=off 时观察到两次 HTTP 200 但响应体 0 字节，
// 随后 4/4 次 503。根因是 stream.go 的 `defer response.Body.Close()` 与
// `Source: response.Body` 同处一个分支：defer 在函数返回时执行，把借出去的流关掉了。
func TestNonGatedStreamOwnsNoCloseDefer(t *testing.T) {
	const payload = "data: {\"type\":\"message_start\"}\n\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hello\"}}\n\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, payload)
		w.(http.Flusher).Flush()
	}))
	defer server.Close()

	deps := Deps{
		Dial:   newTestDial(t),
		Facts:  newTestFacts(),
		Limits: Limits{},
	}
	provider := Provider{ID: 1, Name: "供应商甲", Type: convert.ProviderClaude, URL: server.URL, Key: "sk-test"}
	plan, err := BuildPlan(PlanInput{
		Client:            newClaudeRequest(claudeRequestBody),
		Target:            Target{Provider: provider, Endpoint: Endpoint{URL: server.URL}},
		ConversionEnabled: false,
	})
	if err != nil {
		t.Fatalf("构造计划失败: %v", err)
	}
	outcome := &AttemptOutcome{ProviderID: 1, ProviderName: "供应商甲", Attempt: 1}
	options := StreamOptions{
		Format:   convert.FormatClaude,
		GateMode: gate.ModeOff, // 关掉门控：bug 所在的正是这条分支
	}

	resp, fail := deps.executeStreamAttempt(context.Background(), newTestPctx(t), provider, plan, outcome, options)
	if fail != nil {
		t.Fatalf("非门控流式尝试失败: %s", fail.Message)
	}
	if resp == nil || resp.Stream == nil {
		t.Fatalf("未返回流（resp=%+v）", resp)
	}

	// 核心断言：读得到正文（若 Source 已被关闭，这里读到 0 字节或直接报错）。
	body, readErr := io.ReadAll(resp.Stream.Source)
	if readErr != nil {
		t.Fatalf("读取流失败（Source 可能已被提前关闭）: %v", readErr)
	}
	if len(body) == 0 {
		t.Fatal("读到的正文为 0 字节：Source 已被提前关闭（流所有权 bug 复发）")
	}
	_ = resp.Stream.Source.Close()
}
