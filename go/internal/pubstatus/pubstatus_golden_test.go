package pubstatus

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// 本文件用 **Node 真产物** 做字段级对拍：golden 由 bun 跑 Node 的
// src/lib/public-status/{config,config-snapshot,vendor-icon-key}.ts 生成
// （testdata/node_pubstatus_golden.json），不是手抄的期望值。
func loadGolden(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "node_pubstatus_golden.json"))
	if err != nil {
		t.Fatalf("读 golden 失败: %v", err)
	}
	var golden map[string]any
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("解析 golden 失败: %v", err)
	}
	return golden
}

// descriptionCases 列出 golden 用来产出的描述文本（顺序无关，按内容取）。
func descriptionInputs(t *testing.T, golden map[string]any) map[string]string {
	t.Helper()
	raw, ok := golden["descriptions"].(map[string]any)
	if !ok {
		t.Fatal("golden 缺 descriptions")
	}
	out := make(map[string]string, len(raw))
	for key, value := range raw {
		text, isString := value.(string)
		if !isString {
			t.Fatalf("descriptions.%s 不是字符串", key)
		}
		out[key] = text
	}
	return out
}

// parsedToJSON 把 Go 的解析结果摊成与 Node 同形的 JSON 值（键名、缺省省略规则都照 Node）。
func parsedToJSON(parsed ParsedPublicStatusDescription) any {
	out := map[string]any{"note": nil, "publicStatus": nil}
	if parsed.Note != nil {
		out["note"] = *parsed.Note
	}
	if parsed.PublicStatus == nil {
		return out
	}
	models := make([]any, 0, len(parsed.PublicStatus.PublicModels))
	for _, model := range parsed.PublicStatus.PublicModels {
		entry := map[string]any{"modelKey": model.ModelKey}
		if model.ProviderTypeOverride != "" {
			entry["providerTypeOverride"] = model.ProviderTypeOverride
		}
		models = append(models, entry)
	}
	group := map[string]any{
		"explanatoryCopy": nil,
		"publicModels":    models,
	}
	// Node 的 sanitizeString 返回 undefined，JSON.stringify 会**省略**该键；空串在这里等价于省略。
	if parsed.PublicStatus.DisplayName != "" {
		group["displayName"] = parsed.PublicStatus.DisplayName
	}
	if parsed.PublicStatus.PublicGroupSlug != "" {
		group["publicGroupSlug"] = parsed.PublicStatus.PublicGroupSlug
	}
	if parsed.PublicStatus.ExplanatoryCopy != nil {
		group["explanatoryCopy"] = *parsed.PublicStatus.ExplanatoryCopy
	}
	if parsed.PublicStatus.SortOrder != nil {
		group["sortOrder"] = *parsed.PublicStatus.SortOrder
	}
	out["publicStatus"] = group
	return out
}

func TestParsePublicStatusDescriptionMatchesNodeGolden(t *testing.T) {
	golden := loadGolden(t)
	inputs := descriptionInputs(t, golden)
	expected, ok := golden["parsed"].(map[string]any)
	if !ok {
		t.Fatal("golden 缺 parsed")
	}

	for key, text := range inputs {
		raw := text
		got := parsedToJSON(ParsePublicStatusDescription(&raw))
		want := expected[key]
		if !reflect.DeepEqual(normalizeJSON(got), normalizeJSON(want)) {
			t.Errorf("描述 %s 解析不一致\n got: %#v\nwant: %#v", key, got, want)
		}
	}
}

func TestParsePublicStatusDescriptionEmpty(t *testing.T) {
	if parsed := ParsePublicStatusDescription(nil); parsed.Note != nil || parsed.PublicStatus != nil {
		t.Fatalf("nil 描述应解析为空: %#v", parsed)
	}
	empty := ""
	if parsed := ParsePublicStatusDescription(&empty); parsed.Note != nil || parsed.PublicStatus != nil {
		t.Fatalf("空串描述应解析为空: %#v", parsed)
	}
}

