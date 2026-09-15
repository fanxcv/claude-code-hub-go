package forward

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// 本文件是 Node 三份 provider-overrides 单测的逐条搬运（同一批用例、同一批期望值）。
//
// 真源：
//   - tests/unit/proxy/anthropic-provider-overrides.test.ts（1154 行）
//   - tests/unit/proxy/codex-provider-overrides.test.ts（907 行）
//   - tests/unit/lib/gemini/provider-overrides.test.ts（361 行）
//
// 搬运口径：Node 的 `expect(output.max_tokens).toBe(32000)` 一类断言，在 Go 侧一律改成
// 「解析出站正文后按字段路径断言」——判据相同，但顺便钉住**序列化结果**（正文是发上游的字节，
// 只断言内存里的值证明不了真发出去的形态）。

// applyOverridesAt 走一次覆写并把出站正文解析成便于断言的值树。
//
// clientPath 是客户端请求路径——它与正文一样是**请求级**输入，不是供应商属性，故由调用方显式
// 给出（生产上由 dataplane 从 pctx 取）。
func applyOverridesAt(
	t *testing.T,
	provider Provider,
	protocol convert.WireProtocol,
	clientPath string,
	body string,
) (*convert.Value, *ProviderOverrideApplier) {
	t.Helper()
	applier := NewProviderOverrideApplier(clientPath)
	out, err := applier.Apply(provider, protocol, []byte(body))
	if err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	tree, err := convert.ParseJSON(out)
	if err != nil {
		t.Fatalf("出站正文不是合法 JSON: %v\n%s", err, out)
	}
	return tree, applier
}

func applyOverrides(t *testing.T, provider Provider, protocol convert.WireProtocol, body string) (*convert.Value, *ProviderOverrideApplier) {
	t.Helper()
	return applyOverridesAt(t, provider, protocol, "", body)
}

// assertBodyUnchangedAt 断言覆写**未**改动正文（Node 的 `expect(output).toBe(request)`）。
func assertBodyUnchangedAt(
	t *testing.T,
	provider Provider,
	protocol convert.WireProtocol,
	clientPath string,
	body string,
) {
	t.Helper()
	applier := NewProviderOverrideApplier(clientPath)
	out, err := applier.Apply(provider, protocol, []byte(body))
	if err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	if string(out) != body {
		t.Fatalf("无命中时正文必须逐字节原样（前缀缓存依赖字节稳定）\n得到: %s\n期望: %s", out, body)
	}
	if len(applier.OverrideAuditEntries()) != 0 {
		t.Fatalf("无命中时不应产生审计条目: %#v", applier.OverrideAuditEntries())
	}
}

// assertBodyUnchangedAudited 断言正文未改动，但**允许**有审计条目。
//
// 为什么需要它：Node 的语义是「配了偏好就写条目」（`hit` 取「有没有配」，`changed` 取「有没有真改」）。
// 例如 codex 图像工具「已存在即不重复注入」——正文一字未动，条目照样写（hit=true, changed=false）。
// 把它与「未配置」的用例混在一个助手，就会把正确行为判成失败。
func assertBodyUnchangedAudited(
	t *testing.T,
	provider Provider,
	protocol convert.WireProtocol,
	clientPath string,
	body string,
) {
	t.Helper()
	applier := NewProviderOverrideApplier(clientPath)
	out, err := applier.Apply(provider, protocol, []byte(body))
	if err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	if string(out) != body {
		t.Fatalf("正文应逐字节原样\n得到: %s\n期望: %s", out, body)
	}
	if len(applier.OverrideAuditEntries()) == 0 {
		t.Fatal("配了偏好但正文未变时，Node 仍会写审计条目（hit=true, changed=false）")
	}
}

func assertBodyUnchanged(t *testing.T, provider Provider, protocol convert.WireProtocol, body string) {
	t.Helper()
	assertBodyUnchangedAt(t, provider, protocol, "", body)
}

func fieldJSON(t *testing.T, tree *convert.Value, key string) string {
	t.Helper()
	value, ok := tree.Get(key)
	if !ok {
		return "<缺失>"
	}
	return value.MarshalCompact()
}

// arrayItemJSON 取数组字段的第 index 个元素的紧凑序列化（Get 只认对象成员，数组要按序取）。
func arrayItemJSON(t *testing.T, tree *convert.Value, key string, index int) string {
	t.Helper()
	array, ok := tree.Get(key)
	if !ok || array == nil || !array.IsArray() {
		return "<缺失>"
	}
	items := array.Items()
	if index >= len(items) {
		return "<缺失>"
	}
	return items[index].MarshalCompact()
}

func nestedJSON(t *testing.T, tree *convert.Value, outer, inner string) string {
	t.Helper()
	object, ok := tree.Get(outer)
	if !ok || object == nil {
		return "<缺失>"
	}
	value, ok := object.Get(inner)
	if !ok {
		return "<缺失>"
	}
	return value.MarshalCompact()
}

func auditEntryOf(t *testing.T, applier *ProviderOverrideApplier) map[string]any {
	t.Helper()
	entries := applier.OverrideAuditEntries()
	if len(entries) != 1 {
		t.Fatalf("审计条目数 = %d，期望 1: %#v", len(entries), entries)
	}
	return entries[0]
}

func changeOf(t *testing.T, entry map[string]any, path string) map[string]any {
	t.Helper()
	changes, ok := entry["changes"].([]any)
	if !ok {
		t.Fatalf("审计缺 changes: %#v", entry)
	}
	for _, raw := range changes {
		change, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if change["path"] == path {
			return change
		}
	}
	t.Fatalf("审计里没有 %s 这一条: %#v", path, changes)
	return nil
}

// ---------------------------------------------------------------------------
// anthropic（anthropic-provider-overrides.test.ts）
// ---------------------------------------------------------------------------

func claudeOverrideProvider() Provider {
	return Provider{ID: 11, Name: "供应商甲", Type: convert.ProviderClaude}
}

