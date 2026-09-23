package forward

import (
	"errors"
	"io"
	"testing"
)

// TestTerminalKindForInvariant 钉住终态分类里一条**下游赖以判账**的不变式：
//
//	TerminalUpstreamTruncated  ⟹  !Observation.CompletionMarker
//
// 为何值得单测：dataplane 侧据此判「正文是否已交付」——截断即记失败并写渠道冷却
// （见 dataplane.streamErrorMessage 与 affinityDirectiveForStream）。若本不变式被打破
// （即出现「已见终止标记却落截断」的终态），一条正文已完整交付的健康流会被记成失败、
// 给健康渠道写冷却。2026-09-23 的独立审查正是在这里提出的风险：io.EOF 分支原本不看标记。
//
// 该分支当前**不可达**（泵在 io.EOF 时调 settle(true, nil, nil)，completion.Err 永远不是
// io.EOF；见 pump.go 的 readSourceOnce 与全部 settle 调用点）。本用例把它的行为也钉住，
// 是为了让不变式**不依赖分支是否可达**——将来若有路径真的传入 io.EOF，本用例会当场变红，
// 而不是让健康流被静默误判。
func TestTerminalKindForInvariant(t *testing.T) {
	readErr := errors.New("forward: 上游读错")

	cases := []struct {
		name        string
		completion  PumpCompletion
		observation Observation
		want        TerminalKind
	}{
		{
			name:        "客户端中断优先于一切",
			completion:  PumpCompletion{ClientAborted: true, Err: readErr},
			observation: Observation{CompletionMarker: true},
			want:        TerminalClientAborted,
		},
		{
			name:       "静默超时",
			completion: PumpCompletion{Err: errStreamIdleTimeout},
			want:       TerminalIdleTimeout,
		},
		{
			name:       "io.EOF 且未见标记：截断",
			completion: PumpCompletion{Err: io.EOF},
			want:       TerminalUpstreamTruncated,
		},
		{
			// 这一格是本用例的主要目的：已见标记 + io.EOF 必须归 Completed，
			// 否则下游会把正文已交付的健康流记成失败并写冷却。
			name:        "io.EOF 但已见终止标记：算完成（不变式守卫）",
			completion:  PumpCompletion{Err: io.EOF},
			observation: Observation{CompletionMarker: true},
			want:        TerminalCompleted,
		},
		{
			name:       "其它读错：本地错误",
			completion: PumpCompletion{Err: readErr},
			want:       TerminalLocalError,
		},
		{
			name:        "无错但有流内错误帧",
			completion:  PumpCompletion{},
			observation: Observation{ErrorText: "overloaded_error", CompletionMarker: true},
			want:        TerminalUpstreamError,
		},
		{
			name:        "干净收尾且已见标记：完成",
			completion:  PumpCompletion{StreamEndedNormally: true},
			observation: Observation{CompletionMarker: true},
			want:        TerminalCompleted,
		},
		{
			name:       "干净收尾但未见标记：截断（不变式的另一半）",
			completion: PumpCompletion{StreamEndedNormally: true},
			want:       TerminalUpstreamTruncated,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := terminalKindFor(tc.completion, tc.observation)
			if got != tc.want {
				t.Fatalf("终态 = %q，期望 %q", got, tc.want)
			}
			// 不变式本体：截断必然意味着没见到终止标记。
			if got == TerminalUpstreamTruncated && tc.observation.CompletionMarker {
				t.Fatalf("不变式被打破：终态为截断，但 CompletionMarker 为真" +
					"（下游会据此把健康流记成失败并写渠道冷却）")
			}
		})
	}
}
