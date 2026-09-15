package providertest

import (
	"net/url"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
)

// prompts.go 复刻 `src/lib/provider-testing/utils/test-prompts.ts`：
// 协议级兜底默认值（预设不可用时的 URL/头/体）。
//
// 注意与 presets.ts 的关系：真正执行时**优先**用预设模板；本文件是兜底。

// UserAgents 复刻 USER_AGENTS（test-prompts.ts:11-18）。
var UserAgents = map[ProviderType]string{
	TypeClaude:           claudeCLIUserAgent,
	TypeClaudeAuth:       claudeCLIUserAgent,
	TypeCodex:            "Codex-CLI/1.0",
	TypeOpenAICompatible: "OpenAI-Compatible/2026.04",
	TypeGemini:           geminiCLIUserAgent,
	TypeGeminiCLI:        geminiCLIUserAgent,
}

// BaseHeaders 复刻 BASE_HEADERS（test-prompts.ts:20-25）。
var BaseHeaders = map[string]string{
	"Accept":          "application/json, text/event-stream",
	"Accept-Language": "en-US,en;q=0.9",
	"Accept-Encoding": "gzip, deflate, br, zstd",
	"Connection":      "keep-alive",
}

// TestBodies 是四类协议级兜底请求体（test-prompts.ts:27-93 的 CLAUDE/CODEX/OPENAI/GEMINI_TEST_BODY）。
var (
	claudeTestBody = map[string]any{
		"model":      "claude-haiku-4-5-20251001",
		"max_tokens": 20,
		"stream":     true,
		"metadata":   map[string]any{"user_id": "cch_probe_test"},
		"system": []any{map[string]any{
			"type": "text",
			"text": "You are Claude Code, Anthropic's official CLI for Claude.",
		}},
		"messages": []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "text", "text": "ping, please reply 'pong'"}},
		}},
	}
	codexTestBody = map[string]any{
		"model": "gpt-5.5",
		"instructions": "You are Codex, based on GPT-5. You are running as a coding agent " +
			"in the Codex CLI on a user's computer.",
		"input": []any{map[string]any{
			"type":    "message",
			"role":    "user",
			"content": []any{map[string]any{"type": "input_text", "text": "ping"}},
		}},
		"tools":               []any{},
		"tool_choice":         "auto",
		"parallel_tool_calls": false,
		"reasoning":           map[string]any{"effort": "low", "summary": "auto"},
		"store":               false,
		"stream":              true,
	}
	openAITestBody = map[string]any{
		"model": "gpt-4.1-mini",
		"messages": []any{
			map[string]any{"role": "system", "content": "You are an echo bot. Reply with exactly pong."},
			map[string]any{"role": "user", "content": "ping"},
		},
		"max_tokens": 20,
		"stream":     false,
	}
	geminiTestBody = map[string]any{
		"contents": []any{map[string]any{
			"role":  "user",
			"parts": []any{map[string]any{"text": "ping"}},
		}},
		"generationConfig": map[string]any{
			"temperature":     0,
			"maxOutputTokens": 256,
			"thinkingConfig":  map[string]any{"thinkingBudget": 0},
		},
	}
)

// DefaultModels 复刻 DEFAULT_MODELS。
var DefaultModels = map[ProviderType]string{
	TypeClaude:           "claude-haiku-4-5-20251001",
	TypeClaudeAuth:       "claude-haiku-4-5-20251001",
	TypeCodex:            "gpt-5.5",
	TypeOpenAICompatible: "gpt-4.1-mini",
	TypeGemini:           "gemini-2.5-flash",
	TypeGeminiCLI:        "gemini-2.5-flash",
}

// DefaultSuccessContains 复刻 DEFAULT_SUCCESS_CONTAINS（六类型都是 "pong"）。
var DefaultSuccessContains = map[ProviderType]string{
	TypeClaude:           "pong",
	TypeClaudeAuth:       "pong",
	TypeCodex:            "pong",
	TypeOpenAICompatible: "pong",
	TypeGemini:           "pong",
	TypeGeminiCLI:        "pong",
}

// APIEndpoints 复刻 API_ENDPOINTS。
var APIEndpoints = map[ProviderType]string{
	TypeClaude:           "/v1/messages",
	TypeClaudeAuth:       "/v1/messages",
	TypeCodex:            "/v1/responses",
	TypeOpenAICompatible: "/v1/chat/completions",
	TypeGemini:           "/v1beta/models/{model}:generateContent",
	TypeGeminiCLI:        "/v1beta/models/{model}:generateContent",
}

// TestBody 复刻 getTestBody：按类型取兜底体，并按需覆写 model（Gemini 不带 model）。
func TestBody(providerType ProviderType, model string) (map[string]any, error) {
	targetModel := model
	if targetModel == "" {
		targetModel = DefaultModels[providerType]
	}
	switch providerType {
	case TypeClaude, TypeClaudeAuth:
		body := deepCopyMap(claudeTestBody)
		body["model"] = targetModel
		return body, nil
	case TypeCodex:
		body := deepCopyMap(codexTestBody)
		body["model"] = targetModel
		return body, nil
	case TypeOpenAICompatible:
		body := deepCopyMap(openAITestBody)
		body["model"] = targetModel
		return body, nil
	case TypeGemini, TypeGeminiCLI:
		return deepCopyMap(geminiTestBody), nil
	default:
		return nil, &UnsupportedProviderTypeError{ProviderType: providerType}
	}
}

