import { act } from "react";
import { createRoot } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

const viewProps = vi.hoisted(() => ({ current: null as Record<string, unknown> | null }));

vi.mock("./usage-logs-view-virtualized", async () => {
  const { createElement, Fragment } = await import("react");
  return {
    UsageLogsViewVirtualized: (props: Record<string, unknown>) => {
      viewProps.current = props;
      return createElement(Fragment, null);
    },
  };
});

vi.mock("@/components/customs/active-sessions-list", async () => {
  const { createElement, Fragment } = await import("react");
  return {
    ActiveSessionsList: (props: Record<string, unknown>) => {
      viewProps.current = props;
      return createElement(Fragment, null);
    },
  };
});

import { UsageLogsActiveSessionsSection, UsageLogsDataSection } from "./usage-logs-sections";

/**
 * 静态化改造后本模块**不再取数**，只做透传：数据由 `page.tsx` 用客户端 action 取好传入。
 * 因此这里钉住的是「传下来的东西原样到视图」，取代原先「从 `getSystemSettings()` 读出来」的断言。
 */
describe("UsageLogsDataSection", () => {
  let container: HTMLDivElement;

  beforeEach(() => {
    (
      globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT: boolean }
    ).IS_REACT_ACT_ENVIRONMENT = true;
    viewProps.current = null;
    container = document.createElement("div");
    document.body.appendChild(container);
  });

  afterEach(() => {
    container.remove();
  });

  it("透传结算口径、币种、时区与身份", () => {
    act(() => {
      createRoot(container).render(
        <UsageLogsDataSection
          isAdmin={true}
          userId={1}
          currencyCode="CNY"
          billingModelSource="redirected"
          serverTimeZone="Asia/Shanghai"
        />
      );
    });

    expect(viewProps.current).toMatchObject({
      isAdmin: true,
      userId: 1,
      currencyCode: "CNY",
      billingModelSource: "redirected",
      serverTimeZone: "Asia/Shanghai",
    });
  });

  it("上游未就绪时不编造值：币种/口径/时区保持 undefined，由视图自身回落", () => {
    act(() => {
      createRoot(container).render(
        <UsageLogsDataSection isAdmin={false} userId={42} serverTimeZone={undefined} />
      );
    });

    expect(viewProps.current).toMatchObject({ isAdmin: false, userId: 42 });
    expect(viewProps.current?.currencyCode).toBeUndefined();
    expect(viewProps.current?.billingModelSource).toBeUndefined();
    expect(viewProps.current?.serverTimeZone).toBeUndefined();
  });

  it("活跃会话区块仍把币种交给 ActiveSessionsList", () => {
    act(() => {
      createRoot(container).render(<UsageLogsActiveSessionsSection currencyCode="USD" />);
    });

    expect(viewProps.current).toMatchObject({
      currencyCode: "USD",
      maxHeight: "200px",
      showTokensCost: false,
    });
  });
});
