package convert

import (
	"strings"
	"testing"
)

// 本文件钉住 responses 线 `input_file` 分片的跨线承载（#6）：
//
//	file_data / file_url -> anthropic document（base64 / url 两种 source）
//	file_id              -> 不承载，记 document / input_file.file_id.unreachable
//	三形态同线往返         -> 逐字节一致（原分片回写，不按枢纽字段重建）
//	filename             -> 无跨线承载位，记 info 档 document.filename
//
// 为何同线要逐字节：input_file 在改造前是 opaque 块、原样回写。落 BlockDocument 后若按
// MediaType/Data/URL 重建，`filename`（anthropic document 无该成员，故枢纽不承载它）会消失、
// 键序也会被重排——那是同线退化，不只是少一个字段。

const (
	testInputFilePDFB64           = "JVBERi0="
	testInputFileData             = `{"type":"input_file","file_data":"data:application/pdf;base64,` + testInputFilePDFB64 + `"}`
	testInputFileDataWithFilename = `{"type":"input_file","file_data":"data:application/pdf;base64,` +
		testInputFilePDFB64 + `","filename":"a.pdf"}`
	testInputFileURL = `{"type":"input_file","file_url":"https://example.com/a.pdf"}`
	testInputFileID  = `{"type":"input_file","file_id":"file-abc"}`
)

// responsesInputFileBody 造一个只带给定分片的 responses 请求体。
func responsesInputFileBody(part string) string {
	return `{"model":"m","input":[{"type":"message","role":"user","content":[` + part + `]}]}`
}

// convertResponsesInputFile 走真实解码 + 编码，返回出站正文与两侧合并的损失台账（同 forward 口径）。
func convertResponsesInputFile(t *testing.T, part string, target WireProtocol) (*Value, LossReport) {
	t.Helper()
	ctx := ConvertCtx{
		ClientFormat:   FormatResponse,
		TargetProto:    target,
		Model:          "m",
		ToWireToolName: NormalizeToolName,
	}
	decoded, ok := DecodeRequest(ProtocolOpenAIResponses, mustParsePayload(t, responsesInputFileBody(part)), ctx)
	if !ok {
		t.Fatal("responses 正文必须能解码")
	}
	encoded, ok := EncodeRequest(target, decoded.Value, ctx)
	if !ok {
		t.Fatalf("%s 目标必须能编码", target)
	}
	loss := decoded.Loss
	loss.Entries = append(loss.Entries, encoded.Loss.Entries...)
	return encoded.Body, loss
}

// anthropicFirstContentPart 取出 anthropic 出站首条消息的第一个 content 分片。
func anthropicFirstContentPart(t *testing.T, body *Value) *Value {
	t.Helper()
	messages, present := body.Get("messages")
	if !present || messages == nil || !messages.IsArray() || len(messages.Items()) == 0 {
		t.Fatalf("anthropic 出站体没有 messages：%s", body.MarshalCompact())
	}
	content, present := messages.Items()[0].Get("content")
	if !present || content == nil || !content.IsArray() || len(content.Items()) == 0 {
		t.Fatalf("anthropic 首条消息没有 content 分片：%s", body.MarshalCompact())
	}
	return content.Items()[0]
}

func TestInputFileDataBecomesAnthropicDocument(t *testing.T) {
	body, loss := convertResponsesInputFile(t, testInputFileData, ProtocolAnthropicMessages)

	part := anthropicFirstContentPart(t, body)
	if blockType, _ := stringField(part, "type"); blockType != "document" {
		t.Fatalf("file_data 应落 anthropic document，实际 %q：%s", blockType, body.MarshalCompact())
	}
	source, present := part.Get("source")
	if !present || source == nil {
		t.Fatalf("document 缺 source：%s", body.MarshalCompact())
	}
	if sourceType, _ := stringField(source, "type"); sourceType != "base64" {
		t.Fatalf("source.type 应为 base64，实际 %q：%s", sourceType, body.MarshalCompact())
	}
	if mediaType, _ := stringField(source, "media_type"); mediaType != "application/pdf" {
		t.Fatalf("media_type 应从 data URL 带出 application/pdf，实际 %q", mediaType)
	}
	if data, _ := stringField(source, "data"); data != testInputFilePDFB64 {
		t.Fatalf("data 应原样搬运 %q，实际 %q", testInputFilePDFB64, data)
	}
	for _, entry := range loss.Entries {
		if entry.Capability == LossDocument {
			t.Fatalf("文件已送达，不该记 document 类损失：%+v", entry)
		}
	}
}