// UnsupportedProviderTypeError 对应 Node 的 `Unsupported provider type: ${providerType}`。
type UnsupportedProviderTypeError struct{ ProviderType ProviderType }

func (e *UnsupportedProviderTypeError) Error() string {
	return "Unsupported provider type: " + string(e.ProviderType)
}

// TestHeaders 复刻 getTestHeaders（含 Anthropic 认证解析、Gemini 的 Bearer/x-goog-api-key 二选一）。
func TestHeaders(
	providerType ProviderType,
	apiKey string,
	providerURL string,
	userAgent string,
	extraHeaders map[string]string,
	geminiBearerAuth bool,
) (map[string]string, error) {
	headers := make(map[string]string, len(BaseHeaders)+4)
	for key, value := range BaseHeaders {
		headers[key] = value
	}
	agent := userAgent
	if agent == "" {
		agent = UserAgents[providerType]
	}
	headers["User-Agent"] = agent

	switch providerType {
	case TypeClaude, TypeClaudeAuth:
		headers["anthropic-version"] = "2023-06-01"
		headers["content-type"] = "application/json"
		// 复用数据面的同一实现（Node 也是同一个 resolveAnthropicAuthHeaders）。
		for key, value := range forward.ResolveAnthropicAuthHeaders(apiKey, providerURL, providerType == TypeClaudeAuth) {
			headers[key] = value
		}
	case TypeCodex:
		headers["content-type"] = "application/json"
		headers["openai-beta"] = "responses=experimental"
		headers["Authorization"] = "Bearer " + apiKey
	case TypeOpenAICompatible:
		headers["content-type"] = "application/json"
		headers["Authorization"] = "Bearer " + apiKey
	case TypeGemini, TypeGeminiCLI:
		headers["content-type"] = "application/json"
		headers["x-goog-api-client"] = "google-genai-sdk/1.30.0 gl-node/v24.11.0"
		if geminiBearerAuth {
			headers["Authorization"] = "Bearer " + apiKey
		} else {
			headers["x-goog-api-key"] = apiKey
		}
	default:
		return nil, &UnsupportedProviderTypeError{ProviderType: providerType}
	}

	for key, value := range extraHeaders {
		headers[key] = value
	}
	return headers, nil
}

// TestURL 复刻 getTestUrl：把 path 拼到 baseURL 上，**复用数据面的 URL 语义**
// （dial.BuildUpstreamURL，即数据面的 buildProxyUrl 对应实现），避开探测面另起一套拼接规则。
//
// 为何必须走 dial：探测面的 URL 就是数据面即将发出的 URL，两边不一致会出现「探活通过、真实
// 请求 404」（或反之）。供应商基址停在版本根（如 `…/api/plan/v3`）时，裸拼
// `base + "/v1/chat/completions"` 会多出一个版本段，上游 404（2026-09-15 ARK 生产实证）。
func TestURL(baseURL string, providerType ProviderType, model string, pathOverride string) (string, error) {
	targetModel := model
	if targetModel == "" {
		targetModel = DefaultModels[providerType]
	}
	path := pathOverride
	if path == "" {
		path = APIEndpoints[providerType]
	}
	if path == "" {
		return "", &UnsupportedProviderTypeError{ProviderType: providerType}
	}
	path = strings.ReplaceAll(path, "{model}", targetModel)
	// pathOverride 可能自带查询串（预设模板的 `?beta=true` 之类），拆开交给 dial 原样带上。
	rawPath, rawQuery := path, ""
	if index := strings.Index(path, "?"); index >= 0 {
		rawPath, rawQuery = path[:index], path[index+1:]
	}
	return dial.BuildUpstreamURLWithQuery(strings.TrimSpace(baseURL), rawPath, rawQuery)
}

// versionedFallbackPaths 复刻 OPENAI_VERSIONED_FALLBACK_PATHS（test-prompts.ts:180-184）。
var versionedFallbackPaths = [][2]string{
	{"/v1/responses", "/responses"},
	{"/v1/chat/completions", "/chat/completions"},
	{"/v1/models", "/models"},
}

// VersionlessOpenAIFallbackURL 复刻 getVersionlessOpenAiFallbackUrl：
// 命中「最靠近路径尾部」的 versioned 段时改写为无版本路径，否则返回空串。
func VersionlessOpenAIFallbackURL(requestURL string) string {
	parsed, err := url.Parse(requestURL)
	if err != nil {
		return ""
	}
	path := parsed.Path
	bestIndex := -1
	bestPath := ""
	for _, pair := range versionedFallbackPaths {
		index := strings.LastIndex(path, pair[0])
		if index == -1 {
			continue
		}
		suffix := path[index+len(pair[0]):]
		if suffix != "" && !strings.HasPrefix(suffix, "/") {
			continue
		}
		candidate := path[:index] + pair[1] + suffix
		if candidate == path {
			continue
		}
		if bestIndex == -1 || index > bestIndex {
			bestIndex = index
			bestPath = candidate
		}
	}
	if bestIndex == -1 {
		return ""
	}
	parsed.Path = bestPath
	return parsed.String()
}
