package dataplane

import (
	"regexp"
	"strings"
)

// 端点族目录：Node 侧判定「这个路径归数据面、且该怎么处理」的唯一事实来源。
//
// 权威出处：`src/app/v1/_lib/proxy/endpoint-family-catalog.ts`（`KNOWN_ENDPOINT_FAMILIES`）
// 与 `src/app/v1/_lib/proxy/endpoint-paths.ts`（归一化）。**本文件是它的移植**，两边必须同步；
// `family_sync_test.go` 会解析该 TS 文件并逐条比对 id 顺序，漂移即红。
//
// 为什么需要它：Node 的数据面是 catch-all（`app.all("*")`），族只决定
// surface / 记账档位 / 是否原始透传，**不决定是否需要逐条实现**——匹配到族的路径一律走
// 「通用透传」（上游 URL 由 `src/app/v1/_lib/url.ts` 的 `buildProxyUrl` 按
// `basePath + requestPath` 拼接）。故 Go 侧的正确形态是「族表 + 一条通用透传」，
// 而不是为每一路径写一个处理器；族表是那条通用透传的路由依据。
//
// 匹配顺序敏感（首个命中即返回）：例如 `/v1/responses/compact` 必须先于
// `/v1/responses` 的精确与前缀族，`/v1/messages/count_tokens` 必须先于 `/v1/messages`。

// EndpointFamilySurface 是族所属的客户端协议面。
type EndpointFamilySurface string

const (
	SurfaceResponse  EndpointFamilySurface = "response"
	SurfaceOpenAI    EndpointFamilySurface = "openai"
	SurfaceClaude    EndpointFamilySurface = "claude"
	SurfaceGemini    EndpointFamilySurface = "gemini"
	SurfaceGeminiCLI EndpointFamilySurface = "gemini-cli"
)

// AccountingTier 是该族的记账档位：required 必须有用量、optional 有则记、none 不记账。
type AccountingTier string

const (
	TierRequiredAccounting AccountingTier = "required_usage"
	TierOptionalAccounting AccountingTier = "optional_usage"
	TierNoAccounting       AccountingTier = "none"
)

// EndpointFamily 是一条端点族。
type EndpointFamily struct {
	// ID 与 Node 侧族 id 逐字相同（同步测试据它比对）。
	ID string
	// Surface 是客户端侧协议面。
	Surface EndpointFamilySurface
	// Accounting 是记账档位。
	Accounting AccountingTier
	// ModelRequired 为真表示该族要求请求体带 model。
	ModelRequired bool
	// RawPassthrough 为真表示原始透传：不做转发前预处理、不做响应改写。
	// 目前仅 count_tokens 与 responses/compact 为真（对齐 Node 的 rawPassthroughEndpointPathSet）。
	RawPassthrough bool
	// match 判定归一化后的路径是否属于本族。只能由本文件的构造器赋值。
	match func(normalizedPath string) bool
}

// normalizeEndpointPath 与 Node 的 `normalizeEndpointPath` 同义：
// 去查询串、去尾斜杠（根路径除外）、转小写。
func normalizeEndpointPath(pathname string) string {
	if i := strings.IndexByte(pathname, '?'); i >= 0 {
		pathname = pathname[:i]
	}
	if len(pathname) > 1 && strings.HasSuffix(pathname, "/") {
		pathname = pathname[:len(pathname)-1]
	}
	return strings.ToLower(pathname)
}

// hasEndpointPrefix 与 Node 的 `hasPrefix` 同义：等于前缀或以其为路径段前缀。
//
// 注意不能用裸 strings.HasPrefix：那会让 `/v1/modelsfoo` 命中 `/v1/models`。
func hasEndpointPrefix(pathname, prefix string) bool {
	return pathname == prefix || strings.HasPrefix(pathname, prefix+"/")
}

// geminiActionPrefixes 是可承载 `:action` 后缀的 Gemini 模型路径前缀。
// 与 Node 的 GEMINI_STANDARD_MODEL_PREFIXES / GEMINI_PREDICT_MODEL_PREFIXES 对应。
var (
	geminiStandardModelPrefixes = []string{
		"/v1beta/models/",
		"/v1/publishers/google/models/",
		"/v1/models/",
	}
	geminiPredictModelPrefixes = []string{
		"/v1beta/models/",
		"/v1/publishers/google/models/",
	}
	geminiCLIModelPrefixes = []string{"/v1internal/models/"}
)

