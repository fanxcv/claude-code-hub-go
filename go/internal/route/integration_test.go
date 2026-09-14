package route

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/convert"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
	"github.com/redis/go-redis/v9"
)

const testRedisEnv = "CCH_TEST_REDIS_URL"

// integrationPools 读门控变量建池；未设置 CCH_TEST_DSN 时整组集成测试跳过。
func integrationPools(t *testing.T) *store.Pools {
	t.Helper()
	dsn := os.Getenv("CCH_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 CCH_TEST_DSN，跳过数据库集成测试")
	}
	pools, err := store.Open(context.Background(), store.Options{
		DSN:                 dsn,
		Budget:              config.SplitPoolBudget(6),
		Timeouts:            config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
		ApplicationNameBase: "cch-route-it",
	})
	if err != nil {
		t.Fatalf("建立连接池失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	return pools
}

// integrationRedis 读门控变量建 Redis 客户端；未设置时跳过。
// 库号固定落在 13：URL 自带库号时以其为准，否则显式切到 13，避免碰生产键空间。
func integrationRedis(t *testing.T) redis.UniversalClient {
	t.Helper()
	raw := os.Getenv(testRedisEnv)
	if raw == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过 Redis 集成测试")
	}
	options, err := redis.ParseURL(raw)
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
	return client
}

// TestIntegrationStoreSourceReadsRoutingColumns 用真实库断言选路视图的四列确实读得到，
// 且与 store 只读视图逐字段一致（本包已不自持 SQL，只剩类型桥接）：
// provider_vendor_id（vendor-type 熔断与厂级端点）、group_priorities（分组优先级覆盖）、
// protocol_conversion_enabled（跨协议可服务）、disable_session_reuse（会话粘性 opt-out）。
func TestIntegrationStoreSourceReadsRoutingColumns(t *testing.T) {
	pools := integrationPools(t)
	source := NewStoreSource(pools)
	ctx := context.Background()

	providers, err := source.Providers(ctx)
	if err != nil {
		t.Fatalf("读取供应商失败: %v", err)
	}
	if len(providers) == 0 {
		t.Skip("库中没有启用态供应商，跳过该断言")
	}

	for _, p := range providers {
		encoded, err := json.Marshal(p)
		if err != nil {
			t.Fatalf("供应商序列化失败: %v", err)
		}
		var asMap map[string]any
		if err := json.Unmarshal(encoded, &asMap); err != nil {
			t.Fatalf("供应商反序列化失败: %v", err)
		}
		for _, column := range []string{
			"id", "name", "provider_type", "weight", "priority", "cost_multiplier",
			"group_tag", "group_priorities", "allowed_models", "provider_vendor_id",
			"protocol_conversion_enabled", "disable_session_reuse",
		} {
			if _, ok := asMap[column]; !ok {
				t.Fatalf("选路视图缺少列 %q（供应商 %d）", column, p.ID)
			}
		}

		// 桥接忠实性：同一行分开读两遍（走本包接口 vs 直接走 store）必须逐字段相同，
		// 否则说明桥接自己造了值，而真实库里的值被丢掉。
		viaSource, fromStore, reclaimed, err := routeFixtureBridge(ctx, source, pools, p.ID)
		if err != nil {
			t.Fatalf("按 id 读取供应商失败: %v", err)
		}
		if reclaimed {
			continue
		}
		assertRouteFixtureBridge(t, p.ID, viaSource, fromStore)
	}

	first := providers[0]
	if first.ProviderVendorID != nil && *first.ProviderVendorID > 0 {
		if _, err := source.Endpoints(ctx, *first.ProviderVendorID, first.ProviderType); err != nil {
			t.Fatalf("读取厂级端点失败: %v", err)
		}
	}
}

