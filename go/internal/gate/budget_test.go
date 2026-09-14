package gate

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBudgetReserveAndRelease(t *testing.T) {
	budget := NewBudget(func() int { return 100 })
	ctx := context.Background()

	if _, err := budget.Reserve(ctx, 40); err != nil {
		t.Fatalf("立即预留应成功: %v", err)
	}
	if snapshot := budget.Snapshot(); snapshot.ReservedBytes != 40 || snapshot.Limit != 100 {
		t.Fatalf("快照不符: %+v", snapshot)
	}

	lease, err := budget.Reserve(ctx, 60)
	if err != nil {
		t.Fatalf("正好用尽预算应成功: %v", err)
	}
	lease.Release()
	lease.Release() // 幂等
	if snapshot := budget.Snapshot(); snapshot.ReservedBytes != 40 {
		t.Fatalf("释放后应剩 40，得到 %d", snapshot.ReservedBytes)
	}
}

func TestBudgetRejectsInvalidReservation(t *testing.T) {
	budget := NewBudget(func() int { return 100 })
	ctx := context.Background()

	if _, err := budget.Reserve(ctx, 0); !errors.Is(err, ErrReservationNotPositive) {
		t.Fatalf("零预留应报 ErrReservationNotPositive，得到 %v", err)
	}
	if _, err := budget.Reserve(ctx, -1); !errors.Is(err, ErrReservationNotPositive) {
		t.Fatalf("负预留应报 ErrReservationNotPositive，得到 %v", err)
	}
	if _, err := budget.Reserve(ctx, 101); !errors.Is(err, ErrReservationExceedsBudget) {
		t.Fatalf("超出全局预算应报 ErrReservationExceedsBudget，得到 %v", err)
	}
	zeroLimit := NewBudget(func() int { return 0 })
	if _, err := zeroLimit.Reserve(ctx, 1); !errors.Is(err, ErrReservationExceedsBudget) {
		t.Fatalf("上限非正时应报 ErrReservationExceedsBudget，得到 %v", err)
	}
}

func TestBudgetShrinkFreesBudget(t *testing.T) {
	budget := NewBudget(func() int { return 100 })
	ctx := context.Background()

	one, err := budget.Reserve(ctx, 90)
	if err != nil {
		t.Fatalf("预留失败: %v", err)
	}
	if err := one.ShrinkTo(-1); err == nil {
		t.Fatal("负数收窄应报错")
	}
	if err := one.ShrinkTo(95); err != nil {
		t.Fatalf("扩大应静默无操作: %v", err)
	}
	if got := one.ReservedBytes(); got != 90 {
		t.Fatalf("扩大后仍应为 90，得到 %d", got)
	}
	if err := one.ShrinkTo(30); err != nil {
		t.Fatalf("收窄失败: %v", err)
	}
	if got := one.ReservedBytes(); got != 30 {
		t.Fatalf("收窄后应为 30，得到 %d", got)
	}

	// 收窄应唤醒等待者：30 + 20 <= 100。
	other, err := budget.Reserve(ctx, 20)
	if err != nil {
		t.Fatalf("收窄后应能继续预留: %v", err)
	}
	other.Release()
	one.Release()
	if snapshot := budget.Snapshot(); snapshot.ReservedBytes != 0 {
		t.Fatalf("全部释放后应为 0，得到 %d", snapshot.ReservedBytes)
	}
}

func TestBudgetFIFOWithoutJumpAhead(t *testing.T) {
	budget := NewBudget(func() int { return 100 })
	ctx := context.Background()

	held, err := budget.Reserve(ctx, 80)
	if err != nil {
		t.Fatalf("预留失败: %v", err)
	}

	first := reserveAsync(budget, ctx, 90)
	waitForWaiting(t, budget, 1)
	// 队首放不下 90，则新请求不得插队（TS 的 waitingCount === 0 条件）。
	second := reserveAsync(budget, ctx, 20)
	waitForWaiting(t, budget, 2)

	select {
	case <-first.done:
		t.Fatal("预算不足时不应放行")
	case <-second.done:
		t.Fatal("新请求不得插队")
	case <-time.After(30 * time.Millisecond):
	}

	held.Release()
	select {
	case lease := <-first.done:
		if lease == nil {
			t.Fatalf("队首应被放行: %v", first.err)
		}
		defer lease.Release()
	case <-time.After(time.Second):
		t.Fatal("队首超时未放行")
	}
	if snapshot := budget.Snapshot(); snapshot.Waiting != 1 {
		t.Fatalf("第二个请求应仍在排队，得到 %+v", snapshot)
	}
	select {
	case <-second.done:
		t.Fatal("队首占用 90 时第二个请求不应放行")
	default:
	}
}

