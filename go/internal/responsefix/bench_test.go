package responsefix

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// 语料取默认配置下的典型帧：`data: ` 前缀 + 已闭合的合法 JSON（上游绝大多数帧都是这样）。

var benchClosedJSONLine = []byte(`data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hello"}}]}`)

var benchCompleteChunk = []byte(
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n",
)

var benchInertChunk = []byte(
	"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}],\"usage\":null}\n\n",
)

var benchLossyUTF8 = []byte("data: {\"text\":\"\xff\xfe bad utf8 \xe4\xbd\"}\n")

// BenchmarkFixMaybeDataJSONLine 钉住 A：每帧的 `data:` 行 JSON 预判开销。
func BenchmarkFixMaybeDataJSONLine(b *testing.B) {
	jsonFixer := JSONFixer{MaxDepth: DefaultConfig().MaxJSONDepth, MaxSize: DefaultConfig().MaxFixSize}
	b.ReportAllocs()
	for b.Loop() {
		if _, applied := fixMaybeDataJSONLine(benchClosedJSONLine, jsonFixer); applied {
			b.Fatal("已闭合的合法 JSON 不应被改")
		}
	}
}

// BenchmarkStreamFixerCompleteChunk 钉住 D：完整 chunk（末尾 LF）走 Write 的拷贝开销。
func BenchmarkStreamFixerCompleteChunk(b *testing.B) {
	fixer := NewStreamFixer(DefaultConfig(), false)
	out := make([]byte, 0, 4096)
	b.ReportAllocs()
	for b.Loop() {
		// Write 的返回值可能借用输入，按契约在下次喂入前消费完（这里只累加长度）。
		out = out[:0]
		out = append(out, fixer.Write(benchCompleteChunk)...)
	}
	if len(out) == 0 {
		b.Fatal("完整 chunk 应产出字节")
	}
}

// BenchmarkFilterInertChatCompletionChunks 钉住 E：惰性帧过滤的行扫描开销。
func BenchmarkFilterInertChatCompletionChunks(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		result := filterInertChatCompletionChunks(benchInertChunk)
		if !result.Applied {
			b.Fatal("夹具含惰性帧")
		}
	}
}

// BenchmarkEncodingFixerLossyUTF8 钉住 F：非法 UTF-8 的有损替换开销。
func BenchmarkEncodingFixerLossyUTF8(b *testing.B) {
	fixer := EncodingFixer{}
	b.ReportAllocs()
	for b.Loop() {
		result := fixer.Fix(benchLossyUTF8)
		if !result.Applied || result.Details != detailLossyUTF8DecodeEncode {
			b.Fatal("夹具应走有损修复")
		}
	}
}

// TestEndsWithClosingBracket 钉住 A 的预判口径：只看跳过尾部空白后的末字节。
func TestEndsWithClosingBracket(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{`{"a":1}`, true},
		{`[1,2]`, true},
		{"{\"a\":1}\n  ", true},
		{`{"a":1`, false},
		{`{"a":`, false},
		{``, false},
		{"   ", false},
		{`"text"`, false},
	}
	for _, item := range cases {
		if got := endsWithClosingBracket([]byte(item.input)); got != item.want {
			t.Fatalf("endsWithClosingBracket(%q) = %v，期望 %v", item.input, got, item.want)
		}
	}
}

