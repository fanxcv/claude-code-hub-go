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

// defaultRuleMatcher 是默认表的纯内存镜像，口径与 guard/adapters_rules.go 的装载一致，
// 并且同样把 category 与样式同序存放——本文件的断言不只问「命没命」，还问「归哪一档」。
type defaultRuleMatcher struct {
	contains           []string
	containsCategories []string
	exact              map[string]string
	regex              []*regexp.Regexp
	regexCategories    []string
}

// compileDefaultRules 按 guard 的装载口径编译一组规则。
func compileDefaultRules(t *testing.T, rules []AdminDefaultErrorRule) defaultRuleMatcher {
	t.Helper()
	matcher := defaultRuleMatcher{exact: map[string]string{}}
	for _, rule := range rules {
		pattern := strings.TrimSpace(rule.Pattern)
		switch rule.MatchType {
		case "contains":
			matcher.contains = append(matcher.contains, strings.ToLower(pattern))
			matcher.containsCategories = append(matcher.containsCategories, rule.Category)
		case "exact":
			matcher.exact[strings.ToLower(pattern)] = rule.Category
		case "regex":
			compiled, err := regexp.Compile("(?i)" + pattern)
			if err != nil {
				t.Fatalf("默认规则 %q 的正则无法编译: %v", pattern, err)
			}
			matcher.regex = append(matcher.regex, compiled)
			matcher.regexCategories = append(matcher.regexCategories, rule.Category)
		}
	}
	return matcher
}

func (m defaultRuleMatcher) matches(content string) bool {
	return len(m.matchedCategories(content)) > 0
}

