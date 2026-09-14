package gate

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"
)

func anthropicContentFrame(text string) string {
	return "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"" +
		text + "\"}}\n\n"
}

func responsesEchoFrame(payloadBytes int) string {
	return "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"instructions\":\"" +
		strings.Repeat("e", payloadBytes) + "\"}}\n\n"
}

func newOptions(family Family) Options {
	return Options{
		Family:            family,
		ProviderID:        7,
		ProviderName:      "provider-under-test",
		PrebufferEventCap: 64,
		PrebufferByteCap:  4096,
	}
}

func TestRunCommitsOnFirstContentFrame(t *testing.T) {
	body := anthropicContentFrame("hello")
	result, err := Run(context.Background(), strings.NewReader(body), newOptions(FamilyAnthropic))
	if err != nil {
		t.Fatalf("首个内容帧应提交: %v", err)
	}
	if string(result.PrefixBytes()) != body {
		t.Fatalf("前缀应等于上游字节: %q", result.PrefixBytes())
	}
	if result.FramesSeen != 1 {
		t.Fatalf("帧数应为 1，得到 %d", result.FramesSeen)
	}
	if result.ReaderDone {
		t.Fatal("提交发生在上游 EOF 之前")
	}
	if result.Lease != nil {
		t.Fatal("未传入预算时不应有租约")
	}
	if result.Marker != nil {
		t.Fatal("未开启 CaptureCommitMarker 时不应有标记")
	}
}

func TestRunCommitsWithNeutralPrefix(t *testing.T) {
	options := newOptions(FamilyOpenAIChat)
	options.CaptureCommitMarker = true
	body := "data: {\"choices\":[{\"delta\":{}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
	result, err := Run(context.Background(), strings.NewReader(body), options)
	if err != nil {
		t.Fatalf("中性前缀后应提交: %v", err)
	}
	if result.FramesSeen != 2 {
		t.Fatalf("应看到 2 帧，得到 %d", result.FramesSeen)
	}
	if string(result.PrefixBytes()) != body {
		t.Fatalf("前缀应含中性帧: %q", result.PrefixBytes())
	}
	marker := result.Marker
	if marker == nil {
		t.Fatal("应产出提交标记")
	}
	if marker.FrameIndex != 2 || marker.ChunkIndex != 1 || marker.EventName != "" {
		t.Fatalf("标记不符: %+v", marker)
	}
	if marker.BufferedBytes != len(body) {
		t.Fatalf("标记缓冲字节应为 %d，得到 %d", len(body), marker.BufferedBytes)
	}
}

func TestRunCommitsOnTrailingFrameAtEOF(t *testing.T) {
	// 无结尾空行：帧只能由 EOF 冲刷产出，故提交时已读到上游 EOF。
	body := "event: content_block_delta\ndata: {\"delta\":{\"text\":\"tail\"}}"
	result, err := Run(context.Background(), strings.NewReader(body), newOptions(FamilyAnthropic))
	if err != nil {
		t.Fatalf("尾部帧应提交: %v", err)
	}
	if !result.ReaderDone {
		t.Fatal("EOF 冲刷路径应标记 ReaderDone")
	}
	if string(result.PrefixBytes()) != body {
		t.Fatalf("前缀不符: %q", result.PrefixBytes())
	}
}

