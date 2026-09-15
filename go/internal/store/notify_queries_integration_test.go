package store

import (
	"context"
	"testing"
	"time"
)

// 本文件是「通知数据生成器」取数面的真库用例（`CCH_TEST_DSN` 未设置时整组跳过）。
//
// 只有真库能证明的部分：
//   - 缓存口径 SQL 的 eligible 判定（prev 行 join、gap <= TTL、warmup/replay/软删/状态码过滤）；
//   - 「按供应商×模型」分组的定点口径（分母 = input + cache_creation + cache_read）；
//   - 限额列的 numeric 过滤（> 0）与文本读回。
//
// 期望值都按 Node 源码手推（src/repository/cache-hit-rate-alert.ts:113-362 与
// src/lib/notification/tasks/cost-alert.ts:62-76、181-191），不是把 Go 的输出回抄。
//
// 夹具纪律（与其它 store 集成用例同一条）：合成 provider_id 远离真实供应商、key 带唯一前缀、
// 按前缀精确清理；插 message_request 会触发账本与 outbox 触发器，删夹具行时触发器一并回收。
const (
	notifyQueriesITKeyPrefix = "go-notify-it-"
	// notifyQueriesITProviderID 是合成供应商 id：远离真实供应商 id，便于断言与清理。
	notifyQueriesITProviderID int64 = 990201
)

type notifyCacheFixtureRow struct {
	session   string
	sequence  int
	createdAt time.Time
	input     int
	read      int
	status    int
	blockedBy string
}

