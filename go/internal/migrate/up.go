package migrate

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// AdvisoryLockName 是迁移咨询锁的名字（src/lib/migrate.ts:16）。
//
// 名字必须与 Node 逐字相同：双跑期两个进程会对同一把锁互斥，
// 否则各自以为自己在独自演进 schema。锁键在 SQL 侧由 `hashtext($1)` 计算，
// 与 Node 的 `pg_advisory_lock(hashtext(name))` 完全同值。
const AdvisoryLockName = "claude-code-hub:migrations"

// Options 控制单次迁移运行的行为，仅测试使用。
type Options struct {
	// SkipSessionReplayPreflight 跳过 ⑤ 的索引 preflight（仅测试用；
	// 生产路径必须与 Node 一致地跑它）。
	SkipSessionReplayPreflight bool
	// Migrations 替换待应用列表（仅测试用：注入一条会失败的迁移以验证整体回滚与
	// 账本不登记的语义，而无需改真源 drizzle/*.sql）。nil 表示用内嵌清单。
	Migrations []Migration
}

// Result 是一次 Up 的可观测结果。
type Result struct {
	// Applied 是本次真正应用（执行 DDL/DML 并登记账本）的迁移条数。
	Applied int
	// AppliedTags 是本次应用的迁移 tag，按 journal 顺序。
	AppliedTags []string
	// Statements 是本次执行的语句块总数。
	Statements int
	// RepairedRows 是 created_at 自愈修复的行数。
	RepairedRows int
	// PreflightRuns 是索引 preflight 的执行次数（0/1/2，见 ⑤）。
	PreflightRuns int
	// LatestBefore / LatestAfter 是迁移前后的账本水位（drizzle 阈值口径：
	// `order by created_at desc limit 1` 那一行；NULL 行会被 PostgreSQL 排在首位）。
	LatestBefore *int64
	LatestAfter  *int64
	// LatestMaxAfter 是迁移后的 MAX(created_at)（计划门禁口径；无行或全为 NULL 时为 nil）。
	LatestMaxAfter *int64
}

// UpToDate 表示本次没有需要应用的迁移。
func (r Result) UpToDate() bool { return r.Applied == 0 }

// Open 建一条**专用**连接池（MaxConns=1），与 Node 的
// `postgres(process.env.DSN, {max: 1})`（src/lib/migrate.ts:131）同义。
//
// 为什么不复用共享池：迁移要在独占连接上 `SET search_path` 并在 session 级持有咨询锁，
// 借用共享池的连接会把这两个副作用漏进请求路径。
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("migrate: DSN 未设置")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("migrate: 解析 DSN 失败: %w", err)
	}
	cfg.MaxConns = 1
	cfg.MinConns = 0
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("migrate: 建迁移连接失败: %w", err)
	}
	return pool, nil
}

