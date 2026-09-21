package terminal

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住异步终态写队列（batch.go）的语义：攒批与定时触发、满队列降级同步写、
// 提交后动作的时机、Stop 的收尾，以及**「不带队列即同步写」这条零回归路径**。
//
// 判据优先级：库里落没落、什么顺序落 —— 用假 writer 记录调用序列来断言。

// blockingWriter 让指定 id 的终态写停住，用来制造确定的「队列积压」与「满队列」状态。
type blockingWriter struct {
	*fakeWriter
	blockID   int64
	entered   chan struct{}
	release   chan struct{}
	enterOnce sync.Once
}

func newBlockingWriter(blockID int64) *blockingWriter {
	return &blockingWriter{
		fakeWriter: &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}},
		blockID:    blockID,
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
	}
}

func (w *blockingWriter) UpdateDetailsIfUnfinalized(
	ctx context.Context,
	id int64,
	patch store.DetailsPatch,
) (bool, error) {
	if id == w.blockID {
		w.enterOnce.Do(func() { close(w.entered) })
		<-w.release
	}
	return w.fakeWriter.UpdateDetailsIfUnfinalized(ctx, id, patch)
}

// debugRecordingLogger 除 warn 外还收集 debug 事件（队列的 flush 行只走 debug 级）。
type debugRecordingLogger struct {
	mu     sync.Mutex
	warns  []string
	debugs []string
}

func (l *debugRecordingLogger) Warn(event string, _ map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.warns = append(l.warns, event)
}

func (l *debugRecordingLogger) Debug(event string, _ map[string]any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.debugs = append(l.debugs, event)
}

func (l *debugRecordingLogger) hasDebug(want string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return containsEvent(l.debugs, want)
}

func (l *debugRecordingLogger) hasWarn(want string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return containsEvent(l.warns, want)
}

func containsEvent(events []string, want string) bool {
	for _, event := range events {
		if event == want {
			return true
		}
	}
	return false
}

func (l *debugRecordingLogger) warnCount(want string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	count := 0
	for _, event := range l.warns {
		if event == want {
			count++
		}
	}
	return count
}

// newAsyncFixture 造一个异步队列 + 结算器；队列由 t.Cleanup 停掉，用例不必自己收尾。
func newAsyncFixture(t *testing.T, writer Writer, options AsyncOptions) (*WriteQueue, *Settler) {
	t.Helper()
	queue := NewWriteQueue(options)
	t.Cleanup(queue.Stop)
	settler := New(writer, Options{
		MaxAttempts: 1,
		Backoff:     func(int) time.Duration { return 0 },
		Queue:       queue,
		Logger:      options.Logger,
	})
	return queue, settler
}

// 攒够 batchSize 就落库：三条一次 flush，且严格按入队顺序写。
func TestAsyncQueueFlushesWhenBatchFills(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{
		{committed: true}, {committed: true}, {committed: true},
	}}
	logger := &debugRecordingLogger{}
	queue, settler := newAsyncFixture(t, writer, AsyncOptions{
		MaxPending: 8, BatchSize: 3, FlushInterval: time.Hour, Logger: logger,
	})

	for id := int64(1); id <= 3; id++ {
		result, err := settler.Settle(context.Background(), id, okSettlement(nil))
		if err != nil {
			t.Fatalf("第 %d 条入队失败: %v", id, err)
		}
		if !result.Queued {
			t.Fatalf("异步模式下 Settle 应先入队（Queued=true），第 %d 条收到 %+v", id, result)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := queue.AwaitSettlement(ctx, 3); err != nil {
		t.Fatalf("攒够一批后应立刻落库，收到 %v，stats=%+v", err, queue.Stats())
	}
	if got := writer.unfinalizedIDs; len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("批内必须按入队顺序逐条写，收到 %v", got)
	}
	stats := queue.Stats()
	if stats.Rows != 3 || stats.Batches != 1 || stats.Pending != 0 {
		t.Fatalf("落库计数不符：%+v", stats)
	}
	if !logger.hasDebug("terminal_async_flush") {
		t.Fatalf("每次 flush 必须留一条 debug 行（含行数/耗时/积压）")
	}
}

