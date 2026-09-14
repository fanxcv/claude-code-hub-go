package ingress

import (
	"errors"
	"fmt"
)

// 可判别错误。调用方用 errors.Is 判定，用 StatusOf 映射 HTTP 状态码。
var (
	// ErrTooManyLayers 表示 content-encoding 层数超过上限（400）。多层编码只会放大 CPU 与峰值内存。
	ErrTooManyLayers = errors.New("ingress: content-encoding 层数超过上限")
	// ErrCompressedTooLarge 表示压缩体本身超过上限（413）。
	ErrCompressedTooLarge = errors.New("ingress: 压缩请求体超过上限")
	// ErrDecodedTooLarge 表示解压输出超过上限（413）。
	ErrDecodedTooLarge = errors.New("ingress: 解压后请求体超过上限")
	// ErrCorruptBody 表示解压流损坏（400）。
	ErrCorruptBody = errors.New("ingress: 请求体解压失败")
	// ErrDecompressionBusy 表示在途解压预算饱和（503）。刻意不排队：排队会让峰值内存事后才出现。
	ErrDecompressionBusy = errors.New("ingress: 在途解压预算已满")
	// ErrBodyBudgetExhausted 表示进程级在途请求体预算已满（503）。
	ErrBodyBudgetExhausted = errors.New("ingress: 在途请求体预算已满")
	// ErrInsufficientMemory 表示进程内存逼近 GOMEMLIMIT（503）。拒绝而非 OOM 退出。
	ErrInsufficientMemory = errors.New("ingress: 进程内存逼近上限")
)

// HTTP 状态码语义。503 是「服务端瞬时容量」拒绝：既非体积错误（413），也非客户端限速（429）。
const (
	StatusBadRequest         = 400
	StatusRequestEntityLarge = 413
	StatusUnavailable        = 503
)

// StatusOf 把本包的可判别错误映射为 HTTP 状态码；未识别的错误（含 nil）返回 0，由调用方决定。
func StatusOf(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrTooManyLayers), errors.Is(err, ErrCorruptBody):
		return StatusBadRequest
	case errors.Is(err, ErrCompressedTooLarge), errors.Is(err, ErrDecodedTooLarge):
		return StatusRequestEntityLarge
	case errors.Is(err, ErrDecompressionBusy),
		errors.Is(err, ErrBodyBudgetExhausted),
		errors.Is(err, ErrInsufficientMemory):
		return StatusUnavailable
	default:
		return 0
	}
}

// wrap 让错误带上可读细节，同时保留 errors.Is 判定能力。
func wrap(sentinel error, format string, args ...any) error {
	return fmt.Errorf("%w: %s", sentinel, fmt.Sprintf(format, args...))
}
