package ingress

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"io"
	"math/rand"
	"runtime"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// compressors 给出「编码名 → 压缩函数」，覆盖 out 的 Content-Encoding 取值（含 x-gzip 别名）。
func compressors(t *testing.T) map[string]func([]byte) []byte {
	t.Helper()
	return map[string]func([]byte) []byte{
		"gzip": func(in []byte) []byte {
			var buf bytes.Buffer
			w := gzip.NewWriter(&buf)
			if _, err := w.Write(in); err != nil {
				t.Fatalf("gzip 写入失败: %v", err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("gzip 关闭失败: %v", err)
			}
			return buf.Bytes()
		},
		"x-gzip": func(in []byte) []byte {
			var buf bytes.Buffer
			w := gzip.NewWriter(&buf)
			_, _ = w.Write(in)
			_ = w.Close()
			return buf.Bytes()
		},
		"deflate": func(in []byte) []byte {
			// zlib 包装；裸 deflate 另有专门用例。
			var buf bytes.Buffer
			w := zlib.NewWriter(&buf)
			_, _ = w.Write(in)
			_ = w.Close()
			return buf.Bytes()
		},
		"deflate-raw": func(in []byte) []byte {
			var buf bytes.Buffer
			w, err := flate.NewWriter(&buf, flate.BestSpeed)
			if err != nil {
				t.Fatalf("flate 构造失败: %v", err)
			}
			_, _ = w.Write(in)
			_ = w.Close()
			return buf.Bytes()
		},
		"br": func(in []byte) []byte {
			var buf bytes.Buffer
			w := brotli.NewWriter(&buf)
			_, _ = w.Write(in)
			_ = w.Close()
			return buf.Bytes()
		},
		"zstd": func(in []byte) []byte {
			var buf bytes.Buffer
			w, err := zstd.NewWriter(&buf)
			if err != nil {
				t.Fatalf("zstd 构造失败: %v", err)
			}
			if _, err := w.Write(in); err != nil {
				t.Fatalf("zstd 写入失败: %v", err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("zstd 关闭失败: %v", err)
			}
			return buf.Bytes()
		},
	}
}

// TestRoundTripAllEncodings 覆盖五种编码的往返：identity（无编码）、gzip、deflate（zlib 与裸流）、
// br、zstd，并断言 Decoded/Encoding 反馈正确。
func TestRoundTripAllEncodings(t *testing.T) {
	payload := bytes.Repeat([]byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user"}]}`), 64)
	comp := compressors(t)

	cases := []struct {
		name        string
		header      string
		body        []byte
		wantDecoded bool
		wantEnc     string
	}{
		{name: "identity 无编码", header: "", body: payload},
		{name: "identity 显式", header: "identity", body: payload},
		{name: "gzip", header: "gzip", body: comp["gzip"](payload), wantDecoded: true, wantEnc: "gzip"},
		{name: "x-gzip 别名", header: "x-gzip", body: comp["x-gzip"](payload), wantDecoded: true, wantEnc: "x-gzip"},
		{name: "deflate zlib 包装", header: "deflate", body: comp["deflate"](payload), wantDecoded: true, wantEnc: "deflate"},
		{name: "deflate 裸流", header: "deflate", body: comp["deflate-raw"](payload), wantDecoded: true, wantEnc: "deflate"},
		{name: "br", header: "br", body: comp["br"](payload), wantDecoded: true, wantEnc: "br"},
		{name: "zstd", header: "zstd", body: comp["zstd"](payload), wantDecoded: true, wantEnc: "zstd"},
		{name: "大小写与空白", header: " GZIP ", body: comp["gzip"](payload), wantDecoded: true, wantEnc: "gzip"},
	}

	opts := Options{MaxDecodedBytes: 1 << 20}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewReader(bytes.NewReader(tc.body), tc.header, int64(len(tc.body)), opts)
			if err != nil {
				t.Fatalf("NewReader 失败: %v", err)
			}
			defer func() { _ = r.Close() }()

			got, err := io.ReadAll(r)
			if err != nil {
				t.Fatalf("读取失败: %v", err)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("解压结果不一致：得到 %d 字节，期望 %d 字节", len(got), len(payload))
			}
			if r.Decoded() != tc.wantDecoded {
				t.Fatalf("Decoded() = %v，期望 %v", r.Decoded(), tc.wantDecoded)
			}
			if r.Encoding() != tc.wantEnc {
				t.Fatalf("Encoding() = %q，期望 %q", r.Encoding(), tc.wantEnc)
			}
		})
	}
}

