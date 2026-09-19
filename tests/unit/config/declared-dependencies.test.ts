import { readFileSync, readdirSync, statSync } from "node:fs";
import path from "node:path";
import { describe, expect, test } from "vitest";

/**
 * 依赖声明钉子：`src/**` 里出现的裸模块引用必须在 `package.json` 显式声明。
 *
 * 为什么需要它：本仓长期靠「别的包恰好把它装进 node_modules」来解析部分 import——
 * `big-screen/page.tsx` 曾直接 `import useSWR from "swr"`，而 `swr` 只是 `@lobehub/ui`
 * 的传递依赖；`@radix-ui/react-portal`、`@radix-ui/react-visually-hidden` 同形。
 * 这类幽灵依赖平时能跑，等上游哪天不再传递引入就变成构建期报错，且报错点离真因很远。
 *
 * 口径：
 * 1. 范围只扫 `src/**`（运行时代码）；测试与脚本不在其列，避免为测试专用包做显式声明；
 * 2. 跳过相对路径、`@/`、`@messages/`（tsconfig 别名）与 `node:` 内建；
 * 3. 只认「像包名」的说明符（小写、允许 scope 与子路径），滤掉跨行 import 的碎片匹配；
 * 4. `geojson` 这类纯类型引用按 `@types/<name>`（scope 记为 `@types/<scope>__<name>`）计入声明。
 */
const ROOT = process.cwd();
const SOURCE_ROOT = path.join(ROOT, "src");
const SPECIFIER = /(?:from|import)\s*\(?\s*["']([^"']+)["']/g;
const PACKAGE_LIKE =
  /^(@[a-z0-9][a-z0-9._-]*\/[a-z0-9][a-z0-9._-]*|[a-z0-9][a-z0-9._-]*)(\/[a-z0-9._-]+)*$/;

type PackageJson = {
  dependencies?: Record<string, string>;
  devDependencies?: Record<string, string>;
};

const pkg = JSON.parse(readFileSync(path.join(ROOT, "package.json"), "utf-8")) as PackageJson;
const declared = new Set([
  ...Object.keys(pkg.dependencies ?? {}),
  ...Object.keys(pkg.devDependencies ?? {}),
]);

function typePackageName(name: string): string {
  if (name.startsWith("@")) {
    const [scope, rest] = name.split("/");
    return `@types/${scope.slice(1)}__${rest}`;
  }
  return `@types/${name}`;
}

function isDeclared(name: string): boolean {
  return declared.has(name) || declared.has(typePackageName(name));
}

function toPackageName(specifier: string): string {
  const parts = specifier.split("/");
  return specifier.startsWith("@") ? parts.slice(0, 2).join("/") : parts[0];
}

function sourceFiles(dir: string): string[] {
  return readdirSync(dir).flatMap((entry) => {
    const full = path.join(dir, entry);
    if (statSync(full).isDirectory()) return sourceFiles(full);
    return full.endsWith(".ts") || full.endsWith(".tsx") ? [full] : [];
  });
}

describe("依赖声明", () => {
  test("src/ 的裸模块引用都在 package.json 中显式声明", () => {
    const undeclared = new Map<string, Set<string>>();

    for (const file of sourceFiles(SOURCE_ROOT)) {
      const text = readFileSync(file, "utf-8");
      for (const specifier of text.matchAll(SPECIFIER)) {
        const spec = specifier[1];
        if (spec.startsWith(".") || spec.startsWith("@/") || spec.startsWith("@messages/"))
          continue;
        if (spec.startsWith("node:")) continue;
        if (!PACKAGE_LIKE.test(spec)) continue;

        const name = toPackageName(spec);
        if (isDeclared(name)) continue;
        const files = undeclared.get(name) ?? new Set<string>();
        files.add(path.relative(ROOT, file));
        undeclared.set(name, files);
      }
    }

    expect(
      [...undeclared.entries()].map(([name, files]) => `${name} <- ${[...files].join(", ")}`)
    ).toEqual([]);
  });
});
