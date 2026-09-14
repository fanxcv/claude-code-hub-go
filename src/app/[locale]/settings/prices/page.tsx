"use client";

import { useQuery } from "@tanstack/react-query";
import { useSearchParams } from "next/navigation";
import { useTranslations } from "next-intl";
import { Section } from "@/components/section";
import { QueryErrorState } from "@/components/ui/query-error-state";
import { UiSessionGate } from "@/components/ui-session-gate";
import { getModelPrices } from "@/lib/api-client/v1/actions/model-prices";
import { apiClient } from "@/lib/api-client/v1/client";
import type { ModelPrice, ModelPriceSource } from "@/types/model-price";
import { SettingsPageHeader } from "../_components/settings-page-header";
import { ModelPriceDrawer } from "./_components/model-price-drawer";
import { PriceList } from "./_components/price-list";
import { PricesSkeleton } from "./_components/prices-skeleton";
import { SyncLiteLLMButton } from "./_components/sync-litellm-button";
import { UploadPriceDialog } from "./_components/upload-price-dialog";

/**
 * 价格表页。改造前服务端读 `searchParams` 并取分页数据；现在分页参数改由
 * `useSearchParams()` 读取（静态导出下无 `searchParams` prop），数据经 api-client 拉取。
 *
 * 分页端点不经 `actions/model-prices` 的 `getModelPricesPaginated` 封装：那条封装的
 * `toQuery` 不透传 `vendor`，会静默丢掉 vendor 过滤；这里直接走同一个 api-client 与端点，
 * 参数集合与原服务端调用逐个对齐（page/pageSize/search/source/vendor）。
 */
export default function SettingsPricesPage() {
  const t = useTranslations("settings");

  return (
    <UiSessionGate requireRole="admin">
      <SettingsPageHeader
        title={t("prices.title")}
        description={t("prices.description")}
        icon="dollar-sign"
      />
      <SettingsPricesContent />
    </UiSessionGate>
  );
}

interface PaginatedPriceResponse {
  items?: ModelPrice[];
  total?: number;
  page?: number;
  pageSize?: number;
}

function SettingsPricesContent() {
  const t = useTranslations("settings");
  const tErrors = useTranslations("settings.errors");
  const searchParams = useSearchParams();

  // 解析分页参数（口径与原服务端页一致）
  const requestedPage = parseInt(searchParams.get("page") || "1", 10);
  const requestedPageSize = parseInt(
    searchParams.get("pageSize") || searchParams.get("size") || "50",
    10
  );
  const search = searchParams.get("search")?.trim() || undefined;
  const rawSource = searchParams.get("source");
  const source: ModelPriceSource | undefined =
    rawSource === "manual" || rawSource === "cloud" || rawSource === "litellm"
      ? rawSource
      : undefined;
  const vendor = searchParams.get("vendor")?.trim() || undefined;
  const isRequired = searchParams.get("required") === "true";

  const pricesQuery = useQuery({
    queryKey: [
      "model-prices",
      "paginated",
      requestedPage,
      requestedPageSize,
      search,
      source,
      vendor,
    ],
    queryFn: async () => {
      const query = new URLSearchParams();
      query.set("page", String(requestedPage));
      query.set("pageSize", String(requestedPageSize));
      if (search) query.set("search", search);
      if (source) query.set("source", source);
      if (vendor) query.set("vendor", vendor);

      try {
        const body = await apiClient.get<PaginatedPriceResponse>(
          `/api/v1/model-prices?${query.toString()}`
        );
        return {
          prices: body.items ?? [],
          total: body.total ?? 0,
          page: body.page ?? requestedPage,
          pageSize: body.pageSize ?? requestedPageSize,
        };
      } catch {
        // 与原服务端页同一降级口径：分页取数失败则退回全量列表（此时显示所有数据）。
        const allPrices = ((await getModelPrices()) ?? []) as ModelPrice[];
        return {
          prices: allPrices,
          total: allPrices.length,
          page: 1,
          pageSize: allPrices.length,
        };
      }
    },
  });

  if (pricesQuery.isLoading) return <PricesSkeleton />;

  const data = pricesQuery.data;
  if (pricesQuery.isError || !data) {
    return (
      <QueryErrorState
        message={tErrors("fetchFailed")}
        onRetry={() => void pricesQuery.refetch()}
      />
    );
  }

  return (
    <Section
      title={t("prices.section.title")}
      description={t("prices.section.description")}
      icon="dollar-sign"
      iconColor="text-[#E25706]"
      variant="default"
      actions={
        <div className="flex gap-2">
          <ModelPriceDrawer mode="create" />
          <SyncLiteLLMButton />
          <UploadPriceDialog defaultOpen={isRequired && data.total === 0} isRequired={isRequired} />
        </div>
      }
    >
      <PriceList
        initialPrices={data.prices}
        initialTotal={data.total}
        initialPage={data.page}
        initialPageSize={data.pageSize}
        initialSearchTerm={search ?? ""}
        initialSourceFilter={source ?? ""}
        initialVendorFilter={vendor ?? ""}
      />
    </Section>
  );
}
