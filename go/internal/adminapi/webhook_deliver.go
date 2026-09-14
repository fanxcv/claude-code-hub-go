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

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是推送投递面：把一条通知发给一个 webhook 目标，并把「成功/失败 + 耗时」交回调用方。
//
// 唯一真源：
//   - 端点 URL 与签名：src/lib/webhook/notifier.ts（getEndpointUrl / withDingtalkSignature）
//   - 请求体形状：src/lib/webhook/renderers/*（五家各一种信封）
//   - 重试：src/lib/webhook/utils/retry.ts（attempt 上限即 maxRetries，退避 baseDelay*2^(n-1)）
//   - 响应判定：src/lib/webhook/notifier.ts:checkResponse（errcode/code/ok 三族）
//
// **登记进差异白名单的一项（重要）**：Node 的正文由「消息构建器 + 渲染器」两级拼装
// （templates/test-messages.ts 调 circuit-breaker / cost-alert / daily-leaderboard /
// cache-hit-rate-alert 四个 builder，再经五家渲染器成为 markdown / HTML / 卡片 JSON）。
// Go 侧本 lane 只对齐**信封与端点语义**（msgtype / msg_type / chat_id / 自定义模板插值），
// 正文文案是简化版（标题 + 与通知类型相关的几行示例字段）。因此：
//   - 对管理 API 的调用方：无差异（该端点只回 {latencyMs}，失败时才回 Problem）。
//   - 对 webhook 接收端：收到的正文文案与 Node 不同，但渠道、字段名、可解析性一致。
// 若日后要逐字对齐，只需替换本文件的 buildWebhookBody，接口不变。

// webhookSendOptions 一次投递的输入。
type webhookSendOptions struct {
	// NotificationType 取 webhook-notification 的四个枚举值之一（正文与模板变量用）。
	NotificationType string
	// Timezone 是系统时区（正文时间戳用）。
	Timezone string
	// MaxAttempts 是**总尝试次数**（Node 的 withRetry 语义：attempt <= maxRetries）。
	MaxAttempts int
	// Data 是模板变量来源（自定义模板插值用），可为 nil。
	Data json.RawMessage
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

// buildWebhookBody 按渠道产出请求体与附加头（复刻五家渲染器的信封）。
func buildWebhookBody(
	target store.AdminWebhookTarget,
	options webhookSendOptions,
) ([]byte, map[string]string, error) {
	text := webhookTestText(options.NotificationType, options.Timezone)

	switch target.ProviderType {
	case "wechat":
		return webhookBodyOf(map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]any{"content": text},
		})
	case "dingtalk":
		return webhookBodyOf(map[string]any{
			"msgtype": "markdown",
			"markdown": map[string]any{
				"title": webhookTestTitle(options.NotificationType),
				"text":  text,
			},
		})
	case "feishu":
		return webhookBodyOf(map[string]any{
			"msg_type": "interactive",
			"card": map[string]any{
				"schema": "2.0",
				"header": map[string]any{
					"title":    map[string]any{"tag": "plain_text", "content": webhookTestTitle(options.NotificationType)},
					"template": webhookFeishuTemplate(options.NotificationType),
				},
				"body": map[string]any{
					"elements": []any{map[string]any{"tag": "markdown", "content": text}},
				},
			},
		})
	case "telegram":
		if strings.TrimSpace(webhookDeref(target.TelegramChatID)) == "" {
			return nil, nil, errors.New("Telegram Chat ID 不能为空")
		}
		return webhookBodyOf(map[string]any{
			"chat_id":                  strings.TrimSpace(*target.TelegramChatID),
			"text":                     text,
			"parse_mode":               "HTML",
			"disable_web_page_preview": true,
		})
	case "custom":
		body, err := webhookCustomBody(target.CustomTemplate, options)
		if err != nil {
			return nil, nil, err
		}
		return body, webhookCustomHeaders(target.CustomHeaders), nil
	default:
		return nil, nil, fmt.Errorf("不支持的推送渠道: %s", target.ProviderType)
	}
}

// webhookBodyOf 把「对象 -> JSON 字节」与「无附加头」合成 buildWebhookBody 的三返回值。
func webhookBodyOf(body map[string]any) ([]byte, map[string]string, error) {
	payload, err := adminMarshalJSON(body)
	if err != nil {
		return nil, nil, err
	}
	return payload, nil, nil
}

