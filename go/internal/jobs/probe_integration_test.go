package jobs

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fanxcv/claude-code-hub-go/go/internal/health"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件把探活任务整轮跑在真实 PG 上：自建厂/供应商/端点（唯一域名与 URL，避免撞既有数据），
// 用 httptest 当上游，断言「快照列 + 探活历史」都落库。
//
// 为什么必须自建而不是复用库里既有端点：既有端点的 URL 指向真实上游，拨测结果不稳定；
// 且用例若改写它们的 last_probe_* 就是在污染别人的数据。自建的这几行只服务本用例，
// 按**精确 id** 逆序清理（历史 → 端点 → 供应商 → 厂）。
//
// 本文件里的调度类用例（拨测、探活历史清理）用 probeCycleRedis 拿客户端：它们必须成为分布式
// 调度锁的唯一持有者，而锁键是**生产常量**（locks:endpoint-probe-scheduler、locks:probe-log-cleanup），
// 切换期两侧（Node/Go）与任何在跑的实例都争同一把。
//
// 「谁是 leader」因此不由用例决定：另一个分支的 cchd、冒烟环境、甚至外部遗留的 Node 实例都能合法
// 持锁，此时 Run 返回 skipped=not_leader 是**正确**行为，用例却会报错（且旧写法在断言处 panic 成
// 「interface{} is nil, not int」，看不出真因——这批用例因此在共享环境长期飘红）。实测：外部遗留的
// Node 实例（.next/standalone，REDIS_URL 指向测试库）持锁并每 25s 续约，两个用例稳定红。
//
// 修法是**把锁键空间按用例切分**：probeCycleRedis 给客户端装一个 hook，把这两个锁键重写进本用例
// 独有的前缀。取锁/续约/释放仍走真 Redis 与真 Lua，只是同库其它实例再也抢不到本用例的键，
// 领导权回到用例自己手里；「他人持锁→跳过」的语义在同前缀内仍可确定性地构造（见 SkipsWhenLocked）。
//
// 本库仍需按**精确键名**清理，绝不 FLUSHDB：库里可能有别人的键。
const probeCycleRedisDB = 15

// lockKeyNamespaceHook 把调度锁键重写进用例独有前缀（前缀为空时行为与不装 hook 一致）。
type lockKeyNamespaceHook struct{ prefix string }

func (h lockKeyNamespaceHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (h lockKeyNamespaceHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		h.rewrite(cmd)
		return next(ctx, cmd)
	}
}

func (h lockKeyNamespaceHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, cmd := range cmds {
			h.rewrite(cmd)
		}
		return next(ctx, cmds)
	}
}

// rewrite 就地改写命令参数里的锁键。Eval 的键在 [3, 3+numKeys)，其余命令的键在 args[1]。
func (h lockKeyNamespaceHook) rewrite(cmd redis.Cmder) {
	args := cmd.Args()
	if len(args) < 2 {
		return
	}
	name, _ := args[0].(string)
	positions := []int{1}
	if name == "eval" || name == "evalsha" {
		numKeys, ok := args[2].(int)
		if !ok || len(args) < 3+numKeys {
			return
		}
		positions = positions[:0]
		for index := 0; index < numKeys; index++ {
			positions = append(positions, 3+index)
		}
	}
	for _, position := range positions {
		key, ok := args[position].(string)
		if !ok {
			continue
		}
		if key == EndpointProbeLockKey || key == ProbeLogCleanupLockKey {
			args[position] = h.prefix + key
		}
	}
}

// probeCycleRedis 取调度用例的 Redis 客户端（门控变量同 opsTestRedis），锁键空间按用例切分。
func probeCycleRedis(t *testing.T) redis.UniversalClient {
	t.Helper()
	raw := os.Getenv("CCH_TEST_REDIS_URL")
	if raw == "" {
		t.Skip("未设置 CCH_TEST_REDIS_URL，跳过 Redis 集成测试")
	}
	options, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("解析 Redis URL 失败: %v", err)
	}
	options.DB = probeCycleRedisDB
	client := redis.NewClient(options)
	client.AddHook(lockKeyNamespaceHook{prefix: "go-jobs-it-lock:" + opsFixtureKey(t) + ":"})
	t.Cleanup(func() { _ = client.Close() })
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("Redis 不可达: %v", err)
	}
	return client
}

