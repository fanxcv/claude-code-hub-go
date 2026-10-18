package convert

import (
	"strings"
	"testing"
)

// 本文件钉住 responses 源/目标在非流式转换上的五处缺口（审计差异表 #1/#3/#5/#6/#10）：
//
//  1. `input_file` 的三种承载形态（file_data / file_url / file_id）此前无解码分支，整块落
//     opaque，跨线全丢——只带文件的请求因此变成 `messages: []`，上游直接 400；
//  2. `custom_tool_call` 响应项此前落 default 被丢，客户端拿到空 assistant，看不到模型调了什么；
//  3. `refusal` 分片此前记损并丢——拒答文本是模型给出的全部输出，丢了客户端就只剩空消息；
//  4. `input_image.image_url` 的对象形态（`{url, detail}`，官方两种形态都收）此前整块落 opaque；
//  5. 响应侧 `annotations` 此前静默丢弃，连损失条目都没有。
//
// 为何单测而不是只靠语料：语料是 Node 行为的逐字节快照，上面这些格位在 Node 侧本就没有实现
// （refusal 丢弃正是快照里的期望），无法用「与 Node 对齐」证明改对了。

const testFilePDFData = "JVBERi0xLjQKJcfsj6IK"

// convertResponsesRequestTo 走 responses 源 → 目标线的请求转换，返回出站正文与两侧损失合并。
func convertResponsesRequestTo(t *testing.T, body string, target WireProtocol) (*Value, LossReport) {
	t.Helper()
	ctx := ConvertCtx{
		ClientFormat:   clientFormatOfProtocol(target),
		TargetProto:    target,
		Model:          "m",
		ToWireToolName: NormalizeToolName,
	}
	decoded, ok := DecodeRequest(ProtocolOpenAIResponses, mustParsePayload(t, body), ctx)
	if !ok {
		t.Fatal("responses 线必须能解码")
	}
	encoded, ok := EncodeRequest(target, decoded.Value, ctx)
	if !ok {
		t.Fatalf("目标线 %s 必须能编码", target)
	}
	loss := decoded.Loss
	loss.Entries = append(loss.Entries, encoded.Loss.Entries...)
	return encoded.Body, loss
}

// convertResponsesResponseTo 走 responses 源 → 目标线的非流式响应转换。
func convertResponsesResponseTo(t *testing.T, body string, target WireProtocol) (*Value, LossReport) {
	t.Helper()
	ctx := ConvertCtx{
		ClientFormat:     clientFormatOfProtocol(target),
		TargetProto:      target,
		Model:            "m",
		FromWireToolName: NormalizeToolName,
	}
	decoded, ok := DecodeResponse(ProtocolOpenAIResponses, mustParsePayload(t, body), ctx)
	if !ok {
		t.Fatal("responses 线必须能解码响应")
	}
	encoded, ok := EncodeResponse(target, decoded.Value, ctx)
	if !ok {
		t.Fatalf("目标线 %s 必须能编码响应", target)
	}
	loss := decoded.Loss
	loss.Entries = append(loss.Entries, encoded.Loss.Entries...)
	return encoded.Body, loss
}

// anthropicContentBlocks 取出 anthropic 正文里第一条消息的 content 数组。
func anthropicContentBlocks(t *testing.T, body *Value) []*Value {
	t.Helper()
	messages := body.ArrayField("messages")
	if len(messages) == 0 {
		t.Fatalf("anthropic 正文没有 messages：%s", string(body.MarshalCompact()))
	}
	return messages[0].ArrayField("content")
}

// inputFileRequest 拼一份只带一个 input_file 分片的 responses 请求。
func inputFileRequest(part string) string {
	// max_output_tokens 显式给出：否则 anthropic 出口按缺省值补一条 max_tokens.defaulted，
	// 与本文件的「是否还有其它损失」判据混在一起。
	return `{"model":"m","max_output_tokens":4096,"input":[{"type":"message","role":"user","content":[` + part + `]}]}`
}

// TestResponsesInputFileDataBecomesAnthropicDocument：file_data 是 data URL，须解成
// document 块的 base64 源（裸 base64 + 媒体类型），而不是整块丢弃。
func TestResponsesInputFileDataBecomesAnthropicDocument(t *testing.T) {
	body, loss := convertResponsesRequestTo(t, inputFileRequest(
		`{"type":"input_file","filename":"a.pdf","file_data":"data:application/pdf;base64,`+testFilePDFData+`"}`),
		ProtocolAnthropicMessages)

	blocks := anthropicContentBlocks(t, body)
	if len(blocks) != 1 {
		t.Fatalf("应当只有一个 document 块，实际 %d 块：%s", len(blocks), string(body.MarshalCompact()))
	}
	if kind, _ := blocks[0].StringField("type"); kind != "document" {
		t.Fatalf("file_data 必须跨成 document 块，实际 %q：%s", kind, string(body.MarshalCompact()))
	}
	source := blocks[0].ObjectField("source")
	if sourceType, _ := source.StringField("type"); sourceType != "base64" {
		t.Fatalf("document.source.type 应为 base64，实际 %q", sourceType)
	}
	if mediaType, _ := source.StringField("media_type"); mediaType != "application/pdf" {
		t.Fatalf("document.source.media_type 应为 application/pdf，实际 %q", mediaType)
	}
	if data, _ := source.StringField("data"); data != testFilePDFData {
		t.Fatalf("document.source.data 应原样承载裸 base64，实际 %q", data)
	}
	if len(loss.Entries) != 0 {
		t.Fatalf("file_data 全程可承载，不应有损失条目，实际 %+v", loss.Entries)
	}
}

