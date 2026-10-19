package convert

import (
	"fmt"
	"strings"
	"testing"
)

// 本文件钉住 responses 线正文累积的线性分配（与 stream_chat_linear_test.go 同一判据），
// 并给出 responses 解码/编码与 SSE 编码路径的分配基线。

// responsesDeltaStream 造一条纯增量正文的 responses 上游流（parts 个正文帧）。
func responsesDeltaStream(parts int) []byte {
	events := responsesTextItem(0, "msg0", "")
	for i := 0; i < parts; i++ {
		events = append(events, responsesTextDeltaAt(0, "msg0", chatTestParts[i%len(chatTestParts)]))
	}
	events = append(events, responsesCompletedWith(`[]`))
	return []byte(responsesEventStream(events))
}

// responsesDecodeAllocs 解一条纯增量流，返回累计分配字节数与客户端可见正文长度。
func responsesDecodeAllocs(t *testing.T, parts int) (allocBytes uint64, textLen int) {
	t.Helper()
	raw := responsesDeltaStream(parts)
	decoder := newResponsesStreamDecoder(ConvertCtx{
		ClientFormat: FormatResponse,
		TargetProto:  ProtocolOpenAIResponses,
	})
	var text strings.Builder
	collect := func(chunks []Chunk) {
		for _, chunk := range chunks {
			if chunk.TextDelta != nil {
				text.WriteString(*chunk.TextDelta)
			}
		}
	}
	allocBytes = totalAlloc()
	collect(decoder.Push(raw))
	collect(decoder.Flush())
	allocBytes = totalAlloc() - allocBytes
	return allocBytes, text.Len()
}

// responsesEncodeChunks 造一条「一个文本块 + parts 个正文增量」的枢纽块序列。
func responsesEncodeChunks(parts int) []Chunk {
	chunks := []Chunk{
		{Kind: ChunkStart},
		{Kind: ChunkBlockStart, BlockIndex: intPtr(0), Block: &Block{Kind: BlockText}},
	}
	for i := 0; i < parts; i++ {
		chunks = append(chunks, Chunk{
			Kind:       ChunkBlockDelta,
			BlockIndex: intPtr(0),
			TextDelta:  stringPtr(chatTestParts[i%len(chatTestParts)]),
		})
	}
	reason := StopEndTurn
	chunks = append(chunks,
		Chunk{Kind: ChunkBlockStop, BlockIndex: intPtr(0)},
		Chunk{Kind: ChunkDelta, StopReason: &reason},
		Chunk{Kind: ChunkEnd})
	return chunks
}

// responsesEncodeAllocs 编一条纯增量流，返回累计分配字节数与客户端字节数。
func responsesEncodeAllocs(t *testing.T, parts int) (allocBytes uint64, emitted int) {
	t.Helper()
	chunks := responsesEncodeChunks(parts)
	allocBytes = totalAlloc()
	encoder := newResponsesStreamEncoder(ConvertCtx{
		ClientFormat: FormatResponse,
		TargetProto:  ProtocolOpenAIResponses,
		Model:        "m",
	})
	for _, chunk := range chunks {
		for _, frame := range encoder.Push(chunk) {
			emitted += len(frame)
		}
	}
	for _, frame := range encoder.Flush() {
		emitted += len(frame)
	}
	allocBytes = totalAlloc() - allocBytes
	return allocBytes, emitted
}

