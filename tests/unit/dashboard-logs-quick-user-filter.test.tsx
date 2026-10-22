/**
 * @vitest-environment happy-dom
 */

import { act, useState } from "react";
import { createRoot } from "react-dom/client";
import { beforeEach, describe, expect, test, vi } from "vitest";

import { QuickUserFilter } from "@/app/[locale]/dashboard/logs/_components/quick-user-filter";

const translate = (key: string) => key;

vi.mock("next-intl", () => ({
  useTranslations: () => translate,
}));

const usersActionMocks = vi.hoisted(() => ({
  searchUsersForFilter: vi.fn(),
}));

vi.mock("@/lib/api-client/v1/actions/users", () => ({
  searchUsersForFilter: usersActionMocks.searchUsersForFilter,
}));

// Radix Popover / cmdk 在 happy-dom 里不便驱动，换成仓内惯用的受控轻量替身。
vi.mock("@/components/ui/popover", async () => {
  const React = await import("react");

  type PopoverCtx = { open: boolean; onOpenChange?: (open: boolean) => void };
  const PopoverContext = React.createContext<PopoverCtx>({ open: false });

  function Popover({
    open,
    onOpenChange,
    children,
  }: {
    open?: boolean;
    onOpenChange?: (open: boolean) => void;
    children?: React.ReactNode;
  }) {
    return (
      <PopoverContext.Provider value={{ open: Boolean(open), onOpenChange }}>
        {children}
      </PopoverContext.Provider>
    );
  }

  function PopoverTrigger({
    asChild,
    children,
  }: {
    asChild?: boolean;
    children?: React.ReactNode;
  }) {
    const { open, onOpenChange } = React.useContext(PopoverContext);
    const child = React.Children.only(children) as unknown as {
      props: { onClick?: (e: unknown) => void };
    };

    const handleClick = (e: unknown) => {
      child.props.onClick?.(e);
      onOpenChange?.(!open);
    };

    if (asChild) {
      return React.cloneElement(child as never, { onClick: handleClick });
    }

    return (
      <button type="button" onClick={handleClick}>
        {children}
      </button>
    );
  }

  function PopoverContent({ children }: { children?: React.ReactNode }) {
    const { open } = React.useContext(PopoverContext);
    if (!open) return null;
    return <div>{children}</div>;
  }

  function PopoverAnchor({ children }: { children?: React.ReactNode }) {
    return <>{children}</>;
  }

  return { Popover, PopoverTrigger, PopoverContent, PopoverAnchor };
});

vi.mock("@/components/ui/command", () => ({
  Command: ({ children }: { children?: React.ReactNode }) => <div>{children}</div>,
  CommandInput: ({
    placeholder,
    value,
    onValueChange,
  }: {
    placeholder?: string;
    value?: string;
    onValueChange?: (value: string) => void;
  }) => (
    <input
      placeholder={placeholder}
      value={value ?? ""}
      onChange={(event) => onValueChange?.(event.target.value)}
    />
  ),
  CommandList: ({ children }: { children?: React.ReactNode }) => <div>{children}</div>,
  CommandEmpty: ({ children }: { children?: React.ReactNode }) => <div>{children}</div>,
  CommandGroup: ({ children }: { children?: React.ReactNode }) => <div>{children}</div>,
  CommandItem: ({
    children,
    onSelect,
  }: {
    children?: React.ReactNode;
    value?: string;
    onSelect?: () => void;
  }) => (
    <div cmdk-item="" onClick={onSelect}>
      {children}
    </div>
  ),
}));

async function flush() {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
}

async function waitForDebounce() {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 350));
  });
}

async function actClick(el: Element | null) {
  if (!el) throw new Error("element not found");
  await act(async () => {
    el.dispatchEvent(new MouseEvent("click", { bubbles: true }));
  });
}

function renderQuickUserFilter(initialUserId?: number) {
  const container = document.createElement("div");
  document.body.appendChild(container);
  const root = createRoot(container);
  const onUserChange = vi.fn();

  // 与真实父组件同形：回调把 userId 回灌成 props（父侧是 URL query）。
  function Harness() {
    const [userId, setUserId] = useState(initialUserId);
    return (
      <QuickUserFilter
        userId={userId}
        onUserChange={(nextUserId) => {
          onUserChange(nextUserId);
          setUserId(nextUserId);
        }}
      />
    );
  }

  act(() => {
    root.render(<Harness />);
  });

  return {
    container,
    onUserChange,
    unmount: () => {
      act(() => root.unmount());
      container.remove();
    },
  };
}

