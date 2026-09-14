package providertest

import (
	"encoding/json"
	"strings"
)

// parse.go 复刻 `src/lib/provider-testing/parsers/*.ts` 与
// `utils/sse-collector.ts` 的解析语义，以及 `validators/*.ts` 的两级判定。

// TestStatus 复刻 TestStatus。
type TestStatus string

// TestSubStatus 复刻 TestSubStatus。
type TestSubStatus string

// 三档状态与子状态（逐字对应 types.ts:18-44）。
const (
	StatusGreen  TestStatus = "green"
	StatusYellow TestStatus = "yellow"
	StatusRed    TestStatus = "red"

	SubStatusSuccess         TestSubStatus = "success"
	SubStatusSlowLatency     TestSubStatus = "slow_latency"
	SubStatusRateLimit       TestSubStatus = "rate_limit"
	SubStatusServerError     TestSubStatus = "server_error"
	SubStatusClientError     TestSubStatus = "client_error"
	SubStatusAuthError       TestSubStatus = "auth_error"
	SubStatusInvalidRequest  TestSubStatus = "invalid_request"
	SubStatusNetworkError    TestSubStatus = "network_error"
	SubStatusContentMismatch TestSubStatus = "content_mismatch"
)

// TokenUsage 复刻 TokenUsage。
type TokenUsage struct {
	InputTokens                int64  `json:"inputTokens"`
	OutputTokens               int64  `json:"outputTokens"`
	CacheCreationInputTokens   *int64 `json:"cacheCreationInputTokens,omitempty"`
	CacheReadInputTokens       *int64 `json:"cacheReadInputTokens,omitempty"`
	CacheCreation5mInputTokens *int64 `json:"cacheCreation5mInputTokens,omitempty"`
	CacheCreation1hInputTokens *int64 `json:"cacheCreation1hInputTokens,omitempty"`
}

// ParsedResponse 复刻 ParsedResponse。
type ParsedResponse struct {
	Content        string
	Model          string
	Usage          *TokenUsage
	IsStreaming    bool
	ChunksReceived int
}

// ParseResponse 复刻 parseResponse：按 providerType 选解析器。
func ParseResponse(providerType ProviderType, body string, contentType string) ParsedResponse {
	switch providerType {
	case TypeClaude, TypeClaudeAuth:
		return parseAnthropicResponse(body, contentType)
	case TypeCodex:
		return parseCodexResponse(body, contentType)
	case TypeGemini, TypeGeminiCLI:
		return parseGeminiResponse(body)
	default:
		return parseOpenAIResponse(body, contentType)
	}
}

// IsSSEResponse 复刻 isSSEResponse：content-type 命中，或正文同时含 "event:" 与 "data:"。
func IsSSEResponse(body string, contentType string) bool {
	if strings.Contains(contentType, "text/event-stream") || strings.Contains(contentType, "text/x-event-stream") {
		return true
	}
	return strings.Contains(body, "event:") && strings.Contains(body, "data:")
}

// isNDJSONResponse 复刻 codex-parser 的 isNDJSONResponse：content-type 命中，
// 或正文有多行且前两行都能解析成 JSON。
func isNDJSONResponse(body string, contentType string) bool {
	if strings.Contains(contentType, "application/x-ndjson") {
		return true
	}
	lines := make([]string, 0, 2)
	for _, line := range strings.Split(body, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
			if len(lines) == 2 {
				break
			}
		}
	}
	if len(lines) < 2 {
		return false
	}
	for _, line := range lines {
		var probe any
		if json.Unmarshal([]byte(line), &probe) != nil {
			return false
		}
	}
	return true
}

