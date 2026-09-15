package responsefix

import (
	"bytes"
	"strings"
	"testing"
)

// 本文件的用例对拍 Node 的 response-fixer.test.ts（编排层：非流式/流式两条路径、
// 惰性帧过滤、缓冲降级、审计条目形状），以及配置合并语义。

func runStream(fixer *StreamFixer, chunks ...string) (string, Audit) {
	var out bytes.Buffer
	for _, chunk := range chunks {
		piece := fixer.Write([]byte(chunk))
		if len(piece) > 0 {
			out.Write(piece)
		}
	}
	if tail := fixer.Flush(); len(tail) > 0 {
		out.Write(tail)
	}
	return out.String(), fixer.Audit()
}

func TestApplyNonStreamDisabledKeepsBodyUnchanged(t *testing.T) {
	body := []byte{0xef, 0xbb, 0xbf, '{', '"', 'a', '"', ':', '1', '}'}

	out, audit := ApplyNonStream(body, Config{})

	if !bytes.Equal(out, body) {
		t.Fatalf("全部子项关闭时应原样返回：got %q", out)
	}
	if audit.Hit {
		t.Fatalf("未修复任何东西时 Hit 应为 false")
	}
}

func TestApplyNonStreamRemovesBOMAndReportsAudit(t *testing.T) {
	body := append([]byte{0xef, 0xbb, 0xbf}, []byte(`{"a":1}`)...)

	out, audit := ApplyNonStream(body, DefaultConfig())

	if string(out) != `{"a":1}` {
		t.Fatalf("BOM 应被移除：got %q", out)
	}
	if !audit.Hit || !audit.EncodingApplied || audit.JSONApplied {
		t.Fatalf("审计应记编码修复命中、JSON 未命中：%+v", audit)
	}
	if audit.TotalBytesProcessed != int64(len(body)) {
		t.Fatalf("totalBytesProcessed 应为输入长度：got %d", audit.TotalBytesProcessed)
	}

	entry := audit.Entry()
	if entry["type"] != "response_fixer" || entry["scope"] != "response" || entry["hit"] != true {
		t.Fatalf("审计条目头字段不符：%v", entry)
	}
	fixers, ok := entry["fixersApplied"].([]map[string]any)
	if !ok || len(fixers) != 2 {
		t.Fatalf("非流式的 fixersApplied 应只有 encoding 与 json 两项：%v", entry["fixersApplied"])
	}
	if fixers[0]["fixer"] != "encoding" || fixers[1]["fixer"] != "json" {
		t.Fatalf("fixersApplied 顺序应为 encoding → json：%v", fixers)
	}
	if fixers[0]["details"] != detailRemovedUTF8BOM {
		t.Fatalf("encoding 的 details 应为 %q：%v", detailRemovedUTF8BOM, fixers[0])
	}
	if _, present := fixers[0]["applied"]; !present {
		t.Fatalf("fixersApplied 每项必须带 applied：%v", fixers[0])
	}
}

func TestApplyNonStreamRepairsTruncatedJSON(t *testing.T) {
	out, audit := ApplyNonStream([]byte(`{"key":`), DefaultConfig())

	if string(out) != `{"key":null}` {
		t.Fatalf("截断 JSON 应补成 {\\\"key\\\":null}：got %q", out)
	}
	if !audit.Hit || !audit.JSONApplied {
		t.Fatalf("审计应记 JSON 修复命中：%+v", audit)
	}
	// 非流式路径不给 sse 项（Node 的 includeSse=false）。
	entry := audit.Entry()
	fixers := entry["fixersApplied"].([]map[string]any)
	if len(fixers) != 2 {
		t.Fatalf("非流式不该出现 sse 项：%v", fixers)
	}
}

func TestStreamFixerBuffersAcrossChunksForTruncatedJSON(t *testing.T) {
	fixer := NewStreamFixer(DefaultConfig(), false)

	text, audit := runStream(fixer, `data: {"key":`, "\n\n")

	if text != "data: {\"key\":null}\n\n" {
		t.Fatalf("跨块截断 JSON 应被补齐：got %q", text)
	}
	if !audit.Hit || !audit.JSONApplied {
		t.Fatalf("审计应记 JSON 修复命中：%+v", audit)
	}
	if !audit.IncludeSSE {
		t.Fatalf("流式审计应含 sse 项")
	}
}

