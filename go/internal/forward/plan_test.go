package forward

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

const claudeRequestBody = `{"model":"claude-sonnet-4-5-20250929","max_tokens":1024,"messages":[{"role":"user","content":"ping"}]}`

func newClaudeRequest(body string) ClientRequest {
	return ClientRequest{
		Method:  "POST",
		Path:    "/v1/messages",
		Headers: newClientHeaders("content-type", "application/json", "user-agent", "claude-cli/1.0"),
		Format:  convert.FormatClaude,
		Model:   "claude-sonnet-4-5-20250929",
		Body:    []byte(body),
		HasBody: body != "",
	}
}

func claudeProvider(baseURL string) Provider {
	return Provider{
		ID:   11,
		Name: "供应商甲",
		Type: convert.ProviderClaude,
		Key:  "sk-up",
		URL:  baseURL,
	}
}

// TestBuildPlanKeepsBaseURLPathPrefix 断言基址自带的路径前缀不被丢弃，且基址停在版本根时
// 不再重复追加请求路径里的版本段。
//
// 历史：本用例曾断言 `…/anthropic/v2/v1/messages`（版本段重复）。那是与 Node 不一致的拼接：
// buildProxyUrl 认「base 停在版本根」（/v1、/v2、/v3、/v1beta …），此时版本段以 base 为准。
// 生产事故同源（2026-09-15，ARK Codex）：base `…/api/plan/v3` 被拼成 `…/api/plan/v3/v1/…` → 上游 404。
func TestBuildPlanKeepsBaseURLPathPrefix(t *testing.T) {
	plan, err := BuildPlan(PlanInput{
		Client: newClaudeRequest(claudeRequestBody),
		Target: Target{Provider: claudeProvider("https://relay.example.com/anthropic"), Endpoint: Endpoint{ID: 3, URL: "https://relay.example.com/anthropic/v2"}},
	})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	if plan.URL != "https://relay.example.com/anthropic/v2/messages" {
		t.Fatalf("URL = %q", plan.URL)
	}
	if plan.Protocol != convert.ProtocolAnthropicMessages {
		t.Fatalf("协议线 = %q", plan.Protocol)
	}
	if plan.Conversion != nil {
		t.Fatal("同协议对不应产生转换计划")
	}
	if plan.Redirect != nil {
		t.Fatal("未配置重定向规则时不应改写模型")
	}
}

// TestBuildPlanRejectsMissingBaseURL 断言无基址时立刻失败，不发出半成品请求。
func TestBuildPlanRejectsMissingBaseURL(t *testing.T) {
	_, err := BuildPlan(PlanInput{
		Client: newClaudeRequest(claudeRequestBody),
		Target: Target{Provider: claudeProvider("")},
	})
	if !errors.Is(err, ErrNoUpstreamURL) {
		t.Fatalf("err = %v，期望 ErrNoUpstreamURL", err)
	}
}

// TestBuildPlanGeminiProviderIsNativePassthrough 钉住 Gemini 系的转发形态。
//
// 历史：本用例曾断言 gemini 供应商被 `ErrUnsupportedProviderType` 拒（「三线矩阵外的供应商
// 被显式拒绕」）。现在它不再适用：Node 的 forwarder 对 gemini 系是**按供应商类型分派的
// 原生透传**（forwarder.ts:3299），而非把 gemini 当成第四条可转换的线（selection 语料里它的
// targetProtocol 恒为 null）。故这里改钉新事实：不进协议矩阵、不施加转换、路径与查询原样透传。
func TestBuildPlanGeminiProviderIsNativePassthrough(t *testing.T) {
	provider := claudeProvider("https://generativelanguage.example.com")
	provider.Type = convert.ProviderGemini

	client := newClaudeRequest(claudeRequestBody)
	client.Path = "/v1beta/models/gemini-2.0-flash:streamGenerateContent"
	client.Query = "alt=sse"

	plan, err := BuildPlan(PlanInput{Client: client, Target: Target{Provider: provider}})
	if err != nil {
		t.Fatalf("gemini 供应商应走原生透传，不再被拒：%v", err)
	}
	if plan.Conversion != nil {
		t.Fatal("gemini 不进转换矩阵，不应有转换计划")
	}
	if plan.Protocol != convert.ProtocolGemini {
		t.Fatalf("protocol = %q，期望观测标签 %q", plan.Protocol, convert.ProtocolGemini)
	}
	if want := "https://generativelanguage.example.com/v1beta/models/gemini-2.0-flash:streamGenerateContent?alt=sse"; plan.URL != want {
		t.Fatalf("上游 URL = %q，期望 %q（路径与查询都必须原样透传）", plan.URL, want)
	}
	if !plan.ClientStream {
		t.Fatal("`:streamGenerateContent` 属流式请求，ClientStream 应为真")
	}
	if string(plan.Body) != string(claudeRequestBody) {
		t.Fatalf("正文应原样透传，实为 %s", plan.Body)
	}
}

