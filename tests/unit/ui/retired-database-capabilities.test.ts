import fs from "node:fs";
import path from "node:path";
import { describe, expect, test } from "vitest";

/**
 * 回归钉子：**已随 Node 退役下线的能力不得再被 UI 引用**。
 *
 * 背景（2026-09-13 实测）：`/api/admin/database/export`、`/api/admin/database/import`、
 * `/api/internal/data-gen` 三条端点随 Node 后端一起下线（依赖容器内 pg_dump/psql；导入是破坏性
 * 操作；data-gen 写生产库），Go 侧**有意不实现**（见 `bun scripts/admin-endpoint-gap.mjs`
 * 的豁免表与 ``）。
 * 故 UI 里任何对这些路径的调用都必然是死按钮——点了只会 404/502。本钉子把「不留死按钮」
 * 这条纪律固化为可重跑的检查：将来若有人把控件加回来，这里会先红。
 *
 * 为什么用源码扫描而不是渲染测试：这几条端点的调用点是 `fetch()` 字符串，渲染测试需要起
 * 组件树与 MSW；而「源码里是否还引用」是**更强**的不变量——它连未渲染的死代码也一起拦住。
 */

const PROJECT_ROOT = path.resolve(__dirname, "../../..");
const LOCALE_ROOT = path.join(PROJECT_ROOT, "src", "app", "[locale]");

/** 已下线的端点路径（UI 不得再调用）。 */
const RETIRED_ENDPOINTS = [
  "/api/admin/database/export",
  "/api/admin/database/import",
  "/api/internal/data-gen",
];

/**
 * 剥掉注释，只留代码。
 *
 * 为何必须剥：本钉子的不变量是「**代码**不得再调用已下线端点」，而注释里提到该路径是正常的
 * （退役说明本身就要写清「哪个端点没了」）。不剥的话钉子会把解释性注释当成违规——
 * 首版就被自己的 page.tsx 注释抓了一次，属误报。
 *
 * `//` 的判定排除了前面是 `:` 的情形（`https://` 里的双斜杠），否则同行后续代码会被误删。
 */
function stripComments(source: string): string {
  return source
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .split("\n")
    .map((line) => {
      const at = line.search(/(^|[^:])\/\//);
      return at === -1 ? line : line.slice(0, at + (line[at] === "/" ? 0 : 1));
    })
    .join("\n");
}

function walk(dir: string, out: string[] = []): string[] {
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    if (entry.name === "node_modules" || entry.name.startsWith(".")) continue;
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) walk(full, out);
    else if (/\.(ts|tsx)$/.test(entry.name) && !/\.test\.(ts|tsx)$/.test(entry.name))
      out.push(full);
  }
  return out;
}

/** 抽出源码里出现的已下线端点（只看代码，注释已剥掉）。 */
function retiredEndpointReferences(source: string): string[] {
  const code = stripComments(source);
  return RETIRED_ENDPOINTS.filter((endpoint) => code.includes(endpoint));
}

describe("UI：已退役能力不得再被引用", () => {
  const scanned = walk(LOCALE_ROOT);
  const offenders: string[] = [];
  for (const file of scanned) {
    const hits = retiredEndpointReferences(fs.readFileSync(file, "utf8"));
    if (hits.length > 0) {
      offenders.push(`${path.relative(PROJECT_ROOT, file)} → ${hits.join(", ")}`);
    }
  }

  test("扫描面非空（防扫描静默失败导致假绿）", () => {
    // 实测 locale 树下约 300+ 个 tsx/ts；给一个宽松下界即可发现「walk 坏了」。
    expect(scanned.length).toBeGreaterThan(50);
    expect(retiredEndpointReferences(fs.readFileSync(scanned[0], "utf8"))).toBeDefined();
  });

  test("locale 页面里没有任何对已下线端点的调用", () => {
    expect(
      offenders,
      [
        "以下界面代码仍在调用随 Node 退役下线的端点（点了必然失败）：",
        ...offenders.map((line) => `  - ${line}`),
        "修法：删除控件并原位给出说明（参见 src/app/[locale]/settings/data/_components/retired-capability.tsx）。",
      ].join("\n")
    ).toEqual([]);
  });

  test("退役说明仍在位，且两个受影响页面都引用它", () => {
    const notice = path.join(
      PROJECT_ROOT,
      "src/app/[locale]/settings/data/_components/retired-capability.tsx"
    );
    expect(fs.existsSync(notice)).toBe(true);

    const dataPage = fs.readFileSync(
      path.join(PROJECT_ROOT, "src/app/[locale]/settings/data/page.tsx"),
      "utf8"
    );
    expect(dataPage).toContain("RetiredCapability");

    const genPage = fs.readFileSync(
      path.join(PROJECT_ROOT, "src/app/[locale]/internal/data-gen/page.tsx"),
      "utf8"
    );
    expect(genPage).toContain("internal.dataGenerator.retired");
  });

  test("判定规则自检：能识别违规样例", () => {
    expect(retiredEndpointReferences('await fetch("/api/admin/database/export")')).toEqual([
      "/api/admin/database/export",
    ]);
    expect(
      retiredEndpointReferences('fetch("/api/internal/data-gen", { method: "POST" })')
    ).toEqual(["/api/internal/data-gen"]);
    expect(retiredEndpointReferences('fetch("/api/admin/database/status")')).toEqual([]);
    expect(retiredEndpointReferences('fetch("/api/admin/log-cleanup/manual")')).toEqual([]);
  });

  test("判定规则自检：注释里的路径不算违规（否则退役说明本身会被误报）", () => {
    expect(retiredEndpointReferences("// 曾在 /api/internal/data-gen 造数")).toEqual([]);
    expect(retiredEndpointReferences('/* fetch("/api/admin/database/export") */')).toEqual([]);
    expect(retiredEndpointReferences("/** 旧的 /api/admin/database/import 已下线 */")).toEqual([]);
    // 同行 `https://` 的双斜杠不得让后续代码被吃掉（否则会漏报）
    expect(
      retiredEndpointReferences('fetch("https://x/y"); fetch("/api/admin/database/import")')
    ).toEqual(["/api/admin/database/import"]);
  });
});
