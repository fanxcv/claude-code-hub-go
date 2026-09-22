package adminapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"testing"

	"github.com/fanxcv/claude-code-hub-go/go/internal/logx"
	"github.com/fanxcv/claude-code-hub-go/go/internal/route"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件钉住 `/providers/health` 的**实时并发数**投影与它的**全局开关**。
//
// 四条纪律（每条都有对应的失败模式）：
//  1. 开关关闭 → 并发字段为 null，且**一条 Redis 计数命令都不发**（「关上完全不占用资源」
//     在服务侧的那一半）；
//  2. 开关开启 → 真读数透出，**0 也是读数**（不能与「没开统计」同形）；
//  3. 开关**逐请求读**：同一进程内改库后立刻生效（反的是构造期快照那个坑）；
//  4. 读设置失败 / 未装配 → 不报数，也不去 Redis 发计数命令。
//
// 假读面计数调用次数，以此断言「没查」（比断言结果更硬：结果为 0 也可能是查了但没数据）。
// 熔断面走真 Redis 替身（`circuitTestRedis`）——它是本端点的先决条件，用假替身会缺 Pipeline。

// countingObservedSessions 是计数替身：记录本进程内 ProviderInFlightCounts 被调用的次数。
type countingObservedSessions struct {
	calls        int
	providerWise map[int64]int
	err          error
}

func (f *countingObservedSessions) ObservedSessionCount(_ context.Context) (int, error) {
	return 0, f.err
}

func (f *countingObservedSessions) ObservedSessionIdentities(_ context.Context) ([]string, error) {
	return nil, f.err
}

func (f *countingObservedSessions) ProviderInFlightCounts(
	_ context.Context,
	providerIDs []int64,
) (map[int64]int, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	counts := make(map[int64]int, len(providerIDs))
	for _, id := range providerIDs {
		counts[id] = f.providerWise[id]
	}
	return counts, nil
}

// liveStatsTestPools 取测试库；未设置 DSN 时跳过。
//
// 与 testPools 的差别只有一处：本文件的用例要写 system_settings，故需要真库。
func liveStatsTestPools(t *testing.T) *store.Pools {
	t.Helper()
	if os.Getenv("CCH_TEST_DSN") == "" {
		t.Skip("未设置 CCH_TEST_DSN，跳过数据库集成测试")
	}
	return testPools(t)
}

