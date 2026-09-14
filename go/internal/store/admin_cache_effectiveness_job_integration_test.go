package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// 本文件是真库集成用例（`CCH_TEST_DSN` 未设置时整组跳过），覆盖**只有真库才能证明**的部分：
// 聚合 SQL 的过滤/分组/定点整数数学、窗口边界的左闭右开、以及水位（MAX(window_end)）。
//
// 与 Node 的对照口径：期望值都是按 `src/lib/cache-effectiveness/service.ts:104-145` 的公式
// 手算出来的常量（不是把 Go 的输出回抄一遍），故断言能抓住「公式被改动」。
//
// 夹具纪律（与 internal/jobs 的集成测试同一条）：只用唯一 key/合成 provider_id，并按标记精确清理；
// 插入 message_request 会触发账本与 outbox 触发器，删除夹具行时触发器会一并回收。
const (
	cacheEffectivenessITKeyPrefix = "go-cacheeff-it-"
	// cacheEffectivenessITProviderBase 是合成 provider_id 起点：远离真实供应商 id，便于断言与清理。
	cacheEffectivenessITProviderBase int64 = 990001
)

type cacheEffectivenessFixture struct {
	providerID int64
	model      string
	// bucket 为空串时写入 NULL（验证 COALESCE(cache_ttl_bucket,'5m')）。
	bucket string
	// compatibilityKey 为空串时写入 NULL（该列非空是聚合的前置过滤条件）。
	compatibilityKey string
	eligible         bool
	theoretical      int64
	observed         int64
	createdAt        time.Time
	deleted          bool
}

// insertCacheEffectivenessFixture 插入一行 message_request（只给聚合关心的列）。
func insertCacheEffectivenessFixture(
	t *testing.T,
	pools *Pools,
	key string,
	spec cacheEffectivenessFixture,
) {
	t.Helper()
	ctx := context.Background()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取连接失败: %v", err)
	}
	var bucket, compatibilityKey any
	if spec.bucket != "" {
		bucket = spec.bucket
	}
	if spec.compatibilityKey != "" {
		compatibilityKey = spec.compatibilityKey
	}
	var deletedAt any
	if spec.deleted {
		deletedAt = spec.createdAt
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO message_request (
			provider_id, user_id, key, model,
			cache_ttl_bucket, cache_compatibility_key,
			cache_score_eligible, theoretical_cache_tokens, cache_read_input_tokens,
			deleted_at, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		spec.providerID, int64(1), key, spec.model,
		bucket, compatibilityKey,
		spec.eligible, spec.theoretical, spec.observed,
		deletedAt, spec.createdAt,
	); err != nil {
		t.Fatalf("插入夹具行失败: %v", err)
	}
}

// cacheEffectivenessFixtureCleanup 注册清理：先删夹具请求行（触发器回收账本/outbox），
// 再删本次产出的聚合行。
func cacheEffectivenessFixtureCleanup(t *testing.T, pools *Pools, keyPrefix, modelPrefix string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		pool, err := pools.Control()
		if err != nil {
			t.Logf("清理时取连接失败（夹具残留需人工清理）: %v", err)
			return
		}
		if _, err := pool.Exec(ctx,
			`DELETE FROM message_request WHERE key LIKE $1`, keyPrefix+"%"); err != nil {
			t.Logf("清理夹具请求行失败: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`DELETE FROM provider_cache_effectiveness WHERE model LIKE $1`, modelPrefix+"%"); err != nil {
			t.Logf("清理聚合行失败: %v", err)
		}
	})
}

type cacheEffectivenessAggregateRow struct {
	sampleCount             int
	eligibleCount           int
	theoreticalCacheTokens  int64
	observedCacheReadTokens int64
	rawEffectivenessBp      int
	confidenceBp            int
	effectivenessBp         int
}

