package route

import "encoding/json"

// 本文件是管理面复用的**导出包装**：`internal/adminapi` 不能调用 route 包里的未导出函数，
// 而把语义在 adminapi 里再抄一份就是制造第二处漂移源。包装只转发、不含逻辑。
//
// 消费者：`/providers/model-suggestions`（providers_model_suggestions.go）。

// CheckProviderGroupMatch 导出 checkProviderGroupMatch（Node 的
// actions/providers.ts:5656，与 provider-selector.ts:74 同名同义）。
func CheckProviderGroupMatch(providerGroupTag *string, userGroups []string) bool {
	return matchProviderGroups(providerGroupTag, userGroups)
}

// SplitProviderGroups 导出 splitProviderGroups（Node 的 parseProviderGroups）：
// 按 /[,，\n\r]+/ 切分并去空白。
func SplitProviderGroups(value string) []string {
	return splitProviderGroups(value)
}

// AllowedModelExactPatterns 取出 allowedModels 里 matchType=exact 的 pattern（保序、未去重）。
//
// 与 Node 的 getModelSuggestionsByProviderGroup 同口径（actions/providers.ts:5700-5708）：
// 只收 exact 规则；`normalizeAllowedModelRules` 返回「无规则」时视为不贡献任何 pattern。
func AllowedModelExactPatterns(raw json.RawMessage) []string {
	rules, ok := normalizeAllowedModelRules(raw)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(rules))
	for _, rule := range rules {
		if rule.MatchType != "exact" {
			continue
		}
		if rule.Pattern == "" {
			continue
		}
		out = append(out, rule.Pattern)
	}
	return out
}
