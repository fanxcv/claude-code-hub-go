package adminapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是 GET /api/leaderboard 的端到端用例：真路由表 + 真库。
//
// 验的是三件只有真库真路由才看得出的事：
//  1. **周期边界在 SQL 里**（`CURRENT_TIMESTAMP AT TIME ZONE tz` 与 DATE_TRUNC）：40 天前的行
//     必须被 daily 排除、被 allTime 收下；
//  2. **作答形状**：`modelStats` 的存在性取决于请求开关、`*Formatted` 字段只由 currencyDisplay 决定、
//     provider 面的 `cacheCoefficientBp` 无数据时为 null；
//  3. **乐观缓存**：同键第二次请求不再查库（用假缓存统计次数），键里嵌着 scope/周期/时区/币种/开关。

const leaderboardITMarker = "go-leaderboard-it"

// leaderboardIT 是一次用例的夹具集合。
type leaderboardIT struct {
	router *Router
	pool   *store.Pool
	prefix string
	// 两个用户用来验 user 面的花费降序；供应商用来验 provider 面。
	userAID, userBID, providerID, vendorID int64
	// cache 是假缓存（nil 表示本用例不带缓存）。
	cache *fakeLeaderboardCache
}

// fakeLeaderboardCache 是进程内假缓存：只用于断言「读了几次、写了几次、键长什么样」。
type fakeLeaderboardCache struct {
	values  map[string][]byte
	gets    int
	sets    int
	lastKey string
}

func newFakeLeaderboardCache() *fakeLeaderboardCache {
	return &fakeLeaderboardCache{values: map[string][]byte{}}
}

func (c *fakeLeaderboardCache) Get(_ context.Context, key string) ([]byte, bool, error) {
	c.gets++
	c.lastKey = key
	value, ok := c.values[key]
	return value, ok, nil
}

func (c *fakeLeaderboardCache) SetEX(
	_ context.Context,
	key string,
	_ time.Duration,
	value []byte,
) error {
	c.sets++
	c.lastKey = key
	c.values[key] = value
	return nil
}

