import { act } from "react";
import { createRoot } from "react-dom/client";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { UsageLogFilters } from "./filters/types";

const routerMocks = vi.hoisted(() => ({
  pushedHref: "",
  push: vi.fn((href: string) => {
    routerMocks.pushedHref = href.startsWith("/") ? `/zh-CN${href}` : href;
  }),
}));

const searchParamMocks = vi.hoisted(() => ({
  value: new URLSearchParams(),
}));

const filterPropMocks = vi.hoisted(() => ({
  panel: undefined as UsageLogFilters | undefined,
  table: undefined as UsageLogFilters | undefined,
  controls: undefined as UsageLogFilters | undefined,
}));

vi.mock("next-intl", () => ({
  useLocale: () => "zh-CN",
  useTranslations: () => (key: string) => key,
}));

vi.mock("next/navigation", () => ({
  useSearchParams: () => searchParamMocks.value,
  useRouter: () => {
    throw new Error("logs navigation must use the locale-aware i18n router");
  },
}));

vi.mock("@/i18n/routing", () => ({
  useRouter: () => ({
    push: routerMocks.push,
  }),
}));

const queryClientMock = vi.hoisted(() => ({
  invalidateQueries: vi.fn(),
  resetQueries: vi.fn(),
  getQueryData: () => undefined,
  setQueryData: vi.fn(),
}));

vi.mock("@tanstack/react-query", () => ({
  useQuery: () => ({ data: undefined, isLoading: false }),
  // 刷新引擎会读/写列表缓存（增量合并走 setQueryData）、手动刷新会清回首页（resetQueries）；
  // 三者必须齐备，否则未处理的 Promise 会被 vitest 记为错误。
  useQueryClient: () => queryClientMock,
}));

vi.mock("@/hooks/use-fullscreen", () => ({
  useFullscreen: () => ({
    supported: false,
    isFullscreen: false,
    request: vi.fn(),
    exit: vi.fn(),
  }),
}));

vi.mock("@/lib/column-visibility", () => ({
  getHiddenColumns: () => [],
}));

vi.mock("@/components/ui/button", () => ({
  Button: ({ children, ...props }: React.ButtonHTMLAttributes<HTMLButtonElement>) => (
    <button type="button" {...props}>
      {children}
    </button>
  ),
}));

vi.mock("@/components/ui/badge", () => ({
  Badge: ({ children }: { children: React.ReactNode }) => <span>{children}</span>,
}));

const collapsibleMocks = vi.hoisted(() => ({
  toggleOpen: undefined as (() => void) | undefined,
}));

vi.mock("@/components/ui/collapsible", () => ({
  // 展开状态在组件自己的 state 里；mock 只负责把「点触发器 → onOpenChange(翻转)」接回去，
  // 否则测不到「展开后快捷下拉消失」（open 仍由服务侧的 state 提供）。
  Collapsible: ({
    children,
    open,
    onOpenChange,
  }: {
    children: React.ReactNode;
    open?: boolean;
    onOpenChange?: (open: boolean) => void;
  }) => {
    collapsibleMocks.toggleOpen = () => onOpenChange?.(!open);
    return <div>{children}</div>;
  },
  CollapsibleContent: ({ children }: { children: React.ReactNode }) => <div>{children}</div>,
  CollapsibleTrigger: ({ children }: { children: React.ReactNode }) => (
    <div onClickCapture={() => collapsibleMocks.toggleOpen?.()}>{children}</div>
  ),
}));

// Radix Select 在 happy-dom 里要打 pointer 事件才能展开选项，仓内惯式是换成原生 select 驱动
// onValueChange（见 filters/status-filters.test.tsx）；工具栏里的刷新控件也吃这个 mock。
vi.mock("@/components/ui/select", () => ({
  Select: ({
    value,
    onValueChange,
    children,
  }: {
    value?: string;
    onValueChange: (value: string) => void;
    children: React.ReactNode;
  }) => (
    <select value={value} onChange={(event) => onValueChange(event.target.value)}>
      {children}
    </select>
  ),
  SelectContent: ({ children }: { children: React.ReactNode }) => <>{children}</>,
  SelectItem: ({ children, value }: { children: React.ReactNode; value: string }) => (
    <option value={value}>{children}</option>
  ),
  SelectTrigger: () => null,
  SelectValue: () => null,
}));

