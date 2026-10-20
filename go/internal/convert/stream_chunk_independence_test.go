package convert

import (
	"fmt"
	"strings"
	"testing"
)

// 分块无关性的钉子：同一个上游字节流，按任意大小分块喂入，客户端输出必须逐字节一致。
//
// 为何要钉：切帧层（sseFrameBuffer）与工具参数对账都持有跨块状态。这两处在 v1.13.0 之前
// 用 `buffer += string(chunk)` 全量重拷 + 重扫，改后改为「只扫新增部分、只追加」。残留边界
// （`\r\n` 被切开、多字节字符被切开、帧分隔符跨块、超长单帧）一旦算错，表现就是「分块大小
// 影响输出字节」——这是转换层最隐蔽也最致命的一类回归，故以分块矩阵正面钉住。

// sseChunkSizes 是分块矩阵：覆盖单字节（每个边界都被切开）、7（非 2 的幂）、1 KiB、32 KiB
// （dataplane 的读缓冲大小）、256 KiB（超过整条夹具的长度，即整块喂入）。
var sseChunkSizes = []int{1, 7, 1024, 32768, 262144}

// bigToolArgsFrame 造一个超长工具参数单帧（256 KiB 载荷），用于钉住「未闭合长帧」路径。
func bigToolArgsFrame(size int) string {
	return chatChunk(chatChoice(`"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function",` +
		`"function":{"name":"f","arguments":` + jsonString(`{"blob":"`+strings.Repeat("a", size)+`"}`) + `}}]},"finish_reason":null`))
}

// streamEquivalenceCases 是分块无关性的上游夹具：三线各自的关键形态都进一份。
func streamEquivalenceCases() []struct {
	name     string
	upstream WireProtocol
	client   WireProtocol
	input    string
} {
	chatToolArgs := func() string {
		var builder strings.Builder
		builder.WriteString(chatChunk(chatChoice(`"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":""}}]},"finish_reason":null`)))
		for i := 0; i < 40; i++ {
			builder.WriteString(chatChunk(chatChoice(fmt.Sprintf(`"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"k%02d\":1,"}}]},"finish_reason":null`, i))))
		}
		builder.WriteString(chatChunk(chatChoice(`"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"end\":true}"}}]},"finish_reason":"tool_calls"`)))
		builder.WriteString("data: [DONE]\n\n")
		return builder.String()
	}
	scene := streamScenes()[4]
	crlf := strings.ReplaceAll(chatSceneStream(streamScenes()[2]), "\n", "\r\n")
	return []struct {
		name     string
		upstream WireProtocol
		client   WireProtocol
		input    string
	}{
		{name: "chat-tool-args", upstream: ProtocolOpenAIChat, client: ProtocolAnthropicMessages, input: chatToolArgs()},
		{name: "chat-scene-to-anthropic", upstream: ProtocolOpenAIChat, client: ProtocolAnthropicMessages, input: chatSceneStream(scene)},
		{name: "chat-crlf-to-anthropic", upstream: ProtocolOpenAIChat, client: ProtocolAnthropicMessages, input: crlf},
		{name: "anthropic-scene-to-chat", upstream: ProtocolAnthropicMessages, client: ProtocolOpenAIChat, input: anthropicSceneStream(scene)},
		{name: "responses-scene-to-chat", upstream: ProtocolOpenAIResponses, client: ProtocolOpenAIChat, input: responsesSceneStream(scene)},
		{name: "chat-big-frame-256k", upstream: ProtocolOpenAIChat, client: ProtocolOpenAIChat, input: bigToolArgsFrame(256 << 10)},
		{name: "chat-big-frame-multibyte", upstream: ProtocolOpenAIChat, client: ProtocolOpenAIChat, input: chatChunk(chatChoice(`"delta":{"content":`+jsonString(strings.Repeat("中文", 4096))+`},"finish_reason":null`)) + chatFrameEmpty()},
	}
}

