package slowrate

import (
	"context"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/slowlog"
)

// 本文件钉住「进入隔离」事件（W1）：一次隔离期只记一条，且带触发原因。
//
// 为什么必须钉：这条事件是「今日隔离多少次」的唯一数据源。漏记、重记、或把两条写入路径的
// 成因混成一个，都不会报错——只会在界面上静默给出错的次数。

// TestQuarantineEnteredRecordedOncePerEpisode 钉住：达标那一次记一条，其后每个慢样本不重复记。
func TestQuarantineEnteredRecordedOncePerEpisode(t *testing.T) {
	h := newRecoveryHarness(t, 9310, 10)
	ctx := context.Background()

	// 触发阈值是 3（harness 的 params）：前两条未达阈值，不推进状态、不记事件。
	h.recorder.Record(ctx, h.slowFactsFor(1001))
	h.recorder.Record(ctx, h.slowFactsFor(1002))
	if got := quarantineEnteredCount(readSlowLogs(t, h, 9310)); got != 0 {
		t.Fatalf("未达触发阈值不该记进入隔离事件，实得 %d 条", got)
	}

	// 第三条达标：记一条。
	h.recorder.Record(ctx, h.slowFactsFor(1003))
	// 第四条仍在同一隔离期内：不重复记。
	h.recorder.Record(ctx, h.slowFactsFor(1004))

	events := readSlowLogs(t, h, 9310)
	if got := quarantineEnteredCount(events); got != 1 {
		t.Fatalf("一次隔离期应只记一条进入隔离事件，实得 %d 条：%+v", got, events)
	}
	for _, event := range events {
		if event.Kind != slowlog.KindQuarantineEntered {
			continue
		}
		if event.Reason != triggerSlowSample {
			t.Errorf("实测慢样本触发的隔离，reason 应为 %q，收到 %q", triggerSlowSample, event.Reason)
		}
		if event.ModelKey != h.model {
			t.Errorf("事件应带 modelKey %q，收到 %q", h.model, event.ModelKey)
		}
	}
}

// TestQuarantineEnteredCarriesPrecommitTrigger 钉住第二条写入路径的成因不被吞掉。
func TestQuarantineEnteredCarriesPrecommitTrigger(t *testing.T) {
	h := newRecoveryHarness(t, 9311, 10)
	ctx := context.Background()

	for _, requestID := range []int64{2001, 2002, 2003} {
		h.recorder.RecordPrecommit(ctx, PrecommitFacts{
			ProviderID: h.provider,
			SessionID:  "s-1",
			KeyID:      7,
			ModelKey:   h.model,
			RequestID:  requestID,
		})
	}

	events := readSlowLogs(t, h, 9311)
	if got := quarantineEnteredCount(events); got != 1 {
		t.Fatalf("提交前判废达标应记一条进入隔离事件，实得 %d 条：%+v", got, events)
	}
	for _, event := range events {
		if event.Kind == slowlog.KindQuarantineEntered && event.Reason != triggerPrecommit {
			t.Errorf("提交前判废触发的隔离，reason 应为 %q，收到 %q", triggerPrecommit, event.Reason)
		}
	}
}

func quarantineEnteredCount(events []slowlog.Event) int {
	count := 0
	for _, event := range events {
		if event.Kind == slowlog.KindQuarantineEntered {
			count++
		}
	}
	return count
}
