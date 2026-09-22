package gate

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// 本文件钉住分级速率闸在门控层的语义：后移提交点、三档裁决、判慢时客户端零字节。
// 纯逻辑（状态机与计量口径）在 progress_test.go。

// scriptedStep 是脚本读取器的一步：先等 delay，再吐 text。
// text 为空串表示「产出 EOF」（结束流）；脚本耗尽则阻塞，模拟「上游还在但不再发字节」。
type scriptedStep struct {
	delay time.Duration
	text  string
}

type scriptedReader struct {
	steps   []scriptedStep
	index   int
	release chan struct{}
	once    sync.Once
}

func newScriptedReader(steps ...scriptedStep) *scriptedReader {
	return &scriptedReader{steps: steps, release: make(chan struct{})}
}

func (r *scriptedReader) Read(p []byte) (int, error) {
	if r.index >= len(r.steps) {
		<-r.release
		return 0, io.EOF
	}
	step := r.steps[r.index]
	r.index++
	if step.delay > 0 {
		timer := time.NewTimer(step.delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-r.release:
			return 0, io.EOF
		}
	}
	if step.text == "" {
		return 0, io.EOF
	}
	if len(step.text) > len(p) {
		panic("scriptedReader: 单步文本超过读缓冲，测试脚本有误")
	}
	return copy(p, step.text), nil
}

// Close 释放阻塞中的读取 goroutine；门控不关闭源，故由测试负责。
func (r *scriptedReader) Close() error {
	r.once.Do(func() { close(r.release) })
	return nil
}

// chatContentChunk 造一个含 size 个 ASCII 字符内容的 openai-chat 内容帧。
func chatContentChunk(size int) string {
	return `data: {"choices":[{"delta":{"content":"` + strings.Repeat("a", size) + `"}}]}` + "\n\n"
}

const ladderChatPingChunk = "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n"

// ladderOptions 是速率闸用例的公共选项：θ=1000 B/s，检查点缩到 20/60/200ms。
func ladderOptions() Options {
	opts := baseOptions()
	opts.PrebufferEventCap = 500
	opts.CaptureCommitMarker = true
	opts.PrecommitRate = 1000
	opts.ladderStages = testStages()
	return opts
}

// TestRateLadderCommitsAtFirstCheckpoint 是最短正例：首档（2θ）达标即放行，
// 不得等到次档——否则正常请求会被无谓拖到 3s。
func TestRateLadderCommitsAtFirstCheckpoint(t *testing.T) {
	opts := ladderOptions()
	// 首档 20ms 需 > 1000×2×0.02 = 40 字节；给 100。
	source := newScriptedReader(scriptedStep{text: chatContentChunk(100)})
	defer func() { _ = source.Close() }()

	startedAt := time.Now()
	result, err := Run(context.Background(), source, opts)
	elapsed := time.Since(startedAt)
	if err != nil {
		t.Fatalf("首档达标应提交，得错误 %v", err)
	}
	if len(result.Prefix) == 0 {
		t.Fatal("提交必须带上门控期间暂存的前缀")
	}
	if result.Marker == nil || result.Marker.LadderStage != 0 {
		t.Fatalf("应由第 0 档放开，得 marker=%+v", result.Marker)
	}
	if elapsed >= testStages()[1].elapsed {
		t.Fatalf("首档达标不得拖到次档：耗时 %v", elapsed)
	}
}

// TestRateLadderCommitsAtSecondCheckpoint 钉住次档的存在意义：
// 「首秒慢但随后追上」必须能在 3s（此处 60ms）档放行，而不是一路判死。
// 这也是「离散检查点」的判别性用例：若改成连续越线即提交，本用例走的路径就不复存在。
func TestRateLadderCommitsAtSecondCheckpoint(t *testing.T) {
	opts := ladderOptions()
	stages := testStages()
	source := newScriptedReader(
		// 首档 20ms 需 40 字节，只给 10 ⇒ 首档不过。
		scriptedStep{text: chatContentChunk(10)},
		// 次档 60ms 需 60 字节；30ms 时再补 55 ⇒ 累计 65 达标。
		scriptedStep{delay: 30 * time.Millisecond, text: chatContentChunk(55)},
	)
	defer func() { _ = source.Close() }()

	startedAt := time.Now()
	result, err := Run(context.Background(), source, opts)
	elapsed := time.Since(startedAt)
	if err != nil {
		t.Fatalf("次档达标应提交，得错误 %v", err)
	}
	if result.Marker == nil || result.Marker.LadderStage != 1 {
		t.Fatalf("应由第 1 档放开，得 marker=%+v", result.Marker)
	}
	if elapsed < stages[0].elapsed {
		t.Fatalf("首档未达标时不得提前放行：耗时 %v", elapsed)
	}
}

