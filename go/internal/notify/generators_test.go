package notify

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住三个生成器的**窗口切分与冷却两段式**——这些是只有走完整生成路径才看得见的部分。
// 期望值按 Node 源码手推：
//   - 窗口：tasks/cache-hit-rate-alert.ts:243-256
//   - 冷却：同文件 318-366（发送前 MGET 抑制）与 436-477（发送成功后 SETEX）

// fakeLeaderboardQuerier 记录查询并返回固定行。
type fakeLeaderboardQuerier struct {
	rows  []store.AdminLeaderboardUserRow
	query store.AdminLeaderboardQuery
}

func (q *fakeLeaderboardQuerier) AdminUserLeaderboard(
	_ context.Context,
	query store.AdminLeaderboardQuery,
) ([]store.AdminLeaderboardUserRow, error) {
	q.query = query
	return q.rows, nil
}

// fakeCostQuerier 按「实体 + 窗口起点」返回已花；costOf 为 nil 时一律回 "0"。
type fakeCostQuerier struct {
	keys      []store.NotifyKeyCostLimit
	providers []store.NotifyProviderCostLimit
	costOf    func(entityKey string, start time.Time) string
	windowHit []string
}

func (q *fakeCostQuerier) NotifyKeysWithCostLimits(context.Context) ([]store.NotifyKeyCostLimit, error) {
	return q.keys, nil
}

func (q *fakeCostQuerier) NotifyProvidersWithCostLimits(context.Context) ([]store.NotifyProviderCostLimit, error) {
	return q.providers, nil
}

func (q *fakeCostQuerier) SumLedgerCostInTimeRange(
	_ context.Context,
	entityType store.LedgerEntityType,
	entityID any,
	start time.Time,
	end time.Time,
) (string, error) {
	key := string(entityType) + ":" + stringifyEntityID(entityID)
	q.windowHit = append(q.windowHit, key+"|"+start.UTC().Format("2006-01-02T15:04"))
	if q.costOf != nil {
		return q.costOf(key, start), nil
	}
	return "0", nil
}

func stringifyEntityID(entityID any) string {
	switch value := entityID.(type) {
	case string:
		return value
	case int64:
		return strconv.FormatInt(value, 10)
	default:
		return ""
	}
}

// fakeCacheQuerier 按窗口（start 的分钟偏移）给出固定口径行，并记录每次查询的窗口。
type fakeCacheQuerier struct {
	byStart  map[string][]store.NotifyCacheMetric
	windows  []string
	refs     []store.NotifyProviderRef
	settings *store.SystemSettings
}

func (q *fakeCacheQuerier) NotifyProviderModelCacheMetrics(
	_ context.Context,
	start time.Time,
	end time.Time,
	_ string,
) ([]store.NotifyCacheMetric, error) {
	key := start.UTC().Format("2006-01-02T15:04")
	q.windows = append(q.windows, key+"→"+end.UTC().Format("2006-01-02T15:04"))
	return q.byStart[key], nil
}

func (q *fakeCacheQuerier) NotifyProviderRefs(context.Context) ([]store.NotifyProviderRef, error) {
	return q.refs, nil
}

func (q *fakeCacheQuerier) FindSystemSettings(context.Context) (*store.SystemSettings, error) {
	return q.settings, nil
}

func newGenerators(leaderboard LeaderboardQuerier, cost CostQuerier, cache CacheQuerier, cooldown Cooldown) *Generators {
	return &Generators{
		Leaderboard: leaderboard,
		Cost:        cost,
		Cache:       cache,
		Cooldown:    cooldown,
		Logger:      logx.New(nil),
	}
}

