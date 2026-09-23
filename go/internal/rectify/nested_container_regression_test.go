package rectify

import "testing"

// 本文件钉住一处既有缺陷的修复：旧扫描器按固定层深判定目标，且读到一个**容器值**后不把父对象的
// 「下一个 token 是键」状态复位，于是「容器值之后还有键」的对象会让其后所有路径判定错位——
// 表现是整条归一被**静默跳过**（不是误改）。泛化实现按整条 JSON 路径判定，必须仍命中。

func TestNormalizeResponsesEmptyToolArgsAfterNestedContainer(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "顶层 input 之前有容器值",
			body: `{"a":{"x":1},"input":[{"type":"function_call","arguments":""}]}`,
			want: `{"a":{"x":1},"input":[{"type":"function_call","arguments":"{}"}]}`,
		},
		{
			name: "同一对象内 arguments 之前有容器值",
			body: `{"input":[{"extra":{"a":1},"type":"function_call","arguments":""}]}`,
			want: `{"input":[{"extra":{"a":1},"type":"function_call","arguments":"{}"}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(NormalizeResponsesEmptyToolArgs([]byte(tc.body))); got != tc.want {
				t.Fatalf("容器值之后的空参数应被归一\n want = %s\n  got = %s", tc.want, got)
			}
		})
	}
}

func TestNormalizeChatEmptyToolArgsAfterNestedContainer(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "顶层 messages 之前有容器值",
			body: `{"metadata":{"k":1},"messages":[{"role":"assistant","tool_calls":[{"function":{"name":"t","arguments":""}}]}]}`,
			want: `{"metadata":{"k":1},"messages":[{"role":"assistant","tool_calls":[{"function":{"name":"t","arguments":"{}"}}]}]}`,
		},
		{
			name: "同一 message 内 tool_calls 之前有容器值",
			body: `{"messages":[{"role":"assistant","extra":{"a":1},"tool_calls":[{"function":{"name":"t","arguments":""}}]}]}`,
			want: `{"messages":[{"role":"assistant","extra":{"a":1},"tool_calls":[{"function":{"name":"t","arguments":"{}"}}]}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(NormalizeChatEmptyToolArgs([]byte(tc.body))); got != tc.want {
				t.Fatalf("容器值之后的空参数应被归一\n want = %s\n  got = %s", tc.want, got)
			}
		})
	}
}
