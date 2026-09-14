package forward

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// manualTimer 是可手动触发的阈值计时器（配合 HedgeOptions.After 注入）。
type manualTimer struct {
	mu    sync.Mutex
	fn    func()
	fired bool
}

func newManualTimer(fn func()) *manualTimer {
	return &manualTimer{fn: fn}
}

func (t *manualTimer) Stop() bool { return true }

func (t *manualTimer) fire() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.fired {
		return
	}
	t.fired = true
	t.fn()
}

// hedgeHarness 是竞速测试的公共装配：可控上游 + 可触发阈值计时器。
type hedgeHarness struct {
	t           *testing.T
	pc          *pctx.Context
	deps        Deps
	options     StreamOptions
	cfg         HedgeOptions
	timerCh     chan *manualTimer
	selects     int
	selectCalls chan struct{}
}

// newHedgeHarness 构造竞速测试装配。
func newHedgeHarness(t *testing.T) *hedgeHarness {
	t.Helper()
	harness := &hedgeHarness{
		t:           t,
		pc:          newTestPctx(t),
		timerCh:     make(chan *manualTimer, 8),
		selectCalls: make(chan struct{}, 8),
	}
	harness.deps = Deps{
		Dial:   newTestDial(t),
		Facts:  newStreamFacts(),
		Limits: Limits{RetryDelay: time.Millisecond},
	}
	harness.options = StreamOptions{
		Format:              convert.FormatClaude,
		CaptureCommitMarker: true,
		ChunkBytes:          1024,
		Now:                 time.Now,
	}
	harness.cfg = HedgeOptions{
		MaxInFlight:       2,
		LoserDrainTimeout: 5 * time.Second,
		After: func(_ time.Duration, fn func()) hedgeTimer {
			timer := newManualTimer(fn)
			harness.timerCh <- timer
			return timer
		},
		Now: time.Now,
	}
	return harness
}

// setSelect 注入按排除列表返回候选的 Select（依次尝试 secondary 列表）。
func (h *hedgeHarness) setSelect(secondaries []*Candidate) {
	h.deps.Select = func(_ context.Context, excludeIDs []int64) (*Candidate, error) {
		h.selects++
		h.selectCalls <- struct{}{}
		for _, candidate := range secondaries {
			skip := false
			for _, id := range excludeIDs {
				if id == candidate.Provider.ID {
					skip = true
					break
				}
			}
			if skip {
				continue
			}
			return candidate, nil
		}
		return nil, nil
	}
}

// withBillLosers 开启输家计费并挂上记录器。
func (h *hedgeHarness) withBillLosers(biller HedgeLoserBiller) *hedgeHarness {
	h.cfg.BillLosers = true
	h.cfg.LoserBiller = biller
	return h
}

// withRequestID 在请求上下文上开一行。
func (h *hedgeHarness) withRequestID(id int64) *hedgeHarness {
	h.t.Helper()
	if err := h.pc.SetMessageRequestID(id); err != nil {
		h.t.Fatalf("写入请求行 id 失败: %v", err)
	}
	return h
}

// sseCandidate 构造一个指向指定地址的 claude 流式候选。
func sseCandidate(id int64, name, url string, thresholdMS int) *Candidate {
	return &Candidate{
		Provider: Provider{
			ID:                          id,
			Name:                        name,
			Type:                        convert.ProviderClaude,
			Key:                         "sk-test",
			URL:                         url,
			MaxRetryAttempts:            intPointer(1),
			FirstByteTimeoutStreamingMS: thresholdMS,
		},
	}
}

func intPointer(value int) *int { return &value }

// UsageSeen 报告计费证据里是否存在可用量。
func (b HedgeLoserBill) UsageSeen() bool {
	return !b.Usage.IsEmpty()
}

// billRecorder 是输家计费接缝的记录器：断言恰好调用一次且不重复。
type billRecorder struct {
	mu    sync.Mutex
	calls []HedgeLoserBill
	done  chan struct{}
}

func newBillRecorder() *billRecorder {
	return &billRecorder{done: make(chan struct{}, 4)}
}

