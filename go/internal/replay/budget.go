package replay

import "sync/atomic"

// 全局并发 spool 预算：与 Node 的 activeSpoolCount / REPLAY_MAX_CONCURRENT_SPOOLS 对应。
// 超限时创建方直接放弃回放（fail-open 不排队），由调用方把 owner 租约一并释放。

var spoolBudget atomic.Int64

// ActiveSpoolCount 返回本进程当前存活的 spool 数。
func ActiveSpoolCount() int64 {
	return spoolBudget.Load()
}

// acquireSpool 尝试占一个 spool 名额；达到上限返回 false。
func acquireSpool(cap int64) bool {
	for {
		current := spoolBudget.Load()
		if current >= cap {
			return false
		}
		if spoolBudget.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

// releaseSpool 归还名额，幂等（不归到负）。
func releaseSpool() {
	for {
		current := spoolBudget.Load()
		if current <= 0 {
			return
		}
		if spoolBudget.CompareAndSwap(current, current-1) {
			return
		}
	}
}

// durablePersistenceGate 串行化「终态正文重建 + PG 写入」。容量为 1，等价于 Node 的
// serializeDurablePersistence 全局单链：并发收尾时同一时刻只有一个 N MiB 级正文被重建到
// 堆上，不会把峰值放大成 N 倍正文。
//
// 等待不设超时（与 Node 的 promise 链一致）：持门操作自身带调用方 ctx，最终必然释放。
var durablePersistenceGate = make(chan struct{}, 1)

func withDurablePersistence[T any](operation func() (T, error)) (T, error) {
	durablePersistenceGate <- struct{}{}
	defer func() { <-durablePersistenceGate }()
	return operation()
}
