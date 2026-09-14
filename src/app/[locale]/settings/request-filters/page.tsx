"use client";

import { useQuery } from "@tanstack/react-query";
import { useTranslations } from "next-intl";
import { Section } from "@/components/section";
import { QueryErrorState } from "@/components/ui/query-error-state";
import { UiSessionGate } from "@/components/ui-session-gate";
import {
  listProvidersForFilterAction,
  listRequestFilters,
} from "@/lib/api-client/v1/actions/request-filters";
import type { RequestFilter } from "@/types/request-filters";
import { SettingsPageHeader } from "../_components/settings-page-header";
import { FilterTable } from "./_components/filter-table";
import { RequestFiltersTableSkeleton } from "./_components/request-filters-skeleton";

/**
 * 请求过滤页。改造前服务端读 `listRequestFilters()` 与 `findAllProviders()`，
 * 后者只为拿 `{id, name}` 供筛选（服务端刻意不把供应商密钥传给客户端）。
 * 现改用同语义的选项端点 `/api/v1/request-filters/options/providers`——它正是为此而存在，
 * 且不含密钥字段，与原先的裁剪口径一致。
 */
export default function RequestFiltersPage() {
  const t = useTranslations("settings.requestFilters");

  return (
    <UiSessionGate requireRole="admin">
      <SettingsPageHeader title={t("title")} description={t("description")} icon="filter" />
      <Section
        title={t("title")}
        description={t("description")}
        icon="filter"
        iconColor="text-[#E25706]"
        variant="default"
      >
        <RequestFiltersContent />
      </Section>
    </UiSessionGate>
  );
}

function RequestFiltersContent() {
  const tErrors = useTranslations("settings.errors");
  const filtersQuery = useQuery({
    queryKey: ["request-filters", "list"],
    queryFn: async () => {
      const result = await listRequestFilters();
      if (!result.ok) throw new Error(result.error);
      return result.data as RequestFilter[];
    },
  });
  const providersQuery = useQuery({
    queryKey: ["request-filters", "options", "providers"],
    queryFn: async () => {
      const result = await listProvidersForFilterAction();
      if (!result.ok) throw new Error(result.error);
      return result.data;
    },
  });

  if (filtersQuery.isLoading || providersQuery.isLoading) {
    return <RequestFiltersTableSkeleton />;
  }

  const filters = filtersQuery.data;
  const providers = providersQuery.data;
  if (filtersQuery.isError || !filters || providersQuery.isError || !providers) {
    return (
      <QueryErrorState
        message={tErrors("fetchFailed")}
        onRetry={() => {
          if (filtersQuery.isError) void filtersQuery.refetch();
          if (providersQuery.isError) void providersQuery.refetch();
        }}
      />
    );
  }

  return <FilterTable filters={filters} providers={providers} />;
}
