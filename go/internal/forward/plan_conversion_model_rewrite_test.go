package forward

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// 本文件钉住「转换路改 model 在树上原位做，不再整份复制正文」这一分工。

const conversionRedirectRules = `[{"matchType":"exact","source":"claude-sonnet-4-5-20250929","target":"glm-4.6"}]`

// buildConversionPlan 编译一次「跨线转换 + 模型重定向」的计划，并断言转换真的生效。
func buildConversionPlan(t *testing.T, client ClientRequest, providerType convert.ProviderType) *Plan {
	t.Helper()
	provider := claudeProvider("https://relay.example.com")
	provider.Type = providerType
	provider.ModelRedirects = json.RawMessage(conversionRedirectRules)
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
	return plan
}

// TestConvertedBodyModelRewriteKeepsKeyOrder 钉住转换路出站正文的两个要求：
// 顶层 model 是重定向目标，且**顶层键序与该线不带重定向时完全相同**。
//
// 为何要分方言逐个钉：三个编码器取顶层 model 的次序不同——anthropic / responses 是
// firstString(request.Model, ctx.Model)，chat 是反序 firstString(ctx.Model, request.Model)。
// 只改树或只改 ctx 都会剩下一条线发出客户端别名，本表就是那条分叉的钉子。
//
// 键序的意义：转换路的出站正文由编码器自建（键序由编码器决定，与客户端正文次序无关）。
// 改写在树上是原位替换，故不得把编码器的键序改掉——旧做法（编码后再 rewriteModelField）
// 靠字节替换也能保住键序，故这条是防回归而非新增语义。
func TestConvertedBodyModelRewriteKeepsKeyOrder(t *testing.T) {
	const claudeBody = `{"max_tokens":1024,"model":"claude-sonnet-4-5-20250929","messages":[{"role":"user","content":"ping"}]}`
	const chatBody = `{"max_tokens":1024,"model":"claude-sonnet-4-5-20250929","messages":[{"role":"user","content":"ping"}]}`
	const responsesBody = `{"max_output_tokens":1024,"model":"claude-sonnet-4-5-20250929","input":"ping"}`

	cases := []struct {
		name        string
		client      ClientRequest
		provider    convert.ProviderType
		description string
	}{
		{"claude→chat", newClaudeRequest(claudeBody), convert.ProviderOpenAICompatible,
			"chat 编码器 ctx.Model 优先"},
		{"claude→responses", newClaudeRequest(claudeBody), convert.ProviderCodex,
			"responses 编码器 request.Model 优先"},
		{"chat→claude", chatClient(chatBody), convert.ProviderClaude,
			"anthropic 编码器 request.Model 优先"},
		{"responses→claude", responsesClient(responsesBody), convert.ProviderClaude,
			"anthropic 编码器 request.Model 优先"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := buildConversionPlan(t, tc.client, tc.provider)
			if plan.Redirect == nil || plan.Redirect.Target != "glm-4.6" {
				t.Fatalf("重定向事实应留在计划上（供落链/计费）: %+v", plan.Redirect)
			}

			var decoded map[string]any
			if err := json.Unmarshal(plan.Body, &decoded); err != nil {
				t.Fatalf("出站正文不是 JSON: %v", err)
			}
			if decoded["model"] != "glm-4.6" {
				t.Fatalf("出站正文顶层 model = %v，期望重定向目标 glm-4.6（%s）",
					decoded["model"], tc.description)
			}

			// 键序：同一条线不配重定向时编码器给出的顶层键序，这里必须逐个相同。
			want := topLevelKeys(t, buildConversionPlanWithoutRedirect(t, tc.client, tc.provider).Body)
			got := topLevelKeys(t, plan.Body)
			if strings.Join(got, ",") != strings.Join(want, ",") {
				t.Fatalf("改写改变了顶层键序：got=%v want=%v", got, want)
			}
		})
	}
}

// topLevelKeys 按正文里的出现次序取顶层键名。
func topLevelKeys(t *testing.T, body []byte) []string {
	t.Helper()
	value, err := convert.ParseJSON(body)
	if err != nil {
		t.Fatalf("正文不是 JSON: %v", err)
	}
	keys := make([]string, 0, len(value.Members()))
	for _, member := range value.Members() {
		keys = append(keys, member.Key)
	}
	return keys
}

// TestConvertedBodyModelRewriteSkipsByteRewrite 钉住转换路**不再**做字节级整份改写。
//
// 判据：把同一份正文按转换路重算一遍，但让 redirect 目标名与客户端模型名等长——
// rewriteModelField 命中时必然复制一份正文（见其注释），而树上原位 Set 不复制。
// 故按「分配量之差」间接证伪；这里取更稳的等价断言：convertBody 拿到的 value 树
// 在返回后已带目标模型名（即改写发生在树里，而不是编码后又在字节上改一次），
// 且出站正文与「树里已是目标名、不带 redirect 再编码一次」逐字节相同。
func TestConvertedBodyModelRewriteSkipsByteRewrite(t *testing.T) {
	client := chatClient(`{"model":"claude-sonnet-4-5-20250929","messages":[{"role":"user","content":"ping"}]}`)
	plan := buildConversionPlan(t, client, convert.ProviderClaude)

	// 手工复现「只在树上改、不带 redirect 编码」：若两者逐字节相同，说明计划走的正是这条路。
	parsed, err := convert.ParseJSON(client.Body)
	if err != nil {
		t.Fatalf("解析客户端正文失败: %v", err)
	}
	parsed.Set("model", convert.NewString("glm-4.6"))
	expected, _, _, err := convertBody(client, plan.Conversion, parsed, nil)
	if err != nil {
		t.Fatalf("convertBody 失败: %v", err)
	}
	if string(plan.Body) != string(expected) {
		t.Fatalf("转换路正文与「树上原位改 model 再编码」不一致：\n got = %s\nwant = %s", plan.Body, expected)
	}
}

