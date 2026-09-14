package store

import (
	"encoding/json"
	"testing"
)

// 钉住 providers.custom_headers 的容错解码口径，逐条对齐 Node 的
// applyProviderCustomHeaders（src/app/v1/_lib/proxy/forwarder.ts:262-273）：
// 整份不合法 → 视作「没有自定义头」；**单条非字符串值只跳过该条**，不让其余头一起失效。
//
// 为什么值得单测：列是 jsonb 且历史上无写入侧校验，真实库里可能有脏行；
// 若这里改成严格解码，一条脏值会让整个供应商的自定义头全部失效（静默）。

func TestDecodeCustomHeaders(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want map[string]string
	}{
		{"空原文", "", nil},
		{"JSON null", "null", nil},
		{"空对象", "{}", nil},
		{"数组", `["a"]`, nil},
		{"标量", `"x"`, nil},
		{"非法 JSON", `{`, nil},
		{"普通记录", `{"x-a":"1","x-b":"2"}`, map[string]string{"x-a": "1", "x-b": "2"}},
		{"合法头与非字符串值混用", `{"x-a":"1","x-n":7,"x-b":"2"}`, map[string]string{"x-a": "1", "x-b": "2"}},
		{"值全为非字符串", `{"x-n":7,"x-z":null}`, nil},
		{"空串值保留", `{"x-empty":""}`, map[string]string{"x-empty": ""}},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var raw json.RawMessage
			if testCase.raw != "" {
				raw = json.RawMessage(testCase.raw)
			}
			got := DecodeCustomHeaders(raw)
			if len(got) != len(testCase.want) {
				t.Fatalf("长度不符：got=%v want=%v", got, testCase.want)
			}
			for key, want := range testCase.want {
				if got[key] != want {
					t.Fatalf("%s = %q，期望 %q", key, got[key], want)
				}
			}
		})
	}
}
