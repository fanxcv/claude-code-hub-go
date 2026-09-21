package providertest

import (
	"strings"
	"testing"
)

// engine_test.go：解析器 / 校验器 / 提示构造的语义钉（夹具取自 Node 各分支的实际形态）。

func TestParseAnthropicNonStreaming(t *testing.T) {
	body := `{"model":"claude-haiku-4-5-20251001","content":[{"type":"text","text":"po"},{"type":"thinking","text":"X"},{"type":"text","text":"ng"}],"usage":{"input_tokens":7,"output_tokens":3,"cache_creation":{"ephemeral_5m_input_tokens":11,"ephemeral_1h_input_tokens":4}}}`
	parsed := ParseResponse(TypeClaude, body, "application/json")
	if parsed.Content != "pong" {
		t.Errorf("应只拼 text 段，实际 %q", parsed.Content)
	}
	if parsed.Model != "claude-haiku-4-5-20251001" {
		t.Errorf("model 应回填，实际 %q", parsed.Model)
	}
	if parsed.Usage == nil || parsed.Usage.InputTokens != 7 || parsed.Usage.OutputTokens != 3 {
		t.Fatalf("usage 不符: %+v", parsed.Usage)
	}
	// cache_creation_input_tokens 缺失但有 5m/1h -> 相加补齐（11+4=15）。
	if parsed.Usage.CacheCreationInputTokens == nil || *parsed.Usage.CacheCreationInputTokens != 15 {
		t.Errorf("cacheCreationInputTokens 应补齐为 15，实际 %+v", parsed.Usage.CacheCreationInputTokens)
	}
}

func TestParseErrorBodiesUseMessage(t *testing.T) {
	cases := []struct {
		name         string
		providerType ProviderType
		body         string
	}{
		{"anthropic", TypeClaude, `{"error":{"message":"rate limited"}}`},
		{"openai", TypeOpenAICompatible, `{"error":{"message":"rate limited"}}`},
		{"codex", TypeCodex, `{"error":{"message":"rate limited"}}`},
		{"gemini", TypeGemini, `{"error":{"message":"rate limited"}}`},
	}
	for _, testCase := range cases {
		parsed := ParseResponse(testCase.providerType, testCase.body, "application/json")
		if parsed.Content != "rate limited" {
			t.Errorf("%s: 错误正文应取 message，实际 %q", testCase.name, parsed.Content)
		}
		if parsed.Usage != nil {
			t.Errorf("%s: 错误正文不应有 usage", testCase.name)
		}
	}
}

func TestParseCodexNonStreamingAndNDJSON(t *testing.T) {
	body := `{"model":"gpt-5.5","output":[{"content":[{"type":"output_text","text":"po"},{"type":"other","text":"ng"}]}],"usage":{"input_tokens":5,"output_tokens":2}}`
	parsed := ParseResponse(TypeCodex, body, "application/json")
	if parsed.Content != "pong" {
		t.Errorf("codex 非流式应拼 output[].content[].text，实际 %q", parsed.Content)
	}
	if parsed.Usage == nil || parsed.Usage.OutputTokens != 2 {
		t.Errorf("usage 不符: %+v", parsed.Usage)
	}

	ndjson := `{"type":"response.output_text.delta","delta":"po"}
{"type":"response.output_text.delta","delta":"ng"}`
	parsedND := ParseResponse(TypeCodex, ndjson, "application/x-ndjson")
	if !parsedND.IsStreaming || parsedND.Content != "pong" || parsedND.ChunksReceived != 2 {
		t.Errorf("NDJSON 应识别为流式并拼出 pong，实际 %+v", parsedND)
	}
}

func TestParseSSEDeltaFlavors(t *testing.T) {
	cases := []struct {
		name         string
		providerType ProviderType
		body         string
		want         string
	}{
		{
			"anthropic delta.text",
			TypeClaude,
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"pong\"}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":9}}\n\n",
			"pong",
		},
		{
			"codex response.output_text.delta",
			TypeCodex,
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"pong\"}\n\n",
			"pong",
		},
		{
			"openai choices delta",
			TypeOpenAICompatible,
			"data: {\"choices\":[{\"delta\":{\"content\":\"po\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"ng\"}}]}\n\n",
			"pong",
		},
	}
	for _, testCase := range cases {
		parsed := ParseResponse(testCase.providerType, testCase.body, "text/event-stream")
		if !parsed.IsStreaming {
			t.Errorf("%s: 应判为流式", testCase.name)
		}
		if parsed.Content != testCase.want {
			t.Errorf("%s: 正文应为 %q，实际 %q", testCase.name, testCase.want, parsed.Content)
		}
	}
}