// readCacheEffectivenessAggregate 读回某一分组在给定窗口内的聚合行（不存在则 ok=false）。
func readCacheEffectivenessAggregate(
	t *testing.T,
	pools *Pools,
	providerID int64,
	model string,
	bucket string,
	windowStart time.Time,
	windowEnd time.Time,
) (cacheEffectivenessAggregateRow, bool) {
	t.Helper()
	ctx := context.Background()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取连接失败: %v", err)
	}
	var row cacheEffectivenessAggregateRow
	err = pool.QueryRow(ctx, `
		SELECT sample_count, eligible_count, theoretical_cache_tokens, observed_cache_read_tokens,
		       raw_effectiveness_bp, confidence_bp, effectiveness_bp
		FROM provider_cache_effectiveness
		WHERE provider_id = $1 AND model = $2 AND cache_ttl_bucket = $3
		  AND window_start = $4 AND window_end = $5`,
		providerID, model, bucket, windowStart, windowEnd,
	).Scan(
		&row.sampleCount, &row.eligibleCount, &row.theoreticalCacheTokens, &row.observedCacheReadTokens,
		&row.rawEffectivenessBp, &row.confidenceBp, &row.effectivenessBp,
	)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return cacheEffectivenessAggregateRow{}, false
		}
		t.Fatalf("读回聚合行失败: %v", err)
	}
	return row, true
}

