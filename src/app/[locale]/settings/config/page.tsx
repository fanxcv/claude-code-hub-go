"use client";

import { useQuery } from "@tanstack/react-query";
import { useTranslations } from "next-intl";
import { Section } from "@/components/section";
import { QueryErrorState } from "@/components/ui/query-error-state";
import { UiSessionGate } from "@/components/ui-session-gate";
import { getSystemSettings } from "@/lib/api-client/v1/actions/system-config";
import { SettingsPageHeader } from "../_components/settings-page-header";
import { AutoCleanupForm } from "./_components/auto-cleanup-form";
import { SettingsConfigSkeleton } from "./_components/settings-config-skeleton";
import { SystemSettingsForm } from "./_components/system-settings-form";

/**
 * 站点参数设置。
 *
 * 改造前服务端读 `getSystemSettings()` 与 `getEnvConfig()`；现在设置改由客户端拉取
 * （`/api/v1/system/settings`，与 `useSystemSettings` 同一端点）。
 *
 * 两点已知差异（读取 `getEnvConfig()` 的只读运行态值，客户端拿不到）：
 * - `sessionTtlSeconds`（env `SESSION_TTL`）与 `replayDefaultEnabled`（env `ENABLE_REQUEST_REPLAY`）
 *   不再透传，表单回落到其自带默认值。这两个值只影响**展示**：`replayEnabled` 未触碰时仍按
 *   `null`（跟随环境变量）保存，不会把覆写写死。建议后续波把两者并入 Go 壳注入的
 *   `__CCH_BOOTSTRAP__`，或在 `/api/v1/system/settings` 增加两个只读字段。
 */
export default function SettingsConfigPage() {
  const t = useTranslations("settings");

  return (
    <UiSessionGate requireRole="admin">
      <SettingsPageHeader
        title={t("config.title")}
        description={t("config.description")}
        icon="settings"
      />
      <SettingsConfigContent />
    </UiSessionGate>
  );
}

function SettingsConfigContent() {
  const t = useTranslations("settings");
  const tErrors = useTranslations("settings.errors");
  const settingsQuery = useQuery({
    queryKey: ["system-settings"],
    queryFn: getSystemSettings,
  });

  if (settingsQuery.isLoading) return <SettingsConfigSkeleton />;
  if (settingsQuery.isError || !settingsQuery.data) {
    return (
      <QueryErrorState
        message={tErrors("fetchFailed")}
        onRetry={() => void settingsQuery.refetch()}
      />
    );
  }

  const settings = settingsQuery.data;

  return (
    <>
      <Section
        title={t("config.section.siteParams.title")}
        description={t("config.section.siteParams.description")}
        icon="settings"
        variant="default"
      >
        <SystemSettingsForm
          initialSettings={{
            siteTitle: settings.siteTitle,
            allowGlobalUsageView: settings.allowGlobalUsageView,
            currencyDisplay: settings.currencyDisplay,
            billingModelSource: settings.billingModelSource,
            codexPriorityBillingSource: settings.codexPriorityBillingSource,
            billNonSuccessfulRequests: settings.billNonSuccessfulRequests,
            billHedgeLosers: settings.billHedgeLosers,
            legacyHedgeMaxInFlight: settings.legacyHedgeMaxInFlight,
            discoveryEnabled: settings.discoveryEnabled,
            discoveryConcurrency: settings.discoveryConcurrency,
            maxDiscoveryRounds: settings.maxDiscoveryRounds,
            discoverySlaMs: settings.discoverySlaMs,
            stickySlaMs: settings.stickySlaMs,
            racingTotalTimeoutMs: settings.racingTotalTimeoutMs,
            stickyTimeoutCooldownMs: settings.stickyTimeoutCooldownMs,
            timezone: settings.timezone,
            verboseProviderError: settings.verboseProviderError,
            passThroughUpstreamErrorMessage: settings.passThroughUpstreamErrorMessage,
            enableHttp2: settings.enableHttp2,
            enableOpenaiResponsesWebsocket: settings.enableOpenaiResponsesWebsocket,
            enableHighConcurrencyMode: settings.enableHighConcurrencyMode,
            interceptAnthropicWarmupRequests: settings.interceptAnthropicWarmupRequests,
            enableThinkingSignatureRectifier: settings.enableThinkingSignatureRectifier,
            enableThinkingBudgetRectifier: settings.enableThinkingBudgetRectifier,
            enableThinkingEffortConflictRectifier: settings.enableThinkingEffortConflictRectifier,
            enableGeminiFunctionIdRectifier: settings.enableGeminiFunctionIdRectifier,
            enableBillingHeaderRectifier: settings.enableBillingHeaderRectifier,
            enableResponseInputRectifier: settings.enableResponseInputRectifier,
            allowNonConversationEndpointProviderFallback:
              settings.allowNonConversationEndpointProviderFallback,
            fakeStreamingWhitelist: settings.fakeStreamingWhitelist,
            streamGateMode: settings.streamGateMode,
            affinityIgnoreClientSessionId: settings.affinityIgnoreClientSessionId,
            replayEnabled: settings.replayEnabled,
            replayCacheTtlMinutes: settings.replayCacheTtlMinutes,
            cacheEffectivenessEnabled: settings.cacheEffectivenessEnabled,
            enableCodexSessionIdCompletion: settings.enableCodexSessionIdCompletion,
            enableClaudeMetadataUserIdInjection: settings.enableClaudeMetadataUserIdInjection,
            enableResponseFixer: settings.enableResponseFixer,
            responseFixerConfig: settings.responseFixerConfig,
            quotaDbRefreshIntervalSeconds: settings.quotaDbRefreshIntervalSeconds,
            quotaLeasePercent5h: settings.quotaLeasePercent5h,
            quotaLeasePercentDaily: settings.quotaLeasePercentDaily,
            quotaLeasePercentWeekly: settings.quotaLeasePercentWeekly,
            quotaLeasePercentMonthly: settings.quotaLeasePercentMonthly,
            quotaLeaseCapUsd: settings.quotaLeaseCapUsd,
            ipGeoLookupEnabled: settings.ipGeoLookupEnabled,
            ipExtractionConfig: settings.ipExtractionConfig,
          }}
        />
      </Section>

      <Section
        title={t("config.section.autoCleanup.title")}
        description={t("config.section.autoCleanup.description")}
        icon="trash"
        iconColor="text-red-400"
        variant="default"
      >
        <AutoCleanupForm settings={settings} />
      </Section>
    </>
  );
}
