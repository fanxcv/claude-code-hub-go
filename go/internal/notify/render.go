package notify

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
)

// 本文件是**五家渠道的渲染器**，逐条对齐 Node 的 src/lib/webhook/renderers/*.ts。
//
// 分工与 Node 相同：渲染器只回答「这条消息在本渠道长什么样」（正文 + 附加头），
// 端点 URL、签名与响应判定在投递层（internal/adminapi 的 webhook_deliver.go）。
//
// 用 convert.Value 而不是 map[string]any 序列化：Node 的 JSON.stringify 保留属性书写顺序，
// 而 encoding/json 会把键排序——信封的键序虽然对聊天平台无影响，但「逐字对拍」是验收口径，
// 且自定义模板的键序对接收端可见。convert.Value 的序列化语义与 JS 一致（不转义 < > &）。

// WebhookRenderConfig 是目标自带的渲染输入（Node 的 WebhookTargetConfig 的相关子集）。
type WebhookRenderConfig struct {
	// CustomTemplate 是自定义渠道的目标模板（custom 渠道必填，否则与 Node 一样报错）。
	CustomTemplate json.RawMessage
	// CustomHeaders 是自定义渠道的附加请求头。
	CustomHeaders json.RawMessage
	// TelegramChatID 是 Telegram 的会话 id（telegram 渠道必填）。
	TelegramChatID string
}

// WebhookRenderOptions 是一次渲染的可选项（Node 的 WebhookSendOptions）。
type WebhookRenderOptions struct {
	// NotificationType 决定模板变量的类型段（Node 的 notificationType）。
	NotificationType string
	// Data 是真实投递数据；自定义模板的 {{...}} 从这里取值。
	Data json.RawMessage
	// TemplateOverride 是绑定级模板覆盖（Node 只在 custom 渠道消费它）。
	TemplateOverride json.RawMessage
	// Timezone 是文案时间戳用的时区名。
	Timezone string
}

// RenderedWebhook 是渲染产物（Node 的 WebhookPayload）。
type RenderedWebhook struct {
	Body    []byte
	Headers map[string]string
}

// RenderWebhook 按渠道把消息渲染成请求体。
func RenderWebhook(
	providerType string,
	message StructuredMessage,
	config WebhookRenderConfig,
	options WebhookRenderOptions,
) (RenderedWebhook, error) {
	switch providerType {
	case "wechat":
		return webhookPayloadOf(buildWeChatBody(message, options.Timezone)), nil
	case "dingtalk":
		return webhookPayloadOf(buildDingTalkBody(message, options.Timezone)), nil
	case "feishu":
		return webhookPayloadOf(buildFeishuBody(message, options.Timezone)), nil
	case "telegram":
		chatID := strings.TrimSpace(config.TelegramChatID)
		if chatID == "" {
			return RenderedWebhook{}, errors.New("Telegram Chat ID 不能为空")
		}
		return webhookPayloadOf(buildTelegramBody(chatID, message, options.Timezone)), nil
	case "custom":
		return buildCustomBody(config, options, message)
	default:
		return RenderedWebhook{}, errors.New("不支持的推送渠道: " + providerType)
	}
}

// webhookPayloadOf 把一个保序 JSON 值包成渲染产物（渲染器都不带附加头）。
func webhookPayloadOf(body *convert.Value) RenderedWebhook {
	return RenderedWebhook{Body: []byte(body.MarshalCompact())}
}

// buildWeChatBody 复刻 WeChatRenderer.render（renderers/wechat.ts:10）。
func buildWeChatBody(message StructuredMessage, timezone string) *convert.Value {
	lines := []string{}
	lines = append(lines, wechatHeader(message))
	lines = append(lines, "")

	for _, section := range message.Sections {
		lines = append(lines, wechatSection(section)...)
		lines = append(lines, "")
	}

	if message.Footer != nil {
		lines = append(lines, "---")
		for _, section := range message.Footer {
			lines = append(lines, wechatSection(section)...)
		}
		lines = append(lines, "")
	}

	lines = append(lines, FormatWebhookTimestamp(message.Timestamp, timezone))

	return convert.NewObject().Set("msgtype", convert.NewString("markdown")).
		Set("markdown", convert.NewObject().
			Set("content", convert.NewString(strings.Join(lines, "\n"))))
}

