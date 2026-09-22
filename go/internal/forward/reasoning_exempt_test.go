package forward

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/gate"
)

// 本文件钉住「推理型请求豁免提交前速率闸」这条契约，判据一律走**真正的消费缝**
// （precommitRate 与 gateOptions 递给 gate 的那两个值），不是只测内部的布尔字段——
// 否则「豁免标记被摘掉/没接上」照样全绿（同目录 rate_sampler_test.go 的
// TestPrecommitRateIsZeroInShadow 就是同一形制的先例）。
//
// 为什么必须豁免：推理型流是「先吐一小片思考、再静默十余秒」，闸的时钟已在首片语义内容帧
// 启动，10s 档的均速必然低于 θ ⇒ 结构上分不出「模型在思考」与「上游卡住」。生产实证：
// 6 分钟内 13 次判慢，13/13 全是 codex 高/最高推理档。

// reasoningGateOptions 造一份「闸已开且停滞探测已配」的选项，只按参数区分是否推理型。
func reasoningGateOptions(reasoning bool) StreamOptions {
	return StreamOptions{
		PrecommitRateFor:       func(int64) int { return 1000 },
		ProbeAfterFirstByteFor: func(int64) int { return 30 },
		ReasoningRequest:       reasoning,
	}
}

// TestReasoningRequestSkipsPrecommitRateGate 钉住豁免本身（对应钉子①②）。
//
// 两条断言缺一不可：precommitRate 是配置面的消费缝，gateOptions 递给 gate 的
// PrecommitRate 才是门控真正的输入（gate.NewLadder(<=0) 返回 nil ⇒ 走「首个语义内容帧
// 即提交」的既有路径）。只看前者会漏掉「gateOptions 里又被写回 θ」这种改法。
func TestReasoningRequestSkipsPrecommitRateGate(t *testing.T) {
	options := reasoningGateOptions(true)
	if got := options.precommitRate(167); got != 0 {
		t.Fatalf("推理型请求的 θ 必须为 0（否则闸仍会裁决），得 %d", got)
	}
	outcome := &AttemptOutcome{ProviderID: 167, ProviderName: "wb"}
	gateOptions := options.gateOptions(gate.FamilyOpenAIResponses, outcome, func() {})
	if gateOptions.PrecommitRate != 0 {
		t.Fatalf("递给 gate 的 PrecommitRate 必须为 0，得 %d", gateOptions.PrecommitRate)
	}
	if ladder := gate.NewLadder(gateOptions.PrecommitRate); ladder != nil {
		t.Fatalf("θ<=0 时 Ladder 必须为 nil（未开闸语义），得 %#v", ladder)
	}
}

// TestNonReasoningRequestKeepsPrecommitRateGate 钉住防误伤（对应钉子③，最关键的一条）。
//
// 非推理型请求下，配置面与门控输入都必须与改造前**逐字一致**：θ 原样透出、采样器照旧
// 按影子开关决定、递给 gate 的仍是同一个 θ。
func TestNonReasoningRequestKeepsPrecommitRateGate(t *testing.T) {
	options := reasoningGateOptions(false)
	if got := options.precommitRate(167); got != 1000 {
		t.Fatalf("非推理型请求的 θ 必须原样透出（1000），得 %d", got)
	}
	if !options.rateSamplerEnabled(167) {
		t.Fatal("非推理型请求的提交后采样器必须照旧启用")
	}
	outcome := &AttemptOutcome{ProviderID: 167, ProviderName: "wb"}
	gateOptions := options.gateOptions(gate.FamilyOpenAIResponses, outcome, func() {})
	if gateOptions.PrecommitRate != 1000 {
		t.Fatalf("递给 gate 的 PrecommitRate 必须仍是 1000，得 %d", gateOptions.PrecommitRate)
	}
	if ladder := gate.NewLadder(gateOptions.PrecommitRate); ladder == nil {
		t.Fatal("非推理型请求下闸必须仍然可用（Ladder 非 nil）")
	}
	// 影子开关的既有语义不得被本次改动扰动：影子优先于一切。
	shadowed := options
	shadowed.PrecommitShadow = true
	if got := shadowed.precommitRate(167); got != 0 {
		t.Fatalf("影子期 θ 仍必须为 0，得 %d", got)
	}
}

