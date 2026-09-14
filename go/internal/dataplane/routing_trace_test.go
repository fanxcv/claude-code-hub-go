package dataplane

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
)

// baseRoutingTraceFacts 造一份「真跑过一条串行成功路径」的事实：
// 一次尝试、有实测起止时刻、2xx、带首字节读数。
func baseRoutingTraceFacts(startedAt time.Time) routingTraceFacts {
	firstByte := int64(120)
	ttft := int64(130)
	duration := int64(400)
	return routingTraceFacts{
		StartedAt:  startedAt,
		FinishedAt: startedAt.Add(time.Duration(duration) * time.Millisecond),
		Attempts: []forward.AttemptOutcome{{
			ProviderID:   42,
			ProviderName: "provider-a",
			Attempt:      1,
			StatusCode:   200,
			Reason:       forward.ReasonRequestSuccess,
			DurationMS:   duration,
			StartedAt:    startedAt.Add(5 * time.Millisecond),
			FinishedAt:   startedAt.Add(time.Duration(duration) * time.Millisecond),
		}},
		StatusCode:  200,
		DurationMS:  duration,
		FirstByteMS: &firstByte,
		TTFTMS:      &ttft,
	}
}

func decodeRoutingTrace(t *testing.T, raw []byte) routingTraceV1 {
	t.Helper()
	if raw == nil {
		t.Fatal("期望产出 trace 字节，实际为 nil")
	}
	var trace routingTraceV1
	if err := json.Unmarshal(raw, &trace); err != nil {
		t.Fatalf("trace 不是合法 JSON：%v（原文 %s）", err, raw)
	}
	return trace
}

// TestRoutingTraceRecordsSingleUpstreamPath 钉住串行路径的记录口径：
// 模式是 single_upstream（**绝不**是 discovery），事件带实测时刻与真实供应商。
func TestRoutingTraceRecordsSingleUpstreamPath(t *testing.T) {
	startedAt := time.Date(2026, 9, 13, 3, 0, 0, 0, time.UTC)
	trace := decodeRoutingTrace(t, buildRoutingTrace(baseRoutingTraceFacts(startedAt)))

	if trace.Version != 1 {
		t.Fatalf("version 应为 1，实际 %d", trace.Version)
	}
	if trace.Mode != routingTraceModeSingleUpstream {
		t.Fatalf("模式应为 %s，实际 %s", routingTraceModeSingleUpstream, trace.Mode)
	}
	// Go 没有 discovery 子系统：这两项必须恒为 false，否则是在声称不存在的机制。
	if trace.DiscoveryEnabled || trace.Eligible {
		t.Fatalf("Go 无 discovery 机制，discoveryEnabled/eligible 必须为 false：%+v", trace)
	}
	if trace.StartedAt != startedAt.UnixMilli() {
		t.Fatalf("startedAt 应为请求进入数据面的实测时刻，实际 %d", trace.StartedAt)
	}
	types := make([]string, 0, len(trace.Events))
	for _, event := range trace.Events {
		types = append(types, event.Type)
		if event.At == 0 {
			t.Fatalf("事件 %s 缺实测时间戳（at=0）", event.Type)
		}
	}
	want := []string{
		routingTraceEventRequestStarted,
		routingTraceEventAttemptStarted,
		routingTraceEventAttemptFinished,
		routingTraceEventWinnerCommitted,
		routingTraceEventRequestFinished,
	}
	if len(types) != len(want) {
		t.Fatalf("事件序列应为 %v，实际 %v", want, types)
	}
	for index := range want {
		if types[index] != want[index] {
			t.Fatalf("事件序列第 %d 项应为 %s，实际 %s（全部 %v）", index, want[index], types[index], types)
		}
	}
	if trace.Summary == nil {
		t.Fatal("summary 必须产出")
	}
	if trace.Summary.MaxActiveAttempts != 1 {
		t.Fatalf("串行路径的并发上限应为 1，实际 %d", trace.Summary.MaxActiveAttempts)
	}
	if trace.Summary.WinnerProviderID == nil || *trace.Summary.WinnerProviderID != 42 {
		t.Fatalf("winnerProviderId 应为 42，实际 %v", trace.Summary.WinnerProviderID)
	}
	if trace.Summary.TTFTMs == nil || *trace.Summary.TTFTMs != 130 {
		t.Fatalf("ttftMs 应透传实测值 130，实际 %v", trace.Summary.TTFTMs)
	}
	if trace.Summary.Outcome != routingTraceOutcomeSuccess {
		t.Fatalf("结局应为 success，实际 %s", trace.Summary.Outcome)
	}
}

