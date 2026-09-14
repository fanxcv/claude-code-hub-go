package pubstatus

import (
	"encoding/json"
	"math"
	"sort"
	"strconv"
	"strings"
)

// 本文件是 Node src/lib/public-status/config.ts 的转写：分组「公开状态描述」的解析与
// 「启用公开状态的分组」收集。描述是**存在 provider_groups.description 里的 JSON 文本**
// （不是独立列），版本号 2；版本高于 2 的一律当普通文本处理（前向兼容：新版本写的描述在
// 老代码眼里只是备注，不会被误读成配置）。
//
// 分组被收进公开快照的三个门槛（照 Node，缺一不可）：
//  1. description 能解析出 publicStatus 段；
//  2. publicModels 去重后非空；
//  3. slug 不与更早的分组相撞（相撞时按 groupName 的稳定哈希加后缀，不是丢弃）。

// publicStatusDescriptionVersion 是当前描述格式版本（config.ts:3）。
const publicStatusDescriptionVersion = 2

// publicStatusSlugMaxLength 等三个常量照 config.ts:41-44。
const (
	publicStatusSlugMaxLength     = 64
	publicStatusSlugSuffixLength  = 6
	publicStatusSlugFallbackPrefi = "group"
)

// validProviderTypes 是 publicModels 里 providerTypeOverride 的合法取值（config.ts:5-12）。
var validProviderTypes = map[string]struct{}{
	"claude":            {},
	"claude-auth":       {},
	"codex":             {},
	"gemini":            {},
	"gemini-cli":        {},
	"openai-compatible": {},
}

// PublicStatusModelConfig 是分组配置里的单个公开模型（config.ts:14-17）。
type PublicStatusModelConfig struct {
	ModelKey             string
	ProviderTypeOverride string
}

// PublicStatusGroupConfig 是描述里的 publicStatus 段（config.ts:19-25）。
type PublicStatusGroupConfig struct {
	DisplayName     string
	PublicGroupSlug string
	ExplanatoryCopy *string
	SortOrder       *float64
	PublicModels    []PublicStatusModelConfig
}

// ParsedPublicStatusDescription 是描述解析结果（config.ts:27-30）。
type ParsedPublicStatusDescription struct {
	Note         *string
	PublicStatus *PublicStatusGroupConfig
}

// EnabledPublicStatusGroup 是「启用公开状态」的分组（config.ts:37-44）。
type EnabledPublicStatusGroup struct {
	GroupName       string
	DisplayName     string
	PublicGroupSlug string
	ExplanatoryCopy *string
	SortOrder       float64
	PublicModels    []PublicStatusModelConfig
}

// PublicStatusConfiguredGroupInput 是收集函数的入参（config.ts:32-35）。
type PublicStatusConfiguredGroupInput struct {
	GroupName string
	ParsedPublicStatusDescription
}

// sanitizeString 复刻 config.ts:79-88：非字符串或空白 → 无。
func sanitizeString(value any) (string, bool) {
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "", false
	}
	return trimmed, true
}

// sanitizeProviderType 复刻 config.ts:90-97：不在白名单 → 无。
func sanitizeProviderType(value any) (string, bool) {
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	normalized := strings.TrimSpace(text)
	if _, valid := validProviderTypes[normalized]; !valid {
		return "", false
	}
	return normalized, true
}

// sanitizePublicModels 复刻 config.ts:99-129：允许裸字符串与对象两种写法，按 modelKey 去重。
func sanitizePublicModels(raw any) []PublicStatusModelConfig {
	entries, ok := raw.([]any)
	if !ok {
		return nil
	}

	seen := make(map[string]struct{}, len(entries))
	normalized := make([]PublicStatusModelConfig, 0, len(entries))
	for _, entry := range entries {
		var modelKey string
		var hasKey bool
		var providerTypeOverride string

		if text, isString := entry.(string); isString {
			modelKey, hasKey = sanitizeString(text)
		} else if object, isObject := entry.(map[string]any); isObject {
			modelKey, hasKey = sanitizeString(object["modelKey"])
			if value, present := sanitizeProviderType(object["providerTypeOverride"]); present {
				providerTypeOverride = value
			}
		}

		if !hasKey {
			continue
		}
		if _, duplicate := seen[modelKey]; duplicate {
			continue
		}
		seen[modelKey] = struct{}{}
		normalized = append(normalized, PublicStatusModelConfig{
			ModelKey:             modelKey,
			ProviderTypeOverride: providerTypeOverride,
		})
	}
	return normalized
}

// sanitizeLegacyPublicModels 复刻 config.ts:131-141：新字段为空时回退旧字段名。
func sanitizeLegacyPublicModels(publicModels any, fallbacks ...any) []PublicStatusModelConfig {
	if normalized := sanitizePublicModels(publicModels); len(normalized) > 0 {
		return normalized
	}
	for _, fallback := range fallbacks {
		if normalized := sanitizePublicModels(fallback); len(normalized) > 0 {
			return normalized
		}
	}
	return nil
}

