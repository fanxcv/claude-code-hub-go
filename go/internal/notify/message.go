package notify

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
)

// 本文件是**结构化消息**与四个正文构建器，逐条对齐 Node 的 src/lib/webhook/types.ts
// （StructuredMessage / Section / SectionContent）与 src/lib/webhook/templates/*.ts。
//
// 为什么形状要与 Node 一一对应而不是直接拼 markdown：正文要经五家渠道渲染
// （render.go 的 wechat/dingtalk/feishu/telegram 各有一套排版与转义），
// 若在中途就拼成一种渠道的方言，其余四家只能将就。
//
// 登记差异（本文件唯一的一处，全仓约定所迫）：
//   - Node 在标题与列表项上带 emoji（熔断的插头、成本的钞票、榜单的图表、前三名的奖牌、
//     用量指示灯的红黄绿圆）。仓库指南 §8 禁止在任何代码、注释、字符串里使用 emoji，
//     故 Go 侧一律省略这些字形：`MessageHeader.Icon` 字段留着（结构对齐，将来产品要放
//     非 emoji 标记即可），四个构建器不填它。列表名次的**信息**用等价的文字标记保留
//     （见 rankingMarker），不随 emoji 一起丢。
//   - Node 的构建器内部用 new Date() 取时刻；Go 一律显式传入 now（仓内既有约定：
//     时间必须可注入，否则测试只能靠 sleep）。
//
// 与 Node 逐条对应的行号写在每个函数上。

// MessageLevel 是消息级别（Node 的 MessageLevel）。
type MessageLevel string

// 三个级别（取值与 Node 逐字一致，渲染器按它取色）。
const (
	LevelInfo    MessageLevel = "info"
	LevelWarning MessageLevel = "warning"
	LevelError   MessageLevel = "error"
)

// MessageHeader 是消息头（Node 的 MessageHeader）。
type MessageHeader struct {
	Title string
	// Icon 是标题前的小图标。Node 会填 emoji；Go 侧留空（见文件头差异说明）。
	Icon  string
	Level MessageLevel
}

// ListItem 是一个列表项（Node 的 ListItem）。
type ListItem struct {
	Icon      string
	Primary   string
	Secondary string
}

// FieldItem 是「字段: 值」的一项（Node 的 fields 内容的 item）。
type FieldItem struct {
	Label string
	Value string
}

// ContentKind 是段落内容种类（Node 的 SectionContent 判别式）。
type ContentKind string

// 五种内容（与 Node 的 type 取值逐字一致）。
const (
	ContentText    ContentKind = "text"
	ContentQuote   ContentKind = "quote"
	ContentFields  ContentKind = "fields"
	ContentList    ContentKind = "list"
	ContentDivider ContentKind = "divider"
)

// ListStyle 是列表样式（Node 的 list.style）。
type ListStyle string

// 两种列表样式。
const (
	ListOrdered ListStyle = "ordered"
	ListBullet  ListStyle = "bullet"
)

// SectionContent 是一段内容。
//
// 用带判别式的结构体而不是接口：Node 是联合类型，落到 Go 若用接口，五家渲染器都得做
// 一次类型断言与默认分支，反而比按 Kind 分支更难穷尽（switch 少了分支编译器也会报）。
type SectionContent struct {
	Kind  ContentKind
	Value string
	// Items 是 fields 的字段列表。
	Items []FieldItem
	// ListItems 是 list 的条目。
	ListItems []ListItem
	// Style 是 list 的样式。
	Style ListStyle
}

// Section 是一段带可选标题的内容（Node 的 Section）。
type Section struct {
	Title   string
	Content []SectionContent
}

// StructuredMessage 是一条待渲染的消息（Node 的 StructuredMessage）。
type StructuredMessage struct {
	Header    MessageHeader
	Sections  []Section
	Footer    []Section
	Timestamp time.Time
}

// CircuitBreakerAlertData 是熔断告警的数据（Node 的 CircuitBreakerAlertData）。
//
// 事件型：由转发路径在开闸那一刻产生（见 circuit_breaker.go 的发布器），不经生成器。
type CircuitBreakerAlertData struct {
	ProviderName   string `json:"providerName"`
	ProviderID     int64  `json:"providerId"`
	FailureCount   int64  `json:"failureCount"`
	RetryAt        string `json:"retryAt"`
	LastError      string `json:"lastError,omitempty"`
	IncidentSource string `json:"incidentSource,omitempty"`
	EndpointID     *int64 `json:"endpointId,omitempty"`
	EndpointURL    string `json:"endpointUrl,omitempty"`
}

