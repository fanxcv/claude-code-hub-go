package notify

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// 本文件钉住四个正文构建器与五家渲染器：字面量取自 Node 的 templates/*.ts 与 renderers/*.ts
// （逐行对照后手写成期望值，不用 Go 侧实现反向生成，否则对拍等于自证）。
//
// 与 Node 的唯一有意差异是 emoji 被省略（仓库指南 §8）；期望值里因此没有 emoji，
// 其余（标题、字段名、单位、顺序、缩进、分隔符）逐字对齐。

// fixedNow 是本文件固定的时刻（UTC 2026-01-02 15:04:05）。
var fixedNow = time.Date(2026, 1, 2, 15, 4, 5, 0, time.UTC)

// TestBuildCircuitBreakerMessageMatchesNode 钉住熔断正文（circuit-breaker.ts:5）。
func TestBuildCircuitBreakerMessageMatchesNode(t *testing.T) {
	message := BuildCircuitBreakerMessage(CircuitBreakerAlertData{
		ProviderName: "供应商甲",
		ProviderID:   7,
		FailureCount: 5,
		RetryAt:      "2026-01-02T15:34:05.000Z",
		LastError:    "connection timeout",
	}, "UTC", fixedNow)

	if message.Header.Title != "供应商熔断告警" {
		t.Fatalf("标题不符：%q", message.Header.Title)
	}
	if message.Header.Level != LevelError {
		t.Fatalf("级别应为 error，实际 %q", message.Header.Level)
	}
	// Node 的 emoji 图标在 Go 侧留空（见 message.go 文件头）。
	if message.Header.Icon != "" {
		t.Fatalf("熔断标题不应带图标（emoji 已按仓库约定省略），实际 %q", message.Header.Icon)
	}
	if len(message.Sections) != 2 {
		t.Fatalf("应有两段（描述 + 详细信息），实际 %d", len(message.Sections))
	}
	quote := message.Sections[0].Content[0]
	if quote.Kind != ContentQuote || quote.Value != "供应商 供应商甲 (ID: 7) 已触发熔断保护" {
		t.Fatalf("描述不符：%+v", quote)
	}
	fields := message.Sections[1].Content[0]
	if fields.Kind != ContentFields || len(fields.Items) != 3 {
		t.Fatalf("字段段不符：%+v", fields)
	}
	if fields.Items[0].Label != "失败次数" || fields.Items[0].Value != "5 次" {
		t.Fatalf("失败次数字段不符：%+v", fields.Items[0])
	}
	if fields.Items[1].Label != "预计恢复" || fields.Items[1].Value != "2026/01/02 15:34:05" {
		t.Fatalf("预计恢复字段不符：%+v", fields.Items[1])
	}
	if fields.Items[2].Label != "最后错误" || fields.Items[2].Value != "connection timeout" {
		t.Fatalf("最后错误字段不符：%+v", fields.Items[2])
	}
	if len(message.Footer) != 1 || message.Footer[0].Content[0].Value != "熔断器将在预计时间后自动恢复" {
		t.Fatalf("页脚不符：%+v", message.Footer)
	}
}

// TestBuildCircuitBreakerEndpointVariant 钉住端点级的标题、描述与追加字段
// （circuit-breaker.ts:27-46）。
func TestBuildCircuitBreakerEndpointVariant(t *testing.T) {
	endpointID := int64(42)
	message := BuildCircuitBreakerMessage(CircuitBreakerAlertData{
		ProviderName:   "供应商乙",
		ProviderID:     9,
		FailureCount:   3,
		RetryAt:        "2026-01-02T15:34:05.000Z",
		IncidentSource: IncidentSourceEndpoint,
		EndpointID:     &endpointID,
		EndpointURL:    "https://relay.example.com/v1",
	}, "UTC", fixedNow)

	if message.Header.Title != "端点熔断告警" {
		t.Fatalf("端点级标题不符：%q", message.Header.Title)
	}
	quote := message.Sections[0].Content[0].Value
	if quote != "供应商 供应商乙 的端点 (ID: 42) 已触发熔断保护" {
		t.Fatalf("端点级描述不符：%q", quote)
	}
	fields := message.Sections[1].Content[0].Items
	if len(fields) != 4 {
		t.Fatalf("端点级应有 4 个字段（含端点ID 与端点地址），实际 %d", len(fields))
	}
	if fields[2].Label != "端点ID" || fields[2].Value != "42" {
		t.Fatalf("端点ID 字段不符：%+v", fields[2])
	}
	if fields[3].Label != "端点地址" || fields[3].Value != "https://relay.example.com/v1" {
		t.Fatalf("端点地址字段不符：%+v", fields[3])
	}
}

