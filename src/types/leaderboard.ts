/**
 * leaderboard 域的共享类型。
 *
 * 2026-09 node 退役：这些声明原在被删掉的 Node 层（`src/repository/leaderboard.ts`）里，
 * 但 UI 与 api-client 需要它们，故迁移至此。**改动这里的形状即改契约**：
 * 请同步 Go 侧（`go/internal/adminapi/**`）与 `src/lib/api-client/**` 的解析。
 */

import type { BillingModelSource } from "@/types/system-config";
export interface LeaderboardEntry {
  userId: number;
  userName: string;
  totalRequests: number;
  totalCost: number;
  totalTokens: number;
  modelStats?: UserModelStat[];
}

/**
 * 排行榜周期类型
 */
export type LeaderboardPeriod = "daily" | "weekly" | "monthly" | "allTime" | "custom" | "last24h";

export interface ModelLeaderboardEntry {
  model: string;
  totalRequests: number;
  totalCost: number;
  totalTokens: number;
  successRate: number | null; // 0-1 之间的小数，UI 层负责格式化为百分比
  rowIdentityBasis?: BillingModelSource;
  successRateBasis?: BillingModelSource;
  costTokensBasis?: BillingModelSource;
  basisDisclosureRequired?: boolean;
  successRateUnavailableReason?: "no_countable_outcomes";
}

/**
 * 供应商排行榜条目类型
 */
export interface ProviderLeaderboardEntry {
  providerId: number;
  providerName: string;
  totalRequests: number;
  totalCost: number;
  totalTokens: number;
  successRate: number | null; // 0-1 之间的小数，UI 层负责格式化为百分比
  avgTtftMs: number; // 毫秒
  avgTokensPerSecond: number; // tok/s（仅统计流式且可计算的请求）
  avgCostPerRequest: number | null; // totalCost / totalRequests, null when totalRequests === 0
  avgCostPerMillionTokens: number | null; // totalCost * 1_000_000 / totalTokens, null when totalTokens === 0
  /** F3b 缓存系数（万分比定点值，effectivenessBp 汇总口径）；周期内无聚合数据时为 null */
  cacheCoefficientBp: number | null;
  /**
   * 可选：按模型拆分
   * - undefined: 未请求 includeModelStats
   * - []: 已请求 includeModelStats，但该 provider 下无可用模型统计
   */
  modelStats?: ModelProviderStat[];
}

/**
 * 自定义日期范围参数
 */
export interface DateRangeParams {
  startDate: string; // YYYY-MM-DD format
  endDate: string; // YYYY-MM-DD format
}

/**
 * 供应商缓存命中率 - 模型级统计
 */
export interface ModelCacheHitStat {
  model: string;
  totalRequests: number;
  cacheReadTokens: number;
  totalInputTokens: number;
  cacheHitRate: number; // 0-1
  /** 重定向模型口径的缓存系数；original 口径无法可靠映射时为 null */
  cacheCoefficientBp: number | null;
}

/**
 * 供应商消耗排行榜 - 模型级统计
 */
export interface ModelProviderStat {
  model: string;
  totalRequests: number;
  totalCost: number;
  totalTokens: number;
  successRate: number | null; // 0-1
  avgTtftMs: number; // 毫秒
  avgTokensPerSecond: number; // tok/s
  avgCostPerRequest: number | null;
  avgCostPerMillionTokens: number | null;
  /** 重定向模型口径的缓存系数；original 口径无法可靠映射时为 null */
  cacheCoefficientBp: number | null;
  rowIdentityBasis?: BillingModelSource;
  successRateBasis?: BillingModelSource;
  costTokensBasis?: BillingModelSource;
  basisDisclosureRequired?: boolean;
  successRateUnavailableReason?: "no_countable_outcomes";
}

/**
 * 供应商缓存命中率排行榜条目类型
 */
export interface ProviderCacheHitRateLeaderboardEntry {
  providerId: number;
  providerName: string;
  totalRequests: number;
  cacheReadTokens: number;
  totalCost: number;
  cacheCreationCost: number;
  /** Input tokens only (input + cacheCreation + cacheRead) for cache hit rate denominator */
  totalInputTokens: number;
  /** @deprecated Use totalInputTokens instead */
  totalTokens: number;
  cacheHitRate: number; // 0-1 之间的小数，UI 层负责格式化为百分比
  /** F3b 缓存系数（万分比定点值，effectivenessBp 汇总口径）；周期内无聚合数据时为 null */
  cacheCoefficientBp: number | null;
  modelStats: ModelCacheHitStat[];
}

/**
 * 模型排行榜条目类型
 */
export interface UserCacheHitModelStat {
  model: string | null;
  totalRequests: number;
  cacheReadTokens: number;
  totalInputTokens: number;
  cacheHitRate: number; // 0-1
}

export interface UserCacheHitRateLeaderboardEntry {
  userId: number;
  userName: string;
  totalRequests: number;
  cacheReadTokens: number;
  totalCost: number;
  cacheCreationCost: number;
  totalInputTokens: number;
  /** 为与现有缓存命中率榜单前端保持字段一致而保留；值始终等于 totalInputTokens */
  totalTokens: number;
  cacheHitRate: number; // 0-1
  /**
   * 可选：按模型拆分
   * - undefined: 未请求 includeModelStats
   * - []: 已请求 includeModelStats，但该用户下无可用模型统计
   */
  modelStats?: UserCacheHitModelStat[];
}

/**
 * 排行榜条目类型
 */
export interface UserModelStat {
  model: string | null;
  totalRequests: number;
  totalCost: number;
  totalTokens: number;
}
