package adminapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是投递层（webhook_deliver.go）的单元用例：不碰数据库，只用 httptest 当接收端，
// 钉住五家信封、签名、响应判定与重试次数。

// webhookTestServer 起一个接收端，记录最近一次请求体与头，并按配置作答。
type webhookTestServer struct {
	server   *httptest.Server
	requests atomic.Int64
	lastBody atomic.Value // string
	lastAuth atomic.Value // string
}

func newWebhookTestServer(t *testing.T, status int, body string) *webhookTestServer {
	t.Helper()
	harness := &webhookTestServer{}
	harness.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		harness.requests.Add(1)
		payload, _ := io.ReadAll(request.Body)
		harness.lastBody.Store(string(payload))
		harness.lastAuth.Store(request.Header.Get("X-Test-Token"))
		writer.Header().Set("Content-Type", "application/json")
		if status != 0 {
			writer.WriteHeader(status)
		}
		_, _ = writer.Write([]byte(body))
	}))
	t.Cleanup(harness.server.Close)
	return harness
}

func (h *webhookTestServer) last() string {
	value, _ := h.lastBody.Load().(string)
	return value
}

// TestWebhookDeliveryEnvelopes 钉住五家渠道的信封与成功判定。
func TestWebhookDeliveryEnvelopes(t *testing.T) {
	webhookRetryBaseDelay = 0
	t.Cleanup(func() { webhookRetryBaseDelay = 1_000_000_000 })

	cases := []struct {
		name         string
		target       store.AdminWebhookTarget
		responseBody string
		assert       func(t *testing.T, harness *webhookTestServer)
	}{
		{
			name:         "wechat",
			responseBody: `{"errcode":0,"errmsg":"ok"}`,
			assert: func(t *testing.T, harness *webhookTestServer) {
				var body map[string]any
				if err := json.Unmarshal([]byte(harness.last()), &body); err != nil {
					t.Fatalf("请求体不是 JSON: %v", err)
				}
				if body["msgtype"] != "markdown" {
					t.Fatalf("企业微信信封应为 markdown，实际 %v", body["msgtype"])
				}
			},
		},
		{
			name:         "feishu",
			responseBody: `{"code":0,"msg":"ok"}`,
			assert: func(t *testing.T, harness *webhookTestServer) {
				var body map[string]any
				if err := json.Unmarshal([]byte(harness.last()), &body); err != nil {
					t.Fatalf("请求体不是 JSON: %v", err)
				}
				if body["msg_type"] != "interactive" {
					t.Fatalf("飞书信封应为 interactive，实际 %v", body["msg_type"])
				}
			},
		},
		{
			name:         "dingtalk",
			responseBody: `{"errcode":0,"errmsg":"ok"}`,
			assert: func(t *testing.T, harness *webhookTestServer) {
				var body map[string]any
				if err := json.Unmarshal([]byte(harness.last()), &body); err != nil {
					t.Fatalf("请求体不是 JSON: %v", err)
				}
				if body["msgtype"] != "markdown" {
					t.Fatalf("钉钉信封应为 markdown，实际 %v", body["msgtype"])
				}
			},
		},
		{
			name: "custom",
			target: store.AdminWebhookTarget{
				ProviderType:   "custom",
				CustomTemplate: json.RawMessage(`{"text":"{{title}}","level":"{{level}}"}`),
				CustomHeaders:  json.RawMessage(`{"X-Test-Token":"secret-token"}`),
			},
			responseBody: `{}`,
			assert: func(t *testing.T, harness *webhookTestServer) {
				var body map[string]any
				if err := json.Unmarshal([]byte(harness.last()), &body); err != nil {
					t.Fatalf("请求体不是 JSON: %v", err)
				}
				if body["text"] != webhookTestTitle("circuit_breaker") {
					t.Fatalf("模板插值未生效：%v", body["text"])
				}
				if body["level"] != "warning" {
					t.Fatalf("level 变量应为 warning，实际 %v", body["level"])
				}
				if auth, _ := harness.lastAuth.Load().(string); auth != "secret-token" {
					t.Fatalf("自定义头未透传，实际 %q", auth)
				}
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newWebhookTestServer(t, http.StatusOK, testCase.responseBody)
			target := testCase.target
			if target.ProviderType == "" {
				target.ProviderType = testCase.name
			}
			target.WebhookURL = webhookTestStringPtr(harness.server.URL)
			result := adminSendWebhook(t.Context(), target, webhookSendOptions{
				NotificationType: "circuit_breaker",
				Timezone:         "UTC",
				MaxAttempts:      1,
			})
			if !result.Success {
				t.Fatalf("投递应成功，实际失败：%s", result.Error)
			}
			if harness.requests.Load() != 1 {
				t.Fatalf("应只投递一次，实际 %d 次", harness.requests.Load())
			}
			testCase.assert(t, harness)
		})
	}
}

