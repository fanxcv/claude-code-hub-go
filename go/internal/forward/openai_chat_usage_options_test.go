package forward

import (
	"encoding/json"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// 本文件的用例对齐 Node `src/app/v1/_lib/proxy/openai-chat-usage-options.test.ts` 的断言，
// 并补上 Go 侧特有的分支（数组形态、已为 true 不重编码）。

func decodeUsageBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("正文不是 JSON: %v", err)
	}
	return decoded
}

func TestApplyOpenAIChatStreamUsageOptionAddsIncludeUsage(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}],"stream":true}`)

	rewritten, changed, err := applyOpenAIChatStreamUsageOption(body, convert.ProviderOpenAICompatible, "/v1/chat/completions")
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if !changed {
		t.Fatalf("应报告已改写")
	}
	decoded := decodeUsageBody(t, rewritten)
	options, ok := decoded["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options 形态不对: %v", decoded["stream_options"])
	}
	if include, ok := options["include_usage"].(bool); !ok || !include {
		t.Fatalf("include_usage 未置真: %v", options)
	}
}

func TestApplyOpenAIChatStreamUsageOptionPreservesExistingOptions(t *testing.T) {
	body := []byte(`{"stream":true,"stream_options":{"foo":"bar","include_usage":false}}`)

	rewritten, changed, err := applyOpenAIChatStreamUsageOption(body, convert.ProviderOpenAICompatible, "/v1/chat/completions")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	options := decodeUsageBody(t, rewritten)["stream_options"].(map[string]any)
	if options["foo"] != "bar" {
		t.Fatalf("既有键被丢了: %v", options)
	}
	if options["include_usage"] != true {
		t.Fatalf("include_usage 应为 true: %v", options)
	}
}

func TestApplyOpenAIChatStreamUsageOptionLeavesNonStreaming(t *testing.T) {
	body := []byte(`{"stream":false}`)

	rewritten, changed, err := applyOpenAIChatStreamUsageOption(body, convert.ProviderOpenAICompatible, "/v1/chat/completions")
	if err != nil || changed {
		t.Fatalf("非流式不该动: changed=%v err=%v", changed, err)
	}
	if string(rewritten) != string(body) {
		t.Fatalf("正文被改写了: %s", rewritten)
	}
}

func TestApplyOpenAIChatStreamUsageOptionLeavesOtherProviderTypes(t *testing.T) {
	body := []byte(`{"stream":true}`)

	for _, providerType := range []convert.ProviderType{
		convert.ProviderClaude, convert.ProviderCodex, convert.ProviderGemini,
	} {
		if _, changed, _ := applyOpenAIChatStreamUsageOption(body, providerType, "/v1/chat/completions"); changed {
			t.Fatalf("供应商类型 %s 不该被改写", providerType)
		}
	}
}

func TestApplyOpenAIChatStreamUsageOptionLeavesOtherPaths(t *testing.T) {
	body := []byte(`{"stream":true}`)

	for _, path := range []string{"/v1/messages", "/v1/responses", "/v1/chat/completions/extra"} {
		if _, changed, _ := applyOpenAIChatStreamUsageOption(body, convert.ProviderOpenAICompatible, path); changed {
			t.Fatalf("路径 %s 不该被改写", path)
		}
	}
}

func TestApplyOpenAIChatStreamUsageOptionSkipsWhenAlreadyTrue(t *testing.T) {
	body := []byte(`{"stream":true,"stream_options":{"include_usage":true}}`)

	rewritten, changed, err := applyOpenAIChatStreamUsageOption(body, convert.ProviderOpenAICompatible, "/v1/chat/completions")
	if err != nil || changed {
		t.Fatalf("已是 true 不该重编码: changed=%v err=%v", changed, err)
	}
	if string(rewritten) != string(body) {
		t.Fatalf("正文被改写了: %s", rewritten)
	}
}

func TestApplyOpenAIChatStreamUsageOptionLeavesArrayOptions(t *testing.T) {
	// Node 的 `typeof !== "object" || Array.isArray` 同判：数组形态不动它。
	body := []byte(`{"stream":true,"stream_options":[{"include_usage":false}]}`)

	rewritten, changed, err := applyOpenAIChatStreamUsageOption(body, convert.ProviderOpenAICompatible, "/v1/chat/completions")
	if err != nil || changed {
		t.Fatalf("数组形态不该动: changed=%v err=%v", changed, err)
	}
	if string(rewritten) != string(body) {
		t.Fatalf("正文被改写了: %s", rewritten)
	}
}

func TestApplyOpenAIChatStreamUsageOptionTreatsNullOptionsAsAbsent(t *testing.T) {
	// 显式 null 与缺省同判（Node 的 `streamOptions == null`）。
	body := []byte(`{"stream":true,"stream_options":null}`)

	rewritten, changed, err := applyOpenAIChatStreamUsageOption(body, convert.ProviderOpenAICompatible, "/v1/chat/completions")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	options := decodeUsageBody(t, rewritten)["stream_options"].(map[string]any)
	if options["include_usage"] != true {
		t.Fatalf("include_usage 未置真: %v", options)
	}
}

func TestApplyOpenAIChatStreamUsageOptionIgnoresTruthyNonBooleanStream(t *testing.T) {
	// Node 判的是 `body.stream !== true`，故 1 / "true" 都不算。
	for _, body := range []string{`{"stream":1}`, `{"stream":"true"}`, `{}`} {
		if _, changed, _ := applyOpenAIChatStreamUsageOption([]byte(body), convert.ProviderOpenAICompatible, "/v1/chat/completions"); changed {
			t.Fatalf("正文 %s 的 stream 非布尔真，不该改写", body)
		}
	}
}

// TestBuildPlanAddsIncludeUsageForOpenAICompatibleChat 钉住接线：规则真的落在计划正文上。
func TestBuildPlanAddsIncludeUsageForOpenAICompatibleChat(t *testing.T) {
	provider := claudeProvider("https://upstream.example.com")
	provider.Type = convert.ProviderOpenAICompatible

	client := newClaudeRequest(`{"model":"deepseek-v4-flash","messages":[{"role":"user","content":"ping"}],"stream":true}`)
	client.Path = "/v1/chat/completions"
	client.Format = convert.FormatOpenAI

	plan, err := BuildPlan(PlanInput{Client: client, Target: Target{Provider: provider}})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	options, ok := decodeUsageBody(t, plan.Body)["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("计划正文缺 stream_options: %s", plan.Body)
	}
	if options["include_usage"] != true {
		t.Fatalf("include_usage 未置真: %v", options)
	}
}

// TestBuildPlanLeavesClaudeBodyUntouchedByUsageOption 是对照：claude 线不该被这条规则碰。
func TestBuildPlanLeavesClaudeBodyUntouchedByUsageOption(t *testing.T) {
	client := newClaudeRequest(`{"model":"claude-sonnet-4-5-20250929","stream":true,"messages":[]}`)

	plan, err := BuildPlan(PlanInput{Client: client, Target: Target{Provider: claudeProvider("https://upstream.example.com")}})
	if err != nil {
		t.Fatalf("BuildPlan 失败: %v", err)
	}
	if string(plan.Body) != string(client.Body) {
		t.Fatalf("claude 正文被改写了: %s", plan.Body)
	}
}
