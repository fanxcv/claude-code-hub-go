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

	rewritten, changed := applyOpenAIChatStreamUsageOption(body, convert.ProviderOpenAICompatible, "/v1/chat/completions")
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

	rewritten, changed := applyOpenAIChatStreamUsageOption(body, convert.ProviderOpenAICompatible, "/v1/chat/completions")
	if !changed {
		t.Fatalf("changed=%v", changed)
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

	rewritten, changed := applyOpenAIChatStreamUsageOption(body, convert.ProviderOpenAICompatible, "/v1/chat/completions")
	if changed {
		t.Fatalf("非流式不该动: changed=%v", changed)
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
		if _, changed := applyOpenAIChatStreamUsageOption(body, providerType, "/v1/chat/completions"); changed {
			t.Fatalf("供应商类型 %s 不该被改写", providerType)
		}
	}
}

func TestApplyOpenAIChatStreamUsageOptionLeavesOtherPaths(t *testing.T) {
	body := []byte(`{"stream":true}`)

	for _, path := range []string{"/v1/messages", "/v1/responses", "/v1/chat/completions/extra"} {
		if _, changed := applyOpenAIChatStreamUsageOption(body, convert.ProviderOpenAICompatible, path); changed {
			t.Fatalf("路径 %s 不该被改写", path)
		}
	}
}

func TestApplyOpenAIChatStreamUsageOptionSkipsWhenAlreadyTrue(t *testing.T) {
	body := []byte(`{"stream":true,"stream_options":{"include_usage":true}}`)

	rewritten, changed := applyOpenAIChatStreamUsageOption(body, convert.ProviderOpenAICompatible, "/v1/chat/completions")
	if changed {
		t.Fatalf("已是 true 不该重编码: changed=%v", changed)
	}
	if string(rewritten) != string(body) {
		t.Fatalf("正文被改写了: %s", rewritten)
	}
}

func TestApplyOpenAIChatStreamUsageOptionLeavesArrayOptions(t *testing.T) {
	// Node 的 `typeof !== "object" || Array.isArray` 同判：数组形态不动它。
	body := []byte(`{"stream":true,"stream_options":[{"include_usage":false}]}`)

	rewritten, changed := applyOpenAIChatStreamUsageOption(body, convert.ProviderOpenAICompatible, "/v1/chat/completions")
	if changed {
		t.Fatalf("数组形态不该动: changed=%v", changed)
	}
	if string(rewritten) != string(body) {
		t.Fatalf("正文被改写了: %s", rewritten)
	}
}

func TestApplyOpenAIChatStreamUsageOptionTreatsNullOptionsAsAbsent(t *testing.T) {
	// 显式 null 与缺省同判（Node 的 `streamOptions == null`）。
	body := []byte(`{"stream":true,"stream_options":null}`)

	rewritten, changed := applyOpenAIChatStreamUsageOption(body, convert.ProviderOpenAICompatible, "/v1/chat/completions")
	if !changed {
		t.Fatalf("changed=%v", changed)
	}
	options := decodeUsageBody(t, rewritten)["stream_options"].(map[string]any)
	if options["include_usage"] != true {
		t.Fatalf("include_usage 未置真: %v", options)
	}
}