func TestAnthropicOverridesNonClaudeProvidersUntouched(t *testing.T) {
	cases := map[string]convert.ProviderType{
		"codex":             convert.ProviderCodex,
		"gemini":            convert.ProviderGemini,
		"openai-compatible": convert.ProviderOpenAICompatible,
	}
	for name, providerType := range cases {
		t.Run(name, func(t *testing.T) {
			provider := Provider{
				Type:                              providerType,
				AnthropicMaxTokensPreference:      "32000",
				AnthropicThinkingBudgetPreference: "10240",
			}
			assertBodyUnchanged(t, provider, convert.ProtocolAnthropicMessages,
				`{"model":"claude-3-opus-20240229","messages":[],"max_tokens":8000}`)
		})
	}
}

func TestAnthropicMaxTokensOverride(t *testing.T) {
	t.Run("claude 生效", func(t *testing.T) {
		provider := claudeOverrideProvider()
		provider.AnthropicMaxTokensPreference = "32000"
		tree, _ := applyOverrides(t, provider, convert.ProtocolAnthropicMessages,
			`{"model":"claude-3-opus-20240229","messages":[],"max_tokens":8000}`)
		if got := fieldJSON(t, tree, "max_tokens"); got != "32000" {
			t.Fatalf("max_tokens = %s，期望 32000", got)
		}
	})

	t.Run("claude-auth 生效", func(t *testing.T) {
		provider := claudeOverrideProvider()
		provider.Type = convert.ProviderClaudeAuth
		provider.AnthropicMaxTokensPreference = "16000"
		tree, _ := applyOverrides(t, provider, convert.ProtocolAnthropicMessages,
			`{"model":"claude-3-sonnet-20240229","messages":[],"max_tokens":4000}`)
		if got := fieldJSON(t, tree, "max_tokens"); got != "16000" {
			t.Fatalf("max_tokens = %s，期望 16000", got)
		}
	})

	t.Run("inherit 与空值不动", func(t *testing.T) {
		for _, value := range []string{"inherit", ""} {
			provider := claudeOverrideProvider()
			provider.AnthropicMaxTokensPreference = value
			assertBodyUnchanged(t, provider, convert.ProtocolAnthropicMessages,
				`{"model":"claude-3-opus-20240229","messages":[],"max_tokens":8000}`)
		}
	})

	t.Run("非法数字串不动", func(t *testing.T) {
		provider := claudeOverrideProvider()
		provider.AnthropicMaxTokensPreference = "invalid"
		assertBodyUnchanged(t, provider, convert.ProtocolAnthropicMessages,
			`{"model":"claude-3-opus-20240229","messages":[],"max_tokens":8000}`)
	})

	t.Run("无 max_tokens 时新增该字段", func(t *testing.T) {
		provider := claudeOverrideProvider()
		provider.AnthropicMaxTokensPreference = "32000"
		tree, _ := applyOverrides(t, provider, convert.ProtocolAnthropicMessages,
			`{"model":"claude-3-opus-20240229","messages":[]}`)
		if got := fieldJSON(t, tree, "max_tokens"); got != "32000" {
			t.Fatalf("max_tokens = %s，期望 32000", got)
		}
	})
}

func TestAnthropicThinkingBudgetOverride(t *testing.T) {
	t.Run("写入 type 与 budget_tokens", func(t *testing.T) {
		provider := claudeOverrideProvider()
		provider.AnthropicThinkingBudgetPreference = "10240"
		tree, _ := applyOverrides(t, provider, convert.ProtocolAnthropicMessages,
			`{"model":"claude-3-opus-20240229","messages":[],"max_tokens":32000}`)
		if got := fieldJSON(t, tree, "thinking"); got != `{"type":"enabled","budget_tokens":10240}` {
			t.Fatalf("thinking = %s", got)
		}
	})

	t.Run("保留既有 thinking 的其他字段", func(t *testing.T) {
		provider := claudeOverrideProvider()
		provider.AnthropicThinkingBudgetPreference = "8000"
		tree, _ := applyOverrides(t, provider, convert.ProtocolAnthropicMessages,
			`{"model":"claude-3-opus-20240229","messages":[],"max_tokens":32000,`+
				`"thinking":{"type":"disabled","budget_tokens":2000,"custom_field":"preserve_me"}}`)
		if got := fieldJSON(t, tree, "thinking"); got !=
			`{"type":"enabled","budget_tokens":8000,"custom_field":"preserve_me"}` {
			t.Fatalf("thinking = %s", got)
		}
	})

	t.Run("thinking 不是对象时整个替换", func(t *testing.T) {
		provider := claudeOverrideProvider()
		provider.AnthropicThinkingBudgetPreference = "6000"
		tree, _ := applyOverrides(t, provider, convert.ProtocolAnthropicMessages,
			`{"model":"claude-3-opus-20240229","messages":[],"max_tokens":32000,"thinking":"invalid_string_value"}`)
		if got := fieldJSON(t, tree, "thinking"); got != `{"type":"enabled","budget_tokens":6000}` {
			t.Fatalf("thinking = %s", got)
		}
	})
}

