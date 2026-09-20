package tracing

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"
)

// 队列满时必须丢而不是卡住：上报挂在终态收尾路径上，卡住就等于把响应收尾拖住。
func TestFullQueueDropsInsteadOfBlocking(t *testing.T) {
	collector := newCollector(t, 207)
	collector.hold() // 收集器不返回：后台 goroutine 会卡在出站调用上，队列随之填满

	tracer := newTestTracer(t, collector, Options{
		SampleRate:    1,
		QueueSize:     2,
		BatchSize:     1,
		FlushInterval: time.Hour, // 只靠批大小触发，让队列确定地被填满
	})

	started := time.Now()
	for id := int64(1); id <= 200; id++ {
		tracer.RecordTerminal(testRecord(id))
	}
	elapsed := time.Since(started)

	if elapsed > 2*time.Second {
		t.Fatalf("入队不得阻塞（200 条耗时 %s）", elapsed)
	}
	counters := tracer.Counters()
	if counters.Dropped == 0 {
		t.Fatalf("队列满时应有丢弃计数: %+v", counters)
	}
	if counters.Enqueued+counters.Dropped != 200 {
		t.Fatalf("入队与丢弃应覆盖全部 200 条: %+v", counters)
	}
}

// 采样：0 全丢、1 全留、中间按掷点（边界是严格小于）。
func TestSampling(t *testing.T) {
	cases := []struct {
		name        string
		rate        float64
		roll        float64
		wantEnqueue int
		wantSampled int
	}{
		{"全丢", 0, 0, 0, 1},
		{"全留", 1, 0.999, 1, 0},
		{"掷点命中", 0.5, 0.499, 1, 0},
		{"掷点未命", 0.5, 0.5, 0, 1},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			collector := newCollector(t, 207)
			tracer := newTestTracer(t, collector, Options{
				SampleRate:    testCase.rate,
				FlushInterval: time.Hour,
				BatchSize:     100,
				QueueSize:     8,
				Sample:        func() float64 { return testCase.roll },
			})
			tracer.RecordTerminal(testRecord(1))
			counters := tracer.Counters()
			if counters.Enqueued != uint64(testCase.wantEnqueue) {
				t.Fatalf("入队数不符: %+v", counters)
			}
			if counters.Sampled != uint64(testCase.wantSampled) {
				t.Fatalf("采样挡下数不符: %+v", counters)
			}
		})
	}
}

// 关闭时把队列里已入队的事实发完：进程退出不该把手里已有的事实丢掉。
func TestCloseFlushesPendingRecords(t *testing.T) {
	collector := newCollector(t, 207)
	tracer := newTestTracer(t, collector, Options{
		SampleRate:    1,
		FlushInterval: time.Hour, // 不靠定时器：只可能由收尾发出
		BatchSize:     100,       // 不靠批大小
		QueueSize:     16,
	})
	for id := int64(1); id <= 3; id++ {
		tracer.RecordTerminal(testRecord(id))
	}

	closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tracer.Close(closeCtx)

	captured := collector.captured()
	if len(captured) != 1 {
		t.Fatalf("收尾应把残余合成一次请求发出，得到 %d 次", len(captured))
	}
	batch := decodeBatch(t, captured[0].body)
	if len(batch.Batch) != 3 {
		t.Fatalf("收尾应发出 3 条事件，得到 %d", len(batch.Batch))
	}
	if counters := tracer.Counters(); counters.Sent != 3 {
		t.Fatalf("收尾发出的条数应计入成功: %+v", counters)
	}
}

// 关闭之后的入队必须无效（不阻塞、不 panic、不计数）——收尾与请求收口会并发。
func TestRecordAfterCloseIsNoop(t *testing.T) {
	collector := newCollector(t, 207)
	tracer := newTestTracer(t, collector, Options{SampleRate: 1, FlushInterval: time.Hour})
	tracer.RecordTerminal(testRecord(1))
	tracer.Close(context.Background())

	before := tracer.Counters()
	tracer.RecordTerminal(testRecord(2))
	if after := tracer.Counters(); after != before {
		t.Fatalf("关闭后不得再入队: 之前 %+v，之后 %+v", before, after)
	}
}

// 批量触发：达到批大小即发，不必等间隔。
func TestBatchSizeTriggersSend(t *testing.T) {
	collector := newCollector(t, 207)
	tracer := newTestTracer(t, collector, Options{
		SampleRate:    1,
		FlushInterval: time.Hour,
		BatchSize:     2,
		QueueSize:     8,
	})
	for id := int64(1); id <= 2; id++ {
		tracer.RecordTerminal(testRecord(id))
	}

	collector.waitForRequests(t, 1)
	if collector.count() != 1 {
		t.Fatalf("达到批大小应立刻发出一次，得到 %d 次", collector.count())
	}
}

// Close 与并发的 RecordTerminal 必须互斥：关闭返回后不得有记录落进「已无消费者的队列」。
//
// 判据取「入队数 == 发出数」：只要有一条记录是在后台 goroutine 退出之后才入队的，它就永远
// 不会被发出（旁路不 panic，只静默丢），这个等式就会破。批大小取得比总条数大，保证收尾的
// 一次 drain 能装下全部残余，从而「发不出去」只能由竞态造成。
func TestCloseIsSerializedWithRecord(t *testing.T) {
	const (
		rounds  = 50
		workers = 8
		perW    = 50
	)
	for round := 0; round < rounds; round++ {
		collector := newCollector(t, 207)
		tracer := newTestTracer(t, collector, Options{
			SampleRate:    1,
			QueueSize:     4096,
			BatchSize:     4096,
			FlushInterval: time.Hour,
		})
		var group sync.WaitGroup
		start := make(chan struct{})
		// 先播一条种子记录：保证每轮都有入队发生，不让调度波动把用例变成空跑。
		tracer.RecordTerminal(testRecord(0))
		for i := 0; i < workers; i++ {
			group.Add(1)
			go func(worker int) {
				defer group.Done()
				<-start
				for j := 0; j < perW; j++ {
					tracer.RecordTerminal(testRecord(int64(worker*1000 + j)))
				}
			}(i)
		}
		close(start)
		runtime.Gosched()
		tracer.Close(context.Background())
		group.Wait()

		counters := tracer.Counters()
		if counters.Enqueued < 1 {
			t.Fatalf("第 %d 轮：连种子记录都没入队，用例失去意义", round)
		}
		if counters.Sent != counters.Enqueued || counters.Failed != 0 {
			t.Fatalf("第 %d 轮：入队 %d、发出 %d、失败 %d（关闭竞态丢了记录）",
				round, counters.Enqueued, counters.Sent, counters.Failed)
		}
	}
}
