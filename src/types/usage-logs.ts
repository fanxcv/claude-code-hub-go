/**
 * usage-logs 域的共享类型。
 *
 * 2026-09 node 退役：这些声明原在被删掉的 Node 层（`src/repository/usage-logs.ts`、`src/actions/usage-logs.ts`）里，
 * 但 UI 与 api-client 需要它们，故迁移至此。**改动这里的形状即改契约**：
 * 请同步 Go 侧（`go/internal/adminapi/**`）与 `src/lib/api-client/**` 的解析。
 */

import type { RequestCacheMetricAvailability } from "@/lib/cache-effectiveness/request-metrics";
import type { HedgeLoserBilling, StoredCostBreakdown } from "@/types/cost-breakdown";
import type { ProviderChainItem } from "@/types/message";
import type { RoutingTraceV1 } from "@/types/routing-trace";
import type { SpecialSetting } from "@/types/special-settings";
export interface UsageLogRow {
  id: number;
  createdAt: Date | null;
  sessionId: string | null; // Public Session identity
  sourceSessionId: string | null; // Physical Session source for request-scoped readback
  sourceSessionIds?: string[]; // All physical client session IDs grouped under the public identity
  sessionIdentityKind?: "session_id" | "prefix_affinity" | null;
  requestSequence: number | null; // Request Sequence（Session 内请求序号）
  userName: string;
  keyName: string;
  providerName: string | null; // 改为可选：被拦截的请求没有 provider
  model: string | null;
  originalModel: string | null; // 原始模型（重定向前）
  actualResponseModel: string | null; // 上游响应实际返回的模型名(audit)
  endpoint: string | null;
  statusCode: number | null;
  inputTokens: number | null;
  outputTokens: number | null;
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
  totalTokens: number;
  costUsd: string | null;
  costMultiplier: string | null; // 供应商倍率
  groupCostMultiplier: string | null; // 分组倍率
  costBreakdown: StoredCostBreakdown | null; // 费用明细
  hedgeLosers: HedgeLoserBilling[] | null; // 竞速输家计费明细（费用已计入 costUsd 总额）
  durationMs: number | null;
  ttftMs: number | null;
  firstByteMs: number | null;
  errorMessage: string | null;
  providerChain: ProviderChainItem[] | null;
  routingTrace?: RoutingTraceV1 | null;
  blockedBy: string | null; // 拦截类型（如 'sensitive_word'）
  blockedReason: string | null; // 拦截原因（JSON 字符串）
  isReplay: boolean;
  replaySourceRequestId: number | null;
  userAgent: string | null; // User-Agent（客户端信息）
  clientIp: string | null; // 客户端 IP（IPv4/IPv6）
  messagesCount: number | null; // Messages 数量
  context1mApplied: boolean | null; // 是否应用了1M上下文窗口
  swapCacheTtlApplied: boolean | null; // 是否启用了swap cache TTL billing
  specialSettings: SpecialSetting[] | null; // 特殊设置（审计/展示）
  _liveChain?: {
    chain: ProviderChainItem[];
    phase: string;
    updatedAt: number;
    activeProviders?: Array<{ id: number; name: string }>;
    routingTrace?: RoutingTraceV1 | null;
  } | null;
  anthropicEffort?: string | null;
}

export interface UsageLogSummary {
  totalRequests: number;
  totalCost: number;
  totalTokens: number;
  totalInputTokens: number;
  totalOutputTokens: number;
  totalCacheCreationTokens: number;
  totalCacheReadTokens: number;
  totalCacheCreation5mTokens: number;
  totalCacheCreation1hTokens: number;
}

/**
 * Cursor-based pagination result (no total count, optimized for large datasets)
 */
export interface UsageLogsBatchResult {
  logs: UsageLogRow[];
  sourceSessionIdsByIdentity?: Record<string, string[]>;
  nextCursor: { createdAt: string; id: number } | null;
  hasMore: boolean;
}

export interface UsageLogsExportStatus {
  jobId: string;
  status: "queued" | "running" | "completed" | "failed";
  processedRows: number;
  totalRows: number;
  progressPercent: number;
  format: UsageLogsExportFormat;
  error?: string;
}

export type UsageLogsExportFormat = "csv" | "xlsx";