func TestParseSSEFallbackChainWhenNoDeltas(t *testing.T) {
	// 只有 response.output_text.done（无 delta）时应走兜底链的优先级 1。
	body := "event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\",\"text\":\"pong\"}\n\n"
	parsed := ParseResponse(TypeCodex, body, "text/event-stream")
	if parsed.Content != "pong" {
		t.Errorf("无 delta 时应走兜底链，实际 %q", parsed.Content)
	}
}

// TestParseSSECompleteMessageWithoutDeltas 钉住「流里不发 delta、只给完整正文」的上游。
//
// 为何必须有它：非流式分支（parseOpenAIResponse）读 choices[].message.content 与
// choices[].text，SSE 分支此前只读 choices[].delta.content ⇒ 同一类上游在探测里被解析成
// **空正文**，被误报为不可用。
func TestParseSSECompleteMessageWithoutDeltas(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			"choices[].message.content",
			"data: {\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"pong\"}}]}\n\n",
			"pong",
		},
		{
			"choices[].text",
			"data: {\"choices\":[{\"text\":\"pong\"}]}\n\n",
			"pong",
		},
		{
			// 累计型：每帧 message 都是完整正文，取末帧（同优先级覆盖），不叠加。
			"末帧完整 message 覆盖前帧",
			"data: {\"choices\":[{\"message\":{\"content\":\"po\"}}]}\n\ndata: {\"choices\":[{\"message\":{\"content\":\"pong\"}}]}\n\n",
			"pong",
		},
		{
			// delta 与完整 message 并存：正文取 delta，不得把同一段内容算两遍。
			"delta 与 message 并存不重复计数",
			"data: {\"choices\":[{\"delta\":{\"content\":\"po\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"ng\"}}]}\n\ndata: {\"choices\":[{\"message\":{\"content\":\"pong\"}}]}\n\n",
			"pong",
		},
	}
	for _, testCase := range cases {
		parsed := ParseResponse(TypeOpenAICompatible, testCase.body, "text/event-stream")
		if !parsed.IsStreaming {
			t.Errorf("%s: 应判为流式", testCase.name)
		}
		if parsed.Content != testCase.want {
			t.Errorf("%s: 正文应为 %q，实际 %q", testCase.name, testCase.want, parsed.Content)
		}
	}
}

// TestParseSSEEmptyMessageStaysEmpty 钉住空值陷阱：role 宣告帧与空 content 不得被当成正文
// （否则会把「上游什么都没给」误判成「有有效内容」）。
func TestParseSSEEmptyMessageStaysEmpty(t *testing.T) {
	bodies := []string{
		// 只有 role 的宣告帧
		"data: {\"choices\":[{\"message\":{\"role\":\"assistant\"}}]}\n\n",
		// content 为空串
		"data: {\"choices\":[{\"message\":{\"content\":\"\"}}]}\n\n",
		// role + 空 content + finish_reason，仍无正文
		"data: {\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":\"stop\"}]}\n\n",
	}
	for _, body := range bodies {
		parsed := ParseResponse(TypeOpenAICompatible, body, "text/event-stream")
		if parsed.Content != "" {
			t.Errorf("空 message 不应产生正文，实际 %q（body=%s）", parsed.Content, body)
		}
	}
}

func TestParseGeminiAndRawFallback(t *testing.T) {
	body := `{"candidates":[{"content":{"parts":[{"text":"po"},{"text":"ng"}]}}],"usageMetadata":{"promptTokenCount":4,"candidatesTokenCount":2},"modelVersion":"gemini-2.5-flash"}`
	parsed := ParseResponse(TypeGemini, body, "application/json")
	if parsed.Content != "pong" || parsed.Model != "gemini-2.5-flash" {
		t.Errorf("gemini 解析不符: %+v", parsed)
	}
	if parsed.Usage == nil || parsed.Usage.InputTokens != 4 {
		t.Errorf("gemini usage 不符: %+v", parsed.Usage)
	}

	// 非 JSON：截断 500 字符（Node 的 catch 分支）。
	raw := strings.Repeat("x", 600)
	fallback := ParseResponse(TypeOpenAICompatible, raw, "text/plain")
	if len(fallback.Content) != 500 {
		t.Errorf("非 JSON 应截断 500，实际 %d", len(fallback.Content))
	}
}