func TestInputFileURLBecomesAnthropicDocument(t *testing.T) {
	body, loss := convertResponsesInputFile(t, testInputFileURL, ProtocolAnthropicMessages)

	part := anthropicFirstContentPart(t, body)
	if blockType, _ := stringField(part, "type"); blockType != "document" {
		t.Fatalf("file_url 应落 anthropic document，实际 %q：%s", blockType, body.MarshalCompact())
	}
	source, _ := part.Get("source")
	if sourceType, _ := stringField(source, "type"); sourceType != "url" {
		t.Fatalf("source.type 应为 url，实际 %q：%s", sourceType, body.MarshalCompact())
	}
	if url, _ := stringField(source, "url"); url != "https://example.com/a.pdf" {
		t.Fatalf("url 应原样搬运，实际 %q", url)
	}
	for _, entry := range loss.Entries {
		if entry.Capability == LossDocument {
			t.Fatalf("文件已送达，不该记 document 类损失：%+v", entry)
		}
	}
}

// TestInputFileIDIsNotCarried：file_id 是本线文件库句柄，跨线没有等价物（内容本就不在请求里），
// 故既不出现在目标正文，也必须留下一条可定位的损失（不是静默丢弃）。
func TestInputFileIDIsNotCarried(t *testing.T) {
	body, loss := convertResponsesInputFile(t, testInputFileID, ProtocolAnthropicMessages)

	marshaled := string(body.MarshalCompact())
	if strings.Contains(marshaled, "file-abc") || strings.Contains(marshaled, `"document"`) {
		t.Fatalf("file_id 不得跨线承载：%s", marshaled)
	}
	assertLossEntry(t, loss, LossDocument, string(LossDropped), "input_file.file_id.unreachable")
}

// TestInputFileSameLineRoundTripIsByteIdentical 钉住同线往返：三形态都不许被重建。
func TestInputFileSameLineRoundTripIsByteIdentical(t *testing.T) {
	cases := []struct {
		name string
		part string
	}{
		{"file_data", testInputFileData},
		{"file_data+filename", testInputFileDataWithFilename},
		{"file_url", testInputFileURL},
		{"file_id", testInputFileID},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			body, loss := convertResponsesInputFile(t, testCase.part, ProtocolOpenAIResponses)

			want := responsesInputFileBody(testCase.part)
			if got := string(body.MarshalCompact()); got != want {
				t.Fatalf("同线往返必须逐字节一致：\n want %s\n  got %s", want, got)
			}
			for _, entry := range loss.Entries {
				if entry.Capability == LossDocument || entry.Capability == LossDocumentFilename {
					t.Fatalf("同线原样回写，不该记损失：%+v", entry)
				}
			}
		})
	}
}

