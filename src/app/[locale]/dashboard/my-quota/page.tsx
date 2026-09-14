"use client";

import { useQuery } from "@tanstack/react-query";
import { AlertCircle } from "lucide-react";
import { useTranslations } from "next-intl";
import { LoadingState } from "@/components/loading/page-skeletons";
import { Alert, AlertDescription, AlertTitle } from "@/components/ui/alert";
import { QueryErrorState } from "@/components/ui/query-error-state";
import { getMyQuota } from "@/lib/api-client/v1/actions/my-usage";
import { getSystemSettings } from "@/lib/api-client/v1/actions/system-config";
import { QuotaCards } from "../../my-usage/_components/quota-cards";

/**
 * 我的额度页。静态化改造后改为客户端取数：
 * - `getMyQuota()`：原 `@/actions/my-usage`（server action）→ `@/lib/api-client/v1/actions/my-usage`，
 *   同一 REST 端点 `/api/v1/me/quota`，返回形状同为 `ActionResult`（`ok` / `error`）；
 * - `getSystemSettings()`：原 `@/repository/system-config`（服务端仓库）→ 客户端 action
 *   （`/api/v1/system/settings`），查询键与 `users-page-client`、`settings/config` 一致以便共用缓存。
 *
 * 原页**没有登录态判定**（靠取数失败落到错误态），此处保持同一口径：不套 `UiSessionGate`，
 * 也不做角色重定向；未登录时 `/api/v1/me/quota` 会以 401 落地，页面照旧显示错误提示。
 * 服务器时区不经本页（`QuotaCards` 只需额度与币种）。
 */
export default function MyQuotaPage() {
  const tNav = useTranslations("dashboard.nav");
  const tCommon = useTranslations("common");

  const quotaQuery = useQuery({ queryKey: ["me", "quota"], queryFn: getMyQuota });
  const settingsQuery = useQuery({
    queryKey: ["system-settings"],
    queryFn: getSystemSettings,
    staleTime: 30_000,
  });

  // 两个查询都就绪后再渲染：原服务端版本在首帧即持有二者，先画再换币种会闪。
  if (quotaQuery.isPending || settingsQuery.isPending) {
    return <LoadingState className="p-6" />;
  }

  const header = (
    <div className="flex items-center justify-between">
      <h3 className="text-lg font-medium">{tNav("myQuota")}</h3>
    </div>
  );

  if (quotaQuery.isError) {
    return (
      <div className="space-y-4">
        {header}
        <QueryErrorState
          message={tCommon("error")}
          onRetry={() => {
            void quotaQuery.refetch();
          }}
        />
      </div>
    );
  }

  const quotaResult = quotaQuery.data;

  if (!quotaResult?.ok) {
    return (
      <div className="space-y-4">
        {header}
        <Alert variant="destructive">
          <AlertCircle className="h-4 w-4" />
          <AlertTitle>{tCommon("error")}</AlertTitle>
          <AlertDescription>{quotaResult?.error ?? ""}</AlertDescription>
        </Alert>
      </div>
    );
  }

  return (
    <div className="space-y-4">
      {header}
      <QuotaCards quota={quotaResult.data} currencyCode={settingsQuery.data?.currencyDisplay} />
    </div>
  );
}