func TestStreamFixerLeavesValidStreamUntouched(t *testing.T) {
	fixer := NewStreamFixer(DefaultConfig(), false)

	text, audit := runStream(fixer, "data: {\"a\":1}\n\n")

	if text != "data: {\"a\":1}\n\n" {
		t.Fatalf("有效 SSE 应原样通过：got %q", text)
	}
	if audit.Hit {
		t.Fatalf("无修复时不该产生审计：%+v", audit)
	}
}

func TestStreamFixerFiltersInertChatCompletionChunks(t *testing.T) {
	fixer := NewStreamFixer(DefaultConfig(), true)
	emptyChunk := `{"id":"chatcmpl-dummy","object":"chat.completion.chunk","created":1780753978,"model":"gpt-5.5","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`
	delta := `{"type":"response.output_text.delta","delta":"Hi"}`
	completed := `{"type":"response.completed","response":{"id":"resp_test","object":"response"}}`
	input := strings.Join([]string{
		"data: " + emptyChunk,
		"",
		"event: response.output_text.delta",
		"data: " + delta,
		"",
		"event: response.completed",
		"data: " + completed,
		"",
	}, "\n")

	text, audit := runStream(fixer, input)

	if strings.Contains(text, "chat.completion.chunk") || strings.Contains(text, "chatcmpl-dummy") {
		t.Fatalf("惰性 chat chunk 应被过滤：got %q", text)
	}
	if !strings.HasPrefix(text, "event: response.output_text.delta") {
		t.Fatalf("过滤后应以真实事件开头：got %q", text)
	}
	if !strings.Contains(text, "response.completed") {
		t.Fatalf("其余事件应保留：got %q", text)
	}
	if !audit.Hit || !audit.SSEApplied {
		t.Fatalf("惰性帧过滤应计入 sse 修复：%+v", audit)
	}
	if audit.SSEDetails != detailFilteredInertCompletion {
		t.Fatalf("审计细节应为 %q：%q", detailFilteredInertCompletion, audit.SSEDetails)
	}
}

func TestStreamFixerKeepsMeaningfulChatCompletionChunks(t *testing.T) {
	chunks := map[string]string{
		"含内容":   `{"id":"chatcmpl-content","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"Hi"}}]}`,
		"含结束原因": `{"id":"chatcmpl-finish","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		"含用量":   `{"id":"chatcmpl-usage","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
	}
	for name, chunk := range chunks {
		t.Run(name, func(t *testing.T) {
			fixer := NewStreamFixer(DefaultConfig(), true)
			text, audit := runStream(fixer, "data: "+chunk+"\n\n")
			if !strings.Contains(text, "chat.completion.chunk") {
				t.Fatalf("有实质内容的 chat chunk 必须保留：got %q", text)
			}
			if audit.Hit {
				t.Fatalf("无过滤时不该产生审计：%+v", audit)
			}
		})
	}
}

// delta 不是对象（null/字符串）时 Node 判为惰性：这种帧对 responses 客户端毫无信息量。
func TestStreamFixerFiltersChunkWithNonObjectDelta(t *testing.T) {
	fixer := NewStreamFixer(DefaultConfig(), true)
	chunk := `{"id":"chatcmpl-odd","object":"chat.completion.chunk","choices":[{"index":0,"delta":null}]}`

	text, audit := runStream(fixer, "data: "+chunk+"\n\n")

	if strings.Contains(text, "chatcmpl-odd") {
		t.Fatalf("delta 非对象的 chat chunk 应被过滤：got %q", text)
	}
	if !audit.Hit || !audit.SSEApplied {
		t.Fatalf("过滤应计入 sse 修复：%+v", audit)
	}
}

func TestStreamFixerKeepsInertChunksForNonResponsesClients(t *testing.T) {
	fixer := NewStreamFixer(DefaultConfig(), false) // 客户端是 chat 线
	chunk := `{"id":"chatcmpl-chat-format","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`

	text, audit := runStream(fixer, "data: "+chunk+"\n\n")

	if !strings.Contains(text, "chatcmpl-chat-format") {
		t.Fatalf("非 responses 客户端不该过滤 chat 帧：got %q", text)
	}
	if audit.Hit {
		t.Fatalf("未过滤时不该产生审计：%+v", audit)
	}
}

func TestStreamFixerInertFilterKeepsCJKBytesExactly(t *testing.T) {
	fixer := NewStreamFixer(DefaultConfig(), true)
	emptyChunk := `{"id":"chatcmpl-dummy","object":"chat.completion.chunk","created":1780753978,"model":"gpt-5.5","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}`
	before := `{"type":"response.output_text.delta","delta":"你好，"}`
	after := `{"type":"response.output_text.delta","delta":"世界𠀀"}`
	input := strings.Join([]string{
		"data: " + before,
		"",
		"data: " + emptyChunk,
		"",
		"event: response.output_text.delta",
		"data: " + after,
		"",
		"",
	}, "\n")
	want := strings.Join([]string{
		"data: " + before,
		"",
		"event: response.output_text.delta",
		"data: " + after,
		"",
		"",
	}, "\n")

	text, _ := runStream(fixer, input)

	if text != want {
		t.Fatalf("多字节 CJK 内容应按字节原样保留：\n got %q\nwant %q", text, want)
	}
}

func TestStreamFixerInertFilterEarlyReturnWithoutMarker(t *testing.T) {
	data := []byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"你好\"}\n\n")

	result := filterInertChatCompletionChunks(data)

	if result.Applied {
		t.Fatalf("不含 chat.completion.chunk 标记的块不该被处理")
	}
	if &result.Data[0] != &data[0] {
		t.Fatalf("字节预扫描早退应复用输入切片")
	}
}