// Up 执行一次完整的迁移流程（复刻 runMigrations ①-⑥）。
//
// 失败即整体中止：drizzle 本体在一个事务里应用全部待迁移，任一句失败即回滚
// （pg-core/dialect.js:60-73 的 session.transaction），故不会留下半应用状态。
// 调用方（boot）负责在 err != nil 时按 Node 的语义退出 1。
func Up(ctx context.Context, pool *pgxpool.Pool, options ...Options) (Result, error) {
	var opts Options
	if len(options) > 0 {
		opts = options[0]
	}
	var result Result
	if pool == nil {
		return result, errors.New("migrate: 连接池为空")
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return result, fmt.Errorf("migrate: 取迁移连接失败: %w", err)
	}
	defer conn.Release()

	// ① 咨询锁：阻塞等待，与 Node 一致（不是 try 版）。
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtext($1))`, AdvisoryLockName); err != nil {
		return result, fmt.Errorf("migrate: 获取迁移锁失败: %w", err)
	}
	defer func() {
		// 锁在 session 级；即使 ctx 已取消也要尽力释放，否则连接还池后仍持锁。
		releaseCtx := context.WithoutCancel(ctx)
		if _, err := conn.Exec(releaseCtx, `SELECT pg_advisory_unlock(hashtext($1))`, AdvisoryLockName); err != nil {
			// 释放失败不改变本次迁移结论：连接随池关闭而断开，session 锁亦随之释放。
			_ = err
		}
	}()

	// ② search_path：迁移 SQL 里的非限定名（如 message_request）依赖它。
	if _, err := conn.Exec(ctx, `SET search_path TO public`); err != nil {
		return result, fmt.Errorf("migrate: 设置 search_path 失败: %w", err)
	}
	defer func() {
		// Node 的专用 client 用完即关，故不留残留；池连接会被复用，这里显式复原。
		_, _ = conn.Exec(context.WithoutCancel(ctx), `RESET search_path`)
	}()

	// ③ 账本建表。
	if err := ensureLedger(ctx, conn); err != nil {
		return result, err
	}

	// ④ created_at 自愈。
	repaired, err := repairCreatedAt(ctx, conn)
	if err != nil {
		return result, err
	}
	result.RepairedRows = repaired

	// ⑤ 迁移计划。
	baseReady, err := baseTablesExist(ctx, conn)
	if err != nil {
		return result, err
	}
	latest, err := latestLedgerRow(ctx, conn)
	if err != nil {
		return result, err
	}
	if latest != nil {
		if when, ok := latest.when(); ok {
			result.LatestBefore = &when
		}
	}

	// 计划门禁的水位取 MAX(created_at)（Node 的 getLatestDrizzleMigrationCreatedAt），
	// 与上面 drizzle 阈值那一行**不是同一个口径**。
	maxBefore, err := maxLedgerCreatedAt(ctx, conn)
	if err != nil {
		return result, err
	}

	plan := migrationPlan{
		baseTablesReady: baseReady,
		latestCreatedAt: maxBefore,
		migrate: func() (applied int, statements int, tags []string, err error) {
			return applyPending(ctx, conn, opts.Migrations)
		},
		runIndexPreflight: func(ensureColumns bool) error {
			return runSessionReplayIndexPreflight(ctx, conn, SESSION_REPLAY_INDEX_SPECS,
				IndexPreflightOptions{EnsureColumns: ensureColumns})
		},
		skipPreflight: opts.SkipSessionReplayPreflight,
	}
	applied, statements, tags, err := runMigrationPlan(ctx, &plan)
	if err != nil {
		return result, err
	}
	result.Applied = applied
	result.Statements = statements
	result.AppliedTags = tags
	result.PreflightRuns = plan.preflightRuns

	after, err := latestLedgerRow(ctx, conn)
	if err != nil {
		return result, err
	}
	if after != nil {
		if when, ok := after.when(); ok {
			result.LatestAfter = &when
		}
	}
	if maxAfter, err := maxLedgerCreatedAt(ctx, conn); err == nil {
		result.LatestMaxAfter = maxAfter
	}
	return result, nil
}

// applyPending 复刻 pg-core/dialect.js:60-73：一个事务里应用全部待迁移并登记账本。
//
// override 非 nil 时用它代替内嵌清单（仅测试注入用）。
func applyPending(ctx context.Context, conn *pgxpool.Conn, override []Migration) (int, int, []string, error) {
	migrations := override
	if migrations == nil {
		loaded, err := Load()
		if err != nil {
			return 0, 0, nil, err
		}
		migrations = loaded
	}
	last, err := latestLedgerRow(ctx, conn)
	if err != nil {
		return 0, 0, nil, err
	}

	// 阈值语义严格照抄 Node 的 `!lastDbMigration || Number(last.created_at) < when`：
	//   - 无行 → 全部应用；
	//   - created_at 为 NULL → Number(null) === 0 → 全部应用（when 恒为正）；
	//   - created_at 无法解析 → Node 得 NaN，比较恒假 → **一条都不应用**。
	//     本实现在这种情形直接报错而不是照抄 NaN 的静默跳过：numeric 列不会产生该取值，
	//     一旦出现说明账本被人改坏，静默不迁移比报错更难排查。
	threshold := int64(math.MinInt64)
	if last != nil {
		when, ok := last.when()
		if !ok {
			if last.CreatedAt == nil || strings.TrimSpace(*last.CreatedAt) == "" {
				threshold = 0
			} else {
				return 0, 0, nil, fmt.Errorf(
					"migrate: 账本水位不可解析(id=%d, created_at=%q)，拒绝在未知水位上迁移", last.ID, *last.CreatedAt)
			}
		} else {
			threshold = when
		}
	}

	pending := make([]Migration, 0, len(migrations))
	for _, m := range migrations {
		if threshold == math.MinInt64 || m.When > threshold {
			pending = append(pending, m)
		}
	}
	if len(pending) == 0 {
		return 0, 0, nil, nil
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("migrate: 开启迁移事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	statements := 0
	tags := make([]string, 0, len(pending))
	for _, m := range pending {
		for _, stmt := range m.Statements {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return 0, statements, tags, fmt.Errorf("migrate: 执行迁移 %s 失败: %w", m.Tag, err)
			}
			statements++
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO "drizzle"."__drizzle_migrations" ("hash", "created_at") VALUES ($1, $2)`,
			m.Hash, m.When); err != nil {
			return 0, statements, tags, fmt.Errorf("migrate: 登记迁移 %s 失败: %w", m.Tag, err)
		}
		tags = append(tags, m.Tag)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, statements, tags, fmt.Errorf("migrate: 提交迁移事务失败: %w", err)
	}
	return len(pending), statements, tags, nil
}

