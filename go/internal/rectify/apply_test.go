package rectify

import (
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/specialsettings"
)

// TestApplyRegistryOrderIsContract 钉住注册表顺序：effort 必须先于 signature。
//
// 若把 signature 提前，这类同时命中两者的文案会被 signature 的 invalid request 兜底吞掉，
// 改写的会是 messages 而不是 output_config。Node forwarder.ts:1378-1380 的注释即此约束。
func TestApplyRegistryOrderIsContract(t *testing.T) {
	body := mustParse(t, `{"thinking":{"type":"disabled"},"output_config":{"effort":"max"},"messages":[]}`)
	state := RetryState{}
	message := "invalid signature in thinking block: thinking options type cannot be disabled when reasoning_effort is set"

	result := Apply(body, KindAnthropic, NodeDefaults(), message, &state)

	if result.Type != specialsettings.TypeThinkingEffortConflictRectifier {
		t.Fatalf("命中顺序不对，命中的是 %q", result.Type)
	}
	if !result.Applied || !state.ThinkingEffortConflict || state.ThinkingSignature {
		t.Errorf("状态标记不对: result=%#v state=%#v", result, state)
	}
	if body.Has("output_config") {
		t.Errorf("应由 effort 整流器剥掉 output_config: %s", compact(t, body))
	}
}

func TestApplyDisabledSwitchMeansWholeChainMiss(t *testing.T) {
	body := mustParse(t, `{"thinking":{"type":"enabled","budget_tokens":100},"max_tokens":1000}`)
	state := RetryState{}
	// 只关掉 budget 一项，其余保持 Node 默认：命中后应整链未命中，而不是继续试别的整流器。
	switches := NodeDefaults()
	switches.ThinkingBudget = false

	result := Apply(body, KindAnthropic, switches, "thinking.enabled.budget_tokens: Input should be greater than or equal to 1024", &state)

	if result.Matched {
		t.Errorf("开关关闭时应视为未命中: %#v", result)
	}
	if got, want := compact(t, body), `{"thinking":{"type":"enabled","budget_tokens":100},"max_tokens":1000}`; got != want {
		t.Errorf("开关关闭时正文不得改动: %s", got)
	}
}

func TestApplyAlreadyRetriedIsNotApplied(t *testing.T) {
	body := mustParse(t, `{"thinking":{"type":"disabled"},"output_config":{"effort":"max"}}`)
	state := RetryState{ThinkingEffortConflict: true}

	result := Apply(body, KindAnthropic, NodeDefaults(), "thinking options type cannot be disabled when reasoning_effort is set", &state)

	if !result.Matched || result.Applied || result.Reason != ReasonAlreadyRetried {
		t.Fatalf("已重试过的语义不对: %#v", result)
	}
	if result.Type != specialsettings.TypeThinkingEffortConflictRectifier || result.Trigger != "thinking_disabled_with_reasoning_effort" {
		t.Errorf("类型或触发词不对: %#v", result)
	}
	if !body.Has("output_config") {
		t.Errorf("已重试过时不应再改正文: %s", compact(t, body))
	}
}

func TestApplyNotApplicable(t *testing.T) {
	// signature 触发词命中，但正文里没有 messages：整流器自判不适用。
	body := mustParse(t, `{"model":"claude-test"}`)
	state := RetryState{}

	result := Apply(body, KindAnthropic, NodeDefaults(), "messages.1.content.0: Invalid `signature` in `thinking` block", &state)

	if !result.Matched || result.Applied || result.Reason != ReasonNotApplicable {
		t.Fatalf("不适用的语义不对: %#v", result)
	}
	if result.Fields == nil {
		t.Error("不适用时也要带审计字段（Node 在判定 applied 之前就写条目）")
	}
	if state.ThinkingSignature {
		t.Error("不适用时不应置幂等标记")
	}
}

func TestApplyProviderKindGating(t *testing.T) {
	anthropicMessage := "thinking options type cannot be disabled when reasoning_effort is set"
	geminiMessage := `Invalid JSON payload received. Unknown name "id" at 'contents[1].parts[0].function_call': Cannot find field.`

	cases := []struct {
		name    string
		kind    Kind
		body    string
		message string
	}{
		{"gemini 供应商不跑 anthropic 整流器", KindGemini, `{"thinking":{"type":"disabled"},"output_config":{"effort":"max"}}`, anthropicMessage},
		{"anthropic 供应商不跑 gemini 整流器", KindAnthropic, `{"contents":[{"parts":[{"functionCall":{"name":"f","id":"c1"}}]}]}`, geminiMessage},
		{"其他供应商一律不整流", KindOther, `{"contents":[{"parts":[{"functionCall":{"name":"f","id":"c1"}}]}]}`, geminiMessage},
	}
	for _, testCase := range cases {
		body := mustParse(t, testCase.body)
		state := RetryState{}
		result := Apply(body, testCase.kind, NodeDefaults(), testCase.message, &state)
		if result.Matched {
			t.Errorf("%s：不应命中: %#v", testCase.name, result)
		}
		if got := compact(t, body); got != testCase.body {
			t.Errorf("%s：正文不得改动: %s", testCase.name, got)
		}
	}
}

func TestKindOfProviderType(t *testing.T) {
	cases := map[convert.ProviderType]Kind{
		convert.ProviderClaude:           KindAnthropic,
		convert.ProviderClaudeAuth:       KindAnthropic,
		convert.ProviderGemini:           KindGemini,
		convert.ProviderGeminiCLI:        KindGemini,
		convert.ProviderCodex:            KindOther,
		convert.ProviderOpenAICompatible: KindOther,
	}
	for providerType, want := range cases {
		if got := KindOfProviderType(providerType); got != want {
			t.Errorf("providerType %q → %v，期望 %v", providerType, got, want)
		}
	}
}