// matchGeminiModelAction 与 Node 的同名函数同义：`{prefix}{model}:{action}`，
// model 不得含 `/` 或 `:`，action 非空、不含 `/` 且须在允许集合内。
func matchGeminiModelAction(pathname, prefix string, actions map[string]bool) bool {
	if !strings.HasPrefix(pathname, prefix) {
		return false
	}
	remainder := pathname[len(prefix):]
	sep := strings.IndexByte(remainder, ':')
	if sep <= 0 {
		return false
	}
	model, action := remainder[:sep], remainder[sep+1:]
	if model == "" || strings.ContainsAny(model, "/:") {
		return false
	}
	return action != "" && !strings.Contains(action, "/") && actions[action]
}

func matchGeminiModelActionOnPrefixes(pathname string, prefixes []string, actions map[string]bool) bool {
	for _, prefix := range prefixes {
		if matchGeminiModelAction(pathname, prefix, actions) {
			return true
		}
	}
	return false
}

func actionsOf(names ...string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[name] = true
	}
	return set
}

// openAIModelsPathPattern 对应 Node 的正则 `^\/v1\/models(?:\/[^/:]+)?$`（大小写不敏感，
// 而归一化已转小写）。注意它**不**匹配 `/v1/models/foo:bar`（action 形态归 Gemini 族）。
var openAIModelsPathPattern = regexp.MustCompile(`^/v1/models(?:/[^/:]+)?$`)

// geminiModelsResourcePattern 对应 Node 的 `^\/v1beta\/models\/[^/:]+$` 与
// `^\/v1\/publishers\/google\/models\/[^/:]+$`。
var geminiModelsResourcePattern = regexp.MustCompile(`^/v1(?:beta/models|/publishers/google/models)/[^/:]+$`)