// 不满一批时由 flush 间隔触发；等待面在此期间返回 false，落库后才放行。
func TestAsyncQueueFlushesOnIntervalAndUnblocksAwait(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}}
	queue, settler := newAsyncFixture(t, writer, AsyncOptions{
		MaxPending: 8, BatchSize: 100, FlushInterval: 20 * time.Millisecond,
	})

	if _, err := settler.Settle(context.Background(), 7, okSettlement(nil)); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	// 先把等待面钉在「还没落库」上：用极短窗口问一次，此时批还没到时间。
	shortCtx, cancelShort := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancelShort()
	if err := queue.AwaitSettlement(shortCtx, 7); err == nil {
		t.Fatalf("定时未到、批未满时不应已落库（否则这条用例失去意义）")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("未落库时的等待应是超时结论，收到 %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := queue.AwaitSettlement(ctx, 7); err != nil {
		t.Fatalf("flush 间隔到点后必须落库，收到 %v，stats=%+v", err, queue.Stats())
	}
	if writer.unfinalizedCalls != 1 {
		t.Fatalf("应恰好写一次终态，实际 %d 次", writer.unfinalizedCalls)
	}
}

// 队列满必须降级为同步写（不丢数据、不阻塞），并计数 + 留痕；Flush 等不到清空时如实超时。
func TestAsyncQueueFullFallsBackToSyncWrite(t *testing.T) {
	writer := newBlockingWriter(1)
	logger := &debugRecordingLogger{}
	// 容量 1 且 BatchSize=1：worker 被 id=1 的写卡住（它已从通道取走），id=2 占住通道，
	// id=3 必然撞满。
	queue, settler := newAsyncFixture(t, writer, AsyncOptions{
		MaxPending: 1, BatchSize: 1, FlushInterval: time.Hour, Logger: logger,
	})
	t.Cleanup(func() { close(writer.release) })

	if result, err := settler.Settle(context.Background(), 1, okSettlement(nil)); err != nil || !result.Queued {
		t.Fatalf("第一条应入队: result=%+v err=%v", result, err)
	}
	select {
	case <-writer.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("worker 未在限期内开始写第一条")
	}
	if result, err := settler.Settle(context.Background(), 2, okSettlement(nil)); err != nil || !result.Queued {
		t.Fatalf("第二条应占住通道（容量 1）: result=%+v err=%v", result, err)
	}

	result, err := settler.Settle(context.Background(), 3, okSettlement(nil))
	if err != nil {
		t.Fatalf("满队列时必须降级同步写而不是报错: %v", err)
	}
	if result.Queued {
		t.Fatalf("满队列时不得谎报「已入队」：%+v", result)
	}
	if !result.Committed {
		t.Fatalf("降级同步写必须真的写库并赢得终态：%+v", result)
	}
	stats := queue.Stats()
	if stats.Degraded != 1 {
		t.Fatalf("降级次数应为 1：%+v", stats)
	}
	if logger.warnCount("terminal_async_queue_full") != 1 {
		t.Fatalf("满队列必须留痕（且只记一次，避免过载时刷屏）：%v", logger.warns)
	}

	// 队列未清空时 Flush 必须如实超时，而不是假装干净。
	flushCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := queue.Flush(flushCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("队列未清空时 Flush 应返回超时错误，收到 %v", err)
	}
}

