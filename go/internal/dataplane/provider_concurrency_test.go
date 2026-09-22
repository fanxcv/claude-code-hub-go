package dataplane

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/limit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ratelimit"
)

// 本文件钉住并发名额缝的**闸门**与**上限判定**语义：
// 两条闸门都不满足时连一条 Redis 命令都不发（「关上零开销」的判据），
// 而配了上限的渠道**即使统计开关关着也必须判定**——执法只看渠道自己有没有设并发数，
// 与显示面开关无关（否则「设了上限不生效」这个 bug 会原样留下）。
//
// 开关只管显示面的那一半、以及装配行本身，见 provider_concurrency_switch_test.go。

const testRedisEnv = "CCH_TEST_REDIS_URL"

// countingHook 记录发往 Redis 的命令名。
type countingHook struct {
	mu       sync.Mutex
	commands []string
}

func (h *countingHook) record(name string) {
	h.mu.Lock()
	h.commands = append(h.commands, name)
	h.mu.Unlock()
}

func (h *countingHook) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.commands)
}

func (h *countingHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h *countingHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		h.record(cmd.Name())
		return next(ctx, cmd)
	}
}

func (h *countingHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			h.record(cmd.Name())
		}
		return next(ctx, cmds)
	}
}

// alwaysTrackingEnabled 是「开关恒开」的替身：真开关的逐请求读由
// providerConcurrencyTrackingFor 的用例覆盖，开关翻转由 switch flips 用例覆盖。
func alwaysTrackingEnabled(context.Context) bool { return true }

// unreachableGate 造一个指向不可达地址的 gate：命令会被计数，但一条都到不了 Redis。
// switchFn 为 nil 表示「未接线或开关关闭」。
func unreachableGate(t *testing.T, switchFn func(context.Context) bool) (*providerConcurrencyGate, *countingHook) {
	t.Helper()
	hook := &countingHook{}
	rdb := redis.NewClient(&redis.Options{
		Addr:         "127.0.0.1:1",
		DialTimeout:  100 * time.Millisecond,
		ReadTimeout:  100 * time.Millisecond,
		WriteTimeout: 100 * time.Millisecond,
		MaxRetries:   0,
	})
	rdb.AddHook(hook)
	t.Cleanup(func() { _ = rdb.Close() })
	registry, err := ratelimit.Embedded()
	if err != nil {
		t.Fatalf("装载脚本清单失败: %v", err)
	}
	client, err := ratelimit.New(rdb, registry)
	if err != nil {
		t.Fatalf("组装脚本调用层失败: %v", err)
	}
	return &providerConcurrencyGate{
		tracker:         limit.NewSessionTracker(client, 0, nil),
		trackingEnabled: switchFn,
		logger:          logx.New(nil),
	}, hook
}

// TestProviderConcurrencyGateMakesZeroRedisCallsWhenDisabled 是零开销闸门的主判据：
// 既没开统计、渠道也没配上限时，一条 Redis 命令都不该发。
func TestProviderConcurrencyGateMakesZeroRedisCallsWhenDisabled(t *testing.T) {
	gate, hook := unreachableGate(t, nil)
	result := gate.acquire(context.Background(), 167, 0, "sess-1")
	if !result.Allowed {
		t.Fatal("未接线时应放行")
	}
	if result.Release != nil {
		t.Fatal("未接线时不该给出释放函数")
	}
	if got := hook.count(); got != 0 {
		t.Fatalf("未开启时发出了 %d 条 Redis 命令，应为 0（零开销闸门失效）", got)
	}
}

// TestProviderConcurrencyGateTracksWithoutLimitWhenSwitchOn：统计开关打开时，
// 即使渠道没配上限也要登记（Lua 的 limit<=0 分支只登记不拒绝）。
func TestProviderConcurrencyGateTracksWithoutLimitWhenSwitchOn(t *testing.T) {
	gate, hook := unreachableGate(t, alwaysTrackingEnabled)
	result := gate.acquire(context.Background(), 167, 0, "sess-1")
	if !result.Allowed {
		t.Fatal("Redis 不可达时必须 Fail Open（放行）")
	}
	if got := hook.count(); got == 0 {
		t.Fatal("统计开关打开却没发任何 Redis 命令（登记被闸门误挡）")
	}
}

// TestProviderConcurrencyGateEnforcesLimitWithoutSwitch：配了上限就必须判定，
// 与显示面的统计开关无关——「设了上限不生效」正是本次要修的 bug。
//
// 用户口径（2026-09-22）：「渠道设置大于 0 的并发数，就需要控制这个渠道的并发情况，
// 跟我开不开统计开关有啥关系」。
func TestProviderConcurrencyGateEnforcesLimitWithoutSwitch(t *testing.T) {
	gate, hook := unreachableGate(t, nil)
	result := gate.acquire(context.Background(), 167, 20, "sess-1")
	if !result.Allowed {
		t.Fatal("Redis 不可达时必须 Fail Open（放行）")
	}
	if got := hook.count(); got == 0 {
		t.Fatal("渠道配了并发上限却没发任何 Redis 命令（上限判定被统计开关误挡）")
	}
}

// TestProviderConcurrencyGateSkipsRequestsWithoutSession：无会话身份的请求不占名额
// （名额按会话计，空身份会把所有这类请求压成同一个成员，造成假满）。
func TestProviderConcurrencyGateSkipsRequestsWithoutSession(t *testing.T) {
	gate, hook := unreachableGate(t, alwaysTrackingEnabled)
	for _, sessionID := range []string{"", " "} {
		result := gate.acquire(context.Background(), 167, 20, sessionID)
		if !result.Allowed {
			t.Fatalf("会话身份 %q 应放行", sessionID)
		}
	}
	if got := hook.count(); got != 0 {
		t.Fatalf("空会话身份发出了 %d 条 Redis 命令，应为 0", got)
	}
}