// StatusReport 是账本与 journal 的对账结果（只读，不建任何对象）。
type StatusReport struct {
	// TableExists 表示 drizzle.__drizzle_migrations 是否已存在。
	TableExists bool
	// Applied 是账本行数。
	Applied int
	// Latest 是账本水位（MAX(created_at)，journal when 口径）。
	Latest *int64
	// Pending 是尚待应用的迁移 tag（按 journal 顺序）。
	Pending []string
	// Mismatched 是 hash 已知但 created_at 与 journal 的 when 不一致的账本行 id
	// （Up 的自愈会修掉它们）。
	Mismatched []int32
	// Unknown 是账本里 hash 不在当前 journal 中的行 id（例如别的分支写入）。
	Unknown []int32
}

// UpToDate 表示没有待应用迁移。
func (s StatusReport) UpToDate() bool { return len(s.Pending) == 0 }

// Status 读取账本并与内嵌 journal 对账；不获取咨询锁、不写任何东西。
func Status(ctx context.Context, pool *pgxpool.Pool) (StatusReport, error) {
	var status StatusReport
	if pool == nil {
		return status, errors.New("migrate: 连接池为空")
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return status, fmt.Errorf("migrate: 取连接失败: %w", err)
	}
	defer conn.Release()

	var regclass *string
	if err := conn.QueryRow(ctx,
		`SELECT to_regclass('drizzle.__drizzle_migrations')::text`).Scan(&regclass); err != nil {
		return status, fmt.Errorf("migrate: 探测账本失败: %w", err)
	}
	if regclass == nil || *regclass == "" {
		migrations, err := Load()
		if err != nil {
			return status, err
		}
		for _, m := range migrations {
			status.Pending = append(status.Pending, m.Tag)
		}
		return status, nil
	}
	status.TableExists = true

	rows, err := ledgerRows(ctx, conn)
	if err != nil {
		return status, err
	}
	status.Applied = len(rows)

	expected, err := WhenByHash()
	if err != nil {
		return status, err
	}
	byHash := make(map[string]int64, len(expected))
	for hash, when := range expected {
		byHash[hash] = when
	}
	var highest int64
	haveHighest := false
	for _, row := range rows {
		want, known := expected[row.Hash]
		if !known {
			status.Unknown = append(status.Unknown, row.ID)
			continue
		}
		got, ok := row.when()
		if !ok || got != want {
			status.Mismatched = append(status.Mismatched, row.ID)
		}
		if ok && (!haveHighest || got > highest) {
			highest = got
			haveHighest = true
		}
	}
	if haveHighest {
		status.Latest = &highest
	}

	migrations, err := Load()
	if err != nil {
		return status, err
	}
	appliedHashes := make(map[string]bool, len(rows))
	for _, row := range rows {
		appliedHashes[row.Hash] = true
	}
	for _, m := range migrations {
		if !appliedHashes[m.Hash] {
			status.Pending = append(status.Pending, m.Tag)
		}
	}
	return status, nil
}
