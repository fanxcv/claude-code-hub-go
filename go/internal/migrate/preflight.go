package migrate

import (
	"context"
	"fmt"
)

// 常量与规格逐字来自 src/lib/migrations/session-replay-index-preflight.ts:1-5。
//
// 这些 when 值是**账本水位口径**的里程碑，用来决定「是否需要先跑一次并发建索引 preflight」：
// 老库（水位低于 1785688550789）在迁移正文跑之前先补列 + 并发建索引，避免迁移正文里的
// 非并发建索引把老库的大表长时间锁住。
const (
	// SessionReplayMigrationCreatedAt 对应 SESSION_REPLAY_MIGRATION_CREATED_AT。
	SessionReplayMigrationCreatedAt int64 = 1785563419224
	// SessionIdentityIndexMigrationCreatedAt 对应 SESSION_IDENTITY_INDEX_MIGRATION_CREATED_AT。
	SessionIdentityIndexMigrationCreatedAt int64 = 1785635169798
	// DatabaseTimeoutIndexMigrationCreatedAt 对应 DATABASE_TIMEOUT_INDEX_MIGRATION_CREATED_AT，
	// 也是迁移计划里唯一被用作判据的里程碑。
	DatabaseTimeoutIndexMigrationCreatedAt int64 = 1785688550789
	// SessionReplayIndexMarker 对应 SESSION_REPLAY_INDEX_MARKER（索引注释，用作「已按本规格建过」的凭据）。
	SessionReplayIndexMarker = "cch:migration:0116:session-replay-index:v1"
	// DatabaseTimeoutIndexMarker 对应 DATABASE_TIMEOUT_INDEX_MARKER。
	DatabaseTimeoutIndexMarker = "cch:migration:0118:database-timeout-index:v2"
)

// IndexSpec 对应 SessionReplayIndexSpec：一条待建的索引及其临时名与标记。
type IndexSpec struct {
	// CanonicalName 是最终索引名。
	CanonicalName string
	// TemporaryName 是并发建索引时的临时名（建成后 RENAME 为 CanonicalName）。
	TemporaryName string
	// Marker 是写在索引注释里的凭据，用来区分「按本规格建过」与「同名但形态不同」。
	Marker string
	// Definition 是 `CREATE INDEX CONCURRENTLY "<临时名>" ` 之后的整段定义（含 ON ... WHERE ...）。
	Definition string
}