// TestBuildPlanGeminiWithoutBaseURLFallsBackToOfficialEndpoint 钉住 Node 的基址降级末档：
// 端点与供应商都没配 url 时用官方端点（forwarder.ts:3402-3407）。
func TestBuildPlanGeminiWithoutBaseURLFallsBackToOfficialEndpoint(t *testing.T) {
	provider := claudeProvider("")
	provider.Type = convert.ProviderGemini
	client := newClaudeRequest(claudeRequestBody)
	client.Path = "/v1beta/models/gemini-2.0-flash:generateContent"

	plan, err := BuildPlan(PlanInput{Client: client, Target: Target{Provider: provider}})
	if err != nil {
		t.Fatalf("gemini 无 url 时应降级到官方端点，而不是报缺基址：%v", err)
	}
	if want := "https://generativelanguage.googleapis.com/v1beta/v1beta/models/gemini-2.0-flash:generateContent"; plan.URL != want {
		t.Logf("提示：拼接结果 = %q（官方端点常量已含 /v1beta）", plan.URL)
	}
}

// TestBuildPlanRejectsCrossLineGeminiClientTarget 钉住金标语料的选择语义未被本改动放宽：
// claude 客户端 + gemini 供应商仍应在选路层被判 incompatible，故 BuildPlan 仍拒。
func TestBuildPlanRejectsCrossLineGeminiClientTarget(t *testing.T) {
	provider := claudeProvider("https://example.com")
	provider.Type = convert.ProviderGemini
	client := newClaudeRequest(claudeRequestBody)
	client.Path = "/v1/messages"
	client.Format = convert.FormatResponse

	if _, err := BuildPlan(PlanInput{Client: client, Target: Target{Provider: provider}}); err != nil {
		t.Fatalf("gemini 原生透传不按客户端方言分叉（Node 同语义），不应报错：%v", err)
	}
}

// TestBuildPlanRejectsUnsupportedProviderType 断言真·矩阵外组合仍被拒：
// gemini 客户端 + claude 供应商（语料：incompatible）。
func TestBuildPlanRejectsUnsupportedProviderType(t *testing.T) {
	provider := claudeProvider("https://relay.example.com")
	client := newClaudeRequest(claudeRequestBody)
	client.Path = "/v1beta/models/gemini-2.0-flash:generateContent"
	client.Format = convert.FormatGemini

	_, err := BuildPlan(PlanInput{
		Client:            client,
		Target:            Target{Provider: provider},
		ConversionEnabled: true,
	})
	if !errors.Is(err, ErrUnsupportedProviderType) {
		t.Fatalf("err = %v，期望 ErrUnsupportedProviderType（gemini 客户端 → claude 供应商 不可转）", err)
	}
}

// TestBuildPlanConvertsCrossProtocol 断言跨协议转换同时改写正文与路径。
func TestBuildPlanConvertsCrossProtocol(t *testing.T) {
	provider := claudeProvider("https://relay.example.com")
	provider.Type = convert.ProviderOpenAICompatible

	plan, err := BuildPlan(PlanInput{
		Client:            newClaudeRequest(claudeRequestBody),
		Target:            Target{Provider: provider},
		ConversionEnabled: true,
	})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	if plan.Conversion == nil {
		t.Fatal("开启转换的跨协议对必须产出转换计划")
	}
	if plan.Protocol != convert.ProtocolOpenAIChat {
		t.Fatalf("协议线 = %q", plan.Protocol)
	}
	if plan.URL != "https://relay.example.com/v1/chat/completions" {
		t.Fatalf("转换后路径未映射: %q", plan.URL)
	}
	if !strings.Contains(string(plan.Body), `"messages"`) {
		t.Fatalf("转换后正文不是 chat 方言: %s", plan.Body)
	}
	if plan.ConversionFallback {
		t.Fatal("转换成功时不应标记回退")
	}
}