func TestClassifyHTTPStatusTable(t *testing.T) {
	cases := []struct {
		status   int
		latency  int64
		wantStat TestStatus
		wantSub  TestSubStatus
	}{
		{200, 100, StatusGreen, SubStatusSuccess},
		{204, 100, StatusGreen, SubStatusSuccess},
		{200, 5001, StatusYellow, SubStatusSlowLatency},
		{301, 100, StatusGreen, SubStatusSuccess},
		{401, 100, StatusRed, SubStatusAuthError},
		{403, 100, StatusRed, SubStatusAuthError},
		{400, 100, StatusRed, SubStatusInvalidRequest},
		{429, 100, StatusRed, SubStatusRateLimit},
		{500, 100, StatusRed, SubStatusServerError},
		{503, 100, StatusRed, SubStatusServerError},
		{418, 100, StatusRed, SubStatusClientError},
	}
	for _, testCase := range cases {
		result := ClassifyHTTPStatus(testCase.status, testCase.latency, 5000)
		if result.Status != testCase.wantStat || result.SubStatus != testCase.wantSub {
			t.Errorf("%d@%dms 应为 %s/%s，实际 %s/%s", testCase.status, testCase.latency, testCase.wantStat, testCase.wantSub, result.Status, result.SubStatus)
		}
	}
}

func TestEvaluateContentValidationRules(t *testing.T) {
	// 无 successContains：原样通过。
	if got := EvaluateContentValidation(StatusGreen, SubStatusSuccess, "", ""); !got.ContentPassed || got.Status != StatusGreen {
		t.Errorf("无目标串应直接通过，实际 %+v", got)
	}
	// 已 red：不再降级，contentPassed=false。
	if got := EvaluateContentValidation(StatusRed, SubStatusServerError, "pong", "pong"); got.ContentPassed || got.Status != StatusRed {
		t.Errorf("已 red 应保持并置 contentPassed=false，实际 %+v", got)
	}
	// 429：跳过内容判定但不改状态。
	if got := EvaluateContentValidation(StatusRed, SubStatusRateLimit, "", "pong"); got.SubStatus != SubStatusRateLimit {
		t.Errorf("429 应保持 rate_limit，实际 %+v", got)
	}
	// 空正文 -> content_mismatch。
	if got := EvaluateContentValidation(StatusGreen, SubStatusSuccess, "   ", "pong"); got.Status != StatusRed || got.SubStatus != SubStatusContentMismatch {
		t.Errorf("空正文应 content_mismatch，实际 %+v", got)
	}
	// 不含目标串 -> content_mismatch。
	if got := EvaluateContentValidation(StatusGreen, SubStatusSuccess, "hello", "pong"); got.SubStatus != SubStatusContentMismatch {
		t.Errorf("不含目标串应 content_mismatch，实际 %+v", got)
	}
	// 含目标串 -> 保持原状态。
	if got := EvaluateContentValidation(StatusYellow, SubStatusSlowLatency, "say pong!", "pong"); got.Status != StatusYellow || !got.ContentPassed {
		t.Errorf("含目标串应保持 yellow 且通过，实际 %+v", got)
	}
}