// runStreamChunks 走完整转换路径（上游字节 → 客户端字节），按 chunkSize 分块喂入。
func runStreamChunks(t *testing.T, upstream WireProtocol, client WireProtocol, input string, chunkSize int) string {
	t.Helper()
	pipe, ok := NewStreamPipe(upstream, client, crossSceneCtx(upstream, client, true))
	if !ok {
		t.Fatalf("建管道失败：%s -> %s", upstream, client)
	}
	raw := []byte(input)
	var out []byte
	for start := 0; start < len(raw); start += chunkSize {
		out = append(out, pipe.Push(raw[start:min(start+chunkSize, len(raw))])...)
	}
	out = append(out, pipe.Flush()...)
	return normalizeSyntheticIDs(string(out))
}

// TestStreamOutputIsChunkIndependent 钉住「输出字节与分块大小无关」。
//
// 判据取整块喂入的输出为基准，其余分块必须与之逐字节相等。
func TestStreamOutputIsChunkIndependent(t *testing.T) {
	for _, testCase := range streamEquivalenceCases() {
		t.Run(testCase.name, func(t *testing.T) {
			whole := runStreamChunks(t, testCase.upstream, testCase.client, testCase.input, len(testCase.input))
			if whole == "" {
				t.Fatal("整块喂入不产出任何字节，夹具失效")
			}
			for _, chunkSize := range sseChunkSizes {
				got := runStreamChunks(t, testCase.upstream, testCase.client, testCase.input, chunkSize)
				if got != whole {
					t.Fatalf("分块 %d 字节时输出与整块喂入不同：\n%s", chunkSize, byteDiff(got, whole))
				}
			}
		})
	}
}

// TestSSEFrameBufferMatchesParseSSEFrames 钉住字节累积器与整体切帧函数语义等价。
//
// ParseSSEFrames 是「一次性切帧」的参照实现（测试侧与跨线验收都用它），累积器是流式改造后的
// 实际路径；两者的帧序列与残留必须逐帧相等，否则流式路径会与验收口径悄悄分离。
func TestSSEFrameBufferMatchesParseSSEFrames(t *testing.T) {
	inputs := []string{
		"data: a\n\ndata: b\n\n",
		"data: a\r\n\r\ndata: b\r\n\r\n",
		"data: a\r\r",
		"event: e\ndata: a\n\ndata: b\n\n",
		": heartbeat\n\ndata: a\n\n",
		"data:  two spaces\n\n",
		"data: a\n\npartial",
		"data: a\r",
		"data: a\r\n",
		"data: a\n\r\ndata: b\n\n",
		"data: \ntrailing-non-frame\n",
		"",
	}
	for _, input := range inputs {
		wantFrames, wantRest := ParseSSEFrames(input)
		for _, chunkSize := range sseChunkSizes {
			var buffer sseFrameBuffer
			var gotFrames []SSEFrame
			for start := 0; start < len(input); start += chunkSize {
				gotFrames = append(gotFrames, buffer.push([]byte(input[start:min(start+chunkSize, len(input))]))...)
			}
			if fmt.Sprint(gotFrames) != fmt.Sprint(wantFrames) {
				t.Fatalf("输入 %q 分块 %d：帧序列不等\n got=%v\nwant=%v", input, chunkSize, gotFrames, wantFrames)
			}
			if got := string(buffer.rest); got != wantRest {
				t.Fatalf("输入 %q 分块 %d：残留不等 got=%q want=%q", input, chunkSize, got, wantRest)
			}
		}
	}
}