func TestRunFailureReasons(t *testing.T) {
	cases := []struct {
		name       string
		family     Family
		body       string
		wantReason FailReason
		wantTerm   bool
		wantFrame  string
	}{
		{
			name:       "上游错误帧",
			family:     FamilyAnthropic,
			body:       "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"overloaded\"}}\n\n",
			wantReason: FailGateError,
			wantFrame:  `{"type":"error","error":{"message":"overloaded"}}`,
		},
		{
			name:       "chat 流中 error",
			family:     FamilyOpenAIChat,
			body:       "data: {\"error\":{\"message\":\"rate limited\"}}\n\n",
			wantReason: FailGateError,
			wantFrame:  `{"error":{"message":"rate limited"}}`,
		},
		{
			name:       "损坏载荷",
			family:     FamilyOpenAIChat,
			body:       "data: {broken\n\n",
			wantReason: FailDecodeError,
		},
		{
			name:       "非 JSON 中性载荷",
			family:     FamilyAnthropic,
			body:       "event: ping\ndata: pong\n\n",
			wantReason: FailDecodeError,
		},
		{
			name:       "终止帧先于内容",
			family:     FamilyAnthropic,
			body:       "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
			wantReason: FailEmptyStream,
			wantTerm:   true,
		},
		{
			name:       "无终止帧的 EOF",
			family:     FamilyAnthropic,
			body:       "",
			wantReason: FailEmptyStream,
		},
		{
			name:       "中性帧后 EOF",
			family:     FamilyOpenAIChat,
			body:       "data: {\"choices\":[{\"delta\":{}}]}\n\n",
			wantReason: FailEmptyStream,
		},
		{
			name:       "gemini 安全拦截",
			family:     FamilyGemini,
			body:       "data: {\"candidates\":[{\"finishReason\":\"SAFETY\"}]}\n\n",
			wantReason: FailGateError,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result, err := Run(context.Background(), strings.NewReader(testCase.body), newOptions(testCase.family))
			if err == nil {
				t.Fatalf("应失败，却提交了 %q", result.PrefixBytes())
			}
			if len(result.Prefix) != 0 || result.Lease != nil {
				t.Fatalf("失败时不得交出前缀或租约: %+v", result)
			}
			var precommit *PrecommitError
			if !errors.As(err, &precommit) {
				t.Fatalf("应为 *PrecommitError，得到 %T: %v", err, err)
			}
			if precommit.Reason != testCase.wantReason {
				t.Fatalf("原因应为 %s，得到 %s", testCase.wantReason, precommit.Reason)
			}
			if precommit.TerminalBeforeContent != testCase.wantTerm {
				t.Fatalf("terminalBeforeContent 应为 %v", testCase.wantTerm)
			}
			if precommit.ProviderID != 7 || precommit.ProviderName != "provider-under-test" {
				t.Fatalf("供应商身份应带入错误: %+v", precommit)
			}
			if testCase.wantFrame != "" {
				if precommit.FrameData != testCase.wantFrame {
					t.Fatalf("错误帧原文应为 %q，得到 %q", testCase.wantFrame, precommit.FrameData)
				}
				if GateErrorBody(precommit) != testCase.wantFrame {
					t.Fatalf("gate_error 响应体应为上游原文，得到 %q", GateErrorBody(precommit))
				}
			}
			if strings.Contains(err.Error(), "Stream content gate rejected upstream before first valid content") == false {
				t.Fatalf("错误文案应与 TS 一致: %v", err)
			}
		})
	}
}

func TestRunResponsesCleanAndIncompleteCompletion(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "干净完成但无可见内容",
			body: "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n",
		},
		{
			name: "incomplete 也透传",
			body: "event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\"}}\n\n",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result, err := Run(context.Background(), strings.NewReader(testCase.body), newOptions(FamilyOpenAIResponses))
			if err != nil {
				t.Fatalf("不应失败: %v", err)
			}
			if string(result.PrefixBytes()) != testCase.body {
				t.Fatalf("前缀不符: %q", result.PrefixBytes())
			}
		})
	}
}

