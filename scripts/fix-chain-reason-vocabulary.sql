-- 链上 reason 词汇历史数据订正（Go 自造词 → Node 同词）。
--
-- 背景：Go 数据面曾写 `success`（Node 用 `request_success`）与
-- `non_retryable_client_error`（Node 用 `client_error_non_retryable`）。写入侧已在一
-- 修复，但**库里已有的行**仍是旧词。这些行会被按 Node 词表读的消费者判错：
--   · `provider_chain` 的链项**没有状态码字段**（`route.ChainItem`），公开状态投影只能按
--     reason 词分类 → 旧词落到「失败」兜底分支，每次成功都被算成失败。
--   · `fn_is_message_request_finalized` 的 reason 白名单同样只认 Node 词。
--
-- 为何选「迁移历史行」而不是「读取侧同时认两种词」：
--   1) 双认会让分类器与它要复刻的 Node 侧词表**永久分叉**，且分叉是隐形的（钉子只能钉
--      写入侧词表）。这类「读侧各自归一」正是本次缺陷的成因模式。
--   2) 影响行数有界（本地压测库 281 行；生产待测），且有备份表 → 可逐字节回滚。
--   3) 改写后，**按行重算**的投影（可用性 `avail_current` 那一类，走 SQL 从原始行聚合）
--      会自愈。
--
-- 已知无法由本迁移修复的部分（残余风险，须登记）：
--   · 公开状态页的 rollup 桶是**增量**写的（worker 只从桶构建 payload），已写入的错误
--     分类不会因为改行而回退 → 受影响时间窗内的可用率会偏低，直到窗口自然老化。
--
-- 用法（先看计数，再决定是否执行；生产环境请先备份库）：
--   psql "$DSN" -v ON_ERROR_STOP=1 -f scripts/fix-chain-reason-vocabulary.sql
--   # 只做计数、不改数据时：把下面 BEGIN 之后的 UPDATE 段注释掉
--
-- 回滚（同一文件末尾的 §回滚 段，或直接用备份表）：
--   psql "$DSN" -v ON_ERROR_STOP=1 -c "$(sed -n '/§回滚/,$p' scripts/fix-chain-reason-vocabulary.sql)"

\set ON_ERROR_STOP on

-- 备份表名带日期，便于同时存在多轮；重复执行同一轮不会重复备份（ON CONFLICT DO NOTHING）。
\set backup_table message_request_chain_reason_backup_20260913

-- ── 迁移前计数（留证据：把输出贴进报告） ─────────────────────────────────────
SELECT 'before: success' AS what, count(*) AS rows
FROM message_request WHERE provider_chain @> '[{"reason":"success"}]'
UNION ALL
SELECT 'before: non_retryable_client_error', count(*)
FROM message_request WHERE provider_chain @> '[{"reason":"non_retryable_client_error"}]'
UNION ALL
SELECT 'before: hedge_slot_saturated（信息性记录，改行时一并移除）', count(*)
FROM message_request WHERE provider_chain @> '[{"reason":"hedge_slot_saturated"}]';

BEGIN;

-- ── 备份受影响行（原值整列，回滚即还原） ─────────────────────────────────────
CREATE TABLE IF NOT EXISTS :"backup_table" (
  id            bigint PRIMARY KEY,
  provider_chain jsonb NOT NULL,
  backed_up_at  timestamptz NOT NULL DEFAULT now()
);

INSERT INTO :"backup_table" (id, provider_chain)
SELECT id, provider_chain
FROM message_request
WHERE provider_chain @> '[{"reason":"success"}]'
   OR provider_chain @> '[{"reason":"non_retryable_client_error"}]'
   OR provider_chain @> '[{"reason":"hedge_slot_saturated"}]'
ON CONFLICT (id) DO NOTHING;

-- ── 逐项改写（保持链序：ORDER BY ordinality 不可省） ─────────────────────────
-- 三条映射：
--   1. success                     → request_success
--   2. non_retryable_client_error  → client_error_non_retryable
--   3. hedge_slot_saturated        → 整项删除（Node 只把它记进 routing-trace，
--      provider_chain 里没有这个词；留在链上会被分类器判成失败）
WITH rewritten AS (
  SELECT m.id,
         COALESCE(
           (SELECT jsonb_agg(item ORDER BY ordinality)
            FROM (
              SELECT
                CASE
                  WHEN elem->>'reason' = 'success'
                    THEN jsonb_set(elem, '{reason}', '"request_success"')
                  WHEN elem->>'reason' = 'non_retryable_client_error'
                    THEN jsonb_set(elem, '{reason}', '"client_error_non_retryable"')
                  ELSE elem
                END AS item,
                ordinality
              FROM jsonb_array_elements(m.provider_chain) WITH ORDINALITY AS t(elem, ordinality)
              WHERE elem->>'reason' <> 'hedge_slot_saturated'
            ) items),
           '[]'::jsonb) AS provider_chain
  FROM message_request m
  WHERE m.provider_chain @> '[{"reason":"success"}]'
     OR m.provider_chain @> '[{"reason":"non_retryable_client_error"}]'
     OR m.provider_chain @> '[{"reason":"hedge_slot_saturated"}]'
)
UPDATE message_request m
SET provider_chain = r.provider_chain
FROM rewritten r
WHERE m.id = r.id;

COMMIT;

-- ── 迁移后计数（应为 0） ──────────────────────────────────────────────────────
SELECT 'after: success' AS what, count(*) AS rows
FROM message_request WHERE provider_chain @> '[{"reason":"success"}]'
UNION ALL
SELECT 'after: non_retryable_client_error', count(*)
FROM message_request WHERE provider_chain @> '[{"reason":"non_retryable_client_error"}]'
UNION ALL
SELECT 'after: hedge_slot_saturated', count(*)
FROM message_request WHERE provider_chain @> '[{"reason":"hedge_slot_saturated"}]'
UNION ALL
SELECT 'after: request_success（新的正确词，应大于 0）', count(*)
FROM message_request WHERE provider_chain @> '[{"reason":"request_success"}]';

-- ── §回滚：从备份表逐行还原（只还原本轮改写过的行） ──────────────────────────
--
-- BEGIN;
-- UPDATE message_request m
-- SET provider_chain = b.provider_chain
-- FROM message_request_chain_reason_backup_20260913 b
-- WHERE m.id = b.id;
-- COMMIT;
--
-- 确认无需回滚后，可清理备份表释放空间：
--   DROP TABLE message_request_chain_reason_backup_20260913;