// TestInputFileFilenameLossIsInfo：anthropic document 没有文件名成员，filename 只在对端真有
// 承载位、文件真送出去的时候才记，且必须落 info 档（丢一个客户端标签不改变上游看到的正文）。
func TestInputFileFilenameLossIsInfo(t *testing.T) {
	body, loss := convertResponsesInputFile(t, testInputFileDataWithFilename, ProtocolAnthropicMessages)

	// 文件本身必须送达（否则 filename 的损失应当由 document/dropped 承载，而不是这里）。
	part := anthropicFirstContentPart(t, body)
	if blockType, _ := stringField(part, "type"); blockType != "document" {
		t.Fatalf("filename 不该连累文件本身：%s", body.MarshalCompact())
	}
	if marshaled := string(body.MarshalCompact()); strings.Contains(marshaled, "a.pdf") {
		t.Fatalf("anthropic document 无 name 成员，文件名不得出现：%s", marshaled)
	}
	assertLossEntry(t, loss, LossDocumentFilename, string(LossDropped), "input_file.filename")
	if got := LossSeverityOf(LossDocumentFilename, LossDropped); got != SeverityInfo {
		t.Fatalf("filename 无承载位应为 %s 档，实际 %s", SeverityInfo, got)
	}
	for _, entry := range loss.Entries {
		if entry.Capability == LossDocument {
			t.Fatalf("文件已送达，不该再记 document 类损失：%+v", entry)
		}
	}
}

// TestInputFileDocumentIsNotOpaquePassthrough 钉住「带源分片的 document 不是 opaque 块」：
// 两个跨线目标都必须是「能承载则映射、不能承载则按 capability 记损」，不得把 input_file 原文
// 悄悄透传出去（那会把无法承载的文件变成上游看不懂的分片）。
func TestInputFileDocumentIsNotOpaquePassthrough(t *testing.T) {
	for _, target := range []WireProtocol{ProtocolAnthropicMessages, ProtocolOpenAIChat} {
		for _, part := range []string{testInputFileData, testInputFileURL, testInputFileID} {
			body, loss := convertResponsesInputFile(t, part, target)
			marshaled := string(body.MarshalCompact())
			for _, forbidden := range []string{`"input_file"`, `"file_data"`, `"file_url"`, `"file_id"`} {
				if strings.Contains(marshaled, forbidden) {
					t.Fatalf("%s 目标不得透传源分片（命中 %s）：%s", target, forbidden, marshaled)
				}
			}
			if target == ProtocolOpenAIChat {
				assertLossEntry(t, loss, LossDocument, string(LossDropped), "chat_has_no_document")
			}
		}
	}
}

// TestInputFileAbsentRoundTripIsByteIdentical 是回归钉子：不含 input_file 的正文
// 同线往返仍逐字节一致——新增的 input_file 分支不得顺走别的内容分片（消息正文与工具结果两侧
// 都摆上真实分片，顺走任何一片都会改变出站字节）。
func TestInputFileAbsentRoundTripIsByteIdentical(t *testing.T) {
	const raw = `{"model":"m","instructions":"sys","input":[{"type":"message","role":"user","content":[` +
		`{"type":"input_text","text":"看这张图"},` +
		`{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="},` +
		`{"type":"mcp_call","id":"mcp_1","name":"lookup","arguments":"{}"}]},` +
		`{"type":"function_call","call_id":"call_1","name":"f","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"call_1","output":[` +
		`{"type":"input_text","text":"ok"},` +
		`{"type":"input_image","image_url":"https://example.com/x.png"}]}]}`

	ctx := ConvertCtx{ClientFormat: FormatResponse, TargetProto: ProtocolOpenAIResponses, Model: "m", ToWireToolName: NormalizeToolName}
	decoded, ok := DecodeRequest(ProtocolOpenAIResponses, mustParsePayload(t, raw), ctx)
	if !ok {
		t.Fatal("responses 正文必须能解码")
	}
	encoded, ok := EncodeRequest(ProtocolOpenAIResponses, decoded.Value, ctx)
	if !ok {
		t.Fatal("responses 目标必须能编码")
	}
	if got := string(encoded.Body.MarshalCompact()); got != raw {
		t.Fatalf("无 input_file 的同线往返必须逐字节一致：\n want %s\n  got %s", raw, got)
	}
	for _, entry := range append(append([]LossEntry{}, decoded.Loss.Entries...), encoded.Loss.Entries...) {
		if entry.Capability == LossDocument || entry.Capability == LossDocumentFilename {
			t.Fatalf("无 document 的正文不该记 document 类损失：%+v", entry)
		}
	}
}
