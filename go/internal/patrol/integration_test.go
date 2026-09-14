package patrol

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fanxcv/claude-code-hub-go/go/internal/config"
	"github.com/fanxcv/claude-code-hub-go/go/internal/store"
)

// 本文件的集成用例跑在真实库上（CCH_TEST_DSN），并遵守两条夹具纪律：
//
//  1. **自钉为唯一候选**：夹具行的时间戳被改到 2020 年，而巡检阈值取 24 小时——库里
//     早于 24 小时的未终态行实测为 0，因此本轮的候选集恰好只有夹具行，
//     不会去补写别的 lane 留下的历史行（那正是「共享库互扰」的来源）。
//  2. **按精确 id/key 清理**：清理只认本用例的 key，且先删派生行（账本、投影、outbox）。
//
// 不等待真实长超时：年龄靠改 created_at 造，阈值靠参数给，不用 sleep 等窗口。

const patrolThreshold = 24 * time.Hour

// fixtureEpoch 是夹具行的 created_at：远早于任何阈值，故必然落在候选集里。
var fixtureEpoch = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

func integrationDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("CCH_TEST_DSN")
	if dsn == "" {
		t.Skip("未设置 CCH_TEST_DSN，跳过数据库集成测试")
	}
	return dsn
}

func openPatrolPools(t *testing.T) *store.Pools {
	t.Helper()
	pools, err := store.Open(context.Background(), store.Options{
		DSN:      integrationDSN(t),
		Budget:   config.SplitPoolBudget(6),
		Timeouts: config.DBTimeouts{IdleTimeout: 5 * time.Second, ConnectTimeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatalf("建立连接池失败: %v", err)
	}
	t.Cleanup(func() { _ = pools.Close() })
	return pools
}

func patrolKey(t *testing.T) string {
	t.Helper()
	return "go-patrol-it-" + time.Now().Format("20060102150405.000000000")
}

// createPatrolRow 建一行未终态的 message_request，并把 created_at 钉到指定时刻。
func createPatrolRow(t *testing.T, pools *store.Pools, key string, createdAt time.Time) int64 {
	t.Helper()
	ctx := context.Background()
	model := "go-patrol-it-model"
	endpoint := "/v1/messages"
	cost := "0.000000000000000"
	row, err := pools.CreateMessageRequest(ctx, store.CreateMessageRequestData{
		ProviderID:   1,
		UserID:       1,
		Key:          key,
		Model:        &model,
		Endpoint:     &endpoint,
		CostUSD:      &cost,
		RoutingTrace: []byte(`{"version":1}`),
	})
	if err != nil {
		t.Fatalf("创建夹具行失败: %v", err)
	}
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE message_request SET created_at = $2 WHERE id = $1`, row.ID, createdAt); err != nil {
		t.Fatalf("钉夹具行时间失败: %v", err)
	}
	return row.ID
}

func cleanupPatrolRows(t *testing.T, pools *store.Pools, key string) {
	t.Helper()
	ctx := context.Background()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	// 顺序：先删派生行，再删主行（触发器会写账本与 outbox）。
	for _, statement := range []string{
		`DELETE FROM outbox_events WHERE aggregate_id IN (SELECT id FROM message_request WHERE key = $1)`,
		`DELETE FROM proj_applied_requests WHERE request_id IN (SELECT id FROM message_request WHERE key = $1)`,
		`DELETE FROM usage_ledger WHERE request_id IN (SELECT id FROM message_request WHERE key = $1)`,
		`DELETE FROM message_request WHERE key = $1`,
	} {
		if _, err := pool.Exec(ctx, statement, key); err != nil {
			t.Fatalf("清理夹具失败（%s）: %v", statement, err)
		}
	}
}

// patrolRow 是夹具行的终态视图。
type patrolRow struct {
	Status     *int
	Message    *string
	Stack      *string
	DeletedAt  *time.Time
	DurationMS *int
}

func readPatrolRow(t *testing.T, pools *store.Pools, id int64) patrolRow {
	t.Helper()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	var row patrolRow
	if err := pool.QueryRow(context.Background(),
		`SELECT status_code, error_message, error_stack, deleted_at, duration_ms
		 FROM message_request WHERE id = $1`, id).
		Scan(&row.Status, &row.Message, &row.Stack, &row.DeletedAt, &row.DurationMS); err != nil {
		t.Fatalf("读取夹具行失败: %v", err)
	}
	return row
}

// countOlderCandidates 报告候选集规模；夹具纪律要求本轮除夹具行外没有别的老旧未终态行。
func countOlderCandidates(t *testing.T, pools *store.Pools, key string) int {
	t.Helper()
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	var total int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM message_request
		 WHERE status_code IS NULL AND deleted_at IS NULL
		   AND created_at < $1 AND key <> $2`,
		time.Now().Add(-patrolThreshold), key).Scan(&total); err != nil {
		t.Fatalf("统计候选行失败: %v", err)
	}
	return total
}

func newIntegrationPatrol(t *testing.T, pools *store.Pools) *Patrol {
	t.Helper()
	runner, err := New(Options{
		Store:           NewDBStore(pools),
		UnsettledAfter:  patrolThreshold,
		BatchSize:       200,
		MaxRowsPerRound: 1000,
	})
	if err != nil {
		t.Fatalf("构造巡检失败: %v", err)
	}
	return runner
}

// TestIntegrationPatrolRepairsStaleUnsettledRow 用例①：老于阈值的未终态行被补成 499/CLIENT_ABORTED。
func TestIntegrationPatrolRepairsStaleUnsettledRow(t *testing.T) {
	pools := openPatrolPools(t)
	key := patrolKey(t)
	t.Cleanup(func() { cleanupPatrolRows(t, pools, key) })

	id := createPatrolRow(t, pools, key, fixtureEpoch)
	if other := countOlderCandidates(t, pools, key); other != 0 {
		t.Fatalf("夹具纪律要求候选集只有夹具行，实测另有 %d 行老旧未终态行", other)
	}
	before := readPatrolRow(t, pools, id)
	if before.Status != nil {
		t.Fatalf("夹具行本应无终态，实际 %v", *before.Status)
	}

	result, err := newIntegrationPatrol(t, pools).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("巡检轮次报错: %v", err)
	}
	if result.Repaired != 1 {
		t.Fatalf("本应恰好补写 1 行，实际 %+v", result)
	}

	after := readPatrolRow(t, pools, id)
	if after.Status == nil || *after.Status != RepairStatusCode {
		t.Fatalf("终态应为 %d，实际 %v", RepairStatusCode, after.Status)
	}
	if after.Message == nil || *after.Message != RepairErrorMessage {
		t.Fatalf("error_message 应为 %q，实际 %v", RepairErrorMessage, after.Message)
	}
	if after.Stack == nil || !strings.HasPrefix(*after.Stack, MarkerPrefix) {
		t.Fatalf("error_stack 应带标记前缀，实际 %v", after.Stack)
	}
	// 不变量 P2/P3：不编造时长、不写成本。
	if after.DurationMS != nil {
		t.Fatalf("补写不得编造 duration_ms，实际 %v", *after.DurationMS)
	}

	// 幂等：再跑一轮不再补写同一行。
	second, err := newIntegrationPatrol(t, pools).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("第二轮报错: %v", err)
	}
	if second.Repaired != 0 {
		t.Fatalf("已终态行不得被再次补写，实际 %+v", second)
	}
}

