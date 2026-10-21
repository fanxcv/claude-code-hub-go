package convert

import (
	"testing"
	"unsafe"
)

// 本文件钉住 #5：chat 的 response_format 与 responses 的 text.format 双向互转。
//
// 三种 type 才有等价物（text / json_object / json_schema），json_schema 只差一层嵌套外壳；
// 内层 schema 子树全程共享源树指针（零拷贝，见 schema_sharing_test.go）。未知 type（如未来的
// grammar）不写出去、但必须记损——静默丢弃才是缺陷。

// structuredOutputConversion 解码 from 线后编码到 to 线，返回出站体与合并损失。
//
// 入参是**同一棵已解析的源树**：零拷贝断言要比对指针，故不能各处自行 mustParsePayload。
func structuredOutputConversion(t *testing.T, source *Value, from, to WireProtocol) (*Value, []LossEntry) {
	t.Helper()
	ctx := ConvertCtx{
		ClientFormat:   clientFormatOfProtocol(from),
		TargetProto:    to,
		Model:          stringOrEmpty(source, "model"),
		ToWireToolName: NormalizeToolName,
	}
	decoded, ok := DecodeRequest(from, source, ctx)
	if !ok {
		t.Fatalf("协议线 %s 无解码器", from)
	}
	encoded, ok := EncodeRequest(to, decoded.Value, ctx)
	if !ok {
		t.Fatalf("协议线 %s 无编码器", to)
	}
	return encoded.Body, append(append([]LossEntry{}, decoded.Loss.Entries...), encoded.Loss.Entries...)
}

// assertCompactEqual 按紧凑序列化比对（键序也算在内）。
func assertCompactEqual(t *testing.T, got *Value, want string) {
	t.Helper()
	if got == nil {
		t.Fatalf("缺字段，期望 %s", want)
	}
	if actual := got.MarshalCompact(); actual != want {
		t.Fatalf("出站 %s，期望 %s", actual, want)
	}
}

// assertNoStructuredOutputLoss 断言结构化输出没有被记成损失（承载成功却记损是假账）。
func assertNoStructuredOutputLoss(t *testing.T, entries []LossEntry) {
	t.Helper()
	for _, class := range []string{LossResponseFormat, LossTextControls} {
		for _, entry := range entries {
			if entry.Capability == class {
				t.Fatalf("%s 已成功承载，不得记损，实际 %+v", class, entries)
			}
		}
	}
}

