package forward

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/specialsettings"
)

// 占位思考签名的**主动**剥离：客户端把上一轮我们补的占位签名带回来时，发往 Anthropic 供应商
// 之前必须拿掉。用例分两层——门控（哪一类供应商才剥）与端到端（真的发出去的字节里没有它）。

// TestForwardPlaceholderSignatureStrippedBeforeAnthropicProvider 是端到端断言：走到 claude 系
// 供应商的那一发请求正文里不得再出现占位签名，并留一条主动型审计条目。
func TestForwardPlaceholderSignatureStrippedBeforeAnthropicProvider(t *testing.T) {
	signature := convert.PlaceholderThinkingSignature()
	server := newRecordingServer(t, []int{200}, []string{""})
	body := `{"model":"claude-sonnet-4-5-20250929","max_tokens":16,"messages":[` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"上一轮的思考","signature":"` + signature + `"},` +
		`{"type":"text","text":"上一轮的回答"}]},` +
		`{"role":"user","content":"ping"}]}`
	collector := &auditCollector{}
	deps := Deps{
		Dial:                         newTestDial(t),
		Facts:                        PlanFacts{Client: newClaudeRequest(body)},
		Limits:                       Limits{RetryDelay: time.Millisecond},
		RectifierAudit:               collector.record,
		PlaceholderThinkingSignature: true,
	}

	if _, err := Forward(context.Background(), nil, newTestCandidate(1, "供应商甲", server.URL, 1), deps); err != nil {
		t.Fatalf("转发失败: %v", err)
	}

	received := server.received(t)
	if len(received) != 1 {
		t.Fatalf("主动剥离不产生额外尝试，实际 %d 次", len(received))
	}
	if strings.Contains(received[0], signature) {
		t.Fatalf("占位签名不得发给 Anthropic 供应商: %s", received[0])
	}
	if !strings.Contains(received[0], "上一轮的回答") {
		t.Fatalf("同一条 assistant 消息的其它块必须保留: %s", received[0])
	}
	entries := collector.all()
	if len(entries) != 1 || entries[0]["type"] != specialsettings.TypeThinkingPlaceholderSignatureRectifier {
		t.Fatalf("审计条目不对: %v", auditTypes(t, collector))
	}
	if entries[0]["removedPlaceholderThinkingBlocks"] != 1 {
		t.Fatalf("removedPlaceholderThinkingBlocks 不对: %#v", entries[0]["removedPlaceholderThinkingBlocks"])
	}
}

// TestPlaceholderSignatureStripGate 钉住门控：只有「开关开 + 目标供应商是 Anthropic 系」才剥。
//
// 为什么单独钉门控而不只靠端到端：其它供应商标记（如 openai-compatible）走的是转换路径，
// 那里思考块本就被编码器丢弃——端到端看不出门控是否生效，而门控失效的后果是「换一家
// Anthropic 上游就又开始吃 400」。
func TestPlaceholderSignatureStripGate(t *testing.T) {
	signature := convert.PlaceholderThinkingSignature()
	body := `{"model":"claude-sonnet-4-5-20250929","max_tokens":16,"messages":[` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"` + signature + `"}]}]}`

	cases := []struct {
		name     string
		enabled  bool
		provider convert.ProviderType
		want     bool
	}{
		{name: "开关开 + Anthropic 供应商", enabled: true, provider: convert.ProviderClaude, want: true},
		{name: "开关关 + Anthropic 供应商", enabled: false, provider: convert.ProviderClaude, want: false},
		{name: "开关开 + 非 Anthropic 供应商", enabled: true, provider: convert.ProviderOpenAICompatible, want: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			collector := &auditCollector{}
			state := rectifierState{client: newClaudeRequest(body), sink: collector.record}
			state.applyPlaceholderSignatureStrip(
				Provider{ID: 7, Name: "供应商甲", Type: testCase.provider}, testCase.enabled, logx.New(nil))

			stripped := !strings.Contains(string(state.client.Body), signature)
			if stripped != testCase.want {
				t.Fatalf("剥离结果 = %v，期望 %v；正文 %s", stripped, testCase.want, state.client.Body)
			}
			if got := len(collector.all()); (got > 0) != testCase.want {
				t.Fatalf("审计条目数 = %d，期望与剥离结果一致", got)
			}
		})
	}
}

// TestPlaceholderSignatureStripKeepsRealSignatures 钉住「只剥自家造的」：真实签名的正文
// 即使开关开着也不改一个字节，也不留审计条目（不留痕即「没动手」）。
func TestPlaceholderSignatureStripKeepsRealSignatures(t *testing.T) {
	body := `{"model":"claude-sonnet-4-5-20250929","max_tokens":16,"messages":[` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"t","signature":"Ereal-upstream-signature"}]}]}`
	collector := &auditCollector{}
	state := rectifierState{client: newClaudeRequest(body), sink: collector.record}

	state.applyPlaceholderSignatureStrip(
		Provider{ID: 7, Name: "供应商甲", Type: convert.ProviderClaude}, true, logx.New(nil))

	if got := string(state.client.Body); got != body {
		t.Fatalf("真实签名的正文被改动:\n got %s\nwant %s", got, body)
	}
	if len(collector.all()) != 0 {
		t.Fatalf("没动手就不该留审计条目: %v", auditTypes(t, collector))
	}
}
