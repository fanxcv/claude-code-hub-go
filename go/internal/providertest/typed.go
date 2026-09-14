package providertest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
)

// typed.go 复刻 `src/actions/providers.ts` 的 `executeProviderApiTest`（4292-4667）与四个定型测试
// 端点的配置（testProviderAnthropicMessages 4679、testProviderOpenAIChatCompletions 4705、
// testProviderOpenAIResponses 4739、testProviderGemini 4782）。
//
// 与 unified 路径的分工：unified 走「预设/计划 + 三级校验」（engine.go）；定型路径是
// **单次请求 + 固定 path/模型/头/体**，判定只看 HTTP 状态与「响应能否解析」。
//
// 关键语义（逐条对齐 Node）：
//   - **校验失败也返回 200**（`success:false`），只有最外层不可达才抛 Problem；
//   - `responseTime` 是「总耗时」；错误分支把它放在 details 里（不是 latencyMs）；
//   - extract 直接读**原始 JSON 的顶层字段**（`model` / `usage` / 四路嗅探的正文），
//     不经过 provider-testing 的解析器——故本文件自带 `ExtractFirstTextSnippet`。
//
// 与 Node 的差异（登记，见报告）：定型路径的**流式分支**在 Node 里走 actions 内的
// `parseStreamResponse`/`parseSSEText` 收集器（另有合并语义与 `format` 字段）；本实现复用
// `ParseResponse`（provider-testing 的解析器），`streamInfo` 给 `chunksReceived` + 按
// Content-Type 判定的 `format`。四条定型体默认都不请求流式（anthropic 显式 `stream:false`，
// 其余无 stream 字段），故常态路径不受影响。

// 复刻 API_TEST_CONFIG（actions/providers.ts:179-189）。
const (
	typedTestMaxTokens        = 100
	typedTestPrompt           = "Hello"
	typedResponsePreviewLimit = 500
	typedGeminiTimeoutMS      = 60000
)

// 复刻 PROXY_RETRY_STATUS_CODES（actions/providers.ts:191）。
var proxyRetryStatusCodes = []int{502, 504, 520, 521, 522, 523, 524, 525, 526, 527, 530}

// TypedArgs 复刻 ProviderApiTestArgs。
type TypedArgs struct {
	ProviderURL           string
	APIKey                string
	Model                 string
	ProxyURL              string
	ProxyFallbackToDirect bool
	TimeoutMS             *int64
}

// TypedExtract 是 extract 的结果（model/usage/content，三者皆可缺）。
type TypedExtract struct {
	Model   string
	Usage   *TokenUsage
	Content string
}

// TypedOptions 复刻 executeProviderApiTest 的 options。
type TypedOptions struct {
	// Path 复刻 options.path：字面量或「按 model + apiKey 生成路径」（Gemini 的 URL 认证分支带 key）。
	Path func(model string, apiKey string) string
	// DefaultModel 复刻 options.defaultModel。
	DefaultModel string
	// Headers 复刻 options.headers(apiKey, {providerUrl})。
	Headers func(apiKey string, providerURL string) map[string]string
	// Body 复刻 options.body(model)。
	Body func(model string) any
	// SuccessMessage 复刻 options.successMessage。
	SuccessMessage string
	// UserAgent 复刻 options.userAgent。
	UserAgent string
	// TimeoutMS 复刻 options.timeoutMs（nil 时回落 ApiTestTimeout()）。
	TimeoutMS *int64
	// Extract 复刻 options.extract：入参为「原始响应的解析结果」（本实现给原始 JSON 嗅探结果）。
	Extract func(body string, parsed ParsedResponse) TypedExtract
}

// TypedResult 是定型端点的对外 data（直接序列化；`ok` 恒为 true，与 Node 的 actionJson 同形）。
type TypedResult = map[string]any