// TestForwardStreamReasoningRequestKeepsLegacyCommitPoint 端到端钉住豁免的**提交点语义**：
//
// 推理型请求开着速率闸跑真流时，提交点必须与**未开闸**逐字段一致（首个语义内容帧即提交）——
// 这正是「豁免与未开闸走同一条已验证路径」的含义，也是生产事故里需要的那条修正。
//
// 第三个 arm（同一份配置但**去掉豁免标记**）是**对照**，不可省：没有它，本用例对「配置被
// 摘掉 ⇒ θ 恒为 0」也绿（那时前两条断言仍全部成立），而豁免根本没生效。对照臂必须真的
// 由速率闸放开提交（LadderStage >= 0）。
//
// 流的形状照生产实测：message_start 之后先来一小片思考帧（thinking_delta，属语义内容），
// 闸的时钟就在这一帧启动；随后才有正文。上游在发完内容帧后拖住，以便越过 1s 检查点。
func TestForwardStreamReasoningRequestKeepsLegacyCommitPoint(t *testing.T) {
	chunks := reasoningShapeStreamChunks()

	runOnce := func(options StreamOptions) *gate.CommitMarker {
		t.Helper()
		server := reasoningShapeStreamServer(t, chunks, reasoningStreamHold)
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
		_, _ = consumeStream(t, result.Stream)
		return result.Stream.attempt.Marker
	}

	// θ=1（字节/秒）：假上游吐了几十字节且拖过 1s，首档 1s 检查点必然放行。
	armed := StreamOptions{PrecommitRateFor: func(int64) int { return 1 }}
	baseline := runOnce(StreamOptions{})
	withReasoning := runOnce(StreamOptions{
		PrecommitRateFor: func(int64) int { return 1 },
		ReasoningRequest: true,
	})

	if withReasoning.LadderStage != -1 {
		t.Fatalf("推理型请求不得由速率闸放开提交（LadderStage 应为 -1），得 %d", withReasoning.LadderStage)
	}
	// 只比**确定性**字段：ChunkIndex/BufferedBytes 取决于 TCP 分块（见影子用例的同一条理由）。
	if withReasoning.FrameIndex != baseline.FrameIndex || withReasoning.EventName != baseline.EventName {
		t.Fatalf("推理型请求的提交点必须与未开闸一致：baseline=%+v reasoning=%+v", *baseline, *withReasoning)
	}

	control := runOnce(armed)
	if control.LadderStage < 0 {
		t.Fatalf("对照失效：同一份配置去掉豁免标记后闸仍未放开提交（LadderStage=%d），"+
			"本用例因此证明不了豁免（去掉 precommitRate 里的那个判据它也会绿）", control.LadderStage)
	}
	if control.FrameIndex == baseline.FrameIndex {
		t.Fatalf("对照失效：开闸后的提交点与未开闸相同（%d），说明闸没真正参与提交", control.FrameIndex)
	}
}

// reasoningStreamHold 是上游发完内容帧后拖住的时长：必须越过速率闸的首档 1s 检查点，
// 又不拉长用例（三个 arm 各付一次）。
const reasoningStreamHold = 1300 * time.Millisecond

// reasoningShapeStreamServer 起一个可控节奏的假上游：先一次性发出全部"内容"帧并 flush，
// 拖住 hold 之后才发终止帧——这正是「先吐一小片、再静默」的形状。
func reasoningShapeStreamServer(t *testing.T, chunks []string, hold time.Duration) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		for _, chunk := range chunks[:len(chunks)-2] {
			fmt.Fprint(w, chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
		time.Sleep(hold)
		for _, chunk := range chunks[len(chunks)-2:] {
			fmt.Fprint(w, chunk)
		}
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// reasoningShapeStreamChunks 复刻生产实测的推理型流形状：先一小片思考帧，随后才是正文。
func reasoningShapeStreamChunks() []string {
	return []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-4-5\",\"usage\":{\"input_tokens\":12}}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"先想一秒\"}}\n\n",
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"pong\"}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
}

// TestReasoningRequestKeepsStagnationProbeArmed 钉住安全网（对应钉子④）。
//
// 豁免的只是**速率判据**；首字后停滞探测（FailSlowProbe，wb 现为默认 30s）必须仍然武装，
// 它是「上游真卡住」的兜底。此处断言它原样递给 gate。
func TestReasoningRequestKeepsStagnationProbeArmed(t *testing.T) {
	options := reasoningGateOptions(true)
	if got := options.probeAfterFirstByte(167); got != 30*time.Second {
		t.Fatalf("停滞探测阈值必须仍是 30s，得 %v", got)
	}
	outcome := &AttemptOutcome{ProviderID: 167, ProviderName: "wb"}
	gateOptions := options.gateOptions(gate.FamilyOpenAIResponses, outcome, func() {})
	if gateOptions.ProbeAfterFirstByte != 30*time.Second {
		t.Fatalf("递给 gate 的 ProbeAfterFirstByte 必须仍是 30s，得 %v", gateOptions.ProbeAfterFirstByte)
	}
}
