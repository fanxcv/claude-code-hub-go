/**
 * @vitest-environment happy-dom
 *
 * `provider-rich-list-item` 的**实时并发数徽标**渲染契约。
 *
 * 这个徽标的数据来自全局开关（默认关）门控的统计：关闭时服务端不报数、本页也不轮询。
 * 故徽标有四态，其中三态**都不该占位**——写错的代价是管理员看到「并发 0」并以为渠道空闲，
 * 而真相是「根本没开统计」（与熔断/降权徽标同一类误导，见本目录另一支测试的说明）。
 *
 * 每条「不显示」都配一条「显示」的对照：只断言不显示的话，把整个徽标删掉用例也会绿。
 *
 * 范式沿用仓库既有做法（本仓不用 testing-library）：createRoot + act + happy-dom。
 */

import type { ReactNode } from "react";
import { act } from "react";
import { createRoot } from "react-dom/client";
import { afterEach, describe, expect, it, vi } from "vitest";

vi.mock("next-intl", () => ({
  useLocale: () => "zh-CN",
  useTranslations: (namespace: string) => (key: string, params?: Record<string, unknown>) =>
    params ? `${namespace}.${key}(${JSON.stringify(params)})` : `${namespace}.${key}`,
}));

vi.mock("@tanstack/react-query", () => ({
  useQueryClient: () => ({ invalidateQueries: vi.fn() }),
}));

vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn() },
}));

vi.mock("@/lib/api-client/v1/actions/providers", () => ({
  editProvider: vi.fn(),
  getUnmaskedProviderKey: vi.fn(),
  removeProvider: vi.fn(),
  resetProviderCircuit: vi.fn(),
  resetProviderTotalUsage: vi.fn(),
  undoProviderDelete: vi.fn(),
}));

vi.mock("@/app/[locale]/settings/providers/_components/forms/provider-form", () => ({
  ProviderForm: () => <div data-testid="provider-form" />,
}));
vi.mock("@/app/[locale]/settings/providers/_components/provider-form-dialog-content", () => ({
  ProviderFormDialogContent: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
}));
vi.mock("@/app/[locale]/settings/providers/_components/provider-circuit-logs-dialog", () => ({
  ProviderCircuitLogsDialog: () => <div data-testid="circuit-logs" />,
}));
vi.mock("@/app/[locale]/settings/providers/_components/provider-endpoint-hover", () => ({
  ProviderEndpointHover: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
}));
vi.mock("@/app/[locale]/settings/providers/_components/inline-edit-popover", () => ({
  InlineEditPopover: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
}));
vi.mock("@/app/[locale]/settings/providers/_components/priority-edit-popover", () => ({
  PriorityEditPopover: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
}));
vi.mock("@/app/[locale]/settings/providers/_components/group-edit-combobox", () => ({
  GroupEditCombobox: () => <div data-testid="group-edit" />,
}));
vi.mock("@/app/[locale]/settings/providers/_components/invalidate-provider-queries", () => ({
  invalidateProviderQueries: vi.fn(),
}));
vi.mock("@/components/ui/switch", () => ({
  Switch: ({ checked }: { checked?: boolean }) => (
    <input type="checkbox" role="switch" checked={checked} readOnly />
  ),
}));
vi.mock("@/components/ui/tooltip", () => ({
  Tooltip: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  TooltipContent: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  TooltipTrigger: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
}));
vi.mock("@/components/ui/dropdown-menu", () => ({
  DropdownMenu: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  DropdownMenuContent: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  DropdownMenuItem: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  DropdownMenuSeparator: () => <div />,
  DropdownMenuTrigger: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
}));
vi.mock("@/components/ui/dialog", () => ({
  Dialog: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  DialogContent: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  DialogDescription: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  DialogHeader: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  DialogTitle: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
}));
vi.mock("@/components/ui/alert-dialog", () => ({
  AlertDialog: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  AlertDialogAction: ({ children }: { children?: ReactNode }) => <button>{children}</button>,
  AlertDialogCancel: ({ children }: { children?: ReactNode }) => <button>{children}</button>,
  AlertDialogContent: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  AlertDialogDescription: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  AlertDialogHeader: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  AlertDialogTitle: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  AlertDialogTrigger: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
}));
vi.mock("@/components/ui/checkbox", () => ({
  Checkbox: ({ checked }: { checked?: boolean }) => (
    <input type="checkbox" checked={checked} readOnly />
  ),
}));
vi.mock("@/components/ui/badge", () => ({
  Badge: ({ children }: { children?: ReactNode }) => <span>{children}</span>,
}));

import { ProviderRichListItem } from "@/app/[locale]/settings/providers/_components/provider-rich-list-item";
import type { ProviderViewer } from "@/app/[locale]/settings/providers/_components/provider-viewer";
import type { ProviderCircuitHealth, ProviderDisplay } from "@/types/provider";

const adminViewer: ProviderViewer = { role: "admin", providerGroup: null };

function providerFixture(overrides: Partial<ProviderDisplay> = {}): ProviderDisplay {
  return {
    id: 156,
    name: "HC_Chat",
    url: "https://upstream.example",
    maskedKey: "sk-****",
    isEnabled: true,
    weight: 1,
    priority: 0,
    groupPriorities: null,
    costMultiplier: 1,
    groupTag: "chat",
    providerType: "claude",
    providerVendorId: 1,
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
    limit5hResetMode: "fixed",
    limitDailyUsd: null,
    dailyResetMode: "fixed",
    dailyResetTime: "00:00",
    limitWeeklyUsd: null,
    limitMonthlyUsd: null,
    limitTotalUsd: null,
    limitConcurrentSessions: 0,
    ...overrides,
  } as ProviderDisplay;
}

