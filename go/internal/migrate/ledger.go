package migrate

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// LedgerSchema / LedgerTable 是账本的位置（Node 默认值，见 pg-core/dialect.js:45-46）。
const (
	LedgerSchema = "drizzle"
	LedgerTable  = "__drizzle_migrations"
)

// querier 是账本操作需要的连接能力；pgxpool.Pool、pgxpool.Conn 与 pgx.Conn 都满足。
type querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// ledgerRow 是账本一行的原始形态。
//
// created_at 是 numeric 列，这里统一按文本取回再自行解析（Node 侧 postgres-js 也返回字符串，
// 再由 `Number(...)` 转换，见 pg-core/dialect.js:57）。走 text 而非 ::bigint 是为了
// 不做任何隐式取整：非整数取值会解析失败并被当作「无法判定」，而不是被悄悄截断。
type ledgerRow struct {
	ID        int32
	Hash      string
	CreatedAt *string
}

func (r ledgerRow) when() (int64, bool) {
	if r.CreatedAt == nil {
		return 0, false
	}
	text := strings.TrimSpace(*r.CreatedAt)
	if text == "" {
		return 0, false
	}
	value, err := strconv.ParseInt(text, 10, 64)
	if err == nil {
		return value, true
	}
	// numeric 可能带小数点（例如 "1761057311271.000"）：仅在整数部分可解析时采用。
	if idx := strings.IndexByte(text, '.'); idx > 0 {
		if value, err := strconv.ParseInt(text[:idx], 10, 64); err == nil {
			return value, true
		}
	}
	return 0, false
}

// ensureLedger 建 schema 与账本表（幂等）。
//
// SQL 逐字等同 src/lib/migrate.ts:71-78：created_at 是 **numeric**。
// 顺序也一致：先 CREATE SCHEMA，再 CREATE TABLE。
func ensureLedger(ctx context.Context, q querier) error {
	if _, err := q.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS "drizzle"`); err != nil {
		return fmt.Errorf("migrate: 建 schema drizzle 失败: %w", err)
	}
	const createTable = `
    CREATE TABLE IF NOT EXISTS "drizzle"."__drizzle_migrations" (
      id SERIAL PRIMARY KEY,
      hash text NOT NULL,
      created_at numeric
    )
  `
	if _, err := q.Exec(ctx, createTable); err != nil {
		return fmt.Errorf("migrate: 建账本表失败: %w", err)
	}
	return nil
}

// ledgerRows 读回账本全部行（按 created_at 升序，NULL 排最后）。
func ledgerRows(ctx context.Context, q querier) ([]ledgerRow, error) {
	rows, err := q.Query(ctx,
		`SELECT id, hash, created_at::text FROM "drizzle"."__drizzle_migrations" ORDER BY created_at ASC NULLS LAST, id ASC`)
	if err != nil {
		return nil, fmt.Errorf("migrate: 读账本失败: %w", err)
	}
	defer rows.Close()

	var out []ledgerRow
	for rows.Next() {
		var row ledgerRow
		if err := rows.Scan(&row.ID, &row.Hash, &row.CreatedAt); err != nil {
			return nil, fmt.Errorf("migrate: 解析账本行失败: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migrate: 遍历账本失败: %w", err)
	}
	return out, nil
}

// latestLedgerRow 复刻 pg-core/dialect.js:55-57 的
// `select id, hash, created_at ... order by created_at desc limit 1`。
//
// 注意语义细节：drizzle 只比较**这一行的 created_at**，且该值在迁移循环开始前取一次、
// 循环中不再更新。故 journal 里若出现 when 非单调的条目，它会被跳过——本实现照此复刻，
// 不改成「滚动取最大值」。
func latestLedgerRow(ctx context.Context, q querier) (*ledgerRow, error) {
	row := q.QueryRow(ctx,
		`SELECT id, hash, created_at::text FROM "drizzle"."__drizzle_migrations" ORDER BY created_at DESC LIMIT 1`)
	var out ledgerRow
	if err := row.Scan(&out.ID, &out.Hash, &out.CreatedAt); err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("migrate: 读账本水位失败: %w", err)
	}
	return &out, nil
}

// maxLedgerCreatedAt 复刻 src/lib/migrate.ts:176-187 的
// `SELECT MAX(created_at) FROM "drizzle"."__drizzle_migrations"`。
//
// 它与 latestLedgerRow 口径**不同且不可互换**：
//   - drizzle 的迁移阈值（applyPending）取 `order by created_at desc limit 1` 那一行；
//     PostgreSQL 在 DESC 下把 NULL 排在最前，故该行可能是 created_at 为 NULL 的行；
//   - 迁移计划的 preflight 门禁取 MAX(created_at)（**忽略** NULL）。
//
// 两者只在「账本里存在 NULL/不可解析的 created_at」时分歧，本函数保证了那一种情形下
// 也与 Node 同判（不跑首次 preflight）。返回 nil 表示无行或全部为 NULL。
func maxLedgerCreatedAt(ctx context.Context, q querier) (*int64, error) {
	var text *string
	if err := q.QueryRow(ctx,
		`SELECT MAX(created_at)::text FROM "drizzle"."__drizzle_migrations"`).Scan(&text); err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("migrate: 读账本最大水位失败: %w", err)
	}
	if text == nil {
		return nil, nil
	}
	row := ledgerRow{CreatedAt: text}
	when, ok := row.when()
	if !ok {
		return nil, nil
	}
	return &when, nil
}

// repairCreatedAt 复刻 src/lib/migrate.ts:81-147 的 repairDrizzleMigrationsCreatedAt。
//
// 为什么需要：drizzle 只按 created_at 判定「已应用」。历史 journal 的 when 若被修正过，
// 旧实例会因 created_at 偏大而永久跳过后续迁移。自愈按 **hash** 对齐 created_at，
// 让升级对用户无感。hash 不在 journal 里的行（例如别的分支写入）保持原样。
func repairCreatedAt(ctx context.Context, q querier) (int, error) {
	expected, err := WhenByHash()
	if err != nil {
		return 0, err
	}
	rows, err := ledgerRows(ctx, q)
	if err != nil {
		return 0, err
	}

	type fix struct {
		id    int32
		value int64
	}
	var pending []fix
	for _, row := range rows {
		want, ok := expected[row.Hash]
		if !ok {
			continue
		}
		got, ok := row.when()
		if ok && got == want {
			continue
		}
		pending = append(pending, fix{id: row.ID, value: want})
	}
	for _, f := range pending {
		if _, err := q.Exec(ctx,
			`UPDATE "drizzle"."__drizzle_migrations" SET created_at = $1 WHERE id = $2`, f.value, f.id); err != nil {
			return 0, fmt.Errorf("migrate: 修复账本 created_at 失败(id=%d): %w", f.id, err)
		}
	}
	return len(pending), nil
}

// baseTablesExist 复刻 src/lib/migrate.ts:190-197：两张基表是否都已存在。
func baseTablesExist(ctx context.Context, q querier) (bool, error) {
	var messageRequest, usageLedger bool
	err := q.QueryRow(ctx, `
		SELECT
		  to_regclass('public.message_request') IS NOT NULL AS message_request_exists,
		  to_regclass('public.usage_ledger') IS NOT NULL AS usage_ledger_exists`).
		Scan(&messageRequest, &usageLedger)
	if err != nil {
		return false, fmt.Errorf("migrate: 探测基表失败: %w", err)
	}
	return messageRequest && usageLedger, nil
}