func TestApplyOpenAIChatStreamUsageOptionIgnoresTruthyNonBooleanStream(t *testing.T) {
	// Node 判的是 `body.stream !== true`，故 1 / "true" 都不算。
	for _, body := range []string{`{"stream":1}`, `{"stream":"true"}`, `{}`} {
		if _, changed := applyOpenAIChatStreamUsageOption([]byte(body), convert.ProviderOpenAICompatible, "/v1/chat/completions"); changed {
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

// TestApplyOpenAIChatStreamUsageOptionKeepsNumberLiterals 钉住数字字面量不经 float64 往返：
// 顶层与嵌套层各放一个会被 map[string]any 往返改写的字面量（超 2^53 的整数与 1e21）。
func TestApplyOpenAIChatStreamUsageOptionKeepsNumberLiterals(t *testing.T) {
	body := []byte(`{"stream":true,"max_int":9007199254740993,"meta":{"huge":1e21,"nested_int":9007199254740993}}`)

	rewritten, changed := applyOpenAIChatStreamUsageOption(body, convert.ProviderOpenAICompatible, "/v1/chat/completions")
	if !changed {
		t.Fatalf("changed=%v", changed)
	}
	decoded, err := convert.ParseJSON(rewritten)
	if err != nil {
		t.Fatalf("改写后不是合法 JSON: %v", err)
	}
	maxInt, _ := decoded.Get("max_int")
	if got, _ := maxInt.NumberLiteral(); got != "9007199254740993" {
		t.Fatalf("顶层整数精度丢失: %q", got)
	}
	meta := decoded.ObjectField("meta")
	if meta == nil {
		t.Fatalf("嵌套对象被丢了: %s", rewritten)
	}
	huge, _ := meta.Get("huge")
	if got, _ := huge.NumberLiteral(); got != "1e21" {
		t.Fatalf("嵌套 1e21 字面量被重排: %q", got)
	}
	nestedInt, _ := meta.Get("nested_int")
	if got, _ := nestedInt.NumberLiteral(); got != "9007199254740993" {
		t.Fatalf("嵌套整数精度丢失: %q", got)
	}
}

// TestApplyOpenAIChatStreamUsageOptionKeepsTopLevelKeyOrder 钉住顶层键序保持（不受字典序重排）。
func TestApplyOpenAIChatStreamUsageOptionKeepsTopLevelKeyOrder(t *testing.T) {
	// 顶层键故意非字典序：z_flag 在 stream 之前。
	body := []byte(`{"z_flag":true,"stream":true,"a_flag":false}`)

	rewritten, changed := applyOpenAIChatStreamUsageOption(body, convert.ProviderOpenAICompatible, "/v1/chat/completions")
	if !changed {
		t.Fatalf("changed=%v", changed)
	}
	decoded, err := convert.ParseJSON(rewritten)
	if err != nil {
		t.Fatalf("改写后不是合法 JSON: %v", err)
	}
	var keys []string
	for _, member := range decoded.Members() {
		keys = append(keys, member.Key)
	}
	want := []string{"z_flag", "stream", "a_flag", "stream_options"}
	if len(keys) != len(want) {
		t.Fatalf("顶层键集合被改：got %v want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("顶层键序被改：got %v want %v", keys, want)
		}
	}
}

// TestApplyOpenAIChatStreamUsageOptionPreservesNestedStructure 钉住除
// stream_options.include_usage 外语义等价：多层嵌套对象与数组一并不丢。
func TestApplyOpenAIChatStreamUsageOptionPreservesNestedStructure(t *testing.T) {
	body := []byte(`{"stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object","required":["a","b"]}}}],"meta":{"n":{"deep":[1,2,{"x":null}]}}}`)

	rewritten, changed := applyOpenAIChatStreamUsageOption(body, convert.ProviderOpenAICompatible, "/v1/chat/completions")
	if !changed {
		t.Fatalf("changed=%v", changed)
	}
	original := decodeUsageBody(t, body)
	got := decodeUsageBody(t, rewritten)
	options, ok := got["stream_options"].(map[string]any)
	if !ok {
		t.Fatalf("stream_options 形态不对: %s", rewritten)
	}
	if options["include_usage"] != true || len(options) != 1 {
		t.Fatalf("stream_options 应只有 include_usage=true: %v", options)
	}
	// 去掉补齐的 stream_options 后，其余部分须与原文逐值相等（json.Marshal 对 map 是
	// 字典序的确定性序列化，可作深比较）。
	delete(got, "stream_options")
	wantJSON, _ := json.Marshal(original)
	gotJSON, _ := json.Marshal(got)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("除 stream_options 外语义被改：\n got %s\nwant %s", gotJSON, wantJSON)
	}
}

// TestApplyOpenAIChatStreamUsageOptionKeepsExistingOptionKeys 钉住 stream_options 已有其它键时
// 其键序与内容保持，只有 include_usage 被置真。
func TestApplyOpenAIChatStreamUsageOptionKeepsExistingOptionKeys(t *testing.T) {
	body := []byte(`{"stream":true,"stream_options":{"alpha":1,"include_usage":false,"beta":{"n":2}}}`)

	rewritten, changed := applyOpenAIChatStreamUsageOption(body, convert.ProviderOpenAICompatible, "/v1/chat/completions")
	if !changed {
		t.Fatalf("changed=%v", changed)
	}
	decoded, err := convert.ParseJSON(rewritten)
	if err != nil {
		t.Fatalf("改写后不是合法 JSON: %v", err)
	}
	options := decoded.ObjectField("stream_options")
	if options == nil {
		t.Fatalf("stream_options 被丢了: %s", rewritten)
	}
	var keys []string
	for _, member := range options.Members() {
		keys = append(keys, member.Key)
	}
	want := []string{"alpha", "include_usage", "beta"}
	if len(keys) != len(want) {
		t.Fatalf("stream_options 键集合被改：got %v want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("stream_options 键序被改：got %v want %v", keys, want)
		}
	}
	alpha, _ := options.Get("alpha")
	if got, _ := alpha.NumberLiteral(); got != "1" {
		t.Fatalf("alpha 被改: %q", got)
	}
	includeUsage, _ := options.Get("include_usage")
	if include, isBool := includeUsage.Bool(); !isBool || !include {
		t.Fatalf("include_usage 未置真: %s", rewritten)
	}
	beta := options.ObjectField("beta")
	if beta == nil {
		t.Fatalf("beta 被丢了: %s", rewritten)
	}
	betaN, _ := beta.Get("n")
	if got, _ := betaN.NumberLiteral(); got != "2" {
		t.Fatalf("beta.n 被改: %q", got)
	}
}

// TestApplyOpenAIChatStreamUsageOptionNormalizesEscapeForm 说明性用例：convert.Value 按
// JS JSON.stringify 语义序列化，`\u003c` 形态会归一为 `<`。这是**有意接受**的形态变化
// （语义等价），故只断言解码后的值，不断言字节形态。
func TestApplyOpenAIChatStreamUsageOptionNormalizesEscapeForm(t *testing.T) {
	body := []byte(`{"stream":true,"content":"\u003cdiv\u003e"}`)

	rewritten, changed := applyOpenAIChatStreamUsageOption(body, convert.ProviderOpenAICompatible, "/v1/chat/completions")
	if !changed {
		t.Fatalf("changed=%v", changed)
	}
	if got := decodeUsageBody(t, rewritten)["content"]; got != "<div>" {
		t.Fatalf("转义归一后语义应不变: %v", got)
	}
}
