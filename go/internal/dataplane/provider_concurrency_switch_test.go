package dataplane

import (
	"context"
	"errors"
	"os"
	"regexp"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/limit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/ratelimit"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住「供应商并发统计」**显示面开关**的语义与接线：它只管页面显不显示并发数，
// **不管执法**——执法只取决于渠道自己有没有设 providers.limit_concurrent_sessions。
//
// 为什么单独立一个文件：闸门内部的行为（登记/判定/释放/异常路径归还）已由
// provider_concurrency_test.go 覆盖，而本文件覆盖两件它盖不住的事：
//
//   - **执法与开关无关**（用户 2026-09-22 口径：「渠道设置大于 0 的并发数，就需要控制这个
//     渠道的并发情况，跟我开不开统计开关有啥关系」）——开关关着时满员渠道仍必须 429；
//   - **装配行是否真的存在**：本仓反复踩「已定义≠未接线」，而摘掉装配行时闸门自身的用例全绿。

// realRedisGate 造一个接真 Redis 的闸门，并在其上挂命令计数钩子。
// switchFn 为 nil 表示开关关闭。
func realRedisGate(
	t *testing.T,
	switchFn func(context.Context) bool,
) (*providerConcurrencyGate, *countingHook, redis.UniversalClient) {
	t.Helper()
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
	// 钩子在 Ping 之后挂：计数只反映被测代码发出的命令。
	hook := &countingHook{}
	rdb.AddHook(hook)
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
	}, hook, rdb
}

// saturateChannel 直接把渠道的并发集合填到上限（不经闸门，避免用例依赖被测代码造前置态）。
func saturateChannel(t *testing.T, rdb redis.UniversalClient, providerID int64, member string) {
	t.Helper()
	ctx := context.Background()
	key := limit.ProviderActiveSessionsKey(providerID)
	t.Cleanup(func() { _ = rdb.Del(context.Background(), key, limit.ProviderSessionRefsKey(providerID)).Err() })
	if err := rdb.ZAdd(ctx, key, redis.Z{Score: float64(time.Now().UnixMilli()), Member: member}).Err(); err != nil {
		t.Fatalf("填满渠道失败: %v", err)
	}
	if got := rdb.ZCard(ctx, key).Val(); got != 1 {
		t.Fatalf("前置态不符：并发集合应有 1 个成员，实为 %d", got)
	}
}

// TestProviderConcurrencyEnforcesLimitRegardlessOfSwitch：渠道配了上限时，**开关关着**
// 也必须登记并按上限拒绝（429 信封的读数就来自这里）。
//
// 为什么必须用真 Redis 造满员：这条判据的失败形态是「开关关着时配了上限的渠道不再 429」，
// 而那要求集合里真的有成员——只用替身测不出「读到了满员读数」。
func TestProviderConcurrencyEnforcesLimitRegardlessOfSwitch(t *testing.T) {
	for _, tc := range []struct {
		name     string
		switchFn func(context.Context) bool
	}{
		{"开关关闭", nil},
		{"开关打开", alwaysTrackingEnabled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gate, _, rdb := realRedisGate(t, tc.switchFn)
			const providerID = int64(900201)
			saturateChannel(t, rdb, providerID, "sess-occupied")

			result := gate.acquire(context.Background(), providerID, 1, "sess-new")
			if result.Allowed {
				t.Fatal("渠道上限已满时必须拒绝（执法与显示面开关无关）")
			}
			if result.Current != 1 {
				t.Fatalf("被拒时的读数 = %d，应为 1", result.Current)
			}
		})
	}
}

// TestProviderConcurrencySwitchOffSkipsLimitlessChannel：没配上限的渠道在开关关着时
// 一条 Redis 命令都不发（它的登记只为页面显示，而页面不显示）。
func TestProviderConcurrencySwitchOffSkipsLimitlessChannel(t *testing.T) {
	gate, hook, _ := realRedisGate(t, nil)
	before := hook.count()
	result := gate.acquire(context.Background(), 900202, 0, "sess-1")
	if !result.Allowed {
		t.Fatal("没配上限时应放行")
	}
	if result.Release != nil {
		t.Fatal("没登记就不该给出释放函数")
	}
	if got := hook.count() - before; got != 0 {
		t.Fatalf("开关关闭且渠道无上限时发出了 %d 条 Redis 命令，应为 0", got)
	}
}

// TestProviderConcurrencySwitchOnRegistersLimitlessChannel：开关打开时，没配上限的渠道
// 也要登记（Lua 的 limit<=0 分支只登记不拒绝）——这一半纯粹为页面显示。
func TestProviderConcurrencySwitchOnRegistersLimitlessChannel(t *testing.T) {
	gate, _, rdb := realRedisGate(t, alwaysTrackingEnabled)
	const providerID = int64(900203)
	ctx := context.Background()
	key := limit.ProviderActiveSessionsKey(providerID)
	t.Cleanup(func() { _ = rdb.Del(context.Background(), key, limit.ProviderSessionRefsKey(providerID)).Err() })

	result := gate.acquire(ctx, providerID, 0, "sess-1")
	if !result.Allowed || result.Release == nil {
		t.Fatalf("开关打开时应登记并给出释放函数: %+v", result)
	}
	if got := rdb.ZCard(ctx, key).Val(); got != 1 {
		t.Fatalf("登记后计数 = %d，应为 1（显示面拿不到数）", got)
	}
	result.Release()
	if got := rdb.ZCard(ctx, key).Val(); got != 0 {
		t.Fatalf("释放后计数 = %d，应为 0（名额泄漏）", got)
	}
}

