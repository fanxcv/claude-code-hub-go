package gate

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// 本文件钉住「首字后低速探测」在门控层的语义：自首个非空 chunk 起超过阈值仍未提交
// 内容即判 FailSlowProbe（可换家）。默认（阈值 <=0）行为完全不变。

// slowReader 按给定间隔逐块吐出，模拟「先发头帧、内容帧迟迟不来」的上游。
type slowReader struct {
	chunks []string
	delays []time.Duration
	index  int
}

func (r *slowReader) Read(p []byte) (int, error) {
	if r.index >= len(r.chunks) {
		return 0, io.EOF
	}
	if r.index < len(r.delays) && r.delays[r.index] > 0 {
		time.Sleep(r.delays[r.index])
	}
	n := copy(p, r.chunks[r.index])
	r.index++
	return n, nil
}

// chatHeadFrame 是 openai-chat 的纯头帧（只有 role，无内容）——判为中性，不提交。
const chatHeadFrame = "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n"

// chatContentFrame 是带内容的帧——判为 VerdictContent，触发提交。
const chatContentFrame = "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"

func baseOptions() Options {
	return Options{
		Family:            FamilyOpenAIChat,
		ProviderID:        1,
		ProviderName:      "probe-test",
		PrebufferEventCap: 100,
		PrebufferByteCap:  1 << 20,
	}
}

// TestSlowProbeFiresWhenHeadArrivesButContentStalls 是本机制的正例：
// 头帧先到（首字节已到），内容帧迟迟不来 ⇒ 自首字节起超过阈值即判 FailSlowProbe。
func TestSlowProbeFiresWhenHeadArrivesButContentStalls(t *testing.T) {
	opts := baseOptions()
	opts.ProbeAfterFirstByte = 50 * time.Millisecond
	source := &slowReader{
		chunks: []string{chatHeadFrame, chatContentFrame},
		delays: []time.Duration{0, 400 * time.Millisecond},
	}

	startedAt := time.Now()
	_, err := Run(context.Background(), source, opts)
	elapsed := time.Since(startedAt)

	var precommit *PrecommitError
	if !errors.As(err, &precommit) {
		t.Fatalf("期望 PrecommitError，实得 %v", err)
	}
	if precommit.Reason != FailSlowProbe {
		t.Fatalf("原因应为 %s，实得 %s", FailSlowProbe, precommit.Reason)
	}
	// 判废必须发生在内容帧到达之前：耗时显著小于内容帧的 400ms 延迟。
	if elapsed >= 400*time.Millisecond {
		t.Fatalf("判废过晚（%v）：应在上游吐内容之前就判出", elapsed)
	}
}

// TestSlowProbeDisabledLeavesBehaviorUnchanged 是「默认关闭」的钉子：
// 阈值 <=0（列 NULL）时，同样「头帧到了、内容迟到」的上游必须正常提交，不得判废。
func TestSlowProbeDisabledLeavesBehaviorUnchanged(t *testing.T) {
	opts := baseOptions()
	opts.ProbeAfterFirstByte = 0
	source := &slowReader{
		chunks: []string{chatHeadFrame, chatContentFrame},
		delays: []time.Duration{0, 200 * time.Millisecond},
	}

	result, err := Run(context.Background(), source, opts)
	if err != nil {
		t.Fatalf("阈值关闭时不应判废，实得 %v", err)
	}
	if result.FramesSeen == 0 {
		t.Fatal("应正常提交并看到帧")
	}
	// 前缀里必须真的含内容帧（证明是正常提交而非空跑）。
	if !strings.Contains(string(result.PrefixBytes()), "hi") {
		t.Fatal("提交前缀里应含内容帧")
	}
}

// TestSlowProbeDoesNotFireWithinThreshold 钉住阈值内不误杀：
// 内容帧在阈值内到达即正常提交。
func TestSlowProbeDoesNotFireWithinThreshold(t *testing.T) {
	opts := baseOptions()
	opts.ProbeAfterFirstByte = 500 * time.Millisecond
	source := &slowReader{
		chunks: []string{chatHeadFrame, chatContentFrame},
		delays: []time.Duration{0, 30 * time.Millisecond},
	}

	result, err := Run(context.Background(), source, opts)
	if err != nil {
		t.Fatalf("阈值内到达不应判废，实得 %v", err)
	}
	if !strings.Contains(string(result.PrefixBytes()), "hi") {
		t.Fatal("提交前缀里应含内容帧")
	}
}

// stallReader 先吐一个头帧，然后**阻塞**（不返回 EOF）——模拟「首字到了、内容卡住」。
//
// 为何不能靠「发完头帧就 EOF」模拟：EOF 比探测定时器先到，门控会按 FailEmptyStream（空流）
// 判废而不是按 FailSlowProbe。两者都是换家，但归因不同，钉子必须能区分它们。
type stallReader struct {
	sent  bool
	block chan struct{}
}

func (r *stallReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, chatHeadFrame), nil
	}
	<-r.block
	return 0, io.EOF
}

