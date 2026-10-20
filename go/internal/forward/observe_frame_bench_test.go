package forward

import (
	"testing"
	"time"

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

// benchmarkFrame 量「流的首帧」：观测器每轮新建，故首次 Push 前没有任何打戳。
//
// 改造后它比改造前贵一档——首帧要多跑一次内容分类（这是口径统一的单价，见
// BenchmarkObserveFrameClaudeNeutral 的注释）。稳态成本见 benchmarkSettledFrame。
func benchmarkFrame(b *testing.B, format convert.ClientFormat, event, body string) {
	b.ReportAllocs()
	for index := 0; index < b.N; index++ {
		observer := NewObserver(ObservationOptions{Format: format})
		observer.observeFrame(gate.Frame{Event: event, Data: body})
	}
}

// benchmarkSettledFrame 量「首个内容帧之后」的每帧成本——流的主体形态。
//
// 它必须与改造前同量级：打戳后 markFirstContent 首行即返回，故 TTFT 口径不随流长付费。
func benchmarkSettledFrame(b *testing.B, format convert.ClientFormat, event, body string) {
	b.ReportAllocs()
	stamped := time.Unix(0, 0)
	for index := 0; index < b.N; index++ {
		observer := NewObserver(ObservationOptions{Format: format})
		// 直接置位打戳状态：这正是「首帧之后」的观测器形态。
		observer.observation.FirstContentAt = stamped
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

// 稳态三条：与上面同帧、同装置，只差「已打戳」这一位。两组的差就是 TTFT 口径的稳态代价。
func BenchmarkObserveSettledClaudeText(b *testing.B) {
	benchmarkSettledFrame(b, convert.FormatClaude, "content_block_delta", benchFrameBody())
}

func BenchmarkObserveSettledOpenAIText(b *testing.B) {
	benchmarkSettledFrame(b, convert.FormatOpenAI, "", benchOpenAIBody())
}

func BenchmarkObserveSettledGeminiText(b *testing.B) {
	benchmarkSettledFrame(b, convert.FormatGemini, "", benchGeminiBody())
}

func BenchmarkObserveSettledClaudeUsage(b *testing.B) {
	body := `{"type":"message_delta","delta":{"stop_reason":"end_turn"},` +
		`"usage":{"input_tokens":1234,"output_tokens":567}}`
	benchmarkSettledFrame(b, convert.FormatClaude, "message_delta", body)
}

// 首个内容帧之前的两类：中性帧（必解析才能判定它不含内容，故是内容口径的固有代价）
// 与心跳帧（预筛直接挡掉，故改造没有让它多付钱）。
func BenchmarkObserveFrameClaudeNeutral(b *testing.B) {
	body := `{"type":"message_start","message":{"id":"msg_1","model":"claude-sonnet-4-5"}}`
	for index := 0; index < b.N; index++ {
		observer := NewObserver(ObservationOptions{Format: convert.FormatClaude})
		observer.observeFrame(gate.Frame{Event: "message_start", Data: body})
	}
}

func BenchmarkObserveFrameClaudeHeartbeat(b *testing.B) {
	for index := 0; index < b.N; index++ {
		observer := NewObserver(ObservationOptions{Format: convert.FormatClaude})
		observer.observeFrame(gate.Frame{Event: "ping", Data: `{"type":"ping"}`})
	}
}
