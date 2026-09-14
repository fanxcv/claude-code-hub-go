// Package providertest 复刻 Node 的「供应商协议级探测」引擎
// （`src/lib/provider-testing/**`）：预设模板表、探测执行、响应解析与校验。
//
// 与 Node 的对应关系：
//
//	presets.ts              -> 本文件
//	types.ts (TEST_DEFAULTS) -> defaults.go
//	test-service.ts         -> service.go
//	utils/test-prompts.ts   -> prompts.go
//	utils/sse-collector.ts  -> sse.go
//	parsers/*.ts            -> parse.go
//	validators/*.ts         -> validate.go
//
// 载荷模板（`data/*.json`）是 `src/lib/provider-testing/data/*.json` 的**逐字节副本**，
// 由 `TestPresetPayloadCopiesMatchNodeSource` 钉住漂移（复制而非单一真源是刻意的：
// 用 go:embed 无法跨模块目录引用，钉子负责发现漂移，人工负责同步）。
package providertest

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

//go:embed data
var payloadFS embed.FS

// ProviderType 是供应商类型。取值为 Node 的 PUBLIC + HIDDEN 两个集合的并集
// （`src/lib/api/v1/_shared/constants.ts:8-19`）。
type ProviderType string

// 六个供应商类型（逐字对应 Node 的字符串常量）。
const (
	TypeClaude           ProviderType = "claude"
	TypeClaudeAuth       ProviderType = "claude-auth"
	TypeCodex            ProviderType = "codex"
	TypeOpenAICompatible ProviderType = "openai-compatible"
	TypeGemini           ProviderType = "gemini"
	TypeGeminiCLI        ProviderType = "gemini-cli"
)

// Preset 逐字对应 Node 的 `PresetConfig`（`presets.ts:19-32`）。
type Preset struct {
	ID                     string
	Description            string
	ProviderTypes          []ProviderType
	Payload                map[string]any
	DefaultSuccessContains string
	DefaultModel           string
	Path                   string
	UserAgent              string
	ExtraHeaders           map[string]string
	Score                  int
	ModelHints             []string
	URLHints               []string
}

// claudeCLIUserAgent 是三个 Claude 预设共用的 UA（`presets.ts:60,76,93`）。
const claudeCLIUserAgent = "claude-cli/2.1.84 (external, cli)"

// geminiCLIUserAgent 是两个 Gemini 预设共用的 UA（`presets.ts:190,205`）。
const geminiCLIUserAgent = "GeminiCLI/v24.11.0 (linux; x64)"

// geminiCLIExtraHeaders 是两个 Gemini 预设共用的额外头。
var geminiCLIExtraHeaders = map[string]string{
	"x-goog-api-client": "google-genai-sdk/1.30.0 gl-node/v24.11.0",
}

// presetOrder 是 PRESET_MAPPING 的出现顺序，决定 ExecutionCandidates 的稳定排序基准。
var presetOrder = map[ProviderType][]string{
	TypeClaude:           {"cc_haiku_basic", "cc_beta_cli", "cc_public_thinking"},
	TypeClaudeAuth:       {"cc_haiku_basic", "cc_beta_cli", "cc_public_thinking"},
	TypeCodex:            {"cx_codex_basic", "cx_gpt_basic"},
	TypeOpenAICompatible: {"oa_chat_basic", "oa_chat_stream"},
	TypeGemini:           {"gm_flash_basic", "gm_pro_basic"},
	TypeGeminiCLI:        {"gm_flash_basic", "gm_pro_basic"},
}

