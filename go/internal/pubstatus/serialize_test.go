package pubstatus

import (
	"encoding/json"
	"testing"
)

// 本文件钉住描述串**写侧**的契约（Node config.ts:298-349 的 serializePublicStatusDescription）。
// 最关键的一条是**键序**：描述串要落库并被读侧与前端逐字比对，键序不同 = 同一份配置两个哈希。
// 故这里用逐字节的字面量断言，而不是反序列化后比对字段。

func TestSerializeMatchesNodeKeyOrderAndOmission(t *testing.T) {
	// 1. 全空 → null（SQL NULL）。
	if got := SerializePublicStatusDescription(ParsedPublicStatusDescription{}); got != nil {
		t.Fatalf("全空应返回 nil（Node 的 return null），实得 %v", *got)
	}
	// 空白字符串同样算「没给」（sanitizeString 会 trim）。
	blank := "   "
	if got := SerializePublicStatusDescription(ParsedPublicStatusDescription{Note: &blank}); got != nil {
		t.Fatalf("纯空白 note 应返回 nil，实得 %v", *got)
	}

	// 2. 只有 note：键序 version → note。
	note := "  维护中  "
	got := SerializePublicStatusDescription(ParsedPublicStatusDescription{Note: &note})
	if got == nil {
		t.Fatal("有 note 应产出描述串")
	}
	if want := `{"version":2,"note":"维护中"}`; *got != want {
		t.Fatalf("序列化结果与 Node 不同\nwant=%s\n got=%s", want, *got)
	}

	// 3. 完整 publicStatus：键序 version → note → publicStatus{displayName, publicGroupSlug,
	//    explanatoryCopy, sortOrder, publicModels}，且 publicModels 恒在。
	copyText := "说明文案"
	sortOrder := 3.0
	full := SerializePublicStatusDescription(ParsedPublicStatusDescription{
		Note: &note,
		PublicStatus: &PublicStatusGroupConfig{
			DisplayName:     "主力",
			PublicGroupSlug: "main",
			ExplanatoryCopy: &copyText,
			SortOrder:       &sortOrder,
			PublicModels: []PublicStatusModelConfig{
				{ModelKey: "claude-sonnet-4-5"},
				{ModelKey: "gpt-5", ProviderTypeOverride: "openai-compatible"},
			},
		},
	})
	if full == nil {
		t.Fatal("完整配置应产出描述串")
	}
	want := `{"version":2,"note":"维护中","publicStatus":{"displayName":"主力","publicGroupSlug":"main",` +
		`"explanatoryCopy":"说明文案","sortOrder":3,"publicModels":[{"modelKey":"claude-sonnet-4-5"},` +
		`{"modelKey":"gpt-5","providerTypeOverride":"openai-compatible"}]}}`
	if *full != want {
		t.Fatalf("序列化结果与 Node 不同\nwant=%s\n got=%s", want, *full)
	}

	// 4. 只有模型（无 displayName 等）：publicStatus 段仍要出现，且只带 publicModels。
	modelsOnly := SerializePublicStatusDescription(ParsedPublicStatusDescription{
		PublicStatus: &PublicStatusGroupConfig{
			PublicModels: []PublicStatusModelConfig{{ModelKey: "m1"}},
		},
	})
	if modelsOnly == nil {
		t.Fatal("只有模型也应产出描述串")
	}
	if want := `{"version":2,"publicStatus":{"publicModels":[{"modelKey":"m1"}]}}`; *modelsOnly != want {
		t.Fatalf("只有模型时 publicStatus 段仍须出现\nwant=%s\n got=%s", want, *modelsOnly)
	}

	// 5. publicStatus 存在但字段全空 → **仍然返回 null**。
	//
	// 这条容易写反：直觉是「对象在就该写 version 层」，但 Node 的判据逐字段看值——
	// `!displayName && !publicGroupSlug && !explanatoryCopy && sortOrder === undefined &&
	// publicModels.length === 0` 全成立即 `return null`，与 publicStatus 是否为 null 无关。
	emptyStatus := SerializePublicStatusDescription(ParsedPublicStatusDescription{
		PublicStatus: &PublicStatusGroupConfig{},
	})
	if emptyStatus != nil {
		t.Fatalf("publicStatus 字段全空时应返回 nil（Node 逐字段判值，不看对象是否存在），实得 %v", *emptyStatus)
	}
}

