package convert

import (
	"encoding/json"
	"strings"
	"testing"
)

// chat 线流式正文对账的钉子。
//
// 背景（2026-09-21 生产取证）：上游存在「声明式回放」形态——尾帧（`delta` 或 `message`）载累计全文
// 而非新增片段。chat 解码器原先无对账，整篇被二次放出，客户端看到「前半 == 后半」而 usage 只计一份；
// 生产 chat 线实测重复率 37%（同机 responses/anthropic 线为 0——它们有对账）。
//
// 输入形态取自 /tmp/lanes/dup-msgframe 的实验矩阵（逐帧定义见那里的 upstream.py）。

// chatTestParts 是六段互不相同的可辨认片段：任一段重复都能在断言里看出来。
var chatTestParts = []string{"SEGONE-aaa7x、", "SEGTWO-bbb3y、", "SEGTHREE-ccc9z、", "SEGFOUR-ddd1w、", "SEGFIVE-eee5v、", "SEGSIX-fff8u。"}

func chatTestFull() string { return strings.Join(chatTestParts, "") }

func chatTestHalf() string { return strings.Join(chatTestParts[:3], "") }

func chatChunk(inner string) string {
	return "data: " + inner + "\n\n"
}

func chatChoice(deltaJSON string) string {
	return `{"id":"cc1","object":"chat.completion.chunk","choices":[{"index":0,` + deltaJSON + `}]}`
}

// chatFrameDelta 造一帧增量正文（`finish_reason` 为 null 或 "stop"）。
func chatFrameDelta(content string, finish string) string {
	fin := "null"
	if finish != "" {
		fin = `"` + finish + `"`
	}
	return chatChunk(chatChoice(`"delta":{"content":` + jsonString(content) + `},"finish_reason":` + fin))
}

// chatFrameMessage 造一帧以 `message` 承载正文的帧（OpenAI 规范里 message 是整条消息的载体）。
func chatFrameMessage(content string, finish string) string {
	fin := "null"
	if finish != "" {
		fin = `"` + finish + `"`
	}
	return chatChunk(chatChoice(`"message":{"role":"assistant","content":` + jsonString(content) + `},"finish_reason":` + fin))
}

func chatFrameEmpty() string {
	return chatChunk(chatChoice(`"delta":{},"finish_reason":"stop"`))
}

func jsonString(s string) string {
	raw, _ := json.Marshal(s)
	return string(raw)
}

// chatStreamModes 是实验矩阵的八种帧序列；want 为客户端应得的正文。
func chatStreamModes() map[string]struct {
	frames []string
	want   string
} {
	full, half := chatTestFull(), chatTestHalf()
	incremental := func() []string {
		out := make([]string, 0, len(chatTestParts))
		for _, p := range chatTestParts {
			out = append(out, chatFrameDelta(p, ""))
		}
		return out
	}
	withTail := func() []string { return append(incremental(), chatFrameEmpty()) }

	modes := map[string]struct {
		frames []string
		want   string
	}{}

	modes["baseline"] = struct {
		frames []string
		want   string
	}{withTail(), full}

	modes["delta_cum"] = struct {
		frames []string
		want   string
	}{append(incremental(), chatFrameDelta(full, "stop")), full}

	// delta_cum_mid：中途一帧发累计全文（末帧 finish_reason 仍为 null）。
	mid := make([]string, 0, len(chatTestParts)+2)
	for i, p := range chatTestParts {
		mid = append(mid, chatFrameDelta(p, ""))
		if i == 2 {
			mid = append(mid, chatFrameDelta(half, ""))
		}
	}
	modes["delta_cum_mid"] = struct {
		frames []string
		want   string
	}{append(mid, chatFrameEmpty()), full}

	modes["msg_tail"] = struct {
		frames []string
		want   string
	}{append(incremental(), chatFrameMessage(full, "stop")), full}

	msgMid := make([]string, 0, len(chatTestParts)+2)
	for i, p := range chatTestParts {
		msgMid = append(msgMid, chatFrameDelta(p, ""))
		if i == 2 {
			msgMid = append(msgMid, chatFrameMessage(half, ""))
		}
	}
	modes["msg_mid"] = struct {
		frames []string
		want   string
	}{append(msgMid, chatFrameEmpty()), full}

	modes["msg_null"] = struct {
		frames []string
		want   string
	}{append(incremental(), chatChunk(chatChoice(`"delta":null,"message":{"role":"assistant","content":`+jsonString(full)+`},"finish_reason":"stop"`))), full}

	modes["msg_partial"] = struct {
		frames []string
		want   string
	}{append(incremental(), chatFrameMessage(half, "stop")), full}

	modes["msg_only"] = struct {
		frames []string
		want   string
	}{[]string{chatFrameMessage(full, "stop")}, full}

	return modes
}