// TestResponseFormatChatJSONSchemaToResponses 钉住 chat json_schema → responses text.format，
// 且内层 name / strict / schema 逐个搬运、schema 仍是源树的那一个指针。
func TestResponseFormatChatJSONSchemaToResponses(t *testing.T) {
	source := mustParsePayload(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],`+
		`"response_format":{"type":"json_schema","json_schema":`+
		`{"name":"big","strict":true,"schema":{"type":"object","properties":{}}}}}`)

	out, losses := structuredOutputConversion(t, source, ProtocolOpenAIChat, ProtocolOpenAIResponses)
	format := fieldOrNil(out, "text", "format")
	assertCompactEqual(t, format, `{"type":"json_schema","name":"big","strict":true,"schema":{"type":"object","properties":{}}}`)

	if fieldOrNil(format, "json_schema") != nil {
		t.Fatal("responses 线的 format 是扁平形状，不得保留 chat 的 json_schema 外壳")
	}
	sourceSchema := fieldOrNil(source, "response_format", "json_schema", "schema")
	if fieldOrNil(format, "schema") != sourceSchema {
		t.Fatal("schema 子树跨线后必须是源树的同一 *Value（零拷贝），实际被复制")
	}
	assertNoStructuredOutputLoss(t, losses)
}

// TestResponseFormatResponsesJSONSchemaToChat 钉住反方向：responses 的扁平 format 折回 chat 的
// json_schema 外壳，成员与 schema 指针同样原样搬运。
func TestResponseFormatResponsesJSONSchemaToChat(t *testing.T) {
	source := mustParsePayload(t, `{"model":"gpt-5","input":"hi",`+
		`"text":{"format":{"type":"json_schema","name":"big","strict":false,"schema":{"type":"object"}}}}`)

	out, losses := structuredOutputConversion(t, source, ProtocolOpenAIResponses, ProtocolOpenAIChat)
	format := fieldOrNil(out, "response_format")
	assertCompactEqual(t, format, `{"type":"json_schema","json_schema":{"name":"big","strict":false,"schema":{"type":"object"}}}`)

	sourceSchema := fieldOrNil(source, "text", "format", "schema")
	if fieldOrNil(format, "json_schema", "schema") != sourceSchema {
		t.Fatal("schema 子树跨线后必须是源树的同一 *Value（零拷贝），实际被复制")
	}
	assertNoStructuredOutputLoss(t, losses)
}

// TestResponseFormatTextAndJSONObjectBothWays 钉住两线同形的两种 type 双向原样搬运。
func TestResponseFormatTextAndJSONObjectBothWays(t *testing.T) {
	for _, kind := range []string{"text", "json_object"} {
		t.Run("chat→responses/"+kind, func(t *testing.T) {
			source := mustParsePayload(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],`+
				`"response_format":{"type":"`+kind+`"}}`)
			out, losses := structuredOutputConversion(t, source, ProtocolOpenAIChat, ProtocolOpenAIResponses)
			assertCompactEqual(t, fieldOrNil(out, "text", "format"), `{"type":"`+kind+`"}`)
			assertNoStructuredOutputLoss(t, losses)
		})
		t.Run("responses→chat/"+kind, func(t *testing.T) {
			source := mustParsePayload(t, `{"model":"gpt-5","input":"hi",`+
				`"text":{"format":{"type":"`+kind+`"}}}`)
			out, losses := structuredOutputConversion(t, source, ProtocolOpenAIResponses, ProtocolOpenAIChat)
			assertCompactEqual(t, fieldOrNil(out, "response_format"), `{"type":"`+kind+`"}`)
			assertNoStructuredOutputLoss(t, losses)
		})
	}
}

// TestResponseFormatUnknownTypeRecordedNotWritten 钉住 fail-closed 的另一半：两线没有等价物的
// 未知 type 不得写出去（不猜语义），但必须记损——跨线丢一条，同线原样保留。
func TestResponseFormatUnknownTypeRecordedNotWritten(t *testing.T) {
	body := `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],` +
		`"response_format":{"type":"grammar","grammar":{"definition":"select-1"}}}`

	source := mustParsePayload(t, body)
	out, losses := structuredOutputConversion(t, source, ProtocolOpenAIChat, ProtocolOpenAIResponses)
	if out.Has("text") {
		t.Fatalf("未知 type 不得写出 responses 的 text.format，实际 %s", out.MarshalCompact())
	}
	if got := countLoss(losses, LossResponseFormat, "response_format"); got != 1 {
		t.Fatalf("未知 type 跨线须恰好记一条损失，实际 %d 条（%+v）", got, losses)
	}

	// 同线直通：passthrough 原样带回，不记损也不改写形状。
	same, sameLosses := structuredOutputConversion(t, source, ProtocolOpenAIChat, ProtocolOpenAIChat)
	assertCompactEqual(t, fieldOrNil(same, "response_format"),
		`{"type":"grammar","grammar":{"definition":"select-1"}}`)
	assertNoStructuredOutputLoss(t, sameLosses)

	// responses 侧的未知 type 同样只记损、不写出。
	responses := mustParsePayload(t, `{"model":"gpt-5","input":"hi",`+
		`"text":{"format":{"type":"grammar","grammar":{"definition":"select-1"}}}}`)
	out, losses = structuredOutputConversion(t, responses, ProtocolOpenAIResponses, ProtocolOpenAIChat)
	if out.Has("response_format") {
		t.Fatalf("未知 type 不得写出 chat 的 response_format，实际 %s", out.MarshalCompact())
	}
	if got := countLoss(losses, LossTextControls, "text"); got != 1 {
		t.Fatalf("未知 type 跨线须恰好记一条损失，实际 %d 条（%+v）", got, losses)
	}
}