// seedLeaderboard 造夹具：一个厂商、一个供应商、两个用户、四行账本。
//
// 行设计（便于断言）：
//
//	行1 用户A 模型 lb-a 成功：cost 1.5、input 10 / output 20 / cacheCreation 40 / cacheRead 30、
//	    ttft 100、duration 1000、firstByte 200（TPS = 20 / 0.8s = 25）
//	行2 用户B 模型 lb-b 失败：cost 0.5、input 10 / output 20
//	行3 用户A 模型 lb-a 成功但 40 天前：cost 9（只应出现在 allTime / 覆盖它的 custom 里）
//	行4 用户A 模型 lb-a 成功但 blocked_by 非空：cost 9（计价口径必须排除）
func seedLeaderboard(t *testing.T, pools *store.Pools, withCache bool) *leaderboardIT {
	t.Helper()
	ctx := context.Background()
	it := &leaderboardIT{prefix: fmt.Sprintf("%s-%d", leaderboardITMarker, time.Now().UnixNano())}

	pool, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	it.pool = pool

	if err := pool.QueryRow(ctx,
		`INSERT INTO provider_vendors (website_domain, display_name) VALUES ($1, $2) RETURNING id`,
		it.prefix+".invalid", it.prefix).Scan(&it.vendorID); err != nil {
		t.Fatalf("建厂商失败: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO providers (name, url, key, provider_vendor_id, is_enabled, weight, priority,
			cost_multiplier, group_tag, provider_type)
		VALUES ($1, $2, $3, $4, true, 1, 0, '1.0'::numeric, $1, 'claude')
		RETURNING id`,
		it.prefix+"-provider", "https://"+it.prefix+".invalid", "sk-leaderboard-it",
		it.vendorID).Scan(&it.providerID); err != nil {
		t.Fatalf("建供应商失败: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO users (name) VALUES ($1) RETURNING id`,
		it.prefix+"-a").Scan(&it.userAID); err != nil {
		t.Fatalf("建用户A失败: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO users (name) VALUES ($1) RETURNING id`,
		it.prefix+"-b").Scan(&it.userBID); err != nil {
		t.Fatalf("建用户B失败: %v", err)
	}

	t.Cleanup(func() {
		cleanup := context.Background()
		_, _ = pool.Exec(cleanup, `DELETE FROM usage_ledger WHERE provider_id = $1`, it.providerID)
		_, _ = pool.Exec(cleanup, `DELETE FROM users WHERE id = ANY($1)`,
			[]int64{it.userAID, it.userBID})
		_, _ = pool.Exec(cleanup, `DELETE FROM providers WHERE id = $1`, it.providerID)
		_, _ = pool.Exec(cleanup, `DELETE FROM provider_cache_effectiveness WHERE provider_id = $1`,
			it.providerID)
		_, _ = pool.Exec(cleanup, `DELETE FROM provider_vendors WHERE id = $1`, it.vendorID)
	})

	requestID := time.Now().Unix() % 1_000_000 * 1000
	insert := func(
		userID int64,
		model string,
		cost string,
		inputTokens, outputTokens int,
		cacheCreation, cacheRead int,
		createdAt time.Time,
		outcome string,
		blocked bool,
	) {
		var blockedBy *string
		if blocked {
			value := "sensitive_word"
			blockedBy = &value
		}
		requestID++
		if _, err := pool.Exec(ctx, `
			INSERT INTO usage_ledger (request_id, provider_id, user_id, key, final_provider_id, model,
				original_model, endpoint, cost_usd, input_tokens, output_tokens,
				cache_creation_input_tokens, cache_read_input_tokens, status_code, is_success,
				success_rate_outcome, duration_ms, ttfb_ms, first_byte_ms, created_at, is_replay,
				blocked_by)
			VALUES ($1, $2, $3, $4, $2, $5, $5, '/v1/messages', $6::numeric, $7, $8, $9, $10,
				$11, $12, $13, $14, $15, $16, $17, false, $18)`,
			requestID, it.providerID, userID, it.prefix+"-key", model,
			cost, inputTokens, outputTokens, cacheCreation, cacheRead,
			200, outcome == "success", outcome, 1000, 100, 200, createdAt, blockedBy); err != nil {
			t.Fatalf("种账本行失败: %v", err)
		}
	}

	now := time.Now().UTC()
	success, failure := "success", "failure"
	insert(it.userAID, "lb-a", "1.5", 10, 20, 40, 30, now, success, false)
	insert(it.userBID, "lb-b", "0.5", 10, 20, 0, 0, now, failure, false)
	insert(it.userAID, "lb-a", "9", 10, 20, 0, 0, now.AddDate(0, 0, -40), success, false)
	insert(it.userAID, "lb-a", "9", 10, 20, 0, 0, now, success, true)

	deps := Deps{
		Logger: logx.New(nil),
		Guard:  principalGuard{principal: Principal{UserID: it.userAID, Username: "admin", IsAdmin: true}},
		Store:  pools,
	}
	router := New(Options{Deps: deps})
	var cache LeaderboardCacheStore
	if withCache {
		it.cache = newFakeLeaderboardCache()
		cache = it.cache
	}
	RegisterLeaderboardRoutes(router, deps, cache)
	it.router = router
	return it
}

// call 发一个 GET 并返回状态码与正文。
func (it *leaderboardIT) call(t *testing.T, path string) (int, string) {
	t.Helper()
	return call(it.router, http.MethodGet, path, "")
}

// decodeEntries 把正文解成对象数组（真库返回的是裸数组）。
func decodeEntries(t *testing.T, body string) []map[string]any {
	t.Helper()
	var entries []map[string]any
	if err := json.Unmarshal([]byte(body), &entries); err != nil {
		t.Fatalf("正文不是对象数组：%s（%v）", body, err)
	}
	return entries
}

// findEntry 按字符串字段找一行。
func findEntry(t *testing.T, entries []map[string]any, field, value string) map[string]any {
	t.Helper()
	for _, entry := range entries {
		if entry[field] == value {
			return entry
		}
	}
	t.Fatalf("未找到 %s=%s 的行：%v", field, value, entries)
	return nil
}

// indexOfEntry 返回第一个匹配行的下标（找不到返回 -1）。
func indexOfEntry(entries []map[string]any, field, value string) int {
	for index, entry := range entries {
		if entry[field] == value {
			return index
		}
	}
	return -1
}

// adminLocalToday 取「服务端时区下的今天」（date 类型，零时刻由 UTC 承载）。
//
// 与产品同源：时区走 pools.AdminSystemTimezoneOrUTC（DB -> TZ -> Asia/Shanghai 的降级链），
// 日期由库的 CURRENT_TIMESTAMP 折算，从而避免用例对宿主时钟/时区的隐式依赖——
// 按当地日界切窗的查询，其期望日期必须也按当地日界算。
func adminLocalToday(t *testing.T, pools *store.Pools) time.Time {
	t.Helper()
	ctx := context.Background()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取 control 分道失败: %v", err)
	}
	var today time.Time
	if err := pool.QueryRow(ctx,
		`SELECT (CURRENT_TIMESTAMP AT TIME ZONE $1)::date`,
		pools.AdminSystemTimezoneOrUTC(ctx),
	).Scan(&today); err != nil {
		t.Fatalf("按服务端时区取今日失败: %v", err)
	}
	return today
}

