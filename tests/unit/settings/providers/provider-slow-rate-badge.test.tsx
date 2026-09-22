/**
 * @vitest-environment happy-dom
 */

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { NextIntlClientProvider } from "next-intl";
import { type ReactNode, act } from "react";
import { createRoot } from "react-dom/client";
import { beforeEach, describe, expect, test, vi } from "vitest";
import { ProviderRichListItem } from "@/app/[locale]/settings/providers/_components/provider-rich-list-item";
import type { ProviderCircuitHealth, ProviderDisplay } from "@/types/provider";
import type { User } from "@/types/user";
import enMessages from "../../../../messages/en";

vi.mock("next/navigation", () => ({
  useRouter: () => ({ refresh: vi.fn() }),
}));

vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn() },
}));

vi.mock("@/lib/api-client/v1/actions/provider-endpoints", () => ({
  getProviderVendors: vi.fn(async () => []),
  getProviderEndpointsByVendor: vi.fn(async () => []),
}));

vi.mock("@/lib/api-client/v1/actions/providers", () => ({
  editProvider: vi.fn(async () => ({ ok: true })),
  removeProvider: vi.fn(async () => ({ ok: true })),
  getUnmaskedProviderKey: vi.fn(async () => ({ ok: true, data: { key: "sk-test" } })),
  resetProviderCircuit: vi.fn(async () => ({ ok: true })),
  resetProviderTotalUsage: vi.fn(async () => ({ ok: true })),
  // 并发徽标引用了这个导出（它自带 useQuery）；本文件不钉徽标，给个空实现即可。
  getProvidersHealthStatus: vi.fn(async () => ({})),
}));

vi.mock("@/components/ui/tooltip", () => ({
  Tooltip: ({ children }: { children: ReactNode }) => <>{children}</>,
  TooltipTrigger: ({ children }: { children: ReactNode }) => <>{children}</>,
  TooltipContent: ({ children }: { children: ReactNode }) => <div>{children}</div>,
  TooltipProvider: ({ children }: { children: ReactNode }) => <>{children}</>,
}));

vi.mock("@/app/[locale]/settings/providers/_components/provider-endpoint-hover", () => ({
  ProviderEndpointHover: () => null,
}));

const ADMIN_USER: User = {
  id: 1,
  name: "admin",
  description: "",
  role: "admin",
  rpm: null,
  dailyQuota: null,
  providerGroup: null,
  tags: [],
  createdAt: new Date("2026-01-01"),
  updatedAt: new Date("2026-01-01"),
  dailyResetMode: "fixed",
  dailyResetTime: "00:00",
  isEnabled: true,
};

function makeProviderDisplay(overrides: Partial<ProviderDisplay> = {}): ProviderDisplay {
  return {
    id: 1,
    name: "Claude 3.5 Sonnet",
    url: "https://api.anthropic.com",
    maskedKey: "sk-***",
    isEnabled: true,
    weight: 1,
    priority: 1,
    costMultiplier: 1,
    groupTag: null,
    providerType: "claude",
    providerVendorId: null,
    preserveClientIp: false,
    disableSessionReuse: false,
    modelRedirects: null,
    activeTimeStart: null,
    activeTimeEnd: null,
    allowedModels: null,
    allowedClients: [],
    blockedClients: [],
    mcpPassthroughType: "none",
    mcpPassthroughUrl: null,
    protocolConversionEnabled: false,
    limit5hUsd: null,
    limitDailyUsd: null,
    dailyResetMode: "fixed",
    dailyResetTime: "00:00",
    limitWeeklyUsd: null,
    limitMonthlyUsd: null,
    limitTotalUsd: null,
    limitConcurrentSessions: 1,
    maxRetryAttempts: null,
    circuitBreakerFailureThreshold: 1,
    circuitBreakerOpenDuration: 60,
    circuitBreakerHalfOpenSuccessThreshold: 1,
    proxyUrl: null,
    proxyFallbackToDirect: false,
    firstByteTimeoutStreamingMs: 0,
    streamingIdleTimeoutMs: 0,
    requestTimeoutNonStreamingMs: 0,
    websiteUrl: null,
    faviconUrl: null,
    cacheTtlPreference: null,
    swapCacheTtlBilling: false,
    context1mPreference: null,
    codexReasoningEffortPreference: null,
    codexReasoningSummaryPreference: null,
    codexTextVerbosityPreference: null,
    codexParallelToolCallsPreference: null,
    codexImageGenerationPreference: null,
    codexServiceTierPreference: null,
    codexMaxTokensPreference: null,
    anthropicMaxTokensPreference: null,
    anthropicThinkingBudgetPreference: null,
    anthropicAdaptiveThinking: null,
    openaiMaxTokensPreference: null,
    geminiGoogleSearchPreference: null,
    slowRateMonitorEnabled: true,
    slowRateWindowSeconds: null,
    slowRateMinSamples: null,
    slowRateTriggerCount: null,
    slowRateRatioPerMille: null,
    slowRatePenaltyStep: null,
    slowRatePenaltyMax: null,
    tpm: null,
    rpm: null,
    rpd: null,
    cc: null,
    createdAt: "2026-01-01",
    updatedAt: "2026-01-01",
    ...overrides,
  };
}