func TestRunResponsesEmptyStreamIsRequestScoped(t *testing.T) {
	// 终止帧先于内容：openai-responses 的空结果是请求作用域，不计熔断。
	body := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"failed\"}}\n\n"
	_, err := Run(context.Background(), strings.NewReader(body), newOptions(FamilyOpenAIResponses))
	if err == nil {
		t.Fatal("status=failed 的完成帧不是干净完成，应失败")
	}
	if !IsRequestScopedGateFailure(err) {
		t.Fatalf("responses 的空流应判为请求作用域: %v", err)
	}

	_, anthropicErr := Run(context.Background(), strings.NewReader(
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"), newOptions(FamilyAnthropic))
	if IsRequestScopedGateFailure(anthropicErr) {
		t.Fatal("anthropic 的空流是真实供应商异常，不得判为请求作用域")
	}

	// EOF 断流（无终止帧）必须计入熔断。
	_, brokenErr := Run(context.Background(), strings.NewReader(""), newOptions(FamilyOpenAIResponses))
	if IsRequestScopedGateFailure(brokenErr) {
		t.Fatal("断流必须计入熔断")
	}
	if IsRequestScopedGateFailure(errors.New("其它错误")) {
		t.Fatal("非门控错误不得判为请求作用域")
	}
}

func TestRunEventCapOverflow(t *testing.T) {
	options := newOptions(FamilyOpenAIChat)
	options.PrebufferEventCap = 2
	body := "data: {\"choices\":[{\"delta\":{}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{}}]}\n\n"
	_, err := Run(context.Background(), strings.NewReader(body), options)
	assertPrecommitReason(t, err, FailPrebufferOverflow)
}

func TestRunByteCapOverflow(t *testing.T) {
	options := newOptions(FamilyOpenAIChat)
	options.PrebufferByteCap = 256
	options.PrebufferEventCap = 64
	body := "data: {\"choices\":[{\"delta\":{}}],\"pad\":\"" + strings.Repeat("p", 200) + "\"}\n\n" +
		"data: {\"choices\":[{\"delta\":{}}],\"pad\":\"" + strings.Repeat("p", 200) + "\"}\n\n"
	_, err := Run(context.Background(), strings.NewReader(body), options)
	assertPrecommitReason(t, err, FailPrebufferOverflow)
}

func TestRunOversizedSingleChunk(t *testing.T) {
	options := newOptions(FamilyOpenAIChat)
	options.PrebufferByteCap = 64
	// 单个 chunk 直接超过 2×cap：在持有引用之前就应判 overflow。
	chunk := []byte("data: {\"choices\":[{\"delta\":{}}],\"pad\":\"" + strings.Repeat("p", 200) + "\"}\n\n")
	_, err := Run(context.Background(), &chunkedReader{chunks: [][]byte{chunk}}, options)
	assertPrecommitReason(t, err, FailPrebufferOverflow)
}

func TestRunParserBufferLimitBecomesOverflow(t *testing.T) {
	options := newOptions(FamilyOpenAIChat)
	options.PrebufferByteCap = 128
	// 未终止的超大 data 行：parser 保留状态超限 → 归为 overflow。
	body := strings.Repeat("data: "+strings.Repeat("x", 64)+"\n", 8)
	_, err := Run(context.Background(), strings.NewReader(body), options)
	assertPrecommitReason(t, err, FailPrebufferOverflow)
}

func TestRunRequestEchoExemption(t *testing.T) {
	options := newOptions(FamilyOpenAIResponses)
	options.PrebufferByteCap = 512
	options.CaptureCommitMarker = true
	echo := responsesEchoFrame(600) // 单帧载荷超过 cap，但未超 2×cap
	content := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"
	result, err := Run(context.Background(), strings.NewReader(echo+content), options)
	if err != nil {
		t.Fatalf("请求回显帧应被字节豁免: %v", err)
	}
	if result.FramesSeen != 2 {
		t.Fatalf("应看到 2 帧，得到 %d", result.FramesSeen)
	}
	if result.Marker == nil || result.Marker.EchoExcludedBytes < 600 {
		t.Fatalf("应记录被排除的回显字节: %+v", result.Marker)
	}

	// 对照：同一形态换成非豁免事件即 overflow。
	notEcho := strings.Replace(echo, "response.created", "response.output_item.added", 1)
	_, notEchoErr := Run(context.Background(), strings.NewReader(notEcho+content), options)
	assertPrecommitReason(t, notEchoErr, FailPrebufferOverflow)
}

func TestRunOnFirstByteFiresOnce(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"
	options := newOptions(FamilyOpenAIChat)
	var mu sync.Mutex
	calls := 0
	options.OnFirstByte = func() {
		mu.Lock()
		defer mu.Unlock()
		calls++
	}
	reader := &chunkedReader{chunks: [][]byte{[]byte(body[:20]), []byte(body[20:])}}
	if _, err := Run(context.Background(), reader, options); err != nil {
		t.Fatalf("应提交: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("OnFirstByte 应只回调一次，得到 %d", calls)
	}
}

func TestRunIdleTimeout(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	reader := &blockingReader{first: []byte("event: ping\ndata: {}\n\n"), release: release}
	options := newOptions(FamilyAnthropic)
	options.IdleTimeout = 20 * time.Millisecond
	_, err := Run(context.Background(), reader, options)
	assertPrecommitReason(t, err, FailIdleTimeout)
}

func TestRunContextCancel(t *testing.T) {
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	ctx, cancel := context.WithCancel(context.Background())
	reader := &blockingReader{first: []byte("event: ping\ndata: {}\n\n"), release: release}
	options := newOptions(FamilyAnthropic)
	options.IdleTimeout = time.Second
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := Run(ctx, reader, options)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应返回 context.Canceled，得到 %v", err)
	}
}

func TestRunReadErrorPropagates(t *testing.T) {
	readErr := errors.New("connection reset by peer")
	_, err := Run(context.Background(), &failingReader{err: readErr}, newOptions(FamilyAnthropic))
	if !errors.Is(err, readErr) {
		t.Fatalf("读错误应原样上抛，得到 %v", err)
	}
}

func TestRunRejectsInvalidOptions(t *testing.T) {
	options := newOptions(FamilyAnthropic)
	options.PrebufferByteCap = 0
	if _, err := Run(context.Background(), strings.NewReader(""), options); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("应报 ErrInvalidOptions，得到 %v", err)
	}
	options = newOptions(FamilyAnthropic)
	options.PrebufferEventCap = 0
	if _, err := Run(context.Background(), strings.NewReader(""), options); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("应报 ErrInvalidOptions，得到 %v", err)
	}
}

