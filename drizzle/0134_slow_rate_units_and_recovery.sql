-- 日期: 2026-09-22
-- 功能: 低速监控参数改单位（分钟/天/0-1 小数）并新增恢复策略阈值
--
-- 为什么：用户 2026-09-22 裁决三处单位——判定滑窗由**秒**改为**分钟**（默认 30）、
-- 基线主窗由**秒**改为**天**（默认 3）、低速系数由**千分比整数**改为 **0-1 小数**
-- （默认 0.3）；并新增恢复策略阈值 slow_rate_recovery_requests（默认 10，见
-- slowrate/recorder.go）与恢复策略本身。
--
-- 为何另立新列而不是改旧列语义（用户裁决：新列名 + REST 字段名沿旧）：
--   1. 旧列名带 `_seconds` / `_per_mille` 后缀，改语义而不改名会让「列名与语义永久不符」
--      ——正是 0133 删 slow_rate_probe_min_tokens 要消掉的那种无声旋钮；
--   2. 纯 ADD 可回滚：旧列仍在，代码回退一版即恢复旧语义（旧值未被覆盖）；
--      改旧列语义则不可逆（本仓无 down 迁移机制，见 migrate/up.go）。
--
-- 已废弃，勿读，保留以保回滚（Go 侧结构体字段已删，任何新代码不得再读这三列）：
--   providers.slow_rate_window_seconds          -> slow_rate_window_minutes（值 ÷ 60）
--   providers.slow_rate_baseline_window_seconds -> slow_rate_baseline_window_days（值 ÷ 86400）
--   providers.slow_rate_ratio_per_mille         -> slow_rate_ratio（值 ÷ 1000）
-- 存量行不做回填：新列留 NULL 即取代码默认（30 分钟 / 3 天 / 0.3），旧列保持用户原值不动。
--
-- 回滚：ALTER TABLE "providers" DROP COLUMN "slow_rate_window_minutes",
--         DROP COLUMN "slow_rate_baseline_window_days", DROP COLUMN "slow_rate_ratio",
--         DROP COLUMN "slow_rate_recovery_requests";
ALTER TABLE "providers" ADD COLUMN "slow_rate_window_minutes" integer;--> statement-breakpoint
ALTER TABLE "providers" ADD COLUMN "slow_rate_baseline_window_days" integer;--> statement-breakpoint
ALTER TABLE "providers" ADD COLUMN "slow_rate_ratio" numeric(5,4);--> statement-breakpoint
ALTER TABLE "providers" ADD COLUMN "slow_rate_recovery_requests" integer;
