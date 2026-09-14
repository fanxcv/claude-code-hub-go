package jobs

import (
	"encoding/json"
	"fmt"
)

// 本文件是 CPT v1 价格表的类型与结构校验，逐条复刻 src/lib/price-sync/cpt-schema.ts。
//
// 与 TS 的差异只在两处，都是「更严」或等价方向：
//   - TS 用 isRecord + 逐字段 typeof 校验后 `entry as CptModelEntry` 断言（多余字段透传）。
//     Go 用结构体解码，多余字段丢弃——转换器只读结构体里列出的字段，故结果一致。
//   - TS 的 `Object.create(null)` 防原型污染在 Go 里无对应物；键过滤保持一致（见 providers 处理）。

// CptCharge 是一次报价的单价与计量单位。
type CptCharge struct {
	Price    string `json:"price"`
	Unit     string `json:"unit"`
	Currency string `json:"currency,omitempty"`
}

// CptTrackTrigger 是分层/服务档轨道的触发条件。
type CptTrackTrigger struct {
	Kind      string  `json:"kind"`
	Field     string  `json:"field,omitempty"`
	Header    string  `json:"header,omitempty"`
	Pattern   string  `json:"pattern,omitempty"`
	Threshold float64 `json:"threshold,omitempty"`
	Inclusive bool    `json:"inclusive,omitempty"`
}

// CptTrack 是一条价格轨道（分层或 priority 服务档）。
type CptTrack struct {
	Label         string            `json:"label"`
	Factor        string            `json:"factor"`
	ChargeFactors map[string]string `json:"charge_factors,omitempty"`
	Triggers      []CptTrackTrigger `json:"triggers"`
}

// CptPricingVariant 是某 provider 对某模型的一份报价。
type CptPricingVariant struct {
	Provider        string               `json:"provider"`
	Official        bool                 `json:"official"`
	Source          string               `json:"source"`
	ProviderModelID string               `json:"provider_model_id,omitempty"`
	Region          *string              `json:"region,omitempty"`
	Charges         map[string]CptCharge `json:"charges"`
	Tracks          []CptTrack           `json:"tracks,omitempty"`
}

// CptCapabilities 是模型能力旗标；只取转换器关心的那些。
type CptCapabilities map[string]bool

// CptModelEntry 是价格表里的一个模型条目。
type CptModelEntry struct {
	Slug            string              `json:"slug"`
	ModelName       string              `json:"model_name"`
	Vendor          string              `json:"vendor"`
	DisplayName     string              `json:"display_name"`
	Aliases         []string            `json:"aliases,omitempty"`
	Family          string              `json:"family,omitempty"`
	ModelType       string              `json:"model_type,omitempty"`
	KnowledgeCutoff string              `json:"knowledge_cutoff,omitempty"`
	Deprecated      bool                `json:"deprecated,omitempty"`
	MaxInputTokens  *float64            `json:"max_input_tokens,omitempty"`
	MaxOutputTokens *float64            `json:"max_output_tokens,omitempty"`
	Capabilities    CptCapabilities     `json:"capabilities,omitempty"`
	Pricing         []CptPricingVariant `json:"pricing"`
}

// CptProviderInfo 是 providers 字典的一项。
type CptProviderInfo struct {
	Name     string `json:"name"`
	Doc      string `json:"doc,omitempty"`
	Icon     string `json:"icon,omitempty"`
	IconMono bool   `json:"icon_mono,omitempty"`
}

// CptTable 是一张已解析的 CPT v1 价格表。
type CptTable struct {
	Schema      string                     `json:"schema"`
	Version     string                     `json:"version"`
	Currency    string                     `json:"currency"`
	RefreshedAt string                     `json:"refreshed_at"`
	Models      []CptModelEntry            `json:"models"`
	Providers   map[string]CptProviderInfo `json:"providers"`
}

// cptRawEntry 是解析期的宽松形状：TS 只校验「必填字段是非空字符串 / pricing 是数组」，
// 缺字段的条目直接丢弃而不是报错，因此不能直接用 CptModelEntry 解码（缺字段会得到零值，
// 无法区分「缺失」与「空串」）。所有字段用 *string / json.RawMessage 保留「是否出现」。
type cptRawEntry struct {
	Slug        *string         `json:"slug"`
	ModelName   *string         `json:"model_name"`
	Vendor      *string         `json:"vendor"`
	DisplayName string          `json:"display_name"`
	Aliases     []string        `json:"aliases"`
	Family      string          `json:"family"`
	ModelType   *string         `json:"model_type"`
	Cutoff      string          `json:"knowledge_cutoff"`
	Deprecated  bool            `json:"deprecated"`
	MaxInput    *float64        `json:"max_input_tokens"`
	MaxOutput   *float64        `json:"max_output_tokens"`
	Capability  CptCapabilities `json:"capabilities"`
	Pricing     json.RawMessage `json:"pricing"`
}

