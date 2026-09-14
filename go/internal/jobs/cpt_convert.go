package jobs

import (
	"math"
	"sort"
	"strconv"
	"strings"
)

// 本文件是 CPT v1 -> 内部 ModelPriceData 的转换器，逐条复刻 src/lib/price-sync/cpt-convert.ts。
//
// 为什么逐条对齐：转换结果直接写进 model_prices.price_data，是计费取价的唯一输入；
// 任何「等价但不同」的改写都会让两端对同一张云端价格表算出不同的行。
//
// 与 TS 的差异（全部已复核，不影响结果）：
//   - TS 的 Object.entries 顺序是插入序；本实现在可能产生同名字段的循环里改用**显式有序切片**
//     （chargeFieldOrder / tierChargeOrder / priorityChargeOrder），在其余位置用排序后的键，
//     以消除 Go map 的随机序。这些循环赋的是互不相同的字段，故顺序不影响取值。
//   - TS `Number(value)` 接受 "0x10" 这类十六进制串，本实现显式支持；空白串按 TS 的
//     `!value.trim()` 提前返回 null。

const cptMillion = 1_000_000

// 分层阈值的容差归一（cpt-convert.ts:26-29）。
const (
	tier200kMin = 200000
	tier200kMax = 200001
	tier272kMin = 272000
	tier272kMax = 272001
)

const (
	chargeUnitPerMTokens = "per_M_tokens"
	chargeUnitPerImage   = "per_image"
	chargeUnitPerRequest = "per_request"
	chargeUnitPerKCalls  = "per_k_calls"
)

// CloudVendorSummary 对应 TS 的 CloudVendorSummary。
//
// IconMono 用指针：TS 只在图标命中时才写这两个键（值是 false 也写），
// 用 bool + omitempty 会在 iconMono=false 时丢键，与 Node 落库的 jsonb 分叉。
type CloudVendorSummary struct {
	Vendor     string `json:"vendor"`
	Name       string `json:"name"`
	Icon       string `json:"icon,omitempty"`
	IconMono   *bool  `json:"iconMono,omitempty"`
	ModelCount int    `json:"modelCount"`
}

// ConvertedCptTable 对应 TS 的 ConvertedCptTable。
type ConvertedCptTable struct {
	// Models 以 canonical bare model_name 为键，别名已展开为同价的独立键。
	Models      map[string]map[string]any
	Vendors     []CloudVendorSummary
	Providers   map[string]CptProviderInfo
	Version     string
	Currency    string
	RefreshedAt string
}

// tokenChargeFields 对应 TOKEN_CHARGE_FIELDS。
var tokenChargeFields = map[string]string{
	"prompt":         "input_cost_per_token",
	"completion":     "output_cost_per_token",
	"cache_read":     "cache_read_input_token_cost",
	"cache_write":    "cache_creation_input_token_cost",
	"cache_write_1h": "cache_creation_input_token_cost_above_1hr",
}

type tierFields struct {
	above200k string
	above272k string
}

// tierFieldByCharge 对应 TIER_FIELD_BY_CHARGE。
var tierFieldByCharge = map[string]tierFields{
	"prompt": {
		above200k: "input_cost_per_token_above_200k_tokens",
		above272k: "input_cost_per_token_above_272k_tokens",
	},
	"completion": {
		above200k: "output_cost_per_token_above_200k_tokens",
		above272k: "output_cost_per_token_above_272k_tokens",
	},
	"cache_read": {
		above200k: "cache_read_input_token_cost_above_200k_tokens",
		above272k: "cache_read_input_token_cost_above_272k_tokens",
	},
	"cache_write": {
		above200k: "cache_creation_input_token_cost_above_200k_tokens",
		above272k: "cache_creation_input_token_cost_above_272k_tokens",
	},
	"cache_write_1h": {
		above200k: "cache_creation_input_token_cost_above_1hr_above_200k_tokens",
		above272k: "cache_creation_input_token_cost_above_1hr_above_272k_tokens",
	},
}

type priorityFields struct {
	base      string
	above200k string
	above272k string
}