// TestProviderConcurrencyLimitlessRegistersEveryAttempt：没配上限（limit<=0）时**每次尝试**
// 都登记（只登记不判定），且同会话的多个尝试各占一个成员——这是页面「在飞请求数」的口径。
//
// 为何单独钉：limit<=0 走的是 Lua 的「只登记不判定」分支，容易被当成「不做事」而写成早退，
// 而那会让统计面在没配上限的渠道上恒为 0。
func TestProviderConcurrencyLimitlessRegistersEveryAttempt(t *testing.T) {
	gate, _, rdb := realRedisGate(t, alwaysTrackingEnabled)
	const providerID = int64(900204)
	ctx := context.Background()
	key := limit.ProviderActiveSessionsKey(providerID)
	t.Cleanup(func() { _ = rdb.Del(context.Background(), key, limit.ProviderSessionRefsKey(providerID)).Err() })

	// 同一会话的两次尝试：都放行，且各占一个成员（计数 2）——limit<=0 不判定但登记。
	first := gate.acquire(ctx, providerID, 0, "sess-same")
	second := gate.acquire(ctx, providerID, 0, "sess-same")
	if !first.Allowed || !second.Allowed || first.Release == nil || second.Release == nil {
		t.Fatalf("没配上限时两个尝试都应放行并登记: first=%+v second=%+v", first, second)
	}
	if got := rdb.ZCard(ctx, key).Val(); got != 2 {
		t.Fatalf("没配上限时同会话两个尝试应各占一个成员（计 2），实际 %d", got)
	}
	first.Release()
	second.Release()
	if got := rdb.ZCard(ctx, key).Val(); got != 0 {
		t.Fatalf("全部释放后计数 = %d，应为 0", got)
	}
}

// TestProviderConcurrencySwitchFlipsPerCallWithoutRebuild：开关逐请求读——同一个闸门实例，
// 翻转开关即改变行为，不需要重建。构造期快照会让「管理面改了开关、进程重启才生效」。
//
// 同时钉住「翻转只影响显示面那一半」：无论开关开还是关，配了上限的渠道都必须发命令。
func TestProviderConcurrencySwitchFlipsPerCallWithoutRebuild(t *testing.T) {
	enabled := false
	gate, hook := unreachableGate(t, func(context.Context) bool { return enabled })
	ctx := context.Background()

	// 关闭 + 无上限：零命令。
	if got := gate.acquire(ctx, 167, 0, "sess-1"); got.Release != nil {
		t.Fatal("关闭且无上限时不该给出释放函数")
	}
	if got := hook.count(); got != 0 {
		t.Fatalf("关闭且无上限时发出 %d 条 Redis 命令，应为 0", got)
	}

	// 关闭 + 有上限：**仍然发命令**（执法与开关无关）。
	before := hook.count()
	if got := gate.acquire(ctx, 167, 20, "sess-1"); !got.Allowed {
		t.Fatal("Redis 不可达时必须 Fail Open（放行）")
	}
	if hook.count() == before {
		t.Fatal("开关关着时配了上限的渠道却没发命令（执法被开关误挡）")
	}

	// 打开 + 无上限：发命令（登记供显示）。
	enabled = true
	before = hook.count()
	if got := gate.acquire(ctx, 167, 0, "sess-2"); !got.Allowed {
		t.Fatal("Redis 不可达时必须 Fail Open（放行）")
	}
	if hook.count() == before {
		t.Fatal("开关打开后无上限的渠道却没发命令（显示面拿不到数）")
	}

	// 再关闭 + 无上限：又回到零命令。
	before = hook.count()
	enabled = false
	if got := gate.acquire(ctx, 167, 0, "sess-3"); got.Release != nil {
		t.Fatal("再次关闭时不该给出释放函数")
	}
	if got := hook.count() - before; got != 0 {
		t.Fatalf("再次关闭后发出 %d 条 Redis 命令，应为 0", got)
	}
}

// TestProviderConcurrencyForRequestIgnoresSwitch：登记缝**不因开关关闭而变 nil**。
//
// 为什么这条要紧：缝为 nil 时 forward 整段跳过（一次都不调），于是配了上限的渠道也会漏判。
// 执法与开关无关，故这里只能在「未接线」时给 nil。
func TestProviderConcurrencyForRequestIgnoresSwitch(t *testing.T) {
	state := &RequestState{sessionID: "sess-1"}
	offGate := &providerConcurrencyGate{logger: logx.New(nil)}
	handler := &Handler{options: Options{providerConcurrency: offGate}, logger: logx.New(nil)}
	if got := handler.providerConcurrencyForRequest(state); got == nil {
		t.Fatal("开关关闭时登记缝不该为 nil（配了上限的渠道会被整段跳过）")
	}
	bare := &Handler{options: Options{}, logger: logx.New(nil)}
	if got := bare.providerConcurrencyForRequest(state); got != nil {
		t.Fatal("未接线时应返回 nil（forward 整段跳过）")
	}
}