const usersActionMocks = vi.hoisted(() => ({
  searchUsersForFilter: vi.fn(
    async (): Promise<{
      ok: boolean;
      data?: Array<{ id: number; name: string }>;
      error?: string;
    }> => ({
      ok: true,
      data: [
        { id: 7, name: "alice" },
        { id: 9, name: "bob" },
      ],
    })
  ),
}));

vi.mock("@/lib/api-client/v1/actions/users", () => ({
  searchUsersForFilter: usersActionMocks.searchUsersForFilter,
}));

vi.mock("@/components/ui/switch", () => ({
  Switch: () => <button type="button" role="switch" />,
}));

vi.mock("./column-visibility-dropdown", () => ({
  ColumnVisibilityDropdown: () => <div data-testid="column-visibility-dropdown" />,
}));

vi.mock("./usage-logs-stats-panel", () => ({
  UsageLogsStatsPanel: ({ filters }: { filters: UsageLogFilters }) => {
    filterPropMocks.panel = filters;
    return <div data-testid="usage-logs-stats-panel" />;
  },
}));

vi.mock("./virtualized-logs-table", () => ({
  VirtualizedLogsTable: ({ filters }: { filters: UsageLogFilters }) => {
    filterPropMocks.table = filters;
    return <div data-testid="virtualized-logs-table" />;
  },
}));

vi.mock("./usage-logs-filters", () => ({
  UsageLogsFilters: ({
    filters,
    onChange,
    onReset,
  }: {
    filters: UsageLogFilters;
    onChange: (filters: UsageLogFilters) => void;
    onReset: () => void;
  }) => {
    filterPropMocks.controls = filters;
    return (
      <div>
        <button
          type="button"
          onClick={() =>
            onChange({
              userId: 2,
              keyId: 3,
              providerId: 4,
              sessionId: "session-abc",
              startTime: 1000,
              endTime: 2000,
              statusCode: 500,
              model: "claude-sonnet",
              actualResponseModelMismatch: true,
              endpoint: "/v1/messages",
              minRetryCount: 1,
              replayFilter: "replay",
            })
          }
        >
          apply all filters
        </button>
        <button
          type="button"
          onClick={() =>
            onChange({
              excludeStatusCode200: true,
            })
          }
        >
          apply exclude 200
        </button>
        <button type="button" onClick={onReset}>
          reset filters
        </button>
      </div>
    );
  },
}));

import { UsageLogsViewVirtualized } from "./usage-logs-view-virtualized";

function renderUsageLogsView({ isAdmin = true }: { isAdmin?: boolean } = {}) {
  const container = document.createElement("div");
  document.body.appendChild(container);
  const root = createRoot(container);

  act(() => {
    root.render(
      <UsageLogsViewVirtualized
        isAdmin={isAdmin}
        userId={1}
        providers={[]}
        initialKeys={[]}
        currencyCode="USD"
        billingModelSource="original"
      />
    );
  });

  return {
    container,
    unmount: () => {
      act(() => root.unmount());
      container.remove();
    },
  };
}

function clickButton(container: HTMLElement, text: string) {
  const button = Array.from(container.querySelectorAll("button")).find(
    (item) => item.textContent?.trim() === text
  );
  if (!button) {
    throw new Error(`Button not found: ${text}`);
  }
  act(() => {
    button.dispatchEvent(new MouseEvent("click", { bubbles: true }));
  });
}

/** 快捷用户下拉是全页唯一带「全部用户」哨兵值的 select（刷新控件那两个只有模式/间隔）。 */ function findQuickUserSelect(
  container: HTMLElement
): HTMLSelectElement | null {
  return (
    Array.from(container.querySelectorAll("select")).find((element) =>
      Array.from(element.options).some((option) => option.value === "__all__")
    ) ?? null
  );
}

