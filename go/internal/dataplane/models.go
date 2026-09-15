// 本文件承载**聚合式模型列表**端点（Node 的 `/v1/models` 一族）。
//
// 为什么它不能走通用透传：透传会把「上游某一家供应商的模型列表」原样端给客户端，
// 而 Node 的语义是「聚合**用户可用**的模型」——按客户端格式选出应决策的供应商类型集，
// 过滤出用户分组内的启用态供应商，逐个拉它们的 `/v1/models`（或直接用其 allowed_models），
// 去重后按客户端方言重新成形。这是真处理器，不是代理路径。
//
// 与 Node 的对应关系（`src/app/v1/_lib/models/available-models.ts`）：
//   - `handleAvailableModels`        → `/v1/models` 与 `/v1beta/models`（按请求推断形状）
//   - `handleCodexModels`            → `/v1/responses/models`（固定 codex 类型 + OpenAI 形状）
//   - `handleOpenAICompatibleModels` → `/v1/chat/completions/models`、`/v1/chat/models`（固定 openai-compatible）
//
// 记账口径：Node 的三个处理器都**不走 handleProxyRequest**，因此不写 message_request 行，
// 也不跑完整守卫链（只做一次认证）。本文件的处理器保持在同样的边界内：
// 数据面只给它一次「已认证」的调用，不建请求日志行。
package dataplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/dial"
	"github.com/fanxcv/claude-code-hub-go/go/internal/egress"
	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pctx"
)

// codexModelsManifestETag 对应 Node 的 CODEX_MODELS_MANIFEST_ETAG（available-models.ts:33）。
//
// 它是一段**固定**弱 ETag：Codex 客户端带 `client_version` 查询来问「内置模型清单有没有变」，
// Node 恒答「没变」（304 或空清单）。值必须逐字相同，否则客户端每次都会重新解析清单。
const codexModelsManifestETag = `W/"cch-codex-bundled-v1"`

// defaultModelsFetchTimeoutMS 对应 Node 的 DEFAULT_MODELS_TIMEOUT_MS（available-models.ts:30）。
const defaultModelsFetchTimeoutMS = 10_000

// modelListSpec 描述一条聚合式模型列表端点。
type modelListSpec struct {
	// Endpoint 是日志里的端点名（Node 的 endpointName）。
	Endpoint string
	// ProviderTypes 非空表示**固定**供应商类型集（Node 的 createFixedProviderTypesModelsHandler）；
	// nil 表示按客户端格式决策（Node 的 getProviderTypesForFormat）。
	ProviderTypes []string
	// OpenAIOnly 为真表示响应恒为 OpenAI 形状（三个固定处理器都调 formatOpenAIResponse）。
	OpenAIOnly bool
}

// modelListRoutes 是聚合式模型列表端点全集。
//
// 只登记 **GET**：Node 用 `app.get(...)` 挂这三个处理器，其余方法落到 `app.all("*")`
// 走代理（`POST /v1beta/models` 就属后者），因此这里多登记一个方法就会抢走本该透传的请求。
//
// `/v1beta/models` 与 `/v1/models` 共用 handleAvailableModels（v1beta 的 app 也挂了它），
// 形状按请求推断；`/v1/chat/models` 与 `/v1/chat/completions/models` 同处理器、同固定类型集。
var modelListRoutes = []routeSpec{
	{
		Method:    http.MethodGet,
		Path:      "/v1/models",
		Format:    convert.FormatOpenAI,
		Family:    egress.FamilyOpenAIChat,
		ModelList: &modelListSpec{Endpoint: "models"},
	},
	{
		Method:    http.MethodGet,
		Path:      "/v1beta/models",
		Format:    convert.FormatGemini,
		Family:    egress.FamilyGemini,
		ModelList: &modelListSpec{Endpoint: "models"},
	},
	{
		Method:    http.MethodGet,
		Path:      "/v1/responses/models",
		Format:    convert.FormatResponse,
		Family:    egress.FamilyOpenAIResponses,
		ModelList: &modelListSpec{Endpoint: "responses/models", ProviderTypes: []string{"codex"}, OpenAIOnly: true},
	},
	{
		Method:    http.MethodGet,
		Path:      "/v1/chat/completions/models",
		Format:    convert.FormatOpenAI,
		Family:    egress.FamilyOpenAIChat,
		ModelList: &modelListSpec{Endpoint: "chat/models", ProviderTypes: []string{"openai-compatible"}, OpenAIOnly: true},
	},
	{
		Method:    http.MethodGet,
		Path:      "/v1/chat/models",
		Format:    convert.FormatOpenAI,
		Family:    egress.FamilyOpenAIChat,
		ModelList: &modelListSpec{Endpoint: "chat/models", ProviderTypes: []string{"openai-compatible"}, OpenAIOnly: true},
	},
}