func TestBudgetContextCancel(t *testing.T) {
	budget := NewBudget(func() int { return 100 })
	held, err := budget.Reserve(context.Background(), 90)
	if err != nil {
		t.Fatalf("预留失败: %v", err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	waiter := reserveAsync(budget, canceled, 20)
	waitForWaiting(t, budget, 1)
	cancel()
	select {
	case lease := <-waiter.done:
		lease.Release()
		t.Fatalf("取消后不应拿到租约")
	case <-waiter.errCh:
		if !errors.Is(waiter.err, context.Canceled) {
			t.Fatalf("应为 context.Canceled，得到 %v", waiter.err)
		}
	case <-time.After(time.Second):
		t.Fatal("取消后应立刻退出队列")
	}
	if snapshot := budget.Snapshot(); snapshot.Waiting != 0 {
		t.Fatalf("取消者应已出队，得到 %+v", snapshot)
	}

	preCanceled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if _, err := budget.Reserve(preCanceled, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消的 ctx 应直接报错，得到 %v", err)
	}

	held.Release()
	if snapshot := budget.Snapshot(); snapshot.ReservedBytes != 0 {
		t.Fatalf("取消后不得泄漏额度，得到 %+v", snapshot)
	}
}

func TestBudgetConcurrencyAdmission(t *testing.T) {
	const (
		limit    = 8
		workers  = 64
		batchCap = limit
	)
	budget := NewBudget(func() int { return limit })

	var inFlight int64
	var peak int64
	var overAdmission int64

	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			lease, err := budget.Reserve(ctx, 1)
			if err != nil {
				t.Errorf("预留失败: %v", err)
				return
			}
			current := atomic.AddInt64(&inFlight, 1)
			for {
				observed := atomic.LoadInt64(&peak)
				if current <= observed || atomic.CompareAndSwapInt64(&peak, observed, current) {
					break
				}
			}
			if current > batchCap {
				atomic.AddInt64(&overAdmission, 1)
			}
			time.Sleep(time.Millisecond)
			atomic.AddInt64(&inFlight, -1)
			lease.Release()
		}()
	}
	wg.Wait()

	if overAdmission > 0 {
		t.Fatalf("并发准入越限 %d 次", overAdmission)
	}
	if peak > batchCap {
		t.Fatalf("峰值在途 %d 超过上限 %d", peak, batchCap)
	}
	if snapshot := budget.Snapshot(); snapshot.ReservedBytes != 0 || snapshot.Waiting != 0 {
		t.Fatalf("结束后应清零，得到 %+v", snapshot)
	}
}

func TestResolveGlobalPrebufferByteCap(t *testing.T) {
	t.Setenv("STREAM_GATE_GLOBAL_PREBUFFER_BYTE_CAP", "41943040")
	if got := ResolveGlobalPrebufferByteCap(); got != 41943040 {
		t.Fatalf("应读到环境变量，得到 %d", got)
	}
	// 与 STREAM_GATE_PREBUFFER_BYTE_CAP（4 倍约束）冲突时回退出厂默认值。
	t.Setenv("STREAM_GATE_GLOBAL_PREBUFFER_BYTE_CAP", "2048")
	if got := ResolveGlobalPrebufferByteCap(); got != defaultGlobalPrebufferBytes {
		t.Fatalf("配置非法时应回退默认值，得到 %d", got)
	}
}

func TestDefaultBudgetIsSingleton(t *testing.T) {
	// 两次调用分开取值再比较：直接写 `DefaultBudget() != DefaultBudget()` 会被 staticcheck
	// 判为 SA4000（左右两侧是同一表达式），而这里想验证的恰恰是「两次调用指向同一实例」——
	// 分开取值既保住该断言，也不再触发误报。
	first := DefaultBudget()
	second := DefaultBudget()
	if first != second {
		t.Fatal("进程级预算应为单例")
	}
	if DefaultBudget().Snapshot().Limit <= 0 {
		t.Fatal("默认预算上限应为正")
	}
}

type asyncReserve struct {
	done  chan *Lease
	errCh chan struct{}
	err   error
}

func reserveAsync(budget *Budget, ctx context.Context, reservedBytes int) *asyncReserve {
	result := &asyncReserve{done: make(chan *Lease, 1), errCh: make(chan struct{}, 1)}
	go func() {
		lease, err := budget.Reserve(ctx, reservedBytes)
		if err != nil {
			result.err = err
			close(result.errCh)
			return
		}
		result.done <- lease
	}()
	return result
}

func waitForWaiting(t *testing.T, budget *Budget, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if budget.Snapshot().Waiting == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("等待排队数达到 %d 超时，当前 %+v", want, budget.Snapshot())
}
