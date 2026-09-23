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
			name:    "上游 404 写资源类墓碑（会话绑定只清绑定）",
			failure: &forward.Failure{Category: forward.CategoryResourceNotFound, ProviderID: 9, StatusCode: 404},
			want: terminal.AffinityDirective{
				TombstoneProviderID: 9,
				TombstoneKind:       terminal.AffinityTombstoneResourceNotFound,
			},
		},
		{
			name:    "供应商故障的墓碑种类是故障类（零值，语义与改造前逐字一致）",
			failure: &forward.Failure{Category: forward.CategoryProviderError, ProviderID: 9, StatusCode: 502},
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
			outcome: forward.StreamOutcome{Kind: forward.TerminalIncomplete, StatusCode: 200,
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
			name: "客户端中断只写前缀墓碑（Node 的 499/CLIENT_ABORTED 分支）",
			outcome: forward.StreamOutcome{Kind: forward.TerminalClientAborted, StatusCode: 200,
				Provider: forward.Provider{ID: 7}, ClientAbort: true},
			// 前缀墓碑照写（Node 对齐：这次请求确实没成，同前缀后续该绕开它），
			// 但会话绑定侧不得动作：供应商没出错，冷却一家健康渠道会让下一请求无故换家。
			want: terminal.AffinityDirective{
				TombstoneProviderID: 7,
				TombstoneKind:       terminal.AffinityTombstonePrefixOnly,
			},
		},
		{
			name: "静默超时写墓碑（即便已交付过正文：不是干净 EOF）",
			outcome: forward.StreamOutcome{Kind: forward.TerminalIdleTimeout, StatusCode: 200,
				Provider:    forward.Provider{ID: 7},
				Observation: forward.Observation{Bytes: 4096, Frames: 12}},
			want: terminal.AffinityDirective{TombstoneProviderID: 7},
		},
		{
			name: "未正常结束（截断）写墓碑",
			outcome: forward.StreamOutcome{Kind: forward.TerminalUpstreamTruncated, StatusCode: 200,
				Provider: forward.Provider{ID: 7}},
			want: terminal.AffinityDirective{TombstoneProviderID: 7},
		},
		// 以下五条钉住「流尾缺终止标记」的判据与其边界。
		//
		// 判据是：TerminalUpstreamTruncated **结构上蕴含**未见到协议终止标记
		// （terminalKindFor 只在 !CompletionMarker 时落本类型；其 io.EOF 分支不可达，且已补齐
		// 标记判断，见 forward/stream.go 与 forward 包的同名钉子）。故本类型一律写墓碑与冷却。
		//
		// 生产实证（2026-09-23，wb 池代理）：上游在 tool_calls 参数中间干净地 FIN，客户端拿到
		// 残流（尾部既无 finish_reason 也无 [DONE]），终态正是本类型。
		//
		// 历史注记：本组曾断言「TerminalUpstreamTruncated 就是干净的 EOF，故只缺协议标记」，
		// 并把 Bytes=60332/Frames=242 那一形态钉成「两边都不写」——那条断言与生产事实相抵
		// （该夹具正是被切断的那类流），现已改期望值。原先那条「标记已见 ⇒ 两边都不写」的用例
		// 已删除：该组合结构上不可达，留着会让人以为存在这一类收尾。
		{
			name: "流尾无终止标记且正文被中途切断：写墓碑与冷却",
			outcome: forward.StreamOutcome{Kind: forward.TerminalUpstreamTruncated, StatusCode: 200,
				Provider:    forward.Provider{ID: 7},
				Observation: forward.Observation{Bytes: 60332, Frames: 242, Model: "deepseek-v4.1-flash"}},
			want: terminal.AffinityDirective{TombstoneProviderID: 7},
		},
		{
			name: "流尾缺终止标记且正文未送达（零字节）：仍写墓碑",
			outcome: forward.StreamOutcome{Kind: forward.TerminalUpstreamTruncated, StatusCode: 200,
				Provider:    forward.Provider{ID: 7},
				Observation: forward.Observation{Frames: 1}},
			want: terminal.AffinityDirective{TombstoneProviderID: 7},
		},
		{
			name: "流尾缺终止标记但流内错误帧：仍写墓碑",
			outcome: forward.StreamOutcome{Kind: forward.TerminalUpstreamTruncated, StatusCode: 200,
				Provider:    forward.Provider{ID: 7},
				Observation: forward.Observation{Bytes: 1024, ErrorText: "overloaded_error"}},
			want: terminal.AffinityDirective{TombstoneProviderID: 7},
		},
		{
			name: "流尾缺终止标记且非 2xx：仍写墓碑",
			outcome: forward.StreamOutcome{Kind: forward.TerminalUpstreamTruncated, StatusCode: 502,
				Provider:    forward.Provider{ID: 7},
				Observation: forward.Observation{Bytes: 512}},
			want: terminal.AffinityDirective{TombstoneProviderID: 7},
		},
		{
			name: "本地读错（报文体被中途切断）即便有正文也写墓碑",
			outcome: forward.StreamOutcome{Kind: forward.TerminalLocalError, StatusCode: 200,
				Provider:    forward.Provider{ID: 7},
				Observation: forward.Observation{Bytes: 2048}},
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