// 熔断事件的两种来源（Node 的 incidentSource 取值）。
const (
	IncidentSourceProvider = "provider"
	IncidentSourceEndpoint = "endpoint"
)

// textContent / quoteContent / dividerContent 是三种简单内容的构造捷径。
func textContent(value string) SectionContent {
	return SectionContent{Kind: ContentText, Value: value}
}

func quoteContent(value string) SectionContent {
	return SectionContent{Kind: ContentQuote, Value: value}
}

func dividerContent() SectionContent {
	return SectionContent{Kind: ContentDivider}
}

func fieldsContent(items ...FieldItem) SectionContent {
	return SectionContent{Kind: ContentFields, Items: items}
}

func listContent(style ListStyle, items ...ListItem) SectionContent {
	return SectionContent{Kind: ContentList, ListItems: items, Style: style}
}

// BuildCircuitBreakerMessage 复刻 buildCircuitBreakerMessage（templates/circuit-breaker.ts:5）。
func BuildCircuitBreakerMessage(
	data CircuitBreakerAlertData,
	timezone string,
	now time.Time,
) StructuredMessage {
	isEndpoint := data.IncidentSource == IncidentSourceEndpoint

	fields := []FieldItem{
		{Label: "失败次数", Value: strconv.FormatInt(data.FailureCount, 10) + " 次"},
		{Label: "预计恢复", Value: FormatWebhookDateTime(data.RetryAt, timezone)},
	}
	if data.LastError != "" {
		fields = append(fields, FieldItem{Label: "最后错误", Value: data.LastError})
	}
	if isEndpoint {
		if data.EndpointID != nil {
			fields = append(fields, FieldItem{
				Label: "端点ID",
				Value: strconv.FormatInt(*data.EndpointID, 10),
			})
		}
		if data.EndpointURL != "" {
			fields = append(fields, FieldItem{Label: "端点地址", Value: data.EndpointURL})
		}
	}

	title := "供应商熔断告警"
	description := "供应商 " + data.ProviderName + " (ID: " +
		strconv.FormatInt(data.ProviderID, 10) + ") 已触发熔断保护"
	if isEndpoint {
		title = "端点熔断告警"
		endpointID := "N/A"
		if data.EndpointID != nil {
			endpointID = strconv.FormatInt(*data.EndpointID, 10)
		}
		description = "供应商 " + data.ProviderName + " 的端点 (ID: " + endpointID + ") 已触发熔断保护"
	}

	return StructuredMessage{
		Header: MessageHeader{Title: title, Level: LevelError},
		Sections: []Section{
			{Content: []SectionContent{quoteContent(description)}},
			{Title: "详细信息", Content: []SectionContent{fieldsContent(fields...)}},
		},
		Footer: []Section{
			{Content: []SectionContent{textContent("熔断器将在预计时间后自动恢复")}},
		},
		Timestamp: now,
	}
}

// BuildCostAlertMessage 复刻 buildCostAlertMessage（templates/cost-alert.ts:10）。
func BuildCostAlertMessage(data CostAlertData, now time.Time) StructuredMessage {
	usagePercent := 0.0
	if data.QuotaLimit != 0 {
		usagePercent = (data.CurrentCost / data.QuotaLimit) * 100
	}
	remaining := data.QuotaLimit - data.CurrentCost
	targetTypeText := "供应商"
	if data.TargetType == "user" {
		targetTypeText = "用户"
	}

	return StructuredMessage{
		Header: MessageHeader{Title: "成本预警提醒", Level: LevelWarning},
		Sections: []Section{
			{
				Content: []SectionContent{
					quoteContent(targetTypeText + " " + data.TargetName + " 的消费已达到预警阈值"),
				},
			},
			{
				Title: "消费详情",
				Content: []SectionContent{fieldsContent(
					FieldItem{Label: "当前消费", Value: "$" + fixed4(data.CurrentCost)},
					FieldItem{Label: "配额限制", Value: "$" + fixed4(data.QuotaLimit)},
					FieldItem{Label: "使用比例", Value: fixed1(usagePercent) + "%"},
					FieldItem{Label: "剩余额度", Value: "$" + fixed4(remaining)},
					FieldItem{Label: "统计周期", Value: data.Period},
				)},
			},
		},
		Footer: []Section{
			{Content: []SectionContent{textContent("请注意控制消费")}},
		},
		Timestamp: now,
	}
}