// presets 是预设表，逐字段对应 `presets.ts` 的 PRESETS 字面量。
var presets = map[string]Preset{
	"cc_haiku_basic": {
		ID:                     "cc_haiku_basic",
		Description:            "Claude CLI haiku stream",
		ProviderTypes:          []ProviderType{TypeClaude, TypeClaudeAuth},
		Payload:                mustLoadPayload("cc_haiku_basic"),
		DefaultSuccessContains: "pong",
		DefaultModel:           "claude-haiku-4-5-20251001",
		Path:                   "/v1/messages",
		UserAgent:              claudeCLIUserAgent,
		ExtraHeaders: map[string]string{
			"Anthropic-Beta": "oauth-2025-04-20,interleaved-thinking-2025-05-14",
			"Anthropic-Dangerous-Direct-Browser-Access": "true",
			"X-App": "cli",
		},
		Score:      100,
		ModelHints: []string{"haiku"},
	},
	"cc_beta_cli": {
		ID:                     "cc_beta_cli",
		Description:            "Claude CLI beta relay profile",
		ProviderTypes:          []ProviderType{TypeClaude, TypeClaudeAuth},
		Payload:                mustLoadPayload("cc_beta_cli"),
		DefaultSuccessContains: "pong",
		DefaultModel:           "claude-haiku-4-5-20251001",
		Path:                   "/v1/messages?beta=true",
		UserAgent:              claudeCLIUserAgent,
		ExtraHeaders: map[string]string{
			"Anthropic-Beta": "oauth-2025-04-20,interleaved-thinking-2025-05-14," +
				"context-management-2025-06-27,prompt-caching-scope-2026-01-05",
			"Anthropic-Dangerous-Direct-Browser-Access": "true",
			"X-App": "cli",
		},
		Score:    90,
		URLHints: []string{"beta=true", "gateway", "relay", "router", "worker", "proxy"},
	},
	"cc_public_thinking": {
		ID:                     "cc_public_thinking",
		Description:            "Public Claude with thinking enabled",
		ProviderTypes:          []ProviderType{TypeClaude, TypeClaudeAuth},
		Payload:                mustLoadPayload("public_cc_base"),
		DefaultSuccessContains: "pong",
		DefaultModel:           "claude-sonnet-4-5-20250929",
		Path:                   "/v1/messages",
		UserAgent:              "claude-cli/2.1.76 (external, cli)",
		ExtraHeaders: map[string]string{
			"Anthropic-Beta": "interleaved-thinking-2025-05-14,context-management-2025-06-27",
			"Anthropic-Dangerous-Direct-Browser-Access": "true",
			"X-App": "cli",
		},
		Score:      80,
		ModelHints: []string{"sonnet", "opus"},
	},
	"cx_codex_basic": {
		ID:                     "cx_codex_basic",
		Description:            "Codex Responses stream",
		ProviderTypes:          []ProviderType{TypeCodex},
		Payload:                mustLoadPayload("cx_codex_basic"),
		DefaultSuccessContains: "pong",
		DefaultModel:           "gpt-5.5",
		Path:                   "/v1/responses",
		UserAgent:              "Codex-CLI/1.0",
		ExtraHeaders:           map[string]string{"openai-beta": "responses=experimental"},
		Score:                  100,
		ModelHints:             []string{"codex"},
	},
	"cx_gpt_basic": {
		ID:                     "cx_gpt_basic",
		Description:            "Responses API GPT profile",
		ProviderTypes:          []ProviderType{TypeCodex},
		Payload:                mustLoadPayload("cx_gpt_basic"),
		DefaultSuccessContains: "pong",
		DefaultModel:           "gpt-5.5",
		Path:                   "/v1/responses",
		UserAgent:              "Codex-CLI/1.0",
		ExtraHeaders:           map[string]string{"openai-beta": "responses=experimental"},
		Score:                  85,
		ModelHints:             []string{"gpt-", "o1", "o3", "o4"},
	},
	"oa_chat_basic": {
		ID:                     "oa_chat_basic",
		Description:            "OpenAI compatible chat completion",
		ProviderTypes:          []ProviderType{TypeOpenAICompatible},
		Payload:                mustLoadPayload("oa_chat_basic"),
		DefaultSuccessContains: "pong",
		DefaultModel:           "gpt-4.1-mini",
		Path:                   "/v1/chat/completions",
		UserAgent:              "OpenAI-Compatible/2026.04",
		Score:                  100,
	},
	"oa_chat_stream": {
		ID:                     "oa_chat_stream",
		Description:            "OpenAI compatible chat completion stream",
		ProviderTypes:          []ProviderType{TypeOpenAICompatible},
		Payload:                mustLoadPayload("oa_chat_stream"),
		DefaultSuccessContains: "pong",
		DefaultModel:           "gpt-4.1-mini",
		Path:                   "/v1/chat/completions",
		UserAgent:              "OpenAI-Compatible/2026.04",
		ExtraHeaders:           map[string]string{"Accept": "application/json, text/event-stream"},
		Score:                  85,
	},
	"gm_flash_basic": {
		ID:                     "gm_flash_basic",
		Description:            "Gemini generateContent flash",
		ProviderTypes:          []ProviderType{TypeGemini, TypeGeminiCLI},
		Payload:                mustLoadPayload("gm_flash_basic"),
		DefaultSuccessContains: "pong",
		DefaultModel:           "gemini-2.5-flash",
		Path:                   "/v1beta/models/{model}:generateContent",
		UserAgent:              geminiCLIUserAgent,
		ExtraHeaders:           geminiCLIExtraHeaders,
		Score:                  100,
		ModelHints:             []string{"flash"},
	},
	"gm_pro_basic": {
		ID:                     "gm_pro_basic",
		Description:            "Gemini generateContent pro",
		ProviderTypes:          []ProviderType{TypeGemini, TypeGeminiCLI},
		Payload:                mustLoadPayload("gm_pro_basic"),
		DefaultSuccessContains: "pong",
		DefaultModel:           "gemini-2.5-pro",
		Path:                   "/v1beta/models/{model}:generateContent",
		UserAgent:              geminiCLIUserAgent,
		ExtraHeaders:           geminiCLIExtraHeaders,
		Score:                  90,
		ModelHints:             []string{"pro", "thinking"},
	},
}

