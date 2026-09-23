"use client";

import { useRouter } from "next/navigation";
import { useTranslations } from "next-intl";
import { RefreshCacheButton as RefreshCacheButtonBase } from "@/components/ui/refresh-cache-button";
import { refreshCacheAction } from "@/lib/api-client/v1/actions/error-rules";

interface RefreshCacheButtonProps {
  stats: {
    regexCount: number;
    containsCount: number;
    exactCount: number;
    totalCount: number;
    lastReloadTime: number;
    isLoading: boolean;
  } | null;
}

export function RefreshCacheButton({ stats }: RefreshCacheButtonProps) {
  const t = useTranslations("settings");
  const router = useRouter();

  return (
    <RefreshCacheButtonBase
      stats={stats}
      label={t("errorRules.refreshCache")}
      title={
        stats
          ? t("errorRules.cacheStats", { totalCount: stats.totalCount })
          : t("errorRules.refreshCache")
      }
      className="bg-muted/50 border-border hover:bg-muted hover:border-border"
      refresh={refreshCacheAction}
      successMessage={(count) => t("errorRules.refreshCacheSuccess", { count })}
      failureMessage={t("errorRules.refreshCacheFailed")}
      onRefreshed={() => router.refresh()}
    />
  );
}
