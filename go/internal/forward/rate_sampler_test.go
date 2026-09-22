package forward

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/gate"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
)

// 本文件钉住提交后速率采样器（影子标定 + 二级闸）与影子期「不改变提交时机」的契约。

// testMarks 把标定点缩到毫秒级：否则每个用例都要真等 10 秒。
func testMarks() []rateMark {
	return []rateMark{
		{0, 20 * time.Millisecond, gate.PrecommitFastMultiplier},
		{1, 60 * time.Millisecond, 1},
		{2, 200 * time.Millisecond, 1},
	}
}

// immediateMarks 把标定点压在零点：端到端用例的流只有毫秒级寿命，
// 故要验证「日志有产出」必须让标定点在首个内容帧后立刻到期。
func immediateMarks() []rateMark {
	return []rateMark{
		{0, 0, gate.PrecommitFastMultiplier},
		{1, 0, 1},
		{2, 0, 1},
	}
}

// fakeClock 是与注入时钟**同步推进**的假定时器工厂：advance 既推进 now，也按到期顺序触发回调。
//
// 为何必须有它：标定点现在由定时器驱动（见 rate_sampler.go 的 F1 说明）。用真 AfterFunc 就得
// 真等 1s/3s/10s，而用注入的假时钟又与真定时器错配（回调在真实时间到点，快照却按假时钟取）——
// 两者都会让「样本记的是期限当刻的值」这条断言测不出来。
//
// 只供单测：回调在 advance 的调用方 goroutine 上同步跑，故数据竞争面与生产无关。
type fakeClock struct {
	now     time.Time
	pending []*fakeTimer
}

type fakeTimer struct {
	due    time.Time
	fire   func()
	active bool
}

func (t *fakeTimer) Stop() bool {
	if !t.active {
		return false
	}
	t.active = false
	return true
}

func newFakeClock(now time.Time) *fakeClock { return &fakeClock{now: now} }

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) AfterFunc(delay time.Duration, fire func()) rateTimer {
	timer := &fakeTimer{due: c.now.Add(delay), fire: fire, active: true}
	c.pending = append(c.pending, timer)
	return timer
}

// advance 把时钟推到 now+d 并按到期时间顺序触发回调；回调里新安排的定时器同样参与。
func (c *fakeClock) advance(d time.Duration) {
	target := c.now.Add(d)
	for {
		next := c.nextDue(target)
		if next == nil {
			break
		}
		// 回调执行期间时钟停在它的到期时刻：样本带出的实际发射时刻于是等于期限。
		c.now = next.due
		next.active = false
		next.fire()
	}
	c.now = target
}

func (c *fakeClock) nextDue(limit time.Time) *fakeTimer {
	var best *fakeTimer
	for _, timer := range c.pending {
		if !timer.active || timer.due.After(limit) {
			continue
		}
		if best == nil || timer.due.Before(best.due) {
			best = timer
		}
	}
	return best
}

// samplerChatChunk 造一个含 size 个字符内容的 openai-chat 内容帧。
func samplerChatChunk(size int) string {
	return `data: {"choices":[{"delta":{"content":"` + strings.Repeat("a", size) + `"}}]}` + "\n\n"
}