// TestIntegrationSelectProducesChainItem 在真实快照上跑一轮选路，断言产出的链项可原样落库
// （JSON 形态、决策上下文计数自洽）。
func TestIntegrationSelectProducesChainItem(t *testing.T) {
	pools := integrationPools(t)
	selector := NewSelector(Options{
		Source: NewStoreSource(pools),
		Rand:   func() float64 { return 0.5 },
	})

	result, err := selector.Select(context.Background(), Request{Model: "claude-3-5-sonnet", Format: convert.FormatClaude})
	if err != nil {
		t.Fatalf("选路失败: %v", err)
	}
	item := result.ChainItem()
	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("链项序列化失败: %v", err)
	}
	if !strings.Contains(string(encoded), `"decisionContext"`) {
		t.Fatalf("链项应携带决策上下文: %s", encoded)
	}
	ctx := result.Context
	if ctx.BeforeHealthCheck > ctx.TotalProviders {
		t.Errorf("beforeHealthCheck (%d) 不应大于 totalProviders (%d)", ctx.BeforeHealthCheck, ctx.TotalProviders)
	}
	if ctx.AfterHealthCheck > ctx.BeforeHealthCheck {
		t.Errorf("afterHealthCheck (%d) 不应大于 beforeHealthCheck (%d)", ctx.AfterHealthCheck, ctx.BeforeHealthCheck)
	}
	if result.Provider == nil {
		t.Logf("当前库中无可用供应商，仅断言留痕结构自洽")
		return
	}
	if result.Provider.ID != item.ID {
		t.Errorf("链项 id 与选中项不一致")
	}
}

// TestIntegrationHealthAndAffinityAgainstRedis 用真实 Redis 验证键形制与命中语义。
// 全程使用合成 ID 与合成 scope，测试结束即删除自己写入的键。
func TestIntegrationHealthAndAffinityAgainstRedis(t *testing.T) {
	client := integrationRedis(t)
	ctx := context.Background()

	const probeProviderID int64 = 999000101
	stateKey := ProviderStateKeyPrefix + strconv.FormatInt(probeProviderID, 10)
	if err := client.HSet(ctx, stateKey, map[string]any{
		"circuitState":     "open",
		"circuitOpenUntil": strconv.FormatInt(time.Now().Add(time.Hour).UnixMilli(), 10),
		"failureCount":     "6",
	}).Err(); err != nil {
		t.Fatalf("写入熔断状态失败: %v", err)
	}
	t.Cleanup(func() { _ = client.Del(context.Background(), stateKey).Err() })

	reader := NewHealthReader(HealthOptions{Redis: client})
	open, reason := reader.ProviderOpen(ctx, probeProviderID)
	if !open || reason != "" {
		t.Fatalf("Node 形制的 circuit_breaker:state 键应被读成 open，实际 open=%v reason=%q", open, reason)
	}
	if state := reader.ProviderState(ctx, probeProviderID); state != StateOpen {
		t.Fatalf("状态快照应为 open，实际 %q", state)
	}
	if open, _ := reader.ProviderOpen(ctx, probeProviderID+1); open {
		t.Fatalf("无状态供应商应判为 closed")
	}

	scope := "routeit" + strconv.FormatInt(time.Now().UnixNano(), 10)
	fp := hash32(scope)
	key := affinityKeyPrefix + "{" + scope + "}:fp:" + fp
	if err := client.Set(ctx, key, "1|7|idfp|v3:gen", time.Minute).Err(); err != nil {
		t.Fatalf("写入亲和键失败: %v", err)
	}
	t.Cleanup(func() { _ = client.Del(context.Background(), key).Err() })

	store := NewAffinityStore(AffinityOptions{Redis: client, Window: 8, SlidingTTLSeconds: 60})
	lookup, ok := store.Lookup(ctx, scope, []string{fp})
	if !ok || lookup.Hint == nil {
		t.Fatalf("Node 形制的 cch:pfx 键应被命中: %+v ok=%v", lookup, ok)
	}
	if lookup.Hint.ProviderID != 7 || lookup.Hint.MatchedIndex != 0 {
		t.Fatalf("命中结果不符: %+v", lookup.Hint)
	}
	if lookup.IdentityFP != "idfp" || lookup.Generation != "v3:gen" {
		t.Fatalf("四段值应原样带回 identity/generation: %q / %q", lookup.IdentityFP, lookup.Generation)
	}
}

