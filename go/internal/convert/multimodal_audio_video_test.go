package convert

import (
	"strings"
	"testing"
)

// 本文件钉住音频与视频的跨线承载（审计 audit-responses #2、audit-chat #18）：
//
//  1. `input_audio` 在 chat ↔ responses 两向都不丢；
//  2. `video_url` 在 chat ↔ responses 两向都不丢；
//  3. chat 的 `file`（内联 base64 PDF）跨到 responses / anthropic 不丢；
//  4. anthropic 目标线收到音频/视频时记损而不崩（该线无承载位）。
//
// 为何单测而不是只靠语料：这两类分片在语料快照里根本不存在（Node 侧同样 opaque → 跨线丢），
// 无法用「与 Node 对齐」证明改对了。

const (
	testAudioBase64 = "UklGRiQAAABXQVZF"
	testVideoURL    = "https://example.com/clip.mp4"
)

// chatAudioBody 造一份带 input_audio 分片的 chat 请求。
func chatAudioBody() string {
	return `{"model":"m","messages":[{"role":"user","content":[` +
		`{"type":"text","text":"听听"},` +
		`{"type":"input_audio","input_audio":{"data":"` + testAudioBase64 + `","format":"wav"}}]}]}`
}

// responsesAudioBody 造一份带 input_audio 分片的 responses 请求。
func responsesAudioBody() string {
	return `{"model":"m","input":[{"type":"message","role":"user","content":[` +
		`{"type":"input_text","text":"听听"},` +
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

// TestChatAudioReachesResponses：chat 的 input_audio 必须落成 responses 的 input_audio。
func TestChatAudioReachesResponses(t *testing.T) {
	body, entries := encodeChatSource(t, chatAudioBody(), ProtocolOpenAIResponses)
	part := responsesPartOfType(t, body, "input_audio")
	if got, _ := stringField(part, "data"); got != testAudioBase64 {
		t.Fatalf("audio data 必须逐字保留，实际 %q", got)
	}
	if got, _ := stringField(part, "format"); got != "wav" {
		t.Fatalf("audio format 必须逐字保留，实际 %q", got)
	}
	if len(entries) != 0 {
		t.Fatalf("全程可承载，不应有损失条目，实际 %+v", entries)
	}
}

// TestResponsesAudioReachesChat：responses 的 input_audio 必须落成 chat 的 input_audio 对象形态。
func TestResponsesAudioReachesChat(t *testing.T) {
	body, loss := convertResponsesRequestTo(t, responsesAudioBody(), ProtocolOpenAIChat)
	part := chatPartOfType(t, body, "input_audio")
	inner := fieldOrNil(part, "input_audio")
	if got, _ := stringField(inner, "data"); got != testAudioBase64 {
		t.Fatalf("audio data 必须逐字保留，实际 %q", got)
	}
	if got, _ := stringField(inner, "format"); got != "wav" {
		t.Fatalf("audio format 必须逐字保留，实际 %q", got)
	}
	if len(loss.Entries) != 0 {
		t.Fatalf("全程可承载，不应有损失条目，实际 %+v", loss.Entries)
	}
}

// TestChatVideoReachesResponses：chat 的 video_url 必须落成 responses 的 input_video。
func TestChatVideoReachesResponses(t *testing.T) {
	body, entries := encodeChatSource(t, chatVideoBody(), ProtocolOpenAIResponses)
	part := responsesPartOfType(t, body, "input_video")
	if got, _ := stringField(fieldOrNil(part, "video_url"), "url"); got != testVideoURL {
		t.Fatalf("video url 必须逐字保留，实际 %q", got)
	}
	if len(entries) != 0 {
		t.Fatalf("全程可承载，不应有损失条目，实际 %+v", entries)
	}
}

// TestResponsesVideoReachesChat：responses 的 input_video 必须落成 chat 的 video_url 分片。
func TestResponsesVideoReachesChat(t *testing.T) {
	body, loss := convertResponsesRequestTo(t, responsesVideoBody(), ProtocolOpenAIChat)
	part := chatPartOfType(t, body, "video_url")
	if got, _ := stringField(fieldOrNil(part, "video_url"), "url"); got != testVideoURL {
		t.Fatalf("video url 必须逐字保留，实际 %q", got)
	}
	if len(loss.Entries) != 0 {
		t.Fatalf("全程可承载，不应有损失条目，实际 %+v", loss.Entries)
	}
}

// TestChatFilePDFReachesOtherLines：chat 的内联 PDF 跨到 responses（input_file）与 anthropic
// （document）都不丢。
func TestChatFilePDFReachesOtherLines(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"user","content":[` +
		`{"type":"file","file":{"filename":"a.pdf","file_data":"data:application/pdf;base64,` + testFilePDFData + `"}}]}]}`

	t.Run("responses", func(t *testing.T) {
		out, entries := encodeChatSource(t, body, ProtocolOpenAIResponses)
		part := responsesPartOfType(t, out, "input_file")
		if got, _ := stringField(part, "file_data"); got != "data:application/pdf;base64,"+testFilePDFData {
			t.Fatalf("file_data 必须原样回写 data URL，实际 %q", got)
		}
		if len(entries) != 0 {
			t.Fatalf("全程可承载，不应有损失条目，实际 %+v", entries)
		}
	})

	t.Run("anthropic", func(t *testing.T) {
		out, entries := encodeChatSource(t, body, ProtocolAnthropicMessages)
		blocks := anthropicContentBlocks(t, out)
		if len(blocks) != 1 {
			t.Fatalf("应当只有一个 document 块，实际 %d 块：%s", len(blocks), out.MarshalCompact())
		}
		if got, _ := stringField(blocks[0], "type"); got != "document" {
			t.Fatalf("PDF 必须落成 document 块，实际 %q", got)
		}
		source := fieldOrNil(blocks[0], "source")
		if got, _ := stringField(source, "media_type"); got != "application/pdf" {
			t.Fatalf("document.source.media_type 应为 application/pdf，实际 %q", got)
		}
		if got, _ := stringField(source, "data"); got != testFilePDFData {
			t.Fatalf("document.source.data 应原样承载裸 base64，实际 %q", got)
		}
		for _, entry := range entries {
			if entry.Capability == LossDocument || strings.Contains(entry.Detail, "file") {
				t.Fatalf("PDF 已送出，不得再记 document / file 类损失：%+v", entries)
			}
		}
	})
}

// TestAnthropicTargetRecordsAudioAndVideoLoss：anthropic 线无音视频承载位，必须记损且不崩，
// 同一消息里的文本不得受连累。
func TestAnthropicTargetRecordsAudioAndVideoLoss(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		target WireProtocol
		detail string
	}{
		{"chat audio", chatAudioBody(), ProtocolAnthropicMessages, "audio"},
		{"chat video", chatVideoBody(), ProtocolAnthropicMessages, "video"},
		{"responses audio", responsesAudioBody(), ProtocolAnthropicMessages, "audio"},
		{"responses video", responsesVideoBody(), ProtocolAnthropicMessages, "video"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var body *Value
			var entries []LossEntry
			if strings.HasPrefix(testCase.name, "chat") {
				body, entries = encodeChatSource(t, testCase.body, testCase.target)
			} else {
				var loss LossReport
				body, loss = convertResponsesRequestTo(t, testCase.body, testCase.target)
				entries = loss.Entries
			}
			if body == nil {
				t.Fatal("编码必须成功（记损不崩）")
			}
			for _, entry := range entries {
				if entry.Action == LossDropped && strings.Contains(entry.Detail, testCase.detail) {
					return
				}
			}
			t.Fatalf("被丢的 %s 必须记损，实际 %+v", testCase.detail, entries)
		})
	}
}

// TestChatAudioFileIDIsReportedNotBoxed：chat 的 file_id 是上游文件库句柄，跨线无载体，
// 必须记损而不是装箱（装箱块跨线一律丢，等于同线也丢）。
func TestChatAudioFileIDIsReportedNotBoxed(t *testing.T) {
	_, entries := encodeChatSource(t,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"file","file":{"file_id":"file_1"}}]}]}`,
		ProtocolOpenAIResponses)
	for _, entry := range entries {
		if entry.Action == LossDropped && entry.Detail == "file.file_id" {
			return
		}
	}
	t.Fatalf("file_id 必须记损，实际 %+v", entries)
}

// responsesPartOfType 取 responses 出站首条 message 里指定 type 的 content 分片。
func responsesPartOfType(t *testing.T, body *Value, wantType string) *Value {
	t.Helper()
	input := body.ArrayField("input")
	if len(input) == 0 {
		t.Fatalf("responses 出站没有 input：%s", body.MarshalCompact())
	}
	return partOfType(t, input[0].ArrayField("content"), wantType, body)
}

// chatPartOfType 取 chat 出站首条消息里指定 type 的 content 分片。
func chatPartOfType(t *testing.T, body *Value, wantType string) *Value {
	t.Helper()
	messages := body.ArrayField("messages")
	if len(messages) == 0 {
		t.Fatalf("chat 出站没有 messages：%s", body.MarshalCompact())
	}
	return partOfType(t, messages[0].ArrayField("content"), wantType, body)
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
