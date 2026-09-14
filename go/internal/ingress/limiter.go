package ingress

import "sync"

// Stats 是限流器的可观测读数（供 /readyz 与日志使用）。
type Stats struct {
	MaxConcurrent int
	MaxBytes      int64
	Active        int
	ActiveBytes   int64
	PeakBytes     int64
	Rejected      uint64
}

// Limiter 是「并发数 + 字节数」双重记账的在途准入闸门。
//
// 刻意不做排队：TryAcquire 同步失败即由调用方立即拒绝。排队会让峰值内存事后才出现，
// 正是本闸门要消灭的东西。零值不可用，用 NewLimiter 构造；两个上限都允许为 0 表示该维度不设限。
type Limiter struct {
	mu     sync.Mutex
	maxCon int
	maxByt int64

	active     int
	activeByte int64
	peakByte   int64
	rejected   uint64
}

// NewLimiter 构造限流器。maxConcurrent <= 0 表示不限并发数，maxBytes <= 0 表示不限字节数。
func NewLimiter(maxConcurrent int, maxBytes int64) *Limiter {
	return &Limiter{maxCon: maxConcurrent, maxByt: maxBytes}
}

// TryAcquire 原子地申请「一个名额 + bytes 字节」。并发数或字节预算任一越界即立即返回
// ErrDecompressionBusy（或 ErrBodyBudgetExhausted，由调用方在语义层选择错误类型）——
// 本函数只返回 ErrDecompressionBusy，调用方可用 wrap 换成面向用户的错误。
func (l *Limiter) TryAcquire(bytes int64) (*Lease, error) {
	if bytes < 0 {
		bytes = 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.maxCon > 0 && l.active >= l.maxCon {
		l.rejected++
		return nil, wrap(ErrDecompressionBusy, "并发数已达上限 %d", l.maxCon)
	}
	if l.maxByt > 0 && l.activeByte+bytes > l.maxByt {
		l.rejected++
		return nil, wrap(
			ErrDecompressionBusy,
			"在途字节 %d + 申请 %d 超过预算 %d",
			l.activeByte,
			bytes,
			l.maxByt,
		)
	}
	l.active++
	l.activeByte += bytes
	if l.activeByte > l.peakByte {
		l.peakByte = l.activeByte
	}
	return &Lease{limiter: l, bytes: bytes}, nil
}

// Stats 返回当前读数快照。
func (l *Limiter) Stats() Stats {
	l.mu.Lock()
	defer l.mu.Unlock()
	return Stats{
		MaxConcurrent: l.maxCon,
		MaxBytes:      l.maxByt,
		Active:        l.active,
		ActiveBytes:   l.activeByte,
		PeakBytes:     l.peakByte,
		Rejected:      l.rejected,
	}
}

// Lease 是一次准入占位。释放前必须调用 Release（defer 即可）；Release 幂等。
type Lease struct {
	limiter *Limiter

	mu       sync.Mutex
	bytes    int64
	released bool
}

// Bytes 返回当前已占字节数。
func (le *Lease) Bytes() int64 {
	le.mu.Lock()
	defer le.mu.Unlock()
	return le.bytes
}

// Grow 增量申请 extra 字节（流式读取时压缩体实际大小可能事先未知）。失败返回错误，
// **原有额度保持不变**，调用方应立即结束该请求并 Release。
func (le *Lease) Grow(extra int64) error {
	if extra <= 0 {
		return nil
	}
	le.mu.Lock()
	defer le.mu.Unlock()
	if le.released {
		return wrap(ErrDecompressionBusy, "占位已释放，不能再扩额")
	}
	l := le.limiter
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.maxByt > 0 && l.activeByte+extra > l.maxByt {
		l.rejected++
		return wrap(
			ErrDecompressionBusy,
			"在途字节 %d + 追加 %d 超过预算 %d",
			l.activeByte,
			extra,
			l.maxByt,
		)
	}
	l.activeByte += extra
	if l.activeByte > l.peakByte {
		l.peakByte = l.activeByte
	}
	le.bytes += extra
	return nil
}

// Release 归还全部占位（名额与字节）。重复调用无副作用。
func (le *Lease) Release() {
	le.mu.Lock()
	defer le.mu.Unlock()
	if le.released {
		return
	}
	le.released = true
	l := le.limiter
	l.mu.Lock()
	defer l.mu.Unlock()
	l.active--
	l.activeByte -= le.bytes
	if l.activeByte < 0 {
		l.activeByte = 0
	}
	le.bytes = 0
}