// TestResponsesInputFileURLBecomesAnthropicDocument：file_url 无抓取层，按 URL 源原样送出。
func TestResponsesInputFileURLBecomesAnthropicDocument(t *testing.T) {
	const url = "https://example.com/report.pdf"
	body, loss := convertResponsesRequestTo(t, inputFileRequest(
		`{"type":"input_file","file_url":"`+url+`"}`),
		ProtocolAnthropicMessages)

	blocks := anthropicContentBlocks(t, body)
	if len(blocks) != 1 {
		t.Fatalf("应当只有一个 document 块，实际 %d 块：%s", len(blocks), string(body.MarshalCompact()))
	}
	if kind, _ := blocks[0].StringField("type"); kind != "document" {
		t.Fatalf("file_url 必须跨成 document 块，实际 %q：%s", kind, string(body.MarshalCompact()))
	}
	source := blocks[0].ObjectField("source")
	if sourceType, _ := source.StringField("type"); sourceType != "url" {
		t.Fatalf("document.source.type 应为 url，实际 %q", sourceType)
	}
	if got, _ := source.StringField("url"); got != url {
		t.Fatalf("document.source.url 应原样承载，实际 %q", got)
	}
	if len(loss.Entries) != 0 {
		t.Fatalf("file_url 全程可承载，不应有损失条目，实际 %+v", loss.Entries)
	}
}

// TestResponsesInputFileIDIsReportedNotBoxed：file_id 无跨线载体，必须记损而不是装箱——
// 装箱块只在同线编码时原样回写，跨线一律丢，等于「同线也丢」。
// 同时钉住「不产生空消息」：只有文件、没有文本的消息必须整条不产出，而不是发一条空 content。
func TestResponsesInputFileIDIsReportedNotBoxed(t *testing.T) {
	body, loss := convertResponsesRequestTo(t, inputFileRequest(
		`{"type":"input_file","file_id":"file_abc"}`),
		ProtocolAnthropicMessages)

	if messages := body.ArrayField("messages"); len(messages) != 0 {
		t.Fatalf("只有 file_id 的消息不得产出一条空消息，实际 %s", string(body.MarshalCompact()))
	}
	assertLossEntry(t, loss, LossUnknownFieldStructure, "dropped", "input_file.file_id")
	if serialized := string(body.MarshalCompact()); strings.Contains(serialized, "file_id") {
		t.Fatalf("file_id 不得被装箱回写，实际 %s", serialized)
	}
}

// TestResponsesInputFileRoundTripsWithinSameLine：file_data 在同线往返时须还原成 input_file，
// 否则 responses → responses 也把文件弄丢（比跨线丢失更难发现）。
func TestResponsesInputFileRoundTripsWithinSameLine(t *testing.T) {
	body, _ := convertResponsesRequestTo(t, inputFileRequest(
		`{"type":"input_file","file_data":"data:application/pdf;base64,`+testFilePDFData+`"}`),
		ProtocolOpenAIResponses)

	input := body.ArrayField("input")
	if len(input) != 1 {
		t.Fatalf("同线往返应保留一条消息，实际 %s", string(body.MarshalCompact()))
	}
	parts := input[0].ArrayField("content")
	if len(parts) != 1 {
		t.Fatalf("应保留一个分片，实际 %s", string(body.MarshalCompact()))
	}
	if kind, _ := parts[0].StringField("type"); kind != "input_file" {
		t.Fatalf("同线往返须还原成 input_file，实际 %q：%s", kind, string(body.MarshalCompact()))
	}
	if data, _ := parts[0].StringField("file_data"); data != "data:application/pdf;base64,"+testFilePDFData {
		t.Fatalf("file_data 应逐字节还原，实际 %q", data)
	}
}

// TestResponsesObjectImageURLDecodes：`image_url` 的两种官方形态都须解出图片；
// 对象形态的 `detail` 无枢纽承载位，记损即可，但不能连 url 一起丢。
func TestResponsesObjectImageURLDecodes(t *testing.T) {
	const url = "https://example.com/shot.png"
	body, loss := convertResponsesRequestTo(t, inputFileRequest(
		`{"type":"input_image","image_url":{"url":"`+url+`","detail":"high"}}`),
		ProtocolAnthropicMessages)

	blocks := anthropicContentBlocks(t, body)
	if len(blocks) != 1 {
		t.Fatalf("对象形态 image_url 必须解出图片，实际 %s", string(body.MarshalCompact()))
	}
	if kind, _ := blocks[0].StringField("type"); kind != "image" {
		t.Fatalf("应为 image 块，实际 %q", kind)
	}
	if got, _ := blocks[0].ObjectField("source").StringField("url"); got != url {
		t.Fatalf("image.source.url 应取对象里的 url，实际 %q", got)
	}
	assertLossEntry(t, loss, LossUnknownFieldContent, "dropped", "image_url.detail")
}

