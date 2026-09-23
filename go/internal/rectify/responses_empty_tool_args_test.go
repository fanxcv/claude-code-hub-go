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

// TestNormalizeResponsesEmptyToolArgsRejectsTrailingGarbage 钉住闸门：json.Decoder 是流式分词器，
// 多值流与尾随垃圾都能分词成功，但正文并非单一完整 JSON 文档，故一律不改。
func TestNormalizeResponsesEmptyToolArgsRejectsTrailingGarbage(t *testing.T) {
	for _, body := range []string{
		`{"input":[{"type":"function_call","arguments":""}]} trailing`,
		`{"input":[{"type":"function_call","arguments":""}]}{"a":1}`,
	} {
		if got := string(NormalizeResponsesEmptyToolArgs([]byte(body))); got != body {
			t.Fatalf("正文 %q 应原样返回，实际 %q", body, got)
		}
	}
}

// TestNormalizeResponsesEmptyToolArgsIgnoresOtherShapes 钉住刻意边界：arguments 缺失、以及
// arguments 非空但本身是坏 JSON 时都不补写（不替客户端修 JSON）。
func TestNormalizeResponsesEmptyToolArgsIgnoresOtherShapes(t *testing.T) {
	for _, body := range []string{
		`{"input":[{"type":"function_call","name":"t"}]}`,
		`{"input":[{"type":"function_call","arguments":"{\"a\":"}]}`,
	} {
		if got := string(NormalizeResponsesEmptyToolArgs([]byte(body))); got != body {
			t.Fatalf("正文 %q 应原样返回，实际 %q", body, got)
		}
	}
}

// TestNormalizeResponsesEmptyToolArgsDuplicateArgumentsKey 钉住同键重复时以最后一个为准：
// 后出现的非空串要清掉先前候选（前者被遮蔽，不该改）；后出现的空串才改写。
func TestNormalizeResponsesEmptyToolArgsDuplicateArgumentsKey(t *testing.T) {
	shadowed := `{"input":[{"type":"function_call","arguments":"","arguments":"x"}]}`
	if got := string(NormalizeResponsesEmptyToolArgs([]byte(shadowed))); got != shadowed {
		t.Fatalf("被遮蔽的空串不该改\n want = %s\n  got = %s", shadowed, got)
	}
	lastEmpty := `{"input":[{"type":"function_call","arguments":"x","arguments":""}]}`
	want := `{"input":[{"type":"function_call","arguments":"x","arguments":"{}"}]}`
	if got := string(NormalizeResponsesEmptyToolArgs([]byte(lastEmpty))); got != want {
		t.Fatalf("最后一个空串应改成 {}\n want = %s\n  got = %s", want, got)
	}
}