// ExecuteProviderAPITest 复刻 executeProviderApiTest。
func ExecuteProviderAPITest(
	ctx context.Context,
	args TypedArgs,
	options TypedOptions,
	factory ClientFactory,
) TypedResult {
	if factory == nil {
		factory = DefaultClientFactory
	}

	// 复刻 4315-4326：代理地址格式非法 → 200 + success:false。
	if strings.TrimSpace(args.ProxyURL) != "" && !IsValidProxyURL(args.ProxyURL) {
		return TypedResult{
			"success": false,
			"message": "代理地址格式无效",
			"details": map[string]any{
				"error": "支持格式: http://, https://, socks5://, socks4://",
			},
		}
	}

	// 复刻 4328-4338：供应商 URL 校验失败 → 200 + success:false（details 取校验错误本身）。
	normalized, message, ok := ValidateProviderURLForConnectivity(args.ProviderURL)
	if !ok {
		return TypedResult{
			"success": false,
			"message": message,
			"details": map[string]any{"error": message},
		}
	}
	normalized = strings.TrimSuffix(normalized, "/")

	startTime := time.Now()
	model := args.Model
	if model == "" {
		model = options.DefaultModel
	}
	requestURL, err := TestURL(normalized, "", model, options.Path(model, args.APIKey))
	if err != nil {
		return typedConnectionFailure(startTime, err)
	}

	timeout := time.Duration(typedTimeoutMS(args, options)) * time.Millisecond
	payload, err := json.Marshal(options.Body(model))
	if err != nil {
		return typedConnectionFailure(startTime, err)
	}

	headers := map[string]string{}
	if options.Headers != nil {
		for key, value := range options.Headers(args.APIKey, normalized) {
			headers[key] = value
		}
	}
	// 复刻 4366-4374：定型路径自己的四件头（注意 Accept-Encoding 不含 zstd）。
	headers["User-Agent"] = options.UserAgent
	headers["Accept"] = "application/json, text/event-stream"
	headers["Accept-Language"] = "en-US,en;q=0.9"
	headers["Accept-Encoding"] = "gzip, deflate, br"
	headers["Connection"] = "keep-alive"

	response, body, responseTime, err := typedPerform(ctx, args, requestURL, headers, payload, timeout, factory)
	if err != nil {
		return TypedResult{
			"success": false,
			"message": "连接失败: " + err.Error(),
			"details": map[string]any{
				"responseTime": responseTime,
				"error":        err.Error(),
			},
		}
	}

	// 复刻 4417-4497：非 2xx 也读正文；details.error 取「错误 JSON 的可读消息 → 前 200 字 → 兜底文案」。
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return TypedResult{
			"success": false,
			"message": fmt.Sprintf("API 返回错误: HTTP %d", response.StatusCode),
			"details": map[string]any{
				"responseTime": responseTime,
				"error":        typedErrorDetail(body),
				"rawResponse":  body,
			},
		}
	}

	contentType := response.Header.Get("Content-Type")
	isDeclaredStream := strings.Contains(contentType, "text/event-stream") ||
		strings.Contains(contentType, "application/x-ndjson")
	if isDeclaredStream {
		// 复刻 4499-4539：声明为流 → 成功文案带「（流式响应）」。
		parsed := ParseResponse(TypeOpenAICompatible, body, contentType)
		if !parsed.IsStreaming {
			return TypedResult{
				"success": false,
				"message": "流式响应解析失败",
				"details": map[string]any{
					"responseTime": responseTime,
					"error":        "无法解析流式响应数据",
				},
			}
		}
		details := map[string]any{
			"responseTime": responseTime,
			"streamInfo": map[string]any{
				"chunksReceived": parsed.ChunksReceived,
				"format":         streamFormat(contentType),
			},
		}
		fillTypedExtract(details, typedExtract(options, body, parsed))
		return TypedResult{
			"success": true,
			"message": options.SuccessMessage + "（流式响应）",
			"details": details,
		}
	}

	// 复刻 4542-4592：Content-Type 没声明但正文是 SSE → 成功文案带「（流式响应，Content-Type 未正确设置）」。
	if looksLikeSSE(body) {
		parsed := ParseResponse(TypeOpenAICompatible, body, "text/event-stream")
		details := map[string]any{
			"responseTime": responseTime,
			"rawResponse":  body,
			"streamInfo": map[string]any{
				"chunksReceived": parsed.ChunksReceived,
				"format":         "sse",
			},
		}
		fillTypedExtract(details, typedExtract(options, body, parsed))
		return TypedResult{
			"success": true,
			"message": options.SuccessMessage + "（流式响应，Content-Type 未正确设置）",
			"details": details,
		}
	}

	// 复刻 4594-4620：JSON 解析失败 → 200 + success:false。
	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		return TypedResult{
			"success": false,
			"message": "响应格式无效: 无法解析 JSON",
			"details": map[string]any{
				"responseTime": responseTime,
				"error":        "JSON 解析失败: " + err.Error(),
				"rawResponse":  body,
			},
		}
	}

	// 复刻 4622-4637：成功路径。
	parsed := ParseResponse(TypeOpenAICompatible, body, contentType)
	details := map[string]any{
		"responseTime": responseTime,
		"rawResponse":  body,
	}
	fillTypedExtract(details, typedExtract(options, body, parsed))
	return TypedResult{
		"success": true,
		"message": options.SuccessMessage,
		"details": details,
	}
}