// priorityFieldByCharge 对应 PRIORITY_FIELD_BY_CHARGE（注意只有三个维度）。
var priorityFieldByCharge = map[string]priorityFields{
	"prompt": {
		base:      "input_cost_per_token_priority",
		above200k: "input_cost_per_token_above_200k_tokens_priority",
		above272k: "input_cost_per_token_above_272k_tokens_priority",
	},
	"completion": {
		base:      "output_cost_per_token_priority",
		above200k: "output_cost_per_token_above_200k_tokens_priority",
		above272k: "output_cost_per_token_above_272k_tokens_priority",
	},
	"cache_read": {
		base:      "cache_read_input_token_cost_priority",
		above200k: "cache_read_input_token_cost_above_200k_tokens_priority",
		above272k: "cache_read_input_token_cost_above_272k_tokens_priority",
	},
}

// tierChargeOrder / priorityChargeOrder 复刻 TS 的 Object.entries 插入序。
var (
	tierChargeOrder     = []string{"prompt", "completion", "cache_read", "cache_write", "cache_write_1h"}
	priorityChargeOrder = []string{"prompt", "completion", "cache_read"}
)

// capabilityFieldMap 对应 CAPABILITY_FIELD_MAP。
var capabilityFieldMap = map[string][]string{
	"assistant_prefill": {"supports_assistant_prefill"},
	"computer_use":      {"supports_computer_use"},
	"function_calling":  {"supports_function_calling", "supports_tool_choice"},
	"pdf_input":         {"supports_pdf_input"},
	"prompt_caching":    {"supports_prompt_caching"},
	"reasoning":         {"supports_reasoning"},
	"structured_output": {"supports_response_schema"},
	"vision":            {"supports_vision"},
	"audio_input":       {"supports_audio_input"},
	"audio_output":      {"supports_audio_output"},
	"video_input":       {"supports_video_input"},
	"web_search":        {"supports_web_search"},
}

// parseDecimal 复刻 TS 的 parseDecimal：`Number(value)` + `Number.isFinite`。
//
// `Number` 的边界行为按需补齐：去空白、支持 "0x" 十六进制、拒绝 Infinity/NaN。
func parseDecimal(value string) (float64, bool) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, false
	}
	var parsed float64
	if len(trimmed) > 2 && (trimmed[0] == '0') && (trimmed[1] == 'x' || trimmed[1] == 'X') {
		hexValue, err := strconv.ParseUint(trimmed[2:], 16, 64)
		if err != nil {
			return 0, false
		}
		parsed = float64(hexValue)
	} else {
		value, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			return 0, false
		}
		parsed = value
	}
	if math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, false
	}
	return parsed, true
}

// roundPrecision 复刻 TS 的 roundPrecision：`Number(value.toPrecision(12))`。
//
// toPrecision(12) 保留 12 位有效数字后重新解析回 double，用于收敛浮点长尾。
// Go 用 FormatFloat 'g' + 12 位有效数字再解析，与 JS 得到同一 double。
func roundPrecision(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	if value == 0 {
		return 0
	}
	formatted := strconv.FormatFloat(value, 'g', 12, 64)
	parsed, err := strconv.ParseFloat(formatted, 64)
	if err != nil {
		return 0
	}
	if parsed == 0 || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0
	}
	return parsed
}

type chargeTargetKind int

const (
	chargeTargetPerToken chargeTargetKind = iota
	chargeTargetScalar
)

type chargeTarget struct {
	kind  chargeTargetKind
	field string
}

// chargeTargetOf 复刻 chargeTarget。
func chargeTargetOf(chargeKey string, charge CptCharge) (chargeTarget, bool) {
	switch charge.Unit {
	case chargeUnitPerMTokens:
		if field, ok := tokenChargeFields[chargeKey]; ok {
			return chargeTarget{kind: chargeTargetPerToken, field: field}, true
		}
		if chargeKey == "image_input" {
			return chargeTarget{kind: chargeTargetPerToken, field: "input_cost_per_image_token"}, true
		}
		if chargeKey == "image_output" {
			return chargeTarget{kind: chargeTargetPerToken, field: "output_cost_per_image_token"}, true
		}
		return chargeTarget{}, false
	case chargeUnitPerImage:
		if chargeKey == "image_input" {
			return chargeTarget{kind: chargeTargetScalar, field: "input_cost_per_image"}, true
		}
		if chargeKey == "image_output" {
			// 带尺寸后缀的变体（image_output_1024x1536 等）跳过。
			return chargeTarget{kind: chargeTargetScalar, field: "output_cost_per_image"}, true
		}
		return chargeTarget{}, false
	case chargeUnitPerRequest:
		if chargeKey == "request" {
			return chargeTarget{kind: chargeTargetScalar, field: "input_cost_per_request"}, true
		}
	}
	return chargeTarget{}, false
}