// JSON 解析用结构（字段名与 Node 的 interface 逐字对应）。
type anthropicResponse struct {
	Model   string `json:"model"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Usage *struct {
		InputTokens              *int64 `json:"input_tokens"`
		OutputTokens             *int64 `json:"output_tokens"`
		CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
		CacheCreation            *struct {
			Ephemeral5mInputTokens *int64 `json:"ephemeral_5m_input_tokens"`
			Ephemeral1hInputTokens *int64 `json:"ephemeral_1h_input_tokens"`
		} `json:"cache_creation"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func parseAnthropicResponse(body string, contentType string) ParsedResponse {
	if IsSSEResponse(body, contentType) {
		return parseSSEStream(body)
	}
	var data anthropicResponse
	if err := json.Unmarshal([]byte(body), &data); err != nil {
		return rawFallback(body)
	}
	if data.Error != nil {
		message := data.Error.Message
		if message == "" {
			message = "Unknown error"
		}
		return ParsedResponse{Content: message}
	}
	texts := make([]string, 0, len(data.Content))
	for _, item := range data.Content {
		if item.Type == "text" {
			texts = append(texts, item.Text)
		}
	}
	var usage *TokenUsage
	if data.Usage != nil {
		usage = &TokenUsage{
			InputTokens:              derefInt64(data.Usage.InputTokens),
			OutputTokens:             derefInt64(data.Usage.OutputTokens),
			CacheCreationInputTokens: data.Usage.CacheCreationInputTokens,
			CacheReadInputTokens:     data.Usage.CacheReadInputTokens,
		}
		if data.Usage.CacheCreation != nil {
			usage.CacheCreation5mInputTokens = data.Usage.CacheCreation.Ephemeral5mInputTokens
			usage.CacheCreation1hInputTokens = data.Usage.CacheCreation.Ephemeral1hInputTokens
			// Node：cache_creation_input_tokens 缺失但 5m/1h 存在时，两者相加补齐。
			if usage.CacheCreationInputTokens == nil &&
				(derefInt64(usage.CacheCreation5mInputTokens) != 0 || derefInt64(usage.CacheCreation1hInputTokens) != 0) {
				sum := derefInt64(usage.CacheCreation5mInputTokens) + derefInt64(usage.CacheCreation1hInputTokens)
				usage.CacheCreationInputTokens = &sum
			}
		}
	}
	return ParsedResponse{Content: strings.Join(texts, ""), Model: data.Model, Usage: usage}
}

type openAIResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message *struct {
			Content string `json:"content"`
		} `json:"message"`
		Text string `json:"text"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     *int64 `json:"prompt_tokens"`
		CompletionTokens *int64 `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func parseOpenAIResponse(body string, contentType string) ParsedResponse {
	if IsSSEResponse(body, contentType) {
		return parseSSEStream(body)
	}
	var data openAIResponse
	if err := json.Unmarshal([]byte(body), &data); err != nil {
		return rawFallback(body)
	}
	if data.Error != nil {
		message := data.Error.Message
		if message == "" {
			message = "Unknown error"
		}
		return ParsedResponse{Content: message}
	}
	texts := make([]string, 0, len(data.Choices))
	for _, choice := range data.Choices {
		if choice.Message != nil && choice.Message.Content != "" {
			texts = append(texts, choice.Message.Content)
		} else if choice.Text != "" {
			texts = append(texts, choice.Text)
		}
	}
	var usage *TokenUsage
	if data.Usage != nil {
		usage = &TokenUsage{
			InputTokens:  derefInt64(data.Usage.PromptTokens),
			OutputTokens: derefInt64(data.Usage.CompletionTokens),
		}
	}
	return ParsedResponse{Content: strings.Join(texts, ""), Model: data.Model, Usage: usage}
}

type codexResponse struct {
	Model  string `json:"model"`
	Output []struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	Usage *struct {
		InputTokens  *int64 `json:"input_tokens"`
		OutputTokens *int64 `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func parseCodexResponse(body string, contentType string) ParsedResponse {
	if IsSSEResponse(body, contentType) {
		return parseSSEStream(body)
	}
	if isNDJSONResponse(body, contentType) {
		return parseNDJSONStream(body)
	}
	var data codexResponse
	if err := json.Unmarshal([]byte(body), &data); err != nil {
		return rawFallback(body)
	}
	if data.Error != nil {
		message := data.Error.Message
		if message == "" {
			message = "Unknown error"
		}
		return ParsedResponse{Content: message}
	}
	texts := make([]string, 0, len(data.Output))
	for _, item := range data.Output {
		for _, part := range item.Content {
			// Node：output_text 优先，其余「有 text 就收」。
			if part.Text != "" {
				texts = append(texts, part.Text)
			}
		}
	}
	var usage *TokenUsage
	if data.Usage != nil {
		usage = &TokenUsage{
			InputTokens:  derefInt64(data.Usage.InputTokens),
			OutputTokens: derefInt64(data.Usage.OutputTokens),
		}
	}
	return ParsedResponse{Content: strings.Join(texts, ""), Model: data.Model, Usage: usage}
}

type geminiResponse struct {
	Candidates []struct {
		Content *struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount     *int64 `json:"promptTokenCount"`
		CandidatesTokenCount *int64 `json:"candidatesTokenCount"`
	} `json:"usageMetadata"`
	ModelVersion string `json:"modelVersion"`
	Error        *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// parseGeminiResponse：Gemini 分支**不做 SSE 判定**（Node 的 gemini-parser 只解 JSON）。
func parseGeminiResponse(body string) ParsedResponse {
	var data geminiResponse
	if err := json.Unmarshal([]byte(body), &data); err != nil {
		return rawFallback(body)
	}
	if data.Error != nil {
		message := data.Error.Message
		if message == "" {
			message = "Unknown error"
		}
		return ParsedResponse{Content: message}
	}
	texts := make([]string, 0, len(data.Candidates))
	for _, candidate := range data.Candidates {
		if candidate.Content == nil {
			continue
		}
		for _, part := range candidate.Content.Parts {
			if part.Text != "" {
				texts = append(texts, part.Text)
			}
		}
	}
	var usage *TokenUsage
	if data.UsageMetadata != nil {
		usage = &TokenUsage{
			InputTokens:  derefInt64(data.UsageMetadata.PromptTokenCount),
			OutputTokens: derefInt64(data.UsageMetadata.CandidatesTokenCount),
		}
	}
	return ParsedResponse{Content: strings.Join(texts, ""), Model: data.ModelVersion, Usage: usage}
}

// rawFallback 复刻各解析器 catch 分支：正文截断 500 字符。
func rawFallback(body string) ParsedResponse {
	if len(body) > 500 {
		return ParsedResponse{Content: body[:500]}
	}
	return ParsedResponse{Content: body}
}

// parseSSEStream 复刻 parseSSEStream：逐 `data:` 行取 delta/内容，带四级正文兜底与 usage 提取。
func parseSSEStream(body string) ParsedResponse {
	texts := make([]string, 0, 16)
	fallbackTexts := make([]string, 0, 1)
	fallbackPriority := 1 << 30
	assignFallback := func(priority int, next []string) {
		if len(next) == 0 || fallbackPriority < priority {
			return
		}
		fallbackPriority = priority
		fallbackTexts = append(fallbackTexts[:0], next...)
	}

	model := ""
	var usage *TokenUsage
	chunks := 0

	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		chunks++

		var obj map[string]any
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			continue
		}
		eventType, _ := obj["type"].(string)
		response, _ := obj["response"].(map[string]any)

		if model == "" {
			if value, ok := obj["model"].(string); ok {
				model = value
			} else if response != nil {
				if value, ok := response["model"].(string); ok {
					model = value
				}
			}
		}

		// Anthropic：delta.text
		if delta, ok := obj["delta"].(map[string]any); ok {
			if text, ok := delta["text"].(string); ok && text != "" {
				texts = append(texts, text)
			}
			// OpenAI：choices[].delta.content
		}
		if choices, ok := obj["choices"].([]any); ok {
			for _, choice := range choices {
				choiceMap, ok := choice.(map[string]any)
				if !ok {
					continue
				}
				if delta, ok := choiceMap["delta"].(map[string]any); ok {
					if content, ok := delta["content"].(string); ok && content != "" {
						texts = append(texts, content)
					}
				}
			}
		}
		// Codex：response.output_text.delta（delta 是字符串）
		if eventType == "response.output_text.delta" {
			if text, ok := obj["delta"].(string); ok && text != "" {
				texts = append(texts, text)
			}
		}
		if eventType == "response.output_text.done" {
			if text, ok := obj["text"].(string); ok {
				assignFallback(1, []string{text})
			}
		}
		if eventType == "response.content_part.done" {
			if part, ok := obj["part"].(map[string]any); ok {
				if text, ok := part["text"].(string); ok {
					assignFallback(2, []string{text})
				}
			}
		}
		if eventType == "response.output_item.done" {
			if item, ok := obj["item"].(map[string]any); ok {
				if content, ok := item["content"].([]any); ok {
					assignFallback(3, collectTexts(content))
				}
			}
		}
		if eventType == "response.completed" && response != nil {
			if output, ok := response["output"].([]any); ok {
				collected := make([]string, 0, len(output))
				for _, item := range output {
					if itemMap, ok := item.(map[string]any); ok {
						if content, ok := itemMap["content"].([]any); ok {
							collected = append(collected, collectTexts(content)...)
						}
					}
				}
				assignFallback(4, collected)
			}
		}
		// Codex：顶层 output[].content[].text
		if output, ok := obj["output"].([]any); ok {
			for _, item := range output {
				if itemMap, ok := item.(map[string]any); ok {
					if content, ok := itemMap["content"].([]any); ok {
						texts = append(texts, collectTexts(content)...)
					}
				}
			}
		}

		// usage：Anthropic 的 message_delta
		if eventType == "message_delta" {
			if usageMap, ok := obj["usage"].(map[string]any); ok {
				if value, ok := numberField(usageMap, "output_tokens"); ok && value != 0 {
					usage = &TokenUsage{InputTokens: 0, OutputTokens: value}
				}
			}
		}
		if usageMap, ok := obj["usage"].(map[string]any); ok {
			usage = &TokenUsage{
				InputTokens:  numberOrZero(usageMap, "prompt_tokens"),
				OutputTokens: numberOrZero(usageMap, "completion_tokens"),
			}
		}
		if response != nil {
			if usageMap, ok := response["usage"].(map[string]any); ok {
				usage = &TokenUsage{
					InputTokens:  numberOrZero(usageMap, "input_tokens"),
					OutputTokens: numberOrZero(usageMap, "output_tokens"),
				}
			}
		}
	}

	content := strings.Join(texts, "")
	if content == "" {
		content = strings.Join(fallbackTexts, "")
	}
	return ParsedResponse{Content: content, Model: model, Usage: usage, IsStreaming: true, ChunksReceived: chunks}
}

// parseNDJSONStream 复刻 parseNDJSONStream（Codex 的 NDJSON 形态）。
func parseNDJSONStream(body string) ParsedResponse {
	texts := make([]string, 0, 16)
	model := ""
	var usage *TokenUsage
	chunks := 0
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
			continue
		}
		chunks++
		if model == "" {
			if value, ok := obj["model"].(string); ok {
				model = value
			}
		}
		if eventType, _ := obj["type"].(string); eventType == "response.output_text.delta" {
			if text, ok := obj["delta"].(string); ok && text != "" {
				texts = append(texts, text)
			}
		}
		if output, ok := obj["output"].([]any); ok {
			for _, item := range output {
				if itemMap, ok := item.(map[string]any); ok {
					if content, ok := itemMap["content"].([]any); ok {
						texts = append(texts, collectTexts(content)...)
					}
				}
			}
		}
		if usageMap, ok := obj["usage"].(map[string]any); ok {
			usage = &TokenUsage{
				InputTokens:  numberOrZero(usageMap, "input_tokens"),
				OutputTokens: numberOrZero(usageMap, "output_tokens"),
			}
		}
	}
	return ParsedResponse{Content: strings.Join(texts, ""), Model: model, Usage: usage, IsStreaming: true, ChunksReceived: chunks}
}

func collectTexts(items []any) []string {
	collected := make([]string, 0, len(items))
	for _, item := range items {
		if itemMap, ok := item.(map[string]any); ok {
			if text, ok := itemMap["text"].(string); ok && text != "" {
				collected = append(collected, text)
			}
		}
	}
	return collected
}

func numberField(source map[string]any, key string) (int64, bool) {
	value, ok := source[key].(float64)
	if !ok {
		return 0, false
	}
	return int64(value), true
}

func numberOrZero(source map[string]any, key string) int64 {
	value, _ := numberField(source, key)
	return value
}

func derefInt64(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

// HttpValidationResult 复刻 classifyHttpStatus 的结果。
type HttpValidationResult struct {
	Status    TestStatus
	SubStatus TestSubStatus
}

// ClassifyHTTPStatus 复刻 classifyHttpStatus（relay-pulse 的八档判定）。
func ClassifyHTTPStatus(statusCode int, latencyMS int64, slowThresholdMS int64) HttpValidationResult {
	switch {
	case statusCode >= 200 && statusCode < 300:
		if latencyMS > slowThresholdMS {
			return HttpValidationResult{StatusYellow, SubStatusSlowLatency}
		}
		return HttpValidationResult{StatusGreen, SubStatusSuccess}
	case statusCode >= 300 && statusCode < 400:
		return HttpValidationResult{StatusGreen, SubStatusSuccess}
	case statusCode == 401 || statusCode == 403:
		return HttpValidationResult{StatusRed, SubStatusAuthError}
	case statusCode == 400:
		return HttpValidationResult{StatusRed, SubStatusInvalidRequest}
	case statusCode == 429:
		return HttpValidationResult{StatusRed, SubStatusRateLimit}
	case statusCode >= 500:
		return HttpValidationResult{StatusRed, SubStatusServerError}
	default:
		return HttpValidationResult{StatusRed, SubStatusClientError}
	}
}

// ContentValidationResult 复刻 evaluateContentValidation 的结果。
type ContentValidationResult struct {
	Status        TestStatus
	SubStatus     TestSubStatus
	ContentPassed bool
}

// EvaluateContentValidation 复刻 evaluateContentValidation（五条规则，顺序不可换）。
func EvaluateContentValidation(
	baseStatus TestStatus,
	baseSubStatus TestSubStatus,
	responseBody string,
	successContains string,
) ContentValidationResult {
	if successContains == "" {
		return ContentValidationResult{baseStatus, baseSubStatus, true}
	}
	if baseStatus == StatusRed {
		return ContentValidationResult{baseStatus, baseSubStatus, false}
	}
	if baseSubStatus == SubStatusRateLimit {
		return ContentValidationResult{baseStatus, baseSubStatus, false}
	}
	if strings.TrimSpace(responseBody) == "" {
		return ContentValidationResult{StatusRed, SubStatusContentMismatch, false}
	}
	if !strings.Contains(responseBody, successContains) {
		return ContentValidationResult{StatusRed, SubStatusContentMismatch, false}
	}
	return ContentValidationResult{baseStatus, baseSubStatus, true}
}