// matchedCategories 扫全表并去重返回命中的 category，顺序与 guard 的装载一致：
// contains -> exact -> regex。
func (m defaultRuleMatcher) matchedCategories(content string) []string {
	if strings.TrimSpace(content) == "" {
		return nil
	}
	lower := strings.ToLower(content)
	matched := make([]string, 0, 2)
	seen := map[string]bool{}
	record := func(category string) {
		if seen[category] {
			return
		}
		seen[category] = true
		matched = append(matched, category)
	}
	for index, pattern := range m.contains {
		if strings.Contains(lower, pattern) {
			record(m.containsCategories[index])
		}
	}
	if len(m.exact) > 0 {
		if category, ok := m.exact[strings.TrimSpace(lower)]; ok {
			record(category)
		}
	}
	for index, re := range m.regex {
		if re.MatchString(content) {
			record(m.regexCategories[index])
		}
	}
	return matched
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

// TestAdminUnsupportedInputErrorRulesMatchUpstreamRejections 钉住 Go 侧增补那一族的**命中边界与归口**。
//
// 真实文案取自 2026-09-16 生产实测（上游对 URL 形态图片的拒绝体）。命中和归口同等重要：
// 归错档要么把可换家自救的请求判死，要么把瞬时故障的同家重试提前掉。
func TestAdminUnsupportedInputErrorRulesMatchUpstreamRejections(t *testing.T) {
	if len(adminUnsupportedInputErrorRules) == 0 {
		t.Fatal("增补规则族为空")
	}
	matcher := compileDefaultRules(t, adminUnsupportedInputErrorRules)

	// want 为真时必须正好命中一族，且 category 必须是 provider_unsupported_input：
	// 它才能对应到 forward 的「同家不重试、可换家」那一档。
	cases := []struct {
		name     string
		body     string
		want     bool
		category string
	}{
		{
			name:     "真实拒绝体：图片以 URL 形态送达（含 ref 后缀，故判据不得锚定行尾）",
			body:     `{"error":{"message":"image URLs are not currently supported, please use base64 encoded data instead (ref: a9d442cb-0e5a-4e5a-9f4a-1a2b3c4d5e6f)","type":"invalid_request_error"}}`,
			want:     true,
			category: "provider_unsupported_input",
		},
		{
			name:     "上游自己说改送 base64：属本档（同家不重试，但可换家）",
			body:     "image URLs are not currently supported by this provider; retry with another provider",
			want:     true,
			category: "provider_unsupported_input",
		},
		{name: "无 currently 的变体", body: "image urls are not supported", want: true, category: "provider_unsupported_input"},
		{name: "音频链接形态", body: "audio links are not currently supported", want: true, category: "provider_unsupported_input"},
		{
			name:     "无系词变体：<媒体><URL> <不支持>",
			body:     "image url format not supported",
			want:     true,
			category: "provider_unsupported_input",
		},
		{name: "中文上游：图片URL暂不支持", body: "图片URL暂不支持，请改用 base64", want: true, category: "provider_unsupported_input"},
		{name: "中文上游：图片链接形式不支持", body: "图片链接形式不支持", want: true, category: "provider_unsupported_input"},

		// 以下为**有意不命中**的边界；每条都写明“不收”的理由，改样式时先读名字。
		{
			name: "边界：瞬时故障（fetch service unavailable；retry later）——被吞就丢掉同家重试",
			body: "temporarily unsupported image URL because the fetch service is unavailable; retry later",
			want: false,
		},
		{
			name: "边界：瞬时副词夹在系词与 supported 之间",
			body: "image URLs are temporarily unsupported by the fetch service",
			want: false,
		},
		{
			name: "边界：前缀形态（unsupported image url）——RE2 无 lookbehind，与瞬时口语同形，有意不收",
			body: `{"error":{"message":"unsupported image url in content part"}}`,
			want: false,
		},
		{
			name: "边界：中文前缀形态（不支持图片链接形式）——同样与“暂时不支持”同形，有意不收",
			body: "不支持图片链接形式",
			want: false,
		},
		{
			name: "边界：模型局部能力（当前模型不支持图片URL）——换家本就可能成，走可重试路径",
			body: "当前模型不支持图片URL，请切换到支持该能力的供应商",
			want: false,
		},
		{name: "边界：模态能力陈述", body: "This model does not support image inputs", want: false},
		{name: "边界：中文模态能力陈述", body: "本模型不支持图片识别，请改用纯文本模型", want: false},
		{name: "边界：参数能力而非输入形态", body: "unsupported value for reasoning_effort: minimal", want: false},
		{name: "边界：工具能力陈述", body: "the selected model does not support tool calling", want: false},
		{name: "边界：普通可重试 400", body: `{"error":{"message":"rate limit exceeded, please retry later"}}`, want: false},
		{name: "边界：图片体积超限（走既有 image exceeds 规则）", body: "image exceeds 5 MB maximum: 6.2 MB", want: false},
		{name: "边界：空正文", body: "", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			matched := matcher.matchedCategories(tc.body)
			if got := len(matched) > 0; got != tc.want {
				t.Fatalf("matchedCategories(%q) 命中=%v，期望 %v", tc.body, got, tc.want)
			}
			if !tc.want {
				return
			}
			if len(matched) != 1 || matched[0] != tc.category {
				t.Fatalf("matchedCategories(%q) = %v，期望恰好 [%s]", tc.body, matched, tc.category)
			}
		})
	}
}

// TestAdminUnsupportedInputErrorRulesCarrySwitchingCategory 钉住这一族的 category 值。
//
// 它是跨包契约：forward.RuleCategoryProviderUnsupportedInput 必须同值（那里有对应的钉子），
// 否则规则命中后会被当成「不可重试的客户端错误」，把可换家自救的请求判死。
func TestAdminUnsupportedInputErrorRulesCarrySwitchingCategory(t *testing.T) {
	for _, rule := range adminUnsupportedInputErrorRules {
		if rule.Category != "provider_unsupported_input" {
			t.Fatalf("规则 %q 的 category = %q，期望 provider_unsupported_input", rule.Pattern, rule.Category)
		}
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