// 终态提交但成本写失败：必须计数成 cost_gap 并留 warn，而不是静默。
func TestAsyncQueueCountsCostGap(t *testing.T) {
	writer := &fakeWriter{
		unfinalizedQueue: []unfinalizedResult{{committed: true}},
		costQueue:        []error{errors.New("writer lane down")},
	}
	logger := &debugRecordingLogger{}
	queue, settler := newAsyncFixture(t, writer, AsyncOptions{
		MaxPending: 8, BatchSize: 1, FlushInterval: time.Hour, Logger: logger,
	})

	cost := &Cost{Total: "0.000024", Breakdown: []byte(`{"total":"0.000024"}`)}
	if _, err := settler.Settle(context.Background(), 11, okSettlement(cost)); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// 成本没落库就不是「已落库」：等待面必须把失败报出来，而不是报成功。
	if err := queue.AwaitSettlement(ctx, 11); !errors.Is(err, ErrCostWriteFailed) {
		t.Fatalf("成本写失败必须由等待面暴露（ErrCostWriteFailed），收到 %v", err)
	}
	stats := queue.Stats()
	if stats.CostGap != 1 || stats.Failed != 1 {
		t.Fatalf("「终态已提交、成本未落库」必须计入 cost_gap：%+v", stats)
	}
	if stats.Rows != 0 {
		t.Fatalf("写入失败的条目不得计入 Rows（否则落库量虚高）：%+v", stats)
	}
	// 排空但其中有失败：Flush 不得报「干净」。
	if err := queue.Flush(context.Background()); !errors.Is(err, ErrQueueWriteFailed) {
		t.Fatalf("队列清空但有写入失败时 Flush 应报 ErrQueueWriteFailed，收到 %v", err)
	}
	// 失败计数是「自上回 Flush 以来」的：再冲一次不应重复报同一条。
	if err := queue.Flush(context.Background()); err != nil {
		t.Fatalf("失败已被上回 Flush 报过，再冲一次应是干净的，收到 %v", err)
	}
	if !logger.hasWarn("terminal_async_flush_failed") {
		t.Fatalf("写入失败必须留 warn")
	}
}

// Stop 与 enqueue 并发时不得留下「既不在队列里、也永不被写」的条目。
//
// 窗口是真实的：停止判定与投递分开做时，Stop 可能恰好插在中间——worker 退出，而这条已登记
// pending 的条目永远等不到消费者（pending 不归零 ⇒ AwaitSettlement 一直等到超时，随后关
// 连接池就永久丢掉这条终态）。修法是把四步放同一把锁内（见 enqueue 的注释）。
//
// 断言的是不变式而不是时序：入队成功几条，worker 就得写完几条；Stop 返回后 pending 必须归零。
func TestStopRacesEnqueueLeavesNoOrphan(t *testing.T) {
	const rounds = 300
	for round := 0; round < rounds; round++ {
		// 容量 1 + 批 1：入队与 flush 都最频，窗口最窄也最容易被撞上。
		queue := NewWriteQueue(AsyncOptions{MaxPending: 1, BatchSize: 1, FlushInterval: time.Hour, Logger: nil})
		var accepted, writtenByWorker atomic.Int64
		stop := make(chan struct{})
		start := make(chan struct{})
		var workers sync.WaitGroup
		for i := 0; i < 8; i++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				for {
					select {
					case <-stop:
						return
					default:
					}
					ok := queue.enqueue(context.Background(), 0, func(context.Context) (Result, error) {
						writtenByWorker.Add(1)
						return Result{Committed: true}, nil
					}, nil)
					if ok {
						accepted.Add(1)
					}
					// !ok 即降级：调用方自己同步写（不丢，故不计入孤儿）。
				}
			}()
		}
		close(start)
		runtime.Gosched()
		queue.Stop()
		close(stop)
		workers.Wait()

		if pending := queue.Pending(); pending != 0 {
			t.Fatalf("第 %d 轮：Stop 之后仍有 %d 条卡在「已登记但无消费者」：%+v", round, pending, queue.Stats())
		}
		if got, want := writtenByWorker.Load(), accepted.Load(); got != want {
			t.Fatalf("第 %d 轮：入队成功 %d 条，worker 实写 %d 条（有孤儿）", round, want, got)
		}
	}
}

