package terminal

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// 本文件是终态写入的**异步批量队列**（MESSAGE_REQUEST_WRITE_MODE=async）。
//
// 动机：终态结算长在响应尾链上，占掉两笔 writer 道往返（终态 UPDATE 含缓存回退推导 SELECT，
// 以及成本 UPDATE），而这两笔写的结果只被「终态提交之后发放的副作用」消费。把它们交给单
// writer 的后台队列后，响应尾链不再等这两笔写。
//
// 三条硬约束（每条都有对应测试）：
//
//  1. **不丢数据**：队列有界（MESSAGE_REQUEST_ASYNC_MAX_PENDING），满时**降级为同步写**
//     并计数，绝不静默丢弃——保数据优先于保延迟。
//  2. **副作用不早于终态提交**：入队只带「怎么写」；副作用（rollup、新行信号、亲和 winner、
//     租约结算）由 worker 在写入返回 committed 之后发，与同步路径同一闸门。
//  3. **默认不开**：MESSAGE_REQUEST_WRITE_MODE 默认 sync，此时队列根本没有实例，结算走
//     原路径（零回归）。
//
// 与设计稿的两处刻意取舍：
//   - 单 writer **逐条**发 UPDATE（不跨行长事务）：writer 分道固定 1 条连接，并发 writer 只会
//     自相排队；逐条（非长事务）让迟到补写这类同步 writer 道调用可以交错，不会被饿死。
//   - 不合并同 id 的多条写：同一行的多次写入分属不同阶段（终态 / 成本 / 迟到补写）且列不重叠，
//     合并收益低、正确性风险高。批的语义因此是**节奏控制**（攒够 batchSize 或到 flush 间隔
//     就写一批），而不是减少往返。

// 出厂默认与 env 契约的区间一致（MAX_PENDING [100,200000]、BATCH_SIZE [1,2000]、
// FLUSH_INTERVAL_MS [10,60000]）。
const (
	DefaultAsyncMaxPending    = 20000
	DefaultAsyncBatchSize     = 128
	DefaultAsyncFlushInterval = 200 * time.Millisecond
)

// asyncWriteTimeout 是 worker 单条写入的墙钟上限。
//
// 后台协程没有请求的 deadline，没有它，一条卡住的语句会永久占住队列（并且让 Flush 永远等不到
// pending 归零，退出序列被拖住）。
const asyncWriteTimeout = 30 * time.Second

// finishedTombstoneLimit 是「已完成条目」墓碑表的上界。
//
// 为什么需要墓碑：worker 写完一条就把它从 inflight 里注销，而等待方（流终态屏障、退出序列）
// 可能在注销**之后**才来查——不记结论就会把「写入失败」误报成「不在队列」，而后者按语义
// 意味着「已落库」。窗口有界：只记失败（成功不入表，因为「已落库」与「不在队列」对等待方
// 是同一个 nil 结论），超出上界按入表顺序淘汰，淘汰后退回「不在队列」语义。
//
// 取 512 的依据：等待方在条目完成后的极短窗口内查询，而失败是罕见事件，正常运行时表几乎为空；
// 上界存在的唯一目的是让内存**严格有界**。
const finishedTombstoneLimit = 512

// AsyncOptions 是异步队列的构造参数；零值取出厂默认。
type AsyncOptions struct {
	// MaxPending 是队列容量上限（条）。<=0 取 DefaultAsyncMaxPending。
	MaxPending int
	// BatchSize 是单批上限。<=0 取 DefaultAsyncBatchSize。
	BatchSize int
	// FlushInterval 是「攒批」的最长等待（从上一批结束算起）。<=0 取 DefaultAsyncFlushInterval。
	FlushInterval time.Duration
	// Logger 供队列记痕（满队列降级、写入失败）。nil 时静默。
	Logger Logger
}

// QueueStats 是队列的计数快照（设计稿第 6 节的可观测性口径）。
type QueueStats struct {
	// Pending 是已入队但尚未落库的条数（含正在写的那条）。
	Pending int64
	// Enqueued 是累计入队条数。
	Enqueued int64
	// Rows 是累计**成功落库**的条数。写入失败与幂等去重（NotSettled）不计入它——
	// 把失败的也算进去会让这个数在故障时比实际落库量虚高。
	Rows int64
	// Batches 是累计批数。
	Batches int64
	// Degraded 是满队列（或已停）改走同步写的次数。它不为 0 说明入队速率超过了 flush 速率。
	Degraded int64
	// Failed 是写入报错的条数（含 CostGap 那一类）。
	Failed int64
	// NotSettled 是「未赢得该行」的幂等结论条数（重复投递、patrol 先补了终态）：
	// 既不是失败，也不是本队列写下的行。
	NotSettled int64
	// CostGap 是「终态已提交、成本未落库」的次数：这些行的 cost_usd 为 NULL，
	// 收入会少计。设计稿要求把它显式暴露出来，不掩盖。
	CostGap int64
}

