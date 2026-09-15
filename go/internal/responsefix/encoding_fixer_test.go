package responsefix

import (
	"bytes"
	"testing"
	"unicode/utf8"
)

// 本文件的用例逐条对拍 Node 的 encoding-fixer.test.ts。

func TestEncodingFixerPassesValidUTF8(t *testing.T) {
	input := []byte("Hello 世界")

	result := EncodingFixer{}.Fix(input)

	if result.Applied {
		t.Fatalf("有效 UTF-8 不该被标记为已修复")
	}
	if !bytes.Equal(result.Data, input) {
		t.Fatalf("有效 UTF-8 应原样通过：got %q", result.Data)
	}
}

func TestEncodingFixerRemovesUTF8BOM(t *testing.T) {
	input := append([]byte{0xef, 0xbb, 0xbf}, []byte("Hello")...)

	result := EncodingFixer{}.Fix(input)

	if !result.Applied {
		t.Fatalf("去 UTF-8 BOM 应标记为已修复")
	}
	if string(result.Data) != "Hello" {
		t.Fatalf("去 BOM 后应为 Hello：got %q", result.Data)
	}
	if result.Details != detailRemovedUTF8BOM {
		t.Fatalf("审计细节应为 %q：got %q", detailRemovedUTF8BOM, result.Details)
	}
}

func TestEncodingFixerRemovesUTF16BOM(t *testing.T) {
	// UTF-16LE BOM + "A"（0x41 0x00）。
	input := []byte{0xff, 0xfe, 0x41, 0x00}

	result := EncodingFixer{}.Fix(input)

	if !result.Applied {
		t.Fatalf("去 UTF-16 BOM 应标记为已修复")
	}
	if string(result.Data) != "A" {
		t.Fatalf("去 BOM 后应为 A：got %q", result.Data)
	}
	if result.Details != detailRemovedUTF16BOM {
		t.Fatalf("审计细节应为 %q：got %q", detailRemovedUTF16BOM, result.Details)
	}
}

func TestEncodingFixerRemovesNullBytes(t *testing.T) {
	input := []byte{0x48, 0x65, 0x00, 0x6c, 0x6c, 0x6f} // He\0llo

	result := EncodingFixer{}.Fix(input)

	if !result.Applied {
		t.Fatalf("去空字节应标记为已修复")
	}
	if string(result.Data) != "Hello" {
		t.Fatalf("去空字节后应为 Hello：got %q", result.Data)
	}
	if result.Details != detailRemovedNullBytes {
		t.Fatalf("审计细节应为 %q：got %q", detailRemovedNullBytes, result.Details)
	}
}

func TestEncodingFixerRepairsInvalidUTF8Lossily(t *testing.T) {
	// 0xC3 0x28 是非法 UTF-8 序列（0xC3 后跟的不是续接字节）。
	input := []byte{0xc3, 0x28, 0x61}

	result := EncodingFixer{}.Fix(input)

	if !result.Applied {
		t.Fatalf("非法 UTF-8 应被有损修复")
	}
	if !utf8.Valid(result.Data) {
		t.Fatalf("有损修复的输出必须是合法 UTF-8：got %v", result.Data)
	}
	if result.Details != detailLossyUTF8DecodeEncode {
		t.Fatalf("审计细节应为 %q：got %q", detailLossyUTF8DecodeEncode, result.Details)
	}
	if string(result.Data) != "\uFFFD(a" {
		t.Fatalf("有损修复应把非法序列替换为 U+FFFD：got %q", result.Data)
	}
}

func TestEncodingFixerCanFixPredicate(t *testing.T) {
	cases := []struct {
		name  string
		input []byte
		want  bool
	}{
		{name: "有效 UTF-8", input: []byte("plain"), want: false},
		{name: "UTF-8 BOM", input: []byte{0xef, 0xbb, 0xbf, 'a'}, want: true},
		{name: "NUL 字节", input: []byte{'a', 0x00, 'b'}, want: true},
		{name: "非法 UTF-8", input: []byte{0xc3, 0x28}, want: true},
		{name: "空输入", input: nil, want: false},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if got := (EncodingFixer{}).CanFix(item.input); got != item.want {
				t.Fatalf("CanFix = %v，期望 %v", got, item.want)
			}
		})
	}
}