// matchModelListRoute 用**精确路径 + 方法**匹配聚合式模型列表端点。
//
// 精确匹配是必须的：`/v1/models/{model}` 与 `POST /v1beta/models` 归通用透传，
// 只有这五条精确路径归聚合处理器（Node 的 app.get 与 catch-all 的分工）。
func matchModelListRoute(upperMethod string, normalizedPath string) (routeSpec, bool) {
	for _, candidate := range modelListRoutes {
		if candidate.Method == upperMethod && candidate.Path == normalizedPath {
			return candidate, true
		}
	}
	return routeSpec{}, false
}

// ModelProvider 是模型列表决策所需的供应商事实。
//
// 刻意只带「决策与取数」用到的列：分组标签、活动时段与系统时区的过滤在实现侧完成
// （见 ModelCatalog 的注释），本包不碰库。
type ModelProvider struct {
	ID   int64
	Name string
	// Type 是 providers.provider_type。
	Type string
	// URL 是供应商基址。
	URL string
	// Key 是供应商凭据（只用于向上游取模型列表，绝不进日志）。
	Key string
	// AllowedModels 是 providers.allowed_models 原始 jsonb；非空时**不再访问上游**，
	// 直接取其 exact 规则（Node available-models.ts:325-334）。
	AllowedModels json.RawMessage
	// RequestTimeoutNonStreamingMS 是供应商级超时；零值取 defaultModelsFetchTimeoutMS。
	RequestTimeoutNonStreamingMS int
}

// ModelCatalog 是聚合式模型列表的只读视图。
//
// 为什么把过滤放在实现侧：Node 的过滤条件是「启用态 + 类型命中 + 活动时段（按系统时区）+
// 分组命中（key.providerGroup > user.providerGroup）」，其中活动时段与分组都要读库与设置，
// 塞进本包会把数据面变成第二个业务层。实现侧照 Node 的
// `getAvailableModelsByProviderTypes`（available-models.ts:437-470）逐条过滤即可。
type ModelCatalog interface {
	// ProvidersForModelList 返回参与模型列表的供应商（已完成 Node 侧四项过滤）。
	ProvidersForModelList(ctx context.Context, providerTypes []string, keyID, userID int64) ([]ModelProvider, error)
}

// fetchedModel 是聚合前的单条模型（对应 Node 的 FetchedModel）。
type fetchedModel struct {
	ID          string
	DisplayName string
	CreatedAt   string
}