func (b *billRecorder) BillLoser(_ context.Context, bill HedgeLoserBill) error {
	b.mu.Lock()
	b.calls = append(b.calls, bill)
	b.mu.Unlock()
	select {
	case b.done <- struct{}{}:
	default:
	}
	return nil
}

func (b *billRecorder) billCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.calls)
}

func (b *billRecorder) last() HedgeLoserBill {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.calls) == 0 {
		return HedgeLoserBill{}
	}
	return b.calls[len(b.calls)-1]
}

func (b *billRecorder) waitBill(t *testing.T) {
	t.Helper()
	select {
	case <-b.done:
	case <-time.After(10 * time.Second):
		t.Fatal("输家计费未被调用")
	}
}

// TestHedgeNoRaceWhenFirstByteWithinThreshold 阈值未到不竞速：首个供应商在阈值内
// 产出内容，备选供应商绝不被启动。
func TestHedgeNoRaceWhenFirstByteWithinThreshold(t *testing.T) {
	fast := streamServer(t, "text/event-stream", claudeStreamChunks(), true)
	never := streamServer(t, "text/event-stream", claudeStreamChunks(), true)

	harness := newHedgeHarness(t)
	// 阈值 60s：本测试根本不触发竞速，但阈值计时器装配后不 fire。
	initial := sseCandidate(1, "供应商甲", fast.URL, 60_000)
	harness.setSelect([]*Candidate{sseCandidate(2, "供应商乙", never.URL, 0)})

	result, err := ForwardStreamHedge(context.Background(), harness.pc,
		initial, harness.deps, harness.options, harness.cfg)
	if err != nil {
		t.Fatalf("ForwardStreamHedge 失败: %v", err)
	}
	if result.Provider.ID != 1 {
		t.Fatalf("胜者供应商 = %d，期望 1", result.Provider.ID)
	}
	if len(result.Attempts) != 1 {
		t.Fatalf("尝试留痕 = %d 条，期望 1（未竞速）", len(result.Attempts))
	}
	if result.Attempts[0].Reason != "request_success" {
		t.Fatalf("胜者理由 = %q，期望 request_success（无真实竞速）", result.Attempts[0].Reason)
	}
	if harness.selects != 0 {
		t.Fatalf("Select 被调用 %d 次，期望 0（阈值未到不启动备选）", harness.selects)
	}
	received, _ := consumeStream(t, result.Stream)
	if !strings.Contains(string(received), "message_stop") {
		t.Fatalf("胜者流内容异常: %q", received)
	}
}

// TestHedgeFirstValidContentWinsAndLoserCancelled 先到者胜：首供应商超过阈值不出
// 内容，备选供应商先产出有效内容——备选成为胜者，首供应商被取消。
func TestHedgeFirstValidContentWinsAndLoserCancelled(t *testing.T) {
	// 首供应商：收到请求后挂起（不吐任何字节），直到测试放行。
	blocked := make(chan struct{})
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		<-blocked
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(func() {
		close(blocked)
		first.Close()
	})
	second := streamServer(t, "text/event-stream", claudeStreamChunks(), true)

	harness := newHedgeHarness(t)
	initial := sseCandidate(1, "供应商甲", first.URL, 1)
	harness.setSelect([]*Candidate{sseCandidate(2, "供应商乙", second.URL, 0)})

	type outcome struct {
		result *StreamResult
		err    error
	}
	outCh := make(chan outcome, 1)
	go func() {
		result, err := ForwardStreamHedge(context.Background(), harness.pc,
			initial, harness.deps, harness.options, harness.cfg)
		outCh <- outcome{result, err}
	}()

	// 阈值 1ms 已装配：手动触发竞速（不等待真实时间）。
	timer := <-harness.timerCh
	timer.fire()
	// 竞速由阈值触发：备选 Select 随后被调用。
	<-harness.selectCalls

	out := <-outCh
	if out.err != nil {
		t.Fatalf("ForwardStreamHedge 失败: %v", out.err)
	}
	if out.result.Provider.ID != 2 {
		t.Fatalf("胜者供应商 = %d，期望 2（备选先出内容）", out.result.Provider.ID)
	}
	foundWinner := false
	for _, attempt := range out.result.Attempts {
		if attempt.Reason == "hedge_winner" && attempt.ProviderID == 2 {
			foundWinner = true
		}
	}
	if !foundWinner {
		t.Fatalf("缺少 hedge_winner 留痕: %+v", out.result.Attempts)
	}
	received, _ := consumeStream(t, out.result.Stream)
	if !strings.Contains(string(received), "message_stop") {
		t.Fatalf("胜者流内容异常: %q", received)
	}
}

