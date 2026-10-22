// Package ingress 负责入站请求体的读取、解压与进程级内存准入。
//
// 三条不变量（违反其一即为缺陷）：
//
//  1. **请求体永不落盘**。解压与缓存只发生在堆上；任何需要「留一份」的场景都必须显式申请
//     在途预算（见 Admission），预算不足时立即拒绝而不是排队。
//  2. **同一份字节不得同时以 []byte 与 string 两种形式长期持有**。解析 JSON 时用一次性转换并
//     在解析后立即释放原切片；不要为了「顺手」把正文转成 string 存进结构体。
//  3. **解压在鉴权之后执行**，且压缩体与解压体各有独立上限。这是相对 Node 的有意改进：
//
// Node 侧解压在鉴权 guard 之前，
// 会把未鉴权的输入展开到堆上。
//
// 本包只提供能力，不决定调用顺序：`PeekSize` 让调用方在读体之前按 content-length 与
// content-encoding 做结构性拒绝，`NewReader` 负责流式解压与双向限额，`Admission` 负责
// 进程级内存与在途字节的准入判定。
package ingress

import (
	"math"
	"os"
	"strconv"
	"strings"
)

// 出厂默认值，与 Node 侧 request-body-codec.ts 一一对应（便于行为对账）。
const (
	// DefaultMaxDecodedBytes 是解压输出硬上限（防御解压炸弹）。Node 侧同名默认 100 MiB。
	DefaultMaxDecodedBytes int64 = 100 * 1024 * 1024
	// DefaultMaxConcurrentDecompressions 是在途解压任务数上限，等于 libuv 默认线程池容量。
	DefaultMaxConcurrentDecompressions = 4
	// DefaultMaxInflightDecompressionBytes 是在途解压占用的压缩体字节上限。
	DefaultMaxInflightDecompressionBytes int64 = 128 * 1024 * 1024
	// DefaultMaxLayers 是 content-encoding 层数上限。真实客户端只发单层；多层只是攻击面。
	DefaultMaxLayers = 1
	// DefaultMaxInflightBodyBytes 是进程级在途请求体（含解压后明文）字节上限。
	// 这是 Go 侧新增的总量闸门（CCH_GO_MAX_INFLIGHT_BYTES），与 GOMEMLIMIT 协同工作。
	// 只是**未注入时的兜底**：生产唯一真源是 config.Config.MaxInflightBytes（由 dataplane
	// 装配处注入），默认值 96 MiB 与那一侧一致。
	//
	// 记账口径是「预算 × 放大系数 = 成本上限」：96 MiB × 6 = 576 MiB 成本上限。成本上限可高于
	// 容器内存上限，此时由内存余量闸门（GOMEMLIMIT × 0.9）先拦，不会因此 OOM。
	// 旧的 64 MiB 按正文字节记账会让 9 条并发大请求把堆推到 380 MB 以上。
	DefaultMaxInflightBodyBytes int64 = 96 * 1024 * 1024
	// DefaultInflightBytesMultiplier 是准入记账的放大系数默认值。
	//
	// 记账口径是「正文 × 系数」而不是正文本身：同一份正文在堆上同时以多种形态驻留
	// （解析树、改写副本、编码缓冲），实测堆峰值是正文的 3.0~6.2 倍，取 6 作上界估计。
	DefaultInflightBytesMultiplier = 6
)

// 环境变量名。解压三项与 Node 同名同默认，便于逐项对账；总量闸门是 Go 侧新增旋钮。
const (
	EnvMaxDecodedBytes              = "MAX_DECOMPRESSED_REQUEST_BYTES"
	EnvMaxCompressedBytes           = "MAX_COMPRESSED_REQUEST_BYTES"
	EnvMaxConcurrentDecompressions  = "MAX_CONCURRENT_REQUEST_DECOMPRESSIONS"
	EnvMaxInflightDecompressionByte = "MAX_INFLIGHT_REQUEST_DECOMPRESSION_BYTES"
	EnvMaxInflightBodyBytes         = "CCH_GO_MAX_INFLIGHT_BYTES"
	EnvInflightBytesMultiplier      = "CCH_GO_INFLIGHT_BYTES_MULTIPLIER"
)

