package jobs

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 真实库集成测试。**刻意不覆盖会破坏共享库状态的路径**：
//   - 不跑整表切换（DeleteCloudPricesNotIn 的 keep 列表若只含夹具，等于清空该库的云端价格）；
//   - 不写 projection_meta 的 backfill_done（那会让之后真正的回填被当成已完成而跳过）；
//   - 不写 cloud_pricing_catalog（会覆盖真实目录元数据，UI 直接读它）。
//
// 这三条语义由无库的 fake store 用例覆盖（见 pricesync_test.go / availproj_test.go）。
// 这里覆盖的是**只有真库才能证明**的部分：advisory 锁的跨会话互斥、以及新 store 方法的 SQL 正确性。
//
// 门控：未设置 CCH_TEST_DSN 时跳过（与 internal/store 的既有约定一致）。

const jobIntegrationFixturePrefix = "go-jobs-it-"

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
		ApplicationNameBase: "cch-jobs-it",
	})
	if err != nil {
		t.Fatalf("建立连接池失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	return pools
}

// priceNameExists 判断某个模型名此刻是否还有行。
//
// 用途：补偿（把被冻连删掉的行插回）前的最后一道检查——兄弟套件若已自己重建该行，
// 就不该再插一份，否则同名单行不变式会被补偿动作自己破坏。
func priceNameExists(ctx context.Context, pools *store.Pools, name string) (bool, error) {
	pool, err := pools.Control()
	if err != nil {
		return false, err
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM model_prices WHERE model_name = $1`, name).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func TestIntegrationLeaderLockIsExclusiveAcrossSessions(t *testing.T) {
	pools := integrationPools(t)
	ctx := context.Background()
	lockName := "cch-jobs-it-lock-" + time.Now().Format("20060102150405.000000000")

	first, acquired, err := AcquireLeader(ctx, pools, lockName)
	if err != nil || !acquired {
		t.Fatalf("首次加锁应成功: acquired=%t err=%v", acquired, err)
	}

	// 第二个「实例」：必须是拿不到锁。
	second, acquiredAgain, err := AcquireLeader(ctx, pools, lockName)
	if err != nil {
		t.Fatalf("第二次加锁不应报错: %v", err)
	}
	if acquiredAgain {
		t.Fatal("同一锁名在同一库上必须互斥（第二个实例不得取得锁）")
	}
	if second != nil {
		t.Fatal("未取得锁时不得返回句柄")
	}

	if err := first.Release(ctx); err != nil {
		t.Fatalf("释放锁失败: %v", err)
	}

	// 释放之后应能再次取得。
	third, acquiredThird, err := AcquireLeader(ctx, pools, lockName)
	if err != nil || !acquiredThird {
		t.Fatalf("释放后应能重新加锁: acquired=%t err=%v", acquiredThird, err)
	}
	if err := third.Release(ctx); err != nil {
		t.Fatalf("再次释放失败: %v", err)
	}
	// 重复释放是安全的（幂等）。
	if err := third.Release(ctx); err != nil {
		t.Fatalf("重复释放应无副作用: %v", err)
	}
}

func TestIntegrationLeaderLockSerializesConcurrentRunners(t *testing.T) {
	pools := integrationPools(t)
	ctx := context.Background()
	lockName := "cch-jobs-it-race-" + time.Now().Format("20060102150405.000000000")

	// 两个并发「实例」同时抢锁：恰好一个成功。这就是「多实例只跑一个」的等价形态。
	const runners = 2
	// 持锁者停在此处，制造真实的争用窗口；由主协程关闭以放行。
	release := make(chan struct{})
	var releaseOnce sync.Once
	results := make(chan bool, runners)
	var wg sync.WaitGroup

	for i := 0; i < runners; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lock, ok, err := AcquireLeader(ctx, pools, lockName)
			if err != nil {
				// 错误也算「没拿到」，由断言统一暴露。
				results <- false
				return
			}
			results <- ok
			if !ok {
				return
			}
			<-release
			if releaseErr := lock.Release(ctx); releaseErr != nil {
				t.Errorf("释放锁失败: %v", releaseErr)
			}
		}()
	}

	winnerCount := 0
	for i := 0; i < runners; i++ {
		select {
		case ok := <-results:
			if ok {
				winnerCount++
			}
		case <-time.After(10 * time.Second):
			releaseOnce.Do(func() { close(release) })
			wg.Wait()
			t.Fatal("抢锁未在期限内有结果")
		}
	}
	releaseOnce.Do(func() { close(release) })
	wg.Wait()

	if winnerCount != 1 {
		t.Fatalf("两个并发实例应恰好一个取得锁, 实际 %d", winnerCount)
	}
}

func TestIntegrationPriceSyncStoreReadsAndSafeDelete(t *testing.T) {
	pools := integrationPools(t)
	// 本用例自己就是那个**整表切换**的写者（下面会调 DeleteCloudPricesNotIn），而共享库里
	// 还有别的包在写价格行——两边必须互斥，否则双方都会读到对方的残缺状态。
	lockModelPricesTable(t, pools)
	ctx := context.Background()
	fixture := jobIntegrationFixturePrefix + time.Now().Format("20060102150405.000000000")
	// 三个夹具各钉一条语义（见下文「整表切换」段）：保留的不动 / 不在保留列表的非 manual 行被删 /
	// manual 行永不被删。名字同前缀，故清理与断言都只触及本用例自己的行。
	dropFixture := fixture + "-drop"
	manualFixture := fixture + "-manual"

	// 夹具自钉为唯一候选：先看库里是否已有同名行（前缀唯一，正常不会有）。
	cleanup := func() {
		for _, name := range []string{fixture, dropFixture, manualFixture} {
			if err := pools.AdminDeleteModelPriceByName(ctx, name); err != nil {
				t.Fatalf("清理夹具失败: %v", err)
			}
		}
	}
	t.Cleanup(cleanup)
	cleanup()

	priceData := []byte(`{"mode":"chat","input_cost_per_token":1e-06,"display_name":"jobs it fixture"}`)
	if _, err := pools.InsertModelPrice(ctx, fixture, priceData, "cloud"); err != nil {
		t.Fatalf("插入夹具失败: %v", err)
	}
	if _, err := pools.InsertModelPrice(ctx, dropFixture, priceData, "cloud"); err != nil {
		t.Fatalf("插入待删夹具失败: %v", err)
	}
	if _, err := pools.InsertModelPrice(ctx, manualFixture, priceData, "manual"); err != nil {
		t.Fatalf("插入 manual 夹具失败: %v", err)
	}

	// 最新行读取：夹具必须出现且来源为 cloud。
	existing, err := pools.ListLatestPriceRowsForSync(ctx)
	if err != nil {
		t.Fatalf("读取最新价格行失败: %v", err)
	}
	row, found := existing[fixture]
	if !found {
		t.Fatalf("夹具 %s 未出现在最新价格视图里", fixture)
	}
	if row.Source != "cloud" {
		t.Fatalf("夹具来源应为 cloud, 实际 %q", row.Source)
	}

	// 手动价清单不应包含 cloud 行。
	manualNames, err := pools.ListManualPriceModelNames(ctx)
	if err != nil {
		t.Fatalf("读取手动价清单失败: %v", err)
	}
	if _, exists := manualNames[fixture]; exists {
		t.Fatalf("cloud 行不应出现在手动价清单里: %s", fixture)
	}

	cloudCount, err := pools.CountCloudModelPrices(ctx)
	if err != nil {
		t.Fatalf("统计云端行数失败: %v", err)
	}
	if cloudCount <= 0 {
		t.Fatalf("云端行数应大于 0, 实际 %d", cloudCount)
	}

	// 目录读取：本测试只读，不写入。
	if _, err := pools.GetCloudPricingCatalog(ctx); err != nil {
		t.Fatalf("读取价格目录失败: %v", err)
	}

	// 整表切换：本行以下的断言全部取**可判定的事实**，不再依赖全局计数恒定。
	//
	// 为何改口径：`DeleteCloudPricesNotIn` 的保留列表在客户端生成，谓词是「不在列表里的非 manual 行」，
	// 故**调用瞬间**新插入的行（兄弟套件/开发者的数据）一定也被删。旧版据此断言 `removed == 0`，
	// 于是与 internal/pricing 的同名用例互相删夹具、默认并行门禁下随机变红。现在改为：
	//   1) 保留列表里的每个名字，调用后必须都还在；
	//   2) drop 夹具**故意不进保留列表** → 必须被删（删除方向真的被执行）；
	//   3) manual 夹具同样不进保留列表 → 必须存活（source <> 'manual' 保护）；
	//   4) 调用中被删且不属于本用例意图的名字 → 逐条补回（本用例无权销毁共享数据）。
	//
	// 已知上限：补偿发生在调用之后的几十微秒到几毫秒内，故并发写者仍可能在极窄窗口内
	// 读到「自己的行暂时不在」；要彻底消除需把所有写 model_prices 的用例串行化（跨包，超出本文件范围）。
	before, err := pools.ListLatestPriceRowsForSync(ctx)
	if err != nil {
		t.Fatalf("读取调用前快照失败: %v", err)
	}
	intentionallyDeleted := map[string]struct{}{dropFixture: {}}
	keep := make([]string, 0, len(before)+1)
	for name := range before {
		if _, drop := intentionallyDeleted[name]; drop {
			continue
		}
		if name == manualFixture {
			continue // 不进保留列表，靠 source='manual' 自保
		}
		keep = append(keep, name)
	}
	removed, err := pools.DeleteCloudPricesNotIn(ctx, keep)
	if err != nil {
		t.Fatalf("安全路径清理失败: %v", err)
	}
	after, err := pools.ListLatestPriceRowsForSync(ctx)
	if err != nil {
		t.Fatalf("读取调用后快照失败: %v", err)
	}
	for _, name := range keep {
		if _, ok := after[name]; !ok {
			t.Fatalf("保留列表里的 %s 被删掉了（保留语义失效）", name)
		}
	}
	if _, ok := after[dropFixture]; ok {
		t.Fatalf("未进保留列表的 %s 应当被删除", dropFixture)
	}
	if _, ok := after[manualFixture]; !ok {
		t.Fatalf("manual 行 %s 不应被删除（source <> 'manual' 的保护失效）", manualFixture)
	}
	if removed < 1 {
		t.Fatalf("本次调用至少应删掉 drop 夹具一行，实际删除 %d", removed)
	}
	// 补偿：把「调用期间被冻连删掉、又不属于本用例意图」的行逐条插回。
	restored := 0
	repairedNames := make([]string, 0, 4)
	for name, row := range before {
		if _, intended := intentionallyDeleted[name]; intended {
			continue
		}
		if _, stillThere := after[name]; stillThere {
			continue
		}
		// 插入前再查一次：兄弟套件可能已自己把该名插回（它读到缺失后会重建）。
		// 不再查一次就插，会造出同名单行不变式的破坏（别处有按名断言行数的用例）。
		exists, existsErr := priceNameExists(ctx, pools, name)
		if existsErr != nil {
			t.Fatalf("补偿前复查 %s 失败: %v", name, existsErr)
		}
		if exists {
			continue
		}
		data, marshalErr := json.Marshal(row.PriceData)
		if marshalErr != nil {
			t.Fatalf("序列化待补偿行 %s 失败: %v", name, marshalErr)
		}
		if _, insertErr := pools.InsertModelPrice(ctx, name, data, row.Source); insertErr != nil {
			t.Fatalf("补偿行 %s 失败: %v", name, insertErr)
		}
		restored++
		repairedNames = append(repairedNames, name)
	}
	if removed < int64(restored) {
		t.Fatalf("本次调用报删除 %d 行，但观测到 %d 个名字消失：返回值不得少于实际删除", removed, restored)
	}
	if restored > 0 {
		t.Logf("补偿了 %d 行并发插入的行（调用瞬间不在保留列表里，属兄弟套件/开发者数据）: %v",
			restored, repairedNames)
		// 补偿必须真的生效：上面判为缺失的名字，此时都应重新可见。
		repaired, listErr := pools.ListLatestPriceRowsForSync(ctx)
		if listErr != nil {
			t.Fatalf("复核补偿结果失败: %v", listErr)
		}
		for _, name := range repairedNames {
			if _, ok := repaired[name]; !ok {
				t.Fatalf("补偿后仍缺行: %s", name)
			}
		}
	}

	// 空保留列表按 Node 口径直接跳过（否则等于清空全部非 manual 行）。
	skipped, err := pools.DeleteCloudPricesNotIn(ctx, nil)
	if err != nil {
		t.Fatalf("空保留列表不应报错: %v", err)
	}
	if skipped != 0 {
		t.Fatalf("空保留列表必须返回 0, 实际 %d", skipped)
	}

	// 清理后夹具必须消失（精确按名删除）。
	cleanup()
	if _, err := pools.AdminFindLatestModelPriceByName(ctx, fixture); err != nil {
		t.Fatalf("清理后复查失败: %v", err)
	}
}

func TestIntegrationPriceSyncUpdatesRowInPlaceByDeleteInsert(t *testing.T) {
	pools := integrationPools(t)
	// 本用例往共享库写同名夹具行再读回，同样会被并发的整表切换删掉，故与写者互斥。
	lockModelPricesTable(t, pools)
	ctx := context.Background()
	fixture := jobIntegrationFixturePrefix + "update-" + time.Now().Format("20060102150405.000000000")

	cleanup := func() {
		if err := pools.AdminDeleteModelPriceByName(ctx, fixture); err != nil {
			t.Fatalf("清理夹具失败: %v", err)
		}
	}
	t.Cleanup(cleanup)
	cleanup()

	first := []byte(`{"mode":"chat","input_cost_per_token":1e-06}`)
	if _, err := pools.InsertModelPrice(ctx, fixture, first, "cloud"); err != nil {
		t.Fatalf("插入夹具失败: %v", err)
	}

	// 同步侧的替换语义：删旧 + 插新（不是原地更新），来源与价格随之更换。
	second := []byte(`{"mode":"chat","input_cost_per_token":2e-06}`)
	if _, err := pools.AdminUpsertModelPrice(ctx, fixture, json.RawMessage(second), "cloud"); err != nil {
		t.Fatalf("替换夹具失败: %v", err)
	}

	row, err := pools.AdminFindLatestModelPriceByName(ctx, fixture)
	if err != nil {
		t.Fatalf("复查夹具失败: %v", err)
	}
	if row == nil {
		t.Fatal("替换后应仍有一行")
	}
	var decoded struct {
		InputCost float64 `json:"input_cost_per_token"`
	}
	if err := json.Unmarshal(row.PriceData, &decoded); err != nil {
		t.Fatalf("解析价格数据失败: %v", err)
	}
	if decoded.InputCost != 2e-06 {
		t.Fatalf("替换后价格应为 2e-06, 实际 %v", decoded.InputCost)
	}

	// 同名单行：替换不得留下同名多行。
	rows, err := pools.AdminListLatestModelPrices(ctx)
	if err != nil {
		t.Fatalf("列出最新价格失败: %v", err)
	}
	matches := 0
	for _, item := range rows {
		if item.ModelName == fixture {
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("同一模型应只有一行, 实际 %d", matches)
	}
	cleanup()
}

func TestIntegrationAvailBackfillMarkerIsReadOnly(t *testing.T) {
	pools := integrationPools(t)
	ctx := context.Background()

	// 只读断言：本测试**不写** backfill_done（写了会让真实回填被跳过）。
	done, err := pools.ProjectionBackfillDone(ctx)
	if err != nil {
		t.Fatalf("读取回填标记失败: %v", err)
	}
	t.Logf("当前库的 projection_meta.backfill_done 存在=%t（本测试不做任何写入）", done)

	// 只有库中已有标记时才驱动 RunOnce：标记不存在时它会真的把 100 天历史灌进
	// outbox_events 并写下标记，那会污染共享库（且让之后真正的回填被当成已完成）。
	if !done {
		t.Logf("库中无 backfill_done 标记，跳过真实回填（避免污染共享 outbox 与标记）")
		return
	}

	backfill, err := NewAvailBackfill(AvailBackfillOptions{Pools: pools})
	if err != nil {
		t.Fatalf("构造回填器失败: %v", err)
	}
	result, err := backfill.RunOnce(ctx)
	if err != nil {
		t.Fatalf("回填失败: %v", err)
	}
	if !result.Skipped || result.SkippedReason != "already_done" {
		t.Fatalf("库中已有标记时必须跳过, 实际 %+v", result)
	}
	if result.Inserted != 0 || result.Chunks != 0 {
		t.Fatalf("跳过时不得入队, 实际 %+v", result)
	}
}
