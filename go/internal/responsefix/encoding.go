package responsefix

import (
	"bytes"
	"strings"
	"unicode/utf8"
)

// EncodingFixer 移植 Node 的 `EncodingFixer`（encoding-fixer.ts）。
//
// 修复三类坏字节，顺序固定：
//
//  1. BOM（UTF-8 的 EF BB BF 与 UTF-16 的 FE FF / FF FE）——上游把整个响应当文件写时会出现，
//     客户端拿到首个字符是 U+FEFF，JSON 解析会因「意外的 BOM」直接失败；
//  2. NUL 空字节——多出现在上游用定长缓冲回显正文时；
//  3. 非法 UTF-8——上游按错的编码切了多字节字符。
//
// 前两类是确定性删除；第三类是有损替换（见 doc.go 的边界说明）。
type EncodingFixer struct{}

// CanFix 判定是否存在需要修复的编码问题（Node 同名方法）。
func (EncodingFixer) CanFix(data []byte) bool {
	if hasUTF8BOM(data) || hasUTF16BOM(data) {
		return true
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return true
	}
	return !utf8.Valid(data)
}

// Fix 返回修复后的字节。未命中修复条件时原样返回且 Applied=false。
func (f EncodingFixer) Fix(input []byte) Result {
	if !f.CanFix(input) {
		return Result{Data: input}
	}

	stripped, bomDetail, bomStripped := stripBOM(input)
	withoutNulls, nullStripped := stripNullBytes(stripped)
	intermediate := withoutNulls
	changedByStrip := bomStripped || nullStripped

	// 经过 BOM/空字节清理后已是有效 UTF-8：不再做有损替换。
	if utf8.Valid(intermediate) {
		detail := bomDetail
		if detail == "" && nullStripped {
			detail = detailRemovedNullBytes
		}
		return Result{Data: intermediate, Applied: changedByStrip, Details: detail}
	}

	// 有损修复：非法序列替换为 U+FFFD 后重新编码，确保输出一定是有效 UTF-8。
	lossy := strings.ToValidUTF8(string(intermediate), "\uFFFD")
	return Result{Data: []byte(lossy), Applied: true, Details: detailLossyUTF8DecodeEncode}
}

// hasUTF8BOM 判定 UTF-8 BOM。
func hasUTF8BOM(data []byte) bool {
	return len(data) >= 3 && data[0] == 0xef && data[1] == 0xbb && data[2] == 0xbf
}

// hasUTF16BOM 判定 UTF-16 BOM（大小端各一）。
func hasUTF16BOM(data []byte) bool {
	if len(data) < 2 {
		return false
	}
	return (data[0] == 0xfe && data[1] == 0xff) || (data[0] == 0xff && data[1] == 0xfe)
}

// stripBOM 去 BOM，返回去后字节、审计细节与是否真的去过。
func stripBOM(data []byte) ([]byte, string, bool) {
	if hasUTF8BOM(data) {
		return data[3:], detailRemovedUTF8BOM, true
	}
	if hasUTF16BOM(data) {
		return data[2:], detailRemovedUTF16BOM, true
	}
	return data, "", false
}

// stripNullBytes 删除全部 NUL 空字节（Node 的 stripNullBytes 语义等价于「删掉所有 0x00」）。
func stripNullBytes(data []byte) ([]byte, bool) {
	first := bytes.IndexByte(data, 0)
	if first < 0 {
		return data, false
	}
	out := make([]byte, 0, len(data))
	out = append(out, data[:first]...)
	for _, b := range data[first+1:] {
		if b != 0 {
			out = append(out, b)
		}
	}
	return out, true
}
