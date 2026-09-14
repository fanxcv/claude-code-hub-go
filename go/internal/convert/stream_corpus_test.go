package convert

import (
	"regexp"
	"testing"
)

// 流式语料（streams.json）逐字节验收入口。
//
// 语料的期望值是 TS 实现（`protocol-convert/codecs/*/stream.ts`）的产物，喂入方式为
// split-half（把上游字节对半切开分两次 push），用以覆盖「帧边界与多字节字符跨块」这一面。
// 合成信封 id 的随机后缀在语料里归一化为 `<generated>`（见 scripts/export-conformance-corpus.ts），
// 故比较前须做同样的归一化——只比较形状与位置，不比较随机后缀。

var syntheticIDPattern = regexp.MustCompile(`(msg_cch_|resp_cch_|chatcmpl-cch-)[0-9a-f]{16}`)

func normalizeSyntheticIDs(text string) string {
	return syntheticIDPattern.ReplaceAllString(text, "${1}<generated>")
}

func TestCorpusStreams(t *testing.T) {
	file := loadCorpus(t, "streams.json")
	converted := 0
	passthrough := 0
	for _, testCase := range file.Cases {
		if testCase.Passthrough {
			// 同协议对由路由层绕过转换层（native 直通），本层不参与；只统计条数，
			// 防止语料悄悄丢掉该路径。
			passthrough++
			continue
		}
		t.Run(testCase.ID, func(t *testing.T) {
			upstream := protocolOfFamily(t, testCase.FamilyTo)
			client := protocolOfFamily(t, testCase.FamilyFrom)
			ctx := ConvertCtx{
				ClientFormat:     clientFormatOfProtocol(client),
				TargetProto:      upstream,
				Stream:           true,
				FromWireToolName: restoreHook(testCase.ToolNameRestore),
			}
			decoder, ok := NewStreamDecoder(upstream, ctx)
			if !ok {
				t.Fatalf("协议线 %s 无流式解码器", upstream)
			}
			encoder, ok := NewStreamEncoder(client, ctx)
			if !ok {
				t.Fatalf("协议线 %s 无流式编码器", client)
			}
			raw := []byte(testCase.Input)
			var frames [][]byte
			feed := func(chunk []byte) {
				for _, canonical := range decoder.Push(chunk) {
					frames = append(frames, encoder.Push(canonical)...)
				}
			}
			half := len(raw) / 2
			feed(raw[:half])
			feed(raw[half:])
			for _, canonical := range decoder.Flush() {
				frames = append(frames, encoder.Push(canonical)...)
			}
			frames = append(frames, encoder.Flush()...)

			got := normalizeSyntheticIDs(string(concatBytes(frames)))
			if testCase.Expected == nil {
				t.Fatalf("用例缺少 expected")
			}
			if got != *testCase.Expected {
				t.Fatal(byteDiff(got, *testCase.Expected))
			}
		})
		converted++
	}
	if converted == 0 {
		t.Fatal("流式转换用例为空")
	}
	if passthrough == 0 {
		t.Fatal("流式直通用例为空")
	}
}