func TestSlugifyPublicGroupMatchesNodeGolden(t *testing.T) {
	golden := loadGolden(t)
	cases, ok := golden["slugify"].([]any)
	if !ok {
		t.Fatal("golden 缺 slugify")
	}
	for _, entry := range cases {
		pair := entry.([]any)
		input := pair[0].(string)
		want := pair[1].(string)
		if got := SlugifyPublicGroup(input); got != want {
			t.Errorf("SlugifyPublicGroup(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestCollectEnabledPublicStatusGroupsMatchesNodeGolden(t *testing.T) {
	golden := loadGolden(t)
	inputs := descriptionInputs(t, golden)
	expected, ok := golden["enabledGroups"].([]any)
	if !ok {
		t.Fatal("golden 缺 enabledGroups")
	}

	// golden 的入参顺序：团队一、团队二、flat、empty（见生成器）。
	names := []string{"团队一", "团队二", "flat", "empty"}
	sourceKeys := []string{"full", "full", "legacyModels", "noModels"}
	groups := make([]PublicStatusConfiguredGroupInput, 0, len(names))
	for index, name := range names {
		text := inputs[sourceKeys[index]]
		groups = append(groups, PublicStatusConfiguredGroupInput{
			GroupName:                     name,
			ParsedPublicStatusDescription: ParsePublicStatusDescription(&text),
		})
	}

	got := CollectEnabledPublicStatusGroups(groups)
	if len(got) != len(expected) {
		t.Fatalf("启用分组数不一致: got %d, want %d", len(got), len(expected))
	}
	for index, entry := range expected {
		want := entry.(map[string]any)
		actual := got[index]
		if actual.GroupName != want["groupName"] {
			t.Errorf("[%d] groupName = %q, want %q", index, actual.GroupName, want["groupName"])
		}
		if actual.DisplayName != want["displayName"] {
			t.Errorf("[%d] displayName = %q, want %q", index, actual.DisplayName, want["displayName"])
		}
		if actual.PublicGroupSlug != want["publicGroupSlug"] {
			t.Errorf("[%d] slug = %q, want %q", index, actual.PublicGroupSlug, want["publicGroupSlug"])
		}
		if want["explanatoryCopy"] == nil {
			if actual.ExplanatoryCopy != nil {
				t.Errorf("[%d] explanatoryCopy 应为 nil", index)
			}
		} else if actual.ExplanatoryCopy == nil || *actual.ExplanatoryCopy != want["explanatoryCopy"] {
			t.Errorf("[%d] explanatoryCopy 不一致", index)
		}
		if actual.SortOrder != want["sortOrder"] {
			t.Errorf("[%d] sortOrder = %v, want %v", index, actual.SortOrder, want["sortOrder"])
		}
		wantModels := want["publicModels"].([]any)
		if len(actual.PublicModels) != len(wantModels) {
			t.Fatalf("[%d] 模型数 = %d, want %d", index, len(actual.PublicModels), len(wantModels))
		}
		for modelIndex, modelEntry := range wantModels {
			wantModel := modelEntry.(map[string]any)
			if actual.PublicModels[modelIndex].ModelKey != wantModel["modelKey"] {
				t.Errorf("[%d][%d] modelKey = %q, want %q", index, modelIndex,
					actual.PublicModels[modelIndex].ModelKey, wantModel["modelKey"])
			}
			wantOverride, hasOverride := wantModel["providerTypeOverride"]
			if hasOverride != (actual.PublicModels[modelIndex].ProviderTypeOverride != "") {
				t.Errorf("[%d][%d] providerTypeOverride 存在性不一致", index, modelIndex)
			}
			if hasOverride && actual.PublicModels[modelIndex].ProviderTypeOverride != wantOverride {
				t.Errorf("[%d][%d] providerTypeOverride = %q, want %v", index, modelIndex,
					actual.PublicModels[modelIndex].ProviderTypeOverride, wantOverride)
			}
		}
	}
}

func TestResolveRequestTypeBadgeMatchesNodeGolden(t *testing.T) {
	golden := loadGolden(t)
	cases, ok := golden["badgeCases"].([]any)
	if !ok {
		t.Fatal("golden 缺 badgeCases")
	}
	for _, entry := range cases {
		triple := entry.([]any)
		modelName := triple[0].(string)
		override := ""
		if triple[1] != nil {
			override = triple[1].(string)
		}
		want := triple[2].(string)
		if got := ResolveRequestTypeBadge(modelName, override); got != want {
			t.Errorf("ResolveRequestTypeBadge(%q, %q) = %q, want %q", modelName, override, got, want)
		}
	}
}

func TestResolveVendorIconKeyMatchesNodeGolden(t *testing.T) {
	golden := loadGolden(t)
	cases, ok := golden["iconCases"].([]any)
	if !ok {
		t.Fatal("golden 缺 iconCases")
	}
	for _, entry := range cases {
		quad := entry.([]any)
		modelName := quad[0].(string)
		vendorIconKey := ""
		if quad[1] != nil {
			vendorIconKey = quad[1].(string)
		}
		override := ""
		if quad[2] != nil {
			override = quad[2].(string)
		}
		want := quad[3].(string)
		if got := resolvePublicStatusVendorIconKey(modelName, vendorIconKey, override); got != want {
			t.Errorf("vendorIconKey(%q, %q, %q) = %q, want %q", modelName, vendorIconKey, override, got, want)
		}
	}
}

func TestResolveSiteDescriptionMatchesNodeGolden(t *testing.T) {
	golden := loadGolden(t)
	cases, ok := golden["siteDescription"].([]any)
	if !ok {
		t.Fatal("golden 缺 siteDescription")
	}
	for _, entry := range cases {
		triple := entry.([]any)
		siteTitle := triple[0].(string)
		var description *string
		if triple[1] != nil {
			value := triple[1].(string)
			description = &value
		}
		want := triple[2].(string)
		if got := ResolveSiteDescription(siteTitle, description); got != want {
			t.Errorf("ResolveSiteDescription(%q, %v) = %q, want %q", siteTitle, description, got, want)
		}
	}
}

// TestBuildSnapshotMatchesNodeGoldenShape 对拍整份快照（字段名、排序与逐模型取值）。
//
// 入参按 golden 生成器的口径构造：三组启用分组（flat / 团队一 / 团队二），模型标签取 modelKey、
// 厂商图标与请求类型徽标走本包已单独对拍过的两个解析函数。golden 的 generatedAt 是生成时刻，
// 比对时剔除。
func TestBuildSnapshotMatchesNodeGoldenShape(t *testing.T) {
	golden := loadGolden(t)
	inputs := descriptionInputs(t, golden)

	names := []string{"团队一", "团队二", "flat", "empty"}
	sourceKeys := []string{"full", "full", "legacyModels", "noModels"}
	configured := make([]PublicStatusConfiguredGroupInput, 0, len(names))
	for index, name := range names {
		text := inputs[sourceKeys[index]]
		configured = append(configured, PublicStatusConfiguredGroupInput{
			GroupName:                     name,
			ParsedPublicStatusDescription: ParsePublicStatusDescription(&text),
		})
	}
	enabled := CollectEnabledPublicStatusGroups(configured)

	timeZone := "Asia/Shanghai"
	input := ConfigSnapshotInput{
		ConfigVersion:          "cfg-1700000000000",
		SiteTitle:              "  CC Hub  ",
		TimeZone:               &timeZone,
		DefaultIntervalMinutes: 5,
		DefaultRangeHours:      24,
		Groups:                 make([]ConfigSnapshotGroup, 0, len(enabled)),
	}
	for index, group := range enabled {
		groupID := int64(index + 1)
		models := make([]PublicStatusModelSnapshot, 0, len(group.PublicModels))
		for _, model := range group.PublicModels {
			models = append(models, PublicStatusModelSnapshot{
				PublicModelKey: model.ModelKey,
				Label:          model.ModelKey,
				VendorIconKey: resolvePublicStatusVendorIconKey(
					model.ModelKey, "", model.ProviderTypeOverride),
				RequestTypeBadge: ResolveRequestTypeBadge(model.ModelKey, model.ProviderTypeOverride),
			})
		}
		input.Groups = append(input.Groups, ConfigSnapshotGroup{
			SourceGroupID:   &groupID,
			SourceGroupName: group.GroupName,
			Slug:            group.PublicGroupSlug,
			DisplayName:     group.DisplayName,
			SortOrder:       group.SortOrder,
			Description:     group.ExplanatoryCopy,
			Models:          models,
		})
	}

	public := BuildConfigSnapshot(input, time.UnixMilli(1700000000000))
	internal := BuildInternalConfigSnapshot(input, time.UnixMilli(1700000000000))

	compareSnapshotJSON(t, "publicSnapshot", public, golden["publicSnapshot"])
	compareSnapshotJSON(t, "internalSnapshot", internal, golden["internalSnapshot"])
}

func compareSnapshotJSON(t *testing.T, label string, got any, want any) {
	t.Helper()
	gotJSON := normalizeJSON(dropGeneratedAt(toJSONValue(t, got)))
	wantJSON := normalizeJSON(dropGeneratedAt(want))
	if !reflect.DeepEqual(gotJSON, wantJSON) {
		gotRaw, _ := json.MarshalIndent(gotJSON, "", " ")
		wantRaw, _ := json.MarshalIndent(wantJSON, "", " ")
		t.Errorf("%s 形状不一致\n got: %s\nwant: %s", label, gotRaw, wantRaw)
	}
}

// dropGeneratedAt 剔除 golden 里不可复现的生成时刻。
func dropGeneratedAt(value any) any {
	object, ok := value.(map[string]any)
	if !ok {
		return value
	}
	out := make(map[string]any, len(object))
	for key, entry := range object {
		if key == "generatedAt" {
			continue
		}
		out[key] = entry
	}
	return out
}

func toJSONValue(t *testing.T, value any) any {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	return out
}

// normalizeJSON 递归把数值统一成 float64，便于与 golden 的 json.Number 兼容。
func normalizeJSON(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, entry := range typed {
			out[key] = normalizeJSON(entry)
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, entry := range typed {
			out = append(out, normalizeJSON(entry))
		}
		return out
	case json.Number:
		if parsed, err := typed.Float64(); err == nil {
			return parsed
		}
		return typed.String()
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	case float64:
		return typed
	default:
		return value
	}
}