// BuildDailyLeaderboardMessage 复刻 buildDailyLeaderboardMessage
// （templates/daily-leaderboard.ts:22）。
func BuildDailyLeaderboardMessage(data DailyLeaderboardData, now time.Time) StructuredMessage {
	header := MessageHeader{Title: "过去24小时用户消费排行榜", Level: LevelInfo}

	if len(data.Entries) == 0 {
		return StructuredMessage{
			Header: header,
			Sections: []Section{
				{Content: []SectionContent{
					quoteContent("统计时间: " + data.Date),
					textContent("暂无数据"),
				}},
			},
			Timestamp: now,
		}
	}

	listItems := make([]ListItem, 0, len(data.Entries))
	for index, entry := range data.Entries {
		listItems = append(listItems, ListItem{
			Icon:    rankingMarker(index),
			Primary: entry.UserName + " (ID: " + strconv.FormatInt(entry.UserID, 10) + ")",
			Secondary: "消费 $" + fixed4(entry.TotalCost) +
				" · 请求 " + localeNumber(entry.TotalRequests) + " 次" +
				" · Token " + formatTokens(entry.TotalTokens),
		})
	}

	return StructuredMessage{
		Header: header,
		Sections: []Section{
			{Content: []SectionContent{quoteContent("统计时间: " + data.Date)}},
			{Title: "排名情况", Content: []SectionContent{listContent(ListOrdered, listItems...)}},
			{Content: []SectionContent{dividerContent()}},
			{
				Title: "总览",
				Content: []SectionContent{textContent(
					"总请求 " + localeNumber(data.TotalRequests) + " 次 · 总消费 $" +
						fixed4(data.TotalCost),
				)},
			},
		},
		Timestamp: now,
	}
}

// BuildCacheHitRateAlertMessage 复刻 buildCacheHitRateAlertMessage
// （templates/cache-hit-rate-alert.ts:60）。
func BuildCacheHitRateAlertMessage(
	data CacheHitRateAlertData,
	timezone string,
	now time.Time,
) StructuredMessage {
	anomalyCount := len(data.Anomalies)

	quote := "未检测到异常"
	if anomalyCount > 0 {
		quote = "检测到缓存命中率异常（" + strconv.Itoa(anomalyCount) + " 条）"
	}

	sections := []Section{
		{Content: []SectionContent{quoteContent(quote)}},
		{
			Title: "检测窗口",
			Content: []SectionContent{fieldsContent(
				FieldItem{
					Label: "窗口",
					Value: data.Window.Mode + " (" +
						strconv.Itoa(data.Window.DurationMinutes) + " 分钟)",
				},
				FieldItem{Label: "开始", Value: FormatWebhookDateTime(data.Window.StartTime, timezone)},
				FieldItem{Label: "结束", Value: FormatWebhookDateTime(data.Window.EndTime, timezone)},
				FieldItem{Label: "抑制数量", Value: strconv.Itoa(data.SuppressedCount)},
			)},
		},
		{
			Title: "阈值",
			Content: []SectionContent{fieldsContent(
				FieldItem{Label: "绝对下限(absMin)", Value: formatPercent(data.Settings.AbsMin)},
				FieldItem{Label: "绝对跌幅(dropAbs)", Value: formatPercent(data.Settings.DropAbs)},
				FieldItem{Label: "相对跌幅(dropRel)", Value: formatPercent(data.Settings.DropRel)},
				FieldItem{
					Label: "最小样本",
					Value: "req>=" + strconv.Itoa(data.Settings.MinEligibleRequests) +
						", tok>=" + strconv.Itoa(data.Settings.MinEligibleTokens),
				},
				FieldItem{
					Label: "冷却",
					Value: strconv.Itoa(data.Settings.CooldownMinutes) + " 分钟",
				},
			)},
		},
	}

	if anomalyCount > 0 {
		items := make([]ListItem, 0, anomalyCount)
		for _, anomaly := range data.Anomalies {
			items = append(items, ListItem{
				Primary:   cacheAnomalyTitle(anomaly),
				Secondary: cacheAnomalyDetails(anomaly),
			})
		}
		sections = append(sections, Section{
			Title:   "异常列表",
			Content: []SectionContent{listContent(ListBullet, items...)},
		})
	}

	return StructuredMessage{
		// Node 这一处的 icon 是 "[CACHE]"（非 emoji），故照抄。
		Header:    MessageHeader{Title: "缓存命中率异常告警", Icon: "[CACHE]", Level: LevelWarning},
		Sections:  sections,
		Timestamp: now,
	}
}

