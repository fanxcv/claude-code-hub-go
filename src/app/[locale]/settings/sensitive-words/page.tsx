"use client";

import { useQuery } from "@tanstack/react-query";
import { useTranslations } from "next-intl";
import { Section } from "@/components/section";
import { QueryErrorState } from "@/components/ui/query-error-state";
import { Skeleton } from "@/components/ui/skeleton";
import { UiSessionGate } from "@/components/ui-session-gate";
import { getCacheStats, listSensitiveWords } from "@/lib/api-client/v1/actions/sensitive-words";
import type { SensitiveWord } from "@/types/sensitive-words";
import { SettingsPageHeader } from "../_components/settings-page-header";
import { AddWordDialog } from "./_components/add-word-dialog";
import { RefreshCacheButton } from "./_components/refresh-cache-button";
import { SensitiveWordsTableSkeleton } from "./_components/sensitive-words-skeleton";
import { WordListTable } from "./_components/word-list-table";

/**
 * 敏感词页。改造前两个 async 子组件分别读 `listSensitiveWords()` 与 `getCacheStats()`
 * （`@/actions/sensitive-words`）；现改为客户端经 `src/lib/api-client` 打同一批 REST 端点。
 */
export default function SensitiveWordsPage() {
  const t = useTranslations("settings");

  return (
    <UiSessionGate requireRole="admin">
      <SettingsPageHeader
        title={t("sensitiveWords.title")}
        description={t("sensitiveWords.description")}
        icon="shield-alert"
      />
      <Section
        title={t("sensitiveWords.section.title")}
        description={t("sensitiveWords.section.description")}
        icon="shield-alert"
        iconColor="text-primary"
        variant="default"
        actions={
          <div className="flex gap-2">
            <SensitiveWordsRefreshAction />
            <AddWordDialog />
          </div>
        }
      >
        <SensitiveWordsTableContent />
      </Section>
    </UiSessionGate>
  );
}

function SensitiveWordsRefreshAction() {
  const tErrors = useTranslations("settings.errors");
  const statsQuery = useQuery({
    queryKey: ["sensitive-words", "cache-stats"],
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

function SensitiveWordsTableContent() {
  const tErrors = useTranslations("settings.errors");
  const wordsQuery = useQuery({
    queryKey: ["sensitive-words", "list"],
    queryFn: async () => {
      const result = await listSensitiveWords();
      if (!result.ok) throw new Error(result.error);
      return result.data as SensitiveWord[];
    },
  });

  if (wordsQuery.isLoading) return <SensitiveWordsTableSkeleton />;
  const words = wordsQuery.data;
  if (wordsQuery.isError || !words) {
    return (
      <QueryErrorState message={tErrors("fetchFailed")} onRetry={() => void wordsQuery.refetch()} />
    );
  }

  return <WordListTable words={words} />;
}
