package forward

import (
	"errors"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// 本文件钉住「状态型字段跨协议线 fail-closed」。
//
// 背景：previous_response_id / conversation / store:true 是 Responses 线的服务端状态语义。
// 跨线转换没有任何承载位（目标编码器只读本线 passthrough），旧行为是静默丢弃——上游拿到一个
// 缺了历史的对话照常作答，客户端收到「形状正确但答非所问」的 200，比一个 400 难查得多。
//
// 判据边界（与实现同行，避免被后续改动无意放宽）：
//   - 只在**转换真的发生**时判定（同线直通原样承载，字段不丢）；
//   - 只对 OpenAI 两族客户端判定（错误体是 OpenAI 形状；其它方言里同名字段只是杂项键）；
//   - store:false 不算冲突（多数 Responses 客户端的默认值，且语义等价于「不额外落库」）。

const statefulResponsesBody = `{"model":"gpt-5","input":"继续刚才那个会话","previous_response_id":"resp_123"}`

func TestBuildPlanRejectsUnservableStatefulField(t *testing.T) {
	cases := []struct {
		name        string
		client      ClientRequest
		provider    convert.ProviderType
		wantField   string
		wantConvert bool
	}{
		{
			name:   "responses→chat：previous_response_id 无承载位",
			client: responsesClient(statefulResponsesBody), provider: convert.ProviderOpenAICompatible,
			wantField: "previous_response_id",
		},
		{
			name:   "responses→anthropic：同样 fail-closed",
			client: responsesClient(statefulResponsesBody), provider: convert.ProviderClaude,
			wantField: "previous_response_id",
		},
		{
			name:     "conversation 亦判",
			client:   responsesClient(`{"model":"gpt-5","input":"hi","conversation":"conv_1"}`),
			provider: convert.ProviderOpenAICompatible, wantField: "conversation",
		},
		{
			name:     "store:true 亦判",
			client:   responsesClient(`{"model":"gpt-5","input":"hi","store":true}`),
			provider: convert.ProviderOpenAICompatible, wantField: "store",
		},
		{
			// 同线直通不转换，字段原样发给上游 —— 这正是「不能无差别拦截」的边界。
			name:   "responses→responses（原生对）不拦",
			client: responsesClient(statefulResponsesBody), provider: convert.ProviderCodex,
		},
		{
			name:     "store:false 不拦（客户端默认值）",
			client:   responsesClient(`{"model":"gpt-5","input":"hi","store":false}`),
			provider: convert.ProviderOpenAICompatible,
		},
		{
			// 判据不跨方言扩张：chat 线上没有这些参数，同名键是杂项键，不该把请求打回。
			name:     "chat 线上的同名字段不拦",
			client:   chatClient(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"conversation":"conv_1"}`),
			provider: convert.ProviderClaude,
		},
		{
			name:     "claude 线上的同名字段不拦（错误体形状也不对）",
			client:   newClaudeRequest(`{"model":"claude-sonnet-4-5-20250929","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"previous_response_id":"resp_1"}`),
			provider: convert.ProviderOpenAICompatible,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := claudeProvider("https://relay.example.com")
			provider.Type = tc.provider

			plan, err := BuildPlan(PlanInput{
				Client:            tc.client,
				Target:            Target{Provider: provider},
				ConversionEnabled: true,
			})

			if tc.wantField == "" {
				if err != nil {
					t.Fatalf("不该拦下：%v", err)
				}
				if plan == nil {
					t.Fatal("计划为空")
				}
				return
			}

			if err == nil {
				t.Fatal("应 fail-closed，实际拿到了可用计划")
			}
			if !errors.Is(err, ErrStatefulConversion) {
				t.Fatalf("错误必须是 ErrStatefulConversion：%v", err)
			}
			var rejected *StatefulConversionError
			if !errors.As(err, &rejected) {
				t.Fatalf("错误必须带字段名：%v", err)
			}
			if rejected.Field != tc.wantField {
				t.Fatalf("字段名应为 %s，收到 %s", tc.wantField, rejected.Field)
			}
		})
	}
}

// TestBuildPlanStatefulCheckNeedsConversion 钉住「判据只在转换真的发生时生效」。
//
// 关掉转换开关时跨线供应商本来就不会被选中（选路层按三态过滤），此时 BuildPlan 不转正文；
// 用一个非 OpenAI 方言的客户端体（无状态字段）验证：不因「供应商跨线」而误判。
func TestBuildPlanStatefulCheckNeedsConversion(t *testing.T) {
	provider := claudeProvider("https://relay.example.com")
	provider.Type = convert.ProviderOpenAICompatible

	plan, err := BuildPlan(PlanInput{
		Client:            chatClient(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`),
		Target:            Target{Provider: provider},
		ConversionEnabled: false,
	})
	if err != nil {
		t.Fatalf("不该失败：%v", err)
	}
	if plan.Conversion != nil {
		t.Fatal("转换开关关闭时不该有转换计划")
	}
}