// TestResponsesStreamAccumulationIsLinear 钉住「正文累积必须是线性分配」。
//
// 为何要钉：`emitted` / `messageText` 曾用 `string +=` 累积，每帧把已收正文全量拷一遍，
// 整条流 O(正文长度²)（chat 线已先修，见 TestChatStreamAccumulationIsLinear）。
// 判据取「分配字节 / 正文长度」的比值：线性实现下它随帧数近似不变，二次实现下随帧数线性增长。
func TestResponsesStreamAccumulationIsLinear(t *testing.T) {
	if testing.Short() {
		t.Skip("分配量随规模变化，短模式跳过")
	}
	smallBytes, smallLen := responsesDecodeAllocs(t, 50)
	largeBytes, largeLen := responsesDecodeAllocs(t, 2000)
	if smallLen == 0 || largeLen == 0 {
		t.Fatal("正文长度不得为零")
	}
	smallRatio := float64(smallBytes) / float64(smallLen)
	largeRatio := float64(largeBytes) / float64(largeLen)

	// 帧数放大 40 倍：线性实现的比值基本不变（缓冲扩容是摊还的），二次实现会增长数十倍。
	if largeRatio > smallRatio*2 {
		t.Fatalf("正文累积疑似二次：50 帧时 %.1f B/字节，2000 帧时 %.1f B/字节（比值 %.2f，上限 2）",
			smallRatio, largeRatio, largeRatio/smallRatio)
	}
}

// TestResponsesStreamEncodingIsLinear 钉住编码侧同一条判据（块内已发正文的累积）。
func TestResponsesStreamEncodingIsLinear(t *testing.T) {
	if testing.Short() {
		t.Skip("分配量随规模变化，短模式跳过")
	}
	smallBytes, smallLen := responsesEncodeAllocs(t, 50)
	largeBytes, largeLen := responsesEncodeAllocs(t, 2000)
	if smallLen == 0 || largeLen == 0 {
		t.Fatal("客户端字节数不得为零")
	}
	smallRatio := float64(smallBytes) / float64(smallLen)
	largeRatio := float64(largeBytes) / float64(largeLen)

	if largeRatio > smallRatio*2 {
		t.Fatalf("编码侧正文累积疑似二次：50 帧时 %.1f B/字节，2000 帧时 %.1f B/字节（比值 %.2f，上限 2）",
			smallRatio, largeRatio, largeRatio/smallRatio)
	}
}

func BenchmarkResponsesStreamDecode(b *testing.B) {
	raw := responsesDeltaStream(64)
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	for i := 0; i < b.N; i++ {
		decoder := newResponsesStreamDecoder(ConvertCtx{
			ClientFormat: FormatResponse,
			TargetProto:  ProtocolOpenAIResponses,
		})
		_ = decoder.Push(raw)
		_ = decoder.Flush()
	}
}

func BenchmarkResponsesStreamEncode(b *testing.B) {
	chunks := responsesEncodeChunks(64)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		encoder := newResponsesStreamEncoder(ConvertCtx{
			ClientFormat: FormatResponse,
			TargetProto:  ProtocolOpenAIResponses,
			Model:        "m",
		})
		for _, chunk := range chunks {
			_ = encoder.Push(chunk)
		}
		_ = encoder.Flush()
	}
}

// BenchmarkAnthropicStreamEncode 量 SSE 编码路径（emit 的 JSON→帧融合）。
func BenchmarkAnthropicStreamEncode(b *testing.B) {
	for _, size := range []int{16, 1024} {
		delta := strings.Repeat("d", size)
		b.Run(fmt.Sprintf("delta%dB", size), func(b *testing.B) {
			chunks := []Chunk{
				{Kind: ChunkStart},
				{Kind: ChunkBlockStart, BlockIndex: intPtr(0), Block: &Block{Kind: BlockText}},
			}
			for i := 0; i < 64; i++ {
				chunks = append(chunks, Chunk{
					Kind:       ChunkBlockDelta,
					BlockIndex: intPtr(0),
					TextDelta:  stringPtr(delta),
				})
			}
			reason := StopEndTurn
			chunks = append(chunks,
				Chunk{Kind: ChunkBlockStop, BlockIndex: intPtr(0)},
				Chunk{Kind: ChunkDelta, StopReason: &reason},
				Chunk{Kind: ChunkEnd})
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				encoder := newAnthropicStreamEncoder(ConvertCtx{
					ClientFormat: FormatClaude,
					TargetProto:  ProtocolOpenAIResponses,
					Model:        "m",
				})
				for _, chunk := range chunks {
					_ = encoder.Push(chunk)
				}
				_ = encoder.Flush()
			}
		})
	}
}
