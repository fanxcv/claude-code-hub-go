package gate

import "testing"

func TestClassifyPerFamily(t *testing.T) {
	cases := []struct {
		name   string
		family Family
		event  string
		data   string
		want   Verdict
	}{
		{
			name:   "anthropic 文本增量",
			family: FamilyAnthropic,
			event:  "content_block_delta",
			data:   `{"type":"content_block_delta","delta":{"type":"text_delta","text":"hi"}}`,
			want:   VerdictContent,
		},
		{
			name:   "anthropic 空文本不算内容",
			family: FamilyAnthropic,
			event:  "content_block_delta",
			data:   `{"delta":{"text":""}}`,
			want:   VerdictNeutral,
		},
		{
			name:   "anthropic thinking 增量",
			family: FamilyAnthropic,
			event:  "content_block_delta",
			data:   `{"delta":{"thinking":"hmm"}}`,
			want:   VerdictContent,
		},
		{
			name:   "anthropic tool_use start 只有 id/name 不算内容",
			family: FamilyAnthropic,
			event:  "content_block_start",
			data:   `{"content_block":{"type":"tool_use","id":"t1","name":"f"}}`,
			want:   VerdictNeutral,
		},
		{
			name:   "anthropic redacted_thinking start 算内容",
			family: FamilyAnthropic,
			event:  "content_block_start",
			data:   `{"content_block":{"type":"redacted_thinking","data":"x"}}`,
			want:   VerdictContent,
		},
		{
			name:   "anthropic 终止事件",
			family: FamilyAnthropic,
			event:  "message_stop",
			data:   `{"type":"message_stop"}`,
			want:   VerdictTerminal,
		},
		{
			name:   "anthropic error 事件",
			family: FamilyAnthropic,
			event:  "error",
			data:   `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`,
			want:   VerdictError,
		},
		{
			name:   "anthropic 任意帧携带非空 error",
			family: FamilyAnthropic,
			event:  "message_delta",
			data:   `{"error":{"message":"x"}}`,
			want:   VerdictError,
		},
		{
			name:   "anприthropic 生命周期帧为中性",
			family: FamilyAnthropic,
			event:  "message_start",
			data:   `{"type":"message_start","message":{"id":"m1"}}`,
			want:   VerdictNeutral,
		},
		{
			name:   "chat 内容增量",
			family: FamilyOpenAIChat,
			event:  "",
			data:   `{"choices":[{"delta":{"content":"a"}}]}`,
			want:   VerdictContent,
		},
		{
			name:   "chat reasoning_content 别名算内容",
			family: FamilyOpenAIChat,
			event:  "",
			data:   `{"choices":[{"delta":{"reasoning_content":"r"}}]}`,
			want:   VerdictContent,
		},
		{
			name:   "chat 裸 reasoning 别名算内容",
			family: FamilyOpenAIChat,
			event:  "",
			data:   `{"choices":[{"delta":{"reasoning":"r"}}]}`,
			want:   VerdictContent,
		},
		{
			name:   "chat 工具参数增量算内容",
			family: FamilyOpenAIChat,
			event:  "",
			data:   `{"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"{}"}}]}}]}`,
			want:   VerdictContent,
		},
		{
			name:   "chat 空 delta 为中性",
			family: FamilyOpenAIChat,
			event:  "",
			data:   `{"choices":[{"delta":{}}]}`,
			want:   VerdictNeutral,
		},
		{
			name:   "chat DONE 哨兵",
			family: FamilyOpenAIChat,
			event:  "",
			data:   `[DONE]`,
			want:   VerdictTerminal,
		},
		{
			name:   "chat 流中 error",
			family: FamilyOpenAIChat,
			event:  "",
			data:   `{"error":{"message":"rate limited"}}`,
			want:   VerdictError,
		},
		{
			name:   "chat 空 choices 为中性",
			family: FamilyOpenAIChat,
			event:  "",
			data:   `{"choices":[]}`,
			want:   VerdictNeutral,
		},
		{
			name:   "非 JSON 载荷为 malformed",
			family: FamilyOpenAIChat,
			event:  "",
			data:   `not-json`,
			want:   VerdictMalformed,
		},
		{
			name:   "chat 下 DONE 之外的非 JSON 也无豁免",
			family: FamilyOpenAIChat,
			event:  "",
			data:   `[DONE]x`,
			want:   VerdictMalformed,
		},
		{
			name:   "JSON 标量载荷为 malformed",
			family: FamilyOpenAIChat,
			event:  "",
			data:   `"just-a-string"`,
			want:   VerdictMalformed,
		},
		{
			name:   "JSON null 载荷为 malformed",
			family: FamilyOpenAIChat,
			event:  "",
			data:   `null`,
			want:   VerdictMalformed,
		},
		{
			name:   "空 data 为中性",
			family: FamilyOpenAIChat,
			event:  "",
			data:   `   `,
			want:   VerdictNeutral,
		},
		{
			name:   "responses 文本增量",
			family: FamilyOpenAIResponses,
			event:  "response.output_text.delta",
			data:   `{"type":"response.output_text.delta","delta":"hi"}`,
			want:   VerdictContent,
		},
		{
			name:   "responses 请求回显帧为中性",
			family: FamilyOpenAIResponses,
			event:  "response.created",
			data:   `{"type":"response.created","response":{"id":"r1"}}`,
			want:   VerdictNeutral,
		},
		{
			name:   "responses 无 event 行时按 type 字段判别",
			family: FamilyOpenAIResponses,
			event:  "",
			data:   `{"type":"response.output_text.delta","delta":"z"}`,
			want:   VerdictContent,
		},
		{
			name:   "responses output_item.added 带 payload 算内容",
			family: FamilyOpenAIResponses,
			event:  "response.output_item.added",
			data:   `{"item":{"content":[{"text":"x"}]}}`,
			want:   VerdictContent,
		},
		{
			name:   "responses output_item.added 只有元数据为中性",
			family: FamilyOpenAIResponses,
			event:  "response.output_item.added",
			data:   `{"item":{"id":"i1","name":"f","status":"in_progress"}}`,
			want:   VerdictNeutral,
		},
		{
			name:   "responses response.failed 为 error",
			family: FamilyOpenAIResponses,
			event:  "response.failed",
			data:   `{"type":"response.failed","response":{"status":"failed"}}`,
			want:   VerdictError,
		},
		{
			name:   "responses 子工具失败为中性",
			family: FamilyOpenAIResponses,
			event:  "response.mcp_call.failed",
			data:   `{"type":"response.mcp_call.failed"}`,
			want:   VerdictNeutral,
		},
		{
			name:   "responses response.completed 为终态",
			family: FamilyOpenAIResponses,
			event:  "response.completed",
			data:   `{"type":"response.completed","response":{"status":"completed"}}`,
			want:   VerdictTerminal,
		},
		{
			name:   "responses compaction opaque state 算内容",
			family: FamilyOpenAIResponses,
			event:  "response.output_item.done",
			data:   `{"type":"response.output_item.done","item":{"type":"compaction","encrypted_content":"blob"}}`,
			want:   VerdictContent,
		},
		{
			name:   "responses completed 内嵌 compaction 算内容",
			family: FamilyOpenAIResponses,
			event:  "response.completed",
			data:   `{"type":"response.completed","response":{"status":"completed","output":[{"type":"compaction","encrypted_content":"blob"}]}}`,
			want:   VerdictContent,
		},
		{
			name:   "responses compaction 缺 opaque state 不算内容",
			family: FamilyOpenAIResponses,
			event:  "response.output_item.done",
			data:   `{"type":"response.output_item.done","item":{"type":"compaction"}}`,
			want:   VerdictNeutral,
		},
		{
			name:   "gemini 文本内容",
			family: FamilyGemini,
			event:  "",
			data:   `{"candidates":[{"content":{"parts":[{"text":"hi"}]}}]}`,
			want:   VerdictContent,
		},
		{
			name:   "gemini 空 parts 元数据为中性",
			family: FamilyGemini,
			event:  "",
			data:   `{"candidates":[{"content":{"parts":[{"text":""}]}}]}`,
			want:   VerdictNeutral,
		},
		{
			name:   "gemini STOP 为终态",
			family: FamilyGemini,
			event:  "",
			data:   `{"candidates":[{"finishReason":"STOP"}]}`,
			want:   VerdictTerminal,
		},
		{
			name:   "gemini SAFETY 为 error",
			family: FamilyGemini,
			event:  "",
			data:   `{"candidates":[{"finishReason":"SAFETY"}]}`,
			want:   VerdictError,
		},
		{
			name:   "gemini promptFeedback 拦截为 error",
			family: FamilyGemini,
			event:  "",
			data:   `{"promptFeedback":{"blockReason":"SAFETY"}}`,
			want:   VerdictError,
		},
		{
			name:   "gemini envelope 解包后按内层分类",
			family: FamilyGemini,
			event:  "",
			data:   `{"response":{"candidates":[{"content":{"parts":[{"text":"x"}]}}]}}`,
			want:   VerdictContent,
		},
		{
			name:   "gemini envelope 内层错误",
			family: FamilyGemini,
			event:  "",
			data:   `{"response":{"error":{"code":429,"message":"quota"}}}`,
			want:   VerdictError,
		},
		{
			name:   "未知家族一律中性",
			family: Family("unknown"),
			event:  "response.output_text.delta",
			data:   `{"delta":"hi"}`,
			want:   VerdictNeutral,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := Classify(testCase.family, testCase.event, testCase.data)
			if got != testCase.want {
				t.Fatalf("Classify(%s, %q, %s) = %s，期望 %s",
					testCase.family, testCase.event, testCase.data, got, testCase.want)
			}
		})
	}
}