// TestRateLadderVerdictsSlowWithZeroClientBytes 是最重要的判别性用例：
// 三档皆不过即判 FailSlowRate，且**客户端零字节**（失败不带前缀）。
func TestRateLadderVerdictsSlowWithZeroClientBytes(t *testing.T) {
	opts := ladderOptions()
	stages := testStages()
	source := newScriptedReader(scriptedStep{text: chatContentChunk(20)})
	defer func() { _ = source.Close() }()

	startedAt := time.Now()
	result, err := Run(context.Background(), source, opts)
	elapsed := time.Since(startedAt)

	var precommit *PrecommitError
	if !errors.As(err, &precommit) {
		t.Fatalf("三档皆不过应判慢，得错误 %v", err)
	}
	if precommit.Reason != FailSlowRate {
		t.Fatalf("失败原因应为 %s，得 %s", FailSlowRate, precommit.Reason)
	}
	if len(result.Prefix) != 0 {
		t.Fatalf("判慢时客户端必须零字节，前缀却非空（%d 块）", len(result.Prefix))
	}
	if precommit.SlowPayloadBytes != 20 {
		t.Fatalf("报文应带实测累计字节 20，得 %d", precommit.SlowPayloadBytes)
	}
	if elapsed < stages[2].elapsed {
		t.Fatalf("末档未到即判慢：耗时 %v", elapsed)
	}
	if elapsed >= stages[2].elapsed+100*time.Millisecond {
		t.Fatalf("末档到期后应立即判慢，耗时 %v", elapsed)
	}
	// 报文必须可解析且带可区分的种类，供上层与标定消费。
	body := GateErrorBody(precommit)
	if !strings.Contains(body, `"reason":"slow_rate"`) || !strings.Contains(body, `"slow_payload_bytes":20`) {
		t.Fatalf("失败体应带种类与实测速率，得 %s", body)
	}
}

// TestRateLadderShortResponseCommitsImmediately 钉住短响应豁免：
// 总长不够阈值但上游已正常结束（终止帧），必须立即提交而不是判慢。
func TestRateLadderShortResponseCommitsImmediately(t *testing.T) {
	opts := ladderOptions()
	// 内容帧与终止帧同处一个 chunk：内容只有 7 字节，远不够任何档位。
	chunk := chatContentChunk(7) + "data: [DONE]\n\n"
	source := newScriptedReader(scriptedStep{text: chunk})
	defer func() { _ = source.Close() }()

	startedAt := time.Now()
	result, err := Run(context.Background(), source, opts)
	elapsed := time.Since(startedAt)
	if err != nil {
		t.Fatalf("短响应应提交，得错误 %v", err)
	}
	if len(result.Prefix) == 0 {
		t.Fatal("提交必须带上已缓冲的内容")
	}
	if elapsed >= testStages()[0].elapsed {
		t.Fatalf("短响应不得等检查点：耗时 %v", elapsed)
	}
}

// TestRateLadderIgnoresHeartbeatOnlyStream 钉住时钟起点：
// 只发中性帧（心跳/头帧）的流不启动时钟，故到点后判的是「空流」而不是「低速」。
//
// 本用例把心跳时长设到**超过末档**：若时钟被中性帧错误启动，判出的会是 FailSlowRate。
func TestRateLadderIgnoresHeartbeatOnlyStream(t *testing.T) {
	opts := ladderOptions()
	source := newScriptedReader(
		scriptedStep{text: ladderChatPingChunk},
		scriptedStep{delay: testStages()[2].elapsed + 50*time.Millisecond, text: ladderChatPingChunk},
		scriptedStep{text: ""},
	)
	defer func() { _ = source.Close() }()

	_, err := Run(context.Background(), source, opts)
	var precommit *PrecommitError
	if !errors.As(err, &precommit) {
		t.Fatalf("应报空流，得错误 %v", err)
	}
	if precommit.Reason != FailEmptyStream {
		t.Fatalf("中性帧不得启动时钟（否则会误判为低速）：原因 %s", precommit.Reason)
	}
}