function findTrigger(container: HTMLElement): HTMLButtonElement | null {
  return container.querySelector('button[aria-label="logs.filters.quickUser"]');
}

function itemTexts(container: HTMLElement): string[] {
  return Array.from(container.querySelectorAll("[cmdk-item]")).map((item) =>
    (item.textContent || "").trim()
  );
}

async function openAndSearch(container: HTMLElement, term: string) {
  await actClick(findTrigger(container));
  const input = container.querySelector(
    'input[placeholder="logs.filters.searchUser"]'
  ) as HTMLInputElement | null;
  if (!input) throw new Error("search input not found");
  const setter = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, "value")?.set;
  if (!setter) throw new Error("missing native input value setter");

  await act(async () => {
    setter.call(input, term);
    input.dispatchEvent(new Event("input", { bubbles: true }));
  });
  await waitForDebounce();
}

const DEFAULT_USERS = [
  { id: 7, name: "alice" },
  { id: 9, name: "bob" },
];

describe("QuickUserFilter remote search", () => {
  beforeEach(() => {
    usersActionMocks.searchUsersForFilter.mockReset();
    usersActionMocks.searchUsersForFilter.mockResolvedValue({
      ok: true,
      data: DEFAULT_USERS,
    });
    document.body.innerHTML = "";
  });

  test("挂载即加载缺省一页，未选用户时触发钮显示全部用户", async () => {
    const { container, unmount } = renderQuickUserFilter();
    await flush();

    expect(usersActionMocks.searchUsersForFilter).toHaveBeenCalledTimes(1);
    expect(usersActionMocks.searchUsersForFilter).toHaveBeenLastCalledWith(undefined);
    expect(findTrigger(container)?.textContent).toContain("logs.filters.allUsers");

    await actClick(findTrigger(container));
    expect(itemTexts(container)).toEqual(["logs.filters.allUsers", "alice", "bob"]);

    unmount();
  });

  test("输入关键字按词远端搜索，点选结果回调 id 且触发钮记住名字", async () => {
    const { container, onUserChange, unmount } = renderQuickUserFilter();
    await flush();
    usersActionMocks.searchUsersForFilter.mockResolvedValueOnce({
      ok: true,
      data: [{ id: 42, name: "carol" }],
    });
    await openAndSearch(container, "carol");

    expect(usersActionMocks.searchUsersForFilter).toHaveBeenLastCalledWith("carol");
    expect(itemTexts(container)).toContain("carol");

    await actClick(
      Array.from(container.querySelectorAll("[cmdk-item]")).find(
        (item) => (item.textContent || "").trim() === "carol"
      ) ?? null
    );

    expect(onUserChange).toHaveBeenCalledWith(42);

    // 收起后搜索词清空、缺省页重拉（不含 carol），触发钮仍显示选中的名字而不是数字 id。
    await waitForDebounce();
    expect(itemTexts(container)).toEqual([]);
    expect(findTrigger(container)?.textContent).not.toContain("42");
    expect(findTrigger(container)?.textContent).toContain("carol");

    unmount();
  });

  test("搜索无结果时给「无匹配用户」空态", async () => {
    const { container, unmount } = renderQuickUserFilter();
    await flush();
    usersActionMocks.searchUsersForFilter.mockResolvedValueOnce({ ok: true, data: [] });
    await openAndSearch(container, "nobody");

    expect(container.textContent).toContain("logs.filters.noUserFound");
    expect(itemTexts(container)).toEqual(["logs.filters.allUsers"]);

    unmount();
  });

  test("加载失败时给可读提示且不阻塞触发钮", async () => {
    usersActionMocks.searchUsersForFilter.mockResolvedValueOnce({
      ok: false,
      error: "users unavailable",
    });

    const { container, unmount } = renderQuickUserFilter();
    await flush();

    expect(findTrigger(container)).not.toBeNull();

    await actClick(findTrigger(container));
    expect(container.textContent).toContain("logs.error.loadFailed");
    expect(itemTexts(container)).toEqual(["logs.filters.allUsers"]);

    unmount();
  });
});