// runCacheEffectivenessAggregate 在一个事务里跑一次聚合（与 jobs 任务的调用形状一致：
// 取锁 → 聚合一窗口）。返回写入分组数。
func runCacheEffectivenessAggregate(
	t *testing.T,
	pools *Pools,
	windowStart time.Time,
	windowEnd time.Time,
) int {
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

	acquired, err := pools.TryCacheEffectivenessXactLock(ctx, tx)
	if err != nil {
		t.Fatalf("取锁失败: %v", err)
	}
	if !acquired {
		t.Fatalf("同一事务内首次取锁应当成功")
	}
	written, err := pools.InsertCacheEffectivenessWindow(ctx, tx, windowStart, windowEnd)
	if err != nil {
		t.Fatalf("聚合写入失败: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	return written
}

// TestIntegrationInsertCacheEffectivenessWindowComputesFixedPointMetrics 钉住定点整数数学与样本量分档。
//
// 期望值手算（Node service.ts:104-145），五组分别覆盖：
//   - 基本命中率（5 档样本量 3000）与未截断;
//   - theoretical=0 → raw 恒 0;
//   - observed 超过 theoretical → raw 被 clamp 到 10000;
//   - 样本量 100 档（本用例用 30 行 + 全合格 → 6000 档）与 1 档（1000）;
//   - 不合格样本只进 sample、不进 eligible（从而压低 confidence）。
func TestIntegrationInsertCacheEffectivenessWindowComputesFixedPointMetrics(t *testing.T) {
	pools := openTestPools(t)
	keyPrefix := cacheEffectivenessITKeyPrefix + t.Name() + "-"
	modelPrefix := "go-cacheeff-metrics-"
	cacheEffectivenessFixtureCleanup(t, pools, keyPrefix, modelPrefix)

	windowStart := time.Now().UTC().Add(-6 * time.Hour).Truncate(time.Second)
	windowEnd := windowStart.Add(time.Minute)
	at := windowStart.Add(10 * time.Second)

	// 组 1：5 行全合格，theoretical 合计 1000、observed 合计 250。
	// raw = 250*10000/1000 = 2500；observable = 5*10000/5 = 10000；factor(5)=3000；
	// confidence = 10000*3000/10000 = 3000；effectiveness = 2500*3000/10000 = 750。
	g1 := modelPrefix + "basic"
	for i := 0; i < 5; i++ {
		insertCacheEffectivenessFixture(t, pools, keyPrefix+"g1", cacheEffectivenessFixture{
			providerID: cacheEffectivenessITProviderBase + 1, model: g1, bucket: "5m",
			compatibilityKey: "ck-g1", eligible: true, theoretical: 200, observed: 50, createdAt: at,
		})
	}
	// 组 2：theoretical = 0 → raw 恒 0（防除零），effectiveness 也是 0。
	g2 := modelPrefix + "zero-theoretical"
	insertCacheEffectivenessFixture(t, pools, keyPrefix+"g2", cacheEffectivenessFixture{
		providerID: cacheEffectivenessITProviderBase + 2, model: g2, bucket: "5m",
		compatibilityKey: "ck-g2", eligible: true, theoretical: 0, observed: 0, createdAt: at,
	})
	// 组 3：30 行全合格、observed 远超 theoretical → raw clamp 到 10000；factor(30)=6000；
	// confidence = 10000*6000/10000 = 6000；effectiveness = 10000*6000/10000 = 6000。
	g3 := modelPrefix + "clamped"
	for i := 0; i < 30; i++ {
		insertCacheEffectivenessFixture(t, pools, keyPrefix+"g3", cacheEffectivenessFixture{
			providerID: cacheEffectivenessITProviderBase + 3, model: g3, bucket: "1h",
			compatibilityKey: "ck-g3", eligible: true, theoretical: 100, observed: 500, createdAt: at,
		})
	}
	// 组 4：4 行里只 1 行合格 → sample=4、eligible=1（factor(1)=1000）；
	// 注意令牌合计**只累加合格行**（Node 的 `SUM(...) FILTER (WHERE cache_score_eligible)`），
	// 故 theoretical/observed 都是 100 而非 4×100=400（首版手算错了这一格，被本用例拦住）。
	// theoretical=100、observed=100 → raw = 10000；observable = 1*10000/4 = 2500；
	// confidence = 2500*1000/10000 = 250；effectiveness = 10000*250/10000 = 250。
	g4 := modelPrefix + "mixed-eligibility"
	for i := 0; i < 4; i++ {
		insertCacheEffectivenessFixture(t, pools, keyPrefix+"g4", cacheEffectivenessFixture{
			providerID: cacheEffectivenessITProviderBase + 4, model: g4, bucket: "",
			compatibilityKey: "ck-g4", eligible: i == 0, theoretical: 100, observed: 100, createdAt: at,
		})
	}

	written := runCacheEffectivenessAggregate(t, pools, windowStart, windowEnd)
	if written < 4 {
		t.Fatalf("应至少写入 4 个分组，实际 %d", written)
	}

	type expect struct {
		model                                string
		providerID                           int64
		bucket                               string
		sampleCount, eligibleCount           int
		theoretical, observed                int64
		rawBp, confidenceBp, effectivenessBp int
	}
	for _, tc := range []expect{
		{g1, cacheEffectivenessITProviderBase + 1, "5m", 5, 5, 1000, 250, 2500, 3000, 750},
		{g2, cacheEffectivenessITProviderBase + 2, "5m", 1, 1, 0, 0, 0, 1000, 0},
		{g3, cacheEffectivenessITProviderBase + 3, "1h", 30, 30, 3000, 15000, 10000, 6000, 6000},
		{g4, cacheEffectivenessITProviderBase + 4, "5m", 4, 1, 100, 100, 10000, 250, 250},
	} {
		row, ok := readCacheEffectivenessAggregate(
			t, pools, tc.providerID, tc.model, tc.bucket, windowStart, windowEnd)
		if !ok {
			t.Fatalf("分组未写出：provider=%d model=%s bucket=%s", tc.providerID, tc.model, tc.bucket)
		}
		if row.sampleCount != tc.sampleCount || row.eligibleCount != tc.eligibleCount {
			t.Fatalf("%s 计数不符：sample=%d eligible=%d，期望 %d/%d",
				tc.model, row.sampleCount, row.eligibleCount, tc.sampleCount, tc.eligibleCount)
		}
		if row.theoreticalCacheTokens != tc.theoretical || row.observedCacheReadTokens != tc.observed {
			t.Fatalf("%s 令牌合计不符：theoretical=%d observed=%d，期望 %d/%d",
				tc.model, row.theoreticalCacheTokens, row.observedCacheReadTokens,
				tc.theoretical, tc.observed)
		}
		if row.rawEffectivenessBp != tc.rawBp ||
			row.confidenceBp != tc.confidenceBp ||
			row.effectivenessBp != tc.effectivenessBp {
			t.Fatalf("%s 定点值不符：raw=%d confidence=%d effectiveness=%d，期望 %d/%d/%d",
				tc.model, row.rawEffectivenessBp, row.confidenceBp, row.effectivenessBp,
				tc.rawBp, tc.confidenceBp, tc.effectivenessBp)
		}
	}
}

// TestIntegrationInsertCacheEffectivenessWindowBoundaries 钉住窗口边界的左闭右开与三处排除条件。
func TestIntegrationInsertCacheEffectivenessWindowBoundaries(t *testing.T) {
	pools := openTestPools(t)
	keyPrefix := cacheEffectivenessITKeyPrefix + t.Name() + "-"
	modelPrefix := "go-cacheeff-boundary-"
	cacheEffectivenessFixtureCleanup(t, pools, keyPrefix, modelPrefix)

	windowStart := time.Now().UTC().Add(-8 * time.Hour).Truncate(time.Second)
	windowEnd := windowStart.Add(time.Hour)
	model := modelPrefix + "edges"
	providerID := cacheEffectivenessITProviderBase + 10

	base := cacheEffectivenessFixture{providerID: providerID, model: model, bucket: "5m",
		compatibilityKey: "ck-edge", eligible: true, theoretical: 10, observed: 5}

	// 计入：起点（左闭）、终点前 1 微秒。
	in1 := base
	in1.createdAt = windowStart
	insertCacheEffectivenessFixture(t, pools, keyPrefix+"in1", in1)

	in2 := base
	in2.createdAt = windowEnd.Add(-time.Microsecond)
	insertCacheEffectivenessFixture(t, pools, keyPrefix+"in2", in2)

	in3 := base
	in3.createdAt = windowStart.Add(30 * time.Minute)
	insertCacheEffectivenessFixture(t, pools, keyPrefix+"in3", in3)

	// 排除：终点（右开）。
	out1 := base
	out1.createdAt = windowEnd
	insertCacheEffectivenessFixture(t, pools, keyPrefix+"out1", out1)

	// 排除：起点前 1 微秒。
	out2 := base
	out2.createdAt = windowStart.Add(-time.Microsecond)
	insertCacheEffectivenessFixture(t, pools, keyPrefix+"out2", out2)

	// 排除：cache_compatibility_key IS NULL。
	out3 := base
	out3.createdAt = windowStart.Add(5 * time.Minute)
	out3.compatibilityKey = ""
	insertCacheEffectivenessFixture(t, pools, keyPrefix+"out3", out3)

	// 排除：provider_id = 0。
	out4 := base
	out4.createdAt = windowStart.Add(5 * time.Minute)
	out4.providerID = 0
	insertCacheEffectivenessFixture(t, pools, keyPrefix+"out4", out4)

	// 排除：软删。
	out5 := base
	out5.createdAt = windowStart.Add(5 * time.Minute)
	out5.deleted = true
	insertCacheEffectivenessFixture(t, pools, keyPrefix+"out5", out5)

	runCacheEffectivenessAggregate(t, pools, windowStart, windowEnd)

	row, ok := readCacheEffectivenessAggregate(t, pools, providerID, model, "5m", windowStart, windowEnd)
	if !ok {
		t.Fatal("边界分组未写出")
	}
	// 恰 3 行计入：终点右开、起点左闭、NULL 兼容键/零供应商/软删三处排除都必须生效。
	if row.sampleCount != 3 {
		t.Fatalf("窗口左闭右开或排除条件失效：sample_count=%d，期望 3（左闭 1 + 中间 1 + 右开前 1）",
			row.sampleCount)
	}
	if row.eligibleCount != 3 {
		t.Fatalf("eligible_count=%d，期望 3", row.eligibleCount)
	}
}

// TestIntegrationLastCacheEffectivenessWindowEndTracksMax 钉住水位：MAX(window_end) 是下一轮起点，
// 而**不是**最新插入的行（Node 用 MAX 而非 DESC 首行——两者只在窗口非单调插入时分歧）。
func TestIntegrationLastCacheEffectivenessWindowEndTracksMax(t *testing.T) {
	pools := openTestPools(t)
	keyPrefix := cacheEffectivenessITKeyPrefix + t.Name() + "-"
	modelPrefix := "go-cacheeff-watermark-"
	cacheEffectivenessFixtureCleanup(t, pools, keyPrefix, modelPrefix)

	earlier := time.Now().UTC().Add(-10 * time.Hour).Truncate(time.Second)
	later := earlier.Add(2 * time.Hour)
	earlierEnd := earlier.Add(time.Minute)
	laterEnd := later.Add(time.Minute)
	model := modelPrefix + "row"

	// 两个窗口各需**一行落在窗口内**的夹具：InsertCacheEffectivenessWindow 是按
	// [window_start, window_end) 从 message_request 聚合后再写入，窗口内没有夹具行时
	// **一个分组都写不出来**，水位便退化成库里遗留行的最大窗口。
	// 本用例首版把唯一的夹具行写在 earlier+1m（恰好是右开端点），两个窗口都空，于是
	// 水位取到历史行 2026-09-13 00:13:03+08，早于本用例的较晚窗口而假红。
	insertCacheEffectivenessFixture(t, pools, keyPrefix+"wm-earlier", cacheEffectivenessFixture{
		providerID: cacheEffectivenessITProviderBase + 20, model: model, bucket: "5m",
		compatibilityKey: "ck-wm", eligible: true, theoretical: 10, observed: 5,
		createdAt: earlier.Add(30 * time.Second),
	})
	insertCacheEffectivenessFixture(t, pools, keyPrefix+"wm-later", cacheEffectivenessFixture{
		providerID: cacheEffectivenessITProviderBase + 20, model: model, bucket: "5m",
		compatibilityKey: "ck-wm", eligible: true, theoretical: 10, observed: 5,
		createdAt: later.Add(30 * time.Second),
	})

	// 先写较晚的窗口，再写较早的——若实现改成「取最新插入行」，本用例会读回较早的那个。
	// written==0 说明夹具没落进窗口，那是**本用例自己的前置条件**失效，而非水位语义问题。
	if written := runCacheEffectivenessAggregate(t, pools, later, laterEnd); written == 0 {
		t.Fatal("较晚窗口未写出分组：夹具行没落在窗口内，本用例前置条件失效")
	}
	if written := runCacheEffectivenessAggregate(t, pools, earlier, earlierEnd); written == 0 {
		t.Fatal("较早窗口未写出分组：夹具行没落在窗口内，本用例前置条件失效")
	}

	ctx := context.Background()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取连接失败: %v", err)
	}
	// 两次读取必须同一快照：REPEATABLE READ 下函数与直查看到同一份数据，「水位 == 全局 MAX」
	// 才是可判定的事实；READ COMMITTED 下两条语句各取一次快照，并发写入者会让它随机不等。
	tx, err := pool.Raw().BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	lastEnd, err := pools.LastCacheEffectivenessWindowEnd(ctx, tx)
	if err != nil {
		t.Fatalf("读水位失败: %v", err)
	}
	if lastEnd == nil {
		t.Fatal("水位不应为空")
	}
	// 先钉住夹具确实进了表（按 model 前缀收窄）；这一步把「库里遗留的更晚窗口」与
	// 「本用例自己写没写进去」彻底分开——前者只会抬高全局水位，后者才是断言的前提。
	var fixtureMax time.Time
	if err := tx.QueryRow(ctx,
		`SELECT MAX(window_end) FROM provider_cache_effectiveness WHERE model LIKE $1`,
		modelPrefix+"%",
	).Scan(&fixtureMax); err != nil {
		t.Fatalf("直查夹具窗口失败: %v", err)
	}
	if !fixtureMax.Equal(laterEnd) {
		t.Fatalf("夹具窗口水位 = %s，期望较晚窗口终点 %s", fixtureMax, laterEnd)
	}
	// 直查必须在**同一事务**内进行：控制道只有 1 条连接（`SplitPoolBudget(6)` 的 control=1），
	// 持着 tx 再向池里借第二条会永久阻塞（本用例首版就死在这里，实测 600s 超时）。
	// 我用合成 provider 造了两个窗口，但真实库可能已有更晚的窗口；断言取的是**全局最大**。
	var globalMax time.Time
	if err := tx.QueryRow(ctx, `SELECT MAX(window_end) FROM provider_cache_effectiveness`).Scan(&globalMax); err != nil {
		t.Fatalf("直查最大窗口失败: %v", err)
	}
	if !lastEnd.Equal(globalMax) {
		t.Fatalf("水位 = %s，直查 MAX(window_end) = %s，两者必须逐位一致", lastEnd, globalMax)
	}
	if lastEnd.Before(laterEnd) {
		t.Fatalf("水位 %s 早于本用例写入的较晚窗口 %s：MAX 语义失效", lastEnd, laterEnd)
	}
}

// TestIntegrationCacheEffectivenessXactLockExcludesConcurrentRun 钉住事务级锁的跨会话互斥：
// 另一条连接持锁期间，本连接必须拿不到（Node 同样直接跳过而非排队）。
func TestIntegrationCacheEffectivenessXactLockExcludesConcurrentRun(t *testing.T) {
	pools := openTestPools(t)
	ctx := context.Background()

	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取连接失败: %v", err)
	}
	// 持锁方走**专用连接**（独立单连接池，不计入分道预算）：控制道只有 1 条连接，
	// 若持锁方也走分道池，竞争方会阻塞在「借连接」上而不是快速拿到「锁被占了」的结论，
	// 用例会变成「挂住」而不是「断言失败」（本用例首版就这样挂了 120s）。
	// 这与生产同形：jobs.AcquireLeader 同样借专用连接持锁。
	holderConn, holderPool, err := pools.OpenDedicatedConn(ctx)
	if err != nil {
		t.Fatalf("取专用连接失败: %v", err)
	}
	t.Cleanup(func() {
		// 顺序要紧：`pgxpool.Close` 会等所有连接归还，先 Close 再 Release 会永久挂住
		// （本用例首版就挂在 Cleanup 里，表现为「测试超时」而非断言失败）。
		holderConn.Release()
		holderPool.Close()
	})
	holder, err := holderConn.Begin(ctx)
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

	contender, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("开启竞争事务失败: %v", err)
	}
	defer func() { _ = contender.Rollback(ctx) }()
	got, err := pools.TryCacheEffectivenessXactLock(ctx, contender)
	if err != nil {
		t.Fatalf("竞争取锁出错: %v", err)
	}
	if got {
		t.Fatal("另一事务持锁期间不得取到锁：跨语言互斥是双跑期的硬约束")
	}

	if err := holder.Rollback(ctx); err != nil {
		t.Fatalf("释放持锁事务失败: %v", err)
	}
	after, err := pools.TryCacheEffectivenessXactLock(ctx, contender)
	if err != nil {
		t.Fatalf("释放后取锁出错: %v", err)
	}
	if !after {
		t.Fatal("持锁事务结束后应能取到锁（事务级锁随事务结束释放）")
	}
}