// serveModelList 应答一条聚合式模型列表请求。
//
// 前置条件：调用方已完成认证（Node 的 authenticateRequest 在三处处理器里都是第一步），
// 并把认证态原样传入——本函数不重复鉴权，也不写请求日志行。
func (h *Handler) serveModelList(
	writer http.ResponseWriter,
	request *http.Request,
	spec routeSpec,
	auth pctx.AuthState,
) {
	catalog := h.options.ModelCatalog
	if catalog == nil {
		// 装配缺口：注册这条路由就不再成立（入口分支已按 nil 拦住），走到这里说明装配自相矛盾。
		h.logger.Error("dataplane.model_list_catalog_missing", map[string]any{"path": request.URL.Path})
		writeJSONError(writer, http.StatusInternalServerError, "模型列表未接线", "internal_server_error")
		return
	}
	spec_ := spec.ModelList
	responseFormat := detectModelsResponseFormat(request)

	// codex 内置清单探针：只在「精确的 /v1/models + OpenAI 形状 + 无格式覆盖 + 带 client_version」
	// 四条同时成立时命中，返回空清单 + 固定 ETag（Node available-models.ts:562-574）。
	if spec_.ProviderTypes == nil &&
		request.URL.Path == "/v1/models" &&
		responseFormat == modelsFormatOpenAI &&
		detectModelsClientFormatOverride(request) == "" &&
		strings.TrimSpace(request.URL.Query().Get("client_version")) != "" {
		writer.Header().Set("ETag", codexModelsManifestETag)
		writer.Header().Set("Cache-Control", "no-cache")
		if modelsIfNoneMatchMatches(request.Header.Get("If-None-Match"), codexModelsManifestETag) {
			writer.WriteHeader(http.StatusNotModified)
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"models": []any{}})
		return
	}

	clientFormat := detectModelsClientFormatOverride(request)
	if clientFormat == "" {
		clientFormat = mapModelsResponseFormatToClientFormat(responseFormat)
	}
	providerTypes := spec_.ProviderTypes
	if providerTypes == nil {
		providerTypes = modelsProviderTypesForFormat(clientFormat)
	}

	providers, err := catalog.ProvidersForModelList(request.Context(), providerTypes, auth.KeyID, auth.UserID)
	if err != nil {
		h.logger.Error("dataplane.model_list_catalog_failed", map[string]any{
			"path":  request.URL.Path,
			"error": err.Error(),
		})
		writeJSONError(writer, http.StatusServiceUnavailable, "模型列表读取失败", "service_unavailable_error")
		return
	}
	if len(providers) == 0 {
		// Node 在这里只记一条 warn 并回空清单（available-models.ts:466-471）：供应商为空不是错误，
		// 客户端的语义是「暂时没有可用模型」。
		h.logger.Warn("dataplane.model_list_no_provider", map[string]any{
			"path":          request.URL.Path,
			"providerTypes": strings.Join(providerTypes, ","),
		})
	}

	models := h.aggregateModels(request.Context(), providers)
	h.logModelsAggregated(spec_.Endpoint, len(providers), len(models))

	shape := modelsFormatOpenAI
	if !spec_.OpenAIOnly {
		shape = responseFormat
	}
	writeJSON(writer, http.StatusOK, formatModels(shape, models, h.nowOrDefault()))
}

// aggregateModels 逐个供应商取模型并去重，保持首次出现的顺序（Node 的 Promise.all + seenIds）。
//
// 单个供应商失败**不**影响整体：Node 在 fetchModelsFromProvider 里 catch 后返回空数组
// （available-models.ts:321-327），本函数照抄该隔离粒度。
func (h *Handler) aggregateModels(ctx context.Context, providers []ModelProvider) []fetchedModel {
	type result struct {
		index  int
		models []fetchedModel
	}
	results := make([]result, len(providers))
	var wg sync.WaitGroup
	for index, provider := range providers {
		wg.Add(1)
		go func(index int, provider ModelProvider) {
			defer wg.Done()
			models, err := h.modelsFromProvider(ctx, provider)
			if err != nil {
				h.logger.Warn("dataplane.model_list_provider_failed", map[string]any{
					"providerId": provider.ID,
					"provider":   provider.Name,
					"error":      err.Error(),
				})
				results[index] = result{index: index}
				return
			}
			results[index] = result{index: index, models: models}
		}(index, provider)
	}
	wg.Wait()

	seen := make(map[string]bool)
	aggregated := make([]fetchedModel, 0, len(providers))
	for _, item := range results {
		for _, model := range item.models {
			if model.ID == "" || seen[model.ID] {
				continue
			}
			seen[model.ID] = true
			aggregated = append(aggregated, model)
		}
	}
	return aggregated
}