func TestLeaderboardUserScopeIntegration(t *testing.T) {
	pools := endpointWritePools(t)
	it := seedLeaderboard(t, pools, false)
	nameA, nameB := it.prefix+"-a", it.prefix+"-b"

	status, body := it.call(t, "/api/leaderboard?period=daily&scope=user")
	if status != http.StatusOK {
		t.Fatalf("user 榜应 200，实得 %d：%s", status, body)
	}
	entries := decodeEntries(t, body)
	// 榜是全站排序（共享库里还有别家的行），故只按夹具用户名定位、并断言两者的相对次序。
	first := findEntry(t, entries, "userName", nameA)
	if indexOfEntry(entries, "userName", nameA) > indexOfEntry(entries, "userName", nameB) {
		t.Fatalf("A 花费 1.5 高于 B 的 0.5，应排在前面：%s", body)
	}
	// 形状：user 面不带 modelStats（未请求），金额字段是 number + 格式化串。
	for _, key := range []string{"userId", "userName", "totalRequests", "totalCost", "totalTokens",
		"totalCostFormatted"} {
		if _, ok := first[key]; !ok {
			t.Fatalf("user 行缺字段 %s：%s", key, body)
		}
	}
	if _, ok := first["modelStats"]; ok {
		t.Fatalf("未请求 includeUserModelStats 时不应带 modelStats：%s", body)
	}
	if first["totalCost"].(float64) != 1.5 || first["totalCostFormatted"] != "$1.50" {
		t.Fatalf("A 的金额应为 1.5 / $1.50：%v", first)
	}
	if first["totalTokens"].(float64) != 100 {
		t.Fatalf("token 口径 = input+output+cacheCreation+cacheRead = 100：%v", first)
	}
	// blocked_by 非空的那行（cost 9）不得计入。
	if first["totalRequests"].(float64) != 1 {
		t.Fatalf("拦截行不得计入：%v", first)
	}

	// allTime 应含 40 天前的那行（A 合计 10.5）。
	status, body = it.call(t, "/api/leaderboard?period=allTime&scope=user")
	if status != http.StatusOK {
		t.Fatalf("allTime 应 200，实得 %d：%s", status, body)
	}
	entries = decodeEntries(t, body)
	a := findEntry(t, entries, "userName", nameA)
	if a["totalCost"].(float64) != 10.5 || a["totalCostFormatted"] != "$10.50" ||
		a["totalRequests"].(float64) != 2 {
		t.Fatalf("allTime 的 A 应含 40 天前的行（10.5 / 2 条）：%v", a)
	}

	// custom 覆盖今天：只含今天两行。
	//
	// 起止日期必须按**服务端时区的日历日**取：产品把 startDate/endDate 当当地日历日
	// （`(startDate::date)::timestamp AT TIME ZONE <tz>`，见 store/admin_leaderboard.go 的
	// leaderboardLedgerCondition），右端是 endDate 次日零点（排他）。若这里用 UTC 日期，
	// CST 00:00–08:00 期间 UTC 日期比当地日期落后一天，endDate 便落在「昨天」，
	// 今天的那行被排到窗外——此时期望值 10.5 是**反的**（会放过按 UTC 切窗的实现、
	// 却判失败正确的当地日界实现）。
	today := adminLocalToday(t, pools)
	start := today.AddDate(0, 0, -60).Format("2006-01-02")
	end := today.Format("2006-01-02")
	query := url.Values{}
	query.Set("period", "custom")
	query.Set("scope", "user")
	query.Set("startDate", start)
	query.Set("endDate", end)
	status, body = it.call(t, "/api/leaderboard?"+query.Encode())
	if status != http.StatusOK {
		t.Fatalf("custom 应 200，实得 %d：%s", status, body)
	}
	entries = decodeEntries(t, body)
	customA := findEntry(t, entries, "userName", nameA)
	if customA["totalCost"].(float64) != 10.5 {
		t.Fatalf("custom 60 天应含 A 的 40 天前行（10.5）：%v", customA)
	}

	// 带 includeUserModelStats（管理员）：出现 modelStats，且逐模型金额带格式化串。
	status, body = it.call(t, "/api/leaderboard?period=daily&scope=user&includeUserModelStats=1")
	if status != http.StatusOK {
		t.Fatalf("user + modelStats 应 200，实得 %d：%s", status, body)
	}
	entries = decodeEntries(t, body)
	first = findEntry(t, entries, "userName", nameA)
	modelStats, ok := first["modelStats"].([]any)
	if !ok || len(modelStats) != 1 {
		t.Fatalf("A 应有一个模型拆分：%v", first)
	}
	modelStat := modelStats[0].(map[string]any)
	if modelStat["model"] != "lb-a" || modelStat["totalCostFormatted"] != "$1.50" {
		t.Fatalf("模型拆分的字段不符：%v", modelStat)
	}
}

