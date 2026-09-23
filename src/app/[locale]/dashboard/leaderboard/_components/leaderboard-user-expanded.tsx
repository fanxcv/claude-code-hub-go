"use client";

import { useQuery } from "@tanstack/react-query";
import { ChevronDown, ChevronRight } from "lucide-react";
import { useTimeZone, useTranslations } from "next-intl";
import { type ReactNode, useState } from "react";
import { Skeleton } from "@/components/ui/skeleton";
import { TableCell, TableRow } from "@/components/ui/table";
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  getUserInsightsModelBreakdown,
  getUserInsightsProviderBreakdown,
} from "@/lib/api-client/v1/actions/admin-user-insights";
import { formatTokenAmount } from "@/lib/utils";
import { cacheHitRateColorClass, cacheHitRatePercent } from "@/lib/utils/cache-hit-rate";
import { type CurrencyCode, formatCurrency } from "@/lib/utils/currency";
import type { LeaderboardPeriod } from "@/types/leaderboard";
import { getDateRangeForPeriod } from "./date-range-picker";

interface BreakdownEntry {
  name: string | null;
  requests: number;
  cost: number;
  inputTokens: number;
  outputTokens: number;
  cacheCreationTokens: number;
  cacheReadTokens: number;
}

interface ProviderBreakdownEntry extends BreakdownEntry {
  providerId: number;
}

interface LeaderboardUserExpandedProps {
  userId: number;
  period: LeaderboardPeriod;
  dateRange?: { startDate: string; endDate: string };
  /** 主表列数（含排名列），展开行逐列对齐用 */
  columnCount: number;
}

// 排行榜 period → 用户洞察接口的闭区间日期。
//
// 与排行榜服务端条件同口径（本地日历日的 DATE_TRUNC）：daily/weekly/monthly 复用 date-range-picker
// 的既有换算；custom 直接取所选区间；allTime 不传日期（服务端无日期条件，等价 1=1）。
function resolvePeriodDates(
  period: LeaderboardPeriod,
  dateRange: LeaderboardUserExpandedProps["dateRange"],
  timeZone: string
): { startDate?: string; endDate?: string } {
  if (period === "custom") {
    return { startDate: dateRange?.startDate, endDate: dateRange?.endDate };
  }
  if (period === "allTime") {
    return {};
  }
  // last24h 不在 URL 白名单里（UI 不可达），与 date-range-picker 的回落一致按 daily 处理。
  if (period === "last24h") {
    return getDateRangeForPeriod("daily", timeZone);
  }
  return getDateRangeForPeriod(period, timeZone);
}

function toBreakdownEntry(item: {
  model: string | null;
  requests: number;
  cost: number;
  inputTokens: number;
  outputTokens: number;
  cacheCreationTokens: number;
  cacheReadTokens: number;
}): BreakdownEntry {
  return {
    name: item.model,
    requests: item.requests,
    cost: item.cost,
    inputTokens: item.inputTokens,
    outputTokens: item.outputTokens,
    cacheCreationTokens: item.cacheCreationTokens,
    cacheReadTokens: item.cacheReadTokens,
  };
}

// 名称列之后的四格：请求数 / Token 数（含缓存命中率）/ 消耗金额。
function BreakdownCells({
  entry,
  currencyCode,
  indentClass,
  fallbackName,
}: {
  entry: BreakdownEntry;
  currencyCode: CurrencyCode;
  indentClass: string;
  fallbackName: string;
}) {
  const totalAllTokens =
    entry.inputTokens + entry.outputTokens + entry.cacheCreationTokens + entry.cacheReadTokens;
  const cacheHitRate = cacheHitRatePercent(entry);

  return (
    <>
      <TableCell>
        <div className={indentClass}>
          <span className="font-mono text-sm">{entry.name || fallbackName}</span>
        </div>
      </TableCell>
      <TableCell className="text-right">{entry.requests.toLocaleString()}</TableCell>
      <TableCell className="text-right">
        {formatTokenAmount(totalAllTokens)}
        {" · "}
        <span className={cacheHitRateColorClass(cacheHitRate)}>{cacheHitRate.toFixed(1)}%</span>
      </TableCell>
      <TableCell className="text-right font-mono font-bold">
        {formatCurrency(entry.cost, currencyCode)}
      </TableCell>
    </>
  );
}