// TestBuildCostAlertMessageMatchesNode 钉住成本预警正文（cost-alert.ts:10）。
func TestBuildCostAlertMessageMatchesNode(t *testing.T) {
	message := BuildCostAlertMessage(CostAlertData{
		TargetType:  "provider",
		TargetName:  "供应商丙",
		TargetID:    3,
		CurrentCost: 82.5,
		QuotaLimit:  100,
		Threshold:   0.8,
		Period:      "本月",
	}, fixedNow)

	if message.Header.Title != "成本预警提醒" || message.Header.Level != LevelWarning {
		t.Fatalf("标题或级别不符：%+v", message.Header)
	}
	quote := message.Sections[0].Content[0].Value
	if quote != "供应商 供应商丙 的消费已达到预警阈值" {
		t.Fatalf("描述不符（provider 应渲染成「供应商」）：%q", quote)
	}
	items := message.Sections[1].Content[0].Items
	want := []FieldItem{
		{Label: "当前消费", Value: "$82.5000"},
		{Label: "配额限制", Value: "$100.0000"},
		// 使用比例保留一位小数；Node 的用量指示灯 emoji 按仓库约定省略。
		{Label: "使用比例", Value: "82.5%"},
		{Label: "剩余额度", Value: "$17.5000"},
		{Label: "统计周期", Value: "本月"},
	}
	if len(items) != len(want) {
		t.Fatalf("字段数不符：%d vs %d", len(items), len(want))
	}
	for index := range want {
		if items[index] != want[index] {
			t.Fatalf("字段 %d 不符：%+v vs %+v", index, items[index], want[index])
		}
	}
	if message.Footer[0].Content[0].Value != "请注意控制消费" {
		t.Fatalf("页脚不符：%+v", message.Footer)
	}
}

// TestBuildCostAlertUserAndZeroQuota 钉住 user 分支与配额为 0 的除零处理。
func TestBuildCostAlertUserAndZeroQuota(t *testing.T) {
	message := BuildCostAlertMessage(CostAlertData{
		TargetType: "user", TargetName: "张三", CurrentCost: 10, QuotaLimit: 0, Period: "5 小时",
	}, fixedNow)
	if !strings.HasPrefix(message.Sections[0].Content[0].Value, "用户 ") {
		t.Fatalf("user 应渲染成「用户」：%q", message.Sections[0].Content[0].Value)
	}
	// Node 除以 0 得 Infinity，toFixed 后是 "Infinity%"；Go 侧按 0 处理，见实现的注释。
	if message.Sections[1].Content[0].Items[2].Value != "0.0%" {
		t.Fatalf("配额为 0 时使用比例应回退 0.0%%，实际 %q",
			message.Sections[1].Content[0].Items[2].Value)
	}
}

// TestBuildDailyLeaderboardMessageMatchesNode 钉住榜单正文（daily-leaderboard.ts:22）。
func TestBuildDailyLeaderboardMessageMatchesNode(t *testing.T) {
	message := BuildDailyLeaderboardMessage(DailyLeaderboardData{
		Date: "2026-01-02",
		Entries: []DailyLeaderboardEntry{
			{UserID: 1, UserName: "用户A", TotalRequests: 150, TotalCost: 12.5, TotalTokens: 50000},
			{UserID: 2, UserName: "用户B", TotalRequests: 120, TotalCost: 10.2, TotalTokens: 1500000},
		},
		TotalRequests: 270,
		TotalCost:     22.7,
	}, fixedNow)

	if message.Header.Title != "过去24小时用户消费排行榜" || message.Header.Level != LevelInfo {
		t.Fatalf("标题或级别不符：%+v", message.Header)
	}
	list := message.Sections[1].Content[0]
	if list.Kind != ContentList || list.Style != ListOrdered {
		t.Fatalf("排名段应为有序列表：%+v", list)
	}
	if list.ListItems[0].Icon != "1." || list.ListItems[1].Icon != "2." {
		t.Fatalf("名次标记应保留信息（Node 用奖牌 emoji，Go 用 N.）：%+v", list.ListItems)
	}
	if list.ListItems[0].Primary != "用户A (ID: 1)" {
		t.Fatalf("主行不符：%q", list.ListItems[0].Primary)
	}
	if list.ListItems[0].Secondary != "消费 $12.5000 · 请求 150 次 · Token 50.00K" {
		t.Fatalf("次行不符：%q", list.ListItems[0].Secondary)
	}
	// 百万级走 M 分支。
	if list.ListItems[1].Secondary != "消费 $10.2000 · 请求 120 次 · Token 1.50M" {
		t.Fatalf("次行（百万级）不符：%q", list.ListItems[1].Secondary)
	}
	if message.Sections[2].Content[0].Kind != ContentDivider {
		t.Fatalf("第三段应为分隔线：%+v", message.Sections[2].Content[0])
	}
	if message.Sections[3].Content[0].Value != "总请求 270 次 · 总消费 $22.7000" {
		t.Fatalf("总览不符：%q", message.Sections[3].Content[0].Value)
	}
}