// trackFactorFor 复刻 trackFactorFor：charge_factors 覆盖默认 factor。
func trackFactorFor(track CptTrack, chargeKey string) (float64, bool) {
	if explicit, ok := track.ChargeFactors[chargeKey]; ok {
		return parseDecimal(explicit)
	}
	return parseDecimal(track.Factor)
}

type trackClassKind int

const (
	trackClassDefault trackClassKind = iota
	trackClassTier
	trackClassPriority
	trackClassPriorityTier
	trackClassUnsupported
)

type trackClass struct {
	kind trackClassKind
	tier string // "200k" / "272k"
}

func classifyThreshold(threshold float64) string {
	if threshold >= tier200kMin && threshold <= tier200kMax {
		return "200k"
	}
	if threshold >= tier272kMin && threshold <= tier272kMax {
		return "272k"
	}
	return ""
}

// isPriorityTrigger 复刻 /priority/.test(pattern)——子串匹配，大小写敏感。
func isPriorityTrigger(trigger CptTrackTrigger) bool {
	return trigger.Kind == "body_matches" &&
		trigger.Field == "service_tier" &&
		strings.Contains(trigger.Pattern, "priority")
}

// classifyTrack 复刻 classifyTrack。
func classifyTrack(track CptTrack) trackClass {
	if len(track.Triggers) == 0 {
		return trackClass{kind: trackClassDefault}
	}
	tier := ""
	priority := false
	for _, trigger := range track.Triggers {
		switch {
		case trigger.Kind == "input_tokens_above":
			// threshold 缺省时 Go 得到 0、TS 得到 undefined：两者都不落在 200K/272K 容差内，
			// 同样判为 unsupported，故无需区分。
			classified := classifyThreshold(trigger.Threshold)
			if classified == "" {
				return trackClass{kind: trackClassUnsupported}
			}
			tier = classified
		case isPriorityTrigger(trigger):
			priority = true
		case trigger.Kind == "header_matches":
			// 长上下文 beta header 与 token 阈值组合出现，按分层轨道归类即可。
		default:
			return trackClass{kind: trackClassUnsupported}
		}
	}
	switch {
	case tier != "" && priority:
		return trackClass{kind: trackClassPriorityTier, tier: tier}
	case tier != "":
		return trackClass{kind: trackClassTier, tier: tier}
	case priority:
		return trackClass{kind: trackClassPriority}
	default:
		return trackClass{kind: trackClassUnsupported}
	}
}

