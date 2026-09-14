package gate

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
)

// defaultGlobalPrebufferBytes 与 TS 侧 DEFAULT_STREAM_GATE_GLOBAL_PREBUFFER_BYTE_CAP 一致。
const defaultGlobalPrebufferBytes = 256 * 1024 * 1024

var (
	// ErrReservationNotPositive 表示预留量不是正整数（TS 侧 RangeError 的等价物）。
	ErrReservationNotPositive = errors.New("stream gate reservation must be a positive integer")
	// ErrReservationExceedsBudget 表示单次预留已超过全局预算上限。
	ErrReservationExceedsBudget = errors.New("stream gate reservation exceeds the global prebuffer budget")
)

// Budget 是流门控的进程级共享前缀预算（TS 的 StreamGatePrebufferBudget）。
//
// 每个门禁在读取上游前预留自身最坏缓冲量；预算不足时排队，让上游读取的背压接管，
// 而不是关闭门禁或把本地资源压力误报成供应商故障。
//
// Snapshot 与 Acquire 之间没有异步边界，故二者可组合判断「本次调用是否真会排队」。
// 所有方法并发安全。
type Budget struct {
	mu           sync.Mutex
	resolveLimit func() int
	reserved     int
	head         *waiter
	tail         *waiter
	waiting      int
}

// Snapshot 是预算瞬时状态。
type Snapshot struct {
	ReservedBytes int
	Waiting       int
	Limit         int
}

type waiter struct {
	reservedBytes int
	granted       chan *Lease
	prev          *waiter
	next          *waiter
	queued        bool
}

// NewBudget 用「当前上限」的读取函数构造预算。上限需为正整数。
func NewBudget(resolveLimit func() int) *Budget {
	return &Budget{resolveLimit: resolveLimit}
}

// DefaultBudget 返回进程级共享预算（TS 的 getStreamGatePrebufferBudget）。
func DefaultBudget() *Budget {
	defaultBudgetOnce.Do(func() {
		defaultBudget = NewBudget(ResolveGlobalPrebufferByteCap)
	})
	return defaultBudget
}

var (
	defaultBudgetOnce sync.Once
	defaultBudget     *Budget
)

// ResolveGlobalPrebufferByteCap 读取全局前缀预算上限（TS 的
// resolveStreamGateGlobalPrebufferByteCap）：配置不可用时回退到出厂默认值。
func ResolveGlobalPrebufferByteCap() int {
	env, err := config.LoadEnv(os.LookupEnv)
	if err != nil {
		return defaultGlobalPrebufferBytes
	}
	return env.StreamGateGlobalPrebufferByteCap
}

// Reserve 预留 reservedBytes 字节。预算不足时按 FIFO 排队；ctx 取消则退出队列。
//
// 返回的租约必须在用完后 Release；前缀被消费掉后应 ShrinkTo 到实际仍占用的字节数。
func (b *Budget) Reserve(ctx context.Context, reservedBytes int) (*Lease, error) {
	if reservedBytes <= 0 {
		return nil, ErrReservationNotPositive
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	b.mu.Lock()
	limit := b.resolveLimit()
	if limit <= 0 || reservedBytes > limit {
		b.mu.Unlock()
		return nil, ErrReservationExceedsBudget
	}
	if b.waiting == 0 && b.reserved+reservedBytes <= limit {
		lease := b.createLeaseLocked(reservedBytes)
		b.mu.Unlock()
		return lease, nil
	}
	pending := &waiter{reservedBytes: reservedBytes, granted: make(chan *Lease, 1)}
	b.enqueueWaiterLocked(pending)
	b.drainWaitersLocked()
	b.mu.Unlock()

	select {
	case lease := <-pending.granted:
		return lease, nil
	case <-ctx.Done():
		b.mu.Lock()
		removed := b.removeWaiterLocked(pending)
		if removed {
			b.drainWaitersLocked()
		}
		var orphan *Lease
		if !removed {
			// 已被 grant：归还预算，绝不因取消泄漏额度。
			select {
			case orphan = <-pending.granted:
			default:
			}
		}
		b.mu.Unlock()
		if orphan != nil {
			orphan.Release()
		}
		return nil, ctx.Err()
	}
}

// Snapshot 返回瞬时状态，供调用方判断本次预留是否会排队。
func (b *Budget) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Snapshot{
		ReservedBytes: b.reserved,
		Waiting:       b.waiting,
		Limit:         b.resolveLimit(),
	}
}

func (b *Budget) enqueueWaiterLocked(pending *waiter) {
	pending.queued = true
	pending.prev = b.tail
	if b.tail != nil {
		b.tail.next = pending
	} else {
		b.head = pending
	}
	b.tail = pending
	b.waiting++
}

func (b *Budget) removeWaiterLocked(pending *waiter) bool {
	if !pending.queued {
		return false
	}
	if pending.prev != nil {
		pending.prev.next = pending.next
	} else {
		b.head = pending.next
	}
	if pending.next != nil {
		pending.next.prev = pending.prev
	} else {
		b.tail = pending.prev
	}
	pending.prev = nil
	pending.next = nil
	pending.queued = false
	b.waiting--
	return true
}

func (b *Budget) createLeaseLocked(reservedBytes int) *Lease {
	b.reserved += reservedBytes
	return &Lease{budget: b, reservedBytes: reservedBytes}
}

// drainWaitersLocked 按 FIFO 发放额度；队首放不下即停（保持公平，不允许插队）。
func (b *Budget) drainWaitersLocked() {
	for b.head != nil {
		pending := b.head
		limit := b.resolveLimit()
		if b.reserved+pending.reservedBytes > limit {
			return
		}
		b.removeWaiterLocked(pending)
		pending.granted <- b.createLeaseLocked(pending.reservedBytes)
	}
}

// Lease 是一次前缀预留。Release 幂等；ShrinkTo 只能收窄，不能扩回。
type Lease struct {
	budget        *Budget
	reservedBytes int
	released      bool
}

// ReservedBytes 返回当前仍占用的预算字节数。
func (l *Lease) ReservedBytes() int {
	l.budget.mu.Lock()
	defer l.budget.mu.Unlock()
	return l.reservedBytes
}

// ShrinkTo 把租约收窄到实际仍被前缀占用的字节数（提交后调用）。
// 不小于当前值或已释放时为 no-op；负数返回错误（TS 侧 RangeError 的等价物）。
func (l *Lease) ShrinkTo(reservedBytes int) error {
	if reservedBytes < 0 {
		return fmt.Errorf("stream gate lease size must be a non-negative integer: %d", reservedBytes)
	}
	l.budget.mu.Lock()
	defer l.budget.mu.Unlock()
	if l.released || reservedBytes >= l.reservedBytes {
		return nil
	}
	l.budget.reserved -= l.reservedBytes - reservedBytes
	l.reservedBytes = reservedBytes
	l.budget.drainWaitersLocked()
	return nil
}

// Release 归还全部剩余额度；幂等。
func (l *Lease) Release() {
	l.budget.mu.Lock()
	defer l.budget.mu.Unlock()
	if l.released {
		return
	}
	l.released = true
	l.budget.reserved -= l.reservedBytes
	l.reservedBytes = 0
	l.budget.drainWaitersLocked()
}