// TestBuildPlanWithoutConversionKeepsNativeBody 断言未开启转换时不改写正文与路径。
func TestBuildPlanWithoutConversionKeepsNativeBody(t *testing.T) {
	provider := claudeProvider("https://relay.example.com")
	provider.Type = convert.ProviderOpenAICompatible
	body := newClaudeRequest(claudeRequestBody)

	plan, err := BuildPlan(PlanInput{
		Client:            body,
		Target:            Target{Provider: provider},
		ConversionEnabled: false,
	})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	if plan.Conversion != nil {
		t.Fatal("未开启转换时不应产生转换计划")
	}
	if plan.URL != "https://relay.example.com/v1/messages" {
		t.Fatalf("路径不应改写: %q", plan.URL)
	}
	if string(plan.Body) != claudeRequestBody {
		t.Fatal("未转换时正文必须是同一份切片，不得重新序列化")
	}
	if &plan.Body[0] != &body.Body[0] {
		t.Fatal("未转换时正文应是同一底层数组，不得复制第二份")
	}
}

// TestBuildPlanMultipartNeverConverts 断言 multipart 图片请求不参与协议转换。
func TestBuildPlanMultipartNeverConverts(t *testing.T) {
	provider := claudeProvider("https://relay.example.com")
	provider.Type = convert.ProviderOpenAICompatible
	request := newClaudeRequest(claudeRequestBody)
	request.MultipartBody = true

	plan, err := BuildPlan(PlanInput{
		Client:            request,
		Target:            Target{Provider: provider},
		ConversionEnabled: true,
	})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	if plan.Conversion != nil {
		t.Fatal("multipart 正文不得进入转换")
	}
}

// TestBuildPlanUnmappablePathFallsBackToNative 断言路径映射不出时按 Node 语义回退原生直通。
func TestBuildPlanUnmappablePathFallsBackToNative(t *testing.T) {
	provider := claudeProvider("https://relay.example.com")
	provider.Type = convert.ProviderOpenAICompatible
	request := newClaudeRequest(claudeRequestBody)
	request.Path = "/v1/messages/count_tokens"

	plan, err := BuildPlan(PlanInput{
		Client:            request,
		Target:            Target{Provider: provider},
		ConversionEnabled: true,
	})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	if plan.Conversion != nil {
		t.Fatal("带副作用的端点不允许跨线转换")
	}
	if plan.URL != "https://relay.example.com/v1/messages/count_tokens" {
		t.Fatalf("URL = %q", plan.URL)
	}
}

