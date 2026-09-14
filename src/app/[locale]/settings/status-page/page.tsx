"use client";

import { useQuery } from "@tanstack/react-query";
import { useTranslations } from "next-intl";
import { LoadingState } from "@/components/loading/page-skeletons";
import { QueryErrorState } from "@/components/ui/query-error-state";
import { UiSessionGate } from "@/components/ui-session-gate";
import { getSystemSettings } from "@/lib/api-client/v1/actions/system-config";
import { useProviderGroups } from "@/lib/api-client/v1/provider-groups/hooks";
import { deriveStatusPageGroupInputs } from "@/lib/public-status/group-settings";
import { SettingsPageHeader } from "../_components/settings-page-header";
import { PublicStatusSettingsForm } from "./_components/public-status-settings-form";

/**
 * 公开状态页设置。
 *
 * 改造前服务端 `loadStatusPageSettings()` 读系统设置 + `bootstrapProviderGroupsFromProviders()`。
 * 现在客户端读同两项数据：`/api/v1/system/settings` 与 `/api/v1/provider-groups`，分组默认值
 * 用与 loader 相同的纯函数（`parsePublicStatusDescription` / `normalizePublicGroupSlug` /
 * `createUniquePublicGroupSlug`）算出，口径逐字对齐。
 *
 * 已知差异（登记为待办）：`bootstrapProviderGroupsFromProviders()` 里带一次**自愈**
 * （把仅存在于 provider `groupTag`、尚无分组行的名字补进分组表）。自愈是服务端的写副作用，
 * 客户端无法复刻；本页因此只列已有分组。建议在 U4 之前把自愈挪到「保存供应商分组」的服务端路径
 * （或在 `internal/adminapi` 分组写路径中补齐），使页面不再依赖读时自愈。
 */
export default function StatusPageSettingsPage() {
  const t = useTranslations("settings");

  return (
    <UiSessionGate requireRole="admin">
      <div className="space-y-6">
        <SettingsPageHeader
          title={t("statusPage.title")}
          description={t("statusPage.description")}
          icon="activity"
        />
        <StatusPageSettingsContent />
      </div>
    </UiSessionGate>
  );
}

function StatusPageSettingsContent() {
  const tErrors = useTranslations("settings.errors");
  const settingsQuery = useQuery({
    queryKey: ["system-settings"],
    queryFn: getSystemSettings,
  });
  const groupsQuery = useProviderGroups();

  if (settingsQuery.isLoading || groupsQuery.isLoading) {
    return <LoadingState className="p-6" />;
  }

  const settings = settingsQuery.data;
  const groups = groupsQuery.data?.items;
  if (settingsQuery.isError || !settings || groupsQuery.isError || !groups) {
    return (
      <QueryErrorState
        message={tErrors("fetchFailed")}
        onRetry={() => {
          if (settingsQuery.isError) void settingsQuery.refetch();
          if (groupsQuery.isError) void groupsQuery.refetch();
        }}
      />
    );
  }

  const initialGroups = deriveStatusPageGroupInputs(groups);

  return (
    <PublicStatusSettingsForm
      initialWindowHours={settings.publicStatusWindowHours}
      initialAggregationIntervalMinutes={settings.publicStatusAggregationIntervalMinutes}
      initialGroups={initialGroups}
    />
  );
}