// TestDecompressionBombRejected 覆盖高压缩比输入：8 MiB 全零经 gzip 后只有几 KB，
// 输出上限 1 MiB 时必须按 413 拒绝，而不是把 8 MiB 展开到堆上。
func TestDecompressionBombRejected(t *testing.T) {
	bomb := compressors(t)["gzip"](make([]byte, 8<<20))
	if len(bomb) > 64<<10 {
		t.Fatalf("夹具前提不成立：压缩体 %d 字节，不够「小输入大输出」", len(bomb))
	}

	opts := Options{MaxDecodedBytes: 1 << 20}
	r, err := NewReader(bytes.NewReader(bomb), "gzip", int64(len(bomb)), opts)
	if err != nil {
		t.Fatalf("NewReader 失败: %v", err)
	}
	defer func() { _ = r.Close() }()

	n, err := io.Copy(io.Discard, r)
	if err == nil {
		t.Fatalf("解压炸弹未被拒绝，读出了 %d 字节", n)
	}
	if !errors.Is(err, ErrDecodedTooLarge) {
		t.Fatalf("错误类型 = %v，期望 ErrDecodedTooLarge", err)
	}
	if status := StatusOf(err); status != StatusRequestEntityLarge {
		t.Fatalf("StatusOf = %d，期望 413", status)
	}
	if n > 1<<20 {
		t.Fatalf("在报错前已读出 %d 字节，超过上限", n)
	}
}

// TestTruncatedInputErrors 覆盖截断压缩流：必须报「损坏」（400），不得静默返回半截正文。
func TestTruncatedInputErrors(t *testing.T) {
	comp := compressors(t)
	for name, enc := range map[string]string{"gzip": "gzip", "zstd": "zstd", "br": "br"} {
		t.Run(name, func(t *testing.T) {
			full := comp[name](bytes.Repeat([]byte("abcdefgh"), 4096))
			truncated := full[:len(full)/2]

			opts := Options{MaxDecodedBytes: 4 << 20}
			r, err := NewReader(bytes.NewReader(truncated), enc, int64(len(truncated)), opts)
			if err != nil {
				// 构造期即失败也算合格（部分解压器在头/校验阶段就拒绝）。
				if !errors.Is(err, ErrCorruptBody) {
					t.Fatalf("构造错误 = %v，期望 ErrCorruptBody: %v", err, ErrCorruptBody)
				}
				return
			}
			defer func() { _ = r.Close() }()

			_, err = io.Copy(io.Discard, r)
			if err == nil {
				t.Fatal("截断输入未报错")
			}
			if !errors.Is(err, ErrCorruptBody) {
				t.Fatalf("错误类型 = %v，期望 ErrCorruptBody", err)
			}
		})
	}
}

// TestCompressedTooLarge 覆盖压缩体上限的两条路径：已知体积时构造期即拒绝（413）；
// 体积未知时在流式读取中由计数兜底拒绝（413）。
func TestCompressedTooLarge(t *testing.T) {
	payload := bytes.Repeat([]byte("0123456789"), 1024)
	body := compressors(t)["gzip"](payload)
	if len(body) < 2 {
		t.Fatalf("夹具前提不成立：压缩体仅 %d 字节", len(body))
	}
	// 上限比真实压缩体少 1 字节，确定越界且不需要猜压缩比。
	opts := Options{MaxDecodedBytes: 1 << 20, MaxCompressedBytes: int64(len(body) - 1)}

	t.Run("已知体积构造期拒绝", func(t *testing.T) {
		_, err := NewReader(bytes.NewReader(body), "gzip", int64(len(body)), opts)
		if !errors.Is(err, ErrCompressedTooLarge) {
			t.Fatalf("错误 = %v，期望 ErrCompressedTooLarge", err)
		}
		if StatusOf(err) != StatusRequestEntityLarge {
			t.Fatalf("StatusOf = %d，期望 413", StatusOf(err))
		}
	})

	t.Run("体积未知在读取中拒绝", func(t *testing.T) {
		// 不可压缩正文：压缩体与明文同量级，保证上限在读到中段时才触发（而非构造期就把整体读完）。
		rnd := rand.New(rand.NewSource(1))
		incompressible := make([]byte, 64<<10)
		_, _ = rnd.Read(incompressible)
		randomBody := compressors(t)["gzip"](incompressible)
		streamOpts := Options{MaxDecodedBytes: 1 << 20, MaxCompressedBytes: 8 << 10}

		r, err := NewReader(bytes.NewReader(randomBody), "gzip", -1, streamOpts)
		if err == nil {
			defer func() { _ = r.Close() }()
			_, err = io.Copy(io.Discard, r)
		}
		if !errors.Is(err, ErrCompressedTooLarge) {
			t.Fatalf("错误 = %v，期望 ErrCompressedTooLarge", err)
		}
		if StatusOf(err) != StatusRequestEntityLarge {
			t.Fatalf("StatusOf = %d，期望 413", StatusOf(err))
		}
	})
}

