/**
 * @vitest-environment happy-dom
 */

import type { ReactNode } from "react";
import { act } from "react";
import { createRoot } from "react-dom/client";
import { NextIntlClientProvider } from "next-intl";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { UserMenu } from "./user-menu";

const mockPush = vi.hoisted(() => vi.fn());
const mockRefresh = vi.hoisted(() => vi.fn());

vi.mock("@/i18n/routing", () => ({
  Link: ({ href, children, ...rest }: { href: string; children: ReactNode }) => (
    <a href={href} {...rest}>
      {children}
    </a>
  ),
  useRouter: () => ({
    push: mockPush,
    refresh: mockRefresh,
  }),
}));

const messages = {
  dashboard: {
    nav: {
      documentation: "Docs",
      logout: "Logout",
    },
  },
};

describe("UserMenu", () => {
  let container: HTMLDivElement;
  let root: ReturnType<typeof createRoot>;

  beforeEach(() => {
    // 登出按钮会 fire-and-forget 一个 POST /api/auth/logout（组件不等响应就跳转）。
    // 单测里必须把它接住：否则 happy-dom 会在用例收尾时 abort 掉这个真实网络请求，
    // 抛成 Vitest 的 unhandled rejection（会让整套测试退出码非 0）。
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => new Response(null, { status: 200 }))
    );
    container = document.createElement("div");
    document.body.appendChild(container);
    root = createRoot(container);
  });

  afterEach(() => {
    act(() => {
      root.unmount();
    });
    container.remove();
    vi.unstubAllGlobals();
  });

  const render = (user: { id: number; name: string }) => {
    act(() => {
      root.render(
        <NextIntlClientProvider locale="en" messages={messages} timeZone="UTC">
          <UserMenu user={user} />
        </NextIntlClientProvider>
      );
    });
  };

  it("不再渲染使用文档入口（该入口已按要求移除）", () => {
    render({ id: 1, name: "Ada Lovelace" });

    expect(container.querySelector('a[href="/usage-doc"]')).toBeNull();
    expect(container.textContent).not.toContain("Docs");
  });

  it("保留用户名与退出登录按钮", () => {
    render({ id: 1, name: "Ada Lovelace" });

    expect(container.textContent).toContain("Ada Lovelace");
    const logout = container.querySelector('button[title="Logout"]');
    expect(logout).not.toBeNull();

    act(() => {
      (logout as HTMLButtonElement).dispatchEvent(
        new MouseEvent("click", { bubbles: true, cancelable: true })
      );
    });

    expect(mockPush).toHaveBeenCalledWith("/login");
  });
});