// cacheAnomalyTitle 复刻 formatAnomalyTitle（cache-hit-rate-alert.ts:20）。
func cacheAnomalyTitle(anomaly CacheHitRateAlertAnomaly) string {
	provider := strings.TrimSpace(anomaly.ProviderName)
	if provider == "" {
		provider = "Provider #" + strconv.FormatInt(anomaly.ProviderID, 10)
	}
	return provider + " / " + anomaly.Model
}

// cacheAnomalyDetails 复刻 buildAnomalyDetails（cache-hit-rate-alert.ts:26）。
//
// 多行以一个 \n 相连（渲染器按原样输出，故各家渠道里就是多行一段）。
func cacheAnomalyDetails(anomaly CacheHitRateAlertAnomaly) string {
	lines := []string{}
	lines = append(lines, "当前("+anomaly.Current.Kind+"): "+formatPercent(anomaly.Current.HitRateTokens)+
		" (req="+formatNumber(anomaly.Current.Requests)+
		", tok="+formatNumber(anomaly.Current.DenominatorTokens)+")")

	if anomaly.Baseline != nil {
		source := "unknown"
		if anomaly.BaselineSource != nil {
			source = *anomaly.BaselineSource
		}
		lines = append(lines, "基线("+source+" "+anomaly.Baseline.Kind+"): "+
			formatPercent(anomaly.Baseline.HitRateTokens)+
			" (req="+formatNumber(anomaly.Baseline.Requests)+
			", tok="+formatNumber(anomaly.Baseline.DenominatorTokens)+")")
	} else {
		lines = append(lines, "基线: 无")
	}

	if anomaly.DropAbs != nil && !math.IsInf(*anomaly.DropAbs, 0) && !math.IsNaN(*anomaly.DropAbs) {
		lines = append(lines, "绝对跌幅: "+formatPercent(*anomaly.DropAbs))
	}
	if anomaly.DeltaRel != nil && !math.IsInf(*anomaly.DeltaRel, 0) && !math.IsNaN(*anomaly.DeltaRel) {
		lines = append(lines, "相对变化: "+formatPercent(*anomaly.DeltaRel))
	}

	return strings.Join(lines, "\n")
}

// formatPercent 复刻 formatPercent（cache-hit-rate-alert.ts:5）：非有限数给空串。
func formatPercent(rate float64) string {
	if math.IsInf(rate, 0) || math.IsNaN(rate) {
		return ""
	}
	return fixed1(rate*100) + "%"
}

// formatNumber 复刻 formatNumber（cache-hit-rate-alert.ts:10）：四舍五入到整数。
func formatNumber(value float64) string {
	if math.IsInf(value, 0) || math.IsNaN(value) {
		return ""
	}
	return strconv.FormatFloat(math.Round(value), 'f', -1, 64)
}

// formatTokens 复刻 formatTokens（daily-leaderboard.ts:9）。
func formatTokens(tokens float64) string {
	switch {
	case tokens >= 1_000_000:
		return fixed2(tokens/1_000_000) + "M"
	case tokens >= 1_000:
		return fixed2(tokens/1_000) + "K"
	default:
		return localeNumber(tokens)
	}
}

// rankingMarker 给出榜单的**名次标记**。
//
// Node 用奖牌 emoji（前三名各一枚，其余是 "N."）。emoji 被仓库指南禁掉后，
// 若前三条不给标记，企业微信与飞书的渲染器（只认 ListItem.Icon，不认样式）会整段失去名次——
// 那是**信息**丢失而非装饰丢失，故这里统一用 "N." 保住名次。
func rankingMarker(index int) string {
	return strconv.Itoa(index+1) + "."
}

