package rectify

import "testing"

// 用例逐条对应 Node thinking-signature-rectifier.test.ts。
func TestDetectThinkingSignature(t *testing.T) {
	cases := []struct {
		message string
		want    string
	}{
		{"messages.1.content.0: Invalid `signature` in `thinking` block", "invalid_signature_in_thinking_block"},
		{"Messages.1.Content.0: invalid signature in thinking block", "invalid_signature_in_thinking_block"},
		{"messages.69.content.0.type: Expected `thinking` or `redacted_thinking`, but found `tool_use`. When `thinking` is enabled, a final `assistant` message must start with a thinking block (preceeding the lastmost set of `tool_use` and `tool_result` blocks). To avoid this requirement, disable `thinking`.", "assistant_message_must_start_with_thinking"},
		{"非法请求", "invalid_request"},
		{"illegal request format", "invalid_request"},
		{"invalid request: malformed JSON", "invalid_request"},
		{"***.***.***.***.***.signature: Field required", "invalid_signature_in_thinking_block"},
		{"content[0].signature: Field required", "invalid_signature_in_thinking_block"},
		{"messages.1.content.0.signature: Field required", "invalid_signature_in_thinking_block"},
		{"Signature: field required", "invalid_signature_in_thinking_block"},
		{"content.1.tool_use.signature: Extra inputs are not permitted", "invalid_signature_in_thinking_block"},
		{`{"error":{"type":"<nil>","message":"***.***content.1.tool_use.signature: Extra inputs are not permitted (request id: 20260122042750493345237oUastQMk)"}}`, "invalid_signature_in_thinking_block"},
		{"Signature: EXTRA INPUTS ARE NOT PERMITTED", "invalid_signature_in_thinking_block"},
		{"`thinking` or `redacted_thinking` blocks in the latest assistant message cannot be modified. These blocks must remain as they were in the original response.", "invalid_signature_in_thinking_block"},
		{`{"error":{"type":"<nil>","message":"***.***.content.53: ` + "`thinking`" + ` or ` + "`redacted_thinking`" + ` blocks in the latest assistant message cannot be modified. (request id: 20260203160354671091064E4xAq0t0)"},"type":"error"}`, "invalid_signature_in_thinking_block"},
		{"", ""},
		{"Request timeout", ""},
	}
	for _, testCase := range cases {
		if got := detectThinkingSignature(testCase.message); got != testCase.want {
			t.Errorf("检测结果不符: %q → %q（期望 %q）", testCase.message, got, testCase.want)
		}
	}
}

func TestRectifyThinkingSignatureRemovesBlocksAndStraySignatures(t *testing.T) {
	body := mustParse(t, `{"model":"claude-test","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"sig_thinking"},{"type":"text","text":"hello","signature":"sig_text_should_remove"},{"type":"tool_use","id":"toolu_1","name":"WebSearch","input":{"query":"q"},"signature":"sig_tool_should_remove"},{"type":"redacted_thinking","data":"r","signature":"sig_redacted"}]},{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	fields, applied := rectifyThinkingSignature(body)

	if !applied {
		t.Fatal("应判定为已应用")
	}
	if intField(t, fields, "removedThinkingBlocks") != 1 ||
		intField(t, fields, "removedRedactedThinkingBlocks") != 1 ||
		intField(t, fields, "removedSignatureFields") != 2 {
		t.Errorf("计数不对: %#v", fields)
	}
	want := `{"model":"claude-test","messages":[{"role":"assistant","content":[{"type":"text","text":"hello"},{"type":"tool_use","id":"toolu_1","name":"WebSearch","input":{"query":"q"}}]},{"role":"user","content":[{"type":"text","text":"hi"}]}]}`
	if got := compact(t, body); got != want {
		t.Errorf("正文不对:\n got %s\nwant %s", got, want)
	}
}

func TestRectifyThinkingSignatureWithoutMessagesIsNoOp(t *testing.T) {
	body := mustParse(t, `{"model":"claude-test"}`)

	fields, applied := rectifyThinkingSignature(body)

	if applied {
		t.Error("无 messages 时不应整流")
	}
	if intField(t, fields, "removedThinkingBlocks") != 0 ||
		intField(t, fields, "removedRedactedThinkingBlocks") != 0 ||
		intField(t, fields, "removedSignatureFields") != 0 {
		t.Errorf("计数应为零: %#v", fields)
	}
}

func TestRectifyThinkingSignatureDeletesThinkingWhenPrefixMissing(t *testing.T) {
	body := mustParse(t, `{"model":"claude-test","thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"WebSearch","input":{"query":"q"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}]}`)

	_, applied := rectifyThinkingSignature(body)

	if !applied {
		t.Fatal("应判定为已应用")
	}
	if body.Has("thinking") {
		t.Errorf("顶层 thinking 必须删除: %s", compact(t, body))
	}
}

func TestRectifyThinkingSignatureMissingSignatureWithToolUse(t *testing.T) {
	// 从非 Anthropic 渠道切到 Anthropic：thinking 块缺 signature，且最后一条 assistant 以 tool_use 开头。
	body := mustParse(t, `{"model":"claude-3-5-sonnet-20241022","thinking":{"type":"enabled","budget_tokens":10000},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"Let me analyze this..."},{"type":"tool_use","id":"toolu_01","name":"WebSearch","input":{"query":"test"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":"search result"}]}]}`)

	fields, applied := rectifyThinkingSignature(body)

	if !applied || intField(t, fields, "removedThinkingBlocks") != 1 {
		t.Errorf("判定或计数不对: applied=%v fields=%#v", applied, fields)
	}
	want := `{"model":"claude-3-5-sonnet-20241022","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_01","name":"WebSearch","input":{"query":"test"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":"search result"}]}]}`
	if got := compact(t, body); got != want {
		t.Errorf("正文不对:\n got %s\nwant %s", got, want)
	}
}

func TestRectifyThinkingSignatureKeepsThinkingWhenNoToolUse(t *testing.T) {
	// 反例：末条 assistant 内容不含 tool_use 时，顶层 thinking 保留（Node 的兜底以 tool_use 为条件）。
	body := mustParse(t, `{"thinking":{"type":"enabled"},"messages":[{"role":"assistant","content":[{"type":"text","text":"hi"}]}]}`)

	if _, applied := rectifyThinkingSignature(body); applied {
		t.Error("没有可整流内容时不应判定为已应用")
	}
	if !body.Has("thinking") {
		t.Errorf("不含 tool_use 时不应删顶层 thinking: %s", compact(t, body))
	}
}

func TestRectifyThinkingSignatureDeletesThinkingAfterBlocksRemoved(t *testing.T) {
	// 顺序契约：thinking 块先被移走，兜底再看到「末条 assistant 首块是 tool_use」——与 Node 同序，
	// 故此处顶层 thinking 会被删（Node 自己的用例也这样断言）。
	body := mustParse(t, `{"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"s"},{"type":"tool_use","id":"toolu_1","name":"X","input":{}}]}]}`)

	if _, applied := rectifyThinkingSignature(body); !applied {
		t.Error("移除 thinking 块即应判定为已应用")
	}
	if body.Has("thinking") {
		t.Errorf("末条 assistant 首块变成 tool_use 后应删顶层 thinking: %s", compact(t, body))
	}
}
