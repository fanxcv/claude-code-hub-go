package jobs

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/pubstatus"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/redis/go-redis/v9"
)

// 本文件是**剖析夹具**，不是功能测试：把生产上按固定节奏触发的那几个后台任务各跑一轮，
// 量出「每 tick 的 CPU 与分配」，再乘上生产节奏（见 cmd/cchd/jobs.go 与 jobs_ops.go 的
// interval 默认值）就能把空闲期 CPU 归因到具体任务。
//
// 门控：PG 任务要 CCH_TEST_DSN，Redis 任务要 CCH_TEST_REDIS_URL；两者都没有时整包跳过，
// 因此它不会污染无依赖环境的门禁。用法：
//
//	CCH_TEST_DSN=... CCH_TEST_REDIS_URL=... \
//	  go test -run XXX -bench 'Tick' -benchmem -benchtime 30x ./internal/jobs/
//
// cpu profile 与内存剖面的长时采集见 profile_idle_test.go。
const profileRedisDB = 15

// profilePools 建连接池（ApplicationNameBase 与集成测试区分，便于在 pg_stat_activity 里认人）。
func profilePools(tb testing.TB) *store.Pools {
	tb.Helper()
	dsn := os.Getenv("CCH_TEST_DSN")
	if dsn == "" {
		tb.Skip("未设置 CCH_TEST_DSN，跳过 PG 剖析夹具")
	}
	pools, err := store.Open(context.Background(), store.Options{
		DSN:                 dsn,
		Budget:              config.SplitPoolBudget(6),
		Timeouts:            config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
		ApplicationNameBase: "cch-jobs-profile",
	})
	if err != nil {
		tb.Fatalf("建立连接池失败: %v", err)
	}
	tb.Cleanup(func() { _ = pools.Close() })
	return pools
}

// profileRedis 建 Redis 客户端（缺省 DB 15，与集成测试同一约定）。
func profileRedis(tb testing.TB) redis.UniversalClient {
	tb.Helper()
	raw := os.Getenv("CCH_TEST_REDIS_URL")
	if raw == "" {
		tb.Skip("未设置 CCH_TEST_REDIS_URL，跳过 Redis 剖析夹具")
	}
	options, err := redis.ParseURL(raw)
	if err != nil {
		tb.Fatalf("解析 Redis URL 失败: %v", err)
	}
	if options.DB == 0 {
		options.DB = profileRedisDB
	}
	client := redis.NewClient(options)
	tb.Cleanup(func() { _ = client.Close() })
	return client
}

func profileDeps(tb testing.TB) OpsDeps {
	tb.Helper()
	return OpsDeps{
		Pools:  profilePools(tb),
		Redis:  profileRedis(tb),
		Logger: logx.New(os.Stderr),
	}
}

// BenchmarkAvailProjectionConsumeTick 量「可用性投影消费」一轮的成本。
//
// 生产节奏：cmd/cchd/jobs.go 传 CCH_JOB_AVAIL_PROJECTION_INTERVAL_MS（默认 200ms）⇒ 每秒 5 轮。
// 空库路径 = advisory 锁往返 + 取待处理事件（无） + 释放锁。
func BenchmarkAvailProjectionConsumeTick(b *testing.B) {
	deps := profileDeps(b)
	consumer, err := NewAvailProjectionConsumer(AvailProjectionConsumerOptions{
		Pools:  deps.Pools,
		Logger: deps.Logger,
	})
	if err != nil {
		b.Fatalf("构造消费器失败: %v", err)
	}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := consumer.RunOnce(ctx); err != nil {
			b.Fatalf("第 %d 轮失败: %v", index, err)
		}
	}
}

// BenchmarkOutboxReplayTick 量「routing-trace outbox 回放」一轮的成本。生产节奏 30s。
func BenchmarkOutboxReplayTick(b *testing.B) {
	deps := profileDeps(b)
	replay := NewOutboxReplay(deps)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := replay.Run(ctx); err != nil {
			b.Fatalf("第 %d 轮失败: %v", index, err)
		}
	}
}

// BenchmarkPublicStatusRebuildTick 量「公开状态投影重建」一轮的成本。生产节奏 30s，只需 Redis。
func BenchmarkPublicStatusRebuildTick(b *testing.B) {
	deps := profileDeps(b)
	rebuild := NewPublicStatusRebuild(
		deps,
		pubstatus.NewRedisProjectionRedis(deps.Redis),
		nil,
		nil,
	)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := rebuild.Run(ctx); err != nil {
			b.Fatalf("第 %d 轮失败: %v", index, err)
		}
	}
}

// BenchmarkEndpointProbeIdleTick 量「端点探活」在**无到期目标**时的空转成本。
//
// 生产节奏：TickInterval = min(BaseInterval 60s, TimeoutRetryInterval 10s) = 10s。
// ProbeTargets 注入空列表是**刻意的**：默认实现会扫全库启用端点并真的去拨测它们
// （OpsDeps.ProbeTargets 的注释写明了这个缝就是为测试隔离而留）。
func BenchmarkEndpointProbeIdleTick(b *testing.B) {
	deps := profileDeps(b)
	deps.ProbeTargets = func(context.Context) ([]store.ProbeEndpoint, error) { return nil, nil }
	probe := NewEndpointProbe(deps, ProbeConfig{})
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := probe.Run(ctx); err != nil {
			b.Fatalf("第 %d 轮失败: %v", index, err)
		}
	}
}

// BenchmarkCacheEffectivenessTick 量「缓存命中率窗口聚合」一轮的成本。生产节奏 5 分钟。
//
// 它是**唯一会扫 message_request 大表**的固定任务，故是周期性尖峰的头号嫌疑。
func BenchmarkCacheEffectivenessTick(b *testing.B) {
	deps := profileDeps(b)
	job, err := NewCacheEffectiveness(CacheEffectivenessOptions{
		Pools:        deps.Pools,
		Logger:       deps.Logger,
		EnabledByEnv: true,
	})
	if err != nil {
		b.Fatalf("构造聚合任务失败: %v", err)
	}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := job.RunOnce(ctx); err != nil {
			b.Fatalf("第 %d 轮失败: %v", index, err)
		}
	}
}

// BenchmarkAvailBackfillTick 量「可用性投影回填复查」一轮的成本。生产节奏 5 分钟。
func BenchmarkAvailBackfillTick(b *testing.B) {
	deps := profileDeps(b)
	backfill, err := NewAvailBackfill(AvailBackfillOptions{
		Pools:  deps.Pools,
		Logger: deps.Logger,
	})
	if err != nil {
		b.Fatalf("构造回填任务失败: %v", err)
	}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := backfill.RunOnce(ctx); err != nil {
			b.Fatalf("第 %d 轮失败: %v", index, err)
		}
	}
}