// ErrQueueWriteFailed 表示队列里有**终态未落库**的条目（写入报错）。
//
// 退出序列靠它判定「能否安全关依赖」：终态没进库而关掉连接池，那些行会永久留在未终态
// （只能由 patrol 按年龄兜底，且成本无从补回）。
var ErrQueueWriteFailed = errors.New("terminal: 异步终态写队列中有写入失败")

// DebugLogger 是可选的调试级日志面：每次 flush 都要留一条（行数、耗时、积压），而 flush
// 默认每 200ms 一次——用 warn 级会淹没日志。terminal.Logger 仍只要求 Warn，故这里用可选断言
// （与 RollupFactsReader 同一手法）。
type DebugLogger interface {
	Debug(event string, fields map[string]any)
}

// asyncEntry 是一次待落库的终态结算。
//
// write 由构造它的 Settler 提供（终态 + 成本 + 副作用的完整执行体），队列本身不认识结算语义
// ——这样同一个队列可以被两个 Settler 实例共用（拦截路径与主路径各一个），而每条走的仍是
// 自己那份接线。
type asyncEntry struct {
	// id 是 message_request 行标识，同时是等待面的键。
	id int64
	// write 执行这次写入（含提交后的副作用）。
	write func(ctx context.Context) (Result, error)
	// afterCommit 是调用方附加的「提交后动作」（当前是亲和 winner 写回）；异步模式下它由
	// worker 在本条写完之后调用，因为提交结论只有在写完后才知道。
	afterCommit func(Result)
	// ctx 是入队时从请求上下文派生的**不可取消**上下文：请求在响应之后随时可能断开，
	// 而这一笔写入仍需完成。
	ctx context.Context
	// done 在本条落库（或失败）后关闭，供 AwaitSettlement 等待。
	done chan struct{}
	// failure 是本条写入的**真实失败**（幂等结论 ErrNotSettled 不算失败）；nil 即成功。
	//
	// 只由 worker 在 process 里写、只由 AwaitSettlement 在 done 关闭后读，
	// 可见性由 channel close 保证，故不需额外同步。
	failure error
}

// WriteQueue 是单 writer 的终态写入队列。并发安全。
type WriteQueue struct {
	capacity      int
	batchSize     int
	flushInterval time.Duration
	logger        Logger

	entries chan *asyncEntry
	// nudge 请求 worker 立即 flush（容量 1，重复请求会被合并）。
	nudge chan struct{}
	stop  chan struct{}
	done  chan struct{}

	stopOnce sync.Once

	mu       sync.Mutex
	stopped  bool
	pending  int64
	idle     chan struct{} // pending 归零时关闭；0→1 转换时重建
	inflight map[int64]*asyncEntry
	// finished 是已完成条目的**短期墓碑**：键是有行 id 的条目，值是该条的写入失败。
	// 只记失败：成功对等待方就是 nil，而 nil 也是「不在队列」的结论，无需入表。
	finished map[int64]error
	// finishedOrder 是墓碑的入表顺序（FIFO 淘汰用），与 finished 同锁维护。
	finishedOrder []int64

	enqueued      atomic.Int64
	rows          atomic.Int64
	batches       atomic.Int64
	degraded      atomic.Int64
	failed        atomic.Int64
	notSettled    atomic.Int64
	costGap       atomic.Int64
	degradeLogged atomic.Bool
	// pendingFailures 是「自上次 Flush 以来写入失败的条数」：Flush 读走并清零，
	// 故退出序列那一次 Flush 必然看到这之前发生的全部失败。
	pendingFailures atomic.Int64
}