// setLiveStatsSwitch 直接写库改全局开关，缺行则先建一行。
//
// 为何不用管理面 PUT：本文件要钉的是**读侧逐请求读**，写路径的契约已由 system_settings
// 自己的用例覆盖。直写库能确定性地把开关翻到指定值。
//
// 为何要建行：`Pools.FindSystemSettings` 对空表返回 ErrNotFound，而 handler 对读不到
// 设置按关闭处理（fail-closed）——空表的测试库会让「开启」的用例永远看不到读数。
// 真部署里 system_settings 必有一行（迁移 0047 就建了它）。
//
// 无需清缓存：`FindSystemSettings` 是直读单行，没有缓存层——这正是「改完立即生效」
// 的机制（见 providers_health.go 里那段注释）。
func setLiveStatsSwitch(t *testing.T, pools *store.Pools, enabled bool) {
	t.Helper()
	pool, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	ctx := context.Background()
	var count int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM system_settings`).Scan(&count); err != nil {
		t.Fatalf("数 system_settings 行失败: %v", err)
	}
	if count == 0 {
		if _, err := pool.Exec(ctx,
			`INSERT INTO system_settings (provider_live_stats_enabled) VALUES ($1)`, enabled); err != nil {
			t.Fatalf("建 system_settings 行失败: %v", err)
		}
		return
	}
	if _, err := pool.Exec(ctx,
		`UPDATE system_settings SET provider_live_stats_enabled = $1`, enabled); err != nil {
		t.Fatalf("改统计开关失败: %v", err)
	}
}

// liveStatsCall 发一次 /providers/health 并取回指定渠道的并发投影。
func liveStatsCall(t *testing.T, router *Router, providerID int64) *providerConcurrencyHealth {
	t.Helper()
	status, body := circuitCall(t, router, http.MethodGet, "/providers/health", "", true)
	if status != http.StatusOK {
		t.Fatalf("health 应为 200，收到 %d：%.300s", status, body)
	}
	var snapshot map[string]struct {
		Concurrency *providerConcurrencyHealth `json:"concurrency"`
	}
	if err := json.Unmarshal([]byte(body), &snapshot); err != nil {
		t.Fatalf("解析 health 响应失败: %v（原文 %.300s）", err, body)
	}
	entry, ok := snapshot[strconv.FormatInt(providerID, 10)]
	if !ok {
		t.Fatalf("响应里没有渠道 %d：%.300s", providerID, body)
	}
	return entry.Concurrency
}

// liveStatsRouter 建一个只关心并发维的 Router：熔断面走真 Redis，读面用给定替身。
func liveStatsRouter(
	t *testing.T,
	pools *store.Pools,
	sessions ObservedSessionRuntime,
	extra func(*Deps),
) *Router {
	t.Helper()
	client := circuitTestRedis(t)
	deps := Deps{
		Logger:           logx.New(nil),
		Guard:            principalGuard{principal: Principal{UserID: 1, Username: "admin", IsAdmin: true}},
		Store:            pools,
		Problems:         NewProblems(nil),
		CircuitStates:    NewRedisCircuitStates(client, nil, nil),
		ObservedSessions: sessions,
	}
	if extra != nil {
		extra(&deps)
	}
	router := New(Options{Deps: deps})
	RegisterProviders(router, deps)
	return router
}

// TestProviderConcurrencyProjectionStates 是**纯单测**（不依赖 PG/Redis），钉住四态。
// CI 不注入 DSN/Redis，故集成用例整组跳过——本用例是那份判据在 CI 里的常驻哨兵。
func TestProviderConcurrencyProjectionStates(t *testing.T) {
	// 开关关闭：整段不存在（**不是** 0）。
	if got := providerConcurrencyProjection(false, true, false, 5); got != nil {
		t.Errorf("开关关闭时应为 null，收到 %+v", got)
	}
	// 未装配读面：整段不存在。
	if got := providerConcurrencyProjection(true, false, false, 5); got != nil {
		t.Errorf("未装配读面时应为 null，收到 %+v", got)
	}
	// 读失败：available=false 且不得给数（尤其不得是 0）。
	failed := providerConcurrencyProjection(true, true, true, 0)
	if failed == nil || failed.Available || failed.ActiveSessions != nil {
		t.Errorf("读失败应为 available=false 且无读数，收到 %+v", failed)
	}
	if failed != nil && (failed.UnavailableReason == nil || *failed.UnavailableReason != "redis_unavailable") {
		t.Errorf("读失败应带原因，收到 %+v", failed)
	}
	// 正常读数为 0：必须是**可用读数**（指针非空），否则与「没开统计」同形。
	idle := providerConcurrencyProjection(true, true, false, 0)
	if idle == nil || !idle.Available || idle.ActiveSessions == nil || *idle.ActiveSessions != 0 {
		t.Errorf("0 应是可用读数，收到 %+v", idle)
	}
	// 正常读数非零。
	busy := providerConcurrencyProjection(true, true, false, 7)
	if busy == nil || !busy.Available || busy.ActiveSessions == nil || *busy.ActiveSessions != 7 {
		t.Errorf("非零读数应透出，收到 %+v", busy)
	}
	if busy != nil && !busy.TrackingEnabled {
		t.Error("开启时 TrackingEnabled 必须为真")
	}
}

// TestProvidersHealthConcurrencyIsNullWhenTrackingOff 钉住「关闭即不存在」：
// 字段为 null，且**读面一次都没被调用**（一条计数命令都不发）。
func TestProvidersHealthConcurrencyIsNullWhenTrackingOff(t *testing.T) {
	pools := liveStatsTestPools(t)
	fixture := seedProviders(t, pools)
	setLiveStatsSwitch(t, pools, false)
	t.Cleanup(func() { setLiveStatsSwitch(t, pools, false) })

	sessions := &countingObservedSessions{providerWise: map[int64]int{fixture.enabledID: 3}}
	router := liveStatsRouter(t, pools, sessions, nil)

	if got := liveStatsCall(t, router, fixture.enabledID); got != nil {
		t.Errorf("统计关闭时并发字段应为 null，收到 %+v", got)
	}
	if sessions.calls != 0 {
		t.Errorf("统计关闭时不得查询计数键，实际调用读面 %d 次", sessions.calls)
	}
}

// TestProvidersHealthConcurrencyIsReadWhenTrackingOn 钉住开启后真读数透出，
// 且 **0 是合法读数**（不能与「没开统计」同形）。
func TestProvidersHealthConcurrencyIsReadWhenTrackingOn(t *testing.T) {
	pools := liveStatsTestPools(t)
	fixture := seedProviders(t, pools)
	setLiveStatsSwitch(t, pools, true)
	t.Cleanup(func() { setLiveStatsSwitch(t, pools, false) })

	sessions := &countingObservedSessions{providerWise: map[int64]int{fixture.enabledID: 7}}
	router := liveStatsRouter(t, pools, sessions, nil)

	busy := liveStatsCall(t, router, fixture.enabledID)
	if busy == nil || !busy.TrackingEnabled || !busy.Available {
		t.Fatalf("开启后应带可用读数，收到 %+v", busy)
	}
	if busy.ActiveSessions == nil || *busy.ActiveSessions != 7 {
		t.Errorf("并发数应为 7，收到 %+v", busy.ActiveSessions)
	}

	// 另一家没有在飞请求：0 必须是**有效读数**，否则前端会把「空闲」与「没开统计」画成一样。
	idle := liveStatsCall(t, router, fixture.otherID)
	if idle == nil || !idle.TrackingEnabled || !idle.Available {
		t.Fatalf("开启后无在飞的渠道也应是可用读数，收到 %+v", idle)
	}
	if idle.ActiveSessions == nil || *idle.ActiveSessions != 0 {
		t.Errorf("空闲渠道的并发数应为 0（指针非空），收到 %+v", idle.ActiveSessions)
	}
	if sessions.calls == 0 {
		t.Error("开启时应当查询计数键")
	}
}

// TestProvidersHealthConcurrencySwitchIsPerRequest 钉住**热更新**：同一进程内改库后
// 下一次请求立即生效，无需重启。
//
// 反的是仓内已踩过的坑（affinityIgnoreClientSessionId 曾是启动期快照，管理面改完
// 返回 200 而运行中行为不变）。把开关读成构造期快照会让本用例第二次断言失败。
func TestProvidersHealthConcurrencySwitchIsPerRequest(t *testing.T) {
	pools := liveStatsTestPools(t)
	fixture := seedProviders(t, pools)
	setLiveStatsSwitch(t, pools, false)
	t.Cleanup(func() { setLiveStatsSwitch(t, pools, false) })

	sessions := &countingObservedSessions{providerWise: map[int64]int{fixture.enabledID: 4}}
	router := liveStatsRouter(t, pools, sessions, nil)

	if got := liveStatsCall(t, router, fixture.enabledID); got != nil {
		t.Fatalf("初始关闭时并发字段应为 null，收到 %+v", got)
	}

	// 同一个 Router（同一个进程）内翻开关：下一次请求必须看到新值。
	setLiveStatsSwitch(t, pools, true)
	got := liveStatsCall(t, router, fixture.enabledID)
	if got == nil || !got.TrackingEnabled {
		t.Fatalf("改库后应立即生效（无需重启），收到 %+v", got)
	}
	if got.ActiveSessions == nil || *got.ActiveSessions != 4 {
		t.Errorf("改库后应读到 4，收到 %+v", got.ActiveSessions)
	}
}

// TestProvidersHealthConcurrencyReadFailureIsNotZero 钉住读失败不用 0 冒充
// （与 slowRate 的 available 同一条纪律）。
func TestProvidersHealthConcurrencyReadFailureIsNotZero(t *testing.T) {
	pools := liveStatsTestPools(t)
	fixture := seedProviders(t, pools)
	setLiveStatsSwitch(t, pools, true)
	t.Cleanup(func() { setLiveStatsSwitch(t, pools, false) })

	sessions := &countingObservedSessions{err: errors.New("redis down")}
	router := liveStatsRouter(t, pools, sessions, nil)

	got := liveStatsCall(t, router, fixture.enabledID)
	if got == nil || !got.TrackingEnabled {
		t.Fatalf("开启时字段应在（只是读不到），收到 %+v", got)
	}
	if got.Available {
		t.Error("读失败时 Available 必须为假")
	}
	if got.ActiveSessions != nil {
		t.Errorf("读失败时不得给出并发数（尤其不得是 0），收到 %d", *got.ActiveSessions)
	}
	if got.UnavailableReason == nil || *got.UnavailableReason != "redis_unavailable" {
		t.Errorf("读失败应带原因，收到 %+v", got.UnavailableReason)
	}
}

// TestProvidersHealthConcurrencyUnwiredIsNull 钉住未装配读面时整段不显示。
func TestProvidersHealthConcurrencyUnwiredIsNull(t *testing.T) {
	pools := liveStatsTestPools(t)
	fixture := seedProviders(t, pools)
	setLiveStatsSwitch(t, pools, true)
	t.Cleanup(func() { setLiveStatsSwitch(t, pools, false) })

	router := liveStatsRouter(t, pools, nil, nil)

	if got := liveStatsCall(t, router, fixture.enabledID); got != nil {
		t.Errorf("未装配读面时并发字段应为 null，收到 %+v", got)
	}
}

// TestProviderSlowRateAndConcurrencyCoexist 钉住两维**互不干扰**：
// slowRate 的读面在而并发关着时，slowRate 照旧有值、并发为 null。
//
// 为何值得单钉：两维共用同一次响应拼装，且并发那段复用了 readErr 变量——
// 若哪次改动让并发分支把 slowRate 的读数一起清掉，本条会红。
func TestProviderSlowRateAndConcurrencyCoexist(t *testing.T) {
	pools := liveStatsTestPools(t)
	fixture := seedProviders(t, pools)
	setLiveStatsSwitch(t, pools, false)

	values := map[string]string{}
	writeSlowRateState(values, fixture.enabledID, "model-a", 40, "primary")
	client := &slowRateHealthFakeRedis{values: values}

	router := liveStatsRouter(t, pools, &countingObservedSessions{
		providerWise: map[int64]int{fixture.enabledID: 5},
	}, func(deps *Deps) {
		deps.ProviderSlowRates = NewRedisProviderSlowRates(client,
			route.NewSlowRateReader(route.SlowRateOptions{Redis: client}), nil)
	})

	status, body := circuitCall(t, router, http.MethodGet, "/providers/health", "", true)
	if status != http.StatusOK {
		t.Fatalf("health 应为 200，收到 %d：%.300s", status, body)
	}
	var snapshot map[string]struct {
		SlowRate    *providerSlowRateHealth    `json:"slowRate"`
		Concurrency *providerConcurrencyHealth `json:"concurrency"`
	}
	if err := json.Unmarshal([]byte(body), &snapshot); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	entry := snapshot[strconv.FormatInt(fixture.enabledID, 10)]
	if entry.SlowRate == nil || entry.SlowRate.Penalty == nil || *entry.SlowRate.Penalty != 40 {
		t.Errorf("并发关闭不得影响慢率读数，收到 %+v", entry.SlowRate)
	}
	if entry.Concurrency != nil {
		t.Errorf("并发关闭时该字段应为 null，收到 %+v", entry.Concurrency)
	}
}