async function selectQuickUser(container: HTMLElement, value: string) {
  const select = findQuickUserSelect(container);
  if (!select) {
    throw new Error("Quick user select not found");
  }
  await act(async () => {
    select.value = value;
    select.dispatchEvent(new Event("change", { bubbles: true }));
  });
}

/** 用户列表在 effect 里异步加载，用一次宏任务把 setUsers 湿完再断言选项。 */
async function flushUserLoad() {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
}

/** 「筛选条件」按钮在有生效筛选时会多一个计数徽标，故按前缀认。 */
function clickFilterTrigger(container: HTMLElement) {
  const button = Array.from(container.querySelectorAll("button")).find((item) =>
    item.textContent?.trim().startsWith("title.filterCriteria")
  );
  if (!button) {
    throw new Error("Filter trigger button not found");
  }
  act(() => {
    button.dispatchEvent(new MouseEvent("click", { bubbles: true }));
  });
}

describe("UsageLogsViewVirtualized filter navigation", () => {
  beforeEach(() => {
    routerMocks.pushedHref = "";
    routerMocks.push.mockClear();
    searchParamMocks.value = new URLSearchParams();
    filterPropMocks.panel = undefined;
    filterPropMocks.table = undefined;
    filterPropMocks.controls = undefined;
    document.body.innerHTML = "";
  });

  it("applies every logs filter through the locale-aware dashboard route", () => {
    const { container, unmount } = renderUsageLogsView();

    clickButton(container, "apply all filters");

    expect(routerMocks.pushedHref).toBe(
      "/zh-CN/dashboard/logs?userId=2&keyId=3&providerId=4&sessionId=session-abc&startTime=1000&endTime=2000&statusCode=500&model=claude-sonnet&actualResponseModelMismatch=true&endpoint=%2Fv1%2Fmessages&minRetry=1&replayFilter=replay"
    );

    unmount();
  });

  it("propagates the Replay URL filter to controls, stats, and table", () => {
    searchParamMocks.value = new URLSearchParams("replayFilter=non-replay");

    const { unmount } = renderUsageLogsView();

    expect(filterPropMocks.controls?.replayFilter).toBe("non-replay");
    expect(filterPropMocks.panel?.replayFilter).toBe("non-replay");
    expect(filterPropMocks.table?.replayFilter).toBe("non-replay");

    unmount();
  });

  it('treats replayFilter="all" as no statistics filter', () => {
    searchParamMocks.value = new URLSearchParams("replayFilter=all");

    const { container, unmount } = renderUsageLogsView();

    expect(filterPropMocks.controls?.replayFilter).toBe("all");
    expect(filterPropMocks.table?.replayFilter).toBe("all");
    expect(filterPropMocks.panel).toBeUndefined();
    expect(container.querySelector('[data-testid="usage-logs-stats-panel"]')).toBeNull();

    unmount();
  });

  it("applies exclude-200 status filter through the locale-aware dashboard route", () => {
    const { container, unmount } = renderUsageLogsView();

    clickButton(container, "apply exclude 200");

    expect(routerMocks.pushedHref).toBe("/zh-CN/dashboard/logs?statusCode=%21200");

    unmount();
  });

  it("resets logs filters through the locale-aware dashboard route", () => {
    const { container, unmount } = renderUsageLogsView();

    clickButton(container, "reset filters");

    expect(routerMocks.pushedHref).toBe("/zh-CN/dashboard/logs");

    unmount();
  });

  it("手动刷新清回首页（resetQueries）而不是 invalidate（后者会留下取不到的夹缝）", async () => {
    queryClientMock.resetQueries.mockClear();
    queryClientMock.invalidateQueries.mockClear();
    const { container, unmount } = renderUsageLogsView();

    const button = container.querySelector('[aria-label="logs.actions.refresh"]');
    expect(button).not.toBeNull();
    await act(async () => {
      button?.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    });

    expect(queryClientMock.resetQueries).toHaveBeenCalledWith({ queryKey: ["usage-logs-batch"] });
    expect(queryClientMock.invalidateQueries).not.toHaveBeenCalled();

    unmount();
  });
});

