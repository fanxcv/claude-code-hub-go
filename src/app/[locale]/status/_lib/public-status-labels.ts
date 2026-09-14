"use client";

import { useTranslations } from "next-intl";

/**
 * 公开状态页的文案表。
 *
 * 改造前 `/status` 与 `/status/[slug]` 两个服务端页各自把 `settings.statusPage.public.*`
 * 逐键摊成同一个 labels 对象（两份近乎逐字重复的四十行）。静态化后两页都要在客户端拿文案，
 * 抽成一个 hook 免得复制两份——键集合与原两页完全一致。
 */
export function usePublicStatusLabels() {
  const t = useTranslations("settings.statusPage.public");

  return {
    systemStatus: t("systemStatus"),
    heroPrimary: t("heroPrimary"),
    heroSecondary: t("heroSecondary"),
    generatedAt: t("generatedAt"),
    history: t("history"),
    availability: t("availability"),
    ttft: t("ttft"),
    freshnessWindow: t("freshnessWindow"),
    fresh: t("fresh"),
    stale: t("stale"),
    staleDetail: t("staleDetail"),
    rebuilding: t("rebuilding"),
    noSnapshot: t("noSnapshot"),
    noData: t("noData"),
    emptyDescription: t("emptyDescription"),
    requestTypes: {
      openaiCompatible: t("requestTypes.openaiCompatible"),
      codex: t("requestTypes.codex"),
      anthropic: t("requestTypes.anthropic"),
      gemini: t("requestTypes.gemini"),
    },
    statusBadge: {
      operational: t("statusBadge.operational"),
      degraded: t("statusBadge.degraded"),
      failed: t("statusBadge.failed"),
      noData: t("statusBadge.noData"),
    },
    tooltip: {
      availability: t("tooltip.availability"),
      ttft: t("tooltip.ttft"),
      tps: t("tooltip.tps"),
      historyAriaLabel: t("tooltip.historyAriaLabel"),
    },
    searchPlaceholder: t("searchPlaceholder"),
    customSort: t("customSort"),
    resetSort: t("resetSort"),
    emptyByFilter: t("emptyByFilter"),
    modelsLabel: t("modelsLabel"),
    issuesLabel: t("issuesLabel"),
    clearSearch: t("clearSearch"),
    dragHandle: t("dragHandle"),
    toggleGroup: t("toggleGroup"),
    openGroupPage: t("openGroupPage"),
  };
}