// stubSwitchSettings 是 system_settings 读取面的替身。
type stubSwitchSettings struct {
	row *store.SystemSettings
	err error
}

func (s stubSwitchSettings) FindSystemSettings(context.Context) (*store.SystemSettings, error) {
	return s.row, s.err
}

// TestProviderConcurrencyTrackingForReadsSwitchColumn 钉住装配缝的取值口径：
// 读 system_settings 的 provider_live_stats_enabled 列，读取失败按「关」处理。
//
// 为什么失败要按关：统计是可选增值面，读不到开关时宁可不报数；它**不影响执法**
// （执法走 providerLimit > 0 那条闸门）。
func TestProviderConcurrencyTrackingForReadsSwitchColumn(t *testing.T) {
	ctx := context.Background()
	on := providerConcurrencyTrackingFor(StoreOptions{},
		stubSwitchSettings{row: &store.SystemSettings{ProviderLiveStatsEnabled: true}})
	if !on(ctx) {
		t.Fatal("列打开时开关应为真")
	}
	off := providerConcurrencyTrackingFor(StoreOptions{},
		stubSwitchSettings{row: &store.SystemSettings{ProviderLiveStatsEnabled: false}})
	if off(ctx) {
		t.Fatal("列关闭时开关应为假")
	}
	failed := providerConcurrencyTrackingFor(StoreOptions{},
		stubSwitchSettings{err: errors.New("boom")})
	if failed(ctx) {
		t.Fatal("读取失败时应按关闭处理（fail-closed 到不报数）")
	}
	if providerConcurrencyTrackingFor(StoreOptions{}, nil)(ctx) {
		t.Fatal("无设置读取面时应按关闭处理")
	}
	injected := providerConcurrencyTrackingFor(
		StoreOptions{ProviderConcurrencyTracking: alwaysTrackingEnabled},
		stubSwitchSettings{row: &store.SystemSettings{ProviderLiveStatsEnabled: false}})
	if !injected(ctx) {
		t.Fatal("调用方显式注入的替身应优先于设置快照")
	}
}

// 装配行：闸门必须从装配处拿到开关，而不是读构造期快照。
var providerConcurrencyTrackingWiring = regexp.MustCompile(
	`(?m)^\s*trackingEnabled:\s+providerConcurrencyTrackingFor\(options, adapters\.Settings\),\s*$`)

// 请求入口：登记缝不按开关短路（执法与开关无关，而这里还不知道本次会落到哪家）。
var providerConcurrencyRequestWiring = regexp.MustCompile(
	`(?m)^\s*fwd\.ProviderInFlight = h\.providerConcurrencyForRequest\(state\)\s*$`)

// TestProviderConcurrencyTrackingWiringNail 是**源码结构性钉子**：断言开关真的从装配处
// 接到了闸门上。
//
// 为什么必须有：把装配行改回 `options.ProviderConcurrencyTracking`（生产恒 nil，无人赋值）
// 或让请求入口按开关短路时，provider_concurrency_test.go 里那批用例**全绿**——闸门自身的
// 语义没变，变的只是「生产装配有没有把开关交到它手上」。这正是「已定义≠未接线」的盲区，
// 同类教训见同目录 slow_probe_wiring_nail_test.go、custom_headers_wiring_nail_test.go。
func TestProviderConcurrencyTrackingWiringNail(t *testing.T) {
	source, err := os.ReadFile("assemble.go")
	if err != nil {
		t.Fatalf("读取 assemble.go 失败：%v", err)
	}
	if !providerConcurrencyTrackingWiring.Match(source) {
		t.Fatalf("assemble.go 里找不到「全局开关 → 并发闸门」的接线：\n" +
			"  期望形如 `trackingEnabled: providerConcurrencyTrackingFor(options, adapters.Settings),`\n" +
			"  该行缺失或写成 options.ProviderConcurrencyTracking（生产恒 nil）时，\n" +
			"  开关永远读不到 system_settings 的列（闸门自身的用例不会红）。")
	}
	requestSource, err := os.ReadFile("dataplane.go")
	if err != nil {
		t.Fatalf("读取 dataplane.go 失败：%v", err)
	}
	if !providerConcurrencyRequestWiring.Match(requestSource) {
		t.Fatalf("dataplane.go 里登记缝的接线变了：\n" +
			"  期望形如 `fwd.ProviderInFlight = h.providerConcurrencyForRequest(state)`\n" +
			"  若这里按统计开关短路成 nil，配了上限的渠道会被 forward 整段跳过（漏判）。")
	}
}