describe("UsageLogsViewVirtualized height chain", () => {
  beforeEach(() => {
    searchParamMocks.value = new URLSearchParams();
    document.body.innerHTML = "";
  });

  it("passes the available height down to the logs card through a growable flex column", () => {
    const { container, unmount } = renderUsageLogsView();

    // 表格卡片上方的两层容器都必须能把「视口高减 header」的余量往下传，
    // 否则滚动体只能停在 600px 基线高度，大屏下方留下空白。
    const viewRoot = container.firstElementChild as HTMLElement | null;
    const card = container.querySelector('[data-testid="virtualized-logs-table"]')
      ?.parentElement as HTMLElement | null;

    expect(viewRoot).not.toBeNull();
    expect(card).not.toBeNull();
    for (const element of [viewRoot, card]) {
      expect(element?.className).toContain("flex-col");
      expect(element?.className).toContain("grow");
      expect(element?.className).toContain("shrink-0");
    }

    unmount();
  });
});

describe("UsageLogsViewVirtualized collapsed-state quick user filter", () => {
  beforeEach(() => {
    routerMocks.pushedHref = "";
    routerMocks.push.mockClear();
    searchParamMocks.value = new URLSearchParams();
    collapsibleMocks.toggleOpen = undefined;
    document.body.innerHTML = "";
  });

  it("navigates on user change, keeping other filters and dropping page and keyId", async () => {
    searchParamMocks.value = new URLSearchParams("userId=1&keyId=3&providerId=4&page=3");

    const { container, unmount } = renderUsageLogsView();
    // 带筛选条件时面板默认展开，先收起才会露出快捷用户下拉
    clickFilterTrigger(container);
    await flushUserLoad();

    const quickSelect = findQuickUserSelect(container);
    expect(quickSelect).not.toBeNull();
    expect(Array.from(quickSelect?.options ?? []).map((option) => option.textContent)).toEqual([
      "logs.filters.allUsers",
      "alice",
      "bob",
    ]);

    await selectQuickUser(container, "7");

    expect(routerMocks.pushedHref).toBe("/zh-CN/dashboard/logs?userId=7&providerId=4");
    expect(routerMocks.pushedHref).not.toContain("keyId");
    expect(routerMocks.pushedHref).not.toContain("page");

    unmount();
  });

  it("clears userId and keyId when switching back to all users", async () => {
    searchParamMocks.value = new URLSearchParams("userId=7&keyId=3&model=claude-sonnet");

    const { container, unmount } = renderUsageLogsView();
    clickFilterTrigger(container);
    await flushUserLoad();

    await selectQuickUser(container, "__all__");

    expect(routerMocks.pushedHref).toBe("/zh-CN/dashboard/logs?model=claude-sonnet");
    expect(routerMocks.pushedHref).not.toContain("keyId");

    unmount();
  });

  it("hides the quick user filter once the filter panel is expanded", () => {
    const { container, unmount } = renderUsageLogsView();

    expect(findQuickUserSelect(container)).not.toBeNull();

    clickFilterTrigger(container);

    expect(findQuickUserSelect(container)).toBeNull();

    unmount();
  });

  it("renders nothing for non-admin users", () => {
    const { container, unmount } = renderUsageLogsView({ isAdmin: false });

    expect(findQuickUserSelect(container)).toBeNull();

    unmount();
  });

  it("keeps the all-users option when the user list fails to load", async () => {
    usersActionMocks.searchUsersForFilter.mockResolvedValueOnce({
      ok: false,
      error: "users unavailable",
    });

    const { container, unmount } = renderUsageLogsView();
    await flushUserLoad();

    const quickSelect = findQuickUserSelect(container);
    expect(quickSelect).not.toBeNull();
    expect(Array.from(quickSelect?.options ?? []).map((option) => option.value)).toEqual([
      "__all__",
    ]);

    unmount();
  });
});
