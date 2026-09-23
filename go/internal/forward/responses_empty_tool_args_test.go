package forward

import (
	"bytes"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// codexProvider 是原生 Responses 线（codex 型）的供应商投影。
func codexProvider(baseURL string) Provider {
	provider := claudeProvider(baseURL)
	provider.Type = convert.ProviderCodex
	return provider
}

// emptyArgsBody 是含一个空参数 function_call 的 Responses 正文（客户端工具无参数时的真实形态）。
const emptyArgsBody = `{"model":"gpt-5-codex","stream":true,"input":[` +
	`{"type":"message","role":"user","content":[{"type":"input_text","text":"ping"}]},` +
	`{"type":"function_call","call_id":"c1","name":"t","arguments":""}` +
	`],"tool_choice":"auto"}`

// TestBuildPlanNormalizesEmptyToolArgsOnNativeResponsesPath 钉住生产 400 的修复点：
// native（同协议）路的 Responses 正文里，空参数被归一为规范无参形态 `{}`，其余字节逐字不动。
func TestBuildPlanNormalizesEmptyToolArgsOnNativeResponsesPath(t *testing.T) {
	plan, err := BuildPlan(PlanInput{
		Client: responsesClient(emptyArgsBody),
		Target: Target{Provider: codexProvider("https://ollama.example.com")},
	})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	if plan.Conversion != nil {
		t.Fatalf("前置不成立：responses 客户端 + codex 供应商应是 native 路，实际有转换计划")
	}
	want := `"arguments":"{}"`
	if got := string(plan.Body); !bytes.Contains(plan.Body, []byte(want)) {
		t.Fatalf("空参数未归一：正文 = %s", got)
	}
	if bytes.Contains(plan.Body, []byte(`"arguments":""`)) {
		t.Fatalf("空参数仍在：正文 = %s", plan.Body)
	}
	// 其余字节逐字不动：把那一处 `""` 换回后，必须与原正文完全相同（键序/空白/数字字面量全保真）。
	if got := bytes.Replace(plan.Body, []byte(want), []byte(`"arguments":""`), 1); !bytes.Equal(got, []byte(emptyArgsBody)) {
		t.Fatalf("除该处外正文被改动：\n got = %s\nwant = %s", got, emptyArgsBody)
	}
}

// TestBuildPlanEmptyToolArgsInvariants 逐条钉住不改的样子：非空参数、其他 input[] 项类型、
// 大整数与键序。这些字节必须原样出站。
func TestBuildPlanEmptyToolArgsInvariants(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "arguments 已是 {}",
			body: `{"input":[{"type":"function_call","call_id":"c1","name":"t","arguments":"{}"}]}`,
		},
		{
			name: "arguments 有内容",
			body: `{"input":[{"type":"function_call","name":"t","arguments":"{\"path\":\"/tmp\"}"}]}`,
		},
		{
			name: "其他项类型",
			body: `{"input":[` +
				`{"type":"message","role":"user","content":"p"},` +
				`{"type":"function_call_output","call_id":"c1","output":"ok"},` +
				`{"type":"custom_tool_call","call_id":"c2","name":"t","input":""},` +
				`{"type":"reasoning","summary":[]}` +
				`]}`,
		},
		{
			name: "大整数与键序",
			body: `{"z_flag":true,"input":[{"type":"function_call","arguments":"{}","call_id":"c1"}],` +
				`"a_big":9007199254740993,"m_nested":{"k1":1e21,"k2":"\u003cu\u003e"}}`,
		},
		{
			name: "arguments 非字符串",
			body: `{"input":[{"type":"function_call","name":"t","arguments":{}}]}`,
		},
		{
			name: "参数在 type 之前且为空",
			body: `{"input":[{"arguments":"","type":"function_call","name":"t"}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := BuildPlan(PlanInput{
				Client: responsesClient(tc.body),
				Target: Target{Provider: codexProvider("https://ollama.example.com")},
			})
			if err != nil {
				t.Fatalf("BuildPlan 失败: %v", err)
			}
			want := tc.body
			if tc.name == "参数在 type 之前且为空" {
				want = `{"input":[{"arguments":"{}","type":"function_call","name":"t"}]}`
			}
			if string(plan.Body) != want {
				t.Fatalf("正文应逐字节为\n want = %s\n  got = %s", want, plan.Body)
			}
		})
	}
}

// TestBuildPlanLeavesEmptyToolArgsOnNonResponsesTarget 钉住适用面：
// 目标线不是 openai-responses（chat / anthropic）时完全不碰正文。
func TestBuildPlanLeavesEmptyToolArgsOnNonResponsesTarget(t *testing.T) {
	const body = `{"input":[{"type":"function_call","name":"t","arguments":""}]}`
	cases := []struct {
		name     string
		client   ClientRequest
		provider convert.ProviderType
		protocol convert.WireProtocol
	}{
		{"原生 chat 线", chatClient(body), convert.ProviderOpenAICompatible, convert.ProtocolOpenAIChat},
		{"anthropic 线", responsesClient(body), convert.ProviderClaude, convert.ProtocolAnthropicMessages},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := claudeProvider("https://relay.example.com")
			provider.Type = tc.provider
			plan, err := BuildPlan(PlanInput{Client: tc.client, Target: Target{Provider: provider}})
			if err != nil {
				t.Fatalf("BuildPlan 失败: %v", err)
			}
			if plan.Protocol != tc.protocol {
				t.Fatalf("前置不成立：目标线 = %q，期望 %q", plan.Protocol, tc.protocol)
			}
			// 正文用 responses 的形状是故意的：它把「按目标线不适用」与「按正文形状不适用」两件事分开钉住。
			if string(plan.Body) != body {
				t.Fatalf("目标线非 responses 时正文不得被改：%s", plan.Body)
			}
		})
	}
}

// TestBuildPlanConversionPathBodyUnaffectedByEmptyArgsNormalization 钉住「跨协议路不重复处理」：
// 有转换计划时出站正文必须与编解码器的输出逐字节相同。
func TestBuildPlanConversionPathBodyUnaffectedByEmptyArgsNormalization(t *testing.T) {
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
