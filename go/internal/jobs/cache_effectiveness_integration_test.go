package jobs

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件覆盖只有真库才能证明的两件事：
//  1. `RunOnce` 端到端（读开关 → 取锁 → 算窗口 → 聚合写库），以及**连续两轮不重复计数**
//     —— 不重叠靠「下一轮起点 = 上一轮终点」保证，而不是靠 upsert 去重（Node 也是普通 INSERT）；
//  2. 锁被另一连接持有时**整轮跳过**（Node 同样不排队）。
//
// 夹具纪律与 internal/store 的集成用例一致：合成 provider_id + 唯一 key，并按标记精确清理。
// 聚合 SQL 的定点数学与窗口边界在 store 侧用例里逐格钉死，这里只做端到端与不变量。
const cacheEffectivenessJobsITProviderID int64 = 993001

func cacheEffectivenessJobsITCleanup(t *testing.T, pools *store.Pools, keyPrefix, model string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		pool, err := pools.Writer()
		if err != nil {
			t.Logf("清理时取连接失败（夹具残留需人工清理）: %v", err)
			return
		}
		if _, err := pool.Exec(ctx, `DELETE FROM message_request WHERE key LIKE $1`, keyPrefix+"%"); err != nil {
			t.Logf("清理夹具请求行失败: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`DELETE FROM provider_cache_effectiveness WHERE provider_id = $1 AND model = $2`,
			cacheEffectivenessJobsITProviderID, model); err != nil {
			t.Logf("清理聚合行失败: %v", err)
		}
	})
}

func insertCacheEffectivenessJobsITRow(
	t *testing.T,
	pools *store.Pools,
	key string,
	model string,
	observed int64,
	createdAt time.Time,
) {
	t.Helper()
	ctx := context.Background()
	// 写路径走 Writer 道：与生产一致（请求行由写分道落库），且读路径的控制道只有 1 条连接，
	// 在持事务的用例里不该再占用它。
	pool, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写连接失败: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO message_request (
			provider_id, user_id, key, model,
			cache_ttl_bucket, cache_compatibility_key,
			cache_score_eligible, theoretical_cache_tokens, cache_read_input_tokens,
			created_at
		) VALUES ($1, $2, $3, $4, '5m', $5, true, 100, $6, $7)`,
		cacheEffectivenessJobsITProviderID, int64(1), key, model, "ck-"+key, observed, createdAt,
	); err != nil {
		t.Fatalf("插入夹具行失败: %v", err)
	}
}

// cacheEffectivenessJobsITWindow 复刻 RunOnce 的窗口算法，供用例构造「落在下一窗口内」的夹具数据。
//
// 与实现同算法（而不是另写一套期望）：这里要的是「夹具落在窗口内」这一事实，
// 窗口算法本身由无库单测逐边界钉住。
func cacheEffectivenessJobsITWindow(t *testing.T, pools *store.Pools, now time.Time) (time.Time, time.Time, bool) {
	t.Helper()
	ctx := context.Background()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取连接失败: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	lastEnd, err := pools.LastCacheEffectivenessWindowEnd(ctx, tx)
	if err != nil {
		t.Fatalf("读水位失败: %v", err)
	}
	return cacheEffectivenessWindow(lastEnd, now)
}

func readCacheEffectivenessJobsITRow(
	t *testing.T,
	pools *store.Pools,
	model string,
	windowStart time.Time,
	windowEnd time.Time,
) (sample, eligible int, theoretical, observed int64, rawBp, confidenceBp, effectivenessBp int, found bool) {
	t.Helper()
	ctx := context.Background()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取连接失败: %v", err)
	}
	err = pool.QueryRow(ctx, `
		SELECT sample_count, eligible_count, theoretical_cache_tokens, observed_cache_read_tokens,
		       raw_effectiveness_bp, confidence_bp, effectiveness_bp
		FROM provider_cache_effectiveness
		WHERE provider_id = $1 AND model = $2 AND cache_ttl_bucket = '5m'
		  AND window_start = $3 AND window_end = $4`,
		cacheEffectivenessJobsITProviderID, model, windowStart, windowEnd,
	).Scan(&sample, &eligible, &theoretical, &observed, &rawBp, &confidenceBp, &effectivenessBp)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return 0, 0, 0, 0, 0, 0, 0, false
		}
		t.Fatalf("读回聚合行失败: %v", err)
	}
	return sample, eligible, theoretical, observed, rawBp, confidenceBp, effectivenessBp, true
}

func countCacheEffectivenessJobsITRows(t *testing.T, pools *store.Pools, model string) int {
	t.Helper()
	ctx := context.Background()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取连接失败: %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*)::int FROM provider_cache_effectiveness WHERE provider_id = $1 AND model = $2`,
		cacheEffectivenessJobsITProviderID, model).Scan(&count); err != nil {
		t.Fatalf("统计聚合行失败: %v", err)
	}
	return count
}

