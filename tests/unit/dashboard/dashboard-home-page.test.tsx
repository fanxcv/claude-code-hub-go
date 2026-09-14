import { act } from "react";
import { createRoot } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const replaceMock = vi.hoisted(() => vi.fn());
const pricesState = vi.hoisted(() => ({ current: {} as Record<string, unknown> }));
const sessionState = vi.hoisted(() => ({ current: {} as Record<string, unknown> }));
const bentoProps = vi.hoisted(() => ({ current: null as Record<string, unknown> | null }));

vi.mock("@tanstack/react-query", () => ({
  useQuery: () => pricesState.current,
}));

vi.mock("@/i18n/routing", () => ({
  useRouter: () => ({ replace: replaceMock, push: vi.fn() }),
  usePathname: () => "/dashboard",
}));

vi.mock("@/components/ui-session-gate", async () => {
  const { createElement, Fragment } = await import("react");
  return {
    UiSessionGate: (props: { children: React.ReactNode }) =>
      createElement(Fragment, null, props.children),
    useUiSession: () => sessionState.current,
  };
});

vi.mock("@/lib/api-client/v1/actions/model-prices", () => ({
  hasPriceTable: vi.fn().mockResolvedValue(true),
}));

vi.mock("@/app/[locale]/dashboard/_components/dashboard-bento-sections", async () => {
  const { createElement, Fragment } = await import("react");
  return {
    DashboardBentoSection: (props: Record<string, unknown>) => {
      bentoProps.current = props;
      return createElement(
        Fragment,
        null,
        createElement("div", null, `bento:isAdmin=${String(props.isAdmin)}`)
      );
    },
  };
});

import DashboardPage from "@/app/[locale]/dashboard/page";

/**
 * 首页在静态化改造后由客户端取数：`hasPriceTable()` 取代服务端 `redirect()` 判价格表，
 * 角色由鉴权壳给出（经 `useUiSession`）。本文件钉住这两个新分支。
 */
describe("DashboardPage", () => {
  let container: HTMLDivElement;

  beforeEach(() => {
    (
      globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT: boolean }
    ).IS_REACT_ACT_ENVIRONMENT = true;
    replaceMock.mockReset();
    bentoProps.current = null;
    pricesState.current = { data: true, isLoading: false };
    sessionState.current = { status: "authenticated", session: { user: { id: 1, role: "admin" } } };
    container = document.createElement("div");
    document.body.appendChild(container);
  });

  afterEach(() => {
    container.remove();
  });

  async function render() {
    const root = createRoot(container);
    await act(async () => {
      root.render(<DashboardPage />);
    });
    return root;
  }

  it("未建价格表时把用户送去建表页且不渲染 bento", async () => {
    pricesState.current = { data: false, isLoading: false };
    const root = await render();

    expect(replaceMock).toHaveBeenCalledWith("/settings/prices?required=true");
    expect(bentoProps.current).toBeNull();
    await act(async () => root.unmount());
  });

  it("已建价格表时渲染 bento，并把 admin 角色透传为 isAdmin", async () => {
    const root = await render();

    expect(replaceMock).not.toHaveBeenCalled();
    expect(bentoProps.current?.isAdmin).toBe(true);
    await act(async () => root.unmount());
  });

  it("非管理员会话下 isAdmin 为 false", async () => {
    sessionState.current = { status: "authenticated", session: { user: { id: 2, role: "user" } } };
    const root = await render();

    expect(bentoProps.current?.isAdmin).toBe(false);
    await act(async () => root.unmount());
  });
});