// NewWriteQueue 构造并启动异步终态写队列。
//
// **只应在 MESSAGE_REQUEST_WRITE_MODE=async 时调用**：sync（默认）模式下不该有队列实例，
// 结算走逐字保留的原路径。
func NewWriteQueue(options AsyncOptions) *WriteQueue {
	capacity := options.MaxPending
	if capacity <= 0 {
		capacity = DefaultAsyncMaxPending
	}
	batchSize := options.BatchSize
	if batchSize <= 0 {
		batchSize = DefaultAsyncBatchSize
	}
	interval := options.FlushInterval
	if interval <= 0 {
		interval = DefaultAsyncFlushInterval
	}
	queue := &WriteQueue{
		capacity:      capacity,
		batchSize:     batchSize,
		flushInterval: interval,
		logger:        options.Logger,
		entries:       make(chan *asyncEntry, capacity),
		nudge:         make(chan struct{}, 1),
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
		inflight:      map[int64]*asyncEntry{},
		finished:      map[int64]error{},
	}
	queue.idle = make(chan struct{})
	close(queue.idle) // 初始即空闲
	go queue.run()
	return queue
}

// enqueue 尝试入队。返回 false 表示队列已满或已关停——调用方**必须**退回同步写（绝不丢弃）。
//
// 「停止判定 + 容量判定 + 登记 pending + 投递」在同一把锁内完成，与 Stop 互斥。分开做会留下
// 一个真实的窗口：检查过 stopped 之后、投递之前 Stop 跑完（worker 退出），这条既不在队列里
// 也永不被写——pending 永不归零，AwaitSettlement 会一直等到超时，随后关连接池就丢了这条终态。
// 锁内发送不会阻塞：容量刚刚判过，且 worker 只读通道、不抢这把锁。
func (q *WriteQueue) enqueue(
	ctx context.Context,
	id int64,
	write func(context.Context) (Result, error),
	afterCommit func(Result),
) bool {
	if q == nil {
		return false
	}
	entry := &asyncEntry{
		id:          id,
		write:       write,
		afterCommit: afterCommit,
		ctx:         context.WithoutCancel(ctx),
		done:        make(chan struct{}),
	}
	q.mu.Lock()
	if q.stopped {
		q.mu.Unlock()
		return false
	}
	if len(q.entries) >= q.capacity {
		q.mu.Unlock()
		// 满队列：交回调用方同步写。计数并留一次痕（不是每条都记：持续过载时逐条 warn 只会
		// 淹没日志，而计数与 Debug 级的 flush 行仍如实带出 degraded）。
		q.degraded.Add(1)
		q.logDegrade()
		return false
	}
	q.addPendingLocked(entry)
	q.entries <- entry
	q.mu.Unlock()
	q.enqueued.Add(1)
	return true
}

