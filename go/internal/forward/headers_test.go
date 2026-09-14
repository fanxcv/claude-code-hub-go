package forward

import (
	"net/http"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

func newClientHeaders(pairs ...string) http.Header {
	headers := http.Header{}
	for i := 0; i+1 < len(pairs); i += 2 {
		headers.Set(pairs[i], pairs[i+1])
	}
	return headers
}

// TestBuildUpstreamHeadersOverrideOrder 断言覆盖顺序：默认头 → 自定义头 → 鉴权 → codex UA → 客户端 IP → 缓存 beta。
func TestBuildUpstreamHeadersOverrideOrder(t *testing.T) {
	headers := BuildUpstreamHeaders(HeaderInput{
		ClientHeaders: newClientHeaders(
			"content-type", "text/plain",
			"x-cch-internal-secret", "injected",
			"x-trace-id", "trace-1",
			"anthropic-version", "2023-06-01",
		),
		Provider: Provider{
			ID:   1,
			Type: convert.ProviderClaude,
			Key:  "sk-upstream",
			CustomHeaders: map[string]string{
				"x-custom":       "v1",
				"authorization":  "Bearer attacker",
				"host":           "evil.example",
				"content-length": "999",
			},
			PreserveClientIP: true,
		},
		BaseURL:    "https://api.example.com/anthropic",
		CacheTTL1h: true,
	})

	if got := headers.Get("host"); got != "api.example.com" {
		t.Fatalf("host = %q", got)
	}
	if got := headers.Get("content-type"); got != "application/json" {
		t.Fatalf("content-type = %q", got)
	}
	if got := headers.Get("accept-encoding"); got != "identity" {
		t.Fatalf("accept-encoding = %q", got)
	}
	if got := headers.Get("x-custom"); got != "v1" {
		t.Fatalf("自定义头未生效: %q", got)
	}
	if got := headers.Get("authorization"); got != "Bearer sk-upstream" {
		t.Fatalf("鉴权头必须覆盖自定义头: %q", got)
	}
	if got := headers.Get("x-api-key"); got != "sk-upstream" {
		t.Fatalf("claude 供应商应同时发 x-api-key: %q", got)
	}
	if got := headers.Get("content-length"); got == "999" {
		t.Fatal("保留名 content-length 不得由自定义头写入")
	}
	if got := headers.Get("x-cch-internal-secret"); got != "" {
		t.Fatalf("内部标记头不得透传: %q", got)
	}
	if got := headers.Get("x-trace-id"); got != "" {
		t.Fatalf("追踪头应在黑名单内: %q", got)
	}
	if got := headers.Get("anthropic-version"); got != "2023-06-01" {
		t.Fatalf("普通客户端头应透传: %q", got)
	}
	if got := headers.Get("anthropic-beta"); got != "extended-cache-ttl-2025-04-11, prompt-caching-2024-07-31" {
		t.Fatalf("1h 缓存标记缺失: %q", got)
	}
}

// TestBuildUpstreamHeadersStripsClientAuthAndForwarding 断言客户端鉴权与转发头一律不出站。
func TestBuildUpstreamHeadersStripsClientAuthAndForwarding(t *testing.T) {
	headers := BuildUpstreamHeaders(HeaderInput{
		ClientHeaders: newClientHeaders(
			"authorization", "Bearer client-key",
			"x-api-key", "client-key",
			"x-forwarded-for", "1.2.3.4",
			"x-real-ip", "1.2.3.4",
			"connection", "keep-alive",
		),
		Provider: Provider{ID: 2, Type: convert.ProviderOpenAICompatible, Key: "sk-up"},
		BaseURL:  "https://api.example.com",
	})

	if got := headers.Get("authorization"); got != "Bearer sk-up" {
		t.Fatalf("authorization = %q", got)
	}
	if got := headers.Get("x-api-key"); got != "" {
		t.Fatalf("客户端 x-api-key 必须剥离: %q", got)
	}
	if got := headers.Get("x-forwarded-for"); got != "" {
		t.Fatalf("未开启保留时不得透传 x-forwarded-for: %q", got)
	}
	if got := headers.Get("x-real-ip"); got != "" {
		t.Fatalf("未开启保留时不得透传 x-real-ip: %q", got)
	}
	if got := headers.Get("connection"); got != "" {
		t.Fatalf("逐跳头必须剥离: %q", got)
	}
}

// TestBuildUpstreamHeadersInjectsClientIPWhenPreserved 断言保留开关打开时按客户端头重新注入 IP。
func TestBuildUpstreamHeadersInjectsClientIPWhenPreserved(t *testing.T) {
	headers := BuildUpstreamHeaders(HeaderInput{
		ClientHeaders: newClientHeaders(
			"x-forwarded-for", "10.0.0.1, 10.0.0.2",
			"x-real-ip", "10.0.0.1",
		),
		Provider: Provider{ID: 3, Type: convert.ProviderClaude, Key: "sk", PreserveClientIP: true},
		BaseURL:  "https://api.example.com",
	})
	if got := headers.Get("x-forwarded-for"); got != "10.0.0.1, 10.0.0.2" {
		t.Fatalf("x-forwarded-for = %q", got)
	}
	if got := headers.Get("x-real-ip"); got != "10.0.0.1" {
		t.Fatalf("x-real-ip = %q", got)
	}
}

// TestBuildUpstreamHeadersCodexUserAgent 断言 codex 的三分支 UA 取值。
func TestBuildUpstreamHeadersCodexUserAgent(t *testing.T) {
	base := HeaderInput{
		ClientHeaders:   newClientHeaders("user-agent", "codex-original"),
		Provider:        Provider{ID: 4, Type: convert.ProviderCodex, Key: "sk"},
		BaseURL:         "https://api.example.com",
		ClientUserAgent: "codex-original",
	}

	t.Run("过滤器未改动时用原始 UA", func(t *testing.T) {
		headers := BuildUpstreamHeaders(base)
		if got := headers.Get("user-agent"); got != "codex-original" {
			t.Fatalf("user-agent = %q", got)
		}
	})

	t.Run("过滤器改写后用过滤值", func(t *testing.T) {
		in := base
		in.UserAgentModified = true
		in.FilteredUserAgent = "codex-filtered"
		headers := BuildUpstreamHeaders(in)
		if got := headers.Get("user-agent"); got != "codex-filtered" {
			t.Fatalf("user-agent = %q", got)
		}
	})

	t.Run("过滤器删除且原本无 UA 时用兜底值", func(t *testing.T) {
		in := base
		in.ClientUserAgent = ""
		in.ClientHeaders = http.Header{}
		in.UserAgentModified = true
		in.FilteredUserAgent = ""
		headers := BuildUpstreamHeaders(in)
		if got := headers.Get("user-agent"); got != defaultCodexUserAgent {
			t.Fatalf("user-agent = %q", got)
		}
	})
}

// TestResolveAnthropicAuthHeaders 断言三种鉴权形态的判定顺序。
func TestResolveAnthropicAuthHeaders(t *testing.T) {
	cases := []struct {
		name      string
		url       string
		bearer    bool
		wantKey   string
		wantAuth  string
		wantNoKey bool
	}{
		{
			name:     "官方域名默认双发",
			url:      "https://api.anthropic.com",
			wantKey:  "sk-1",
			wantAuth: "Bearer sk-1",
		},
		{
			name:      "claude-auth 只发 bearer",
			url:       "https://api.anthropic.com",
			bearer:    true,
			wantAuth:  "Bearer sk-1",
			wantNoKey: true,
		},
		{
			name:      "relay 主机名只发 bearer",
			url:       "https://my-relay.example.com",
			wantAuth:  "Bearer sk-1",
			wantNoKey: true,
		},
		{
			name:      "AWS External 网关只发 x-api-key，忽略 bearer 要求",
			url:       "https://aws-external-anthropic.us-east-1.api.aws",
			bearer:    true,
			wantKey:   "sk-1",
			wantNoKey: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveAnthropicAuthHeaders("sk-1", tc.url, tc.bearer)
			if tc.wantAuth == "" {
				if _, ok := got["authorization"]; ok {
					t.Fatalf("不应有 authorization: %v", got)
				}
			} else if got["authorization"] != tc.wantAuth {
				t.Fatalf("authorization = %q", got["authorization"])
			}
			if tc.wantNoKey {
				if _, ok := got["x-api-key"]; ok {
					t.Fatalf("不应有 x-api-key: %v", got)
				}
				return
			}
			if got["x-api-key"] != tc.wantKey {
				t.Fatalf("x-api-key = %q", got["x-api-key"])
			}
		})
	}
}