// wechatHeader 复刻 renderHeader：`## ${displayIcon} ${title}`。
//
// Node 的 displayIcon 是「消息自带图标，否则按 level 取 emoji」。Go 侧两个来源都不放
// emoji（见 message.go 文件头），故无图标时直接写标题——不留 Node 那个多余的双空格。
func wechatHeader(message StructuredMessage) string {
	if message.Header.Icon != "" {
		return "## " + message.Header.Icon + " " + message.Header.Title
	}
	return "## " + message.Header.Title
}

func wechatSection(section Section) []string {
	lines := []string{}
	if section.Title != "" {
		lines = append(lines, "**"+section.Title+"**")
	}
	for _, content := range section.Content {
		lines = append(lines, wechatContent(content)...)
	}
	return lines
}

func wechatContent(content SectionContent) []string {
	switch content.Kind {
	case ContentText:
		return []string{content.Value}
	case ContentQuote:
		return []string{"> " + content.Value}
	case ContentFields:
		lines := make([]string, 0, len(content.Items))
		for _, item := range content.Items {
			lines = append(lines, item.Label+": "+item.Value)
		}
		return lines
	case ContentList:
		// Node 的 renderList 每项后面补一个空行（ListItem 之间因此是空行分隔）。
		lines := make([]string, 0, len(content.ListItems)*2)
		for _, item := range content.ListItems {
			prefix := ""
			if item.Icon != "" {
				prefix = item.Icon + " "
			}
			line := prefix + "**" + item.Primary + "**"
			if item.Secondary != "" {
				line += "\n" + item.Secondary
			}
			lines = append(lines, line, "")
		}
		return lines
	case ContentDivider:
		return []string{"---"}
	default:
		return nil
	}
}

// buildDingTalkBody 复刻 DingTalkRenderer.render（renderers/dingtalk.ts:11）。
func buildDingTalkBody(message StructuredMessage, timezone string) *convert.Value {
	text := strings.TrimSpace(dingtalkMarkdown(message, timezone))
	return convert.NewObject().Set("msgtype", convert.NewString("markdown")).
		Set("markdown", convert.NewObject().
			Set("title", convert.NewString(escapeDingTalk(message.Header.Title))).
			Set("text", convert.NewString(text)))
}

func dingtalkMarkdown(message StructuredMessage, timezone string) string {
	lines := []string{}
	lines = append(lines, "### "+escapeDingTalk(message.Header.Title))
	lines = append(lines, "")

	for _, section := range message.Sections {
		lines = append(lines, dingtalkSection(section)...)
		lines = append(lines, "")
	}

	if message.Footer != nil {
		lines = append(lines, "---")
		for _, section := range message.Footer {
			lines = append(lines, dingtalkSection(section)...)
		}
		lines = append(lines, "")
	}

	lines = append(lines, FormatWebhookTimestamp(message.Timestamp, timezone))
	return strings.Join(lines, "\n")
}

func dingtalkSection(section Section) []string {
	lines := []string{}
	if section.Title != "" {
		lines = append(lines, "**"+escapeDingTalk(section.Title)+"**")
	}
	for _, content := range section.Content {
		lines = append(lines, dingtalkContent(content)...)
	}
	return lines
}

func dingtalkContent(content SectionContent) []string {
	switch content.Kind {
	case ContentText:
		return []string{escapeDingTalk(content.Value)}
	case ContentQuote:
		return []string{"> " + escapeDingTalk(content.Value)}
	case ContentFields:
		lines := make([]string, 0, len(content.Items))
		for _, item := range content.Items {
			lines = append(lines, "- "+escapeDingTalk(item.Label)+": "+escapeDingTalk(item.Value))
		}
		return lines
	case ContentList:
		lines := make([]string, 0, len(content.ListItems))
		for index, item := range content.ListItems {
			prefix := "-"
			if content.Style == ListOrdered {
				prefix = strconv.Itoa(index+1) + "."
			}
			lines = append(lines, prefix+" **"+escapeDingTalk(item.Primary)+"**")
			if item.Secondary != "" {
				lines = append(lines, "  "+escapeDingTalk(item.Secondary))
			}
		}
		return lines
	case ContentDivider:
		return []string{"---"}
	default:
		return nil
	}
}

