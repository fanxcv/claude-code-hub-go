import fs from "node:fs";
import path from "node:path";
import { describe, expect, test } from "vitest";

/**
 * UI 静态导出的**客户端图边界**钉子。
 *
 * 背景（2026-09-13 生产实测踩过）：静态导出时 `scripts/build-ui-export.mjs` 会把
 * `src/app/{api,v1,v1beta}` 整树移出（它们是 Go 的运行时路由，导出没有 Node 进程）。
 * 若某个 **客户端组件** 的依赖链里出现对这些目录的运行时 import，`next build`
 * 会直接失败：
 *
 *   Module not found: Can't resolve '@/app/v1/_lib/url'
 *     ← src/app/[locale]/settings/providers/_components/forms/url-preview.tsx
 *
 * 该缺陷的特征是**传递性**的：出问题的往往不是页面本身，而是页面链上某个深处的
 * 客户端组件（当时 `page.tsx` 转成客户端组件后，其链条才第一次被拉进客户端图）。
 * 因此本钉子扫的是「**所有带 `"use client"` 的文件**」这一集合——它们都在客户端图里。
 *
 * 修法方向不是放宽钉子，而是把被引用的模块**移出被移出目录树**
 * （当时把 `src/app/v1/_lib/url.ts` 迁到 `src/lib/v1-url.ts`）。
 */

const PROJECT_ROOT = path.resolve(__dirname, "../../..");
const SOURCE_ROOT = path.join(PROJECT_ROOT, "src");
const STASHED_TREES = ["api", "v1", "v1beta"] as const;

/** 静态导出时会被整树移出的源码目录（相对 `src/app`）。 */
const STASHED_IMPORT_PATTERN = new RegExp(`^@/app/(${STASHED_TREES.join("|")})/`);

/** 是否带 `"use client"` 指令（只看文件头部若干行，Next 要求指令在顶部）。 */
function hasUseClientDirective(source: string): boolean {
  const head = source.split("\n").slice(0, 5).join("\n");
  return /^\s*["']use client["'];?\s*$/m.test(head);
}

/** 抽出运行时 import 的模块说明符：跳过 `import type`（类型不产生运行时依赖）。 */
function runtimeImportSpecifiers(source: string): string[] {
  const lines = source.split("\n").filter((line) => !/^\s*import\s+type\b/.test(line));
  const specs: string[] = [];
  for (const line of lines) {
    const fromMatch = line.match(/^\s*import\s[^"']*["']([^"']+)["']/);
    if (fromMatch) {
      specs.push(fromMatch[1]);
      continue;
    }
    // 动态 import() 与副作用 import "x"
    const sideEffect = line.match(/^\s*import\s+["']([^"']+)["']/);
    if (sideEffect) specs.push(sideEffect[1]);
    for (const dynamic of line.matchAll(/import\(\s*["']([^"']+)["']\s*\)/g)) {
      specs.push(dynamic[1]);
    }
  }
  return specs;
}

/** 找出会破坏静态导出的 import：客户端组件引到被移出目录树。 */
function stashedImportsInClientModule(source: string): string[] {
  if (!hasUseClientDirective(source)) return [];
  return runtimeImportSpecifiers(source).filter((spec) => STASHED_IMPORT_PATTERN.test(spec));
}

function walk(dir: string, out: string[] = []): string[] {
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    if (entry.name === "node_modules" || entry.name.startsWith(".")) continue;
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) {
      walk(full, out);
    } else if (/\.(ts|tsx)$/.test(entry.name) && !/\.test\.(ts|tsx)$/.test(entry.name)) {
      out.push(full);
    }
  }
  return out;
}

describe("UI 导出：客户端图不得引到被移出的目录树", () => {
  const offenders: string[] = [];

  for (const file of walk(SOURCE_ROOT)) {
    const source = fs.readFileSync(file, "utf8");
    const hits = stashedImportsInClientModule(source);
    if (hits.length > 0) {
      offenders.push(`${path.relative(PROJECT_ROOT, file)} → ${hits.join(", ")}`);
    }
  }

  test("没有客户端组件 import @/app/{api,v1,v1beta}/**（否则静态导出构建会失败）", () => {
    expect(
      offenders,
      [
        "以下客户端组件的依赖会破坏静态导出（这些目录在导出时被整树移出）：",
        ...offenders.map((line) => `  - ${line}`),
        "修法：把被引模块迁出被移出目录树（参见 src/lib/v1-url.ts 的先例）。",
      ].join("\n")
    ).toEqual([]);
  });

  test("判定规则自检：能识别违规样例、放行 import type 与非客户端文件", () => {
    const bad = ['"use client";', 'import { buildProxyUrl } from "@/app/v1/_lib/url";'].join("\n");
    expect(stashedImportsInClientModule(bad)).toEqual(["@/app/v1/_lib/url"]);

    const dynamicBad = ['"use client";', 'const m = await import("@/app/api/health/route");'].join(
      "\n"
    );
    expect(stashedImportsInClientModule(dynamicBad)).toEqual(["@/app/api/health/route"]);

    const typeOnly = ['"use client";', 'import type { X } from "@/app/v1/_lib/types";'].join("\n");
    expect(stashedImportsInClientModule(typeOnly)).toEqual([]);

    const serverFile = ['import { buildProxyUrl } from "@/app/v1/_lib/url";'].join("\n");
    expect(stashedImportsInClientModule(serverFile)).toEqual([]);

    const legal = ['"use client";', 'import { buildProxyUrl } from "@/lib/v1-url";'].join("\n");
    expect(stashedImportsInClientModule(legal)).toEqual([]);
  });
});