// TestWebhookDeliveryTelegramUsesBotEndpoint 钉住 telegram 的端点拼接与信封。
func TestWebhookDeliveryTelegramUsesBotEndpoint(t *testing.T) {
	webhookRetryBaseDelay = 0
	t.Cleanup(func() { webhookRetryBaseDelay = 1_000_000_000 })

	// telegram 的端点固定指向 api.telegram.org，无法用 httptest 直接接；本用例改验证
	// 「端点拼接」与「缺少 chat id 时报错」，投递路径由其它渠道覆盖。
	_, err := webhookEndpointURL(store.AdminWebhookTarget{
		ProviderType:     "telegram",
		TelegramBotToken: webhookTestStringPtr("123:abc"),
	})
	if err != nil {
		t.Fatalf("拼接 telegram 端点不应报错：%v", err)
	}
	endpoint, _ := webhookEndpointURL(store.AdminWebhookTarget{
		ProviderType:     "telegram",
		TelegramBotToken: webhookTestStringPtr("123:abc"),
	})
	if endpoint != "https://api.telegram.org/bot123:abc/sendMessage" {
		t.Fatalf("telegram 端点不符合 Node 的形状：%s", endpoint)
	}
	if _, err := webhookEndpointURL(store.AdminWebhookTarget{ProviderType: "telegram"}); err == nil {
		t.Fatal("缺少 Bot Token 应报错")
	}
	if _, _, err := buildWebhookBody(store.AdminWebhookTarget{ProviderType: "telegram"},
		webhookSendOptions{NotificationType: "circuit_breaker"}); err == nil {
		t.Fatal("缺少 Chat ID 应报错")
	}
}

// TestWebhookDeliveryDingtalkSignature 钉住钉钉签名参数（timestamp + HMAC-SHA256 的 base64）。
func TestWebhookDeliveryDingtalkSignature(t *testing.T) {
	endpoint, err := webhookEndpointURL(store.AdminWebhookTarget{
		ProviderType:   "dingtalk",
		WebhookURL:     webhookTestStringPtr("https://oapi.dingtalk.com/robot/send?access_token=tok"),
		DingtalkSecret: webhookTestStringPtr("SEC-test"),
	})
	if err != nil {
		t.Fatalf("签名拼接失败：%v", err)
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		t.Fatalf("签名后的 URL 非法：%v", err)
	}
	query := parsed.Query()
	if query.Get("timestamp") == "" || query.Get("sign") == "" {
		t.Fatalf("缺少签名参数：%s", endpoint)
	}
	if query.Get("access_token") != "tok" {
		t.Fatalf("原有查询参数应保留：%s", endpoint)
	}
	// 不带 secret 时 URL 原样返回。
	unsigned, _ := webhookEndpointURL(store.AdminWebhookTarget{
		ProviderType: "dingtalk",
		WebhookURL:   webhookTestStringPtr("https://oapi.dingtalk.com/robot/send?access_token=tok"),
	})
	if strings.Contains(unsigned, "sign=") {
		t.Fatalf("无 secret 时不应带签名：%s", unsigned)
	}
}

// TestWebhookDeliveryFailureAndRetry 钉住失败判定与重试次数（Node 的 maxRetries 即总尝试数）。
func TestWebhookDeliveryFailureAndRetry(t *testing.T) {
	webhookRetryBaseDelay = 0
	t.Cleanup(func() { webhookRetryBaseDelay = 1_000_000_000 })

	t.Run("http_500_重试三次后失败", func(t *testing.T) {
		harness := newWebhookTestServer(t, http.StatusInternalServerError, `{"error":"boom"}`)
		result := adminSendWebhook(t.Context(), store.AdminWebhookTarget{
			ProviderType:   "custom",
			WebhookURL:     webhookTestStringPtr(harness.server.URL),
			CustomTemplate: json.RawMessage(`{"text":"hi"}`),
		}, webhookSendOptions{NotificationType: "cost_alert", MaxAttempts: 3})
		if result.Success {
			t.Fatal("HTTP 500 不应判成功")
		}
		if harness.requests.Load() != 3 {
			t.Fatalf("MaxAttempts=3 应发起 3 次，实际 %d 次", harness.requests.Load())
		}
		if !strings.Contains(result.Error, "HTTP 500") {
			t.Fatalf("错误文案应含 HTTP 状态：%s", result.Error)
		}
	})

	t.Run("业务错误码判失败", func(t *testing.T) {
		harness := newWebhookTestServer(t, http.StatusOK, `{"errcode":40001,"errmsg":"invalid token"}`)
		result := adminSendWebhook(t.Context(), store.AdminWebhookTarget{
			ProviderType: "wechat",
			WebhookURL:   webhookTestStringPtr(harness.server.URL),
		}, webhookSendOptions{NotificationType: "cost_alert", MaxAttempts: 1})
		if result.Success {
			t.Fatal("errcode != 0 不应判成功")
		}
		if !strings.Contains(result.Error, "40001") {
			t.Fatalf("错误文案应含业务码：%s", result.Error)
		}
	})

	t.Run("自定义渠道只看 2xx", func(t *testing.T) {
		harness := newWebhookTestServer(t, http.StatusOK, `not-json-at-all`)
		result := adminSendWebhook(t.Context(), store.AdminWebhookTarget{
			ProviderType:   "custom",
			WebhookURL:     webhookTestStringPtr(harness.server.URL),
			CustomTemplate: json.RawMessage(`{"text":"hi"}`),
		}, webhookSendOptions{NotificationType: "cost_alert", MaxAttempts: 1})
		if !result.Success {
			t.Fatalf("自定义渠道 2xx 即成功，实际失败：%s", result.Error)
		}
	})
}

func webhookTestStringPtr(value string) *string { return &value }