func TestAnthropicClamping(t *testing.T) {
	cases := []struct {
		name          string
		maxTokens     string
		budget        string
		body          string
		wantMaxTokens string
		wantThinking  string
	}{
		{
			name: "与覆写后的 max_tokens 相撞则夹到 max-1", maxTokens: "10000", budget: "15000",
			body:          `{"model":"claude-3-opus-20240229","messages":[]}`,
			wantMaxTokens: "10000", wantThinking: `{"type":"enabled","budget_tokens":9999}`,
		},
		{
			name: "与请求自带的 max_tokens 相撞则夹到 max-1", budget: "20000",
			body:         `{"model":"claude-3-opus-20240229","messages":[],"max_tokens":16000}`,
			wantThinking: `{"type":"enabled","budget_tokens":15999}`,
		},
		{
			name: "恰好相等也夹", maxTokens: "8000", budget: "8000",
			body:          `{"model":"claude-3-opus-20240229","messages":[]}`,
			wantMaxTokens: "8000", wantThinking: `{"type":"enabled","budget_tokens":7999}`,
		},
		{
			name: "预算小于 max_tokens 时不夹", maxTokens: "32000", budget: "10000",
			body:          `{"model":"claude-3-opus-20240229","messages":[]}`,
			wantMaxTokens: "32000", wantThinking: `{"type":"enabled","budget_tokens":10000}`,
		},
		{
			name: "max_tokens 缺失时不夹", budget: "50000",
			body:         `{"model":"claude-3-opus-20240229","messages":[]}`,
			wantThinking: `{"type":"enabled","budget_tokens":50000}`,
		},
		{
			name: "夹完低于 1024 则整段跳过", maxTokens: "500", budget: "10000",
			body:          `{"model":"claude-3-opus-20240229","messages":[]}`,
			wantMaxTokens: "500", wantThinking: "<缺失>",
		},
		{
			name: "偏好本身低于 1024 则跳过", budget: "500",
			body:         `{"model":"claude-3-opus-20240229","messages":[],"max_tokens":32000}`,
			wantThinking: "<缺失>",
		},
		{
			name: "夹完恰好 1023 则跳过", maxTokens: "1024", budget: "2000",
			body:          `{"model":"claude-3-opus-20240229","messages":[]}`,
			wantMaxTokens: "1024", wantThinking: "<缺失>",
		},
		{
			name: "夹完恰好 1024 则施加", maxTokens: "1025", budget: "2000",
			body:          `{"model":"claude-3-opus-20240229","messages":[]}`,
			wantMaxTokens: "1025", wantThinking: `{"type":"enabled","budget_tokens":1024}`,
		},
		{
			name: "预算恰好 1024 且无需夹", budget: "1024",
			body:         `{"model":"claude-3-opus-20240229","messages":[],"max_tokens":32000}`,
			wantThinking: `{"type":"enabled","budget_tokens":1024}`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			provider := claudeOverrideProvider()
			provider.AnthropicMaxTokensPreference = testCase.maxTokens
			provider.AnthropicThinkingBudgetPreference = testCase.budget
			tree, _ := applyOverrides(t, provider, convert.ProtocolAnthropicMessages, testCase.body)
			if testCase.wantMaxTokens != "" {
				if got := fieldJSON(t, tree, "max_tokens"); got != testCase.wantMaxTokens {
					t.Fatalf("max_tokens = %s，期望 %s", got, testCase.wantMaxTokens)
				}
			}
			if got := fieldJSON(t, tree, "thinking"); got != testCase.wantThinking {
				t.Fatalf("thinking = %s，期望 %s", got, testCase.wantThinking)
			}
		})
	}
}

func TestAnthropicAdaptiveThinking(t *testing.T) {
	t.Run("all 模式恒命中", func(t *testing.T) {
		provider := claudeOverrideProvider()
		provider.AnthropicAdaptiveThinking = json.RawMessage(
			`{"effort":"high","modelMatchMode":"all","models":[]}`)
		tree, _ := applyOverrides(t, provider, convert.ProtocolAnthropicMessages,
			`{"model":"claude-opus-4-6","messages":[],"max_tokens":8000}`)
		if got := fieldJSON(t, tree, "thinking"); got != `{"type":"adaptive"}` {
			t.Fatalf("thinking = %s", got)
		}
		if got := fieldJSON(t, tree, "output_config"); got != `{"effort":"high"}` {
			t.Fatalf("output_config = %s", got)
		}
	})

	t.Run("specific 模式按前缀命中", func(t *testing.T) {
		provider := claudeOverrideProvider()
		provider.AnthropicAdaptiveThinking = json.RawMessage(
			`{"effort":"max","modelMatchMode":"specific","models":["claude-opus-4-6"]}`)
		tree, _ := applyOverrides(t, provider, convert.ProtocolAnthropicMessages,
			`{"model":"claude-opus-4-6-20250514","messages":[]}`)
		if got := fieldJSON(t, tree, "thinking"); got != `{"type":"adaptive"}` {
			t.Fatalf("thinking = %s", got)
		}
	})

	t.Run("specific 模式未命中则原样", func(t *testing.T) {
		provider := claudeOverrideProvider()
		provider.AnthropicAdaptiveThinking = json.RawMessage(
			`{"effort":"high","modelMatchMode":"specific","models":["claude-opus-4-6"]}`)
		assertBodyUnchangedAudited(t, provider, convert.ProtocolAnthropicMessages, "",
			`{"model":"claude-sonnet-4-5","messages":[],"max_tokens":8000,`+
				`"thinking":{"type":"enabled","budget_tokens":5000}}`)
	})

	t.Run("保留既有 output_config 的兄弟字段", func(t *testing.T) {
		provider := claudeOverrideProvider()
		provider.AnthropicAdaptiveThinking = json.RawMessage(
			`{"effort":"medium","modelMatchMode":"all","models":[]}`)
		tree, _ := applyOverrides(t, provider, convert.ProtocolAnthropicMessages,
			`{"model":"claude-opus-4-6","messages":[],"output_config":{"some_other_field":"preserve"}}`)
		if got := fieldJSON(t, tree, "output_config"); got != `{"some_other_field":"preserve","effort":"medium"}` {
			t.Fatalf("output_config = %s", got)
		}
	})

	t.Run("自适应替换掉既有 budget_tokens", func(t *testing.T) {
		provider := claudeOverrideProvider()
		provider.AnthropicAdaptiveThinking = json.RawMessage(
			`{"effort":"high","modelMatchMode":"all","models":[]}`)
		tree, _ := applyOverrides(t, provider, convert.ProtocolAnthropicMessages,
			`{"model":"claude-opus-4-6","messages":[],"thinking":{"type":"enabled","budget_tokens":10240}}`)
		if got := fieldJSON(t, tree, "thinking"); got != `{"type":"adaptive"}` {
			t.Fatalf("thinking = %s", got)
		}
	})

	t.Run("自适应与 max_tokens 同时生效", func(t *testing.T) {
		provider := claudeOverrideProvider()
		provider.AnthropicMaxTokensPreference = "32000"
		provider.AnthropicAdaptiveThinking = json.RawMessage(
			`{"effort":"high","modelMatchMode":"all","models":[]}`)
		tree, _ := applyOverrides(t, provider, convert.ProtocolAnthropicMessages,
			`{"model":"claude-opus-4-6","messages":[],"max_tokens":8000}`)
		if got := fieldJSON(t, tree, "max_tokens"); got != "32000" {
			t.Fatalf("max_tokens = %s", got)
		}
		if got := fieldJSON(t, tree, "thinking"); got != `{"type":"adaptive"}` {
			t.Fatalf("thinking = %s", got)
		}
	})

	t.Run("脏 jsonb 视同未配置", func(t *testing.T) {
		provider := claudeOverrideProvider()
		provider.AnthropicAdaptiveThinking = json.RawMessage(`"not-an-object"`)
		assertBodyUnchanged(t, provider, convert.ProtocolAnthropicMessages,
			`{"model":"claude-opus-4-6","messages":[],"max_tokens":8000}`)
	})
}

