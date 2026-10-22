/**
 * @vitest-environment happy-dom
 */

import { act } from "react";
import { createRoot } from "react-dom/client";
import { beforeEach, describe, expect, test, vi } from "vitest";

import { IdentityFilters } from "@/app/[locale]/dashboard/logs/_components/filters/identity-filters";
import type { UsageLogFilters } from "@/app/[locale]/dashboard/logs/_components/filters/types";

// 换语言不必重拉密钥；替身必须是稳定引用，否则「t 每次渲染都新建」会拖着 effect 反复重拉。
const translate = (key: string) => key;

vi.mock("next-intl", () => ({
  useTranslations: () => translate,
}));

const toastMocks = vi.hoisted(() => ({
  error: vi.fn(),
}));

vi.mock("sonner", () => ({ toast: toastMocks }));

const keysActionMocks = vi.hoisted(() => ({
  getKeys: vi.fn(),
}));

vi.mock("@/lib/api-client/v1/actions/keys", () => ({
  getKeys: keysActionMocks.getKeys,
}));

const usersActionMocks = vi.hoisted(() => ({
  searchUsersForFilter: vi.fn(async () => ({
    ok: true,
    data: [
      { id: 1, name: "alice" },
      { id: 2, name: "bob" },
    ],
  })),
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

vi.mock("@/components/ui/select", () => ({
  Select: ({ children }: { children?: React.ReactNode }) => <select>{children}</select>,
  SelectContent: ({ children }: { children?: React.ReactNode }) => <>{children}</>,
  SelectItem: ({ children, value }: { children?: React.ReactNode; value: string }) => (
    <option value={value}>{children}</option>
  ),
  SelectTrigger: () => null,
  SelectValue: () => null,
}));

function buildKeys(userId: number) {
  return [
    { id: userId * 10 + 1, name: `key-${userId}-a` },
    { id: userId * 10 + 2, name: `key-${userId}-b` },
  ];
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((res) => {
    resolve = res;
  });
  return { promise, resolve };
}

async function flush() {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
}

async function actClick(el: Element | null) {
  if (!el) throw new Error("element not found");
  await act(async () => {
    el.dispatchEvent(new MouseEvent("click", { bubbles: true }));
  });
}

function keyOptionNames(container: HTMLElement): string[] {
  return Array.from(container.querySelectorAll("option")).map((option) => option.textContent ?? "");
}

function renderIdentityFilters(initialFilters: UsageLogFilters) {
  const container = document.createElement("div");
  document.body.appendChild(container);
  const root = createRoot(container);
  const onFiltersChange = vi.fn();
  const onKeysChange = vi.fn();

  const render = (filters: UsageLogFilters) => {
    act(() => {
      root.render(
        <IdentityFilters
          isAdmin
          filters={filters}
          onFiltersChange={onFiltersChange}
          initialKeys={[]}
          onKeysChange={onKeysChange}
        />
      );
    });
  };

  render(initialFilters);

  return {
    container,
    onFiltersChange,
    onKeysChange,
    rerender: render,
    unmount: () => {
      act(() => root.unmount());
      container.remove();
    },
  };
}

describe("IdentityFilters key loading follows filters.userId", () => {
  beforeEach(() => {
    keysActionMocks.getKeys.mockReset();
    usersActionMocks.searchUsersForFilter.mockClear();
    toastMocks.error.mockClear();
    document.body.innerHTML = "";
  });

  test("切换 filters.userId 后按新用户重拉密钥", async () => {
    keysActionMocks.getKeys.mockImplementation(async (userId: number) => ({
      ok: true,
      data: buildKeys(userId),
    }));

    const { container, rerender, unmount } = renderIdentityFilters({ userId: 1 });
    await flush();

    expect(keysActionMocks.getKeys).toHaveBeenLastCalledWith(1);
    expect(keyOptionNames(container)).toEqual(["logs.filters.allKeys", "key-1-a", "key-1-b"]);

    // 折叠态快捷下拉只改 URL 里的 userId，本组件不重挂（面板 forceMount），只是 props 变。
    rerender({ userId: 2 });
    await flush();

    expect(keysActionMocks.getKeys).toHaveBeenLastCalledWith(2);
    expect(keyOptionNames(container)).toEqual(["logs.filters.allKeys", "key-2-a", "key-2-b"]);

    unmount();
  });

  test("旧用户的慢返回不覆盖新用户的密钥列表", async () => {
    const slowFirstUser = deferred<{ ok: boolean; data: ReturnType<typeof buildKeys> }>();
    const fastSecondUser = deferred<{ ok: boolean; data: ReturnType<typeof buildKeys> }>();
    keysActionMocks.getKeys.mockImplementation((userId: number) =>
      userId === 1 ? slowFirstUser.promise : fastSecondUser.promise
    );

    const { container, rerender, unmount } = renderIdentityFilters({ userId: 1 });
    rerender({ userId: 2 });

    fastSecondUser.resolve({ ok: true, data: buildKeys(2) });
    await flush();
    expect(keyOptionNames(container)).toContain("key-2-a");

    slowFirstUser.resolve({ ok: true, data: buildKeys(1) });
    await flush();

    expect(keyOptionNames(container)).toEqual(["logs.filters.allKeys", "key-2-a", "key-2-b"]);

    unmount();
  });

  test("切回全部用户时清空密钥列表", async () => {
    keysActionMocks.getKeys.mockImplementation(async (userId: number) => ({
      ok: true,
      data: buildKeys(userId),
    }));

    const { container, onKeysChange, rerender, unmount } = renderIdentityFilters({ userId: 1 });
    await flush();
    expect(keyOptionNames(container)).toContain("key-1-a");

    rerender({});
    await flush();

    expect(keyOptionNames(container)).toEqual(["logs.filters.allKeys"]);
    expect(keysActionMocks.getKeys).toHaveBeenCalledTimes(1);
    expect(onKeysChange).toHaveBeenLastCalledWith([]);

    unmount();
  });

  test("面板内换用户仍只改筛选（userId 变、keyId 清），密钥由 userId 变化驱动", async () => {
    keysActionMocks.getKeys.mockImplementation(async (userId: number) => ({
      ok: true,
      data: buildKeys(userId),
    }));

    const { container, onFiltersChange, unmount } = renderIdentityFilters({ userId: 1, keyId: 5 });
    await flush();

    await actClick(container.querySelector('button[role="combobox"]'));
    const bobItem = Array.from(container.querySelectorAll("[cmdk-item]")).find(
      (item) => (item.textContent || "").trim() === "bob"
    );
    await actClick(bobItem ?? null);

    expect(onFiltersChange).toHaveBeenCalledWith({ userId: 2, keyId: undefined });
    // 面板自己不再直接拉密钥：换用户的密钥重拉是对 filters.userId 变化响应的 effect 的职责。
    expect(keysActionMocks.getKeys).toHaveBeenCalledTimes(1);
    expect(keysActionMocks.getKeys).toHaveBeenLastCalledWith(1);

    unmount();
  });
});