// TestRoutingTraceMarksHedgeRunAndSaturation 钉住竞速路径：
// 有 hedge 词条或饱和事件时模式为 legacy_hedge，且饱和事件带上实测并发数与上限。
func TestRoutingTraceMarksHedgeRunAndSaturation(t *testing.T) {
	startedAt := time.Date(2026, 9, 13, 3, 0, 0, 0, time.UTC)
	facts := baseRoutingTraceFacts(startedAt)
	facts.Attempts[0].Reason = forward.ReasonHedgeWinner
	recorder := newRoutingTraceRecorder(startedAt)
	recorder.HedgeSlotSaturated(7, "provider-b", 2, 2, startedAt.Add(150*time.Millisecond))
	facts.Saturations = recorder.saturations

	trace := decodeRoutingTrace(t, buildRoutingTrace(facts))
	if trace.Mode != routingTraceModeLegacyHedge {
		t.Fatalf("模式应为 %s，实际 %s", routingTraceModeLegacyHedge, trace.Mode)
	}
	var saturation *routingTraceEventV1
	for index := range trace.Events {
		if trace.Events[index].Type == routingTraceEventHedgeSaturated {
			saturation = &trace.Events[index]
		}
	}
	if saturation == nil {
		t.Fatal("饱和事件必须落进 events（这正是链词表装不下的那类事实）")
	}
	if saturation.ActiveAttemptCount != 2 || saturation.ConfiguredCap != 2 {
		t.Fatalf("饱和事件应带实测并发数与上限 2/2，实际 %d/%d",
			saturation.ActiveAttemptCount, saturation.ConfiguredCap)
	}
	if saturation.Provider == nil || saturation.Provider.ID != 7 {
		t.Fatalf("饱和事件应带被拒候选的供应商身份，实际 %+v", saturation.Provider)
	}
	if trace.Summary.MaxActiveAttempts < 2 {
		t.Fatalf("竞速路径的并发下界应 ≥2（有一次饱和且并发实测为 2），实际 %d", trace.Summary.MaxActiveAttempts)
	}
}

// TestRoutingTraceSkipsUnmeasuredAttemptTimes 钉住「没有实测时刻就不产出该事件」：
// hedge 在途输家在胜者裁决时被批量落链，本就没有起止时刻——不得拿别的时刻顶替。
func TestRoutingTraceSkipsUnmeasuredAttemptTimes(t *testing.T) {
	startedAt := time.Date(2026, 9, 13, 3, 0, 0, 0, time.UTC)
	facts := baseRoutingTraceFacts(startedAt)
	facts.Attempts = append(facts.Attempts, forward.AttemptOutcome{
		ProviderID:   99,
		ProviderName: "loser",
		Attempt:      2,
		Reason:       forward.ReasonHedgeLoserCancel,
		// StartedAt / FinishedAt 零值：没有测量过，故不该出现在 events 里。
	})

	trace := decodeRoutingTrace(t, buildRoutingTrace(facts))
	for _, event := range trace.Events {
		if event.Provider != nil && event.Provider.ID == 99 {
			t.Fatalf("未测到时刻的尝试不得产出事件，实际产出 %+v", event)
		}
	}
	if trace.Summary.AttemptsPerRequest != 2 {
		t.Fatalf("attempts 计数应含未测时刻的那次（它确实发生了），实际 %d", trace.Summary.AttemptsPerRequest)
	}
	if trace.Summary.CancelFailures != 1 {
		t.Fatalf("输家取消应计入 cancelFailures，实际 %d", trace.Summary.CancelFailures)
	}
}

// TestRoutingTraceNilWhenNothingMeasured 是反证：没有任何真实留痕时不产出 trace
// （列保持原值），避免界面显示「已记录但什么都没有」。
func TestRoutingTraceNilWhenNothingMeasured(t *testing.T) {
	startedAt := time.Date(2026, 9, 13, 3, 0, 0, 0, time.UTC)
	if raw := buildRoutingTrace(routingTraceFacts{StartedAt: startedAt}); raw != nil {
		t.Fatalf("无尝试且无饱和事件时应返回 nil，实际 %s", raw)
	}
	if raw := buildRoutingTrace(routingTraceFacts{}); raw != nil {
		t.Fatalf("连请求起点都没有时应返回 nil，实际 %s", raw)
	}
}