function StateRow({ columnCount, children }: { columnCount: number; children: ReactNode }) {
  return (
    <TableRow className="bg-muted/30 hover:bg-muted/30">
      <TableCell colSpan={columnCount}>{children}</TableCell>
    </TableRow>
  );
}

function ModelRows({
  entries,
  currencyCode,
  indentClass,
  testId,
  fallbackName,
}: {
  entries: BreakdownEntry[];
  currencyCode: CurrencyCode;
  indentClass: string;
  testId: string;
  fallbackName: string;
}) {
  return entries.map((entry, index) => (
    <TableRow
      key={`${entry.name ?? "unknown"}-${index}`}
      className="bg-muted/30 hover:bg-muted/30"
      data-testid={testId}
    >
      <TableCell>
        <div className="flex items-center gap-1">
          <div className="h-4 w-4" />
        </div>
      </TableCell>
      <BreakdownCells
        entry={entry}
        currencyCode={currencyCode}
        indentClass={indentClass}
        fallbackName={fallbackName}
      />
    </TableRow>
  ));
}

interface ProviderModelsProps {
  userId: number;
  providerId: number;
  startDate?: string;
  endDate?: string;
  columnCount: number;
}

// 只在供应商行展开时挂载，故请求天然懒加载。
function ProviderModels({
  userId,
  providerId,
  startDate,
  endDate,
  columnCount,
}: ProviderModelsProps) {
  const t = useTranslations("dashboard.leaderboard.userInsights");
  const query = useQuery({
    queryKey: ["leaderboard-provider-models", userId, providerId, startDate, endDate],
    queryFn: async () => {
      const result = await getUserInsightsModelBreakdown(userId, startDate, endDate, {
        providerId,
      });
      if (!result.ok) throw new Error(result.error);
      return result.data;
    },
  });

  if (query.isLoading) {
    return (
      <StateRow columnCount={columnCount}>
        <Skeleton className="h-4 w-full" />
      </StateRow>
    );
  }
  if (query.isError) {
    return (
      <StateRow columnCount={columnCount}>
        <span className="text-sm text-destructive">{t("loadError")}</span>
      </StateRow>
    );
  }

  const entries = (query.data?.breakdown ?? []).map(toBreakdownEntry);
  if (entries.length === 0) {
    return (
      <StateRow columnCount={columnCount}>
        <span className="text-sm text-muted-foreground">{t("noData")}</span>
      </StateRow>
    );
  }

  return (
    <ModelRows
      entries={entries}
      currencyCode={(query.data?.currencyCode ?? "USD") as CurrencyCode}
      indentClass="pl-12"
      testId={`leaderboard-provider-models-${providerId}`}
      fallbackName={t("unknownModel")}
    />
  );
}

interface ProviderBreakdownRowProps {
  entry: ProviderBreakdownEntry;
  currencyCode: CurrencyCode;
  userId: number;
  startDate?: string;
  endDate?: string;
  columnCount: number;
  fallbackName: string;
}

function ProviderBreakdownRow({
  entry,
  currencyCode,
  userId,
  startDate,
  endDate,
  columnCount,
  fallbackName,
}: ProviderBreakdownRowProps) {
  const t = useTranslations("dashboard.leaderboard");
  const [open, setOpen] = useState(false);

  return (
    <>
      <TableRow
        className="bg-muted/30 hover:bg-muted/30"
        data-testid="leaderboard-user-expanded-provider-list"
      >
        <TableCell>
          <div className="flex items-center gap-1">
            <button
              type="button"
              aria-expanded={open}
              aria-label={open ? t("collapseModelStats") : t("expandModelStats")}
              className="inline-flex cursor-pointer items-center rounded-sm focus:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
              onClick={() => setOpen((prev) => !prev)}
            >
              {open ? (
                <ChevronDown className="h-4 w-4 shrink-0 text-muted-foreground" />
              ) : (
                <ChevronRight className="h-4 w-4 shrink-0 text-muted-foreground" />
              )}
            </button>
          </div>
        </TableCell>
        <BreakdownCells
          entry={entry}
          currencyCode={currencyCode}
          indentClass="pl-6"
          fallbackName={fallbackName}
        />
      </TableRow>
      {open ? (
        <ProviderModels
          userId={userId}
          providerId={entry.providerId}
          startDate={startDate}
          endDate={endDate}
          columnCount={columnCount}
        />
      ) : null}
    </>
  );
}