// SESSION_REPLAY_INDEX_SPECS 逐条对应 TS 侧的 SESSION_REPLAY_INDEX_SPECS，顺序一致。
//
// 顺序有意义：先建 0118 组（5 条）再建 0116/0117 组（8 条），与 Node 相同。
var SESSION_REPLAY_INDEX_SPECS = []IndexSpec{
	{
		CanonicalName: "idx_message_request_session_identity_created_at",
		TemporaryName: "cch_0118_tmp_01",
		Marker:        DatabaseTimeoutIndexMarker,
		Definition:    `ON "public"."message_request" USING btree (COALESCE("session_identity", "session_id"),"created_at" DESC NULLS LAST,"id" DESC NULLS LAST) WHERE "message_request"."deleted_at" IS NULL`,
	},
	{
		CanonicalName: "idx_usage_ledger_session_identity_created_at",
		TemporaryName: "cch_0118_tmp_02",
		Marker:        DatabaseTimeoutIndexMarker,
		Definition:    `ON "public"."usage_ledger" USING btree (COALESCE("session_identity", "session_id"),"user_id","created_at" DESC NULLS LAST) WHERE "usage_ledger"."blocked_by" IS NULL AND "usage_ledger"."is_replay" = false`,
	},
	{
		CanonicalName: "idx_message_request_proxy_status_active",
		TemporaryName: "cch_0118_tmp_03",
		Marker:        DatabaseTimeoutIndexMarker,
		Definition:    `ON "public"."message_request" USING btree ("created_at" DESC NULLS LAST,"user_id") WHERE "message_request"."deleted_at" IS NULL AND "message_request"."is_replay" = false AND "message_request"."status_code" IS NULL AND ("message_request"."blocked_by" IS NULL OR "message_request"."blocked_by" <> 'warmup')`,
	},
	{
		CanonicalName: "idx_message_request_proxy_status_latest",
		TemporaryName: "cch_0118_tmp_04",
		Marker:        DatabaseTimeoutIndexMarker,
		Definition:    `ON "public"."message_request" USING btree ("user_id","updated_at" DESC NULLS LAST,"id" DESC NULLS LAST) WHERE "message_request"."deleted_at" IS NULL AND "message_request"."is_replay" = false AND "message_request"."status_code" IS NOT NULL AND ("message_request"."blocked_by" IS NULL OR "message_request"."blocked_by" <> 'warmup')`,
	},
	{
		CanonicalName: "idx_usage_ledger_user_id_reset",
		TemporaryName: "cch_0118_tmp_05",
		Marker:        DatabaseTimeoutIndexMarker,
		Definition:    `ON "public"."usage_ledger" USING btree ("user_id")`,
	},
	{
		CanonicalName: "idx_usage_ledger_session_identity",
		TemporaryName: "cch_0117_tmp_01",
		Marker:        SessionReplayIndexMarker,
		Definition:    `ON "usage_ledger" USING btree (COALESCE("session_identity", "session_id"))`,
	},
	{
		CanonicalName: "idx_usage_ledger_user_created_at",
		TemporaryName: "cch_0116_tmp_03",
		Marker:        SessionReplayIndexMarker,
		Definition:    `ON "usage_ledger" USING btree ("user_id","created_at") WHERE "usage_ledger"."blocked_by" IS NULL AND "usage_ledger"."is_replay" = false`,
	},
	{
		CanonicalName: "idx_usage_ledger_key_created_at",
		TemporaryName: "cch_0116_tmp_04",
		Marker:        SessionReplayIndexMarker,
		Definition:    `ON "usage_ledger" USING btree ("key","created_at") WHERE "usage_ledger"."blocked_by" IS NULL AND "usage_ledger"."is_replay" = false`,
	},
	{
		CanonicalName: "idx_usage_ledger_provider_created_at",
		TemporaryName: "cch_0116_tmp_05",
		Marker:        SessionReplayIndexMarker,
		Definition:    `ON "usage_ledger" USING btree ("final_provider_id","created_at") WHERE "usage_ledger"."blocked_by" IS NULL AND "usage_ledger"."is_replay" = false`,
	},
	{
		CanonicalName: "idx_usage_ledger_key_cost",
		TemporaryName: "cch_0116_tmp_06",
		Marker:        SessionReplayIndexMarker,
		Definition:    `ON "usage_ledger" USING btree ("key","created_at","cost_usd","endpoint") WHERE "usage_ledger"."blocked_by" IS NULL AND "usage_ledger"."is_replay" = false`,
	},
	{
		CanonicalName: "idx_usage_ledger_user_cost_cover",
		TemporaryName: "cch_0116_tmp_07",
		Marker:        SessionReplayIndexMarker,
		Definition:    `ON "usage_ledger" USING btree ("user_id","created_at","cost_usd","endpoint") WHERE "usage_ledger"."blocked_by" IS NULL AND "usage_ledger"."is_replay" = false`,
	},
	{
		CanonicalName: "idx_usage_ledger_provider_cost_cover",
		TemporaryName: "cch_0116_tmp_08",
		Marker:        SessionReplayIndexMarker,
		Definition:    `ON "usage_ledger" USING btree ("final_provider_id","created_at","cost_usd","endpoint") WHERE "usage_ledger"."blocked_by" IS NULL AND "usage_ledger"."is_replay" = false`,
	},
	{
		CanonicalName: "idx_usage_ledger_key_created_at_desc_cover",
		TemporaryName: "cch_0116_tmp_09",
		Marker:        SessionReplayIndexMarker,
		Definition:    `ON "usage_ledger" USING btree ("key","created_at" DESC NULLS LAST,"final_provider_id") WHERE "usage_ledger"."blocked_by" IS NULL AND "usage_ledger"."is_replay" = false`,
	},
}

// IndexState 对应 MigrationIndexState。
type IndexState struct {
	Exists bool
	Valid  bool
	// Marker 是索引注释；未注释时为 nil。
	Marker *string
}

// IndexPreflightOptions 对应 runSessionReplayIndexPreflight 的 options。
type IndexPreflightOptions struct {
	// EnsureColumns 为真时先补四列（0116/0118 依赖 session_identity 与 is_replay）。
	EnsureColumns bool
}

func isValidatedIndex(state IndexState, marker string) bool {
	return state.Exists && state.Valid && state.Marker != nil && *state.Marker == marker
}