// typedPerform 发一次 POST；带代理且命中「代理重试状态码」且允许回退直连时改直连重试一次。
//
// 复刻 executeProviderApiTest:4379-4436（shouldAttemptDirectRetry 分支）。
func typedPerform(
	ctx context.Context,
	args TypedArgs,
	requestURL string,
	headers map[string]string,
	payload []byte,
	timeout time.Duration,
	factory ClientFactory,
) (*http.Response, string, int64, error) {
	useProxy := strings.TrimSpace(args.ProxyURL) != ""
	doer, err := typedDoer(factory, args, timeout, useProxy)
	if err != nil {
		return nil, "", 0, err
	}

	start := time.Now()
	response, err := typedSend(ctx, doer, requestURL, headers, payload, timeout)
	if err != nil {
		return nil, "", time.Since(start).Milliseconds(), err
	}
	body, readErr := readBody(response)
	responseTime := time.Since(start).Milliseconds()
	if readErr != nil {
		return nil, "", responseTime, readErr
	}

	if useProxy && args.ProxyFallbackToDirect && isProxyRetryStatus(response.StatusCode) {
		directDoer, directErr := typedDoer(factory, args, timeout, false)
		if directErr != nil {
			return nil, "", responseTime, directErr
		}
		fallbackStart := time.Now()
		directResponse, directErr := typedSend(ctx, directDoer, requestURL, headers, payload, timeout)
		if directErr != nil {
			// 复刻 4415-4429：两条错误都进 message（代理错误 + 直连错误）。
			return nil, "", time.Since(fallbackStart).Milliseconds(), fmt.Errorf(
				"代理和直连均失败\n代理错误: HTTP %d\n直连错误: %s", response.StatusCode, directErr.Error())
		}
		directBody, directReadErr := readBody(directResponse)
		if directReadErr != nil {
			return nil, "", time.Since(fallbackStart).Milliseconds(), directReadErr
		}
		return directResponse, directBody, time.Since(fallbackStart).Milliseconds(), nil
	}
	return response, body, responseTime, nil
}

func typedDoer(factory ClientFactory, args TypedArgs, timeout time.Duration, useProxy bool) (Doer, error) {
	if !useProxy {
		return factory("", timeout)
	}
	return factory(args.ProxyURL, timeout)
}

func typedSend(
	ctx context.Context,
	doer Doer,
	requestURL string,
	headers map[string]string,
	payload []byte,
	timeout time.Duration,
) (*http.Response, error) {
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, requestURL, strings.NewReader(string(payload)))
	if err != nil {
		cancel()
		return nil, err
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := doer.Do(request)
	if err != nil {
		cancel()
		return nil, err
	}
	response.Body = &cancelOnCloseBody{ReadCloser: response.Body, stop: cancel}
	return response, nil
}

func typedTimeoutMS(args TypedArgs, options TypedOptions) int64 {
	if options.TimeoutMS != nil && *options.TimeoutMS > 0 {
		return *options.TimeoutMS
	}
	if args.TimeoutMS != nil && *args.TimeoutMS > 0 {
		return *args.TimeoutMS
	}
	return ApiTestTimeout().Milliseconds()
}

