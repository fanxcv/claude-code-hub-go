package migrate

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// 集成用例需要 CCH_TEST_DSN（用户须有 CREATEDB 权限——本仓测试库账号即是）。
// 刻意不复用被测包之外的夹具：迁移用例要自己造库、自己算账本，避免与共享库的既有状态纠缠。
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("CCH_TEST_DSN")
	if strings.TrimSpace(dsn) == "" {
		t.Skip("未设置 CCH_TEST_DSN，跳过迁移集成测试")
	}
	return dsn
}

func openTestPool(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := migrateOpen(ctx, dsn)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// migrateOpen 是本文件内的薄封装，避免直接依赖 Options 结构的变化。
func migrateOpen(ctx context.Context, dsn string) (*pgxpool.Pool, error) { return Open(ctx, dsn) }

// ledgerSnapshot 是账本的完整快照（用于「前后逐字段不变」的断言）。
type ledgerSnapshot struct {
	rows []ledgerRow
}

func snapshotLedger(t *testing.T, pool *pgxpool.Pool) ledgerSnapshot {
	t.Helper()
	rows, err := ledgerRows(context.Background(), pool)
	if err != nil {
		t.Fatalf("读账本失败: %v", err)
	}
	return ledgerSnapshot{rows: rows}
}

func (s ledgerSnapshot) equal(other ledgerSnapshot) bool {
	if len(s.rows) != len(other.rows) {
		return false
	}
	for i := range s.rows {
		a, b := s.rows[i], other.rows[i]
		if a.ID != b.ID || a.Hash != b.Hash || !sameText(a.CreatedAt, b.CreatedAt) {
			return false
		}
	}
	return true
}

func sameText(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// scratchDatabase 造一个临时库并返回它的 DSN；用例结束即 DROP（含强制断开残留连接）。
func scratchDatabase(t *testing.T, baseDSN string) string {
	t.Helper()
	parsed, err := url.Parse(baseDSN)
	if err != nil {
		t.Fatalf("解析 DSN 失败: %v", err)
	}
	name := fmt.Sprintf("cch_migrate_%d", time.Now().UnixNano()%1e9)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	admin, err := migrateOpen(ctx, baseDSN)
	if err != nil {
		t.Fatalf("连接管理库失败: %v", err)
	}
	if _, err := admin.Exec(ctx, `CREATE DATABASE "`+name+`"`); err != nil {
		admin.Close()
		t.Fatalf("建临时库失败（需要 CREATEDB 权限）: %v", err)
	}
	admin.Close()

	scratch := *parsed
	scratch.Path = "/" + name
	scratchDSN := scratch.String()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		admin, err := migrateOpen(ctx, baseDSN)
		if err != nil {
			t.Logf("清理：连接管理库失败（临时库 %s 可能残留）: %v", name, err)
			return
		}
		defer admin.Close()
		_, _ = admin.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, name)
		if _, err := admin.Exec(ctx, `DROP DATABASE IF EXISTS "`+name+`"`); err != nil {
			t.Logf("清理：DROP DATABASE %s 失败: %v", name, err)
		}
	})
	return scratchDSN
}