func TestRunBudgetLeaseLifecycle(t *testing.T) {
	body := anthropicContentFrame("hello")
	options := newOptions(FamilyAnthropic)
	options.PrebufferByteCap = 1024
	budget := NewBudget(func() int { return 4096 })
	options.Budget = budget

	result, err := Run(context.Background(), strings.NewReader(body), options)
	if err != nil {
		t.Fatalf("应提交: %v", err)
	}
	if result.Lease == nil {
		t.Fatal("提交时应移交租约")
	}
	if result.Lease.ReservedBytes() != len(body) {
		t.Fatalf("提交后租约应收缩到前缀大小 %d，得到 %d", len(body), result.Lease.ReservedBytes())
	}
	result.Lease.Release()
	if snapshot := budget.Snapshot(); snapshot.ReservedBytes != 0 {
		t.Fatalf("释放后应清零: %+v", snapshot)
	}
}

func TestRunBudgetReleasedOnFailure(t *testing.T) {
	budget := NewBudget(func() int { return 4096 })
	options := newOptions(FamilyAnthropic)
	options.PrebufferByteCap = 1024
	options.Budget = budget
	_, err := Run(context.Background(), strings.NewReader("event: error\ndata: {\"error\":{}}\n\n"), options)
	assertPrecommitReason(t, err, FailGateError)
	if snapshot := budget.Snapshot(); snapshot.ReservedBytes != 0 || snapshot.Waiting != 0 {
		t.Fatalf("失败时应归还全部预算: %+v", snapshot)
	}
}

