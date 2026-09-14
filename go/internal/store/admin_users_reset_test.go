package store

import (
	"context"
	"testing"
	"time"
)

// 本文件钉住用户统计重置的落库面（reset-service.ts 的四段 SQL）的两条易错边界：
//  1. 切点两侧的行：只删 `<= 切点`，切点之后的行必须留下；
//  2. 成本重置标记：只清 `<= 切点` 的，之后的保留（那代表一次刚生效的重置）。

// resetFixture 建一个专属用户与若干行，并登记清理。
func resetFixture(t *testing.T, pools *Pools) int64 {
	t.Helper()
	ctx := context.Background()
	pool, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	name := "go-store-reset-it-" + time.Now().Format("20060102150405.000000000")
	var userID int64
	if err := pool.QueryRow(ctx, `INSERT INTO users (name) VALUES ($1) RETURNING id`, name).Scan(&userID); err != nil {
		t.Fatalf("建测试用户失败: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		pool, err := pools.Writer()
		if err != nil {
			return
		}
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM usage_ledger WHERE user_id = $1`, userID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM message_request WHERE user_id = $1`, userID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM keys WHERE user_id = $1`, userID)
		_, _ = pool.Exec(cleanupCtx, `DELETE FROM users WHERE id = $1`, userID)
	})
	return userID
}

// createResetRequest 建一条 message_request 并把 created_at 改成给定时刻。
func createResetRequest(t *testing.T, pools *Pools, userID int64, key string, createdAt time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	model := "go-store-reset-it-model"
	endpoint := "/v1/messages"
	cost := "0.000000000000000"
	row, err := pools.CreateMessageRequest(ctx, CreateMessageRequestData{
		ProviderID:   1,
		UserID:       userID,
		Key:          key,
		Model:        &model,
		Endpoint:     &endpoint,
		CostUSD:      &cost,
		RoutingTrace: []byte(`{"version":1}`),
	})
	if err != nil {
		t.Fatalf("建 message_request 失败: %v", err)
	}
	pool, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE message_request SET created_at = $2 WHERE id = $1`, row.ID, createdAt); err != nil {
		t.Fatalf("改 created_at 失败: %v", err)
	}
	return row.ID
}

func TestAdminUserResetDrainsOnlyBeforeCutoff(t *testing.T) {
	pools := openTestPools(t)
	ctx := context.Background()
	userID := resetFixture(t, pools)
	key := "go-store-reset-it-" + time.Now().Format("20060102150405.000000001")
	oldID := createResetRequest(t, pools, userID, key, time.Now().Add(-2*time.Hour))
	cut := time.Now().Add(-time.Hour)
	midID := createResetRequest(t, pools, userID, key, time.Now().Add(-90*time.Minute))
	newID := createResetRequest(t, pools, userID, key, time.Now().Add(30*time.Minute))

	deleted, err := pools.DrainAdminUserMessageRequests(ctx, userID, cut, UserStatisticsResetBatchSize)
	if err != nil {
		t.Fatalf("分批删除失败: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("应删掉切点及之前的 2 行，实际 %d", deleted)
	}
	remaining, err := pools.HasAdminUserRemainingRows(ctx, "message_request", userID, cut)
	if err != nil {
		t.Fatalf("检查残留失败: %v", err)
	}
	if remaining {
		t.Fatal("删完后不应有残留")
	}
	// 切点之后的行必须还在（含那条「未来时刻」的行）。
	pool, err := pools.Data()
	if err != nil {
		t.Fatalf("取读分道失败: %v", err)
	}
	for _, id := range []int64{newID} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM message_request WHERE id = $1)`, id).Scan(&exists); err != nil {
			t.Fatalf("查行存在性失败: %v", err)
		}
		if !exists {
			t.Fatalf("切点之后的行 %d 不得被删", id)
		}
	}
	for _, id := range []int64{oldID, midID} {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM message_request WHERE id = $1)`, id).Scan(&exists); err != nil {
			t.Fatalf("查行存在性失败: %v", err)
		}
		if exists {
			t.Fatalf("切点及之前的行 %d 必须被删", id)
		}
	}
	// 分批：batch=1 时应一次只删一行。
	extraID := createResetRequest(t, pools, userID, key, time.Now().Add(-3*time.Hour))
	one, err := pools.DrainAdminUserMessageRequests(ctx, userID, cut, 1)
	if err != nil {
		t.Fatalf("单行批删除失败: %v", err)
	}
	if one != 1 {
		t.Fatalf("batch=1 时应删 1 行，实际 %d", one)
	}
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM message_request WHERE id = $1)`, extraID).Scan(&exists); err != nil {
		t.Fatalf("查行存在性失败: %v", err)
	}
	if exists {
		t.Fatal("该行应已被删掉")
	}
}

func TestAdminUserResetClearsOnlyStaleResetTimestamps(t *testing.T) {
	pools := openTestPools(t)
	ctx := context.Background()
	userID := resetFixture(t, pools)
	pool, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	stale := time.Now().Add(-3 * time.Hour)
	fresh := time.Now().Add(time.Hour)
	if _, err := pool.Exec(ctx, `
		UPDATE users SET cost_reset_at = $2, limit_5h_cost_reset_at = $3 WHERE id = $1`,
		userID, stale, fresh); err != nil {
		t.Fatalf("设置重置标记失败: %v", err)
	}
	cut := time.Now()
	updated, err := pools.ClearAdminUserResetTimestamps(ctx, userID, cut)
	if err != nil {
		t.Fatalf("清重置标记失败: %v", err)
	}
	if !updated {
		t.Fatal("应命中一行")
	}
	var costResetAt, limit5h *time.Time
	if err := pool.QueryRow(ctx,
		`SELECT cost_reset_at, limit_5h_cost_reset_at FROM users WHERE id = $1`, userID).
		Scan(&costResetAt, &limit5h); err != nil {
		t.Fatalf("读重置标记失败: %v", err)
	}
	if costResetAt != nil {
		t.Fatalf("切点之前的标记应被清空，实际 %s", costResetAt)
	}
	if limit5h == nil {
		t.Fatal("切点之后的标记必须保留")
	}
	// 软删用户不得被更新（Node 的 `isNull(users.deletedAt)`）。
	if _, err := pool.Exec(ctx, `UPDATE users SET deleted_at = now() WHERE id = $1`, userID); err != nil {
		t.Fatalf("软删用户失败: %v", err)
	}
	hit, err := pools.ClearAdminUserResetTimestamps(ctx, userID, time.Now())
	if err != nil {
		t.Fatalf("软删用户的清理报错: %v", err)
	}
	if hit {
		t.Fatal("软删用户不应被更新")
	}
}

func TestAdminUserResetListsActiveKeysWithPlaintext(t *testing.T) {
	pools := openTestPools(t)
	ctx := context.Background()
	userID := resetFixture(t, pools)
	plain := "sk-go-store-reset-it-" + time.Now().Format("20060102150405.000000002")
	keyID, err := pools.CreateAdminUserDefaultKey(ctx, userID, plain, "default", nil)
	if err != nil {
		t.Fatalf("建密钥失败: %v", err)
	}
	keys, err := pools.ListAdminUserResetKeys(ctx, userID)
	if err != nil {
		t.Fatalf("列密钥失败: %v", err)
	}
	if len(keys) != 1 || keys[0].ID != keyID || keys[0].Key != plain {
		t.Fatalf("密钥清单不符：%+v（期望 id=%d）", keys, keyID)
	}
	// 软删的密钥不在清单里（Node 的 findUserStatisticsResetKeyIds 同条件）。
	pool, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE keys SET deleted_at = now() WHERE id = $1`, keyID); err != nil {
		t.Fatalf("软删密钥失败: %v", err)
	}
	keys, err = pools.ListAdminUserResetKeys(ctx, userID)
	if err != nil {
		t.Fatalf("列密钥失败: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("软删的密钥不应出现：%+v", keys)
	}
}

func TestAdminUserResetRejectsUnknownTable(t *testing.T) {
	pools := openTestPools(t)
	if _, err := pools.HasAdminUserRemainingRows(context.Background(), "users", 1, time.Now()); err == nil {
		t.Fatal("未声明的表名必须报错（标识符不得来自调用方）")
	}
}
