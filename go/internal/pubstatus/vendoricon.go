package pubstatus

// 本文件由 Node 的 src/lib/public-status/vendor-icon-key.ts 机械转写（同序、同表）。

import "strings"

var publicStatusVendorIconKeys = map[string]struct{}{
	"anthropic":  {},
	"azure":      {},
	"baichuan":   {},
	"bedrock":    {},
	"cohere":     {},
	"deepseek":   {},
	"fireworks":  {},
	"gemini":     {},
	"gemma":      {},
	"generic":    {},
	"groq":       {},
	"hunyuan":    {},
	"internlm":   {},
	"kimi":       {},
	"meta":       {},
	"minimax":    {},
	"mistral":    {},
	"moonshot":   {},
	"nvidia":     {},
	"ollama":     {},
	"openai":     {},
	"openrouter": {},
	"perplexity": {},
	"qwen":       {},
	"sensenova":  {},
	"spark":      {},
	"stepfun":    {},
	"together":   {},
	"volcengine": {},
	"wenxin":     {},
	"xai":        {},
	"yi":         {},
	"zhipuai":    {},
	"] as const;\n\nexport type PublicStatusVendorIconKey = (typeof PUBLIC_STATUS_VENDOR_ICON_KEYS)[number": {},
}

var rawProviderToIconKey = map[string]string{
	"anthropic":                 "anthropic",
	"azure":                     "azure",
	"bedrock":                   "bedrock",
	"cohere_chat":               "cohere",
	"deepseek":                  "deepseek",
	"fireworks_ai":              "fireworks",
	"groq":                      "groq",
	"meta":                      "meta",
	"minimax":                   "minimax",
	"mistral":                   "mistral",
	"nvidia_nim":                "nvidia",
	"ollama":                    "ollama",
	"openai":                    "openai",
	"openrouter":                "openrouter",
	"qwen":                      "qwen",
	"together_ai":               "together",
	"vertex_ai-language-models": "gemini",
	"volcengine":                "volcengine",
	"xai":                       "xai",
	"zhipuai":                   "zhipuai",
	"google":                    "gemini",
	"google-vertex":             "gemini",
	"alibaba":                   "qwen",
	"bytedance":                 "volcengine",
	"moonshotai":                "moonshot",
	"tencent":                   "hunyuan",
	"baidu":                     "wenxin",
	"iflytek":                   "spark",
	"01-ai":                     "yi",
	"amazon":                    "bedrock",
	"amazon-bedrock":            "bedrock",
}

var modelVendorToIconKey = map[string]string{
	"anthropic":  "anthropic",
	"azure":      "azure",
	"baichuan":   "baichuan",
	"bedrock":    "bedrock",
	"amazon":     "bedrock",
	"cohere":     "cohere",
	"deepseek":   "deepseek",
	"google":     "gemini",
	"groq":       "groq",
	"tencent":    "hunyuan",
	"internlm":   "internlm",
	"meta":       "meta",
	"minimax":    "minimax",
	"mistral":    "mistral",
	"moonshotai": "moonshot",
	"nvidia":     "nvidia",
	"ollama":     "ollama",
	"openai":     "openai",
	"openrouter": "openrouter",
	"perplexity": "perplexity",
	"alibaba":    "qwen",
	"sensenova":  "sensenova",
	"iflytek":    "spark",
	"stepfun":    "stepfun",
	"together":   "together",
	"bytedance":  "volcengine",
	"baidu":      "wenxin",
	"xai":        "xai",
	"01-ai":      "yi",
	"zhipuai":    "zhipuai",
}

var providerTypeIconKeys = map[string]string{
	"claude-auth": "anthropic",
	"claude":      "anthropic",
	"codex":       "openai",
	"gemini":      "gemini",
	"gemini-cli":  "gemini",
}

func normalizeVendorIconKey(vendorIconKey string) (string, bool) {
	if vendorIconKey == "" {
		return "", false
	}
	normalized := strings.ToLower(strings.TrimSpace(vendorIconKey))
	if normalized == "" {
		return "", false
	}
	if mapped, ok := rawProviderToIconKey[normalized]; ok {
		return mapped, true
	}
	if _, ok := publicStatusVendorIconKeys[normalized]; ok {
		return normalized, true
	}
	return "", false
}

// resolvePublicStatusVendorIconKey 照 Node 的优先级：providerTypeOverride -> 显式 vendor 键
// -> 从模型名推断 -> 兜底 generic。
func resolvePublicStatusVendorIconKey(modelName string, vendorIconKey string, providerTypeOverride string) string {
	if providerTypeOverride != "" {
		if mapped, ok := providerTypeIconKeys[providerTypeOverride]; ok {
			return mapped
		}
	}

	explicitKey, hasExplicit := normalizeVendorIconKey(vendorIconKey)
	if hasExplicit && explicitKey != "generic" {
		return explicitKey
	}

	inferred := inferVendorFromModelName(modelName)
	if inferred != unknownVendor {
		if mapped, ok := modelVendorToIconKey[inferred]; ok {
			return mapped
		}
	}

	if hasExplicit {
		return explicitKey
	}
	return "generic"
}