// TestTextFormatResidualMembersStillRecorded 钉住 text 容器里除 format 之外的成员：format 已转出，
// verbosity 仍无等价物，故逐成员留痕（而不是整键记，把成功承载的 format 也算成丢）。
func TestTextFormatResidualMembersStillRecorded(t *testing.T) {
	source := mustParsePayload(t, `{"model":"gpt-5","input":"hi",`+
		`"text":{"format":{"type":"json_object"},"verbosity":"low"}}`)

	out, losses := structuredOutputConversion(t, source, ProtocolOpenAIResponses, ProtocolOpenAIChat)
	assertCompactEqual(t, fieldOrNil(out, "response_format"), `{"type":"json_object"}`)
	if got := countLoss(losses, LossTextControls, "text.verbosity"); got != 1 {
		t.Fatalf("verbosity 跨线须恰好记一条损失，实际 %d 条（%+v）", got, losses)
	}
	if got := countLoss(losses, LossTextControls, "text"); got != 0 {
		t.Fatalf("format 已转出，不得把整键 text 记成丢失，实际 %+v", losses)
	}
}

// TestResponseFormatSameLineKeptVerbatim 钉住同线往返：两种外壳都按源字节回写，不重排键序。
func TestResponseFormatSameLineKeptVerbatim(t *testing.T) {
	chatSource := mustParsePayload(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],`+
		`"response_format":{"json_schema":{"strict":true,"name":"big"},"type":"json_schema"}}`)
	chatOut, _ := structuredOutputConversion(t, chatSource, ProtocolOpenAIChat, ProtocolOpenAIChat)
	assertCompactEqual(t, fieldOrNil(chatOut, "response_format"),
		`{"json_schema":{"strict":true,"name":"big"},"type":"json_schema"}`)

	responsesSource := mustParsePayload(t, `{"model":"gpt-5","input":"hi",`+
		`"text":{"verbosity":"low","format":{"type":"json_object"}}}`)
	responsesOut, _ := structuredOutputConversion(t, responsesSource, ProtocolOpenAIResponses, ProtocolOpenAIResponses)
	assertCompactEqual(t, fieldOrNil(responsesOut, "text"), `{"verbosity":"low","format":{"type":"json_object"}}`)
}

// TestResponseFormatAbsentLeavesBodyUnchanged 是回归钉子：不带结构化输出的请求，出站字节一个
// 也不能变（本 lane 只该在客户端声明了约束时才动正文）。
func TestResponseFormatAbsentLeavesBodyUnchanged(t *testing.T) {
	chatOut, chatLosses := structuredOutputConversion(t,
		mustParsePayload(t, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`),
		ProtocolOpenAIChat, ProtocolOpenAIResponses)
	assertCompactEqual(t, chatOut,
		`{"model":"gpt-4o","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)

	responsesOut, responsesLosses := structuredOutputConversion(t,
		mustParsePayload(t, `{"model":"gpt-5","input":"hi"}`),
		ProtocolOpenAIResponses, ProtocolOpenAIChat)
	assertCompactEqual(t, responsesOut,
		`{"model":"gpt-5","stream":false,"messages":[{"role":"user","content":"hi"}]}`)

	assertNoStructuredOutputLoss(t, chatLosses)
	assertNoStructuredOutputLoss(t, responsesLosses)
}

// TestRequestSizeStaysAt224 钉住枢纽请求的尺寸：结构化输出约束的承载位必须是指针，且正好填掉
// HasPromptCacheKey 之后的 7 字节对齐空隙——216 与 224 落在同一档 size class，实际占位不变。
//
// 指针换成值（或字段摆位打乱多占一槽）会把 Request 顶出本档，即每个请求都多付一次分配。
func TestRequestSizeStaysAt224(t *testing.T) {
	const want = 224
	got := unsafe.Sizeof(Request{})
	t.Logf("Request=%d", got)
	if got > want {
		t.Fatalf("Request 必须守在 %d 字节以内，实际 %d 字节（承载位应为按需分配的指针）", want, got)
	}
}
