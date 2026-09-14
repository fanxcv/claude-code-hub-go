package replay

import "testing"

// 分段契约：分段可任意，但拼接必须逐字节还原正文，且不得把一个 UTF-8 字符切在两段里。
func TestSplitAtSafeTextBoundaryNeverSplitsRune(t *testing.T) {
	const text = "가가ab😀가가"
	cases := []struct {
		name   string
		offset int
		max    int
		want   int
	}{
		{"起点落在 rune 内时回退到边界", 0, 4, 3},
		{"ASCII 段正常推进", 3, 4, 7},
		{"到文本结尾即返回长度", 0, len(text), len(text)},
		{"越界 offset 原样返回", len(text) + 5, 4, len(text) + 5},
	}
	for _, tc := range cases {
		if got := splitAtSafeTextBoundary(text, tc.offset, tc.max); got != tc.want {
			t.Fatalf("%s: got %d want %d", tc.name, got, tc.want)
		}
	}
}

func TestAppendChunksPreservesBytes(t *testing.T) {
	text := ""
	for i := 0; i < 200; i++ {
		text += "data: 가😀\n\n"
	}
	for _, max := range []int{4, 8, 64} {
		chunks := appendChunks(nil, text, max)
		joined := ""
		for _, chunk := range chunks {
			joined += chunk
		}
		if joined != text {
			t.Fatalf("max=%d 拼接不等于原文", max)
		}
	}
	if chunks := appendChunks(nil, "", 64); len(chunks) != 0 {
		t.Fatalf("空文本不应产生分段: %d", len(chunks))
	}
}