// 终态写失败必须由等待面报出，且 Flush 不得报「干净」：这两条是退出序列判「能否关依赖」的依据。
func TestAsyncQueueSurfacesTerminalWriteFailure(t *testing.T) {
	writeErr := errors.New("writer lane down")
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{err: writeErr}}}
	logger := &debugRecordingLogger{}
	queue, settler := newAsyncFixture(t, writer, AsyncOptions{
		MaxPending: 8, BatchSize: 1, FlushInterval: time.Hour, Logger: logger,
	})

	if _, err := settler.Settle(context.Background(), 41, okSettlement(nil)); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := queue.AwaitSettlement(ctx, 41); !errors.Is(err, writeErr) {
		t.Fatalf("写失败必须由等待面报出原错误，收到 %v", err)
	}
	if err := queue.Flush(context.Background()); !errors.Is(err, ErrQueueWriteFailed) {
		t.Fatalf("有写入失败时 Flush 应报 ErrQueueWriteFailed，收到 %v", err)
	}
	stats := queue.Stats()
	if stats.Failed != 1 || stats.Rows != 0 || stats.Pending != 0 {
		t.Fatalf("计数不符（失败不得计入 Rows）：%+v", stats)
	}
}

// 幂等去重（未赢得该行）既不是失败也不计入 Rows：两种口径搞混会让故障期的数虚高或虚低。
func TestAsyncQueueCountsIdempotentSeparately(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: false}}}
	queue, settler := newAsyncFixture(t, writer, AsyncOptions{
		MaxPending: 8, BatchSize: 1, FlushInterval: time.Hour,
	})

	if _, err := settler.Settle(context.Background(), 42, okSettlement(nil)); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := queue.AwaitSettlement(ctx, 42); err != nil {
		t.Fatalf("未赢得该行是幂等结论，不是失败，等待面应报 nil，收到 %v", err)
	}
	if err := queue.Flush(context.Background()); err != nil {
		t.Fatalf("幂等去重不得让 Flush 报错，收到 %v", err)
	}
	stats := queue.Stats()
	if stats.NotSettled != 1 || stats.Failed != 0 || stats.Rows != 0 {
		t.Fatalf("幂等去重应单独计数：%+v", stats)
	}
}

// Stop 是「不再收新的」，不是「丢掉已有的」：已入队的记录必须落库后才返回。
func TestAsyncQueueStopDrainsQueuedEntries(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}}
	queue := NewWriteQueue(AsyncOptions{MaxPending: 8, BatchSize: 100, FlushInterval: time.Hour})
	settler := New(writer, Options{
		MaxAttempts: 1,
		Backoff:     func(int) time.Duration { return 0 },
		Queue:       queue,
	})

	if _, err := settler.Settle(context.Background(), 9, okSettlement(nil)); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	// 批未满、时间未到：此时它一定还躺在队列里。
	if writer.unfinalizedCalls != 0 {
		t.Fatalf("未触发 flush 时不应已写库，实际写了 %d 次", writer.unfinalizedCalls)
	}
	queue.Stop()
	if writer.unfinalizedCalls != 1 {
		t.Fatalf("Stop 必须把已入队的记录写完，实际写了 %d 次", writer.unfinalizedCalls)
	}
	if queue.Pending() != 0 {
		t.Fatalf("Stop 之后不应再有积压：%d", queue.Pending())
	}
	// 停后再入队：一律降级同步写（不丢），不得阻塞。
	result, err := settler.Settle(context.Background(), 10, okSettlement(nil))
	if err != nil || result.Queued || !result.Committed {
		t.Fatalf("队列已停时必须降级同步写：result=%+v err=%v", result, err)
	}
}

// 零回归钉子：不带队列时结算仍是同步写——Settle 返回即已落库。
func TestSettleWithoutQueueStaysSynchronous(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}}
	settler := New(writer, Options{MaxAttempts: 1, Backoff: func(int) time.Duration { return 0 }})

	result, err := settler.Settle(context.Background(), 21, okSettlement(nil))
	if err != nil {
		t.Fatalf("同步结算失败: %v", err)
	}
	if result.Queued {
		t.Fatalf("未装配队列时不得报「已入队」：%+v", result)
	}
	if !result.Committed || writer.unfinalizedCalls != 1 {
		t.Fatalf("同步模式下 Settle 返回时终态必须已落库：result=%+v calls=%d", result, writer.unfinalizedCalls)
	}
}

// asyncAffinityRecorder 记录亲和写回顺序（本包内的最小替身，不碰 Redis）。
type asyncAffinityRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *asyncAffinityRecorder) RecordWinner(_ context.Context, _ int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, "affinity_put")
	return true
}

