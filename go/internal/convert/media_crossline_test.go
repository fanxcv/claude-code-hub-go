package convert

import (
	"strings"
	"testing"
)

// 本文件钉住音频、视频与 chat file 分片的跨线承载（#13），以及三处不能动的东西：
//
//  1. 两条 OpenAI 线之间 audio / video / 内联文档双向都不丢（此前落 default → opaque，
//     跨线被目标线整块丢掉，台账还把它记成 tool.native / unknown_field.structure）；
//  2. anthropic 目标线没有音视频承载位 → 记 media 类损失，detail 带来源线；
//  3. 同线往返逐字节不变、无媒体时出站字节不变（回归钉子）；
//  4. 文档的媒体类型闸门挡在目标线上游会 400 的类型之前（400 计入供应商熔断）。
//
// Block 仍是 192 字节（新块类型只加常量，不新增字段）由既有的 TestBlockSizeStaysAt192 钉住。
//
// 为何单测而不是只靠语料：这三类分片在语料快照里根本不存在（Node 侧同样 opaque → 跨线丢），
// 无法用「与 Node 对齐」证明改对了。无媒体出站字节另由 corpus 黄金样本整份钉住。

const (
	testAudioBase64  = "UklGRiQAAABXQVZF"
	testVideoURL     = "https://example.com/clip.mp4"
	testFilePDFBytes = "JVBERi0="
	testFileDataURL  = "data:application/pdf;base64," + testFilePDFBytes
)

func chatAudioBody() string {
	return `{"model":"m","messages":[{"role":"user","content":[` +
		`{"type":"input_audio","input_audio":{"data":"` + testAudioBase64 + `","format":"wav"}}]}]}`
}

func responsesAudioBody() string {
	return `{"model":"m","input":[{"type":"message","role":"user","content":[` +
		`{"type":"input_audio","data":"` + testAudioBase64 + `","format":"wav"}]}]}`
}

func chatVideoBody() string {
	return `{"model":"m","messages":[{"role":"user","content":[` +
		`{"type":"video_url","video_url":{"url":"` + testVideoURL + `"}}]}]}`
}

func responsesVideoBody() string {
	return `{"model":"m","input":[{"type":"message","role":"user","content":[` +
		`{"type":"input_video","video_url":{"url":"` + testVideoURL + `"}}]}]}`
}

func responsesFileBody() string {
	return `{"model":"m","input":[{"type":"message","role":"user","content":[` +
		`{"type":"input_file","filename":"a.pdf","file_data":"` + testFileDataURL + `"}]}]}`
}

// TestAudioCrossLineKeepsBothOpenAILines：chat 的嵌套 input_audio 与 responses 的平铺
// input_audio 双向互转，data / format 逐字保留且不记损。
func TestAudioCrossLineKeepsBothOpenAILines(t *testing.T) {
	t.Run("chat→responses", func(t *testing.T) {
		body, entries := encodeChatSource(t, chatAudioBody(), ProtocolOpenAIResponses)
		part := partOfType(t, responsesParts(t, body), "input_audio", body)
		if got, _ := stringField(part, "data"); got != testAudioBase64 {
			t.Fatalf("audio data 必须逐字保留，实际 %q：%s", got, body.MarshalCompact())
		}
		if got, _ := stringField(part, "format"); got != "wav" {
			t.Fatalf("audio format 必须逐字保留，实际 %q：%s", got, body.MarshalCompact())
		}
		if len(entries) != 0 {
			t.Fatalf("全程有承载位，不应有损失条目，实际 %+v", entries)
		}
	})

	t.Run("responses→chat", func(t *testing.T) {
		body, entries := encodeResponsesSource(t, responsesAudioBody(), ProtocolOpenAIChat)
		part := partOfType(t, chatParts(t, body), "input_audio", body)
		inner := fieldOrNil(part, "input_audio")
		if got, _ := stringField(inner, "data"); got != testAudioBase64 {
			t.Fatalf("audio data 必须逐字保留，实际 %q：%s", got, body.MarshalCompact())
		}
		if got, _ := stringField(inner, "format"); got != "wav" {
			t.Fatalf("audio format 必须逐字保留，实际 %q：%s", got, body.MarshalCompact())
		}
		if len(entries) != 0 {
			t.Fatalf("全程有承载位，不应有损失条目，实际 %+v", entries)
		}
	})
}

