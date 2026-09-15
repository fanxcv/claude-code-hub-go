package adminapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/notify"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是推送投递面：把一条通知发给一个 webhook 目标，并把「成功/失败 + 耗时」交回调用方。
//
// 唯一真源：
//   - 端点 URL 与签名：src/lib/webhook/notifier.ts（getEndpointUrl / withDingtalkSignature）
//   - **正文**：src/lib/webhook/templates/*.ts（四个构建器 + test-messages）与
//     src/lib/webhook/renderers/*.ts（五家信封与排版）——已移植到 internal/notify 的
//     message.go / render.go，本文件只负责把构建好的消息交给渲染器。
//   - 重试：src/lib/webhook/utils/retry.ts（attempt 上限即 maxRetries，退避 baseDelay*2^(n-1)）
//   - 响应判定：src/lib/webhook/notifier.ts:checkResponse（errcode/code/ok 三族）
//
// 登记差异：正文里 Node 的 emoji（标题图标、奖牌、用量指示灯）在 Go 侧省略，
// 依据仓库指南 §8 的禁 emoji 约定；其余（标题、字段、单位、顺序、名次、截断）逐条对齐。
// 详见 internal/notify/message.go 的文件头。

// webhookSendOptions 一次投递的输入。
type webhookSendOptions struct {
	// NotificationType 取 webhook-notification 的四个枚举值之一（正文与模板变量用）。
	NotificationType string
	// Timezone 是系统时区（正文时间戳用）。
	Timezone string
	// MaxAttempts 是**总尝试次数**（Node 的 withRetry 语义：attempt <= maxRetries）。
	MaxAttempts int
	// Data 是通知数据（Node 的 options.data）：四个构建器据此产出正文，
	// 也是自定义模板 {{...}} 的取值来源；为空表示走「测试推送」的示例数据。
	Data json.RawMessage
	// TemplateOverride 是绑定级模板覆盖（Node 的 options.templateOverride，只在 custom 渠道消费）。
	TemplateOverride json.RawMessage
	// Now 是消息时刻（Node 的 new Date()）；零值表示用真实时钟。
	Now time.Time
}

// now 取消息时刻。
func (o webhookSendOptions) now() time.Time {
	if o.Now.IsZero() {
		return time.Now()
	}
	return o.Now
}

// webhookSendResult 一次投递的结果（形状对应 repository 的 WebhookTestResult）。
type webhookSendResult struct {
	Success   bool   `json:"success"`
	Error     string `json:"error,omitempty"`
	LatencyMS int64  `json:"latencyMs"`
}

// webhookRetryBaseDelay 是重试的基础退避时长（Node 默认 1000ms）。
//
// 声明成变量而不是常量：测试要把它压到 0，否则一次失败投递会把用例拖慢数秒。
var webhookRetryBaseDelay = time.Second

// webhookRequestTimeout 是单次 HTTP 请求的上限。
//
// Node 侧用 undici 的默认超时（较大且不可配），Go 侧给一个有界的 10s：管理面的「测试连通性」
// 不该把请求线程挂住几十秒。**登记进白名单**（与 Node 的超时数值不同）。
const webhookRequestTimeout = 10 * time.Second

// adminSendWebhook 把测试通知投递给目标。
//
// 返回的 result 一定带 latencyMs（含失败路径），调用方据此写 last_test_result，与 Node 的
// testWebhookTargetAction 一致。
func adminSendWebhook(
	ctx context.Context,
	target store.AdminWebhookTarget,
	options webhookSendOptions,
) webhookSendResult {
	started := time.Now()
	attempts := options.MaxAttempts
	if attempts <= 0 {
		attempts = 1
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		lastErr = adminSendWebhookOnce(ctx, target, options)
		if lastErr == nil {
			return webhookSendResult{Success: true, LatencyMS: time.Since(started).Milliseconds()}
		}
		if attempt == attempts {
			break
		}
		delay := webhookRetryBaseDelay * time.Duration(1<<(attempt-1))
		if delay > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(delay):
			}
		}
	}

	return webhookSendResult{
		Success:   false,
		Error:     lastErr.Error(),
		LatencyMS: time.Since(started).Milliseconds(),
	}
}

// adminSendWebhookOnce 投递一次（不重试）。
func adminSendWebhookOnce(
	ctx context.Context,
	target store.AdminWebhookTarget,
	options webhookSendOptions,
) error {
	endpoint, err := webhookEndpointURL(target)
	if err != nil {
		return err
	}
	body, headers, err := buildWebhookBody(target, options)
	if err != nil {
		return err
	}

	requestCtx, cancel := context.WithTimeout(ctx, webhookRequestTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint,
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		request.Header.Set(name, value)
	}

	response, err := webhookHTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()

	payload, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := fmt.Sprintf("HTTP %d: %s", response.StatusCode, http.StatusText(response.StatusCode))
		if len(bytes.TrimSpace(payload)) > 0 {
			message += " - " + string(payload)
		}
		return errors.New(message)
	}
	return checkWebhookResponse(target.ProviderType, payload)
}

