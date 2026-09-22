/**
 * @vitest-environment happy-dom
 *
 * `provider-rich-list-item` 的**实时并发数徽标**渲染与局部更新契约。
 *
 * 数据来自全局开关（默认关）门控的统计：关闭时服务端不报数、页面也不轮询。故徽标有四态，
 * 其中三态**都不该占位**——写错的代价是管理员看到「并发 0」并以为渠道空闲，而真相是
 * 「根本没开统计」（与熔断/降权徽标同一类误导，见本目录另一支测试的说明）。
 *
 * 每条「不显示」都配一条「显示」的对照：只断言不显示的话，把整个徽标删掉用例也会绿。
 *
 * **本文件另钉「局部更新」**：数据源从「列表项接收 healthStatus」改为「徽标自己订阅
 * `providers-health-live`」（改因见 provider-concurrency-badge.tsx）。故这里用**真** QueryClient，
 * 并用 `setQueryData` 模拟一次 5 秒轮询，断言：读数更新、且**所在行不重挂载**（DOM 节点同一个）、
 * 且不因此多发请求。范式沿用仓库既有做法（createRoot + act + happy-dom）。
 */

import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { act } from "react";
import { createRoot } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("next-intl", () => ({
  useLocale: () => "zh-CN",
  useTranslations: (namespace: string) => (key: string, params?: Record<string, unknown>) =>
    params ? `${namespace}.${key}(${JSON.stringify(params)})` : `${namespace}.${key}`,
}));

vi.mock("sonner", () => ({
  toast: { success: vi.fn(), error: vi.fn() },
}));

/** health 的返回值（可变）：每个用例自己摆。 */
let healthPayload: Record<number, unknown> = {};

const getProvidersHealthStatusMock = vi.fn(async () => healthPayload);