// TestRateSamplerEmitsEachMarkOnceWithVerdict 钉住标定契约：
// 三个标定点各落一次，且带出实测速率与「本会怎么判」。
func TestRateSamplerEmitsEachMarkOnceWithVerdict(t *testing.T) {
	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	var samples []RateSample
	sampler := newRateSampler(RateSamplerConfig{
		Family:     gate.FamilyOpenAIChat,
		ProviderID: 167,
		Rate:       1000,
		marks:      testMarks(),
		Now:        clock.Now,
		AfterFunc:  clock.AfterFunc,
		OnSample:   func(sample RateSample) { samples = append(samples, sample) },
	})

	// t=0：100 字节 ⇒ 启动时钟。首档 20ms 需 > 1000×2×0.02 = 40 字节。
	sampler.Observe([]byte(samplerChatChunk(100)))
	if len(samples) != 0 {
		t.Fatalf("未到检查点不应落样本，得 %d 条", len(samples))
	}

	// t=25ms：首档到期 ⇒ 100 > 40 ⇒ 本会 commit。
	clock.advance(25 * time.Millisecond)
	// t=65ms：次档到期，需 60 字节 ⇒ 100 > 60 ⇒ 本会 commit。
	clock.advance(40 * time.Millisecond)
	// t=210ms：末档到期，需 200 字节 ⇒ 100 不过 ⇒ 本会 slow。
	clock.advance(145 * time.Millisecond)

	if len(samples) != 3 {
		t.Fatalf("应有三个标定点各一条，得 %d 条：%+v", len(samples), samples)
	}
	wantStages := []int{0, 1, 2}
	wantVerdicts := []string{"commit", "commit", "slow"}
	for index, sample := range samples {
		if sample.Stage != wantStages[index] {
			t.Fatalf("第 %d 条的档位 = %d，期望 %d", index, sample.Stage, wantStages[index])
		}
		if sample.Verdict != wantVerdicts[index] {
			t.Fatalf("第 %d 条的结论 = %q，期望 %q", index, sample.Verdict, wantVerdicts[index])
		}
		if sample.PayloadBytes != 100 {
			t.Fatalf("第 %d 条应带出累计 100 字节，得 %d", index, sample.PayloadBytes)
		}
		// ElapsedMS 是**名义期限**（判据 θ×倍数×t 同轴），与发射时刻无关。
		if wantMS := testMarks()[index].elapsed.Milliseconds(); int64(sample.ElapsedMS) != wantMS {
			t.Fatalf("第 %d 条的期限 = %d，期望 %d", index, sample.ElapsedMS, wantMS)
		}
		if sample.EmitElapsedMS != sample.ElapsedMS {
			t.Fatalf("第 %d 条按时发射时实际与名义应相等，得 %d / %d",
				index, sample.EmitElapsedMS, sample.ElapsedMS)
		}
		if sample.Threshold != 1000 || sample.ProviderID != 167 {
			t.Fatalf("第 %d 条应带出阈值与渠道，得 %+v", index, sample)
		}
	}
	// 速率按**名义期限**折算：20ms 档 100 字节 ⇒ 5000 B/s。
	if samples[0].BytesPerSecond != 5000 {
		t.Fatalf("首档速率 = %d，期望 5000", samples[0].BytesPerSecond)
	}
}