func TestAnthropicAdaptiveAndBudgetCoexistence(t *testing.T) {
	t.Run("模型命中自适应时忽略预算", func(t *testing.T) {
		provider := claudeOverrideProvider()
		provider.AnthropicThinkingBudgetPreference = "10240"
		provider.AnthropicAdaptiveThinking = json.RawMessage(
			`{"effort":"high","modelMatchMode":"specific","models":["claude-opus-4-6"]}`)
		tree, _ := applyOverrides(t, provider, convert.ProtocolAnthropicMessages,
			`{"model":"claude-opus-4-6","messages":[],"max_tokens":32000}`)
		if got := fieldJSON(t, tree, "thinking"); got != `{"type":"adaptive"}` {
			t.Fatalf("thinking = %s", got)
		}
	})

	t.Run("模型未命中则退回预算", func(t *testing.T) {
		provider := claudeOverrideProvider()
		provider.AnthropicThinkingBudgetPreference = "10240"
		provider.AnthropicAdaptiveThinking = json.RawMessage(
			`{"effort":"high","modelMatchMode":"specific","models":["claude-opus-4-6"]}`)
		tree, _ := applyOverrides(t, provider, convert.ProtocolAnthropicMessages,
			`{"model":"claude-sonnet-4-5","messages":[],"max_tokens":32000}`)
		if got := fieldJSON(t, tree, "thinking"); got != `{"type":"enabled","budget_tokens":10240}` {
			t.Fatalf("thinking = %s", got)
		}
		if got := fieldJSON(t, tree, "output_config"); got != "<缺失>" {
			t.Fatalf("退回预算时不应写 output_config: %s", got)
		}
	})
}

func TestAnthropicAuditTrail(t *testing.T) {
	t.Run("偏好全为 inherit/null 时无条目", func(t *testing.T) {
		provider := claudeOverrideProvider()
		provider.AnthropicMaxTokensPreference = "inherit"
		assertBodyUnchanged(t, provider, convert.ProtocolAnthropicMessages,
			`{"model":"claude-3-opus-20240229","messages":[],"max_tokens":8000}`)
	})

	t.Run("记录 before/after 与 changed", func(t *testing.T) {
		provider := claudeOverrideProvider()
		provider.AnthropicMaxTokensPreference = "32000"
		_, applier := applyOverrides(t, provider, convert.ProtocolAnthropicMessages,
			`{"model":"claude-3-opus-20240229","messages":[],"max_tokens":8000}`)
		entry := auditEntryOf(t, applier)
		if entry["type"] != "provider_parameter_override" || entry["scope"] != "provider" {
			t.Fatalf("审计形状不符: %#v", entry)
		}
		if entry["providerId"] != int64(11) || entry["providerName"] != "供应商甲" {
			t.Fatalf("审计缺 provider 身份: %#v", entry)
		}
		change := changeOf(t, entry, "max_tokens")
		if change["before"] != json.Number("8000") || change["after"] != json.Number("32000") {
			t.Fatalf("max_tokens 的 before/after = %#v", change)
		}
		if change["changed"] != true {
			t.Fatalf("max_tokens 应记 changed=true: %#v", change)
		}
	})

	t.Run("值恰好相同则 changed=false 但 hit=true", func(t *testing.T) {
		provider := claudeOverrideProvider()
		provider.AnthropicMaxTokensPreference = "8000"
		_, applier := applyOverrides(t, provider, convert.ProtocolAnthropicMessages,
			`{"model":"claude-3-opus-20240229","messages":[],"max_tokens":8000}`)
		entry := auditEntryOf(t, applier)
		if entry["hit"] != true || entry["changed"] != false {
			t.Fatalf("hit/changed = %#v", entry)
		}
	})

	t.Run("自适应模式记 output_config.effort", func(t *testing.T) {
		provider := claudeOverrideProvider()
		provider.AnthropicAdaptiveThinking = json.RawMessage(
			`{"effort":"high","modelMatchMode":"all","models":[]}`)
		_, applier := applyOverrides(t, provider, convert.ProtocolAnthropicMessages,
			`{"model":"claude-opus-4-6","messages":[]}`)
		entry := auditEntryOf(t, applier)
		if got := changeOf(t, entry, "output_config.effort")["after"]; got != "high" {
			t.Fatalf("output_config.effort after = %#v", got)
		}
		if got := changeOf(t, entry, "thinking.type")["after"]; got != "adaptive" {
			t.Fatalf("thinking.type after = %#v", got)
		}
	})
}

// ---------------------------------------------------------------------------
// codex（codex-provider-overrides.test.ts）
// ---------------------------------------------------------------------------

func codexOverrideProvider() Provider {
	return Provider{ID: 21, Name: "codex 甲", Type: convert.ProviderCodex}
}

func TestCodexOverridesNonCodexProviderUntouched(t *testing.T) {
	provider := Provider{
		Type:                             convert.ProviderClaude,
		CodexReasoningEffortPreference:   "high",
		CodexParallelToolCallsPreference: "true",
	}
	assertBodyUnchanged(t, provider, convert.ProtocolOpenAIResponses, `{"model":"gpt-5","input":[]}`)
}

