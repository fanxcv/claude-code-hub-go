package forward

import (
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/gate"
)

// 量「每帧观测成本」：observeFrame 里的 JSON 解析曾是每帧无条件执行，
// 而绝大多数正文帧既无 usage 也无终态字段。判据是 ns/op 与 allocs/op 的前后差。
//
// 帧形状照真实上游：Anthropic 的 content_block_delta（正文增量，最大宗）、
// OpenAI 的 choices[].delta（正文增量）、Gemini 的 candidates[].content.parts。
// 每条线的正文帧都刻意**不含** usage/model/stop_reason，即预筛应当跳过的那一类。

func benchFrameBody() string {
	// 约 96 B 的正文增量，接近真实文本片大小。
	return `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` +
		"012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345" +
		`"}}`
}

func benchOpenAIBody() string {
	return `{"id":"chatcmpl-x","object":"chat.completion.chunk","created":1,"model":"gpt-5.6",` +
		`"choices":[{"index":0,"delta":{"content":"` +
		"012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345" +
		`"},"finish_reason":null}]}`
}

func benchGeminiBody() string {
	return `{"candidates":[{"content":{"parts":[{"text":"` +
		"012345678901234567890123456789012345678901234567890123456789012345678901234567890123456789012345" +
		`"}],"role":"model"},"index":0}]}`
}

func benchmarkFrame(b *testing.B, format convert.ClientFormat, event, body string) {
	b.ReportAllocs()
	for index := 0; index < b.N; index++ {
		observer := NewObserver(ObservationOptions{Format: format})
		observer.observeFrame(gate.Frame{Event: event, Data: body})
	}
}

func BenchmarkObserveFrameClaudeText(b *testing.B) {
	benchmarkFrame(b, convert.FormatClaude, "content_block_delta", benchFrameBody())
}

func BenchmarkObserveFrameOpenAIText(b *testing.B) {
	benchmarkFrame(b, convert.FormatOpenAI, "", benchOpenAIBody())
}

func BenchmarkObserveFrameGeminiText(b *testing.B) {
	benchmarkFrame(b, convert.FormatGemini, "", benchGeminiBody())
}

// BenchmarkObserveFrameClaudeUsage 是预筛必须放行的一类：终态帧。
func BenchmarkObserveFrameClaudeUsage(b *testing.B) {
	body := `{"type":"message_delta","delta":{"stop_reason":"end_turn"},` +
		`"usage":{"input_tokens":1234,"output_tokens":567}}`
	benchmarkFrame(b, convert.FormatClaude, "message_delta", body)
}