// chatDecode 解码一段上游字节并返回客户端可见正文与「开过的正文块数」。
//
// 分半喂入：帧边界与多字节字符跨块是最常见的分块切分形态（与语料验收同口径）。
func chatDecode(t *testing.T, frames []string) (string, int) {
	t.Helper()
	decoder, ok := NewStreamDecoder(ProtocolOpenAIChat, ConvertCtx{ClientFormat: FormatOpenAI, TargetProto: ProtocolOpenAIChat, Stream: true})
	if !ok {
		t.Fatal("chat 线无流式解码器")
	}
	var text strings.Builder
	blocks := 0
	collect := func(chunks []Chunk) {
		for _, c := range chunks {
			switch c.Kind {
			case ChunkBlockStart:
				if c.Block != nil && c.Block.Kind == BlockText {
					blocks++
				}
			case ChunkBlockDelta:
				if c.TextDelta != nil {
					text.WriteString(*c.TextDelta)
				}
			}
		}
	}
	raw := []byte(strings.Join(frames, ""))
	half := len(raw) / 2
	collect(decoder.Push(raw[:half]))
	collect(decoder.Push(raw[half:]))
	collect(decoder.Flush())
	return text.String(), blocks
}

// TestChatStreamReconcileMatrix 钉住八种帧序列的客户端可见正文。
func TestChatStreamReconcileMatrix(t *testing.T) {
	for name, mode := range chatStreamModes() {
		t.Run(name, func(t *testing.T) {
			got, blocks := chatDecode(t, mode.frames)
			if got != mode.want {
				t.Errorf("客户端正文不符：\n got=%q\nwant=%q", got, mode.want)
			}
			if mode.want != "" && blocks != 1 {
				t.Errorf("正文块数 = %d，期望 1（空块不得开出）", blocks)
			}
		})
	}
}

// TestChatStreamBaselineIsByteIdenticalToIncrementalConcat 钉住保守性：
// 纯增量上游的输出必须恰为增量拼接，逐字节相同——这是「不误伤常态」的硬判据。
func TestChatStreamBaselineIsByteIdenticalToIncrementalConcat(t *testing.T) {
	frames := make([]string, 0, len(chatTestParts)+1)
	for _, p := range chatTestParts {
		frames = append(frames, chatFrameDelta(p, ""))
	}
	frames = append(frames, chatFrameEmpty())

	got, _ := chatDecode(t, frames)
	if want := strings.Join(chatTestParts, ""); got != want {
		t.Fatalf("纯增量输出被改动：\n got=%q\nwant=%q", got, want)
	}
}

// TestChatStreamKeepsLegitimateRepeat 钉住对账不吞合法重复：
// 非声明式增量帧里连续两个相同片段（模型输出「哈哈」）必须两个都发。
func TestChatStreamKeepsLegitimateRepeat(t *testing.T) {
	frames := []string{
		chatFrameDelta("哈", ""),
		chatFrameDelta("哈", ""),
		chatFrameDelta("哈", ""),
		chatFrameEmpty(),
	}
	got, _ := chatDecode(t, frames)
	if want := "哈哈哈"; got != want {
		t.Fatalf("合法重复被吞：got=%q want=%q", got, want)
	}
}