// TestVideoCrossLineKeepsBothOpenAILines：chat 的 video_url 与 responses 的 input_video 双向互转，
// URL 逐字保留且不记损（视频只有 URL 形态）。
func TestVideoCrossLineKeepsBothOpenAILines(t *testing.T) {
	t.Run("chat→responses", func(t *testing.T) {
		body, entries := encodeChatSource(t, chatVideoBody(), ProtocolOpenAIResponses)
		part := partOfType(t, responsesParts(t, body), "input_video", body)
		if got, _ := stringField(fieldOrNil(part, "video_url"), "url"); got != testVideoURL {
			t.Fatalf("video url 必须逐字保留，实际 %q：%s", got, body.MarshalCompact())
		}
		if len(entries) != 0 {
			t.Fatalf("全程有承载位，不应有损失条目，实际 %+v", entries)
		}
	})

	t.Run("responses→chat", func(t *testing.T) {
		body, entries := encodeResponsesSource(t, responsesVideoBody(), ProtocolOpenAIChat)
		part := partOfType(t, chatParts(t, body), "video_url", body)
		if got, _ := stringField(fieldOrNil(part, "video_url"), "url"); got != testVideoURL {
			t.Fatalf("video url 必须逐字保留，实际 %q：%s", got, body.MarshalCompact())
		}
		if len(entries) != 0 {
			t.Fatalf("全程有承载位，不应有损失条目，实际 %+v", entries)
		}
	})
}

