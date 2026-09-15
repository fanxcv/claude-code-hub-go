package rectify

import "testing"

// 用例逐条对应 Node thinking-budget-rectifier.test.ts。
func TestDetectThinkingBudget(t *testing.T) {
	matches := []string{
		"thinking.enabled.budget_tokens: Input should be greater than or equal to 1024",
		`{"error":{"type":"invalid_request_error","message":"thinking.enabled.budget_tokens: Input should be greater than or equal to 1024"}}`,
		"THINKING.ENABLED.BUDGET_TOKENS: INPUT SHOULD BE GREATER THAN OR EQUAL TO 1024",
		"thinking budget_tokens must be >= 1024",
	}
	for _, message := range matches {
		if got := detectThinkingBudget(message); got != "budget_tokens_too_low" {
			t.Errorf("命中失败: %q → %q", message, got)
		}
	}

	misses := []string{
		"",
		"invalid_request_error: model not found",
		"max_tokens must be greater than 0",
		"invalid signature in thinking block",
		"assistant message must start with a thinking block",
		"thinking.enabled.budget_tokens: Input should be greater than 0",
		"budget_tokens: Input should be greater than or equal to 1024",
	}
	for _, message := range misses {
		if got := detectThinkingBudget(message); got != "" {
			t.Errorf("不应命中: %q → %q", message, got)
		}
	}
}

func TestRectifyThinkingBudgetSetsBudgetWhenMissing(t *testing.T) {
	body := mustParse(t, `{"max_tokens":50000}`)

	fields, applied := rectifyThinkingBudget(body)

	if !applied {
		t.Fatal("应判定为已应用")
	}
	before := fields["before"].(map[string]any)
	after := fields["after"].(map[string]any)
	if before["thinkingBudgetTokens"] != nil || after["thinkingBudgetTokens"] != int64(32000) {
		t.Errorf("before/after 不对: %#v / %#v", before, after)
	}
	if got, want := compact(t, body), `{"max_tokens":50000,"thinking":{"type":"enabled","budget_tokens":32000}}`; got != want {
		t.Errorf("正文不对:\n got %s\nwant %s", got, want)
	}
}

func TestRectifyThinkingBudgetRaisesLowBudgetAndType(t *testing.T) {
	body := mustParse(t, `{"max_tokens":50000,"thinking":{"type":"disabled","budget_tokens":500}}`)

	fields, applied := rectifyThinkingBudget(body)

	if !applied {
		t.Fatal("应判定为已应用")
	}
	before := fields["before"].(map[string]any)
	after := fields["after"].(map[string]any)
	if before["thinkingType"] != "disabled" || after["thinkingType"] != "enabled" {
		t.Errorf("thinking 类型迁移不对: %#v / %#v", before, after)
	}
	if before["thinkingBudgetTokens"] != int64(500) || after["thinkingBudgetTokens"] != int64(32000) {
		t.Errorf("预算迁移不对: %#v / %#v", before, after)
	}
	if got, want := compact(t, body), `{"max_tokens":50000,"thinking":{"type":"enabled","budget_tokens":32000}}`; got != want {
		t.Errorf("正文不对:\n got %s\nwant %s", got, want)
	}
}

func TestRectifyThinkingBudgetMaxTokensBoundaries(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantBody string
	}{
		{"缺失时抬到 64000", `{}`, `{"thinking":{"type":"enabled","budget_tokens":32000},"max_tokens":64000}`},
		{"低于 32001 抬到 64000", `{"max_tokens":1000}`, `{"max_tokens":64000,"thinking":{"type":"enabled","budget_tokens":32000}}`},
		{"等于 32001 不动", `{"max_tokens":32001}`, `{"max_tokens":32001,"thinking":{"type":"enabled","budget_tokens":32000}}`},
		{"等于 32000 抬到 64000", `{"max_tokens":32000}`, `{"max_tokens":64000,"thinking":{"type":"enabled","budget_tokens":32000}}`},
		{"已达标不动", `{"max_tokens":50000}`, `{"max_tokens":50000,"thinking":{"type":"enabled","budget_tokens":32000}}`},
	}
	for _, testCase := range cases {
		body := mustParse(t, testCase.body)
		if _, applied := rectifyThinkingBudget(body); !applied {
			t.Errorf("%s：应判定为已应用", testCase.name)
		}
		if got := compact(t, body); got != testCase.wantBody {
			t.Errorf("%s：正文不对\n got %s\nwant %s", testCase.name, got, testCase.wantBody)
		}
	}
}

func TestRectifyThinkingBudgetNoOpWhenAlreadyAtTarget(t *testing.T) {
	body := mustParse(t, `{"max_tokens":64000,"thinking":{"type":"enabled","budget_tokens":32000}}`)

	fields, applied := rectifyThinkingBudget(body)

	if applied {
		t.Error("值已达标时不应判定为已应用")
	}
	before := fields["before"].(map[string]any)
	after := fields["after"].(map[string]any)
	for _, key := range []string{"maxTokens", "thinkingType", "thinkingBudgetTokens"} {
		if before[key] != after[key] {
			t.Errorf("before/after 应相同: %s %#v vs %#v", key, before[key], after[key])
		}
	}
}

func TestRectifyThinkingBudgetReplacesNonObjectThinkingAndKeepsSiblings(t *testing.T) {
	replaced := mustParse(t, `{"max_tokens":50000,"thinking":"invalid"}`)
	if _, applied := rectifyThinkingBudget(replaced); !applied {
		t.Error("非对象 thinking 应被替换并判定为已应用")
	}
	if got, want := compact(t, replaced), `{"max_tokens":50000,"thinking":{"type":"enabled","budget_tokens":32000}}`; got != want {
		t.Errorf("正文不对:\n got %s\nwant %s", got, want)
	}

	siblings := mustParse(t, `{"max_tokens":50000,"thinking":{"type":"enabled","budget_tokens":500,"custom_field":"preserved"}}`)
	if _, applied := rectifyThinkingBudget(siblings); !applied {
		t.Error("应判定为已应用")
	}
	if got, want := compact(t, siblings), `{"max_tokens":50000,"thinking":{"type":"enabled","budget_tokens":32000,"custom_field":"preserved"}}`; got != want {
		t.Errorf("兄弟字段必须保留:\n got %s\nwant %s", got, want)
	}
}

func TestRectifyThinkingBudgetAdaptiveIsNotTouched(t *testing.T) {
	body := mustParse(t, `{"max_tokens":1000,"thinking":{"type":"adaptive"}}`)

	fields, applied := rectifyThinkingBudget(body)

	if applied {
		t.Error("adaptive 不应整流")
	}
	before := fields["before"].(map[string]any)
	after := fields["after"].(map[string]any)
	if before["thinkingType"] != "adaptive" || after["thinkingType"] != "adaptive" {
		t.Errorf("adaptive 快照不对: %#v / %#v", before, after)
	}
	if got, want := compact(t, body), `{"max_tokens":1000,"thinking":{"type":"adaptive"}}`; got != want {
		t.Errorf("正文被改动:\n got %s\nwant %s", got, want)
	}
}