// TestIntegrationCacheEffectivenessRunOnceAggregatesAndDoesNotDoubleCount 端到端 + 幂等。
//
// 幂等的判据不是「结果不变」（聚合是追加写、窗口逐轮推进），而是**不重复计数**：
// 第二轮起点等于第一轮终点，故同一批请求行只会被聚一次。
func TestIntegrationCacheEffectivenessRunOnceAggregatesAndDoesNotDoubleCount(t *testing.T) {
	pools := integrationPools(t)
	ctx := context.Background()

	// 开关：库里显式关掉时本用例无意义（RunOnce 会整轮跳过），如实跳过而不是强行改库。
	setting, err := pools.CacheEffectivenessEnabledSetting(ctx)
	if err != nil {
		t.Fatalf("读开关失败: %v", err)
	}
	if setting != nil && !*setting {
		t.Skip("测试库的系统设置显式关闭了缓存效果聚合，跳过端到端用例")
	}

	now := time.Now().UTC()
	windowStart, windowEnd, advanced := cacheEffectivenessJobsITWindow(t, pools, now)
	if !advanced || windowEnd.Sub(windowStart) < time.Second {
		t.Skipf("窗口水位过新（start=%s end=%s），无法构造可推进窗口", windowStart, windowEnd)
	}
	seedAt := windowStart.Add(windowEnd.Sub(windowStart) / 2)
	model := fmt.Sprintf("go-cacheeff-job-%d", now.UnixNano())
	keyPrefix := fmt.Sprintf("go-cacheeff-job-it-%d-", now.UnixNano())
	cacheEffectivenessJobsITCleanup(t, pools, keyPrefix, model)

	// 两行夹具：各 theoretical=100、observed=25 → 合计 200/50。
	// raw = 50*10000/200 = 2500；sample=eligible=2 → factor(2)=1000、observable=10000；
	// confidence = 10000*1000/10000 = 1000；effectiveness = 2500*1000/10000 = 250。
	insertCacheEffectivenessJobsITRow(t, pools, keyPrefix+"a", model, 25, seedAt)
	insertCacheEffectivenessJobsITRow(t, pools, keyPrefix+"b", model, 25, seedAt.Add(time.Second))

	job, err := NewCacheEffectiveness(CacheEffectivenessOptions{
		Pools: pools,
		// 固定时钟：窗口必须正好是本用例算出的那一段，否则夹具可能落在窗口外。
		Now:          func() time.Time { return now },
		EnabledByEnv: true,
	})
	if err != nil {
		t.Fatalf("构造任务失败: %v", err)
	}

	first, err := job.RunOnce(ctx)
	if err != nil {
		t.Fatalf("第一轮聚合失败: %v", err)
	}
	if first.Skipped {
		t.Fatalf("第一轮不应跳过：reason=%s", first.Reason)
	}

	sample, eligible, theoretical, observed, rawBp, confidenceBp, effectivenessBp, found :=
		readCacheEffectivenessJobsITRow(t, pools, model, windowStart, windowEnd)
	if !found {
		t.Fatal("夹具分组未写出：窗口起点/终点与聚合写入不一致")
	}
	if sample != 2 || eligible != 2 || theoretical != 200 || observed != 50 {
		t.Fatalf("计数/令牌不符：sample=%d eligible=%d theoretical=%d observed=%d，期望 2/2/200/50",
			sample, eligible, theoretical, observed)
	}
	if rawBp != 2500 || confidenceBp != 1000 || effectivenessBp != 250 {
		t.Fatalf("定点值不符：raw=%d confidence=%d effectiveness=%d，期望 2500/1000/250",
			rawBp, confidenceBp, effectivenessBp)
	}

	// 第二轮：起点 = 上轮终点，夹具行已落在窗口之外 → 不得再写一行、不得重复计数。
	second, err := job.RunOnce(ctx)
	if err != nil {
		t.Fatalf("第二轮聚合失败: %v", err)
	}
	if second.GroupsWritten != 0 {
		t.Fatalf("第二轮不应写入任何分组（窗口已推进），实际 %d", second.GroupsWritten)
	}
	if got := countCacheEffectivenessJobsITRows(t, pools, model); got != 1 {
		t.Fatalf("同一分组被重复写入：%d 行（期望 1）——窗口推进未生效", got)
	}
}

// TestIntegrationCacheEffectivenessRunOnceSkipsWhenLocked 钉住跨会话互斥：另一连接持锁时整轮跳过。
func TestIntegrationCacheEffectivenessRunOnceSkipsWhenLocked(t *testing.T) {
	pools := integrationPools(t)
	ctx := context.Background()

	setting, err := pools.CacheEffectivenessEnabledSetting(ctx)
	if err != nil {
		t.Fatalf("读开关失败: %v", err)
	}
	if setting != nil && !*setting {
		t.Skip("测试库的系统设置显式关闭了缓存效果聚合，跳过锁用例")
	}

	// 持锁方走专用连接（独立单连接池，不计入分道预算），与 jobs.AcquireLeader 同形。
	conn, dedicatedPool, err := pools.OpenDedicatedConn(ctx)
	if err != nil {
		t.Fatalf("取专用连接失败: %v", err)
	}
	t.Cleanup(func() {
		// 顺序要紧：Close 会等连接归还，故先 Release。
		conn.Release()
		dedicatedPool.Close()
	})
	holder, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("开启持锁事务失败: %v", err)
	}
	acquired, err := pools.TryCacheEffectivenessXactLock(ctx, holder)
	if err != nil {
		t.Fatalf("持锁失败: %v", err)
	}
	if !acquired {
		t.Fatal("首次取锁应当成功")
	}

	job, err := NewCacheEffectiveness(CacheEffectivenessOptions{Pools: pools, EnabledByEnv: true})
	if err != nil {
		t.Fatalf("构造任务失败: %v", err)
	}
	result, err := job.RunOnce(ctx)
	if err != nil {
		t.Fatalf("持锁期间的聚合不应报错（跳过是正常路径）: %v", err)
	}
	if !result.Skipped || result.Reason != "locked" {
		t.Fatalf("锁被占用时应整轮跳过，实际 skipped=%v reason=%q", result.Skipped, result.Reason)
	}

	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("释放持锁事务失败: %v", err)
	}
	released, err := job.RunOnce(ctx)
	if err != nil {
		t.Fatalf("释放后的聚合失败: %v", err)
	}
	if released.Reason == "locked" {
		t.Fatal("持锁事务结束后不应再被判为 locked（事务级锁应随事务释放）")
	}
}
