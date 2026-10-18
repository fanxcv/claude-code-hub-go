package convert

import "testing"

// 本文件钉住 chat **源**请求侧五处字段搬运/可观测性缺口（审计 audit-chat #1/#2/#4/#5/#10）：
// 这些字段客户端声明了、网关此前静默不认，报告里也查不到痕迹。

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

// TestChatDeveloperRoleKeptAsSystem：developer 与 system 同义（relaykit 亦如此），不得整条丢弃。
func TestChatDeveloperRoleKeptAsSystem(t *testing.T) {
	body, entries := encodeChatSource(t,
		`{"model":"gpt-4o","messages":[{"role":"developer","content":"be terse"},{"role":"user","content":"hi"}]}`,
		ProtocolAnthropicMessages)
	system, present := body.Get("system")
	if !present {
		t.Fatalf("developer 消息必须落到 anthropic 的 system，实际正文 %s（损失 %+v）",
			body.MarshalCompact(), entries)
	}
	text, _ := system.String()
	if text != "be terse" {
		t.Fatalf("system 内容应为 %q，实际 %q", "be terse", text)
	}
	for _, entry := range entries {
		if entry.Detail == "messages[0].role" {
			t.Fatalf("developer 角色不该被当成未知角色装箱，实际 %+v", entries)
		}
	}
}

// TestChatForeignToolDeclarationsRecordedAsLoss：web_search_options 与 legacy functions/function_call
// 跨线无承载位，但此前连损失都不记（reportForeignPreservedTools 只看 tools 键）。
func TestChatForeignToolDeclarationsRecordedAsLoss(t *testing.T) {
	_, entries := encodeChatSource(t,
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],`+
			`"web_search_options":{"search_context_size":"high"},`+
			`"functions":[{"name":"f","parameters":{"type":"object"}}],`+
			`"function_call":"auto"}`,
		ProtocolAnthropicMessages)
	// web_search_options 与 responses 线的 web_search 工具同属一个能力，共用专用类别；
	// legacy functions/function_call 是 function 声明，归 catch-all 的工具子类。
	want := map[string]string{
		"web_search_options": LossWebSearchTool,
		"functions":          LossUnknownFieldTool,
		"function_call":      LossUnknownFieldTool,
	}
	for key, class := range want {
		found := false
		for _, entry := range entries {
			if entry.Capability == class && entry.Detail == key {
				found = true
			}
		}
		if !found {
			t.Fatalf("跨线丢弃 %s 必须留一条 %s 损失，实际 %+v", key, class, entries)
		}
	}
}

// TestChatReasoningMaxTokensBecomesBudget：chat 线的思考预算载体是 reasoning.max_tokens，
// anthropic 线以 thinking.budget_tokens 承载同一语义，必须原值搬运而不是忽略。
func TestChatReasoningMaxTokensBecomesBudget(t *testing.T) {
	body, _ := encodeChatSource(t,
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"reasoning":{"max_tokens":2048}}`,
		ProtocolAnthropicMessages)
	thinking, present := body.Get("thinking")
	if !present {
		t.Fatalf("思考预算必须落到 thinking，实际正文 %s", body.MarshalCompact())
	}
	budget, ok := numberField(thinking, "budget_tokens")
	if !ok || budget == nil || *budget != 2048 {
		t.Fatalf("budget_tokens 应为 2048，实际 %s", body.MarshalCompact())
	}
}

// TestChatReasoningDisabledAndExclude：enabled:false 与 anthropic 的 disabled 同口径（强度 none），
// 且 exclude:true 无承载位必须记损（false 是默认值，记它等于每请求一条噪声）。
func TestChatReasoningDisabledAndExclude(t *testing.T) {
	body, entries := encodeChatSource(t,
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],`+
			`"reasoning":{"enabled":false,"exclude":true}}`,
		ProtocolAnthropicMessages)
	if _, present := body.Get("thinking"); present {
		t.Fatalf("enabled:false 不得写出 thinking，实际 %s", body.MarshalCompact())
	}
	if outputConfig, present := body.Get("output_config"); present {
		if effort, ok := stringField(outputConfig, "effort"); ok && effort != "none" {
			t.Fatalf("enabled:false 的强度只能是不写或 none，实际 %q", effort)
		}
	}
	assertLossEntry(t, LossReport{Entries: entries}, LossUnknownFieldParam, string(LossDropped), "reasoning.exclude")

	// exclude:false 是默认值，不得记损（否则每个 Codex 请求都白挂一条）。
	_, quiet := encodeChatSource(t,
		`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"reasoning":{"enabled":false,"exclude":false}}`,
		ProtocolAnthropicMessages)
	for _, entry := range quiet {
		if entry.Detail == "reasoning.exclude" {
			t.Fatalf("exclude:false 不得记损，实际 %+v", quiet)
		}
	}
}