// TestProviderConcurrencyGateAgainstRealRedis 用真 Redis 钉住「登记 + 上限 + 释放」整条链。
func TestProviderConcurrencyGateAgainstRealRedis(t *testing.T) {
	rawURL := os.Getenv(testRedisEnv)
	if rawURL == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过 Redis 集成测试")
	}
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		t.Fatalf("解析 Redis URL 失败: %v", err)
	}
	rdb := redis.NewClient(options)
	t.Cleanup(func() { _ = rdb.Close() })
	if err := rdb.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("Redis 不可达: %v", err)
	}
	registry, err := ratelimit.Embedded()
	if err != nil {
		t.Fatalf("装载脚本清单失败: %v", err)
	}
	client, err := ratelimit.New(rdb, registry)
	if err != nil {
		t.Fatalf("组装脚本调用层失败: %v", err)
	}
	gate := &providerConcurrencyGate{
		tracker:         limit.NewSessionTracker(client, 0, nil),
		trackingEnabled: alwaysTrackingEnabled,
		logger:          logx.New(nil),
	}

	const providerID = int64(900001)
	ctx := context.Background()
	key := limit.ProviderActiveSessionsKey(providerID)
	refs := limit.ProviderSessionRefsKey(providerID)
	t.Cleanup(func() {
		_ = rdb.Del(context.Background(), key, refs).Err()
	})

	// 上限 1：第一个会话放行并占住名额。
	first := gate.acquire(ctx, providerID, 1, "sess-a")
	if !first.Allowed || first.Release == nil {
		t.Fatalf("首个会话应放行并拿到释放函数: %+v", first)
	}
	if got := rdb.ZCard(ctx, key).Val(); got != 1 {
		t.Fatalf("登记后计数 = %d，应为 1", got)
	}

	// 第二个会话被拒，且读数与上限一致（429 信封的两个字段就来自这里）。
	second := gate.acquire(ctx, providerID, 1, "sess-b")
	if second.Allowed {
		t.Fatal("上限已满时应拒绝")
	}
	if second.Current != 1 {
		t.Fatalf("被拒时的读数 = %d，应为 1", second.Current)
	}

	// 同一会话重复登记不涨计数（成员按 sessionID 去重，引用计数只在释放时才归零）。
	same := gate.acquire(ctx, providerID, 1, "sess-a")
	if !same.Allowed {
		t.Fatal("同一会话重复登记应放行（已占名额者不该被自己锁死）")
	}
	if got := rdb.ZCard(ctx, key).Val(); got != 1 {
		t.Fatalf("重复登记后计数 = %d，应为 1", got)
	}

	// 释放到引用归零才真正摘除，随后被拒的会话可以进来。
	first.Release()
	if got := rdb.ZCard(ctx, key).Val(); got != 1 {
		t.Fatalf("释放一个引用后计数 = %d，应为 1（引用计数未生效）", got)
	}
	same.Release()
	if got := rdb.ZCard(ctx, key).Val(); got != 0 {
		t.Fatalf("引用全部归还后计数 = %d，应为 0（名额泄漏）", got)
	}
	if again := gate.acquire(ctx, providerID, 1, "sess-b"); !again.Allowed {
		t.Fatal("名额归还后新会话应能进来")
	} else {
		again.Release()
	}
}

// TestProviderConcurrencyReleaseIsIdempotent：释放函数被重复调用不得多减引用
// （body 会被多条路径 Close，释放必须自备幂等）。
func TestProviderConcurrencyReleaseIsIdempotent(t *testing.T) {
	rawURL := os.Getenv(testRedisEnv)
	if rawURL == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过 Redis 集成测试")
	}
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		t.Fatalf("解析 Redis URL 失败: %v", err)
	}
	rdb := redis.NewClient(options)
	t.Cleanup(func() { _ = rdb.Close() })
	registry, err := ratelimit.Embedded()
	if err != nil {
		t.Fatalf("装载脚本清单失败: %v", err)
	}
	client, err := ratelimit.New(rdb, registry)
	if err != nil {
		t.Fatalf("组装脚本调用层失败: %v", err)
	}
	gate := &providerConcurrencyGate{
		tracker:         limit.NewSessionTracker(client, 0, nil),
		trackingEnabled: alwaysTrackingEnabled,
		logger:          logx.New(nil),
	}

	const providerID = int64(900002)
	ctx := context.Background()
	key := limit.ProviderActiveSessionsKey(providerID)
	refs := limit.ProviderSessionRefsKey(providerID)
	t.Cleanup(func() {
		_ = rdb.Del(context.Background(), key, refs).Err()
	})

	// 两个会话各占一个引用；其中之一的释放被调用两次，另一个的名额不得被连带抹掉。
	a := gate.acquire(ctx, providerID, 2, "sess-a")
	b := gate.acquire(ctx, providerID, 2, "sess-b")
	if !a.Allowed || !b.Allowed || a.Release == nil || b.Release == nil {
		t.Fatalf("两个会话都应放行: a=%+v b=%+v", a, b)
	}
	a.Release()
	a.Release()
	if got := rdb.ZCard(ctx, key).Val(); got != 1 {
		t.Fatalf("重复释放后计数 = %d，应为 1（b 的名额被连带抹掉）", got)
	}
	b.Release()
	if got := rdb.ZCard(ctx, key).Val(); got != 0 {
		t.Fatalf("全部释放后计数 = %d，应为 0", got)
	}
}
