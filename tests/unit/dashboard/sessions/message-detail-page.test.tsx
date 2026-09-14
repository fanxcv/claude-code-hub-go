import { isValidElement } from "react";
import { describe, expect, test, vi } from "vitest";

vi.mock("@/components/ui-session-gate", async () => {
  const { createElement, Fragment } = await import("react");
  return {
    UiSessionGate: (props: { children: React.ReactNode; requireRole?: string }) =>
      createElement(Fragment, null, props.children),
  };
});

vi.mock(
  "@/app/[locale]/dashboard/sessions/[sessionId]/messages/_components/session-messages-client",
  () => ({
    SessionMessagesClient: function MockSessionMessagesClient() {
      return null;
    },
  })
);

/**
 * 该页在静态化改造后不再是 SSR 页（服务端 `getSession` + `redirect` 已删，改由
 * `UiSessionGate` 在客户端判定），故断言改为：页面把 admin 要求交给鉴权壳，
 * 壳内才是消息客户端。鉴权壳自身的行为在其单测
 * （`src/components/ui-session-gate.test.tsx`）里逐条覆盖。
 */
describe("SessionMessagesPage", () => {
  test("renders SessionMessagesClient behind an admin-only session gate", async () => {
    const { default: SessionMessagesPage } = await import(
      "@/app/[locale]/dashboard/sessions/[sessionId]/messages/page"
    );

    const element = SessionMessagesPage();

    expect(isValidElement(element)).toBe(true);
    if (!isValidElement(element)) {
      throw new Error("SessionMessagesPage should return a React element");
    }

    expect(element.props.requireRole).toBe("admin");
    expect(element.props.forbiddenHref).toBe("/dashboard");

    const child = element.props.children;
    expect(isValidElement(child)).toBe(true);
    if (!isValidElement(child)) {
      throw new Error("SessionMessagesPage should wrap a React element");
    }
    expect((child.type as { name?: string }).name).toBe("MockSessionMessagesClient");
  });
});