// TestStreamFixerWriteMatchesBufferedPath 钉住 D 的等价性：完整块直通与「入缓冲 + take」
// 逐字节一致，含 CRLF、裸 CR、跨块残行、末尾无换行与非法 UTF-8，审计同口径。
func TestStreamFixerWriteMatchesBufferedPath(t *testing.T) {
	bodies := [][]byte{
		[]byte("data: {\"a\":1}\n\ndata: {\"b\":2}\n\n"),
		[]byte("data: {\"a\":1}\r\n\r\ndata: {\"b\":2}\r\n\r\n"),
		[]byte("data: {\"a\":1}\rdata: {\"b\":2}\r\r"),
		[]byte("data:{\"a\":1}\n\ndata: {\"trunc\":\n\n"),
		[]byte("event: e\ndata: [DONE]\n\n"),
		[]byte("data: {\"a\":1}\n\ndata: {\"b\":2}"),
		[]byte("data: {\"text\":\"\xff\xfe bad\"}\n\n"),
		[]byte("data: {\"object\":\"chat.completion.chunk\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: {\"x\":1}\n\n"),
	}
	for _, body := range bodies {
		for _, size := range []int{1, 2, 3, 5, 7, 11, 13, len(body)} {
			fastFixer := NewStreamFixer(DefaultConfig(), true)
			bufferedFixer := NewStreamFixer(DefaultConfig(), true)
			var got, want []byte
			for offset := 0; offset < len(body); offset += size {
				end := offset + size
				if end > len(body) {
					end = len(body)
				}
				// 按契约立刻消费 Write 的返回值（它可能借用输入切片）。
				got = append(got, fastFixer.Write(body[offset:end])...)
				want = append(want, writeThroughBuffer(bufferedFixer, body[offset:end])...)
			}
			got = append(got, fastFixer.Flush()...)
			want = append(want, bufferedFixer.Flush()...)
			if !bytes.Equal(got, want) {
				t.Fatalf("块大小 %d 输出不一致：\nbody=%q\ngot =%q\nwant=%q", size, body, got, want)
			}
			if gotAudit, wantAudit := fastFixer.Audit(), bufferedFixer.Audit(); gotAudit != wantAudit {
				t.Fatalf("块大小 %d 审计不一致：\nbody=%q\ngot =%+v\nwant=%+v", size, body, gotAudit, wantAudit)
			}
		}
	}
}

// writeThroughBuffer 只走「入缓冲 + take」路径，作为完整块直通路径的对照实现。
func writeThroughBuffer(f *StreamFixer, chunk []byte) []byte {
	f.audit.TotalBytesProcessed += int64(len(chunk))
	if f.passthrough {
		return chunk
	}
	if len(f.buffer)+len(chunk) > f.config.MaxFixSize {
		f.passthrough = true
		out := f.flushBuffer()
		return append(out, chunk...)
	}
	f.push(chunk)
	end := f.processableEndBytes()
	if end <= 0 {
		return nil
	}
	return f.applyFixers(f.take(end))
}

// TestStreamFixerCompleteChunkBorrowsInput 钉住 D 的借用契约：缓冲为空、块以 LF 收尾且
// 无需修复时，返回的切片就是输入切片（不再复制）。
func TestStreamFixerCompleteChunkBorrowsInput(t *testing.T) {
	fixer := NewStreamFixer(DefaultConfig(), false)
	chunk := []byte("data: {\"a\":1}\n\n")
	got := fixer.Write(chunk)
	// SSEFixer 在 `data:` 已带空格时不改字节，故整条链原样返回输入切片。
	if len(got) != len(chunk) || &got[0] != &chunk[0] {
		t.Fatalf("完整块未借用输入：got %q", got)
	}
}

var benchTruncatedPayload = []byte(`{"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hel`)

// refJSONValidLenient 是 A 改动前的实现，仅作 benchmem 前后对照用。
func refJSONValidLenient(data []byte) bool {
	if json.Valid(data) {
		return true
	}
	if bytes.Equal(data, []byte(strings.ToValidUTF8(string(data), "\uFFFD"))) {
		return false
	}
	return json.Valid([]byte(strings.ToValidUTF8(string(data), "\uFFFD")))
}

// BenchmarkJSONValidLenientClosed 钉住 A 在「已合法闭合 JSON」上的口径：预判救不了这一档，
// 因为它只是必要条件判定，json.Valid 仍要跑（两条候选路实测都不优于原实现）。
func BenchmarkJSONValidLenientClosed(b *testing.B) {
	payload := benchClosedJSONLine[len("data: "):]
	b.ReportAllocs()
	for b.Loop() {
		if !jsonValidLenient(payload) {
			b.Fatal("夹具应合法")
		}
	}
}

func BenchmarkRefJSONValidLenientClosed(b *testing.B) {
	payload := benchClosedJSONLine[len("data: "):]
	b.ReportAllocs()
	for b.Loop() {
		if !refJSONValidLenient(payload) {
			b.Fatal("夹具应合法")
		}
	}
}

// BenchmarkJSONValidLenientTruncated 钉住 A 真正的收益点：截断载荷原先要付
// json.Valid + ToValidUTF8 + json.Valid 三次全扫，现在一次预判即判出。
func BenchmarkJSONValidLenientTruncated(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if jsonValidLenient(benchTruncatedPayload) {
			b.Fatal("夹具应非法")
		}
	}
}

func BenchmarkRefJSONValidLenientTruncated(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if refJSONValidLenient(benchTruncatedPayload) {
			b.Fatal("夹具应非法")
		}
	}
}
