package dataplane

import (
	"errors"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
)

// TestStreamErrorMessage 钉住「哪些流式终态算失败」。
//
// 为何值得单测：`message_request.is_success` 由 DB 触发器按 error_message 是否为空算，
// 仪表盘错误数同源——这个函数的返回值直接决定一条 200 的流式请求在账上算成功还是失败。
// 尤其要紧的是「上游在正文中途断流」那一格：它此前被记成成功，于是客户端看到的残流
// 在我们的统计里完全不存在（生产 2026-09-23 wb 池代理，约 3.7% 的 wb 流）。
func TestStreamErrorMessage(t *testing.T) {
	readErr := errors.New("forward: 上游读错")

	cases := []struct {
		name        string
		outcome     forward.StreamOutcome
		observation forward.Observation
		want        string
		wantNil     bool
	}{
		{
			name:    "有错误对象时用它（优先于终态名）",
			outcome: forward.StreamOutcome{Kind: forward.TerminalUpstreamTruncated, Err: readErr},
			want:    readErr.Error(),
		},
		{
			name:    "上游中途断流（未见终止标记）：记失败",
			outcome: forward.StreamOutcome{Kind: forward.TerminalUpstreamTruncated, StatusCode: 200},
			// 夹具取自生产实证：wb 池代理在 tool_calls 参数中间干净地 FIN，
			// 尾部既无 finish_reason 也无 [DONE]，Bytes/Frames 与正常流无异。
			observation: forward.Observation{Bytes: 60332, Frames: 242, Model: "deepseek-v4.1-flash"},
			want:        upstreamStreamCutMessage,
		},
		{
			// 本格钉的是不变式守卫（见 forward/terminal_kind_test.go 的 TestTerminalKindForInvariant）：
			// 「已见标记 + 截断」结构上不可达，但守卫在场时它归成功——错误方向取「少记失败」，
			// 因为反过来会连带写渠道冷却。
			name:        "截断但已见终止标记：按成功记账（守卫，该组合结构上不可达）",
			outcome:     forward.StreamOutcome{Kind: forward.TerminalUpstreamTruncated, StatusCode: 200},
			observation: forward.Observation{Bytes: 60332, Frames: 242, CompletionMarker: true},
			wantNil:     true,
		},
		{
			name:        "正常完成：按成功记账",
			outcome:     forward.StreamOutcome{Kind: forward.TerminalCompleted, StatusCode: 200},
			observation: forward.Observation{Bytes: 1024, Frames: 8, CompletionMarker: true},
			wantNil:     true,
		},
		{
			name:    "静默超时：用终态名",
			outcome: forward.StreamOutcome{Kind: forward.TerminalIdleTimeout, StatusCode: 200},
			want:    string(forward.TerminalIdleTimeout),
		},
		{
			name:    "客户端中断：用终态名",
			outcome: forward.StreamOutcome{Kind: forward.TerminalClientAborted, StatusCode: 200},
			want:    string(forward.TerminalClientAborted),
		},
		{
			name: "有错误对象且非截断：错误对象仍优先",
			outcome: forward.StreamOutcome{
				Kind: forward.TerminalLocalError, Err: readErr},
			want: readErr.Error(),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := streamErrorMessage(tc.outcome, tc.observation)
			if tc.wantNil {
				if got != nil {
					t.Fatalf("期望按成功记账（nil），实得 %q", *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("期望错误文案 %q，实得 nil（会被记成成功）", tc.want)
			}
			if *got != tc.want {
				t.Fatalf("错误文案 = %q，期望 %q", *got, tc.want)
			}
		})
	}
}