// convertCptVariant 复刻 convertCptVariant；返回 nil 表示该变体没有任何可识别计费字段。
func convertCptVariant(variant CptPricingVariant) map[string]any {
	charges := variant.Charges
	tracks := variant.Tracks

	var defaultTrack *CptTrack
	for index := range tracks {
		if len(tracks[index].Triggers) == 0 {
			defaultTrack = &tracks[index]
			break
		}
	}

	node := map[string]any{}
	hasBillableField := false

	basePriceOf := func(chargeKey string) (float64, bool) {
		charge, ok := charges[chargeKey]
		if !ok {
			return 0, false
		}
		// 非 USD 报价与内部计费不可比（与基础价循环同口径）。
		if charge.Currency != "" && charge.Currency != "USD" {
			return 0, false
		}
		price, ok := parseDecimal(charge.Price)
		if !ok || price < 0 {
			return 0, false
		}
		return price, true
	}

	// 基础价 = base price x 默认轨道 factor（无默认轨道时 factor=1）。
	for _, chargeKey := range sortedChargeKeys(charges) {
		charge := charges[chargeKey]
		if charge.Currency != "" && charge.Currency != "USD" {
			continue
		}
		target, ok := chargeTargetOf(chargeKey, charge)
		price, priceOK := parseDecimal(charge.Price)
		if !ok || !priceOK || price < 0 {
			continue
		}
		factor := 1.0
		if defaultTrack != nil {
			if parsed, ok := trackFactorFor(*defaultTrack, chargeKey); ok {
				factor = parsed
			}
		}
		effective := price * factor
		if target.kind == chargeTargetPerToken {
			node[target.field] = roundPrecision(effective / cptMillion)
		} else {
			node[target.field] = roundPrecision(effective)
		}
		hasBillableField = true
	}

	// web_search(per_k_calls) -> 每次查询成本，兼容旧字段形状。
	if webSearch, ok := charges["web_search"]; ok && webSearch.Unit == chargeUnitPerKCalls {
		if price, ok := parseDecimal(webSearch.Price); ok && price >= 0 {
			perQuery := roundPrecision(price / 1000)
			node["search_context_cost_per_query"] = map[string]any{
				"search_context_size_low":    perQuery,
				"search_context_size_medium": perQuery,
				"search_context_size_high":   perQuery,
			}
			hasBillableField = true
		}
	}

	// file_search_call ?? file_search（TS 的 nullish 合并）。
	fileSearch, hasFileSearch := charges["file_search_call"]
	if !hasFileSearch {
		fileSearch, hasFileSearch = charges["file_search"]
	}
	if hasFileSearch && fileSearch.Unit == chargeUnitPerKCalls {
		if price, ok := parseDecimal(fileSearch.Price); ok && price >= 0 {
			node["file_search_cost_per_1k_calls"] = roundPrecision(price)
		}
	}

	// 分层 / priority 轨道。
	for index := range tracks {
		classified := classifyTrack(tracks[index])
		track := tracks[index]
		switch classified.kind {
		case trackClassDefault, trackClassUnsupported:
			continue
		case trackClassTier, trackClassPriorityTier:
			isPriority := classified.kind == trackClassPriorityTier
			for _, chargeKey := range tierChargeOrder {
				basePrice, ok := basePriceOf(chargeKey)
				if !ok {
					continue
				}
				factor, ok := trackFactorFor(track, chargeKey)
				if !ok || factor < 0 {
					continue
				}
				var field string
				if isPriority {
					fields, known := priorityFieldByCharge[chargeKey]
					if !known {
						continue
					}
					if classified.tier == "200k" {
						field = fields.above200k
					} else {
						field = fields.above272k
					}
				} else {
					fields := tierFieldByCharge[chargeKey]
					if classified.tier == "200k" {
						field = fields.above200k
					} else {
						field = fields.above272k
					}
				}
				if field == "" {
					continue
				}
				node[field] = roundPrecision((basePrice * factor) / cptMillion)
				hasBillableField = true
			}
		case trackClassPriority:
			for _, chargeKey := range priorityChargeOrder {
				basePrice, ok := basePriceOf(chargeKey)
				if !ok {
					continue
				}
				factor, ok := trackFactorFor(track, chargeKey)
				if !ok || factor < 0 {
					continue
				}
				node[priorityFieldByCharge[chargeKey].base] = roundPrecision((basePrice * factor) / cptMillion)
				hasBillableField = true
			}
		}
	}

	if !hasBillableField {
		return nil
	}
	return node
}

// modeOfModelType 复刻 modeOfModelType。
func modeOfModelType(modelType string) string {
	switch modelType {
	case "":
		return "chat"
	case "chat":
		return "chat"
	case "completion":
		return "completion"
	case "responses":
		return "responses"
	case "image", "image_generation":
		return "image_generation"
	default:
		return modelType
	}
}

// variantPricingKey 复刻 variantPricingKey：provider + (@region)。
func variantPricingKey(variant CptPricingVariant) string {
	region := ""
	if variant.Region != nil {
		region = *variant.Region
	}
	if region != "" {
		return variant.Provider + "@" + region
	}
	return variant.Provider
}

const maxAliasesPerModel = 64

