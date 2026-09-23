import fs from "node:fs";
import path from "node:path";
import type { ReactNode } from "react";
import { act } from "react";
import { createRoot } from "react-dom/client";
import { NextIntlClientProvider } from "next-intl";
import { beforeEach, describe, expect, test, vi } from "vitest";
import { SystemSettingsForm } from "@/app/[locale]/settings/config/_components/system-settings-form";
import type { SystemSettings } from "@/types/system-config";

vi.mock("next/navigation", () => ({
  useRouter: () => ({ refresh: vi.fn() }),
}));

const systemConfigActionMocks = vi.hoisted(() => ({
  saveSystemSettings: vi.fn(async () => ({ ok: true })),
}));
vi.mock("@/lib/api-client/v1/actions/system-config", () => systemConfigActionMocks);

const sonnerMocks = vi.hoisted(() => ({
  toast: {
    success: vi.fn(),
    error: vi.fn(),
    warning: vi.fn(),
  },
}));
vi.mock("sonner", () => sonnerMocks);

const baseSettings = {
  siteTitle: "CC Hub",
  allowGlobalUsageView: true,
  currencyDisplay: "USD",
  billingModelSource: "original",
  codexPriorityBillingSource: "requested",
  timezone: "UTC",
  verboseProviderError: false,
  enableHttp2: true,
  enableHighConcurrencyMode: false,
  interceptAnthropicWarmupRequests: false,
  enableThinkingSignatureRectifier: true,
  enableThinkingBudgetRectifier: true,
  enableBillingHeaderRectifier: true,
  enableResponseInputRectifier: true,
  enableCodexSessionIdCompletion: true,
  enableClaudeMetadataUserIdInjection: true,
  enableResponseFixer: true,
  allowNonConversationEndpointProviderFallback: true,
  fakeStreamingWhitelist: [],
  streamGateMode: "enforce",
  responseFixerConfig: {
    fixEncoding: true,
    fixSseFormat: true,
    fixTruncatedJson: true,
  },
  quotaDbRefreshIntervalSeconds: 10,
  quotaLeasePercent5h: 0.05,
  quotaLeasePercentDaily: 0.05,
  quotaLeasePercentWeekly: 0.05,
  quotaLeasePercentMonthly: 0.05,
  quotaLeaseCapUsd: null,
  ipGeoLookupEnabled: true,
  ipExtractionConfig: null,
} satisfies Pick<
  SystemSettings,
  | "siteTitle"
  | "allowGlobalUsageView"
  | "currencyDisplay"
  | "billingModelSource"
  | "codexPriorityBillingSource"
  | "timezone"
  | "verboseProviderError"
  | "enableHttp2"
  | "enableHighConcurrencyMode"
  | "interceptAnthropicWarmupRequests"
  | "enableThinkingSignatureRectifier"
  | "enableThinkingBudgetRectifier"
  | "enableBillingHeaderRectifier"
  | "enableResponseInputRectifier"
  | "enableCodexSessionIdCompletion"
  | "enableClaudeMetadataUserIdInjection"
  | "enableResponseFixer"
  | "allowNonConversationEndpointProviderFallback"
  | "fakeStreamingWhitelist"
  | "streamGateMode"
  | "responseFixerConfig"
  | "quotaDbRefreshIntervalSeconds"
  | "quotaLeasePercent5h"
  | "quotaLeasePercentDaily"
  | "quotaLeasePercentWeekly"
  | "quotaLeasePercentMonthly"
  | "quotaLeaseCapUsd"
  | "ipGeoLookupEnabled"
  | "ipExtractionConfig"
>;

function loadMessages(locale: string) {
  const base = path.join(process.cwd(), `messages/${locale}/settings`);
  const read = (name: string) => JSON.parse(fs.readFileSync(path.join(base, name), "utf8"));

  return {
    settings: {
      common: read("common.json"),
      config: read("config.json"),
    },
  };
}

function render(node: ReactNode) {
  const container = document.createElement("div");
  document.body.appendChild(container);
  const root = createRoot(container);

  act(() => {
    root.render(
      <NextIntlClientProvider locale="en" messages={loadMessages("en")} timeZone="UTC">
        {node}
      </NextIntlClientProvider>
    );
  });

  return {
    unmount: () => {
      act(() => root.unmount());
      container.remove();
    },
  };
}

async function submitForm() {
  const form = document.body.querySelector("form");
  if (!form) throw new Error("未找到系统设置表单");

  await act(async () => {
    form.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
    await Promise.resolve();
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
}

function clickStreamGateSwitch() {
  const trigger = document.getElementById("stream-gate-mode") as HTMLButtonElement | null;
  if (!trigger) {
    throw new Error("未找到流式内容门控开关");
  }

  act(() => {
    trigger.dispatchEvent(new MouseEvent("click", { bubbles: true }));
  });
}

describe("SystemSettingsForm stream gate switch", () => {
  beforeEach(() => {
    document.body.innerHTML = "";
    vi.clearAllMocks();
  });

  test("switch off submits off", async () => {
    const { unmount } = render(<SystemSettingsForm initialSettings={baseSettings} />);

    clickStreamGateSwitch();
    await submitForm();

    expect(systemConfigActionMocks.saveSystemSettings).toHaveBeenCalledWith(
      expect.objectContaining({ streamGateMode: "off" })
    );

    unmount();
  });

  test("switch on submits enforce", async () => {
    const { unmount } = render(
      <SystemSettingsForm initialSettings={{ ...baseSettings, streamGateMode: "off" }} />
    );

    clickStreamGateSwitch();
    await submitForm();

    expect(systemConfigActionMocks.saveSystemSettings).toHaveBeenCalledWith(
      expect.objectContaining({ streamGateMode: "enforce" })
    );

    unmount();
  });

  test("all locales define gate switch labels and no shadow option", () => {
    for (const locale of ["zh-CN", "en"] as const) {
      const form = loadMessages(locale).settings.config.form;
      expect(form.streamGateMode).toBeTruthy();
      expect(form.streamGateModeDesc).toBeTruthy();
      expect(Object.keys(form.streamGateModeOptions).sort()).toEqual(["enforce", "off"]);
    }
  });
});
