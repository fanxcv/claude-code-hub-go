package convert

import (
	"runtime"
	"strings"
	"testing"
)

// totalAlloc 返回进程累计分配字节数（单调递增，相减即区间分配量）。
func totalAlloc() uint64 {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	return stats.TotalAlloc
}

// chatDecodeAllocs 解一条纯增量流，返回累计分配字节数与客户端可见正文长度。
func chatDecodeAllocs(t *testing.T, parts int) (allocBytes uint64, textLen int) {
	t.Helper()
	frames := make([]string, 0, parts+1)
	for i := 0; i < parts; i++ {
		frames = append(frames, chatFrameDelta(chatTestParts[i%len(chatTestParts)], ""))
	}
	frames = append(frames, chatFrameEmpty())
	raw := []byte(strings.Join(frames, ""))

	decoder := newChatStreamDecoder(ConvertCtx{ClientFormat: FormatOpenAI, TargetProto: ProtocolOpenAIChat})
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

// TestChatStreamAccumulationIsLinear 钉住「正文累积必须是线性分配」。
//
// 为何要钉：v1.11.0 之前 emitted / messageText 用 `string +=` 累积，每帧把已收正文全量
// 拷一遍，整条流 O(正文长度²)。生产表现为输出越长每帧越慢、流到后段明显掉速。
// 判据取「分配字节 / 正文长度」的比值：线性实现下它随帧数近似不变，二次实现下随帧数线性增长。
func TestChatStreamAccumulationIsLinear(t *testing.T) {
	if testing.Short() {
		t.Skip("分配量随规模变化，短模式跳过")
	}
	smallBytes, smallLen := chatDecodeAllocs(t, 50)
	largeBytes, largeLen := chatDecodeAllocs(t, 2000)
	if smallLen == 0 || largeLen == 0 {
		t.Fatal("正文长度不得为零")
	}
	smallRatio := float64(smallBytes) / float64(smallLen)
	largeRatio := float64(largeBytes) / float64(largeLen)

	// 帧数放大 40 倍：线性实现的比值基本不变（缓冲扩容是摊还的），二次实现会增长数十倍。
	if largeRatio > smallRatio*2 {
		t.Fatalf("正文累积疑似二次：40 帧时 %.1f B/字节，400 帧时 %.1f B/字节（比值 %.2f，上限 2）",
			smallRatio, largeRatio, largeRatio/smallRatio)
	}
}
