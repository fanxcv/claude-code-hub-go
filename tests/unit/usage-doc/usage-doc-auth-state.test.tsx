/**
 * @vitest-environment happy-dom
 */

import fs from "node:fs";
import path from "node:path";
import type { ReactNode } from "react";
import { act } from "react";
import { createRoot } from "react-dom/client";
import { NextIntlClientProvider } from "next-intl";
import { describe, expect, test, vi } from "vitest";
import { UsageDocAuthProvider } from "@/app/[locale]/usage-doc/_components/usage-doc-auth-context";
import { QuickLinks } from "@/app/[locale]/usage-doc/_components/quick-links";

vi.mock("@/i18n/routing", () => ({
  Link: ({
    href,
    children,
    ...rest
  }: {
    href: string;
    children: ReactNode;
    className?: string;
  }) => (
    <a href={href} {...rest}>
      {children}
    </a>
  ),
}));

function loadUsageMessages() {
  return JSON.parse(
    fs.readFileSync(path.join(process.cwd(), "messages", "en", "usage.json"), "utf8")
  );
}

function renderWithAuth(node: ReactNode) {
  const container = document.createElement("div");
  document.body.appendChild(container);
  const root = createRoot(container);
  const usageMessages = loadUsageMessages();

  act(() => {
    root.render(
      <NextIntlClientProvider locale="en" messages={{ usage: usageMessages }} timeZone="UTC">
        {node}
      </NextIntlClientProvider>
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

describe("usage-doc auth state - HttpOnly cookie alignment", () => {
  test("logged-in: QuickLinks renders dashboard link when isLoggedIn=true", () => {
    Object.defineProperty(window, "scrollTo", { value: vi.fn(), writable: true });

    const { container, unmount } = renderWithAuth(
      <UsageDocAuthProvider isLoggedIn={true}>
        <QuickLinks isLoggedIn={true} />
      </UsageDocAuthProvider>
    );

    const dashboardLink = container.querySelector('a[href="/dashboard"]');
    expect(dashboardLink).not.toBeNull();

    unmount();
  });

  test("logged-out: QuickLinks does NOT render dashboard link when isLoggedIn=false", () => {
    Object.defineProperty(window, "scrollTo", { value: vi.fn(), writable: true });

    const { container, unmount } = renderWithAuth(
      <UsageDocAuthProvider isLoggedIn={false}>
        <QuickLinks isLoggedIn={false} />
      </UsageDocAuthProvider>
    );

    const dashboardLink = container.querySelector('a[href="/dashboard"]');
    expect(dashboardLink).toBeNull();

    unmount();
  });

  test("default context value is isLoggedIn=false (no provider ancestor)", () => {
    Object.defineProperty(window, "scrollTo", { value: vi.fn(), writable: true });

    const { container, unmount } = renderWithAuth(<QuickLinks isLoggedIn={false} />);

    const dashboardLink = container.querySelector('a[href="/dashboard"]');
    expect(dashboardLink).toBeNull();

    unmount();
  });

  test("page.tsx no longer reads document.cookie for auth state", async () => {
    const srcContent = fs.readFileSync(
      path.join(process.cwd(), "src", "app", "[locale]", "usage-doc", "page.tsx"),
      "utf8"
    );
    expect(srcContent).not.toContain("document.cookie");
  });

  test("page.tsx uses useUsageDocAuth hook for session state", async () => {
    const srcContent = fs.readFileSync(
      path.join(process.cwd(), "src", "app", "[locale]", "usage-doc", "page.tsx"),
      "utf8"
    );
    expect(srcContent).toContain("useUsageDocAuth");
  });

  test("客户端外壳 wraps children with UsageDocAuthProvider", async () => {
    const srcContent = fs.readFileSync(
      path.join(
        process.cwd(),
        "src",
        "app",
        "[locale]",
        "usage-doc",
        "_components",
        "usage-doc-chrome.tsx"
      ),
      "utf8"
    );
    expect(srcContent).toContain("UsageDocAuthProvider");
    expect(srcContent).toContain("isLoggedIn={canUseDashboard}");
    // 会话来源：原为服务端 `getSession({ allowReadOnlyAccess: true })`（只读密钥也能看文档）。
    // 静态导出下没有服务端会话，改由 `__CCH_BOOTSTRAP__`（Go 壳注入）经 `useUiSession()` 提供；
    // 只读语义未变：壳对只读密钥同样会注入 session，`canUseDashboard` 依旧是「管理员或
    // canLoginWebUi」的判定（见下一条用例）。
    expect(srcContent).toContain("useUiSession");
  });

  test("客户端外壳 gates dashboard access on canLoginWebUi for read-only sessions (U06)", async () => {
    const srcContent = fs.readFileSync(
      path.join(
        process.cwd(),
        "src",
        "app",
        "[locale]",
        "usage-doc",
        "_components",
        "usage-doc-chrome.tsx"
      ),
      "utf8"
    );
    // The provider's isLoggedIn must be derived from a dashboard-access predicate
    // (admin OR canLoginWebUi), not from mere session presence, otherwise a
    // read-only session gets a "Back to Dashboard" link that dead-ends at /login.
    expect(srcContent).toContain("canUseDashboard");
    expect(srcContent).toContain("canLoginWebUi");
  });
});