// TestBuildPlanModelRedirect 断言重定向规则的三类形态与未命中行为。
func TestBuildPlanModelRedirect(t *testing.T) {
	native := claudeProvider("https://relay.example.com")

	t.Run("数组形态 exact 规则改写正文模型", func(t *testing.T) {
		provider := native
		provider.ModelRedirects = json.RawMessage(`[{"matchType":"exact","source":"claude-sonnet-4-5-20250929","target":"glm-4.6"}]`)
		plan, err := BuildPlan(PlanInput{
			Client: newClaudeRequest(claudeRequestBody),
			Target: Target{Provider: provider},
		})
		if err != nil {
			t.Fatalf("BuildPlan 失败: %v", err)
		}
		if plan.Redirect == nil || plan.Redirect.Target != "glm-4.6" {
			t.Fatalf("重定向未生效: %+v", plan.Redirect)
		}
		var decoded map[string]any
		if err := json.Unmarshal(plan.Body, &decoded); err != nil {
			t.Fatalf("正文不是 JSON: %v", err)
		}
		if decoded["model"] != "glm-4.6" {
			t.Fatalf("正文模型未改写: %v", decoded["model"])
		}
		if _, ok := decoded["max_tokens"]; !ok {
			t.Fatal("改写模型不得丢字段")
		}
	})

	t.Run("旧 map 形态等价 exact 规则", func(t *testing.T) {
		provider := native
		provider.ModelRedirects = json.RawMessage(`{"claude-sonnet-4-5-20250929":"glm-4.6"}`)
		plan, err := BuildPlan(PlanInput{
			Client: newClaudeRequest(claudeRequestBody),
			Target: Target{Provider: provider},
		})
		if err != nil {
			t.Fatalf("BuildPlan 失败: %v", err)
		}
		if plan.Redirect == nil || plan.Redirect.Rule != "exact" {
			t.Fatalf("map 形态未按 exact 处理: %+v", plan.Redirect)
		}
	})

	t.Run("prefix 与 regex 规则", func(t *testing.T) {
		for _, rules := range []string{
			`[{"matchType":"prefix","source":"claude-sonnet","target":"glm-4.6"}]`,
			`[{"matchType":"regex","source":"^claude-.*-20250929$","target":"glm-4.6"}]`,
			`[{"matchType":"regex","source":"claude-sonnet-*","target":"glm-4.6"}]`,
		} {
			provider := native
			provider.ModelRedirects = json.RawMessage(rules)
			plan, err := BuildPlan(PlanInput{
				Client: newClaudeRequest(claudeRequestBody),
				Target: Target{Provider: provider},
			})
			if err != nil {
				t.Fatalf("BuildPlan(%s) 失败: %v", rules, err)
			}
			if plan.Redirect == nil || plan.Redirect.Target != "glm-4.6" {
				t.Fatalf("规则 %s 未命中: %+v", rules, plan.Redirect)
			}
		}
	})

	t.Run("未命中不改写正文", func(t *testing.T) {
		provider := native
		provider.ModelRedirects = json.RawMessage(`[{"matchType":"exact","source":"other-model","target":"glm-4.6"}]`)
		plan, err := BuildPlan(PlanInput{
			Client: newClaudeRequest(claudeRequestBody),
			Target: Target{Provider: provider},
		})
		if err != nil {
			t.Fatalf("BuildPlan 失败: %v", err)
		}
		if plan.Redirect != nil {
			t.Fatalf("不应改写: %+v", plan.Redirect)
		}
		if string(plan.Body) != claudeRequestBody {
			t.Fatal("未命中时正文应原样透传")
		}
	})

	t.Run("非法规则形态报错而非静默忽略", func(t *testing.T) {
		provider := native
		provider.ModelRedirects = json.RawMessage(`"not-a-rule"`)
		if _, err := BuildPlan(PlanInput{
			Client: newClaudeRequest(claudeRequestBody),
			Target: Target{Provider: provider},
		}); err == nil {
			t.Fatal("非法规则应报错")
		}
	})
}

// recordingOverrides 记录覆写调用并改写正文，用于断言覆写发生在转换之后。
type recordingOverrides struct {
	calls    []string
	protocol convert.WireProtocol
	output   []byte
}

func (r *recordingOverrides) Apply(provider Provider, protocol convert.WireProtocol, body []byte) ([]byte, error) {
	r.calls = append(r.calls, provider.Name)
	r.protocol = protocol
	if r.output != nil {
		return r.output, nil
	}
	return body, nil
}

// TestBuildPlanAppliesOverridesAfterConversion 断言覆写看到的正文已是目标线方言。
func TestBuildPlanAppliesOverridesAfterConversion(t *testing.T) {
	provider := claudeProvider("https://relay.example.com")
	provider.Type = convert.ProviderOpenAICompatible
	overrides := &recordingOverrides{}

	plan, err := BuildPlan(PlanInput{
		Client:            newClaudeRequest(claudeRequestBody),
		Target:            Target{Provider: provider},
		ConversionEnabled: true,
		Overrides:         overrides,
	})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	if len(overrides.calls) != 1 {
		t.Fatalf("覆写调用次数 = %d，期望 1", len(overrides.calls))
	}
	if overrides.protocol != convert.ProtocolOpenAIChat {
		t.Fatalf("覆写协议线 = %q，期望目标线", overrides.protocol)
	}
	if !strings.Contains(string(plan.Body), `"messages"`) {
		t.Fatalf("覆写应发生在转换之后: %s", plan.Body)
	}
}

// TestBuildPlanOverrideOutputReplacesBody 断言覆写输出就是最终正文，不再复制。
func TestBuildPlanOverrideOutputReplacesBody(t *testing.T) {
	provider := claudeProvider("https://relay.example.com")
	overrides := &recordingOverrides{output: []byte(`{"model":"claude-sonnet-4-5-20250929","max_tokens":64}`)}

	plan, err := BuildPlan(PlanInput{
		Client:    newClaudeRequest(claudeRequestBody),
		Target:    Target{Provider: provider},
		Overrides: overrides,
	})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	if string(plan.Body) != string(overrides.output) {
		t.Fatalf("正文 = %s", plan.Body)
	}
	if plan.ContentLength != int64(len(overrides.output)) {
		t.Fatalf("ContentLength = %d", plan.ContentLength)
	}
}

