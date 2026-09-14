package forward

import (
	"sync"
	"time"
)

// 本文件实现客户端断线后的计量引流预算，对应 Node 的 detached-stream-budget.ts。
//
// 语义要点：
//   - 引流是「用配额换账务准确度」：客户端已经走了，我们继续读上游只为拿到终态 usage。
//   - 预算不足时**不排队**：立刻放弃引流并按已观测到的部分结算。排队会把断线风暴
//     变成长尾内存占用，而断线本就是常态。
//   - 权重是字节预留量：并发数上限之外还要限总量，否则单流预留很大时并发上限失去意义。

const (
	// DefaultDetachedStreamMaxConcurrency 对齐 DETACHED_STREAM_MAX_CONCURRENCY。
	DefaultDetachedStreamMaxConcurrency = 64
	// DefaultDetachedStreamBudgetBytes 对齐 DETACHED_STREAM_BUDGET_BYTES。
	DefaultDetachedStreamBudgetBytes int64 = 64 << 20
	// DefaultDetachedStreamMeteringReserveBytes 对齐 DETACHED_STREAM_METERING_RESERVE_BYTES：
	// 留给计量类租约的专用配额，避免回放等旁路把计量挤掉。
	DefaultDetachedStreamMeteringReserveBytes int64 = 16 << 20
	// DefaultDetachedStreamDrainTimeout 对齐 CLIENT_ABORT_DRAIN_MAX_MS：
	// 单次引流的绝对上限，超过即放弃引流。
	DefaultDetachedStreamDrainTimeout = 60 * time.Second
	// DetachedStreamFixedOverheadBytes 对齐 CLIENT_ABORT_DRAIN_FIXED_OVERHEAD_BYTES。
	DetachedStreamFixedOverheadBytes int64 = 3 << 20
)

// DetachedStreamKind 是引流租约的类别。
type DetachedStreamKind string

const (
	// DetachedKindMetering 是断线计量引流。
	DetachedKindMetering DetachedStreamKind = "metering"
	// DetachedKindReplay 是回放引流（属后续波次，此处只保留预算口径）。
	DetachedKindReplay DetachedStreamKind = "replay"
	// DetachedKindLoser 是竞速输家引流（属后续波次，此处只保留预算口径）。
	DetachedKindLoser DetachedStreamKind = "loser"
)

// DetachedStreamLimits 是引流预算上限。
type DetachedStreamLimits struct {
	MaxConcurrency      int
	MaxReservedBytes    int64
	MeteringReserveByte int64
}

// WithDefaults 补齐零值。
func (l DetachedStreamLimits) WithDefaults() DetachedStreamLimits {
	if l.MaxConcurrency <= 0 {
		l.MaxConcurrency = DefaultDetachedStreamMaxConcurrency
	}
	if l.MaxReservedBytes <= 0 {
		l.MaxReservedBytes = DefaultDetachedStreamBudgetBytes
	}
	if l.MeteringReserveByte <= 0 {
		l.MeteringReserveByte = DefaultDetachedStreamMeteringReserveBytes
	}
	// 预留不得吞掉全部预算：否则任何非计量类租约永远拿不到配额。
	if l.MeteringReserveByte >= l.MaxReservedBytes {
		l.MeteringReserveByte = l.MaxReservedBytes / 4
	}
	return l
}

// DetachedStreamRefusal 是租约被拒的原因。
type DetachedStreamRefusal string

const (
	// DetachedRefusedConcurrency 表示并发数已满。
	DetachedRefusedConcurrency DetachedStreamRefusal = "concurrency_exhausted"
	// DetachedRefusedMemory 表示字节预算已满。
	DetachedRefusedMemory DetachedStreamRefusal = "memory_budget_exhausted"
	// DetachedRefusedMeteringReserve 表示只剩计量预留，非计量租约不得占用。
	DetachedRefusedMeteringReserve DetachedStreamRefusal = "metering_reserve"
	// DetachedRefusedDisabled 表示预算未配置（nil 预算），引流一律不可用。
	DetachedRefusedDisabled DetachedStreamRefusal = "budget_disabled"
)

// DetachedStreamLease 是一次引流配额租约；Release 幂等且并发安全。
type DetachedStreamLease struct {
	kind          DetachedStreamKind
	reservedBytes int64
	once          sync.Once
	release       func()
}

// Kind 返回租约类别。
func (l *DetachedStreamLease) Kind() DetachedStreamKind { return l.kind }

// ReservedBytes 返回该租约占用的字节预留。
func (l *DetachedStreamLease) ReservedBytes() int64 { return l.reservedBytes }

// Release 归还配额；重复调用无副作用。
func (l *DetachedStreamLease) Release() {
	if l == nil {
		return
	}
	l.once.Do(l.release)
}

// DetachedStreamBudget 是进程级引流预算。并发安全。
type DetachedStreamBudget struct {
	mu        sync.Mutex
	limits    DetachedStreamLimits
	streams   int
	reserved  int64
	byKind    map[DetachedStreamKind]int
	kindBytes map[DetachedStreamKind]int64
}

// NewDetachedStreamBudget 构造预算；limits 的零值取默认。
func NewDetachedStreamBudget(limits DetachedStreamLimits) *DetachedStreamBudget {
	return &DetachedStreamBudget{
		limits:    limits.WithDefaults(),
		byKind:    make(map[DetachedStreamKind]int, 3),
		kindBytes: make(map[DetachedStreamKind]int64, 3),
	}
}

// TryAcquire 尝试获取引流配额；不足时立即返回拒绝原因，不排队。
func (b *DetachedStreamBudget) TryAcquire(
	kind DetachedStreamKind,
	reservedBytes int64,
) (*DetachedStreamLease, DetachedStreamRefusal) {
	if b == nil {
		return nil, DetachedRefusedDisabled
	}
	if reservedBytes <= 0 {
		reservedBytes = DetachedStreamFixedOverheadBytes
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.streams >= b.limits.MaxConcurrency {
		return nil, DetachedRefusedConcurrency
	}
	next := b.reserved + reservedBytes
	if next > b.limits.MaxReservedBytes {
		return nil, DetachedRefusedMemory
	}
	if kind != DetachedKindMetering &&
		next > b.limits.MaxReservedBytes-b.limits.MeteringReserveByte {
		return nil, DetachedRefusedMeteringReserve
	}

	b.streams++
	b.reserved = next
	b.byKind[kind]++
	b.kindBytes[kind] += reservedBytes

	return &DetachedStreamLease{
		kind:          kind,
		reservedBytes: reservedBytes,
		release: func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.streams--
			b.reserved -= reservedBytes
			b.byKind[kind]--
			b.kindBytes[kind] -= reservedBytes
		},
	}, ""
}

// Snapshot 返回当前占用（诊断与测试用）。
type DetachedStreamSnapshot struct {
	ActiveStreams int
	ReservedBytes int64
	Limits        DetachedStreamLimits
}

// Snapshot 返回当前占用快照。
func (b *DetachedStreamBudget) Snapshot() DetachedStreamSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	return DetachedStreamSnapshot{
		ActiveStreams: b.streams,
		ReservedBytes: b.reserved,
		Limits:        b.limits,
	}
}