// TestIntegrationCacheEffectivenessEnabledSetting 钉住开关读取的**三态**：
// 未设置（NULL）与 false 必须可区分——前者回落 env，后者是显式关闭。
func TestIntegrationCacheEffectivenessEnabledSetting(t *testing.T) {
	pools := openTestPools(t)
	ctx := context.Background()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取连接失败: %v", err)
	}

	// 本用例只读不改：断言「读回来的值与直查一致」以及「NULL 被解成 nil 而非 false」。
	setting, err := pools.CacheEffectivenessEnabledSetting(ctx)
	if err != nil {
		t.Fatalf("读开关失败: %v", err)
	}
	var direct *bool
	if err := pool.QueryRow(ctx,
		`SELECT cache_effectiveness_enabled FROM system_settings ORDER BY id ASC LIMIT 1`).Scan(&direct); err != nil {
		t.Fatalf("直查开关失败: %v", err)
	}
	switch {
	case direct == nil && setting != nil:
		t.Fatalf("库里为 NULL 时应返回 nil，实际 %v", *setting)
	case direct != nil && setting == nil:
		t.Fatal("库里有值时不应返回 nil")
	case direct != nil && setting != nil && *direct != *setting:
		t.Fatalf("读回 %v，直查 %v", *setting, *direct)
	}
	// 三态语义的判定本身在 jobs 包的无库单测里覆盖（cacheEffectivenessEnabled 表驱动）。
	if setting == nil {
		t.Log("当前库该设置为 NULL：回落到 ENABLE_CACHE_EFFECTIVENESS")
	}
}