// inspectIndex 复刻 src/lib/migrate.ts:170-188 的 inspectIndex。
//
// 判定三要素：对象是否存在（to_regclass）、是否为**有效**索引（pg_index.indisvalid）、
// 以及注释是否等于本规格的 marker。只有三者齐备才认为「已按本规格建好」——
// 这使并发建索引被中断（invalid index 残留）时能自愈重来。
func inspectIndex(ctx context.Context, q querier, name string) (IndexState, error) {
	qualified := `public."` + name + `"`
	var state IndexState
	var marker *string
	err := q.QueryRow(ctx, `
		SELECT
		  c.oid IS NOT NULL AS exists,
		  COALESCE(i.indisvalid, false) AS valid,
		  obj_description(c.oid, 'pg_class') AS marker
		FROM (SELECT to_regclass($1) AS oid) resolved
		LEFT JOIN pg_class c ON c.oid = resolved.oid
		LEFT JOIN pg_index i ON i.indexrelid = c.oid`, qualified).
		Scan(&state.Exists, &state.Valid, &marker)
	if err != nil {
		return state, fmt.Errorf("migrate: 探测索引 %s 失败: %w", name, err)
	}
	state.Marker = marker
	return state, nil
}

// ensurePreflightColumns 复刻 session-replay-index-preflight.ts:133-147 的 ensurePreflightColumns。
//
// 四句 ALTER 作为**一次** Exec 发出：Node 侧是 `client.unsafe(sql)`（simple query），
// PostgreSQL 在 simple query 里把多句放进同一个隐式事务，故四句要么全成要么全不成；
// 本实现保持同样的一次性，不拆成四次调用。
//
// lock_timeout 设为 5s：老库的大表上 ALTER 可能等锁，5s 内拿不到就失败重试（由运维重启触发），
// 好过无限期挂在启动路径上。
func ensurePreflightColumns(ctx context.Context, q querier) error {
	if _, err := q.Exec(ctx, "SET lock_timeout = '5s'"); err != nil {
		return fmt.Errorf("migrate: 设置 lock_timeout 失败: %w", err)
	}
	defer func() {
		_, _ = q.Exec(context.WithoutCancel(ctx), "RESET lock_timeout")
	}()
	const addColumns = `ALTER TABLE "message_request"
  ADD COLUMN IF NOT EXISTS "session_identity" varchar(64);
ALTER TABLE "message_request"
  ADD COLUMN IF NOT EXISTS "is_replay" boolean DEFAULT false NOT NULL;
ALTER TABLE "usage_ledger"
  ADD COLUMN IF NOT EXISTS "session_identity" varchar(64);
ALTER TABLE "usage_ledger"
  ADD COLUMN IF NOT EXISTS "is_replay" boolean DEFAULT false NOT NULL`
	if _, err := q.Exec(ctx, addColumns); err != nil {
		return fmt.Errorf("migrate: 补 preflight 列失败: %w", err)
	}
	return nil
}