func TestCodexScalarPreferences(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(provider *Provider)
		body     string
		path     []string
		expected string
	}{
		{
			name:     "reasoning.effort",
			mutate:   func(p *Provider) { p.CodexReasoningEffortPreference = "xhigh" },
			body:     `{"model":"gpt-5","reasoning":{"summary":"auto"}}`,
			path:     []string{"reasoning", "effort"},
			expected: `"xhigh"`,
		},
		{
			name:     "reasoning.summary",
			mutate:   func(p *Provider) { p.CodexReasoningSummaryPreference = "detailed" },
			body:     `{"model":"gpt-5","reasoning":{"effort":"low"}}`,
			path:     []string{"reasoning", "summary"},
			expected: `"detailed"`,
		},
		{
			name:     "text.verbosity",
			mutate:   func(p *Provider) { p.CodexTextVerbosityPreference = "high" },
			body:     `{"model":"gpt-5","text":{"format":{"type":"text"}}}`,
			path:     []string{"text", "verbosity"},
			expected: `"high"`,
		},
		{
			name:     "parallel_tool_calls",
			mutate:   func(p *Provider) { p.CodexParallelToolCallsPreference = "false" },
			body:     `{"model":"gpt-5","parallel_tool_calls":true}`,
			path:     nil,
			expected: `false`,
		},
		{
			name:     "service_tier",
			mutate:   func(p *Provider) { p.CodexServiceTierPreference = "priority" },
			body:     `{"model":"gpt-5"}`,
			path:     nil,
			expected: `"priority"`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			provider := codexOverrideProvider()
			testCase.mutate(&provider)
			tree, _ := applyOverrides(t, provider, convert.ProtocolOpenAIResponses, testCase.body)
			var got string
			if testCase.path == nil {
				key := testCase.name
				got = fieldJSON(t, tree, key)
			} else {
				got = nestedJSON(t, tree, testCase.path[0], testCase.path[1])
			}
			if got != testCase.expected {
				t.Fatalf("%s = %s，期望 %s", testCase.name, got, testCase.expected)
			}
		})
	}
}

func TestCodexReasoningGroupPreservesSiblings(t *testing.T) {
	provider := codexOverrideProvider()
	provider.CodexReasoningEffortPreference = "high"
	provider.CodexReasoningSummaryPreference = "detailed"
	tree, _ := applyOverrides(t, provider, convert.ProtocolOpenAIResponses,
		`{"model":"gpt-5","reasoning":{"effort":"low","summary":"auto","custom":"keep"}}`)
	if got := fieldJSON(t, tree, "reasoning"); got != `{"effort":"high","summary":"detailed","custom":"keep"}` {
		t.Fatalf("reasoning = %s", got)
	}
}

func TestCodexImageGenerationInjectAndRemove(t *testing.T) {
	t.Run("注入工具", func(t *testing.T) {
		provider := codexOverrideProvider()
		provider.CodexImageGenerationPreference = "true"
		tree, _ := applyOverrides(t, provider, convert.ProtocolOpenAIResponses,
			`{"model":"gpt-5","tools":[{"type":"function","name":"f"}]}`)
		if got := fieldJSON(t, tree, "tools"); got != `[{"type":"function","name":"f"},{"type":"image_generation"}]` {
			t.Fatalf("tools = %s", got)
		}
	})

	t.Run("已有则不重复注入", func(t *testing.T) {
		provider := codexOverrideProvider()
		provider.CodexImageGenerationPreference = "true"
		assertBodyUnchangedAudited(t, provider, convert.ProtocolOpenAIResponses, "",
			`{"model":"gpt-5","tools":[{"type":"image_generation"}]}`)
	})

	t.Run("input 里已有也不重复注入", func(t *testing.T) {
		provider := codexOverrideProvider()
		provider.CodexImageGenerationPreference = "true"
		assertBodyUnchangedAudited(t, provider, convert.ProtocolOpenAIResponses, "",
			`{"model":"gpt-5","input":[{"type":"additional_tools","tools":[{"type":"image_generation"}]}]}`)
	})

	t.Run("namespace 形态也算图像工具", func(t *testing.T) {
		provider := codexOverrideProvider()
		provider.CodexImageGenerationPreference = "true"
		assertBodyUnchangedAudited(t, provider, convert.ProtocolOpenAIResponses, "",
			`{"model":"gpt-5","tools":[{"type":"namespace","name":"image_gen"}]}`)
	})

	t.Run("关闭时移除工具", func(t *testing.T) {
		provider := codexOverrideProvider()
		provider.CodexImageGenerationPreference = "false"
		tree, _ := applyOverrides(t, provider, convert.ProtocolOpenAIResponses,
			`{"model":"gpt-5","tools":[{"type":"function","name":"f"},{"type":"image_generation"}]}`)
		if got := fieldJSON(t, tree, "tools"); got != `[{"type":"function","name":"f"}]` {
			t.Fatalf("tools = %s", got)
		}
	})

	t.Run("关闭且滤空则整键删除", func(t *testing.T) {
		provider := codexOverrideProvider()
		provider.CodexImageGenerationPreference = "false"
		tree, _ := applyOverrides(t, provider, convert.ProtocolOpenAIResponses,
			`{"model":"gpt-5","tools":[{"type":"image_generation"}]}`)
		if got := fieldJSON(t, tree, "tools"); got != "<缺失>" {
			t.Fatalf("tools = %s", got)
		}
	})

	t.Run("关闭时清理 input.additional_tools 且滤空条目整条丢弃", func(t *testing.T) {
		provider := codexOverrideProvider()
		provider.CodexImageGenerationPreference = "false"
		tree, _ := applyOverrides(t, provider, convert.ProtocolOpenAIResponses,
			`{"model":"gpt-5","input":[{"type":"message"},`+
				`{"type":"additional_tools","tools":[{"type":"image_generation"}]}]}`)
		if got := fieldJSON(t, tree, "input"); got != `[{"type":"message"}]` {
			t.Fatalf("input = %s", got)
		}
	})

	t.Run("关闭时保留 additional_tools 里的其他工具", func(t *testing.T) {
		provider := codexOverrideProvider()
		provider.CodexImageGenerationPreference = "false"
		tree, _ := applyOverrides(t, provider, convert.ProtocolOpenAIResponses,
			`{"model":"gpt-5","input":[{"type":"additional_tools","tools":[`+
				`{"type":"image_generation"},{"type":"function","name":"f"}]}]}`)
		if got := fieldJSON(t, tree, "input"); got !=
			`[{"type":"additional_tools","tools":[{"type":"function","name":"f"}]}]` {
			t.Fatalf("input = %s", got)
		}
	})
}