func TestRunWaitsForBudget(t *testing.T) {
	budget := NewBudget(func() int { return 4096 })
	held, err := budget.Reserve(context.Background(), 4096)
	if err != nil {
		t.Fatalf("占住预算失败: %v", err)
	}

	var mu sync.Mutex
	events := []string{}
	options := newOptions(FamilyAnthropic)
	options.PrebufferByteCap = 1024
	options.Budget = budget
	options.OnBudgetWaitStart = func() {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, "start")
	}
	options.OnBudgetWaitEnd = func() {
		mu.Lock()
		defer mu.Unlock()
		events = append(events, "end")
	}

	done := make(chan error, 1)
	go func() {
		_, runErr := Run(context.Background(), strings.NewReader(anthropicContentFrame("hello")), options)
		done <- runErr
	}()

	waitForWaiting(t, budget, 1)
	held.Release()

	select {
	case runErr := <-done:
		if runErr != nil {
			t.Fatalf("预算释放后应提交: %v", runErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("等待预算超时")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 2 || events[0] != "start" || events[1] != "end" {
		t.Fatalf("等待预算应成对回调 start/end，得到 %v", events)
	}
}

func TestRunNormalPathDoesNotPauseBudgetTimers(t *testing.T) {
	budget := NewBudget(func() int { return 4096 })
	options := newOptions(FamilyAnthropic)
	options.PrebufferByteCap = 1024
	options.Budget = budget
	called := false
	options.OnBudgetWaitStart = func() { called = true }
	if _, err := Run(context.Background(), strings.NewReader(anthropicContentFrame("hello")), options); err != nil {
		t.Fatalf("应提交: %v", err)
	}
	if called {
		t.Fatal("热路径不应进入预算等待")
	}
}

// TestRunSlicingInvariance 是属性式测试：同一上游字节按任意切片喂入，
// 帧计数与失败原因必须一致，且前缀永远是上游字节的前缀。
func TestRunSlicingInvariance(t *testing.T) {
	bodies := []struct {
		name      string
		body      string
		family    Family
		wantErr   bool
		wantCause FailReason
	}{
		{
			name:   "内容帧提交",
			family: FamilyAnthropic,
			body:   "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" + anthropicContentFrame("hello 世界") + "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		},
		{
			name:   "chat 内容帧提交",
			family: FamilyOpenAIChat,
			body:   "data: {\"choices\":[{\"delta\":{}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n",
		},
		{
			name:      "错误帧失败",
			family:    FamilyAnthropic,
			wantErr:   true,
			wantCause: FailGateError,
			body:      "event: message_start\ndata: {\"type\":\"message_start\"}\n\nevent: error\ndata: {\"error\":{\"message\":\"x\"}}\n\n",
		},
		{
			name:      "事件上限失败",
			family:    FamilyOpenAIChat,
			wantErr:   true,
			wantCause: FailPrebufferOverflow,
			body:      strings.Repeat("data: {\"choices\":[{\"delta\":{}}]}\n\n", 6),
		},
		{
			name:      "空流失败",
			family:    FamilyAnthropic,
			wantErr:   true,
			wantCause: FailEmptyStream,
			body:      "event: ping\ndata: {\"type\":\"ping\"}\n\n",
		},
	}

	random := rand.New(rand.NewSource(20260912))
	for _, testCase := range bodies {
		t.Run(testCase.name, func(t *testing.T) {
			options := newOptions(testCase.family)
			options.PrebufferEventCap = 4
			baseline, _ := Run(context.Background(), strings.NewReader(testCase.body), options)
			for round := range 100 {
				chunks := randomChunks(random, testCase.body)
				result, err := Run(context.Background(), &chunkedReader{chunks: chunks}, options)
				if (err != nil) != testCase.wantErr {
					t.Fatalf("第 %d 轮失败与否不一致: %v", round, err)
				}
				if testCase.wantErr {
					var precommit *PrecommitError
					if !errors.As(err, &precommit) || precommit.Reason != testCase.wantCause {
						t.Fatalf("第 %d 轮原因不一致: %v", round, err)
					}
					if len(result.Prefix) != 0 {
						t.Fatalf("第 %d 轮失败时不得交出前缀", round)
					}
					continue
				}
				if result.FramesSeen != baseline.FramesSeen {
					t.Fatalf("第 %d 轮帧数不一致: %d vs %d", round, result.FramesSeen, baseline.FramesSeen)
				}
				prefix := result.PrefixBytes()
				if !bytes.HasPrefix([]byte(testCase.body), prefix) {
					t.Fatalf("第 %d 轮前缀不是上游字节的前缀: %q", round, prefix)
				}
				if !bytes.Contains(prefix, []byte(`"text":"hello 世界"`)) &&
					!bytes.Contains(prefix, []byte(`"content":"ok"`)) {
					t.Fatalf("第 %d 轮前缀未包含触发提交的内容帧: %q", round, prefix)
				}
			}
		})
	}
}

func TestConcatPrefix(t *testing.T) {
	if ConcatPrefix(nil) != nil {
		t.Fatal("空切片应返回 nil")
	}
	single := []byte("abc")
	if &ConcatPrefix([][]byte{single})[0] != &single[0] {
		t.Fatal("单块应零拷贝返回")
	}
	joined := ConcatPrefix([][]byte{[]byte("ab"), []byte("cd"), nil})
	if string(joined) != "abcd" {
		t.Fatalf("拼接结果不符: %q", joined)
	}
}

func TestGateErrorBody(t *testing.T) {
	precommit := &PrecommitError{
		Reason:            FailPrebufferOverflow,
		Family:            FamilyOpenAIChat,
		FramesSeen:        65,
		BufferedBytes:     2048,
		EchoExcludedBytes: 700,
		FrameData:         strings.Repeat("f", 900),
	}
	body := GateErrorBody(precommit)
	for _, want := range []string{
		`"type":"stream_gate_precommit"`,
		`"reason":"prebuffer_overflow"`,
		`"family":"openai-chat"`,
		`"frames_seen":65`,
		`"buffered_bytes":2048`,
		`"echo_excluded_bytes":700`,
		`"frame_preview":"` + strings.Repeat("f", 500) + `"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("响应体缺少 %s: %s", want, body)
		}
	}
	if strings.Contains(body, "terminal_before_content") {
		t.Fatalf("非空流原因不应带 terminal_before_content: %s", body)
	}

	empty := GateErrorBody(&PrecommitError{Reason: FailEmptyStream, Family: FamilyOpenAIResponses, TerminalBeforeContent: true})
	if !strings.Contains(empty, `"terminal_before_content":true`) {
		t.Fatalf("空流应带 terminal_before_content: %s", empty)
	}

	truncated := GateErrorBody(&PrecommitError{Reason: FailGateError, FrameData: strings.Repeat("g", 3000)})
	if len(truncated) != 2000 {
		t.Fatalf("gate_error 应截断到 2000 字节，得到 %d", len(truncated))
	}
	withoutFrame := GateErrorBody(&PrecommitError{Reason: FailGateError, Family: FamilyAnthropic})
	if !strings.Contains(withoutFrame, `"reason":"gate_error"`) {
		t.Fatalf("无原文的 gate_error 应走结构化分支: %s", withoutFrame)
	}
}

func TestParseMode(t *testing.T) {
	for _, value := range []string{"off", "shadow", "enforce"} {
		mode, ok := ParseMode(value)
		if !ok || string(mode) != value {
			t.Fatalf("ParseMode(%q) 失败", value)
		}
	}
	if _, ok := ParseMode("strict"); ok {
		t.Fatal("未知模式应返回 false")
	}
}

func assertPrecommitReason(t *testing.T, err error, want FailReason) {
	t.Helper()
	var precommit *PrecommitError
	if !errors.As(err, &precommit) {
		t.Fatalf("应为 *PrecommitError，得到 %T: %v", err, err)
	}
	if precommit.Reason != want {
		t.Fatalf("原因应为 %s，得到 %s", want, precommit.Reason)
	}
}

func randomChunks(random *rand.Rand, body string) [][]byte {
	var chunks [][]byte
	offset := 0
	for offset < len(body) {
		size := 1 + random.Intn(7)
		end := offset + size
		if end > len(body) {
			end = len(body)
		}
		chunks = append(chunks, []byte(body[offset:end]))
		offset = end
	}
	return chunks
}

type chunkedReader struct {
	chunks [][]byte
	index  int
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	if r.index >= len(r.chunks) {
		return 0, io.EOF
	}
	chunk := r.chunks[r.index]
	r.index++
	return copy(p, chunk), nil
}

type blockingReader struct {
	first   []byte
	sent    bool
	release chan struct{}
}

func (r *blockingReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, r.first), nil
	}
	<-r.release
	return 0, io.EOF
}

type failingReader struct {
	err error
}

func (r *failingReader) Read([]byte) (int, error) {
	return 0, r.err
}