func TestLeaderboardProviderScopeIntegration(t *testing.T) {
	pools := endpointWritePools(t)
	it := seedLeaderboard(t, pools, false)
	providerName := it.prefix + "-provider"

	// provider 面：无 includeModelStats → 不带 modelStats，但均值与系数列都在（系数无数据为 null）。
	status, body := it.call(t, "/api/leaderboard?period=daily&scope=provider")
	if status != http.StatusOK {
		t.Fatalf("provider 榜应 200，实得 %d：%s", status, body)
	}
	entries := decodeEntries(t, body)
	entry := findEntry(t, entries, "providerName", providerName)
	if _, ok := entry["modelStats"]; ok {
		t.Fatalf("未请求 includeModelStats 时不应带 modelStats：%s", body)
	}
	// 均值：供应商当日合计 cost 2.0 / 2 次 = 1.0；tokens 130 → 每百万 15384.615…（两位截断）。
	if entry["avgCostPerRequest"].(float64) != 1 ||
		entry["avgCostPerRequestFormatted"] != "$1.00" {
		t.Fatalf("单请求均值不符：%v", entry)
	}
	if entry["avgCostPerMillionTokensFormatted"] != "$15,384.62" {
		t.Fatalf("每百万 token 均值不符：%v", entry)
	}
	if entry["cacheCoefficientBp"] != nil {
		t.Fatalf("无缓存效果数据时系数应为 null：%v", entry)
	}
	// TPS = output / ((duration - firstByte)/1000) = 20 / 0.8 = 25。
	if entry["avgTokensPerSecond"].(float64) != 25 {
		t.Fatalf("TPS 口径不符（应 25）：%v", entry)
	}
	// 成败口径：分子只数 success、分母数 countable。夹具 A 成功 + B 失败 → 1/2 = 0.5。
	if entry["successRate"].(float64) != 0.5 {
		t.Fatalf("成功率应为 1/2：%v", entry)
	}

	// includeModelStats：按模型拆分行带 basis 披露与前缀字段。
	status, body = it.call(t, "/api/leaderboard?period=daily&scope=provider&includeModelStats=1")
	if status != http.StatusOK {
		t.Fatalf("provider + modelStats 应 200，实得 %d：%s", status, body)
	}
	entries = decodeEntries(t, body)
	entry = findEntry(t, entries, "providerName", providerName)
	modelStats, ok := entry["modelStats"].([]any)
	if !ok || len(modelStats) != 2 {
		t.Fatalf("应有 lb-a / lb-b 两个模型拆分：%v", entry["modelStats"])
	}
	stat := modelStats[0].(map[string]any)
	if stat["model"] != "lb-a" {
		t.Fatalf("按花费降序应 lb-a 在前：%v", modelStats)
	}
	for _, key := range []string{"rowIdentityBasis", "successRateBasis", "costTokensBasis",
		"basisDisclosureRequired", "cacheCoefficientBp", "avgCostPerRequestFormatted"} {
		if _, ok := stat[key]; !ok {
			t.Fatalf("模型拆分缺字段 %s：%v", key, stat)
		}
	}
	// billingModelSource 库内为 redirected/model 时不要求披露；这里只断言字段存在且布尔。
	if _, ok := stat["basisDisclosureRequired"].(bool); !ok {
		t.Fatalf("basisDisclosureRequired 应是布尔：%v", stat)
	}

	// providerType 过滤：白名单外的值 400。
	status, body = it.call(t, "/api/leaderboard?period=daily&scope=provider&providerType=nope")
	if status != http.StatusBadRequest || !strings.Contains(body, "参数 providerType 不合法") {
		t.Fatalf("非法 providerType 应 400：%d %s", status, body)
	}
}