// modelsFromProvider 取一家供应商的模型：配了 allowed_models 就不打上游，否则按类型取。
func (h *Handler) modelsFromProvider(ctx context.Context, provider ModelProvider) ([]fetchedModel, error) {
	if exact := exactAllowedModels(provider.AllowedModels); len(exact) > 0 {
		models := make([]fetchedModel, 0, len(exact))
		for _, id := range exact {
			models = append(models, fetchedModel{ID: id})
		}
		return models, nil
	}
	// 固定类型集的三个处理器与「按格式决策」走的是同一条取数路径：Node 里两者都调
	// getAvailableModelsByProviderTypes，区别只在 providerTypes 的来处。
	return h.fetchProviderModels(ctx, provider)
}

// fetchProviderModels 按供应商类型向上游取模型列表。
//
// 三种上游形态逐条对齐 Node 的 UPSTREAM_CONFIGS（available-models.ts:206-243）：
//   - claude / claude-auth：`{base}/v1/models`，`x-api-key` + `anthropic-version`；
//   - codex / openai-compatible：`{base}/v1/models`，`Authorization: Bearer`；
//   - gemini / gemini-cli：`{base}/v1beta/models`（base 已以 /v1beta 结尾则不重复），`x-goog-api-key`。
//
// 非 200 一律当失败（Node 抛错后被上层隔离成空数组）。
func (h *Handler) fetchProviderModels(ctx context.Context, provider ModelProvider) ([]fetchedModel, error) {
	client := h.options.Forward.Dial
	if client == nil {
		return nil, errors.New("dataplane: 未接线上游拨号器")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(provider.URL), "/")
	if baseURL == "" {
		return nil, errors.New("dataplane: 供应商基址为空")
	}
	url, headers, parse, err := upstreamModelsConfig(provider.Type, baseURL, provider.Key)
	if err != nil {
		return nil, err
	}
	if url == "" {
		return nil, fmt.Errorf("dataplane: 未知的供应商类型 %q", provider.Type)
	}

	timeout := time.Duration(provider.RequestTimeoutNonStreamingMS) * time.Millisecond
	if timeout <= 0 {
		timeout = defaultModelsFetchTimeoutMS * time.Millisecond
	}
	fetchCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	response, err := client.RoundTrip(fetchCtx, dial.Request{
		Method:  http.MethodGet,
		URL:     url,
		Headers: headers,
	})
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return nil, fmt.Errorf("上游返回 %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
	}
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	return parse(payload)
}

// upstreamModelsConfig 按供应商类型给出上游 URL、请求头与响应解析器。
//
// 标准 `/v1/models` 一律经 dial.BuildUpstreamURL 拼接（与数据面同一条语义）：供应商基址自带
// 路径前缀或停在版本根（如 `…/api/plan/v3`）时，裸拼 `base + "/v1/models"` 会多出一个版本段。
// gemini 的分支保留自己的形态（`/v1beta` 前缀 + 查询参数），不适用标准端点根语义。
func upstreamModelsConfig(providerType, baseURL, key string) (string, http.Header, func([]byte) ([]fetchedModel, error), error) {
	headers := http.Header{}
	switch providerType {
	case "claude", "claude-auth":
		headers.Set("x-api-key", key)
		headers.Set("anthropic-version", "2023-06-01")
		target, err := dial.BuildUpstreamURL(baseURL, "/v1/models")
		return target, headers, parseClaudeModels, err
	case "codex", "openai-compatible":
		headers.Set("Authorization", "Bearer "+key)
		target, err := dial.BuildUpstreamURL(baseURL, "/v1/models")
		return target, headers, parseOpenAIModels, err
	case "gemini", "gemini-cli":
		headers.Set("x-goog-api-key", key)
		prefix := baseURL
		if !strings.HasSuffix(baseURL, "/v1beta") {
			prefix = baseURL + "/v1beta"
		}
		return prefix + "/models", headers, parseGeminiModels, nil
	default:
		return "", headers, nil, nil
	}
}

