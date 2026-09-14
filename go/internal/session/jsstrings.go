package session

import (
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// 本文件是 headers.go 依赖的 JS 语义小工具：UTF-16 码元计数/切片、宽松百分号解码、
// 有序头名遍历。分开成文件是因为它们的**共同理由**是「JS 与 Go 的字符串模型不同」，
// 而不是响应工件本身。

// sortedHeaderNames 按字典序给出头名（Go 的 map 无序，排序让落盘结果可复现）。
func sortedHeaderNames(headers map[string][]string) []string {
	names := make([]string, 0, len(headers))
	for name := range headers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// utf16Units 把字符串拆成 UTF-16 码元序列（JS 的 .length 与 .slice 的单位）。
//
// 为什么需要它：Node 的 `trimmed.length <= 8` 与 `slice(0, 4)` 单位是**码元**，一个增补平面
// 字符（如 emoji）占 2 个码元。按 Go 的 rune 计数会得出不同的长度判定，于是同一个 header 值
// 在一侧被判「短于 8 → 全遮」而另一侧「保留前后 4」。
func utf16Units(value string) []uint16 {
	if value == "" {
		return nil
	}
	return utf16.Encode([]rune(value))
}

// utf16Slice 按码元下标切片并编回字符串。
//
// 切在代理对中间时（Node 同样会这样切）用替换字符占位：JS 会留下一个孤立代理，序列化成
// JSON 时变成 U+FFFD，故这里直接给 U+FFFD——落盘结果与 Node 的 JSON 输出一致。
func utf16Slice(units []uint16, start, end int) string {
	if start < 0 {
		start = 0
	}
	if end > len(units) {
		end = len(units)
	}
	if start >= end {
		return ""
	}
	slice := units[start:end]
	var builder strings.Builder
	for index := 0; index < len(slice); {
		unit := slice[index]
		if unit >= 0xD800 && unit <= 0xDBFF && index+1 < len(slice) {
			next := slice[index+1]
			if next >= 0xDC00 && next <= 0xDFFF {
				builder.WriteRune(utf16.DecodeRune(rune(unit), rune(next)))
				index += 2
				continue
			}
		}
		if unit >= 0xD800 && unit <= 0xDFFF {
			builder.WriteRune(utf8.RuneError)
			index++
			continue
		}
		builder.WriteRune(rune(unit))
		index++
	}
	return builder.String()
}

// urlPathUnescape 是**宽松**的百分号解码：非法转义原样保留（JS 的 URLSearchParams/atob 语义），
// 与 Go 的 url.PathUnescape（遇非法转义直接报错）不同。
func urlPathUnescape(value string) (string, error) {
	if !strings.Contains(value, "%") {
		return value, nil
	}
	var builder strings.Builder
	builder.Grow(len(value))
	for index := 0; index < len(value); {
		if value[index] != '%' {
			builder.WriteByte(value[index])
			index++
			continue
		}
		if index+2 >= len(value) {
			builder.WriteByte(value[index])
			index++
			continue
		}
		high, okHigh := hexDigit(value[index+1])
		low, okLow := hexDigit(value[index+2])
		if !okHigh || !okLow {
			builder.WriteByte(value[index])
			index++
			continue
		}
		builder.WriteByte(high<<4 | low)
		index += 3
	}
	return builder.String(), nil
}

// hexDigit 解一个十六进制字符。
func hexDigit(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
}