func TestLeaderboardCacheHitRateAndModelScopesIntegration(t *testing.T) {
	pools := endpointWritePools(t)
	it := seedLeaderboard(t, pools, false)

	// providerCacheHitRate：命中率 = cacheRead / (input + cacheCreation + cacheRead)
	// = 30 / (10 + 40 + 30) = 0.375；且这条**恒带** modelStats。
	status, body := it.call(t, "/api/leaderboard?period=daily&scope=providerCacheHitRate")
	if status != http.StatusOK {
		t.Fatalf("缓存命中率榜应 200，实得 %d：%s", status, body)
	}
	entries := decodeEntries(t, body)
	entry := findEntry(t, entries, "providerName", it.prefix+"-provider")
	if _, ok := entry["modelStats"].([]any); !ok {
		t.Fatalf("providerCacheHitRate 恒带 modelStats：%s", body)
	}
	if entry["totalInputTokens"].(float64) != 80 || entry["cacheReadTokens"].(float64) != 30 {
		t.Fatalf("输入侧 token 口径不符（应 80 / 30）：%v", entry)
	}
	if entry["cacheHitRate"].(float64) != 0.375 {
		t.Fatalf("命中率应为 0.375：%v", entry)
	}
	// cacheCreationCost 只算 cacheCreation > 0 的那行：1.5。
	if entry["cacheCreationCost"].(float64) != 1.5 ||
		entry["cacheCreationCostFormatted"] != "$1.50" {
		t.Fatalf("cacheCreationCost 口径不符：%v", entry)
	}
	// 兼容别名 totalTokens 等于 totalInputTokens。
	if entry["totalTokens"].(float64) != entry["totalInputTokens"].(float64) {
		t.Fatalf("totalTokens 应等于 totalInputTokens：%v", entry)
	}

	// user 面的缓存命中率：只有 A 有 cache 数据（B 的 cache 全 0 被 cacheRequired 条件排除）。
	status, body = it.call(t, "/api/leaderboard?period=daily&scope=userCacheHitRate")
	if status != http.StatusOK {
		t.Fatalf("用户缓存命中率榜应 200，实得 %d：%s", status, body)
	}
	entries = decodeEntries(t, body)
	cacheUserA := findEntry(t, entries, "userName", it.prefix+"-a")
	if cacheUserA["cacheHitRate"].(float64) != 0.375 {
		t.Fatalf("A 的命中率应为 0.375：%v", cacheUserA)
	}
	// B 的缓存列全 0，被 cacheRequired 条件排除，不该出现在这条榜上。
	for _, entry := range entries {
		if entry["userName"] == it.prefix+"-b" {
			t.Fatalf("无缓存需求的用户不该上榜：%v", entry)
		}
	}

	// model 面：lb-a 成功 1 / countable 2 → 0.5；无 countable 的模型带 successRateUnavailableReason。
	status, body = it.call(t, "/api/leaderboard?period=daily&scope=model")
	if status != http.StatusOK {
		t.Fatalf("模型榜应 200，实得 %d：%s", status, body)
	}
	entries = decodeEntries(t, body)
	modelA := findEntry(t, entries, "model", "lb-a")
	// daily 下 lb-a 只有今天那一条成功行（40 天前的行与拦截行都不算）→ 1/1。
	if modelA["successRate"].(float64) != 1 || modelA["totalRequests"].(float64) != 1 {
		t.Fatalf("lb-a 当日应为 1 条成功：%v", modelA)
	}
	for _, key := range []string{"rowIdentityBasis", "successRateBasis", "costTokensBasis",
		"basisDisclosureRequired", "totalCostFormatted"} {
		if _, ok := modelA[key]; !ok {
			t.Fatalf("模型行缺字段 %s：%v", key, modelA)
		}
	}
}

