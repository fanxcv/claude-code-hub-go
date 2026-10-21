package convert

import "testing"

// 三处此前静默丢弃的跨线字段：只补记损，不改承载行为（metadata 不为它新建枢纽字段）。
//
//  1. `metadata` / `metadata.user_id`：anthropic 的已知键表曾把它列为「已承载」而跳过
//     passthrough，而枢纽 Request 根本没有这个字段 → 跨线时消失且无痕迹；
//  2. `context_management`：不在 foreignDroppableFields 清单里 → 跨线时消失且无痕迹；
//  3. `prompt_cache_key` 反向（chat / anthropic 源 → responses 目标）：只有 responses 解码器
//     把它抬进枢纽，另两线跨线时它留在本线 passthrough 里，目标线读不到。

// conversionLosses 走真实编解码链，返回解码与编码的合并损失（与 forward 的 mergeLoss 同口径）。
func conversionLosses(t *testing.T, source, target WireProtocol, body string) []LossEntry {
	t.Helper()
	ctx := ConvertCtx{
		ClientFormat:   clientFormatOfProtocol(source),
		TargetProto:    target,
		Model:          "m",
		ToWireToolName: NormalizeToolName,
	}
	decoded, ok := DecodeRequest(source, mustParsePayload(t, body), ctx)
	if !ok {
		t.Fatalf("协议线 %s 无解码器", source)
	}
	encoded, ok := EncodeRequest(target, decoded.Value, ctx)
	if !ok {
		t.Fatalf("协议线 %s 无编码器", target)
	}
	return append(append([]LossEntry{}, decoded.Loss.Entries...), encoded.Loss.Entries...)
}

// countLoss 数「能力 + detail」都命中的条目。
func countLoss(entries []LossEntry, capability, detail string) int {
	count := 0
	for _, entry := range entries {
		if entry.Capability == capability && entry.Detail == detail {
			count++
		}
	}
	return count
}

func TestMetadataCrossWireRecordedAsIdentityLoss(t *testing.T) {
	// anthropic 源：knownKeys 曾把 metadata 列为已承载，于是它既不进 passthrough 也无消费者。
	for _, target := range []WireProtocol{ProtocolOpenAIChat, ProtocolOpenAIResponses} {
		entries := conversionLosses(t, ProtocolAnthropicMessages, target, `{
			"model":"claude","max_tokens":16,
			"messages":[{"role":"user","content":"hi"}],
			"metadata":{"user_id":"u1"}}`)
		if got := countLoss(entries, LossUnknownFieldIdentity, "metadata"); got != 1 {
			t.Fatalf("anthropic→%s：metadata 跨线须恰好记一条 identity 损失，实际 %d 条（%+v）", target, got, entries)
		}
	}
	// responses 源：同样按 identity 记。
	entries := conversionLosses(t, ProtocolOpenAIResponses, ProtocolOpenAIChat,
		`{"model":"gpt-5","input":"hi","metadata":{"user_id":"u1"}}`)
	if got := countLoss(entries, LossUnknownFieldIdentity, "metadata"); got != 1 {
		t.Fatalf("responses→chat：metadata 须恰好记一条 identity 损失，实际 %d 条（%+v）", got, entries)
	}
	// 同线直通不记：字段原样承载，没有丢。
	same := conversionLosses(t, ProtocolOpenAIResponses, ProtocolOpenAIResponses,
		`{"model":"gpt-5","input":"hi","metadata":{"user_id":"u1"}}`)
	if got := countLoss(same, LossUnknownFieldIdentity, "metadata"); got != 0 {
		t.Fatalf("同线直通不得记 metadata 损失，实际 %d 条（%+v）", got, same)
	}
	// 身份类损失是 info 档：它不改变上游看到的生成内容。
	if got := LossSeverityOf(LossUnknownFieldIdentity, LossDropped); got != SeverityInfo {
		t.Fatalf("身份类元数据应为 %s 档，实际 %s", SeverityInfo, got)
	}
}

func TestContextManagementCrossWireRecordedAsLoss(t *testing.T) {
	const body = `{"model":"gpt-5","input":"hi",` +
		`"context_management":{"edits":[{"type":"clear_thinking_20251015"}]}}`
	for _, target := range []WireProtocol{ProtocolOpenAIChat, ProtocolAnthropicMessages} {
		entries := conversionLosses(t, ProtocolOpenAIResponses, target, body)
		if got := countLoss(entries, LossUnknownFieldStructure, "context_management"); got != 1 {
			t.Fatalf("responses→%s：context_management 跨线须恰好记一条损失，实际 %d 条（%+v）", target, got, entries)
		}
	}
	// 空对象不算给出（与其它外线字段同口径）。
	bare := conversionLosses(t, ProtocolOpenAIResponses, ProtocolOpenAIChat,
		`{"model":"gpt-5","input":"hi","context_management":{}}`)
	if got := countLoss(bare, LossUnknownFieldStructure, "context_management"); got != 0 {
		t.Fatalf("空 context_management 不算给出，不得记损，实际 %d 条（%+v）", got, bare)
	}
}

func TestPromptCacheKeyFromNonResponsesSourceRecorded(t *testing.T) {
	// chat 源与 anthropic 源都没有把它抬进枢纽，跨到 responses 时目标线读不到它。
	for _, source := range []WireProtocol{ProtocolOpenAIChat, ProtocolAnthropicMessages} {
		body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"prompt_cache_key":"k1"}`
		if source == ProtocolAnthropicMessages {
			body = `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"prompt_cache_key":"k1"}`
		}
		entries := conversionLosses(t, source, ProtocolOpenAIResponses, body)
		if got := countLoss(entries, LossPromptCacheKey, "prompt_cache_key"); got != 1 {
			t.Fatalf("%s→responses：prompt_cache_key 须恰好记一条损失，实际 %d 条（%+v）", source, got, entries)
		}
	}
	// responses 源已进枢纽，chat 目标线原样回写：不得记损（否则每个 Codex 请求白多一条）。
	carried := conversionLosses(t, ProtocolOpenAIResponses, ProtocolOpenAIChat,
		`{"model":"gpt-5","input":"hi","prompt_cache_key":"k1"}`)
	if got := countLoss(carried, LossPromptCacheKey, "prompt_cache_key"); got != 0 {
		t.Fatalf("responses→chat 原样回写，不得记损，实际 %d 条（%+v）", got, carried)
	}
}