func typedExtract(options TypedOptions, body string, parsed ParsedResponse) TypedExtract {
	if options.Extract != nil {
		return options.Extract(body, parsed)
	}
	return TypedExtract{Model: parsed.Model, Usage: parsed.Usage, Content: parsed.Content}
}

func fillTypedExtract(details map[string]any, extracted TypedExtract) {
	if extracted.Model != "" {
		details["model"] = extracted.Model
	}
	if extracted.Usage != nil {
		details["usage"] = extracted.Usage
	}
	if extracted.Content != "" {
		details["content"] = extracted.Content
	}
}

func typedConnectionFailure(startTime time.Time, err error) TypedResult {
	return TypedResult{
		"success": false,
		"message": "连接失败: " + err.Error(),
		"details": map[string]any{
			"responseTime": time.Since(startTime).Milliseconds(),
			"error":        err.Error(),
		},
	}
}

// typedErrorDetail 复刻 4460-4482 的 finalErrorDetail：
// errorDetail（错误 JSON 的可读消息）→ 正文前 200 字 → 「No error details available」。
func typedErrorDetail(body string) string {
	if detail := extractErrorMessage(body); detail != "" {
		return detail
	}
	trimmed := strings.TrimSpace(body)
	if trimmed == "" {
		return "No error details available"
	}
	if len(body) > 200 {
		return body[:200]
	}
	return body
}

func isProxyRetryStatus(status int) bool {
	for _, candidate := range proxyRetryStatusCodes {
		if candidate == status {
			return true
		}
	}
	return false
}

func streamFormat(contentType string) string {
	if strings.Contains(contentType, "application/x-ndjson") {
		return "ndjson"
	}
	return "sse"
}

// looksLikeSSE 复刻 executeProviderApiTest:4550 的正则
// （`/^(event:|data:)|\n\n(event:|data:)/`）。
func looksLikeSSE(body string) bool {
	if strings.HasPrefix(body, "event:") || strings.HasPrefix(body, "data:") {
		return true
	}
	return strings.Contains(body, "\n\nevent:") || strings.Contains(body, "\n\ndata:")
}

// extractErrorMessage 复刻 extractErrorMessage：从错误 JSON 里取可读消息。
func extractErrorMessage(body string) string {
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return ""
	}
	for _, key := range []string{"message", "detail", "error_description"} {
		if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	if nested, ok := payload["error"].(map[string]any); ok {
		if value, ok := nested["message"].(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	if value, ok := payload["error"].(string); ok && strings.TrimSpace(value) != "" {
		return value
	}
	return ""
}

// ExtractFirstTextSnippet 复刻 extractFirstTextSnippet（actions/providers.ts:3731-3771）：
// 四路嗅探（`content[]` / `choices[]` / `output[]` / `candidates[]`），上限 500 字符。
func ExtractFirstTextSnippet(body string) string {
	var payload map[string]any
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		return ""
	}
	limit := typedResponsePreviewLimit

	if content, ok := payload["content"].([]any); ok {
		for _, item := range content {
			entry, ok := item.(map[string]any)
			if !ok || entry["type"] != "text" {
				continue
			}
			if text, ok := entry["text"].(string); ok {
				return clipSnippet(text, limit)
			}
		}
	}

	if choices, ok := payload["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if message, ok := choice["message"].(map[string]any); ok {
				if text, ok := message["content"].(string); ok && text != "" {
					return clipSnippet(text, limit)
				}
			}
		}
	}

	if output, ok := payload["output"].([]any); ok && len(output) > 0 {
		if first, ok := output[0].(map[string]any); ok && first["type"] == "message" {
			if content, ok := first["content"].([]any); ok {
				for _, item := range content {
					entry, ok := item.(map[string]any)
					if !ok || entry["type"] != "output_text" {
						continue
					}
					if text, ok := entry["text"].(string); ok {
						return clipSnippet(text, limit)
					}
				}
			}
		}
	}

	if candidates, ok := payload["candidates"].([]any); ok && len(candidates) > 0 {
		if first, ok := candidates[0].(map[string]any); ok {
			if content, ok := first["content"].(map[string]any); ok {
				if parts, ok := content["parts"].([]any); ok && len(parts) > 0 {
					if part, ok := parts[0].(map[string]any); ok {
						if text, ok := part["text"].(string); ok && text != "" {
							return clipSnippet(text, limit)
						}
					}
				}
			}
		}
	}
	return ""
}

