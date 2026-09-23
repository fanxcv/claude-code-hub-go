package forward

import (
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/gate"
)

// TestGateFamilyRespectsGateMode 钉住门控开关的两态（Node forwarder.ts:2034、5334）。
//
// 判据：`gateMode === "enforce" || replayOwner || forceCodexResponsesStream` 才门控。
// 本包只判模式与 ForceGate（replay owner 由接线层把模式直接给 enforce，见 dataplane 的
// gateModeForRequest）。家族必须**始终**解得出来，SkipGate 除外。
func TestGateFamilyRespectsGateMode(t *testing.T) {
	provider := Provider{Type: convert.ProviderClaude}
	want, ok := gate.MapProviderTypeToFamily(string(provider.Type))
	if !ok {
		t.Fatalf("供应商类型 %s 未映射到家族，用例前提不成立", provider.Type)
	}

	cases := []struct {
		name      string
		options   StreamOptions
		wantGated bool
		// wantFamily 为空表示期望交回空家族（SkipGate 是 Go 自己的「整包不做」开关）。
		wantFamily gate.Family
	}{
		{name: "未设模式等同 enforce（开）", options: StreamOptions{}, wantGated: true, wantFamily: want},
		{name: "开关开 = enforce 门控", options: StreamOptions{GateMode: gate.ModeEnforce}, wantGated: true, wantFamily: want},
		{name: "开关关 = off 不门控", options: StreamOptions{GateMode: gate.ModeOff}, wantGated: false, wantFamily: want},
		{
			name:       "ForceGate 覆盖 off（codex 强制流式必须门控）",
			options:    StreamOptions{GateMode: gate.ModeOff, ForceGate: true},
			wantGated:  true,
			wantFamily: want,
		},
		{name: "SkipGate 优先于一切", options: StreamOptions{SkipGate: true}, wantGated: false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			family, gated := testCase.options.gateFamily(provider)
			if gated != testCase.wantGated {
				t.Errorf("gated = %v，期望 %v", gated, testCase.wantGated)
			}
			if family != testCase.wantFamily {
				t.Errorf("family = %q，期望 %q（off 也要解出家族，SkipGate 除外）", family, testCase.wantFamily)
			}
		})
	}
}
