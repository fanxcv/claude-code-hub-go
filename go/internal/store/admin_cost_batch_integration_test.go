package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// 本测试对齐的是 Node 的**取数语义**（`src/repository/statistics.ts` 的两个批量函数 +
// 改造前 SSR 页面的实际调用方式），不是某个形状。故断言用「**手写 SQL 复刻 Node 的分支语义**」
// 与 Go 的**单查询实现**对账——两种独立表达互相印证，比拿 Go 自己的输出当期望值强。
//
// Node 的分支语义（复刻要点）：
//   - 有 `costResetAt` 的实体：**逐条**算 `SUM(cost_usd) WHERE created_at >= resetAt`；
//   - 没有的：一次 `GROUP BY` 全时段；
//   - 密钥维度：重置时刻取 **key 与 user 两者较晚者**（`resolveKeyCostResetAt`）；
//   - 计费条件：排除 `blocked_by` 非空、`is_replay = true`、非计费端点（count_tokens / compact）。
//
// 夹具一律带唯一 marker 并在 t.Cleanup 里按 id 清理（共享测试库只增删自己的行）。

const costBatchITPrefix = "cch-it-costbatch"

type costBatchFixture struct {
	userIDs []int64
	keyIDs  []int64
	// userResetFree / userResetCut 是两个语义不同的用户：
	// 前者无 cost_reset_at（全时段），后者有（只算其后的行）。
	userResetFree int64
	userResetCut  int64
	keyResetFree  int64
	keyResetLater int64
}