// escapeDingTalk 复刻 DingTalk 的 escapeText（只转义三个字符）。
func escapeDingTalk(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	return replacer.Replace(value)
}

// buildFeishuBody 复刻 FeishuCardRenderer.render（renderers/feishu.ts:15）。
func buildFeishuBody(message StructuredMessage, timezone string) *convert.Value {
	elements := []*convert.Value{}
	for _, section := range message.Sections {
		elements = append(elements, feishuSection(section)...)
	}

	if message.Footer != nil {
		elements = append(elements, convert.NewObject().Set("tag", convert.NewString("hr")))
		for _, section := range message.Footer {
			elements = append(elements, feishuSection(section)...)
		}
	}

	elements = append(elements, convert.NewObject().
		Set("tag", convert.NewString("markdown")).
		Set("content", convert.NewString(FormatWebhookTimestamp(message.Timestamp, timezone))).
		Set("text_size", convert.NewString("notation")))

	displayTitle := message.Header.Title
	if message.Header.Icon != "" {
		displayTitle = message.Header.Icon + " " + message.Header.Title
	}

	card := convert.NewObject().
		Set("schema", convert.NewString("2.0")).
		Set("header", convert.NewObject().
			Set("title", convert.NewObject().
				Set("tag", convert.NewString("plain_text")).
				Set("content", convert.NewString(displayTitle))).
			Set("template", convert.NewString(feishuTemplate(message.Header.Level)))).
		Set("body", convert.NewObject().Set("elements", convert.NewArray(elements...)))

	return convert.NewObject().
		Set("msg_type", convert.NewString("interactive")).
		Set("card", card)
}

// feishuTemplate 复刻 levelToTemplate：error→red、warning→orange、其余→blue。
func feishuTemplate(level MessageLevel) string {
	switch level {
	case LevelError:
		return "red"
	case LevelWarning:
		return "orange"
	default:
		return "blue"
	}
}

func feishuSection(section Section) []*convert.Value {
	elements := []*convert.Value{}
	if section.Title != "" {
		elements = append(elements, markdownElement("**"+section.Title+"**"))
	}
	for _, content := range section.Content {
		elements = append(elements, feishuContent(content)...)
	}
	return elements
}

func feishuContent(content SectionContent) []*convert.Value {
	switch content.Kind {
	case ContentText:
		return []*convert.Value{markdownElement(content.Value)}
	case ContentQuote:
		return []*convert.Value{markdownElement("> " + content.Value)}
	case ContentFields:
		return feishuFields(content.Items)
	case ContentList:
		// Node 的 renderList 忽略 style，逐项按「图标 + 粗体主行 + 次行」拼一段 markdown，
		// 项间用空行分隔。
		lines := make([]string, 0, len(content.ListItems))
		for _, item := range content.ListItems {
			prefix := ""
			if item.Icon != "" {
				prefix = item.Icon + " "
			}
			line := prefix + "**" + item.Primary + "**"
			if item.Secondary != "" {
				line += "\n" + item.Secondary
			}
			lines = append(lines, line)
		}
		return []*convert.Value{markdownElement(strings.Join(lines, "\n\n"))}
	case ContentDivider:
		return []*convert.Value{convert.NewObject().Set("tag", convert.NewString("hr"))}
	default:
		return nil
	}
}

// markdownElement 是一个 markdown 元素。
func markdownElement(content string) *convert.Value {
	return convert.NewObject().
		Set("tag", convert.NewString("markdown")).
		Set("content", convert.NewString(content))
}

// feishuFields 复刻 renderFields：每两个字段一行（column_set + column）。
func feishuFields(items []FieldItem) []*convert.Value {
	columns := make([]*convert.Value, 0, len(items))
	for _, item := range items {
		columns = append(columns, convert.NewObject().
			Set("tag", convert.NewString("column")).
			Set("width", convert.NewString("weighted")).
			Set("weight", convert.NewNumberInt(1)).
			Set("elements", convert.NewArray(
				markdownElement("**"+item.Label+"**\n"+item.Value),
			)))
	}

	rows := []*convert.Value{}
	for index := 0; index < len(columns); index += 2 {
		upper := index + 2
		if upper > len(columns) {
			upper = len(columns)
		}
		rows = append(rows, convert.NewObject().
			Set("tag", convert.NewString("column_set")).
			Set("flex_mode", convert.NewString("bisect")).
			Set("columns", convert.NewArray(columns[index:upper]...)))
	}
	return rows
}

