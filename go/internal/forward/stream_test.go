package forward

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// countingSettler 记录结算调用次数与最近一次终态，用于钉住「只结算一次」。
type countingSettler struct {
	mu      sync.Mutex
	calls   int
	last    StreamOutcome
	settled chan struct{}
}

func newCountingSettler() *countingSettler {
	return &countingSettler{settled: make(chan struct{}, 4)}
}

func (s *countingSettler) SettleStream(_ context.Context, outcome StreamOutcome) error {
	s.mu.Lock()
	s.calls++
	s.last = outcome
	s.mu.Unlock()
	select {
	case s.settled <- struct{}{}:
	default:
	}
	return nil
}

func (s *countingSettler) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *countingSettler) outcome() StreamOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

// streamServer 返回按脚本输出的假上游；脚本形如 [][2]string{chunk, ...}。
//
// flush 为真时每个 chunk 立即下刷，模拟真实 SSE 的逐块到达。
func streamServer(t *testing.T, contentType string, chunks []string, flush bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", contentType)
		w.WriteHeader(http.StatusOK)
		for _, chunk := range chunks {
			_, _ = w.Write([]byte(chunk))
			if flush {
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
			}
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// newStreamFacts 构造一次客户端请求了流式输出的会话事实。
func newStreamFacts() PlanFacts {
	return PlanFacts{Client: newClaudeRequest(claudeStreamRequestBody)}
}

const claudeStreamRequestBody = `{"model":"claude-sonnet-4-5-20250929","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"ping"}]}`

// consumeStream 读完一条流并返回全部字节与终态。
func consumeStream(t *testing.T, stream *Stream) ([]byte, StreamOutcome) {
	t.Helper()
	buffer := make([]byte, 4096)
	var received []byte
	for {
		n, err := stream.Read(buffer)
		if n > 0 {
			received = append(received, buffer[:n]...)
		}
		if err != nil {
			break
		}
	}
	return received, stream.Completion()
}

// claudeStreamChunks 是一段完整的 anthropic 流（含用量与终止标记）。
func claudeStreamChunks() []string {
	return []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-4-5\",\"usage\":{\"input_tokens\":12}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"pong\"}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
}

func TestForwardStreamDeliversGateCommittedStream(t *testing.T) {
	server := streamServer(t, "text/event-stream", claudeStreamChunks(), true)
	settler := newCountingSettler()
	deps := Deps{Dial: newTestDial(t), Facts: newStreamFacts(), Limits: Limits{RetryDelay: time.Millisecond}}

	result, err := ForwardStream(context.Background(), newTestPctx(t), newTestCandidate(1, "供应商甲", server.URL, 1), deps,
		StreamOptions{Format: convert.FormatClaude, Settle: settler, CaptureCommitMarker: true})
	if err != nil {
		t.Fatalf("ForwardStream 失败: %v", err)
	}
	if result.Stream == nil {
		t.Fatal("SSE 响应应当被提交为流")
	}
	stream := result.Stream
	if !stream.attempt.Gated {
		t.Fatal("SSE 响应应经过门控")
	}
	if stream.attempt.Marker == nil {
		t.Fatal("应记录门控提交标记")
	}

	received, outcome := consumeStream(t, stream)
	if string(received) != strings.Join(claudeStreamChunks(), "") {
		t.Fatalf("透传字节与上游不一致: %q", received)
	}
	if outcome.Kind != TerminalCompleted {
		t.Fatalf("终态 = %q，期望 %q（%v）", outcome.Kind, TerminalCompleted, outcome.Err)
	}
	if !outcome.Observation.CompletionMarker {
		t.Fatal("缺少终止标记")
	}
	if outcome.Observation.Model != "claude-sonnet-4-5" {
		t.Fatalf("Model = %q", outcome.Observation.Model)
	}
	if !outcome.Observation.UsageSeen || deref(outcome.Observation.Usage.OutputTokens) != 7 {
		t.Fatalf("用量 = %+v", outcome.Observation.Usage)
	}
	if settler.callCount() != 1 {
		t.Fatalf("结算次数 = %d，期望 1", settler.callCount())
	}
}

func TestForwardStreamFailsOverWhenGateRejectsBeforeContent(t *testing.T) {
	// 第一个供应商在首个有效内容帧之前就吐错误帧：门控拒绝，客户端必须零字节。
	errorFrames := []string{
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"上游过载\",\"status_code\":529}}\n\n",
	}
	rejecting := streamServer(t, "text/event-stream", errorFrames, true)
	healthy := streamServer(t, "text/event-stream", claudeStreamChunks(), true)

	deps := Deps{
		Dial:   newTestDial(t),
		Facts:  newStreamFacts(),
		Limits: Limits{RetryDelay: time.Millisecond},
		Select: func(_ context.Context, excludeIDs []int64) (*Candidate, error) {
			if len(excludeIDs) != 1 || excludeIDs[0] != 1 {
				t.Fatalf("排除列表 = %v", excludeIDs)
			}
			return newTestCandidate(2, "供应商乙", healthy.URL, 1), nil
		},
	}

	result, err := ForwardStream(context.Background(), newTestPctx(t), newTestCandidate(1, "供应商甲", rejecting.URL, 1), deps,
		StreamOptions{Format: convert.FormatClaude})
	if err != nil {
		t.Fatalf("ForwardStream 失败: %v", err)
	}
	if result.Provider.ID != 2 {
		t.Fatalf("最终供应商 = %d，期望切换到 2", result.Provider.ID)
	}
	if len(result.Attempts) != 2 {
		t.Fatalf("尝试留痕 = %d 条", len(result.Attempts))
	}
	if result.Attempts[0].Category != CategoryProviderError {
		t.Fatalf("门控失败的分类 = %q", result.Attempts[0].Category)
	}

	received, outcome := consumeStream(t, result.Stream)
	// 客户端只看到第二个供应商的流：被拒的门控前缀不会被透传。
	if !strings.Contains(string(received), "message_stop") {
		t.Fatalf("透传内容不含终止帧: %q", received)
	}
	if strings.Contains(string(received), "上游过载") {
		t.Fatal("被门控拒绝的前缀泄漏给了客户端")
	}
	if outcome.Kind != TerminalCompleted {
		t.Fatalf("终态 = %q", outcome.Kind)
	}
}

func TestForwardStreamMarksTruncatedUpstream(t *testing.T) {
	// 声明了远大于实际写出量的 Content-Length：客户端会看到意外 EOF。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.Header().Set("content-length", "100000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"半截\"}}\n\n"))
	}))
	t.Cleanup(server.Close)

	deps := Deps{Dial: newTestDial(t), Facts: newStreamFacts(), Limits: Limits{RetryDelay: time.Millisecond}}
	result, err := ForwardStream(context.Background(), newTestPctx(t), newTestCandidate(1, "供应商甲", server.URL, 1), deps,
		StreamOptions{Format: convert.FormatClaude})
	if err != nil {
		t.Fatalf("ForwardStream 失败: %v", err)
	}
	_, outcome := consumeStream(t, result.Stream)
	if outcome.Kind != TerminalLocalError && outcome.Kind != TerminalUpstreamTruncated {
		t.Fatalf("断流终态 = %q（%v）", outcome.Kind, outcome.Err)
	}
	if outcome.Observation.CompletionMarker {
		t.Fatal("断流不应出现终止标记")
	}
}

