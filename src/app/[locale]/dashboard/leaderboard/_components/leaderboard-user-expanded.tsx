"use client";

import { useQuery } from "@tanstack/react-query";
import { ChevronDown, ChevronRight } from "lucide-react";
import { useTimeZone, useTranslations } from "next-intl";
import { useState } from "react";
import {
  ModelBreakdownColumn,
  type ModelBreakdownItem,
  type ModelBreakdownLabels,
  ModelBreakdownRow,
} from "@/components/analytics/model-breakdown-column";
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";
import { Skeleton } from "@/components/ui/skeleton";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import {
  getUserInsightsModelBreakdown,
  getUserInsightsProviderBreakdown,
} from "@/lib/api-client/v1/actions/admin-user-insights";
import type { CurrencyCode } from "@/lib/utils/currency";
import type { LeaderboardPeriod } from "@/types/leaderboard";
import { getDateRangeForPeriod } from "./date-range-picker";

type Translate = (key: string) => string;
type ProviderBreakdownItem = ModelBreakdownItem & { providerId: number };

interface LeaderboardUserExpandedProps {
  userId: number;
  period: LeaderboardPeriod;
  dateRange?: { startDate: string; endDate: string };
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

function buildLabels(tStats: Translate, unknownName: string): ModelBreakdownLabels {
  return {
    unknownModel: unknownName,
    modal: {
      requests: tStats("modal.requests"),
      cost: tStats("modal.cost"),
      inputTokens: tStats("modal.inputTokens"),
      outputTokens: tStats("modal.outputTokens"),
      cacheCreationTokens: tStats("modal.cacheWrite"),
      cacheReadTokens: tStats("modal.cacheRead"),
      totalTokens: tStats("modal.totalTokens"),
      costPercentage: tStats("modal.cost"),
      cacheHitRate: tStats("modal.cacheHitRate"),
      cacheTokens: tStats("modal.cacheTokens"),
      performanceHigh: tStats("modal.performanceHigh"),
      performanceMedium: tStats("modal.performanceMedium"),
      performanceLow: tStats("modal.performanceLow"),
    },
  };
}

function toBreakdownItem(item: {
  model: string | null;
  requests: number;
  cost: number;
  inputTokens: number;
  outputTokens: number;
  cacheCreationTokens: number;
  cacheReadTokens: number;
}): ModelBreakdownItem {
  return {
    model: item.model,
    requests: item.requests,
    cost: item.cost,
    inputTokens: item.inputTokens,
    outputTokens: item.outputTokens,
    cacheCreationTokens: item.cacheCreationTokens,
    cacheReadTokens: item.cacheReadTokens,
  };
}

interface BreakdownListProps {
  isLoading: boolean;
  isError: boolean;
  items: ModelBreakdownItem[];
  currencyCode: CurrencyCode;
  labels: ModelBreakdownLabels;
  keyPrefix: string;
  testId: string;
}

function BreakdownList({
  isLoading,
  isError,
  items,
  currencyCode,
  labels,
  keyPrefix,
  testId,
}: BreakdownListProps) {
  const t = useTranslations("dashboard.leaderboard.userInsights");

  if (isLoading) {
    return <Skeleton className="h-16 w-full" />;
  }
  if (isError) {
    return <div className="text-sm text-destructive">{t("loadError")}</div>;
  }
  if (items.length === 0) {
    return <div className="text-sm text-muted-foreground">{t("noData")}</div>;
  }

  const totalCost = items.reduce((sum, item) => sum + item.cost, 0);
  return (
    <div data-testid={testId}>
      <ModelBreakdownColumn
        pageItems={items}
        currencyCode={currencyCode}
        totalCost={totalCost}
        keyPrefix={keyPrefix}
        pageOffset={0}
        labels={labels}
      />
    </div>
  );
}

interface ProviderModelsProps {
  userId: number;
  providerId: number;
  startDate?: string;
  endDate?: string;
  labels: ModelBreakdownLabels;
}

// 只在供应商行展开时挂载，故请求天然懒加载。
function ProviderModels({ userId, providerId, startDate, endDate, labels }: ProviderModelsProps) {
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

  return (
    <BreakdownList
      isLoading={query.isLoading}
      isError={query.isError}
      items={(query.data?.breakdown ?? []).map(toBreakdownItem)}
      currencyCode={(query.data?.currencyCode ?? "USD") as CurrencyCode}
      labels={labels}
      keyPrefix={`leaderboard-provider-${providerId}`}
      testId={`leaderboard-provider-models-${providerId}`}
    />
  );
}

interface ProviderBreakdownRowProps {
  item: ProviderBreakdownItem;
  currencyCode: CurrencyCode;
  totalCost: number;
  labels: ModelBreakdownLabels;
  userId: number;
  startDate?: string;
  endDate?: string;
}

function ProviderBreakdownRow({
  item,
  currencyCode,
  totalCost,
  labels,
  userId,
  startDate,
  endDate,
}: ProviderBreakdownRowProps) {
  const t = useTranslations("dashboard.leaderboard");
  const [open, setOpen] = useState(false);

  return (
    <Collapsible open={open} onOpenChange={setOpen}>
      <div className="flex items-center gap-1">
        <CollapsibleTrigger asChild>
          <button
            type="button"
            aria-expanded={open}
            aria-label={open ? t("collapseModelStats") : t("expandModelStats")}
            className="inline-flex cursor-pointer items-center rounded-sm focus:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2"
          >
            {open ? (
              <ChevronDown className="h-4 w-4 shrink-0 text-muted-foreground" />
            ) : (
              <ChevronRight className="h-4 w-4 shrink-0 text-muted-foreground" />
            )}
          </button>
        </CollapsibleTrigger>
        <div className="min-w-0 flex-1">
          <ModelBreakdownRow
            model={item.model}
            requests={item.requests}
            cost={item.cost}
            inputTokens={item.inputTokens}
            outputTokens={item.outputTokens}
            cacheCreationTokens={item.cacheCreationTokens}
            cacheReadTokens={item.cacheReadTokens}
            currencyCode={currencyCode}
            totalCost={totalCost}
            labels={labels}
          />
        </div>
      </div>
      <CollapsibleContent className="pt-2 pl-6">
        <ProviderModels
          userId={userId}
          providerId={item.providerId}
          startDate={startDate}
          endDate={endDate}
          labels={labels}
        />
      </CollapsibleContent>
    </Collapsible>
  );
}

// 用户视图行内展开区：供应商聚合（默认）与模型聚合两个 tab。
//
// 两个 tab 同源同指标集——都取自 usage_ledger 的两条 breakdown 聚合（requests / cost /
// inputTokens / outputTokens / cacheCreationTokens / cacheReadTokens），故可横向对账；
// 排行榜接口的 modelStats 指标集不同（无缓存 token 拆分），不用于此处。
export function LeaderboardUserExpanded({
  userId,
  period,
  dateRange,
}: LeaderboardUserExpandedProps) {
  const t = useTranslations("dashboard.leaderboard.userInsights");
  const tStats = useTranslations("myUsage.stats");
  const timeZone = useTimeZone() ?? "UTC";
  const [tab, setTab] = useState("provider");

  const { startDate, endDate } = resolvePeriodDates(period, dateRange, timeZone);

  const providerLabels = buildLabels(tStats, t("unknownProvider"));
  const modelLabels = buildLabels(tStats, t("unknownModel"));

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

  const providerItems: ProviderBreakdownItem[] = (providerQuery.data?.breakdown ?? []).map(
    (item) => ({
      providerId: item.providerId as number,
      model: item.providerName,
      requests: item.requests,
      cost: item.cost,
      inputTokens: item.inputTokens,
      outputTokens: item.outputTokens,
      cacheCreationTokens: item.cacheCreationTokens,
      cacheReadTokens: item.cacheReadTokens,
    })
  );
  const providerTotalCost = providerItems.reduce((sum, item) => sum + item.cost, 0);

  return (
    <div className="p-3" data-testid="leaderboard-user-expanded">
      <Tabs value={tab} onValueChange={setTab}>
        <TabsList>
          <TabsTrigger value="provider" data-testid="leaderboard-user-expanded-provider-tab">
            {t("providerBreakdown")}
          </TabsTrigger>
          <TabsTrigger value="model" data-testid="leaderboard-user-expanded-model-tab">
            {t("modelBreakdown")}
          </TabsTrigger>
        </TabsList>

        <TabsContent value="provider" className="pt-2">
          {providerQuery.isLoading ? (
            <Skeleton className="h-16 w-full" />
          ) : providerQuery.isError ? (
            <div className="text-sm text-destructive">{t("loadError")}</div>
          ) : providerItems.length === 0 ? (
            <div className="text-sm text-muted-foreground">{t("noData")}</div>
          ) : (
            <div className="space-y-2" data-testid="leaderboard-user-expanded-provider-list">
              {providerItems.map((item) => (
                <ProviderBreakdownRow
                  key={item.providerId}
                  item={item}
                  currencyCode={(providerQuery.data?.currencyCode ?? "USD") as CurrencyCode}
                  totalCost={providerTotalCost}
                  labels={providerLabels}
                  userId={userId}
                  startDate={startDate}
                  endDate={endDate}
                />
              ))}
            </div>
          )}
        </TabsContent>

        <TabsContent value="model" className="pt-2">
          <BreakdownList
            isLoading={modelQuery.isLoading}
            isError={modelQuery.isError}
            items={(modelQuery.data?.breakdown ?? []).map(toBreakdownItem)}
            currencyCode={(modelQuery.data?.currencyCode ?? "USD") as CurrencyCode}
            labels={modelLabels}
            keyPrefix="leaderboard-user-models"
            testId="leaderboard-user-expanded-model-list"
          />
        </TabsContent>
      </Tabs>
    </div>
  );
}
