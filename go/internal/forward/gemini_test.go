package forward

import (
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// TestGeminiCredential 钉住凭据形态判定（Node 的 GeminiAuth.getAccessToken / isApiKey）。
func TestGeminiCredential(t *testing.T) {
	cases := []struct {
		name     string
		key      string
		token    string
		isAPIKey bool
	}{
		{name: "普通 API Key", key: "AIzaSy-example", token: "AIzaSy-example", isAPIKey: true},
		{name: "Google access token 前缀仍走 Bearer", key: "ya29.abc", token: "ya29.abc", isAPIKey: false},
		{
			name:     "OAuth JSON 取 access_token 并走 Bearer",
			key:      `{"access_token":"ya29.from-json","refresh_token":"r","client_id":"c","client_secret":"s"}`,
			token:    "ya29.from-json",
			isAPIKey: false,
		},
		{
			name:     "JSON 无 access_token：退回原文当密钥（与 Node 的 parse 失败分支等价）",
			key:      `{"refresh_token":"r"}`,
			token:    `{"refresh_token":"r"}`,
			isAPIKey: true,
		},
		{
			name:     "JSON 损坏：同样退回原文",
			key:      `{"access_token":`,
			token:    `{"access_token":`,
			isAPIKey: true,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			token, isAPIKey := geminiCredential(testCase.key)
			if token != testCase.token {
				t.Fatalf("token = %q，期望 %q", token, testCase.token)
			}
			if isAPIKey != testCase.isAPIKey {
				t.Fatalf("isAPIKey = %v，期望 %v", isAPIKey, testCase.isAPIKey)
			}
		})
	}
}

// TestBuildUpstreamHeadersGeminiAPIKey 钉住 API Key 形态：x-goog-api-key 用供应商凭据，
// 客户端自带的同名头与 x-api-key 都不得透传（Node buildGeminiHeaders 的黑名单）。
func TestBuildUpstreamHeadersGeminiAPIKey(t *testing.T) {
	clientHeaders := map[string][]string{
		"X-Goog-Api-Key": {"client-key-must-not-pass"},
		"X-Api-Key":      {"client-key-must-not-pass"},
		"User-Agent":     {"gemini-cli/1.2.3"},
	}
	headers := BuildUpstreamHeaders(HeaderInput{
		ClientHeaders: clientHeaders,
		Provider: Provider{
			Type: convert.ProviderGemini,
			Key:  "upstream-gemini-key",
		},
		BaseURL:         "https://generativelanguage.googleapis.com/v1beta",
		ClientUserAgent: "gemini-cli/1.2.3",
	})

	if got := headers.Get("x-goog-api-key"); got != "upstream-gemini-key" {
		t.Fatalf("x-goog-api-key 应为供应商凭据，实为 %q", got)
	}
	if got := headers.Get("x-api-key"); got != "" {
		t.Fatalf("客户端 x-api-key 不得透传，实为 %q", got)
	}
	if got := headers.Get("authorization"); got != "" {
		t.Fatalf("API Key 形态不应带 Authorization，实为 %q", got)
	}
	if got := headers.Get("host"); got != "generativelanguage.googleapis.com" {
		t.Fatalf("host 应指向上游基址，实为 %q", got)
	}
	if got := headers.Get("accept-encoding"); got != "identity" {
		t.Fatalf("accept-encoding 应为 identity，实为 %q", got)
	}
	if got := headers.Get("user-agent"); got != "gemini-cli/1.2.3" {
		t.Fatalf("user-agent 应保留客户端值，实为 %q", got)
	}
}

// TestBuildUpstreamHeadersGeminiOAuthGoesBearer 钉住 OAuth 形态：走 Authorization: Bearer。
func TestBuildUpstreamHeadersGeminiOAuthGoesBearer(t *testing.T) {
	headers := BuildUpstreamHeaders(HeaderInput{
		ClientHeaders: map[string][]string{},
		Provider: Provider{
			Type: convert.ProviderGemini,
			Key:  `{"access_token":"ya29.token"}`,
		},
		BaseURL: "https://generativelanguage.googleapis.com/v1beta",
	})
	if got := headers.Get("authorization"); got != "Bearer ya29.token" {
		t.Fatalf("OAuth 形态应走 Bearer，实为 %q", got)
	}
	if got := headers.Get("x-goog-api-key"); got != "" {
		t.Fatalf("OAuth 形态不应带 x-goog-api-key，实为 %q", got)
	}
}

// TestBuildUpstreamHeadersGeminiCLIClientHeader 钉住 gemini-cli 的写死客户端标识。
func TestBuildUpstreamHeadersGeminiCLIClientHeader(t *testing.T) {
	headers := BuildUpstreamHeaders(HeaderInput{
		ClientHeaders: map[string][]string{},
		Provider: Provider{
			Type: convert.ProviderGeminiCLI,
			Key:  "AIzaSy-example",
		},
		BaseURL: "https://cloudcode-pa.googleapis.com/v1internal",
	})
	if got := headers.Get("x-goog-api-client"); got != "GeminiCLI/1.0" {
		t.Fatalf("gemini-cli 应带 x-goog-api-client: GeminiCLI/1.0，实为 %q", got)
	}
}

// TestBuildUpstreamHeadersGeminiDefaultUserAgent 钉住无 UA 时的兜底（Node: claude-code-hub）。
func TestBuildUpstreamHeadersGeminiDefaultUserAgent(t *testing.T) {
	headers := BuildUpstreamHeaders(HeaderInput{
		ClientHeaders: map[string][]string{},
		Provider:      Provider{Type: convert.ProviderGemini, Key: "k"},
		BaseURL:       "https://generativelanguage.googleapis.com/v1beta",
	})
	if got := headers.Get("user-agent"); got != "claude-code-hub" {
		t.Fatalf("无客户端 UA 时应兜底，实为 %q", got)
	}
}

// TestClientStreamRequestedForGeminiShapes 钉住 Gemini 的流式信号（路径与 alt=sse）。
func TestClientStreamRequestedForGeminiShapes(t *testing.T) {
	cases := []struct {
		name      string
		pathname  string
		rawQuery  string
		body      string
		wantStrea bool
	}{
		{
			name:      "路径含 :streamGenerateContent",
			pathname:  "/v1beta/models/gemini-2.0-flash:streamGenerateContent",
			body:      `{"contents":[]}`,
			wantStrea: true,
		},
		{
			name:      "查询 alt=sse（大小写与额外参数都要认）",
			pathname:  "/v1beta/models/gemini-2.0-flash:generateContent",
			rawQuery:  "key=x&ALT=SSE",
			body:      `{"contents":[]}`,
			wantStrea: true,
		},
		{
			name:      "正文 stream:true（其它线形态）",
			pathname:  "/v1/messages",
			body:      `{"stream":true}`,
			wantStrea: true,
		},
		{
			name:      "普通非流式 Gemini 请求",
			pathname:  "/v1beta/models/gemini-2.0-flash:generateContent",
			body:      `{"contents":[]}`,
			wantStrea: false,
		},
		{
			name:      "alt=json 不算流式",
			pathname:  "/v1beta/models/gemini-2.0-flash:generateContent",
			rawQuery:  "alt=json",
			body:      `{"contents":[]}`,
			wantStrea: false,
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := ClientStreamRequestedFor(testCase.pathname, testCase.rawQuery, []byte(testCase.body))
			if got != testCase.wantStrea {
				t.Fatalf("ClientStreamRequestedFor = %v，期望 %v", got, testCase.wantStrea)
			}
		})
	}
}
