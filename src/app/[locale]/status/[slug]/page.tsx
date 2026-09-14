"use client";

import { useQuery } from "@tanstack/react-query";
import { useParams } from "next/navigation";
import { useLocale, useTranslations } from "next-intl";
import { LoadingState } from "@/components/loading/page-skeletons";
import { QueryErrorState } from "@/components/ui/query-error-state";
import { apiClient } from "@/lib/api-client/v1/client";
import {
  type PublicStatusRouteResponse,
  toPublicStatusPayload,
} from "@/lib/public-status/public-api-contract";
import { resolveSiteTitle } from "@/lib/site-title";
import { PublicStatusView } from "../_components/public-status-view";
import { usePublicStatusLabels } from "../_lib/public-status-labels";

/**
 * 单分组公开状态页（动态段）。
 *
 * 动态段在静态导出下不进产物，故段值改由 `useParams()` 在客户端解析，数据按 `groupSlug`
 * 查询（与原服务端的 `loadPublicStatusPageData({ groupSlug })` 同一条查询）。
 *
 * 与改造前的两点差异（均属静态导出固有限制，待 U4 导出波用「排除该路由 + Go 壳回退」收口）：
 * 1. 客户端组件不能导出 `generateMetadata`，分组的自定义标题不再生效（回落到 layout 标题）；
 * 2. `notFound()` 不可用于客户端组件，分组不存在时改为渲染本页的空态（用既有 i18n 文案）。
 */
export default function PublicStatusGroupPage() {
  const t = useTranslations("settings.statusPage.public");
  const tErrors = useTranslations("settings.errors");
  const locale = useLocale();
  const labels = usePublicStatusLabels();
  const params = useParams<{ slug: string }>();
  const slug = typeof params?.slug === "string" ? params.slug : "";

  const statusQuery = useQuery({
    queryKey: ["public", "status", slug],
    queryFn: () =>
      apiClient.get<PublicStatusRouteResponse>(
        `/api/v1/public/status?include=meta&groupSlug=${encodeURIComponent(slug)}`
      ),
    enabled: slug.length > 0,
    refetchInterval: 30_000,
    retry: false,
  });

  if (slug.length === 0 || statusQuery.isLoading) return <LoadingState className="p-6" />;

  const data = statusQuery.data;
  if (statusQuery.isError || !data) {
    return (
      <QueryErrorState
        message={tErrors("fetchFailed")}
        onRetry={() => void statusQuery.refetch()}
      />
    );
  }

  const payload = toPublicStatusPayload(data);
  const targetGroup = payload.groups.find((group) => group.publicGroupSlug === slug);

  if (!targetGroup) {
    return (
      <div className="mx-auto flex max-w-3xl flex-col items-center justify-center gap-2 px-4 py-16 text-center">
        <p className="text-lg font-medium">{t("noData")}</p>
        <p className="text-sm text-muted-foreground">{t("emptyDescription")}</p>
      </div>
    );
  }

  return (
    <PublicStatusView
      initialPayload={{ ...payload, groups: [targetGroup] }}
      intervalMinutes={data.resolvedQuery.intervalMinutes}
      rangeHours={data.resolvedQuery.rangeHours}
      followServerDefaults
      filterSlug={slug}
      initialStatus={data.status}
      locale={locale}
      siteTitle={resolveSiteTitle(data.meta?.siteTitle)}
      timeZone={data.meta?.timeZone ?? "UTC"}
      labels={labels}
    />
  );
}
