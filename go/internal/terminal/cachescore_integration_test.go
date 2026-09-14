package terminal

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// TestIntegrationCostMultipliersAndCacheScoreColumns 是「使用记录页计费倍率与缓存评分列」
// 的落库证据：这两组列过去在生产里**恒为 NULL**（实测近 2 小时 990 行全空），
// 本用例断言它们在真库上真的写得进去、且账本行跟着更新（两列都在账本触发器的监视列表里）。
//
// 为什么必须打真库：列名、类型转换（numeric）、以及触发器联动都不在纯函数里，
// 只在真库上才成立。
func TestIntegrationCostMultipliersAndCacheScoreColumns(t *testing.T) {
	pools := integrationPools(t)
	ctx := context.Background()
	key := itKey(t)
	model := "gpt-5.6"
	userAgent := "terminal-it-multiplier"
	clientIP := "127.0.0.1"
	endpoint := "/v1/messages"
	originalModel := model

	row, err := pools.CreateMessageRequest(ctx, store.CreateMessageRequestData{
		ProviderID:    1,
		UserID:        1,
		Key:           key,
		Model:         &model,
		OriginalModel: &originalModel,
		UserAgent:     &userAgent,
		ClientIP:      &clientIP,
		Endpoint:      &endpoint,
		MessagesCount: intPtr(1),
	})
	if err != nil {
		t.Fatalf("建行失败: %v", err)
	}
	cleanupRequest(t, pools, row.ID, key)

	cost, err := ComputeCost(CostInput{
		Usage:              Usage{InputTokens: int64Ptr(10), OutputTokens: int64Ptr(10)},
		PriceData:          []byte(`{"input_cost_per_token":0.000004,"output_cost_per_token":0.00002}`),
		ProviderMultiplier: floatPtr(0.3),
		GroupMultiplier:    floatPtr(1.5),
	})
	if err != nil {
		t.Fatalf("计算成本失败: %v", err)
	}

	eligibility := true
	compatibilityKey := "scope-abc:fp-tip"
	ttlBucket := "5m"
	theoretical := int64(250)
	costMultiplier := "0.3"
	groupCostMultiplier := "1.5"

	settler := New(StoreWriter{Pools: pools}, Options{MaxAttempts: 3, Backoff: cacheScoreNoBackoff})
	result, err := settler.Settle(ctx, row.ID, Settlement{
		StatusCode:    200,
		DurationMS:    intPtr(80),
		Usage:         Usage{InputTokens: int64Ptr(10), OutputTokens: int64Ptr(10)},
		Cost:          &cost,
		ProviderChain: []byte(`[{"id":1,"name":"mock-upstream","reason":"request_success","statusCode":200}]`),
		Model:         &model,

		CostMultiplier:         &costMultiplier,
		GroupCostMultiplier:    &groupCostMultiplier,
		CacheCompatibilityKey:  &compatibilityKey,
		CacheScoreEligible:     &eligibility,
		TheoreticalCacheTokens: &theoretical,
		CacheTTLBucket:         &ttlBucket,
	})
	if err != nil {
		t.Fatalf("终态结算失败: %v", err)
	}
	if !result.Committed {
		t.Fatal("首次终态写应当赢得该行")
	}

	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}

	var (
		gotProviderMultiplier *string
		gotGroupMultiplier    *string
		gotCompatibilityKey   *string
		gotEligible           *bool
		gotExcludedReason     *string
		gotTheoretical        *int64
		gotTTLBucket          *string
	)
	if err := pool.QueryRow(ctx,
		`SELECT cost_multiplier::text, group_cost_multiplier::text, cache_compatibility_key,
		        cache_score_eligible, cache_score_excluded_reason, theoretical_cache_tokens,
		        cache_ttl_bucket
		 FROM message_request WHERE id = $1`, row.ID,
	).Scan(&gotProviderMultiplier, &gotGroupMultiplier, &gotCompatibilityKey, &gotEligible,
		&gotExcludedReason, &gotTheoretical, &gotTTLBucket); err != nil {
		t.Fatalf("读回倍率与缓存评分列失败: %v", err)
	}

	if gotProviderMultiplier == nil || !strings.HasPrefix(*gotProviderMultiplier, "0.3") {
		t.Fatalf("cost_multiplier = %v, want 0.3", gotProviderMultiplier)
	}
	if gotGroupMultiplier == nil || !strings.HasPrefix(*gotGroupMultiplier, "1.5") {
		t.Fatalf("group_cost_multiplier = %v, want 1.5", gotGroupMultiplier)
	}
	if gotCompatibilityKey == nil || *gotCompatibilityKey != compatibilityKey {
		t.Fatalf("cache_compatibility_key = %v, want %q", gotCompatibilityKey, compatibilityKey)
	}
	if gotEligible == nil || !*gotEligible {
		t.Fatalf("cache_score_eligible = %v, want true", gotEligible)
	}
	// 合格请求的排除原因必须是 NULL 而不是空串：下游按 NULL 判「无原因」。
	if gotExcludedReason != nil {
		t.Fatalf("cache_score_excluded_reason = %q, want NULL", *gotExcludedReason)
	}
	if gotTheoretical == nil || *gotTheoretical != theoretical {
		t.Fatalf("theoretical_cache_tokens = %v, want %d", gotTheoretical, theoretical)
	}
	if gotTTLBucket == nil || *gotTTLBucket != ttlBucket {
		t.Fatalf("cache_ttl_bucket = %v, want %q", gotTTLBucket, ttlBucket)
	}

	// 账本联动：倍率两列在 trg_upsert_usage_ledger 的监视列表里，终态写必须把值带进账本行。
	// 这一条防的是「列写进 message_request 却没进账本」的半边写入。
	var ledgerProviderMultiplier, ledgerGroupMultiplier *string
	if err := pool.QueryRow(ctx,
		`SELECT cost_multiplier::text, group_cost_multiplier::text
		 FROM usage_ledger WHERE request_id = $1`, row.ID,
	).Scan(&ledgerProviderMultiplier, &ledgerGroupMultiplier); err != nil {
		t.Fatalf("读回账本行失败: %v", err)
	}
	if ledgerProviderMultiplier == nil || !strings.HasPrefix(*ledgerProviderMultiplier, "0.3") {
		t.Fatalf("账本 cost_multiplier = %v, want 0.3（触发器不会无中生有，必须是终态写带过去的）", ledgerProviderMultiplier)
	}
	if ledgerGroupMultiplier == nil || !strings.HasPrefix(*ledgerGroupMultiplier, "1.5") {
		t.Fatalf("账本 group_cost_multiplier = %v, want 1.5", ledgerGroupMultiplier)
	}
}

