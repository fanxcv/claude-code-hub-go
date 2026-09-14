package forward

import "testing"

// TestDetachedBudgetRefusesInsteadOfQueueing 断言预算不足时立即拒绝，不排队等待。
func TestDetachedBudgetRefusesInsteadOfQueueing(t *testing.T) {
	budget := NewDetachedStreamBudget(DetachedStreamLimits{
		MaxConcurrency:   2,
		MaxReservedBytes: 32 << 20,
	})
	first, refusal := budget.TryAcquire(DetachedKindMetering, 1<<20)
	if first == nil {
		t.Fatalf("首个租约应成功: %s", refusal)
	}
	second, refusal := budget.TryAcquire(DetachedKindMetering, 1<<20)
	if second == nil {
		t.Fatalf("第二个租约应成功: %s", refusal)
	}
	if _, refusal := budget.TryAcquire(DetachedKindMetering, 1<<20); refusal != DetachedRefusedConcurrency {
		t.Fatalf("超并发拒绝原因 = %q", refusal)
	}

	first.Release()
	if lease, refusal := budget.TryAcquire(DetachedKindMetering, 1<<20); lease == nil {
		t.Fatalf("释放后应能再取: %s", refusal)
	}
	second.Release()
}

// TestDetachedBudgetRefusesWhenBytesExhausted 断言字节预算独立于并发上限生效。
func TestDetachedBudgetRefusesWhenBytesExhausted(t *testing.T) {
	budget := NewDetachedStreamBudget(DetachedStreamLimits{
		MaxConcurrency:      16,
		MaxReservedBytes:    4 << 20,
		MeteringReserveByte: 1 << 20,
	})
	first, refusal := budget.TryAcquire(DetachedKindMetering, 3<<20)
	if first == nil {
		t.Fatalf("首个租约应成功: %s", refusal)
	}
	// 4 MiB - 3 MiB = 1 MiB 余量，但其中 1 MiB 留给计量预留，第二个计量租约仍能占用预留。
	if lease, refusal := budget.TryAcquire(DetachedKindMetering, 1<<20); lease == nil {
		t.Fatalf("计量租约可用自己的预留: %s", refusal)
	}
	if _, refusal := budget.TryAcquire(DetachedKindMetering, 1<<20); refusal != DetachedRefusedMemory {
		t.Fatalf("超字节预算拒绝原因 = %q", refusal)
	}
	first.Release()
}

// TestDetachedBudgetProtectsMeteringReserve 断言非计量租约不得吃掉计量预留。
func TestDetachedBudgetProtectsMeteringReserve(t *testing.T) {
	budget := NewDetachedStreamBudget(DetachedStreamLimits{
		MaxConcurrency:      16,
		MaxReservedBytes:    8 << 20,
		MeteringReserveByte: 2 << 20,
	})
	// 非计量租约最多用到 6 MiB。
	lease, refusal := budget.TryAcquire(DetachedKindReplay, 5<<20)
	if lease == nil {
		t.Fatalf("5 MiB 回放租约应成功: %s", refusal)
	}
	if _, refusal := budget.TryAcquire(DetachedKindReplay, 2<<20); refusal != DetachedRefusedMeteringReserve {
		t.Fatalf("侵入计量预留的拒绝原因 = %q", refusal)
	}
	// 计量类租约仍可用预留。
	metering, refusal := budget.TryAcquire(DetachedKindMetering, 2<<20)
	if metering == nil {
		t.Fatalf("计量租约应可用预留: %s", refusal)
	}
	lease.Release()
	metering.Release()
	if snapshot := budget.Snapshot(); snapshot.ActiveStreams != 0 || snapshot.ReservedBytes != 0 {
		t.Fatalf("释放后快照 = %+v", snapshot)
	}
}

// TestDetachedLeaseReleaseIsIdempotent 断言重复释放不会把配额算成负数。
func TestDetachedLeaseReleaseIsIdempotent(t *testing.T) {
	budget := NewDetachedStreamBudget(DetachedStreamLimits{})
	lease, refusal := budget.TryAcquire(DetachedKindMetering, 1<<20)
	if lease == nil {
		t.Fatalf("租约应成功: %s", refusal)
	}
	lease.Release()
	lease.Release()
	snapshot := budget.Snapshot()
	if snapshot.ActiveStreams != 0 || snapshot.ReservedBytes != 0 {
		t.Fatalf("重复释放后快照 = %+v", snapshot)
	}
}

// TestDetachedBudgetClampsOversizedReserve 断言预留配置不会吞掉整个预算。
func TestDetachedBudgetClampsOversizedReserve(t *testing.T) {
	limits := DetachedStreamLimits{
		MaxConcurrency:      4,
		MaxReservedBytes:    8 << 20,
		MeteringReserveByte: 64 << 20,
	}.WithDefaults()
	if limits.MeteringReserveByte >= limits.MaxReservedBytes {
		t.Fatalf("预留未被夹取: %+v", limits)
	}
	budget := NewDetachedStreamBudget(limits)
	if lease, refusal := budget.TryAcquire(DetachedKindReplay, 1<<20); lease == nil {
		t.Fatalf("非计量租约不应被预留饿死: %s", refusal)
	}
}

// TestDetachedBudgetDefaults 断言零值配置落到与 Node 一致的默认量级。
func TestDetachedBudgetDefaults(t *testing.T) {
	limits := DetachedStreamLimits{}.WithDefaults()
	if limits.MaxConcurrency != DefaultDetachedStreamMaxConcurrency {
		t.Fatalf("MaxConcurrency = %d", limits.MaxConcurrency)
	}
	if limits.MaxReservedBytes != DefaultDetachedStreamBudgetBytes {
		t.Fatalf("MaxReservedBytes = %d", limits.MaxReservedBytes)
	}
	if limits.MeteringReserveByte != DefaultDetachedStreamMeteringReserveBytes {
		t.Fatalf("MeteringReserveByte = %d", limits.MeteringReserveByte)
	}
}

// TestDetachedBudgetDisabledWithoutInstance 断言未配置预算时引流一律不可用（而非无限放行）。
func TestDetachedBudgetDisabledWithoutInstance(t *testing.T) {
	var budget *DetachedStreamBudget
	lease, refusal := budget.TryAcquire(DetachedKindMetering, 1<<20)
	if lease != nil {
		t.Fatal("nil 预算不应发出租约")
	}
	if refusal != DetachedRefusedDisabled {
		t.Fatalf("拒绝原因 = %q", refusal)
	}
}
