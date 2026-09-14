package store

import (
	"context"
	"testing"
)

// 本文件是**供应商写入**的真库用例（`CCH_TEST_DSN` 未设置时跳过）。
//
// 为什么必须有真库用例：写入字段表（adminProviderWriteFields）与 SQL 的列名是两处配置，
// 字段能通过校验、能进 payload，却仍可能因为列名或可空性与表定义不符而**静默丢字段**
// （历史事故：写入表列了字段但 SQL 没绑定）。只有真库能证明「写进去、读回来一致」。
//
// 夹具纪律同其它 store 用例：唯一 marker、按 marker 清理、只增删自己的行。

// TestAdminCreateProviderPersistsLadderColumns 造一个带等待阶梯的供应商，直接读库核对两列。
func TestAdminCreateProviderPersistsLadderColumns(t *testing.T) {
	pools := openTestPools(t)
	ctx := context.Background()
	marker := itKey(t)
	name := marker + "-ladder-create"

	id, err := pools.AdminCreateProvider(ctx, map[string]any{
		"name":          name,
		"url":           "https://ladder-create.invalid",
		"key":           "sk-ladder-create",
		"provider_type": "claude",
		// 阶梯：递增 10 分钟（毫秒）、封顶 3 级。单位与 circuit_breaker_open_duration 一致。
		"circuit_breaker_release_increment": ladderPtrInt64(600000),
		"circuit_breaker_max_open_count":    ladderPtrInt64(3),
	}, marker+".invalid")
	if err != nil {
		t.Fatalf("创建供应商失败: %v", err)
	}
	t.Cleanup(func() { cleanupLadderProvider(pools, id) })

	increment, maxOpen := readLadderColumns(t, pools, id)
	if increment == nil || *increment != 600000 {
		t.Fatalf("递增时长未落库或值不符：%v（期望 600000）", increment)
	}
	if maxOpen == nil || *maxOpen != 3 {
		t.Fatalf("递增最大次数未落库或值不符：%v（期望 3）", maxOpen)
	}
}

// TestAdminPatchProviderClearsLadderColumns 钉住「clear = null」这一语义：
// 阶梯不启用是**清空两列**，而不是写 0（0 会被读侧当成一个真实阶数上限）。
func TestAdminPatchProviderClearsLadderColumns(t *testing.T) {
	pools := openTestPools(t)
	ctx := context.Background()
	marker := itKey(t)
	name := marker + "-ladder-patch"

	id, err := pools.AdminCreateProvider(ctx, map[string]any{
		"name":                              name,
		"url":                               "https://ladder-patch.invalid",
		"key":                               "sk-ladder-patch",
		"provider_type":                     "claude",
		"circuit_breaker_release_increment": ladderPtrInt64(900000),
		"circuit_breaker_max_open_count":    ladderPtrInt64(5),
	}, marker+".invalid")
	if err != nil {
		t.Fatalf("创建供应商失败: %v", err)
	}
	t.Cleanup(func() { cleanupLadderProvider(pools, id) })

	ok, err := pools.AdminPatchProvider(ctx, id, map[string]any{
		"circuit_breaker_release_increment": nil,
		"circuit_breaker_max_open_count":    nil,
	})
	if err != nil {
		t.Fatalf("清空阶梯失败: %v", err)
	}
	if !ok {
		t.Fatal("清空阶梯返回 false（目标应存在）")
	}

	increment, maxOpen := readLadderColumns(t, pools, id)
	if increment != nil || maxOpen != nil {
		t.Fatalf("清空后两列应为 NULL，实际 increment=%v maxOpen=%v", increment, maxOpen)
	}

	// 反向钉子：只 patch 一列时，另一列**不动**（Node 的 `!== undefined` 语义）。
	ok, err = pools.AdminPatchProvider(ctx, id, map[string]any{
		"circuit_breaker_release_increment": ladderPtrInt64(1200000),
	})
	if err != nil || !ok {
		t.Fatalf("只 patch 递增时长失败: ok=%v err=%v", ok, err)
	}
	increment, maxOpen = readLadderColumns(t, pools, id)
	if increment == nil || *increment != 1200000 {
		t.Fatalf("递增时长应为 1200000，实际 %v", increment)
	}
	if maxOpen != nil {
		t.Fatalf("未触碰的递增最大次数不该被改写，实际 %v", maxOpen)
	}
}

// ladderPtrInt64 造一个 *int64（nullableInt 列的值类型）。
func ladderPtrInt64(value int64) *int64 { return &value }

// readLadderColumns 直读两列（绕开投影，断的就是「真的写进库了吗」）。
func readLadderColumns(t *testing.T, pools *Pools, id int64) (*int, *int) {
	t.Helper()
	control, err := pools.Control()
	if err != nil {
		t.Fatalf("取控制连接失败: %v", err)
	}
	var increment, maxOpen *int
	if err := control.QueryRow(context.Background(),
		"SELECT circuit_breaker_release_increment, circuit_breaker_max_open_count FROM providers WHERE id = $1",
		id,
	).Scan(&increment, &maxOpen); err != nil {
		t.Fatalf("读回阶梯两列失败: %v", err)
	}
	return increment, maxOpen
}

func cleanupLadderProvider(pools *Pools, id int64) {
	writer, err := pools.Writer()
	if err != nil {
		return
	}
	_, _ = writer.Exec(context.Background(), "DELETE FROM providers WHERE id = $1", id)
}