// TestHedgeLoserBilledExactlyOnce 败者 drain 计费被累加且不重复：输家已产生内容并
// 开启计费时，后台 drain 正文拿回用量并恰好计费一次。
func TestHedgeLoserBilledExactlyOnce(t *testing.T) {
	// 首供应商：挂起直到赢家提交后才吐内容（成为带内容的输家）。
	blocked := make(chan struct{})
	var closeOnce sync.Once
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		<-blocked
		for _, chunk := range claudeStreamChunks() {
			_, _ = w.Write([]byte(chunk))
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(func() {
		closeOnce.Do(func() { close(blocked) })
		first.Close()
	})
	second := streamServer(t, "text/event-stream", claudeStreamChunks(), true)

	recorder := newBillRecorder()
	harness := newHedgeHarness(t)
	initial := sseCandidate(1, "供应商甲", first.URL, 1)
	harness.setSelect([]*Candidate{sseCandidate(2, "供应商乙", second.URL, 0)})
	harness.withRequestID(8801).withBillLosers(recorder)

	type outcome struct {
		result *StreamResult
		err    error
	}
	outCh := make(chan outcome, 1)
	go func() {
		result, err := ForwardStreamHedge(context.Background(), harness.pc,
			initial, harness.deps, harness.options, harness.cfg)
		outCh <- outcome{result, err}
	}()

	// 触发竞速：备选供应商先提交，成为赢家。
	timer := <-harness.timerCh
	timer.fire()
	// 等待备选被启动（阈值触发调用了 Select）。
	<-harness.selectCalls

	out := <-outCh
	if out.err != nil {
		t.Fatalf("ForwardStreamHedge 失败: %v", out.err)
	}
	if out.result.Provider.ID != 2 {
		t.Fatalf("胜者供应商 = %d，期望 2", out.result.Provider.ID)
	}
	// 消费赢家流（避免留下读取协程）。
	_, _ = consumeStream(t, out.result.Stream)

	// 放行首供应商内容：它的门控随后提交，成为带内容的输家并走计费。
	closeOnce.Do(func() { close(blocked) })

	recorder.waitBill(t)
	if recorder.billCount() != 1 {
		t.Fatalf("输家计费次数 = %d，期望恰好 1 次", recorder.billCount())
	}
	bill := recorder.last()
	if bill.RequestID != 8801 {
		t.Fatalf("计费请求行 id = %d，期望 8801", bill.RequestID)
	}
	if bill.ProviderID != 1 {
		t.Fatalf("计费供应商 = %d，期望 1（输家）", bill.ProviderID)
	}
	if bill.Sequence != 1 {
		t.Fatalf("计费序号 = %d，期望 1", bill.Sequence)
	}
	if !bill.UsageSeen() {
		t.Fatalf("计费用量缺失: %+v", bill.Usage)
	}
	if !bill.DrainComplete {
		t.Fatal("输家正文应被 drain 到自然结束")
	}
	// 稍候断言不重复计费。
	select {
	case <-recorder.done:
		t.Fatal("输家被重复计费")
	case <-time.After(50 * time.Millisecond):
	}
}

// TestHedgeSlotSaturated 并发上限饱和：达到 maxInFlight 后不再启动备选。
//
// 链上**不得**出现 `hedge_slot_saturated`：Node 把它记为 routing-trace 事件
// （`type: "hedge_slot_saturated"`），`provider_chain[].reason` 里没有这个词；
// 而公开状态分类器对「未知且非空」的 reason 一律判**失败**（
// `ClassifyRequestOutcomeSignal` 的兜底分支），即一条信息性记录会把可用率拉低。
// Go 侧改为 debug 日志（`forward.hedge.slot_saturated`）保留可观测性。
func TestHedgeSlotSaturated(t *testing.T) {
	blocked := make(chan struct{})
	var closeOnce sync.Once
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		<-blocked
		for _, chunk := range claudeStreamChunks() {
			_, _ = w.Write([]byte(chunk))
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(func() {
		closeOnce.Do(func() { close(blocked) })
		first.Close()
	})
	second := streamServer(t, "text/event-stream", claudeStreamChunks(), true)

	// maxInFlight=1：首供应商尚未提交时阈值到期也不得启动备选。
	harness := newHedgeHarness(t)
	harness.cfg.MaxInFlight = 1
	initial := sseCandidate(1, "供应商甲", first.URL, 1)
	harness.setSelect([]*Candidate{sseCandidate(2, "供应商乙", second.URL, 0)})

	type outcome struct {
		result *StreamResult
		err    error
	}
	outCh := make(chan outcome, 1)
	go func() {
		result, err := ForwardStreamHedge(context.Background(), harness.pc,
			initial, harness.deps, harness.options, harness.cfg)
		outCh <- outcome{result, err}
	}()

	// 阈值到期：并发已满，不启动备选、不调用 Select。
	timer := <-harness.timerCh
	timer.fire()

	// 放行首供应商，让它正常胜出。
	closeOnce.Do(func() { close(blocked) })
	out := <-outCh
	if out.err != nil {
		t.Fatalf("ForwardStreamHedge 失败: %v", out.err)
	}
	if out.result.Provider.ID != 1 {
		t.Fatalf("胜者供应商 = %d，期望 1（并发已满，备选未启动）", out.result.Provider.ID)
	}
	for _, attempt := range out.result.Attempts {
		if attempt.Reason == "hedge_slot_saturated" {
			t.Fatalf("链上不得出现 hedge_slot_saturated（Node 该信息记在 routing-trace，不在 provider_chain）: %+v", out.result.Attempts)
		}
	}
	// 胜者必须仍是首供应商，且链上只应有它的结局原因。
	if len(out.result.Attempts) != 1 || out.result.Attempts[0].Reason != ReasonRequestSuccess {
		t.Fatalf("饱和后应只留胜者一条留痕，实际 %+v", out.result.Attempts)
	}
	_, _ = consumeStream(t, out.result.Stream)
}

// slowStreamServer 是首帧立即到达、其余帧按 gap 间隔吐出的假上游。
//
// 这个间隔是必要的：上游把全部帧一次性灌进客户端缓冲时，胜者交接就算提前掐断连接也会
// 「碰巧」读到完整正文——缺陷因此只在真实往返延迟下显形（对齐线上「并发负载下偶发」的现象）。
func slowStreamServer(t *testing.T, chunks []string, gap time.Duration) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for index, chunk := range chunks {
			if index > 0 {
				time.Sleep(gap)
			}
			_, _ = w.Write([]byte(chunk))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// assertWinnerStreamComplete 读光胜者流并断言正文完整、终态为 completed。
func assertWinnerStreamComplete(t *testing.T, result *StreamResult, providerID int64, want string) {
	t.Helper()
	if result.Provider.ID != providerID {
		t.Fatalf("胜者供应商 = %d，期望 %d", result.Provider.ID, providerID)
	}
	received, outcome := consumeStream(t, result.Stream)
	if string(received) != want {
		t.Fatalf("胜者流被截断：读到 %d 字节，期望 %d（终态 %s err=%v）",
			len(received), len(want), outcome.Kind, outcome.Err)
	}
	if outcome.Kind != TerminalCompleted {
		t.Fatalf("胜者终态 = %q，期望 %q（err=%v）", outcome.Kind, TerminalCompleted, outcome.Err)
	}
}

// TestHedgeWinnerStreamNotTruncatedBySlowUpstream 钉住「胜者 ctx 不早于最后一次上游读被取消」：
// 上游首帧之后隔 150ms 才吐后续帧时，胜者的正文必须完整、终态必须是 completed。
//
// 若胜者的 attemptCtx 随 attempt 返回被取消（上游连接在同一 ctx 上），这里会稳定红：
// 正文读到一半就 context canceled，终态 local_error。这也解释了两条既有用例在并发负载下
// 偶发红的现象——负载让最后一帧赶不上那一次提前取消，而非测试自身时序错。
func TestHedgeWinnerStreamNotTruncatedBySlowUpstream(t *testing.T) {
	chunks := claudeStreamChunks()
	want := strings.Join(chunks, "")

	t.Run("首个候选胜出", func(t *testing.T) {
		slow := slowStreamServer(t, chunks, 150*time.Millisecond)

		harness := newHedgeHarness(t)
		// 阈值 60s：本用例不触发竞速，胜者就是首个候选。
		initial := sseCandidate(1, "供应商甲", slow.URL, 60_000)
		harness.setSelect([]*Candidate{sseCandidate(2, "供应商乙", slow.URL, 0)})

		result, err := ForwardStreamHedge(context.Background(), harness.pc,
			initial, harness.deps, harness.options, harness.cfg)
		if err != nil {
			t.Fatalf("ForwardStreamHedge 失败: %v", err)
		}
		assertWinnerStreamComplete(t, result, 1, want)
	})

	t.Run("备选胜出", func(t *testing.T) {
		// 首供应商挂起不出内容，阈值到期后启动的备选成为胜者。
		blocked := make(chan struct{})
		var closeOnce sync.Once
		first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("content-type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			<-blocked
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}))
		t.Cleanup(func() {
			closeOnce.Do(func() { close(blocked) })
			first.Close()
		})
		slow := slowStreamServer(t, chunks, 150*time.Millisecond)

		harness := newHedgeHarness(t)
		initial := sseCandidate(1, "供应商甲", first.URL, 1)
		harness.setSelect([]*Candidate{sseCandidate(2, "供应商乙", slow.URL, 0)})

		type outcome struct {
			result *StreamResult
			err    error
		}
		outCh := make(chan outcome, 1)
		go func() {
			result, err := ForwardStreamHedge(context.Background(), harness.pc,
				initial, harness.deps, harness.options, harness.cfg)
			outCh <- outcome{result, err}
		}()

		timer := <-harness.timerCh
		timer.fire()
		<-harness.selectCalls

		out := <-outCh
		if out.err != nil {
			t.Fatalf("ForwardStreamHedge 失败: %v", out.err)
		}
		assertWinnerStreamComplete(t, out.result, 2, want)
	})
}

// TestHedgeWinnerContextReleasedWithStream 钉住取消权转移不等于泄漏：胜者的 attemptCtx
// 在流结束（泵关闭上游正文）时必须被取消，且不得早于最后一次上游读。
func TestHedgeWinnerContextReleasedWithStream(t *testing.T) {
	slow := slowStreamServer(t, claudeStreamChunks(), 50*time.Millisecond)

	detached := make(chan context.Context, 1)
	harness := newHedgeHarness(t)
	harness.cfg.WinnerDetached = func(_ int64, attemptCtx context.Context) {
		detached <- attemptCtx
	}
	initial := sseCandidate(1, "供应商甲", slow.URL, 60_000)
	harness.setSelect([]*Candidate{sseCandidate(2, "供应商乙", slow.URL, 0)})

	result, err := ForwardStreamHedge(context.Background(), harness.pc,
		initial, harness.deps, harness.options, harness.cfg)
	if err != nil {
		t.Fatalf("ForwardStreamHedge 失败: %v", err)
	}

	var attemptCtx context.Context
	select {
	case attemptCtx = <-detached:
	case <-time.After(5 * time.Second):
		t.Fatal("胜者交接未上报 attempt ctx")
	}

	// 流尚未结束：此刻取消会截断正文（见上一个用例），故 ctx 必须仍然活着。
	firstRead := make([]byte, 256)
	n, readErr := result.Stream.Read(firstRead)
	if n == 0 || readErr != nil {
		t.Fatalf("读首帧失败: n=%d err=%v", n, readErr)
	}
	if attemptCtx.Err() != nil {
		t.Fatalf("流尚未结束但胜者 attempt ctx 已被取消: %v", attemptCtx.Err())
	}

	// 读光剩余正文：泵在终态关闭上游，取消权随之释放。
	_, _ = consumeStream(t, result.Stream)
	select {
	case <-attemptCtx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("流已结束但胜者 attempt ctx 未被取消：取消权转移后泄漏")
	}
}

// TestHedgeFailedAttemptNotMarkedAsLoser 钉住：**失败的 attempt 不得被标成竞速输家**。
//
// 真实事故（生产 `message_request` 行 946876 / 946908 / 946915，同一形态）：
// `HC_Chat` 上游 500/503 失败，链里却同时出现两条留痕——
//
//	{"name":"HC_Chat","reason":"retry_failed"}        ← 真实的失败结局
//	{"name":"HC_Chat","reason":"hedge_loser_billed"}  ← 凭空多出的「竞速输家」
//
// 根因：失败路径落了留痕却没置 `outcomeRecorded`，胜者裁决时 `markLoserOutcomeLocked`
// 仍把它当成**在途**输家再落一条。Node 侧 `abortAttempt` 首行即 `if (attempt.settled) return;`，
// 且已结束的 attempt 会被移出 `attempts` 集合，故失败者不会被标成输家。
//
// 反证：把失败路径里的 `attempt.outcomeRecorded = true` 去掉，本用例转红。
func TestHedgeFailedAttemptNotMarkedAsLoser(t *testing.T) {
	// 首供应商：直接回 500（**已结束的失败**，不是挂起的在途输家）。
	failed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream boom"}}`))
	}))
	t.Cleanup(failed.Close)
	second := streamServer(t, "text/event-stream", claudeStreamChunks(), true)

	harness := newHedgeHarness(t)
	initial := sseCandidate(1, "供应商甲", failed.URL, 1)
	harness.setSelect([]*Candidate{sseCandidate(2, "供应商乙", second.URL, 0)})

	type outcome struct {
		result *StreamResult
		err    error
	}
	outCh := make(chan outcome, 1)
	go func() {
		result, err := ForwardStreamHedge(context.Background(), harness.pc,
			initial, harness.deps, harness.options, harness.cfg)
		outCh <- outcome{result, err}
	}()

	// 首供应商失败后竞速会拉起备选；期间排期的阈值定时器一律放行（不依赖真实时间）。
	var out outcome
	deadline := time.After(20 * time.Second)
	// 阈值定时器一律扣到失败路径自己拉起备选之后才放行：`harness.selectCalls` 在
	// launchAlternative → Select 时触发，而失败路径是**先落失败留痕、后调 launchAlternative**
	// （见 finishAttemptFailed），故收到它即意味着首供应商的失败已结束。
	//
	// 若一上来就放行定时器，备选会与仍在途的首供应商并发竞速，赢家裁决可能先于失败观测
	// 拿到锁——于是同一份事实按调度顺序得出两种结论（CI 上 2 核 runner 红的成因：实测
	// 2 核 + 负载下 25 次里 18 次红）。本用例的名字就在断言「已结束的失败不得被标成输家」，
	// 故必须把这个前提钉死，而不是靠机器快慢。
	alternativeLaunched := false