// TestRateSamplerSnapshotsAtCheckpointNotOnNextFrame 是 F1 的**缺陷钉子**：
// 样本必须记「期限当刻」的累计，而不是「下一帧到达时」的累计。
//
// 旧实现下本用例必红：回填用的是下一帧到达那刻的 payload 与 elapsed，
// 于是 1s 档被记成 501B/4000ms。快速流下误差仅毫秒，但慢流下误差最大——
// 而慢流正是三档阈值要标定的那一端，故这批数据的慢尾会整体失真。
func TestRateSamplerSnapshotsAtCheckpointNotOnNextFrame(t *testing.T) {
	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	var samples []RateSample
	sampler := newRateSampler(RateSamplerConfig{
		Family:    gate.FamilyOpenAIChat,
		Rate:      1000,
		marks:     rateSamplerMarks[:],
		Now:       clock.Now,
		AfterFunc: clock.AfterFunc,
		OnSample:  func(sample RateSample) { samples = append(samples, sample) },
	})

	// 首帧 1 字节 @ t=0：启动时钟。
	sampler.Observe([]byte(samplerChatChunk(1)))
	// 直到 t=4s 都没有任何字节到达（慢流的典型形态）：已过去的 1s/3s 两档必须已按时发射。
	clock.advance(4 * time.Second)
	if len(samples) != 2 {
		t.Fatalf("1s/3s 两档应按时发射，得 %d 条：%+v", len(samples), samples)
	}
	// 再走到 11s，末档也过去。
	clock.advance(7 * time.Second)
	if len(samples) != 3 {
		t.Fatalf("三档都应按时发射，得 %d 条：%+v", len(samples), samples)
	}
	wantMS := []int{1000, 3000, 10000}
	for index, sample := range samples {
		if sample.PayloadBytes != 1 {
			t.Fatalf("第 %d 档必须只记期限当刻的 1 字节，得 %d（回填缺陷）",
				index, sample.PayloadBytes)
		}
		if sample.ElapsedMS != wantMS[index] {
			t.Fatalf("第 %d 档期限 = %d，期望 %d", index, sample.ElapsedMS, wantMS[index])
		}
	}

	// 11s 才到的 500 字节**不得**回填进已到的档位（旧实现在这里会重算）。
	sampler.Observe([]byte(samplerChatChunk(500)))
	if len(samples) != 3 {
		t.Fatalf("已发射的档位不得因后到的字节重发，得 %d 条", len(samples))
	}
	for index, sample := range samples {
		if sample.PayloadBytes != 1 {
			t.Fatalf("第 %d 档被后到字节污染：%d 字节", index, sample.PayloadBytes)
		}
	}
	// 终值（F7）：结束时记整条流的累计语义字节数，供与 output_tokens 相除标定 bytesPerToken。
	sampler.Close("test_end")
	if len(samples) != 4 {
		t.Fatalf("Close 应额外落一条流结束终值，得 %d 条：%+v", len(samples), samples)
	}
	end := samples[3]
	if !end.Ended || end.Stage != StageStreamEnd {
		t.Fatalf("终值样本应带 Ended 与 StageStreamEnd，得 %+v", end)
	}
	if end.PayloadBytes != 501 {
		t.Fatalf("终值应记整条流的 501 字节，得 %d", end.PayloadBytes)
	}
	if end.Verdict != "test_end" {
		t.Fatalf("终值应带结束原因，得 %q", end.Verdict)
	}
}

// TestRateSamplerCloseCancelsPendingMarks 钉住短流的收尾：流未到末档就结束，
// 未到期的档位不得再发射（否则样本属于一个已死的流），且定时器不得滞留。
func TestRateSamplerCloseCancelsPendingMarks(t *testing.T) {
	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	var samples []RateSample
	sampler := newRateSampler(RateSamplerConfig{
		Family:    gate.FamilyOpenAIChat,
		Rate:      1000,
		marks:     rateSamplerMarks[:],
		Now:       clock.Now,
		AfterFunc: clock.AfterFunc,
		OnSample:  func(sample RateSample) { samples = append(samples, sample) },
	})

	sampler.Observe([]byte(samplerChatChunk(10)))
	// 流在 2s 结束：1s 档发射，2s 时又落了终值，共 2 条。
	clock.advance(2 * time.Second)
	sampler.Close("test_end")
	if len(samples) != 2 || samples[0].Stage != 0 {
		t.Fatalf("2s 结束应落 1s 档与终值各一条，得 %+v", samples)
	}
	// 时钟继续走到 10s：被撤销的档位不得复活，重复 Close 也不得再落终值（幂等）。
	clock.advance(8 * time.Second)
	sampler.Close("test_end")
	if len(samples) != 2 {
		t.Fatalf("Close 后未到期档位不得再发射且 Close 幂等，得 %d 条", len(samples))
	}
	// 无滞留：所有在途定时器都已停止。
	for _, timer := range clock.pending {
		if timer.active {
			t.Fatal("Close 必须撤销全部未到期定时器（否则拖着已死的流到末档）")
		}
	}
}