// TestChatFileCrossLineCarriesInlineDataOnly：chat file 分片的内联数据跨到 responses 的
// input_file；file_id 是上游文件库句柄，跨线无载体，必须记损而不是静默丢弃。
func TestChatFileCrossLineCarriesInlineDataOnly(t *testing.T) {
	t.Run("file_data 跨线到 input_file", func(t *testing.T) {
		body, entries := encodeChatSource(t,
			`{"model":"m","messages":[{"role":"user","content":[{"type":"file",`+
				`"file":{"filename":"a.pdf","file_data":"`+testFileDataURL+`"}}]}]}`,
			ProtocolOpenAIResponses)
		part := partOfType(t, responsesParts(t, body), "input_file", body)
		if got, _ := stringField(part, "file_data"); got != testFileDataURL {
			t.Fatalf("file_data 必须原样回写 data URL，实际 %q：%s", got, body.MarshalCompact())
		}
		if got, _ := stringField(part, "filename"); got != "document.pdf" {
			t.Fatalf("filename 由目标线按媒体类型合成，实际 %q：%s", got, body.MarshalCompact())
		}
		if len(entries) != 0 {
			t.Fatalf("内联 PDF 全程有承载位，不应有损失条目，实际 %+v", entries)
		}
	})

	t.Run("file_id 记损不承载", func(t *testing.T) {
		body, entries := encodeChatSource(t,
			`{"model":"m","messages":[{"role":"user","content":[{"type":"file",`+
				`"file":{"file_id":"file_1"}}]}]}`,
			ProtocolOpenAIResponses)
		if !strings.Contains(body.MarshalCompact(), `"input":[]`) {
			t.Fatalf("file_id 没有跨线载体，不得出现在出站正文：%s", body.MarshalCompact())
		}
		found := false
		for _, entry := range entries {
			if entry.Capability == LossMedia && entry.Action == LossDropped && entry.Detail == "document.file_id" {
				found = true
			}
		}
		if !found {
			t.Fatalf("file_id 必须记 media/document.file_id 损失，实际 %+v", entries)
		}
	})

	t.Run("只有 filename 记损不发送", func(t *testing.T) {
		// 只给展示标签而没有字节：无从承载，必须记损而不是静默丢。
		body, entries := encodeChatSource(t,
			`{"model":"m","messages":[{"role":"user","content":[{"type":"file",`+
				`"file":{"filename":"a.pdf"}}]}]}`,
			ProtocolOpenAIResponses)
		if strings.Contains(body.MarshalCompact(), "a.pdf") {
			t.Fatalf("无字节的 file 分片不得送出：%s", body.MarshalCompact())
		}
		assertLossEntry(t, LossReport{Entries: entries}, LossMedia, string(LossDropped), "document.file_data")
	})

	t.Run("responses input_file 跨线到 chat file", func(t *testing.T) {
		body, entries := encodeResponsesSource(t, responsesFileBody(), ProtocolOpenAIChat)
		part := partOfType(t, chatParts(t, body), "file", body)
		file := fieldOrNil(part, "file")
		if got, _ := stringField(file, "file_data"); got != testFileDataURL {
			t.Fatalf("file_data 必须原样回写 data URL，实际 %q：%s", got, body.MarshalCompact())
		}
		if len(entries) != 0 {
			t.Fatalf("内联 PDF 全程有承载位，不应有损失条目，实际 %+v", entries)
		}
	})

	t.Run("非 PDF 文档不落 chat file 分片", func(t *testing.T) {
		// chat 的 file 分片只被保证支持 PDF；送其它类型换来的是上游 400（计入熔断）。
		body, entries := encodeResponsesSource(t,
			`{"model":"m","input":[{"type":"message","role":"user","content":[
			{"type":"input_file","filename":"a.csv","file_data":"data:text/csv;base64,YSxi"}]}]}`,
			ProtocolOpenAIChat)
		if strings.Contains(body.MarshalCompact(), "file") {
			t.Fatalf("非 PDF 文档不得送出 file 分片：%s", body.MarshalCompact())
		}
		if len(entries) == 0 {
			t.Fatal("被丢的非 PDF 文档必须记损")
		}
		assertLossEntry(t, LossReport{Entries: entries}, LossMedia, string(LossDropped), "document.media_type:text/csv")
	})

	t.Run("非本线认的文档类型不送 anthropic", func(t *testing.T) {
		// anthropic 的 document 只接受 pdf / text/plain；送 text/csv 会被 400。
		body, entries := encodeResponsesSource(t,
			`{"model":"m","max_output_tokens":8,"input":[{"type":"message","role":"user","content":[
			{"type":"input_file","filename":"a.csv","file_data":"data:text/csv;base64,YSxi"}]}]}`,
			ProtocolAnthropicMessages)
		if strings.Contains(body.MarshalCompact(), "YSxi") {
			t.Fatalf("本线不认的文档媒体类型不得送出：%s", body.MarshalCompact())
		}
		assertLossEntry(t, LossReport{Entries: entries}, LossMedia, string(LossDropped), "document.media_type:text/csv")
	})

	t.Run("text/plain 文档进 anthropic 原样承载", func(t *testing.T) {
		body, entries := encodeResponsesSource(t,
			`{"model":"m","max_output_tokens":8,"input":[{"type":"message","role":"user","content":[
			{"type":"input_file","filename":"a.txt","file_data":"data:text/plain;base64,aGk="}]}]}`,
			ProtocolAnthropicMessages)
		if !strings.Contains(body.MarshalCompact(), `"media_type":"text/plain"`) {
			t.Fatalf("text/plain 文档应落成 document 块：%s", body.MarshalCompact())
		}
		// 文件本身有承载位，但 anthropic 的 document 没有文件名成员：filename 必须落一条 info
		// 档损失（丢客户端标签不改变上游看到的正文），除此之外不该有任何损失。
		for _, entry := range entries {
			if entry.Capability != LossDocumentFilename {
				t.Fatalf("text/plain 文档已承载，只允许 filename 的 info 档损失，实际 %+v", entries)
			}
			if LossSeverityOf(entry.Capability, entry.Action) != SeverityInfo {
				t.Fatalf("filename 应为 info 档，实际 %+v", entry)
			}
		}
	})
}

