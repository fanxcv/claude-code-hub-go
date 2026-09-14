"use client";

import { useQuery } from "@tanstack/react-query";
import { useTranslations } from "next-intl";
import { Button } from "@/components/ui/button";
import { getSystemSettings } from "@/lib/api-client/v1/actions/system-config";
import { DashboardBento } from "./bento/dashboard-bento";
import { DashboardOverviewSkeleton } from "./dashboard-skeletons";

interface DashboardBentoSectionProps {
  isAdmin: boolean;
}

/**
 * 首页 bento 的数据入口。改造前它是服务端组件（`@/repository/system-config` + `@/actions/*`）；
 * 静态导出后没有服务端，故改为客户端取数：设置走既有 api-client（queryKey `["system-settings"]`，
 * 与 active-sessions-client / session-messages-client 同一范式），统计与概览由 `DashboardBento`
 * 自己拉（它本就是客户端组件，`initial*` 只是首屏加速，客户端改造后不再需要）。
 */
export function DashboardBentoSection({ isAdmin }: DashboardBentoSectionProps) {
  const t = useTranslations("dashboard");
  const tc = useTranslations("common");

  const {
    data: systemSettings,
    isLoading,
    isError,
    refetch,
  } = useQuery({
    queryKey: ["system-settings"],
    queryFn: getSystemSettings,
  });

  if (isLoading) return <DashboardOverviewSkeleton />;

  if (isError || !systemSettings) {
    return (
      <div className="flex items-center justify-between gap-3 border border-destructive/30 bg-destructive/5 px-4 py-3 text-sm text-destructive">
        <p>{t("errors.fetchSystemSettingsFailed")}</p>
        <Button variant="outline" size="sm" onClick={() => refetch()}>
          {tc("retry")}
        </Button>
      </div>
    );
  }

  return (
    <DashboardBento
      isAdmin={isAdmin}
      currencyCode={systemSettings.currencyDisplay}
      allowGlobalUsageView={systemSettings.allowGlobalUsageView}
      enableHighConcurrencyMode={systemSettings.enableHighConcurrencyMode}
    />
  );
}