// GetPublicStatusModelKeys 复刻 config.ts:143-145。
func GetPublicStatusModelKeys(publicModels []PublicStatusModelConfig) []string {
	keys := make([]string, 0, len(publicModels))
	for _, model := range publicModels {
		keys = append(keys, model.ModelKey)
	}
	return keys
}

// createStablePublicGroupSlugSuffix 复刻 config.ts:147-156 的 FNV-1a（按**码点**迭代）。
func createStablePublicGroupSlugSuffix(input string) string {
	hash := uint32(0x811c9dc5)
	for _, character := range input {
		hash ^= uint32(character)
		hash *= 0x01000193
	}
	suffix := strconv.FormatUint(uint64(hash), 36)
	for len(suffix) < publicStatusSlugSuffixLength {
		suffix = "0" + suffix
	}
	if len(suffix) > publicStatusSlugSuffixLength {
		suffix = suffix[:publicStatusSlugSuffixLength]
	}
	return suffix
}

// appendStablePublicGroupSlugSuffix 复刻 config.ts:158-163。
func appendStablePublicGroupSlugSuffix(base string, suffix string) string {
	prefixLength := publicStatusSlugMaxLength - len(suffix) - 1
	if prefixLength < 1 {
		prefixLength = 1
	}
	prefix := strings.TrimRight(sliceRunes(base, prefixLength), "-")
	if prefix == "" {
		prefix = publicStatusSlugFallbackPrefi
	}
	return prefix + "-" + suffix
}

// sliceRunes 按**码点**截断（JS 的 String#slice 按 UTF-16 码元，含 BMP 时与码点一致）。
func sliceRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

// slugifyPublicGroupAscii 复刻 config.ts:165-172。
func slugifyPublicGroupAscii(input string) string {
	trimmed := strings.ToLower(strings.TrimSpace(input))
	var builder strings.Builder
	lastWasDash := false
	for _, character := range trimmed {
		isAlphaNumeric := (character >= 'a' && character <= 'z') ||
			(character >= '0' && character <= '9')
		if isAlphaNumeric {
			builder.WriteRune(character)
			lastWasDash = false
			continue
		}
		if !lastWasDash {
			builder.WriteByte('-')
			lastWasDash = true
		}
	}
	slug := strings.Trim(builder.String(), "-")
	return sliceRunes(slug, publicStatusSlugMaxLength)
}

// SlugifyPublicGroup 复刻 config.ts:174-193：纯 ASCII 用滑串，含非 ASCII 追加稳定哈希后缀。
func SlugifyPublicGroup(input string) string {
	trimmed := strings.ToLower(strings.TrimSpace(input))
	if trimmed == "" {
		return ""
	}

	asciiSlug := slugifyPublicGroupAscii(trimmed)
	hasNonASCII := false
	for _, character := range trimmed {
		if character > 0x7f {
			hasNonASCII = true
			break
		}
	}
	if !hasNonASCII {
		return asciiSlug
	}

	suffix := createStablePublicGroupSlugSuffix(trimmed)
	if asciiSlug == "" {
		return publicStatusSlugFallbackPrefi + "-" + suffix
	}
	return appendStablePublicGroupSlugSuffix(asciiSlug, suffix)
}

// NormalizePublicGroupSlug 复刻 config.ts:195-198。
func NormalizePublicGroupSlug(groupName string, publicGroupSlug string) string {
	candidate := strings.TrimSpace(publicGroupSlug)
	if candidate == "" {
		candidate = groupName
	}
	normalized := SlugifyPublicGroup(candidate)
	if normalized != "" {
		return normalized
	}
	return SlugifyPublicGroup(groupName)
}

// createAvailablePublicGroupSlug 复刻 config.ts:200-216。
func createAvailablePublicGroupSlug(baseSlug string, groupName string, usedSlugs map[string]struct{}) string {
	counter := 1
	candidate := baseSlug
	for {
		if _, used := usedSlugs[candidate]; !used {
			return candidate
		}
		suffixSource := groupName
		if counter > 1 {
			suffixSource = groupName + "-" + strconv.Itoa(counter)
		}
		base := baseSlug
		if base == "" {
			base = publicStatusSlugFallbackPrefi
		}
		candidate = appendStablePublicGroupSlugSuffix(base, createStablePublicGroupSlugSuffix(suffixSource))
		counter++
	}
}