// TestAudioVideoUnreachableOnAnthropicTarget：anthropic 线无音视频承载位，必须记 media 类损失
// （detail 带来源线），且同一消息里的文本不受连累。
func TestAudioVideoUnreachableOnAnthropicTarget(t *testing.T) {
	const (
		chatAudioWithText = `{"model":"m","messages":[{"role":"user","content":[` +
			`{"type":"text","text":"听"},` +
			`{"type":"input_audio","input_audio":{"data":"` + testAudioBase64 + `","format":"wav"}}]}]}`
		chatVideoWithText = `{"model":"m","messages":[{"role":"user","content":[` +
			`{"type":"text","text":"看"},` +
			`{"type":"video_url","video_url":{"url":"` + testVideoURL + `"}}]}]}`
		responsesAudioWithText = `{"model":"m","input":[{"type":"message","role":"user","content":[` +
			`{"type":"input_text","text":"听"},` +
			`{"type":"input_audio","data":"` + testAudioBase64 + `","format":"wav"}]}]}`
		responsesVideoWithText = `{"model":"m","input":[{"type":"message","role":"user","content":[` +
			`{"type":"input_text","text":"看"},` +
			`{"type":"input_video","video_url":{"url":"` + testVideoURL + `"}}]}]}`
	)
	cases := []struct {
		name   string
		source WireProtocol
		body   string
		detail string
	}{
		{"chat audio", ProtocolOpenAIChat, chatAudioWithText, "audio.unreachable:openai-chat"},
		{"chat video", ProtocolOpenAIChat, chatVideoWithText, "video.unreachable:openai-chat"},
		{"responses audio", ProtocolOpenAIResponses, responsesAudioWithText, "audio.unreachable:openai-responses"},
		{"responses video", ProtocolOpenAIResponses, responsesVideoWithText, "video.unreachable:openai-responses"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var body *Value
			var entries []LossEntry
			if testCase.source == ProtocolOpenAIChat {
				body, entries = encodeChatSource(t, testCase.body, ProtocolAnthropicMessages)
			} else {
				body, entries = encodeResponsesSource(t, testCase.body, ProtocolAnthropicMessages)
			}
			report := LossReport{Entries: entries}
			if body == nil {
				t.Fatal("编码必须成功（记损不崩）")
			}
			assertLossEntry(t, report, LossMedia, string(LossDropped), testCase.detail)
			marshaled := body.MarshalCompact()
			if strings.Contains(marshaled, testAudioBase64) || strings.Contains(marshaled, testVideoURL) {
				t.Fatalf("本线无音视频承载位，不得送出任何音视频字节：%s", marshaled)
			}
			// 同一消息里的文本不受连累（否则丢一个分片会把整条消息弄空）。
			if !strings.Contains(marshaled, `"听"`) && !strings.Contains(marshaled, `"看"`) {
				t.Fatalf("同消息的文本必须保留：%s", marshaled)
			}
		})
	}
}

// TestMediaSameLineRoundTripBytesUnchanged 钉住同线往返逐字节不变：三形态的同线出站字节与
// 改动前（本文件写下时的实测输出，见各 want 常量）完全一致。
//
// 逐字节而不是「语义等价」：同线出站是客户端原样内容的回写，file 分片的 filename 这类
// 枢纽不承载的展示标签也必须原封不动（故解码时留了原始分片，见 media.go 的文件头）。
func TestMediaSameLineRoundTripBytesUnchanged(t *testing.T) {
	cases := []struct {
		name string
		wire WireProtocol
		body string
		want string
	}{
		{
			name: "chat input_audio",
			wire: ProtocolOpenAIChat,
			body: chatAudioBody(),
			want: `{"model":"m","stream":false,"messages":[{"role":"user","content":[` +
				`{"type":"input_audio","input_audio":{"data":"` + testAudioBase64 + `","format":"wav"}}]}]}`,
		},
		{
			name: "chat video_url",
			wire: ProtocolOpenAIChat,
			body: chatVideoBody(),
			want: `{"model":"m","stream":false,"messages":[{"role":"user","content":[` +
				`{"type":"video_url","video_url":{"url":"` + testVideoURL + `"}}]}]}`,
		},
		{
			// filename 是枢纽不承载的展示标签：只有「原样回写原始分片」才能保住它。
			name: "chat file",
			wire: ProtocolOpenAIChat,
			body: `{"model":"m","messages":[{"role":"user","content":[{"type":"file",` +
				`"file":{"filename":"a.pdf","file_data":"` + testFileDataURL + `"}}]}]}`,
			want: `{"model":"m","stream":false,"messages":[{"role":"user","content":[{"type":"file",` +
				`"file":{"filename":"a.pdf","file_data":"` + testFileDataURL + `"}}]}]}`,
		},
		{
			name: "responses input_audio",
			wire: ProtocolOpenAIResponses,
			body: responsesAudioBody(),
			want: `{"model":"m","input":[{"type":"message","role":"user","content":[` +
				`{"type":"input_audio","data":"` + testAudioBase64 + `","format":"wav"}]}]}`,
		},
		{
			name: "responses input_video",
			wire: ProtocolOpenAIResponses,
			body: responsesVideoBody(),
			want: `{"model":"m","input":[{"type":"message","role":"user","content":[` +
				`{"type":"input_video","video_url":{"url":"` + testVideoURL + `"}}]}]}`,
		},
		{
			name: "responses input_file",
			wire: ProtocolOpenAIResponses,
			body: responsesFileBody(),
			want: `{"model":"m","input":[{"type":"message","role":"user","content":[` +
				`{"type":"input_file","filename":"a.pdf","file_data":"` + testFileDataURL + `"}]}]}`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := ConvertCtx{ClientFormat: FormatOpenAI, TargetProto: testCase.wire, Model: "m", ToWireToolName: NormalizeToolName}
			decoded, ok := DecodeRequest(testCase.wire, mustParsePayload(t, testCase.body), ctx)
			if !ok {
				t.Fatal("同线必须能解码")
			}
			encoded, ok := EncodeRequest(testCase.wire, decoded.Value, ctx)
			if !ok {
				t.Fatal("同线必须能编码")
			}
			if got := encoded.Body.MarshalCompact(); got != testCase.want {
				t.Fatalf("同线出站字节必须与改动前逐字节一致：\n got %s\nwant %s", got, testCase.want)
			}
			if len(decoded.Loss.Entries)+len(encoded.Loss.Entries) != 0 {
				t.Fatalf("同线可承载形态不应记损，实际 %+v / %+v", decoded.Loss.Entries, encoded.Loss.Entries)
			}
		})
	}
}