// webhookHTTPClient 是投递用的 HTTP 客户端。
//
// 显式禁用环境代理：Node 侧走 undici 且只认目标自带的 proxyUrl（代理配置未移植时不得让
// 环境变量悄悄改变投递路径）。
var webhookHTTPClient = &http.Client{
	Transport: &http.Transport{Proxy: nil},
}

// webhookEndpointURL 复刻 notifier.getEndpointUrl。
func webhookEndpointURL(target store.AdminWebhookTarget) (string, error) {
	switch target.ProviderType {
	case "telegram":
		token := strings.TrimSpace(webhookDeref(target.TelegramBotToken))
		if token == "" {
			return "", errors.New("Telegram Bot Token 不能为空")
		}
		return "https://api.telegram.org/bot" + token + "/sendMessage", nil
	case "dingtalk":
		raw := strings.TrimSpace(webhookDeref(target.WebhookURL))
		if raw == "" {
			return "", errors.New("Webhook URL 不能为空")
		}
		return webhookWithDingtalkSignature(raw, webhookDeref(target.DingtalkSecret))
	case "wechat", "feishu", "custom":
		raw := strings.TrimSpace(webhookDeref(target.WebhookURL))
		if raw == "" {
			return "", errors.New("Webhook URL 不能为空")
		}
		return raw, nil
	default:
		return "", fmt.Errorf("不支持的推送渠道: %s", target.ProviderType)
	}
}

// webhookWithDingtalkSignature 复刻 withDingtalkSignature：secret 非空时补 timestamp 与 sign。
func webhookWithDingtalkSignature(rawURL, secret string) (string, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return rawURL, nil
	}
	timestamp := time.Now().UnixMilli()
	toSign := strconv.FormatInt(timestamp, 10) + "\n" + secret
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(toSign))
	sign := base64.StdEncoding.EncodeToString(mac.Sum(nil))

	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	query := parsed.Query()
	query.Set("timestamp", strconv.FormatInt(timestamp, 10))
	query.Set("sign", sign)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// buildWebhookBody 把一条通知渲染成请求体与附加头。
//
// 两级拼装与 Node 同形：先把数据建成结构化消息（notify.BuildDeliveryMessage——
// data 为空时走测试消息），再交五家渲染器（notify.RenderWebhook）。
// 渲染器不碰端点与签名，那两件在本文件的后半段。
func buildWebhookBody(
	target store.AdminWebhookTarget,
	options webhookSendOptions,
) ([]byte, map[string]string, error) {
	message, err := notify.BuildDeliveryMessage(
		options.NotificationType,
		options.Data,
		options.Timezone,
		options.now(),
	)
	if err != nil {
		return nil, nil, err
	}

	rendered, err := notify.RenderWebhook(
		target.ProviderType,
		message,
		notify.WebhookRenderConfig{
			CustomTemplate: target.CustomTemplate,
			CustomHeaders:  target.CustomHeaders,
			TelegramChatID: webhookDeref(target.TelegramChatID),
		},
		notify.WebhookRenderOptions{
			NotificationType: options.NotificationType,
			Data:             options.Data,
			TemplateOverride: options.TemplateOverride,
			Timezone:         options.Timezone,
		},
	)
	if err != nil {
		return nil, nil, err
	}
	return rendered.Body, rendered.Headers, nil
}

// checkWebhookResponse 复刻 checkResponse 的三族判定（custom 只看 2xx）。
func checkWebhookResponse(providerType string, payload []byte) error {
	switch providerType {
	case "custom":
		return nil
	case "wechat", "dingtalk":
		var decoded struct {
			ErrCode *json.Number `json:"errcode"`
			ErrMsg  string       `json:"errmsg"`
		}
		if err := json.Unmarshal(payload, &decoded); err != nil {
			return fmt.Errorf("响应不是合法 JSON: %w", err)
		}
		if decoded.ErrCode != nil && decoded.ErrCode.String() == "0" {
			return nil
		}
		code := "unknown"
		if decoded.ErrCode != nil {
			code = decoded.ErrCode.String()
		}
		if providerType == "wechat" {
			return fmt.Errorf("WeChat API Error %s: %s", code, decoded.ErrMsg)
		}
		return fmt.Errorf("DingTalk API Error %s: %s", code, decoded.ErrMsg)
	case "feishu":
		var decoded struct {
			Code *json.Number `json:"code"`
			Msg  string       `json:"msg"`
		}
		if err := json.Unmarshal(payload, &decoded); err != nil {
			return fmt.Errorf("响应不是合法 JSON: %w", err)
		}
		if decoded.Code != nil && decoded.Code.String() == "0" {
			return nil
		}
		code := "unknown"
		if decoded.Code != nil {
			code = decoded.Code.String()
		}
		return fmt.Errorf("Feishu API Error %s: %s", code, decoded.Msg)
	case "telegram":
		var decoded struct {
			OK          bool   `json:"ok"`
			Description string `json:"description"`
		}
		if err := json.Unmarshal(payload, &decoded); err != nil {
			return fmt.Errorf("响应不是合法 JSON: %w", err)
		}
		if decoded.OK {
			return nil
		}
		description := decoded.Description
		if description == "" {
			description = "unknown"
		}
		return fmt.Errorf("Telegram API Error: %s", description)
	default:
		return nil
	}
}

// webhookDeref 读可空字符串（nil 当空串）。
func webhookDeref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