collect:
	for {
		select {
		case <-harness.selectCalls:
			alternativeLaunched = true
		case timer := <-harness.timerCh:
			if alternativeLaunched {
				timer.fire()
			}
		case out = <-outCh:
			break collect
		case <-deadline:
			t.Fatal("ForwardStreamHedge 超时（20s）")
		}
	}

	if out.err != nil {
		t.Fatalf("ForwardStreamHedge 失败: %v", out.err)
	}
	if out.result.Provider.ID != 2 {
		t.Fatalf("胜者供应商 = %d，期望 2（首供应商已失败）", out.result.Provider.ID)
	}

	var reasons []string
	for _, attempt := range out.result.Attempts {
		if attempt.ProviderID == 1 {
			reasons = append(reasons, attempt.Reason)
		}
	}
	if len(reasons) != 1 {
		t.Fatalf("首供应商（失败者）应有且仅有一条留痕，实际 %d 条：%v", len(reasons), reasons)
	}
	if strings.HasPrefix(reasons[0], "hedge_loser_") {
		t.Fatalf("失败的 attempt 被标成了竞速输家：%q（它的结局是失败，不是输掉竞速）", reasons[0])
	}
	if reasons[0] != ReasonRetryFailed {
		t.Fatalf("失败留痕的 reason = %q，期望 %q", reasons[0], ReasonRetryFailed)
	}

	// 消费胜者流，避免留下读取协程。
	_, _ = consumeStream(t, out.result.Stream)
}