// TestNoMediaOutboundBytesUnchanged 是无媒体的回归钉子：纯文本请求的同线出站字节与改动前一致
// （新分支只按块类型分流，不得改变其余任何路径）。跨线的无媒体出站另有 corpus 黄金样本整份钉住。
func TestNoMediaOutboundBytesUnchanged(t *testing.T) {
	cases := []struct {
		name string
		wire WireProtocol
		body string
		want string
	}{
		{
			name: "chat 文本数组",
			wire: ProtocolOpenAIChat,
			body: `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`,
			want: `{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "chat 文本字符串",
			wire: ProtocolOpenAIChat,
			body: `{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
			want: `{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`,
		},
		{
			name: "responses 文本",
			wire: ProtocolOpenAIResponses,
			body: `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
			want: `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := ConvertCtx{ClientFormat: FormatOpenAI, TargetProto: testCase.wire, Model: "m", ToWireToolName: NormalizeToolName}
			decoded, _ := DecodeRequest(testCase.wire, mustParsePayload(t, testCase.body), ctx)
			encoded, _ := EncodeRequest(testCase.wire, decoded.Value, ctx)
			if got := encoded.Body.MarshalCompact(); got != testCase.want {
				t.Fatalf("无媒体时出站字节必须不变：\n got %s\nwant %s", got, testCase.want)
			}
		})
	}
}

// encodeResponsesSource 走真实编解码链：responses 源解码 → 目标线编码，返回出站正文与合并后的损失。
func encodeResponsesSource(t *testing.T, body string, target WireProtocol) (*Value, []LossEntry) {
	t.Helper()
	ctx := ConvertCtx{ClientFormat: FormatResponse, TargetProto: target, Model: "m", ToWireToolName: NormalizeToolName}
	decoded, ok := DecodeRequest(ProtocolOpenAIResponses, mustParsePayload(t, body), ctx)
	if !ok {
		t.Fatal("responses 线必须能解码")
	}
	encoded, ok := EncodeRequest(target, decoded.Value, ctx)
	if !ok {
		t.Fatalf("%s 线必须能编码", target)
	}
	entries := append(append([]LossEntry{}, decoded.Loss.Entries...), encoded.Loss.Entries...)
	return encoded.Body, entries
}

// chatParts 取 chat 出站首条消息的 content 分片。
func chatParts(t *testing.T, body *Value) []*Value {
	t.Helper()
	messages := body.ArrayField("messages")
	if len(messages) == 0 {
		t.Fatalf("chat 出站没有 messages：%s", body.MarshalCompact())
	}
	return messages[0].ArrayField("content")
}

// responsesParts 取 responses 出站首条 input 项的 content 分片。
func responsesParts(t *testing.T, body *Value) []*Value {
	t.Helper()
	input := body.ArrayField("input")
	if len(input) == 0 {
		t.Fatalf("responses 出站没有 input：%s", body.MarshalCompact())
	}
	return input[0].ArrayField("content")
}

func partOfType(t *testing.T, content []*Value, wantType string, body *Value) *Value {
	t.Helper()
	for _, part := range content {
		if partType, _ := stringField(part, "type"); partType == wantType {
			return part
		}
	}
	t.Fatalf("出站没有 %s 分片：%s", wantType, body.MarshalCompact())
	return nil
}
