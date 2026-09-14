package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
)

// integrationDSN 读取门控变量；未设置时整组集成测试跳过。
func integrationDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("CCH_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 CCH_TEST_DSN，跳过数据库集成测试")
	}
	return dsn
}

func openTestPools(t *testing.T) *Pools {
	t.Helper()
	pools, err := Open(context.Background(), Options{
		DSN:      integrationDSN(t),
		Budget:   config.SplitPoolBudget(6),
		Timeouts: config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("建立连接池失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	return pools
}

// itKey 为本次集成测试生成唯一标记，用于事后精确清理，避免污染既有数据。
func itKey(t *testing.T) string {
	t.Helper()
	return "go-store-it-" + time.Now().Format("20060102150405.000000000")
}

func createRequestFixture(t *testing.T, pools *Pools, key string, isReplay bool) MessageRequest {
	t.Helper()
	ctx := context.Background()
	cost := "0.000000000000000"
	model := "go-store-it-model"
	endpoint := "/v1/messages"
	request, err := pools.CreateMessageRequest(ctx, CreateMessageRequestData{
		ProviderID:   1,
		UserID:       1,
		Key:          key,
		Model:        &model,
		Endpoint:     &endpoint,
		CostUSD:      &cost,
		IsReplay:     isReplay,
		RoutingTrace: []byte(`{"version":1}`),
	})
	if err != nil {
		t.Fatalf("创建 message_request 失败: %v", err)
	}
	if request.ID == 0 {
		t.Fatal("创建后应当返回自增 id")
	}
	return request
}

func cleanupRequestRows(t *testing.T, pools *Pools, keys []string) {
	t.Helper()
	ctx := context.Background()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	for _, key := range keys {
		// usage_ledger 与 proj_applied_requests 由触发器/测试写入，一并按 request_id 清理。
		if _, err := pool.Exec(ctx,
			`DELETE FROM proj_applied_requests WHERE request_id IN (
				SELECT id FROM message_request WHERE key = $1)`, key); err != nil {
			t.Fatalf("清理 proj_applied_requests 失败: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`DELETE FROM usage_ledger WHERE request_id IN (
				SELECT id FROM message_request WHERE key = $1)`, key); err != nil {
			t.Fatalf("清理 usage_ledger 失败: %v", err)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM message_request WHERE key = $1`, key); err != nil {
			t.Fatalf("清理 message_request 失败: %v", err)
		}
	}
}

func TestIntegrationCreateAndIfUnfinalized(t *testing.T) {
	pools := openTestPools(t)
	key := itKey(t)
	t.Cleanup(func() { cleanupRequestRows(t, pools, []string{key}) })

	created := createRequestFixture(t, pools, key, false)
	if created.StatusCode != nil {
		t.Fatalf("新建行不应有终态，实际 %v", *created.StatusCode)
	}

	ctx := context.Background()
	durationMS := 1234
	statusCode := 200
	first := DetailsPatch{DurationMS: &durationMS, StatusCode: &statusCode}
	won, err := pools.UpdateDetailsIfUnfinalized(ctx, created.ID, first)
	if err != nil {
		t.Fatalf("首次终态更新失败: %v", err)
	}
	if !won {
		t.Fatal("首次终态更新应当赢得唯一终态")
	}

	// 第二次必须落空：幂等谓词 status_code IS NULL 已不成立。
	second := DetailsPatch{StatusCode: &statusCode}
	won, err = pools.UpdateDetailsIfUnfinalized(ctx, created.ID, second)
	if err != nil {
		t.Fatalf("第二次终态更新报错: %v", err)
	}
	if won {
		t.Fatal("重复终态写入必须返回 false，否则会重复计费")
	}

	// 不存在的 id 同样返回 false，而不是报错。
	// 注意 id 列是 int4：取 int64 上限会因超出范围在编码阶段报错，而不是走到「无行」分支。
	won, err = pools.UpdateDetailsIfUnfinalized(ctx, 2147483647, second)
	if err != nil {
		t.Fatalf("不存在的 id 不应报错: %v", err)
	}
	if won {
		t.Fatal("不存在的 id 必须返回 false")
	}
}

func TestIntegrationHedgeLoserCostIsIdempotent(t *testing.T) {
	pools := openTestPools(t)
	key := itKey(t)
	t.Cleanup(func() { cleanupRequestRows(t, pools, []string{key}) })

	created := createRequestFixture(t, pools, key, false)
	ctx := context.Background()
	entry := HedgeLoserEntry{ProviderID: 7, AttemptNumber: 1}

	if err := pools.AddHedgeLoserCost(ctx, created.ID, "0.5", entry); err != nil {
		t.Fatalf("首次累加输家成本失败: %v", err)
	}
	// 同一输家重复投递必须被去重谓词挡住。
	if err := pools.AddHedgeLoserCost(ctx, created.ID, "0.5", entry); err != nil {
		t.Fatalf("重复累加输家成本失败: %v", err)
	}
	// 另一个 attempt 是不同输家，应当照常累加。
	if err := pools.AddHedgeLoserCost(ctx, created.ID, "0.25", HedgeLoserEntry{ProviderID: 7, AttemptNumber: 2}); err != nil {
		t.Fatalf("第二个输家累加失败: %v", err)
	}

	cost, err := pools.FindCostUSD(ctx, created.ID)
	if err != nil {
		t.Fatalf("读取成本失败: %v", err)
	}
	if formatted, ok := FormatCostForStorage("0.75"); !ok || trimCost(cost) != trimCost(formatted) {
		t.Fatalf("输家成本合计 = %q, want 0.75", cost)
	}
}

func TestIntegrationWinnerCostAddsLosses(t *testing.T) {
	pools := openTestPools(t)
	key := itKey(t)
	t.Cleanup(func() { cleanupRequestRows(t, pools, []string{key}) })

	created := createRequestFixture(t, pools, key, false)
	ctx := context.Background()

	if err := pools.AddHedgeLoserCost(ctx, created.ID, "0.5", HedgeLoserEntry{ProviderID: 3, AttemptNumber: 1}); err != nil {
		t.Fatalf("累加输家成本失败: %v", err)
	}
	if err := pools.UpdateWinnerCost(ctx, created.ID, "1.25", []byte(`{"total":1.25}`)); err != nil {
		t.Fatalf("写入 winner 成本失败: %v", err)
	}

	cost, err := pools.FindCostUSD(ctx, created.ID)
	if err != nil {
		t.Fatalf("读取成本失败: %v", err)
	}
	// winner 1.25 + loser 0.5 = 1.75（与 TS 的 cost_usd + SUM(hedge_losers) 语义一致）。
	if formatted, ok := FormatCostForStorage("1.75"); !ok || trimCost(cost) != trimCost(formatted) {
		t.Fatalf("winner 成本 = %q, want 1.75", cost)
	}
}

func TestIntegrationUpdateCostSkipsInvalidValue(t *testing.T) {
	pools := openTestPools(t)
	key := itKey(t)
	t.Cleanup(func() { cleanupRequestRows(t, pools, []string{key}) })

	created := createRequestFixture(t, pools, key, false)
	if err := pools.UpdateCost(context.Background(), created.ID, "not-a-number"); err != nil {
		t.Fatalf("非法成本应当被静默跳过，而不是报错: %v", err)
	}
}

func TestIntegrationProjectionDedupe(t *testing.T) {
	pools := openTestPools(t)
	key := itKey(t)
	t.Cleanup(func() { cleanupRequestRows(t, pools, []string{key}) })

	created := createRequestFixture(t, pools, key, false)
	ctx := context.Background()
	eventID := "3f2504e0-4f89-11d3-9a0c-0305e82c3301"

	fresh, err := pools.InsertProjAppliedRequest(ctx, created.ID, eventID)
	if err != nil {
		t.Fatalf("首次投影写入失败: %v", err)
	}
	if !fresh {
		t.Fatal("首次投影应当判定为新应用")
	}
	fresh, err = pools.InsertProjAppliedRequest(ctx, created.ID, eventID)
	if err != nil {
		t.Fatalf("重复投影写入失败: %v", err)
	}
	if fresh {
		t.Fatal("同一 request_id 重复投递必须判定为已应用（幂等键是 request_id）")
	}
}

func TestIntegrationLedgerSumsRespectBillingCondition(t *testing.T) {
	pools := openTestPools(t)
	key := itKey(t)
	t.Cleanup(func() { cleanupRequestRows(t, pools, []string{key}) })

	ctx := context.Background()
	billable := createRequestFixture(t, pools, key, false)
	replayKey := key + "-replay"
	replay := createRequestFixture(t, pools, replayKey, true)
	t.Cleanup(func() { cleanupRequestRows(t, pools, []string{replayKey}) })

	// 触发器 trg_upsert_usage_ledger 会把 message_request 落到 usage_ledger；
	// 这里只断言「可计费口径排除了 replay 行」这一条，不依赖具体行数。
	if err := pools.UpdateCost(ctx, billable.ID, "2"); err != nil {
		t.Fatalf("写入成本失败: %v", err)
	}
	if err := pools.UpdateCost(ctx, replay.ID, "3"); err != nil {
		t.Fatalf("写入 replay 成本失败: %v", err)
	}

	start := time.Now().Add(-time.Hour)
	end := time.Now().Add(time.Hour)
	count, err := pools.CountLedgerRequestsInTimeRange(ctx, LedgerEntityKey, key, start, end)
	if err != nil {
		t.Fatalf("统计可计费请求数失败: %v", err)
	}
	if count < 1 {
		t.Fatalf("可计费请求数 = %d，触发器未把 message_request 写入 usage_ledger？", count)
	}
	replayCount, err := pools.CountLedgerRequestsInTimeRange(ctx, LedgerEntityKey, replayKey, start, end)
	if err != nil {
		t.Fatalf("统计 replay 请求数失败: %v", err)
	}
	if replayCount != 0 {
		t.Fatalf("replay 行不得进入可计费口径，实际计入 %d 条", replayCount)
	}
}

func TestIntegrationReadPaths(t *testing.T) {
	pools := openTestPools(t)
	ctx := context.Background()

	settings, err := pools.FindSystemSettings(ctx)
	if err != nil {
		t.Fatalf("读取 system_settings 失败: %v", err)
	}
	if settings.ID == 0 {
		t.Fatal("system_settings 应当有单行")
	}

	providers, err := pools.FindEnabledProviders(ctx)
	if err != nil {
		t.Fatalf("读取启用态供应商失败: %v", err)
	}
	for _, provider := range providers {
		if !provider.IsEnabled {
			t.Fatalf("查询返回了未启用供应商: %+v", provider)
		}
	}

	keys, err := pools.FindKeyByValue(ctx, "definitely-not-a-real-key")
	if err != nil && err != ErrNotFound {
		t.Fatalf("按密钥查询应当返回 ErrNotFound 而非查询错误: %v", err)
	}
	if err == nil {
		t.Fatalf("不存在的密钥不应命中: %+v", keys)
	}
}

func trimCost(value string) string {
	// numeric 读出可能带尾部零或整型表现形式，比较前统一裁成不带尾部零的形式。
	trimmed := value
	for len(trimmed) > 0 && trimmed[len(trimmed)-1] == '0' {
		trimmed = trimmed[:len(trimmed)-1]
	}
	if len(trimmed) > 0 && trimmed[len(trimmed)-1] == '.' {
		trimmed = trimmed[:len(trimmed)-1]
	}
	return trimmed
}