func TestCodexToolChoiceHandling(t *testing.T) {
	t.Run("开启时把图像工具补进白名单", func(t *testing.T) {
		provider := codexOverrideProvider()
		provider.CodexImageGenerationPreference = "true"
		tree, _ := applyOverrides(t, provider, convert.ProtocolOpenAIResponses,
			`{"model":"gpt-5","tools":[{"type":"function","name":"f"}],`+
				`"tool_choice":{"type":"allowed_tools","tools":[{"type":"function","name":"f"}]}}`)
		if got := fieldJSON(t, tree, "tool_choice"); got !=
			`{"type":"allowed_tools","tools":[{"type":"function","name":"f"},{"type":"image_generation"}]}` {
			t.Fatalf("tool_choice = %s", got)
		}
	})

	t.Run("开启时非白名单形态只补工具、不动 tool_choice", func(t *testing.T) {
		provider := codexOverrideProvider()
		provider.CodexImageGenerationPreference = "true"
		tree, _ := applyOverrides(t, provider, convert.ProtocolOpenAIResponses,
			`{"model":"gpt-5","tool_choice":"auto"}`)
		if got := fieldJSON(t, tree, "tool_choice"); got != `"auto"` {
			t.Fatalf("tool_choice = %s，期望原样 auto（只有白名单形态才补）", got)
		}
		if got := fieldJSON(t, tree, "tools"); got != `[{"type":"image_generation"}]` {
			t.Fatalf("tools = %s", got)
		}
	})

	t.Run("关闭时图像工具即选则改成 none", func(t *testing.T) {
		provider := codexOverrideProvider()
		provider.CodexImageGenerationPreference = "false"
		tree, _ := applyOverrides(t, provider, convert.ProtocolOpenAIResponses,
			`{"model":"gpt-5","tools":[{"type":"function","name":"f"}],"tool_choice":"image_generation"}`)
		if got := fieldJSON(t, tree, "tool_choice"); got != `"none"` {
			t.Fatalf("tool_choice = %s，期望 none（不得回退 auto）", got)
		}
	})

	t.Run("关闭时白名单被清空改成 none", func(t *testing.T) {
		provider := codexOverrideProvider()
		provider.CodexImageGenerationPreference = "false"
		tree, _ := applyOverrides(t, provider, convert.ProtocolOpenAIResponses,
			`{"model":"gpt-5","tools":[{"type":"function","name":"f"}],`+
				`"tool_choice":{"type":"allowed_tools","tools":[{"type":"image_generation"}]}}`)
		if got := fieldJSON(t, tree, "tool_choice"); got != `"none"` {
			t.Fatalf("tool_choice = %s，期望 none（不得回退 auto）", got)
		}
	})

	t.Run("关闭时白名单里保留其他工具", func(t *testing.T) {
		provider := codexOverrideProvider()
		provider.CodexImageGenerationPreference = "false"
		tree, _ := applyOverrides(t, provider, convert.ProtocolOpenAIResponses,
			`{"model":"gpt-5","tools":[{"type":"function","name":"f"}],`+
				`"tool_choice":{"type":"allowed_tools","tools":[`+
				`{"type":"image_generation"},{"type":"function","name":"f"}]}}`)
		if got := fieldJSON(t, tree, "tool_choice"); got !=
			`{"type":"allowed_tools","tools":[{"type":"function","name":"f"}]}` {
			t.Fatalf("tool_choice = %s", got)
		}
	})

	t.Run("关闭且已无可用工具则删掉 tool_choice", func(t *testing.T) {
		provider := codexOverrideProvider()
		provider.CodexImageGenerationPreference = "false"
		tree, _ := applyOverrides(t, provider, convert.ProtocolOpenAIResponses,
			`{"model":"gpt-5","tool_choice":"auto"}`)
		if got := fieldJSON(t, tree, "tool_choice"); got != "<缺失>" {
			t.Fatalf("tool_choice = %s", got)
		}
	})
}

func TestCodexAuditTrail(t *testing.T) {
	t.Run("请求自带 priority 时即使无偏好也写条目", func(t *testing.T) {
		provider := codexOverrideProvider()
		_, applier := applyOverrides(t, provider, convert.ProtocolOpenAIResponses,
			`{"model":"gpt-5","service_tier":"priority"}`)
		entry := auditEntryOf(t, applier)
		if entry["hit"] != true {
			t.Fatalf("priority 应触发审计: %#v", entry)
		}
		if got := changeOf(t, entry, "service_tier")["changed"]; got != false {
			t.Fatalf("priority 且无覆写时 changed 应为 false: %#v", got)
		}
	})

	t.Run("无偏好且非 priority 时不写条目", func(t *testing.T) {
		provider := codexOverrideProvider()
		assertBodyUnchanged(t, provider, convert.ProtocolOpenAIResponses, `{"model":"gpt-5"}`)
	})

	t.Run("图像工具与 tool_choice 各记一条 change", func(t *testing.T) {
		provider := codexOverrideProvider()
		provider.CodexImageGenerationPreference = "true"
		_, applier := applyOverrides(t, provider, convert.ProtocolOpenAIResponses,
			`{"model":"gpt-5","tools":[{"type":"function","name":"f"}]}`)
		entry := auditEntryOf(t, applier)
		if got := changeOf(t, entry, "tools.image_generation"); got["before"] != false || got["after"] != true {
			t.Fatalf("tools.image_generation = %#v", got)
		}
		if got := changeOf(t, entry, "tool_choice")["changed"]; got != false {
			t.Fatalf("未动 tool_choice 时 changed 应为 false: %#v", got)
		}
	})
}

// ---------------------------------------------------------------------------
// gemini（gemini/provider-overrides.test.ts）
// ---------------------------------------------------------------------------

func geminiOverrideProvider() Provider {
	return Provider{
		ID:                           31,
		Name:                         "gemini 甲",
		Type:                         convert.ProviderGemini,
		GeminiGoogleSearchPreference: "inherit",
	}
}

