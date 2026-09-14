"use client";

import { useQuery } from "@tanstack/react-query";
import { useTranslations } from "next-intl";
import { UiSessionGate } from "@/components/ui-session-gate";
import { DASHBOARD_COMPAT_HEADER } from "@/lib/api/v1/_shared/constants";
import { getProviders } from "@/lib/api-client/v1/actions/providers";
import { getSystemSettings } from "@/lib/api-client/v1/actions/system-config";
import { apiClient } from "@/lib/api-client/v1/client";
import type { ProviderDisplay } from "@/types/provider";
import { ProvidersQuotaSkeleton } from "../_components/providers-quota-skeleton";
import {
  type ProviderQuota,
  ProvidersQuotaManager,
  type ProviderWithQuota,
} from "./_components/providers-quota-manager";

type ProviderLimitBatchResponse = {
  items: { id: number; usage: ProviderQuota | null }[];
};

/**
 * 供应商配额页。改造前是 SSR（`getSession()` + `getProviders()` + `getProviderLimitUsageBatch()`）。
 *
 * 权限口径不变：仅 admin；非 admin 已登录 → /dashboard/my-quota，未登录 → /login。
 *
 * 数据面依赖：批量限额读数 `POST /api/v1/providers/limit-usage:batch` **Go 已实现**
 * （实测生产返回 200，形状 `{items:[{id, usage:{concurrentSessions,cost5h,costDaily,costWeekly,costMonthly}}]}`；
 * 见 ``），不再是待补项。
 * 保留的降级仍有用：读数失败时其余数据照常渲染、限额列退化为「无读数」，而不是整页报错。
 * 请求体形状与 Node 一致：`{ providerIds: number[] }`，服务端自行按可见性解析供应商元数据。
 */
export default function ProvidersQuotaPage() {
  return (
    <UiSessionGate requireRole="admin" forbiddenHref="/dashboard/my-quota">
      <ProvidersQuotaContent />
    </UiSessionGate>
  );
}

function ProvidersQuotaContent() {
  const t = useTranslations("quota.providers");

  const providers = useQuery<ProviderDisplay[]>({
    queryKey: ["providers"],
    queryFn: getProviders,
    staleTime: 30_000,
  });

  const providerIds = (providers.data ?? []).map((provider) => provider.id).sort((a, b) => a - b);
  const quotas = useQuery({
    queryKey: ["v1", "providers", "limit-usage-batch", providerIds],
    enabled: providerIds.length > 0,
    retry: false,
    queryFn: async () => {
      const body = await apiClient.post<ProviderLimitBatchResponse>(
        "/api/v1/providers/limit-usage:batch",
        { providerIds },
        { headers: { [DASHBOARD_COMPAT_HEADER]: "1" } }
      );
      return new Map(body.items.map((item) => [item.id, item.usage]));
    },
  });

  const settings = useQuery({
    queryKey: ["v1", "system", "settings"],
    queryFn: () => getSystemSettings(),
  });

  if (providers.isPending || settings.isPending) return <ProvidersQuotaSkeleton />;

  if (providers.isError) {
    return (
      <p className="text-sm text-destructive">
        {providers.error instanceof Error ? providers.error.message : t("title")}
      </p>
    );
  }

  const rows: ProviderWithQuota[] = (providers.data ?? []).map((provider) => ({
    id: provider.id,
    name: provider.name,
    providerType: provider.providerType,
    isEnabled: provider.isEnabled,
    priority: provider.priority,
    weight: provider.weight,
    // 读数缺失（尚未移植 / 请求失败）时退化为 null，与「无限额配置」同形。
    quota: quotas.data?.get(provider.id) ?? null,
  }));

  return (
    <div className="space-y-3">
      <p className="text-sm text-muted-foreground">{t("totalCount", { count: rows.length })}</p>
      <ProvidersQuotaManager providers={rows} currencyCode={settings.data?.currencyDisplay} />
    </div>
  );
}
