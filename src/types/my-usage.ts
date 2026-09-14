/**
 * my-usage 域的共享类型。
 *
 * 2026-09 node 退役：这些声明原在被删掉的 Node 层（`src/actions/my-usage.ts`）里，
 * 但 UI 与 api-client 需要它们，故迁移至此。**改动这里的形状即改契约**：
 * 请同步 Go 侧（`go/internal/adminapi/**`）与 `src/lib/api-client/**` 的解析。
 */

import type { RequestCacheMetricAvailability } from "@/lib/cache-effectiveness/request-metrics";
import type { CurrencyCode } from "@/lib/utils";
import type { BillingModelSource } from "@/types/system-config";
import type { UsageLogSummary } from "@/types/usage-logs";
export interface MyStatsSummary extends UsageLogSummary {
  keyModelBreakdown: ModelBreakdownItem[];
  userModelBreakdown: ModelBreakdownItem[];
  currencyCode: CurrencyCode;
}

export interface MyTodayStats {
  calls: number;
  inputTokens: number;
  outputTokens: number;
  costUsd: number;
  modelBreakdown: Array<{
    model: string | null;
    billingModel: string | null;
    calls: number;
    costUsd: number;
    inputTokens: number;
    outputTokens: number;
  }>;
  currencyCode: CurrencyCode;
  billingModelSource: BillingModelSource;
}

export interface MyUsageLogEntry {
  id: number;
  createdAt: Date | null;
  model: string | null;
  billingModel: string | null;
  anthropicEffort?: string | null;
  modelRedirect: string | null;
  inputTokens: number;
  outputTokens: number;
  cost: number;
  statusCode: number | null;
  duration: number | null;
  endpoint: string | null;
  cacheCreationInputTokens: number | null;
  cacheReadInputTokens: number | null;
  cacheCreation5mInputTokens: number | null;
  cacheCreation1hInputTokens: number | null;
  cacheTtlApplied: string | null;
  theoreticalCacheTokens: number | null;
  cacheScoreEligible: boolean | null;
  cacheScoreExcludedReason: string | null;
  cacheInputTotal: number;
  actualCacheRate: number | null;
  theoreticalCacheRate: number | null;
  requestCacheCoefficientBp: number | null;
  requestCacheMetricAvailability: RequestCacheMetricAvailability;
}

export interface MyUsageLogsBatchResult {
  logs: MyUsageLogEntry[];
  nextCursor: { createdAt: string; id: number } | null;
  hasMore: boolean;
  currencyCode: CurrencyCode;
  billingModelSource: BillingModelSource;
}

export interface MyUsageMetadata {
  keyName: string;
  keyProviderGroup: string | null;
  keyExpiresAt: Date | null;
  keyIsEnabled: boolean;
  userName: string;
  userProviderGroup: string | null;
  userExpiresAt: Date | null;
  userIsEnabled: boolean;
  dailyResetMode: "fixed" | "rolling";
  dailyResetTime: string;
  currencyCode: CurrencyCode;
  billingModelSource: BillingModelSource;
}

export interface MyUsageQuota {
  keyLimit5hUsd: number | null;
  keyLimitDailyUsd: number | null;
  keyLimitWeeklyUsd: number | null;
  keyLimitMonthlyUsd: number | null;
  keyLimitTotalUsd: number | null;
  keyLimitConcurrentSessions: number;
  keyCurrent5hUsd: number;
  keyCurrentDailyUsd: number;
  keyCurrentWeeklyUsd: number;
  keyCurrentMonthlyUsd: number;
  keyCurrentTotalUsd: number;
  keyCurrentConcurrentSessions: number;

  userLimit5hUsd: number | null;
  userLimitWeeklyUsd: number | null;
  userLimitMonthlyUsd: number | null;
  userLimitTotalUsd: number | null;
  userLimitConcurrentSessions: number | null;
  userRpmLimit: number | null;
  userCurrent5hUsd: number;
  userCurrentDailyUsd: number;
  userCurrentWeeklyUsd: number;
  userCurrentMonthlyUsd: number;
  userCurrentTotalUsd: number;
  userCurrentConcurrentSessions: number;

  userLimitDailyUsd: number | null;
  userExpiresAt: Date | null;
  userProviderGroup: string | null;
  userName: string;
  userIsEnabled: boolean;

  keyProviderGroup: string | null;
  keyName: string;
  keyIsEnabled: boolean;

  userAllowedModels: string[];
  userAllowedClients: string[];

  expiresAt: Date | null;
  dailyResetMode: "fixed" | "rolling";
  dailyResetTime: string;
}

export interface ModelBreakdownItem {
  model: string | null;
  requests: number;
  cost: number;
  inputTokens: number;
  outputTokens: number;
  cacheCreationTokens: number;
  cacheReadTokens: number;
  cacheCreation5mTokens: number;
  cacheCreation1hTokens: number;
}