func TestGeminiGoogleSearchOverride(t *testing.T) {
	t.Run("非 gemini 供应商不动", func(t *testing.T) {
		provider := geminiOverrideProvider()
		provider.Type = convert.ProviderClaude
		provider.GeminiGoogleSearchPreference = "enabled"
		assertBodyUnchanged(t, provider, convert.ProtocolGemini, `{"contents":[{"parts":[{"text":"hi"}]}]}`)
	})

	t.Run("inherit 与空值不动", func(t *testing.T) {
		for _, value := range []string{"inherit", ""} {
			provider := geminiOverrideProvider()
			provider.GeminiGoogleSearchPreference = value
			assertBodyUnchanged(t, provider, convert.ProtocolGemini,
				`{"contents":[],"tools":[{"googleSearch":{}}]}`)
		}
	})

	t.Run("enabled 且不存在则注入", func(t *testing.T) {
		provider := geminiOverrideProvider()
		provider.GeminiGoogleSearchPreference = "enabled"
		tree, _ := applyOverrides(t, provider, convert.ProtocolGemini, `{"contents":[]}`)
		if got := fieldJSON(t, tree, "tools"); got != `[{"googleSearch":{}}]` {
			t.Fatalf("tools = %s", got)
		}
	})

	t.Run("enabled 与既有工具并存", func(t *testing.T) {
		provider := geminiOverrideProvider()
		provider.GeminiGoogleSearchPreference = "enabled"
		tree, _ := applyOverrides(t, provider, convert.ProtocolGemini,
			`{"contents":[],"tools":[{"codeExecution":{}}]}`)
		if got := fieldJSON(t, tree, "tools"); got != `[{"codeExecution":{}},{"googleSearch":{}}]` {
			t.Fatalf("tools = %s", got)
		}
	})

	t.Run("enabled 且已有则不重复", func(t *testing.T) {
		provider := geminiOverrideProvider()
		provider.GeminiGoogleSearchPreference = "enabled"
		assertBodyUnchangedAudited(t, provider, convert.ProtocolGemini, "",
			`{"contents":[],"tools":[{"googleSearch":{}}]}`)
	})

	t.Run("gemini-cli 同样生效", func(t *testing.T) {
		provider := geminiOverrideProvider()
		provider.Type = convert.ProviderGeminiCLI
		provider.GeminiGoogleSearchPreference = "enabled"
		tree, _ := applyOverrides(t, provider, convert.ProtocolGemini, `{"contents":[]}`)
		if got := fieldJSON(t, tree, "tools"); got != `[{"googleSearch":{}}]` {
			t.Fatalf("tools = %s", got)
		}
	})

	t.Run("非生成类端点不注入", func(t *testing.T) {
		provider := geminiOverrideProvider()
		provider.GeminiGoogleSearchPreference = "enabled"
		assertBodyUnchangedAt(t, provider, convert.ProtocolGemini,
			"/v1beta/models/gemini-embedding-001:embedContent",
			`{"content":{"parts":[{"text":"hello"}]}}`)
	})

	t.Run("生成类端点注入（大小写与尾斜杠归一）", func(t *testing.T) {
		provider := geminiOverrideProvider()
		provider.GeminiGoogleSearchPreference = "enabled"
		tree, _ := applyOverridesAt(t, provider, convert.ProtocolGemini,
			"/v1beta/models/gemini-2.5-flash:streamGenerateContent/", `{"contents":[]}`)
		if got := fieldJSON(t, tree, "tools"); got != `[{"googleSearch":{}}]` {
			t.Fatalf("tools = %s", got)
		}
	})

	t.Run("disabled 且存在则移除，滤空则删键", func(t *testing.T) {
		provider := geminiOverrideProvider()
		provider.GeminiGoogleSearchPreference = "disabled"
		tree, _ := applyOverrides(t, provider, convert.ProtocolGemini,
			`{"contents":[],"tools":[{"googleSearch":{}}]}`)
		if got := fieldJSON(t, tree, "tools"); got != "<缺失>" {
			t.Fatalf("tools = %s", got)
		}
	})

	t.Run("disabled 保留其他工具", func(t *testing.T) {
		provider := geminiOverrideProvider()
		provider.GeminiGoogleSearchPreference = "disabled"
		tree, _ := applyOverrides(t, provider, convert.ProtocolGemini,
			`{"contents":[],"tools":[{"codeExecution":{}},{"googleSearch":{}},{"functionDeclarations":[]}]}`)
		if got := fieldJSON(t, tree, "tools"); got != `[{"codeExecution":{}},{"functionDeclarations":[]}]` {
			t.Fatalf("tools = %s", got)
		}
	})

	t.Run("disabled 且不存在则不动", func(t *testing.T) {
		provider := geminiOverrideProvider()
		provider.GeminiGoogleSearchPreference = "disabled"
		assertBodyUnchangedAudited(t, provider, convert.ProtocolGemini, "",
			`{"contents":[],"tools":[{"codeExecution":{}}]}`)
	})
}

func TestGeminiGoogleSearchAudit(t *testing.T) {
	cases := []struct {
		name          string
		preference    string
		body          string
		wantAction    string
		wantHadSearch bool
	}{
		{"注入", "enabled", `{"contents":[]}`, "inject", false},
		{"已存在则 passthrough", "enabled", `{"contents":[],"tools":[{"googleSearch":{}}]}`, "passthrough", true},
		{"移除", "disabled", `{"contents":[],"tools":[{"googleSearch":{}}]}`, "remove", true},
		{"不存在则 passthrough", "disabled", `{"contents":[],"tools":[{"codeExecution":{}}]}`, "passthrough", false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			provider := geminiOverrideProvider()
			provider.GeminiGoogleSearchPreference = testCase.preference
			_, applier := applyOverrides(t, provider, convert.ProtocolGemini, testCase.body)
			entry := auditEntryOf(t, applier)
			if entry["type"] != "gemini_google_search_override" || entry["scope"] != "request" {
				t.Fatalf("审计形状不符: %#v", entry)
			}
			if entry["action"] != testCase.wantAction {
				t.Fatalf("action = %#v，期望 %s", entry["action"], testCase.wantAction)
			}
			if entry["preference"] != testCase.preference {
				t.Fatalf("preference = %#v", entry["preference"])
			}
			if entry["hadGoogleSearchInRequest"] != testCase.wantHadSearch {
				t.Fatalf("hadGoogleSearchInRequest = %#v", entry["hadGoogleSearchInRequest"])
			}
		})
	}

	t.Run("继承时不写条目", func(t *testing.T) {
		provider := geminiOverrideProvider()
		assertBodyUnchanged(t, provider, convert.ProtocolGemini, `{"contents":[]}`)
	})
}

// ---------------------------------------------------------------------------
// 缓存 TTL（forwarder.ts:774 / 805 / 8829）
// ---------------------------------------------------------------------------

