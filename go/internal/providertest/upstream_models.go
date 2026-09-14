package providertest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/forward"
)

// UpstreamModelsTimeoutDefault 复刻 actions/providers.ts:5370 的 UPSTREAM_FETCH_TIMEOUT_MS。
const UpstreamModelsTimeoutDefault = 10 * time.Second

// FetchUpstreamModels 复刻 actions/providers.ts:5453-5494 的 fetchUpstreamModels：
// 按供应商类型选不同的上游模型列表 API，返回**升序去重前**的模型 id 列表。
//
// 三条分支（与 Node 逐条对应）：
//   - claude / claude-auth -> GET {base}/v1/models，Anthropic 认证（复用 forward.ResolveAnthropicAuthHeaders）
//   - gemini / gemini-cli  -> GET {base}/v1beta/models?pageSize=100，x-goog-api-key；401/403 时带 key 查询参数重试
//   - codex / openai-compatible -> GET {base}/v1/models，Authorization: Bearer
//
// 与 Node 的已知差异（登记，非静默）：
//  1. **Gemini JSON 凭据换取 OAuth access token** 未移植（Node 的 `GeminiAuth.getAccessToken`/`isJson`）；
//     本实现一律发 `x-goog-api-key`。JSON 凭据的供应商在探测时会得到 401，与 Node 不同。
//  2. 排序用 `sort.Strings`（按字节），Node 的 `Array.prototype.sort()` 按 UTF-16 码元；
//     模型 id 全为 ASCII 时两者一致，非 ASCII 时理论上有序差。
func FetchUpstreamModels(
	ctx context.Context,
	client *http.Client,
	providerType ProviderType,
	baseURL string,
	apiKey string,
) ([]string, error) {
	normalized := strings.TrimSuffix(strings.TrimSpace(baseURL), "/")

	switch providerType {
	case TypeClaude, TypeClaudeAuth:
		return fetchAnthropicModels(ctx, client, providerType, normalized, apiKey)
	case TypeGemini, TypeGeminiCLI:
		return fetchGeminiModels(ctx, client, normalized, apiKey)
	default:
		return fetchOpenAIModels(ctx, client, normalized, apiKey)
	}
}

func fetchOpenAIModels(ctx context.Context, client *http.Client, baseURL, apiKey string) ([]string, error) {
	response, body, err := doJSONRequest(ctx, client, http.MethodGet, baseURL+"/v1/models", map[string]string{
		"Authorization": "Bearer " + apiKey,
	})
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("API 返回错误: HTTP %d", response.StatusCode)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Data == nil {
		return nil, fmt.Errorf("响应格式无效：缺少 data 数组")
	}
	models := make([]string, 0, len(payload.Data))
	for _, item := range payload.Data {
		models = append(models, item.ID)
	}
	sort.Strings(models)
	return models, nil
}

func fetchAnthropicModels(ctx context.Context, client *http.Client, providerType ProviderType, baseURL, apiKey string) ([]string, error) {
	headers := forward.ResolveAnthropicAuthHeaders(apiKey, baseURL, providerType == TypeClaudeAuth)
	response, body, err := doJSONRequest(ctx, client, http.MethodGet, baseURL+"/v1/models", headers)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("API 返回错误: HTTP %d", response.StatusCode)
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Data == nil {
		return nil, fmt.Errorf("响应格式无效：缺少 data 数组")
	}
	models := make([]string, 0, len(payload.Data))
	for _, item := range payload.Data {
		models = append(models, item.ID)
	}
	sort.Strings(models)
	return models, nil
}

func fetchGeminiModels(ctx context.Context, client *http.Client, baseURL, apiKey string) ([]string, error) {
	requestURL := baseURL + "/v1beta/models?pageSize=100"
	headers := map[string]string{"x-goog-api-key": apiKey}

	response, body, err := doJSONRequest(ctx, client, http.MethodGet, requestURL, headers)
	if err != nil {
		return nil, err
	}

	// 复刻 Node 的「header 认证失败则改用 URL 查询参数再试一次」（actions/providers.ts:5566-5575）。
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		retryURL := baseURL + "/v1beta/models?pageSize=100&key=" + url.QueryEscape(apiKey)
		response, body, err = doJSONRequest(ctx, client, http.MethodGet, retryURL, headers)
		if err != nil {
			return nil, err
		}
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("API 返回错误: HTTP %d", response.StatusCode)
	}

	var payload struct {
		Models []struct {
			Name                       string   `json:"name"`
			SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil || payload.Models == nil {
		return nil, fmt.Errorf("响应格式无效：缺少 models 数组")
	}

	models := make([]string, 0, len(payload.Models))
	for _, item := range payload.Models {
		// 「部分代理返回 supportedGenerationMethods 为 null，此时不过滤」（Node 注释）。
		if item.SupportedGenerationMethods != nil && !containsString(item.SupportedGenerationMethods, "generateContent") {
			continue
		}
		models = append(models, strings.TrimPrefix(item.Name, "models/"))
	}
	sort.Strings(models)
	return models, nil
}

func containsString(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// doJSONRequest 发一次 GET 并读回正文（非 2xx 也读，交由调用方判状态码，与 Node 一致）。
func doJSONRequest(
	ctx context.Context,
	client *http.Client,
	method string,
	requestURL string,
	headers map[string]string,
) (*http.Response, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, method, requestURL, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("请求失败: %v", err)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, nil, fmt.Errorf("请求失败: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	// 上限防御：模型列表是元数据，10 MiB 已远超正常量级；防止畸形上游把内存打满。
	body, err := io.ReadAll(io.LimitReader(response.Body, 10*1024*1024))
	if err != nil {
		return nil, nil, fmt.Errorf("请求失败: %v", err)
	}
	return response, body, nil
}