func (r *asyncAffinityRecorder) TombstoneOnFailure(_ context.Context, _ int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, "affinity_tombstone")
	return true
}

func (r *asyncAffinityRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// 亲和写回在异步模式下的时机：墓碑**不推迟**（与是否赢得终态无关），winner 必须等到 flush 之后。
func TestAsyncQueueDefersAffinityWinnerButNotTombstone(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}}
	queue, settler := newAsyncFixture(t, writer, AsyncOptions{
		MaxPending: 8, BatchSize: 8, FlushInterval: time.Hour,
	})
	pc := newTestContext(t)
	if err := pc.SetMessageRequestID(77); err != nil {
		t.Fatalf("写行标识失败: %v", err)
	}
	recorder := &asyncAffinityRecorder{}
	pc.SetAffinityWriteback(recorder)

	settlement := okSettlement(nil)
	settlement.Affinity = AffinityDirective{WinnerProviderID: 5, TombstoneProviderID: 6}
	result, err := settler.SettleContext(context.Background(), pc, settlement, nil)
	if err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	if !result.Queued {
		t.Fatalf("异步模式下终态应先入队：%+v", result)
	}
	if got := recorder.snapshot(); len(got) != 1 || got[0] != "affinity_tombstone" {
		t.Fatalf("墓碑应在入队后立即发出、winner 必须等提交，收到 %v", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := queue.Flush(ctx); err != nil {
		t.Fatalf("冲队列失败: %v", err)
	}
	got := recorder.snapshot()
	if len(got) != 2 || got[1] != "affinity_put" {
		t.Fatalf("winner 必须在终态落库之后发出，收到 %v", got)
	}
}

// ctxObservingWriteback 在 winner 写回时记下上下文的存活状态。
//
// 为什么必须记上下文：亲和写回在异步模式下由队列 flush 触发，而 flush 发生在请求返回**之后**
// ——net/http 那时已取消请求 ctx。用请求 ctx 去写 Redis 会静默失败（CAS 脚本报 context canceled，
// 写回返回 false），粘性绑定永不落库。
type ctxObservingWriteback struct {
	mu      sync.Mutex
	ctxErr  error
	written bool
}

func (w *ctxObservingWriteback) RecordWinner(ctx context.Context, _ int64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ctxErr = ctx.Err()
	w.written = true
	return true
}

func (w *ctxObservingWriteback) TombstoneOnFailure(context.Context, int64) bool { return false }

func (w *ctxObservingWriteback) result() (error, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ctxErr, w.written
}

// 请求返回后（ctx 已取消）flush，winner 写回仍须带着可用上下文发出。
func TestAsyncQueueAffinityWinnerOutlivesRequestContext(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: true}}}
	queue, settler := newAsyncFixture(t, writer, AsyncOptions{
		MaxPending: 8, BatchSize: 8, FlushInterval: time.Hour,
	})
	pc := newTestContext(t)
	if err := pc.SetMessageRequestID(99); err != nil {
		t.Fatalf("写行标识失败: %v", err)
	}
	recorder := &ctxObservingWriteback{}
	pc.SetAffinityWriteback(recorder)

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	settlement := okSettlement(nil)
	settlement.Affinity = AffinityDirective{WinnerProviderID: 5}
	if _, err := settler.SettleContext(requestCtx, pc, settlement, nil); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	// 请求返回：与 net/http 同形，请求 ctx 到此取消。
	cancelRequest()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := queue.Flush(ctx); err != nil {
		t.Fatalf("冲队列失败: %v", err)
	}
	ctxErr, written := recorder.result()
	if !written {
		t.Fatal("winner 写回未发出")
	}
	if ctxErr != nil {
		t.Fatalf("winner 写回不得携带已取消的请求上下文（真实 Redis CAS 会静默失败）：%v", ctxErr)
	}
}