// TestLooksLikeAnthropicProxyURL 断言官方域名不被误判为代理。
func TestLooksLikeAnthropicProxyURL(t *testing.T) {
	cases := map[string]bool{
		"https://api.anthropic.com":            false,
		"https://claude.ai":                    false,
		"https://my-relay.example.com":         true,
		"https://openrouter.ai":                true,
		"https://api.business.example.com":     false,
		"https://gateway.internal.example.com": true,
	}
	for rawURL, want := range cases {
		if got := looksLikeAnthropicProxyURL(rawURL); got != want {
			t.Fatalf("looksLikeAnthropicProxyURL(%q) = %v，期望 %v", rawURL, got, want)
		}
	}
}

// TestExtractHostFallback 断言非法基址的兜底行为与 Node 一致。
func TestExtractHostFallback(t *testing.T) {
	if got := extractHost("https://api.example.com/v1"); got != "api.example.com" {
		t.Fatalf("extractHost = %q", got)
	}
	if got := extractHost("not a url at all"); got != "localhost" {
		t.Fatalf("兜底应为 localhost，实际 %q", got)
	}
}

// TestMergeAnthropicCacheTTLBetaFlag 断言合并语义：既有标记按序去重，再无条件补齐两项依赖。
func TestMergeAnthropicCacheTTLBetaFlag(t *testing.T) {
	cases := map[string]string{
		"":                                      "extended-cache-ttl-2025-04-11, prompt-caching-2024-07-31",
		"prompt-caching-2024-07-31":             "prompt-caching-2024-07-31, extended-cache-ttl-2025-04-11",
		"extended-cache-ttl-2025-04-11":         "extended-cache-ttl-2025-04-11, prompt-caching-2024-07-31",
		"other-beta, prompt-caching-2024-07-31": "other-beta, prompt-caching-2024-07-31, extended-cache-ttl-2025-04-11",
	}
	for input, want := range cases {
		if got := mergeAnthropicCacheTTLBetaFlag(input); got != want {
			t.Fatalf("mergeAnthropicCacheTTLBetaFlag(%q) = %q，期望 %q", input, got, want)
		}
	}
}
