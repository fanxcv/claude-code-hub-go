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
 * 本页只关心这一个字段；其余设置项与本页无关，不必进依赖。
 *
 * 用窄类型而不是完整的 `SystemSettings`：避免把「设置页改了别的字段」当成「本页要重渲」的原因。
 * 并发统计开关在这里**只读一次并透传**给列表项里的徽标（开关只有一个真源）；轮询本身不在本组件。
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

  // 设置查询先声明：货币符号在本组件渲染时就要用（与原来的声明顺序一致，避免无谓的依赖变更）。
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

  const { data: healthStatus = {} as ProviderHealthStatus, isLoading: isHealthLoading } =
    useQuery<ProviderHealthStatus>({
      queryKey: ["providers-health"],
      queryFn: getProvidersHealthStatus,
      refetchOnWindowFocus: false,
      // **不再轮询**：实时并发数已下沉到 `ProviderConcurrencyBadge`（它独占 `providers-health-live` 这个 key）。
      // 轮询留在这里会让每次重取把整条链路（loader → manager → list → 全部列表项）重渲染一遍，
      // 且 `isHealthFetching` 会让列表上方的加载条反复挂载/卸载——用户看到的就是「整页刷新」。
      // 本查询只供熔断/等待阶梯/降权等**慢变**读数使用，按挂载时取一次 + 变更后的失效重取即可。
      staleTime: 30_000,
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
  // 刻意**不**含 `isHealthFetching`：health 的定期重取不再经过本组件（见上面 health 查询的说明），
  // 而把它算进整页指示器正是「列表每 5 秒上下跳一下」的直接原因。
  const refreshing = !loading && (isProvidersFetching || isSettingsFetching);
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
      liveStatsEnabled={liveStatsEnabled}
      addDialogSlot={<AddProviderDialog enableMultiProviderTypes={enableMultiProviderTypes} />}
    />
  );
}

export function ProviderManagerLoader(props: ProviderManagerLoaderProps) {
  return <ProviderManagerLoaderContent {...props} />;
}