// 未赢得终态时（重复投递、patrol 先补）异步路径不得发 winner 写回。
func TestAsyncQueueSkipsAffinityWinnerWhenNotCommitted(t *testing.T) {
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{committed: false}}}
	queue, settler := newAsyncFixture(t, writer, AsyncOptions{
		MaxPending: 8, BatchSize: 1, FlushInterval: time.Hour,
	})
	pc := newTestContext(t)
	if err := pc.SetMessageRequestID(88); err != nil {
		t.Fatalf("写行标识失败: %v", err)
	}
	recorder := &asyncAffinityRecorder{}
	pc.SetAffinityWriteback(recorder)

	settlement := okSettlement(nil)
	settlement.Affinity = AffinityDirective{WinnerProviderID: 5}
	if _, err := settler.SettleContext(context.Background(), pc, settlement, nil); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := queue.Flush(ctx); err != nil {
		t.Fatalf("冲队列失败: %v", err)
	}
	if got := recorder.snapshot(); len(got) != 0 {
		t.Fatalf("未赢得终态时不得写回 winner（粘性会指向本次没服务成功的供应商）：%v", got)
	}
}

// 迟到查询：条目写完就从 inflight 注销，此后查询仍必须拿到失败结论——否则「写入失败」会被
// 误报成「不在队列（= 已落库）」，退出序列据此认为排空完成，那些终态就随关池消失。
func TestAwaitSettlementReportsFailureAfterEntryRetired(t *testing.T) {
	writeErr := errors.New("writer lane down")
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{err: writeErr}}}
	queue, settler := newAsyncFixture(t, writer, AsyncOptions{
		MaxPending: 8, BatchSize: 1, FlushInterval: time.Hour,
	})

	if _, err := settler.Settle(context.Background(), 41, okSettlement(nil)); err != nil {
		t.Fatalf("入队失败: %v", err)
	}
	// Flush 等到 pending 归零：此时该条已写完并注销，之后的查询就是「迟到查询」。
	if err := queue.Flush(context.Background()); !errors.Is(err, ErrQueueWriteFailed) {
		t.Fatalf("有写入失败时 Flush 应报 ErrQueueWriteFailed，收到 %v", err)
	}
	queue.mu.Lock()
	_, stillInflight := queue.inflight[41]
	queue.mu.Unlock()
	if stillInflight {
		t.Fatal("前提不成立：条目仍在 inflight，本用例验不到注销后的查询")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := queue.AwaitSettlement(ctx, 41); !errors.Is(err, writeErr) {
		t.Fatalf("注销后的查询必须报出原失败，收到 %v", err)
	}
}

// 墓碑表严格有界：超出上界按入表顺序淘汰，淘汰后回到「不在队列」语义（这是有界的代价，
// 不是遗漏）。无界累积会把「罕见失败」变成内存增长。
func TestAwaitSettlementTombstonesAreBounded(t *testing.T) {
	writeErr := errors.New("writer lane down")
	writer := &fakeWriter{unfinalizedQueue: []unfinalizedResult{{err: writeErr}}}
	total := finishedTombstoneLimit + 64
	queue, settler := newAsyncFixture(t, writer, AsyncOptions{
		MaxPending: total + 8, BatchSize: 1, FlushInterval: time.Hour,
	})

	for id := int64(1); id <= int64(total); id++ {
		if _, err := settler.Settle(context.Background(), id, okSettlement(nil)); err != nil {
			t.Fatalf("入队 %d 失败: %v", id, err)
		}
	}
	if err := queue.Flush(context.Background()); !errors.Is(err, ErrQueueWriteFailed) {
		t.Fatalf("Flush 应报 ErrQueueWriteFailed，收到 %v", err)
	}
	queue.mu.Lock()
	size := len(queue.finished)
	queue.mu.Unlock()
	if size > finishedTombstoneLimit {
		t.Fatalf("墓碑表必须严格有界：%d > %d", size, finishedTombstoneLimit)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := queue.AwaitSettlement(ctx, 1); err != nil {
		t.Fatalf("最早那条已被淘汰，应回到「不在队列」语义（nil），收到 %v", err)
	}
	if err := queue.AwaitSettlement(ctx, int64(total)); !errors.Is(err, writeErr) {
		t.Fatalf("窗口内最近那条的失败必须报出，收到 %v", err)
	}
}