func TestForwardStreamIdleTimeoutAbortsUpstream(t *testing.T) {
	// 首个内容帧之后长时间不再输出：静默超时必须定终态，而不是一直等。
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"首块\"}}\n\n"))
		w.(http.Flusher).Flush()
		<-release
	}))
	// 注册顺序刻意如此：cleanup 是后进先出，必须先放行 handler 再关服务器，
	// 否则 Server.Close 会一直等在途请求，测试挂死。
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })

	settler := newCountingSettler()
	deps := Deps{Dial: newTestDial(t), Facts: newStreamFacts(), Limits: Limits{RetryDelay: time.Millisecond}}
	result, err := ForwardStream(context.Background(), newTestPctx(t), newTestCandidate(1, "供应商甲", server.URL, 1), deps,
		StreamOptions{
			Format:         convert.FormatClaude,
			Settle:         settler,
			IdleTimeoutFor: func(int64) time.Duration { return 30 * time.Millisecond },
		})
	if err != nil {
		t.Fatalf("ForwardStream 失败: %v", err)
	}

	_, outcome := consumeStream(t, result.Stream)
	if outcome.Kind != TerminalIdleTimeout {
		t.Fatalf("终态 = %q（%v），期望静默超时", outcome.Kind, outcome.Err)
	}
	if !errors.Is(outcome.Err, errStreamIdleTimeout) {
		t.Fatalf("终态错误 = %v", outcome.Err)
	}
	if settler.callCount() != 1 {
		t.Fatalf("结算次数 = %d，期望 1", settler.callCount())
	}
}

