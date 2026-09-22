"use client";

import { useQuery } from "@tanstack/react-query";
import {
  getProviderStatisticsAsync,
  getProviders,
  getProvidersHealthStatus,
  type ProviderHealthStatus,
} from "@/lib/api-client/v1/actions/providers";
import { getSystemSettings } from "@/lib/api-client/v1/actions/system-config";
import type { ProviderDisplay, ProviderStatisticsMap } from "@/types/provider";
import type { SystemSettings } from "@/types/system-config";
import { AddProviderDialog } from "./add-provider-dialog";
import { ProviderManager } from "./provider-manager";
import type { ProviderViewer } from "./provider-viewer";

/**
 * 实时并发统计的刷新间隔（5 秒，用户要求）。
 *
 * 仓内既有同尺度范式的取值也是 5000（`dashboard-bento.tsx`、`active-sessions-list.tsx`）。
 */
const LIVE_STATS_REFRESH_MS = 5_000;

/**
 * 本页只关心这两个字段；其余设置项与本页无关，不必进依赖。
 *
 * 用窄类型而不是完整的 `SystemSettings`：避免把「设置页改了别的字段」当成「本页要重渲」的原因。
 */
type SystemSettingsSummary = Pick<SystemSettings, "currencyDisplay" | "providerLiveStatsEnabled">;

interface ProviderManagerLoaderProps {
  currentUser?: ProviderViewer;
  enableMultiProviderTypes?: boolean;
}

function ProviderManagerLoaderContent({
  currentUser,
  enableMultiProviderTypes = true,
}: ProviderManagerLoaderProps) {
  const {
    data: providers = [],
    isLoading: isProvidersLoading,
    isFetching: isProvidersFetching,
  } = useQuery<ProviderDisplay[]>({
    queryKey: ["providers"],
    queryFn: getProviders,
    refetchOnWindowFocus: false,
    staleTime: 30_000,
  });

  // 设置先取：实时并发统计的开关在 health 查询的 refetchInterval 里用，
  // 必须先于 health 查询声明（同一渲染周期内前者已解构出值）。
  // 两者同一次页面加载发出，无额外往返。
  const {
    data: systemSettings,
    isLoading: isSettingsLoading,
    isFetching: isSettingsFetching,
  } = useQuery<SystemSettingsSummary>({
    queryKey: ["system-settings"],
    queryFn: getSystemSettings,
    refetchOnWindowFocus: false,
    staleTime: 30_000,
  });

  const liveStatsEnabled = systemSettings?.providerLiveStatsEnabled ?? false;

  const {
    data: healthStatus = {} as ProviderHealthStatus,
    isLoading: isHealthLoading,
    isFetching: isHealthFetching,
  } = useQuery<ProviderHealthStatus>({
    queryKey: ["providers-health"],
    queryFn: getProvidersHealthStatus,
    refetchOnWindowFocus: false,
    // 统计开关关闭时不轮询：关闭时 refetchInterval 为 false，TanStack Query 不排任何定时器。
    refetchInterval: liveStatsEnabled ? LIVE_STATS_REFRESH_MS : false,
    staleTime: liveStatsEnabled ? LIVE_STATS_REFRESH_MS : 30_000,
  });

  // Statistics loaded independently with longer cache
  const { data: statistics = {} as ProviderStatisticsMap, isLoading: isStatisticsLoading } =
    useQuery<ProviderStatisticsMap>({
      queryKey: ["providers-statistics"],
      queryFn: getProviderStatisticsAsync,
      refetchOnWindowFocus: false,
      staleTime: 30_000,
      refetchInterval: 60_000,
    });

  const loading = isProvidersLoading || isHealthLoading || isSettingsLoading;
  const refreshing = !loading && (isProvidersFetching || isHealthFetching || isSettingsFetching);
  const currencyCode = systemSettings?.currencyDisplay ?? "USD";

  return (
    <ProviderManager
      providers={providers}
      currentUser={currentUser}
      healthStatus={healthStatus}
      statistics={statistics}
      statisticsLoading={isStatisticsLoading}
      currencyCode={currencyCode}
      enableMultiProviderTypes={enableMultiProviderTypes}
      loading={loading}
      refreshing={refreshing}
      addDialogSlot={<AddProviderDialog enableMultiProviderTypes={enableMultiProviderTypes} />}
    />
  );
}

export function ProviderManagerLoader(props: ProviderManagerLoaderProps) {
  return <ProviderManagerLoaderContent {...props} />;
}
