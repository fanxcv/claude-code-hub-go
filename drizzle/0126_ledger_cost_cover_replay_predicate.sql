-- Add `is_replay = false` to the three usage_ledger SUM(cost_usd) covering indexes so the
-- LEDGER_BILLING_CONDITION filter stays index-only.
--
-- Why: every consumer of these indexes filters `is_replay = false` (store/ledger.go:
-- BillingCondition). 0103 added `endpoint` so the non-billing-endpoint filter could be answered
-- from the index, but left `is_replay` out -- and a filter that is missing from the index turns
-- the index-only scan into a heap fetch, so the planner stops choosing the covering index at all.
-- Measured on 2.05M rows / 272MB (same DB, same data, only the index shape differs):
--   old shape: Bitmap Heap Scan on idx_usage_ledger_user_created_at -- per-request quota sum 62ms
--   new shape: Index Only Scan using idx_usage_ledger_user_cost_cover, Heap Fetches: 0 -- 26ms
--
-- The index preflight spec (SESSION_REPLAY_INDEX_SPECS entries cch_0116_tmp_06..08,
-- session-replay-index-preflight.ts) and 0118's session-identity index already carry
-- `is_replay = false`; the migration DDL never did. The preflight only runs below its watermark
-- milestone, so a database that was already past it when the preflight shipped keeps the old
-- shape forever -- fresh installs and old installs disagree about the same logical index. This
-- migration closes that drift for the three indexes whose shape actually changes planner behavior.
--
-- usage_ledger is a high-write table, and the migrator runs ALL pending statements of a batch in
-- ONE transaction (migrate/up.go: applyPending), so the DROP INDEX below (ACCESS EXCLUSIVE on the
-- table) is held until commit while the three CREATE INDEX statements run (SHARE, which blocks
-- writes). Measured cost of this migration on 2.05M rows / 272MB: drops 3-10ms, builds 1.0s /
-- 1.8s / 2.5s. To rebuild WITHOUT blocking (recommended when the table is large), run the
-- following BEFORE this migration (psql, outside a transaction), once per index -- e.g. for
-- idx_usage_ledger_user_cost_cover:
--   DROP INDEX CONCURRENTLY IF EXISTS "idx_usage_ledger_user_cost_cover";
--   CREATE INDEX CONCURRENTLY "idx_usage_ledger_user_cost_cover"
--     ON "usage_ledger" ("user_id","created_at","cost_usd","endpoint")
--     WHERE "blocked_by" IS NULL AND "is_replay" = false;
-- The guarded blocks below detect the already-fixed shape -- and invalid leftovers from an
-- interrupted concurrent build -- through pg_index/pg_get_indexdef, so they become a no-op.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_index i
    JOIN pg_class c ON c.oid = i.indexrelid
    WHERE c.relname = 'idx_usage_ledger_user_cost_cover'
      AND i.indisvalid
      AND pg_get_indexdef(i.indexrelid) LIKE '%is_replay%'
  ) THEN
    DROP INDEX IF EXISTS "idx_usage_ledger_user_cost_cover";
    CREATE INDEX IF NOT EXISTS "idx_usage_ledger_user_cost_cover" ON "usage_ledger" USING btree ("user_id","created_at","cost_usd","endpoint") WHERE "usage_ledger"."blocked_by" IS NULL AND "usage_ledger"."is_replay" = false;
  END IF;

  IF NOT EXISTS (
    SELECT 1 FROM pg_index i
    JOIN pg_class c ON c.oid = i.indexrelid
    WHERE c.relname = 'idx_usage_ledger_provider_cost_cover'
      AND i.indisvalid
      AND pg_get_indexdef(i.indexrelid) LIKE '%is_replay%'
  ) THEN
    DROP INDEX IF EXISTS "idx_usage_ledger_provider_cost_cover";
    CREATE INDEX IF NOT EXISTS "idx_usage_ledger_provider_cost_cover" ON "usage_ledger" USING btree ("final_provider_id","created_at","cost_usd","endpoint") WHERE "usage_ledger"."blocked_by" IS NULL AND "usage_ledger"."is_replay" = false;
  END IF;

  IF NOT EXISTS (
    SELECT 1 FROM pg_index i
    JOIN pg_class c ON c.oid = i.indexrelid
    WHERE c.relname = 'idx_usage_ledger_key_cost'
      AND i.indisvalid
      AND pg_get_indexdef(i.indexrelid) LIKE '%is_replay%'
  ) THEN
    DROP INDEX IF EXISTS "idx_usage_ledger_key_cost";
    CREATE INDEX IF NOT EXISTS "idx_usage_ledger_key_cost" ON "usage_ledger" USING btree ("key","created_at","cost_usd","endpoint") WHERE "usage_ledger"."blocked_by" IS NULL AND "usage_ledger"."is_replay" = false;
  END IF;
END $$;