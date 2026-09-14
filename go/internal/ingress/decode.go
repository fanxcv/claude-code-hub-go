package ingress

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"io"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// maxZstdWindowBytes 是 zstd 解压窗口上限。
//
// 窗口由压缩帧自己声明，与正文体积无关：参考实现（klauspost 编码器，与 zstd CLI 同口径）对
// 1 MiB 与 8 MiB 正文都声明 8 MiB 窗口，因此窗口上限绝不能从「解压输出上限」推导——小请求体
// 会把上限压到窗口之下，真实客户端反而被误拒（历史缺陷）。
// 16 MiB 给出 2 倍余量，同时挡住「声明巨窗口骗取分配」的放大行为；
// 实际分配跟随帧声明的窗口（低内存模式下不会按上限预占）。
const maxZstdWindowBytes = 16 * 1024 * 1024

// Reader 是入站请求体的流式读取器：按 content-encoding 解压，并对压缩体、解压输出、
// 在途预算三重限额。它实现 io.Reader 与 io.Closer。
//
// 使用约定：拿到 *Reader 后必须 Close（defer 即可），否则在途预算不会归还。Close 只归还
// 本包自己的资源，**不关闭调用方传入的 src**——上游连接由调用方掌管。
type Reader struct {
	src io.Reader

	// decoded 表示是否实际执行了解压；透传时为 false。
	decoded bool
	// empty 表示压缩体为空（在体积未知时由嗅探得出），用于保持「空体透传」语义。
	empty bool
	// encoding 是实际应用的编码链（如 "gzip" 或 "br, gzip"）；透传时为 ""。
	encoding string

	closers []io.Closer
	lease   *Lease
}

// Decoded 报告是否实际执行了解压。
func (r *Reader) Decoded() bool { return r.decoded }

// Encoding 返回实际应用的编码链（按解码顺序，逗号加空格分隔）；未解压时为空串。
func (r *Reader) Encoding() string { return r.encoding }

// Read 从解压流读取。任一限额被突破时返回可判别错误（见 errors.go），此后读取一律返回同一错误。
func (r *Reader) Read(p []byte) (int, error) { return r.src.Read(p) }