// PresetIDsForTestPresetsEndpoint 是 `GET /providers/test:presets` 的返回顺序来源：
// 与 Node 一致，直接取该类型的 PRESET_MAPPING 顺序。
func PresetIDsForTestPresetsEndpoint(t ProviderType) []string {
	ids := presetOrder[t]
	out := make([]string, len(ids))
	copy(out, ids)
	return out
}

func mustLoadPayload(name string) map[string]any {
	raw, err := payloadFS.ReadFile("data/" + name + ".json")
	if err != nil {
		panic(fmt.Sprintf("providertest: 载荷缺失 %s: %v", name, err))
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		panic(fmt.Sprintf("providertest: 载荷解析失败 %s: %v", name, err))
	}
	return payload
}

// GetPreset 复刻 getPreset。
func GetPreset(presetID string) (Preset, bool) {
	preset, ok := presets[presetID]
	return preset, ok
}

// PresetsForProvider 复刻 getPresetsForProvider（保持 PRESET_MAPPING 顺序）。
func PresetsForProvider(t ProviderType) []Preset {
	ids := presetOrder[t]
	out := make([]Preset, 0, len(ids))
	for _, id := range ids {
		if preset, ok := presets[id]; ok {
			out = append(out, preset)
		}
	}
	return out
}

// IsPresetCompatible 复刻 isPresetCompatible。
func IsPresetCompatible(presetID string, t ProviderType) bool {
	for _, id := range presetOrder[t] {
		if id == presetID {
			return true
		}
	}
	return false
}

// DefaultPreset 复刻 getDefaultPreset（取该类型第一个）。
func DefaultPreset(t ProviderType) (Preset, bool) {
	list := PresetsForProvider(t)
	if len(list) == 0 {
		return Preset{}, false
	}
	return list[0], true
}

// PresetPayload 复刻 getPresetPayload：深拷贝模板，且**仅当模板里有 model 键**时才覆写
// （Gemini 走 URL path，不该把 model 塞回 body）。
func PresetPayload(presetID, model string) (map[string]any, error) {
	preset, ok := presets[presetID]
	if !ok {
		return nil, fmt.Errorf("preset not found: %s", presetID)
	}
	payload := deepCopyMap(preset.Payload)
	if model != "" {
		if _, has := payload["model"]; has {
			payload["model"] = model
		}
	}
	return payload, nil
}

// ExecutionPresetCandidates 复刻 getExecutionPresetCandidates：按 scorePreset 降序。
//
// Node 用 Array.sort（V8 稳定排序），故同分保持 PRESET_MAPPING 顺序；此处用 SliceStable。
func ExecutionPresetCandidates(t ProviderType, providerURL, model string) []Preset {
	list := PresetsForProvider(t)
	scores := make(map[string]int, len(list))
	for _, preset := range list {
		scores[preset.ID] = scorePreset(preset, t, providerURL, model)
	}
	sort.SliceStable(list, func(i, j int) bool {
		return scores[list[i].ID] > scores[list[j].ID]
	})
	return list
}

// scorePreset 复刻 presets.ts 的 scorePreset（含 codex/gemini 的两处 provider 专属加成，
// 以及「命中 modelHints 后不再叠加 boost」的规则）。
func scorePreset(preset Preset, t ProviderType, providerURL, model string) int {
	score := preset.Score
	lowerModel := strings.ToLower(model)
	lowerURL := strings.ToLower(providerURL)

	matchedModelHint := false
	for _, hint := range preset.ModelHints {
		if strings.Contains(lowerModel, hint) {
			matchedModelHint = true
			break
		}
	}
	if matchedModelHint {
		score += 50
	}
	for _, hint := range preset.URLHints {
		if strings.Contains(lowerURL, hint) {
			score += 30
			break
		}
	}
	if model == "" {
		return score
	}

	switch t {
	case TypeCodex:
		if !matchedModelHint && strings.Contains(lowerModel, "codex") && preset.ID == "cx_codex_basic" {
			score += 40
		}
		if !matchedModelHint && !strings.Contains(lowerModel, "codex") && preset.ID == "cx_gpt_basic" {
			score += 40
		}
	case TypeGemini, TypeGeminiCLI:
		if !matchedModelHint && strings.Contains(lowerModel, "flash") && preset.ID == "gm_flash_basic" {
			score += 40
		}
		if !matchedModelHint && (strings.Contains(lowerModel, "pro") || strings.Contains(lowerModel, "thinking")) && preset.ID == "gm_pro_basic" {
			score += 40
		}
	}
	return score
}

// deepCopyMap 复刻 structuredClone 的语义（本处只需要 map/切片/标量的深拷贝）。
func deepCopyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = deepCopyValue(value)
	}
	return out
}

func deepCopyValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return deepCopyMap(typed)
	case []any:
		out := make([]any, len(typed))
		for i, item := range typed {
			out[i] = deepCopyValue(item)
		}
		return out
	default:
		return value
	}
}
