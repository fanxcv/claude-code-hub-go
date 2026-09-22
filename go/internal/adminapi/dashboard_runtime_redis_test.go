package adminapi

import (
	"context"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// 本文件是会话观测读数（dashboard 的并发数 / 供应商插槽数）的**真实 Redis** 集成测试。
//
// 为什么必须有它：`countZSet` 有三处只能靠真 Redis 才验得出的行为——过期成员会被顺手摘除、
// 供应商维度的引用计数 HASH 会同步清理、以及「ZSET 成员存在但 session:{id}:info 已删」时
// 不计入。桩测试只能证明我们调了它，不能证明这三条真的发生。
//
// 夹具自钉：用固定前缀的键名并在测试结束时按名删除；未设置 CCH_TEST_REDIS_URL 时跳过
// （与 auth_issue_redis_test.go 同一门控）。

const dashboardRedisGate = "CCH_TEST_REDIS_URL"

// dashboardCommandCounter 数发往 Redis 的**往返次数**（每个命令与整段 pipeline 各算一次）。
//
// 为什么数往返而不是命令条数：判据是「N 个渠道的往返次数 < N、与渠道数同阶」。
// 命令条数看不出 pipeline 是否真的合并（逐渠道 pipeline 也是同样条数），而往返次数才是
// 「一次 5 秒轮询会不会被放大成几百次网络往返」的直接量。桩替身证不了这一点。
type dashboardCommandCounter struct {
	mu sync.Mutex
	n  int
}

func (c *dashboardCommandCounter) add(n int) {
	c.mu.Lock()
	c.n += n
	c.mu.Unlock()
}

func (c *dashboardCommandCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func (c *dashboardCommandCounter) DialHook(next redis.DialHook) redis.DialHook { return next }

func (c *dashboardCommandCounter) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		c.add(1)
		return next(ctx, cmd)
	}
}

func (c *dashboardCommandCounter) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		// 整段 pipeline 算**一次**往返（这正是批量化要证的事）。
		c.add(1)
		return next(ctx, cmds)
	}
}

func dashboardRedisRuntime(t *testing.T, ttlSeconds int64) (*redis.Client, ObservedSessionRuntime) {
	t.Helper()
	rawURL := os.Getenv(dashboardRedisGate)
	if rawURL == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过 Redis 集成测试")
	}
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		t.Fatalf("解析 Redis URL 失败: %v", err)
	}
	if options.DB == 0 {
		options.DB = 13
	}
	client := redis.NewClient(options)
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("Redis 不可达: %v", err)
	}
	return client, NewRedisSessionRuntime(client, ttlSeconds, nil)
}

