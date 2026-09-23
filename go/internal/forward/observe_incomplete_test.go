package forward

import (
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// TestObserverIncompleteFinishes 各族沿用 observe_test.go 的收尾帧形制；
// Responses 的真实 Ollama 上游帧另由 TestObserverOllamaResponsesUsageRepro 钉住。
func TestObserverIncompleteFinishes(t *testing.T) {
	cases := []struct {
		name       string
		format     convert.ClientFormat
		frames     string
		incomplete bool
		want       TerminalKind
	}{
		{"chat 输出触顶", convert.FormatOpenAI, "data: {\"choices\":[{\"finish_reason\":\"length\"}]}\n\n", true, TerminalIncomplete},
		{"anthropic 输出触顶", convert.FormatClaude, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", true, TerminalIncomplete},
		{"gemini 输出触顶", convert.FormatGemini, "data: {\"candidates\":[{\"finishReason\":\"MAX_TOKENS\"}]}\n\n", true, TerminalIncomplete},
		{"gemini 非首候选终止", convert.FormatGemini, "data: {\"candidates\":[{}, {\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"totalTokenCount\":9}}\n\n", false, TerminalCompleted},
		{"gemini 非首候选触顶", convert.FormatGemini, "data: {\"candidates\":[{\"finishReason\":\"STOP\"}, {\"finishReason\":\"MAX_TOKENS\"}]}\n\n", true, TerminalIncomplete},
		{"chat 非首候选触顶", convert.FormatOpenAI, "data: {\"choices\":[{\"finish_reason\":\"stop\"}, {\"finish_reason\":\"length\"}]}\n\n", true, TerminalIncomplete},
		{"gemini 独立 usage 帧不算终止", convert.FormatGemini, "data: {\"usageMetadata\":{\"totalTokenCount\":9}}\n\n", false, TerminalUpstreamTruncated},
		{"chat 空 choices usage 帧不算终止", convert.FormatOpenAI, "data: {\"choices\":[],\"usage\":{\"completion_tokens\":9}}\n\n", false, TerminalUpstreamTruncated},
		{"chat DONE 正常收尾", convert.FormatOpenAI, "data: [DONE]\n\n", false, TerminalCompleted},
		{"anthropic 非法 DONE 不算 message_stop", convert.FormatClaude, "data: [DONE]\n\n", false, TerminalUpstreamTruncated},
		{"gemini 非法 DONE 不算 finishReason", convert.FormatGemini, "data: [DONE]\n\n", false, TerminalUpstreamTruncated},
		{"无终止标记的正文中途 FIN", convert.FormatOpenAI, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"function\":{\"arguments\":\"{\\\"query\\\":\"}}]}}]}\n\n", false, TerminalUpstreamTruncated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			observer := newTestObserver(tc.format, 4096, 4096)
			observer.Push([]byte(tc.frames))
			snapshot := observer.Snapshot()
			if got := terminalKindFor(PumpCompletion{}, snapshot); got != tc.want || snapshot.SawIncomplete != tc.incomplete {
				t.Fatalf("终态=%s, incomplete=%v, marker=%v; 期望 %s/%v", got, snapshot.SawIncomplete, snapshot.CompletionMarker, tc.want, tc.incomplete)
			}
		})
	}
}