// convertCptModelEntry 复刻 convertCptModelEntry；返回 nil 表示所有变体都无可计费字段。
func convertCptModelEntry(entry CptModelEntry, providers map[string]CptProviderInfo) map[string]any {
	pricingMap := map[string]map[string]any{}
	pricingOrder := make([]string, 0, len(entry.Pricing))
	officialKeys := make([]string, 0, 2)

	defaultKey := ""
	var defaultNode map[string]any

	for _, variant := range entry.Pricing {
		if variant.Provider == "" {
			continue
		}
		converted := convertCptVariant(variant)
		if converted == nil {
			continue
		}

		key := variantPricingKey(variant)
		node := make(map[string]any, len(converted)+2)
		for field, value := range converted {
			node[field] = value
		}
		if variant.Official {
			node["official"] = true
		}
		if variant.ProviderModelID != "" {
			node["provider_model_id"] = variant.ProviderModelID
		}
		// TS 的 pricingMap[key] = node 是覆盖语义：同名键（provider+region）后到者胜，
		// 但 pricingKeys 的插入位置保持首次出现的顺序。
		if _, exists := pricingMap[key]; !exists {
			pricingOrder = append(pricingOrder, key)
		}
		pricingMap[key] = node

		if variant.Official {
			officialKeys = append(officialKeys, key)
			if defaultKey == "" {
				defaultKey = key
				defaultNode = converted
			}
		}
	}

	if len(pricingOrder) == 0 {
		return nil
	}

	if defaultKey == "" {
		defaultKey = pricingOrder[0]
		copied := make(map[string]any, len(pricingMap[defaultKey]))
		for field, value := range pricingMap[defaultKey] {
			copied[field] = value
		}
		delete(copied, "official")
		delete(copied, "provider_model_id")
		defaultNode = copied
	}

	priceData := make(map[string]any, len(defaultNode)+12)
	for field, value := range defaultNode {
		priceData[field] = value
	}
	priceData["mode"] = modeOfModelType(entry.ModelType)
	displayName := entry.DisplayName
	if displayName == "" {
		displayName = entry.ModelName
	}
	priceData["display_name"] = displayName
	priceData["vendor"] = entry.Vendor
	priceData["slug"] = entry.Slug
	priceData["providers"] = append([]string(nil), pricingOrder...)
	pricingJSON := make(map[string]any, len(pricingMap))
	for key, node := range pricingMap {
		pricingJSON[key] = node
	}
	priceData["pricing"] = pricingJSON
	if len(officialKeys) > 0 {
		priceData["official_pricing_provider"] = officialKeys[0]
	} else {
		priceData["official_pricing_provider"] = nil
	}
	delete(priceData, "official")
	delete(priceData, "provider_model_id")

	if icon, ok := resolveVendorIcon(entry.Vendor, providers); ok {
		priceData["vendor_icon"] = icon.File
		if icon.Mono {
			priceData["vendor_icon_mono"] = true
		}
	}

	if len(entry.Aliases) > 0 {
		aliases := make([]string, 0, len(entry.Aliases))
		for _, alias := range entry.Aliases {
			if !hasNonSpace(alias) || alias == entry.ModelName {
				continue
			}
			aliases = append(aliases, alias)
			if len(aliases) == maxAliasesPerModel {
				break
			}
		}
		if len(aliases) > 0 {
			priceData["aliases"] = aliases
		}
	}

	if entry.Family != "" {
		priceData["model_family"] = entry.Family
	}
	if entry.MaxInputTokens != nil {
		priceData["max_input_tokens"] = *entry.MaxInputTokens
	}
	if entry.MaxOutputTokens != nil {
		priceData["max_output_tokens"] = *entry.MaxOutputTokens
		priceData["max_tokens"] = *entry.MaxOutputTokens
	}
	if entry.Deprecated {
		priceData["deprecated"] = true
	}
	if entry.KnowledgeCutoff != "" {
		priceData["knowledge_cutoff"] = entry.KnowledgeCutoff
	}

	for capability, fields := range capabilityFieldMap {
		if !entry.Capabilities[capability] {
			continue
		}
		for _, field := range fields {
			priceData[field] = true
		}
	}

	return priceData
}

