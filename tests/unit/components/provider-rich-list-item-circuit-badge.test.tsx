/**
 * @vitest-environment happy-dom
 *
 * provider-rich-list-item 的熔断徽标渲染契约（用户实报：「有的供应商我已经禁用了，却还显示熔断恢复中」）。
 *
 * 为什么值得单测：熔断读数存在 Redis（TTL 24h），供应商被禁用后旧状态照样留着；而禁用的供应商
 * **不参与选路**，这个读数与「它会不会被选中」无关。两者并排显示时，管理员会去排查熔断，
 * 而真实原因是「它被禁用了」——这是误导。故徽标只在**启用**时呈现（接口口径不变，只看展示层）。
 *
 * 另一面同样要钉：**启用的**供应商仍须照实显示熔断/半开徽标。只断言「不显示」很容易写出
 * 一条永远绿的空跑用例（把徽标整个删掉它也绿），所以每个「不显示」用例都配一条「显示」的对照。
 *
 * 范式沿用仓库既有做法：`react-dom/client` 的 createRoot + act + happy-dom（本仓不用 testing-library）。
 */

import type { ReactNode } from "react";
import { act } from "react";
import { createRoot } from "react-dom/client";
import { afterEach, describe, expect, it, vi } from "vitest";

// next-intl：把 t() 变成「命名空间.键」的可断字符串，断言钉的是**用没用对键**，不是某语种文案。
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

// 管理面 action：本文件只渲染，不点保存路径；列出来是为了不把真网络层拉进测试。
vi.mock("@/lib/api-client/v1/actions/providers", () => ({
  editProvider: vi.fn(),
  getUnmaskedProviderKey: vi.fn(),
  removeProvider: vi.fn(),
  resetProviderCircuit: vi.fn(),
  resetProviderTotalUsage: vi.fn(),
  undoProviderDelete: vi.fn(),
}));

// 弹窗/表单类子组件：本文件断的是**行上**的徽标，与这些无关，换成轻量占位。
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

// Switch 透传勾选态：行上的「启用/禁用」标示就是它的勾选态，用例据此断言禁用仍有标示。
vi.mock("@/components/ui/switch", () => ({
  Switch: ({ checked }: { checked?: boolean }) => (
    <input type="checkbox" role="switch" checked={checked} readOnly />
  ),
}));

// Tooltip 家族在本文件里没有断言价值，直通即可（真实现在 happy-dom 下还需要 Provider）。
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

// 行上的启用开关与操作按钮都受 canEdit（role 为 admin）控制；非管理员看不到它们，
// 而「禁用仍有标示」这条正是要断开关的勾选态，故管理员是本文件观察位。
const adminViewer: ProviderViewer = { role: "admin", providerGroup: null };

/** 只填渲染这条行所需的字段；其余取类型允许的空值（本文件不测这些字段的语义）。 */
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

/** 熔断读数：默认取「生产 156/145 同款」——raw 半开、窗口已过期、计数 0。 */
function healthFixture(overrides: Partial<ProviderCircuitHealth> = {}): ProviderCircuitHealth {
  return {
    circuitState: "half-open",
    failureCount: 11,
    lastFailureTime: 1789265134080,
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

const KEY_BROKEN = "settings.providers.list.keyCircuitBroken";
const KEY_HALF_OPEN = "settings.providers.list.keyCircuitHalfOpen";
const ENDPOINT_BROKEN = "settings.providers.list.endpointCircuitBroken";

describe("ProviderRichListItem 的熔断徽标", () => {
  afterEach(() => {
    while (document.body.firstChild) document.body.removeChild(document.body.firstChild);
  });

  it("启用的供应商：half-open 显示「恢复中」、open 显示「已熔断」", () => {
    const unmountHalfOpen = render(
      <ProviderRichListItem
        provider={providerFixture()}
        healthStatus={healthFixture()}
        enableMultiProviderTypes={false}
      />
    );
    expect(text()).toContain(KEY_HALF_OPEN);
    expect(text()).not.toContain(KEY_BROKEN);
    unmountHalfOpen();

    const unmountOpen = render(
      <ProviderRichListItem
        provider={providerFixture()}
        healthStatus={healthFixture({ circuitState: "open", circuitOpenUntil: 1789266934080 })}
        enableMultiProviderTypes={false}
      />
    );
    expect(text()).toContain(KEY_BROKEN);
    unmountOpen();
  });

  it("禁用的供应商：残留的 half-open 读数不再显示「恢复中」", () => {
    const unmount = render(
      <ProviderRichListItem
        provider={providerFixture({ isEnabled: false })}
        healthStatus={healthFixture()}
        enableMultiProviderTypes={false}
      />
    );
    const rendered = text();
    expect(rendered).not.toContain(KEY_HALF_OPEN);
    // 反面：接口读数照实存在（只是不显示），不是把数据吞掉。
    expect(rendered).not.toContain(KEY_BROKEN);
    unmount();
  });

  it("禁用的供应商：窗口内的 open 也不再显示「已熔断」", () => {
    const unmount = render(
      <ProviderRichListItem
        provider={providerFixture({ isEnabled: false })}
        healthStatus={healthFixture({ circuitState: "open", circuitOpenUntil: 1789266934080 })}
        enableMultiProviderTypes={false}
      />
    );
    const rendered = text();
    expect(rendered).not.toContain(KEY_BROKEN);
    expect(rendered).not.toContain(KEY_HALF_OPEN);
    unmount();
  });

  it("启用的供应商：端点级熔断照常显示；禁用后一并收起", () => {
    const endpoints = [
      { endpointId: 1, circuitState: "open" as const, failureCount: 3, circuitOpenUntil: null },
    ];
    const unmountEnabled = render(
      <ProviderRichListItem
        provider={providerFixture()}
        endpointCircuitInfo={endpoints}
        enableMultiProviderTypes={false}
      />
    );
    expect(text()).toContain(ENDPOINT_BROKEN);
    unmountEnabled();

    const unmountDisabled = render(
      <ProviderRichListItem
        provider={providerFixture({ isEnabled: false })}
        endpointCircuitInfo={endpoints}
        enableMultiProviderTypes={false}
      />
    );
    expect(text()).not.toContain(ENDPOINT_BROKEN);
    unmountDisabled();
  });

  it("禁用状态本身仍有标示：行上的启用开关勾选态为 false", () => {
    const unmount = render(
      <ProviderRichListItem
        provider={providerFixture({ isEnabled: false })}
        healthStatus={healthFixture()}
        currentUser={adminViewer}
        enableMultiProviderTypes={false}
      />
    );
    const switches = [...document.querySelectorAll("input[role='switch']")];
    expect(switches.length).toBeGreaterThan(0);
    // 收起熔断徽标不能把「它是禁用的」这件事一起藏掉。
    expect(switches.some((item) => (item as HTMLInputElement).checked === false)).toBe(true);
    unmount();
  });
});
