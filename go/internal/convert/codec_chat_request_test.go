package convert

import "testing"

// 本文件钉住 chat **源**请求侧的三处字段搬运缺口：这些字段客户端声明了、网关此前静默不认，
// 报告里也查不到痕迹。

// encodeChatSource 走真实编解码链：chat 源解码 → 目标线编码，返回出站正文与合并后的损失。
func encodeChatSource(t *testing.T, body string, target WireProtocol) (*Value, []LossEntry) {
	t.Helper()
	ctx := ConvertCtx{ClientFormat: FormatOpenAI, TargetProto: target, Model: "m", ToWireToolName: NormalizeToolName}
	decoded, ok := DecodeRequest(ProtocolOpenAIChat, mustParsePayload(t, body), ctx)
	if !ok {
		t.Fatal("chat 线必须能解码")
	}
	encoded, ok := EncodeRequest(target, decoded.Value, ctx)
	if !ok {
		t.Fatalf("%s 线必须能编码", target)
	}
	entries := append(append([]LossEntry{}, decoded.Loss.Entries...), encoded.Loss.Entries...)
	return encoded.Body, entries
}

// TestChatDeveloperRoleKeptAsSystem：developer 与 system 同义（OpenAI 对 system 的新名），
// 此前落 default 分支被装箱成未知角色，跨到 anthropic 后系统提示整条丢失。
func TestChatDeveloperRoleKeptAsSystem(t *testing.T) {
	body, entries := encodeChatSource(t,
		`{"model":"gpt-4o","messages":[{"role":"developer","content":"be terse"},{"role":"user","content":"hi"}]}`,
		ProtocolAnthropicMessages)
	system, present := body.Get("system")
	if !present {
		t.Fatalf("developer 消息必须落到 anthropic 的 system，实际正文 %s（损失 %+v）",
			body.MarshalCompact(), entries)
	}
	if text, _ := system.String(); text != "be terse" {
		t.Fatalf("system 内容应为 %q，实际 %q", "be terse", text)
	}
	for _, entry := range entries {
		if entry.Detail == "messages[0].role" {
			t.Fatalf("developer 角色不该被当成未知角色装箱，实际 %+v", entries)
		}
	}
}

// TestChatReasoningMaxTokensBecomesBudget：chat 线的思考预算载体是 reasoning.max_tokens，
// 此前解码器只认等级、预算整个丢掉，跨到 anthropic 线上表现为思考强度消失。
func TestChatReasoningMaxTokensBecomesBudget(t *testing.T) {
	body, _ := encodeChatSource(t,
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"reasoning":{"max_tokens":2048}}`,
		ProtocolAnthropicMessages)
	thinking, present := body.Get("thinking")
	if !present {
		t.Fatalf("思考预算必须落到 thinking，实际正文 %s", body.MarshalCompact())
	}
	if budget, ok := numberField(thinking, "budget_tokens"); !ok || budget == nil || *budget != 2048 {
		t.Fatalf("budget_tokens 应为 2048，实际 %s", body.MarshalCompact())
	}
}

// TestChatReasoningEffortCarriedToResponses：嵌套 reasoning.effort 与顶层 reasoning_effort 同义，
// 必须进枢纽后原值写到只有等级载体的目标线。
func TestChatReasoningEffortCarriedToResponses(t *testing.T) {
	body, _ := encodeChatSource(t,
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"reasoning":{"effort":"high"}}`,
		ProtocolOpenAIResponses)
	reasoning, present := body.Get("reasoning")
	if !present {
		t.Fatalf("等级必须落到 responses 的 reasoning，实际正文 %s", body.MarshalCompact())
	}
	if effort, _ := stringField(reasoning, "effort"); effort != "high" {
		t.Fatalf("reasoning.effort 应为 high，实际 %s", body.MarshalCompact())
	}
}

// TestChatPromptCacheKeyReachesResponses：chat 源的缓存路由键必须进枢纽，才能在 responses 线写出。
func TestChatPromptCacheKeyReachesResponses(t *testing.T) {
	body, _ := encodeChatSource(t,
		`{"model":"gpt-4o","prompt_cache_key":"sess_1","messages":[{"role":"user","content":"hi"}]}`,
		ProtocolOpenAIResponses)
	if key, _ := stringField(body, "prompt_cache_key"); key != "sess_1" {
		t.Fatalf("chat 源的 prompt_cache_key 必须带到 responses 出站，实际 %q（正文 %s）",
			key, body.MarshalCompact())
	}
}