// TestObservedSessionCountOnRealRedis 钉住观测计数的三处行为。
func TestObservedSessionCountOnRealRedis(t *testing.T) {
	client, runtime := dashboardRedisRuntime(t, 300)
	ctx := context.Background()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 10)
	live, stale, orphan := "it-"+suffix+"-live", "it-"+suffix+"-stale", "it-"+suffix+"-orphan"
	infoKeys := []string{
		"session:" + live + ":info",
		"session:" + stale + ":info",
	}
	t.Cleanup(func() {
		keys := append([]string{observedSessionsKey}, infoKeys...)
		_ = client.Del(context.Background(), keys...).Err()
		_ = client.ZRem(context.Background(), observedSessionsKey, live, stale, orphan).Err()
	})

	if err := client.Del(ctx, observedSessionsKey).Err(); err != nil {
		t.Fatalf("清空观测集合失败: %v", err)
	}

	t.Run("键不存在时为 0", func(t *testing.T) {
		count, err := runtime.ObservedSessionCount(ctx)
		if err != nil {
			t.Fatalf("读数失败: %v", err)
		}
		if count != 0 {
			t.Fatalf("键不存在时应为 0，实际 %d", count)
		}
	})

	t.Run("成员存在但 info 已删不计入", func(t *testing.T) {
		now := time.Now().UnixMilli()
		if err := client.ZAdd(ctx, observedSessionsKey,
			redis.Z{Score: float64(now), Member: orphan},
			redis.Z{Score: float64(now), Member: live},
		).Err(); err != nil {
			t.Fatalf("写观测集合失败: %v", err)
		}
		if err := client.Set(ctx, "session:"+live+":info", "1", time.Minute).Err(); err != nil {
			t.Fatalf("写 info 键失败: %v", err)
		}
		count, err := runtime.ObservedSessionCount(ctx)
		if err != nil {
			t.Fatalf("读数失败: %v", err)
		}
		if count != 1 {
			t.Fatalf("只有 info 存在的成员才计入（应 1），实际 %d", count)
		}
	})

	t.Run("过期成员被顺手摘除", func(t *testing.T) {
		staleScore := time.Now().Add(-10 * time.Minute).UnixMilli()
		if err := client.ZAdd(ctx, observedSessionsKey,
			redis.Z{Score: float64(staleScore), Member: stale}).Err(); err != nil {
			t.Fatalf("写过期成员失败: %v", err)
		}
		if err := client.Set(ctx, "session:"+stale+":info", "1", time.Minute).Err(); err != nil {
			t.Fatalf("写 info 键失败: %v", err)
		}
		count, err := runtime.ObservedSessionCount(ctx)
		if err != nil {
			t.Fatalf("读数失败: %v", err)
		}
		// 过期成员虽有 info 也不计入，且会被从 ZSET 里摘掉（不是只在内存里过滤）。
		if count != 1 {
			t.Fatalf("过期成员不该计入（应 1），实际 %d", count)
		}
		if _, err := client.ZScore(ctx, observedSessionsKey, stale).Result(); err != redis.Nil {
			t.Fatalf("过期成员应被从 ZSET 里摘除（不是只在内存里过滤），实际 err=%v", err)
		}
		if _, err := client.ZScore(ctx, observedSessionsKey, live).Result(); err != nil {
			t.Fatalf("活跃成员不该被摘除：%v", err)
		}
	})
}

// TestProviderInFlightCountsOnRealRedis 钉住供应商在飞尝试数的口径与同步清理。
//
// 口径（用户 2026-09-22：「在飞请求数（每尝试计）」）：成员是「会话身份 + 尝试 token」，
// 故**计数 = 成员个数**，同一会话的两个在飞尝试计 2；且不再做 session:{id}:info 校验
// （尝试不是会话，那个键对在飞数没有判定力——本用例用「故意不建 info 键」反证这一点）。
func TestProviderInFlightCountsOnRealRedis(t *testing.T) {
	client, runtime := dashboardRedisRuntime(t, 300)
	ctx := context.Background()
	// 用远高于既有夹具的 id，避免与其它用例（或真实运行态）相撞。
	providerID := time.Now().UnixNano() % 900000000
	key := providerActiveSessionsKey(providerID)
	refsKey := providerRefsKey(key)
	if refsKey == "" {
		t.Fatal("供应商键应能推出引用计数键")
	}
	liveFirst, liveSecond := "it-prov-live\x1fatk1", "it-prov-live\x1fatk2"
	stale := "it-prov-stale\x1fatk1"
	t.Cleanup(func() {
		_ = client.Del(context.Background(), key, refsKey).Err()
	})

	// 同一会话的两个在飞尝试 + 一个过期尝试；**故意不建 session:*:info**。
	if err := client.ZAdd(ctx, key,
		redis.Z{Score: float64(time.Now().UnixMilli()), Member: liveFirst},
		redis.Z{Score: float64(time.Now().UnixMilli()), Member: liveSecond},
		redis.Z{Score: float64(time.Now().Add(-10 * time.Minute).UnixMilli()), Member: stale},
	).Err(); err != nil {
		t.Fatalf("写供应商在飞集合失败: %v", err)
	}
	if err := client.HSet(ctx, refsKey, liveFirst, 1, liveSecond, 1, stale, 1).Err(); err != nil {
		t.Fatalf("写引用计数失败: %v", err)
	}

	counts, err := runtime.ProviderInFlightCounts(ctx, []int64{providerID, providerID + 1})
	if err != nil {
		t.Fatalf("读供应商在飞数失败: %v", err)
	}
	// 同一会话的两个尝试都计入（不按会话去重），过期的那个不计入。
	if counts[providerID] != 2 {
		t.Errorf("同一会话的两个在飞尝试应计 2（每尝试计），实际 %d", counts[providerID])
	}
	if counts[providerID+1] != 0 {
		t.Errorf("没有在飞集合的供应商应为 0，实际 %d", counts[providerID+1])
	}

	refs, err := client.HGetAll(ctx, refsKey).Result()
	if err != nil {
		t.Fatalf("读引用计数失败: %v", err)
	}
	if _, present := refs[stale]; present {
		t.Errorf("过期尝试的引用计数应被删掉，实际 %+v", refs)
	}
	for _, member := range []string{liveFirst, liveSecond} {
		if _, present := refs[member]; !present {
			t.Errorf("未过期尝试的引用计数不该被删（%q），实际 %+v", member, refs)
		}
	}
}

