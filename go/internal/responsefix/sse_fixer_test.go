package responsefix

import (
	"strings"
	"testing"
)

// 本文件的用例逐条对拍 Node 的 sse-fixer.test.ts。

func fixSSEString(t *testing.T, input string) string {
	t.Helper()
	result := (SSEFixer{}).Fix([]byte(input))
	return string(result.Data)
}

func TestSSEFixerPassesValidFrames(t *testing.T) {
	input := "data: {\"test\": true}\n"
	if got := fixSSEString(t, input); got != input {
		t.Fatalf("有效 SSE 应原样通过：got %q", got)
	}
}

func TestSSEFixerReusesInputWhenUnchanged(t *testing.T) {
	input := []byte("data: {\"test\": true}\n")

	result := (SSEFixer{}).Fix(input)

	if result.Applied {
		t.Fatalf("有效 SSE 不该标记为已修复")
	}
	if &result.Data[0] != &input[0] {
		t.Fatalf("未变更时应复用输入切片（Node 的同引用断言）")
	}
}

func TestSSEFixerAddsMissingSpaceAfterData(t *testing.T) {
	if got, want := fixSSEString(t, "data:{\"test\": true}\n"), "data: {\"test\": true}\n"; got != want {
		t.Fatalf("data: 后缺空格应补齐：got %q want %q", got, want)
	}
}

func TestSSEFixerHandlesVeryLongDataLine(t *testing.T) {
	payload := strings.Repeat("a", 100_000)

	got := fixSSEString(t, "data:"+payload+"\n")

	if got != "data: "+payload+"\n" {
		t.Fatalf("超长 data 行应可处理（got %d 字节）", len(got))
	}
}

func TestSSEFixerWrapsBareJSONLines(t *testing.T) {
	if got, want := fixSSEString(t, "{\"content\": \"hello\"}\n"), "data: {\"content\": \"hello\"}\n"; got != want {
		t.Fatalf("裸 JSON 行应补 data: 前缀：got %q", got)
	}
	if got, want := fixSSEString(t, "[{\"delta\": {}}]\n"), "data: [{\"delta\": {}}]\n"; got != want {
		t.Fatalf("裸 JSON 数组行应补 data: 前缀：got %q", got)
	}
	if got, want := fixSSEString(t, "[DONE]\n"), "data: [DONE]\n"; got != want {
		t.Fatalf("[DONE] 应补 data: 前缀：got %q", got)
	}
}

func TestSSEFixerKeepsCommentLines(t *testing.T) {
	input := ": this is a comment\ndata: test\n"
	if got := fixSSEString(t, input); got != input {
		t.Fatalf("注释行应保留：got %q", got)
	}
}

func TestSSEFixerFixesFieldSpacing(t *testing.T) {
	cases := []struct{ input, want string }{
		{"event:message\ndata: test\n", "event: message\ndata: test\n"},
		{"id:123\ndata: test\n", "id: 123\ndata: test\n"},
		{"retry:1000\ndata: test\n", "retry: 1000\ndata: test\n"},
	}
	for _, item := range cases {
		if got := fixSSEString(t, item.input); got != item.want {
			t.Fatalf("字段空格应修复：input=%q got %q want %q", item.input, got, item.want)
		}
	}
}

func TestSSEFixerNormalizesLineEndings(t *testing.T) {
	if got, want := fixSSEString(t, "data: test\r\ndata: test2\r\n"), "data: test\ndata: test2\n"; got != want {
		t.Fatalf("CRLF 应归一为 LF：got %q", got)
	}
	if got, want := fixSSEString(t, "data: test\rdata: test2\r"), "data: test\ndata: test2\n"; got != want {
		t.Fatalf("裸 CR 应归一为 LF：got %q", got)
	}
}

func TestSSEFixerLowercasesDataPrefix(t *testing.T) {
	if got, want := fixSSEString(t, "Data:{\"test\": true}\n"), "data: {\"test\": true}\n"; got != want {
		t.Fatalf("Data: 应归一为小写：got %q", got)
	}
	if got, want := fixSSEString(t, "DATA:{\"test\": true}\n"), "data: {\"test\": true}\n"; got != want {
		t.Fatalf("DATA: 应归一为小写：got %q", got)
	}
}

func TestSSEFixerFixesDataSpaceVariant(t *testing.T) {
	if got, want := fixSSEString(t, "data :{\"test\": true}\n"), "data: {\"test\": true}\n"; got != want {
		t.Fatalf("`data :` 应被修复：got %q", got)
	}
}

func TestSSEFixerCollapsesConsecutiveBlankLines(t *testing.T) {
	if got, want := fixSSEString(t, "data: test\n\n\n\ndata: test2\n"), "data: test\n\ndata: test2\n"; got != want {
		t.Fatalf("连续空行应折叠为一个：got %q", got)
	}
}

func TestSSEFixerKeepsMultiLineData(t *testing.T) {
	input := "data: line1\ndata: line2\n\n"
	if got := fixSSEString(t, input); got != input {
		t.Fatalf("多行 data 应保持：got %q", got)
	}
}

func TestSSEFixerAppendsTrailingNewline(t *testing.T) {
	// 末尾缺换行：Node 的历史行为是补一个 LF，且必须标记为已变更。
	got := fixSSEString(t, "data: test")
	if got != "data: test\n" {
		t.Fatalf("末尾缺换行应补 LF：got %q", got)
	}
}

func TestSSEFixerCanFixPredicate(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "data 前缀", input: "data: x", want: true},
		{name: "event 前缀", input: "event: x", want: true},
		{name: "带空格的 data", input: "data : x", want: true},
		{name: "大小写错误", input: "Data:x", want: true},
		{name: "裸 JSON", input: `{"a":1}`, want: true},
		{name: "纯文本", input: "hello", want: false},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if got := (SSEFixer{}).CanFix([]byte(item.input)); got != item.want {
				t.Fatalf("CanFix = %v，期望 %v", got, item.want)
			}
		})
	}
}
