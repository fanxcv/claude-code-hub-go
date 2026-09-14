package forward

import (
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

func newTestObserver(format convert.ClientFormat, headBytes, tailBytes int) *Observer {
	return NewObserver(ObservationOptions{
		StartedAt: time.Unix(0, 0),
		Format:    format,
		HeadBytes: headBytes,
		TailBytes: tailBytes,
		Now:       func() time.Time { return time.Unix(0, int64(20*time.Millisecond)) },
	})
}

// TestObserverMarksTruncationWhenBodyExceedsWindow 断言超窗时保留首尾两段并带截断标记。
func TestObserverMarksTruncationWhenBodyExceedsWindow(t *testing.T) {
	observer := newTestObserver(convert.FormatClaude, 8, 8)
	observer.Push([]byte("HEAD0123"))
	observer.Push([]byte("MIDDLE-BYTES-THAT-ARE-DROPPED"))
	observer.Push([]byte("TAIL4567"))

	snapshot := observer.Snapshot()
	if !snapshot.Truncated {
		t.Fatal("超窗正文应标记截断")
	}
	if !strings.HasPrefix(snapshot.Snapshot, "HEAD0123") {
		t.Fatalf("窗口头部丢失: %q", snapshot.Snapshot)
	}
	if !strings.HasSuffix(snapshot.Snapshot, "TAIL4567") {
		t.Fatalf("窗口尾部丢失: %q", snapshot.Snapshot)
	}
	if !strings.Contains(snapshot.Snapshot, StreamTruncatedMarker) {
		t.Fatalf("缺少截断标记: %q", snapshot.Snapshot)
	}
	if want := int64(len("HEAD0123") + len("MIDDLE-BYTES-THAT-ARE-DROPPED") + len("TAIL4567")); snapshot.Bytes != want {
		t.Fatalf("Bytes = %d，期望 %d", snapshot.Bytes, want)
	}
	if snapshot.Chunks != 3 {
		t.Fatalf("Chunks = %d", snapshot.Chunks)
	}
}

// TestObserverKeepsWholeBodyWhenItFitsWindow 断言未超窗时不丢字节、不加标记。
func TestObserverKeepsWholeBodyWhenItFitsWindow(t *testing.T) {
	observer := newTestObserver(convert.FormatClaude, 64, 64)
	observer.Push([]byte("段一"))
	observer.Push([]byte("段二"))

	snapshot := observer.Snapshot()
	if snapshot.Truncated {
		t.Fatal("未超窗不应标记截断")
	}
	if snapshot.Snapshot != "段一段二" {
		t.Fatalf("窗口文本 = %q", snapshot.Snapshot)
	}
}

// TestObserverResidencyStaysBoundedForLargeStream 断言驻留量与正文长度无关。
//
// 这是本项目最重要的内存不变量：8 MiB 正文下，单流驻留必须远小于 1 MiB。
func TestObserverResidencyStaysBoundedForLargeStream(t *testing.T) {
	observer := newTestObserver(convert.FormatClaude, DefaultStreamHeadBytes, DefaultStreamTailBytes)
	chunk := make([]byte, 32<<10)
	for index := range chunk {
		chunk[index] = 'x'
	}
	total := 0
	for range 256 { // 256 × 32 KiB = 8 MiB
		observer.Push(chunk)
		total += len(chunk)
	}

	snapshot := observer.Snapshot()
	if snapshot.Bytes != int64(total) {
		t.Fatalf("Bytes = %d，期望 %d", snapshot.Bytes, total)
	}
	if !snapshot.Truncated {
		t.Fatal("8 MiB 正文应标记截断")
	}
	if snapshot.RetainedBytes >= 1<<20 {
		t.Fatalf("单流驻留 = %d 字节，超过 1 MiB", snapshot.RetainedBytes)
	}
}

// TestObserverExtractsUsageAndMarkers 按方言验证增量终态事实（用量、模型、终止标记、错误）。
func TestObserverExtractsUsageAndMarkers(t *testing.T) {
	frames := strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"model":"claude-sonnet-4-5","usage":{"input_tokens":100,"output_tokens":1,"cache_read_input_tokens":30,"cache_creation_input_tokens":5}}}`,
		"",
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"pong"}}`,
		"",
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`,
		"",
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		"",
		"",
	}, "\n")

	observer := newTestObserver(convert.FormatClaude, 4096, 4096)
	observer.Push([]byte(frames))
	snapshot := observer.Snapshot()

	if snapshot.Model != "claude-sonnet-4-5" {
		t.Fatalf("Model = %q", snapshot.Model)
	}
	if !snapshot.CompletionMarker {
		t.Fatal("缺少 message_stop 终止标记")
	}
	if snapshot.StopReason != "end_turn" {
		t.Fatalf("StopReason = %q", snapshot.StopReason)
	}
	if !snapshot.UsageSeen {
		t.Fatal("未解析到用量")
	}
	if got := deref(snapshot.Usage.InputTokens); got != 100 {
		t.Fatalf("InputTokens = %v", got)
	}
	if got := deref(snapshot.Usage.OutputTokens); got != 42 {
		t.Fatalf("OutputTokens = %v", got)
	}
	if got := deref(snapshot.Usage.CacheReadTokens); got != 30 {
		t.Fatalf("CacheReadTokens = %v", got)
	}
	if got := deref(snapshot.Usage.CacheWriteTokens); got != 5 {
		t.Fatalf("CacheWriteTokens = %v", got)
	}
	if snapshot.TTFT != 20*time.Millisecond {
		t.Fatalf("TTFT = %v", snapshot.TTFT)
	}
}

// TestObserverUsageFamilies 覆盖四条线的用量映射，重点是「输入计数含缓存」的两条线。
func TestObserverUsageFamilies(t *testing.T) {
	cases := []struct {
		name        string
		format      convert.ClientFormat
		frame       string
		wantInput   float64
		wantRead    float64
		wantOutput  float64
		wantReason  float64
		marker      bool
		expectUsage bool
	}{
		{
			name:        "openai-chat 扣掉缓存命中",
			format:      convert.FormatOpenAI,
			frame:       `data: {"model":"gpt-5","choices":[{"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":40},"completion_tokens_details":{"reasoning_tokens":7}}}`,
			wantInput:   60,
			wantRead:    40,
			wantOutput:  20,
			wantReason:  7,
			marker:      true,
			expectUsage: true,
		},
		{
			name:        "responses 扣掉缓存命中",
			format:      convert.FormatResponse,
			frame:       `data: {"type":"response.completed","response":{"model":"gpt-5","status":"completed","usage":{"input_tokens":50,"output_tokens":9,"input_tokens_details":{"cached_tokens":20},"output_tokens_details":{"reasoning_tokens":3}}}}`,
			wantInput:   30,
			wantRead:    20,
			wantOutput:  9,
			wantReason:  3,
			marker:      true,
			expectUsage: true,
		},
		{
			name:        "gemini 扣掉缓存内容",
			format:      convert.FormatGemini,
			frame:       `data: {"candidates":[{"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":80,"candidatesTokenCount":11,"cachedContentTokenCount":30,"thoughtsTokenCount":4}}`,
			wantInput:   50,
			wantRead:    30,
			wantOutput:  11,
			wantReason:  4,
			marker:      true,
			expectUsage: true,
		},
		{
			name:        "openai-chat 的 [DONE] 单独构成终止标记",
			format:      convert.FormatOpenAI,
			frame:       "data: [DONE]",
			wantInput:   0,
			wantRead:    0,
			wantOutput:  0,
			wantReason:  0,
			marker:      true,
			expectUsage: false,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			observer := newTestObserver(testCase.format, 4096, 4096)
			observer.Push([]byte(testCase.frame + "\n\n"))
			snapshot := observer.Snapshot()
			if snapshot.CompletionMarker != testCase.marker {
				t.Fatalf("CompletionMarker = %v", snapshot.CompletionMarker)
			}
			if snapshot.UsageSeen != testCase.expectUsage {
				t.Fatalf("UsageSeen = %v", snapshot.UsageSeen)
			}
			if !testCase.expectUsage {
				return
			}
			if deref(snapshot.Usage.InputTokens) != testCase.wantInput {
				t.Fatalf("InputTokens = %v", deref(snapshot.Usage.InputTokens))
			}
			if deref(snapshot.Usage.CacheReadTokens) != testCase.wantRead {
				t.Fatalf("CacheReadTokens = %v", deref(snapshot.Usage.CacheReadTokens))
			}
			if deref(snapshot.Usage.OutputTokens) != testCase.wantOutput {
				t.Fatalf("OutputTokens = %v", deref(snapshot.Usage.OutputTokens))
			}
			if deref(snapshot.Usage.ReasoningTokens) != testCase.wantReason {
				t.Fatalf("ReasoningTokens = %v", deref(snapshot.Usage.ReasoningTokens))
			}
		})
	}
}

// TestObserverRecordsProtocolErrorAndIncomplete 断言错误帧与未完成语义都被记录。
func TestObserverRecordsProtocolErrorAndIncomplete(t *testing.T) {
	observer := newTestObserver(convert.FormatResponse, 4096, 4096)
	observer.Push([]byte(`data: {"type":"response.incomplete","response":{"status":"incomplete"}}` + "\n\n"))
	observer.Push([]byte(`data: {"type":"error","error":{"message":"上游过载"}}` + "\n\n"))

	snapshot := observer.Snapshot()
	if !snapshot.SawIncomplete {
		t.Fatal("未记录 response.incomplete")
	}
	if snapshot.ErrorText != "上游过载" {
		t.Fatalf("ErrorText = %q", snapshot.ErrorText)
	}
	if snapshot.CompletionMarker {
		t.Fatal("错误帧不应构成终止标记")
	}
}

// TestObserverCountsFramesAcrossChunkBoundaries 断言跨 chunk 切分的帧仍被完整分类。
func TestObserverCountsFramesAcrossChunkBoundaries(t *testing.T) {
	full := "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	observer := newTestObserver(convert.FormatClaude, 4096, 4096)
	for index := 0; index < len(full); index += 3 {
		end := index + 3
		if end > len(full) {
			end = len(full)
		}
		observer.Push([]byte(full[index:end]))
	}
	if !observer.Snapshot().CompletionMarker {
		t.Fatalf("跨 chunk 的终止帧未被识别: %q", observer.Snapshot().Snapshot)
	}
}

// TestObserverIgnoresEmptyChunks 断言空 chunk 不改变计数与首字节时刻。
func TestObserverIgnoresEmptyChunks(t *testing.T) {
	observer := newTestObserver(convert.FormatClaude, 64, 64)
	observer.Push(nil)
	observer.Push([]byte{})
	snapshot := observer.Snapshot()
	if snapshot.Bytes != 0 || snapshot.Chunks != 0 || !snapshot.FirstByteAt.IsZero() {
		t.Fatalf("空 chunk 影响了观测: %+v", snapshot)
	}
}

func deref(value *float64) float64 {
	if value == nil {
		return -1
	}
	return *value
}