// Options 是请求体读取与解压的限额配置。
//
// 零值不可用：用 DefaultOptions 构造，再按需覆盖字段。
type Options struct {
	// MaxDecodedBytes 是单请求解压后字节上限。<=0 时取 DefaultMaxDecodedBytes。
	MaxDecodedBytes int64
	// MaxCompressedBytes 是单请求压缩体字节上限。<=0 时取 MaxDecodedBytes（与 Node 的默认一致：
	// 真实压缩比下合法请求的压缩体不会超过其解压体，过紧的上限会误拒大上下文请求）。
	MaxCompressedBytes int64
	// MaxLayers 是 content-encoding 层数上限。<=0 时取 DefaultMaxLayers。
	MaxLayers int
	// Decompression 是在途解压准入限流器；为 nil 表示不做在途准入（仅供单测与基准使用）。
	Decompression *Limiter
}

// DefaultOptions 按环境变量解析配置：缺失或非法时回退出厂默认（与 Node 的 parseLimitEnv 一致，
// 不接受 "10MiB" 之类后缀）。同时构造与 Node 同参的在途解压限流器。
func DefaultOptions() Options {
	maxDecoded := parseBytesEnv(EnvMaxDecodedBytes, DefaultMaxDecodedBytes)
	maxCompressed := parseBytesEnv(EnvMaxCompressedBytes, maxDecoded)
	concurrent := int(parseBytesEnv(EnvMaxConcurrentDecompressions, DefaultMaxConcurrentDecompressions))
	inflightBytes := parseBytesEnv(
		EnvMaxInflightDecompressionByte,
		DefaultMaxInflightDecompressionBytes,
	)
	return Options{
		MaxDecodedBytes:    maxDecoded,
		MaxCompressedBytes: maxCompressed,
		MaxLayers:          DefaultMaxLayers,
		Decompression:      NewLimiter(concurrent, inflightBytes),
	}
}

// normalized 把零值补成默认值，使 Options 的零值语义与 Node 默认一致。
func (o Options) normalized() Options {
	if o.MaxDecodedBytes <= 0 {
		o.MaxDecodedBytes = DefaultMaxDecodedBytes
	}
	if o.MaxCompressedBytes <= 0 {
		o.MaxCompressedBytes = o.MaxDecodedBytes
	}
	if o.MaxLayers <= 0 {
		o.MaxLayers = DefaultMaxLayers
	}
	return o
}

// InflightBytesMultiplier 返回生效的放大系数（CCH_GO_INFLIGHT_BYTES_MULTIPLIER），
// 缺失或非正整数时取出厂默认。
//
// config 层对同一变量做 fail fast 校验（0/负/小数装载即失败）；这里的回退只服务于未接线的
// 调用方，与 parseBytesEnv 读 CCH_GO_MAX_INFLIGHT_BYTES 同策。
func InflightBytesMultiplier() int {
	raw := strings.TrimSpace(os.Getenv(EnvInflightBytesMultiplier))
	if raw == "" {
		return DefaultInflightBytesMultiplier
	}
	multiplier, err := strconv.Atoi(raw)
	if err != nil || multiplier < 1 {
		return DefaultInflightBytesMultiplier
	}
	return multiplier
}

// InflightCostBytes 把正文字节换算成准入记账成本——**唯一的**换算入口。
//
// multiplier <= 1 时恒等，即退化为「按正文字节记账」的旧口径。溢出饱和到 MaxInt64：成本只与
// 预算比较，饱和仍表达「远超预算」；回绕成负数会让闸门恒放行，是这里唯一不可接受的取值。
func InflightCostBytes(bodyBytes, multiplier int64) int64 {
	if bodyBytes <= 0 || multiplier <= 1 {
		return bodyBytes
	}
	if bodyBytes > math.MaxInt64/multiplier {
		return math.MaxInt64
	}
	return bodyBytes * multiplier
}

// parseBytesEnv 解析正整数字节数；非法或缺失时返回 fallback。
func parseBytesEnv(name string, fallback int64) int64 {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}