// TestSlowProbeFiresWithoutAnyContent 钉住一个**关键事实**：探测窗口内上游只发中性帧，
// 一个内容 token 都没有。
//
// 这不是实现细节而是判据的前提：L2 的窗口是「首字节已到、内容尚未提交」，按定义窗口内
// 不含内容帧，因此窗口内已生成 token 恒为 0 —— 任何以「token/时长」为分子的速率判据
// （含 IsSlowProbe 的样本量闸与速率闸）在该窗口内恒不成立，只有纯时间判据可用。
func TestSlowProbeFiresWithoutAnyContent(t *testing.T) {
	opts := baseOptions()
	opts.ProbeAfterFirstByte = 40 * time.Millisecond
	block := make(chan struct{})
	defer close(block)
	source := &stallReader{block: block}

	_, err := Run(context.Background(), source, opts)
	var precommit *PrecommitError
	if !errors.As(err, &precommit) || precommit.Reason != FailSlowProbe {
		t.Fatalf("只发中性帧后卡住应判 FailSlowProbe，实得 %v", err)
	}
}

// TestProbeReadTimeoutDisabledNeverInventsThreshold 钉住「关闭就是关闭」：
// 阈值 <=0 时 probeReadTimeout 必须**绝不**使用任何出厂默认值（否则「列 NULL 即机制关闭」
// 的承诺会被静默推翻，而短测试的延迟小于任何合理默认值，端到端用例抓不到这个缺口）。
func TestProbeReadTimeoutDisabledNeverInventsThreshold(t *testing.T) {
	now := time.Now()
	// 不带 IdleTimeout，也不带探测阈值：两条超时都没配。
	opts := Options{}
	timeout, probeDecides := probeReadTimeout(opts, now.Add(-time.Hour), now)
	if timeout != 0 || probeDecides {
		t.Fatalf("阈值未配时不得启用探测，实得 %v/%v", timeout, probeDecides)
	}

	// 只配 IdleTimeout：首字节已过一小时，仍必须走 IdleTimeout 而非探测。
	opts = Options{IdleTimeout: 10 * time.Second}
	timeout, probeDecides = probeReadTimeout(opts, now.Add(-time.Hour), now)
	if timeout != 10*time.Second || probeDecides {
		t.Fatalf("阈值未配时应走 IdleTimeout，实得 %v/%v", timeout, probeDecides)
	}

	// 显式配 0（管理面把它设成 0 想关掉）：同样不得启用。
	opts = Options{IdleTimeout: 10 * time.Second, ProbeAfterFirstByte: 0}
	timeout, probeDecides = probeReadTimeout(opts, now.Add(-time.Hour), now)
	if timeout != 10*time.Second || probeDecides {
		t.Fatalf("阈值为 0 时不得启用探测，实得 %v/%v", timeout, probeDecides)
	}

	// 负值（越界配置）同样不得启用。
	opts = Options{IdleTimeout: 10 * time.Second, ProbeAfterFirstByte: -5 * time.Second}
	timeout, probeDecides = probeReadTimeout(opts, now.Add(-time.Hour), now)
	if timeout != 10*time.Second || probeDecides {
		t.Fatalf("阈值为负时不得启用探测，实得 %v/%v", timeout, probeDecides)
	}
}

// TestProbeReadTimeoutIsSmallerOfTwo 钉住两条超时的归因规则（纯函数，无需 IO）。
func TestProbeReadTimeoutIsSmallerOfTwo(t *testing.T) {
	now := time.Now()
	opts := Options{IdleTimeout: 10 * time.Second, ProbeAfterFirstByte: 30 * time.Second}

	// 首字节未到：探测未开始计时，只能用读间隔上限。
	timeout, probeDecides := probeReadTimeout(opts, time.Time{}, now)
	if timeout != 10*time.Second || probeDecides {
		t.Fatalf("首字节未到时应用 IdleTimeout，实得 %v/%v", timeout, probeDecides)
	}

	// 首字节已过 5 秒：探测剩余 25 秒 > IdleTimeout 10 秒 ⇒ 由 IdleTimeout 决定归因。
	timeout, probeDecides = probeReadTimeout(opts, now.Add(-5*time.Second), now)
	if timeout != 10*time.Second || probeDecides {
		t.Fatalf("剩余大于读间隔时应由 IdleTimeout 决定，实得 %v/%v", timeout, probeDecides)
	}

	// 首字节已过 25 秒：探测剩余 5 秒 < IdleTimeout ⇒ 由探测决定归因。
	timeout, probeDecides = probeReadTimeout(opts, now.Add(-25*time.Second), now)
	if timeout != 5*time.Second || !probeDecides {
		t.Fatalf("剩余小于读间隔时应由探测决定，实得 %v/%v", timeout, probeDecides)
	}

	// 探测已过期：必须返回**极小正超时**（0 是「不启用超时」的语义，会让读永久阻塞）。
	timeout, probeDecides = probeReadTimeout(opts, now.Add(-40*time.Second), now)
	if timeout <= 0 || !probeDecides {
		t.Fatalf("过期时应返回极小正超时并归因探测，实得 %v/%v", timeout, probeDecides)
	}
}
