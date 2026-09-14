import { act } from "react";
import { createRoot } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const replaceMock = vi.hoisted(() => vi.fn());
const probeState = vi.hoisted(() => ({ current: {} as Record<string, unknown> }));
const headerProps = vi.hoisted(() => ({ current: null as unknown }));

vi.mock("@tanstack/react-query", () => ({
  useQuery: () => probeState.current,
}));

vi.mock("@/lib/api-client/v1/client", () => ({
  apiClient: { get: vi.fn() },
}));

vi.mock("@/i18n/routing", () => ({
  useRouter: () => ({ replace: replaceMock, push: vi.fn() }),
  usePathname: () => "/usage-doc",
  Link: ({ children, href }: { children?: unknown; href?: string }) => (
    <a data-testid="login-link" href={typeof href === "string" ? href : undefined}>
      {children as never}
    </a>
  ),
}));

vi.mock("next-intl", () => ({
  useTranslations: () => (key: string) => key,
}));

vi.mock("../../dashboard/_components/dashboard-header", () => ({
  DashboardHeader: (props: unknown) => {
    headerProps.current = props;
    return <div data-testid="dashboard-header" />;
  },
}));

import type { UiBootstrap } from "@/components/ui-session-gate";
import { UsageDocChrome } from "./usage-doc-chrome";
import { useUsageDocAuth } from "./usage-doc-auth-context";

/** 探针子组件：读上下文里的 isLoggedIn，用来断言 provider 的取值。 */
function AuthProbe() {
  const { isLoggedIn } = useUsageDocAuth();
  return <span data-testid="is-logged-in">{String(isLoggedIn)}</span>;
}

function setBootstrap(value: UiBootstrap | undefined) {
  window.__CCH_BOOTSTRAP__ = value;
}

describe("UsageDocChrome（文档页外壳的登录态判定）", () => {
  let container: HTMLDivElement;

  beforeEach(() => {
    (
      globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT: boolean }
    ).IS_REACT_ACT_ENVIRONMENT = true;
    probeState.current = {};
    headerProps.current = null;
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
        <UsageDocChrome>
          <AuthProbe />
        </UsageDocChrome>
      );
    });
    return root;
  }

  it("未登录：渲染简化头（带登录入口），上下文 isLoggedIn=false", async () => {
    setBootstrap({ session: null });
    const root = await render();

    expect(container.querySelector('[data-testid="dashboard-header"]')).toBeNull();
    expect(container.querySelector('[data-testid="login-link"]')?.getAttribute("href")).toBe(
      "/login?from=/usage-doc"
    );
    expect(container.querySelector('[data-testid="is-logged-in"]')?.textContent).toBe("false");

    await act(async () => root.unmount());
  });

  it("管理员：渲染完整头部，上下文 isLoggedIn=true", async () => {
    setBootstrap({
      session: { user: { id: 1, name: "root", role: "admin" }, key: { canLoginWebUi: true } },
    });
    const root = await render();

    expect(container.querySelector('[data-testid="dashboard-header"]')).not.toBeNull();
    expect(headerProps.current).toMatchObject({ session: { user: { id: 1, role: "admin" } } });
    expect(container.querySelector('[data-testid="is-logged-in"]')?.textContent).toBe("true");

    await act(async () => root.unmount());
  });

  it("只读会话（canLoginWebUi=false 的普通用户）：仍算已登录，但 isLoggedIn=false", async () => {
    // U06：只读会话能看文档，但「回到控制台」的快捷入口会死在登录页，故按同一判据关掉它。
    setBootstrap({
      session: { user: { id: 9, name: "reader", role: "user" }, key: { canLoginWebUi: false } },
    });
    const root = await render();

    expect(container.querySelector('[data-testid="dashboard-header"]')).not.toBeNull();
    expect(container.querySelector('[data-testid="login-link"]')).toBeNull();
    expect(container.querySelector('[data-testid="is-logged-in"]')?.textContent).toBe("false");

    await act(async () => root.unmount());
  });

  it("普通用户（canLoginWebUi=true）：算已登录且可回控制台", async () => {
    setBootstrap({
      session: { user: { id: 10, name: "user", role: "user" }, key: { canLoginWebUi: true } },
    });
    const root = await render();

    expect(container.querySelector('[data-testid="is-logged-in"]')?.textContent).toBe("true");

    await act(async () => root.unmount());
  });
});
