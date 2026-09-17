package guard

import (
	"encoding/json"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// 本文件钉住「网关注入」判定与转换层损失台账之间的那条事实链。
//
// 契约（与 guard/codex_session.go 的注释互相印证）：**客户端原文里存在一个非 null 的值**
// 才算「客户端提供了这个字段」。判据不能是「值能不能归一成会话标识」——两者在
// 「客户端给了值但值非法」时相反：归一规则把 `"short"` / `42` 这类值作废、正文被网关改写，
// 但那个字段确实是客户端声明的约束，跨线丢弃时该记一条损失（信息档）。
// 修前：它被登记成网关注入而整条略过，于是**少报**一条——与「每个转换请求凭空多一条」
// 正好相反方向的错，两者同源（判据取错了对象）。
// `null` 落在契约的边界上：它不承载约束，故视同未给（若算提供，每个请求会凭空多一条损失）。

// codexBodyWithCacheKey 造一条带指定 `prompt_cache_key` 原值的 Codex 正文。
func codexBodyWithCacheKey(value any) map[string]any {
	body := codexRequestBody()
	body["prompt_cache_key"] = value
	return body
}

// runSessionStepWithBody 走一次真实的会话步骤（守卫链），返回上下文与写回后的正文。
func runSessionStepWithBody(t *testing.T, body map[string]any) (*pctx.Context, map[string]any) {
	t.Helper()
	settings := fakeSettings{}
	settings.settings.codexCompletion = true
	completer := &fakeCodexCompleter{result: completeAll(codexTestSessionID)}
	factory, access := bodyFactory(t, body)
	deps := Deps{Settings: settings, Sessions: &fakeBinder{}, Body: factory, CodexCompletion: completer}

	ctx := newContext(t, nil, body)
	withAuth(ctx, 3, 7, "sk-x")

	if _, err := deps.sessionStep()(ctx); err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if access.current["prompt_cache_key"] != codexTestSessionID {
		t.Fatalf("补全语义不该变：正文里的 prompt_cache_key 应被改写为生成的会话标识，实际 %v", access.current)
	}
	return ctx, access.current
}

const codexTestSessionID = "01a0a2a1-c7ff-7747-81cc-4e27411e8938"

// TestSessionStepCacheKeyPresenceContractIsNonNull 钉住契约：原文里存在一个非 null 的值才算提供。
func TestSessionStepCacheKeyPresenceContractIsNonNull(t *testing.T) {
	cases := []struct {
		name         string
		body         map[string]any
		wantInjected bool
	}{
		{"客户端未给该键", codexRequestBody(), true},
		{"客户端给了非法短串", codexBodyWithCacheKey("short"), false},
		{"客户端给了数值", codexBodyWithCacheKey(42), false},
		{"客户端给了空串", codexBodyWithCacheKey(""), false},
		{"客户端给了合法值", codexBodyWithCacheKey("01a0a2a1-c7ff-7747-81cc-4e27411e8938"), false},
		// null 视同未给：它本来就没声明任何约束，登记为注入才不会给每个请求凭空加一条损失。
		{"客户端给了 null（视同未给）", codexBodyWithCacheKey(nil), true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, _ := runSessionStepWithBody(t, testCase.body)
			got := false
			for _, injected := range ctx.GatewayInjectedBodyFields() {
				if injected == "prompt_cache_key" {
					got = true
				}
			}
			if got != testCase.wantInjected {
				t.Fatalf("登记为网关注入 = %v，期望 %v（全表 %v）",
					got, testCase.wantInjected, ctx.GatewayInjectedBodyFields())
			}
		})
	}
}

// TestClientProvidedCacheKeySurvivesAsDeclaredConstraintInConversion 是事实链的末端断言：
// 守卫链的判定 → 真实转换器 → 损失台账里就是否有那条 prompt_cache_key。
//
// 为何要跨包钉在一起：两侧各自绿而中间断掉时没有任何用例会红——守卫链若登记成注入，
// 转换层就永远不会记这一条；转换层若不再认这个能力，登记与否也都看不出差别。
//
// 为何取 anthropic 做目标线：chat 线现已**原样转发**这个字段（守卫链补它是为了命中前缀缓存，
// 丢掉等于白补），声明与注入在出站正文里看不出差别；anthropic 无此概念，两者的差别恰好就是
// 那一条损失。
func TestClientProvidedCacheKeySurvivesAsDeclaredConstraintInConversion(t *testing.T) {
	cases := []struct {
		name      string
		body      map[string]any
		wantCount int
	}{
		{"非法但非空的原值仍是客户端声明的约束", codexBodyWithCacheKey("short"), 1},
		{"客户端没给过这个键：不得凭空记一条", codexRequestBody(), 0},
		// 契约的边界：`null` 不承载约束、视同未给，同样不得凭空记一条（否则每个请求 +1）。
		{"客户端给了 null：不得凭空记一条", codexBodyWithCacheKey(nil), 0},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx, rewritten := runSessionStepWithBody(t, testCase.body)

			payload, err := json.Marshal(rewritten)
			if err != nil {
				t.Fatalf("正文序列化失败: %v", err)
			}
			parsed, err := convert.ParseJSON(payload)
			if err != nil {
				t.Fatalf("正文解析失败: %v", err)
			}
			// 与 dataplane 的装配同形：pctx 的注入事实经 ClientRequest 传进 ConvertCtx。
			convertCtx := convert.ConvertCtx{
				ClientFormat:              convert.FormatResponse,
				TargetProto:               convert.ProtocolAnthropicMessages,
				Model:                     "m",
				ToWireToolName:            convert.NormalizeToolName,
				GatewayInjectedBodyFields: ctx.GatewayInjectedBodyFields(),
			}
			decoded, ok := convert.DecodeRequest(convert.ProtocolOpenAIResponses, parsed, convertCtx)
			if !ok {
				t.Fatal("responses 线必须能解码")
			}
			encoded, ok := convert.EncodeRequest(convert.ProtocolAnthropicMessages, decoded.Value, convertCtx)
			if !ok {
				t.Fatal("anthropic 线必须能编码")
			}
			entries := []convert.LossEntry{}
			for _, entry := range append(append([]convert.LossEntry{}, decoded.Loss.Entries...), encoded.Loss.Entries...) {
				if entry.Capability == convert.LossPromptCacheKey {
					entries = append(entries, entry)
				}
			}
			if len(entries) != testCase.wantCount {
				t.Fatalf("prompt_cache_key 损失应为 %d 条，实际 %d 条：%+v",
					testCase.wantCount, len(entries), entries)
			}
			if testCase.wantCount == 0 {
				return
			}
			if entries[0].Action != convert.LossDropped || entries[0].Detail != "anthropic_has_no_prompt_cache_key" {
				t.Fatalf("损失形状不符：%+v", entries[0])
			}
			// 档位必须是信息档：丢掉它本次作答不变（只是缓存路由失效），不该在列表徽章上占位。
			if severity := convert.LossSeverityOf(entries[0].Capability, entries[0].Action); severity != convert.SeverityInfo {
				t.Fatalf("prompt_cache_key 应为信息档，实际 %s", severity)
			}
		})
	}
}
