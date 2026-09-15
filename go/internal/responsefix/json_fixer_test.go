package responsefix

import (
	"bytes"
	"encoding/json"
	"testing"
)

// 本文件的用例逐条对拍 Node 的 json-fixer.test.ts。

func newTestJSONFixer() JSONFixer {
	return JSONFixer{MaxDepth: 200, MaxSize: 1024 * 1024}
}

func TestJSONFixerPassesValidJSON(t *testing.T) {
	input := []byte(`{"a":1}`)

	result := newTestJSONFixer().Fix(input)

	if result.Applied {
		t.Fatalf("合法 JSON 不该被标记为已修复")
	}
	if !bytes.Equal(result.Data, input) {
		t.Fatalf("合法 JSON 应原样通过：got %q", result.Data)
	}
}

func TestJSONFixerClosesTruncatedStructures(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{name: "未闭合对象", input: `{"key":"value"`},
		{name: "未闭合数组", input: `[1, 2, 3`},
		{name: "未闭合字符串", input: `{"key":"val`},
		{name: "嵌套未闭合", input: `{"outer": {"inner": [1, 2`},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			result := newTestJSONFixer().Fix([]byte(item.input))
			if !result.Applied {
				t.Fatalf("截断 JSON 应被修复：input=%q", item.input)
			}
			if !json.Valid(result.Data) {
				t.Fatalf("修复结果应可解析：input=%q got=%q", item.input, result.Data)
			}
		})
	}
}

func TestJSONFixerRemovesTrailingCommas(t *testing.T) {
	for _, input := range []string{`{"a": 1,}`, `[1, 2,]`} {
		result := newTestJSONFixer().Fix([]byte(input))
		if !json.Valid(result.Data) {
			t.Fatalf("尾随逗号应被删除：input=%q got=%q", input, result.Data)
		}
	}
}

func TestJSONFixerFillsNullForDanglingColon(t *testing.T) {
	result := newTestJSONFixer().Fix([]byte(`{"key":`))

	if !result.Applied {
		t.Fatalf("冒号后缺值应被修复")
	}
	var decoded map[string]any
	if err := json.Unmarshal(result.Data, &decoded); err != nil {
		t.Fatalf("修复结果应可解析：%v", err)
	}
	if value, exists := decoded["key"]; !exists || value != nil {
		t.Fatalf(`期望 {"key":null}：got %s`, result.Data)
	}
}

func TestJSONFixerSkipsBeyondMaxDepth(t *testing.T) {
	input := []byte(`{"a":{"b":{"c":{"d":`)

	result := JSONFixer{MaxDepth: 3, MaxSize: 1024 * 1024}.Fix(input)

	if result.Applied {
		t.Fatalf("超过最大深度应保持原样")
	}
	if !bytes.Equal(result.Data, input) {
		t.Fatalf("超过最大深度应保持原样：got %q", result.Data)
	}
	if result.Details != detailRepairFailed {
		t.Fatalf("审计细节应为 %q：got %q", detailRepairFailed, result.Details)
	}
}

func TestJSONFixerSkipsBeyondMaxSize(t *testing.T) {
	input := []byte(`{"key":"very long value"}`)

	result := JSONFixer{MaxDepth: 200, MaxSize: 10}.Fix(input)

	if result.Applied {
		t.Fatalf("超过最大大小应保持原样")
	}
	if !bytes.Equal(result.Data, input) {
		t.Fatalf("超过最大大小应保持原样：got %q", result.Data)
	}
	if result.Details != detailExceededMaxSize {
		t.Fatalf("审计细节应为 %q：got %q", detailExceededMaxSize, result.Details)
	}
}

func TestJSONFixerLeavesRepairedButIllegalInputUntouched(t *testing.T) {
	// `{"a":1` 后跟一个无法通过补全修好的非法 token：补出来仍非法时不得交给客户端。
	input := []byte(`{"a":1} garbage`)

	result := newTestJSONFixer().Fix(input)

	if result.Applied {
		t.Fatalf("补全后仍非法时应保持原样")
	}
	if !bytes.Equal(result.Data, input) {
		t.Fatalf("补全后仍非法时应保持原样：got %q", result.Data)
	}
}

func TestJSONFixerKeepsBracesInsideStrings(t *testing.T) {
	result := newTestJSONFixer().Fix([]byte(`{"s":"a\"b","n":1`))

	if !result.Applied {
		t.Fatalf("含转义引号的截断 JSON 应被修复")
	}
	if !json.Valid(result.Data) {
		t.Fatalf("修复结果应可解析：got %q", result.Data)
	}
	var decoded map[string]any
	if err := json.Unmarshal(result.Data, &decoded); err != nil {
		t.Fatalf("修复结果应可解析：%v", err)
	}
	if decoded["s"] != `a"b` {
		t.Fatalf("字符串内容不该被改：got %v", decoded["s"])
	}
}

func TestJSONFixerCanFixPredicate(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "对象", input: `{"a":1}`, want: true},
		{name: "前导空白后的数组", input: "\n  [1]", want: true},
		{name: "裸字符串", input: `"text"`, want: false},
		{name: "空输入", input: "", want: false},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			if got := newTestJSONFixer().CanFix([]byte(item.input)); got != item.want {
				t.Fatalf("CanFix = %v，期望 %v", got, item.want)
			}
		})
	}
}
