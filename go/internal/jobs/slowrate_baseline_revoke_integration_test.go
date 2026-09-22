package jobs

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件是真库集成用例：钉住 publishScope 在「计数达标但取行一行都取不到」时**必须撤销旧基线键**
// （第 5 轮审查 P2），以及两个不得混的邻接情形（查询失败保留旧键、A3 仍撤销）。
//
// 为何必须真库真 Redis：分支判据是「取行返回空」，而取行走的是 store 的真查询
// （slowRateSampleCleaningPredicate）；撤销动作是 Redis 的 DEL。两者都是断言对象——替身若能
// 返回空，就恰好绕过了「计数与取行同谓词同分道」这个前提，而该前提正是「不构成抖动式撤销」的
// 唯一依据（见 slowrate_baseline.go 该分支的注释）。
//
// 夹具纪律：本用例**不写 message_request 任何行**，只用合成渠道 id 借「查不到」这件事，故可安全
// 跑在共享测试库上；基线键按 scope 唯一，用完即删。
//
// 门控：CCH_TEST_DSN 与 CCH_TEST_REDIS_URL 缺一即跳过（与 internal/jobs 既有约定一致）。

// 合成渠道 id 与模型键：993xxx 段是 jobs 集成用例的保留段（见 cache_effectiveness_integration_test.go）。
const (
	slowRateBaselineRevokeITProviderID int64 = 993777
	slowRateBaselineRevokeITModelKey         = "go-jobs-it-baseline-revoke"
)

// slowRateBaselineRevokeITNow 取 2020 年：窗口落在这段等于「表里必然没有样本」，与库中现有数据无关。
// 判定不受它影响——本用例直接把「计数结果」当入参交给 publishScope，窗口只决定取行是否为空。
var slowRateBaselineRevokeITNow = time.Date(2020, 1, 8, 0, 0, 0, 0, time.UTC)

func slowRateBaselineRevokeITRedis(t *testing.T) redis.UniversalClient {
	t.Helper()
	raw := os.Getenv("CCH_TEST_REDIS_URL")
	if raw == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过 Redis 集成测试")
	}
	options, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("解析 Redis URL 失败: %v", err)
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("Redis 不可达: %v", err)
	}
	return client
}