func clipSnippet(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

// TypedRawExtract 复刻四条定型端点共用的 extract：直接读原始 JSON 顶层字段。
//
// Node 的 extract 收的是**原始解析结果**（`JSON.parse(responseText)`），故这里同判：
// `model`（Gemini 用 `modelVersion`）、`usage`（Gemini 用 `usageMetadata`）、
// 正文由 ExtractFirstTextSnippet 嗅探。
func TypedRawExtract(gemini bool) func(body string, parsed ParsedResponse) TypedExtract {
	return func(body string, parsed ParsedResponse) TypedExtract {
		extracted := TypedExtract{Content: ExtractFirstTextSnippet(body)}
		var payload map[string]any
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			return extracted
		}
		modelKey := "model"
		usageKey := "usage"
		if gemini {
			modelKey = "modelVersion"
			usageKey = "usageMetadata"
		}
		if value, ok := payload[modelKey].(string); ok {
			extracted.Model = value
		}
		if usage, ok := payload[usageKey].(map[string]any); ok {
			extracted.Usage = decodeTokenUsage(usage)
		}
		_ = parsed
		return extracted
	}
}

// decodeTokenUsage 把 usage 对象解成 TokenUsage（字段名与 Node 的 usage 对象一致）。
func decodeTokenUsage(usage map[string]any) *TokenUsage {
	raw, err := json.Marshal(usage)
	if err != nil {
		return nil
	}
	var decoded TokenUsage
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil
	}
	return &decoded
}

// TypedConfigAnthropicMessages 复刻 testProviderAnthropicMessages（4679-4703）。
func TypedConfigAnthropicMessages() TypedOptions {
	return TypedOptions{
		Path:         func(string, string) string { return "/v1/messages" },
		DefaultModel: "claude-sonnet-4-6",
		Headers: func(apiKey, providerURL string) map[string]string {
			headers := map[string]string{
				"Content-Type":      "application/json",
				"anthropic-version": "2023-06-01",
			}
			for key, value := range forward.ResolveAnthropicAuthHeaders(apiKey, providerURL, false) {
				headers[key] = value
			}
			return headers
		},
		Body: func(model string) any {
			return map[string]any{
				"model":      model,
				"max_tokens": typedTestMaxTokens,
				"stream":     false,
				"messages": []any{
					map[string]any{"role": "user", "content": typedTestPrompt},
				},
			}
		},
		UserAgent:      "claude-cli/2.1.76 (external, cli)",
		SuccessMessage: "Anthropic Messages API 测试成功",
		Extract:        TypedRawExtract(false),
	}
}

// TypedConfigOpenAIChatCompletions 复刻 testProviderOpenAIChatCompletions（4705-4737）。
func TypedConfigOpenAIChatCompletions() TypedOptions {
	return TypedOptions{
		Path:         func(string, string) string { return "/v1/chat/completions" },
		DefaultModel: "gpt-5.5",
		Headers: func(apiKey, _ string) map[string]string {
			return map[string]string{
				"Content-Type":  "application/json",
				"Authorization": "Bearer " + apiKey,
			}
		},
		Body: func(model string) any {
			return map[string]any{
				"model":      model,
				"max_tokens": typedTestMaxTokens,
				"messages": []any{
					map[string]any{"role": "developer", "content": "你是一个有帮助的助手。"},
					map[string]any{"role": "user", "content": "你好"},
				},
			}
		},
		UserAgent:      "OpenAI/NodeJS/3.2.1",
		SuccessMessage: "OpenAI Chat Completions API 测试成功",
		Extract:        TypedRawExtract(false),
	}
}