func parseOpenAIModels(payload []byte) ([]fetchedModel, error) {
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, err
	}
	models := make([]fetchedModel, 0, len(body.Data))
	for _, item := range body.Data {
		models = append(models, fetchedModel{ID: item.ID})
	}
	return models, nil
}

func parseClaudeModels(payload []byte) ([]fetchedModel, error) {
	var body struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			CreatedAt   string `json:"created_at"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, err
	}
	models := make([]fetchedModel, 0, len(body.Data))
	for _, item := range body.Data {
		models = append(models, fetchedModel{ID: item.ID, DisplayName: item.DisplayName, CreatedAt: item.CreatedAt})
	}
	return models, nil
}

func parseGeminiModels(payload []byte) ([]fetchedModel, error) {
	var body struct {
		Models []struct {
			Name        string `json:"name"`
			DisplayName string `json:"displayName"`
		} `json:"models"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		return nil, err
	}
	models := make([]fetchedModel, 0, len(body.Models))
	for _, item := range body.Models {
		models = append(models, fetchedModel{
			ID:          strings.TrimPrefix(item.Name, "models/"),
			DisplayName: item.DisplayName,
		})
	}
	return models, nil
}

// modelsResponseFormat 是响应形状三态（Node 的 ResponseFormat）。
type modelsResponseFormat string

const (
	modelsFormatOpenAI    modelsResponseFormat = "openai"
	modelsFormatAnthropic modelsResponseFormat = "anthropic"
	modelsFormatGemini    modelsResponseFormat = "gemini"
)

// detectModelsResponseFormat 对应 Node 的 detectResponseFormat（available-models.ts:138-150）。
func detectModelsResponseFormat(request *http.Request) modelsResponseFormat {
	if request.Header.Get("anthropic-version") != "" {
		return modelsFormatAnthropic
	}
	if request.Header.Get("x-goog-api-key") != "" || strings.Contains(request.URL.Path, "/v1beta/") {
		return modelsFormatGemini
	}
	return modelsFormatOpenAI
}

// detectModelsClientFormatOverride 对应 Node 的 detectClientFormatOverride（available-models.ts:155-176）：
// 查询串优先于请求头，无法识别一律当作「无覆盖」（返回空串）。
func detectModelsClientFormatOverride(request *http.Request) convert.ClientFormat {
	query := request.URL.Query()
	raw := firstNonEmpty(
		query.Get("api_type"), query.Get("apiType"), query.Get("format"),
		request.Header.Get("x-openai-api-type"),
		request.Header.Get("x-cch-api-type"),
		request.Header.Get("openai-beta"),
	)
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "response", "responses", "codex":
		return convert.FormatResponse
	case "openai", "chat":
		return convert.FormatOpenAI
	case "claude", "anthropic":
		return convert.FormatClaude
	case "gemini":
		return convert.FormatGemini
	case "gemini-cli", "geminicli":
		return convert.FormatGeminiCLI
	default:
		return ""
	}
}

// mapModelsResponseFormatToClientFormat 对应 Node 的 mapResponseFormatToClientFormat。
func mapModelsResponseFormatToClientFormat(format modelsResponseFormat) convert.ClientFormat {
	switch format {
	case modelsFormatAnthropic:
		return convert.FormatClaude
	case modelsFormatGemini:
		return convert.FormatGemini
	default:
		return convert.FormatOpenAI
	}
}

// modelsProviderTypesForFormat 对应 Node 的 getProviderTypesForFormat（available-models.ts:332-352）。
func modelsProviderTypesForFormat(format convert.ClientFormat) []string {
	switch format {
	case convert.FormatClaude:
		return []string{"claude", "claude-auth"}
	case convert.FormatGemini, convert.FormatGeminiCLI:
		return []string{"gemini", "gemini-cli"}
	case convert.FormatResponse:
		return []string{"codex"}
	default:
		return []string{"codex", "openai-compatible"}
	}
}