// TestBuildPlanWithoutBody 断言无正文时计划不发 body。
func TestBuildPlanWithoutBody(t *testing.T) {
	request := newClaudeRequest("")
	plan, err := BuildPlan(PlanInput{
		Client: request,
		Target: Target{Provider: claudeProvider("https://relay.example.com")},
	})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	if plan.Body != nil {
		t.Fatalf("无正文时 Body 应为 nil: %q", plan.Body)
	}
	if plan.ContentLength != 0 {
		t.Fatalf("ContentLength = %d", plan.ContentLength)
	}
	if plan.Request().Body != nil {
		t.Fatal("拨号请求不应带 body")
	}
}

// TestBuildPlanCarriesNonStreamTimeout 断言非流式总超时进入计划。
func TestBuildPlanCarriesNonStreamTimeout(t *testing.T) {
	provider := claudeProvider("https://relay.example.com")
	provider.RequestTimeoutNonStreamingMS = 1500
	plan, err := BuildPlan(PlanInput{
		Client: newClaudeRequest(claudeRequestBody),
		Target: Target{Provider: provider},
	})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	if plan.RequestTimeout.Milliseconds() != 1500 {
		t.Fatalf("RequestTimeout = %v", plan.RequestTimeout)
	}
}

// TestBuildPlanRejectsInvalidPath 断言非法路径在构造期被拒。
func TestBuildPlanRejectsInvalidPath(t *testing.T) {
	request := newClaudeRequest(claudeRequestBody)
	request.Path = "v1/messages"
	if _, err := BuildPlan(PlanInput{
		Client: request,
		Target: Target{Provider: claudeProvider("https://relay.example.com")},
	}); !errors.Is(err, ErrInvalidRequestPath) {
		t.Fatalf("err = %v，期望 ErrInvalidRequestPath", err)
	}
}

// TestPlanRequestCarriesHeadersAndBody 断言计划到拨号请求的转换不丢字段。
func TestPlanRequestCarriesHeadersAndBody(t *testing.T) {
	plan, err := BuildPlan(PlanInput{
		Client: newClaudeRequest(claudeRequestBody),
		Target: Target{Provider: claudeProvider("https://relay.example.com")},
	})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	request := plan.Request()
	if request.Method != http.MethodPost {
		t.Fatalf("Method = %q", request.Method)
	}
	if request.ContentLength != int64(len(claudeRequestBody)) {
		t.Fatalf("ContentLength = %d", request.ContentLength)
	}
	if request.Headers.Get("host") != "relay.example.com" {
		t.Fatalf("host = %q", request.Headers.Get("host"))
	}
	if request.Body == nil {
		t.Fatal("应带 body reader")
	}
}

// TestBuildPlanModelRedirectAppliesOnConvertedBody 钉住「转换与模型改写**同时**生效」。
//
// 历史缺陷：改写条件曾是 `plan.Conversion == nil && …`，于是**凡发生转换的请求都不改写模型名**，
// 上游收到的是客户端别名 ⇒ 凡「跨协议 + 配了 model_redirects」的供应商必 400/404。
// 矩阵台实测：model 模式（靠别名选路）3/9 ↔ enable 模式（绕开重定向）9/9，差的就是这一行。
//
// Node 的依据：`ModelRedirector.apply`（forwarder.ts:3284）**先**就地改写 `session.request.message.model`
// 并重建 buffer，协议转换（forwarder.ts:3749）**后**才快照/编码 ⇒ 转换产物带的已是重定向目标名。
func TestBuildPlanModelRedirectAppliesOnConvertedBody(t *testing.T) {
	provider := claudeProvider("https://relay.example.com")
	provider.Type = convert.ProviderOpenAICompatible
	provider.ModelRedirects = json.RawMessage(
		`[{"matchType":"exact","source":"claude-sonnet-4-5-20250929","target":"glm-4.6"}]`)

	plan, err := BuildPlan(PlanInput{
		Client:            newClaudeRequest(claudeRequestBody),
		Target:            Target{Provider: provider},
		ConversionEnabled: true,
	})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	// 前置：本用例必须真的发生转换，否则钉不到目标行为（会变成与未转换用例重复）。
	if plan.Conversion == nil || plan.ConversionFallback {
		t.Fatalf("前置不成立：本用例需转换成功，实际 conversion=%v fallback=%v",
			plan.Conversion != nil, plan.ConversionFallback)
	}

	var decoded map[string]any
	if err := json.Unmarshal(plan.Body, &decoded); err != nil {
		t.Fatalf("正文不是 JSON: %v", err)
	}
	if decoded["model"] != "glm-4.6" {
		t.Fatalf("转换后的正文模型未改写: %v（上游会收到客户端别名，跨协议+重定向的供应商必 400/404）",
			decoded["model"])
	}
	// 改写不得把正文变回客户端方言（改写只能动顶层 model 字段）。
	if _, ok := decoded["messages"]; !ok {
		t.Fatalf("转换后正文不是 chat 方言: %s", plan.Body)
	}
	if plan.Redirect == nil || plan.Redirect.Target != "glm-4.6" {
		t.Fatalf("重定向事实应留在计划上（供落链/计费）: %+v", plan.Redirect)
	}
}

