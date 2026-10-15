import { readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, test } from "vitest";
import { locales } from "@/i18n/config";
import { loadClientMessages } from "@/i18n/client-messages";

/**
 * 词表供给契约的钉子。
 *
 * 背景：词表曾由服务端 layout 以 props 传给客户端 provider，会被序列化进每个路由每个 locale
 * 的 RSC flight payload（实测占 payload 65%，全站多出约 88 MiB）。改为客户端模块按 locale
 * 取用后，必须保证：2 个 locale 都真能取到完整词表，且未知 locale 有确定回落。
 * 若某 locale 漏在 loaders 表里，页面会静默回落成中文——这条用例就是防它。
 */
describe("client messages catalog", () => {
  test("每 locale 都能取到词表，且不是空表", async () => {
    for (const locale of locales) {
      const messages = await loadClientMessages(locale);
      expect(Object.keys(messages).length, `${locale} 词表为空`).toBeGreaterThan(0);
    }
  });

  test("各 locale 词表互相独立（未串语言）", async () => {
    const en = JSON.stringify(await loadClientMessages("en"));
    const zh = JSON.stringify(await loadClientMessages("zh-CN"));
    expect(en).not.toBe(zh);
    // 取一个必然存在的命名空间做区分确认
    expect(await loadClientMessages("en")).toHaveProperty("common");
    expect(await loadClientMessages("zh-CN")).toHaveProperty("common");
  });

  test("未知 locale 回落到默认语言", async () => {
    expect(await loadClientMessages("de-DE")).toBe(await loadClientMessages("zh-CN"));
  });

  test("同一 locale 复用同一 promise（use() 依赖其身份稳定）", () => {
    expect(loadClientMessages("en")).toBe(loadClientMessages("en"));
  });

  /**
   * 分块前提的钉子：本模块一旦改回对 catalog 的**静态** import，两份词表就回到同一个 eager
   * chunk（实测 raw 577,580 B / gzip 176,173 B，被 66/73 个导出 html 引用），按 locale 分块失效。
   * 具体体积与引用面由 `bun run build` 后的导出产物核对，这里只钉住取用方式。
   */
  test("catalog 只经 import() 取用", () => {
    const source = readFileSync(join(process.cwd(), "src/i18n/client-messages.ts"), "utf8");
    expect(source).not.toMatch(/^\s*import\s+\w+\s+from\s+"@messages\//m);
  });
});
