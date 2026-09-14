// Package migrate 是 Node 侧迁移链路的 Go 等价物。
//
// 为什么需要它：Node 归档后 schema 仍要能演进。Node 侧的迁移不是「直接调 drizzle」，
// 而是 `src/lib/migrate.ts:123 runMigrations()` 的一整套流程，本包逐段复刻：
//
//	① 咨询锁     `SELECT pg_advisory_lock(hashtext('claude-code-hub:migrations'))`
//	             （src/lib/migrate.ts:16,136,160）
//	② search_path `SET search_path TO public`（src/lib/migrate.ts:138）
//	③ 账本建表   `CREATE SCHEMA/TABLE IF NOT EXISTS drizzle.__drizzle_migrations`
//	             `(id SERIAL PRIMARY KEY, hash text NOT NULL, created_at numeric)`
//	             （src/lib/migrate.ts:68-79；注意 created_at 是 **numeric**，
//	             drizzle 自己的建表语句写的是 bigint，但应用先建表、IF NOT EXISTS 使然）
//	④ created_at 自愈  `repairDrizzleMigrationsCreatedAt`（src/lib/migrate.ts:81-147）：
//	             账本行 hash 能在 journal 里找到时，把 created_at 对齐回 journal 的 when
//	⑤ 迁移计划   `runSessionReplayMigrationPlan`（src/lib/migrations/
//	             session-replay-index-preflight.ts:180-206）：baseTablesReady 且
//	             账本水位 < 1785688550789 时先跑一次「补列 + 并发建索引」preflight，
//	             然后 `migrate()`，最后再跑一次 preflight（ensureColumns=false）
//	⑥ drizzle 本体（node_modules/drizzle-orm/migrator.js:3-30 +
//	             pg-core/dialect.js:44-…）：
//	             读 meta/_journal.json → 逐条读 `${tag}.sql` → 按字面量
//	             `--> statement-breakpoint` 字符串切分（不 trim）→
//	             hash = sha256(整份文件 utf8 内容) 的十六进制 →
//	             取账本 `ORDER BY created_at DESC LIMIT 1`，对
//	             `when > 该行 created_at` 的迁移在**一个事务**里逐条执行并登记
//	             `(hash, created_at=when)`
//	⑦ 失败语义   `catch { logger.error; process.exit(1) }`（src/lib/migrate.ts:155-157）
//
// 与 Node 的**互认**是本包的核心约束：账本表、列、hash 算法、created_at 取值、
// 咨询锁名字逐字段同义，故 Go 迁完 Node 认为已是最新，反之亦然。
//
// 迁移正文不在此包书写：真源是仓库根的 `drizzle/`，本包持有它的**逐字节副本**
// （`sql/`），原因是 go:embed 只能嵌入本包目录之下的文件，而模块根在 go/，
// 够不到模块外的 drizzle/。副本漂移由 embed_sync_test.go 与 ../../../drizzle 逐字节比对兜住
// ——与 `go/internal/ratelimit/lua/` 对仓库根 `lua/` 的成法一致。
package migrate