// TestIntegrationPatrolLeavesFreshRowUntouched 用例②：新于阈值的行不补。
func TestIntegrationPatrolLeavesFreshRowUntouched(t *testing.T) {
	pools := openPatrolPools(t)
	key := patrolKey(t)
	t.Cleanup(func() { cleanupPatrolRows(t, pools, key) })

	id := createPatrolRow(t, pools, key, time.Now())
	result, err := newIntegrationPatrol(t, pools).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("巡检轮次报错: %v", err)
	}
	if result.Found != 0 || result.Repaired != 0 {
		t.Fatalf("阈值内的行不该进候选集，实际 %+v", result)
	}
	if row := readPatrolRow(t, pools, id); row.Status != nil {
		t.Fatalf("新行不应被补终态，实际 %v", *row.Status)
	}
}

// TestIntegrationPatrolDoesNotOverwriteTerminalRow 用例③：已终态行绝不覆盖。
func TestIntegrationPatrolDoesNotOverwriteTerminalRow(t *testing.T) {
	pools := openPatrolPools(t)
	key := patrolKey(t)
	t.Cleanup(func() { cleanupPatrolRows(t, pools, key) })

	id := createPatrolRow(t, pools, key, fixtureEpoch)
	durationMS := 1234
	statusCode := 200
	won, err := pools.UpdateDetailsIfUnfinalized(context.Background(), id, store.DetailsPatch{
		DurationMS: &durationMS,
		StatusCode: &statusCode,
	})
	if err != nil || !won {
		t.Fatalf("夹具终态写失败: won=%v err=%v", won, err)
	}

	result, err := newIntegrationPatrol(t, pools).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("巡检轮次报错: %v", err)
	}
	if result.Repaired != 0 || result.Failed != 0 {
		t.Fatalf("已终态行不该被补写或报错，实际 %+v", result)
	}
	row := readPatrolRow(t, pools, id)
	if row.Status == nil || *row.Status != 200 {
		t.Fatalf("终态必须保持 200，实际 %v", row.Status)
	}
	if row.Message != nil || row.Stack != nil {
		t.Fatalf("已终态行的 error 列不得被写入，实际 message=%v stack=%v", row.Message, row.Stack)
	}
	if row.DurationMS == nil || *row.DurationMS != durationMS {
		t.Fatalf("已终态行的 duration_ms 不得被改写，实际 %v", row.DurationMS)
	}
}

