"use client";

import { useQuery } from "@tanstack/react-query";
import { useTranslations } from "next-intl";
import { Section } from "@/components/section";
import { QueryErrorState } from "@/components/ui/query-error-state";
import { Skeleton } from "@/components/ui/skeleton";
import { UiSessionGate } from "@/components/ui-session-gate";
import { getCacheStats, listErrorRules } from "@/lib/api-client/v1/actions/error-rules";
import type { ErrorRule } from "@/types/error-rules";
import { SettingsPageHeader } from "../_components/settings-page-header";
import { AddRuleDialog } from "./_components/add-rule-dialog";
import { ErrorRuleTester } from "./_components/error-rule-tester";
import { ErrorRulesTableSkeleton } from "./_components/error-rules-skeleton";
import { RefreshCacheButton } from "./_components/refresh-cache-button";
import { RuleListTable } from "./_components/rule-list-table";

/**
 * 错误规则页。改造前两个 async 子组件分别读 `listErrorRules()` 与 `getCacheStats()`
 * （`@/actions/error-rules`）；现改为客户端经 `src/lib/api-client` 打同一批 REST 端点。
 */
export default function ErrorRulesPage() {
  const t = useTranslations("settings");

  return (
    <UiSessionGate requireRole="admin">
      <SettingsPageHeader
        title={t("errorRules.title")}
        description={t("errorRules.description")}
        icon="alert-triangle"
      />
      <div className="space-y-6">
        <Section
          title={t("errorRules.tester.title")}
          description={t("errorRules.tester.description")}
          icon="flask-conical"
          iconColor="text-blue-400"
        >
          <ErrorRuleTester />
        </Section>

        <Section
          title={t("errorRules.section.title")}
          icon="alert-triangle"
          iconColor="text-orange-400"
          variant="default"
          actions={
            <div className="flex gap-2">
              <ErrorRulesRefreshAction />
              <AddRuleDialog />
            </div>
          }
        >
          <ErrorRulesTableContent />
        </Section>
      </div>
    </UiSessionGate>
  );
}

function ErrorRulesRefreshAction() {
  const tErrors = useTranslations("settings.errors");
  const statsQuery = useQuery({
    queryKey: ["error-rules", "cache-stats"],
    queryFn: async () => {
      const result = await getCacheStats();
      if (!result.ok) throw new Error(result.error);
      return result.data;
    },
  });

  if (statsQuery.isLoading) return <Skeleton className="h-9 w-24" />;
  const stats = statsQuery.data;
  if (statsQuery.isError || !stats) {
    return (
      <QueryErrorState
        className="flex items-center gap-2 text-xs text-destructive"
        message={tErrors("fetchFailed")}
        onRetry={() => void statsQuery.refetch()}
      />
    );
  }

  return <RefreshCacheButton stats={stats} />;
}

function ErrorRulesTableContent() {
  const tErrors = useTranslations("settings.errors");
  const rulesQuery = useQuery({
    queryKey: ["error-rules", "list"],
    queryFn: async () => {
      const result = await listErrorRules();
      if (!result.ok) throw new Error(result.error);
      return result.data as ErrorRule[];
    },
  });

  if (rulesQuery.isLoading) return <ErrorRulesTableSkeleton />;
  const rules = rulesQuery.data;
  if (rulesQuery.isError || !rules) {
    return (
      <QueryErrorState message={tErrors("fetchFailed")} onRetry={() => void rulesQuery.refetch()} />
    );
  }

  return <RuleListTable rules={rules} />;
}
