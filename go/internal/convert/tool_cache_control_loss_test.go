package convert

import (
	"strings"
	"testing"
)

// 本文件钉住「工具块自身的 cache_control 不再在解码期无声消失」这一行为：
//  1. anthropic 解码侧必须把 tool_use / tool_result 块上的 cache_control 解为 CacheHint
//     （此前只解 text / image / document / 工具定义，工具块上的提示在解码即丢，下游连损失都记不出来）；
//  2. 目标线（chat / responses）没有块级承载位时，各自编码器必须记 LossCacheControl；
//  3. 没有提示时不得凭空记损（防误报，与 assertLossEntry 的「不多不少」纪律同口径）。
//
// 不回写不测：anthropic -> anthropic 走原生直通（selection.IsNativePair），不经编码器。

const toolCacheControlPayload = `{
	"model":"claude-sonnet-4-5","max_tokens":1024,
	"messages":[
		{"role":"user","content":[{"type":"text","text":"hi"}]},
		{"role":"assistant","content":[
			{"type":"text","text":"calling"},
			{"type":"tool_use","id":"toolu_1","name":"read","input":{"path":"/a"},
			 "cache_control":{"type":"ephemeral","ttl":"5m"}}
		]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"r1"}],
			 "cache_control":{"type":"ephemeral"}}
		]}
	]
}`

const toolCacheControllessPayload = `{
	"model":"claude-sonnet-4-5","max_tokens":1024,
	"messages":[
		{"role":"user","content":[{"type":"text","text":"hi"}]},
		{"role":"assistant","content":[
			{"type":"text","text":"calling"},
			{"type":"tool_use","id":"toolu_1","name":"read","input":{"path":"/a"}}
		]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"r1"}]}
		]}
	]
}`

// decodeAnthropicToolCacheControl 解出 payload，并断言前置条件：工具块与工具结果块都带上了 CacheHint。
func decodeAnthropicToolCacheControl(t *testing.T, payload string) *Request {
	t.Helper()
	decoded, ok := DecodeRequest(ProtocolAnthropicMessages, mustParsePayload(t, payload), ConvertCtx{})
	if !ok {
		t.Fatal("anthropic 线必须能解码")
	}
	var callHint, resultHint *CacheHint
	for _, item := range decoded.Value.Items {
		switch item.Kind {
		case ItemMessage:
			for _, block := range item.Blocks {
				if block.Kind == BlockToolCall {
					callHint = block.CacheHint
				}
			}
		case ItemToolResult:
			resultHint = item.CacheHint
		}
	}
	if callHint == nil {
		t.Fatal("tool_use 块上的 cache_control 必须解为 Block.CacheHint（否则解码即丢，下游无从记损）")
	}
	if callHint.TTL != "5m" || !callHint.HasTTL {
		t.Fatalf("tool_use 的 cache_control.ttl 必须原样解出：%+v", callHint)
	}
	if resultHint == nil {
		t.Fatal("tool_result 块上的 cache_control 必须解为 Item.CacheHint")
	}
	return decoded.Value
}

func countCacheControlLoss(report LossReport, detail string) int {
	count := 0
	for _, entry := range report.Entries {
		if entry.Capability == LossCacheControl && entry.Action == LossDropped && entry.Detail == detail {
			count++
		}
	}
	return count
}

func assertToolCacheControlLoss(t *testing.T, report LossReport, line string) {
	t.Helper()
	if got := countCacheControlLoss(report, "tool_call"); got != 1 {
		t.Fatalf("%s 线必须恰好一条 tool_call 缓存损失，实得 %d 条：%+v", line, got, report.Entries)
	}
	if got := countCacheControlLoss(report, "tool_result"); got != 1 {
		t.Fatalf("%s 线必须恰好一条 tool_result 缓存损失，实得 %d 条：%+v", line, got, report.Entries)
	}
}

func TestAnthropicToolCacheControlDecode(t *testing.T) {
	decodeAnthropicToolCacheControl(t, toolCacheControlPayload)

	// 无 cache_control 时不得凭空造提示（解出即无损失可记）。
	decoded, _ := DecodeRequest(ProtocolAnthropicMessages, mustParsePayload(t, toolCacheControllessPayload), ConvertCtx{})
	for _, item := range decoded.Value.Items {
		if item.Kind == ItemToolResult && item.CacheHint != nil {
			t.Fatal("未给 cache_control 的 tool_result 不得带上 CacheHint")
		}
		for _, block := range item.Blocks {
			if block.Kind == BlockToolCall && block.CacheHint != nil {
				t.Fatal("未给 cache_control 的 tool_use 不得带上 CacheHint")
			}
		}
	}
}

// TestChatToolBlockCacheControlRecordedAsLoss 与 parity_test.go 的同名钉子（工具定义上的提示）
// 各测一侧：本用例的提示挂在消息内的 tool_use / tool_result 块上，故断言的是 tool_call /
// tool_result 两类明细。两条钉子内容不同，仅函数名在整合时撞车，故此处改名为块级。
func TestChatToolBlockCacheControlRecordedAsLoss(t *testing.T) {
	request := decodeAnthropicToolCacheControl(t, toolCacheControlPayload)
	encoded, ok := EncodeRequest(ProtocolOpenAIChat, request, ConvertCtx{
		ClientFormat: FormatClaude,
		TargetProto:  ProtocolOpenAIChat,
	})
	if !ok {
		t.Fatal("chat 线必须能编码")
	}
	assertToolCacheControlLoss(t, encoded.Loss, "chat")
	if body := encoded.Body.MarshalCompact(); strings.Contains(body, "cache_control") {
		t.Fatalf("chat 出站不得出现 cache_control（无承载位）：%s", body)
	}
}

func TestResponsesToolCacheControlRecordedAsLoss(t *testing.T) {
	request := decodeAnthropicToolCacheControl(t, toolCacheControlPayload)
	encoded, ok := EncodeRequest(ProtocolOpenAIResponses, request, ConvertCtx{
		ClientFormat: FormatClaude,
		TargetProto:  ProtocolOpenAIResponses,
	})
	if !ok {
		t.Fatal("responses 线必须能编码")
	}
	assertToolCacheControlLoss(t, encoded.Loss, "responses")
	if body := encoded.Body.MarshalCompact(); strings.Contains(body, "cache_control") {
		t.Fatalf("responses 出站不得出现 cache_control（无承载位）：%s", body)
	}
}

func TestToolCacheControlAbsentRecordsNoLoss(t *testing.T) {
	decoded, _ := DecodeRequest(ProtocolAnthropicMessages, mustParsePayload(t, toolCacheControllessPayload), ConvertCtx{})
	for _, protocol := range []WireProtocol{ProtocolOpenAIChat, ProtocolOpenAIResponses} {
		encoded, ok := EncodeRequest(protocol, decoded.Value, ConvertCtx{
			ClientFormat: FormatClaude,
			TargetProto:  protocol,
		})
		if !ok {
			t.Fatalf("%s 线必须能编码", protocol)
		}
		if got := countCacheControlLoss(encoded.Loss, "tool_call") + countCacheControlLoss(encoded.Loss, "tool_result"); got != 0 {
			t.Fatalf("%s 线在无 cache_control 时不得记工具缓存损失，实得 %d 条：%+v", protocol, got, encoded.Loss.Entries)
		}
	}
}
