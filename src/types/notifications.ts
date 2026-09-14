/**
 * notifications 域的共享类型。
 *
 * 2026-09 node 退役：这些声明原在被删掉的 Node 层（`src/repository/notifications.ts`）里，
 * 但 UI 与 api-client 需要它们，故迁移至此。**改动这里的形状即改契约**：
 * 请同步 Go 侧（`go/internal/adminapi/**`）与 `src/lib/api-client/**` 的解析。
 */

import type { CacheHitRateAlertSettingsWindowMode } from "@/lib/webhook/types";
/**
 * 通知设置类型
 */
export interface NotificationSettings {
  id: number;
  enabled: boolean;
  useLegacyMode: boolean;

  // 熔断器告警配置
  circuitBreakerEnabled: boolean;
  circuitBreakerWebhook: string | null;

  // 每日排行榜配置
  dailyLeaderboardEnabled: boolean;
  dailyLeaderboardWebhook: string | null;
  dailyLeaderboardTime: string | null;
  dailyLeaderboardTopN: number | null;

  // 成本预警配置
  costAlertEnabled: boolean;
  costAlertWebhook: string | null;
  costAlertThreshold: string | null; // numeric 类型作为 string
  costAlertCheckInterval: number | null;

  // 缓存命中率异常告警配置（provider × model）
  cacheHitRateAlertEnabled: boolean;
  cacheHitRateAlertWebhook: string | null;
  cacheHitRateAlertWindowMode: CacheHitRateAlertSettingsWindowMode | null;
  cacheHitRateAlertCheckInterval: number | null;
  cacheHitRateAlertHistoricalLookbackDays: number | null;
  cacheHitRateAlertMinEligibleRequests: number | null;
  cacheHitRateAlertMinEligibleTokens: number | null;
  cacheHitRateAlertAbsMin: string | null; // numeric 类型作为 string
  cacheHitRateAlertDropRel: string | null; // numeric 类型作为 string
  cacheHitRateAlertDropAbs: string | null; // numeric 类型作为 string
  cacheHitRateAlertCooldownMinutes: number | null;
  cacheHitRateAlertTopN: number | null;

  createdAt: Date;
  updatedAt: Date;
}

/**
 * 通知类型（绑定维度的枚举）。
 *
 * 原定义在 `src/repository/notification-bindings.ts`，Node 退役时该文件已删，
 * 而 `src/lib/webhook/migration.ts`（UI 迁移向导用）仍需此类型，故按原值补入 `src/types/`。
 */
export type NotificationType =
  | "circuit_breaker"
  | "daily_leaderboard"
  | "cost_alert"
  | "cache_hit_rate_alert";
