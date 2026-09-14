package ingress

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestParseContentEncoding(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   []string
	}{
		{name: "空", header: "", want: nil},
		{name: "只有 identity", header: "identity", want: nil},
		{name: "单层", header: "gzip", want: []string{"gzip"}},
		{name: "大小写与空白", header: "  GZip  ", want: []string{"gzip"}},
		{name: "多层保序", header: "br, gzip", want: []string{"br", "gzip"}},
		{name: "剔除 identity 与空 token", header: "identity, gzip, , identity", want: []string{"gzip"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseContentEncoding(tc.header)
			if len(got) != len(tc.want) {
				t.Fatalf("得到 %v，期望 %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("得到 %v，期望 %v", got, tc.want)
				}
			}
		})
	}
}

func TestPlanRequestBody(t *testing.T) {
	opts := Options{MaxDecodedBytes: 1 << 20, MaxCompressedBytes: 1 << 20, MaxLayers: 1}

	t.Run("无编码透传", func(t *testing.T) {
		plan, err := PlanRequestBody(1024, "", opts)
		if err != nil {
			t.Fatalf("意外错误: %v", err)
		}
		if plan.Action != ActionPass || plan.Reason != "no-encoding" {
			t.Fatalf("plan = %+v", plan)
		}
	})

	t.Run("空体优先于层数校验", func(t *testing.T) {
		plan, err := PlanRequestBody(0, "gzip, br", opts)
		if err != nil {
			t.Fatalf("空体不应报错: %v", err)
		}
		if plan.Action != ActionPass || plan.Reason != "empty" {
			t.Fatalf("plan = %+v", plan)
		}
	})

	t.Run("层数超限 400", func(t *testing.T) {
		_, err := PlanRequestBody(1024, "gzip, br", opts)
		if !errors.Is(err, ErrTooManyLayers) {
			t.Fatalf("错误 = %v，期望 ErrTooManyLayers", err)
		}
		if StatusOf(err) != StatusBadRequest {
			t.Fatalf("StatusOf = %d，期望 400", StatusOf(err))
		}
	})

	t.Run("不支持编码透传并列出", func(t *testing.T) {
		plan, err := PlanRequestBody(1024, "snappy", opts)
		if err != nil {
			t.Fatalf("意外错误: %v", err)
		}
		if plan.Action != ActionPass || plan.Reason != "unsupported" {
			t.Fatalf("plan = %+v", plan)
		}
		if len(plan.Unsupported) != 1 || plan.Unsupported[0] != "snappy" {
			t.Fatalf("Unsupported = %v", plan.Unsupported)
		}
	})

	t.Run("压缩体超限 413", func(t *testing.T) {
		_, err := PlanRequestBody((1<<20)+1, "gzip", opts)
		if !errors.Is(err, ErrCompressedTooLarge) {
			t.Fatalf("错误 = %v，期望 ErrCompressedTooLarge", err)
		}
		if StatusOf(err) != StatusRequestEntityLarge {
			t.Fatalf("StatusOf = %d，期望 413", StatusOf(err))
		}
	})

	t.Run("体积未知不按体积拒绝", func(t *testing.T) {
		plan, err := PlanRequestBody(-1, "gzip", opts)
		if err != nil {
			t.Fatalf("体积未知不应在计划阶段被拒: %v", err)
		}
		if plan.Action != ActionDecode {
			t.Fatalf("plan = %+v", plan)
		}
	})

	t.Run("解码顺序反向", func(t *testing.T) {
		plan, err := PlanRequestBody(1024, "gzip", opts)
		if err != nil {
			t.Fatalf("意外错误: %v", err)
		}
		if len(plan.DecodeOrder) != 1 || plan.DecodeOrder[0] != "gzip" {
			t.Fatalf("DecodeOrder = %v", plan.DecodeOrder)
		}
	})
}

// countingBody 记录被读取的次数，用来断言「读体前的拒绝确实没有碰正文」。
type countingBody struct {
	reads int
}

func (c *countingBody) Read(p []byte) (int, error) {
	c.reads++
	return 0, nil
}

// TestPeekSizeRejectsOversizeContentLengthWithoutReadingBody 覆盖「超长 content-length 在未读体时
// 即被拒」：PeekSize 只看头部，拒绝后请求体一个字节都不应被读取。
func TestPeekSizeRejectsOversizeContentLengthWithoutReadingBody(t *testing.T) {
	body := &countingBody{}
	header := http.Header{}
	header.Set("Content-Length", "104857601") // 超 100 MiB 一字节
	header.Set("Content-Encoding", "gzip")

	_, _, err := PeekSize(header, Options{})
	if !errors.Is(err, ErrCompressedTooLarge) {
		t.Fatalf("错误 = %v，期望 ErrCompressedTooLarge", err)
	}
	if StatusOf(err) != StatusRequestEntityLarge {
		t.Fatalf("StatusOf = %d，期望 413", StatusOf(err))
	}
	if body.reads != 0 {
		t.Fatalf("读体前拒绝却读了正文 %d 次", body.reads)
	}
}