// TestEmptyEncodedBodyPassthrough 覆盖「声明了编码但体为空」：按 Node 语义透传，不得报 400。
func TestEmptyEncodedBodyPassthrough(t *testing.T) {
	r, err := NewReader(strings.NewReader(""), "gzip", -1, Options{})
	if err != nil {
		t.Fatalf("NewReader 失败: %v", err)
	}
	defer func() { _ = r.Close() }()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("得到 %d 字节，期望 0", len(got))
	}
	if r.Decoded() {
		t.Fatal("空体不应被标记为已解压")
	}
}

// TestUnsupportedEncodingPassthrough 覆盖不支持的编码：原样透传并保留原始字节，交给上游处理。
func TestUnsupportedEncodingPassthrough(t *testing.T) {
	body := []byte("not-really-compressed")
	r, err := NewReader(bytes.NewReader(body), "snappy", int64(len(body)), Options{})
	if err != nil {
		t.Fatalf("NewReader 失败: %v", err)
	}
	defer func() { _ = r.Close() }()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("透传字节被改动: %q", got)
	}
	if r.Decoded() {
		t.Fatal("不支持的编码不应被标记为已解压")
	}
}

// TestMultipleLayersRejected 覆盖多层编码：按 400 拒绝（防御解压层数放大）。
func TestMultipleLayersRejected(t *testing.T) {
	payload := []byte("hello")
	inner := compressors(t)["gzip"](payload)
	outer := compressors(t)["gzip"](inner)

	_, err := NewReader(bytes.NewReader(outer), "gzip, gzip", int64(len(outer)), Options{})
	if !errors.Is(err, ErrTooManyLayers) {
		t.Fatalf("错误 = %v，期望 ErrTooManyLayers", err)
	}
	if StatusOf(err) != StatusBadRequest {
		t.Fatalf("StatusOf = %d，期望 400", StatusOf(err))
	}
}

// TestStreamingDoesNotBufferWholeBody 是「流式」这一要求的可执行断言：8 MiB 解压输出在
// 读透全程后，堆上累计分配量必须远小于正文体积（整体缓冲会接近 2 倍正文）。
func TestStreamingDoesNotBufferWholeBody(t *testing.T) {
	const size = 8 << 20
	payload := bytes.Repeat([]byte("cch-streaming-fixture-"), size/22+1)[:size]
	body := compressors(t)["gzip"](payload)

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	r, err := NewReader(bytes.NewReader(body), "gzip", int64(len(body)), Options{MaxDecodedBytes: 16 << 20})
	if err != nil {
		t.Fatalf("NewReader 失败: %v", err)
	}
	n, err := io.Copy(io.Discard, r)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if n != size {
		t.Fatalf("读出 %d 字节，期望 %d", n, size)
	}
	_ = r.Close()
	runtime.ReadMemStats(&after)

	allocated := after.TotalAlloc - before.TotalAlloc
	if allocated > size/2 {
		t.Fatalf("本轮累计分配 %d 字节，接近正文体积 %d：实现疑似整体缓冲而非流式", allocated, size)
	}
}
