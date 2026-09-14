import type { ReactNode } from "react";
import { beforeEach, describe, expect, test, vi } from "vitest";

const authMocks = vi.hoisted(() => ({
  getSession: vi.fn(async () => null),
}));

vi.mock("@/lib/auth", () => authMocks);

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

const intlServerMocks = vi.hoisted(() => ({
  getTranslations: vi.fn(async ({ locale, namespace }: { locale: string; namespace: string }) => {
    return (key: string) => `${namespace}.${key}.${locale}`;
  }),
}));

vi.mock("next-intl/server", () => intlServerMocks);

function makeAsyncParams(locale: string) {
  const promise = Promise.resolve({ locale });

  Object.defineProperty(promise, "locale", {
    get() {
      throw new Error("sync access to params.locale is not allowed");
    },
  });

  return promise as Promise<{ locale: string }> & { locale: string };
}

describe("Next.js async params compatibility", () => {
  beforeEach(() => {
    authMocks.getSession.mockReset();
    authMocks.getSession.mockResolvedValue(null);
    intlServerMocks.getTranslations.mockClear();
  });

  // 原有两个用例断言 usage-doc 的 `generateMetadata` 与 `UsageDocLayout` 会 await params。
  // 该页布局今为**客户端组件**（静态导出无服务端会话，见 dashboard/layout.tsx 的同源说明）——
  // 它不再接收 `params`，也不再导出 `generateMetadata`，故这两条断言的标的已不存在。
  // 取而代之的守卫是 tests/unit/export/ui-export-client-boundaries.test.ts：
  // 钉住这几个布局不得再引入服务端绑定（否则会被导出脚本整体移出，重新引发「全站无菜单」）。
  test("big-screen generateMetadata awaits params before reading locale", async () => {
    const BigScreenLayoutModule = await import(
      "@/app/[locale]/internal/dashboard/big-screen/layout"
    );

    const metadata = await BigScreenLayoutModule.generateMetadata({
      params: makeAsyncParams("en") as unknown as Promise<{ locale: string }>,
    });

    expect(metadata).toEqual({
      title: "bigScreen.pageTitle.en",
      description: "bigScreen.pageDescription.en",
    });

    const element = BigScreenLayoutModule.default({ children: <div /> });
    expect(element).toBeTruthy();
  });
});
