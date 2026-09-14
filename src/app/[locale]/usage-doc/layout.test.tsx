import { afterEach, describe, expect, it, vi } from "vitest";

const translations = vi.hoisted(() => ({
  pageTitle: "使用文档",
  pageDescription: "如何接入与使用",
}));
const setRequestLocaleMock = vi.hoisted(() => vi.fn());

vi.mock("next-intl/server", () => ({
  getTranslations: async () => (key: keyof typeof translations) => translations[key],
  setRequestLocale: (locale: string) => setRequestLocaleMock(locale),
}));

vi.mock("./_components/usage-doc-chrome", () => ({
  UsageDocChrome: ({ children }: { children?: unknown }) => (
    <div data-testid="chrome">{children as never}</div>
  ),
}));

import UsageDocLayout, { generateMetadata } from "./layout";

describe("UsageDocLayout（服务端 metadata + 客户端外壳）", () => {
  afterEach(() => {
    setRequestLocaleMock.mockReset();
  });

  it("generateMetadata 返回文档段的 title/description（Node 版经 getTranslations 设置过）", async () => {
    const meta = await generateMetadata({ params: Promise.resolve({ locale: "zh-CN" }) });

    expect(meta).toEqual({
      title: translations.pageTitle,
      description: translations.pageDescription,
    });
  });

  it("渲染外壳并把 children 透传进去，同时声明本段 locale（静态渲染前提）", async () => {
    const element = (await UsageDocLayout({
      children: <span data-testid="doc-body" />,
      params: Promise.resolve({ locale: "en" }),
    })) as { type: { name?: string }; props: { children?: unknown } };

    expect(setRequestLocaleMock).toHaveBeenCalledWith("en");
    expect(element.props.children).toBeDefined();
  });

  it("布局本身不得命中导出脚本的服务端绑定判定（否则整个 layout 会被移出产物）", async () => {
    const fs = await import("node:fs/promises");
    // vitest 下 import.meta.url 不是 file: 方案，故按仓库根相对路径读源文件。
    const source = await fs.readFile("src/app/[locale]/usage-doc/layout.tsx", "utf-8");
    const valueLines = source
      .split("\n")
      .filter((line) => !/^\s*import\s+type\b/.test(line))
      .join("\n");

    expect(/export const dynamic = "force-dynamic"/.test(source)).toBe(false);
    expect(/from\s+["']@\/(?:lib\/auth|repository\/|actions\/)/.test(valueLines)).toBe(false);
    expect(/from\s+["']next\/(?:headers|cookies)["']/.test(valueLines)).toBe(false);
  });
});
