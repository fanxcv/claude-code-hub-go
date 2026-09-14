package forward

import (
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// TestObserverOllamaResponsesUsageRepro 用**生产实测抓到的真实帧形状**复现
// 「Ollama Codex 供应商 700 行里 695 行 usage 全空」这一现象。
//
// 形状来自 https://ollama.com/v1/responses 的真实 SSE（2026-09-13 抓取）：
//   - 帧 0  response.created            → response.usage 为 **null**
//   - 帧 1  response.in_progress        → response.usage 为 null
//   - 帧 2  response.output_item.added
//   - 帧 3-7 response.reasoning_summary_text.delta
//   - 帧 8  response.reasoning_summary_text.done
//   - 帧 9  response.output_item.done
//   - 帧 10 response.incomplete         → response.usage **有真值**（input 31 / output 16）
//
// 与 Node 侧对照：Node 时代同一供应商同模型 814 行 usage 零缺失。
func TestObserverOllamaResponsesUsageRepro(t *testing.T) {
	head := `{"response":{"background":false,"completed_at":null,"created_at":1757700000,` +
		`"error":null,"id":"resp_repro","incomplete_details":null,"instructions":null,` +
		`"max_output_tokens":16,"metadata":{},"model":"deepseek-v4.1-flash","object":"response",` +
		`"output":[],"parallel_tool_calls":true,"previous_response_id":null,"prompt_cache_key":null,` +
		`"reasoning":{"effort":null,"summary":null},"safety_identifier":null,"service_tier":"default",` +
		`"status":"in_progress","store":false,"temperature":1,"text":{"format":{"type":"text"}},` +
		`"tool_choice":"auto","tools":[],"top_logprobs":0,"top_p":1,"truncation":"disabled","usage":null},` +
		`"sequence_number":0,"type":"%s"}`

	usage := `"usage":{"input_tokens":31,"input_tokens_details":{"cached_tokens":0},` +
		`"output_tokens":16,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":47}`

	var stream strings.Builder
	frames := []string{
		`event: response.created\ndata: ` + strings.Replace(head, "%s", "response.created", 1) + `\n\n`,
		`event: response.in_progress\ndata: ` + strings.Replace(head, "%s", "response.in_progress", 1) + `\n\n`,
		`event: response.output_item.added\ndata: {"item":{"id":"rs_1","type":"reasoning"},"output_index":0,"sequence_number":2,"type":"response.output_item.added"}\n\n`,
		`event: response.reasoning_summary_text.delta\ndata: {"delta":"think","item_id":"rs_1","output_index":0,"sequence_number":3,"summary_index":0,"type":"response.reasoning_summary_text.delta"}\n\n`,
		`event: response.reasoning_summary_text.done\ndata: {"item_id":"rs_1","output_index":0,"sequence_number":8,"summary_index":0,"text":"think","type":"response.reasoning_summary_text.done"}\n\n`,
		`event: response.output_item.done\ndata: {"item":{"id":"rs_1","type":"reasoning"},"output_index":0,"sequence_number":9,"type":"response.output_item.done"}\n\n`,
	}
	for _, frame := range frames {
		stream.WriteString(strings.ReplaceAll(frame, `\n`, "\n"))
	}
	terminal := `{"response":{` + strings.Replace(head, "%s", "response.incomplete", 1)[len(`{"response":{`):len(`{"response":{`)+strings.Index(head, `,"usage":null`)-len(`{"response":{`)] +
		`,` + usage + `},"sequence_number":10,"type":"response.incomplete"}`
	stream.WriteString("event: response.incomplete\ndata: " + terminal + "\n\n")

	observer := NewObserver(ObservationOptions{
		StartedAt: time.Now(),
		Format:    convert.FormatResponse,
	})
	// 按真实到达方式喂：整段一次读入（生产里也是小 chunk，形状等价）。
	observer.Push([]byte(stream.String()))

	snapshot := observer.Snapshot()
	t.Logf("一次喂完: Model=%q UsageSeen=%v Usage=%+v Frames=%d CompletionMarker=%v SawIncomplete=%v",
		snapshot.Model, snapshot.UsageSeen, snapshot.Usage, snapshot.Frames, snapshot.CompletionMarker, snapshot.SawIncomplete)

	if snapshot.Model != "deepseek-v4.1-flash" {
		t.Errorf("模型名应被取到，实际 %q", snapshot.Model)
	}
	if !snapshot.UsageSeen {
		t.Fatalf("应取到用量（真实流里 response.incomplete 带 usage），实际 UsageSeen=false")
	}
	if snapshot.Usage.InputTokens == nil || *snapshot.Usage.InputTokens != 31 {
		t.Errorf("input_tokens 应为 31，实际 %v", snapshot.Usage.InputTokens)
	}
	if snapshot.Usage.OutputTokens == nil || *snapshot.Usage.OutputTokens != 16 {
		t.Errorf("output_tokens 应为 16，实际 %v", snapshot.Usage.OutputTokens)
	}

	// 同上，但按**真实网络的到达方式**分块喂：小块 + 边界与帧边界不对齐。
	// 这是生产路径的形状（TCP 分片与读缓冲都不保证按帧对齐）。
	for _, size := range []int{1, 3, 7, 64, 512} {
		chunked := NewObserver(ObservationOptions{StartedAt: time.Now(), Format: convert.FormatResponse})
		payload := []byte(stream.String())
		for cursor := 0; cursor < len(payload); cursor += size {
			end := cursor + size
			if end > len(payload) {
				end = len(payload)
			}
			chunked.Push(payload[cursor:end])
		}
		got := chunked.Snapshot()
		t.Logf("分块 %d 字节: Model=%q UsageSeen=%v input=%v output=%v Frames=%d",
			size, got.Model, got.UsageSeen, got.Usage.InputTokens, got.Usage.OutputTokens, got.Frames)
		if !got.UsageSeen {
			t.Errorf("分块 %d 字节时丢失用量：这正是生产现象", size)
		}
	}
}
