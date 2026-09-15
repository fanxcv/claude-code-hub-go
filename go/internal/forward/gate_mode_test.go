package forward

import (
	"io"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/gate"
)

// TestGateFamilyRespectsGateMode 钉住门控模式的两态（Node forwarder.ts:2034、5334）。
//
// 判据：`gateMode === "enforce" || replayOwner || forceCodexResponsesStream` 才门控。
// 本包只判模式与 ForceGate（replay owner 由接线层把模式直接给 enforce，见 dataplane 的
// gateModeForRequest）。家族必须**始终**解得出来——shadow 模式要靠它做旁路分类。
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
		{name: "未设模式等同 enforce", options: StreamOptions{}, wantGated: true, wantFamily: want},
		{name: "enforce 门控", options: StreamOptions{GateMode: gate.ModeEnforce}, wantGated: true, wantFamily: want},
		{name: "off 不门控", options: StreamOptions{GateMode: gate.ModeOff}, wantGated: false, wantFamily: want},
		{name: "shadow 不门控", options: StreamOptions{GateMode: gate.ModeShadow}, wantGated: false, wantFamily: want},
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
				t.Errorf("family = %q，期望 %q（off/shadow 也要解出家族，SkipGate 除外）", family, testCase.wantFamily)
			}
		})
	}
}

// TestShadowSourceWrapsOnlyInShadowMode 钉住旁路观察的挂载条件与「不改字节」。
func TestShadowSourceWrapsOnlyInShadowMode(t *testing.T) {
	const payload = "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
	outcome := &AttemptOutcome{ProviderID: 7, ProviderName: "供应商甲"}

	t.Run("enforce 下交回原 reader", func(t *testing.T) {
		body := io.NopCloser(strings.NewReader(payload))
		got := (StreamOptions{GateMode: gate.ModeEnforce}).shadowSource(body, gate.FamilyAnthropic, outcome)
		if got != io.ReadCloser(body) {
			t.Fatal("非 shadow 模式不得包装 reader")
		}
	})

	t.Run("家族未知时不包装", func(t *testing.T) {
		body := io.NopCloser(strings.NewReader(payload))
		got := (StreamOptions{GateMode: gate.ModeShadow}).shadowSource(body, "", outcome)
		if got != io.ReadCloser(body) {
			t.Fatal("家族未知时不应包装（分类无从谈起）")
		}
	})

	t.Run("shadow 下包装且字节不变", func(t *testing.T) {
		body := io.NopCloser(strings.NewReader(payload))
		wrapped := (StreamOptions{GateMode: gate.ModeShadow}).shadowSource(body, gate.FamilyAnthropic, outcome)
		if wrapped == io.ReadCloser(body) {
			t.Fatal("shadow 模式应包装 reader")
		}
		data, err := io.ReadAll(wrapped)
		if err != nil {
			t.Fatalf("读取包装后的 reader 失败：%v", err)
		}
		if string(data) != payload {
			t.Errorf("包装改变了字节：%q", string(data))
		}
		if err := wrapped.Close(); err != nil {
			t.Errorf("Close 应透传到源：%v", err)
		}
	})
}
