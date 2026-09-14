import fs from "node:fs";
import path from "node:path";
import { describe, expect, test } from "vitest";

/**
 * 导出产物的客户端边界钉子。
 *
 * 背景（实测）：`scripts/build-ui-export.mjs` 会把**服务端绑定**的 page/layout 整份移出导出。
 * `dashboard/layout.tsx`、`settings/layout.tsx`、`usage-doc/layout.tsx` 三者都渲染站点头部，
 * 一旦被判为服务端绑定，产物里就**一个菜单都没有**（用户症状：只能看到首页、其余页面进不去；
 * 实测 `out/zh-CN/dashboard/index.html` 修复前 `<header` 命中数为 0）。
 *
 * 故这里钉住「这三个布局 + 头部组件必须留在客户端」——即判定函数必须为假。
 * 否则改动者会在毫无报错的情况下再次把全站导航弄丢（导出照常成功，只是没有菜单）。
 */

/**
 * 判定规则镜像 `scripts/build-ui-export.mjs` 的 `isServerBound`（该文件是脚本、无法直接引入）：
 * `force-dynamic`、运行时的 `@/lib/auth` / `@/repository/` / `@/actions/` 值导入、
 * `next/headers` / `next/cookies`，任一命中即会被移出。`import type` 行不计入（类型不产生运行时依赖）。
 *
 * 漂移风险：脚本改了判定而这里没跟改，钉子会静默失效。修脚本时请同步本函数。
 */
function isServerBound(source: string): boolean {
  const valueLines = source.split("\n").filter((line) => !/^\s*import\s+type\b/.test(line));
  const joined = valueLines.join("\n");

  if (/export const dynamic = "force-dynamic"/.test(source)) return true;
  if (/from\s+["']@\/(?:lib\/auth|repository\/|actions\/)/.test(joined)) return true;
  if (/from\s+["']next\/(?:headers|cookies)["']/.test(joined)) return true;
  return false;
}

function readProjectFile(...segments: string[]) {
  return fs.readFileSync(path.join(process.cwd(), ...segments), "utf8");
}

const CLIENT_BOUNDARY_FILES = [
  "src/app/[locale]/dashboard/layout.tsx",
  "src/app/[locale]/settings/layout.tsx",
  "src/app/[locale]/usage-doc/_components/usage-doc-chrome.tsx",
  "src/app/[locale]/dashboard/_components/dashboard-header.tsx",
  "src/app/[locale]/settings/_lib/nav-items.ts",
];

/** 纯模块（只导出函数/常量，不导出组件与 hook）：不需指令，只需不被判为服务端绑定。 */
const CLIENT_SAFE_MODULES = ["src/app/[locale]/dashboard/_components/dashboard-nav-items.ts"];

describe("UI 导出：站点头部链路的客户端边界", () => {
  for (const rel of CLIENT_BOUNDARY_FILES) {
    test(`${rel} 是客户端组件且不是服务端绑定（否则会被导出脚本移出、全站无菜单）`, () => {
      const source = readProjectFile(...rel.split("/"));

      expect(isServerBound(source)).toBe(false);
      expect(source.startsWith('"use client";')).toBe(true);
    });
  }

  for (const rel of CLIENT_SAFE_MODULES) {
    test(`${rel} 不被判为服务端绑定`, () => {
      const source = readProjectFile(...rel.split("/"));

      expect(isServerBound(source)).toBe(false);
    });
  }

  test("判定函数自检：旧实现（getSession + getTranslations）会被判为服务端绑定", () => {
    // 反证：确认这个钉子真的能抓到「引回服务端绑定」的改动，而不是永远为绿。
    const legacy = [
      '"use client";',
      'import { getSession } from "@/lib/auth";',
      "export default async function Layout() {",
      "  const session = await getSession();",
      "}",
    ].join("\n");

    expect(isServerBound(legacy)).toBe(true);
  });

  test("判定函数放行 import type（类型导入不产生运行时依赖）", () => {
    const typeOnly = [
      '"use client";',
      'import type { AuthSession } from "@/lib/auth";',
      "export const x = 1;",
    ].join("\n");

    expect(isServerBound(typeOnly)).toBe(false);
  });
});