// TestChatToolArgsAccumulationDoesNotCopyWholeHistory 钉住工具参数对账不再全量重拷。
//
// 同 stream_chat_linear_test.go 的判据：线性实现下「分配字节 / 参数长度」随增量数近似不变，
// `emitted + args` 的二次实现下它会随增量数线性增长。
func TestChatToolArgsAccumulationDoesNotCopyWholeHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("分配量随规模变化，短模式跳过")
	}
	measure := func(parts int) (uint64, int) {
		var builder strings.Builder
		builder.WriteString(chatChunk(chatChoice(`"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":""}}]},"finish_reason":null`)))
		for i := 0; i < parts; i++ {
			builder.WriteString(chatChunk(chatChoice(`"delta":{"tool_calls":[{"index":0,"function":{"arguments":"0123456789abcdef"}}]},"finish_reason":null`)))
		}
		builder.WriteString("data: [DONE]\n\n")
		raw := []byte(builder.String())

		decoder, _ := NewStreamDecoder(ProtocolOpenAIChat, ConvertCtx{ClientFormat: FormatOpenAI, TargetProto: ProtocolOpenAIChat, Stream: true})
		argsLen := 0
		baseline := totalAlloc()
		for _, chunk := range decoder.Push(raw) {
			if chunk.ArgsDelta != nil {
				argsLen += len(*chunk.ArgsDelta)
			}
		}
		decoder.Flush()
		return totalAlloc() - baseline, argsLen
	}

	smallBytes, smallLen := measure(50)
	largeBytes, largeLen := measure(2000)
	if smallLen == 0 || largeLen == 0 {
		t.Fatal("参数长度不得为零")
	}
	smallRatio := float64(smallBytes) / float64(smallLen)
	largeRatio := float64(largeBytes) / float64(largeLen)
	// 增量数放大 40 倍：线性实现比值基本不变（缓冲扩容摊还），二次实现增长数十倍。
	if largeRatio > smallRatio*2 {
		t.Fatalf("工具参数累积疑似二次：50 片时 %.1f B/字节，2000 片时 %.1f B/字节（比值 %.2f，上限 2）",
			smallRatio, largeRatio, largeRatio/smallRatio)
	}
}

// BenchmarkSSEFrameBufferClosedFrames 量「整块喂入成帧流」的切帧成本（小帧高频面）。
func BenchmarkSSEFrameBufferClosedFrames(b *testing.B) {
	var input strings.Builder
	for i := 0; i < 512; i++ {
		input.WriteString(chatChunk(chatChoice(`"delta":{"content":"x"},"finish_reason":null`)))
	}
	raw := []byte(input.String())
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	for i := 0; i < b.N; i++ {
		var buffer sseFrameBuffer
		if frames := buffer.push(raw); len(frames) != 512 {
			b.Fatalf("成帧数 %d，期望 512", len(frames))
		}
	}
}

// BenchmarkSSEFrameBufferLongFrame 量「未闭合超长单帧被小分块喂入」的切帧成本。
//
// 这一面是 O(帧长²) 回归的哨兵：旧实现每块重拷并重扫整个累积缓冲，1 KiB 分块喂 256 KiB
// 帧时约 20 ms/op、37 MiB/op；只扫新增部分后降到约 1 ms/op、3 MiB/op。
func BenchmarkSSEFrameBufferLongFrame(b *testing.B) {
	raw := []byte(bigToolArgsFrame(256 << 10))
	const chunkSize = 1024
	b.ReportAllocs()
	b.SetBytes(int64(len(raw)))
	for i := 0; i < b.N; i++ {
		var buffer sseFrameBuffer
		frames := 0
		for start := 0; start < len(raw); start += chunkSize {
			frames += len(buffer.push(raw[start:min(start+chunkSize, len(raw))]))
		}
		if frames != 1 {
			b.Fatalf("成帧数 %d，期望 1", frames)
		}
	}
}

// BenchmarkChatToolArgsDecode 量工具参数对账路径的每片成本（P0 的哨兵基准）。
//
// 判据：ns/op 与 B/op 应随片数线性增长。旧实现（`emitted + args`）下 2000 片约 8.8 ms /
// 46 MiB，8000 片约 70 ms / 659 MiB；改后分别约 3.7 ms / 6.1 MiB 与 14.8 ms / 25.5 MiB。
func BenchmarkChatToolArgsDecode(b *testing.B) {
	for _, parts := range []int{500, 2000, 8000} {
		var input strings.Builder
		input.WriteString(chatChunk(chatChoice(`"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":""}}]},"finish_reason":null`)))
		for i := 0; i < parts; i++ {
			input.WriteString(chatChunk(chatChoice(`"delta":{"tool_calls":[{"index":0,"function":{"arguments":",\"key_001\":12345678"}}]},"finish_reason":null`)))
		}
		input.WriteString("data: [DONE]\n\n")
		raw := []byte(input.String())
		b.Run(fmt.Sprintf("parts=%d", parts), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			for i := 0; i < b.N; i++ {
				decoder, _ := NewStreamDecoder(ProtocolOpenAIChat, ConvertCtx{ClientFormat: FormatOpenAI, TargetProto: ProtocolOpenAIChat, Stream: true})
				decoder.Push(raw)
				decoder.Flush()
			}
		})
	}
}
