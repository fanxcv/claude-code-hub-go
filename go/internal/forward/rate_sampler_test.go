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

// samplerChatChunk 造一个含 size 个字符内容的 openai-chat 内容帧。
func samplerChatChunk(size int) string {
	return `data: {"choices":[{"delta":{"content":"` + strings.Repeat("a", size) + `"}}]}` + "\n\n"
}

// TestRateSamplerEmitsEachMarkOnceWithVerdict 钉住标定契约：
// 三个标定点各落一次，且带出实测速率与「本会怎么判」。
func TestRateSamplerEmitsEachMarkOnceWithVerdict(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var samples []RateSample
	sampler := newRateSampler(RateSamplerConfig{
		Family:     gate.FamilyOpenAIChat,
		ProviderID: 167,
		Rate:       1000,
		marks:      testMarks(),
		Now:        func() time.Time { return now },
		OnSample:   func(sample RateSample) { samples = append(samples, sample) },
	})

	// t=0：100 字节 ⇒ 启动时钟。首档 20ms 需 > 1000×2×0.02 = 40 字节。
	sampler.Observe([]byte(samplerChatChunk(100)))
	if len(samples) != 0 {
		t.Fatalf("未到检查点不应落样本，得 %d 条", len(samples))
	}

	// t=25ms：首档到期 ⇒ 100 > 40 ⇒ 本会 commit。
	now = now.Add(25 * time.Millisecond)
	sampler.Observe([]byte(samplerChatChunk(0)))

	// t=65ms：次档到期，需 60 字节 ⇒ 100 > 60 ⇒ 本会 commit。
	now = now.Add(40 * time.Millisecond)
	sampler.Observe([]byte(samplerChatChunk(0)))

	// t=210ms：末档到期，需 200 字节 ⇒ 100 不过 ⇒ 本会 slow。
	now = now.Add(145 * time.Millisecond)
	sampler.Observe([]byte(samplerChatChunk(0)))

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
		if sample.Threshold != 1000 || sample.ProviderID != 167 {
			t.Fatalf("第 %d 条应带出阈值与渠道，得 %+v", index, sample)
		}
	}
	// 速率必须与「字节/实际流逝」一致：25ms 时 100 字节 ⇒ 4000 B/s。
	if samples[0].BytesPerSecond != 4000 {
		t.Fatalf("首档速率 = %d，期望 4000", samples[0].BytesPerSecond)
	}
}

// TestRateSamplerCountsOnlySemanticPayload 钉住计量口径与门控一致：
// 心跳/头帧不启动时钟，也不计入字节。
func TestRateSamplerCountsOnlySemanticPayload(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var samples []RateSample
	sampler := newRateSampler(RateSamplerConfig{
		Family:   gate.FamilyOpenAIChat,
		Rate:     1000,
		marks:    testMarks(),
		Now:      func() time.Time { return now },
		OnSample: func(sample RateSample) { samples = append(samples, sample) },
	})

	sampler.Observe([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
	now = now.Add(300 * time.Millisecond)
	// 只有头帧：始终未启动时钟 ⇒ 即便早已越过末档也不落任何样本。
	sampler.Observe([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"))
	if len(samples) != 0 {
		t.Fatalf("只有中性帧时不得落样本，得 %+v", samples)
	}
}

// TestRateSamplerSecondLevelGateFiresOnce 钉住二级闸：
// 越过裁决期限后按滑动窗对照 θ，掉速回调**只触发一次**。
func TestRateSamplerSecondLevelGateFiresOnce(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var samples []RateSample
	var degraded []RateSample
	sampler := newRateSampler(RateSamplerConfig{
		Family:   gate.FamilyOpenAIChat,
		Rate:     1000,
		Window:   100 * time.Millisecond,
		marks:    testMarks(),
		Now:      func() time.Time { return now },
		OnSample: func(sample RateSample) { samples = append(samples, sample) },
		OnDegraded: func(sample RateSample) {
			degraded = append(degraded, sample)
		},
	})

	sampler.Observe([]byte(samplerChatChunk(10)))
	// 越过末档（200ms）后方开始计滑动窗。
	now = now.Add(250 * time.Millisecond)
	sampler.Observe([]byte(samplerChatChunk(1)))
	if len(degraded) != 0 {
		t.Fatalf("滑动窗首个基准点不应判掉速，得 %+v", degraded)
	}
	// 窗满（100ms）而窗内几乎无产出 ⇒ 掉速。
	now = now.Add(120 * time.Millisecond)
	sampler.Observe([]byte(samplerChatChunk(1)))
	if len(degraded) != 1 {
		t.Fatalf("应判掉速一次，得 %d 次", len(degraded))
	}
	if !degraded[0].Degraded || degraded[0].Stage != -1 || degraded[0].Verdict != "slow" {
		t.Fatalf("掉速样本字段不对：%+v", degraded[0])
	}
	// 再等一个窗：仍慢，但不得重复报。
	now = now.Add(120 * time.Millisecond)
	sampler.Observe([]byte(samplerChatChunk(1)))
	if len(degraded) != 1 {
		t.Fatalf("掉速只应报一次，得 %d 次", len(degraded))
	}
}

// TestRateSamplerShadowNeverFiresSecondLevelGate 钉住影子期语义：
// 只落标定样本，绝不触发二级闸（影子期不得有任何处置动作）。
func TestRateSamplerShadowNeverFiresSecondLevelGate(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	var samples []RateSample
	degradedCalls := 0
	sampler := newRateSampler(RateSamplerConfig{
		Family:   gate.FamilyOpenAIChat,
		Rate:     1000,
		Window:   100 * time.Millisecond,
		Shadow:   true,
		marks:    testMarks(),
		Now:      func() time.Time { return now },
		OnSample: func(sample RateSample) { samples = append(samples, sample) },
		OnDegraded: func(RateSample) {
			degradedCalls++
		},
	})

	sampler.Observe([]byte(samplerChatChunk(1)))
	for step := 0; step < 6; step++ {
		now = now.Add(100 * time.Millisecond)
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
// 速率闸判慢走「供应商错误 + 524」，与停滞判慢同档（客户端可重试的 5xx）。
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
	if failure.Category != CategoryProviderError {
		t.Fatalf("分类 = %s，期望 %s", failure.Category, CategoryProviderError)
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