// modelsIfNoneMatchMatches 复刻 Node 的 matchesIfNoneMatch：支持逗号分隔与 `*`。
func modelsIfNoneMatchMatches(value string, etag string) bool {
	for _, item := range strings.Split(value, ",") {
		candidate := strings.TrimSpace(item)
		if candidate == "*" || candidate == etag {
			return true
		}
	}
	return false
}

// exactAllowedModels 取 providers.allowed_models 里的 **exact** 规则（Node available-models.ts:325-334）。
//
// 输入形态与 Node 的 normalizeAllowedModelRule 同义：数组元素要么是字符串（等价 exact），
// 要么是 `{matchType, pattern}`；matchType 不在枚举内或 pattern 为空的条目被丢弃。
func exactAllowedModels(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		var text string
		if err := json.Unmarshal(item, &text); err == nil {
			if trimmed := strings.TrimSpace(text); trimmed != "" {
				out = append(out, trimmed)
			}
			continue
		}
		var rule struct {
			MatchType string `json:"matchType"`
			Pattern   string `json:"pattern"`
		}
		if err := json.Unmarshal(item, &rule); err != nil {
			continue
		}
		if rule.MatchType != "exact" {
			continue
		}
		if trimmed := strings.TrimSpace(rule.Pattern); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// inferModelOwner 对应 Node 的 inferOwner（available-models.ts:196-206）。
func inferModelOwner(modelID string) string {
	switch {
	case strings.HasPrefix(modelID, "claude-"):
		return "anthropic"
	case strings.HasPrefix(modelID, "gpt-"), strings.HasPrefix(modelID, "o1"), strings.HasPrefix(modelID, "o3"):
		return "openai"
	case strings.HasPrefix(modelID, "gemini-"):
		return "google"
	case strings.HasPrefix(modelID, "deepseek"):
		return "deepseek"
	case strings.HasPrefix(modelID, "qwen"):
		return "alibaba"
	default:
		return "unknown"
	}
}

// formatModels 按形状成形（Node 的三个 format*Response）。
func formatModels(shape modelsResponseFormat, models []fetchedModel, now time.Time) any {
	switch shape {
	case modelsFormatAnthropic:
		data := make([]map[string]any, 0, len(models))
		for _, model := range models {
			displayName := model.DisplayName
			if displayName == "" {
				displayName = model.ID
			}
			createdAt := normalizeAnthropicTimestamp(model.CreatedAt)
			if createdAt == "" {
				createdAt = normalizeAnthropicTimestamp(now.UTC().Format(time.RFC3339))
			}
			data = append(data, map[string]any{
				"id":           model.ID,
				"type":         "model",
				"display_name": displayName,
				"created_at":   createdAt,
			})
		}
		return map[string]any{"data": data, "has_more": false}
	case modelsFormatGemini:
		items := make([]map[string]any, 0, len(models))
		for _, model := range models {
			displayName := model.DisplayName
			if displayName == "" {
				displayName = model.ID
			}
			items = append(items, map[string]any{
				"name":                       "models/" + model.ID,
				"displayName":                displayName,
				"supportedGenerationMethods": []string{"generateContent"},
			})
		}
		return map[string]any{"models": items}
	default:
		created := now.Unix()
		data := make([]map[string]any, 0, len(models))
		for _, model := range models {
			data = append(data, map[string]any{
				"id":       model.ID,
				"object":   "model",
				"created":  created,
				"owned_by": inferModelOwner(model.ID),
			})
		}
		return map[string]any{"object": "list", "data": data}
	}
}

// normalizeAnthropicTimestamp 对应 Node 的 normalizeAnthropicTimestamp：解析不了就原样返回，
// 能解析则去掉毫秒（Anthropic 官方形如 `2026-05-29T09:22:44Z`）。
func normalizeAnthropicTimestamp(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return value
	}
	return parsed.UTC().Format(time.RFC3339)
}