// insertNotifyQueriesFixtures 插入供应商行与一批 message_request 行。
func insertNotifyQueriesFixtures(
	t *testing.T,
	pools *Pools,
	key string,
	model string,
	rows []notifyCacheFixtureRow,
) {
	t.Helper()
	ctx := context.Background()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取连接失败: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if _, err := pool.Exec(cleanupCtx,
			`DELETE FROM message_request WHERE key LIKE $1`, notifyQueriesITKeyPrefix+"%"); err != nil {
			t.Logf("清理夹具请求行失败: %v", err)
		}
		if _, err := pool.Exec(cleanupCtx,
			`DELETE FROM providers WHERE id = $1`, notifyQueriesITProviderID); err != nil {
			t.Logf("清理夹具供应商失败: %v", err)
		}
	})

	// openai-compatible 的 TTL fallback 是 600 秒（Node：ttlFallbackSecondsByProviderType）。
	if _, err := pool.Exec(ctx, `
		INSERT INTO providers (id, name, url, key, provider_type, is_enabled)
		VALUES ($1, $2, $3, $4, 'openai-compatible', true)
		ON CONFLICT (id) DO UPDATE SET provider_type = 'openai-compatible', deleted_at = NULL`,
		notifyQueriesITProviderID, "go-notify-it 供应商", "https://notify.example.test", "sk-notify-it",
	); err != nil {
		t.Fatalf("插入夹具供应商失败: %v", err)
	}

	for _, row := range rows {
		var blockedBy any
		if row.blockedBy != "" {
			blockedBy = row.blockedBy
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO message_request (
				provider_id, user_id, key, model, session_id, request_sequence,
				input_tokens, cache_read_input_tokens, status_code, blocked_by,
				is_replay, endpoint, created_at
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, false, '/v1/messages', $11)`,
			notifyQueriesITProviderID, int64(1), key, model, row.session, row.sequence,
			row.input, row.read, row.status, blockedBy, row.createdAt,
		); err != nil {
			t.Fatalf("插入夹具请求行失败: %v", err)
		}
	}
}

// TestIntegrationNotifyProviderModelCacheMetricsEligibility 钉住 eligible 判定与分组口径。
//
// 夹具（T = 窗口右端，provider_type=openai-compatible → TTL fallback 600 秒）：
//
//	会话 SC：seq1 在 T-2000s、seq2 在 T-100s（gap 1900 > 600 → 不 eligible）、seq3 在 T-90s（gap 10 → eligible）
//	会话 SB：seq1 在 T-60s（无 prev → 不 eligible）、seq2 在 T-30s（gap 30 → eligible）
//	会话 SW：两行 blocked_by='warmup' → 整行排除
//	会话 SE：一行 status_code=500 → 整行排除
//
// 手推（Node 同一段 SQL）：
//
//	participating rows = SC1、SC2、SC3、SB1、SB2 → totalRequests = 5
//	denominatorTokens = 10 + 25 + 40 + 40 + 75 = 190，cacheRead = 0+5+10+0+25 = 40
//	hitRateTokens = 40/190
//	eligible = SC3（40/10）、SB2（75/25）→ eligibleRequests = 2
//	eligibleDenominatorTokens = 115，eligibleCacheReadTokens = 35 → hitRateTokensEligible = 35/115
//	cacheSignalRequests = 3（SC2、SC3、SB2 有 cache_read）→ engagementRate = 3/5
func TestIntegrationNotifyProviderModelCacheMetricsEligibility(t *testing.T) {
	pools := openTestPools(t)
	key := notifyQueriesITKey(t)
	model := notifyQueriesITKeyPrefix + "model"
	now := time.Now().UTC().Truncate(time.Second)

	insertNotifyQueriesFixtures(t, pools, key, model, []notifyCacheFixtureRow{
		{session: "SC", sequence: 1, createdAt: now.Add(-2000 * time.Second), input: 10, status: 200},
		{session: "SC", sequence: 2, createdAt: now.Add(-100 * time.Second), input: 20, read: 5, status: 200},
		{session: "SC", sequence: 3, createdAt: now.Add(-90 * time.Second), input: 30, read: 10, status: 200},
		{session: "SB", sequence: 1, createdAt: now.Add(-60 * time.Second), input: 40, status: 200},
		{session: "SB", sequence: 2, createdAt: now.Add(-30 * time.Second), input: 50, read: 25, status: 200},
		{session: "SW", sequence: 1, createdAt: now.Add(-600 * time.Second), input: 7, status: 200, blockedBy: "warmup"},
		{session: "SW", sequence: 2, createdAt: now.Add(-590 * time.Second), input: 8, read: 4, status: 200, blockedBy: "warmup"},
		{session: "SE", sequence: 1, createdAt: now.Add(-20 * time.Second), input: 100, status: 500},
	})

	metrics, err := pools.NotifyProviderModelCacheMetrics(
		context.Background(), now.Add(-time.Hour), now.Add(time.Second), "redirected",
	)
	if err != nil {
		t.Fatalf("查询缓存口径失败: %v", err)
	}
	metric, ok := findNotifyMetric(metrics, notifyQueriesITProviderID, model)
	if !ok {
		t.Fatalf("未取到夹具分组的行: %+v", metrics)
	}

	almostEqual := func(name string, got, want float64) {
		t.Helper()
		if diff := got - want; diff > 1e-9 || diff < -1e-9 {
			t.Errorf("%s = %v，期望 %v", name, got, want)
		}
	}
	almostEqual("totalRequests", metric.TotalRequests, 5)
	almostEqual("denominatorTokens", metric.DenominatorTokens, 190)
	almostEqual("sumCacheReadTokens", metric.SumCacheReadTokens, 40)
	almostEqual("hitRateTokens", metric.HitRateTokens, 40.0/190.0)
	almostEqual("eligibleRequests", metric.EligibleRequests, 2)
	almostEqual("eligibleDenominatorTokens", metric.EligibleDenominatorTokens, 115)
	almostEqual("eligibleCacheReadTokens", metric.EligibleCacheReadTokens, 35)
	almostEqual("hitRateTokensEligible", metric.HitRateTokensEligible, 35.0/115.0)
	almostEqual("cacheSignalRequests", metric.CacheSignalRequests, 3)
	almostEqual("engagementRate", metric.EngagementRate, 0.6)
	if metric.ProviderType != "openai-compatible" {
		t.Errorf("providerType = %q", metric.ProviderType)
	}
}

// TestIntegrationNotifyCacheMetricsExcludesSoftDeletedProvider 钉住 inner join 的软删过滤
// （Node：innerJoin providers on ... isNull(providers.deletedAt)）。
func TestIntegrationNotifyCacheMetricsExcludesSoftDeletedProvider(t *testing.T) {
	pools := openTestPools(t)
	key := notifyQueriesITKey(t)
	model := notifyQueriesITKeyPrefix + "soft-deleted"
	now := time.Now().UTC().Truncate(time.Second)
	insertNotifyQueriesFixtures(t, pools, key, model, []notifyCacheFixtureRow{
		{session: "SA", sequence: 1, createdAt: now.Add(-30 * time.Second), input: 10, status: 200},
	})

	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取连接失败: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE providers SET deleted_at = now() WHERE id = $1`, notifyQueriesITProviderID); err != nil {
		t.Fatalf("软删夹具供应商失败: %v", err)
	}

	metrics, err := pools.NotifyProviderModelCacheMetrics(
		context.Background(), now.Add(-time.Hour), now.Add(time.Second), "redirected",
	)
	if err != nil {
		t.Fatalf("查询缓存口径失败: %v", err)
	}
	if _, ok := findNotifyMetric(metrics, notifyQueriesITProviderID, model); ok {
		t.Fatalf("软删供应商不该出现在缓存口径里: %+v", metrics)
	}
}

