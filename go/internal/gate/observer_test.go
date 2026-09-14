package gate

import (
	"strings"
	"testing"
	"time"
)

func TestShadowObserverReportsOnceOnDecisiveFrame(t *testing.T) {
	var reports []ShadowReport
	base := time.Unix(1000, 0)
	observer := NewShadowObserver(ShadowConfig{
		Family:       FamilyAnthropic,
		ProviderID:   9,
		ProviderName: "shadow-provider",
		OnReport:     func(report ShadowReport) { reports = append(reports, report) },
		Now:          func() time.Time { return base },
	})

	chunk := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
	observer.Observe([]byte(chunk))
	if len(reports) != 0 {
		t.Fatalf("中性帧不应产出报告: %+v", reports)
	}

	observer.Observe([]byte(anthropicContentFrame("hi")))
	if len(reports) != 1 {
		t.Fatalf("首个内容帧应产出恰好一份报告，得到 %d", len(reports))
	}
	report := reports[0]
	if report.DecisiveVerdict != VerdictContent {
		t.Fatalf("决定性判定应为 content，得到 %s", report.DecisiveVerdict)
	}
	if !report.Divergent {
		t.Fatal("存在中性前缀时应标记 divergence")
	}
	if report.VerdictCounts[VerdictNeutral] != 1 || report.VerdictCounts[VerdictContent] != 1 {
		t.Fatalf("计数不符: %+v", report.VerdictCounts)
	}
	if report.ProviderID != 9 || report.ProviderName != "shadow-provider" {
		t.Fatalf("上下文不符: %+v", report)
	}
	if report.Incomplete {
		t.Fatal("正常路径不应标记 observation-incomplete")
	}

	observer.Observe([]byte(anthropicContentFrame("more")))
	if len(reports) != 1 {
		t.Fatalf("报告应只有一份，得到 %d", len(reports))
	}
}

func TestShadowObserverRecordsLagAndDivergence(t *testing.T) {
	now := time.Unix(2000, 0)
	var report *ShadowReport
	observer := NewShadowObserver(ShadowConfig{
		Family: FamilyOpenAIChat,
		OnReport: func(got ShadowReport) {
			copied := got
			report = &copied
		},
		Now: func() time.Time { return now },
	})

	observer.Observe([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"))
	if report == nil {
		t.Fatal("首个内容帧即决定性帧，应产出报告")
	}
	if report.Divergent {
		t.Fatal("无中性前缀且首个决定性帧为内容时不应标记 divergence")
	}
	if report.FirstContentLag != 0 {
		t.Fatalf("同一时刻应无延迟，得到 %v", report.FirstContentLag)
	}

	now = time.Unix(2000, 0)
	var lagged *ShadowReport
	lagObserver := NewShadowObserver(ShadowConfig{
		Family: FamilyOpenAIChat,
		OnReport: func(got ShadowReport) {
			copied := got
			lagged = &copied
		},
		Now: func() time.Time { return now },
	})
	lagObserver.Observe([]byte("data: {\"choices\":[{\"delta\":{}}]}\n\n"))
	now = time.Unix(2000, 0).Add(40 * time.Millisecond)
	lagObserver.Observe([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n"))
	if lagged == nil || lagged.FirstContentLag != 40*time.Millisecond {
		t.Fatalf("延迟应用首字节到决定性帧的间隔，得到 %+v", lagged)
	}
}

func TestShadowObserverReportsErrorFrame(t *testing.T) {
	var report *ShadowReport
	observer := NewShadowObserver(ShadowConfig{
		Family: FamilyAnthropic,
		OnReport: func(got ShadowReport) {
			copied := got
			report = &copied
		},
	})
	observer.Observe([]byte("event: error\ndata: {\"error\":{\"message\":\"x\"}}\n\n"))
	if report == nil || report.DecisiveVerdict != VerdictError {
		t.Fatalf("error 帧应作为决定性判定上报: %+v", report)
	}
	if !report.Divergent {
		t.Fatal("error 帧意味着门控会 failover，应标记 divergence")
	}
}

func TestShadowObserverIncompleteOnOverflow(t *testing.T) {
	var report *ShadowReport
	observer := NewShadowObserver(ShadowConfig{
		Family:           FamilyOpenAIChat,
		MaxBufferedBytes: 16,
		OnReport: func(got ShadowReport) {
			copied := got
			report = &copied
		},
	})
	observer.Observe([]byte("data: {\"pad\":\"" + strings.Repeat("x", 64) + "\"}\n"))
	if report == nil {
		t.Fatal("超限应产出 observation-incomplete 报告")
	}
	if !report.Incomplete {
		t.Fatalf("应标记 Incomplete: %+v", report)
	}
	if report.DecisiveVerdict != "" {
		t.Fatalf("提前终止不应有决定性判定: %+v", report)
	}
}

func TestShadowObserverWithoutSink(t *testing.T) {
	observer := NewShadowObserver(ShadowConfig{Family: FamilyAnthropic})
	observer.Observe([]byte(anthropicContentFrame("hi")))
	observer.Observe(nil)
	observer.Observe([]byte("data: {}\n\n"))
}

func TestShadowObserverDefaultBufferLimit(t *testing.T) {
	observer := NewShadowObserver(ShadowConfig{Family: FamilyAnthropic})
	if observer.parser.opts.MaxBufferedBytes != ShadowObserverMaxBufferBytes {
		t.Fatalf("默认上限应为 %d，得到 %d", ShadowObserverMaxBufferBytes, observer.parser.opts.MaxBufferedBytes)
	}
}
