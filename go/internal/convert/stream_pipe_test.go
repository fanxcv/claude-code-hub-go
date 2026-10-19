package convert

import (
	"strings"
	"testing"
)

// 本文件钉住 StreamPipe 的字节层契约：跨块残字必须自有存储（不得别名调用方的读缓冲），
// 并给出管道吞吐的分配基线。

// responsesMultibyteStream 造一条正文为「中文」的 responses 上游流。
func responsesMultibyteStream() string {
	events := append(responsesTextItem(0, "msg0", ""), responsesTextDeltaAt(0, "msg0", "中文"))
	events = append(events, responsesTextDone(0, "msg0", "中文"), responsesCompletedWith(`[]`))
	return responsesEventStream(events)
}

// responsesMultibyteBulkStream 造一条正文全为多字节字符的 responses 上游流。
func responsesMultibyteBulkStream(repeat int) []byte {
	events := append(responsesTextItem(0, "msg0", ""),
		responsesTextDeltaAt(0, "msg0", strings.Repeat("中文", repeat)))
	events = append(events, responsesCompletedWith(`[]`))
	return []byte(responsesEventStream(events))
}

// TestStreamPipePendingSurvivesBufferReuse 钉住跨块残字不被调用方的读缓冲复用改写。
//
// 为什么必须钉：dataplane 复用同一个 32 KiB 读缓冲逐块 Read（internal/dataplane/stream.go）。
// 若 pending 别名该缓冲的子切片，下次 Read 就会把被切开的多字节字符改写掉，客户端拿到乱码。
func TestStreamPipePendingSurvivesBufferReuse(t *testing.T) {
	upstream := responsesMultibyteStream()
	// 在「中」的首字节之后切开：半个字符留在 pending 里等下一块。
	cut := strings.IndexByte(upstream, 0xE4) + 1
	if cut <= 0 {
		t.Fatal("夹具里找不到「中」的首字节")
	}
	pipe, ok := NewStreamPipe(ProtocolOpenAIResponses, ProtocolOpenAIResponses, ConvertCtx{
		ClientFormat: FormatResponse,
		TargetProto:  ProtocolOpenAIResponses,
		Stream:       true,
	})
	if !ok {
		t.Fatal("建管道失败：responses -> responses")
	}
	buffer := make([]byte, 32<<10)
	var out []byte
	feed := func(piece string) {
		copy(buffer, piece)
		out = append(out, pipe.Push(buffer[:len(piece)])...)
	}
	feed(upstream[:cut])
	for i := range buffer {
		buffer[i] = 'x' // 模拟下一次 Read 覆盖整个缓冲
	}
	feed(upstream[cut:])
	out = append(out, pipe.Flush()...)

	if text := clientDialectText(t, ProtocolOpenAIResponses, string(out)); text != "中文" {
		t.Fatalf("跨块残字被读缓冲复用改写：正文 = %q，输出 %q", text, string(out))
	}
}

// BenchmarkStreamPipeChunked 量整条管道（上游字节 → 解码 → 编码 → 客户端字节）的每块成本。
func BenchmarkStreamPipeChunked(b *testing.B) {
	raw := responsesDeltaStream(4096)
	const chunkSize = 32 << 10
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	for i := 0; i < b.N; i++ {
		pipe, ok := NewStreamPipe(ProtocolOpenAIResponses, ProtocolOpenAIResponses, ConvertCtx{
			ClientFormat: FormatResponse,
			TargetProto:  ProtocolOpenAIResponses,
			Stream:       true,
		})
		if !ok {
			b.Fatal("建管道失败：responses -> responses")
		}
		var out []byte
		for start := 0; start < len(raw); start += chunkSize {
			end := min(start+chunkSize, len(raw))
			out = append(out, pipe.Push(raw[start:end])...)
		}
		out = append(out, pipe.Flush()...)
		if len(out) == 0 {
			b.Fatal("管道没有产出")
		}
	}
}

// BenchmarkStreamPipeMultibyteChunked 量「残字跨块」高频路径：3 字节一块，几乎每块都带半个字符。
func BenchmarkStreamPipeMultibyteChunked(b *testing.B) {
	raw := responsesMultibyteBulkStream(512)
	const chunkSize = 3
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	for i := 0; i < b.N; i++ {
		pipe, ok := NewStreamPipe(ProtocolOpenAIResponses, ProtocolOpenAIResponses, ConvertCtx{
			ClientFormat: FormatResponse,
			TargetProto:  ProtocolOpenAIResponses,
			Stream:       true,
		})
		if !ok {
			b.Fatal("建管道失败：responses -> responses")
		}
		var out []byte
		for start := 0; start < len(raw); start += chunkSize {
			end := min(start+chunkSize, len(raw))
			out = append(out, pipe.Push(raw[start:end])...)
		}
		out = append(out, pipe.Flush()...)
		if len(out) == 0 {
			b.Fatal("管道没有产出")
		}
	}
}
