package store

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// 本文件是「模型价格批量写」的真库用例，证明批量原语与逐条写**终态等价**：
//
//	InsertModelPrices      ≡ N 次 InsertModelPrice
//	ReplaceModelPrices     ≡ N 次 AdminUpsertModelPrice（先删该模型全部旧行、再插一行）
//
// 为什么必须真库：等价的要害是 SQL 里 unnest 的三条并行数组下标对齐、以及
// 「先删后插」对**旧行（含 id 与历史）**的处理——这两点在假实现上证明不了。
//
// 共享库纪律（同 internal/{pricing,jobs,adminapi} 的同名用例）：只写带本测试前缀的行，
// cleanup 按前缀删；并握 model_prices 的跨包咨询锁，否则并行跑包时会被兄弟包的
// 整表切换（DeleteCloudPricesNotIn）删掉夹具。

const modelPricesTestLockName = "claude-code-hub:model-prices-tests"

func lockModelPricesTable(t *testing.T, pools *Pools) {
	t.Helper()
	ctx := context.Background()

	conn, lockPool, err := pools.OpenDedicatedConn(ctx)
	if err != nil {
		t.Fatalf("建立互斥用独立连接失败: %v", err)
	}
	t.Cleanup(func() {
		conn.Release()
		lockPool.Close()
	})

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("开启取锁事务失败: %v", err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '120s'`); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("设置取锁等待上限失败: %v", err)
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_lock(hashtext($1))`, modelPricesTestLockName); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("取 model_prices 互斥锁失败（等待超上限，可能有别的用例持锁未释放）: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("提交取锁事务失败: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext($1))`, modelPricesTestLockName)
	})
}

func priceBatchPrefix(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("it-store-batch-%d", time.Now().UnixNano())
}

func cleanupPriceBatchRows(t *testing.T, pools *Pools, prefix string) {
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

// readPriceBatchRows 读出某个前缀下每模型一行（本用例里每个模型只应有一行）。
func readPriceBatchRows(t *testing.T, pools *Pools, prefix string) map[string]string {
	t.Helper()
	ctx := context.Background()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取读连接失败: %v", err)
	}
	rows, err := pool.Query(ctx,
		`SELECT model_name, source, price_data::text FROM model_prices WHERE model_name LIKE $1`, prefix+"%")
	if err != nil {
		t.Fatalf("查询价格行失败: %v", err)
	}
	defer rows.Close()

	result := make(map[string]string, 8)
	for rows.Next() {
		var name, source, data string
		if err := rows.Scan(&name, &source, &data); err != nil {
			t.Fatalf("扫描价格行失败: %v", err)
		}
		if _, exists := result[name]; exists {
			t.Fatalf("模型 %s 出现了多行：先删后插语义被破坏", name)
		}
		result[name] = source + "|" + data
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历价格行失败: %v", err)
	}
	return result
}

// TestIntegrationModelPriceBatchWritesMatchRowByRow 是本次改动的核心等价证明：
// 同一份输入（两个已存在的模型走替换、一个新模型走插入），一组用批量原语、一组用逐条原语，
// 终态（每模型一行的 source 与 price_data）必须逐字相同。
func TestIntegrationModelPriceBatchWritesMatchRowByRow(t *testing.T) {
	pools := openTestPools(t)
	lockModelPricesTable(t, pools)
	ctx := context.Background()
	prefix := priceBatchPrefix(t)
	cleanupPriceBatchRows(t, pools, prefix)

	batchPrefix := prefix + "-batch-"
	rowPrefix := prefix + "-row-"

	price := func(cost string) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"mode":"chat","input_cost_per_token":%s}`, cost))
	}

	// 两组同形种数据：alpha/beta 已存在（各有旧值与一行 stale 旧行），gamma 不存在。
	for _, group := range []string{batchPrefix, rowPrefix} {
		if _, err := pools.InsertModelPrice(ctx, group+"alpha", price("0.5"), "cloud"); err != nil {
			t.Fatalf("种 alpha 失败: %v", err)
		}
		// alpha 多插一行：替换必须把该模型的**全部**旧行删净（不是只删最新一行）。
		if _, err := pools.InsertModelPrice(ctx, group+"alpha", price("0.75"), "cloud"); err != nil {
			t.Fatalf("种 alpha 旧行失败: %v", err)
		}
		if _, err := pools.InsertModelPrice(ctx, group+"beta", price("1.5"), "litellm"); err != nil {
			t.Fatalf("种 beta 失败: %v", err)
		}
	}

	// 批量组：插入 gamma + 批量替换 alpha/beta。
	if err := pools.InsertModelPrices(ctx, []ModelPriceWrite{
		{ModelName: batchPrefix + "gamma", PriceData: price("2"), Source: "cloud"},
	}); err != nil {
		t.Fatalf("批量插入失败: %v", err)
	}
	if err := pools.ReplaceModelPrices(ctx, []ModelPriceWrite{
		{ModelName: batchPrefix + "alpha", PriceData: price("0.9"), Source: "cloud"},
		{ModelName: batchPrefix + "beta", PriceData: price("1.1"), Source: "cloud"},
	}); err != nil {
		t.Fatalf("批量替换失败: %v", err)
	}

	// 逐条组：同形操作。
	if _, err := pools.InsertModelPrice(ctx, rowPrefix+"gamma", price("2"), "cloud"); err != nil {
		t.Fatalf("逐条插入失败: %v", err)
	}
	if _, err := pools.AdminUpsertModelPrice(ctx, rowPrefix+"alpha", price("0.9"), "cloud"); err != nil {
		t.Fatalf("逐条替换 alpha 失败: %v", err)
	}
	if _, err := pools.AdminUpsertModelPrice(ctx, rowPrefix+"beta", price("1.1"), "cloud"); err != nil {
		t.Fatalf("逐条替换 beta 失败: %v", err)
	}

	batchRows := readPriceBatchRows(t, pools, batchPrefix)
	rowRows := readPriceBatchRows(t, pools, rowPrefix)

	if len(batchRows) != 3 {
		t.Fatalf("批量组应有 3 行（alpha/beta/gamma 各一行），实际 %d 行：%+v", len(batchRows), batchRows)
	}
	for _, name := range []string{"alpha", "beta", "gamma"} {
		batchValue, ok := batchRows[batchPrefix+name]
		if !ok {
			t.Fatalf("批量组缺 %s 行：%+v", name, batchRows)
		}
		rowValue, ok := rowRows[rowPrefix+name]
		if !ok {
			t.Fatalf("逐条组缺 %s 行：%+v", name, rowRows)
		}
		if batchValue != rowValue {
			t.Fatalf("%s 终态不一致：批量=%q 逐条=%q", name, batchValue, rowValue)
		}
	}

	// 空入参不落任何写（调用方在无待写项时也会走到这里）。
	if err := pools.InsertModelPrices(ctx, nil); err != nil {
		t.Fatalf("空批量插入应成功: %v", err)
	}
	if err := pools.ReplaceModelPrices(ctx, nil); err != nil {
		t.Fatalf("空批量替换应成功: %v", err)
	}
}