// TestDailyLeaderboardTotalsCoverWholeList 钉住「合计是全量，entries 只取前 N」
// （tasks/daily-leaderboard.ts:27-31、45-56）。
func TestDailyLeaderboardTotalsCoverWholeList(t *testing.T) {
	querier := &fakeLeaderboardQuerier{rows: []store.AdminLeaderboardUserRow{
		{UserID: 1, UserName: "甲", TotalRequests: 10, TotalCostText: "1.5", TotalTokens: 100},
		{UserID: 2, UserName: "乙", TotalRequests: 20, TotalCostText: "2.5", TotalTokens: 200},
		{UserID: 3, UserName: "丙", TotalRequests: 30, TotalCostText: "3.0", TotalTokens: 300},
	}}
	generators := newGenerators(querier, nil, nil, nil)

	data, err := generators.DailyLeaderboard(
		context.Background(), 2, "Asia/Shanghai", time.Date(2026, 3, 1, 1, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if len(data.Entries) != 2 {
		t.Fatalf("应只取前 2 名，实际 %d", len(data.Entries))
	}
	if data.TotalRequests != 60 || data.TotalCost != 7.0 {
		t.Fatalf("合计应覆盖全量（60 / 7.0），实际 %v / %v", data.TotalRequests, data.TotalCost)
	}
	// 2026-03-01T01:00Z 在 Asia/Shanghai 是 09:00，日期为 03-01。
	if data.Date != "2026-03-01" {
		t.Fatalf("日期应按系统时区取本地日，实际 %s", data.Date)
	}
	if querier.query.Period != "last24h" {
		t.Fatalf("日报窗口应是 last24h，实际 %s", querier.query.Period)
	}
}

// TestDailyLeaderboardEmptyReturnsNil 钉住「榜单为空回 nil」（tasks/...:21-24）：不发空榜。
func TestDailyLeaderboardEmptyReturnsNil(t *testing.T) {
	generators := newGenerators(&fakeLeaderboardQuerier{}, nil, nil, nil)
	data, err := generators.DailyLeaderboard(context.Background(), 5, "UTC", time.Now())
	if err != nil || data != nil {
		t.Fatalf("空榜应回 (nil, nil)，实际 (%+v, %v)", data, err)
	}
}

// TestCostAlertsWindowsAndThreshold 钉住成本预警的窗口与阈值：
//   - 5h 是滚动窗（now-5h..now），周/月是系统时区的自然周/月（tasks/cost-alert.ts:79-83、96）；
//   - 触发条件是 `已花 >= 限额 × 阈值`（同文件 96 行）；
//   - 限额为 null 或 <= 0 的档位不检查（87-89 行）；
//   - 供应商只有周/月两档（176-197 行）。
func TestCostAlertsWindowsAndThreshold(t *testing.T) {
	now := time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC) // 周三
	querier := &fakeCostQuerier{
		keys: []store.NotifyKeyCostLimit{
			// 5h 限额 10、已花 8 → 恰好到 10*0.8 触发；周 100、已花 79 → 不触发；月 null → 不检查。
			{ID: 1, Key: "sk-1", UserName: "甲", Limit5h: strPtr("10"), LimitWeek: strPtr("100")},
		},
		providers: []store.NotifyProviderCostLimit{
			// 周 50、已花 90 → 触发；月 0 → 不检查（Node 的 `limitMonth > 0`）。
			{ID: 7, Name: "供应商甲", LimitWeek: strPtr("50"), LimitMonth: strPtr("0")},
		},
		costOf: func(entityKey string, start time.Time) string {
			switch {
			case entityKey == "key:sk-1" && start.Equal(now.Add(-5*time.Hour)):
				return "8"
			case entityKey == "key:sk-1":
				// 周与月两窗都给 79：都不到 100*0.8，也都不到「无月限额」的检查。
				return "79"
			case entityKey == "provider:7":
				return "90"
			default:
				return "0"
			}
		},
	}
	generators := newGenerators(nil, querier, nil, nil)

	alerts, err := generators.CostAlerts(context.Background(), 0.8, "UTC", now)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	periods := map[string]bool{}
	for _, alert := range alerts {
		periods[alert.TargetType+"|"+alert.Period] = true
	}
	if !periods["user|5小时"] {
		t.Fatalf("5h 恰好到阈值应触发: %+v", alerts)
	}
	if periods["user|本周"] {
		t.Fatalf("周 79 < 80 不该触发: %+v", alerts)
	}
	if periods["user|本月"] {
		t.Fatalf("本月限额为 null 不该触发: %+v", alerts)
	}
	if !periods["provider|本周"] {
		t.Fatalf("供应商周 90 >= 40 应触发: %+v", alerts)
	}
	// 5h 窗口 = now-5h；周起点 = 2026-03-02（周一）00:00（Node 的 startOfWeek(weekStartsOn: 1)）。
	found := false
	for _, window := range querier.windowHit {
		if window == "key:sk-1|2026-03-04T05:00" {
			found = true
		}
	}
	if !found {
		t.Fatalf("5h 应是滚动窗（now-5h）: %v", querier.windowHit)
	}
	// 周起点 = 2026-03-02（周一）00:00：Node 的 startOfWeek(weekStartsOn: 1) 在系统时区上取自然周。
	weekFound := false
	for _, window := range querier.windowHit {
		if window == "key:sk-1|2026-03-02T00:00" {
			weekFound = true
		}
	}
	if !weekFound {
		t.Fatalf("周起点应是本周一 00:00（系统时区）: %v", querier.windowHit)
	}
}

// TestCostAlertsInvalidLimitIsSkipped 钉住「限额解析不出数字即跳过该档」（tasks/cost-alert.ts:87-89）。
func TestCostAlertsInvalidLimitIsSkipped(t *testing.T) {
	querier := &fakeCostQuerier{
		keys: []store.NotifyKeyCostLimit{
			{ID: 1, Key: "sk-1", UserName: "甲", Limit5h: strPtr("abc")},
		},
		costOf: func(string, time.Time) string { return "999" },
	}
	generators := newGenerators(nil, querier, nil, nil)
	alerts, err := generators.CostAlerts(context.Background(), 0.8, "UTC", time.Now())
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if len(alerts) != 0 {
		t.Fatalf("限额不可解析应跳过: %+v", alerts)
	}
}

// TestCacheHitRateAlertSuppressesCooldownButOnlyCommitsRemaining 钉住冷却两段式：
//   - 发送前读到键已存在 => 该条被抑制并计入 suppressedCount（tasks/...:332-352）；
//   - 返回的 CooldownKeys **只含未被抑制的那些**，由调用方在投递成功后写下（414-423）。
func TestCacheHitRateAlertSuppressesCooldownButOnlyCommitsRemaining(t *testing.T) {
	now := time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)
	currentStart := now.Add(-5 * time.Minute).Format("2006-01-02T15:04")
	cache := &fakeCacheQuerier{
		byStart: map[string][]store.NotifyCacheMetric{
			currentStart: {
				{ProviderID: 1, ProviderType: "openai-compatible", Model: "m1", TotalRequests: 30, DenominatorTokens: 1000, HitRateTokens: 0.01,
					EligibleRequests: 30, EligibleDenominatorTokens: 1000, HitRateTokensEligible: 0.01},
				{ProviderID: 2, ProviderType: "openai-compatible", Model: "m2", TotalRequests: 30, DenominatorTokens: 1000, HitRateTokens: 0.02,
					EligibleRequests: 30, EligibleDenominatorTokens: 1000, HitRateTokensEligible: 0.02},
			},
		},
		refs: []store.NotifyProviderRef{
			{ID: 1, Name: "渠道甲", ProviderType: "openai-compatible"},
			{ID: 2, Name: "渠道乙", ProviderType: "openai-compatible"},
		},
	}
	// 渠道 1 已在冷却期。
	cooldown := &fakeCooldownReader{present: map[string]bool{
		BuildCacheHitRateAlertCooldownKey(cooldownKeyParams{ProviderID: 1, Model: "m1", WindowMode: CacheWindowMode5m}): true,
	}}
	generators := newGenerators(nil, nil, cache, cooldown)

	settings := store.AdminNotificationSettings{CacheHitRateAlertEnabled: true}
	result, err := generators.CacheHitRateAlert(context.Background(), settings, "UTC", now, 0)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if result == nil {
		t.Fatal("应有一条未被抑制的异常")
	}
	if len(result.Payload.Anomalies) != 1 || result.Payload.Anomalies[0].ProviderID != 2 {
		t.Fatalf("只剩渠道 2: %+v", result.Payload.Anomalies)
	}
	if result.Payload.SuppressedCount != 1 {
		t.Fatalf("应记 1 条被抑制，实际 %d", result.Payload.SuppressedCount)
	}
	if len(result.CooldownKeys) != 1 {
		t.Fatalf("只应待写渠道 2 的键: %v", result.CooldownKeys)
	}
	// 渠道名来自供应商投影（tasks/...:368-369）。
	if result.Payload.Anomalies[0].ProviderName != "渠道乙" {
		t.Fatalf("应补上供应商名: %+v", result.Payload.Anomalies[0])
	}
	if result.CooldownMinutes != 30 {
		t.Fatalf("冷却分钟数应随结果返回: %d", result.CooldownMinutes)
	}
	// 窗口：5 分钟窗（间隔缺省 5 → auto 反推 5m）。
	if result.Payload.Window.Mode != CacheWindowMode5m || result.Payload.Window.DurationMinutes != 5 {
		t.Fatalf("窗口不对: %+v", result.Payload.Window)
	}
	wantStart := now.Add(-5 * time.Minute).Format("2006-01-02T15:04")
	if result.Payload.Window.StartTime[:16] != wantStart {
		t.Fatalf("窗口起点应是 now-5m: %s", result.Payload.Window.StartTime)
	}
}