// slowRateBaselineRevokeITJob 造一个只用来调 publishScope 的任务实例：渠道枚举缝给空实现
// （本用例不经 RunOnce 的枚举，直接喂 scope）。
func slowRateBaselineRevokeITJob(
	t *testing.T,
	pools *store.Pools,
	client redis.UniversalClient,
) *SlowRateBaseline {
	t.Helper()
	job, err := NewSlowRateBaseline(SlowRateBaselineOptions{
		Pools: pools,
		Redis: client,
		EnabledProviders: func(context.Context) ([]store.SlowRateProviderConfig, error) {
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("构造基线任务失败: %v", err)
	}
	return job
}

// slowRateBaselineRevokeITScope 是固定的合成 scope：键与窗口都由它派生，保证三个用例互不串味。
func slowRateBaselineRevokeITScope(w1Samples, w2Samples int64) store.SlowRateScopeSamples {
	return store.SlowRateScopeSamples{
		ProviderID: slowRateBaselineRevokeITProviderID,
		ModelKey:   slowRateBaselineRevokeITModelKey,
		W1Samples:  w1Samples,
		W2Samples:  w2Samples,
	}
}

// seedOldBaseline 写入一条「旧基线」并登记清理，返回键名与查键闭包。
//
// 先造键再断言撤销，是为了让「撤销真的发生」有反例可辨：若实现只是没写新键，
// 键仍在 ⇒ 用例红；若实现撤了键 ⇒ 绿。没有这条前置，两种实现都是绿。
func seedOldBaseline(
	t *testing.T,
	client redis.UniversalClient,
	scope store.SlowRateScopeSamples,
) string {
	t.Helper()
	ctx := context.Background()
	key := BaselineKey(scope.ProviderID, scope.ModelKey)
	if err := client.Set(ctx, key, `{"median":239.6,"samples":7215}`, time.Hour).Err(); err != nil {
		t.Fatalf("写入旧基线失败: %v", err)
	}
	t.Cleanup(func() { _ = client.Del(context.Background(), key).Err() })
	return key
}

// baselineKeyExists 用后台 ctx 查键：被测调用可能带着已取消的 ctx，键的状态不该受它影响。
func baselineKeyExists(t *testing.T, client redis.UniversalClient, key string) bool {
	t.Helper()
	exists, err := client.Exists(context.Background(), key).Result()
	if err != nil {
		t.Fatalf("查键失败: %v", err)
	}
	return exists == 1
}

// TestIntegrationPublishScopeRevokesOldBaselineOnSuccessfulEmptyRates 是本 P2 的核心正例。
//
// 场景：计数说「该组合有 500 条样本，该发布」，但取行时一条都取不到（并发清理把样本删了）。
// 期望：不发布、不算截断、**旧键被撤销**。旧键若留下，读侧只在「键不存在」时 fail-open，
// 于是 recorder 会继续据这条陈旧中位数判慢、写 state 与会话冷却，直到 7 天 TTL 到期。
func TestIntegrationPublishScopeRevokesOldBaselineOnSuccessfulEmptyRates(t *testing.T) {
	ctx := context.Background()
	pools := integrationPools(t)
	client := slowRateBaselineRevokeITRedis(t)
	scope := slowRateBaselineRevokeITScope(500, 0)
	w1Start := slowRateBaselineRevokeITNow.Add(-slowRateBaselineW1Span)

	// 前置：窗口内确实一行都取不到（用与取行同一条 store 方法核，不另立口径）。
	// 缺这条，「取行取不到」可能只是夹具没造对，断言旧键被撤销就成了假绿。
	rows, err := pools.SlowRateScopeRates(
		ctx, scope.ProviderID, scope.ModelKey, w1Start.UnixMilli(), slowRateBaselineRevokeITNow.UnixMilli(), 10)
	if err != nil {
		t.Fatalf("前置取行失败: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("前置不成立：该 scope 在窗口内应无样本，实得 %d 行", len(rows))
	}

	key := seedOldBaseline(t, client, scope)
	job := slowRateBaselineRevokeITJob(t, pools, client)

	published, truncated, err := job.publishScope(
		ctx, scope, w1Start, slowRateBaselineRevokeITNow.Add(-30*24*time.Hour), slowRateBaselineRevokeITNow,
		store.SlowRateProviderConfig{ProviderID: scope.ProviderID},
	)
	if err != nil {
		t.Fatalf("成功空结果不该报错（应走 revoke 分支）: %v", err)
	}
	if published || truncated {
		t.Fatalf("成功空结果既不该发布也不该算截断: published=%t truncated=%t", published, truncated)
	}
	if baselineKeyExists(t, client, key) {
		t.Fatal("计数达标但取行取不到样本时，旧基线键必须被撤销" +
			"（否则它以 7 天 TTL 继续支配 recorder 的判慢与会话冷却）")
	}
}

// TestIntegrationPublishScopeKeepsOldBaselineOnRateQueryError 是反向钉子。
//
// 场景：取行失败。期望：错误交回调用方（由 RunOnce 记 scope_failed），**旧键保留**——
// 设计稿明定的抖动保护：宁可留着旧基线，也不要在查询抖动时把基线清空。
//
// 为何用「已关闭的池」而不是「已取消的 ctx」逼失败：后者会让**错误实现**里那次顺手撤销的 DEL
// 也一并失败（同一个死 ctx），键照样留着 ⇒ 反例不红，钉子就失去分辨力。已关闭的池给出的是
// 一个**活 ctx 上的读失败**：错误实现若撤销，DEL 会成功、键会消失，本用例随即红。
func TestIntegrationPublishScopeKeepsOldBaselineOnRateQueryError(t *testing.T) {
	ctx := context.Background()
	pools := integrationPools(t)
	client := slowRateBaselineRevokeITRedis(t)
	scope := slowRateBaselineRevokeITScope(500, 0)
	w1Start := slowRateBaselineRevokeITNow.Add(-slowRateBaselineW1Span)

	// 先确认池可用，再关掉它：否则「失败」可能来自夹具而不是被测的那次读。
	if _, err := pools.SlowRateScopeRates(
		ctx, scope.ProviderID, scope.ModelKey, w1Start.UnixMilli(), slowRateBaselineRevokeITNow.UnixMilli(), 1,
	); err != nil {
		t.Fatalf("预热查询失败: %v", err)
	}
	pools.Close()

	key := seedOldBaseline(t, client, scope)
	job := slowRateBaselineRevokeITJob(t, pools, client)

	published, truncated, err := job.publishScope(
		ctx, scope, w1Start, slowRateBaselineRevokeITNow.Add(-30*24*time.Hour), slowRateBaselineRevokeITNow,
		store.SlowRateProviderConfig{ProviderID: scope.ProviderID},
	)
	if err == nil {
		t.Fatal("取行失败必须把错误交回调用方，不得静默当作空结果")
	}
	if published || truncated {
		t.Fatalf("取行失败不该发布也不该算截断: published=%t truncated=%t", published, truncated)
	}
	if !baselineKeyExists(t, client, key) {
		t.Fatal("取行失败时旧基线必须保留（设计稿明定的抖动保护，与成功空结果不可混）")
	}
}

// TestIntegrationPublishScopeStillRevokesOnInsufficientSamples 是回归钉子：A3 语义未变。
//
// 场景：两窗样本都不足且 W1 < 10（长期静默）。期望：仍撤销旧键（本 P2 之前就有的行为）。
func TestIntegrationPublishScopeStillRevokesOnInsufficientSamples(t *testing.T) {
	ctx := context.Background()
	pools := integrationPools(t)
	client := slowRateBaselineRevokeITRedis(t)
	scope := slowRateBaselineRevokeITScope(5, 50)
	w1Start := slowRateBaselineRevokeITNow.Add(-slowRateBaselineW1Span)

	key := seedOldBaseline(t, client, scope)
	job := slowRateBaselineRevokeITJob(t, pools, client)

	published, truncated, err := job.publishScope(
		ctx, scope, w1Start, slowRateBaselineRevokeITNow.Add(-30*24*time.Hour), slowRateBaselineRevokeITNow,
		store.SlowRateProviderConfig{ProviderID: scope.ProviderID},
	)
	if err != nil {
		t.Fatalf("A3 不该报错: %v", err)
	}
	if published || truncated {
		t.Fatalf("A3 不该发布也不该算截断: published=%t truncated=%t", published, truncated)
	}
	if baselineKeyExists(t, client, key) {
		t.Fatal("A3（样本不足）必须撤销旧键——这是本 P2 之前就有的语义，不得被本次改动破坏")
	}
}