func TestStreamFixerDegradesToPassthroughBeyondMaxFixSize(t *testing.T) {
	config := DefaultConfig()
	config.MaxFixSize = 12
	fixer := NewStreamFixer(config, false)

	text, audit := runStream(fixer, `data: {"k":`, `"v"`)

	// 降级只是「不再修复并停止缓冲」：字节原样透传，缺失的右括号不会被补。
	if text != `data: {"k":"v"` {
		t.Fatalf("超限时应降级透传（缓冲先吐、当前块直通）：got %q", text)
	}
	if audit.Hit {
		t.Fatalf("降级后不该再产生修复审计：%+v", audit)
	}
}

func TestStreamFixerEmitsOnlyOnLineBoundary(t *testing.T) {
	fixer := NewStreamFixer(DefaultConfig(), false)

	if piece := fixer.Write([]byte(`data: {"a":`)); piece != nil {
		t.Fatalf("未构成完整行时不该吐出字节：got %q", piece)
	}
	piece := fixer.Write([]byte("1}\n"))

	if string(piece) != "data: {\"a\":1}\n" {
		t.Fatalf("行结束时才吐出：got %q", piece)
	}
}

func TestStreamFixerHandlesCRLFSplitAcrossChunks(t *testing.T) {
	fixer := NewStreamFixer(DefaultConfig(), false)

	// 第一块以 CR 结尾：需等下一块确认是否 CRLF，故不切分。
	if piece := fixer.Write([]byte("data: a\r")); piece != nil {
		t.Fatalf("块尾 CR 应等待下一块：got %q", piece)
	}
	piece := fixer.Write([]byte("\ndata: b\r\n"))

	if string(piece) != "data: a\ndata: b\n" {
		t.Fatalf("CRLF 应归一为 LF：got %q", piece)
	}
}

func TestParseConfigKeepsNodeMergeSemantics(t *testing.T) {
	defaults := DefaultConfig()

	if got := ParseConfig(nil); got != defaults {
		t.Fatalf("空配置应取出厂值：%+v", got)
	}
	if got := ParseConfig([]byte("null")); got != defaults {
		t.Fatalf("null 配置应取出厂值：%+v", got)
	}

	partial := ParseConfig([]byte(`{"fixEncoding": false}`))
	if partial.FixEncoding {
		t.Fatalf("显式 false 应覆盖默认 true")
	}
	if !partial.FixTruncatedJSON || !partial.FixSSEFormat {
		t.Fatalf("未出现的键应保留出厂值：%+v", partial)
	}
	if partial.MaxJSONDepth != defaults.MaxJSONDepth || partial.MaxFixSize != defaults.MaxFixSize {
		t.Fatalf("未出现的数值键应保留出厂值：%+v", partial)
	}

	explicitZero := ParseConfig([]byte(`{"maxFixSize": 0}`))
	if explicitZero.MaxFixSize != 0 {
		t.Fatalf("显式 0 必须覆盖默认（Node 的展开语义）：%+v", explicitZero)
	}

	production := ParseConfig([]byte(`{"maxFixSize": 1048576, "fixEncoding": true, "fixSseFormat": true, "maxJsonDepth": 200, "fixTruncatedJson": true}`))
	if production != defaults {
		t.Fatalf("生产里的那份配置等于出厂值：%+v", production)
	}

	if got := ParseConfig([]byte("{ 这不是 JSON")); got != defaults {
		t.Fatalf("解析失败应退回出厂值：%+v", got)
	}
}