// TestRateSamplerCountsOnlySemanticPayload 钉住计量口径与门控一致：
// 心跳/头帧不启动时钟，也不计入字节。
func TestRateSamplerCountsOnlySemanticPayload(t *testing.T) {
	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	var samples []RateSample
	sampler := newRateSampler(RateSamplerConfig{
		Family:    gate.FamilyOpenAIChat,
		Rate:      1000,
		marks:     testMarks(),
		Now:       clock.Now,
		AfterFunc: clock.AfterFunc,
		OnSample:  func(sample RateSample) { samples = append(samples, sample) },
	})

	sampler.Observe([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
	clock.advance(300 * time.Millisecond)
	// 只有头帧：始终未启动时钟 ⇒ 即便早已越过末档也不落任何样本。
	sampler.Observe([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
	if len(samples) != 0 {
		t.Fatalf("只有中性帧时不得落样本，得 %+v", samples)
	}
}

// TestRateSamplerCloseSkipsEndSampleWithoutContent 钉住终值的条件：整条流没出现过语义内容帧时
// 不记终值（无字节可标定），保持零开销。
func TestRateSamplerCloseSkipsEndSampleWithoutContent(t *testing.T) {
	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	var samples []RateSample
	sampler := newRateSampler(RateSamplerConfig{
		Family:    gate.FamilyOpenAIChat,
		Rate:      1000,
		marks:     rateSamplerMarks[:],
		Now:       clock.Now,
		AfterFunc: clock.AfterFunc,
		OnSample:  func(sample RateSample) { samples = append(samples, sample) },
	})
	sampler.Observe([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
	clock.advance(time.Second)
	sampler.Close("test_end")
	if len(samples) != 0 {
		t.Fatalf("只有中性帧时不得落任何样本（含终值），得 %+v", samples)
	}
}

// TestRateSamplerSecondLevelGateFiresOnce 钉住二级闸：
// 越过裁决期限后按滑动窗对照 θ，掉速回调**只触发一次**。
func TestRateSamplerSecondLevelGateFiresOnce(t *testing.T) {
	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	var samples []RateSample
	var degraded []RateSample
	sampler := newRateSampler(RateSamplerConfig{
		Family:    gate.FamilyOpenAIChat,
		Rate:      1000,
		Window:    100 * time.Millisecond,
		marks:     testMarks(),
		Now:       clock.Now,
		AfterFunc: clock.AfterFunc,
		OnSample:  func(sample RateSample) { samples = append(samples, sample) },
		OnDegraded: func(sample RateSample) {
			degraded = append(degraded, sample)
		},
	})

	sampler.Observe([]byte(samplerChatChunk(10)))
	// 越过末档（200ms）后方开始计滑动窗。
	clock.advance(250 * time.Millisecond)
	sampler.Observe([]byte(samplerChatChunk(1)))
	if len(degraded) != 0 {
		t.Fatalf("滑动窗首个基准点不应判掉速，得 %+v", degraded)
	}
	// 窗满（100ms）而窗内几乎无产出 ⇒ 掉速。
	clock.advance(120 * time.Millisecond)
	sampler.Observe([]byte(samplerChatChunk(1)))
	if len(degraded) != 1 {
		t.Fatalf("应判掉速一次，得 %d 次", len(degraded))
	}
	if !degraded[0].Degraded || degraded[0].Stage != -1 || degraded[0].Verdict != "slow" {
		t.Fatalf("掉速样本字段不对：%+v", degraded[0])
	}
	// 再等一个窗：仍慢，但不得重复报。
	clock.advance(120 * time.Millisecond)
	sampler.Observe([]byte(samplerChatChunk(1)))
	if len(degraded) != 1 {
		t.Fatalf("掉速只应报一次，得 %d 次", len(degraded))
	}
}

// TestRateSamplerShadowNeverFiresSecondLevelGate 钉住影子期语义：
// 只落标定样本，绝不触发二级闸（影子期不得有任何处置动作）。
func TestRateSamplerShadowNeverFiresSecondLevelGate(t *testing.T) {
	clock := newFakeClock(time.Unix(1_700_000_000, 0))
	var samples []RateSample
	degradedCalls := 0
	sampler := newRateSampler(RateSamplerConfig{
		Family:    gate.FamilyOpenAIChat,
		Rate:      1000,
		Window:    100 * time.Millisecond,
		Shadow:    true,
		marks:     testMarks(),
		Now:       clock.Now,
		AfterFunc: clock.AfterFunc,
		OnSample:  func(sample RateSample) { samples = append(samples, sample) },
		OnDegraded: func(RateSample) {
			degradedCalls++
		},
	})

	sampler.Observe([]byte(samplerChatChunk(1)))
	for step := 0; step < 6; step++ {
		clock.advance(100 * time.Millisecond)
		sampler.Observe([]byte(samplerChatChunk(1)))
	}
	if degradedCalls != 0 {
		t.Fatalf("影子期不得触发二级闸，得 %d 次", degradedCalls)
	}
	if len(samples) != 3 {
		t.Fatalf("影子期仍必须落满三个标定点，得 %d 条", len(samples))
	}
}

// TestProbeSlowKindDistinguishesStallAndRate 钉住判慢来源可区分：
// 两种来源都要置 ProbeSlow（另一 lane 靠它做「判慢即标慢」的闭环），种类用于区分归因。
func TestProbeSlowKindDistinguishesStallAndRate(t *testing.T) {
	cases := []struct {
		name string
		err  error
		kind string
		slow bool
	}{
		{"停滞", &gate.PrecommitError{Reason: gate.FailSlowProbe}, ProbeSlowKindStall, true},
		{"速率", &gate.PrecommitError{Reason: gate.FailSlowRate}, ProbeSlowKindRate, true},
		{"空流不是判慢", &gate.PrecommitError{Reason: gate.FailEmptyStream}, "", false},
		{"非门控错误不是判慢", context.DeadlineExceeded, "", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			kind, slow := probeSlowKind(testCase.err)
			if slow != testCase.slow || kind != testCase.kind {
				t.Fatalf("probeSlowKind = (%q,%v)，期望 (%q,%v)", kind, slow, testCase.kind, testCase.slow)
			}
		})
	}
}

// TestGateFailureMapsSlowRateToRetryableProviderTimeout 钉住失败映射：
// 速率闸判慢走「慢速分类 + 524」——仍是客户端可重试的 5xx，但**不再**是 provider_error，
// 故其重试语义变为「不同家、立即换家」（见 F6）。
func TestGateFailureMapsSlowRateToRetryableProviderTimeout(t *testing.T) {
	deps := Deps{}
	plan := &Plan{URL: "https://example.com"}
	outcome := &AttemptOutcome{ProviderID: 167, ProviderName: "wb", Attempt: 1}
	precommit := &gate.PrecommitError{
		Reason:           gate.FailSlowRate,
		ProviderID:       167,
		SlowPayloadBytes: 100,
		SlowElapsedMS:    1000,
	}

	failure := deps.gateFailure(precommit, plan, outcome)
	if failure == nil {
		t.Fatal("应映射为尝试失败")
	}
	if failure.Category != CategorySlowRate {
		t.Fatalf("分类 = %s，期望 %s", failure.Category, CategorySlowRate)
	}
	if failure.Category.RetriesSameProvider() {
		t.Fatal("判慢不得在同一家重试（否则客户端要白等一个完整判慢周期）")
	}
	if !failure.Category.SwitchesProvider() {
		t.Fatal("判慢必须换家（否则候选耗尽即直接失败）")
	}
	if failure.Category.CountsTowardCircuit() {
		t.Fatal("判慢不得计入熔断：慢不是错，计了就没有渐进恢复的机会")
	}
	if failure.StatusCode != statusUpstreamTimeout {
		t.Fatalf("状态码 = %d，期望 %d（可重试的 5xx）", failure.StatusCode, statusUpstreamTimeout)
	}
	if got := slowRateBytesPerSecond(precommit); got != 100 {
		t.Fatalf("实测速率 = %d，期望 100", got)
	}
	if !strings.Contains(failure.Body, `"reason":"slow_rate"`) {
		t.Fatalf("失败体应带可区分的种类，得 %s", failure.Body)
	}
}

// TestPrecommitRateIsZeroInShadow 钉住影子期不改变提交时机的那一半契约：
// 影子期 θ 必须回落为 0，门控因此走「首个内容帧即提交」的既有路径
// （该路径本身由 gate 包 TestRateLadderDisabledKeepsLegacyCommitPoint 钉住）。
func TestPrecommitRateIsZeroInShadow(t *testing.T) {
	options := StreamOptions{
		PrecommitRateFor: func(int64) int { return 1000 },
		PrecommitShadow:  true,
	}
	if got := options.precommitRate(167); got != 0 {
		t.Fatalf("影子期 θ 必须为 0（否则会改变提交时机），得 %d", got)
	}
	if !options.rateSamplerEnabled(167) {
		t.Fatal("影子期仍必须开启采样器（标定数据来源）")
	}
	options.PrecommitShadow = false
	if got := options.precommitRate(167); got != 1000 {
		t.Fatalf("非影子期应透出 θ，得 %d", got)
	}
	if got := options.precommitRate(1); got != 1000 {
		t.Fatalf("阈值按渠道取值，得 %d", got)
	}
}

// TestForwardStreamShadowKeepsCommitPointAndLogs 端到端钉住影子的另一半契约：
// 开着影子速率配置跑真流，**提交标记与不开时逐字段一致**（提交点未移动），且日志有产出。
func TestForwardStreamShadowKeepsCommitPointAndLogs(t *testing.T) {
	chunks := claudeStreamChunks()

	runOnce := func(options StreamOptions) *gate.CommitMarker {
		t.Helper()
		server := streamServer(t, "text/event-stream", chunks, true)
		deps := Deps{Dial: newTestDial(t), Facts: newStreamFacts(), Limits: Limits{RetryDelay: time.Millisecond}}
		options.Format = convert.FormatClaude
		options.Settle = newCountingSettler()
		options.CaptureCommitMarker = true
		result, err := ForwardStream(context.Background(), newTestPctx(t), newTestCandidate(1, "供应商甲", server.URL, 1), deps, options)
		if err != nil {
			t.Fatalf("ForwardStream 失败: %v", err)
		}
		if result.Stream == nil || result.Stream.attempt.Marker == nil {
			t.Fatal("应提交为流并记录提交标记")
		}
		// 消费完，保证采样器确实跑过整条流。
		_, _ = consumeStream(t, result.Stream)
		return result.Stream.attempt.Marker
	}

	baseline := runOnce(StreamOptions{})

	var logs bytes.Buffer
	shadow := StreamOptions{
		PrecommitShadow:  true,
		PrecommitRateFor: func(int64) int { return 1 },
		Logger:           logx.New(&logs),
		// 标定点压在零点：整条流只有毫秒级寿命，生产用出厂 1s/3s/10s。
		rateMarks: immediateMarks(),
	}
	withShadow := runOnce(shadow)

	// 只比**确定性**字段：ChunkIndex 与 BufferedBytes 取决于 TCP 分块（同一上游两次运行
	// 本就会不同），拿来比会把网络抖动当成提交点移动。触发提交的**帧**才是判据。
	if withShadow.FrameIndex != baseline.FrameIndex || withShadow.EventName != baseline.EventName {
		t.Fatalf("影子期提交点不得移动：baseline=%+v shadow=%+v", *baseline, *withShadow)
	}
	if withShadow.LadderStage != -1 || baseline.LadderStage != -1 {
		t.Fatalf("影子期不得让门控走速率闸路径：baseline=%d shadow=%d", baseline.LadderStage, withShadow.LadderStage)
	}
	if !strings.Contains(logs.String(), "post-commit rate sample") {
		t.Fatalf("影子期必须落标定日志，实际日志：%s", logs.String())
	}
}