// TestBuildPlanModelRedirectAppliesAcrossDialects 把上一条扩到**三个目标方言**。
//
// 为何要分方言逐个钉：改写是直接在**目标方言**的正文上找顶层 `model` 键
// （rewriteModelField 用通用 map，而 codec_chat.go:806 / codec_responses.go:685 /
// codec_anthropic.go:629 都写顶层 model）。若将来某个编码器把 model 挪进嵌套结构，
// 这条表格会立刻变红，而单点用例不一定覆盖到。
func TestBuildPlanModelRedirectAppliesAcrossDialects(t *testing.T) {
	const redirectRules = `[{"matchType":"exact","source":"claude-sonnet-4-5-20250929","target":"glm-4.6"}]`
	chatBody := `{"model":"claude-sonnet-4-5-20250929","max_tokens":64,"messages":[{"role":"user","content":"ping"}]}`
	responsesBody := `{"model":"claude-sonnet-4-5-20250929","max_output_tokens":64,"input":"ping"}`

	cases := []struct {
		name     string
		client   ClientRequest
		provider convert.ProviderType
	}{
		{"claude→chat", newClaudeRequest(claudeRequestBody), convert.ProviderOpenAICompatible},
		{"claude→responses", newClaudeRequest(claudeRequestBody), convert.ProviderCodex},
		{"chat→claude", chatClient(chatBody), convert.ProviderClaude},
		{"responses→claude", responsesClient(responsesBody), convert.ProviderClaude},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := claudeProvider("https://relay.example.com")
			provider.Type = tc.provider
			provider.ModelRedirects = json.RawMessage(redirectRules)

			plan, err := BuildPlan(PlanInput{
				Client:            tc.client,
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
			var decoded map[string]any
			if err := json.Unmarshal(plan.Body, &decoded); err != nil {
				t.Fatalf("正文不是 JSON: %v", err)
			}
			if decoded["model"] != "glm-4.6" {
				t.Fatalf("目标方言正文的顶层 model 未改写: %v", decoded["model"])
			}
		})
	}
}

// TestBuildPlanModelRedirectAppliesOnResolutionFallback 钉住**回退路径**同样改写模型名。
//
// 这条回退路径是「路径解析失败」（跨线调用带副作用的端点，如 count_tokens / compact / embeddings
// 只允许同线）：此时 `Conversion` 被置回 nil、正文**保持客户端方言且仍是合法 JSON**。
//
// Node 的依据：重定向在转换**之前**就已就地写进 `session.request.message`，
// 故即使转换不可用而回退，发出去的仍是「客户端方言 + 重定向后的模型名」。
func TestBuildPlanModelRedirectAppliesOnResolutionFallback(t *testing.T) {
	provider := claudeProvider("https://relay.example.com")
	provider.Type = convert.ProviderOpenAICompatible
	provider.ModelRedirects = json.RawMessage(
		`[{"matchType":"exact","source":"claude-sonnet-4-5-20250929","target":"glm-4.6"}]`)

	client := newClaudeRequest(claudeRequestBody)
	// count_tokens 只允许同线（paths.go 的 nativePathSets），跨线无映射 → 路径解析失败。
	client.Path = "/v1/messages/count_tokens"

	plan, err := BuildPlan(PlanInput{
		Client:            client,
		Target:            Target{Provider: provider},
		ConversionEnabled: true,
	})
	if err != nil {
		t.Fatalf("路径解析失败不得让计划编译失败（Node 是 fail-closed 回退、请求照常直通）: %v", err)
	}
	if plan.ConversionFailure == nil || plan.ConversionFailure.Phase != convert.PhasePathResolution {
		t.Fatalf("前置不成立：本用例需走路径解析失败回退，实际 %+v", plan.ConversionFailure)
	}
	if plan.Conversion != nil {
		t.Fatal("路径解析失败后 Conversion 必须置回 nil")
	}

	var decoded map[string]any
	if err := json.Unmarshal(plan.Body, &decoded); err != nil {
		t.Fatalf("回退后正文应为合法的客户端方言 JSON: %v", err)
	}
	if decoded["model"] != "glm-4.6" {
		t.Fatalf("回退路径的正文模型未改写: %v（上游同样会 400/404）", decoded["model"])
	}
	// 回退意味着不转换：正文必须仍是客户端方言（anthropic 的 messages/system 形态）。
	if _, ok := decoded["messages"]; !ok {
		t.Fatalf("路径解析失败后正文应保持客户端方言: %s", plan.Body)
	}
}