func TestStreamClientCancelSettlesExactlyOnce(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"首块\"}}\n\n"))
		w.(http.Flusher).Flush()
		<-release
	}))
	// 注册顺序刻意如此：cleanup 是后进先出，必须先放行 handler 再关服务器，
	// 否则 Server.Close 会一直等在途请求，测试挂死。
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })

	settler := newCountingSettler()
	deps := Deps{Dial: newTestDial(t), Facts: newStreamFacts(), Limits: Limits{RetryDelay: time.Millisecond}}
	result, err := ForwardStream(context.Background(), newTestPctx(t), newTestCandidate(1, "供应商甲", server.URL, 1), deps,
		StreamOptions{Format: convert.FormatClaude, Settle: settler})
	if err != nil {
		t.Fatalf("ForwardStream 失败: %v", err)
	}
	stream := result.Stream

	// 先消费一个块，再模拟客户端断开。
	if _, err := stream.Read(make([]byte, 4096)); err != nil {
		t.Fatalf("首次读取失败: %v", err)
	}
	stream.ClientCancel(errors.New("客户端断开"))

	select {
	case <-settler.settled:
	case <-time.After(2 * time.Second):
		t.Fatal("客户端取消后未结算")
	}
	// 重复触发不得再次结算。
	stream.Close()
	stream.ClientCancel(errors.New("迟到的断开"))
	<-stream.completion

	if settler.callCount() != 1 {
		t.Fatalf("结算次数 = %d，期望 1", settler.callCount())
	}
	outcome := settler.outcome()
	if outcome.Kind != TerminalClientAborted {
		t.Fatalf("终态 = %q", outcome.Kind)
	}
	if !outcome.ClientAbort {
		t.Fatal("终态应记录客户端中断")
	}
}

