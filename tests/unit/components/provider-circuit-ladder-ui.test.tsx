/**
 * @vitest-environment happy-dom
 *
 * 熔断「等待阶梯」（circuit backoff ladder）的**界面**契约。
 *
 * 公式（用户明示口径）：第 n 次连续熔断的窗口时长 = 基础时长 + 递增时长 × n（n 封顶最大次数）；
 * 首次开闸 n = 0；恢复（转 closed）即 n 归零。
 *
 * 本文件钉四件：
 *  1. **供应商列表徽标旁**：n > 0 时能看出「第几阶 / 本次窗口多长」；
 *  2. **n = 0（含阶梯未启用）不堆字**：首次开闸的窗口就是基础时长，多报一个数字只是噪声；
 *  3. **禁用供应商仍不显示**（既有行为，阶梯徽标跟同一开关）；
 *  4. **熔断日志弹窗**：报当前阶数与最近一次变化时间，并**如实说明历史不可回溯**。
 *
 * 范式与 provider-rich-list-item-circuit-badge.test.tsx 一致（createRoot + act + happy-dom）。
 */

import type { ReactNode } from "react";
import { act } from "react";
import { createRoot } from "react-dom/client";
import { afterEach, describe, expect, it, vi } from "vitest";

// next-intl：把 t() 变成「命名空间.键(参数)」的可断字符串，断言钉的是**用没用对键与参数**，
// 而不是某语种的文案（文案的齐全性由 tests/unit/i18n 的缺键钉子负责）。
vi.mock("next-intl", () => ({
  useLocale: () => "zh-CN",
  useTranslations: (namespace: string) => (key: string, params?: Record<string, unknown>) =>
    params ? `${namespace}.${key}(${JSON.stringify(params)})` : `${namespace}.${key}`,
}));

// react-query：由各用例替换返回值，避免真起 QueryClient。
// 默认返回一个空 envelope：列表项内含的并发徽标会解构 `data`，桩返回 undefined 会在渲染时崩。
const useQueryMock = vi.fn(() => ({ data: undefined, isLoading: false, isFetching: false }));
vi.mock("@tanstack/react-query", () => ({
  useQueryClient: () => ({ invalidateQueries: vi.fn() }),
  useQuery: (options: unknown) => useQueryMock(options),
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
  // 列表项现在内含并发徽标（它自带 useQuery，引用了这个导出）；本文件不钉徽标，给个空实现即可。
  getProvidersHealthStatus: vi.fn(async () => ({})),
}));

