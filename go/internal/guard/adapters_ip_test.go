package guard

import (
	"context"
	"encoding/json"
	"testing"
)

// 本文件钉住 IP 解析的可见行为：规则链顺序、取值模式、以及「非法值一律跳过、绝不误判」。

func TestExtractClientIPDefaultChainIgnoresUntrustedHeaders(t *testing.T) {
	headers := map[string][]string{
		// 最左 XFF 与 cf-connecting-ip 都是客户端可控的，默认链不得采信。
		"Cf-Connecting-Ip": {"203.0.113.9"},
		"X-Forwarded-For":  {"203.0.113.9, 198.51.100.7"},
	}
	if got := extractClientIP(headers, defaultIPHeaderRules()); got != "198.51.100.7" {
		t.Fatalf("默认链应取最右 XFF，得到 %q", got)
	}
}

func TestExtractClientIPHonorsConfiguredChain(t *testing.T) {
	headers := map[string][]string{
		"Cf-Connecting-Ip": {"203.0.113.9"},
		"X-Real-Ip":        {"198.51.100.7"},
		"X-Forwarded-For":  {"10.0.0.1, 10.0.0.2"},
	}
	raw := json.RawMessage(`{"headers":[{"name":"cf-connecting-ip"},{"name":"x-real-ip"}]}`)
	cfg, ok := parseIPExtractionConfig(raw)
	if !ok {
		t.Fatal("配置应能解码")
	}
	if got := extractClientIP(headers, cfg); got != "203.0.113.9" {
		t.Fatalf("配置链应取 cf-connecting-ip，得到 %q", got)
	}

	// 显式空规则表 == 不信任任何头（不得静默退回默认链）。
	empty, ok := parseIPExtractionConfig(json.RawMessage(`{"headers":[]}`))
	if !ok || len(empty) != 0 {
		t.Fatalf("显式空表应被接受为空规则，得到 ok=%v len=%d", ok, len(empty))
	}
	if got := extractClientIP(headers, empty); got != "" {
		t.Fatalf("空规则表应解析不出 IP，得到 %q", got)
	}
}

func TestParseIPExtractionConfigPickModes(t *testing.T) {
	cases := []struct {
		raw      string
		wantMode int
		wantIdx  int
	}{
		{`{"headers":[{"name":"x-forwarded-for"}]}`, ipPickRightmost, 0},
		{`{"headers":[{"name":"x-forwarded-for","pick":"leftmost"}]}`, ipPickLeftmost, 0},
		{`{"headers":[{"name":"x-forwarded-for","pick":{"kind":"index","index":2}}]}`, ipPickIndex, 2},
	}
	for _, testCase := range cases {
		rules, ok := parseIPExtractionConfig(json.RawMessage(testCase.raw))
		if !ok || len(rules) != 1 {
			t.Fatalf("%s 解码失败", testCase.raw)
		}
		if rules[0].pick.mode != testCase.wantMode || rules[0].pick.index != testCase.wantIdx {
			t.Fatalf("%s 模式解析错误：%+v", testCase.raw, rules[0].pick)
		}
	}
	// 非法 pick 与非法 JSON 一律拒绝（由调用方退回默认链）。
	if _, ok := parseIPExtractionConfig(json.RawMessage(`{"headers":[{"name":"x","pick":"middle"}]}`)); !ok {
		t.Fatal("非法 pick 应跳过该规则而不是整表失败")
	}
	if rules, ok := parseIPExtractionConfig(json.RawMessage(`{"headers":[{"name":"x","pick":"middle"}]}`)); ok && len(rules) != 0 {
		t.Fatalf("非法 pick 的规则应被丢弃，得到 %+v", rules)
	}
	if _, ok := parseIPExtractionConfig(json.RawMessage(`{`)); ok {
		t.Fatal("非法 JSON 应返回 false")
	}
}

func TestExtractClientIPNormalization(t *testing.T) {
	cases := []struct {
		raw  string
		want string
	}{
		{"198.51.100.7", "198.51.100.7"},
		{"198.51.100.7:5678", "198.51.100.7"},
		{"[2001:db8::1]", "2001:db8::1"},
		{"[2001:db8::1]:443", "2001:db8::1"},
		{"2001:db8::1", "2001:db8::1"},
		{"not-an-ip", ""},
		{"", ""},
		{"[2001:db8::1", ""},
	}
	for _, testCase := range cases {
		got := extractClientIP(map[string][]string{"X-Real-Ip": {testCase.raw}}, defaultIPHeaderRules())
		if got != testCase.want {
			t.Fatalf("normalize(%q) = %q，期望 %q", testCase.raw, got, testCase.want)
		}
	}
}

func TestExtractClientIPSkipsInvalidRuleAndTriesNext(t *testing.T) {
	// 第一条规则命中「非 IP」，必须继续走第二条，而不是就此返回空。
	headers := map[string][]string{
		"X-Real-Ip":       {"unknown"},
		"X-Forwarded-For": {"198.51.100.7"},
	}
	if got := extractClientIP(headers, defaultIPHeaderRules()); got != "198.51.100.7" {
		t.Fatalf("应跳过非法值并采信下一条规则，得到 %q", got)
	}
}

func TestIPExtractorAdapterFallsBackToDefaultChain(t *testing.T) {
	// settings 为 nil（未接线）：仍按默认链解析，绝不返回错误。
	adapter := newIPExtractor(nil, quietLogger())
	ip, err := adapter.ClientIP(context.Background(), map[string][]string{"X-Real-Ip": {"198.51.100.7"}})
	if err != nil || ip != "198.51.100.7" {
		t.Fatalf("默认链解析失败：ip=%q err=%v", ip, err)
	}
}
