package pricing

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/jobs"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 真库集成门控：未设置 CCH_TEST_DSN 时整组跳过。
//
// **共享库纪律**：本文件只写「带测试前缀的模型行」，并在 cleanup 里按前缀删除；
// cloud_pricing_catalog 是单行表，写入前先快照、cleanup 时还原——否则会把别的用例
// 或本地开发留下的目录数据冲掉。
const integrationDSNEnv = "CCH_TEST_DSN"

// syncDSNEnv 指向**可弃库**：只为「会做整表切换」的用例准备（见 openSyncTestPools）。
const syncDSNEnv = "CCH_TEST_SYNC_DSN"

func openTestPools(t *testing.T) *store.Pools {
	t.Helper()
	dsn := os.Getenv(integrationDSNEnv)
	if dsn == "" {
		t.Skip("未设置 CCH_TEST_DSN，跳过 model-prices 集成测试")
	}
	pools, err := store.Open(context.Background(), store.Options{
		DSN:      dsn,
		Budget:   config.SplitPoolBudget(6),
		Timeouts: config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("建立连接池失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	return pools
}

func testPrefix(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("it-pricing-%d", time.Now().UnixNano())
}

// cleanupModelPrices 按前缀删除本测试写入的所有行（manual 与 cloud 都要删）。
func cleanupModelPrices(t *testing.T, pools *store.Pools, prefix string) {
	t.Helper()
	t.Cleanup(func() {
		pool, err := pools.Writer()
		if err != nil {
			t.Logf("cleanup 取写连接失败: %v", err)
			return
		}
		if _, err := pool.Exec(context.Background(),
			`DELETE FROM model_prices WHERE model_name LIKE $1`, prefix+"%"); err != nil {
			t.Logf("cleanup 删除价格行失败: %v", err)
		}
	})
}

// snapshotCatalog 快照 cloud_pricing_catalog（单行表），并在 cleanup 时还原。
func snapshotCatalog(t *testing.T, pools *store.Pools) {
	t.Helper()
	ctx := context.Background()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取读连接失败: %v", err)
	}
	var snapshot []map[string]any
	rows, err := pool.Query(ctx, `SELECT row_to_json(t)::text FROM (SELECT * FROM cloud_pricing_catalog) t`)
	if err == nil {
		for rows.Next() {
			var encoded string
			if scanErr := rows.Scan(&encoded); scanErr == nil {
				var decoded map[string]any
				if jsonErr := json.Unmarshal([]byte(encoded), &decoded); jsonErr == nil {
					snapshot = append(snapshot, decoded)
				}
			}
		}
		rows.Close()
	}

	t.Cleanup(func() {
		writer, writeErr := pools.Writer()
		if writeErr != nil {
			t.Logf("cleanup 取写连接失败: %v", writeErr)
			return
		}
		if _, delErr := writer.Exec(ctx, `DELETE FROM cloud_pricing_catalog`); delErr != nil {
			t.Logf("cleanup 清空目录失败: %v", delErr)
			return
		}
		for _, row := range snapshot {
			encoded, _ := json.Marshal(row)
			if _, insertErr := writer.Exec(ctx,
				`INSERT INTO cloud_pricing_catalog
				 SELECT * FROM jsonb_populate_record(NULL::cloud_pricing_catalog, $1::jsonb)`,
				string(encoded)); insertErr != nil {
				t.Logf("cleanup 还原目录失败: %v", insertErr)
			}
		}
	})
}

// TestWriteEntriesAgainstRealDatabase 真库验证写入分类：新增 → 不变 → 变更 → manual 保护 → 清理。
func TestWriteEntriesAgainstRealDatabase(t *testing.T) {
	pools := openTestPools(t)
	if pools == nil {
		return
	}
	// 跨包互斥：本用例「建行 → 改价 → 断言 updated」的序列会被并发写者的整表切换打断
	// （兄弟包的 DeleteCloudPricesNotIn 会删掉调用后新插入的行，于是改价被判成新增）。
	lockModelPricesTable(t, pools)
	ctx := context.Background()
	prefix := testPrefix(t)
	cleanupModelPrices(t, pools, prefix)

	modelA := prefix + "-a"
	modelB := prefix + "-b"
	entries := []Entry{
		entry(modelA, fmt.Sprintf(`{"mode":"chat","input_cost_per_token":0.000001,"display_name":%q}`, modelA)),
		entry(modelB, fmt.Sprintf(`{"mode":"chat","input_cost_per_token":0.000002,"display_name":%q}`, modelB)),
	}

	// 1) 首次写入 → 两条 added。
	first, err := WriteEntries(ctx, pools, entries, SourceCloud, nil, testLogger())
	if err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	if len(first.Added) != 2 || len(first.Updated) != 0 || len(first.Failed) != 0 {
		t.Fatalf("首次写入分类不符：%+v", first)
	}

	// 2) 同内容再写 → unchanged（键序不同也要判为相同，等值比较走规范化）。
	reordered := []Entry{
		entry(modelA, fmt.Sprintf(`{"display_name":%q,"input_cost_per_token":0.000001,"mode":"chat"}`, modelA)),
		entry(modelB, fmt.Sprintf(`{"mode":"chat","display_name":%q,"input_cost_per_token":0.000002}`, modelB)),
	}
	second, err := WriteEntries(ctx, pools, reordered, SourceCloud, nil, testLogger())
	if err != nil {
		t.Fatalf("二次写入失败: %v", err)
	}
	if len(second.Unchanged) != 2 || len(second.Added) != 0 || len(second.Updated) != 0 {
		t.Fatalf("二次写入应全部 unchanged（键序不同不算变更）：%+v", second)
	}

	// 3) 改价 → updated。
	changed := []Entry{entry(modelA, fmt.Sprintf(`{"mode":"chat","input_cost_per_token":0.000009,"display_name":%q}`, modelA))}
	third, err := WriteEntries(ctx, pools, changed, SourceCloud, nil, testLogger())
	if err != nil {
		t.Fatalf("改价写入失败: %v", err)
	}
	if len(third.Updated) != 1 || third.Updated[0] != modelA {
		t.Fatalf("改价应判 updated：%+v", third)
	}

	// 4) manual 行 + 云端写入（未列入 overwrite）→ skippedConflicts，且库里仍是 manual 价。
	manual := []Entry{entry(modelB, fmt.Sprintf(`{"mode":"chat","input_cost_per_token":0.0005,"display_name":%q}`, modelB))}
	if _, err := WriteEntries(ctx, pools, manual, SourceManual, nil, testLogger()); err != nil {
		t.Fatalf("manual 写入失败: %v", err)
	}
	protected := []Entry{entry(modelB, fmt.Sprintf(`{"mode":"chat","input_cost_per_token":0.000002,"display_name":%q}`, modelB))}
	fourth, err := WriteEntries(ctx, pools, protected, SourceCloud, nil, testLogger())
	if err != nil {
		t.Fatalf("受保护写入失败: %v", err)
	}
	if len(fourth.SkippedConflicts) != 1 || fourth.SkippedConflicts[0] != modelB {
		t.Fatalf("manual 模型应被保护跳过：%+v", fourth)
	}
	row, err := pools.AdminFindLatestModelPriceByName(ctx, modelB)
	if err != nil {
		t.Fatalf("读回 manual 行失败: %v", err)
	}
	if row.Source != SourceManual || !strings.Contains(string(row.PriceData), "0.0005") {
		t.Fatalf("manual 价被覆盖了：source=%s price=%s", row.Source, row.PriceData)
	}

	// 5) 列入 overwrite 后 → updated（manual → cloud）。
	fifth, err := WriteEntries(ctx, pools, protected, SourceCloud, []string{modelB}, testLogger())
	if err != nil {
		t.Fatalf("覆盖写入失败: %v", err)
	}
	if len(fifth.Updated) != 1 || fifth.Updated[0] != modelB {
		t.Fatalf("列入覆盖后应 updated：%+v", fifth)
	}
	row, _ = pools.AdminFindLatestModelPriceByName(ctx, modelB)
	if row.Source != SourceCloud {
		t.Fatalf("覆盖后来源应为 cloud，实际 %s", row.Source)
	}

	// 6) 整表切换：**只跑安全路径**——保留列表 ⊇ 调用前一刻的全部现存行，断言「保留的都不被删」。
	//
	// 为何不在这里传窄保留列表：`DELETE FROM model_prices WHERE source <> 'manual' AND NOT
	// (model_name = ANY($1))` 会把共享测试库里**其它套件与开发留下的数千条云端行**一并删掉
	// （本 lane 初版就是这样把 internal/jobs 的集成用例弄红的）。
	//
	// 可判定性口径（本用例曾与 internal/jobs 的同名用例互相删夹具、默认并行门禁下随机变红，故改口径）：
	// 保留列表在客户端生成，谓词是「不在列表里的非 manual 行」，故**调用瞬间**新插入的行
	//（兄弟套件/开发者的数据）一定也被删。旧版据此断言 `removed == 0` 与「非 manual 行数不变」，
	// 两者都要求全局计数在调用前后恒定，而并发写者会随机破坏它。现在改为逐条断言可判定的事实：
	//   1) 保留列表里的每个名字，调用后必须都还在（安全路径的可判定语义）；
	//   2) 调用期间消失的名字（并发插入的行）→ 逐条补回，本用例无权销毁共享数据。
	//
	// 已知上限：补偿发生在调用之后的几十微秒到几毫秒内，故并发写者仍可能在极窄窗口内
	// 读到「自己的行暂时不在」；要彻底消除需把所有写 model_prices 的用例串行化（跨包，超出本文件范围）。
	snapshot, err := pools.ListLatestPriceRowsForSync(ctx)
	if err != nil {
		t.Fatalf("读取调用前快照失败: %v", err)
	}
	existingNames, err := allNonManualModelNames(ctx, pools)
	if err != nil {
		t.Fatalf("读取现有非 manual 行失败: %v", err)
	}
	// 保留列表 = 现存全部非 manual + 本测试的 A、B（B 也保留，使断言只验证「保留下来的都不被删」）。
	keep := append(append([]string{}, existingNames...), modelA, modelB)
	removed, err := pools.DeleteCloudPricesNotIn(ctx, keep)
	if err != nil {
		t.Fatalf("安全路径清理失败: %v", err)
	}
	afterSnapshot, err := pools.ListLatestPriceRowsForSync(ctx)
	if err != nil {
		t.Fatalf("读取调用后快照失败: %v", err)
	}
	for _, name := range keep {
		if _, ok := afterSnapshot[name]; !ok {
			t.Fatalf("保留列表里的 %s 被删掉了（安全路径语义失效）", name)
		}
	}
	// 调用期间被冻连删掉的行逐条补回（只可能是并发插入、且不在保留列表里的行）。
	restored := 0
	repairedNames := make([]string, 0, 4)
	for name, row := range snapshot {
		if _, stillThere := afterSnapshot[name]; stillThere {
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
	}
}

// priceNameExists 判断某个模型名此刻是否还有行。
//
// 用途：补偿（把调用期间被冻连删掉的行插回）前的最后一道检查——兄弟套件若已自己重建该行，
// 就不该再插一份，否则同名单行不变式会被补偿动作自己破坏。jobs 包的同名集成用例里有同一份。
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

// allNonManualModelNames 列出库里全部非 manual 模型名（安全路径的保留列表用它）。
func allNonManualModelNames(ctx context.Context, pools *store.Pools) ([]string, error) {
	pool, err := pools.Control()
	if err != nil {
		return nil, err
	}
	rows, err := pool.Query(ctx, `SELECT DISTINCT model_name FROM model_prices WHERE source <> 'manual'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := make([]string, 0, 128)
	for rows.Next() {
		var name string
		if scanErr := rows.Scan(&name); scanErr != nil {
			return nil, scanErr
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// TestSyncNowAgainstRealDatabase 端到端：假上游 CPT → SyncNow 写入 + 清理 + 目录落库。
//
// **为何需要显式指定的可弃库**：SyncNow 内含「整表切换」——它会删掉本次表内不存在的
// 非 manual 行。共享测试库（cch_loadtest / cch_smoke）都有数千条云端价格行，在上面跑这条
// 等价于把它们清空（本 lane 初版踩过，见 §报告「共享库纪律」）。故本用例要求
// `CCH_TEST_SYNC_DSN` 指向**可弃库**（如 `CREATE DATABASE x TEMPLATE cch_loadtest`），
// 未设置即跳过：默认套件永不动共享数据。
func TestSyncNowAgainstRealDatabase(t *testing.T) {
	pools := openSyncTestPools(t)
	if pools == nil {
		return
	}
	prefix := testPrefix(t)
	cleanupModelPrices(t, pools, prefix)
	snapshotCatalog(t, pools)

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(cloudTableFixture(prefix)))
	}))
	defer server.Close()

	syncer, err := jobs.NewPriceSyncer(jobs.PriceSyncOptions{
		Pools:  pools,
		Logger: testLogger(),
		URL:    server.URL,
	})
	if err != nil {
		t.Fatalf("建同步器失败: %v", err)
	}

	first, err := syncer.SyncNow(context.Background())
	if err != nil {
		t.Fatalf("SyncNow 失败: %v", err)
	}
	if len(first.Added) != 2 {
		t.Fatalf("首次同步应新增 2 行：%+v", first)
	}

	catalog, err := pools.GetCloudPricingCatalog(context.Background())
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	if catalog == nil {
		t.Fatal("目录未落库")
	}
	if !strings.Contains(catalog.Version, "+cvt1") {
		t.Errorf("版本指纹应带 +cvt1 后缀，实际 %q", catalog.Version)
	}
	// modelCount 记的是**写库后的实际非 manual 行数**（Node 同口径）。
	if catalog.ModelCount == 0 {
		t.Error("目录的 modelCount 不应为 0")
	}

	second, err := syncer.SyncNow(context.Background())
	if err != nil {
		t.Fatalf("二次 SyncNow 失败: %v", err)
	}
	if len(second.Unchanged) != 2 {
		t.Fatalf("二次同步应全部 unchanged（端点不做版本短路，仍重放整表）：%+v", second)
	}
}

// cloudTableFixture 造一张最小 CPT 表（两个模型，一个含分层与 priority 轨道）。
func cloudTableFixture(prefix string) string {
	return fmt.Sprintf(`{
	  "schema": "cchp.pricing-table/v1",
	  "version": %q,
	  "currency": "USD",
	  "refreshed_at": "2026-09-13T00:00:00Z",
	  "providers": {"acme": {"name": "Acme"}},
	  "models": [
	    {
	      "slug": "%s-a", "model_name": "%s-a", "vendor": "acme", "display_name": "A",
	      "model_type": "chat",
	      "pricing": [{
	        "provider": "acme", "official": true, "source": "vendor",
	        "charges": {
	          "prompt": {"price": "1.0", "unit": "per_M_tokens"},
	          "completion": {"price": "2.0", "unit": "per_M_tokens"}
	        },
	        "tracks": [
	          {"label": "default", "factor": "1"},
	          {"label": "above-200k", "factor": "2", "triggers": [{"kind": "input_tokens_above", "threshold": 200000}]}
	        ]
	      }]
	    },
	    {
	      "slug": "%s-b", "model_name": "%s-b", "vendor": "acme", "display_name": "B",
	      "model_type": "chat",
	      "pricing": [{
	        "provider": "acme", "official": true, "source": "vendor",
	        "charges": {"prompt": {"price": "3.0", "unit": "per_M_tokens"}}
	      }]
	    }
	  ]
	}`, "2026.09.13-it-"+prefix, prefix, prefix, prefix, prefix)
}

// openSyncTestPools 打开**可弃库**（CCH_TEST_SYNC_DSN）；未设置即跳过。
//
// 与 openTestPools 的分工：后者连共享测试库，只跑不会删数据的用例；前者给「整表切换」这类
// 会清掉非 manual 行的用例用。推荐准备方式：
//
//	psql -U postgres -c 'CREATE DATABASE cch_sync_it TEMPLATE cch_loadtest'
//	CCH_TEST_SYNC_DSN=postgres://postgres:postgres@127.0.0.1:5432/cch_sync_it go test ...
//
// 用完 DROP 掉即可（模板复制会带上 schema 与部分数据，写入语义与真库一致）。
func openSyncTestPools(t *testing.T) *store.Pools {
	t.Helper()
	dsn := os.Getenv(syncDSNEnv)
	if dsn == "" {
		t.Skip("未设置 CCH_TEST_SYNC_DSN（可弃库），跳过会做整表切换的同步用例")
	}
	pools, err := store.Open(context.Background(), store.Options{
		DSN:      dsn,
		Budget:   config.SplitPoolBudget(6),
		Timeouts: config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("建立可弃库连接池失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	return pools
}
