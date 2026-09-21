-- 日期: 2026-09-21
-- 功能: 亲和拆成两个独立开关——总数闸（affinity_enabled）+ 模式开关（沿用 affinity_ignore_client_session_id）
--
-- 为什么加列：原实现把 affinity_ignore_client_session_id **一字段两用**——既当「亲和总开关」
-- （cmd/cchd/affinity.go 的 `env || settings`）又当「忽略会话 ID / 强制前缀」。翻它的默认值会
-- 连带把整套亲和关掉，而保持 true 又无法让会话粘性生效，两个诉求不可兼得。
-- 拆开后：affinity_enabled 管总闸（默认开），affinity_ignore_client_session_id 只管模式
-- （true = 强制前缀粘性，false = 会话优先、前缀兜底）。
--
-- 第二条 UPDATE 是**确定性**的数据迁移：存量行原本是 true（前缀时代默认），而本特性上线后
-- 期望「会话粘性生效」，故把存量行显式置 false。只改默认值（见 0131）管不到存量行，
-- 必须 UPDATE；否则生产会继续跑旧的前缀粘性，改了代码等于没改。
--
-- 可回滚：
--   ALTER TABLE "system_settings" ALTER COLUMN "affinity_enabled" DROP DEFAULT;
--   ALTER TABLE "system_settings" DROP COLUMN "affinity_enabled";
--   UPDATE "system_settings" SET "affinity_ignore_client_session_id" = true;
ALTER TABLE "system_settings" ADD COLUMN "affinity_enabled" boolean DEFAULT true NOT NULL;--> statement-breakpoint
UPDATE "system_settings" SET "affinity_ignore_client_session_id" = false WHERE "affinity_ignore_client_session_id" = true;