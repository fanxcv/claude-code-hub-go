package dataplane

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/guard"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
)

// TestVerboseNoProviderResponse 钉住 verbose_provider_error 打开时的详细 503 体。
//
// 判据来自 Node provider-selector.ts:522-558：文案取决于「限流 / 熔断」两类理由的条数
// 与 totalEnabled（剔除禁用、模型不允许、客户端名单拒绝三类后的家数）。
func TestVerboseNoProviderResponse(t *testing.T) {
	entries := func(reason route.Reason, count int) []guard.NoProviderFiltered {
		out := make([]guard.NoProviderFiltered, 0, count)
		for i := 0; i < count; i++ {
			out = append(out, guard.NoProviderFiltered{ID: int64(100 + i), Reason: string(reason)})
		}
		return out
	}
	decode := func(t *testing.T, body []byte) map[string]any {
		t.Helper()
		payload := map[string]any{}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("响应体不是 JSON：%v（%s）", err, string(body))
		}
		errorObj, _ := payload["error"].(map[string]any)
		if errorObj == nil {
			t.Fatalf("响应体缺 error 对象：%s", string(body))
		}
		return errorObj
	}

	cases := []struct {
		name        string
		filtered    []guard.NoProviderFiltered
		wantMessage string
		wantType    string
	}{
		{
			name:        "全部限流",
			filtered:    entries(route.ReasonRateLimited, 3),
			wantMessage: "All providers rate limited (3 providers)",
			wantType:    "rate_limit_exceeded",
		},
		{
			name:        "全部熔断",
			filtered:    entries(route.ReasonCircuitOpen, 2),
			wantMessage: "All providers circuit breaker open (2 providers)",
			wantType:    "circuit_breaker_open",
		},
		{
			name: "限流与熔断混合",
			filtered: append(entries(route.ReasonRateLimited, 1),
				entries(route.ReasonCircuitOpen, 2)...),
			wantMessage: "All providers unavailable (1 rate limited, 2 circuit open)",
			wantType:    "mixed_unavailable",
		},
		{
			// 关键在于 totalEnabled 要扣掉 model_not_allowed：1 家限流 + 1 家模型不允许
			// 时，可用家数是 1 而非 2，故判「全限流」。
			name: "限流加模型不允许（总数按可用家数算）",
			filtered: append(entries(route.ReasonRateLimited, 1),
				entries(route.ReasonModelNotAllowed, 1)...),
			wantMessage: "All providers rate limited (1 providers)",
			wantType:    "rate_limit_exceeded",
		},
		{
			name:        "无剔除留痕时退回固定文案",
			filtered:    nil,
			wantMessage: "No available providers",
			wantType:    "no_available_providers",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			response := verboseNoProviderResponse(guard.NoProviderDiagnostic{Filtered: testCase.filtered})
			if response.Status != 503 {
				t.Fatalf("状态码 = %d，期望 503", response.Status)
			}
			errorObj := decode(t, response.Body)
			if errorObj["message"] != testCase.wantMessage {
				t.Errorf("message = %v，期望 %q", errorObj["message"], testCase.wantMessage)
			}
			if errorObj["type"] != testCase.wantType {
				t.Errorf("type = %v，期望 %q", errorObj["type"], testCase.wantType)
			}
			details, _ := errorObj["details"].(map[string]any)
			if details == nil {
				t.Fatalf("详细模式必须带 details：%s", string(response.Body))
			}
			if details["excludedCount"] == nil || details["totalAttempts"] == nil {
				t.Errorf("details 缺计数键：%v", details)
			}
			// 脱敏纪律：条目只带 id 与理由，绝不带供应商名称。
			if len(testCase.filtered) > 0 {
				items, _ := details["filteredProviders"].([]any)
				if len(items) != len(testCase.filtered) {
					t.Fatalf("filteredProviders 条数 = %d，期望 %d", len(items), len(testCase.filtered))
				}
				first, _ := items[0].(map[string]any)
				if _, ok := first["name"]; ok {
					t.Errorf("filteredProviders 不得带供应商名称：%v", first)
				}
				if first["reason"] == nil || first["id"] == nil {
					t.Errorf("filteredProviders 条目缺 id/reason：%v", first)
				}
			}
		})
	}

	t.Run("客户端名单拒绝单独成键", func(t *testing.T) {
		response := verboseNoProviderResponse(guard.NoProviderDiagnostic{
			Filtered: append(entries(route.ReasonClientRestriction, 1),
				entries(route.ReasonRateLimited, 1)...),
		})
		errorObj := decode(t, response.Body)
		details, _ := errorObj["details"].(map[string]any)
		if _, ok := details["clientRestrictedProviders"]; !ok {
			t.Errorf("有 client_restriction 条目时必须带 clientRestrictedProviders：%v", details)
		}
	})
}

// TestGenericUpstreamErrorMessageTable 钉住状态码兜底文案表（Node error-handler.ts:76-105）。
func TestGenericUpstreamErrorMessageTable(t *testing.T) {
	cases := map[int]string{
		400: "上游请求参数无效，请检查后重试",
		401: "上游鉴权失败，请稍后重试",
		404: "上游资源不存在",
		408: "上游服务响应超时，请稍后重试",
		422: "上游无法处理当前请求",
		429: "上游服务当前限流，请稍后重试",
		504: "上游服务响应超时，请稍后重试",
		524: "上游服务响应超时，请稍后重试",
		500: "上游服务暂时不可用，请稍后重试",
		503: "上游服务暂时不可用，请稍后重试",
		418: "请求上游服务失败，请稍后重试",
	}
	for status, want := range cases {
		if got := genericUpstreamErrorMessage(status); got != want {
			t.Errorf("status %d 的兜底文案 = %q，期望 %q", status, got, want)
		}
	}
}

