package terminal

import (
	"context"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件量的是**响应尾链上省掉的那部分**：终态 + 成本两笔 writer 道往返。
//
// 夹具用「每次写固定耗时」的假 writer 模拟真实往返（本地 PG 实测在 1ms 量级，生产经连接池
// 同量级）。测的是 Settle 调用本身的墙钟，不含上游耗时——那部分两种模式相同。
//
// 跑法：
//
//	go test -run '^$' -bench 'BenchmarkResponsePathSettle' -benchtime=20000x ./internal/terminal/
//
// 为什么必须给定次数：异步模式测的是「入队」，队列容量有限（这里取 env 契约上限 200000），
// 无限跑会把降级同步写也算进来，那测的就不是入队成本了。用例结尾用 Degraded/Dropped 自检。

// latencyWriter 是「每次写都要走一次往返」的假 writer。
type latencyWriter struct {
	*fakeWriter
	roundTrip time.Duration
}

func (w *latencyWriter) UpdateDetailsIfUnfinalized(
	ctx context.Context,
	id int64,
	patch store.DetailsPatch,
) (bool, error) {
	time.Sleep(w.roundTrip)
	return w.fakeWriter.UpdateDetailsIfUnfinalized(ctx, id, patch)
}

func (w *latencyWriter) UpdateWinnerCost(ctx context.Context, id int64, winnerCost string, breakdown []byte) error {
	time.Sleep(w.roundTrip)
	return w.fakeWriter.UpdateWinnerCost(ctx, id, winnerCost, breakdown)
}

func newLatencyWriter(roundTrip time.Duration) *latencyWriter {
	// 队列里放一条 committed=true：后续调用都复用最后一条结果，故每次都真的写完「终态 + 成本」。
	return &latencyWriter{
		fakeWriter: &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}},
		roundTrip:  roundTrip,
	}
}

func benchmarkSettlement() Settlement {
	return Settlement{
		StatusCode:    200,
		DurationMS:    intPtr(62),
		TTFTMS:        intPtr(43),
		Usage:         Usage{InputTokens: int64Ptr(1), OutputTokens: int64Ptr(1)},
		Cost:          &Cost{Total: "0.000024", Breakdown: []byte(`{"total":"0.000024"}`)},
		ProviderChain: []byte(`[{"id":1,"reason":"request_success"}]`),
	}
}

// BenchmarkResponsePathSettleSync 是接线前的形态：Settle 返回时两笔写都已完成。
func BenchmarkResponsePathSettleSync(b *testing.B) {
	writer := newLatencyWriter(time.Millisecond)
	settler := New(writer, Options{MaxAttempts: 1, Backoff: func(int) time.Duration { return 0 }})
	settlement := benchmarkSettlement()
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := settler.Settle(ctx, int64(i+1), settlement); err != nil {
			b.Fatalf("同步结算失败: %v", err)
		}
	}
}

// BenchmarkResponsePathSettleAsyncEnqueue 是异步形态：Settle 只校验入参并入队。
func BenchmarkResponsePathSettleAsyncEnqueue(b *testing.B) {
	writer := newLatencyWriter(time.Millisecond)
	queue := NewWriteQueue(AsyncOptions{
		MaxPending:    200000,
		BatchSize:     128,
		FlushInterval: 20 * time.Millisecond,
	})
	defer queue.Stop()
	settler := New(writer, Options{
		MaxAttempts: 1,
		Backoff:     func(int) time.Duration { return 0 },
		Queue:       queue,
	})
	settlement := benchmarkSettlement()
	ctx := context.Background()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, err := settler.Settle(ctx, int64(i+1), settlement)
		if err != nil {
			b.Fatalf("入队失败: %v", err)
		}
		if !result.Queued {
			b.Fatalf("第 %d 条没走队列（容量不足会让本次测量失去意义）", i)
		}
	}
	b.StopTimer()
	stats := queue.Stats()
	b.ReportMetric(float64(stats.Degraded), "degraded")
	b.ReportMetric(float64(stats.Enqueued), "enqueued")
	if stats.Degraded != 0 {
		b.Fatalf("队列容量不足导致降级同步写，本次测量无效：%+v", stats)
	}
}
