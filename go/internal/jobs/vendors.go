package jobs

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// 本文件是 vendor 图标与显示名的 Go 侧解析，复刻：
//   - src/lib/model-vendor/vendor-icon-files.ts（iconFileForVendor）
//   - src/lib/model-vendor/dash-prefix-lookup.ts（resolveByDashPrefix）
//   - src/lib/model-vendor/vendor-inference.ts:309-392（VENDOR_DISPLAY_NAMES / vendorDisplayName）
//
// 图标映射表是 src/lib/model-vendor/vendor-icon-map.json 的**逐字节副本**（它本身已是
// cch-plus.com 官方图标表的 verbatim 副本）。副本必须放在本包目录下才能 go:embed，
// 漂移由 vendors_test.go 与 ../../../src/lib/model-vendor/vendor-icon-map.json 逐字节比对兜住
// ——与 internal/ratelimit 对 lua/ 的做法一致。

//go:embed vendor-icon-map.json
var vendorIconMapJSON []byte

// VendorIconFileEntry 对应 TS 的 VendorIconFileEntry。
type VendorIconFileEntry struct {
	File string `json:"file"`
	Mono bool   `json:"mono"`
}

var (
	vendorIconsOnce sync.Once
	vendorIcons     map[string]VendorIconFileEntry
	vendorIconsErr  error
)

// vendorIconMap 返回解析后的图标映射表；解析只做一次，损坏时返回错误而不是 panic。
func vendorIconMap() (map[string]VendorIconFileEntry, error) {
	vendorIconsOnce.Do(func() {
		var parsed map[string]VendorIconFileEntry
		if err := json.Unmarshal(vendorIconMapJSON, &parsed); err != nil {
			vendorIconsErr = fmt.Errorf("解析内嵌 vendor 图标映射失败: %w", err)
			return
		}
		if len(parsed) == 0 {
			vendorIconsErr = fmt.Errorf("内嵌 vendor 图标映射为空")
			return
		}
		vendorIcons = parsed
	})
	return vendorIcons, vendorIconsErr
}

// resolveByDashPrefix 复刻 dash-prefix-lookup.ts：精确命中，否则逐段剥离末尾 dash 前缀。
func resolveByDashPrefix[T any](slug string, table map[string]T) (T, bool) {
	var zero T
	key := strings.ToLower(strings.TrimSpace(slug))
	if key == "" {
		return zero, false
	}
	if value, ok := table[key]; ok {
		return value, true
	}
	probe := key
	for strings.Contains(probe, "-") {
		probe = probe[:strings.LastIndex(probe, "-")]
		if value, ok := table[probe]; ok {
			return value, true
		}
	}
	return zero, false
}

// iconFileForVendor 复刻 iconFileForVendor（vendor-icon-files.ts:23）。
func iconFileForVendor(slug string) (VendorIconFileEntry, bool) {
	table, err := vendorIconMap()
	if err != nil {
		return VendorIconFileEntry{}, false
	}
	return resolveByDashPrefix(slug, table)
}

// vendorDisplayNames 是 vendor-inference.ts:309 的原样表。
//
// 只搬转换器用得到的那张表，不搬整个 vendor-inference（那包含从模型名反推 vendor 的
// 大段规则，价格同步路径用不到）。漂移由 vendors_test.go 解析 TS 原文比对兜住。
var vendorDisplayNames = map[string]string{
	"other":        "Other",
	"anthropic":    "Anthropic",
	"openai":       "OpenAI",
	"google":       "Google",
	"meta":         "Meta",
	"deepseek":     "DeepSeek",
	"alibaba":      "Alibaba",
	"mistral":      "Mistral",
	"xai":          "xAI",
	"cohere":       "Cohere",
	"ai21":         "AI21",
	"moonshotai":   "Moonshot AI",
	"zhipuai":      "Zhipu AI",
	"minimax":      "MiniMax",
	"perplexity":   "Perplexity",
	"stepfun":      "StepFun",
	"baidu":        "Baidu",
	"tencent":      "Tencent",
	"bytedance":    "ByteDance",
	"xiaomi":       "Xiaomi",
	"01-ai":        "01.AI",
	"reka":         "Reka",
	"nvidia":       "NVIDIA",
	"ibm":          "IBM",
	"liquid":       "Liquid AI",
	"amazon":       "Amazon",
	"inception":    "Inception",
	"morph":        "Morph",
	"360":          "360",
	"microsoft":    "Microsoft",
	"iflytek":      "iFlytek",
	"tii":          "TII",
	"deepgram":     "Deepgram",
	"jina":         "Jina AI",
	"voyage":       "Voyage AI",
	"baai":         "BAAI",
	"bfl":          "Black Forest Labs",
	"kling":        "Kling",
	"recraft":      "Recraft",
	"longcat":      "LongCat",
	"alephalpha":   "Aleph Alpha",
	"antgroup":     "Ant Group",
	"arcee":        "Arcee AI",
	"assemblyai":   "AssemblyAI",
	"baichuan":     "Baichuan",
	"briaai":       "Bria AI",
	"coqui":        "Coqui",
	"databricks":   "Databricks",
	"elevenlabs":   "ElevenLabs",
	"essentialai":  "Essential AI",
	"fishaudio":    "Fish Audio",
	"haiper":       "Haiper",
	"hedra":        "Hedra",
	"ideogram":     "Ideogram",
	"inflection":   "Inflection AI",
	"internlm":     "InternLM",
	"kwaipilot":    "Kwaipilot",
	"llava":        "LLaVA",
	"luma":         "Luma AI",
	"midjourney":   "Midjourney",
	"myshell":      "MyShell",
	"nousresearch": "Nous Research",
	"novelai":      "NovelAI",
	"openchat":     "OpenChat",
	"pika":         "Pika",
	"pixverse":     "PixVerse",
	"reve":         "Reve",
	"runway":       "Runway",
	"rwkv":         "RWKV",
	"sensenova":    "SenseNova",
	"skywork":      "Skywork",
	"stability":    "Stability AI",
	"suno":         "Suno",
	"tripo":        "Tripo",
	"upstage":      "Upstage",
	"vidu":         "Vidu",
	"xuanyuan":     "XuanYuan",
	"yandex":       "Yandex",
}

// vendorDisplayName 复刻 vendorDisplayName：表里没有就回落到 slug 本身。
func vendorDisplayName(slug string) string {
	if name, ok := vendorDisplayNames[slug]; ok {
		return name
	}
	return slug
}

// resolveVendorIcon 复刻 cpt-convert.ts:349 resolveVendorIcon：
// providers 字典给的 icon 优先，否则查内嵌图标映射表。
func resolveVendorIcon(vendor string, providers map[string]CptProviderInfo) (VendorIconFileEntry, bool) {
	if info, ok := providers[vendor]; ok && info.Icon != "" {
		return VendorIconFileEntry{File: info.Icon, Mono: info.IconMono}, true
	}
	return iconFileForVendor(vendor)
}
