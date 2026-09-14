package convert

import (
	"fmt"
	"strings"
	"testing"
)

// 本文件按**生产形状**加压：小语料用例不足以触发只在长历史下才出现的不确定性，
// 而生产上出事的那次请求正文约 207K tokens。
// 关注两条不变量：
//  1. 同一输入重复转换 ⇒ 出站字节恒定（repeatability_test.go 已在小语料上钉住，这里压大体积）；
//  2. **追加轮次后，此前历史的渲染必须原样保持** —— 这条才是上游前缀缓存真正依赖的：
//     上游按 messages 数组的字节前缀命中缓存，只要老内容有一字节被改写，缓存就从该点起全失效。

// responsesTurnBlock 造一轮「用户提问 → 思考 → 助手正文 → 工具调用 → 工具结果」的形状。
// 与 tests/load/protocol-conformance/corpus/requests.json 里
// request.openai-responses.{reasoning-sources,tool-roundtrip}.to.openai-chat 两条真实用例的
// item 形状保持一致，避免自造出解码器不认识的体。
func responsesTurnBlock(index int) string {
	return fmt.Sprintf(`{"type":"message","role":"user","content":[{"type":"input_text","text":"第 %d 轮问题：请读一下 a%d.txt"}]},`+
		`{"type":"reasoning","id":"rs_%d","summary":[{"type":"summary_text","text":"第 %d 轮先确认路径，再调用工具。"}]},`+
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"我先读 a%d.txt。"}]},`+
		`{"type":"function_call","id":"fc_%d","call_id":"call_%d","name":"read_file","arguments":"{\"path\":\"a%d.txt\"}"},`+
		`{"type":"function_call_output","call_id":"call_%d","output":"第 %d 轮的文件内容"}`,
		index, index, index, index, index, index, index, index, index, index)
}

// buildResponsesBody 造一份**长历史、多工具、带 instructions** 的 responses 请求体。
// turns 为轮数，instructionsBytes 用于把系统提示撑到生产量级。
func buildResponsesBody(turns int, instructionsBytes int) string {
	blocks := make([]string, 0, turns)
	for i := 1; i <= turns; i++ {
		blocks = append(blocks, responsesTurnBlock(i))
	}
	instructions := strings.Repeat("你是一个严谨的工程助手，先取证再下结论。", instructionsBytes/24+1)

	tools := make([]string, 0, 24)
	for i := 0; i < 24; i++ {
		tools = append(tools, fmt.Sprintf(`{"type":"function","name":"tool_%02d","description":"第 %d 个工具，用于读取与检索。","parameters":{"type":"object","properties":{"path":{"type":"string"},"limit":{"type":"integer"}},"required":["path"]}}`, i, i))
	}

	return fmt.Sprintf(`{"model":"deepseek-v4.1-flash","instructions":%q,"input":[%s],"tools":[%s],"reasoning":{"effort":"high"},"stream":true}`,
		instructions, strings.Join(blocks, ","), strings.Join(tools, ","))
}

// encodeResponsesBodyForTest 走「客户端 responses → 上游 chat」这条生产路径。
func encodeResponsesBodyForTest(t *testing.T, raw string) (string, string) {
	t.Helper()
	source, err := ParseJSON([]byte(raw))
	if err != nil {
		t.Fatalf("解析请求体失败: %v", err)
	}
	ctx := ConvertCtx{
		ClientFormat:   clientFormatOfProtocol(ProtocolOpenAIChat),
		TargetProto:    ProtocolOpenAIChat,
		Model:          caseModel(source),
		Stream:         responsesIsStream(source),
		ToWireToolName: NormalizeToolName,
	}
	decoded, ok := DecodeRequest(ProtocolOpenAIResponses, source, ctx)
	if !ok {
		t.Fatal("responses 线无解码器")
	}
	encoded, ok := EncodeRequest(ProtocolOpenAIChat, decoded.Value, ctx)
	if !ok {
		t.Fatal("chat 线无编码器")
	}
	messages, _ := encoded.Body.Get("messages")
	return encoded.Body.MarshalCompact(), messages.MarshalCompact()
}

// TestLargeBodyConversionIsRepeatable 在**生产量级**正文上钉字节恒定性。
func TestLargeBodyConversionIsRepeatable(t *testing.T) {
	raw := buildResponsesBody(120, 80*1024)
	t.Logf("加压正文大小: %d 字节", len(raw))

	firstBody, firstMessages := encodeResponsesBodyForTest(t, raw)
	t.Logf("出站体大小: %d 字节 / messages %d 字节", len(firstBody), len(firstMessages))
	t.Logf("第 1 轮 body sha256=%s", hashOf(firstBody))

	const rounds = 20
	for round := 2; round <= rounds; round++ {
		body, _ := encodeResponsesBodyForTest(t, raw)
		if hashOf(body) != hashOf(firstBody) {
			t.Fatalf("第 %d 轮出站体与第 1 轮不同（上游前缀缓存必失效）\n第 1 轮 sha256=%s\n第 %d 轮 sha256=%s\n%s",
				round, hashOf(firstBody), round, hashOf(body), byteDiff(firstBody, body))
		}
	}
	t.Logf("连续 %d 轮字节恒定", rounds)
}

// TestAppendingTurnsKeepsEarlierRendering 钉前缀不变量：
// 轮数从 k 增到 k+1 时，前 k 轮的渲染必须**逐字节不变**（否则上游前缀缓存从分叉点起全失效）。
//
// 生产含义：客户端的对话历史每轮只会在**尾部追加**。若我们的渲染会因为「后面多了东西」而
// 改写前面已发过的内容，那么每一轮的上游缓存都不可能命中——这正是 cache_read 骤降的机制。
func TestAppendingTurnsKeepsEarlierRendering(t *testing.T) {
	const (
		maxTurns          = 120
		instructionsBytes = 80 * 1024
	)

	var previousMessages string
	var previousTurns int
	for turns := 5; turns <= maxTurns; turns += 5 {
		raw := buildResponsesBody(turns, instructionsBytes)
		_, messages := encodeResponsesBodyForTest(t, raw)

		if previousMessages != "" {
			want := strings.TrimSuffix(previousMessages, "]")
			if !strings.HasPrefix(messages, want) {
				// 找出第一个不同字节的位置，便于定位改写点。
				divergence := firstDifference(want, messages)
				t.Fatalf("轮数 %d → %d 时，前 %d 轮的渲染被改写了 —— 上游前缀缓存必从该点起失效\n"+
					"分叉字节位置: %d（前一轮 messages 长 %d，本轮 %d）\n前一轮尾部: %s\n本轮同位置: %s",
					previousTurns, turns, previousTurns, divergence, len(want), len(messages),
					window(want, divergence), window(messages, divergence))
			}
		}
		previousMessages, previousTurns = messages, turns
	}
	t.Logf("轮数 5..%d 逐级追加：此前渲染始终逐字节保持（前缀不变量成立）", maxTurns)
}

// firstDifference 返回两个字符串首个不同字节的下标（无差异则返回较短者长度）。
func firstDifference(a, b string) int {
	limit := len(a)
	if len(b) < limit {
		limit = len(b)
	}
	for i := 0; i < limit; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return limit
}

// window 取以 at 为中心的一小段，用于失败时人眼定位。
func window(text string, at int) string {
	const radius = 120
	start := at - radius
	if start < 0 {
		start = 0
	}
	end := at + radius
	if end > len(text) {
		end = len(text)
	}
	return text[start:end]
}
