package ingress

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// TestLimiterConcurrentNeverExceedsBudget 是并发准入的核心断言：64 个 goroutine 同时申请，
// 字节预算只够 10 个，则放行数必须恰为 10，且在途字节任何时刻不超过预算。
//
// 刻意不用 sleep 排序：TryAcquire 在互斥锁内完成「判定 + 记账」，因此「全部 goroutine 都尝试过」
// 之后即可确定性地断言，不需要等待。
func TestLimiterConcurrentNeverExceedsBudget(t *testing.T) {
	const (
		goroutines  = 64
		perLease    = 100
		maxBytes    = 1000
		wantGranted = maxBytes / perLease
	)
	limiter := NewLimiter(0, maxBytes)

	var granted atomic.Int64
	var overBudget atomic.Int64
	var maxObservedBytes atomic.Int64

	start := make(chan struct{})
	release := make(chan struct{})
	var attempted sync.WaitGroup
	var holding sync.WaitGroup

	attempted.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			<-start
			lease, err := limiter.TryAcquire(perLease)
			if err == nil {
				granted.Add(1)
				holding.Add(1)
				if observed := limiter.Stats().ActiveBytes; observed > maxBytes {
					overBudget.Add(1)
				}
				attempted.Done()
				<-release
				lease.Release()
				holding.Done()
				return
			}
			attempted.Done()
		}()
	}

	close(start)
	attempted.Wait() // 所有 goroutine 都已尝试完毕

	if got := granted.Load(); got != wantGranted {
		t.Fatalf("放行 %d 个，期望 %d 个", got, wantGranted)
	}
	if got := overBudget.Load(); got != 0 {
		t.Fatalf("有 %d 次观察到在途字节超过预算", got)
	}
	stats := limiter.Stats()
	if stats.ActiveBytes > maxBytes {
		t.Fatalf("在途字节 %d 超过预算 %d", stats.ActiveBytes, maxBytes)
	}
	if stats.Active != wantGranted {
		t.Fatalf("在途条数 %d，期望 %d", stats.Active, wantGranted)
	}
	if stats.PeakBytes > maxBytes {
		t.Fatalf("峰值 %d 超过预算 %d", stats.PeakBytes, maxBytes)
	}
	if stats.Rejected != goroutines-wantGranted {
		t.Fatalf("拒绝计数 %d，期望 %d", stats.Rejected, goroutines-wantGranted)
	}
	maxObservedBytes.Store(stats.PeakBytes)

	close(release)
	holding.Wait()
	if stats := limiter.Stats(); stats.Active != 0 || stats.ActiveBytes != 0 {
		t.Fatalf("释放后未归零: %+v", stats)
	}
}

// TestLimiterConcurrencyCap 覆盖并发条数上限：第三个申请必须立即失败（不排队）。
func TestLimiterConcurrencyCap(t *testing.T) {
	limiter := NewLimiter(2, 0)
	first, err := limiter.TryAcquire(1)
	if err != nil {
		t.Fatalf("第 1 个申请失败: %v", err)
	}
	second, err := limiter.TryAcquire(1)
	if err != nil {
		t.Fatalf("第 2 个申请失败: %v", err)
	}
	if _, err := limiter.TryAcquire(1); !errors.Is(err, ErrDecompressionBusy) {
		t.Fatalf("第 3 个申请错误 = %v，期望 ErrDecompressionBusy", err)
	}
	first.Release()
	third, err := limiter.TryAcquire(1)
	if err != nil {
		t.Fatalf("释放后应可再次申请: %v", err)
	}
	third.Release()
	second.Release()
	if stats := limiter.Stats(); stats.Active != 0 {
		t.Fatalf("在途条数未归零: %d", stats.Active)
	}
}

// TestLeaseGrowRespectsBudget 覆盖流式读取时的增量申请：额度耗尽即失败，且原额度不受影响。
func TestLeaseGrowRespectsBudget(t *testing.T) {
	limiter := NewLimiter(0, 1000)
	lease, err := limiter.TryAcquire(0)
	if err != nil {
		t.Fatalf("申请失败: %v", err)
	}
	if err := lease.Grow(600); err != nil {
		t.Fatalf("600 字节追加失败: %v", err)
	}
	if got := lease.Bytes(); got != 600 {
		t.Fatalf("占位 = %d，期望 600", got)
	}
	if err := lease.Grow(500); !errors.Is(err, ErrDecompressionBusy) {
		t.Fatalf("越界追加错误 = %v，期望 ErrDecompressionBusy", err)
	}
	if got := lease.Bytes(); got != 600 {
		t.Fatalf("失败的追加改变了占位：%d", got)
	}
	if got := limiter.Stats().ActiveBytes; got != 600 {
		t.Fatalf("限流器记账 = %d，期望 600", got)
	}
	lease.Release()
	if got := limiter.Stats().ActiveBytes; got != 0 {
		t.Fatalf("释放后记账 = %d，期望 0", got)
	}
}

// TestLeaseReleaseIdempotent 覆盖重复释放：幂等，不得把计数减成负数。
func TestLeaseReleaseIdempotent(t *testing.T) {
	limiter := NewLimiter(1, 100)
	lease, err := limiter.TryAcquire(50)
	if err != nil {
		t.Fatalf("申请失败: %v", err)
	}
	lease.Release()
	lease.Release()
	stats := limiter.Stats()
	if stats.Active != 0 || stats.ActiveBytes != 0 {
		t.Fatalf("重复释放后账目错乱: %+v", stats)
	}
	if err := lease.Grow(1); err == nil {
		t.Fatal("已释放的占位不应能继续扩额")
	}
}

// TestLimiterZeroLimitsMeanUnbounded 覆盖「上限为 0 表示该维度不设限」的语义。
func TestLimiterZeroLimitsMeanUnbounded(t *testing.T) {
	limiter := NewLimiter(0, 0)
	leases := make([]*Lease, 0, 4)
	for i := 0; i < 4; i++ {
		lease, err := limiter.TryAcquire(1 << 20)
		if err != nil {
			t.Fatalf("不设限时第 %d 个申请失败: %v", i, err)
		}
		leases = append(leases, lease)
	}
	for _, lease := range leases {
		lease.Release()
	}
}