// TestBuildDailyLeaderboardEmpty 钉住空榜的「暂无数据」分支
// （daily-leaderboard.ts:24-42）。
func TestBuildDailyLeaderboardEmpty(t *testing.T) {
	message := BuildDailyLeaderboardMessage(DailyLeaderboardData{Date: "2026-01-02"}, fixedNow)
	if len(message.Sections) != 1 {
		t.Fatalf("空榜只应有一段，实际 %d", len(message.Sections))
	}
	content := message.Sections[0].Content
	if len(content) != 2 || content[0].Value != "统计时间: 2026-01-02" || content[1].Value != "暂无数据" {
		t.Fatalf("空榜正文不符：%+v", content)
	}
}

// TestBuildCacheHitRateAlertMessageMatchesNode 钉住缓存命中率告警正文
// （cache-hit-rate-alert.ts:60）。
func TestBuildCacheHitRateAlertMessageMatchesNode(t *testing.T) {
	baselineSource := "historical"
	dropAbs := 0.33
	deltaRel := -0.7333
	message := BuildCacheHitRateAlertMessage(CacheHitRateAlertData{
		Window: CacheHitRateAlertWindow{
			Mode: "5m", StartTime: "2026-01-02T14:59:05.000Z",
			EndTime: "2026-01-02T15:04:05.000Z", DurationMinutes: 5,
		},
		Anomalies: []CacheHitRateAlertAnomaly{{
			ProviderID: 1, ProviderName: "供应商甲", Model: "test-model",
			BaselineSource: &baselineSource,
			Current:        CacheHitRateAlertSample{Kind: "eligible", Requests: 100, DenominatorTokens: 10000, HitRateTokens: 0.12},
			Baseline:       &CacheHitRateAlertSample{Kind: "eligible", Requests: 100, DenominatorTokens: 10000, HitRateTokens: 0.45},
			DeltaRel:       &deltaRel,
			DropAbs:        &dropAbs,
		}},
		SuppressedCount: 2,
		Settings: CacheHitRateAlertSettingsSnapshot{
			AbsMin: 0.05, DropRel: 0.3, DropAbs: 0.1,
			MinEligibleRequests: 20, MinEligibleTokens: 0, CooldownMinutes: 30, TopN: 10,
		},
		GeneratedAt: "2026-01-02T15:04:05.000Z",
	}, "UTC", fixedNow)

	if message.Header.Title != "缓存命中率异常告警" {
		t.Fatalf("标题不符：%q", message.Header.Title)
	}
	// Node 这一处的 icon 是 "[CACHE]"（非 emoji），照抄。
	if message.Header.Icon != "[CACHE]" {
		t.Fatalf("图标应为 [CACHE]：%q", message.Header.Icon)
	}
	if message.Sections[0].Content[0].Value != "检测到缓存命中率异常（1 条）" {
		t.Fatalf("描述不符：%q", message.Sections[0].Content[0].Value)
	}
	window := message.Sections[1].Content[0].Items
	if window[0].Value != "5m (5 分钟)" {
		t.Fatalf("窗口字段不符：%+v", window[0])
	}
	if window[1].Value != "2026/01/02 14:59:05" || window[2].Value != "2026/01/02 15:04:05" {
		t.Fatalf("窗口起止不符：%+v %+v", window[1], window[2])
	}
	if window[3].Value != "2" {
		t.Fatalf("抑制数量不符：%+v", window[3])
	}
	thresholds := message.Sections[2].Content[0].Items
	if thresholds[0].Value != "5.0%" || thresholds[1].Value != "10.0%" || thresholds[2].Value != "30.0%" {
		t.Fatalf("阈值百分比不符：%+v", thresholds[:3])
	}
	if thresholds[3].Value != "req>=20, tok>=0" || thresholds[4].Value != "30 分钟" {
		t.Fatalf("样本与冷却不符：%+v %+v", thresholds[3], thresholds[4])
	}
	list := message.Sections[3].Content[0]
	if list.Style != ListBullet {
		t.Fatalf("异常列表应为无序列表：%+v", list.Style)
	}
	if list.ListItems[0].Primary != "供应商甲 / test-model" {
		t.Fatalf("异常标题不符：%q", list.ListItems[0].Primary)
	}
	wantDetails := strings.Join([]string{
		"当前(eligible): 12.0% (req=100, tok=10000)",
		"基线(historical eligible): 45.0% (req=100, tok=10000)",
		"绝对跌幅: 33.0%",
		"相对变化: -73.3%",
	}, "\n")
	if list.ListItems[0].Secondary != wantDetails {
		t.Fatalf("异常明细不符：\n%q\n期望：\n%q", list.ListItems[0].Secondary, wantDetails)
	}
}