func TestStreamResidencyStaysBoundedForLargeStream(t *testing.T) {
	const totalBytes = 8 << 20
	chunk := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"" +
		strings.Repeat("x", 30<<10) + "\"}}\n\n"
	iterations := totalBytes / len(chunk)
	chunks := make([]string, 0, iterations+1)
	for range iterations {
		chunks = append(chunks, chunk)
	}
	chunks = append(chunks, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	expectedTotal := 0
	for _, item := range chunks {
		expectedTotal += len(item)
	}

	server := streamServer(t, "text/event-stream", chunks, false)
	deps := Deps{Dial: newTestDial(t), Facts: newStreamFacts(), Limits: Limits{RetryDelay: time.Millisecond}}
	result, err := ForwardStream(context.Background(), newTestPctx(t), newTestCandidate(1, "供应商甲", server.URL, 1), deps,
		StreamOptions{Format: convert.FormatClaude})
	if err != nil {
		t.Fatalf("ForwardStream 失败: %v", err)
	}

	buffer := make([]byte, 32<<10)
	received := 0
	peakRetained := int64(0)
	for {
		n, err := result.Stream.Read(buffer)
		received += n
		if retained := int64(result.Stream.Observation().RetainedBytes); retained > peakRetained {
			peakRetained = retained
		}
		if err != nil {
			break
		}
	}

	outcome := result.Stream.Completion()
	if received != expectedTotal {
		t.Fatalf("透传字节 = %d，期望 %d", received, expectedTotal)
	}
	if outcome.Kind != TerminalCompleted {
		t.Fatalf("终态 = %q（%v）", outcome.Kind, outcome.Err)
	}
	if !outcome.Observation.Truncated {
		t.Fatal("8 MiB 正文应标记窗口截断")
	}
	if peakRetained >= 1<<20 {
		t.Fatalf("单流峰值驻留 = %d 字节，超过 1 MiB", peakRetained)
	}
}

// newTestPctx 构造一个最小可用的请求上下文。
func newTestPctx(t *testing.T) *pctx.Context {
	t.Helper()
	pc, err := pctx.New(pctx.Init{
		Method:  "POST",
		Path:    "/v1/messages",
		Headers: newClientHeaders("content-type", "application/json"),
	})
	if err != nil {
		t.Fatalf("构造 pctx 失败: %v", err)
	}
	return pc
}

// TestStreamClientCancelLeavesNoUpstreamGoroutines 断言客户端中断不会留下上游读取 goroutine。
//
// 断线是常态：每条断线留一个阻塞在上游 Read 上的 goroutine，进程会在流量高峰里
// 慢慢耗光内存与连接。判据是「重复取消后 goroutine 数不再随轮次增长」。
func TestStreamClientCancelLeavesNoUpstreamGoroutines(t *testing.T) {
	// 每轮服务端 handler 都停在自己的 gate 上，直到测试放行：
	// 这样每一轮都是「上游还卡住时客户端就断了」，且 handler 不会长期滞留。
	gates := make(chan chan struct{}, 8)
	var handlersFinished atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"首块\"}}\n\n"))
		w.(http.Flusher).Flush()
		defer handlersFinished.Add(1)
		gate := <-gates
		<-gate
	}))
	t.Cleanup(server.Close)

	deps := Deps{Dial: newTestDial(t), Facts: newStreamFacts(), Limits: Limits{RetryDelay: time.Millisecond}}

	runOnce := func() {
		gate := make(chan struct{})
		gates <- gate
		result, err := ForwardStream(context.Background(), newTestPctx(t), newTestCandidate(1, "供应商甲", server.URL, 1), deps,
			StreamOptions{Format: convert.FormatClaude})
		if err != nil {
			t.Fatalf("ForwardStream 失败: %v", err)
		}
		if _, err := result.Stream.Read(make([]byte, 4096)); err != nil {
			t.Fatalf("首次读取失败: %v", err)
		}
		result.Stream.ClientCancel(errors.New("客户端断开"))
		<-result.Stream.completion
		before := handlersFinished.Load()
		close(gate)
		waitForCondition(t, 5*time.Second, func() bool {
			return handlersFinished.Load() > before
		}, "服务端 handler 未退场")
	}

	// 判据只数「栈上有非测试 forward 代码」的 goroutine：进程里其它包的 goroutine
	// （连接池、测试服务器）与本次不变量无关，把它们算进来只会让结论随负载抖动。
	// 判据取**相对基线的增量**，而不是绝对为零：本包其它用例（尤其竞速的输家 drain 与
	// 排空）在收尾期可能还有 forward 的 goroutine 在跑，绝对计数会把它们的瞬时存活算成
	// 本用例的泄漏。CI 上出现过这种误判：同一秒里先有竞速用例转红，本用例随即跟红在
	// 「首轮未清零」上——那条断言实际报的是前一个用例留下的残响。
	// 本用例要钉的不变量是「重复取消不让计数增长」，故基线口径更贴。
	baseline := forwardGoroutines()
	runOnce()
	waitForCondition(t, 5*time.Second, func() bool { return forwardGoroutines() <= baseline },
		"首轮结束后 forward 包 goroutine 相对基线增长了（上游读取/结算 goroutine 泄漏）")

	for range 4 {
		runOnce()
	}
	waitForCondition(t, 5*time.Second, func() bool { return forwardGoroutines() <= baseline },
		"重复取消后 forward 包 goroutine 相对基线增长了（上游读取/结算 goroutine 泄漏）")
}

// forwardGoroutines 统计栈上出现 forward 包非测试代码的 goroutine 数。
func forwardGoroutines() int {
	buffer := make([]byte, 1<<20)
	written := runtime.Stack(buffer, true)
	count := 0
	for _, block := range strings.Split(string(buffer[:written]), "\n\n") {
		if !strings.Contains(block, "/internal/forward/") || strings.Contains(block, "_test.go") {
			continue
		}
		count++
	}
	return count
}

// TestStreamDeliversBufferedPrefixWhenGateReadToEOF 断言门控读尽上游时前缀即全部正文，
// 且上游仍被按约定关闭（否则连接会漏）。
func TestStreamDeliversBufferedPrefixWhenGateReadToEOF(t *testing.T) {
	source := newFakeSource()
	attempt := &streamAttempt{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Prefix:     [][]byte{[]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")},
		Source:     source,
		ReaderDone: true,
		Gated:      true,
	}

	settler := newCountingSettler()
	stream := newStream(context.Background(), attempt, Provider{ID: 1, Name: "供应商甲"}, Endpoint{}, nil, nil,
		StreamOptions{Format: convert.FormatClaude, Settle: settler}, nil)

	received, outcome := consumeStream(t, stream)
	if string(received) != "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n" {
		t.Fatalf("前缀未完整交付: %q", received)
	}
	if outcome.Kind != TerminalCompleted {
		t.Fatalf("终态 = %q", outcome.Kind)
	}
	if source.closeCount() == 0 {
		t.Fatal("上游未被关闭")
	}
	if settler.callCount() != 1 {
		t.Fatalf("结算次数 = %d", settler.callCount())
	}
}