// TestResponsesCustomToolCallResponseItemRestored：响应侧的 custom_tool_call 此前落 default 被丢，
// 客户端拿到空 assistant。参数包一层 {"input":…}，与请求侧降级同形状。
func TestResponsesCustomToolCallResponseItemRestored(t *testing.T) {
	const upstream = `{"id":"resp_1","object":"response","model":"m","status":"completed","output":[` +
		`{"type":"custom_tool_call","id":"ctc_1","call_id":"call_abc","name":"exec",` +
		`"input":"await tools.exec_command({cmd:\"ls\"})"}]}`

	t.Run("anthropic", func(t *testing.T) {
		body, _ := convertResponsesResponseTo(t, upstream, ProtocolAnthropicMessages)
		blocks := body.ArrayField("content")
		if len(blocks) != 1 {
			t.Fatalf("应有一个 tool_use 块，实际 %s", string(body.MarshalCompact()))
		}
		if kind, _ := blocks[0].StringField("type"); kind != "tool_use" {
			t.Fatalf("应为 tool_use，实际 %q", kind)
		}
		if id, _ := blocks[0].StringField("id"); id != "call_abc" {
			t.Fatalf("tool_use.id 应取 call_id，实际 %q", id)
		}
		if name, _ := blocks[0].StringField("name"); name != "exec" {
			t.Fatalf("tool_use.name 应取 name，实际 %q", name)
		}
		input := blocks[0].ObjectField("input")
		if payload, _ := input.StringField("input"); payload != `await tools.exec_command({cmd:"ls"})` {
			t.Fatalf("input.input 应原样承载 freeform 载荷，实际 %q（整块 %s）",
				payload, string(blocks[0].MarshalCompact()))
		}
		if reason, _ := body.StringField("stop_reason"); reason != "tool_use" {
			t.Fatalf("有工具块时 stop_reason 应为 tool_use，实际 %q", reason)
		}
	})

	t.Run("chat", func(t *testing.T) {
		body, _ := convertResponsesResponseTo(t, upstream, ProtocolOpenAIChat)
		message := body.ArrayField("choices")[0].ObjectField("message")
		toolCalls := message.ArrayField("tool_calls")
		if len(toolCalls) != 1 {
			t.Fatalf("应有 tool_calls，实际 %s", string(body.MarshalCompact()))
		}
		function := toolCalls[0].ObjectField("function")
		if name, _ := function.StringField("name"); name != "exec" {
			t.Fatalf("tool_calls.function.name 应为 exec，实际 %q", name)
		}
		if args, _ := function.StringField("arguments"); args != `{"input":"await tools.exec_command({cmd:\"ls\"})"}` {
			t.Fatalf("tool_calls.function.arguments 应为包了 input 的 JSON，实际 %q", args)
		}
	})
}

// TestResponsesRefusalPartKeepsText：拒答文本对客户端可见，丢弃后客户端只拿到空 assistant。
func TestResponsesRefusalPartKeepsText(t *testing.T) {
	body, loss := convertResponsesRequestTo(t, inputFileRequest(
		`{"type":"refusal","refusal":"其余不便回答"}`),
		ProtocolAnthropicMessages)

	blocks := anthropicContentBlocks(t, body)
	if len(blocks) != 1 {
		t.Fatalf("refusal 应承载为文本块，实际 %s", string(body.MarshalCompact()))
	}
	if kind, _ := blocks[0].StringField("type"); kind != "text" {
		t.Fatalf("应为 text 块，实际 %q", kind)
	}
	if text, _ := blocks[0].StringField("text"); text != "其余不便回答" {
		t.Fatalf("拒答文本应原样承载，实际 %q", text)
	}
	if len(loss.Entries) != 0 {
		t.Fatalf("拒答文本已承载，不应再记损，实际 %+v", loss.Entries)
	}
}

// TestResponsesAnnotationsReportedAsLoss：枢纽块没有注解承载位，跨线只能记损——
// 但绝不能静默丢弃：url_citation 是客户端可见内容，无条目就无从排查。
func TestResponsesAnnotationsReportedAsLoss(t *testing.T) {
	const upstream = `{"id":"resp_1","object":"response","model":"m","status":"completed","output":[` +
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"See [1]",` +
		`"annotations":[{"type":"url_citation","url":"https://example.com/a","title":"A",` +
		`"start_index":4,"end_index":7}]}]}]}`

	body, loss := convertResponsesResponseTo(t, upstream, ProtocolOpenAIChat)
	message := body.ArrayField("choices")[0].ObjectField("message")
	if content, _ := message.StringField("content"); content != "See [1]" {
		t.Fatalf("正文不应受影响，实际 %q", content)
	}
	assertLossEntry(t, loss, LossUnknownFieldOther, "dropped", "annotations")
}