// runSessionReplayIndexPreflight 复刻 session-replay-index-preflight.ts:137-178。
//
// 每条规格的处理：
//   - 规范名已存在且注释等于 marker → 只清理可能残留的临时索引，跳过；
//   - 否则确保临时索引存在且有效（不存在则 CREATE INDEX CONCURRENTLY + COMMENT，
//     并复检；无效则先 DROP CONCURRENTLY 再重建）；
//   - 有旧规范名索引则先 DROP CONCURRENTLY，再把临时名 RENAME 成规范名；
//   - 复检规范名；不合格即报错（不回退、不静默）。
//
// CONCURRENTLY 不能跑在事务里：本函数全部语句都在自动提交模式下执行。
func runSessionReplayIndexPreflight(
	ctx context.Context,
	q querier,
	specs []IndexSpec,
	options IndexPreflightOptions,
) error {
	if options.EnsureColumns {
		if err := ensurePreflightColumns(ctx, q); err != nil {
			return err
		}
	}

	for _, spec := range specs {
		canonical, err := inspectIndex(ctx, q, spec.CanonicalName)
		if err != nil {
			return err
		}
		if isValidatedIndex(canonical, spec.Marker) {
			stale, err := inspectIndex(ctx, q, spec.TemporaryName)
			if err != nil {
				return err
			}
			if stale.Exists {
				if _, err := q.Exec(ctx, fmt.Sprintf(`DROP INDEX CONCURRENTLY IF EXISTS "%s"`, spec.TemporaryName)); err != nil {
					return fmt.Errorf("migrate: 清理残留临时索引 %s 失败: %w", spec.TemporaryName, err)
				}
			}
			continue
		}

		temporary, err := inspectIndex(ctx, q, spec.TemporaryName)
		if err != nil {
			return err
		}
		if !isValidatedIndex(temporary, spec.Marker) {
			if temporary.Exists {
				if _, err := q.Exec(ctx, fmt.Sprintf(`DROP INDEX CONCURRENTLY IF EXISTS "%s"`, spec.TemporaryName)); err != nil {
					return fmt.Errorf("migrate: 丢弃无效临时索引 %s 失败: %w", spec.TemporaryName, err)
				}
			}
			if _, err := q.Exec(ctx, fmt.Sprintf(`CREATE INDEX CONCURRENTLY "%s" %s`, spec.TemporaryName, spec.Definition)); err != nil {
				return fmt.Errorf("migrate: 并发建索引 %s 失败: %w", spec.TemporaryName, err)
			}
			if _, err := q.Exec(ctx, fmt.Sprintf(`COMMENT ON INDEX "public"."%s" IS '%s'`, spec.TemporaryName, spec.Marker)); err != nil {
				return fmt.Errorf("migrate: 注释索引 %s 失败: %w", spec.TemporaryName, err)
			}
			temporary, err = inspectIndex(ctx, q, spec.TemporaryName)
			if err != nil {
				return err
			}
			if !isValidatedIndex(temporary, spec.Marker) {
				return fmt.Errorf("migrate: 并发建索引产出无效索引: %s", spec.TemporaryName)
			}
		}

		if canonical.Exists {
			if _, err := q.Exec(ctx, fmt.Sprintf(`DROP INDEX CONCURRENTLY IF EXISTS "public"."%s"`, spec.CanonicalName)); err != nil {
				return fmt.Errorf("migrate: 丢弃旧索引 %s 失败: %w", spec.CanonicalName, err)
			}
		}
		if _, err := q.Exec(ctx, fmt.Sprintf(`ALTER INDEX "public"."%s" RENAME TO "%s"`, spec.TemporaryName, spec.CanonicalName)); err != nil {
			return fmt.Errorf("migrate: 重命名索引 %s → %s 失败: %w", spec.TemporaryName, spec.CanonicalName, err)
		}

		replaced, err := inspectIndex(ctx, q, spec.CanonicalName)
		if err != nil {
			return err
		}
		if !isValidatedIndex(replaced, spec.Marker) {
			return fmt.Errorf("migrate: 并发 preflight 未能装好索引: %s", spec.CanonicalName)
		}
	}
	return nil
}

// migrationPlan 对应 runSessionReplayMigrationPlan 的入参。
type migrationPlan struct {
	baseTablesReady   bool
	latestCreatedAt   *int64
	migrate           func() (applied int, statements int, tags []string, err error)
	runIndexPreflight func(ensureColumns bool) error
	// skipPreflight 仅测试用。
	skipPreflight bool
	// preflightRuns 由 runMigrationPlan 回填，供调用方观测。
	preflightRuns int
}

// runMigrationPlan 复刻 session-replay-index-preflight.ts:180-206 的执行序。
//
// 取指针而不是值：preflightRuns 是回填给调用方的观测值，按值传会让它永远停在 0
// （实测踩过：Result.PreflightRuns 恒为 0，看上去像「preflight 没跑」）。
func runMigrationPlan(
	ctx context.Context,
	plan *migrationPlan,
) (int, int, []string, error) {
	if !plan.skipPreflight && plan.baseTablesReady &&
		(plan.latestCreatedAt == nil || *plan.latestCreatedAt < DatabaseTimeoutIndexMigrationCreatedAt) {
		if err := plan.runIndexPreflight(true); err != nil {
			return 0, 0, nil, err
		}
		plan.preflightRuns++
	}

	applied, statements, tags, err := plan.migrate()
	if err != nil {
		return 0, statements, tags, err
	}

	if !plan.skipPreflight {
		if err := plan.runIndexPreflight(false); err != nil {
			return 0, statements, tags, err
		}
		plan.preflightRuns++
	}
	return applied, statements, tags, nil
}
