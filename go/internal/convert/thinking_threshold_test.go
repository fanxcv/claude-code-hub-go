package convert

import "testing"

// 思考档位：缺预算与 adaptive 取保守档，不代客户端抬高思考预算。
// 抬高会拉长首字时间、压低流式吞吐（v1.11.0 起生产实测劣化）。
func TestAnthropicReasoningConservativeFallback(t *testing.T) {
	cases := []struct {
		name     string
		thinking string
		want     string
	}{
		{"enabled 缺 budget_tokens 取 low", `{"type":"enabled"}`, thinkingLevelLow},
		{"adaptive 取 medium", `{"type":"adaptive"}`, thinkingLevelMedium},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := ParseJSON([]byte(`{"thinking":` + tc.thinking + `}`))
			if err != nil {
				t.Fatalf("ParseJSON: %v", err)
			}
			loss := &LossCollector{}
			reasoning := decodeAnthropicReasoning(body, loss)
			if reasoning == nil || !reasoning.HasEffort {
				t.Fatalf("期望记出思考强度，实际 %+v", reasoning)
			}
			if reasoning.Effort != tc.want {
				t.Fatalf("Effort = %q，期望 %q", reasoning.Effort, tc.want)
			}
			if reasoning.Effort == thinkingLevelHigh {
				t.Fatalf("不得回落到 high：会把「模型自定」改写成「想最多」")
			}
		})
	}
}