// knownEndpointFamilies 是族的全集，顺序与 Node 侧一致（首个命中即返回）。
var knownEndpointFamilies = []EndpointFamily{
	{ID: "claude-messages", Surface: SurfaceClaude, Accounting: TierRequiredAccounting,
		match: func(p string) bool { return p == "/v1/messages" }},
	{ID: "claude-count-tokens", Surface: SurfaceClaude, Accounting: TierNoAccounting, RawPassthrough: true,
		match: func(p string) bool { return p == "/v1/messages/count_tokens" }},
	{ID: "response-compact", Surface: SurfaceResponse, Accounting: TierNoAccounting, RawPassthrough: true,
		match: func(p string) bool { return p == "/v1/responses/compact" }},
	{ID: "response-execution", Surface: SurfaceResponse, Accounting: TierRequiredAccounting,
		match: func(p string) bool { return p == "/v1/responses" }},
	{ID: "response-resources", Surface: SurfaceResponse, Accounting: TierNoAccounting,
		match: func(p string) bool { return hasEndpointPrefix(p, "/v1/responses") }},
	{ID: "openai-chat-completions", Surface: SurfaceOpenAI, Accounting: TierRequiredAccounting, ModelRequired: true,
		match: func(p string) bool { return p == "/v1/chat/completions" }},
	{ID: "openai-chat-completions-resources", Surface: SurfaceOpenAI, Accounting: TierNoAccounting,
		match: func(p string) bool { return hasEndpointPrefix(p, "/v1/chat/completions") }},
	{ID: "openai-models", Surface: SurfaceOpenAI, Accounting: TierNoAccounting,
		match: func(p string) bool { return openAIModelsPathPattern.MatchString(p) }},
	{ID: "gemini-generate-content", Surface: SurfaceGemini, Accounting: TierRequiredAccounting, ModelRequired: true,
		match: func(p string) bool {
			return matchGeminiModelActionOnPrefixes(p, geminiStandardModelPrefixes, actionsOf("generatecontent"))
		}},
	{ID: "gemini-stream-generate-content", Surface: SurfaceGemini, Accounting: TierRequiredAccounting, ModelRequired: true,
		match: func(p string) bool {
			return matchGeminiModelActionOnPrefixes(p, geminiStandardModelPrefixes, actionsOf("streamgeneratecontent"))
		}},
	{ID: "gemini-count-tokens", Surface: SurfaceGemini, Accounting: TierOptionalAccounting, ModelRequired: true,
		match: func(p string) bool {
			return matchGeminiModelActionOnPrefixes(p, geminiStandardModelPrefixes, actionsOf("counttokens"))
		}},
	{ID: "gemini-embed-content", Surface: SurfaceGemini, Accounting: TierOptionalAccounting, ModelRequired: true,
		match: func(p string) bool {
			return matchGeminiModelActionOnPrefixes(p, geminiStandardModelPrefixes, actionsOf("embedcontent"))
		}},
	{ID: "gemini-batch-generate-content", Surface: SurfaceGemini, Accounting: TierOptionalAccounting, ModelRequired: true,
		match: func(p string) bool {
			return matchGeminiModelActionOnPrefixes(p, geminiStandardModelPrefixes, actionsOf("batchgeneratecontent"))
		}},
	{ID: "gemini-batch-embed-contents", Surface: SurfaceGemini, Accounting: TierOptionalAccounting, ModelRequired: true,
		match: func(p string) bool {
			return matchGeminiModelActionOnPrefixes(p, geminiStandardModelPrefixes, actionsOf("batchembedcontents"))
		}},
	{ID: "gemini-async-batch-embed-content", Surface: SurfaceGemini, Accounting: TierOptionalAccounting, ModelRequired: true,
		match: func(p string) bool {
			return matchGeminiModelActionOnPrefixes(p, geminiStandardModelPrefixes, actionsOf("asyncbatchembedcontent"))
		}},
	{ID: "gemini-predict", Surface: SurfaceGemini, Accounting: TierOptionalAccounting, ModelRequired: true,
		match: func(p string) bool {
			return matchGeminiModelActionOnPrefixes(p, geminiPredictModelPrefixes, actionsOf("predict"))
		}},
	{ID: "gemini-predict-long-running", Surface: SurfaceGemini, Accounting: TierOptionalAccounting, ModelRequired: true,
		match: func(p string) bool {
			return matchGeminiModelActionOnPrefixes(p, geminiPredictModelPrefixes, actionsOf("predictlongrunning"))
		}},
	{ID: "gemini-files", Surface: SurfaceGemini, Accounting: TierNoAccounting,
		match: func(p string) bool { return hasEndpointPrefix(p, "/v1beta/files") }},
	{ID: "gemini-models-resource", Surface: SurfaceGemini, Accounting: TierNoAccounting,
		match: func(p string) bool {
			return p == "/v1beta/models" || geminiModelsResourcePattern.MatchString(p)
		}},
	{ID: "gemini-cli-generate-content", Surface: SurfaceGeminiCLI, Accounting: TierRequiredAccounting, ModelRequired: true,
		match: func(p string) bool {
			return matchGeminiModelActionOnPrefixes(p, geminiCLIModelPrefixes, actionsOf("generatecontent"))
		}},
	{ID: "gemini-cli-stream-generate-content", Surface: SurfaceGeminiCLI, Accounting: TierRequiredAccounting, ModelRequired: true,
		match: func(p string) bool {
			return matchGeminiModelActionOnPrefixes(p, geminiCLIModelPrefixes, actionsOf("streamgeneratecontent"))
		}},
	{ID: "openai-completions", Surface: SurfaceOpenAI, Accounting: TierRequiredAccounting, ModelRequired: true,
		match: func(p string) bool { return hasEndpointPrefix(p, "/v1/completions") }},
	{ID: "openai-embeddings", Surface: SurfaceOpenAI, Accounting: TierRequiredAccounting, ModelRequired: true,
		match: func(p string) bool { return p == "/v1/embeddings" }},
	{ID: "openai-moderations", Surface: SurfaceOpenAI, Accounting: TierNoAccounting,
		match: func(p string) bool { return hasEndpointPrefix(p, "/v1/moderations") }},
	{ID: "openai-audio-generation", Surface: SurfaceOpenAI, Accounting: TierNoAccounting,
		match: func(p string) bool { return p == "/v1/audio/speech" }},
	{ID: "openai-audio-transcription", Surface: SurfaceOpenAI, Accounting: TierNoAccounting,
		match: func(p string) bool {
			return p == "/v1/audio/transcriptions" || p == "/v1/audio/translations"
		}},
	{ID: "openai-audio-resources", Surface: SurfaceOpenAI, Accounting: TierNoAccounting,
		match: func(p string) bool {
			return hasEndpointPrefix(p, "/v1/audio/voice_consents") || hasEndpointPrefix(p, "/v1/audio/voices")
		}},
	{ID: "openai-images", Surface: SurfaceOpenAI, Accounting: TierNoAccounting,
		match: func(p string) bool { return hasEndpointPrefix(p, "/v1/images") }},
	{ID: "openai-files", Surface: SurfaceOpenAI, Accounting: TierNoAccounting,
		match: func(p string) bool { return hasEndpointPrefix(p, "/v1/files") }},
	{ID: "openai-uploads", Surface: SurfaceOpenAI, Accounting: TierNoAccounting,
		match: func(p string) bool { return hasEndpointPrefix(p, "/v1/uploads") }},
	{ID: "openai-batches", Surface: SurfaceOpenAI, Accounting: TierNoAccounting,
		match: func(p string) bool { return hasEndpointPrefix(p, "/v1/batches") }},
	{ID: "openai-fine-tuning", Surface: SurfaceOpenAI, Accounting: TierNoAccounting,
		match: func(p string) bool { return hasEndpointPrefix(p, "/v1/fine_tuning") }},
	{ID: "openai-evals", Surface: SurfaceOpenAI, Accounting: TierNoAccounting,
		match: func(p string) bool { return hasEndpointPrefix(p, "/v1/evals") }},
	{ID: "openai-assistants", Surface: SurfaceOpenAI, Accounting: TierNoAccounting,
		match: func(p string) bool { return hasEndpointPrefix(p, "/v1/assistants") }},
	{ID: "openai-threads", Surface: SurfaceOpenAI, Accounting: TierNoAccounting,
		match: func(p string) bool { return hasEndpointPrefix(p, "/v1/threads") }},
	{ID: "openai-conversations", Surface: SurfaceOpenAI, Accounting: TierNoAccounting,
		match: func(p string) bool { return hasEndpointPrefix(p, "/v1/conversations") }},
	{ID: "openai-vector-stores", Surface: SurfaceOpenAI, Accounting: TierNoAccounting,
		match: func(p string) bool { return hasEndpointPrefix(p, "/v1/vector_stores") }},
	{ID: "openai-containers", Surface: SurfaceOpenAI, Accounting: TierNoAccounting,
		match: func(p string) bool { return hasEndpointPrefix(p, "/v1/containers") }},
	{ID: "openai-realtime-http", Surface: SurfaceOpenAI, Accounting: TierNoAccounting,
		match: func(p string) bool { return hasEndpointPrefix(p, "/v1/realtime") }},
	{ID: "openai-videos", Surface: SurfaceOpenAI, Accounting: TierNoAccounting,
		match: func(p string) bool { return hasEndpointPrefix(p, "/v1/videos") }},
	{ID: "openai-skills", Surface: SurfaceOpenAI, Accounting: TierNoAccounting,
		match: func(p string) bool { return hasEndpointPrefix(p, "/v1/skills") }},
	{ID: "openai-chatkit", Surface: SurfaceOpenAI, Accounting: TierNoAccounting,
		match: func(p string) bool { return hasEndpointPrefix(p, "/v1/chatkit") }},
}

// KnownEndpointFamilies 返回族全集（调用方不得修改其元素）。
func KnownEndpointFamilies() []EndpointFamily {
	out := make([]EndpointFamily, len(knownEndpointFamilies))
	copy(out, knownEndpointFamilies)
	return out
}

// ResolveEndpointFamily 按归一化路径解析端点族，未命中返回 false。
// 与 Node 的 `resolveEndpointFamilyByPath` 同义（含首个命中即返回的顺序语义）。
func ResolveEndpointFamily(pathname string) (EndpointFamily, bool) {
	normalized := normalizeEndpointPath(pathname)
	for i := range knownEndpointFamilies {
		if knownEndpointFamilies[i].match(normalized) {
			return knownEndpointFamilies[i], true
		}
	}
	return EndpointFamily{}, false
}

// IsKnownDataPlanePath 等价于 Node 的 `isStandardProxyEndpointPath`。
func IsKnownDataPlanePath(pathname string) bool {
	_, ok := ResolveEndpointFamily(pathname)
	return ok
}
