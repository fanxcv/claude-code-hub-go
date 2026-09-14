package pubstatus

import (
	"encoding/json"
	"strings"
)

// 本文件是描述串的**写侧**（Node：src/lib/public-status/config.ts:298-349 的
// serializePublicStatusDescription，与 description-limit.ts 的上限判定）。
//
// 与写侧放同一个包的理由：读侧（ParsePublicStatusDescription）与写侧是同一份 wire format 的
// 两半，分开放会让「读侧认得、写侧写不出」或反之成为一次参数省略，而不是一次编译错误。
// 本仓已经因为「第二个转换器」吃过亏，这里不重复。

// ProviderGroupDescriptionMaxBytes 是分组描述串的上限（description-limit.ts:1）。
const ProviderGroupDescriptionMaxBytes = 16 * 1024

// ExceedsProviderGroupDescriptionLimit 复刻 exceedsProviderGroupDescriptionLimit：
// 按 **UTF-8 字节数**判（不是字符数），空值不算超限。
func ExceedsProviderGroupDescriptionLimit(value *string) bool {
	if value == nil || *value == "" {
		return false
	}
	return len(*value) > ProviderGroupDescriptionMaxBytes
}

// serializedDescription 的字段顺序**必须**与 Node 的对象字面量一致：
// `{version, note?, publicStatus?}`。用 map 序列化会按字母序输出（note/publicStatus/version），
// 而描述串是要落库并被读侧与前端逐字比对的，键序不同就是「同一份配置两个哈希」。
type serializedDescription struct {
	Version      int                     `json:"version"`
	Note         string                  `json:"note,omitempty"`
	PublicStatus *serializedPublicStatus `json:"publicStatus,omitempty"`
}

// serializedPublicStatus 的键序同理对齐 Node：
// `{displayName?, publicGroupSlug?, explanatoryCopy?, sortOrder?, publicModels}`。
// 注意 `publicModels` **无 omitempty**：Node 在 publicStatus 段里恒带这个键（空数组也带）。
type serializedPublicStatus struct {
	DisplayName     string   `json:"displayName,omitempty"`
	PublicGroupSlug string   `json:"publicGroupSlug,omitempty"`
	ExplanatoryCopy string   `json:"explanatoryCopy,omitempty"`
	SortOrder       *float64 `json:"sortOrder,omitempty"`
	PublicModels    []any    `json:"publicModels"`
}

// SerializePublicStatusDescription 复刻 serializePublicStatusDescription。
//
// 三条语义逐条对齐：
//
//  1. 每个字符串字段都过 `sanitizeString`（trim 后为空即视为「没给」）——Node 用的是同一个
//     函数，故 `"  "` 与 `""` 在这里都落到「省略该键」；
//  2. 全空（无 note、无任一 publicStatus 字段、模型为空）→ **返回 nil**（Node 的 `return null`，
//     落库即 SQL NULL）；
//  3. 模型列表走 `sanitizeLegacyPublicModels`（兼容 `publicModelKeys` 旧写法）——
//     本函数签名只收 publicModels，旧键的兼容在调用方解析时已归一。
func SerializePublicStatusDescription(input ParsedPublicStatusDescription) *string {
	note, hasNote := sanitizeStringForSerialize(input.Note)
	if !hasNote {
		note = ""
	}

	var displayName, publicGroupSlug, explanatoryCopy string
	var sortOrder *float64
	var models []any

	if input.PublicStatus != nil {
		if value, ok := sanitizeString(input.PublicStatus.DisplayName); ok {
			displayName = value
		}
		if value, ok := sanitizeString(input.PublicStatus.PublicGroupSlug); ok {
			publicGroupSlug = value
		}
		if input.PublicStatus.ExplanatoryCopy != nil {
			if value, ok := sanitizeString(*input.PublicStatus.ExplanatoryCopy); ok {
				explanatoryCopy = value
			}
		}
		sortOrder = input.PublicStatus.SortOrder
		models = modelConfigsToAny(input.PublicStatus.PublicModels)
	}
	if models == nil {
		// Node 的 `sanitizeLegacyPublicModels(undefined…)` 返回 `[]`；空数组也要序列化成 `[]`
		// 而不是 `null`（`publicModels` 恒在）——故这里显式建一个非 nil 切片。
		models = []any{}
	}

	hasPublicStatus := displayName != "" || publicGroupSlug != "" || explanatoryCopy != "" ||
		sortOrder != nil || len(models) > 0
	if note == "" && !hasPublicStatus {
		return nil
	}

	description := serializedDescription{Version: publicStatusDescriptionVersion}
	if note != "" {
		description.Note = note
	}
	if hasPublicStatus {
		description.PublicStatus = &serializedPublicStatus{
			DisplayName:     displayName,
			PublicGroupSlug: publicGroupSlug,
			ExplanatoryCopy: explanatoryCopy,
			SortOrder:       sortOrder,
			PublicModels:    models,
		}
	}

	encoded, err := json.Marshal(description)
	if err != nil {
		// 定长结构体，序列化不可能失败。
		return nil
	}
	value := string(encoded)
	return &value
}

// sanitizeStringForSerialize 是 `sanitizeString(input.note) ?? null` 的读入侧：
// 区分「没给 / 给了空白」与「给了值」，避免把指针解引用拆成两处判空。
func sanitizeStringForSerialize(value *string) (string, bool) {
	if value == nil {
		return "", false
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return "", false
	}
	return trimmed, true
}