// opsProbeInt 读一轮结果的整数字段；形状不符时报出可读错误。
//
// 旧写法直接 `.(int)`：一旦本轮走的是 skipped 分支（锁被占、无到期项），断言先 panic 成
// 「interface{} is nil, not int」，真因（本轮压根没拨测）反而被掩盖。
func opsProbeInt(t *testing.T, outcome OpsOutcome, key string) int {
	t.Helper()
	value, ok := outcome.Fields[key]
	if !ok {
		t.Fatalf("本轮结果缺少字段 %q，实际字段=%v", key, outcome.Fields)
	}
	number, ok := value.(int)
	if !ok {
		t.Fatalf("本轮结果字段 %q 应为 int，实际 %T(%v)", key, value, value)
	}
	return number
}

// opsProbeFixture 是一次用例自建的数据（逆序清理所需 id 都在这里）。
type opsProbeFixture struct {
	vendorID   int64
	providerID int64
	endpointID int64
	url        string
}

// opsCreateProbeFixture 建厂 + 启用供应商 + 启用端点，并登记精确清理。
//
// 三行缺一不可：FindProbeEndpoints 按「存在启用供应商的 (vendor, type)」门控端点，
// 没有启用供应商时端点根本不会进入拨测集合（那正是 #779/#781 的语义）。
func opsCreateProbeFixture(t *testing.T, pools *store.Pools, url string) opsProbeFixture {
	t.Helper()
	ctx := context.Background()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}

	// 域名取纳秒后缀：provider_vendors.website_domain 上有唯一索引，并发跑用例不能撞。
	domain := fmt.Sprintf("go-jobs-probe-%d.example.com", time.Now().UnixNano())

	var vendorID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO provider_vendors (website_domain, display_name) VALUES ($1, $2) RETURNING id`,
		domain, "go-jobs-probe",
	).Scan(&vendorID); err != nil {
		t.Fatalf("建厂失败: %v", err)
	}

	var providerID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO providers (name, url, key, is_enabled, provider_type, provider_vendor_id, priority)
		 VALUES ($1, $2, 'go-jobs-probe-key', true, 'claude', $3, 0) RETURNING id`,
		domain+"-provider", url, vendorID,
	).Scan(&providerID); err != nil {
		t.Fatalf("建供应商失败: %v", err)
	}

	var endpointID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO provider_endpoints (vendor_id, provider_type, url, is_enabled, sort_order)
		 VALUES ($1, 'claude', $2, true, 0) RETURNING id`,
		vendorID, url,
	).Scan(&endpointID); err != nil {
		t.Fatalf("建端点失败: %v", err)
	}

	t.Cleanup(func() {
		// 清理用**同一个池**（不会被自己关掉），并按精确 id 逆序删。
		cleanupCtx := context.Background()
		cleanupPool, poolErr := pools.Control()
		if poolErr != nil {
			t.Errorf("清理时取分道失败: %v", poolErr)
			return
		}
		if _, execErr := cleanupPool.Exec(cleanupCtx,
			`DELETE FROM provider_endpoint_probe_logs WHERE endpoint_id = $1`, endpointID); execErr != nil {
			t.Errorf("清理探活历史失败: %v", execErr)
		}
		if _, execErr := cleanupPool.Exec(cleanupCtx,
			`DELETE FROM provider_endpoints WHERE id = $1`, endpointID); execErr != nil {
			t.Errorf("清理端点失败: %v", execErr)
		}
		if _, execErr := cleanupPool.Exec(cleanupCtx,
			`DELETE FROM providers WHERE id = $1`, providerID); execErr != nil {
			t.Errorf("清理供应商失败: %v", execErr)
		}
		if _, execErr := cleanupPool.Exec(cleanupCtx,
			`DELETE FROM provider_vendors WHERE id = $1`, vendorID); execErr != nil {
			t.Errorf("清理厂失败: %v", execErr)
		}
	})

	return opsProbeFixture{vendorID: vendorID, providerID: providerID, endpointID: endpointID, url: url}
}

// opsOnlyTarget 把探活目标钉成**只有一个**：本用例自建的端点。
//
// 不这样做的话，任务会扫全库启用端点并真的去拨外部上游——用例既慢又不稳，
// 还会改写其它端点的 last_probe_* 快照列。
func opsOnlyTarget(fixture opsProbeFixture) func(context.Context) ([]store.ProbeEndpoint, error) {
	return func(context.Context) ([]store.ProbeEndpoint, error) {
		// 目标必须来自库内真实行（快照列由任务更新），而不是内存里编一个。
		return []store.ProbeEndpoint{{
			ID:           fixture.endpointID,
			URL:          fixture.url,
			VendorID:     fixture.vendorID,
			ProviderType: "claude",
		}}, nil
	}
}

// opsReadEndpointSnapshot 读回端点的探活快照列。
func opsReadEndpointSnapshot(t *testing.T, pools *store.Pools, endpointID int64) (
	string, *bool, *string, *string,
) {
	t.Helper()
	pool, err := pools.Data()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	var (
		probedAt  *time.Time
		ok        *bool
		errorType *string
		errorText *string
	)
	if err := pool.QueryRow(context.Background(),
		`SELECT last_probed_at, last_probe_ok, last_probe_error_type, last_probe_error_message
		 FROM provider_endpoints WHERE id = $1`, endpointID,
	).Scan(&probedAt, &ok, &errorType, &errorText); err != nil {
		t.Fatalf("读端点快照失败: %v", err)
	}
	if probedAt == nil {
		return "", ok, errorType, errorText
	}
	return probedAt.Format(time.RFC3339Nano), ok, errorType, errorText
}

// opsCountProbeLogs 数该端点的探活历史条数。
func opsCountProbeLogs(t *testing.T, pools *store.Pools, endpointID int64) int {
	t.Helper()
	pool, err := pools.Data()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	var count int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM provider_endpoint_probe_logs WHERE endpoint_id = $1`, endpointID,
	).Scan(&count); err != nil {
		t.Fatalf("统计探活历史失败: %v", err)
	}
	return count
}