func TestClassifyStructuredMatchesText(t *testing.T) {
	raw := `{"type":"response.output_text.delta","delta":"hi"}`
	fromText := Classify(FamilyOpenAIResponses, "response.output_text.delta", raw)
	if fromText != VerdictContent {
		t.Fatalf("期望 content，得到 %s", fromText)
	}
	if got := Classify(Family("unknown"), "", raw); got != VerdictNeutral {
		t.Fatalf("未知家族应中性，得到 %s", got)
	}
}

func TestMapProviderTypeToFamily(t *testing.T) {
	cases := []struct {
		providerType string
		want         Family
		wantOK       bool
	}{
		{"claude", FamilyAnthropic, true},
		{"claude-auth", FamilyAnthropic, true},
		{"codex", FamilyOpenAIResponses, true},
		{"openai-compatible", FamilyOpenAIChat, true},
		{"gemini", FamilyGemini, true},
		{"gemini-cli", FamilyGemini, true},
		{"", "", false},
		{"vertex", "", false},
	}
	for _, testCase := range cases {
		got, ok := MapProviderTypeToFamily(testCase.providerType)
		if ok != testCase.wantOK || got != testCase.want {
			t.Fatalf("MapProviderTypeToFamily(%q) = (%q, %v)，期望 (%q, %v)",
				testCase.providerType, got, ok, testCase.want, testCase.wantOK)
		}
	}
}