// FormatWebhookDateTime 复刻 formatDateTime（utils/date.ts:14）：yyyy/MM/dd HH:mm:ss。
//
// 入参可以是 RFC3339 字符串或已格式化的文本：解析不了时**原样返回**（Node 会给出
// "Invalid Date"；原样返回至少不丢信息，且不会把内部解析失败伪装成一条正常时间）。
func FormatWebhookDateTime(value string, timezone string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return ""
	}
	parsed, err := parseWebhookTime(trimmed)
	if err != nil {
		return trimmed
	}
	return parsed.In(Location(timezone)).Format("2006/01/02 15:04:05")
}

// FormatWebhookTimestamp 把时刻按渠道习惯格式化（渲染器页脚用）。
func FormatWebhookTimestamp(at time.Time, timezone string) string {
	return at.In(Location(timezone)).Format("2006/01/02 15:04:05")
}

// unmarshalNotifyData 解一份投递数据；形状不符时报错（调用方据此回退到测试正文）。
func unmarshalNotifyData(data []byte, target any) error {
	if err := json.Unmarshal(data, target); err != nil {
		return errors.New("通知数据不是预期形状: " + err.Error())
	}
	return nil
}

// parseWebhookTime 解析 Node 侧会传进来的时间形态。
//
// 只认 ISO 8601：生成器写入的就是 RFC3339（Go 侧生成器与 Node 的 toISOString 同形）。
func parseWebhookTime(value string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.000Z"} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, errors.New("无法解析的时间")
}

// fixed1 / fixed2 / fixed4 复刻 JS 的 toFixed(n)。
func fixed1(value float64) string { return roundFixed(value, 1) }
func fixed2(value float64) string { return roundFixed(value, 2) }
func fixed4(value float64) string { return roundFixed(value, 4) }

// roundFixed 把值四舍五入到 n 位小数后定点输出。
//
// 与 JS toFixed 的差别：JS 在十进制表示上做「四舍五入远离零」，Go 的 FormatFloat 在二进制
// 值上做「就近取偶」。仅在恰好落在半格上的十进制字面量（如 0.125 取两位）会不同，
// 而金额与百分比都来自数据库的定点值，实际不会撞到。
func roundFixed(value float64, digits int) string {
	if math.IsInf(value, 0) || math.IsNaN(value) {
		return "0"
	}
	return strconv.FormatFloat(value, 'f', digits, 64)
}

// localeNumber 复刻 JS 的 Number.toLocaleString()（默认区域为 en-US）：
// 整数千分位分隔，小数最多三位。
//
// 为什么不用 golang.org/x/text：为一条通知文案引一个依赖不值当；这里的语义只有
// 「千分位 + 最多三位小数」两条，写清楚比引依赖便宜。
func localeNumber(value float64) string {
	if math.IsInf(value, 0) || math.IsNaN(value) {
		return "0"
	}
	negative := value < 0
	absolute := math.Abs(value)
	rounded := math.Round(absolute*1000) / 1000
	text := strconv.FormatFloat(rounded, 'f', -1, 64)

	integerPart := text
	fraction := ""
	if index := strings.IndexByte(text, '.'); index >= 0 {
		integerPart, fraction = text[:index], text[index:]
	}
	grouped := groupThousands(integerPart)
	if negative {
		grouped = "-" + grouped
	}
	return grouped + fraction
}

// groupThousands 给整数部分插千分位。
func groupThousands(digits string) string {
	if len(digits) <= 3 {
		return digits
	}
	var builder strings.Builder
	lead := len(digits) % 3
	if lead > 0 {
		builder.WriteString(digits[:lead])
	}
	for index := lead; index < len(digits); index += 3 {
		if builder.Len() > 0 {
			builder.WriteString(",")
		}
		builder.WriteString(digits[index : index+3])
	}
	return builder.String()
}