// TestBuildCacheHitRateAlertWithoutAnomalies 钉住「未检测到异常」时不出现异常列表。
func TestBuildCacheHitRateAlertWithoutAnomalies(t *testing.T) {
	message := BuildCacheHitRateAlertMessage(CacheHitRateAlertData{}, "UTC", fixedNow)
	if message.Sections[0].Content[0].Value != "未检测到异常" {
		t.Fatalf("描述不符：%q", message.Sections[0].Content[0].Value)
	}
	if len(message.Sections) != 3 {
		t.Fatalf("无异常时不应有异常列表段，实际 %d 段", len(message.Sections))
	}
	// 无基线时给出「基线: 无」，且不输出 null 跌幅行——由上面的用例覆盖有值分支，
	// 这里只钉住「没有基线」的一句话。
	anomaly := CacheHitRateAlertAnomaly{ProviderID: 5}
	details := cacheAnomalyDetails(anomaly)
	if !strings.Contains(details, "Provider #5") && !strings.Contains(details, "基线: 无") {
		t.Fatalf("无基线明细应含「基线: 无」：%q", details)
	}
	if strings.Contains(details, "未知") || strings.Contains(details, "null") {
		t.Fatalf("无值字段不应输出占位符：%q", details)
	}
	// 未命名供应商时标题回退到 Provider #id。
	if cacheAnomalyTitle(anomaly) != "Provider #5 / " {
		t.Fatalf("未命名供应商标题不符：%q", cacheAnomalyTitle(anomaly))
	}
}

// TestFormatHelpersMatchNode 钉住数字与百分比的格式化（三个源文件里的同名助手）。
func TestFormatHelpersMatchNode(t *testing.T) {
	cases := []struct {
		name string
		got  string
		want string
	}{
		{"localeNumber 千分位", localeNumber(1234567), "1,234,567"},
		{"localeNumber 小数", localeNumber(1234.5), "1,234.5"},
		{"localeNumber 千以下", localeNumber(270), "270"},
		{"formatPercent 常规", formatPercent(0.1234), "12.3%"},
		{"formatPercent 负值", formatPercent(-0.7333), "-73.3%"},
		{"formatNumber 取整", formatNumber(99.6), "100"},
		{"formatTokens K", formatTokens(50000), "50.00K"},
		{"formatTokens M", formatTokens(1500000), "1.50M"},
		{"formatTokens 原样", formatTokens(999), "999"},
	}
	for _, testCase := range cases {
		if testCase.got != testCase.want {
			t.Fatalf("%s：得到 %q，期望 %q", testCase.name, testCase.got, testCase.want)
		}
	}
}

// TestRenderWeChatBodyMatchesNode 钉住企业微信渲染（renderers/wechat.ts:10）。
func TestRenderWeChatBodyMatchesNode(t *testing.T) {
	rendered, err := RenderWebhook("wechat",
		BuildCostAlertMessage(CostAlertData{
			TargetType: "user", TargetName: "张三", CurrentCost: 80, QuotaLimit: 100, Period: "本月",
		}, fixedNow),
		WebhookRenderConfig{}, WebhookRenderOptions{Timezone: "UTC"})
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}

	var body struct {
		MsgType  string `json:"msgtype"`
		Markdown struct {
			Content string `json:"content"`
		} `json:"markdown"`
	}
	if err := json.Unmarshal(rendered.Body, &body); err != nil {
		t.Fatalf("请求体不是 JSON：%v", err)
	}
	if body.MsgType != "markdown" {
		t.Fatalf("信封不符：%q", body.MsgType)
	}
	// 头部 `## 标题`、空行、引言 `> ...`、段标题 `**...**`、字段 `标签: 值`、
	// 页脚前 `---`、末尾时间戳——逐段对齐。
	content := body.Markdown.Content
	for _, want := range []string{
		"## 成本预警提醒\n",
		"> 用户 张三 的消费已达到预警阈值",
		"**消费详情**",
		"当前消费: $80.0000",
		"---\n请注意控制消费",
		"2026/01/02 15:04:05",
	} {
		if !strings.Contains(content, want) {
			t.Fatalf("企业微信正文缺少 %q：\n%s", want, content)
		}
	}
	// 无图标时不留 Node 那个多余的双空格。
	if strings.Contains(content, "##  成本") {
		t.Fatalf("无图标时不应出现双空格：%s", content)
	}
	if !strings.HasSuffix(content, "2026/01/02 15:04:05") {
		t.Fatalf("时间戳应在末尾：%s", content)
	}
}