// Close 释放在途占位与解压器资源；幂等，且不关闭调用方传入的 src。
func (r *Reader) Close() error {
	var firstErr error
	for _, c := range r.closers {
		if err := c.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	r.closers = nil
	if r.lease != nil {
		r.lease.Release()
		r.lease = nil
	}
	return firstErr
}

// NewReader 按 contentEncoding 把 src 包装成流式解压读取器。
//
// declaredCompressed 是调用方已知的压缩体字节数（-1 表示未知，通常取自 content-length）：
// 已知时先按 PlanRequestBody 做体积拒绝并一次性申请预算，未知时由读取过程中的计数兜底。
//
// 错误语义与 Node 侧 decodeRequestBodyAsync 对齐：层数超限 400、压缩体超限 413、
// 解压输出超限 413、流损坏 400、在途预算饱和 503（同步失败，不排队）。
func NewReader(src io.Reader, contentEncoding string, declaredCompressed int64, opts Options) (*Reader, error) {
	o := opts.normalized()
	if src == nil {
		return nil, wrap(ErrCorruptBody, "src 为 nil")
	}
	plan, err := PlanRequestBody(declaredCompressed, contentEncoding, o)
	if err != nil {
		return nil, err
	}
	// 透传：不改动字节，也不占用在途预算（不涉及解压）。
	if plan.Action == ActionPass {
		return &Reader{src: src, decoded: false, encoding: ""}, nil
	}

	var lease *Lease
	if o.Decompression != nil {
		// 名额与「已知压缩体字节」一起申请；未知体积先按 0 申请，读取时用 Grow 增量补。
		reserve := declaredCompressed
		if reserve < 0 {
			reserve = 0
		}
		lease, err = o.Decompression.TryAcquire(reserve)
		if err != nil {
			return nil, err
		}
	}

	r, err := buildDecoder(src, plan.DecodeOrder, o, lease)
	if err != nil {
		if lease != nil {
			lease.Release()
		}
		return nil, err
	}
	// 体积未知但流是空的（分块的空体）：与 Node 的「空体直接透传」保持一致，
	// 否则空流会被解压器当成损坏流报 400。
	if r.empty {
		if lease != nil {
			lease.Release()
		}
		return &Reader{src: r.src, decoded: false, encoding: ""}, nil
	}
	r.decoded = true
	r.encoding = joinEncoding(plan.DecodeOrder)
	return r, nil
}

// buildDecoder 组装「压缩体计数 → 解压 → 输出限额」三层管道。失败时释放已建资源。
func buildDecoder(src io.Reader, order []string, opts Options, lease *Lease) (*Reader, error) {
	out := &Reader{lease: lease}
	buffered := bufio.NewReaderSize(src, 32*1024)
	// 空流：不建解压器（解压器对空输入会报「损坏」）。
	if _, err := buffered.Peek(1); err != nil {
		if errors.Is(err, io.EOF) {
			out.src = buffered
			out.empty = true
			return out, nil
		}
		_ = out.Close()
		return nil, wrap(ErrCorruptBody, "读取请求体失败: %v", err)
	}
	// 压缩体计数：按实际读取字节增量申请预算；超上限即拒绝。
	counted := &countingReader{src: buffered, limit: opts.MaxCompressedBytes, lease: lease}

	current := io.Reader(counted)
	for _, enc := range order {
		next, closer, err := newDecompressor(current, enc, opts, buffered)
		if err != nil {
			_ = out.Close()
			return nil, err
		}
		if closer != nil {
			out.closers = append(out.closers, closer)
		}
		// 解压器在构造期报错只是一部分：截断/损坏流通常在读到中段才被发现，
		// 因此每一层的读取错误都要过同一套映射。
		current = &decodeErrorReader{src: next, encoding: enc}
	}

	out.src = &limitReader{src: current, remaining: opts.MaxDecodedBytes}
	return out, nil
}

// decodeErrorReader 把解压器在读取过程中报出的错误映射为可判别错误（损坏为 400，
// 本包自己的哨兵错误原样透传）。
type decodeErrorReader struct {
	src      io.Reader
	encoding string
}

func (d *decodeErrorReader) Read(p []byte) (int, error) {
	n, err := d.src.Read(p)
	if err == nil || errors.Is(err, io.EOF) {
		return n, err
	}
	if errors.Is(err, ErrCorruptBody) || errors.Is(err, ErrDecodedTooLarge) ||
		errors.Is(err, ErrCompressedTooLarge) {
		return n, err
	}
	return n, mapDecodeError(err, d.encoding)
}

// newDecompressor 构造单层解码器。sniffer 是压缩体的 bufio 读取器，deflate 需要嗅探头部来决定
// 走 zlib 包装还是裸流（Node 侧是「先试 zlib，失败再试裸流」；Go 侧用两字节头判定，
// 结果等价且是流式的，不需要把整个压缩体读进内存再重试）。
func newDecompressor(
	current io.Reader,
	encoding string,
	opts Options,
	sniffer *bufio.Reader,
) (io.Reader, io.Closer, error) {
	switch encoding {
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(current)
		if err != nil {
			return nil, nil, mapDecodeError(err, encoding)
		}
		return zr, zr, nil

	case "deflate":
		zr, err := newDeflateReader(current, sniffer)
		if err != nil {
			return nil, nil, mapDecodeError(err, encoding)
		}
		return zr, zr, nil

	case "br":
		// brotli.Reader 无需显式关闭；其内部窗口由流头声明且受实现限制。
		return brotli.NewReader(current), nil, nil

	case "zstd":
		zr, err := zstd.NewReader(
			current,
			zstd.WithDecoderConcurrency(1), // 单解码器单 goroutine：并发由调用方按在途预算控制
			zstd.WithDecoderLowmem(true),
			zstd.WithDecoderMaxWindow(maxZstdWindowBytes),
			zstd.WithDecoderMaxMemory(maxZstdWindowBytes),
		)
		if err != nil {
			return nil, nil, mapDecodeError(err, encoding)
		}
		return zr, zr.IOReadCloser(), nil

	default:
		// ParseContentEncoding + 支持集校验已保证不会走到这里。
		return nil, nil, wrap(ErrCorruptBody, "不支持的 content-encoding: %s", encoding)
	}
}

// newDeflateReader 判定 deflate 是 zlib 包装还是裸流：zlib 头满足 CMF 低四位为 8 且
// (CMF<<8|FLG) 能被 31 整除。嗅探只 Peek 不消费，因此可流式进行。
func newDeflateReader(current io.Reader, sniffer *bufio.Reader) (io.ReadCloser, error) {
	head, peekErr := sniffer.Peek(2)
	if peekErr == nil && len(head) == 2 {
		checksum := (uint16(head[0])<<8 | uint16(head[1]))
		if head[0]&0x0f == 8 && checksum%31 == 0 {
			return zlib.NewReader(current)
		}
		return flate.NewReader(current), nil
	}
	// 不足两字节：交给 zlib 判定，损坏流由它报错（空体在 PlanRequestBody 已按透传处理）。
	return zlib.NewReader(current)
}

// countingReader 统计压缩体字节数并同步申请在途预算。
//
// 它同时承担两道闸：单请求压缩体上限（413）与全局在途字节预算（503）。计数按**实际读取**而非
// 声明值，因此分块请求也逃不掉；预算按增量申请，所以大请求不会一次性占满全局额度。
type countingReader struct {
	src   io.Reader
	limit int64
	lease *Lease

	read int64
	fail error
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.fail != nil {
		return 0, c.fail
	}
	n, err := c.src.Read(p)
	if n > 0 {
		c.read += int64(n)
		if c.limit > 0 && c.read > c.limit {
			c.fail = wrap(ErrCompressedTooLarge, "压缩请求体超过上限 %d 字节", c.limit)
			return 0, c.fail
		}
		if c.lease != nil {
			if growErr := c.lease.Grow(int64(n)); growErr != nil {
				c.fail = growErr
				return 0, growErr
			}
		}
	}
	return n, err
}

// limitReader 是输出限额读取器：超出上限返回 ErrDecodedTooLarge（413）而非静默截断——
// 截断会让上层把半个请求体当完整请求解析。恰好读满上限且流真的结束时不报错：
// 剩余额度耗尽后先探一字节，确认 EOF 才返回 EOF。
type limitReader struct {
	src       io.Reader
	remaining int64
	fail      error
}

func (l *limitReader) Read(p []byte) (int, error) {
	if l.fail != nil {
		return 0, l.fail
	}
	if l.remaining <= 0 {
		var probe [1]byte
		n, err := l.src.Read(probe[:])
		if n == 0 {
			return 0, err
		}
		l.fail = wrap(ErrDecodedTooLarge, "解压后请求体超过上限")
		return 0, l.fail
	}
	if int64(len(p)) > l.remaining {
		p = p[:l.remaining]
	}
	n, err := l.src.Read(p)
	l.remaining -= int64(n)
	return n, err
}

// mapDecodeError 把解压器错误映射为可判别错误：本包自己的哨兵（输出/压缩体超限）原样透传，
// 其余（损坏流）为 400。
func mapDecodeError(err error, encoding string) error {
	if errors.Is(err, ErrDecodedTooLarge) || errors.Is(err, ErrCompressedTooLarge) ||
		errors.Is(err, ErrCorruptBody) {
		return err
	}
	return wrap(ErrCorruptBody, "content-encoding %q 的解压流损坏: %v", encoding, err)
}

func joinEncoding(order []string) string {
	if len(order) == 0 {
		return ""
	}
	var buf bytes.Buffer
	for i, enc := range order {
		if i > 0 {
			buf.WriteString(", ")
		}
		buf.WriteString(enc)
	}
	return buf.String()
}

// 编译期断言：Reader 满足 io.ReadCloser。
var _ io.ReadCloser = (*Reader)(nil)