func TestLeaderboardValidationIntegration(t *testing.T) {
	pools := endpointWritePools(t)
	it := seedLeaderboard(t, pools, false)

	cases := []struct {
		name    string
		path    string
		message string
	}{
		{"周期非法", "/api/leaderboard?period=hourly",
			"参数 period 必须是 daily, weekly, monthly, allTime, custom 之一"},
		{"维度非法", "/api/leaderboard?scope=providerX",
			"参数 scope 必须是 'user'、'userCacheHitRate'、'provider'、'providerCacheHitRate' 或 'model'"},
		{"custom 缺参数", "/api/leaderboard?period=custom", "当 period=custom 时，必须提供 startDate 和 endDate 参数"},
		{"日期格式", "/api/leaderboard?period=custom&startDate=2026-1-1&endDate=2026-01-02",
			"日期格式必须是 YYYY-MM-DD"},
		{"起止倒置", "/api/leaderboard?period=custom&startDate=2026-02-01&endDate=2026-01-01",
			"startDate 不能大于 endDate"},
	}
	for _, testCase := range cases {
		status, body := it.call(t, testCase.path)
		if status != http.StatusBadRequest || !strings.Contains(body, `"error":"`+testCase.message+`"`) {
			t.Fatalf("[%s] 应 400 且正文为 %q，实得 %d：%s",
				testCase.name, testCase.message, status, body)
		}
	}

	// 缺省参数可用：period=daily、scope=user。
	status, body := it.call(t, "/api/leaderboard")
	if status != http.StatusOK {
		t.Fatalf("缺省参数应 200，实得 %d：%s", status, body)
	}
	if !strings.Contains(body, it.prefix+"-a") {
		t.Fatalf("缺省参数应回今日的用户榜（含夹具用户）：%s", body)
	}
}

func TestLeaderboardPermissionIntegration(t *testing.T) {
	pools := endpointWritePools(t)
	it := seedLeaderboard(t, pools, false)

	settings, err := pools.FindSystemSettings(context.Background())
	if err != nil {
		t.Fatalf("读设置失败: %v", err)
	}
	allowGlobal := settings != nil && settings.AllowGlobalUsageView

	// 非管理员：是否放行由 allowGlobalUsageView 决定（Node 同判）。
	deps := Deps{
		Logger: logx.New(nil),
		Guard: principalGuard{principal: Principal{
			UserID: it.userBID, Username: "viewer", IsAdmin: false,
		}},
		Store: pools,
	}
	router := New(Options{Deps: deps})
	RegisterLeaderboardRoutes(router, deps, nil)
	status, body := call(router, http.MethodGet, "/api/leaderboard?period=daily&scope=user", "")
	if !allowGlobal {
		if status != http.StatusForbidden ||
			!strings.Contains(body, "无权限访问排行榜，请联系管理员开启全站使用权限") {
			t.Fatalf("未开全站可见时应 403 并给出原文，实得 %d：%s", status, body)
		}
	} else {
		if status != http.StatusOK {
			t.Fatalf("开了全站可见时非管理员应 200，实得 %d：%s", status, body)
		}
		// includeUserModelStats 是管理员专属：非管理员带这个开关一律 403 专属码。
		status, body = call(router, http.MethodGet,
			"/api/leaderboard?period=daily&scope=user&includeUserModelStats=1", "")
		if status != http.StatusForbidden ||
			!strings.Contains(body, "INCLUDE_USER_MODEL_STATS_ADMIN_REQUIRED") {
			t.Fatalf("非管理员请求用户级模型拆分应 403 专属码，实得 %d：%s", status, body)
		}
	}
}