// TestUpIsNoOpOnMigratedDatabase 是验证 (a)：对**已迁移库**跑 Up 必须零应用、账本逐字段不变。
//
// 用「由本包亲自迁完的临时库」作为「已迁移库」：仓内共享测试库 cch_loadtest 的 drizzle schema
// 归 postgres 超级用户所有，测试账号读不到账本（生产进程连的正是超级用户，故线上不受影响）。
// 用临时库还能顺带覆盖「同一套代码连续跑两次」的真实重启场景。
func TestUpIsNoOpOnMigratedDatabase(t *testing.T) {
	dsn := scratchDatabase(t, testDSN(t))
	pool := openTestPool(t, dsn)
	ctx := context.Background()

	if _, err := Up(ctx, pool); err != nil {
		t.Fatalf("首次迁移失败: %v", err)
	}

	before := snapshotLedger(t, pool)
	if len(before.rows) == 0 {
		t.Fatal("首次迁移后账本仍为空")
	}

	result, err := Up(ctx, pool)
	if err != nil {
		t.Fatalf("Up 失败: %v", err)
	}
	if !result.UpToDate() {
		t.Fatalf("已迁移库上不应有应用项，实际应用 %d 条: %v", result.Applied, result.AppliedTags)
	}
	if result.Applied != 0 || result.Statements != 0 {
		t.Errorf("零应用断言失败: applied=%d statements=%d", result.Applied, result.Statements)
	}

	after := snapshotLedger(t, pool)
	if !before.equal(after) {
		t.Errorf("账本被改动：前 %d 行 / 后 %d 行（id-hash-created_at 须逐字段相同）",
			len(before.rows), len(after.rows))
	}

	migrations, err := Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	latestWant := migrations[len(migrations)-1].When
	if result.LatestAfter == nil || *result.LatestAfter != latestWant {
		t.Errorf("迁移后水位应等于末条 when=%d，实际 %v", latestWant, result.LatestAfter)
	}

	status, err := Status(ctx, pool)
	if err != nil {
		t.Fatalf("Status 失败: %v", err)
	}
	if !status.UpToDate() {
		t.Errorf("Status 认为仍有待应用项: %v", status.Pending)
	}
	if status.Applied != len(before.rows) {
		t.Errorf("Status 报账本 %d 行，快照为 %d 行", status.Applied, len(before.rows))
	}
	if len(status.Mismatched) != 0 {
		t.Errorf("账本存在 created_at 与 journal 不一致的行（自愈应修掉它们）: %v", status.Mismatched)
	}
	if len(status.Unknown) != 0 {
		t.Errorf("账本存在 journal 中不认识的 hash: %v", status.Unknown)
	}
}

// TestUpMigratesEmptyDatabase 是验证 (b)：空库上全量迁移，且账本逐条可核对。
func TestUpMigratesEmptyDatabase(t *testing.T) {
	dsn := scratchDatabase(t, testDSN(t))
	pool := openTestPool(t, dsn)
	ctx := context.Background()

	migrations, err := Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	statements := 0
	for _, m := range migrations {
		statements += len(m.Statements)
	}

	result, err := Up(ctx, pool)
	if err != nil {
		t.Fatalf("空库迁移失败: %v", err)
	}
	if result.Applied != len(migrations) {
		t.Fatalf("应应用全部 %d 条，实际 %d 条", len(migrations), result.Applied)
	}
	if result.Statements != statements {
		t.Errorf("执行的语句块数 %d，期望 %d", result.Statements, statements)
	}
	if len(result.AppliedTags) != len(migrations) {
		t.Errorf("AppliedTags 长度 %d，期望 %d", len(result.AppliedTags), len(migrations))
	}

	// 空库上 baseTablesReady=false → 第一条 preflight 不跑；收尾那条恒跑。
	if result.PreflightRuns != 1 {
		t.Errorf("空库的 preflight 次数应为 1（只跑收尾那条），实际 %d", result.PreflightRuns)
	}

	expected, err := WhenByHash()
	if err != nil {
		t.Fatalf("WhenByHash 失败: %v", err)
	}
	rows, err := ledgerRows(ctx, pool)
	if err != nil {
		t.Fatalf("读账本失败: %v", err)
	}
	if len(rows) != len(migrations) {
		t.Fatalf("账本 %d 行，期望 %d 行", len(rows), len(migrations))
	}
	for _, row := range rows {
		want, ok := expected[row.Hash]
		if !ok {
			t.Errorf("账本行 id=%d 的 hash 不在 journal 中: %s", row.ID, row.Hash)
			continue
		}
		got, ok := row.when()
		if !ok || got != want {
			t.Errorf("账本行 id=%d 的 created_at 与 journal when 不符: 得到 %v，期望 %d", row.ID, row.CreatedAt, want)
		}
	}

	// 二次运行必须 no-op（幂等）。
	second, err := Up(ctx, pool)
	if err != nil {
		t.Fatalf("二次 Up 失败: %v", err)
	}
	if !second.UpToDate() {
		t.Errorf("二次运行不应再应用任何迁移，实际 %d 条", second.Applied)
	}

	// 基表存在后再跑：第一次 preflight 的判据此时为真，但水位已过里程碑 → 仍只跑收尾那条。
	third, err := Up(ctx, pool)
	if err != nil {
		t.Fatalf("三次 Up 失败: %v", err)
	}
	if third.PreflightRuns != 1 {
		t.Errorf("水位已过里程碑时 preflight 次数应为 1，实际 %d", third.PreflightRuns)
	}
}

