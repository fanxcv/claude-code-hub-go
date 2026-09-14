package forward

import (
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// TestObserverSurvivesOversizedRequestEchoFrame 钉住「大回显帧不得让观测器丢掉整条流的事实」。
//
// 背景（2026-09-13 生产实测）：`ollama.com` 这类 openai-responses 上游会回显**完整请求体**
// （大上下文请求下单帧可达数百 KB）。观测器的分帧器此前只设 `MaxBufferedBytes`、**没设
// `BufferLimitExemption`**，于是超限时 `Push` 返回 `BufferLimitError`、`consumeFrames` 直接
// `return` 丢弃该批帧 → 该流的 `model` 与 `usage` **全部丢失**（生产表现：同一供应商短请求
// 正常、大上下文请求 97% 的落库行没有 usage/actual_response_model；claude 线无回显帧故不受影响）。
//
// 门控侧早就为此配了豁免（`gate.go` 的 `BufferLimitExemption` + `IsRequestEchoFrame`，
// 其注释原文：「openai-responses 的请求回显帧会带上完整请求体，单帧即可达数百 KB，
// 不应把『请求大』误判成『流异常』」）——观测器必须用同一判据。
func TestObserverSurvivesOversizedRequestEchoFrame(t *testing.T) {
	// 构造一个远超解析器缓冲上限（max(HeadBytes, 64KiB)）的请求回显帧。
	big := strings.Repeat("x", 200<<10)
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"model":"deepseek-v4.1-flash","echo":"` + big + `"}}`,
		"",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"hi"}`,
		"",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"model":"deepseek-v4.1-flash","usage":{"input_tokens":1234,"output_tokens":567}}}`,
		"",
		"",
	}, "\n")

	observer := newTestObserver(convert.FormatResponse, 4096, 4096)
	// 模拟真实喂入：按 8 KiB 分块（真实网络不会一次给完整帧）。
	for offset := 0; offset < len(stream); offset += 8 << 10 {
		end := offset + (8 << 10)
		if end > len(stream) {
			end = len(stream)
		}
		observer.Push([]byte(stream[offset:end]))
	}
	snapshot := observer.Snapshot()

	if snapshot.Model != "deepseek-v4.1-flash" {
		t.Fatalf("大回显帧之后仍应抽到 model，实际 %q（观测器把整条流丢了）", snapshot.Model)
	}
	if snapshot.Usage.InputTokens == nil || *snapshot.Usage.InputTokens != 1234 {
		t.Fatalf("大回显帧之后仍应抽到 usage.input_tokens=1234，实际 %#v", snapshot.Usage.InputTokens)
	}
	if snapshot.Usage.OutputTokens == nil || *snapshot.Usage.OutputTokens != 567 {
		t.Fatalf("大回显帧之后仍应抽到 usage.output_tokens=567，实际 %#v", snapshot.Usage.OutputTokens)
	}
}

// TestObserverSurvivesHugeSingleLineStream 砸住另一类上游形态：**整条流是一条超长行**。
//
// 为何单列：部分上游（实测 `ollama.com`）不是「一行一事件」，而是把整段回答作为一条长 JSON 行
// （NDJSON 风格、无 `data:` 前缀）。当行长超过分帧器上限时，`Push` 报错、`consumeFrames` 直接
// `return`，而**整条流只有这一行** → 该请求的 model/usage **全部丢失**（与生产现象一致：
// 同一供应商短请求正常、大上下文请求 97% 无 usage）。
func TestObserverSurvivesHugeSingleLineStream(t *testing.T) {
	// 一条 200 KiB 的行，头部是 model、尾部是 usage（真实长行里 key 就在这两处）。
	filler := strings.Repeat("x", 200<<10)
	line := `{"type":"response.completed","response":{"model":"deepseek-v4.1-flash","filler":"` + filler + `","usage":{"input_tokens":1234,"output_tokens":567}}}`
	stream := line + "\n"

	observer := newTestObserver(convert.FormatResponse, 4096, 4096)
	for offset := 0; offset < len(stream); offset += 8 << 10 {
		end := offset + (8 << 10)
		if end > len(stream) {
			end = len(stream)
		}
		observer.Push([]byte(stream[offset:end]))
	}
	snapshot := observer.Snapshot()

	if snapshot.Model != "deepseek-v4.1-flash" {
		t.Fatalf("超长单行流仍应抽到 model，实际 %q", snapshot.Model)
	}
	if snapshot.Usage.InputTokens == nil || *snapshot.Usage.InputTokens != 1234 {
		t.Fatalf("超长单行流仍应抽到 usage.input_tokens=1234，实际 %#v", snapshot.Usage.InputTokens)
	}
	if snapshot.Usage.OutputTokens == nil || *snapshot.Usage.OutputTokens != 567 {
		t.Fatalf("超长单行流仍应抽到 usage.output_tokens=567，实际 %#v", snapshot.Usage.OutputTokens)
	}
}