func TestLeaderboardCacheIntegration(t *testing.T) {
	pools := endpointWritePools(t)
	it := seedLeaderboard(t, pools, true)

	path := "/api/leaderboard?period=daily&scope=user"
	status, firstBody := it.call(t, path)
	if status != http.StatusOK {
		t.Fatalf("首次请求应 200，实得 %d：%s", status, firstBody)
	}
	if it.cache.gets != 1 || it.cache.sets != 1 {
		t.Fatalf("首次应读一次写一次缓存，实得 get=%d set=%d", it.cache.gets, it.cache.sets)
	}
	key := it.cache.lastKey
	if !strings.HasPrefix(key, "leaderboard:v4:user:daily:") || !strings.Contains(key, ":tz:") {
		t.Fatalf("缓存键不符合 Node 的键方案：%s", key)
	}

	// 第二次同键：命中缓存，不再查库（写次数不增）。
	status, body := it.call(t, path)
	if status != http.StatusOK {
		t.Fatalf("第二次请求应 200，实得 %d：%s", status, body)
	}
	if body != firstBody {
		t.Fatalf("命中缓存的正文应与首次逐字一致：\n%s\n%s", firstBody, body)
	}
	if it.cache.gets != 2 || it.cache.sets != 1 {
		t.Fatalf("命中缓存时不应回写，实得 get=%d set=%d", it.cache.gets, it.cache.sets)
	}
	if !strings.Contains(body, it.prefix+"-a") {
		t.Fatalf("命中的作答应含夹具用户：%s", body)
	}

	// 换 scope / 换开关：键必须不同（否则会串答案）。
	status, body = it.call(t, "/api/leaderboard?period=daily&scope=provider")
	if status != http.StatusOK {
		t.Fatalf("换 scope 应 200，实得 %d：%s", status, body)
	}
	if it.cache.lastKey == key {
		t.Fatalf("不同 scope 必须用不同的缓存键：%s", it.cache.lastKey)
	}
	status, _ = it.call(t, "/api/leaderboard?period=daily&scope=user&includeUserModelStats=1")
	if status != http.StatusOK {
		t.Fatalf("带模型拆分应 200，实得 %d", status)
	}
	if it.cache.lastKey == key {
		t.Fatalf("带 includeUserModelStats 必须另用缓存键：%s", it.cache.lastKey)
	}
}

func TestLeaderboardFormatCurrency(t *testing.T) {
	// formatCurrency 的口径：HALF_UP 两位 + 按 locale 分组（只有 de-DE 与其余不同）。
	cases := []struct {
		value    float64
		currency string
		want     string
	}{
		{0, "USD", "$0.00"},
		{1.5, "USD", "$1.50"},
		{1234.567, "USD", "$1,234.57"},
		{1234567.891, "USD", "$1,234,567.89"},
		{1234.56, "EUR", "€1.234,56"},
		{1234.56, "CNY", "¥1,234.56"},
		{1234.56, "KRW", "₩1,234.56"},
		// 1.005 是最容易写错的一位：先乘 100 再 math.Round 会因二进制表示得 1.00。
		{1.005, "USD", "$1.01"},
		{-1234.567, "USD", "$-1,234.57"},
		{0.0000005, "USD", "$0.00"},
	}
	for _, testCase := range cases {
		if got := leaderboardFormatCurrency(testCase.value, testCase.currency); got != testCase.want {
			t.Fatalf("formatCurrency(%v, %s) = %q，期望 %q",
				testCase.value, testCase.currency, got, testCase.want)
		}
	}
	// 未知币种回退 USD（登记差异：Node 这里会 500）。
	if got := leaderboardFormatCurrency(1, "XXX"); got != "$1.00" {
		t.Fatalf("未知币种应回退 USD：%q", got)
	}
}