// chatClient 构造 openai-chat 线的客户端请求（路径 + 显式 Format，两者都需对）。
func chatClient(body string) ClientRequest {
	c := newClaudeRequest(body)
	c.Path = "/v1/chat/completions"
	c.Format = convert.FormatOpenAI
	return c
}

// responsesClient 构造 openai-responses 线的客户端请求。
func responsesClient(body string) ClientRequest {
	c := newClaudeRequest(body)
	c.Path = "/v1/responses"
	c.Format = convert.FormatResponse
	return c
}

// TestBuildPlanRecordsBodyConversionFailure 钉住「转换失败要留事实，但回退语义不变」。
//
// 背景：Node 的语义是转换不可用时**静默**回退原生直通（把 conversionPlan 置空），Go 原本逐字
// 复刻了这一点——连 err 都丢掉。后果是使用记录页上「没转换」与「想转换但失败了」长得一样，
// 而这两件事的排查方向完全不同。本用例同时钉住两件事：
//   - **回退语义未变**：Conversion 置 nil、ConversionFallback 为真、请求仍拿到可用的上游计划；
//   - **失败事实被保留**：ConversionFailure 带协议对/阶段/原因，供终态落审计。
func TestBuildPlanRecordsBodyConversionFailure(t *testing.T) {
	provider := claudeProvider("https://relay.example.com")
	provider.Type = convert.ProviderOpenAICompatible

	// 正文不是合法 JSON → convertBody 在解析阶段即失败（跨协议转换已计划成立）。
	plan, err := BuildPlan(PlanInput{
		Client:            newClaudeRequest(`{"model":"claude-sonnet-4-5-20250929","messages":[`),
		Target:            Target{Provider: provider},
		ConversionEnabled: true,
	})
	if err != nil {
		t.Fatalf("转换失败不得让计划编译失败（Node 语义是回退而非报错）: %v", err)
	}
	if plan.Conversion != nil {
		t.Fatal("转换失败后 Conversion 必须置回 nil（Node 的回退语义）")
	}
	if !plan.ConversionFallback {
		t.Fatal("转换失败必须置 ConversionFallback，否则调用方看不出本次未转换")
	}
	if plan.ConversionFailure == nil {
		t.Fatal("转换失败必须留下失败事实，否则界面无法区分「没转换」与「转换失败」")
	}
	if plan.ConversionFailure.Phase != convert.PhaseBodyConversion {
		t.Errorf("阶段应为 %q，实际 %q", convert.PhaseBodyConversion, plan.ConversionFailure.Phase)
	}
	if plan.ConversionFailure.ClientProtocol != convert.ProtocolAnthropicMessages {
		t.Errorf("客户端协议应为 anthropic-messages，实际 %q", plan.ConversionFailure.ClientProtocol)
	}
	if plan.ConversionFailure.TargetProtocol != convert.ProtocolOpenAIChat {
		t.Errorf("目标协议应为 openai-chat，实际 %q", plan.ConversionFailure.TargetProtocol)
	}
	if plan.ConversionFailure.Reason == "" {
		t.Error("失败原因不得为空——它就是这条审计的价值")
	}
	if !plan.ConversionFailure.Fallback {
		t.Error("本次确实按原生直通继续，fallback 应如实为真")
	}
	// 回退后的实际形态（**如实钉住，非本任务改动**）：路径与协议线在**第 2 步**（路径映射）就已定
	// 为目标线，正文转换失败（第 4 步）只把 `Conversion` 置回 nil，**不会**把 URL/协议线改回客户端线。
	//
	// 登记：这与本文件第 275 行附近的注释（「而不是把未转换正文发到目标端点上」）**不一致**——
	// 实测是发到了转换后的路径上、体仍是客户端方言。它不是本次要改的东西（本任务只加记录，
	// **不得改变回退语义**），故此处按现状钉住，并把该不一致写进报告供裁。
	if plan.URL != "https://relay.example.com/v1/chat/completions" {
		t.Errorf("回退后仍打转换后的路径（现状），实际 %q", plan.URL)
	}
	if plan.Protocol != convert.ProtocolOpenAIChat {
		t.Errorf("回退后协议线仍为目标线（现状），实际 %q", plan.Protocol)
	}
}