// buildTelegramBody 复刻 TelegramRenderer.render（renderers/telegram.ts:10）。
func buildTelegramBody(chatID string, message StructuredMessage, timezone string) *convert.Value {
	html := strings.TrimSpace(telegramHTML(message, timezone))
	return convert.NewObject().
		Set("chat_id", convert.NewString(chatID)).
		Set("text", convert.NewString(html)).
		Set("parse_mode", convert.NewString("HTML")).
		Set("disable_web_page_preview", convert.NewBool(true))
}

func telegramHTML(message StructuredMessage, timezone string) string {
	lines := []string{}
	lines = append(lines, "<b>"+escapeTelegram(message.Header.Title)+"</b>")
	lines = append(lines, "")

	for _, section := range message.Sections {
		lines = append(lines, telegramSection(section)...)
		lines = append(lines, "")
	}

	if message.Footer != nil {
		lines = append(lines, "---")
		for _, section := range message.Footer {
			lines = append(lines, telegramSection(section)...)
		}
		lines = append(lines, "")
	}

	lines = append(lines, escapeTelegram(FormatWebhookTimestamp(message.Timestamp, timezone)))
	return strings.Join(lines, "\n")
}

func telegramSection(section Section) []string {
	lines := []string{}
	if section.Title != "" {
		lines = append(lines, "<b>"+escapeTelegram(section.Title)+"</b>")
	}
	for _, content := range section.Content {
		lines = append(lines, telegramContent(content)...)
	}
	return lines
}

func telegramContent(content SectionContent) []string {
	switch content.Kind {
	case ContentText:
		return []string{escapeTelegram(content.Value)}
	case ContentQuote:
		return []string{"&gt; " + escapeTelegram(content.Value)}
	case ContentFields:
		lines := make([]string, 0, len(content.Items))
		for _, item := range content.Items {
			lines = append(lines, "<b>"+escapeTelegram(item.Label)+"</b>: "+
				escapeTelegram(item.Value))
		}
		return lines
	case ContentList:
		lines := make([]string, 0, len(content.ListItems))
		for index, item := range content.ListItems {
			prefix := "-"
			if content.Style == ListOrdered {
				prefix = strconv.Itoa(index+1) + "."
			}
			lines = append(lines, prefix+" <b>"+escapeTelegram(item.Primary)+"</b>")
			if item.Secondary != "" {
				lines = append(lines, "  "+escapeTelegram(item.Secondary))
			}
		}
		return lines
	case ContentDivider:
		return []string{"---"}
	default:
		return nil
	}
}

// escapeTelegram 复刻 Telegram 的 escapeHtml（比钉钉多转义双引号）。
func escapeTelegram(value string) string {
	replacer := strings.NewReplacer(
		"&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;",
	)
	return replacer.Replace(value)
}

// buildCustomBody 复刻 CustomRenderer.render（renderers/custom.ts:16）。
//
// 与 Node 同序的两道判定：先要求目标自带模板存在（Node 在构造渲染器时就抛
// 「自定义 Webhook 模板不能为空」），再用 options.templateOverride 覆盖它。
func buildCustomBody(
	config WebhookRenderConfig,
	options WebhookRenderOptions,
	message StructuredMessage,
) (RenderedWebhook, error) {
	targetTemplate, err := jsonObject(config.CustomTemplate)
	if err != nil || targetTemplate == nil {
		return RenderedWebhook{}, errors.New("自定义 Webhook 模板不能为空")
	}

	template := targetTemplate
	if len(options.TemplateOverride) > 0 && string(options.TemplateOverride) != "null" {
		override, overrideErr := jsonObject(options.TemplateOverride)
		if overrideErr != nil {
			return RenderedWebhook{}, errors.New("自定义 Webhook 模板必须是 JSON 对象")
		}
		if override != nil {
			template = override
		}
	}

	variables := TemplateVariables(message, options)
	interpolated := interpolateValue(template, variables)
	return RenderedWebhook{
		Body:    []byte(interpolated.MarshalCompact()),
		Headers: customHeadersOf(config.CustomHeaders),
	}, nil
}