function makeHealth(overrides: Partial<ProviderCircuitHealth> = {}): ProviderCircuitHealth {
  return {
    circuitState: "closed",
    failureCount: 0,
    lastFailureTime: null,
    circuitOpenUntil: null,
    recoveryMinutes: null,
    consecutiveOpenCount: 0,
    consecutiveOpenCountChangedAt: null,
    openWindowMinutes: null,
    slowRate: { available: true, penalty: null, modelKey: null, combinations: 0 },
    ...overrides,
  };
}

let queryClient: QueryClient;

function renderWithProviders(node: ReactNode) {
  const container = document.createElement("div");
  document.body.appendChild(container);
  const root = createRoot(container);

  act(() => {
    root.render(
      <QueryClientProvider client={queryClient}>
        <NextIntlClientProvider locale="en" messages={enMessages} timeZone="UTC">
          {node}
        </NextIntlClientProvider>
      </QueryClientProvider>
    );
  });

  return {
    unmount: () => {
      act(() => root.unmount());
      container.remove();
    },
    container,
  };
}

async function flushTicks(times = 5) {
  for (let i = 0; i < times; i++) {
    await act(async () => {
      await new Promise((r) => setTimeout(r, 0));
    });
  }
}

/** 找出低速降权徽章：带 title 的 badge 且文案以「Slow demoted」开头。 */
function findSlowRateBadges(): Element[] {
  return Array.from(document.querySelectorAll('span[data-slot="badge"]')).filter((el) =>
    el.textContent?.trim().startsWith("Slow demoted")
  );
}

const EXPECTED_TOOLTIP = enMessages.settings.providers.list.slowRate.tooltip;

describe("ProviderRichListItem 低速降权徽章", () => {
  beforeEach(() => {
    queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    vi.clearAllMocks();
    while (document.body.firstChild) {
      document.body.removeChild(document.body.firstChild);
    }
  });

  test("有降权时，移动端与桌面端各渲染一个徽章并带读数与模型键", async () => {
    const { unmount } = renderWithProviders(
      <ProviderRichListItem
        provider={makeProviderDisplay({ isEnabled: true })}
        currentUser={ADMIN_USER}
        enableMultiProviderTypes={true}
        healthStatus={makeHealth({
          slowRate: { available: true, penalty: 40, modelKey: "model-a", combinations: 1 },
        })}
      />
    );

    await flushTicks();

    const badges = findSlowRateBadges();
    expect(badges).toHaveLength(2);
    for (const badge of badges) {
      expect(badge.textContent?.trim()).toBe("Slow demoted +40 (model-a)");
      expect(badge.getAttribute("title")).toBe(EXPECTED_TOOLTIP);
    }

    unmount();
  });

  test("无降权（penalty=null）时不渲染徽章", async () => {
    const { unmount } = renderWithProviders(
      <ProviderRichListItem
        provider={makeProviderDisplay({ isEnabled: true })}
        currentUser={ADMIN_USER}
        enableMultiProviderTypes={true}
        healthStatus={makeHealth({
          slowRate: { available: true, penalty: null, modelKey: null, combinations: 0 },
        })}
      />
    );

    await flushTicks();

    expect(findSlowRateBadges()).toHaveLength(0);

    unmount();
  });

  test("读不到（available=false）时不渲染徽章，且不把读不到显示成 0", async () => {
    const { unmount } = renderWithProviders(
      <ProviderRichListItem
        provider={makeProviderDisplay({ isEnabled: true })}
        currentUser={ADMIN_USER}
        enableMultiProviderTypes={true}
        healthStatus={makeHealth({
          slowRate: {
            available: false,
            penalty: null,
            modelKey: null,
            combinations: 0,
            unavailableReason: "redis_unavailable",
          },
        })}
      />
    );

    await flushTicks();

    expect(findSlowRateBadges()).toHaveLength(0);
    expect(document.body.textContent).not.toContain("Slow demoted +0");

    unmount();
  });

  test("渠道被禁用时不渲染徽章（读数是残留，禁用渠道不参与选路）", async () => {
    const { unmount } = renderWithProviders(
      <ProviderRichListItem
        provider={makeProviderDisplay({ isEnabled: false })}
        currentUser={ADMIN_USER}
        enableMultiProviderTypes={true}
        healthStatus={makeHealth({
          slowRate: { available: true, penalty: 40, modelKey: "model-a", combinations: 1 },
        })}
      />
    );

    await flushTicks();

    expect(findSlowRateBadges()).toHaveLength(0);

    unmount();
  });

  test("多组合时如实说「还有 N 个模型」", async () => {
    const { unmount } = renderWithProviders(
      <ProviderRichListItem
        provider={makeProviderDisplay({ isEnabled: true })}
        currentUser={ADMIN_USER}
        enableMultiProviderTypes={true}
        healthStatus={makeHealth({
          slowRate: { available: true, penalty: 30, modelKey: "global:model-b", combinations: 3 },
        })}
      />
    );

    await flushTicks();

    const badges = findSlowRateBadges();
    expect(badges).toHaveLength(2);
    for (const badge of badges) {
      expect(badge.textContent?.trim()).toBe("Slow demoted +30 (global:model-b and 3 models)");
    }

    unmount();
  });
});
