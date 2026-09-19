package store

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// 本文件量化批量写与逐条写的往返代价差异，跑法：
//
//	CCH_TEST_DSN=... CCH_TEST_PRICE_WRITE_BENCH=1 go test -run TestModelPriceWriteRoundTripComparison -v ./internal/store/
//
// 为什么不用 Benchmark：每次迭代都要写库并清理，b.N 会把它变成分钟级；用显式开关的普通用例
// 更可控，也不会给默认门禁添负担。
//
// 往返次数是**代码路径**决定的常量，不是估的：
//   - 逐条新增：每个模型 1 次 INSERT → N 次（改前新增路径）
//   - 逐条替换：每个模型 BEGIN/DELETE/INSERT/COMMIT → 4N 次（改前替换路径）
//   - 批量新增：1 次 INSERT ... unnest → 1 次
//   - 批量替换：BEGIN/DELETE/INSERT/COMMIT → 4 次
//
// 这里测的是同一张表、同一份数据下的**墙钟时间**，用来给上面的常量一个可观测的印证。
func TestModelPriceWriteRoundTripComparison(t *testing.T) {
	if os.Getenv("CCH_TEST_PRICE_WRITE_BENCH") == "" {
		t.Skip("未设置 CCH_TEST_PRICE_WRITE_BENCH，跳过往返对比（默认门禁不写库基准数据）")
	}
	pools := openTestPools(t)
	lockModelPricesTable(t, pools)
	ctx := context.Background()
	prefix := priceBatchPrefix(t)
	cleanupPriceBatchRows(t, pools, prefix)

	const modelCount = 200
	payload := func(index int) []byte {
		return []byte(fmt.Sprintf(`{"mode":"chat","input_cost_per_token":%d.5}`, index))
	}

	rowPrefix := prefix + "-row-"
	rowNames := make([]string, modelCount)
	for index := range rowNames {
		rowNames[index] = fmt.Sprintf("%s%04d", rowPrefix, index)
	}
	started := time.Now()
	for index, name := range rowNames {
		if _, err := pools.InsertModelPrice(ctx, name, payload(index), "cloud"); err != nil {
			t.Fatalf("逐条新增失败: %v", err)
		}
	}
	rowInsertElapsed := time.Since(started)

	started = time.Now()
	for index, name := range rowNames {
		if _, err := pools.AdminUpsertModelPrice(ctx, name, payload(index), "cloud"); err != nil {
			t.Fatalf("逐条替换失败: %v", err)
		}
	}
	rowReplaceElapsed := time.Since(started)

	batchPrefix := prefix + "-batch-"
	batchWrites := make([]ModelPriceWrite, modelCount)
	for index := range batchWrites {
		batchWrites[index] = ModelPriceWrite{
			ModelName: fmt.Sprintf("%s%04d", batchPrefix, index),
			PriceData: payload(index),
			Source:    "cloud",
		}
	}
	started = time.Now()
	if err := pools.InsertModelPrices(ctx, batchWrites); err != nil {
		t.Fatalf("批量新增失败: %v", err)
	}
	batchInsertElapsed := time.Since(started)

	started = time.Now()
	if err := pools.ReplaceModelPrices(ctx, batchWrites); err != nil {
		t.Fatalf("批量替换失败: %v", err)
	}
	batchReplaceElapsed := time.Since(started)

	t.Logf("N=%d 逐条新增 %d 次往返: %s", modelCount, modelCount, rowInsertElapsed)
	t.Logf("N=%d 批量新增 1 次往返: %s", modelCount, batchInsertElapsed)
	t.Logf("N=%d 逐条替换 %d 次往返: %s", modelCount, modelCount*4, rowReplaceElapsed)
	t.Logf("N=%d 批量替换 4 次往返: %s", modelCount, batchReplaceElapsed)
	t.Logf("替换路径加速比: %.1fx（新增 %.1fx）",
		float64(rowReplaceElapsed)/float64(batchReplaceElapsed),
		float64(rowInsertElapsed)/float64(batchInsertElapsed))

	if batchReplaceElapsed >= rowReplaceElapsed {
		t.Fatalf("批量替换未快于逐条替换：批量=%s 逐条=%s", batchReplaceElapsed, rowReplaceElapsed)
	}
}
