package ingress

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// gzipCompress / zstdCompress 供基准预压缩正文（与解码路径分开计时）。
func gzipCompress(b *testing.B, in []byte) []byte {
	b.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(in); err != nil {
		b.Fatalf("gzip 写入失败: %v", err)
	}
	if err := w.Close(); err != nil {
		b.Fatalf("gzip 关闭失败: %v", err)
	}
	return buf.Bytes()
}

func zstdCompress(b *testing.B, in []byte) []byte {
	b.Helper()
	var buf bytes.Buffer
	w, err := zstd.NewWriter(&buf)
	if err != nil {
		b.Fatalf("zstd 构造失败: %v", err)
	}
	if _, err := w.Write(in); err != nil {
		b.Fatalf("zstd 写入失败: %v", err)
	}
	if err := w.Close(); err != nil {
		b.Fatalf("zstd 关闭失败: %v", err)
	}
	return buf.Bytes()
}

// benchPayload 生成指定大小的可压缩正文（真实请求体是 JSON，重复度高但非全零）。
func benchPayload(size int) []byte {
	unit := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"cch"}]}`)
	buf := bytes.Repeat(unit, size/len(unit)+1)
	return buf[:size]
}

// benchBodies 预压缩一次并复用，避免把压缩开销算进解压路径。
func benchBodies(b *testing.B, size int) (payload, gzipBody, zstdBody []byte) {
	b.Helper()
	payload = benchPayload(size)
	gzipBody = gzipCompress(b, payload)
	zstdBody = zstdCompress(b, payload)
	return payload, gzipBody, zstdBody
}

// BenchmarkDecode1MiB / BenchmarkDecode8MiB 覆盖两种体积下的透传与三种解压路径。
// 报出的 B/op 直接回答「是否流式」：整体缓冲会接近正文体积两倍，流式则在 100 KiB 量级。
func BenchmarkDecode1MiB(b *testing.B) { benchmarkDecode(b, 1<<20) }
func BenchmarkDecode8MiB(b *testing.B) { benchmarkDecode(b, 8<<20) }

func benchmarkDecode(b *testing.B, size int) {
	payload, gzipBody, zstdBody := benchBodies(b, size)
	opts := Options{MaxDecodedBytes: int64(size) + (1 << 20)}

	cases := []struct {
		name string
		enc  string
		body []byte
	}{
		{name: "identity", enc: "", body: payload},
		{name: "gzip", enc: "gzip", body: gzipBody},
		{name: "zstd", enc: "zstd", body: zstdBody},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(size))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r, err := NewReader(bytes.NewReader(tc.body), tc.enc, int64(len(tc.body)), opts)
				if err != nil {
					b.Fatalf("NewReader 失败: %v", err)
				}
				n, err := io.Copy(io.Discard, r)
				if err != nil {
					b.Fatalf("读取失败: %v", err)
				}
				if n != int64(size) {
					b.Fatalf("读出 %d 字节，期望 %d", n, size)
				}
				_ = r.Close()
			}
		})
	}
}

// BenchmarkPeekSize 覆盖读体前的准入判定：必须是微秒级且零分配（热路径每个请求都跑）。
func BenchmarkPeekSize(b *testing.B) {
	header := http.Header{}
	header.Set("Content-Length", "8388608")
	header.Set("Content-Encoding", "gzip")
	opts := Options{}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := PeekSize(header, opts); err != nil {
			b.Fatalf("PeekSize 失败: %v", err)
		}
	}
}

// BenchmarkLimiterAcquireRelease 覆盖准入闸门的争用开销：并发解压路径每个请求都要走一次。
func BenchmarkLimiterAcquireRelease(b *testing.B) {
	limiter := NewLimiter(16, 64<<20)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			lease, err := limiter.TryAcquire(1 << 20)
			if err != nil {
				b.Errorf("申请失败: %v", err)
				return
			}
			lease.Release()
		}
	})
}