// TestIntegrationCacheScoreColumnsStayNullWhenAbsent 是上一条的**反证**：
// 不提供这五个字段时，列必须保持 NULL——证明上面的断言不是「反正都会写」的空断言。
func TestIntegrationCacheScoreColumnsStayNullWhenAbsent(t *testing.T) {
	pools := integrationPools(t)
	ctx := context.Background()
	key := itKey(t) + "-absent"
	model := "gpt-5.6"
	endpoint := "/v1/messages"

	row, err := pools.CreateMessageRequest(ctx, store.CreateMessageRequestData{
		ProviderID: 1,
		UserID:     1,
		Key:        key,
		Model:      &model,
		Endpoint:   &endpoint,
	})
	if err != nil {
		t.Fatalf("建行失败: %v", err)
	}
	cleanupRequest(t, pools, row.ID, key)

	settler := New(StoreWriter{Pools: pools}, Options{MaxAttempts: 3, Backoff: cacheScoreNoBackoff})
	if _, err := settler.Settle(ctx, row.ID, Settlement{
		StatusCode: 200,
		Usage:      Usage{InputTokens: int64Ptr(1)},
		Model:      &model,
	}); err != nil {
		t.Fatalf("终态结算失败: %v", err)
	}

	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	var compatibilityKey, ttlBucket, eligible, theoretical *string
	if err := pool.QueryRow(ctx,
		`SELECT cache_compatibility_key, cache_ttl_bucket,
		        cache_score_eligible::text, theoretical_cache_tokens::text
		 FROM message_request WHERE id = $1`, row.ID,
	).Scan(&compatibilityKey, &ttlBucket, &eligible, &theoretical); err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if compatibilityKey != nil || ttlBucket != nil || eligible != nil || theoretical != nil {
		t.Fatalf("未提供 F3b 字段时列必须保持 NULL，实际 compat=%v bucket=%v eligible=%v theoretical=%v",
			compatibilityKey, ttlBucket, eligible, theoretical)
	}
}

func cacheScoreNoBackoff(int) time.Duration { return 0 }
