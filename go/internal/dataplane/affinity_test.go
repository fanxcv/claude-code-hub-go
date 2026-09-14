package dataplane

import (
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
	"github.com/fanxcv/claude-code-hub-go/go/internal/terminal"
)

// 亲和写回时机判定的表驱动钉子：Node 的分支逐条对应（依据见 affinity.go 的注释）。
// 判错时不会报错、只会让粘性悄悄失效或让羊群撞向故障供应商，故必须逐分支钉住。
func TestAffinityDirectiveForNonStream(t *testing.T) {
	cases := []struct {
		name    string
		result  *forward.Result
		failure *forward.Failure
		want    terminal.AffinityDirective
	}{
		{
			name:   "成功 200 写回给产出响应的供应商",
			result: &forward.Result{Provider: forward.Provider{ID: 7}, StatusCode: 200},
			want:   terminal.AffinityDirective{WinnerProviderID: 7},
		},
		{
			name:   "成功但非 2xx 不写回",
			result: &forward.Result{Provider: forward.Provider{ID: 7}, StatusCode: 502},
			want:   terminal.AffinityDirective{},
		},
		{
			name:    "供应商错误写墓碑",
			failure: &forward.Failure{Category: forward.CategoryProviderError, ProviderID: 9, StatusCode: 500},
			want:    terminal.AffinityDirective{TombstoneProviderID: 9},
		},
		{
			name:    "上游 404 写墓碑",
			failure: &forward.Failure{Category: forward.CategoryResourceNotFound, ProviderID: 9, StatusCode: 404},
			want:    terminal.AffinityDirective{TombstoneProviderID: 9},
		},
		{
			name:    "请求级闸门失败（request-scoped）不写墓碑",
			failure: &forward.Failure{Category: forward.CategoryProviderError, ProviderID: 9, RequestScoped: true},
			want:    terminal.AffinityDirective{},
		},
		{
			name:    "系统错误不写墓碑",
			failure: &forward.Failure{Category: forward.CategorySystemError, ProviderID: 9},
			want:    terminal.AffinityDirective{},
		},
		{
			name:    "客户端中断不写墓碑",
			failure: &forward.Failure{Category: forward.CategoryClientAbort, ProviderID: 9},
			want:    terminal.AffinityDirective{},
		},
		{
			name:    "本地过载不写墓碑",
			failure: &forward.Failure{Category: forward.CategoryLocalOverload, ProviderID: 9},
			want:    terminal.AffinityDirective{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := affinityDirectiveForNonStream(tc.result, tc.failure)
			if got != tc.want {
				t.Errorf("指令 = %+v，期望 %+v", got, tc.want)
			}
		})
	}
}

func TestAffinityDirectiveForStream(t *testing.T) {
	cases := []struct {
		name    string
		outcome forward.StreamOutcome
		want    terminal.AffinityDirective
	}{
		{
			name: "协议终态正常抵达写回",
			outcome: forward.StreamOutcome{Kind: forward.TerminalCompleted, StatusCode: 200,
				Provider: forward.Provider{ID: 7}},
			want: terminal.AffinityDirective{WinnerProviderID: 7},
		},
		{
			name: "incomplete（2xx 且无错误帧）两边都不写",
			outcome: forward.StreamOutcome{Kind: forward.TerminalUpstreamTruncated, StatusCode: 200,
				Provider:    forward.Provider{ID: 7},
				Observation: forward.Observation{SawIncomplete: true}},
			want: terminal.AffinityDirective{},
		},
		{
			name: "上游错误帧写墓碑",
			outcome: forward.StreamOutcome{Kind: forward.TerminalUpstreamError, StatusCode: 200,
				Provider:    forward.Provider{ID: 7},
				Observation: forward.Observation{ErrorText: "overloaded_error"}},
			want: terminal.AffinityDirective{TombstoneProviderID: 7},
		},
		{
			name: "客户端中断写墓碑（Node 的 499/CLIENT_ABORTED 分支）",
			outcome: forward.StreamOutcome{Kind: forward.TerminalClientAborted, StatusCode: 200,
				Provider: forward.Provider{ID: 7}, ClientAbort: true},
			want: terminal.AffinityDirective{TombstoneProviderID: 7},
		},
		{
			name: "静默超时写墓碑",
			outcome: forward.StreamOutcome{Kind: forward.TerminalIdleTimeout, StatusCode: 200,
				Provider: forward.Provider{ID: 7}},
			want: terminal.AffinityDirective{TombstoneProviderID: 7},
		},
		{
			name: "未正常结束（截断）写墓碑",
			outcome: forward.StreamOutcome{Kind: forward.TerminalUpstreamTruncated, StatusCode: 200,
				Provider: forward.Provider{ID: 7}},
			want: terminal.AffinityDirective{TombstoneProviderID: 7},
		},
		{
			name: "非 2xx 终态写墓碑",
			outcome: forward.StreamOutcome{Kind: forward.TerminalCompleted, StatusCode: 400,
				Provider: forward.Provider{ID: 7}},
			want: terminal.AffinityDirective{TombstoneProviderID: 7},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := affinityDirectiveForStream(tc.outcome)
			if got != tc.want {
				t.Errorf("指令 = %+v，期望 %+v", got, tc.want)
			}
		})
	}
}
