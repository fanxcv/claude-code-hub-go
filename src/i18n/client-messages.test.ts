import { describe, expect, test } from "vitest";
import { locales } from "@/i18n/config";
import { getClientMessages } from "@/i18n/client-messages";

/**
 * 词表供给契约的钉子。
 *
 * 背景：词表曾由服务端 layout 以 props 传给客户端 provider，会被序列化进每个路由每个 locale
 * 的 RSC flight payload（实测占 payload 65%，全站多出约 88 MiB）。改为客户端模块按 locale
 * 取用后，必须保证：5 个 locale 都真能取到完整词表，且未知 locale 有确定回落。
 * 若某 locale 漏在 catalogs 表里，页面会静默回落成中文——这条用例就是防它。
 */
describe("client messages catalog", () => {
  test("每 locale 都能取到词表，且不是空表", () => {
    for (const locale of locales) {
      const messages = getClientMessages(locale);
      expect(Object.keys(messages).length, `${locale} 词表为空`).toBeGreaterThan(0);
    }
  });

  test("各 locale 词表互相独立（未串语言）", () => {
    const en = JSON.stringify(getClientMessages("en"));
    const zh = JSON.stringify(getClientMessages("zh-CN"));
    expect(en).not.toBe(zh);
    // 取一个必然存在的命名空间做区分确认
    expect(getClientMessages("en")).toHaveProperty("common");
    expect(getClientMessages("zh-CN")).toHaveProperty("common");
  });

  test("未知 locale 回落到默认语言", () => {
    expect(getClientMessages("de-DE")).toBe(getClientMessages("zh-CN"));
  });
});
