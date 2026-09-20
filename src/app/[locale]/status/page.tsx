"use client";

import { useQuery } from "@tanstack/react-query";
import { useLocale, useTranslations } from "next-intl";
import { LoadingState } from "@/components/loading/page-skeletons";
import { QueryErrorState } from "@/components/ui/query-error-state";
import { apiClient } from "@/lib/api-client/v1/client";
import {
  type PublicStatusRouteResponse,
  toPublicStatusPayload,
} from "@/lib/public-status/public-api-contract";
import { resolveSiteTitle } from "@/lib/site-title";
import { PublicStatusView } from "./_components/public-status-view";
import { usePublicStatusLabels } from "./_lib/public-status-labels";

/**
 * 公开状态页。改造前由 `loadPublicStatusPageData()`（服务端）读 `/api/public-status`。
 *
 * 静态化后没有服务端取数，改由客户端打同语义的 `/api/v1/public/status`；响应体与
 * 旧路径同为 `PublicStatusRouteResponse`，含 `meta.siteTitle`/`meta.timeZone`。
 * 路由状态 `rebuilding` 会以 503 返回，此时**不是**取数失败：按契约照常渲染
 * （`rebuildState: rebuilding`），只有网络/其它错误才落到错误态。
 *
 * 已知差异：原服务端页从 `cookies`/`headers` 拿站点标题与时区，现取自响应体的 `meta`
 * （`siteTitle` 缺失时由 `DEFAULT_SITE_TITLE` 兜底）；Go 壳注入 `__CCH_BOOTSTRAP__` 后可直接改用注入值。
 */
export default function PublicStatusPage() {
  const tErrors = useTranslations("settings.errors");
  const locale = useLocale();
  const labels = usePublicStatusLabels();

  const statusQuery = useQuery({
    queryKey: ["public", "status", "root"],
    queryFn: () => apiClient.get<PublicStatusRouteResponse>("/api/v1/public/status?include=meta"),
    refetchInterval: 30_000,
    retry: false,
  });

  if (statusQuery.isLoading) return <LoadingState className="p-6" />;

  const data = statusQuery.data;
  if (statusQuery.isError || !data) {
    return (
      <QueryErrorState
        message={tErrors("fetchFailed")}
        onRetry={() => void statusQuery.refetch()}
      />
    );
  }

  const initialPayload = toPublicStatusPayload(data);

  return (
    <PublicStatusView
      initialPayload={initialPayload}
      intervalMinutes={data.resolvedQuery.intervalMinutes}
      rangeHours={data.resolvedQuery.rangeHours}
      followServerDefaults
      initialStatus={data.status}
      locale={locale}
      siteTitle={resolveSiteTitle(data.meta?.siteTitle)}
      timeZone={data.meta?.timeZone ?? "UTC"}
      labels={labels}
    />
  );
}
