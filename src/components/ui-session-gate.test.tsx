import { act } from "react";
import { createRoot } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const replaceMock = vi.hoisted(() => vi.fn());
const probeState = vi.hoisted(() => ({ current: {} as Record<string, unknown> }));

vi.mock("@tanstack/react-query", () => ({
  useQuery: () => probeState.current,
}));

vi.mock("@/i18n/routing", () => ({
  useRouter: () => ({ replace: replaceMock, push: vi.fn() }),
  usePathname: () => "/dashboard/audit-logs",
}));

vi.mock("@/lib/api-client/v1/client", () => ({
  apiClient: { get: vi.fn() },
}));

import { resolveSessionGateTarget, UiSessionGate, type UiBootstrap } from "./ui-session-gate";

function setBootstrap(value: UiBootstrap) {
  window.__CCH_BOOTSTRAP__ = value;
}

describe("UiSessionGate", () => {
  let container: HTMLDivElement;

  beforeEach(() => {
    (
      globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT: boolean }
    ).IS_REACT_ACT_ENVIRONMENT = true;
    replaceMock.mockReset();
    probeState.current = {};
    container = document.createElement("div");
    document.body.appendChild(container);
  });

  afterEach(() => {
    window.__CCH_BOOTSTRAP__ = undefined;
    container.remove();
  });

  async function render(requireRole?: "admin") {
    const root = createRoot(container);
    await act(async () => {
      root.render(
        <UiSessionGate requireRole={requireRole} forbiddenHref="/dashboard">
          <div>protected-content</div>
        </UiSessionGate>
      );
    });
    return root;
  }

  it("壳注入且角色相符时首帧即渲染受保护内容", async () => {
    setBootstrap({ session: { user: { id: 1, role: "admin" } } });
    const root = await render("admin");

    expect(container.textContent).toContain("protected-content");
    expect(replaceMock).not.toHaveBeenCalled();
    await act(async () => root.unmount());
  });

  it("壳注入且未登录时首帧不渲染受保护内容并跳登录页（带 from）", async () => {
    setBootstrap({ session: null });
    const root = await render();

    expect(container.textContent).not.toContain("protected-content");
    expect(replaceMock).toHaveBeenCalledWith("/login?from=%2Fdashboard%2Faudit-logs");
    await act(async () => root.unmount());
  });

  it("壳注入且角色不符时跳 forbiddenHref", async () => {
    setBootstrap({ session: { user: { id: 2, role: "user" } } });
    const root = await render("admin");

    expect(container.textContent).not.toContain("protected-content");
    expect(replaceMock).toHaveBeenCalledWith("/dashboard");
    await act(async () => root.unmount());
  });

  it("未注入壳时以探针判定：成功即渲染", async () => {
    probeState.current = { isSuccess: true, data: { userName: "alice" } };
    const root = await render();

    expect(container.textContent).toContain("protected-content");
    expect(replaceMock).not.toHaveBeenCalled();
    await act(async () => root.unmount());
  });

  it("未注入壳时探针失败按未登录处理", async () => {
    probeState.current = { isError: true, error: new Error("unauthenticated") };
    const root = await render();

    expect(container.textContent).not.toContain("protected-content");
    expect(replaceMock).toHaveBeenCalledWith("/login?from=%2Fdashboard%2Faudit-logs");
    await act(async () => root.unmount());
  });

  it("回退态角色未知时不误跳（不踢掉真管理员）", async () => {
    probeState.current = { isSuccess: true, data: { userName: "alice" } };
    const root = await render("admin");

    expect(container.textContent).toContain("protected-content");
    expect(replaceMock).not.toHaveBeenCalled();
    await act(async () => root.unmount());
  });

  it("探针未决时既不渲染受保护内容也不跳转", async () => {
    probeState.current = { isLoading: true };
    const root = await render();

    expect(container.textContent).not.toContain("protected-content");
    expect(replaceMock).not.toHaveBeenCalled();
    await act(async () => root.unmount());
  });
});

describe("resolveSessionGateTarget（各控制台布局共用的跳转判定）", () => {
  const DASHBOARD = {
    anonymousHref: "/login?from=/dashboard",
    forbiddenHref: "/my-usage",
    forbiddenRule: "admin-or-webui" as const,
  };
  const SETTINGS = {
    anonymousHref: "/login",
    forbiddenHref: "/dashboard",
    forbiddenRule: "admin" as const,
  };

  it("未登录 → 用 anonymousHref（dashboard 带 from 回跳参数，settings 不带）", () => {
    expect(resolveSessionGateTarget({ status: "anonymous" }, DASHBOARD)).toBe(
      "/login?from=/dashboard"
    );
    expect(resolveSessionGateTarget({ status: "anonymous" }, SETTINGS)).toBe("/login");
  });

  it("pending（预渲染期无 bootstrap）→ 不跳转，否则会把真管理员踢出去", () => {
    expect(resolveSessionGateTarget({ status: "pending" }, DASHBOARD)).toBeNull();
    expect(resolveSessionGateTarget({ status: "pending" }, SETTINGS)).toBeNull();
  });

  it("管理员 → 不跳转", () => {
    const admin = {
      status: "authenticated" as const,
      session: {
        user: { id: 1, name: "A", role: "admin" as const },
        key: { canLoginWebUi: false },
      },
    };
    expect(resolveSessionGateTarget(admin, DASHBOARD)).toBeNull();
    expect(resolveSessionGateTarget(admin, SETTINGS)).toBeNull();
  });

  it("普通用户 + canLoginWebUi=false → 两家都退（dashboard→my-usage，settings→dashboard）", () => {
    const user = {
      status: "authenticated" as const,
      session: { user: { id: 2, name: "U", role: "user" as const }, key: { canLoginWebUi: false } },
    };
    expect(resolveSessionGateTarget(user, DASHBOARD)).toBe("/my-usage");
    expect(resolveSessionGateTarget(user, SETTINGS)).toBe("/dashboard");
  });

  it("普通用户 + canLoginWebUi=true → dashboard 放行，但 settings 仍退（门槛不同）", () => {
    const user = {
      status: "authenticated" as const,
      session: { user: { id: 6, name: "U2", role: "user" as const }, key: { canLoginWebUi: true } },
    };
    expect(resolveSessionGateTarget(user, DASHBOARD)).toBeNull();
    expect(resolveSessionGateTarget(user, SETTINGS)).toBe("/dashboard");
  });

  it("角色未知（回退探针态）→ 不跳转", () => {
    const unknownRole = {
      status: "authenticated" as const,
      session: { user: { id: 3, name: "P" } },
    };
    expect(resolveSessionGateTarget(unknownRole, DASHBOARD)).toBeNull();
    expect(resolveSessionGateTarget(unknownRole, SETTINGS)).toBeNull();
  });

  it("只读密钥（role=user, canLoginWebUi=false）→ 跳 forbiddenHref", () => {
    const readonly = {
      status: "authenticated" as const,
      session: { user: { id: 4, name: "R", role: "user" as const }, key: { canLoginWebUi: false } },
    };
    expect(resolveSessionGateTarget(readonly, DASHBOARD)).toBe("/my-usage");
  });

  it("不给 forbiddenHref（只读页）→ 从不按角色跳转", () => {
    const user = {
      status: "authenticated" as const,
      session: { user: { id: 5, name: "U", role: "user" as const } },
    };
    expect(resolveSessionGateTarget(user, { anonymousHref: "/login" })).toBeNull();
  });
});