// ParsePublicStatusDescription 复刻 config.ts:232-291。
func ParsePublicStatusDescription(description *string) ParsedPublicStatusDescription {
	if description == nil || *description == "" {
		return ParsedPublicStatusDescription{}
	}
	raw := *description

	var parsed any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		note := raw
		return ParsedPublicStatusDescription{Note: &note}
	}

	object, isObject := parsed.(map[string]any)
	if !isObject {
		note := raw
		return ParsedPublicStatusDescription{Note: &note}
	}
	if version, isNumber := object["version"].(float64); isNumber && version > publicStatusDescriptionVersion {
		note := raw
		return ParsedPublicStatusDescription{Note: &note}
	}

	var note *string
	if value, ok := sanitizeString(object["note"]); ok {
		note = &value
	}

	publicStatusRaw, hasPublicStatus := object["publicStatus"].(map[string]any)
	if !hasPublicStatus {
		return ParsedPublicStatusDescription{Note: note}
	}

	var displayName string
	if value, ok := sanitizeString(publicStatusRaw["displayName"]); ok {
		displayName = value
	}
	var publicGroupSlug string
	if value, ok := sanitizeString(publicStatusRaw["publicGroupSlug"]); ok {
		publicGroupSlug = value
	}
	var explanatoryCopy *string
	if value, ok := sanitizeString(publicStatusRaw["explanatoryCopy"]); ok {
		explanatoryCopy = &value
	}
	var sortOrder *float64
	if value, isNumber := publicStatusRaw["sortOrder"].(float64); isNumber && !math.IsNaN(value) && !math.IsInf(value, 0) {
		sortOrder = &value
	}
	publicModels := sanitizeLegacyPublicModels(
		publicStatusRaw["publicModels"], publicStatusRaw["publicModelKeys"], publicStatusRaw["modelIds"])

	groupConfig := PublicStatusGroupConfig{
		DisplayName:     displayName,
		PublicGroupSlug: publicGroupSlug,
		ExplanatoryCopy: explanatoryCopy,
		SortOrder:       sortOrder,
		PublicModels:    publicModels,
	}
	isEmpty := groupConfig.DisplayName == "" && groupConfig.PublicGroupSlug == "" &&
		groupConfig.ExplanatoryCopy == nil && groupConfig.SortOrder == nil &&
		len(groupConfig.PublicModels) == 0
	if isEmpty {
		return ParsedPublicStatusDescription{Note: note}
	}
	return ParsedPublicStatusDescription{Note: note, PublicStatus: &groupConfig}
}

// CollectEnabledPublicStatusGroups 复刻 config.ts:355-409（duplicateSlugStrategy 固定为 suffix：
// 公开状态页的重发路径用的就是它）。
func CollectEnabledPublicStatusGroups(groups []PublicStatusConfiguredGroupInput) []EnabledPublicStatusGroup {
	seenGroupNamesBySlug := make(map[string]string, len(groups))
	usedSlugs := make(map[string]struct{}, len(groups))
	enabled := make([]EnabledPublicStatusGroup, 0, len(groups))

	for _, group := range groups {
		var publicModels []PublicStatusModelConfig
		if group.PublicStatus != nil {
			publicModels = sanitizePublicModels(modelConfigsToAny(group.PublicStatus.PublicModels))
		}
		if len(publicModels) == 0 {
			continue
		}

		declaredSlug := ""
		if group.PublicStatus != nil {
			declaredSlug = group.PublicStatus.PublicGroupSlug
		}
		normalizedSlug := NormalizePublicGroupSlug(group.GroupName, declaredSlug)
		publicGroupSlug := normalizedSlug

		if existingName, collision := seenGroupNamesBySlug[publicGroupSlug]; collision {
			_ = existingName
			publicGroupSlug = createAvailablePublicGroupSlug(normalizedSlug, group.GroupName, usedSlugs)
		}

		seenGroupNamesBySlug[publicGroupSlug] = group.GroupName
		usedSlugs[publicGroupSlug] = struct{}{}

		displayName := group.GroupName
		explanatoryCopy := (*string)(nil)
		sortOrder := 0.0
		if group.PublicStatus != nil {
			if trimmed := strings.TrimSpace(group.PublicStatus.DisplayName); trimmed != "" {
				displayName = trimmed
			}
			if group.PublicStatus.ExplanatoryCopy != nil {
				if trimmed := strings.TrimSpace(*group.PublicStatus.ExplanatoryCopy); trimmed != "" {
					explanatoryCopy = &trimmed
				}
			}
			if group.PublicStatus.SortOrder != nil {
				sortOrder = *group.PublicStatus.SortOrder
			}
		}

		enabled = append(enabled, EnabledPublicStatusGroup{
			GroupName:       group.GroupName,
			DisplayName:     displayName,
			PublicGroupSlug: publicGroupSlug,
			ExplanatoryCopy: explanatoryCopy,
			SortOrder:       sortOrder,
			PublicModels:    publicModels,
		})
	}

	sort.SliceStable(enabled, func(left, right int) bool {
		if enabled[left].SortOrder != enabled[right].SortOrder {
			return enabled[left].SortOrder < enabled[right].SortOrder
		}
		return enabled[left].DisplayName < enabled[right].DisplayName
	})
	return enabled
}

// modelConfigsToAny 把已解析的模型配置还原成 sanitizePublicModels 认得的形状。
//
// 往返一次是有意的：Node 的 collect 对 `group.publicStatus.publicModels` 再跑一遍
// sanitizePublicModels（config.ts:361），而不是复用解析阶段的数组。两边形状一致时结果相同，
// 但保留这一步能让「解析阶段」与「收集阶段」各自独立地对齐 Node 的同一函数。
func modelConfigsToAny(models []PublicStatusModelConfig) []any {
	out := make([]any, 0, len(models))
	for _, model := range models {
		entry := map[string]any{"modelKey": model.ModelKey}
		if model.ProviderTypeOverride != "" {
			entry["providerTypeOverride"] = model.ProviderTypeOverride
		}
		out = append(out, entry)
	}
	return out
}