// TestConvertBodyRedirectNilKeepsSnapshotModel 钉住「无重定向时模型取正文快照里的值」。
//
// convertBody 的注释承诺编码用的模型名来自快照正文；加了 redirect 参数后这条不得被顺手改掉，
// 否则所有转换请求的模型名都会退化成 client.Model（两者在真实请求里通常相同，掩盖问题）。
func TestConvertBodyRedirectNilKeepsSnapshotModel(t *testing.T) {
	client := chatClient(`{"model":"in-body-model","messages":[{"role":"user","content":"ping"}]}`)
	// client.Model 与正文里的 model 故意不同：编码结果必须跟正文走。
	client.Model = "header-side-model"
	plan := buildConversionPlanWithoutRedirect(t, client, convert.ProviderCodex)

	parsed, err := convert.ParseJSON(client.Body)
	if err != nil {
		t.Fatalf("解析客户端正文失败: %v", err)
	}
	body, _, _, err := convertBody(client, plan.Conversion, parsed, nil)
	if err != nil {
		t.Fatalf("convertBody 失败: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("正文不是 JSON: %v", err)
	}
	if decoded["model"] != "in-body-model" {
		t.Fatalf("无重定向时模型应取正文快照 %q，实际 %v", "in-body-model", decoded["model"])
	}
}

// buildConversionPlanWithoutRedirect 编译一个不配重定向的转换计划（对照组：编码器的原生键序）。
func buildConversionPlanWithoutRedirect(t *testing.T, client ClientRequest, providerType convert.ProviderType) *Plan {
	t.Helper()
	provider := claudeProvider("https://relay.example.com")
	provider.Type = providerType
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
	return plan
}

// TestNativePathModelRewriteStaysByteLevel 钉住 native 路（无转换）仍走字节级改写。
//
// 为什么不能统一成树路：native 路出站的就是客户端原文字节，逐字稳定直接决定上游前缀缓存
// 的命中率（见 rewriteModelField 的注释）。树路会经编码器重建对象、重排键序、抹掉空白与
// 转义形态。故这里用「带空白 + 带转义写法」的正文钉住：除 model 值外逐字不变。
func TestNativePathModelRewriteStaysByteLevel(t *testing.T) {
	const body = `{ "model" : "claude-sonnet-4-5-20250929", "messages" : [ {"role":"user","content":"a\u0041b"} ] }`
	const want = `{ "model" : "glm-4.6", "messages" : [ {"role":"user","content":"a\u0041b"} ] }`

	provider := claudeProvider("https://relay.example.com")
	provider.Type = convert.ProviderOpenAICompatible
	provider.ModelRedirects = json.RawMessage(conversionRedirectRules)

	plan, err := BuildPlan(PlanInput{Client: newClaudeRequest(body), Target: Target{Provider: provider}})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	if plan.Conversion != nil {
		t.Fatalf("本用例不转换（原生直通），实际 conversion=%+v", plan.Conversion)
	}
	if string(plan.Body) != want {
		t.Fatalf("native 路正文未逐字保留（应只换 model 值）：\n got = %q\nwant = %q", plan.Body, want)
	}
}

// BenchmarkBuildPlanConversionLargeBodyWithRedirect 量「转换 + 重定向」的每 attempt 成本。
//
// 对照 BenchmarkBuildPlanConversionLargeBody（同样 1.9MB、无重定向，43.83MB/op）：
// 旧实现会在这条路上多出一份全量正文拷贝（rewriteModelField 的命中路径，约 +1.9MB/op）。
// 现在改写落在已解析的树上，故两者的分配量应基本相等。
func BenchmarkBuildPlanConversionLargeBodyWithRedirect(b *testing.B) {
	client := responsesClient(string(planBenchBody(1900 << 10)))
	// 规则按 client.Model 匹配（resolveModelRedirect 的入参就是它），不是正文里的 model。
	provider := claudeProvider("https://relay.example.com")
	provider.ModelRedirects = json.RawMessage(
		`[{"matchType":"exact","source":"claude-sonnet-4-5-20250929","target":"glm-4.6"}]`)
	input := PlanInput{Client: client, Target: Target{Provider: provider}, ConversionEnabled: true}

	b.ReportAllocs()
	b.SetBytes(int64(len(client.Body)))
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		plan, err := BuildPlan(input)
		if err != nil {
			b.Fatalf("BuildPlan 失败: %v", err)
		}
		if plan.Conversion == nil || plan.ConversionFallback {
			b.Fatal("前置不成立：本基准需转换成功")
		}
		if plan.Redirect == nil {
			b.Fatal("前置不成立：本基准需命中重定向")
		}
	}
}