// AwaitSettlement 等某一行真正落库。三种结论：
//
//   - nil：已落库，或本就不在队列里（已落库 / 从未入队——同步写模式即此形态）；
//   - 写入失败的错误：该行**没有**落库，调用方不得当成排空完成；
//   - ctx 的错误：等待超时，是否落库未知。
//
// 退出序列与流终态屏障都靠它：异步模式下「已入队」不等于「已落库」，而关连接池会把尚未
// 发出的终态 UPDATE 一并带走。
//
// 迟到的查询（条目已写完并从 inflight 注销）必须仍能拿到失败结论：结论在 finished 墓碑里
// 保留一个**有界**窗口（见 finishedTombstoneLimit）。窗口之外回到「不在队列」语义——这是
// 有界的代价，不是遗漏；窗口取得足够大，正常时序下等待方不会落在它之外。
func (q *WriteQueue) AwaitSettlement(ctx context.Context, id int64) error {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	entry, ok := q.inflight[id]
	failure := q.finished[id]
	q.mu.Unlock()
	if !ok {
		return failure
	}
	select {
	case <-entry.done:
		return entry.failure
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Pending 返回已入队但尚未落库的条数。
func (q *WriteQueue) Pending() int64 {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.pending
}

// PendingSettlements 是 Pending 的别名，供退出序列把队列积压并入「待落库数」。
//
// 流路径的积压已经由结算跟踪器盖住（它的屏障等到 flush），但**非流路径**（拦截类终态等）
// 入队后没有跟踪器，不并进来的话退出日志会报 0，而队列里其实还躺着没写的终态。
func (q *WriteQueue) PendingSettlements() int64 { return q.Pending() }

// FlushSettlements / StopSettlements 是数据面退出接缝（dataplane.SettlementFlusher）要求的
// 方法名。生产装配把本类型**裸装**进 dataplane.SettlementBarrier，而退出序列按这两个名字
// 做类型断言——名字对不上时断言恒失败，关池前的冲队列会静默变成 no-op。
//
// 2026-09-20 生产事故正是如此：当时队列只有 Flush/Stop，`SettlementFlusher` 断言恒假，
// 退出时那批已入队的终态再没被写出去，账本行永久留在 status_code IS NULL，而进程
// 依然以成功退出（连 warn 都没有）。这两个方法与 Flush/Stop 是同一份实现，不允许各写一份。
// 接缝由编译期断言钉住：见 dataplane/settlement_seam_conformance_test.go。
func (q *WriteQueue) FlushSettlements(ctx context.Context) error { return q.Flush(ctx) }
func (q *WriteQueue) StopSettlements()                           { q.Stop() }

// Flush 请求立刻写入并等到队列清空；ctx 先结束返回其错误（未完成数由 Pending 如实反映）。
//
// 排空但**其中有写入失败**时返回 ErrQueueWriteFailed（带失败计数）：清空的是队列，不是
// 「所有终态都已落库」这个事实。失败计数是「自上次 Flush 以来」的（读走即清零），
// 故退出序列那次 Flush 必然看到它之前发生的全部失败。
//
// 循环而不是等一次：排空窗口里仍在收尾的请求会继续入队，只等一次会漏掉它们。
func (q *WriteQueue) Flush(ctx context.Context) error {
	if q == nil {
		return nil
	}
	for {
		q.mu.Lock()
		pending := q.pending
		idle := q.idle
		q.mu.Unlock()
		if pending == 0 {
			// 失败在 process 里先于 finishEntry 记账，故 pending 归零时它已可见。
			if failed := q.pendingFailures.Swap(0); failed > 0 {
				return fmt.Errorf("%w: %d 条终态未落库", ErrQueueWriteFailed, failed)
			}
			return nil
		}
		select {
		case q.nudge <- struct{}{}:
		default:
		}
		select {
		case <-idle:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Stop 停止 worker：此后的入队一律走同步降级，已入队的记录写完再返回。
//
// 调用方应先 Flush：Stop 的收尾只覆盖恰好还在批里或通道里的那些。它等待 worker 收尾，
// 而每条写入都有 asyncWriteTimeout 上界，故等待是有界的。
//
// 与 enqueue 互斥（同一把锁）：不会出现「入队已登记、worker 却已退出」的窗口。
func (q *WriteQueue) Stop() {
	if q == nil {
		return
	}
	q.stopOnce.Do(func() {
		q.mu.Lock()
		q.stopped = true
		q.mu.Unlock()
		close(q.stop)
		<-q.done
	})
}

// Stats 返回计数快照。
func (q *WriteQueue) Stats() QueueStats {
	if q == nil {
		return QueueStats{}
	}
	return QueueStats{
		Pending:    q.Pending(),
		Enqueued:   q.enqueued.Load(),
		Rows:       q.rows.Load(),
		Batches:    q.batches.Load(),
		Degraded:   q.degraded.Load(),
		Failed:     q.failed.Load(),
		NotSettled: q.notSettled.Load(),
		CostGap:    q.costGap.Load(),
	}
}

// run 是单 writer 主循环：攒批 → flush；触发条件先到者胜（条数到 batchSize 或到 flush 间隔）。
func (q *WriteQueue) run() {
	defer close(q.done)
	batch := make([]*asyncEntry, 0, q.batchSize)
	timer := time.NewTimer(q.flushInterval)
	defer timer.Stop()
	for {
		select {
		case <-q.stop:
			// 停之前把通道里剩的与手上这批写完：Stop 是「不再收新的」，不是「丢掉已有的」。
			q.drainInto(&batch)
			q.flush(batch)
			return
		case entry := <-q.entries:
			batch = append(batch, entry)
			if len(batch) >= q.batchSize {
				q.drainInto(&batch)
				q.flush(batch)
				batch = batch[:0]
				resetAsyncTimer(timer, q.flushInterval)
			}
		case <-q.nudge:
			// 必须先把通道里已经积下的取完再 flush：nudge 可能先于条目被消费，
			// 只冲手上这批会漏掉刚入队的那几条，Flush 就要等到下一次触发才可能返回。
			q.drainInto(&batch)
			q.flush(batch)
			batch = batch[:0]
			resetAsyncTimer(timer, q.flushInterval)
		case <-timer.C:
			q.drainInto(&batch)
			q.flush(batch)
			batch = batch[:0]
			resetAsyncTimer(timer, q.flushInterval)
		}
	}
}

// drainInto 把通道里已经积下的条目一次性取出，供 Stop 的收尾使用。
func (q *WriteQueue) drainInto(batch *[]*asyncEntry) {
	for {
		select {
		case entry := <-q.entries:
			*batch = append(*batch, entry)
		default:
			return
		}
	}
}

// flush 逐条写入一批；每条写完后发放它自己的提交后动作并放行等待者。
func (q *WriteQueue) flush(batch []*asyncEntry) {
	if len(batch) == 0 {
		return
	}
	started := time.Now()
	for _, entry := range batch {
		q.process(entry)
	}
	q.batches.Add(1)
	if logger, ok := q.logger.(DebugLogger); ok {
		stats := q.Stats()
		logger.Debug("terminal_async_flush", map[string]any{
			"rows":        len(batch),
			"batches":     stats.Batches,
			"duration_ms": time.Since(started).Milliseconds(),
			"pending":     stats.Pending,
			"degraded":    stats.Degraded,
			"failed":      stats.Failed,
		})
	}
}

// process 写入一条。顺序不可换：先终态、后成本、再副作用——与同步路径同一套闸门。
//
// 三种结论分别计数：真落库（Rows）、幂等去重（NotSettled）、写入失败（Failed +
// pendingFailures，后者供 Flush 与退出序列判「能否关依赖」）。
func (q *WriteQueue) process(entry *asyncEntry) {
	ctx, cancel := context.WithTimeout(entry.ctx, asyncWriteTimeout)
	result, err := entry.write(ctx)
	cancel()
	switch {
	case err == nil:
		q.rows.Add(1)
	case errors.Is(err, ErrNotSettled):
		// 幂等结论而不是写入失败：重复投递、patrol 先补终态都会走到这里，
		// 计成 failed 会把「正常去重」报成故障，计成 rows 又会虚报落库量。
		q.notSettled.Add(1)
	default:
		entry.failure = err
		q.failed.Add(1)
		q.pendingFailures.Add(1)
		costGap := errors.Is(err, ErrCostWriteFailed)
		if costGap {
			q.costGap.Add(1)
		}
		q.warn("terminal_async_flush_failed", map[string]any{
			"messageRequestId": entry.id,
			"cost_gap":         costGap,
			"error":            err.Error(),
		})
	}
	if entry.afterCommit != nil {
		entry.afterCommit(result)
	}
	q.finishEntry(entry)
}

// addPendingLocked 登记一条待落库记录；**前提是调用方已持 q.mu**。
func (q *WriteQueue) addPendingLocked(entry *asyncEntry) {
	if q.pending == 0 {
		q.idle = make(chan struct{})
	}
	q.pending++
	if entry.id > 0 {
		q.inflight[entry.id] = entry
	}
}

// finishEntry 放行等待者并注销登记；失败结论转入墓碑，供迟到查询取回。
func (q *WriteQueue) finishEntry(entry *asyncEntry) {
	close(entry.done)
	q.mu.Lock()
	defer q.mu.Unlock()
	if entry.id > 0 && q.inflight[entry.id] == entry {
		delete(q.inflight, entry.id)
		if entry.failure != nil {
			q.rememberFailureLocked(entry.id, entry.failure)
		}
	}
	q.pending--
	if q.pending == 0 {
		close(q.idle)
	}
}

// rememberFailureLocked 把一条失败结论记入墓碑，并按上界 FIFO 淘汰；**前提是调用方已持 q.mu**。
//
// 同 id 重复入表时只刷新值、不重复占位（否则淘汰顺序会与实际入表顺序脱节）。
func (q *WriteQueue) rememberFailureLocked(id int64, failure error) {
	if _, exists := q.finished[id]; !exists {
		q.finishedOrder = append(q.finishedOrder, id)
	}
	q.finished[id] = failure
	for len(q.finishedOrder) > finishedTombstoneLimit {
		oldest := q.finishedOrder[0]
		q.finishedOrder = q.finishedOrder[1:]
		delete(q.finished, oldest)
	}
}

// logDegrade 对「满队列降级」只记一次 warn：持续过载时逐条记只会淹没日志，
// 而计数与 Debug 级的 flush 行仍如实带出。
func (q *WriteQueue) logDegrade() {
	if q.logger == nil || !q.degradeLogged.CompareAndSwap(false, true) {
		return
	}
	q.logger.Warn("terminal_async_queue_full", map[string]any{
		"maxPending": q.capacity,
		"hint":       "队列已满，本条改走同步写（不丢数据，只回退延迟）；持续出现说明写入追不上到达率",
	})
}

// warn 在未装配日志时静默。
func (q *WriteQueue) warn(event string, fields map[string]any) {
	if q.logger == nil {
		return
	}
	q.logger.Warn(event, fields)
}

// resetAsyncTimer 复位计时器，且不吞掉已到期的信号（Go 计时器的经典坑：Stop 后必须排空）。
func resetAsyncTimer(timer *time.Timer, d time.Duration) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(d)
}