// TestChatStreamReconcilesCumulativeToolArguments 钉住工具参数侧的同源对账：
// 累计型 arguments 若无对账会产出 `{...}{...}` 非法 JSON。
func TestChatStreamReconcilesCumulativeToolArguments(t *testing.T) {
	toolChoice := func(args string, finish string) string {
		fin := "null"
		if finish != "" {
			fin = `"` + finish + `"`
		}
		return `{"id":"cc1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":` + jsonString(args) + `}}]},"finish_reason":` + fin + `}]}`
	}
	frames := []string{
		chatChunk(toolChoice(`{"a":`, "")),
		chatChunk(toolChoice(`{"a":1}`, "stop")),
		chatChunk(`[DONE]`),
	}
	decoder, _ := NewStreamDecoder(ProtocolOpenAIChat, ConvertCtx{ClientFormat: FormatOpenAI, TargetProto: ProtocolOpenAIChat, Stream: true})
	var args strings.Builder
	collect := func(chunks []Chunk) {
		for _, c := range chunks {
			if c.Kind == ChunkBlockDelta && c.ArgsDelta != nil {
				args.WriteString(*c.ArgsDelta)
			}
		}
	}
	raw := []byte(strings.Join(frames, ""))
	collect(decoder.Push(raw))
	collect(decoder.Flush())

	got := args.String()
	if got != `{"a":1}` {
		t.Fatalf("工具参数被重复拼接：got=%q want=%q", got, `{"a":1}`)
	}
	if strings.Count(got, `{`) != 1 {
		t.Fatalf("工具参数含非法 JSON（花括号重开）：%q", got)
	}
}

// TestChatStreamReconcileEdges 钉住边界：空 delta 帧、首帧（emitted 为空）、content 数组形态。
func TestChatStreamReconcileEdges(t *testing.T) {
	t.Run("首个声明帧（emitted 为空）照常发出", func(t *testing.T) {
		got, blocks := chatDecode(t, []string{chatFrameMessage("你好", "stop")})
		if got != "你好" || blocks != 1 {
			t.Fatalf("got=%q blocks=%d", got, blocks)
		}
	})
	t.Run("空 delta 帧不产生任何输出", func(t *testing.T) {
		got, blocks := chatDecode(t, []string{
			chatFrameDelta("甲", ""),
			chatChunk(chatChoice(`"delta":{},"finish_reason":null`)),
			chatFrameEmpty(),
		})
		if got != "甲" || blocks != 1 {
			t.Fatalf("got=%q blocks=%d", got, blocks)
		}
	})
	t.Run("空 content 数组形态", func(t *testing.T) {
		got, _ := chatDecode(t, []string{
			chatChunk(chatChoice(`"delta":{"content":[]},"finish_reason":null`)),
			chatFrameDelta("乙", ""),
			chatFrameEmpty(),
		})
		if got != "乙" {
			t.Fatalf("got=%q", got)
		}
	})
	t.Run("content 数组带 text 分段", func(t *testing.T) {
		got, _ := chatDecode(t, []string{
			chatChunk(chatChoice(`"delta":{"content":[{"type":"text","text":"甲"}]},"finish_reason":null`)),
			chatChunk(chatChoice(`"delta":{"content":[{"type":"text","text":"乙"}]},"finish_reason":null`)),
			chatFrameEmpty(),
		})
		if got != "甲乙" {
			t.Fatalf("got=%q", got)
		}
	})
}

// TestChatReconcile 是判据本身的表驱动钉子（纯函数，不含 IO）。
func TestChatReconcile(t *testing.T) {
	cases := []struct {
		name         string
		emitted      string
		text         string
		declared     bool
		emittedParts int
		wantTail     string
		wantHit      bool
	}{
		{"累计型：严格扩展只补差额", "AB", "ABCD", false, 2, "CD", true},
		{"累计型：声明帧同样只补差额", "AB", "ABCD", true, 2, "CD", true},
		{"完整回放：声明帧一字不发", "ABCD", "ABCD", true, 2, "", true},
		{"完整回放：非声明帧但已发内容跨多帧", "ABCD", "ABCD", false, 4, "", true},
		{"合法重复：单帧相等则放行", "哈", "哈", false, 1, "", false},
		{"声明比已发短：一字不发", "ABCD", "AB", true, 4, "", true},
		{"非声明帧缩短：按普通增量放行", "ABCD", "AB", false, 4, "", false},
		{"无前缀关系：放行", "AB", "XY", true, 2, "", false},
		{"emitted 为空：放行", "", "AB", false, 0, "", false},
		{"emitted 为空且声明式：放行", "", "AB", true, 0, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tail, hit := chatReconcile(tc.emitted, tc.text, tc.declared, tc.emittedParts)
			if tail != tc.wantTail || hit != tc.wantHit {
				t.Fatalf("chatReconcile(%q,%q,%v,%d) = (%q,%v)，期望 (%q,%v)",
					tc.emitted, tc.text, tc.declared, tc.emittedParts, tail, hit, tc.wantTail, tc.wantHit)
			}
		})
	}
}
