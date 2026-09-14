package replay

import "unicode/utf8"

// splitAtSafeTextBoundary 返回把 text 切在 UTF-8 字符边界上的分段终点（对应
// replay-text.ts 的 splitAtSafeTextBoundary：不拆开代理对的语义——Go 以 rune 为单位
// 切，天然不拆代码点）。
//
// 上限单位不同（Node 是 UTF-16 码元，这里用字节）：同一正文的元素数理论上可与 Node
// 相差（非 BMP 字符占 2 码元时 Node 每 32K 元素切，这里每 64K 字节切）。元素内容不变、
// 顺序不变，读端把元素当不透明字符串拼接，故不影响逐字节一致。
func splitAtSafeTextBoundary(text string, offset, maxBytes int) int {
	if offset >= len(text) {
		return offset
	}
	end := offset + maxBytes
	if end >= len(text) {
		return len(text)
	}
	for end > offset && !utf8.RuneStart(text[end]) {
		end--
	}
	return end
}

// appendChunks 把 text 按 utf8 边界切成 ≤maxBytes 的小块追加到 dst，返回扩展后的切片。
func appendChunks(dst []string, text string, maxBytes int) []string {
	for offset := 0; offset < len(text); {
		end := splitAtSafeTextBoundary(text, offset, maxBytes)
		dst = append(dst, text[offset:end])
		offset = end
	}
	return dst
}
