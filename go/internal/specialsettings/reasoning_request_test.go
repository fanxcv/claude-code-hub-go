package specialsettings

import (
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// 本文件钉住「客户端要求了推理吗」这条判定：提交前速率闸的豁免以它为唯一输入
// （见 dataplane 的 captureRequestedEffort 与 forward.StreamOptions.ReasoningRequest）。
//
// 三条线各自的载体都覆盖，且「显式关掉思考」与「压根没提思考」必须都判 false——
// 后者是防误伤的方向（不该豁免的请求不能豁免），前者同样：`reasoning_effort: "none"`
// 是「不要推理」，把它当推理型等于把闸整体关掉。
func TestRequestSeeksReasoningAcrossLines(t *testing.T) {
	cases := []struct {
		name    string
		body    map[string]any
		format  convert.ClientFormat
		endpoit string
		want    bool
	}{
		{
			// 生产命中就是这一格（special_settings 的 codex_reasoning_effort: max|high）。
			name:    "responses 线 reasoning.effort=high ⇒ 是",
			body:    map[string]any{"reasoning": map[string]any{"effort": "high"}},
			format:  convert.FormatResponse,
			endpoit: "/v1/responses",
			want:    true,
		},
		{
			name:    "responses 线 reasoning.effort=max ⇒ 是",
			body:    map[string]any{"reasoning": map[string]any{"effort": "max"}},
			format:  convert.FormatResponse,
			endpoit: "/v1/responses",
			want:    true,
		},
		{
			name:    "responses 线 reasoning.effort=none ⇒ 否（显式关掉思考）",
			body:    map[string]any{"reasoning": map[string]any{"effort": "none"}},
			format:  convert.FormatResponse,
			endpoit: "/v1/responses",
			want:    false,
		},
		{
			name:    "chat 线顶层 reasoning_effort ⇒ 是",
			body:    map[string]any{"reasoning_effort": "high"},
			format:  convert.FormatOpenAI,
			endpoit: "/v1/chat/completions",
			want:    true,
		},
		{
			name:    "chat 线 reasoning.effort ⇒ 是",
			body:    map[string]any{"reasoning": map[string]any{"effort": "medium"}},
			format:  convert.FormatOpenAI,
			endpoit: "/v1/chat/completions",
			want:    true,
		},
		{
			name:    "chat 线 reasoning_effort=none ⇒ 否",
			body:    map[string]any{"reasoning_effort": "none"},
			format:  convert.FormatOpenAI,
			endpoit: "/v1/chat/completions",
			want:    false,
		},
		{
			// chat 线的端点位守卫：非 chat/completions 的 body 不算对话请求体。
			name:    "chat 线但端点为 responses ⇒ 否",
			body:    map[string]any{"reasoning_effort": "high"},
			format:  convert.FormatOpenAI,
			endpoit: "/v1/responses",
			want:    false,
		},
		{
			// Anthropic 扩展思考：RequestTrimmedEffort 认不出这一格（它只认 output_config.effort）。
			name:    "anthropic 线 thinking.enabled + budget_tokens ⇒ 是",
			body:    map[string]any{"thinking": map[string]any{"type": "enabled", "budget_tokens": 8192}},
			format:  convert.FormatClaude,
			endpoit: "/v1/messages",
			want:    true,
		},
		{
			name:    "anthropic 线 thinking.adaptive ⇒ 是（判据同 rectify：非空且非 disabled）",
			body:    map[string]any{"thinking": map[string]any{"type": "adaptive"}},
			format:  convert.FormatClaude,
			endpoit: "/v1/messages",
			want:    true,
		},
		{
			name:    "anthropic 线 thinking.disabled ⇒ 否",
			body:    map[string]any{"thinking": map[string]any{"type": "disabled"}},
			format:  convert.FormatClaude,
			endpoit: "/v1/messages",
			want:    false,
		},
		{
			name:    "anthropic 线 output_config.effort=high ⇒ 是",
			body:    map[string]any{"output_config": map[string]any{"effort": "high"}},
			format:  convert.FormatClaude,
			endpoit: "/v1/messages",
			want:    true,
		},
		{
			name:    "anthropic 线 thinking.type 空串 ⇒ 否",
			body:    map[string]any{"thinking": map[string]any{"type": ""}},
			format:  convert.FormatClaude,
			endpoit: "/v1/messages",
			want:    false,
		},
		{
			name:    "anthropic 线只有 budget_tokens（无 type） ⇒ 否（不猜）",
			body:    map[string]any{"thinking": map[string]any{"budget_tokens": 8192}},
			format:  convert.FormatClaude,
			endpoit: "/v1/messages",
			want:    false,
		},
		{
			name:    "三线都没提思考 ⇒ 否",
			body:    map[string]any{"model": "claude-sonnet-4-5", "max_tokens": 16},
			format:  convert.FormatClaude,
			endpoit: "/v1/messages",
			want:    false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := RequestSeeksReasoning(testCase.body, testCase.format, testCase.endpoit); got != testCase.want {
				t.Fatalf("RequestSeeksReasoning = %v，期望 %v（body=%v）", got, testCase.want, testCase.body)
			}
		})
	}
}