// BuildTestMessage 复刻 buildTestMessage（templates/test-messages.ts:15）：
// 用示例数据走**同一批构建器**，让「测试推送」展示的正文与真实投递同形。
func BuildTestMessage(notificationType string, timezone string, now time.Time) (StructuredMessage, error) {
	switch notificationType {
	case "circuit_breaker":
		return BuildCircuitBreakerMessage(CircuitBreakerAlertData{
			ProviderName: "测试供应商",
			ProviderID:   0,
			FailureCount: 3,
			RetryAt:      now.Add(30 * time.Minute).UTC().Format(time.RFC3339Nano),
			LastError:    "Connection timeout (示例错误)",
		}, timezone, now), nil
	case "cost_alert":
		return BuildCostAlertMessage(CostAlertData{
			TargetType:  "user",
			TargetName:  "测试用户",
			TargetID:    0,
			CurrentCost: 80,
			QuotaLimit:  100,
			Threshold:   0.8,
			Period:      "本月",
		}, now), nil
	case "daily_leaderboard":
		return BuildDailyLeaderboardMessage(DailyLeaderboardData{
			Date: now.UTC().Format("2006-01-02"),
			Entries: []DailyLeaderboardEntry{
				{UserID: 1, UserName: "用户A", TotalRequests: 150, TotalCost: 12.5, TotalTokens: 50000},
				{UserID: 2, UserName: "用户B", TotalRequests: 120, TotalCost: 10.2, TotalTokens: 40000},
			},
			TotalRequests: 270,
			TotalCost:     22.7,
		}, now), nil
	case "cache_hit_rate_alert":
		baselineSource := "historical"
		deltaAbs := -0.33
		deltaRel := -0.7333
		dropAbs := 0.33
		return BuildCacheHitRateAlertMessage(CacheHitRateAlertData{
			Window: CacheHitRateAlertWindow{
				Mode:            "5m",
				StartTime:       now.Add(-5 * time.Minute).UTC().Format(time.RFC3339Nano),
				EndTime:         now.UTC().Format(time.RFC3339Nano),
				DurationMinutes: 5,
			},
			Anomalies: []CacheHitRateAlertAnomaly{
				{
					ProviderID:     1,
					ProviderName:   "测试供应商",
					ProviderType:   "claude",
					Model:          "test-model",
					BaselineSource: &baselineSource,
					Current: CacheHitRateAlertSample{
						Kind: "eligible", Requests: 100,
						DenominatorTokens: 10000, HitRateTokens: 0.12,
					},
					Baseline: &CacheHitRateAlertSample{
						Kind: "eligible", Requests: 100,
						DenominatorTokens: 10000, HitRateTokens: 0.45,
					},
					DeltaAbs:    &deltaAbs,
					DeltaRel:    &deltaRel,
					DropAbs:     &dropAbs,
					ReasonCodes: []string{"abs_min", "drop_abs_rel"},
				},
			},
			SuppressedCount: 0,
			Settings: CacheHitRateAlertSettingsSnapshot{
				WindowMode: "auto", CheckIntervalMinutes: 5, HistoricalLookbackDays: 7,
				MinEligibleRequests: 20, MinEligibleTokens: 0,
				AbsMin: 0.05, DropRel: 0.3, DropAbs: 0.1, CooldownMinutes: 30, TopN: 10,
			},
			GeneratedAt: now.UTC().Format(time.RFC3339Nano),
		}, timezone, now), nil
	default:
		return StructuredMessage{}, errors.New("未知通知类型: " + notificationType)
	}
}

// BuildDeliveryMessage 按通知类型把**真实数据**构建成消息。
//
// data 为空表示「测试推送」路径（Node 的 buildTestMessage），非空则按类型解出数据再走
// 对应的构建器（Node 的 notification-queue 在投递前用同一批构建器把 payload 变成消息）。
func BuildDeliveryMessage(
	notificationType string,
	data []byte,
	timezone string,
	now time.Time,
) (StructuredMessage, error) {
	if len(data) == 0 || string(data) == "null" {
		return BuildTestMessage(notificationType, timezone, now)
	}
	switch notificationType {
	case "circuit_breaker":
		var payload CircuitBreakerAlertData
		if err := unmarshalNotifyData(data, &payload); err != nil {
			return StructuredMessage{}, err
		}
		return BuildCircuitBreakerMessage(payload, timezone, now), nil
	case "cost_alert":
		var payload CostAlertData
		if err := unmarshalNotifyData(data, &payload); err != nil {
			return StructuredMessage{}, err
		}
		return BuildCostAlertMessage(payload, now), nil
	case "daily_leaderboard":
		var payload DailyLeaderboardData
		if err := unmarshalNotifyData(data, &payload); err != nil {
			return StructuredMessage{}, err
		}
		return BuildDailyLeaderboardMessage(payload, now), nil
	case "cache_hit_rate_alert":
		var payload CacheHitRateAlertData
		if err := unmarshalNotifyData(data, &payload); err != nil {
			return StructuredMessage{}, err
		}
		return BuildCacheHitRateAlertMessage(payload, timezone, now), nil
	default:
		return StructuredMessage{}, errors.New("未知通知类型: " + notificationType)
	}
}
