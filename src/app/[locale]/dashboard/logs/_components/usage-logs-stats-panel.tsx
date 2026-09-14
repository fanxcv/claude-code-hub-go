"use client";

import { BarChart3 } from "lucide-react";
import { useTranslations } from "next-intl";
import { useCallback, useEffect, useState } from "react";
import { Skeleton } from "@/components/ui/skeleton";
import { getUsageLogsStats } from "@/lib/api-client/v1/actions/usage-logs";
import { cn, formatTokenAmount } from "@/lib/utils";
import type { CurrencyCode } from "@/lib/utils/currency";
import { formatCurrency } from "@/lib/utils/currency";
import type { UsageLogSummary } from "@/types/usage-logs";

interface UsageLogsStatsPanelProps {
  filters: {
    userId?: number;
    keyId?: number;
    providerId?: number;
    sessionId?: string;
    startTime?: number;
    endTime?: number;
    statusCode?: number;
    excludeStatusCode200?: boolean;
    model?: string;
    endpoint?: string;
    minRetryCount?: number;
    replayFilter?: "all" | "replay" | "non-replay";
  };
  currencyCode?: CurrencyCode;
  /**
   * 手动刷新计数器：每次递增都会触发一次统计汇总重新拉取（与列表的手动刷新联动）。
   * 仅在值变化时生效，不影响按 filters 变化的常规重拉。
   */
  refreshKey?: number;
  /**
   * 自动刷新间隔（毫秒）：`<= 0` 表示不自动刷新（仅手动/筛选变化时拉）。
   *
   * 为什么独立成参数、且**有意比列表稀疏**：统计单次服务端耗时 735–925ms
   * 若与列表同频（3s），每 tick 会多出近一秒的
   * 库查询，反而更慢。改前的行为是「只在手动刷新时更新」，用户看到的就是顶部数字不动。
   */
  autoRefreshIntervalMs?: number;
}

/**
 * Stats panel component with glass morphism UI
 * Always expanded (not collapsible), loads data asynchronously
 * Re-fetches when filters change or when refreshKey is bumped (manual refresh)
 */