func TestIsRequestEchoFrame(t *testing.T) {
	cases := []struct {
		name   string
		family Family
		event  string
		data   string
		want   bool
	}{
		{"responses created", FamilyOpenAIResponses, "response.created", "", true},
		{"responses in_progress 带空格", FamilyOpenAIResponses, " response.in_progress ", "", true},
		{"responses queued", FamilyOpenAIResponses, "response.queued", "", true},
		{"responses 无 event 行按 data 头部嗅探", FamilyOpenAIResponses, "", `{"type":"response.in_progress","response":{}}`, true},
		{"responses 内容帧不是回显", FamilyOpenAIResponses, "response.output_text.delta", "", false},
		{"responses 嗅探不到时不豁免", FamilyOpenAIResponses, "", `{"delta":"hi"}`, false},
		{"其他家族不回显", FamilyAnthropic, "response.created", "", false},
		{"其他家族无嗅探", FamilyOpenAIChat, "", `{"type":"response.created"}`, false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := IsRequestEchoFrame(testCase.family, testCase.event, testCase.data); got != testCase.want {
				t.Fatalf("IsRequestEchoFrame = %v，期望 %v", got, testCase.want)
			}
		})
	}
}

func TestResponsesCompletionHelpers(t *testing.T) {
	clean := `{"type":"response.completed","response":{"status":"completed","output":[]}}`
	incomplete := `{"type":"response.incomplete","response":{"status":"incomplete"}}`

	if !IsCleanResponsesCompletion("response.completed", clean) {
		t.Fatal("干净完成应判定为 true")
	}
	if !IsCleanResponsesCompletion("", clean) {
		t.Fatal("无 event 行时应按 type 字段判定")
	}
	if IsCleanResponsesCompletion("response.output_text.done", clean) {
		t.Fatal("非完成事件不得判定为干净完成")
	}
	if IsCleanResponsesCompletion("response.completed", `{"type":"response.completed","response":{"status":"failed"}}`) {
		t.Fatal("failed 不是干净完成")
	}
	if IsCleanResponsesCompletion("response.completed", `{"type":"response.completed","response":{"status":"completed","error":{"message":"x"}}}`) {
		t.Fatal("带非空 error 不是干净完成")
	}
	if IsCleanResponsesCompletion("response.completed", `not-json`) {
		t.Fatal("非 JSON 不得判定为干净完成")
	}
	if IsCleanResponsesCompletion("response.completed", `[]`) {
		t.Fatal("数组载荷不得判定为干净完成")
	}

	if !IsResponsesIncompleteCompletion("response.incomplete", incomplete) {
		t.Fatal("incomplete 终态应判定为 true")
	}
	if IsResponsesIncompleteCompletion("response.completed", incomplete) {
		t.Fatal("事件不匹配时不得判定")
	}
	if IsResponsesIncompleteCompletion("response.incomplete", `{"type":"response.incomplete","response":{"status":"completed"}}`) {
		t.Fatal("状态不匹配时不得判定")
	}
	if IsResponsesIncompleteCompletion("response.incomplete", `not-json`) {
		t.Fatal("非 JSON 不得判定")
	}
}