// TestRateLadderCommitsWhenContentFillsByteCap 钉住「内容撞满前缀上限即提交」：
// 能在一秒内撞满上限的内容本身就证明这家不慢，此时报 prebuffer_overflow 是回归
// （旧语义下早已在首个内容帧提交，根本不会走到这里）。
func TestRateLadderCommitsWhenContentFillsByteCap(t *testing.T) {
	opts := ladderOptions()
	// 上限压到极小，让累计内容跨过它；单帧仍需短于上限（帧自身的行长度限制由 parser 管，
	// 那是既有约束，与本用例无关）。检查点故意设得很远，确保不是检查点放开的。
	opts.PrebufferByteCap = 256
	opts.PrebufferEventCap = 10_000
	opts.ladderStages = []ladderStage{
		{time.Hour, PrecommitFastMultiplier},
		{time.Hour, 1},
		{time.Hour, 1},
	}
	source := newScriptedReader(
		scriptedStep{text: chatContentChunk(100)},
		scriptedStep{text: chatContentChunk(100)},
		scriptedStep{text: chatContentChunk(100)},
	)
	defer func() { _ = source.Close() }()

	result, err := Run(context.Background(), source, opts)
	if err != nil {
		t.Fatalf("内容撞满上限应提交而非报错，得 %v", err)
	}
	if len(result.Prefix) == 0 {
		t.Fatal("提交必须带上前缀")
	}
}

// TestRateLadderDisabledKeepsLegacyCommitPoint 钉住未启用时的既有语义：
// θ<=0 时提交点仍在首个内容帧，不得引入任何等待。
func TestRateLadderDisabledKeepsLegacyCommitPoint(t *testing.T) {
	opts := ladderOptions()
	opts.PrecommitRate = 0
	// 极小内容：若误启用速率闸，这一帧在任一档都通不过，会被判慢。
	source := newScriptedReader(scriptedStep{text: chatContentChunk(3)})
	defer func() { _ = source.Close() }()

	startedAt := time.Now()
	result, err := Run(context.Background(), source, opts)
	elapsed := time.Since(startedAt)
	if err != nil {
		t.Fatalf("未启用速率闸时应照旧提交，得错误 %v", err)
	}
	if len(result.Prefix) == 0 {
		t.Fatal("提交必须带上前缀")
	}
	if result.Marker != nil && result.Marker.LadderStage != -1 {
		t.Fatalf("未启用时不应有速率闸档位，得 %d", result.Marker.LadderStage)
	}
	if elapsed >= testStages()[0].elapsed {
		t.Fatalf("未启用时不得有任何等待：耗时 %v", elapsed)
	}
}

// TestRateLadderBuffersContentAcrossChunks 钉住「后移提交点」的核心不变量：
// 判慢前暂存的内容一字节都不能丢——提交时前缀必须包含全部已收字节。
func TestRateLadderBuffersContentAcrossChunks(t *testing.T) {
	opts := ladderOptions()
	source := newScriptedReader(
		scriptedStep{text: chatContentChunk(30)},
		scriptedStep{delay: 10 * time.Millisecond, text: chatContentChunk(30)},
	)
	defer func() { _ = source.Close() }()

	result, err := Run(context.Background(), source, opts)
	if err != nil {
		t.Fatalf("累计 60 字节应在次档放行，得错误 %v", err)
	}
	prefix := string(result.PrefixBytes())
	if !strings.Contains(prefix, strings.Repeat("a", 30)) {
		t.Fatalf("前缀必须包含已缓冲的两个内容帧，得 %q", prefix)
	}
	if strings.Count(prefix, `"choices"`) != 2 {
		t.Fatalf("两个内容帧都应在前缀里，得 %q", prefix)
	}
}
