/**
 * @vitest-environment happy-dom
 *
 * 回归钉子：用户管理表格的「行展开态」必须跨子树重挂载存活。
 *
 * 背景：对该用户 key 做新增 / 编辑保存 / 删除后，链路末尾的 router.refresh() 会重建路由段，
 * 表格组件被卸载后重挂（本用例用 unmount + 重新 render 模拟同一效果）。若展开态只存在 useState
 * 里，重挂即被初始化器重置，表现为「操作完 key 后用户行自动收起，每次都要重新点开」。
 *
 * 反向也钉住：收起态同样必须存活，避免用「一律展开」糊过去。
 */
import type { ReactNode } from "react";
import { act } from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { afterEach, describe, expect, it, vi } from "vitest";
import { UserManagementTable } from "@/app/[locale]/dashboard/_components/user/user-management-table";
import type { User, UserDisplay } from "@/types/user";

vi.mock("next-intl", () => ({
  useTranslations: () => (key: string) => key,
  useLocale: () => "zh-CN",
}));

vi.mock("next/navigation", () => ({
  useRouter: () => ({ refresh: vi.fn(), push: vi.fn(), replace: vi.fn() }),
}));

vi.mock("@/i18n/routing", () => ({
  Link: ({ children }: { children: ReactNode }) => children,
  useRouter: () => ({ refresh: vi.fn(), push: vi.fn(), replace: vi.fn() }),
}));

// 虚拟列表依赖元素测量与 ResizeObserver，happy-dom 下不渲染任何行；钉住只渲染首行。
vi.mock("@tanstack/react-virtual", () => ({
  useVirtualizer: () => ({
    getVirtualItems: () => [{ index: 0, size: 52, start: 0 }],
    getTotalSize: () => 52,
    measureElement: () => {},
    scrollToOffset: () => {},
  }),
}));

const translations = {
  table: {
    columns: {
      username: "username",
      note: "note",
      expiresAt: "expiresAt",
      expiresAtHint: "expiresAtHint",
      limitRpm: "limitRpm",
      limit5h: "limit5h",
      limitDaily: "limitDaily",
      limitWeekly: "limitWeekly",
      limitMonthly: "limitMonthly",
      limitTotal: "limitTotal",
      limitSessions: "limitSessions",
    },
    keyRow: { fields: {}, actions: {}, status: {} },
    expand: "expand",
    collapse: "collapse",
    noKeys: "noKeys",
    defaultGroup: "defaultGroup",
  },
  editDialog: {},
  actions: {
    edit: "edit",
    status: "status",
    details: "details",
    logs: "logs",
    delete: "delete",
  },
} as never;

const currentUser = { id: 9, role: "admin" } as User;

const users = [
  { id: 1, name: "alice", role: "user", rpm: null, dailyQuota: null, isEnabled: true, keys: [] },
] as UserDisplay[];

describe("UserManagementTable 行展开态跨重挂载保留", () => {
  let container: HTMLDivElement | null = null;
  let root: ReturnType<typeof createRoot> | null = null;

  function mount() {
    container = document.createElement("div");
    document.body.appendChild(container);
    root = createRoot(container);
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    act(() => {
      root!.render(
        <QueryClientProvider client={queryClient}>
          <UserManagementTable
            users={users}
            currentUser={currentUser}
            translations={translations}
          />
        </QueryClientProvider>
      );
    });
  }

  function unmount() {
    if (root) act(() => root!.unmount());
    if (container) container.remove();
    root = null;
    container = null;
  }

  // 表格行是首个带显式 role=button 且带 aria-expanded 的元素
  const row = () => container!.querySelector('[role="button"][aria-expanded]') as HTMLElement;
  const clickRow = () => act(() => row().dispatchEvent(new MouseEvent("click", { bubbles: true })));

  afterEach(unmount);

  it("展开后重挂载仍展开，收起后重挂载仍收起", () => {
    mount();
    expect(row().getAttribute("aria-expanded")).toBe("false");

    clickRow();
    expect(row().getAttribute("aria-expanded")).toBe("true");

    // 模拟 router.refresh() 造成的卸载重挂
    unmount();
    mount();
    expect(row().getAttribute("aria-expanded")).toBe("true");

    clickRow();
    expect(row().getAttribute("aria-expanded")).toBe("false");

    unmount();
    mount();
    expect(row().getAttribute("aria-expanded")).toBe("false");
  });
});