func seedCostBatchFixture(t *testing.T, pools *Pools) costBatchFixture {
	t.Helper()
	ctx := context.Background()
	pool, err := pools.Writer()
	if err != nil {
		t.Fatalf("取写分道失败: %v", err)
	}
	marker := itKey(t)

	var providerID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO providers (name, url, key, provider_type, is_enabled, weight, priority,
			group_tag, protocol_conversion_enabled)
		VALUES ($1, 'http://127.0.0.1:9', 'upstream-not-used', 'codex', true, 1, 0, 'default', true)
		RETURNING id`, costBatchITPrefix+"-"+marker).Scan(&providerID); err != nil {
		t.Fatalf("建供应商失败: %v", err)
	}

	cut := time.Now().Add(-2 * time.Hour).Truncate(time.Millisecond)
	// 密钥自身的重置时刻刻意**早于**其用户的重置时刻：生效值必须取较晚者（用户那一侧），
	// 这正是 `resolveKeyCostResetAt` 的语义，也是最容易写反的一处。
	earlier := cut.Add(-1 * time.Hour)

	newUser := func(name string, resetAt *time.Time) int64 {
		var id int64
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (name, cost_reset_at) VALUES ($1, $2) RETURNING id`,
			costBatchITPrefix+"-"+name+"-"+marker, resetAt,
		).Scan(&id); err != nil {
			t.Fatalf("建用户失败: %v", err)
		}
		return id
	}

	userResetFree := newUser("user-free", nil)
	userResetCut := newUser("user-cut", &cut)

	newKey := func(userID int64, name string, resetAt *time.Time) int64 {
		keyValue := costBatchITPrefix + "-" + name + "-" + marker
		var id int64
		if err := pool.QueryRow(ctx, `
			INSERT INTO keys (user_id, key, name, is_enabled, provider_group, cost_reset_at)
			VALUES ($1, $2, $3, true, 'default', $4) RETURNING id`,
			userID, keyValue, keyValue, resetAt).Scan(&id); err != nil {
			t.Fatalf("建密钥失败: %v", err)
		}
		return id
	}

	keyResetFree := newKey(userResetFree, "key-free", nil)
	// 该密钥自身的重置时刻**早于**其用户的重置时刻 → 生效值必须取较晚者（用户那一侧）。
	keyResetLater := newKey(userResetCut, "key-later", &earlier)

	requestID := int(time.Now().UnixNano() % 1_000_000_000)
	insert := func(userID int64, keyValue string, cost string, createdAt time.Time, endpoint string, replay bool, blocked bool) {
		var blockedBy any
		if blocked {
			blockedBy = "sensitive_word"
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO usage_ledger (request_id, provider_id, user_id, key, final_provider_id, model,
				cost_usd, input_tokens, output_tokens, status_code, is_success, duration_ms,
				created_at, is_replay, blocked_by, endpoint)
			VALUES ($1, $2, $3, $4, $2, $5, $6::numeric, 1, 1, 200, true, 1, $7, $8, $9, $10)`,
			requestID, providerID, userID, keyValue, costBatchITPrefix, cost, createdAt, replay, blockedBy, endpoint,
		); err != nil {
			t.Fatalf("种账本行失败: %v", err)
		}
		requestID++
	}

	keyFreeValue := costBatchITPrefix + "-key-free-" + marker
	keyLaterValue := costBatchITPrefix + "-key-later-" + marker

	// 用户 A（无重置）：全时段两行 = 1.5 + 0.5 = 2.0
	insert(userResetFree, keyFreeValue, "1.5", time.Now().Add(-72*time.Hour), "/v1/messages", false, false)
	insert(userResetFree, keyFreeValue, "0.5", time.Now().Add(-time.Hour), "/v1/messages", false, false)

	// 用户 B（cut 起算）：cut 前一行 10.0（**不得计入**）、cut 后两行 1.25 + 0.75 = 2.0
	insert(userResetCut, keyLaterValue, "10.0", cut.Add(-time.Minute), "/v1/messages", false, false)
	insert(userResetCut, keyLaterValue, "1.25", cut.Add(time.Minute), "/v1/messages", false, false)
	insert(userResetCut, keyLaterValue, "0.75", cut.Add(2*time.Minute), "/v1/messages", false, false)

	// 三种**必须排除**的行（全部挂在用户 A 名下，若排除失效则 A 的期望值会变）：
	insert(userResetFree, keyFreeValue, "7.0", time.Now().Add(-time.Hour), "/v1/messages", false, true)               // 被拦截
	insert(userResetFree, keyFreeValue, "8.0", time.Now().Add(-time.Hour), "/v1/messages", true, false)               // replay
	insert(userResetFree, keyFreeValue, "9.0", time.Now().Add(-time.Hour), "/v1/messages/count_tokens", false, false) // 非计费端点

	t.Cleanup(func() {
		cleanup := context.Background()
		users := []int64{userResetFree, userResetCut}
		for _, statement := range []string{
			`DELETE FROM usage_ledger WHERE user_id = ANY($1)`,
			`DELETE FROM keys WHERE user_id = ANY($1)`,
			`DELETE FROM users WHERE id = ANY($1)`,
			`DELETE FROM providers WHERE id = $1`,
		} {
			args := []any{users}
			if statement == `DELETE FROM providers WHERE id = $1` {
				args = []any{providerID}
			}
			if _, err := pool.Exec(cleanup, statement, args...); err != nil {
				t.Errorf("清理夹具失败（%s）: %v", statement, err)
			}
		}
	})

	return costBatchFixture{
		userIDs:       []int64{userResetFree, userResetCut},
		keyIDs:        []int64{keyResetFree, keyResetLater},
		userResetFree: userResetFree,
		userResetCut:  userResetCut,
		keyResetFree:  keyResetFree,
		keyResetLater: keyResetLater,
	}
}

// nodeUserCost 用 Node 的分支语义（有重置逐条算、无重置一次 GROUP BY）直接算期望值。
func nodeUserCost(t *testing.T, pools *Pools, userID int64, resetAt *time.Time) string {
	t.Helper()
	pool, err := pools.Data()
	if err != nil {
		t.Fatalf("取数据分道失败: %v", err)
	}
	query := fmt.Sprintf(
		`SELECT COALESCE(SUM(cost_usd), 0)::text FROM usage_ledger
		  WHERE user_id = $1 AND %s`, BillingCondition)
	args := []any{userID}
	if resetAt != nil {
		query += " AND created_at >= $2"
		args = append(args, *resetAt)
	}
	var total string
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&total); err != nil {
		t.Fatalf("复刻 Node 用户语义失败: %v", err)
	}
	return total
}

// TestIntegrationUserCostBatchMatchesNodeSemantics 用户维度：Go 的单查询实现 vs Node 的分支语义。
func TestIntegrationUserCostBatchMatchesNodeSemantics(t *testing.T) {
	pools := openTestPools(t)
	fixture := seedCostBatchFixture(t, pools)
	ctx := context.Background()
	pool, err := pools.Data()
	if err != nil {
		t.Fatalf("取数据分道失败: %v", err)
	}

	// 取出夹具用户各自的重置时刻，按 Node 的两条分支分别算期望值。
	var cut *time.Time
	if err := pool.QueryRow(ctx, `SELECT cost_reset_at FROM users WHERE id = $1`, fixture.userResetCut).Scan(&cut); err != nil {
		t.Fatalf("读夹具用户重置时刻失败: %v", err)
	}
	if cut == nil {
		t.Fatal("带重置的用户应当有 cost_reset_at")
	}

	expected := map[int64]string{
		fixture.userResetFree: nodeUserCost(t, pools, fixture.userResetFree, nil),
		fixture.userResetCut:  nodeUserCost(t, pools, fixture.userResetCut, cut),
	}

	got, err := pools.UserCostBatchTotalCost(ctx, fixture.userIDs)
	if err != nil {
		t.Fatalf("批量用户成本失败: %v", err)
	}
	for id, want := range expected {
		if got[id] != want {
			t.Fatalf("用户 %d 累计成本 = %q, 复刻 Node 语义得 %q", id, got[id], want)
		}
	}

	// 语义自证：无重置用户 = 1.5 + 0.5；有重置用户 = 1.25 + 0.75；三类排除行都没进账。
	if got[fixture.userResetFree] != "2.000000000000000" {
		t.Fatalf("无重置用户应为 2.0（1.5+0.5，排除被拦截/replay/非计费三行），实际 %q", got[fixture.userResetFree])
	}
	if got[fixture.userResetCut] != "2.000000000000000" {
		t.Fatalf("有重置用户应为 2.0（重置后 1.25+0.75，重置前 10.0 不计），实际 %q", got[fixture.userResetCut])
	}

	// 不存在的 id 必须有结果且为 "0"（Node 预填 0 的同义）。
	missing := int64(2_000_000_000)
	withMissing, err := pools.UserCostBatchTotalCost(ctx, append([]int64{missing}, fixture.userIDs...))
	if err != nil {
		t.Fatalf("含不存在 id 的批量查询失败: %v", err)
	}
	if withMissing[missing] != "0" {
		t.Fatalf("不存在的用户应为 \"0\"，实际 %q", withMissing[missing])
	}

	// 空入参：空表（不是错误）。
	empty, err := pools.UserCostBatchTotalCost(ctx, nil)
	if err != nil {
		t.Fatalf("空入参不应报错: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("空入参应返回空表，实际 %v", empty)
	}
}

// TestIntegrationKeyCostBatchMatchesNodeSemantics 密钥维度：含「key 与 user 取较晚者」的重置语义。
func TestIntegrationKeyCostBatchMatchesNodeSemantics(t *testing.T) {
	pools := openTestPools(t)
	fixture := seedCostBatchFixture(t, pools)
	ctx := context.Background()
	pool, err := pools.Data()
	if err != nil {
		t.Fatalf("取数据分道失败: %v", err)
	}

	// Node 的密钥语义：重置时刻 = max(key.cost_reset_at, user.cost_reset_at)（忽略 NULL）。
	expected := map[int64]string{}
	for _, keyID := range fixture.keyIDs {
		var keyValue string
		if err := pool.QueryRow(ctx, `
			SELECT k.key, k.cost_reset_at, u.cost_reset_at
			  FROM keys k LEFT JOIN users u ON u.id = k.user_id
			 WHERE k.id = $1`, keyID).Scan(&keyValue, new(*time.Time), new(*time.Time)); err != nil {
			t.Fatalf("读夹具密钥失败: %v", err)
		}
		query := fmt.Sprintf(
			`SELECT COALESCE(SUM(cost_usd), 0)::text FROM usage_ledger
			  WHERE key = $1 AND %s
			    AND created_at >= COALESCE(GREATEST(
			      (SELECT cost_reset_at FROM keys WHERE id = $2),
			      (SELECT u.cost_reset_at FROM keys k JOIN users u ON u.id = k.user_id WHERE k.id = $2)
			    ), '-infinity'::timestamptz)`, BillingCondition)
		var total string
		if err := pool.QueryRow(ctx, query, keyValue, keyID).Scan(&total); err != nil {
			t.Fatalf("复刻 Node 密钥语义失败: %v", err)
		}
		expected[keyID] = total
	}

	got, err := pools.KeyCostBatchTotalCost(ctx, fixture.keyIDs)
	if err != nil {
		t.Fatalf("批量密钥成本失败: %v", err)
	}
	for id, want := range expected {
		if got[id] != want {
			t.Fatalf("密钥 %d 累计成本 = %q, 复刻 Node 语义得 %q", id, got[id], want)
		}
	}

	// 语义自证：无重置密钥 = 0.4? 见夹具——key-free 挂在无重置用户下，其账本行是 1.5 + 0.5。
	if got[fixture.keyResetFree] != "2.000000000000000" {
		t.Fatalf("无重置密钥应为 2.0（即该密钥名下两行），实际 %q", got[fixture.keyResetFree])
	}
	// key-later：自身重置早于用户重置 → 生效的是用户那一侧（cut），故只计 cut 之后的两行。
	if got[fixture.keyResetLater] != "2.000000000000000" {
		t.Fatalf("较晚者语义应使该密钥只计重置后两行 = 2.0，实际 %q", got[fixture.keyResetLater])
	}

	missingKey := int64(2_000_000_000)
	withMissing, err := pools.KeyCostBatchTotalCost(ctx, append([]int64{missingKey}, fixture.keyIDs...))
	if err != nil {
		t.Fatalf("含不存在密钥 id 的批量查询失败: %v", err)
	}
	if withMissing[missingKey] != "0" {
		t.Fatalf("不存在的密钥应为 \"0\"，实际 %q", withMissing[missingKey])
	}
}