// TypedConfigOpenAIResponses 复刻 testProviderOpenAIResponses（4739-4780）。
func TypedConfigOpenAIResponses() TypedOptions {
	return TypedOptions{
		Path:         func(string, string) string { return "/v1/responses" },
		DefaultModel: "gpt-5.5",
		Headers: func(apiKey, _ string) map[string]string {
			return map[string]string{
				"Content-Type":  "application/json",
				"Authorization": "Bearer " + apiKey,
			}
		},
		Body: func(model string) any {
			// 不含 max_output_tokens（部分中转不支持）；input 必须带 type 字段。
			return map[string]any{
				"model": model,
				"input": []any{
					map[string]any{
						"type": "message",
						"role": "user",
						"content": []any{
							map[string]any{"type": "input_text", "text": typedTestPrompt},
						},
					},
				},
			}
		},
		UserAgent:      "codex_cli_rs/0.63.0",
		SuccessMessage: "OpenAI Responses API 测试成功",
		Extract:        TypedRawExtract(false),
	}
}

// TypedConfigGemini 复刻 testProviderGemini 首次尝试（4782-4889）：只走 header 认证。
func TypedConfigGemini(geminiBearerAuth bool) TypedOptions {
	timeout := int64(typedGeminiTimeoutMS)
	return TypedOptions{
		Path: func(model, _ string) string {
			return "/v1beta/models/" + model + ":generateContent"
		},
		DefaultModel: "gemini-2.5-pro",
		Headers: func(apiKey, _ string) map[string]string {
			headers := map[string]string{
				"Content-Type":      "application/json",
				"x-goog-api-client": "google-genai-sdk/1.30.0 gl-node/v24.11.0",
			}
			if geminiBearerAuth {
				headers["Authorization"] = "Bearer " + apiKey
			} else {
				headers["x-goog-api-key"] = apiKey
			}
			return headers
		},
		Body: func(string) any {
			return map[string]any{
				"contents": []any{
					map[string]any{"role": "user", "parts": []any{map[string]any{"text": typedTestPrompt}}},
				},
				"generationConfig": map[string]any{
					"temperature":     0,
					"maxOutputTokens": typedTestMaxTokens,
					"thinkingConfig":  map[string]any{"thinkingBudget": 0},
				},
			}
		},
		UserAgent:      "GeminiCLI/v24.11.0 (linux; x64)",
		SuccessMessage: "Gemini API 测试成功",
		TimeoutMS:      &timeout,
		Extract:        TypedRawExtract(true),
	}
}

// TypedConfigGeminiURLParam 复刻 testProviderGemini 第二次尝试（4890-4947）：
// key 同时进 URL 查询串与 header，并在成功文案后追加 ` [FALLBACK:URL_PARAM]`。
func TypedConfigGeminiURLParam() TypedOptions {
	options := TypedConfigGemini(false)
	options.Path = func(model, apiKey string) string {
		return "/v1beta/models/" + model + ":generateContent?key=" + encodeURIComponent(apiKey)
	}
	options.SuccessMessage = "Gemini API 测试成功 (URL 认证)"
	options.Headers = func(apiKey, _ string) map[string]string {
		return map[string]string{
			"Content-Type":      "application/json",
			"x-goog-api-client": "google-genai-sdk/1.30.0 gl-node/v24.11.0",
			"x-goog-api-key":    apiKey,
		}
	}
	return options
}

// GeminiAuthErrorFromResult 判定定型 Gemini 的失败是否属认证失败（复刻 testProviderGemini
// 对 message 里 `HTTP 401`/`HTTP 403` 的探测），决定是否走 URL 参数二次尝试。
func GeminiAuthErrorFromResult(result TypedResult) bool {
	message, _ := result["message"].(string)
	return strings.Contains(message, "HTTP 401") || strings.Contains(message, "HTTP 403")
}

// encodeURIComponent 复刻 JS 的 encodeURIComponent（Go 的 QueryEscape 会把空格编成 `+`）。
func encodeURIComponent(value string) string {
	escaped := url.QueryEscape(value)
	escaped = strings.ReplaceAll(escaped, "+", "%20")
	return escaped
}
