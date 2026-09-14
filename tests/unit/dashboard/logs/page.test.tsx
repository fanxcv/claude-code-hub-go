import { act } from "react";
import { createRoot } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const queryState = vi.hoisted(() => ({ current: {} as Record<string, Record<string, unknown>> }));
const sessionState = vi.hoisted(() => ({ current: {} as Record<string, unknown> }));
const sectionProps = vi.hoisted(() => ({
  data: null as Record<string, unknown> | null,
  sessions: null as Record<string, unknown> | null,
}));

vi.mock("@tanstack/react-query", () => ({
  useQuery: (options: { queryKey: readonly unknown[] }) => {
    const key = String((options.queryKey as unknown[] | undefined)?.[0]);
    return queryState.current[key] ?? { isPending: false, data: undefined, isError: false };
  },
}));

vi.mock("@/components/ui-session-gate", async () => {
  const { createElement, Fragment } = await import("react");
  return {
    UiSessionGate: (props: { children: React.ReactNode }) =>
      createElement(Fragment, null, props.children),
    useUiSession: () => sessionState.current,
  };
});

vi.mock("@/lib/api-client/v1/actions/system-config", () => ({
  getSystemSettings: vi.fn(),
  getServerTimeZone: vi.fn(),
}));

vi.mock("@/app/[locale]/dashboard/logs/_components/usage-logs-skeleton", async () => {
  const { createElement, Fragment } = await import("react");
  return {
    UsageLogsSkeleton: () => createElement(Fragment, null, createElement("span", null, "skeleton")),
  };
});

vi.mock("@/app/[locale]/dashboard/logs/_components/usage-logs-sections", async () => {
  const { createElement, Fragment } = await import("react");
  return {
    UsageLogsActiveSessionsSection: (props: Record<string, unknown>) => {
      sectionProps.sessions = props;
      return createElement(Fragment, null, createElement("span", null, "active-sessions"));
    },
    UsageLogsDataSection: (props: Record<string, unknown>) => {
      sectionProps.data = props;
      return createElement(Fragment, null, createElement("span", null, "data-section"));
    },
  };
});

import UsageLogsPage from "@/app/[locale]/dashboard/logs/page";

/**
 * 用量日志页在静态化改造后由客户端取数：设置与系统时区分别来自
 * `/api/v1/system/settings` 与 `/api/v1/system/timezone`（经 react-query），角色与用户 id
 * 来自壳注入的会话（`useUiSession`）。本文件钉住四件事：
 * 高并发模式下的区块取舍、普通模式下的区块组合、设置未就绪时的等待（避免先画再换币种）、
 * 以及时区取数失败时不编造值。
 */
describe("UsageLogsPage", () => {
  let container: HTMLDivElement;

  beforeEach(() => {
    (
      globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT: boolean }
    ).IS_REACT_ACT_ENVIRONMENT = true;
    container = document.createElement("div");
    document.body.appendChild(container);
    sectionProps.data = null;
    sectionProps.sessions = null;
    sessionState.current = {
      status: "authenticated",
      session: { user: { id: 7, role: "admin" } },
    };
    queryState.current = {
      "system-settings": {
        isPending: false,
        isError: false,
        data: {
          enableHighConcurrencyMode: false,
          currencyDisplay: "CNY",
          billingModelSource: "redirected",
        },
      },
      "system-timezone": {
        isPending: false,
        isError: false,
        data: { ok: true, data: { timeZone: "Asia/Shanghai" } },
      },
    };
  });

  afterEach(() => {
    container.remove();
  });

  function renderPage() {
    act(() => {
      createRoot(container).render(<UsageLogsPage />);
    });
    return container.textContent ?? "";
  }

  it("普通模式：活跃会话区块 + 数据区块，且口径/时区/身份都传到位", () => {
    const text = renderPage();

    expect(text).toContain("active-sessions");
    expect(text).toContain("data-section");
    expect(sectionProps.sessions).toMatchObject({ currencyCode: "CNY" });
    expect(sectionProps.data).toMatchObject({
      isAdmin: true,
      userId: 7,
      currencyCode: "CNY",
      billingModelSource: "redirected",
      serverTimeZone: "Asia/Shanghai",
    });
  });

  it("高并发模式：不渲染活跃会话区块", () => {
    queryState.current["system-settings"] = {
      isPending: false,
      isError: false,
      data: {
        enableHighConcurrencyMode: true,
        currencyDisplay: "USD",
        billingModelSource: "original",
      },
    };

    const text = renderPage();

    expect(text).not.toContain("active-sessions");
    expect(text).toContain("data-section");
    expect(sectionProps.sessions).toBeNull();
  });

  it("设置未就绪时先显示骨架，不渲染区块（避免先画再换币种）", () => {
    queryState.current["system-settings"] = { isPending: true, isError: false, data: undefined };

    const text = renderPage();

    expect(text).toContain("skeleton");
    expect(sectionProps.data).toBeNull();
    expect(sectionProps.sessions).toBeNull();
  });

  it("时区取数失败时不编造值，其余照常渲染", () => {
    queryState.current["system-timezone"] = {
      isPending: false,
      isError: false,
      data: { ok: false, error: "boom" },
    };

    renderPage();

    expect(sectionProps.data).toMatchObject({ serverTimeZone: undefined, userId: 7 });
  });

  it("未登录（壳已接管跳转）时不渲染任何区块", () => {
    sessionState.current = { status: "anonymous" };

    const text = renderPage();

    expect(text).not.toContain("data-section");
    expect(sectionProps.data).toBeNull();
  });
});
