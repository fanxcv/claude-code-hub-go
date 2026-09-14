package route

import (
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

func TestTargetTypeOf(t *testing.T) {
	cases := map[convert.ClientFormat]TargetType{
		convert.FormatClaude:    TargetClaude,
		convert.FormatResponse:  TargetCodex,
		convert.FormatOpenAI:    TargetOpenAICompatible,
		convert.FormatGemini:    TargetGemini,
		convert.FormatGeminiCLI: TargetGeminiCLI,
		"":                      TargetClaude,
		"unknown":               TargetClaude,
	}
	for format, want := range cases {
		if got := TargetTypeOf(format); got != want {
			t.Errorf("TargetTypeOf(%q) = %q，期望 %q", format, got, want)
		}
	}
}

func TestClampUnit(t *testing.T) {
	cases := map[float64]float64{-1: 0, 0: 0, 0.5: 0.5, 1: 0.9999999999999999}
	for input, want := range cases {
		if got := clampUnit(input); got != want {
			t.Errorf("clampUnit(%v) = %v，期望 %v", input, got, want)
		}
	}
}

func TestDigestMediaSource(t *testing.T) {
	if got := digestMediaSource(nil); got != "" {
		t.Errorf("nil source 应为空串，实际 %q", got)
	}
	first := digestMediaSource(map[string]any{"media_type": "image/png", "data": "AAAA"})
	second := digestMediaSource(map[string]any{"media_type": "image/png", "data": "BBBB"})
	if first == second {
		t.Errorf("同长异图不应碰撞（digest 取内容摘要而非长度）")
	}
	if first == "" || first[:10] != "image/png:" {
		t.Errorf("摘要应带媒体类型前缀，实际 %q", first)
	}
	byURL := digestMediaSource(map[string]any{"mediaType": "image/jpeg", "url": "https://x/y.jpg"})
	if byURL != "image/jpeg:https://x/y.jpg" {
		t.Errorf("URL 形态应直出 url，实际 %q", byURL)
	}
}

func TestNormalizeContentBlockVariants(t *testing.T) {
	cases := []struct {
		name  string
		block any
		want  string
	}{
		{"字符串块", "text", sep + "text:text"},
		{"文本块", map[string]any{"type": "text", "text": "hi"}, sep + "text:hi"},
		{"thinking", map[string]any{"type": "thinking", "thinking": "hmm"}, sep + "thinking:hmm"},
		{
			"redacted_thinking",
			map[string]any{"type": "redacted_thinking", "data": "opaque"},
			sep + "redacted_thinking:opaque",
		},
		{
			"tool_result",
			map[string]any{"type": "tool_result", "content": "ok"},
			sep + "tool_result:ok",
		},
		{"未知类型无 type 字段", map[string]any{"foo": "bar"}, ""},
		{"非对象非字符串", 42, ""},
	}
	for _, testCase := range cases {
		if got := normalizeContentBlock(testCase.block); got != testCase.want {
			t.Errorf("%s：normalizeContentBlock = %q，期望 %q", testCase.name, got, testCase.want)
		}
	}

	document := map[string]any{
		"type": "document",
		"source": map[string]any{
			"media_type": "application/pdf",
			"data":       "QQ==",
		},
	}
	if got := normalizeContentBlock(document); len(got) == 0 {
		t.Errorf("document 块应产出摘要")
	}

	// 未知类型带 type：走 stripVolatileKeys 的兜底分支，id 属易变键必须被剥掉。
	unknown := map[string]any{"type": "custom", "id": "volatile", "payload": "stable"}
	first := normalizeContentBlock(unknown)
	unknown["id"] = "changed"
	second := normalizeContentBlock(unknown)
	if first != second {
		t.Errorf("兜底分支应剥掉 id 等易变键：%q vs %q", first, second)
	}
}

func TestExtractResponsesVariants(t *testing.T) {
	stringInput := map[string]any{"input": "hello"}
	chain, ok := Fingerprint(stringInput, convert.FormatResponse, 8)
	if !ok || len(chain.Tail) != 1 {
		t.Fatalf("字符串 input 应产出 1 条会话消息")
	}

	items := map[string]any{"input": []any{
		map[string]any{"type": "function_call", "name": "f", "arguments": "{}", "call_id": "volatile"},
		map[string]any{"type": "function_call_output", "output": "done"},
		map[string]any{"type": "reasoning", "content": "think"},
		map[string]any{"type": "custom_item", "id": "volatile", "payload": 1},
		map[string]any{"role": "user", "content": "no type field"},
	}}
	chain, ok = Fingerprint(items, convert.FormatResponse, 8)
	if !ok || len(chain.Tail) != 5 {
		t.Fatalf("五类 item 都应产出会话消息，实际 %d", len(chain.Tail))
	}

	unsupported := map[string]any{"input": 42}
	if _, ok := Fingerprint(unsupported, convert.FormatResponse, 8); ok {
		t.Errorf("非字符串非数组的 input 应不可指纹化")
	}
}

func TestSerializeUnknownContent(t *testing.T) {
	if got := serializeUnknownContent(nil); got != "" {
		t.Errorf("nil 应为空串，实际 %q", got)
	}
	if got := serializeUnknownContent("raw"); got != "raw" {
		t.Errorf("字符串应原样返回，实际 %q", got)
	}
	if got := serializeUnknownContent(map[string]any{"b": 1.0, "a": 2.0}); got != `{"a":2,"b":1}` {
		t.Errorf("对象应按键序稳定序列化，实际 %q", got)
	}
	if got := serializeUnknownContent([]any{1.0, "x"}); got != `[1,"x"]` {
		t.Errorf("数组应保序，实际 %q", got)
	}
}
