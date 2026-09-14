"use client";

import { useQuery } from "@tanstack/react-query";
import { useTranslations } from "next-intl";
import { QueryErrorState } from "@/components/ui/query-error-state";
import { UiSessionGate } from "@/components/ui-session-gate";
import { fetchClientVersionStats } from "@/lib/api-client/v1/actions/client-versions";
import { fetchSystemSettings } from "@/lib/api-client/v1/actions/system-config";
import type { ClientVersionStats } from "@/types/client-versions";
import { SettingsPageHeader } from "../_components/settings-page-header";
import { SettingsSection } from "../_components/ui/settings-ui";
import { ClientVersionStatsTable } from "./_components/client-version-stats-table";
import { ClientVersionToggle } from "./_components/client-version-toggle";
import {
  ClientVersionsSettingsSkeleton,
  ClientVersionsTableSkeleton,
} from "./_components/client-versions-skeleton";

/**
 * 客户端版本页。改造前服务端读 `fetchClientVersionStats()` 与 `fetchSystemSettings()`
 * （`@/actions/client-versions`、`@/actions/system-config`）；两条都在 api-client 里有
 * 同语义的 REST 封装，故改为客户端取数。原服务端的 `redirect("/login")` 判定由鉴权壳承担。
 */
export default function ClientVersionsPage() {
  const t = useTranslations("settings");

  return (
    <UiSessionGate requireRole="admin">
      <div className="space-y-6">
        <SettingsPageHeader
          title={t("clientVersions.title")}
          description={t("clientVersions.description")}
          icon="smartphone"
        />
        {/* Settings Toggle Section */}
        <SettingsSection
          title={t("clientVersions.section.settings.title")}
          description={t("clientVersions.section.settings.description")}
          icon="smartphone"
          iconColor="text-[#E25706]"
        >
          <ClientVersionsSettingsContent />
        </SettingsSection>

        {/* Version Distribution Section */}
        <SettingsSection
          title={t("clientVersions.section.distribution.title")}
          description={t("clientVersions.section.distribution.description")}
          icon="smartphone"
          iconColor="text-[#E25706]"
        >
          <ClientVersionsStatsContent />
        </SettingsSection>
      </div>
    </UiSessionGate>
  );
}

function ClientVersionsSettingsContent() {
  const tErrors = useTranslations("settings.errors");
  const settingsQuery = useQuery({
    queryKey: ["system-settings"],
    queryFn: fetchSystemSettings,
  });

  if (settingsQuery.isLoading) return <ClientVersionsSettingsSkeleton />;
  const result = settingsQuery.data;
  if (settingsQuery.isError || !result?.ok) {
    return (
      <QueryErrorState
        message={tErrors("fetchFailed")}
        onRetry={() => void settingsQuery.refetch()}
      />
    );
  }

  return <ClientVersionToggle enabled={result.data.enableClientVersionCheck} />;
}

function ClientVersionsStatsContent() {
  const t = useTranslations("settings");
  const tErrors = useTranslations("settings.errors");
  const statsQuery = useQuery({
    queryKey: ["dashboard", "client-versions"],
    queryFn: fetchClientVersionStats,
  });

  if (statsQuery.isLoading) return <ClientVersionsTableSkeleton />;
  const result = statsQuery.data;
  if (statsQuery.isError || !result?.ok) {
    return (
      <QueryErrorState message={tErrors("fetchFailed")} onRetry={() => void statsQuery.refetch()} />
    );
  }

  const stats = (result.data ?? []) as ClientVersionStats[];

  if (stats.length === 0) {
    return (
      <div className="flex flex-col items-center justify-center py-12 text-center rounded-xl bg-white/[0.02] border border-white/5">
        <p className="text-muted-foreground">{t("clientVersions.empty.title")}</p>
        <p className="mt-2 text-sm text-muted-foreground">
          {t("clientVersions.empty.description")}
        </p>
      </div>
    );
  }

  return <ClientVersionStatsTable data={stats} />;
}