// routeFixtureBridge 复读同一行：走本包接口一遍、直接走 store 一遍，并判定这一行是否已被并发夹具回收。
//
// 共享库上「先列表、再按 id 复读」天然会撞并发的删除：此刻另一个包的集成测试（例如
// internal/dataplane 的供应商夹具）或另一个 worktree 里的同一套用例正在按精确 id 回收自己的行。
// 行已消失就没有「桥接是否忠实」可言，交由调用方跳过——不当成失败。
//
// 本包的 StoreSource.Provider 会把 store.ErrNotFound 包成带上下文的错误而丢掉哨兵值，
// 故「已回收」一律以 store 侧复读为准。
func routeFixtureBridge(
	ctx context.Context,
	source *StoreSource,
	pools *store.Pools,
	id int64,
) (viaSource *Provider, fromStore *store.Provider, reclaimed bool, err error) {
	viaSource, err = source.Provider(ctx, id)
	if err != nil {
		if _, storeErr := pools.FindProviderByID(ctx, id); errors.Is(storeErr, store.ErrNotFound) {
			return nil, nil, true, nil
		}
		return nil, nil, false, err
	}
	fromStore, err = pools.FindProviderByID(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil, true, nil
		}
		return nil, nil, false, err
	}
	return viaSource, fromStore, false, nil
}

// assertRouteFixtureBridge 断言桥接忠实性：同一行两遍读法（本包接口 vs 直接 store）必须逐字段相同。
func assertRouteFixtureBridge(t *testing.T, id int64, viaSource *Provider, fromStore *store.Provider) {
	t.Helper()
	// group_priorities 现在按**原文**过 store 视图（脏值不得让整行失败，见 store.DecodeGroupPriorities），
	// 而选路侧的类型是解码后的 map：故桥接忠实性按「同一条解码规则」比——用同一个解码器解
	// store 原文再比，并顺带比诊断条目数（脏值的可见性也是桥接契约的一部分，钉子见
	// group_priorities_dirty_test.go）。
	decoded, issues := store.DecodeGroupPriorities(fromStore.GroupPriorities)
	if !sameInt64Pointer(viaSource.ProviderVendorID, fromStore.ProviderVendorID) ||
		viaSource.ConversionEnabled() != fromStore.ProtocolConversionEnabled ||
		viaSource.DisableSessionReuse != fromStore.DisableSessionReuse ||
		viaSource.EffectivePriority() != fromStore.Priority ||
		!reflect.DeepEqual(viaSource.GroupPriorities, decoded) ||
		len(viaSource.groupPrioritiesIssues) != len(issues) {
		t.Fatalf("选路视角与 store 视图不一致（供应商 %d）: route=%+v store=%+v（解出覆盖 %v，问题 %v）",
			id, viaSource, fromStore, decoded, issues)
	}
}

// routeChurnBatch 每次扰动插入的启用态供应商行数。
const routeChurnBatch = 24

// routeChurnVisibleWindow 扰动行「已提交但尚未回收」的可见窗口：只读用例正是在这个窗口里
// 列表读到它、复读时又看不到它。窗口取毫秒级即可，钉子不靠真实长等待。
const routeChurnVisibleWindow = 5 * time.Millisecond

// routeChurnPriority 扰动行与本 lane 夹具的优先级：刻意排在最低档（远低于内部夹具共用的
// leftover 档），确保它们不会被任何并发用例的选路选中——本 lane 只需要它们出现在
// 「启用态供应商」列表里。allowed_models 只投一个不存在于任何用例的模型，作为第二道保险；
// URL 指向死端口，万一被选中也是快速失败而非挂住。
const routeChurnPriority = -2_000_000

// routeChurnModel 夹具行声明的模型：任何用例都不会请求它，故这些行只影响「列全部启用态供应商」的
// 用例（本 lane 的目标），不会被任何选路用例选中。
const routeChurnModel = `["route-it-churn-no-such-model"]`

