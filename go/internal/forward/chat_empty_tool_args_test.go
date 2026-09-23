package forward

import (
	"bytes"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// chatEmptyArgsBody 是含一个空参数 tool_call 的 chat 正文（客户端工具无参数时的真实形态）。
const chatEmptyArgsBody = `{"model":"gpt-4o","messages":[` +
	`{"role":"user","content":"ping"},` +
	`{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"t","arguments":""}}]}` +
	`]}`

// openAICompatibleProvider 是原生 chat 线（openai-compatible 型）的供应商投影。
func openAICompatibleProvider(baseURL string) Provider {
	provider := claudeProvider(baseURL)
	provider.Type = convert.ProviderOpenAICompatible
	return provider
}

// TestBuildPlanNormalizesEmptyToolArgsOnNativeChatPath 钉住 native（同协议）chat 路的空参数归一：
// 空参数被归一为规范无参形态 `{}`，其余字节逐字不动。
func TestBuildPlanNormalizesEmptyToolArgsOnNativeChatPath(t *testing.T) {
	plan, err := BuildPlan(PlanInput{
		Client: chatClient(chatEmptyArgsBody),
		Target: Target{Provider: openAICompatibleProvider("https://relay.example.com")},
	})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	if plan.Conversion != nil {
		t.Fatalf("前置不成立：chat 客户端 + openai-compatible 供应商应是 native 路，实际有转换计划")
	}
	if plan.Protocol != convert.ProtocolOpenAIChat {
		t.Fatalf("前置不成立：目标线 = %q，期望 %q", plan.Protocol, convert.ProtocolOpenAIChat)
	}
	want := `"arguments":"{}"`
	if got := string(plan.Body); !bytes.Contains(plan.Body, []byte(want)) {
		t.Fatalf("空参数未归一：正文 = %s", got)
	}
	if bytes.Contains(plan.Body, []byte(`"arguments":""`)) {
		t.Fatalf("空参数仍在：正文 = %s", plan.Body)
	}
	// 其余字节逐字不动：把那一处 `""` 换回后，必须与原正文完全相同（键序/空白/数字字面量全保真）。
	if got := bytes.Replace(plan.Body, []byte(want), []byte(`"arguments":""`), 1); !bytes.Equal(got, []byte(chatEmptyArgsBody)) {
		t.Fatalf("除该处外正文被改动：\n got = %s\nwant = %s", got, chatEmptyArgsBody)
	}
}

// TestBuildPlanNormalizesChatEmptyToolArgsAfterContentArray 钉住真实 chat 形态：
// 同一 message 内 content 是数组、tool_calls 在其后时，路径判定不得错位（旧实现会静默跳过）。
func TestBuildPlanNormalizesChatEmptyToolArgsAfterContentArray(t *testing.T) {
	const body = `{"messages":[{"role":"assistant","content":[{"type":"text","text":"hi"}],` +
		`"tool_calls":[{"id":"c1","type":"function","function":{"name":"t","arguments":""}}]}]}`
	plan, err := BuildPlan(PlanInput{
		Client: chatClient(body),
		Target: Target{Provider: openAICompatibleProvider("https://relay.example.com")},
	})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	want := `"arguments":"{}"`
	if got := string(plan.Body); !bytes.Contains(plan.Body, []byte(want)) {
		t.Fatalf("content 数组之后的空参数未归一：正文 = %s", got)
	}
	if got := bytes.Replace(plan.Body, []byte(want), []byte(`"arguments":""`), 1); !bytes.Equal(got, []byte(body)) {
		t.Fatalf("除该处外正文被改动：\n got = %s\nwant = %s", got, body)
	}
}

// TestBuildPlanChatEmptyToolArgsInvariants 逐条钉住不改的样子：非空参数、无 tool_calls、
// arguments 非字符串、大整数与键序。这些字节必须原样出站。
func TestBuildPlanChatEmptyToolArgsInvariants(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "arguments 已是 {}",
			body: `{"messages":[{"role":"assistant","tool_calls":[{"function":{"name":"t","arguments":"{}"}}]}]}`,
		},
		{
			name: "arguments 有内容",
			body: `{"messages":[{"role":"assistant","tool_calls":[{"function":{"name":"t","arguments":"{\"path\":\"/tmp\"}"}}]}]}`,
		},
		{
			name: "无 tool_calls",
			body: `{"messages":[{"role":"user","content":"ping"},{"role":"assistant","content":"pong"}]}`,
		},
		{
			name: "arguments 非字符串",
			body: `{"messages":[{"role":"assistant","tool_calls":[{"function":{"name":"t","arguments":{}}}]}]}`,
		},
		{
			name: "大整数与键序",
			body: `{"z_flag":true,"messages":[{"role":"assistant","tool_calls":[{"function":{"arguments":"{}","name":"t"}}]}],` +
				`"a_big":9007199254740993,"m_nested":{"k1":1e21,"k2":"\u003cu\u003e"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := BuildPlan(PlanInput{
				Client: chatClient(tc.body),
				Target: Target{Provider: openAICompatibleProvider("https://relay.example.com")},
			})
			if err != nil {
				t.Fatalf("BuildPlan 失败: %v", err)
			}
			if string(plan.Body) != tc.body {
				t.Fatalf("正文应逐字节为\n want = %s\n  got = %s", tc.body, plan.Body)
			}
		})
	}
}

// TestBuildPlanLeavesChatEmptyToolArgsOnNonChatTarget 钉住适用面：
// 目标线不是 openai-chat（anthropic / responses）时完全不碰正文。
func TestBuildPlanLeavesChatEmptyToolArgsOnNonChatTarget(t *testing.T) {
	const body = `{"messages":[{"role":"assistant","tool_calls":[{"function":{"name":"t","arguments":""}}]}]}`
	cases := []struct {
		name     string
		provider convert.ProviderType
		protocol convert.WireProtocol
	}{
		{"anthropic 线", convert.ProviderClaude, convert.ProtocolAnthropicMessages},
		{"codex 线", convert.ProviderCodex, convert.ProtocolOpenAIResponses},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := claudeProvider("https://relay.example.com")
			provider.Type = tc.provider
			plan, err := BuildPlan(PlanInput{Client: chatClient(body), Target: Target{Provider: provider}})
			if err != nil {
				t.Fatalf("BuildPlan 失败: %v", err)
			}
			if plan.Protocol != tc.protocol {
				t.Fatalf("前置不成立：目标线 = %q，期望 %q", plan.Protocol, tc.protocol)
			}
			// 正文用 chat 的形状是故意的：它把「按目标线不适用」与「按正文形状不适用」两件事分开钉住。
			if string(plan.Body) != body {
				t.Fatalf("目标线非 chat 时正文不得被改：%s", plan.Body)
			}
		})
	}
}

// TestBuildPlanConversionPathBodyUnaffectedByChatEmptyArgsNormalization 钉住「跨协议路不重复处理」：
// 有转换计划时出站正文必须与编解码器的输出逐字节相同。
func TestBuildPlanConversionPathBodyUnaffectedByChatEmptyArgsNormalization(t *testing.T) {
	provider := claudeProvider("https://relay.example.com")
	provider.Type = convert.ProviderCodex

	client := chatClient(`{"model":"gpt-5-codex","messages":[{"role":"user","content":"ping"},` +
		`{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"t","arguments":""}}]}]}`)
	plan, err := BuildPlan(PlanInput{
		Client:            client,
		Target:            Target{Provider: provider},
		ConversionEnabled: true,
	})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	if plan.Conversion == nil || plan.ConversionFallback {
		t.Fatalf("前置不成立：本用例需转换成功，实际 conversion=%v fallback=%v",
			plan.Conversion != nil, plan.ConversionFallback)
	}
	expected, _, _, err := convertBody(client, plan.Conversion)
	if err != nil {
		t.Fatalf("convertBody 失败: %v", err)
	}
	if !bytes.Equal(plan.Body, expected) {
		t.Fatalf("转换路正文被二次改写：\n got = %s\nwant = %s", plan.Body, expected)
	}
}