// TestCacheHitRateAlertAllSuppressedReturnsNil 钉住「异常全被抑制时不发」
// （tasks/...:357-366）：否则收件人会收到一份零条的告警。
func TestCacheHitRateAlertAllSuppressedReturnsNil(t *testing.T) {
	now := time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)
	currentStart := now.Add(-5 * time.Minute).Format("2006-01-02T15:04")
	cache := &fakeCacheQuerier{byStart: map[string][]store.NotifyCacheMetric{
		currentStart: {{ProviderID: 1, Model: "m1", TotalRequests: 30, DenominatorTokens: 1000, HitRateTokens: 0.01,
			EligibleRequests: 30, EligibleDenominatorTokens: 1000, HitRateTokensEligible: 0.01}},
	}}
	cooldown := &fakeCooldownReader{present: map[string]bool{
		BuildCacheHitRateAlertCooldownKey(cooldownKeyParams{ProviderID: 1, Model: "m1", WindowMode: CacheWindowMode5m}): true,
	}}
	generators := newGenerators(nil, nil, cache, cooldown)

	result, err := generators.CacheHitRateAlert(context.Background(), store.AdminNotificationSettings{}, "UTC", now, 0)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if result != nil {
		t.Fatalf("全被抑制应回 (nil, nil)，实际 %+v", result)
	}
}