// TestProviderInFlightCountsBatchesRoundTrips 钉住读面的**批量化**：N 个渠道的**往返次数**
// 必须**少于**渠道数（三段 pipeline ⇒ 常数级），不随渠道数线性增长——旧实现是逐渠道串行
// （每空渠道 1 次、每活跃渠道 5 次往返），几十个渠道会把一次 5 秒轮询放大成几百次串行往返。
func TestProviderInFlightCountsBatchesRoundTrips(t *testing.T) {
	client, runtime := dashboardRedisRuntime(t, 300)
	ctx := context.Background()

	baseID := time.Now().UnixNano() % 900000000
	activeID := baseID + 7
	key := providerActiveSessionsKey(activeID)
	t.Cleanup(func() { _ = client.Del(context.Background(), key).Err() })
	if err := client.ZAdd(ctx, key,
		redis.Z{Score: float64(time.Now().UnixMilli()), Member: "it-batch\x1fatk1"}).Err(); err != nil {
		t.Fatalf("写在飞成员失败: %v", err)
	}

	const providerCount = 30
	ids := make([]int64, 0, providerCount)
	for i := 0; i < providerCount; i++ {
		ids = append(ids, baseID+int64(i))
	}

	hook := &dashboardCommandCounter{}
	client.AddHook(hook)
	before := hook.count()
	counts, err := runtime.ProviderInFlightCounts(ctx, ids)
	if err != nil {
		t.Fatalf("批量读数失败: %v", err)
	}
	roundTrips := hook.count() - before
	if counts[activeID] != 1 {
		t.Fatalf("活跃渠道应读到 1，实际 %d", counts[activeID])
	}
	// 三段 pipeline ⇒ 3 次往返，与渠道数无关；必须严格少于渠道数（否则就是逐渠往返）。
	if roundTrips >= providerCount {
		t.Fatalf("%d 个渠道用了 %d 次 Redis 往返，应少于渠道数（三段 pipeline）",
			providerCount, roundTrips)
	}
	if roundTrips > 3 {
		t.Fatalf("三段 pipeline 应恰好 3 次往返，实际 %d", roundTrips)
	}
}

// TestObservedSessionKeyTypeSelfHeal 钉住「键类型不对时自愈」：Node 对历史遗留的 Set 数据
// 就是这么处理的（删键并返回 0），不删会让计数永远读到类型错误。
func TestObservedSessionKeyTypeSelfHeal(t *testing.T) {
	client, runtime := dashboardRedisRuntime(t, 300)
	ctx := context.Background()
	t.Cleanup(func() {
		_ = client.Del(context.Background(), observedSessionsKey).Err()
	})

	if err := client.Set(ctx, observedSessionsKey, "legacy-set-marker", time.Minute).Err(); err != nil {
		t.Fatalf("写遗留键失败: %v", err)
	}
	count, err := runtime.ObservedSessionCount(ctx)
	if err != nil {
		t.Fatalf("读数失败: %v", err)
	}
	if count != 0 {
		t.Fatalf("类型不对时应为 0，实际 %d", count)
	}
	exists, err := client.Exists(ctx, observedSessionsKey).Result()
	if err != nil {
		t.Fatalf("读存在性失败: %v", err)
	}
	if exists != 0 {
		t.Fatalf("类型不对的键应被删除，实际仍存在")
	}
}