// routeChurnRounds 复读轮数的硬上限；每轮都是一次「列表 → 按 id 复读」。
// 共享库上启用态供应商可达数百行，故最早撞上一次并发回收且已跑够 routeChurnMinRounds 轮就收工。
const routeChurnRounds = 60

// routeChurnMinRounds 提前收工的下限轮数：至少跑这么多轮，避免刚开头撞上一次就收工而没跑出覆盖。
const routeChurnMinRounds = 20

// routeChurnPrefix 生成本 lane 的夹具名前缀（含 pid 与纳秒），命名空间独占。
func routeChurnPrefix() string {
	return fmt.Sprintf("route-it-churn-%d-%d", os.Getpid(), time.Now().UnixNano())
}

// insertRouteChurnProviders 插入一批启用态供应商夹具，等到可见窗口结束才按精确 id 回收，
// 复刻其它集成测试在「列表 → 按 id 复读」之间插删自己夹具的行为。
// goroutine 里不能调 t.Fatalf，故错误一律上抛；已插入的 id 一并返回以便调用方兜底回收。
func insertRouteChurnProviders(
	ctx context.Context,
	pools *store.Pools,
	prefix string,
	batch int,
) ([]int64, error) {
	writer, err := pools.Writer()
	if err != nil {
		return nil, err
	}
	rows, err := writer.Query(ctx, `
		INSERT INTO providers (name, url, key, provider_type, is_enabled, weight, priority, cost_multiplier, allowed_models)
		SELECT $1 || '-' || g, 'http://127.0.0.1:1/v1/messages', 'route-it-churn-key',
		       'openai-compatible', true, 1, $2, 1.0, $4::jsonb
		FROM generate_series(1, $3) AS g
		RETURNING id`, prefix, routeChurnPriority, batch, routeChurnModel)
	if err != nil {
		return nil, err
	}
	ids := make([]int64, 0, batch)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return ids, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ids, err
	}
	time.Sleep(routeChurnVisibleWindow)
	if _, err := writer.Exec(ctx, `DELETE FROM providers WHERE id = ANY($1)`, ids); err != nil {
		return ids, err
	}
	return ids, nil
}