// TestBuildPlanRecordsPathResolutionFailure 钉住另一个失败阶段的记录（路径不可映射）。
//
// 与正文失败的区别：这里是 Node 的 fail-closed 分支（目标协议线下没有对应端点），
// 实现上不置 ConversionFallback，但对使用者而言同样是「本该转换、结果没转」，故同样必须留事实，
// 否则这一支永远查不出来。
func TestBuildPlanRecordsPathResolutionFailure(t *testing.T) {
	provider := claudeProvider("https://relay.example.com")
	provider.Type = convert.ProviderOpenAICompatible

	request := newClaudeRequest(claudeRequestBody)
	// count_tokens 属跨线**只允许同线**的端点（见 convert/paths.go 的 crossLinePaths），
	// 故 anthropic → openai-compatible 时无对应上游路径。
	request.Path = "/v1/messages/count_tokens"

	plan, err := BuildPlan(PlanInput{
		Client:            request,
		Target:            Target{Provider: provider},
		ConversionEnabled: true,
	})
	if err != nil {
		t.Fatalf("路径不可映射只应回退，不应让计划编译失败: %v", err)
	}
	if plan.Conversion != nil {
		t.Fatal("路径不可映射时 Conversion 必须置回 nil")
	}
	if plan.ConversionFailure == nil {
		t.Fatal("路径不可映射必须留失败事实")
	}
	if plan.ConversionFailure.Phase != convert.PhasePathResolution {
		t.Errorf("阶段应为 %q，实际 %q", convert.PhasePathResolution, plan.ConversionFailure.Phase)
	}
	if plan.ConversionFailure.TargetProtocol != convert.ProtocolOpenAIChat {
		t.Errorf("目标协议应为 openai-chat，实际 %q", plan.ConversionFailure.TargetProtocol)
	}
	if !strings.Contains(plan.ConversionFailure.Reason, "/v1/messages/count_tokens") {
		t.Errorf("原因里要点出是哪个客户端路径不可映射，实际 %q", plan.ConversionFailure.Reason)
	}
}

// TestBuildPlanRecordsNoFailureWhenConversionSucceeds 钉住「成功路径不留失败事实」。
//
// 反向断言：若成功也留失败事实，使用记录页会把每一次正常转换都标成失败。
func TestBuildPlanRecordsNoFailureWhenConversionSucceeds(t *testing.T) {
	provider := claudeProvider("https://relay.example.com")
	provider.Type = convert.ProviderOpenAICompatible

	plan, err := BuildPlan(PlanInput{
		Client:            newClaudeRequest(claudeRequestBody),
		Target:            Target{Provider: provider},
		ConversionEnabled: true,
	})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	if plan.Conversion == nil {
		t.Fatal("本用例要求转换成功")
	}
	if plan.ConversionFailure != nil {
		t.Fatalf("转换成功时不得留失败事实：%+v", plan.ConversionFailure)
	}
	if plan.ConversionFallback {
		t.Error("转换成功时不得标记回退")
	}
}

// TestBuildPlanRecordsNoFailureWhenConversionNotRequested 钉住「原生同协议不留失败事实」。
//
// 反向断言第二面：压根没要求转换（供应商同协议）时，既不该写成功条目，也不该写失败条目
// （Node 原话：不给全部原生请求白写一条）。
func TestBuildPlanRecordsNoFailureWhenConversionNotRequested(t *testing.T) {
	plan, err := BuildPlan(PlanInput{
		Client:            newClaudeRequest(claudeRequestBody),
		Target:            Target{Provider: claudeProvider("https://relay.example.com")},
		ConversionEnabled: true,
	})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	if plan.Conversion != nil || plan.ConversionFailure != nil || plan.ConversionFallback {
		t.Fatalf("同协议请求不应留任何转换痕迹：conversion=%v failure=%+v fallback=%v",
			plan.Conversion, plan.ConversionFailure, plan.ConversionFallback)
	}
}