// TestCacheHitRateAlertCooldownReadFailureStillSends 钉住「读冷却失败照发」
// （tasks/...:147-162）：否则 Redis 抖动会让告警整体静默。
func TestCacheHitRateAlertCooldownReadFailureStillSends(t *testing.T) {
	now := time.Date(2026, 3, 4, 10, 0, 0, 0, time.UTC)
	currentStart := now.Add(-5 * time.Minute).Format("2006-01-02T15:04")
	cache := &fakeCacheQuerier{byStart: map[string][]store.NotifyCacheMetric{
		currentStart: {{ProviderID: 1, Model: "m1", TotalRequests: 30, DenominatorTokens: 1000, HitRateTokens: 0.01,
			EligibleRequests: 30, EligibleDenominatorTokens: 1000, HitRateTokensEligible: 0.01}},
	}}
	cooldown := &fakeCooldownReader{getErr: errors.New("redis down")}
	generators := newGenerators(nil, nil, cache, cooldown)

	result, err := generators.CacheHitRateAlert(context.Background(), store.AdminNotificationSettings{}, "UTC", now, 0)
	if err != nil {
		t.Fatalf("读冷却失败不该中断生成: %v", err)
	}
	if result == nil || len(result.Payload.Anomalies) != 1 {
		t.Fatalf("应照发: %+v", result)
	}
	if result.Payload.SuppressedCount != 0 {
		t.Fatalf("读失败时不该计抑制: %d", result.Payload.SuppressedCount)
	}
}

func strPtr(value string) *string { return &value }