// jsonObject 把 JSON 字节解析成保序对象；非对象（数组、标量）返回 nil。
func jsonObject(raw json.RawMessage) (*convert.Value, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	value, err := convert.ParseJSON(raw)
	if err != nil {
		return nil, err
	}
	if !value.IsObject() {
		return nil, nil
	}
	return value, nil
}

// interpolateValue 递归替换字符串节点里的占位符（Node 的 interpolate 同语义）。
func interpolateValue(value *convert.Value, variables map[string]string) *convert.Value {
	switch {
	case value == nil:
		return convert.NewNull()
	case value.IsString():
		text, _ := value.String()
		return convert.NewString(interpolateString(text, variables))
	case value.IsArray():
		items := make([]*convert.Value, 0, len(value.Items()))
		for _, item := range value.Items() {
			items = append(items, interpolateValue(item, variables))
		}
		return convert.NewArray(items...)
	case value.IsObject():
		result := convert.NewObject()
		for _, member := range value.Members() {
			result.Set(member.Key, interpolateValue(member.Value, variables))
		}
		return result
	default:
		return value
	}
}

// interpolateString 逐个占位符做替换（Node 用 replaceAll，故顺序无关）。
func interpolateString(template string, variables map[string]string) string {
	result := template
	for key, value := range variables {
		result = strings.ReplaceAll(result, key, value)
	}
	return result
}

// customHeadersOf 取自定义头（Node 直接透传 customHeaders）。
func customHeadersOf(raw json.RawMessage) map[string]string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var headers map[string]string
	if err := json.Unmarshal(raw, &headers); err != nil {
		return nil
	}
	return headers
}

// TemplateVariables 复刻 buildTemplateVariables（templates/placeholders.ts:158）。
//
// 键名带花括号（Node 的键就是 `{{title}}` 这种形态），替换是纯字符串替换。
// 类型段一律容错：数据缺字段给空串（或 Node 写死的默认值），不让模板渲染阻塞发送。
func TemplateVariables(message StructuredMessage, options WebhookRenderOptions) map[string]string {
	values := map[string]string{
		"{{timestamp}}":       isoMillis(message.Timestamp),
		"{{timestamp_local}}": FormatWebhookTimestamp(message.Timestamp, options.Timezone),
		"{{title}}":           message.Header.Title,
		"{{level}}":           string(message.Header.Level),
		"{{sections}}":        RenderMessageSections(message),
	}

	switch options.NotificationType {
	case "circuit_breaker":
		var data CircuitBreakerAlertData
		if len(options.Data) > 0 {
			_ = json.Unmarshal(options.Data, &data)
		}
		source := data.IncidentSource
		if source == "" {
			source = IncidentSourceProvider
		}
		endpointID := ""
		if data.EndpointID != nil {
			endpointID = strconv.FormatInt(*data.EndpointID, 10)
		}
		values["{{provider_name}}"] = data.ProviderName
		values["{{provider_id}}"] = optionalID(data.ProviderID)
		values["{{failure_count}}"] = optionalCount(data.FailureCount)
		values["{{retry_at}}"] = data.RetryAt
		values["{{last_error}}"] = data.LastError
		values["{{incident_source}}"] = source
		values["{{endpoint_id}}"] = endpointID
		values["{{endpoint_url}}"] = data.EndpointURL
	case "daily_leaderboard":
		var data DailyLeaderboardData
		if len(options.Data) > 0 {
			_ = json.Unmarshal(options.Data, &data)
		}
		values["{{date}}"] = data.Date
		values["{{entries_json}}"] = jsonStringify(data.Entries)
		values["{{total_requests}}"] = jsNumberString(data.TotalRequests)
		values["{{total_cost}}"] = jsNumberString(data.TotalCost)
	case "cost_alert":
		var data CostAlertData
		if len(options.Data) > 0 {
			_ = json.Unmarshal(options.Data, &data)
		}
		values["{{target_type}}"] = data.TargetType
		values["{{target_name}}"] = data.TargetName
		values["{{current_cost}}"] = jsNumberString(data.CurrentCost)
		values["{{quota_limit}}"] = jsNumberString(data.QuotaLimit)
		values["{{usage_percent}}"] = usagePercentOf(data)
	case "cache_hit_rate_alert":
		var data CacheHitRateAlertData
		if len(options.Data) > 0 {
			_ = json.Unmarshal(options.Data, &data)
		}
		values["{{window_mode}}"] = data.Window.Mode
		values["{{window_start}}"] = data.Window.StartTime
		values["{{window_end}}"] = data.Window.EndTime
		values["{{anomaly_count}}"] = strconv.Itoa(len(data.Anomalies))
		values["{{suppressed_count}}"] = strconv.Itoa(data.SuppressedCount)
		values["{{anomalies_json}}"] = jsonStringify(data.Anomalies)
		values["{{abs_min}}"] = jsNumberString(data.Settings.AbsMin)
		values["{{drop_rel}}"] = jsNumberString(data.Settings.DropRel)
		values["{{drop_abs}}"] = jsNumberString(data.Settings.DropAbs)
		values["{{cooldown_minutes}}"] = strconv.Itoa(data.Settings.CooldownMinutes)
		values["{{top_n}}"] = strconv.Itoa(data.Settings.TopN)
		values["{{generated_at}}"] = data.GeneratedAt
	}

	return values
}

