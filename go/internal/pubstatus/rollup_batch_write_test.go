package pubstatus

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// batchRecordingWriter 记录写入面被调用的次数与整份载荷。
//
// 存在的理由：写侧的唯一性能契约是「一次事件 = 一次往返」（Node 用 pipeline）。
// 逐条发送不会改变 dump 出来的键值，因此 golden 对照测不出这个回归——本替身才能钉住它。
type batchRecordingWriter struct {
	calls         int
	key           string
	coverageKey   string
	coverageValue string
	increments    []RollupIncrement
	ttlSeconds    int
	err           error
}

func (w *batchRecordingWriter) ApplyBatch(
	_ context.Context,
	key, coverageKey, coverageValue string,
	increments []RollupIncrement,
	ttlSeconds int,
) error {
	w.calls++
	w.key = key
	w.coverageKey = coverageKey
	w.coverageValue = coverageValue
	w.increments = increments
	w.ttlSeconds = ttlSeconds
	return w.err
}

// TestWriteRollupEventIsOneWrite 钉住事件捕获的往返数与载荷：
// 整次事件只调一次写入面，且这一次带齐「N 条增量 + coverage 起点 + 两个键的 TTL」。
func TestWriteRollupEventIsOneWrite(t *testing.T) {
	golden := loadWriteGolden(t)
	groups := goldenConfiguredGroups(t, golden.Groups)
	if RollupTTLSeconds != golden.RollupTTLSecs {
		t.Fatalf("TTL 与 Node golden 不一致：Go=%d Node=%d", RollupTTLSeconds, golden.RollupTTLSecs)
	}

	for _, item := range golden.Events {
		event := goldenEvent(t, item.Event)
		want := BuildRollupIncrements(event, groups)

		writer := &batchRecordingWriter{}
		result, err := WriteRollupEvent(context.Background(), writer, event, groups, golden.Prefix)
		if err != nil {
			t.Fatalf("事件 %s：写入报错 %v", item.Name, err)
		}

		// 无增量的事件是 `ignored`：连一次往返都不该有。
		if len(want) == 0 {
			if writer.calls != 0 {
				t.Fatalf("事件 %s：无增量却写了一次 Redis", item.Name)
			}
			if result.Written {
				t.Fatalf("事件 %s：无增量却被判为已写入", item.Name)
			}
			continue
		}

		if writer.calls != 1 {
			t.Fatalf("事件 %s：写入面被调用 %d 次，应恰好 1 次（一次事件一次往返）", item.Name, writer.calls)
		}
		if len(writer.increments) != len(want) {
			t.Fatalf("事件 %s：增量数 %d ≠ 折算结果 %d", item.Name, len(writer.increments), len(want))
		}
		for index := range want {
			if writer.increments[index] != want[index] {
				t.Fatalf("事件 %s 第 %d 条增量：Go=%+v 折算=%+v", item.Name, index, writer.increments[index], want[index])
			}
		}
		if writer.ttlSeconds != RollupTTLSeconds {
			t.Fatalf("事件 %s：TTL=%d，应为 %d", item.Name, writer.ttlSeconds, RollupTTLSeconds)
		}
		if result.IncrementCount != len(want) {
			t.Fatalf("事件 %s：IncrementCount=%d，应为 %d", item.Name, result.IncrementCount, len(want))
		}

		// coverage 起点 = 事件时刻对齐到 5 分钟边界；两个键都续 TTL（一次调用里带两个 Expire）。
		wantBucket, err := AlignBucketStartUTC(event.CreatedAt.UTC().Format(isoMilliLayout), PublicStatusRollupBucketMinutes)
		if err != nil {
			t.Fatalf("事件 %s：对齐桶起点失败 %v", item.Name, err)
		}
		if writer.coverageValue != wantBucket {
			t.Fatalf("事件 %s：coverage 起点=%s，应为 %s", item.Name, writer.coverageValue, wantBucket)
		}
		wantCoverageKey, err := BuildRollupCoverageStartKey(PublicStatusRollupBucketMinutes, golden.Prefix)
		if err != nil {
			t.Fatalf("事件 %s：构造 coverage 键失败 %v", item.Name, err)
		}
		if writer.coverageKey != wantCoverageKey {
			t.Fatalf("事件 %s：coverage 键=%s，应为 %s", item.Name, writer.coverageKey, wantCoverageKey)
		}
		if !strings.HasPrefix(writer.key, golden.Prefix) {
			t.Fatalf("事件 %s：桶键 %s 未带前缀 %s", item.Name, writer.key, golden.Prefix)
		}
	}
}

// TestWriteRollupEventBatchFailureIsRetryable 钉住批写入失败的分类：
// 写入面报错时结果必须是「未写入 + 可重试 + RollupWriteFailed」，与逐条发送时逐字一致。
func TestWriteRollupEventBatchFailureIsRetryable(t *testing.T) {
	golden := loadWriteGolden(t)
	groups := goldenConfiguredGroups(t, golden.Groups)

	var event RollupEvent
	for _, item := range golden.Events {
		candidate := goldenEvent(t, item.Event)
		if len(BuildRollupIncrements(candidate, groups)) > 0 {
			event = candidate
			break
		}
	}
	if event.CreatedAt.IsZero() {
		t.Fatalf("golden 里没有带增量的事件，夹具失效")
	}

	sentinel := errors.New("redis pipeline down")
	writer := &batchRecordingWriter{err: sentinel}
	result, err := WriteRollupEvent(context.Background(), writer, event, groups, golden.Prefix)
	if !errors.Is(err, sentinel) {
		t.Fatalf("写入失败应回到底层错误，实际 err=%v", err)
	}
	if result.Written || !result.Retryable || result.Reason != RollupWriteFailed {
		t.Fatalf("写入失败分类不符：%+v", result)
	}
	if result.IncrementCount != len(BuildRollupIncrements(event, groups)) {
		t.Fatalf("失败结果也要带增量数，实际 %d", result.IncrementCount)
	}
}
