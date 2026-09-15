package rectify

import (
	"reflect"
	"testing"
)

// billing header：Node 无独立测试文件，用例按 billing-header-rectifier.ts 的三态语义自造。
func TestStripBillingHeaderStringForm(t *testing.T) {
	body := mustParse(t, `{"model":"claude","system":"  x-anthropic-billing-header: abc","messages":[]}`)

	fields, applied := StripBillingHeader(body)

	if !applied {
		t.Fatal("纯字符串命中时应整体删除")
	}
	if intField(t, fields, "removedCount") != 1 {
		t.Errorf("removedCount 不对: %#v", fields)
	}
	if got, want := fields["extractedValues"], []string{"x-anthropic-billing-header: abc"}; !reflect.DeepEqual(got, want) {
		t.Errorf("extractedValues 不对: %#v", got)
	}
	if got, want := compact(t, body), `{"model":"claude","messages":[]}`; got != want {
		t.Errorf("正文不对:\n got %s\nwant %s", got, want)
	}
}

func TestStripBillingHeaderArrayForm(t *testing.T) {
	body := mustParse(t, `{"system":[{"type":"text","text":"keep me"},{"type":"text","text":"x-anthropic-billing-header: v1"},{"type":"text","text":"  X-Anthropic-Billing-Header : v2"}],"messages":[]}`)

	fields, applied := StripBillingHeader(body)

	if !applied {
		t.Fatal("数组内命中时应过滤掉命中块")
	}
	if intField(t, fields, "removedCount") != 2 {
		t.Errorf("removedCount 不对: %#v", fields)
	}
	want := []string{"x-anthropic-billing-header: v1", "X-Anthropic-Billing-Header : v2"}
	if got := fields["extractedValues"]; !reflect.DeepEqual(got, want) {
		t.Errorf("extractedValues 不对: %#v", got)
	}
	if got, wantBody := compact(t, body), `{"system":[{"type":"text","text":"keep me"}],"messages":[]}`; got != wantBody {
		t.Errorf("正文不对:\n got %s\nwant %s", got, wantBody)
	}
}

func TestStripBillingHeaderNonMatchingAndUnknownShapes(t *testing.T) {
	cases := []string{
		// 字符串但不像 billing header
		`{"system":"you are a helpful assistant"}`,
		// 数组但块不是 text
		`{"system":[{"type":"image","source":{"data":"x-anthropic-billing-header: v"}}]}`,
		// 数组但文本不以该头开头（不是行首匹配）
		`{"system":[{"type":"text","text":"see x-anthropic-billing-header: v"}]}`,
		// system 缺失
		`{"messages":[]}`,
		// system 为 null
		`{"system":null}`,
		// system 为其他类型
		`{"system":{"type":"text","text":"x-anthropic-billing-header: v"}}`,
	}
	for _, fixture := range cases {
		body := mustParse(t, fixture)
		fields, applied := StripBillingHeader(body)
		if applied {
			t.Errorf("不应整流: %s", fixture)
		}
		if intField(t, fields, "removedCount") != 0 {
			t.Errorf("removedCount 应为零: %s → %#v", fixture, fields)
		}
		if got := compact(t, body); got != fixture {
			t.Errorf("正文被改动: %s → %s", fixture, got)
		}
	}
}

// responses input 归一：Node 无独立测试文件，用例按 response-input-rectifier.ts 的五态语义自造。
func TestNormalizeResponseInput(t *testing.T) {
	cases := []struct {
		name         string
		input        any
		wantApplied  bool
		wantAction   string
		wantOriginal string
		wantInput    any
	}{
		{
			name:         "字符串归一为 user/input_text",
			input:        "hello",
			wantApplied:  true,
			wantAction:   "string_to_array",
			wantOriginal: "string",
			wantInput:    []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hello"}}}},
		},
		{
			name:         "空字符串归一为空数组",
			input:        "",
			wantApplied:  true,
			wantAction:   "empty_string_to_empty_array",
			wantOriginal: "string",
			wantInput:    []any{},
		},
		{
			name:         "含 role 的单对象包装成数组",
			input:        map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}},
			wantApplied:  true,
			wantAction:   "object_to_array",
			wantOriginal: "object",
			wantInput:    []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "hi"}}}},
		},
		{
			name:         "含 type 的单对象（工具输出）包装成数组",
			input:        map[string]any{"type": "function_call_output", "call_id": "c1", "output": "ok"},
			wantApplied:  true,
			wantAction:   "object_to_array",
			wantOriginal: "object",
			wantInput:    []any{map[string]any{"type": "function_call_output", "call_id": "c1", "output": "ok"}},
		},
		{
			name:         "数组原样透传",
			input:        []any{map[string]any{"role": "user"}},
			wantApplied:  false,
			wantAction:   "passthrough",
			wantOriginal: "array",
			wantInput:    []any{map[string]any{"role": "user"}},
		},
		{
			name:         "既无 role 也无 type 的对象透传",
			input:        map[string]any{"foo": "bar"},
			wantApplied:  false,
			wantAction:   "passthrough",
			wantOriginal: "object",
			wantInput:    map[string]any{"foo": "bar"},
		},
		{
			name:         "null 透传",
			input:        nil,
			wantApplied:  false,
			wantAction:   "passthrough",
			wantOriginal: "other",
			wantInput:    nil,
		},
	}

	for _, testCase := range cases {
		body := map[string]any{"input": testCase.input}
		fields, applied := NormalizeResponseInput(body)

		if applied != testCase.wantApplied {
			t.Errorf("%s：applied=%v，期望 %v", testCase.name, applied, testCase.wantApplied)
		}
		if fields["action"] != testCase.wantAction || fields["originalType"] != testCase.wantOriginal {
			t.Errorf("%s：审计字段不对: %#v", testCase.name, fields)
		}
		if got := body["input"]; !reflect.DeepEqual(got, testCase.wantInput) {
			t.Errorf("%s：input 不对\n got %#v\nwant %#v", testCase.name, got, testCase.wantInput)
		}
	}
}

func TestNormalizeResponseInputMissingInputIsNoOp(t *testing.T) {
	body := map[string]any{"model": "gpt-5"}

	fields, applied := NormalizeResponseInput(body)

	if applied || fields["action"] != "passthrough" || fields["originalType"] != "other" {
		t.Errorf("缺 input 时应透传: applied=%v fields=%#v", applied, fields)
	}
}