// webhookCustomBody 复刻 CustomRenderer：用模板变量替换 `{{key}}` 后再序列化。
//
// 变量表只实现通用五项（timestamp / timestamp_local / title / level / sections）与四个通知类型的
// 少数直观字段；模板里出现未实现的占位符时**原样保留**（Node 侧会替换成实际值）。
// 这条差异已登记在文件头。
func webhookCustomBody(template json.RawMessage, options webhookSendOptions) ([]byte, error) {
	if len(template) == 0 || string(template) == "null" {
		return nil, errors.New("自定义 Webhook 模板不能为空")
	}
	var decoded any
	if err := json.Unmarshal(template, &decoded); err != nil {
		return nil, errors.New("自定义 Webhook 模板必须是 JSON 对象")
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return nil, errors.New("自定义 Webhook 模板必须是 JSON 对象")
	}
	variables := webhookTemplateVariables(options)
	interpolated := webhookInterpolate(object, variables)
	return adminMarshalJSON(interpolated)
}

// webhookInterpolate 递归替换字符串节点里的占位符。
func webhookInterpolate(value any, variables map[string]string) any {
	switch typed := value.(type) {
	case string:
		result := typed
		for key, replacement := range variables {
			result = strings.ReplaceAll(result, key, replacement)
		}
		return result
	case []any:
		items := make([]any, 0, len(typed))
		for _, item := range typed {
			items = append(items, webhookInterpolate(item, variables))
		}
		return items
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			result[key] = webhookInterpolate(item, variables)
		}
		return result
	default:
		return value
	}
}

// webhookTemplateVariables 产出通用占位符的值（见 webhookCustomBody 的差异说明）。
func webhookTemplateVariables(options webhookSendOptions) map[string]string {
	now := time.Now()
	timezone := options.Timezone
	if timezone == "" {
		timezone = "UTC"
	}
	return map[string]string{
		"{{timestamp}}":       now.UTC().Format(time.RFC3339),
		"{{timestamp_local}}": webhookFormatInZone(now, timezone),
		"{{title}}":           webhookTestTitle(options.NotificationType),
		"{{level}}":           webhookTestLevel(options.NotificationType),
		"{{sections}}":        webhookTestText(options.NotificationType, timezone),
	}
}

// webhookCustomHeaders 取自定义头（Node 侧直接透传 customHeaders）。
func webhookCustomHeaders(raw json.RawMessage) map[string]string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var headers map[string]string
	if err := json.Unmarshal(raw, &headers); err != nil {
		return nil
	}
	return headers
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

// webhookTestTitle 是测试消息标题（简化版文案，见文件头差异说明）。
func webhookTestTitle(notificationType string) string {
	switch notificationType {
	case "circuit_breaker":
		return "熔断告警测试"
	case "daily_leaderboard":
		return "每日排行榜测试"
	case "cost_alert":
		return "成本预警测试"
	case "cache_hit_rate_alert":
		return "缓存命中率异常告警测试"
	default:
		return "通知测试"
	}
}

// webhookTestLevel 对应 StructuredMessage.header.level 的三档（测试消息用 warning/info）。
func webhookTestLevel(notificationType string) string {
	switch notificationType {
	case "circuit_breaker", "cost_alert", "cache_hit_rate_alert":
		return "warning"
	default:
		return "info"
	}
}

// webhookTestText 拼一段与渠道无关的纯文本正文（markdown 与 Telegram 的 HTML 共用）。
func webhookTestText(notificationType, timezone string) string {
	if timezone == "" {
		timezone = "UTC"
	}
	lines := []string{"## " + webhookTestTitle(notificationType), ""}
	switch notificationType {
	case "circuit_breaker":
		lines = append(lines,
			"**供应商**: 测试供应商 (ID 0)",
			"**连续失败**: 3",
			"**最近错误**: Connection timeout (示例错误)",
		)
	case "daily_leaderboard":
		lines = append(lines,
			"**日期**: "+time.Now().Format("2006-01-02"),
			"- **用户A**: 150 请求 / $12.50 / 50000 tokens",
			"- **用户B**: 120 请求 / $10.20 / 40000 tokens",
		)
	case "cost_alert":
		lines = append(lines,
			"**目标**: 测试用户",
			"**当前消费**: $80.00 / $100.00 (阈值 80%)",
		)
	case "cache_hit_rate_alert":
		lines = append(lines,
			"**窗口**: 5m",
			"**测试供应商 / test-model**: 命中率 12% (基线 45%)",
		)
	}
	lines = append(lines, "", webhookFormatInZone(time.Now(), timezone))
	return strings.Join(lines, "\n")
}

// webhookFeishuTemplate 是卡片头部的配色（Node 按 level 取色）。
func webhookFeishuTemplate(notificationType string) string {
	if webhookTestLevel(notificationType) == "warning" {
		return "orange"
	}
	return "blue"
}

// webhookFormatInZone 复刻 formatDateTime（yyyy/MM/dd HH:mm:ss，指定时区）。
func webhookFormatInZone(at time.Time, timezone string) string {
	location, err := time.LoadLocation(timezone)
	if err != nil {
		location = time.UTC
	}
	return at.In(location).Format("2006/01/02 15:04:05")
}

// webhookDeref 读可空字符串（nil 当空串）。
func webhookDeref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