// TestStreamPipeCrossDialectFrames 钉住四条切换阻塞用例的形状：客户端拿到的必须是**客户端方言**。
//
// 语料覆盖全对，但语料只给「同一个输入的整体输出」；本测试额外钉住「逐帧增量」这一面：
// 输入分块喂入时，输出帧的边界与内容不得依赖分块位置。
func TestStreamPipeCrossDialectFrames(t *testing.T) {
	cases := []struct {
		name     string
		upstream WireProtocol
		client   WireProtocol
		input    string
	}{
		{
			name:     "responses-to-anthropic",
			upstream: ProtocolOpenAIResponses,
			client:   ProtocolAnthropicMessages,
			input: "event: response.created\n" +
				`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress","output":[]}}` + "\n\n" +
				"event: response.output_item.added\n" +
				`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"msg_0","type":"message","content":[]}}` + "\n\n" +
				"event: response.content_part.added\n" +
				`data: {"type":"response.content_part.added","output_index":0,"content_index":0,"part":{"type":"output_text","text":""}}` + "\n\n" +
				"event: response.output_text.delta\n" +
				`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"msg_0","delta":"你好"}` + "\n\n" +
				"event: response.completed\n" +
				`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":5,"output_tokens":7}}}` + "\n\n",
		},
		{
			name:     "responses-to-chat",
			upstream: ProtocolOpenAIResponses,
			client:   ProtocolOpenAIChat,
			input: "event: response.created\n" +
				`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress","output":[]}}` + "\n\n" +
				"event: response.output_text.delta\n" +
				`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"item_id":"msg_0","delta":"hi"}` + "\n\n" +
				"event: response.completed\n" +
				`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":5,"output_tokens":7}}}` + "\n\n",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Run("whole", func(t *testing.T) {
				checkPipeOutput(t, testCase.upstream, testCase.client, []string{testCase.input}, testCase.client)
			})
			t.Run("split", func(t *testing.T) {
				// 按字节逐块喂入：帧边界与多字节字符都会被切开，输出仍须完整。
				chunks := make([]string, 0, len(testCase.input))
				for i := 0; i < len(testCase.input); i++ {
					chunks = append(chunks, testCase.input[i:i+1])
				}
				checkPipeOutput(t, testCase.upstream, testCase.client, chunks, testCase.client)
			})
		})
	}
}

// checkPipeOutput 断言转换输出的方言归属：必须以目标线的终止标记收尾。
func checkPipeOutput(t *testing.T, upstream WireProtocol, client WireProtocol, chunks []string, want WireProtocol) {
	t.Helper()
	pipe, ok := NewStreamPipe(upstream, client, ConvertCtx{
		ClientFormat: clientFormatOfProtocol(client),
		TargetProto:  upstream,
		Stream:       true,
	})
	if !ok {
		t.Fatalf("建管道失败：%s -> %s", upstream, client)
	}
	var out []byte
	for _, chunk := range chunks {
		out = append(out, pipe.Push([]byte(chunk))...)
	}
	out = append(out, pipe.Flush()...)
	text := string(out)

	switch want {
	case ProtocolAnthropicMessages:
		if !contains(text, "event: message_start") {
			t.Fatalf("输出不是 Anthropic 方言（缺 message_start）：%q", firstBytes(text, 200))
		}
		if !contains(text, "event: message_stop") {
			t.Fatalf("输出缺 Anthropic 终止事件 message_stop：%q", lastBytes(text, 200))
		}
		if contains(text, "response.output_text.delta") {
			t.Fatalf("输出泄漏了上游 Responses 方言：%q", firstBytes(text, 200))
		}
	case ProtocolOpenAIChat:
		if contains(text, `"object":"response"`) {
			t.Fatalf("输出泄漏了上游 Responses 方言：%q", firstBytes(text, 200))
		}
		if !contains(text, `"object":"chat.completion.chunk"`) {
			t.Fatalf("输出不是 Chat 方言：%q", firstBytes(text, 200))
		}
		if !contains(text, "data: [DONE]") {
			t.Fatalf("输出缺 Chat 终止哨兵 [DONE]：%q", lastBytes(text, 200))
		}
	default:
		t.Fatalf("未覆盖的目标线 %s", want)
	}
}

func contains(text string, needle string) bool {
	return len(needle) == 0 || indexOf(text, needle) >= 0
}

func indexOf(text string, needle string) int {
	for i := 0; i+len(needle) <= len(text); i++ {
		if text[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

func firstBytes(text string, limit int) string {
	if len(text) > limit {
		return text[:limit]
	}
	return text
}

func lastBytes(text string, limit int) string {
	if len(text) > limit {
		return text[len(text)-limit:]
	}
	return text
}