func (h *Handler) nowOrDefault() time.Time {
	if h.options.Now != nil {
		return h.options.Now()
	}
	return time.Now()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// modelListPipeline 是模型列表端点的守卫链。
//
// 只取 auth 一步：Node 的三个处理器只调 authenticateRequest（既不选路、不开行、不过滤），
// 比 raw 预设还短。复用它而不是另写一套鉴权，是为了让 401 的形状（错误码、文案、状态码）
// 与数据面其它端点同源，不出现两套鉴权语义。
var modelListPipeline = guard.Pipeline{
	Name:  "MODELS_PIPELINE",
	Steps: []guard.StepKey{guard.StepAuth},
}

// runModelListAuth 跑模型列表的鉴权链；返回的 response 非空时表示已被拦截（调用方原样回写）。
func (h *Handler) runModelListAuth(pc *pctx.Context, deps guard.Deps) (*guard.Response, error) {
	chain, err := deps.FromPipeline(modelListPipeline)
	if err != nil {
		return nil, err
	}
	return chain.Run(pc)
}

// logModelsAggregated 是聚合完成后的诊断日志（Node 的 logger.info("[AvailableModels] Aggregated models")）。
func (h *Handler) logModelsAggregated(endpoint string, providers int, models int) {
	h.logger.Info("dataplane.model_list_aggregated", map[string]any{
		"endpoint":      endpoint,
		"providerCount": providers,
		"modelCount":    models,
	})
}

// writeJSON 写 JSON 响应（Content-Type 与 Node 的 c.json 同形）。
func writeJSON(writer http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		writer.WriteHeader(http.StatusInternalServerError)
		_, _ = writer.Write([]byte(`{"error":{"message":"序列化失败"}}`))
		return
	}
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_, _ = writer.Write(body)
}

// writeJSONError 写与数据面同形的错误信封。
func writeJSONError(writer http.ResponseWriter, status int, message string, errorType string) {
	payload := map[string]any{"error": map[string]any{"message": message, "type": errorType, "code": errorType}}
	writeJSON(writer, status, payload)
}

// serveModelListRequest 是模型列表端点在数据面入口的接线：建上下文 → 只跑鉴权 → 交处理器。
//
// 与 ServeHTTP 主路径的三点差异都是**刻意的**：
//   - 只跑 auth 一步（Node 的三个处理器只调 authenticateRequest）；
//   - 不开请求日志行、不做会话观测、不做回放（Node 同样不在这些端点上记账）；
//   - 不绑 fallback：本函数只在装配了 ModelCatalog 时被调用，不存在「答不了再回退」的分支。
func (h *Handler) serveModelListRequest(writer http.ResponseWriter, request *http.Request, spec routeSpec) {
	requestCtx, cancel := context.WithCancel(request.Context())
	defer cancel()
	pc, err := pctx.New(pctx.Init{
		Method:       request.Method,
		Path:         request.URL.Path,
		Headers:      request.Header,
		Body:         request.Body,
		ClientIP:     h.options.ClientIP(request),
		ProtocolFrom: spec.Family,
		Logger:       h.logger,
		Now:          h.options.Now,
	})
	if err != nil {
		writeJSONError(writer, http.StatusBadRequest, "请求上下文构造失败", "internal_server_error")
		return
	}
	if err := pc.SetOwner(egress.OwnerGo); err != nil {
		h.logger.Warn("dataplane.owner_conflict", map[string]any{"path": request.URL.Path, "error": err.Error()})
	}
	body := newBodyAccess(requestCtx, pc, h.options.BodyOptions)
	deps := h.options.Base
	deps.Body = body.factory
	deps.RequestContext = func(*pctx.Context) context.Context { return requestCtx }

	response, err := h.runModelListAuth(pc, deps)
	if err != nil {
		h.logger.Error("dataplane.model_list_auth_failed", map[string]any{
			"path":  request.URL.Path,
			"error": err.Error(),
		})
		writeJSONError(writer, http.StatusInternalServerError, "请求处理失败", "internal_server_error")
		return
	}
	if response != nil {
		h.writeGuardResponse(writer, response)
		return
	}
	auth, _ := pc.Auth()
	h.serveModelList(writer, request, spec, auth)
}
