package jobs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住 P1-5（第 2 轮核心审查）：基线任务**成功**判定为无基线（设计稿 A3）时必须撤销旧键。
//
// 缺陷形态：某（渠道 × 模型）在 W1/W2 都有行但都低于发布门槛时，DecideBaseline 返回
// Publish=false，publishScope 直接 `return false, false, nil`——既不覆盖也不删除旧键；又因该
// 组合仍在 RunOnce 的 seen 集合里，末尾 sweep 也不删它。旧键 TTL 是 7 天，recorder 会继续读它
// 判慢、写 state 与冷却。而设计稿对 A3 明定 fail-open，读侧只在**键不存在**时才 fail-open——
// 所以「不撤旧键」等于 A3 从未真正生效。
//
// 本文件守的是**分界**，两条钉子各守一半（任一半写错都只会静默改行为）：
//   - 成功判定无基线 -> 必须撤键（否则 A3 失效）；
//   - 查询失败       -> 必须**保留**键（设计稿：宁可在抖动时留着，也不要把基线清空）。

const revokeTestModelKey = "deepseek-v4.1-flash"

// errRevokeStub 是替身 Del 的注入错误（测「撤键失败不许静默吞」）。
var errRevokeStub = errors.New("stub: del 失败")

// brokenPools 造一个**必然查库失败**的池：DSN 指向 127.0.0.1:1（无人监听，连接立即被拒），
// 而 store.Open 是惰性的（不 ping），故建池成功、查询必失败。无需 CCH_TEST_DSN、无需真库。
//
// 为何不用零值 `&store.Pools{}`：其 state 零值恰是 poolOpen（pool.go 里 iota 首项），
// 于是 Lane() 不会早退，会继续走到 `p.lanes[physical] = created`——lanes 是 nil map，直接 panic
// （实测如此，整测试二进制被 panic 中止）。
func brokenPools(t *testing.T) *store.Pools {
	t.Helper()
	pools, err := store.Open(context.Background(), store.Options{DSN: "postgres://127.0.0.1:1/none"})
	if err != nil {
		t.Fatalf("建空池失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	return pools
}

// revokeStubRedis 只实现 Del。
//
// 本文件观察的是「任务在哪种判定下调了 Del、传的是哪个键」，故只需 Del；其余方法由内嵌接口
// 兜底（内嵌为 nil，一调即 panic，正合用——用例不该碰别的命令）。
type revokeStubRedis struct {
	redis.UniversalClient
	deleted []string
	delErr  error
}

func (r *revokeStubRedis) Del(ctx context.Context, keys ...string) *redis.IntCmd {
	r.deleted = append(r.deleted, keys...)
	cmd := redis.NewIntCmd(ctx)
	if r.delErr != nil {
		cmd.SetErr(r.delErr)
	} else {
		cmd.SetVal(int64(len(keys)))
	}
	return cmd
}

func revokeTestScope(w1Samples, w2Samples int64) store.SlowRateScopeSamples {
	return store.SlowRateScopeSamples{
		ProviderID: 167,
		ModelKey:   revokeTestModelKey,
		W1Samples:  w1Samples,
		W2Samples:  w2Samples,
	}
}

// revokeTestConfig 造一个样本下限 100 的渠道配置：W1=5 / W2=50 都低于它，落在 A3。
func revokeTestConfig() store.SlowRateProviderConfig {
	return store.SlowRateProviderConfig{ProviderID: 167, MinSamples: intPtr(100)}
}

// TestBaselineNoPublishRevokesStaleKey 是 P1-5 的直接回归。
//
// 判据（A3：W1=5、W2=50、下限=100）：published=false 且**必须调 Del 撤掉该 scope 的键**。
// 修复前这里什么都不做，旧键一直留到 TTL 到期。
func TestBaselineNoPublishRevokesStaleKey(t *testing.T) {
	red := &revokeStubRedis{}
	var logs bytes.Buffer
	b := &SlowRateBaseline{redis: red, logger: logx.New(&logs)}

	published, truncated, err := b.publishScope(
		context.Background(),
		revokeTestScope(5, 50),
		time.Unix(0, 0), time.Unix(0, 0), time.Unix(0, 0),
		revokeTestConfig(),
	)
	if err != nil {
		t.Fatalf("A3 撤键不该报错: %v", err)
	}
	if published || truncated {
		t.Fatalf("A3 不该发布也不该算截断: published=%v truncated=%v", published, truncated)
	}
	want := BaselineKey(167, revokeTestModelKey)
	if len(red.deleted) != 1 || red.deleted[0] != want {
		t.Fatalf("A3 必须撤掉旧键 %q，实际 Del 调用: %v", want, red.deleted)
	}
	if !strings.Contains(logs.String(), "slow_rate_baseline_revoked") {
		t.Fatalf("撤键须留痕（slow_rate_baseline_revoked），实际日志: %s", logs.String())
	}
}

// TestBaselineRateQueryErrorKeepsStaleKey 守分界的另一半：取行失败**不许**撤键。
//
// 为何必须单独钉：最省事的写法（「只要没发布就撤」）会让查询失败也撤键，于是每次 PG 抖动都把
// 基线清空——设计稿明定「读不到配置时保持键，宁可留着也不要在抖动时清空」。
//
// 用不可达 DSN 的池造确定性失败（见 brokenPools：无需 DSN、无需真库，连接被立即拒绝）。
func TestBaselineRateQueryErrorKeepsStaleKey(t *testing.T) {
	red := &revokeStubRedis{}
	b := &SlowRateBaseline{redis: red, pools: brokenPools(t), logger: logx.New(io.Discard)}

	// W1=200 >= 下限 100 ⇒ 判定为「发布」，于是继续往下取行——取行必失败。
	published, _, err := b.publishScope(
		context.Background(),
		revokeTestScope(200, 0),
		time.Unix(0, 0), time.Unix(0, 0), time.Unix(0, 0),
		revokeTestConfig(),
	)
	if err == nil {
		t.Fatal("取行失败必须上抛（不许当成「无基线」吞掉）")
	}
	if published {
		t.Fatal("取行失败不该算发布")
	}
	if len(red.deleted) != 0 {
		t.Fatalf("查询失败必须保留旧键，实际 Del 调用: %v", red.deleted)
	}
}

// TestBaselineCountQueryErrorKeepsStaleKey 钉整轮级的同类分界：计数查询失败时整轮早退，
// 一个键都不许动（与单 scope 的取行失败同理，但发生在更外层）。
func TestBaselineCountQueryErrorKeepsStaleKey(t *testing.T) {
	red := &revokeStubRedis{}
	b := &SlowRateBaseline{
		redis:  red,
		pools:  brokenPools(t),
		logger: logx.New(io.Discard),
		now:    func() time.Time { return time.Unix(0, 0) },
		enabledProviders: func(context.Context) ([]store.SlowRateProviderConfig, error) {
			return []store.SlowRateProviderConfig{revokeTestConfig()}, nil
		},
	}

	if _, err := b.RunOnce(context.Background()); err == nil {
		t.Fatal("计数查询失败必须上抛")
	}
	if len(red.deleted) != 0 {
		t.Fatalf("整轮失败不许动任何键，实际 Del 调用: %v", red.deleted)
	}
}

// TestBaselineNoPublishRemovesKeyInRedis 用真 Redis 钉字面判据：撤键之后**键不存在**。
//
// 与替身钉子分工：替身钉「在哪种判定下调了 Del、传的是哪个键」（无环境也能跑），本条钉
// 「真 Redis 里确实没了」（门控 CCH_TEST_REDIS_URL，复用同包既有的门控辅助，不新造第四份）。
func TestBaselineNoPublishRemovesKeyInRedis(t *testing.T) {
	rdb := opsTestRedis(t)
	ctx := context.Background()
	key := BaselineKey(167, revokeTestModelKey)
	if err := rdb.Set(ctx, key, `{"median":239.68,"samples":7215}`, time.Minute).Err(); err != nil {
		t.Fatalf("预置旧基线失败: %v", err)
	}
	t.Cleanup(func() { _ = rdb.Del(context.Background(), key).Err() })

	b := &SlowRateBaseline{redis: rdb, logger: logx.New(io.Discard)}
	if _, _, err := b.publishScope(
		ctx, revokeTestScope(5, 50),
		time.Unix(0, 0), time.Unix(0, 0), time.Unix(0, 0),
		revokeTestConfig(),
	); err != nil {
		t.Fatalf("A3 撤键不该报错: %v", err)
	}

	exists, err := rdb.Exists(ctx, key).Result()
	if err != nil {
		t.Fatalf("查键失败: %v", err)
	}
	if exists != 0 {
		t.Fatalf("A3 之后旧键必须不存在，实际 exists=%d", exists)
	}
}

// TestBaselineRevokeFailureSurfaces 钉撤键失败不许静默吞：Del 报错时 publishScope 必须上抛
// （由 RunOnce 记 scope_failed 后继续），而不是当成功。
func TestBaselineRevokeFailureSurfaces(t *testing.T) {
	red := &revokeStubRedis{delErr: errRevokeStub}
	b := &SlowRateBaseline{redis: red, logger: logx.New(io.Discard)}

	if _, _, err := b.publishScope(
		context.Background(),
		revokeTestScope(5, 50),
		time.Unix(0, 0), time.Unix(0, 0), time.Unix(0, 0),
		revokeTestConfig(),
	); err == nil {
		t.Fatal("撤键失败必须上抛，不许静默吞")
	}
}
