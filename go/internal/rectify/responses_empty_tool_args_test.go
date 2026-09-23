package rectify

import "testing"

// TestNormalizeResponsesEmptyToolArgsReturnsOriginalSliceWhenNothingToDo 钉住零扰动：
// 没有空参数项时必须返回**同一个**底层数组，不重新序列化。
func TestNormalizeResponsesEmptyToolArgsReturnsOriginalSliceWhenNothingToDo(t *testing.T) {
	body := []byte(`{"input":[{"type":"function_call","name":"t","arguments":"{}"}],"a":1}`)
	got := NormalizeResponsesEmptyToolArgs(body)
	if &got[0] != &body[0] {
		t.Fatalf("无空参数项时应原样返回原切片，实际重新分配了新切片：%s", got)
	}
}

// TestNormalizeResponsesEmptyToolArgsHandlesMultipleItemsAndWhitespace 钉住多处改写与
// 空白/转义保真：只有那两字节被替换。
func TestNormalizeResponsesEmptyToolArgsHandlesMultipleItemsAndWhitespace(t *testing.T) {
	body := []byte("{\n  \"input\": [\n    { \"type\": \"function_call\", \"arguments\" : \"\" },\n" +
		"    { \"type\": \"function_call\", \"arguments\": \"\" },\n" +
		"    { \"type\": \"message\", \"content\": \"\\u4e2d\" }\n  ]\n}")
	want := "{\n  \"input\": [\n    { \"type\": \"function_call\", \"arguments\" : \"{}\" },\n" +
		"    { \"type\": \"function_call\", \"arguments\": \"{}\" },\n" +
		"    { \"type\": \"message\", \"content\": \"\\u4e2d\" }\n  ]\n}"
	if got := string(NormalizeResponsesEmptyToolArgs(body)); got != want {
		t.Fatalf("改写结果不符\n want = %s\n  got = %s", want, got)
	}
}

// TestNormalizeResponsesEmptyToolArgsIgnoresMalformedBody 钉住不新增失败模式：
// 正文不是合法 JSON / 结构对不上时原样返回。
func TestNormalizeResponsesEmptyToolArgsIgnoresMalformedBody(t *testing.T) {
	for _, body := range []string{
		``,
		`not json`,
		`{"input":[{"type":"function_call","arguments":""}`,
		`{"input":{"type":"function_call","arguments":""}}`,
		`[{"type":"function_call","arguments":""}]`,
	} {
		if got := string(NormalizeResponsesEmptyToolArgs([]byte(body))); got != body {
			t.Fatalf("正文 %q 应原样返回，实际 %q", body, got)
		}
	}
}