// 用户视图行内展开区：供应商聚合（默认）与模型聚合两个 tab，行与主表逐列对齐。
//
// 两个 tab 同源同指标集——都取自 usage_ledger 的两条 breakdown 聚合（requests / cost /
// inputTokens / outputTokens / cacheCreationTokens / cacheReadTokens），故可横向对账；
// 排行榜接口的 modelStats 指标集不同（无缓存 token 拆分），不用于此处。
export function LeaderboardUserExpanded({
  userId,
  period,
  dateRange,
  columnCount,
}: LeaderboardUserExpandedProps) {
  const t = useTranslations("dashboard.leaderboard.userInsights");
  const timeZone = useTimeZone() ?? "UTC";
  const [tab, setTab] = useState("provider");

  const { startDate, endDate } = resolvePeriodDates(period, dateRange, timeZone);

  const providerQuery = useQuery({
    queryKey: ["leaderboard-user-provider-breakdown", userId, startDate, endDate],
    queryFn: async () => {
      const result = await getUserInsightsProviderBreakdown(userId, startDate, endDate);
      if (!result.ok) throw new Error(result.error);
      return result.data;
    },
  });

  const modelQuery = useQuery({
    queryKey: ["leaderboard-user-model-breakdown", userId, startDate, endDate],
    queryFn: async () => {
      const result = await getUserInsightsModelBreakdown(userId, startDate, endDate);
      if (!result.ok) throw new Error(result.error);
      return result.data;
    },
    enabled: tab === "model",
  });

  const currencyCode = (providerQuery.data?.currencyCode ?? "USD") as CurrencyCode;
  const modelEntries = (modelQuery.data?.breakdown ?? []).map(toBreakdownEntry);
  const providerEntries: ProviderBreakdownEntry[] = (providerQuery.data?.breakdown ?? []).map(
    (item) => ({
      providerId: item.providerId as number,
      name: item.providerName,
      requests: item.requests,
      cost: item.cost,
      inputTokens: item.inputTokens,
      outputTokens: item.outputTokens,
      cacheCreationTokens: item.cacheCreationTokens,
      cacheReadTokens: item.cacheReadTokens,
    })
  );

  return (
    <>
      <TableRow>
        <TableCell colSpan={columnCount} className="p-3">
          <Tabs value={tab} onValueChange={setTab}>
            <TabsList>
              <TabsTrigger value="provider" data-testid="leaderboard-user-expanded-provider-tab">
                {t("providerBreakdown")}
              </TabsTrigger>
              <TabsTrigger value="model" data-testid="leaderboard-user-expanded-model-tab">
                {t("modelBreakdown")}
              </TabsTrigger>
            </TabsList>
          </Tabs>
        </TableCell>
      </TableRow>
      {tab === "provider" ? (
        providerQuery.isLoading ? (
          <StateRow columnCount={columnCount}>
            <Skeleton className="h-16 w-full" />
          </StateRow>
        ) : providerQuery.isError ? (
          <StateRow columnCount={columnCount}>
            <span className="text-sm text-destructive">{t("loadError")}</span>
          </StateRow>
        ) : providerEntries.length === 0 ? (
          <StateRow columnCount={columnCount}>
            <span className="text-sm text-muted-foreground">{t("noData")}</span>
          </StateRow>
        ) : (
          providerEntries.map((entry) => (
            <ProviderBreakdownRow
              key={entry.providerId}
              entry={entry}
              currencyCode={currencyCode}
              userId={userId}
              startDate={startDate}
              endDate={endDate}
              columnCount={columnCount}
              fallbackName={t("unknownProvider")}
            />
          ))
        )
      ) : modelQuery.isLoading ? (
        <StateRow columnCount={columnCount}>
          <Skeleton className="h-16 w-full" />
        </StateRow>
      ) : modelQuery.isError ? (
        <StateRow columnCount={columnCount}>
          <span className="text-sm text-destructive">{t("loadError")}</span>
        </StateRow>
      ) : modelEntries.length === 0 ? (
        <StateRow columnCount={columnCount}>
          <span className="text-sm text-muted-foreground">{t("noData")}</span>
        </StateRow>
      ) : (
        <ModelRows
          entries={modelEntries}
          currencyCode={(modelQuery.data?.currencyCode ?? "USD") as CurrencyCode}
          indentClass="pl-6"
          testId="leaderboard-user-expanded-model-list"
          fallbackName={t("unknownModel")}
        />
      )}
    </>
  );
}
