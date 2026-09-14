/**
 * 开关必须带可访问名。
 *
 * 来源：a11y 审计发现 33 处 `Switch` 只有开关本体、没有名字——屏幕阅读器只会念「开关」，
 * 用户无法知道它在控制什么（本次审计前的覆盖面见 tests 里那条 33 处的名单）。
 * 这些开关的可视文字都在相邻的 `<Label>` 或卡片标题里，但 Label 没有 `htmlFor`／没有被
 * `aria-labelledby` 引用，等于对辅助技术不存在。
 *
 * 这条钉子按源码形状判定，理由：`<Switch>` 是纯 DOM 组件，单测要断言可访问名须把每个页面
 * 连同 i18n／路由／查询客户端一起挂载，成本远高于收益；而漏名字是**语法层**的疏漏，
 * 扫标签本身即可判定，且新加一处未命名开关会立刻转红。
 *
 * 覆盖面（2026-09-14 补盲区）：扫两类形态——`<Switch>` 组件，以及**任何带 `role="switch"` 的标签**。
 * 后者是手写开关（如 `settings/_components/ui/settings-ui.tsx` 的 `SettingsToggleRow`，用
 * `div[role=switch]` + `aria-checked` + `tabIndex` 自己搭），**此前完全不在扫描面上**：一个手写
 * 开关漏名字时这条钉子是绿的。
 *
 * 手写开关也要求**显式**取名，不接受「内容即名字」：这类容器的可见文本通常同时含标题与描述，
 * 名字会变成两者拼接；显式 `aria-label={title}` 既钉住「名字 = 可见标题」，也让判定停在
 * **语法层可机械校验**的范围内（在源码形状上无法可靠推演可访问名计算）。
 *
 * 测试文件（`*.test.tsx`／`*.spec.tsx`）不入扫：那里的 `role="switch"` 是替身对原子的模仿，
 * 不给可访问名无用户影响，报出来只是噪声。实测 src 下测试文件**零处** `<Switch>`，故排除不丢嘴覆盖面。
 *
 * 已知天花板（有意不改）：本检查是**形状匹配**，不是解析器。标签内部若出现 `//` 行注释，
 * 注释文字会被当作属性一并读取（今天全仓无此写法；真被咬到再换真解析器，不要在这个函数里加固）。
 */
import fs from "node:fs";
import path from "node:path";
import { describe, expect, test } from "vitest";

const SRC_DIR = path.join(process.cwd(), "src");
const SWITCH_ATOM = path.join("components", "ui", "switch.tsx");
/** 测试文件里的开关是替身，不入扫（理由见文件头「覆盖面」）。 */
const TEST_FILE = /\.(test|spec)\.tsx$/;

/** 递归收集 src 下的 tsx 文件。 */
function collectTsx(dir: string): string[] {
  const out: string[] = [];
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      out.push(...collectTsx(full));
    } else if (entry.name.endsWith(".tsx")) {
      out.push(full);
    }
  }
  return out;
}

/**
 * 从 `<Switch` 的起始下标取回完整标签文本。
 *
 * 必须自己配对 `{}`／`()`／`[]` 并跳过字符串与模板串：属性里常见
 * `onCheckedChange={(checked) => dispatch({ ... })}` 这种带 `>` 或 `/` 的表达式，
 * 用正则截到第一个 `/>` 会把标签切短，从而漏看后面的 `aria-label`（本项目真实踩过：
 * `request-filters.tsx` 的名字写在标签末尾，正则扫描曾判它没有名字）。
 */
function readTag(source: string, start: number): string {
  let depth = 0;
  let quote: string | null = null;
  for (let i = start; i < source.length; i += 1) {
    const ch = source[i];
    if (quote) {
      if (ch === "\\") {
        i += 1;
      } else if (ch === quote) {
        quote = null;
      }
      continue;
    }
    if (ch === '"' || ch === "'" || ch === "`") {
      quote = ch;
    } else if (ch === "{" || ch === "[" || ch === "(") {
      depth += 1;
    } else if (ch === "}" || ch === "]" || ch === ")") {
      depth -= 1;
    } else if (ch === ">" && depth === 0) {
      return source.slice(start, i + 1);
    }
  }
  return source.slice(start, start + 40);
}

/**
 * 找出所有开关标签：`<Switch …>` 与任何带 `role="switch"` 的标签。
 *
 * 用「遍历每个 `<` 再交给 readTag 配平」而不是「从属性名往回找起始 `<`」：后者在属性值里
 * 出现 `>`／`<` 时会认错标签起点（本仓的 className 与 tooltip 文案里两者都有）。
 */
function findSwitchTags(source: string): { tag: string; index: number }[] {
  const found: { tag: string; index: number }[] = [];
  for (let i = 0; i < source.length; i += 1) {
    if (source[i] !== "<") continue;
    if (!/[A-Za-z]/.test(source[i + 1] ?? "")) continue;
    const tag = readTag(source, i);
    if (/^<Switch[\s/>]/.test(tag) || /\brole="switch"/.test(tag)) {
      found.push({ tag, index: i });
    }
  }
  return found;
}

/** 参与扫描的文件：src 下的 tsx，去掉原子组件（命名责任在使用处）与测试替身。 */
function scannedFiles(): string[] {
  return collectTsx(SRC_DIR).filter((f) => !f.endsWith(SWITCH_ATOM) && !TEST_FILE.test(f));
}

function findUnnamedSwitches(): string[] {
  const unnamed: string[] = [];
  for (const file of scannedFiles()) {
    const source = fs.readFileSync(file, "utf8");
    for (const { tag, index } of findSwitchTags(source)) {
      if (/\baria-label\b|\baria-labelledby\b/.test(tag)) continue;
      const line = source.slice(0, index).split("\n").length;
      unnamed.push(`${path.relative(process.cwd(), file)}:${line}`);
    }
  }
  return unnamed;
}

describe("Switch 可访问名", () => {
  test("src 下每个 Switch 都带 aria-label 或 aria-labelledby", () => {
    const unnamed = findUnnamedSwitches();
    expect(
      unnamed,
      "以下开关没有可访问名，请把相邻 Label／卡片标题的文案挂到 aria-label 上：\n" +
        unnamed.join("\n")
    ).toEqual([]);
  });

  test("扫描确实覆盖到两类开关（防空跑假绿）", () => {
    const tags = scannedFiles().flatMap((f) => findSwitchTags(fs.readFileSync(f, "utf8")));
    const atomCount = tags.filter((t) => /^<Switch[\s/>]/.test(t.tag)).length;
    const handRolled = tags.filter((t) => /\brole="switch"/.test(t.tag)).length;
    // 2026-09-14 实测 73 处 `<Switch>` + 1 处手写开关；只校验下界，加开关不必改这条
    expect(atomCount, "`<Switch>` 一处都没扫到 ⇒ 遍历器坏了").toBeGreaterThanOrEqual(50);
    expect(
      handRolled,
      '`role="switch"` 一处都没扫到 ⇒ 手写开关那一类又掉回盲区了（settings-ui.tsx 的 SettingsToggleRow）'
    ).toBeGreaterThanOrEqual(1);
  });
});
