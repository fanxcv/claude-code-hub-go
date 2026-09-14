import { act } from "react";
import { createRoot } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const replaceMock = vi.hoisted(() => vi.fn());
const probeState = vi.hoisted(() => ({ current: {} as Record<string, unknown> }));
const headerProps = vi.hoisted(() => ({ current: null as unknown }));
const navItems = vi.hoisted(() => ({ current: [] as unknown[] }));

vi.mock("@tanstack/react-query", () => ({
  useQuery: () => probeState.current,
}));

vi.mock("@/lib/api-client/v1/client", () => ({
  apiClient: { get: vi.fn() },
}));

vi.mock("@/i18n/routing", () => ({
  useRouter: () => ({ replace: replaceMock, push: vi.fn() }),
  usePathname: () => "/settings",
  Link: ({ children }: { children?: unknown }) => <span>{children as never}</span>,
}));

vi.mock("next-intl", () => ({
  useTranslations: () => (key: string) => key,
}));

// 头部与子导航各有自己的测试（dashboard-header.test.tsx / settings-nav 由导出产物覆盖）；
// 本文件只验证**布局**把会话语义与导航项正确接上，故以桩替身观测它们的入参。
vi.mock("../dashboard/_components/dashboard-header", () => ({
  DashboardHeader: (props: unknown) => {
    headerProps.current = props;
    return <div data-testid="site-header" />;
  },
}));

vi.mock("./_components/settings-nav", () => ({
  SettingsNav: ({ items }: { items: unknown[] }) => {
    navItems.current = items;
    return <nav data-testid="settings-nav" />;
  },
}));

vi.mock("./_components/page-transition", () => ({
  PageTransition: ({ children }: { children?: unknown }) => <div>{children as never}</div>,
}));

import type { UiBootstrap } from "@/components/ui-session-gate";
import SettingsLayout from "./layout";

function setBootstrap(value: UiBootstrap | undefined) {
  window.__CCH_BOOTSTRAP__ = value;
}

describe("SettingsLayout（静态导出后的客户端门禁）", () => {
  let container: HTMLDivElement;

  beforeEach(() => {
    (
      globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT: boolean }
    ).IS_REACT_ACT_ENVIRONMENT = true;
    replaceMock.mockReset();
    probeState.current = {};
    headerProps.current = null;
    navItems.current = [];
    container = document.createElement("div");
    document.body.appendChild(container);
  });

  afterEach(() => {
    window.__CCH_BOOTSTRAP__ = undefined;
    container.remove();
  });

  async function render() {
    const root = createRoot(container);
    await act(async () => {
      root.render(
        <SettingsLayout>
          <div data-testid="page-body">settings-page</div>
        </SettingsLayout>
      );
    });
    return root;
  }

  it("未登录：跳 /login（与 Node 此处不带 from 的行为一致）且不渲染页面", async () => {
    setBootstrap({ session: null });
    const root = await render();

    expect(replaceMock).toHaveBeenCalledWith("/login");
    expect(container.querySelector('[data-testid="page-body"]')).toBeNull();
    // 首页帧即不渲染受保护内容，避免「先画再跳」
    expect(container.querySelector('[data-testid="settings-nav"]')).toBeNull();

    await act(async () => root.unmount());
  });

  it("已登录但非管理员：跳 /dashboard", async () => {
    setBootstrap({ session: { user: { id: 7, role: "user" } } });
    const root = await render();

    expect(replaceMock).toHaveBeenCalledWith("/dashboard");
    expect(container.querySelector('[data-testid="page-body"]')).toBeNull();

    await act(async () => root.unmount());
  });

  it("管理员：渲染页面本体，并把会话接到头部、导航项接到子导航", async () => {
    setBootstrap({
      session: { user: { id: 1, name: "root", role: "admin" }, key: { canLoginWebUi: true } },
    });
    const root = await render();

    expect(replaceMock).not.toHaveBeenCalled();
    expect(container.querySelector('[data-testid="page-body"]')).not.toBeNull();
    expect(container.querySelector('[data-testid="settings-nav"]')).not.toBeNull();
    expect(headerProps.current).toMatchObject({
      session: { user: { id: 1, name: "root", role: "admin" } },
    });
    // 子导航项由客户端 hook 供给（服务端 getTranslatedNavItems 已被导出脚本判为服务端绑定）
    expect((navItems.current as unknown[]).length).toBeGreaterThan(0);

    await act(async () => root.unmount());
  });

  it("角色未知（壳注入缺失、回退探针未给出角色）：不跳转，渲染匿名外壳", async () => {
    setBootstrap(undefined);
    probeState.current = {
      isSuccess: true,
      data: { userName: "probe-user" },
    };
    const root = await render();

    // 此刻无从判断是否管理员，误跳会把真正的管理员挡在门外；受保护数据仍由端点把关（403）
    expect(replaceMock).not.toHaveBeenCalled();
    expect(container.querySelector('[data-testid="page-body"]')).not.toBeNull();

    await act(async () => root.unmount());
  });
});