func TestCacheTTLOverrideRewritesEphemeralBlocks(t *testing.T) {
	body := `{"model":"claude-sonnet-4-5","system":[{"type":"text","text":"sys",` +
		`"cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}},` +
		`{"type":"text","text":"plain"}]}]}`

	t.Run("1h 写进 system 与 messages 的块", func(t *testing.T) {
		provider := claudeOverrideProvider()
		provider.CacheTTLPreference = "1h"
		tree, applier := applyOverrides(t, provider, convert.ProtocolAnthropicMessages, body)
		if got := arrayItemJSON(t, tree, "system", 0); got !=
			`{"type":"text","text":"sys","cache_control":{"type":"ephemeral","ttl":"1h"}}` {
			t.Fatalf("system[0] = %s", got)
		}
		if got := applier.ResolvedCacheTTL(provider); got != "1h" {
			t.Fatalf("ResolvedCacheTTL = %q，期望 1h（出站 anthropic-beta 头靠它）", got)
		}
	})

	t.Run("5m 同样写入", func(t *testing.T) {
		provider := claudeOverrideProvider()
		provider.CacheTTLPreference = "5m"
		tree, applier := applyOverrides(t, provider, convert.ProtocolAnthropicMessages, body)
		if got := arrayItemJSON(t, tree, "system", 0); got !=
			`{"type":"text","text":"sys","cache_control":{"type":"ephemeral","ttl":"5m"}}` {
			t.Fatalf("system[0] = %s", got)
		}
		if got := applier.ResolvedCacheTTL(provider); got != "5m" {
			t.Fatalf("ResolvedCacheTTL = %q", got)
		}
	})

	t.Run("inherit 不动正文", func(t *testing.T) {
		provider := claudeOverrideProvider()
		provider.CacheTTLPreference = "inherit"
		assertBodyUnchanged(t, provider, convert.ProtocolAnthropicMessages, body)
	})

	t.Run("没有 ephemeral 块时不动", func(t *testing.T) {
		provider := claudeOverrideProvider()
		provider.CacheTTLPreference = "1h"
		assertBodyUnchanged(t, provider, convert.ProtocolAnthropicMessages,
			`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	})

	t.Run("非 anthropic 供应商不解析 TTL", func(t *testing.T) {
		provider := codexOverrideProvider()
		provider.CacheTTLPreference = "1h"
		applier := NewProviderOverrideApplier("")
		if got := applier.ResolvedCacheTTL(provider); got != "" {
			t.Fatalf("非 anthropic 供应商不应解析 TTL: %q", got)
		}
	})
}

// TestBuildPlanSurfacesCacheTTL1h 钉住「正文写了 1h，出站头也必须带 beta 标记」。
//
// 两处取同一个解析结果：正文里的 `cache_control.ttl` 由覆写写，`anthropic-beta` 头由
// BuildUpstreamHeaders 读 CacheTTL1h 补（forwarder.ts:8829）。若只做前一半，上游会按 5m 缓存。
func TestBuildPlanSurfacesCacheTTL1h(t *testing.T) {
	provider := claudeProvider("https://relay.example.com")
	provider.CacheTTLPreference = "1h"
	applier := NewProviderOverrideApplier("")

	plan, err := BuildPlan(PlanInput{
		Client: newClaudeRequest(`{"model":"claude-sonnet-4-5","max_tokens":64,"system":[` +
			`{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],` +
			`"messages":[{"role":"user","content":"hi"}]}`),
		Target:    Target{Provider: provider},
		Overrides: applier,
	})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	if !strings.Contains(string(plan.Body), `"ttl":"1h"`) {
		t.Fatalf("正文应写入 ttl: %s", plan.Body)
	}
	beta := plan.Headers.Get("anthropic-beta")
	if !strings.Contains(beta, "extended-cache-ttl-2025-04-11") {
		t.Fatalf("出站 anthropic-beta 缺 1h 标记: %q", beta)
	}
	if !strings.Contains(beta, "prompt-caching-2024-07-31") {
		t.Fatalf("出站 anthropic-beta 缺缓存依赖标记: %q", beta)
	}
}

// TestBuildPlanCarriesOverrideAudits 钉住审计条目随计划交给终态追加。
func TestBuildPlanCarriesOverrideAudits(t *testing.T) {
	provider := claudeProvider("https://relay.example.com")
	provider.AnthropicMaxTokensPreference = "32000"
	applier := NewProviderOverrideApplier("")

	plan, err := BuildPlan(PlanInput{
		Client:    newClaudeRequest(`{"model":"claude-sonnet-4-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`),
		Target:    Target{Provider: provider},
		Overrides: applier,
	})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	if len(plan.OverrideSpecialSettings) != 1 {
		t.Fatalf("计划上的覆写审计条目数 = %d，期望 1", len(plan.OverrideSpecialSettings))
	}
	if plan.OverrideSpecialSettings[0]["type"] != "provider_parameter_override" {
		t.Fatalf("审计类型 = %#v", plan.OverrideSpecialSettings[0]["type"])
	}
}

// TestGeminiPassthroughPlanAppliesOverrides 钉住 gemini 原生透传路径也走覆写。
//
// gemini 一族在 BuildPlan 早期就分派到 buildGeminiPassthroughPlan，不经过通用正文改写路径；
// 若不在这条支线上单独施加，googleSearch 的注入/移除永远不会生效。
func TestGeminiPassthroughPlanAppliesOverrides(t *testing.T) {
	provider := Provider{
		ID:                           31,
		Name:                         "gemini 甲",
		Type:                         convert.ProviderGemini,
		URL:                          "https://generativelanguage.example.com",
		GeminiGoogleSearchPreference: "enabled",
	}
	applier := NewProviderOverrideApplier("/v1beta/models/gemini-2.5-flash:generateContent")

	plan, err := BuildPlan(PlanInput{
		Client: ClientRequest{
			Method:  "POST",
			Path:    "/v1beta/models/gemini-2.5-flash:generateContent",
			Format:  convert.FormatGemini,
			HasBody: true,
			Body:    []byte(`{"contents":[{"parts":[{"text":"hi"}]}]}`),
		},
		Target:    Target{Provider: provider},
		Overrides: applier,
	})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	if !strings.Contains(string(plan.Body), `"googleSearch"`) {
		t.Fatalf("gemini 透传路径应注入 googleSearch: %s", plan.Body)
	}
	if len(plan.OverrideSpecialSettings) != 1 {
		t.Fatalf("审计条目数 = %d", len(plan.OverrideSpecialSettings))
	}
}
