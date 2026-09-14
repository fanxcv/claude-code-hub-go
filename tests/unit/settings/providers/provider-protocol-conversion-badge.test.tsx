/**
 * @vitest-environment happy-dom
 */

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { NextIntlClientProvider } from "next-intl";
import { type ReactNode, act } from "react";
import { createRoot } from "react-dom/client";
import { beforeEach, describe, expect, test, vi } from "vitest";
import { ProviderRichListItem } from "@/app/[locale]/settings/providers/_components/provider-rich-list-item";
import type { ProviderDisplay } from "@/types/provider";
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
    tpm: null,
    rpm: null,
    rpd: null,
    cc: null,
    createdAt: "2026-01-01",
    updatedAt: "2026-01-01",
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

async function flushTicks(times = 3) {
  for (let i = 0; i < times; i++) {
    await act(async () => {
      await new Promise((r) => setTimeout(r, 0));
    });
  }
}

/** 找出「协议转换」徽章：带 title 的 badge 且文案为指定值 */
function findProtocolConversionBadges(badgeText: string): Element[] {
  return Array.from(document.querySelectorAll('span[data-slot="badge"][title]')).filter(
    (el) => el.textContent?.trim() === badgeText
  );
}

const EXPECTED_BADGE_TEXT = "Protocol conversion";
const EXPECTED_TOOLTIP = enMessages.settings.providers.list.protocolConversion.tooltip;

describe("ProviderRichListItem 协议转换徽章", () => {
  beforeEach(() => {
    queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    vi.clearAllMocks();
    while (document.body.firstChild) {
      document.body.removeChild(document.body.firstChild);
    }
  });

  test("protocolConversionEnabled 为 true 时，移动端与桌面端各渲染一个协议转换徽章", async () => {
    const { unmount } = renderWithProviders(
      <ProviderRichListItem
        provider={makeProviderDisplay({ protocolConversionEnabled: true })}
        currentUser={ADMIN_USER}
        enableMultiProviderTypes={true}
      />
    );

    await flushTicks(5);

    const badges = findProtocolConversionBadges(EXPECTED_BADGE_TEXT);
    // 移动端 + 桌面端两处落点，窄屏与宽屏都能看到
    expect(badges).toHaveLength(2);
    for (const badge of badges) {
      expect(badge.getAttribute("title")).toBe(EXPECTED_TOOLTIP);
    }

    unmount();
  });

  test("protocolConversionEnabled 为 false 时不渲染任何协议转换徽章", async () => {
    const { unmount } = renderWithProviders(
      <ProviderRichListItem
        provider={makeProviderDisplay({ protocolConversionEnabled: false })}
        currentUser={ADMIN_USER}
        enableMultiProviderTypes={true}
      />
    );

    await flushTicks(5);

    expect(findProtocolConversionBadges(EXPECTED_BADGE_TEXT)).toHaveLength(0);
    expect(document.body.textContent).not.toContain(EXPECTED_BADGE_TEXT);

    unmount();
  });
});
