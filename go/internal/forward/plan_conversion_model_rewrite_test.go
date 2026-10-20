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
func buildConversionPlan(
	t *testing.T,
	client ClientRequest,
	providerType convert.ProviderType,
	shared *PlanShared,
) *Plan {
	t.Helper()
	provider := claudeProvider("https://relay.example.com")
	provider.Type = providerType
	provider.ModelRedirects = json.RawMessage(conversionRedirectRules)
	plan, err := BuildPlan(PlanInput{
		Client:            client,
		Target:            Target{Provider: provider},
		ConversionEnabled: true,
		Shared:            shared,
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
			plan := buildConversionPlan(t, tc.client, tc.provider, nil)
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

// TestConvertedBodyModelRewriteSkipsByteRewrite 钉住转换路**不再**做字节级整份改写：
// 模型名在编码**前**就位，不是编码完再对已转换正文做一次 rewriteModelField。
//
// 判据（不靠分配量）：直接拿枢纽请求手编一遍、只把 Model 换成目标名，若与计划产物逐字节
// 相同，则说明计划走的就是「先在树上就位再编码」这条路。旧做法两者也会相同（字节改写结果
// 一样），故额外钉住**共享的枢纽请求没有被改写**——这正是新结构下的真实危险：把重定向写进
// 共享树，下一个同协议候选就会拿到别人的模型名。
func TestConvertedBodyModelRewriteSkipsByteRewrite(t *testing.T) {
	client := chatClient(`{"model":"claude-sonnet-4-5-20250929","messages":[{"role":"user","content":"ping"}]}`)
	shared := NewPlanShared()
	plan := buildConversionPlan(t, client, convert.ProviderClaude, shared)

	// 共享产物里躺着的枢纽请求必须仍是客户端别名（重定向只写进浅拷贝）。
	if got := shared.decoded.request.Model; got != "claude-sonnet-4-5-20250929" {
		t.Fatalf("共享的枢纽请求被重定向改写了：model = %q（会把甲的模型名发给乙）", got)
	}

	// 手工复现「编码前就已是目标名」：与计划产物逐字节相同即证明改写发生在编码前。
	redirected := *shared.decoded.request
	redirected.Model = "glm-4.6"
	encoded, ok := convert.EncodeRequest(plan.Conversion.TargetProtocol, &redirected, convert.ConvertCtx{
		ClientFormat:   client.Format,
		TargetProto:    plan.Conversion.TargetProtocol,
		Model:          "glm-4.6",
		Stream:         shared.decoded.stream,
		ToWireToolName: convert.NormalizeToolName,
	})
	if !ok || encoded.Body == nil {
		t.Fatal("手工编码失败")
	}
	if string(plan.Body) != string(encoded.Body.MarshalCompact()) {
		t.Fatalf("转换路正文与「编码前就改好 model」不一致：\n got = %s\nwant = %s",
			plan.Body, encoded.Body.MarshalCompact())
	}
}

// TestConvertBodyRedirectNilKeepsSnapshotModel 钉住「无重定向时模型取正文快照里的值」。
//
// 编码用的模型名来自快照正文；重定向参数化后这条不得被顺手改掉，否则所有转换请求的模型名
// 都会退化成 client.Model（两者在真实请求里通常相同，掩盖问题）。
func TestConvertBodyRedirectNilKeepsSnapshotModel(t *testing.T) {
	client := chatClient(`{"model":"in-body-model","messages":[{"role":"user","content":"ping"}]}`)
	// client.Model 与正文里的 model 故意不同：编码结果必须跟正文走。
	client.Model = "header-side-model"
	plan := buildConversionPlanWithoutRedirect(t, client, convert.ProviderCodex)

	body := computeConversion(client, plan.Conversion, "")
	if body.failure != nil {
		t.Fatalf("转换失败: %v", body.failure)
	}
	var decoded map[string]any
	if err := json.Unmarshal(body.body, &decoded); err != nil {
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