// TestRoutingTraceTruncatesAtNodeCap 钉住上限：与 Node 的 ROUTING_TRACE_MAX_EVENTS 一致。
func TestRoutingTraceTruncatesAtNodeCap(t *testing.T) {
	startedAt := time.Date(2026, 9, 13, 3, 0, 0, 0, time.UTC)
	facts := routingTraceFacts{StartedAt: startedAt, FinishedAt: startedAt.Add(time.Second), StatusCode: 200}
	for index := 0; index < routingTraceMaxEvents; index++ {
		at := startedAt.Add(time.Duration(index) * time.Millisecond)
		facts.Attempts = append(facts.Attempts, forward.AttemptOutcome{
			ProviderID: int64(index + 1),
			Attempt:    index + 1,
			StatusCode: 200,
			Reason:     forward.ReasonRequestSuccess,
			StartedAt:  at,
			FinishedAt: at.Add(time.Millisecond),
		})
	}
	trace := decodeRoutingTrace(t, buildRoutingTrace(facts))
	if len(trace.Events) != routingTraceMaxEvents {
		t.Fatalf("事件应截断到 %d 条，实际 %d", routingTraceMaxEvents, len(trace.Events))
	}
	if !trace.Truncated {
		t.Fatal("截断时必须置 truncated=true（Node 的同一语义）")
	}
}

// TestRoutingTraceFactsComeFromSettler 钉住接线：结算器把**实际尝试留痕**交给构造器，
// 而不是自己造一份。用桩 state 覆盖 convert 依赖以外的字段。
func TestRoutingTraceFactsComeFromSettler(t *testing.T) {
	startedAt := time.Date(2026, 9, 13, 3, 0, 0, 0, time.UTC)
	settler := &storeSettler{state: &RequestState{StartedAt: startedAt, trace: newRoutingTraceRecorder(startedAt)}}
	attempts := []forward.AttemptOutcome{{
		ProviderID: 5,
		Attempt:    1,
		StatusCode: 200,
		Reason:     forward.ReasonRequestSuccess,
		StartedAt:  startedAt,
		FinishedAt: startedAt.Add(10 * time.Millisecond),
	}}
	raw := settler.routingTrace(attempts, startedAt.Add(10*time.Millisecond), 200, nil, nil)
	trace := decodeRoutingTrace(t, raw)
	if trace.Summary == nil || trace.Summary.AttemptsPerRequest != 1 {
		t.Fatalf("trace 应取自传入的真实留痕，实际 %+v", trace.Summary)
	}
	if trace.Summary.TTFTMs != nil {
		t.Fatalf("无实测 TTFT 时应写 null，实际 %v", trace.Summary.TTFTMs)
	}
}

// TestRoutingTraceRecorderIsConcurrencySafe 覆盖收集器的并发调用面：
// 饱和事件由竞速的阈值计时器协程发出（每个 attempt 一个 time.AfterFunc），多个候选可能
// 同时被拒；读取发生在终态结算的另一个协程。本用例在 -race 下必须干净。
func TestRoutingTraceRecorderIsConcurrencySafe(t *testing.T) {
	startedAt := time.Date(2026, 9, 13, 3, 0, 0, 0, time.UTC)
	recorder := newRoutingTraceRecorder(startedAt)
	const writers, perWriter = 8, 16

	var wg sync.WaitGroup
	for writer := 0; writer < writers; writer++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for index := 0; index < perWriter; index++ {
				recorder.HedgeSlotSaturated(int64(id+1), "provider", 2, 2, startedAt.Add(time.Duration(index)*time.Millisecond))
				_ = recorder.snapshotSaturations() // 读侧与写侧并发
			}
		}(writer)
	}
	wg.Wait()

	saturations := recorder.snapshotSaturations()
	if len(saturations) != writers*perWriter {
		t.Fatalf("饱和事件应全部保留（%d 条），实际 %d 条", writers*perWriter, len(saturations))
	}
	facts := baseRoutingTraceFacts(startedAt)
	facts.Saturations = saturations
	trace := decodeRoutingTrace(t, buildRoutingTrace(facts))
	if trace.Mode != routingTraceModeLegacyHedge {
		t.Fatalf("有饱和事件时模式应为 %s，实际 %s", routingTraceModeLegacyHedge, trace.Mode)
	}
}