vi.mock("@/lib/api-client/v1/actions/providers", () => ({
  editProvider: vi.fn(),
  getUnmaskedProviderKey: vi.fn(),
  removeProvider: vi.fn(),
  resetProviderCircuit: vi.fn(),
  resetProviderTotalUsage: vi.fn(),
  undoProviderDelete: vi.fn(),
  getProvidersHealthStatus: () => getProvidersHealthStatusMock(),
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

let queryClient: QueryClient;

function render(node: ReactNode) {
  const container = document.createElement("div");
  document.body.appendChild(container);
  const root = createRoot(container);
  act(() => root.render(<QueryClientProvider client={queryClient}>{node}</QueryClientProvider>));
  return {
    container,
    unmount: () => {
      act(() => root.unmount());
      container.remove();
    },
  };
}

async function flushTicks(times = 5) {
  for (let index = 0; index < times; index++) {
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
  }
}

function text(): string {
  return document.body.textContent ?? "";
}

const NS = "settings.providers.list.concurrency";

/** 渲染列表项；`liveStatsEnabled` 显式给出（等价于页面级开关透传下来）。 */
function renderItem(options: {
  provider?: Partial<ProviderDisplay>;
  concurrency?: ProviderCircuitHealth["concurrency"];
  liveStatsEnabled?: boolean;
}) {
  const provider = providerFixture(options.provider);
  if (options.concurrency !== undefined) {
    healthPayload = {
      [provider.id]: healthFixture({ concurrency: options.concurrency }),
    };
  }
  return render(
    <ProviderRichListItem
      provider={provider}
      currentUser={adminViewer}
      healthStatus={healthFixture()}
      enableMultiProviderTypes={false}
      liveStatsEnabled={options.liveStatsEnabled ?? true}
    />
  );
}

describe("ProviderRichListItem 的实时并发徽标", () => {
  beforeEach(() => {
    queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    healthPayload = {};
    getProvidersHealthStatusMock.mockClear();
    while (document.body.firstChild) {
      document.body.removeChild(document.body.firstChild);
    }
  });

  afterEach(() => {
    while (document.body.firstChild) document.body.removeChild(document.body.firstChild);
  });

  it("统计开启且读到数：显示并发数", async () => {
    const { unmount } = renderItem({
      concurrency: { trackingEnabled: true, available: true, activeSessions: 7 },
    });
    await flushTicks();
    expect(text()).toContain(`${NS}.badge`);
    expect(text()).toContain('"count":7');
    unmount();
  });

  it("统计开启且读到 0：仍显示（0 是有效读数，不是「没有」）", async () => {
    const { unmount } = renderItem({
      concurrency: { trackingEnabled: true, available: true, activeSessions: 0 },
    });
    await flushTicks();
    expect(text()).toContain(`${NS}.badge`);
    expect(text()).toContain('"count":0');
    unmount();
  });

  it("统计关闭（trackingEnabled=false）：不显示", async () => {
    const { unmount } = renderItem({
      // 服务端在关闭时不会给 activeSessions；这里刻意给一个值，钉住判据不看它。
      concurrency: { trackingEnabled: false, available: true, activeSessions: 9 },
    });
    await flushTicks();
    expect(text()).not.toContain(`${NS}.badge`);
    unmount();
  });

  it("统计开着但读不到（available=false）：不显示 0 冒充", async () => {
    const { unmount } = renderItem({
      concurrency: {
        trackingEnabled: true,
        available: false,
        activeSessions: null,
        unavailableReason: "redis_unavailable",
      },
    });
    await flushTicks();
    expect(text()).not.toContain(`${NS}.badge`);
    unmount();
  });

  it("未装配（concurrency 缺省）：不显示", async () => {
    const { unmount } = renderItem({ concurrency: null });
    await flushTicks();
    expect(text()).not.toContain(`${NS}.badge`);
    unmount();
  });

  it("禁用的渠道：即使统计开着也不显示（与熔断/降权同一纪律）", async () => {
    const { unmount } = renderItem({
      provider: { isEnabled: false },
      concurrency: { trackingEnabled: true, available: true, activeSessions: 3 },
    });
    await flushTicks();
    expect(text()).not.toContain(`${NS}.badge`);
    unmount();
  });

  it("设了并发上限：显示「分子/上限」", async () => {
    const { unmount } = renderItem({
      provider: { limitConcurrentSessions: 20 },
      concurrency: { trackingEnabled: true, available: true, activeSessions: 5 },
    });
    await flushTicks();
    expect(text()).toContain(`${NS}.badgeWithLimit`);
    expect(text()).toContain('"count":5');
    expect(text()).toContain('"limit":20');
    unmount();
  });

  it("未设并发上限（0）：只显示分子，不显示一个假的「/0」", async () => {
    const { unmount } = renderItem({
      provider: { limitConcurrentSessions: 0 },
      concurrency: { trackingEnabled: true, available: true, activeSessions: 5 },
    });
    await flushTicks();
    expect(text()).toContain(`${NS}.badge`);
    expect(text()).not.toContain(`${NS}.badgeWithLimit`);
    unmount();
  });

  it("开关关着：连取数都不发（「关上就完全不占用资源」）", async () => {
    const { unmount } = renderItem({
      concurrency: { trackingEnabled: true, available: true, activeSessions: 7 },
      liveStatsEnabled: false,
    });
    await flushTicks();
    expect(getProvidersHealthStatusMock).not.toHaveBeenCalled();
    expect(text()).not.toContain(`${NS}.badge`);
    unmount();
  });

  it("轮询重取是**局部更新**：读数更新、所在行不重挂载、也不多发请求", async () => {
    const { container, unmount } = renderItem({
      provider: { limitConcurrentSessions: 20 },
      concurrency: { trackingEnabled: true, available: true, activeSessions: 5 },
    });
    await flushTicks();
    expect(text()).toContain('"count":5');
    const rowBefore = container.firstElementChild;
    expect(rowBefore).not.toBeNull();
    const callsBefore = getProvidersHealthStatusMock.mock.calls.length;

    // 模拟一次 5 秒轮询落地：直接把新值写进同一个缓存条目（等价于服务端回了新数）。
    act(() => {
      queryClient.setQueryData(["providers-health-live"], {
        156: healthFixture({
          concurrency: { trackingEnabled: true, available: true, activeSessions: 9 },
        }),
      });
    });
    await flushTicks();

    expect(text()).toContain('"count":9');
    expect(text()).not.toContain('"count":5');
    // 行没有重挂载（DOM 节点是同一个）。重挂载才是会丢展开态/滚动位置的那种「整页刷新」。
    expect(container.firstElementChild).toBe(rowBefore);
    // 缓存更新不该触发额外请求。
    expect(getProvidersHealthStatusMock.mock.calls.length).toBe(callsBefore);
    unmount();
  });
});
