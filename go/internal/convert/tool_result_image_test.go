package convert

import (
	"strings"
	"testing"
)

// 本文件钉住 function_call_output 的承载形态：工具结果里的图片必须能跨到 responses 线。
//
// 动机（2026-10-18 本地矩阵实测）：Claude Code / Codex 的截图类工具把图放在 tool_result /
// function_call_output 里，此前 responsesFunctionCallOutputItem 只拼文本，图整幅丢弃。
// OpenAI 官方 function calling 指南明确支持 `output` 用 input_text / input_image / input_file
// 数组表达「工具返回图片或文件」。纯文本仍写字符串，保持既有兼容面。

const testToolResultImageB64 = "iVBORw0KGgoAAAANSUhEUg=="

// findFunctionCallOutput 在编码后的 responses 正文里取第一条 function_call_output。
func findFunctionCallOutput(t *testing.T, body *Value) *Value {
	t.Helper()
	for _, item := range body.ArrayField("input") {
		if kind, _ := item.StringField("type"); kind == "function_call_output" {
			return item
		}
	}
	t.Fatalf("编码结果里没有 function_call_output：%s", string(body.MarshalCompact()))
	return nil
}

func TestResponsesFunctionCallOutputKeepsImage(t *testing.T) {
	ctx := ConvertCtx{
		ClientFormat:   FormatClaude,
		TargetProto:    ProtocolOpenAIResponses,
		Model:          "m",
		ToWireToolName: NormalizeToolName,
	}
	body := `{"model":"m","max_tokens":64,"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"截图"}]},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"screenshot","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[` +
		`{"type":"text","text":"截图如下"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + testToolResultImageB64 + `"}}]}]}],` +
		`"tools":[{"name":"screenshot","description":"d","input_schema":{"type":"object"}}]}`

	decoded, ok := DecodeRequest(ProtocolAnthropicMessages, mustParsePayload(t, body), ctx)
	if !ok {
		t.Fatal("anthropic 正文必须能解码")
	}
	encoded, ok := EncodeRequest(ProtocolOpenAIResponses, decoded.Value, ctx)
	if !ok {
		t.Fatal("responses 目标必须能编码")
	}

	item := findFunctionCallOutput(t, encoded.Body)
	output, present := item.Get("output")
	if !present || output == nil {
		t.Fatal("function_call_output 应有 output")
	}
	if output.Kind() != "array" {
		t.Fatalf("带图的工具结果 output 应为数组，实际 %s（正文 %s）", output.Kind(), string(encoded.Body.MarshalCompact()))
	}
	parts := output.Items()
	if len(parts) != 2 {
		t.Fatalf("应有两段（文本 + 图），实际 %d（正文 %s）", len(parts), string(encoded.Body.MarshalCompact()))
	}
	if kind, _ := parts[0].StringField("type"); kind != "input_text" {
		t.Fatalf("第一段应为 input_text，实际 %q", kind)
	}
	text, _ := parts[0].StringField("text")
	if text != "截图如下" {
		t.Fatalf("文本段应为原文本，实际 %q", text)
	}
	kind, _ := parts[1].StringField("type")
	if kind != "input_image" {
		t.Fatalf("第二段应为 input_image，实际 %q", kind)
	}
	url, _ := parts[1].StringField("image_url")
	if !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Fatalf("图片应表达为 data URL，实际 %q", url)
	}
	if !strings.Contains(url, testToolResultImageB64) {
		t.Fatalf("图片字节应保持在承载值里，实际 %q", url)
	}
	// anthropic 的 base64 源不是 data URL：转成 data URL 只算承载形态变化，不记损。
	// 关键是不能出现 image/dropped——那代表图又丢了。
	for _, entry := range append(append([]LossEntry{}, decoded.Loss.Entries...), encoded.Loss.Entries...) {
		if entry.Capability == LossImage && entry.Action == LossDropped {
			t.Fatalf("图已送出却记了丢弃：%+v", entry)
		}
	}
}

// 纯文本工具结果必须仍写字符串：数组形态只有带图时才用，避免无谓改变绝大多数请求的形状。
func TestResponsesFunctionCallOutputTextStaysString(t *testing.T) {
	ctx := ConvertCtx{ClientFormat: FormatResponse, TargetProto: ProtocolOpenAIResponses, Model: "m"}
	body := `{"model":"m","max_output_tokens":64,"input":[` +
		`{"role":"user","content":[{"type":"input_text","text":"查"}]},` +
		`{"type":"function_call","call_id":"call_1","name":"t","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"call_1","output":"晴 25"}],` +
		`"tools":[{"type":"function","name":"t","parameters":{"type":"object"}}]}`
	decoded, ok := DecodeRequest(ProtocolOpenAIResponses, mustParsePayload(t, body), ctx)
	if !ok {
		t.Fatal("responses 正文必须能解码")
	}
	encoded, ok := EncodeRequest(ProtocolOpenAIResponses, decoded.Value, ctx)
	if !ok {
		t.Fatal("responses 目标必须能编码")
	}
	item := findFunctionCallOutput(t, encoded.Body)
	output, present := item.Get("output")
	if !present || output == nil {
		t.Fatal("function_call_output 应有 output")
	}
	if output.Kind() != "string" {
		t.Fatalf("纯文本工具结果应保持字符串，实际 %s", output.Kind())
	}
	if text, _ := output.String(); text != "晴 25" {
		t.Fatalf("文本应原样保留，实际 %q", text)
	}
}