func TestSerializeRoundTripsThroughParser(t *testing.T) {
	// 写侧产出的串必须能被读侧原样读回——两半分家正是本仓吃过亏的地方。
	note := "备注"
	copyText := "copy"
	sortOrder := 1.0
	encoded := SerializePublicStatusDescription(ParsedPublicStatusDescription{
		Note: &note,
		PublicStatus: &PublicStatusGroupConfig{
			DisplayName:     "组",
			PublicGroupSlug: "group",
			ExplanatoryCopy: &copyText,
			SortOrder:       &sortOrder,
			PublicModels:    []PublicStatusModelConfig{{ModelKey: "m"}},
		},
	})
	if encoded == nil {
		t.Fatal("应产出描述串")
	}
	parsed := ParsePublicStatusDescription(encoded)
	if parsed.Note == nil || *parsed.Note != note {
		t.Fatalf("note 读回不符: %v", parsed.Note)
	}
	if parsed.PublicStatus == nil {
		t.Fatal("publicStatus 读回为空")
	}
	if parsed.PublicStatus.DisplayName != "组" || parsed.PublicStatus.PublicGroupSlug != "group" {
		t.Fatalf("publicStatus 读回不符: %+v", parsed.PublicStatus)
	}
	if len(parsed.PublicStatus.PublicModels) != 1 || parsed.PublicStatus.PublicModels[0].ModelKey != "m" {
		t.Fatalf("publicModels 读回不符: %+v", parsed.PublicStatus.PublicModels)
	}
}

func TestSerializeOutputIsValidJSON(t *testing.T) {
	note := "n"
	encoded := SerializePublicStatusDescription(ParsedPublicStatusDescription{
		Note:         &note,
		PublicStatus: &PublicStatusGroupConfig{PublicModels: []PublicStatusModelConfig{}},
	})
	if encoded == nil {
		t.Fatal("应产出描述串")
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(*encoded), &decoded); err != nil {
		t.Fatalf("产出不是合法 JSON: %v（%s）", err, *encoded)
	}
	if decoded["version"].(float64) != float64(publicStatusDescriptionVersion) {
		t.Fatalf("version 应为 %d，实得 %v", publicStatusDescriptionVersion, decoded["version"])
	}
}

func TestExceedsDescriptionLimitUsesUtf8Bytes(t *testing.T) {
	// 上限按 **UTF-8 字节数**：中文每字 3 字节，故 16 KiB 上限约 5461 个汉字。
	small := "小"
	if ExceedsProviderGroupDescriptionLimit(&small) {
		t.Fatal("短串不该超限")
	}
	// 恰好等于上限：不超（Node 的判据是 `>` 而不是 `>=`）。
	exact := make([]byte, ProviderGroupDescriptionMaxBytes)
	for index := range exact {
		exact[index] = 'a'
	}
	atLimit := string(exact)
	if ExceedsProviderGroupDescriptionLimit(&atLimit) {
		t.Fatal("恰好等于上限不该算超限（Node 用 > 比较）")
	}
	over := atLimit + "a"
	if !ExceedsProviderGroupDescriptionLimit(&over) {
		t.Fatal("超出上限应判超限")
	}
	if ExceedsProviderGroupDescriptionLimit(nil) {
		t.Fatal("nil 不算超限")
	}
	empty := ""
	if ExceedsProviderGroupDescriptionLimit(&empty) {
		t.Fatal("空串不算超限")
	}
	// 字节数而非字符数：19000 个汉字远超 16 KiB，但字符数不到 16K 的「看起来没超」是错觉。
	wide := ""
	for len(wide) <= ProviderGroupDescriptionMaxBytes {
		wide += "汉"
	}
	if !ExceedsProviderGroupDescriptionLimit(&wide) {
		t.Fatalf("按字节判：%d 字节（%d 字符）应超限", len(wide), len([]rune(wide)))
	}
}
