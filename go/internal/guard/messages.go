package guard

import (
	"sort"
	"strconv"
	"strings"
)

// 本文件复刻认证守卫用到的三条 i18n 文案（messages/<locale>/errors.json 的 PROXY_*），
// 以及语种解析。
//
// 为什么不做成通用 i18n 层：数据面热路径只用到这三条，且必须在 Go 侧逐字复刻，任何
// 模板化抽象都会让「文案是否与 Node 一致」变得不可断言。真需要更多文案时再扩这张表。
const (
	// MessageInvalidAPIKey 对应 errors.PROXY_INVALID_API_KEY。
	MessageInvalidAPIKey = "PROXY_INVALID_API_KEY"
	// MessageAPIKeyDisabled 对应 errors.PROXY_API_KEY_DISABLED。
	MessageAPIKeyDisabled = "PROXY_API_KEY_DISABLED"
	// MessageAPIKeyExpired 对应 errors.PROXY_API_KEY_EXPIRED。
	MessageAPIKeyExpired = "PROXY_API_KEY_EXPIRED"
)

// DefaultLocale 与 i18n/config.ts 的 defaultLocale 一致。
const DefaultLocale = "zh-CN"

// SupportedLocales 与 i18n/config.ts 的 locales 一致（顺序也保持原样）。
var SupportedLocales = []string{"zh-CN", "zh-TW", "en", "ru", "ja"}

// LocaleCookieName 与 i18n/config.ts 的 localeCookieName 一致。
const LocaleCookieName = "NEXT_LOCALE"

// proxyErrorMessages 是三条文案的全部语种取值，逐字取自 messages/<locale>/errors.json。
var proxyErrorMessages = map[string]map[string]string{
	MessageInvalidAPIKey: {
		"zh-CN": "API 密钥无效。提供的密钥不存在或已被删除。",
		"zh-TW": "API 金鑰無效。提供的金鑰不存在或已被刪除。",
		"en":    "Invalid API key. The provided key does not exist or has been deleted.",
		"ru":    "Неверный API-ключ. Указанный ключ не существует или был удалён.",
		"ja":    "API キーが無効です。指定されたキーは存在しないか、削除されています。",
	},
	MessageAPIKeyDisabled: {
		"zh-CN": "API 密钥已被禁用。请联系管理员重新启用，或使用其他可用密钥。",
		"zh-TW": "API 金鑰已被停用。請聯絡管理員重新啟用，或使用其他可用金鑰。",
		"en":    "This API key has been disabled. Please contact your administrator to re-enable it, or use a different key.",
		"ru":    "Этот API-ключ отключён. Обратитесь к администратору, чтобы повторно включить его, или используйте другой ключ.",
		"ja":    "この API キーは無効化されています。管理者に再有効化を依頼するか、別のキーをご使用ください。",
	},
	MessageAPIKeyExpired: {
		"zh-CN": "API 密钥已过期。请联系管理员续期或更换密钥。",
		"zh-TW": "API 金鑰已過期。請聯絡管理員續期或更換金鑰。",
		"en":    "This API key has expired. Please contact your administrator to renew it or rotate to a new key.",
		"ru":    "Срок действия этого API-ключа истёк. Обратитесь к администратору, чтобы продлить срок, или замените ключ.",
		"ja":    "この API キーは期限切れです。管理者に更新を依頼するか、新しいキーへ切り替えてください。",
	},
}

// Message 取一条文案；语种不支持时退回默认语种（对应 next-intl 的 fallback 行为）。
func Message(locale, code string) string {
	byLocale, ok := proxyErrorMessages[code]
	if !ok {
		return ""
	}
	if text, ok := byLocale[locale]; ok {
		return text
	}
	return byLocale[DefaultLocale]
}

// ResolveLocale 解析请求语种。
//
// 顺序对齐 Node 侧 next-intl 的行为：NEXT_LOCALE cookie 优先，其次是 Accept-Language
// 协商，最后退回默认语种。协商只做前缀匹配（zh → zh-CN，zh-TW 优先于 zh），并按 q 值
// 从高到低尝试——与 next-intl 的默认协商规则一致。
func ResolveLocale(cookieLocale, acceptLanguage string) string {
	if normalized, ok := matchLocale(cookieLocale); ok {
		return normalized
	}
	for _, candidate := range parseAcceptLanguage(acceptLanguage) {
		if normalized, ok := matchLocale(candidate); ok {
			return normalized
		}
	}
	return DefaultLocale
}

// matchLocale 把一个语种标签归一到受支持的取值。
func matchLocale(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false
	}
	lowered := strings.ToLower(trimmed)
	for _, locale := range SupportedLocales {
		if strings.ToLower(locale) == lowered {
			return locale, true
		}
	}
	// 前缀匹配：zh-TW-Hant 之类更具体的标签退到主标签；zh 优先 zh-CN。
	primary := lowered
	if index := strings.IndexByte(lowered, '-'); index >= 0 {
		primary = lowered[:index]
	}
	for _, locale := range SupportedLocales {
		loweredLocale := strings.ToLower(locale)
		if loweredLocale == primary {
			return locale, true
		}
	}
	for _, locale := range SupportedLocales {
		if strings.HasPrefix(strings.ToLower(locale), primary+"-") {
			return locale, true
		}
	}
	return "", false
}

// parseAcceptLanguage 按 q 值降序返回 Accept-Language 里的语种标签。
func parseAcceptLanguage(raw string) []string {
	type weighted struct {
		tag   string
		score float64
		order int
	}
	var items []weighted
	for index, part := range strings.Split(raw, ",") {
		fields := strings.Split(part, ";")
		tag := strings.TrimSpace(fields[0])
		if tag == "" {
			continue
		}
		score := 1.0
		for _, parameter := range fields[1:] {
			parameter = strings.TrimSpace(parameter)
			if !strings.HasPrefix(strings.ToLower(parameter), "q=") {
				continue
			}
			parsed, err := strconv.ParseFloat(strings.TrimSpace(parameter[2:]), 64)
			if err == nil {
				score = parsed
			}
		}
		items = append(items, weighted{tag: tag, score: score, order: index})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].score != items[j].score {
			return items[i].score > items[j].score
		}
		return items[i].order < items[j].order
	})
	tags := make([]string, 0, len(items))
	for _, item := range items {
		tags = append(tags, item.tag)
	}
	return tags
}

// cookieValue 从 Cookie 头里取指定 cookie 的值。
func cookieValue(cookieHeader, name string) string {
	for _, part := range strings.Split(cookieHeader, ";") {
		trimmed := strings.TrimSpace(part)
		if trimmed == "" {
			continue
		}
		key, value, found := strings.Cut(trimmed, "=")
		if !found {
			continue
		}
		if strings.TrimSpace(key) == name {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