vi.mock("@/app/[locale]/settings/providers/_components/forms/provider-form", () => ({
  ProviderForm: () => <div data-testid="provider-form" />,
}));
vi.mock("@/app/[locale]/settings/providers/_components/provider-form-dialog-content", () => ({
  ProviderFormDialogContent: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
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
  DialogTrigger: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
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
vi.mock("@/components/ui/breadcrumb", () => ({
  Breadcrumb: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  BreadcrumbItem: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  BreadcrumbLink: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  BreadcrumbList: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  BreadcrumbPage: ({ children }: { children?: ReactNode }) => <div>{children}</div>,
  BreadcrumbSeparator: () => <div />,
}));

import { ProviderRichListItem } from "@/app/[locale]/settings/providers/_components/provider-rich-list-item";
import { ProviderCircuitLogsDialog } from "@/app/[locale]/settings/providers/_components/provider-circuit-logs-dialog";
import type { ProviderViewer } from "@/app/[locale]/settings/providers/_components/provider-viewer";
import type { ProviderCircuitHealth, ProviderDisplay } from "@/types/provider";

const adminViewer: ProviderViewer = { role: "admin", providerGroup: null };

const LADDER_BADGE = "settings.providers.list.ladder.badge";
const LADDER_BADGE_WINDOW = "settings.providers.list.ladder.badgeWithWindow";
const LADDER_LEVEL = "settings.providers.list.circuitLogs.ladder.level";
const LADDER_WINDOW = "settings.providers.list.circuitLogs.ladder.window";
const LADDER_EXPLANATION = "settings.providers.list.circuitLogs.ladder.explanation";

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

/** 熔断读数：默认是「阶梯未启用」（n=0、无窗口），用例按需覆盖。 */
function healthFixture(overrides: Partial<ProviderCircuitHealth> = {}): ProviderCircuitHealth {
  return {
    circuitState: "open",
    failureCount: 11,
    lastFailureTime: 1789265134080,
    circuitOpenUntil: 1789266934080,
    recoveryMinutes: 30,
    consecutiveOpenCount: 0,
    consecutiveOpenCountChangedAt: null,
    openWindowMinutes: null,
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

describe("熔断等待阶梯：列表徽标", () => {
  afterEach(() => {
    while (document.body.firstChild) document.body.removeChild(document.body.firstChild);
  });

  it("第 3 阶且已知窗口时长：徽标带出「阶数 + 本次窗口分钟数」", () => {
    const unmount = render(
      <ProviderRichListItem
        provider={providerFixture()}
        healthStatus={healthFixture({ consecutiveOpenCount: 3, openWindowMinutes: 35 })}
        enableMultiProviderTypes={false}
      />
    );
    const rendered = text();
    expect(rendered).toContain(LADDER_BADGE_WINDOW);
    // 参数必须落在文案里：只断言键名会让「参数传错」悄悄溜过去。
    expect(rendered).toContain('"level":3');
    expect(rendered).toContain('"minutes":35');
    unmount();
  });

  it("阶数 > 0 但服务端未给窗口时长：退化为只报阶数（不编一个数字）", () => {
    const unmount = render(
      <ProviderRichListItem
        provider={providerFixture()}
        healthStatus={healthFixture({ consecutiveOpenCount: 2, openWindowMinutes: null })}
        enableMultiProviderTypes={false}
      />
    );
    const rendered = text();
    expect(rendered).toContain(LADDER_BADGE);
    expect(rendered).toContain('"level":2');
    expect(rendered).not.toContain(LADDER_BADGE_WINDOW);
    unmount();
  });

  it("阶梯未启用（n = 0）：不渲染阶梯徽标", () => {
    const unmount = render(
      <ProviderRichListItem
        provider={providerFixture()}
        healthStatus={healthFixture({ consecutiveOpenCount: 0, openWindowMinutes: null })}
        enableMultiProviderTypes={false}
      />
    );
    const rendered = text();
    expect(rendered).not.toContain(LADDER_BADGE);
    expect(rendered).not.toContain(LADDER_BADGE_WINDOW);
    unmount();
  });

  it("首次开闸（n = 0）但服务端给了窗口：仍不渲染（n = 0 就是基础时长，是噪声）", () => {
    const unmount = render(
      <ProviderRichListItem
        provider={providerFixture()}
        healthStatus={healthFixture({ consecutiveOpenCount: 0, openWindowMinutes: 30 })}
        enableMultiProviderTypes={false}
      />
    );
    expect(text()).not.toContain(LADDER_BADGE);
    unmount();
  });

  it("禁用的供应商：残留的阶数也不再显示阶梯徽标", () => {
    const unmount = render(
      <ProviderRichListItem
        provider={providerFixture({ isEnabled: false })}
        healthStatus={healthFixture({ consecutiveOpenCount: 3, openWindowMinutes: 35 })}
        enableMultiProviderTypes={false}
      />
    );
    const rendered = text();
    expect(rendered).not.toContain(LADDER_BADGE);
    expect(rendered).not.toContain(LADDER_BADGE_WINDOW);
    unmount();
  });
});

describe("熔断等待阶梯：熔断日志弹窗", () => {
  afterEach(() => {
    while (document.body.firstChild) document.body.removeChild(document.body.firstChild);
  });

  function logsPayload(overrides: {
    consecutiveOpenCount?: number | null;
    openWindowMinutes?: number | null;
    consecutiveOpenCountChangedAt?: number | null;
  }): Record<string, unknown> {
    return {
      providerId: 156,
      circuit: {
        available: true,
        circuitState: "open",
        failureCount: 21,
        lastFailureTime: 1789265134080,
        circuitOpenUntil: 1789266934080,
        halfOpenSuccessCount: 0,
        recoveryMinutes: 30,
        consecutiveOpenCount: overrides.consecutiveOpenCount ?? 3,
        consecutiveOpenCountChangedAt: overrides.consecutiveOpenCountChangedAt ?? 1789265134080,
        openWindowMinutes: overrides.openWindowMinutes ?? 35,
        unavailableReason: null,
      },
      thresholds: { failureThreshold: 5, openDuration: 300000, halfOpenSuccessThreshold: 2 },
      window: { limit: 20, lookbackHours: 24, since: "2026-09-13T00:00:00.000Z" },
      errors: [],
      errorsUnavailableReason: null,
    };
  }

  // 弹窗自己发 query；这里用真 useQuery 的替身直接回数据，断的是渲染。
  function renderDialogWith(payload: Record<string, unknown>) {
    useQueryMock.mockReturnValue({
      data: payload,
      isLoading: false,
      isFetching: false,
      refetch: vi.fn(),
    });
    return render(
      <ProviderCircuitLogsDialog providerId={156} providerName="HC_Chat">
        <button type="button">open</button>
      </ProviderCircuitLogsDialog>
    );
  }

  it("阶数 > 0：显示当前阶数、本次窗口，并说明历史不可回溯", () => {
    const unmount = renderDialogWith(logsPayload({}));
    const rendered = text();
    expect(rendered).toContain(LADDER_LEVEL);
    expect(rendered).toContain(LADDER_WINDOW);
    // 历史不可回溯这句必须在：否则界面会被误读成「这里有完整时间线」。
    expect(rendered).toContain(LADDER_EXPLANATION);
    unmount();
  });

  it("阶数为 0：不展开阶梯块", () => {
    const unmount = renderDialogWith(
      logsPayload({ consecutiveOpenCount: 0, openWindowMinutes: null })
    );
    const rendered = text();
    expect(rendered).not.toContain(LADDER_LEVEL);
    expect(rendered).not.toContain(LADDER_EXPLANATION);
    unmount();
  });
});