// TestDeriveClientSafeUpstreamErrorMessage 钉住派生与脱敏的两态边界。
//
// 判据来自 Node client-error-message.ts:164-197：候选文案含内部码、原始 JSON 载荷、
// 供应商品牌名、URL/域名/IP/请求 id/密钥形状时一律弃用（返回空串 → 调用方回退通用文案）。
func TestDeriveClientSafeUpstreamErrorMessage(t *testing.T) {
	derive := func(candidate, providerName string) string {
		return deriveClientSafeUpstreamErrorMessage(deriveClientSafeUpstreamErrorMessageInput{
			CandidateMessage: candidate,
			ProviderName:     providerName,
		})
	}

	t.Run("正常文案原样保留", func(t *testing.T) {
		if got := derive("context length exceeded", ""); got != "context length exceeded" {
			t.Errorf("派生 = %q，期望原样保留", got)
		}
	})

	t.Run("内部错误码被拒", func(t *testing.T) {
		for _, candidate := range []string{"EMPTY_RESPONSE", "HTTP 502", "No available providers"} {
			if got := derive(candidate, ""); got != "" {
				t.Errorf("%q 应被拒，得到 %q", candidate, got)
			}
		}
	})

	t.Run("原始 JSON 载荷被拒", func(t *testing.T) {
		if got := derive(`{"error":{"message":"boom"}}`, ""); got != "" {
			t.Errorf("JSON 载荷应被拒，得到 %q", got)
		}
		if got := derive(`Bad request: {"error":{"message":"boom"}}`, ""); got != "" {
			t.Errorf("内嵌 JSON 载荷应被拒，得到 %q", got)
		}
	})

	t.Run("供应商品牌名与自报名被拒", func(t *testing.T) {
		if got := derive("Claude upstream refused", ""); got != "" {
			t.Errorf("含品牌名应被拒，得到 %q", got)
		}
		if got := derive("relay-seven overloaded", "relay-seven"); got != "" {
			t.Errorf("含本次供应商名应被拒，得到 %q", got)
		}
		if got := derive("Provider acme returned 500", ""); got != "" {
			t.Errorf("Provider 前缀应被拒，得到 %q", got)
		}
	})

	t.Run("URL 域名 IP 与请求 id 被剥除", func(t *testing.T) {
		candidate := "upstream https://api.example.com/v1 failed at 198.51.100.7 request_id: 8f3c2b1a9d"
		got := derive(candidate, "")
		for _, forbidden := range []string{"example.com", "198.51.100.7", "8f3c2b1a9d", "https://"} {
			if strings.Contains(got, forbidden) {
				t.Errorf("派生文案仍含 %q：%q", forbidden, got)
			}
		}
		if got == "" {
			t.Fatal("剥除后应仍有可交付文案")
		}
	})

	t.Run("长密钥被脱敏后保留", func(t *testing.T) {
		// Node 的 sanitizeErrorTextForDetail 会把 ≥16 位的 sk-/rk-/pk- 前缀密钥换成
		// [REDACTED_KEY]，故这类文案**保留**（不再含密钥形状）——这是脱敏而非丢弃。
		got := derive("invalid key sk-abcdefghijklmnop1234567890", "")
		if got != "invalid key [REDACTED_KEY]" {
			t.Errorf("派生 = %q，期望脱敏后保留", got)
		}
		if strings.Contains(got, "sk-abcdefghijklmnop1234567890") {
			t.Errorf("派生文案仍在回显密钥：%q", got)
		}
	})

	t.Run("短密钥形状未被替换则整条被拒", func(t *testing.T) {
		// 8~15 位的 sk- 前缀不会被 sanitize 换掉，但归一化后的安全判据仍会把它当密钥形状，
		// 故整条候选被弃（回退通用文案）。这条边界是「脱敏阈值 16」与「安全判据阈值 8」的差。
		if got := derive("invalid key sk-abcdefgh", ""); got != "" {
			t.Errorf("含未被脱敏的密钥形状应被拒，得到 %q", got)
		}
	})

	t.Run("超长文案截断到上限", func(t *testing.T) {
		long := strings.Repeat("alpha ", 80)
		got := derive(long, "")
		if got == "" {
			t.Fatal("长文案应被截断而非被拒")
		}
		// 词元里不含被禁标签，剥除后仍是纯文本，故只断言长度上界（按字符数）。
		if runes := []rune(got); len(runes) > maxClientErrorMessageChars {
			t.Errorf("截断后长度 = %d，超过上限 %d", len(runes), maxClientErrorMessageChars)
		}
	})
}

// TestSanitizeErrorTextForDetail 钉住凭据脱敏。
func TestSanitizeErrorTextForDetail(t *testing.T) {
	cases := []struct {
		input   string
		want    string
		notWant string
	}{
		{input: "Authorization: Bearer abc.def-ghi", want: "Bearer [REDACTED]", notWant: "abc.def-ghi"},
		{input: "key sk-abcdefghijklmnop1234567890", want: "[REDACTED_KEY]", notWant: "sk-abcdefghijklmnop1234567890"},
		{input: "token=supersecretvalue", want: "token:***", notWant: "supersecretvalue"},
		{input: "read /etc/app.env failed", want: "[PATH]", notWant: "/etc/app.env"},
	}
	for _, testCase := range cases {
		got := sanitizeErrorTextForDetail(testCase.input)
		if !strings.Contains(got, testCase.want) {
			t.Errorf("脱敏 %q = %q，期望含 %q", testCase.input, got, testCase.want)
		}
		if strings.Contains(got, testCase.notWant) {
			t.Errorf("脱敏 %q 仍含 %q", testCase.input, testCase.notWant)
		}
	}
}
