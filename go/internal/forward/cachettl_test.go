package forward

import "testing"

// TestPlanAppliedCacheTTL 钉住「计费读到的缓存 TTL」与出站正文里实际写下的 TTL 同源。
//
// 生产上供应商偏好绝大多数是 inherit（见 ResolvedCacheTTL 的注释），此时正文里的
// cache_control.ttl 全由客户端决定——故本用例的重头在 inherit 那几档。
func TestPlanAppliedCacheTTL(t *testing.T) {
	cases := []struct {
		name       string
		preference string
		body       string
		want       string
	}{
		{
			name:       "inherit：客户端标 1h 就按 1h 计",
			preference: "inherit",
			body: `{"model":"claude-sonnet-4-5","max_tokens":64,` +
				`"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral","ttl":"1h"}}],` +
				`"messages":[{"role":"user","content":"hi"}]}`,
			want: "1h",
		},
		{
			name:       "inherit：客户端未标 ttl（Anthropic 默认 5m）",
			preference: "inherit",
			body: `{"model":"claude-sonnet-4-5","max_tokens":64,` +
				`"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],` +
				`"messages":[{"role":"user","content":"hi"}]}`,
			want: "5m",
		},
		{
			name:       "inherit：客户端 5m/1h 混用",
			preference: "inherit",
			body: `{"model":"claude-sonnet-4-5","max_tokens":64,` +
				`"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral","ttl":"1h"}}],` +
				`"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","ttl":"5m"}}]}]}`,
			want: "mixed",
		},
		{
			name:       "inherit：脏 ttl 归 5m（只有恰好 1h 才算 1h）",
			preference: "inherit",
			body: `{"model":"claude-sonnet-4-5","max_tokens":64,` +
				`"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral","ttl":"2h"}}],` +
				`"messages":[{"role":"user","content":"hi"}]}`,
			want: "5m",
		},
		{
			name:       "inherit：正文没有缓存块则不归任何档",
			preference: "inherit",
			body: `{"model":"claude-sonnet-4-5","max_tokens":64,` +
				`"messages":[{"role":"user","content":"hi"}]}`,
			want: "",
		},
		{
			name:       "偏好 1h 覆写客户端的 5m（读的是覆写后的正文）",
			preference: "1h",
			body: `{"model":"claude-sonnet-4-5","max_tokens":64,` +
				`"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral","ttl":"5m"}}],` +
				`"messages":[{"role":"user","content":"hi"}]}`,
			want: "1h",
		},
		{
			name:       "只在 tools 上标 1h 不计入（扫描面与写入面一致）",
			preference: "inherit",
			body: `{"model":"claude-sonnet-4-5","max_tokens":64,"tools":[{"name":"t","description":"d",` +
				`"input_schema":{"type":"object"},"cache_control":{"type":"ephemeral","ttl":"1h"}}],` +
				`"messages":[{"role":"user","content":"hi"}]}`,
			want: "",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			provider := claudeProvider("https://relay.example.com")
			provider.CacheTTLPreference = testCase.preference

			plan, err := BuildPlan(PlanInput{
				Client:    newClaudeRequest(testCase.body),
				Target:    Target{Provider: provider},
				Overrides: NewProviderOverrideApplier(""),
			})
			if err != nil {
				t.Fatalf("BuildPlan 失败: %v", err)
			}
			if got := plan.AppliedCacheTTL(); got != testCase.want {
				t.Fatalf("AppliedCacheTTL = %q，期望 %q\n正文: %s", got, testCase.want, plan.Body)
			}
		})
	}
}

// TestPlanAppliedCacheTTLIsNilSafe 没有计划（无上游尝试）时不得 panic，也不得给出归属。
func TestPlanAppliedCacheTTLIsNilSafe(t *testing.T) {
	var plan *Plan
	if got := plan.AppliedCacheTTL(); got != "" {
		t.Fatalf("空计划 AppliedCacheTTL = %q，期望空串", got)
	}
}
