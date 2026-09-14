package forward

import (
	"encoding/json"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// Gemini 线的转发事实：默认端点与凭据形态。
//
// 与 Node 的对应物：
//   - 端点常量：src/app/v1/_lib/gemini/protocol.ts 的 GEMINI_PROTOCOL.{OFFICIAL_ENDPOINT,CLI_ENDPOINT}
//   - 凭据解析：src/app/v1/_lib/gemini/auth.ts 的 GeminiAuth.{getAccessToken,isApiKey}
//
// 为什么这些放在 forward 而不放 convert：gemini 不参与协议转换（见 convert/codec_gemini.go），
// 它只在**转发层**按供应商类型被特殊处理——与 Node 在 forwarder.ts:3299 的分派位置一致。

const (
	// geminiOfficialEndpoint 是 gemini 供应商未配 url 时的官方端点（Node 的降级顺序：
	// endpoint.url > provider.url > 官方端点）。
	geminiOfficialEndpoint = "https://generativelanguage.googleapis.com/v1beta"
	// geminiCLIEndpoint 是 gemini-cli 供应商的默认端点。
	geminiCLIEndpoint = "https://cloudcode-pa.googleapis.com/v1internal"

	// geminiAPIKeyHeader 是 API Key 形态的鉴权头（OAuth 形态走 Authorization: Bearer）。
	geminiAPIKeyHeader = "x-goog-api-key"
	// geminiCLIClientHeader 是 gemini-cli 的客户端标识，Node 恒写死该值。
	geminiCLIClientHeader = "x-goog-api-client"
	// geminiCLIClientValue 对齐 Node 的 `GeminiCLI/1.0`。
	geminiCLIClientValue = "GeminiCLI/1.0"
	// geminiDefaultUserAgent 是客户端没带 UA 时的兜底（Node: "claude-code-hub"）。
	geminiDefaultUserAgent = "claude-code-hub"
)

// IsGeminiProviderType 报告某供应商类型是否走 Gemini 原生透传。
func IsGeminiProviderType(providerType convert.ProviderType) bool {
	return providerType == convert.ProviderGemini || providerType == convert.ProviderGeminiCLI
}

// GeminiDefaultEndpoint 返回该供应商类型的默认上游端点；非 gemini 系返回空串。
func GeminiDefaultEndpoint(providerType convert.ProviderType) string {
	switch providerType {
	case convert.ProviderGemini:
		return geminiOfficialEndpoint
	case convert.ProviderGeminiCLI:
		return geminiCLIEndpoint
	default:
		return ""
	}
}

// geminiCredential 解析供应商凭据，返回「用于上游鉴权的令牌」与「是否按 API Key 发送」。
//
// Node 语义（auth.ts）：
//   - key 是 JSON（claude-code-hub 里常见的是 OAuth 凭据对象）→ 取 access_token，按 Bearer 发；
//   - 否则原样当 API Key，但 `ya29.` 前缀是 Google 的 access token 形态 → 仍按 Bearer 发。
//
// **已知缺口（如实记录，不静默降级）**：Node 在 access_token 过期且带 refresh 三件套时
// 会向 oauth2.googleapis.com 换新 token；Go 侧当前没有这段刷新逻辑（本仓也没有既有的
// OAuth 刷新设施），故这里只取现值。刷新的行为差异已写进报告，且它不影响
// 「路径/正文/鉴权头形态」这些本次要对齐的语义。
func geminiCredential(rawKey string) (token string, isAPIKey bool) {
	trimmed := strings.TrimSpace(rawKey)
	if !strings.HasPrefix(trimmed, "{") {
		return trimmed, !strings.HasPrefix(trimmed, "ya29.")
	}

	var parsed struct {
		AccessToken *string `json:"access_token"`
	}
	if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil || parsed.AccessToken == nil || *parsed.AccessToken == "" {
		// 与 Node 的 parse 失败 / 缺 access_token 等价：退回原文字符串路径
		// （Node 的 parse 在 JSON.parse 抛错时返回原文，isApiKey 于是按字符串判定）。
		return trimmed, !strings.HasPrefix(trimmed, "ya29.")
	}
	return *parsed.AccessToken, false
}
