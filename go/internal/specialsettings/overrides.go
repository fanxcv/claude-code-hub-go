package specialsettings

// 供应商级参数覆写的审计条目（`message_request.special_settings`）。
//
// 形状真源：`src/types/special-settings.ts` 的 `ProviderParameterOverrideSpecialSetting`（:33-47）
// 与 `GeminiGoogleSearchOverrideSpecialSetting`（:300-310）；产出条件照 Node 的
// `applyCodexProviderOverridesWithAudit` / `applyAnthropicProviderOverridesWithAudit`
// （`src/lib/{codex,anthropic}/provider-overrides.ts`）与
// `applyGeminiGoogleSearchOverrideWithAudit`（`src/lib/gemini/provider-overrides.ts`）。
//
// 两类条目**不可混同**（与整流器同一条纪律）：
//   - provider_parameter_override：**供应商作用域**（scope="provider"），带 providerId /
//     providerName / providerType，载荷是 `changes[{path,before,after,changed}]`；
//   - gemini_google_search_override：**请求作用域**（scope="request"），带动作与偏好原值，
//     没有 changes 数组。
//
// 共同点：**无偏好命中时 Node 不写条目**（audit 返回 null），故下面每个构造函数都要求调用方
// 已经判定命中——本包不替调用方猜「该不该写」。

const (
	// TypeProviderParameterOverride 是供应商级参数覆写（codex 六件 / anthropic 三件）的审计类型。
	TypeProviderParameterOverride = "provider_parameter_override"
	// TypeGeminiGoogleSearchOverride 是 gemini googleSearch 工具的注入/移除审计类型。
	TypeGeminiGoogleSearchOverride = "gemini_google_search_override"
)

// OverrideChange 是一条字段级改动，对应 Node 的 `{path, before, after, changed}`。
//
// Before/After 取值必须是 JSON 标量（string/number/boolean）或 nil——Node 的 `toAuditValue`
// 对对象与数组一律记 null，故调用方在取值时就该收敛到标量，不要在这里传结构体。
type OverrideChange struct {
	Path    string
	Before  any
	After   any
	Changed bool
}

// ProviderParameterOverrideEntry 组一条 provider_parameter_override。
//
// `changed` 是 changes 里有没有任何一条真的变了（Node：`changes.some(c => c.changed)`）：
// 它与 `hit` 不同——偏好值恰好等于请求现值时 hit=true 而 changed=false，前端据此区分
// 「配了但本次无改动」与「真的改了」。
func ProviderParameterOverrideEntry(
	providerID int64,
	providerName string,
	providerType string,
	changes []OverrideChange,
) map[string]any {
	changed := false
	list := make([]any, 0, len(changes))
	for _, change := range changes {
		if change.Changed {
			changed = true
		}
		list = append(list, map[string]any{
			"path":    change.Path,
			"before":  change.Before,
			"after":   change.After,
			"changed": change.Changed,
		})
	}
	return map[string]any{
		"type":         TypeProviderParameterOverride,
		"scope":        "provider",
		"providerId":   providerID,
		"providerName": providerName,
		"providerType": providerType,
		"hit":          true,
		"changed":      changed,
		"changes":      list,
	}
}

// GeminiGoogleSearchOverrideEntry 组一条 gemini_google_search_override。
//
// action 为 inject（注入）、remove（移除）或 passthrough（偏好与现状一致，无需动作）；
// hadGoogleSearchInRequest 是**改写前**是否已有 googleSearch 工具（Node 的同名字段）。
func GeminiGoogleSearchOverrideEntry(
	providerID int64,
	providerName string,
	action string,
	preference string,
	hadGoogleSearchInRequest bool,
) map[string]any {
	return map[string]any{
		"type":                     TypeGeminiGoogleSearchOverride,
		"scope":                    "request",
		"hit":                      true,
		"providerId":               providerID,
		"providerName":             providerName,
		"action":                   action,
		"preference":               preference,
		"hadGoogleSearchInRequest": hadGoogleSearchInRequest,
	}
}
