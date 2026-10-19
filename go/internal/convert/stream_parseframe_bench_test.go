package convert

import (
	"testing"
)

// 量「每帧解码的 JSON 解析成本」。判据是 ns/op 与 allocs/op。
// 帧形状照真实上游：Anthropic content_block_delta、OpenAI choices[].delta、Gemini parts。

func benchParseFrameBody() string {
	return `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` +
		"012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345" +
		`"}}`
}

func BenchmarkParseFrameJSONTextDelta(b *testing.B) {
	frame := SSEFrame{Event: "content_block_delta", Data: benchParseFrameBody()}
	b.ReportAllocs()
	for index := 0; index < b.N; index++ {
		if parseFrameJSON(frame) == nil {
			b.Fatal("解析失败")
		}
	}
}

// BenchmarkAnthropicDecoderContentFrame 量整条解码路径（含 parseFrameJSON）的每帧成本。
func BenchmarkAnthropicDecoderContentFrame(b *testing.B) {
	start := []byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
	chunk := []byte("event: content_block_delta\ndata: " + benchParseFrameBody() + "\n\n")
	b.ReportAllocs()
	b.SetBytes(int64(len(chunk)))
	for index := 0; index < b.N; index++ {
		decoder, ok := NewStreamDecoder(ProtocolAnthropicMessages, ConvertCtx{})
		if !ok {
			b.Fatal("缺少解码器")
		}
		decoder.Push(start)
		if chunks := decoder.Push(chunk); len(chunks) == 0 {
			b.Fatal("正文帧必须产出枢纽块")
		}
	}
}