// deleteRouteChurnProviders 按精确 id 兜底回收（重复执行无害）。
// 用 pools 自己的分道池：它在 integrationPools 的 t.Cleanup 里才关闭，且本清理先注册、后执行。
func deleteRouteChurnProviders(t *testing.T, pools *store.Pools, ids []int64) {
	t.Helper()
	if len(ids) == 0 {
		return
	}
	writer, err := pools.Writer()
	if err != nil {
		t.Logf("回收扰动夹具失败（共享库会留下 %d 行）: %v", len(ids), err)
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := writer.Exec(cleanupCtx, `DELETE FROM providers WHERE id = ANY($1)`, ids); err != nil {
		t.Logf("回收扰动夹具失败（共享库会留下 %d 行）: %v", len(ids), err)
	}
}

// TestIntegrationProviderBridgeSurvivesConcurrentFixtureReclaim 是共享库上的抗扰钉子。
//
// 只读用例要在「列表 → 按 id 复读」之间保证桥接忠实，而共享库上别的集成测试会在同一窗口里
// 插入并回收自己的供应商夹具。本钉子把这件并发事搬进本进程：夹具存活期间持续插入并回收同表
// 其它启用态行，断言 ① 桥接断言链全程不判失败（回收的行被跳过），② 本测试自己的夹具全程
// 读得到且一致（它是本测试唯一会删的行）。
func TestIntegrationProviderBridgeSurvivesConcurrentFixtureReclaim(t *testing.T) {
	pools := integrationPools(t)
	source := NewStoreSource(pools)
	ctx := context.Background()
	prefix := routeChurnPrefix()

	pinnedID := insertRouteChurnFixture(t, pools, prefix)

	stop := make(chan struct{})
	var (
		churnMu     sync.Mutex
		churnIDs    []int64
		churnErr    error
		churnCycles int
		churnWg     sync.WaitGroup
	)
	churnWg.Add(1)
	go func() {
		defer churnWg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			ids, err := insertRouteChurnProviders(ctx, pools, prefix, routeChurnBatch)
			churnMu.Lock()
			churnIDs = append(churnIDs, ids...)
			churnCycles++
			if err != nil && churnErr == nil {
				churnErr = err
			}
			churnMu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		churnWg.Wait()
		churnMu.Lock()
		ids := append([]int64(nil), churnIDs...)
		err := churnErr
		churnMu.Unlock()
		if err != nil {
			t.Errorf("并发扰动失败: %v", err)
		}
		deleteRouteChurnProviders(t, pools, ids)
	})

	var (
		checked       int
		sawChurnRow   bool
		sawReclaim    bool
		churnRowCount int
		rounds        int
	)
	for round := 0; round < routeChurnRounds; round++ {
		rounds = round + 1
		providers, err := source.Providers(ctx)
		if err != nil {
			t.Fatalf("第 %d 轮读取供应商失败: %v", round, err)
		}
		for _, p := range providers {
			if strings.HasPrefix(p.Name, prefix) {
				sawChurnRow = true
				churnRowCount++
			}
			viaSource, fromStore, reclaimed, err := routeFixtureBridge(ctx, source, pools, p.ID)
			if err != nil {
				t.Fatalf("第 %d 轮按 id 读取供应商 %d 失败: %v", round, p.ID, err)
			}
			if reclaimed {
				sawReclaim = true
				continue
			}
			assertRouteFixtureBridge(t, p.ID, viaSource, fromStore)
			checked++
		}
		if sawReclaim && round >= routeChurnMinRounds {
			break
		}
	}

	if checked == 0 {
		t.Skip("库里没有可复读的启用态供应商，抗扰钉子未生效")
	}
	if !sawChurnRow {
		t.Skipf("并发窗口一次都没撞上（复读 %d 行），钉子无法证伪", checked)
	}

	// 自己的夹具必须整轮存活：它是本测试唯一会删的行，若能读回却判定「已回收」，说明容忍逻辑放过了真错。
	viaSource, fromStore, reclaimed, err := routeFixtureBridge(ctx, source, pools, pinnedID)
	if err != nil {
		t.Fatalf("读回夹具供应商失败: %v", err)
	}
	if reclaimed {
		t.Fatalf("夹具供应商 %d 在抗扰期间不应被回收", pinnedID)
	}
	assertRouteFixtureBridge(t, pinnedID, viaSource, fromStore)

	churnMu.Lock()
	cycles := churnCycles
	churnMu.Unlock()
	t.Logf("抗扰钉子生效：复读 %d 行、撞见扰动行 %d 次、容忍回收 %v、共 %d 轮复读、扰动 %d 轮",
		checked, churnRowCount, sawReclaim, rounds, cycles)
}

// insertRouteChurnFixture 建本钉子自己的供应商夹具：启用态、最低档、死端口，按精确 id 回收。
func insertRouteChurnFixture(t *testing.T, pools *store.Pools, prefix string) int64 {
	t.Helper()
	writer, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	var id int64
	if err := writer.QueryRow(context.Background(), `
		INSERT INTO providers (name, url, key, provider_type, is_enabled, weight, priority, cost_multiplier, allowed_models)
		VALUES ($1, 'http://127.0.0.1:1/v1/messages', 'route-it-churn-key',
		        'openai-compatible', true, 1, $2, 1.0, $3::jsonb)
		RETURNING id`, prefix+"-pinned", routeChurnPriority, routeChurnModel).Scan(&id); err != nil {
		t.Fatalf("建供应商夹具失败: %v", err)
	}
	t.Cleanup(func() { deleteRouteChurnProviders(t, pools, []int64{id}) })
	return id
}

func sameInt64Pointer(left *int64, right *int64) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
