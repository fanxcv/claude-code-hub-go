package store

import (
	"regexp"
	"strings"
	"testing"
)

// 本文件是**默认错误规则表**的纯单测（不依赖数据库）：表自身的不变量，以及「哪类上游文案该命中」
// 这一判定本身。它钉住的是 store 的表内容，判定语义的镜像如下所述。
//
// 判定语义的镜像：装载与匹配只在这三处实现，且三者同序（contains → exact → regex，一律先小写，
// regex 编译加 `(?i)`）——guard/adapters_rules.go 的 matches()（数据面）、
// adminapi/error_rules_shared.go 的 detect()（`:test` 端点）、本文件的 defaultRuleMatcher。
// 真正的端到端行为（命中即 1 次尝试、不切换）由 forward 包的
// TestForwardNonRetryableClientErrorStopsImmediately 钉住。

type defaultRuleMatcher struct {
	contains []string
	exact    map[string]struct{}
	regex    []*regexp.Regexp
}

// compileDefaultRules 按 guard 的装载口径编译一组规则。
func compileDefaultRules(t *testing.T, rules []AdminDefaultErrorRule) defaultRuleMatcher {
	t.Helper()
	matcher := defaultRuleMatcher{exact: map[string]struct{}{}}
	for _, rule := range rules {
		pattern := strings.TrimSpace(rule.Pattern)
		switch rule.MatchType {
		case "contains":
			matcher.contains = append(matcher.contains, strings.ToLower(pattern))
		case "exact":
			matcher.exact[strings.ToLower(pattern)] = struct{}{}
		case "regex":
			compiled, err := regexp.Compile("(?i)" + pattern)
			if err != nil {
				t.Fatalf("默认规则 %q 的正则无法编译: %v", pattern, err)
			}
			matcher.regex = append(matcher.regex, compiled)
		}
	}
	return matcher
}

func (m defaultRuleMatcher) matches(content string) bool {
	if strings.TrimSpace(content) == "" {
		return false
	}
	lower := strings.ToLower(content)
	for _, pattern := range m.contains {
		if strings.Contains(lower, pattern) {
			return true
		}
	}
	if len(m.exact) > 0 {
		if _, ok := m.exact[strings.TrimSpace(lower)]; ok {
			return true
		}
	}
	for _, re := range m.regex {
		if re.MatchString(content) {
			return true
		}
	}
	return false
}

// TestAdminDefaultErrorRulesInvariants 钉住默认表自身的不变量：这些一旦破坏，同步会在库里报冲突
// 或静默丢规则（regex 编译失败的那条只丢自己，见 guard 的装载）。
func TestAdminDefaultErrorRulesInvariants(t *testing.T) {
	if len(adminDefaultErrorRules) == 0 {
		t.Fatal("默认错误规则表为空")
	}
	seen := make(map[string]bool, len(adminDefaultErrorRules))
	allowed := map[string]bool{"contains": true, "exact": true, "regex": true}
	for _, rule := range adminDefaultErrorRules {
		if strings.TrimSpace(rule.Pattern) == "" {
			t.Fatal("存在空 pattern 的默认规则")
		}
		if seen[rule.Pattern] {
			t.Fatalf("pattern 重复（库上有 unique_pattern 唯一索引）: %q", rule.Pattern)
		}
		seen[rule.Pattern] = true
		if !allowed[rule.MatchType] {
			t.Fatalf("match_type 非法: %q", rule.MatchType)
		}
		if strings.TrimSpace(rule.Category) == "" {
			t.Fatalf("category 为空: %q", rule.Pattern)
		}
		if len(rule.Category) > 50 {
			t.Fatalf("category 超过 varchar(50): %q", rule.Category)
		}
		if rule.MatchType == "regex" {
			if _, err := regexp.Compile("(?i)" + rule.Pattern); err != nil {
				t.Fatalf("正则无法编译: %q: %v", rule.Pattern, err)
			}
		}
	}
}

// TestAdminUnsupportedInputErrorRulesMatchUpstreamRejections 钉住 Go 侧增补那一族的命中边界。
//
// 真实文案取自 2026-09-16 生产实测（上游对 URL 形态图片的拒绝体）。
func TestAdminUnsupportedInputErrorRulesMatchUpstreamRejections(t *testing.T) {
	if len(adminUnsupportedInputErrorRules) == 0 {
		t.Fatal("增补规则族为空")
	}
	matcher := compileDefaultRules(t, adminUnsupportedInputErrorRules)

	cases := []struct {
		name string
		body string
		want bool
	}{
		{
			name: "真实拒绝体：图片以 URL 形态送达（含 ref 后缀，故判据不得锚定行尾）",
			body: `{"error":{"message":"image URLs are not currently supported, please use base64 encoded data instead (ref: a9d442cb-0e5a-4e5a-9f4a-1a2b3c4d5e6f)","type":"invalid_request_error"}}`,
			want: true,
		},
		{name: "无 currently 的变体", body: "image urls are not supported", want: true},
		{name: "音频链接形态", body: "audio links are not currently supported", want: true},
		{name: "unsupported <媒体> url 形态", body: `{"error":{"message":"unsupported image url in content part"}}`, want: true},
		{name: "中文上游：图片URL暂不支持", body: "图片URL暂不支持，请改用 base64", want: true},
		{name: "中文上游：不支持图片链接", body: "不支持图片链接形式", want: true},

		{name: "边界：模态能力陈述（换一家供应商可能成功，仍须重试与切换）", body: "This model does not support image inputs", want: false},
		{name: "边界：中文模态能力陈述", body: "本模型不支持图片识别，请改用纯文本模型", want: false},
		{name: "边界：参数能力而非输入形态", body: "unsupported value for reasoning_effort: minimal", want: false},
		{name: "边界：工具能力陈述", body: "the selected model does not support tool calling", want: false},
		{name: "边界：普通可重试 400", body: `{"error":{"message":"rate limit exceeded, please retry later"}}`, want: false},
		{name: "边界：图片体积超限（走既有 image exceeds 规则）", body: "image exceeds 5 MB maximum: 6.2 MB", want: false},
		{name: "边界：空正文", body: "", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := matcher.matches(tc.body); got != tc.want {
				t.Fatalf("matches(%q) = %v, 期望 %v", tc.body, got, tc.want)
			}
		})
	}
}

// TestAdminDefaultErrorRulesIncludeUnsupportedInputFamily 钉住增补那一族确实进了同步表——
// 少拼一个 append，规则就进不了库（而这不会让任何编译或既有用例失败）。
func TestAdminDefaultErrorRulesIncludeUnsupportedInputFamily(t *testing.T) {
	inTable := make(map[string]bool, len(adminDefaultErrorRules))
	for _, rule := range adminDefaultErrorRules {
		inTable[rule.Pattern] = true
	}
	for _, rule := range adminUnsupportedInputErrorRules {
		if !inTable[rule.Pattern] {
			t.Fatalf("增补规则未进入默认表: %q", rule.Pattern)
		}
		if !rule.IsEnabled {
			t.Fatalf("增补规则必须为启用态（同步 only 插入启用行）: %q", rule.Pattern)
		}
	}
}