func TestTestURLAndHeaders(t *testing.T) {
	url, err := TestURL("https://api.example.com/", TypeClaude, "", "")
	if err != nil || url != "https://api.example.com/v1/messages" {
		t.Errorf("claude URL 不符: %q err=%v", url, err)
	}
	url, err = TestURL("https://api.example.com", TypeGemini, "gemini-2.5-pro", "")
	if err != nil || url != "https://api.example.com/v1beta/models/gemini-2.5-pro:generateContent" {
		t.Errorf("gemini URL 应替换 {model}: %q err=%v", url, err)
	}
	url, err = TestURL("https://api.example.com", TypeClaude, "", "/v1/messages?beta=true")
	if err != nil || url != "https://api.example.com/v1/messages?beta=true" {
		t.Errorf("pathOverride 应生效: %q err=%v", url, err)
	}

	headers, err := TestHeaders(TypeClaude, "sk-ant", "https://api.anthropic.com", "", nil, false)
	if err != nil {
		t.Fatalf("建头失败: %v", err)
	}
	if headers["anthropic-version"] != "2023-06-01" || headers["content-type"] != "application/json" {
		t.Errorf("claude 头不符: %v", headers)
	}
	if headers["User-Agent"] != claudeCLIUserAgent {
		t.Errorf("UA 应取默认: %q", headers["User-Agent"])
	}
	if _, has := headers["x-api-key"]; !has {
		t.Errorf("官方域名应发 x-api-key: %v", headers)
	}

	// claude-auth：forceBearerOnly（除非 AWS External 网关）。
	authHeaders, _ := TestHeaders(TypeClaudeAuth, "sk", "https://api.anthropic.com", "", nil, false)
	if _, has := authHeaders["x-api-key"]; has {
		t.Errorf("claude-auth 应只发 Bearer: %v", authHeaders)
	}

	// gemini：默认 x-goog-api-key；JSON 凭据时改 Bearer。
	geminiHeaders, _ := TestHeaders(TypeGemini, "gm", "https://generativelanguage.googleapis.com", "", nil, false)
	if geminiHeaders["x-goog-api-key"] != "gm" {
		t.Errorf("gemini 默认应发 x-goog-api-key: %v", geminiHeaders)
	}
	geminiBearer, _ := TestHeaders(TypeGemini, "gm", "https://generativelanguage.googleapis.com", "", nil, true)
	if geminiBearer["Authorization"] != "Bearer gm" {
		t.Errorf("gemini JSON 凭据应改 Bearer: %v", geminiBearer)
	}

	// 额外头覆盖默认（预设的 Anthropic-Beta 等）。
	withExtra, _ := TestHeaders(TypeClaude, "k", "https://api.anthropic.com", "custom-ua", map[string]string{"X-App": "cli"}, false)
	if withExtra["X-App"] != "cli" || withExtra["User-Agent"] != "custom-ua" {
		t.Errorf("额外头/UA 覆盖不符: %v", withExtra)
	}

	if _, err := TestHeaders(ProviderType("nope"), "k", "", "", nil, false); err == nil {
		t.Error("未知类型应报不支持")
	}
}

func TestTestBodyPerType(t *testing.T) {
	claude, err := TestBody(TypeClaude, "claude-sonnet-4-5")
	if err != nil || claude["model"] != "claude-sonnet-4-5" || claude["stream"] != true {
		t.Errorf("claude 兜底体不符: %v err=%v", claude, err)
	}
	gemini, err := TestBody(TypeGemini, "gemini-2.5-pro")
	if err != nil {
		t.Fatalf("gemini 兜底体失败: %v", err)
	}
	if _, has := gemini["model"]; has {
		t.Error("gemini 兜底体不应带 model")
	}
	// 默认模型兜底。
	codex, _ := TestBody(TypeCodex, "")
	if codex["model"] != "gpt-5.5" {
		t.Errorf("codex 默认模型应为 gpt-5.5，实际 %v", codex["model"])
	}
	if _, err := TestBody(ProviderType("nope"), ""); err == nil {
		t.Error("未知类型应报不支持")
	}
}

func TestVersionlessOpenAIFallback(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"https://relay.example.com/v1/chat/completions", "https://relay.example.com/chat/completions"},
		{"https://relay.example.com/api/v1/responses", "https://relay.example.com/api/responses"},
		{"https://relay.example.com/v1/messages", ""},
		{"not a url", ""},
	}
	for _, testCase := range cases {
		if got := VersionlessOpenAIFallbackURL(testCase.in); got != testCase.want {
			t.Errorf("%s: 应改写成 %q，实际 %q", testCase.in, testCase.want, got)
		}
	}
}

func TestApiTestTimeoutFromEnv(t *testing.T) {
	t.Setenv("API_TEST_TIMEOUT_MS", "30000")
	if got := ApiTestTimeout(); got.Seconds() != 30 {
		t.Errorf("应读 30000ms，实际 %v", got)
	}
	t.Setenv("API_TEST_TIMEOUT_MS", "1000") // 越界 -> 回落默认 15s
	if got := ApiTestTimeout(); got.Seconds() != 15 {
		t.Errorf("越界应回落 15s，实际 %v", got)
	}
	t.Setenv("API_TEST_TIMEOUT_MS", "abc")
	if got := ApiTestTimeout(); got.Seconds() != 15 {
		t.Errorf("非法应回落 15s，实际 %v", got)
	}
	t.Setenv("API_TEST_TIMEOUT_MS", "")
	if got := ApiTestTimeout(); got.Seconds() != 15 {
		t.Errorf("空值应回落 15s，实际 %v", got)
	}
}
