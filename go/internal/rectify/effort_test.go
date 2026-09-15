package rectify

import "testing"

// 用例逐条对应 Node thinking-effort-conflict-rectifier.test.ts。
func TestDetectThinkingEffortConflict(t *testing.T) {
	matches := []string{
		"thinking options type cannot be disabled when reasoning_effort is set",
		`Provider deepseek returned 400: Provider returned 400: Bad Request | Upstream: {"error":{"message":"thinking options type cannot be disabled when reasoning_effort is set","type":"invalid_request_error"}}`,
		"Thinking options `type` cannot be disabled when `reasoning_effort` is set.",
		"thinking cannot be disabled when output_config.effort is set",
	}
	for _, message := range matches {
		if got := detectThinkingEffortConflict(message); got != "thinking_disabled_with_reasoning_effort" {
			t.Errorf("命中失败: %q → %q", message, got)
		}
	}

	misses := []string{
		"",
		"Invalid `signature` in `thinking` block",
		"thinking.enabled.budget_tokens: Input should be greater than or equal to 1024",
		"reasoning_effort must be one of low|medium",
		"invalid request: malformed",
	}
	for _, message := range misses {
		if got := detectThinkingEffortConflict(message); got != "" {
			t.Errorf("不应命中: %q → %q", message, got)
		}
	}
}

func TestRectifyThinkingEffortConflictRemovesOutputConfigWhenDisabled(t *testing.T) {
	body := mustParse(t, `{"model":"deepseek-v4-pro","thinking":{"type":"disabled"},"output_config":{"effort":"max"},"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)

	fields, applied := rectifyThinkingEffortConflict(body)

	if !applied {
		t.Fatal("应判定为已应用")
	}
	if !boolField(t, fields, "removedOutputConfigEffort") || boolField(t, fields, "removedReasoningEffort") {
		t.Errorf("剥离标记不对: %#v", fields)
	}
	if fields["thinkingType"] != "disabled" || fields["effort"] != "max" {
		t.Errorf("审计值不对: %#v", fields)
	}
	if got, want := compact(t, body), `{"model":"deepseek-v4-pro","thinking":{"type":"disabled"},"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`; got != want {
		t.Errorf("正文不对:\n got %s\nwant %s", got, want)
	}
}

func TestRectifyThinkingEffortConflictStripsOnlyEffortAndKeepsSiblings(t *testing.T) {
	body := mustParse(t, `{"thinking":{"type":"disabled"},"output_config":{"effort":"max","verbosity":"high","future_flag":true},"messages":[]}`)

	fields, applied := rectifyThinkingEffortConflict(body)

	if !applied || !boolField(t, fields, "removedOutputConfigEffort") || fields["effort"] != "max" {
		t.Errorf("判定或审计不对: applied=%v fields=%#v", applied, fields)
	}
	if got, want := compact(t, body), `{"thinking":{"type":"disabled"},"output_config":{"verbosity":"high","future_flag":true},"messages":[]}`; got != want {
		t.Errorf("兄弟键必须保留:\n got %s\nwant %s", got, want)
	}
}

func TestRectifyThinkingEffortConflictDropsOutputConfigWhenOnlyKey(t *testing.T) {
	body := mustParse(t, `{"thinking":{"type":"disabled"},"output_config":{"effort":"max"},"messages":[]}`)

	if _, applied := rectifyThinkingEffortConflict(body); !applied {
		t.Fatal("应判定为已应用")
	}
	if body.Has("output_config") {
		t.Errorf("effort 是唯一键时 output_config 必须整体删除: %s", compact(t, body))
	}
}

func TestRectifyThinkingEffortConflictRemovesTopLevelReasoningEffort(t *testing.T) {
	body := mustParse(t, `{"thinking":{"type":"disabled"},"reasoning_effort":"high","messages":[]}`)

	fields, applied := rectifyThinkingEffortConflict(body)

	if !applied || boolField(t, fields, "removedOutputConfigEffort") || !boolField(t, fields, "removedReasoningEffort") {
		t.Errorf("判定或审计不对: applied=%v fields=%#v", applied, fields)
	}
	if fields["effort"] != "high" {
		t.Errorf("effort 应取 reasoning_effort 的值: %#v", fields)
	}
	if got, want := compact(t, body), `{"thinking":{"type":"disabled"},"messages":[]}`; got != want {
		t.Errorf("正文不对:\n got %s\nwant %s", got, want)
	}
}

func TestRectifyThinkingEffortConflictTreatsMissingThinkingAsDisabled(t *testing.T) {
	body := mustParse(t, `{"output_config":{"effort":"medium"},"messages":[]}`)

	fields, applied := rectifyThinkingEffortConflict(body)

	if !applied || !boolField(t, fields, "removedOutputConfigEffort") {
		t.Errorf("判定不对: applied=%v fields=%#v", applied, fields)
	}
	if fields["thinkingType"] != nil {
		t.Errorf("thinking 缺失时审计应为 null: %#v", fields["thinkingType"])
	}
}

func TestRectifyThinkingEffortConflictKeepsEnabledAndAdaptiveThinking(t *testing.T) {
	enabled := mustParse(t, `{"thinking":{"type":"enabled","budget_tokens":2048},"output_config":{"effort":"max"}}`)
	if _, applied := rectifyThinkingEffortConflict(enabled); applied {
		t.Error("thinking=enabled 时不应整流")
	}
	if got, want := compact(t, enabled), `{"thinking":{"type":"enabled","budget_tokens":2048},"output_config":{"effort":"max"}}`; got != want {
		t.Errorf("正文被改动: %s", got)
	}

	adaptive := mustParse(t, `{"thinking":{"type":"adaptive"},"output_config":{"effort":"low"}}`)
	if _, applied := rectifyThinkingEffortConflict(adaptive); applied {
		t.Error("thinking=adaptive 时不应整流")
	}
	if got, want := compact(t, adaptive), `{"thinking":{"type":"adaptive"},"output_config":{"effort":"low"}}`; got != want {
		t.Errorf("正文被改动: %s", got)
	}
}

func TestRectifyThinkingEffortConflictNoOpWithoutEffortFields(t *testing.T) {
	body := mustParse(t, `{"thinking":{"type":"disabled"},"messages":[]}`)

	fields, applied := rectifyThinkingEffortConflict(body)

	if applied {
		t.Error("无 effort 字段时不应整流")
	}
	if boolField(t, fields, "removedOutputConfigEffort") || boolField(t, fields, "removedReasoningEffort") {
		t.Errorf("剥离标记应为假: %#v", fields)
	}
}

func TestRectifyThinkingEffortConflictKeepsEffortlessOutputConfig(t *testing.T) {
	body := mustParse(t, `{"thinking":{"type":"disabled"},"output_config":{"something_else":true},"reasoning_effort":"low"}`)

	fields, applied := rectifyThinkingEffortConflict(body)

	if !applied || boolField(t, fields, "removedOutputConfigEffort") || !boolField(t, fields, "removedReasoningEffort") {
		t.Errorf("判定不对: applied=%v fields=%#v", applied, fields)
	}
	if got, want := compact(t, body), `{"thinking":{"type":"disabled"},"output_config":{"something_else":true}}`; got != want {
		t.Errorf("无 effort 的 output_config 必须留原位:\n got %s\nwant %s", got, want)
	}
}