// TestRenderDingTalkEscapesAndEnvelope 钉住钉钉渲染（renderers/dingtalk.ts:11）。
func TestRenderDingTalkEscapesAndEnvelope(t *testing.T) {
	rendered, err := RenderWebhook("dingtalk",
		StructuredMessage{
			Header: MessageHeader{Title: "标题 <a>", Level: LevelInfo},
			Sections: []Section{{Content: []SectionContent{
				textContent("含 & 与 <b> 的正文"),
				fieldsContent(FieldItem{Label: "字段", Value: "值 <x>"}),
			}}},
			Timestamp: fixedNow,
		},
		WebhookRenderConfig{}, WebhookRenderOptions{Timezone: "UTC"})
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}

	var body struct {
		MsgType  string `json:"msgtype"`
		Markdown struct {
			Title string `json:"title"`
			Text  string `json:"text"`
		} `json:"markdown"`
	}
	if err := json.Unmarshal(rendered.Body, &body); err != nil {
		t.Fatalf("请求体不是 JSON：%v", err)
	}
	if body.Markdown.Title != "标题 &lt;a&gt;" {
		t.Fatalf("标题应转义：%q", body.Markdown.Title)
	}
	if !strings.HasPrefix(body.Markdown.Text, "### 标题 &lt;a&gt;") {
		t.Fatalf("正文应以三级标题开头：%q", body.Markdown.Text)
	}
	if !strings.Contains(body.Markdown.Text, "- 字段: 值 &lt;x&gt;") {
		t.Fatalf("字段行应带 `- ` 前缀并转义：%q", body.Markdown.Text)
	}
	if !strings.Contains(body.Markdown.Text, "含 &amp; 与 &lt;b&gt; 的正文") {
		t.Fatalf("正文应转义：%q", body.Markdown.Text)
	}
	// Node 的 dingtalk buildMarkdown 末尾 trim 过。
	if strings.HasSuffix(body.Markdown.Text, "\n") {
		t.Fatalf("正文不应以换行结尾：%q", body.Markdown.Text)
	}
}

// TestRenderFeishuCardStructure 钉住飞书卡片（renderers/feishu.ts:15）。
func TestRenderFeishuCardStructure(t *testing.T) {
	rendered, err := RenderWebhook("feishu",
		StructuredMessage{
			Header: MessageHeader{Title: "标题", Icon: "[CACHE]", Level: LevelWarning},
			Sections: []Section{
				{Title: "段标题", Content: []SectionContent{
					quoteContent("引言"),
					fieldsContent(
						FieldItem{Label: "甲", Value: "1"},
						FieldItem{Label: "乙", Value: "2"},
						FieldItem{Label: "丙", Value: "3"},
					),
					dividerContent(),
				}},
			},
			Footer:    []Section{{Content: []SectionContent{textContent("页脚")}}},
			Timestamp: fixedNow,
		},
		WebhookRenderConfig{}, WebhookRenderOptions{Timezone: "UTC"})
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}

	var body struct {
		MsgType string `json:"msg_type"`
		Card    struct {
			Schema string `json:"schema"`
			Header struct {
				Title struct {
					Tag     string `json:"tag"`
					Content string `json:"content"`
				} `json:"title"`
				Template string `json:"template"`
			} `json:"header"`
			Body struct {
				Elements []map[string]any `json:"elements"`
			} `json:"body"`
		} `json:"card"`
	}
	if err := json.Unmarshal(rendered.Body, &body); err != nil {
		t.Fatalf("请求体不是 JSON：%v", err)
	}
	if body.MsgType != "interactive" || body.Card.Schema != "2.0" {
		t.Fatalf("信封不符：%+v", body)
	}
	// 有图标时标题是「图标 + 空格 + 标题」（Node 的 displayTitle）。
	if body.Card.Header.Title.Content != "[CACHE] 标题" {
		t.Fatalf("标题应带图标前缀：%q", body.Card.Header.Title.Content)
	}
	if body.Card.Header.Template != "orange" {
		t.Fatalf("warning 对应 orange：%q", body.Card.Header.Template)
	}

	// 元素序：段标题 markdown → 引言 markdown → 两个 column_set（3 个字段分两行）→
	// 分隔线 hr → 页脚分隔 hr → 页脚 markdown → 时间戳 markdown。
	// （两个连续的 hr：一个是正文里的 divider，一个是 Node 在页脚前插的 `tag:"hr"`。）
	kinds := make([]string, 0, len(body.Card.Body.Elements))
	for _, element := range body.Card.Body.Elements {
		tag, _ := element["tag"].(string)
		kinds = append(kinds, tag)
	}
	want := []string{"markdown", "markdown", "column_set", "column_set", "hr", "hr", "markdown", "markdown"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("飞书元素序不符：%v，期望 %v", kinds, want)
	}
	// 第二个 column_set 只应有一个列（3 个字段 → 2 + 1）。
	secondRow, _ := body.Card.Body.Elements[3]["columns"].([]any)
	if len(secondRow) != 1 {
		t.Fatalf("第二行应只有一列：%v", secondRow)
	}
	// 字段单元格的 markdown 内容是「粗体标签 + 换行 + 值」。
	firstRow, _ := body.Card.Body.Elements[2]["columns"].([]any)
	column, _ := firstRow[0].(map[string]any)
	elements, _ := column["elements"].([]any)
	cell, _ := elements[0].(map[string]any)
	if cell["content"] != "**甲**\n1" {
		t.Fatalf("字段单元格不符：%v", cell["content"])
	}
	// 末尾元素是时间戳（notation 字号）。
	last := body.Card.Body.Elements[len(body.Card.Body.Elements)-1]
	if last["text_size"] != "notation" || last["content"] != "2026/01/02 15:04:05" {
		t.Fatalf("末尾时间戳元素不符：%v", last)
	}
}