// TestIntegrationPatrolSkipsSoftDeletedRow 用例④：软删除的行不补（它已不在任何读路径上）。
func TestIntegrationPatrolSkipsSoftDeletedRow(t *testing.T) {
	pools := openPatrolPools(t)
	key := patrolKey(t)
	t.Cleanup(func() { cleanupPatrolRows(t, pools, key) })

	id := createPatrolRow(t, pools, key, fixtureEpoch)
	pool, err := pools.Control()
	if err != nil {
		t.Fatalf("取分道失败: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE message_request SET deleted_at = now() WHERE id = $1`, id); err != nil {
		t.Fatalf("软删除夹具失败: %v", err)
	}

	result, err := newIntegrationPatrol(t, pools).RunOnce(context.Background())
	if err != nil {
		t.Fatalf("巡检轮次报错: %v", err)
	}
	if result.Repaired != 0 {
		t.Fatalf("软删除行不该被补写，实际 %+v", result)
	}
	if row := readPatrolRow(t, pools, id); row.Status != nil {
		t.Fatalf("软删除行不应被补终态，实际 %v", *row.Status)
	}
}

// TestIntegrationPatrolConcurrentRoundsSingleWinner 用例⑤：并发两轮只有一个赢家，不重复补。
func TestIntegrationPatrolConcurrentRoundsSingleWinner(t *testing.T) {
	pools := openPatrolPools(t)
	key := patrolKey(t)
	t.Cleanup(func() { cleanupPatrolRows(t, pools, key) })

	id := createPatrolRow(t, pools, key, fixtureEpoch)
	if other := countOlderCandidates(t, pools, key); other != 0 {
		t.Fatalf("夹具纪律要求候选集只有夹具行，实测另有 %d 行", other)
	}

	runner := newIntegrationPatrol(t, pools)
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []Result
		errs    []error
	)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := runner.RunOnce(context.Background())
			mu.Lock()
			results = append(results, result)
			errs = append(errs, err)
			mu.Unlock()
		}()
	}
	wg.Wait()

	repaired, skipped, failed := 0, 0, 0
	for index, result := range results {
		if errs[index] != nil {
			t.Fatalf("并发轮次报错: %v", errs[index])
		}
		repaired += result.Repaired
		skipped += result.Skipped
		failed += result.Failed
	}
	if failed != 0 {
		t.Fatalf("并发轮次不应有失败，实际 %d", failed)
	}
	if repaired != 1 {
		t.Fatalf("同一行只能被补写一次，实际 repaired=%d skipped=%d", repaired, skipped)
	}
	if skipped != 1 {
		t.Fatalf("输的那一轮应记为 skipped，实际 %d（results=%+v）", skipped, results)
	}
	if row := readPatrolRow(t, pools, id); row.Status == nil || *row.Status != RepairStatusCode {
		t.Fatalf("终态应为 %d，实际 %v", RepairStatusCode, row.Status)
	}
}
