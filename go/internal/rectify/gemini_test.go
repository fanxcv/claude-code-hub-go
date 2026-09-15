package rectify

import "testing"

// 夹具取自 gemini-function-id-rectifier.ts:15-24 的文档注释（Vertex 真实报错原文）。
func TestDetectGeminiFunctionID(t *testing.T) {
	matches := []string{
		`Invalid JSON payload received. Unknown name "id" at 'contents[1].parts[0].function_call': Cannot find field.`,
		`Invalid JSON payload received. Unknown name "id" at 'contents[2].parts[0].function_response': Cannot find field.`,
		`Invalid JSON payload received. Unknown name "id" at contents[0].parts[0].functionCall: Cannot find field.`,
		`Provider returned 400: Upstream: {"error":{"message":"Invalid JSON payload received. Unknown name \"id\" at 'contents[1].parts[0].function_call': Cannot find field."}}`,
	}
	for _, message := range matches {
		if got := detectGeminiFunctionID(message); got != "unknown_function_id_field" {
			t.Errorf("命中失败: %q → %q", message, got)
		}
	}

	misses := []string{
		"",
		// 路径段必须**整段**匹配：function_calling_config 含 function_call 子串，但不该命中。
		`Invalid JSON payload received. Unknown name "id" at 'tool_config.function_calling_config': Cannot find field.`,
		`Invalid JSON payload received. Unknown name "temperature" at 'contents[1].parts[0].function_call': Cannot find field.`,
		`Invalid JSON payload received. Unknown name "id" at 'contents[1].parts[0].text': Cannot find field.`,
	}
	for _, message := range misses {
		if got := detectGeminiFunctionID(message); got != "" {
			t.Errorf("不应命中: %q → %q", message, got)
		}
	}
}

func TestRectifyGeminiFunctionIDsStripsBothShapesAndKeyStyles(t *testing.T) {
	body := mustParse(t, `{"contents":[{"parts":[{"functionCall":{"name":"f","args":{},"id":"call_1"}},{"functionResponse":{"name":"f","response":{},"id":"resp_1"}},{"function_call":{"name":"g","id":"call_2"}},{"function_response":{"name":"g","id":"resp_2"}},{"text":"keep","thoughtSignature":"sig"}]}],"request":{"contents":[{"parts":[{"functionCall":{"name":"h","id":"call_3"}}]}]}}`)

	fields, applied := rectifyGeminiFunctionIDs(body)

	if !applied {
		t.Fatal("应判定为已应用")
	}
	if intField(t, fields, "strippedFunctionCallIds") != 3 || intField(t, fields, "strippedFunctionResponseIds") != 2 {
		t.Errorf("计数不对: %#v", fields)
	}
	want := `{"contents":[{"parts":[{"functionCall":{"name":"f","args":{}}},{"functionResponse":{"name":"f","response":{}}},{"function_call":{"name":"g"}},{"function_response":{"name":"g"}},{"text":"keep","thoughtSignature":"sig"}]}],"request":{"contents":[{"parts":[{"functionCall":{"name":"h"}}]}]}}`
	if got := compact(t, body); got != want {
		t.Errorf("正文不对:\n got %s\nwant %s", got, want)
	}
}

func TestRectifyGeminiFunctionIDsNoOpWithoutIDs(t *testing.T) {
	body := mustParse(t, `{"contents":[{"parts":[{"functionCall":{"name":"f"}},{"text":"hi"}]}]}`)

	fields, applied := rectifyGeminiFunctionIDs(body)

	if applied {
		t.Error("没有 id 时不应判定为已应用")
	}
	if intField(t, fields, "strippedFunctionCallIds") != 0 || intField(t, fields, "strippedFunctionResponseIds") != 0 {
		t.Errorf("计数应为零: %#v", fields)
	}
	if got, want := compact(t, body), `{"contents":[{"parts":[{"functionCall":{"name":"f"}},{"text":"hi"}]}]}`; got != want {
		t.Errorf("正文被改动: %s", got)
	}
}
