package store

import (
	"context"
	"testing"
)

// 事务包装必须把准入计数在 Commit 与 Rollback 两个出口都释放，否则在途计数会永久漂移。
func TestIntegrationAdmittedTransactionReleasesOnCommitAndRollback(t *testing.T) {
	pools := openTestPools(t)
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	ctx := context.Background()

	before := pool.Outstanding()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}
	if got := pool.Outstanding(); got != before+1 {
		t.Fatalf("事务在途数 = %d, want %d", got, before+1)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("提交事务失败: %v", err)
	}
	if got := pool.Outstanding(); got != before {
		t.Fatalf("提交后在途数 = %d, want %d", got, before)
	}

	second, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("第二次开启事务失败: %v", err)
	}
	if err := second.Rollback(ctx); err != nil {
		t.Fatalf("回滚事务失败: %v", err)
	}
	if got := pool.Outstanding(); got != before {
		t.Fatalf("回滚后在途数 = %d, want %d", got, before)
	}
}

// 在事务内执行投影去重写入：与 projection-worker 在 tx 内调用 INSERT 的用法一致。
func TestIntegrationProjectionInsideTransaction(t *testing.T) {
	pools := openTestPools(t)
	key := itKey(t)
	t.Cleanup(func() { cleanupRequestRows(t, pools, []string{key}) })

	ctx := context.Background()
	created := createRequestFixture(t, pools, key, false)

	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("开启事务失败: %v", err)
	}

	eventID := "3f2504e0-4f89-11d3-9a0c-0305e82c3302"
	// 事务内的语句直接走 pgx.Tx：准入计数已在 Begin 时计入一次。
	var returned int64
	if err := tx.QueryRow(ctx,
		`INSERT INTO proj_applied_requests (request_id, event_id) VALUES ($1, $2::uuid)
		 ON CONFLICT (request_id) DO NOTHING RETURNING request_id`,
		created.ID, eventID).Scan(&returned); err != nil {
		t.Fatalf("事务内投影写入失败: %v", err)
	}
	if returned != created.ID {
		t.Fatalf("事务内返回 id = %d, want %d", returned, created.ID)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("提交事务失败: %v", err)
	}

	// 提交后再投递同一 request_id 必须被判定为已应用。
	fresh, err := pools.InsertProjAppliedRequest(ctx, created.ID, eventID)
	if err != nil {
		t.Fatalf("重复投递失败: %v", err)
	}
	if fresh {
		t.Fatal("事务内已写入的 request_id 不得再次判定为新应用")
	}
}
