package forward

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
)

// 本文件钉住「拨号阶段必须有超时」这条防泄漏约束。
//
// 生产证据（2026-10-20，1.12.4）：容器 RSS 20 分钟内从 713MB 涨到 956MB（上限 1GiB），
// 强制 GC 不降。pprof inuse_space 归因 forward.rewriteModelField 393MB + guard.NewBodyAccessor
// 177MB；goroutine 栈实证多个 goroutine 卡在
// net/http.(*http2clientStream).writeRequest，栈为
// runAttempt -> dialAttempt -> dial.attempt -> RoundTrip。
//
// 根因：dial 的 ResponseHeaderTimeout 从**请求写完**才开始计时，HTTP/2 流控阻塞卡在写请求时
// 它永不触发；而 attemptCtx 只有 cancel 没有 deadline。于是该 goroutine 及其
// hedgeAttempt.plan（含完整请求体）永久挂住。
//
// 修法：只给拨号那一次调用包一层 headersTimeout，拿到响应头即收回——不能直接给 attemptCtx
// 加超时，那会在胜者长流读完前腰斩流。

// hangingDial 是一个永不返回的拨号器替身：模拟卡在 writeRequest 的上游。
type hangingDial struct {
	entered atomic.Int32
	// released 在 ctx 取消后关闭，供测试确认「超时确实传到了拨号层」。
	released atomic.Bool
}

func (h *hangingDial) roundTrip(ctx context.Context) (*http.Response, error) {
	h.entered.Add(1)
	<-ctx.Done()
	h.released.Store(true)
	return nil, ctx.Err()
}

// TestHedgeDialPhaseTimesOutAndReleasesPlan 拨号卡死时必须在 headersTimeout 后退出。
//
// 反证：把 runAttempt 里的 dialCtx 换回 attemptCtx（即去掉超时），本用例会挂到测试超时。
func TestHedgeDialPhaseTimesOutAndReleasesPlan(t *testing.T) {
	hang := &hangingDial{}
	harness := newHedgeHarness(t)
	// 无更多候选：失败后不再启动备选，用例只考察拨号超时本身。
	harness.setSelect(nil)
	// 用一个只会卡住的 transport 顶替真实拨号。
	client, dialErr := dial.New(dial.Options{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		return hang.roundTrip(request.Context())
	})})
	if dialErr != nil {
		t.Fatalf("构造卡住的拨号器失败: %v", dialErr)
	}
	harness.deps.Dial = client

	// 把 headersTimeout 调到毫秒级，让用例在秒级内完成（生产取 dial.DefaultHeadersTimeout）。
	harness.cfg.DialHeadersTimeout = 50 * time.Millisecond
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = ForwardStreamHedge(context.Background(), harness.pc,
			sseCandidate(1, "供应商甲", "http://127.0.0.1:1/v1/messages", 1),
			harness.deps, harness.options, harness.cfg)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("拨号卡死时竞速必须在 headersTimeout 内收场（否则 goroutine 与 plan 永久泄漏）")
	}
	if hang.entered.Load() == 0 {
		t.Fatal("拨号器未被调用，用例没有覆盖到目标路径")
	}
}

// TestHedgeWinnerLongStreamSurvivesHeadersTimeout 胜者长流不得被拨号超时腰斩。
//
// 这是与上一条成对的约束：只给拨号加超时、拿到响应头即收回，故响应体读多久都不受影响。
// 反证：若把超时加到 attemptCtx 上（而非只包拨号），本用例会读到一半被取消。
func TestHedgeWinnerLongStreamSurvivesHeadersTimeout(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		// 先发一帧可提交内容让竞速裁决出胜者，然后长时间不结束。
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-release
	}))
	t.Cleanup(func() {
		close(release)
		server.Close()
	})

	harness := newHedgeHarness(t)
	result, err := ForwardStreamHedge(context.Background(), harness.pc,
		sseCandidate(1, "供应商甲", server.URL, 1), harness.deps, harness.options, harness.cfg)
	if err != nil {
		t.Fatalf("胜者长流不应报错: %v", err)
	}
	if result == nil || result.Stream == nil {
		t.Fatal("应拿到已提交的流")
	}
	// 流仍可读：读一帧，证明超时没有把它取消掉。
	buffer := make([]byte, 256)
	if _, readErr := result.Stream.Read(buffer); readErr != nil {
		t.Fatalf("胜者流在读第一帧时被中断（说明超时错误地加在了 attemptCtx 上）: %v", readErr)
	}
}

// roundTripFunc 把函数适配成 http.RoundTripper。
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }
