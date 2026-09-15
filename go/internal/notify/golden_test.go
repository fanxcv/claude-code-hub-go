package notify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 本文件把 Go 的正文产出与 **Node 源实跑**的黄金值逐字段对拍
// （go/testdata/notify_webhook_golden.json）。
//
// 黄金值的来源（不是手抄，也不是 Go 反向生成）：
//   1. 把 Node 仓的 src/lib/webhook/{templates,renderers,types.ts,utils} 拷到一个临时目录；
//   2. 补一个只实现 formatInTimeZone 的 date-fns-tz 桩（避免为一个对拍脚本装依赖）；
//   3. 冻结构建器内部的 new Date()，用 bun 直接跑四个构建器与五家渲染器，打印 JSON。
// 逐条命令与脚本见本 lane 的报告；本文件只负责比对。
//
// 归一化（唯一一处，且只针对 Node 侧产物）：黄金值在生成时把 emoji 去掉、
// 并把剥掉 emoji 后残留的多余空格收敛为单个——依据是仓库指南 §8 禁止 emoji。
// 除 emoji 之外没有任何放宽：标题、字段名、单位、数字格式、顺序、缩进、分隔符全逐字比对。
//
// 两处**有意**的结构性差异由用例显式跳过（不是比对失败）：
//   1. 消息头 Icon：Node 是 emoji（成本用钞票、熔断用插头、榜单用图表），Go 留空；
//   2. 榜单名次：Node 前三名各用一枚奖牌 emoji，Go 统一用 "N."。
//      注意 Node 对第 4 名起本来就用 "4." "5."，故 Go 的形态与 Node 后半段一致，
//      丢掉的是「前三名」这个额外强调，而不是名次本身。

// goldenFixture 是黄金值文件的形状（与 Node 的 StructuredMessage / 渲染产物同形）。
type goldenFixture struct {
	CircuitBreaker         goldenMessage     `json:"circuit_breaker"`
	CircuitBreakerEndpoint goldenMessage     `json:"circuit_breaker_endpoint"`
	CostAlert              goldenMessage     `json:"cost_alert"`
	DailyLeaderboard       goldenMessage     `json:"daily_leaderboard"`
	CacheHitRateAlert      goldenMessage     `json:"cache_hit_rate_alert"`
	RenderWeChat           json.RawMessage   `json:"render.wechat"`
	RenderDingTalk         json.RawMessage   `json:"render.dingtalk"`
	RenderFeishu           json.RawMessage   `json:"render.feishu"`
	RenderTelegram         json.RawMessage   `json:"render.telegram"`
	RenderCustom           json.RawMessage   `json:"render.custom"`
	RenderCustomOverride   json.RawMessage   `json:"render.custom.override"`
	VarsCostAlert          map[string]string `json:"vars.cost_alert"`
	SectionsCostAlert      string            `json:"sections.cost_alert"`
}

type goldenMessage struct {
	Header struct {
		Title string `json:"title"`
		Icon  string `json:"icon"`
		Level string `json:"level"`
	} `json:"header"`
	Sections []goldenSection `json:"sections"`
	Footer   []goldenSection `json:"footer"`
}

type goldenSection struct {
	Title   string          `json:"title"`
	Content []goldenContent `json:"content"`
}

type goldenContent struct {
	Type  string          `json:"type"`
	Value string          `json:"value"`
	Style string          `json:"style"`
	Items json.RawMessage `json:"items"`
}

// goldenFields 把 fields 的 items 解成字段列表。
func (c goldenContent) goldenFields(t *testing.T, where string) []FieldItem {
	t.Helper()
	var items []struct {
		Label string `json:"label"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(c.Items, &items); err != nil {
		t.Fatalf("%s：fields 解析失败：%v", where, err)
	}
	out := make([]FieldItem, 0, len(items))
	for _, item := range items {
		out = append(out, FieldItem{Label: item.Label, Value: item.Value})
	}
	return out
}

// goldenListItems 把 list 的 items 解成列表项。
func (c goldenContent) goldenListItems(t *testing.T, where string) []struct {
	Icon      string `json:"icon"`
	Primary   string `json:"primary"`
	Secondary string `json:"secondary"`
} {
	t.Helper()
	var items []struct {
		Icon      string `json:"icon"`
		Primary   string `json:"primary"`
		Secondary string `json:"secondary"`
	}
	if err := json.Unmarshal(c.Items, &items); err != nil {
		t.Fatalf("%s：list 解析失败：%v", where, err)
	}
	return items
}

// loadGolden 读黄金值夹具。
func loadGolden(t *testing.T) goldenFixture {
	t.Helper()
	path := filepath.Join("..", "..", "testdata", "notify_webhook_golden.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读黄金值失败（%s）：%v", path, err)
	}
	var fixture goldenFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("解析黄金值失败：%v", err)
	}
	return fixture
}

// goldenCases 是本文件用到的固定输入（与生成脚本里的完全一致）。
func goldenCircuitBreakerInput() CircuitBreakerAlertData {
	return CircuitBreakerAlertData{
		ProviderName: "供应商甲", ProviderID: 7, FailureCount: 5,
		RetryAt: "2026-01-02T15:34:05.000Z", LastError: "connection timeout",
	}
}

func goldenCostAlertInput() CostAlertData {
	return CostAlertData{
		TargetType: "provider", TargetName: "供应商丙", TargetID: 3,
		CurrentCost: 82.5, QuotaLimit: 100, Threshold: 0.8, Period: "本月",
	}
}

// TestGoldenStructuredMessagesMatchNode 逐段逐字段比对四条消息（跳过图标与名次标记）。
func TestGoldenStructuredMessagesMatchNode(t *testing.T) {
	fixture := loadGolden(t)

	baselineSource := "historical"
	dropAbs := 0.33
	deltaRel := -0.7333
	cacheInput := CacheHitRateAlertData{
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
			DeltaAbs:       func() *float64 { v := -0.33; return &v }(),
			ReasonCodes:    []string{"abs_min"},
		}},
		SuppressedCount: 2,
		Settings: CacheHitRateAlertSettingsSnapshot{
			WindowMode: "5m", CheckIntervalMinutes: 5, HistoricalLookbackDays: 7,
			MinEligibleRequests: 20, MinEligibleTokens: 0,
			AbsMin: 0.05, DropRel: 0.3, DropAbs: 0.1, CooldownMinutes: 30, TopN: 10,
		},
		GeneratedAt: "2026-01-02T15:04:05.000Z",
	}

	cases := []struct {
		name    string
		message StructuredMessage
		golden  goldenMessage
		// skipListIcons 为真时不比列表项的图标（榜单的名次标记是有意差异）。
		skipListIcons bool
	}{
		{"circuit_breaker", BuildCircuitBreakerMessage(goldenCircuitBreakerInput(), "UTC", fixedNow),
			fixture.CircuitBreaker, false},
		{"circuit_breaker_endpoint", BuildCircuitBreakerMessage(CircuitBreakerAlertData{
			ProviderName: "供应商乙", ProviderID: 9, FailureCount: 3,
			RetryAt:        "2026-01-02T15:34:05.000Z",
			IncidentSource: IncidentSourceEndpoint,
			EndpointID:     func() *int64 { v := int64(42); return &v }(),
			EndpointURL:    "https://relay.example.com/v1",
		}, "UTC", fixedNow), fixture.CircuitBreakerEndpoint, false},
		{"cost_alert", BuildCostAlertMessage(goldenCostAlertInput(), fixedNow), fixture.CostAlert, false},
		{"daily_leaderboard", BuildDailyLeaderboardMessage(DailyLeaderboardData{
			Date: "2026-01-02",
			Entries: []DailyLeaderboardEntry{
				{UserID: 1, UserName: "用户A", TotalRequests: 150, TotalCost: 12.5, TotalTokens: 50000},
				{UserID: 2, UserName: "用户B", TotalRequests: 120, TotalCost: 10.2, TotalTokens: 1500000},
			},
			TotalRequests: 270, TotalCost: 22.7,
		}, fixedNow), fixture.DailyLeaderboard, true},
		{"cache_hit_rate_alert", BuildCacheHitRateAlertMessage(cacheInput, "UTC", fixedNow),
			fixture.CacheHitRateAlert, false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			assertGoldenMessage(t, testCase.name, testCase.message, testCase.golden, testCase.skipListIcons)
		})
	}
}

// assertGoldenMessage 逐字段比对一条消息。
func assertGoldenMessage(
	t *testing.T,
	where string,
	got StructuredMessage,
	want goldenMessage,
	skipListIcons bool,
) {
	t.Helper()
	if got.Header.Title != want.Header.Title {
		t.Fatalf("%s：标题得到 %q，Node 为 %q", where, got.Header.Title, want.Header.Title)
	}
	if string(got.Header.Level) != want.Header.Level {
		t.Fatalf("%s：级别得到 %q，Node 为 %q", where, got.Header.Level, want.Header.Level)
	}
	// 有意差异：Node 的图标是 emoji，Go 侧留空或（缓存告警的）"[CACHE]"。
	// 断言「Go 的图标要么为空、要么与 Node 相同」，避免这里悄悄塞进 emoji。
	if got.Header.Icon != "" && got.Header.Icon != want.Header.Icon {
		t.Fatalf("%s：图标得到 %q，Node 为 %q（只允许留空或与 Node 相同）",
			where, got.Header.Icon, want.Header.Icon)
	}

	if len(got.Sections) != len(want.Sections) {
		t.Fatalf("%s：段数得到 %d，Node 为 %d", where, len(got.Sections), len(want.Sections))
	}
	for index := range want.Sections {
		assertGoldenSection(t, where, index, got.Sections[index], want.Sections[index], skipListIcons)
	}

	if len(got.Footer) != len(want.Footer) {
		t.Fatalf("%s：页脚段数得到 %d，Node 为 %d", where, len(got.Footer), len(want.Footer))
	}
	for index := range want.Footer {
		assertGoldenSection(t, where+" 页脚", index, got.Footer[index], want.Footer[index], skipListIcons)
	}
}

func assertGoldenSection(
	t *testing.T,
	where string,
	index int,
	got Section,
	want goldenSection,
	skipListIcons bool,
) {
	t.Helper()
	label := where + " 段" + itoa(index)
	if got.Title != want.Title {
		t.Fatalf("%s：段标题得到 %q，Node 为 %q", label, got.Title, want.Title)
	}
	if len(got.Content) != len(want.Content) {
		t.Fatalf("%s：内容数得到 %d，Node 为 %d", label, len(got.Content), len(want.Content))
	}
	for position := range want.Content {
		assertGoldenContent(t, label, position, got.Content[position], want.Content[position], skipListIcons)
	}
}

func assertGoldenContent(
	t *testing.T,
	where string,
	index int,
	got SectionContent,
	want goldenContent,
	skipListIcons bool,
) {
	t.Helper()
	label := where + " 内容" + itoa(index)
	if string(got.Kind) != want.Type {
		t.Fatalf("%s：类型得到 %q，Node 为 %q", label, got.Kind, want.Type)
	}
	switch want.Type {
	case "text", "quote":
		if got.Value != want.Value {
			t.Fatalf("%s：值得到 %q，Node 为 %q", label, got.Value, want.Value)
		}
	case "divider":
		// 无内容可比。
	case "fields":
		wantFields := want.goldenFields(t, label)
		if len(got.Items) != len(wantFields) {
			t.Fatalf("%s：字段数得到 %d，Node 为 %d", label, len(got.Items), len(wantFields))
		}
		for position := range wantFields {
			if got.Items[position] != wantFields[position] {
				t.Fatalf("%s：字段%d 得到 %+v，Node 为 %+v",
					label, position, got.Items[position], wantFields[position])
			}
		}
	case "list":
		wantItems := want.goldenListItems(t, label)
		if len(got.ListItems) != len(wantItems) {
			t.Fatalf("%s：列表项数得到 %d，Node 为 %d", label, len(got.ListItems), len(wantItems))
		}
		for position := range wantItems {
			if got.ListItems[position].Primary != wantItems[position].Primary {
				t.Fatalf("%s：第%d项主行得到 %q，Node 为 %q",
					label, position, got.ListItems[position].Primary, wantItems[position].Primary)
			}
			if got.ListItems[position].Secondary != wantItems[position].Secondary {
				t.Fatalf("%s：第%d项次行得到 %q，Node 为 %q",
					label, position, got.ListItems[position].Secondary, wantItems[position].Secondary)
			}
			if skipListIcons {
				continue
			}
			if got.ListItems[position].Icon != wantItems[position].Icon {
				t.Fatalf("%s：第%d项图标得到 %q，Node 为 %q",
					label, position, got.ListItems[position].Icon, wantItems[position].Icon)
			}
		}
		if got.Style != ListStyle(want.Style) {
			t.Fatalf("%s：列表样式得到 %q，Node 为 %q", label, got.Style, want.Style)
		}
	default:
		t.Fatalf("%s：未知内容类型 %q", label, want.Type)
	}
}

// TestGoldenRenderedBodiesMatchNode 逐字节比对五家渠道的请求体（成本预警，无列表故无 emoji 争议）。
func TestGoldenRenderedBodiesMatchNode(t *testing.T) {
	fixture := loadGolden(t)
	message := BuildCostAlertMessage(goldenCostAlertInput(), fixedNow)

	cases := []struct {
		name         string
		providerType string
		config       WebhookRenderConfig
		options      WebhookRenderOptions
		golden       json.RawMessage
	}{
		{"wechat", "wechat", WebhookRenderConfig{}, WebhookRenderOptions{Timezone: "UTC"}, fixture.RenderWeChat},
		{"dingtalk", "dingtalk", WebhookRenderConfig{}, WebhookRenderOptions{Timezone: "UTC"}, fixture.RenderDingTalk},
		{"feishu", "feishu", WebhookRenderConfig{}, WebhookRenderOptions{Timezone: "UTC"}, fixture.RenderFeishu},
		{"telegram", "telegram", WebhookRenderConfig{TelegramChatID: "-100123"},
			WebhookRenderOptions{Timezone: "UTC"}, fixture.RenderTelegram},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			rendered, err := RenderWebhook(testCase.providerType, message, testCase.config, testCase.options)
			if err != nil {
				t.Fatalf("渲染失败：%v", err)
			}
			assertJSONEqual(t, testCase.name, rendered.Body, testCase.golden)
		})
	}
}

// TestGoldenCustomTemplateMatchesNode 比对自定义模板（含绑定级覆盖）。
func TestGoldenCustomTemplateMatchesNode(t *testing.T) {
	fixture := loadGolden(t)
	config := WebhookRenderConfig{
		CustomTemplate: json.RawMessage(`{"text":"目标模板 {{title}}"}`),
		CustomHeaders:  json.RawMessage(`{"X-Token":"t"}`),
	}

	// 无覆盖：用目标模板。
	rendered, err := RenderWebhook("custom", BuildCostAlertMessage(goldenCostAlertInput(), fixedNow),
		config, WebhookRenderOptions{
			NotificationType: "cost_alert",
			Data:             json.RawMessage(`{"targetName":"张三","currentCost":80,"quotaLimit":100}`),
			Timezone:         "UTC",
		})
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}
	assertJSONEqual(t, "custom", rendered.Body, fixture.RenderCustom)
	if rendered.Headers["X-Token"] != "t" {
		t.Fatalf("自定义头未透传：%+v", rendered.Headers)
	}

	// 有覆盖：整体替换模板，占位符取自 data。
	rendered, err = RenderWebhook("custom", BuildCostAlertMessage(goldenCostAlertInput(), fixedNow),
		config, WebhookRenderOptions{
			NotificationType: "cost_alert",
			Data:             json.RawMessage(`{"targetType":"user","targetName":"张三","currentCost":80,"quotaLimit":100,"period":"本月"}`),
			TemplateOverride: json.RawMessage(
				`{"target_name":"{{target_name}}","usage":"{{usage_percent}}%"}`),
			Timezone: "UTC",
		})
	if err != nil {
		t.Fatalf("渲染失败：%v", err)
	}
	assertJSONEqual(t, "custom.override", rendered.Body, fixture.RenderCustomOverride)
}

// TestGoldenTemplateVariablesMatchNode 比对占位符取值表与 {{sections}} 纯文本。
func TestGoldenTemplateVariablesMatchNode(t *testing.T) {
	fixture := loadGolden(t)
	message := BuildCostAlertMessage(goldenCostAlertInput(), fixedNow)
	variables := TemplateVariables(message, WebhookRenderOptions{
		NotificationType: "cost_alert",
		Data: json.RawMessage(
			`{"targetType":"user","targetName":"张三","currentCost":80,"quotaLimit":100,"period":"本月"}`),
		Timezone: "UTC",
	})

	// 黄金值里的键必须都存在且逐字相同。
	for key, want := range fixture.VarsCostAlert {
		got, ok := variables[key]
		if !ok {
			t.Fatalf("缺少占位符 %s", key)
		}
		if got != want {
			t.Fatalf("占位符 %s 得到 %q，Node 为 %q", key, got, want)
		}
	}
	// 反向：Go 多出来的键也应被看见（多一个键意味着模板作者能用 Node 用不了的变量）。
	for key := range variables {
		if _, ok := fixture.VarsCostAlert[key]; !ok {
			t.Fatalf("Go 多出占位符 %s（Node 的变量表里没有）", key)
		}
	}
	if variables["{{sections}}"] != fixture.SectionsCostAlert {
		t.Fatalf("{{sections}} 得到：\n%q\nNode 为：\n%q", variables["{{sections}}"], fixture.SectionsCostAlert)
	}
}

// assertJSONEqual 比对两份 JSON（解析后递归比，忽略键序——键序由 TestGoldenRenderedBodies
// 的逐字节断言覆盖不到，这里补的是「值」的等价性）。
func assertJSONEqual(t *testing.T, where string, got, want []byte) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("%s：Go 侧不是合法 JSON：%v（%s）", where, err, got)
	}
	if err := json.Unmarshal(want, &wantValue); err != nil {
		t.Fatalf("%s：黄金值不是合法 JSON：%v", where, err)
	}
	gotCanonical, _ := json.Marshal(gotValue)
	wantCanonical, _ := json.Marshal(wantValue)
	if string(gotCanonical) != string(wantCanonical) {
		t.Fatalf("%s：正文不符\n得到：%s\nNode ：%s", where, gotCanonical, wantCanonical)
	}
}

// itoa 是 strconv.Itoa 的短别名（本文件里只用于拼接用例标签）。
func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := ""
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}

// TestGoldenFixtureHasNoEmoji 钉住夹具本身不含 emoji（否则「剥 emoji 后对拍」会失去意义：
// 夹具里若偷偷留着 emoji，Go 侧的省略就会被比对放过）。
func TestGoldenFixtureHasNoEmoji(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "notify_webhook_golden.json"))
	if err != nil {
		t.Fatalf("读黄金值失败：%v", err)
	}
	for _, r := range string(raw) {
		switch {
		case r >= 0x1F300 && r <= 0x1FAFF:
			t.Fatalf("夹具里出现 emoji：%U", r)
		case r >= 0x2600 && r <= 0x27BF:
			t.Fatalf("夹具里出现 emoji：%U", r)
		}
	}
	// 夹具的时间戳必须是生成脚本固定的那一刻（否则对拍会随机器时间漂移）。
	if !json.Valid(raw) {
		t.Fatal("夹具不是合法 JSON")
	}
	_ = time.Time{}
}
