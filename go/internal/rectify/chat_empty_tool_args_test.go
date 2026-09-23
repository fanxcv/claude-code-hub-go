package rectify

import "testing"

// TestNormalizeChatEmptyToolArgsReturnsOriginalSliceWhenNothingToDo 钉住零扰动：
// 没有空参数 tool_call 时必须返回**同一个**底层数组，不重新序列化。
func TestNormalizeChatEmptyToolArgsReturnsOriginalSliceWhenNothingToDo(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"function":{"name":"t","arguments":"{}"}}]}]}`)
	got := NormalizeChatEmptyToolArgs(body)
	if &got[0] != &body[0] {
		t.Fatalf("无空参数时应原样返回原切片，实际重新分配了新切片：%s", got)
	}
}

// TestNormalizeChatEmptyToolArgsHandlesMultipleToolCallsAndWhitespace 钉住多处改写与空白/转义保真：
// 只有那两字节被替换，冒号两侧的空白原样保留。
func TestNormalizeChatEmptyToolArgsHandlesMultipleToolCallsAndWhitespace(t *testing.T) {
	body := []byte("{\n  \"messages\": [\n    { \"role\": \"assistant\", \"tool_calls\": [\n" +
		"      { \"id\": \"c1\", \"type\": \"function\", \"function\" : { \"name\": \"t\", \"arguments\" : \"\" } },\n" +
		"      { \"id\": \"c2\", \"type\": \"function\", \"function\": { \"name\": \"u\", \"arguments\": \"\" } }\n" +
		"    ] },\n    { \"role\": \"user\", \"content\": \"\\u4e2d\" }\n  ]\n}")
	want := "{\n  \"messages\": [\n    { \"role\": \"assistant\", \"tool_calls\": [\n" +
		"      { \"id\": \"c1\", \"type\": \"function\", \"function\" : { \"name\": \"t\", \"arguments\" : \"{}\" } },\n" +
		"      { \"id\": \"c2\", \"type\": \"function\", \"function\": { \"name\": \"u\", \"arguments\": \"{}\" } }\n" +
		"    ] },\n    { \"role\": \"user\", \"content\": \"\\u4e2d\" }\n  ]\n}"
	if got := string(NormalizeChatEmptyToolArgs(body)); got != want {
		t.Fatalf("改写结果不符\n want = %s\n  got = %s", want, got)
	}
}

// TestNormalizeChatEmptyToolArgsInvariants 逐条钉住不改的样子：非空参数、无 tool_calls、
// arguments 非字符串、大整数与键序。这些字节必须原样出站。
func TestNormalizeChatEmptyToolArgsInvariants(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "arguments 已是 {}",
			body: `{"messages":[{"role":"assistant","tool_calls":[{"function":{"name":"t","arguments":"{}"}}]}]}`,
		},
		{
			name: "arguments 有内容",
			body: `{"messages":[{"role":"assistant","tool_calls":[{"function":{"name":"t","arguments":"{\"path\":\"/tmp\"}"}}]}]}`,
		},
		{
			name: "无 tool_calls",
			body: `{"messages":[{"role":"user","content":"ping"},{"role":"assistant","content":"pong"}]}`,
		},
		{
			name: "arguments 非字符串",
			body: `{"messages":[{"role":"assistant","tool_calls":[{"function":{"name":"t","arguments":{}}}]}]}`,
		},
		{
			name: "大整数与键序",
			body: `{"z_flag":true,"messages":[{"role":"assistant","tool_calls":[{"function":{"arguments":"{}","name":"t"}}]}],` +
				`"a_big":9007199254740993,"m_nested":{"k1":1e21,"k2":"\u003cu\u003e"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(NormalizeChatEmptyToolArgs([]byte(tc.body))); got != tc.body {
				t.Fatalf("正文应逐字节不变\n want = %s\n  got = %s", tc.body, got)
			}
		})
	}
}

// TestNormalizeChatEmptyToolArgsIgnoresMalformedBody 钉住不新增失败模式：
// 正文不是合法 JSON / 结构对不上时原样返回。
func TestNormalizeChatEmptyToolArgsIgnoresMalformedBody(t *testing.T) {
	for _, body := range []string{
		``,
		`not json`,
		`{"messages":[{"role":"assistant","tool_calls":[{"function":{"arguments":""}}]}`,
		`{"messages":{"role":"assistant"}}`,
		`[{"messages":[]}]`,
	} {
		if got := string(NormalizeChatEmptyToolArgs([]byte(body))); got != body {
			t.Fatalf("正文 %q 应原样返回，实际 %q", body, got)
		}
	}
}

// TestNormalizeChatEmptyToolArgsRejectsTrailingGarbage 钉住闸门：多值流与尾随垃圾不是单一完整
// JSON 文档，即使能分词成功也一律不改。
func TestNormalizeChatEmptyToolArgsRejectsTrailingGarbage(t *testing.T) {
	for _, body := range []string{
		`{"messages":[{"tool_calls":[{"function":{"arguments":""}}]}]} trailing`,
		`{"messages":[{"tool_calls":[{"function":{"arguments":""}}]}]}{"a":1}`,
	} {
		if got := string(NormalizeChatEmptyToolArgs([]byte(body))); got != body {
			t.Fatalf("正文 %q 应原样返回，实际 %q", body, got)
		}
	}
}

// TestNormalizeChatEmptyToolArgsIgnoresInvalidNestedArguments 钉住刻意边界：arguments 非空但本身
// 是坏 JSON 时不改写（不替客户端补 JSON）。
func TestNormalizeChatEmptyToolArgsIgnoresInvalidNestedArguments(t *testing.T) {
	body := `{"messages":[{"tool_calls":[{"function":{"arguments":"{\"a\":"}}]}]}`
	if got := string(NormalizeChatEmptyToolArgs([]byte(body))); got != body {
		t.Fatalf("正文应原样返回，实际 %q", got)
	}
}