// usagePercentOf 复刻 buildUsagePercent（placeholders.ts:216）：配额为 0 或缺字段给空串。
func usagePercentOf(data CostAlertData) string {
	if data.QuotaLimit == 0 {
		return ""
	}
	percent := (data.CurrentCost / data.QuotaLimit) * 100
	return fixed1(percent)
}

// optionalID / optionalCount 复刻 Node 的「undefined 给空串，否则 String(...)」。
//
// 与 Node 的差别：Node 用 `!== undefined` 判空，Go 的数字零值与「缺字段」不可分，
// 故这里把 0 也当缺省（熔断告警里 providerId 与 failureCount 为 0 都是异常数据，
// 而测试消息恰好用 0——Node 在测试消息里给的是 0 并会渲染成 "0"，此处会渲染成空串）。
func optionalID(value int64) string {
	if value == 0 {
		return ""
	}
	return strconv.FormatInt(value, 10)
}

func optionalCount(value int64) string {
	if value == 0 {
		return ""
	}
	return strconv.FormatInt(value, 10)
}

// jsNumberString 复刻 JS 的 String(Number)：最短往返表示。
func jsNumberString(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

// jsonStringify 复刻 Node 的 safeJsonStringify：序列化失败给 "[]"，且不转义 HTML 字符
// （JS 的 JSON.stringify 不转义 < > &，而 Go 的 encoding/json 默认会转）。
func jsonStringify(value any) string {
	buffer := &bytes.Buffer{}
	encoder := json.NewEncoder(buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "[]"
	}
	return strings.TrimRight(buffer.String(), "\n")
}

// RenderMessageSections 复刻 renderMessageSections（placeholders.ts:239）：
// `{{sections}}` 的纯文本形态，与任何渠道渲染器都不同（这里没有粗体、没有渠道包装）。
func RenderMessageSections(message StructuredMessage) string {
	lines := []string{}
	for _, section := range message.Sections {
		lines = append(lines, renderPlainSection(section)...)
		lines = append(lines, "")
	}

	if message.Footer != nil {
		lines = append(lines, "---")
		for _, section := range message.Footer {
			lines = append(lines, renderPlainSection(section)...)
		}
		lines = append(lines, "")
	}

	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func renderPlainSection(section Section) []string {
	lines := []string{}
	if section.Title != "" {
		lines = append(lines, section.Title)
	}
	for _, content := range section.Content {
		lines = append(lines, renderPlainContent(content)...)
	}
	return lines
}

func renderPlainContent(content SectionContent) []string {
	switch content.Kind {
	case ContentText:
		return []string{content.Value}
	case ContentQuote:
		return []string{"> " + content.Value}
	case ContentFields:
		lines := make([]string, 0, len(content.Items))
		for _, item := range content.Items {
			lines = append(lines, item.Label+": "+item.Value)
		}
		return lines
	case ContentList:
		lines := []string{}
		for index, item := range content.ListItems {
			prefix := "-"
			if content.Style == ListOrdered {
				prefix = strconv.Itoa(index+1) + "."
			}
			lines = append(lines, prefix+" "+item.Primary)
			if item.Secondary != "" {
				lines = append(lines, "  "+item.Secondary)
			}
		}
		return lines
	case ContentDivider:
		return []string{"---"}
	default:
		return nil
	}
}