// preferEntry 复刻 preferEntry：官方报价 > 非 other vendor > 变体多者（并列取前者）。
func preferEntry(existing, candidate CptModelEntry) CptModelEntry {
	officialExisting := hasOfficialVariant(existing)
	officialCandidate := hasOfficialVariant(candidate)
	if officialExisting != officialCandidate {
		if officialExisting {
			return existing
		}
		return candidate
	}
	otherExisting := existing.Vendor == "other"
	otherCandidate := candidate.Vendor == "other"
	if otherExisting != otherCandidate {
		if otherExisting {
			return candidate
		}
		return existing
	}
	if len(candidate.Pricing) > len(existing.Pricing) {
		return candidate
	}
	return existing
}

func hasOfficialVariant(entry CptModelEntry) bool {
	for _, variant := range entry.Pricing {
		if variant.Official {
			return true
		}
	}
	return false
}

// ConvertCptTable 复刻 convertCptTable：以 canonical bare model_name 为键，并把 aliases
// 展开为同价的独立模型键。
func ConvertCptTable(table *CptTable) *ConvertedCptTable {
	entryByName := map[string]CptModelEntry{}
	canonicalOrder := make([]string, 0, len(table.Models))
	for _, entry := range table.Models {
		name := strings.TrimSpace(entry.ModelName)
		if name == "" {
			continue
		}
		existing, ok := entryByName[name]
		if !ok {
			entryByName[name] = entry
			canonicalOrder = append(canonicalOrder, name)
			continue
		}
		entryByName[name] = preferEntry(existing, entry)
	}

	models := map[string]map[string]any{}
	vendorCounts := map[string]int{}
	modelOrder := make([]string, 0, len(canonicalOrder))

	for _, name := range canonicalOrder {
		if isUnsafeKey(name) {
			continue
		}
		entry := entryByName[name]
		converted := convertCptModelEntry(entry, table.Providers)
		if converted == nil {
			continue
		}
		models[name] = converted
		modelOrder = append(modelOrder, name)
		vendorCounts[entry.Vendor]++
	}

	// 别名展开：canonical 名先全部落位，别名不覆盖已有键（canonical 优先，别名冲突先到先得）。
	// vendors 统计保持按 canonical 模型计数（在上一层已完成）。
	for _, name := range modelOrder {
		aliases, ok := models[name]["aliases"].([]string)
		if !ok || len(aliases) == 0 {
			continue
		}
		for _, alias := range aliases {
			if isUnsafeKey(alias) {
				continue
			}
			if _, exists := models[alias]; exists {
				continue
			}
			copied := make(map[string]any, len(models[name]))
			for field, value := range models[name] {
				copied[field] = value
			}
			models[alias] = copied
		}
	}

	vendors := make([]CloudVendorSummary, 0, len(vendorCounts))
	for vendor, modelCount := range vendorCounts {
		summary := CloudVendorSummary{Vendor: vendor, ModelCount: modelCount}
		if info, ok := table.Providers[vendor]; ok && info.Name != "" {
			summary.Name = info.Name
		} else {
			summary.Name = vendorDisplayName(vendor)
		}
		if icon, ok := resolveVendorIcon(vendor, table.Providers); ok {
			summary.Icon = icon.File
			mono := icon.Mono
			summary.IconMono = &mono
		}
		vendors = append(vendors, summary)
	}
	sort.Slice(vendors, func(i, j int) bool {
		if vendors[i].ModelCount != vendors[j].ModelCount {
			return vendors[i].ModelCount > vendors[j].ModelCount
		}
		return vendors[i].Vendor < vendors[j].Vendor
	})

	return &ConvertedCptTable{
		Models:      models,
		Vendors:     vendors,
		Providers:   table.Providers,
		Version:     table.Version,
		Currency:    table.Currency,
		RefreshedAt: table.RefreshedAt,
	}
}

// sortedChargeKeys 返回排序后的计费维度键，用于消除 Go map 的随机序。
func sortedChargeKeys(charges map[string]CptCharge) []string {
	keys := make([]string, 0, len(charges))
	for key := range charges {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