function healthFixture(overrides: Partial<ProviderCircuitHealth> = {}): ProviderCircuitHealth {
  return {
    circuitState: "closed",
    failureCount: 0,
    lastFailureTime: null,
    circuitOpenUntil: null,
    halfOpenSuccessCount: 0,
    recoveryMinutes: null,
    ...overrides,
  } as ProviderCircuitHealth;
}

function render(node: ReactNode) {
  const container = document.createElement("div");
  document.body.appendChild(container);
  const root = createRoot(container);
  act(() => root.render(node));
  return () => {
    act(() => root.unmount());
    container.remove();
  };
}

function text(): string {
  return document.body.textContent ?? "";
}

const NS = "settings.providers.list.concurrency";

describe("ProviderRichListItem 的实时并发徽标", () => {
  afterEach(() => {
    while (document.body.firstChild) document.body.removeChild(document.body.firstChild);
  });

  it("统计开启且读到数：显示并发数", () => {
    const unmount = render(
      <ProviderRichListItem
        provider={providerFixture()}
        currentUser={adminViewer}
        healthStatus={healthFixture({
          concurrency: { trackingEnabled: true, available: true, activeSessions: 7 },
        })}
        enableMultiProviderTypes={false}
      />
    );
    expect(text()).toContain(`${NS}.badge`);
    expect(text()).toContain('"count":7');
    unmount();
  });

  it("统计开启且读到 0：仍显示（0 是有效读数，不是「没有」）", () => {
    const unmount = render(
      <ProviderRichListItem
        provider={providerFixture()}
        currentUser={adminViewer}
        healthStatus={healthFixture({
          concurrency: { trackingEnabled: true, available: true, activeSessions: 0 },
        })}
        enableMultiProviderTypes={false}
      />
    );
    expect(text()).toContain(`${NS}.badge`);
    expect(text()).toContain('"count":0');
    unmount();
  });

  it("统计关闭（trackingEnabled=false）：不显示", () => {
    const unmount = render(
      <ProviderRichListItem
        provider={providerFixture()}
        currentUser={adminViewer}
        healthStatus={healthFixture({
          // 服务端在关闭时不会给 activeSessions；这里刻意给一个值，钉住判据不看它。
          concurrency: { trackingEnabled: false, available: true, activeSessions: 9 },
        })}
        enableMultiProviderTypes={false}
      />
    );
    expect(text()).not.toContain(`${NS}.badge`);
    unmount();
  });

  it("统计开着但读不到（available=false）：不显示 0 冒充", () => {
    const unmount = render(
      <ProviderRichListItem
        provider={providerFixture()}
        currentUser={adminViewer}
        healthStatus={healthFixture({
          concurrency: {
            trackingEnabled: true,
            available: false,
            activeSessions: null,
            unavailableReason: "redis_unavailable",
          },
        })}
        enableMultiProviderTypes={false}
      />
    );
    expect(text()).not.toContain(`${NS}.badge`);
    unmount();
  });

  it("未装配（concurrency 为 null/缺省）：不显示", () => {
    const unmount = render(
      <ProviderRichListItem
        provider={providerFixture()}
        currentUser={adminViewer}
        healthStatus={healthFixture({ concurrency: null })}
        enableMultiProviderTypes={false}
      />
    );
    expect(text()).not.toContain(`${NS}.badge`);
    unmount();
  });

  it("禁用的渠道：即使统计开着也不显示（与熔断/降权同一纪律）", () => {
    const unmount = render(
      <ProviderRichListItem
        provider={providerFixture({ isEnabled: false })}
        currentUser={adminViewer}
        healthStatus={healthFixture({
          concurrency: { trackingEnabled: true, available: true, activeSessions: 3 },
        })}
        enableMultiProviderTypes={false}
      />
    );
    expect(text()).not.toContain(`${NS}.badge`);
    unmount();
  });

  it("设了并发上限：显示「分子/上限」", () => {
    const unmount = render(
      <ProviderRichListItem
        provider={providerFixture({ limitConcurrentSessions: 20 })}
        currentUser={adminViewer}
        healthStatus={healthFixture({
          concurrency: { trackingEnabled: true, available: true, activeSessions: 5 },
        })}
        enableMultiProviderTypes={false}
      />
    );
    expect(text()).toContain(`${NS}.badgeWithLimit`);
    expect(text()).toContain('"count":5');
    expect(text()).toContain('"limit":20');
    unmount();
  });

  it("未设并发上限（0）：只显示分子，不显示一个假的「/0」", () => {
    const unmount = render(
      <ProviderRichListItem
        provider={providerFixture({ limitConcurrentSessions: 0 })}
        currentUser={adminViewer}
        healthStatus={healthFixture({
          concurrency: { trackingEnabled: true, available: true, activeSessions: 5 },
        })}
        enableMultiProviderTypes={false}
      />
    );
    expect(text()).toContain(`${NS}.badge`);
    expect(text()).not.toContain(`${NS}.badgeWithLimit`);
    unmount();
  });
});