// ParseCptTable 解析并校验 CPT v1 价格表 JSON 文本（cpt-schema.ts:107 parseCptTable）。
//
// 结构级校验与 TS 一致：schema 必须匹配、models 必须是数组、providers 必须是对象；
// 条目级只做「必填字段非空」过滤，字段内容的健壮性由转换器兜底。
func ParseCptTable(jsonText []byte) (*CptTable, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(jsonText, &root); err != nil {
		return nil, fmt.Errorf("价格表 JSON 解析失败: %w", err)
	}

	var schema string
	if raw, ok := root["schema"]; ok {
		_ = json.Unmarshal(raw, &schema)
	}
	if schema != cptSchemaID {
		return nil, fmt.Errorf("价格表格式无效：schema 不是 %s（实际为 %s）", cptSchemaID, schema)
	}

	rawModels, ok := root["models"]
	if !ok || len(rawModels) == 0 || rawModels[0] != '[' {
		return nil, fmt.Errorf("价格表格式无效：缺少 models 数组")
	}
	var entries []cptRawEntry
	if err := json.Unmarshal(rawModels, &entries); err != nil {
		return nil, fmt.Errorf("价格表格式无效：models 不是对象数组: %w", err)
	}

	rawProviders, ok := root["providers"]
	if !ok || len(rawProviders) == 0 || rawProviders[0] != '{' {
		return nil, fmt.Errorf("价格表格式无效：缺少 providers 字典")
	}
	var providerMap map[string]CptProviderInfo
	if err := json.Unmarshal(rawProviders, &providerMap); err != nil {
		return nil, fmt.Errorf("价格表格式无效：providers 不是对象: %w", err)
	}

	models := make([]CptModelEntry, 0, len(entries))
	for _, entry := range entries {
		// TS：model_name/slug/vendor 必须是非空字符串、pricing 必须是数组，否则整条跳过。
		if entry.ModelName == nil || !hasNonSpace(*entry.ModelName) {
			continue
		}
		if entry.Slug == nil || !hasNonSpace(*entry.Slug) {
			continue
		}
		if entry.Vendor == nil || !hasNonSpace(*entry.Vendor) {
			continue
		}
		if len(entry.Pricing) == 0 || entry.Pricing[0] != '[' {
			continue
		}
		var pricing []CptPricingVariant
		if err := json.Unmarshal(entry.Pricing, &pricing); err != nil {
			continue
		}
		modelType := ""
		if entry.ModelType != nil {
			modelType = *entry.ModelType
		}
		models = append(models, CptModelEntry{
			Slug:            *entry.Slug,
			ModelName:       *entry.ModelName,
			Vendor:          *entry.Vendor,
			DisplayName:     entry.DisplayName,
			Aliases:         entry.Aliases,
			Family:          entry.Family,
			ModelType:       modelType,
			KnowledgeCutoff: entry.Cutoff,
			Deprecated:      entry.Deprecated,
			MaxInputTokens:  entry.MaxInput,
			MaxOutputTokens: entry.MaxOutput,
			Capabilities:    entry.Capability,
			Pricing:         pricing,
		})
	}

	if len(models) == 0 {
		return nil, fmt.Errorf("价格表格式无效：models 为空")
	}

	// providers 的键过滤与 TS 同口径：原型污染键丢弃、name 必须是非空字符串。
	providers := make(map[string]CptProviderInfo, len(providerMap))
	for slug, info := range providerMap {
		if isUnsafeKey(slug) || info.Name == "" {
			continue
		}
		providers[slug] = info
	}

	version, currency, refreshedAt := "", "USD", ""
	if raw, ok := root["version"]; ok {
		_ = json.Unmarshal(raw, &version)
	}
	if raw, ok := root["currency"]; ok {
		_ = json.Unmarshal(raw, &currency)
		if currency == "" {
			currency = "USD"
		}
	}
	if raw, ok := root["refreshed_at"]; ok {
		_ = json.Unmarshal(raw, &refreshedAt)
	}

	return &CptTable{
		Schema:      cptSchemaID,
		Version:     version,
		Currency:    currency,
		RefreshedAt: refreshedAt,
		Models:      models,
		Providers:   providers,
	}, nil
}

// hasNonSpace 对应 TS 的 `value.trim()` 真值判断（含 Unicode 空白）。
func hasNonSpace(value string) bool {
	for _, r := range value {
		if !isSpaceRune(r) {
			return true
		}
	}
	return false
}

func isSpaceRune(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\v', '\f', '\r', 0x85, 0xA0:
		return true
	}
	// Unicode 空格类（JS 的 String#trim 覆盖 Zs 与部分 Cf）。
	if r < 0x2000 {
		return false
	}
	switch {
	case r == 0x1680, r >= 0x2000 && r <= 0x200A, r == 0x2028, r == 0x2029,
		r == 0x202F, r == 0x205F, r == 0x3000, r == 0xFEFF:
		return true
	}
	return false
}

// isUnsafeKey 对应 TS 里那组原型污染键过滤（__proto__ / constructor / prototype）。
func isUnsafeKey(key string) bool {
	return key == "__proto__" || key == "constructor" || key == "prototype"
}
