"use client";

import { useQuery } from "@tanstack/react-query";
import { Activity } from "lucide-react";
import { useTranslations } from "next-intl";
import { Badge } from "@/components/ui/badge";
import {
  getProvidersHealthStatus,
  type ProviderHealthStatus,
} from "@/lib/api-client/v1/actions/providers";
import { cn } from "@/lib/utils";
import type { ProviderConcurrencyHealth } from "@/types/provider";

/**
 * 实时并发统计的刷新间隔（5 秒，用户要求）。
 *
 * 仓内既有同尺度范式的取值也是 5000（`dashboard-bento.tsx`、`active-sessions-list.tsx`）。
 */
const LIVE_STATS_REFRESH_MS = 5_000;

/**
 * 渠道行的实时并发徽标（桌面端与移动端共用）。
 *
 * **为什么把轮询放在这个叶子组件里，而不是让 loader 轮询 health 再层层传下来**：
 * TanStack Query 的重取是**缓存级**的——同一个 queryKey 下任何一个观察者触发重取并落库，
 * 该 key 的**全部**观察者都会拿到新引用而重渲染。若轮询挂在 `provider-manager-loader` 的
 * `providers-health` 上，则每 5 秒整条链路（loader → manager → list → 全部列表项）都要重渲染一次，
 * 且 `isHealthFetching` 会让列表上方的加载条反复挂载/卸载（列表整体上下跳动）——用户看到的就是
 * 「整页刷新」。故轮询单独占一个 key（`providers-health-live`），**只有本组件**订阅它：
 * 每 5 秒只有徽标这一小块重渲染，列表、筛选、展开态一概不受影响。
 *
 * 代价（刻意取舍）：开关打开时页面加载多一次 health 请求——loader 那条已不再轮询，只取一次。
 *
 * `liveStatsEnabled` 由页面级设置查询（loader 的 `system-settings`）经 props 传下来，本组件**不**自己
 * 再取一次设置：开关只有一个真源，且未传时（例如单测直接渲染列表项）本组件完全惰性、不发任何请求。
 *
 * 四态与列表项原有的纪律一致（见 `ProviderRichListItem` 的说明）：`trackingEnabled` 为假（开关没开）、
 * `available` 为假（开着但读不到）、`activeSessions` 为 null，三态**都不占位**——写错的代价是管理员
 * 看到「并发 0」并以为渠道空闲，而真相是「根本没开统计」。停用渠道同样不报读数。
 */
export function ProviderConcurrencyBadge({
  providerId,
  limit,
  isEnabled,
  liveStatsEnabled,
  className,
}: {
  providerId: number;
  /** 渠道级并发上限；为 0（未设）时只显示分子，不显示一个假的「/0」。 */
  limit: number;
  /** 停用渠道不报读数（与熔断/降权徽标同一条纪律）。 */
  isEnabled: boolean;
  /** 全局统计开关的本次读数；为假时连取数都不发（用户要求「关上就完全不占用资源」）。 */
  liveStatsEnabled: boolean;
  className?: string;
}) {
  const tList = useTranslations("settings.providers.list");

  const { data: concurrency } = useQuery<
    ProviderHealthStatus,
    Error,
    ProviderConcurrencyHealth | null
  >({
    queryKey: ["providers-health-live"],
    queryFn: getProvidersHealthStatus,
    refetchOnWindowFocus: false,
    // 两道闸门都给：enabled 决定「发不发请求」，refetchInterval 决定「排不排定时器」。
    enabled: liveStatsEnabled,
    refetchInterval: liveStatsEnabled ? LIVE_STATS_REFRESH_MS : false,
    staleTime: LIVE_STATS_REFRESH_MS,
    // 只取本渠道这一小块：select 的返回值按引用比较，故本渠道的读数没变时本组件不重渲染。
    select: (all) => all[providerId]?.concurrency ?? null,
  });

  const activeSessions =
    concurrency?.trackingEnabled === true &&
    concurrency.available &&
    concurrency.activeSessions !== null
      ? concurrency.activeSessions
      : null;

  if (!isEnabled || activeSessions === null) {
    return null;
  }

  return (
    <Badge
      variant="outline"
      className={cn(
        "flex items-center gap-1 bg-sky-50 text-sky-700 border-sky-300 hover:bg-sky-100 dark:bg-sky-950/40 dark:text-sky-400 dark:border-sky-800",
        className
      )}
      title={tList("concurrency.tooltip")}
    >
      <Activity className="h-3 w-3" />
      {limit > 0
        ? tList("concurrency.badgeWithLimit", { count: activeSessions, limit })
        : tList("concurrency.badge", { count: activeSessions })}
    </Badge>
  );
}
