import { readFileSync } from "node:fs";
import { join } from "node:path";
import { act } from "react";
import { createRoot } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

vi.mock("@/i18n/routing", () => ({
  Link: ({ children, ...props }: Record<string, unknown>) => <a {...props}>{children as never}</a>,
}));

// 版本号是**构建期注入**的（导出脚本把 APP_VERSION 改名成 NEXT_PUBLIC_APP_VERSION 交给 next build），
// 所以本用例通过改环境变量再重新 import 来模拟两次不同的构建。
//
// 为什么值得钉：页脚曾经 `readFile(process.cwd()/VERSION)`，把仓库工作区里的 0.9.5 烙进静态产物，
// 而同一进程的 /api/health 报 0.9.0、镜像 tag 是 1.0.1——三处各说各话，排障时据此误判过
// 「有两台不同的实例」。现在页脚只认注入值，未注入就不显示（不留第四个会漂的版本源）。
async function renderFooter(injected: string | undefined) {
  vi.resetModules();
  if (injected === undefined) {
    delete process.env.NEXT_PUBLIC_APP_VERSION;
  } else {
    process.env.NEXT_PUBLIC_APP_VERSION = injected;
  }
  const { Footer } = await import("@/components/customs/footer");

  const container = document.createElement("div");
  document.body.appendChild(container);
  const root = createRoot(container);
  await act(async () => {
    root.render(<Footer />);
  });
  const text = container.textContent ?? "";
  act(() => {
    root.unmount();
  });
  document.body.removeChild(container);
  return text;
}

describe("Footer 版本号", () => {
  let original: string | undefined;

  beforeEach(() => {
    original = process.env.NEXT_PUBLIC_APP_VERSION;
  });

  afterEach(() => {
    if (original === undefined) {
      delete process.env.NEXT_PUBLIC_APP_VERSION;
    } else {
      process.env.NEXT_PUBLIC_APP_VERSION = original;
    }
  });

  it("注入 1.0.1 时显示 v1.0.1", async () => {
    expect(await renderFooter("1.0.1")).toContain("· v1.0.1");
  });

  it("注入已带前缀的 V2.0.0 时统一成小写 v2.0.0", async () => {
    expect(await renderFooter("V2.0.0")).toContain("· v2.0.0");
  });

  it("未注入时不显示版本（也不留悬空的分隔符）", async () => {
    const text = await renderFooter(undefined);
    expect(text).toContain("CC Hub");
    expect(text).not.toContain("·");
    expect(text).not.toMatch(/v\d+\.\d+/);
  });
});

// 版本号在 UI 侧是**构建期注入**的：导出脚本把 APP_VERSION（缺省用 package.json 的声明值）
// 改名成 NEXT_PUBLIC_APP_VERSION 交给 `next build`，页脚读的就是这个名字。两端必须成对存在——
// 少了导出端，页脚静默变成「没有版本」；少了页脚端，注入的值没人用。故这里同时钉两侧。
describe("页脚版本的两端成对", () => {
  // 只看代码行：这两个文件的注释里正好在讲「以前读文件」，按字面断言会把自己的说明判成违规。
  const codeOf = (relative: string) =>
    readFileSync(join(process.cwd(), relative), "utf-8")
      .split("\n")
      .filter((line) => {
        const trimmed = line.trim();
        return !trimmed.startsWith("//") && !trimmed.startsWith("*") && !trimmed.startsWith("/*");
      })
      .join("\n");

  it("导出脚本注入的变量名 == 页脚读取的变量名", () => {
    const footer = codeOf("src/components/customs/footer.tsx");
    const exporter = codeOf("scripts/build-ui-export.mjs");
    expect(footer).toContain("process.env.NEXT_PUBLIC_APP_VERSION");
    expect(exporter).toContain("NEXT_PUBLIC_APP_VERSION: appVersion");
    // 导出端也必须先认发布时注入的真源名（发布脚本传的就是它）。
    expect(exporter).toContain("process.env.APP_VERSION");
  });

  it("页脚不再读工作区的文件（真源是注入，不是磁盘）", () => {
    const footer = codeOf("src/components/customs/footer.tsx");
    expect(footer).not.toMatch(/readFile\s*\(/);
    expect(footer).not.toContain('"VERSION"');
  });
});