func TestPeekSizeHints(t *testing.T) {
	t.Run("无编码时给出精确明文体积", func(t *testing.T) {
		header := http.Header{}
		header.Set("Content-Length", "2048")
		compressed, decoded, err := PeekSize(header, Options{})
		if err != nil {
			t.Fatalf("意外错误: %v", err)
		}
		if compressed != 2048 || decoded != 2048 {
			t.Fatalf("compressed=%d decoded=%d，期望同为 2048", compressed, decoded)
		}
	})

	t.Run("有编码时不猜解压体积", func(t *testing.T) {
		header := http.Header{}
		header.Set("Content-Length", "2048")
		header.Set("Content-Encoding", "zstd")
		compressed, decoded, err := PeekSize(header, Options{})
		if err != nil {
			t.Fatalf("意外错误: %v", err)
		}
		if compressed != 2048 || decoded != -1 {
			t.Fatalf("compressed=%d decoded=%d，期望 2048 与 -1", compressed, decoded)
		}
	})

	t.Run("无 content-length 时为未知", func(t *testing.T) {
		compressed, decoded, err := PeekSize(http.Header{}, Options{})
		if err != nil {
			t.Fatalf("意外错误: %v", err)
		}
		if compressed != -1 || decoded != -1 {
			t.Fatalf("compressed=%d decoded=%d，期望同为 -1", compressed, decoded)
		}
	})

	t.Run("content-length 非法视为未知", func(t *testing.T) {
		header := http.Header{}
		header.Set("Content-Length", "12abc")
		compressed, _, err := PeekSize(header, Options{})
		if err != nil {
			t.Fatalf("意外错误: %v", err)
		}
		if compressed != -1 {
			t.Fatalf("compressed=%d，期望 -1", compressed)
		}
	})

	t.Run("不支持的编码不报错", func(t *testing.T) {
		header := http.Header{}
		header.Set("Content-Encoding", "snappy")
		_, _, err := PeekSize(header, Options{})
		if err != nil {
			t.Fatalf("不支持的编码应透传而非报错: %v", err)
		}
	})
}

// TestOptionsFromEnv 覆盖环境变量解析与非法值回退（与 Node 的 parseLimitEnv 同语义）。
func TestOptionsFromEnv(t *testing.T) {
	t.Setenv(EnvMaxDecodedBytes, "2097152")
	t.Setenv(EnvMaxCompressedBytes, "1048576")
	t.Setenv(EnvMaxConcurrentDecompressions, "2")
	t.Setenv(EnvMaxInflightDecompressionByte, "4194304")

	opts := DefaultOptions()
	if opts.MaxDecodedBytes != 2097152 {
		t.Fatalf("MaxDecodedBytes = %d", opts.MaxDecodedBytes)
	}
	if opts.MaxCompressedBytes != 1048576 {
		t.Fatalf("MaxCompressedBytes = %d", opts.MaxCompressedBytes)
	}
	if opts.MaxLayers != DefaultMaxLayers {
		t.Fatalf("MaxLayers = %d", opts.MaxLayers)
	}
	stats := opts.Decompression.Stats()
	if stats.MaxConcurrent != 2 || stats.MaxBytes != 4194304 {
		t.Fatalf("限流器上限 = %+v", stats)
	}

	t.Setenv(EnvMaxDecodedBytes, "not-a-number")
	if got := DefaultOptions().MaxDecodedBytes; got != DefaultMaxDecodedBytes {
		t.Fatalf("非法值未回退默认：%d", got)
	}
	t.Setenv(EnvMaxDecodedBytes, "-5")
	if got := DefaultOptions().MaxDecodedBytes; got != DefaultMaxDecodedBytes {
		t.Fatalf("负值未回退默认：%d", got)
	}
}

// TestCompressedDefaultFollowsDecoded 覆盖「未单独设压缩体上限时跟随解压上限」（Node 同语义：
// 避免明文放行而压缩体被拒的不对称）。
func TestCompressedDefaultFollowsDecoded(t *testing.T) {
	t.Setenv(EnvMaxDecodedBytes, "1048576")
	t.Setenv(EnvMaxCompressedBytes, "")
	opts := DefaultOptions()
	if opts.MaxCompressedBytes != 1048576 {
		t.Fatalf("MaxCompressedBytes = %d，期望跟随 MaxDecodedBytes", opts.MaxCompressedBytes)
	}
}

// TestStatusOfUnknownError 覆盖未识别错误返回 0（由调用方决定，本包不猜）。
func TestStatusOfUnknownError(t *testing.T) {
	if got := StatusOf(errors.New("别的东西")); got != 0 {
		t.Fatalf("StatusOf = %d，期望 0", got)
	}
	if got := StatusOf(nil); got != 0 {
		t.Fatalf("StatusOf(nil) = %d，期望 0", got)
	}
	if !strings.Contains(ErrCorruptBody.Error(), "解压") {
		t.Fatalf("哨兵错误文案不便于定位: %q", ErrCorruptBody.Error())
	}
}
