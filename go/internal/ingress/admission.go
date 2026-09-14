package ingress

import "errors"

// AdmissionOptions 是进程级准入配置。零值字段按环境变量与出厂默认补齐。
type AdmissionOptions struct {
	// MaxInflightBodyBytes 是在途请求体（含解压后明文）字节上限；<=0 时取
	// CCH_GO_MAX_INFLIGHT_BYTES 或 DefaultMaxInflightBodyBytes。
	MaxInflightBodyBytes int64
	// MaxConcurrentDecompressions 是在途解压并发上限；<=0 时取环境变量或默认值。
	MaxConcurrentDecompressions int
	// MaxInflightDecompressionBytes 是在途解压占用的压缩体字节上限；<=0 时取环境变量或默认值。
	MaxInflightDecompressionBytes int64
	// MemoryRatio 是允许动用的 GOMEMLIMIT 比例；<=0 时取 0.9。
	MemoryRatio float64
	// Pressure 覆盖内存读数来源（测试注入）；nil 时用 DefaultMemoryPressure。
	Pressure MemoryPressure
}

// AdmissionStats 是准入层的可观测读数，供 /readyz 与日志使用。
type AdmissionStats struct {
	Body          Stats
	Decompression Stats
	MemoryFree    int64
	MemoryKnown   bool
}

// Admission 组合三层准入：进程内存（与 GOMEMLIMIT 协同）、在途请求体字节、在途解压。
//
// 拒绝一律是**立即失败**而不是排队：排队会让峰值内存事后才出现，反而放大 OOM 风险。
// 三层是叠加关系而不是替代关系——内存判定回答「现在还有没有预算」，两个限流器回答
// 「本进程允许同时在途多少」，最终以更严的一侧为准。
type Admission struct {
	Body          *Limiter
	Decompression *Limiter
	Memory        *MemoryGuard
}

// NewAdmission 构造准入层。未显式给出的上限按环境变量与出厂默认补齐。
func NewAdmission(opts AdmissionOptions) *Admission {
	maxBody := opts.MaxInflightBodyBytes
	if maxBody <= 0 {
		maxBody = parseBytesEnv(EnvMaxInflightBodyBytes, DefaultMaxInflightBodyBytes)
	}
	concurrent := opts.MaxConcurrentDecompressions
	if concurrent <= 0 {
		concurrent = int(parseBytesEnv(EnvMaxConcurrentDecompressions, DefaultMaxConcurrentDecompressions))
	}
	inflight := opts.MaxInflightDecompressionBytes
	if inflight <= 0 {
		inflight = parseBytesEnv(
			EnvMaxInflightDecompressionByte,
			DefaultMaxInflightDecompressionBytes,
		)
	}
	return &Admission{
		Body:          NewLimiter(0, maxBody), // 请求体不按条数限流：总量由字节预算表达
		Decompression: NewLimiter(concurrent, inflight),
		Memory:        NewMemoryGuard(opts.Pressure, opts.MemoryRatio),
	}
}

// DefaultAdmission 按环境变量构造准入层。
func DefaultAdmission() *Admission { return NewAdmission(AdmissionOptions{}) }

// AcquireBody 为「即将读入内存的请求体」申请预算，字节数应为**解压后**的明文长度。
//
// 判定顺序：先内存（GOMEMLIMIT 协同），再全局在途字节。前者回答「进程还有没有余量」，
// 后者回答「本进程允许同时在途多少」。任一不足即立即拒绝：
// ErrInsufficientMemory 或 ErrBodyBudgetExhausted（两者都映射 503）。
func (a *Admission) AcquireBody(decodedBytes int64) (*Lease, error) {
	if err := a.Memory.Allow(decodedBytes); err != nil {
		return nil, err
	}
	lease, err := a.Body.TryAcquire(decodedBytes)
	if err != nil {
		return nil, wrap(ErrBodyBudgetExhausted, "在途请求体预算不足（申请 %d 字节）", decodedBytes)
	}
	return lease, nil
}

// AcquireDecompression 为一次解压申请在途预算（一个名额 + 压缩体字节）。语义与
// NewReader 内部的一致，供调用方在自建解压管道时复用同一份全局额度。
func (a *Admission) AcquireDecompression(compressedBytes int64) (*Lease, error) {
	return a.Decompression.TryAcquire(compressedBytes)
}

// Stats 返回三层读数快照。
func (a *Admission) Stats() AdmissionStats {
	free, known := a.Memory.Headroom()
	return AdmissionStats{
		Body:          a.Body.Stats(),
		Decompression: a.Decompression.Stats(),
		MemoryFree:    free,
		MemoryKnown:   known,
	}
}

// IsCapacityError 报告错误是否属于「服务端瞬时容量不足」（503 语义）。
// 调用方据此决定是返回 503 还是走其它错误分支。
func IsCapacityError(err error) bool {
	return errors.Is(err, ErrInsufficientMemory) ||
		errors.Is(err, ErrBodyBudgetExhausted) ||
		errors.Is(err, ErrDecompressionBusy)
}