// TestUpRollsBackOnFailingMigration 是验证 (d)：一条迁移失败时整体回滚、账本不登记。
//
// 注入而非改真源：注入列表的 when 远大于当前水位，故只有它被视为待应用；
// 其中第一条语句必然失败（引用不存在的表），第二条是「若前一条不回滚就会留下痕迹」的建表语句。
func TestUpRollsBackOnFailingMigration(t *testing.T) {
	dsn := scratchDatabase(t, testDSN(t))
	pool := openTestPool(t, dsn)
	ctx := context.Background()

	if _, err := Up(ctx, pool); err != nil {
		t.Fatalf("先做一次全量迁移以建立基表: %v", err)
	}
	before := snapshotLedger(t, pool)

	const marker = "cch_migrate_rollback_marker"
	injected := []Migration{{
		Tag:  "9999_broken",
		When: 9999999999999,
		Hash: "0000000000000000000000000000000000000000000000000000000000000000",
		Statements: []string{
			`CREATE TABLE "public"."` + marker + `" (id int)`,
			`SELECT * FROM "public"."definitely_missing_table_for_rollback_test"`,
		},
	}}

	_, err := Up(ctx, pool, Options{Migrations: injected})
	if err == nil {
		t.Fatal("注入的坏迁移应当让 Up 失败")
	}

	after := snapshotLedger(t, pool)
	if !before.equal(after) {
		t.Errorf("坏迁移失败后账本被改动：前 %d 行 / 后 %d 行", len(before.rows), len(after.rows))
	}

	var exists *string
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.`+marker+`')::text`).Scan(&exists); err != nil {
		t.Fatalf("探测回滚标记表失败: %v", err)
	}
	if exists != nil && *exists != "" {
		t.Errorf("坏迁移的第一条语句未被回滚：表 %s 仍存在（Node 侧是一个事务，任一句失败即整体回滚）", marker)
	}
}

// TestPlanGateUsesMaxNotTopRow 钉住两处**不同口径**的水位取值。
//
// Node 里有两个水位读数，用途不同且不可互换：
//   - drizzle 的迁移阈值：`order by created_at desc limit 1`（NULL 行会被排在首位）；
//   - 迁移计划的 preflight 门禁：`MAX(created_at)`（忽略 NULL）。
//
// 造一个「账本顶部是 NULL 行、其余为正常水位」的库：若门禁误用「首行」，LatestBefore 会因
// when() 解析失败而为 nil → 首次 preflight 被多跑一次（PreflightRuns=2）；用 MAX 则为 1。
func TestPlanGateUsesMaxNotTopRow(t *testing.T) {
	dsn := scratchDatabase(t, testDSN(t))
	pool := openTestPool(t, dsn)
	ctx := context.Background()

	if _, err := Up(ctx, pool); err != nil {
		t.Fatalf("先全量迁移: %v", err)
	}
	// 插入一行 created_at 为 NULL 的账本行（hash 不在 journal 里，故自愈不会动它）。
	if _, err := pool.Exec(ctx,
		`INSERT INTO "drizzle"."__drizzle_migrations" ("hash","created_at") VALUES ('deadbeef', NULL)`,
	); err != nil {
		t.Fatalf("插入 NULL 水位行失败: %v", err)
	}

	// 注入**空**迁移列表把 applyPending 变成 no-op：NULL 首行会让 drizzle 的阈值口径
	// 退回 0（Number(null)=0），于是「首行口径」下所有迁移都算待应用并真的重放 DDL
	// （Node 同样如此——这是账本被人写坏时的固有行为）。要单独观察门禁口径，
	// 必须把应用阶段摘出去。
	result, err := Up(ctx, pool, Options{Migrations: []Migration{}})
	if err != nil {
		t.Fatalf("Up 失败: %v", err)
	}
	if result.Applied != 0 {
		t.Errorf("不应有应用项，实际 %d 条", result.Applied)
	}
	if result.LatestBefore != nil {
		t.Errorf("首行口径应因 NULL 而无法解析（LatestBefore=nil），实际 %v", *result.LatestBefore)
	}
	if result.PreflightRuns != 1 {
		t.Errorf("门禁应按 MAX 判定（水位已过里程碑 → 只跑收尾一次），实际 preflight 次数 %d", result.PreflightRuns)
	}
	if result.LatestMaxAfter == nil || *result.LatestMaxAfter != nodeLastWhen {
		t.Errorf("MAX 水位应为 %d，实际 %v", nodeLastWhen, result.LatestMaxAfter)
	}
}