// TestRenderTelegramHTML 钉住 Telegram 渲染（renderers/telegram.ts:10）。
func TestRenderTelegramHTML(t *testing.T) {
	rendered, err := RenderWebhook("telegram",
		StructuredMessage{
			Header: MessageHeader{Title: `标题 "引号"`, Level: LevelWarning},
			Sections: []Section{{Content: []SectionContent{
				quoteContent("引言 & 符号"),
				listContent(ListBullet, ListItem{Primary: "甲", Secondary: "次行"}),
			}}},
			Timestamp: fixedNow,
		},
		WebhookRenderConfig{TelegramChatID: "-100123"},
		WebhookRenderOptions{Timezone: "UTC"})
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}

	var body struct {
		ChatID         string `json:"chat_id"`
		Text           string `json:"text"`
		ParseMode      string `json:"parse_mode"`
		DisablePreview bool   `json:"disable_web_page_preview"`
	}
	if err := json.Unmarshal(rendered.Body, &body); err != nil {
		t.Fatalf("请求体不是 JSON：%v", err)
	}
	if body.ChatID != "-100123" || body.ParseMode != "HTML" || !body.DisablePreview {
		t.Fatalf("信封不符：%+v", body)
	}
	if !strings.HasPrefix(body.Text, "<b>标题 &quot;引号&quot;</b>") {
		t.Fatalf("标题应按 Telegram 规则转义（含双引号）：%q", body.Text)
	}
	// 引言在 Telegram 下是 `&gt; `（先转义再拼）。
	if !strings.Contains(body.Text, "&gt; 引言 &amp; 符号") {
		t.Fatalf("引言渲染不符：%q", body.Text)
	}
	if !strings.Contains(body.Text, "- <b>甲</b>\n  次行") {
		t.Fatalf("列表渲染不符：%q", body.Text)
	}
	if !strings.HasSuffix(body.Text, "2026/01/02 15:04:05") {
		t.Fatalf("时间戳应在末尾：%q", body.Text)
	}
}

// TestRenderTelegramRequiresChatID 钉住缺 chat id 与缺模板的两条构造期报错。
func TestRenderTelegramRequiresChatID(t *testing.T) {
	if _, err := RenderWebhook("telegram", StructuredMessage{}, WebhookRenderConfig{},
		WebhookRenderOptions{}); err == nil {
		t.Fatal("缺 Chat ID 应报错")
	}
	if _, err := RenderWebhook("custom", StructuredMessage{}, WebhookRenderConfig{},
		WebhookRenderOptions{}); err == nil {
		t.Fatal("缺自定义模板应报错")
	}
	if _, err := RenderWebhook("nope", StructuredMessage{}, WebhookRenderConfig{},
		WebhookRenderOptions{}); err == nil {
		t.Fatal("未知渠道应报错")
	}
}