func TestClassifyTerminalKind(t *testing.T) {
	cases := []struct {
		name   string
		family Family
		event  string
		parsed any
		want   TerminalKind
	}{
		{"responses completed", FamilyOpenAIResponses, "response.completed", map[string]any{"type": "response.completed"}, TerminalComplete},
		{"responses done", FamilyOpenAIResponses, "response.done", nil, TerminalComplete},
		{"responses incomplete", FamilyOpenAIResponses, "response.incomplete", map[string]any{"type": "response.incomplete"}, TerminalIncomplete},
		{"responses 无 event 行取 type", FamilyOpenAIResponses, "", map[string]any{"type": "response.incomplete"}, TerminalIncomplete},
		{"responses 内容帧无终态", FamilyOpenAIResponses, "response.output_text.delta", nil, TerminalNone},
		{"anthropic 无结构化终态信息时为完成", FamilyAnthropic, "message_stop", nil, TerminalComplete},
		{"anthropic 终态事件", FamilyAnthropic, "message_stop", map[string]any{"type": "message_stop"}, TerminalComplete},
		// anthropic/gemini 没有 incomplete 概念：本函数只在帧已被判为终态时被调用，
		// 非 responses 家族无结构化终态信号即视为完成（与 TS 一致）。
		{"anthropic 非终态信号也归为完成", FamilyAnthropic, "message_delta", map[string]any{"type": "message_delta"}, TerminalComplete},
		{"gemini 结构化终态", FamilyGemini, "", map[string]any{"candidates": []any{map[string]any{"finishReason": "STOP"}}}, TerminalComplete},
		{"gemini 无结构化终态信号也归为完成", FamilyGemini, "", map[string]any{"candidates": []any{map[string]any{"content": map[string]any{}}}}, TerminalComplete},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := ClassifyTerminalKind(testCase.family, testCase.event, testCase.parsed); got != testCase.want {
				t.Fatalf("ClassifyTerminalKind = %q，期望 %q", got, testCase.want)
			}
		})
	}
}
