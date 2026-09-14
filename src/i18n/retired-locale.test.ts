import fs from "node:fs";
import path from "node:path";
import { describe, expect, test } from "vitest";
import {
  defaultLocale,
  localeLabels,
  localeNamesInEnglish,
  locales,
  retiredLocales,
} from "./config";
import { retiredLocaleRedirectHtml } from "./retired-locale";

/**
 * 从重定向壳里抽出内联脚本并**真跑一遍**，返回它算出的跳转目标。
 *
 * 为什么执行而不只匹配字符串：这个壳是唯一承接退役语种旧 URL 的东西，而它只在浏览器里生效；
 * 断字符串只能证明「文件里有这行」，跑脚本才能证明「`/ja/dashboard/logs?tab=1` 真会去
 * `/zh-CN/dashboard/logs?tab=1`」。查询与片段丢失是这类跳转最常见的错法。
 */
function redirectTargetFor(pathname: string, search = "", hash = ""): string {
  const html = retiredLocaleRedirectHtml();
  const script = html.match(/<script>([\s\S]*?)<\/script>/)?.[1];
  expect(script, "重定向壳里必须有一段内联脚本").toBeTruthy();

  let target = "";
  const windowStub = {
    location: {
      pathname,
      search,
      hash,
      replace: (url: string) => {
        target = url;
      },
    },
  };
  new Function("window", script as string)(windowStub);
  return target;
}

describe("语种清单（单一真源 src/i18n/config.ts）", () => {
  test("只留简体中文与英文", () => {
    expect([...locales]).toEqual(["zh-CN", "en"]);
    expect(defaultLocale).toBe("zh-CN");
  });

  test("退役语种与现存语种互斥（同时列进两处的清单会让导出脚本拒建壳）", () => {
    expect([...retiredLocales]).toEqual(["zh-TW", "ru", "ja"]);
    for (const locale of retiredLocales) {
      expect(locales as readonly string[]).not.toContain(locale);
    }
  });

  test("切换器文案与 SEO 名的键集与 locales 完全一致", () => {
    // 注：这两张表的类型（按 Locale 取值得）已保证「多一个键」是编译错，故此用例只是把
    // 「清单改了、标签忘了改」再兜一道，成本近零。
    expect(Object.keys(localeLabels).sort()).toEqual([...locales].sort());
    expect(Object.keys(localeNamesInEnglish).sort()).toEqual([...locales].sort());
  });
});

describe("退役语种重定向壳", () => {
  test("深层路径与查询/片段一并带过去", () => {
    expect(redirectTargetFor("/ja/dashboard/logs", "?tab=1", "#row-2")).toBe(
      "/zh-CN/dashboard/logs?tab=1#row-2"
    );
  });

  test("三个退役前缀都能接住（同一份壳服务三者，故逐个验一遍）", () => {
    expect(redirectTargetFor("/ja/dashboard")).toBe("/zh-CN/dashboard");
    expect(redirectTargetFor("/ru/status")).toBe("/zh-CN/status");
    expect(redirectTargetFor("/zh-TW/my-usage")).toBe("/zh-CN/my-usage");
  });

  test("只带语种前缀（无深层路径）时落到默认语言的仪表盘，不是空路径", () => {
    expect(redirectTargetFor("/ja")).toBe("/zh-CN/dashboard");
    expect(redirectTargetFor("/ru/")).toBe("/zh-CN/");
  });

  test("目标语言不在壳内硬编码为退役语种（避免拼出 /zh-CN/ja/...）", () => {
    for (const locale of retiredLocales) {
      expect(redirectTargetFor(`/${locale}/dashboard`)).not.toContain(`/${locale}/`);
    }
  });
});

describe("导出脚本与清单的一致性（防「漏改一处清单」）", () => {
  const scriptSource = fs.readFileSync(
    path.join(process.cwd(), "scripts/build-ui-export.mjs"),
    "utf8"
  );

  test("导出脚本从 config.ts 取语种，而不是自带一份清单", () => {
    expect(scriptSource).toContain('from "../src/i18n/config.ts"');
    expect(scriptSource).toContain("retiredLocales");
    // 自带清单的典型症状：脚本里出现退役语种字面量。出现即说明有人又抄了一份。
    for (const locale of retiredLocales) {
      expect(scriptSource, `导出脚本不得硬编码退役语种 ${locale}`).not.toContain(`"${locale}"`);
    }
  });

  test("导出脚本为每个退役语种写壳（按清单迭代，而不是写死三个）", () => {
    expect(scriptSource).toMatch(/for \(const locale of retiredLocales\)/);
    expect(scriptSource).toContain("retiredLocaleRedirectHtml()");
  });
});