// TestRenderCustomUsesTemplateOverride 钉住绑定级 templateOverride 覆盖目标模板
// （renderers/custom.ts:16）。
func TestRenderCustomUsesTemplateOverride(t *testing.T) {
	config := WebhookRenderConfig{
		CustomTemplate: json.RawMessage(`{"text":"目标模板 {{title}}"}`),
		CustomHeaders:  json.RawMessage(`{"X-Token":"t"}`),
	}
	data := json.RawMessage(`{"targetType":"user","targetName":"张三","currentCost":80,"quotaLimit":100,"period":"本月"}`)
	options := WebhookRenderOptions{
		NotificationType: "cost_alert",
		Data:             data,
		Timezone:         "UTC",
	}

	// 不给覆盖：用目标模板。
	rendered, err := RenderWebhook("custom", BuildCostAlertMessage(CostAlertData{
		TargetType: "user", TargetName: "张三", CurrentCost: 80, QuotaLimit: 100, Period: "本月",
	}, fixedNow), config, options)
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}
	if string(rendered.Body) != `{"text":"目标模板 成本预警提醒"}` {
		t.Fatalf("目标模板渲染不符：%s", rendered.Body)
	}
	if rendered.Headers["X-Token"] != "t" {
		t.Fatalf("自定义头未透传：%+v", rendered.Headers)
	}

	// 给覆盖：整体换模板（不是合并）。{{entries_json}} 只在 daily_leaderboard 下有定义，
	// 故这里在 cost_alert 下应原样保留（Node 的变量表同判）。
	options.TemplateOverride = json.RawMessage(
		`{"target_name":"{{target_name}}","usage":"{{usage_percent}}%","entries":"{{entries_json}}"}`)
	rendered, err = RenderWebhook("custom", BuildCostAlertMessage(CostAlertData{
		TargetType: "user", TargetName: "张三", CurrentCost: 80, QuotaLimit: 100, Period: "本月",
	}, fixedNow), config, options)
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}
	if string(rendered.Body) != `{"target_name":"张三","usage":"80.0%","entries":"{{entries_json}}"}` {
		t.Fatalf("覆盖模板渲染不符：%s", rendered.Body)
	}
}

// TestRenderCustomMissingVariableStaysLiteral 钉住「模板里的未知占位符原样保留」。
//
// Node 的变量表是固定的一批键，模板里写了别的 {{...}} 不会被替换；Go 侧同判——
// 这不是缺陷，而是「不发明值」的约束（否则用户看到的正文会出现凭空的空串）。
func TestRenderCustomMissingVariableStaysLiteral(t *testing.T) {
	rendered, err := RenderWebhook("custom",
		BuildCostAlertMessage(CostAlertData{
			TargetType: "user", TargetName: "张三", CurrentCost: 1, QuotaLimit: 2, Period: "本月",
		}, fixedNow),
		WebhookRenderConfig{CustomTemplate: json.RawMessage(`{"a":"{{nope}}","b":["{{title}}"]}`)},
		WebhookRenderOptions{NotificationType: "cost_alert", Timezone: "UTC"})
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(rendered.Body, &body); err != nil {
		t.Fatalf("请求体不是 JSON：%v", err)
	}
	if body["a"] != "{{nope}}" {
		t.Fatalf("未知占位符应原样保留：%v", body["a"])
	}
	items, ok := body["b"].([]any)
	if !ok || len(items) != 1 || items[0] != "成本预警提醒" {
		t.Fatalf("数组内的占位符也应替换：%v", body["b"])
	}
}