// TestIntegrationNotifyCostLimitQueries 钉住限额查询的过滤与文本读回。
//
// Node：keys 的过滤是「三档任一 > 0」（tasks/cost-alert.ts:74-76），providers 是「周/月任一 > 0」
// （同文件 191 行）；两者都**不筛软删**。
func TestIntegrationNotifyCostLimitQueries(t *testing.T) {
	pools := openTestPools(t)
	key := notifyQueriesITKey(t)
	ctx := context.Background()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取连接失败: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		if _, err := pool.Exec(cleanupCtx, `DELETE FROM keys WHERE key LIKE $1`, notifyQueriesITKeyPrefix+"%"); err != nil {
			t.Logf("清理夹具密钥失败: %v", err)
		}
		if _, err := pool.Exec(cleanupCtx, `DELETE FROM providers WHERE id = $1`, notifyQueriesITProviderID); err != nil {
			t.Logf("清理夹具供应商失败: %v", err)
		}
	})

	// 两把密钥：一把有 5h 限额（应为限额文本 "12.50"），一把三档全 0（不得出现）。
	if _, err := pool.Exec(ctx, `
		INSERT INTO keys (user_id, key, name, limit_5h_usd, limit_weekly_usd, limit_monthly_usd)
		VALUES ($1, $2, $3, 12.50, 0, 0), ($1, $4, $5, 0, 0, 0)`,
		int64(1), key, "go-notify-it 甲", key+"-zero", "go-notify-it 乙",
	); err != nil {
		t.Fatalf("插入夹具密钥失败: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO providers (id, name, url, key, provider_type, limit_weekly_usd, limit_monthly_usd)
		VALUES ($1, $2, $3, $4, 'openai-compatible', 30, NULL)`,
		notifyQueriesITProviderID, "go-notify-it 供应商", "https://notify.example.test", "sk-notify-it",
	); err != nil {
		t.Fatalf("插入夹具供应商失败: %v", err)
	}

	keys, err := pools.NotifyKeysWithCostLimits(ctx)
	if err != nil {
		t.Fatalf("查询密钥限额失败: %v", err)
	}
	found := false
	for _, item := range keys {
		if item.Key == key {
			found = true
			if item.UserName != "go-notify-it 甲" {
				t.Errorf("姓名读回不对: %q", item.UserName)
			}
			if item.Limit5h == nil || *item.Limit5h != "12.50" {
				t.Errorf("限额应读回 numeric 文本 12.50，实际 %v", item.Limit5h)
			}
		}
		if item.Key == key+"-zero" {
			t.Errorf("三档限额全为 0 的密钥不该出现")
		}
	}
	if !found {
		t.Fatalf("未取到夹具密钥: %+v", keys)
	}

	providers, err := pools.NotifyProvidersWithCostLimits(ctx)
	if err != nil {
		t.Fatalf("查询供应商限额失败: %v", err)
	}
	found = false
	for _, item := range providers {
		if item.ID == notifyQueriesITProviderID {
			found = true
			if item.LimitWeek == nil || *item.LimitWeek != "30.00" {
				t.Errorf("供应商周限额应读回 30.00，实际 %v", item.LimitWeek)
			}
			if item.LimitMonth != nil {
				t.Errorf("月限额为空时应为 nil，实际 %v", *item.LimitMonth)
			}
		}
	}
	if !found {
		t.Fatalf("未取到夹具供应商: %+v", providers)
	}

	refs, err := pools.NotifyProviderRefs(ctx)
	if err != nil {
		t.Fatalf("查询供应商投影失败: %v", err)
	}
	// 软删后不再出现（Node 的 findAllProvidersFresh 带 isNull(deletedAt)）。
	if _, err := pool.Exec(ctx, `UPDATE providers SET deleted_at = now() WHERE id = $1`, notifyQueriesITProviderID); err != nil {
		t.Fatalf("软删夹具供应商失败: %v", err)
	}
	refs, err = pools.NotifyProviderRefs(ctx)
	if err != nil {
		t.Fatalf("查询供应商投影失败: %v", err)
	}
	for _, ref := range refs {
		if ref.ID == notifyQueriesITProviderID {
			t.Errorf("软删供应商不该出现在投影里")
		}
	}
}

// notifyQueriesITKey 生成带本文件专属前缀的唯一 key：清理按该前缀精确删除。
func notifyQueriesITKey(t *testing.T) string {
	t.Helper()
	return notifyQueriesITKeyPrefix + time.Now().Format("20060102150405.000000000")
}

func findNotifyMetric(
	metrics []NotifyCacheMetric,
	providerID int64,
	model string,
) (NotifyCacheMetric, bool) {
	for _, metric := range metrics {
		if metric.ProviderID == providerID && metric.Model == model {
			return metric, true
		}
	}
	return NotifyCacheMetric{}, false
}