// 探活成功：快照列写入、历史追加一条、熔断状态被归闭。
func TestIntegrationEndpointProbeSuccess(t *testing.T) {
	pools := opsTestPools(t)
	client := probeCycleRedis(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	fixture := opsCreateProbeFixture(t, pools, upstream.URL)

	// 先把该端点的熔断状态推成 open，验证「探活成功会归闭」而不只是「记一次成功」。
	healthWriter := health.NewWriter(health.Options{
		Redis:                         client,
		EndpointCircuitBreakerEnabled: true,
		Logger:                        opsTestLogger(t),
	})
	stateKey := "endpoint_circuit_breaker:state:" + strconv.FormatInt(fixture.endpointID, 10)
	t.Cleanup(func() { _ = client.Del(context.Background(), stateKey).Err() })
	if err := client.HSet(context.Background(), stateKey,
		"circuitState", "open", "failureCount", "3").Err(); err != nil {
		t.Fatalf("预置熔断状态失败: %v", err)
	}

	cfg := DefaultProbeConfig()
	// 用例只要一轮：基础间隔拉长，避免 tick 抖动把第二轮也带进来。
	cfg.BaseInterval = time.Hour
	cfg.Timeout = 2 * time.Second
	cfg.Concurrency = 1
	cfg.CycleJitter = 0

	deps := opsNewTestDeps(t, pools, client)
	deps.Health = healthWriter
	deps.ProbeTargets = opsOnlyTarget(fixture)

	outcome, err := NewEndpointProbe(deps, cfg).Run(context.Background())
	if err != nil {
		t.Fatalf("探活任务失败: %v", err)
	}
	// 注入唯一目标后，本轮必须恰好只拨测自建端点：多出来的就是在打真实上游。
	if opsProbeInt(t, outcome, "probed") != 1 || opsProbeInt(t, outcome, "ok") != 1 {
		t.Fatalf("本轮应恰好成功拨测一个端点，得到 %v", outcome.Fields)
	}

	probedAt, ok, errorType, errorText := opsReadEndpointSnapshot(t, pools, fixture.endpointID)
	if probedAt == "" {
		t.Fatal("探活后 last_probed_at 必须被写入")
	}
	if ok == nil || !*ok {
		t.Fatalf("可达端点应记为成功，得到 ok=%v", ok)
	}
	// 成功时错误列必须清空（Node：ok ? null : value），否则库里会留陈旧错误。
	if errorType != nil || errorText != nil {
		t.Fatalf("成功探活应清空错误列，得到 type=%v msg=%v", errorType, errorText)
	}

	if count := opsCountProbeLogs(t, pools, fixture.endpointID); count != 1 {
		t.Fatalf("应追加恰好一条探活历史，得到 %d", count)
	}

	// 归闭证据：状态键应被直接删除（Node resetEndpointCircuit）。
	exists, err := client.Exists(context.Background(), stateKey).Result()
	if err != nil {
		t.Fatalf("查熔断状态失败: %v", err)
	}
	if exists != 0 {
		t.Fatal("探活成功后端点熔断状态键应被删除")
	}
}

// 锁被他人持有时整轮跳过：不拨测、不落库（Node runProbeCycle 的 skipIfLocked 分支）。
//
// 这是「另一个实例正在拨测」的**合法**结果而不是缺陷：本用例把它钉成断言，
// 免得将来有人把 not_leader 当成异常改成重试或抢锁——那会让两个实例同时拨测同一批端点。
func TestIntegrationEndpointProbeSkipsWhenLocked(t *testing.T) {
	pools := opsTestPools(t)
	client := probeCycleRedis(t)
	ctx := context.Background()

	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	fixture := opsCreateProbeFixture(t, pools, upstream.URL)

	// 由「另一个实例」持锁：本用例自己占（取不到就说明别人已占，同样满足前提）。
	// 两者都确定性：持锁者不释放，Run 就不可能成为 leader。
	held, err := client.SetNX(ctx, EndpointProbeLockKey, "another-instance", 30*time.Second).Result()
	if err != nil {
		t.Fatalf("预置锁失败: %v", err)
	}
	if held {
		t.Cleanup(func() { _ = client.Del(context.Background(), EndpointProbeLockKey).Err() })
	}

	cfg := DefaultProbeConfig()
	cfg.BaseInterval = time.Hour
	cfg.Timeout = 2 * time.Second
	cfg.Concurrency = 1
	cfg.CycleJitter = 0

	deps := opsNewTestDeps(t, pools, client)
	deps.ProbeTargets = opsOnlyTarget(fixture)
	outcome, err := NewEndpointProbe(deps, cfg).Run(ctx)
	if err != nil {
		t.Fatalf("锁被占时不应报错: %v", err)
	}
	if outcome.Fields["skipped"] != "not_leader" {
		t.Fatalf("锁被他人持有时应跳过，得到 %v", outcome.Fields)
	}

	// 跳过的证据：快照列未写、历史未追加（不是「跑了但没落库」）。
	probedAt, ok, errorType, errorText := opsReadEndpointSnapshot(t, pools, fixture.endpointID)
	if probedAt != "" || ok != nil || errorType != nil || errorText != nil {
		t.Fatalf("跳过的一轮不得写快照列，得到 probedAt=%q ok=%v type=%v msg=%v",
			probedAt, ok, errorType, errorText)
	}
	if count := opsCountProbeLogs(t, pools, fixture.endpointID); count != 0 {
		t.Fatalf("跳过的一轮不得追加探活历史，得到 %d", count)
	}
}

// 探活失败：错误列写入、success 语义不成立，且历史仍要追加（历史是无条件写的）。
func TestIntegrationEndpointProbeFailure(t *testing.T) {
	pools := opsTestPools(t)
	client := probeCycleRedis(t)

	// 指向一个刚关掉的端口：连接必被拒绝。
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := closed.URL
	closed.Close()

	fixture := opsCreateProbeFixture(t, pools, closedURL)

	cfg := DefaultProbeConfig()
	cfg.BaseInterval = time.Hour
	cfg.Timeout = 2 * time.Second
	cfg.Concurrency = 1
	cfg.CycleJitter = 0

	failDeps := opsNewTestDeps(t, pools, client)
	failDeps.ProbeTargets = opsOnlyTarget(fixture)
	outcome, err := NewEndpointProbe(failDeps, cfg).Run(context.Background())
	if err != nil {
		t.Fatalf("探活任务失败: %v", err)
	}
	if opsProbeInt(t, outcome, "probed") != 1 || opsProbeInt(t, outcome, "failed") != 1 {
		t.Fatalf("本轮应恰好失败拨测一个端点，得到 %v", outcome.Fields)
	}

	probedAt, ok, errorType, errorText := opsReadEndpointSnapshot(t, pools, fixture.endpointID)
	if probedAt == "" {
		t.Fatal("失败也要写 last_probed_at")
	}
	if ok == nil || *ok {
		t.Fatalf("不可达端点应记为失败，得到 ok=%v", ok)
	}
	if errorType == nil || *errorType == "" {
		t.Fatal("失败必须记 error_type")
	}
	if errorText == nil || *errorText == "" {
		t.Fatal("失败必须记 error_message")
	}
	if count := opsCountProbeLogs(t, pools, fixture.endpointID); count != 1 {
		t.Fatalf("失败也要追加一条探活历史（Node 已移除过滤逻辑），得到 %d", count)
	}
}

// 端点在拨测期间被删除：整轮不得中断，也不得撞 FK。
func TestIntegrationEndpointProbeToleratesDeletedEndpoint(t *testing.T) {
	pools := opsTestPools(t)
	client := probeCycleRedis(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	fixture := opsCreateProbeFixture(t, pools, upstream.URL)
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	// 直接删除端点（模拟管理面/级联删除发生在拨测途中）。
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM provider_endpoints WHERE id = $1`, fixture.endpointID); err != nil {
		t.Fatalf("删除端点失败: %v", err)
	}

	cfg := DefaultProbeConfig()
	cfg.BaseInterval = time.Hour
	cfg.Timeout = 2 * time.Second
	cfg.Concurrency = 1
	cfg.CycleJitter = 0

	// 关键断言：不返回错误、不 panic。端点已不在库里，因此本轮不会把它算作到期项；
	// 即便竞态下仍被拨测，落库也会命中「更新 0 行」分支而直接返回，不写历史（否则撞 FK）。
	deletedDeps := opsNewTestDeps(t, pools, client)
	deletedDeps.ProbeTargets = opsOnlyTarget(fixture)
	// 目标仍指向已删除的端点：这正是竞态下会发生的事（扫描后、拨测前被删）。
	if _, err := NewEndpointProbe(deletedDeps, cfg).Run(context.Background()); err != nil {
		t.Fatalf("端点被删不该让整轮失败: %v", err)
	}
	if count := opsCountProbeLogs(t, pools, fixture.endpointID); count != 0 {
		t.Fatalf("端点已删除时不应留下孤儿历史，得到 %d 条", count)
	}
}

// 探活历史清理：只删早于保留窗口的行（自己造的旧行必删，新行必须留着）。
func TestIntegrationProbeLogCleanupRespectsRetention(t *testing.T) {
	pools := opsTestPools(t)
	client := probeCycleRedis(t)
	ctx := context.Background()

	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	fixture := opsCreateProbeFixture(t, pools, upstream.URL)
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}

	// 两条历史：一条 10 天前（应被删），一条刚刚（应保留）。
	for _, createdAt := range []string{"now() - interval '10 days'", "now()"} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO provider_endpoint_probe_logs
				(endpoint_id, source, ok, created_at) VALUES ($1, 'manual', true, `+createdAt+`)`,
			fixture.endpointID,
		); err != nil {
			t.Fatalf("插入探活历史失败: %v", err)
		}
	}

	deps := opsNewTestDeps(t, pools, client)
	cleanup := NewProbeLogCleanup(deps, ProbeLogCleanupConfig{
		Enabled:   true,
		Retention: 24 * time.Hour,
		BatchSize: 10_000,
		LockTTL:   5 * time.Minute,
	})
	outcome, err := cleanup.Run(ctx)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if outcome.Processed < 1 {
		t.Fatalf("应至少删除一条过期历史，得到 %v", outcome.Fields)
	}

	// 新行必须还在（保留窗口内的行不该被误删）。
	if count := opsCountProbeLogs(t, pools, fixture.endpointID); count != 1 {
		t.Fatalf("保留窗口内的历史应保留 1 条，得到 %d", count)
	}
}

// 探活历史清理的锁被占用时跳过（切换期 Node 与 Go 只有一个能跑清理）。
func TestIntegrationProbeLogCleanupSkipsWhenLocked(t *testing.T) {
	pools := opsTestPools(t)
	client := probeCycleRedis(t)
	ctx := context.Background()

	// 由「另一个实例」持锁。
	if err := client.Set(ctx, ProbeLogCleanupLockKey, "other-owner", time.Minute).Err(); err != nil {
		t.Fatalf("预置锁失败: %v", err)
	}
	t.Cleanup(func() { _ = client.Del(context.Background(), ProbeLogCleanupLockKey).Err() })

	cleanup := NewProbeLogCleanup(opsNewTestDeps(t, pools, client), ProbeLogCleanupConfig{
		Enabled:   true,
		Retention: time.Hour,
		BatchSize: 100,
		LockTTL:   time.Minute,
	})
	outcome, err := cleanup.Run(ctx)
	if err != nil {
		t.Fatalf("清理失败: %v", err)
	}
	if outcome.Fields["skipped"] != "not_leader" {
		t.Fatalf("他人持锁时应跳过，得到 %v", outcome.Fields)
	}
}