// TestTemplateVariablesCoverAllNodeKeys 钉住占位符表与 Node 的 TEMPLATE_PLACEHOLDERS 对齐。
//
// 键名写错（少花括号、拼写差异）是这类移植最容易出的错，且只在用户配了自定义模板时才暴露，
// 故用「Node 的键清单」逐个断言存在。
func TestTemplateVariablesCoverAllNodeKeys(t *testing.T) {
	now := fixedNow
	message, err := BuildTestMessage("cache_hit_rate_alert", "UTC", now)
	if err != nil {
		t.Fatalf("构建测试消息失败：%v", err)
	}
	variables := TemplateVariables(message, WebhookRenderOptions{
		NotificationType: "cache_hit_rate_alert",
		Data: json.RawMessage(`{"window":{"mode":"5m","startTime":"2026-01-02T14:59:05.000Z",
			"endTime":"2026-01-02T15:04:05.000Z","durationMinutes":5},
			"anomalies":[{"providerId":1,"model":"m"}],"suppressedCount":2,
			"settings":{"absMin":0.05,"dropRel":0.3,"dropAbs":0.1,"cooldownMinutes":30,"topN":10},
			"generatedAt":"2026-01-02T15:04:05.000Z"}`),
		Timezone: "UTC",
	})

	// 通用五项 + cache_hit_rate_alert 的十二项（placeholders.ts:24-125 的键名逐字）。
	want := []string{
		"{{timestamp}}", "{{timestamp_local}}", "{{title}}", "{{level}}", "{{sections}}",
		"{{window_mode}}", "{{window_start}}", "{{window_end}}", "{{anomaly_count}}",
		"{{suppressed_count}}", "{{anomalies_json}}", "{{abs_min}}", "{{drop_rel}}",
		"{{drop_abs}}", "{{cooldown_minutes}}", "{{top_n}}", "{{generated_at}}",
	}
	for _, key := range want {
		if _, ok := variables[key]; !ok {
			t.Fatalf("缺少占位符 %s", key)
		}
	}
	if variables["{{anomaly_count}}"] != "1" {
		t.Fatalf("anomaly_count 不符：%q", variables["{{anomaly_count}}"])
	}
	if variables["{{window_mode}}"] != "5m" {
		t.Fatalf("window_mode 不符：%q", variables["{{window_mode}}"])
	}
	// {{timestamp}} 是 ISO 毫秒（Node 的 toISOString）。
	if variables["{{timestamp}}"] != "2026-01-02T15:04:05.000Z" {
		t.Fatalf("timestamp 应为 ISO 毫秒：%q", variables["{{timestamp}}"])
	}
	// {{sections}} 是无渠道包装的纯文本。
	if !strings.Contains(variables["{{sections}}"], "检测窗口") {
		t.Fatalf("sections 应含段标题：%q", variables["{{sections}}"])
	}

	// 另外三个类型各自的独有键也逐个点名（拼错即失败）。
	others := map[string][]string{
		"circuit_breaker": {"{{provider_name}}", "{{provider_id}}", "{{failure_count}}",
			"{{retry_at}}", "{{last_error}}", "{{incident_source}}", "{{endpoint_id}}", "{{endpoint_url}}"},
		"daily_leaderboard": {"{{date}}", "{{entries_json}}", "{{total_requests}}", "{{total_cost}}"},
		"cost_alert": {"{{target_type}}", "{{target_name}}", "{{current_cost}}", "{{quota_limit}}",
			"{{usage_percent}}"},
	}
	for notificationType, keys := range others {
		built, buildErr := BuildTestMessage(notificationType, "UTC", now)
		if buildErr != nil {
			t.Fatalf("构建 %s 测试消息失败：%v", notificationType, buildErr)
		}
		values := TemplateVariables(built, WebhookRenderOptions{
			NotificationType: notificationType, Timezone: "UTC",
		})
		for _, key := range keys {
			if _, ok := values[key]; !ok {
				t.Fatalf("%s 缺少占位符 %s", notificationType, key)
			}
		}
	}
}

// TestBuildTestMessageCoversFourTypes 钉住测试消息与真实投递走同一批构建器。
func TestBuildTestMessageCoversFourTypes(t *testing.T) {
	types := map[string]string{
		"circuit_breaker":      "供应商熔断告警",
		"daily_leaderboard":    "过去24小时用户消费排行榜",
		"cost_alert":           "成本预警提醒",
		"cache_hit_rate_alert": "缓存命中率异常告警",
	}
	for notificationType, wantTitle := range types {
		message, err := BuildTestMessage(notificationType, "UTC", fixedNow)
		if err != nil {
			t.Fatalf("%s 构建失败：%v", notificationType, err)
		}
		if message.Header.Title != wantTitle {
			t.Fatalf("%s 标题不符：%q，期望 %q", notificationType, message.Header.Title, wantTitle)
		}
	}
	if _, err := BuildTestMessage("nope", "UTC", fixedNow); err == nil {
		t.Fatal("未知类型应报错")
	}
}

// TestBuildDeliveryMessageFallsBackToTestData 钉住「data 为空走测试消息」这条分流。
func TestBuildDeliveryMessageFallsBackToTestData(t *testing.T) {
	message, err := BuildDeliveryMessage("cost_alert", nil, "UTC", fixedNow)
	if err != nil {
		t.Fatalf("构建失败：%v", err)
	}
	if message.Header.Title != "成本预警提醒" {
		t.Fatalf("空 data 应走测试消息：%q", message.Header.Title)
	}

	// 有 data 时用真实值：目标名与金额都应换成 data 里的。
	message, err = BuildDeliveryMessage("cost_alert",
		json.RawMessage(`{"targetType":"user","targetName":"李四","currentCost":9,"quotaLimit":10,"period":"本周"}`),
		"UTC", fixedNow)
	if err != nil {
		t.Fatalf("构建失败：%v", err)
	}
	if !strings.Contains(message.Sections[0].Content[0].Value, "李四") {
		t.Fatalf("应使用真实数据：%+v", message.Sections[0].Content[0].Value)
	}
	if message.Sections[1].Content[0].Items[0].Value != "$9.0000" {
		t.Fatalf("金额不符：%+v", message.Sections[1].Content[0].Items[0])
	}

	// 形状不对时报错（调用方据此判定数据不可用）。
	if _, err := BuildDeliveryMessage("cost_alert", json.RawMessage(`"not-an-object"`), "UTC", fixedNow); err == nil {
		t.Fatal("数据形状不符应报错")
	}
}
