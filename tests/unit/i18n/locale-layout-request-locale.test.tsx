import type { ReactElement, ReactNode } from "react";
import { describe, expect, test, vi } from "vitest";

const nextIntlMocks = vi.hoisted(() => ({
  provider: vi.fn(({ children }: { children: ReactNode }) => children),
  getMessages: vi.fn(async () => ({ dashboard: { nav: { dashboard: "Dashboard" } } })),
  setRequestLocale: vi.fn(),
}));

vi.mock("next-intl", () => ({
  NextIntlClientProvider: nextIntlMocks.provider,
}));

vi.mock("next-intl/server", () => ({
  getMessages: nextIntlMocks.getMessages,
  setRequestLocale: nextIntlMocks.setRequestLocale,
}));

vi.mock("next/headers", () => ({
  headers: vi.fn(async () => ({
    get: vi.fn(() => null),
  })),
}));

vi.mock("next/navigation", () => ({
  notFound: vi.fn(() => {
    throw new Error("notFound");
  }),
}));

vi.mock("@/components/customs/footer", () => ({
  Footer: () => null,
}));

vi.mock("@/components/ui/sonner", () => ({
  Toaster: () => null,
}));

vi.mock("@/lib/layout-site-metadata", () => ({
  resolveDefaultLayoutTimeZone: vi.fn(async () => "UTC"),
  resolveDefaultSiteMetadataSource: vi.fn(async () => null),
}));

vi.mock("@/lib/public-status/layout-metadata", () => ({
  resolveLayoutTimeZone: vi.fn(async () => "UTC"),
  resolveSiteMetadataSource: vi.fn(async () => null),
}));

vi.mock("@/lib/logger", () => ({
  logger: {
    error: vi.fn(),
  },
}));

vi.mock("@/app/providers", () => ({
  AppProviders: ({ children }: { children: ReactNode }) => children,
}));

vi.mock("@/app/globals.css", () => ({}));

/** 在元素树里按组件类型定位（不渲染，故只沿 props.children 走）。 */
function findElement(node: ReactNode, type: unknown, depth = 0): ReactElement | null {
  if (depth > 20 || !node || typeof node !== "object" || !("props" in node)) {
    return null;
  }

  const element = node as ReactElement<{ children?: ReactNode }>;
  if (element.type === type) {
    return element;
  }

  const children = element.props.children;
  if (Array.isArray(children)) {
    for (const child of children) {
      const match = findElement(child, type, depth + 1);
      if (match) return match;
    }
    return null;
  }

  return findElement(children, type, depth + 1);
}

describe("locale root layout", () => {
  test("pins next-intl request locale and provider locale to the route segment", async () => {
    const { I18nProvider } = await import("@/components/i18n-provider");
    const { default: RootLayout } = await import("@/app/[locale]/layout");

    const tree = await RootLayout({
      children: <main />,
      params: Promise.resolve({ locale: "en" }),
    });

    expect.soft(nextIntlMocks.setRequestLocale).toHaveBeenCalledWith("en");

    const provider = findElement(tree, I18nProvider);
    expect.soft(provider?.props).toMatchObject({
      locale: "en",
      timeZone: "UTC",
    });
  });

  /**
   * 词表不得经由 layout 的 props 流向客户端 provider。
   *
   * 跨 server/client 边界的 props 会被序列化进每个路由每个 locale 的 RSC flight payload：
   * 实测词表占 payload 的 65%（178 KiB / 275 KiB），该 payload 每路由存 3 份另有 HTML 内联一份，
   * 全站因而多出约 88 MiB。词表改由客户端模块按 locale 取用（@/i18n/client-messages），
   * 故此处钉住：layout 既不读取词表，也不把它传给 provider。
   */
  test("does not load or forward the message catalog from the server layout", async () => {
    const { I18nProvider } = await import("@/components/i18n-provider");
    const { default: RootLayout } = await import("@/app/[locale]/layout");

    nextIntlMocks.getMessages.mockClear();
    const tree = await RootLayout({
      children: <main />,
      params: Promise.resolve({ locale: "en" }),
    });

    expect(
      nextIntlMocks.getMessages,
      "layout 不得读取词表（会经 props 进 RSC payload，见本用例注释）"
    ).not.toHaveBeenCalled();

    const provider = findElement(tree, I18nProvider);
    expect(provider, "根布局应渲染 I18nProvider").not.toBeNull();
    expect(provider?.props, "provider 不得接收 messages prop（同上）").not.toHaveProperty(
      "messages"
    );
  });
});
