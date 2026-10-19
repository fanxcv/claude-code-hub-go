package forward

import (
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// 字节预筛（frameGate / frameNeedsParse）的正确性判据只有一条：**受关注的键一个都不能漏**。
// 漏判不是「少解析一帧」而是静默丢终态事实——usage 丢了就是少计费，stop_reason 丢了就是
// 截断判错。故三线各一条：正文帧（应当跳过）与终态帧（必须解析）成对出现，且终态帧里
// 只带该线**真实形状**的键，不用人造键名。

// observeFrames 把若干 SSE 帧喂给观测器并返回快照。
func observeFrames(format convert.ClientFormat, frames ...string) Observation {
	observer := newTestObserver(format, DefaultStreamHeadBytes, DefaultStreamTailBytes)
	for _, frame := range frames {
		observer.Push([]byte(frame))
	}
	return observer.Snapshot()
}

// TestObserverPrefilterKeepsClaudeFacts 断言 Anthropic 线的键不被预筛漏掉。
func TestObserverPrefilterKeepsClaudeFacts(t *testing.T) {
	snapshot := observeFrames(convert.FormatClaude,
		// 正文增量：预筛应跳过（无 usage/model/stop_reason/error）。
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"正文\"}}\n\n",
		// 首帧带 message.model 与 message.usage（两者都在嵌套层，预筛靠 `"model"`/`"usage"` 命中）。
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-4-5\",\"usage\":{\"input_tokens\":11,\"cache_read_input_tokens\":3}}}\n\n",
		// 终态帧：只有 delta.stop_reason 与顶层 usage，没有 event 行。
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":7}}\n\n",
		"data: {\"type\":\"message_stop\"}\n\n",
	)
	if snapshot.Model != "claude-sonnet-4-5" {
		t.Fatalf("Model = %q，预筛漏掉了 message.model", snapshot.Model)
	}
	if !snapshot.UsageSeen {
		t.Fatal("UsageSeen 为假：预筛漏掉了 usage")
	}
	if snapshot.Usage.InputTokens == nil || *snapshot.Usage.InputTokens != 11 {
		t.Fatalf("InputTokens = %v，期望 11", snapshot.Usage.InputTokens)
	}
	if snapshot.Usage.OutputTokens == nil || *snapshot.Usage.OutputTokens != 7 {
		t.Fatalf("OutputTokens = %v，期望 7（跨帧合并后）", snapshot.Usage.OutputTokens)
	}
	if snapshot.StopReason != "max_tokens" {
		t.Fatalf("StopReason = %q，预筛漏掉了 delta.stop_reason", snapshot.StopReason)
	}
	if !snapshot.CompletionMarker {
		t.Fatal("CompletionMarker 为假：预筛漏掉了 message_stop")
	}
	if !snapshot.SawIncomplete {
		t.Fatal("SawIncomplete 为假：max_tokens 收尾应判为未完成")
	}
}

// TestObserverPrefilterKeepsOpenAIFacts 断言 OpenAI Chat 线的键不被预筛漏掉。
func TestObserverPrefilterKeepsOpenAIFacts(t *testing.T) {
	snapshot := observeFrames(convert.FormatOpenAI,
		// 正文增量帧：finish_reason 为 null 时也含 `"finish_reason"` 键，故这一帧仍会被解析
		// （预筛宁可多解析）。真正的跳过对象是没有该键的纯增量帧。
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"正文\"}}]}\n\n",
		"data: {\"model\":\"gpt-5.6\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":9,\"prompt_tokens_details\":{\"cached_tokens\":40}}}\n\n",
	)
	if snapshot.Model != "gpt-5.6" {
		t.Fatalf("Model = %q，预筛漏掉了 model", snapshot.Model)
	}
	if !snapshot.UsageSeen {
		t.Fatal("UsageSeen 为假：预筛漏掉了 usage")
	}
	// 枢纽口径：InputTokens 是扣掉缓存后的新鲜输入。
	if snapshot.Usage.InputTokens == nil || *snapshot.Usage.InputTokens != 60 {
		t.Fatalf("InputTokens = %v，期望 60（100-40）", snapshot.Usage.InputTokens)
	}
	if snapshot.StopReason != "length" {
		t.Fatalf("StopReason = %q，预筛漏掉了 finish_reason", snapshot.StopReason)
	}
	if !snapshot.CompletionMarker {
		t.Fatal("CompletionMarker 为假：预筛漏掉了 finish_reason")
	}
	if !snapshot.SawIncomplete {
		t.Fatal("SawIncomplete 为假：length 收尾应判为未完成")
	}
}

// TestObserverPrefilterKeepsGeminiFacts 断言 Gemini 线（含 gemini-cli）的键不被预筛漏掉。
func TestObserverPrefilterKeepsGeminiFacts(t *testing.T) {
	for _, format := range []convert.ClientFormat{convert.FormatGemini, convert.FormatGeminiCLI} {
		t.Run(string(format), func(t *testing.T) {
			snapshot := observeFrames(format,
				// 正文帧：无 usageMetadata/finishReason。
				"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"正文\"}],\"role\":\"model\"},\"index\":0}]}\n\n",
				"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"\"}],\"role\":\"model\"},\"finishReason\":\"MAX_TOKENS\",\"index\":0}],\"modelVersion\":\"gemini-3-pro\",\"usageMetadata\":{\"promptTokenCount\":120,\"cachedContentTokenCount\":20,\"candidatesTokenCount\":8,\"thoughtsTokenCount\":2}}\n\n",
			)
			if snapshot.Model != "gemini-3-pro" {
				t.Fatalf("Model = %q，预筛漏掉了 modelVersion", snapshot.Model)
			}
			if !snapshot.UsageSeen {
				t.Fatal("UsageSeen 为假：预筛漏掉了 usageMetadata")
			}
			if snapshot.Usage.InputTokens == nil || *snapshot.Usage.InputTokens != 100 {
				t.Fatalf("InputTokens = %v，期望 100（120-20）", snapshot.Usage.InputTokens)
			}
			if snapshot.Usage.ReasoningTokens == nil || *snapshot.Usage.ReasoningTokens != 2 {
				t.Fatalf("ReasoningTokens = %v，期望 2", snapshot.Usage.ReasoningTokens)
			}
			if snapshot.StopReason != "MAX_TOKENS" {
				t.Fatalf("StopReason = %q，预筛漏掉了 finishReason", snapshot.StopReason)
			}
			if !snapshot.SawIncomplete {
				t.Fatal("SawIncomplete 为假：MAX_TOKENS 收尾应判为未完成")
			}
		})
	}
}

// TestObserverPrefilterKeepsErrorFacts 断言错误帧与 Responses 终态帧不被预筛漏掉。
func TestObserverPrefilterKeepsErrorFacts(t *testing.T) {
	snapshot := observeFrames(convert.FormatResponse,
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"正文\"}\n\n",
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5.6\",\"status\":\"incomplete\",\"usage\":{\"input_tokens\":50,\"output_tokens\":5}}}\n\n",
	)
	if snapshot.Model != "gpt-5.6" {
		t.Fatalf("Model = %q，预筛漏掉了嵌套 model", snapshot.Model)
	}
	if !snapshot.UsageSeen {
		t.Fatal("UsageSeen 为假：预筛漏掉了嵌套 usage")
	}
	if !snapshot.CompletionMarker {
		t.Fatal("CompletionMarker 为假：预筛漏掉了 response.completed")
	}
	if snapshot.StopReason != "incomplete" {
		t.Fatalf("StopReason = %q，预筛漏掉了 response.status", snapshot.StopReason)
	}

	failed := observeFrames(convert.FormatResponse,
		"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"上游炸了\"}}}\n\n",
	)
	if failed.ErrorText != "上游炸了" {
		t.Fatalf("ErrorText = %q，预筛漏掉了 response.error.message", failed.ErrorText)
	}
}

// TestObserverPrefilterParsesUnknownFormat 断言未知方言不预筛（一律解析）。
func TestObserverPrefilterParsesUnknownFormat(t *testing.T) {
	if !frameNeedsParse(convert.ClientFormat("未知方言"), `{"anything":1}`) {
		t.Fatal("未知方言不得预筛")
	}
	if frameNeedsParse(convert.FormatClaude, `{"type":"content_block_delta","delta":{"text":"正文"}}`) {
		t.Fatal("无受关注键的正文帧应被预筛跳过")
	}
}

// TestObserverPrefilterParsesEveryFactKey 逐键钉住预筛表：每行只带**一个**事实键，
// 故删掉表里任何一条都会红。行内容照各线真实形状写，不从 frameGate 反推。
func TestObserverPrefilterParsesEveryFactKey(t *testing.T) {
	cases := []struct {
		name   string
		format convert.ClientFormat
		body   string
	}{
		{"claude-usage", convert.FormatClaude, `{"type":"message_delta","usage":{"output_tokens":1}}`},
		{"claude-model", convert.FormatClaude, `{"type":"message_start","message":{"model":"m"}}`},
		{"claude-stop-reason", convert.FormatClaude, `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`},
		{"claude-error", convert.FormatClaude, `{"type":"error","error":{"message":"x"}}`},
		{"claude-top-message", convert.FormatClaude, `{"type":"error","message":"x"}`},
		{"claude-message-stop", convert.FormatClaude, `{"type":"message_stop"}`},
		{"openai-usage", convert.FormatOpenAI, `{"choices":[],"usage":{"prompt_tokens":1}}`},
		{"openai-model", convert.FormatOpenAI, `{"model":"m","choices":[]}`},
		{"openai-finish-reason", convert.FormatOpenAI, `{"choices":[{"finish_reason":"stop"}]}`},
		{"openai-error", convert.FormatOpenAI, `{"error":{"message":"x"}}`},
		{"openai-top-message", convert.FormatOpenAI, `{"message":"x"}`},
		{"response-usage", convert.FormatResponse, `{"type":"response.completed","response":{"usage":{}}}`},
		{"response-model", convert.FormatResponse, `{"type":"response.completed","response":{"model":"m"}}`},
		{"response-status", convert.FormatResponse, `{"type":"response.completed","response":{"status":"completed"}}`},
		{"response-error", convert.FormatResponse, `{"type":"response.failed","response":{"error":{"message":"x"}}}`},
		{"response-top-message", convert.FormatResponse, `{"message":"x"}`},
		{"response-completed", convert.FormatResponse, `{"type":"response.completed","response":{}}`},
		{"response-done", convert.FormatResponse, `{"type":"response.done","response":{}}`},
		{"response-incomplete", convert.FormatResponse, `{"type":"response.incomplete","response":{}}`},
		{"gemini-usage", convert.FormatGemini, `{"candidates":[],"usageMetadata":{}}`},
		{"gemini-model-version", convert.FormatGemini, `{"modelVersion":"m","candidates":[]}`},
		{"gemini-finish-reason", convert.FormatGemini, `{"candidates":[{"finishReason":"STOP"}]}`},
		{"gemini-error", convert.FormatGemini, `{"error":{"message":"x"}}`},
		{"gemini-top-message", convert.FormatGemini, `{"message":"x"}`},
		{"gemini-cli-usage", convert.FormatGeminiCLI, `{"candidates":[],"usageMetadata":{}}`},
		{"gemini-cli-model-version", convert.FormatGeminiCLI, `{"modelVersion":"m","candidates":[]}`},
		{"gemini-cli-finish-reason", convert.FormatGeminiCLI, `{"candidates":[{"finishReason":"STOP"}]}`},
		{"gemini-cli-error", convert.FormatGeminiCLI, `{"error":{"message":"x"}}`},
		{"gemini-cli-top-message", convert.FormatGeminiCLI, `{"message":"x"}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if !frameNeedsParse(testCase.format, testCase.body) {
				t.Fatalf("预筛漏掉了 %s：%s", testCase.name, testCase.body)
			}
		})
	}
}
