package jobs

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// 这两个文件是「Go 侧副本」的漂移钉子：
//   - vendor-icon-map.json 是 src/lib/model-vendor/vendor-icon-map.json 的逐字节副本；
//   - vendorDisplayNames 是 vendor-inference.ts 里那张表的逐项副本。
//
// 漂移的后果是静默的：价格行会带上错的图标或缺省显示名，只有在 UI 上才看得出来。
// 因此这里直接读仓库里的 TS 原文比对，Node 侧一改这两处，本包测试即报红。

const (
	repoVendorIconMap   = "../../../src/lib/model-vendor/vendor-icon-map.json"
	repoVendorInference = "../../../src/lib/model-vendor/vendor-inference.ts"
)

func TestEmbeddedVendorIconMapIsByteIdenticalToRepoCopy(t *testing.T) {
	authoritative, err := os.ReadFile(filepath.Clean(repoVendorIconMap))
	if err != nil {
		t.Fatalf("读取权威图标映射失败: %v", err)
	}
	if !bytes.Equal(authoritative, vendorIconMapJSON) {
		t.Fatalf("内嵌图标映射与 %s 不一致：Node 侧变更后需重新拷贝到 go/internal/jobs/",
			repoVendorIconMap)
	}
}

func TestVendorDisplayNamesMatchTypeScriptSource(t *testing.T) {
	source, err := os.ReadFile(filepath.Clean(repoVendorInference))
	if err != nil {
		t.Fatalf("读取 vendor-inference.ts 失败: %v", err)
	}

	block := regexp.MustCompile(`(?s)VENDOR_DISPLAY_NAMES[^=]*=\s*\{(.*?)\n\};`).FindSubmatch(source)
	if block == nil {
		t.Fatal("未在 vendor-inference.ts 中定位 VENDOR_DISPLAY_NAMES 字面量")
	}
	entry := regexp.MustCompile(`(?m)^\s*"?([A-Za-z0-9_-]+)"?:\s*"([^"]*)",?\s*$`)

	parsed := map[string]string{}
	for _, match := range entry.FindAllSubmatch(block[1], -1) {
		parsed[string(match[1])] = string(match[2])
	}
	if len(parsed) < 50 {
		t.Fatalf("解析到的条目过少（%d），解析规则可能已失效", len(parsed))
	}
	if len(parsed) != len(vendorDisplayNames) {
		t.Fatalf("条目数不一致: TS %d, Go %d", len(parsed), len(vendorDisplayNames))
	}
	for slug, name := range parsed {
		if got, ok := vendorDisplayNames[slug]; !ok || got != name {
			t.Fatalf("vendor %q 的显示名不一致: TS %q, Go %q（存在=%t）", slug, name, got, ok)
		}
	}
}

func TestResolveByDashPrefixFallsBackToLongestPrefix(t *testing.T) {
	table := map[string]string{"alibaba": "alibaba.svg", "01-ai": "zeroone.svg"}
	cases := []struct {
		slug string
		want string
		ok   bool
	}{
		{"alibaba", "alibaba.svg", true},
		{"alibaba-coding-plan-cn", "alibaba.svg", true},
		{"01-ai", "zeroone.svg", true},
		{"unknown-vendor-x", "", false},
		{"  Alibaba-Coding-Plan  ", "alibaba.svg", true},
		{"", "", false},
	}
	for _, testCase := range cases {
		got, ok := resolveByDashPrefix(testCase.slug, table)
		if ok != testCase.ok || got != testCase.want {
			t.Fatalf("resolveByDashPrefix(%q) = (%q,%t), 期望 (%q,%t)",
				testCase.slug, got, ok, testCase.want, testCase.ok)
		}
	}
}

func TestResolveVendorIconPrefersProvidersDictionary(t *testing.T) {
	providers := map[string]CptProviderInfo{
		"anthropic": {Name: "Anthropic", Icon: "custom.svg", IconMono: true},
	}
	icon, ok := resolveVendorIcon("anthropic", providers)
	if !ok || icon.File != "custom.svg" || !icon.Mono {
		t.Fatalf("providers 字典应优先: %+v", icon)
	}
	// providers 未给图标时回落到内嵌映射表（openai 必在其中）。
	icon, ok = resolveVendorIcon("openai", map[string]CptProviderInfo{})
	if !ok || icon.File == "" {
		t.Fatalf("应回落到内嵌映射表, 实际 %+v (ok=%t)", icon, ok)
	}
}

func TestVendorDisplayNameFallsBackToSlug(t *testing.T) {
	if got := vendorDisplayName("anthropic"); got != "Anthropic" {
		t.Fatalf("anthropic 的显示名应为 Anthropic, 实际 %q", got)
	}
	if got := vendorDisplayName("some-unknown-vendor"); got != "some-unknown-vendor" {
		t.Fatalf("未知 vendor 应回落到 slug, 实际 %q", got)
	}
}
