package ingress

import (
	"math"
	"runtime/debug"
	"runtime/metrics"
	"sync"
)

// MemoryPressure 报告「堆在用字节 + 进程内存上限」。
//
// limitKnown=false 表示上限不可知（未设 GOMEMLIMIT），此时判定一律放行——不知道就不假装知道，
// 由 GOMEMLIMIT 自身兜底（超限时 Go runtime 会通过加大 GC 压力处理，进程仍可能 OOM 退出）。
type MemoryPressure func() (inUseBytes uint64, limitBytes uint64, limitKnown bool)

// heapObjectsMetric 是堆上存活对象字节数的采点。相对 runtime.ReadMemStats 无 STW 开销。
const heapObjectsMetric = "/memory/classes/heap/objects:bytes"

var (
	heapSampleOnce sync.Once
	heapSamples    []metrics.Sample
)

// DefaultMemoryPressure 读取真实的堆在用字节与 GOMEMLIMIT。
//
// GOMEMLIMIT 的读法：debug.SetMemoryLimit(-1) 传负值时不改变设置、只返回当前值——这是官方
// 指定的读取方式，不去猜环境变量（runtime 会在未设置时使用 50% 物理内存的软上限，猜环境变量
// 会算错）。
func DefaultMemoryPressure() (uint64, uint64, bool) {
	heapSampleOnce.Do(func() {
		heapSamples = []metrics.Sample{{Name: heapObjectsMetric}}
	})
	metrics.Read(heapSamples)
	var inUse uint64
	if heapSamples[0].Value.Kind() == metrics.KindUint64 {
		inUse = heapSamples[0].Value.Uint64()
	}
	limit := debug.SetMemoryLimit(-1)
	if limit <= 0 || limit == math.MaxInt64 {
		return inUse, 0, false
	}
	return inUse, uint64(limit), true
}

// MemoryGuard 在进程内存逼近 GOMEMLIMIT 时拒绝新的大体请求。
//
// 它不做任何 runtime 黑魔法：只读两个数、比一次大小，判定失败即返回 ErrInsufficientMemory，
// 由调用方按 503 拒绝。headroomRatio 是允许动用的上限比例（默认 0.9），留出的余量给
// GC 峰值与不可控的临时分配。
type MemoryGuard struct {
	pressure MemoryPressure
	ratio    float64
}

// NewMemoryGuard 构造判定钩子。pressure 为 nil 时使用 DefaultMemoryPressure；
// ratio <= 0 或 > 1 时取 0.9。
func NewMemoryGuard(pressure MemoryPressure, ratio float64) *MemoryGuard {
	if pressure == nil {
		pressure = DefaultMemoryPressure
	}
	// NaN 与任何值比较都为假，故写成「非合法区间」而不是「<=0 或 >1」。
	if !(ratio > 0 && ratio <= 1) {
		ratio = 0.9
	}
	return &MemoryGuard{pressure: pressure, ratio: ratio}
}

// Allow 判定「再申请 bytes 字节」是否仍在预算内。内存上限不可知时一律放行。
func (g *MemoryGuard) Allow(bytes int64) error {
	if bytes <= 0 {
		return nil
	}
	inUse, limit, known := g.pressure()
	if !known {
		return nil
	}
	budget := uint64(float64(limit) * g.ratio)
	need := uint64(bytes)
	if inUse > budget || need > budget-inUse {
		return wrap(
			ErrInsufficientMemory,
			"堆在用 %d + 申请 %d 超过可用预算 %d（GOMEMLIMIT %d，比例 %.2f）",
			inUse,
			need,
			budget,
			limit,
			g.ratio,
		)
	}
	return nil
}

// Headroom 返回当前可动用的剩余字节数；上限不可知时 ok=false。
func (g *MemoryGuard) Headroom() (free int64, ok bool) {
	inUse, limit, known := g.pressure()
	if !known {
		return 0, false
	}
	budget := uint64(float64(limit) * g.ratio)
	if inUse >= budget {
		return 0, true
	}
	remaining := budget - inUse
	if remaining > math.MaxInt64 {
		return math.MaxInt64, true
	}
	return int64(remaining), true
}