export function UsageLogsStatsPanel({
  filters,
  currencyCode = "USD",
  refreshKey = 0,
  autoRefreshIntervalMs = 0,
}: UsageLogsStatsPanelProps) {
  const t = useTranslations("dashboard");
  const [stats, setStats] = useState<UsageLogSummary | null>(null);
  const [isLoading, setIsLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  // Create stable filter key for dependency comparison
  const filtersKey = JSON.stringify(filters);

  // Load stats data
  const loadStats = useCallback(async () => {
    setIsLoading(true);
    setError(null);

    try {
      const result = await getUsageLogsStats(filters);
      if (result.ok && result.data) {
        setStats(result.data);
      } else {
        setError(!result.ok ? result.error : t("logs.error.loadFailed"));
      }
    } catch (err) {
      console.error("Failed to load usage logs stats:", err);
      setError(t("logs.error.loadFailed"));
    } finally {
      setIsLoading(false);
    }
  }, [filters, t]);

  // Load data on mount, when filters change, and on manual refresh (refreshKey bump)
  // biome-ignore lint/correctness/useExhaustiveDependencies: filtersKey/refreshKey are the refetch triggers
  useEffect(() => {
    loadStats();
  }, [filtersKey, refreshKey, loadStats]);

  // 自动刷新：与列表联动但降频（间隔由调用方给出）。
  // biome-ignore lint/correctness/useExhaustiveDependencies: filtersKey/refreshKey 变化时的重拉由上面的 effect 负责，这里只挂定时器
  useEffect(() => {
    if (autoRefreshIntervalMs <= 0) return;
    const timer = setInterval(() => void loadStats(), autoRefreshIntervalMs);
    return () => clearInterval(timer);
  }, [autoRefreshIntervalMs, filtersKey, refreshKey]);

  return (
    <div
      className={cn(
        // Glass morphism base
        "relative overflow-hidden rounded-xl border bg-card/30 backdrop-blur-sm",
        "transition-all duration-200",
        "border-border/50 hover:border-border"
      )}
    >
      {/* Glassmorphism gradient overlay */}
      <div className="absolute inset-0 bg-gradient-to-br from-white/[0.02] to-transparent pointer-events-none" />

      <div className="relative z-10">
        {/* Header */}
        <div className="flex items-center gap-3 px-4 py-3 border-b border-border/30">
          <span
            className={cn(
              "flex items-center justify-center w-8 h-8 rounded-lg shrink-0",
              "bg-muted text-muted-foreground"
            )}
          >
            <BarChart3 className="h-4 w-4" />
          </span>
          <div className="space-y-0.5">
            <h3 className="text-sm font-semibold text-foreground leading-none">
              {t("logs.stats.title")}
            </h3>
            <p className="text-xs text-muted-foreground leading-relaxed hidden sm:block">
              {t("logs.stats.description")}
            </p>
          </div>
        </div>

        {/* Content */}
        <div className="px-4 py-4">
          {isLoading ? (
            <StatsSkeletons />
          ) : error ? (
            <div className="text-center py-4 text-destructive">{error}</div>
          ) : stats ? (
            <StatsContent stats={stats} currencyCode={currencyCode} />
          ) : null}
        </div>
      </div>
    </div>
  );
}

/**
 * Stats data skeletons
 */
function StatsSkeletons() {
  return (
    <div className="grid gap-4 md:grid-cols-4">
      {[1, 2, 3, 4].map((i) => (
        <div key={i} className="space-y-2 p-4 border border-border/50 rounded-lg bg-card/20">
          <Skeleton className="h-4 w-24" />
          <Skeleton className="h-8 w-32" />
        </div>
      ))}
    </div>
  );
}

/**
 * Stats data content
 */
function StatsContent({
  stats,
  currencyCode,
}: {
  stats: UsageLogSummary;
  currencyCode: CurrencyCode;
}) {
  const t = useTranslations("dashboard");

  return (
    <div className="grid gap-4 md:grid-cols-4">
      {/* Total Requests */}
      <div className="p-4 border border-border/50 rounded-lg bg-card/20">
        <div className="text-sm text-muted-foreground mb-1">{t("logs.stats.totalRequests")}</div>
        <div className="text-2xl font-mono font-semibold">
          {stats.totalRequests.toLocaleString()}
        </div>
      </div>

      {/* Total Amount */}
      <div className="p-4 border border-border/50 rounded-lg bg-card/20">
        <div className="text-sm text-muted-foreground mb-1">{t("logs.stats.totalAmount")}</div>
        <div className="text-2xl font-mono font-semibold">
          {formatCurrency(stats.totalCost, currencyCode)}
        </div>
      </div>

      {/* Total Tokens */}
      <div className="p-4 border border-border/50 rounded-lg bg-card/20">
        <div className="text-sm text-muted-foreground mb-1">{t("logs.stats.totalTokens")}</div>
        <div className="text-2xl font-mono font-semibold">
          {formatTokenAmount(stats.totalTokens)}
        </div>
        <div className="mt-2 text-xs text-muted-foreground space-y-1">
          <div className="flex justify-between">
            <span>{t("logs.stats.input")}:</span>
            <span className="font-mono">{formatTokenAmount(stats.totalInputTokens)}</span>
          </div>
          <div className="flex justify-between">
            <span>{t("logs.stats.output")}:</span>
            <span className="font-mono">{formatTokenAmount(stats.totalOutputTokens)}</span>
          </div>
        </div>
      </div>

      {/* Cache Tokens */}
      <div className="p-4 border border-border/50 rounded-lg bg-card/20">
        <div className="text-sm text-muted-foreground mb-1">{t("logs.stats.cacheTokens")}</div>
        <div className="text-2xl font-mono font-semibold">
          {formatTokenAmount(stats.totalCacheCreationTokens + stats.totalCacheReadTokens)}
        </div>
        <div className="mt-2 text-xs text-muted-foreground space-y-1">
          <div className="flex justify-between">
            <span>{t("logs.stats.write")}:</span>
            <span className="font-mono">{formatTokenAmount(stats.totalCacheCreationTokens)}</span>
          </div>
          <div className="flex justify-between">
            <span>{t("logs.stats.read")}:</span>
            <span className="font-mono">{formatTokenAmount(stats.totalCacheReadTokens)}</span>
          </div>
        </div>
      </div>
    </div>
  );
}
